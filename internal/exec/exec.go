// Package exec 는 한 회차를 실제로 운용한다 — 회차 시작에 동결된 방향으로
// **지정가 매수 한 건**을 걸고, 회차가 끝나면 미체결을 전량 취소한다.
//
// # 회차마다 한 다리, 한 번
//
//	메이커   상수 [LimitPrice]. 시장이 그 값까지 내려와야 체결된다.
//
// 그 다리가 [risk.CapFraction] 전액을 쓴다([legBudget]).
//
// 다리를 **옮기지 않는다.** 군중을 따라가지 않고, 취소했다가 다시 걸지 않는다.
// 회차가 끝날 때까지 그 자리에 두고, 끝나면 거둔다.
//
// 그래서 이 봇에는 **관통 방지가 없다.** 상대 매도호가가 [LimitPrice] 이하인
// 순간에 걸면 즉시 체결된다. 그것을 알고 고른 설계다(2026-08-12 사용자 결정) —
// 예전에는 매도호가 한 틱 아래로 피했지만, 지금은 피하지 않는다. 이 문단을
// 지우기 전에 그 결정을 먼저 뒤집어야 한다.
//
// # 역선택 — 이 전략의 유일한 진짜 문제
//
// 지정가에 걸어 두면 **시장이 우리 쪽으로 내려올 때만** 체결되는데, 그것은 곧
// 우리 방향이 지고 있다는 뜻이다. 실측이 이것을 정확히 보여 준다:
//
//	가격    회차   체결율   판정 승률   체결분 승률   낙차
//	0.47    238    90.8%     54.2%       49.5%     -4.7%p
//	0.46    124    80.6%     48.4%       36.0%    -12.4%p
//
// 미체결 회차는 0.47 에서 22전 22승, 0.46 에서 24전 24승 — **양쪽 다 전승**이다.
// 그래서 다음 한 줄이 두 구간의 손익을 소수점 첫째 자리까지 설명한다:
//
//	판정 = 미체결비율 × 100% + (1−미체결비율) × 체결분
//
// **가격을 낮출수록 낙차가 커진다.** 낮은 가격이 주는 손익분기 이득보다 대가가
// 한 자릿수 배 크다. 그래서 [LimitPrice] 는 내리는 것이 아니라 올린다.
//
// # 다리가 둘이었다가 다시 하나가 된 이력 (2026-08-14)
//
// 같은 날 테이커 다리를 붙였다가 뗐다. 붙인 이유는 위의 역선택 우회였고 —
// 시장가로 사면 이기는 회차를 놓치지 않는다 — 실제로 미체결이 0 이 됐다.
// 뗀 이유는 값이다:
//
//	메이커 0.46    99회차 36.4% 승  손익분기 47.0%   P&L -42.19
//	테이커 (호가) 123회차 48.8% 승  손익분기 52.8%   P&L -17.22
//
// 테이커는 평균 0.5103 에 샀다. 모델의 최근 실력(2023~ 이력 51.8%)으로는 그
// 값을 감당할 수 없다 — 손익분기 52.8% 를 구조적으로 못 넘는다. 사용자 결정으로
// 제거했다.
//
// # 이 패키지는 아무 가격도 정하지 않는다
//
// 돈을 잃는 판단은 [LimitPrice] 라는 상수와 `internal/risk`(얼마를 걸지,
// 걸어도 되는지)에 있다. 오더북은 로그([Runner.marketNote])에만 쓴다 —
// 2026-08-14 에 [takerTick] 이 잠시 예외였다가 함께 사라졌다. **오더북이
// 결정에 닿는 경로가 다시 생기면 그것은 전략 변경이다**(book.go).
//
// # 노출 불변식이 이 패키지의 존재 이유다
//
//	체결누적 + 미체결 + 취소미확인 < cap
//
// 세 항 중 **취소미확인**이 이 패키지에만 있는 항이다. 취소 요청을 보내고
// 거래소가 확인해 주기 전까지 그 주문은 아직 체결될 수 있다. 이 봇은 회차당
// 수백 번 재호가하므로 취소·재주문 창이 수백 번 열린다. 그 창에서 취소 미확인
// 주문을 0 으로 세면 신규 주문이 그만큼 더 나가고, 옛 주문이 체결되는 순간
// 노출이 두 배가 된다.
//
// 그래서 명목이 노출에서 빠지는 조건은 하나뿐이다: **거래소가 그 주문이
// 사라졌다고 말한 것.**(RemoveResult 의 Removed 또는 Noop) 그 밖의 모든
// 상태 — 거부, 응답에 없음, 취소 요청 자체가 실패, 생성 결과 불명 — 에서는
// 명목이 그대로 남는다.
//
// # 실패 방향 — 주문 생성만 반대다
//
// `risk`·`ledger`·`live` 와 같이 "애매하면 거래하지 않는" 쪽으로 실패한다.
// 예외가 하나 있고 그것이 주문 생성이다. "보냈는데 응답을 못 받은" 상태를
// 실패로 단정해 재시도하면 주문이 둘 들어가고 노출이 두 배가 된다. 그래서
// 생성 실패는 [Orders] 구현이 준 재시도 안전성 분류를 따르고, **분류하지
// 못한 실패는 전부 "보냈을 수 있다"로 읽는다.**
//
// 반대로 **취소 재시도는 언제나 안전하다.** 취소는 멱등이고, 두 번 취소해도
// 두 번째는 noop 이다. 그래서 취소는 확인될 때까지 물고 늘어진다.
//
// # 패닉하지 않는다
//
// 살아 있는 주문을 든 채 죽으면 취소도 못 한다. 설정이 망가졌는지는 주문이
// 하나라도 나가기 **전에** 전부 검사하고, 그 뒤로는 에러로만 빠져나온다.
// 특히 `order.Full`·`order.Ceiling` 은 precision 이 1..18 밖이면 패닉하므로,
// 회차의 precision 을 [Runner.RunRound] 진입부에서 먼저 막는다.
//
// # 아직 비어 있는 자리 — 정산
//
// **[ledger.Settlement] 를 여기서 쓰지 않는다.** `Settlement.Won` 은 거래소
// 정산 결과에서만 와야 하는데, 그 조회 경로가 아직 없다(P4 가 확정한 REST
// 엔드포인트에 정산 조회가 없다). 바이낸스 봉으로 승패를 계산해 채우고 싶은
// 유혹이 있지만 그러면 안 된다 — G2 가 잰 정산 불일치 `d≈0.30%` 가 정확히
// 그 둘의 차이이고, 그 어긋남은 백테스트가 아니라 실거래에서만 드러난다.
// 비어 있는 편이 틀린 값보다 낫다. `exec_test.go` 의
// TestExecNeverWritesSettlement 이 이 자리가 조용히 채워지는 것을 막는다.
//
// 리베이트도 같은 이유로 호출부가 없다. [ledger.RebateValue] 의 bool 인자는
// 컴파일러가 지켜주지 못하므로(잘못 넣어도 컴파일된다), 호출부가 생기는
// 순간을 테스트가 잡게 해 두었다.
package exec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
	"github.com/kdm000718/GLD-9.1/internal/risk"
	"github.com/kdm000718/GLD-9.1/internal/timing"
)

// ---------------------------------------------------------------------------
// 경계 타입 — 이 패키지는 rest 를 임포트하지 않는다
// ---------------------------------------------------------------------------

// Request 는 주문 한 건이다. **금액이 아니라 틱과 주식 수로 표현한다** —
// 실수 가격은 서명 경로에 들어가면 안 되고(order.Tick.WeiPerShare 가 정수
// 연산으로 wei 를 만든다), 이 패키지도 가격을 실수로 비교하지 않는다.
type Request struct {
	Round   live.Round
	Outcome string     // ledger.OutcomeUp | ledger.OutcomeDown
	TokenID string     // 그 방향의 CTF 토큰 ID
	Tick    order.Tick // 지정가
	Shares  float64    // 주식 수
}

// Notional 은 이 주문이 묶는 명목(USD)이다. 노출 회계의 단위다.
func (r Request) Notional() float64 { return r.Shares * r.Tick.Float() }

// CreateResult 는 주문 생성의 결과다. `rest.CreateOrderResult` 에서 이 층이
// 실제로 쓰는 셋만 옮긴 것이다.
type CreateResult struct {
	// ID 는 취소에 쓸 식별자다. 비어 있으면 우리는 이 주문을 취소할 수 없다.
	ID string
	// Hash 는 이 주문을 **단건 조회**할 때 쓰는 키다(`GET /v1/orders/{hash}`).
	// ID 와 다르다 — 숫자 ID 로 그 경로를 부르면 404 다(2026-08-12 실측).
	//
	// 이것이 필요한 이유는 [Orders.Filled] 다. 취소가 확인됐다는 말만으로는
	// 그 주문이 안 찼다고 말할 수 없고, 해시가 없으면 물어볼 수도 없다.
	Hash string
	// LockedUntil 은 이 시각 전에는 취소가 거부되는 잠금 창의 끝이다.
	LockedUntil time.Time
	// RemovalLockUnknown 은 잠금 시각을 못 읽었다는 뜻이다. "잠금 없음"과
	// 구별해야 한다 — 전자는 곧바로 취소해도 되고 후자는 모른다.
	RemovalLockUnknown bool
}

// RemoveResult 는 취소 요청의 결과다. `rest.RemoveResult` 와 같은 네 바구니다.
//
// **Unaccounted 를 빼면 안 된다.** 응답에서 조용히 빠진 ID 를 "거부되지 않음"
// 으로 읽으면 호출자는 재시도를 멈추고 살아 있는 주문을 잊는다. 잊힌 매수
// 주문은 체결되고 그 노출은 어떤 한도에도 잡히지 않는다.
type RemoveResult struct {
	Removed     []string // 취소됨
	Noop        []string // 이미 끝난 주문 — 취소할 것이 없었다
	Rejected    []string // 잠금 창 안이라 거부됨 — 재시도 대상
	Unaccounted []string // 응답 어디에도 없었다 — 살아 있을 수 있다
}

// Orders 는 주문 전송 경로다. 구현은 `cmd/gld91` 에 있고, **서명은 무장
// 여부와 무관하게 항상 한 뒤** DRY-RUN 이면 전송만 건너뛴다. 그 게이트를
// 여기 두지 않는 이유: 여기에 두면 DRY-RUN 이 서명 경로를 타지 않게 되어
// DRY-RUN 이 아무것도 증명하지 못한다.
type Orders interface {
	Create(ctx context.Context, r Request) (CreateResult, error)
	Remove(ctx context.Context, ids []string) (RemoveResult, error)
	// Filled 는 **이 주문이 지금까지 몇 주 찼는지**를 거래소에 직접 묻는다.
	//
	// # 왜 이 메서드가 있어야 하는가
	//
	// 취소 응답의 `removed`/`noop` 은 "그 주문이 더는 호가창에 없다"는 뜻일
	// 뿐이다. **체결된 주문도 호가창에 없다.** 2026-08-11 실거래에서 정확히
	// 그 일이 났다:
	//
	//	17:36:07.554  취소 확인 (id=2024751893) — 명목 3.92 를 노출에서 뺀다
	//	17:36:07.892  신규 49 → 8주 @ 0.49 (잔여 4.2392)   ← 노출 0 이라 믿고
	//	17:36:09.702  체결 8주 @ 0.49                       ← 그 "취소된" 주문이
	//
	// 체결 피드는 2.6~4.6초 늦게 온다(실측). 그 사이 봇은 자기가 아무것도
	// 들고 있지 않다고 믿고 같은 크기를 다시 걸었다. 회차 명목이 상한
	// 4.4 대비 7.84 까지 갔다.
	//
	// 그래서 취소가 확인된 주문은 **버리지 않고 여기에 물어본다.** 답을
	// 들으면 찬 만큼만 노출에 남기고 나머지를 푼다.
	//
	// 에러를 돌려주면 "모른다"는 뜻이고, 호출자는 그 주문의 명목을 계속
	// 노출에 남긴다 — 모르는 것을 안 찼다고 치는 쪽이 이 사고를 만들었다.
	Filled(ctx context.Context, hash string) (shares float64, err error)
}

// Fills 는 이 회차의 체결을 알려주는 유일한 창구다.
//
// **Poll 은 아직 돌려주지 않은 체결만 돌려줘야 한다.** 같은 체결을 두 번
// 돌려주면 원장에 중복 줄이 생기고 노출이 실제보다 크게 잡힌다(그쪽은 안전한
// 방향이지만 원장은 오염된다). 중복 제거는 구현의 책임이다 — [ledger.Fill] 에
// 거래소 체결 ID 를 담을 자리가 없어서 여기서 대신 해 줄 수 없다.
//
// 이 인터페이스가 필수인 이유: 체결을 못 세면 노출 불변식의 첫 항을 모르고,
// 그 상태로 재호가를 반복하면 한도를 몇 배로 넘겨 베팅한다. 배선이 이것을
// 빠뜨리면 조용히 0 으로 도는 대신 [Runner.RunRound] 가 에러를 돌려준다.
type Fills interface {
	Poll(ctx context.Context, rd live.Round) ([]ledger.Fill, error)
}

// FillRecorder 는 원장의 체결 기록 부분이다. `*ledger.Ledger` 가 만족한다.
// 인터페이스로 받는 이유는 테스트가 실제 파일을 쓰지 않게 하기 위해서다.
type FillRecorder interface {
	RecordFill(f ledger.Fill) error
}

// retryClassifier 는 주문 생성 실패의 재시도 안전성이다.
// `*rest.OrderError` 가 이 모양을 만족한다 — rest 를 임포트하지 않고 결합한다.
type retryClassifier interface{ SafeToRetry() bool }

// ---------------------------------------------------------------------------
// 지정가 — 이 봇이 거는 유일한 가격
// ---------------------------------------------------------------------------

// 가격을 **정수 두 개**로 들고 있는 이유: 틱은 정밀도마다 다르고(정밀도 2 면
// 48, 3 이면 480), 실수 0.48 에 10^precision 을 곱해 반올림하는 식으로 만들면
// 정밀도가 커질수록 마지막 자리가 갈릴 여지가 생긴다. 나눗셈이 딱 떨어지는지
// 검사하는 정수 경로에는 그 여지가 없다 — 표현할 수 없는 정밀도는 조용히
// 근사되는 대신 에러가 된다([limitTick]).
const (
	limitPriceNum = 48
	limitPriceDen = 100
)

// LimitPrice 는 **메이커 다리**가 매수호가를 거는 가격이다. 회차마다 이 가격에
// 한 번만 건다.
//
// **상수인 것이 요점이다.** 플래그로 빼면 운영 중에 값이 바뀔 수 있고, 그러면
// 원장의 어떤 줄이 어떤 가격 정책으로 나갔는지 사후에 알 수 없다(2026-08-12
// 사용자 결정). 바꾸려면 이 줄을 고치고 다시 빌드해 배포해야 한다.
//
// # 값의 이력과 그 이유
//
//	0.47   ~2026-08-14   216회차 실측: 체결분 승률 49.5%, 손익분기 47.7%
//	0.46    2026-08-14    99회차 실측: 체결분 승률 36.4%, 손익분기 47.0%
//	0.48    2026-08-14~   사용자 결정
//
// 가격만 1틱 내렸는데 승률이 13.1%p 무너졌다. **역선택이다** — 지정가를 낮출수록
// "시장이 거기까지 밀려온 회차" 만 체결되고, 그건 우리 방향이 틀렸을 때다. 낮은
// 가격이 주는 이득(손익분기 -0.7%p)보다 그 대가가 훨씬 크다.
//
// 그래서 반대로 올린다. 0.48 은 손익분기를 49.0% 로 올리는 대신 체결되는 회차의
// 성격을 덜 편향시킨다. **이 맞교환이 이득인지는 아직 실측되지 않았다** — 0.47
// 위쪽은 표본이 없다.
//
// 이 다리는 테이커 다리의 반대편 — 시장이 우리 쪽으로 내려올 때만 체결되는 쪽 —
// 을 맡는다.
//
// 판단에 쓰지 않는다 — 실제 틱은 [limitTick] 이 정수로 만든다. 이 값은 로그와
// 문서용이다.
const LimitPrice = float64(limitPriceNum) / float64(limitPriceDen)

// legFraction 은 한 다리가 쓸 수 있는 명목의 비율이다 — [risk.CapFraction] 대비.
//
//	0.5   2026-08-14   다리가 둘이었다. 합쳐도 회차 상한을 넘지 않게 반씩 썼다.
//	1.0   2026-08-14~  테이커 다리를 없앴다. 나눌 대상이 없다(사용자 결정).
//
// **1.0 이어도 이 상수를 지운 것이 아니다.** 다리가 다시 둘이 되는 날 나눗셈을
// 여기 한 곳에서만 고치기 위해서다. 다리마다 `Cap/n` 을 따로 쓰면 한쪽을 고칠 때
// 다른 쪽이 남아 합이 cap 을 넘는 날이 온다.
const legFraction = 1.0

// legBudget 은 다리 하나에 허용되는 명목이다.
//
// 두 값 중 작은 쪽을 쓴다:
//
//	cap × legFraction   이 다리의 몫
//	Remaining           이미 나간 것을 뺀 회차 잔여
//
// 후자가 필요한 이유는 첫 다리가 나간 뒤 둘째 다리를 계산할 때다. 몫만 보면
// 두 다리의 합이 cap 과 같아질 수 있는데, 사용자 제약은 **미만**이다.
func legBudget(e risk.Equity, x risk.Exposure) float64 {
	share := risk.Cap(e) * legFraction
	if rem := risk.Remaining(e, x); rem < share {
		return rem
	}
	return share
}

// limitTick 은 이 회차의 정밀도에서 [LimitPrice] 에 해당하는 틱이다 —
// **메이커 다리 전용**이다. 테이커 다리는 [takerTick] 이 만든다.
//
// 두 가지를 막는다:
//
//	표현 불가   정밀도 1 인 마켓에서 0.48 은 존재하지 않는 가격이다. 0.4 나
//	            0.5 로 반올림해 주면 **사용자가 정하지 않은 가격에 건다** —
//	            게다가 0.5 는 아래의 상한 검사에도 걸리는 값이다.
//	0.5 이상    메이커 다리는 여전히 0.5 미만이어야 한다. 상수를 잘못 고치는
//	            날 그 제약이 조용히 사라지지 않도록 여기서 한 번 더 본다.
//	            (2026-08-14 에 풀린 것은 **테이커 다리뿐**이다.)
//
// precision 은 [Runner.check] 가 1..18 로 이미 막았다 — 그래야 order.Full 이
// 패닉하지 않는다.
func limitTick(precision int) (order.Tick, error) {
	return limitTickFor(limitPriceNum, limitPriceDen, precision)
}

// limitTickFor 는 [limitTick] 의 몸통이고, 가격을 인자로 받는다.
//
// **떼어낸 이유는 시험 가능성이다.** 상수가 0.5 미만인 한 위의 상한 검사는
// 영영 돌지 않는다 — 그래서 그 검사를 통째로 지워도 모든 시험이 통과했다
// (2026-08-14 변이 M4). 지금은 시험이 50/100 을 직접 넣어 그 분기를 밟는다.
//
// 이 상한이 지키는 것은 미래의 실수다: 누군가 [limitPriceNum] 을 0.5 이상으로
// 고치는 날, 메이커 다리가 조용히 "이겨도 본전 근처" 인 가격에 걸리는 것을
// 막는다. 2026-08-14 에 풀린 것은 테이커 다리뿐이다.
func limitTickFor(num, den int64, precision int) (order.Tick, error) {
	full := order.Full(precision)
	if full%den != 0 {
		return order.Tick{}, fmt.Errorf("exec: precision %d 에서는 지정가 %d/%d 를 표현할 수 없다",
			precision, num, den)
	}
	v := num * (full / den)
	if ceiling := order.Ceiling(precision).V; v > ceiling {
		return order.Tick{}, fmt.Errorf("exec: 지정가 틱 %d 가 0.5 미만 상한 %d 을 넘는다 (지정가 %d/%d)",
			v, ceiling, num, den)
	}
	return order.NewTick(v, precision), nil
}

// maxPlaceAttempts 는 주문 생성을 다시 시도하는 횟수의 상한이다.
//
// **재시도는 "아무것도 나가지 않았음이 확정된" 실패에서만 한다**([safeToRetry]).
// 그 조건에서는 호가창에 우리 주문이 없으므로 다시 보내도 한 건을 넘지 않는다 —
// "다리당 한 건" 은 깨지지 않는다.
//
// 상한이 필요한 이유는 루프 주기가 50ms 라는 것이다. 상한이 없으면 계속
// 실패하는 회차에서 300초 동안 요청을 퍼붓고 240 req/min 예산을 태운다.
const maxPlaceAttempts = 3

// ---------------------------------------------------------------------------
// 기본값
// ---------------------------------------------------------------------------

const (
	// DefaultPoll 은 오더북을 다시 읽는 주기다. 요청을 만들지 않는 루프라
	// 짧아도 레이트리밋을 쓰지 않는다 — 실제 요청 빈도는 재호가 쿨다운이
	// 지배한다.
	DefaultPoll = 50 * time.Millisecond

	// DefaultRejectBackoff 는 취소가 거부되거나 응답에서 빠졌을 때 다시
	// 시도하기까지 기다리는 시간이다. 잠금 시각을 아는 경우에는 그 시각을
	// 쓰고, 이 값은 **모를 때만** 쓴다.
	DefaultRejectBackoff = 500 * time.Millisecond

	// DefaultFinalCancelTimeout 은 회차 종료 후 미체결 취소에 쓰는 상한이다.
	// 이 안에 확인하지 못하면 에러다 — 잊힌 주문은 체결된다.
	DefaultFinalCancelTimeout = 30 * time.Second

	// DefaultEntryWindow 는 회차 시작으로부터 **주문을 낼 수 있는 창**이다.
	// 이 창이 지나면 그 회차에는 아무것도 걸지 않는다.
	//
	// # 왜 창이 필요한가
	//
	// `p_up` 은 회차 시작(+0분) 정보로 동결된 값이다. 시작 90초 뒤에 그 값으로
	// 거는 것은 **이미 90초 움직인 시장에 90초 전의 판단으로 베팅하는 것**이고,
	// G2 가 잰 엣지는 회차 시작 근처 진입을 가정한 값이다.
	//
	// 예전에는 이 가드가 배선(cmd/gld91 의 -max-join-late, 기본 120초)에만
	// 있었고 회차 선택만 막았다. 선택이 제때 됐어도 equity 조회가 늦으면
	// 주문은 얼마든지 늦게 나갈 수 있었다 — 그 자리가 여기다.
	//
	// # 5초의 근거 (2026-08-12 실측, 291회차)
	//
	//	회차 진입        중앙 0.036초  p90 0.053초
	//	equity 조회 완료  중앙 0.525초  p90 0.593초
	//	첫 주문          중앙 0.527초  p90 0.602초   ← 99.2% 가 2초 안
	//
	// 10초를 넘긴 것은 241건 중 2건뿐이고 둘 다 재시작 뒤 늦은 합류였다
	// (30.6초, 81.0초). 5초는 p90 의 8배 여유를 두고 그 둘을 자른다.
	DefaultEntryWindow = 5 * time.Second

	// DefaultSettleGrace 는 취소가 확인된 주문의 체결 여부를 **거래소에 묻지
	// 못했을 때**, 체결 피드가 따라잡았다고 볼 때까지 기다리는 시간이다.
	//
	// 값의 근거는 실측이다. 2026-08-11 실거래의 원장(거래소가 적은 체결 시각)과
	// 로그(우리가 그것을 본 시각)를 대조하면 지연이 이랬다:
	//
	//	17:21:51 → 17:21:53.658   2.6초
	//	17:21:53 → 17:21:55.677   2.6초
	//	17:21:51 → 17:21:55.677   4.6초
	//	17:25:01 → 17:25:04.373   3.3초
	//	17:36:07 → 17:36:09.702   2.7초
	//
	// 체결 조회 최소 간격 2초가 그 안에 들어 있다. 6초는 관측된 최댓값에
	// 여유를 더한 값이고, **이쪽으로 틀리면 덜 거는 방향**이다.
	DefaultSettleGrace = 6 * time.Second
)

// maxRemoveBatch 는 한 취소 요청에 담을 수 있는 ID 개수다. `rest.MaxRemoveIDs`
// 와 같은 값이고, rest 를 임포트하지 않으려고 복제한다(ws.MaxPrecision 이
// order.weiDecimals 를 복제하는 것과 같은 이유).
//
// **이 상한이 없으면 취소가 통째로 실패한다.** rest.RemoveOrders 는 100개를
// 넘기면 요청을 보내지 않고 에러를 돌려주는데, 취소 미확인 주문은 명목이
// 노출에 남으므로 그 상태에서 배치가 계속 커지면 아무것도 취소되지 않는다.
// 미확인 주문 수의 상한은 대략 cap/$1 이라(주문 하나가 최소 $1) equity 가
// $2,200 을 넘으면 100 을 넘길 수 있다.
const maxRemoveBatch = 100

// ---------------------------------------------------------------------------
// Runner
// ---------------------------------------------------------------------------

// Runner 는 한 회차를 운용한다.
//
// **`*live.Predictor` 필드가 없는 것이 이 구조체의 규약이다.** 들고 다니면
// 회차 중 `p_up` 을 다시 계산하는 경로가 생기고, 그것은 "+0분 정보만" 이라는
// 사용자 제약 위반이다. 예측은 [live.Frozen] 값으로 받아 그대로 들고 다닌다.
// exec_test.go 의 TestRunnerHasNoPredictorField 가 이것을 지킨다.
type Runner struct {
	// Book 은 살아 있는 오더북이다. WS 고루틴이 갱신하고 이 루프가 읽는다 —
	// 둘 다 Book 의 뮤텍스를 지난다.
	Book *ws.Book
	// Orders 는 주문 전송 경로다.
	Orders Orders
	// Fills 는 체결 조회다. 없으면 회차를 돌지 않는다.
	Fills Fills
	// Ledger 는 체결 기록이다.
	Ledger FillRecorder

	// StaleAfter 는 오더북 신선도 문턱이다.
	//
	// **더 이상 거래를 막지 않는다.** 가격이 상수가 된 뒤로 호가창은 어떤
	// 결정에도 들어가지 않으므로, 호가창이 낡았다는 이유로 주문을 미루면
	// 그것은 근거 없이 회차를 버리는 일이다. 지금 이 값이 하는 일은 하나다 —
	// 주문을 낸 순간의 시장 기록([Runner.marketNote])에 "이 값은 낡았다"는
	// 표시를 붙일지 정한다.
	//
	// 그럼에도 0 이하를 거부하는 이유: 제로값이 "항상 stale" 로 읽혀도,
	// "문턱 없음" 으로 읽혀도 기록이 거짓말을 한다.
	StaleAfter time.Duration
	// EntryWindow 는 회차 시작으로부터 주문을 낼 수 있는 창이다. 지나면 그
	// 회차에는 걸지 않는다. 근거는 [DefaultEntryWindow] 에 있다.
	//
	// **0 이하면 회차를 돌지 않는다.** StaleAfter 와 같은 규약이다 — 제로값이
	// "창 없음"으로 읽히면 배선 실수 하나가 "회차 중간에도 건다"를 되살리고,
	// 그 사실은 어떤 로그에도 남지 않는다.
	EntryWindow time.Duration
	// StartedAt 은 이 러너가 살기 시작한 시각이다 — 사실상 프로세스 기동 시각.
	//
	// **첫 바퀴 체결 조회를 건너뛰어도 되는지 판정하는 데만 쓴다**
	// ([skipFirstFillPoll]). 회차가 시작하기 전부터 우리가 살아 있었다면 그
	// 회차에 우리 체결이 있을 수 없다는 논증의 근거가 이 값이다.
	//
	// 제로면 건너뛰지 않는다 — 배선을 잊은 것이 "언제나 안전"으로 읽히면 안 된다.
	StartedAt time.Time
	// Poll 은 루프 주기다. 0 이면 DefaultPoll.
	Poll time.Duration
	// RejectBackoff 는 0 이면 DefaultRejectBackoff.
	RejectBackoff time.Duration
	// FinalCancelTimeout 은 0 이면 DefaultFinalCancelTimeout.
	FinalCancelTimeout time.Duration
	// SettleGrace 는 0 이면 DefaultSettleGrace.
	SettleGrace time.Duration

	// Clock 은 벽시계다. 테스트가 쥔다. nil 이면 time.Now.
	//
	// **time.Now 를 그대로 쓰는 것이 옳다.** Go 의 time.Time 은 단조시계
	// 읽기를 함께 담고 Sub 이 그것을 쓰므로 쿨다운 판정이 NTP 보정에 흔들리지
	// 않는다. 단 직렬화를 거치면 단조 성분이 사라지므로 주문을 낸 시각을
	// **파일에서 되읽지 않는다** — 크래시 복구는 열린 주문을 "쿨다운 만료"로
	// 취급한다(그쪽이 안전하다: 큐 위치를 잃을 뿐 주문이 늘지 않는다).
	Clock func() time.Time
	// MonoClock 은 Book.Stale 판정에 쓰는 단조 경과(ns)다. 프레임의
	// RecvMonoNs 와 **같은 기준점**이어야 한다 — 기본값이 timing.Uptime 인
	// 이유가 그것이다(ws.conn 이 timing.Stamp 로 찍는다). 다른 기준점을 넣으면
	// 신선한 호가창이 영원히 stale 로 보이거나 그 반대가 된다.
	MonoClock func() int64
	// Sleep 은 폴링 대기다. 테스트가 갈아 끼워 시계를 앞으로 민다.
	Sleep func(ctx context.Context, d time.Duration) error
	// Log 는 판단 근거를 남긴다. nil 이면 아무것도 남기지 않는다.
	Log func(format string, args ...any)

	// Observe 는 매 바퀴와 회차 종료 뒤에 우리 상태를 복사해 받는다.
	// nil 이면 부르지 않는다 — 관측은 부수 기능이고, 그 부재가 거래를 막으면
	// 안 된다. 자세한 규약은 [Observation] 참고.
	Observe func(Observation)
}

func (r *Runner) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

func (r *Runner) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *Runner) monoNs() int64 {
	if r.MonoClock != nil {
		return r.MonoClock()
	}
	return timing.Uptime().Nanoseconds()
}

func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Runner) poll() time.Duration {
	if r.Poll > 0 {
		return r.Poll
	}
	return DefaultPoll
}

func (r *Runner) rejectBackoff() time.Duration {
	if r.RejectBackoff > 0 {
		return r.RejectBackoff
	}
	return DefaultRejectBackoff
}

func (r *Runner) finalCancelTimeout() time.Duration {
	if r.FinalCancelTimeout > 0 {
		return r.FinalCancelTimeout
	}
	return DefaultFinalCancelTimeout
}

func (r *Runner) settleGrace() time.Duration {
	if r.SettleGrace > 0 {
		return r.SettleGrace
	}
	return DefaultSettleGrace
}

// ---------------------------------------------------------------------------
// 회차 상태 — 전부 RunRound 안에 산다
// ---------------------------------------------------------------------------

// openOrder 는 우리가 낸 주문 하나다.
type openOrder struct {
	id string
	// hash 는 단건 조회 키다([Orders.Filled]). 비어 있으면 이 주문이 얼마나
	// 찼는지 물어볼 수 없고, 그러면 명목을 회차 끝까지 노출에 남긴다.
	hash     string
	tick     int64
	shares   float64
	notional float64
	// goneAt 은 거래소가 이 주문이 호가창에 없다고 확인해 준 시각이다.
	// 체결 피드가 따라잡았는지 재는 기준점이다.
	goneAt time.Time
	// askAt 은 다음 단건 조회를 허용하는 시각이다. 조회가 실패했을 때 매
	// 바퀴(50ms) 다시 물으면 240 req/min 예산이 순식간에 사라진다.
	askAt time.Time
	// placed 는 [Runner.Clock] 이 준 시각이다. 쿨다운을 여기서 잰다.
	placed time.Time
	// retryAt 은 다음 취소 시도를 허용하는 시각이다. 잠금 창을 아는 동안에는
	// 그 끝이고, 거부·불명 뒤에는 백오프다. **이 값 전에는 취소 요청을 보내지
	// 않는다** — 보내 봐야 거부당하고 240 req/min 예산만 쓴다.
	retryAt time.Time
	// lockUnknown 은 잠금 시각을 못 읽었다는 뜻이다. 그래도 취소는 시도한다 —
	// 모른다는 이유로 시도하지 않으면 주문이 회차 끝까지 남는다.
	lockUnknown bool
	// filledBefore 는 이 주문을 낸 시점의 회차 누적 체결 주수다. 지금 이
	// 주문에 귀속된 체결은 `st.filledShares - filledBefore` 다.
	//
	// **회차 누적만으로는 안 된다.** 앞선 주문이 5주 체결되고 사라진 뒤 8주
	// 짜리 새 주문을 내면, 누적(5)과 새 주문의 수량(8)을 그냥 비교하는 것은
	// 아무 뜻이 없다. 기준점이 있어야 "이 주문이 얼마나 찼는가" 를 말할 수 있다.
	filledBefore float64
}

// roundState 는 한 회차 동안의 우리 상태다. Runner 에 두지 않는 이유: Runner
// 는 여러 회차에 재사용되고, 전 회차의 노출이 다음 회차로 새면 안 된다.
type roundState struct {
	// live 는 지금 걸려 있는 주문이다. 이 봇은 회차당 **두 건**을 건다 —
	// 테이커 다리(최우선 매도호가)와 메이커 다리([LimitPrice]). 걸린 순서대로
	// 들어 있고, 그 순서가 [roundState.retireFullyFilled] 의 귀속 순서다.
	live []*openOrder
	// pending 은 취소를 요청했지만 거래소가 사라졌다고 확인해 주지 **않은**
	// 주문이다. 명목은 여전히 노출에 있다.
	pending []*openOrder
	// confirming 은 거래소가 "호가창에 없다"고 확인해 준 주문이다. 그런데
	// **체결된 주문도 호가창에 없다** — 그러니 아직 이 주문이 돈을 썼는지
	// 안 썼는지 모른다. [Orders.Filled] 가 답할 때까지, 또는 체결 피드가
	// 따라잡을 때까지 명목을 노출에 남긴다([roundState.exposure] 참고).
	confirming []*openOrder
	// confirmedFilled* 는 단건 조회로 **확정된** 체결이다. 체결 피드보다
	// 앞서 도착한다(피드는 2.6~4.6초 늦다).
	confirmedFilledNotional float64
	confirmedFilledShares   float64
	// unknownNotional 은 생성 결과를 모르고 식별자도 없어서 취소조차 할 수
	// 없는 주문의 명목이다. 회차가 끝날 때까지 노출에 남는다 — 그 주문이
	// 존재한다면 체결될 수 있고, 우리는 그것을 막을 수단이 없다.
	unknownNotional float64
	// filledNotional 은 Fills 가 보고한 체결 명목 누적이다.
	filledNotional float64
	// filledShares 는 그 체결의 주수 누적이다.
	//
	// 노출 회계에는 쓰이지 않는다(한도가 재는 것은 나간 돈이다). 밖으로
	// 내보내는 이유는 **감시가 손익을 계산하려면 주수가 필요하기 때문**이다 —
	// 이기면 주당 $1 을 받으므로 배당은 주수로만 정해진다. 모니터는 봇의
	// 원장을 읽을 수 없으므로(다른 호스트다) 이 값이 유일한 경로다.
	filledShares float64

	// lastActionAt 은 이 회차에 주문 생성을 마지막으로 **시도한** 시각이다
	// ([Runner.transmit] 이 찍는다).
	//
	// **정상 회차에서는 한 번만 움직인다.** 예전에는 재호가마다 갱신됐고 정체가
	// 곧 고장이었지만, 지금은 걸고 나면 회차가 끝날 때까지 그대로다. 루프가
	// 살아 있는지는 [Observation.LastLoopAt] 만이 답할 수 있다.
	//
	// 제로면 이 회차에 주문 요청이 한 번도 나가지 않았다는 뜻이다 — 자격 미달,
	// 자본 부족, 체결 조회 실패가 그 경우다.
	lastActionAt time.Time
}

// exposure 는 지금 이 회차가 쓴 것으로 **간주해야 하는** 돈이다.
//
// # 체결분이 max 인 이유
//
// 같은 체결을 두 경로로 알게 된다:
//
//	체결 피드   느리다(실측 2.6~4.6초). 하지만 결국 전부 온다.
//	단건 조회   빠르다(~30ms). 하지만 취소를 확인한 주문에 대해서만 묻는다.
//
// 둘을 더하면 같은 돈을 두 번 세고, 어느 하나만 쓰면 상대가 아는 것을 놓친다.
// 그래서 **max** 다 — 피드가 따라잡으면 두 값이 같아지고, 그때도 max 는
// 그대로다.
//
// 그리고 아직 답을 못 들은 주문(confirming)은 **전액 찼다고 친다.** 모르는
// 것을 안 찼다고 치는 쪽이 2026-08-11 의 노출 2배를 만들었다. 이쪽으로 틀리면
// 덜 걸 뿐이다.
func (st *roundState) exposure() risk.Exposure {
	worst := st.confirmedFilledNotional
	for _, o := range st.confirming {
		worst += o.notional
	}
	x := risk.Exposure{
		FilledNotional: math.Max(st.filledNotional, worst),
		PendingCancel:  st.unknownNotional,
	}
	for _, o := range st.live {
		x.OpenNotional += o.notional
	}
	for _, o := range st.pending {
		x.PendingCancel += o.notional
	}
	return x
}

// filledSharesKnown 은 지금까지 찬 것으로 아는 주수다. 명목과 같은 이유로
// max 다.
//
// **[openOrder.filledBefore] 의 기준점이 이 값이어야 한다.** 피드 값만 쓰면,
// 단건 조회로 먼저 알게 된 체결이 나중에 피드로 또 도착할 때 그것이 *다음*
// 주문의 몫으로 계산된다 — 한 주도 안 찬 주문이 전량 체결로 보이고, 아무도
// 그것을 취소하지 않는다.
func (st *roundState) filledSharesKnown() float64 {
	return math.Max(st.filledShares, st.confirmedFilledShares)
}

// filledBaseline 은 **새 주문에 붙일 기준점**이다. 아는 체결에 더해, 아직
// 답을 못 들은 주문이 **전량 찼다고** 가정한 몫까지 포함한다.
//
// # 왜 filledSharesKnown 으로는 부족한가
//
// 확인 대기 중인 주문 A(10주 @0.20, 명목 2.0)가 있고 그 사이에 B(5주 @0.48)를
// 냈다고 하자. B 의 기준점이 0 이면, 나중에 A 의 10주가 체결 피드로 도착할 때
// `10 − 0 ≥ 5` 가 되어 **한 주도 안 찬 B 가 전량 체결로 보인다.**
// retireFullyFilled 가 B 를 추적에서 빼고, 아무도 B 를 취소하지 않는다 —
// 잊힌 매수 주문은 체결된다.
//
// A 의 몫을 미리 얹어 두면 그 오귀속이 일어나지 않는다. A 가 실제로는 안
// 찼을 때 B 가 물러나지 못하는 부작용이 남지만, 그쪽은 **덜 거는 방향**이고
// B 는 회차 끝에 어차피 취소된다.
func (st *roundState) filledBaseline() float64 {
	base := st.filledSharesKnown()
	for _, o := range st.confirming {
		base += o.shares
	}
	return base
}

// fillEpsilon 은 주수 비교의 여유다. 거래소는 wei 정수를, 우리는 float 을
// 들고 있어 마지막 자리가 갈릴 수 있다. 실측 체결은 9.000000 대 9 였지만
// 그 일치에 기대지 않는다.
const fillEpsilon = 1e-9

// retireFullyFilled 는 **전량 체결된 주문을 추적에서 뺀다.**
//
// # 왜 필요한가 — 같은 돈을 두 번 센다
//
// 체결이 들어오면 filledNotional 이 늘어난다. 그런데 그 주문은 여전히
// st.live 에 남아 미체결로도 세어진다. 2026-08-11 첫 실체결에서 그대로
// 일어났다: 체결 4.41 + 미체결 4.41 = 8.82 대 cap 4.56. 모니터가 노출
// 불변식 위반으로 잡았다.
//
// 방향 자체는 보수적이다(덜 걸게 된다). 그러나 회계가 틀렸고, 더 나쁜 것은
// **회차 종료 때 이미 사라진 주문을 취소하려 든다는 것**이다. 그 취소는
// Removed 도 Noop 도 아닌 응답으로 돌아와 미확인으로 남고, 회차가 "사람이
// 확인해야 한다" 로 끝난다. 체결이 나는 회차마다 그렇게 된다.
//
// # 추적 중인 주문이 하나일 때만 한다
//
// 체결에는 주문 식별자가 없다([Fills] 문서). 그래서 여러 주문을 들고 있을
// 때 어느 것이 찼는지 확정할 수 없고, **잘못 짚으면 살아 있는 주문을
// 추적에서 놓친다** — 그러면 아무도 그것을 취소하지 않고, 잊힌 매수 주문은
// 체결된다. 이 패키지가 가장 피하려는 상태다.
//
// 그래서 취소 대기가 있을 때는 손대지 않는다 — 그 주문도 아직 체결될 수 있고,
// 그러면 어느 것이 찼는지 말할 수 없다.
//
// # 다리가 둘일 때 (2026-08-14)
//
// 회차당 두 건을 걸게 되면서 "하나뿐일 때만" 이라는 예전 조건으로는 아무것도
// 물러나지 못하게 됐다. 그러면 체결된 다리의 명목이 미체결로도 계속 세어져
// 노출이 cap 을 넘고, 감시가 회차마다 불변식 위반을 울린다 — 2026-08-11 에
// 실제로 일어났던 그 상태다.
//
// 그래서 **걸린 순서대로 귀속한다.** 테이커 다리가 먼저 걸리고, 그 다리는
// 정의상 즉시 관통해 먼저 체결된다. 메이커 다리는 시장이 [LimitPrice] 까지
// 내려와야 체결되므로 뒤다. 순서가 곧 체결 순서라는 뜻이고, 그 가정이 깨지는
// 경우(테이커가 관통에 실패)에는 앞 다리가 물러나지 못한 채 남는다 — **덜 거는
// 방향**이라 안전하다.
//
// 앞 다리에 이미 귀속한 주수는 빼고 다음 다리를 본다. 그러지 않으면 한 번의
// 체결이 두 다리를 동시에 물러나게 한다.
func (st *roundState) retireFullyFilled() {
	if len(st.live) == 0 || len(st.pending) > 0 {
		return
	}
	known := st.filledSharesKnown()
	attributed := 0.0
	keep := st.live[:0]
	retiring := true
	for _, o := range st.live {
		if !retiring || o.shares <= 0 {
			retiring = false
			keep = append(keep, o)
			continue
		}
		if known-o.filledBefore-attributed+fillEpsilon >= o.shares {
			attributed += o.shares
			continue
		}
		// 이 다리가 덜 찼으면 뒤 다리도 볼 수 없다 — 체결에 식별자가 없으므로
		// 남은 체결이 어느 쪽 것인지 말할 근거가 사라진다.
		retiring = false
		keep = append(keep, o)
	}
	st.live = keep
}

// hasID 는 이 식별자를 이미 들고 있는지 본다.
func (st *roundState) hasID(id string) bool {
	if id == "" {
		return false
	}
	for _, o := range st.live {
		if o.id == id {
			return true
		}
	}
	for _, o := range st.pending {
		if o.id == id {
			return true
		}
	}
	// confirming 도 본다. 같은 식별자를 든 주문이 둘이면 단건 조회의 답을
	// 어느 쪽에 붙일지 알 수 없다.
	for _, o := range st.confirming {
		if o.id == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// RunRound
// ---------------------------------------------------------------------------

// RunRound 는 회차 하나를 T 부터 T+300 까지 운용한다.
//
// f 는 회차 시작에 동결된 예측이고 e 는 회차 시작에 한 번 조회한 자본이다.
// **둘 다 값으로 받아 회차 내내 고정한다** — 회차 중에 다시 계산하거나 다시
// 조회하는 경로는 존재하지 않는다.
//
// 돌아올 때 우리 주문은 하나도 남아 있지 않아야 한다. 남았으면 에러다.
func (r *Runner) RunRound(ctx context.Context, rd live.Round, f live.Frozen, e risk.Equity) error {
	if err := r.check(rd, f); err != nil {
		return err
	}
	if !f.Eligible {
		r.logf("회차 %s: confidence %.4f < %.4f — 아무것도 하지 않는다", rd.Slug, f.Confidence, live.ConfidenceThreshold)
		return nil
	}
	tokenID, err := tokenFor(rd, f.Direction)
	if err != nil {
		return err
	}
	// 지정가는 회차 시작에 한 번 정한다. 정밀도가 이 가격을 표현하지 못하면
	// **주문이 하나도 나가기 전에** 여기서 멈춘다 — 근사한 가격으로 거는 것은
	// 사용자가 정하지 않은 값에 베팅하는 것이다.
	tick, err := limitTick(rd.Precision)
	if err != nil {
		return err
	}
	// 이 회차에 오더북을 어느 쪽으로 읽을지 여기서 한 번 정한다. 루프 안에서
	// 매 바퀴 다시 정하면 방향이 바뀔 수 있는 자리가 생긴다 — 방향은 동결값이
	// 정하는 것이고 회차 내내 하나여야 한다.
	view, err := newBookView(f.Direction, rd.Precision)
	if err != nil {
		return err
	}

	st := &roundState{}
	loopErr := r.loop(ctx, rd, f, e, tokenID, st, tick, view)
	// 회차가 어떻게 끝났든 — 정상 종료든, 무장 해제든, ctx 취소든 — 살아 있는
	// 주문은 반드시 거둔다. ctx 가 이미 죽었어도 취소는 나가야 하므로
	// WithoutCancel 로 새 시한을 판다.
	cancelErr := r.cancelEverything(ctx, st)
	// 회차가 끝난 뒤 한 번 더 관측한다. **이 마지막 관측이 "회차가 깨끗이
	// 끝났는가" 를 말한다** — 여기에 미체결이 남아 있으면 그것이 곧 사고다.
	r.observe(st)
	if loopErr != nil {
		return loopErr
	}
	return cancelErr
}

// Observation 은 회차 한 바퀴의 우리 상태를 밖으로 복사한 것이다.
//
// roundState 를 그대로 내보내지 않는 이유: 그것은 포인터를 담고 있고, 밖에서
// 읽는 동안 루프가 고쳐 쓴다. 값 복사만 내보내면 경합이 없다.
//
// **이 훅은 관측 전용이다.** 돌려주는 값이 없고, 여기서 나온 것이 회차 진행에
// 영향을 주는 경로도 없다. 있으면 감시자가 거래를 바꾸게 되고, 그것은 P6
// 설계서 §9 의 단방향 불변식을 정반대로 깨는 일이다.
// exec_test.go 의 TestObserveSignatureReturnsNothing 이 리플렉션으로 고정한다.
//
// **`internal/beat` 를 임포트하지 않는다.** 계약 타입이 이 패키지에 새면
// 그 계약이 바뀔 때마다 exec 테스트가 깨진다. 이미 쓰는 risk.Exposure 로
// 표현하고, 변환은 배선(cmd/gld91)의 몫이다.
type Observation struct {
	Exposure risk.Exposure
	// OpenIDs·OpenTicks·OpenNotionals 는 같은 순서의 병렬 슬라이스다.
	// 지금 걸린 주문과 취소 미확인 주문을 모두 담는다 — 후자도 아직 체결될
	// 수 있으므로 밖에서는 둘을 구분할 이유가 없다.
	OpenIDs       []string
	OpenTicks     []int64
	OpenNotionals []float64
	// Unaccounted 는 생성 결과를 모르고 식별자도 없어 취소조차 못 하는 주문의
	// 명목이다. 회차가 끝날 때까지 노출에 남는다.
	Unaccounted float64
	// FilledShares 는 체결 주수 누적이다. 명목(Exposure.FilledNotional)과
	// 별개로 필요하다 — 이기면 주당 $1 이므로 배당은 주수로만 정해진다.
	FilledShares float64
	// LastActionAt 은 이 회차에 주문 생성을 마지막으로 시도한 시각이다.
	// 정상 회차에서는 한 번만 움직이고, 제로면 요청이 한 번도 나가지 않았다.
	LastActionAt time.Time
	// LastLoopAt 은 이 관측이 만들어진 시각, 즉 **루프가 마지막으로 한 바퀴를
	// 돈 시각**이다. 행동(LastActionAt)과 다르다.
	//
	// 두 값을 나누는 이유가 실측에서 나왔다. 예전에도 한 번 걸고 군중이 안
	// 움직이면 LastActionAt 이 회차 내내 멈췄고 — 정상이었다 — 그것을 정체로
	// 읽으면 회차마다 Crit 이 울렸다. 늘 울리는 경보 옆의 진짜 Crit 은 묻힌다.
	// 회차당 한 번만 거는 지금은 그 정체가 **모든 회차의 정상 상태**다. 루프가
	// 멎었는가는 **바퀴가 도는가**로만 답할 수 있고, 그 답은 이 값에만 있다.
	//
	// 회차마다 제로에서 시작한다(roundState 가 회차당 새로 만들어진다).
	// 그래서 제로는 "멎었다" 가 아니라 "이 회차의 첫 바퀴가 아직" 이다.
	LastLoopAt time.Time
}

// observe 는 훅을 부른다.
//
// **패닉을 삼킨다.** 관측자가 터졌다고 살아 있는 주문을 든 채 죽으면 취소도
// 못 한다 — 이 패키지 전체의 원칙이다.
func (r *Runner) observe(st *roundState) {
	if r.Observe == nil {
		return
	}
	o := Observation{
		Exposure:     st.exposure(),
		Unaccounted:  st.unknownNotional,
		FilledShares: st.filledSharesKnown(),
		LastActionAt: st.lastActionAt,
		LastLoopAt:   r.now(),
	}
	add := func(od *openOrder) {
		o.OpenIDs = append(o.OpenIDs, od.id)
		o.OpenTicks = append(o.OpenTicks, od.tick)
		o.OpenNotionals = append(o.OpenNotionals, od.notional)
	}
	for _, od := range st.live {
		add(od)
	}
	for _, od := range st.pending {
		add(od)
	}
	defer func() { _ = recover() }()
	r.Observe(o)
}

// check 는 주문이 하나라도 나가기 전에 배선과 회차 메타데이터를 전부 본다.
// 여기를 통과한 뒤에는 패닉할 수 있는 경로가 없어야 한다.
func (r *Runner) check(rd live.Round, f live.Frozen) error {
	if r.Book == nil {
		return errors.New("exec: Book 이 배선되지 않았다")
	}
	if r.Orders == nil {
		return errors.New("exec: Orders 가 배선되지 않았다")
	}
	if r.Fills == nil {
		return errors.New("exec: Fills 가 배선되지 않았다 — 체결을 못 세면 노출 상한을 지킬 근거가 없다")
	}
	if r.Ledger == nil {
		return errors.New("exec: Ledger 가 배선되지 않았다")
	}
	if r.StaleAfter <= 0 {
		return fmt.Errorf("exec: StaleAfter 가 %s 다 — 제로값은 '문턱 없음'이 아니라 '설정 안 됨'이다", r.StaleAfter)
	}
	if r.EntryWindow <= 0 {
		return fmt.Errorf("exec: EntryWindow 가 %s 다 — 제로값은 '창 없음'이 아니라 '설정 안 됨'이다"+
			" (회차 중간 진입을 막는 가드다)", r.EntryWindow)
	}
	// order.Full·order.Ceiling 은 이 범위 밖에서 패닉한다([limitTick] 이 둘 다
	// 부른다). 살아 있는 주문을 들기 전에 여기서 막아야 패닉이 일어날 자리가
	// 없어진다.
	if rd.Precision < 1 || rd.Precision > ws.MaxPrecision {
		return fmt.Errorf("exec: 회차 %s 의 precision 이 %d 다 (1..%d 여야 한다)", rd.Slug, rd.Precision, ws.MaxPrecision)
	}
	if !rd.EndsAt.After(rd.StartsAt) {
		return fmt.Errorf("exec: 회차 %s 의 endsAt 이 startsAt 보다 뒤가 아니다", rd.Slug)
	}
	if !f.Eligible {
		return nil // 아래 대조는 실제로 걸 때만 의미가 있다
	}
	// 동결값이 이 회차의 것인지 본다. 다르면 다른 회차의 p_up 으로 이 회차에
	// 거는 것이고, 그 사실은 어떤 로그에도 남지 않는다.
	if f.T != rd.StartMS() {
		return fmt.Errorf("exec: 동결된 예측이 다른 회차의 것이다 (동결 t=%d, 회차 시작=%d)", f.T, rd.StartMS())
	}
	// 문턱을 여기서 한 번 더 본다. live.Freeze 가 이미 정했지만 exec 는 주문이
	// 나가기 전 마지막 관문이고, 문턱은 사용자가 정한 값이라 배선
	// 실수로 우회되면 안 된다. Eligible 플래그만 믿으면 손으로 만든 Frozen 하나가
	// 그 제약을 통째로 지운다.
	if !(f.Confidence >= live.ConfidenceThreshold) {
		return fmt.Errorf("exec: Eligible 인데 confidence 가 %v 다 (문턱 %v) — 동결값이 스스로 어긋난다",
			f.Confidence, live.ConfidenceThreshold)
	}
	// 방향과 확률이 같은 쪽을 가리키는지 본다. 어긋나면 매 회차 정확히 반대에
	// 베팅하는 봇이 되고, 승률로만 보면 "모델이 나쁘다"로 읽힌다.
	if (f.PUp > 0.5) != (f.Direction == ledger.OutcomeUp) {
		return fmt.Errorf("exec: p_up %v 와 방향 %q 가 어긋난다", f.PUp, f.Direction)
	}
	return nil
}

// tokenFor 는 방향에 맞는 CTF 토큰 ID 를 고른다.
//
// 문자열을 정확히 두 철자만 받는다. "up" 을 통과시키면 어느 쪽으로도 매칭되지
// 않아 빈 토큰 ID 가 주문에 실린다 — 존재하지 않는 토큰에 걸리거나 서명
// 단계에서 조용히 0 이 된다.
func tokenFor(rd live.Round, direction string) (string, error) {
	switch direction {
	case ledger.OutcomeUp:
		return rd.UpTokenID, nil
	case ledger.OutcomeDown:
		return rd.DownTokenID, nil
	}
	return "", tokenDirectionError(direction)
}

// tokenDirectionError 는 모르는 방향 문자열에 대한 하나뿐인 에러다. 방향을
// 해석하는 자리가 둘(토큰 선택, 호가창 거울)이므로 문구를 한 곳에 둔다.
func tokenDirectionError(direction string) error {
	return fmt.Errorf("exec: 모르는 방향 %q (%q 또는 %q 여야 한다)", direction, ledger.OutcomeUp, ledger.OutcomeDown)
}

// loop 는 회차가 끝날 때까지 도는 본체다.
//
// # 이 루프가 하는 일은 세 가지뿐이다
//
//	다리를 건다     회차 시작에 테이커·메이커 각 한 건([buildLegs]).
//	                그 뒤로는 걸지 않는다.
//	체결을 센다     원장에 적고 노출에 더한다.
//	거둔다          회차가 끝나면 미체결을 전량 취소한다(cancelEverything).
//
// **주문을 옮기는 경로가 없다.** 재호가도, 취소 후 재주문도 없다. 걸린 주문은
// 회차가 끝날 때까지 그 자리에 있다 — 그것이 이 전략의 전부다. 테이커 가격도
// 첫 시도 때 한 번 읽고 얼린다.
func (r *Runner) loop(ctx context.Context, rd live.Round, f live.Frozen, e risk.Equity, tokenID string, st *roundState, tick order.Tick, view bookView) error {
	// placed 는 "이 회차에서 주문을 낼 일이 끝났다"는 뜻이다. 성공했거나, 낼
	// 수 없었거나, 재시도를 다 썼거나, 진입 창이 지났다.
	placed := false
	attempts := 0
	var nextAttempt time.Time
	// legs 는 첫 시도 직전에 만들어진다. nil 인 동안에는 아직 아무 다리도
	// 정해지지 않았다는 뜻이다.
	var legs []*leg
	// firstPass 는 이 회차의 첫 바퀴다. 체결 조회를 건너뛸 수 있는 유일한
	// 바퀴이고([skipFirstFillPoll]), 그 뒤로는 매 바퀴 묻는다.
	firstPass := true

	// **진입 창은 회차 시작에서 잰다.** 우리가 이 회차를 언제 잡았는지가
	// 아니라 회차가 언제 시작했는지가 기준이다 — 늦게 잡은 것이 늦게 걸어도
	// 되는 이유가 될 수는 없다. p_up 은 시작 시점의 값이다.
	entryDeadline := rd.StartsAt.Add(r.EntryWindow)

	for {
		now := r.now()
		if !now.Before(rd.EndsAt) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		// 1) 체결을 먼저 센다. 노출 불변식의 첫 항이고, 이 값을 모르면 신규
		//    주문을 낼 근거가 없다.
		fillsOK := true
		if r.skipFirstFillPoll(rd, now, firstPass) {
			// 이 회차가 우리가 보는 앞에서 시작했고 아직 아무것도 걸지 않았다.
			// 우리 체결이 존재할 수 없으므로 물어볼 것이 없다([skipFirstFillPoll]).
		} else if err := r.absorbFills(ctx, rd, st); err != nil {
			if isDisarm(err) {
				return err
			}
			fillsOK = false
			r.logf("회차 %s: 체결 조회 실패 — 이 주기에는 신규 주문을 내지 않는다: %v", rd.Slug, err)
		}
		firstPass = false

		// 2) 진입 창이 지났으면 이 회차에는 걸지 않는다.
		//
		// **회차 중간에 거는 것은 90초 움직인 시장에 90초 전의 판단으로
		// 베팅하는 것이다.** 늦게 잡힌 회차(재시작 직후, 조회 지연)를 조용히
		// 통과시키면 그 베팅이 원장에서 정상 회차와 구분되지 않는다.
		if !placed && !now.Before(entryDeadline) {
			placed = true
			r.logf("회차 %s: 진입 창 %s 이 지났다(시작 후 %s) — 이 회차에는 걸지 않는다",
				rd.Slug, r.EntryWindow, now.Sub(rd.StartsAt).Truncate(time.Millisecond))
		}

		// 3) 아직 안 걸었으면 건다. **이 블록이 회차당 한 번만 통과한다.**
		//
		// 호가창은 보지 않는다 — 가격이 상수라 볼 이유가 없다. 낡은 호가창을
		// 이유로 미루면 근거 없이 회차의 앞부분을 버리는 일이고, 큐 위치는
		// 앞설수록 좋다.
		if !placed && fillsOK && !now.Before(nextAttempt) {
			// 다리 목록은 **첫 시도 직전에 한 번** 만든다. 테이커 가격이 여기서
			// 얼어붙는다 — 재시도마다 호가창을 다시 읽으면 원장의 어느 줄이 어느
			// 호가에 근거했는지 사후에 말할 수 없다.
			if legs == nil {
				legs = r.buildLegs(tick)
			}
			attempts++
			if err := r.placeLegs(ctx, rd, f, tokenID, e, st, legs, view, now); err != nil {
				return err
			}
			switch {
			case legsDone(legs):
				placed = true
			case attempts >= maxPlaceAttempts:
				// 전부 "아무것도 나가지 않았다"가 확정된 실패였다. 더 두드려도
				// 예산만 쓴다.
				placed = true
				r.logf("회차 %s: 주문 생성이 %d회 연속 실패했다(전부 재시도 안전) — 이 회차는 걸지 않는다", rd.Slug, attempts)
			default:
				nextAttempt = now.Add(r.rejectBackoff())
			}
		}

		// 4) 취소를 확인할 때까지 물고 늘어진다. 취소 재시도는 멱등이라 안전하다.
		//
		//    회차 중에 취소가 생기는 경로는 없지만(옮기지 않으므로) 이 호출은
		//    남겨 둔다 — 생성 결과 불명으로 곧바로 취소 대기에 들어간 주문이
		//    있을 수 있고, 그것을 회차 끝까지 방치하면 잊힌 매수 주문이 된다.
		r.sweepPending(ctx, st, now)
		// 4-1) 취소가 확인된 주문에 "얼마나 찼나"를 묻는다. **이것이 노출을
		//      푸는 유일한 자리다.**
		r.resolveConfirming(ctx, st, r.now())

		// 5) 바깥에 우리 상태를 복사해 준다. 관측 전용이다 — 이 호출의 결과가
		//    회차 진행에 영향을 주는 경로는 없다.
		r.observe(st)

		if err := r.sleep(ctx, r.poll()); err != nil {
			return err
		}
	}
}

// leg 는 이 회차의 다리 하나다. **2026-08-14 부터 다리는 하나다** — 메이커뿐.
//
// 목록 구조를 남겨 둔 이유는 [roundState] 가 이미 여러 다리를 다루기 때문이다.
// 하나로 되돌리려고 그 기계를 걷어내면 체결 귀속([roundState.retireFullyFilled])
// 을 다시 쓰게 되는데, 그건 이 변경이 요구하지 않은 위험이다. n=1 은 그 기계의
// 특수한 경우일 뿐 별도 경로가 아니다.
//
// **가격은 회차마다 한 번 정해 얼려 둔다.** 재시도가 일어나도 다시 읽지 않는다 —
// 원장의 어느 줄이 어느 가격 정책으로 나갔는지 사후에 말할 수 있어야 한다.
type leg struct {
	// name 은 로그에만 쓴다.
	name string
	tick order.Tick
	// done 은 이 다리에 더 시도하지 않는다는 뜻이다 — 걸렸거나, 걸 수 없거나,
	// 결과를 모른다.
	done bool
	// skipped 는 아예 걸지 않기로 한 다리다. 지금은 아무도 이 값을 켜지 않는다 —
	// 메이커 가격은 상수라 "몰라서 못 거는" 경우가 없다. 테이커 다리가 쓰던
	// 자리이고, 다리가 다시 늘면 그때 쓴다.
	skipped bool
}

// buildLegs 는 이 회차의 다리 목록을 만든다.
//
// **메이커 하나뿐이다.** 테이커 다리는 2026-08-14 에 제거됐다(사용자 결정) —
// 실측 123회차에서 평균 0.5103 에 사서 승률 48.8%, 손익분기 52.8% 로 -17.22
// 였다. 모델의 최근 실력(2023~ 51.8%)으로는 호가를 관통해 사는 값을 감당할 수
// 없다는 뜻이다.
//
// 오더북은 이제 어떤 결정에도 들어가지 않는다 — 로그뿐이다(book.go).
func (r *Runner) buildLegs(maker order.Tick) []*leg {
	return []*leg{{name: "메이커", tick: maker}}
}

// placeLegs 는 아직 끝나지 않은 다리를 건다. 한 바퀴에 남은 다리를 모두 시도한다.
//
// 다리마다 예산이 따로다([legBudget]) — 한쪽이 커져서 다른 쪽을 굶기지 않는다.
func (r *Runner) placeLegs(ctx context.Context, rd live.Round, f live.Frozen, tokenID string,
	e risk.Equity, st *roundState, legs []*leg, view bookView, now time.Time) error {
	for _, lg := range legs {
		if lg.done {
			continue
		}
		done, err := r.placeLeg(ctx, rd, f, tokenID, e, st, lg, view, now)
		if err != nil {
			return err
		}
		lg.done = done
	}
	return nil
}

// placeLeg 는 다리 하나를 건다. 크기는 risk 가 정한다.
//
// done=true 는 **이 다리에 더 시도하지 말라**는 뜻이다 — 주문이 존재하거나,
// 존재할 수 있거나, 이 회차에서는 낼 수 없다. done=false 는 아무것도 나가지
// 않았음이 확정된 경우뿐이고, 그때만 호출자가 다시 부른다.
func (r *Runner) placeLeg(ctx context.Context, rd live.Round, f live.Frozen, tokenID string,
	e risk.Equity, st *roundState, lg *leg, view bookView, now time.Time) (done bool, err error) {
	budget := legBudget(e, st.exposure())
	shares := risk.Shares(budget, lg.tick.Float())
	if shares <= 0 {
		// 자본은 회차 시작에 동결된 값이다. 앞 다리가 이미 나갔다면 노출이
		// 늘어 예산이 줄 수는 있지만, 그 값도 이 바퀴에서 이미 반영됐다 —
		// 다시 물어도 같은 답이므로 이 다리는 여기서 끝이다.
		r.logf("회차 %s: %s 다리 — 예산 %.4f USD 로는 %v 에 유효한 주문을 만들 수 없다",
			rd.Slug, lg.name, budget, lg.tick.Float())
		return true, nil
	}
	req := Request{Round: rd, Outcome: f.Direction, TokenID: tokenID, Tick: lg.tick, Shares: shares}
	r.logf("회차 %s: %s 다리 — %s %.4f주 @ %v (명목 %.4f, 예산 %.4f) — %s",
		rd.Slug, lg.name, f.Direction, shares, lg.tick.Float(), req.Notional(), budget, r.marketNote(view))
	return r.transmit(ctx, st, req, now)
}

// firstFillPollGrace 는 "회차가 막 시작했다"로 보는 창이다.
//
// 진입 창(5초)보다 짧다. 늦게 잡힌 회차에서는 그 사이에 무슨 일이 있었는지
// 알 수 없으므로 물어보는 쪽으로 간다.
const firstFillPollGrace = 2 * time.Second

// skipFirstFillPoll 은 이 회차의 **첫 바퀴에서 체결 조회를 건너뛸 수 있는지**
// 본다.
//
// # 왜 건너뛰나 — 임계 경로에서 REST 한 번을 뺀다
//
// REST 클라이언트는 요청 사이에 333ms 를 강제로 끼운다(`rest.minInterval`,
// 3 req/s). 회차 시작에 나가는 요청이 셋이라(equity → 체결 조회 → 주문 생성)
// 주문이 나가기까지 실측 830ms 가 걸렸고 그 대부분이 대기였다. 가운데 하나를
// 빼면 테이커 다리가 그만큼 일찍 나간다 — **테이커 가격은 호가창에서 읽으므로
// 읽은 시점과 주문이 닿는 시점이 벌어질수록 빗나간다.**
//
// # 왜 안전한가, 그리고 어디서 안전하지 않은가
//
// 회차가 방금 시작했고 우리가 아직 아무것도 걸지 않았다면 **이 회차에 우리
// 체결이 존재할 수 없다.** 조회해 봐야 빈 배열이다.
//
// 그 말이 성립하지 않는 경우가 하나 있다: **직전 프로세스가 이 회차에 주문을
// 내고 죽은 뒤 재시작한 경우.** 그때 새 프로세스의 roundState 는 비어 있지만
// 거래소에는 우리 체결이 있고, 그것을 못 세면 노출 상한이 그만큼 늘어난다.
//
// 그래서 세 조건을 모두 요구한다:
//
//	firstPass          이 회차의 첫 바퀴다. 두 번째 바퀴부터는 늘 묻는다.
//	방금 시작           now 가 회차 시작 + firstFillPollGrace 안이다.
//	우리가 먼저 있었다   [Runner.StartedAt] 이 회차 시작보다 앞이다 —
//	                   즉 이 프로세스는 회차가 시작하기 전부터 살아 있었다.
//
// 마지막 조건이 재시작 구멍을 막는다. 재시작한 프로세스는 StartedAt 이 회차
// 시작보다 뒤이므로 언제나 묻는다.
//
// StartedAt 이 제로면 **건너뛰지 않는다** — 배선하지 않은 것을 "언제나
// 안전하다"로 읽으면 안 된다.
func (r *Runner) skipFirstFillPoll(rd live.Round, now time.Time, firstPass bool) bool {
	if !firstPass || r.StartedAt.IsZero() {
		return false
	}
	if !r.StartedAt.Before(rd.StartsAt) {
		return false
	}
	return now.Before(rd.StartsAt.Add(firstFillPollGrace))
}

// legsDone 은 모든 다리가 끝났는지 본다.
func legsDone(legs []*leg) bool {
	for _, lg := range legs {
		if !lg.done {
			return false
		}
	}
	return true
}

// marketNote 는 주문을 낸 순간의 시장 모습이다.
//
// **로그 문자열 하나로만 나간다.** 이 값을 읽어 가격이나 수량을 바꾸는 경로는
// 없고, 생기면 그것은 전략 변경이다(book.go 참고). 그럼에도 남기는 이유:
// "그때 좋은 가격이었나" 를 사후에 답할 근거가 이것뿐이다 — 특히
// 매도호가가 [LimitPrice] 이하였다면 메이커 다리도 관통해 테이커 수수료를 문 것이고,
// 그 사실은 여기 아니면 어디에도 남지 않는다.
func (r *Runner) marketNote(view bookView) string {
	s := view.sides(r.Book)
	note := fmt.Sprintf("시장 매수 %s / 매도 %s", view.label(s.BestBid, s.HasBid), view.label(s.BestAsk, s.HasAsk))
	if r.Book.Stale(r.monoNs(), r.StaleAfter.Nanoseconds()) {
		note += fmt.Sprintf(" (호가창이 %s 넘게 낡았다 — 이 값은 믿을 수 없다)", r.StaleAfter)
	}
	return note
}

// transmit 은 이 패키지에서 [Orders.Create] 를 부르는 **유일한** 자리다.
// 주문이 나가는 부수효과를 한 곳에 모아 두면 실패 분류도 한 곳에서 끝난다.
//
// done 의 뜻은 [Runner.place] 와 같다.
func (r *Runner) transmit(ctx context.Context, st *roundState, req Request, now time.Time) (done bool, err error) {
	res, err := r.Orders.Create(ctx, req)
	// **요청이 나간 순간이 행동이다.** 결과가 무엇이든 우리는 호가창에 손을
	// 댔다. 여기가 아니라 "성공했을 때" 만 찍으면, 낼 수 없어서 아무것도 안 한
	// 회차와 냈는데 결과를 모르는 회차가 밖에서 똑같아 보인다.
	st.lastActionAt = now
	if err == nil && res.ID == "" {
		// 성공이라는데 식별자가 없다. 이 주문은 취소할 수 없다.
		err = errors.New("주문 생성 응답에 식별자가 없다")
		res = CreateResult{}
	}
	if res.ID != "" && st.hasID(res.ID) {
		// 이미 들고 있는 식별자가 또 왔다. 그대로 담으면 취소 확인 한 번이
		// **두 주문의 명목을 함께** 노출에서 빼 준다 — 한도가 조용히 늘어난다.
		// 추적할 수 없는 주문으로 다룬다.
		err = fmt.Errorf("주문 식별자 %q 가 중복이다 — 이 주문은 추적할 수 없다", res.ID)
		res = CreateResult{}
	}
	if err != nil {
		if safeToRetry(err) {
			// 주문은 존재하지 않는다. 노출에 아무것도 더하지 않고, 호출자가
			// 다시 부를 수 있게 done=false 로 돌아간다 — 그래도 호가창에 우리
			// 주문이 없으므로 "다리당 한 건" 은 깨지지 않는다.
			r.logf("주문 생성 실패(재시도 안전): %v", err)
			return false, nil
		}
		// **보냈을 수 있다.** 다시 보내면 둘 들어간다.
		o := &openOrder{
			id: res.ID, hash: res.Hash, tick: req.Tick.V, shares: req.Shares,
			notional: req.Notional(), placed: now,
			retryAt: retryAtFrom(res, now), lockUnknown: res.RemovalLockUnknown,
		}
		if o.id != "" {
			// 식별자가 있으면 취소는 시도할 수 있다.
			st.pending = append(st.pending, o)
			r.logf("주문 결과 불명 — 재전송하지 않고 취소를 시도한다 (id=%s): %v", o.id, err)
			return true, nil
		}
		st.unknownNotional += o.notional
		r.logf("주문 결과 불명이고 식별자도 없다 — 명목 %.4f 를 회차 끝까지 노출에 남긴다: %v", o.notional, err)
		return true, nil
	}

	st.live = append(st.live, &openOrder{
		id: res.ID, hash: res.Hash, tick: req.Tick.V, shares: req.Shares,
		notional: req.Notional(), placed: now,
		retryAt: retryAtFrom(res, now), lockUnknown: res.RemovalLockUnknown,
		filledBefore: st.filledBaseline(),
	})
	return true, nil
}

// retryAtFrom 은 이 주문에 취소를 시도할 수 있는 가장 이른 시각이다.
//
// 잠금 시각을 알면 그대로 지킨다(길이를 가정하지 않는다). 모르면 **지금**이다 —
// 모른다는 이유로 미루면 주문이 회차 끝까지 남을 수 있고, 거부당하면 그때
// 백오프를 건다. 요청 하나를 낭비하는 쪽이 주문을 잊는 쪽보다 낫다.
func retryAtFrom(res CreateResult, now time.Time) time.Time {
	if res.RemovalLockUnknown || res.LockedUntil.IsZero() {
		return now
	}
	return res.LockedUntil
}

// safeToRetry 는 같은 주문을 다시 보내도 이중 주문이 나지 않는지 본다.
//
// **분류하지 못한 에러는 false 다.** 새로운 실패 모드가 생길 때마다 이중 주문
// 위험이 열리는 것을 막는다 — `rest.classifyCreate` 의 기본값이 OrderUnknown 인
// 것과 같은 규칙이고, 그쪽이 우리에게 분류를 넘겨주지 못하는 경우에도 성립해야
// 한다.
func safeToRetry(err error) bool {
	var c retryClassifier
	if errors.As(err, &c) {
		return c.SafeToRetry()
	}
	return false
}

// beginCancel 은 살아 있는 주문을 취소 대기로 옮긴다.
//
// **명목은 옮겨질 뿐 줄지 않는다.** OpenNotional 에서 PendingCancel 로 갈
// 뿐이고 risk.Remaining 은 둘을 똑같이 뺀다.
func (r *Runner) beginCancel(st *roundState, now time.Time, why string) {
	if len(st.live) == 0 {
		return
	}
	live := st.live
	st.live = nil
	for _, o := range live {
		if o.retryAt.IsZero() {
			o.retryAt = now
		}
		st.pending = append(st.pending, o)
		r.logf("취소 요청 준비 (id=%s, 틱 %d): %s", o.id, o.tick, why)
	}
}

// sweepPending 은 지금 취소를 시도해도 되는 주문들을 한 요청으로 취소한다.
//
// **retryAt 전에는 요청을 보내지 않는다.** removalLockedUntil 안에서 보내면
// 거부당하고 240 req/min 예산만 쓴다.
func (r *Runner) sweepPending(ctx context.Context, st *roundState, now time.Time) {
	if len(st.pending) == 0 {
		return
	}
	var ids []string
	for _, o := range st.pending {
		if now.Before(o.retryAt) {
			continue
		}
		// 배치 상한을 넘기면 거래소 층이 요청 자체를 거절한다 — 그러면
		// **아무것도 취소되지 않는다.** 넘치는 것은 다음 바퀴로 넘긴다.
		if len(ids) >= maxRemoveBatch {
			break
		}
		ids = append(ids, o.id)
	}
	if len(ids) == 0 {
		return
	}

	res, err := r.Orders.Remove(ctx, ids)
	if err != nil {
		// 취소가 나갔는지도 모른다. 재시도는 멱등이므로 물러났다가 다시 한다.
		r.backoff(st, ids, now)
		r.logf("취소 요청 실패 — 백오프 후 재시도한다: %v", err)
		return
	}

	// **"사라졌다"는 "안 찼다"가 아니다.**
	//
	// 예전 이 자리의 주석은 "부분체결분은 Fills 가 따로 세므로 여기서 빼도
	// 이중 계산이 아니다" 라고 적혀 있었다. 그 문장은 체결 피드가 **이미**
	// 그 체결을 셌다고 가정하는데, 피드는 2.6~4.6초 늦게 온다(실측).
	//
	// 그 가정이 2026-08-11 에 회차 명목을 상한의 1.9배로 만들었다. 그러니
	// 사라진 주문은 노출에서 빼는 것이 아니라 **확인 대기로 옮긴다** —
	// 명목은 그대로 두고, [Runner.resolveConfirming] 이 거래소에 얼마나
	// 찼는지 물어본 뒤에 푼다.
	gone := map[string]bool{}
	for _, id := range res.Removed {
		gone[id] = true
	}
	for _, id := range res.Noop {
		gone[id] = true
	}
	stillThere := map[string]bool{}
	for _, id := range res.Rejected {
		stillThere[id] = true
	}
	for _, id := range res.Unaccounted {
		stillThere[id] = true
	}

	kept := st.pending[:0]
	for _, o := range st.pending {
		switch {
		case gone[o.id]:
			o.goneAt = now
			o.askAt = now // 곧바로 물어본다
			st.confirming = append(st.confirming, o)
			r.logf("취소 확인 (id=%s) — 명목 %.4f 는 체결 여부를 확인할 때까지 노출에 남긴다", o.id, o.notional)
			continue
		case stillThere[o.id]:
			o.retryAt = now.Add(r.rejectBackoff())
			o.lockUnknown = true // 남은 잠금 길이를 모른다
		}
		kept = append(kept, o)
	}
	// 잘려 나간 꼬리가 옛 포인터를 붙들지 않게 한다.
	for i := len(kept); i < len(st.pending); i++ {
		st.pending[i] = nil
	}
	st.pending = kept
}

// resolveConfirming 은 취소가 확인된 주문에 **얼마나 찼는지**를 물어 노출을
// 푼다. 노출에서 명목이 빠지는 자리는 이 함수 하나뿐이다.
//
// # 두 가지 방법으로만 풀린다
//
//	거래소가 답했다        찬 만큼만 남기고 나머지를 푼다 (~30ms, 정상 경로)
//	체결 피드가 따라잡았다  피드 값이 진실이 됐으므로 그대로 푼다 (느린 경로)
//
// 두 번째가 필요한 이유: 해시가 없거나 조회가 계속 실패할 수 있다. 그때
// 영영 잠가 두면 회차 하나가 통째로 멈춘다. [Runner.SettleGrace] 만큼
// 기다리면 피드가 그 체결을 이미 실어 왔다고 볼 수 있다 — 실측 지연이
// 2.6~4.6초이므로 기본값은 그보다 넉넉하다.
//
// **에러는 삼킨다.** 조회 실패는 "모른다"이고, 모르는 동안 명목은 노출에
// 남아 있다 — 이미 안전한 쪽이다. 여기서 회차를 죽이면 살아 있는 주문을
// 든 채 멈추는 경로가 하나 더 생긴다.
func (r *Runner) resolveConfirming(ctx context.Context, st *roundState, now time.Time) {
	if len(st.confirming) == 0 {
		return
	}
	grace := r.settleGrace()
	kept := st.confirming[:0]
	for _, o := range st.confirming {
		if o.hash != "" && !now.Before(o.askAt) {
			shares, err := r.Orders.Filled(ctx, o.hash)
			switch {
			case err != nil:
				o.askAt = now.Add(r.rejectBackoff())
				r.logf("주문 %s 의 체결량을 확인하지 못했다 — 명목 %.4f 를 노출에 남긴다: %v", o.id, o.notional, err)
			case shares < 0 || math.IsNaN(shares) || math.IsInf(shares, 0):
				// 값이 말이 안 되면 답을 못 들은 것과 같이 다룬다. 음수
				// 체결량을 그대로 더하면 노출이 **줄어든다** — 사고의 방향이다.
				o.askAt = now.Add(r.rejectBackoff())
				r.logf("주문 %s 의 체결량이 %v 다 — 값을 믿지 않고 명목 %.4f 를 노출에 남긴다", o.id, shares, o.notional)
			default:
				// 찬 만큼만 남긴다. 주문 수량을 넘는 답은 그대로 쓰지 않는다
				// (거래소가 다른 단위를 주면 노출이 터무니없이 커진다).
				filled := math.Min(shares, o.shares)
				st.confirmedFilledShares += filled
				if o.shares > 0 {
					st.confirmedFilledNotional += o.notional * (filled / o.shares)
				}
				r.logf("주문 %s 확인 — %.4f/%.4f주 체결(명목 %.4f 중 %.4f 를 쓴 것으로 센다)",
					o.id, filled, o.shares, o.notional, o.notional*safeFrac(filled, o.shares))
				continue
			}
		}
		if !o.goneAt.IsZero() && now.Sub(o.goneAt) >= grace {
			// 피드가 따라잡을 시간이 지났다. 이 주문의 체결은 이미
			// filledNotional 에 들어 있다고 본다.
			r.logf("주문 %s: %s 이 지나 체결 피드에 맡긴다 (명목 %.4f)", o.id, grace, o.notional)
			continue
		}
		kept = append(kept, o)
	}
	for i := len(kept); i < len(st.confirming); i++ {
		st.confirming[i] = nil
	}
	st.confirming = kept
}

// safeFrac 는 로그 문구용 비율이다. 0 으로 나누지 않는다.
func safeFrac(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// backoff 는 이번에 시도한 ID 들만 뒤로 민다.
func (r *Runner) backoff(st *roundState, ids []string, now time.Time) {
	tried := make(map[string]bool, len(ids))
	for _, id := range ids {
		tried[id] = true
	}
	at := now.Add(r.rejectBackoff())
	for _, o := range st.pending {
		if tried[o.id] {
			o.retryAt = at
		}
	}
}

// cancelEverything 은 회차 종료(또는 중단) 시 남은 주문을 전부 거둔다.
//
// ctx 가 이미 죽었어도 취소는 나가야 한다 — 살아 있는 매수 주문을 두고 나가면
// 그 노출은 어떤 한도에도 잡히지 않는다. 그래서 취소 전용 컨텍스트를 새로 판다.
func (r *Runner) cancelEverything(ctx context.Context, st *roundState) error {
	if len(st.live) > 0 {
		r.beginCancel(st, r.now(), "회차 종료 — 미체결 전량 취소")
	}
	if len(st.pending) == 0 && len(st.confirming) == 0 {
		return r.leftovers(st)
	}

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.finalCancelTimeout())
	defer cancel()

	deadline := r.now().Add(r.finalCancelTimeout())
	for {
		now := r.now()
		r.sweepPending(cctx, st, now)
		// 회차가 끝나도 **얼마나 찼는지는 알아야 한다.** 그 값이 마지막
		// 관측으로 나가고, 감시는 그것으로 이 회차의 배당을 계산한다.
		r.resolveConfirming(cctx, st, r.now())
		if len(st.pending) == 0 && len(st.confirming) == 0 {
			break
		}
		if !now.Before(deadline) {
			break
		}
		if err := r.sleep(cctx, r.poll()); err != nil {
			break
		}
	}
	return r.leftovers(st)
}

// leftovers 는 거두지 못한 주문을 에러로 만든다. 조용히 성공하면 그 주문은
// 잊히고, 잊힌 매수 주문은 체결된다.
func (r *Runner) leftovers(st *roundState) error {
	if len(st.pending) == 0 && st.unknownNotional == 0 {
		return nil
	}
	ids := make([]string, 0, len(st.pending))
	for _, o := range st.pending {
		ids = append(ids, o.id)
	}
	// **disarmError 로 감싼다.** 이 문구는 "무장을 풀고 사람이 확인해야 한다"
	// 라고 말하는데, 2026-08-11 실거래에서 봇은 그 말을 하고도 다음 회차로
	// 넘어가 주문을 계속 냈다. 배선이 회차 에러를 전부 똑같이 다뤘기 때문이다
	// (로그만 찍고 계속). 말과 행동이 갈리면 말이 아니라 행동이 진짜다.
	//
	// 여기서 남은 주문은 "거래소에 살아 있을 수 있는데 우리가 모르는" 주문이다.
	// 그 상태로 다음 회차를 시작하면 노출이 두 회차에 걸쳐 겹친다.
	if st.unknownNotional > 0 {
		return &disarmError{err: fmt.Errorf("회차가 끝났는데 우리 주문이 남아 있다 (취소 미확인 %v, 식별자 없는 명목 %.4f) — 사람이 확인해야 한다", ids, st.unknownNotional)}
	}
	return &disarmError{err: fmt.Errorf("회차가 끝났는데 취소를 확인하지 못한 주문이 있다 (%v) — 사람이 확인해야 한다", ids)}
}

// ---------------------------------------------------------------------------
// 체결 흡수
// ---------------------------------------------------------------------------

// disarmError 는 "이 회차를 계속 돌면 안 된다"는 뜻이다. 일시적 조회 실패와
// 구분해야 한다 — 앞은 그 주기를 건너뛰면 되고 뒤는 멈춰야 한다.
type disarmError struct{ err error }

func (e *disarmError) Error() string { return "exec: 무장 해제: " + e.err.Error() }
func (e *disarmError) Unwrap() error { return e.err }

func isDisarm(err error) bool {
	var d *disarmError
	return errors.As(err, &d)
}

// IsDisarm 은 이 에러가 **거래를 멈춰야 하는** 종류인지다. 배선이 회차 에러를
// 가려낼 유일한 수단이다.
//
// 일시적 실패(조회 실패, 한 회차의 설정 오류)와 구분해야 한다 — 앞은 다음
// 회차를 그냥 잡으면 되고, 뒤는 사람이 볼 때까지 새 회차를 잡으면 안 된다.
// 이 구분이 배선에 없어서 2026-08-11 실거래에서 "무장을 풀라"는 에러가 난
// 직후에 봇이 다음 주문을 냈다.
func IsDisarm(err error) bool { return isDisarm(err) }

// absorbFills 는 새 체결을 원장에 적고 노출에 더한다.
//
// # 원장 기록 실패에서 재시도하지 않는 이유
//
// [ledger.ErrInvalidRecord] 만이 "파일을 손대지 않았다"를 보장한다. 그 밖의
// 에러(디스크 가득 참, I/O 실패)는 줄이 이미 들어갔을 수도 있고, append 전용
// 파일이라 되돌릴 수단이 없다. 같은 레코드를 다시 넣으면 중복 체결 줄이 생기고,
// 크래시 복구가 포지션을 실제보다 크게 읽어 한도를 넘겨 베팅한다. 그러니
// 대응은 재시도가 아니라 **무장 해제**다.
//
// # 기록하지 못한 체결도 노출에는 남는다
//
// ErrInvalidRecord 로 줄을 버려도 명목은 더한다. 돈은 이미 나갔고 우리가 적지
// 못했을 뿐이다. 노출에서 빼면 그만큼 더 걸게 되는데, 그것이야말로 한도를
// 넘기는 방향이다.
func (r *Runner) absorbFills(ctx context.Context, rd live.Round, st *roundState) error {
	fills, err := r.Fills.Poll(ctx, rd)
	if err != nil {
		return fmt.Errorf("체결 조회: %w", err)
	}
	for _, f := range fills {
		cost := ledger.FillCost(f)
		if rerr := r.Ledger.RecordFill(f); rerr != nil {
			if !errors.Is(rerr, ledger.ErrInvalidRecord) {
				return &disarmError{err: fmt.Errorf("원장 기록: %w", rerr)}
			}
			r.logf("체결을 원장에 적지 못했다(레코드 불량, 재시도하지 않는다): %v", rerr)
		}
		// **수수료를 뺀 순수 명목이 아니라 FillCost 를 쓴다.** 나간 돈이
		// 한도가 재는 대상이다.
		st.filledNotional += cost
		st.filledShares += f.Shares
	}
	// **체결을 흡수한 직후에 부른다.** 전량 체결된 주문은 호가창에 더 이상
	// 없으므로 미체결로 세면 안 되고, 취소할 대상도 아니다.
	st.retireFullyFilled()
	return nil
}
