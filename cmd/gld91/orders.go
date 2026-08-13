package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/kdm000718/GLD-9.1/internal/exec"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// 이 파일이 주문의 부수효과를 전부 담는다 — 서명과 전송.
//
// # 서명은 무장 여부와 무관하게 **항상** 한다
//
// DRY-RUN 이 실거래와 다른 코드를 타면 DRY-RUN 이 아무것도 증명하지 못한다.
// 그래서 게이트는 서명 **뒤**, 전송 **앞** 딱 한 줄에만 있다. 이 파일에서
// `if s.Armed` 가 나오는 자리가 하나뿐인 것이 그 규약이다.
//
// # 스키마와 Exchange 주소는 확정됐다 (2026-08-10, P5 Task 9)
//
// 서명을 싣는 자리는 **order 객체 안**이다 — `/docs` 의 OpenAPI 스펙에서
// `ContractOrder` 스키마에 `signature`(maxLength 200)와 `hash`(maxLength 66)가
// 있다. 86바이트 봉투는 `0x` + 172자 = 174자라 200 안에 들어간다. SDK 도
// 같다(`SignedOrder extends Order { signature: string }`).
//
// Exchange 주소는 **회차마다 마켓 응답에서 고른다**(domainFor). 상수로 박지
// 않는 이유는 order/exchange.go 에 있다.

// weiPerShare 는 주식 수량의 18 decimals 스케일이다(order.AmountsForBuy 가
// 받는 단위).
var weiPerShare = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// maxSalt 는 2^31 이다. 스펙 §6 이 salt < 2^31 을 적고 있다.
var maxSalt = new(big.Int).Lsh(big.NewInt(1), 31)

// zeroAddress 는 taker 다 — 아무나 체결할 수 있다는 뜻이다.
const zeroAddress = "0x0000000000000000000000000000000000000000"

// maxSignatureLen 은 스펙 `ContractOrder.signature` 의 maxLength 다.
const maxSignatureLen = 200

// expirationGrace 는 회차 종료 뒤 주문이 유효한 시간이다.
//
// # 왜 만료를 채우는가
//
// `exec` 가 회차 끝에 미체결을 전량 취소한다. 그러나 그 취소는 **프로세스가
// 살아 있어야** 일어난다. 봇이 크래시하거나 호스트가 죽으면 취소해 줄 주체가
// 없고, 만료가 0(= 만료 없음)이면 그 주문은 체인에서 유효한 채로 남는다.
// 만료는 거래소·체인이 지키므로 우리가 죽어도 지켜진다 — 취소와 독립인
// 두 번째 방어다.
//
// 이것이 없으면 봇 사망이 곧 "아무도 모르는 살아 있는 매수 주문"이 된다.
// 모니터도 메울 수 없다: 미체결 조회(`GET /v1/orders`)는 JWT 가 필요하고,
// 그것은 개인키를 감시 호스트에 두어야 한다는 뜻이기 때문이다.
//
// # 왜 0 이 아니고, 왜 길지 않은가
//
// 0 이 아닌 이유: 회차 종료 시각은 거래소 메타데이터에서 오고 우리 시계와
// 정확히 같지 않다. 종료 직전에 낸 주문이 시계 차이로 서명 시점에 이미
// 만료돼 있으면 그 회차의 마지막 진입이 통째로 사라진다.
//
// 길지 않은 이유: 이 값이 곧 **봇이 죽은 뒤 주문이 체결될 수 있는 창**이다.
const expirationGrace = 60 * time.Second

// orderSender 는 exec.Orders 구현이다.
type orderSender struct {
	// Rest 는 전송 경로다. 무장하지 않으면 쓰지 않는다.
	Rest *rest.Client
	// Signer 는 EOA 서명자다(등록 서명자여야 한다 — 자가 점검 2번).
	Signer *auth.Signer
	// Account 는 PREDICT_ACCOUNT(스마트계정)다. maker·signer 필드가 되고
	// Kernel 도메인의 verifyingContract 가 된다.
	//
	// **API 응답에서 유도하지 않는다.** `GET /v1/account` 의 address 는
	// 서명자 EOA 이지 스마트계정이 아니다(P4 실측) — 그 값으로 주문을 내면
	// 자금 없는 EOA 로 나간다.
	Account   common.Address
	Validator common.Address

	// ChainID 는 EIP-712 도메인의 chainId 다(메인넷 56).
	ChainID int64

	// ExchangeOverride 는 verifyingContract 를 손으로 지정하는 탈출구다.
	//
	// **비어 있는 것이 정상이다.** 비어 있으면 회차마다 마켓 응답의
	// isNegRisk/isYieldBearing 으로 고른다(order.ExchangeFor). 계약이
	// 이전됐는데 표를 아직 못 고친 날을 위해 남겨 둔 자리이고, 켜져 있으면
	// 기동 로그가 그 사실을 시끄럽게 알린다.
	ExchangeOverride string

	// Armed 가 false 면 서명까지만 하고 전송하지 않는다.
	Armed bool

	Log func(format string, args ...any)

	// Now 는 DRY-RUN 결과의 시각이다. nil 이면 time.Now.
	Now func() time.Time
	// Salt 는 주문 salt 를 만든다. nil 이면 crypto/rand.
	Salt func() (*big.Int, error)

	// dryRunSeq 는 DRY-RUN 주문 식별자의 일련번호다. **비어 있지 않은
	// 식별자를 주는 것이 중요하다** — exec 는 빈 식별자를 "취소할 수 없는
	// 주문" 으로 다루고 회차 끝에 에러를 낸다. DRY-RUN 은 취소까지 포함한
	// 전체 상태 기계를 돌려야 의미가 있다.
	dryRunSeq atomic.Int64
}

func (s *orderSender) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(format, args...)
	}
}

// domainFor 는 이 회차의 EIP-712 도메인이다.
//
// **회차마다 다시 고른다.** verifyingContract 는 마켓이 정하고
// (isNegRisk/isYieldBearing), 그 값이 틀리면 다이제스트가 통째로 달라진다 —
// 거래소가 거부해 주면 다행이고, 그 주소가 실재하는 다른 Exchange 라면
// 우리는 의도하지 않은 계약에 유효한 서명을 실어 보낸다. 상수로 박으면
// 거래소가 상품을 negRisk 로 옮기는 날 그 일이 **조용히** 일어난다.
func (s *orderSender) domainFor(rd live.Round) (order.Domain, error) {
	if s.ExchangeOverride != "" {
		return order.Domain{
			Name:              order.DomainName,
			Version:           order.DomainVersion,
			ChainID:           s.ChainID,
			VerifyingContract: s.ExchangeOverride,
		}, nil
	}
	return order.DomainFor(s.ChainID, rd.IsNegRisk, rd.IsYieldBearing)
}

func (s *orderSender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// randomSalt 는 [1, 2^31) 의 정수다. 0 을 피하는 이유: 같은 주문을 두 번 낼 때
// salt 가 같으면 다이제스트가 같아 거래소가 중복으로 볼 수 있다.
func randomSalt() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Sub(maxSalt, big.NewInt(1)))
	if err != nil {
		return nil, fmt.Errorf("salt 생성 실패: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}

// sharesToWei 는 주식 수를 18 decimals 정수로 바꾼다.
//
// **big.Float 을 거친다.** float64 에 1e18 을 곱한 뒤 int64 로 자르면 2^63 을
// 넘어 조용히 음수가 되고, 음수 수량은 makerAmount 를 음수로 만든다 —
// EIP-712 는 그런 값도 멀쩡히 서명한다. risk.Shares 는 2^53 미만만 통과시키지만
// 그 값에 1e18 을 곱하면 이미 int64 범위 밖이다.
func sharesToWei(shares float64) (*big.Int, error) {
	if shares <= 0 {
		return nil, fmt.Errorf("주식 수가 %v 다", shares)
	}
	f := new(big.Float).SetPrec(200).SetFloat64(shares)
	f.Mul(f, new(big.Float).SetPrec(200).SetInt(weiPerShare))
	n, acc := f.Int(nil)
	if n == nil {
		return nil, fmt.Errorf("주식 수 %v 를 정수로 바꾸지 못했다 (%v)", shares, acc)
	}
	if n.Sign() <= 0 {
		return nil, fmt.Errorf("주식 수 %v 가 wei 로 %s 가 됐다", shares, n)
	}
	return n, nil
}

// build 는 서명된 주문 한 건을 만든다. **전송하지 않는다.**
//
// 돌려주는 body 는 POST /v1/orders 의 요청 본문이고, envelope 는 86바이트
// 서명 봉투다(로그·검증용).
func (s *orderSender) build(r exec.Request) (body map[string]any, envelope []byte, err error) {
	if s.Signer == nil {
		return nil, nil, errors.New("서명자가 배선되지 않았다")
	}
	tokenID, ok := new(big.Int).SetString(r.TokenID, 10)
	if !ok || tokenID.Sign() <= 0 {
		return nil, nil, fmt.Errorf("토큰 ID 가 10진 양수가 아니다 (%q)", r.TokenID)
	}
	qtyWei, err := sharesToWei(r.Shares)
	if err != nil {
		return nil, nil, err
	}
	// WeiPerShare 는 0 < V < 10^precision 을 벗어나면 패닉한다. 여기까지 오는
	// 틱은 quote 가 만든 것이라 범위 안이지만, 패닉은 살아 있는 주문을 든 채
	// 죽는 것이므로 그 전에 막는다.
	if r.Tick.V <= 0 {
		return nil, nil, fmt.Errorf("틱이 %d 다", r.Tick.V)
	}
	if r.Tick.Precision < 1 || r.Tick.Precision > 18 {
		return nil, nil, fmt.Errorf("틱 precision 이 %d 다", r.Tick.Precision)
	}
	// 여기 있던 "0.5 미만" 관문은 2026-08-14 에 풀렸다.
	//
	// 그날 주문이 두 다리가 됐다. 메이커 다리는 여전히 0.46 이고 exec 의
	// limitTick 이 0.5 미만을 지킨다. 그러나 **테이커 다리는 최우선 매도호가
	// 그대로 건다** — 실측 220 회차 중 61% 가 0.50 초과였으므로, 여기서 0.49 로
	// 자르면 그 회차들에서 관통이 관통이 아니라 그냥 대기 주문이 된다(사용자
	// 결정: "푼다 — 호가 그대로 관통").
	//
	// 남은 상한은 1.00 이다. 이진 시장에서 결과 토큰은 최대 1.00 을 지급하므로
	// 그 이상에 사면 이겨도 손해다 — 그 주문은 어떤 전략에서도 실수다.
	if full := order.Full(r.Tick.Precision); r.Tick.V >= full {
		return nil, nil, fmt.Errorf("틱 %d 가 1.00(%d) 이상이다 — 이기고도 손해인 가격이다", r.Tick.V, full)
	}
	priceWei := r.Tick.WeiPerShare()

	// **SDK 의 유효숫자 3자리 절단이 우리 틱을 바꾸면 주문을 내지 않는다.**
	//
	// order.AmountsForBuy 는 SDK 규칙대로 가격을 3자리로 절단한다. 실측된
	// precision 2·3 에서는 틱 값이 최대 3자리라 절단이 아무것도 바꾸지 않지만,
	// precision 4 이상이면 바뀐다(실측: V=4999/p=4 → 0.4999 가 0.4990 이 된다).
	//
	// 그때 무슨 일이 일어나는가: quote 는 0.4999 에 걸었다고 믿고 exec 는 그
	// 틱을 상태로 들고 다니는데, 거래소에 실제로 놓인 주문은 0.4990 이다.
	// "같은 가격이면 재주문하지 않는다" 판정이 존재하지 않는 호가를 기준으로
	// 돌고, 우리 주문을 호가창에서 빼는 계산도 엉뚱한 틱에서 이뤄진다 —
	// **에러도 로그도 없이.** live.FetchLive 는 precision 1..18 을 허용하므로
	// 거래소가 정밀도를 올리면 그날 바로 이 상태가 된다.
	//
	// 낮은 가격에 걸리는 것이라 손실 방향은 아니지만, 우리가 정한 가격과 다른
	// 가격에 거는 것 자체가 이 봇이 해서는 안 되는 일이다.
	if truncated := order.RetainSignificantDigits(priceWei, 3); truncated.Cmp(priceWei) != 0 {
		return nil, nil, fmt.Errorf(
			"틱 %d(precision %d)의 가격이 SDK 의 유효숫자 3자리 절단에 걸린다 (%s → %s wei) — "+
				"우리가 정한 가격과 다른 가격에 걸리므로 주문하지 않는다",
			r.Tick.V, r.Tick.Precision, priceWei, truncated)
	}

	makerAmount, takerAmount, err := order.AmountsForBuy(priceWei, qtyWei)
	if err != nil {
		return nil, nil, fmt.Errorf("주문 금액 계산: %w", err)
	}
	if r.Round.FeeRateBps < 0 {
		return nil, nil, fmt.Errorf("feeRateBps 가 음수다 (%d)", r.Round.FeeRateBps)
	}
	// 만료 없는 주문은 내지 않는다(expirationGrace 참고). 제로시각의 Unix()
	// 는 음수라 서명에 들어가면 거래소가 어떻게 읽을지 모른다 — 애매하면
	// 주문하지 않는 쪽으로 실패한다.
	if r.Round.EndsAt.IsZero() {
		return nil, nil, errors.New("회차 종료 시각이 없다 — 만료 없는 주문은 내지 않는다")
	}
	expiration := r.Round.EndsAt.Add(expirationGrace).Unix()

	saltFn := s.Salt
	if saltFn == nil {
		saltFn = randomSalt
	}
	salt, err := saltFn()
	if err != nil {
		return nil, nil, err
	}

	// **maker == signer == 스마트계정이다.** 그리고 인증 세션도 같은 주소로
	// 맺어야 한다(auth.Authenticator.Account 참고).
	//
	// 거래소가 요구하는 두 조건을 2026-08-11 실측으로 확정했다:
	//
	//	① maker == signer            (create_order_maker_signer_mismatch)
	//	② signer == 인증된 주소        (Authenticated signer does not match…)
	//
	// 합치면 셋이 모두 같아야 한다. 담보는 스마트계정에 있으므로 그 주소여야
	// 하고, 그래서 세션도 스마트계정으로 맺는다.
	//
	// **signatureType 으로 maker≠signer 를 허용받을 수 없다.** 0·1·2 를 각각
	// 보내 봤고 셋 다 같은 400 이었다. 이 엔드포인트는 무조건 둘이 같기를
	// 요구한다.
	//
	// **maker 를 EOA 로 바꿔서 고치면 안 된다.** 그쪽에는 자금이 없다(P4 실측).
	// 서명은 EOA 키가 하되 Kernel 봉투로 감싸고, 계정 컨트랙트가 ERC-1271 로
	// 검증한다 — 그래서 signatureType 은 0 이다.
	o := order.Order{
		Salt:    salt,
		Maker:   s.Account.Hex(),
		Signer:  s.Account.Hex(),
		Taker:   zeroAddress,
		TokenID: tokenID,

		MakerAmount: makerAmount,
		TakerAmount: takerAmount,
		// 회차 종료 + 유예. **exec 의 전량 취소와 독립인 두 번째 방어다** —
		// 그 취소는 프로세스가 살아 있어야 일어난다(expirationGrace 참고).
		// SDK 골든 벡터가 0 을 쓰지만 그것은 "만료 없음"의 예시일 뿐이고,
		// 스펙은 이 필드를 "Unix timestamp in seconds" 로 정의한다.
		Expiration:    big.NewInt(expiration),
		Nonce:         big.NewInt(0),
		FeeRateBps:    big.NewInt(int64(r.Round.FeeRateBps)),
		Side:          0, // BUY. 이 봇은 매도 주문을 내지 않는다.
		SignatureType: 0, // EOA (EIP-1271 로 Kernel 이 검증한다)
	}

	dom, err := s.domainFor(r.Round)
	if err != nil {
		return nil, nil, err
	}
	digest, err := order.Hash(o, dom)
	if err != nil {
		return nil, nil, fmt.Errorf("주문 다이제스트: %w", err)
	}
	envelope, err = order.SignForPredictAccount(digest, dom.ChainID, s.Account, s.Validator, s.Signer)
	if err != nil {
		return nil, nil, fmt.Errorf("주문 서명: %w", err)
	}

	sig := "0x" + hex.EncodeToString(envelope)
	// **스키마의 상한을 우리가 지킨다.** ContractOrder.signature 는
	// maxLength 200 이다. 지금 봉투는 86바이트 = 174자라 여유가 있지만, 서명
	// 형식이 바뀌어 넘치면 거래소는 400 을 돌려줄 뿐이고 우리는 그 400 을
	// "거래소가 주문을 거부했다"로 읽는다 — 원인이 우리 쪽이라는 사실이 사라진다.
	if len(sig) > maxSignatureLen {
		return nil, nil, fmt.Errorf("서명이 %d자다 — ContractOrder.signature 의 상한 %d자를 넘는다", len(sig), maxSignatureLen)
	}

	body = map[string]any{
		"pricePerShare": priceWei.String(),
		"strategy":      "LIMIT",
		// **서명은 order 객체 안이다 — 확정됐다(2026-08-10).**
		//
		// `/docs` 의 OpenAPI 스펙에서 `ContractOrder` 스키마를 직접 읽었다.
		// required 12개(salt…signatureType) 외에 optional 로 `hash`
		// (maxLength 66)와 `signature`(maxLength 200)가 있다. P4 가 적어 둔
		// 필드 목록은 required 만 보고 이 둘을 빠뜨렸다(docs/results/
		// p4-findings.md 를 함께 고쳤다). 본문 최상위에는 signature 자리가
		// 없다 — CreateOrderData 의 필드는 pricePerShare, strategy,
		// slippageBps, isFillOrKill, isPostOnly, reservedBalancePolicy,
		// isMinAmountOut, selfTradePrevention, order 뿐이다.
		//
		// 길이도 맞는다: 86바이트 봉투는 "0x" + 172자 = 174자로 200 이하다.
		// SDK 도 같은 자리다 — `SignedOrder extends Order { signature: string }`.
		"order": map[string]any{
			"salt":        o.Salt.String(),
			"maker":       o.Maker,
			"signer":      o.Signer,
			"taker":       o.Taker,
			"tokenId":     o.TokenID.String(),
			"makerAmount": o.MakerAmount.String(),
			"takerAmount": o.TakerAmount.String(),
			// **expiration 만 숫자다.** 스펙의 ContractOrder 스키마에서
			// salt·tokenId·makerAmount·takerAmount·nonce·feeRateBps 는
			// `type: string` 인데 expiration 은 `type: integer, format: int64,
			// "Unix timestamp in seconds"` 다. 문자열로 보내면 스키마 위반이고,
			// 값이 0 일 때는 관대한 파서가 삼켜서 드러나지 않는다.
			"expiration":    o.Expiration.Int64(),
			"nonce":         o.Nonce.String(),
			"feeRateBps":    o.FeeRateBps.String(),
			"side":          o.Side,
			"signatureType": o.SignatureType,
			"signature":     sig,
		},
	}
	return body, envelope, nil
}

// Create 는 주문 한 건이다. **서명은 항상 하고, 전송은 무장했을 때만 한다.**
func (s *orderSender) Create(ctx context.Context, r exec.Request) (exec.CreateResult, error) {
	body, envelope, err := s.build(r)
	if err != nil {
		// 서명 전 단계의 실패다 — 요청이 나가지 않았음이 확실하므로 재시도해도
		// 이중 주문이 되지 않는다. exec 는 이 분류를 *rest.OrderError 로 읽는다.
		return exec.CreateResult{}, &rest.OrderError{Kind: rest.OrderNotSent, Err: err}
	}

	// 서명 결과를 로그로 남긴다 — 봉투 길이와 첫 바이트가 DRY-RUN 이 실제로
	// 서명 경로를 탔다는 증거다. **주소도 서명 원문도 찍지 않는다**(저장소가
	// GitHub 에 올라간다).
	// 변종 **이름**을 함께 찍는다. 주소는 찍지 않는다 — 이름만으로도 "어느
	// 계약에 서명했는가"가 남고, 그것이 이 줄이 증명해야 할 전부다.
	exch := order.ExchangeName(r.Round.IsNegRisk, r.Round.IsYieldBearing)
	if s.ExchangeOverride != "" {
		exch = "수동지정"
	}
	s.logf("주문 서명 완료: 봉투 %d바이트, 선두 0x%02x, %s %.0f주 @ 틱 %d (%s, %s)",
		len(envelope), envelope[0], r.Outcome, r.Shares, r.Tick.V, r.Round.Slug, exch)

	if !s.Armed {
		// ── 여기가 이 봇의 유일한 무장 게이트다 ──
		id := "dryrun-" + strconv.FormatInt(s.dryRunSeq.Add(1), 10)
		s.logf("DRY-RUN — 전송하지 않는다 (가상 id=%s)", id)
		// **가상 해시도 준다.** 이것이 없으면 DRY-RUN 이 체결 확인 경로를
		// 통째로 건너뛰고, 24시간을 돌려도 그 경로에 대해 아무것도 증명하지
		// 못한다 — 이 봇의 실거래 결함 아홉 중 다섯이 그렇게 숨어 있었다.
		return exec.CreateResult{ID: id, Hash: dryRunHashPrefix + id, LockedUntil: s.now()}, nil
	}

	res, err := s.Rest.CreateOrder(ctx, body)
	if err != nil {
		return exec.CreateResult{}, err
	}
	return exec.CreateResult{
		ID:                 res.OrderID,
		Hash:               res.OrderHash,
		LockedUntil:        res.RemovalLockedUntil,
		RemovalLockUnknown: res.RemovalLockUnknown,
	}, nil
}

// dryRunHashPrefix 는 가상 주문의 해시 앞머리다. [orderSender.Filled] 가
// 이것으로 "전송된 적 없는 주문" 을 가려낸다 — 거래소에 없는 해시를 물어보면
// 404 가 오고, DRY-RUN 이 매 회차 조회 실패 로그로 뒤덮인다.
const dryRunHashPrefix = "dryrun-hash-"

// Filled 는 이 주문이 몇 주 찼는지 거래소에 묻는다.
//
// DRY-RUN 에서는 언제나 0 이다 — 전송하지 않았으니 찰 수가 없다. 그래도 이
// 경로를 타는 것이 요점이다: exec 는 무장 여부와 무관하게 같은 코드를 돌고,
// 확인 대기 주문이 풀리는지가 DRY-RUN 에서도 관찰된다.
func (s *orderSender) Filled(ctx context.Context, hash string) (float64, error) {
	if strings.HasPrefix(hash, dryRunHashPrefix) {
		return 0, nil
	}
	if !s.Armed {
		// 무장하지 않았는데 진짜 해시가 왔다 — 배선이 어긋난 것이다.
		// 0 을 돌려주면 "안 찼다"가 되어 그 어긋남이 조용히 묻힌다.
		return 0, fmt.Errorf("DRY-RUN 인데 실제 주문 해시를 확인하려 한다 (%q)", hash)
	}
	return s.Rest.OrderFilled(ctx, hash)
}

// Remove 는 주문 취소다. DRY-RUN 에서는 우리가 만든 가상 주문을 우리가
// 지운다 — 거래소에는 아무것도 없으므로 전부 Removed 다.
func (s *orderSender) Remove(ctx context.Context, ids []string) (exec.RemoveResult, error) {
	if len(ids) == 0 {
		return exec.RemoveResult{}, nil
	}
	if !s.Armed {
		s.logf("DRY-RUN — 가상 주문 %d건 취소", len(ids))
		return exec.RemoveResult{Removed: append([]string(nil), ids...)}, nil
	}
	res, err := s.Rest.RemoveOrders(ctx, ids)
	if err != nil {
		return exec.RemoveResult{}, err
	}
	return exec.RemoveResult{
		Removed:     res.Removed,
		Noop:        res.Noop,
		Rejected:    res.Rejected,
		Unaccounted: res.Unaccounted,
	}, nil
}
