package kernel

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/sha3"
)

// 하드코딩한 셀렉터를 실제 keccak과 대조한다. 이 상수가 틀리면 eth_call이
// 빈 결과나 엉뚱한 값을 돌려주는데, 그 실패는 "서명자가 없다"와 구분되지
// 않는다 — 계정이 멀쩡한데 "등록된 서명자가 없다"고 보고하게 된다.
func TestSelectorMatchesKeccak(t *testing.T) {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte("ecdsaValidatorStorage(address)"))
	want := hex.EncodeToString(h.Sum(nil)[:4])
	if ecdsaValidatorStorageSelector != want {
		t.Errorf("셀렉터 = %s, keccak 앞 4바이트는 %s", ecdsaValidatorStorageSelector, want)
	}
}

func TestCallDataLayout(t *testing.T) {
	// 대문자·체크섬 표기로 넣어도 소문자로 정규화돼야 한다.
	got, err := CallData("0xAAAAbbbbCCCCddddEEEEffff0000111122223333")
	if err != nil {
		t.Fatal(err)
	}
	want := "0x20709efc" + "000000000000000000000000" + "aaaabbbbccccddddeeeeffff0000111122223333"
	if got != want {
		t.Errorf("calldata =\n  %s\n기대\n  %s", got, want)
	}
	// 4바이트 셀렉터 + 32바이트 인자 = 36바이트 = hex 72자 + "0x"
	if len(got) != 2+72 {
		t.Errorf("calldata 길이 %d, 기대 %d", len(got), 2+72)
	}
}

func TestCallDataRejectsBadAccount(t *testing.T) {
	for _, bad := range []string{
		"",
		"0x",
		"0xdeadbeef", // 짧다
		"0xAAAAbbbbCCCCddddEEEEffff00001111222233334", // 41자
		"0xZZZZbbbbCCCCddddEEEEffff0000111122223333",  // hex 아님
	} {
		if _, err := CallData(bad); err == nil {
			t.Errorf("%q 를 받아들였다", bad)
		}
	}
}

func TestNormalizeAddressAcceptsBothPrefixForms(t *testing.T) {
	want := "aaaabbbbccccddddeeeeffff0000111122223333"
	for _, in := range []string{
		"0xAAAAbbbbCCCCddddEEEEffff0000111122223333",
		"AAAAbbbbCCCCddddEEEEffff0000111122223333",
		"  0xaaaabbbbccccddddeeeeffff0000111122223333  ",
	} {
		got, err := NormalizeAddress(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Errorf("%q → %q, 기대 %q", in, got, want)
		}
	}
}

func word(hexBody string) string {
	return "0x" + strings.Repeat("0", 64-len(hexBody)) + hexBody
}

func TestDecodeAddressResult(t *testing.T) {
	addr, zero, err := DecodeAddressResult(word("1111111111111111111111111111111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	if zero {
		t.Fatal("0 주소로 판정했다")
	}
	if addr != "1111111111111111111111111111111111111111" {
		t.Errorf("주소 = %s", addr)
	}
}

// 전부 0인 결과는 "등록된 서명자가 없다"는 사실이지 파싱 실패가 아니다.
// 이걸 에러로 뭉뚱그리면 "계정 주소가 틀렸다"와 "서명자가 안 걸려 있다"를
// 호출자가 구분할 수 없다.
func TestDecodeAddressResultZeroIsDistinct(t *testing.T) {
	addr, zero, err := DecodeAddressResult(word(""))
	if err != nil {
		t.Fatalf("0 주소를 에러로 처리했다: %v", err)
	}
	if !zero {
		t.Error("zero=false")
	}
	if addr != "" {
		t.Errorf("주소 = %q, 기대 빈 문자열", addr)
	}
}

// 상위 12바이트가 0이 아니면 그 반환값은 주소가 아니다 — 잘못된 셀렉터나
// 다른 컨트랙트를 부른 것이다. 뒤 20바이트만 잘라 쓰면 쓰레기를 "등록된
// 서명자"라고 보고하고, 그 뒤 판정이 전부 무의미해진다.
func TestDecodeAddressResultRejectsDirtyHighBytes(t *testing.T) {
	dirty := "0x" + "0000000000000000000000ff" + "1111111111111111111111111111111111111111"
	if _, _, err := DecodeAddressResult(dirty); err == nil {
		t.Error("상위 바이트가 더러운 워드를 받아들였다")
	}
}

func TestDecodeAddressResultRejectsWrongLength(t *testing.T) {
	for _, bad := range []string{
		"0x",
		"0x1111111111111111111111111111111111111111", // 20바이트만
		"0x" + strings.Repeat("0", 63),               // 1자 짧다
		"0x" + strings.Repeat("0", 65),               // 1자 길다
		"0x" + strings.Repeat("z", 64),               // hex 아님
	} {
		if _, _, err := DecodeAddressResult(bad); err == nil {
			t.Errorf("%q 를 받아들였다", bad)
		}
	}
}

// 대소문자만 다른 같은 주소를 "다르다"고 판정하면, 멀쩡한 키를 두고 계정을
// 새로 만들게 된다. 정규화가 그것을 막는지 본다.
// 픽스처에 반드시 **글자가 섞여 있어야** 한다. 0x1111… 처럼 숫자만 있는
// 자리표시자를 쓰면 대소문자 변환이 아무것도 바꾸지 않아, 정규화를 통째로
// 지워도 이 테스트가 통과한다 — 검사하려던 것을 검사하지 않게 된다.
func TestMatchIsCaseInsensitiveAfterNormalize(t *testing.T) {
	const upper = "0xAbCdEf0123456789aBcDeF0123456789AbCdEf01"
	const lower = "abcdef0123456789abcdef0123456789abcdef01"

	a, err := NormalizeAddress(upper)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := DecodeAddressResult(word(lower))
	if err != nil {
		t.Fatal(err)
	}
	if !Match(a, b) {
		t.Errorf("같은 주소를 다르다고 판정했다: %s vs %s", a, b)
	}
}

func TestMatchRejectsDifferentAddresses(t *testing.T) {
	if Match("2222222222222222222222222222222222222222", "1111111111111111111111111111111111111111") {
		t.Error("다른 주소를 같다고 판정했다 — 이 판정이 뒤집히면 잘못된 키로 승인·실주문을 진행하게 된다")
	}
}

// 빈 문자열 둘은 "일치"가 아니다. 조회 실패로 양쪽이 비었을 때 통과로
// 읽히면 안 된다.
func TestMatchRejectsEmpty(t *testing.T) {
	if Match("", "") {
		t.Error("빈 문자열 둘을 일치로 판정했다")
	}
}

// --- Verify: 체인 왕복까지 포함한 판정 ---------------------------------------

const (
	fixtureAccount  = "0xAAAAbbbbCCCCddddEEEEffff0000111122223333"
	fixtureSignerHi = "0xAbCdEf0123456789aBcDeF0123456789AbCdEf01"
	fixtureSignerLo = "abcdef0123456789abcdef0123456789abcdef01"
	otherSigner     = "0x9999999999999999999999999999999999999999"
)

// rpcServer 는 eth_call 하나에 답하는 가짜 노드다. 요청 본문을 검사에 쓸 수
// 있게 잡아 둔다 — calldata 가 실제로 나가는지 확인하지 않으면, 셀렉터를
// 통째로 잘못 보내면서 "일치"를 받는 경로가 열린다.
func rpcServer(t *testing.T, result string, gotData *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(b, &req); err == nil && len(req.Params) > 0 {
			var call struct {
				To   string `json:"to"`
				Data string `json:"data"`
			}
			if json.Unmarshal(req.Params[0], &call) == nil && gotData != nil {
				*gotData = call.Data
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"`+result+`"}`)
	}))
}

func TestVerifyAcceptsRegisteredSigner(t *testing.T) {
	var data string
	srv := rpcServer(t, word(fixtureSignerLo), &data)
	defer srv.Close()

	v := Verifier{RPC: srv.URL, HTTP: srv.Client()}
	if err := v.Verify(context.Background(), fixtureAccount, fixtureSignerHi); err != nil {
		t.Fatalf("일치인데 에러다: %v", err)
	}
	want, err := CallData(fixtureAccount)
	if err != nil {
		t.Fatal(err)
	}
	if data != want {
		t.Errorf("나간 calldata = %s, 기대 %s", data, want)
	}
}

// 불일치는 전용 타입이어야 한다. 종료 코드가 갈려야 스크립트가 "키가 틀렸다"와
// "조회가 안 됐다"를 나눠 다룰 수 있다.
func TestVerifyReportsMismatchAsTypedError(t *testing.T) {
	srv := rpcServer(t, word(strings.TrimPrefix(otherSigner, "0x")), nil)
	defer srv.Close()

	v := Verifier{RPC: srv.URL, HTTP: srv.Client()}
	err := v.Verify(context.Background(), fixtureAccount, fixtureSignerHi)
	var m *MismatchError
	if !errors.As(err, &m) {
		t.Fatalf("불일치가 *MismatchError 가 아니다: %v", err)
	}
	if m.Derived == m.Registered {
		t.Error("에러가 같은 주소 둘을 담고 있다")
	}
}

// 0 주소는 "서명자가 없다"는 사실이고 불일치와 다르다. 둘을 같은 타입으로
// 뭉치면 "계정이 아직 배포 안 됨"과 "키가 틀림"이 같은 조치를 받는다.
func TestVerifyReportsMissingSignerSeparately(t *testing.T) {
	srv := rpcServer(t, word(""), nil)
	defer srv.Close()

	v := Verifier{RPC: srv.URL, HTTP: srv.Client()}
	err := v.Verify(context.Background(), fixtureAccount, fixtureSignerHi)
	var n *NoSignerError
	if !errors.As(err, &n) {
		t.Fatalf("0 주소가 *NoSignerError 가 아니다: %v", err)
	}
	var m *MismatchError
	if errors.As(err, &m) {
		t.Error("0 주소를 불일치로도 판정했다 — 두 상태가 겹치면 안 된다")
	}
}

// RPC 가 200 과 함께 error 필드를 주는 경우. 이걸 놓치면 result 가 빈 문자열인
// 채로 파싱 단계까지 가서 엉뚱한 에러 메시지가 나온다.
func TestVerifyRejectsRPCErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"execution reverted"}}`)
	}))
	defer srv.Close()

	v := Verifier{RPC: srv.URL, HTTP: srv.Client()}
	err := v.Verify(context.Background(), fixtureAccount, fixtureSignerHi)
	if err == nil {
		t.Fatal("RPC 에러인데 통과했다")
	}
	if !strings.Contains(err.Error(), "execution reverted") {
		t.Errorf("사유가 에러에 없다: %v", err)
	}
	var m *MismatchError
	if errors.As(err, &m) {
		t.Error("조회 실패를 불일치로 판정했다 — 멀쩡한 키를 폐기하게 만든다")
	}
}

// 잘못된 유도 주소(예: 배선 실수로 빈 문자열)를 넘기면 조회를 하기 전에
// 막아야 한다. 빈 주소로 조회해 0 주소를 받고 "서명자 없음"으로 보고하면
// 원인이 통째로 가려진다.
func TestVerifyRejectsBadDerivedAddressBeforeCalling(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = io.WriteString(w, `{"result":"`+word("")+`"}`)
	}))
	defer srv.Close()

	v := Verifier{RPC: srv.URL, HTTP: srv.Client()}
	if err := v.Verify(context.Background(), fixtureAccount, ""); err == nil {
		t.Fatal("빈 유도 주소를 받아들였다")
	}
	if called {
		t.Error("잘못된 입력으로 RPC 를 불렀다")
	}
}
