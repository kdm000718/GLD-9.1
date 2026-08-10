package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleModel() *LogReg {
	return &LogReg{
		L2: 10, Coef: []float64{0.25, -1.5}, Intercept: 0.125,
		Mu: []float64{1, 2}, Sd: []float64{3, 4}, NTrain: 1234,
		FeatureNames: []string{"a", "b"},
	}
}

func TestSaveLoadRoundTripIsExact(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	orig := sampleModel()
	if err := Save(p, orig); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	x := []float32{7, 9}
	if got.Logit(x) != orig.Logit(x) {
		t.Fatalf("왕복 후 logit 이 다르다: %v vs %v", got.Logit(x), orig.Logit(x))
	}
	if got.NTrain != orig.NTrain || got.L2 != orig.L2 {
		t.Errorf("메타데이터가 왕복하지 않았다")
	}
}

func TestLoadRejectsFeatureNameMismatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	if err := Save(p, sampleModel()); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p, []string{"a", "c"})
	if err == nil {
		t.Fatal("피처 이름이 다른데 로드가 성공했다 — 유일한 근거 불변식이 안 지켜진다")
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("어느 위치가 다른지 알려주지 않는다: %v", err)
	}
}

// 위 테스트는 불일치 인덱스(1)가 len(wantNames)-1(=1) 과 같아, 구현이 실제
// 인덱스 대신 len(wantNames)-1 을 하드코딩해 리포트해도 통과한다. 불일치 위치가
// 마지막이 아닌 케이스로 그 오검출을 막는다.
func TestLoadRejectsFeatureNameMismatchNotAtLastIndex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	m := &LogReg{
		L2: 10, Coef: []float64{0.1, 0.2, 0.3}, Intercept: 0,
		Mu: []float64{0, 0, 0}, Sd: []float64{1, 1, 1}, NTrain: 1,
		FeatureNames: []string{"a", "x", "c"}, // 불일치는 인덱스 1, len-1 은 2
	}
	if err := Save(p, m); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p, []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("피처 이름이 다른데 로드가 성공했다")
	}
	if !strings.Contains(err.Error(), "1번째") {
		t.Errorf("실제 불일치 위치(1)를 짚지 않는다: %v", err)
	}
	if strings.Contains(err.Error(), "2번째") {
		t.Errorf("마지막 인덱스(2)를 하드코딩해 리포트한 것으로 보인다: %v", err)
	}
}

// Python 의 models.json 을 그대로 주면 encoding/json 이 모르는 필드를 무시해
// 전 필드가 제로값이 된다 — 에러 없이. 원인을 짚는 메시지가 나와야 한다.
func TestLoadRejectsPythonMapShape(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	body := `{"k0":{"l2":10,"coef":[1,2],"intercept":0,"mu":[0,0],"sd":[1,1],` +
		`"n_train":1,"feature_names":["a","b"]},"k3":{}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p, []string{"a", "b"})
	if err == nil {
		t.Fatal("Python 맵 형태를 받아들였다 — 전 필드가 제로값인 모델이 통과했다")
	}
	if !strings.Contains(err.Error(), "k0") || !strings.Contains(err.Error(), "k3") {
		t.Errorf("원인을 짚지 않는다: %v", err)
	}
}

// 위 테스트는 최상위 키가 우연히 "k0" 라 통과한다 — 메시지의 "k0" 가 실제로
// 읽은 값이 아니라 하드코딩된 예시 문구일 수도 있다. 다른 키 이름으로 실제
// 파싱을 검증한다.
func TestLoadRejectsPythonMapShapeReportsActualKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	body := `{"foo":{"l2":10,"coef":[1,2],"intercept":0,"mu":[0,0],"sd":[1,1],` +
		`"n_train":1,"feature_names":["a","b"]},"bar":{}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p, []string{"a", "b"})
	if err == nil {
		t.Fatal("Python 맵 형태를 받아들였다")
	}
	if !strings.Contains(err.Error(), "foo") || !strings.Contains(err.Error(), "bar") {
		t.Errorf("실제 최상위 키를 짚지 않는다: %v", err)
	}
	if strings.Contains(err.Error(), "k0") {
		t.Errorf("하드코딩된 예시 문구가 그대로 나온 것으로 보인다: %v", err)
	}
}

// sampleModel 은 FeatureNames/Coef/Mu/Sd 길이가 항상 같이 붙어다녀서, guard
// 2(개수 비교)가 없어도 뒤의 배열 길이 가드가 우연히 같은 실패를 대신 잡는다.
// guard 2 만 고립시키려면 feature_names 는 3개, coef/mu/sd 는 wantNames 와
// 같은 2개로 만들어야 한다 — 배열 길이 가드가 침묵하는 상태에서 guard 2 만이
// 이걸 막는다.
func TestLoadRejectsFeatureCountMismatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	body := `{"l2":10,"coef":[1,2],"intercept":0,"mu":[1,2],"sd":[1,1],"n_train":1,"feature_names":["a","b","c"]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p, []string{"a", "b"})
	if err == nil {
		t.Fatal("피처 개수가 다른데 로드가 성공했다")
	}
	if !strings.Contains(err.Error(), "피처 개수 3") || !strings.Contains(err.Error(), "기대 2") {
		t.Errorf("개수를 짚지 않는다: %v", err)
	}
}

func TestLoadRejectsInconsistentArrayLengths(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	// coef 2개인데 mu 1개 — Logit 이 인덱스 범위를 넘는다
	body := `{"l2":10,"coef":[1,2],"intercept":0,"mu":[1],"sd":[1,1],"n_train":1,"feature_names":["a","b"]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, []string{"a", "b"}); err == nil {
		t.Fatal("coef 와 mu 길이가 다른데 로드가 성공했다")
	}
}

func TestLoadRejectsZeroSd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	body := `{"l2":10,"coef":[1,1],"intercept":0,"mu":[0,0],"sd":[1,0],"n_train":1,"feature_names":["a","b"]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, []string{"a", "b"}); err == nil {
		t.Fatal("sd 에 0 이 있는데 로드가 성공했다 — Logit 이 Inf 를 낸다")
	}
}
