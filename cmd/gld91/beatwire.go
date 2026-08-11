package main

// 이 파일은 봇의 감시 배선을 한 곳에 모은다 — 관측 훅, 스냅샷 발행, 명령
// 소비, 모니터 사망 알림.
//
// # 모니터 없이도 봇은 돈다
//
// `BEAT_ENDPOINT` 가 없으면 배선 전체가 꺼진다(`nil` 와이어). 감시가 없다고
// 거래를 막으면 감시 장치의 부재가 거래 장애가 된다 — DRY-RUN 이나 로컬
// 실행에서 매번 모니터를 띄우게 만들 이유가 없다.
//
// # 설정을 Config 에 넣지 않은 이유
//
// `config.go` 는 봇의 거래 설정이고 이것은 감시 설정이다. 섞으면 감시 쪽
// 변경이 거래 쪽 검사를 건드리게 된다. 환경변수 셋으로 자족한다.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
	"github.com/kdm000718/GLD-9.1/internal/exec"
)

const (
	EnvBeatEndpoint = "BEAT_ENDPOINT"
	EnvBeatSecret   = "BEAT_SECRET"
	// EnvBotTGToken 은 **모니터가 죽었을 때 봇이 직접 알리는** 경로다.
	// 없으면 그 알림만 꺼지고 나머지는 그대로 돈다.
	EnvBotTGToken = "BOT_TELEGRAM_TOKEN"
	EnvBotTGChat  = "BOT_TELEGRAM_CHAT_ID"
)

// fallbackThreshold 는 모니터 무응답을 사람에게 알리기까지의 시간이다.
// beat 3초 주기의 20회 — 일시적 네트워크 끊김으로는 닿지 않는다.
const fallbackThreshold = 60 * time.Second

// fallbackTick 은 폴백 판정 주기다.
const fallbackTick = 15 * time.Second

// beatWire 는 봇 쪽 감시 배선 전부다. nil 이면 감시가 꺼진 것이고, 모든
// 메서드가 안전하게 아무것도 하지 않는다.
type beatWire struct {
	rp   *reporter
	gate *runGate
	fb   *fallbackNotifier

	version string
	armed   bool

	mu sync.Mutex
	// obs 는 마지막 관측이다. 회차 밖에서는 제로값이다.
	obs exec.Observation
	// ctx 는 마지막으로 발행한 회차 맥락이다. 관측 훅은 회차를 모르므로
	// (exec 는 계약 타입을 임포트하지 않는다) 여기서 합친다.
	ctx snapshotInput
	// skips 는 사유별 누적 스킵 수다. 모니터의 연속 판정과 달리 이것은
	// 리포트용 누적이다.
	skips map[beat.SkipReason]int
	// probes 는 발행 때마다 당겨오는 루프 상태다(loopProbes 참고).
	probes loopProbes
}

// newBeatWire 는 환경변수를 읽어 배선을 만든다. 엔드포인트가 없으면 nil 이다.
func newBeatWire(getenv func(string) string, version string, armed bool, bootID string) *beatWire {
	endpoint := strings.TrimSpace(getenv(EnvBeatEndpoint))
	if endpoint == "" {
		return nil
	}
	w := &beatWire{
		rp:      newReporter(endpoint, []byte(strings.TrimSpace(getenv(EnvBeatSecret))), bootID),
		gate:    &runGate{},
		fb:      &fallbackNotifier{Threshold: fallbackThreshold},
		version: version,
		armed:   armed,
		skips:   map[beat.SkipReason]int{},
	}
	if tok := strings.TrimSpace(getenv(EnvBotTGToken)); tok != "" {
		chat := strings.TrimSpace(getenv(EnvBotTGChat))
		w.fb.Send = botTelegramSender(tok, chat)
	}
	return w
}

// MayEnterRound 는 새 회차를 잡아도 되는지다. 배선이 없으면 언제나 참이다.
func (w *beatWire) MayEnterRound() bool { return w == nil || w.gate.MayEnterRound() }

// ShouldExit 는 모니터가 종료를 지시했는지다.
func (w *beatWire) ShouldExit() bool { return w != nil && w.gate.ShouldExit() }

// Observe 는 exec.Runner.Observe 에 꽂는다.
//
// **여기서 네트워크를 타지 않는다.** 회차당 6,000 번 불리므로 원자적 교체
// 하나로 끝나야 한다(reporter.Publish 가 그렇다).
func (w *beatWire) Observe(o exec.Observation) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.obs = o
	in := w.ctx
	w.mu.Unlock()

	in.Obs = o
	w.publish(in)
}

// Report 는 회차 경계에서 맥락을 갱신하고 한 번 발행한다.
//
// 관측 훅은 회차·자본·스킵 사유를 모른다(exec 는 계약 타입을 임포트하지
// 않는다). 그 맥락이 여기서 들어오고, 이후 관측은 그것을 재사용한다.
func (w *beatWire) Report(in snapshotInput) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if in.Round.Slug != w.ctx.Round.Slug {
		// 회차가 바뀌면 이전 회차의 관측을 물려주지 않는다 — 그러면 새 회차
		// 시작 직후에 지난 회차의 노출이 실려 나간다.
		w.obs = exec.Observation{}
	}
	// 스킵 사유는 입력에서 유도한다 — 힌트 필드를 따로 두면 그 둘이 갈리고,
	// 갈린 쪽이 틀렸을 때 모니터가 엉뚱한 사유로 판정한다.
	if _, reason := roundState(in); reason != "" {
		w.skips[reason]++
	}
	in.Obs = w.obs
	in.Skips = copySkips(w.skips)
	w.ctx = in
	w.mu.Unlock()

	w.publish(in)
}

// loopProbes 는 하트비트가 **발행할 때마다 새로 읽어야 하는** 값들이다.
//
// # 왜 Report 인자가 아니라 탐침인가
//
// 이 셋은 회차 경계가 아니라 초 단위로 변한다. snapshotInput 에 실어
// 나르면 `Report` 호출부 네 곳이 전부 최신값을 알아야 하고, 한 곳이라도
// 빠뜨리면 그 경로의 스냅샷만 조용히 옛 값을 싣는다. 발행 통로가 하나뿐이니
// 거기서 당겨오는 것이 맞다.
//
// 각 함수는 뮤텍스 읽기 하나로 끝나야 한다 — [beatWire.Observe] 가 회차당
// 6,000 번 부른다. 네트워크를 타는 함수를 여기 꽂으면 마켓메이킹 루프가
// 감시 때문에 멎는다.
type loopProbes struct {
	// WSLastDataAt 은 마지막 마켓데이터 수신 시각이다(하트비트 제외).
	WSLastDataAt func() time.Time
	// FillsPollAt 은 마지막 체결 조회 시도 시각이다.
	FillsPollAt func() time.Time
	// RateRemaining 은 남은 REST 요청 예산이다.
	RateRemaining func() int
}

// SetProbes 는 탐침을 꽂는다. 꽂지 않은 필드는 제로값으로 나간다.
//
// **nil 필드는 기존 값을 지우지 않는다.** 탐침의 주인이 둘로 나뉘어 있기
// 때문이다 — 체결·예산은 REST 배선이 끝난 곳에서, WS 수신 시각은 소켓이
// 만들어지는 곳에서 꽂는다. 통째로 덮어쓰면 나중에 부르는 쪽이 먼저 꽂힌
// 탐침을 조용히 뽑아 버리고, 그 필드만 영원히 제로가 된다.
func (w *beatWire) SetProbes(p loopProbes) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if p.WSLastDataAt != nil {
		w.probes.WSLastDataAt = p.WSLastDataAt
	}
	if p.FillsPollAt != nil {
		w.probes.FillsPollAt = p.FillsPollAt
	}
	if p.RateRemaining != nil {
		w.probes.RateRemaining = p.RateRemaining
	}
}

func (w *beatWire) publish(in snapshotInput) {
	in.Version = w.version
	in.Armed = w.armed
	in.Acked = w.rp.Acked()

	w.mu.Lock()
	p := w.probes
	w.mu.Unlock()
	if p.WSLastDataAt != nil {
		in.WSLastDataAt = p.WSLastDataAt()
	}
	if p.FillsPollAt != nil {
		in.FillsPollAt = p.FillsPollAt()
	}
	if p.RateRemaining != nil {
		in.RateRemaining = p.RateRemaining()
	}

	w.rp.Publish(buildSnapshot(in))
}

// Run 은 전송·명령 소비·폴백 알림 셋을 띄운다. ctx 가 끝날 때까지 돈다.
func (w *beatWire) Run(ctx context.Context) {
	if w == nil {
		return
	}
	go w.rp.Run(ctx)

	// 명령 소비. 회차 루프를 블록하지 않게 게이트만 바꾼다.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-w.rp.Commands():
				w.gate.Apply(c)
				logf("모니터 명령: %s", c)
			}
		}
	}()

	// 모니터 사망 감지. GLD-7 은 감시 서버가 죽으면 아무도 몰랐다 —
	// 텔레그램의 침묵이 "이상 없음" 과 구분되지 않는다.
	go func() {
		t := time.NewTicker(fallbackTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.fb.Step(w.rp.ConsecFail(), time.Now())
			}
		}
	}()
}

func copySkips(m map[beat.SkipReason]int) map[beat.SkipReason]int {
	out := make(map[beat.SkipReason]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// botTelegramSender 는 봇이 직접 쓰는 최소 전송기다.
//
// 모니터의 것과 코드가 겹치지만 **패키지가 다르고 목적이 다르다** — 이쪽은
// 모니터가 죽었을 때만 쓰는 비상 경로이고, 공유하려면 둘 중 하나가
// 상대 패키지를 임포트해야 한다. 그 결합이 이 중복보다 비싸다.
func botTelegramSender(token, chatID string) func(string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	return func(msg string) error {
		body, err := json.Marshal(map[string]any{"chat_id": chatID, "text": msg})
		if err != nil {
			return err
		}
		resp, err := client.Post("https://api.telegram.org/bot"+token+"/sendMessage",
			"application/json", bytes.NewReader(body))
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("telegram: %s", resp.Status)
		}
		return nil
	}
}

// newBootID 는 이 프로세스의 식별자다.
//
// **mtime 하트비트는 크래시루프를 완벽히 건강하게 보고한다** — 3초마다 죽고
// 살아나도 파일은 계속 신선하기 때문이다. 이 값의 변화가 재시작을 드러내는
// 유일한 신호이므로, 기동마다 반드시 달라야 한다.
func newBootID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 난수를 못 얻으면 시각으로 대체한다. 같은 나노초에 두 번 기동할 수
		// 없으므로 재시작 감지는 그대로 성립한다.
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// buildVersion 은 로그·감시에 남길 빌드 식별자다. 커밋 해시가 있으면 그것을
// 쓴다 — 모니터의 "다른 바이너리가 돌고 있다" 판정에 쓰인다.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return "gld91 " + s.Value[:7]
			}
		}
	}
	return "gld91 unknown"
}
