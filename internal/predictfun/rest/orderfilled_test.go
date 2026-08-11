package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 이 파일은 `GET /v1/orders/{hash}` 파싱을 **2026-08-12 실측 응답 원문**으로
// 고정한다. 이 값 하나가 "취소된 주문이 사실은 체결됐다"를 가려내는 근거이고,
// 그것을 못 가려내서 회차 명목이 상한의 1.9배가 됐다.

// measuredOrderResponse 는 실제로 받은 응답이다(주소·서명은 지웠다).
const measuredOrderResponse = `{"data":{"amount":"2000000000000000000","amountFilled":"2000000000000000000","currency":"USDT","id":"9999999999","isNegRisk":false,"isYieldBearing":false,"marketId":1300000,"order":{"expiration":4102444800,"feeRateBps":"200","makerAmount":"1320000000000000000","nonce":"0","salt":"1","side":0,"signatureType":0,"takerAmount":"2000000000000000000"},"rewardEarningRate":0,"status":"FILLED","strategy":"LIMIT"},"success":true}`

func filledServer(t *testing.T, status int, body string) (*Client, *string) {
	t.Helper()
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c := New("k")
	c.BaseURL = srv.URL
	c.SetTokenSource(tokenFunc(func(context.Context) (string, error) { return "tok", nil }))
	return c, &path
}

func TestOrderFilledReadsTheMeasuredResponse(t *testing.T) {
	c, path := filledServer(t, 200, measuredOrderResponse)

	got, err := c.OrderFilled(context.Background(), "0x1111111111111111111111111111111111111111")
	if err != nil {
		t.Fatalf("OrderFilled: %v", err)
	}
	// amountFilled 2e18 = 2주. wei 그대로 읽으면 2e18 이 나오고, 그 값은
	// 노출을 천문학적으로 부풀려 회차를 통째로 멈춘다.
	if got != 2 {
		t.Fatalf("체결 주수 = %v, want 2 (amountFilled 2e18 / 1e18)", got)
	}
	if !strings.HasPrefix(*path, "/v1/orders/0x") {
		t.Fatalf("요청 경로 = %q — 해시가 경로에 실려야 한다", *path)
	}
}

// **숫자 id 로 부르면 404 다**(실측). 그래서 해시가 없으면 요청 자체를
// 보내지 않는다 — 보내 봐야 404 만 받고 예산만 쓴다.
func TestOrderFilledRefusesAnEmptyHash(t *testing.T) {
	c, path := filledServer(t, 200, measuredOrderResponse)
	if _, err := c.OrderFilled(context.Background(), ""); err == nil {
		t.Fatal("빈 해시가 통과했다")
	}
	if *path != "" {
		t.Fatalf("요청이 나갔다: %q", *path)
	}
}

// 부분 체결.
func TestOrderFilledReadsPartialFills(t *testing.T) {
	c, _ := filledServer(t, 200,
		`{"success":true,"data":{"amount":"8000000000000000000","amountFilled":"2500000000000000000","status":"CANCELLED"}}`)
	got, err := c.OrderFilled(context.Background(), "0xabc")
	if err != nil {
		t.Fatalf("OrderFilled: %v", err)
	}
	if got != 2.5 {
		t.Fatalf("체결 주수 = %v, want 2.5", got)
	}
}

// **amountFilled 가 없으면 에러다. 0 이 아니다.**
//
// 0 으로 떨어뜨리면 "이 주문은 안 찼다"가 되고, 그것이 정확히 이 함수가
// 막으려는 거짓말이다. 필드 이름이 바뀌는 날 이 구분이 전부다 —
// encoding/json 은 모르는 이름을 조용히 제로값으로 둔다.
func TestMissingAmountFilledIsAnErrorNotZero(t *testing.T) {
	for _, body := range []string{
		`{"success":true,"data":{"amount":"8000000000000000000","status":"CANCELLED"}}`,
		`{"success":true,"data":{}}`,
		`{"success":true}`,
	} {
		c, _ := filledServer(t, 200, body)
		got, err := c.OrderFilled(context.Background(), "0xabc")
		if err == nil {
			t.Errorf("amountFilled 없는 응답이 %v 로 통과했다: %s", got, body)
		}
	}
}

// 음수는 노출을 **깎는** 방향이다. 통과시키지 않는다.
func TestNegativeAmountFilledIsRejected(t *testing.T) {
	c, _ := filledServer(t, 200,
		`{"success":true,"data":{"amount":"8000000000000000000","amountFilled":"-1000000000000000000"}}`)
	if _, err := c.OrderFilled(context.Background(), "0xabc"); err == nil {
		t.Fatal("음수 amountFilled 가 통과했다")
	}
}

// 404 는 에러다 — 그 주문을 모른다는 뜻이지 "안 찼다"가 아니다.
func TestNotFoundIsAnError(t *testing.T) {
	c, _ := filledServer(t, 404,
		`{"success":false,"code":404,"error":"not_found","message":"order not found"}`)
	if _, err := c.OrderFilled(context.Background(), "0xabc"); err == nil {
		t.Fatal("404 가 통과했다")
	}
}

// 200 인데 success:false 인 경우도 에러다.
func TestSuccessFalseIsAnError(t *testing.T) {
	c, _ := filledServer(t, 200,
		`{"success":false,"message":"nope","data":{"amountFilled":"0"}}`)
	if _, err := c.OrderFilled(context.Background(), "0xabc"); err == nil {
		t.Fatal("success:false 가 통과했다")
	}
}
