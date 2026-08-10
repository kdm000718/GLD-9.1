// Package model 의 직렬화 경로.
//
// Python `engines/logreg.py` 의 to_dict/from_dict 와 키가 같아야 한다. 학습은 Go 로
// 하지만, Python 이 만든 models.json 을 읽어 대조할 수 있어야 진단이 된다.
package model

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// Validate 는 로드된 모델이 실제로 쓸 수 있는 상태인지 본다.
//
// wantNames 대조가 핵심이다. 피처 순서는 features.FeatureNames 가 유일한 근거인데,
// 그 불변식은 학습 시점에만 강제되고 예측 시점에는 아무도 확인하지 않았다. 모델을
// 새로 학습한 뒤 피처를 하나 추가하면, 옛 models.json 이 조용히 로드되어 60개 계수를
// 엉뚱한 피처에 곱한다. 확률은 여전히 0~1 사이라 눈에 띄지 않는다.
func (m *LogReg) Validate(wantNames []string) error {
	if len(m.FeatureNames) == 0 {
		// Python 의 models.json 은 {"k0": {...}, "k3": {...}} 형태의 맵이다.
		// 그것을 LogReg 로 언마샬하면 encoding/json 이 모르는 필드를 조용히 무시해
		// 전 필드가 제로값이 된다. 에러가 안 나므로 여기서 짚어준다.
		return fmt.Errorf("모델이 비었다 — Python 의 models.json 은 " +
			`{"k0": {...}, "k3": {...}} 형태의 맵이라 그대로 읽히지 않는다. ` +
			"cmd/train 으로 만들거나 k0 항목만 꺼내서 저장할 것")
	}
	if len(m.FeatureNames) != len(wantNames) {
		return fmt.Errorf("피처 개수 %d, 기대 %d — 모델과 코드의 피처 집합이 다르다",
			len(m.FeatureNames), len(wantNames))
	}
	for i := range wantNames {
		if m.FeatureNames[i] != wantNames[i] {
			return fmt.Errorf("피처 이름이 %d번째에서 다르다: 모델 %q, 코드 %q",
				i, m.FeatureNames[i], wantNames[i])
		}
	}
	p := len(wantNames)
	if len(m.Coef) != p || len(m.Mu) != p || len(m.Sd) != p {
		return fmt.Errorf("배열 길이가 어긋난다: coef %d / mu %d / sd %d, 기대 %d",
			len(m.Coef), len(m.Mu), len(m.Sd), p)
	}
	for i, s := range m.Sd {
		if s == 0 || math.IsNaN(s) || math.IsInf(s, 0) {
			return fmt.Errorf("sd[%d] = %v — 표준화에서 0 나눗셈이나 비유한 값이 된다", i, s)
		}
	}
	for i, v := range m.Coef {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("coef[%d] 가 비유한 값이다: %v", i, v)
		}
	}
	for i, v := range m.Mu {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("mu[%d] 가 비유한 값이다: %v", i, v)
		}
	}
	if math.IsNaN(m.Intercept) || math.IsInf(m.Intercept, 0) {
		return fmt.Errorf("intercept 가 비유한 값이다: %v", m.Intercept)
	}
	return nil
}

// Save 는 원자적으로 쓴다 — 봇이 읽는 중에 학습이 덮어쓸 수 있다.
func Save(path string, m *LogReg) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-model-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// topLevelKeys 는 원본 JSON 바이트의 최상위 객체 키를 정렬해서 돌려준다.
// 최상위가 객체가 아니거나(배열·스칼라) 키가 없으면 nil 이다. Validate 는 원본
// 바이트를 보지 못하므로, "왜 비었나" 를 실제로 짚는 진단은 여기 Load 의 몫이다.
func topLevelKeys(b []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil || len(raw) == 0 {
		return nil
	}
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 순서가 map 순회에 좌우되면 에러 메시지가 실행마다 바뀐다
	return keys
}

// Load 는 읽은 즉시 Validate 를 건다. 검증 없이 모델을 돌려주는 경로는 두지 않는다.
func Load(path string, wantNames []string) (*LogReg, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("모델을 읽지 못했다: %w\n  cmd/train 으로 만든다", err)
	}
	var m LogReg
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("모델 JSON 파싱 실패 (%s): %w", path, err)
	}
	// Python 의 models.json 은 {"k0": {...}, "k3": {...}} 형태의 회차별 맵이다.
	// LogReg 로 언마샬하면 encoding/json 이 모르는 필드를 조용히 무시해 전 필드가
	// 제로값이 된다 — Validate 만으로는 "모델이 비었다" 는 것만 알 뿐 원인을 못
	// 짚는다. 원본 바이트에서 실제 최상위 키를 읽어 메시지에 넣어준다.
	if len(m.FeatureNames) == 0 {
		if keys := topLevelKeys(b); len(keys) > 0 {
			return nil, fmt.Errorf("모델 검증 실패 (%s): 모델이 비었다 — 최상위 키 %v 만 있고 "+
				"모델 필드가 없다. Python 의 models.json 은 회차별 맵이라 그대로 읽히지 않는다. "+
				"cmd/train 으로 만들거나 해당 항목만 꺼내서 저장할 것", path, keys)
		}
	}
	if err := m.Validate(wantNames); err != nil {
		return nil, fmt.Errorf("모델 검증 실패 (%s): %w", path, err)
	}
	return &m, nil
}
