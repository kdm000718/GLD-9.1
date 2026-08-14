package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
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
//
// **비는 것만이 문제가 아니다. 얼어붙는 것도 같은 고장이다**(2026-08-14 실거래).
// 탐침을 Publish 시점에 박아 두면 그 값의 신선도가 곧 "Publish 가 얼마나 자주
// 불리는가" 가 된다. 시간대 관문이 생기면서 걸지 않는 회차는 exec 루프를 아예
// 돌지 않게 됐고, 그 5분 동안 같은 WSLastDataAt 이 3초마다 재전송돼 모니터가
// "마켓데이터 300초 정체" Crit 을 하루 절반 내내 울렸다 — 호가는 5 Hz 로
// 멀쩡히 오는데. 그래서 탐침은 **전송 직전에** 읽는다.

func TestPublishPullsLoopProbes(t *testing.T) {
	w := newTestWire(t)
	at := time.Date(2026, 8, 11, 7, 30, 0, 0, time.UTC)
	w.SetProbes(loopProbes{
		WSLastDataAt:  func() time.Time { return at },
		FillsPollAt:   func() time.Time { return at.Add(-2 * time.Second) },
		RateRemaining: func() int { return 137 },
	})

	w.Report(snapshotInput{})
	got := beatToSend(t, w)

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
	if got := beatToSend(t, w); got.Loop.RateLimitRemaining != 99 {
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
	got := beatToSend(t, w)
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
	got := beatToSend(t, w)
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

// beatToSend 는 **지금 전송된다면 실려 나갈** 스냅샷이다. 전송은 하지 않는다 —
// Run 을 띄우지 않았으므로 네트워크를 타지 않는다.
//
// 저장된 스냅샷을 그대로 읽지 않고 refresh 를 거치는 것이 요점이다. 탐침은
// 전송 직전에 다시 읽히므로(beat.go 의 reporter.refresh), 저장본만 보면
// 실제로 나가는 값과 다른 것을 검사하게 된다.
func beatToSend(t *testing.T, w *beatWire) beat.Snapshot {
	t.Helper()
	snap := w.rp.latest.Load()
	if snap == nil {
		t.Fatal("발행된 스냅샷이 없다")
	}
	out := *snap
	if w.rp.refresh != nil {
		w.rp.refresh(&out)
	}
	return out
}

// **탐침은 Publish 가 아니라 전송 직전에 읽힌다.**
//
// 이 시험이 그 계약의 전부다: 한 번 발행한 뒤 Publish 를 다시 부르지 않아도,
// 다음 전송은 그 사이에 바뀐 탐침 값을 실어야 한다. 걸지 않는 회차는 5분 동안
// Publish 가 한 번도 불리지 않으므로, 이것이 깨지면 그 5분이 통째로 얼어붙은
// 값으로 보고된다.
func TestProbesAreReadAtSendTimeNotPublishTime(t *testing.T) {
	w := newTestWire(t)
	now := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	var wsAt = now
	var rate = 200
	w.SetProbes(loopProbes{
		WSLastDataAt:  func() time.Time { return wsAt },
		RateRemaining: func() int { return rate },
	})

	// 회차 하나가 발행한다. 이후 **Publish 는 다시 불리지 않는다** —
	// 시간대 관문에 걸린 회차가 정확히 그 상태다.
	w.Report(snapshotInput{})

	// 그 사이 마켓데이터는 계속 들어오고 레이트도 움직인다.
	wsAt = now.Add(4 * time.Minute)
	rate = 173

	got := beatToSend(t, w)
	if !got.Loop.WSLastDataAt.Equal(wsAt) {
		t.Errorf("ws_last_data_at = %v, want %v — Publish 시점에 얼어붙었다",
			got.Loop.WSLastDataAt, wsAt)
	}
	if got.Loop.RateLimitRemaining != rate {
		t.Errorf("ratelimit_remaining = %d, want %d — Publish 시점에 얼어붙었다",
			got.Loop.RateLimitRemaining, rate)
	}
}

// 배선이 실제로 꽂혀 있어야 한다. refresh 가 nil 이면 위 시험은 저장본을
// 그대로 읽어 통과할 수 있고, 운영에서는 값이 얼어붙는다.
func TestRefreshHookIsPluggedIntoTheReporter(t *testing.T) {
	w := newTestWire(t)
	if w.rp.refresh == nil {
		t.Fatal("reporter.refresh 가 nil 이다 — 탐침이 전송 시점에 갱신되지 않는다")
	}
}

// **send 가 실제로 훅을 부르는지 본다.**
//
// 위 시험들은 훅이 옳게 동작하는지만 본다 — 도우미가 스스로 refresh 를
// 적용하기 때문이다. send 안의 호출을 지워도 전부 통과한다(2026-08-14 변이 W2).
// 이 저장소의 단골 실패 형태다: 함수는 맞는데 호출부가 없다.
//
// 그래서 여기서는 **진짜 전송**을 태우고 나간 본문을 뜯어본다.
func TestSendActuallyRefreshesProbes(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	env := map[string]string{EnvBeatEndpoint: srv.URL}
	w := newBeatWire(func(k string) string { return env[k] }, "test", false, "boot")
	if w == nil {
		t.Fatal("배선이 nil 이다")
	}
	now := time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)
	wsAt := now
	w.SetProbes(loopProbes{
		WSLastDataAt:  func() time.Time { return wsAt },
		RateRemaining: func() int { return 240 },
	})

	// 회차 하나가 발행하고, 그 뒤로 Publish 는 없다.
	w.Report(snapshotInput{})
	// 5분이 흐르는 동안 마켓데이터는 계속 들어온다.
	wsAt = now.Add(5 * time.Minute)

	w.rp.send(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("전송 %d건, 기대 1건", len(bodies))
	}
	var got beat.Snapshot
	if err := json.Unmarshal(bodies[0], &got); err != nil {
		t.Fatalf("본문 파싱: %v", err)
	}
	if !got.Loop.WSLastDataAt.Equal(wsAt) {
		t.Errorf("나간 본문의 ws_last_data_at = %v, want %v — send 가 탐침을 다시 읽지 않았다",
			got.Loop.WSLastDataAt, wsAt)
	}
}
