package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
)

// percentile은 최근접 순위법이다: rank = ceil(p·n), 값은 1-기반 rank번째.
// 기대값은 전부 손으로 계산했다 — 구현을 다시 옮겨 적으면 같은 실수를 같이
// 한다.
func TestPercentileNearestRank(t *testing.T) {
	// n=3: rank(0.50) = ceil(1.5) = 2 → 2번째 = 20
	//      rank(0.90) = ceil(2.7) = 3 → 3번째 = 30
	//      rank(0.99) = ceil(2.97) = 3 → 3번째 = 30
	three := []float64{10, 20, 30}
	// n=10: rank(0.50) = ceil(5) = 5 → 5번째 = 5
	//       rank(0.90) = ceil(9) = 9 → 9번째 = 9
	//       rank(0.99) = ceil(9.9) = 10 → 10번째 = 10
	ten := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

	cases := []struct {
		name   string
		sorted []float64
		p      float64
		want   float64
	}{
		{"n=3 중앙값", three, 0.50, 20},
		{"n=3 p90", three, 0.90, 30},
		{"n=3 p99", three, 0.99, 30},
		{"n=10 중앙값", ten, 0.50, 5},
		{"n=10 p90", ten, 0.90, 9},
		{"n=10 p99", ten, 0.99, 10},
		// p=0은 rank 0 → 하한 클램프로 1번째.
		{"p=0 하한", ten, 0.0, 1},
		// p=1은 rank n → 마지막.
		{"p=1 상한", ten, 1.0, 10},
		// n=1은 어떤 p에서도 그 하나.
		{"n=1 중앙값", []float64{42}, 0.50, 42},
		{"n=1 p99", []float64{42}, 0.99, 42},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := percentile(tc.sorted, tc.p); got != tc.want {
				t.Errorf("percentile(%v, %v) = %v, 기대 %v", tc.sorted, tc.p, got, tc.want)
			}
		})
	}
}

func TestPercentileEmpty(t *testing.T) {
	if got := percentile(nil, 0.5); got != 0 {
		t.Errorf("빈 슬라이스에서 %v, 기대 0", got)
	}
}

// 버림 구현(int(p·n)-1)이었다면 n=3 중앙값이 20이 아니라 10으로 나온다.
// 그 회귀를 이름으로 못 박아 둔다.
func TestPercentileIsNotTruncating(t *testing.T) {
	if got := percentile([]float64{10, 20, 30}, 0.50); got == 10 {
		t.Error("중앙값이 최솟값으로 나왔다 — 올림이 아니라 버림으로 되돌아갔다")
	}
}

func fr(marketID int64, sec float64) frameRecord {
	return frameRecord{MarketID: marketID, RecvUnixNs: int64(sec * float64(time.Second))}
}

// 두 마켓이 번갈아 갱신되는 표본. 합산으로 세면 간격이 전부 1초로 보이지만,
// 각 호가창은 실제로 2초씩 멈춰 있다 — Stale 이 보는 것은 후자다.
//
// 이 구분이 없으면 30분 소크의 p99 가 2.0s 대신 0.9s 로, 최대가 9.0s 대신
// 3.8s 로 나온다. 그 숫자로 Stale 문턱을 잡으면 정상적인 소강을 죽은
// 연결로 오판한다.
func TestMarketGapsDoesNotMergeMarkets(t *testing.T) {
	frames := []frameRecord{
		fr(1, 0), fr(2, 1), fr(1, 2), fr(2, 3), fr(1, 4), fr(2, 5),
	}
	gaps := marketGaps(frames)
	want := []float64{2, 2, 2, 2}
	if len(gaps) != len(want) {
		t.Fatalf("간격 %d개(%v), 기대 %d개 — 마켓을 섞어 셌다", len(gaps), gaps, len(want))
	}
	for i := range want {
		if gaps[i] != want[i] {
			t.Errorf("gaps[%d] = %v, 기대 %v (전체=%v)", i, gaps[i], want[i], gaps)
		}
	}
}

func TestMarketGapsIsSortedAscending(t *testing.T) {
	// 수신 시각 오름차순(0,1,4,5,6). 마켓 1은 5초·1초 간격, 마켓 2는 3초.
	frames := []frameRecord{fr(1, 0), fr(2, 1), fr(2, 4), fr(1, 5), fr(1, 6)}
	gaps := marketGaps(frames)
	want := []float64{1, 3, 5}
	if len(gaps) != 3 {
		t.Fatalf("간격 %v, 기대 3개", gaps)
	}
	for i := range want {
		if gaps[i] != want[i] {
			t.Errorf("gaps = %v, 기대 %v", gaps, want)
			break
		}
	}
}

// 마켓마다 프레임이 하나뿐이면 잴 간격이 없다 — 0건이지 0초가 아니다.
// 여기서 0초짜리 간격을 만들어 내면 중앙값이 0으로 내려앉는다.
func TestMarketGapsSingleFramePerMarketYieldsNoGaps(t *testing.T) {
	if gaps := marketGaps([]frameRecord{fr(1, 0), fr(2, 1), fr(3, 2)}); len(gaps) != 0 {
		t.Errorf("간격 %v, 기대 없음", gaps)
	}
}

func cat(slug, startsAt, endsAt string) category {
	return category{Slug: slug, StartsAt: startsAt, EndsAt: endsAt}
}

// roundIsLive의 경계. now는 회차 하나의 한가운데로 잡는다.
func TestRoundIsLive(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 2, 30, 0, time.UTC)
	rfc := func(t time.Time) string { return t.Format(time.RFC3339) }

	cases := []struct {
		name string
		c    category
		want bool
	}{
		{
			"진행 중(12:00~12:05)",
			cat("btc-updown-5m-1", rfc(now.Add(-150*time.Second)), rfc(now.Add(150*time.Second))),
			true,
		},
		{
			"막 시작(startsAt == now)",
			cat("btc-updown-5m-2", rfc(now), rfc(now.Add(5*time.Minute))),
			true,
		},
		{
			"lookahead 안에 시작(+59초)",
			cat("btc-updown-5m-3", rfc(now.Add(59*time.Second)), rfc(now.Add(59*time.Second+5*time.Minute))),
			true,
		},
		{
			"lookahead 경계에 시작(+60초)",
			cat("btc-updown-5m-4", rfc(now.Add(roundLookahead)), rfc(now.Add(roundLookahead+5*time.Minute))),
			true,
		},
		{
			"lookahead 밖에 시작(+61초)",
			cat("btc-updown-5m-5", rfc(now.Add(roundLookahead+time.Second)), rfc(now.Add(roundLookahead+time.Second+5*time.Minute))),
			false,
		},
		{
			"23시간 뒤 사전 등록 — 이것이 앞선 소크를 망친 표본",
			cat("btc-updown-5m-6", rfc(now.Add(23*time.Hour)), rfc(now.Add(23*time.Hour+5*time.Minute))),
			false,
		},
		{
			"이미 끝남(endsAt == now)",
			cat("btc-updown-5m-7", rfc(now.Add(-5*time.Minute)), rfc(now)),
			false,
		},
		{
			"이미 끝남(endsAt < now, API는 아직 OPEN)",
			cat("btc-updown-5m-8", rfc(now.Add(-10*time.Minute)), rfc(now.Add(-5*time.Minute))),
			false,
		},
		{
			"1초 남음",
			cat("btc-updown-5m-9", rfc(now.Add(-5*time.Minute)), rfc(now.Add(time.Second))),
			true,
		},
		{"startsAt 파싱 실패는 버린다", cat("btc-updown-5m-a", "", rfc(now.Add(time.Minute))), false},
		{"endsAt 파싱 실패는 버린다", cat("btc-updown-5m-b", rfc(now.Add(-time.Minute)), "nonsense"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roundIsLive(tc.c, now); got != tc.want {
				t.Errorf("roundIsLive(%+v) = %v, 기대 %v", tc.c, got, tc.want)
			}
		})
	}
}

// --- onFrame: 버려진 프레임이 계측을 오염시키지 않아야 한다 ---
//
// Book.Apply 가 (applied, err) 를 돌려주게 바꾼 것은 절반일 뿐이다. 컴파일러는
// 호출자가 applied 를 **받도록** 강제할 뿐, 그 값에 따라 **행동하도록** 강제하지
// 못한다 — `if !applied { 카운터만 올리고 return 을 뺀다` 같은 변경은 멀쩡히
// 컴파일되면서 원래 결함을 그대로 되살린다. 그래서 여기서 못 박는다.
//
// 버려진 프레임이 적용으로 세어지면 세 가지가 동시에 틀린다: 수신 개수가
// 실제 갱신보다 많아지고, lastOrderbookMonoNs 가 앞당겨져 **멈춘 호가창이
// 신선하다고 보고되며**, frames 에 없던 짧은 간격이 섞여 P5 Stale 문턱의
// 실측 근거가 오염된다.
func obFrame(marketID int64, updateTS int64, recvMonoNs int64) ws.Frame {
	raw := fmt.Sprintf(`{"asks":[],"bids":[[0.48,20]],"updateTimestampMs":%d}`, updateTS)
	return ws.Frame{
		RecvMonoNs: recvMonoNs,
		RecvUnixNs: recvMonoNs,
		Raw:        []byte(raw),
		Msg: ws.Message{
			Type:  ws.TypeMessage,
			Topic: ws.TopicOrderbook(marketID),
			Data:  json.RawMessage(raw),
		},
	}
}

func TestOnFrameDroppedFrameDoesNotPolluteMetrics(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	const marketID = int64(1)

	s := newReadState(0)
	s.tracked[marketID] = &trackedMarket{
		ID: marketID, Slug: "btc-updown-5m-t", DecimalPrecision: 2, Book: ws.NewBook(2),
	}

	// 1) 정상 프레임 하나 — 적용된다.
	s.onFrame(obFrame(marketID, 2_000, 100), log)
	if got := atomic.LoadInt64(&s.orderbookFrames); got != 1 {
		t.Fatalf("적용 프레임 %d개, 기대 1", got)
	}
	if got := atomic.LoadInt64(&s.lastOrderbookMonoNs); got != 100 {
		t.Fatalf("lastOrderbookMonoNs = %d, 기대 100", got)
	}
	if got := len(s.frames); got != 1 {
		t.Fatalf("frames %d건, 기대 1", got)
	}

	// 2) 과거 타임스탬프 — Apply 가 버린다. 아래 셋 중 어느 것도 변하면 안 된다.
	s.onFrame(obFrame(marketID, 1_000, 500), log)
	// 3) 같은 타임스탬프(재구독 직후 재전송) — 역시 버린다.
	s.onFrame(obFrame(marketID, 2_000, 900), log)

	if got := atomic.LoadInt64(&s.orderbookFrames); got != 1 {
		t.Errorf("버려진 프레임이 적용 개수에 들어갔다: %d, 기대 1", got)
	}
	if got := atomic.LoadInt64(&s.lastOrderbookMonoNs); got != 100 {
		t.Errorf("버려진 프레임이 마지막 갱신 시각을 앞당겼다: %d, 기대 100"+
			" — 멈춘 호가창이 신선하다고 보고된다", got)
	}
	if got := len(s.frames); got != 1 {
		t.Errorf("버려진 프레임이 간격 분포 표본에 들어갔다: %d건, 기대 1건"+
			" — 이 분포가 P5 Stale 문턱의 실측 근거다", got)
	}
	if got := atomic.LoadInt64(&s.droppedFrames); got != 2 {
		t.Errorf("버림 카운터 = %d, 기대 2 — 버린 사실 자체는 보고돼야 한다", got)
	}
}
