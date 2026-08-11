// Package exec 는 한 회차를 실제로 운용한다 — 오더북을 읽고, 목표가에 주문을
// 걸고, 군중을 따라 재호가하고, 회차가 끝나면 미체결을 전량 취소한다.
//
// # 이 패키지는 결정을 내리지 않는다
//
// 돈을 잃는 판단은 전부 `internal/quote`(어느 가격에 걸지, 옮길지 둘지)와
// `internal/risk`(얼마를 걸지, 걸어도 되는지)에 있다. 여기서 하는 일은 그
// 결정을 오더북·주문·시계에 연결하는 배선뿐이다. 이 경계가 무너지는 순간 —
// 예를 들어 여기에 `if bestBid < …` 같은 가격 판단이 하나라도 생기면 — 이
// 패키지는 모의 오더북으로 검증할 수 없게 된다. 값 몇 개로 시험할 수 있던
// 판단이 네트워크·시계·상태에 얽힌 판단이 되기 때문이다.
//
// 그래서 이 파일에는 가격 비교가 없다. 틱을 비교하는 곳은 quote 뿐이고,
// 금액을 비교하는 곳은 risk 뿐이다.
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
// 특히 `quote.Ceiling` 은 precision 이 1..18 밖이면 패닉하므로, 회차의
// precision 을 [Runner.RunRound] 진입부에서 먼저 막는다.
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
	"github.com/kdm000718/GLD-9.1/internal/quote"
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

// maxExcludableShares 는 [ws.Qty] 가 int64 로 표현할 수 있는 최대 주식 수다.
//
// **이 상한을 넘는 주문은 내지 않는다.** 넘으면 ws.Qty 의 int64 변환이 넘쳐
// 음수가 되고, BestBid 의 `qty − exclude[t] <= 0` 검사가 뒤집혀 **우리 주문이
// 호가창에서 제외되지 않는다** — 봇이 자기 호가를 쫓는 바로 그 고장이다.
// 에러도 로그도 없이 일어난다.
//
// 도달 가능한 경로가 실제로 있다: risk.Shares 는 2^53(≈9.0e15) 미만이면
// 통과시키는데 ws.QtyDecimals 가 6 이므로 9.2e12 주만 넘어도 곱이 int64 를
// 넘는다. precision 이 6 이상인 마켓에서 저가 틱이면 그 구간에 들어간다
// (실측된 precision 은 2·3 이지만 ws.MaxPrecision 은 18 을 허용한다).
var maxExcludableShares = float64(math.MaxInt64) / math.Pow(10, ws.QtyDecimals)

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

	// Cooldown 은 재호가 쿨다운이다(기본 500ms 는 배선의 몫 — 여기서는 0 이
	// 유효한 값이다. 테스트가 쿨다운 없는 경우를 만들 수 있어야 한다).
	Cooldown time.Duration
	// Dwell 은 목표가가 굳어야 하는 시간이다. 쿨다운과 **다른 것을 잰다** —
	// 쿨다운은 우리가 마지막으로 낸 뒤의 경과이고, 이쪽은 군중이 새 자리에
	// 머문 시간이다. 근거와 실측은 quote.Dwell 문서에 있다. 0 이면 검사하지
	// 않는다(쿨다운과 같은 규약 — 테스트가 없는 경우를 만들 수 있어야 한다).
	Dwell time.Duration
	// StaleAfter 는 오더북 신선도 문턱이다. 0 이하면 회차를 돌지 않는다 —
	// 제로값이 "항상 stale" 로 읽혀도, "문턱 없음" 으로 읽혀도 둘 다 사고다.
	StaleAfter time.Duration
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

// exposedOrder 는 최우선 호가 계산에서 빼야 할 우리 물량이다.
type exposedOrder struct {
	tick   int64
	shares float64
}

// roundState 는 한 회차 동안의 우리 상태다. Runner 에 두지 않는 이유: Runner
// 는 여러 회차에 재사용되고, 전 회차의 노출이 다음 회차로 새면 안 된다.
type roundState struct {
	// dwell 은 목표가가 굳었는지를 세는 상태다. **회차마다 새로 만든다** —
	// 이전 회차의 목표가를 물려주면 새 회차의 첫 재호가가 남의 나이로
	// 판단된다. 이 패키지는 안을 들여다보지 않는다(틱 비교는 quote 의 몫).
	dwell *quote.Dwell
	// live 는 지금 걸려 있는 주문이다. 이 봇은 한 번에 한 건만 건다.
	live *openOrder
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

	// reprices 는 quote.Reprice 판정 횟수다. 관측 전용이고 판단에 쓰이지 않는다.
	reprices int64
	// lastActionAt 은 마지막으로 주문을 **걸거나 옮긴** 시각이다.
	//
	// 이 값이 정체하면 프로세스는 멀쩡한데 호가창에 아무 일도 하지 않는
	// 상태다 — mtime 하트비트가 절대 잡지 못하는 고장이고, 감시가 이것을
	// 보려면 밖으로 나가야 한다.
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
	if st.live != nil {
		x.OpenNotional = st.live.notional
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
// 그래서 모호하지 않은 경우에만 처리한다: live 하나뿐이고 취소 대기가 없을
// 때. 이 봇은 한 번에 한 건만 걸므로 그것이 압도적인 다수이고, 실제로
// 관측된 경우도 그것이다. 여러 건을 들고 있을 때는 예전처럼 둔다 — 이중
// 계산이 남지만 그쪽은 덜 거는 방향이라 안전하다.
func (st *roundState) retireFullyFilled() {
	if st.live == nil || len(st.pending) > 0 {
		return
	}
	if st.live.shares <= 0 {
		return
	}
	if st.filledSharesKnown()-st.live.filledBefore+fillEpsilon >= st.live.shares {
		st.live = nil
	}
}

// hasID 는 이 식별자를 이미 들고 있는지 본다.
func (st *roundState) hasID(id string) bool {
	if id == "" {
		return false
	}
	if st.live != nil && st.live.id == id {
		return true
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

// ours 는 지금 호가창에 있을 수 있는 우리 물량 전부다. 취소 미확인 주문도
// 포함한다 — 아직 책에 남아 있을 수 있고, 그렇다면 그것도 우리 자신이다.
func (st *roundState) ours() []exposedOrder {
	out := make([]exposedOrder, 0, len(st.pending)+1)
	if st.live != nil {
		out = append(out, exposedOrder{tick: st.live.tick, shares: st.live.shares})
	}
	for _, o := range st.pending {
		out = append(out, exposedOrder{tick: o.tick, shares: o.shares})
	}
	return out
}

// excludeOurs 는 [ws.Book.BestBid] 에 넘길 제외 맵을 만든다.
//
// **수량은 반드시 [ws.Qty] 를 거친다.** 자연 단위 float 을 그대로 넣으면
// 컴파일되지 않지만, 다른 배율의 int64 를 넣으면 조용히 틀린다 — 20주가
// 0.00002주로 취급되어 우리 층이 그대로 남고, 봇은 자기 호가를 쫓는다.
//
// ok=false 는 **우리 주문을 호가창에서 뺄 수 없다**는 뜻이다(수량이 고정소수점
// int64 를 넘는다). 그때 호출자는 그 호가창을 믿으면 안 된다 — 자기 자신을
// 군중으로 착각한 채 판단하게 된다.
func excludeOurs(orders []exposedOrder) (m map[int64]ws.Shares, ok bool) {
	if len(orders) == 0 {
		return nil, true
	}
	m = make(map[int64]ws.Shares, len(orders))
	total := 0.0
	for _, o := range orders {
		if !representable(o.shares) {
			return nil, false
		}
		total += o.shares
		// 같은 틱에 여러 건이 겹치면 합이 넘칠 수 있다. 건별 검사만으로는
		// 부족하다.
		if !representable(total) {
			return nil, false
		}
		m[o.tick] += ws.Qty(o.shares)
	}
	return m, true
}

// representable 은 주식 수가 [ws.Shares] 고정소수점으로 표현되는지 본다.
func representable(shares float64) bool {
	return !math.IsNaN(shares) && !math.IsInf(shares, 0) &&
		shares >= 0 && shares <= maxExcludableShares
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
	// 이 회차에 오더북을 어느 쪽으로 읽을지 여기서 한 번 정한다. 루프 안에서
	// 매 바퀴 다시 정하면 방향이 바뀔 수 있는 자리가 생긴다 — 방향은 동결값이
	// 정하는 것이고 회차 내내 하나여야 한다.
	view, err := newBookView(f.Direction, rd.Precision)
	if err != nil {
		return err
	}

	st := &roundState{dwell: &quote.Dwell{Need: r.Dwell}}
	loopErr := r.loop(ctx, rd, f, e, tokenID, st, view)
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
	Reprices     int64
	LastActionAt time.Time
	// LastLoopAt 은 이 관측이 만들어진 시각, 즉 **루프가 마지막으로 한 바퀴를
	// 돈 시각**이다. 행동(LastActionAt)과 다르다.
	//
	// 두 값을 나누는 이유가 실측에서 나왔다. 한 번 걸고 군중이 안 움직이면
	// quote 는 계속 DoNothing 을 돌려주고 LastActionAt 이 회차 내내 멈춘다 —
	// 정상이다. 그것을 정체로 읽으면 회차마다 Crit 이 울리고, 늘 울리는
	// 경보 옆의 진짜 Crit 은 묻힌다. 루프가 멎었는가는 **바퀴가 도는가**로만
	// 답할 수 있고, 그 답은 이 값에만 있다.
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
		Reprices:     st.reprices,
		LastActionAt: st.lastActionAt,
		LastLoopAt:   r.now(),
	}
	add := func(od *openOrder) {
		o.OpenIDs = append(o.OpenIDs, od.id)
		o.OpenTicks = append(o.OpenTicks, od.tick)
		o.OpenNotionals = append(o.OpenNotionals, od.notional)
	}
	if st.live != nil {
		add(st.live)
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
	if r.Cooldown < 0 {
		return fmt.Errorf("exec: Cooldown 이 음수다 (%s)", r.Cooldown)
	}
	// quote.Ceiling 은 이 범위 밖에서 패닉한다. 살아 있는 주문을 들기 전에
	// 여기서 막아야 패닉이 일어날 자리가 없어진다.
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
	// 나가기 전 마지막 관문이고, 문턱 0.0172 는 사용자가 정한 값이라 배선
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
func (r *Runner) loop(ctx context.Context, rd live.Round, f live.Frozen, e risk.Equity, tokenID string, st *roundState, view bookView) error {
	staleNs := r.StaleAfter.Nanoseconds()
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
		if err := r.absorbFills(ctx, rd, st); err != nil {
			if isDisarm(err) {
				return err
			}
			fillsOK = false
			r.logf("회차 %s: 체결 조회 실패 — 이 주기에는 신규 주문을 내지 않는다: %v", rd.Slug, err)
		}

		// 2) 오더북에서 우리 주문을 뺀 최우선 호가를 읽는다.
		//
		// **exclude 를 우리 미체결 주문으로 채우는 것이 이 줄의 전부이자
		// 요점이다.** 빠뜨리면 BestBid 가 우리 자신을 돌려주고, 목표가가
		// 우리 호가와 같아져 "동일가 유지"로 영원히 굳는다 — 군중이 내려가도
		// 따라 내려오지 못한다. quote 테스트로는 절대 잡히지 않는다.
		//
		// **view 를 거치는 것이 두 번째 요점이다.** 오더북은 마켓당 한 벌이고
		// Up 기준이다 — Down 회차에 그대로 읽으면 남의 책을 보고 가격을
		// 정하게 되고, 실제로 관통해 테이커 수수료를 물었다(book.go 참고).
		ex, exOK := excludeOurs(view.ourTicks(st.ours()))
		// 우리 주문을 뺄 수 없으면 그 호가창은 우리 자신이 섞인 값이다.
		// 오래된 호가창과 똑같이 다룬다 — 믿을 수 없는 책으로는 새로 걸지 않고
		// 걸린 것은 거둔다.
		untrusted := !exOK
		if untrusted {
			r.logf("회차 %s: 우리 주문을 호가창에서 뺄 수 없다(수량이 고정소수점 범위를 넘는다) — 신규 중단·기존 취소", rd.Slug)
		}
		stale := r.Book.Stale(r.monoNs(), staleNs) || untrusted

		// 3) 판단은 quote 가 한다. 여기서는 아무것도 비교하지 않는다.
		qb := view.quoteBook(r.Book, ex)
		qo := quote.Open{}
		if st.live != nil {
			qo = quote.Open{Tick: st.live.tick, Placed: st.live.placed, Live: true}
		}
		d := quote.Decide(qb, qo, now, r.Cooldown, stale, st.dwell)

		switch d.Action {
		case quote.Place:
			if fillsOK {
				if err := r.place(ctx, rd, f, tokenID, e, st, d, now); err != nil {
					return err
				}
				st.lastActionAt = now
			}
		case quote.Reprice, quote.CancelOnly:
			// 취소만 한다. 재주문은 거래소가 취소를 확인해 준 다음 바퀴에
			// 일어난다 — 확인 전에 다시 걸면 그 순간 노출이 두 배다.
			r.beginCancel(st, now, d.Why)
			if d.Action == quote.Reprice {
				st.reprices++
				st.lastActionAt = now
			}
		case quote.DoNothing:
		}

		// 4) 취소를 확인할 때까지 물고 늘어진다. 취소 재시도는 멱등이라 안전하다.
		r.sweepPending(ctx, st, now)
		// 4-1) 취소가 확인된 주문에 "얼마나 찼나"를 묻는다. **이것이 노출을
		//      푸는 유일한 자리다.** 이 호출이 빠지면 confirming 이 계속 쌓여
		//      회차가 한 번 걸고 멈춘다 — 예전처럼 두 배로 거는 것보다는
		//      낫지만, 그것도 고장이다.
		r.resolveConfirming(ctx, st, r.now())

		// 5) 바깥에 우리 상태를 복사해 준다. 관측 전용이다 — 이 호출의 결과가
		//    회차 진행에 영향을 주는 경로는 없다.
		r.observe(st)

		if err := r.sleep(ctx, r.poll()); err != nil {
			return err
		}
	}
}

// place 는 신규 주문 한 건이다. 크기는 risk 가 정한다.
func (r *Runner) place(ctx context.Context, rd live.Round, f live.Frozen, tokenID string, e risk.Equity, st *roundState, d quote.Decision, now time.Time) error {
	remaining := risk.Remaining(e, st.exposure())
	tick := order.NewTick(d.Tick, rd.Precision)
	shares := risk.Shares(remaining, tick.Float())
	if shares <= 0 {
		r.logf("회차 %s: 잔여 %.4f USD 로는 %v 에 유효한 주문을 만들 수 없다 — 건너뛴다", rd.Slug, remaining, tick.Float())
		return nil
	}
	// 호가창에서 다시 뺄 수 없는 크기의 주문은 내지 않는다. 내고 나면 그
	// 순간부터 우리는 자기 호가를 군중으로 읽는다 — 조용히.
	if !representable(shares) {
		r.logf("회차 %s: %.0f주는 호가창 제외 수량으로 표현할 수 없다(상한 %.0f) — 주문하지 않는다", rd.Slug, shares, maxExcludableShares)
		return nil
	}
	req := Request{Round: rd, Outcome: f.Direction, TokenID: tokenID, Tick: tick, Shares: shares}
	r.logf("회차 %s: %s → %s %.4f주 @ %v (명목 %.4f, 잔여 %.4f)",
		rd.Slug, d.Why, f.Direction, shares, tick.Float(), req.Notional(), remaining)
	return r.transmit(ctx, st, req, now)
}

// transmit 은 이 패키지에서 [Orders.Create] 를 부르는 **유일한** 자리다.
// 주문이 나가는 부수효과를 한 곳에 모아 두면 실패 분류도 한 곳에서 끝난다.
func (r *Runner) transmit(ctx context.Context, st *roundState, req Request, now time.Time) error {
	res, err := r.Orders.Create(ctx, req)
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
			// 주문은 존재하지 않는다. 노출에 아무것도 더하지 않고, 다음
			// 바퀴에 같은 판단이 서면 다시 낸다.
			r.logf("주문 생성 실패(재시도 안전): %v", err)
			return nil
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
			return nil
		}
		st.unknownNotional += o.notional
		r.logf("주문 결과 불명이고 식별자도 없다 — 명목 %.4f 를 회차 끝까지 노출에 남긴다: %v", o.notional, err)
		return nil
	}

	st.live = &openOrder{
		id: res.ID, hash: res.Hash, tick: req.Tick.V, shares: req.Shares,
		notional: req.Notional(), placed: now,
		retryAt: retryAtFrom(res, now), lockUnknown: res.RemovalLockUnknown,
		filledBefore: st.filledSharesKnown(),
	}
	return nil
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
	if st.live == nil {
		return
	}
	o := st.live
	st.live = nil
	if o.retryAt.IsZero() {
		o.retryAt = now
	}
	st.pending = append(st.pending, o)
	r.logf("취소 요청 준비 (id=%s, 틱 %d): %s", o.id, o.tick, why)
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
	if st.live != nil {
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
