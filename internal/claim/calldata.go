package claim

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// 컨트랙트 주소. HAR 의 성공한 claim 에서 실제로 쓰인 값이다.
const (
	// CTF 는 조건부 토큰 프레임워크다. redeemPositions 를 여기로 보낸다.
	CTF = "0x22DA1810B194ca018378464a58f6Ac2B10C9d244"
	// Collateral 은 BSC USDT 다 (18 decimals — internal/onchain.USDT 와 같다).
	Collateral = "0x55d398326f99059fF775485246999027B3197955"
	// EntryPoint 는 ERC-4337 v0.7 엔트리포인트다.
	EntryPoint = "0x0000000071727De22E5E9d8BAf0edAc6f37da032"
	// ECDSAValidator 는 Kernel 계정의 ECDSA 검증자다
	// (internal/kernel.ECDSAValidator 와 같은 주소).
	ECDSAValidator = "0x845ADb2C711129d4f3966735eD98a9F09fC4cE57"
)

// 셀렉터. keccak256 앞 4바이트를 하드코딩하고 런타임에 계산하지 않는다 —
// internal/kernel 이 같은 이유로 같은 선택을 했다. 상수가 틀리면 호출이
// "실패" 가 아니라 **다른 함수 호출**이 되고, 그 실패는 읽어서는 구분되지
// 않는다. calldata_test.go 의 TestSelectorsMatchKeccak 이 실제 keccak 과
// 대조해 고정한다.
const (
	// selRedeemPositions 는
	// redeemPositions(address,bytes32,bytes32,uint256[]) 다.
	selRedeemPositions = "01b7037c"
	// selExecute 는 Kernel v3 의 execute(bytes32,bytes) 다.
	selExecute = "e9ae5c53"
	// selGetNonce 는 EntryPoint v0.7 의 getNonce(address,uint192) 다.
	selGetNonce = "35567e1a"
)

// batchExecMode 는 Kernel execute 의 execMode 첫 바이트다. 0x01 = 배치.
//
// 나머지 31바이트는 0 이다(execType=revert-on-failure, modeSelector·payload
// 없음). HAR 의 callData 가 `0xe9ae5c5301000…` 로 시작하는 것이 이것이다.
const batchExecMode = "01" + zeros62

const zeros62 = "00000000000000000000000000000000000000000000000000000000000000"

// Execution 은 Kernel 배치의 한 건이다 — (target, value, callData).
type Execution struct {
	Target   string
	Value    *big.Int
	CallData []byte
}

// RedeemCalldata 는 redeemPositions 호출의 calldata 다.
//
// parentCollectionId 는 항상 0 이다 — 이 시장들은 중첩 조건이 아니다(HAR 실측).
// indexSets 는 **결과 인덱스의 비트마스크**다: outcome index 1(Up) → 1,
// index 2(Down) → 2. [IndexSet] 이 그 변환을 한다.
func RedeemCalldata(collateral, conditionID string, indexSets []*big.Int) ([]byte, error) {
	col, err := normalizeAddress(collateral)
	if err != nil {
		return nil, fmt.Errorf("담보 주소: %w", err)
	}
	cond, err := normalizeWord(conditionID)
	if err != nil {
		return nil, fmt.Errorf("conditionId: %w", err)
	}
	if len(indexSets) == 0 {
		return nil, fmt.Errorf("indexSets 가 비었다 — 회수할 결과가 없으면 호출 자체를 만들지 않는다")
	}

	var b strings.Builder
	b.WriteString(selRedeemPositions)
	b.WriteString(leftPad(col))             // collateralToken
	b.WriteString(strings.Repeat("0", 64))  // parentCollectionId = 0
	b.WriteString(cond)                     // conditionId
	b.WriteString(word(big.NewInt(4 * 32))) // indexSets 오프셋 (동적 배열)
	b.WriteString(word(big.NewInt(int64(len(indexSets)))))
	for i, s := range indexSets {
		if s == nil || s.Sign() <= 0 {
			return nil, fmt.Errorf("indexSets[%d] 가 0 이하다 — 유효한 결과 비트마스크가 아니다", i)
		}
		b.WriteString(word(s))
	}
	return hex.DecodeString(b.String())
}

// IndexSet 은 결과 인덱스(1-based)를 CTF 비트마스크로 바꾼다.
//
// **1-based 다.** predict.fun 의 outcome.index 는 Up=1, Down=2 이고 CTF 의
// indexSet 은 `1 << (index-1)` 이다. HAR 이 이것을 고정한다: WON 이었던
// Down(index 2) 이 indexSet 2 로, LOST 였던 Up(index 1) 이 indexSet 1 로 갔다.
// 0-based 로 읽으면 Up 을 회수하려다 Down 을 회수하게 되는데, 그 호출은
// 실패하지 않는다 — 조용히 0 을 돌려준다.
func IndexSet(outcomeIndex int) (*big.Int, error) {
	if outcomeIndex < 1 || outcomeIndex > 8 {
		return nil, fmt.Errorf("결과 인덱스가 범위 밖이다 (%d) — 1..8 이어야 한다", outcomeIndex)
	}
	return new(big.Int).Lsh(big.NewInt(1), uint(outcomeIndex-1)), nil
}

// ExecuteBatchCalldata 는 Execution 목록을 Kernel 의 execute(bytes32,bytes) 로
// 감싼다.
//
// executionCalldata 는 `abi.encode(Execution[])` 이고 Execution 은
// `(address target, uint256 value, bytes callData)` 다. 동적 필드가 둘 겹쳐
// 오프셋이 세 층이므로 HAR 의 실물과 바이트 단위로 대조한다
// (claim_golden_test.go).
func ExecuteBatchCalldata(execs []Execution) ([]byte, error) {
	if len(execs) == 0 {
		return nil, fmt.Errorf("배치가 비었다 — 회수할 것이 없으면 UserOperation 을 만들지 않는다")
	}

	// 1층: Execution[] 자체를 인코딩한다.
	heads := make([]string, len(execs))
	var tails strings.Builder
	// 각 원소의 head 는 배열 길이 바로 뒤를 기준으로 한 오프셋이다.
	offset := len(execs) * 32
	for i, e := range execs {
		tgt, err := normalizeAddress(e.Target)
		if err != nil {
			return nil, fmt.Errorf("배치 %d번째 대상 주소: %w", i, err)
		}
		val := e.Value
		if val == nil {
			val = big.NewInt(0)
		}
		if val.Sign() < 0 {
			return nil, fmt.Errorf("배치 %d번째 value 가 음수다", i)
		}
		// 2층: (address,uint256,bytes) 튜플.
		var t strings.Builder
		t.WriteString(leftPad(tgt))
		t.WriteString(word(val))
		t.WriteString(word(big.NewInt(3 * 32))) // callData 오프셋
		t.WriteString(word(big.NewInt(int64(len(e.CallData)))))
		t.WriteString(padRight(hex.EncodeToString(e.CallData)))

		heads[i] = word(big.NewInt(int64(offset)))
		offset += t.Len() / 2
		tails.WriteString(t.String())
	}

	var arr strings.Builder
	arr.WriteString(word(big.NewInt(32))) // Execution[] 자체의 오프셋
	arr.WriteString(word(big.NewInt(int64(len(execs)))))
	for _, h := range heads {
		arr.WriteString(h)
	}
	arr.WriteString(tails.String())

	inner := arr.String()
	if len(inner)%2 != 0 {
		return nil, fmt.Errorf("내부 인코딩 길이가 홀수다 (%d)", len(inner))
	}

	// 3층: execute(bytes32 execMode, bytes executionCalldata).
	var b strings.Builder
	b.WriteString(selExecute)
	b.WriteString(batchExecMode)
	b.WriteString(word(big.NewInt(2 * 32))) // executionCalldata 오프셋
	b.WriteString(word(big.NewInt(int64(len(inner) / 2))))
	b.WriteString(padRight(inner))
	return hex.DecodeString(b.String())
}

// NonceKey 는 Kernel v3 의 nonce key(192비트)를 만든다.
//
// # 왜 0 을 넣으면 안 되는가
//
// Kernel v3 는 **검증자 정보를 nonce key 에 인코딩한다.** HAR 의 실측값이
// 이것이다:
//
//	0x0000 845adb2c711129d4f3966735ed98a9f09fc4ce57 2711 | 0000000000000005
//	  └모드·타입┘└──── ECDSA_VALIDATOR 주소 ────────┘└키┘   └─ 시퀀스(8바이트) ─┘
//
// 앞 2바이트는 검증 모드(0x00 = 기본)와 검증 타입(0x00 = 루트)이고, 루트
// 타입이므로 서명은 봉투 없는 65바이트 원문이다([SignUserOpHash] 참조).
//
// # 끝의 0x2711 은 설명하지 못한다 — 그래서 지어내지 않는다
//
// 이 2바이트가 무엇에서 왔는지 우리는 모른다. 모르는 값을 "아마 0 이겠지"로
// 바꾸는 것이 이 저장소가 2026-08-11 에 결함 9개를 낸 방식이다. 대신
// **관측된 그대로 쓴다.** 이 key 가 유효하다는 것은 실물이 증명했고,
// EntryPoint.getNonce 가 같은 key 의 다음 시퀀스를 돌려주므로 재사용이
// 정확히 맞다. key 마다 시퀀스가 독립이라 다른 값을 쓰면 검증되지 않은
// 경로로 들어간다.
const kernelNonceSuffix = "2711"

// NonceKey 는 validator 주소로 nonce key 24바이트를 만든다.
func NonceKey(validator string) ([]byte, error) {
	v, err := normalizeAddress(validator)
	if err != nil {
		return nil, fmt.Errorf("validator 주소: %w", err)
	}
	// 모드(1) + 타입(1) + 주소(20) + 키(2) = 24바이트.
	return hex.DecodeString("0000" + v + kernelNonceSuffix)
}

// GetNonceCalldata 는 EntryPoint.getNonce(sender, key) 의 calldata 다.
// key 는 24바이트여야 한다 — uint192 는 32바이트 워드에 **왼쪽 0 8바이트**로
// 채워 들어간다.
func GetNonceCalldata(sender string, key []byte) (string, error) {
	s, err := normalizeAddress(sender)
	if err != nil {
		return "", fmt.Errorf("sender 주소: %w", err)
	}
	if len(key) != 24 {
		return "", fmt.Errorf("nonce key 는 24바이트여야 한다 (받은 길이 %d)", len(key))
	}
	return "0x" + selGetNonce + leftPad(s) + strings.Repeat("0", 16) + hex.EncodeToString(key), nil
}

// ---------------------------------------------------------------- 인코딩 보조

func word(x *big.Int) string {
	h := x.Text(16)
	if len(h) > 64 {
		panic("32바이트를 넘는 값이 워드로 들어왔다: " + h)
	}
	return strings.Repeat("0", 64-len(h)) + h
}

func leftPad(hexNoPrefix string) string {
	return strings.Repeat("0", 64-len(hexNoPrefix)) + hexNoPrefix
}

// padRight 는 동적 바이트열을 32바이트 경계까지 0 으로 채운다.
func padRight(h string) string {
	if r := len(h) % 64; r != 0 {
		return h + strings.Repeat("0", 64-r)
	}
	return h
}

func normalizeAddress(s string) (string, error) {
	t := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	if len(t) != 40 {
		return "", fmt.Errorf("40자리 hex여야 한다 (0x 제외 %d자)", len(t))
	}
	if _, err := hex.DecodeString(t); err != nil {
		return "", fmt.Errorf("hex가 아니다: %w", err)
	}
	return strings.ToLower(t), nil
}

func normalizeWord(s string) (string, error) {
	t := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	if len(t) != 64 {
		return "", fmt.Errorf("32바이트 hex여야 한다 (0x 제외 %d자)", len(t))
	}
	if _, err := hex.DecodeString(t); err != nil {
		return "", fmt.Errorf("hex가 아니다: %w", err)
	}
	return strings.ToLower(t), nil
}
