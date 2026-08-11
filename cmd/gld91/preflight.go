package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"

	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/kernel"
	"github.com/kdm000718/GLD-9.1/internal/model"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// 이 파일은 기동 자가 점검이다. **주문이 하나라도 나가기 전에** 전부 본다.
//
//	1. 환경변수 셋이 있는가            → 없으면 종료
//	2. 키의 EOA 가 계정의 등록 서명자인가 → 아니면 종료
//	3. 모델이 코드의 피처와 같은가       → 아니면 종료
//	4. equity 로 유효한 주문을 낼 수 있는가 → 아니면 **비무장 운전**(종료 아님)
//	5. 원장과 거래소 상태가 맞는가        → 어긋나면 종료
//
// 4번만 종료가 아닌 것은 사용자 지시다. 자본이 부족한 것은 설정 오류가 아니라
// 사실이고, DRY-RUN 은 그 상태에서도 계속 도는 것이 맞다.

// ErrReconcileMismatch 는 원장과 거래소가 말하는 상태가 어긋난다는 뜻이다.
//
// **이것은 조회 실패와 다르다.** 조회 실패는 "모른다" 이고 이것은 "다르다" 다.
// 다르다는 것은 이전 인스턴스가 주문을 든 채 죽었거나, 체결이 원장에 안
// 적혔거나, 사람이 웹에서 따로 거래했다는 뜻이고, 셋 다 봇이 이어서 판단할
// 근거가 없다. **추측으로 이어가지 않는다.**
var ErrReconcileMismatch = errors.New("원장과 거래소 상태가 어긋난다")

// verifySigner 는 자가 점검 2번이다 — cmd/signercheck 와 **같은 함수**를 쓴다.
//
// P4 실측: 준비된 키가 등록 서명자가 아니면 **주문 서명이 전부 거부된다.**
// 이 대조 없이 진행하면 온체인 승인에 가스를 먼저 쓰고, 첫 주문이 거부되고
// 나서야 원인을 찾는다.
//
// 키는 이 함수 밖으로 나가지 않는다 — 주소만 유도해서 넘긴다.
func verifySigner(ctx context.Context, v kernel.Verifier, account string, signer *auth.Signer) error {
	if signer == nil {
		return errors.New("서명자가 없다")
	}
	return v.Verify(ctx, account, signer.Address().Hex())
}

// loadModel 은 자가 점검 3번이다. model.Load 가 피처 이름을 순서까지 대조한다 —
// 이름이 어긋난 채로 곱하면 계수가 엉뚱한 피처에 붙고, 확률은 여전히 0~1
// 사이라 눈에 띄지 않는다.
func loadModel(path string) (*model.LogReg, error) {
	return model.Load(path, features.FeatureNames)
}

// ledgerState 는 원장이 말하는 우리 상태다.
type ledgerState struct {
	// Rows 는 데이터 행 수(헤더 제외)다.
	Rows int
	// FillShares 는 매수 누적 주식 수다. 거래소 포지션과 대조하는 값이다.
	FillShares float64
	// SettledRounds 는 정산 행이 있는 회차 수다. 지금은 항상 0 이다 —
	// exec 가 정산을 쓰지 않는다(정산 조회 경로가 없다). 0 이 아니면 누군가
	// 그 자리를 채운 것이고, 그 사실은 보여야 한다.
	SettledRounds int
}

// readLedger 는 원장을 읽어 상태를 만든다.
//
// **열은 위치가 아니라 헤더 이름으로 찾는다** — ledger 패키지가 헤더를 쓰는
// 이유가 그것이다. 위치로 읽으면 열이 하나 추가되는 순간 수량 열에서 가격을
// 읽는다.
//
// 파일이 없으면 빈 상태다(첫 실행). 파일이 있는데 읽지 못하면 **에러다** —
// 못 읽은 원장을 "비었다" 로 다루면 이전 포지션을 통째로 잊는다.
func readLedger(path string) (ledgerState, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ledgerState{}, nil
	}
	if err != nil {
		return ledgerState{}, fmt.Errorf("원장을 열지 못했다: %w", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	// 열 개수 고정을 끄지 않는다 — 줄이 잘렸으면 그 사실이 에러로 나와야 한다.
	head, err := r.Read()
	if errors.Is(err, io.EOF) {
		return ledgerState{}, nil // 열려만 있고 헤더도 없는 새 파일
	}
	if err != nil {
		return ledgerState{}, fmt.Errorf("원장 헤더를 읽지 못했다: %w", err)
	}
	col := map[string]int{}
	for i, name := range head {
		col[name] = i
	}
	for _, need := range []string{"record", "round_start", "shares"} {
		if _, ok := col[need]; !ok {
			return ledgerState{}, fmt.Errorf("원장에 %q 열이 없다 (헤더 %v) — 다른 형식의 파일이거나 헤더가 깨졌다", need, head)
		}
	}

	var st ledgerState
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ledgerState{}, fmt.Errorf("원장 %d번째 데이터 행: %w", st.Rows+1, err)
		}
		st.Rows++
		kind := rec[col["record"]]
		shares, err := strconv.ParseFloat(rec[col["shares"]], 64)
		if err != nil {
			return ledgerState{}, fmt.Errorf("원장 %d번째 행의 shares 를 읽지 못했다: %w", st.Rows, err)
		}
		if math.IsNaN(shares) || math.IsInf(shares, 0) {
			return ledgerState{}, fmt.Errorf("원장 %d번째 행의 shares 가 유한하지 않다 (%v)", st.Rows, shares)
		}
		switch kind {
		case "fill":
			st.FillShares += shares
		case "settlement":
			st.SettledRounds++
		case "rebate":
			// 리베이트는 반대편 주식이라 우리 매수 포지션과 섞지 않는다.
		default:
			return ledgerState{}, fmt.Errorf("원장 %d번째 행의 record 가 %q 다 — 모르는 종류를 세면 틀린 결론이 나온다", st.Rows, kind)
		}
	}
	return st, nil
}

// reconcile 은 자가 점검 5번이다 — 원장과 거래소를 대조한다.
//
// # 무엇을 대조하는가
//
//  1. **열린 주문이 있으면 안 된다.** 이 봇은 회차가 끝날 때 미체결을 전량
//     취소하고, 그것을 확인하지 못하면 에러로 죽는다. 그러니 기동 시점에
//     주문이 남아 있다는 것은 이전 인스턴스가 주문을 든 채 죽었다는 뜻이다.
//     열린 주문 목록 엔드포인트는 P4 가 확인하지 못했으므로 **예약 USDT** 로
//     본다 — 미체결 매수 주문이 묶어 두는 값이다.
//
//  2. **거래소가 우리 원장보다 많은 주식을 알고 있으면 안 된다.** 반대
//     방향(원장이 더 많다)은 정상이다 — 정산된 회차의 포지션은 사라지지만
//     원장 줄은 남는다.
//
// # 조회 실패와 불일치를 구분한다
//
// 불일치는 [ErrReconcileMismatch] 를 감싼다. 조회 실패는 그렇지 않다. 호출자는
// 앞은 언제나 종료로, 뒤는 무장 여부에 따라 다르게 다룬다 — 조회가 안 되는
// 것은 "다르다" 가 아니라 "모른다" 이고, 아무것도 전송하지 않는 DRY-RUN 에서
// 모른다는 이유로 죽을 필요는 없다.
func reconcile(ctx context.Context, rc *rest.Client, ledgerPath string) error {
	st, err := readLedger(ledgerPath)
	if err != nil {
		// 원장을 못 읽는 것은 조회 실패가 아니다. 우리 파일이고, 읽히지
		// 않는다면 그 자체가 어긋난 상태다.
		return fmt.Errorf("%w: %v", ErrReconcileMismatch, err)
	}

	reserved, err := rc.ReservedUSDT(ctx)
	if err != nil {
		return fmt.Errorf("크래시 복구 대조: 예약잔고를 읽지 못했다: %w", err)
	}
	if reserved > 0 {
		return fmt.Errorf("%w: 예약 USDT 가 %v 다 — 이전 인스턴스가 미체결 주문을 든 채 죽었을 수 있다. "+
			"사람이 열린 주문을 확인하고 취소한 뒤에 다시 띄워라", ErrReconcileMismatch, reserved)
	}

	ps, err := rc.Positions(ctx)
	if err != nil {
		return fmt.Errorf("크래시 복구 대조: 포지션을 읽지 못했다: %w", err)
	}
	var held float64
	for _, p := range ps {
		held += p.AmountShares
	}
	// 상대 오차를 둔다. 거래소는 wei 정수를, 원장은 십진 float 을 들고 있으므로
	// 마지막 자리가 갈릴 수 있다. 절대값 하한도 함께 둔다 — 원장이 비었을 때
	// 상대 오차만으로는 어떤 차이도 통과하지 못한다.
	const relTol, absTol = 1e-6, 1e-9
	if held > st.FillShares+absTol+relTol*math.Abs(st.FillShares) {
		return fmt.Errorf("%w: 거래소 포지션 %.6f주 > 원장 매수 누적 %.6f주 — "+
			"체결이 원장에 안 적혔거나 이 계정으로 봇 밖에서 거래한 것이다. 사람이 확인해야 한다",
			ErrReconcileMismatch, held, st.FillShares)
	}
	return nil
}

// armingBlockers 는 **지금 무장할 수 없는 이유**들이다. 비어 있지 않으면
// 무장하지 않는다.
//
// 목록으로 두는 이유: 하나를 고쳤을 때 나머지가 자동으로 드러나야 한다.
// 사유를 코드 여기저기의 if 로 흩뿌리면 하나를 고친 사람이 "이제 무장된다"고
// 믿는다.
// # 닫힌 사유와 그 근거 (2026-08-10, P5 Task 9)
//
// 셋을 닫았다. **여기에 근거를 남기는 이유**: 나중에 누가 "이게 왜 안전한가"를
// 물을 때 답이 커밋 메시지나 보고서가 아니라 코드에 있어야 하기 때문이다.
//
//	(닫힘 1) 체결 조회 미배선 — fills.go 의 restFills 가 배선됐다.
//	         `GET /v1/orders/matches` 를 marketId + signerAddress 로 좁혀
//	         받고, taker/makers[] 중 signer 가 우리인 원소만 센다. 최상위
//	         amountFilled 는 **테이커 기준 트랜잭션 전체 수치**라 파싱조차
//	         하지 않는다(rest/matches.go). 폴링은 자체 주기로 조이고
//	         (DefaultFillsPollInterval), 한 번도 성공하지 못한 회차에서는
//	         빈 목록이 아니라 에러를 돌려준다 — exec 는 그것을 "이 주기에는
//	         신규 주문을 내지 않는다"로 다룬다. 무장 경로는 armedFills 만
//	         받으므로 noFills 는 여전히 **컴파일 단계에서** 막힌다.
//
//	(닫힘 2) 서명을 싣는 자리 — `/docs` 의 OpenAPI 스펙에서 `ContractOrder`
//	         스키마를 직접 읽었다. required 12개 외에 optional 로 `hash`
//	         (maxLength 66)와 `signature`(maxLength 200)가 있고, 본문 최상위
//	         (CreateOrderData)에는 signature 자리가 없다. 86바이트 봉투는
//	         "0x"+172자 = 174자로 상한 안이다. P4 의 필드 목록이 required 만
//	         보고 둘을 빠뜨렸던 것이고, 그 문서도 함께 고쳤다. orders.go 의
//	         build 가 상한을 코드로 강제한다(maxSignatureLen).
//
//	(닫힘 3) Exchange 변종 — **상수로 박지 않고 회차마다 고른다.** 마켓 응답의
//	         isNegRisk/isYieldBearing 두 불린으로 네 변종 중 하나를 고른다
//	         (order.ExchangeFor, SDK 의 getExchangeIdentifier 와 같은 분기).
//	         2026-08-10 메인넷 실측에서 진행 중 btc-5m 마켓은 false/false =
//	         CTF_EXCHANGE 였지만, 그 사실을 상수로 박으면 거래소가 상품을
//	         negRisk 로 옮기는 날 조용히 틀린 계약에 서명한다. 두 필드는
//	         live.Market 에서 *bool 로 받아 **없으면 회차를 거부한다** —
//	         bool 이면 빠진 값이 false 가 되고 false/false 는 하필 오늘 맞는
//	         답이라, 파싱이 깨진 사실이 덮인다.
//
// 남은 하나(자금·승인)는 돈이 필요해서 코드로 닫을 수 없다.
func armingBlockers() []string {
	return []string{
		// (4) 온체인 승인. 자금과 승인 없이는 어떤 주문도 체결되지 않는다.
		"USDT 승인(approve)과 자금 입금이 확인되지 않았다",
	}
}
