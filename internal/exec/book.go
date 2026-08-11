package exec

import (
	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
	"github.com/kdm000718/GLD-9.1/internal/quote"
)

// ---------------------------------------------------------------------------
// 오더북은 한 벌인데 토큰은 둘이다
// ---------------------------------------------------------------------------
//
// predict.fun 의 오더북 토픽은 **마켓 단위**다(`predictOrderbook/<marketId>`).
// 그런데 마켓에는 Up·Down 두 토큰이 있고, 우리는 회차마다 그중 하나를 산다.
//
// # 실측 (2026-08-12)
//
// `predictTrades` 스트림이 구조를 그대로 드러낸다:
//
//	taker  {outcomeIndex:1, price:0.58, side:Bid}
//	makers [{outcomeIndex:2, price:0.42, side:Bid, matchType:SPLIT}]
//
// 두 결과의 가격 합이 정확히 1.00 이고(CTF 이진 시장의 정의), 같은 순간
// 오더북의 매수측 최우선이 0.58 이었다. 즉 **책은 Up(outcomeIndex 1) 기준
// 한 벌**이고, 매도측은 Down 매수호가의 거울이다:
//
//	책의 매도호가 0.59  ⟺  Down 을 0.41 에 사겠다는 호가
//
// (카테고리 응답에서 indexSet 1 = "Up", 2 = "Down" 임을 확인했다.)
//
// # 이것을 몰라서 무슨 일이 있었나
//
// 2026-08-11 실거래에서 Down 방향 회차 두 건이 **지정가 0.49 매수인데 0.40·
// 0.44 에 체결되며 테이커 수수료 2% 를 물었다.** 지정가보다 좋은 가격에
// 체결됐다는 것은 우리가 관통했다는 뜻이다.
//
// 원인은 이 파일이 없었다는 것이다. Down 을 사면서 Up 의 매수호가를 "군중"
// 으로 읽었고, 관통 방지는 Up 의 매도호가와 비교했다. 그 회차들의 로그는
// 이렇게 말한다 — `군중 55 는 0.5 이상 → 상한 49`. Up 매수호가가 0.55 면
// **Down 의 진짜 매도호가는 0.45** 이므로, 0.49 매수는 관통이다.
//
// 그리고 이것은 수수료만의 문제가 아니다. Down 회차 내내 우리는 상관없는
// 책을 보고 가격을 정하고 있었다.
//
// # 그래서 이 파일이 하는 일
//
// 회차 방향에 따라 책을 뒤집어 **우리가 사려는 토큰 기준의 호가창**을 만든다.
// quote 는 지금처럼 "내 토큰의 책"만 받으면 되고, 거울이 있다는 사실을 알
// 필요가 없다.

// bookView 는 회차 방향에 맞춰 오더북을 읽는 방법이다.
//
// 값 타입이고 회차 시작에 한 번 만든다. 상태가 없다 — 매 바퀴 새로 만들어도
// 결과가 같아야 하고, 그래야 방향이 회차 도중에 새는 자리가 없다.
type bookView struct {
	// mirror 가 참이면 책은 우리 토큰의 반대편 기준이다(Down 회차).
	mirror bool
	// full 은 가격 1.00 의 틱 수다. 거울은 이 값에서 빼서 만든다.
	full int64
	// precision 은 quote.Book 에 그대로 실린다.
	precision int
}

// newBookView 는 방향과 정밀도로 읽기 방법을 정한다.
//
// 방향 문자열을 정확히 두 철자만 받는다. 모르는 값이면 **거울을 걸지 않는
// 쪽**이 아니라 에러다 — 조용히 Up 으로 읽으면 Down 회차 전체가 엉뚱한 책을
// 보게 되고, 그게 바로 이 파일이 생긴 이유다.
func newBookView(direction string, precision int) (bookView, error) {
	switch direction {
	case ledger.OutcomeUp:
		return bookView{mirror: false, full: order.Full(precision), precision: precision}, nil
	case ledger.OutcomeDown:
		return bookView{mirror: true, full: order.Full(precision), precision: precision}, nil
	}
	return bookView{}, tokenDirectionError(direction)
}

// ourTicks 는 우리 주문을 **책의 좌표계**로 옮긴다.
//
// 우리 주문의 틱은 우리가 사는 토큰 기준이다. Down 주문 t 는 책에서
// `full − t` 자리의 매도호가로 나타난다 — 거기서 빼야 우리 자신을 군중으로
// 읽지 않는다.
//
// **이 변환이 빠지면 제외가 통째로 헛돈다.** Down 회차에서 우리 주문은 책의
// 매도측에 있는데 제외는 매수측에 걸리므로, 아무것도 빠지지 않은 채 "우리
// 주문을 뺐다"고 믿게 된다.
func (v bookView) ourTicks(ours []exposedOrder) []exposedOrder {
	if !v.mirror || len(ours) == 0 {
		return ours
	}
	out := make([]exposedOrder, len(ours))
	for i, o := range ours {
		out[i] = exposedOrder{tick: v.full - o.tick, shares: o.shares}
	}
	return out
}

// quoteBook 는 우리 토큰 기준의 호가창을 만든다.
//
// ex 는 [bookView.ourTicks] 를 거친 뒤 [excludeOurs] 가 만든 제외 맵이다.
// 거울일 때는 우리 물량이 매도측에 있으므로 매도측에만 건다.
//
// 거울 뒤에 틱이 0 이하가 되면 **그 쪽 호가는 없는 것으로 친다.** 0 은
// 가격 0.00 이고, order.Tick.WeiPerShare 는 그 값에서 패닉한다 — 살아 있는
// 주문을 든 채 죽는 자리를 만들지 않는다.
func (v bookView) quoteBook(b bookReader, ex map[int64]ws.Shares) quote.Book {
	if !v.mirror {
		bid, hasBid := b.BestBid(ex)
		ask, hasAsk := b.BestAsk(nil)
		return quote.Book{
			BestBid: bid, HasBid: hasBid,
			BestAsk: ask, HasAsk: hasAsk,
			Precision: v.precision,
		}
	}

	// 거울. 우리 매수호가는 책의 매도측에서, 우리 매도호가는 책의 매수측에서
	// 온다. 제외는 우리 물량이 있는 매도측에만 건다.
	upAsk, hasUpAsk := b.BestAsk(ex)
	upBid, hasUpBid := b.BestBid(nil)

	qb := quote.Book{Precision: v.precision}
	if hasUpAsk {
		if t := v.full - upAsk; t > 0 {
			qb.BestBid, qb.HasBid = t, true
		}
	}
	if hasUpBid {
		if t := v.full - upBid; t > 0 {
			qb.BestAsk, qb.HasAsk = t, true
		}
	}
	return qb
}

// bookReader 는 [ws.Book] 중 이 파일이 쓰는 두 메서드다. 인터페이스로 받는
// 이유는 시험이 프레임을 흘려 넣지 않고도 호가창 모양을 만들 수 있게 하기
// 위해서다.
type bookReader interface {
	BestBid(exclude map[int64]ws.Shares) (tick int64, ok bool)
	BestAsk(exclude map[int64]ws.Shares) (tick int64, ok bool)
}
