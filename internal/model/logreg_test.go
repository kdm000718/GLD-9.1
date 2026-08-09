package model

import (
	"encoding/json"
	"math"
	"math/rand"
	"testing"

	"gonum.org/v1/gonum/optimize"
)

func TestSigmoidIsStableAtExtremes(t *testing.T) {
	if got := Sigmoid(0); math.Abs(got-0.5) > 1e-15 {
		t.Errorf("Sigmoid(0) = %v", got)
	}
	if got := Sigmoid(1000); got != 1 {
		t.Errorf("Sigmoid(1000) = %v, 기대 1", got)
	}
	if got := Sigmoid(-1000); got != 0 {
		t.Errorf("Sigmoid(-1000) = %v, 기대 0", got)
	}
	if math.IsNaN(Sigmoid(-745)) || math.IsNaN(Sigmoid(745)) {
		t.Error("극단값에서 NaN 이 나온다")
	}
}

// 선형 분리 가능한 인공 데이터에서 계수 부호가 맞아야 한다.
func TestFitRecoversSignal(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	n, p := 4000, 3
	m := NewMatrix(n, p)
	y := make([]float64, n)
	rows := make([]int, n)
	for i := 0; i < n; i++ {
		x0 := r.NormFloat64()
		x1 := r.NormFloat64()
		x2 := r.NormFloat64() // 무관한 피처
		m.SetRow(i, []float64{x0, x1, x2})
		z := 1.5*x0 - 1.0*x1
		if r.Float64() < Sigmoid(z) {
			y[i] = 1
		}
		rows[i] = i
	}
	lr, err := Fit(m, rows, y, []string{"a", "b", "c"}, 1.0)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if lr.Coef[0] <= 0 {
		t.Errorf("a 계수 = %v, 양수여야 한다", lr.Coef[0])
	}
	if lr.Coef[1] >= 0 {
		t.Errorf("b 계수 = %v, 음수여야 한다", lr.Coef[1])
	}
	if math.Abs(lr.Coef[2]) > math.Abs(lr.Coef[0]) {
		t.Errorf("무관한 피처 계수가 너무 크다: %v", lr.Coef[2])
	}
	if lr.NTrain != n {
		t.Errorf("NTrain = %d, 기대 %d", lr.NTrain, n)
	}
}

// L2 를 키우면 계수가 0 쪽으로 수축해야 한다.
func TestStrongerL2ShrinksCoefficients(t *testing.T) {
	r := rand.New(rand.NewSource(8))
	n := 2000
	m := NewMatrix(n, 2)
	y := make([]float64, n)
	rows := make([]int, n)
	for i := 0; i < n; i++ {
		x0, x1 := r.NormFloat64(), r.NormFloat64()
		m.SetRow(i, []float64{x0, x1})
		if r.Float64() < Sigmoid(2*x0) {
			y[i] = 1
		}
		rows[i] = i
	}
	weak, err := Fit(m, rows, y, []string{"a", "b"}, 0.1)
	if err != nil {
		t.Fatalf("Fit(약한 L2): %v", err)
	}
	strong, err := Fit(m, rows, y, []string{"a", "b"}, 1000.0)
	if err != nil {
		t.Fatalf("Fit(강한 L2): %v", err)
	}
	if math.Abs(strong.Coef[0]) >= math.Abs(weak.Coef[0]) {
		t.Errorf("L2 를 키웠는데 수축하지 않았다: %v vs %v", strong.Coef[0], weak.Coef[0])
	}
}

// 표준화 통계는 학습 구간에서만 나와야 한다 (테스트 통계 누수 방지).
func TestStandardisationUsesTrainingRowsOnly(t *testing.T) {
	m := NewMatrix(4, 1)
	m.SetRow(0, []float64{0})
	m.SetRow(1, []float64{2})
	m.SetRow(2, []float64{1000}) // 학습에 안 쓰는 행
	m.SetRow(3, []float64{1000})
	y := []float64{0, 1, 1, 1}
	lr, err := Fit(m, []int{0, 1}, y, []string{"a"}, 1.0)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if math.Abs(lr.Mu[0]-1.0) > 1e-9 {
		t.Errorf("Mu = %v, 학습 행 {0,2} 의 평균 1 이어야 한다", lr.Mu[0])
	}
}

// 표준편차가 0 인 피처는 1 로 대체해 0 나눗셈을 막는다.
func TestZeroVarianceFeatureGetsUnitSd(t *testing.T) {
	m := NewMatrix(3, 2)
	for i := 0; i < 3; i++ {
		m.SetRow(i, []float64{5, float64(i)})
	}
	lr, err := Fit(m, []int{0, 1, 2}, []float64{0, 1, 1}, []string{"const", "x"}, 1.0)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	if lr.Sd[0] != 1.0 {
		t.Errorf("분산 0 피처의 Sd = %v, 기대 1", lr.Sd[0])
	}
	if math.IsNaN(lr.Prob(m.Row(0))) {
		t.Error("Prob 가 NaN 이다")
	}
}

func TestJSONRoundTripMatchesPythonSchema(t *testing.T) {
	lr := &LogReg{
		L2: 10, Coef: []float64{0.1, -0.2}, Intercept: 0.05,
		Mu: []float64{1, 2}, Sd: []float64{3, 4},
		NTrain: 123, FeatureNames: []string{"a", "b"},
	}
	b, err := json.Marshal(lr)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// Python models.json 의 키와 같아야 한다
	var raw map[string]any
	json.Unmarshal(b, &raw)
	for _, k := range []string{"l2", "coef", "intercept", "mu", "sd", "n_train", "feature_names"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("키 %q 가 없다 — Python models.json 과 호환되지 않는다", k)
		}
	}
	var back LogReg
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Intercept != lr.Intercept || back.NTrain != lr.NTrain || back.Coef[1] != lr.Coef[1] {
		t.Errorf("왕복 불일치: %+v", back)
	}
}

// Python models.json 을 그대로 읽어 같은 확률을 내는지 확인한다.
func TestLoadsPythonModelsJSON(t *testing.T) {
	const sample = `{"l2":10.0,"coef":[0.5,-0.25],"intercept":0.1,
	  "mu":[0.0,0.0],"sd":[1.0,1.0],"n_train":100,"feature_names":["a","b"]}`
	var lr LogReg
	if err := json.Unmarshal([]byte(sample), &lr); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got := lr.Logit([]float32{2, 4})
	want := 0.5*2 - 0.25*4 + 0.1 // = 0.1
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("Logit = %v, 기대 %v", got, want)
	}
}

// y 는 위치가 아니라 원본 행 인덱스로 정렬돼 있다: 학습 라벨은 y[rows[i]] 여야
// 하고 y[i] 면 버그다. rows 를 항등이 아닌 순열로 주고, 각 행의 부호와
// 라벨이 원래는 완벽히 일치하도록 데이터를 짰다 — y[i] 를 쓰면(자리만 다른
// 라벨을 갖다 붙이면) 정확히 반대 상관관계가 돼서 계수 부호가 뒤집힌다.
func TestFitIndexesLabelsByOriginalRowNotByPosition(t *testing.T) {
	xs := []float64{-3, 3, -2, 2, -1, 1}
	y := []float64{0, 1, 0, 1, 0, 1} // y[r] = 1 ⇔ xs[r] > 0
	m := NewMatrix(len(xs), 1)
	for i, x := range xs {
		m.SetRow(i, []float64{x})
	}
	rows := []int{1, 0, 3, 2, 5, 4} // 항등이 아닌 순열 (인접 쌍을 맞바꿈)
	lr, err := Fit(m, rows, y, []string{"x"}, 0.01)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	// y[rows[i]] 규약: 위치 순서대로 라벨이 [1,0,1,0,1,0] 이고 x 도 같은
	// 순서로 [3,-3,2,-2,1,-1] → "x>0 이면 라벨 1"이 유지된다 → 계수 양수.
	// y[i] 규약(버그): 라벨이 [0,1,0,1,0,1] 로 자리만 따라가서 같은 x 순서와
	// 정확히 반대로 짝지어진다 → 계수 음수로 뒤집힌다.
	if lr.Coef[0] <= 0 {
		t.Errorf("Coef[0] = %v, 양수여야 한다 — y 가 rows 위치가 아니라 원본 행 인덱스로 인덱싱됐는지 확인", lr.Coef[0])
	}
}

// sd 는 모표준편차(ddof=0)다. ddof=1(표본표준편차)이면 값이 달라진다.
func TestSdUsesPopulationNotSampleStdDev(t *testing.T) {
	m := NewMatrix(3, 1)
	m.SetRow(0, []float64{0})
	m.SetRow(1, []float64{2})
	m.SetRow(2, []float64{4})
	// 평균 2, 편차 {-2,0,2}, 제곱합 8.
	// ddof=0: sqrt(8/3) = 1.63299316185545...
	// ddof=1: sqrt(8/2) = 2.0  (다른 값 — 이 테스트가 구분해야 한다)
	lr, err := Fit(m, []int{0, 1, 2}, []float64{0, 0, 1}, []string{"x"}, 1.0)
	if err != nil {
		t.Fatalf("Fit: %v", err)
	}
	want := math.Sqrt(8.0 / 3.0)
	if math.Abs(lr.Sd[0]-want) > 1e-9 {
		t.Errorf("Sd = %v, 기대(ddof=0) %v — ddof=1 로 계산됐다면 %v 가 나온다", lr.Sd[0], want, math.Sqrt(8.0/2.0))
	}
}

// gradient 가 objective 의 실제 그라디언트인지 유한차분으로 직접 검증한다.
// grad[p] += r(절편 항)를 빼먹는 등 어느 성분이 빠져도 이 테스트가 잡는다.
func TestGradientMatchesFiniteDifference(t *testing.T) {
	r := rand.New(rand.NewSource(42))
	n, p := 60, 4
	z := make([]float64, n*p)
	yy := make([]float64, n)
	for i := range z {
		z[i] = r.NormFloat64()
	}
	for i := range yy {
		if r.Float64() < 0.5 {
			yy[i] = 1
		}
	}
	l2 := 2.5
	w := make([]float64, p+1)
	for i := range w {
		w[i] = r.NormFloat64()
	}
	grad := make([]float64, p+1)
	negLogLossGrad(grad, w, z, yy, l2, n, p)

	const h = 1e-6
	for k := 0; k < len(w); k++ {
		wp := append([]float64(nil), w...)
		wm := append([]float64(nil), w...)
		wp[k] += h
		wm[k] -= h
		fd := (negLogLoss(wp, z, yy, l2, n, p) - negLogLoss(wm, z, yy, l2, n, p)) / (2 * h)
		if diff := math.Abs(fd - grad[k]); diff > 1e-4 {
			t.Errorf("grad[%d] = %v, 유한차분 = %v (차이 %v)", k, grad[k], fd, diff)
		}
	}
}

// checkConverged 는 gonum 의 Status 딱지가 아니라 기울기를 직접 본다.
// Status.Err() == nil ("정상" 상태) 이라도 기울기가 크면 거부해야 한다.
func TestCheckConvergedRejectsLargeGradientEvenOnSuccessStatus(t *testing.T) {
	res := &optimize.Result{Status: optimize.Success}
	res.Gradient = []float64{10, 10, 10}
	if err := checkConverged(res, 2, 10); err == nil {
		t.Error("Status 가 Success 라는 이유만으로 큰 기울기를 통과시켰다")
	}
}

// Status.Err() != nil(Failure) 이어도 그 지점의 기울기가 이미 충분히 작으면
// (float64 정밀도 바닥에서 라인서치가 멈춘 경우) 수렴으로 인정해야 한다.
func TestCheckConvergedAcceptsSmallGradientEvenOnFailureStatus(t *testing.T) {
	res := &optimize.Result{Status: optimize.Failure}
	res.Gradient = []float64{1e-9, -1e-9, 1e-9}
	if err := checkConverged(res, 2, 1000); err != nil {
		t.Errorf("기울기가 충분히 작은데도 거부했다: %v", err)
	}
}

// Gradient 가 채워지지 않은 채(nil) 끝난 결과는 |g|max 가 우연히 0 으로
// 계산돼 무조건 통과해 버리면 안 된다 — 시작점이 NaN/Inf 로 발산한 경우
// 정확히 이 모양(길이 0)으로 끝난다.
func TestCheckConvergedRejectsMissingGradient(t *testing.T) {
	res := &optimize.Result{Status: optimize.Failure}
	if err := checkConverged(res, 2, 1000); err == nil {
		t.Error("기울기가 없는(nil) 결과를 수렴으로 인정했다")
	}
}

// 허용 상한은 n 에 비례해야 한다 — 절대 상수면 작은 학습 구간에서는 헐겁고
// 100만행 규모에서는 너무 빡빡해서 실패하는 최악의 형태가 된다. gradTolPerSample
// 상수 자체를 참조해서 검증한다 — 이 값이 나중에 다시 조정돼도(실제로 이미
// 한 번 바뀌었다) 이 테스트는 "n 에 비례한다"는 성질만 계속 확인한다.
func TestCheckConvergedToleranceScalesWithN(t *testing.T) {
	small, large := 1000, 1_000_000
	// small 기준 상한의 2배인 기울기 — small 에서는 거부, large 에서는 통과해야 한다.
	gnorm := gradTolPerSample * float64(small) * 2
	res := &optimize.Result{Status: optimize.GradientThreshold}
	res.Gradient = []float64{gnorm, 0, 0}
	if err := checkConverged(res, 2, small); err == nil {
		t.Errorf("|g|max=%v 가 n=%d 상한(%v)의 2배인데 통과했다", gnorm, small, gradTolPerSample*float64(small))
	}
	if err := checkConverged(res, 2, large); err != nil {
		t.Errorf("|g|max=%v 는 n=%d 상한(%v) 안쪽인데 거부했다: %v", gnorm, large, gradTolPerSample*float64(large), err)
	}
}
