package exec

import (
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
	"github.com/kdm000718/GLD-9.1/internal/quote"
)

// fakeBook 은 호가창 모양을 값으로 만든다. ws.Book 은 프레임을 흘려 넣어야
// 모양이 잡히는데, 여기서 시험하는 것은 **어느 쪽을 읽고 어떻게 뒤집는가**
// 뿐이라 그 왕복이 필요 없다.
type fakeBook struct {
	bid, ask       int64
	hasBid, hasAsk bool
	// 마지막으로 각 쪽에 넘어온 제외 맵. 제외가 엉뚱한 쪽에 걸리는 것을
	// 잡으려면 값이 아니라 **어디로 갔는지**를 봐야 한다.
	bidEx, askEx map[int64]ws.Shares
}

func (b *fakeBook) BestBid(ex map[int64]ws.Shares) (int64, bool) {
	b.bidEx = ex
	return b.bid, b.hasBid
}

func (b *fakeBook) BestAsk(ex map[int64]ws.Shares) (int64, bool) {
	b.askEx = ex
	return b.ask, b.hasAsk
}

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

	qb := v.quoteBook(b, nil)
	if qb.BestBid != 47 || !qb.HasBid || qb.BestAsk != 52 || !qb.HasAsk {
		t.Fatalf("Up 회차인데 책이 바뀌었다: %+v", qb)
	}
	if qb.Precision != 2 {
		t.Fatalf("Precision = %d, want 2", qb.Precision)
	}
}

// TestDownMirrorsTheBook 이 이 파일의 핵심이다.
//
// 오더북은 마켓당 한 벌이고 Up 기준이다. Down 을 사려면 두 결과의 가격 합이
// 1 이라는 사실로 뒤집어야 한다.
func TestDownMirrorsTheBook(t *testing.T) {
	v := mustView(t, ledger.OutcomeDown, 2)
	b := &fakeBook{bid: 55, hasBid: true, ask: 60, hasAsk: true}

	qb := v.quoteBook(b, nil)
	// Down 최우선 매수 = 1.00 − Up 매도 0.60 = 0.40
	if qb.BestBid != 40 || !qb.HasBid {
		t.Errorf("Down 매수호가 = %d(%v), want 40 — 1.00 에서 Up 매도 0.60 을 뺀 값", qb.BestBid, qb.HasBid)
	}
	// Down 최우선 매도 = 1.00 − Up 매수 0.55 = 0.45
	if qb.BestAsk != 45 || !qb.HasAsk {
		t.Errorf("Down 매도호가 = %d(%v), want 45 — 1.00 에서 Up 매수 0.55 를 뺀 값", qb.BestAsk, qb.HasAsk)
	}
}

// TestDownRoundDoesNotCrossTheSpread 는 **2026-08-11 실거래를 그대로 재현한
// 회귀 시험이다.**
//
// 회차 btc-updown-5m-1786469100, 방향 Down. 로그가 남긴 판단은
//
//	신규 49: 군중 55 는 0.5 이상 → 상한 49 → Down 8.0000주 @ 0.49
//
// 이고, 4초 뒤 8주가 **0.44 에** 체결되며 명목의 2% 를 수수료로 물었다.
// 지정가보다 좋은 가격에 체결됐다는 것은 우리가 관통했다는 뜻이다.
//
// 그 "군중 55" 는 Up 의 매수호가였다. Down 의 진짜 매도호가는 0.45 이므로
// 0.49 매수는 관통이고, 관통 방지는 엉뚱한 쪽(Up 매도호가)과 비교하느라
// 발동하지 않았다.
func TestDownRoundDoesNotCrossTheSpread(t *testing.T) {
	v := mustView(t, ledger.OutcomeDown, 2)
	// 실측 상황: Up 매수 0.55. Up 매도는 책에 없었다(관통 방지 문구가 로그에
	// 한 번도 붙지 않았다).
	b := &fakeBook{bid: 55, hasBid: true}

	qb := v.quoteBook(b, nil)
	if !qb.HasAsk || qb.BestAsk != 45 {
		t.Fatalf("Down 매도호가를 못 만들었다: %+v — 관통 방지가 비교할 대상이 없다", qb)
	}

	tgt, ok := quote.Target(qb)
	if !ok {
		t.Fatal("목표가가 없다")
	}
	if tgt >= qb.BestAsk {
		t.Fatalf("목표가 %d 가 매도호가 %d 이상이다 — 관통이다(실측: 0.49 로 걸어 0.44 에 체결, 수수료 2%%)",
			tgt, qb.BestAsk)
	}
	if tgt != 44 {
		t.Fatalf("목표가 = %d, want 44 (매도호가 45 의 한 틱 아래)", tgt)
	}
}

// TestDownExcludesOurOrdersFromTheAskSide 는 제외가 **어느 쪽에 걸리는가**를
// 본다.
//
// Down 주문 46 은 책에서 100−46 = 54 자리의 매도호가로 나타난다. 제외를
// 매수측에 걸면 아무것도 빠지지 않은 채 "우리 주문을 뺐다"고 믿게 되고,
// 봇은 자기 호가를 군중으로 읽으며 그 자리에 굳는다.
func TestDownExcludesOurOrdersFromTheAskSide(t *testing.T) {
	v := mustView(t, ledger.OutcomeDown, 2)

	ours := v.ourTicks([]exposedOrder{{tick: 46, shares: 9}})
	if len(ours) != 1 || ours[0].tick != 54 {
		t.Fatalf("우리 주문의 책 좌표 = %+v, want 틱 54 (100−46)", ours)
	}
	ex, ok := excludeOurs(ours)
	if !ok {
		t.Fatal("제외 맵을 만들지 못했다")
	}

	b := &fakeBook{bid: 55, hasBid: true, ask: 54, hasAsk: true}
	_ = v.quoteBook(b, ex)

	if b.askEx == nil {
		t.Error("매도측에 제외가 걸리지 않았다 — Down 회차에서 우리 물량은 그쪽에 있다")
	}
	if b.bidEx != nil {
		t.Error("매수측에 제외가 걸렸다 — 그쪽에는 우리 물량이 없다(군중을 지운다)")
	}
	if got := b.askEx[54]; got != ws.Qty(9) {
		t.Errorf("제외 수량 askEx[54] = %v, want %v", got, ws.Qty(9))
	}
}

func TestUpExcludesOurOrdersFromTheBidSide(t *testing.T) {
	v := mustView(t, ledger.OutcomeUp, 2)
	ex, ok := excludeOurs(v.ourTicks([]exposedOrder{{tick: 47, shares: 9}}))
	if !ok {
		t.Fatal("제외 맵을 만들지 못했다")
	}

	b := &fakeBook{bid: 47, hasBid: true, ask: 52, hasAsk: true}
	_ = v.quoteBook(b, ex)

	if b.bidEx == nil {
		t.Error("매수측에 제외가 걸리지 않았다")
	}
	if b.askEx != nil {
		t.Error("매도측에 제외가 걸렸다 — Up 회차에서 우리 물량은 매수측이다")
	}
}

// 거울 뒤에 0 이하가 되는 쪽은 없는 것으로 친다. 틱 0 은 가격 0.00 이고
// order.Tick.WeiPerShare 는 거기서 패닉한다 — 살아 있는 주문을 든 채 죽는다.
func TestMirrorDropsNonPositiveTicks(t *testing.T) {
	v := mustView(t, ledger.OutcomeDown, 2)

	// Up 매도호가가 1.00 이면 Down 매수호가는 0.00 이다 — 가격이 아니다.
	qb := v.quoteBook(&fakeBook{ask: 100, hasAsk: true}, nil)
	if qb.HasBid {
		t.Errorf("Down 매수호가 %d 를 만들었다 — 틱 0 은 가격이 아니다", qb.BestBid)
	}
	// Up 매수호가가 1.00 이면 Down 매도호가는 0.00 이다.
	qb = v.quoteBook(&fakeBook{bid: 100, hasBid: true}, nil)
	if qb.HasAsk {
		t.Errorf("Down 매도호가 %d 를 만들었다", qb.BestAsk)
	}
}

// 한쪽이 비어 있으면 거울에서도 그쪽만 빈다 — 없는 호가를 만들어내지 않는다.
func TestMirrorKeepsMissingSidesMissing(t *testing.T) {
	v := mustView(t, ledger.OutcomeDown, 2)
	qb := v.quoteBook(&fakeBook{}, nil)
	if qb.HasBid || qb.HasAsk {
		t.Fatalf("빈 책에서 호가가 나왔다: %+v", qb)
	}
}

// 방향 문자열이 이상하면 **Up 으로 조용히 읽지 않는다.** 그렇게 읽으면 Down
// 회차 전체가 남의 책을 보게 되고, 그것이 이 파일이 생긴 이유다.
func TestUnknownDirectionIsAnError(t *testing.T) {
	for _, d := range []string{"", "up", "DOWN", "long"} {
		if _, err := newBookView(d, 2); err == nil {
			t.Errorf("방향 %q 가 통과했다", d)
		}
	}
}

// precision 3 에서도 거울의 기준은 1.00 이다 — 리터럴 100 이 박혀 있으면
// 여기서 드러난다.
func TestMirrorUsesPrecisionNotALiteral(t *testing.T) {
	v := mustView(t, ledger.OutcomeDown, 3)
	qb := v.quoteBook(&fakeBook{bid: 550, hasBid: true, ask: 600, hasAsk: true}, nil)
	if qb.BestBid != 400 || qb.BestAsk != 450 {
		t.Fatalf("precision 3 거울 = 매수 %d / 매도 %d, want 400 / 450", qb.BestBid, qb.BestAsk)
	}
}

// TestDownDecisionRunsThroughDecide 는 quote.Decide 까지 지나간다.
// Target 만 보면 Decide 가 거울 이전 값을 쓰는 배선 실수를 놓친다.
func TestDownDecisionRunsThroughDecide(t *testing.T) {
	v := mustView(t, ledger.OutcomeDown, 2)
	qb := v.quoteBook(&fakeBook{bid: 55, hasBid: true}, nil)

	d := quote.Decide(qb, quote.Open{}, time.Unix(0, 0), 0, false, nil)
	if d.Action != quote.Place {
		t.Fatalf("Action = %v, want Place", d.Action)
	}
	if d.Tick != 44 {
		t.Fatalf("Tick = %d, want 44 — 관통하지 않는 최고가", d.Tick)
	}
}

// ---------------------------------------------------------------------------
// 배선 — 위 시험들은 bookView 를 직접 부른다
// ---------------------------------------------------------------------------
//
// 그래서 `loop` 안의 `view.quoteBook(...)` 한 줄을 예전처럼 되돌려도 전부
// 통과한다. 이 봇에서 사고가 난 것은 늘 함수가 아니라 **그 함수를 부르는
// 자리**였다. 아래 둘은 RunRound 를 통째로 지나간다.

// TestDownRoundNeverPlacesAboveTheMirroredAsk 는 2026-08-11 의 손실 경로를
// 루프째로 재현한다: Up 매수호가만 0.55 로 있고 매도호가는 비어 있는 책.
//
// 고치기 전에는 `군중 55 는 0.5 이상 → 상한 49` 로 0.49 에 걸었고, 그것이
// Down 의 진짜 매도호가 0.45 를 관통해 테이커 수수료 2% 를 물었다.
func TestDownRoundNeverPlacesAboveTheMirroredAsk(t *testing.T) {
	h := newHarness(t)
	h.frozen.PUp = 0.47
	h.frozen.Direction = ledger.OutcomeDown
	// 군중: Up 매수 0.55 만 있고 매도측은 비어 있다(실측 그대로).
	h.setCrowd(map[float64]float64{0.55: 100}, nil)

	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}

	ticks := h.orders.createdTicks()
	if len(ticks) == 0 {
		t.Fatal("주문이 하나도 나가지 않았다 — 이 시험은 걸린 가격을 봐야 한다")
	}
	// Down 매도호가 = 100 − 55 = 45. 그 이상은 전부 관통이다.
	for _, tk := range ticks {
		if tk >= 45 {
			t.Fatalf("틱 %d 에 걸었다 — Down 매도호가 45 를 관통한다 (걸린 틱 전부: %v)", tk, ticks)
		}
	}
	if ticks[0] != 44 {
		t.Fatalf("첫 주문 틱 = %d, want 44 (관통하지 않는 최고가)", ticks[0])
	}
}

// TestDownRoundFollowsTheMirroredBid 는 거울의 **매수쪽**이 배선을 지나는지
// 본다. 위 시험은 매도호가만 확인하므로, 거울에서 매수측을 빠뜨려도(즉
// 군중을 못 따라가도) 통과한다.
func TestDownRoundFollowsTheMirroredBid(t *testing.T) {
	h := newHarness(t)
	h.frozen.PUp = 0.47
	h.frozen.Direction = ledger.OutcomeDown
	// Up 매도 0.62 → Down 매수 0.38. Up 매수 0.55 → Down 매도 0.45.
	h.setCrowd(map[float64]float64{0.55: 100}, map[float64]float64{0.62: 100})

	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	ticks := h.orders.createdTicks()
	if len(ticks) == 0 {
		t.Fatal("주문이 나가지 않았다")
	}
	if ticks[0] != 38 {
		t.Fatalf("첫 주문 틱 = %d, want 38 (= 100 − Up 매도 62) — 거울의 매수쪽을 안 쓰고 있다", ticks[0])
	}
}
