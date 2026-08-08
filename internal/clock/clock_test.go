package clock

import (
	"errors"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/bars"
)

func mk(n int, stepMS int64) bars.Bars {
	b := bars.Bars{
		OpenTime: make([]int64, n), CloseTime: make([]int64, n),
		Open: make([]float64, n), High: make([]float64, n), Low: make([]float64, n),
		Close: make([]float64, n), Volume: make([]float64, n),
		QuoteVolume: make([]float64, n), Trades: make([]int64, n),
		TakerBuyBase: make([]float64, n), TakerBuyQuote: make([]float64, n),
	}
	for i := 0; i < n; i++ {
		b.OpenTime[i] = int64(i) * stepMS
		b.CloseTime[i] = b.OpenTime[i] + stepMS - 1
		b.Close[i] = float64(i)
	}
	return b
}

func TestCutRemovesEverythingAtOrAfterT(t *testing.T) {
	b1, b5 := mk(600, 60_000), mk(120, 300_000)
	target := int64(300) * 60_000 // 1분봉 300번째의 시작
	v, err := New(target, b1, b5, target)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n := v.Bars1m.Len(); n != 300 {
		t.Errorf("1분봉 %d개, 기대 300개", n)
	}
	last := v.Bars1m.CloseTime[v.Bars1m.Len()-1]
	if last >= target {
		t.Errorf("마지막 close_time %d 가 t=%d 이상이다 — 미래가 남았다", last, target)
	}
	for i := 0; i < v.Bars5m.Len(); i++ {
		if v.Bars5m.CloseTime[i] >= target {
			t.Fatalf("5분봉 %d 의 close_time 이 t 이상이다", i)
		}
	}
}

func TestTargetCandleIsNeverVisible(t *testing.T) {
	// 대상 5분봉 자신은 close_time = candle_start+299999 이므로 k<=4 면 안 보인다
	b1, b5 := mk(600, 60_000), mk(120, 300_000)
	cs := int64(60) * 300_000
	for k := int64(0); k <= 4; k++ {
		v, err := New(cs+k*60_000, b1, b5, cs)
		if err != nil {
			t.Fatalf("k=%d New: %v", k, err)
		}
		if idx := v.Bars5m.IndexOfOpenTime(cs); idx >= 0 {
			t.Errorf("k=%d 인데 대상 5분봉이 보인다 (idx=%d)", k, idx)
		}
		if got := v.ElapsedMin(); got != int(k) {
			t.Errorf("ElapsedMin = %d, 기대 %d", got, k)
		}
	}
}

func TestElapsedOutOfRangeIsError(t *testing.T) {
	b1, b5 := mk(600, 60_000), mk(120, 300_000)
	cs := int64(60) * 300_000
	if _, err := New(cs+300_000, b1, b5, cs); err == nil {
		t.Error("경과 5분인데 에러가 없다")
	}
	if _, err := New(cs-1, b1, b5, cs); err == nil {
		t.Error("t 가 candle_start 보다 이른데 에러가 없다")
	}
}

func TestNoVisibleBarsIsLookaheadError(t *testing.T) {
	b1, b5 := mk(600, 60_000), mk(120, 300_000)
	var le *LookaheadError
	_, err := New(0, b1, b5, 0)
	if err == nil {
		t.Fatal("t=0 이면 볼 수 있는 봉이 없는데 에러가 없다")
	}
	if !errors.As(err, &le) {
		t.Errorf("에러 타입이 LookaheadError 가 아니다: %T", err)
	}
}

func TestLastPriceIsLastClosedBar(t *testing.T) {
	b1, b5 := mk(600, 60_000), mk(120, 300_000)
	target := int64(300) * 60_000
	v, _ := New(target, b1, b5, target)
	if got := v.LastPrice(); got != 299 {
		t.Errorf("LastPrice = %v, 기대 299 (299번 봉의 종가)", got)
	}
}

func TestBarClosingExactlyAtTIsExcluded(t *testing.T) {
	// close_time == t 인 봉은 보이지 않는다. Python 의
	// searchsorted(close_time, t, side="left") 와 같은 경계다.
	// 실데이터에서는 close_time 이 항상 open+interval-1 이라 이 경우가
	// 생기지 않지만, 절단 규칙 자체를 고정해 둔다.
	b1 := mk(10, 60_000)
	b5 := mk(10, 300_000)
	// 5번 1분봉의 close_time 을 정확히 t 로 맞춘다
	target := b1.OpenTime[5] + 60_000 - 1
	b1.CloseTime[5] = target

	v, err := New(target, b1, b5, target-(target%300_000))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// 인덱스 5 는 close_time == t 이므로 제외돼야 한다 → 0..4 만 보인다
	if got := v.Bars1m.Len(); got != 5 {
		t.Fatalf("보이는 1분봉 %d개, 기대 5개 (close_time == t 인 봉은 제외)", got)
	}
	if last := v.Bars1m.CloseTime[v.Bars1m.Len()-1]; last >= target {
		t.Errorf("마지막 close_time %d 가 t=%d 이상이다", last, target)
	}
}
