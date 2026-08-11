package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// resolutionJSON 은 한 회차의 응답 항목을 만든다.
func resolutionJSON(slug string, winners []string, statuses []string, start, end string) map[string]any {
	var markets []any
	for i, name := range winners {
		markets = append(markets, map[string]any{
			"resolution": map[string]any{
				"indexSet": i + 1, "name": name, "status": statuses[i],
			},
		})
	}
	return map[string]any{
		"slug": slug, "status": "RESOLVED",
		"variantData": map[string]any{"startPrice": start, "endPrice": end, "priceFeedSymbol": "BTCUSDT"},
		"markets":     markets,
	}
}

// settleServer 는 페이지들을 순서대로 돌려주고 호출 횟수를 센다.
type settleServer struct {
	mu    sync.Mutex
	pages []map[string]any
	calls int
	srv   *httptest.Server
}

func newSettleServer(t *testing.T, pages []map[string]any) (*settleServer, *rest.Client) {
	t.Helper()
	ss := &settleServer{pages: pages}
	ss.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ss.mu.Lock()
		i := ss.calls
		ss.calls++
		ss.mu.Unlock()
		if i >= len(ss.pages) {
			// 페이지가 동나도 계속 같은 것을 돌려준다 — 상한이 없으면 무한
			// 루프가 된다는 것을 시험하려면 서버가 멈춰 주면 안 된다.
			i = len(ss.pages) - 1
		}
		_ = json.NewEncoder(w).Encode(ss.pages[i])
	}))
	t.Cleanup(ss.srv.Close)
	c := rest.New("test-key")
	c.BaseURL = ss.srv.URL
	return ss, c
}

func page(cursor string, items ...map[string]any) map[string]any {
	m := map[string]any{"success": true, "data": items}
	if cursor != "" {
		m["cursor"] = cursor
	}
	return m
}

// --- settlementFrom ---

// **`status:"WON"` 인 것만 승자다.** 두 outcome 이 모두 resolution 객체를
// 갖고 하나만 WON 인데, 이름이나 indexSet 만 보고 고르면 진 쪽을 승자로 읽는다.
func TestSettlementOnlyWonCounts(t *testing.T) {
	item := resolutionJSON("btc-updown-5m-1786275000",
		[]string{"Up", "Down"}, []string{"LOST", "WON"}, "100", "99")
	ss, c := newSettleServer(t, []map[string]any{page("", item)})
	_ = ss

	got, err := fetchSettlements(context.Background(), c, "btc", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("정산 %d건, want 1", len(got))
	}
	if got[0].WonName != "Down" {
		t.Errorf("승자 = %q, want Down — LOST 를 승자로 읽고 있다", got[0].WonName)
	}
	if got[0].WonIndexSet != 2 {
		t.Errorf("indexSet = %d, want 2", got[0].WonIndexSet)
	}
	if got[0].StartPrice != 100 || got[0].EndPrice != 99 {
		t.Errorf("가격 = %v/%v", got[0].StartPrice, got[0].EndPrice)
	}
}

// 승자가 둘이면 우리 해석이 틀린 것이다. 조용히 하나를 고르면 손익이 절반쯤
// 틀린다 — 그 회차를 통째로 버리는 편이 낫다.
func TestSettlementRejectsTwoWinners(t *testing.T) {
	item := resolutionJSON("btc-updown-5m-1786275000",
		[]string{"Up", "Down"}, []string{"WON", "WON"}, "100", "101")
	_, c := newSettleServer(t, []map[string]any{page("", item)})

	got, err := fetchSettlements(context.Background(), c, "btc", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("승자가 둘인 회차가 통과했다: %+v", got)
	}
}

// 아직 resolution 이 없는 회차(정산 지연)는 건너뛴다.
func TestSettlementSkipsUnresolved(t *testing.T) {
	item := map[string]any{
		"slug": "btc-updown-5m-1786275000", "status": "RESOLVED",
		"variantData": map[string]any{"startPrice": "100", "endPrice": "101"},
		"markets":     []any{map[string]any{}},
	}
	_, c := newSettleServer(t, []map[string]any{page("", item)})
	got, err := fetchSettlements(context.Background(), c, "btc", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("resolution 없는 회차가 통과했다: %+v", got)
	}
}

// 5분 상품만 남는다. 15분 회차가 섞여 들어오면 슬롯이 충돌하고 방향 비교가
// 무의미해진다 — G2 의 앞 두 숫자가 그것 때문에 오염됐다.
func TestSettlementFiltersTimeframeAndSymbol(t *testing.T) {
	items := []map[string]any{
		resolutionJSON("btc-updown-15m-1786275000", []string{"Up"}, []string{"WON"}, "1", "2"),
		resolutionJSON("eth-updown-5m-1786275000", []string{"Up"}, []string{"WON"}, "1", "2"),
		resolutionJSON("btc-updown-5m-1786275000", []string{"Up"}, []string{"WON"}, "1", "2"),
	}
	_, c := newSettleServer(t, []map[string]any{page("", items...)})
	got, err := fetchSettlements(context.Background(), c, "btc", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Slug != "btc-updown-5m-1786275000" {
		t.Errorf("걸러지지 않았다: %+v", got)
	}
}

// **페이지 상한이 없으면 무한 루프다.** 스펙에 기록된 그 사고(창이 열린 뒤
// 14초 만에 240 소진)의 원인이 무제한 페이지네이션이었다. 서버가 커서를
// 계속 새로 주면 상한이 유일한 제동이다.
func TestSettlementStopsAtPageCap(t *testing.T) {
	// 매 페이지가 새 커서를 주고 항목은 늘 새롭다.
	var pages []map[string]any
	for i := 0; i < 50; i++ {
		slug := fmt.Sprintf("btc-updown-5m-%d", 1786275000+i*300)
		pages = append(pages, page(fmt.Sprintf("cur-%d", i),
			resolutionJSON(slug, []string{"Up"}, []string{"WON"}, "1", "2")))
	}
	ss, c := newSettleServer(t, pages)

	if _, err := fetchSettlements(context.Background(), c, "btc", 0); err != nil {
		t.Fatal(err)
	}
	ss.mu.Lock()
	calls := ss.calls
	ss.mu.Unlock()
	if calls > settleMaxPages {
		t.Errorf("요청 %d회 — 상한 %d 를 넘었다. 봇의 예산을 먹는다", calls, settleMaxPages)
	}
}

// 커서가 전진하지 않으면 멈춘다 — 같은 페이지를 3 req/s 로 도는 것보다 낫다.
func TestSettlementStopsOnStuckCursor(t *testing.T) {
	item := resolutionJSON("btc-updown-5m-1786275000", []string{"Up"}, []string{"WON"}, "1", "2")
	ss, c := newSettleServer(t, []map[string]any{page("same", item), page("same", item)})
	if _, err := fetchSettlements(context.Background(), c, "btc", 0); err != nil {
		t.Fatal(err)
	}
	ss.mu.Lock()
	calls := ss.calls
	ss.mu.Unlock()
	if calls > 2 {
		t.Errorf("멈추지 않는 커서에 %d회 요청했다", calls)
	}
}

// sinceUnix 보다 오래된 회차를 만나면 더 거슬러 가지 않는다.
func TestSettlementStopsAtCutoff(t *testing.T) {
	old := resolutionJSON("btc-updown-5m-1786275000", []string{"Up"}, []string{"WON"}, "1", "2")
	ss, c := newSettleServer(t, []map[string]any{page("next", old), page("", old)})
	got, err := fetchSettlements(context.Background(), c, "btc", 1786275300)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("cutoff 이전 회차가 담겼다: %+v", got)
	}
	ss.mu.Lock()
	calls := ss.calls
	ss.mu.Unlock()
	if calls != 1 {
		t.Errorf("cutoff 를 만났는데 %d회 요청했다", calls)
	}
}

// 커서 페이지네이션은 항목을 겹쳐 줄 수 있다. 그것을 독립 표본으로 세면
// 표본 수가 부풀려져 표준오차가 과소평가된다.
func TestSettlementDeduplicates(t *testing.T) {
	item := resolutionJSON("btc-updown-5m-1786275000", []string{"Up"}, []string{"WON"}, "1", "2")
	_, c := newSettleServer(t, []map[string]any{page("a", item), page("", item)})
	got, err := fetchSettlements(context.Background(), c, "btc", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("중복이 %d건으로 담겼다", len(got))
	}
}

// **맞출 대상이 없으면 조회하지 않는다.** ApplySettlements 는 우리가 건
// 회차만 맞추므로, 캐시가 비어 있으면 무엇을 받아 오든 버려진다 — 그 조회는
// 봇의 예산만 먹는다. 기동 직후가 정확히 그 상태다.
func TestOldestUnsettledEmptyMeansNoQuery(t *testing.T) {
	st := newTestState()
	if since, n := st.OldestUnsettled(); n != 0 || since != 0 {
		t.Errorf("빈 캐시에 since=%d n=%d, want 0/0", since, n)
	}
}

// 조회 구간은 미정산 회차 중 가장 오래된 것까지만이다. 고정 24시간으로 잡으면
// 맞출 회차가 둘뿐인데도 하루치를 훑어 페이지가 늘고 예산을 먹는다.
func TestOldestUnsettledBoundsLookback(t *testing.T) {
	st := newTestState()
	seq := uint64(0)
	add := func(slug string) {
		seq++
		s := healthySnapshot()
		s.Seq, s.BootID, s.Round.Slug = seq, "a", slug
		s.Exposure.Filled, s.Exposure.FilledShares = 1, 2
		post(t, st, *s)
	}
	add("btc-updown-5m-1786275000")
	add("btc-updown-5m-1786275300")
	add("btc-updown-5m-1786275600")

	since, n := st.OldestUnsettled()
	if n != 3 {
		t.Errorf("미정산 %d건, want 3", n)
	}
	if since != 1786275000 {
		t.Errorf("since = %d, want 1786275000 (가장 오래된 것)", since)
	}

	// 정산된 것은 빠진다.
	st.ApplySettlements([]settlement{{Slug: "btc-updown-5m-1786275000", WonName: "Up"}})
	since, n = st.OldestUnsettled()
	if n != 2 || since != 1786275300 {
		t.Errorf("정산 뒤 since=%d n=%d, want 1786275300/2", since, n)
	}

	// 전부 정산되면 조회할 이유가 없다.
	st.ApplySettlements([]settlement{
		{Slug: "btc-updown-5m-1786275300", WonName: "Up"},
		{Slug: "btc-updown-5m-1786275600", WonName: "Up"},
	})
	if _, n := st.OldestUnsettled(); n != 0 {
		t.Errorf("전부 정산됐는데 %d건이 남았다", n)
	}
}

// **페이지를 연속으로 치지 않는다.** 상한이 낮아도 등이 붙은 요청은 순간
// 버스트이고, 그 순간 봇의 재호가가 밀린다.
func TestSettlementPagesAreSpaced(t *testing.T) {
	var mu sync.Mutex
	var times []time.Time
	item := resolutionJSON("btc-updown-5m-1786275000", []string{"Up"}, []string{"WON"}, "1", "2")
	item2 := resolutionJSON("btc-updown-5m-1786275300", []string{"Up"}, []string{"WON"}, "1", "2")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		times = append(times, time.Now())
		n := len(times)
		mu.Unlock()
		if n == 1 {
			_ = json.NewEncoder(w).Encode(page("cur", item))
			return
		}
		_ = json.NewEncoder(w).Encode(page("", item2))
	}))
	defer srv.Close()
	c := rest.New("k")
	c.BaseURL = srv.URL

	start := time.Now()
	if _, err := fetchSettlements(context.Background(), c, "btc", 0); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(times) < 2 {
		t.Fatalf("요청이 %d회뿐이다 — 간격을 잴 수 없다", len(times))
	}
	if gap := times[1].Sub(times[0]); gap < settlePageGap {
		t.Errorf("페이지 간격 %v < %v — 연속으로 치고 있다", gap, settlePageGap)
	}
	_ = start
}

// ctx 가 끝나면 페이지 대기 중에도 빠져나온다 — 종료가 3초씩 밀리면 안 된다.
func TestSettlementPageGapRespectsContext(t *testing.T) {
	item := resolutionJSON("btc-updown-5m-1786275000", []string{"Up"}, []string{"WON"}, "1", "2")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(page("cur", item))
	}))
	defer srv.Close()
	c := rest.New("k")
	c.BaseURL = srv.URL

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _ = fetchSettlements(ctx, c, "btc", 0)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("ctx 취소에 %v 걸렸다 — 대기가 ctx 를 안 본다", d)
	}
}
