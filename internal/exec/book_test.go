package exec

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/live"
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

// **2026-08-16 에 이 파일의 불변식이 뒤집혔다.**
//
// 그전까지는 "어떤 책 모양에서도 주문은 같다" 였다. 이제는 반대다 — **책의
// 최우선 매수호가가 곧 우리 지정가다.** 이 시험은 그 대응이 정확한지 본다:
// 책이 0.53 을 말하면 0.53 에 걸어야 하고, 0.61 을 말하면 0.61 에 걸어야 한다.
//
// 0.5 상한이 없다는 것도 여기서 못 박는다 — 상한이 되살아나면 0.5 이상 칸이
// 전부 0.49 로 내려앉아 갈라진다.
func TestOrderFollowsTheBestBid(t *testing.T) {
	// 우리 토큰 기준 최우선 매수호가가 want 여야 한다.
	//
	// Down 회차에서는 책이 Up 기준이므로 거울이다 — Up 매도 0.39 는 Down 매수
	// 0.61 이다. **거울이 빠지면 여기서 갈라진다.**
	cases := []struct {
		name       string
		dir        string
		bids, asks map[float64]float64
		want       int64
	}{
		{"Up · 최우선 0.45", ledger.OutcomeUp, map[float64]float64{0.45: 100, 0.44: 50}, map[float64]float64{0.55: 100}, 45},
		{"Up · 최우선 0.49", ledger.OutcomeUp, map[float64]float64{0.49: 100}, map[float64]float64{0.52: 100}, 49},
		{"Up · 최우선 0.50 (예전 상한 정각)", ledger.OutcomeUp, map[float64]float64{0.50: 100}, map[float64]float64{0.53: 100}, 50},
		{"Up · 최우선 0.53", ledger.OutcomeUp, map[float64]float64{0.53: 100}, map[float64]float64{0.56: 100}, 53},
		{"Up · 최우선 0.68 (실측 최고)", ledger.OutcomeUp, map[float64]float64{0.68: 100}, map[float64]float64{0.71: 100}, 68},
		{"Up · 최우선 0.99", ledger.OutcomeUp, map[float64]float64{0.99: 100}, nil, 99},
		{"Down 거울 · Up매도 0.55 → 0.45", ledger.OutcomeDown, map[float64]float64{0.52: 100}, map[float64]float64{0.55: 100}, 45},
		{"Down 거울 · Up매도 0.39 → 0.61", ledger.OutcomeDown, map[float64]float64{0.36: 100}, map[float64]float64{0.39: 100}, 61},
		{"Down 거울 · Up매도 0.50 → 0.50", ledger.OutcomeDown, map[float64]float64{0.47: 100}, map[float64]float64{0.50: 100}, 50},
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

		// 회차당 주문은 한 건뿐이다. 두 건이 나가면 테이커 다리가 되살아난
		// 것이고, 그것은 전략 변경이다.
		if len(creates) != 1 {
			t.Fatalf("%s: 주문 %d건, 기대 1건", c.name, len(creates))
		}
		if got := creates[0].Tick.V; got != c.want {
			t.Errorf("%s: 주문 틱 %d, 기대 %d — 최우선 매수호가를 따라가지 않았다", c.name, got, c.want)
		}
		// **크기는 가격에서 따라 나온다.** cap / 가격 을 내림한 주수다.
		wantShares := int(4.55 / (float64(c.want) / 100))
		if int(creates[0].Shares) != wantShares {
			t.Errorf("%s: %v주, 기대 %d주 (cap 4.55 / %.2f)", c.name, creates[0].Shares, wantShares, float64(c.want)/100)
		}
	}
}

// **매수호가가 없으면 걸지 않는다.** 근사한 가격에 거느니 회차를 통째로
// 버리는 쪽이 맞다 — 우리가 정하지 않은 값에 베팅하는 것이기 때문이다.
func TestNoBidMeansNoOrder(t *testing.T) {
	for _, c := range []struct {
		name       string
		dir        string
		bids, asks map[float64]float64
	}{
		{"빈 책", ledger.OutcomeUp, nil, nil},
		{"매도만 있다", ledger.OutcomeUp, nil, map[float64]float64{0.55: 100}},
		{"Down 회차인데 매도가 없다", ledger.OutcomeDown, map[float64]float64{0.40: 100}, nil},
	} {
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
		n := len(h.orders.creates)
		h.orders.mu.Unlock()
		if n != 0 {
			t.Errorf("%s: 주문 %d건 — 매수호가가 없는데 걸었다", c.name, n)
		}
	}
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

// **우리가 거는 가격과 우리가 기록하는 가격이 같아야 한다.**
//
// 둘이 갈리면 "그때 그 가격이 좋았나" 를 사후에 답할 수 없다. 최우선호가에
// 거는 지금은 로그의 "시장 매수" 값이 곧 우리 지정가여야 한다.
func TestLoggedMarketBidIsWhatWeActuallyBid(t *testing.T) {
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
		if len(ticks) != 1 {
			t.Fatalf("매수호가 %v: 틱 %v, 기대 1건", bid, ticks)
		}
		want := int64(bid*100 + 0.5)
		if ticks[0] != want {
			t.Errorf("매수호가 %v: 주문 틱 %d, 기대 %d", bid, ticks[0], want)
		}
		if !strings.Contains(placed, fmt.Sprintf("시장 매수 %v ", bid)) {
			t.Errorf("매수호가 %v: 로그 %q 에 그 값이 없다 — 건 가격과 찍은 가격이 갈렸다", bid, placed)
		}
	}
}

// **책이 아무리 움직여도 나가는 주문은 하나다.**
//
// 2026-08-14 에 테이커 다리가 잠깐 이 파일의 값을 읽어 두 번째 주문을 냈다.
// 같은 날 제거됐고, 이 시험이 그 경로가 되살아나지 않았음을 지킨다. 가격은
// 이제 책을 따라가지만 **건수는 여전히 하나다.**
func TestBookNeverAddsASecondOrder(t *testing.T) {
	for _, ask := range []float64{0.20, 0.40, 0.48, 0.49, 0.55, 0.64, 0.99} {
		h := newHarness(t)
		h.setCrowd(map[float64]float64{0.10: 100}, map[float64]float64{ask: 100})
		if err := h.run(); err != nil {
			t.Fatalf("매도호가 %v: %v", ask, err)
		}
		ticks := h.orders.createdTicks()
		if len(ticks) != 1 || ticks[0] != 10 {
			t.Errorf("매도호가 %v: 틱 %v, 기대 [10] (최우선 매수호가 0.10)", ask, ticks)
		}
	}
}
