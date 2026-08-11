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
)

// testReporter 는 짧은 주기로 도는 리포터다.
func testReporter(t *testing.T, url string, secret []byte) *reporter {
	t.Helper()
	r := newReporter(url, secret, "boot-1")
	r.interval = 10 * time.Millisecond
	return r
}

// Publish 는 exec 루프에서 불린다 — 회차당 6,000 바퀴, 기본 50ms. 여기서
// 네트워크를 기다리면 재호가가 밀리고, 밀린 재호가는 큐 위치를 잃는다.
//
// **전송이 멈춰 있는 동안** 재는 것이 요점이다. 서버가 응답하지 않는 상태
// (= 죽은 모니터)에서도 Publish 가 그대로 빨라야 한다.
//
// 반복 횟수를 회차 한 바퀴 수(6,000)로 잡고 예산을 100ms 로 둔다. 정상
// 구현은 원자적 교체 하나라 마이크로초 단위로 끝나므로 여유가 크고, 한
// 번이라도 네트워크를 기다리면(클라이언트 시한 2초) 즉시 초과한다.
func TestPublishNeverBlocks(t *testing.T) {
	// 요청을 붙잡되 테스트 종료 시 반드시 풀린다. r.Context().Done() 만
	// 기다리면 클라이언트 시한까지 핸들러가 쌓여 패키지 전체가 느려진다.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	rp := newReporter(srv.URL, []byte("s"), "boot-1")
	// 느린 주기 — 전송이 멈춰 있는 것을 만들되 핸들러를 쌓지 않는다.
	rp.interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)
	time.Sleep(20 * time.Millisecond) // 전송이 한 번 걸리게 둔다

	start := time.Now()
	for i := 0; i < 6_000; i++ {
		rp.Publish(beat.Snapshot{})
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("전송이 멈춰 있는 동안 Publish 6,000회에 %v 걸렸다 — 블록하고 있다", d)
	}
}

// 최신값만 보낸다. 큐가 쌓이면 모니터가 과거를 현재로 읽고, 3초 전 스냅샷
// 으로 "노출 위반 없음"을 판정한다 — 판정하지 않는 것보다 나쁘다.
func TestReporterSendsLatestOnly(t *testing.T) {
	var mu sync.Mutex
	var got []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s beat.Snapshot
		_ = json.NewDecoder(r.Body).Decode(&s)
		mu.Lock()
		got = append(got, s.Loop.Reprices)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(beat.Reply{Command: beat.CmdNone})
	}))
	defer srv.Close()

	rp := testReporter(t, srv.URL, []byte("s"))
	for i := 1; i <= 500; i++ {
		rp.Publish(beat.Snapshot{Loop: beat.Loop{Reprices: int64(i)}})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	rp.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 {
		t.Fatal("아무것도 전송되지 않았다")
	}
	// **내용**으로 확인한다. seq 로 확인하면 seq 가 전송마다 증가하는 것과
	// 큐가 쌓이는 것을 구분하지 못한다.
	for _, n := range got {
		if n != 500 {
			t.Errorf("%d번째 스냅샷이 전송됐다 — 최신(500)이 아니면 큐가 쌓인 것이다", n)
		}
	}
}

// BootID 는 Publish 가 채운다. 호출자가 채우게 하면 언젠가 빠뜨린다.
func TestPublishStampsBootID(t *testing.T) {
	rp := newReporter("http://unused", []byte("s"), "boot-xyz")
	rp.Publish(beat.Snapshot{})
	if s := rp.latest.Load(); s.BootID != "boot-xyz" {
		t.Errorf("bootID = %q", s.BootID)
	}
}

// **실거래가 잡은 결함(2026-08-11 11:15~11:20).**
//
// confidence 미달로 건너뛴 회차는 `exec` 루프가 돌지 않아 Publish 가 한 번도
// 불리지 않는다. seq 를 Publish 에서 찍으면 그 5분 내내 같은 seq 를 재전송하고,
// 모니터의 재전송 방지가 전부 거부해 `하트비트 무응답` 이 회차 내내 울린다.
// 건너뛴 회차는 이 봇에서 **가장 흔한 정상 상태**다.
func TestSeqAdvancesWithoutNewPublish(t *testing.T) {
	var mu sync.Mutex
	var seqs []uint64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s beat.Snapshot
		_ = json.NewDecoder(r.Body).Decode(&s)
		mu.Lock()
		seqs = append(seqs, s.Seq)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(beat.Reply{Command: beat.CmdNone})
	}))
	defer srv.Close()

	rp := testReporter(t, srv.URL, []byte("s"))
	rp.Publish(beat.Snapshot{}) // 회차 경계에서 딱 한 번. 이후 발행 없음.
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	rp.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(seqs) < 3 {
		t.Fatalf("전송 %d회 — 이 시험이 반복 전송을 보고 있지 않다", len(seqs))
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("발행 없이 재전송했더니 seq 가 %d → %d 다 — 모니터가 재전송으로 거부한다",
				seqs[i-1], seqs[i])
		}
	}
}

// 서명이 붙고, 서버가 받은 본문 그대로 검증된다.
func TestReporterSignsBody(t *testing.T) {
	secret := []byte("s3cret")
	done := make(chan struct{})
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !beat.Verify(secret, body, r.Header.Get(beat.SigHeader)) {
			t.Error("서명이 검증되지 않는다")
		}
		_ = json.NewEncoder(w).Encode(beat.Reply{Command: beat.CmdNone})
		once.Do(func() { close(done) })
	}))
	defer srv.Close()

	rp := testReporter(t, srv.URL, secret)
	rp.Publish(beat.Snapshot{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("요청이 오지 않았다")
	}
}

// POST 가 실패해도 Run 은 죽지 않는다. 부수 기능의 실패가 주 기능을 죽이면
// 안 된다 — internal/ledger 와 같은 원칙이다.
func TestReporterSurvivesFailure(t *testing.T) {
	rp := newReporter("http://127.0.0.1:1", []byte("s"), "boot") // 연결 거부
	rp.interval = 5 * time.Millisecond
	rp.Publish(beat.Snapshot{})

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	rp.Run(ctx) // 패닉도 조기 반환도 없이 ctx 만료까지 돈다

	if rp.ConsecFail() == 0 {
		t.Error("실패가 세어지지 않았다 — 봇이 모니터의 사망을 알 수 없다")
	}
}

// 5xx 도 실패다. 200 만 성공으로 세지 않으면 "모니터가 살아 있지만 고장난"
// 상태가 정상으로 보인다.
func TestReporterCountsNon200AsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	rp := testReporter(t, srv.URL, []byte("s"))
	rp.Publish(beat.Snapshot{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	rp.Run(ctx)

	if rp.ConsecFail() == 0 {
		t.Error("500 응답이 실패로 세어지지 않았다")
	}
}

// 성공하면 카운터가 0 으로 돌아간다 — 그러지 않으면 한 번 끊긴 뒤 영원히
// "모니터 없음"으로 보고한다.
func TestConsecFailResetsOnSuccess(t *testing.T) {
	var fail = true
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		f := fail
		mu.Unlock()
		if f {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(beat.Reply{Command: beat.CmdNone})
	}))
	defer srv.Close()

	rp := testReporter(t, srv.URL, []byte("s"))
	rp.Publish(beat.Snapshot{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)

	waitFor(t, "실패가 쌓이기", func() bool { return rp.ConsecFail() > 0 })
	mu.Lock()
	fail = false
	mu.Unlock()
	waitFor(t, "카운터가 0 으로 돌아가기", func() bool { return rp.ConsecFail() == 0 })
}

// **모니터는 봇이 받아 갈 때까지 같은 명령을 반복해 답한다.** 그것을 매번
// 처리하면 종료가 3초마다 다시 시작된다. 명령은 사건이 아니라 상태이므로
// 값이 바뀔 때만 흘린다.
func TestRepeatedCommandIsDeliveredOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(beat.Reply{Command: beat.CmdShutdown})
	}))
	defer srv.Close()

	rp := testReporter(t, srv.URL, []byte("s"))
	rp.Publish(beat.Snapshot{})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	go rp.Run(ctx)

	var got []beat.Command
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case c := <-rp.Commands():
			got = append(got, c)
		case <-deadline:
			if len(got) != 1 {
				t.Errorf("명령이 %d회 전달됐다, want 1: %v", len(got), got)
			}
			if rp.Acked() != beat.CmdShutdown {
				t.Errorf("Acked = %q, want shutdown", rp.Acked())
			}
			return
		}
	}
}

// 명령이 거둬졌다가 다시 오면 그때는 새 명령이다 — /cancel_shutdown 뒤에
// 다시 /shutdown 을 보낼 수 있어야 한다.
func TestCommandRefiresAfterNone(t *testing.T) {
	var mu sync.Mutex
	cmd := beat.CmdShutdown
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		c := cmd
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(beat.Reply{Command: c})
	}))
	defer srv.Close()

	rp := testReporter(t, srv.URL, []byte("s"))
	rp.Publish(beat.Snapshot{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)

	recv := func(what string) beat.Command {
		select {
		case c := <-rp.Commands():
			return c
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: 명령이 오지 않았다", what)
			return ""
		}
	}
	if c := recv("첫 shutdown"); c != beat.CmdShutdown {
		t.Fatalf("첫 명령 = %q", c)
	}
	mu.Lock()
	cmd = beat.CmdNone
	mu.Unlock()
	waitFor(t, "none 이 반영되기", func() bool { return rp.Acked() == beat.CmdNone })
	mu.Lock()
	cmd = beat.CmdShutdown
	mu.Unlock()
	if c := recv("두 번째 shutdown"); c != beat.CmdShutdown {
		t.Errorf("두 번째 명령 = %q", c)
	}
}

// 알 수 없는 명령은 무시한다. 모니터가 오작동해도 봇은 자기가 아는 것만
// 한다 — 모르는 문자열을 "일단 멈춤"으로 읽으면 모니터의 버그가 거래
// 중단이 된다.
func TestUnknownCommandIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"command":"restart"}`))
	}))
	defer srv.Close()

	rp := testReporter(t, srv.URL, []byte("s"))
	rp.Publish(beat.Snapshot{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)

	select {
	case c := <-rp.Commands():
		t.Errorf("알 수 없는 명령 %q 가 전달됐다", c)
	case <-time.After(80 * time.Millisecond):
	}
	if rp.ConsecFail() != 0 {
		t.Error("모르는 명령이 전송 실패로 세어졌다 — 서버는 살아 있다")
	}
}

// 발행 전에는 아무것도 보내지 않는다. 빈 스냅샷이 나가면 모니터가 그것을
// 진짜 상태로 읽고 equity 0·무장 해제 알람을 낸다.
func TestNothingSentBeforeFirstPublish(t *testing.T) {
	var hits int32
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer srv.Close()

	rp := testReporter(t, srv.URL, []byte("s"))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	rp.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Errorf("발행 전에 %d회 전송됐다", hits)
	}
	if rp.ConsecFail() != 0 {
		t.Error("발행 전 무전송이 실패로 세어졌다")
	}
}

// 채널이 차 있으면 명령을 버리는데, **그때 Acked 를 갱신하면 안 된다.**
// 갱신하면 흘리지도 못한 명령을 "받아서 처리했다" 고 보고하게 되고, 모니터는
// 그것을 보고 /cancel_shutdown 에 "이미 받아 갔습니다 — 되돌릴 수 없습니다"
// 라고 답한다. 봇은 그 명령을 본 적이 없는데도.
//
// 버퍼가 1 이므로 **읽지 않은 채로** 서로 다른 명령을 둘 보내면 두 번째가
// default 로 떨어진다.
func TestDroppedCommandDoesNotCountAsAcked(t *testing.T) {
	var mu sync.Mutex
	cmd := beat.CmdShutdown
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		c := cmd
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(beat.Reply{Command: c})
	}))
	defer srv.Close()

	rp := testReporter(t, srv.URL, []byte("s"))
	rp.Publish(beat.Snapshot{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)

	// 채널이 차기를 기다린다 — 아무도 읽지 않는다.
	waitFor(t, "첫 명령이 채널에 차기", func() bool { return len(rp.cmds) == 1 })
	if rp.Acked() != beat.CmdShutdown {
		t.Fatalf("Acked = %q, want shutdown", rp.Acked())
	}

	mu.Lock()
	cmd = beat.CmdHalt
	mu.Unlock()

	// halt 는 채널이 차 있어 버려진다. 여러 주기를 지나도 Acked 는 그대로여야 한다.
	time.Sleep(60 * time.Millisecond)
	if got := rp.Acked(); got != beat.CmdShutdown {
		t.Errorf("Acked = %q — 흘리지도 못한 명령을 처리했다고 보고한다", got)
	}
	// 그리고 채널에는 여전히 첫 명령만 있다.
	if c := <-rp.Commands(); c != beat.CmdShutdown {
		t.Errorf("채널의 명령 = %q, want shutdown", c)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("시한 내 %s 실패", what)
}
