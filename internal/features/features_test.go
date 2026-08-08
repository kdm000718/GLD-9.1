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
	cis := []int{200, 300, 380}
	compared := 0
	for _, ci := range cis {
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
		// 양쪽이 다 거부하면 비교를 한 건도 하지 않고 통과해 버린다. 세 ci 는 전부
		// 워밍업이 충분한 고정 합성 데이터이므로 거부는 그 자체로 회귀다.
		if !ok1 {
			t.Fatalf("ci=%d: Build 가 양쪽 다 거부했다 — 워밍업이 충분한데", ci)
		}
		compared++
		for i := range a {
			if a[i] != bb[i] {
				t.Fatalf("ci=%d %s: %v vs %v — 피처가 미래를 본다", ci, FeatureNames[i], a[i], bb[i])
			}
		}
	}
	// 이 테스트가 공허하게 통과하지 않았음을 남긴다.
	if compared != len(cis) {
		t.Fatalf("비교한 시점 %d개, 기대 %d개 — 비교를 건너뛰었다", compared, len(cis))
	}
}
