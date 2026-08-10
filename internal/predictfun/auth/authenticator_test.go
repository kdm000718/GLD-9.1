package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

func TestAuthenticatorTokenRoundTrip(t *testing.T) {
	var gotAddrOnGet, gotAddrOnPost, gotSig, gotMsg, gotAPIKeyGet, gotAPIKeyPost string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/auth/message":
			gotAddrOnGet = r.URL.Query().Get("address")
			gotAPIKeyGet = r.Header.Get("x-api-key")
			json.NewEncoder(w).Encode(map[string]any{"message": "sign-this-nonce"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth":
			gotAPIKeyPost = r.Header.Get("x-api-key")
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			gotAddrOnPost, _ = body["signer"].(string) // 실제 필드명은 "signer"다(Task 10 실측) — "address"가 아니다.
			gotMsg, _ = body["message"].(string)
			gotSig, _ = body["signature"].(string)
			json.NewEncoder(w).Encode(map[string]any{"token": "eyJ.abc.123", "expiresIn": float64(3600)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := rest.New("test-api-key")
	c.BaseURL = srv.URL
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	a := &Authenticator{Rest: c, Signer: s}

	tok, err := a.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "eyJ.abc.123" {
		t.Errorf("token = %q, 기대 %q", tok, "eyJ.abc.123")
	}
	if gotAddrOnGet != s.Address().Hex() {
		t.Errorf("GET address = %q, 기대 %q", gotAddrOnGet, s.Address().Hex())
	}
	if gotAddrOnPost != s.Address().Hex() {
		t.Errorf("POST signer = %q, 기대 %q", gotAddrOnPost, s.Address().Hex())
	}
	if gotMsg != "sign-this-nonce" {
		t.Errorf("POST message = %q, 기대 %q", gotMsg, "sign-this-nonce")
	}
	if !strings.HasPrefix(gotSig, "0x") || len(gotSig) != 2+65*2 {
		t.Errorf("서명 형식이 이상하다: %q", gotSig)
	}
	if gotAPIKeyGet != "test-api-key" || gotAPIKeyPost != "test-api-key" {
		t.Errorf("x-api-key 가 두 요청 모두에 안 붙었다: GET=%q POST=%q", gotAPIKeyGet, gotAPIKeyPost)
	}
}

func TestAuthenticatorTokenCachesUntilNearExpiry(t *testing.T) {
	var getCalls, postCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/auth/message":
			atomic.AddInt32(&getCalls, 1)
			json.NewEncoder(w).Encode(map[string]any{"message": "nonce"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth":
			atomic.AddInt32(&postCalls, 1)
			json.NewEncoder(w).Encode(map[string]any{"token": "cached-token", "expiresIn": float64(3600)})
		}
	}))
	defer srv.Close()

	c := rest.New("k")
	c.BaseURL = srv.URL
	s, _ := NewSigner(testKey)
	a := &Authenticator{Rest: c, Signer: s}

	tok1, err := a.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tok2, err := a.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok1 != tok2 {
		t.Errorf("캐시된 토큰이 달라졌다: %q vs %q", tok1, tok2)
	}
	if atomic.LoadInt32(&getCalls) != 1 || atomic.LoadInt32(&postCalls) != 1 {
		t.Errorf("두 번째 Token 호출이 네트워크를 다시 탔다: GET=%d POST=%d", getCalls, postCalls)
	}
}

func TestAuthenticatorTokenRefreshesNearExpiry(t *testing.T) {
	var postCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/auth/message":
			json.NewEncoder(w).Encode(map[string]any{"message": "nonce"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth":
			n := atomic.AddInt32(&postCalls, 1)
			json.NewEncoder(w).Encode(map[string]any{"token": "token-" + strconv.Itoa(int(n)), "expiresIn": float64(3600)})
		}
	}))
	defer srv.Close()

	c := rest.New("k")
	c.BaseURL = srv.URL
	s, _ := NewSigner(testKey)
	a := &Authenticator{Rest: c, Signer: s}

	if _, err := a.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 만료 60초 이내로 강제로 당겨서 갱신 경로를 탄다.
	a.mu.Lock()
	a.expires = time.Now().Add(30 * time.Second)
	a.mu.Unlock()

	tok2, err := a.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok2 != "token-2" {
		t.Errorf("만료 임박인데 갱신하지 않았다: token = %q", tok2)
	}
	if atomic.LoadInt32(&postCalls) != 2 {
		t.Errorf("POST 호출 %d회, 기대 2회", postCalls)
	}
}

func TestAuthenticatorTokenErrorsOnMissingMessageField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"unexpected": "field"})
	}))
	defer srv.Close()

	c := rest.New("k")
	c.BaseURL = srv.URL
	s, _ := NewSigner(testKey)
	a := &Authenticator{Rest: c, Signer: s}

	if _, err := a.Token(context.Background()); err == nil {
		t.Error("서명할 메시지가 없는 응답을 받아들였다")
	}
}

func TestAuthenticatorTokenErrorsOnMissingTokenField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"message": "nonce"})
		case r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{"unexpected": "field"})
		}
	}))
	defer srv.Close()

	c := rest.New("k")
	c.BaseURL = srv.URL
	s, _ := NewSigner(testKey)
	a := &Authenticator{Rest: c, Signer: s}

	_, err := a.Token(context.Background())
	if err == nil {
		t.Fatal("토큰 필드가 없는 응답을 받아들였다")
	}
	assertNoKeyLeak(t, "Token 에러", err.Error())
}

// 실측 API 봉투({"data": …, "success": true})를 그대로 재현한다. 이게 실제
// 결함이 있던 자리다 — pickString 이 "data" 를 후보 키에 우연히 낀 메시지
// 쪽에서만 동작하고 JWT 쪽에서는 조용히 실패했었다.
func TestAuthenticatorTokenRoundTripWithRealEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/auth/message":
			json.NewEncoder(w).Encode(map[string]any{
				"data":    map[string]any{"message": "Please sign this message to log in. Timestamp: 1786260363421"},
				"success": true,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/auth":
			json.NewEncoder(w).Encode(map[string]any{
				"data":    map[string]any{"token": "eyJhbG.envelope.tok"},
				"success": true,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := rest.New("k")
	c.BaseURL = srv.URL
	s, _ := NewSigner(testKey)
	a := &Authenticator{Rest: c, Signer: s}

	tok, err := a.Token(context.Background())
	if err != nil {
		t.Fatalf("실측 봉투 형태로 Token 실패: %v", err)
	}
	if tok != "eyJhbG.envelope.tok" {
		t.Errorf("token = %q, 기대 %q", tok, "eyJhbG.envelope.tok")
	}
}

// 핵심 회귀 테스트다. pickString 이 봉투를 벗기게 고쳤어도(라운드 2), 바로
// 옆의 expiresIn 읽기는 여전히 원본(봉투 안) 맵을 보고 있어서 서버가 준
// 만료를 무시하고 하드코딩 10분으로 떨어졌었다 — 실제 만료가 10분보다 짧으면
// 만료된 토큰을 계속 유효하다고 캐시해서 돌려주는 결함이었다.
func TestAuthenticatorTokenReadsExpiresInFromEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"message": "nonce"}, "success": true})
		case r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{
				"data":    map[string]any{"token": "tok", "expiresIn": float64(300)},
				"success": true,
			})
		}
	}))
	defer srv.Close()

	c := rest.New("k")
	c.BaseURL = srv.URL
	s, _ := NewSigner(testKey)
	a := &Authenticator{Rest: c, Signer: s}

	before := time.Now()
	if _, err := a.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	a.mu.Lock()
	expires := a.expires
	a.mu.Unlock()

	wantMin := before.Add(299 * time.Second)
	wantMax := before.Add(301 * time.Second)
	if expires.Before(wantMin) || expires.After(wantMax) {
		t.Errorf("a.expires = %v (지금부터 %v 뒤) — 봉투 안 expiresIn=300 을 무시하고 다른 값으로 떨어졌다",
			expires, time.Until(expires))
	}
}

func TestAuthenticatorTokenDefaultsExpiryWhenExpiresInMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"message": "nonce"})
		case r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{"token": "tok"})
		}
	}))
	defer srv.Close()

	c := rest.New("k")
	c.BaseURL = srv.URL
	s, _ := NewSigner(testKey)
	a := &Authenticator{Rest: c, Signer: s}

	before := time.Now()
	if _, err := a.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	a.mu.Lock()
	expires := a.expires
	a.mu.Unlock()

	wantMin := before.Add(9*time.Minute + 59*time.Second)
	wantMax := before.Add(10*time.Minute + 1*time.Second)
	if expires.Before(wantMin) || expires.After(wantMax) {
		t.Errorf("expiresIn 이 없는데 기본 10분이 아니다: a.expires = %v", expires)
	}
}

func TestAuthenticatorTokenDefaultsExpiryWhenExpiresInNonPositive(t *testing.T) {
	for _, bad := range []float64{0, -300} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet:
				json.NewEncoder(w).Encode(map[string]any{"message": "nonce"})
			case r.Method == http.MethodPost:
				json.NewEncoder(w).Encode(map[string]any{"token": "tok", "expiresIn": bad})
			}
		}))

		c := rest.New("k")
		c.BaseURL = srv.URL
		s, _ := NewSigner(testKey)
		a := &Authenticator{Rest: c, Signer: s}

		before := time.Now()
		if _, err := a.Token(context.Background()); err != nil {
			srv.Close()
			t.Fatalf("expiresIn=%v: Token: %v", bad, err)
		}
		a.mu.Lock()
		expires := a.expires
		a.mu.Unlock()
		srv.Close()

		wantMin := before.Add(9*time.Minute + 59*time.Second)
		wantMax := before.Add(10*time.Minute + 1*time.Second)
		if expires.Before(wantMin) || expires.After(wantMax) {
			t.Errorf("expiresIn=%v 인데 기본 10분이 아니다: a.expires = %v", bad, expires)
		}
	}
}

// data 가 배열이면(예상 밖 응답 형태) 패닉하지 않고 정상적으로 "못 찾았다"
// 에러를 내야 한다.
func TestAuthenticatorTokenHandlesArrayDataWithoutPanicking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"message": "nonce"})
		case r.Method == http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{"data": []any{1, 2, 3}})
		}
	}))
	defer srv.Close()

	c := rest.New("k")
	c.BaseURL = srv.URL
	s, _ := NewSigner(testKey)
	a := &Authenticator{Rest: c, Signer: s}

	if _, err := a.Token(context.Background()); err == nil {
		t.Error("data 가 배열인데 에러 없이 성공했다")
	}
}

// 실측(2026-08-09, Task 10): 토큰 자체가 JWT이고 exp 클레임(Unix초)을 담고
// 있다 — expiresIn 필드가 아니라 이 값이 실제 만료 소스다. 이 테스트는 그
// 경로가 Token() 전체 왕복에 반영되는지 본다.
func TestAuthenticatorTokenReadsExpiryFromJWTClaim(t *testing.T) {
	wantExp := time.Now().Add(24 * time.Hour)
	tok := fakeJWT(t, map[string]any{"exp": wantExp.Unix(), "iat": wantExp.Add(-24 * time.Hour).Unix()})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"message": "nonce"}, "success": true})
		case r.Method == http.MethodPost:
			// expiresIn을 일부러 같이 보낸다 — exp 클레임이 그것보다 우선해야 한다
			// (아래 TestAuthenticatorTokenJWTExpTakesPriorityOverExpiresIn에서
			// 그 우선순위 자체를 별도로 검증한다. 여기서는 exp 클레임 하나만으로도
			// 정확한 만료가 나오는지를 본다).
			json.NewEncoder(w).Encode(map[string]any{
				"data":    map[string]any{"token": tok},
				"success": true,
			})
		}
	}))
	defer srv.Close()

	c := rest.New("k")
	c.BaseURL = srv.URL
	s, _ := NewSigner(testKey)
	a := &Authenticator{Rest: c, Signer: s}

	if _, err := a.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	a.mu.Lock()
	expires := a.expires
	a.mu.Unlock()

	if expires.Unix() != wantExp.Unix() {
		t.Errorf("a.expires = %v, 기대 %v (JWT exp 클레임)", expires, wantExp)
	}
}

// exp 클레임과 expiresIn 필드가 둘 다 있으면 exp가 이겨야 한다 — 실제
// 서버에는 expiresIn이 없다는 것이 확정됐지만(위 Token() 주석), 있다고
// 가정해도 우선순위가 뒤집히지 않는지 확인한다.
func TestAuthenticatorTokenJWTExpTakesPriorityOverExpiresIn(t *testing.T) {
	jwtExp := time.Now().Add(2 * time.Hour)
	tok := fakeJWT(t, map[string]any{"exp": jwtExp.Unix()})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			json.NewEncoder(w).Encode(map[string]any{"message": "nonce"})
		case r.Method == http.MethodPost:
			// expiresIn=300(5분)은 jwtExp(2시간)와 확연히 다르다 — 어느 쪽이
			// 반영됐는지 명확히 구별된다.
			json.NewEncoder(w).Encode(map[string]any{"token": tok, "expiresIn": float64(300)})
		}
	}))
	defer srv.Close()

	c := rest.New("k")
	c.BaseURL = srv.URL
	s, _ := NewSigner(testKey)
	a := &Authenticator{Rest: c, Signer: s}

	if _, err := a.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	a.mu.Lock()
	expires := a.expires
	a.mu.Unlock()

	if expires.Unix() != jwtExp.Unix() {
		t.Errorf("a.expires = %v (지금부터 %v) — expiresIn=300에 밀렸다, JWT exp(%v)를 썼어야 한다",
			expires, time.Until(expires), jwtExp)
	}
}

func TestAuthenticatorTokenPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := rest.New("k")
	c.BaseURL = srv.URL
	s, _ := NewSigner(testKey)
	a := &Authenticator{Rest: c, Signer: s}

	if _, err := a.Token(context.Background()); err == nil {
		t.Error("401 인데 에러가 없다")
	}
}
