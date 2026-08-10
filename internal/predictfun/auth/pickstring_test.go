package auth

import "testing"

// pickString 은 후보 키만 본다 — 봉투 처리는 unwrapEnvelope 가 응답을 받은
// 직후 미리 끝낸다 (unwrapenvelope_test.go 참고). 여기서는 봉투 없는 평평한
// 맵만 다룬다.

func TestPickStringFindsFirstMatchingCandidate(t *testing.T) {
	m := map[string]any{"jwt": "eyJhbG.yyy"}
	got, err := pickString(m, "token", "jwt", "accessToken")
	if err != nil {
		t.Fatalf("pickString: %v", err)
	}
	if got != "eyJhbG.yyy" {
		t.Errorf("got %q", got)
	}
}

func TestPickStringSkipsEmptyStringCandidate(t *testing.T) {
	m := map[string]any{"token": "", "jwt": "eyJhbG.real"}
	got, err := pickString(m, "token", "jwt")
	if err != nil {
		t.Fatalf("pickString: %v", err)
	}
	if got != "eyJhbG.real" {
		t.Errorf("빈 문자열 후보를 골랐다: got %q", got)
	}
}

// 빈 응답에서 빈 토큰을 조용히 돌려주면 안 된다 — 서명은 성공했는데 빈
// Authorization 헤더로 주문이 거부되고, 원인이 로그에서 안 보인다.
func TestPickStringErrorsWhenNothingMatches(t *testing.T) {
	m := map[string]any{"unrelated": "x"}
	if got, err := pickString(m, "token", "jwt"); err == nil {
		t.Errorf("아무 후보도 없는데 %q 를 돌려줬다", got)
	}
}

func TestPickStringErrorsOnEmptyMap(t *testing.T) {
	if got, err := pickString(map[string]any{}, "token", "jwt"); err == nil {
		t.Errorf("빈 맵에서 에러 없이 %q 를 돌려줬다", got)
	}
}
