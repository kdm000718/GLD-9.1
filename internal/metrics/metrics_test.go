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
