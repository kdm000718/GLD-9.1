package exec

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
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
// 어떤 책 모양에서도 주문은 같은 곳에 같은 크기로 나간다. 책을 읽어 가격이나
// 수량을 정하는 경로가 하나라도 생기면 여기서 갈라진다.
func TestBookNeverChangesTheOrder(t *testing.T) {
	type shape struct {
		name       string
		bids, asks map[float64]float64
	}
	shapes := []shape{
		{"빈 책", nil, nil},
		{"매수만 낮게", map[float64]float64{0.10: 100}, nil},
		{"매수만 높게", map[float64]float64{0.90: 100}, nil},
		{"매도가 지정가 아래", map[float64]float64{0.10: 100}, map[float64]float64{0.20: 100}},
		{"매도가 지정가 위", map[float64]float64{0.45: 100}, map[float64]float64{0.80: 100}},
		{"매도가 지정가와 같음", map[float64]float64{0.20: 100}, map[float64]float64{0.47: 100}},
	}
	for _, dir := range []string{ledger.OutcomeUp, ledger.OutcomeDown} {
		for _, s := range shapes {
			h := newHarness(t)
			h.frozen.Direction = dir
			if dir == ledger.OutcomeDown {
				h.frozen.PUp = 0.47
			}
			h.setCrowd(s.bids, s.asks)
			if err := h.run(); err != nil {
				t.Fatalf("%s/%s: %v", dir, s.name, err)
			}
			h.orders.mu.Lock()
			creates := append([]Request(nil), h.orders.creates...)
			h.orders.mu.Unlock()
			if len(creates) != 1 {
				t.Fatalf("%s/%s: 주문 %d건, 기대 1건", dir, s.name, len(creates))
			}
			if creates[0].Tick.V != 47 {
				t.Errorf("%s/%s: 주문 틱 %d, 기대 47 — 책이 가격을 바꿨다", dir, s.name, creates[0].Tick.V)
			}
			// 크기도 책과 무관하다. cap 4.55 / 0.47 → 9주.
			if creates[0].Shares != 9 {
				t.Errorf("%s/%s: 주문 %v주, 기대 9주 — 책이 크기를 바꿨다", dir, s.name, creates[0].Shares)
			}
		}
	}
}

// 그럼에도 **기록은 방향에 맞아야 한다.** Down 회차에서 Up 의 값을 그대로
// 찍으면 "0.47 이 그때 좋은 가격이었나" 를 사후에 정반대로 답하게 된다 —
// 2026-08-11 의 손실이 정확히 그 착각에서 나왔다.
func TestDownRoundLogsTheMirroredMarket(t *testing.T) {
	h := newHarness(t)
	h.frozen.Direction = ledger.OutcomeDown
	h.frozen.PUp = 0.47
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

// 지정가 틱은 order 층에서 만든다. 이 테스트는 그 두 층이 같은 값을 말하는지
// 본다 — 갈리면 우리가 거는 가격과 우리가 기록하는 가격이 달라진다.
func TestLimitTickAgreesWithTheOrderLayer(t *testing.T) {
	for _, p := range []int{2, 3} {
		tk, err := limitTick(p)
		if err != nil {
			t.Fatalf("precision %d: %v", p, err)
		}
		v := mustView(t, ledger.OutcomeUp, p)
		if got, want := v.label(tk.V, true), "0.47"; got != want {
			t.Errorf("precision %d: 지정가 표기 %q, 기대 %q", p, got, want)
		}
		if tk.Precision != p {
			t.Errorf("precision %d: 틱의 정밀도가 %d 다", p, tk.Precision)
		}
		if order.NewTick(tk.V, p).Float() != LimitPrice {
			t.Errorf("precision %d: 틱 %d 가 %v 가 아니다", p, tk.V, LimitPrice)
		}
	}
}
