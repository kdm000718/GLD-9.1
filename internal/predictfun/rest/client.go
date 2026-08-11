// Package rest 는 predict.fun REST API 클라이언트다.
//
// 읽기(카테고리·포지션)와 쓰기(주문 생성·취소)를 같은 클라이언트가 한다.
// 레이트리밋(240 req/min)이 API 키 단위이므로 두 종류를 다른 클라이언트로
// 나누면 스로틀이 두 배로 새어나간다.
//
// # 인증이 엔드포인트마다 다르다
//
// `/v1/categories` 는 x-api-key 만으로 200 이고, `/v1/account`·`/v1/orders`·
// `/v1/positions` 는 Bearer JWT 없이는 401 이다(P4 실측). 스펙의 security 가
// 엔드포인트별로 다르게 선언돼 있다. 그래서 Bearer 는 모든 요청에 자동으로
// 붙지 않고, 호출부가 GetAuth/PostAuth 를 골라서 쓴다.
//
// **모든 요청에 자동으로 붙이면 안 되는 결정적인 이유**: 토큰을 발급하는
// auth.Authenticator 자신이 이 클라이언트의 Get/Post 로 `/v1/auth/message`·
// `/v1/auth` 를 부른다. 자동 주입은 "토큰을 받으러 가는 요청이 토큰을 요구하는"
// 재귀가 되고, Authenticator 가 락을 쥔 채 왕복하므로 그 재귀는 교착으로
// 끝난다. clienterr_test.go 의 TestTokenSourceMayCallTheClient 가 그것을 고정한다.
package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// UserAgent 는 명시해야 한다. 기본 Go User-Agent 는 WAF 가 403 으로 막는다.
const UserAgent = "gld91/0.1 (+https://github.com/kdm000718/GLD-9.1)"

// 레이트리밋은 키당 240 req/min = 4 req/s. 여유를 두고 3 req/s 로 간다.
const minInterval = 333 * time.Millisecond

// maxErrorBody 는 에러에 싣는 응답 본문의 상한(바이트)이다.
//
// 본문을 아예 버리면 거래소가 왜 거부했는지가 사라진다 — P4 에서 실제로 값을
// 치렀다(`/v1/auth/jwt` 가 404 였는데 원인을 스펙을 뒤져서야 알았다). 반대로
// 전부 실으면 에러 한 줄이 메가바이트가 될 수 있다. 앞 512 바이트면 JSON
// 에러 응답의 code/message 는 사실상 항상 들어온다.
const maxErrorBody = 512

// TokenSource 는 요청마다 Bearer 토큰을 준다. auth.Authenticator 가 이걸
// 만족한다. 캐시·갱신은 구현체의 책임이다 — 이 패키지는 매 요청 Token 을 부르고
// 그 값을 저장하지 않는다.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

type Client struct {
	BaseURL string
	apiKey  string
	http    *http.Client

	// mu 는 last 를 보호한다. 마켓메이킹 루프는 이 클라이언트를 여러 고루틴에서
	// 동시에 부르므로 뮤텍스가 없으면 데이터 레이스이면서 동시에 레이트리밋이
	// 무력화된다 — 동시 호출자가 모두 같은 last 를 읽고 한 창에 다 나간다.
	mu   sync.Mutex
	last time.Time

	// tsMu 는 tokenSource 만 보호한다. mu 와 나누는 이유: mu 는 스로틀 대기
	// 내내 잡혀 있어서(throttle 주석 참고) 그 위에 토큰 읽기를 얹으면 토큰
	// 조회가 최대 333ms 씩 막힌다.
	tsMu        sync.Mutex
	tokenSource TokenSource

	// redMu 는 extraRedact 를 보호한다.
	redMu       sync.Mutex
	extraRedact []string

	// rlMu 는 남은 요청 예산을 보호한다. mu 와 나누는 이유는 tsMu 와 같다 —
	// mu 는 스로틀 대기 내내 잡혀 있고, 예산 읽기는 하트비트 경로에서
	// 불리므로 그 대기에 묶이면 안 된다. 자세한 사정은 ratelimit.go 참고.
	rlMu        sync.Mutex
	rlRemaining int
	rlAt        time.Time
}

// Redact 는 에러 문자열에서 지울 값을 더한다.
//
// # 왜 필요한가
//
// 지금까지 지울 것은 헤더에 실리는 API 키와 Bearer 토큰뿐이었다. 그런데
// `GET /v1/orders/matches` 는 계정 주소를 **쿼리 문자열**로 받는다
// (signerAddress). 전송이 실패하면 net/http 는 *url.Error 에 전체 URL 을
// 그대로 싣고, 그 에러는 봇 로그에 찍히고 로그는 보고서에 붙는다.
//
// 이 저장소는 주소를 로그에 남기지 않는다(cmd/gld91 패키지 문서). 그 규약이
// "이 엔드포인트만 조심하면 된다"로 유지되면 언젠가 깨지므로, 클라이언트가
// 통째로 지우게 한다 — Get·Post·GetAuth·PostAuth 가 do 하나로 모여 있으니
// 여기 한 곳이면 네 동사 전부에 걸린다.
//
// 너무 짧은 값은 무시한다. 세 글자를 전역 치환하면 에러 메시지가 알아볼 수
// 없게 망가진다(secrets 와 같은 규칙이다).
func (c *Client) Redact(values ...string) {
	c.redMu.Lock()
	defer c.redMu.Unlock()
	for _, v := range values {
		if len(v) < 8 {
			continue
		}
		// 주소는 체크섬 대소문자가 섞여 오간다. 우리가 쿼리에 실은 형태와
		// 소문자 형태를 둘 다 등록한다 — 한 형태만 지우면 다른 형태가 그대로
		// 로그에 남는다("한쪽만 막았다"의 전형이다).
		c.extraRedact = append(c.extraRedact, v)
		if l := strings.ToLower(v); l != v {
			c.extraRedact = append(c.extraRedact, l)
		}
	}
}

func New(apiKey string) *Client {
	return &Client{
		BaseURL: "https://api.predict.fun",
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
		// 아직 아무 요청도 보내지 않았으므로 예산은 실제로 가득 차 있다.
		// 0 으로 두면 첫 요청 전의 하트비트가 감시 규칙에 거짓 Crit 을 만든다.
		rlRemaining: RateLimitPerMin,
	}
}

// SetTokenSource 는 Bearer 인증이 필요한 호출(GetAuth/PostAuth)이 쓸 토큰
// 공급자를 배선한다. 배선하지 않은 채 그 호출을 하면 요청을 보내지 않고
// 에러를 낸다 — 무인증으로 나간 요청은 401 로 돌아올 뿐이지만, 그 401 을
// "거래소가 주문을 거부했다"로 읽는 것이 더 나쁘다.
func (c *Client) SetTokenSource(ts TokenSource) {
	c.tsMu.Lock()
	defer c.tsMu.Unlock()
	c.tokenSource = ts
}

func (c *Client) tokenSourceRef() TokenSource {
	c.tsMu.Lock()
	defer c.tsMu.Unlock()
	return c.tokenSource
}

// Get 은 path 로 GET 하고 응답 JSON 을 out 에 디코딩한다. x-api-key 만 붙는다.
// 에러 메시지에 API 키를 절대 넣지 않는다.
func (c *Client) Get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, q, nil, out, false)
}

// GetAuth 는 Get 과 같지만 Bearer 토큰을 함께 보낸다. `/v1/positions` 처럼
// 계정 스코프 엔드포인트에 쓴다.
func (c *Client) GetAuth(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, q, nil, out, true)
}

// Post 는 body 를 JSON 으로 인코딩해 path 로 POST 하고 응답 JSON 을 out 에
// 디코딩한다. Get 과 같은 헤더 설정과 throttle(ctx) 을 그대로 따른다 —
// 레이트리밋은 API 키 단위이고 인증 요청도 같은 예산에서 나가므로 별도의
// 우회 경로를 만들지 않는다. x-api-key 만 붙는다(`/v1/auth` 가 이걸 쓴다).
// 에러 메시지에 API 키를 절대 넣지 않는다.
func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, out, false)
}

// PostAuth 는 Post 와 같지만 Bearer 토큰을 함께 보낸다. `/v1/orders` 계열이
// 이걸 쓴다.
func (c *Client) PostAuth(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, out, true)
}

// HTTPError 는 2xx 가 아닌 응답이다. 상태 코드만으로는 거래소가 무엇을 문제
// 삼았는지 알 수 없으므로 본문 앞 maxErrorBody 바이트를 함께 싣는다.
//
// 본문에 시크릿이 실릴 일은 없다 — 시크릿은 요청 헤더에 있고 응답에는 없다.
// 그래도 서버가 우리가 보낸 값을 에러에 되돌려주는 경우가 있을 수 있어
// Body 는 저장 전에 API 키·토큰을 지운 문자열이다.
type HTTPError struct {
	Path   string
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("%s: HTTP %d (본문 없음)", e.Path, e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Path, e.Status, e.Body)
}

// transportError 는 응답을 받기 전에 끝난 실패다.
//
// Sent 가 이 타입의 존재 이유다. 주문 생성에서 "보내지 않았다"와 "보냈는데
// 결과를 모른다"는 정반대의 조치를 요구한다 — 앞은 재시도해야 하고 뒤는
// 재시도하면 이중 주문이 된다. 상위(OrderError)가 이 값으로 그 둘을 가른다.
type transportError struct {
	Path string
	Sent bool
	Err  error
}

func (e *transportError) Error() string {
	if e.Sent {
		return fmt.Sprintf("%s: 요청은 나갔으나 응답을 받지 못했다: %v", e.Path, e.Err)
	}
	return fmt.Sprintf("%s: 요청을 보내지 못했다: %v", e.Path, e.Err)
}

func (e *transportError) Unwrap() error { return e.Err }

// decodeError 는 2xx 를 받았는데 본문을 우리 타입으로 못 읽은 경우다.
// HTTPError 와 갈라 두는 이유: 주문 생성에서 이건 "거부"가 아니라 "결과 불명"
// 이다. 주문은 이미 들어갔을 수 있다.
type decodeError struct {
	Path string
	Err  error
}

func (e *decodeError) Error() string {
	return fmt.Sprintf("응답 디코딩 실패 %s: %v", e.Path, e.Err)
}

func (e *decodeError) Unwrap() error { return e.Err }

// do 는 Get/Post/GetAuth/PostAuth 의 공통 경로다.
//
// 네 동사를 한 함수로 모은 것은 의도적이다. 이 저장소는 "Get 에만 고치고
// Post 에는 안 고쳤다"를 여덟 번 반복했고, 비200 본문 보존과 2xx 수용도
// 정확히 그렇게 한쪽만 고쳐질 뻔한 변경이다. 갈래가 하나면 그럴 수가 없다.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any, needBearer bool) error {
	// --- 전송 전 단계. 여기서 실패하면 요청은 확실히 나가지 않았다. ---
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return &transportError{Path: path, Err: fmt.Errorf("요청 본문 인코딩 실패: %w", err)}
		}
		payload = b
	}

	var token string
	if needBearer {
		ts := c.tokenSourceRef()
		if ts == nil {
			return &transportError{Path: path, Err: errors.New("Bearer 인증이 필요한 엔드포인트인데 TokenSource 가 배선되지 않았다")}
		}
		t, err := ts.Token(ctx)
		if err != nil {
			return &transportError{Path: path, Err: fmt.Errorf("토큰 발급 실패: %w", err)}
		}
		if t == "" {
			return &transportError{Path: path, Err: errors.New("TokenSource 가 빈 토큰을 돌려줬다")}
		}
		token = t
	}
	secrets := c.secrets(token)

	if err := c.throttle(ctx); err != nil {
		return &transportError{Path: path, Err: err}
	}

	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return &transportError{Path: path, Err: fmt.Errorf("요청 생성 실패: %w", err)}
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("User-Agent", UserAgent)
	if payload != nil {
		// JSON 본문을 보낼 때만 붙인다.
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	// --- 전송. 여기서부터는 요청이 나갔을 수 있다. ---
	resp, err := c.http.Do(req)
	if err != nil {
		return &transportError{Path: path, Sent: !neverLeftTheHost(err), Err: redactErr(err, secrets)}
	}
	defer resp.Body.Close()

	// **상태 코드 검사보다 먼저 읽는다.** 429 야말로 남은 예산을 가장 알고
	// 싶은 순간이다(ratelimit.go 참고).
	c.noteRateLimit(resp.Header)

	// 2xx 전부를 성공으로 본다. 200 만 성공으로 보면 POST /v1/orders 가 201 을
	// 줄 때 **성공한 주문이 에러로 보고된다** — 봇은 실패로 알고 재시도하고
	// 노출이 두 배가 된다. 이 패키지에서 가장 나쁜 실패 모드다.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &HTTPError{
			Path:   path,
			Status: resp.StatusCode,
			Body:   readErrorBody(resp.Body, secrets),
		}
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			// 2xx 인데 본문이 비었다. 조용히 제로값을 남기면 호출자는 "포지션
			// 0건" 같은 거짓을 사실로 읽는다.
			return &decodeError{Path: path, Err: errors.New("응답 본문이 비었다")}
		}
		return &decodeError{Path: path, Err: redactErr(err, secrets)}
	}
	return nil
}

// readErrorBody 는 비2xx 응답의 앞부분을 읽어 에러에 실을 문자열로 만든다.
// 읽기 자체가 실패하면 빈 문자열이다 — 에러를 만드는 도중에 다시 에러를 내지
// 않는다.
func readErrorBody(r io.Reader, secrets []string) string {
	b, err := io.ReadAll(io.LimitReader(r, maxErrorBody+1))
	if err != nil && len(b) == 0 {
		return ""
	}
	suffix := ""
	if len(b) > maxErrorBody {
		b = b[:maxErrorBody]
		suffix = "…(잘림)"
	}
	// 제어문자는 로그 한 줄을 깨뜨린다.
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, string(b))
	return redact(strings.TrimSpace(s)+suffix, secrets)
}

// secrets 는 에러 문자열에서 지워야 할 값들이다. 너무 짧은 값은 넣지 않는다 —
// 두세 글자를 전역 치환하면 에러 메시지가 알아볼 수 없게 망가진다.
func (c *Client) secrets(token string) []string {
	var out []string
	if len(c.apiKey) >= 8 {
		out = append(out, c.apiKey)
	}
	if len(token) >= 8 {
		out = append(out, token)
	}
	c.redMu.Lock()
	out = append(out, c.extraRedact...)
	c.redMu.Unlock()
	return out
}

func redact(s string, secrets []string) string {
	for _, sec := range secrets {
		s = strings.ReplaceAll(s, sec, "<redacted>")
	}
	return s
}

// redactErr 는 표준 라이브러리가 만든 에러 문자열에 시크릿이 섞이는 경로를
// 막는다. url.Error 는 URL 을 그대로 싣는데, 쿼리에 토큰이 들어가는 API 로
// 바뀌면 그 즉시 로그로 샌다.
func redactErr(err error, secrets []string) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	if r := redact(s, secrets); r != s {
		return errors.New(r)
	}
	return err
}

// neverLeftTheHost 는 요청이 네트워크에 나가지 않았다고 **단정할 수 있는**
// 경우에만 true 다. 판단이 서지 않으면 false 다 — 모르면 "보냈을 수도 있다"로
// 두는 쪽이 안전하다. 주문 생성에서 이 값이 틀리면 이중 주문이 된다.
//
// DNS 해석 실패와 dial 실패는 연결 자체가 성립하지 않은 것이므로 확정이다.
// 그 밖(쓰기 도중 끊김, 읽기 타임아웃, 서버가 응답 없이 닫음)은 전부 모른다.
func neverLeftTheHost(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return false
}

// throttle 은 직전 요청이 나간 시각으로부터 minInterval 이 지날 때까지 기다린다.
//
// 대기 자체를 락 안에서 하는 것이 핵심이다. 락을 놓고 기다리면 동시 호출자가 전부
// 같은 last 를 읽은 뒤 같은 순간에 깨어나므로 직렬화가 되지 않는다. 락 안에서
// 기다리면 호출 N개가 반드시 (N−1)×minInterval 이상 걸린다.
//
// ctx 취소로 빠져나갈 때는 last 를 갱신하지 않는다 — 요청을 보내지 않았으므로
// 다음 호출자가 그만큼 덜 기다려도 실제 발신 간격은 유지된다.
func (c *Client) throttle(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := minInterval - time.Since(c.last); wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.last = time.Now()
	return nil
}
