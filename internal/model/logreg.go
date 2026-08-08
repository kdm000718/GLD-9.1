// Package model 은 L2 정규화 로지스틱 회귀다. 학습 후 계수는 동결된다.
//
// 미래참조 차단의 핵심: Fit 은 테스트 구간이 시작되기 전 데이터로만 호출된다.
// 테스트 중에는 재학습도 재표준화도 하지 않는다. 표준화 평균·표준편차도
// 학습 구간에서만 계산한다 (테스트 통계 누수 방지).
package model

import (
	"fmt"
	"math"

	"gonum.org/v1/gonum/optimize"
)

type LogReg struct {
	L2           float64   `json:"l2"`
	Coef         []float64 `json:"coef"`
	Intercept    float64   `json:"intercept"`
	Mu           []float64 `json:"mu"`
	Sd           []float64 `json:"sd"`
	NTrain       int       `json:"n_train"`
	FeatureNames []string  `json:"feature_names"`
}

// Sigmoid 는 극단값에서도 NaN 을 내지 않는다.
func Sigmoid(z float64) float64 {
	if z >= 0 {
		return 1.0 / (1.0 + math.Exp(-z))
	}
	e := math.Exp(z)
	return e / (1.0 + e)
}

// Logit 은 표준화 후 선형결합이다.
func (m *LogReg) Logit(x []float32) float64 {
	z := m.Intercept
	for i, c := range m.Coef {
		z += (float64(x[i]) - m.Mu[i]) / m.Sd[i] * c
	}
	return z
}

func (m *LogReg) Prob(x []float32) float64 { return Sigmoid(m.Logit(x)) }

// Fit 은 rows 로 지정한 행만 써서 학습한다.
func Fit(mat *Matrix, rows []int, y []float64, names []string, l2 float64) (*LogReg, error) {
	n, p := len(rows), mat.Cols
	if n == 0 {
		return nil, fmt.Errorf("학습 표본이 없습니다")
	}

	mu := make([]float64, p)
	for _, r := range rows {
		row := mat.Row(r)
		for j := 0; j < p; j++ {
			mu[j] += float64(row[j])
		}
	}
	for j := range mu {
		mu[j] /= float64(n)
	}
	sd := make([]float64, p)
	for _, r := range rows {
		row := mat.Row(r)
		for j := 0; j < p; j++ {
			d := float64(row[j]) - mu[j]
			sd[j] += d * d
		}
	}
	for j := range sd {
		// Python 은 np.std 기본값(ddof=0)을 쓴다. 여기서도 같게 맞춘다.
		sd[j] = math.Sqrt(sd[j] / float64(n))
		if sd[j] < 1e-12 {
			sd[j] = 1.0
		}
	}

	// 표준화된 설계행렬을 미리 만든다 (반복 계산 회피)
	z := make([]float64, n*p)
	yy := make([]float64, n)
	for i, r := range rows {
		row := mat.Row(r)
		for j := 0; j < p; j++ {
			z[i*p+j] = (float64(row[j]) - mu[j]) / sd[j]
		}
		yy[i] = y[r]
	}

	// w = [계수 p개, 절편]. 절편은 정규화에서 제외한다.
	objective := func(w []float64) float64 {
		var ll float64
		for i := 0; i < n; i++ {
			s := w[p]
			zi := z[i*p : (i+1)*p]
			for j := 0; j < p; j++ {
				s += zi[j] * w[j]
			}
			// 수치 안정 log-loss: logaddexp(0, s) − y*s
			ll += logAddExp0(s) - yy[i]*s
		}
		var reg float64
		for j := 0; j < p; j++ {
			reg += w[j] * w[j]
		}
		return ll + 0.5*l2*reg
	}

	gradient := func(grad, w []float64) {
		for j := range grad {
			grad[j] = 0
		}
		for i := 0; i < n; i++ {
			s := w[p]
			zi := z[i*p : (i+1)*p]
			for j := 0; j < p; j++ {
				s += zi[j] * w[j]
			}
			r := Sigmoid(s) - yy[i]
			for j := 0; j < p; j++ {
				grad[j] += zi[j] * r
			}
			grad[p] += r
		}
		for j := 0; j < p; j++ {
			grad[j] += l2 * w[j]
		}
	}

	problem := optimize.Problem{Func: objective, Grad: gradient}
	// 목적함수가 L2 때문에 강볼록이라 최적점이 유일하다. 여기서는 그 최적점까지
	// 제대로 수렴시킨다. 참조 구현의 scipy 는 기본 ftol 때문에 |g|max 0.6~1.0 에서
	// 멈추지만, 그 정지 지점을 흉내내지 않는다 — 실거래 모델은 최적점에 있는 편이
	// 낫고, 실측한 차이는 확률 1.2e-3 수준으로 작다. Task 12 가 그만큼을 감안한다.
	settings := &optimize.Settings{
		GradientThreshold: 1e-8,
		MajorIterations:   2000,
		Converger:         &optimize.FunctionConverge{Absolute: 1e-12, Iterations: 50},
	}
	res, err := optimize.Minimize(problem, make([]float64, p+1), settings, &optimize.LBFGS{})
	if err != nil && res == nil {
		return nil, fmt.Errorf("최적화 실패: %w", err)
	}
	if serr := res.Status.Err(); serr != nil {
		// 기울기 1e-8 문턱은 float64 정밀도 바닥에 거의 닿아 있다. 라인서치가
		// 그 바닥에서 더는 전진하지 못해 Failure 로 끝나더라도, 마지막 위치의
		// 기울기가 이미 충분히 작다면(강볼록이므로 최적점 근방) 수렴으로 본다.
		// 흉내내는 대상은 scipy 의 이른 정지가 아니라, float64 로 갈 수 있는
		// 한계까지 실제로 밀어붙인 결과다.
		if gnorm := maxAbs(res.Gradient); gnorm > convergedGradTol {
			return nil, fmt.Errorf("최적화 상태: %v (|g|max=%.3g): %w", res.Status, gnorm, serr)
		}
	}

	coef := make([]float64, p)
	copy(coef, res.X[:p])
	return &LogReg{
		L2: l2, Coef: coef, Intercept: res.X[p],
		Mu: mu, Sd: sd, NTrain: n, FeatureNames: append([]string(nil), names...),
	}, nil
}

// logAddExp0 은 log(1 + exp(s)) 를 오버플로 없이 계산한다.
func logAddExp0(s float64) float64 {
	if s > 0 {
		return s + math.Log1p(math.Exp(-s))
	}
	return math.Log1p(math.Exp(s))
}

// convergedGradTol 은 라인서치가 float64 정밀도 바닥에서 Failure 로 끝났을 때
// "사실상 수렴"으로 인정할 |g|max 상한이다. 팀리드가 실측한 조인 설정의 도달치가
// ~1e-4 였던 것에 여유를 두었다 — scipy 의 0.57~1.04 와는 두 자릿수 이상 차이난다.
const convergedGradTol = 1e-3

func maxAbs(v []float64) float64 {
	var m float64
	for _, x := range v {
		if a := math.Abs(x); a > m {
			m = a
		}
	}
	return m
}
