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

func TestFindPivotsTieKeepsFirstBarOfPlateau(t *testing.T) {
	// 평탄 구간에서는 첫 봉만 피벗이다. 'c > 왼쪽' 과 'c >= 오른쪽' 의
	// 비대칭이 그것을 만든다 — 대칭으로 바꾸면 Python 과 어긋난다.
	cases := []struct {
		name   string
		values []float64
		kind   string
		want   []int
	}{
		{"고점 평탄 3봉", []float64{1, 5, 5, 5, 1, 1, 1}, "high", []int{1}},
		{"고점 평탄 2봉", []float64{1, 2, 5, 5, 2, 1, 1}, "high", []int{2}},
		{"저점 평탄 3봉", []float64{9, 5, 5, 5, 9, 9, 9}, "low", []int{1}},
	}
	for _, c := range cases {
		got := FindPivots(c.values, 1, c.kind)
		if len(got) != len(c.want) {
			t.Fatalf("%s: %v, 기대 %v", c.name, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("%s: %v, 기대 %v", c.name, got, c.want)
			}
		}
	}
}

func TestFindPivotsUnaffectedByDistantNaN(t *testing.T) {
	v := []float64{math.NaN(), 1, 2, 5, 2, 1, 2, 6, 2, 1, 1}
	got := FindPivots(v, 2, "high")
	want := []int{3, 7}
	if len(got) != len(want) {
		t.Fatalf("피벗 %v, 기대 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("피벗 %v, 기대 %v", got, want)
		}
	}
}

func TestDedupeCloseKeepsLaterPivot(t *testing.T) {
	// minSep 보다 가까우면 나중 것만 남는다.
	got := dedupeClose([]int{10, 12, 30}, 5)
	want := []int{12, 30}
	if len(got) != len(want) {
		t.Fatalf("%v, 기대 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%v, 기대 %v", got, want)
		}
	}
	// 떨어져 있으면 둘 다 남는다
	if g := dedupeClose([]int{10, 20}, 5); len(g) != 2 {
		t.Errorf("%v, 둘 다 남아야 한다", g)
	}
	// 빈 입력
	if g := dedupeClose(nil, 5); len(g) != 0 {
		t.Errorf("%v, 빈 결과여야 한다", g)
	}
}
