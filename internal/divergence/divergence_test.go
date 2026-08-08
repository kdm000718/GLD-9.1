package divergence

import (
	"math"
	"testing"
)

func TestFindPivotsHigh(t *testing.T) {
	//        0  1  2  3  4  5  6  7  8
	v := []float64{1, 2, 5, 2, 1, 2, 6, 2, 1}
	got := FindPivots(v, 2, "high")
	want := []int{2, 6}
	if len(got) != len(want) {
		t.Fatalf("피벗 %v, 기대 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("피벗 %v, 기대 %v", got, want)
		}
	}
}

func TestFindPivotsRejectsUnconfirmedTail(t *testing.T) {
	// 마지막 봉이 최고점이어도 오른쪽 k봉이 없으면 확정 피벗이 아니다
	v := []float64{1, 2, 3, 4, 9}
	for _, i := range FindPivots(v, 2, "high") {
		if i > len(v)-1-2 {
			t.Errorf("미확정 피벗 %d 를 인정했다 (n=%d, k=2)", i, len(v))
		}
	}
}

func TestFindPivotsSkipsWindowsWithNaN(t *testing.T) {
	v := []float64{1, math.NaN(), 5, 2, 1, 2, 6, 2, 1}
	for _, i := range FindPivots(v, 2, "high") {
		if i == 2 {
			t.Error("NaN 이 포함된 창을 피벗으로 인정했다")
		}
	}
}

func TestFindPivotsLow(t *testing.T) {
	v := []float64{9, 8, 1, 8, 9, 8, 0, 8, 9}
	got := FindPivots(v, 2, "low")
	if len(got) != 2 || got[0] != 2 || got[1] != 6 {
		t.Fatalf("저점 피벗 %v, 기대 [2 6]", got)
	}
}

func TestFindPivotsTooShort(t *testing.T) {
	if got := FindPivots([]float64{1, 2, 3}, 2, "high"); len(got) != 0 {
		t.Errorf("길이가 2k+1 미만인데 %v 를 돌려줬다", got)
	}
}

func TestRegularBearDivergence(t *testing.T) {
	// 가격 고점은 올라가는데 RSI 고점은 내려간다 → 정규 하락 다이버전스
	n := 40
	high := make([]float64, n)
	low := make([]float64, n)
	rsi := make([]float64, n)
	for i := range high {
		high[i], low[i], rsi[i] = 100, 90, 50
	}
	high[10], rsi[10] = 120, 80 // 첫 고점
	high[25], rsi[25] = 130, 65 // 더 높은 고점, 더 낮은 RSI
	s := Detect(high, low, rsi, 3, 5, 60, 15.0)
	if s.RegularBear <= 0 {
		t.Fatalf("정규 하락 다이버전스를 못 잡았다: %+v", s)
	}
	if s.Score() >= 0 {
		t.Errorf("Score = %v, 하락 신호이므로 음수여야 한다", s.Score())
	}
}

func TestRegularBullDivergence(t *testing.T) {
	n := 40
	high := make([]float64, n)
	low := make([]float64, n)
	rsi := make([]float64, n)
	for i := range high {
		high[i], low[i], rsi[i] = 100, 90, 50
	}
	low[10], rsi[10] = 80, 20 // 첫 저점
	low[25], rsi[25] = 70, 35 // 더 낮은 저점, 더 높은 RSI
	s := Detect(high, low, rsi, 3, 5, 60, 15.0)
	if s.RegularBull <= 0 {
		t.Fatalf("정규 상승 다이버전스를 못 잡았다: %+v", s)
	}
	if s.Score() <= 0 {
		t.Errorf("Score = %v, 상승 신호이므로 양수여야 한다", s.Score())
	}
}

func TestStrengthAndDecayBounds(t *testing.T) {
	if got := strength(10); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("strength(10) = %v, 기대 0.5", got)
	}
	if got := strength(0); got != 0 {
		t.Errorf("strength(0) = %v, 기대 0", got)
	}
	if got := decay(15, 15); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("반감기만큼 지났으면 0.5 여야 한다: %v", got)
	}
	if got := decay(0, 15); math.Abs(got-1.0) > 1e-12 {
		t.Errorf("decay(0) = %v, 기대 1", got)
	}
}

func TestEmptyInputIsZeroSignal(t *testing.T) {
	s := Detect(nil, nil, nil, 3, 5, 60, 10)
	if s.Score() != 0 {
		t.Errorf("빈 입력인데 Score = %v", s.Score())
	}
}
