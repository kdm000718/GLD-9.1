package main

import (
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

// healthySnapshot 은 아무 알람도 나지 않아야 하는 스냅샷이다.
// 포인터를 돌려주는 이유: 테스트가 한 곳만 고쳐 쓰는 일이 잦다.
func healthySnapshot() *beat.Snapshot {
	now := time.Now().UTC()
	return &beat.Snapshot{
		BootID: "a", TS: now, Version: "test", Armed: true, Consts: wantConsts(),
		Equity: beat.Equity{
			AvailableUSDT: 63.2, PositionCost: 126.1, CapUSD: 8.62,
			CanArm: true, DailyPnL: -2.58, DailyLimit: -19.19,
		},
		Round:    beat.Round{State: beat.RoundActive, EndsAt: now.Add(2 * time.Minute)},
		Exposure: beat.Exposure{Filled: 4, Open: 2, PendingCancel: 1, Cap: 8.62},
		// LastActionAt 을 일부러 오래된 값으로 둔다. 한 번 걸고 군중이 안
		// 움직이는 회차가 **정상**이고, "건강한 스냅샷" 이 그 사실을 담고
		// 있어야 옛 규칙(무행동=정체)이 되살아날 때 통합 시험이 잡는다.
		Loop: beat.Loop{
			WSLastDataAt: now, RateLimitRemaining: 118,
			LastActionAt: now.Add(-3 * time.Minute), LastLoopAt: now,
		},
		Skips: map[beat.SkipReason]int{},
	}
}
