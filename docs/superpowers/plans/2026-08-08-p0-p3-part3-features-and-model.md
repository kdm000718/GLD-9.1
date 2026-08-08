# GLD-9.1 P0~P3 계획 3부 — 피처와 모델

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. 1부(`2026-08-08-p0-p3-model-port-and-alignment-gate.md`)의 Global Constraints 가 그대로 적용된다. Task 번호는 2부에서 이어진다.

**선행 조건:** Task 1~6 완료.

---

### Task 7: 60 피처

Python `btc5m/features.py` 의 포팅. 이 태스크의 성패는 Task 8 의 골든 벡터 대조로
판정되므로, 여기서는 **Python 코드를 옆에 두고 한 줄씩 옮기는 것**이 가장 빠르다.
참조: `/home/kdm00/kdm/btc5m_prediction_agent/btc5m/features.py`.

**Files:**
- Create: `internal/features/names.go`, `internal/features/names_test.go`
- Create: `internal/features/features.go`, `internal/features/features_test.go`

**Interfaces:**
- Consumes: `bars.Bars`, `clock.MarketView`, `indicators.*`, `divergence.*`
- Produces: `features.FeatureNames []string` (60개), `features.Build(v *clock.MarketView) ([]float64, bool)` — 워밍업 부족이거나 비유한 값이 있으면 `(nil, false)`
- Produces: 상수 `features.Min1mBars = 200`, `features.Min5mBars = 100`, `features.Win1m = 260`, `features.Win5m = 200`

- [ ] **Step 1: 피처 이름 테스트를 쓴다**

`internal/features/names_test.go`:

```go
package features

import "testing"

func TestFeatureNamesCountAndUniqueness(t *testing.T) {
	if len(FeatureNames) != 60 {
		t.Fatalf("피처 %d개, 기대 60개", len(FeatureNames))
	}
	seen := map[string]bool{}
	for i, n := range FeatureNames {
		if n == "" {
			t.Fatalf("%d번째 이름이 비어 있다", i)
		}
		if seen[n] {
			t.Fatalf("이름 중복: %s", n)
		}
		seen[n] = true
	}
}

// Python FEATURE_NAMES 와 순서까지 완전히 같아야 한다.
// 이 목록은 btc5m/features.py 의 _build_feature_names() 출력을 그대로 옮긴 것이다.
func TestFeatureNamesMatchPythonOrder(t *testing.T) {
	want := []string{
		"m1_ret1", "m1_ret3", "m1_ret5", "m1_ret15", "m1_ret30", "m1_ret60",
		"m1_rsi", "m1_rsi_slope", "m1_ema_spread", "m1_atr_pct", "m1_dist_ema_atr",
		"m1_range_pos", "m1_body", "m1_vol", "m1_vol_z", "m1_streak",
		"m1_div_reg_bear", "m1_div_reg_bull", "m1_div_hid_bear", "m1_div_hid_bull", "m1_div_score",
		"m5_ret1", "m5_ret2", "m5_ret3", "m5_ret6", "m5_ret12",
		"m5_rsi", "m5_rsi_slope", "m5_ema_spread", "m5_atr_pct", "m5_dist_ema_atr",
		"m5_range_pos", "m5_body", "m5_vol", "m5_vol_z", "m5_streak",
		"m5_div_reg_bear", "m5_div_reg_bull", "m5_div_hid_bear", "m5_div_hid_bull", "m5_div_score",
		"m1_taker_ratio5", "m1_taker_ratio15", "m1_taker_ratio60", "m5_taker_ratio6",
		"m1_upbar_ratio15", "m1_vwap_dev30", "m1_trades_z30", "m1_vol_ratio",
		"tod_sin", "tod_cos",
		"p_elapsed", "p_ret_from_open", "p_body_atr", "p_range_pos",
		"p_high_ext_atr", "p_low_ext_atr", "p_vol_frac", "p_taker_ratio", "p_up_min_frac",
	}
	if len(want) != len(FeatureNames) {
		t.Fatalf("길이 불일치: %d vs %d", len(FeatureNames), len(want))
	}
	for i := range want {
		if FeatureNames[i] != want[i] {
			t.Fatalf("%d번째: %q, 기대 %q", i, FeatureNames[i], want[i])
		}
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

Run: `go test ./internal/features/ -run TestFeatureNames -v`
Expected: FAIL — `undefined: FeatureNames`

- [ ] **Step 3: 피처 이름 구현**

`internal/features/names.go`:

```go
package features

import "fmt"

// FeatureNames 는 피처 벡터의 순서를 정의하는 유일한 근거다.
// Python btc5m/features.py 의 FEATURE_NAMES 와 원소·순서가 완전히 같아야 한다.
var FeatureNames = buildFeatureNames()

func buildFeatureNames() []string {
	var names []string
	for _, tf := range []struct {
		prefix string
		lags   []int
	}{
		{"m1", []int{1, 3, 5, 15, 30, 60}},
		{"m5", []int{1, 2, 3, 6, 12}},
	} {
		for _, l := range tf.lags {
			names = append(names, fmt.Sprintf("%s_ret%d", tf.prefix, l))
		}
		for _, suffix := range []string{
			"rsi", "rsi_slope", "ema_spread", "atr_pct", "dist_ema_atr",
			"range_pos", "body", "vol", "vol_z", "streak",
			"div_reg_bear", "div_reg_bull", "div_hid_bear", "div_hid_bull", "div_score",
		} {
			names = append(names, tf.prefix+"_"+suffix)
		}
	}
	return append(names,
		"m1_taker_ratio5", "m1_taker_ratio15", "m1_taker_ratio60", "m5_taker_ratio6",
		"m1_upbar_ratio15", "m1_vwap_dev30", "m1_trades_z30", "m1_vol_ratio",
		"tod_sin", "tod_cos",
		// 진행 중인 5분봉의 마감된 부분 (k=0 이면 전부 0)
		"p_elapsed", "p_ret_from_open", "p_body_atr", "p_range_pos",
		"p_high_ext_atr", "p_low_ext_atr", "p_vol_frac", "p_taker_ratio", "p_up_min_frac",
	)
}

// index 는 이름 → 위치. Build 가 값을 채울 때 쓴다.
var index = func() map[string]int {
	m := make(map[string]int, len(FeatureNames))
	for i, n := range FeatureNames {
		m[n] = i
	}
	return m
}()
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/features/ -run TestFeatureNames -v`
Expected: PASS

- [ ] **Step 5: 커밋**

```bash
git add internal/features/names.go internal/features/names_test.go
git commit -m "피처 이름 60개 — Python 순서와 일치"
```

- [ ] **Step 6: Build 동작 테스트를 쓴다**

`internal/features/features_test.go`:

```go
package features

import (
	"math"
	"math/rand"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/bars"
	"github.com/kdm000718/GLD-9.1/internal/clock"
)

func synth(n int, stepMS int64, seed int64) bars.Bars {
	r := rand.New(rand.NewSource(seed))
	b := bars.Bars{
		OpenTime: make([]int64, n), CloseTime: make([]int64, n),
		Open: make([]float64, n), High: make([]float64, n), Low: make([]float64, n),
		Close: make([]float64, n), Volume: make([]float64, n),
		QuoteVolume: make([]float64, n), Trades: make([]int64, n),
		TakerBuyBase: make([]float64, n), TakerBuyQuote: make([]float64, n),
	}
	p := 60000.0
	for i := 0; i < n; i++ {
		o := p
		p *= 1 + (r.Float64()-0.5)*0.004
		c := p
		b.OpenTime[i] = int64(i) * stepMS
		b.CloseTime[i] = b.OpenTime[i] + stepMS - 1
		b.Open[i], b.Close[i] = o, c
		b.High[i] = math.Max(o, c) * 1.0005
		b.Low[i] = math.Min(o, c) * 0.9995
		b.Volume[i] = 10 + r.Float64()*5
		b.QuoteVolume[i] = b.Volume[i] * (o + c) / 2
		b.Trades[i] = int64(100 + r.Intn(50))
		b.TakerBuyBase[i] = b.Volume[i] * (0.3 + r.Float64()*0.4)
		b.TakerBuyQuote[i] = b.TakerBuyBase[i] * c
	}
	return b
}

func TestBuildReturnsSixtyFiniteValues(t *testing.T) {
	b1, b5 := synth(2000, 60_000, 11), synth(400, 300_000, 12)
	cs := int64(300) * 300_000
	v, err := clock.New(cs, b1, b5, cs)
	if err != nil {
		t.Fatalf("clock.New: %v", err)
	}
	got, ok := Build(v)
	if !ok {
		t.Fatal("Build 가 false 를 돌려줬다 — 워밍업이 충분한데")
	}
	if len(got) != 60 {
		t.Fatalf("피처 %d개, 기대 60개", len(got))
	}
	for i, x := range got {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			t.Errorf("%s = %v — 유한하지 않다", FeatureNames[i], x)
		}
	}
}

func TestBuildRejectsInsufficientWarmup(t *testing.T) {
	b1, b5 := synth(2000, 60_000, 13), synth(400, 300_000, 14)
	// 5분봉이 Min5mBars 미만이 되도록 이른 시점을 고른다
	cs := int64(50) * 300_000
	v, err := clock.New(cs, b1, b5, cs)
	if err != nil {
		t.Fatalf("clock.New: %v", err)
	}
	if _, ok := Build(v); ok {
		t.Error("워밍업이 부족한데 Build 가 성공했다")
	}
}

// k=0 이면 진행 중인 봉에서 볼 것이 없으므로 p_* 는 p_elapsed 를 빼고 전부 0 이다.
func TestPartialFeaturesAreZeroAtElapsedZero(t *testing.T) {
	b1, b5 := synth(2000, 60_000, 15), synth(400, 300_000, 16)
	cs := int64(300) * 300_000
	v, _ := clock.New(cs, b1, b5, cs)
	got, ok := Build(v)
	if !ok {
		t.Fatal("Build 실패")
	}
	zero := []string{
		"p_ret_from_open", "p_body_atr", "p_range_pos", "p_high_ext_atr",
		"p_low_ext_atr", "p_vol_frac", "p_taker_ratio", "p_up_min_frac",
	}
	for _, name := range zero {
		if v := got[index[name]]; v != 0 {
			t.Errorf("%s = %v, k=0 이면 0 이어야 한다", name, v)
		}
	}
	if got[index["p_elapsed"]] != 0 {
		t.Errorf("p_elapsed = %v, k=0 이면 0", got[index["p_elapsed"]])
	}
}

// 이 패키지 전체의 존재 이유: 절단해도 같은 값이 나와야 한다.
func TestBuildIsInvariantUnderTruncation(t *testing.T) {
	b1, b5 := synth(2000, 60_000, 17), synth(400, 300_000, 18)
	for _, ci := range []int{200, 300, 380} {
		cs := int64(ci) * 300_000
		full, _ := clock.New(cs, b1, b5, cs)
		a, ok1 := Build(full)

		// t 이후 데이터를 물리적으로 삭제한 사본
		var h1, h5 int
		for h1 < b1.Len() && b1.CloseTime[h1] < cs {
			h1++
		}
		for h5 < b5.Len() && b5.CloseTime[h5] < cs {
			h5++
		}
		trunc, err := clock.New(cs, b1.Slice(0, h1), b5.Slice(0, h5), cs)
		if err != nil {
			t.Fatalf("절단 사본 clock.New: %v", err)
		}
		bb, ok2 := Build(trunc)

		if ok1 != ok2 {
			t.Fatalf("ci=%d: ok 가 다르다 %v vs %v", ci, ok1, ok2)
		}
		if !ok1 {
			continue
		}
		for i := range a {
			if a[i] != bb[i] {
				t.Fatalf("ci=%d %s: %v vs %v — 피처가 미래를 본다", ci, FeatureNames[i], a[i], bb[i])
			}
		}
	}
}
```

- [ ] **Step 7: 테스트 실패 확인**

Run: `go test ./internal/features/ -run TestBuild -v`
Expected: FAIL — `undefined: Build`

- [ ] **Step 8: Build 구현**

`internal/features/features.go`:

```go
// Package features 는 MarketView 를 60차원 피처 벡터로 바꾼다.
//
// 여기 있는 모든 코드는 MarketView 가 이미 잘라낸 데이터만 만진다.
// 미래 봉은 객체 안에 존재하지 않으므로 참조할 수단 자체가 없다.
package features

import (
	"math"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/bars"
	"github.com/kdm000718/GLD-9.1/internal/clock"
	"github.com/kdm000718/GLD-9.1/internal/divergence"
	"github.com/kdm000718/GLD-9.1/internal/indicators"
)

// 워밍업 요구치 — 부족하면 그 시점은 표본에서 제외한다.
const (
	Min1mBars = 200
	Min5mBars = 100
	Win1m     = 260
	Win5m     = 200
)

// Build 는 피처 벡터를 만든다. 워밍업 부족이거나 비유한 값이 섞이면 (nil, false).
func Build(v *clock.MarketView) ([]float64, bool) {
	if v.Bars1m.Len() < Min1mBars || v.Bars5m.Len() < Min5mBars {
		return nil, false
	}
	f := make([]float64, len(FeatureNames))
	// 이름을 못 찾으면 즉시 죽인다. map 의 제로값을 그대로 쓰면 오타 하나가
	// 에러 없이 f[0](= m1_ret1)을 덮어쓴다. Python 은 dict 라 나중에
	// FEATURE_NAMES 순회에서 KeyError 가 나지만 Go 슬라이스는 조용히 넘어간다.
	set := func(name string, val float64) {
		i, ok := index[name]
		if !ok {
			panic("알 수 없는 피처 이름: " + name)
		}
		f[i] = val
	}

	b1 := v.Bars1m.Last(Win1m)
	b5 := v.Bars5m.Last(Win5m)

	fillTimeframe(set, b1, "m1", 14, 3, 15.0)
	atr5 := fillTimeframe(set, b5, "m5", 14, 2, 6.0)
	fillOrderflow(set, b1, b5)
	fillTime(set, v.CandleStart)
	if !fillPartial(set, v, b1, atr5) {
		return nil, false
	}

	for _, x := range f {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, false
		}
	}
	return f, true
}

// fillTimeframe 은 한 타임프레임의 피처를 채우고 마지막 ATR 을 돌려준다.
func fillTimeframe(set func(string, float64), b bars.Bars, prefix string,
	rsiPeriod, divK int, divHalflife float64) float64 {

	close, high, low, open := b.Close, b.High, b.Low, b.Open

	lags := []int{1, 3, 5, 15, 30, 60}
	if prefix == "m5" {
		lags = []int{1, 2, 3, 6, 12}
	}
	for _, lag := range lags {
		set(prefix+"_ret"+itoa(lag), indicators.LogReturn(close, lag)*100.0)
	}

	rsiV := indicators.RSI(close, rsiPeriod)
	last := len(rsiV) - 1
	set(prefix+"_rsi", (rsiV[last]-50.0)/50.0)
	set(prefix+"_rsi_slope", (rsiV[last]-rsiV[last-3])/50.0)

	eF := indicators.EMA(close, 9)
	eS := indicators.EMA(close, 21)
	set(prefix+"_ema_spread", (eF[last]-eS[last])/eS[last]*100.0)

	atrV := indicators.ATR(high, low, close, 14)
	lastATR := atrV[last]
	set(prefix+"_atr_pct", lastATR/close[last]*100.0)

	longPeriod := 60
	if prefix == "m5" {
		longPeriod = 50
	}
	eLong := indicators.EMA(close, longPeriod)
	set(prefix+"_dist_ema_atr", (close[last]-eLong[last])/math.Max(lastATR, 1e-9))

	win := 30
	if prefix == "m5" {
		win = 20
	}
	hh, ll := maxOf(high[len(high)-win:]), minOf(low[len(low)-win:])
	if rng := hh - ll; rng > 0 {
		set(prefix+"_range_pos", (close[last]-ll)/rng-0.5)
	} else {
		set(prefix+"_range_pos", 0.0)
	}

	if rngLast := high[last] - low[last]; rngLast > 0 {
		set(prefix+"_body", (close[last]-open[last])/rngLast)
	} else {
		set(prefix+"_body", 0.0)
	}

	volWin := 30
	if prefix == "m5" {
		volWin = 20
	}
	set(prefix+"_vol", indicators.RealizedVol(close, volWin)*100.0)
	set(prefix+"_vol_z", indicators.ZScore(b.Volume, volWin))

	set(prefix+"_streak", streak(close, open)/5.0)

	sig := divergence.Detect(high, low, rsiV, divK, 5, 60, divHalflife)
	set(prefix+"_div_reg_bear", sig.RegularBear)
	set(prefix+"_div_reg_bull", sig.RegularBull)
	set(prefix+"_div_hid_bear", sig.HiddenBear)
	set(prefix+"_div_hid_bull", sig.HiddenBull)
	set(prefix+"_div_score", sig.Score())
	return lastATR
}

// streak 은 연속 같은 방향 봉 개수를 부호 있게 센다. ±5 로 클리핑한다.
// Python 의 누산 로직을 그대로 옮긴 것이다 (부호별 ±1 을 더해 나간다).
func streak(close, open []float64) float64 {
	var s float64
	for i := len(close) - 1; i >= 0; i-- {
		sign := 0.0
		switch {
		case close[i] > open[i]:
			sign = 1
		case close[i] < open[i]:
			sign = -1
		}
		if sign == 0 || (s != 0 && math.Copysign(1, sign) != math.Copysign(1, s)) {
			break
		}
		if s != 0 {
			s += sign
		} else {
			s = sign
		}
		if math.Abs(s) >= 5 {
			break
		}
	}
	return math.Max(-5, math.Min(5, s))
}

func fillOrderflow(set func(string, float64), b1, b5 bars.Bars) {
	for _, w := range []int{5, 15, 60} {
		vol := sumOf(b1.Volume[len(b1.Volume)-w:])
		taker := sumOf(b1.TakerBuyBase[len(b1.TakerBuyBase)-w:])
		v := 0.0
		if vol > 0 {
			v = taker/vol - 0.5
		}
		set("m1_taker_ratio"+itoa(w), v)
	}

	vol5 := sumOf(b5.Volume[len(b5.Volume)-6:])
	taker5 := sumOf(b5.TakerBuyBase[len(b5.TakerBuyBase)-6:])
	v5 := 0.0
	if vol5 > 0 {
		v5 = taker5/vol5 - 0.5
	}
	set("m5_taker_ratio6", v5)

	up := 0
	for i := b1.Len() - 15; i < b1.Len(); i++ {
		if b1.Close[i] > b1.Open[i] {
			up++
		}
	}
	set("m1_upbar_ratio15", float64(up)/15.0-0.5)

	totV := sumOf(b1.Volume[len(b1.Volume)-30:])
	last := b1.Len() - 1
	vwap := b1.Close[last]
	if totV > 0 {
		vwap = sumOf(b1.QuoteVolume[len(b1.QuoteVolume)-30:]) / totV
	}
	set("m1_vwap_dev30", (b1.Close[last]-vwap)/vwap*100.0)

	trades := make([]float64, b1.Len())
	for i, t := range b1.Trades {
		trades[i] = float64(t)
	}
	set("m1_trades_z30", indicators.ZScore(trades, 30))

	shortV := indicators.RealizedVol(b1.Close, 30)
	longV := indicators.RealizedVol(b1.Close, 120)
	if longV > 0 && !math.IsNaN(longV) {
		set("m1_vol_ratio", shortV/longV)
	} else {
		set("m1_vol_ratio", 1.0)
	}
}

// fillPartial 은 진행 중인 5분봉의 이미 마감된 부분에서 피처를 뽑는다.
// k=0 이면 볼 것이 없으므로 전부 0(중립)으로 둔다.
func fillPartial(set func(string, float64), v *clock.MarketView, b1 bars.Bars, atr5 float64) bool {
	k := v.ElapsedMin()
	set("p_elapsed", float64(k)/5.0)
	if k == 0 {
		for _, n := range []string{
			"p_ret_from_open", "p_body_atr", "p_range_pos", "p_high_ext_atr",
			"p_low_ext_atr", "p_vol_frac", "p_taker_ratio", "p_up_min_frac",
		} {
			set(n, 0.0)
		}
		return true
	}

	i0 := b1.IndexOfOpenTime(v.CandleStart)
	if i0 < 0 || b1.Len()-i0 != k {
		return false // 결측 1분봉 — 표본에서 제외
	}
	part := b1.Slice(i0, b1.Len())
	o := part.Open[0] // k>=1 이면 대상 5분봉의 시가를 확정적으로 안다
	c := part.Close[part.Len()-1]
	hi, lo := maxOf(part.High), minOf(part.Low)
	denom := math.Max(atr5, 1e-9)

	if o > 0 {
		set("p_ret_from_open", math.Log(c/o)*100.0)
	} else {
		set("p_ret_from_open", 0.0)
	}
	set("p_body_atr", (c-o)/denom)
	if hi > lo {
		set("p_range_pos", (c-lo)/(hi-lo)-0.5)
	} else {
		set("p_range_pos", 0.0)
	}
	set("p_high_ext_atr", (hi-o)/denom)
	set("p_low_ext_atr", (o-lo)/denom)

	pv := sumOf(part.Volume)
	refVol := meanOf(v.Bars5m.Volume[max0(v.Bars5m.Len()-20):])
	if refVol > 0 {
		set("p_vol_frac", pv/refVol)
	} else {
		set("p_vol_frac", 0.0)
	}
	if pv > 0 {
		set("p_taker_ratio", sumOf(part.TakerBuyBase)/pv-0.5)
	} else {
		set("p_taker_ratio", 0.0)
	}
	upn := 0
	for i := 0; i < part.Len(); i++ {
		if part.Close[i] > part.Open[i] {
			upn++
		}
	}
	set("p_up_min_frac", float64(upn)/float64(part.Len())-0.5)
	return true
}

func fillTime(set func(string, float64), t int64) {
	dt := time.UnixMilli(t).UTC()
	frac := float64(dt.Hour()*60+dt.Minute()) / 1440.0
	set("tod_sin", math.Sin(2*math.Pi*frac))
	set("tod_cos", math.Cos(2*math.Pi*frac))
}

func sumOf(x []float64) float64 {
	var s float64
	for _, v := range x {
		s += v
	}
	return s
}

func meanOf(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	return sumOf(x) / float64(len(x))
}

func maxOf(x []float64) float64 {
	m := x[0]
	for _, v := range x[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func minOf(x []float64) float64 {
	m := x[0]
	for _, v := range x[1:] {
		if v < m {
			m = v
		}
	}
	return m
}

func max0(i int) int {
	if i < 0 {
		return 0
	}
	return i
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
```

- [ ] **Step 9: 테스트 통과 확인**

Run: `go test ./internal/features/ -v && go vet ./...`
Expected: PASS (6개 테스트). `TestBuildIsInvariantUnderTruncation` 이 특히 중요하다.

#### 피처가 누락되는 두 경로와 그 방어

이름을 잘못 쓰거나 빠뜨리면 조용히 틀린 값이 나갈 수 있다. 두 경로를 구분해 둔다.

**오타** — `set("m1_ret5x", …)` 처럼 없는 이름을 쓰는 경우. `set` 이 맵 조회에
실패하면 즉시 panic 한다(Step 8 코드). 방어하지 않으면 맵의 제로값 때문에
`f[0]`(= `m1_ret1`)이 덮여 쓰이고, 두 피처가 동시에 틀리면서 아무 에러도 안 난다.

**호출 누락** — 어떤 피처에 `set` 을 아예 안 하는 경우. 그 피처는 0 으로 남는다.
이건 panic 으로 못 잡지만 **Task 8 의 골든 대조가 잡는다**: 51개 실값 피처는
Python 쪽이 0 이 아니므로 대조에서 즉시 어긋난다. 남은 9개(`p_*`)는 `+0분`에서
정상값도 0 이라 누락돼도 결과가 같다 — 이 범위에서는 무해하다.
(`p_*` 를 쓰는 `k>=1` 경로를 나중에 살릴 때는 이 보증이 사라지므로, 그때
골든 벡터를 `k>=1` 시점으로도 뽑아야 한다.)

- [ ] **Step 10: 커밋**

```bash
git add internal/features/
git commit -m "60 피처 생성 — 절단 불변성 테스트 포함"
```

---

### Task 8: G1 — Python 골든 벡터 대조

Go 피처가 Python 과 같은 값을 내는지 판정한다. **여기서 실패하면 Task 7 로 되돌아간다.**

**Files:**
- Create: `tools/export_golden.py`
- Create: `cmd/goldencheck/main.go`
- Create: `testdata/golden_features.jsonl` (생성물, 커밋한다)

**Interfaces:**
- Consumes: `features.Build`, `vision.LoadFullHistory`, `clock.New`
- Produces: `testdata/golden_features.jsonl` — 한 줄에 `{"candle_start": <ms>, "values": [60개]}`

- [ ] **Step 1: Python 내보내기 스크립트를 쓴다**

`tools/export_golden.py`:

```python
#!/usr/bin/env python3
"""Go 포팅 대조용 골든 벡터를 내보낸다.

btc5m_prediction_agent 의 피처 파이프라인으로 9년 구간에서 2,000시점을 뽑아
(candle_start, 피처 60개) 를 JSONL 로 쓴다. 시드가 고정이라 재현 가능하다.

    cd /home/kdm00/kdm/btc5m_prediction_agent
    python3 /home/kdm00/kdm/GLD-9.1/tools/export_golden.py \
        --out /home/kdm00/kdm/GLD-9.1/testdata/golden_features.jsonl
"""

import argparse
import json
import random
import sys

import numpy as np

from btc5m import features as ft
from btc5m import vision
from btc5m.clock import LookaheadError, MarketView

SEED = 20260808
N = 2000


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    ap.add_argument("--n", type=int, default=N)
    args = ap.parse_args()

    b1 = vision.load_full_history("BTCUSDT", "1m", log=lambda *_: None)
    b5 = vision.load_full_history("BTCUSDT", "5m", log=lambda *_: None)
    print(f"1분봉 {len(b1):,} / 5분봉 {len(b5):,}", file=sys.stderr)

    # 워밍업이 확보되는 구간에서만 뽑는다
    rng = random.Random(SEED)
    lo = int(np.searchsorted(b5.open_time, int(b5.open_time[0]) + 30 * 86_400_000))
    picks = sorted(rng.sample(range(lo, len(b5)), args.n))

    written = skipped = 0
    with open(args.out, "w") as fh:
        for i in picks:
            t = int(b5.open_time[i])
            try:
                built = ft.build(MarketView(t, b1, b5, candle_start=t))
            except LookaheadError:
                skipped += 1
                continue
            if built is None:
                skipped += 1
                continue
            # repr 왕복이 손실 없도록 float 를 그대로 쓴다 (json 은 repr 을 쓴다)
            fh.write(json.dumps({
                "candle_start": t,
                "values": [float(x) for x in built[0]],
            }) + "\n")
            written += 1

    print(f"기록 {written}개, 제외 {skipped}개 → {args.out}", file=sys.stderr)
    print(f"피처 이름 {len(ft.FEATURE_NAMES)}개", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
```

- [ ] **Step 2: 골든 벡터를 생성한다**

```bash
mkdir -p /home/kdm00/kdm/GLD-9.1/testdata
cd /home/kdm00/kdm/btc5m_prediction_agent
python3 /home/kdm00/kdm/GLD-9.1/tools/export_golden.py \
  --out /home/kdm00/kdm/GLD-9.1/testdata/golden_features.jsonl
wc -l /home/kdm00/kdm/GLD-9.1/testdata/golden_features.jsonl
```

Expected: 약 2,000줄. (Vision 캐시가 없으면 다운로드에 수십 분 걸린다.)

- [ ] **Step 3: 대조 러너를 쓴다**

`cmd/goldencheck/main.go`:

```go
// Command goldencheck 는 Go 피처가 Python 골든 벡터와 일치하는지 판정한다 (게이트 G1).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"

	"github.com/kdm000718/GLD-9.1/internal/clock"
	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/vision"
)

type golden struct {
	CandleStart int64     `json:"candle_start"`
	Values      []float64 `json:"values"`
}

func main() {
	path := flag.String("golden", "testdata/golden_features.jsonl", "골든 벡터 파일")
	cache := flag.String("cache", "data", "Vision 캐시 디렉터리")
	tol := flag.Float64("tol", 1e-9, "허용오차")
	flag.Parse()

	if err := run(*path, *cache, *tol); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(1)
	}
}

func run(path, cache string, tol float64) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	var rows []golden
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var g golden
		if err := json.Unmarshal(sc.Bytes(), &g); err != nil {
			return err
		}
		rows = append(rows, g)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	fmt.Printf("[골든] %d개 시점\n", len(rows))

	ctx := context.Background()
	fmt.Println("[데이터] Binance Vision 로드")
	b1, err := vision.LoadFullHistory(ctx, "BTCUSDT", "1m", cache, nil)
	if err != nil {
		return err
	}
	b5, err := vision.LoadFullHistory(ctx, "BTCUSDT", "5m", cache, nil)
	if err != nil {
		return err
	}
	fmt.Printf("  1분봉 %d / 5분봉 %d\n", b1.Len(), b5.Len())

	var checked, mismatch, buildFail int
	worst := 0.0
	worstName := ""
	for _, g := range rows {
		v, err := clock.New(g.CandleStart, b1, b5, g.CandleStart)
		if err != nil {
			buildFail++
			continue
		}
		got, ok := features.Build(v)
		if !ok {
			buildFail++
			continue
		}
		checked++
		bad := false
		for i := range got {
			d := math.Abs(got[i] - g.Values[i])
			if d > worst {
				worst, worstName = d, features.FeatureNames[i]
			}
			if d > tol {
				bad = true
			}
		}
		if bad {
			mismatch++
			if mismatch <= 3 {
				fmt.Printf("  불일치 t=%d\n", g.CandleStart)
				for i := range got {
					if d := math.Abs(got[i] - g.Values[i]); d > tol {
						fmt.Printf("    %-20s go=%+.12g py=%+.12g  차이 %.3g\n",
							features.FeatureNames[i], got[i], g.Values[i], d)
					}
				}
			}
		}
	}

	fmt.Println()
	fmt.Println("==================== G1 피처 동등성 ====================")
	fmt.Printf("  대조 %d개 (Build 실패 %d개)\n", checked, buildFail)
	fmt.Printf("  불일치 %d개\n", mismatch)
	fmt.Printf("  최대 절대차 %.3g (%s), 허용 %.3g\n", worst, worstName, tol)
	if mismatch > 0 {
		return fmt.Errorf("G1 실패 — Task 7 로 돌아간다")
	}
	if checked == 0 {
		return fmt.Errorf("대조한 시점이 하나도 없다")
	}
	fmt.Println("  판정: 통과")
	return nil
}
```

- [ ] **Step 4: G1 을 실행한다**

```bash
cd /home/kdm00/kdm/GLD-9.1
go build ./... && mkdir -p out
go run ./cmd/goldencheck 2>&1 | tee out/g1-golden.log
```

Expected: `판정: 통과`. 불일치가 나오면 출력된 피처 이름으로 Task 7 을 고친다.

- [ ] **Step 5: 결과와 골든 벡터를 커밋**

```bash
cp out/g1-golden.log docs/results/g1-golden.log
git add tools/ cmd/goldencheck/ testdata/golden_features.jsonl docs/results/g1-golden.log
git commit -m "G1 피처 동등성 게이트 — Python 골든 벡터 2000시점 대조"
```

---

### Task 9: 로지스틱 회귀 — 추론과 학습

`scipy.optimize.minimize(method="L-BFGS-B")` 자리에 `gonum.org/v1/gonum/optimize` 의
LBFGS 를 쓴다. 목적함수가 L2 항 때문에 **강볼록**이라 최적점이 유일하고, 수렴
조건만 충분히 조이면 두 최적화기가 같은 점에 도달한다. 이것이 Task 12 재현의 근거다.

100만 행 × 60 피처를 다루므로 `[][]float64` 가 아니라 평면 `float32` 행렬을 쓴다
(Python 도 float32 를 썼다).

**Files:**
- Create: `internal/model/matrix.go`, `internal/model/matrix_test.go`
- Create: `internal/model/logreg.go`, `internal/model/logreg_test.go`

**Interfaces:**
- Produces: `model.Matrix{Rows, Cols int, Data []float32}`, `model.NewMatrix(rows, cols int) *Matrix`, `(*Matrix).Row(i int) []float32`, `(*Matrix).SetRow(i int, v []float64)`, `(*Matrix).Truncate(n int)`
- Produces: `model.LogReg{L2 float64, Coef []float64, Intercept float64, Mu, Sd []float64, NTrain int, FeatureNames []string}`
- Produces: `model.Fit(m *Matrix, rows []int, y []float64, names []string, l2 float64) (*LogReg, error)`, `(*LogReg).Logit(x []float32) float64`, `(*LogReg).Prob(x []float32) float64`, `model.Sigmoid(z float64) float64`
- Produces: `(*LogReg).MarshalJSON`/`UnmarshalJSON` 로 `models.json` 호환 직렬화

- [ ] **Step 1: 행렬 테스트를 쓴다**

`internal/model/matrix_test.go`:

```go
package model

import "testing"

func TestMatrixRowRoundTrip(t *testing.T) {
	m := NewMatrix(3, 4)
	m.SetRow(1, []float64{1, 2, 3, 4})
	r := m.Row(1)
	if len(r) != 4 {
		t.Fatalf("행 길이 %d, 기대 4", len(r))
	}
	for i, want := range []float32{1, 2, 3, 4} {
		if r[i] != want {
			t.Errorf("Row(1)[%d] = %v, 기대 %v", i, r[i], want)
		}
	}
	// 다른 행은 건드리지 않는다
	for _, v := range m.Row(0) {
		if v != 0 {
			t.Errorf("Row(0) 가 오염됐다: %v", m.Row(0))
			break
		}
	}
}

func TestMatrixTruncate(t *testing.T) {
	m := NewMatrix(10, 3)
	m.SetRow(0, []float64{7, 8, 9})
	m.Truncate(4)
	if m.Rows != 4 {
		t.Fatalf("Rows = %d, 기대 4", m.Rows)
	}
	if len(m.Data) != 12 {
		t.Errorf("Data 길이 %d, 기대 12", len(m.Data))
	}
	if m.Row(0)[0] != 7 {
		t.Error("Truncate 가 남은 데이터를 훼손했다")
	}
}
```

- [ ] **Step 2: 실패 확인 후 행렬 구현**

Run: `go test ./internal/model/ -v` → FAIL

`internal/model/matrix.go`:

```go
package model

// Matrix 는 행 우선 평면 float32 행렬이다.
// 100만 행 × 60 피처 규모에서 [][]float64 는 메모리와 GC 압력이 과하다.
type Matrix struct {
	Rows int
	Cols int
	Data []float32
}

func NewMatrix(rows, cols int) *Matrix {
	return &Matrix{Rows: rows, Cols: cols, Data: make([]float32, rows*cols)}
}

func (m *Matrix) Row(i int) []float32 { return m.Data[i*m.Cols : (i+1)*m.Cols] }

func (m *Matrix) SetRow(i int, v []float64) {
	dst := m.Row(i)
	for j := range dst {
		dst[j] = float32(v[j])
	}
}

// Truncate 는 앞 n행만 남긴다. 표본을 미리 크게 잡고 실제 개수만큼 줄일 때 쓴다.
func (m *Matrix) Truncate(n int) {
	m.Rows = n
	m.Data = m.Data[:n*m.Cols]
}
```

- [ ] **Step 3: 로지스틱 회귀 테스트를 쓴다**

`internal/model/logreg_test.go`:

```go
package model

import (
	"encoding/json"
	"math"
	"math/rand"
	"testing"
)

func TestSigmoidIsStableAtExtremes(t *testing.T) {
	if got := Sigmoid(0); math.Abs(got-0.5) > 1e-15 {
		t.Errorf("Sigmoid(0) = %v", got)
	}
	if got := Sigmoid(1000); got != 1 {
		t.Errorf("Sigmoid(1000) = %v, 기대 1", got)
	}
	if got := Sigmoid(-1000); got != 0 {
		t.Errorf("Sigmoid(-1000) = %v, 기대 0", got)
	}
	if math.IsNaN(Sigmoid(-745)) || math.IsNaN(Sigmoid(745)) {
		t.Error("극단값에서 NaN 이 나온다")
	}
}

// 선형 분리 가능한 인공 데이터에서 계수 부호가 맞아야 한다.
func TestFitRecoversSignal(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	n, p := 4000, 3
	m := NewMatrix(n, p)
	y := make([]float64, n)
	rows := make([]int, n)
	for i := 0; i < n; i++ {
		x0 := r.NormFloat64()
		x1 := r.NormFloat64()
		x2 := r.NormFloat64() // 무관한 피처
		m.SetRow(i, []float64{x0, x1, x2})
		z := 1.5*x0 - 1.0*x1
		if r.Float64() < Sigmoid(z) {
			y[i] = 1
		}
		rows[i] = i
	}
	lr, err := Fit(m, rows, y, []string{"a", "b", "c"}, 1.0)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if lr.Coef[0] <= 0 {
		t.Errorf("a 계수 = %v, 양수여야 한다", lr.Coef[0])
	}
	if lr.Coef[1] >= 0 {
		t.Errorf("b 계수 = %v, 음수여야 한다", lr.Coef[1])
	}
	if math.Abs(lr.Coef[2]) > math.Abs(lr.Coef[0]) {
		t.Errorf("무관한 피처 계수가 너무 크다: %v", lr.Coef[2])
	}
	if lr.NTrain != n {
		t.Errorf("NTrain = %d, 기대 %d", lr.NTrain, n)
	}
}

// L2 를 키우면 계수가 0 쪽으로 수축해야 한다.
func TestStrongerL2ShrinksCoefficients(t *testing.T) {
	r := rand.New(rand.NewSource(8))
	n := 2000
	m := NewMatrix(n, 2)
	y := make([]float64, n)
	rows := make([]int, n)
	for i := 0; i < n; i++ {
		x0, x1 := r.NormFloat64(), r.NormFloat64()
		m.SetRow(i, []float64{x0, x1})
		if r.Float64() < Sigmoid(2*x0) {
			y[i] = 1
		}
		rows[i] = i
	}
	weak, _ := Fit(m, rows, y, []string{"a", "b"}, 0.1)
	strong, _ := Fit(m, rows, y, []string{"a", "b"}, 1000.0)
	if math.Abs(strong.Coef[0]) >= math.Abs(weak.Coef[0]) {
		t.Errorf("L2 를 키웠는데 수축하지 않았다: %v vs %v", strong.Coef[0], weak.Coef[0])
	}
}

// 표준화 통계는 학습 구간에서만 나와야 한다 (테스트 통계 누수 방지).
func TestStandardisationUsesTrainingRowsOnly(t *testing.T) {
	m := NewMatrix(4, 1)
	m.SetRow(0, []float64{0})
	m.SetRow(1, []float64{2})
	m.SetRow(2, []float64{1000}) // 학습에 안 쓰는 행
	m.SetRow(3, []float64{1000})
	y := []float64{0, 1, 1, 1}
	lr, err := Fit(m, []int{0, 1}, y, []string{"a"}, 1.0)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if math.Abs(lr.Mu[0]-1.0) > 1e-9 {
		t.Errorf("Mu = %v, 학습 행 {0,2} 의 평균 1 이어야 한다", lr.Mu[0])
	}
}

// 표준편차가 0 인 피처는 1 로 대체해 0 나눗셈을 막는다.
func TestZeroVarianceFeatureGetsUnitSd(t *testing.T) {
	m := NewMatrix(3, 2)
	for i := 0; i < 3; i++ {
		m.SetRow(i, []float64{5, float64(i)})
	}
	lr, err := Fit(m, []int{0, 1, 2}, []float64{0, 1, 1}, []string{"const", "x"}, 1.0)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if lr.Sd[0] != 1.0 {
		t.Errorf("분산 0 피처의 Sd = %v, 기대 1", lr.Sd[0])
	}
	if math.IsNaN(lr.Prob(m.Row(0))) {
		t.Error("Prob 가 NaN 이다")
	}
}

func TestJSONRoundTripMatchesPythonSchema(t *testing.T) {
	lr := &LogReg{
		L2: 10, Coef: []float64{0.1, -0.2}, Intercept: 0.05,
		Mu: []float64{1, 2}, Sd: []float64{3, 4},
		NTrain: 123, FeatureNames: []string{"a", "b"},
	}
	b, err := json.Marshal(lr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Python models.json 의 키와 같아야 한다
	var raw map[string]any
	json.Unmarshal(b, &raw)
	for _, k := range []string{"l2", "coef", "intercept", "mu", "sd", "n_train", "feature_names"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("키 %q 가 없다 — Python models.json 과 호환되지 않는다", k)
		}
	}
	var back LogReg
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Intercept != lr.Intercept || back.NTrain != lr.NTrain || back.Coef[1] != lr.Coef[1] {
		t.Errorf("왕복 불일치: %+v", back)
	}
}

// Python models.json 을 그대로 읽어 같은 확률을 내는지 확인한다.
func TestLoadsPythonModelsJSON(t *testing.T) {
	const sample = `{"l2":10.0,"coef":[0.5,-0.25],"intercept":0.1,
	  "mu":[0.0,0.0],"sd":[1.0,1.0],"n_train":100,"feature_names":["a","b"]}`
	var lr LogReg
	if err := json.Unmarshal([]byte(sample), &lr); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got := lr.Logit([]float32{2, 4})
	want := 0.5*2 - 0.25*4 + 0.1 // = 0.1
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("Logit = %v, 기대 %v", got, want)
	}
}
```

- [ ] **Step 4: 테스트 실패 확인**

Run: `go test ./internal/model/ -run 'TestSigmoid|TestFit|TestStronger|TestStandard|TestZero|TestJSON|TestLoads' -v`
Expected: FAIL — `undefined: Sigmoid`

- [ ] **Step 5: 구현**

`internal/model/logreg.go`:

```go
// Package model 은 L2 정규화 로지스틱 회귀다. 학습 후 계수는 동결된다.
//
// 미래참조 차단의 핵심: Fit 은 테스트 구간이 시작되기 전 데이터로만 호출된다.
// 테스트 중에는 재학습도 재표준화도 하지 않는다. 표준화 평균·표준편차도
// 학습 구간에서만 계산한다 (테스트 통계 누수 방지).
package model

import (
	"fmt"
	"math"

	"gonum.org/v1/gonum/optimize"
)

type LogReg struct {
	L2           float64   `json:"l2"`
	Coef         []float64 `json:"coef"`
	Intercept    float64   `json:"intercept"`
	Mu           []float64 `json:"mu"`
	Sd           []float64 `json:"sd"`
	NTrain       int       `json:"n_train"`
	FeatureNames []string  `json:"feature_names"`
}

// Sigmoid 는 극단값에서도 NaN 을 내지 않는다.
func Sigmoid(z float64) float64 {
	if z >= 0 {
		return 1.0 / (1.0 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1.0 + e)
}

// Logit 은 표준화 후 선형결합이다.
func (m *LogReg) Logit(x []float32) float64 {
	z := m.Intercept
	for i, c := range m.Coef {
		z += (float64(x[i]) - m.Mu[i]) / m.Sd[i] * c
	}
	return z
}

func (m *LogReg) Prob(x []float32) float64 { return Sigmoid(m.Logit(x)) }

// Fit 은 rows 로 지정한 행만 써서 학습한다.
func Fit(mat *Matrix, rows []int, y []float64, names []string, l2 float64) (*LogReg, error) {
	n, p := len(rows), mat.Cols
	if n == 0 {
		return nil, fmt.Errorf("학습 표본이 없습니다")
	}

	mu := make([]float64, p)
	for _, r := range rows {
		row := mat.Row(r)
		for j := 0; j < p; j++ {
			mu[j] += float64(row[j])
		}
	}
	for j := range mu {
		mu[j] /= float64(n)
	}
	sd := make([]float64, p)
	for _, r := range rows {
		row := mat.Row(r)
		for j := 0; j < p; j++ {
			d := float64(row[j]) - mu[j]
			sd[j] += d * d
		}
	}
	for j := range sd {
		// Python 은 np.std 기본값(ddof=0)을 쓴다. 여기서도 같게 맞춘다.
		sd[j] = math.Sqrt(sd[j] / float64(n))
		if sd[j] < 1e-12 {
			sd[j] = 1.0
		}
	}

	// 표준화된 설계행렬을 미리 만든다 (반복 계산 회피)
	z := make([]float64, n*p)
	yy := make([]float64, n)
	for i, r := range rows {
		row := mat.Row(r)
		for j := 0; j < p; j++ {
			z[i*p+j] = (float64(row[j]) - mu[j]) / sd[j]
		}
		yy[i] = y[r]
	}

	// w = [계수 p개, 절편]. 절편은 정규화에서 제외한다.
	objective := func(w []float64) float64 {
		var ll float64
		for i := 0; i < n; i++ {
			s := w[p]
			zi := z[i*p : (i+1)*p]
			for j := 0; j < p; j++ {
				s += zi[j] * w[j]
			}
			// 수치 안정 log-loss: logaddexp(0, s) − y*s
			ll += logAddExp0(s) - yy[i]*s
		}
		var reg float64
		for j := 0; j < p; j++ {
			reg += w[j] * w[j]
		}
		return ll + 0.5*l2*reg
	}

	gradient := func(grad, w []float64) {
		for j := range grad {
			grad[j] = 0
		}
		for i := 0; i < n; i++ {
			s := w[p]
			zi := z[i*p : (i+1)*p]
			for j := 0; j < p; j++ {
				s += zi[j] * w[j]
			}
			r := Sigmoid(s) - yy[i]
			for j := 0; j < p; j++ {
				grad[j] += zi[j] * r
			}
			grad[p] += r
		}
		for j := 0; j < p; j++ {
			grad[j] += l2 * w[j]
		}
	}

	problem := optimize.Problem{Func: objective, Grad: gradient}
	// 목적함수가 L2 때문에 강볼록이라 최적점이 유일하다. 수렴만 충분히 조이면
	// scipy L-BFGS-B 와 같은 점에 도달한다 — Task 12 재현의 근거다.
	settings := &optimize.Settings{
		GradientThreshold: 1e-8,
		MajorIterations:   2000,
		Converger:         &optimize.FunctionConverge{Absolute: 1e-12, Iterations: 50},
	}
	res, err := optimize.Minimize(problem, make([]float64, p+1), settings, &optimize.LBFGS{})
	if err != nil && res == nil {
		return nil, fmt.Errorf("최적화 실패: %w", err)
	}
	if err = res.Status.Err(); err != nil {
		return nil, fmt.Errorf("최적화 상태: %w", err)
	}

	coef := make([]float64, p)
	copy(coef, res.X[:p])
	return &LogReg{
		L2: l2, Coef: coef, Intercept: res.X[p],
		Mu: mu, Sd: sd, NTrain: n, FeatureNames: append([]string(nil), names...),
	}, nil
}

// logAddExp0 은 log(1 + exp(s)) 를 오버플로 없이 계산한다.
func logAddExp0(s float64) float64 {
	if s > 0 {
		return s + math.Log1p(math.Exp(-s))
	}
	return math.Log1p(math.Exp(s))
}
```

- [ ] **Step 6: 의존성을 받고 테스트 통과 확인**

```bash
GOTOOLCHAIN=local go get gonum.org/v1/gonum@v0.15.1
GOTOOLCHAIN=local go mod tidy
go test ./internal/model/ -v
```

Expected: PASS (8개 테스트)

- [ ] **Step 7: 커밋**

```bash
git add go.mod go.sum internal/model/
git commit -m "L2 로지스틱 회귀 — gonum LBFGS 학습, models.json 호환 직렬화"
```

---

### Task 10: 평가 지표

Python `btc5m/metrics.py` 의 필요한 부분만 옮긴다. **주의: 이항검정 p값은 정규근사로
구현하고 이름에 그 사실을 드러낸다.** scipy 의 정확 이항검정을 재현하는 것은 별개
작업이고, G1' 게이트에 p값은 들어가지 않는다 (n 이 수십만이라 근사가 사실상 정확하다).

`calibration_table` 은 `np.array_split` 의 분할 규칙을 그대로 따라야 한다 — 앞
`n % bins` 개 묶음이 하나씩 더 크다.

**Files:**
- Create: `internal/metrics/metrics.go`, `internal/metrics/metrics_test.go`

**Interfaces:**
- Produces: `metrics.AUC(y, p []float64) float64`, `metrics.CalibrationTable(y, p []float64, bins int) []Bin`, `metrics.ECE(y, p []float64, bins int) float64`, `metrics.Bin{N int, PredLow, PredHigh, MeanPred, Observed, Gap float64}`, `metrics.BinomTestNormal(successes, n int) float64`, `metrics.ArraySplit(n, bins int) [][2]int`

- [ ] **Step 1: 실패 테스트를 쓴다**

`internal/metrics/metrics_test.go`:

```go
package metrics

import (
	"math"
	"testing"
)

func TestArraySplitMatchesNumpy(t *testing.T) {
	// np.array_split(range(10), 3) → 크기 4, 3, 3
	got := ArraySplit(10, 3)
	want := [][2]int{{0, 4}, {4, 7}, {7, 10}}
	if len(got) != len(want) {
		t.Fatalf("묶음 %d개, 기대 %d개", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%d번째 %v, 기대 %v", i, got[i], want[i])
		}
	}
	// 나누어떨어지는 경우
	if g := ArraySplit(9, 3); g[0] != [2]int{0, 3} || g[2] != [2]int{6, 9} {
		t.Errorf("9/3 분할 오류: %v", g)
	}
	// bins 가 n 보다 클 때
	if g := ArraySplit(2, 5); len(g) != 5 {
		t.Errorf("bins > n 일 때 %d개", len(g))
	}
}

func TestAUCPerfectAndRandom(t *testing.T) {
	y := []float64{0, 0, 1, 1}
	if got := AUC(y, []float64{0.1, 0.2, 0.8, 0.9}); math.Abs(got-1.0) > 1e-12 {
		t.Errorf("완전 분리 AUC = %v, 기대 1", got)
	}
	if got := AUC(y, []float64{0.9, 0.8, 0.2, 0.1}); math.Abs(got-0.0) > 1e-12 {
		t.Errorf("완전 역분리 AUC = %v, 기대 0", got)
	}
	// 전부 동점이면 0.5 (동점을 평균 순위로 처리)
	if got := AUC(y, []float64{0.5, 0.5, 0.5, 0.5}); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("전부 동점 AUC = %v, 기대 0.5", got)
	}
}

func TestAUCHandlesTiesLikeMannWhitney(t *testing.T) {
	y := []float64{0, 1, 0, 1}
	p := []float64{0.3, 0.3, 0.7, 0.7}
	if got := AUC(y, p); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("AUC = %v, 기대 0.5", got)
	}
}

func TestAUCSingleClassIsNaN(t *testing.T) {
	if got := AUC([]float64{1, 1}, []float64{0.4, 0.6}); !math.IsNaN(got) {
		t.Errorf("한 클래스뿐인데 %v 를 돌려줬다", got)
	}
}

func TestECEIsZeroWhenPerfectlyCalibrated(t *testing.T) {
	// 각 묶음에서 말한 확률과 실제 빈도가 같으면 ECE 는 0 이다
	var y, p []float64
	for i := 0; i < 100; i++ {
		p = append(p, 0.5)
		y = append(y, float64(i%2))
	}
	if got := ECE(y, p, 10); got > 1e-12 {
		t.Errorf("ECE = %v, 기대 0", got)
	}
}

func TestECEDetectsOverconfidence(t *testing.T) {
	var y, p []float64
	for i := 0; i < 100; i++ {
		p = append(p, 0.9) // 90% 라고 말하는데
		y = append(y, 0)   // 실제로는 전부 틀린다
	}
	if got := ECE(y, p, 10); math.Abs(got-0.9) > 1e-9 {
		t.Errorf("ECE = %v, 기대 0.9", got)
	}
}

func TestBinomTestNormalBounds(t *testing.T) {
	if got := BinomTestNormal(500, 1000); got < 0.9 {
		t.Errorf("정확히 반이면 p 가 1 에 가까워야 한다: %v", got)
	}
	if got := BinomTestNormal(600, 1000); got > 1e-8 {
		t.Errorf("60%% 면 p 가 아주 작아야 한다: %v", got)
	}
	if got := BinomTestNormal(0, 0); !math.IsNaN(got) {
		t.Errorf("n=0 인데 %v", got)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/metrics/ -v`
Expected: FAIL — `undefined: ArraySplit`

- [ ] **Step 3: 구현**

`internal/metrics/metrics.go`:

```go
// Package metrics 는 정확도 중심 평가 지표를 계산한다.
package metrics

import (
	"math"
	"sort"
)

// ArraySplit 은 numpy.array_split 의 분할 규칙을 따른다.
// 앞 n%bins 개 묶음이 하나씩 더 크다. 반환값은 [시작, 끝) 쌍이다.
func ArraySplit(n, bins int) [][2]int {
	if bins <= 0 {
		return nil
	}
	base, extra := n/bins, n%bins
	out := make([][2]int, 0, bins)
	start := 0
	for i := 0; i < bins; i++ {
		size := base
		if i < extra {
			size++
		}
		out = append(out, [2]int{start, start + size})
		start += size
	}
	return out
}

// AUC 는 Mann-Whitney U 기반 ROC AUC 다. 동점은 평균 순위로 처리한다.
func AUC(y, p []float64) float64 {
	var pos, neg int
	for _, v := range y {
		if v == 1 {
			pos++
		} else {
			neg++
		}
	}
	if pos == 0 || neg == 0 {
		return math.NaN()
	}
	order := make([]int, len(p))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return p[order[a]] < p[order[b]] })

	ranks := make([]float64, len(p))
	for i := 0; i < len(order); {
		j := i
		for j+1 < len(order) && p[order[j+1]] == p[order[i]] {
			j++
		}
		avg := float64(i+j)/2.0 + 1.0
		for k := i; k <= j; k++ {
			ranks[order[k]] = avg
		}
		i = j + 1
	}
	var sum float64
	for i, v := range y {
		if v == 1 {
			sum += ranks[i]
		}
	}
	fp, fn := float64(pos), float64(neg)
	return (sum - fp*(fp+1)/2.0) / (fp * fn)
}

// Bin 은 교정표의 한 구간이다.
type Bin struct {
	N        int
	PredLow  float64
	PredHigh float64
	MeanPred float64
	Observed float64
	Gap      float64
}

// CalibrationTable 은 예측 확률을 분위로 나눠 말한 확률과 실제 빈도를 비교한다.
// 확률이 0.5 근처에 몰려 있으므로 등간격이 아니라 분위 구간을 쓴다.
func CalibrationTable(y, p []float64, bins int) []Bin {
	n := len(y)
	if n == 0 {
		return nil
	}
	if bins > n {
		bins = n
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return p[order[a]] < p[order[b]] })

	var out []Bin
	for _, seg := range ArraySplit(n, bins) {
		idx := order[seg[0]:seg[1]]
		if len(idx) == 0 {
			continue
		}
		lo, hi := p[idx[0]], p[idx[0]]
		var sp, sy float64
		for _, i := range idx {
			if p[i] < lo {
				lo = p[i]
			}
			if p[i] > hi {
				hi = p[i]
			}
			sp += p[i]
			sy += y[i]
		}
		m := float64(len(idx))
		mp, ob := sp/m, sy/m
		out = append(out, Bin{
			N: len(idx), PredLow: lo, PredHigh: hi,
			MeanPred: mp, Observed: ob, Gap: ob - mp,
		})
	}
	return out
}

// ECE 는 구간별 |말한 확률 − 실제 빈도| 를 표본 수로 가중 평균한다.
func ECE(y, p []float64, bins int) float64 {
	rows := CalibrationTable(y, p, bins)
	if len(rows) == 0 || len(y) == 0 {
		return math.NaN()
	}
	var s float64
	for _, r := range rows {
		s += float64(r.N) * math.Abs(r.Gap)
	}
	return s / float64(len(y))
}

// BinomTestNormal 은 p=0.5 귀무가설의 양측 검정 p값을 정규근사로 낸다.
// scipy 의 정확 이항검정이 아니다 — n 이 크면 사실상 같고, G1' 게이트에는
// p값이 들어가지 않는다. 이름으로 근사임을 드러낸다.
func BinomTestNormal(successes, n int) float64 {
	if n <= 0 {
		return math.NaN()
	}
	mu := float64(n) * 0.5
	sd := math.Sqrt(float64(n) * 0.25)
	d := math.Abs(float64(successes)-mu) - 0.5 // 연속성 보정
	if d < 0 {
		d = 0
	}
	return math.Erfc(d / (sd * math.Sqrt2))
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/metrics/ -v`
Expected: PASS (7개 테스트)

- [ ] **Step 5: 커밋**

```bash
git add internal/metrics/
git commit -m "평가 지표 — AUC·교정표·ECE, numpy array_split 규칙 준수"
```

---

### Task 11: 워크포워드

블록마다 재학습하되 **블록 시작 전에 정답이 확정된 표본만** 학습에 쓴다. 이 조건을
놓치면 블록 경계마다 미래참조가 샌다. Python 에서 실제로 off-by-one 을 잡았던 곳이다.

**Files:**
- Create: `internal/walkforward/walkforward.go`, `internal/walkforward/walkforward_test.go`

**Interfaces:**
- Consumes: `model.Matrix`, `model.Fit`
- Produces: `walkforward.TrainableBefore(candleStarts []int64, cutoff, window int64) []int`, `walkforward.Run(cs []int64, m *model.Matrix, y []float64, names []string, testStart int64, refitDays, trainDays float64, l2 float64, log func(string, ...any)) ([]float64, int, error)`
- Produces: 상수 `walkforward.DayMS = 86_400_000`, `walkforward.FiveMinMS = 300_000`

- [ ] **Step 1: 경계 테스트를 쓴다**

`internal/walkforward/walkforward_test.go`:

```go
package walkforward

import "testing"

func TestTrainableBeforeExcludesUnresolvedLabels(t *testing.T) {
	// 5분봉이 5분마다 하나씩. cutoff 시점에 정답이 확정된 것만 학습 가능하다.
	var cs []int64
	for i := 0; i < 2000; i++ {
		cs = append(cs, int64(i)*FiveMinMS)
	}
	cutoff := int64(1000) * FiveMinMS
	got := TrainableBefore(cs, cutoff, 100*DayMS)

	inSet := map[int]bool{}
	for _, i := range got {
		inSet[i] = true
	}
	// cs[999] 는 999*5분 에 열려 1000*5분 에 닫힌다 → 정확히 cutoff 에 확정. 포함.
	if !inSet[999] {
		t.Error("cs[999] 는 cutoff 에 정답이 확정되므로 학습 가능해야 한다")
	}
	// cs[1000] 은 cutoff 이후에 닫힌다 → 제외.
	if inSet[1000] {
		t.Error("cs[1000] 은 정답이 확정되지 않았는데 학습에 포함됐다")
	}
	if inSet[1500] {
		t.Error("미래 표본이 학습에 포함됐다")
	}
}

func TestTrainableBeforeRespectsWindow(t *testing.T) {
	var cs []int64
	for i := 0; i < 2000; i++ {
		cs = append(cs, int64(i)*FiveMinMS)
	}
	cutoff := int64(1000) * FiveMinMS
	window := int64(10) * FiveMinMS
	got := TrainableBefore(cs, cutoff, window)
	for _, i := range got {
		if cs[i] < cutoff-window {
			t.Fatalf("창 밖의 표본 %d (cs=%d, 하한=%d)", i, cs[i], cutoff-window)
		}
	}
	if len(got) != 10 {
		t.Errorf("표본 %d개, 기대 10개", len(got))
	}
}

func TestTrainableBeforeEmptyWhenNothingResolved(t *testing.T) {
	cs := []int64{100 * FiveMinMS, 101 * FiveMinMS}
	if got := TrainableBefore(cs, 0, DayMS); len(got) != 0 {
		t.Errorf("cutoff 가 0 인데 %d개를 돌려줬다", len(got))
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/walkforward/ -v`
Expected: FAIL — `undefined: TrainableBefore`

- [ ] **Step 3: 구현**

`internal/walkforward/walkforward.go`:

```go
// Package walkforward 는 블록마다 재학습하며 앞으로 나아가는 검증을 돌린다.
//
// 핵심 규칙: 블록 시작 시점에 이미 정답이 확정된 표본만 학습에 쓴다.
// 5분봉의 정답은 candleStart + 5분 에 확정되므로 그 시각이 블록 시작 이하여야 한다.
// 이 조건을 놓치면 블록 경계마다 미래참조가 샌다.
package walkforward

import (
	"fmt"
	"math"

	"github.com/kdm000718/GLD-9.1/internal/model"
)

const (
	DayMS     = 86_400_000
	FiveMinMS = 300_000
)

// TrainableBefore 는 cutoff 시점에 학습 가능한 표본의 인덱스를 돌려준다.
// window 는 학습에 쓸 과거 구간 길이다.
func TrainableBefore(candleStarts []int64, cutoff, window int64) []int {
	var out []int
	for i, cs := range candleStarts {
		if cs >= cutoff-window && cs+FiveMinMS <= cutoff {
			out = append(out, i)
		}
	}
	return out
}

// Run 은 워크포워드를 돌리고 각 표본의 예측 확률을 돌려준다.
// 평가되지 않은 표본은 NaN 이다. 두 번째 반환값은 재학습 횟수다.
func Run(cs []int64, m *model.Matrix, y []float64, names []string,
	testStart int64, refitDays, trainDays, l2 float64,
	log func(string, ...any)) ([]float64, int, error) {

	if log == nil {
		log = func(string, ...any) {}
	}
	prob := make([]float64, len(y))
	for i := range prob {
		prob[i] = math.NaN()
	}
	step := int64(refitDays * DayMS)
	window := int64(trainDays * DayMS)
	end := cs[len(cs)-1] + FiveMinMS
	nFit := 0

	for start := testStart; start < end; start += step {
		stop := start + step
		if stop > end {
			stop = end
		}
		train := TrainableBefore(cs, start, window)
		var test []int
		for i, c := range cs {
			if c >= start && c < stop {
				test = append(test, i)
			}
		}
		if len(train) < 5000 || len(test) == 0 {
			continue
		}
		lr, err := model.Fit(m, train, y, names, l2)
		if err != nil {
			return nil, nFit, fmt.Errorf("블록 %d 학습 실패: %w", nFit, err)
		}
		for _, i := range test {
			prob[i] = lr.Prob(m.Row(i))
		}
		nFit++
		if nFit%12 == 0 {
			log("    ... 재학습 %d회", nFit)
		}
	}
	log("    총 재학습 %d회", nFit)
	return prob, nFit, nil
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/walkforward/ -v && go vet ./...`
Expected: PASS (3개 테스트)

- [ ] **Step 5: 커밋**

```bash
git add internal/walkforward/
git commit -m "워크포워드 — 정답 확정 시각 기준 학습 표본 선별"
```

---

### Task 12: G1' — 전 구간 재현

Go 파이프라인 전체를 2017-08-17~ 전 구간에 돌려 Python 이 낸 숫자를 재현한다.
**이것이 P0~P1 의 최종 판정이다.**

Python 참조값 (`out_full/summary_full.json`):
정확도 **52.773%**, AUC **0.5408**, ECE **0.0080**, n **888,525**, 재학습 **104회**.

### 먼저: 입력 구간을 Python 실행과 맞춰야 한다

`vision.LoadFullHistory` 는 **실행일 기준 "어제까지"** 를 받는다. Python 참조 실행은
2026-08-06 까지였으므로, 오늘 Go 를 돌리면 데이터가 더 많아 표본 수가 반드시 어긋난다.
실측으로 확인된 차이다 — Go 가 받은 5분봉 942,313 봉 vs Python 942,025 봉, 차이 288 =
정확히 하루치.

**n 의 완전 일치는 입력이 같을 때만 성립하는 기준이다.** 이 절단을 빠뜨리면 게이트가
반드시 실패하고, 그 실패를 "포팅 오류" 로 오독하게 된다. `cmd/backtest` 의 `-end`
플래그가 그 역할을 하며 기본값은 Python 참조 실행의 마지막 5분봉 시작 시각이다.

`run` 안에서 `b1`·`b5` 를 로드한 직후, 아래 truncate 를 적용한 뒤 `buildMatrix` 를
호출한다. `sort` 를 import 에 추가한다.

```go
// truncateTo 는 open_time 이 endMS 를 넘는 봉을 잘라낸다.
func truncateTo(b bars.Bars, endMS int64) bars.Bars {
	hi := sort.Search(b.Len(), func(i int) bool { return b.OpenTime[i] > endMS })
	return b.Slice(0, hi)
}
```

```go
	end, err := time.Parse(time.RFC3339, endFlag)
	if err != nil {
		return fmt.Errorf("-end 파싱 실패: %w", err)
	}
	endMS := end.UnixMilli()
	// 경계는 타임프레임마다 다르다. -end 는 마지막 5분봉의 '시작' 시각이므로
	// 5분봉은 그대로 자르고, 1분봉은 그 봉을 구성하는 마지막 분(+4분)까지 남긴다.
	// 같은 경계를 두 곳에 쓰면 1분봉이 정확히 4개 모자란다 (실측 확인).
	b1 = truncateTo(b1, endMS+4*60_000)
	b5 = truncateTo(b5, endMS)
	fmt.Printf("  절단 후 1분봉 %d / 5분봉 %d  (기준 %s)\n", b1.Len(), b5.Len(), endFlag)
	if b1.Len() != 4_710_079 || b5.Len() != 942_025 {
		return fmt.Errorf("절단 후 봉 수가 Python 참조와 다르다: 1분봉 %d(기대 4710079) / 5분봉 %d(기대 942025)",
			b1.Len(), b5.Len())
	}
```

절단 후 봉 수가 Python 참조(1분봉 4,710,079 / 5분봉 942,025)와 맞는지 먼저 확인하고,
어긋나면 표본 생성으로 넘어가기 전에 멈춘다. 여기서 안 맞으면 뒤의 어떤 수치도 비교할
의미가 없다.

**이 절단은 이미 실측 검증했다.** 받아둔 캐시에 위 경계를 적용하면 1분봉 4,710,079 /
5분봉 942,025 로 Python 참조와 정확히 일치한다. 두 타임프레임에 같은 경계를 쓰면
1분봉이 23:56~23:59 네 개만큼 모자라므로, `+4*60_000` 을 빼먹지 말 것.

**Files:**
- Create: `cmd/backtest/main.go`
- Create: `docs/results/g1prime-fullhistory.log` (실행 결과)

**Interfaces:**
- Consumes: 앞의 모든 패키지

- [ ] **Step 1: 러너를 쓴다**

`cmd/backtest/main.go`:

```go
// Command backtest 는 전 구간 워크포워드를 돌려 Python 결과를 재현한다 (게이트 G1').
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/clock"
	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/metrics"
	"github.com/kdm000718/GLD-9.1/internal/model"
	"github.com/kdm000718/GLD-9.1/internal/vision"
	"github.com/kdm000718/GLD-9.1/internal/walkforward"
)

// 결측봉 방어: 예측에 실제로 쓰이는 최근 구간이 시간상 연속이어야 한다.
const (
	req1m  = 60
	req5m  = 12
	minMS  = 60_000
	fiveMS = walkforward.FiveMinMS
)

func main() {
	symbol := flag.String("symbol", "BTCUSDT", "심볼")
	cache := flag.String("cache", "data", "Vision 캐시 디렉터리")
	trainDays := flag.Float64("train-days", 180, "학습 창 (일)")
	refitDays := flag.Float64("refit-days", 30, "재학습 주기 (일)")
	l2 := flag.Float64("l2", 10, "L2 세기")
	// Python 참조 실행과 입력을 맞추기 위한 절단 기준. 이것 없이는 실행일에 따라
	// 데이터가 늘어나 표본 수가 어긋난다.
	endFlag := flag.String("end", "2026-08-06T23:55:00Z", "마지막 5분봉 시작 시각 (RFC3339)")
	flag.Parse()

	if err := run(*symbol, *cache, *trainDays, *refitDays, *l2, *endFlag); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(1)
	}
}

func run(symbol, cache string, trainDays, refitDays, l2 float64, endFlag string) error {
	ctx := context.Background()
	t0 := time.Now()

	fmt.Println("[데이터] Binance Vision 수집 (SHA256 검증)")
	b1, err := vision.LoadFullHistory(ctx, symbol, "1m", cache, logf)
	if err != nil {
		return err
	}
	b5, err := vision.LoadFullHistory(ctx, symbol, "5m", cache, logf)
	if err != nil {
		return err
	}
	fmt.Printf("  1분봉 %d개  %s ~ %s\n", b1.Len(), iso(b1.OpenTime[0]), iso(b1.OpenTime[b1.Len()-1]))
	fmt.Printf("  5분봉 %d개  %s ~ %s  (%.0fs)\n", b5.Len(),
		iso(b5.OpenTime[0]), iso(b5.OpenTime[b5.Len()-1]), time.Since(t0).Seconds())

	fmt.Println("\n[표본] +0분 피처 생성")
	cs, mat, y, err := buildMatrix(b1, b5)
	if err != nil {
		return err
	}

	testStart := cs[0] + int64(trainDays*walkforward.DayMS)
	fmt.Printf("\n[워크포워드] %g일마다 직전 %g일로 재학습, 평가 시작 %s\n",
		refitDays, trainDays, iso(testStart))
	prob, nFit, err := walkforward.Run(cs, mat, y, features.FeatureNames,
		testStart, refitDays, trainDays, l2, logf)
	if err != nil {
		return err
	}

	// 평가된 표본만 남긴다
	var ey, ep []float64
	for i := range prob {
		if !math.IsNaN(prob[i]) {
			ey = append(ey, y[i])
			ep = append(ep, prob[i])
		}
	}
	if len(ey) == 0 {
		return fmt.Errorf("평가된 표본이 없습니다")
	}
	correct := 0
	for i := range ey {
		pred := 0.0
		if ep[i] >= 0.5 {
			pred = 1.0
		}
		if pred == ey[i] {
			correct++
		}
	}
	acc := float64(correct) / float64(len(ey))
	auc := metrics.AUC(ey, ep)
	ece := metrics.ECE(ey, ep, 10)

	fmt.Println("\n========================================================")
	fmt.Printf("전체 결과  n=%d\n", len(ey))
	fmt.Println("========================================================")
	fmt.Printf("  정확도  : %.3f%%   (p=%.4g)\n", acc*100, metrics.BinomTestNormal(correct, len(ey)))
	fmt.Printf("  AUC     : %.4f\n", auc)
	fmt.Printf("  ECE     : %.4f\n", ece)
	fmt.Printf("  재학습  : %d회\n", nFit)
	fmt.Printf("  소요    : %.0f초\n", time.Since(t0).Seconds())

	// ---- G1' 판정 ----
	const (
		wantAcc  = 0.52773
		wantAUC  = 0.5408
		wantN    = 888_525
		wantFit  = 104
		tolAcc   = 0.0001 // ±0.01%p
		tolAUC   = 0.0005
	)
	fmt.Println("\n==================== G1' Python 재현 ====================")
	pass := true
	check := func(name string, got, want, tol float64) {
		ok := math.Abs(got-want) <= tol
		mark := "통과"
		if !ok {
			mark, pass = "실패", false
		}
		fmt.Printf("  %-10s go=%.5f  py=%.5f  차이 %+.5f  (허용 %.5f)  %s\n",
			name, got, want, got-want, tol, mark)
	}
	check("정확도", acc, wantAcc, tolAcc)
	check("AUC", auc, wantAUC, tolAUC)
	if len(ey) != wantN {
		fmt.Printf("  %-10s go=%d  py=%d  ■ 완전 일치 필요 — 절단·연속성 로직이 다르다\n",
			"표본 수", len(ey), wantN)
		pass = false
	} else {
		fmt.Printf("  %-10s go=%d  py=%d  통과\n", "표본 수", len(ey), wantN)
	}
	if nFit != wantFit {
		fmt.Printf("  %-10s go=%d  py=%d  ■ 불일치\n", "재학습", nFit, wantFit)
		pass = false
	}
	fmt.Println()
	if !pass {
		return fmt.Errorf("G1' 실패 — 포팅이 틀렸다")
	}
	fmt.Println("  판정: 통과 — P0~P1 완료")
	return nil
}

func iso(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04 UTC") }

func logf(format string, args ...any) { fmt.Printf(format+"\n", args...) }
```

`buildMatrix` 는 다음 스텝에서 같은 파일에 이어 쓴다. import 에 `bars` 를 넣는다.

- [ ] **Step 2: `buildMatrix` 를 같은 파일에 이어 쓴다**

제외 규칙은 Python `run_full_history.py:build_matrix` 와 완전히 같아야 한다 —
도지 / 워밍업 / 결측 셋이고, 이 셋의 개수가 어긋나면 G1' 의 표본 수가 안 맞는다.

```go
// buildMatrix 는 +0분 표본을 행렬에 직접 채운다.
// 표본이 100만 개 규모라 객체를 쌓지 않고 미리 잡은 행렬에 바로 쓴다.
func buildMatrix(b1, b5 bars.Bars) ([]int64, *model.Matrix, []float64, error) {
	n := b5.Len()
	mat := model.NewMatrix(n, len(features.FeatureNames))
	cs := make([]int64, n)
	y := make([]float64, n)

	kept := 0
	skipDoji, skipWarmup, skipGap := 0, 0, 0
	t0 := time.Now()

	for i := 0; i < n; i++ {
		t := b5.OpenTime[i]
		o, c := b5.Open[i], b5.Close[i]
		if c == o {
			skipDoji++
			continue
		}
		v, err := clock.New(t, b1, b5, t)
		if err != nil {
			// t 이전에 마감된 봉이 아예 없다 (데이터 시작부)
			skipWarmup++
			continue
		}
		if v.Bars1m.Len() < req1m || v.Bars5m.Len() < req5m {
			skipWarmup++
			continue
		}
		ot1, ot5 := v.Bars1m.OpenTime, v.Bars5m.OpenTime
		l1, l5 := len(ot1), len(ot5)
		if ot1[l1-1] != t-minMS ||
			ot1[l1-1]-ot1[l1-req1m] != int64(req1m-1)*minMS ||
			ot5[l5-1]-ot5[l5-req5m] != int64(req5m-1)*fiveMS {
			skipGap++
			continue
		}
		vals, ok := features.Build(v)
		if !ok {
			skipWarmup++
			continue
		}
		mat.SetRow(kept, vals)
		cs[kept] = t
		if c > o {
			y[kept] = 1
		} else {
			y[kept] = 0
		}
		kept++
		if kept%200_000 == 0 {
			fmt.Printf("    ... %d개 (%.0fs)\n", kept, time.Since(t0).Seconds())
		}
	}
	fmt.Printf("    표본 %d개  제외: 도지 %d / 워밍업 %d / 결측 %d  (%.0fs)\n",
		kept, skipDoji, skipWarmup, skipGap, time.Since(t0).Seconds())

	mat.Truncate(kept)
	return cs[:kept], mat, y[:kept], nil
}
```

`run` 안의 호출부를 `cs, mat, y, err := buildMatrix(b1, b5)` 로 맞추고
`bars` 패키지를 import 한다.

- [ ] **Step 3: 빌드하고 실행한다**

```bash
go build ./... && go vet ./...
mkdir -p out docs/results
go run ./cmd/backtest 2>&1 | tee out/g1prime.log
```

Expected: 수십 분 소요. 마지막에 `판정: 통과`.

**실패 시 진단 순서:**
1. 표본 수가 다르다 → `buildMatrix` 의 제외 규칙(도지/워밍업/결측)을 Python 과 대조
2. 표본 수는 같은데 정확도가 다르다 → `walkforward.TrainableBefore` 의 경계
3. 둘 다 같은데 AUC 만 다르다 → `metrics.AUC` 의 동점 처리
4. 정확도가 크게 다르다 → `model.Fit` 의 수렴 설정 (`GradientThreshold` 를 더 조인다)

- [ ] **Step 4: 결과를 커밋**

```bash
cp out/g1prime.log docs/results/g1prime-fullhistory.log
git add cmd/backtest/ docs/results/g1prime-fullhistory.log
git commit -m "G1' 전 구간 재현 게이트 — Python 52.773%/AUC 0.5408/n=888,525 대조"
```

- [ ] **Step 5: 전체 테스트와 빌드 최종 확인**

```bash
go build ./... && go vet ./... && go test -race ./...
```

Expected: 전부 통과.

---

## P0~P3 완료 조건

- [ ] G2 통과 — 정산 정합 기대값 합계가 양수
- [ ] G1 통과 — 피처 2,000시점 `1e-9` 이내 일치
- [ ] G1' 통과 — 정확도·AUC·표본 수·재학습 횟수 재현
- [ ] `go test -race ./...` 전부 통과
- [ ] `go vet ./...` 무경고

세 게이트가 모두 통과하면 P4(EIP-712 서명·주문)의 계획을 새로 쓴다. 그 계획은
이 문서가 아니라 별도 문서다 — G2 결과를 보고 나서 쓰기로 스펙에 정해뒀다.
