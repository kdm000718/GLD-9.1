# P5 — 봇 본체 (호가·사이징·집행·원장) 구현 계획

> **에이전트에게:** 이 계획은 `superpowers:subagent-driven-development` 로 태스크 단위 실행한다. 단계는 체크박스(`- [ ]`)다.

**목표:** P4 가 만든 거래소 층 위에 실제로 판단하고 주문을 내는 봇을 올린다 — 회차 선택, `p_up` 동결, 호가 추종, 사이징, 집행, 원장.

**아키텍처:** 순수 함수 모듈(`quote`·`risk`)이 결정을 내리고, `exec` 가 오더북·주문·시계를 그 결정에 연결한다. 부수효과(주문 전송)는 한 곳(`exec.transmit`)에만 있고 그 지점에 `LIVE_ARM` 게이트가 있다. 서명은 무장 여부와 무관하게 **항상** 한다 — DRY-RUN 이 실거래와 다른 코드를 타면 DRY-RUN 이 아무것도 증명하지 못한다.

**기술 스택:** Go, 기존 `internal/*` 재사용. 새 의존성 없음(`go-ethereum` v1.15.0 고정).

## Global Constraints

스펙(`docs/superpowers/specs/2026-08-08-gld91-predictfun-bot-design.md`)과 사용자 지시에서 그대로 옮긴다. 어기면 실제로 돈을 잃는다.

- **문턱 `0.0172`.** `confidence = 2 × |p_up − 0.5|` 가 이 값 **미만이면 그 회차는 아무것도 하지 않는다.**
- **회차당 최대 명목 `< equity × 0.0455`.** 강한 부등호다. 최소 주문 `$1`.
- **매수만 한다.** 매도 주문 없음. 청산은 정산으로만.
- **0.5 미만 지정가만.** `ceiling = 0.5 − 1틱` (정밀도 2면 0.49, 3이면 0.499). 리터럴 금지 — 틱에서 유도한다.
- **관통 방지.** `target >= best_ask` 이면 `target = best_ask − 1틱`. 테이커가 되면 안 된다.
- **같은 가격이면 취소·재주문 하지 않는다.** 큐 위치 보존.
- **최우선 호가에서 우리 주문을 제외한다.**
- **재호가 쿨다운 기본 500ms.**
- **`p_up` 은 회차 시작에 동결.** 회차 중간에 피처를 다시 만드는 코드 경로가 존재하면 안 된다.
- **`Stale` 문턱 기본 3초** — 넘으면 신규 주문 중단 + 기존 주문 취소. (P4 실측: 같은 호가창 p99 2.0s, 최대 9.0s. 기본값 3초는 스펙 값이며 운영에서 조정한다.)
- **시크릿은 환경변수로만.** `PREDICT_API_KEY` / `WALLET_PRIVATE_KEY` / `PREDICT_ACCOUNT`. 소스·로그·에러·테스트·골든 어디에도 실값 금지. 저장소는 GitHub 에 올라간다.
- **`LIVE_ARM='I_UNDERSTAND_THE_RISK'` 없으면 DRY-RUN** — 서명은 하되 전송하지 않는다.
- **`maker`/`signer` 는 `PREDICT_ACCOUNT` 에서만 온다.** `GET /v1/account` 의 `address` 는 서명자 EOA 이지 스마트계정이 아니다(P4 실측). API 응답에서 유도 금지.
- **레이트리밋 240 req/min 은 키 단위.** `rest.Client` 의 333ms 스로틀을 우회하지 않는다.

## 미확정 값 — 격리해서 한 곳에 모은다

Task 10 Step 4~5(실주문 1건)가 아직 안 돌았다. 세 값이 미확정이고, **그 값이 바뀔 때 고칠 자리를 한 곳으로 모으는 것이 이 계획의 설계 제약이다.**

| 미확정 | 지금 쓰는 가정 | 바뀌면 고칠 곳 |
|---|---|---|
| 가용 USDT 를 어디서 읽나 | 온체인 `USDT.balanceOf(PREDICT_ACCOUNT)` | `internal/live/equity.go` 한 파일 |
| `removalLockedUntil` 잠금 길이 | 응답값을 그대로 읽어 지킨다(길이 가정 없음) | 없음 — 설계가 값에 의존하지 않는다 |
| `shareThreshold` 의 의미 | 리베이트 자격선 **가설**. 사이징에 반영하지 않는다 | `ledger` 의 기대 리베이트 주석만 |

**`shareThreshold` 를 사이징에 반영하지 않는 이유**: 가설이 맞아도 그것은 리베이트 유무일 뿐이고 엣지는 리베이트 없이도 +1.974%p 양수다(G2). 미확정 가설로 주문 크기를 바꾸면 틀렸을 때 손실이 나지만, 반영하지 않으면 틀렸을 때 리베이트를 조금 못 받을 뿐이다.

## 파일 구조

```
internal/sample/serve.go      학습·서빙 공용 자격 검사 (신규, Build 가 이걸 쓰게 바꾼다)
internal/quote/quote.go       목표가·행동 결정 (순수)
internal/risk/risk.go         사이저·노출·일손실 (순수)
internal/ledger/ledger.go     CSV 원장
internal/predictfun/rest/orders.go     주문 엔드포인트 + Bearer
internal/live/rounds.go       회차 선택 (probe 의 진단 구현을 승격)
internal/live/equity.go       equity 조회 — 미확정 가정이 사는 유일한 파일
internal/live/predict.go      회차 시작에 p_up 계산·동결
internal/exec/exec.go         마켓메이킹 루프
cmd/gld91/main.go             설정·배선·무장 게이트
```

---

### Task 1: `internal/sample` — 학습과 서빙이 같은 자격 검사를 쓰게 한다

**Files:**
- Create: `internal/sample/serve.go`
- Modify: `internal/sample/sample.go` (`Build` 가 새 함수를 쓰도록)
- Test: `internal/sample/serve_test.go`

**Interfaces:**
- Produces: `sample.Features(b1, b5 bars.Bars, t int64) (vals []float64, reason Reason)`, `type Reason int` 상수 `Eligible/Warmup/Gap`.

**왜 이 태스크가 먼저인가**: 서빙 경로가 자격 검사를 따로 구현하면 학습 표본과 서빙 표본이 갈린다. `internal/sample` 패키지 자체가 그 드리프트를 막으려고 존재한다(패키지 주석). 그 규칙을 서빙에도 **같은 함수로** 적용한다.

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`internal/sample/serve_test.go`:

```go
package sample

import "testing"

// Features 와 Build 가 같은 표본을 채택해야 한다. 하나라도 어긋나면
// 게이트가 검증한 적 없는 입력으로 실거래 예측이 나간다.
func TestFeaturesAgreesWithBuild(t *testing.T) {
	b1, b5 := syntheticBars(t) // 기존 테스트 헬퍼 재사용 (sample_test.go)
	cs, mat, _, counts := Build(b1, b5, nil)

	accepted := map[int64]bool{}
	for i := 0; i < counts.Kept; i++ {
		accepted[cs[i]] = true
	}
	for i := 0; i < b5.Len(); i++ {
		ts := b5.OpenTime[i]
		vals, reason := Features(b1, b5, ts)
		// 도지는 Build 만의 규칙(라벨이 없다)이므로 제외하고 비교한다.
		if b5.Close[i] == b5.Open[i] {
			continue
		}
		if (reason == Eligible) != accepted[ts] {
			t.Errorf("t=%d: Features reason=%v, Build 채택=%v", ts, reason, accepted[ts])
		}
		if reason == Eligible {
			row := mat.Row(indexOf(cs, counts.Kept, ts))
			for j := range vals {
				if float32(vals[j]) != row[j] {
					t.Errorf("t=%d 피처[%d] 불일치: %v vs %v", ts, j, vals[j], row[j])
					break
				}
			}
		}
	}
}
```

`indexOf` 는 이 테스트 안에 작은 헬퍼로 둔다.

- [ ] **Step 2: 실패를 확인한다**

`GOTOOLCHAIN=local go test ./internal/sample/ -run TestFeaturesAgreesWithBuild`
기대: 컴파일 실패 (`Features` 없음).

- [ ] **Step 3: `serve.go` 를 만든다**

```go
package sample

import (
	"github.com/kdm000718/GLD-9.1/internal/bars"
	"github.com/kdm000718/GLD-9.1/internal/clock"
	"github.com/kdm000718/GLD-9.1/internal/features"
)

// Reason 은 표본이 채택되지 못한 사유다.
type Reason int

const (
	Eligible Reason = iota
	Warmup
	Gap
)

func (r Reason) String() string {
	switch r {
	case Eligible:
		return "eligible"
	case Warmup:
		return "warmup"
	case Gap:
		return "gap"
	}
	return "unknown"
}

// Features 는 시각 t 의 +0분 피처를 만든다. 채택되지 않으면 vals 는 nil 이다.
//
// **이 함수가 학습과 서빙의 유일한 접점이다.** cmd/train 이 만드는 모델은
// Build 가 채택한 표본으로 학습되는데, 그 Build 가 이 함수를 부른다. 서빙도
// 이 함수를 부른다 — 규칙이 갈릴 자리가 없다.
func Features(b1, b5 bars.Bars, t int64) ([]float64, Reason) {
	v, err := clock.New(t, b1, b5, t)
	if err != nil {
		return nil, Warmup
	}
	if v.Bars1m.Len() < Req1m || v.Bars5m.Len() < Req5m {
		return nil, Warmup
	}
	ot1, ot5 := v.Bars1m.OpenTime, v.Bars5m.OpenTime
	l1, l5 := len(ot1), len(ot5)
	if ot1[l1-1] != t-minMS ||
		ot1[l1-1]-ot1[l1-Req1m] != int64(Req1m-1)*minMS ||
		ot5[l5-1]-ot5[l5-Req5m] != int64(Req5m-1)*fiveMS {
		return nil, Gap
	}
	vals, ok := features.Build(v)
	if !ok {
		return nil, Warmup
	}
	return vals, Eligible
}
```

- [ ] **Step 4: `Build` 가 이 함수를 쓰게 바꾼다**

`sample.go` 의 루프 본문에서 `clock.New` ~ `features.Build` 구간을 통째로 `Features(b1, b5, t)` 호출로 바꾸고, `Reason` 에 따라 `c.Warmup++` / `c.Gap++` 를 센다. 도지 검사는 루프에 그대로 남긴다(라벨 규칙이라 서빙에 없다).

- [ ] **Step 5: 테스트가 통과하는지 확인한다**

`GOTOOLCHAIN=local go test ./internal/sample/`

- [ ] **Step 6: G1′ 회귀를 확인한다 — 이 태스크의 진짜 관문**

`Build` 를 리팩터링했으므로 표본 집합이 변하지 않았음을 증명해야 한다.

```
GOTOOLCHAIN=local go run ./cmd/backtest 2>&1 | tail -20
```

기대: **888,525 표본 / 52.772% / 421 flips / 104 refits** — P0~P3 이 고정한 값. 하나라도 다르면 리팩터링이 의미를 바꾼 것이다. 다르면 되돌리고 원인을 찾는다.

- [ ] **Step 7: 커밋**

```bash
git add internal/sample/serve.go internal/sample/sample.go internal/sample/serve_test.go
git commit -m "sample: 학습과 서빙이 같은 자격 검사를 쓰게 한다"
```

---

### Task 2: `internal/quote` — 목표가와 행동 결정 (순수 함수)

**Files:**
- Create: `internal/quote/quote.go`, `internal/quote/quote_test.go`

**Interfaces:**
- Consumes: `ws.Book` 의 `BestBid`/`BestAsk` 결과(틱과 존재여부)만. 패키지를 임포트하지 않는다 — 값으로 받는다.
- Produces:

```go
type Book struct {
	BestBid    int64 // 우리 주문 제외
	HasBid     bool
	BestAsk    int64
	HasAsk     bool
	Precision  int
}

type Open struct {
	Tick   int64
	Placed time.Time // 이 주문을 낸 시각
	Live   bool      // 걸린 주문이 있는가
}

type Action int
const (
	DoNothing Action = iota // 조건 미달 또는 같은 가격
	Place                   // 신규 주문
	Reprice                 // 취소 후 재주문
	CancelOnly              // 주문 불가 상태인데 걸린 것이 있다
)

type Decision struct {
	Action Action
	Tick   int64
	Why    string // 로그용. 판단 근거를 사람이 읽을 수 있게.
}

func Ceiling(precision int) int64
func Target(b Book) (tick int64, ok bool)
func Decide(b Book, open Open, now time.Time, cooldown time.Duration, stale bool) Decision
```

- [ ] **Step 1: 실패하는 테스트를 쓴다**

기대값은 **전부 손으로 계산한다.** 구현을 옮겨 적으면 같은 실수를 같이 한다.

```go
package quote

import (
	"testing"
	"time"
)

func TestCeilingFromPrecision(t *testing.T) {
	// 0.5 미만의 최대 틱. 정밀도 2면 0.49 = 49틱, 3이면 0.499 = 499틱.
	cases := []struct{ prec int; want int64 }{
		{2, 49}, {3, 499}, {1, 4},
	}
	for _, c := range cases {
		if got := Ceiling(c.prec); got != c.want {
			t.Errorf("Ceiling(%d) = %d, 기대 %d", c.prec, got, c.want)
		}
	}
}

func TestTargetFollowsCrowdBelowHalf(t *testing.T) {
	// 남의 최우선 매수호가가 0.47 이면 거기에 붙는다.
	tick, ok := Target(Book{BestBid: 47, HasBid: true, Precision: 2})
	if !ok || tick != 47 {
		t.Errorf("target = %d,%v, 기대 47,true", tick, ok)
	}
}

func TestTargetCapsAtCeilingWhenCrowdIsAtOrAboveHalf(t *testing.T) {
	// 남의 매수호가가 0.50 이상이면 우리는 못 따라간다 — ceiling 으로 간다.
	for _, bid := range []int64{50, 60, 99} {
		tick, ok := Target(Book{BestBid: bid, HasBid: true, Precision: 2})
		if !ok || tick != 49 {
			t.Errorf("bid=%d → target = %d,%v, 기대 49,true", bid, tick, ok)
		}
	}
}

func TestTargetUsesCeilingWhenNoBid(t *testing.T) {
	tick, ok := Target(Book{HasBid: false, Precision: 2})
	if !ok || tick != 49 {
		t.Errorf("target = %d,%v, 기대 49,true", tick, ok)
	}
}

// 관통 방지 — 이것이 이 패키지의 존재 이유다.
func TestTargetNeverCrossesAsk(t *testing.T) {
	// 매도호가가 0.45 인데 ceiling 이 0.49 다. 그대로 가면 테이커가 된다.
	tick, ok := Target(Book{HasBid: false, BestAsk: 45, HasAsk: true, Precision: 2})
	if !ok || tick != 44 {
		t.Errorf("target = %d,%v, 기대 44,true — ask 아래 한 틱", tick, ok)
	}
	// 군중 매수호가가 매도호가와 같은 자리인 경우
	tick, ok = Target(Book{BestBid: 47, HasBid: true, BestAsk: 47, HasAsk: true, Precision: 2})
	if !ok || tick != 46 {
		t.Errorf("target = %d,%v, 기대 46,true", tick, ok)
	}
}

func TestTargetRefusesWhenAskIsAtBottom(t *testing.T) {
	// ask 가 1틱이면 그 아래는 0 — 유효한 가격이 없다.
	if _, ok := Target(Book{BestAsk: 1, HasAsk: true, Precision: 2}); ok {
		t.Error("ask=1 에서 주문 가능으로 판정했다")
	}
}

func TestDecideSamePriceDoesNothing(t *testing.T) {
	b := Book{BestBid: 47, HasBid: true, Precision: 2}
	now := time.Unix(1000, 0)
	open := Open{Tick: 47, Live: true, Placed: now.Add(-time.Hour)}
	d := Decide(b, open, now, 500*time.Millisecond, false)
	if d.Action != DoNothing {
		t.Errorf("action = %v, 기대 DoNothing — 같은 가격에 재주문하면 큐 맨 뒤로 밀린다", d.Action)
	}
}

func TestDecideCooldownDefersReprice(t *testing.T) {
	b := Book{BestBid: 46, HasBid: true, Precision: 2}
	now := time.Unix(1000, 0)
	// 400ms 전에 냈다 — 쿨다운 500ms 미만이므로 미룬다.
	open := Open{Tick: 47, Live: true, Placed: now.Add(-400 * time.Millisecond)}
	if d := Decide(b, open, now, 500*time.Millisecond, false); d.Action != DoNothing {
		t.Errorf("action = %v, 기대 DoNothing (쿨다운)", d.Action)
	}
	// 정확히 500ms 는 허용한다(경계 포함).
	open.Placed = now.Add(-500 * time.Millisecond)
	if d := Decide(b, open, now, 500*time.Millisecond, false); d.Action != Reprice {
		t.Errorf("action = %v, 기대 Reprice (경계 500ms 는 허용)", d.Action)
	}
}

func TestDecideStaleCancelsAndBlocksNewOrders(t *testing.T) {
	b := Book{BestBid: 47, HasBid: true, Precision: 2}
	now := time.Unix(1000, 0)
	// 걸린 주문이 있으면 취소한다.
	d := Decide(b, Open{Tick: 47, Live: true, Placed: now.Add(-time.Hour)}, now, 0, true)
	if d.Action != CancelOnly {
		t.Errorf("stale 인데 action = %v, 기대 CancelOnly", d.Action)
	}
	// 없으면 아무것도 하지 않는다 — 오래된 호가로 새 주문을 내면 안 된다.
	d = Decide(b, Open{}, now, 0, true)
	if d.Action != DoNothing {
		t.Errorf("stale + 무주문에서 action = %v, 기대 DoNothing", d.Action)
	}
}

func TestDecidePlacesWhenNoOpenOrder(t *testing.T) {
	b := Book{BestBid: 47, HasBid: true, Precision: 2}
	d := Decide(b, Open{}, time.Unix(1000, 0), 500*time.Millisecond, false)
	if d.Action != Place || d.Tick != 47 {
		t.Errorf("action=%v tick=%d, 기대 Place 47", d.Action, d.Tick)
	}
}
```

- [ ] **Step 2: 실패를 확인한다** — 컴파일 실패.

- [ ] **Step 3: 구현한다**

`Ceiling(precision)` = `10^precision / 2 − 1`. 정밀도 가드는 `order.NewTick` 과 같은 규약(1..18, 패닉).

`Target` 순서: (1) `HasBid && BestBid < half` 면 `BestBid`, 아니면 `Ceiling`. (2) `HasAsk && tick >= BestAsk` 면 `tick = BestAsk − 1`. (3) `tick <= 0` 이면 `ok=false`.

`Decide` 순서: (1) `stale` 이면 `Live ? CancelOnly : DoNothing`. (2) `Target` 이 `!ok` 면 `Live ? CancelOnly : DoNothing`. (3) `!Live` 면 `Place`. (4) `open.Tick == target` 이면 `DoNothing`. (5) `now.Sub(open.Placed) < cooldown` 이면 `DoNothing`. (6) `Reprice`.

`Why` 에는 판단 근거를 한 줄로 채운다(예: `"군중 47 추종"`, `"ask 45 관통 방지 → 44"`, `"동일가 47 유지"`).

- [ ] **Step 4: 통과 확인** — `go test ./internal/quote/ -v`

- [ ] **Step 5: 변이 시험 — 컴파일되는 것만 유효하다**

각 변이마다 백업 → 변이 → `go test` → 복원 → `diff -q` 확인:

| 변이 | 죽여야 할 테스트 |
|---|---|
| 관통 방지 `>=` → `>` | `TestTargetNeverCrossesAsk` (bid==ask 케이스) |
| `BestAsk − 1` → `BestAsk` | `TestTargetNeverCrossesAsk` |
| 같은 가격 검사 삭제 | `TestDecideSamePriceDoesNothing` |
| 쿨다운 `<` → `<=` | `TestDecideCooldownDefersReprice` (경계) |
| `Ceiling` 의 `−1` 삭제 | `TestCeilingFromPrecision` |

- [ ] **Step 6: 커밋**

---

### Task 3: `internal/risk` — 사이저·노출·일손실 (순수 함수)

**Files:** Create `internal/risk/risk.go`, `internal/risk/risk_test.go`

**Interfaces:**

```go
type Equity struct {
	AvailableUSDT float64 // 가용 잔고
	PositionCost  float64 // 미정산 포지션 취득원가 합
}
func (e Equity) Total() float64

type Exposure struct {
	FilledNotional  float64 // 체결 누적 명목
	OpenNotional    float64 // 미체결 주문 명목
	PendingCancel   float64 // 취소 확인 전 주문 명목
}
func (x Exposure) Total() float64

const CapFraction = 0.0455
const MinOrderUSD = 1.0

func Cap(e Equity) float64                        // e.Total() * CapFraction
func Remaining(e Equity, x Exposure) float64      // Cap − Exposure.Total()
func CanArm(e Equity) bool                        // Cap(e) > MinOrderUSD  (equity > $21.98…)
func Shares(remaining, priceUSD float64) float64  // 내림. 한도 초과 금지.
```

- [ ] **Step 1: 실패하는 테스트 — 기대값은 손으로 계산한다**

```go
// 강한 부등호다. 정확히 cap 인 주문은 허용하지 않는다.
func TestRemainingIsStrictlyBelowCap(t *testing.T) {
	e := Equity{AvailableUSDT: 1000}
	// cap = 1000 * 0.0455 = 45.5
	if got := Cap(e); got != 45.5 {
		t.Fatalf("cap = %v, 기대 45.5", got)
	}
	x := Exposure{FilledNotional: 45.5}
	if got := Remaining(e, x); got != 0 {
		t.Errorf("remaining = %v, 기대 0", got)
	}
}

// 취소 확인 전 주문도 살아있는 것으로 센다. 그러지 않으면 취소·재주문
// 경합 순간에 노출이 두 배가 된다.
func TestPendingCancelCountsAsExposure(t *testing.T) {
	e := Equity{AvailableUSDT: 1000}          // cap 45.5
	x := Exposure{OpenNotional: 20, PendingCancel: 20}
	if got := Remaining(e, x); got != 5.5 {
		t.Errorf("remaining = %v, 기대 5.5 — 취소 미확인분을 빼먹었다", got)
	}
}

// equity 가 $22 아래면 cap 이 $1 미만이라 유효한 주문을 낼 수 없다.
func TestCanArmRequiresEquityAboveTwentyTwo(t *testing.T) {
	// 22 * 0.0455 = 1.001 > 1  → 무장 가능
	if !CanArm(Equity{AvailableUSDT: 22}) {
		t.Error("equity 22 에서 무장 불가로 판정했다 (cap 1.001 > 1)")
	}
	// 21 * 0.0455 = 0.9555 < 1 → 불가
	if CanArm(Equity{AvailableUSDT: 21}) {
		t.Error("equity 21 에서 무장 가능으로 판정했다 (cap 0.9555 < 1)")
	}
}

// 미정산 포지션은 취득원가로 센다(시가 아님).
func TestPositionCostCountsTowardEquity(t *testing.T) {
	e := Equity{AvailableUSDT: 100, PositionCost: 50}
	if got := Cap(e); got != 150*0.0455 {
		t.Errorf("cap = %v", got)
	}
}

// 주식 수는 내림한다 — 올리면 한도를 넘는다.
//
// **정정(2026-08-10)**: 이 절의 초안은 둘째 케이스를 20 으로 적었다. 틀렸다.
// 20 × 0.49 는 float64 에서 9.8 과 **정확히 같아서** 명목이 잔여를 다 쓰고,
// 그러면 노출이 cap 과 같아진다 — 사용자가 두 번 명시한 `< equity × 0.0455`
// (강한 부등호)를 위반한다. 구현자가 이 충돌을 짚어 계획서 쪽을 고쳤다.
// `Shares` 는 `n × price < remaining` 을 보장해야 한다.
func TestSharesRoundsDown(t *testing.T) {
	// 잔여 10 USD, 가격 0.49 → 20.408… 주 → 20 주 (20×0.49 = 9.8 < 10)
	if got := Shares(10, 0.49); got != 20 {
		t.Errorf("shares = %v, 기대 20", got)
	}
	// 나누어떨어져도 잔여를 다 쓰지 않는다 — 다 쓰면 명목 == cap 이 된다.
	if got := Shares(9.8, 0.49); got != 19 {
		t.Errorf("shares = %v, 기대 19 — 20 이면 명목이 잔여와 같아진다", got)
	}
}

// 잔여를 정확히 소진하는 입력에서 명목이 잔여보다 **작아야** 한다.
func TestSharesNeverExhaustsRemaining(t *testing.T) {
	// equity 1000 → cap 45.5. 가격 0.35 → 130 주면 명목이 정확히 45.5 = cap.
	const remaining, price = 45.5, 0.35
	n := Shares(remaining, price)
	if n*price >= remaining {
		t.Errorf("명목 %v 가 잔여 %v 이상이다 — 강한 부등호 위반", n*price, remaining)
	}
}

func TestSharesRefusesBelowMinimum(t *testing.T) {
	if got := Shares(0.99, 0.49); got != 0 {
		t.Errorf("shares = %v, 기대 0 — 최소 주문 $1 미만", got)
	}
}
```

- [ ] **Step 2~4:** 실패 확인 → 구현 → 통과 확인.

`Shares` 는 `remaining < MinOrderUSD` 면 0. 아니면 `math.Floor(remaining / priceUSD)`, 다시 `shares*price < MinOrderUSD` 면 0. 가격이 0 이하면 0.

- [ ] **Step 5: 일손실 한도**

```go
type DailyLimit struct {
	StartEquity float64 // UTC 자정 시점
	Fraction    float64 // 기본 0.10
}
func (d DailyLimit) Breached(realizedPnL float64) bool  // realizedPnL <= -StartEquity*Fraction
```

경계 테스트: 정확히 −10% 는 **차단**(`<=`).

- [ ] **Step 6: 변이 시험** — `CapFraction` 값, `>` vs `>=`, `Floor` → `Ceil`, `PendingCancel` 누락.

- [ ] **Step 7: 커밋**

---

### Task 4: `internal/ledger` — CSV 원장과 부호

**Files:** Create `internal/ledger/ledger.go`, `internal/ledger/ledger_test.go`

**왜 부호에 테스트를 붙이는가**: `pmmm-go` 에서 손익 부호를 뒤집어 +40 이 −90 이 된 전례가 있다. 그 회귀를 고정한다.

**Interfaces:**

```go
type Fill struct {
	RoundStart  int64
	MarketID    int64
	Outcome     string  // "Up" | "Down"
	Shares      float64
	PriceUSD    float64
	FeeUSD      float64 // 우리가 낸 수수료. 양수면 지출.
	At          time.Time
}
type Rebate struct {
	RoundStart int64
	Shares     float64 // 반대편 주식 수
	At         time.Time
}
type Settlement struct {
	RoundStart int64
	Won        bool
	Shares     float64
	At         time.Time
}
func Open(path string) (*Ledger, error)   // 기존 파일에 append, 헤더는 새 파일에만
func (l *Ledger) RecordFill(f Fill) error
func (l *Ledger) RecordRebate(r Rebate) error
func (l *Ledger) RecordSettlement(s Settlement) error
func (l *Ledger) Close() error

// PnL 계산 — 부호 규약을 한 곳에 못박는다.
func FillCost(f Fill) float64        // 지출: +shares*price + fee  (양수 = 나간 돈)
func SettlementProceeds(s Settlement) float64 // 수입: won ? shares*1.0 : 0
func RebateValue(r Rebate, settledUp bool) float64 // 반대편 주식이므로 우리가 졌을 때 1.0, 이겼을 때 0
```

- [ ] **Step 1: 부호 테스트를 먼저 쓴다**

```go
// 매수는 돈이 나간다. 부호가 뒤집히면 손익이 통째로 뒤집힌다.
func TestFillCostIsPositiveOutflow(t *testing.T) {
	f := Fill{Shares: 100, PriceUSD: 0.49, FeeUSD: 0.2}
	if got := FillCost(f); got != 49.2 {
		t.Errorf("FillCost = %v, 기대 49.2 (49 + 0.2)", got)
	}
}

// 이기면 주당 1.0 이 들어온다. 지면 0.
func TestSettlementProceeds(t *testing.T) {
	if got := SettlementProceeds(Settlement{Won: true, Shares: 100}); got != 100 {
		t.Errorf("이겼는데 %v", got)
	}
	if got := SettlementProceeds(Settlement{Won: false, Shares: 100}); got != 0 {
		t.Errorf("졌는데 %v", got)
	}
}

// **리베이트는 반대편 주식이다** — 우리가 이기면 그 주식은 0 이 되고,
// 우리가 져야 1.0 이 된다. 이 방향을 뒤집으면 엣지 계산이 무너진다
// (G2 가 이 규칙으로 +0.487%p 를 계산했다).
func TestRebateOnlyPaysWhenWeLose(t *testing.T) {
	r := Rebate{Shares: 5}
	if got := RebateValue(r, true); got != 0 {
		t.Errorf("우리가 이겼는데 리베이트 %v, 기대 0 — 반대편 주식은 0 이 된다", got)
	}
	if got := RebateValue(r, false); got != 5 {
		t.Errorf("우리가 졌는데 리베이트 %v, 기대 5", got)
	}
}
```

- [ ] **Step 2~4:** 실패 확인 → 구현 → 통과.

CSV 는 원자적으로 쓴다 — 각 레코드마다 `Flush()` 하고 파일은 `O_APPEND`. 크래시 복구가 이 파일을 읽으므로 반쯤 쓰인 줄이 남으면 안 된다.

- [ ] **Step 5: 변이** — 부호 셋을 각각 뒤집어 해당 테스트가 죽는지.

- [ ] **Step 6: 커밋**

---

### Task 5: `internal/predictfun/rest` — 주문 엔드포인트와 Bearer 인증

**Files:**
- Create: `internal/predictfun/rest/orders.go`, `internal/predictfun/rest/orders_test.go`
- Modify: `internal/predictfun/rest/client.go` (Bearer 토큰 주입)

**Interfaces:**

```go
// TokenSource 는 요청마다 Bearer 토큰을 준다. auth.Authenticator 가 이걸 만족한다.
type TokenSource interface{ Token(ctx context.Context) (string, error) }
func (c *Client) SetTokenSource(ts TokenSource)

type CreateOrderResult struct {
	Code               string
	OrderID            string
	OrderHash          string
	RemovalLockedUntil time.Time // 이 시각 전에는 취소가 거부된다
}
func (c *Client) CreateOrder(ctx context.Context, body any) (CreateOrderResult, error)

type RemoveResult struct {
	Removed  []string
	Noop     []string
	Rejected []string // 취소 잠금 창 안이라 거부됨 — 재시도 대상
}
func (c *Client) RemoveOrders(ctx context.Context, ids []string) (RemoveResult, error)

func (c *Client) Positions(ctx context.Context) ([]Position, error)
func (c *Client) ReservedUSDT(ctx context.Context) (float64, error)
```

- [ ] **Step 1: httptest 서버로 실패 테스트를 쓴다**

핵심 케이스:
- `CreateOrder` 가 `{"data":{...},"success":true}` 봉투를 벗기고 `removalLockedUntil` 을 파싱한다
- `RemoveOrders` 가 `rejected` 를 별도로 돌려준다(빈 배열과 구분)
- **비200 응답의 본문이 에러에 실린다** (P4 최종 리뷰 Minor 7 — 거래소가 왜 거부했는지가 사라지면 안 된다)
- **`Post` 가 201 Created 도 성공으로 본다** (P4 Minor 8)
- **에러 메시지에 API 키나 토큰이 절대 없다** — 응답 본문을 싣게 되므로 요청 헤더는 싣지 않는다는 것을 테스트로 고정한다

```go
func TestErrorIncludesBodyButNeverSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"success":false,"error":"invalid_signature","message":"bad sig"}`))
	}))
	defer srv.Close()
	c := New("SECRET-API-KEY-DO-NOT-LEAK")
	c.BaseURL = srv.URL
	_, err := c.CreateOrder(context.Background(), map[string]any{})
	if err == nil {
		t.Fatal("422 인데 에러가 없다")
	}
	if !strings.Contains(err.Error(), "invalid_signature") {
		t.Errorf("거부 사유가 에러에 없다: %v", err)
	}
	if strings.Contains(err.Error(), "SECRET-API-KEY-DO-NOT-LEAK") {
		t.Fatal("에러 메시지에 API 키가 샜다")
	}
}
```

- [ ] **Step 2~4:** 실패 → 구현 → 통과.

- [ ] **Step 5: 변이** — 봉투 벗기기 제거, `rejected` 무시, 본문 미포함, 201 거부.

- [ ] **Step 6: 커밋**

---

### Task 6: `internal/live` — 회차 선택·equity·예측 (미확정 가정의 격리 지점)

**Files:** Create `internal/live/rounds.go`, `internal/live/equity.go`, `internal/live/predict.go` + 테스트

**Interfaces:**

```go
// rounds.go — cmd/probe 의 fetchLiveRounds/roundIsLive 를 승격한다.
// probe 의 것은 진단용이었고 findings 가 "P5 는 새로 써야 한다"고 적었다.
// 실제로는 규칙이 같으므로 여기로 옮기고 probe 가 이것을 쓰게 한다 —
// 두 곳에 두면 갈린다.
type Round struct {
	CategoryID  int64
	MarketID    int64
	Slug        string
	StartsAt    time.Time
	EndsAt      time.Time
	Precision   int
	UpTokenID   string
	DownTokenID string
}
func FetchLive(ctx context.Context, c *rest.Client, symbol string, now time.Time, lookahead time.Duration) ([]Round, error)
func ParseRoundStart(slug string) (int64, bool) // btc-updown-5m-<T> 에서 T. G2 오염 사례의 그 함수 — 접두사를 엄격히 검사한다.

// equity.go — **미확정 가정이 사는 유일한 파일**
type EquitySource struct {
	Rest      *rest.Client
	RPC       string
	Account   string
	USDTToken string
	IncludePositions bool
}
func (s *EquitySource) Read(ctx context.Context) (risk.Equity, error)

// predict.go
type Predictor struct {
	Model *model.LogReg
}
// Freeze 는 회차 시작 시각 T 의 p_up 을 구해 동결한다.
// **회차 중 다시 부르는 코드 경로가 존재하면 안 된다** — exec 는 이 값을
// 구조체에 담아 들고 다니고 Predictor 를 참조하지 않는다.
func (p *Predictor) Freeze(ctx context.Context, t int64) (Frozen, error)
type Frozen struct {
	T          int64
	PUp        float64
	Confidence float64 // 2*|p_up-0.5|
	Direction  string  // "Up" | "Down"
	Eligible   bool    // confidence >= 0.0172
}
```

`Freeze` 는 Binance REST 로 1분봉 260개·5분봉 200개를 받아 `sample.Features(b1,b5,t)` 를 부른다. **진행 중인 봉은 `clock.New` 가 이미 잘라내지만**, 받아온 마지막 봉이 미마감일 수 있으므로 `openTime + 간격 <= t` 인 것만 버퍼에 넣는다.

- [ ] **Step 1: `ParseRoundStart` 부터 — G2 오염을 낸 그 함수**

```go
func TestParseRoundStartRejectsOtherProducts(t *testing.T) {
	// 15분 상품이 섞여 들어와 측정 두 번을 오염시킨 전례가 있다.
	for _, bad := range []string{
		"btc-updown-15m-1786275000",
		"eth-updown-5m-1786275000",
		"btc-updown-5m-",
		"btc-updown-5m-abc",
		"", "btc",
	} {
		if _, ok := ParseRoundStart(bad); ok {
			t.Errorf("%q 를 받아들였다", bad)
		}
	}
	got, ok := ParseRoundStart("btc-updown-5m-1786275000")
	if !ok || got != 1786275000 {
		t.Errorf("= %d,%v", got, ok)
	}
}

// 회차 시작은 5분 경계여야 한다. 아니면 모델의 봉 시작과 어긋난다.
func TestParseRoundStartRequiresFiveMinuteBoundary(t *testing.T) {
	if _, ok := ParseRoundStart("btc-updown-5m-1786275001"); ok {
		t.Error("5분 경계가 아닌 시각을 받아들였다")
	}
}
```

- [ ] **Step 2: `Freeze` 의 동결 테스트**

```go
// p_up 은 한 번 계산하면 바뀌지 않는다. 같은 t 로 두 번 불러도 같은 값이고,
// exec 가 Frozen 을 들고 다니는 동안 Predictor 를 다시 부를 방법이 없다.
func TestFrozenIsAValueNotAReference(t *testing.T) {
	// Frozen 에 포인터나 함수 필드가 없음을 리플렉션으로 고정한다 —
	// 나중에 누가 "다시 계산" 경로를 추가하면 여기서 걸린다.
	typ := reflect.TypeOf(Frozen{})
	for i := 0; i < typ.NumField(); i++ {
		switch typ.Field(i).Type.Kind() {
		case reflect.Ptr, reflect.Func, reflect.Interface, reflect.Chan, reflect.Map, reflect.Slice:
			t.Errorf("Frozen.%s 가 %v 다 — 값 타입만 허용한다", typ.Field(i).Name, typ.Field(i).Type.Kind())
		}
	}
}

func TestEligibleThreshold(t *testing.T) {
	// confidence = 2*|p-0.5|. 문턱 0.0172.
	// p=0.5086 → confidence = 0.0172 정확히 → 통과(>=)
	// p=0.5085 → 0.017 → 미달
	...
}
```

- [ ] **Step 3~5:** 구현 → 통과 → 변이(문턱 `>=`→`>`, 방향 뒤집기, 봉 미마감 필터 제거).

- [ ] **Step 6: 커밋**

---

### Task 7: `internal/exec` — 마켓메이킹 루프

**Files:** Create `internal/exec/exec.go`, `internal/exec/exec_test.go`

**이 태스크의 설계 원칙**: `exec` 는 **결정을 내리지 않는다.** `quote.Decide` 와 `risk` 가 결정하고 `exec` 는 그것을 오더북·주문·시계에 연결한다. 그래야 모의 오더북으로 전체 루프를 테스트할 수 있다.

**Interfaces:**

```go
type Orders interface {
	Create(ctx context.Context, r Request) (id string, lockedUntil time.Time, err error)
	Remove(ctx context.Context, ids []string) (rejected []string, err error)
}
type Runner struct {
	Book     *ws.Book
	Orders   Orders
	Ledger   *ledger.Ledger
	Cooldown time.Duration
	StaleAfter time.Duration
	Clock    func() time.Time   // 테스트가 시계를 쥔다
}
func (r *Runner) RunRound(ctx context.Context, rd live.Round, f live.Frozen, e risk.Equity) error
```

- [ ] **Step 1: 모의 오더북으로 세 가지를 고정하는 테스트**

스펙 §8 이 `exec` 에 요구하는 셋이다 — **관통 방지·동일가 무동작·노출 상한.**

```go
// 군중이 한 틱 내려가면 따라 내려가되, 같은 가격에서는 아무 요청도 나가지 않는다.
func TestLoopDoesNotReorderAtSamePrice(t *testing.T) { ... }

// 노출이 cap 에 닿으면 더 주문하지 않는다. 취소 미확인분도 센다.
func TestLoopStopsAtCap(t *testing.T) { ... }

// removalLockedUntil 안에서는 취소를 시도하지 않는다.
func TestLoopRespectsRemovalLock(t *testing.T) { ... }

// stale 이면 신규를 멈추고 기존을 취소한다.
func TestLoopCancelsOnStale(t *testing.T) { ... }

// 회차가 끝나면 미체결 전량 취소.
func TestLoopCancelsAllAtRoundEnd(t *testing.T) { ... }
```

- [ ] **Step 2~4:** 구현 → 통과.

- [ ] **Step 5: 변이** — 각 테스트에 대응하는 조건을 하나씩 무력화.

- [ ] **Step 6: 커밋**

---

### Task 8: `cmd/gld91` — 배선과 무장 게이트

**Files:** Create `cmd/gld91/main.go`, `cmd/gld91/config.go`, `cmd/gld91/main_test.go`

- [ ] **Step 1: 무장 게이트 테스트**

```go
// LIVE_ARM 이 정확히 그 값일 때만 전송한다. 오타·빈 값·"true" 는 전부 DRY-RUN.
func TestArmedOnlyWithExactValue(t *testing.T) {
	for _, v := range []string{"", "true", "1", "yes", "I_UNDERSTAND", "i_understand_the_risk"} {
		if Armed(v) {
			t.Errorf("%q 로 무장됐다", v)
		}
	}
	if !Armed("I_UNDERSTAND_THE_RISK") {
		t.Error("정확한 값인데 무장되지 않았다")
	}
}

// DRY-RUN 에서도 서명은 한다 — 서명 경로가 실거래와 달라지면
// DRY-RUN 이 아무것도 증명하지 못한다.
func TestDryRunStillSigns(t *testing.T) { ... }
```

- [ ] **Step 2: 기동 시 자가 점검**

`main` 은 순서대로 확인하고 하나라도 실패하면 **무장하지 않고 종료**한다:

1. `PREDICT_ACCOUNT` / `WALLET_PRIVATE_KEY` / `PREDICT_API_KEY` 존재
2. **`signercheck` 와 같은 대조** — 키의 EOA 가 계정의 등록 서명자인가. 아니면 즉시 종료. (P4 가 이 대조 없이는 모든 주문이 거부됨을 실측했다.)
3. 모델 로드 + `FeatureNames` 대조
4. `risk.CanArm(equity)` — equity 가 $22 미만이면 그 사실을 로그에 남기고 무장하지 않는다
5. 원장 파일과 열린 주문 대조(크래시 복구) — 불일치면 멈춘다

- [ ] **Step 3: DRY-RUN 실행**

```
GOTOOLCHAIN=local go run ./cmd/gld91 -dry-run-minutes 10
```

기대: 회차를 잡고, `p_up` 을 동결하고, 목표가를 계산하고, 주문을 **서명까지 하고 전송하지 않고** 로그에 남긴다.

- [ ] **Step 4: 커밋**

---

## 이 계획이 의도적으로 하지 않는 것

- **실주문.** Step 4~5(승인·실주문)는 자금이 들어온 뒤 별도로 한다. 이 계획의 산출물은 DRY-RUN 까지다.
- **켈리 사이징.** 항상 고정 4.55% 미만(사용자 결정).
- **매도 주문.** 진입은 매수만, 청산은 정산으로만(스펙 §10).
- **`shareThreshold` 반영.** 미확정 가설이라 사이징에 넣지 않는다.
- **배포(P6).** AWS 도쿄는 DRY-RUN 24시간 뒤.
