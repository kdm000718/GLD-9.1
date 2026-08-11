package rule

import (
	"sort"
	"time"
)

// Latch 는 알람의 에지를 잡는다 — 같은 조건이 3초마다 우는 것을 막고,
// 사라지면 복구를 한 번 알린다. GLD-7 하트비트 모니터의 `Changed` 패턴이다.
//
// **알람 피로가 감시 장치의 실패 모드다.** 같은 알람이 3초마다 오면 사람은
// 알림을 끄고, 그 순간 이 저장소의 감시가 통째로 사라진다. 정기 푸시를
// 4시간으로 늘렸으므로 더욱 그렇다 — 알람이 사실상 유일한 감지 경로다.
//
// 순수하다. 현재 시각은 인자로 받는다.
type Latch struct {
	sustain time.Duration
	state   map[string]*latchEntry
}

type latchEntry struct {
	// since 는 이 조건을 **연속으로** 보기 시작한 시각이다. 중간에 한 번이라도
	// 끊기면 항목 자체가 지워지므로 다시 셈이 시작된다.
	since time.Time
	fired bool
	level Level
}

// NewLatch 는 지속시간 문턱을 받는다. 0 이면 첫 관측에 바로 운다.
func NewLatch(sustain time.Duration) *Latch {
	return &Latch{sustain: sustain, state: map[string]*latchEntry{}}
}

// Step 은 이번 판정을 넣고, 새로 울릴 것과 복구된 키를 돌려준다.
//
// **Crit 은 지속을 기다리지 않는다.** 노출 불변식 위반은 지금 이 순간 한도를
// 넘겨 베팅하고 있다는 뜻이고, 그것을 30초 확인하는 동안 더 나간다. 반면
// Warn 은 순간 스파이크가 흔하므로 지속을 본다.
//
// 등급이 올라가면 다시 운다. Warn 으로 울고 조용해진 사이에 Crit 이 되면
// 사람은 여전히 Warn 인 줄 안다 — 등급은 사람이 지금 일어나야 하는지를
// 정하는 값이므로 그 변화가 묻히면 안 된다.
//
// resolved 는 정렬해 돌려준다. 맵 순회 순서가 그대로 나가면 같은 상황에서
// 알림 순서가 매번 달라져 로그 대조가 어려워진다.
func (l *Latch) Step(fs []Finding, now time.Time) (fire []Finding, resolved []string) {
	seen := make(map[string]bool, len(fs))
	for _, f := range fs {
		seen[f.Key] = true
		e, ok := l.state[f.Key]
		if !ok {
			e = &latchEntry{since: now, level: f.Level}
			l.state[f.Key] = e
		}
		if f.Level > e.level {
			e.level, e.fired = f.Level, false
		}
		if e.fired {
			continue
		}
		if f.Level == Crit || now.Sub(e.since) >= l.sustain {
			e.fired = true
			fire = append(fire, f)
		}
	}
	for key, e := range l.state {
		if seen[key] {
			continue
		}
		// 울린 적 없이 사라진 조건(지속 문턱을 못 넘긴 스파이크)은 복구를
		// 알리지 않는다 — 사람이 본 적 없는 문제의 "복구"는 소음이다.
		if e.fired {
			resolved = append(resolved, key)
		}
		delete(l.state, key)
	}
	sort.Strings(resolved)
	return fire, resolved
}
