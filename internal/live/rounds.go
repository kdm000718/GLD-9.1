// Package live 는 봇이 "어느 회차에, 얼마를 기준으로, 어느 방향으로" 거는지를
// 정한다 — 회차 선택(rounds.go), equity 조회(equity.go), p_up 동결(predict.go).
//
// # 이 패키지가 지키는 세 가지
//
//  1. **회차 선택은 한 곳에만 있다.** cmd/probe 가 진단용으로 만든
//     fetchLiveRounds/roundIsLive 를 여기로 옮기고 probe 가 이것을 부른다.
//     두 벌로 두면 갈리고, 갈린 쪽이 틀렸을 때 실거래가 그 틀린 쪽을 탄다.
//  2. **자격 검사는 sample.Features 하나만 쓴다.** 서빙에서 자격 검사를 따로
//     구현하면 게이트가 검증한 적 없는 입력으로 예측이 나간다(predict.go).
//  3. **미확정 가정은 equity.go 한 파일에만 산다.** 실주문으로 진짜 답이
//     확정되면 그 파일만 고친다.
//
// # 실패 방향
//
// 이 패키지의 모든 함수는 **거래하지 않는 쪽으로 실패한다.** 회차 메타데이터가
// 예상과 다르면 0 개를 조용히 돌려주는 대신 에러다 — 조용한 0 은 "지금 열린
// 회차가 없다"와 구분되지 않고, 그 구분이 사라진 채로 P4 의 60분 소크 두 번이
// 통째로 헛돌았다.
package live

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
)

// --- /v1/categories 응답 (OPEN 회차의 마켓 메타데이터 전체) ---
//
// internal/predictfun/rest/categories.go 의 파서는 정산된(RESOLVED) 회차의
// 시작/종료가만 뽑도록 좁혀져 있어(unexported) decimalPrecision·outcomes 가
// 없다. 여기서는 OPEN 회차의 메타데이터 전체가 필요하므로 별도 타입을 둔다.
//
// 타입을 내보내는 이유: cmd/probe 가 소크 진단에서 feeRateBps·shareThreshold·
// spreadThreshold·outcomes 를 그대로 출력한다. Round 로만 내보내면 probe 가
// 자기 파서를 다시 갖게 되고, 그 순간 이 패키지를 만든 이유가 사라진다.
type CategoryPage struct {
	Success bool       `json:"success"`
	Cursor  *string    `json:"cursor"`
	Data    []Category `json:"data"`
}

// Category 는 응답 그대로의 회차 하나다. 시각이 문자열인 것은 의도적이다 —
// 파싱 실패를 제로시각으로 뭉개면 IsLive 가 그것을 "1970년에 끝난 회차"로도
// "지금 열린 회차"로도 볼 수 있고, 그 판단이 필드 이름 변경을 덮는다.
// 파싱은 IsLive/toRound 가 각자 하고, 실패하면 각자의 방식으로 거부한다.
type Category struct {
	ID       int64    `json:"id"`
	Slug     string   `json:"slug"`
	Status   string   `json:"status"`
	StartsAt string   `json:"startsAt"`
	EndsAt   string   `json:"endsAt"`
	Markets  []Market `json:"markets"`
}

type Market struct {
	ID               int64     `json:"id"`
	Status           string    `json:"status"`
	TradingStatus    string    `json:"tradingStatus"`
	DecimalPrecision int       `json:"decimalPrecision"`
	FeeRateBps       int       `json:"feeRateBps"`
	ShareThreshold   int       `json:"shareThreshold"`
	SpreadThreshold  float64   `json:"spreadThreshold"`
	Outcomes         []Outcome `json:"outcomes"`

	// IsNegRisk·IsYieldBearing 은 **어느 Exchange 계약에 서명하는가**를 정한다
	// (order.ExchangeFor). 스펙상 Market 의 required 필드다.
	//
	// **포인터인 것이 이 두 줄의 요점이다.** bool 로 받으면 필드 이름이
	// 바뀌거나 응답에서 빠졌을 때 encoding/json 이 false 를 넣고, false/false 는
	// 하필 지금 맞는 답(CTF_EXCHANGE)이다. 즉 파싱이 깨진 날에도 오늘은
	// 통과하고, 거래소가 상품을 negRisk 로 옮긴 날 조용히 틀린 계약에
	// 서명한다. 포인터면 "없음"과 "false"가 갈리고 toRound 가 없음을 거부한다.
	IsNegRisk      *bool `json:"isNegRisk"`
	IsYieldBearing *bool `json:"isYieldBearing"`
}

type Outcome struct {
	Name      string `json:"name"`
	IndexSet  int    `json:"indexSet"`
	OnChainID string `json:"onChainId"`
}

// Round 는 지금 거래할 수 있는 회차 하나다. 카테고리와 마켓이 1:1 이므로
// 둘을 하나로 접는다(P4 실측: btc-updown-5m-* 카테고리에는 OPEN 마켓이 하나다).
type Round struct {
	CategoryID int64
	MarketID   int64
	Slug       string
	StartsAt   time.Time
	EndsAt     time.Time
	Precision  int
	// FeeRateBps 는 이 마켓의 수수료율이다. **주문 서명에 들어가는 값이라
	// 여기에 담는다** — EIP-712 Order 의 feeRateBps 필드이므로 우리가 임의로
	// 0 을 넣으면 거래소가 기대하는 다이제스트와 달라지고, 서명이 조용히
	// 거부되거나(좋은 경우) 의도와 다른 수수료의 주문이 서명된다. 배선이
	// 마켓 메타데이터를 다시 조회하게 두면 그 조회가 회차와 어긋날 수 있다.
	FeeRateBps  int
	UpTokenID   string
	DownTokenID string

	// IsNegRisk·IsYieldBearing 은 이 마켓의 Exchange 변종이다. **주문 서명의
	// EIP-712 도메인 verifyingContract 가 여기서 나온다**(order.ExchangeFor).
	//
	// 회차와 함께 들고 다니는 이유는 FeeRateBps 와 같다: 서명 경로가 마켓
	// 메타데이터를 다시 조회하면 그 조회 결과가 이 회차와 어긋날 수 있고,
	// 어긋난 순간 우리는 다른 계약에 서명한다.
	IsNegRisk      bool
	IsYieldBearing bool
}

// StartMS 는 회차 시작을 밀리초로 준다. Predictor.Freeze 가 받는 단위다 —
// 초/밀리초 혼동은 이 저장소가 이미 값을 치른 축이라 변환을 한 곳에 둔다.
func (r Round) StartMS() int64 { return r.StartsAt.Unix() * 1000 }

const (
	// SlugSymbol 은 이 봇이 거래하는 유일한 심볼이다. 모델이 BTC 5분봉으로
	// 학습됐으므로 다른 심볼의 회차는 예측 대상이 아니다.
	SlugSymbol = "btc"
	// SlugProduct 는 5분 Up/Down 상품의 슬러그 중간부다.
	SlugProduct = "-updown-5m-"
	// RoundSlugPrefix 는 ParseRoundStart 가 요구하는 접두사다.
	RoundSlugPrefix = SlugSymbol + SlugProduct
	// RoundSeconds 는 회차 길이(초)다. 슬러그의 시작 시각은 이 값의 배수여야
	// 한다 — 아니면 모델의 5분봉 시작과 어긋난다.
	RoundSeconds = 300
)

// DefaultLookahead 는 "아직 시작 안 했지만 곧 시작하는" 회차를 함께 잡는
// 창이다. 회차가 시작하는 순간부터 호가를 받으려면 시작 전에 구독이 끝나
// 있어야 하고, 폴링 간격만큼의 여유는 있어야 한다.
const DefaultLookahead = 60 * time.Second

// 슬러그 시각의 타당 범위(유닉스 초). 2001-09-09 ~ 2096-10-02.
//
// 이 범위가 잡는 것은 **단위 혼동**이다. 밀리초 값(1786275000000)은 300 의
// 배수라 5분 경계 검사를 통과해 버리고, 그대로 Freeze 로 넘어가면 서기 58589년
// 구간의 봉을 요청한다. 5분 경계만으로는 못 막는 실패 모드다.
const (
	minPlausibleUnix = 1_000_000_000
	maxPlausibleUnix = 4_000_000_000
)

// ParseRoundStart 는 슬러그에서 회차 시작 시각(유닉스 초)을 뽑는다.
//
// **이 함수는 G2 측정을 두 번 오염시킨 그 함수다.** 주석에는 "5분 상품만"이라고
// 적혀 있었는데 코드가 그것을 강제하지 않아 15분 상품(`btc-updown-15m-*`)이
// 섞여 들어왔다. 접두사를 느슨하게 보면(예: "btc-updown-" 만 확인, 또는
// 마지막 '-' 뒤를 자르기) 15분·1시간 회차가 전부 통과한다. 그래서 여기서는
// 세 가지를 전부 강제한다.
//
//  1. 접두사가 정확히 RoundSlugPrefix 다. 대소문자 변형·다른 심볼·다른 만기는
//     전부 거부한다.
//  2. 남은 부분이 **숫자만**이다. strconv.ParseInt 는 "+123"·"-123" 을
//     받아들이므로 부호를 직접 막는다.
//  3. 값이 5분 경계(RoundSeconds 의 배수)이고 타당 범위 안이다.
func ParseRoundStart(slug string) (int64, bool) {
	digits, ok := strings.CutPrefix(slug, RoundSlugPrefix)
	if !ok || digits == "" {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	v, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false // 자릿수 초과(int64 범위 밖)
	}
	if v%RoundSeconds != 0 {
		return 0, false
	}
	if v < minPlausibleUnix || v > maxPlausibleUnix {
		return 0, false
	}
	return v, true
}

// IsLive 는 회차가 "진행 중이거나 lookahead 안에 시작"하는지 본다.
//
// startsAt/endsAt 을 못 읽으면 **버린다**. 파싱 실패를 통과시키면 창이 없는
// 것과 같아져서 24시간 앞 사전등록 회차가 다시 전부 들어온다 — 걸러내려던
// 바로 그 실패 모드다. 파싱 실패는 필드 이름이 바뀐 경우이므로, 조용히
// 통과시키는 것보다 회차가 0개로 보여 눈에 띄는 편이 낫다.
func IsLive(cat Category, now time.Time, lookahead time.Duration) bool {
	startsAt, err := time.Parse(time.RFC3339, cat.StartsAt)
	if err != nil {
		return false
	}
	endsAt, err := time.Parse(time.RFC3339, cat.EndsAt)
	if err != nil {
		return false
	}
	if !endsAt.After(now) {
		return false // 이미 끝났다(API 가 아직 OPEN 으로 보여주더라도)
	}
	return !startsAt.After(now.Add(lookahead))
}

// FetchCategories 는 symbolPrefix(예: "btc")로 시작하는 5분 Up/Down 회차 중
// **지금 진행 중인 것과 곧 시작할 것**의 원본 메타데이터를 돌려준다.
//
// **정렬이 핵심이다(2026-08-09 실측)**: predict.fun 은 회차를 24시간 앞까지
// 사전 등록해 둔다. status=OPEN 은 그 미래 회차도 전부 포함하므로,
// PUBLISHED_AT_DESC(나중에 등록된 것 먼저)로 받으면 1페이지가 +23.1~23.9시간
// 뒤 회차로 가득 차고 진행 중 회차에 닿으려면 24페이지쯤 들어가야 한다.
// P4 의 60분 소크 두 번이 진행 중 회차를 하나도 잡지 못한 원인이 이것이다.
// PUBLISHED_AT_ASC 로 뒤집으면 "아직 안 끝난 것 중 가장 먼저 등록된 것"부터
// 오므로 1페이지에 -0.07 ~ +0.68시간이 들어온다.
// (STARTS_AT_ASC / ENDS_AT_ASC 는 400 — 서버가 받지 않는 sort 값이다.)
//
// 정렬만으로는 부족해서 시각 창으로 한 번 더 거른다: ASC 1페이지에도 최대
// +0.68시간 뒤 회차까지 섞여 오기 때문이다.
//
// 페이지네이션을 하지 않는 이유: 1페이지(first=50)에 btc-updown-5m-* 이
// 10~13개 — 5분 회차 한 시간치다. 시각 창을 통과하는 것은 그중 한둘뿐이고,
// 새로 열리는 회차도 매 폴링에서 1페이지 안에 들어온다.
func FetchCategories(ctx context.Context, c *rest.Client, symbolPrefix string, now time.Time, lookahead time.Duration) ([]Category, error) {
	if c == nil {
		return nil, fmt.Errorf("회차 조회: rest.Client 가 nil 이다")
	}
	// 빈 접두사를 그대로 두면 "-updown-5m-" 로 시작하는 슬러그가 없으므로
	// 회차 0개가 조용히 나온다 — 배선 실수가 "지금 열린 회차 없음"으로 보인다.
	if symbolPrefix == "" {
		return nil, fmt.Errorf("회차 조회: 심볼 접두사가 비었다")
	}
	if lookahead < 0 {
		return nil, fmt.Errorf("회차 조회: lookahead 가 음수다 (%s)", lookahead)
	}

	q := url.Values{}
	q.Set("marketVariant", "CRYPTO_UP_DOWN")
	q.Set("status", "OPEN")
	q.Set("sort", "PUBLISHED_AT_ASC")
	q.Set("first", "50")

	var pg CategoryPage
	if err := c.Get(ctx, "/v1/categories", q, &pg); err != nil {
		return nil, err
	}

	prefix := symbolPrefix + SlugProduct
	var out []Category
	for _, cat := range pg.Data {
		if !strings.HasPrefix(cat.Slug, prefix) {
			continue
		}
		if !IsLive(cat, now, lookahead) {
			continue
		}
		out = append(out, cat)
	}
	return out, nil
}

// FetchLive 는 지금 거래할 수 있는 회차를 돌려준다.
//
// FetchCategories 와 달리 **거래에 필요한 것이 하나라도 미심쩍으면 에러다.**
// 진단(probe)은 이상한 마켓을 건너뛰고 계속 도는 편이 낫지만, 거래는 반대다 —
// precision 을 모르면 틱을 모르고, 토큰 ID 를 모르면 어느 쪽에 거는지를 모른다.
// 그 상태로 넘어간 값은 주문이 되어 나간다.
//
// **한 회차가 이상하면 그 폴링 전체가 에러다.** 나쁜 것만 건너뛰고 나머지를
// 돌려주면 "회차가 하나 줄었다"는 사실이 아무 데도 남지 않는다 — 15분 상품이
// 섞여 G2 측정 두 번을 오염시켰을 때가 정확히 그 모습이었다. 시각 창을
// 통과하는 회차는 많아야 한둘이므로 이 엄격함이 정상 경로를 막지 않는다.
func FetchLive(ctx context.Context, c *rest.Client, symbol string, now time.Time, lookahead time.Duration) ([]Round, error) {
	// 심볼을 여기서 막는다. ParseRoundStart 는 btc 5분 상품만 받으므로
	// 다른 심볼을 넘기면 아래에서 "슬러그를 못 읽었다"는 엉뚱한 에러가 난다.
	if symbol != SlugSymbol {
		return nil, fmt.Errorf("회차 조회: 이 봇은 %q 5분 회차만 거래한다 (받은 심볼 %q) — 모델이 BTC 5분봉으로 학습됐다", SlugSymbol, symbol)
	}
	cats, err := FetchCategories(ctx, c, symbol, now, lookahead)
	if err != nil {
		return nil, err
	}
	out := make([]Round, 0, len(cats))
	for _, cat := range cats {
		r, err := toRound(cat)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// toRound 는 카테고리 하나를 거래 가능한 Round 로 바꾼다. 바꿀 수 없으면 에러다.
func toRound(cat Category) (Round, error) {
	startUnix, ok := ParseRoundStart(cat.Slug)
	if !ok {
		return Round{}, fmt.Errorf("회차 %q: 슬러그에서 5분 회차 시작 시각을 읽지 못했다 — 다른 만기 상품이 섞였을 수 있다", cat.Slug)
	}
	startsAt, err := time.Parse(time.RFC3339, cat.StartsAt)
	if err != nil {
		return Round{}, fmt.Errorf("회차 %q: startsAt 을 읽지 못했다 (%q)", cat.Slug, cat.StartsAt)
	}
	endsAt, err := time.Parse(time.RFC3339, cat.EndsAt)
	if err != nil {
		return Round{}, fmt.Errorf("회차 %q: endsAt 을 읽지 못했다 (%q)", cat.Slug, cat.EndsAt)
	}
	// 슬러그와 startsAt 의 대조. 둘 중 하나가 우리 이해와 다르면 여기서 걸린다 —
	// p_up 은 슬러그에서 온 시각으로 동결하고 회차 종료는 endsAt 으로 판단하므로,
	// 둘이 어긋나면 다른 회차를 예측한 값으로 이 회차에 거는 것이 된다.
	if startsAt.Unix() != startUnix {
		return Round{}, fmt.Errorf("회차 %q: 슬러그의 시작 시각(%d)과 startsAt(%d)이 다르다", cat.Slug, startUnix, startsAt.Unix())
	}
	if !endsAt.After(startsAt) {
		return Round{}, fmt.Errorf("회차 %q: endsAt 이 startsAt 보다 뒤가 아니다 (%s ~ %s)", cat.Slug, cat.StartsAt, cat.EndsAt)
	}

	var open []Market
	for _, m := range cat.Markets {
		if m.TradingStatus == "OPEN" {
			open = append(open, m)
		}
	}
	if len(open) == 0 {
		return Round{}, fmt.Errorf("회차 %q: tradingStatus 가 OPEN 인 마켓이 없다 (마켓 %d개)", cat.Slug, len(cat.Markets))
	}
	// P4 실측상 카테고리당 마켓은 하나다. 둘 이상이면 어느 쪽에 걸지 우리가
	// 모른다는 뜻이고, 임의로 첫 번째를 고르면 그 선택이 아무 데도 안 남는다.
	if len(open) > 1 {
		return Round{}, fmt.Errorf("회차 %q: OPEN 마켓이 %d개다 — 어느 마켓에 거는지 정할 수 없다", cat.Slug, len(open))
	}
	m := open[0]

	// decimalPrecision 이 0 이면 십중팔구 필드 이름이 바뀌어 encoding/json 이
	// 제로값을 넣은 것이다(이 저장소가 두 번 겪은 실패 모드). 그 값으로
	// order.Ceiling 을 부르면 패닉하고, 패닉은 살아 있는 주문을 들고 죽는다.
	if m.DecimalPrecision < 1 || m.DecimalPrecision > ws.MaxPrecision {
		return Round{}, fmt.Errorf("회차 %q: decimalPrecision 이 %d 다 (1..%d 여야 한다) — 필드 이름이 바뀌었을 수 있다(0 은 JSON 제로값이다)",
			cat.Slug, m.DecimalPrecision, ws.MaxPrecision)
	}

	// outcome 이름은 ledger 의 상수를 그대로 쓴다. "Up" 을 여기 다시 적으면
	// 원장이 기록하는 방향 문자열과 주문이 향하는 토큰이 서로 다른 근거를
	// 갖게 된다 — 소문자 하나 차이로 갈라질 자리를 만들지 않는다.
	up, down := "", ""
	for _, o := range m.Outcomes {
		switch {
		case strings.EqualFold(o.Name, ledger.OutcomeUp):
			up = o.OnChainID
		case strings.EqualFold(o.Name, ledger.OutcomeDown):
			down = o.OnChainID
		}
	}
	if err := checkTokenID(cat.Slug, ledger.OutcomeUp, up); err != nil {
		return Round{}, err
	}
	if err := checkTokenID(cat.Slug, ledger.OutcomeDown, down); err != nil {
		return Round{}, err
	}
	// 두 토큰 ID 가 같으면 어느 방향으로 걸어도 같은 주식을 산다. 응답이
	// 잘못됐든 우리 파싱이 잘못됐든, 그 상태의 주문은 방향이 없는 주문이다.
	if up == down {
		return Round{}, fmt.Errorf("회차 %q: Up 과 Down 의 onChainId 가 같다 (%s)", cat.Slug, up)
	}

	// feeRateBps 가 음수면 수수료가 아니라 리베이트로 서명된다. 응답에 그런
	// 값이 올 이유가 없으므로 파싱이나 필드가 어긋난 것이고, 그 값으로 만든
	// 주문은 우리가 의도한 주문이 아니다.
	if m.FeeRateBps < 0 {
		return Round{}, fmt.Errorf("회차 %q: feeRateBps 가 %d 다 (음수일 수 없다)", cat.Slug, m.FeeRateBps)
	}

	// **없으면 거래하지 않는다.** 이 두 불린이 어느 Exchange 계약에 서명할지를
	// 정한다(order.ExchangeFor). 빠진 값을 false 로 읽으면 오늘은 맞는 답
	// (CTF_EXCHANGE)이 나오지만, 그건 파싱이 깨졌다는 사실을 덮을 뿐이다 —
	// 거래소가 이 상품을 negRisk 로 옮기는 날 조용히 틀린 계약에 서명한다.
	if m.IsNegRisk == nil || m.IsYieldBearing == nil {
		return Round{}, fmt.Errorf(
			"회차 %q: isNegRisk/isYieldBearing 이 응답에 없다 (isNegRisk=%v, isYieldBearing=%v) — "+
				"어느 Exchange 계약에 서명할지 정할 수 없다 (필드 이름이 바뀌었을 수 있다)",
			cat.Slug, m.IsNegRisk, m.IsYieldBearing)
	}

	return Round{
		CategoryID:     cat.ID,
		MarketID:       m.ID,
		Slug:           cat.Slug,
		StartsAt:       startsAt,
		EndsAt:         endsAt,
		Precision:      m.DecimalPrecision,
		FeeRateBps:     m.FeeRateBps,
		UpTokenID:      up,
		DownTokenID:    down,
		IsNegRisk:      *m.IsNegRisk,
		IsYieldBearing: *m.IsYieldBearing,
	}, nil
}

// checkTokenID 는 onChainId 가 CTF 토큰 ID 로 쓸 수 있는 10진 문자열인지 본다.
// 빈 값·0·비숫자는 전부 거부한다 — 그 값으로 만든 주문은 존재하지 않는
// 토큰에 걸리거나 서명 단계에서 조용히 0 이 된다.
func checkTokenID(slug, side, id string) error {
	if id == "" {
		return fmt.Errorf("회차 %q: %s outcome 의 onChainId 가 없다 — 필드 이름이 바뀌었을 수 있다", slug, side)
	}
	for i := 0; i < len(id); i++ {
		if id[i] < '0' || id[i] > '9' {
			return fmt.Errorf("회차 %q: %s outcome 의 onChainId 가 10진 숫자가 아니다 (%q)", slug, side, id)
		}
	}
	if strings.Trim(id, "0") == "" {
		return fmt.Errorf("회차 %q: %s outcome 의 onChainId 가 0 이다", slug, side)
	}
	return nil
}
