package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/ws"
)

func mkRound(startUnix int64, marketID int64) live.Round {
	return live.Round{
		CategoryID: marketID, MarketID: marketID,
		Slug:      fmt.Sprintf("btc-updown-5m-%d", startUnix),
		StartsAt:  time.Unix(startUnix, 0).UTC(),
		EndsAt:    time.Unix(startUnix+300, 0).UTC(),
		Precision: 2, FeeRateBps: 200,
		UpTokenID: "1111111111111111111", DownTokenID: "2222222222222222222",
	}
}

func routerWith(rounds ...live.Round) *router {
	rt := newRouter()
	for _, r := range rounds {
		rt.items[r.MarketID] = &tracked{round: r, book: ws.NewBook(r.Precision)}
	}
	return rt
}

// pick 은 "시작했고 안 끝났고 아직 안 돌린" 회차 중 **가장 먼저 시작한** 것을
// 고른다. 나중 것을 고르면 앞 회차를 통째로 건너뛴다.
func TestPickChoosesEarliestRunnableRound(t *testing.T) {
	const t0 = 1786275000
	rt := routerWith(mkRound(t0, 1), mkRound(t0+300, 2), mkRound(t0+600, 3))

	// t0+310: 첫 회차는 끝났고(t0+300), 둘째가 진행 중, 셋째는 아직이다.
	got, ok := rt.pick(time.Unix(t0+310, 0).UTC(), 2*time.Minute, map[string]bool{})
	if !ok {
		t.Fatal("고른 회차가 없다")
	}
	if got.round.MarketID != 2 {
		t.Errorf("marketId %d 를 골랐다, 기대 2", got.round.MarketID)
	}
}

// 이미 돌린 회차를 다시 고르면 같은 회차를 200ms 마다 반복해서 잡는다.
func TestPickSkipsRoundsAlreadyRun(t *testing.T) {
	const t0 = 1786275000
	r := mkRound(t0, 1)
	rt := routerWith(r)
	done := map[string]bool{r.Slug: true}
	if got, ok := rt.pick(time.Unix(t0+10, 0).UTC(), 2*time.Minute, done); ok {
		t.Fatalf("이미 돌린 회차를 다시 골랐다: %s", got.round.Slug)
	}
}

// 아직 시작 안 한 회차는 고르지 않는다 — p_up 은 회차 시작 시각의 마감된
// 봉으로만 만든다. 시작 전에 동결하면 그 시각의 봉이 아직 안 닫혀 자격
// 검사에서 걸리고, 걸리지 않는다면 그것이야말로 미래참조다.
func TestPickWaitsForRoundStart(t *testing.T) {
	const t0 = 1786275000
	rt := routerWith(mkRound(t0, 1))

	if _, ok := rt.pick(time.Unix(t0-1, 0).UTC(), 2*time.Minute, map[string]bool{}); ok {
		t.Error("시작 1초 전 회차를 골랐다")
	}
	// 정각은 고른다 — 그 시각의 직전 봉은 이미 닫혀 있다.
	if _, ok := rt.pick(time.Unix(t0, 0).UTC(), 2*time.Minute, map[string]bool{}); !ok {
		t.Error("시작 정각 회차를 고르지 못했다")
	}
}

// 끝난 회차는 고르지 않는다. endsAt 정각도 끝난 것이다 — exec 의 루프가
// `!now.Before(EndsAt)` 에서 즉시 빠져나오므로 잡아 봐야 빈 회차를 돈다.
func TestPickSkipsFinishedRounds(t *testing.T) {
	const t0 = 1786275000
	rt := routerWith(mkRound(t0, 1))
	// 합류 상한을 회차보다 크게 둬서 "끝났는가" 하나만 본다 — 상한과 섞으면
	// 어느 규칙이 걸렀는지 알 수 없다.
	const noLateLimit = time.Hour
	if _, ok := rt.pick(time.Unix(t0+300, 0).UTC(), noLateLimit, map[string]bool{}); ok {
		t.Error("endsAt 정각 회차를 골랐다")
	}
	if _, ok := rt.pick(time.Unix(t0+299, 0).UTC(), noLateLimit, map[string]bool{}); !ok {
		t.Error("1초 남은 회차를 고르지 못했다")
	}
}

// **너무 늦게 합류하지 않는다.** 회차 중반에 봇을 띄우면 p_up 은 회차 시작
// 시각의 것인데 시장은 이미 그 방향으로 움직였을 수 있고, G2 가 잰 엣지는
// 회차 시작 근처 진입을 가정한 값이다.
func TestPickRefusesToJoinTooLate(t *testing.T) {
	const t0 = 1786275000
	rt := routerWith(mkRound(t0, 1))
	const late = 120 * time.Second

	if _, ok := rt.pick(time.Unix(t0, 0).Add(late).UTC(), late, map[string]bool{}); !ok {
		t.Error("경계(정확히 상한)에서 거부했다")
	}
	if _, ok := rt.pick(time.Unix(t0, 0).Add(late+time.Second).UTC(), late, map[string]bool{}); ok {
		t.Error("상한을 넘겨 합류했다")
	}
}

// sync 는 새 회차를 구독하고, **끝난 회차만** 구독해제한다. 아직 안 끝난
// 회차를 목록에서 빠졌다는 이유로 지우면 운용 중인 호가창이 사라진다.
func TestSyncKeepsRunningRoundEvenIfPollDropsIt(t *testing.T) {
	const t0 = 1786275000
	rt := newRouter()
	sender := &recordingSender{}

	now := time.Unix(t0+10, 0).UTC()
	rt.sync(context.Background(), sender, []live.Round{mkRound(t0, 1)}, now)
	if len(rt.items) != 1 {
		t.Fatalf("구독 %d개", len(rt.items))
	}
	if len(sender.sent) != 1 || sender.sent[0].Method != "subscribe" {
		t.Fatalf("구독 요청이 안 나갔다: %+v", sender.sent)
	}

	// 폴링이 회차를 빠뜨렸다(예: 응답이 잠깐 이상했다). 아직 안 끝났으므로
	// 유지해야 한다.
	rt.sync(context.Background(), sender, nil, now)
	if len(rt.items) != 1 {
		t.Errorf("진행 중 회차를 지웠다 (남은 %d개)", len(rt.items))
	}

	// 끝난 뒤에는 지운다.
	rt.sync(context.Background(), sender, nil, time.Unix(t0+300, 0).UTC())
	if len(rt.items) != 0 {
		t.Errorf("끝난 회차가 남았다 (%d개)", len(rt.items))
	}
}

// 같은 회차를 두 번 받아도 호가창을 새로 만들지 않는다 — 새로 만들면 그
// 순간 호가창이 비고, 봇은 stale 로 판정해 걸린 주문을 취소한다.
func TestSyncDoesNotRecreateBooks(t *testing.T) {
	const t0 = 1786275000
	rt := newRouter()
	sender := &recordingSender{}
	now := time.Unix(t0+10, 0).UTC()

	rt.sync(context.Background(), sender, []live.Round{mkRound(t0, 1)}, now)
	first := rt.items[1].book
	rt.sync(context.Background(), sender, []live.Round{mkRound(t0, 1)}, now)
	if rt.items[1].book != first {
		t.Error("같은 회차의 호가창을 다시 만들었다")
	}
	if n := len(sender.sent); n != 1 {
		t.Errorf("구독 요청이 %d건 나갔다, 기대 1건", n)
	}
}

// done 맵이 무한히 자라지 않는다. 24시간이면 288회차라 그대로 둬도 크지
// 않지만, 무한히 자라는 맵을 남기지 않는다.
func TestPruneDoneDropsEntriesForVanishedRounds(t *testing.T) {
	const t0 = 1786275000
	rt := routerWith(mkRound(t0, 1))
	done := map[string]bool{}
	for i := 0; i < 100; i++ {
		done[fmt.Sprintf("btc-updown-5m-%d", t0-int64(i+1)*300)] = true
	}
	done[mkRound(t0, 1).Slug] = true

	pruneDone(done, rt, time.Unix(t0+10, 0).UTC())
	if len(done) != 1 {
		t.Errorf("정리 후 %d개 남았다, 기대 1개", len(done))
	}
	if !done[mkRound(t0, 1).Slug] {
		t.Error("아직 추적 중인 회차의 표시를 지웠다 — 같은 회차를 다시 잡게 된다")
	}
}

// recordingSender 는 나간 구독 요청을 기록한다. 실제 소켓을 열지 않는다.
type recordingSender struct{ sent []ws.Request }

func (s *recordingSender) Send(_ context.Context, req ws.Request) error {
	s.sent = append(s.sent, req)
	return nil
}
