package model

import (
	"encoding/json"
	"math"
	"math/rand"
	"testing"
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
	weak, _ := Fit(m, rows, y, []string{"a", "b"}, 0.1)
	strong, _ := Fit(m, rows, y, []string{"a", "b"}, 1000.0)
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
