package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/exec"
	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// 이 파일은 [exec.Fills] 를 배선한다. 그리고 그것은 **매 바퀴**(기본 50ms,
// 회차당 6,000 바퀴) 불린다.
//
// # 세 가지 제약이 이 파일의 전부다
//
//  1. **DRY-RUN 에서 "체결 없음" 은 정확하다.** 주문을 전송하지 않으므로
//     체결될 주문이 존재하지 않는다. 추정도 근사도 아니고 사실이다.
//
//  2. **그 구현이 실거래 배선에 남으면 노출 상한이 무력화된다.** 체결이
//     0 으로 고정되면 exec 의 노출 불변식 첫 항이 영원히 0 이고, 봇은
//     체결이 쌓여도 잔여 한도가 줄지 않는다고 믿는다 — cap 을 몇 배로
//     넘겨 베팅한다. 그래서 **타입으로 막는다**(아래 armedFills).
//
//  3. **어떤 구현도 매 바퀴 REST 를 치면 안 된다.** 회차당 6,000 바퀴이고
//     레이트리밋은 키당 240 req/min 이다. noFills 는 I/O 를 전혀 하지 않고,
//     restFills 는 자체 주기(fillsPollInterval)로 스스로를 조인다.

// armedFills 는 **무장 상태에서 쓸 수 있는** 체결 조회다.
//
// exec.Fills 에 표식 메서드 하나를 더한 것이 전부다. 그 메서드가 하는 일은
// 없다 — 존재 자체가 계약이다. 무장 경로는 이 인터페이스만 받으므로,
// noFills 를 거기 넘기려는 코드는 **컴파일되지 않는다.** 런타임 가드는
// 잊히거나 조건이 뒤집힐 수 있지만 타입은 그렇지 않다.
//
// 구현체가 되려면 다음을 만족해야 한다(주석이 아니라 리뷰의 체크리스트다):
//   - 아직 돌려주지 않은 체결만 돌려준다(중복 제거는 구현의 책임 — exec 의
//     Fills 문서 참고).
//   - Poll 이 매 바퀴 네트워크를 치지 않는다.
//   - 조회하지 못한 구간을 "체결 없음" 으로 돌려주지 않는다. 모르면 에러다 —
//     exec 는 에러를 "이 주기에는 신규 주문을 내지 않는다" 로 다룬다.
type armedFills interface {
	pollingFills
	// safeWithRealOrders 는 표식이다. 이름이 곧 계약이라 주석보다 이름이 길다.
	safeWithRealOrders()
}

// pollingFills 는 [exec.Fills] 에 "마지막으로 조회한 시각" 을 더한 것이다.
// 하트비트 스냅샷의 `loop.fills_poll_at` 이 이 값이다.
//
// **타입에 넣는다.** 배선에서 `f.(interface{ LastPollAt() time.Time })` 로
// 꺼내면, 구현이 그 메서드를 잃는 날 컴파일은 통과하고 스냅샷 필드만
// 조용히 제로가 된다. 감시 장치의 필드가 조용히 비는 것 — 이 파일이 막으려는
// 고장이 정확히 그것이다.
type pollingFills interface {
	exec.Fills
	// LastPollAt 은 마지막으로 체결 조회를 시도한 시각이다(성공·실패 무관).
	// 한 번도 조회하지 않았으면 제로값이다.
	LastPollAt() time.Time
}

// noFills 는 **아무 주문도 전송되지 않았을 때만 옳은** 체결 조회다.
//
// DRY-RUN 전용이다. 이름에 "no" 가 들어간 이유는 이것이 "체결이 없다" 를
// 보고하는 구현이어서가 아니라, **체결이 존재할 수 없는 상황에서만** 옳기
// 때문이다. 무장 상태에서 이것을 쓰면 봇은 체결을 영원히 0 으로 세고 노출
// 상한이 사라진다.
//
// 일부러 safeWithRealOrders 를 갖지 않는다 — armedFills 를 만족하지 않으므로
// 무장 경로에 넘길 수 없다. fills_test.go 가 그 사실을 고정한다.
type noFills struct{}

// Poll 은 항상 빈 목록이다. 네트워크도 파일도 시계도 건드리지 않는다 —
// 매 바퀴 불리는 함수라 그래야 한다.
func (noFills) Poll(context.Context, live.Round) ([]ledger.Fill, error) { return nil, nil }

// LastPollAt 은 언제나 제로값이다 — DRY-RUN 은 조회를 하지 않으므로 "마지막
// 조회 시각" 이 없다. 지금 시각을 돌려주면 하트비트가 살아 있는 폴링으로
// 보인다.
func (noFills) LastPollAt() time.Time { return time.Time{} }

// ---------------------------------------------------------------------------
// 무장 경로 — GET /v1/orders/matches
// ---------------------------------------------------------------------------

// DefaultFillsPollInterval 은 restFills 가 REST 를 다시 치기까지 기다리는
// 최소 시간이다.
//
// # 이 값이 무엇을 사는가, 무엇을 파는가
//
// **판다: 노출 갱신 지연.** 체결이 일어나고 이 시간까지는 exec 가 그것을
// 모른다. 그동안 체결된 주문의 명목은 "미체결" 항에 남아 있으므로 노출
// 총합은 여전히 계산되지만, 우리가 그 주문을 취소하고 거래소가 "이미
// 끝났다(noop)" 고 답하는 순간 그 명목이 노출에서 빠진다 — 체결을 아직
// 흡수하지 않았다면 그 사이 노출이 실제보다 작게 보인다. 재호가 쿨다운이
// 500ms 이므로 2초는 그 창이 최대 네 바퀴라는 뜻이다.
//
// **산다: 요청 예산과 지연.** rest.Client 는 모든 요청을 333ms 간격으로
// 직렬화한다(레이트리밋 240 req/min 에 대한 여유). 즉 전체 트래픽은 구조적으로
// 180 req/min 을 넘을 수 없어서 **레이트리밋으로 죽는 일은 이 값과 무관하게
// 일어나지 않는다.** 이 값이 실제로 사는 것은 그 333ms 슬롯의 점유율이다 —
// 2초 주기면 슬롯 여섯 개 중 하나를 체결 조회가 쓰고, 나머지가 주문 생성·
// 취소에 남는다. 주기를 1초로 줄이면 점유율이 두 배가 되고 그만큼 주문이
// 늦게 나간다. 회차당 요청 수로는 300초/2초 = 150건 = 30 req/min 이다
// (예산의 12.5%).
//
// 500ms(쿨다운 한 바퀴)보다 짧게 두는 것은 의미가 없다. 그보다 자주 물어도
// 새 체결이 생길 수 있는 주문 자체가 그 주기로만 바뀐다.
const DefaultFillsPollInterval = 2 * time.Second

// MaxFillsPollInterval 은 설정으로 줄 수 있는 상한이다. 이 값을 크게 잡으면
// 노출 갱신이 그만큼 늦어지므로 "체결을 세지 않는 것"에 점점 가까워진다.
// 회차 길이(300초)의 10분의 1 을 상한으로 둔다.
const MaxFillsPollInterval = 30 * time.Second

// errFillsNotWired 는 무장 경로의 체결 조회를 만들 수 없다는 뜻이다.
//
// **이것이 있으면 무장하지 않는다.** 체결을 못 세면 노출 상한을 지킬 근거가
// 없다. 배선 실수(클라이언트 nil, 계정 주소 없음, 주기가 범위 밖)를 조용히
// 기본값으로 메우지 않는다 — 그 메움이 곧 "0 으로 세는 구현"이다.
var errFillsNotWired = errors.New("무장 상태에서 쓸 체결 조회를 만들 수 없다")

// restFills 는 `GET /v1/orders/matches` 로 우리 체결을 센다.
//
// # 왜 matches 인가 (GET /v1/orders/{hash} 가 아니라)
//
// 둘 다 스펙에 있다. matches 를 쓰는 이유는 셋이다.
//
//  1. **요청 수가 주문 수에 비례하지 않는다.** `/v1/orders/{hash}` 는 주문
//     하나당 요청 하나다. 이 봇은 회차당 수백 번 재호가하므로 그 방식은
//     폴링 한 번이 수백 요청이 된다 — 333ms 스로틀에서 한 번의 폴링이
//     회차보다 오래 걸린다. matches 는 마켓 하나로 좁혀 한 요청이면 된다.
//  2. **체결 단위의 사실을 준다.** OrderData 는 `amountFilled` 누적과
//     status 만 주므로 "얼마에, 언제, 수수료 얼마로" 체결됐는지가 없다.
//     원장은 그 셋을 요구한다.
//  3. **JWT 가 필요 없다.** 스펙의 security 가 ApiKeyAuth 뿐이다.
//
// `/v1/orders/{hash}` 를 아예 버린다는 뜻은 아니다 — 주문 생성이
// `OrderUnknown`(보냈는데 결과 불명)으로 끝났을 때 그 해시 하나를 대조하는
// 용도로는 그쪽이 맞다. 그건 이 파일이 아니라 생성 경로의 일이다.
//
// # 우리 체결을 어떻게 고르는가
//
// 스펙 원문: *"A match event describes a whole transaction, so filtering by a
// maker order also includes the other fills settled in the same transaction."*
//
// **최상위 `amountFilled`/`priceExecuted` 는 테이커 기준 트랜잭션 전체
// 수치다.** 그것을 우리 체결로 세면 노출을 크게 과대계상한다 — 2026-08-10
// 실측에서 메이커가 5명 붙은 트랜잭션을 봤다. rest.Match 는 그 두 필드를
// 아예 파싱하지 않으므로 여기서 실수로 쓸 자리가 없다.
//
// 우리 체결은 `taker`/`makers[]` 중 **signer 가 우리 계정인 원소**뿐이다.
// taker 쪽도 보는 이유: 이 봇은 메이커로만 걸지만 지정가가 최우선 매도호가에
// 닿으면 그 주문은 테이커로 체결된다. 한쪽만 보면 그 체결이 통째로 사라지고,
// 사라진 체결은 노출에 잡히지 않는다.
type restFills struct {
	rest *rest.Client
	// account 는 우리 스마트계정 주소다. 주문의 maker 이자 signer 이므로
	// 응답의 signer 와 이 값을 대조해 우리 체결을 고른다.
	//
	// **소문자로 정규화해 들고 다닌다.** 응답이 체크섬 대소문자로 오는데
	// 그대로 비교하면 우리 체결이 남의 체결로 보인다 — 그 순간 노출이
	// 0 으로 고정되고, 이 파일이 막으려던 바로 그 상태가 된다.
	account  string
	interval time.Duration
	now      func() time.Time
	log      func(format string, args ...any)

	mu sync.Mutex
	// roundKey 는 지금 세고 있는 회차다. 바뀌면 아래 상태를 전부 버린다 —
	// seen 이 회차를 넘어 자라면 24시간 운전에서 계속 커진다.
	roundKey int64
	// lastPoll 은 마지막으로 REST 를 친 시각이다(성공·실패 무관).
	lastPoll time.Time
	// polled 는 이 회차에서 한 번이라도 성공적으로 조회했는지다.
	polled bool
	// seen 은 이미 돌려준 체결의 멱등 키다.
	seen map[string]bool
}

// LastPollAt 은 마지막으로 REST 를 친 시각이다(성공·실패 무관).
//
// **실패도 포함하는 것이 요점이다.** 성공만 센다면, 조회가 계속 실패하는
// 동안에도 이 값이 멈춰 있어 감시 장치는 "폴링이 멎었다" 로 읽는다. 그런데
// 폴링은 멎지 않았다 — 응답이 오지 않을 뿐이다. 둘은 다른 고장이고 대응도
// 다르다(전자는 봇 재시작, 후자는 거래소 확인).
func (f *restFills) LastPollAt() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPoll
}

// newRestFills 는 무장 경로의 체결 조회를 만든다. 배선이 모자라면 에러다.
func newRestFills(rc *rest.Client, account string, interval time.Duration,
	log func(string, ...any)) (*restFills, error) {

	if rc == nil {
		return nil, fmt.Errorf("%w: rest.Client 가 nil 이다", errFillsNotWired)
	}
	acct := strings.ToLower(strings.TrimSpace(account))
	// 40자리 hex + "0x" = 42자. 주소가 아니면 응답의 signer 와 절대 같아지지
	// 않고, 그러면 체결이 영원히 0 건이다 — noFills 와 구별되지 않는 상태다.
	if len(acct) != 42 || !strings.HasPrefix(acct, "0x") {
		return nil, fmt.Errorf("%w: 계정 주소가 0x + 40자리 hex 가 아니다 (%d자)", errFillsNotWired, len(acct))
	}
	if interval <= 0 || interval > MaxFillsPollInterval {
		return nil, fmt.Errorf("%w: 체결 조회 주기가 %s 다 (0 초과 %s 이하여야 한다)",
			errFillsNotWired, interval, MaxFillsPollInterval)
	}
	return &restFills{rest: rc, account: acct, interval: interval, now: time.Now, log: log}, nil
}

func (f *restFills) safeWithRealOrders() {}

func (f *restFills) logf(format string, args ...any) {
	if f.log != nil {
		f.log(format, args...)
	}
}

func (f *restFills) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// Poll 은 아직 돌려주지 않은 우리 체결을 돌려준다.
//
// # 매 바퀴 REST 를 치지 않는다
//
// 마지막 조회로부터 interval 이 지나지 않았으면 **빈 목록**을 돌려준다.
// 이것은 "체결이 없다"는 주장이 아니라 "지난번에 말한 것 말고 새로 말할 것이
// 없다"는 뜻이다 — 회차마다 처음 불릴 때는 반드시 실제로 조회하고, 그 뒤로도
// interval 마다 실제로 조회한다. 그 사이의 지연이 DefaultFillsPollInterval 이
// 파는 값이고, 그 문서에 크기와 근거가 있다.
//
// # 조회에 실패하면 에러다
//
// 실패를 "체결 없음"으로 뭉개지 않는다. exec 는 에러를 "이 주기에는 신규
// 주문을 내지 않는다"로 다룬다(TestFillsPollErrorBlocksNewOrdersButNotTheRound).
// 체결을 못 세는 동안 새로 거는 것이야말로 한도를 넘기는 경로다.
func (f *restFills) Poll(ctx context.Context, rd live.Round) ([]ledger.Fill, error) {
	f.mu.Lock()
	if f.roundKey != rd.MarketID {
		// 새 회차다. 이전 회차의 멱등 키는 필요 없다 — 매치는 마켓으로
		// 좁혀 받으므로 회차를 넘어 같은 키가 올 수 없다.
		f.roundKey = rd.MarketID
		f.seen = map[string]bool{}
		f.polled = false
		// **lastPoll 도 지운다.** 안 지우면 새 회차의 첫 폴링이 이전 회차의
		// 주기에 묶인다. 그 순간 polled 는 방금 false 로 돌아갔으므로 아래
		// 게이트는 "모른다"로 에러를 내고, exec 는 그것을 "신규 주문 금지"로
		// 다룬다 — 회차가 바뀔 때마다 최대 interval 만큼 진입이 막힌다.
		//
		// 레이트리밋 대가는 회차당 조회 1건이다(5분에 1건). 240 req/min
		// 예산에서 무시할 수 있는 양이고, 회차 초반은 이 봇이 주문을 거는
		// 유일한 구간이라 그 구간을 막는 쪽이 훨씬 비싸다.
		f.lastPoll = time.Time{}
	}
	now := f.clock()
	if !f.lastPoll.IsZero() && now.Sub(f.lastPoll) < f.interval {
		polled := f.polled
		f.mu.Unlock()
		if !polled {
			// **아직 한 번도 성공하지 못했다.** 이 상태에서 빈 목록을
			// 돌려주면 그것은 "새로 말할 것이 없다"가 아니라 "모른다"이고,
			// 모르는 것을 체결 없음으로 돌려주는 순간 이 파일이 막으려던
			// 상태(노출 상한 없음)가 된다.
			return nil, fmt.Errorf("체결 조회: 회차 %s 에서 아직 한 번도 성공하지 못했다 — 모르는 구간을 '체결 없음' 으로 돌려주지 않는다", rd.Slug)
		}
		return nil, nil
	}
	// **성공 여부와 무관하게 여기서 시각을 찍는다.** 성공했을 때만 찍으면
	// 조회가 계속 실패하는 동안 게이트가 열린 채로 남아 매 바퀴(50ms) REST 를
	// 치게 된다 — 이 필드가 막으려던 바로 그 일이다.
	f.lastPoll = now
	f.mu.Unlock()

	if rd.MarketID <= 0 {
		return nil, fmt.Errorf("회차의 marketId 가 %d 다 — 어느 마켓의 체결을 물어야 할지 모른다", rd.MarketID)
	}

	// **마켓으로 좁히고 signer 로 좁힌다.**
	//
	// 시각 창(executedAfter)을 쓰지 않는 이유: 마켓 하나가 곧 회차 하나이므로
	// 마켓 필터만으로 이미 이 회차로 좁혀진다. 거기에 시각 창을 얹으면
	// executedAt 이 초 단위인데 필터는 밀리초 배타적이라, 같은 초에 뒤늦게
	// 정산된 체결이 조용히 빠질 수 있다. 매번 회차 전체를 다시 받고 멱등 키로
	// 거르는 편이 그 실패 모드가 없다 — 우리 signer 로 좁힌 매치는 회차당
	// 한 자리 수라 한 페이지에 들어온다.
	matches, err := f.rest.Matches(ctx, rest.MatchQuery{
		MarketID:      rd.MarketID,
		SignerAddress: f.account,
	})
	if err != nil {
		return nil, err
	}

	out, err := f.collect(rd, matches)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.polled = true
	f.mu.Unlock()
	return out, nil
}

// collect 는 매치 목록에서 **우리 체결만** 골라 원장 레코드로 바꾼다.
func (f *restFills) collect(rd live.Round, matches []rest.Match) ([]ledger.Fill, error) {
	var out []ledger.Fill
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range matches {
		if m.MarketID != rd.MarketID {
			// 마켓으로 필터해 받았는데 다른 마켓이 왔다. 필터가 무시된
			// 것이므로 이 응답 전체를 믿을 수 없다 — 다른 회차의 체결을
			// 이 회차 노출로 세면 한도 계산이 통째로 틀린다.
			return nil, fmt.Errorf("체결 조회: marketId=%d 를 물었는데 %d 가 왔다 — 필터가 무시된 것으로 보인다",
				rd.MarketID, m.MarketID)
		}
		// 같은 트랜잭션 안에서 같은 주문 해시가 두 번 나올 수 있다. 그때
		// 키가 같으면 둘째 건이 중복으로 보여 조용히 사라진다 — 노출을
		// 과소계상하는 방향이다. 그래서 매치 안에서의 등장 순번을 키에 넣는다.
		ord := map[string]int{}
		// **두 자리를 다 본다.** 이 봇은 메이커로 걸지만 지정가가 최우선
		// 매도호가에 닿으면 그 주문은 테이커로 체결된다. 한쪽만 보면 그
		// 체결이 통째로 사라지고, 사라진 체결은 어떤 한도에도 잡히지 않는다.
		if err := f.take(rd, m, "t", m.Taker, ord, &out); err != nil {
			return nil, err
		}
		for _, mk := range m.Makers {
			if err := f.take(rd, m, "m", mk, ord, &out); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// take 는 체결 하나가 우리 것이면 원장 레코드로 만들어 out 에 붙인다.
// 호출자가 f.mu 를 쥐고 있어야 한다.
func (f *restFills) take(rd live.Round, m rest.Match, role string, fill rest.MatchFill,
	ord map[string]int, out *[]ledger.Fill) error {

	if !strings.EqualFold(fill.Signer, f.account) {
		return nil // 같은 트랜잭션에 섞여 온 남의 체결이다
	}

	// 멱등 키. 같은 매치가 다음 폴링에서 다시 와도 두 번 세지 않는다.
	// settlementId 는 스펙상 optional 이라 빈 문자열일 수 있으므로
	// transactionHash 와 함께 쓴다 — 둘 중 하나만으로는 부족하다.
	base := m.SettlementID + "|" + m.TransactionHash + "|" + role + "|" + fill.Hash
	n := ord[base]
	ord[base] = n + 1
	key := base + "#" + fmt.Sprint(n)
	if f.seen[key] {
		return nil
	}

	// **매도는 이 봇이 낼 수 없는 주문이다.** 우리 서명으로 Ask 가 체결됐다면
	// 우리가 이해하지 못하는 일이 일어난 것이고, 그것을 매수로 원장에 적으면
	// 포지션 부호가 뒤집힌 채 크래시 복구가 돈다. 애매하면 거래하지 않는다.
	if !strings.EqualFold(fill.QuoteType, rest.QuoteTypeBid) {
		return fmt.Errorf("체결 조회: 우리 서명의 체결인데 quoteType 이 %q 다 — 이 봇은 매도 주문을 내지 않는다 (tx %s)",
			fill.QuoteType, shortHash(m.TransactionHash))
	}

	// **방향은 이름이 아니라 토큰 ID 로 정한다.** 이름 표기가 바뀌면 이름
	// 매칭은 조용히 어느 쪽도 아니게 되지만, 토큰 ID 는 이 회차 메타데이터가
	// 준 값과 같아야 한다.
	var outcome string
	switch fill.OutcomeOnChainID {
	case rd.UpTokenID:
		outcome = ledger.OutcomeUp
	case rd.DownTokenID:
		outcome = ledger.OutcomeDown
	default:
		return fmt.Errorf("체결 조회: outcome 토큰 ID 가 이 회차의 Up/Down 어느 쪽도 아니다 (이름 %q, tx %s)",
			fill.OutcomeName, shortHash(m.TransactionHash))
	}

	feeUSD, err := feeToUSD(fill)
	if err != nil {
		return fmt.Errorf("체결 조회: %w (tx %s)", err, shortHash(m.TransactionHash))
	}

	f.seen[key] = true
	*out = append(*out, ledger.Fill{
		RoundStart: rd.StartsAt.Unix(),
		MarketID:   rd.MarketID,
		Outcome:    outcome,
		Shares:     fill.Shares,
		PriceUSD:   fill.PriceUSD,
		FeeUSD:     feeUSD,
		At:         m.ExecutedAt,
	})
	f.logf("체결: %s %.6f주 @ %.4f (수수료 %.6f USD, tx %s)",
		outcome, fill.Shares, fill.PriceUSD, feeUSD, shortHash(m.TransactionHash))
	return nil
}

// shortHash 는 로그·에러에 실을 트랜잭션 해시의 앞부분이다. 전체를 실으면
// 한 줄이 길어지고, 앞 10자면 사람이 익스플로러에서 찾기에 충분하다.
// **주소가 아니다** — 트랜잭션 해시는 이미 공개 체인 데이터이고, 이 봇이
// 로그에 남기지 않기로 한 것은 지갑·계정 주소다.
func shortHash(h string) string {
	if len(h) <= 10 {
		return h
	}
	return h[:10] + "…"
}

// feeToUSD 는 수수료를 USD 로 옮긴다.
//
// 스펙의 FeeType 은 둘이다. COLLATERAL 은 USDT 라 그대로 USD 이고,
// SHARES 는 주식으로 걷힌 것이라 체결가를 곱해 값을 매긴다(2026-08-10 실측:
// 메이커 쪽 수수료가 `{"amount":"0","type":"SHARES"}` 로 온다).
//
// **모르는 type 은 에러다.** 0 으로 두면 비용이 사라지고, 그것은 적자를
// 흑자로 보이게 하는 방향이다 — pmmm-go 에서 테이커 수수료 부호를 놓쳐
// +40 이 −90 이 된 사고와 같은 방향이다.
func feeToUSD(fill rest.MatchFill) (float64, error) {
	switch {
	case strings.EqualFold(fill.FeeType, rest.FeeTypeCollateral):
		return fill.FeeAmount, nil
	case strings.EqualFold(fill.FeeType, rest.FeeTypeShares):
		return fill.FeeAmount * fill.PriceUSD, nil
	}
	return 0, fmt.Errorf("모르는 수수료 type %q — 0 으로 세면 비용이 사라진다", fill.FeeType)
}

// fillsDeps 는 무장 경로 체결 조회의 배선이다.
type fillsDeps struct {
	Rest     *rest.Client
	Account  string
	Interval time.Duration
	Log      func(format string, args ...any)
}

// chooseFills 는 무장 여부에 맞는 구현을 고른다.
//
// 무장이 아니면 noFills 를 exec.Fills 로 돌려주고, 무장이면 armedFills 를
// 요구한다. 두 경로가 한 함수 안에 있어야 "무장인데 noFills" 라는 조합이
// 이 파일 밖으로 나갈 수 없다. **반환 타입이 armedFills 인 newArmedFills 를
// 거치는 것이 요점이다** — noFills 를 거기서 돌려주려면 컴파일이 깨진다.
func chooseFills(armed bool, d fillsDeps) (pollingFills, error) {
	if !armed {
		return noFills{}, nil
	}
	return newArmedFills(d)
}

// newArmedFills 의 반환 타입이 armedFills 인 것이 타입 가드다.
func newArmedFills(d fillsDeps) (armedFills, error) {
	f, err := newRestFills(d.Rest, d.Account, d.Interval, d.Log)
	if err != nil {
		return nil, err
	}
	return f, nil
}
