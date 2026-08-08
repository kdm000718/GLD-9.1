// Package bars 는 정렬·중복제거된 OHLCV 시계열을 담는다.
// 시점 절단(미래참조 차단)은 internal/clock 의 책임이지 여기가 아니다.
package bars

import "sort"

// Bars 의 모든 슬라이스는 길이가 같다.
type Bars struct {
	OpenTime      []int64 // ms, 봉 시작
	CloseTime     []int64 // ms, 봉의 마지막 순간 (OpenTime + 간격 − 1)
	Open          []float64
	High          []float64
	Low           []float64
	Close         []float64
	Volume        []float64
	QuoteVolume   []float64
	Trades        []int64
	TakerBuyBase  []float64
	TakerBuyQuote []float64
}

func (b Bars) Len() int { return len(b.OpenTime) }

// Slice 는 [lo, hi) 구간을 돌려준다. Go 슬라이스는 백킹 배열을 공유하므로 복사가 없다.
func (b Bars) Slice(lo, hi int) Bars {
	return Bars{
		OpenTime:      b.OpenTime[lo:hi],
		CloseTime:     b.CloseTime[lo:hi],
		Open:          b.Open[lo:hi],
		High:          b.High[lo:hi],
		Low:           b.Low[lo:hi],
		Close:         b.Close[lo:hi],
		Volume:        b.Volume[lo:hi],
		QuoteVolume:   b.QuoteVolume[lo:hi],
		Trades:        b.Trades[lo:hi],
		TakerBuyBase:  b.TakerBuyBase[lo:hi],
		TakerBuyQuote: b.TakerBuyQuote[lo:hi],
	}
}

// Last 는 마지막 n개. n 이 길이보다 크면 있는 만큼 돌려준다.
func (b Bars) Last(n int) Bars {
	if n >= b.Len() {
		return b
	}
	return b.Slice(b.Len()-n, b.Len())
}

// IndexOfOpenTime 은 OpenTime == t 인 봉의 인덱스. 없으면 -1.
func (b Bars) IndexOfOpenTime(t int64) int {
	i := sort.Search(b.Len(), func(i int) bool { return b.OpenTime[i] >= t })
	if i < b.Len() && b.OpenTime[i] == t {
		return i
	}
	return -1
}
