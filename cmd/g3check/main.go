// Command g3check 는 Go 서명 경로가 predict.fun TS SDK 와 비트 단위로 같은지
// 판정한다 (게이트 G3).
//
// testdata/sdk_signatures.json 에는 실제 @predictdotfun/sdk 를 호출해 뽑은
// 골든 벡터가 들어 있다 — EOA 경로 5개, predictAccount(Kernel 스마트 계정)
// 경로 4개. 이 커맨드는 같은 입력을 Go 의 order.Hash / order.KernelDigest /
// order.SignEOA / order.SignForPredictAccount 에 넣어 SDK 출력과 완전 일치하는지
// 본다. 다이제스트나 서명이 1비트라도 다르면 거래소가 주문을 거부하거나,
// 더 나쁘면 의도와 다른 주문이 체결된다.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"

	"github.com/ethereum/go-ethereum/common"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
)

// ecdsaValidatorHex 는 predict.fun ECDSA_VALIDATOR 주소다 — BNB 메인넷과
// 테스트넷이 같은 값을 쓴다 (스펙 §6). 골든 파일에는 이 주소가 없다 — 봉투
// 바이트 안에 이미 박혀 있는 값이라, 검증하려면 우리 쪽에서 독립적으로 알고
// 있는 값이 있어야 한다. 여기서만 쓰는 우리 Go 상수다 — 골든을 만드는 TS
// 스크립트가 쓰는 SDK 상수와 출처가 다르다.
const ecdsaValidatorHex = "0x845ADb2C711129d4f3966735eD98a9F09fC4cE57"

// testSignerHex 는 go-ethereum 문서에 실린 공개 테스트 키다(주소
// 0x27000F84214f79B0600aa86841958b13ac98242a). 골든 생성 스크립트가 쓴 것과
// 같은 키라야 서명이 일치한다. 실지갑 키가 아니므로 자금을 보내면 안 된다.
const testSignerHex = "4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3"

type goldenDomain struct {
	Name              string `json:"name"`
	Version           string `json:"version"`
	ChainID           int64  `json:"chainId"`
	VerifyingContract string `json:"verifyingContract"`
}

type goldenOrder struct {
	Salt          string `json:"salt"`
	Maker         string `json:"maker"`
	Signer        string `json:"signer"`
	Taker         string `json:"taker"`
	TokenID       string `json:"tokenId"`
	MakerAmount   string `json:"makerAmount"`
	TakerAmount   string `json:"takerAmount"`
	Expiration    string `json:"expiration"`
	Nonce         string `json:"nonce"`
	FeeRateBps    string `json:"feeRateBps"`
	Side          uint8  `json:"side"`
	SignatureType uint8  `json:"signatureType"`
}

type goldenVector struct {
	Label          string       `json:"label"`
	Kind           string       `json:"kind"` // "eoa" 또는 "predictAccount"
	Domain         goldenDomain `json:"domain"`
	Order          goldenOrder  `json:"order"`
	OrderDigest    string       `json:"orderDigest"`
	PredictAccount *string      `json:"predictAccount"`
	KernelDigest   *string      `json:"kernelDigest"`
	Signature      string       `json:"signature"`
}

func main() {
	path := flag.String("golden", "testdata/sdk_signatures.json", "SDK 골든 벡터 파일")
	flag.Parse()

	if err := run(*path); err != nil {
		fmt.Fprintln(os.Stderr, "판정: 실패 — G3")
		fmt.Fprintln(os.Stderr, "사유:", err)
		os.Exit(1)
	}
	fmt.Println("판정: 통과 — G3")
}

func run(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("골든 파일을 읽지 못했다: %w", err)
	}
	var vectors []goldenVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		return fmt.Errorf("골든 파일을 파싱하지 못했다: %w", err)
	}
	if len(vectors) == 0 {
		return fmt.Errorf("골든 벡터가 0개다")
	}

	signer, err := auth.NewSigner(testSignerHex)
	if err != nil {
		return fmt.Errorf("테스트 서명자를 만들지 못했다: %w", err)
	}
	ecdsaValidator := common.HexToAddress(ecdsaValidatorHex)

	var compared, comparedEOA, comparedKernel int
	// orderDigest 종수는 kind 별로 따로 센다 — 9개 전체를 한 무더기로 세면 eoa
	// 5개가 전부 같은 다이제스트(입력이 해시에 안 들어간 상태)여도 predictAccount
	// 4개가 서로 다르기만 하면 통과해버린다. 각 무리 안에서 따로 갈려야 그
	// 무리의 입력이 실제로 해시에 반영됐다는 증거가 된다.
	distinctOrderDigestsEOA := map[string]bool{}
	distinctOrderDigestsPA := map[string]bool{}
	distinctKernelDigests := map[string]bool{}

	fmt.Printf("[골든] %d개 벡터, 서명자 %s\n", len(vectors), signer)

	for _, v := range vectors {
		o, d, err := toOrderAndDomain(v)
		if err != nil {
			return fmt.Errorf("%s: 골든 파싱 실패: %w", v.Label, err)
		}

		gotDigest, err := order.Hash(o, d)
		if err != nil {
			return fmt.Errorf("%s: order.Hash 실패: %w", v.Label, err)
		}
		wantDigest, err := decodeHex(v.OrderDigest)
		if err != nil {
			return fmt.Errorf("%s: orderDigest 파싱 실패: %w", v.Label, err)
		}
		if !bytesEqual(gotDigest, wantDigest) {
			return fmt.Errorf("%s: orderDigest 불일치\n  got  %s\n  want %s",
				v.Label, hexOf(gotDigest), hexOf(wantDigest))
		}

		wantSig, err := decodeHex(v.Signature)
		if err != nil {
			return fmt.Errorf("%s: signature 파싱 실패: %w", v.Label, err)
		}

		switch v.Kind {
		case "eoa":
			if v.PredictAccount != nil || v.KernelDigest != nil {
				return fmt.Errorf("%s: eoa 벡터인데 predictAccount/kernelDigest 필드가 채워져 있다", v.Label)
			}
			gotSig, err := order.SignEOA(o, d, signer)
			if err != nil {
				return fmt.Errorf("%s: order.SignEOA 실패: %w", v.Label, err)
			}
			if len(gotSig) != 65 {
				return fmt.Errorf("%s: EOA 서명 %d바이트, 기대 65", v.Label, len(gotSig))
			}
			if !bytesEqual(gotSig, wantSig) {
				return fmt.Errorf("%s: EOA 서명 불일치\n  got  %s\n  want %s",
					v.Label, hexOf(gotSig), hexOf(wantSig))
			}
			comparedEOA++
			distinctOrderDigestsEOA[hexOf(gotDigest)] = true
			fmt.Printf("  eoa             %-32s order=%s sig=%s\n", v.Label, short(gotDigest), short(gotSig))

		case "predictAccount":
			if v.PredictAccount == nil || v.KernelDigest == nil {
				return fmt.Errorf("%s: predictAccount 벡터인데 predictAccount/kernelDigest 필드가 비었다", v.Label)
			}
			predictAccount := common.HexToAddress(*v.PredictAccount)

			gotKernel, err := order.KernelDigest(gotDigest, d.ChainID, predictAccount)
			if err != nil {
				return fmt.Errorf("%s: order.KernelDigest 실패: %w", v.Label, err)
			}
			wantKernel, err := decodeHex(*v.KernelDigest)
			if err != nil {
				return fmt.Errorf("%s: kernelDigest 파싱 실패: %w", v.Label, err)
			}
			if !bytesEqual(gotKernel, wantKernel) {
				return fmt.Errorf("%s: kernelDigest 불일치\n  got  %s\n  want %s",
					v.Label, hexOf(gotKernel), hexOf(wantKernel))
			}
			distinctKernelDigests[hexOf(gotKernel)] = true

			gotSig, err := order.SignForPredictAccount(gotDigest, d.ChainID, predictAccount, ecdsaValidator, signer)
			if err != nil {
				return fmt.Errorf("%s: order.SignForPredictAccount 실패: %w", v.Label, err)
			}
			// 86바이트 봉투 형식을 여기서 고정한다 — 길이만 맞고 앞 21바이트가
			// 틀리면 체인에서야 드러난다.
			if len(gotSig) != 86 {
				return fmt.Errorf("%s: 봉투 %d바이트, 기대 86", v.Label, len(gotSig))
			}
			if gotSig[0] != 0x01 {
				return fmt.Errorf("%s: 봉투 첫 바이트 0x%02x, 기대 0x01", v.Label, gotSig[0])
			}
			if got := common.BytesToAddress(gotSig[1:21]); got != ecdsaValidator {
				return fmt.Errorf("%s: 봉투의 validator %s, 기대 %s", v.Label, got, ecdsaValidator)
			}
			if !bytesEqual(gotSig, wantSig) {
				return fmt.Errorf("%s: predictAccount 서명 불일치\n  got  %s\n  want %s",
					v.Label, hexOf(gotSig), hexOf(wantSig))
			}
			comparedKernel++
			distinctOrderDigestsPA[hexOf(gotDigest)] = true
			fmt.Printf("  predictAccount  %-32s order=%s kernel=%s sig=%s\n",
				v.Label, short(gotDigest), short(gotKernel), short(gotSig))

		default:
			return fmt.Errorf("%s: 알 수 없는 kind %q", v.Label, v.Kind)
		}
		compared++
	}

	fmt.Printf("[대조] 총 %d개 (eoa %d / predictAccount %d)\n", compared, comparedEOA, comparedKernel)

	// --- 판정 규칙. 산문이 아니라 코드로 강제한다. ---

	if compared == 0 {
		return fmt.Errorf("대조한 벡터가 없다")
	}
	// 현재 루프 구조에서는 실패마다 즉시 return 하므로 이 분기는 오늘 도달할
	// 방법이 없다 — 그래도 지우지 않는다. 나중에 누가 실패 케이스를 continue 로
	// 바꿔 넘기는 실수를 하면 이 줄이 잡아준다.
	if compared != len(vectors) {
		return fmt.Errorf("골든 %d개 중 %d개만 대조했다 — 일부가 조용히 스킵됐다", len(vectors), compared)
	}
	// orderDigest 종수는 kind 별로 따로 본다 — eoa 5개가 전부 같은 다이제스트여도
	// predictAccount 4개가 서로 다르면 "전체 2종 이상"은 만족해버려 그 결함을
	// 놓친다. 각 무리 안에서 최소 2종이어야 그 무리의 입력이 해시에 반영된
	// 증거가 된다.
	if comparedEOA >= 2 && len(distinctOrderDigestsEOA) < 2 {
		return fmt.Errorf("eoa 벡터의 orderDigest 가 %d종뿐이다(%d개 중) — 입력이 해시에 반영되지 않는다",
			len(distinctOrderDigestsEOA), comparedEOA)
	}
	if comparedKernel >= 2 && len(distinctOrderDigestsPA) < 2 {
		return fmt.Errorf("predictAccount 벡터의 orderDigest 가 %d종뿐이다(%d개 중) — 입력이 해시에 반영되지 않는다",
			len(distinctOrderDigestsPA), comparedKernel)
	}
	// 두 서명 경로를 대칭으로 지킨다 — 한쪽만 막으면 다른 쪽이 통째로 빠진
	// 골든도 통과해버린다(predictAccount 만 있는 골든이 실제로 그랬다). eoa 는
	// order.SignEOA 경로, predictAccount 는 order.SignForPredictAccount +
	// order.KernelDigest 경로이자 실제 운용 경로다.
	//
	// 정확한 개수(5/4)를 상수로 박지 않고 0 만 막는다 — 박아두면 골든에 벡터를
	// 추가할 때마다 이 파일도 고쳐야 한다. 대신 위의 kind 별 orderDigest 종수
	// 검사(각 무리 2종 이상)가 "개수는 있지만 다 겹친다"는 퇴화를 잡아준다.
	// 정확히 1개로 줄어드는 경우까지는 못 잡지만, 그건 경로가 통째로 빠지는
	// 것보다 훨씬 약한 결함이고 골든 형태 자체는 Step 1 의 파이썬 스크립트가
	// 별도로 개수(5/4)를 셈으로써 잡는다.
	if comparedEOA == 0 {
		return fmt.Errorf("eoa 벡터를 하나도 대조하지 않았다 — order.SignEOA 경로가 검증되지 않았다")
	}
	if comparedKernel == 0 {
		return fmt.Errorf("predictAccount 벡터를 하나도 대조하지 않았다 — 실제 운용 경로가 검증되지 않았다")
	}
	if len(distinctKernelDigests) < comparedKernel {
		return fmt.Errorf("kernelDigest 가 %d종뿐인데 predictAccount 벡터는 %d개다 — 계정·체인이 다이제스트에 반영되지 않는다",
			len(distinctKernelDigests), comparedKernel)
	}

	return nil
}

// toOrderAndDomain 은 골든 JSON 의 문자열 필드를 order.Order/order.Domain 으로 바꾼다.
func toOrderAndDomain(v goldenVector) (order.Order, order.Domain, error) {
	salt, err := bigFromString(v.Order.Salt)
	if err != nil {
		return order.Order{}, order.Domain{}, fmt.Errorf("salt: %w", err)
	}
	tokenID, err := bigFromString(v.Order.TokenID)
	if err != nil {
		return order.Order{}, order.Domain{}, fmt.Errorf("tokenId: %w", err)
	}
	makerAmount, err := bigFromString(v.Order.MakerAmount)
	if err != nil {
		return order.Order{}, order.Domain{}, fmt.Errorf("makerAmount: %w", err)
	}
	takerAmount, err := bigFromString(v.Order.TakerAmount)
	if err != nil {
		return order.Order{}, order.Domain{}, fmt.Errorf("takerAmount: %w", err)
	}
	expiration, err := bigFromString(v.Order.Expiration)
	if err != nil {
		return order.Order{}, order.Domain{}, fmt.Errorf("expiration: %w", err)
	}
	nonce, err := bigFromString(v.Order.Nonce)
	if err != nil {
		return order.Order{}, order.Domain{}, fmt.Errorf("nonce: %w", err)
	}
	feeRateBps, err := bigFromString(v.Order.FeeRateBps)
	if err != nil {
		return order.Order{}, order.Domain{}, fmt.Errorf("feeRateBps: %w", err)
	}

	o := order.Order{
		Salt:          salt,
		Maker:         v.Order.Maker,
		Signer:        v.Order.Signer,
		Taker:         v.Order.Taker,
		TokenID:       tokenID,
		MakerAmount:   makerAmount,
		TakerAmount:   takerAmount,
		Expiration:    expiration,
		Nonce:         nonce,
		FeeRateBps:    feeRateBps,
		Side:          v.Order.Side,
		SignatureType: v.Order.SignatureType,
	}
	d := order.Domain{
		Name:              v.Domain.Name,
		Version:           v.Domain.Version,
		ChainID:           v.Domain.ChainID,
		VerifyingContract: v.Domain.VerifyingContract,
	}
	return o, d, nil
}

func bigFromString(s string) (*big.Int, error) {
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("10진수가 아니다: %q", s)
	}
	return n, nil
}

func decodeHex(s string) ([]byte, error) {
	if len(s) < 2 || s[0:2] != "0x" {
		return nil, fmt.Errorf("0x 접두사가 없다: %q", s)
	}
	b := common.FromHex(s)
	if len(b) == 0 && s != "0x" {
		return nil, fmt.Errorf("16진수 디코딩 실패: %q", s)
	}
	return b, nil
}

func hexOf(b []byte) string { return "0x" + common.Bytes2Hex(b) }

func short(b []byte) string {
	h := hexOf(b)
	if len(h) > 18 {
		return h[:18]
	}
	return h
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
