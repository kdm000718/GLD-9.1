// Package indicators 는 왼쪽만 보는 인과적 지표를 계산한다.
//
// x[i] 의 값은 오직 x[0..i] 만으로 결정된다. 워밍업이 부족한 앞부분은 NaN 이다.
// (피벗 탐지만 오른쪽 k봉을 쓰는데, 그건 확정 시점 자체가 뒤로 밀리므로
//
//	미래참조가 아니다 — internal/divergence 참고.)
package indicators

import "math"

func nan() float64 { return math.NaN() }

func filled(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}

// RecursiveSmooth 는 out[start-1]=seed, out[i]=alpha*x[i]+(1-alpha)*out[i-1] 이다.
func RecursiveSmooth(x []float64, alpha, seed float64, start int) []float64 {
	out := filled(len(x))
	if start-1 < 0 || start-1 >= len(x) {
		return out
	}
	out[start-1] = seed
	for i := start; i < len(x); i++ {
		out[i] = alpha*x[i] + (1-alpha)*out[i-1]
	}
	return out
}

// EMA 의 시드는 앞 period 개의 단순평균이다.
func EMA(x []float64, period int) []float64 {
	if len(x) < period || period <= 0 {
		return filled(len(x))
	}
	alpha := 2.0 / (float64(period) + 1.0)
	return RecursiveSmooth(x, alpha, mean(x[:period]), period)
}

// RSI 는 Wilder 방식이다.
func RSI(close []float64, period int) []float64 {
	n := len(close)
	out := filled(n)
	if n <= period || period <= 0 {
		return out
	}
	gain := make([]float64, n-1)
	loss := make([]float64, n-1)
	for i := 1; i < n; i++ {
		d := close[i] - close[i-1]
		if d > 0 {
			gain[i-1] = d
		} else if d < 0 {
			loss[i-1] = -d
		}
	}
	alpha := 1.0 / float64(period)
	ag := RecursiveSmooth(gain, alpha, mean(gain[:period]), period)
	al := RecursiveSmooth(loss, alpha, mean(loss[:period]), period)

	// delta[i] 는 close[i+1] 기준이므로 한 칸 밀어 정렬한다.
	for i := 0; i < n-1; i++ {
		var v float64
		switch {
		case math.IsNaN(ag[i]) || math.IsNaN(al[i]):
			v = math.NaN()
		case al[i] == 0:
			if ag[i] > 0 {
				v = 100.0
			} else {
				v = 50.0
			}
		default:
			v = 100.0 - 100.0/(1.0+ag[i]/al[i])
		}
		out[i+1] = v
	}
	for i := 0; i < period && i < n; i++ {
		out[i] = math.NaN()
	}
	return out
}

// ATR 은 Wilder 평활을 쓴다. 시드는 tr[1..period] 의 평균이다.
func ATR(high, low, close []float64, period int) []float64 {
	n := len(close)
	out := filled(n)
	if n <= period || period <= 0 {
		return out
	}
	tr := make([]float64, n)
	for i := 0; i < n; i++ {
		prev := close[0]
		if i > 0 {
			prev = close[i-1]
		}
		a := high[i] - low[i]
		b := math.Abs(high[i] - prev)
		c := math.Abs(low[i] - prev)
		tr[i] = math.Max(a, math.Max(b, c))
	}
	return RecursiveSmooth(tr, 1.0/float64(period), mean(tr[1:period+1]), period+1)
}

// LogReturn 은 마지막 봉 기준 lag 개 전 대비 로그수익률이다.
func LogReturn(close []float64, lag int) float64 {
	n := len(close)
	if n <= lag || close[n-1-lag] <= 0 {
		return nan()
	}
	return math.Log(close[n-1] / close[n-1-lag])
}

// RealizedVol 은 마지막 window 개 로그수익률의 표본표준편차다.
func RealizedVol(close []float64, window int) float64 {
	if len(close) < window+1 {
		return nan()
	}
	seg := close[len(close)-(window+1):]
	r := make([]float64, len(seg)-1)
	for i := 1; i < len(seg); i++ {
		r[i-1] = math.Log(seg[i] / seg[i-1])
	}
	if len(r) <= 1 {
		return nan()
	}
	return StdSample(r)
}

// ZScore 는 마지막 값의 직전 window 구간 대비 z점수다. 마지막 값 자신은 분포에서 뺀다.
func ZScore(values []float64, window int) float64 {
	if len(values) < window+1 {
		return nan()
	}
	ref := values[len(values)-(window+1) : len(values)-1]
	sd := StdSample(ref)
	if sd <= 0 {
		return 0.0
	}
	return (values[len(values)-1] - mean(ref)) / sd
}

// StdSample 은 ddof=1 표본표준편차다.
func StdSample(x []float64) float64 {
	if len(x) < 2 {
		return nan()
	}
	m := mean(x)
	var s float64
	for _, v := range x {
		d := v - m
		s += d * d
	}
	return math.Sqrt(s / float64(len(x)-1))
}

func mean(x []float64) float64 {
	if len(x) == 0 {
		return nan()
	}
	var s float64
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}
