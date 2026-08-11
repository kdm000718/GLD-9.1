package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
	"github.com/kdm000718/GLD-9.1/internal/timing"
)

// 회차 선택(카테고리 조회·시각 창)은 internal/live 에 있다.
//
// 여기 있던 fetchLiveRounds/roundIsLive 를 그리로 옮겼다. 진단(probe)과
// 실거래(gld91)가 서로 다른 회차 선택 규칙을 쓰면, 소크로 확인한 것이
// 실거래가 하는 일과 다른 것이 된다 — PUBLISHED_AT_ASC + 시각 창이라는
// 이 규칙 자체가 60분 소크 두 번을 헛돌게 한 실패에서 나온 것이라 더욱
// 그렇다. 두 곳에 두면 갈린다.

// --- 오더북 60분 소크 테스트 ---

type trackedMarket struct {
	ID               int64
	Slug             string
	DecimalPrecision int
	Book             *ws.Book
	// RoundStart는 이 회차의 시작 시각이다(카테고리 응답의 startsAt). 프레임이
	// 회차 안 몇 초 지점에서 왔는지를 재는 데 쓴다 — Stale 문턱을 정하려면
	// "언제 갱신이 몰리는가"가 필요하다(팀리드 지시, 2026-08-09).
	RoundStart time.Time
}

// frameRecord는 오더북 프레임 하나의 수신 시각과, 그 회차 시작으로부터 몇 초
// 지났는지를 남긴다. 이 두 값의 분포가 P5의 Stale 문턱을 정하는 실측
// 근거다 — 총 개수만으로는 문턱을 못 정한다(팀리드 지시).
type frameRecord struct {
	RecvUnixNs        int64
	MarketID          int64
	ElapsedInRoundSec float64
	// RoundStartOK는 RoundStart를 실제로 파싱했는지다. false와
	// ElapsedInRoundSec==0(회차 시작 정각)을 구분하기 위한 별도 플래그다 —
	// 그 둘을 같은 값(0)으로 겹치면 "파싱 실패"와 "막 시작한 회차"가 섞인다.
	RoundStartOK bool
}

type gapEvent struct {
	Start  time.Time
	End    time.Time
	Reason string
}

// readState는 mode=read 실행 전체의 공유 상태다. 동시 접근자: pollOnce(폴링
// 고루틴), onFrame/onGap(ws 읽기 고루틴), 상태 출력(메인 고루틴).
type readState struct {
	mu      sync.Mutex
	tracked map[int64]*trackedMarket

	reqID uint64 // atomic

	orderbookFrames int64 // atomic — Book.Apply 가 **실제로 적용한** 프레임 수
	// droppedFrames는 Apply 가 updateTimestampMs 역전/중복으로 버린 프레임
	// 수다(에러가 아니다). 이것을 orderbookFrames 와 섞으면 안 된다 —
	// onFrame 의 주석 참고. 0 이 아니면 간격 분포에서 8.6%를 차지하던
	// "간격 0.0s" 표본의 정체(진짜 빠른 갱신 vs 버려진 중복)가 드러난다.
	droppedFrames     int64 // atomic
	otherFrames       int64 // atomic
	subscribeFailures int64 // atomic — 구독/구독해제 요청이 success:false로 거부된 횟수
	// lastOrderbookMonoNs는 마지막으로 오더북 프레임을 적용한 단조시계
	// 시각이다. 어느 마켓이든 상관없이 "데이터가 흐르고 있는가"만 본다.
	lastOrderbookMonoNs int64 // atomic

	gaps            []gapEvent
	decimalPrecs    map[int]int // 관측된 decimalPrecision → 횟수
	subscribedTotal int
	// skippedPrecision은 decimalPrecision 이 1..ws.MaxPrecision 밖이라 구독을
	// 건너뛴 **마켓** 을 marketID → 관측된 precision 으로 모은 것이다. 0 이
	// 아니면 필드 이름이 바뀌었을 가능성이 높다 — 최종 보고에서 눈에 띄어야
	// 한다(pollOnce 의 주석 참고). s.mu 로 보호한다.
	//
	// **정수 카운터가 아니라 맵인 이유**: 걸러낸 마켓은 s.tracked 에 안 들어가므로
	// 폴링마다 다시 걸린다. 정수로 세면 30분·20초 주기에서 마켓 2개가 "180개"로
	// 보고된다 — 사건 수를 마켓 수라고 말하는 것이고, 바로 옆 subscribedTotal
	// 은 마켓당 한 번만 센다. 같은 보고 안에서 두 카운터의 규약이 갈리면 안 된다.
	// precision 값 자체를 담는 것은 최종 보고만 보고도 0(필드명 변경)인지
	// 19(범위 초과)인지 구분하기 위해서다 — decimalPrecs 에는 안 들어간다.
	skippedPrecision map[int64]int

	// frames는 오더북 프레임 전체의 개별 기록이다(집계 카운터와 별개) —
	// 무갱신 간격의 분포(중앙값/p90/p99)와 회차 안 시각 분포를 계산하려면
	// 1분 단위 상태줄로는 부족하고 개별 시각이 있어야 한다.
	frames []frameRecord

	// runStartMonoNs/maxOrderbookGapNs는 "오더북 연속성"(배관과는 별개 판정
	// 축, printReadFinal 참고)을 재는 데 쓴다. 최대 무갱신 간격은 프레임
	// 사이만이 아니라 실행 시작→첫 프레임, 마지막 프레임→실행 종료까지도
	// 포함해야 실제 "끊긴 시간"을 놓치지 않는다.
	runStartMonoNs    int64
	maxOrderbookGapNs int64 // atomic
}

func newReadState(runStartMonoNs int64) *readState {
	return &readState{
		tracked:          map[int64]*trackedMarket{},
		decimalPrecs:     map[int]int{},
		skippedPrecision: map[int64]int{},
		runStartMonoNs:   runStartMonoNs,
	}
}

// subscribeAll은 현재 tracked 전체를 구독한다. OnConnect(최초 연결 및 매
// 재접속)에서 호출된다 — 서버는 구독 상태를 기억하지 않으므로 재접속마다
// 전체 재구독이 필요하다(ws 패키지 문서 그대로).
func (s *readState) subscribeAll(ctx context.Context, sender ws.Sender) error {
	s.mu.Lock()
	ids := make([]int64, 0, len(s.tracked))
	for id := range s.tracked {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		req := ws.SubscribeRequest(atomic.AddUint64(&s.reqID, 1), ws.TopicOrderbook(id))
		if err := sender.Send(ctx, req); err != nil {
			return fmt.Errorf("marketId=%d 구독 실패: %w", id, err)
		}
	}
	return nil
}

// pollOnce은 카테고리를 한 번 조회해 tracked 집합을 최신 OPEN 회차로 맞춘다.
// 신규 회차는 구독하고, 더 이상 OPEN이 아닌 회차는 구독해제 후 제거한다.
func (s *readState) pollOnce(ctx context.Context, rc *rest.Client, sender ws.Sender, symbolPrefix string, log *slog.Logger) {
	cats, err := live.FetchCategories(ctx, rc, symbolPrefix, time.Now(), live.DefaultLookahead)
	if err != nil {
		log.Warn("카테고리 조회 실패", "err", err)
		return
	}

	seen := map[int64]bool{}
	var newMarkets []*trackedMarket

	s.mu.Lock()
	for _, cat := range cats {
		for _, m := range cat.Markets {
			if m.TradingStatus != "OPEN" {
				continue
			}
			seen[m.ID] = true
			if _, ok := s.tracked[m.ID]; ok {
				continue
			}
			// decimalPrecision 이 1..ws.MaxPrecision 밖이면 이 마켓을 건너뛴다.
			//
			// ws.NewBook 은 범위 밖에서 **패닉한다**(order 패키지와 같은 규약).
			// 그 패닉은 폴링 고루틴에서 터져 프로세스를 죽이고, 소크 실행이
			// 통째로 날아간다 — 30분 소크의 관측이 마켓 하나의 이상한 메타데이터
			// 때문에 사라지는 것은 진단 도구로서 최악의 거래다. 여기서 미리
			// 걸러 나머지 마켓의 소크는 계속되게 한다.
			//
			// 조용히 넘기지는 않는다. 값 0 은 십중팔구 필드 이름이 바뀌어
			// encoding/json 이 제로값을 넣은 것이므로(이 저장소가 두 번 겪은
			// 실패 모드), 경고와 최종 보고의 집계 둘 다 남긴다.
			if m.DecimalPrecision < 1 || m.DecimalPrecision > ws.MaxPrecision {
				// 경고는 마켓당 최초 1회만. 걸러낸 마켓은 tracked 에 안 들어가
				// 폴링마다 다시 오므로, 매번 찍으면 30분 소크에서 같은 줄이
				// 90번 쌓여 정작 읽어야 할 [프레임] 줄을 덮는다.
				if _, seen := s.skippedPrecision[m.ID]; !seen {
					log.Warn("decimalPrecision 이 범위 밖이라 이 마켓을 건너뛴다 — 필드 이름이 바뀌었을 수 있다(값 0 은 JSON 제로값이다)",
						"marketId", m.ID, "slug", cat.Slug,
						"decimalPrecision", m.DecimalPrecision, "허용범위", fmt.Sprintf("1..%d", ws.MaxPrecision))
				}
				s.skippedPrecision[m.ID] = m.DecimalPrecision
				continue
			}
			// startsAt 파싱 실패는 조용히 넘어간다(영시각) — 회차 시각 분포
			// 계산에서만 쓰이는 값이라 실패해도 소크 자체를 막을 이유가 없다.
			roundStart, _ := time.Parse(time.RFC3339, cat.StartsAt)
			tm := &trackedMarket{
				ID:               m.ID,
				Slug:             cat.Slug,
				DecimalPrecision: m.DecimalPrecision,
				Book:             ws.NewBook(m.DecimalPrecision),
				RoundStart:       roundStart,
			}
			s.tracked[m.ID] = tm
			newMarkets = append(newMarkets, tm)
			s.decimalPrecs[m.DecimalPrecision]++
		}
	}
	var removed []int64
	for id := range s.tracked {
		if !seen[id] {
			removed = append(removed, id)
			delete(s.tracked, id)
		}
	}
	s.subscribedTotal += len(newMarkets)
	s.mu.Unlock()

	for _, tm := range newMarkets {
		req := ws.SubscribeRequest(atomic.AddUint64(&s.reqID, 1), ws.TopicOrderbook(tm.ID))
		if err := sender.Send(ctx, req); err != nil {
			log.Warn("신규 회차 구독 실패", "marketId", tm.ID, "slug", tm.Slug, "err", err)
			continue
		}
		log.Info("신규 회차 구독", "marketId", tm.ID, "slug", tm.Slug, "decimalPrecision", tm.DecimalPrecision)
	}
	for _, id := range removed {
		req := ws.UnsubscribeRequest(atomic.AddUint64(&s.reqID, 1), ws.TopicOrderbook(id))
		// 베스트에포트: 실패해도 조용히 넘어간다. 회차가 이미 끝나 서버가
		// 자연히 갱신을 멈추므로, 구독해제가 안 돼도 데이터 정확성에 영향 없다.
		_ = sender.Send(ctx, req)
		log.Info("만료 회차 구독해제", "marketId", id)
	}
}

func (s *readState) pollLoop(ctx context.Context, rc *rest.Client, sender ws.Sender, symbolPrefix string, interval time.Duration, log *slog.Logger) {
	s.pollOnce(ctx, rc, sender, symbolPrefix, log) // 즉시 1회
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollOnce(ctx, rc, sender, symbolPrefix, log)
		}
	}
}

func (s *readState) onFrame(f ws.Frame, log *slog.Logger) {
	if f.Msg.Type == ws.TypeResponse {
		// 구독/구독해제 요청에 대한 응답이다. 실패(예: invalid_topic)를
		// 조용히 넘기지 않는다 — 구독이 거부됐는데도 "정상 구독함"으로
		// 잘못 보고하면 안 된다.
		ok := f.Msg.Success != nil && *f.Msg.Success
		if !ok {
			atomic.AddInt64(&s.subscribeFailures, 1)
			errMsg := ""
			if f.Msg.Error != nil {
				errMsg = f.Msg.Error.Code + ": " + f.Msg.Error.Message
			}
			log.Warn("구독 요청 거부됨", "requestId", f.Msg.RequestID, "err", errMsg)
		}
		atomic.AddInt64(&s.otherFrames, 1)
		return
	}

	kind, marketID, ok := ws.ParseTopic(f.Msg.Topic)
	if !ok || kind != "predictOrderbook" {
		atomic.AddInt64(&s.otherFrames, 1)
		return
	}

	s.mu.Lock()
	tm := s.tracked[marketID]
	s.mu.Unlock()
	if tm == nil {
		// 방금 구독해제한 회차의 지연 프레임일 수 있다. 무해하다.
		return
	}
	applied, err := tm.Book.Apply(f)
	if err != nil {
		return // 관대한 파싱 — 프레임 하나로 소크 테스트를 죽이지 않는다.
	}
	// **버려진 프레임은 아무것도 건드리지 않는다.** Apply 가 updateTimestampMs
	// 역전/중복으로 버린 프레임을 "갱신됨" 으로 세면 세 가지가 동시에 틀린다:
	// 수신 개수가 실제 갱신보다 많아지고, lastOrderbookMonoNs 가 앞당겨져
	// **멈춘 호가창이 신선하다고 보고되며**, 간격 표본에 없던 짧은 간격이
	// 섞여 P5 Stale 문턱의 실측 근거가 오염된다(Book.Apply 주석 참고).
	// 대신 따로 세어 최종 보고에 찍는다 — 이 수가 크면 위 셋이 얼마나
	// 오염됐을지가 바로 보인다.
	if !applied {
		atomic.AddInt64(&s.droppedFrames, 1)
		return
	}
	atomic.AddInt64(&s.orderbookFrames, 1)

	// prev==0(첫 프레임)이면 실행 시작부터 지금까지를 간격으로 잰다 — 그래야
	// "60분 내내 한 건도 안 왔다"가 간격 0으로 숨지 않는다. onFrame은 ws
	// readLoop 하나에서만 불리므로(패키지 문서) 이 스왑 앞뒤로 경쟁이 없다.
	prev := atomic.SwapInt64(&s.lastOrderbookMonoNs, f.RecvMonoNs)
	gap := f.RecvMonoNs - s.runStartMonoNs
	if prev != 0 {
		gap = f.RecvMonoNs - prev
	}
	if gap > atomic.LoadInt64(&s.maxOrderbookGapNs) {
		atomic.StoreInt64(&s.maxOrderbookGapNs, gap)
	}

	var elapsedInRound float64
	roundStartOK := !tm.RoundStart.IsZero()
	if roundStartOK {
		elapsedInRound = time.Unix(0, f.RecvUnixNs).Sub(tm.RoundStart).Seconds()
	}
	s.mu.Lock()
	s.frames = append(s.frames, frameRecord{
		RecvUnixNs: f.RecvUnixNs, MarketID: marketID,
		ElapsedInRoundSec: elapsedInRound, RoundStartOK: roundStartOK,
	})
	s.mu.Unlock()

	// 개별 프레임을 즉시 로그에도 남긴다 — printFrameDistribution은 실행이
	// 끝까지 살아남아야만(printReadFinal이 불려야만) 값을 낸다. 실측(2026-08-09):
	// testnet·mainnet 소크 둘 다 하니스가 59~60분 근처에서 프로세스를 죽여
	// printReadFinal이 한 번도 안 불렸다 — s.frames에 쌓인 데이터가 통째로
	// 사라졌다. 이 로그 한 줄이 있으면 프로세스가 언제 죽든 그때까지의 간격
	// 분포를 로그만으로 사후 재구성할 수 있다.
	fmt.Printf("[프레임] marketId=%d elapsedInRound=%.1fs\n", marketID, elapsedInRound)
}

func (s *readState) onGap(start, end time.Time, reason string) {
	s.mu.Lock()
	s.gaps = append(s.gaps, gapEvent{Start: start, End: end, Reason: reason})
	s.mu.Unlock()
}

// runRead는 Step 1(설정 확인)과 Step 2(오더북 소크 테스트)를 함께 수행한다.
func runRead(parent context.Context, rc *rest.Client, symbolPrefix string, minutes float64, pollInterval time.Duration, log *slog.Logger) error {
	fmt.Printf("[Step 1] REST 베이스 URL = %s\n", rc.BaseURL)

	// 무키 확인: testnet은 x-api-key 없이 200을 받아야 한다.
	probe, err := live.FetchCategories(parent, rc, symbolPrefix, time.Now(), live.DefaultLookahead)
	if err != nil {
		return fmt.Errorf("초기 카테고리 조회 실패 — 설정을 다시 확인하라: %w", err)
	}
	// "무키"라고 단정하지 않는다 — 이 함수는 testnet·mainnet 둘 다에서 쓰이고
	// mainnet은 실제로 x-api-key를 붙인다(main.go). 키 사용 여부는 위에서
	// 이미 로그로 남겼다(rc.BaseURL로 어느 환경인지는 구분된다).
	fmt.Printf("[Step 1] 카테고리 조회 성공, 진행 중(+%s 내 시작) %s-updown-5m-* 회차 %d개 발견\n",
		live.DefaultLookahead, symbolPrefix, len(probe))
	if len(probe) == 0 {
		fmt.Println("[Step 1] 경고: 진행 중 회차가 0개다 — 시각 창을 통과한 회차가 없다." +
			" 소크는 계속하지만(다음 폴링에서 열릴 수 있다) 0이 계속되면 startsAt/endsAt 필드나 sort 값을 의심하라.")
	}
	if len(probe) == 0 {
		return fmt.Errorf("OPEN 상태인 %s-updown-5m-* 회차가 없다 — 소크 테스트를 시작할 수 없다", symbolPrefix)
	}
	for _, cat := range probe {
		for _, m := range cat.Markets {
			fmt.Printf("    %-28s marketId=%-8d precision=%d feeRateBps=%d shareThreshold=%d spreadThreshold=%v\n",
				cat.Slug, m.ID, m.DecimalPrecision, m.FeeRateBps, m.ShareThreshold, m.SpreadThreshold)
			for _, o := range m.Outcomes {
				fmt.Printf("        outcome=%-4s indexSet=%d onChainId=%s\n", o.Name, o.IndexSet, o.OnChainID)
			}
		}
	}

	fmt.Printf("\n[Step 2] 오더북 소크 테스트 시작 — %.0f분, 폴링 주기 %s\n", minutes, pollInterval)

	ctx, cancel := context.WithTimeout(parent, time.Duration(minutes*float64(time.Minute)))
	defer cancel()

	state := newReadState(monoNow())

	var wsc *ws.Client
	opts := ws.Options{
		URL:    wsURL,
		Logger: log,
		OnConnect: func(ctx context.Context, sender ws.Sender) error {
			if n := wsc.Reconnects(); n > 0 {
				log.Warn("재접속 발생, 전체 재구독한다", "reconnects", n)
			}
			return state.subscribeAll(ctx, sender)
		},
		OnFrame: func(f ws.Frame) { state.onFrame(f, log) },
		OnGap:   state.onGap,
	}
	wsc = ws.New(opts)

	go state.pollLoop(ctx, rc, wsc, symbolPrefix, pollInterval, log)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = wsc.Run(ctx) // ctx가 끝날 때만 반환한다(ws.Client.Run 문서).
	}()

	start := time.Now()
	statusTicker := time.NewTicker(60 * time.Second)
	defer statusTicker.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-statusTicker.C:
			printReadStatus(state, wsc, start)
		}
	}

	// wsc.Run이 소켓을 정리할 시간을 준다.
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		log.Warn("wsc.Run 정리 대기 시간 초과")
	}

	return printReadFinal(state, wsc, minutes, log)
}

func printReadStatus(s *readState, wsc *ws.Client, start time.Time) {
	s.mu.Lock()
	tracked := len(s.tracked)
	s.mu.Unlock()

	lastNs := atomic.LoadInt64(&s.lastOrderbookMonoNs)
	var age string
	if lastNs == 0 {
		age = "아직 없음"
	} else {
		age = time.Duration(monoNow() - lastNs).Round(time.Second).String()
	}
	// 버림 개수를 여기 넣는 이유: droppedFrames 가 최종 보고에만 있으면,
	// 프로세스가 중간에 죽는 경우(이 소크에서 네 번 일어났다) 그 값이 통째로
	// 사라진다. 그런데 이 카운터가 결정적인 상황 — updateTimestampMs 필드
	// 이름이 바뀌어 전량 버려지는 경우 — 에는 [프레임] 줄이 한 줄도 안 찍히므로
	// 상태줄이 유일한 단서다. "프레임 0개 / 버림 0개"(시장 무활동)와
	// "프레임 0개 / 버림 다수"(필드명 변경)를 여기서 구분할 수 있어야 한다.
	fmt.Printf("[상태] 경과 %s | 추적 마켓 %d개 | 재접속 %d회 | 오더북 프레임 %d개 | 버림 %d개 | 마지막 갱신 %s 전\n",
		time.Since(start).Round(time.Second), tracked, wsc.Reconnects(),
		atomic.LoadInt64(&s.orderbookFrames), atomic.LoadInt64(&s.droppedFrames), age)
}

func printReadFinal(s *readState, wsc *ws.Client, minutes float64, log *slog.Logger) error {
	s.mu.Lock()
	gaps := append([]gapEvent(nil), s.gaps...)
	precs := make(map[int]int, len(s.decimalPrecs))
	for k, v := range s.decimalPrecs {
		precs[k] = v
	}
	subscribedTotal := s.subscribedTotal
	skipped := make(map[int64]int, len(s.skippedPrecision))
	for k, v := range s.skippedPrecision {
		skipped[k] = v
	}
	s.mu.Unlock()

	var totalGapDur time.Duration
	fmt.Printf("\n[Step 2] 완료 — %.0f분 경과\n", minutes)
	fmt.Printf("  총 재접속: %d회 (최초 연결은 세지 않는다)\n", wsc.Reconnects())
	fmt.Printf("  추적한 회차 수(누적): %d개\n", subscribedTotal)
	fmt.Printf("  구독/구독해제 거부(success=false): %d건\n", atomic.LoadInt64(&s.subscribeFailures))
	fmt.Printf("  오더북 프레임 적용: %d개\n", atomic.LoadInt64(&s.orderbookFrames))
	dropped := atomic.LoadInt64(&s.droppedFrames)
	fmt.Printf("  오더북 프레임 버림(updateTimestampMs 역전/중복): %d개\n", dropped)
	if dropped > 0 {
		fmt.Println("    → 버린 프레임은 위 적용 개수·마지막 갱신 시각·간격 분포 어디에도 들어가지 않았다." +
			" 이 값이 크면 재구독 직후 중복 재전송이 잦다는 뜻이다. 적용 0 + 버림 다수라면" +
			" updateTimestampMs 필드 이름이 바뀌어 전량 버려지는 중인지 의심하라.")
	}
	fmt.Printf("  기타(하트비트 제외) 프레임: %d개\n", atomic.LoadInt64(&s.otherFrames))
	fmt.Printf("  관측된 decimalPrecision: %v\n", precs)
	if len(skipped) > 0 {
		// 관측된 값별로 묶어 찍는다 — 전부 0 이면 필드 이름 변경(JSON 제로값)이고,
		// 19 같은 값이면 거래소가 정밀도를 늘린 것이다. 둘은 대응이 다르다.
		byPrec := map[int]int{}
		for _, p := range skipped {
			byPrec[p]++
		}
		fmt.Printf("  ⚠️ decimalPrecision 범위 밖이라 건너뛴 마켓: %d개, 관측값별 %v (허용 1..%d — 값이 전부 0 이면 필드 이름 변경을 의심하라)\n",
			len(skipped), byPrec, ws.MaxPrecision)
	}
	fmt.Printf("  데이터 공백(재접속에 따른 OnGap) %d건:\n", len(gaps))
	for _, g := range gaps {
		d := g.End.Sub(g.Start)
		totalGapDur += d
		fmt.Printf("    %s ~ %s (%s, 사유=%s)\n",
			g.Start.Format(time.RFC3339), g.End.Format(time.RFC3339), d.Round(time.Millisecond), g.Reason)
	}
	fmt.Printf("  공백 합계(재접속 기인): %s\n", totalGapDur.Round(time.Millisecond))

	// 최대 무갱신 간격: 프레임 사이만이 아니라 실행 시작→첫 프레임,
	// 마지막 프레임→실행 종료 구간도 포함한다(onFrame 주석 참고). 한 건도
	// 못 받았으면 전체 실행 시간이 그대로 최대 간격이다.
	maxGap := atomic.LoadInt64(&s.maxOrderbookGapNs)
	lastNs := atomic.LoadInt64(&s.lastOrderbookMonoNs)
	tailGap := monoNow() - s.runStartMonoNs
	if lastNs != 0 {
		tailGap = monoNow() - lastNs
	}
	if tailGap > maxGap {
		maxGap = tailGap
	}
	fmt.Printf("  최대 무갱신 간격(오더북): %s\n", time.Duration(maxGap).Round(time.Second))

	s.mu.Lock()
	frameRecords := append([]frameRecord(nil), s.frames...)
	s.mu.Unlock()
	printFrameDistribution(frameRecords)

	failures := atomic.LoadInt64(&s.subscribeFailures)
	frames := atomic.LoadInt64(&s.orderbookFrames)

	// **판정을 두 축으로 나눈다 — 섞지 않는다(팀리드 지시, 2026-08-09).**
	//
	// (1) 배관 — 우리가 통제하는 것: 연결·구독·하트비트·재접속·전체 재구독·
	//     회차 롤오버. 이건 ✅/❌로 판정한다. exit code도 이것만 반영한다.
	// (2) 오더북 연속성 — 시장에 활동이 있어야 관측되는 것. 프레임 수가
	//     0이거나 적다고 우리 쪽 결함으로 단정하면 안 된다(실측:
	//     GET /v1/markets/{id}/orderbook이 이 시각 testnet 회차에서
	//     asks/bids 둘 다 빈 배열, updateTimestampMs=0 — 마켓메이커가
	//     없다). 그렇다고 "통과"로 적으면 기준을 조용히 느슨하게 만드는
	//     것이므로, 프레임이 있으면 관측치를 그대로 보고하고 없으면
	//     "미검증"이라고 명시한다 — ✅도 ❌도 아니다.
	fmt.Println()
	if failures > 0 {
		fmt.Println("판정(배관): 실패 —", failures, "건의 구독/구독해제 요청이 success=false로 거부됐다")
		return fmt.Errorf("판정(배관): 실패 — 구독 요청이 %d건 success=false로 거부됐다", failures)
	}
	// 관측 시간을 판정 문구에 하드코딩하지 않는다. -minutes 30 으로 돌린 실행이
	// "60분 내내 연결이 유지됐다"고 보고하던 것을 실측에서 잡았다 — 판정문이
	// 실행 조건과 다른 사실을 말하면, 그 로그를 나중에 읽는 사람이 없는 증거를
	// 있다고 믿는다.
	if subscribedTotal == 0 {
		msg := fmt.Sprintf("판정(배관): 실패 — %.0f분 동안 구독할 회차를 하나도 찾지 못했다", minutes)
		fmt.Println(msg)
		return fmt.Errorf("%s", msg)
	}
	fmt.Printf("판정(배관): 통과 — %.0f분 내내 연결이 유지됐고(재접속이 있었다면 전부 복구됐고) 모든 구독/구독해제 요청이 승인됐다\n", minutes)

	if frames == 0 {
		fmt.Printf("판정(오더북 연속성): ⚠️ 미검증 — %.0f분 동안 오더북 프레임을 한 건도 못 받았다. 이 환경에 시장 활동이 없었기 때문이지(REST 스냅샷도 빈 채였다) 배관 결함이 아니다. 통과도 실패도 아니다.\n", minutes)
	} else {
		fmt.Printf("판정(오더북 연속성): 관측됨 — %d개 프레임, 최대 무갱신 간격 %s. 이 값이 재호가 루프 설계의 입력이다(끊기지 않았다는 보장은 아니다 — 위 최대 간격을 직접 봐라).\n",
			frames, time.Duration(maxGap).Round(time.Second))
	}
	return nil
}

// printFrameDistribution은 **P5가 Stale 문턱을 정할 때 쓸 데이터**를 찍는다
// (팀리드 지시, 2026-08-09). 총 개수나 마지막 갱신 시각만으로는 문턱을 못
// 정한다 — 필요한 것은 (1) 연속된 오더북 프레임 사이 간격의 분포(중앙값·
// p90·p99), (2) 5분 회차 안에서 갱신이 언제 몰리는가(시작 직후/만기 직전/
// 균등)다. 후자가 특히 중요하다 — 이 봇은 회차 시작에 p_up을 고정하고
// 그 뒤로 호가를 따라가므로, 갱신이 만기 직전에 몰린다면 정작 주문을 걸어
// 두는 회차 초반에는 호가창이 거의 안 움직인다는 뜻이고 Stale 문턱과
// 재호가 루프 부하가 둘 다 달라진다.
func printFrameDistribution(frames []frameRecord) {
	fmt.Printf("  개별 프레임 기록: %d건\n", len(frames))
	if len(frames) < 2 {
		fmt.Println("  → 표본이 2건 미만이라 간격 분포를 계산할 수 없다.")
		return
	}

	sort.Slice(frames, func(i, j int) bool { return frames[i].RecvUnixNs < frames[j].RecvUnixNs })

	gaps := marketGaps(frames)
	if len(gaps) == 0 {
		fmt.Println("  → 같은 마켓의 연속 프레임 쌍이 없어 간격 분포를 계산할 수 없다.")
	} else {
		fmt.Printf("  프레임 간 간격 분포(**같은 마켓의** 연속 프레임 사이, n=%d): 중앙값=%.1fs p90=%.1fs p99=%.1fs 최대=%.1fs\n",
			len(gaps), percentile(gaps, 0.50), percentile(gaps, 0.90), percentile(gaps, 0.99), gaps[len(gaps)-1])
	}

	// 회차 안 시각 분포: 5분(300초) 회차를 30초씩 10구간으로 나눠 각 구간에
	// 몇 프레임이 왔는지 센다.
	//
	// **함정**: 이 프로브는 "지금 OPEN인 회차 전부"를 동시에 추적한다
	// (Step 1 실측: 한 시점에 15개 넘게 동시 OPEN). 그중 실제로 "지금
	// 진행 중"인 것은 한둘뿐이고 나머지는 아직 시작 전(startsAt이 미래)
	// 이거나 이미 끝난(endsAt이 과거인데 API가 아직 OPEN으로 보여주는)
	// 회차다. 시작 전 프레임은 경과가 **음수**인데, 그걸 단순히 0으로
	// 클램프하면 "회차가 막 시작했다"와 "회차가 아직 안 열렸다"가 같은
	// 구간에 섞인다 — 애초에 이 분포를 재는 목적(언제 갱신이 몰리는가)이
	// 무너진다. 그래서 [0, 300)초 밖은 별도로 세고 10구간에는 안 넣는다.
	const bucketSec = 30.0
	const numBuckets = 10 // 300 / 30
	buckets := make([]int, numBuckets)
	var beforeStart, afterEnd, unknown int
	for _, f := range frames {
		if !f.RoundStartOK {
			unknown++
			continue
		}
		switch {
		case f.ElapsedInRoundSec < 0:
			beforeStart++
		case f.ElapsedInRoundSec >= 300:
			afterEnd++
		default:
			idx := int(f.ElapsedInRoundSec / bucketSec)
			if idx >= numBuckets { // 부동소수점 경계(정확히 300.0 근접)
				idx = numBuckets - 1
			}
			buckets[idx]++
		}
	}
	fmt.Println("  회차 안 시각 분포(30초 구간, 회차 시작부터 경과 — [0,300)초만):")
	for i, c := range buckets {
		fmt.Printf("    %3d~%3ds: %d\n", i*30, (i+1)*30, c)
	}
	fmt.Printf("    회차 시작 전(아직 안 열림, 사전 구독): %d건\n", beforeStart)
	fmt.Printf("    회차 종료 후(만기 지났는데 API가 아직 OPEN으로 보여줌): %d건\n", afterEnd)
	if unknown > 0 {
		fmt.Printf("    (회차 시작 시각 파싱 실패로 분류 불가: %d건)\n", unknown)
	}
}

// marketGaps는 **같은 마켓 안에서** 연속한 두 프레임의 간격(초)을 모아
// 오름차순으로 돌려준다.
//
// **마켓을 섞어 세면 안 된다.** `(*ws.Book).Stale`은 호가창 하나씩 검사한다 —
// 마켓 A가 조용한 동안 마켓 B가 활발하면, 합산 간격은 짧게 나오지만 A의 책은
// 실제로 오래 멈춰 있다. 30분 소크 실측이 그 차이를 보여줬다: 합산으로는
// p99=0.9s·최대 3.8s, 같은 표본을 마켓별로 쪼개면 **p99=2.0s·최대 9.0s**다.
// 합산값으로 `Stale` 문턱을 잡으면 정상적인 소강을 죽은 연결로 오판한다.
//
// frames는 수신 시각 오름차순으로 정렬돼 있어야 한다.
func marketGaps(frames []frameRecord) []float64 {
	last := make(map[int64]int64, 8) // marketID → 직전 프레임의 RecvUnixNs
	gaps := make([]float64, 0, len(frames))
	for _, f := range frames {
		if prev, ok := last[f.MarketID]; ok {
			gaps = append(gaps, float64(f.RecvUnixNs-prev)/float64(time.Second))
		}
		last[f.MarketID] = f.RecvUnixNs
	}
	sort.Float64s(gaps)
	return gaps
}

// percentile은 정렬된 오름차순 슬라이스에서 p(0~1) 백분위수를 최근접 순위법
// (nearest-rank)으로 구한다. 통계 라이브러리를 새로 끌어오지 않기 위한
// 최소 구현이다 — 이 진단 도구 하나에서만 쓴다.
//
// 최근접 순위법의 정의는 rank = ceil(p·n), 값은 1-기반 rank번째다. **버림이
// 아니라 올림**이다 — int(p·n)-1로 쓰면 n=3, p=0.5에서 rank가 1이 되어
// 중앙값 자리에 최솟값이 온다. 이 프로브가 내놓는 p90/p99가 실제보다
// 낙관적으로 나오고, 그 숫자로 정하는 Stale 문턱이 너무 짧아진다.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// monoNow는 ws.Frame.RecvMonoNs와 같은 기준점(프로세스 시작 시각 이후 경과)의
// 현재 값이다. time.Now().UnixNano()(벽시계 에폭)를 쓰면 축이 완전히 달라
// 뺄셈이 터무니없는 값을 낸다 — internal/timing.Stamp가 쓰는 것과 같은
// start 기준점(timing.Uptime)을 그대로 써야 한다.
func monoNow() int64 {
	return timing.Uptime().Nanoseconds()
}
