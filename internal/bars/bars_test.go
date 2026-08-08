package bars

import "testing"

func sample(n int) Bars {
	b := Bars{
		OpenTime: make([]int64, n), CloseTime: make([]int64, n),
		Open: make([]float64, n), High: make([]float64, n),
		Low: make([]float64, n), Close: make([]float64, n),
		Volume: make([]float64, n), QuoteVolume: make([]float64, n),
		Trades: make([]int64, n), TakerBuyBase: make([]float64, n),
		TakerBuyQuote: make([]float64, n),
	}
	for i := 0; i < n; i++ {
		b.OpenTime[i] = int64(i) * 60_000
		b.CloseTime[i] = b.OpenTime[i] + 59_999
		b.Close[i] = float64(i)
	}
	return b
}

func TestLenAndSlice(t *testing.T) {
	b := sample(10)
	if b.Len() != 10 {
		t.Fatalf("Len = %d, 기대 10", b.Len())
	}
	s := b.Slice(2, 5)
	if s.Len() != 3 {
		t.Fatalf("Slice(2,5).Len = %d, 기대 3", s.Len())
	}
	if s.OpenTime[0] != 120_000 || s.Close[2] != 4 {
		t.Errorf("슬라이스 내용 오류: %v %v", s.OpenTime, s.Close)
	}
}

func TestSliceCoversEveryField(t *testing.T) {
	// 필드를 하나라도 빠뜨리면 길이가 어긋난다 — 조용한 버그를 여기서 잡는다
	b := sample(10)
	s := b.Slice(3, 7)
	lens := []int{
		len(s.OpenTime), len(s.CloseTime), len(s.Open), len(s.High), len(s.Low),
		len(s.Close), len(s.Volume), len(s.QuoteVolume), len(s.Trades),
		len(s.TakerBuyBase), len(s.TakerBuyQuote),
	}
	for i, l := range lens {
		if l != 4 {
			t.Errorf("필드 %d 의 길이가 %d 다 — Slice 가 그 필드를 빠뜨렸다", i, l)
		}
	}
}

func TestLastClampsToLength(t *testing.T) {
	b := sample(5)
	if got := b.Last(10).Len(); got != 5 {
		t.Errorf("Last(10).Len = %d, 기대 5", got)
	}
	if got := b.Last(3); got.Len() != 3 || got.Close[0] != 2 {
		t.Errorf("Last(3) 오류: len=%d close[0]=%v", got.Len(), got.Close[0])
	}
}

func TestIndexOfOpenTime(t *testing.T) {
	b := sample(5)
	if got := b.IndexOfOpenTime(180_000); got != 3 {
		t.Errorf("IndexOfOpenTime(180000) = %d, 기대 3", got)
	}
	if got := b.IndexOfOpenTime(180_001); got != -1 {
		t.Errorf("없는 시각인데 %d 를 돌려줬다", got)
	}
	if got := b.IndexOfOpenTime(999_999_999); got != -1 {
		t.Errorf("범위 밖인데 %d 를 돌려줬다", got)
	}
}
