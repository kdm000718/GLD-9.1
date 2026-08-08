package rest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

// 커서가 제자리면 종료 조건 네 개 중 어느 것도 잡지 못하고 3 req/s 로 무한히 돈다.
func TestFetchResolvedRoundsFailsOnStationaryCursor(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Write([]byte(`{"success":true,"cursor":"SAME","data":[
		  {"slug":"btc-updown-5m-1786190100","status":"RESOLVED",
		   "variantData":{"startPrice":"1","endPrice":"2","priceFeedSymbol":"BTCUSDT"}}
		]}`))
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	// 첫 페이지는 cursor="" 로 요청하고 "SAME" 을 받는다. 두 번째 페이지에서
	// 같은 커서가 다시 오면 그때 끊겨야 한다.
	_, err := FetchResolvedRounds(context.Background(), c, "btc", 0)
	if err == nil {
		t.Fatal("커서가 제자리인데 에러가 없다 — 무한 루프다")
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("요청 %d건, 기대 2건 — 두 번째 페이지에서 끊어야 한다", got)
	}
}

// 커서가 계속 바뀌지만 순환하는 경우는 전진 검사로 못 잡는다. 페이지 상한이 받는다.
func TestFetchResolvedRoundsStopsAtMaxPages(t *testing.T) {
	orig := maxPages
	maxPages = 4
	t.Cleanup(func() { maxPages = orig })

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		// 커서가 A/B 로 번갈아 바뀌므로 전진 검사는 통과한다
		cur := "A"
		if n%2 == 0 {
			cur = "B"
		}
		fmt.Fprintf(w, `{"success":true,"cursor":%q,"data":[
		  {"slug":"btc-updown-5m-%d","status":"RESOLVED",
		   "variantData":{"startPrice":"1","endPrice":"2","priceFeedSymbol":"BTCUSDT"}}
		]}`, cur, 1786190100+int64(n)*300)
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	_, err := FetchResolvedRounds(context.Background(), c, "btc", 0)
	if err == nil {
		t.Fatal("커서가 순환하는데 에러가 없다 — 무한 루프다")
	}
	if got := atomic.LoadInt32(&hits); got != 4 {
		t.Errorf("요청 %d건, 기대 %d건 — 상한에서 멈춰야 한다", got, 4)
	}
}

// 페이지가 겹쳐서 같은 회차를 돌려주면 표본 수가 부풀려진다 (G2 의 n=2,669 가 그랬다).
func TestFetchResolvedRoundsDeduplicatesSlugs(t *testing.T) {
	pages := []string{
		`{"success":true,"cursor":"P2","data":[
		  {"slug":"btc-updown-5m-1786190100","status":"RESOLVED",
		   "variantData":{"startPrice":"100","endPrice":"101","priceFeedSymbol":"BTCUSDT"}},
		  {"slug":"btc-updown-5m-1786189800","status":"RESOLVED",
		   "variantData":{"startPrice":"100","endPrice":"99","priceFeedSymbol":"BTCUSDT"}}
		]}`,
		// 두 번째 페이지가 첫 페이지 두 건을 그대로 다시 준다 + 새 회차 하나
		`{"success":true,"cursor":null,"data":[
		  {"slug":"btc-updown-5m-1786190100","status":"RESOLVED",
		   "variantData":{"startPrice":"100","endPrice":"101","priceFeedSymbol":"BTCUSDT"}},
		  {"slug":"btc-updown-5m-1786189800","status":"RESOLVED",
		   "variantData":{"startPrice":"100","endPrice":"99","priceFeedSymbol":"BTCUSDT"}},
		  {"slug":"btc-updown-5m-1786189500","status":"RESOLVED",
		   "variantData":{"startPrice":"100","endPrice":"102","priceFeedSymbol":"BTCUSDT"}}
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
	if len(got) != 3 {
		t.Fatalf("회차 %d개, 기대 3개 — 겹친 2건이 중복 제거되어야 한다", len(got))
	}
	seen := map[string]bool{}
	for _, r := range got {
		if seen[r.Slug] {
			t.Errorf("슬러그 %s 가 두 번 들어 있다", r.Slug)
		}
		seen[r.Slug] = true
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
