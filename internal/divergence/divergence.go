// Package divergence 는 가격 스윙과 RSI 스윙을 비교해 다이버전스를 판정한다.
//
// 미래참조에 대한 해명: 스윙 피벗은 좌우 k봉을 비교해 판정하므로 인덱스 i 가
// 피벗인지 알려면 i+k 봉까지 필요하다. 여기서는 확정된 피벗만 쓴다 —
// 가시 구간의 마지막 인덱스를 n-1 이라 할 때 i <= n-1-k 인 것만 인정한다.
// 판정에 쓰인 봉이 전부 가시 구간 안에 있으므로 미래를 보지 않는다.
// 대가는 지연이다: 피벗은 최소 k봉 늦게 확정된다. 실거래와 같은 조건이다.
package divergence

import "math"

// Signal 은 한 타임프레임의 다이버전스 상태다.
type Signal struct {
	RegularBear        float64 // 고점 상승 + RSI 고점 하락 → 하락 신호 강도 (0..1)
	RegularBull        float64 // 저점 하락 + RSI 저점 상승 → 상승 신호 강도 (0..1)
	HiddenBear         float64
	HiddenBull         float64
	BarsSinceHighPivot float64
	BarsSinceLowPivot  float64
}

// Score 는 부호 있는 종합 점수다. + 는 상승, − 는 하락.
func (s Signal) Score() float64 {
	return (s.RegularBull + s.HiddenBull) - (s.RegularBear + s.HiddenBear)
}

// FindPivots 는 확정된 스윙 피벗 인덱스를 오래된 것부터 돌려준다.
// kind 가 "high" 면 좌우 k봉보다 크거나 같은 지점, "low" 면 작거나 같은 지점이다.
func FindPivots(values []float64, k int, kind string) []int {
	n := len(values)
	if k <= 0 || n < 2*k+1 {
		return nil
	}
	var out []int
	for i := k; i <= n-1-k; i++ {
		// 창에 NaN 이 하나라도 있으면 피벗이 아니다.
		// (numpy 는 max 가 NaN 이 되고 비교가 False 라 같은 결과였다.)
		bad := false
		for j := i - k; j <= i+k; j++ {
			if math.IsNaN(values[j]) {
				bad = true
				break
			}
		}
		if bad {
			continue
		}
		c := values[i]
		ok := true
		if kind == "high" {
			for j := i - k; j <= i+k; j++ {
				if values[j] > c {
					ok = false
					break
				}
			}
			ok = ok && c > values[i-1] && c >= values[i+1]
		} else {
			for j := i - k; j <= i+k; j++ {
				if values[j] < c {
					ok = false
					break
				}
			}
			ok = ok && c < values[i-1] && c <= values[i+1]
		}
		if ok {
			out = append(out, i)
		}
	}
	return out
}

// dedupeClose 는 너무 가까이 붙은 피벗 중 최근 것만 남긴다.
func dedupeClose(pivots []int, minSep int) []int {
	var kept []int
	for _, p := range pivots {
		if len(kept) > 0 && p-kept[len(kept)-1] < minSep {
			kept[len(kept)-1] = p
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// Detect 는 마지막 두 개의 확정 피벗을 비교해 다이버전스를 판정한다.
// 강도는 RSI 격차를 0..1 로 스케일하고 피벗이 오래될수록 지수 감쇠시킨다.
func Detect(high, low, rsiValues []float64, k, minSep, maxSpan int, recencyHalflife float64) Signal {
	n := len(high)
	if n == 0 || len(rsiValues) != n || len(low) != n {
		return Signal{}
	}
	var s Signal
	s.BarsSinceHighPivot = math.NaN()
	s.BarsSinceLowPivot = math.NaN()

	hiPiv := dedupeClose(FindPivots(high, k, "high"), minSep)
	if len(hiPiv) >= 2 {
		p1, p2 := hiPiv[len(hiPiv)-2], hiPiv[len(hiPiv)-1]
		s.BarsSinceHighPivot = float64(n - 1 - p2)
		span := p2 - p1
		if span > 0 && span <= maxSpan && !math.IsNaN(rsiValues[p1]) && !math.IsNaN(rsiValues[p2]) {
			dPrice := high[p2] - high[p1]
			dRSI := rsiValues[p2] - rsiValues[p1]
			d := decay(float64(n-1-p2), recencyHalflife)
			switch {
			case dPrice > 0 && dRSI < 0:
				s.RegularBear = strength(math.Abs(dRSI)) * d
			case dPrice < 0 && dRSI > 0:
				s.HiddenBear = strength(math.Abs(dRSI)) * d
			}
		}
	}

	loPiv := dedupeClose(FindPivots(low, k, "low"), minSep)
	if len(loPiv) >= 2 {
		q1, q2 := loPiv[len(loPiv)-2], loPiv[len(loPiv)-1]
		s.BarsSinceLowPivot = float64(n - 1 - q2)
		span := q2 - q1
		if span > 0 && span <= maxSpan && !math.IsNaN(rsiValues[q1]) && !math.IsNaN(rsiValues[q2]) {
			dPrice := low[q2] - low[q1]
			dRSI := rsiValues[q2] - rsiValues[q1]
			d := decay(float64(n-1-q2), recencyHalflife)
			switch {
			case dPrice < 0 && dRSI > 0:
				s.RegularBull = strength(math.Abs(dRSI)) * d
			case dPrice > 0 && dRSI < 0:
				s.HiddenBull = strength(math.Abs(dRSI)) * d
			}
		}
	}
	return s
}

// strength 는 RSI 격차를 0..1 강도로 만든다. 10포인트 차이에서 0.5 다.
func strength(rsiGap float64) float64 { return rsiGap / (rsiGap + 10.0) }

func decay(ageBars, halflife float64) float64 {
	if halflife <= 0 {
		return 1.0
	}
	return math.Exp(-math.Ln2 * ageBars / halflife)
}
