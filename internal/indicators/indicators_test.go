package indicators

import (
	"math"
	"math/rand"
	"testing"
)

func series(n int, seed int64) []float64 {
	r := rand.New(rand.NewSource(seed))
	out := make([]float64, n)
	p := 100.0
	for i := range out {
		p *= 1 + (r.Float64()-0.5)*0.01
		out[i] = p
	}
	return out
}

// 인과성: 앞부분만 넣고 계산한 값이 전체를 넣고 계산한 앞부분과 같아야 한다.
func TestCausality(t *testing.T) {
	x := series(300, 1)
	hi := make([]float64, len(x))
	lo := make([]float64, len(x))
	for i, v := range x {
		hi[i], lo[i] = v*1.002, v*0.998
	}

	fns := map[string]func(n int) []float64{
		"EMA9":  func(n int) []float64 { return EMA(x[:n], 9) },
		"EMA21": func(n int) []float64 { return EMA(x[:n], 21) },
		"RSI14": func(n int) []float64 { return RSI(x[:n], 14) },
		"ATR14": func(n int) []float64 { return ATR(hi[:n], lo[:n], x[:n], 14) },
	}
	for name, fn := range fns {
		full := fn(len(x))
		for _, m := range []int{120, 200, 260} {
			part := fn(m)
			for i := 0; i < m; i++ {
				a, b := full[i], part[i]
				if math.IsNaN(a) && math.IsNaN(b) {
					continue
				}
				if math.Abs(a-b) > 1e-12 {
					t.Fatalf("%s 가 미래를 본다: m=%d i=%d full=%v part=%v", name, m, i, a, b)
				}
			}
		}
	}
}

func TestEMAWarmupIsNaN(t *testing.T) {
	x := series(50, 2)
	e := EMA(x, 9)
	for i := 0; i < 8; i++ {
		if !math.IsNaN(e[i]) {
			t.Errorf("EMA[%d] = %v, 워밍업 구간은 NaN 이어야 한다", i, e[i])
		}
	}
	if math.IsNaN(e[8]) {
		t.Error("EMA[period-1] 은 시드값이어야 한다")
	}
	// 시드는 앞 period 개의 단순평균
	var s float64
	for i := 0; i < 9; i++ {
		s += x[i]
	}
	if math.Abs(e[8]-s/9) > 1e-12 {
		t.Errorf("EMA 시드 = %v, 기대 %v", e[8], s/9)
	}
}

func TestEMAShorterThanPeriodIsAllNaN(t *testing.T) {
	e := EMA(series(5, 3), 9)
	for i, v := range e {
		if !math.IsNaN(v) {
			t.Errorf("EMA[%d] = %v, 전부 NaN 이어야 한다", i, v)
		}
	}
}

func TestRSIBoundsAndFlatSeries(t *testing.T) {
	x := series(100, 4)
	r := RSI(x, 14)
	for i := 14; i < len(r); i++ {
		if math.IsNaN(r[i]) {
			t.Fatalf("RSI[%d] 가 NaN 이다", i)
		}
		if r[i] < 0 || r[i] > 100 {
			t.Fatalf("RSI[%d] = %v, 0..100 을 벗어났다", i, r[i])
		}
	}
	// 완전히 평평한 시계열: 상승도 하락도 0 → 50
	flat := make([]float64, 100)
	for i := range flat {
		flat[i] = 42.0
	}
	rf := RSI(flat, 14)
	if math.Abs(rf[99]-50.0) > 1e-9 {
		t.Errorf("평평한 시계열 RSI = %v, 기대 50", rf[99])
	}
	// 단조 상승: 하락이 0 → 100
	up := make([]float64, 100)
	for i := range up {
		up[i] = float64(i + 1)
	}
	ru := RSI(up, 14)
	if math.Abs(ru[99]-100.0) > 1e-9 {
		t.Errorf("단조 상승 RSI = %v, 기대 100", ru[99])
	}
}

func TestZScoreExcludesLastValue(t *testing.T) {
	// 마지막 값 자신은 분포에서 빠져야 한다
	x := []float64{1, 1, 1, 1, 1, 5}
	got := ZScore(x, 5)
	if math.IsNaN(got) || math.IsInf(got, 0) {
		// 앞 5개가 전부 같아 표준편차 0 → 0.0 을 돌려주는 규약
		t.Fatalf("ZScore = %v, 표준편차 0 이면 0 이어야 한다", got)
	}
	if got != 0.0 {
		t.Errorf("ZScore = %v, 기대 0 (sd == 0 규약)", got)
	}
}

func TestLogReturnAndRealizedVol(t *testing.T) {
	x := []float64{100, 110}
	if got := LogReturn(x, 1); math.Abs(got-math.Log(1.1)) > 1e-12 {
		t.Errorf("LogReturn = %v, 기대 %v", got, math.Log(1.1))
	}
	if got := LogReturn(x, 5); !math.IsNaN(got) {
		t.Errorf("lag 가 길이보다 큰데 %v 를 돌려줬다", got)
	}
	if got := RealizedVol([]float64{1, 2}, 5); !math.IsNaN(got) {
		t.Errorf("창보다 짧은데 %v 를 돌려줬다", got)
	}
}
