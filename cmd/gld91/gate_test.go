package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

// 종료 명령은 신규 회차 진입을 멈춘다. 이 봇은 매도를 내지 않으므로 청산
// 단계가 없다 — 미체결은 회차 끝에 취소되고 포지션은 정산에 맡긴다.
func TestShutdownStopsNewRounds(t *testing.T) {
	g := &runGate{}
	if !g.MayEnterRound() {
		t.Fatal("아무 명령도 없는데 진입이 막혔다")
	}
	g.Apply(beat.CmdShutdown)
	if g.MayEnterRound() {
		t.Error("종료 명령 뒤에 신규 회차에 진입한다")
	}
	if !g.ShouldExit() {
		t.Error("종료 명령 뒤에 종료하지 않는다")
	}
}

// halt 도 프로세스를 끝낸다. 봇 안에서는 shutdown 과 구분되지 않고, 차이는
// 다음 기동에 있다 — 모니터가 halt 를 또 내려 준다(runGate 문서 참고).
//
// none 이 뒤따라도 풀리지 않는다. 이미 회차 진입을 멈춘 뒤에 명령이 거둬져도
// 되돌리지 않는 것이 안전하다 — 되돌리면 종료 도중에 새 회차를 잡는다.
func TestHaltExitsAndDoesNotUnwind(t *testing.T) {
	g := &runGate{}
	g.Apply(beat.CmdHalt)
	if g.MayEnterRound() {
		t.Error("halt 뒤에 신규 회차에 진입한다")
	}
	if !g.ShouldExit() {
		t.Error("halt 인데 종료하지 않는다")
	}
	g.Apply(beat.CmdNone)
	if g.MayEnterRound() || !g.ShouldExit() {
		t.Error("none 이 종료를 되돌렸다 — 종료 도중에 새 회차를 잡게 된다")
	}
}

// 모르는 명령은 아무것도 바꾸지 않는다. 모니터가 오작동해도 봇은 자기가
// 아는 것만 한다 — 모르는 값을 "일단 멈춤" 으로 읽으면 모니터의 버그가
// 거래 중단이 된다.
func TestUnknownCommandDoesNothing(t *testing.T) {
	g := &runGate{}
	for _, c := range []beat.Command{"", "restart", "SHUTDOWN", beat.CmdNone} {
		g.Apply(c)
	}
	if !g.MayEnterRound() || g.ShouldExit() {
		t.Error("모르는 명령이 게이트를 바꿨다")
	}
}

// --- 폴백 알림 ---

type msgSink struct{ msgs []string }

func (r *msgSink) send(m string) error { r.msgs = append(r.msgs, m); return nil }

// 모니터가 죽으면 텔레그램이 조용해지는데, 그 침묵은 "이상 없음" 과 구분되지
// 않는다. 봇이 직접 한 번 알린다 — GLD-7 에는 이 경로가 없었다.
func TestFallbackNotifiesOnceThenRecovers(t *testing.T) {
	r := &msgSink{}
	f := &fallbackNotifier{Threshold: 20 * time.Second, Interval: time.Second, Send: r.send}
	now := time.Unix(1786000000, 0).UTC()

	f.Step(0, now)                     // 정상
	f.Step(5, now.Add(10*time.Second)) // 5초째 끊김 — 아직 임계 미달
	if len(r.msgs) != 0 {
		t.Fatalf("임계 전에 알렸다: %v", r.msgs)
	}
	f.Step(25, now.Add(30*time.Second)) // 25초째 — 알린다
	f.Step(30, now.Add(35*time.Second)) // 계속 끊김 — 조용
	f.Step(0, now.Add(40*time.Second))  // 복구 — 알린다
	f.Step(0, now.Add(50*time.Second))  // 조용

	if len(r.msgs) != 2 {
		t.Fatalf("알림 %d회, want 2: %v", len(r.msgs), r.msgs)
	}
	if !strings.Contains(r.msgs[0], "모니터가 응답하지") {
		t.Errorf("첫 알림 = %q", r.msgs[0])
	}
	if !strings.Contains(r.msgs[1], "복구") {
		t.Errorf("복구 알림 = %q", r.msgs[1])
	}
}

// 끊겼다 복구됐다를 반복하면 매번 알린다 — 그 반복 자체가 신호다.
func TestFallbackRefiresAfterRecovery(t *testing.T) {
	r := &msgSink{}
	f := &fallbackNotifier{Threshold: 10 * time.Second, Interval: time.Second, Send: r.send}
	now := time.Unix(1786000000, 0).UTC()
	for i := 0; i < 3; i++ {
		base := now.Add(time.Duration(i) * time.Minute)
		f.Step(15, base)
		f.Step(0, base.Add(5*time.Second))
	}
	if len(r.msgs) != 6 {
		t.Errorf("알림 %d회, want 6 (끊김 3 + 복구 3): %v", len(r.msgs), r.msgs)
	}
}

// 알림 전송이 실패해도 봇은 죽지 않는다.
func TestFallbackSurvivesSendFailure(t *testing.T) {
	f := &fallbackNotifier{
		Threshold: time.Second, Interval: time.Second,
		Send: func(string) error { return errors.New("전송 실패") },
	}
	now := time.Unix(1786000000, 0).UTC()
	f.Step(5, now)
	f.Step(5, now.Add(2*time.Second)) // 패닉하지 않는다
}

// Send 가 nil 이어도 패닉하지 않는다 — 텔레그램을 배선하지 않은 실행이 있다.
func TestFallbackWithoutSend(t *testing.T) {
	f := &fallbackNotifier{Threshold: time.Second, Interval: time.Second}
	f.Step(5, time.Unix(1786000000, 0).UTC())
}

// **이것은 최후 경로다.** 모니터가 죽어서 부르는 것이므로, 이 알림이 실패하면
// 그 사실을 관측할 다른 눈이 없다. 조용히 삼키면 "알림이 안 왔다" 와
// "이상이 없었다" 가 구분되지 않는다 — 모니터 쪽에서 이미 한 번 고친 결함이다.
func TestFallbackLogsSendFailure(t *testing.T) {
	var logged []string
	f := &fallbackNotifier{
		Threshold: time.Second, Interval: time.Second,
		Send: func(string) error { return errors.New("telegram: 401 Unauthorized") },
		Log:  func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	}
	f.Step(5, time.Unix(1786000000, 0).UTC())
	if len(logged) != 1 {
		t.Fatalf("기록 %d줄, want 1: %v", len(logged), logged)
	}
	if !strings.Contains(logged[0], "401") || !strings.Contains(logged[0], "닿지 않았다") {
		t.Errorf("실패 기록에 원인이 없다: %q", logged[0])
	}
}

// 성공도 남긴다. 성공이 조용하면 이 경로가 실제로 도는지 확인할 방법이 없다.
func TestFallbackLogsSuccess(t *testing.T) {
	var logged []string
	r := &msgSink{}
	f := &fallbackNotifier{
		Threshold: time.Second, Interval: time.Second, Send: r.send,
		Log: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	}
	f.Step(5, time.Unix(1786000000, 0).UTC())
	if len(logged) != 1 || !strings.Contains(logged[0], "모니터가 응답하지") {
		t.Errorf("성공 기록 = %v", logged)
	}
}

// **경로가 아예 없는 것도 알려야 한다.** 봇 박스에 BOT_TELEGRAM_TOKEN 을
// 넣지 않으면 이 기능이 통째로 꺼지는데, 배포에서 실제로 그 상태였다.
func TestFallbackLogsMissingSink(t *testing.T) {
	var logged []string
	f := &fallbackNotifier{
		Threshold: time.Second, Interval: time.Second,
		Log: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	}
	f.Step(5, time.Unix(1786000000, 0).UTC())
	if len(logged) != 1 || !strings.Contains(logged[0], EnvBotTGToken) {
		t.Errorf("경로 부재 기록에 환경변수 이름이 없다: %v", logged)
	}
}
