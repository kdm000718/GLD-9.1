package main

// gld91-monitor 는 봇(A호스트)을 별도 호스트(B)에서 감시한다.
//
// 설계서: docs/superpowers/specs/2026-08-11-gld91-heartbeat-monitor-design.md
//
// # 왜 별도 바이너리인가
//
// 계약(internal/beat)을 봇과 **같은 타입**으로 쓰려면 같은 모듈이어야 한다.
// 별도 저장소로 두면 계약이 두 벌이 되고, 갈린 쪽이 틀렸을 때 알 방법이 없다
// (internal/live/rounds.go 와 같은 원칙). 배포만 따로 하면 B호스트는 소스를
// 갖되 개인키를 갖지 않는다.
//
// # 연결은 봇 → 모니터 단방향이다
//
// 개인키가 있는 A호스트는 인바운드 포트를 열지 않는다. 봇이 3초마다
// POST 하고 모니터가 200 에 명령을 실어 답한다 — 한 번의 왕복이 하트비트와
// 명령을 동시에 해결하고, GLD-7 의 공유 마운트 세 파일이 통째로 사라진다.

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// evalInterval 은 판정 주기다. beat 주기와 같은 3초 — 판정 자체는 순수 함수라
// 값 몇 개를 보는 것이 전부이므로 싸다.
const evalInterval = 3 * time.Second

func main() {
	if err := run(); err != nil {
		log.Fatalf("모니터: %v", err)
	}
}

func run() error {
	cfg, err := loadMonitorConfig(os.Getenv)
	if err != nil {
		return err
	}

	st := newState(wantConsts(), cfg.BeatSecret)
	// 이력 저장소. 경로가 없으면 아무것도 하지 않는다(메모리에만 둔다).
	st.attachStore(&store{path: cfg.StatePath, logf: log.Printf})
	tg := newTG(cfg.TGToken, cfg.TGChat)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go runAlarms(ctx, st, tg.Send)
	go tg.Poll(ctx, st)

	// 정산 관측. 키가 없으면 그 고루틴만 꺼진다 — 하트비트 감시는 그대로 돈다.
	var rc *rest.Client
	if cfg.APIKey != "" {
		rc = rest.New(cfg.APIKey)
	}
	go runSettleWatcher(ctx, st, rc, cfg.SharedKey)
	go runReportTicker(ctx, st, tg.Send, cfg.ReportInterval)

	// 핸들러를 특정 경로로 좁히지 않는 것이 의도다. 서명 없는 요청은 어느
	// 경로든 404 이므로 경로 자체가 정보를 주지 않는다.
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           http.HandlerFunc(st.handleBeat),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("모니터 시작: %s (봇 상수 %+v)", cfg.Listen, wantConsts())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runAlarms 는 주기마다 판정하고, 새로 울릴 것과 복구된 것을 알린다.
//
// 판정(rule.Evaluate)과 에지 처리(rule.Latch)는 순수 함수다. 이 고루틴이 하는
// 일은 시각을 주고 결과를 사람에게 흘리는 배선뿐이다.
func runAlarms(ctx context.Context, st *state, notify func(string)) {
	t := time.NewTicker(evalInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fire, resolved := st.Step(time.Now().UTC())
			// **보낸 것을 로컬에도 남긴다.** 텔레그램이 잘못 설정돼 있으면
			// 알람이 통째로 사라지는데, 로그가 없으면 그 사실조차 알 수
			// 없다 — 감시 장치가 조용한 것과 이상이 없는 것이 구분되지
			// 않는 상태이고, 이 저장소가 반복해서 값을 치른 모양이다.
			for _, f := range fire {
				msg := f.Level.String() + " " + f.Message
				log.Printf("[알람 %s] %s", f.Key, f.Message)
				notify(msg)
			}
			for _, key := range resolved {
				log.Printf("[복구 %s]", key)
				notify("✅ 복구: " + key)
			}
		}
	}
}

// runReportTicker 는 정기 리포트를 보낸다.
//
// 4시간이면 48회차다. 1시간(12회차)마다 보내면 알림이 흔해져 사람이 보지
// 않게 되는데, 이 봇은 confidence 문턱 미달로 조용한 것이 정상이라 리포트
// 대부분이 "특별한 일 없음" 이 된다. 대신 알람이 사실상 유일한 감지 경로가
// 되므로 rule 쪽 규칙이 촘촘해야 한다.
func runReportTicker(ctx context.Context, st *state, notify func(string), every time.Duration) {
	if every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			snap, _ := st.Latest()
			msg := formatReport(accumulate(st.Participations()), snap, every.String())
			log.Print("[리포트]\n" + msg)
			notify(msg)
		}
	}
}
