package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// staticToken 은 테스트용 TokenSource 다. 실제 JWT 는 auth.Authenticator 가 준다.
//
// 호출 횟수를 atomic 으로 세는 이유: 실물 TokenSource 는 여러 고루틴에서 동시에
// 불린다. 처음에 평범한 int 로 뒀더니 -race 가 이 테스트 더블에서 레이스를
// 잡았다 — 계측 도구가 대상보다 먼저 틀린 전형적인 경우다.
type staticToken struct {
	tok string
	err error
	n   atomic.Int64
}

func (s *staticToken) Token(ctx context.Context) (string, error) {
	s.n.Add(1)
	if s.err != nil {
		return "", s.err
	}
	return s.tok, nil
}

// armed 는 Bearer 가 필요한 엔드포인트를 부를 수 있게 배선한 클라이언트다.
func armed(t *testing.T, baseURL string) *Client {
	t.Helper()
	c := New("test-key")
	c.BaseURL = baseURL
	c.SetTokenSource(&staticToken{tok: "test-jwt-token"})
	return c
}

// ---------------------------------------------------------------- CreateOrder

func TestCreateOrderUnwrapsEnvelopeAndParsesLock(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		// 실물 응답 형태: 봉투 + CreateOrderResponseData.
		w.WriteHeader(http.StatusCreated) // 201 도 성공이어야 한다 (P4 Minor 8)
		io.WriteString(w, `{"success":true,"data":{
			"code":"OK","orderId":"ord-1","orderHash":"0xabc",
			"removalLockedUntil":"2026-08-10T12:00:05Z"}}`)
	}))
	defer srv.Close()

	res, err := armed(t, srv.URL).CreateOrder(context.Background(), map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if gotPath != "/v1/orders" {
		t.Errorf("경로 %q, 기대 /v1/orders", gotPath)
	}
	if gotAuth != "Bearer test-jwt-token" {
		t.Errorf("Authorization = %q — 주문 생성은 Bearer 가 필요하다", gotAuth)
	}
	if res.OrderID != "ord-1" || res.OrderHash != "0xabc" || res.Code != "OK" {
		t.Errorf("봉투를 안 벗겼거나 필드가 틀렸다: %+v", res)
	}
	want := time.Date(2026, 8, 10, 12, 0, 5, 0, time.UTC)
	if !res.RemovalLockedUntil.Equal(want) {
		t.Errorf("removalLockedUntil = %v, 기대 %v", res.RemovalLockedUntil, want)
	}
	if res.RemovalLockUnknown {
		t.Error("잠금 시각을 파싱했는데 RemovalLockUnknown 이 true 다")
	}
}

// 잠금 시각을 못 읽었을 때 "잠금 없음"(제로값)과 구분되지 않으면, 호출자는
// 곧바로 취소를 시도하고 rejected 를 받는다. 그 상태를 명시적으로 표시한다.
func TestCreateOrderFlagsUnknownRemovalLock(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"필드 없음", `{"success":true,"data":{"orderId":"o"}}`},
		{"null", `{"success":true,"data":{"orderId":"o","removalLockedUntil":null}}`},
		{"빈 문자열", `{"success":true,"data":{"orderId":"o","removalLockedUntil":""}}`},
		{"RFC3339 아님", `{"success":true,"data":{"orderId":"o","removalLockedUntil":"내일"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			res, err := armed(t, srv.URL).CreateOrder(context.Background(), map[string]any{})
			if err != nil {
				t.Fatalf("주문은 생성됐는데 에러다: %v", err)
			}
			if !res.RemovalLockUnknown {
				t.Error("잠금 시각을 모르는데 RemovalLockUnknown 이 false 다")
			}
			if !res.RemovalLockedUntil.IsZero() {
				t.Errorf("잠금 시각 = %v, 기대 제로값", res.RemovalLockedUntil)
			}
		})
	}
}

// 주문 생성만은 "실패로 단정"이 위험하다. 재시도하면 이중 주문이 된다.
// 전송 전 실패 / 거래소 거부 / 결과 불명 셋을 호출자가 구분할 수 있어야 한다.
func TestCreateOrderClassifiesFailureForRetrySafety(t *testing.T) {
	t.Run("4xx 는 거부 — 주문은 없다", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(422)
			io.WriteString(w, `{"success":false,"error":"invalid_signature"}`)
		}))
		defer srv.Close()
		_, err := armed(t, srv.URL).CreateOrder(context.Background(), map[string]any{})
		assertOrderKind(t, err, OrderRejected, true)
	})

	t.Run("5xx 는 불명 — 재시도 금지", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(502)
			io.WriteString(w, `bad gateway`)
		}))
		defer srv.Close()
		_, err := armed(t, srv.URL).CreateOrder(context.Background(), map[string]any{})
		assertOrderKind(t, err, OrderUnknown, false)
	})

	t.Run("연결 자체가 안 되면 전송 전 실패", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := srv.URL
		srv.Close() // 아무도 듣지 않는 포트 → dial 거부
		_, err := armed(t, url).CreateOrder(context.Background(), map[string]any{})
		assertOrderKind(t, err, OrderNotSent, true)
	})

	t.Run("보냈는데 응답이 없으면 불명", func(t *testing.T) {
		// 요청은 이미 소켓에 나갔고 서버가 응답 없이 끊었다. 주문이 들어갔는지
		// 우리는 알 수 없다 — 여기서 "실패"로 단정하고 재시도하면 이중 주문이다.
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Skip("Hijacker 미지원")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Skip("Hijack 실패")
			}
			conn.Close()
		}))
		defer srv.Close()
		_, err := armed(t, srv.URL).CreateOrder(context.Background(), map[string]any{})
		assertOrderKind(t, err, OrderUnknown, false)
	})

	t.Run("토큰 발급 실패는 전송 전 실패", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("토큰이 없는데 요청이 나갔다")
		}))
		defer srv.Close()
		c := New("k")
		c.BaseURL = srv.URL
		c.SetTokenSource(&staticToken{err: errors.New("서명 실패")})
		_, err := c.CreateOrder(context.Background(), map[string]any{})
		assertOrderKind(t, err, OrderNotSent, true)
	})

	t.Run("본문이 인코딩 안 되면 전송 전 실패", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("인코딩 못 한 본문이 나갔다")
		}))
		defer srv.Close()
		_, err := armed(t, srv.URL).CreateOrder(context.Background(), map[string]any{"f": func() {}})
		assertOrderKind(t, err, OrderNotSent, true)
	})

	t.Run("2xx 인데 본문을 못 읽으면 불명", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"success":true,"data":{`) // 잘린 JSON
		}))
		defer srv.Close()
		_, err := armed(t, srv.URL).CreateOrder(context.Background(), map[string]any{})
		assertOrderKind(t, err, OrderUnknown, false)
	})

	t.Run("2xx 인데 주문 식별자가 없으면 불명", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"success":true,"data":{"code":"OK"}}`)
		}))
		defer srv.Close()
		_, err := armed(t, srv.URL).CreateOrder(context.Background(), map[string]any{})
		assertOrderKind(t, err, OrderUnknown, false)
	})
}

// HTTP 200 인데 success:false 인 응답을 성공으로 읽으면, 나가지도 않은 주문을
// 살아 있다고 믿는다.
func TestCreateOrderRejectsSuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":false,"message":"insufficient_balance","data":{"orderId":"o"}}`)
	}))
	defer srv.Close()
	_, err := armed(t, srv.URL).CreateOrder(context.Background(), map[string]any{})
	assertOrderKind(t, err, OrderRejected, true)
	if !strings.Contains(err.Error(), "insufficient_balance") {
		t.Errorf("거부 사유가 에러에 없다: %v", err)
	}
}

// 주문 식별자가 숫자로 오면 string 필드는 디코딩 에러를 낸다 — 그러면 이미
// 생성된 주문의 ID 를 통째로 잃는다. 가장 나쁜 실패 모드다.
func TestCreateOrderAcceptsNumericIdentifiers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true,"data":{"orderId":12345,"orderHash":"0xabc"}}`)
	}))
	defer srv.Close()
	res, err := armed(t, srv.URL).CreateOrder(context.Background(), map[string]any{})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	if res.OrderID != "12345" {
		t.Errorf("OrderID = %q, 기대 \"12345\"", res.OrderID)
	}
}

// 식별자가 없는 채로 성공을 돌려주면 취소할 수 없는 주문이 남는다.
// 해시라도 있으면 에러 메시지에 실어 대조할 수 있게 한다.
func TestCreateOrderUnknownErrorCarriesHash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true,"data":{"orderHash":"0xdeadbeef"}}`)
	}))
	defer srv.Close()
	_, err := armed(t, srv.URL).CreateOrder(context.Background(), map[string]any{})
	assertOrderKind(t, err, OrderUnknown, false)
	if !strings.Contains(err.Error(), "0xdeadbeef") {
		t.Errorf("대조에 쓸 해시가 에러에 없다: %v", err)
	}
}

func assertOrderKind(t *testing.T, err error, want OrderErrorKind, wantSafe bool) {
	t.Helper()
	if err == nil {
		t.Fatal("에러가 없다")
	}
	var oe *OrderError
	if !errors.As(err, &oe) {
		t.Fatalf("*OrderError 가 아니다 (%T): %v", err, err)
	}
	if oe.Kind != want {
		t.Errorf("Kind = %v, 기대 %v (에러: %v)", oe.Kind, want, err)
	}
	if oe.SafeToRetry() != wantSafe {
		t.Errorf("SafeToRetry = %v, 기대 %v — 틀리면 이중 주문이거나 주문 유실이다", oe.SafeToRetry(), wantSafe)
	}
}

// --------------------------------------------------------------- RemoveOrders

func TestRemoveOrdersSeparatesRejected(t *testing.T) {
	var gotBody map[string]any
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		decodeJSON(t, r.Body, &gotBody)
		io.WriteString(w, `{"success":true,"data":{
			"removed":["a"],"noop":["b"],"rejected":["c"]}}`)
	}))
	defer srv.Close()

	res, err := armed(t, srv.URL).RemoveOrders(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("RemoveOrders: %v", err)
	}
	if gotAuth == "" {
		t.Error("취소도 Bearer 가 필요하다 — Authorization 헤더가 없다")
	}
	// ids 는 `data` 봉투 안이다(스펙 RemoveOrdersRequest.required = [data]).
	// 이 단언이 최상위 ids 를 보고 있었던 것이 2026-08-11 무장 실패가 시험을
	// 통과한 채로 지나온 이유다. 봉투 자체의 시험은 envelope_test.go 에 있다.
	data, _ := gotBody["data"].(map[string]any)
	ids, _ := data["ids"].([]any)
	if len(ids) != 3 {
		t.Errorf("요청 본문 = %v, 기대 data.ids 3개", gotBody)
	}
	if len(res.Removed) != 1 || res.Removed[0] != "a" {
		t.Errorf("Removed = %v", res.Removed)
	}
	if len(res.Noop) != 1 || res.Noop[0] != "b" {
		t.Errorf("Noop = %v", res.Noop)
	}
	// 여기가 핵심이다. rejected 를 removed 나 noop 에 섞으면 "취소됐다"고 믿고
	// 잊어버린 주문이 살아서 체결된다.
	if len(res.Rejected) != 1 || res.Rejected[0] != "c" {
		t.Errorf("Rejected = %v — 잠금 창 안이라 거부된 주문은 재시도 대상이다", res.Rejected)
	}
	if len(res.Unaccounted) != 0 {
		t.Errorf("Unaccounted = %v, 기대 없음", res.Unaccounted)
	}
}

// 빈 배열과 필드 없음을 같은 것으로 다뤄도 되지만, 요청한 ID 가 세 바구니
// 어디에도 없는 것은 다르다 — 그 주문의 상태를 우리는 모른다.
func TestRemoveOrdersReportsUnaccountedIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true,"data":{"removed":["a"],"noop":[],"rejected":[]}}`)
	}))
	defer srv.Close()
	res, err := armed(t, srv.URL).RemoveOrders(context.Background(), []string{"a", "ghost"})
	if err != nil {
		t.Fatalf("RemoveOrders: %v", err)
	}
	if len(res.Unaccounted) != 1 || res.Unaccounted[0] != "ghost" {
		t.Errorf("Unaccounted = %v, 기대 [ghost]", res.Unaccounted)
	}
}

func TestRemoveOrdersEmptyInputSendsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("빈 목록인데 요청이 나갔다")
	}))
	defer srv.Close()
	res, err := armed(t, srv.URL).RemoveOrders(context.Background(), nil)
	if err != nil {
		t.Fatalf("RemoveOrders: %v", err)
	}
	if len(res.Removed)+len(res.Noop)+len(res.Rejected)+len(res.Unaccounted) != 0 {
		t.Errorf("빈 결과가 아니다: %+v", res)
	}
}

// 스펙 상한은 100개다. 넘겨서 보내면 서버가 무엇을 취소했는지 알 수 없다.
func TestRemoveOrdersRejectsOverBatchLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("상한을 넘겼는데 요청이 나갔다")
	}))
	defer srv.Close()
	ids := make([]string, MaxRemoveIDs+1)
	for i := range ids {
		ids[i] = "x"
	}
	if _, err := armed(t, srv.URL).RemoveOrders(context.Background(), ids); err == nil {
		t.Fatal("상한 초과인데 에러가 없다")
	}
}

func TestRemoveOrdersFailsOnSuccessFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":false,"message":"nope","data":{"removed":["a"]}}`)
	}))
	defer srv.Close()
	if _, err := armed(t, srv.URL).RemoveOrders(context.Background(), []string{"a"}); err == nil {
		t.Fatal("success:false 인데 취소됐다고 보고했다")
	}
}

// ------------------------------------------------------------------ Positions

// 페이징 파라미터를 틀리면 서버가 400 을 주지 않고 조용히 무시한다 —
// 매번 1페이지만 받으면서 아무 에러도 안 난다(P4 실측).
func TestPositionsPaginatesWithFirstAfter(t *testing.T) {
	var queries []url.Values
	pages := []string{
		`{"success":true,"cursor":"P2","data":[
		  {"amount":"2000000000000000000","valueUsd":"0.8","averageBuyPriceUsd":"0.30","pnlUsd":"0.2"}]}`,
		`{"success":true,"cursor":null,"data":[
		  {"amount":1000000000000000000,"valueUsd":0.4,"averageBuyPriceUsd":0.25,"pnlUsd":-0.1}]}`,
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		if n >= len(pages) {
			t.Errorf("페이지를 %d개보다 많이 요청했다", len(pages))
			w.WriteHeader(500)
			return
		}
		io.WriteString(w, pages[n])
		n++
	}))
	defer srv.Close()

	got, err := armed(t, srv.URL).Positions(context.Background())
	if err != nil {
		t.Fatalf("Positions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("포지션 %d개, 기대 2개 — 2페이지를 못 받았다", len(got))
	}
	if len(queries) != 2 {
		t.Fatalf("요청 %d건", len(queries))
	}
	if queries[0].Get("first") == "" {
		t.Errorf("1페이지 쿼리 %v — 페이지 크기 파라미터는 first 다 (limit 이 아니다)", queries[0])
	}
	if queries[0].Get("limit") != "" || queries[1].Get("cursor") != "" {
		t.Errorf("limit/cursor 를 보냈다 — 서버가 조용히 무시한다: %v %v", queries[0], queries[1])
	}
	if queries[1].Get("after") != "P2" {
		t.Errorf("2페이지 쿼리 %v — 커서 파라미터는 after 다", queries[1])
	}
	if got[0].AmountShares != 2 || got[0].AverageBuyPriceUSD != 0.30 {
		t.Errorf("문자열 필드 파싱 실패: %+v", got[0])
	}
	if got[1].AmountShares != 1 || got[1].PnLUSD != -0.1 {
		t.Errorf("숫자 필드 파싱 실패: %+v", got[1])
	}
}

// 커서가 제자리면 3 req/s 무한 루프를 돈다. 페이지 상한이 결국 멈추기는 하지만
// 그건 200페이지(≈66초) 뒤다 — 회차가 5분인 봇에서는 멈춘 것과 같다.
// 그래서 "에러가 났다"가 아니라 "몇 번 만에 멈췄다"를 단언한다.
func TestPositionsStopsOnStuckCursor(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		io.WriteString(w, `{"success":true,"cursor":"SAME","data":[
		  {"amount":"1","valueUsd":"0","averageBuyPriceUsd":"0","pnlUsd":"0"}]}`)
	}))
	defer srv.Close()
	if _, err := armed(t, srv.URL).Positions(context.Background()); err == nil {
		t.Fatal("커서가 전진하지 않는데 에러가 없다")
	}
	if hits > 3 {
		t.Errorf("요청 %d건 — 커서가 제자리인 것을 바로 알아채야 한다", hits)
	}
}

// 페이지가 가득 찼는데 커서가 없다 = 커서 필드 이름이 바뀐 모습이다.
// 그대로 두면 포지션 일부를 못 본 채 조용히 성공한다.
func TestPositionsRejectsFullPageWithoutCursor(t *testing.T) {
	rows := make([]string, positionsPageSize)
	for i := range rows {
		rows[i] = `{"amount":"1000000000000000000","valueUsd":"0.4","averageBuyPriceUsd":"0.25","pnlUsd":"0"}`
	}
	body := `{"success":true,"data":[` + strings.Join(rows, ",") + `]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer srv.Close()
	if _, err := armed(t, srv.URL).Positions(context.Background()); err == nil {
		t.Fatal("가득 찬 페이지에 커서가 없는데 성공으로 봤다")
	}
}

// 응답 파싱이 틀리면 거래소가 무엇을 말했는지 자체를 모르게 된다.
// 사이징이 이 값을 쓰므로 조용한 0 대신 에러를 낸다.
func TestPositionsFailsClosedOnBrokenFields(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"amount 없음", `{"success":true,"data":[{"valueUsd":"1","averageBuyPriceUsd":"0.3","pnlUsd":"0"}]}`},
		{"amount 가 불리언", `{"success":true,"data":[{"amount":true,"averageBuyPriceUsd":"0.3"}]}`},
		{"평균단가 없음", `{"success":true,"data":[{"amount":"1000000000000000000"}]}`},
		{"평균단가가 숫자 아님", `{"success":true,"data":[{"amount":"1","averageBuyPriceUsd":"cheap"}]}`},
		{"success 가 false", `{"success":false,"data":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			if _, err := armed(t, srv.URL).Positions(context.Background()); err == nil {
				t.Fatal("망가진 응답인데 에러가 없다")
			}
		})
	}
}

// --------------------------------------------------------------- ReservedUSDT

func TestReservedUSDTSumsWeiAndSkipsShares(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		decodeJSON(t, r.Body, &gotBody)
		io.WriteString(w, `{"success":true,"data":[
		  {"type":"USDT","amount":"1500000000000000000"},
		  {"type":"SHARE","amount":"9000000000000000000000"},
		  {"type":"USDT","amount":"500000000000000000"}]}`)
	}))
	defer srv.Close()

	got, err := armed(t, srv.URL).ReservedUSDT(context.Background())
	if err != nil {
		t.Fatalf("ReservedUSDT: %v", err)
	}
	if math.Abs(got-2.0) > 1e-9 {
		t.Errorf("예약잔고 = %v, 기대 2.0 (1.5 + 0.5, SHARE 는 제외)", got)
	}
	// **요청 키 이름을 고정한다.** P4 가 `"queries"` 로 추측해 뒀는데 틀렸고,
	// 메인넷 첫 기동이 HTTP 400 으로 돌려줬다("Expected input type
	// list_AssetQuery, found null"). `/docs` 의 OpenAPI
	// ReservedBalancesRequest 는 required 필드가 `assets` 하나다.
	// 이 이름이 틀리면 예약잔고를 영원히 못 읽고 봇은 무장하지 못한다.
	assets, ok := gotBody["assets"].([]any)
	if !ok {
		t.Fatalf("요청 본문에 assets 배열이 없다: %v", gotBody)
	}
	if len(assets) != 1 {
		t.Fatalf("assets %d개, 기대 1개", len(assets))
	}
	item, _ := assets[0].(map[string]any)
	if item["type"] != "USDT" {
		t.Errorf("assets[0].type = %v, 기대 USDT", item["type"])
	}
}

// 금액 필드 이름이 바뀌었는데 0 을 돌려주면 봇은 자유 자금이 실제보다 많다고
// 믿고 한도를 넘겨 주문한다. 실제 돈이 나가는 방향의 실패다 — 막는다.
func TestReservedUSDTFailsClosedOnUnknownShape(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"금액 키를 모른다", `{"success":true,"data":[{"type":"USDT","reserved_wei":"1"}]}`},
		{"type 이 없다", `{"success":true,"data":[{"amount":"1"}]}`},
		{"금액이 숫자가 아니다", `{"success":true,"data":[{"type":"USDT","amount":"많음"}]}`},
		{"금액이 음수다", `{"success":true,"data":[{"type":"USDT","amount":"-1"}]}`},
		// strconv.ParseFloat 은 "NaN"/"Inf"/"infinity" 를 **에러 없이** 파싱한다.
		// 이 값들이 통과하면 예약잔고가 NaN 이 되고, 가용 자금 계산이 통째로
		// 무의미해진다(Task 3 이 <=0 가드를 NaN 이 통과하는 것을 찾은 것과 같은 부류).
		{"금액이 NaN", `{"success":true,"data":[{"type":"USDT","amount":"NaN"}]}`},
		{"금액이 Inf", `{"success":true,"data":[{"type":"USDT","amount":"Inf"}]}`},
		{"금액이 infinity", `{"success":true,"data":[{"type":"USDT","amount":"infinity"}]}`},
		{"금액이 -Inf", `{"success":true,"data":[{"type":"USDT","amount":"-Inf"}]}`},
		{"data 가 객체다", `{"success":true,"data":{"type":"USDT","amount":"1"}}`},
		{"success 가 false", `{"success":false,"data":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			got, err := armed(t, srv.URL).ReservedUSDT(context.Background())
			if err == nil {
				t.Fatalf("망가진 응답인데 %v 를 돌려줬다", got)
			}
		})
	}
}

// 아무것도 예약돼 있지 않은 것은 정상이다 — 이건 에러가 아니다.
func TestReservedUSDTEmptyIsZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true,"data":[]}`)
	}))
	defer srv.Close()
	got, err := armed(t, srv.URL).ReservedUSDT(context.Background())
	if err != nil || got != 0 {
		t.Fatalf("= %v, %v — 빈 목록은 예약 0 이다", got, err)
	}
}

// 터무니없이 큰 값이 float64 에서 +Inf 가 되면 사이징이 무한대 잔여로 계산한다.
func TestReservedUSDTRejectsNonFinite(t *testing.T) {
	huge := "9" + strings.Repeat("0", 400)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true,"data":[{"type":"USDT","amount":"`+huge+`"}]}`)
	}))
	defer srv.Close()
	got, err := armed(t, srv.URL).ReservedUSDT(context.Background())
	if err == nil {
		t.Fatalf("= %v — 유한하지 않은 값을 통과시켰다", got)
	}
}

// ------------------------------------------------------- 계정 스코프 엔드포인트

// "한쪽만 막았다"를 막는다. 계정 스코프 엔드포인트 넷 전부가 Bearer 를 보내야
// 하고, 하나라도 빠지면 401 이다. 반대로 TokenSource 가 없으면 요청 자체가
// 나가면 안 된다 — 무인증으로 나간 요청은 조용히 401 로 돌아온다.
func TestAccountScopedEndpointsAllUseBearer(t *testing.T) {
	calls := []struct {
		name string
		fn   func(*Client) error
	}{
		{"CreateOrder", func(c *Client) error { _, err := c.CreateOrder(context.Background(), map[string]any{}); return err }},
		{"RemoveOrders", func(c *Client) error { _, err := c.RemoveOrders(context.Background(), []string{"a"}); return err }},
		{"Positions", func(c *Client) error { _, err := c.Positions(context.Background()); return err }},
		{"ReservedUSDT", func(c *Client) error { _, err := c.ReservedUSDT(context.Background()); return err }},
	}
	for _, call := range calls {
		t.Run(call.name+" 는 Bearer 를 보낸다", func(t *testing.T) {
			var auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth = r.Header.Get("Authorization")
				io.WriteString(w, `{"success":true,"cursor":null,"data":[]}`)
			}))
			defer srv.Close()
			_ = call.fn(armed(t, srv.URL))
			if auth != "Bearer test-jwt-token" {
				t.Errorf("Authorization = %q", auth)
			}
		})
		t.Run(call.name+" 는 토큰 없이 나가지 않는다", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("TokenSource 가 없는데 요청이 나갔다")
			}))
			defer srv.Close()
			c := New("k")
			c.BaseURL = srv.URL
			if err := call.fn(c); err == nil {
				t.Error("TokenSource 가 없는데 에러가 없다")
			}
		})
	}
}

func decodeJSON(t *testing.T, r io.Reader, out any) {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("본문 읽기: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("본문 디코딩: %v (%s)", err, b)
	}
}
