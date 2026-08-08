package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
