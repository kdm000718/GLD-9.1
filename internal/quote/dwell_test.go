package quote

import (
	"testing"
	"time"
)

// 이 파일이 지키는 것 하나: **취소는 되돌릴 수 없다.**
//
// 재호가는 한 바퀴에 끝나지 않는다 — 취소하고, 거래소가 확인해 주면, 그
// 다음 바퀴에 다시 건다. 그 사이 51ms 에 군중이 제자리로 돌아오면 우리는
// 방금 버린 자리에 큐 맨 뒤로 다시 선다. 2026-08-11 실측에서 재호가
// 27,860건의 46.8% 가 그랬다.

const tick0 = time.Millisecond * 0

func at(ms int) time.Time { return time.Unix(1786400000, 0).Add(time.Duration(ms) * time.Millisecond) }

func bookAt(bid int64) Book {
	return Book{BestBid: bid, HasBid: true, Precision: 2}
}

func openAt(tick int64, placedMs int) Open {
	return Open{Tick: tick, Placed: at(placedMs), Live: true}
}

// TestDwellHoldsQueueThroughAFlicker 는 이 수정의 이유 그 자체다.
// 군중이 49→45 로 갔다가 51ms 만에 49 로 돌아온다. 드웰이 없으면 그 사이에
// 취소가 나가고 49 의 큐 위치를 잃는다.
func TestDwellHoldsQueueThroughAFlicker(t *testing.T) {
	d := &Dwell{Need: 600 * time.Millisecond}
	op := openAt(49, 0)

	// t=1000: 군중이 45 로 내려간다. 쿨다운(500ms)은 이미 지났다.
	got := Decide(bookAt(45), op, at(1000), 500*time.Millisecond, false, d)
	if got.Action != DoNothing {
		t.Fatalf("깜빡임 직후 = %v, want DoNothing — 취소는 되돌릴 수 없다 (%s)", got.Action, got.Why)
	}

	// t=1051: 군중이 49 로 돌아왔다. 우리는 여전히 49 에 큐를 지키고 있다.
	got = Decide(bookAt(49), op, at(1051), 500*time.Millisecond, false, d)
	if got.Action != DoNothing {
		t.Fatalf("복귀 후 = %v, want DoNothing (동일가) — %s", got.Action, got.Why)
	}
}

// TestDwellStillRepricesOnARealMove 는 반대 방향을 고정한다. 잡음을 거르느라
// 진짜 이동까지 막으면 군중을 따라가지 못하고 호가창 바닥에 홀로 남는다.
func TestDwellStillRepricesOnARealMove(t *testing.T) {
	d := &Dwell{Need: 600 * time.Millisecond}
	op := openAt(49, 0)

	if got := Decide(bookAt(45), op, at(1000), 500*time.Millisecond, false, d); got.Action != DoNothing {
		t.Fatalf("이동 직후 = %v, want DoNothing", got.Action)
	}
	// 45 가 600ms 를 버텼다.
	got := Decide(bookAt(45), op, at(1600), 500*time.Millisecond, false, d)
	if got.Action != Reprice || got.Tick != 45 {
		t.Fatalf("굳은 뒤 = %v tick=%d, want Reprice 45 — %s", got.Action, got.Tick, got.Why)
	}
}

// TestDwellBoundaryIsInclusive 는 경계 규약을 쿨다운과 맞춘다.
func TestDwellBoundaryIsInclusive(t *testing.T) {
	d := &Dwell{Need: 600 * time.Millisecond}
	op := openAt(49, 0)
	Decide(bookAt(45), op, at(1000), 0, false, d)
	if got := Decide(bookAt(45), op, at(1600), 0, false, d); got.Action != Reprice {
		t.Fatalf("경과 == Need 인데 %v — 경계는 통과다 (%s)", got.Action, got.Why)
	}
	if got := Decide(bookAt(45), op, at(1599), 0, false, d); got.Action != DoNothing {
		t.Fatalf("경과 < Need 인데 %v", got.Action)
	}
}

// TestDwellRestartsWhenTargetChangesAgain 은 목표가 다시 바뀌면 나이가 0 으로
// 돌아가는지 본다. 누적으로 세면 흔들리는 내내 조금씩 쌓여 결국 통과한다 —
// 그러면 드웰이 있으나 마나다.
func TestDwellRestartsWhenTargetChangesAgain(t *testing.T) {
	d := &Dwell{Need: 600 * time.Millisecond}
	op := openAt(49, 0)
	Decide(bookAt(45), op, at(0), 0, false, d)   // 45 시작
	Decide(bookAt(45), op, at(500), 0, false, d) // 500ms 유지
	Decide(bookAt(44), op, at(550), 0, false, d) // 44 로 바뀜 → 나이 0
	if got := Decide(bookAt(44), op, at(1000), 0, false, d); got.Action != DoNothing {
		t.Fatalf("450ms 밖에 안 됐는데 %v — 나이가 이어졌다 (%s)", got.Action, got.Why)
	}
	if got := Decide(bookAt(44), op, at(1150), 0, false, d); got.Action != Reprice {
		t.Fatalf("600ms 지났는데 %v (%s)", got.Action, got.Why)
	}
}

// TestDwellNeverDelaysTheFirstOrder 는 신규 주문이 드웰에 걸리지 않는지 본다.
// 걸린 주문이 없으면 잃을 큐 위치도 없고, 회차 초반을 비우면 그 회차의
// 엣지를 통째로 버린다.
func TestDwellNeverDelaysTheFirstOrder(t *testing.T) {
	d := &Dwell{Need: 10 * time.Second}
	got := Decide(bookAt(45), Open{}, at(0), 500*time.Millisecond, false, d)
	if got.Action != Place || got.Tick != 45 {
		t.Fatalf("신규 = %v tick=%d, want Place 45 — 드웰은 신규를 막지 않는다 (%s)",
			got.Action, got.Tick, got.Why)
	}
}

// TestDwellObservesEveryLoop 는 쿨다운에 막혀 있는 동안에도 나이가 자라는지
// 본다. 재호가 직전에만 세면 `since` 가 전진하지 않아 조건이 영영 성립하지
// 않고, 봇은 군중을 아예 따라가지 못한다.
func TestDwellObservesEveryLoop(t *testing.T) {
	d := &Dwell{Need: 600 * time.Millisecond}
	op := openAt(49, 900) // 쿨다운 500ms → t=1400 까지 막힌다

	// 쿨다운 구간에서 목표 45 를 계속 본다.
	for ms := 1000; ms <= 1400; ms += 50 {
		Decide(bookAt(45), op, at(ms), 500*time.Millisecond, false, d)
	}
	// 45 는 t=1000 부터 굳었으므로 t=1600 이면 600ms 다.
	got := Decide(bookAt(45), op, at(1600), 500*time.Millisecond, false, d)
	if got.Action != Reprice {
		t.Fatalf("= %v — 쿨다운 동안 나이가 자라지 않았다 (%s)", got.Action, got.Why)
	}
}

// TestNilDwellKeepsOldBehaviour 는 nil 이 "검사 없음" 인지 본다. 배선이
// 빠졌을 때 봇이 멈추는 것보다는 예전처럼 도는 편이 낫다.
func TestNilDwellKeepsOldBehaviour(t *testing.T) {
	got := Decide(bookAt(45), openAt(49, 0), at(1000), 500*time.Millisecond, false, nil)
	if got.Action != Reprice {
		t.Fatalf("nil dwell = %v, want Reprice (%s)", got.Action, got.Why)
	}
}

// TestZeroNeedIsNoDwell 은 Need=0 규약을 쿨다운과 맞춘다.
func TestZeroNeedIsNoDwell(t *testing.T) {
	got := Decide(bookAt(45), openAt(49, 0), at(1000), 500*time.Millisecond, false, &Dwell{Need: tick0})
	if got.Action != Reprice {
		t.Fatalf("Need=0 = %v, want Reprice (%s)", got.Action, got.Why)
	}
}

// TestDwellDoesNotBlockCancelOnly 는 stale·목표없음 경로가 드웰과 무관한지
// 본다. 그 둘은 "걸린 것을 치워야 하는" 상태이고, 미루면 오래된 호가창을
// 보고 걸어 둔 주문이 살아남는다.
func TestDwellDoesNotBlockCancelOnly(t *testing.T) {
	d := &Dwell{Need: 10 * time.Second}
	if got := Decide(bookAt(45), openAt(49, 0), at(0), 0, true, d); got.Action != CancelOnly {
		t.Fatalf("stale = %v, want CancelOnly (%s)", got.Action, got.Why)
	}
	// 매수호가가 없는 것만으로는 목표가 사라지지 않는다 — 상한 폴백이 있다.
	// 진짜로 목표가 없는 것은 관통 방지가 틱을 0 으로 밀어낼 때다.
	noTarget := Book{HasBid: true, BestBid: 1, HasAsk: true, BestAsk: 1, Precision: 2}
	if got := Decide(noTarget, openAt(49, 0), at(0), 0, false, d); got.Action != CancelOnly {
		t.Fatalf("목표 없음 = %v, want CancelOnly (%s)", got.Action, got.Why)
	}
}
