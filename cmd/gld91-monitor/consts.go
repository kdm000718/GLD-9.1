package main

import (
	"github.com/kdm000718/GLD-9.1/internal/beat"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// wantConsts 는 봇 패키지에서 그대로 가져온 상수다.
//
// **리터럴을 적으면 안 된다.** 0.0455 를 손으로 적어 두면 봇의 CapFraction 을
// 바꿨을 때 모니터가 옛 값을 "정상"으로 확인해 준다 — 상수 대조 규칙이 정확히
// 그 상황을 잡으려고 있는 것인데, 그 규칙 자체가 눈이 먼다.
func wantConsts() beat.Consts {
	return beat.Consts{
		CapFraction:         risk.CapFraction,
		DailyFraction:       risk.DefaultDailyFraction,
		ConfidenceThreshold: live.ConfidenceThreshold,
		MinOrderUSD:         risk.MinOrderUSD,
	}
}
