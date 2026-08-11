package sample

import (
	"github.com/kdm000718/GLD-9.1/internal/bars"
	"github.com/kdm000718/GLD-9.1/internal/clock"
	"github.com/kdm000718/GLD-9.1/internal/features"
)

// Reason 은 표본이 채택되지 못한 사유다.
type Reason int

const (
	Eligible Reason = iota
	Warmup
	Gap
)

func (r Reason) String() string {
	switch r {
	case Eligible:
		return "eligible"
	case Warmup:
		return "warmup"
	case Gap:
		return "gap"
	}
	return "unknown"
}

// Features 는 시각 t 의 +0분 피처를 만든다. 채택되지 않으면 vals 는 nil 이다.
//
// **이 함수가 학습과 서빙의 유일한 접점이다.** cmd/train 이 만드는 모델은
// Build 가 채택한 표본으로 학습되는데, 그 Build 가 이 함수를 부른다. 서빙도
// 이 함수를 부른다 — 규칙이 갈릴 자리가 없다.
func Features(b1, b5 bars.Bars, t int64) ([]float64, Reason) {
	v, err := clock.New(t, b1, b5, t)
	if err != nil {
		// t 이전에 마감된 봉이 아예 없다 (데이터 시작부)
		return nil, Warmup
	}
	if v.Bars1m.Len() < Req1m || v.Bars5m.Len() < Req5m {
		return nil, Warmup
	}
	ot1, ot5 := v.Bars1m.OpenTime, v.Bars5m.OpenTime
	l1, l5 := len(ot1), len(ot5)
	if ot1[l1-1] != t-minMS ||
		ot1[l1-1]-ot1[l1-Req1m] != int64(Req1m-1)*minMS ||
		ot5[l5-1]-ot5[l5-Req5m] != int64(Req5m-1)*fiveMS {
		return nil, Gap
	}
	vals, ok := features.Build(v)
	if !ok {
		return nil, Warmup
	}
	return vals, Eligible
}
