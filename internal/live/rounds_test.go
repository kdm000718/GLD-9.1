package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// --- ParseRoundStart — G2 측정 두 번을 오염시킨 함수 ---

func TestParseRoundStartRejectsOtherProducts(t *testing.T) {
	// 15분 상품이 섞여 들어와 측정 두 번을 오염시킨 전례가 있다.
	for _, bad := range []string{
		"btc-updown-15m-1786275000",
		"btc-updown-1h-1786275000",
		"eth-updown-5m-1786275000",
		"btc-updown-5m-",
		"btc-updown-5m-abc",
		"", "btc",
		// 접두사가 슬러그 **앞**에 있어야 한다. 어딘가에 포함되기만 하면
		// 되게 만들면(strings.Contains) 아래가 통과한다.
		"x-btc-updown-5m-1786275000",
		// 대소문자 변형. EqualFold 로 느슨하게 보면 통과한다.
		"BTC-updown-5m-1786275000",
		"btc-UpDown-5m-1786275000",
		// 부호. strconv.ParseInt 는 이 둘을 그냥 받아들인다.
		"btc-updown-5m-+1786275000",
		"btc-updown-5m--1786275000",
		// 공백. TrimSpace 를 넣으면 통과한다.
		"btc-updown-5m- 1786275000",
		"btc-updown-5m-1786275000 ",
		// 뒤에 뭐가 더 붙은 것.
		"btc-updown-5m-1786275000-up",
		// int64 를 넘는 자릿수.
		"btc-updown-5m-99999999999999999999999",
	} {
		if got, ok := ParseRoundStart(bad); ok {
			t.Errorf("%q 를 받아들였다 (= %d)", bad, got)
		}
	}
	got, ok := ParseRoundStart("btc-updown-5m-1786275000")
	if !ok || got != 1786275000 {
		t.Errorf("= %d,%v", got, ok)
	}
}

// 회차 시작은 5분 경계여야 한다. 아니면 모델의 봉 시작과 어긋난다.
func TestParseRoundStartRequiresFiveMinuteBoundary(t *testing.T) {
	for _, bad := range []string{
		"btc-updown-5m-1786275001",
		"btc-updown-5m-1786275060", // 1분 경계지만 5분 경계는 아니다
		"btc-updown-5m-1786275299",
	} {
		if _, ok := ParseRoundStart(bad); ok {
			t.Errorf("%q — 5분 경계가 아닌 시각을 받아들였다", bad)
		}
	}
}

// **단위 혼동.** 밀리초 값은 300 의 배수라 5분 경계 검사를 통과한다 —
// 타당 범위 검사만이 잡는다. 통과하면 Freeze 가 서기 58589년의 봉을 찾는다.
func TestParseRoundStartRejectsImplausibleTimes(t *testing.T) {
	for _, bad := range []string{
		"btc-updown-5m-1786275000000", // 밀리초
		"btc-updown-5m-0",
		"btc-updown-5m-300", // 1970년
		"btc-updown-5m-9223372036854775800",
	} {
		if got, ok := ParseRoundStart(bad); ok {
			t.Errorf("%q 를 받아들였다 (= %d)", bad, got)
		}
	}
}

// --- IsLive — cmd/probe 에서 옮겨 온 경계 테스트 ---

func cat(slug, startsAt, endsAt string) Category {
	return Category{Slug: slug, StartsAt: startsAt, EndsAt: endsAt}
}

func TestIsLive(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 2, 30, 0, time.UTC)
	rfc := func(t time.Time) string { return t.Format(time.RFC3339) }

	cases := []struct {
		name string
		c    Category
		want bool
	}{
		{
			"진행 중(12:00~12:05)",
			cat("btc-updown-5m-1", rfc(now.Add(-150*time.Second)), rfc(now.Add(150*time.Second))),
			true,
		},
		{
			"막 시작(startsAt == now)",
			cat("btc-updown-5m-2", rfc(now), rfc(now.Add(5*time.Minute))),
			true,
		},
		{
			"lookahead 안에 시작(+59초)",
			cat("btc-updown-5m-3", rfc(now.Add(59*time.Second)), rfc(now.Add(59*time.Second+5*time.Minute))),
			true,
		},
		{
			"lookahead 경계에 시작(+60초)",
			cat("btc-updown-5m-4", rfc(now.Add(DefaultLookahead)), rfc(now.Add(DefaultLookahead+5*time.Minute))),
			true,
		},
		{
			"lookahead 밖에 시작(+61초)",
			cat("btc-updown-5m-5", rfc(now.Add(DefaultLookahead+time.Second)), rfc(now.Add(DefaultLookahead+time.Second+5*time.Minute))),
			false,
		},
		{
			"23시간 뒤 사전 등록 — 이것이 앞선 소크를 망친 표본",
			cat("btc-updown-5m-6", rfc(now.Add(23*time.Hour)), rfc(now.Add(23*time.Hour+5*time.Minute))),
			false,
		},
		{
			"이미 끝남(endsAt == now)",
			cat("btc-updown-5m-7", rfc(now.Add(-5*time.Minute)), rfc(now)),
			false,
		},
		{
			"이미 끝남(endsAt < now, API 는 아직 OPEN)",
			cat("btc-updown-5m-8", rfc(now.Add(-10*time.Minute)), rfc(now.Add(-5*time.Minute))),
			false,
		},
		{
			"1초 남음",
			cat("btc-updown-5m-9", rfc(now.Add(-5*time.Minute)), rfc(now.Add(time.Second))),
			true,
		},
		{"startsAt 파싱 실패는 버린다", cat("btc-updown-5m-a", "", rfc(now.Add(time.Minute))), false},
		{"endsAt 파싱 실패는 버린다", cat("btc-updown-5m-b", rfc(now.Add(-time.Minute)), "nonsense"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsLive(tc.c, now, DefaultLookahead); got != tc.want {
				t.Errorf("IsLive(%+v) = %v, 기대 %v", tc.c, got, tc.want)
			}
		})
	}
}

// --- FetchCategories / FetchLive ---

// nowFor 는 slug 시각(초)의 회차 한가운데다.
func nowFor(startUnix int64) time.Time { return time.Unix(startUnix+150, 0).UTC() }

// catJSON 은 응답 한 건을 만든다. precision·outcome·tradingStatus 를 바꿔
// 가며 실패 경로를 만들 수 있어야 한다.
type catOpt struct {
	slug          string
	startsAt      string
	endsAt        string
	precision     int
	feeRateBps    int
	tradingStatus string
	upID          string
	downID        string
	extraMarket   bool
	noOutcomes    bool
	// negRisk·yieldBearing 은 어느 Exchange 계약에 서명할지를 정한다.
	// nil 은 "응답에 없다" 이고 toRound 가 거부해야 하는 상태다.
	negRisk      *bool
	yieldBearing *bool
}

func boolPtr(v bool) *bool { return &v }

func defaultCat(startUnix int64) catOpt {
	return catOpt{
		slug:          fmt.Sprintf("btc-updown-5m-%d", startUnix),
		startsAt:      time.Unix(startUnix, 0).UTC().Format(time.RFC3339),
		endsAt:        time.Unix(startUnix+300, 0).UTC().Format(time.RFC3339),
		precision:     2,
		feeRateBps:    200,
		tradingStatus: "OPEN",
		upID:          "1111111111111111111",
		downID:        "2222222222222222222",
		// 2026-08-10 메인넷 실측: btc-updown-5m 은 둘 다 false 다.
		negRisk:      boolPtr(false),
		yieldBearing: boolPtr(false),
	}
}

func (o catOpt) build(id int64) Category {
	m := Market{
		ID:               id * 10,
		Status:           "OPEN",
		TradingStatus:    o.tradingStatus,
		DecimalPrecision: o.precision,
		FeeRateBps:       o.feeRateBps,
		ShareThreshold:   100,
		SpreadThreshold:  0.06,
		IsNegRisk:        o.negRisk,
		IsYieldBearing:   o.yieldBearing,
	}
	if !o.noOutcomes {
		m.Outcomes = []Outcome{
			{Name: "Up", IndexSet: 1, OnChainID: o.upID},
			{Name: "Down", IndexSet: 2, OnChainID: o.downID},
		}
	}
	c := Category{ID: id, Slug: o.slug, Status: "OPEN", StartsAt: o.startsAt, EndsAt: o.endsAt, Markets: []Market{m}}
	if o.extraMarket {
		m2 := m
		m2.ID = id*10 + 1
		c.Markets = append(c.Markets, m2)
	}
	return c
}

// serve 는 /v1/categories 한 페이지를 주는 서버와 그것을 보는 클라이언트를
// 만든다. **실제 predict.fun 을 부르지 않는다.**
func serve(t *testing.T, cats []Category) (*rest.Client, *[]string) {
	t.Helper()
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(CategoryPage{Success: true, Data: cats})
	}))
	t.Cleanup(srv.Close)
	c := rest.New("test-api-key-placeholder")
	c.BaseURL = srv.URL
	return c, &queries
}

// **정렬이 핵심이다.** PUBLISHED_AT_DESC 로는 진행 중 회차가 24페이지 깊이에
// 있어서 소크 두 번이 통째로 헛돌았다. 쿼리 문자열을 직접 고정한다 —
// 이 값이 조용히 바뀌면 증상이 "회차 0개"로만 나타나고 원인은 안 보인다.
func TestFetchCategoriesUsesAscendingPublishedSort(t *testing.T) {
	start := int64(1786275000)
	c, queries := serve(t, []Category{defaultCat(start).build(1)})
	if _, err := FetchCategories(context.Background(), c, "btc", nowFor(start), DefaultLookahead); err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if len(*queries) != 1 {
		t.Fatalf("요청 %d회, 기대 1회", len(*queries))
	}
	q := (*queries)[0]
	for _, want := range []string{"sort=PUBLISHED_AT_ASC", "status=OPEN", "marketVariant=CRYPTO_UP_DOWN", "first=50"} {
		if !strings.Contains(q, want) {
			t.Errorf("쿼리에 %q 가 없다: %s", want, q)
		}
	}
	if strings.Contains(q, "DESC") {
		t.Errorf("내림차순 정렬이 들어갔다 — 진행 중 회차가 24페이지 깊이로 밀린다: %s", q)
	}
}

// 시각 창 밖(사전 등록)과 다른 심볼은 걸러진다.
func TestFetchCategoriesFiltersByWindowAndPrefix(t *testing.T) {
	start := int64(1786275000)
	now := nowFor(start)

	future := defaultCat(start + 23*3600)
	future.slug = fmt.Sprintf("btc-updown-5m-%d", start+23*3600)
	eth := defaultCat(start)
	eth.slug = fmt.Sprintf("eth-updown-5m-%d", start)

	c, _ := serve(t, []Category{
		future.build(1),
		defaultCat(start).build(2),
		eth.build(3),
	})
	got, err := FetchCategories(context.Background(), c, "btc", now, DefaultLookahead)
	if err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("회차 %d개, 기대 1개: %+v", len(got), got)
	}
	if got[0].ID != 2 {
		t.Errorf("잡은 회차 ID %d, 기대 2", got[0].ID)
	}
}

// 빈 심볼 접두사는 조용한 0개가 아니라 에러다 — 배선 실수가 "지금 열린 회차
// 없음" 으로 보이면 안 된다.
func TestFetchCategoriesRejectsEmptySymbol(t *testing.T) {
	c, _ := serve(t, nil)
	if _, err := FetchCategories(context.Background(), c, "", time.Now(), DefaultLookahead); err == nil {
		t.Error("빈 심볼 접두사가 통과했다")
	}
}

func TestFetchLiveBuildsRound(t *testing.T) {
	start := int64(1786275000)
	c, _ := serve(t, []Category{defaultCat(start).build(7)})
	rs, err := FetchLive(context.Background(), c, "btc", nowFor(start), DefaultLookahead)
	if err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("회차 %d개, 기대 1개", len(rs))
	}
	r := rs[0]
	if r.CategoryID != 7 || r.MarketID != 70 {
		t.Errorf("식별자 category=%d market=%d, 기대 7/70", r.CategoryID, r.MarketID)
	}
	if r.Precision != 2 {
		t.Errorf("precision %d, 기대 2", r.Precision)
	}
	if r.UpTokenID != "1111111111111111111" || r.DownTokenID != "2222222222222222222" {
		t.Errorf("토큰 ID up=%q down=%q", r.UpTokenID, r.DownTokenID)
	}
	if r.StartsAt.Unix() != start {
		t.Errorf("StartsAt %d, 기대 %d", r.StartsAt.Unix(), start)
	}
	if r.StartMS() != start*1000 {
		t.Errorf("StartMS %d, 기대 %d", r.StartMS(), start*1000)
	}
	// feeRateBps 는 EIP-712 Order 에 들어가는 값이다. 배선이 여기서 못 읽어
	// 0 을 채우면 우리가 서명한 주문과 거래소가 기대하는 주문이 달라진다.
	if r.FeeRateBps != 200 {
		t.Errorf("FeeRateBps %d, 기대 200", r.FeeRateBps)
	}
}

// 마켓의 변종이 회차에 그대로 실려야 한다. **여기서 끊기면 서명 도메인이
// 마켓과 무관해진다** — 상수를 박은 것과 같아지고, 거래소가 상품을 negRisk 로
// 옮기는 날 조용히 틀린 계약에 서명한다.
func TestRoundCarriesTheMarketVariant(t *testing.T) {
	start := int64(1786275000)
	for _, tc := range []struct{ neg, yb bool }{
		{false, false}, {true, false}, {false, true}, {true, true},
	} {
		o := defaultCat(start)
		o.negRisk = boolPtr(tc.neg)
		o.yieldBearing = boolPtr(tc.yb)
		c, _ := serve(t, []Category{o.build(1)})
		rs, err := FetchLive(context.Background(), c, SlugSymbol, nowFor(start), DefaultLookahead)
		if err != nil {
			t.Fatalf("negRisk=%v yieldBearing=%v: %v", tc.neg, tc.yb, err)
		}
		if len(rs) != 1 {
			t.Fatalf("회차 %d개", len(rs))
		}
		if rs[0].IsNegRisk != tc.neg || rs[0].IsYieldBearing != tc.yb {
			t.Errorf("회차의 변종이 마켓과 다르다: negRisk %v→%v, yieldBearing %v→%v",
				tc.neg, rs[0].IsNegRisk, tc.yb, rs[0].IsYieldBearing)
		}
	}
}

// 거래에 필요한 것이 미심쩍으면 **조용히 건너뛰지 않고 에러다.**
func TestFetchLiveRejectsUnusableRounds(t *testing.T) {
	start := int64(1786275000)
	now := nowFor(start)

	cases := []struct {
		name string
		mut  func(*catOpt)
		want string
		// now 를 따로 주는 경우가 있다. FetchCategories 의 시각 창이 먼저
		// 걸러 버리면 toRound 의 가드까지 도달하지 못한다.
		now time.Time
	}{
		{"precision 0 (필드 이름 변경의 모습)", func(o *catOpt) { o.precision = 0 }, "decimalPrecision", time.Time{}},
		{"precision 범위 초과", func(o *catOpt) { o.precision = 19 }, "decimalPrecision", time.Time{}},
		{"tradingStatus 가 OPEN 이 아니다", func(o *catOpt) { o.tradingStatus = "PAUSED" }, "OPEN 인 마켓이 없다", time.Time{}},
		{"OPEN 마켓이 둘", func(o *catOpt) { o.extraMarket = true }, "OPEN 마켓이 2개", time.Time{}},
		{"outcomes 가 없다", func(o *catOpt) { o.noOutcomes = true }, "onChainId 가 없다", time.Time{}},
		{"onChainId 가 비었다", func(o *catOpt) { o.upID = "" }, "onChainId 가 없다", time.Time{}},
		{"onChainId 가 0 이다", func(o *catOpt) { o.downID = "0" }, "onChainId 가 0", time.Time{}},
		{"onChainId 가 숫자가 아니다", func(o *catOpt) { o.upID = "0xdeadbeef" }, "10진 숫자가 아니다", time.Time{}},
		{"Up 과 Down 이 같은 토큰", func(o *catOpt) { o.downID = o.upID }, "같다", time.Time{}},
		// 음수 수수료율은 주문에 리베이트로 서명된다 — 우리가 의도한 주문이
		// 아니다. 응답이 그런 값을 줄 이유가 없으므로 파싱이 어긋난 것이다.
		{"feeRateBps 가 음수", func(o *catOpt) { o.feeRateBps = -1 }, "feeRateBps", time.Time{}},
		// **이 둘이 어느 Exchange 계약에 서명할지를 정한다.** 없는 것을
		// false 로 읽으면 오늘은 맞는 답(CTF_EXCHANGE)이 나오지만, 그건
		// 파싱이 깨졌다는 사실을 덮을 뿐이다 — 거래소가 이 상품을 negRisk 로
		// 옮기는 날 조용히 틀린 계약에 서명한다.
		{"isNegRisk 가 응답에 없다", func(o *catOpt) { o.negRisk = nil }, "isNegRisk", time.Time{}},
		{"isYieldBearing 이 응답에 없다", func(o *catOpt) { o.yieldBearing = nil }, "isYieldBearing", time.Time{}},
		// 접두사는 통과하지만 5분 경계가 아닌 슬러그. 접두사 필터가 못 잡는
		// 유일한 오염 경로이고, ParseRoundStart 가 그것을 잡는다.
		{"슬러그 시각이 5분 경계가 아니다", func(o *catOpt) {
			o.slug = fmt.Sprintf("btc-updown-5m-%d", start+1)
		}, "시작 시각을 읽지 못했다", time.Time{}},
		{"슬러그 시각과 startsAt 이 다르다", func(o *catOpt) {
			o.slug = fmt.Sprintf("btc-updown-5m-%d", start+300)
		}, "다르다", time.Time{}},
		// endsAt <= startsAt. 아직 시작 전(lookahead 안)인 회차라야 시각 창을
		// 통과해 toRound 까지 온다.
		{"endsAt 이 startsAt 보다 앞", func(o *catOpt) {
			o.endsAt = time.Unix(start-30, 0).UTC().Format(time.RFC3339)
		}, "endsAt", time.Unix(start-40, 0).UTC()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := defaultCat(start)
			tc.mut(&o)
			c, _ := serve(t, []Category{o.build(1)})
			at := now
			if !tc.now.IsZero() {
				at = tc.now
			}
			rs, err := FetchLive(context.Background(), c, "btc", at, DefaultLookahead)
			if err == nil {
				t.Fatalf("에러가 없다 — 거래 불가능한 회차가 %d개 통과했다: %+v", len(rs), rs)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("에러가 %q 를 담지 않았다: %v", tc.want, err)
			}
			if rs != nil {
				t.Errorf("에러인데 회차를 %d개 돌려줬다", len(rs))
			}
		})
	}
}

// 15분 상품이 시각 창 안에 있어도 슬러그 접두사에서 이미 걸러진다 —
// FetchCategories 단계의 방어. 여기가 뚫리면 toRound 가 에러를 내고,
// 그 에러가 진행 중 5분 회차까지 함께 막는다.
func TestFetchLiveIgnoresOtherExpiriesByPrefix(t *testing.T) {
	start := int64(1786275000)
	fifteen := defaultCat(start)
	fifteen.slug = fmt.Sprintf("btc-updown-15m-%d", start)

	c, _ := serve(t, []Category{fifteen.build(1), defaultCat(start).build(2)})
	rs, err := FetchLive(context.Background(), c, "btc", nowFor(start), DefaultLookahead)
	if err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if len(rs) != 1 || rs[0].CategoryID != 2 {
		t.Fatalf("회차 %+v, 기대 category=2 하나", rs)
	}
}

// 이 봇은 BTC 5분 회차만 거래한다 — 모델이 그것으로 학습됐다.
func TestFetchLiveRejectsOtherSymbols(t *testing.T) {
	c, _ := serve(t, nil)
	if _, err := FetchLive(context.Background(), c, "eth", time.Now(), DefaultLookahead); err == nil {
		t.Error("eth 심볼이 통과했다")
	}
}

// REST 가 실패하면 에러가 그대로 올라온다 — 0개로 뭉개지지 않는다.
func TestFetchCategoriesPropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"success":false,"error":"boom"}`))
	}))
	defer srv.Close()
	c := rest.New("test-api-key-placeholder")
	c.BaseURL = srv.URL

	got, err := FetchCategories(context.Background(), c, "btc", time.Now(), DefaultLookahead)
	if err == nil {
		t.Fatalf("에러가 없다 (회차 %d개)", len(got))
	}
	var he *rest.HTTPError
	if !errors.As(err, &he) {
		t.Errorf("HTTPError 가 아니다: %v", err)
	}
}

func TestFetchCategoriesRejectsNilClient(t *testing.T) {
	if _, err := FetchCategories(context.Background(), nil, "btc", time.Now(), DefaultLookahead); err == nil {
		t.Error("nil 클라이언트가 통과했다")
	}
}
