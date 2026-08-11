package exec

import (
	"context"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/live"
)

// 이 파일이 지키는 것: **같은 돈을 두 번 세지 않는다.**
//
// 2026-08-11 첫 실체결에서 체결 4.41 + 미체결 4.41 = 8.82 가 cap 4.56 을
// 넘었다. 체결이 들어와도 그 주문이 st.live 에 남아 있었기 때문이다.
// 그리고 회차 종료 때 이미 사라진 주문을 취소하려다 미확인으로 남아,
// 회차가 "사람이 확인해야 한다" 로 끝났다.

func TestFullFillRetiresTheOrder(t *testing.T) {
	st := &roundState{}
	st.live = &openOrder{id: "o1", shares: 9, notional: 4.41, filledBefore: 0}

	// 9주 전량 체결.
	st.filledShares = 9
	st.filledNotional = 4.41
	st.retireFullyFilled()

	if st.live != nil {
		t.Fatal("전량 체결됐는데 미체결로 남아 있다 — 노출이 두 배로 세어진다")
	}
	x := st.exposure()
	if got := x.FilledNotional + x.OpenNotional + x.PendingCancel; got != 4.41 {
		t.Fatalf("노출 합계 %v, 기대 4.41 (체결분만)", got)
	}
}

func TestPartialFillKeepsTheOrder(t *testing.T) {
	st := &roundState{}
	st.live = &openOrder{id: "o1", shares: 9, notional: 4.41, filledBefore: 0}

	st.filledShares = 4 // 9주 중 4주만
	st.filledNotional = 1.96
	st.retireFullyFilled()

	if st.live == nil {
		t.Fatal("부분 체결인데 물러났다 — 남은 5주를 아무도 취소하지 않는다")
	}
}

// TestBaselineIsPerOrderNotPerRound 가 이 수정의 핵심이다.
//
// 회차 누적만 보면, 앞선 주문이 9주 체결되고 사라진 뒤에 낸 8주짜리 새
// 주문이 **한 주도 안 찼는데** 전량 체결로 보인다. 그러면 살아 있는 주문을
// 추적에서 놓치고, 아무도 그것을 취소하지 않는다.
func TestBaselineIsPerOrderNotPerRound(t *testing.T) {
	st := &roundState{filledShares: 9, filledNotional: 4.41} // 앞선 주문의 흔적

	// 새 주문은 그 시점의 누적을 기준점으로 들고 태어난다.
	st.live = &openOrder{id: "o2", shares: 8, notional: 3.92, filledBefore: st.filledShares}
	st.retireFullyFilled()
	if st.live == nil {
		t.Fatal("새 주문이 한 주도 안 찼는데 물러났다 — 회차 누적을 기준점 없이 비교했다")
	}

	// 이 주문에 8주가 차면 그때 물러난다.
	st.filledShares += 8
	st.retireFullyFilled()
	if st.live != nil {
		t.Fatal("이 주문이 전량 찼는데 남아 있다")
	}
}

// TestNeverRetiresWhileCancelsAreUnconfirmed 는 모호한 경우를 건드리지 않는
// 다는 계약이다. 체결에는 주문 식별자가 없으므로(Fills 문서) 여러 건을 들고
// 있을 때 어느 것이 찼는지 확정할 수 없다. 잘못 짚으면 살아 있는 주문을
// 추적에서 놓치고, 잊힌 매수 주문은 체결된다.
func TestNeverRetiresWhileCancelsAreUnconfirmed(t *testing.T) {
	st := &roundState{}
	st.live = &openOrder{id: "o2", shares: 9, notional: 4.41, filledBefore: 0}
	st.pending = []*openOrder{{id: "o1", shares: 9, notional: 4.41}}

	st.filledShares = 9
	st.retireFullyFilled()

	if st.live == nil {
		t.Fatal("취소 미확인 주문이 있는데 물러났다 — 어느 주문이 찼는지 모르는 상태다")
	}
}

// TestRetireIsIdempotent 는 매 바퀴 불려도 안전한지 본다. absorbFills 가
// 회차당 수천 번 부른다.
func TestRetireIsIdempotent(t *testing.T) {
	st := &roundState{}
	st.live = &openOrder{id: "o1", shares: 9, notional: 4.41}
	st.filledShares = 9
	for i := 0; i < 5; i++ {
		st.retireFullyFilled()
	}
	if st.live != nil {
		t.Fatal("물러나지 않았다")
	}
	if st.filledShares != 9 {
		t.Fatalf("체결 누적이 %v 로 바뀌었다 — 이 함수는 그것을 건드리지 않는다", st.filledShares)
	}
}

// 수량이 0 이하인 주문은 건드리지 않는다. 0 >= 0 으로 언제나 참이 되어
// 갓 낸 주문이 곧바로 물러나는 일을 막는다.
func TestZeroShareOrderIsNotRetired(t *testing.T) {
	st := &roundState{}
	st.live = &openOrder{id: "o1", shares: 0, notional: 0}
	st.retireFullyFilled()
	if st.live == nil {
		t.Fatal("수량 0 짜리를 물러나게 했다")
	}
}

// TestAbsorbFillsRetiresThroughTheWiring 은 **배선을 지나는** 시험이다.
//
// 위 시험들은 retireFullyFilled 를 직접 부른다. 그래서 absorbFills 안의
// 호출 한 줄을 지워도 전부 통과한다 — 실제로 변이 시험에서 그랬다. 이
// 봇에서 없어서 사고가 난 것은 함수가 아니라 **그 함수를 부르는 자리**였다.
func TestAbsorbFillsRetiresThroughTheWiring(t *testing.T) {
	st := &roundState{}
	st.live = &openOrder{id: "o1", shares: 9, notional: 4.41, filledBefore: 0}

	r := &Runner{
		Fills: &fakeFills{pollFn: func(n int) ([]ledger.Fill, error) {
			if n > 0 {
				return nil, nil
			}
			return []ledger.Fill{{
				RoundStart: 1786461600, MarketID: 1302510,
				Outcome: ledger.OutcomeUp, Shares: 9, PriceUSD: 0.49,
			}}, nil
		}},
		Ledger: &recordingLedger{},
	}
	if err := r.absorbFills(context.Background(), live.Round{}, st); err != nil {
		t.Fatalf("absorbFills: %v", err)
	}

	if st.live != nil {
		t.Fatal("absorbFills 가 전량 체결된 주문을 물러나게 하지 않았다 — 노출이 두 배로 세어진다")
	}
	x := st.exposure()
	total := x.FilledNotional + x.OpenNotional + x.PendingCancel
	if total > 4.42 {
		t.Fatalf("노출 합계 %.4f — 같은 돈을 두 번 셌다(실측 8.82 대 cap 4.56)", total)
	}
}
