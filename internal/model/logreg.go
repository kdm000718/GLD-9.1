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
//
// y 는 위치가 아니라 원본 행렬 행 인덱스로 정렬된 전체 길이 배열이다 — 즉
// i번째 학습 표본의 라벨은 y[i] 가 아니라 y[rows[i]] 다. rows 가 원본 순서와
// 다르게(비연속·재정렬) 주어져도 각 행이 자기 라벨을 정확히 찾아가도록 하는
// 설계다. Task 11 은 이 규약에 의존한다.
func Fit(mat *Matrix, rows []int, y []float64, names []string, l2 float64) (*LogReg, error) {
	n, p := len(rows), mat.Cols
	if n == 0 {
		return nil, fmt.Errorf("학습 표본이 없습니다")
	}
	if len(names) != p {
		return nil, fmt.Errorf(
			"피처 이름 %d개, 행렬 열 %d개 — 이름과 열이 어긋나면 계수가 엉뚱한 "+
				"피처에 붙는다", len(names), p)
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
	objective := func(w []float64) float64 { return negLogLoss(w, z, yy, l2, n, p) }
	gradient := func(grad, w []float64) { negLogLossGrad(grad, w, z, yy, l2, n, p) }

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
	if res == nil {
		return nil, fmt.Errorf("최적화 실패: %w", err)
	}
	if cerr := checkConverged(res, p, n); cerr != nil {
		if err != nil {
			return nil, fmt.Errorf("최적화 결과 거부 (status=%v): %w: %w", res.Status, cerr, err)
		}
		return nil, fmt.Errorf("최적화 결과 거부 (status=%v): %w", res.Status, cerr)
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

// negLogLoss 는 표준화된 설계행렬 z(n×p, 행 우선), 라벨 yy 에 대한 L2 정규화
// 음의 로그가능도다. w = [계수 p개, 절편]. 절편은 정규화에서 제외한다.
// Fit 밖으로 뺀 이유는 gradient 와 함께 유한차분으로 직접 검증하기 위해서다.
func negLogLoss(w, z, yy []float64, l2 float64, n, p int) float64 {
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

// negLogLossGrad 는 negLogLoss 의 그라디언트다. grad 는 len(w) 로 미리 할당돼
// 있어야 한다.
func negLogLossGrad(grad, w, z, yy []float64, l2 float64, n, p int) {
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

// gradTolPerSample 은 checkConverged 가 요구하는 |g|max 상한을 표본 수 n 에
// 비례해 정한다. negLogLoss 는 n개 표본에 대한 합이라 같은 적합도라도 |g|max
// 는 n 에 거의 비례해서 커진다 — 절대 상수를 쓰면 작은 학습 구간에서는 헐겁고
// 100만행 규모에서는 너무 빡빡해서 실패하는 최악의 형태가 된다(작을 때 통과,
// 커지면 실패).
//
// 9년치 전체 파이프라인(2018~2026, walk-forward 104개 블록, n≈51,840 안팎)을
// 돌려 실측했다. 104개 블록 전부 gonum 의 종료 상태는 Failure 다 — 즉
// checkConverged 가 안전판이 아니라 사실상 유일한 품질 게이트다. 표본당
// |g|max 는 최소 2.24e-09, 중앙값 1.22e-08, 90%ile 2.48e-08, 최대 3.69e-08.
// 5e-7 은 이 최댓값을 13.5배 여유를 두고 덮는다. 참고로 scipy 는 같은
// 104블록에서 |g|max 0.57~1.04(n≈51,840 기준 표본당 1.1e-5~2e-5)에서
// 멈추는데, 5e-7 은 그 지점보다도 22배 더 빡빡하다 — "수렴 안 됨"을 잡는
// 문턱이지 정밀도 목표가 아니며, 실제 도달치는 이 상한보다 두 자릿수 이상
// 작다.
const gradTolPerSample = 5e-7

// checkConverged 는 gonum 이 반환한 결과가 실제로 쓸만한 해인지 검사한다.
// Status 딱지(Success/GradientThreshold/...)에 기대지 않고 기울기를 직접
// 보는 이유는 두 가지다.
//
//  1. Status.Err() 가 nil 인 "정상" 경로라도 |g|max 를 보장하지 않는 경우가
//     있다(예: FunctionConvergence 는 함수값 변화가 작다는 것만 보장한다).
//     그래서 모든 종료 경로에 같은 기울기 사후조건을 건다.
//  2. 시작점이 NaN/Inf 로 평가되면 gonum 은 첫 반복도 못 채운 채 Failure 로
//     끝나고, 이때 res.Gradient 는 nil 이다. maxAbs(nil) 은 0 을 반환하므로
//     길이 검사 없이 기울기만 봤다가는 "발산했는데 기울기가 0 이라 통과"라는
//     최악의 오탐(라벨/피처에 NaN 이 섞였는데도 조용히 절편 0 짜리 모델을
//     내보내는 것)이 생긴다. 그래서 길이부터 확인한다.
func checkConverged(res *optimize.Result, p, n int) error {
	g := res.Gradient
	if len(g) != p+1 {
		return fmt.Errorf("기울기가 채워지지 않았다(len=%d, 기대 %d) — 시작점이 발산했거나 입력에 NaN/Inf 가 섞였을 수 있다", len(g), p+1)
	}
	gnorm := maxAbs(g)
	tol := gradTolPerSample * float64(n)
	if gnorm > tol {
		return fmt.Errorf("|g|max=%.3g 가 허용치 %.3g(=%.1e × n=%d) 를 초과했다", gnorm, tol, gradTolPerSample, n)
	}
	return nil
}

func maxAbs(v []float64) float64 {
	var m float64
	for _, x := range v {
		if a := math.Abs(x); a > m {
			m = a
		}
	}
	return m
}
