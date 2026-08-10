package auth

import "testing"

// predict.fun 은 모든 REST 응답을 {"data": …, "success": true} 로 감싼다
// (실측: 2026-08-09, testnet). unwrapEnvelope 는 응답을 디코딩한 직후 한 번만
// 불러 봉투를 벗기고, 그 뒤의 모든 필드 접근(pickString, expiresIn)은 평평한
// 맵에서 하게 만든다. 이 파일이 이전 라운드의 pickString 봉투 테스트를
// 대체한다 — 커버리지는 그대로 옮겼다(실측 메시지 봉투 / JWT 세 후보명 ×
// 봉투 / 봉투 없음 / 빈 봉투 에러 / 후보·봉투 분리 증명).

func TestUnwrapEnvelopeThenPickStringFindsMessage(t *testing.T) {
	m := map[string]any{
		"data":    map[string]any{"message": "sign this, Timestamp: 123"},
		"success": true,
	}
	got, err := pickString(unwrapEnvelope(m), "message", "nonce")
	if err != nil {
		t.Fatalf("pickString: %v", err)
	}
	if got != "sign this, Timestamp: 123" {
		t.Errorf("got %q", got)
	}
}

// 회귀 테스트다 — 예전 구현(라운드 2 이전)은 "data" 가 후보 키 목록에 있을
// 때만 봉투를 벗겼는데, token/jwt/accessToken 후보에는 "data" 가 없어서 이
// 케이스가 조용히 실패했다.
func TestUnwrapEnvelopeThenPickStringFindsTokenCandidates(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"token", map[string]any{"data": map[string]any{"token": "eyJhbG.xxx"}, "success": true}, "eyJhbG.xxx"},
		{"jwt", map[string]any{"data": map[string]any{"jwt": "eyJhbG.yyy"}, "success": true}, "eyJhbG.yyy"},
		{"accessToken", map[string]any{"data": map[string]any{"accessToken": "eyJhbG.www"}, "success": true}, "eyJhbG.www"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := pickString(unwrapEnvelope(c.body), "token", "jwt", "accessToken")
			if err != nil {
				t.Fatalf("pickString: %v — 봉투 안 %s 를 못 찾았다 (회귀)", err, c.name)
			}
			if got != c.want {
				t.Errorf("got %q, 기대 %q", got, c.want)
			}
		})
	}
}

func TestUnwrapEnvelopeReturnsOriginalWhenNoEnvelope(t *testing.T) {
	m := map[string]any{"token": "eyJhbG.zzz"}
	got, err := pickString(unwrapEnvelope(m), "token", "jwt", "accessToken")
	if err != nil {
		t.Fatalf("pickString: %v", err)
	}
	if got != "eyJhbG.zzz" {
		t.Errorf("got %q", got)
	}
}

// 빈 봉투는 벗겨도 빈 맵이라 pickString 이 에러를 낸다.
func TestUnwrapEnvelopeThenPickStringErrorsOnEmptyData(t *testing.T) {
	m := map[string]any{"success": true, "data": map[string]any{}}
	if got, err := pickString(unwrapEnvelope(m), "token", "jwt", "accessToken"); err == nil {
		t.Errorf("빈 data 에서 에러 없이 %q 를 돌려줬다", got)
	}
}

// 후보 키가 봉투 이름("data")과 전혀 겹치지 않아도 벗겨져야 한다 — 봉투
// 처리가 pickString 의 후보 목록과 완전히 분리됐다는 증거. unwrapEnvelope 는
// pickString 을 부르기 전에 이미 끝나 있으므로 후보가 뭐든 상관없다.
func TestUnwrapEnvelopeIndependentOfCandidates(t *testing.T) {
	m := map[string]any{"data": map[string]any{"weirdFieldName": "value-under-envelope"}}
	got, err := pickString(unwrapEnvelope(m), "weirdFieldName")
	if err != nil {
		t.Fatalf("pickString: %v", err)
	}
	if got != "value-under-envelope" {
		t.Errorf("got %q", got)
	}
}

// data 가 맵이 아니면(배열 등) 패닉하지 않고 원본을 그대로 돌려준다.
func TestUnwrapEnvelopeReturnsOriginalWhenDataIsNotAMap(t *testing.T) {
	m := map[string]any{"data": []any{1, 2, 3}}
	got := unwrapEnvelope(m)
	arr, ok := got["data"].([]any)
	if !ok || len(arr) != 3 {
		t.Errorf("data 가 배열인데 원본을 안 돌려줬다: %#v", got)
	}
}

func TestUnwrapEnvelopeHandlesDoubleEnvelope(t *testing.T) {
	m := map[string]any{"data": map[string]any{"data": map[string]any{"token": "double-wrapped"}}}
	got, err := pickString(unwrapEnvelope(m), "token")
	if err != nil {
		t.Fatalf("이중 봉투를 못 벗겼다: %v", err)
	}
	if got != "double-wrapped" {
		t.Errorf("got %q", got)
	}
}
