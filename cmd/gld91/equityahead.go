package main

import (
	"context"
	"sync"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// ---------------------------------------------------------------------------
// equity 를 회차 시작 **전에** 받아 둔다
// ---------------------------------------------------------------------------
//
// # 왜 — 임계 경로에서 REST 왕복 하나를 뺀다
//
// REST 클라이언트는 요청 사이에 333ms 를 강제로 끼운다(`rest.minInterval`,
// 240 req/min 예산의 절반만 쓰는 자체 제한). 회차 시작의 순서가 이랬다:
//
//	equity 조회   REST  실측 ~430ms
//	체결 조회     REST  +333ms 대기 (2026-08-14 에 첫 바퀴는 건너뛴다)
//	주문 생성     REST  +333ms 대기
//
// 실측 229 회차에서 회차 진입부터 주문 서명까지 중앙 831ms 였고 그 대부분이
// 대기였다. equity 를 미리 받아 두면 회차 시작에 남는 REST 는 주문 생성
// 하나뿐이다.
//
// **테이커 다리가 생기면서 이 지연이 비용이 됐다.** 테이커 가격은 호가창에서
// 읽으므로, 읽은 시점과 주문이 거래소에 닿는 시점이 벌어질수록 빗나간다.
// 메이커 다리만 있던 시절에는 가격이 상수라 지연이 공짜였다.
//
// # 왜 안전한가
//
// equity 는 `-include-positions` 로 **현금 + 미정산 포지션 원가**다. 체결은
// 현금을 포지션으로 옮길 뿐이라 합계를 거의 바꾸지 않는다. 합계를 실제로
// 바꾸는 것은 정산인데, 정산 한 번의 크기는 베팅 하나(= equity 의 4.55%)를
// 넘지 않는다.
//
// 그래서 한 회차 묵은 값을 써도 cap 오차는 4.55% × 4.55% ≈ equity 의 0.2%
// 이하다. 그래도 나이를 [equityMaxAge] 로 자르고, 넘으면 **그 자리에서 다시
// 조회한다** — 오래된 값이 조용히 쓰이는 경로를 두지 않는다.
//
// # 조회가 회차 경계를 방해하면 안 된다
//
// throttle 은 클라이언트 전체에 걸린다. 배경 조회가 회차 시작 직전에 나가면
// 정작 주문이 333ms 를 기다린다 — 고치려던 것을 그대로 되살리는 셈이다.
// 그래서 회차 경계에서 [equityQuietMargin] 안쪽이면 조회하지 않는다.

const (
	// roundPeriod 는 회차 주기다. 이 봇은 `btc-updown-5m` 마켓만 본다.
	//
	// 이 값은 **조용한 창을 고르는 데만** 쓴다 — 회차의 실제 시작·종료는
	// 언제나 거래소가 준 [live.Round] 에서 온다. 여기서 틀려도 배경 조회
	// 시점이 어긋날 뿐 거래 판단에는 닿지 않는다.
	roundPeriod = 5 * time.Minute

	// equityRefreshEvery 는 배경 조회의 최소 간격이다. 회차마다 한 번이면
	// 충분하다 — 더 자주 해도 정확해지지 않고 요청 예산만 쓴다.
	equityRefreshEvery = 2 * time.Minute

	// equityQuietMargin 은 회차 경계 앞뒤로 배경 조회를 금지하는 폭이다.
	// 주문이 나가는 창(진입 창 5초)보다 넉넉히 크다.
	equityQuietMargin = 45 * time.Second

	// equityMaxAge 는 미리 받아 둔 값을 쓸 수 있는 나이의 상한이다.
	// 회차 하나(5분)보다 조금 길다 — 배경 조회가 한 번 실패해도 다음 회차는
	// 캐시로 넘어가고, 두 번 연속 실패하면 그 자리에서 다시 조회한다.
	equityMaxAge = 6 * time.Minute
)

// equityAhead 는 미리 받아 둔 equity 다.
//
// 제로값은 "받아 둔 것이 없다"이고, 그때 [equityAhead.read] 는 그냥 동기
// 조회로 떨어진다. nil 이어도 안전하다 — 배선하지 않은 봇은 예전처럼 돈다.
type equityAhead struct {
	src *live.EquitySource
	cfg *Config
	log func(string, ...any)

	mu sync.Mutex
	eq risk.Equity
	at time.Time
	// ok 는 마지막 조회가 성공했는지다. **실패는 캐시하지 않는다** — 실패한
	// 값을 들고 회차를 시작하면 그 회차는 조용히 걸지 않게 되는데, 그 사이
	// 거래소는 멀쩡했을 수 있다.
	ok bool
}

func newEquityAhead(src *live.EquitySource, cfg *Config, log func(string, ...any)) *equityAhead {
	return &equityAhead{src: src, cfg: cfg, log: log}
}

// quiet 은 지금이 배경 조회를 해도 되는 시점인지 본다 — 회차 경계에서
// [equityQuietMargin] 밖.
func quiet(now time.Time) bool {
	off := now.Sub(now.Truncate(roundPeriod))
	return off >= equityQuietMargin && off <= roundPeriod-equityQuietMargin
}

// refresh 는 조건이 맞으면 한 번 조회해 캐시에 넣는다. 조회했으면 true.
func (a *equityAhead) refresh(ctx context.Context, now time.Time) bool {
	if a == nil {
		return false
	}
	if !quiet(now) {
		return false
	}
	a.mu.Lock()
	fresh := a.ok && now.Sub(a.at) < equityRefreshEvery
	a.mu.Unlock()
	if fresh {
		return false
	}
	eq, err := readEquity(ctx, a.src, a.cfg)
	if err != nil {
		// 조용히 넘어간다. 회차 시작의 동기 조회가 같은 실패를 다시 만나면
		// 그때 제대로 로그를 남기고 그 회차를 거른다.
		a.mu.Lock()
		a.ok = false
		a.mu.Unlock()
		return true
	}
	a.mu.Lock()
	a.eq, a.at, a.ok = eq, now, true
	a.mu.Unlock()
	return true
}

// run 은 배경에서 캐시를 채운다. ctx 가 끝나면 돌아온다.
func (a *equityAhead) run(ctx context.Context) {
	if a == nil {
		return
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			a.refresh(ctx, now)
		}
	}
}

// read 는 회차 시작에 쓸 equity 를 준다.
//
// 미리 받아 둔 값이 [equityMaxAge] 안이면 그것을 쓰고(age 가 그 나이),
// 아니면 **그 자리에서 조회한다**(age 는 0).
func (a *equityAhead) read(ctx context.Context, src *live.EquitySource, cfg *Config, now time.Time) (risk.Equity, time.Duration, error) {
	if eq, age, ok := a.cached(now); ok {
		return eq, age, nil
	}
	eq, err := readEquity(ctx, src, cfg)
	return eq, 0, err
}

// cached 는 **미리 받아 둔 값을 써도 되는지** 판정한다.
//
// [equityAhead.read] 에서 떼어낸 이유는 시험 가능성이다. read 는 판정이
// 거짓일 때 동기 조회로 떨어지는데, 그 경로에는 살아 있는 REST 클라이언트가
// 필요하다. 그러면 나이 상한을 지우는 변이를 시험이 잡지 못한다 — 실제로
// 놓쳤다(2026-08-14 변이 N5).
//
// ok=false 는 셋 중 하나다: 캐시가 없다, 마지막 조회가 실패했다, 너무
// 오래됐다. 셋 다 "그 자리에서 다시 조회하라" 는 뜻이다.
//
// 미래 시각(age < 0)도 거짓이다. 시계가 뒤로 갔다는 뜻이고, 그 상태에서
// 나이를 믿으면 영영 늙지 않는 캐시가 된다.
func (a *equityAhead) cached(now time.Time) (risk.Equity, time.Duration, bool) {
	if a == nil {
		return risk.Equity{}, 0, false
	}
	a.mu.Lock()
	eq, at, ok := a.eq, a.at, a.ok
	a.mu.Unlock()
	if !ok || at.IsZero() {
		return risk.Equity{}, 0, false
	}
	age := now.Sub(at)
	if age < 0 || age > equityMaxAge {
		return risk.Equity{}, 0, false
	}
	return eq, age, true
}
