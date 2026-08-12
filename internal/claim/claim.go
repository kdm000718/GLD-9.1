// Package claim 은 **정산이 끝난 포지션을 스스로 회수한다** (Auto-Claim).
//
// # 무엇을 하는가
//
// predict.fun 의 회차가 정산되면 보유 주식은 CTF 의 조건부 토큰으로 남고,
// 담보(USDT)로 바꾸려면 `redeemPositions` 를 직접 불러야 한다. 웹 UI 의
// "Claim" 버튼이 하는 일이고, 이 패키지가 그것을 봇 쪽에서 한다.
//
// 호출은 EOA 가 아니라 **Kernel 스마트계정**이 해야 한다(주식을 든 것이 그
// 계정이다). 그래서 ERC-4337 UserOperation 을 조립해 ZeroDev 번들러로 보낸다.
//
// # 가스는 들지 않는다
//
// predict.fun 의 ZeroDev 엔드포인트는 `provider=ULTRA_RELAY` 로 가스를
// 후원한다. HAR 실측에서 `maxFeePerGas: 0x0` 으로 전송됐고 영수증의
// `actualGasCost` 도 `0x0` 이었다. 계정에 BNB 가 없어도 된다 — 다만 후원을
// 요청하는 단계([Bundler.Sponsor])를 빠뜨리면 우리가 내야 하고, 그러면 실패한다.
//
// # 이 패키지가 조립하는 것을 왜 믿을 수 있는가
//
// `internal/claim/testdata/golden_userop.json` 은 2026-08-11 에 **실제로
// 성공한** claim 의 UserOperation 전문이다(BSC tx 0x83a80081…). golden_test.go
// 가 우리 조립을 그것과 **바이트 단위로** 대조한다 — callData, nonce key,
// userOpHash, 서명 형식 넷 전부. 이 대조를 먼저 통과시키고 나서 전송을 붙인
// 것이지 반대가 아니다.
//
// 그 대조가 실제로 값을 했다: 명세 §6 은 서명을 "Kernel 봉투로 감싼다"고
// 적었지만 실물은 봉투 없는 65바이트였다([SignUserOpHash] 참조). 명세대로
// 만들었다면 보내는 것마다 거부됐을 것이고, 원인은 키 문제와 구분되지 않았을
// 것이다.
//
// # 확인하지 못한 것은 하지 않는다
//
//   - negRisk·yieldBearing 시장은 회수하지 않는다. 대상 컨트랙트도 calldata 도
//     다른데 우리에겐 그 경로의 실물 기준이 없다([Position.Blocker]).
//   - 조립한 userOpHash 와 번들러가 돌려준 것이 다르면 보내지 않는다.
//   - 우리 서명에서 복구한 EOA 가 예상과 다르면 보내지 않는다.
//   - 영수증을 못 보면 **성공으로 치지 않는다.** 실패로 두고 다음 주기에 다시
//     시도한다 — redeem 은 멱등이다(이미 회수된 포지션은 0 을 준다).
//
// # 기본값은 조립까지다
//
// [Claimer.Send] 가 false 면 조립·서명까지만 하고 전송하지 않는다. 자금이
// 움직이는 경로의 기본값은 "안 보낸다" 여야 한다 — cmd/gld91 의 LIVE_ARM 과
// 같은 규약이다.
package claim

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/live"
)

// 영수증 폴링 값. HAR 에서는 첫 조회가 null 이고 약 1.5초 뒤에 나왔다.
const (
	receiptTimeout = 90 * time.Second
	receiptGap     = 2 * time.Second
)

// Claimer 는 회수 한 바퀴를 돈다.
type Claimer struct {
	// Account 는 주식을 든 Kernel 스마트계정(PREDICT_ACCOUNT)이다.
	Account string
	// Signer 는 그 계정의 등록 서명자 EOA 다. internal/kernel 이 기동 때
	// 이 대응을 체인에서 확인한다.
	Signer Signer
	// Bundler 는 ZeroDev 엔드포인트다.
	Bundler Bundler
	// Source 는 회수 대상 조회다. nil 이면 GraphQL 기본값.
	Source PositionSource
	// Ledger 는 정산 행을 적을 원장이다. nil 이면 적지 않는다.
	Ledger *ledger.Ledger
	// Send 가 false 면 조립까지만 하고 보내지 않는다.
	Send bool
	// Now 는 시험이 갈아끼운다. nil 이면 time.Now.
	Now func() time.Time
}

// Skip 은 회수하지 않은 포지션과 그 이유다.
type Skip struct {
	Position Position
	Reason   string
}

// MarketResult 는 시장 하나(conditionId 하나)에 대한 회수 결과다.
type MarketResult struct {
	ConditionID string
	Title       string
	// Positions 는 이 UserOperation 이 회수한 포지션들이다(전송 순서 그대로).
	Positions []Position
	// CallData 는 조립한 Kernel execute calldata 다. Send 가 false 여도 채워진다.
	CallData []byte
	// UserOpHash 는 우리가 계산한 값이다.
	UserOpHash string
	// Sent 는 실제로 전송했는가다.
	Sent bool
	// TxHash 는 영수증의 트랜잭션 해시다. 전송하고 성공했을 때만 채워진다.
	TxHash string
	// Err 는 이 시장의 실패다. 다른 시장은 계속 진행한다.
	Err error
	// LedgerNote 는 원장 기록에 대한 알림이다(기록했다 / 왜 못 했다).
	LedgerNote string
}

// Result 는 한 바퀴의 결과다.
type Result struct {
	Markets []MarketResult
	Skipped []Skip
}

// Claimed 는 전송해서 영수증까지 성공한 시장 수다.
func (r Result) Claimed() int {
	n := 0
	for _, m := range r.Markets {
		if m.Sent && m.Err == nil {
			n++
		}
	}
	return n
}

// Failed 는 실패한 시장 수다.
func (r Result) Failed() int {
	n := 0
	for _, m := range r.Markets {
		if m.Err != nil {
			n++
		}
	}
	return n
}

// Run 은 회수 대상을 조회해 시장별로 한 건씩 회수한다.
//
// # 왜 시장마다 따로 보내는가
//
// 한 UserOperation 에 여러 시장을 묶을 수도 있다 — 인코딩은 같다. 그러나
// 골든 벡터는 **시장 하나에 결과 둘**이었고, 그 모양을 유지하면 보내는
// 모든 UserOperation 이 실물과 같은 구조를 갖는다. 묶으면 구조가 벡터와
// 달라지는 데다, 한 시장이 실패할 때 나머지까지 같이 죽는다.
func (c *Claimer) Run(ctx context.Context) (Result, error) {
	if _, err := normalizeAddress(c.Account); err != nil {
		return Result{}, fmt.Errorf("PREDICT_ACCOUNT: %w", err)
	}
	if c.Signer == nil {
		return Result{}, fmt.Errorf("서명자가 없다")
	}
	src := c.Source
	if src == nil {
		src = GraphQL{}
	}

	positions, err := src.Claimable(ctx, c.Account)
	if err != nil {
		return Result{}, fmt.Errorf("회수 대상 조회: %w", err)
	}

	var res Result
	groups, order := groupByCondition(positions, &res)
	for _, cond := range order {
		m := c.claimOne(ctx, cond, groups[cond])
		res.Markets = append(res.Markets, m)
	}
	return res, nil
}

// groupByCondition 은 conditionId 로 묶되 **처음 나온 순서**를 지킨다.
// 회수 불가 사유가 있는 것은 res.Skipped 로 뺀다.
func groupByCondition(positions []Position, res *Result) (map[string][]Position, []string) {
	groups := make(map[string][]Position)
	var order []string
	for _, p := range positions {
		if why := p.Blocker(); why != "" {
			res.Skipped = append(res.Skipped, Skip{Position: p, Reason: why})
			continue
		}
		if _, ok := groups[p.ConditionID]; !ok {
			order = append(order, p.ConditionID)
		}
		groups[p.ConditionID] = append(groups[p.ConditionID], p)
	}
	return groups, order
}

func (c *Claimer) claimOne(ctx context.Context, conditionID string, positions []Position) MarketResult {
	m := MarketResult{ConditionID: conditionID, Positions: positions}
	if len(positions) > 0 {
		m.Title = positions[0].Title
	}

	// 1. redeemPositions 를 포지션마다 하나씩. 골든 벡터가 그랬다 — 결과 둘을
	//    한 호출의 indexSets 배열에 묶지 않고 호출 두 건으로 보냈다.
	execs := make([]Execution, 0, len(positions))
	for _, p := range positions {
		set, err := IndexSet(p.OutcomeIndex)
		if err != nil {
			m.Err = fmt.Errorf("%s(%s): %w", p.Title, p.OutcomeName, err)
			return m
		}
		cd, err := RedeemCalldata(Collateral, conditionID, []*big.Int{set})
		if err != nil {
			m.Err = fmt.Errorf("redeemPositions calldata: %w", err)
			return m
		}
		execs = append(execs, Execution{Target: CTF, Value: big.NewInt(0), CallData: cd})
	}

	// 2. Kernel execute 배치로 감싼다.
	callData, err := ExecuteBatchCalldata(execs)
	if err != nil {
		m.Err = fmt.Errorf("execute 배치 인코딩: %w", err)
		return m
	}
	m.CallData = callData

	// 3. nonce — key 에 검증자를 인코딩해야 한다. 0 을 넣으면 안 된다.
	key, err := NonceKey(ECDSAValidator)
	if err != nil {
		m.Err = err
		return m
	}
	nonce, err := c.Bundler.Nonce(ctx, c.Account, key)
	if err != nil {
		m.Err = fmt.Errorf("nonce 조회: %w", err)
		return m
	}

	// 4. 가스 가격 → 후원. 후원을 빠뜨리면 우리가 가스를 낸다.
	maxFee, maxPriority, err := c.Bundler.GasPrice(ctx)
	if err != nil {
		m.Err = fmt.Errorf("가스 가격 조회: %w", err)
		return m
	}
	op := UserOp{
		Sender:               c.Account,
		Nonce:                nonce,
		CallData:             callData,
		MaxFeePerGas:         maxFee,
		MaxPriorityFeePerGas: maxPriority,
	}
	est, err := c.Bundler.Sponsor(ctx, op)
	if err != nil {
		m.Err = fmt.Errorf("페이마스터 후원: %w", err)
		return m
	}
	op.PreVerificationGas = est.PreVerificationGas
	op.VerificationGasLimit = est.VerificationGasLimit
	op.CallGasLimit = est.CallGasLimit

	// 5. 해시 → 서명 → **자가 대조**.
	hash, err := op.Hash(EntryPoint, ChainID)
	if err != nil {
		m.Err = fmt.Errorf("userOpHash: %w", err)
		return m
	}
	m.UserOpHash = "0x" + hexStr(hash)

	sig, err := SignUserOpHash(hash, c.Signer)
	if err != nil {
		m.Err = err
		return m
	}
	op.Signature = sig

	// 서명에서 복구한 EOA 가 우리 키의 EOA 와 같아야 한다. 다르면 서명 경로가
	// 깨진 것이고, 그대로 보내면 번들러의 거부 사유가 "키가 틀렸다"와 구분되지
	// 않는다.
	got, err := RecoverSigner(hash, sig)
	if err != nil {
		m.Err = fmt.Errorf("서명 자가 대조: %w", err)
		return m
	}
	if want := SignerAddress(c.Signer); got != want {
		m.Err = fmt.Errorf("서명에서 복구한 EOA(%s)가 서명자의 EOA(%s)와 다르다 — 보내지 않는다",
			checksum(got), checksum(want))
		return m
	}

	if !c.Send {
		// 조립까지만. 자금이 움직이는 경로의 기본값이다.
		return m
	}

	// 6. 전송. 번들러가 돌려준 해시가 우리 것과 같아야 한다.
	remote, err := c.Bundler.Send(ctx, op)
	if err != nil {
		m.Err = fmt.Errorf("eth_sendUserOperation: %w", err)
		return m
	}
	m.Sent = true
	if !strings.EqualFold(strings.TrimPrefix(remote, "0x"), hexStr(hash)) {
		m.Err = fmt.Errorf("번들러의 userOpHash(%s)가 우리 계산(%s)과 다르다 — 서명한 것과 받은 것이 다르다",
			remote, m.UserOpHash)
		return m
	}

	// 7. 영수증. 못 보면 성공으로 치지 않는다.
	rcpt, err := c.Bundler.WaitReceipt(ctx, remote, receiptTimeout, receiptGap)
	if err != nil {
		m.Err = err
		return m
	}
	if !rcpt.Success {
		m.Err = fmt.Errorf("UserOperation 이 실패로 실렸다 (tx %s)", rcpt.TxHash)
		return m
	}
	m.TxHash = rcpt.TxHash

	// 8. 원장. **여기서 실패해도 회수는 이미 끝났다** — 실패로 뒤집지 않고
	//    알림만 남긴다. 뒤집으면 다음 주기가 이미 회수된 것을 다시 보낸다.
	m.LedgerNote = c.recordSettlement(positions)
	return m
}

// recordSettlement 는 회수한 시장 하나를 원장에 적는다.
//
// [ledger.Settlement.Won] 은 **우리** 포지션이 이겼는가다. 한 시장에서 이긴
// 주식과 진 주식을 함께 들고 있을 수 있으므로(실물이 그랬다), 이긴 것이
// 있으면 그 주식 수로 적는다 — [ledger.SettlementProceeds] 가 이긴 주식만
// 주당 $1 로 세기 때문이다. 진 주식만 있으면 Won=false 로 적어 수입 0 을
// 남긴다.
func (c *Claimer) recordSettlement(positions []Position) string {
	if c.Ledger == nil {
		return "원장 없음 — 정산 행을 적지 않았다"
	}
	if len(positions) == 0 {
		return ""
	}
	slug := positions[0].CategoryID
	start, ok := live.ParseRoundStart(slug)
	if !ok {
		// 회차 시작 시각을 모르면 적지 않는다. 0 으로 적으면 원장에 존재하지
		// 않는 회차의 행이 생기고, 그것은 지우기보다 찾기가 어렵다.
		return fmt.Sprintf("원장 생략 — 카테고리 슬러그 %q 에서 회차 시작을 뽑지 못했다", slug)
	}

	var wonShares, lostShares float64
	for _, p := range positions {
		if p.Won {
			wonShares += p.Shares
		} else {
			lostShares += p.Shares
		}
	}
	s := ledger.Settlement{RoundStart: start, At: c.now()}
	if wonShares > 0 {
		s.Won, s.Shares = true, wonShares
	} else {
		s.Won, s.Shares = false, lostShares
	}
	if err := c.Ledger.RecordSettlement(s); err != nil {
		return fmt.Sprintf("원장 기록 실패(회수는 끝났다): %v", err)
	}
	return fmt.Sprintf("원장에 정산 기록 — 회차 %d, %s %.6f주", start, wonLabel(s.Won), s.Shares)
}

func wonLabel(won bool) string {
	if won {
		return "승"
	}
	return "패"
}

func (c *Claimer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
