package order

import (
	"fmt"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
)

// typedData 는 Order 를 표준 EIP-712 구조로 바꾼다. 타입 정의 12개 필드의
// 순서가 SDK 와 같아야 한다 — EIP-712 타입 해시는 필드 순서에 의존한다.
func typedData(o Order, d Domain) apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"Order": {
				{Name: "salt", Type: "uint256"},
				{Name: "maker", Type: "address"},
				{Name: "signer", Type: "address"},
				{Name: "taker", Type: "address"},
				{Name: "tokenId", Type: "uint256"},
				{Name: "makerAmount", Type: "uint256"},
				{Name: "takerAmount", Type: "uint256"},
				{Name: "expiration", Type: "uint256"},
				{Name: "nonce", Type: "uint256"},
				{Name: "feeRateBps", Type: "uint256"},
				{Name: "side", Type: "uint8"},
				{Name: "signatureType", Type: "uint8"},
			},
		},
		PrimaryType: "Order",
		Domain: apitypes.TypedDataDomain{
			Name:              d.Name,
			Version:           d.Version,
			ChainId:           math.NewHexOrDecimal256(d.ChainID),
			VerifyingContract: d.VerifyingContract,
		},
		Message: apitypes.TypedDataMessage{
			"salt": o.Salt.String(), "maker": o.Maker, "signer": o.Signer, "taker": o.Taker,
			"tokenId": o.TokenID.String(), "makerAmount": o.MakerAmount.String(),
			"takerAmount": o.TakerAmount.String(), "expiration": o.Expiration.String(),
			"nonce": o.Nonce.String(), "feeRateBps": o.FeeRateBps.String(),
			"side": fmt.Sprint(o.Side), "signatureType": fmt.Sprint(o.SignatureType),
		},
	}
}

// Hash 는 표준 Order EIP-712 다이제스트다 (Exchange 도메인).
func Hash(o Order, d Domain) ([]byte, error) {
	h, _, err := apitypes.TypedDataAndHash(typedData(o, d))
	return h, err
}

// KernelDigest 는 Order 다이제스트를 Kernel 도메인으로 감싼다.
//
// SDK 의 eip712WrapHash + hashKernelMessage 를 옮긴 것이다. 세 군데가 함정이다:
// Kernel 도메인의 verifyingContract 는 predictAccount 이지 Kernel 구현 주소가 아니고,
// 도메인에 salt 는 없으며, inner 는 Order 다이제스트를 Kernel(bytes32 hash) 구조체에
// 넣어 다시 해시한 값이다.
func KernelDigest(orderDigest []byte, chainID int64, predictAccount common.Address) ([]byte, error) {
	if len(orderDigest) != 32 {
		return nil, fmt.Errorf("Order 다이제스트는 32바이트여야 한다 (받은 길이 %d)", len(orderDigest))
	}
	typeHash := crypto.Keccak256([]byte("Kernel(bytes32 hash)"))
	inner := crypto.Keccak256(typeHash, orderDigest) // abi.encode(bytes32,bytes32) == 단순 연결

	kd := apitypes.TypedData{
		Types: apitypes.Types{"EIP712Domain": {
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
			{Name: "chainId", Type: "uint256"},
			{Name: "verifyingContract", Type: "address"},
		}},
		PrimaryType: "EIP712Domain",
		Domain: apitypes.TypedDataDomain{
			Name: "Kernel", Version: "0.3.1",
			ChainId:           math.NewHexOrDecimal256(chainID),
			VerifyingContract: predictAccount.Hex(),
		},
	}
	sep, err := kd.HashStruct("EIP712Domain", kd.Domain.Map())
	if err != nil {
		return nil, err
	}
	return crypto.Keccak256([]byte{0x19, 0x01}, sep, inner), nil
}

// SignForPredictAccount 는 Kernel 계정용 서명 봉투를 만든다.
//
// 마지막 서명이 signTypedData 가 아니라 signMessage 다 — 즉 32바이트 다이제스트에
// "\x19Ethereum Signed Message:\n32" 접두사가 한 번 더 붙는다. 이것을 빠뜨리면
// 서명이 조용히 거부된다.
//
// SignHash 가 이미 v 를 27/28 로 보정하므로 여기서 다시 더하지 않는다.
func SignForPredictAccount(orderDigest []byte, chainID int64,
	predictAccount, validator common.Address, s *auth.Signer) ([]byte, error) {

	d, err := KernelDigest(orderDigest, chainID, predictAccount)
	if err != nil {
		return nil, err
	}
	sig, err := s.SignHash(accounts.TextHash(d))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+20+65)
	out = append(out, 0x01)
	out = append(out, validator.Bytes()...)
	out = append(out, sig...)
	return out, nil
}

// SignEOA 는 predictAccount 를 쓰지 않는 경우의 서명이다 — Order 다이제스트를
// 그대로 signHash 한다 (Kernel 래핑 없음).
func SignEOA(o Order, d Domain, s *auth.Signer) ([]byte, error) {
	h, err := Hash(o, d)
	if err != nil {
		return nil, err
	}
	return s.SignHash(h)
}
