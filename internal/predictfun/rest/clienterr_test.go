package rest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 계획서 Step 1 의 테스트. 거래소가 왜 거부했는지가 사라지면 안 되고, 그렇다고
// 시크릿이 새어서도 안 된다. 시크릿은 요청 쪽(헤더)에 있고 응답 쪽에는 없다 —
// 그 사실 자체를 여기서 고정한다.
//
// 계획서 원문은 SetTokenSource 없이 CreateOrder 를 부르는데, 주문 생성은
// Bearer 가 필요한 엔드포인트라 그러면 요청이 나가기도 전에 실패해 422 본문을
// 볼 수 없다. 그래서 토큰을 배선하고, 토큰도 새지 않는지 함께 본다.
func TestErrorIncludesBodyButNeverSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		io.WriteString(w, `{"success":false,"error":"invalid_signature","message":"bad sig"}`)
	}))
	defer srv.Close()

	c := New("SECRET-API-KEY-DO-NOT-LEAK")
	c.BaseURL = srv.URL
	c.SetTokenSource(&staticToken{tok: "SECRET-JWT-DO-NOT-LEAK"})
	_, err := c.CreateOrder(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("422 인데 에러가 없다")
	}
	if !strings.Contains(err.Error(), "invalid_signature") {
		t.Errorf("거부 사유가 에러에 없다: %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-API-KEY-DO-NOT-LEAK") {
		t.Fatal("에러 메시지에 API 키가 샜다")
	}
	if strings.Contains(err.Error(), "SECRET-JWT-DO-NOT-LEAK") {
		t.Fatal("에러 메시지에 Bearer 토큰이 샜다")
	}
}

// "시크릿은 응답 쪽에 없다"는 가정이 깨지는 경우가 하나 있다 — 서버가 우리가
// 보낸 값을 에러에 되돌려주는 것. 본문을 에러에 싣기로 한 이상 그 경로가 열린다.
func TestErrorRedactsSecretsEchoedByServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"message":"unknown api key SECRET-API-KEY-DO-NOT-LEAK for token SECRET-JWT-DO-NOT-LEAK"}`)
	}))
	defer srv.Close()

	c := New("SECRET-API-KEY-DO-NOT-LEAK")
	c.BaseURL = srv.URL
	c.SetTokenSource(&staticToken{tok: "SECRET-JWT-DO-NOT-LEAK"})
	err := c.GetAuth(context.Background(), "/v1/positions", nil, &struct{}{})
	if err == nil {
		t.Fatal("401 인데 에러가 없다")
	}
	if strings.Contains(err.Error(), "SECRET-API-KEY-DO-NOT-LEAK") || strings.Contains(err.Error(), "SECRET-JWT-DO-NOT-LEAK") {
		t.Fatalf("서버가 되돌려준 시크릿이 에러에 그대로 남았다: %v", err)
	}
	if !strings.Contains(err.Error(), "unknown api key") {
		t.Errorf("거부 사유까지 지워졌다: %v", err)
	}
}

// 이 저장소가 여덟 번 밟은 함정: Get 에 고친 것을 Post 에 안 했거나 그 반대.
// 두 동사를 같은 표로 돌린다.
func TestBothVerbsIncludeBodyInError(t *testing.T) {
	for _, v := range allVerbs() {
		t.Run(v.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(400)
				io.WriteString(w, `{"code":"WHY_IT_WAS_REJECTED"}`)
			}))
			defer srv.Close()
			err := v.call(armedClient(srv.URL), &struct{}{})
			if err == nil {
				t.Fatal("400 인데 에러가 없다")
			}
			if !strings.Contains(err.Error(), "WHY_IT_WAS_REJECTED") {
				t.Errorf("본문이 에러에 없다: %v", err)
			}
			var he *HTTPError
			if !errors.As(err, &he) || he.Status != 400 {
				t.Errorf("*HTTPError 로 분류되지 않았다: %v", err)
			}
		})
	}
}

// 본문이 길면 앞부분만 싣는다 — 로그를 메가바이트로 채우지 않는다.
func TestErrorBodyIsTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		io.WriteString(w, strings.Repeat("A", 100000))
	}))
	defer srv.Close()
	err := armedClient(srv.URL).Get(context.Background(), "/v1/x", nil, &struct{}{})
	if err == nil {
		t.Fatal("500 인데 에러가 없다")
	}
	if n := len(err.Error()); n > maxErrorBody+200 {
		t.Errorf("에러 길이 %d — 본문을 잘라야 한다", n)
	}
}

// POST /v1/orders 가 201 을 주면 성공한 주문이 에러로 보고된다. 그러면 봇이
// 재시도하고 노출이 두 배가 된다 — 이 패키지에서 가장 나쁜 실패 모드다.
// Get 에도 같은 규칙을 적용한다("한쪽만 막았다" 방지).
func TestBothVerbsAcceptAll2xx(t *testing.T) {
	for _, v := range allVerbs() {
		for _, code := range []int{200, 201, 202, 299} {
			t.Run(v.name+"/수락"+strconv.Itoa(code), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(code)
					io.WriteString(w, `{"success":true,"data":{"orderId":"o"}}`)
				}))
				defer srv.Close()
				var out struct {
					Success bool `json:"success"`
				}
				if err := v.call(armedClient(srv.URL), &out); err != nil {
					t.Fatalf("HTTP %d 인데 에러다: %v", code, err)
				}
				if !out.Success {
					t.Error("본문 디코딩 실패")
				}
			})
		}
		// 1xx 는 넣지 않는다 — net/http 가 그것을 정보성 응답으로 처리해 뒤이은
		// 본문 쓰기에서 200 을 다시 보내므로, 서버 쪽에서 재현할 수가 없다.
		for _, code := range []int{300, 301, 400, 500} {
			t.Run(v.name+"/거부"+strconv.Itoa(code), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(code)
					io.WriteString(w, `{"success":true}`)
				}))
				defer srv.Close()
				if err := v.call(armedClient(srv.URL), &struct{}{}); err == nil {
					t.Fatalf("HTTP %d 인데 성공으로 봤다", code)
				}
			})
		}
	}
}

// 2xx 인데 본문이 비면 out 을 채울 수 없다. 조용히 제로값을 남기면 호출자는
// "포지션 0건" 같은 거짓을 사실로 읽는다.
func TestEmptyBodyIsAnErrorWhenOutputExpected(t *testing.T) {
	for _, v := range allVerbs() {
		t.Run(v.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(204)
			}))
			defer srv.Close()
			if err := v.call(armedClient(srv.URL), &struct{}{}); err == nil {
				t.Fatal("빈 본문인데 에러가 없다")
			}
			// out 이 nil 이면 본문을 안 읽으므로 정상이다.
			if err := v.callNoOut(armedClient(srv.URL)); err != nil {
				t.Errorf("out 이 nil 인데 에러다: %v", err)
			}
		})
	}
}

// 인증이 필요 없는 엔드포인트(/v1/categories 등)에 Bearer 를 붙이지 않는다.
// 더 중요한 이유가 있다 — auth.Authenticator 자신이 Get/Post 로 토큰을 받는다.
// 모든 요청에 토큰을 붙이면 토큰을 받으러 가는 요청이 토큰을 요구한다.
func TestPlainVerbsNeverAskForToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		io.WriteString(w, `{"success":true}`)
	}))
	defer srv.Close()

	ts := &staticToken{tok: "tok"}
	c := New("k")
	c.BaseURL = srv.URL
	c.SetTokenSource(ts)

	if err := c.Get(context.Background(), "/v1/categories", nil, &struct{}{}); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Get 이 Authorization 을 붙였다: %q", gotAuth)
	}
	if err := c.Post(context.Background(), "/v1/auth", map[string]any{}, &struct{}{}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Post 가 Authorization 을 붙였다: %q", gotAuth)
	}
	if n := ts.n.Load(); n != 0 {
		t.Errorf("TokenSource 를 %d번 불렀다 — 인증 불필요 경로는 부르면 안 된다", n)
	}
}

// reentrantSource 는 auth.Authenticator 를 그대로 흉내낸다: 락을 쥔 채로
// 두 번의 REST 왕복을 한다(그 락은 의도적으로 깊다 — auth.Token 주석 참고).
// Get/Post 가 토큰을 요구하면 여기서 자기 자신의 뮤텍스에 걸려 영영 멈춘다.
type reentrantSource struct {
	mu sync.Mutex
	c  *Client
}

func (s *reentrantSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.c.Get(ctx, "/v1/auth/message", url.Values{"address": {"0xAbCd"}}, &struct{}{}); err != nil {
		return "", err
	}
	if err := s.c.Post(ctx, "/v1/auth", map[string]any{}, &struct{}{}); err != nil {
		return "", err
	}
	return "issued-token", nil
}

func TestTokenSourceMayCallTheClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true,"data":{"orderId":"o"}}`)
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	src := &reentrantSource{c: c}
	c.SetTokenSource(src)

	done := make(chan error, 1)
	go func() {
		_, err := c.CreateOrder(context.Background(), map[string]any{})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CreateOrder: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("교착했다 — 토큰을 받으러 가는 요청이 토큰을 요구하고 있다")
	}
}

// 마켓메이킹 루프는 이 클라이언트를 여러 고루틴에서 쓰고, 토큰 갱신은 그 사이에
// 끼어든다. -race 로 도는 회귀 테스트다.
func TestSetTokenSourceIsRaceFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":true,"cursor":null,"data":[]}`)
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	c.SetTokenSource(&staticToken{tok: "a"})

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.SetTokenSource(&staticToken{tok: "b"})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Positions(context.Background())
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------- 테스트 도구

type verb struct {
	name      string
	call      func(*Client, any) error
	callNoOut func(*Client) error
}

// allVerbs 는 네 조합(Get/Post × 무인증/Bearer)을 전부 돌린다. 새 동사를
// 추가하면 여기에 넣어야 위 테스트 전부가 그것에도 적용된다.
func allVerbs() []verb {
	return []verb{
		{"Get", func(c *Client, out any) error { return c.Get(context.Background(), "/v1/x", nil, out) },
			func(c *Client) error { return c.Get(context.Background(), "/v1/x", nil, nil) }},
		{"Post", func(c *Client, out any) error { return c.Post(context.Background(), "/v1/x", map[string]any{}, out) },
			func(c *Client) error { return c.Post(context.Background(), "/v1/x", map[string]any{}, nil) }},
		{"GetAuth", func(c *Client, out any) error { return c.GetAuth(context.Background(), "/v1/x", nil, out) },
			func(c *Client) error { return c.GetAuth(context.Background(), "/v1/x", nil, nil) }},
		{"PostAuth", func(c *Client, out any) error {
			return c.PostAuth(context.Background(), "/v1/x", map[string]any{}, out)
		},
			func(c *Client) error { return c.PostAuth(context.Background(), "/v1/x", map[string]any{}, nil) }},
	}
}

func armedClient(baseURL string) *Client {
	c := New("test-key")
	c.BaseURL = baseURL
	c.SetTokenSource(&staticToken{tok: "test-jwt-token"})
	return c
}

// ---------------------------------------------------------------------------
// Redact — 계정 주소가 쿼리에 실린다
// ---------------------------------------------------------------------------

// **전송이 실패하면 net/http 는 *url.Error 에 전체 URL 을 그대로 싣는다.**
// `GET /v1/orders/matches` 는 계정 주소를 signerAddress 쿼리로 받으므로,
// 그 실패 한 번이 봇 로그에 주소를 남긴다 — 이 저장소는 주소를 로그에
// 남기지 않기로 했고(cmd/gld91 패키지 문서), 그 규약을 클라이언트가 지킨다.
func TestRedactRemovesQueryValuesFromTransportErrors(t *testing.T) {
	// 자리표시자다. 실계정과 무관하고 대소문자를 섞었다 — 응답·설정이
	// 체크섬 표기로 오가는데 한 형태만 지우면 다른 형태가 그대로 남는다.
	const addr = "0xAbCd1111222233334444555566667777888899Ff"

	// 닫힌 포트로 보내 전송 실패를 만든다.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close()

	c := New("api-key-placeholder-long-enough")
	c.BaseURL = base
	c.Redact(addr)

	q := url.Values{}
	q.Set("signerAddress", addr)
	err := c.Get(context.Background(), "/v1/orders/matches", q, &struct{}{})
	if err == nil {
		t.Fatal("닫힌 서버인데 에러가 없다")
	}
	for _, form := range []string{addr, strings.ToLower(addr)} {
		if strings.Contains(err.Error(), form) {
			t.Fatalf("계정 주소가 에러에 그대로 남았다 (%s): %v", form, err)
		}
	}
}

// 소문자 형태로 쿼리에 실려도 지워야 한다. 등록은 체크섬 표기로 했더라도.
func TestRedactCoversLowercaseForm(t *testing.T) {
	const addr = "0xAbCd1111222233334444555566667777888899Ff"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close()

	c := New("api-key-placeholder-long-enough")
	c.BaseURL = base
	c.Redact(addr)

	q := url.Values{}
	q.Set("signerAddress", strings.ToLower(addr))
	err := c.Get(context.Background(), "/v1/orders/matches", q, &struct{}{})
	if err == nil {
		t.Fatal("닫힌 서버인데 에러가 없다")
	}
	if strings.Contains(err.Error(), strings.ToLower(addr)) {
		t.Fatalf("소문자 주소가 에러에 남았다: %v", err)
	}
}

// 너무 짧은 값은 등록하지 않는다. 세 글자를 전역 치환하면 에러 메시지가
// 알아볼 수 없게 망가진다.
func TestRedactIgnoresShortValues(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"code":"BAD_ORDER"}`)
	}))
	defer srv.Close()

	c := New("api-key-placeholder-long-enough")
	c.BaseURL = srv.URL
	c.Redact("BAD")
	err := c.Get(context.Background(), "/v1/orders/matches", nil, &struct{}{})
	if err == nil {
		t.Fatal("400 인데 에러가 없다")
	}
	if !strings.Contains(err.Error(), "BAD_ORDER") {
		t.Errorf("짧은 값을 지우느라 거부 사유가 망가졌다: %v", err)
	}
}
