package main

import (
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
	"github.com/kdm000718/GLD-9.1/internal/exec"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// **모니터 없이도 봇은 돈다.** 감시가 없다고 거래를 막으면 감시 장치의
// 부재가 그대로 거래 장애가 된다.
func TestNilWireIsInert(t *testing.T) {
	var w *beatWire // BEAT_ENDPOINT 없음
	if !w.MayEnterRound() {
		t.Error("감시가 꺼졌는데 회차 진입이 막혔다")
	}
	if w.ShouldExit() {
		t.Error("감시가 꺼졌는데 종료하라고 한다")
	}
	// 패닉하지 않는다.
	w.Observe(exec.Observation{})
	w.Report(snapshotInput{})
	w.Run(nil)
}

func TestWireDisabledWithoutEndpoint(t *testing.T) {
	env := map[string]string{EnvBeatSecret: "s"}
	if w := newBeatWire(func(k string) string { return env[k] }, "v", false, "boot"); w != nil {
		t.Error("엔드포인트가 없는데 배선이 만들어졌다")
	}
	env[EnvBeatEndpoint] = "http://x"
	if w := newBeatWire(func(k string) string { return env[k] }, "v", false, "boot"); w == nil {
		t.Error("엔드포인트가 있는데 배선이 없다")
	}
}

func testWire(t *testing.T) *beatWire {
	t.Helper()
	env := map[string]string{EnvBeatEndpoint: "http://127.0.0.1:1", EnvBeatSecret: "s"}
	w := newBeatWire(func(k string) string { return env[k] }, "gld91 test", true, "boot-1")
	if w == nil {
		t.Fatal("배선 생성 실패")
	}
	return w
}

// 발행된 스냅샷을 읽는다(전송하지 않고 reporter 의 최신값을 본다).
func published(t *testing.T, w *beatWire) beat.Snapshot {
	t.Helper()
	s := w.rp.latest.Load()
	if s == nil {
		t.Fatal("아무것도 발행되지 않았다")
	}
	return *s
}

// **회차가 바뀌면 이전 회차의 관측을 물려주지 않는다.** 물려주면 새 회차
// 시작 직후에 지난 회차의 노출이 실려 나가고, 모니터가 그것으로 불변식을
// 판정한다.
func TestWireResetsObservationOnRoundChange(t *testing.T) {
	w := testWire(t)
	r1 := live.Round{Slug: "btc-updown-5m-1", EndsAt: time.Now().Add(time.Minute)}
	w.Report(snapshotInput{Round: r1, Frozen: live.Frozen{Eligible: true}, Equity: armable(), Active: true})
	w.Observe(exec.Observation{
		OpenIDs:  []string{"0xa"},
		Exposure: risk.Exposure{OpenNotional: 5},
	})
	if got := published(t, w); len(got.Exposure.OpenOrders) != 1 {
		t.Fatalf("첫 회차 관측이 안 실렸다: %+v", got.Exposure)
	}

	r2 := live.Round{Slug: "btc-updown-5m-2", EndsAt: time.Now().Add(2 * time.Minute)}
	w.Report(snapshotInput{Round: r2, Frozen: live.Frozen{Eligible: true}, Equity: armable(), Active: true})
	got := published(t, w)
	if len(got.Exposure.OpenOrders) != 0 || got.Exposure.Open != 0 {
		t.Errorf("새 회차에 이전 노출이 물려갔다: %+v", got.Exposure)
	}
	if got.Round.Slug != "btc-updown-5m-2" {
		t.Errorf("회차 = %q", got.Round.Slug)
	}
}

// 관측은 회차 맥락을 재사용한다 — exec 는 회차·자본을 모른다.
func TestObserveKeepsRoundContext(t *testing.T) {
	w := testWire(t)
	r := live.Round{Slug: "btc-updown-5m-1", MarketID: 70, EndsAt: time.Now().Add(time.Minute)}
	w.Report(snapshotInput{Round: r, Frozen: live.Frozen{Eligible: true, PUp: 0.53}, Equity: armable(), Active: true})
	w.Observe(exec.Observation{FilledShares: 9, Exposure: risk.Exposure{FilledNotional: 2}})

	got := published(t, w)
	if got.Round.Slug != "btc-updown-5m-1" || got.Round.MarketID != 70 {
		t.Errorf("관측이 회차 맥락을 잃었다: %+v", got.Round)
	}
	if got.Round.PUp != 0.53 || got.Round.State != beat.RoundActive {
		t.Errorf("자격·상태를 잃었다: %+v", got.Round)
	}
	if got.Exposure.FilledShares != 9 || got.Exposure.Filled != 2 {
		t.Errorf("관측값이 안 실렸다: exposure=%+v", got.Exposure)
	}
	if got.Version != "gld91 test" || !got.Armed {
		t.Errorf("version=%q armed=%v", got.Version, got.Armed)
	}
}

// 스킵 사유는 입력에서 유도한다. 힌트 필드를 따로 두면 그 둘이 갈리고,
// 갈린 쪽이 틀렸을 때 모니터가 엉뚱한 사유로 판정한다.
func TestWireCountsSkipsByDerivedReason(t *testing.T) {
	w := testWire(t)
	for i := 0; i < 3; i++ {
		w.Report(snapshotInput{
			Round:  live.Round{Slug: "r"},
			Frozen: live.Frozen{Eligible: false, Confidence: 0.001},
			Equity: armable(),
		})
	}
	w.Report(snapshotInput{
		Round: live.Round{Slug: "r"}, Frozen: live.Frozen{Eligible: true},
		Equity: risk.Equity{AvailableUSDT: 5}, // 무장 불가
	})
	got := published(t, w)
	if got.Skips[beat.SkipConfBelow] != 3 {
		t.Errorf("conf_below = %d, want 3 (%v)", got.Skips[beat.SkipConfBelow], got.Skips)
	}
	if got.Skips[beat.SkipEquity] != 1 {
		t.Errorf("equity = %d, want 1 (%v)", got.Skips[beat.SkipEquity], got.Skips)
	}
}

// 참여한 회차는 스킵으로 세지 않는다.
func TestActiveRoundIsNotCountedAsSkip(t *testing.T) {
	w := testWire(t)
	w.Report(snapshotInput{
		Round: live.Round{Slug: "r"}, Frozen: live.Frozen{Eligible: true},
		Equity: armable(), Active: true,
	})
	if n := len(published(t, w).Skips); n != 0 {
		t.Errorf("참여 회차가 스킵으로 세어졌다: %v", published(t, w).Skips)
	}
}

// 기동마다 boot ID 가 달라야 한다 — 그것이 재시작을 드러내는 유일한 신호다.
func TestBootIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newBootID()
		if id == "" {
			t.Fatal("boot ID 가 비었다")
		}
		if seen[id] {
			t.Fatalf("boot ID 가 중복이다: %q", id)
		}
		seen[id] = true
	}
}
