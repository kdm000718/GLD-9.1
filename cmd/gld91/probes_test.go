package main

import (
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
	"github.com/kdm000718/GLD-9.1/internal/exec"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// 이 파일이 지키는 것은 하나다: **하트비트의 Loop 필드가 조용히 비지 않는다.**
//
// 세 필드가 비면 감시 장치가 두 방향으로 동시에 고장 난다.
//   - RateLimitRemaining=0 → `ratelimit` Crit 이 **매번** 울린다. 늘 울리는
//     경보 옆의 진짜 Crit 은 아무도 보지 않는다. 게다가 모니터의 정산
//     조회가 "예산 부족" 으로 영원히 건너뛴다(gld91-monitor/settle.go).
//   - WSLastDataAt=0 → `ws_data` 규칙이 제로를 "모른다" 로 읽고 넘어간다.
//     호가가 끊긴 채 도는 봇을 잡는 유일한 규칙이 조용히 꺼진다.
//
// 둘 다 봇이 멀쩡할 때는 드러나지 않고, 봇이 아플 때만 드러난다.

func TestPublishPullsLoopProbes(t *testing.T) {
	w := newTestWire(t)
	at := time.Date(2026, 8, 11, 7, 30, 0, 0, time.UTC)
	w.SetProbes(loopProbes{
		WSLastDataAt:  func() time.Time { return at },
		FillsPollAt:   func() time.Time { return at.Add(-2 * time.Second) },
		RateRemaining: func() int { return 137 },
	})

	w.Report(snapshotInput{})
	got := lastPublished(t, w)

	if !got.Loop.WSLastDataAt.Equal(at) {
		t.Errorf("ws_last_data_at = %v, want %v", got.Loop.WSLastDataAt, at)
	}
	if want := at.Add(-2 * time.Second); !got.Loop.FillsPollAt.Equal(want) {
		t.Errorf("fills_poll_at = %v, want %v", got.Loop.FillsPollAt, want)
	}
	if got.Loop.RateLimitRemaining != 137 {
		t.Errorf("ratelimit_remaining = %d, want 137", got.Loop.RateLimitRemaining)
	}
}

func TestObservePathAlsoCarriesProbes(t *testing.T) {
	// 회차 중 발행의 절대다수는 Observe 경로다(회차당 6,000회). Report 만
	// 탐침을 당기고 Observe 가 빠뜨리면, 회차가 도는 내내 필드가 비어 있다가
	// 회차 경계에서만 잠깐 채워진다 — 감시 입장에서는 그것이 곧 정체다.
	w := newTestWire(t)
	w.SetProbes(loopProbes{RateRemaining: func() int { return 99 }})

	w.Observe(exec.Observation{})
	if got := lastPublished(t, w); got.Loop.RateLimitRemaining != 99 {
		t.Fatalf("Observe 경로의 ratelimit_remaining = %d, want 99", got.Loop.RateLimitRemaining)
	}
}

func TestSetProbesDoesNotUnplugEarlierProbes(t *testing.T) {
	// 탐침의 주인이 둘로 나뉘어 있다 — REST 쪽은 무장 배선에서, WS 쪽은
	// 소켓이 만들어지는 loop() 에서 꽂는다. 나중에 부르는 쪽이 통째로
	// 덮어쓰면 먼저 꽂힌 탐침이 조용히 뽑힌다.
	w := newTestWire(t)
	w.SetProbes(loopProbes{RateRemaining: func() int { return 240 }})
	w.SetProbes(loopProbes{WSLastDataAt: func() time.Time { return time.Unix(1786000000, 0).UTC() }})

	w.Report(snapshotInput{})
	got := lastPublished(t, w)
	if got.Loop.RateLimitRemaining != 240 {
		t.Errorf("먼저 꽂은 탐침이 뽑혔다: ratelimit_remaining = %d, want 240", got.Loop.RateLimitRemaining)
	}
	if got.Loop.WSLastDataAt.IsZero() {
		t.Error("나중에 꽂은 탐침이 반영되지 않았다: ws_last_data_at 이 비었다")
	}
}

// TestRateProbeNeverStartsAtZero 는 배선 전체를 실물로 잇는다. 기동 직후,
// 아직 아무 REST 요청도 나가지 않은 상태에서 스냅샷이 0 을 실으면 감시가
// 첫 하트비트부터 거짓 Crit 을 낸다.
func TestRateProbeNeverStartsAtZero(t *testing.T) {
	rc := rest.New("k")
	w := newTestWire(t)
	w.SetProbes(loopProbes{RateRemaining: rc.RateLimitRemaining})

	w.Report(snapshotInput{})
	got := lastPublished(t, w)
	if got.Loop.RateLimitRemaining <= 0 {
		t.Fatalf("기동 직후 ratelimit_remaining = %d — 감시 규칙이 즉시 Crit 을 낸다",
			got.Loop.RateLimitRemaining)
	}
}

// TestNoFillsReportsNoPoll 은 DRY-RUN 이 폴링한 척하지 않는지 본다.
func TestNoFillsReportsNoPoll(t *testing.T) {
	if got := (noFills{}).LastPollAt(); !got.IsZero() {
		t.Fatalf("noFills.LastPollAt = %v, want 제로값 — DRY-RUN 은 조회하지 않는다", got)
	}
}

// TestChooseFillsKeepsPollAtInTheType 은 타입 가드가 살아 있는지 본다.
// chooseFills 의 반환 타입이 exec.Fills 로 되돌아가면 배선은 타입 단언에
// 기대게 되고, 그 순간 스냅샷 필드가 조용히 빌 수 있다.
func TestChooseFillsKeepsPollAtInTheType(t *testing.T) {
	f, err := chooseFills(false, fillsDeps{})
	if err != nil {
		t.Fatalf("chooseFills: %v", err)
	}
	var _ pollingFills = f // 컴파일되지 않으면 가드가 사라진 것이다
	if !f.LastPollAt().IsZero() {
		t.Fatal("DRY-RUN 경로가 폴링 시각을 보고했다")
	}
}

// ---------------------------------------------------------------------------

func newTestWire(t *testing.T) *beatWire {
	t.Helper()
	env := map[string]string{EnvBeatEndpoint: "http://127.0.0.1:1/beat"}
	w := newBeatWire(func(k string) string { return env[k] }, "test", false, "boot")
	if w == nil {
		t.Fatal("배선이 nil 이다")
	}
	return w
}

// lastPublished 는 reporter 가 들고 있는 마지막 스냅샷이다. 전송은 하지
// 않는다 — Run 을 띄우지 않았으므로 네트워크를 타지 않는다.
func lastPublished(t *testing.T, w *beatWire) beat.Snapshot {
	t.Helper()
	snap := w.rp.latest.Load()
	if snap == nil {
		t.Fatal("발행된 스냅샷이 없다")
	}
	return *snap
}
