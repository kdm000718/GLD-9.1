package rest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseSlugStart(t *testing.T) {
	cases := []struct {
		slug string
		want int64
		ok   bool
	}{
		{"btc-updown-5m-1786190100", 1786190100, true},
		{"eth-updown-5m-1786192500", 1786192500, true},
		{"btc-updown-15m-1786190100", 1786190100, true},
		{"btc-updown-5m-notanumber", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseSlugStart(c.slug)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseSlugStart(%q) = (%d, %v), 기대 (%d, %v)", c.slug, got, ok, c.want, c.ok)
		}
	}
}

func TestParseSlugStartRejectsUnaligned(t *testing.T) {
	// 5분 경계가 아닌 값은 회차 슬러그일 수 없다
	if _, ok := ParseSlugStart("btc-updown-5m-1786190123"); ok {
		t.Error("5분 경계가 아닌 슬러그를 받아들였다")
	}
}

func TestFetchResolvedRoundsPaginatesAndFilters(t *testing.T) {
	pages := []string{
		`{"success":true,"cursor":"P2","data":[
		  {"slug":"btc-updown-5m-1786190100","status":"RESOLVED",
		   "variantData":{"startPrice":"100.5","endPrice":"101.0","priceFeedSymbol":"BTCUSDT"}},
		  {"slug":"eth-updown-5m-1786190100","status":"RESOLVED",
		   "variantData":{"startPrice":"1.0","endPrice":"2.0","priceFeedSymbol":"ETHUSDT"}}
		]}`,
		`{"success":true,"cursor":null,"data":[
		  {"slug":"btc-updown-5m-1786189800","status":"RESOLVED",
		   "variantData":{"startPrice":"99.0","endPrice":"98.0","priceFeedSymbol":"BTCUSDT"}}
		]}`,
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n >= len(pages) {
			t.Errorf("페이지를 %d개보다 많이 요청했다", len(pages))
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(pages[n]))
		n++
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	got, err := FetchResolvedRounds(context.Background(), c, "btc", 0)
	if err != nil {
		t.Fatalf("FetchResolvedRounds: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("회차 %d개, 기대 2개 (eth 는 걸러져야 한다)", len(got))
	}
	if got[0].StartPrice != 100.5 || got[0].EndPrice != 101.0 {
		t.Errorf("가격 파싱 실패: %+v", got[0])
	}
	if got[0].StartUnix != 1786190100 {
		t.Errorf("StartUnix = %d, 기대 1786190100", got[0].StartUnix)
	}
}

func TestFetchResolvedRoundsStopsAtSince(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"cursor":"NEXT","data":[
		  {"slug":"btc-updown-5m-1786190100","status":"RESOLVED",
		   "variantData":{"startPrice":"1","endPrice":"2","priceFeedSymbol":"BTCUSDT"}},
		  {"slug":"btc-updown-5m-1000000200","status":"RESOLVED",
		   "variantData":{"startPrice":"1","endPrice":"2","priceFeedSymbol":"BTCUSDT"}}
		]}`))
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	// sinceUnix 보다 오래된 회차를 만나면 멈춰야 한다 — 안 그러면 무한 페이지네이션
	got, err := FetchResolvedRounds(context.Background(), c, "btc", 1786000000)
	if err != nil {
		t.Fatalf("FetchResolvedRounds: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("회차 %d개, 기대 1개 — sinceUnix 에서 멈춰야 한다", len(got))
	}
}
