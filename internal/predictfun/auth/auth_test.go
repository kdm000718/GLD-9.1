package auth

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
)

// 테스트 전용 키.
//
// **이 키는 go-ethereum 문서에 실린 공개 키다.** 인터넷 어디서나 볼 수 있고
// 대응 주소 0x27000F84214f79B0600aa86841958b13ac98242a 의 개인키를 누구나
// 안다. 절대 자금을 보내지 마라 — 보내는 즉시 사라진다.
const testKey = "4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3"

func TestNewSignerDerivesAddress(t *testing.T) {
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Address().Hex()
	if got != "0x27000F84214f79B0600aa86841958b13ac98242a" {
		t.Fatalf("주소 %s, 기대 0x27000F84214f79B0600aa86841958b13ac98242a", got)
	}
}

func TestNewSignerAcceptsZeroXPrefix(t *testing.T) {
	a, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSigner("0x" + testKey)
	if err != nil {
		t.Fatalf("0x 접두사를 거부했다: %v", err)
	}
	if a.Address() != b.Address() {
		t.Error("0x 접두사 유무로 주소가 달라졌다")
	}
}

func TestNewSignerRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "0x", "zzzz", testKey[:60]} {
		if _, err := NewSigner(bad); err == nil {
			t.Errorf("%q 를 받아들였다", bad)
		}
	}
}

// 개인키가 어떤 경로로도 새지 않아야 한다.
//
// **16진수만 찾으면 안 된다.** ecdsa.PrivateKey 는 비밀 스칼라를 D *big.Int 로
// 들고 있고, fmt 는 그것을 **십진수**로 찍는다. 팀리드가 실측했다 —
// fmt.Sprintf("%v", privKey) 한 줄이 `D:+3439081988824039...` 로 키 전체를
// 출력하는데, testKey 의 16진수 부분문자열을 찾는 검사는 여기서 절대 걸리지
// 않는다. 통과하면서 유출을 놓치는 검사가 된다.
//
// 그래서 금지 바늘을 세 가지로 만든다: 소문자 16진수, 대문자 16진수, 십진수.
// keyLeaks 는 out 에 개인키가 들어 있으면 걸린 바늘을, 없으면 빈 문자열을
// 돌려준다. **판정과 실패 보고를 분리한 이유**는 아래 자기검사 테스트가 이
// 함수를 직접 부를 수 있어야 하기 때문이다. t.Fatalf 를 안에서 부르면
// runtime.Goexit 이 걸려 자기검사 자체가 죽는다.
func keyLeaks(out string) string {
	d, ok := new(big.Int).SetString(testKey, 16)
	if !ok {
		panic("테스트 키를 big.Int 로 못 읽었다")
	}
	needles := []string{
		strings.ToLower(testKey)[:16],
		strings.ToUpper(testKey)[:16],
		d.String()[:16], // 십진수 표현 — fmt 가 실제로 찍는 형태
	}
	lower := strings.ToLower(out)
	for _, n := range needles {
		if strings.Contains(out, n) || strings.Contains(lower, strings.ToLower(n)) {
			return n
		}
	}
	return ""
}

func assertNoKeyLeak(t *testing.T, label, out string) {
	t.Helper()
	if n := keyLeaks(out); n != "" {
		t.Fatalf("%s 에 개인키가 들어 있다 (바늘 %q): %s", label, n, out)
	}
}

func TestSignerNeverExposesKey(t *testing.T) {
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}

	// Signer 가 만들어낼 수 있는 모든 문자열을 훑는다.
	assertNoKeyLeak(t, "String()", s.String())
	assertNoKeyLeak(t, "Address().Hex()", s.Address().Hex())
	assertNoKeyLeak(t, `Sprintf("%v")`, fmt.Sprintf("%v", s))
	assertNoKeyLeak(t, `Sprintf("%+v")`, fmt.Sprintf("%+v", s))
	assertNoKeyLeak(t, `Sprintf("%#v")`, fmt.Sprintf("%#v", s))

	// 실패 경로의 에러 메시지도 본다 — 여기서 새는 것이 가장 흔하다.
	if _, err := s.SignHash(make([]byte, 31)); err != nil {
		assertNoKeyLeak(t, "SignHash 에러", err.Error())
	}
}

// 탐지기 자체가 유효한지 확인한다. 바늘 셋을 만들어 놓고도 여전히 아무것도
// 못 잡을 수 있다 — 원래 계획서의 검사가 정확히 그랬다(16진수만 찾는데 fmt 는
// 십진수로 찍는다).
func TestKeyLeakDetectorActuallyDetects(t *testing.T) {
	d, _ := new(big.Int).SetString(testKey, 16)

	// fmt 가 실제로 만들어내는 형태.
	if keyLeaks(fmt.Sprintf("어쩌다 찍힌 값: D:+%s", d.String())) == "" {
		t.Error("십진수로 유출된 키를 못 잡았다 — 이 검사는 무의미하다")
	}
	// 16진수 두 형태도 잡아야 한다.
	if keyLeaks("key="+strings.ToLower(testKey)) == "" {
		t.Error("소문자 16진수 유출을 못 잡았다")
	}
	if keyLeaks("key="+strings.ToUpper(testKey)) == "" {
		t.Error("대문자 16진수 유출을 못 잡았다")
	}
	// 오탐도 없어야 한다 — 주소와 서명은 정상 출력이다.
	if n := keyLeaks("0x27000F84214f79B0600aa86841958b13ac98242a"); n != "" {
		t.Errorf("주소를 유출로 오탐했다 (바늘 %q)", n)
	}
}

func TestSignHashProduces65Bytes(t *testing.T) {
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	h := make([]byte, 32)
	for i := range h {
		h[i] = byte(i)
	}
	sig, err := s.SignHash(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 65 {
		t.Fatalf("서명 %d바이트, 기대 65", len(sig))
	}
	if sig[64] != 27 && sig[64] != 28 {
		t.Errorf("v = %d — 이더리움 관례는 27/28 이다", sig[64])
	}
}

func TestSignHashRejectsWrongLength(t *testing.T) {
	s, _ := NewSigner(testKey)
	if _, err := s.SignHash(make([]byte, 31)); err == nil {
		t.Error("31바이트 해시를 받아들였다")
	}
}
