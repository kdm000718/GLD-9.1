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

func TestArraySplitRealDataScaleAnchor(t *testing.T) {
	// Task 12 가 실제로 돌리는 규모: 888,525 표본을 10 구간으로 나누면
	// 앞 5개가 88853, 뒤 5개가 88852 여야 한다 (888525 % 10 == 5).
	g := ArraySplit(888525, 10)
	if len(g) != 10 {
		t.Fatalf("묶음 %d개, 기대 10개", len(g))
	}
	for i, seg := range g {
		size := seg[1] - seg[0]
		want := 88852
		if i < 5 {
			want = 88853
		}
		if size != want {
			t.Errorf("%d번째 묶음 크기 %d, 기대 %d", i, size, want)
		}
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

func TestAUCNaNProbabilityYieldsNaN(t *testing.T) {
	// 셔플된(비정렬) 입력이어야 드러난다 — 이미 정렬된 입력에 NaN 을 섞으면
	// 깨진 정렬도 우연히 원래 순서를 그대로 돌려주므로 버그가 숨는다.
	y := []float64{0, 1, 0, 1, 0, 1, 0, 1}
	p := []float64{0.9, 0.2, 0.3, 0.7, math.NaN(), 0.1, 0.8, 0.6}
	if got := AUC(y, p); !math.IsNaN(got) {
		t.Errorf("NaN 확률이 섞였는데 %v 를 돌려줬다", got)
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

func TestCalibrationTableBinning(t *testing.T) {
	// TestECEIsZeroWhenPerfectlyCalibrated 와 TestECEDetectsOverconfidence 는
	// p 를 고정값으로 채워서, bins 인자를 무시하고 항상 구간 1개만 만드는
	// 구현도 통과시킨다. 여기서는 p 를 오름차순 정수로 바꿔가며(이미 정렬된
	// 상태라 기대값 계산이 쉽다) bins=5, n=23 으로 각 구간의 N·PredLow·
	// PredHigh·Gap 을 직접 대조한다. n%bins=3 이므로 앞 3구간이 5개씩,
	// 뒤 2구간이 4개씩이어야 한다 (numpy array_split 규칙).
	n := 23
	p := make([]float64, n)
	y := make([]float64, n)
	for i := 0; i < n; i++ {
		p[i] = float64(i) // 이미 정렬돼 있으므로 order == 항등치환
		if i%2 == 0 {
			y[i] = 1
		}
	}
	got := CalibrationTable(y, p, 5)
	want := []Bin{
		{N: 5, PredLow: 0, PredHigh: 4, MeanPred: 2.0, Observed: 0.6, Gap: 0.6 - 2.0},
		{N: 5, PredLow: 5, PredHigh: 9, MeanPred: 7.0, Observed: 0.4, Gap: 0.4 - 7.0},
		{N: 5, PredLow: 10, PredHigh: 14, MeanPred: 12.0, Observed: 0.6, Gap: 0.6 - 12.0},
		{N: 4, PredLow: 15, PredHigh: 18, MeanPred: 16.5, Observed: 0.5, Gap: 0.5 - 16.5},
		{N: 4, PredLow: 19, PredHigh: 22, MeanPred: 20.5, Observed: 0.5, Gap: 0.5 - 20.5},
	}
	if len(got) != len(want) {
		t.Fatalf("구간 %d개, 기대 %d개", len(got), len(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.N != w.N {
			t.Errorf("%d번째 N=%d, 기대 %d", i, g.N, w.N)
		}
		if g.PredLow != w.PredLow || g.PredHigh != w.PredHigh {
			t.Errorf("%d번째 [%v,%v], 기대 [%v,%v]", i, g.PredLow, g.PredHigh, w.PredLow, w.PredHigh)
		}
		if math.Abs(g.MeanPred-w.MeanPred) > 1e-12 {
			t.Errorf("%d번째 MeanPred=%v, 기대 %v", i, g.MeanPred, w.MeanPred)
		}
		if math.Abs(g.Observed-w.Observed) > 1e-12 {
			t.Errorf("%d번째 Observed=%v, 기대 %v", i, g.Observed, w.Observed)
		}
		if math.Abs(g.Gap-w.Gap) > 1e-12 {
			t.Errorf("%d번째 Gap=%v, 기대 %v", i, g.Gap, w.Gap)
		}
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

func TestBinomTestNormalExactValueWithContinuityCorrection(t *testing.T) {
	// 500/1000, 600/1000 은 연속성 보정 유무와 무관하게 느슨한 경계를
	// 통과해버려 보정이 실제로 적용됐는지 구분하지 못한다 (0.024 vs 무보정
	// 값 차이가 나는 505/1000 을 골랐다 — 공식대로 직접 계산한 값과 비교).
	// 파이썬 math.erfc 로 계산: 보정 적용 0.775946788278143, 미적용 0.7518296340458492.
	got := BinomTestNormal(505, 1000)
	want := 0.775946788278143
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("BinomTestNormal(505,1000) = %v, 기대 %v (연속성 보정 누락 시 %v 가 나온다)",
			got, want, 0.7518296340458492)
	}
}
