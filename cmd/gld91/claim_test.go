package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/claim"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
)

// 이 파일이 지키는 것: **회수가 거래를 방해하지 않는다.**
//
// 회수 한 건은 영수증 폴링만 최대 90초다. 회차 종료 직후에 그것을 기다리면
// 다음 회차의 첫 호가가 그만큼 늦고, 이 봇은 회차 시작에 한 번 거는 것이
// 전부이므로 그 지연이 곧 큐 위치다.

// fakeSource 는 회수 대상 조회를 가로챈다. 실제 GraphQL 을 절대 부르지 않는다.
type fakeSource struct {
	mu        sync.Mutex
	calls     int
	err       error
	positions []claim.Position
	// block 이 nil 이 아니면 조회가 여기서 멈춘다. 단일 비행을 시험하려면
	// 앞 바퀴를 붙잡아 둘 수단이 있어야 한다.
	block chan struct{}
	// entered 는 조회에 **들어왔다**는 신호다. block 과 짝이다.
	entered chan struct{}
}

func (f *fakeSource) Claimable(ctx context.Context, _ string) ([]claim.Position, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.entered != nil {
		select {
		case f.entered <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return f.positions, f.err
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// testAutoClaim 은 배선을 **newAutoClaim 을 거쳐** 만든다. 손으로 구조체를
// 채우면 그 생성자의 게이트를 시험하지 못한다.
func testAutoClaim(t *testing.T, env map[string]string, src claim.PositionSource) (*autoClaim, *[]string) {
	t.Helper()
	s, err := auth.NewSigner(testSignerHex)
	if err != nil {
		t.Fatalf("auth.NewSigner: %v", err)
	}
	var lines []string
	var mu sync.Mutex
	log := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	cfg := &Config{AutoClaim: true, RPC: "http://127.0.0.1:1/never-called"}
	if v, ok := env["auto-claim"]; ok && v == "false" {
		cfg.AutoClaim = false
	}
	a := newAutoClaim(cfg, "0x27000F84214f79B0600aa86841958b13ac98242a", s, nil,
		func(k string) string { return env[k] }, log)
	if a != nil && src != nil {
		a.claimer.Source = src
	}
	return a, &lines
}

func armedEnv() map[string]string {
	return map[string]string{
		EnvZeroDevRPC: "https://rpc.zerodev.app/api/v3/deadbeef-0000-0000-0000-000000000000/chain/56",
		EnvClaimArm:   LiveArmValue,
	}
}

func joined(lines *[]string) string { return strings.Join(*lines, "\n") }

// ---------------------------------------------------------------------------
// 켜짐/꺼짐 게이트
// ---------------------------------------------------------------------------

// **번들러가 없으면 켜지 않는다.** 그리고 그 사실을 크게 찍는다 — 조용히
// 꺼지면 운영자는 회수가 되고 있다고 믿고, 포지션은 계정에 계속 쌓인다.
func TestAutoClaimNeedsTheBundler(t *testing.T) {
	a, lines := testAutoClaim(t, map[string]string{EnvClaimArm: LiveArmValue}, nil)
	if a != nil {
		t.Fatal("ZERODEV_RPC 없이 회수 배선을 만들었다 — UserOperation 을 보낼 곳이 없다")
	}
	if !strings.Contains(joined(lines), EnvZeroDevRPC) {
		t.Errorf("꺼진 이유를 찍지 않았다: %v", *lines)
	}
}

func TestAutoClaimCanBeTurnedOff(t *testing.T) {
	env := armedEnv()
	env["auto-claim"] = "false"
	a, lines := testAutoClaim(t, env, nil)
	if a != nil {
		t.Fatal("-auto-claim=false 인데 배선을 만들었다")
	}
	if !strings.Contains(joined(lines), "auto-claim") {
		t.Errorf("꺼진 이유를 찍지 않았다: %v", *lines)
	}
}

// 꺼진 배선의 모든 호출이 안전해야 한다. 회수가 없다고 거래가 죽으면,
// 부가 기능의 부재가 거래 장애가 된다.
func TestNilAutoClaimIsSafe(t *testing.T) {
	var a *autoClaim
	a.after(context.Background(), "btc-updown-5m-1")
	a.wait()
}

// **전송 게이트는 문자열 완전 일치 하나뿐이다.** LIVE_ARM 과 같은 규약이고,
// 공백을 다듬지 않는 것도 같은 이유다 — 무장은 실수로 켜질 수 없어야 한다.
func TestClaimArmIsExactMatch(t *testing.T) {
	for _, v := range []string{"", "true", "1", "i_understand_the_risk", " " + LiveArmValue, LiveArmValue + " "} {
		if claimArmed(v) {
			t.Errorf("%q 를 무장으로 읽었다", v)
		}
	}
	if !claimArmed(LiveArmValue) {
		t.Errorf("%q 를 무장으로 읽지 못했다", LiveArmValue)
	}
}

// 전송 게이트가 실제로 [claim.Claimer.Send] 에 닿아야 한다. 안 닿으면
// CLAIM_ARM 없이도 돈이 움직이거나, 설정해도 안 움직인다.
func TestClaimArmReachesTheSendFlag(t *testing.T) {
	env := armedEnv()
	a, _ := testAutoClaim(t, env, nil)
	if a == nil || !a.claimer.Send {
		t.Fatal("CLAIM_ARM 이 맞는데 전송이 꺼져 있다")
	}
	delete(env, EnvClaimArm)
	b, lines := testAutoClaim(t, env, nil)
	if b == nil {
		t.Fatal("CLAIM_ARM 이 없다고 배선을 아예 만들지 않았다 — 조립까지는 돌아야 DRY-RUN 이 무언가를 증명한다")
	}
	if b.claimer.Send {
		t.Fatal("CLAIM_ARM 이 없는데 전송이 켜져 있다")
	}
	if !strings.Contains(joined(lines), EnvClaimArm) {
		t.Errorf("전송이 꺼진 사실을 찍지 않았다: %v", *lines)
	}
}

// **번들러 URL 을 로그에 그대로 찍지 않는다.** ZeroDev 프로젝트 ID 가 들어
// 있고, 이 저장소의 로그는 보고서에 붙는다.
func TestBundlerURLIsRedactedInTheLog(t *testing.T) {
	env := armedEnv()
	_, lines := testAutoClaim(t, env, nil)
	if strings.Contains(joined(lines), "deadbeef-0000-0000-0000-000000000000") {
		t.Errorf("번들러 URL 의 프로젝트 ID 가 로그에 그대로 실렸다: %v", *lines)
	}
}

// ---------------------------------------------------------------------------
// 배경 실행과 단일 비행
// ---------------------------------------------------------------------------

// after 는 **곧바로 돌아와야 한다.** 여기서 기다리면 다음 회차의 첫 호가가
// 그만큼 늦는다.
func TestAfterDoesNotBlockTheRoundLoop(t *testing.T) {
	src := &fakeSource{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	a, _ := testAutoClaim(t, armedEnv(), src)

	done := make(chan struct{})
	go func() { a.after(context.Background(), "btc-updown-5m-1"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("after 가 회수를 기다렸다 — 회차 루프가 그만큼 멈춘다")
	}
	<-src.entered // 배경에서 실제로 돌고 있다
	close(src.block)
	a.wait()
}

// **겹치지 않는다.** 같은 계정에 UserOperation 두 개를 동시에 띄우면 nonce 가
// 겹쳐 하나는 반드시 실패한다.
func TestOverlappingRoundsDoNotStack(t *testing.T) {
	src := &fakeSource{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	a, lines := testAutoClaim(t, armedEnv(), src)

	a.after(context.Background(), "btc-updown-5m-1")
	<-src.entered // 첫 바퀴가 조회에 들어갔다

	a.after(context.Background(), "btc-updown-5m-2")
	a.after(context.Background(), "btc-updown-5m-3")

	if n := src.count(); n != 1 {
		t.Errorf("조회 %d회 — 앞 바퀴가 도는 동안 새 바퀴를 띄웠다", n)
	}
	if !strings.Contains(joined(lines), "건너뛴다") {
		t.Errorf("건너뛴 사실을 찍지 않았다: %v", *lines)
	}

	close(src.block)
	a.wait()

	// 그리고 앞 바퀴가 끝나면 **다시 받아야 한다.** 한 번 겹쳤다고 영영
	// 잠기면 회수가 통째로 멈춘다.
	src2 := &fakeSource{}
	a.claimer.Source = src2
	a.after(context.Background(), "btc-updown-5m-4")
	a.wait()
	if src2.count() != 1 {
		t.Errorf("앞 바퀴가 끝난 뒤에도 새 바퀴가 돌지 않았다 (조회 %d회)", src2.count())
	}
}

// 회수 실패는 **로그다.** 무장 해제도, 회차 중단도 아니다.
func TestClaimFailureIsOnlyLogged(t *testing.T) {
	src := &fakeSource{err: errors.New("GraphQL 이 죽었다")}
	a, lines := testAutoClaim(t, armedEnv(), src)

	a.after(context.Background(), "btc-updown-5m-1")
	a.wait()

	if !strings.Contains(joined(lines), "GraphQL 이 죽었다") {
		t.Errorf("실패 사유를 찍지 않았다: %v", *lines)
	}
	// 그리고 다음 회차가 다시 시도해야 한다.
	a.after(context.Background(), "btc-updown-5m-2")
	a.wait()
	if src.count() != 2 {
		t.Errorf("조회 %d회 — 한 번 실패했다고 회수를 포기했다", src.count())
	}
}

// 종료 중의 실패는 실패가 아니다. ctx 가 죽어서 난 에러를 회수 실패로 찍으면
// 정상 종료마다 가짜 경보가 하나씩 남는다.
func TestShutdownIsNotReportedAsAFailure(t *testing.T) {
	src := &fakeSource{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	a, lines := testAutoClaim(t, armedEnv(), src)

	ctx, cancel := context.WithCancel(context.Background())
	a.after(ctx, "btc-updown-5m-1")
	<-src.entered
	cancel()
	a.wait()

	if strings.Contains(joined(lines), "실패") {
		t.Errorf("종료를 회수 실패로 찍었다: %v", *lines)
	}
}

// 회수 대상이 없으면 **조용해야 한다.** 대부분의 회차가 이쪽이고, 5분마다
// "대상 없음"을 찍으면 회차 로그가 그것으로 덮인다.
func TestNothingToClaimIsQuiet(t *testing.T) {
	src := &fakeSource{}
	a, lines := testAutoClaim(t, armedEnv(), src)
	a.after(context.Background(), "btc-updown-5m-1")
	a.wait()

	for _, l := range *lines {
		if strings.HasPrefix(l, "Auto-Claim [btc") {
			t.Errorf("회수 대상이 없는데 회차 줄을 찍었다: %q", l)
		}
	}
	if src.count() != 1 {
		t.Errorf("조회 %d회, 기대 1회", src.count())
	}
}

// wait 는 도는 바퀴를 실제로 기다려야 한다. 안 기다리면 원장이 먼저 닫히고,
// 회수는 성공했는데 정산 행만 사라진다.
func TestWaitWaitsForTheRunningPass(t *testing.T) {
	src := &fakeSource{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	a, _ := testAutoClaim(t, armedEnv(), src)

	a.after(context.Background(), "btc-updown-5m-1")
	<-src.entered

	waited := make(chan struct{})
	go func() { a.wait(); close(waited) }()
	select {
	case <-waited:
		t.Fatal("wait 가 도는 바퀴를 두고 돌아왔다")
	case <-time.After(50 * time.Millisecond):
	}
	close(src.block)
	waitFor(t, "wait 가 돌아오기", func() bool {
		select {
		case <-waited:
			return true
		default:
			return false
		}
	})
}

// ---------------------------------------------------------------------------
// 호출부 — 함수가 맞아도 부르는 자리가 없으면 아무 일도 안 일어난다
// ---------------------------------------------------------------------------

// **이 저장소의 반복되는 고장이 그것이다.** 위의 시험들은 autoClaim 을 직접
// 부르므로, 회차 루프가 그것을 부르지 않게 되어도 전부 통과한다. 여기서는
// 호출부가 소스에 있는지를 본다.
//
// loop() 를 통째로 돌리는 시험은 WS·REST·모델이 전부 필요해서 이 배선 하나를
// 보자고 세울 수 없다. 소스 스캔은 거친 대신 **호출부가 사라지는 순간**을
// 정확히 잡는다.
func TestRoundLoopCallsAutoClaim(t *testing.T) {
	s := mainSource(t)
	for _, want := range []string{
		// 회차 종료마다 한 바퀴.
		"claimer.after(runCtx, t.round.Slug)",
		// 종료 때 원장이 닫히기 전에 기다린다.
		"defer claimer.wait()",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("main.go 에 %q 가 없다 — 배선이 빠지면 회수는 영영 일어나지 않는다", want)
		}
	}
	// 그리고 **취소가 대기보다 먼저** defer 돼야 한다(LIFO 라 나중에 적힌
	// 것이 먼저 돈다). 순서가 뒤집히면 종료가 영수증 폴링 90초를 기다린다.
	waitAt := strings.Index(s, "defer claimer.wait()")
	cancelAt := strings.Index(s, "defer cancelAll()")
	if waitAt < 0 || cancelAt < 0 || cancelAt < waitAt {
		t.Errorf("defer 순서가 틀렸다 (wait@%d, cancelAll@%d) — cancelAll 이 뒤에 적혀야 먼저 돈다", waitAt, cancelAt)
	}
}
