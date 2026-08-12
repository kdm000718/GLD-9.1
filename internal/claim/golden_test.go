package claim

// 이 파일이 Auto-Claim 의 게이트다.
//
// `docs/auto-claim-spec.md` 가 작업 순서를 이렇게 못 박았다: HAR 의
// UserOperation 을 골든 벡터로 박고, 같은 입력으로 우리 코드가 **같은 바이트**를
// 만드는지 시험하고, 그것이 통과한 뒤에야 전송을 붙인다. 추측으로 만든
// UserOperation 에 유효한 서명을 붙여 보내는 것이 이 저장소가 실제로 값을 치른
// 실패 방식이다.
//
// testdata/golden_userop.json 은 2026-08-11 18:05:32Z 에 **성공한** claim 의
// 전문이다(BSC tx 0x83a80081…, receipt success=true). HAR 은 재취득할 수 없다.

import (
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

type goldenVector struct {
	ChainID          int64   `json:"chainId"`
	EntryPoint       string  `json:"entryPoint"`
	CTF              string  `json:"ctf"`
	Collateral       string  `json:"collateral"`
	ECDSAValidator   string  `json:"ecdsaValidator"`
	SignerEOA        string  `json:"signerEOA"`
	ConditionID      string  `json:"conditionId"`
	IndexSetsInOrder []int64 `json:"indexSetsInOrder"`
	DummySignature   string  `json:"dummySignature"`
	UserOpHash       string  `json:"userOpHash"`
	ReceiptSuccess   bool    `json:"receiptSuccess"`
	UserOp           struct {
		Sender               string `json:"sender"`
		Nonce                string `json:"nonce"`
		CallData             string `json:"callData"`
		CallGasLimit         string `json:"callGasLimit"`
		VerificationGasLimit string `json:"verificationGasLimit"`
		PreVerificationGas   string `json:"preVerificationGas"`
		MaxFeePerGas         string `json:"maxFeePerGas"`
		MaxPriorityFeePerGas string `json:"maxPriorityFeePerGas"`
		Signature            string `json:"signature"`
	} `json:"userOp"`
}

func loadGolden(t *testing.T) goldenVector {
	t.Helper()
	b, err := os.ReadFile("testdata/golden_userop.json")
	if err != nil {
		t.Fatalf("골든 벡터 읽기: %v", err)
	}
	var g goldenVector
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatalf("골든 벡터 파싱: %v", err)
	}
	if !g.ReceiptSuccess {
		t.Fatal("골든 벡터의 receipt 가 성공이 아니다 — 실패한 호출을 기준으로 삼을 수 없다")
	}
	return g
}

func hexBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil {
		t.Fatalf("hex 디코드 %q: %v", s, err)
	}
	return b
}

func mustInt(t *testing.T, s string) *big.Int {
	t.Helper()
	x, ok := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
	if !ok {
		t.Fatalf("정수 파싱 %q", s)
	}
	return x
}

// TestGoldenCallData 는 우리가 조립한 callData 가 실물과 바이트 단위로 같은지
// 본다. 이것이 어긋나면 배치 인코딩(오프셋 세 층)이 틀렸다는 뜻이고, 그
// UserOperation 은 엉뚱한 함수를 호출한다.
func TestGoldenCallData(t *testing.T) {
	g := loadGolden(t)

	execs := make([]Execution, 0, len(g.IndexSetsInOrder))
	for _, s := range g.IndexSetsInOrder {
		cd, err := RedeemCalldata(g.Collateral, g.ConditionID, []*big.Int{big.NewInt(s)})
		if err != nil {
			t.Fatalf("RedeemCalldata(indexSet=%d): %v", s, err)
		}
		execs = append(execs, Execution{Target: g.CTF, Value: big.NewInt(0), CallData: cd})
	}

	got, err := ExecuteBatchCalldata(execs)
	if err != nil {
		t.Fatalf("ExecuteBatchCalldata: %v", err)
	}
	want := hexBytes(t, g.UserOp.CallData)
	if !bytesEqual(got, want) {
		t.Fatalf("callData 가 골든 벡터와 다르다\n got 0x%x\nwant 0x%x", got, want)
	}
}

// TestGoldenNonceKey 는 우리가 만든 nonce key 가 실측 nonce 안의 key 와 같은지
// 본다. Kernel v3 는 검증자 주소를 key 에 인코딩하므로 0 을 넣으면 검증 자체가
// 다른 경로로 간다.
func TestGoldenNonceKey(t *testing.T) {
	g := loadGolden(t)

	key, err := NonceKey(g.ECDSAValidator)
	if err != nil {
		t.Fatalf("NonceKey: %v", err)
	}
	if len(key) != 24 {
		t.Fatalf("nonce key 길이 %d, 24 여야 한다", len(key))
	}

	// 실측 nonce 를 32바이트로 펴서 앞 24바이트(key)와 뒤 8바이트(시퀀스)로 나눈다.
	full := make([]byte, 32)
	n := mustInt(t, g.UserOp.Nonce).Bytes()
	copy(full[32-len(n):], n)

	if !bytesEqual(key, full[:24]) {
		t.Fatalf("nonce key 가 골든 벡터와 다르다\n got 0x%x\nwant 0x%x", key, full[:24])
	}
	if seq := new(big.Int).SetBytes(full[24:]); seq.Int64() != 5 {
		t.Fatalf("골든 벡터의 시퀀스가 5가 아니다 (%s) — 벡터가 바뀌었다", seq)
	}
}

// TestGoldenUserOpHash 는 EntryPoint v0.7 패킹 전체를 한 번에 잰다. sender·
// nonce·callData·가스 묶음·엔트리포인트·체인ID 중 **하나라도** 틀리면 해시가
// 달라진다.
func TestGoldenUserOpHash(t *testing.T) {
	g := loadGolden(t)

	op := UserOp{
		Sender:               g.UserOp.Sender,
		Nonce:                mustInt(t, g.UserOp.Nonce),
		CallData:             hexBytes(t, g.UserOp.CallData),
		CallGasLimit:         mustInt(t, g.UserOp.CallGasLimit),
		VerificationGasLimit: mustInt(t, g.UserOp.VerificationGasLimit),
		PreVerificationGas:   mustInt(t, g.UserOp.PreVerificationGas),
		MaxFeePerGas:         mustInt(t, g.UserOp.MaxFeePerGas),
		MaxPriorityFeePerGas: mustInt(t, g.UserOp.MaxPriorityFeePerGas),
	}
	got, err := op.Hash(g.EntryPoint, g.ChainID)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if want := hexBytes(t, g.UserOpHash); !bytesEqual(got, want) {
		t.Fatalf("userOpHash 가 골든 벡터와 다르다\n got 0x%x\nwant 0x%x", got, want)
	}
}

// TestGoldenSignatureIsPlainPersonalSign 은 명세 §6 의 주장("Kernel 봉투로
// 감싼다, 평문 65바이트는 거부된다")이 **틀렸다**는 것을 고정한다.
//
// 실제로 보내진 서명은 봉투 없는 65바이트이고, EIP-191 개인서명 해시로
// 복구하면 우리 키의 EOA 가 나온다. 누군가 나중에 명세를 읽고 봉투를 다시
// 붙이면 이 시험이 막는다.
func TestGoldenSignatureIsPlainPersonalSign(t *testing.T) {
	g := loadGolden(t)

	sig := hexBytes(t, g.UserOp.Signature)
	if len(sig) != 65 {
		t.Fatalf("실측 서명이 65바이트가 아니다 (%d) — 봉투가 붙어 있었다면 더 길다", len(sig))
	}
	got, err := RecoverSigner(hexBytes(t, g.UserOpHash), sig)
	if err != nil {
		t.Fatalf("RecoverSigner: %v", err)
	}
	if want := strings.ToLower(g.SignerEOA); got != want {
		t.Fatalf("복구된 서명자가 다르다\n got %s\nwant %s", got, want)
	}
}

// TestDummySignatureLength 는 가스 산정용 더미가 65바이트인지 본다. 짧으면
// verificationGasLimit 이 작게 나오고, 그 값으로 보낸 UserOperation 은 검증 중
// 가스 부족으로 실패한다.
func TestDummySignatureLength(t *testing.T) {
	g := loadGolden(t)
	if DummySignature != g.DummySignature {
		t.Fatalf("더미 서명이 골든 벡터와 다르다\n got %s\nwant %s", DummySignature, g.DummySignature)
	}
	if b := hexBytes(t, DummySignature); len(b) != 65 {
		t.Fatalf("더미 서명이 65바이트가 아니다 (%d)", len(b))
	}
}

// TestSelectorsMatchKeccak 은 하드코딩한 셀렉터 넷을 실제 keccak 과 대조한다.
// 셀렉터가 틀리면 호출이 실패가 아니라 **다른 함수 호출**이 되므로 읽어서는
// 구분되지 않는다 — internal/kernel 이 같은 이유로 같은 시험을 둔다.
func TestSelectorsMatchKeccak(t *testing.T) {
	for _, tc := range []struct{ sig, want string }{
		{"redeemPositions(address,bytes32,bytes32,uint256[])", selRedeemPositions},
		{"execute(bytes32,bytes)", selExecute},
		{"getNonce(address,uint192)", selGetNonce},
	} {
		got := hex.EncodeToString(crypto.Keccak256([]byte(tc.sig))[:4])
		if got != tc.want {
			t.Errorf("%s 셀렉터 %s, 상수는 %s", tc.sig, got, tc.want)
		}
	}
}

// TestGoldenContractsMatchConstants 는 패키지 상수가 실물과 같은지 본다.
// 주소를 손으로 옮기다 한 글자가 틀리면 redeem 이 존재하지 않는 컨트랙트로
// 간다.
func TestGoldenContractsMatchConstants(t *testing.T) {
	g := loadGolden(t)
	for _, tc := range []struct{ name, got, want string }{
		{"CTF", CTF, g.CTF},
		{"Collateral", Collateral, g.Collateral},
		{"EntryPoint", EntryPoint, g.EntryPoint},
		{"ECDSAValidator", ECDSAValidator, g.ECDSAValidator},
	} {
		if !strings.EqualFold(tc.got, tc.want) {
			t.Errorf("%s 상수 %s, 골든 벡터 %s", tc.name, tc.got, tc.want)
		}
	}
}

// TestIndexSetIsOneBased 는 결과 인덱스 변환을 고정한다. 0-based 로 읽으면
// Up 을 회수하려다 Down 을 회수하고, 그 호출은 실패하지 않고 0 을 돌려준다.
func TestIndexSetIsOneBased(t *testing.T) {
	for _, tc := range []struct {
		idx  int
		want int64
	}{{1, 1}, {2, 2}, {3, 4}} {
		got, err := IndexSet(tc.idx)
		if err != nil {
			t.Fatalf("IndexSet(%d): %v", tc.idx, err)
		}
		if got.Int64() != tc.want {
			t.Errorf("IndexSet(%d) = %s, want %d", tc.idx, got, tc.want)
		}
	}
	for _, bad := range []int{0, -1, 9} {
		if _, err := IndexSet(bad); err == nil {
			t.Errorf("IndexSet(%d) 가 통과했다 — 범위 밖을 막아야 한다", bad)
		}
	}
}

// TestGetNonceCalldataPadsKeyTo32Bytes 는 uint192 인자의 패딩을 고정한다.
// 24바이트 key 를 그대로 붙이면 calldata 가 8바이트 짧아지고, 노드는 그것을
// 거부하지 않고 **다른 key** 로 읽는다 — 실제로 이 작업 중에 한 번 그렇게 틀렸다.
func TestGetNonceCalldataPadsKeyTo32Bytes(t *testing.T) {
	key, err := NonceKey(ECDSAValidator)
	if err != nil {
		t.Fatalf("NonceKey: %v", err)
	}
	cd, err := GetNonceCalldata("0xdd5A4da03F4f7b39F574A9bEDCD8D5f2e10bEB42", key)
	if err != nil {
		t.Fatalf("GetNonceCalldata: %v", err)
	}
	// 4(셀렉터) + 32(address) + 32(uint192) = 68바이트.
	if n := (len(cd) - 2) / 2; n != 68 {
		t.Fatalf("calldata 가 %d바이트다, 68 이어야 한다: %s", n, cd)
	}
	want := "0x" + selGetNonce +
		"000000000000000000000000dd5a4da03f4f7b39f574a9bedcd8d5f2e10beb42" +
		"0000000000000000" + "0000845adb2c711129d4f3966735ed98a9f09fc4ce572711"
	if cd != want {
		t.Fatalf("getNonce calldata 가 다르다\n got %s\nwant %s", cd, want)
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
