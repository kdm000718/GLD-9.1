package exec

import (
	"fmt"

	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
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
// # 지금 이 파일이 하는 일 — 판단이 아니라 기록이다
//
// 2026-08-12 에 전략이 바뀌었다. 이 봇은 더 이상 군중을 따라가지도, 관통을
// 피하지도 않는다. 회차마다 [LimitPrice] 한 가격에 한 번만 건다(exec.go 의
// "회차마다 한 번, 한 가격" 참고). 그래서 여기서 만든 호가창은 **어떤 결정에도
// 들어가지 않는다** — 주문을 낸 순간의 시장 모습을 로그 한 줄로 남기는 데만
// 쓴다.
//
// 그럼에도 거울이 남아 있어야 하는 이유: 그 로그가 Down 회차에서 틀리면
// "0.47 이 좋은 가격이었나"를 사후에 판단할 근거가 통째로 사라진다. 위의
// 사고는 정확히 그 착각에서 나왔고, 기록이 같은 착각을 물려받으면 안 된다.
//
// **이 파일의 값을 읽어 가격이나 수량을 바꾸는 경로가 생기면 그것은 전략
// 변경이다.** exec_test.go 의 TestBookNeverChangesTheOrder 가 그것을 막는다.

// bookView 는 회차 방향에 맞춰 오더북을 읽는 방법이다.
//
// 값 타입이고 회차 시작에 한 번 만든다. 상태가 없다 — 매 바퀴 새로 만들어도
// 결과가 같아야 하고, 그래야 방향이 회차 도중에 새는 자리가 없다.
type bookView struct {
	// mirror 가 참이면 책은 우리 토큰의 반대편 기준이다(Down 회차).
	mirror bool
	// full 은 가격 1.00 의 틱 수다. 거울은 이 값에서 빼서 만든다.
	full int64
	// precision 은 틱을 사람이 읽는 가격으로 되돌릴 때 쓴다.
	precision int
}

// newBookView 는 방향과 정밀도로 읽기 방법을 정한다.
//
// 방향 문자열을 정확히 두 철자만 받는다. 모르는 값이면 **거울을 걸지 않는
// 쪽**이 아니라 에러다 — 조용히 Up 으로 읽으면 Down 회차의 기록 전체가 엉뚱한
// 책을 가리키게 되고, 그게 바로 이 파일이 생긴 이유다.
func newBookView(direction string, precision int) (bookView, error) {
	switch direction {
	case ledger.OutcomeUp:
		return bookView{mirror: false, full: order.Full(precision), precision: precision}, nil
	case ledger.OutcomeDown:
		return bookView{mirror: true, full: order.Full(precision), precision: precision}, nil
	}
	return bookView{}, tokenDirectionError(direction)
}

// sideBook 는 **우리가 사려는 토큰 기준**의 최우선 호가다.
//
// Has* 가 false 면 대응하는 틱은 의미가 없다.
type sideBook struct {
	BestBid int64
	HasBid  bool
	BestAsk int64
	HasAsk  bool
}

// sides 는 우리 토큰 기준의 최우선 호가를 읽는다.
//
// 우리 주문을 빼지 않는다. 뺄 이유가 없다 — 이 값은 아무것도 결정하지 않고,
// 우리가 주문을 내는 순간은 회차당 한 번뿐이라 그 시점에 우리 물량은 책에
// 없다.
//
// 거울 뒤에 틱이 0 이하가 되면 **그 쪽 호가는 없는 것으로 친다.** 0 은
// 가격 0.00 이고, 그 값을 가격으로 되돌려 찍으면 로그가 거짓말을 한다.
func (v bookView) sides(b bookReader) sideBook {
	if !v.mirror {
		bid, hasBid := b.BestBid(nil)
		ask, hasAsk := b.BestAsk(nil)
		return sideBook{BestBid: bid, HasBid: hasBid, BestAsk: ask, HasAsk: hasAsk}
	}

	// 거울. 우리 매수호가는 책의 매도측에서, 우리 매도호가는 책의 매수측에서 온다.
	upAsk, hasUpAsk := b.BestAsk(nil)
	upBid, hasUpBid := b.BestBid(nil)

	var s sideBook
	if hasUpAsk {
		if t := v.full - upAsk; t > 0 {
			s.BestBid, s.HasBid = t, true
		}
	}
	if hasUpBid {
		if t := v.full - upBid; t > 0 {
			s.BestAsk, s.HasAsk = t, true
		}
	}
	return s
}

// label 은 틱을 사람이 읽는 가격으로 만든다. 없으면 "-" 다 — 0 으로 찍으면
// "가격 0.00 에 호가가 있다"로 읽힌다.
func (v bookView) label(tick int64, has bool) string {
	if !has {
		return "-"
	}
	return fmt.Sprintf("%v", order.NewTick(tick, v.precision).Float())
}

// bookReader 는 [ws.Book] 중 이 파일이 쓰는 두 메서드다. 인터페이스로 받는
// 이유는 시험이 프레임을 흘려 넣지 않고도 호가창 모양을 만들 수 있게 하기
// 위해서다.
type bookReader interface {
	BestBid(exclude map[int64]ws.Shares) (tick int64, ok bool)
	BestAsk(exclude map[int64]ws.Shares) (tick int64, ok bool)
}
