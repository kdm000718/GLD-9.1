package main

// 이 파일은 봇이 **이미 들고 있는** 값을 beat 스냅샷으로 옮긴다.
//
// **조회를 하지 않는다.** 레이트리밋 240 req/min 은 API 키 단위이고 봇이
// 그것을 독점해야 이 설계가 성립한다(스펙 §"예산은 키 단위다"). 감시를 위해
// API 를 한 번 더 치는 순간 감시가 거래를 굶긴다.
//
// buildSnapshot 은 순수 함수다 — 시계도 네트워크도 타지 않는다. TS·Seq·BootID
// 는 reporter.Publish 가 채운다.

import (
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
	"github.com/kdm000718/GLD-9.1/internal/exec"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// snapshotInput 은 조립에 필요한 전부다. 구조체로 받는 이유는 순수 함수로
// 두어 값 몇 개로 시험하기 위해서다.
type snapshotInput struct {
	Round  live.Round
	Frozen live.Frozen
	Equity risk.Equity
	Obs    exec.Observation

	Armed   bool
	Version string
	// Acked 는 봇이 실제로 받아서 처리한 마지막 명령이다.
	Acked beat.Command

	// 아래 다섯이 스킵 사유를 가른다.
	SampleRejected bool
	DailyBreached  bool
	// OutsideHours 는 거래 시간대가 아니라는 뜻이다(cmd/gld91/hours.go).
	// **equity 보다 먼저 본다** — 이 경로에서는 equity 를 조회하지 않으므로
	// 제로값이 들어오고, 순서를 바꾸면 그것이 "equity 부족" 으로 보고된다.
	OutsideHours bool
	FetchErr     error
	PredictErr   error

	// Active 는 지금 회차를 운용 중인가다. 회차를 잡지 못했으면 false 이고
	// 그때는 IDLE 이다.
	Active bool

	WSLastDataAt  time.Time
	FillsPollAt   time.Time
	RateRemaining int
	Skips         map[beat.SkipReason]int
}

// buildSnapshot 은 봇 상태 하나를 계약 형태로 옮긴다.
func buildSnapshot(in snapshotInput) beat.Snapshot {
	cap := risk.Cap(in.Equity)
	s := beat.Snapshot{
		Version:      in.Version,
		AckedCommand: in.Acked,
		Armed:        in.Armed,
		// **리터럴을 적지 않는다.** 0.0455 를 손으로 적어 두면 봇 상수를
		// 바꿨을 때 모니터가 옛 값을 "정상" 으로 확인해 준다 — 상수 대조
		// 규칙 자체가 눈이 먼다.
		Consts: beat.Consts{
			CapFraction:         risk.CapFraction,
			DailyFraction:       risk.DefaultDailyFraction,
			ConfidenceThreshold: live.ConfidenceThreshold,
			MinOrderUSD:         risk.MinOrderUSD,
		},
		Equity: beat.Equity{
			AvailableUSDT: in.Equity.AvailableUSDT,
			PositionCost:  in.Equity.PositionCost,
			CapUSD:        cap,
			CanArm:        risk.CanArm(in.Equity),
			DailyLimit:    dailyLimitUSD(in.Equity),
		},
		Round: beat.Round{
			MarketID:   in.Round.MarketID,
			Slug:       in.Round.Slug,
			EndsAt:     in.Round.EndsAt,
			PUp:        in.Frozen.PUp,
			Confidence: in.Frozen.Confidence,
			Outcome:    in.Frozen.Direction,
		},
		Exposure: beat.Exposure{
			Filled:        in.Obs.Exposure.FilledNotional,
			FilledShares:  in.Obs.FilledShares,
			Open:          in.Obs.Exposure.OpenNotional,
			PendingCancel: in.Obs.Exposure.PendingCancel,
			Cap:           cap,
			Unaccounted:   in.Obs.Unaccounted,
		},
		Loop: beat.Loop{
			// Reprices 는 항상 0 이다. 이 봇은 회차마다 한 가격에 한 번만 걸고
			// 주문을 옮기지 않는다(exec 패키지 문서 참고). 필드를 지우지 않는
			// 이유는 beat 가 별도로 배포된 모니터와의 계약이기 때문이다 —
			// 보내는 쪽에서 빼면 받는 쪽 파싱이 조용히 제로값을 읽는데, 그건
			// 지금 값과 같으므로 굳이 계약을 흔들 이유가 없다.
			LastActionAt:       in.Obs.LastActionAt,
			LastLoopAt:         in.Obs.LastLoopAt,
			WSLastDataAt:       in.WSLastDataAt,
			FillsPollAt:        in.FillsPollAt,
			RateLimitRemaining: in.RateRemaining,
		},
		Skips: in.Skips,
	}
	for i, id := range in.Obs.OpenIDs {
		o := beat.OpenOrder{ID: id}
		if i < len(in.Obs.OpenTicks) {
			o.Tick = in.Obs.OpenTicks[i]
		}
		if i < len(in.Obs.OpenNotionals) {
			o.Notional = in.Obs.OpenNotionals[i]
		}
		s.Exposure.OpenOrders = append(s.Exposure.OpenOrders, o)
	}
	s.Round.State, s.Round.SkipReason = roundState(in)
	return s
}

// dailyLimitUSD 는 일손실 한도를 **음수**로 돌려준다.
//
// `risk.DailyLimit` 은 Breached 판정만 내주고 한도 값을 내주지 않으므로 여기서
// 계산한다. **부호가 음수인 것이 계약이다** — 모니터가 `DailyPnL <= DailyLimit`
// 으로 비교하고, 부호를 뒤집으면 이익이 날 때 한도로 읽힌다. `~/kdm/pmmm-go`
// 에서 부호 하나로 +40 인 전략이 −90 으로 보고된 전례가 있다.
//
// 리터럴 0.10 을 쓰지 않는 이유는 위 Consts 와 같다.
func dailyLimitUSD(e risk.Equity) float64 {
	return -(e.Total() * risk.DefaultDailyFraction)
}

// roundState 는 회차 상태와 스킵 사유를 정한다.
//
// **심각한 사유가 이긴다.** 일손실 한도에 걸린 회차는 confidence 도 미달일
// 수 있는데, 그때 "문턱 미달" 로 보고하면 모니터가 조용해진다 — 정확히
// 반대로 읽혀야 하는 경우다. `internal/risk` 의 Action 이 심각한 것부터
// 보는 것과 같은 이유다.
func roundState(in snapshotInput) (beat.RoundState, beat.SkipReason) {
	switch {
	case in.PredictErr != nil:
		return beat.RoundSkipped, beat.SkipPredictError
	case in.FetchErr != nil:
		return beat.RoundSkipped, beat.SkipFetchError
	case in.DailyBreached:
		return beat.RoundSkipped, beat.SkipDailyLimit
	case in.OutsideHours:
		return beat.RoundSkipped, beat.SkipOutsideHours
	case !risk.CanArm(in.Equity):
		return beat.RoundSkipped, beat.SkipEquity
	case in.SampleRejected:
		return beat.RoundSkipped, beat.SkipSampleRejected
	case !in.Frozen.Eligible:
		return beat.RoundSkipped, beat.SkipConfBelow
	case in.Active:
		return beat.RoundActive, ""
	}
	return beat.RoundIdle, ""
}
