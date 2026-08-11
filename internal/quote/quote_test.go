package quote

import (
	"testing"
	"time"
)

// mustPanic 은 f 가 패닉하는지 본다. precision 가드는 "조용히 틀린 가격" 대신
// 시끄럽게 죽는 것이 목적이므로, 가드가 살아 있다는 것 자체를 시험한다.
func mustPanic(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s: 패닉하지 않았다", name)
		}
	}()
	f()
}

func TestCeilingFromPrecision(t *testing.T) {
	// 0.5 미만의 최대 틱. 정밀도 2면 0.49 = 49틱, 3이면 0.499 = 499틱.
	cases := []struct {
		prec int
		want int64
	}{
		{2, 49}, {3, 499}, {1, 4},
	}
	for _, c := range cases {
		if got := Ceiling(c.prec); got != c.want {
			t.Errorf("Ceiling(%d) = %d, 기대 %d", c.prec, got, c.want)
		}
	}
}

// 가드의 양쪽 경계. 1 과 18 은 통과해야 하고 0 과 19 는 죽어야 한다.
// 손계산: Ceiling(18) = 10^18/2 − 1 = 500000000000000000 − 1.
func TestCeilingPrecisionBounds(t *testing.T) {
	if got := Ceiling(18); got != 499999999999999999 {
		t.Errorf("Ceiling(18) = %d, 기대 499999999999999999", got)
	}
	if got := Ceiling(4); got != 4999 {
		t.Errorf("Ceiling(4) = %d, 기대 4999", got)
	}
	for _, p := range []int{0, -1, 19, 20} {
		p := p
		mustPanic(t, "Ceiling", func() { Ceiling(p) })
	}
}

func TestTargetFollowsCrowdBelowHalf(t *testing.T) {
	// 남의 최우선 매수호가가 0.47 이면 거기에 붙는다.
	tick, ok := Target(Book{BestBid: 47, HasBid: true, Precision: 2})
	if !ok || tick != 47 {
		t.Errorf("target = %d,%v, 기대 47,true", tick, ok)
	}
}

func TestTargetCapsAtCeilingWhenCrowdIsAtOrAboveHalf(t *testing.T) {
	// 남의 매수호가가 0.50 이상이면 우리는 못 따라간다 — ceiling 으로 간다.
	for _, bid := range []int64{50, 60, 99} {
		tick, ok := Target(Book{BestBid: bid, HasBid: true, Precision: 2})
		if !ok || tick != 49 {
			t.Errorf("bid=%d → target = %d,%v, 기대 49,true", bid, tick, ok)
		}
	}
}

// 상한 경계. 49 는 0.5 미만이므로 그대로 따라가고, 50 은 0.5 이므로 막힌다.
// 두 값이 같은 답(49)을 내지만 경로가 다르다 — 부등호가 <= 로 뒤집히면
// bid=49 가 "상한으로 강등"되어도 답이 49 라 티가 안 난다. 정밀도 3 으로도
// 같은 경계를 둔다: 손계산으로 half=500, ceiling=499.
func TestTargetHalfBoundary(t *testing.T) {
	cases := []struct {
		prec int
		bid  int64
		want int64
	}{
		{2, 48, 48},
		{2, 49, 49},
		{2, 50, 49},
		{3, 470, 470},
		{3, 499, 499},
		{3, 500, 499},
		{1, 4, 4},
		{1, 5, 4},
	}
	for _, c := range cases {
		tick, ok := Target(Book{BestBid: c.bid, HasBid: true, Precision: c.prec})
		if !ok || tick != c.want {
			t.Errorf("prec=%d bid=%d → target = %d,%v, 기대 %d,true", c.prec, c.bid, tick, ok, c.want)
		}
	}
}

func TestTargetUsesCeilingWhenNoBid(t *testing.T) {
	tick, ok := Target(Book{HasBid: false, Precision: 2})
	if !ok || tick != 49 {
		t.Errorf("target = %d,%v, 기대 49,true", tick, ok)
	}
}

// HasBid=false 인데 BestBid 에 쓰레기가 실려 있어도 무시해야 한다.
// ws.Book.BestBid 는 ok=false 일 때 tick 이 의미 없다고 문서화돼 있다.
func TestTargetIgnoresBidWhenHasBidFalse(t *testing.T) {
	tick, ok := Target(Book{BestBid: 47, HasBid: false, Precision: 2})
	if !ok || tick != 49 {
		t.Errorf("target = %d,%v, 기대 49,true — HasBid=false 면 BestBid 를 읽으면 안 된다", tick, ok)
	}
}

// 관통 방지 — 이것이 이 패키지의 존재 이유다.
func TestTargetNeverCrossesAsk(t *testing.T) {
	// 매도호가가 0.45 인데 ceiling 이 0.49 다. 그대로 가면 테이커가 된다.
	tick, ok := Target(Book{HasBid: false, BestAsk: 45, HasAsk: true, Precision: 2})
	if !ok || tick != 44 {
		t.Errorf("target = %d,%v, 기대 44,true — ask 아래 한 틱", tick, ok)
	}
	// 군중 매수호가가 매도호가와 같은 자리인 경우
	tick, ok = Target(Book{BestBid: 47, HasBid: true, BestAsk: 47, HasAsk: true, Precision: 2})
	if !ok || tick != 46 {
		t.Errorf("target = %d,%v, 기대 46,true", tick, ok)
	}
}

// 관통 방지의 부등호 경계를 양쪽에서 집는다.
// ask 가 목표보다 한 틱 위면 건드리지 않고, 같으면 한 틱 내린다.
// 손계산: bid=47 → 목표 47. ask=48 이면 47 유지, ask=47 이면 46, ask=46 이면 45.
func TestTargetAskBoundary(t *testing.T) {
	cases := []struct {
		ask  int64
		want int64
	}{
		{49, 47},
		{48, 47},
		{47, 46},
		{46, 45},
		{40, 39},
	}
	for _, c := range cases {
		tick, ok := Target(Book{BestBid: 47, HasBid: true, BestAsk: c.ask, HasAsk: true, Precision: 2})
		if !ok || tick != c.want {
			t.Errorf("ask=%d → target = %d,%v, 기대 %d,true", c.ask, tick, ok, c.want)
		}
	}
	// HasAsk=false 면 BestAsk 값은 읽지 않는다.
	tick, ok := Target(Book{BestBid: 47, HasBid: true, BestAsk: 10, HasAsk: false, Precision: 2})
	if !ok || tick != 47 {
		t.Errorf("target = %d,%v, 기대 47,true — HasAsk=false 면 BestAsk 를 읽으면 안 된다", tick, ok)
	}
}

func TestTargetRefusesWhenAskIsAtBottom(t *testing.T) {
	// ask 가 1틱이면 그 아래는 0 — 유효한 가격이 없다.
	if _, ok := Target(Book{BestAsk: 1, HasAsk: true, Precision: 2}); ok {
		t.Error("ask=1 에서 주문 가능으로 판정했다")
	}
}

// 0 틱 경계. ask=2 면 1틱(0.01)이 남으므로 주문 가능, ask=1 이면 0 이라 불가.
// bid 가 0 틱으로 들어오는 경우도 같은 자리에서 막혀야 한다.
func TestTargetZeroTickBoundary(t *testing.T) {
	tick, ok := Target(Book{BestAsk: 2, HasAsk: true, Precision: 2})
	if !ok || tick != 1 {
		t.Errorf("ask=2 → target = %d,%v, 기대 1,true", tick, ok)
	}
	if _, ok := Target(Book{BestBid: 0, HasBid: true, Precision: 2}); ok {
		t.Error("bid=0 에서 주문 가능으로 판정했다 — 가격 0.00 은 주문이 아니다")
	}
	// 정밀도 1 에서도 같은 경계: ceiling 4, ask=1 이면 0 이라 불가.
	if _, ok := Target(Book{BestAsk: 1, HasAsk: true, Precision: 1}); ok {
		t.Error("prec=1 ask=1 에서 주문 가능으로 판정했다")
	}
	if tick, ok := Target(Book{BestAsk: 2, HasAsk: true, Precision: 1}); !ok || tick != 1 {
		t.Errorf("prec=1 ask=2 → target = %d,%v, 기대 1,true", tick, ok)
	}
}

// precision 가드가 Ceiling 뿐 아니라 Target 으로 들어오는 경로에도 걸려야 한다.
// 군중 추종 경로는 Ceiling 을 안 거치고 BestBid 를 그대로 돌려줄 수 있는데,
// precision 20 에서는 10^20 이 int64 를 넘어 half 가 엉뚱한 양수로 감싸므로
// 가드 없이는 이 경로가 조용히 통과한다.
func TestTargetGuardsPrecisionOnEveryPath(t *testing.T) {
	for _, p := range []int{0, -1, 19, 20} {
		p := p
		mustPanic(t, "Target(무호가)", func() { Target(Book{Precision: p}) })
		mustPanic(t, "Target(군중추종)", func() { Target(Book{BestBid: 47, HasBid: true, Precision: p}) })
		mustPanic(t, "Target(관통방지)", func() {
			Target(Book{BestBid: 47, HasBid: true, BestAsk: 47, HasAsk: true, Precision: p})
		})
	}
}

func TestDecideSamePriceDoesNothing(t *testing.T) {
	b := Book{BestBid: 47, HasBid: true, Precision: 2}
	now := time.Unix(1000, 0)
	open := Open{Tick: 47, Live: true, Placed: now.Add(-time.Hour)}
	d := Decide(b, open, now, 500*time.Millisecond, false, nil)
	if d.Action != DoNothing {
		t.Errorf("action = %v, 기대 DoNothing — 같은 가격에 재주문하면 큐 맨 뒤로 밀린다", d.Action)
	}
}

func TestDecideCooldownDefersReprice(t *testing.T) {
	b := Book{BestBid: 46, HasBid: true, Precision: 2}
	now := time.Unix(1000, 0)
	// 400ms 전에 냈다 — 쿨다운 500ms 미만이므로 미룬다.
	open := Open{Tick: 47, Live: true, Placed: now.Add(-400 * time.Millisecond)}
	if d := Decide(b, open, now, 500*time.Millisecond, false, nil); d.Action != DoNothing {
		t.Errorf("action = %v, 기대 DoNothing (쿨다운)", d.Action)
	}
	// 정확히 500ms 는 허용한다(경계 포함).
	open.Placed = now.Add(-500 * time.Millisecond)
	if d := Decide(b, open, now, 500*time.Millisecond, false, nil); d.Action != Reprice {
		t.Errorf("action = %v, 기대 Reprice (경계 500ms 는 허용)", d.Action)
	}
}

// 쿨다운 경계를 1ns 단위로 집는다. 499.999999ms 는 막고 500ms 는 통과한다.
// cooldown=0 이면 방금 낸 주문도 즉시 옮길 수 있어야 한다(0 < 0 은 거짓).
func TestDecideCooldownBoundaryToTheNanosecond(t *testing.T) {
	b := Book{BestBid: 46, HasBid: true, Precision: 2}
	now := time.Unix(1000, 0)
	cd := 500 * time.Millisecond

	open := Open{Tick: 47, Live: true, Placed: now.Add(-cd + time.Nanosecond)}
	if d := Decide(b, open, now, cd, false, nil); d.Action != DoNothing {
		t.Errorf("경과 499.999999ms: action = %v, 기대 DoNothing", d.Action)
	}
	open.Placed = now.Add(-cd - time.Nanosecond)
	if d := Decide(b, open, now, cd, false, nil); d.Action != Reprice {
		t.Errorf("경과 500.000001ms: action = %v, 기대 Reprice", d.Action)
	}
	// cooldown 0: 같은 순간에 냈어도 옮긴다.
	open.Placed = now
	if d := Decide(b, open, now, 0, false, nil); d.Action != Reprice {
		t.Errorf("cooldown=0: action = %v, 기대 Reprice", d.Action)
	}
	// 시계가 뒤로 갔거나 Placed 가 미래인 경우(음수 경과)는 쿨다운 안이다.
	open.Placed = now.Add(time.Second)
	if d := Decide(b, open, now, cd, false, nil); d.Action != DoNothing {
		t.Errorf("Placed 가 미래: action = %v, 기대 DoNothing", d.Action)
	}
}

// 쿨다운은 같은 가격 검사보다 뒤에 온다 — 가격이 같으면 쿨다운과 무관하게
// 아무것도 하지 않고, 두 조건이 다 걸려도 결과는 DoNothing 이다.
func TestDecideSamePriceWinsInsideCooldown(t *testing.T) {
	b := Book{BestBid: 47, HasBid: true, Precision: 2}
	now := time.Unix(1000, 0)
	open := Open{Tick: 47, Live: true, Placed: now}
	if d := Decide(b, open, now, 500*time.Millisecond, false, nil); d.Action != DoNothing {
		t.Errorf("action = %v, 기대 DoNothing", d.Action)
	}
}

func TestDecideStaleCancelsAndBlocksNewOrders(t *testing.T) {
	b := Book{BestBid: 47, HasBid: true, Precision: 2}
	now := time.Unix(1000, 0)
	// 걸린 주문이 있으면 취소한다.
	d := Decide(b, Open{Tick: 47, Live: true, Placed: now.Add(-time.Hour)}, now, 0, true, nil)
	if d.Action != CancelOnly {
		t.Errorf("stale 인데 action = %v, 기대 CancelOnly", d.Action)
	}
	// 없으면 아무것도 하지 않는다 — 오래된 호가로 새 주문을 내면 안 된다.
	d = Decide(b, Open{}, now, 0, true, nil)
	if d.Action != DoNothing {
		t.Errorf("stale + 무주문에서 action = %v, 기대 DoNothing", d.Action)
	}
}

// stale 은 다른 어떤 조건보다 먼저다. 가격이 같아도(=평소라면 DoNothing),
// 쿨다운 안이어도, 목표가 유효해도 걸린 주문은 취소한다.
func TestDecideStaleOutranksEverything(t *testing.T) {
	now := time.Unix(1000, 0)
	live := Open{Tick: 47, Live: true, Placed: now}
	cases := []struct {
		name string
		b    Book
	}{
		{"동일가", Book{BestBid: 47, HasBid: true, Precision: 2}},
		{"목표무효", Book{BestAsk: 1, HasAsk: true, Precision: 2}},
		{"호가없음", Book{Precision: 2}},
	}
	for _, c := range cases {
		if d := Decide(c.b, live, now, time.Hour, true, nil); d.Action != CancelOnly {
			t.Errorf("%s + stale: action = %v, 기대 CancelOnly", c.name, d.Action)
		}
	}
}

func TestDecidePlacesWhenNoOpenOrder(t *testing.T) {
	b := Book{BestBid: 47, HasBid: true, Precision: 2}
	d := Decide(b, Open{}, time.Unix(1000, 0), 500*time.Millisecond, false, nil)
	if d.Action != Place || d.Tick != 47 {
		t.Errorf("action=%v tick=%d, 기대 Place 47", d.Action, d.Tick)
	}
}

// 신규 주문은 쿨다운을 보지 않는다 — 취소한 직후에도 낼 수 있어야 한다.
// Live=false 면 Placed/Tick 은 의미가 없다.
func TestDecidePlaceIgnoresCooldownAndStalePlacedTime(t *testing.T) {
	b := Book{HasBid: false, Precision: 2}
	now := time.Unix(1000, 0)
	d := Decide(b, Open{Tick: 47, Live: false, Placed: now}, now, time.Hour, false, nil)
	if d.Action != Place || d.Tick != 49 {
		t.Errorf("action=%v tick=%d, 기대 Place 49 (상한)", d.Action, d.Tick)
	}
}

// 목표가 없으면(ask 가 바닥) 걸린 주문은 취소하고 신규는 내지 않는다.
func TestDecideNoValidTarget(t *testing.T) {
	b := Book{BestAsk: 1, HasAsk: true, Precision: 2}
	now := time.Unix(1000, 0)
	if d := Decide(b, Open{Tick: 44, Live: true, Placed: now.Add(-time.Hour)}, now, 0, false, nil); d.Action != CancelOnly {
		t.Errorf("목표 무효 + 주문 있음: action = %v, 기대 CancelOnly", d.Action)
	}
	if d := Decide(b, Open{}, now, 0, false, nil); d.Action != DoNothing {
		t.Errorf("목표 무효 + 무주문: action = %v, 기대 DoNothing", d.Action)
	}
}

// 재호가는 새 목표 틱을 들고 나와야 한다. 여기서 옛 틱이 새면 취소만 하고
// 같은 자리에 다시 걸리거나 관통 가격으로 나간다.
func TestDecideRepriceCarriesNewTick(t *testing.T) {
	now := time.Unix(1000, 0)
	open := Open{Tick: 47, Live: true, Placed: now.Add(-time.Hour)}
	// 군중이 46 으로 내려갔다.
	if d := Decide(Book{BestBid: 46, HasBid: true, Precision: 2}, open, now, 500*time.Millisecond, false, nil); d.Action != Reprice || d.Tick != 46 {
		t.Errorf("action=%v tick=%d, 기대 Reprice 46", d.Action, d.Tick)
	}
	// 관통 방지가 걸린 목표로 재호가: bid 47, ask 47 → 46.
	if d := Decide(Book{BestBid: 47, HasBid: true, BestAsk: 47, HasAsk: true, Precision: 2}, open, now, 500*time.Millisecond, false, nil); d.Action != Reprice || d.Tick != 46 {
		t.Errorf("action=%v tick=%d, 기대 Reprice 46 (관통 방지)", d.Action, d.Tick)
	}
}

// 주문을 내지 않는 결정에는 가격이 실리면 안 된다. 0 이 아닌 값이 실리면
// 호출자가 그것을 목표가로 오해할 여지가 생긴다.
func TestDecideCarriesNoTickWhenNotOrdering(t *testing.T) {
	now := time.Unix(1000, 0)
	live := Open{Tick: 47, Live: true, Placed: now.Add(-time.Hour)}
	cases := []struct {
		name  string
		b     Book
		open  Open
		stale bool
		cd    time.Duration
	}{
		{"stale+주문", Book{BestBid: 47, HasBid: true, Precision: 2}, live, true, 0},
		{"stale+무주문", Book{BestBid: 47, HasBid: true, Precision: 2}, Open{}, true, 0},
		{"목표무효+주문", Book{BestAsk: 1, HasAsk: true, Precision: 2}, live, false, 0},
		{"목표무효+무주문", Book{BestAsk: 1, HasAsk: true, Precision: 2}, Open{}, false, 0},
		{"동일가", Book{BestBid: 47, HasBid: true, Precision: 2}, live, false, 0},
		{"쿨다운", Book{BestBid: 46, HasBid: true, Precision: 2}, Open{Tick: 47, Live: true, Placed: now}, false, time.Hour},
	}
	for _, c := range cases {
		d := Decide(c.b, c.open, now, c.cd, c.stale, nil)
		if d.Action == Place || d.Action == Reprice {
			t.Fatalf("%s: 표본이 잘못됐다 — action = %v", c.name, d.Action)
		}
		if d.Tick != 0 {
			t.Errorf("%s: tick = %d, 기대 0", c.name, d.Tick)
		}
	}
}

// Why 는 로그로 나가는 유일한 판단 근거다. 비어 있으면 운영 중에 왜 그렇게
// 결정했는지 사후에 복원할 방법이 없다.
func TestDecideAlwaysExplains(t *testing.T) {
	now := time.Unix(1000, 0)
	live := Open{Tick: 47, Live: true, Placed: now.Add(-time.Hour)}
	cases := []struct {
		name  string
		b     Book
		open  Open
		stale bool
		cd    time.Duration
	}{
		{"stale+주문", Book{BestBid: 47, HasBid: true, Precision: 2}, live, true, 0},
		{"stale+무주문", Book{BestBid: 47, HasBid: true, Precision: 2}, Open{}, true, 0},
		{"목표무효+주문", Book{BestAsk: 1, HasAsk: true, Precision: 2}, live, false, 0},
		{"목표무효+무주문", Book{BestAsk: 1, HasAsk: true, Precision: 2}, Open{}, false, 0},
		{"신규", Book{BestBid: 47, HasBid: true, Precision: 2}, Open{}, false, 0},
		{"동일가", Book{BestBid: 47, HasBid: true, Precision: 2}, live, false, 0},
		{"쿨다운", Book{BestBid: 46, HasBid: true, Precision: 2}, Open{Tick: 47, Live: true, Placed: now}, false, time.Hour},
		{"재호가", Book{BestBid: 46, HasBid: true, Precision: 2}, live, false, 0},
		{"상한", Book{Precision: 2}, Open{}, false, 0},
		{"관통방지", Book{BestBid: 47, HasBid: true, BestAsk: 47, HasAsk: true, Precision: 2}, Open{}, false, 0},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		d := Decide(c.b, c.open, now, c.cd, c.stale, nil)
		if d.Why == "" {
			t.Errorf("%s: Why 가 비었다", c.name)
			continue
		}
		if seen[d.Why] {
			t.Errorf("%s: Why 가 다른 경로와 같다 (%q) — 로그로 구분이 안 된다", c.name, d.Why)
		}
		seen[d.Why] = true
	}
}

// Decide 의 stale 경로는 Target 을 호출하지 않고 빠져나간다. Target 안의
// 가드만 믿으면 precision 이 망가진 회차가 stale 인 동안 조용히 지나가다가
// stale 이 풀리는 임의의 시점에 터진다.
func TestDecideGuardsPrecisionEvenWhenStale(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, p := range []int{0, -1, 19, 20} {
		p := p
		mustPanic(t, "Decide(stale, nil)", func() {
			Decide(Book{BestBid: 47, HasBid: true, Precision: p}, Open{Tick: 47, Live: true}, now, 0, true, nil)
		})
		mustPanic(t, "Decide(정상, nil)", func() {
			Decide(Book{BestBid: 47, HasBid: true, Precision: p}, Open{}, now, 0, false, nil)
		})
	}
}

// Action 은 로그·분기에 그대로 쓰이므로 이름이 서로 달라야 한다.
func TestActionString(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range []Action{DoNothing, Place, Reprice, CancelOnly} {
		s := a.String()
		if s == "" || seen[s] {
			t.Errorf("Action(%d).String() = %q — 비었거나 중복", int(a), s)
		}
		seen[s] = true
	}
	if s := Action(99).String(); s == "" {
		t.Error("범위 밖 Action 의 String 이 비었다")
	}
}

// 이 패키지는 순수해야 한다 — 같은 입력이면 항상 같은 답이고, 인자로 받은
// 값을 고치지 않는다.
func TestDecideIsPureAndDoesNotMutateInput(t *testing.T) {
	b := Book{BestBid: 47, HasBid: true, BestAsk: 48, HasAsk: true, Precision: 2}
	now := time.Unix(1000, 0)
	open := Open{Tick: 46, Live: true, Placed: now.Add(-time.Hour)}
	bCopy, openCopy := b, open

	first := Decide(b, open, now, 500*time.Millisecond, false, nil)
	for i := 0; i < 5; i++ {
		if got := Decide(b, open, now, 500*time.Millisecond, false, nil); got != first {
			t.Fatalf("%d회차 결과가 다르다: %+v vs %+v", i, got, first)
		}
	}
	if b != bCopy || open != openCopy {
		t.Errorf("입력이 변경됐다: %+v/%+v → %+v/%+v", bCopy, openCopy, b, open)
	}
}
