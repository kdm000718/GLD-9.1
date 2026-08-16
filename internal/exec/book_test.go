package exec

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// fakeBook 은 호가창 모양을 값으로 만든다. ws.Book 은 프레임을 흘려 넣어야
// 모양이 잡히는데, 여기서 시험하는 것은 **어느 쪽을 읽고 어떻게 뒤집는가**
// 뿐이라 그 왕복이 필요 없다.
type fakeBook struct {
	bid, ask       int64
	hasBid, hasAsk bool
}

func (b *fakeBook) BestBid(map[int64]ws.Shares) (int64, bool) { return b.bid, b.hasBid }
func (b *fakeBook) BestAsk(map[int64]ws.Shares) (int64, bool) { return b.ask, b.hasAsk }

func mustView(t *testing.T, direction string, precision int) bookView {
	t.Helper()
	v, err := newBookView(direction, precision)
	if err != nil {
		t.Fatalf("newBookView(%q): %v", direction, err)
	}
	return v
}

func TestUpReadsTheBookAsItIs(t *testing.T) {
	v := mustView(t, ledger.OutcomeUp, 2)
	b := &fakeBook{bid: 47, hasBid: true, ask: 52, hasAsk: true}

	s := v.sides(b)
	if s.BestBid != 47 || !s.HasBid || s.BestAsk != 52 || !s.HasAsk {
		t.Fatalf("Up 회차인데 책이 바뀌었다: %+v", s)
	}
}

// TestDownMirrorsTheBook 이 이 파일의 핵심이다.
//
// 오더북은 마켓당 한 벌이고 Up 기준이다. Down 을 기록하려면 두 결과의 가격
// 합이 1 이라는 사실로 뒤집어야 한다.
func TestDownMirrorsTheBook(t *testing.T) {
	v := mustView(t, ledger.OutcomeDown, 2)
	// Up 매수 0.55 / 매도 0.59 인 책.
	b := &fakeBook{bid: 55, hasBid: true, ask: 59, hasAsk: true}

	s := v.sides(b)
	// Down 을 사겠다는 최우선 호가는 Up 매도호가의 거울이다: 1.00 − 0.59.
	if s.BestBid != 41 || !s.HasBid {
		t.Errorf("Down 매수호가 = %d,%v — 기대 41 (1.00 − Up매도 0.59)", s.BestBid, s.HasBid)
	}
	// Down 을 팔겠다는 최우선 호가는 Up 매수호가의 거울이다: 1.00 − 0.55.
	if s.BestAsk != 45 || !s.HasAsk {
		t.Errorf("Down 매도호가 = %d,%v — 기대 45 (1.00 − Up매수 0.55)", s.BestAsk, s.HasAsk)
	}
}

// 거울 뒤에 0 이하가 되는 틱은 없는 것으로 친다. 0 은 가격 0.00 이고, 그 값을
// 가격으로 되돌려 찍으면 기록이 거짓말을 한다.
func TestMirrorDropsNonPositiveTicks(t *testing.T) {
	v := mustView(t, ledger.OutcomeDown, 2)
	// Up 매수·매도가 둘 다 1.00 — 거울은 0 이다.
	s := v.sides(&fakeBook{bid: 100, hasBid: true, ask: 100, hasAsk: true})
	if s.HasBid {
		t.Errorf("거울 뒤 틱 0 을 호가로 실었다: %+v", s)
	}
	if s.HasAsk {
		t.Errorf("거울 뒤 틱 0 을 호가로 실었다: %+v", s)
	}
}

// 없는 쪽은 없는 채로 남아야 한다. Has* 가 false 인데 틱을 실어 보내면
// 읽는 쪽이 그 0 을 가격으로 읽는다.
func TestMirrorKeepsMissingSidesMissing(t *testing.T) {
	v := mustView(t, ledger.OutcomeDown, 2)
	s := v.sides(&fakeBook{})
	if s.HasBid || s.HasAsk {
		t.Errorf("빈 책에서 호가가 나왔다: %+v", s)
	}
}

func TestUnknownDirectionIsAnError(t *testing.T) {
	if _, err := newBookView("up", 2); err == nil {
		t.Fatal("소문자 방향을 받아들였다 — 조용히 Up 으로 읽으면 Down 회차 기록이 통째로 틀린다")
	}
}

// 거울의 기준점은 정밀도에서 나온다. 리터럴 100 을 박으면 precision 3 인
// 마켓에서 1.000 대신 0.100 을 기준으로 뒤집는다.
func TestMirrorUsesPrecisionNotALiteral(t *testing.T) {
	v := mustView(t, ledger.OutcomeDown, 3)
	s := v.sides(&fakeBook{ask: 590, hasAsk: true})
	if s.BestBid != 410 {
		t.Errorf("precision 3 에서 Down 매수호가 = %d, 기대 410 (1000 − 590)", s.BestBid)
	}
}

// 없는 호가는 "-" 로 찍는다. 0 으로 찍으면 "가격 0.00 에 호가가 있다"로 읽힌다.
func TestLabelMarksMissingSides(t *testing.T) {
	v := mustView(t, ledger.OutcomeUp, 2)
	if got := v.label(0, false); got != "-" {
		t.Errorf("없는 호가 표기 = %q, 기대 %q", got, "-")
	}
	if got := v.label(47, true); got != "0.47" {
		t.Errorf("틱 47 표기 = %q, 기대 %q", got, "0.47")
	}
}

// ---------------------------------------------------------------------------
// 배선 — 책은 기록일 뿐 주문을 바꾸지 않는다
// ---------------------------------------------------------------------------

// **이것이 book.go 의 경고를 지키는 테스트다.**
//
// 2026-08-16 하루 동안만 이 파일의 불변식이 뒤집혀 있었다(최우선 매수호가가
// 곧 지정가였다). 2026-08-17 부터 다시 원래대로다 — **어떤 책 모양에서도
// 주문은 같은 곳에 같은 크기로 나간다.** 책을 읽어 가격이나 수량을 정하는
// 경로가 하나라도 생기면 여기서 갈라진다.
//
// Down 회차도 함께 돈다. 거울은 **기록에만** 쓰이므로 주문은 그대로여야 한다.
func TestBookNeverChangesTheOrder(t *testing.T) {
	cases := []struct {
		name       string
		dir        string
		bids, asks map[float64]float64
	}{
		{"빈 책", ledger.OutcomeUp, nil, nil},
		{"매도만 있다", ledger.OutcomeUp, nil, map[float64]float64{0.55: 100}},
		{"우리 지정가 아래", ledger.OutcomeUp, map[float64]float64{0.45: 100, 0.44: 50}, map[float64]float64{0.46: 100}},
		{"우리 지정가 위", ledger.OutcomeUp, map[float64]float64{0.53: 100}, map[float64]float64{0.56: 100}},
		{"매도가 우리 지정가 이하 — 관통", ledger.OutcomeUp, map[float64]float64{0.20: 100}, map[float64]float64{0.30: 100}},
		{"극단 — 0.99 매수", ledger.OutcomeUp, map[float64]float64{0.99: 100}, nil},
		{"Down 회차", ledger.OutcomeDown, map[float64]float64{0.52: 100}, map[float64]float64{0.55: 100}},
		{"Down 회차 · 매도 없음", ledger.OutcomeDown, map[float64]float64{0.40: 100}, nil},
	}
	for _, c := range cases {
		h := newHarness(t)
		h.frozen.Direction = c.dir
		if c.dir == ledger.OutcomeDown {
			h.frozen.PUp = 0.5 - live.ConfidenceThreshold
		}
		h.setCrowd(c.bids, c.asks)
		if err := h.run(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		h.orders.mu.Lock()
		creates := append([]Request(nil), h.orders.creates...)
		h.orders.mu.Unlock()

		if len(creates) != 1 {
			t.Fatalf("%s: 주문 %d건, 기대 1건", c.name, len(creates))
		}
		if got := creates[0].Tick.V; got != limitPriceNum {
			t.Errorf("%s: 주문 틱 %d, 기대 %d — 책을 읽어 가격을 정하는 경로가 생겼다",
				c.name, got, limitPriceNum)
		}
		if got, want := creates[0].Shares, wantShares(h); got != want {
			t.Errorf("%s: %v주, 기대 %v주 — 책을 읽어 크기를 정하는 경로가 생겼다",
				c.name, got, want)
		}
	}
}

// wantShares 는 이 하네스에서 나가야 할 주수다. 책과 무관하게 예산과 지정가
// 에서만 나온다.
func wantShares(h *harness) float64 {
	tk, err := limitTick(h.round.Precision)
	if err != nil {
		h.t.Fatalf("limitTick: %v", err)
	}
	return risk.Shares(legBudget(h.equity, risk.Exposure{}, h.frozen.Confidence), tk.Float())
}

// 그럼에도 **기록은 방향에 맞아야 한다.** Down 회차에서 Up 의 값을 그대로
// 찍으면 "0.47 이 그때 좋은 가격이었나" 를 사후에 정반대로 답하게 된다 —
// 2026-08-11 의 손실이 정확히 그 착각에서 나왔다.
func TestDownRoundLogsTheMirroredMarket(t *testing.T) {
	h := newHarness(t)
	h.frozen.Direction = ledger.OutcomeDown
	h.frozen.PUp = 0.5 - live.ConfidenceThreshold
	// Up 매수 0.55 / 매도 0.59  →  Down 매수 0.41 / 매도 0.45
	h.setCrowd(map[float64]float64{0.55: 100}, map[float64]float64{0.59: 100})

	var placed string
	h.runner.Log = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if strings.Contains(line, "시장 매수") {
			placed = line
		}
	}
	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if placed == "" {
		t.Fatal("주문 로그에 시장 기록이 없다")
	}
	if !strings.Contains(placed, "시장 매수 0.41 / 매도 0.45") {
		t.Errorf("Down 회차 기록 = %q — Up 기준 값(0.55/0.59)을 그대로 찍었다면 거울이 빠진 것이다", placed)
	}
}

// **로그의 시장 값은 책에서 오고, 우리 지정가는 상수에서 온다.** 둘이 같은
// 줄에 찍히지만 서로 다른 출처라는 것을 못 박는다 — 로그를 보고 "그때 우리가
// 저 값에 걸었구나" 로 읽으면 안 된다.
func TestLoggedMarketIsTheBookNotOurPrice(t *testing.T) {
	for _, bid := range []float64{0.31, 0.45, 0.49, 0.50, 0.53, 0.68} {
		h := newHarness(t)
		h.setCrowd(map[float64]float64{bid: 100}, map[float64]float64{bid + 0.03: 100})
		var placed string
		h.runner.Log = func(format string, args ...any) {
			if line := fmt.Sprintf(format, args...); strings.Contains(line, "시장 매수") {
				placed = line
			}
		}
		if err := h.run(); err != nil {
			t.Fatalf("매수호가 %v: %v", bid, err)
		}
		ticks := h.orders.createdTicks()
		if len(ticks) != 1 || ticks[0] != limitPriceNum {
			t.Fatalf("매수호가 %v: 주문 틱 %v, 기대 [%d]", bid, ticks, limitPriceNum)
		}
		if !strings.Contains(placed, fmt.Sprintf("시장 매수 %v ", bid)) {
			t.Errorf("매수호가 %v: 로그 %q 에 그 값이 없다 — 기록이 책을 안 보고 있다", bid, placed)
		}
	}
}

// **책이 아무리 움직여도 나가는 주문은 하나다.**
//
// 2026-08-14 에 테이커 다리가 잠깐 이 파일의 값을 읽어 두 번째 주문을 냈다.
// 같은 날 제거됐고, 이 시험이 그 경로가 되살아나지 않았음을 지킨다.
func TestBookNeverAddsASecondOrder(t *testing.T) {
	for _, ask := range []float64{0.20, 0.40, 0.48, 0.49, 0.55, 0.64, 0.99} {
		h := newHarness(t)
		h.setCrowd(map[float64]float64{0.10: 100}, map[float64]float64{ask: 100})
		if err := h.run(); err != nil {
			t.Fatalf("매도호가 %v: %v", ask, err)
		}
		ticks := h.orders.createdTicks()
		if len(ticks) != 1 || ticks[0] != limitPriceNum {
			t.Errorf("매도호가 %v: 틱 %v, 기대 [%d]", ask, ticks, limitPriceNum)
		}
	}
}
