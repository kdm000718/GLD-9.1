// Package sample 은 5분봉마다 +0분 시점 표본을 만든다.
//
// 제외 규칙(도지 / 워밍업 / 연속성)은 이 패키지에만 있다. cmd/backtest 는 G1'
// 게이트 러너이고 cmd/train 은 실거래 모델을 만드는데, 둘이 서로 다른 표본 집합을
// 보면 게이트가 검증한 적 없는 모델이 실거래에 나간다 — 게이트는 backtest 쪽만
// 돌리므로 어긋나도 통과한다. 그래서 규칙을 한 곳에 둔다.
//
// 서빙(실거래)도 같은 이유로 여기를 지난다. 규칙은 Features(serve.go) 하나에만
// 있고 Build 가 그것을 부른다 — 학습 표본과 서빙 입력이 갈릴 자리가 없다.
package sample

import (
	"github.com/kdm000718/GLD-9.1/internal/bars"
	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/model"
	"github.com/kdm000718/GLD-9.1/internal/walkforward"
)

// 피처가 요구하는 최소 봉 수. cmd/backtest 의 단언 메시지가 이 값을 인용하므로
// 내보낸다 — 두 곳에 따로 적으면 메시지와 실제 판정이 어긋날 수 있다.
const (
	Req1m  = 60
	Req5m  = 12
	minMS  = 60_000
	fiveMS = walkforward.FiveMinMS
)

// Counts 는 제외 사유별 개수다. 합계 + Kept 가 입력 5분봉 수와 같아야 한다.
type Counts struct {
	Kept   int
	Doji   int
	Warmup int
	Gap    int
}

// Build 는 +0분 표본을 행렬에 채우고 제외 개수를 함께 돌려준다.
//
// progress 는 nil 이어도 된다. nil 이 아니면 유지 표본이 200,000개 늘 때마다
// 부른다 — 9년치는 15분 가까이 걸리므로 진행이 보여야 한다.
func Build(b1, b5 bars.Bars, progress func(kept int)) ([]int64, *model.Matrix, []float64, Counts) {
	n := b5.Len()
	mat := model.NewMatrix(n, len(features.FeatureNames))
	cs := make([]int64, n)
	y := make([]float64, n)
	var c Counts

	for i := 0; i < n; i++ {
		t := b5.OpenTime[i]
		o, cl := b5.Open[i], b5.Close[i]
		if cl == o {
			c.Doji++
			continue
		}
		// 자격 검사는 Features 하나에만 있다 — 서빙도 같은 함수를 부른다.
		vals, reason := Features(b1, b5, t)
		switch reason {
		case Eligible:
		case Warmup:
			c.Warmup++
			continue
		case Gap:
			c.Gap++
			continue
		default:
			// Reason 이 늘었는데 여기를 안 고치면 Counts 의 분할 불변식이
			// 조용히 깨진다 — 그 봉은 어느 사유로도 세어지지 않는다.
			panic("sample.Build: 알 수 없는 사유 " + reason.String())
		}
		mat.SetRow(c.Kept, vals)
		cs[c.Kept] = t
		if cl > o {
			y[c.Kept] = 1
		} else {
			y[c.Kept] = 0
		}
		c.Kept++
		if progress != nil && c.Kept%200_000 == 0 {
			progress(c.Kept)
		}
	}
	mat.Truncate(c.Kept)
	return cs[:c.Kept], mat, y[:c.Kept], c
}
