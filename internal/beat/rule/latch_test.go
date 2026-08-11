package rule

import (
	"reflect"
	"testing"
	"time"
)

func at(sec int) time.Time { return time.Unix(1786000000+int64(sec), 0).UTC() }

// 같은 알람은 한 번만 운다. 3초마다 같은 메시지가 오면 사람은 알림을 끄고,
// 그 순간 감시가 통째로 사라진다.
func TestLatchFiresOnce(t *testing.T) {
	l := NewLatch(0)
	f := []Finding{{Key: "exposure", Level: Crit, Message: "위반"}}

	fire, _ := l.Step(f, at(0))
	if len(fire) != 1 {
		t.Fatalf("첫 발생에 %d개, want 1", len(fire))
	}
	for i := 1; i < 5; i++ {
		if fire, _ := l.Step(f, at(i)); len(fire) != 0 {
			t.Errorf("%d초에 또 울었다: %+v", i, fire)
		}
	}
}

// 사라지면 복구를 한 번 알리고, 다시 나면 다시 운다.
func TestLatchResolvesAndRefires(t *testing.T) {
	l := NewLatch(0)
	f := []Finding{{Key: "ws_data", Level: Crit}}

	l.Step(f, at(0))
	_, resolved := l.Step(nil, at(1))
	if !reflect.DeepEqual(resolved, []string{"ws_data"}) {
		t.Fatalf("복구 = %v, want [ws_data]", resolved)
	}
	if _, resolved := l.Step(nil, at(2)); len(resolved) != 0 {
		t.Errorf("복구가 두 번 났다: %v", resolved)
	}
	if fire, _ := l.Step(f, at(3)); len(fire) != 1 {
		t.Error("재발에 울지 않았다")
	}
}

// 지속시간을 넘겨야 운다. 순간 스파이크로 울면 4시간 리포트를 늘린 의미가 없다.
func TestLatchRequiresSustain(t *testing.T) {
	l := NewLatch(10 * time.Second)
	f := []Finding{{Key: "can_arm", Level: Warn}}

	if fire, _ := l.Step(f, at(0)); len(fire) != 0 {
		t.Error("첫 관측에 즉시 울었다")
	}
	if fire, _ := l.Step(f, at(9)); len(fire) != 0 {
		t.Error("9초에 울었다")
	}
	if fire, _ := l.Step(f, at(10)); len(fire) != 1 {
		t.Error("정확히 10초(임계)에 울지 않았다")
	}
}

// 지속 중에 끊기면 시계가 리셋된다 — "연속으로" 유지된 것만 지속이다.
func TestLatchSustainResets(t *testing.T) {
	l := NewLatch(10 * time.Second)
	f := []Finding{{Key: "can_arm", Level: Warn}}

	l.Step(f, at(0))
	l.Step(nil, at(5)) // 끊김
	l.Step(f, at(6))   // 다시 시작
	if fire, _ := l.Step(f, at(12)); len(fire) != 0 {
		t.Error("리셋됐어야 하는데 12초에 울었다")
	}
	if fire, _ := l.Step(f, at(16)); len(fire) != 1 {
		t.Error("리셋 뒤 16초(6+10)에 울지 않았다")
	}
}

// Crit 은 지속을 기다리지 않는다. 노출 위반은 지금 돈이 나가는 중이다.
func TestLatchCriticalFiresImmediately(t *testing.T) {
	l := NewLatch(10 * time.Second)
	if fire, _ := l.Step([]Finding{{Key: "exposure", Level: Crit}}, at(0)); len(fire) != 1 {
		t.Error("Crit 이 지속을 기다렸다")
	}
}

// 등급이 올라가면 다시 운다 — Warn 으로 울고 조용해진 사이에 Crit 이 되면
// 사람은 여전히 Warn 인 줄 안다.
func TestLatchRefiresOnEscalation(t *testing.T) {
	l := NewLatch(0)
	l.Step([]Finding{{Key: "skip:equity", Level: Warn}}, at(0))

	fire, _ := l.Step([]Finding{{Key: "skip:equity", Level: Crit}}, at(1))
	if len(fire) != 1 || fire[0].Level != Crit {
		t.Fatalf("승격에 %+v, want Crit 1건", fire)
	}
	// 승격 뒤에는 다시 조용해야 한다.
	if fire, _ := l.Step([]Finding{{Key: "skip:equity", Level: Crit}}, at(2)); len(fire) != 0 {
		t.Errorf("승격 뒤에 또 울었다: %+v", fire)
	}
}

// 등급이 내려가는 것으로는 울지 않는다. Crit 으로 이미 알렸는데 Warn 이
// 되었다고 다시 울면, 완화가 새 사고처럼 보인다.
func TestLatchDoesNotRefireOnDeescalation(t *testing.T) {
	l := NewLatch(0)
	l.Step([]Finding{{Key: "skip:equity", Level: Crit}}, at(0))
	if fire, _ := l.Step([]Finding{{Key: "skip:equity", Level: Warn}}, at(1)); len(fire) != 0 {
		t.Errorf("강등에 울었다: %+v", fire)
	}
}

// 울린 적 없이 사라진 조건(지속 문턱을 못 넘긴 스파이크)은 복구를 알리지
// 않는다 — 사람이 본 적 없는 문제의 "복구"는 소음이다.
func TestLatchDoesNotResolveWhatNeverFired(t *testing.T) {
	l := NewLatch(10 * time.Second)
	l.Step([]Finding{{Key: "can_arm", Level: Warn}}, at(0))
	if _, resolved := l.Step(nil, at(3)); len(resolved) != 0 {
		t.Errorf("울린 적 없는 조건의 복구가 났다: %v", resolved)
	}
}

// 여러 키가 동시에 복구되면 순서가 결정적이어야 한다. 맵 순회 순서가 그대로
// 나가면 같은 상황에서 알림 순서가 매번 달라져 로그 대조가 어려워진다.
func TestLatchResolvedIsSorted(t *testing.T) {
	l := NewLatch(0)
	l.Step([]Finding{
		{Key: "ws_data", Level: Crit},
		{Key: "consts", Level: Crit},
		{Key: "exposure", Level: Crit},
	}, at(0))

	_, resolved := l.Step(nil, at(1))
	want := []string{"consts", "exposure", "ws_data"}
	if !reflect.DeepEqual(resolved, want) {
		t.Errorf("복구 = %v, want %v", resolved, want)
	}
}

// 여러 알람이 서로 독립적으로 래치된다. 하나가 울었다고 다른 것이 묻히면
// 안 된다.
func TestLatchKeysAreIndependent(t *testing.T) {
	l := NewLatch(0)
	a := Finding{Key: "exposure", Level: Crit}
	b := Finding{Key: "ratelimit", Level: Crit}

	if fire, _ := l.Step([]Finding{a}, at(0)); len(fire) != 1 {
		t.Fatalf("a 가 울지 않았다")
	}
	fire, _ := l.Step([]Finding{a, b}, at(1))
	if len(fire) != 1 || fire[0].Key != "ratelimit" {
		t.Errorf("두 번째 스텝에 %+v, want ratelimit 1건만", fire)
	}
}

// 래치가 상태를 무한히 쌓지 않는다. 사라진 키는 지워져야 한다 — 회차마다
// 다른 키가 스쳐 지나가는 봇에서 이것이 없으면 맵이 계속 자란다.
func TestLatchForgetsResolvedKeys(t *testing.T) {
	l := NewLatch(0)
	for i := 0; i < 50; i++ {
		l.Step([]Finding{{Key: "skip:transient", Level: Crit}}, at(i*2))
		l.Step(nil, at(i*2+1))
	}
	if n := len(l.state); n != 0 {
		t.Errorf("래치에 %d개가 남았다 — 복구된 키가 지워지지 않는다", n)
	}
}
