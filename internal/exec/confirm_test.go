package exec

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
)

// 이 파일이 지키는 것: **"취소됐다"는 "안 찼다"가 아니다.**
//
// 2026-08-11 실거래 로그, 회차 btc-updown-5m-1786469700:
//
//	17:36:07.528  취소 요청 준비 (id=2024751893, 틱 49): 재호가 49→47
//	17:36:07.554  취소 확인 (id=2024751893) — 명목 3.9200 를 노출에서 뺀다
//	17:36:07.892  신규 49: → Up 8.0000주 @ 0.49 (명목 3.9200, 잔여 4.2392)
//	17:36:08.999  신규 49: → Up 8.0000주 @ 0.49 (명목 3.9200, 잔여 4.2392)
//	17:36:09.702  체결 8주 @ 0.49                ← 그 "취소된" 주문이었다
//
// **잔여가 네 번 연속 4.2392 로 같다.** 노출이 전혀 세어지지 않았다. 회차
// 명목은 상한 4.4 대비 7.84 로 끝났다.
//
// 거래소가 말한 `removed` 는 "호가창에 없다" 는 뜻이고, 체결된 주문도 호가창에
// 없다. 체결 피드는 2.6~4.6초 늦게 온다.

// resolveHarness 는 회차 루프 없이 상태 기계만 돌린다. 이 파일이 보는 것은
// 노출 회계이지 가격 판단이 아니다.
type resolveHarness struct {
	r  *Runner
	st *roundState
	o  *fakeOrders
}

func newResolveHarness(t *testing.T) *resolveHarness {
	t.Helper()
	o := &fakeOrders{}
	return &resolveHarness{
		r:  &Runner{Orders: o, SettleGrace: 6 * time.Second},
		st: &roundState{},
		o:  o,
	}
}

// cancelled 는 "주문을 걸었고 취소가 확인됐다" 까지를 만든다.
func (h *resolveHarness) cancelled(id, hash string, shares, notional float64, at time.Time) {
	h.st.confirming = append(h.st.confirming, &openOrder{
		id: id, hash: hash, shares: shares, notional: notional,
		goneAt: at, askAt: at,
	})
}

func (h *resolveHarness) exposed() float64 {
	x := h.st.exposure()
	return x.FilledNotional + x.OpenNotional + x.PendingCancel
}

// TestCancelConfirmedDoesNotFreeTheNotionalYet 이 이 파일의 핵심이다.
func TestCancelConfirmedDoesNotFreeTheNotionalYet(t *testing.T) {
	h := newResolveHarness(t)
	now := time.Unix(1786469767, 0)
	h.cancelled("2024751893", "hash-a", 8, 3.92, now)

	if got := h.exposed(); got != 3.92 {
		t.Fatalf("취소 확인 직후 노출 %.4f, want 3.92 — 아직 이 주문이 돈을 썼는지 모른다", got)
	}
}

// 거래소가 "안 찼다" 고 답하면 그때 푼다.
func TestUnfilledOrderReleasesItsNotional(t *testing.T) {
	h := newResolveHarness(t)
	now := time.Unix(1786469767, 0)
	h.cancelled("o1", "hash-a", 8, 3.92, now)

	h.r.resolveConfirming(context.Background(), h.st, now)

	if !h.o.askedFor("hash-a") {
		t.Fatal("거래소에 물어보지 않았다")
	}
	if got := h.exposed(); got != 0 {
		t.Fatalf("노출 %.4f, want 0 — 안 찼다고 확인됐다", got)
	}
	if len(h.st.confirming) != 0 {
		t.Fatalf("확인 대기가 %d건 남았다", len(h.st.confirming))
	}
}

// 전량 찼으면 명목 전액이 **체결분으로** 남는다 — 미체결이 아니라.
func TestFullyFilledOrderKeepsItsNotionalAsFilled(t *testing.T) {
	h := newResolveHarness(t)
	now := time.Unix(1786469767, 0)
	h.cancelled("o1", "hash-a", 8, 3.92, now)
	h.o.filledFn = func(string) (float64, error) { return 8, nil }

	h.r.resolveConfirming(context.Background(), h.st, now)

	x := h.st.exposure()
	if x.FilledNotional != 3.92 {
		t.Fatalf("체결 명목 %.4f, want 3.92", x.FilledNotional)
	}
	if x.PendingCancel != 0 || x.OpenNotional != 0 {
		t.Fatalf("미체결로 남았다: %+v", x)
	}
	if h.st.confirmedFilledShares != 8 {
		t.Fatalf("확정 체결 주수 %v, want 8", h.st.confirmedFilledShares)
	}
}

// 부분 체결은 찬 만큼만 남는다.
func TestPartialFillReleasesOnlyTheUnfilledPart(t *testing.T) {
	h := newResolveHarness(t)
	now := time.Unix(1786469767, 0)
	h.cancelled("o1", "hash-a", 8, 3.92, now) // 8주 @ 0.49
	h.o.filledFn = func(string) (float64, error) { return 2, nil }

	h.r.resolveConfirming(context.Background(), h.st, now)

	x := h.st.exposure()
	want := 3.92 * 2 / 8 // 0.98
	if diff := x.FilledNotional - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("체결 명목 %.6f, want %.6f (8주 중 2주)", x.FilledNotional, want)
	}
	if h.exposed()-want > 1e-9 {
		t.Fatalf("총 노출 %.6f — 안 찬 6주분이 풀리지 않았다", h.exposed())
	}
}

// **같은 돈을 두 번 세지 않는다.** 단건 조회로 먼저 안 체결이 나중에 피드로
// 또 도착한다. 더하면 노출이 두 배가 되어 회차가 통째로 멈춘다.
func TestConfirmedAndFeedAreNotAddedTwice(t *testing.T) {
	h := newResolveHarness(t)
	now := time.Unix(1786469767, 0)
	h.cancelled("o1", "hash-a", 8, 3.92, now)
	h.o.filledFn = func(string) (float64, error) { return 8, nil }
	h.r.resolveConfirming(context.Background(), h.st, now)

	// 2.7초 뒤 같은 체결이 피드로 도착한다.
	h.st.filledShares += 8
	h.st.filledNotional += 3.92

	if got := h.st.exposure().FilledNotional; got != 3.92 {
		t.Fatalf("체결 명목 %.4f, want 3.92 — 같은 체결을 두 번 셌다", got)
	}
	if got := h.st.filledSharesKnown(); got != 8 {
		t.Fatalf("체결 주수 %v, want 8", got)
	}
}

// 조회가 실패하면 **모르는 것이다.** 명목은 노출에 남는다 — 모르는 것을
// 안 찼다고 친 것이 이 사고를 만들었다.
func TestQueryFailureKeepsTheNotionalReserved(t *testing.T) {
	h := newResolveHarness(t)
	now := time.Unix(1786469767, 0)
	h.cancelled("o1", "hash-a", 8, 3.92, now)
	h.o.filledFn = func(string) (float64, error) { return 0, errors.New("504") }

	h.r.resolveConfirming(context.Background(), h.st, now)

	if got := h.exposed(); got != 3.92 {
		t.Fatalf("노출 %.4f, want 3.92 — 조회 실패를 '안 찼다'로 읽었다", got)
	}
	if len(h.st.confirming) != 1 {
		t.Fatalf("확인 대기가 %d건 — 답을 못 들었으면 남아 있어야 한다", len(h.st.confirming))
	}
}

// 조회 실패는 백오프한다. 매 바퀴(50ms) 다시 물으면 240 req/min 예산이
// 순식간에 사라지고, 그러면 취소도 주문도 못 낸다.
func TestQueryFailureBacksOff(t *testing.T) {
	h := newResolveHarness(t)
	h.r.RejectBackoff = 500 * time.Millisecond
	now := time.Unix(1786469767, 0)
	h.cancelled("o1", "hash-a", 8, 3.92, now)
	h.o.filledFn = func(string) (float64, error) { return 0, errors.New("504") }

	h.r.resolveConfirming(context.Background(), h.st, now)
	h.r.resolveConfirming(context.Background(), h.st, now.Add(50*time.Millisecond))
	if n := len(h.o.filledAsks); n != 1 {
		t.Fatalf("50ms 만에 %d번 물었다 — 백오프가 없다", n)
	}
	h.r.resolveConfirming(context.Background(), h.st, now.Add(600*time.Millisecond))
	if n := len(h.o.filledAsks); n != 2 {
		t.Fatalf("백오프가 지났는데 %d번 — 다시 묻지 않는다", n)
	}
}

// 해시가 없으면 물어볼 수 없다. 그래도 명목은 남는다 — 그리고 유예가 지나면
// 체결 피드에 맡긴다. 영영 잠그면 회차 하나가 통째로 멈춘다.
func TestNoHashKeepsTheNotionalReserved(t *testing.T) {
	h := newResolveHarness(t)
	now := time.Unix(1786469767, 0)
	h.cancelled("o1", "", 8, 3.92, now)

	h.r.resolveConfirming(context.Background(), h.st, now)
	if got := h.exposed(); got != 3.92 {
		t.Fatalf("노출 %.4f, want 3.92", got)
	}
	if len(h.o.filledAsks) != 0 {
		t.Fatalf("해시가 없는데 물어봤다: %v", h.o.filledAsks)
	}

	// 유예 안: 아직 잠겨 있다.
	h.r.resolveConfirming(context.Background(), h.st, now.Add(5*time.Second))
	if got := h.exposed(); got != 3.92 {
		t.Fatalf("유예 5s/6s 에 노출 %.4f — 너무 일찍 풀었다", got)
	}
	// 유예 뒤: 피드가 따라잡았다고 본다.
	h.r.resolveConfirming(context.Background(), h.st, now.Add(6*time.Second))
	if got := h.exposed(); got != 0 {
		t.Fatalf("유예가 지났는데 노출 %.4f 로 잠겨 있다 — 회차가 멈춘다", got)
	}
}

// 말이 안 되는 답은 답이 아니다. 음수 체결량을 그대로 더하면 노출이
// **줄어든다** — 사고의 방향이다.
func TestNonsenseFilledAmountIsNotTrusted(t *testing.T) {
	for _, v := range []float64{-1, mathNaN(), mathInf()} {
		h := newResolveHarness(t)
		now := time.Unix(1786469767, 0)
		h.cancelled("o1", "hash-a", 8, 3.92, now)
		h.o.filledFn = func(string) (float64, error) { return v, nil }

		h.r.resolveConfirming(context.Background(), h.st, now)
		if got := h.exposed(); got != 3.92 {
			t.Errorf("체결량 %v 에 노출 %.4f, want 3.92", v, got)
		}
		if h.st.confirmedFilledShares != 0 {
			t.Errorf("체결량 %v 를 확정 체결로 더했다 (%v)", v, h.st.confirmedFilledShares)
		}
	}
}

// 주문 수량보다 큰 답도 그대로 쓰지 않는다. 거래소가 다른 단위를 주면
// (예: wei 를 주 단위로 착각) 노출이 터무니없이 커져 회차가 멈춘다.
func TestFilledAboveOrderSizeIsClamped(t *testing.T) {
	h := newResolveHarness(t)
	now := time.Unix(1786469767, 0)
	h.cancelled("o1", "hash-a", 8, 3.92, now)
	h.o.filledFn = func(string) (float64, error) { return 8e18, nil }

	h.r.resolveConfirming(context.Background(), h.st, now)
	if got := h.st.confirmedFilledShares; got != 8 {
		t.Fatalf("확정 체결 주수 %v, want 8 (주문 수량이 상한이다)", got)
	}
	if got := h.st.exposure().FilledNotional; got != 3.92 {
		t.Fatalf("체결 명목 %.4f, want 3.92", got)
	}
}

// 확정 체결이 **다음 주문의 기준점**이 되어야 한다.
//
// 기준점이 피드 값이면, 단건 조회로 먼저 안 8주가 나중에 피드로 도착할 때
// 그것이 다음 주문의 몫으로 계산된다 — 한 주도 안 찬 8주 주문이 전량 체결로
// 보이고 추적에서 사라진다. 그러면 아무도 그것을 취소하지 않는다.
func TestNewOrderBaselinesOnConfirmedFills(t *testing.T) {
	h := newResolveHarness(t)
	now := time.Unix(1786469767, 0)
	h.cancelled("o1", "hash-a", 8, 3.92, now)
	h.o.filledFn = func(string) (float64, error) { return 8, nil }
	h.r.resolveConfirming(context.Background(), h.st, now)

	// 새 주문. 기준점은 "지금까지 아는 체결" 이어야 한다.
	h.st.live = &openOrder{id: "o2", shares: 8, notional: 3.92, filledBefore: h.st.filledSharesKnown()}

	// 앞 주문의 8주가 이제 피드로 도착한다.
	h.st.filledShares += 8
	h.st.retireFullyFilled()

	if h.st.live == nil {
		t.Fatal("한 주도 안 찬 주문이 전량 체결로 물러났다 — 아무도 그것을 취소하지 않는다")
	}
}

// ---------------------------------------------------------------------------
// 배선 — 위 시험들은 resolveConfirming 을 직접 부른다
// ---------------------------------------------------------------------------

// 취소가 확인된 주문에는 **얼마나 찼는지 물어본다.** 이 배선이 빠지면 명목이
// 영영 잠기고, 더 나쁘게는 취소를 "안 찼다"로 읽는 옛 사고로 되돌아간다.
//
// 회차마다 한 번만 거는 지금은 그 취소가 회차 종료 취소뿐이라, 이 경로는
// cancelEverything 안에서만 지난다 — 그래서 더더욱 배선을 못 박아야 한다.
func TestCancelledOrderIsAskedWhetherItFilled(t *testing.T) {
	h := newHarness(t)
	cap := h.equity.AvailableUSDT * 0.0455

	// 취소가 확인된 그 주문은 사실 전량 체결돼 있었다. 거래소는 그래도
	// removed 로 답한다(호가창에 없으니까).
	h.orders.filledFn = func(hash string) (float64, error) {
		if hash == "hash-ord-0" {
			return 9, nil
		}
		return 0, nil
	}
	if err := h.run(); err != nil && !isDisarm(err) {
		t.Fatalf("RunRound: %v", err)
	}
	if !h.orders.askedFor("hash-ord-0") {
		t.Fatal("취소 확인된 주문의 체결 여부를 묻지 않았다 — 배선에 resolveConfirming 이 없다")
	}

	var placed float64
	h.orders.mu.Lock()
	for _, r := range h.orders.creates {
		placed += r.Notional()
	}
	n := len(h.orders.creates)
	h.orders.mu.Unlock()
	if placed > cap+1e-9 {
		t.Fatalf("이 회차에 건 명목 합계 %.4f > 상한 %.4f (건 주문: %d건)", placed, cap, n)
	}
}

// 회차가 끝날 때 확인 대기가 남아 있으면 **그것도 정리한다.** 남겨 두면
// 마지막 관측의 체결 주수가 틀리고, 감시가 그 값으로 배당을 계산한다.
func TestRoundEndResolvesConfirming(t *testing.T) {
	h := newHarness(t)
	h.orders.filledFn = func(hash string) (float64, error) { return 0, nil }
	var last Observation
	h.runner.Observe = func(o Observation) { last = o }

	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	x := last.Exposure
	if x.OpenNotional != 0 || x.PendingCancel != 0 {
		t.Fatalf("회차가 끝났는데 미체결이 남았다: %+v", x)
	}
}

// 체결 조회 인터페이스가 사라지지 않게 못 박는다. Orders 를 구현한 척하는
// 타입이 Filled 를 빠뜨리면 컴파일에서 걸려야 한다.
var _ Orders = (*fakeOrders)(nil)

// 아래 둘은 NaN/Inf 를 만드는 자리를 한 곳에 모은 것이다.
func mathNaN() float64 { var z float64; return z / z }
func mathInf() float64 { var z float64; return 1 / z }

// TestUnresolvedOrderDoesNotRetireTheNextOne 은 자체 검토에서 나온 결함이다.
//
// 확인 대기 중인 A(10주)가 있는 동안 B(5주)를 냈는데, 나중에 A 의 10주가
// 체결 피드로 도착하면 `10 − 0 ≥ 5` 로 **한 주도 안 찬 B 가 전량 체결로
// 보인다.** 그러면 B 는 추적에서 사라지고 아무도 취소하지 않는다.
func TestUnresolvedOrderDoesNotRetireTheNextOne(t *testing.T) {
	h := newResolveHarness(t)
	now := time.Unix(1786469767, 0)
	// A: 10주 @0.20, 취소 확인됐지만 해시가 없어 물어볼 수 없다.
	h.cancelled("A", "", 10, 2.0, now)

	// 그 상태에서 B 를 **실제 경로로** 낸다. 기준점을 손으로 넣으면
	// transmit 의 그 한 줄을 되돌려도 이 시험이 통과한다.
	req := Request{Tick: order.NewTick(48, 2), Shares: 5, Outcome: ledger.OutcomeUp}
	if _, err := h.r.transmit(context.Background(), h.st, req, now); err != nil {
		t.Fatalf("transmit: %v", err)
	}
	if h.st.live == nil {
		t.Fatal("주문이 걸리지 않았다")
	}
	if h.st.live.filledBefore != 10 {
		t.Fatalf("새 주문의 기준점 %v, want 10 (확인 대기 A 의 10주를 포함해야 한다)",
			h.st.live.filledBefore)
	}

	// A 의 10주가 피드로 도착한다.
	h.st.filledShares += 10
	h.st.retireFullyFilled()

	if h.st.live == nil {
		t.Fatal("한 주도 안 찬 B 가 전량 체결로 물러났다 — 아무도 B 를 취소하지 않는다")
	}
}

// 확인 대기가 없으면 기준점은 그냥 아는 체결이다 — 정상 경로에서 새 주문이
// 괜히 무거워지지 않아야 한다.
func TestBaselineIsPlainWhenNothingIsPending(t *testing.T) {
	h := newResolveHarness(t)
	h.st.filledShares = 3
	if got := h.st.filledBaseline(); got != 3 {
		t.Fatalf("기준점 %v, want 3", got)
	}
}
