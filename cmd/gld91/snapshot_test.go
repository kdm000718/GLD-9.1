package main

import (
	"errors"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
	"github.com/kdm000718/GLD-9.1/internal/exec"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// armable 은 무장 가능한 자본이다(cap = 200×0.0455 = 9.1 > $1).
func armable() risk.Equity { return risk.Equity{AvailableUSDT: 200} }

// **심각한 사유가 이긴다.** 일손실 한도에 걸린 회차는 confidence 도 미달일
// 수 있는데, 그때 "문턱 미달" 로 보고하면 모니터가 조용해진다 — 정확히
// 반대로 읽혀야 하는 경우다.
func TestSnapshotSkipReasonPriority(t *testing.T) {
	base := snapshotInput{
		Frozen: live.Frozen{Eligible: false, Confidence: 0.001},
		Equity: armable(),
	}
	cases := []struct {
		name string
		mut  func(*snapshotInput)
		want beat.SkipReason
	}{
		{"문턱 미달만", func(*snapshotInput) {}, beat.SkipConfBelow},
		{"표본 미채택이 이긴다", func(in *snapshotInput) { in.SampleRejected = true }, beat.SkipSampleRejected},
		{"equity 부족이 이긴다", func(in *snapshotInput) {
			in.SampleRejected = true
			in.Equity = risk.Equity{AvailableUSDT: 5}
		}, beat.SkipEquity},
		{"일손실이 이긴다", func(in *snapshotInput) {
			in.SampleRejected, in.DailyBreached = true, true
			in.Equity = risk.Equity{AvailableUSDT: 5}
		}, beat.SkipDailyLimit},
		{"회차 조회 실패가 이긴다", func(in *snapshotInput) {
			in.DailyBreached, in.FetchErr = true, errors.New("x")
		}, beat.SkipFetchError},
		{"예측 실패가 최우선", func(in *snapshotInput) {
			in.FetchErr, in.PredictErr = errors.New("x"), errors.New("y")
		}, beat.SkipPredictError},
	}
	for _, c := range cases {
		in := base
		c.mut(&in)
		got := buildSnapshot(in)
		if got.Round.State != beat.RoundSkipped {
			t.Errorf("%s: state = %q, want SKIPPED", c.name, got.Round.State)
		}
		if got.Round.SkipReason != c.want {
			t.Errorf("%s: reason = %q, want %q", c.name, got.Round.SkipReason, c.want)
		}
	}
}

func TestSnapshotActiveAndIdle(t *testing.T) {
	in := snapshotInput{Frozen: live.Frozen{Eligible: true}, Equity: armable(), Active: true}
	if got := buildSnapshot(in); got.Round.State != beat.RoundActive || got.Round.SkipReason != "" {
		t.Errorf("state = %q reason = %q, want ACTIVE/빈값", got.Round.State, got.Round.SkipReason)
	}
	in.Active = false
	if got := buildSnapshot(in); got.Round.State != beat.RoundIdle {
		t.Errorf("state = %q, want IDLE", got.Round.State)
	}
}

// **리터럴을 적으면 안 된다.** 0.0455 를 손으로 적어 두면 봇 상수를 바꿨을 때
// 모니터가 옛 값을 "정상" 으로 확인해 준다 — 상수 대조 규칙 자체가 눈이 먼다.
func TestSnapshotConstsComeFromPackages(t *testing.T) {
	c := buildSnapshot(snapshotInput{}).Consts
	want := beat.Consts{
		CapFraction:         risk.CapFraction,
		DailyFraction:       risk.DefaultDailyFraction,
		ConfidenceThreshold: live.ConfidenceThreshold,
		MinOrderUSD:         risk.MinOrderUSD,
	}
	if c != want {
		t.Errorf("consts = %+v, want %+v", c, want)
	}
}

// cap 은 risk.Cap 을 지나야 한다. Total()×0.0455 를 직접 곱하면 risk 패키지의
// 입력 검사(음수·NaN·Inf)를 통째로 우회한다.
func TestSnapshotCapGoesThroughRisk(t *testing.T) {
	e := risk.Equity{AvailableUSDT: 191.92, PositionCost: 30}
	got := buildSnapshot(snapshotInput{Equity: e})
	want := risk.Cap(e)
	if got.Equity.CapUSD != want || got.Exposure.Cap != want {
		t.Errorf("cap = %v/%v, want %v", got.Equity.CapUSD, got.Exposure.Cap, want)
	}
	// 망가진 자본에서는 risk 가 0 을 돌려주고, 그것이 그대로 실려야 한다.
	bad := risk.Equity{AvailableUSDT: -1}
	if c := buildSnapshot(snapshotInput{Equity: bad}).Equity.CapUSD; c != 0 {
		t.Errorf("음수 자본에 cap = %v, want 0", c)
	}
}

// **일손실 한도는 음수다.** 모니터가 `DailyPnL <= DailyLimit` 으로 비교하므로
// 부호를 뒤집으면 이익이 날 때 한도로 읽힌다 — pmmm-go 에서 부호 하나로
// +40 인 전략이 −90 으로 보고된 전례가 있다.
func TestDailyLimitIsNegative(t *testing.T) {
	e := risk.Equity{AvailableUSDT: 150, PositionCost: 50}
	got := buildSnapshot(snapshotInput{Equity: e}).Equity.DailyLimit
	if got >= 0 {
		t.Fatalf("일손실 한도가 %v 다 — 음수여야 한다", got)
	}
	if want := -(200 * risk.DefaultDailyFraction); got != want {
		t.Errorf("한도 = %v, want %v", got, want)
	}
}

// 관측이 미체결 **목록**으로 옮겨진다. 개인키 없는 모니터에게 이것이 유일한
// 정보원이므로 개수만 옮기면 안 된다.
func TestSnapshotCarriesOpenOrderList(t *testing.T) {
	in := snapshotInput{
		Frozen: live.Frozen{Eligible: true}, Equity: armable(), Active: true,
		Obs: exec.Observation{
			OpenIDs:       []string{"0xa", "0xb"},
			OpenTicks:     []int64{487, 486},
			OpenNotionals: []float64{18.0, 4.5},
			Exposure:      risk.Exposure{FilledNotional: 3, OpenNotional: 22.5, PendingCancel: 1},
			Unaccounted:   0.75,
		},
	}
	got := buildSnapshot(in)
	if len(got.Exposure.OpenOrders) != 2 {
		t.Fatalf("미체결 %d건, want 2", len(got.Exposure.OpenOrders))
	}
	if o := got.Exposure.OpenOrders[0]; o.ID != "0xa" || o.Tick != 487 || o.Notional != 18.0 {
		t.Errorf("첫 주문 = %+v", o)
	}
	if o := got.Exposure.OpenOrders[1]; o.ID != "0xb" || o.Notional != 4.5 {
		t.Errorf("둘째 주문 = %+v", o)
	}
	// 노출 3항이 전부 옮겨져야 한다 — 취소미확인이 빠지면 exec 불변식에서
	// 그 봇에만 있는 항이 감시에서 사라진다.
	if got.Exposure.Filled != 3 || got.Exposure.Open != 22.5 || got.Exposure.PendingCancel != 1 {
		t.Errorf("노출 = %+v", got.Exposure)
	}
	if got.Exposure.Unaccounted != 0.75 {
		t.Errorf("unaccounted = %v", got.Exposure.Unaccounted)
	}
}

// 병렬 슬라이스 길이가 어긋나도 패닉하지 않는다. 관측이 깨진 것은 알람으로
// 다룰 일이지, 봇을 죽일 일이 아니다.
func TestSnapshotToleratesRaggedObservation(t *testing.T) {
	in := snapshotInput{
		Obs: exec.Observation{
			OpenIDs:       []string{"a", "b", "c"},
			OpenTicks:     []int64{1},
			OpenNotionals: []float64{},
		},
	}
	got := buildSnapshot(in)
	if len(got.Exposure.OpenOrders) != 3 {
		t.Fatalf("미체결 %d건, want 3", len(got.Exposure.OpenOrders))
	}
	if got.Exposure.OpenOrders[2].Tick != 0 || got.Exposure.OpenOrders[2].Notional != 0 {
		t.Errorf("길이가 모자란 자리가 제로값이 아니다: %+v", got.Exposure.OpenOrders[2])
	}
}

func TestSnapshotCarriesLoopAndAck(t *testing.T) {
	now := time.Unix(1786000000, 0).UTC()
	in := snapshotInput{
		Version: "gld91 abc", Armed: true, Acked: beat.CmdShutdown,
		Obs:           exec.Observation{Reprices: 42, LastActionAt: now, LastLoopAt: now.Add(3 * time.Second)},
		WSLastDataAt:  now.Add(-time.Second),
		RateRemaining: 118,
		Skips:         map[beat.SkipReason]int{beat.SkipConfBelow: 7},
	}
	got := buildSnapshot(in)
	if got.Loop.Reprices != 42 || !got.Loop.LastActionAt.Equal(now) {
		t.Errorf("loop = %+v", got.Loop)
	}
	// **둘을 다른 값으로 넣고 각각 확인한다.** 같은 값으로 시험하면 배선이
	// 서로를 가리켜도 통과하고, 그 순간 정체 판정이 옛 규칙으로 되돌아간다.
	if !got.Loop.LastLoopAt.Equal(now.Add(3 * time.Second)) {
		t.Errorf("LastLoopAt = %v, want %v", got.Loop.LastLoopAt, now.Add(3*time.Second))
	}
	if got.Loop.RateLimitRemaining != 118 {
		t.Errorf("예산 = %d", got.Loop.RateLimitRemaining)
	}
	if got.AckedCommand != beat.CmdShutdown {
		t.Errorf("acked = %q", got.AckedCommand)
	}
	if !got.Armed || got.Version != "gld91 abc" {
		t.Errorf("armed = %v version = %q", got.Armed, got.Version)
	}
	if got.Skips[beat.SkipConfBelow] != 7 {
		t.Errorf("skips = %v", got.Skips)
	}
}
