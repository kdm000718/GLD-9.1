package live

// 이 파일은 회차 시작 시각 T 의 p_up 을 **한 번** 계산해 동결한다.
//
// # 회차 중 다시 계산하는 경로가 존재하면 안 된다
//
// 스펙과 사용자 지시의 한 줄이다. exec 는 Frozen 을 값으로 들고 다니고
// Predictor 를 참조하지 않는다 — 참조가 없으면 "다시 계산" 이 실수로도
// 일어날 수 없다. Frozen 에 포인터·함수·인터페이스·슬라이스·맵 필드를 두지
// 않는 것이 그 규약을 타입으로 못 박는 방법이고, predict_test.go 의
// 리플렉션 테스트가 그것을 지킨다.
//
// # 자격 검사는 sample.Features 하나만 쓴다
//
// cmd/train 이 만드는 모델은 sample.Build 가 채택한 표본으로 학습됐고,
// Build 는 sample.Features 를 부른다. 서빙에서 자격 검사를 따로 구현하면
// G1' 게이트가 검증한 적 없는 입력으로 실거래 예측이 나간다. 그래서 여기서는
// 봉을 모아 주기만 하고 채택 여부는 전부 Features 에 맡긴다.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/kdm000718/GLD-9.1/internal/bars"
	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/klines"
	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/model"
	"github.com/kdm000718/GLD-9.1/internal/sample"
)

// ConfidenceThreshold 는 사용자가 정한 문턱이다. confidence = 2×|p_up − 0.5|
// 가 이 값 **미만이면 그 회차는 아무것도 하지 않는다.**
//
// # 값의 이력
//
//	0.0172   ~2026-08-15   G2 가 이 문턱에서 승률 52.270% 를 실측했다.
//	0.0714    2026-08-15~  사용자 결정. p_up ≥ 0.5357 또는 ≤ 0.4643.
//
// # 왜 올렸나 — 손익분기가 승률이 아니라 **판정 승률**이기 때문이다
//
// 0.48 에 걸어 이기면 정산 수수료 2% 를 떼고 주당 0.98 을 받는다. 체결분만
// 보면 손익분기는 0.48/0.98 = 48.98% 다. 그런데 우리가 정하는 것은 "걸까
// 말까" 이고, 걸어도 7.3% 는 체결되지 않는다(2026-08-14 실측 41회차 중 3건).
// 그리고 **그 미체결 회차는 실측상 전부 우리가 이긴 회차다** — 역선택이다.
//
// 명목 1 을 걸 때의 기대수익은
//
//	EV = (0.98/0.48)·(j − 0.073) − 0.927        j = 판정 승률
//
// 이고, EV=0 이 되는 j 는 **52.70%** 다. 체결분 기준 48.98% 보다 3.72%p 높다.
//
// 2026년 백테스트에서 문턱 0.0172 통과분의 판정 승률은 52.50% 였다 —
// 손익분기 **아래**다. 문턱을 올려야 넘는다:
//
//	문턱 0.0172 (상위 79%)   판정 52.50%   EV −0.43%/회차
//	문턱 0.0259 (상위 75%)   판정 52.60%   EV −0.21%/회차
//	문턱 0.0432 (상위 60%)   판정 52.82%   EV +0.24%/회차
//	문턱 0.0714 (상위 40%)   판정 54.10%   EV +2.85%/회차   ← 총 기대수익 최대
//	문턱 0.1154 (상위 20%)   판정 55.58%   EV +5.88%/회차   (회차가 3분의 1)
//
// 회차당 EV 는 문턱과 함께 계속 오르지만, 회차 수가 줄어 **총합**은 0.0714
// 근처에서 최대다. 그 부근이 완만하므로 0.06~0.09 사이면 결과가 비슷하다 —
// 이 값을 소수점까지 맞출 이유는 없다.
//
// # 이 값이 기대는 가정
//
// 미체결율 7.3% 는 41회차 표본이다(오차 ±8%p). 15% 라면 손익분기 판정 승률이
// 57.2% 로 뛰고 이 문턱도 적자가 된다. `cmd/gld91/recorder.go` 가 그 값을
// 제대로 재기 위한 기록을 쌓고 있다.
const ConfidenceThreshold = 0.0714

// DefaultSymbol 은 예측에 쓰는 바이낸스 심볼이다. cmd/train 의 기본값과 같아야
// 한다 — 다른 심볼로 학습된 계수를 다른 심볼의 봉에 적용하면 예측이 아니다.
const DefaultSymbol = "BTCUSDT"

// 회차 시작에 받아 올 봉 개수. features 가 쓰는 창(Win1m=260, Win5m=200)과
// 같다. 더 받아도 Last(창) 에서 잘리고, 덜 받으면 워밍업 부족으로 표본이
// 채택되지 않는다.
const (
	Bars1mCount = features.Win1m
	Bars5mCount = features.Win5m
)

const (
	minuteMS  = 60_000
	fiveMinMS = 300_000
)

// 회차 시작 시각의 타당 범위(밀리초). rounds.go 의 초 단위 범위와 짝이다.
const (
	minPlausibleMS = int64(minPlausibleUnix) * 1000
	maxPlausibleMS = int64(maxPlausibleUnix) * 1000
)

// ErrIneligible 은 그 시각의 표본이 sample 의 자격 검사를 통과하지 못했다는
// 뜻이다(워밍업 부족·결측). 네트워크 실패와 구분해야 로그에서 원인이 보인다 —
// 앞은 데이터가 없는 것이고 뒤는 우리가 못 받은 것이다.
var ErrIneligible = errors.New("표본이 자격 검사를 통과하지 못했다")

// FetchKlines 는 봉을 받아 오는 함수다. 기본값은 klines.Fetch 이고 테스트가
// 갈아 끼운다 — 실제 바이낸스를 부르지 않고 미마감 봉·역순 봉 같은 경우를
// 만들 수 있어야 한다.
type FetchKlines func(ctx context.Context, symbol, interval string, startMS, endMS int64) ([]klines.Kline, error)

// Predictor 는 동결된 예측을 만든다.
//
// 계획서의 정의는 Model 하나였다. Symbol/Fetch 를 더한 이유는 테스트다 —
// 패키지 전역 변수를 갈아 끼우는 방식은 병렬 테스트에서 레이스이고, 실거래
// 코드에 테스트용 전역을 남긴다.
type Predictor struct {
	Model  *model.LogReg
	Symbol string      // 비면 DefaultSymbol
	Fetch  FetchKlines // 비면 klines.Fetch
}

// Frozen 은 회차 하나에 대해 동결된 판단이다. **값 타입만 담는다.**
type Frozen struct {
	T          int64   // 회차 시작 (ms)
	PUp        float64 // 모델이 낸 상승 확률
	Confidence float64 // 2×|PUp − 0.5|
	Direction  string  // ledger.OutcomeUp | ledger.OutcomeDown
	Eligible   bool    // Confidence >= ConfidenceThreshold
}

// Freeze 는 회차 시작 시각 tMS 의 p_up 을 구해 동결한다.
//
// tMS 는 **밀리초**다. Round.StartMS() 가 그 값을 만든다.
func (p *Predictor) Freeze(ctx context.Context, tMS int64) (Frozen, error) {
	if p == nil || p.Model == nil {
		return Frozen{}, fmt.Errorf("예측: 모델이 없다")
	}
	if err := checkFreezeTime(tMS); err != nil {
		return Frozen{}, err
	}
	if err := checkModel(p.Model); err != nil {
		return Frozen{}, err
	}

	fetch := p.Fetch
	if fetch == nil {
		fetch = klines.Fetch
	}
	symbol := p.Symbol
	if symbol == "" {
		symbol = DefaultSymbol
	}

	// **두 조회는 서로를 기다릴 이유가 없다.** 1분봉과 5분봉은 독립이고
	// 순서도 결과에 닿지 않는다(둘 다 tMS 로 절단된다).
	//
	// 2026-08-14 도쿄 서버 실측으로 요청 하나가 46ms 였고(그중 TLS 31ms),
	// 순차로 부르면 92ms 가 회차 시작의 임계 경로에 그대로 얹혔다. 동시에
	// 부르면 둘 중 느린 쪽 하나로 줄어든다.
	//
	// 실패는 예전과 같이 다룬다 — 하나라도 실패하면 이 회차는 걸지 않는다.
	// 1분봉 에러를 5분봉 에러보다 먼저 보는 순서도 그대로 유지한다.
	var b1, b5 bars.Bars
	var err1, err5 error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b1, err1 = loadBars(ctx, fetch, symbol, "1m", minuteMS, Bars1mCount, tMS) }()
	go func() { defer wg.Done(); b5, err5 = loadBars(ctx, fetch, symbol, "5m", fiveMinMS, Bars5mCount, tMS) }()
	wg.Wait()
	if err1 != nil {
		return Frozen{}, err1
	}
	if err5 != nil {
		return Frozen{}, err5
	}

	vals, reason := sample.Features(b1, b5, tMS)
	if reason != sample.Eligible {
		return Frozen{}, fmt.Errorf("예측: t=%d: %w (%s) — 1분봉 %d개, 5분봉 %d개",
			tMS, ErrIneligible, reason, b1.Len(), b5.Len())
	}
	if len(vals) != len(p.Model.Coef) {
		return Frozen{}, fmt.Errorf("예측: 피처 %d개, 모델 계수 %d개", len(vals), len(p.Model.Coef))
	}

	// **float32 로 좁히는 것이 학습과 같은 경로다.** sample.Build 는 피처를
	// model.Matrix(float32)에 넣고 Fit/Prob 이 그 행을 읽는다. 서빙만 float64
	// 정밀도로 넣으면 학습이 본 적 없는 입력이 되고, 차이는 작지만 문턱
	// 문턱 바로 근처에서 채택/기각을 뒤집을 수 있다.
	row := make([]float32, len(vals))
	for i, v := range vals {
		row[i] = float32(v)
	}
	pUp := p.Model.Prob(row)
	// **0 과 1 도 거부한다.** 로지스틱 모델이 정확히 0 이나 1 을 내는 것은
	// z 가 ±무한대이거나 |z| > 745 라서 Sigmoid 가 언더플로한 경우뿐이다 —
	// 둘 다 모델이나 입력이 망가졌다는 뜻이다. 그런데 그 값은 confidence 가
	// 1.0(최대)이라 **문턱을 통과하고 최대 크기로 베팅된다.** 확률 범위를
	// [0,1] 로만 보면 이 경로가 열린 채로 남는다(실측: Sd=0 인 모델이 p=0 을
	// 내고 "Down 확신 100%" 가 됐다).
	if !finite(pUp) || pUp <= 0 || pUp >= 1 {
		return Frozen{}, fmt.Errorf("예측: t=%d: p_up 이 확률이 아니다 (%v) — 0 과 1 은 확신이 아니라 고장이다", tMS, pUp)
	}
	return newFrozen(tMS, pUp), nil
}

// newFrozen 은 p_up 에서 파생 값을 만든다. Freeze 와 나눠 둔 이유는 문턱·방향
// 규칙을 봉 수집 없이 그대로 시험할 수 있게 하기 위해서다.
func newFrozen(tMS int64, pUp float64) Frozen {
	c := 2 * math.Abs(pUp-0.5)
	dir := ledger.OutcomeDown
	if pUp > 0.5 {
		dir = ledger.OutcomeUp
	}
	return Frozen{
		T:          tMS,
		PUp:        pUp,
		Confidence: c,
		Direction:  dir,
		Eligible:   eligible(c),
	}
}

// eligible 은 문턱 판정 하나만 한다.
//
// 부등호가 `>=` 인 것이 사용자 지시다("이 값 **미만이면** 아무것도 하지
// 않는다"). 별도 함수로 둔 이유: float64 에서 confidence 가 문턱과 정확히
// 같아지는 p_up 을 만들 수 없으므로(2×|p−0.5| 는 십진 경계값을 정확히
// 재현하지 못한다), `>=` 와 `>` 를 가르는 시험을 여기서만 정확히 할 수 있다.
func eligible(confidence float64) bool { return confidence >= ConfidenceThreshold }

// checkFreezeTime 은 회차 시작 시각의 단위와 경계를 본다.
//
// 초를 밀리초 자리에 넣는 실수를 여기서 잡는다 — 그 값은 1970년대의 봉을
// 요청하고, 바이낸스는 빈 배열을 주고, 우리는 "표본 자격 미달"이라는 엉뚱한
// 이유를 보게 된다.
func checkFreezeTime(tMS int64) error {
	if tMS%fiveMinMS != 0 {
		return fmt.Errorf("예측: t=%d 가 5분 경계가 아니다 — 모델의 봉 시작과 어긋난다", tMS)
	}
	if tMS < minPlausibleMS || tMS > maxPlausibleMS {
		return fmt.Errorf("예측: t=%d 가 타당 범위(%d..%d ms) 밖이다 — 초를 밀리초 자리에 넣었을 수 있다",
			tMS, minPlausibleMS, maxPlausibleMS)
	}
	return nil
}

// checkModel 은 계수가 지금 코드의 피처와 같은 것인지 본다.
//
// 이름이 어긋난 채로 곱하면 계수가 엉뚱한 피처에 붙는다 — 에러 없이 매 회차
// 다른 근거로 베팅하는 봇이 된다. cmd/gld91 이 기동 시에도 같은 대조를 하지만,
// 예측이 실제로 일어나는 자리에도 둔다. 여기서는 패닉하지 않는다(살아 있는
// 주문을 든 채 죽으면 취소도 못 한다).
func checkModel(m *model.LogReg) error {
	if len(m.Coef) != len(features.FeatureNames) {
		return fmt.Errorf("예측: 모델 계수 %d개, 피처 %d개", len(m.Coef), len(features.FeatureNames))
	}
	if len(m.Mu) != len(m.Coef) || len(m.Sd) != len(m.Coef) {
		return fmt.Errorf("예측: 모델의 mu %d개·sd %d개가 계수 %d개와 다르다", len(m.Mu), len(m.Sd), len(m.Coef))
	}
	if len(m.FeatureNames) != len(features.FeatureNames) {
		return fmt.Errorf("예측: 모델 피처 이름 %d개, 코드의 피처 %d개", len(m.FeatureNames), len(features.FeatureNames))
	}
	for i, n := range features.FeatureNames {
		if m.FeatureNames[i] != n {
			return fmt.Errorf("예측: %d번째 피처 이름이 다르다 (모델 %q, 코드 %q)", i, m.FeatureNames[i], n)
		}
	}
	// 계수·표준화 값이 성한지 본다. model.Fit 은 표준편차가 1e-12 미만이면
	// 1.0 으로 바닥을 깐다 — 그보다 작은 값이 여기 있다는 것은 models.json 이
	// 학습이 만든 것이 아니거나 손상됐다는 뜻이다. 0 이면 Logit 이 ±Inf 가
	// 되고 p_up 이 정확히 0/1 로 나와 **최대 확신처럼 보인다.**
	if !finite(m.Intercept) {
		return fmt.Errorf("예측: 모델 절편이 %v 다", m.Intercept)
	}
	for i := range m.Coef {
		if !finite(m.Coef[i]) || !finite(m.Mu[i]) || !finite(m.Sd[i]) {
			return fmt.Errorf("예측: %d번째 계수/mu/sd 에 유한하지 않은 값이 있다 (%v/%v/%v)", i, m.Coef[i], m.Mu[i], m.Sd[i])
		}
		if m.Sd[i] < 1e-12 {
			return fmt.Errorf("예측: %d번째 표준편차가 %v 다 (%s) — 0 으로 나누면 p_up 이 0 이나 1 이 되고 그것이 최대 확신으로 읽힌다",
				i, m.Sd[i], features.FeatureNames[i])
		}
	}
	return nil
}

// loadBars 는 t 이전에 **마감된** 봉만 모아 bars.Bars 로 만든다.
//
// 미마감 봉 필터가 이 함수의 존재 이유다. 바이낸스 REST 는 진행 중인 봉을
// 마지막 원소로 준다. clock.New 가 close_time 으로 한 번 더 잘라내지만, 그건
// CloseTime 을 우리가 openTime 에서 유도하기 때문에 성립하는 것이다 — 언젠가
// 응답의 closeTime 을 그대로 쓰게 되면 그 방어는 사라진다. **미래참조가 이
// 저장소의 ★ 위험 축이므로** 방어를 두 겹으로 둔다.
//
// 결측 봉을 여기서 막지는 않는다. 자격 검사는 sample.Features 하나만 쓴다는
// 원칙이 그보다 위다 — 여기서 따로 거르면 학습이 채택했을 표본을 서빙이
// 기각하거나 그 반대가 된다.
func loadBars(ctx context.Context, fetch FetchKlines, symbol, interval string, stepMS int64, count int, tMS int64) (bars.Bars, error) {
	startMS := tMS - int64(count)*stepMS
	// endMS 는 t 직전이다. t 에 열리는 봉(= 이 회차의 봉)을 애초에 요청하지
	// 않는다 — 그 봉의 종가가 곧 우리가 맞히려는 값이다.
	endMS := tMS - 1
	ks, err := fetch(ctx, symbol, interval, startMS, endMS)
	if err != nil {
		return bars.Bars{}, fmt.Errorf("예측: %s %s 봉 조회: %w", symbol, interval, err)
	}

	b := bars.Bars{}
	var prev int64 = math.MinInt64
	for _, k := range ks {
		// 마감된 봉만. openTime + 간격 <= t 가 그 조건이다.
		if k.OpenTime+stepMS > tMS {
			continue
		}
		if k.OpenTime <= prev {
			return bars.Bars{}, fmt.Errorf("예측: %s %s 봉이 오름차순이 아니다 (openTime %d 다음에 %d) — 중복이나 역순은 시점 절단을 무력화한다",
				symbol, interval, prev, k.OpenTime)
		}
		prev = k.OpenTime
		b.OpenTime = append(b.OpenTime, k.OpenTime)
		// CloseTime 은 응답값이 아니라 openTime 에서 유도한다. clock 의 절단이
		// 이 값에 걸려 있으므로, 거래소가 주는 값을 그대로 믿는 대신 우리가
		// 아는 규약(openTime + 간격 − 1)을 쓴다.
		b.CloseTime = append(b.CloseTime, k.OpenTime+stepMS-1)
		b.Open = append(b.Open, k.Open)
		b.High = append(b.High, k.High)
		b.Low = append(b.Low, k.Low)
		b.Close = append(b.Close, k.Close)
		b.Volume = append(b.Volume, k.Volume)
		b.QuoteVolume = append(b.QuoteVolume, k.QuoteVolume)
		b.Trades = append(b.Trades, k.Trades)
		b.TakerBuyBase = append(b.TakerBuyBase, k.TakerBuyBase)
		b.TakerBuyQuote = append(b.TakerBuyQuote, k.TakerBuyQuote)
	}
	if b.Len() == 0 {
		return bars.Bars{}, fmt.Errorf("예측: %s %s: t=%d 이전에 마감된 봉을 하나도 받지 못했다 (응답 %d개)", symbol, interval, tMS, len(ks))
	}
	return b, nil
}
