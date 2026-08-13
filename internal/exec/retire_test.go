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
//
// 2026-08-14 에 회차당 다리가 둘이 되면서 이 파일의 무게가 늘었다 — 예전의
// "하나뿐일 때만 물러난다" 규칙을 그대로 뒀다면 **아무것도 물러나지 못해**
// 위 사고가 회차마다 재현됐을 것이다.

func TestFullFillRetiresTheOrder(t *testing.T) {
	st := &roundState{}
	st.live = []*openOrder{{id: "o1", shares: 9, notional: 4.41, filledBefore: 0}}

	// 9주 전량 체결.
	st.filledShares = 9
	st.filledNotional = 4.41
	st.retireFullyFilled()

	if len(st.live) != 0 {
		t.Fatal("전량 체결됐는데 미체결로 남아 있다 — 노출이 두 배로 세어진다")
	}
	x := st.exposure()
	if got := x.FilledNotional + x.OpenNotional + x.PendingCancel; got != 4.41 {
		t.Fatalf("노출 합계 %v, 기대 4.41 (체결분만)", got)
	}
}

func TestPartialFillKeepsTheOrder(t *testing.T) {
	st := &roundState{}
	st.live = []*openOrder{{id: "o1", shares: 9, notional: 4.41, filledBefore: 0}}

	st.filledShares = 4 // 9주 중 4주만
	st.filledNotional = 1.96
	st.retireFullyFilled()

	if len(st.live) == 0 {
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
	st.live = []*openOrder{{id: "o2", shares: 8, notional: 3.92, filledBefore: st.filledShares}}
	st.retireFullyFilled()
	if len(st.live) == 0 {
		t.Fatal("새 주문이 한 주도 안 찼는데 물러났다 — 회차 누적을 기준점 없이 비교했다")
	}

	// 이 주문에 8주가 차면 그때 물러난다.
	st.filledShares += 8
	st.retireFullyFilled()
	if len(st.live) != 0 {
		t.Fatal("이 주문이 전량 찼는데 남아 있다")
	}
}

// TestNeverRetiresWhileCancelsAreUnconfirmed 는 모호한 경우를 건드리지 않는
// 다는 계약이다. 체결에는 주문 식별자가 없으므로(Fills 문서) 취소 미확인
// 주문이 섞여 있으면 어느 것이 찼는지 확정할 수 없다. 잘못 짚으면 살아 있는
// 주문을 추적에서 놓치고, 잊힌 매수 주문은 체결된다.
func TestNeverRetiresWhileCancelsAreUnconfirmed(t *testing.T) {
	st := &roundState{}
	st.live = []*openOrder{{id: "o2", shares: 9, notional: 4.41, filledBefore: 0}}
	st.pending = []*openOrder{{id: "o1", shares: 9, notional: 4.41}}

	st.filledShares = 9
	st.retireFullyFilled()

	if len(st.live) == 0 {
		t.Fatal("취소 미확인 주문이 있는데 물러났다 — 어느 주문이 찼는지 모르는 상태다")
	}
}

// TestRetireIsIdempotent 는 매 바퀴 불려도 안전한지 본다. absorbFills 가
// 회차당 수천 번 부른다.
func TestRetireIsIdempotent(t *testing.T) {
	st := &roundState{}
	st.live = []*openOrder{{id: "o1", shares: 9, notional: 4.41}}
	st.filledShares = 9
	for i := 0; i < 5; i++ {
		st.retireFullyFilled()
	}
	if len(st.live) != 0 {
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
	st.live = []*openOrder{{id: "o1", shares: 0, notional: 0}}
	st.retireFullyFilled()
	if len(st.live) == 0 {
		t.Fatal("수량 0 짜리를 물러나게 했다")
	}
}

// ---------------------------------------------------------------------------
// 다리가 둘일 때 (2026-08-14)
// ---------------------------------------------------------------------------

// TestTakerLegRetiresWhileMakerLegStays 가 이 변경의 핵심 사례다.
//
// 테이커 다리는 관통이라 즉시 전량 체결되고, 메이커 다리는 시장이 0.46 까지
// 내려와야 체결된다. 실전에서 압도적으로 흔한 조합이 "테이커만 찼다" 이고,
// 그때 테이커의 명목이 미체결로도 계속 세어지면 노출이 cap 을 넘는다.
func TestTakerLegRetiresWhileMakerLegStays(t *testing.T) {
	st := &roundState{}
	taker := &openOrder{id: "taker", shares: 4, notional: 2.12, filledBefore: 0} // 4주 @0.53
	maker := &openOrder{id: "maker", shares: 4, notional: 1.84, filledBefore: 0} // 4주 @0.46
	st.live = []*openOrder{taker, maker}

	// 테이커만 찼다.
	st.filledShares = 4
	st.filledNotional = 2.12
	st.retireFullyFilled()

	if len(st.live) != 1 {
		t.Fatalf("살아 있는 주문이 %d건, 기대 1건(메이커만)", len(st.live))
	}
	if st.live[0].id != "maker" {
		t.Fatalf("남은 주문이 %q 다 — 테이커가 아니라 메이커가 남아야 한다", st.live[0].id)
	}
	x := st.exposure()
	total := x.FilledNotional + x.OpenNotional + x.PendingCancel
	if total > 2.12+1.84+1e-9 {
		t.Fatalf("노출 합계 %.4f — 체결된 테이커를 미체결로도 셌다", total)
	}
}

// 두 다리가 모두 차면 둘 다 물러난다.
func TestBothLegsRetireWhenEverythingFills(t *testing.T) {
	st := &roundState{}
	st.live = []*openOrder{
		{id: "taker", shares: 4, notional: 2.12, filledBefore: 0},
		{id: "maker", shares: 4, notional: 1.84, filledBefore: 0},
	}
	st.filledShares = 8
	st.filledNotional = 3.96
	st.retireFullyFilled()

	if len(st.live) != 0 {
		t.Fatalf("두 다리 모두 찼는데 %d건이 남았다", len(st.live))
	}
}

// TestOneFillNeverRetiresTwoLegs 는 **한 번의 체결이 두 다리를 동시에
// 물러나게 하지 않는지** 본다. 앞 다리에 귀속한 주수를 빼지 않으면 4주 체결이
// 4주짜리 두 다리를 모두 만족시킨다 — 그러면 아직 살아 있는 메이커 주문을
// 아무도 취소하지 않고, 잊힌 매수 주문은 체결된다.
func TestOneFillNeverRetiresTwoLegs(t *testing.T) {
	st := &roundState{}
	st.live = []*openOrder{
		{id: "taker", shares: 4, notional: 2.12, filledBefore: 0},
		{id: "maker", shares: 4, notional: 1.84, filledBefore: 0},
	}
	st.filledShares = 4
	st.retireFullyFilled()

	if len(st.live) != 1 {
		t.Fatalf("4주 체결로 %d건이 물러났다 — 앞 다리 몫을 빼지 않았다", 2-len(st.live))
	}
}

// 앞 다리가 덜 차면 뒤 다리도 보지 않는다. 체결에 식별자가 없으므로 남은
// 체결이 어느 쪽 것인지 말할 근거가 없다 — 짚지 않는 편이 안전하다.
func TestLaterLegWaitsForTheEarlierOne(t *testing.T) {
	st := &roundState{}
	st.live = []*openOrder{
		{id: "taker", shares: 10, notional: 5.30, filledBefore: 0},
		{id: "maker", shares: 2, notional: 0.92, filledBefore: 0},
	}
	// 2주만 찼다 — 메이커 크기와 정확히 같지만, 테이커 것일 수도 있다.
	st.filledShares = 2
	st.retireFullyFilled()

	if len(st.live) != 2 {
		t.Fatalf("살아 있는 주문이 %d건 — 앞 다리가 덜 찼는데 뒤 다리를 물러나게 했다", len(st.live))
	}
}

// TestAbsorbFillsRetiresThroughTheWiring 은 **배선을 지나는** 시험이다.
//
// 위 시험들은 retireFullyFilled 를 직접 부른다. 그래서 absorbFills 안의
// 호출 한 줄을 지워도 전부 통과한다 — 실제로 변이 시험에서 그랬다. 이
// 봇에서 없어서 사고가 난 것은 함수가 아니라 **그 함수를 부르는 자리**였다.
func TestAbsorbFillsRetiresThroughTheWiring(t *testing.T) {
	st := &roundState{}
	st.live = []*openOrder{{id: "o1", shares: 9, notional: 4.41, filledBefore: 0}}

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

	if len(st.live) != 0 {
		t.Fatal("absorbFills 가 전량 체결된 주문을 물러나게 하지 않았다 — 노출이 두 배로 세어진다")
	}
	x := st.exposure()
	total := x.FilledNotional + x.OpenNotional + x.PendingCancel
	if total > 4.42 {
		t.Fatalf("노출 합계 %.4f — 같은 돈을 두 번 셌다(실측 8.82 대 cap 4.56)", total)
	}
}
