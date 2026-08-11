package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimitStartsFullBeforeAnyRequest(t *testing.T) {
	// 첫 요청 전에 0 을 돌려주면 감시 규칙(RateLimitRemaining <= 0)이 기동
	// 직후 매번 거짓 Crit 을 낸다.
	c := New("k")
	if got := c.RateLimitRemaining(); got != RateLimitPerMin {
		t.Fatalf("요청 전 남은 예산 = %d, want %d", got, RateLimitPerMin)
	}
	if !c.RateLimitAt().IsZero() {
		t.Fatal("헤더를 본 적 없으면 RateLimitAt 은 제로여야 한다")
	}
}

func TestRateLimitTracksHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ratelimit-remaining", "137")
		w.Write([]byte(`{"success":true,"data":{}}`))
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	var out struct{}
	if err := c.Get(context.Background(), "/v1/x", nil, &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := c.RateLimitRemaining(); got != 137 {
		t.Fatalf("남은 예산 = %d, want 137", got)
	}
	if c.RateLimitAt().IsZero() {
		t.Fatal("헤더를 읽었으면 RateLimitAt 이 채워져야 한다")
	}
}

func TestRateLimitRecordedOnNon2xx(t *testing.T) {
	// 429 는 남은 예산을 가장 알고 싶은 순간이다. 상태 코드 검사가 먼저
	// return 해 버리면 이 값이 갱신되지 않고, 감시 장치는 마지막 성공
	// 응답의 낙관적인 숫자를 계속 본다.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ratelimit-remaining", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"success":false,"error":"rate limited"}`))
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	var out struct{}
	if err := c.Get(context.Background(), "/v1/x", nil, &out); err == nil {
		t.Fatal("429 는 에러여야 한다")
	}
	if got := c.RateLimitRemaining(); got != 0 {
		t.Fatalf("429 뒤 남은 예산 = %d, want 0 — 상태 코드 검사보다 먼저 읽어야 한다", got)
	}
}

func TestRateLimitKeepsLastValueOnMissingOrGarbageHeader(t *testing.T) {
	// 헤더를 주지 않는 프록시 하나가 예산을 0 으로 떨어뜨리면 감시 규칙에
	// 거짓 Crit 이 생긴다. 모르면 직전 값을 유지한다.
	var mode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "good":
			w.Header().Set("ratelimit-remaining", "55")
		case "garbage":
			w.Header().Set("ratelimit-remaining", "n/a")
		case "negative":
			w.Header().Set("ratelimit-remaining", "-3")
		}
		w.Write([]byte(`{"success":true,"data":{}}`))
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	var out struct{}
	for _, m := range []string{"good", "missing", "garbage", "negative"} {
		mode = m
		if err := c.Get(context.Background(), "/v1/x", nil, &out); err != nil {
			t.Fatalf("%s: Get: %v", m, err)
		}
		if got := c.RateLimitRemaining(); got != 55 {
			t.Fatalf("%s 뒤 남은 예산 = %d, want 55 (직전 값 유지)", m, got)
		}
	}
}
