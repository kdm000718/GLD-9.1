package claim

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// DummySignature 는 가스 산정용 자리표시 서명이다.
//
// zd_sponsorUserOperation 은 **서명을 검증하지 않지만 길이는 센다** — 서명이
// 짧으면 verificationGasLimit 이 실제보다 작게 나오고, 그 값으로 보낸
// UserOperation 은 검증 중에 가스가 떨어져 실패한다. 그래서 65바이트짜리
// 더미를 넣는다. HAR 에서 predict.fun 웹이 보낸 것과 **같은 바이트**다.
const DummySignature = "0xfffffffffffffffffffffffffffffff0000000000000000000000000000000007aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1c"

// UserOp 는 EntryPoint v0.7 의 UserOperation 이다.
//
// 페이마스터 필드가 없는 것은 누락이 아니다. predict.fun 의 ZeroDev
// 엔드포인트는 `provider=ULTRA_RELAY` 로 **번들러 층에서** 후원하므로
// paymasterAndData 가 비고 가스 가격이 0 이다(HAR 실측:
// `actualGasCost: "0x0"`). 페이마스터를 채우면 골든 벡터와 달라진다.
type UserOp struct {
	Sender               string
	Nonce                *big.Int
	CallData             []byte
	CallGasLimit         *big.Int
	VerificationGasLimit *big.Int
	PreVerificationGas   *big.Int
	MaxFeePerGas         *big.Int
	MaxPriorityFeePerGas *big.Int
	Signature            []byte
}

// Hash 는 EntryPoint v0.7 의 userOpHash 다.
//
// v0.6 과 다르다: v0.7 은 verificationGasLimit·callGasLimit 을 한 워드에
// 상위/하위 16바이트로 **묶고**(accountGasLimits), maxPriorityFeePerGas·
// maxFeePerGas 도 같은 방식으로 묶는다(gasFees). 순서를 뒤집으면 해시가
// 달라지고, 그러면 번들러가 서명을 거부한다 — 그 실패는 "키가 틀렸다"와
// 구분되지 않는다.
//
// initCode 와 paymasterAndData 는 이 경로에서 항상 비어 있다(계정은 이미
// 배포돼 있고 페이마스터는 번들러가 대신한다). keccak256("") 을 그대로 넣는다.
func (u UserOp) Hash(entryPoint string, chainID int64) ([]byte, error) {
	sender, err := normalizeAddress(u.Sender)
	if err != nil {
		return nil, fmt.Errorf("sender 주소: %w", err)
	}
	ep, err := normalizeAddress(entryPoint)
	if err != nil {
		return nil, fmt.Errorf("EntryPoint 주소: %w", err)
	}
	for name, v := range map[string]*big.Int{
		"nonce": u.Nonce, "callGasLimit": u.CallGasLimit,
		"verificationGasLimit": u.VerificationGasLimit,
		"preVerificationGas":   u.PreVerificationGas,
		"maxFeePerGas":         u.MaxFeePerGas,
		"maxPriorityFeePerGas": u.MaxPriorityFeePerGas,
	} {
		if v == nil {
			return nil, fmt.Errorf("%s 가 비어 있다 — 가스 산정 응답을 반영하지 않고 해시를 계산하려 한다", name)
		}
	}

	var enc strings.Builder
	enc.WriteString(leftPad(sender))
	enc.WriteString(word(u.Nonce))
	enc.WriteString(hex.EncodeToString(crypto.Keccak256(nil)))        // initCode
	enc.WriteString(hex.EncodeToString(crypto.Keccak256(u.CallData))) // callData
	enc.WriteString(packHiLo(u.VerificationGasLimit, u.CallGasLimit)) // accountGasLimits
	enc.WriteString(word(u.PreVerificationGas))
	enc.WriteString(packHiLo(u.MaxPriorityFeePerGas, u.MaxFeePerGas)) // gasFees
	enc.WriteString(hex.EncodeToString(crypto.Keccak256(nil)))        // paymasterAndData

	encoded, err := hex.DecodeString(enc.String())
	if err != nil {
		return nil, fmt.Errorf("UserOperation 인코딩: %w", err)
	}
	inner := crypto.Keccak256(encoded)

	var outer strings.Builder
	outer.WriteString(hex.EncodeToString(inner))
	outer.WriteString(leftPad(ep))
	outer.WriteString(word(big.NewInt(chainID)))
	b, err := hex.DecodeString(outer.String())
	if err != nil {
		return nil, fmt.Errorf("userOpHash 인코딩: %w", err)
	}
	return crypto.Keccak256(b), nil
}

// Signer 는 32바이트 해시에 서명한다. `auth.Signer` 가 그대로 만족한다.
//
// **개인키를 이 패키지로 들이지 않는다.** internal/kernel 이 같은 선택을 한
// 이유와 같다 — 키를 다루는 곳이 늘어날수록 로그·에러 메시지로 새는 경로가
// 늘어난다. 여기로 오는 것은 서명 능력뿐이다.
type Signer interface {
	SignHash(h []byte) ([]byte, error)
	Address() common.Address
}

// SignUserOpHash 는 userOpHash 에 서명한다.
//
// # 명세가 여기서 틀렸다
//
// `docs/auto-claim-spec.md` §6 은 "Kernel 봉투로 감싼다 —
// order.SignForPredictAccount 를 그대로 쓸 수 있다. 평문 65바이트 서명은
// 거부된다" 라고 적었다. **HAR 이 그 반대를 증명한다.** 실제로 보내진 서명은
// 봉투 없는 65바이트이고, 그것을 EIP-191 개인서명 해시로 복구하면 우리 키의
// EOA(0x701f6b98…)가 나온다. 골든 벡터가 명세의 추측을 잡은 자리다.
//
// 이유는 nonce key 에 있다: 검증 타입이 0x00(루트)이므로 Kernel 이 서명을
// 봉투로 해석하지 않고 루트 검증자(ECDSA)에게 원문 그대로 넘긴다
// ([NonceKey] 참조). 주문 서명(EIP-712 + Kernel 봉투)과는 다른 경로다.
//
// 서명 대상은 userOpHash **자체가 아니라** 그것의 EIP-191 개인서명 해시다 —
// HAR 에서 웹이 `personal_sign(userOpHash)` 를 부른 것이 이것이고,
// [RecoverSigner] 가 같은 규칙으로 복구해 대조한다.
func SignUserOpHash(userOpHash []byte, s Signer) ([]byte, error) {
	if len(userOpHash) != 32 {
		return nil, fmt.Errorf("userOpHash 는 32바이트여야 한다 (받은 길이 %d)", len(userOpHash))
	}
	if s == nil {
		return nil, fmt.Errorf("서명자가 없다")
	}
	sig, err := s.SignHash(accounts.TextHash(userOpHash))
	if err != nil {
		return nil, fmt.Errorf("userOpHash 서명: %w", err)
	}
	if len(sig) != 65 {
		return nil, fmt.Errorf("서명 길이가 65가 아니다 (%d)", len(sig))
	}
	// v 는 27/28 이어야 한다. go-ethereum 의 원시 서명은 0/1 을 주고, 그대로
	// 보내면 컨트랙트가 복구에 실패한다 — auth.Signer 는 이미 보정하므로
	// 여기서 더하면 29/30 이 된다. 그래서 더하지 않고 **확인만** 한다.
	if sig[64] != 27 && sig[64] != 28 {
		return nil, fmt.Errorf("서명의 v 가 27/28 이 아니다 (%d) — 서명자가 보정하지 않았다", sig[64])
	}
	return sig, nil
}

// RecoverSigner 는 서명에서 EOA 를 복구한다. **보내기 전 자가 대조용이다** —
// 우리가 서명한 것이 우리 계정의 등록 서명자인지 확인하지 않고 보내면,
// 거부 응답을 받고 나서야 키가 틀렸다는 것을 알게 된다.
func RecoverSigner(userOpHash, sig []byte) (string, error) {
	if len(sig) != 65 {
		return "", fmt.Errorf("서명 길이가 65가 아니다 (%d)", len(sig))
	}
	rs := make([]byte, 65)
	copy(rs, sig)
	if rs[64] >= 27 {
		rs[64] -= 27
	}
	pub, err := crypto.SigToPub(accounts.TextHash(userOpHash), rs)
	if err != nil {
		return "", fmt.Errorf("서명자 복구: %w", err)
	}
	return strings.ToLower(crypto.PubkeyToAddress(*pub).Hex()), nil
}

// SignerAddress 는 서명자의 EOA 를 소문자 0x 표기로 돌려준다.
func SignerAddress(s Signer) string {
	return strings.ToLower(s.Address().Hex())
}

// packHiLo 는 두 값을 32바이트 워드에 상위 16 / 하위 16 바이트로 넣는다.
// 각 값이 16바이트를 넘으면 조용히 잘리지 않고 에러 대신 panic 이다 —
// 가스 한도가 2^128 을 넘는 일은 응답 파싱이 깨졌다는 뜻이고, 그때 잘라
// 보내면 잘못된 해시에 유효한 서명을 붙이게 된다.
func packHiLo(hi, lo *big.Int) string {
	h, l := hi.Text(16), lo.Text(16)
	if len(h) > 32 || len(l) > 32 {
		panic(fmt.Sprintf("가스 필드가 16바이트를 넘는다: hi=%s lo=%s", h, l))
	}
	return strings.Repeat("0", 32-len(h)) + h + strings.Repeat("0", 32-len(l)) + l
}

// checksum 은 로그용 EIP-55 표기다.
func checksum(addr string) string { return common.HexToAddress(addr).Hex() }
