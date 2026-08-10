package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// testNow는 상한 검사(7일)의 경계를 손으로 계산할 수 있도록 고정한 기준 시각이다.
// time.Now()를 쓰면 경계 바로 위/아래 표본이 실행 시점에 따라 뒤집힌다.
var testNow = time.Unix(1_700_000_000, 0) // 2023-11-14T22:13:20Z

// fakeJWT은 header.payload.signature 형태의 3세그먼트 문자열을 만든다.
// header/signature는 아무 값이나 상관없다 — jwtExpiry는 payload만 본다(서명
// 검증을 하지 않는다).
func fakeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return "header." + payload + ".signature"
}

func TestJWTExpiryReadsExpClaim(t *testing.T) {
	want := testNow.Add(24 * time.Hour).Unix()
	tok := fakeJWT(t, map[string]any{"exp": want, "iat": want - 86400, "sub": "0xabc"})

	got, ok := jwtExpiry(tok, testNow)
	if !ok {
		t.Fatal("exp 클레임이 있는데 ok=false")
	}
	if got.Unix() != want {
		t.Errorf("exp = %v, 기대 %v", got.Unix(), want)
	}
}

func TestJWTExpiryRejectsNonThreeSegmentToken(t *testing.T) {
	for _, tok := range []string{"tok", "a.b", "a.b.c.d", ""} {
		if _, ok := jwtExpiry(tok, testNow); ok {
			t.Errorf("세그먼트 3개가 아닌 %q에서 ok=true", tok)
		}
	}
}

func TestJWTExpiryRejectsUndecodablePayload(t *testing.T) {
	// "!!!"는 base64url 알파벳에 없는 문자다.
	if _, ok := jwtExpiry("header.!!!.signature", testNow); ok {
		t.Error("base64url로 안 풀리는 payload인데 ok=true")
	}
}

func TestJWTExpiryRejectsNonJSONPayload(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte("not json"))
	if _, ok := jwtExpiry("header."+payload+".signature", testNow); ok {
		t.Error("JSON이 아닌 payload인데 ok=true")
	}
}

func TestJWTExpiryRejectsMissingOrNonPositiveExp(t *testing.T) {
	for _, claims := range []map[string]any{
		{"sub": "0xabc"}, // exp 없음
		{"exp": 0},       // 0
		{"exp": -100},    // 음수
	} {
		tok := fakeJWT(t, claims)
		if _, ok := jwtExpiry(tok, testNow); ok {
			t.Errorf("claims=%v인데 ok=true", claims)
		}
	}
}

// 상한 검사의 경계 세 점. 검사가 통째로 빠지면 "7일+1초"가 통과하고,
// 부등호가 >= 로 뒤집히면 "정확히 7일"이 거부된다 — 둘 다 여기서 잡힌다.
func TestJWTExpirySevenDayUpperBound(t *testing.T) {
	const sevenDays = 7 * 24 * time.Hour
	cases := []struct {
		name   string
		exp    time.Time
		wantOK bool
	}{
		{"7일 1초 전", testNow.Add(sevenDays - time.Second), true},
		{"정확히 7일", testNow.Add(sevenDays), true},
		{"7일 1초 후", testNow.Add(sevenDays + time.Second), false},
		{"10년 후", testNow.Add(10 * 365 * 24 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := fakeJWT(t, map[string]any{"exp": tc.exp.Unix()})
			got, ok := jwtExpiry(tok, testNow)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, 기대 %v (exp=%v)", ok, tc.wantOK, tc.exp)
			}
			if ok && got.Unix() != tc.exp.Unix() {
				t.Errorf("exp = %v, 기대 %v", got.Unix(), tc.exp.Unix())
			}
		})
	}
}

// 이미 지난 exp는 상한 검사와 무관하게 그대로 통과한다 — 만료된 토큰을
// "만료됐다"고 보고하는 것이 옳고, 그래야 호출자가 즉시 재발급한다.
// 상한 검사가 실수로 Before/After를 함께 거르면 여기서 잡힌다.
func TestJWTExpiryAcceptsPastExpiry(t *testing.T) {
	past := testNow.Add(-time.Hour)
	tok := fakeJWT(t, map[string]any{"exp": past.Unix()})
	got, ok := jwtExpiry(tok, testNow)
	if !ok {
		t.Fatal("이미 지난 exp인데 ok=false — 재발급 판단을 호출자가 못 한다")
	}
	if got.Unix() != past.Unix() {
		t.Errorf("exp = %v, 기대 %v", got.Unix(), past.Unix())
	}
}
