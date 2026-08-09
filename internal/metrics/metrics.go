// Package metrics 는 정확도 중심 평가 지표를 계산한다.
package metrics

import (
	"math"
	"sort"
)

// ArraySplit 은 numpy.array_split 의 분할 규칙을 따른다.
// 앞 n%bins 개 묶음이 하나씩 더 크다. 반환값은 [시작, 끝) 쌍이다.
func ArraySplit(n, bins int) [][2]int {
	if bins <= 0 {
		return nil
	}
	base, extra := n/bins, n%bins
	out := make([][2]int, 0, bins)
	start := 0
	for i := 0; i < bins; i++ {
		size := base
		if i < extra {
			size++
		}
		out = append(out, [2]int{start, start + size})
		start += size
	}
	return out
}

// AUC 는 Mann-Whitney U 기반 ROC AUC 다. 동점은 평균 순위로 처리한다.
func AUC(y, p []float64) float64 {
	if len(y) != len(p) {
		// p 가 길면 조용히 앞부분만 쓰고, 짧으면 ranks 인덱싱에서 패닉한다.
		// 둘 다 그럴듯해 보이는 틀린 값 또는 엉뚱한 지점의 죽음이므로 NaN 을 돌려준다.
		return math.NaN()
	}
	var pos, neg int
	for _, v := range y {
		switch v {
		case 1:
			pos++
		case 0:
			neg++
		default:
			// 라벨이 정확히 0 또는 1 이 아니면 (NaN, Inf 등) Python 의
			// `y == 0` 비교와 달리 조용히 neg 에 편입시키지 않는다. NaN 라벨이
			// 섞인 채로 계산을 계속하면 그럴듯해 보이는 틀린 값이 나온다 —
			// 확실하게 NaN 을 돌려준다. (이 값은 이 파이프라인에서 항상
			// close>open 으로 결정되는 0/1 이라 실제로는 도달하지 않는다.)
			return math.NaN()
		}
	}
	if pos == 0 || neg == 0 {
		return math.NaN()
	}
	for _, v := range p {
		if math.IsNaN(v) {
			// NaN 이 섞이면 아래 sort.SliceStable 의 비교 함수(<)가 엄격 약순서를
			// 보장하지 못해 정렬이 조용히 깨지고, 그럴듯해 보이는 틀린 값을
			// 돌려준다 — 셔플된 입력이 아니면 드러나지 않는다. numpy 는 NaN 을
			// 끝으로 보내 결정적이지만 그 자체로 무의미한 값을 내는데, 여기서는
			// 그 대신 확실하게 NaN 을 돌려준다.
			return math.NaN()
		}
	}
	order := make([]int, len(p))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return p[order[a]] < p[order[b]] })

	ranks := make([]float64, len(p))
	for i := 0; i < len(order); {
		j := i
		for j+1 < len(order) && p[order[j+1]] == p[order[i]] {
			j++
		}
		avg := float64(i+j)/2.0 + 1.0
		for k := i; k <= j; k++ {
			ranks[order[k]] = avg
		}
		i = j + 1
	}
	var sum float64
	for i, v := range y {
		if v == 1 {
			sum += ranks[i]
		}
	}
	fp, fn := float64(pos), float64(neg)
	return (sum - fp*(fp+1)/2.0) / (fp * fn)
}

// Bin 은 교정표의 한 구간이다.
type Bin struct {
	N        int
	PredLow  float64
	PredHigh float64
	MeanPred float64
	Observed float64
	Gap      float64
}

// CalibrationTable 은 예측 확률을 분위로 나눠 말한 확률과 실제 빈도를 비교한다.
// 확률이 0.5 근처에 몰려 있으므로 등간격이 아니라 분위 구간을 쓴다.
func CalibrationTable(y, p []float64, bins int) []Bin {
	n := len(y)
	if n == 0 {
		return nil
	}
	if len(p) != n {
		// AUC 와 같은 이유. NaN 혼입 때와 동일하게 nil 을 돌려준다 —
		// ECE 는 rows 가 비면 NaN 이므로 관찰되는 동작이 일관된다.
		return nil
	}
	for _, v := range p {
		if math.IsNaN(v) {
			// AUC 와 동일한 이유: NaN 이 섞이면 아래 sort.SliceStable 이 조용히
			// 깨져 구간 경계가 뒤죽박죽인 채로 그럴듯한 틀린 표를 낸다.
			// CalibrationTable 은 공개 함수라 (Python 쪽에서는 리포트에 그대로
			// 노출된다) 확실하게 nil 을 돌려준다. ECE 는 rows 가 비어 있으면
			// 이미 NaN 을 반환하므로 관찰되는 동작은 그대로 유지된다.
			return nil
		}
	}
	if bins > n {
		bins = n
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return p[order[a]] < p[order[b]] })

	var out []Bin
	for _, seg := range ArraySplit(n, bins) {
		idx := order[seg[0]:seg[1]]
		if len(idx) == 0 {
			continue
		}
		lo, hi := p[idx[0]], p[idx[0]]
		var sp, sy float64
		for _, i := range idx {
			if p[i] < lo {
				lo = p[i]
			}
			if p[i] > hi {
				hi = p[i]
			}
			sp += p[i]
			sy += y[i]
		}
		m := float64(len(idx))
		mp, ob := sp/m, sy/m
		out = append(out, Bin{
			N: len(idx), PredLow: lo, PredHigh: hi,
			MeanPred: mp, Observed: ob, Gap: ob - mp,
		})
	}
	return out
}

// ECE 는 구간별 |말한 확률 − 실제 빈도| 를 표본 수로 가중 평균한다.
func ECE(y, p []float64, bins int) float64 {
	rows := CalibrationTable(y, p, bins)
	if len(rows) == 0 || len(y) == 0 {
		return math.NaN()
	}
	var s float64
	for _, r := range rows {
		s += float64(r.N) * math.Abs(r.Gap)
	}
	return s / float64(len(y))
}

// BinomTestNormal 은 p=0.5 귀무가설의 양측 검정 p값을 정규근사로 낸다.
// scipy 의 정확 이항검정이 아니다 — n 이 크면 사실상 같고, G1' 게이트에는
// p값이 들어가지 않는다. 이름으로 근사임을 드러낸다.
func BinomTestNormal(successes, n int) float64 {
	if n <= 0 {
		return math.NaN()
	}
	mu := float64(n) * 0.5
	sd := math.Sqrt(float64(n) * 0.25)
	d := math.Abs(float64(successes)-mu) - 0.5 // 연속성 보정
	if d < 0 {
		d = 0
	}
	return math.Erfc(d / (sd * math.Sqrt2))
}
