// Command gld91 은 predict.fun BTC 5분 Up/Down 시장의 마켓메이킹 봇이다.
//
// # 이 바이너리가 하는 일
//
//	설정을 읽고 → 자가 점검 다섯을 하고 → 무장 여부를 정하고 → 회차를 돈다.
//
// 판단은 전부 다른 패키지에 있다. 목표가는 internal/quote, 크기는
// internal/risk, 예측은 internal/live, 집행은 internal/exec 다. 여기 있는
// 것은 배선과 게이트뿐이다.
//
// # 무장
//
// `LIVE_ARM` 이 **정확히** `I_UNDERSTAND_THE_RISK` 일 때만 주문이 전송된다.
// `"true"`·`"1"`·소문자·앞뒤 공백은 전부 DRY-RUN 이다. 그리고 그 값이 맞아도
// [armingBlockers] 가 비어 있지 않으면 무장하지 않고 **종료한다** — 무장을
// 요청했는데 조용히 DRY-RUN 으로 내려가면 운영자는 실거래 중이라고 믿는다.
//
// **DRY-RUN 에서도 서명은 한다.** 서명 경로가 실거래와 달라지면 DRY-RUN 이
// 아무것도 증명하지 못한다(orders.go).
//
// # 로그에 주소를 찍지 않는다
//
// 이 저장소는 GitHub 에 올라가고 DRY-RUN 로그가 보고서에 붙는다. 그래서 이
// 바이너리는 지갑 주소도 계정 주소도 키도 출력하지 않는다 — 서명자 대조는
// 판정만 찍는다. 주소를 눈으로 봐야 하면 cmd/signercheck 를 쓴다.
//
// 사용법:
//
//	set -a; . ~/.config/predictfun/env; set +a
//	GOTOOLCHAIN=local go run ./cmd/gld91 -minutes 10
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/kdm000718/GLD-9.1/internal/exec"
	"github.com/kdm000718/GLD-9.1/internal/kernel"
	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

func main() {
	cfg := Flags(flag.CommandLine)
	flag.Parse()
	cfg.Arm = os.Getenv(EnvLiveArm)

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(1)
	}
}

// logf 는 UTC 시각을 앞에 붙여 한 줄 찍는다. **주소·키·토큰은 넣지 않는다.**
func logf(format string, args ...any) {
	fmt.Printf("%s %s\n", time.Now().UTC().Format("15:04:05.000"), fmt.Sprintf(format, args...))
}

func run(cfg *Config) error {
	if err := checkConfig(cfg); err != nil {
		return err
	}

	// --- 자가 점검 1: 환경변수 -------------------------------------------
	secrets, err := LoadSecrets(os.Getenv)
	if err != nil {
		return err
	}
	wantArm := Armed(cfg.Arm)
	logf("기동 — LIVE_ARM %s", armLabel(cfg.Arm))

	// --- 인스턴스 단일성 ---------------------------------------------------
	// 두 프로세스가 같은 원장을 열면 헤더가 두 번 쓰이고, 더 나쁘게는 서로의
	// 노출을 모른 채 각자 cap 까지 걸어 실제 노출이 두 배가 된다.
	lock, err := lockInstance(cfg.lockPath())
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	signer, err := auth.NewSigner(secrets.PrivateKey)
	if err != nil {
		// 원본 에러를 감싸지 않는다 — 키가 섞여 나갈 경로를 만들지 않는다.
		return fmt.Errorf("%s 를 키로 읽지 못했다 (64자리 hex 여야 한다)", EnvKey)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// --- 자가 점검 2: 서명자 대조 (cmd/signercheck 와 같은 함수) -----------
	verifier := kernel.Verifier{RPC: cfg.RPC, Validator: cfg.Validator}
	if err := verifySigner(ctx, verifier, secrets.Account, signer); err != nil {
		return fmt.Errorf("서명자 대조 실패: %w — 이 키로 만든 주문 서명은 이 계정에서 전부 거부된다", err)
	}
	logf("자가 점검 2/5: 서명자 대조 ✅ (키의 EOA 가 계정의 등록 서명자다)")

	// --- 자가 점검 3: 모델 --------------------------------------------------
	m, err := loadModel(cfg.ModelPath)
	if err != nil {
		return err
	}
	logf("자가 점검 3/5: 모델 로드 ✅ (%s, 피처 %d개, 이름 대조 통과)", cfg.ModelPath, len(m.Coef))

	rc := rest.New(secrets.APIKey)
	rc.BaseURL = cfg.RestBaseURL
	rc.SetTokenSource(&auth.Authenticator{Rest: rc, Signer: signer})
	// **계정 주소가 쿼리 문자열에 실린다** — 체결 조회가 signerAddress 로
	// 서버 측 필터를 건다(fills.go). 전송이 실패하면 net/http 는 *url.Error 에
	// 전체 URL 을 그대로 싣고, 그 에러는 이 바이너리의 로그로 나간다. 이
	// 저장소는 주소를 로그에 남기지 않기로 했으므로(패키지 문서) 클라이언트가
	// 통째로 지우게 한다.
	rc.Redact(secrets.Account)

	// --- 자가 점검 5: 크래시 복구 대조 -------------------------------------
	//
	// 순서상 4번보다 먼저 하는 이유: equity 조회와 같은 REST 를 쓰므로, 대조가
	// 먼저 실패하면 equity 를 물어볼 이유가 없다.
	reconcileOK := true
	if err := reconcile(ctx, rc, cfg.LedgerPath); err != nil {
		if errors.Is(err, ErrReconcileMismatch) {
			// **어긋난 것은 언제나 종료다.** 추측으로 이어가지 않는다.
			return fmt.Errorf("자가 점검 5/5: %w", err)
		}
		// 조회 실패는 "다르다" 가 아니라 "모른다" 다. 무장 상태면 종료하고,
		// 아무것도 전송하지 않는 DRY-RUN 이면 무장만 막고 계속 돈다.
		if wantArm {
			return fmt.Errorf("자가 점검 5/5: %w", err)
		}
		reconcileOK = false
		logf("자가 점검 5/5: ⚠️ 대조 불가 — 무장하지 않고 DRY-RUN 으로만 계속한다: %v", err)
	} else {
		logf("자가 점검 5/5: 크래시 복구 대조 ✅ (열린 주문 없음, 포지션이 원장을 넘지 않음)")
	}

	// --- 자가 점검 4: equity -----------------------------------------------
	equitySrc := &live.EquitySource{
		Rest:    rc,
		RPC:     cfg.RPC,
		Account: secrets.Account,
		// **기본 false 가 보수적인 쪽이다.** PositionCost 는 cap 을 키우는
		// 항이므로(cap = (가용+취득원가)×4.55%), 더하면 더 걸고 빼면 덜 건다.
		// 포지션 응답이 실주문으로 확정되기 전까지는 덜 거는 쪽에 둔다.
		IncludePositions: cfg.IncludePositions,
	}
	eq, eqErr := readEquity(ctx, equitySrc, cfg)
	canArm := risk.CanArm(eq)
	switch {
	case eqErr != nil:
		logf("자가 점검 4/5: ⚠️ equity 를 읽지 못했다 — 무장하지 않는다: %v", eqErr)
	case !canArm:
		// **종료가 아니다.** 자본이 부족한 것은 설정 오류가 아니라 사실이고,
		// DRY-RUN 은 그 상태에서도 계속 도는 것이 맞다.
		logf("자가 점검 4/5: ⚠️ equity %.4f USDT 로는 cap 이 %.4f USDT 라 최소 주문 %.2f 를 넘지 못한다 — 무장하지 않는다 (필요 equity > %.2f)",
			eq.Total(), risk.Cap(eq), risk.MinOrderUSD, risk.MinOrderUSD/risk.CapFraction)
	default:
		logf("자가 점검 4/5: equity ✅ %.4f USDT (cap %.4f USDT)", eq.Total(), risk.Cap(eq))
	}

	// --- 무장 판정 ----------------------------------------------------------
	blockers := armingBlockers()
	armed := wantArm && canArm && reconcileOK && eqErr == nil && len(blockers) == 0
	if wantArm && !armed {
		// **조용히 DRY-RUN 으로 내려가지 않는다.** 무장을 요청한 운영자는
		// 실거래 중이라고 믿을 것이고, 그 믿음이 가장 위험하다.
		for i, b := range blockers {
			logf("무장 차단 %d: %s", i+1, b)
		}
		return fmt.Errorf("LIVE_ARM 이 설정됐지만 무장할 수 없다 (차단 사유 %d건, equity 가능=%v, 대조 통과=%v)",
			len(blockers), canArm, reconcileOK)
	}
	if !armed {
		logf("모드: DRY-RUN — 서명은 하되 **전송하지 않는다** (무장 차단 사유 %d건)", len(blockers))
		for i, b := range blockers {
			logf("  차단 %d: %s", i+1, b)
		}
	} else {
		logf("모드: 실거래 — 주문이 전송된다")
	}

	// 체결 조회. **무장 경로는 armedFills 만 받는다** — DRY-RUN 전용 noFills 를
	// 여기 넘기려는 코드는 컴파일되지 않는다(fills.go).
	fills, err := chooseFills(armed, fillsDeps{
		Rest:     rc,
		Account:  secrets.Account,
		Interval: cfg.FillsPoll,
		Log:      logf,
	})
	if err != nil {
		return err
	}
	if armed {
		logf("체결 조회: GET /v1/orders/matches, 최소 간격 %s (= 노출 갱신 지연)", cfg.FillsPoll)
	}

	l, err := ledger.Open(cfg.LedgerPath)
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()

	if cfg.Exchange != "" {
		// 수동 지정은 회차의 변종 판정을 통째로 무시한다. 조용히 두면
		// systemd 유닛에 남은 한 줄이 몇 달 뒤 틀린 계약에 서명하게 만든다.
		logf("⚠️ -exchange 가 수동 지정됐다 — 회차의 isNegRisk/isYieldBearing 판정을 무시하고 지정된 계약에 서명한다")
	}
	sender := &orderSender{
		Rest:      rc,
		Signer:    signer,
		Account:   common.HexToAddress(secrets.Account),
		Validator: common.HexToAddress(cfg.Validator),
		ChainID:   cfg.ChainID,
		// **비어 있는 것이 정상이다.** 비면 회차마다 마켓 응답의 두 불린으로
		// Exchange 를 고른다(orderSender.domainFor).
		ExchangeOverride: cfg.Exchange,
		Armed:            armed,
		Log:              logf,
	}

	runner := &exec.Runner{
		Orders:     sender,
		Fills:      fills,
		Ledger:     l,
		Cooldown:   cfg.Cooldown,
		StaleAfter: cfg.StaleAfter,
		Poll:       cfg.Poll,
		Log:        logf,
	}

	predictor := &live.Predictor{Model: m}

	// 감시 배선. BEAT_ENDPOINT 가 없으면 nil 이고 모든 호출이 아무것도
	// 하지 않는다 — 감시가 없다고 거래를 막으면 감시 장치의 부재가 거래
	// 장애가 된다.
	wire := newBeatWire(os.Getenv, buildVersion(), armed, newBootID())
	if wire == nil {
		logf("감시 꺼짐 — %s 가 없다", EnvBeatEndpoint)
	} else {
		logf("감시 켜짐 — beat 를 %s 로 보낸다", os.Getenv(EnvBeatEndpoint))
		runner.Observe = wire.Observe
		// 루프 상태 탐침 중 REST 쪽 둘. WS 수신 시각은 소켓이 만들어지는
		// loop() 에서 꽂는다.
		//
		// **여기서 fills 를 쓰는 것이 요점이다.** runner.Fills 는 exec.Fills 라
		// LastPollAt 이 없고, 거기서 타입 단언으로 꺼내면 구현이 메서드를
		// 잃는 날 스냅샷 필드만 조용히 비어 버린다(fills.go, pollingFills).
		wire.SetProbes(loopProbes{
			FillsPollAt:   fills.LastPollAt,
			RateRemaining: rc.RateLimitRemaining,
		})
		wire.Run(ctx)
	}

	return loop(ctx, cfg, rc, runner, predictor, equitySrc, wire)
}

// armLabel 은 LIVE_ARM 을 로그에 남기되 **값을 그대로 찍지 않는다.**
// 오타를 눈으로 확인하려다 로그에 붙여넣기 좋은 문자열을 남기지 않는다.
func armLabel(v string) string {
	switch {
	case v == "":
		return "미설정 → DRY-RUN"
	case Armed(v):
		return "정확히 일치 → 무장 요청"
	default:
		return fmt.Sprintf("설정됐지만 값이 다르다(%d자) → DRY-RUN", len(v))
	}
}

// readEquity 는 equity 를 읽는다. DRY-RUN 가짜 자본이 켜져 있으면 그것으로
// **덮어쓴다** — 실제 값도 함께 로그에 남긴다.
func readEquity(ctx context.Context, src *live.EquitySource, cfg *Config) (risk.Equity, error) {
	eq, err := src.Read(ctx)
	if cfg.DryRunEquityUSDT > 0 {
		// checkConfig 가 무장 상태에서 이 값을 이미 막았다. 여기서는 실제
		// 조회가 실패했더라도 DRY-RUN 을 계속 돌릴 수 있게 한다.
		logf("⚠️ DRY-RUN 가짜 자본 %.2f USDT 를 쓴다 (실제 조회 결과: %+v, err=%v)", cfg.DryRunEquityUSDT, eq, err)
		return risk.Equity{AvailableUSDT: cfg.DryRunEquityUSDT}, nil
	}
	return eq, err
}

// ---------------------------------------------------------------------------
// 회차 루프
// ---------------------------------------------------------------------------

// tracked 는 구독 중인 회차 하나다.
type tracked struct {
	round live.Round
	book  *ws.Book
}

// router 는 marketID 로 프레임을 호가창에 꽂는다. WS 읽기 고루틴과 폴링
// 고루틴, 그리고 회차 루프가 함께 본다.
type router struct {
	mu    sync.Mutex
	items map[int64]*tracked
	reqID uint64 // atomic

	// assumptionErrs 는 "가정이 틀렸다" 쪽 실패의 누적이다. 네트워크 실패와
	// 섞으면 24시간 DRY-RUN 에서 둘을 구분할 수 없다.
	assumptionErrs int64 // atomic
	transportErrs  int64 // atomic
}

func newRouter() *router { return &router{items: map[int64]*tracked{}} }

func (rt *router) onFrame(f ws.Frame) {
	if f.Msg.Type == ws.TypeResponse {
		if f.Msg.Success != nil && !*f.Msg.Success {
			code := ""
			if f.Msg.Error != nil {
				code = f.Msg.Error.Code + ": " + f.Msg.Error.Message
			}
			logf("구독 요청 거부됨 (requestId=%d) %s", f.Msg.RequestID, code)
		}
		return
	}
	kind, marketID, ok := ws.ParseTopic(f.Msg.Topic)
	if !ok || kind != "predictOrderbook" {
		return
	}
	rt.mu.Lock()
	t := rt.items[marketID]
	rt.mu.Unlock()
	if t == nil {
		return // 방금 구독해제한 회차의 지연 프레임. 무해하다.
	}
	if _, err := t.book.Apply(f); err != nil {
		// 관대한 파싱 — 프레임 하나로 회차를 죽이지 않는다.
		logf("오더북 프레임 파싱 실패 (marketId=%d): %v", marketID, err)
	}
}

// subscribeAll 은 재접속마다 전체를 다시 구독한다 — 서버는 구독 상태를
// 기억하지 않는다.
func (rt *router) subscribeAll(ctx context.Context, s ws.Sender) error {
	rt.mu.Lock()
	ids := make([]int64, 0, len(rt.items))
	for id := range rt.items {
		ids = append(ids, id)
	}
	rt.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		if err := s.Send(ctx, ws.SubscribeRequest(atomic.AddUint64(&rt.reqID, 1), ws.TopicOrderbook(id))); err != nil {
			return fmt.Errorf("marketId=%d 구독 실패: %w", id, err)
		}
	}
	return nil
}

// sync 는 회차 목록을 받아 구독을 맞춘다. 새 회차는 구독하고, 끝난 회차는
// 구독해제한다.
func (rt *router) sync(ctx context.Context, s ws.Sender, rounds []live.Round, now time.Time) {
	alive := map[int64]bool{}
	var added []newSub
	rt.mu.Lock()
	for _, r := range rounds {
		alive[r.MarketID] = true
		if _, ok := rt.items[r.MarketID]; ok {
			continue
		}
		// precision 은 live.FetchLive 가 이미 1..18 로 검사했다. ws.NewBook 은
		// 범위 밖에서 패닉하는데, 여기서 패닉하면 살아 있는 주문을 든 채 죽는다.
		rt.items[r.MarketID] = &tracked{round: r, book: ws.NewBook(r.Precision)}
		added = append(added, newSub{id: r.MarketID, slug: r.Slug})
	}
	var removed []int64
	for id, t := range rt.items {
		if !alive[id] && !t.round.EndsAt.After(now) {
			removed = append(removed, id)
			delete(rt.items, id)
		}
	}
	rt.mu.Unlock()

	for _, a := range added {
		if err := s.Send(ctx, ws.SubscribeRequest(atomic.AddUint64(&rt.reqID, 1), ws.TopicOrderbook(a.id))); err != nil {
			logf("신규 회차 구독 실패 (%s): %v", a.slug, err)
			continue
		}
		logf("회차 구독: %s", a.slug)
	}
	for _, id := range removed {
		// 베스트에포트다. 회차가 끝나면 서버가 자연히 갱신을 멈춘다.
		_ = s.Send(ctx, ws.UnsubscribeRequest(atomic.AddUint64(&rt.reqID, 1), ws.TopicOrderbook(id)))
	}
}

// newSub 는 sync 안에서만 쓰는 짧은 쌍이다. 구독 전송은 락을 놓은 뒤에 해야
// 하므로(전송이 막히면 폴링·회차 루프가 함께 멈춘다) 목록을 들고 나온다.
type newSub struct {
	id   int64
	slug string
}

// pick 은 지금 운용할 회차를 고른다.
//
// 조건: 시작했고, 아직 안 끝났고, 이미 돌린 적 없고, **너무 늦게 합류하지
// 않는다.** 마지막 조건이 중요하다 — 회차 중반에 봇을 띄우면 p_up 은 회차
// 시작 시각의 것인데 시장은 이미 그 방향으로 움직였을 수 있고, G2 가 잰 엣지는
// 회차 시작 근처 진입을 가정한 값이다.
func (rt *router) pick(now time.Time, maxLate time.Duration, done map[string]bool) (*tracked, bool) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	var best *tracked
	for _, t := range rt.items {
		r := t.round
		if done[r.Slug] || r.StartsAt.After(now) || !r.EndsAt.After(now) {
			continue
		}
		if now.Sub(r.StartsAt) > maxLate {
			continue
		}
		if best == nil || r.StartsAt.Before(best.round.StartsAt) {
			best = t
		}
	}
	return best, best != nil
}

// pollRounds 는 회차 목록을 주기적으로 받아 구독을 맞춘다.
//
// **FetchLive 에러는 그 폴링 주기를 건너뛸 뿐이다.** 프로세스를 죽이지 않는다 —
// 카테고리 조회가 한 번 실패했다고 봇이 내려가면 24시간 운전이 성립하지 않는다.
// 대신 실패를 두 종류로 나눠 센다: 가정이 틀린 것(회차 메타데이터가 우리
// 이해와 다르다)과 전송 실패다. 앞은 반복되면 코드를 고쳐야 하고 뒤는 아니다.
func (rt *router) pollRounds(ctx context.Context, rc *rest.Client, wsc *ws.Client, cfg *Config) {
	tick := time.NewTicker(cfg.RoundPoll)
	defer tick.Stop()
	for {
		now := time.Now()
		rounds, err := live.FetchLive(ctx, rc, cfg.Symbol, now, live.DefaultLookahead)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// live.FetchLive 는 회차 메타데이터가 우리 이해와 다르면 폴링
			// 전체를 에러로 만든다(15분 상품 오염 사례). 그 실패는 전송
			// 실패와 성격이 다르다.
			var he *rest.HTTPError
			if errors.As(err, &he) {
				atomic.AddInt64(&rt.transportErrs, 1)
				logf("[전송실패] 회차 조회 — 이 주기를 건너뛴다: %v", err)
			} else {
				atomic.AddInt64(&rt.assumptionErrs, 1)
				logf("[가정오류] 회차 조회 — 이 주기를 건너뛴다 (반복되면 회차 메타데이터 해석이 틀린 것이다): %v", err)
			}
		} else {
			rt.sync(ctx, wsc, rounds, now)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// loop 는 회차를 하나씩 돈다.
func loop(ctx context.Context, cfg *Config, rc *rest.Client, runner *exec.Runner,
	predictor *live.Predictor, equitySrc *live.EquitySource, wire *beatWire) error {

	if cfg.Minutes > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(cfg.Minutes*float64(time.Minute)))
		defer cancel()
		logf("실행 시간 상한 %.1f분", cfg.Minutes)
	}

	rt := newRouter()
	var wsc *ws.Client
	wsc = ws.New(ws.Options{
		URL:    cfg.WSURL,
		APIKey: cfg.APIKey,
		OnConnect: func(ctx context.Context, s ws.Sender) error {
			if n := wsc.Reconnects(); n > 0 {
				logf("재접속 %d회 — 전체 재구독한다", n)
			}
			return rt.subscribeAll(ctx, s)
		},
		OnFrame: rt.onFrame,
		OnGap: func(start, end time.Time, reason string) {
			logf("데이터 공백 %s (%s)", end.Sub(start).Round(time.Millisecond), reason)
		},
	})
	// 마켓데이터 수신 시각 탐침. 이것이 비면 감시의 `ws_data` 규칙이
	// **조용히 꺼진다** — 그 규칙은 제로값을 "모른다" 로 읽고 넘어가므로,
	// 호가가 끊긴 채 도는 봇을 아무도 잡지 못한다.
	wire.SetProbes(loopProbes{WSLastDataAt: wsc.LastDataAt})

	// 배경 고루틴 둘은 ctx 가 끝나야 멈춘다. **정리 순서가 중요하다** —
	// defer 는 LIFO 라 아래 두 줄은 "cancelAll 먼저, wg.Wait 나중" 으로 돈다.
	// 순서가 뒤집히면 ctx 가 살아 있는 채로 wg.Wait 에 들어가 영원히 멈춘다
	// (에러로 빠져나가는 경로가 나중에 하나라도 생기면 그 즉시 교착이다).
	runCtx, cancelAll := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = wsc.Run(runCtx) }()
	go func() { defer wg.Done(); rt.pollRounds(runCtx, rc, wsc, cfg) }()
	defer wg.Wait()
	defer cancelAll()

	done := map[string]bool{}
	rounds := 0
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			logf("종료 — 회차 %d건 운용, 회차조회 실패 가정오류 %d건 / 전송실패 %d건",
				rounds, atomic.LoadInt64(&rt.assumptionErrs), atomic.LoadInt64(&rt.transportErrs))
			return nil
		case <-tick.C:
		}

		// 모니터가 종료를 지시했으면 새 회차를 잡지 않고 끝낸다. 이 봇은
		// 매도를 내지 않으므로 청산 단계가 없다 — 진행 중이던 회차는 exec 가
		// 미체결을 거두고 끝내고, 미정산 포지션은 정산에 맡긴다.
		if wire.ShouldExit() {
			logf("모니터 지시로 종료 — 회차 %d건 운용", rounds)
			return nil
		}
		t, ok := rt.pick(time.Now(), cfg.MaxJoinLate, done)
		if !ok {
			continue
		}
		// **먼저 표시한다.** 아래에서 무엇이 실패하든 같은 회차를 다시 잡으면
		// 안 된다 — 같은 실패를 200ms 마다 반복하며 로그를 채운다.
		done[t.round.Slug] = true
		pruneDone(done, rt, time.Now())
		rounds++
		if err := runRound(ctx, cfg, runner, predictor, equitySrc, t, wire); err != nil {
			if ctx.Err() != nil {
				logf("종료 — 회차 %d건 운용", rounds)
				return nil
			}
			// 회차 하나가 실패해도 봇은 계속 돈다. exec 가 종료 시 미체결
			// 전량 취소를 이미 보장했다(RunRound 는 그것을 못 하면 에러다).
			logf("회차 %s 실패: %v", t.round.Slug, err)
		}
	}
}

// runRound 는 회차 하나를 준비하고 운용한다.
func runRound(ctx context.Context, cfg *Config, runner *exec.Runner, predictor *live.Predictor,
	equitySrc *live.EquitySource, t *tracked, wire *beatWire) error {

	r := t.round
	// Exchange 변종을 여기 찍는다 — **회차마다 다시 정해지는 값이라 회차 줄에
	// 있어야 한다.** 이 줄이 negRisk 로 바뀌는 날이 곧 서명 도메인이 바뀌는
	// 날이고, 그 사실이 로그에 남지 않으면 아무도 모른다.
	logf("회차 %s 진입 — 시작 %s, 종료 %s, precision %d, feeRateBps %d, %s",
		r.Slug, r.StartsAt.UTC().Format(time.RFC3339), r.EndsAt.UTC().Format(time.RFC3339),
		r.Precision, r.FeeRateBps, order.ExchangeName(r.IsNegRisk, r.IsYieldBearing))

	// p_up 은 회차 시작에 **한 번** 계산해 동결한다. exec 는 이 값을 들고
	// 다니고 Predictor 를 참조하지 않는다 — 회차 중 다시 계산할 경로가 없다.
	frozen, err := predictor.Freeze(ctx, r.StartMS())
	if err != nil {
		// 감시에 사유를 남긴다. 이 경로로 조용해지는 것이 이 봇의 실패
		// 모드이므로, 사유 없이 멈추면 모니터가 정상 스킵과 구분하지 못한다.
		wire.Report(snapshotInput{Round: r, PredictErr: err})
		return fmt.Errorf("p_up 동결: %w", err)
	}
	logf("회차 %s: p_up %.6f, confidence %.6f (문턱 %.4f), 방향 %s, 자격 %v",
		r.Slug, frozen.PUp, frozen.Confidence, live.ConfidenceThreshold, frozen.Direction, frozen.Eligible)

	eq, err := readEquity(ctx, equitySrc, cfg)
	if err != nil {
		// **equity 를 모르면 걸지 않는다.** 0 으로 두면 risk 가 전부 0 을
		// 돌려주므로 결과는 같지만, 그 사실이 로그에 남아야 한다.
		if errors.Is(err, live.ErrReservedExceedsBalance) {
			logf("[가정오류] 회차 %s: %v — 예약잔고 해석이 틀렸을 수 있다(반복되면 equity.go 를 고쳐야 한다)", r.Slug, err)
		} else {
			logf("[전송실패] 회차 %s: equity 조회 실패 — 이 회차는 걸지 않는다: %v", r.Slug, err)
		}
		// equity 를 모르면 cap 이 0 이라 CanArm 이 false 다 → SkipEquity.
		wire.Report(snapshotInput{Round: r, Frozen: frozen})
		return nil
	}
	logf("회차 %s: equity %.4f USDT, cap %.4f USDT", r.Slug, eq.Total(), risk.Cap(eq))

	runner.Book = t.book
	// 회차 맥락을 감시에 올린다. 이후 exec 의 관측이 이 맥락 위에 얹힌다 —
	// exec 는 회차·자본·자격을 모른다(계약 타입을 임포트하지 않는다).
	wire.Report(snapshotInput{Round: r, Frozen: frozen, Equity: eq, Active: frozen.Eligible})

	err = runner.RunRound(ctx, r, frozen, eq)

	// 회차가 끝났다. IDLE 로 되돌려 두지 않으면 다음 회차를 잡기 전까지
	// 모니터가 끝난 회차를 운용 중으로 읽는다.
	wire.Report(snapshotInput{Round: live.Round{}, Equity: eq})
	return err
}

// pruneDone 은 이미 끝난 회차의 표시를 지운다. 24시간이면 288회차라 그대로
// 둬도 크지 않지만, 무한히 자라는 맵을 남기지 않는다.
func pruneDone(done map[string]bool, rt *router, now time.Time) {
	if len(done) < 64 {
		return
	}
	keep := map[string]bool{}
	rt.mu.Lock()
	for _, t := range rt.items {
		if t.round.EndsAt.After(now.Add(-time.Hour)) {
			keep[t.round.Slug] = true
		}
	}
	rt.mu.Unlock()
	for slug := range done {
		if !keep[slug] {
			delete(done, slug)
		}
	}
}
