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
