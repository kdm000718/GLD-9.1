// Package walkforward 는 블록마다 재학습하며 앞으로 나아가는 검증을 돌린다.
//
// 핵심 규칙: 블록 시작 시점에 이미 정답이 확정된 표본만 학습에 쓴다.
// 5분봉의 정답은 candleStart + 5분 에 확정되므로 그 시각이 블록 시작 이하여야 한다.
// 이 조건을 놓치면 블록 경계마다 미래참조가 샌다.
package walkforward

import (
	"fmt"
	"math"

	"github.com/kdm000718/GLD-9.1/internal/model"
)

const (
	DayMS     = 86_400_000
	FiveMinMS = 300_000
)

// TrainableBefore 는 cutoff 시점에 학습 가능한 표본의 인덱스를 돌려준다.
// window 는 학습에 쓸 과거 구간 길이다.
func TrainableBefore(candleStarts []int64, cutoff, window int64) []int {
	var out []int
	for i, cs := range candleStarts {
		if cs >= cutoff-window && cs+FiveMinMS <= cutoff {
			out = append(out, i)
		}
	}
	return out
}

// Run 은 워크포워드를 돌리고 각 표본의 예측 확률을 돌려준다.
// 평가되지 않은 표본은 NaN 이다. 두 번째 반환값은 재학습 횟수다.
func Run(cs []int64, m *model.Matrix, y []float64, names []string,
	testStart int64, refitDays, trainDays, l2 float64,
	log func(string, ...any)) ([]float64, int, error) {

	if log == nil {
		log = func(string, ...any) {}
	}
	prob := make([]float64, len(y))
	for i := range prob {
		prob[i] = math.NaN()
	}
	step := int64(refitDays * DayMS)
	window := int64(trainDays * DayMS)
	end := cs[len(cs)-1] + FiveMinMS
	nFit := 0

	for start := testStart; start < end; start += step {
		stop := start + step
		if stop > end {
			stop = end
		}
		train := TrainableBefore(cs, start, window)
		var test []int
		for i, c := range cs {
			if c >= start && c < stop {
				test = append(test, i)
			}
		}
		if len(train) < 5000 || len(test) == 0 {
			continue
		}
		lr, err := model.Fit(m, train, y, names, l2)
		if err != nil {
			return nil, nFit, fmt.Errorf("블록 %d 학습 실패: %w", nFit, err)
		}
		for _, i := range test {
			prob[i] = lr.Prob(m.Row(i))
		}
		nFit++
		if nFit%12 == 0 {
			log("    ... 재학습 %d회", nFit)
		}
	}
	log("    총 재학습 %d회", nFit)
	return prob, nFit, nil
}
