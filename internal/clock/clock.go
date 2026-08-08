// Package clock 은 미래참조 편향을 구조적으로 차단한다.
//
// 핵심 규칙: 의사결정 시각 t 에서 볼 수 있는 것은 close_time < t 인 봉뿐이다.
//
// MarketView 는 생성 시점에 미래 구간을 물리적으로 잘라내고, 잘라낸 결과에
// 미래 봉이 하나도 없음을 확인한다. 따라서 피처 코드가 실수로 미래를
// 참조하려 해도 그 데이터가 객체 안에 아예 없다.
//
// 봉 중간 예측: 대상 5분봉의 시작을 candleStart, 의사결정 시각을 t 라 하면
// t = candleStart + k분 (k=0..4) 이다. k>=1 이면 그 5분봉 안에서 이미 마감된
// 1분봉 k개가 보인다. 반면 5분봉 자신은 close_time = candleStart+299999 이므로
// k<=4 인 한 절대 보이지 않는다 — 같은 절단 규칙이 그대로 적용된다.
package clock

import (
	"fmt"
	"sort"

	"github.com/kdm000718/GLD-9.1/internal/bars"
)

// LookaheadError 는 미래 데이터 접근이 감지되었을 때다.
type LookaheadError struct{ Msg string }

func (e *LookaheadError) Error() string { return e.Msg }

// MarketView 는 시각 t 에서 관측 가능한 시장 상태다.
type MarketView struct {
	T           int64 // 의사결정 시각 (ms)
	CandleStart int64 // 대상 5분봉의 시작 (ms)
	Bars1m      bars.Bars
	Bars5m      bars.Bars
}

// New 는 t 이후의 데이터를 잘라낸 뷰를 만든다.
func New(t int64, b1, b5 bars.Bars, candleStart int64) (*MarketView, error) {
	if d := t - candleStart; d < 0 || d >= 300_000 {
		return nil, &LookaheadError{fmt.Sprintf(
			"의사결정 시각이 대상 5분봉 밖입니다: t=%d, candleStart=%d", t, candleStart)}
	}
	c1, err := cut(b1, t, "1m")
	if err != nil {
		return nil, err
	}
	c5, err := cut(b5, t, "5m")
	if err != nil {
		return nil, err
	}
	return &MarketView{T: t, CandleStart: candleStart, Bars1m: c1, Bars5m: c5}, nil
}

// ElapsedMin 은 대상 5분봉이 시작된 뒤 경과한 분(0..4)이다.
func (v *MarketView) ElapsedMin() int { return int((v.T - v.CandleStart) / 60_000) }

// LastPrice 는 t 직전에 마감된 1분봉의 종가다.
func (v *MarketView) LastPrice() float64 { return v.Bars1m.Close[v.Bars1m.Len()-1] }

// cut 은 close_time < t 인 봉만 남긴다.
func cut(b bars.Bars, t int64, label string) (bars.Bars, error) {
	// close_time 은 단조증가 → 이진탐색으로 close_time >= t 인 첫 인덱스를 찾는다.
	hi := sort.Search(b.Len(), func(i int) bool { return b.CloseTime[i] >= t })
	c := b.Slice(0, hi)
	if c.Len() > 0 && c.CloseTime[c.Len()-1] >= t {
		return bars.Bars{}, &LookaheadError{fmt.Sprintf(
			"%s: 절단 실패 — close_time=%d >= t=%d", label, c.CloseTime[c.Len()-1], t)}
	}
	if c.Len() == 0 {
		return bars.Bars{}, &LookaheadError{fmt.Sprintf("%s: t=%d 이전에 마감된 봉이 없습니다", label, t)}
	}
	return c, nil
}
