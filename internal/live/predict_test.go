package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/klines"
	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/model"
)

// roundT 는 테스트가 쓰는 회차 시작 시각(ms)이다. 5분 경계이고 타당 범위 안이다.
const roundT = int64(1786275000) * 1000

// --- 동결 규약 ---

// p_up 은 한 번 계산하면 바뀌지 않는다. exec 는 Frozen 을 값으로 들고 다니고
// Predictor 를 참조하지 않는다 — 참조가 없으면 재계산이 실수로도 일어나지 않는다.
func TestFrozenIsAValueNotAReference(t *testing.T) {
	// Frozen 에 포인터나 함수 필드가 없음을 리플렉션으로 고정한다 —
	// 나중에 누가 "다시 계산" 경로를 추가하면 여기서 걸린다.
	typ := reflect.TypeOf(Frozen{})
	for i := 0; i < typ.NumField(); i++ {
		switch typ.Field(i).Type.Kind() {
		case reflect.Ptr, reflect.Func, reflect.Interface, reflect.Chan, reflect.Map, reflect.Slice:
			t.Errorf("Frozen.%s 가 %v 다 — 값 타입만 허용한다", typ.Field(i).Name, typ.Field(i).Type.Kind())
		}
	}
	// 값 타입이어야 대입이 복사가 된다. 위 검사가 통과해도 필드가 하나도
	// 없으면 무의미하므로 개수도 본다.
	if typ.NumField() == 0 {
		t.Error("Frozen 에 필드가 없다")
	}
}

// --- 문턱 ---

// confidence = 2×|p−0.5|. 문턱 0.0714 (p_up ≥ 0.5357 또는 ≤ 0.4643).
func TestEligibleThreshold(t *testing.T) {
	cases := []struct {
		pUp  float64
		want bool
	}{
		{0.5358, true},  // conf = 0.0716000000000001… ≥ 0.0714
		{0.5357, false}, // conf = 0.0713999999999999… < 0.0714 (아래 대칭 시험 참고)
		{0.4643, true},  // conf = 0.0714000000000000… ≥ 0.0714
		{0.4644, false}, // conf = 0.0712000000000000… < 0.0714
		{0.52, false},   // conf 0.04 — 예전 문턱(0.0172)에서는 통과했다
		{0.48, false},   // conf 0.04
		{0.5, false},
		{0.501, false},
		{0.9, true},
		{0.1, true},
	}
	for _, tc := range cases {
		f := newFrozen(roundT, tc.pUp)
		if f.Eligible != tc.want {
			t.Errorf("p_up=%v: Eligible=%v(conf=%.17g), 기대 %v", tc.pUp, f.Eligible, f.Confidence, tc.want)
		}
	}
}

// **부등호가 `>=` 인 것을 정확히 못 박는다.**
//
// p_up 쪽에서는 이것을 시험할 수 없다: 2×|p−0.5| 가 문턱과 **정확히** 같아지는
// float64 p 를 만들 수 없다(십진 경계값이 이진수로 정확히 떨어지지 않는다).
// 그래서 문턱 판정만 떼어내 정확한 값으로 시험한다 — 이것이 없으면
// `>=` → `>` 변이가 살아남는다.
func TestEligibleIsInclusiveAtThreshold(t *testing.T) {
	if !eligible(ConfidenceThreshold) {
		t.Error("confidence 가 문턱과 정확히 같은데 기각됐다 — 부등호가 > 로 바뀌었다")
	}
	if eligible(math.Nextafter(ConfidenceThreshold, 0)) {
		t.Error("문턱 바로 아래가 통과했다")
	}
	if !eligible(math.Nextafter(ConfidenceThreshold, 1)) {
		t.Error("문턱 바로 위가 기각됐다")
	}
	if eligible(math.NaN()) {
		t.Error("NaN 이 통과했다 — 비교 결과가 뒤집힌 것이다")
	}
}

// **문턱은 십진수로 대칭이 아니다(실측).**
//
// p=0.5357 과 p=0.4643 은 십진수로는 둘 다 confidence 0.0714 지만, float64 로는
// 앞이 0.071399999999999908, 뒤가 0.071400000000000019 다 — **뒤만 통과한다.**
// 문턱 0.0172 시절에는 반대였다(Up 쪽만 통과). 어느 쪽이 관대한지는 문턱 값에
// 따라 바뀌고, 그것이 이 시험이 값을 박아 두는 이유다.
//
// 손실로 이어지는 차이는 아니다(문턱 근처에서 방향 한 쪽이 머리카락만큼 더
// 엄격할 뿐이다). 여기 적어 두는 이유는 두 가지다: 누군가 "대칭이 아니다"를
// 결함으로 오해해 엡실론을 넣지 않도록, 그리고 십진 경계값으로 이 문턱을
// 시험하려는 시도가 왜 실패하는지 남기려고.
func TestThresholdIsNotDecimalSymmetric(t *testing.T) {
	up := newFrozen(roundT, 0.5357)
	down := newFrozen(roundT, 0.4643)
	if up.Eligible {
		t.Errorf("p=0.5357 이 통과했다 (conf=%.17g) — float64 표현이 바뀌었나", up.Confidence)
	}
	if !down.Eligible {
		t.Errorf("p=0.4643 이 기각됐다 (conf=%.17g)", down.Confidence)
	}
}

// --- 방향 ---

func TestDirection(t *testing.T) {
	if got := newFrozen(roundT, 0.7).Direction; got != ledger.OutcomeUp {
		t.Errorf("p_up=0.7 의 방향이 %q, 기대 %q", got, ledger.OutcomeUp)
	}
	if got := newFrozen(roundT, 0.3).Direction; got != ledger.OutcomeDown {
		t.Errorf("p_up=0.3 의 방향이 %q, 기대 %q", got, ledger.OutcomeDown)
	}
	// 정확히 0.5 는 confidence 0 이라 거래하지 않는다. 방향은 정의돼 있어야
	// 하지만(빈 문자열은 원장 검증에서 거부된다) 그 값은 쓰이지 않는다.
	f := newFrozen(roundT, 0.5)
	if f.Eligible {
		t.Error("p_up=0.5 가 통과했다")
	}
	if f.Direction != ledger.OutcomeUp && f.Direction != ledger.OutcomeDown {
		t.Errorf("방향이 %q — 원장이 받는 값이어야 한다", f.Direction)
	}
}

// 방향 문자열은 ledger 의 상수 그대로여야 한다. "up"/"UP" 은 원장 기록에서
// 거부되고, 그 거부는 체결이 이미 난 뒤에 온다.
func TestDirectionUsesLedgerConstants(t *testing.T) {
	for _, p := range []float64{0.9, 0.1} {
		d := newFrozen(roundT, p).Direction
		if d != ledger.OutcomeUp && d != ledger.OutcomeDown {
			t.Fatalf("방향 %q 는 원장이 아는 값이 아니다", d)
		}
	}
}

// --- Freeze 배선 ---

// synthKlines 는 [startMS, endMS] 안의 정렬된 봉을 만든다. 가격은 완만한
// 추세에 사인파를 얹어 지표가 상수가 되지 않게 한다(상수면 표준편차 0 →
// 피처가 NaN → features.Build 가 거부한다).
func synthKlines(interval string, startMS, endMS int64) []klines.Kline {
	step := int64(minuteMS)
	if interval == "5m" {
		step = fiveMinMS
	}
	first := startMS
	if m := first % step; m != 0 {
		first += step - m
	}
	var out []klines.Kline
	for ot := first; ot <= endMS; ot += step {
		out = append(out, oneKline(ot, step))
	}
	return out
}

func priceAt(minuteIdx int64) float64 {
	return 100 + float64(minuteIdx)*0.01 + 3*math.Sin(float64(minuteIdx)/7)
}

func oneKline(ot, step int64) klines.Kline {
	n := step / minuteMS
	o := priceAt(ot / minuteMS)
	c := priceAt((ot+step)/minuteMS - 1)
	c += 0.02 // open != close 를 유지한다(도지 방지)
	hi, lo := math.Max(o, c)+0.05, math.Min(o, c)-0.05
	return klines.Kline{
		OpenTime:      ot,
		Open:          o,
		High:          hi,
		Low:           lo,
		Close:         c,
		Volume:        10 * float64(n),
		CloseTime:     ot + step - 1,
		QuoteVolume:   1000 * float64(n),
		Trades:        5 * n,
		TakerBuyBase:  5 * float64(n),
		TakerBuyQuote: 500 * float64(n),
	}
}

type fetchRecord struct {
	symbol   string
	interval string
	startMS  int64
	endMS    int64
}

// fakeFetch 는 요청을 기록하고 합성 봉을 돌려준다. **바이낸스를 부르지 않는다.**
//
// Freeze 가 1분봉·5분봉을 **동시에** 부르므로(2026-08-14) 이 클로저는 두
// 고루틴에서 함께 불린다. 뮤텍스 없이 슬라이스에 붙이면 -race 가 잡는다.
func fakeFetch(rec *[]fetchRecord) FetchKlines {
	var mu sync.Mutex
	return func(ctx context.Context, symbol, interval string, startMS, endMS int64) ([]klines.Kline, error) {
		mu.Lock()
		*rec = append(*rec, fetchRecord{symbol, interval, startMS, endMS})
		mu.Unlock()
		return synthKlines(interval, startMS, endMS), nil
	}
}

// zeroModel 은 계수가 전부 0 인 모델이다 — Prob 이 Sigmoid(Intercept) 로
// 고정되므로 p_up 을 테스트가 정할 수 있다.
func zeroModel(intercept float64) *model.LogReg {
	n := len(features.FeatureNames)
	m := &model.LogReg{
		Coef:         make([]float64, n),
		Intercept:    intercept,
		Mu:           make([]float64, n),
		Sd:           make([]float64, n),
		FeatureNames: append([]string(nil), features.FeatureNames...),
	}
	for i := range m.Sd {
		m.Sd[i] = 1
	}
	return m
}

func TestFreezeProducesFrozenPrediction(t *testing.T) {
	var rec []fetchRecord
	p := &Predictor{Model: zeroModel(0.5), Fetch: fakeFetch(&rec)}

	f, err := p.Freeze(context.Background(), roundT)
	if err != nil {
		t.Fatalf("동결 실패: %v", err)
	}
	if f.T != roundT {
		t.Errorf("T = %d, 기대 %d", f.T, roundT)
	}
	want := model.Sigmoid(0.5)
	if f.PUp != want {
		t.Errorf("p_up = %v, 기대 %v", f.PUp, want)
	}
	if f.Direction != ledger.OutcomeUp || !f.Eligible {
		t.Errorf("방향 %q, 자격 %v — 기대 Up/true", f.Direction, f.Eligible)
	}

	// 두 번 불러도 같은 값이다.
	g, err := p.Freeze(context.Background(), roundT)
	if err != nil {
		t.Fatalf("두 번째 동결 실패: %v", err)
	}
	if f != g {
		t.Errorf("같은 t 로 두 번 불렀는데 값이 다르다: %+v vs %+v", f, g)
	}
}

// 회차 시작 시각 t 의 봉은 **요청하지도 않는다.** 그 봉의 종가가 우리가
// 맞히려는 값이다 — 미래참조가 이 저장소의 ★ 위험 축이다.
func TestFreezeNeverRequestsBarsAtOrAfterT(t *testing.T) {
	var rec []fetchRecord
	p := &Predictor{Model: zeroModel(0), Fetch: fakeFetch(&rec)}
	if _, err := p.Freeze(context.Background(), roundT); err != nil {
		t.Fatalf("동결 실패: %v", err)
	}
	if len(rec) != 2 {
		t.Fatalf("봉 조회 %d회, 기대 2회(1분·5분): %+v", len(rec), rec)
	}
	byInterval := map[string]fetchRecord{}
	for _, r := range rec {
		if r.symbol != DefaultSymbol {
			t.Errorf("심볼 %q, 기대 %q", r.symbol, DefaultSymbol)
		}
		if r.endMS >= roundT {
			t.Errorf("%s 봉을 t 이후까지 요청했다 (endMS=%d, t=%d)", r.interval, r.endMS, roundT)
		}
		byInterval[r.interval] = r
	}
	if got := byInterval["1m"]; got.startMS != roundT-int64(Bars1mCount)*minuteMS {
		t.Errorf("1분봉 startMS = %d, 기대 %d", got.startMS, roundT-int64(Bars1mCount)*minuteMS)
	}
	if got := byInterval["5m"]; got.startMS != roundT-int64(Bars5mCount)*fiveMinMS {
		t.Errorf("5분봉 startMS = %d, 기대 %d", got.startMS, roundT-int64(Bars5mCount)*fiveMinMS)
	}
}

// 피처가 실제로 모델에 들어가는지 본다. 계수가 0 인 모델과 1 인 모델의 예측이
// 같다면 피처가 어디선가 버려지고 있는 것이다.
func TestFreezeFeedsFeaturesToModel(t *testing.T) {
	var rec []fetchRecord
	zero := &Predictor{Model: zeroModel(0), Fetch: fakeFetch(&rec)}
	fz, err := zero.Freeze(context.Background(), roundT)
	if err != nil {
		t.Fatalf("동결 실패: %v", err)
	}

	// 계수를 작게 준다(1e-7). 1 로 주면 표준화되지 않은 피처 60개의 합이 커져
	// Sigmoid 가 정확히 1 로 포화하고, 그것은 (당연히) 거부된다.
	m := zeroModel(0)
	for i := range m.Coef {
		m.Coef[i] = 1e-7
	}
	weighted := &Predictor{Model: m, Fetch: fakeFetch(&rec)}
	fw, err := weighted.Freeze(context.Background(), roundT)
	if err != nil {
		t.Fatalf("동결 실패: %v", err)
	}
	if fz.PUp == fw.PUp {
		t.Errorf("계수를 바꿨는데 p_up 이 같다 (%v) — 피처가 모델에 들어가지 않는다", fz.PUp)
	}
	t.Logf("계수 0 → p_up=%v, 계수 1e-7 → p_up=%v", fz.PUp, fw.PUp)
}

// 자격 검사는 sample.Features 하나만 쓴다. 봉이 모자라면 예측을 만들지 않고
// ErrIneligible 로 실패한다 — 네트워크 실패와 구분돼야 로그에서 원인이 보인다.
func TestFreezeFailsWhenSampleIsIneligible(t *testing.T) {
	short := func(ctx context.Context, symbol, interval string, startMS, endMS int64) ([]klines.Kline, error) {
		ks := synthKlines(interval, startMS, endMS)
		if len(ks) > 30 {
			ks = ks[len(ks)-30:] // 워밍업 부족
		}
		return ks, nil
	}
	p := &Predictor{Model: zeroModel(0), Fetch: short}
	_, err := p.Freeze(context.Background(), roundT)
	if err == nil {
		t.Fatal("봉이 모자란데 예측이 나왔다")
	}
	if !errors.Is(err, ErrIneligible) {
		t.Errorf("ErrIneligible 이 아니다: %v", err)
	}
}

// 봉 조회가 실패하면 그 에러가 올라온다. ErrIneligible 로 뭉개지면 안 된다 —
// "데이터가 없다" 와 "우리가 못 받았다" 는 다른 조치를 요구한다.
func TestFreezePropagatesFetchError(t *testing.T) {
	boom := errors.New("네트워크")
	p := &Predictor{Model: zeroModel(0), Fetch: func(ctx context.Context, symbol, interval string, startMS, endMS int64) ([]klines.Kline, error) {
		return nil, boom
	}}
	_, err := p.Freeze(context.Background(), roundT)
	if !errors.Is(err, boom) {
		t.Errorf("원래 에러가 사라졌다: %v", err)
	}
	if errors.Is(err, ErrIneligible) {
		t.Error("네트워크 실패가 자격 미달로 보고됐다")
	}
}

// --- 시각·모델 가드 ---

func TestFreezeRejectsBadTimes(t *testing.T) {
	var rec []fetchRecord
	p := &Predictor{Model: zeroModel(0), Fetch: fakeFetch(&rec)}
	for _, tc := range []struct {
		name string
		t    int64
	}{
		{"5분 경계가 아니다", roundT + 1},
		{"1분 경계지만 5분은 아니다", roundT + minuteMS},
		{"초를 밀리초 자리에 넣었다", roundT / 1000},
		{"0", 0},
		{"음수", -roundT},
		{"밀리초를 한 번 더 곱했다", roundT * 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := p.Freeze(context.Background(), tc.t); err == nil {
				t.Errorf("t=%d 가 통과했다", tc.t)
			}
			if len(rec) != 0 {
				t.Errorf("시각이 이상한데 봉을 %d회 조회했다", len(rec))
			}
		})
	}
}

func TestFreezeRejectsMismatchedModel(t *testing.T) {
	var rec []fetchRecord
	cases := []struct {
		name string
		mut  func(*model.LogReg)
	}{
		{"계수 개수가 다르다", func(m *model.LogReg) { m.Coef = m.Coef[:len(m.Coef)-1] }},
		{"mu 개수가 다르다", func(m *model.LogReg) { m.Mu = m.Mu[:len(m.Mu)-1] }},
		{"피처 이름 개수가 다르다", func(m *model.LogReg) { m.FeatureNames = m.FeatureNames[:1] }},
		{"피처 이름이 다르다", func(m *model.LogReg) { m.FeatureNames[3] = "엉뚱한_이름" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := zeroModel(0)
			tc.mut(m)
			p := &Predictor{Model: m, Fetch: fakeFetch(&rec)}
			if _, err := p.Freeze(context.Background(), roundT); err == nil {
				t.Error("어긋난 모델이 통과했다 — 계수가 엉뚱한 피처에 붙는다")
			}
		})
	}
	if _, err := (&Predictor{Fetch: fakeFetch(&rec)}).Freeze(context.Background(), roundT); err == nil {
		t.Error("모델 없이 통과했다")
	}
}

// **표준편차 0 은 "확신 100%" 로 위장한다(실측).**
//
// Sd=0 이면 (x−mu)/0 = ±Inf 이고 Sigmoid 가 정확히 0 이나 1 을 낸다. 확률
// 범위를 [0,1] 로만 검사하면 그 값이 통과하고, confidence = 2×|0−0.5| = 1.0
// 이라 **문턱을 넉넉히 넘겨 최대 크기로 베팅된다.** 고장난 모델이 가장
// 확신하는 모델처럼 보이는 것이다. 두 겹으로 막는다: 모델 검사(Sd)와
// 확률 검사(0/1 배제).
func TestFreezeRejectsBrokenStandardDeviation(t *testing.T) {
	var rec []fetchRecord
	m := zeroModel(0)
	m.Coef[0] = 1
	m.Sd[0] = 0 // 0 으로 나눈다
	p := &Predictor{Model: m, Fetch: fakeFetch(&rec)}
	if f, err := p.Freeze(context.Background(), roundT); err == nil {
		t.Errorf("p_up=%v(자격 %v, 확신 %v) 가 통과했다", f.PUp, f.Eligible, f.Confidence)
	}
	if len(rec) != 0 {
		t.Errorf("모델이 고장인데 봉을 %d회 조회했다", len(rec))
	}
}

// 모델 검사를 지나쳐도 확률 검사가 남는다. p_up 이 정확히 0/1 이면 거부한다.
func TestFreezeRejectsDegenerateProbability(t *testing.T) {
	var rec []fetchRecord
	m := zeroModel(-1e6) // Sigmoid 가 언더플로해 정확히 0 이 된다
	p := &Predictor{Model: m, Fetch: fakeFetch(&rec)}
	f, err := p.Freeze(context.Background(), roundT)
	if err == nil {
		t.Errorf("p_up=%v(확신 %v) 가 통과했다 — 0 은 확신이 아니라 고장이다", f.PUp, f.Confidence)
	}
}

// --- loadBars: 미마감 봉 ---

// **바이낸스 REST 는 진행 중인 봉을 마지막 원소로 준다.** clock.New 가 한 번 더
// 잘라내지만 그건 CloseTime 을 우리가 유도하기 때문에 성립한다 — 이 필터가
// 없어지면 그 우연에 기대게 된다.
func TestLoadBarsDropsUnclosedBar(t *testing.T) {
	forming := func(ctx context.Context, symbol, interval string, startMS, endMS int64) ([]klines.Kline, error) {
		ks := synthKlines(interval, startMS, endMS)
		step := int64(minuteMS)
		if interval == "5m" {
			step = fiveMinMS
		}
		// 진행 중인 봉 하나를 덧붙인다(openTime == t).
		return append(ks, oneKline(roundT, step)), nil
	}
	b, err := loadBars(context.Background(), forming, "BTCUSDT", "1m", minuteMS, Bars1mCount, roundT)
	if err != nil {
		t.Fatalf("봉 적재 실패: %v", err)
	}
	last := b.OpenTime[b.Len()-1]
	if last != roundT-minuteMS {
		t.Errorf("마지막 1분봉 openTime = %d, 기대 %d — 미마감 봉이 버퍼에 들어갔다", last, roundT-minuteMS)
	}
	for i := 0; i < b.Len(); i++ {
		if b.OpenTime[i]+minuteMS > roundT {
			t.Fatalf("%d 번째 봉(openTime=%d)이 t=%d 에 마감되지 않았다", i, b.OpenTime[i], roundT)
		}
	}
}

// CloseTime 은 응답값이 아니라 openTime 에서 유도한다 — clock 의 절단이
// 이 값에 걸려 있다.
func TestLoadBarsDerivesCloseTime(t *testing.T) {
	liar := func(ctx context.Context, symbol, interval string, startMS, endMS int64) ([]klines.Kline, error) {
		ks := synthKlines(interval, startMS, endMS)
		for i := range ks {
			ks[i].CloseTime = 0 // 거래소가 엉뚱한 값을 줬다고 치자
		}
		return ks, nil
	}
	b, err := loadBars(context.Background(), liar, "BTCUSDT", "5m", fiveMinMS, Bars5mCount, roundT)
	if err != nil {
		t.Fatalf("봉 적재 실패: %v", err)
	}
	for i := 0; i < b.Len(); i++ {
		if want := b.OpenTime[i] + fiveMinMS - 1; b.CloseTime[i] != want {
			t.Fatalf("%d 번째 CloseTime = %d, 기대 %d", i, b.CloseTime[i], want)
		}
	}
}

// 중복·역순은 시점 절단(clock 의 이진탐색)을 무력화한다.
func TestLoadBarsRejectsOutOfOrder(t *testing.T) {
	for _, name := range []string{"중복", "역순"} {
		t.Run(name, func(t *testing.T) {
			bad := func(ctx context.Context, symbol, interval string, startMS, endMS int64) ([]klines.Kline, error) {
				ks := synthKlines(interval, startMS, endMS)
				if name == "중복" {
					ks[10] = ks[9]
				} else {
					ks[9], ks[10] = ks[10], ks[9]
				}
				return ks, nil
			}
			if _, err := loadBars(context.Background(), bad, "BTCUSDT", "1m", minuteMS, Bars1mCount, roundT); err == nil {
				t.Errorf("%s 봉이 통과했다", name)
			}
		})
	}
}

// 봉이 하나도 없으면 빈 버퍼가 아니라 에러다. 빈 버퍼는 아래에서
// "자격 미달" 로 보이고, 원인이 조회 실패였다는 사실이 사라진다.
func TestLoadBarsRejectsEmptyResponse(t *testing.T) {
	empty := func(ctx context.Context, symbol, interval string, startMS, endMS int64) ([]klines.Kline, error) {
		return nil, nil
	}
	if _, err := loadBars(context.Background(), empty, "BTCUSDT", "1m", minuteMS, Bars1mCount, roundT); err == nil {
		t.Error("빈 응답이 통과했다")
	}
}

// --- 기본 배선: Fetch 가 비면 klines.Fetch 를 쓴다 ---

// Predictor.Fetch 를 배선하지 않았을 때 실제로 바이낸스 REST 경로를 타는지
// 본다. **실제 바이낸스가 아니라 httptest 서버를 향한다** — klines.BaseURL 을
// 이 테스트 동안만 바꾼다.
//
// 이 테스트가 없으면 기본 경로(실거래가 쓰는 바로 그 경로)는 한 번도 실행되지
// 않는다. 주입한 Fetch 로만 시험하면 "테스트는 통과하는데 실거래에서는 봉을
// 못 받는" 상태가 만들어진다.
func TestFreezeUsesKlinesFetchByDefault(t *testing.T) {
	// 두 조회가 **동시에** 나가므로 핸들러가 병렬로 불린다(2026-08-14).
	// 뮤텍스 없이 슬라이스에 붙이면 -race 가 잡는다.
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		paths = append(paths, q.Get("interval"))
		mu.Unlock()
		start, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
		end, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
		if q.Get("symbol") != DefaultSymbol {
			t.Errorf("symbol = %q", q.Get("symbol"))
		}
		if end >= roundT {
			t.Errorf("endTime=%d 이 t=%d 이상이다", end, roundT)
		}
		rows := make([][]any, 0, 300)
		for _, k := range synthKlines(q.Get("interval"), start, end) {
			rows = append(rows, []any{
				k.OpenTime,
				strconv.FormatFloat(k.Open, 'f', -1, 64),
				strconv.FormatFloat(k.High, 'f', -1, 64),
				strconv.FormatFloat(k.Low, 'f', -1, 64),
				strconv.FormatFloat(k.Close, 'f', -1, 64),
				strconv.FormatFloat(k.Volume, 'f', -1, 64),
				k.CloseTime,
				strconv.FormatFloat(k.QuoteVolume, 'f', -1, 64),
				k.Trades,
				strconv.FormatFloat(k.TakerBuyBase, 'f', -1, 64),
				strconv.FormatFloat(k.TakerBuyQuote, 'f', -1, 64),
				"0",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	}))
	defer srv.Close()

	old := klines.BaseURL
	klines.BaseURL = srv.URL
	defer func() { klines.BaseURL = old }()

	p := &Predictor{Model: zeroModel(0.5)} // Fetch 를 배선하지 않는다
	f, err := p.Freeze(context.Background(), roundT)
	if err != nil {
		t.Fatalf("동결 실패: %v", err)
	}
	if f.PUp != model.Sigmoid(0.5) {
		t.Errorf("p_up = %v", f.PUp)
	}
	// **순서는 단정하지 않는다.** 동시에 부르므로 도착 순서가 매번 다르다.
	// 중요한 것은 둘 다 정확히 한 번씩 나갔다는 것이다.
	mu.Lock()
	got := append([]string(nil), paths...)
	mu.Unlock()
	sort.Strings(got)
	if len(got) != 2 || got[0] != "1m" || got[1] != "5m" {
		t.Errorf("요청한 인터벌 %v, 기대 1m·5m 각 한 번", got)
	}
}

// --- 동시 조회의 실패 처리 (2026-08-14) ---
//
// 1분봉·5분봉을 동시에 부르게 되면서 실패 처리가 두 갈래가 됐다. 한쪽만
// 실패하는 경우를 시험하지 않으면 `if err != nil` 하나를 지워도 전부
// 통과한다 — 실제로 변이 시험에서 그랬다(P1·P2).
//
// **틀리는 방향이 중요하다.** 봉을 못 받았는데 동결이 성공하면, 그 회차는
// 빠진 데이터로 만든 피처에 베팅한다.
func TestFreezeFailsWhenEitherFetchFails(t *testing.T) {
	for _, bad := range []string{"1m", "5m"} {
		t.Run(bad+" 실패", func(t *testing.T) {
			var mu sync.Mutex
			var called []string
			fetch := func(_ context.Context, symbol, interval string, startMS, endMS int64) ([]klines.Kline, error) {
				mu.Lock()
				called = append(called, interval)
				mu.Unlock()
				if interval == bad {
					return nil, errors.New("거래소가 응답하지 않는다")
				}
				return synthKlines(interval, startMS, endMS), nil
			}
			p := &Predictor{Model: zeroModel(0.5), Fetch: fetch}
			_, err := p.Freeze(context.Background(), roundT)
			if err == nil {
				t.Fatalf("%s 조회가 실패했는데 동결이 성공했다 — 빠진 데이터로 베팅한다", bad)
			}
			if !strings.Contains(err.Error(), bad) {
				t.Errorf("에러가 어느 봉인지 말하지 않는다: %v", err)
			}
			// 다른 쪽도 실제로 나갔어야 한다. 순차로 되돌아가면 1분봉이
			// 실패했을 때 5분봉은 아예 요청되지 않는다.
			mu.Lock()
			n := len(called)
			mu.Unlock()
			if n != 2 {
				t.Errorf("조회 %d회, 기대 2회 — 동시에 부르지 않았다(%v)", n, called)
			}
		})
	}
}

// 둘 다 실패하면 1분봉 에러를 먼저 보여 준다. 예전 순차 코드의 순서를
// 유지한다 — 로그를 읽는 사람이 같은 문구를 기대한다.
func TestFreezeReportsTheOneMinuteErrorFirst(t *testing.T) {
	fetch := func(_ context.Context, _, interval string, _, _ int64) ([]klines.Kline, error) {
		return nil, fmt.Errorf("%s 쪽 실패", interval)
	}
	p := &Predictor{Model: zeroModel(0.5), Fetch: fetch}
	_, err := p.Freeze(context.Background(), roundT)
	if err == nil {
		t.Fatal("둘 다 실패했는데 동결이 성공했다")
	}
	if !strings.Contains(err.Error(), "1m") {
		t.Errorf("1분봉 에러가 먼저 나와야 한다: %v", err)
	}
}
