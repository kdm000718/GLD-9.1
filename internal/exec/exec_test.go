package exec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// ---------------------------------------------------------------------------
// 하네스 — 테스트가 시계와 호가창을 쥔다
// ---------------------------------------------------------------------------

// fakeClock 은 테스트가 쥔 벽시계 + 단조시계다. Sleep 이 둘을 함께 민다.
type fakeClock struct {
	mu   sync.Mutex
	t    time.Time
	mono int64
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) monoNs() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	c.mono += d.Nanoseconds()
}

// advanceMono 는 **단조시계만** 민다. 호가창을 낡게 만들되 회차는 끝내지
// 않는 유일한 방법이다 — 벽시계를 밀면 EndsAt 을 지나 루프가 곧바로 끝난다.
func (c *fakeClock) advanceMono(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mono += d.Nanoseconds()
}

// fakeOrders 는 주문 전송을 가로챈다. 실제 predict.fun 을 절대 부르지 않는다.
//
// step 은 "몇 번째 폴링 주기에 이 요청이 나갔는가"를 기록한다. 쿨다운·잠금
// 창처럼 **언제 요청이 나갔는가**가 판정인 테스트가 있어서, 요청 개수만으로는
// 부족하다.
type fakeOrders struct {
	mu          sync.Mutex
	step        func() int
	creates     []Request
	createSteps []int
	removes     [][]string
	removeSteps []int
	createFn    func(n int, r Request) (CreateResult, error)
	removeFn    func(n int, ids []string) (RemoveResult, error)
	// filledFn 은 단건 조회의 답이다. nil 이면 "한 주도 안 찼다" — 대부분의
	// 시험이 체결 없이 재호가만 본다.
	filledFn func(hash string) (float64, error)
	// filledAsks 는 단건 조회가 불린 해시들이다. **누가 물어봤는지**를 봐야
	// 확인 경로가 배선에 실제로 걸려 있는지 알 수 있다.
	filledAsks []string
	// book 은 지금 거래소 호가창에 살아 있는 우리 주문이다(ID → 주문).
	book map[string]Request
}

func (f *fakeOrders) at() int {
	if f.step == nil {
		return -1
	}
	return f.step()
}

func (f *fakeOrders) Create(_ context.Context, r Request) (CreateResult, error) {
	f.mu.Lock()
	n := len(f.creates)
	f.creates = append(f.creates, r)
	f.createSteps = append(f.createSteps, f.at())
	fn := f.createFn
	f.mu.Unlock()

	id := fmt.Sprintf("ord-%d", n)
	// **해시를 반드시 채운다.** 이것이 없으면 취소가 확인된 주문의 체결
	// 여부를 물어볼 수 없어 명목이 회차 끝까지 잠긴다 — 거래소는 언제나
	// 해시를 준다(rest.CreateOrderResult.OrderHash).
	res := CreateResult{ID: id, Hash: "hash-" + id}
	var err error
	if fn != nil {
		res, err = fn(n, r)
	}
	if err == nil && res.ID != "" {
		// 성사된 주문은 거래소 호가창에 나타난다. 하네스가 그것을 흉내낸다 —
		// 안 그러면 "우리 주문을 제외한다"를 시험할 대상 자체가 없다.
		f.mu.Lock()
		if f.book == nil {
			f.book = map[string]Request{}
		}
		f.book[res.ID] = r
		f.mu.Unlock()
	}
	return res, err
}

func (f *fakeOrders) Remove(_ context.Context, ids []string) (RemoveResult, error) {
	f.mu.Lock()
	n := len(f.removes)
	cp := append([]string(nil), ids...)
	f.removes = append(f.removes, cp)
	f.removeSteps = append(f.removeSteps, f.at())
	fn := f.removeFn
	f.mu.Unlock()

	res := RemoveResult{Removed: cp}
	var err error
	if fn != nil {
		res, err = fn(n, cp)
	}
	if err == nil {
		// 거래소가 사라졌다고 말한 것만 호가창에서 뺀다. Rejected·Unaccounted 는
		// 아직 책에 남아 있다 — 그것이 이 세 바구니를 나눈 이유다.
		f.mu.Lock()
		for _, group := range [][]string{res.Removed, res.Noop} {
			for _, id := range group {
				delete(f.book, id)
			}
		}
		f.mu.Unlock()
	}
	return res, err
}

// Filled 는 그 주문이 몇 주 찼는지다. 기본은 0 — 시험이 따로 말하지 않으면
// 아무것도 차지 않았다.
func (f *fakeOrders) Filled(_ context.Context, hash string) (float64, error) {
	f.mu.Lock()
	f.filledAsks = append(f.filledAsks, hash)
	fn := f.filledFn
	f.mu.Unlock()
	if fn == nil {
		return 0, nil
	}
	return fn(hash)
}

// askedFor 는 이 해시에 단건 조회가 갔는지다.
func (f *fakeOrders) askedFor(hash string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, h := range f.filledAsks {
		if h == hash {
			return true
		}
	}
	return false
}

// bookLevels 는 지금 거래소 호가창에 있는 우리 물량이다(틱 → 주식 수).
func (f *fakeOrders) bookLevels() map[int64]float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int64]float64{}
	for _, r := range f.book {
		out[r.Tick.V] += r.Shares
	}
	return out
}

func (f *fakeOrders) createdCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates)
}

func (f *fakeOrders) createdTicks() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int64, len(f.creates))
	for i, r := range f.creates {
		out[i] = r.Tick.V
	}
	return out
}

// createsAtTick 은 그 틱으로 나간 주문 생성 시도 횟수다.
//
// 다리가 둘이 되면서 "주문 생성 몇 회" 만으로는 재시도 규약을 말할 수 없게
// 됐다 — 한 다리의 재시도와 다른 다리의 첫 시도가 같은 숫자에 섞인다.
// 틱이 다리를 가른다(테이커는 매도호가, 메이커는 [LimitPrice]).
// firstCreateStep 은 첫 주문 생성이 일어난 바퀴다. 없었으면 매우 큰 값.
func (f *fakeOrders) firstCreateStep() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.createSteps) == 0 {
		return 1 << 30
	}
	return f.createSteps[0]
}

func (f *fakeOrders) createsAtTick(v int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.creates {
		if r.Tick.V == v {
			n++
		}
	}
	return n
}

// stepsAtTick 은 그 틱의 주문이 나간 스텝 번호들이다. 재시도 간격을 잰다.
func (f *fakeOrders) stepsAtTick(v int64) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for i, r := range f.creates {
		if r.Tick.V == v {
			out = append(out, f.createSteps[i])
		}
	}
	return out
}

// createdNotionals 는 나간 주문의 명목이다. 다리마다 예산이 따로라 크기를
// 보는 시험이 필요하다.
func (f *fakeOrders) createdNotionals() []float64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]float64, len(f.creates))
	for i, r := range f.creates {
		out[i] = r.Notional()
	}
	return out
}

func (f *fakeOrders) removeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.removes)
}

// fakeFills 는 체결 조회다. 기본은 "체결 없음".
type fakeFills struct {
	mu     sync.Mutex
	pollFn func(n int) ([]ledger.Fill, error)
	polls  int
	// step 은 지금 몇 번째 루프 바퀴인지 준다. 체결 조회와 주문 생성의
	// **순서**를 재는 데 쓴다 — 첫 바퀴 조회를 건너뛰는 최적화의 계약이
	// "주문이 조회보다 먼저" 이기 때문이다.
	step      func() int
	pollSteps []int
}

func (f *fakeFills) pollCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.polls
}

// firstPollStep 은 첫 체결 조회가 일어난 바퀴다. 한 번도 없었으면 매우 큰 값 —
// "주문보다 뒤" 로 비교되게 한다.
func (f *fakeFills) firstPollStep() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pollSteps) == 0 {
		return 1 << 30
	}
	return f.pollSteps[0]
}

func (f *fakeFills) Poll(_ context.Context, _ live.Round) ([]ledger.Fill, error) {
	f.mu.Lock()
	n := f.polls
	f.polls++
	if f.step != nil {
		f.pollSteps = append(f.pollSteps, f.step())
	}
	fn := f.pollFn
	f.mu.Unlock()
	if fn != nil {
		return fn(n)
	}
	return nil, nil
}

// recordingLedger 는 원장 기록을 가로챈다. 실제 파일에 쓰지 않는다.
type recordingLedger struct {
	mu     sync.Mutex
	fills  []ledger.Fill
	fillFn func(n int, f ledger.Fill) error
}

func (l *recordingLedger) RecordFill(f ledger.Fill) error {
	l.mu.Lock()
	n := len(l.fills)
	l.fills = append(l.fills, f)
	fn := l.fillFn
	l.mu.Unlock()
	if fn != nil {
		return fn(n, f)
	}
	return nil
}

func (l *recordingLedger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.fills)
}

// harness 는 한 회차를 도는 데 필요한 최소 배선이다.
type harness struct {
	t      *testing.T
	book   *ws.Book
	orders *fakeOrders
	fills  *fakeFills
	led    *recordingLedger
	clk    *fakeClock
	runner *Runner
	round  live.Round
	frozen live.Frozen
	equity risk.Equity

	// onStep 은 매 폴링 주기마다 불린다. 테스트가 **군중** 호가를 바꿀 자리다.
	onStep func(step int)
	steps  int
	ts     int64 // 호가창 스냅샷의 updateTimestampMs (단조 증가해야 적용된다)

	// 군중 호가. 실제 호가창은 여기에 우리 살아 있는 주문을 얹은 것이다.
	crowdBids map[float64]float64
	crowdAsks map[float64]float64
	// lastRaw 는 마지막으로 적용한 스냅샷이다. 내용이 그대로면 다시 적용하지
	// 않는다 — 그러지 않으면 아무것도 안 변한 호가창이 영원히 신선해 보여서
	// stale 판정을 시험할 수 없다.
	lastRaw string
	// autoBook 이 false 면 하네스가 호가창을 건드리지 않는다(동시성 테스트용).
	autoBook bool
}

const testPrecision = 2

func newHarness(t *testing.T) *harness {
	t.Helper()
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	h := &harness{
		t:      t,
		book:   ws.NewBook(testPrecision),
		orders: &fakeOrders{},
		fills:  &fakeFills{},
		led:    &recordingLedger{},
		clk:    &fakeClock{t: start, mono: 1_000_000_000},
		round: live.Round{
			CategoryID:  1,
			MarketID:    2,
			Slug:        "btc-updown-5m-1786275000",
			StartsAt:    start,
			EndsAt:      start.Add(1 * time.Second), // 짧은 합성 회차: 10 스텝
			Precision:   testPrecision,
			UpTokenID:   "111",
			DownTokenID: "222",
		},
		// equity 100 → cap 4.55. 지정가 0.47 이면 9주(명목 4.23)가 나간다.
		equity: risk.Equity{AvailableUSDT: 100},
	}
	// **문턱을 리터럴로 박지 않는다.** 문턱이 바뀌면 이 하네스가 통째로
	// "Eligible 인데 문턱 미달" 로 죽는다(2026-08-15 에 실제로 그랬다).
	// 문턱의 두 배로 두면 어떤 값이든 넉넉히 통과한다.
	const testConf = 2 * live.ConfidenceThreshold
	h.frozen = live.Frozen{
		T:          h.round.StartMS(),
		PUp:        0.5 + testConf/2,
		Confidence: testConf,
		Direction:  ledger.OutcomeUp,
		Eligible:   true,
	}
	h.orders.step = func() int { return h.steps }
	h.fills.step = func() int { return h.steps }
	h.runner = &Runner{
		Book:       h.book,
		Orders:     h.orders,
		Fills:      h.fills,
		Ledger:     h.led,
		StaleAfter: 3 * time.Second,
		// 합성 회차가 1초이므로 창이 회차 전체를 덮는다 — 진입 창을 **보는**
		// 시험은 아래에서 창을 직접 좁힌다.
		EntryWindow: time.Second,
		Poll:        100 * time.Millisecond,
		Clock:       h.clk.now,
		MonoClock:   h.clk.monoNs,
		Sleep: func(_ context.Context, d time.Duration) error {
			h.clk.advance(d)
			h.steps++
			if h.onStep != nil {
				h.onStep(h.steps)
			}
			h.refreshBook()
			return nil
		},
	}
	h.autoBook = true
	// 최초 군중 호가 — 매수 0.45, 매도 0.60
	h.setCrowd(map[float64]float64{0.45: 100}, map[float64]float64{0.60: 100})
	return h
}

// setCrowd 는 **군중** 호가를 바꾼다. 실제 호가창은 여기에 우리 살아 있는
// 주문을 얹은 것이다.
//
// 봇이 군중을 따라가지 않게 된 뒤로도 이것이 필요한 이유는 두 가지다:
// 주문 로그의 시장 기록(marketNote)이 방향에 맞는 값을 찍는지 봐야 하고,
// **군중이 어떻게 움직이든 주문이 움직이지 않는다**는 것을 보이려면 움직이는
// 군중이 있어야 한다.
func (h *harness) setCrowd(bids, asks map[float64]float64) {
	h.crowdBids, h.crowdAsks = bids, asks
	h.refreshBook()
}

// refreshBook 은 군중 + 우리 주문으로 전체 스냅샷을 만들어 적용한다.
// 내용이 직전과 같으면 아무것도 하지 않는다 — 멈춘 호가창은 멈춘 채로 둬야
// stale 판정을 시험할 수 있다.
func (h *harness) refreshBook() {
	if !h.autoBook {
		return
	}
	bids := map[float64]float64{}
	for p, q := range h.crowdBids {
		bids[p] += q
	}
	asks := map[float64]float64{}
	for p, q := range h.crowdAsks {
		asks[p] += q
	}
	factor := math.Pow(10, float64(h.round.Precision))
	full := order.Full(h.round.Precision)
	for tick, sh := range h.orders.bookLevels() {
		// **우리 주문이 책의 어느 쪽에 서는가는 방향이 정한다.** 책은 Up
		// 기준 한 벌이므로 Down 매수 t 는 `1.00 − t` 자리의 매도호가로
		// 나타난다(book.go). 하네스가 이것을 흉내내지 않으면 Down 회차의
		// 제외 시험이 통째로 헛돈다 — 우리 물량이 애초에 그쪽에 없으니까.
		if h.frozen.Direction == ledger.OutcomeDown {
			asks[float64(full-tick)/factor] += sh
			continue
		}
		bids[float64(tick)/factor] += sh
	}
	raw := fmt.Sprintf(`{"bids":[%s],"asks":[%s],"updateTimestampMs":`,
		levelsJSON(bids), levelsJSON(asks))
	if raw == h.lastRaw {
		return
	}
	h.lastRaw = raw
	h.ts++
	f := ws.Frame{RecvMonoNs: h.clk.monoNs()}
	f.Msg.Data = []byte(raw + fmt.Sprint(h.ts) + "}")
	applied, err := h.book.Apply(f)
	if err != nil {
		h.t.Fatalf("호가창 적용 실패: %v", err)
	}
	if !applied {
		h.t.Fatalf("호가창이 적용되지 않았다 (ts=%d)", h.ts)
	}
}

// levelsJSON 은 가격 오름차순으로 찍는다. 맵 순회 순서가 섞이면 "내용이 같다"
// 판정이 매번 뒤집힌다.
func levelsJSON(m map[float64]float64) string {
	ps := make([]float64, 0, len(m))
	for p := range m {
		ps = append(ps, p)
	}
	sort.Float64s(ps)
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, fmt.Sprintf("[%v,%v]", p, m[p]))
	}
	return strings.Join(out, ",")
}

func (h *harness) run() error {
	return h.runner.RunRound(context.Background(), h.round, h.frozen, h.equity)
}

// ---------------------------------------------------------------------------
// (1) 회차마다 한 번, 한 가격
// ---------------------------------------------------------------------------

// 이 봇의 전부다: 회차 시작에 **지정가 매수 한 건**을 걸고, 끝날 때까지
// 그대로 둔다.
//
// 2026-08-14 에 테이커 다리가 붙었다가 같은 날 제거됐다. 이 시험이 깨지는 날은
// 회차당 주문 수가 바뀐 날이고, 그것은 전략 변경이다.
func TestRoundPlacesExactlyOneOrder(t *testing.T) {
	h := newHarness(t) // 매도호가 0.60
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	ticks := h.orders.createdTicks()
	if len(ticks) != 1 {
		t.Fatalf("주문 %d건 (틱 %v) — 회차당 한 건이어야 한다", len(ticks), ticks)
	}
	if ticks[0] != limitPriceNum {
		t.Errorf("주문 틱 %d, 기대 %d (=%v)", ticks[0], limitPriceNum, LimitPrice)
	}
}

// **테이커 다리가 없다.** 매도호가가 무엇이든 그 값에 거는 주문이 나가면
// 안 된다 — 2026-08-14 에 제거한 다리가 되살아난 것이다.
func TestNoOrderFollowsTheAsk(t *testing.T) {
	for _, ask := range []float64{0.49, 0.53, 0.60, 0.64, 0.95} {
		h := newHarness(t)
		h.setCrowd(map[float64]float64{0.30: 100}, map[float64]float64{ask: 100})
		if err := h.run(); err != nil {
			t.Fatalf("매도호가 %v: %v", ask, err)
		}
		ticks := h.orders.createdTicks()
		if len(ticks) != 1 || ticks[0] != limitPriceNum {
			t.Errorf("매도호가 %v: 주문 틱 %v, 기대 [%d] — 호가를 따라간 주문이 있다",
				ask, ticks, limitPriceNum)
		}
	}
}

// **명목이 회차 상한 미만이어야 한다.** 사용자 제약은 "미만" 이지 "이하" 가
// 아니다. 다리가 하나로 돌아오면서 그 하나가 cap 전액을 쓴다(2026-08-14
// 사용자 결정) — 절반씩 쓰던 시절의 여유가 사라졌으므로 경계가 더 빡빡하다.
func TestOrderStaysUnderTheCap(t *testing.T) {
	h := newHarness(t) // equity 100 → cap 4.55
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	ns := h.orders.createdNotionals()
	if len(ns) != 1 {
		t.Fatalf("주문 %d건, 기대 1건", len(ns))
	}
	cap := risk.Cap(h.equity)
	if ns[0] >= cap {
		t.Errorf("명목 %.4f 가 cap %.4f 이상이다 — 사용자 제약은 미만이다", ns[0], cap)
	}
	// **전액을 쓰는지도 본다.** legFraction 이 조용히 0.5 로 돌아가면 위
	// 검사는 통과하고 노출만 절반이 된다.
	if ns[0] < cap*0.9 {
		t.Errorf("명목 %.4f 가 cap %.4f 의 90%% 에도 못 미친다 — 몫이 줄었나(legFraction=%v)",
			ns[0], cap, legFraction)
	}
}

// legFraction 은 사용자가 정한 값이다. 다른 시험들은 전부 이 상수를 참조하므로
// 값이 바뀌면 조용히 새 값에 맞춰 통과한다 — 여기 한 줄만이 값을 못박는다.
func TestLegFractionIsWhatTheUserChose(t *testing.T) {
	const want = 1.0
	if legFraction != want {
		t.Fatalf("legFraction = %v, 사용자가 정한 값은 %v (cap 전액)", legFraction, want)
	}
}

// **메이커 다리는 군중이 어디로 가든 그대로다.** 예전 봇은 최우선 매수호가를
// 따라 다녔다 — 그 경로가 남아 있으면 이 테스트가 잡는다.
//
// 테이커 다리는 반대로 시장이 정한다. 그러나 그 값도 **첫 시도 때 한 번** 읽고
// 얼린다 — 재호가는 여전히 없다.
func TestCrowdMovesButTheOrdersDoNot(t *testing.T) {
	h := newHarness(t)
	// 위로도 아래로도, 0.5 위로도 움직여 본다. 예전 로직이라면 각각 다른
	// 목표가를 냈을 자리다(추종 → 상한 → 추종).
	h.onStep = func(step int) {
		switch step {
		case 1:
			h.setCrowd(map[float64]float64{0.30: 100}, map[float64]float64{0.61: 100})
		case 3:
			h.setCrowd(map[float64]float64{0.55: 100}, map[float64]float64{0.62: 100})
		case 5:
			h.setCrowd(map[float64]float64{0.41: 100}, map[float64]float64{0.63: 100})
		case 7:
			h.setCrowd(map[float64]float64{0.49: 100}, map[float64]float64{0.64: 100})
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	ticks := h.orders.createdTicks()
	if len(ticks) != 1 || ticks[0] != limitPriceNum {
		t.Errorf("주문 틱 %v, 기대 [%d] — 군중을 따라갔다. 주문은 회차당 한 건뿐이다", ticks, limitPriceNum)
	}
	// 취소는 회차 종료 한 번뿐이다. 그보다 많으면 옮긴 것이다.
	if n := h.orders.removeCount(); n != 1 {
		t.Errorf("취소 %d회 — 회차 종료 취소 1회만 나가야 한다(주문을 옮기지 않는다)", n)
	}
}

// **메이커 다리에도 관통 방지가 없다.** 매도호가가 지정가 이하라도 그대로 건다 —
// 즉시 체결되고 테이커 수수료 2% 를 문다. 2026-08-12 사용자 결정이다.
//
// 이 테스트가 깨지는 날은 누군가 관통 방지를 되살린 날이고, 그것은 전략
// 변경이므로 조용히 지나가면 안 된다.
func TestOrderGoesOutEvenWhenItCrossesTheAsk(t *testing.T) {
	h := newHarness(t)
	h.setCrowd(map[float64]float64{0.30: 100}, map[float64]float64{0.40: 100})
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	ticks := h.orders.createdTicks()
	if len(ticks) != 1 || ticks[0] != limitPriceNum {
		t.Errorf("주문 틱 %v, 기대 [%d] — 매도호가 0.40 을 피해 내려갔다면 관통 방지가 되살아난 것이다", ticks, limitPriceNum)
	}
}

// 지정가 틱은 정밀도에서 유도한다. 리터럴 48 을 박으면 precision 3 인 마켓에서
// 0.048 에 걸린다 — 열 배 싼 가격이고, 채워지지 않는다.
func TestLimitTickComesFromThePrecision(t *testing.T) {
	for _, c := range []struct {
		precision int
		want      int64
	}{{2, 48}, {3, 480}, {4, 4800}, {18, 480_000_000_000_000_000}} {
		got, err := limitTick(c.precision)
		if err != nil {
			t.Fatalf("precision %d: %v", c.precision, err)
		}
		if got.V != c.want {
			t.Errorf("precision %d → 틱 %d, 기대 %d", c.precision, got.V, c.want)
		}
		if got.Float() != LimitPrice {
			t.Errorf("precision %d → 가격 %v, 기대 %v", c.precision, got.Float(), LimitPrice)
		}
	}
}

// precision 1 에서 0.47 은 존재하지 않는 가격이다. 0.4 나 0.5 로 반올림해 주면
// 사용자가 정하지 않은 가격에 걸게 되고, 0.5 는 "0.5 미만" 제약도 깬다.
func TestUnrepresentableLimitPriceIsAnError(t *testing.T) {
	if _, err := limitTick(1); err == nil {
		t.Fatal("precision 1 에서 0.47 을 만들어 냈다 — 표현할 수 없는 가격이다")
	}
	h := newHarness(t)
	h.round.Precision = 1
	h.book = ws.NewBook(1)
	h.runner.Book = h.book
	if err := h.run(); err == nil {
		t.Fatal("precision 1 회차를 돌렸다")
	}
	if h.orders.createdCount() != 0 {
		t.Error("표현할 수 없는 가격인데 주문이 나갔다")
	}
}

// ---------------------------------------------------------------------------
// 진입 창 — 회차 중간에는 걸지 않는다
// ---------------------------------------------------------------------------

// **회차 시작 창을 넘기면 아무것도 걸지 않는다.**
//
// `p_up` 은 회차 시작(+0분) 정보로 동결된 값이다. 시작 90초 뒤에 그 값으로
// 거는 것은 이미 90초 움직인 시장에 90초 전의 판단으로 베팅하는 것이고,
// G2 가 잰 엣지는 회차 시작 근처 진입을 가정한 값이다.
func TestLateRoundPlacesNothing(t *testing.T) {
	h := newHarness(t)
	h.runner.EntryWindow = 200 * time.Millisecond
	// 회차가 시작한 지 300ms 지난 뒤에야 이 회차를 잡았다(재시작 직후의 모양).
	h.clk.advance(300 * time.Millisecond)

	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.createdCount(); n != 0 {
		t.Errorf("주문 %d건 — 진입 창이 지난 회차에 걸었다", n)
	}
	if n := h.orders.removeCount(); n != 0 {
		t.Errorf("취소 %d회 — 걸지도 않았는데 거둘 것이 있었다", n)
	}
}

// 창 안이면 평소대로 건다. 위 가드가 "언제나 안 건다"로 굳으면 봇이 조용히
// 아무것도 하지 않는다.
func TestInsideTheWindowStillPlaces(t *testing.T) {
	h := newHarness(t)
	h.runner.EntryWindow = 500 * time.Millisecond
	h.clk.advance(100 * time.Millisecond)

	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.createdCount(); n != 1 {
		t.Errorf("주문 %d건, 기대 1건 — 창 안인데 걸지 않았다", n)
	}
}

// **창은 회차 시작에서 잰다. 우리가 이 회차를 잡은 시각이 아니다.**
//
// 회차를 제때 잡았어도 조회가 늦으면 주문은 얼마든지 늦게 나갈 수 있다 —
// 배선(-max-join-late)의 회차 선택 가드가 잡지 못하는 자리가 정확히 이것이고,
// 그래서 exec 안에 같은 창이 한 번 더 있다.
func TestSlowStartStillMissesTheWindow(t *testing.T) {
	h := newHarness(t)
	h.runner.EntryWindow = 200 * time.Millisecond
	// 회차는 제때 잡았다(경과 0). 그러나 체결 조회가 세 바퀴(300ms) 실패해
	// 그동안 주문을 낼 수 없었다.
	h.fills.pollFn = func(n int) ([]ledger.Fill, error) {
		if n < 3 {
			return nil, errors.New("일시적 조회 실패")
		}
		return nil, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.createdCount(); n != 0 {
		t.Errorf("주문 %d건 — 조회가 회복됐을 때는 이미 창이 지났다", n)
	}
}

// 재시도도 창을 지킨다. 재시도 경로에만 창이 없으면, 첫 시도가 실패한 회차만
// 창 밖에서 걸린다 — 로그로는 정상 주문과 구분되지 않는다.
func TestRetriesRespectTheWindow(t *testing.T) {
	h := newHarness(t)
	h.runner.EntryWindow = 200 * time.Millisecond
	h.runner.RejectBackoff = 300 * time.Millisecond // 다음 시도는 창 밖이다
	h.orders.createFn = func(n int, r Request) (CreateResult, error) {
		if r.Tick.V == limitPriceNum {
			return CreateResult{}, &fakeOrderError{safe: true, msg: "거래소 거부"}
		}
		id := fmt.Sprintf("ord-%d", n)
		return CreateResult{ID: id, Hash: "hash-" + id}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	// 첫 바퀴에 두 다리를 두드렸고, 메이커는 거부당했다. 백오프 뒤 재시도는
	// 창 밖이므로 다시 나가면 안 된다.
	if n := h.orders.createsAtTick(limitPriceNum); n != 1 {
		t.Errorf("메이커 다리 주문 생성 %d회, 기대 1회 — 재시도가 창을 넘어갔다", n)
	}
}

// **제로값을 '창 없음'으로 읽으면 안 된다.** 배선 실수 하나가 "회차 중간에도
// 건다"를 되살리고, 그 사실은 어떤 로그에도 남지 않는다.
func TestEntryWindowMustBeSet(t *testing.T) {
	h := newHarness(t)
	h.runner.EntryWindow = 0
	if err := h.run(); err == nil {
		t.Fatal("EntryWindow 가 0 인데 회차를 돌렸다")
	}
	if h.orders.createdCount() != 0 {
		t.Error("EntryWindow 가 0 인데 주문이 나갔다")
	}
}

// 메이커 지정가는 **사용자가 정한 값**이다. 나머지 시험들은 전부 상수를 참조하기
// 때문에, 상수 자체가 바뀌면 그 시험들은 새 값에 맞춰 조용히 통과한다. 여기 한
// 줄만이 "그 값이 얼마인가" 를 못박는다 — 이 시험이 깨지는 날은 전략이 바뀐
// 날이고, 그것은 조용히 지나가면 안 된다.
//
// 값의 이력은 [LimitPrice] 의 주석에 있다.
func TestMakerLimitPriceIsWhatTheUserChose(t *testing.T) {
	const want = 0.48
	if LimitPrice != want {
		t.Fatalf("LimitPrice = %v, 사용자가 정한 값은 %v — 전략이 바뀌었다면 이 줄을 함께 고쳐라", LimitPrice, want)
	}
	if limitPriceNum != 48 || limitPriceDen != 100 {
		t.Fatalf("지정가 %d/%d — 48/100 이어야 한다", limitPriceNum, limitPriceDen)
	}
}

// "0.5 미만 지정가 매수만" 은 사용자 제약이다. 상수를 잘못 고치는 날 그것이
// 조용히 사라지지 않도록 가드가 살아 있는지 본다.
func TestLimitPriceIsBelowHalf(t *testing.T) {
	if LimitPrice >= 0.5 {
		t.Fatalf("LimitPrice = %v — 0.5 미만이어야 한다", LimitPrice)
	}
	for _, p := range []int{2, 3, 4, 18} {
		tk, err := limitTick(p)
		if err != nil {
			t.Fatalf("precision %d: %v", p, err)
		}
		if ceiling := order.Ceiling(p).V; tk.V > ceiling {
			t.Errorf("precision %d: 틱 %d 가 상한 %d 을 넘는다", p, tk.V, ceiling)
		}
	}
}

// ---------------------------------------------------------------------------
// (2) Runner 는 Predictor 를 들고 다니지 않는다
// ---------------------------------------------------------------------------

// Runner 가 *live.Predictor 를 들면 회차 중 p_up 을 다시 계산하는 경로가 생긴다.
// 그것은 "+0분 정보만" 제약 위반이고, 어떤 값 테스트로도 드러나지 않는다.
func TestRunnerHasNoPredictorField(t *testing.T) {
	typ := reflect.TypeOf(Runner{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if strings.Contains(f.Type.String(), "Predictor") {
			t.Errorf("Runner.%s 가 %s 다 — 회차 중 p_up 재계산 경로가 열린다", f.Name, f.Type)
		}
	}
}

// RunRound 의 인자에도 Predictor 가 없어야 한다.
func TestRunRoundTakesNoPredictor(t *testing.T) {
	m, ok := reflect.TypeOf(&Runner{}).MethodByName("RunRound")
	if !ok {
		t.Fatal("RunRound 메서드가 없다")
	}
	for i := 0; i < m.Type.NumIn(); i++ {
		if strings.Contains(m.Type.In(i).String(), "Predictor") {
			t.Errorf("RunRound 의 %d번째 인자가 %s 다", i, m.Type.In(i))
		}
	}
}

// ---------------------------------------------------------------------------
// (3)(4) 정산·리베이트 — 아직 호출부가 없다는 사실을 고정한다
// ---------------------------------------------------------------------------

// packageSources 는 이 패키지의 비테스트 소스를 전부 읽는다.
func packageSources(t *testing.T) map[string]string {
	t.Helper()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("패키지 디렉터리를 읽지 못했다: %v", err)
	}
	out := map[string]string{}
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(n))
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		out[n] = string(b)
	}
	if len(out) == 0 {
		t.Fatal("패키지 소스를 하나도 찾지 못했다 — 이 테스트가 공허하게 통과했다")
	}
	return out
}

// **정산 결과는 거래소에서만 온다.** 바이낸스 봉으로 승패를 계산해 원장에
// 넣으면 G2 가 잰 정산 불일치(d≈0.30%)만큼 원장이 실제 현금흐름과 어긋난다.
// 거래소 정산 조회 경로가 아직 없으므로 exec 는 그 자리를 **비워 둔다.**
// 이 테스트는 "비어 있음"이 나중에 조용히 채워지는 것을 막는다.
func TestExecNeverWritesSettlement(t *testing.T) {
	for name, src := range packageSources(t) {
		// 주석에는 등장한다(왜 비었는지 적어야 하므로). 호출만 막는다.
		for _, bad := range []string{"RecordSettlement(", "ledger.Settlement{"} {
			if strings.Contains(stripComments(src), bad) {
				t.Errorf("%s 가 %s 를 쓴다 — 정산은 거래소 조회 경로가 생긴 뒤에만 기록한다", name, bad)
			}
		}
	}
}

// RebateValue 의 bool 인자는 컴파일러가 지켜주지 못한다. exec 에 호출부가
// 생기면 이 테스트가 먼저 깨지고, 그때 방향을 눈으로 확인하게 된다.
func TestExecHasNoRebateCallSiteYet(t *testing.T) {
	for name, src := range packageSources(t) {
		if strings.Contains(stripComments(src), "RebateValue(") {
			t.Errorf("%s 가 RebateValue 를 부른다 — bool 인자 방향을 리뷰에서 확인해야 한다", name)
		}
	}
}

// 방향 자체를 여기서도 못 박는다. exec 가 언젠가 이 값을 쓰게 되면 기준이 된다:
// 리베이트는 **반대편** 주식이므로 우리가 졌을 때만 값이 있다.
func TestRebateValuePaysOnlyWhenWeLose(t *testing.T) {
	r := ledger.Rebate{Shares: 5}
	if got := ledger.RebateValue(r, true); got != 0 {
		t.Errorf("우리가 이겼는데 리베이트 %v, 기대 0", got)
	}
	if got := ledger.RebateValue(r, false); got != 5 {
		t.Errorf("우리가 졌는데 리베이트 %v, 기대 5", got)
	}
}

// stripComments 는 줄 주석과 블록 주석을 대충 지운다. 주석 안의 이름 때문에
// 위 두 테스트가 오탐하지 않게 하는 용도다.
func stripComments(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				return b.String()
			}
			i += j
		case strings.HasPrefix(src[i:], "/*"):
			j := strings.Index(src[i:], "*/")
			if j < 0 {
				return b.String()
			}
			i += j + 2
		default:
			b.WriteByte(src[i])
			i++
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// (5) 원장 I/O 에러는 재시도가 아니라 무장 해제
// ---------------------------------------------------------------------------

// I/O 에러에서 같은 레코드를 다시 넣으면 중복 체결 줄이 생기고, 복구가 포지션을
// 실제보다 크게 읽어 한도를 넘겨 베팅한다. 대응은 재시도가 아니라 정지다.
func TestLedgerIOErrorDisarmsWithoutRetry(t *testing.T) {
	h := newHarness(t)
	ioErr := errors.New("no space left on device")
	h.led.fillFn = func(int, ledger.Fill) error { return ioErr }
	h.fills.pollFn = func(n int) ([]ledger.Fill, error) {
		if n != 1 {
			return nil, nil
		}
		return []ledger.Fill{{
			RoundStart: h.round.StartsAt.Unix(), MarketID: h.round.MarketID,
			Outcome: ledger.OutcomeUp, Shares: 2, PriceUSD: 0.45, At: h.clk.now(),
		}}, nil
	}
	err := h.run()
	if err == nil {
		t.Fatal("원장 I/O 에러인데 RunRound 가 nil 을 돌려줬다")
	}
	if !errors.Is(err, ioErr) {
		t.Errorf("에러가 원인을 잃었다: %v", err)
	}
	if n := h.led.count(); n != 1 {
		t.Errorf("원장 기록 시도 %d회 — 재시도하면 안 된다 (중복 체결 줄)", n)
	}
	if n := h.orders.removeCount(); n == 0 {
		t.Error("무장 해제인데 살아 있는 주문을 취소하지 않았다")
	}
}

// ErrInvalidRecord 는 파일을 손대지 않았다는 뜻이므로 그 줄만 버리고 계속한다.
// **다만 노출에서는 뺄 수 없다** — 돈은 이미 나갔고 우리가 적지 못했을 뿐이다.
func TestInvalidRecordSkipsLineButKeepsExposure(t *testing.T) {
	h := newHarness(t)
	h.led.fillFn = func(int, ledger.Fill) error {
		return fmt.Errorf("%w: 테스트", ledger.ErrInvalidRecord)
	}
	h.fills.pollFn = func(n int) ([]ledger.Fill, error) {
		if n != 1 {
			return nil, nil
		}
		// 명목 45.0 — cap 45.5 를 거의 다 쓴다. 이후 주문이 나가면 안 된다.
		return []ledger.Fill{{
			RoundStart: h.round.StartsAt.Unix(), MarketID: h.round.MarketID,
			Outcome: ledger.OutcomeUp, Shares: 100, PriceUSD: 0.45, At: h.clk.now(),
		}}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("ErrInvalidRecord 로 회차가 죽었다: %v", err)
	}
	if n := h.orders.createdCount(); n != 1 {
		t.Errorf("주문 %d건 — 기대 1건. 기록하지 못한 체결도 노출에 남아야 한다", n)
	}
}

// ---------------------------------------------------------------------------
// (6) 세 상태: RemovalLockUnknown / Rejected / Unaccounted
// ---------------------------------------------------------------------------

// **"보냈는데 응답 불명"을 실패로 단정하면 재시도로 이중 주문이 난다.**
func TestUnknownCreateIsNeverRetried(t *testing.T) {
	h := newHarness(t)
	unknown := &fakeOrderError{safe: false, msg: "결과 불명"}
	// 메이커 다리만 불명으로 만든다. 다리별로 봐야 한 다리의 재시도와 다른
	// 다리의 첫 시도가 같은 숫자에 섞이지 않는다.
	h.orders.createFn = func(n int, r Request) (CreateResult, error) {
		if r.Tick.V == limitPriceNum {
			return CreateResult{}, unknown
		}
		return CreateResult{ID: fmt.Sprintf("ord-%d", n)}, nil
	}
	err := h.run()
	if n := h.orders.createsAtTick(limitPriceNum); n != 1 {
		t.Errorf("메이커 다리 주문 생성 %d회 — 결과 불명 주문을 다시 보내면 둘이 들어간다", n)
	}
	// 식별자가 없으니 취소도 못 한다. 그 명목은 회차 끝까지 노출에 남고,
	// 회차가 끝나면 사람이 확인해야 한다 — 조용히 성공하면 안 된다.
	if err == nil {
		t.Fatal("취소할 수 없는 주문을 남긴 채 nil 을 돌려줬다")
	}
	if !strings.Contains(err.Error(), "식별자 없는 명목") {
		t.Errorf("에러가 원인을 말하지 않는다: %v", err)
	}
}

// 결과는 불명이지만 식별자는 받은 경우 — 재전송하지 않고 **취소**를 시도한다.
func TestUnknownCreateWithIDIsCancelledNotResent(t *testing.T) {
	h := newHarness(t)
	// 해시를 함께 준다 — 응답은 파싱됐는데 분류가 "불명" 인 경우다. 해시가
	// 있어야 취소 확인 뒤 **얼마나 찼는지 물어볼 수 있고**, 그래야 명목이
	// 풀린다. 해시가 없는 경우는 TestNoHashKeepsTheNotionalReserved 가 본다.
	h.orders.createFn = func(n int, r Request) (CreateResult, error) {
		id := fmt.Sprintf("ord-%d", n)
		if r.Tick.V == limitPriceNum {
			return CreateResult{ID: id, Hash: "hash-" + id}, &fakeOrderError{safe: false, msg: "결과 불명"}
		}
		return CreateResult{ID: id, Hash: "hash-" + id}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.removeCount(); n == 0 {
		t.Fatal("식별자가 있는데 취소를 시도하지 않았다")
	}
	// **다시 걸지 않는다.** 그 주문은 살아 있을 수 있고, 다리당 한 건이
	// 이 봇의 규약이다.
	if n := h.orders.createsAtTick(limitPriceNum); n != 1 {
		t.Errorf("메이커 다리 주문 생성 %d회 — 결과 불명 주문을 두고 또 걸면 노출이 두 배다", n)
	}
}

// 거래소가 명시적으로 거부한 주문은 **존재하지 않는다.** 그때만 다시 낸다 —
// 호가창에 우리 주문이 없으므로 "회차당 한 건" 은 깨지지 않고, 일시적 거부
// 하나로 회차를 통째로 버릴 이유도 없다.
func TestRejectedCreateMayBeRetried(t *testing.T) {
	h := newHarness(t)
	first := true
	h.orders.createFn = func(n int, r Request) (CreateResult, error) {
		if r.Tick.V == limitPriceNum && first {
			first = false
			return CreateResult{}, &fakeOrderError{safe: true, msg: "거래소 거부"}
		}
		id := fmt.Sprintf("ord-%d", n)
		return CreateResult{ID: id, Hash: "hash-" + id}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.createsAtTick(limitPriceNum); n != 2 {
		t.Errorf("메이커 다리 주문 생성 %d회, 기대 2회 — 거부 1회 뒤 성공 1회", n)
	}
	// 그리고 곧바로 다시 두드리지 않는다. 루프 주기가 50~100ms 라 백오프가
	// 없으면 회차 하나가 240 req/min 예산을 통째로 태운다.
	steps := h.orders.stepsAtTick(limitPriceNum)
	if len(steps) < 2 {
		t.Fatalf("메이커 다리가 %d회만 나갔다", len(steps))
	}
	if gap := steps[1] - steps[0]; gap < 5 {
		t.Errorf("재시도 간격이 %d스텝(=%dms)이다 — 기본 백오프 500ms 를 지키지 않았다", gap, gap*100)
	}
}

// 그 재시도에는 상한이 있다. 없으면 계속 거부하는 회차에서 300초 내내
// 요청을 퍼붓는다.
func TestRetriesAreBounded(t *testing.T) {
	h := newHarness(t)
	h.runner.RejectBackoff = time.Millisecond // 상한이 없으면 매 바퀴 두드린다
	h.orders.createFn = func(int, Request) (CreateResult, error) {
		return CreateResult{}, &fakeOrderError{safe: true, msg: "계속 거부"}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	// 시도 한 바퀴가 남은 다리를 두드린다 — 다리별로 정확히 maxPlaceAttempts 회다.
	for _, tick := range []int64{limitPriceNum} {
		if n := h.orders.createsAtTick(tick); n != maxPlaceAttempts {
			t.Errorf("틱 %d 주문 생성 시도 %d회, 기대 %d회", tick, n, maxPlaceAttempts)
		}
	}
}

// 분류 불가 에러는 "모른다"로 다룬다 — 이중 주문을 막는 쪽.
func TestUnclassifiedCreateErrorIsTreatedAsUnknown(t *testing.T) {
	h := newHarness(t)
	h.orders.createFn = func(n int, r Request) (CreateResult, error) {
		if r.Tick.V == limitPriceNum {
			return CreateResult{}, errors.New("무엇인지 모르는 실패")
		}
		return CreateResult{ID: fmt.Sprintf("ord-%d", n)}, nil
	}
	if err := h.run(); err == nil {
		t.Fatal("분류하지 못한 실패를 안전한 것으로 다뤘다")
	}
	if n := h.orders.createsAtTick(limitPriceNum); n != 1 {
		t.Errorf("메이커 다리 주문 생성 %d회 — 분류하지 못한 실패는 '보냈을 수 있다'로 다뤄야 한다", n)
	}
}

// 취소가 거부되면(잠금 창 안) 잠금이 풀린 뒤 다시 시도한다. 그동안 그 주문의
// 명목은 노출에 남는다.
func TestRejectedRemovalIsRetriedAndKeepsExposure(t *testing.T) {
	h := newHarness(t)
	h.runner.RejectBackoff = 200 * time.Millisecond
	h.orders.removeFn = func(n int, ids []string) (RemoveResult, error) {
		if n == 0 {
			return RemoveResult{Rejected: ids}, nil
		}
		return RemoveResult{Removed: ids}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.removeCount(); n < 2 {
		t.Errorf("취소 시도 %d회 — 거부된 취소는 잠금이 풀린 뒤 다시 해야 한다", n)
	}
}

// 응답 어디에도 나타나지 않은 ID 는 살아 있을 수 있다. 잊으면 그 노출은 어떤
// 한도에도 잡히지 않는다.
func TestUnaccountedRemovalKeepsOrderAndExposure(t *testing.T) {
	h := newHarness(t)
	h.runner.RejectBackoff = 200 * time.Millisecond
	h.orders.removeFn = func(n int, ids []string) (RemoveResult, error) {
		if n < 2 {
			return RemoveResult{Unaccounted: ids}, nil
		}
		return RemoveResult{Removed: ids}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.removeCount(); n < 3 {
		t.Errorf("취소 시도 %d회 — 응답에서 빠진 ID 를 잊으면 안 된다", n)
	}
}

// 잠금 시각을 모르면(RemovalLockUnknown) 취소를 시도하되, 거부되면 물러난다.
// 모른다는 이유로 영원히 취소하지 않으면 회차 종료 때 주문이 남는다.
func TestUnknownRemovalLockStillAttemptsCancel(t *testing.T) {
	h := newHarness(t)
	h.orders.createFn = func(n int, _ Request) (CreateResult, error) {
		return CreateResult{ID: fmt.Sprintf("ord-%d", n), RemovalLockUnknown: true}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.removeCount(); n == 0 {
		t.Error("잠금 시각을 모른다고 취소를 아예 시도하지 않았다 — 주문이 회차 끝까지 남는다")
	}
}

// ---------------------------------------------------------------------------
// (7) 시각은 Clock 에서 온다 — 파일에서 되읽지 않는다
// ---------------------------------------------------------------------------

// 시각을 파일에서 되읽지 않는다 — 직렬화를 거치면 단조 성분이 사라진다.
func TestExecNeverParsesPlacedFromFile(t *testing.T) {
	for name, src := range packageSources(t) {
		s := stripComments(src)
		for _, bad := range []string{"time.Parse(", "os.Open(", "os.ReadFile("} {
			if strings.Contains(s, bad) {
				t.Errorf("%s 가 %s 를 쓴다 — exec 는 파일에서 시각을 되읽지 않는다", name, bad)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 노출 상한·stale·회차 종료
// ---------------------------------------------------------------------------

// cap 이 최소 주문 $1 을 못 채우면 아무것도 걸지 않는다. 그리고 **다시 묻지
// 않는다** — 자본은 회차 시작에 동결된 값이라 답이 바뀔 수 없다.
func TestTooSmallACapPlacesNothing(t *testing.T) {
	h := newHarness(t)
	h.equity = risk.Equity{AvailableUSDT: 10} // cap 0.455 < $1
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.createdCount(); n != 0 {
		t.Errorf("주문 %d건 — 잔여가 최소 주문에 못 미치면 걸지 않는다", n)
	}
}

// 첫 주문 자체가 cap 을 넘지 않는다.
func TestFirstOrderNeverExceedsCap(t *testing.T) {
	h := newHarness(t)
	h.equity = risk.Equity{AvailableUSDT: 1000} // cap 45.5
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if h.orders.createdCount() == 0 {
		t.Fatal("주문이 나가지 않았다")
	}
	h.orders.mu.Lock()
	r := h.orders.creates[0]
	h.orders.mu.Unlock()
	notional := r.Shares * r.Tick.Float()
	if notional >= risk.Cap(h.equity) {
		t.Errorf("명목 %v 가 cap %v 이상이다 — 강한 부등호 위반", notional, risk.Cap(h.equity))
	}
	if notional < risk.MinOrderUSD {
		t.Errorf("명목 %v 가 최소 주문 $1 미만이다", notional)
	}
}

// 취소 확인 전 주문의 명목은 노출에 남는다. 회차가 그 상태로 끝나면 에러다 —
// 거래소에 살아 있을 수 있는 주문을 조용히 잊으면 안 된다.
func TestPendingCancelCountsAsExposure(t *testing.T) {
	h := newHarness(t)
	h.runner.RejectBackoff = 10 * time.Second
	h.runner.FinalCancelTimeout = time.Second
	h.orders.removeFn = func(n int, ids []string) (RemoveResult, error) {
		return RemoveResult{Rejected: ids}, nil
	}
	if err := h.run(); err == nil {
		t.Fatal("취소를 확인하지 못한 채 회차가 끝났는데 에러가 아니다")
	}
	if n := h.orders.createdCount(); n != 1 {
		t.Errorf("주문 %d건 — 회차당 한 건이어야 한다", n)
	}
}

// removalLockedUntil 안에서는 취소를 시도하지 않는다. 거부당하고 요청만 낭비한다.
//
// **회차 종료 취소만 남은 지금은 루프로 이 상태를 만들 수 없다** — 회차가 끝날
// 즈음이면 잠금은 이미 풀려 있다. 그래서 sweepPending 을 직접 부른다. 가드를
// 지우는 순간을 잡는 것이 이 테스트의 목적이고, 잠금 창은 거래소가 정하는
// 값이라 언제든 길어질 수 있다.
func TestSweepRespectsTheRemovalLock(t *testing.T) {
	now := time.Unix(1000, 0)
	st := &roundState{pending: []*openOrder{
		{id: "locked", notional: 1, retryAt: now.Add(600 * time.Millisecond)},
		{id: "free", notional: 1, retryAt: now},
	}}
	orders := &fakeOrders{}
	r := &Runner{Orders: orders}
	r.sweepPending(context.Background(), st, now)

	orders.mu.Lock()
	defer orders.mu.Unlock()
	if len(orders.removes) != 1 {
		t.Fatalf("취소 요청 %d회, 기대 1회", len(orders.removes))
	}
	if got := orders.removes[0]; len(got) != 1 || got[0] != "free" {
		t.Errorf("취소 대상 %v — 잠금 창 안의 주문까지 보냈다", got)
	}
}

// **낡은 호가창은 주문을 막지 않는다.** 가격이 상수라 호가창은 어떤 결정에도
// 들어가지 않는다. 예전에는 stale 이면 신규를 멈추고 기존을 취소했는데, 지금
// 그렇게 하면 근거 없이 회차를 버리는 일이다.
func TestStaleBookDoesNotStopTheOrder(t *testing.T) {
	h := newHarness(t)
	h.runner.StaleAfter = time.Nanosecond
	h.clk.advanceMono(time.Second) // 이미 오래된 호가창 (회차는 아직 살아 있다)
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.createdCount(); n != 1 {
		t.Errorf("주문 %d건 — 호가창이 낡았다고 회차를 버리면 안 된다", n)
	}
	// 그리고 회차 중간에 취소하지 않는다. 취소는 회차 종료 한 번뿐이다.
	if n := h.orders.removeCount(); n != 1 {
		t.Errorf("취소 %d회 — stale 을 이유로 주문을 거뒀다", n)
	}
}

// 다만 **기록에는 남는다.** 낡은 호가창으로 찍은 시장 값을 그대로 믿으면
// "0.47 이 그때 좋은 가격이었나" 를 사후에 틀리게 답하게 된다.
func TestStaleBookIsMarkedInTheLog(t *testing.T) {
	h := newHarness(t)
	h.runner.StaleAfter = time.Nanosecond
	h.clk.advanceMono(time.Second)
	var lines []string
	h.runner.Log = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	found := false
	for _, l := range lines {
		if strings.Contains(l, "낡았다") {
			found = true
		}
	}
	if !found {
		t.Errorf("낡은 호가창인데 주문 로그가 그 사실을 말하지 않는다: %v", lines)
	}
}

// 회차가 끝나면 미체결 전량 취소.
func TestLoopCancelsAllAtRoundEnd(t *testing.T) {
	h := newHarness(t)
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if h.orders.createdCount() != 1 {
		t.Fatalf("주문 %d건, 기대 1건", h.orders.createdCount())
	}
	h.orders.mu.Lock()
	defer h.orders.mu.Unlock()
	if len(h.orders.removes) == 0 {
		t.Fatal("회차 종료에 취소가 나가지 않았다")
	}
	last := h.orders.removes[len(h.orders.removes)-1]
	if len(last) != 1 || last[0] != "ord-0" {
		t.Errorf("마지막 취소 = %v, 기대 [ord-0] — 미체결을 전량 거둬야 한다", last)
	}
}

// 회차 종료 취소가 끝내 확인되지 않으면 에러다 — 잊힌 주문은 체결된다.
func TestUnconfirmedFinalCancelIsAnError(t *testing.T) {
	h := newHarness(t)
	h.runner.FinalCancelTimeout = 500 * time.Millisecond
	h.orders.removeFn = func(_ int, ids []string) (RemoveResult, error) {
		return RemoveResult{Unaccounted: ids}, nil
	}
	err := h.run()
	if err == nil {
		t.Fatal("미체결 주문을 취소하지 못했는데 nil 을 돌려줬다")
	}
	if !strings.Contains(err.Error(), "ord-0") {
		t.Errorf("에러에 남은 주문 ID 가 없다: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 자격·설정 가드
// ---------------------------------------------------------------------------

// 문턱 미달 회차는 아무것도 하지 않는다.
func TestIneligibleRoundPlacesNothing(t *testing.T) {
	h := newHarness(t)
	h.frozen.Eligible = false
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if h.orders.createdCount() != 0 {
		t.Errorf("문턱 미달인데 주문 %d건", h.orders.createdCount())
	}
}

// 동결된 예측이 이 회차의 것이 아니면 거래하지 않는다. 다른 회차의 p_up 으로
// 이 회차에 거는 것이 된다.
func TestFrozenMustMatchTheRound(t *testing.T) {
	h := newHarness(t)
	h.frozen.T = h.round.StartMS() + 300_000
	if err := h.run(); err == nil {
		t.Fatal("다른 회차의 동결값을 받아들였다")
	}
	if h.orders.createdCount() != 0 {
		t.Errorf("주문 %d건 — 대조 실패 전에 주문이 나갔다", h.orders.createdCount())
	}
}

// 방향이 Up 이면 UpTokenID 로, Down 이면 DownTokenID 로 간다. 뒤집히면 매 회차
// 반대에 베팅한다.
func TestDirectionSelectsTheToken(t *testing.T) {
	for _, c := range []struct {
		dir   string
		pUp   float64
		token string
	}{
		{ledger.OutcomeUp, 0.53, "111"},
		{ledger.OutcomeDown, 0.47, "222"},
	} {
		h := newHarness(t)
		h.frozen.Direction = c.dir
		h.frozen.PUp = c.pUp
		if err := h.run(); err != nil {
			t.Fatalf("%s: %v", c.dir, err)
		}
		if h.orders.createdCount() == 0 {
			t.Fatalf("%s: 주문이 나가지 않았다", c.dir)
		}
		h.orders.mu.Lock()
		got := h.orders.creates[0]
		h.orders.mu.Unlock()
		if got.TokenID != c.token {
			t.Errorf("%s → 토큰 %q, 기대 %q", c.dir, got.TokenID, c.token)
		}
		if got.Outcome != c.dir {
			t.Errorf("%s → outcome %q", c.dir, got.Outcome)
		}
	}
}

func TestUnknownDirectionIsRefused(t *testing.T) {
	h := newHarness(t)
	h.frozen.Direction = "up" // 소문자는 우리 규약이 아니다
	if err := h.run(); err == nil {
		t.Fatal("모르는 방향을 받아들였다")
	}
	if h.orders.createdCount() != 0 {
		t.Error("모르는 방향인데 주문이 나갔다")
	}
}

// **패닉하지 않는다.** precision 이 망가진 회차를 받으면 첫 판단 전에 에러로
// 빠져나온다 — quote.Ceiling 은 그 값에서 패닉하고, 살아 있는 주문을 든 채
// 죽으면 취소도 못 한다.
func TestBadPrecisionErrorsInsteadOfPanicking(t *testing.T) {
	for _, p := range []int{0, -1, 19, 100} {
		h := newHarness(t)
		h.round.Precision = p
		err := h.run()
		if err == nil {
			t.Errorf("precision %d 를 받아들였다", p)
		}
		if h.orders.createdCount() != 0 {
			t.Errorf("precision %d 에서 주문이 나갔다", p)
		}
	}
}

func TestMissingDependenciesAreRefused(t *testing.T) {
	cases := map[string]func(*Runner){
		"Book":   func(r *Runner) { r.Book = nil },
		"Orders": func(r *Runner) { r.Orders = nil },
		"Fills":  func(r *Runner) { r.Fills = nil },
		"Ledger": func(r *Runner) { r.Ledger = nil },
	}
	for name, break_ := range cases {
		h := newHarness(t)
		break_(h.runner)
		if err := h.run(); err == nil {
			t.Errorf("%s 가 nil 인데 회차를 돌렸다", name)
		}
	}
}

// 체결을 셀 수 없으면 신규 주문을 내지 않는다 — 노출 상한을 지킬 근거가 없다.
func TestFillsPollErrorBlocksNewOrdersButNotTheRound(t *testing.T) {
	h := newHarness(t)
	h.fills.pollFn = func(n int) ([]ledger.Fill, error) {
		if n == 0 {
			return nil, errors.New("일시적 조회 실패")
		}
		return nil, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("일시적 체결 조회 실패가 회차를 죽였다: %v", err)
	}
	if h.orders.createdCount() == 0 {
		t.Error("조회가 회복된 뒤에도 주문을 내지 않았다")
	}
	h.orders.mu.Lock()
	defer h.orders.mu.Unlock()
	// 첫 바퀴에서는 못 냈어야 한다.
	if h.firstCreateStepLocked() == 0 {
		t.Error("체결을 세지 못한 바퀴에서 주문을 냈다")
	}
}

// 체결은 원장에 그대로 들어간다. 매수는 언제나 지출이다.
func TestFillsAreRecordedToTheLedger(t *testing.T) {
	h := newHarness(t)
	want := ledger.Fill{
		RoundStart: h.round.StartsAt.Unix(), MarketID: h.round.MarketID,
		Outcome: ledger.OutcomeUp, Shares: 2, PriceUSD: 0.45, At: h.clk.now(),
	}
	h.fills.pollFn = func(n int) ([]ledger.Fill, error) {
		if n != 1 {
			return nil, nil
		}
		return []ledger.Fill{want}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if h.led.count() != 1 {
		t.Fatalf("원장 기록 %d건, 기대 1건", h.led.count())
	}
	h.led.mu.Lock()
	got := h.led.fills[0]
	h.led.mu.Unlock()
	if got != want {
		t.Errorf("기록된 체결이 다르다: %+v vs %+v", got, want)
	}
	if c := ledger.FillCost(got); c != 0.9 {
		t.Errorf("FillCost = %v, 기대 0.9 (2×0.45)", c)
	}
}

// ---------------------------------------------------------------------------
// 극단값 — 지시받지 않은 조사에서 나온 것들
// ---------------------------------------------------------------------------

// 체결 명목이 NaN 이면(망가진 피드) 회차가 죽지 않고, 그 줄은 원장에
// 들어가지 않는다. 파일이 오염되면 집계하는 쪽이 오염 사실조차 모른다.
func TestNaNFillDoesNotKillTheRound(t *testing.T) {
	h := newHarness(t)
	h.fills.pollFn = func(n int) ([]ledger.Fill, error) {
		if n != 1 {
			return nil, nil
		}
		return []ledger.Fill{{
			RoundStart: h.round.StartsAt.Unix(), MarketID: h.round.MarketID,
			Outcome: ledger.OutcomeUp, Shares: math.NaN(), PriceUSD: 0.45, At: h.clk.now(),
		}}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("NaN 체결이 회차를 죽였다: %v", err)
	}
	// RecordFill 은 한 번만 불린다. 재시도하면 중복 체결 줄이 생긴다.
	if h.led.count() != 1 {
		t.Errorf("원장 기록 시도 %d회, 기대 1회", h.led.count())
	}
}

// 같은 식별자가 두 번 오면 취소 확인 **한 번**이 두 주문의 명목을 함께 빼 준다.
// 그러면 한도가 조용히 늘어난다.
//
// **2026-08-14 이전에는 루프가 이 상태에 닿지 못했다** — 회차당 한 건만 걸었고
// risk.Shares 가 잔여를 거의 다 써서 두 번째 주문이 성립하지 않았기 때문이다.
// 그 전제가 그날 사라졌다: 이제 회차마다 다리를 둘 걸고, 두 주문이 동시에
// 살아 있는 것이 정상이다. 거래소가 두 다리에 같은 식별자를 주는 순간 이
// 가드가 유일한 방어선이 된다.
func TestDuplicateOrderIDIsNotTracked(t *testing.T) {
	h := newHarness(t)
	st := &roundState{live: []*openOrder{{id: "same", tick: 45, shares: 10, notional: 4.5}}}
	h.orders.createFn = func(int, Request) (CreateResult, error) {
		return CreateResult{ID: "same"}, nil // 거래소가 같은 ID 를 반복한다
	}
	req := Request{
		Round: h.round, Outcome: ledger.OutcomeUp, TokenID: "111",
		Tick: order.NewTick(44, testPrecision), Shares: 10,
	}
	if _, err := h.runner.transmit(context.Background(), st, req, h.clk.now()); err != nil {
		t.Fatalf("transmit: %v", err)
	}
	if len(st.live) != 1 || st.live[0].tick != 45 {
		t.Errorf("살아 있는 주문이 %d건(틱 %v) — 중복 식별자가 추적 목록에 들어갔다",
			len(st.live), liveTicks(st))
	}
	for _, o := range st.pending {
		if o.id == "same" {
			t.Error("중복 식별자를 취소 대기에 담았다 — 확인 한 번이 둘의 명목을 뺀다")
		}
	}
	if st.unknownNotional != req.Notional() {
		t.Errorf("추적 불가 명목 %v, 기대 %v — 노출에서 빠지면 한도가 늘어난다", st.unknownNotional, req.Notional())
	}
}

// 취소 배치는 거래소 상한(100)을 넘지 않는다. 넘기면 rest 층이 요청 자체를
// 거절해서 **아무것도** 취소되지 않는다.
func TestRemoveBatchNeverExceedsExchangeLimit(t *testing.T) {
	st := &roundState{}
	now := time.Unix(1000, 0)
	for i := 0; i < maxRemoveBatch+37; i++ {
		st.pending = append(st.pending, &openOrder{id: fmt.Sprintf("o%d", i), notional: 1, retryAt: now})
	}
	orders := &fakeOrders{removeFn: func(_ int, ids []string) (RemoveResult, error) {
		if len(ids) > maxRemoveBatch {
			return RemoveResult{}, fmt.Errorf("취소 배치 상한 초과: %d개", len(ids))
		}
		return RemoveResult{Removed: ids}, nil
	}}
	r := &Runner{Orders: orders}
	r.sweepPending(context.Background(), st, now)
	if len(st.pending) != 37 {
		t.Errorf("남은 미확인 주문 %d건, 기대 37건 — 상한을 넘겨 요청이 통째로 거절됐을 수 있다", len(st.pending))
	}
	orders.mu.Lock()
	defer orders.mu.Unlock()
	if len(orders.removes[0]) != maxRemoveBatch {
		t.Errorf("첫 배치 %d건, 기대 %d건", len(orders.removes[0]), maxRemoveBatch)
	}
}

// Eligible 플래그만 믿으면 손으로 만든 Frozen 하나가 문턱 0.0172 를 지운다.
func TestEligibleFlagAloneIsNotEnough(t *testing.T) {
	h := newHarness(t)
	h.frozen.Confidence = 0.001 // Eligible=true 인데 문턱 미달
	if err := h.run(); err == nil {
		t.Fatal("문턱 미달인 동결값을 Eligible 만 보고 받아들였다")
	}
	if h.orders.createdCount() != 0 {
		t.Error("문턱 미달인데 주문이 나갔다")
	}
}

// 방향과 확률이 어긋나면 매 회차 정확히 반대에 베팅한다.
func TestDirectionMustAgreeWithPUp(t *testing.T) {
	h := newHarness(t)
	h.frozen.PUp = 0.5 - live.ConfidenceThreshold // 하락 확률이 높은데
	h.frozen.Direction = ledger.OutcomeUp
	if err := h.run(); err == nil {
		t.Fatal("p_up 과 방향이 어긋난 동결값을 받아들였다")
	}
}

// WS 고루틴이 호가창을 갱신하는 동안 루프가 읽는다. -race 로 도는 것이 목적이다.
func TestConcurrentBookUpdatesAreRaceFree(t *testing.T) {
	h := newHarness(t)
	// 호가창은 이 테스트에서 고루틴이 쥔다. 하네스가 함께 쓰면 두 쪽의
	// updateTimestampMs 가 섞여 "적용 안 됨"이 나온다.
	h.autoBook = false
	stop := make(chan struct{})
	done := make(chan struct{})
	var ts int64 = 1_000_000
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			ts++
			f := ws.Frame{RecvMonoNs: h.clk.monoNs()}
			f.Msg.Data = []byte(fmt.Sprintf(
				`{"bids":[[0.45,100],[0.44,50]],"asks":[[0.60,100]],"updateTimestampMs":%d}`, ts))
			if _, err := h.book.Apply(f); err != nil {
				return
			}
		}
	}()
	err := h.run()
	close(stop)
	<-done
	if err != nil {
		t.Fatalf("RunRound: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 하네스 보조
// ---------------------------------------------------------------------------

// fakeOrderError 는 rest.OrderError 와 같은 모양의 재시도 분류를 흉내낸다.
// rest 를 임포트하지 않는 이유: exec 는 인터페이스로만 결합한다.
type fakeOrderError struct {
	safe bool
	msg  string
}

func (e *fakeOrderError) Error() string     { return e.msg }
func (e *fakeOrderError) SafeToRetry() bool { return e.safe }

// firstRemoveStep 은 첫 취소 요청이 몇 번째 폴링 주기에 나갔는지다.
func (h *harness) firstRemoveStep() int {
	h.orders.mu.Lock()
	defer h.orders.mu.Unlock()
	if len(h.orders.removeSteps) == 0 {
		return -1
	}
	return h.orders.removeSteps[0]
}

func (h *harness) firstCreateStepLocked() int {
	if len(h.orders.createSteps) == 0 {
		return -1
	}
	return h.orders.createSteps[0]
}

// ---------------------------------------------------------------------------
// 관측 훅 — 봇이 자기 상태를 밖으로 알려주는 유일한 경로
// ---------------------------------------------------------------------------

// roundState 는 unexported 이고 그래야 한다(포인터를 담고 있어 밖에서 읽는
// 동안 루프가 고쳐 쓴다). 대신 값 복사를 내보낸다 — 그것이 없으면 감시가
// 노출 불변식을 볼 방법이 없다.
func TestRunnerObservesExposure(t *testing.T) {
	h := newHarness(t)
	var got []Observation
	h.runner.Observe = func(o Observation) { got = append(got, o) }

	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Observe 가 한 번도 불리지 않았다")
	}
	for i, o := range got {
		if o.Exposure.Total() < 0 {
			t.Errorf("%d번째 관측의 노출이 음수다: %+v", i, o.Exposure)
		}
	}
	// **매 바퀴 관측한다.** 회차 끝에 한 번만 부르면 감시가 회차 내내 눈을
	// 감고 있는 것과 같다 — 노출 위반은 회차 중간에 일어난다.
	if len(got) < 3 {
		t.Fatalf("관측이 %d회뿐이다 — 매 바퀴 부르지 않는다", len(got))
	}
	// 회차 중간에는 걸린 주문이 보여야 한다. 안 보이면 이 훅은 빈 값만
	// 나르고 있는 것이다.
	sawLive := false
	for _, o := range got[:len(got)-1] {
		if len(o.OpenIDs) > 0 {
			sawLive = true
			break
		}
	}
	if !sawLive {
		t.Error("회차 내내 미체결이 한 번도 관측되지 않았다")
	}

	// **회차 종료 뒤의 마지막 관측은 깨끗해야 한다.** cancelEverything 이
	// 지나간 자리이고, 여기 미체결이 남아 있으면 그것이 곧 사고다.
	last := got[len(got)-1]
	if len(last.OpenIDs) != 0 {
		t.Errorf("회차 종료 뒤에도 미체결이 %d건 남았다: %v", len(last.OpenIDs), last.OpenIDs)
	}
}

// **취소 미확인 주문도 관측에 담긴다.** 그 주문은 아직 체결될 수 있으므로
// 노출에 남아 있고, 밖에서 그것을 못 보면 봇이 죽었을 때 사람이 확인할
// 근거가 사라진다 — 개인키 없는 모니터는 미체결을 독립으로 조회할 수 없다.
func TestObserveIncludesPendingCancels(t *testing.T) {
	h := newHarness(t)
	// 취소 요청은 받되 사라졌다고 확인해 주지 않는다. 회차 종료 취소가
	// 그렇게 끝나면 그 주문은 거래소에 살아 있을 수 있다.
	h.runner.FinalCancelTimeout = 500 * time.Millisecond
	h.orders.removeFn = func(_ int, ids []string) (RemoveResult, error) {
		return RemoveResult{Unaccounted: append([]string(nil), ids...)}, nil
	}

	var sawPending bool
	h.runner.Observe = func(o Observation) {
		if o.Exposure.PendingCancel > 0 && len(o.OpenIDs) > 0 {
			sawPending = true
		}
	}
	_ = h.run() // 미확인 주문이 남으면 회차가 에러로 끝날 수 있다 — 그것은 여기 관심사가 아니다
	if !sawPending {
		t.Error("취소 미확인 주문이 관측에 담기지 않았다")
	}
}

// 주문을 걸면 LastActionAt 이 찍혀야 한다. 이 봇은 회차당 한 번만 거므로
// 여기서 안 찍히면 회차 전체가 "아무 행동 없음" 으로 보인다.
func TestObserveStampsLastActionOnFirstPlace(t *testing.T) {
	h := newHarness(t)
	var first Observation
	var once bool
	h.runner.Observe = func(o Observation) {
		if !once && len(o.OpenIDs) > 0 {
			first, once = o, true
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if !once {
		t.Fatal("주문이 걸린 관측이 없다")
	}
	if first.LastActionAt.IsZero() {
		t.Error("주문을 걸었는데 LastActionAt 이 제로다")
	}
}

// **LastLoopAt 은 행동이 없어도 매 바퀴 전진해야 한다.**
//
// 감시가 "루프가 멎었는가" 를 이 값 하나로 판정한다. 행동할 때만 찍으면
// 정상 회차(한 번 걸고 군중이 안 움직임)가 멎은 것으로 보이고, 실거래
// 2026-08-11 07:46 의 오경보가 정확히 그 모양이었다.
func TestObserveStampsLoopEveryIteration(t *testing.T) {
	h := newHarness(t)
	var stamps []time.Time
	var actions []time.Time
	h.runner.Observe = func(o Observation) {
		stamps = append(stamps, o.LastLoopAt)
		actions = append(actions, o.LastActionAt)
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if len(stamps) < 3 {
		t.Fatalf("관측이 %d회뿐이다 — 매 바퀴 부르지 않는다", len(stamps))
	}
	for i, ts := range stamps {
		if ts.IsZero() {
			t.Fatalf("%d번째 관측의 LastLoopAt 이 제로다", i)
		}
	}
	// 시계가 전진했는데 도장이 그대로면 한 번만 찍고 재사용하는 것이다.
	if !stamps[len(stamps)-1].After(stamps[0]) {
		t.Errorf("LastLoopAt 이 전진하지 않았다: %v → %v", stamps[0], stamps[len(stamps)-1])
	}
	// 그리고 그 전진이 행동과 무관해야 한다. 마지막 두 관측 사이에 행동이
	// 없었는데 루프 도장만 전진한 구간이 최소 하나는 있어야 한다.
	sawIdleAdvance := false
	for i := 1; i < len(stamps); i++ {
		if actions[i].Equal(actions[i-1]) && stamps[i].After(stamps[i-1]) {
			sawIdleAdvance = true
			break
		}
	}
	if !sawIdleAdvance {
		t.Error("행동 없이 전진한 바퀴가 없다 — LastLoopAt 이 LastActionAt 을 따라가고 있다")
	}
}

// 관측은 부수 기능이다. 그 부재가 거래를 막으면 안 된다.
func TestRunnerWorksWithoutObserve(t *testing.T) {
	h := newHarness(t)
	h.runner.Observe = nil
	if err := h.run(); err != nil {
		t.Fatalf("Observe 없이 실패했다: %v", err)
	}
}

// 관측자가 터졌다고 살아 있는 주문을 든 채 죽으면 취소도 못 한다 —
// 이 패키지 전체의 원칙이다.
func TestObservePanicDoesNotKillRound(t *testing.T) {
	h := newHarness(t)
	h.runner.Observe = func(Observation) { panic("관측자가 터졌다") }
	if err := h.run(); err != nil {
		t.Fatalf("관측자 패닉이 회차를 죽였다: %v", err)
	}
}

// **관측 훅이 회차 진행을 바꿀 수 없어야 한다.** 반환값이 있으면 언젠가 그
// 값이 판단에 쓰이고, 그것은 P6 설계서 §9 의 단방향 불변식을 정반대로 깨는
// 일이다 — 감시자가 거래를 바꾸게 된다.
func TestObserveSignatureReturnsNothing(t *testing.T) {
	f, ok := reflect.TypeOf(Runner{}).FieldByName("Observe")
	if !ok {
		t.Fatal("Runner 에 Observe 필드가 없다")
	}
	if f.Type.Kind() != reflect.Func {
		t.Fatalf("Observe 가 함수가 아니다: %v", f.Type)
	}
	if n := f.Type.NumOut(); n != 0 {
		t.Errorf("Observe 가 %d개를 돌려준다 — 관측 전용이어야 한다", n)
	}
}

// 관측 타입이 internal/beat 를 끌고 오면 그 계약이 바뀔 때마다 이 패키지
// 테스트가 깨진다. 소스 스캔으로 막는다.
// **주석은 벗겨내고 본다.** 이 파일들은 왜 그렇게 했는지를 주석에 길게 적는
// 관례라, 금지된 이름이 설명 안에 나오는 것이 정상이다. 그것까지 잡으면
// 오탐으로 이 가드가 먼저 꺼진다.
func TestExecDoesNotImportBeat(t *testing.T) {
	for name, src := range packageSources(t) {
		if strings.Contains(stripComments(src), "internal/beat") {
			t.Errorf("%s 가 internal/beat 를 임포트한다 — 계약 타입은 배선의 몫이다", name)
		}
	}
}

// **LastActionAt 은 회차당 한 번만 움직인다.** 군중이 아무리 움직여도 우리는
// 주문을 옮기지 않으므로, 이 값이 두 번 이상 바뀌면 어딘가 재호가 경로가
// 되살아난 것이다.
func TestLastActionMovesOnlyOnce(t *testing.T) {
	h := newHarness(t)
	var stamps []time.Time
	h.runner.Observe = func(o Observation) {
		if o.LastActionAt.IsZero() {
			return
		}
		if len(stamps) == 0 || !stamps[len(stamps)-1].Equal(o.LastActionAt) {
			stamps = append(stamps, o.LastActionAt)
		}
	}
	h.onStep = func(step int) {
		switch step {
		case 3:
			h.setCrowd(map[float64]float64{0.48: 100}, map[float64]float64{0.60: 100})
		case 6:
			h.setCrowd(map[float64]float64{0.30: 100}, map[float64]float64{0.60: 100})
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if len(stamps) != 1 {
		t.Errorf("LastActionAt 이 %d번 움직였다 (%v) — 회차당 한 번이어야 한다", len(stamps), stamps)
	}
}

// 관측마다 새 슬라이스를 준다. 같은 배열을 돌려주면 다음 바퀴가 그것을
// 고쳐 써서, 밖에서 들고 있던 값이 조용히 바뀐다.
func TestObservationsAreIndependent(t *testing.T) {
	h := newHarness(t)
	var snaps [][]string
	h.runner.Observe = func(o Observation) { snaps = append(snaps, o.OpenIDs) }
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	for i := 0; i < len(snaps); i++ {
		for j := i + 1; j < len(snaps); j++ {
			if len(snaps[i]) > 0 && len(snaps[j]) > 0 && &snaps[i][0] == &snaps[j][0] {
				t.Fatalf("%d번째와 %d번째 관측이 같은 배열을 공유한다", i, j)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// 체결이 포지션이 된 뒤에도 다시 걸지 않는다
// ---------------------------------------------------------------------------

// **사용자가 물은 그 순서를 그대로 재현한다** (2026-08-12):
// 주문이 체결되어 미체결이 포지션으로 바뀌면, 봇이 "걸린 주문이 없다"로 읽고
// 같은 크기를 한 번 더 걸어 회차 명목이 상한의 두 배가 됐다.
//
// 그 경로에는 자리가 둘이다:
//
//  1. retireFullyFilled 가 전량 체결된 주문을 st.live 에서 뺀다 — 이중
//     계산을 없애려고 넣은 것인데, **뺀 순간 "미체결 없음" 이 된다.**
//  2. 예전에는 그 상태를 quote 가 Place 로 읽어 곧바로 다시 걸었다.
//
// 지금은 2번 자체가 없다(회차당 한 건). 그래도 이 시험을 두는 이유는,
// 1번이 남아 있는 한 "미체결 없음" 상태가 회차 중간에 반드시 만들어지기
// 때문이다 — 재주문 경로가 어떤 이유로든 되살아나면 이 시험이 먼저 깨진다.
func TestFilledOrderBecomingAPositionDoesNotTriggerAnother(t *testing.T) {
	h := newHarness(t)
	// cap 4.55. 첫 주문은 9주 @0.47 = 4.23 이다.
	const filled = 9

	h.fills.pollFn = func(n int) ([]ledger.Fill, error) {
		if n != 2 {
			return nil, nil
		}
		// 걸어 둔 주문이 **전량 체결**된다 = 미체결이 포지션이 됐다.
		return []ledger.Fill{{
			RoundStart: h.round.StartsAt.Unix(), MarketID: h.round.MarketID,
			Outcome: ledger.OutcomeUp, Shares: filled, PriceUSD: 0.47, At: h.clk.now(),
		}}, nil
	}
	// 군중을 계속 흔든다. 예전 로직이라면 "미체결 없음 + 목표가 있음" 이
	// 그대로 신규 주문이었다.
	h.onStep = func(step int) {
		if step%2 == 0 {
			h.setCrowd(map[float64]float64{0.45: 100}, map[float64]float64{0.60: 100})
		} else {
			h.setCrowd(map[float64]float64{0.44: 100}, map[float64]float64{0.60: 100})
		}
	}

	var peak float64
	h.runner.Observe = func(o Observation) {
		if tot := o.Exposure.Total(); tot > peak {
			peak = tot
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}

	h.orders.mu.Lock()
	creates := append([]Request(nil), h.orders.creates...)
	h.orders.mu.Unlock()

	if len(creates) != 1 {
		var ticks []int64
		var sum float64
		for _, r := range creates {
			ticks = append(ticks, r.Tick.V)
			sum += r.Notional()
		}
		t.Fatalf("주문 %d건(틱 %v, 명목 합 %.4f) — 체결이 포지션이 된 뒤 다시 걸었다", len(creates), ticks, sum)
	}
	// 회차 명목의 최댓값이 상한을 넘으면 안 된다. 실측 사고에서는 상한의
	// 1.9배였다.
	cap := risk.Cap(h.equity)
	if placed := creates[0].Notional(); placed >= cap {
		t.Errorf("건 명목 %.4f 가 상한 %.4f 이상이다", placed, cap)
	}
	if peak > cap+1e-9 {
		t.Errorf("회차 중 관측된 노출 최댓값 %.4f > 상한 %.4f", peak, cap)
	}
	// 그리고 체결은 노출에서 사라지지 않아야 한다 — 나간 돈이다.
	if peak < creates[0].Notional()-1e-9 {
		t.Errorf("노출 최댓값 %.4f 가 건 명목 %.4f 보다 작다 — 체결이 노출에서 빠졌다", peak, creates[0].Notional())
	}
}

// 취소가 확인된 주문이 사실은 체결돼 있던 경우 — 2026-08-11 의 실제 손실
// 경로다. 지금은 회차 종료 취소에서만 일어나지만, 그 명목이 확인 전에
// 풀리면 안 된다는 규약은 그대로다.
func TestConfirmedCancelThatActuallyFilledKeepsItsNotional(t *testing.T) {
	h := newHarness(t)
	h.orders.filledFn = func(hash string) (float64, error) {
		return 9, nil // 취소가 확인된 그 주문은 사실 전량 체결돼 있었다
	}
	var peak float64
	h.runner.Observe = func(o Observation) {
		if tot := o.Exposure.Total(); tot > peak {
			peak = tot
		}
	}
	if err := h.run(); err != nil && !isDisarm(err) {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.createdCount(); n != 1 {
		t.Errorf("주문 %d건 — 취소 확인을 '안 찼다'로 읽고 다시 걸었다", n)
	}
	if cap := risk.Cap(h.equity); peak > cap+1e-9 {
		t.Errorf("노출 최댓값 %.4f > 상한 %.4f", peak, cap)
	}
}

// liveTicks 는 살아 있는 주문의 틱 목록이다. 실패 메시지를 읽을 수 있게 한다.
func liveTicks(st *roundState) []int64 {
	out := make([]int64, 0, len(st.live))
	for _, o := range st.live {
		out = append(out, o.tick)
	}
	return out
}

// ---------------------------------------------------------------------------
// 첫 바퀴 체결 조회 건너뛰기 (2026-08-14)
// ---------------------------------------------------------------------------
//
// REST 요청 사이에 333ms 가 강제로 끼므로(rest.minInterval), 회차 시작의
// 세 요청(equity → 체결 조회 → 주문 생성) 중 가운데를 빼면 주문이 그만큼
// 일찍 나간다. 테이커 다리는 호가창에서 가격을 읽으므로 이 지연이 곧 오차다.

// 회차가 우리 눈앞에서 시작했으면 첫 바퀴 체결 조회를 건너뛴다.
func TestFirstFillPollIsSkippedWhenWeWatchedTheRoundStart(t *testing.T) {
	h := newHarness(t)
	h.runner.StartedAt = h.round.StartsAt.Add(-time.Minute) // 회차보다 먼저 살아 있었다
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.fills.pollCount(); n == 0 {
		t.Fatal("체결 조회가 아예 없다 — 두 번째 바퀴부터는 물어야 한다")
	}
	// 첫 주문이 첫 조회보다 먼저 나가야 한다. 그게 이 최적화의 전부다.
	if h.orders.createdCount() == 0 {
		t.Fatal("주문이 나가지 않았다")
	}
	if h.fills.firstPollStep() <= h.orders.firstCreateStep() {
		t.Errorf("체결 조회(스텝 %d)가 첫 주문(스텝 %d)보다 앞이다 — 건너뛰지 않았다",
			h.fills.firstPollStep(), h.orders.firstCreateStep())
	}
}

// **재시작 구멍.** 직전 프로세스가 이 회차에 주문을 내고 죽었을 수 있다.
// 새 프로세스의 roundState 는 비어 있지만 거래소에는 우리 체결이 있고, 그것을
// 못 세면 노출 상한이 그만큼 늘어난다.
func TestRestartedProcessAlwaysPollsFirst(t *testing.T) {
	h := newHarness(t)
	h.runner.StartedAt = h.round.StartsAt.Add(time.Second) // 회차가 시작한 뒤에 기동했다
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if h.fills.firstPollStep() > h.orders.firstCreateStep() {
		t.Error("재시작한 프로세스가 체결 조회를 건너뛰고 주문했다 — 직전 프로세스의 체결이 노출에서 빠진다")
	}
}

// StartedAt 을 배선하지 않았으면 건너뛰지 않는다. 제로값이 "언제나 안전"으로
// 읽히면 배선을 잊은 날 조용히 구멍이 열린다.
func TestUnwiredStartedAtNeverSkips(t *testing.T) {
	h := newHarness(t) // StartedAt 제로
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if h.fills.firstPollStep() > h.orders.firstCreateStep() {
		t.Error("StartedAt 이 제로인데 건너뛰었다")
	}
}

// 늦게 잡힌 회차에서는 묻는다. 그 사이에 무슨 일이 있었는지 알 수 없다.
func TestLateJoinStillPolls(t *testing.T) {
	h := newHarness(t)
	h.runner.StartedAt = h.round.StartsAt.Add(-time.Minute)
	h.runner.EntryWindow = 5 * time.Second
	h.clk.advance(3 * time.Second) // 유예 2초를 넘겨 합류
	h.round.EndsAt = h.round.StartsAt.Add(10 * time.Second)
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if h.fills.pollCount() == 0 {
		t.Fatal("늦게 합류했는데 체결 조회를 한 번도 하지 않았다")
	}
	if h.orders.createdCount() > 0 && h.fills.firstPollStep() > h.orders.firstCreateStep() {
		t.Error("유예 2초 밖에서 합류했는데 체결 조회를 건너뛰었다")
	}
}

// 0.5 상한이 **실제로 거부하는지** 본다.
//
// 상수가 0.48 인 한 [limitTick] 안의 그 분기는 영영 돌지 않는다. 그래서 검사를
// 통째로 지워도 모든 시험이 통과했다(2026-08-14 변이 M4). 여기서는 가격을 직접
// 넣어 분기를 밟는다 — 이 가드는 미래에 상수를 잘못 고치는 날을 위한 것이고,
// 그날 정말 막는지는 지금 확인해 둬야 한다.
func TestLimitTickRefusesHalfOrAbove(t *testing.T) {
	for _, c := range []struct {
		name      string
		num, den  int64
		precision int
		wantErr   bool
	}{
		{"0.48 — 지금 값", 48, 100, 2, false},
		{"0.49 — 상한 정각", 49, 100, 2, false},
		{"0.50 — 상한 초과", 50, 100, 2, true},
		{"0.51", 51, 100, 2, true},
		{"0.90", 90, 100, 2, true},
		{"0.50 은 정밀도가 커져도 거부된다", 50, 100, 6, true},
		{"precision 1 에서 0.48 은 표현 불가", 48, 100, 1, true},
	} {
		tk, err := limitTickFor(c.num, c.den, c.precision)
		if c.wantErr {
			if err == nil {
				t.Errorf("%s: 틱 %d 를 내줬다 — 거부해야 한다", c.name, tk.V)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if want := c.num * (order.Full(c.precision) / c.den); tk.V != want {
			t.Errorf("%s: 틱 %d, 기대 %d", c.name, tk.V, want)
		}
	}
}
