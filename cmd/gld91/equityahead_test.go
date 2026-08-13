package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// 이 파일이 지키는 것: **미리 받아 둔 자본이 조용히 오래되지 않는다**, 그리고
// **배경 조회가 회차 경계를 방해하지 않는다.**
//
// 후자가 미묘하다. REST throttle 은 클라이언트 전체에 걸리므로, 배경 조회가
// 회차 시작 직전에 나가면 정작 주문이 333ms 를 기다린다 — 고치려던 지연을
// 그대로 되살린다.

func at(mmss string) time.Time {
	t, err := time.Parse("04:05", mmss)
	if err != nil {
		panic(err)
	}
	// 5분 주기 안에서의 위치만 의미가 있다. 기준 시각은 5분 경계에 맞춘다.
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(t.Minute()%5)*time.Minute + time.Duration(t.Second())*time.Second)
}

func TestQuietWindowAvoidsRoundBoundaries(t *testing.T) {
	for _, c := range []struct {
		mmss string
		want bool
	}{
		{"00:00", false}, // 회차 시작 — 절대 안 된다
		{"00:05", false}, // 진입 창 안
		{"00:44", false}, // 여유 45초 직전
		{"00:45", true},  // 여유 끝
		{"02:30", true},  // 한복판
		{"04:14", true},  // 다음 경계까지 46초
		{"04:15", true},  // 정확히 45초 전
		{"04:16", false}, // 44초 전 — 막는다
		{"04:59", false}, // 다음 회차 직전
	} {
		if got := quiet(at(c.mmss)); got != c.want {
			t.Errorf("%s: quiet=%v, 기대 %v", c.mmss, got, c.want)
		}
	}
}

// 조용한 창 밖에서는 조회하지 않는다. 이 가드가 없으면 배경 조회가 주문
// 바로 앞에 끼어들 수 있다.
func TestRefreshSkipsNearTheBoundary(t *testing.T) {
	a := &equityAhead{}
	if a.refresh(context.Background(), at("00:02")) {
		t.Error("회차 시작 2초 뒤에 배경 조회를 했다 — 주문이 그만큼 밀린다")
	}
	if a.refresh(context.Background(), at("04:50")) {
		t.Error("다음 회차 10초 전에 배경 조회를 했다")
	}
}

// 캐시가 신선하면 다시 조회하지 않는다. 매번 조회하면 요청 예산만 쓴다.
func TestRefreshRespectsTheInterval(t *testing.T) {
	now := at("02:00")
	a := &equityAhead{eq: risk.Equity{AvailableUSDT: 100}, at: now, ok: true}
	if a.refresh(context.Background(), now.Add(30*time.Second)) {
		t.Error("30초 만에 다시 조회했다 — 최소 간격 2분을 지키지 않았다")
	}
}

// 미리 받아 둔 값이 나이 안이면 그것을 쓴다. 그게 이 파일의 존재 이유다.
func TestReadUsesTheCachedValue(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 5, 0, 0, time.UTC)
	a := &equityAhead{eq: risk.Equity{AvailableUSDT: 96}, at: now.Add(-90 * time.Second), ok: true}

	eq, age, err := a.read(context.Background(), nil, &Config{}, now)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if eq.AvailableUSDT != 96 {
		t.Errorf("equity %v, 기대 96 — 캐시를 쓰지 않았다", eq.AvailableUSDT)
	}
	if age != 90*time.Second {
		t.Errorf("나이 %s, 기대 90s — 나이를 잘못 셌다", age)
	}
}

// **나이가 상한을 넘으면 캐시를 버린다.**
//
// read 가 아니라 cached 를 시험한다 — read 의 거짓 경로는 살아 있는 REST
// 클라이언트를 타므로, 그쪽으로 시험하면 상한을 지우는 변이를 놓친다
// (2026-08-14 변이 N5 에서 실제로 놓쳤다).
func TestCachedRejectsStaleValue(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 5, 0, 0, time.UTC)
	for _, c := range []struct {
		name string
		age  time.Duration
		want bool
	}{
		{"갓 받은 값", 0, true},
		{"한 회차 묵음", 5 * time.Minute, true},
		{"상한 직전", equityMaxAge - time.Second, true},
		{"상한 정각", equityMaxAge, true},
		{"상한 1초 초과", equityMaxAge + time.Second, false},
		{"한참 오래됨", time.Hour, false},
		{"미래 시각(시계 역행)", -time.Second, false},
	} {
		a := &equityAhead{eq: risk.Equity{AvailableUSDT: 96}, at: now.Add(-c.age), ok: true}
		eq, age, ok := a.cached(now)
		if ok != c.want {
			t.Errorf("%s (나이 %s): ok=%v, 기대 %v", c.name, c.age, ok, c.want)
			continue
		}
		if ok && (eq.AvailableUSDT != 96 || age != c.age) {
			t.Errorf("%s: equity %v / 나이 %s — 값이 어긋난다", c.name, eq.AvailableUSDT, age)
		}
	}
}

// 캐시가 아예 없거나 마지막 조회가 실패했으면 쓰지 않는다. 실패한 값을 들고
// 회차를 시작하면 그 회차는 조용히 걸지 않게 되는데, 그 사이 거래소는
// 멀쩡했을 수 있다.
func TestCachedRejectsEmptyOrFailed(t *testing.T) {
	now := time.Date(2026, 8, 14, 12, 5, 0, 0, time.UTC)
	if _, _, ok := (&equityAhead{}).cached(now); ok {
		t.Error("빈 캐시를 썼다")
	}
	if _, _, ok := (&equityAhead{at: now, ok: false}).cached(now); ok {
		t.Error("실패한 조회 결과를 썼다")
	}
}

// nil 이어도 안전하다 — 배선하지 않은 봇은 예전처럼 동기 조회로 돈다.
func TestNilAheadIsSafe(t *testing.T) {
	var a *equityAhead
	if a.refresh(context.Background(), at("02:00")) {
		t.Error("nil 인데 조회했다")
	}
	a.run(context.Background()) // 곧바로 돌아와야 한다
}

// ---------------------------------------------------------------------------
// 배선 — 함수가 맞아도 호출부가 없으면 아무 일도 일어나지 않는다
// ---------------------------------------------------------------------------

// runRound 가 ahead.read 를 지나야 한다. 예전의 readEquity 직접 호출로
// 되돌아가면 선조회가 통째로 죽는데, 위 시험들은 전부 통과한다.
func TestRunRoundReadsThroughAhead(t *testing.T) {
	src := mainSource(t)
	if !strings.Contains(src, "ahead.read(ctx, equitySrc, cfg, time.Now())") {
		t.Error("runRound 이 ahead.read 를 부르지 않는다 — 선조회가 배선되지 않았다")
	}
	if !strings.Contains(src, "go func() { defer wg.Done(); ahead.run(runCtx) }()") {
		t.Error("배경 조회 고루틴이 돌지 않는다 — 캐시가 영영 비어 있다")
	}
}

// applyTiming 이 StartedAt 을 채워야 첫 바퀴 체결 조회 생략이 켜진다.
// 제로면 exec 는 건너뛰지 않으므로, 배선이 빠지면 조용히 예전 속도로 돌아간다.
func TestStartedAtIsWired(t *testing.T) {
	src := mainSource(t)
	if !strings.Contains(src, "r.StartedAt = time.Now()") {
		t.Error("applyTiming 이 StartedAt 을 채우지 않는다 — 첫 바퀴 체결 조회 생략이 죽는다")
	}
}
