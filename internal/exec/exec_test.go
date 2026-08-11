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

	res := CreateResult{ID: fmt.Sprintf("ord-%d", n)}
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
}

func (f *fakeFills) Poll(_ context.Context, _ live.Round) ([]ledger.Fill, error) {
	f.mu.Lock()
	n := f.polls
	f.polls++
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
		// equity 100 → cap 4.55. 이 크기면 우리 주문이 0.45 층에서 10주라
		// 군중 100주에 묻힌다 — 합성 호가창이 우리 주문을 따로 싣지 않아도
		// 제외 뺄셈이 군중 층을 통째로 지우지 않는다. (우리 물량이 층을
		// 지배하는 경우는 TestLoopExcludesOurOwnOrderFromBestBid 가 따로 만든다.)
		equity: risk.Equity{AvailableUSDT: 100},
	}
	h.frozen = live.Frozen{
		T:          h.round.StartMS(),
		PUp:        0.53,
		Confidence: 0.06,
		Direction:  ledger.OutcomeUp,
		Eligible:   true,
	}
	h.orders.step = func() int { return h.steps }
	h.runner = &Runner{
		Book:       h.book,
		Orders:     h.orders,
		Fills:      h.fills,
		Ledger:     h.led,
		Cooldown:   500 * time.Millisecond,
		StaleAfter: 3 * time.Second,
		Poll:       100 * time.Millisecond,
		Clock:      h.clk.now,
		MonoClock:  h.clk.monoNs,
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
// 우리 주문을 하네스가 직접 얹는 것이 요점이다. 테스트가 손으로 "우리 주문이
// 여기 있다"를 적으면, 취소된 뒤에도 그 층이 남아 제외를 빠뜨린 구현이
// 우연히 통과한다(변이 M01 이 실제로 그 틈으로 살아남았다).
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
	factor := math.Pow(10, float64(h.round.Precision))
	for tick, sh := range h.orders.bookLevels() {
		bids[float64(tick)/factor] += sh
	}
	raw := fmt.Sprintf(`{"bids":[%s],"asks":[%s],"updateTimestampMs":`,
		levelsJSON(bids), levelsJSON(h.crowdAsks))
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
// (1) exclude — 우리 주문을 최우선 호가에서 뺀다
// ---------------------------------------------------------------------------

// 우리 주문이 군중보다 위에 홀로 남으면 **내려와야 한다.** exclude 를 빠뜨리면
// BestBid 가 우리 자신이라 target == 우리 틱 이 되고, Decide 는 "동일가 유지"로
// 영원히 그 자리에 머문다 — 자기 호가를 쫓는 순환이다. quote 테스트로는 절대
// 잡히지 않는다(quote 는 이미 제외된 값을 받는다).
func TestLoopExcludesOurOwnOrderFromBestBid(t *testing.T) {
	h := newHarness(t)
	// cap 4.55, 가격 0.47 → 우리 주문은 9주(4.23). 군중이 그 층에서 빠지면
	// 그 층에 남는 것은 우리 9주뿐이다.
	const ourShares = 9
	// 0바퀴: 군중이 0.47 에 있다 → 우리도 0.47 에 건다.
	h.setCrowd(map[float64]float64{0.47: 30}, map[float64]float64{0.60: 100})
	// 1바퀴: 군중이 0.47 에서 통째로 빠진다. 그 층에 남는 것은 **우리 주문뿐**
	// 이고(하네스가 얹는다) 군중의 최우선은 0.45 다.
	//
	// 우리 주문을 빼지 않는 구현은 그 층을 군중으로 읽어 "동일가 유지"로 굳고,
	// 회차가 끝날 때까지 0.47 에 홀로 남는다.
	h.onStep = func(step int) {
		if step == 1 {
			h.setCrowd(map[float64]float64{0.45: 100}, map[float64]float64{0.60: 100})
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	ticks := h.orders.createdTicks()
	if len(ticks) < 2 {
		t.Fatalf("주문이 %d건이다 (틱 %v) — 우리 호가를 제외하지 않아 내려오지 못했다", len(ticks), ticks)
	}
	if ticks[0] != 47 {
		t.Errorf("첫 주문 틱 %d, 기대 47", ticks[0])
	}
	if ticks[1] != 45 {
		t.Errorf("둘째 주문 틱 %d, 기대 45 — 우리 20주를 뺀 군중 최우선은 0.45 다", ticks[1])
	}
}

// exclude 수량은 Qty 를 거쳐야 한다. 자연 단위를 그대로 넣으면 20주가 0.00002주로
// 취급되어 우리 층이 그대로 남는다.
func TestExcludeUsesFixedPointQuantity(t *testing.T) {
	b := ws.NewBook(testPrecision)
	f := ws.Frame{RecvMonoNs: 1}
	f.Msg.Data = []byte(`{"bids":[[0.45,100],[0.47,20]],"asks":[],"updateTimestampMs":1}`)
	if _, err := b.Apply(f); err != nil {
		t.Fatal(err)
	}
	ex, exOK := excludeOurs([]exposedOrder{{tick: 47, shares: 20}})
	if !exOK {
		t.Fatal("20주를 제외 맵으로 만들지 못했다")
	}
	got, ok := b.BestBid(ex)
	if !ok || got != 45 {
		t.Errorf("BestBid = %d,%v — 기대 45,true (우리 20주를 뺀 뒤)", got, ok)
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
	h.orders.createFn = func(n int, _ Request) (CreateResult, error) {
		if n == 0 {
			return CreateResult{}, unknown
		}
		return CreateResult{ID: fmt.Sprintf("ord-%d", n)}, nil
	}
	err := h.run()
	if n := h.orders.createdCount(); n != 1 {
		t.Errorf("주문 생성 %d회 — 결과 불명 주문을 다시 보내면 둘이 들어간다", n)
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
	h.orders.createFn = func(n int, _ Request) (CreateResult, error) {
		if n == 0 {
			return CreateResult{ID: "ord-0"}, &fakeOrderError{safe: false, msg: "결과 불명"}
		}
		return CreateResult{ID: fmt.Sprintf("ord-%d", n)}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.removeCount(); n == 0 {
		t.Fatal("식별자가 있는데 취소를 시도하지 않았다")
	}
	h.orders.mu.Lock()
	defer h.orders.mu.Unlock()
	if len(h.orders.creates) < 2 {
		t.Fatalf("주문 생성 %d회 — 취소가 확인된 뒤에는 다시 걸 수 있어야 한다", len(h.orders.creates))
	}
	// **다음 주문은 취소가 확인된 뒤에만 나간다.** 확인 전에 다시 걸면 그
	// 순간 노출이 두 배다(불명 주문이 살아 있을 수 있다).
	if h.orders.createSteps[1] <= h.orders.removeSteps[0] {
		t.Errorf("둘째 주문이 %d스텝, 첫 취소가 %d스텝 — 취소 확인 전에 다시 걸었다",
			h.orders.createSteps[1], h.orders.removeSteps[0])
	}
}

// 거래소가 명시적으로 거부한 주문은 존재하지 않는다 — 다시 시도해도 안전하다.
func TestRejectedCreateMayBeRetried(t *testing.T) {
	h := newHarness(t)
	h.orders.createFn = func(n int, _ Request) (CreateResult, error) {
		if n == 0 {
			return CreateResult{}, &fakeOrderError{safe: true, msg: "거래소 거부"}
		}
		return CreateResult{ID: fmt.Sprintf("ord-%d", n)}, nil
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.createdCount(); n < 2 {
		t.Errorf("주문 생성 %d회 — 거부된 주문은 존재하지 않으므로 다시 낼 수 있어야 한다", n)
	}
}

// 분류 불가 에러는 "모른다"로 다룬다 — 이중 주문을 막는 쪽.
func TestUnclassifiedCreateErrorIsTreatedAsUnknown(t *testing.T) {
	h := newHarness(t)
	h.orders.createFn = func(n int, _ Request) (CreateResult, error) {
		if n == 0 {
			return CreateResult{}, errors.New("무엇인지 모르는 실패")
		}
		return CreateResult{ID: fmt.Sprintf("ord-%d", n)}, nil
	}
	if err := h.run(); err == nil {
		t.Fatal("분류하지 못한 실패를 안전한 것으로 다뤘다")
	}
	if n := h.orders.createdCount(); n != 1 {
		t.Errorf("주문 생성 %d회 — 분류하지 못한 실패는 '보냈을 수 있다'로 다뤄야 한다", n)
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
	h.onStep = func(step int) {
		if step == 1 {
			h.setCrowd(map[float64]float64{0.40: 100}, map[float64]float64{0.60: 100})
		}
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
	h.onStep = func(step int) {
		if step == 1 {
			h.setCrowd(map[float64]float64{0.40: 100}, map[float64]float64{0.60: 100})
		}
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
	h.onStep = func(step int) {
		if step == 1 {
			h.setCrowd(map[float64]float64{0.40: 100}, map[float64]float64{0.60: 100})
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.removeCount(); n == 0 {
		t.Error("잠금 시각을 모른다고 취소를 아예 시도하지 않았다 — 주문이 회차 끝까지 남는다")
	}
}

// ---------------------------------------------------------------------------
// (7) Placed 는 Clock 에서 온다 — 파일에서 되읽지 않는다
// ---------------------------------------------------------------------------

// 쿨다운은 Placed 로부터 잰다. Placed 가 Clock 에서 오지 않으면 이 테스트가 깨진다.
func TestRepriceWaitsForCooldownMeasuredFromClock(t *testing.T) {
	h := newHarness(t)
	h.runner.Cooldown = 500 * time.Millisecond // 폴링 100ms → 5스텝
	h.onStep = func(step int) {
		if step == 1 {
			h.setCrowd(map[float64]float64{0.40: 100}, map[float64]float64{0.60: 100})
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	// step1 에서 군중이 내려갔지만 쿨다운 500ms 가 지나야 취소가 나간다.
	// 첫 주문은 step0(루프 첫 바퀴), 재호가 가능 시점은 그로부터 500ms 뒤.
	if n := h.orders.removeCount(); n == 0 {
		t.Fatal("쿨다운이 끝났는데 재호가하지 않았다")
	}
	if h.firstRemoveStep() < 5 {
		t.Errorf("첫 취소가 %d스텝(=%dms)에 나갔다 — 쿨다운 500ms 를 지키지 않았다",
			h.firstRemoveStep(), h.firstRemoveStep()*100)
	}
}

// Placed 를 파일에서 되읽지 않는다 — 직렬화를 거치면 단조 성분이 사라진다.
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
// 계획서 Step 1 의 다섯 — 관통 방지·동일가 무동작·노출 상한·stale·회차 종료
// ---------------------------------------------------------------------------

// 같은 가격에서는 아무 요청도 나가지 않는다. 큐 위치가 체결을 지배한다.
func TestLoopDoesNotReorderAtSamePrice(t *testing.T) {
	h := newHarness(t)
	// 호가창은 내내 그대로. 우리 주문이 얹히지만 exclude 로 빠지므로 target 불변.
	h.onStep = func(step int) {
		if step >= 1 {
			h.setCrowd(map[float64]float64{0.45: 100}, map[float64]float64{0.60: 100})
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.createdCount(); n != 1 {
		t.Errorf("주문 %d건 — 같은 가격에 재주문하면 큐 맨 뒤로 밀린다", n)
	}
	if n := h.orders.removeCount(); n != 1 { // 회차 종료 취소 1회만
		t.Errorf("취소 %d회 — 회차 종료 취소 1회만 나가야 한다", n)
	}
}

// 관통 방지: 매도호가 아래 한 틱으로 내려가고 절대 그 위로 걸지 않는다.
func TestLoopNeverCrossesTheAsk(t *testing.T) {
	h := newHarness(t)
	h.setCrowd(map[float64]float64{0.48: 100}, map[float64]float64{0.46: 100})
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	ticks := h.orders.createdTicks()
	if len(ticks) == 0 {
		t.Fatal("주문이 나가지 않았다")
	}
	for i, tk := range ticks {
		if tk >= 46 {
			t.Errorf("%d번째 주문 틱 %d — 매도호가 46 을 관통했다(테이커 전락)", i, tk)
		}
	}
	if ticks[0] != 45 {
		t.Errorf("첫 주문 틱 %d, 기대 45 (ask 46 − 1틱)", ticks[0])
	}
}

// 노출이 cap 에 닿으면 더 주문하지 않는다.
func TestLoopStopsAtCap(t *testing.T) {
	h := newHarness(t)
	h.equity = risk.Equity{AvailableUSDT: 100} // cap 4.55
	// 체결이 4.5 쌓이면 잔여 0.05 < $1 이라 더 못 낸다.
	h.fills.pollFn = func(n int) ([]ledger.Fill, error) {
		if n != 1 {
			return nil, nil
		}
		return []ledger.Fill{{
			RoundStart: h.round.StartsAt.Unix(), MarketID: h.round.MarketID,
			Outcome: ledger.OutcomeUp, Shares: 10, PriceUSD: 0.45, At: h.clk.now(),
		}}, nil
	}
	// 매 스텝 군중이 움직여 재호가를 유도한다.
	h.runner.Cooldown = 0
	h.onStep = func(step int) {
		if step%2 == 0 {
			h.setCrowd(map[float64]float64{0.45: 100}, map[float64]float64{0.60: 100})
		} else {
			h.setCrowd(map[float64]float64{0.44: 100}, map[float64]float64{0.60: 100})
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.createdCount(); n != 1 {
		t.Errorf("주문 %d건 — 체결 4.5 + cap 4.55 면 잔여가 $1 미만이라 더 못 낸다", n)
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

// 취소 확인 전 주문도 노출로 센다. 빼면 취소·재주문 경합 순간에 노출이 두 배가 된다.
func TestPendingCancelCountsAsExposure(t *testing.T) {
	h := newHarness(t)
	h.equity = risk.Equity{AvailableUSDT: 100} // cap 4.55
	h.runner.Cooldown = 0
	h.runner.RejectBackoff = 10 * time.Second // 한 번 거부되면 이 회차 안에 재시도 없음
	h.orders.removeFn = func(n int, ids []string) (RemoveResult, error) {
		return RemoveResult{Rejected: ids}, nil
	}
	h.onStep = func(step int) {
		if step == 1 {
			h.setCrowd(map[float64]float64{0.40: 100}, map[float64]float64{0.60: 100})
		}
	}
	if err := h.run(); err == nil {
		t.Fatal("취소를 확인하지 못한 채 회차가 끝났는데 에러가 아니다")
	}
	if n := h.orders.createdCount(); n != 1 {
		t.Errorf("주문 %d건 — 취소 미확인분을 노출에서 빼면 안 된다", n)
	}
}

// removalLockedUntil 안에서는 취소를 시도하지 않는다. 거부당하고 요청만 낭비한다.
func TestLoopRespectsRemovalLock(t *testing.T) {
	h := newHarness(t)
	h.runner.Cooldown = 0
	lockFor := 600 * time.Millisecond
	h.orders.createFn = func(n int, _ Request) (CreateResult, error) {
		return CreateResult{
			ID:          fmt.Sprintf("ord-%d", n),
			LockedUntil: h.clk.now().Add(lockFor),
		}, nil
	}
	h.onStep = func(step int) {
		if step == 1 {
			h.setCrowd(map[float64]float64{0.40: 100}, map[float64]float64{0.60: 100})
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.removeCount(); n == 0 {
		t.Fatal("취소가 한 번도 나가지 않았다")
	}
	// 첫 주문은 루프 첫 바퀴(0스텝)에 나갔다. 잠금은 600ms → 6스텝.
	if s := h.firstRemoveStep(); s < 6 {
		t.Errorf("첫 취소가 %d스텝(=%dms)에 나갔다 — 잠금 창(600ms) 안에서 취소를 시도했다", s, s*100)
	}
}

// stale 이면 신규를 멈추고 기존을 취소한다.
func TestLoopCancelsOnStale(t *testing.T) {
	h := newHarness(t)
	h.runner.StaleAfter = 300 * time.Millisecond // 3스텝이면 stale
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if h.orders.removeCount() == 0 {
		t.Fatal("stale 인데 기존 주문을 취소하지 않았다")
	}
	// 회차 종료 취소(10스텝)가 아니라 stale 취소여야 한다.
	if s := h.firstRemoveStep(); s > 5 {
		t.Errorf("첫 취소가 %d스텝 — stale(3스텝) 직후에 취소해야 한다", s)
	}
	// 신규 차단은 TestStaleBlocksTheFirstOrderToo 가 따로 잡는다. 여기서는
	// 셀 수 없다 — 우리 주문이 취소되면 호가창이 갱신되어 다시 신선해지고,
	// 그 뒤에 새로 거는 것은 옳은 동작이다.
}

func TestStaleBlocksTheFirstOrderToo(t *testing.T) {
	h := newHarness(t)
	h.runner.StaleAfter = time.Nanosecond
	h.clk.advance(time.Second) // 이미 오래된 호가창
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if n := h.orders.createdCount(); n != 0 {
		t.Errorf("주문 %d건 — 오래된 호가로 신규 주문을 내면 안 된다", n)
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
		t.Errorf("마지막 취소 = %v, 기대 [ord-0]", last)
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

// **ws.Qty 의 int64 오버플로.** risk.Shares 는 2^53(≈9.0e15) 미만이면 통과
// 시키는데 ws.QtyDecimals 가 6 이라 9.22e12 주만 넘어도 곱이 int64 를 넘는다.
// 넘치면 exclude 값이 음수가 되고 `qty − exclude <= 0` 검사가 뒤집혀 **우리
// 주문이 호가창에서 빠지지 않는다** — 에러도 로그도 없이 자기 호가를 쫓는다.
func TestQtyOverflowWouldUnexcludeOurOrder(t *testing.T) {
	// 오버플로가 실제로 부호를 뒤집는다는 것부터 확인한다.
	huge := maxExcludableShares * 2
	if got := ws.Qty(huge); got >= 0 {
		t.Fatalf("Qty(%v) = %d — 이 테스트의 전제(오버플로가 음수를 만든다)가 깨졌다", huge, got)
	}
	if _, ok := excludeOurs([]exposedOrder{{tick: 47, shares: huge}}); ok {
		t.Error("표현할 수 없는 수량을 제외 맵에 담았다")
	}
	// 건별로는 통과하지만 합이 넘치는 경우도 막아야 한다.
	half := maxExcludableShares * 0.6
	if _, ok := excludeOurs([]exposedOrder{{tick: 47, shares: half}, {tick: 47, shares: half}}); ok {
		t.Error("합이 넘치는데 제외 맵을 만들었다")
	}
	// 경계 바로 아래는 정상이어야 한다 — 가드가 정상 주문을 막으면 안 된다.
	if _, ok := excludeOurs([]exposedOrder{{tick: 47, shares: 1_000_000}}); !ok {
		t.Error("100만주를 표현하지 못한다고 판정했다")
	}
}

// 그런 크기의 주문은 애초에 내지 않는다. risk.Shares 가 통과시키는 구간
// (2^53 미만)과 ws.Qty 가 표현하는 구간(9.2e12 이하)이 겹치지 않는 자리다.
func TestRefusesOrdersTooLargeToExcludeFromTheBook(t *testing.T) {
	h := newHarness(t)
	h.round.Precision = 6
	h.book = ws.NewBook(6)
	h.runner.Book = h.book
	h.ts = 0
	// 틱 6 = 0.000006. cap 4.55e10 → floor(4.55e10/6e-6) ≈ 7.58e15 주.
	// 2^53(9.0e15) 미만이라 risk.Shares 는 통과시키고, ws.Qty 는 넘친다.
	h.setCrowd(map[float64]float64{0.000006: 1e6}, nil)
	h.equity = risk.Equity{AvailableUSDT: 1e12}

	// 전제 확인: risk 는 이 크기를 허용한다.
	n := risk.Shares(risk.Cap(h.equity), 0.000006)
	if n <= maxExcludableShares {
		t.Fatalf("전제가 깨졌다: risk.Shares = %v 가 표현 상한 %v 이하다", n, maxExcludableShares)
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if c := h.orders.createdCount(); c != 0 {
		t.Errorf("주문 %d건 — 호가창에서 뺄 수 없는 크기의 주문을 냈다", c)
	}
}

// 체결 명목이 NaN 이면(망가진 피드) 더 걸지 않는다. risk 가 유한하지 않은
// 노출에서 잔여 0 을 돌려주는 것에 기대는 연결이라 여기서 고정한다.
func TestNaNFillStopsFurtherOrders(t *testing.T) {
	h := newHarness(t)
	h.runner.Cooldown = 0
	h.fills.pollFn = func(n int) ([]ledger.Fill, error) {
		if n != 1 {
			return nil, nil
		}
		return []ledger.Fill{{
			RoundStart: h.round.StartsAt.Unix(), MarketID: h.round.MarketID,
			Outcome: ledger.OutcomeUp, Shares: math.NaN(), PriceUSD: 0.45, At: h.clk.now(),
		}}, nil
	}
	// 매 스텝 군중이 움직여 재호가를 유도한다.
	h.onStep = func(step int) {
		if step%2 == 0 {
			h.setCrowd(map[float64]float64{0.45: 100}, map[float64]float64{0.60: 100})
		} else {
			h.setCrowd(map[float64]float64{0.44: 100}, map[float64]float64{0.60: 100})
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("NaN 체결이 회차를 죽였다: %v", err)
	}
	if c := h.orders.createdCount(); c != 1 {
		t.Errorf("주문 %d건 — NaN 노출에서는 더 걸면 안 된다", c)
	}
	// 원장에는 들어가지 않는다(ErrInvalidRecord). 파일이 오염되면 집계하는
	// 쪽이 오염 사실조차 모른다.
	if h.led.count() != 1 {
		t.Errorf("원장 기록 시도 %d회", h.led.count())
	}
}

// 같은 식별자가 두 번 오면 취소 확인 **한 번**이 두 주문의 명목을 함께 빼 준다.
// 그러면 한도가 조용히 늘어난다.
//
// **지금의 루프는 이 상태에 닿지 못한다** — risk.Shares 가 잔여를 거의 다 쓰고
// 남는 잔여는 언제나 가격보다 작아(≤$1) 두 번째 주문이 성립하지 않기 때문이다.
// 그래도 가드와 이 테스트를 두는 이유: 사이저가 부분 주문을 허용하도록 바뀌는
// 순간(예: 큐 위치를 나눠 걸기) 곧바로 열리는 구멍이고, 열려도 아무 신호가
// 없다. 그래서 루프가 아니라 transmit 을 직접 부른다.
func TestDuplicateOrderIDIsNotTracked(t *testing.T) {
	h := newHarness(t)
	st := &roundState{live: &openOrder{id: "same", tick: 45, shares: 10, notional: 4.5}}
	h.orders.createFn = func(int, Request) (CreateResult, error) {
		return CreateResult{ID: "same"}, nil // 거래소가 같은 ID 를 반복한다
	}
	req := Request{
		Round: h.round, Outcome: ledger.OutcomeUp, TokenID: "111",
		Tick: order.NewTick(44, testPrecision), Shares: 10,
	}
	if err := h.runner.transmit(context.Background(), st, req, h.clk.now()); err != nil {
		t.Fatalf("transmit: %v", err)
	}
	if st.live.id == "same" && st.live.tick == 44 {
		t.Error("중복 식별자로 살아 있는 주문을 덮어썼다 — 원래 주문을 잊는다")
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
	h.frozen.PUp = 0.47 // 하락 확률이 높은데
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
	// 취소 요청은 받되 사라졌다고 확인해 주지 않는다.
	h.orders.removeFn = func(_ int, ids []string) (RemoveResult, error) {
		return RemoveResult{Unaccounted: append([]string(nil), ids...)}, nil
	}
	// 군중이 움직여 재호가(=취소)를 유발한다.
	h.onStep = func(step int) {
		if step == 3 {
			h.setCrowd(map[float64]float64{0.46: 100}, map[float64]float64{0.60: 100})
		}
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

// 첫 주문을 걸기만 해도 LastActionAt 이 찍혀야 한다. 재호가에서만 찍으면,
// 한 번 걸고 군중이 안 움직이는 회차가 통째로 "아무 행동 없음" 으로 보인다.
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
	if first.Reprices != 0 {
		t.Fatalf("첫 주문인데 재호가가 %d 다 — 이 테스트가 재호가 경로를 보고 있다", first.Reprices)
	}
	if first.LastActionAt.IsZero() {
		t.Error("주문을 걸었는데 LastActionAt 이 제로다 — 재호가에서만 찍고 있다")
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

// 재호가 횟수와 마지막 행동 시각이 실제로 움직여야 한다. 이 값이 정체하면
// 프로세스는 멀쩡한데 호가창에 아무 일도 하지 않는 상태이고, 감시가 그것을
// 보는 유일한 근거다.
func TestObserveTracksRepricesAndLastAction(t *testing.T) {
	h := newHarness(t)
	var last Observation
	h.runner.Observe = func(o Observation) { last = o }

	// 군중이 위로 움직이면 우리도 따라 올라간다 = 재호가.
	h.onStep = func(step int) {
		switch step {
		case 3:
			h.setCrowd(map[float64]float64{0.46: 100}, map[float64]float64{0.60: 100})
		case 6:
			h.setCrowd(map[float64]float64{0.47: 100}, map[float64]float64{0.60: 100})
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if last.Reprices == 0 {
		t.Error("군중이 두 번 움직였는데 재호가가 0 이다")
	}
	if last.LastActionAt.IsZero() {
		t.Error("주문을 걸었는데 LastActionAt 이 제로다")
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
