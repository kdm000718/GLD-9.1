package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

func TestClientSendsApiKeyAndUserAgent(t *testing.T) {
	var gotKey, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotUA = r.Header.Get("User-Agent")
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer srv.Close()

	c := New("test-key")
	c.BaseURL = srv.URL
	var out struct {
		Success bool `json:"success"`
	}
	if err := c.Get(context.Background(), "/v1/ping", nil, &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotKey != "test-key" {
		t.Errorf("x-api-key = %q, 기대 %q", gotKey, "test-key")
	}
	if gotUA == "" || gotUA == "Go-http-client/1.1" {
		t.Errorf("User-Agent 가 기본값이다: %q — WAF 가 403 으로 막는다", gotUA)
	}
	if !out.Success {
		t.Error("응답 디코딩 실패")
	}
}

func TestClientReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New("bad")
	c.BaseURL = srv.URL
	err := c.Get(context.Background(), "/v1/markets/1", nil, &struct{}{})
	if err == nil {
		t.Fatal("401 인데 에러가 없다")
	}
}

func TestClientNeverLogsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New("super-secret-key")
	c.BaseURL = srv.URL
	err := c.Get(context.Background(), "/v1/x", nil, &struct{}{})
	if err == nil {
		t.Fatal("500 인데 에러가 없다")
	}
	if got := err.Error(); contains(got, "super-secret-key") {
		t.Errorf("에러 메시지에 API 키가 새어나갔다: %s", got)
	}
}

// 레이트리밋은 키당 240 req/min 이고, 넘기면 그 창의 나머지가 전부 429 다.
// 마켓메이킹 루프는 이 클라이언트를 여러 고루틴에서 동시에 부르므로, 동시 호출에서
// 실제로 간격이 벌어지는지를 재는 것이 유일하게 의미 있는 검증이다.
// (뮤텍스가 있는지가 아니라 관측된 간격을 단언한다.)
func TestGetSerializesConcurrentCallers(t *testing.T) {
	const n = 4

	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL

	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.Get(context.Background(), "/v1/ping", nil, &struct{}{})
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("%d번째 Get: %v", i, err)
		}
	}
	if len(at) != n {
		t.Fatalf("서버가 받은 요청 %d건, 기대 %d건", len(at), n)
	}

	// 핵심 단언: 동시 호출 n건은 최소 (n−1)×minInterval 이 걸려야 한다.
	// 직렬화가 없으면 n건이 한 창에 다 나가고 이 값은 거의 0 이 된다.
	if want := time.Duration(n-1) * minInterval; elapsed < want {
		t.Fatalf("동시 호출 %d건이 %v 만에 끝났다 — 최소 %v 이어야 한다 (레이트리밋 무력화)",
			n, elapsed, want)
	}

	// 관측된 발신 간격도 직접 본다. 서버 수신 시각은 스로틀 통과 시각에 HTTP
	// 왕복 오차가 얹히므로 약간의 여유를 둔다.
	sort.Slice(at, func(i, j int) bool { return at[i].Before(at[j]) })
	const slack = 20 * time.Millisecond
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < minInterval-slack {
			t.Errorf("요청 %d→%d 간격 %v — 최소 %v 이어야 한다", i-1, i, gap, minInterval)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
