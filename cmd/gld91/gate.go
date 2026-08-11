package main

// 이 파일은 모니터 명령이 봇의 운용에 미치는 영향의 전부다.
//
// **이 봇은 매도 주문을 내지 않는다**(스펙 §10). 그래서 종료는 "청산 후
// 종료" 가 아니라 "신규 회차 진입 중단 → 미체결 취소 → 정산에 맡기고 종료"
// 다. 미체결 취소는 exec 의 cancelEverything 이 회차 끝에서 이미 한다.

import (
	"sync"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

// runGate 는 "신규 회차에 들어가도 되는가" 하나를 답한다.
//
// 두 고루틴이 만진다 — 명령 소비 고루틴이 쓰고 회차 루프가 읽는다.
//
// # shutdown 과 halt 가 여기서 같은 이유
//
// 둘 다 프로세스를 끝낸다. 차이는 **다음 기동**에 있다: halt 는 봇이 다시
// 올라와 첫 beat 를 보내면 모니터가 그것을 또 내려 주므로 즉시 다시 멈춘다.
// 그 상태는 모니터가 들고 있고 봇은 들고 있지 않다.
//
// 봇에도 halt 플래그를 두려다 지웠다. `exit` 를 되돌리는 경로가 없어서 한
// 프로세스 안에서는 두 값이 절대 갈리지 않았고 — 변이 시험에서 그 필드를
// 지워도 아무 테스트가 깨지지 않아 드러났다 — 시험할 수 없는 상태를 들고
// 있는 것이 전부였다. 상태를 두 곳에 두면 갈리고, 갈린 쪽이 틀렸을 때 봇이
// 못 켜진다.
type runGate struct {
	mu   sync.Mutex
	exit bool
}

// Apply 는 모니터 명령을 반영한다. 모르는 명령은 아무것도 바꾸지 않는다.
func (g *runGate) Apply(c beat.Command) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if c == beat.CmdShutdown || c == beat.CmdHalt {
		g.exit = true
	}
}

// MayEnterRound 는 새 회차를 잡아도 되는지다.
func (g *runGate) MayEnterRound() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.exit
}

// ShouldExit 는 프로세스를 끝내야 하는지다.
func (g *runGate) ShouldExit() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.exit
}

// fallbackNotifier 는 **모니터가 죽었을 때 봇이 직접 알리는** 경로다.
//
// GLD-7 은 감시 서버가 죽으면 아무도 모른다 — 텔레그램의 침묵이 "이상 없음"
// 과 구분되지 않는다. 봇은 POST 실패를 직접 아니까 그것을 알릴 수 있고,
// 텔레그램 토큰은 자금 권한과 무관하므로 "모니터는 개인키를 갖지 않는다" 는
// 결정과 충돌하지 않는다.
//
// 순수에 가깝다 — 시각을 인자로 받고 부수효과는 send 하나다.
type fallbackNotifier struct {
	// Threshold 는 이만큼 연속 실패하면 알린다.
	Threshold time.Duration
	// Interval 은 beat 주기다. 0 이면 defaultBeatInterval.
	Interval time.Duration
	// Send 는 알림 경로다. nil 이면 아무것도 하지 않는다.
	Send func(string) error
	// Log 는 보낸 것과 실패한 것을 남기는 곳이다. nil 이면 기본 로거를 쓴다.
	// 시험이 갈아 끼운다.
	Log func(string, ...any)

	firstFail time.Time
	notified  bool
}

// Step 은 연속 실패 횟수를 받아 필요하면 알린다.
//
// 실패 횟수를 시각으로 되돌리는 이유: 배선이 이 함수를 부르는 주기와 beat
// 주기가 다르므로, "몇 번 실패했나" 만으로는 얼마나 오래 끊겼는지 알 수 없다.
func (f *fallbackNotifier) Step(consecFail int, now time.Time) {
	iv := f.Interval
	if iv <= 0 {
		iv = defaultBeatInterval
	}
	if consecFail <= 0 {
		if f.notified {
			f.emit("✅ 모니터 연결이 복구됐습니다.")
		}
		f.firstFail, f.notified = time.Time{}, false
		return
	}
	if f.firstFail.IsZero() {
		f.firstFail = now.Add(-time.Duration(consecFail) * iv)
	}
	if !f.notified && now.Sub(f.firstFail) >= f.Threshold {
		f.emit("⚠️ 모니터가 응답하지 않습니다 — 봇은 계속 거래 중이지만 감시가 없습니다.")
		f.notified = true
	}
}

// emit 은 전송 실패를 삼킨다. 알림이 안 나갔다고 거래를 멈추면 감시 장치의
// 장애가 그대로 거래 장애가 된다 — internal/ledger 와 같은 원칙이다.
//
// **다만 삼킨 것을 로그에는 남긴다.** 이것은 최후 경로다 — 모니터가 죽어서
// 부르는 것이므로, 이 알림이 실패하면 그 사실을 관측할 다른 눈이 없다.
// 조용히 삼키면 "알림이 안 왔다" 와 "이상이 없었다" 가 구분되지 않는다.
// 모니터 쪽에서 같은 결함을 이미 한 번 고쳤다(06cbe38).
//
// 경로가 아예 없는 것(Send == nil)도 남긴다. 봇 박스에 BOT_TELEGRAM_TOKEN 을
// 넣지 않으면 이 기능이 통째로 꺼지는데, 그것을 아는 방법이 여기밖에 없다.
func (f *fallbackNotifier) emit(msg string) {
	log := f.Log
	if log == nil {
		log = logf
	}
	if f.Send == nil {
		log("모니터 폴백 알림을 보낼 곳이 없다(%s 미설정): %s", EnvBotTGToken, msg)
		return
	}
	if err := f.Send(msg); err != nil {
		log("모니터 폴백 알림 전송 실패 — 이 알림은 아무에게도 닿지 않았다: %v", err)
		return
	}
	log("모니터 폴백 알림 전송: %s", msg)
}
