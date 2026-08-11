# P6 — 하트비트·모니터링 봇 구현 계획

> **에이전트에게:** 이 계획은 `superpowers:subagent-driven-development` 로 태스크 단위 실행한다. 단계는 체크박스(`- [ ]`)다.

**목표:** 봇(A호스트)이 3초마다 상태 스냅샷을 B호스트의 모니터로 밀어 보내고, 모니터가 죽음·정지·노출 위반·정산을 감시해 텔레그램으로 알리고 명령을 되돌려 준다.

**아키텍처:** 계약(`internal/beat`)은 순수 패키지 하나에 모으고 봇과 모니터가 **같은 타입을 임포트**한다. 판정(`internal/beat/rule`)도 순수 함수로 분리해 스냅샷 값만으로 시험한다 — `internal/risk`·`internal/quote` 가 결정을 순수 함수로 떼어낸 것과 같은 이유다. I/O 는 `cmd/gld91`(리포터)과 `cmd/gld91-monitor`(수신·알림)에만 있다.

**모니터가 같은 저장소인 이유:** 계약을 두 벌로 두면 갈린다(`internal/live/rounds.go` 의 원칙). 바이너리만 따로 배포하면 B호스트는 소스를 갖되 `WALLET_PRIVATE_KEY` 를 갖지 않는다.

**기술 스택:** Go 1.22, 표준 라이브러리만. 새 의존성 없음 — 텔레그램도 HTTP 도 `net/http` 로 충분하다.

## Global Constraints

설계서(`docs/superpowers/specs/2026-08-11-gld91-heartbeat-monitor-design.md`)와 기존 스펙에서 그대로 옮긴다.

- **모니터는 `WALLET_PRIVATE_KEY` 를 갖지 않는다.** 모니터 프로세스에서 개인키를 읽는 코드 경로가 존재하면 안 된다.
- **모니터 → 봇 채널에는 명령만 흐른다.** beat 응답 타입에 `Command`·`AckFor` 외의 필드를 두지 않는다. 소스 스캔 테스트로 고정한다.
- **모니터는 봇과 다른 API 키를 쓴다.** `MONITOR_API_KEY` 는 `PREDICT_API_KEY` 와 같으면 기동 거부. 레이트리밋 240 req/min 은 키 단위이고, 스펙 §"예산은 키 단위다"가 같은 키를 쓴 프로세스들이 14초 만에 240 을 소진시킨 사고를 기록했다.
- **Reporter 는 `exec` 루프를 블록하지 않는다.** 회차당 6,000 바퀴(기본 50ms)이고, 그 안에서 네트워크를 기다리면 재호가가 밀린다.
- **beat 실패는 거래를 멈추지 않는다.** 부수 기능의 실패가 주 기능을 죽이면 안 된다(`internal/ledger` 와 같은 원칙).
- **시크릿은 환경변수로만.** 소스·로그·에러·테스트·골든 어디에도 실값 금지. 저장소는 GitHub 에 올라간다.
- **판정 패키지는 순수하다.** 네트워크도, 시계도, 전역 상태도 없다. 현재 시각은 인자로 받는다.
- **망가진 입력에서의 방향:** 판정은 **알리는 쪽**으로 실패한다. 스냅샷이 깨져 있으면 "이상 없음"이 아니라 알람이다. 조용한 정상은 감시 장치의 실패 모드다.
- **`GOTOOLCHAIN=local`.** Makefile 이 설정한다. 직접 `go` 를 부를 때는 붙인다.

---

## 파일 구조

| 파일 | 책임 |
|---|---|
| `internal/beat/snapshot.go` | 스냅샷·명령 타입. 직렬화. 다른 것 없음 |
| `internal/beat/hmac.go` | 서명·검증·재전송 방지. 순수 |
| `internal/beat/rule/rule.go` | 판정 규칙 — 스냅샷 → 알람. 순수 |
| `internal/beat/rule/latch.go` | 에지 트리거·지속시간·래치 |
| `cmd/gld91/beat.go` | 스냅샷 조립 + Reporter 고루틴 (봇 쪽) |
| `cmd/gld91/orders.go` | **수정** — `Expiration` 을 회차 종료로 |
| `cmd/gld91-monitor/main.go` | 배선 |
| `cmd/gld91-monitor/server.go` | `POST /beat` 수신 |
| `cmd/gld91-monitor/telegram.go` | 알림·명령 롱폴 |
| `cmd/gld91-monitor/settle.go` | 정산 관측 (모니터 전용 키) |
| `cmd/gld91-monitor/report.go` | 4시간 리포트 조립 |

---

### Task 1: 주문 만료 서명

설계서 §2(가). **모니터보다 먼저 해야 한다** — 이것이 없으면 봇이 죽었을 때 미체결 주문이 체인에 유효한 채로 남고, 개인키 없는 모니터는 그것을 관측조차 할 수 없다.

`cmd/gld91/orders.go:186` 이 지금 `Expiration: big.NewInt(0)` 이고 주석이 *"회차 종료 시 전량 취소는 exec 가 보장하므로 만료에 기대지 않는다"* 고 적혀 있다. 그 보장은 **봇이 살아 있을 때만** 성립한다. `Runner.RunRound` 의 `cancelEverything` 은 프로세스가 돌아야 실행된다.

**Files:**
- Modify: `cmd/gld91/orders.go:180-190`
- Test: `cmd/gld91/orders_test.go`

**Interfaces:**
- Consumes: `exec.Request.Round` (`live.Round`, 필드 `EndsAt time.Time`), `order.Order.Expiration *big.Int`
- Produces: `const expirationGrace = 60 * time.Second`

- [ ] **Step 1: 만료가 회차 종료 기준으로 들어가는지 보는 실패 테스트**

```go
// 봇이 죽어도 주문이 영원히 살아 있으면 안 된다. exec 의 전량 취소는
// 프로세스가 돌아야 일어나는 일이므로, 만료는 그것과 독립인 두 번째 방어다.
func TestOrderExpiresAtRoundEnd(t *testing.T) {
	end := time.Date(2026, 8, 11, 4, 15, 0, 0, time.UTC)
	s := testSender(t)
	req := testRequest(t)
	req.Round.EndsAt = end

	body, _, err := s.build(req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got, ok := body["expiration"].(string)
	if !ok {
		t.Fatalf("expiration 이 문자열이 아니다: %T", body["expiration"])
	}
	want := fmt.Sprint(end.Add(expirationGrace).Unix())
	if got != want {
		t.Errorf("expiration = %s, want %s", got, want)
	}
}

// 만료 0 은 "영원히 유효" 다. 그 값이 다시 들어오는 것을 막는다.
func TestOrderNeverExpiresZero(t *testing.T) {
	s := testSender(t)
	body, _, err := s.build(testRequest(t))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if body["expiration"] == "0" {
		t.Error("expiration 이 0 이다 — 봇이 죽으면 주문이 체인에 남는다")
	}
}

// EndsAt 이 제로시각이면 주문을 만들지 않는다. 제로시각의 Unix()는 음수이고,
// 그것이 서명에 들어가면 거래소가 어떻게 읽을지 모른다.
func TestOrderRejectsZeroEndsAt(t *testing.T) {
	s := testSender(t)
	req := testRequest(t)
	req.Round.EndsAt = time.Time{}
	if _, _, err := s.build(req); err == nil {
		t.Error("EndsAt 이 제로인데 주문이 만들어졌다")
	}
}

// --- 헬퍼 ---
//
// cmd/gld91 에는 아직 테스트 파일이 없다. 이 둘이 첫 헬퍼이고 이후 태스크가
// 재사용한다. **실키를 쓰지 않는다** — 고정 시드로 만든 테스트 전용 키다.

func testSender(t *testing.T) *orderSender {
	t.Helper()
	// 고정 개인키. 테스트 전용이고 어떤 자금도 없다.
	key, err := crypto.HexToECDSA("1111111111111111111111111111111111111111111111111111111111111111")
	if err != nil {
		t.Fatalf("테스트 키: %v", err)
	}
	signer, err := auth.NewSigner(key)
	if err != nil {
		t.Fatalf("서명자: %v", err)
	}
	acct := common.HexToAddress("0x2222222222222222222222222222222222222222")
	return &orderSender{
		Signer:    signer,
		Account:   acct,
		Validator: common.HexToAddress("0x0000000000000000000000000000000000000001"),
		Domain:    order.Domain{ChainID: big.NewInt(81457), VerifyingContract: acct.Hex()},
		Armed:     false, // 서명만 하고 전송하지 않는다
		Salt:      func() (*big.Int, error) { return big.NewInt(12345), nil },
		Now:       func() time.Time { return time.Date(2026, 8, 11, 4, 10, 0, 0, time.UTC) },
	}
}

func testRequest(t *testing.T) exec.Request {
	t.Helper()
	start := time.Date(2026, 8, 11, 4, 10, 0, 0, time.UTC)
	return exec.Request{
		Round: live.Round{
			CategoryID: 1, MarketID: 2, Slug: "btc-updown-5m-1786277400",
			StartsAt: start, EndsAt: start.Add(5 * time.Minute),
			Precision: 2, FeeRateBps: 0,
			UpTokenID: "111", DownTokenID: "222",
		},
		Outcome: ledger.OutcomeUp,
		TokenID: "111",
		Tick:    order.Tick{V: 45, Precision: 2},
		Shares:  10,
	}
}
```

> `auth.NewSigner` / `order.Domain` / `order.Tick` 의 정확한 생성자 이름은
> `internal/predictfun/auth` 와 `internal/predictfun/order` 에서 확인하고 맞춘다.
> 컴파일이 첫 관문이다.

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91/ -run 'TestOrderExpires|TestOrderNever|TestOrderRejectsZero' -v`
Expected: FAIL — `expirationGrace` 미정의(컴파일 에러)

- [ ] **Step 3: 구현**

`orders.go` 의 상수 블록에 추가:

```go
// expirationGrace 는 회차 종료 뒤 주문이 유효한 시간이다.
//
// 0 이 아닌 이유: 회차 종료 시각은 거래소 메타데이터에서 오고 우리 시계와
// 정확히 같지 않다. 종료 직전에 낸 주문이 시계 차이로 서명 시점에 이미
// 만료돼 있으면 그 회차의 마지막 진입이 통째로 사라진다.
//
// 길지 않은 이유: 이 값이 곧 "봇이 죽은 뒤 주문이 체결될 수 있는 창"이다.
// exec 가 살아 있으면 회차 종료에 전량 취소하므로 이 창은 쓰이지 않는다.
const expirationGrace = 60 * time.Second
```

`build` 에서 `r.Round.FeeRateBps` 검사 바로 뒤에 추가:

```go
	if r.Round.EndsAt.IsZero() {
		return nil, nil, errors.New("회차 종료 시각이 없다 — 만료 없는 주문은 내지 않는다")
	}
```

`order.Order{...}` 리터럴의 `Expiration` 줄을 교체:

```go
		// 회차 종료 + 유예. **exec 의 전량 취소와 독립인 두 번째 방어다** —
		// 그 취소는 프로세스가 살아 있어야 일어나고, 봇이 죽으면 취소해 줄
		// 주체가 없다. 만료는 체인이 지키므로 우리가 죽어도 지켜진다.
		Expiration: big.NewInt(r.Round.EndsAt.Add(expirationGrace).Unix()),
```

- [ ] **Step 4: 통과 확인 + 전체 회귀**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91/ ./internal/... -race`
Expected: PASS. 기존 서명 골든(`internal/predictfun/order`)은 자기 벡터를 쓰므로 영향 없다.

- [x] **Step 5: 거래소가 0 이 아닌 만료를 받는지 확인** — 2026-08-11, 문서 근거까지만

**실주문 실측은 하지 못했다.** `armingBlockers()` 4건(서명 자리 미확정, Exchange 주소 미확정, USDT 승인·입금 없음, `Fills` 미구현)이 실주문을 막고 있고, 그 게이트를 우회해 주문을 내는 것은 정확히 그 게이트가 막으려는 행동이다. testnet 도 주문 생성에는 JWT 인증과 자금이 필요하다.

**대신 스펙에서 확정했다.** `GET https://api.predict.fun/spec` 의 `ContractOrder` 스키마:

```json
"expiration": { "type": "integer", "format": "int64",
                "description": "Unix timestamp in seconds" }
```

만료는 **문서화된 필드**이고 의미가 "초 단위 Unix 타임스탬프"로 명시돼 있다. `expiration=0` 은 SDK 골든 벡터가 쓰는 "만료 없음" 예시일 뿐 요구사항이 아니다.

같은 스키마에서 **두 번째 결함**이 드러났다. `salt`·`tokenId`·`makerAmount`·`takerAmount`·`nonce`·`feeRateBps` 는 전부 `type: string` 인데 **`expiration` 만 `type: integer`** 다. `build` 는 이 필드를 `o.Expiration.String()` 으로 보내고 있었다 — 스키마 위반이고, 값이 `0` 인 동안에는 관대한 파서가 삼켜서 드러나지 않는다. 만료를 채우는 것과 같이 고쳤다.

**남은 미확인 하나 — 조용한 쪽이다.** 거래소가 만료를 *받아들이되 강제하지 않으면*, 우리는 없는 방어를 있다고 믿게 된다. 거부는 400 으로 시끄럽게 드러나지만 무시는 드러나지 않는다. 무장 후 첫 주문에서 응답의 `order.expiration` 이 우리가 보낸 값 그대로인지 확인하고, 회차 종료 후 그 주문이 실제로 사라지는지 한 번 눈으로 본다 — **Task 8 의 실주문 1건(Step 4~5) 체크리스트에 넣는다.**

- [ ] **Step 6: 커밋**

```bash
git add cmd/gld91/orders.go cmd/gld91/orders_test.go
git commit -m "orders: 주문에 회차 종료 만료를 서명한다

exec 의 전량 취소는 프로세스가 살아 있어야 일어난다. 봇이 죽으면 취소할
주체가 없고, expiration=0 이면 그 주문은 체인에서 유효한 채로 남는다.
개인키 없는 모니터는 미체결을 관측할 수 없으므로(GET /v1/orders 는 JWT
필요) 이 층이 없으면 아무도 모르는 주문이 생긴다."
```

---

### Task 2: `internal/beat` — 스냅샷 계약

**Files:**
- Create: `internal/beat/snapshot.go`, `internal/beat/snapshot_test.go`

**Interfaces:**
- Produces: `beat.Snapshot`, `beat.Consts`, `beat.Equity`, `beat.Round`, `beat.Exposure`, `beat.OpenOrder`, `beat.Loop`, `beat.Reply`, `beat.Command`, `beat.SkipReason` 과 상수들

- [ ] **Step 1: 응답 타입이 명령만 싣는지 보는 실패 테스트**

```go
// 설계서 §9 의 불변식: 모니터 → 봇 채널에 데이터는 흐르지 않는다.
//
// 모니터는 정산 결과를 알지만 봇은 몰라야 한다(exec 의
// TestExecNeverWritesSettlement 가 지키는 벽). 그 값이 beat 응답에 실리면
// 벽에 뒷문이 생기고, 뒷문은 원래의 벽보다 찾기 어렵다. 그래서 필드 자체를
// 금지하고 리플렉션으로 고정한다.
func TestReplyCarriesOnlyCommands(t *testing.T) {
	allowed := map[string]bool{"Command": true, "AckFor": true}
	rt := reflect.TypeOf(Reply{})
	for i := 0; i < rt.NumField(); i++ {
		if name := rt.Field(i).Name; !allowed[name] {
			t.Errorf("Reply 에 %q 가 있다 — 응답은 명령만 싣는다", name)
		}
	}
}

func TestCommandsAreClosedSet(t *testing.T) {
	for _, c := range []Command{CmdNone, CmdShutdown, CmdHalt} {
		if !c.Valid() {
			t.Errorf("%q 가 유효하지 않다", c)
		}
	}
	for _, c := range []Command{"", "restart", "SHUTDOWN", "none "} {
		if c.Valid() {
			t.Errorf("%q 가 유효로 통과했다", c)
		}
	}
}

// 스킵 사유는 닫힌 집합이다. 새 사유가 문자열로 슬며시 들어오면 모니터의
// 분기가 그것을 conf_below 와 같이 취급해 조용해진다 — 설계서 §6 이 막는 것.
func TestSkipReasonsAreClosedSet(t *testing.T) {
	all := []SkipReason{SkipConfBelow, SkipSampleRejected, SkipEquity, SkipDailyLimit, SkipFetchError, SkipPredictError}
	seen := map[SkipReason]bool{}
	for _, s := range all {
		if !s.Valid() {
			t.Errorf("%q 가 유효하지 않다", s)
		}
		if seen[s] {
			t.Errorf("%q 가 중복이다", s)
		}
		seen[s] = true
	}
	if SkipReason("").Valid() || SkipReason("unknown").Valid() {
		t.Error("빈 값이나 미지의 사유가 유효로 통과했다")
	}
}

// 라운드트립. 스냅샷은 JSON 으로만 오가므로 필드가 조용히 빠지면 모니터가
// 제로값을 진짜 값으로 읽는다 — equity 0 은 "무장 불가" 로, 노출 0 은
// "위반 없음" 으로 읽히고 둘 다 조용하다.
func TestSnapshotRoundTrip(t *testing.T) {
	in := Snapshot{
		Seq: 42, BootID: "9f2c", TS: time.Unix(1786000000, 0).UTC(), Version: "gld91 abc1234",
		Armed: true,
		Consts: Consts{CapFraction: 0.0455, DailyFraction: 0.10, ConfidenceThreshold: 0.0172, MinOrderUSD: 1.0},
		Equity: Equity{AvailableUSDT: 63.2, PositionCost: 126.1, CapUSD: 8.62, CanArm: true, DailyPnL: -2.58, DailyLimit: -19.19},
		Round:  Round{MarketID: 7, EndsAt: time.Unix(1786000300, 0).UTC(), State: RoundActive, PUp: 0.5314, Confidence: 0.0628, Outcome: "UP"},
		Exposure: Exposure{
			Filled: 41.2, Open: 18.0, PendingCancel: 6.1, Cap: 8.62,
			OpenOrders: []OpenOrder{{ID: "0xabc", Tick: 487, Notional: 18.0}},
		},
		Loop:  Loop{Reprices: 1840, RateLimitRemaining: 118},
		Skips: map[SkipReason]int{SkipConfBelow: 31},
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Snapshot
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("라운드트립 불일치\n in=%+v\nout=%+v", in, out)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./internal/beat/ -v`
Expected: FAIL — 패키지 없음

- [ ] **Step 3: 구현**

```go
// Package beat 는 봇과 모니터가 주고받는 계약이다. 봇도 모니터도 이 타입을
// 임포트한다 — 계약을 두 벌로 두면 갈리고, 갈린 쪽이 틀렸을 때 알 방법이
// 없다(internal/live/rounds.go 와 같은 원칙).
//
// 이 패키지는 순수하다. 네트워크도, 시계도, 전역 상태도 없다.
package beat

import "time"

// Command 는 모니터가 봇에게 시키는 것의 전부다.
type Command string

const (
	CmdNone     Command = "none"
	CmdShutdown Command = "shutdown"
	CmdHalt     Command = "halt"
)

func (c Command) Valid() bool { return c == CmdNone || c == CmdShutdown || c == CmdHalt }

// Reply 는 beat 응답이다.
//
// **필드를 늘리지 마라.** 모니터는 정산 결과를 알지만 봇은 몰라야 한다
// (exec 의 TestExecNeverWritesSettlement). 그 값이 여기 실리면 그 벽에
// 뒷문이 생긴다. snapshot_test.go 의 TestReplyCarriesOnlyCommands 가
// 이것을 리플렉션으로 고정한다.
type Reply struct {
	Command Command `json:"command"`
	// AckFor 는 이 응답이 어느 seq 에 대한 것인지다. 봇이 같은 명령을 두 번
	// 처리하지 않게 하는 유일한 수단이다.
	AckFor uint64 `json:"ack_for"`
}

// SkipReason 은 회차를 건너뛴 이유다.
//
// **사유 없는 스킵은 무가치하다.** 이 봇은 confidence 문턱 미달로 회차를
// 건너뛰는 것이 정상 동작이고 몇 시간 이어질 수 있다. "안 하고 있음"은
// 알람이 될 수 없고, 오직 "왜 안 하는지"만 판단 근거가 된다.
type SkipReason string

const (
	SkipConfBelow      SkipReason = "conf_below"      // 정상 — 알람 아님
	SkipSampleRejected SkipReason = "sample_rejected" // 워밍업·결측·도지
	SkipEquity         SkipReason = "equity"          // risk.CanArm false
	SkipDailyLimit     SkipReason = "daily_limit"
	SkipFetchError     SkipReason = "fetch_error"
	SkipPredictError   SkipReason = "predict_error"
)

func (s SkipReason) Valid() bool {
	switch s {
	case SkipConfBelow, SkipSampleRejected, SkipEquity, SkipDailyLimit, SkipFetchError, SkipPredictError:
		return true
	}
	return false
}

type RoundState string

const (
	RoundActive  RoundState = "ACTIVE"
	RoundSkipped RoundState = "SKIPPED"
	RoundIdle    RoundState = "IDLE"
)

// Consts 는 봇 바이너리에 박힌 상수다.
//
// 모니터가 파생 임계를 따로 계산하지 않고 이것을 받아 **예상값과 다르면
// 알린다.** 다르다는 것은 배포된 바이너리가 우리가 아는 그 바이너리가
// 아니라는 뜻이다.
type Consts struct {
	CapFraction         float64 `json:"cap_fraction"`
	DailyFraction       float64 `json:"daily_fraction"`
	ConfidenceThreshold float64 `json:"confidence_threshold"`
	MinOrderUSD         float64 `json:"min_order_usd"`
}

type Equity struct {
	AvailableUSDT float64 `json:"available_usdt"`
	PositionCost  float64 `json:"position_cost"`
	CapUSD        float64 `json:"cap_usd"`
	CanArm        bool    `json:"can_arm"`
	DailyPnL      float64 `json:"daily_pnl"`
	DailyLimit    float64 `json:"daily_limit"`
}

type Round struct {
	MarketID   int64      `json:"market_id"`
	Slug       string     `json:"slug"`
	EndsAt     time.Time  `json:"ends_at"`
	State      RoundState `json:"state"`
	PUp        float64    `json:"p_up"`
	Confidence float64    `json:"confidence"`
	Outcome    string     `json:"outcome"`
	SkipReason SkipReason `json:"skip_reason,omitempty"`
}

// OpenOrder 는 걸려 있는 주문 하나다.
//
// **개수가 아니라 목록으로 싣는다.** 개인키 없는 모니터는 GET /v1/orders 를
// 부를 수 없어(JWT 필요) 미체결을 독립으로 관측할 수단이 없다. 봇이 죽으면
// 마지막 beat 의 이 목록이 유일한 정보원이다.
type OpenOrder struct {
	ID       string  `json:"id"`
	Tick     int64   `json:"tick"`
	Notional float64 `json:"notional"`
}

// Exposure 는 exec 의 노출 불변식 그대로다: Filled+Open+PendingCancel < Cap.
type Exposure struct {
	Filled        float64     `json:"filled"`
	Open          float64     `json:"open"`
	PendingCancel float64     `json:"pending_cancel"`
	Cap           float64     `json:"cap"`
	OpenOrders    []OpenOrder `json:"open_orders"`
	// Unaccounted 는 생성 결과를 모르고 식별자도 없어 취소조차 못 하는
	// 주문의 명목이다(exec 의 roundState.unknownNotional).
	Unaccounted float64 `json:"unaccounted"`
}

type Loop struct {
	Reprices           int64     `json:"reprices"`
	LastRepriceAt      time.Time `json:"last_reprice_at"`
	WSLastDataAt       time.Time `json:"ws_last_data_at"`
	FillsPollAt        time.Time `json:"fills_poll_at"`
	RateLimitRemaining int       `json:"ratelimit_remaining"`
}

// Snapshot 은 한 번의 beat 다.
type Snapshot struct {
	Seq uint64 `json:"seq"`
	// BootID 는 프로세스마다 새로 만드는 값이다. mtime 하트비트는 3초마다
	// 죽고 살아나는 크래시루프를 완벽히 건강하게 보고한다 — 파일은 계속
	// 신선하기 때문이다. 이 값의 변화가 재시작을 드러낸다.
	BootID   string             `json:"boot_id"`
	TS       time.Time          `json:"ts"`
	Version  string             `json:"version"`
	Armed    bool               `json:"armed"`
	Consts   Consts             `json:"consts"`
	Equity   Equity             `json:"equity"`
	Round    Round              `json:"round"`
	Exposure Exposure           `json:"exposure"`
	Loop     Loop               `json:"loop"`
	Skips    map[SkipReason]int `json:"skips"`
}
```

- [ ] **Step 4: 통과 확인**

Run: `GOTOOLCHAIN=local go test ./internal/beat/ -race -v`
Expected: PASS (4개)

- [ ] **Step 5: 커밋**

```bash
git add internal/beat/
git commit -m "beat: 스냅샷 계약 — 봇과 모니터가 같은 타입을 쓴다"
```

---

### Task 3: `internal/beat` — HMAC 과 재전송 방지

**Files:**
- Create: `internal/beat/hmac.go`, `internal/beat/hmac_test.go`

**Interfaces:**
- Consumes: `beat.Snapshot` (Task 2)
- Produces: `beat.Sign(secret []byte, body []byte) string`, `beat.Verify(secret, body []byte, sig string) bool`, `type Gate struct{ Skew time.Duration }`, `(*Gate) Admit(seq uint64, ts, now time.Time) error`, `beat.ErrReplay`, `beat.ErrSkew`

- [ ] **Step 1: 실패 테스트**

```go
// 서명은 상수시간 비교여야 한다. 바이트별 조기 반환은 타이밍으로 정답을
// 한 바이트씩 알려준다.
func TestVerifyRejectsWrongSignature(t *testing.T) {
	secret, body := []byte("s3cret"), []byte(`{"seq":1}`)
	sig := Sign(secret, body)
	if !Verify(secret, body, sig) {
		t.Fatal("올바른 서명이 거부됐다")
	}
	for _, bad := range []string{"", sig + "0", sig[:len(sig)-1] + "f", Sign([]byte("other"), body)} {
		if Verify(secret, body, bad) {
			t.Errorf("잘못된 서명 %q 가 통과했다", bad)
		}
	}
	if Verify(secret, []byte(`{"seq":2}`), sig) {
		t.Error("본문이 바뀌었는데 서명이 통과했다")
	}
}

// seq 는 단조증가여야 한다. 같은 값이나 역행은 재전송이다.
func TestGateRejectsReplay(t *testing.T) {
	g := &Gate{Skew: time.Minute}
	now := time.Unix(1786000000, 0).UTC()
	if err := g.Admit(10, now, now); err != nil {
		t.Fatalf("첫 beat 가 거부됐다: %v", err)
	}
	for _, seq := range []uint64{10, 9, 0} {
		if err := g.Admit(seq, now, now); !errors.Is(err, ErrReplay) {
			t.Errorf("seq %d 에서 err=%v, want ErrReplay", seq, err)
		}
	}
	if err := g.Admit(11, now, now); err != nil {
		t.Errorf("seq 11 이 거부됐다: %v", err)
	}
}

// 봇이 재시작하면 seq 가 1 로 돌아간다. 그것을 재전송으로 막으면 재시작 뒤
// 모든 beat 가 거부되어 모니터가 봇을 영원히 죽은 것으로 본다 — 감시 장치가
// 스스로 눈을 감는 형태의 고장이다. 재시작은 Gate 를 새로 만들어 처리한다.
func TestGateResetAllowsLowerSeq(t *testing.T) {
	g := &Gate{Skew: time.Minute}
	now := time.Unix(1786000000, 0).UTC()
	_ = g.Admit(9999, now, now)
	g.Reset()
	if err := g.Admit(1, now, now); err != nil {
		t.Errorf("리셋 뒤 seq 1 이 거부됐다: %v", err)
	}
}

func TestGateRejectsSkew(t *testing.T) {
	g := &Gate{Skew: time.Minute}
	now := time.Unix(1786000000, 0).UTC()
	for _, ts := range []time.Time{now.Add(-2 * time.Minute), now.Add(2 * time.Minute)} {
		if err := g.Admit(1, ts, now); !errors.Is(err, ErrSkew) {
			t.Errorf("ts=%v 에서 err=%v, want ErrSkew", ts, err)
		}
		g.Reset()
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./internal/beat/ -run 'TestVerify|TestGate' -v`
Expected: FAIL — `Sign` 미정의

- [ ] **Step 3: 구현**

```go
package beat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrReplay 는 seq 가 전진하지 않았다는 뜻이다.
	ErrReplay = errors.New("beat: seq 가 전진하지 않았다")
	// ErrSkew 는 타임스탬프가 허용창 밖이라는 뜻이다.
	ErrSkew = errors.New("beat: 타임스탬프가 허용창 밖이다")
)

// Sign 은 본문의 HMAC-SHA256 을 hex 로 돌려준다.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify 는 상수시간으로 비교한다. hmac.Equal 을 쓰는 것이 요점이고,
// == 로 바꾸면 타이밍이 정답을 한 바이트씩 흘린다.
func Verify(secret, body []byte, sig string) bool {
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(m.Sum(nil), want)
}

// Gate 는 재전송을 막는다. 모니터가 세션마다 하나 들고, 봇 재시작(BootID
// 변화)에서 Reset 한다.
//
// 순수하다 — 현재 시각을 인자로 받는다.
type Gate struct {
	// Skew 는 봇과 모니터 시계 차이의 허용치다.
	Skew    time.Duration
	last    uint64
	started bool
}

func (g *Gate) Reset() { g.last, g.started = 0, false }

// Admit 은 이 beat 를 받아도 되는지 본다.
func (g *Gate) Admit(seq uint64, ts, now time.Time) error {
	if d := now.Sub(ts); d > g.Skew || d < -g.Skew {
		return fmt.Errorf("%w: %s", ErrSkew, d)
	}
	if g.started && seq <= g.last {
		return fmt.Errorf("%w: %d <= %d", ErrReplay, seq, g.last)
	}
	g.last, g.started = seq, true
	return nil
}
```

- [ ] **Step 4: 통과 확인**

Run: `GOTOOLCHAIN=local go test ./internal/beat/ -race -v`
Expected: PASS (8개)

- [ ] **Step 5: 커밋**

```bash
git add internal/beat/hmac.go internal/beat/hmac_test.go
git commit -m "beat: HMAC 서명과 재전송 방지"
```

---

### Task 4: `internal/beat/rule` — 판정

설계서 §6. **가장 중요한 태스크다.** 정기 푸시가 4시간이라 알람이 사실상 유일한 감지 경로다.

**Files:**
- Create: `internal/beat/rule/rule.go`, `internal/beat/rule/rule_test.go`

**Interfaces:**
- Consumes: `beat.Snapshot`, `beat.SkipReason`, `beat.Consts` (Task 2)
- Produces: `rule.Level`(`Info`/`Warn`/`Crit`), `rule.Finding{Key string; Level Level; Message string}`, `rule.Input{Snap *beat.Snapshot; LastBeat time.Time; Now time.Time; Want beat.Consts; ConsecSkips map[beat.SkipReason]int; BootChanges int}`, `rule.Evaluate(in Input) []Finding`

- [ ] **Step 1: 실패 테스트**

```go
// 이 봇에서 "안 하고 있음"은 알람이 아니다. confidence < 0.0172 스킵은
// 옳은 동작이고 몇 시간 이어질 수 있다. GLD-9(마켓메이커)용 규칙을 그대로
// 옮기면 여기서 오탐이 폭발한다.
func TestConfBelowSkipIsSilent(t *testing.T) {
	in := healthy()
	in.Snap.Round.State = beat.RoundSkipped
	in.Snap.Round.SkipReason = beat.SkipConfBelow
	in.ConsecSkips = map[beat.SkipReason]int{beat.SkipConfBelow: 200}
	if f := Evaluate(in); len(f) != 0 {
		t.Errorf("confidence 미달 스킵에 알람이 났다: %+v", f)
	}
}

func TestOtherSkipReasonsAlert(t *testing.T) {
	cases := []struct {
		reason beat.SkipReason
		consec int
		want   Level
	}{
		{beat.SkipSampleRejected, 5, Warn},
		{beat.SkipSampleRejected, 4, Info}, // 미달이면 알람 없음 → 빈 결과
		{beat.SkipEquity, 1, Warn},
		{beat.SkipDailyLimit, 1, Crit},
		{beat.SkipFetchError, 3, Crit},
		{beat.SkipPredictError, 3, Crit},
	}
	for _, c := range cases {
		in := healthy()
		in.Snap.Round.State = beat.RoundSkipped
		in.Snap.Round.SkipReason = c.reason
		in.ConsecSkips = map[beat.SkipReason]int{c.reason: c.consec}
		got := findLevel(Evaluate(in), "skip:"+string(c.reason))
		if got != c.want {
			t.Errorf("%s×%d → %v, want %v", c.reason, c.consec, got, c.want)
		}
	}
}

// 상수가 다르면 우리가 아는 그 바이너리가 아니다.
func TestConstMismatchIsCritical(t *testing.T) {
	for _, mut := range []func(*beat.Consts){
		func(c *beat.Consts) { c.CapFraction = 0.05 },
		func(c *beat.Consts) { c.DailyFraction = 0.20 },
		func(c *beat.Consts) { c.ConfidenceThreshold = 0.01 },
		func(c *beat.Consts) { c.MinOrderUSD = 5 },
	} {
		in := healthy()
		mut(&in.Snap.Consts)
		if got := findLevel(Evaluate(in), "consts"); got != Crit {
			t.Errorf("상수 불일치가 %v 로 났다, want Crit", got)
		}
	}
}

// exec 의 근본 불변식. 이것이 깨지면 한도를 넘겨 베팅하고 있다.
func TestExposureInvariantViolation(t *testing.T) {
	in := healthy()
	in.Snap.Exposure = beat.Exposure{Filled: 5, Open: 3, PendingCancel: 1, Cap: 8.62}
	if got := findLevel(Evaluate(in), "exposure"); got != Crit {
		t.Errorf("9.0 >= 8.62 인데 %v 다, want Crit", got)
	}
}

// Task 7 report §4(나): 취소 배치 100 을 넘기면 rest 가 요청 자체를 거절해
// 아무것도 취소되지 않는다. 미확인 주문 상한이 cap/$1 이라 equity>$2,200 에서
// 도달한다.
func TestCancelBatchCeilingApproach(t *testing.T) {
	in := healthy()
	in.Snap.Exposure.OpenOrders = make([]beat.OpenOrder, 81)
	if got := findLevel(Evaluate(in), "cancel_batch"); got != Crit {
		t.Errorf("미체결 81건에 %v, want Crit", got)
	}

	in = healthy()
	in.Snap.Equity.AvailableUSDT = 2300
	if got := findLevel(Evaluate(in), "equity_ceiling"); got != Warn {
		t.Errorf("equity $2,300 에 %v, want Warn", got)
	}
}

// cmd/gld91/fills.go 가 스스로 경고한 것: 매 바퀴 REST 를 치면 레이트리밋이
// 즉사한다. 회차당 6,000 바퀴 × 240 req/min.
func TestRateLimitExhaustionIsCritical(t *testing.T) {
	in := healthy()
	in.Snap.Loop.RateLimitRemaining = 0
	if got := findLevel(Evaluate(in), "ratelimit"); got != Crit {
		t.Errorf("예산 0 에 %v, want Crit", got)
	}
}

// beat 가 안 오면 봇이 죽은 것이다.
func TestStaleBeat(t *testing.T) {
	in := healthy()
	in.LastBeat = in.Now.Add(-21 * time.Second)
	if got := findLevel(Evaluate(in), "stale"); got != Crit {
		t.Errorf("21초 무응답에 %v, want Crit", got)
	}
	in.LastBeat = in.Now.Add(-19 * time.Second)
	if got := findLevel(Evaluate(in), "stale"); got != Info {
		t.Errorf("19초에 알람이 났다: %v", got)
	}
}

// mtime 하트비트가 절대 못 잡는 것. 3초마다 죽고 살아나도 파일은 신선하다.
func TestCrashLoop(t *testing.T) {
	in := healthy()
	in.BootChanges = 2
	if got := findLevel(Evaluate(in), "crashloop"); got != Crit {
		t.Errorf("10분 내 재시작 2회에 %v, want Crit", got)
	}
}

// 하트비트는 오는데 마켓데이터만 끊긴 경우 — ws/conn.go:151 이 이미 아는 고장.
func TestWSDataStall(t *testing.T) {
	in := healthy()
	in.Snap.Loop.WSLastDataAt = in.Now.Add(-31 * time.Second)
	if got := findLevel(Evaluate(in), "ws_data"); got != Crit {
		t.Errorf("WS 데이터 31초 정체에 %v, want Crit", got)
	}
}

func TestDisarmAndDailyLimit(t *testing.T) {
	in := healthy()
	in.Snap.Armed = false
	if got := findLevel(Evaluate(in), "disarmed"); got != Crit {
		t.Errorf("무장 해제에 %v, want Crit", got)
	}

	in = healthy()
	in.Snap.Equity.DailyPnL = -20.0 // 한도 -19.19
	if got := findLevel(Evaluate(in), "daily_limit"); got != Crit {
		t.Errorf("일손실 한도 초과에 %v, want Crit", got)
	}
}

// 망가진 입력은 "이상 없음"이 아니다. 조용한 정상이 감시 장치의 실패 모드다.
func TestBrokenSnapshotAlerts(t *testing.T) {
	for name, mut := range map[string]func(*beat.Snapshot){
		"nan-equity":  func(s *beat.Snapshot) { s.Equity.AvailableUSDT = math.NaN() },
		"inf-cap":     func(s *beat.Snapshot) { s.Exposure.Cap = math.Inf(1) },
		"nan-exposed": func(s *beat.Snapshot) { s.Exposure.Filled = math.NaN() },
	} {
		in := healthy()
		mut(in.Snap)
		if got := findLevel(Evaluate(in), "broken"); got != Crit {
			t.Errorf("%s 에 %v, want Crit", name, got)
		}
	}
}

func TestNilSnapshotAlerts(t *testing.T) {
	in := healthy()
	in.Snap = nil
	f := Evaluate(in)
	if len(f) == 0 {
		t.Fatal("스냅샷이 nil 인데 조용하다")
	}
}

// 건강한 봇에는 아무 알람도 없어야 한다. 이것이 없으면 위 테스트들이
// "항상 모든 알람을 낸다" 는 구현으로도 전부 통과한다.
func TestHealthyIsSilent(t *testing.T) {
	if f := Evaluate(healthy()); len(f) != 0 {
		t.Errorf("건강한 스냅샷에 알람이 났다: %+v", f)
	}
}

// --- 헬퍼 ---

func healthy() Input {
	now := time.Unix(1786000000, 0).UTC()
	c := beat.Consts{CapFraction: 0.0455, DailyFraction: 0.10, ConfidenceThreshold: 0.0172, MinOrderUSD: 1.0}
	return Input{
		Now: now, LastBeat: now.Add(-3 * time.Second), Want: c,
		ConsecSkips: map[beat.SkipReason]int{},
		Snap: &beat.Snapshot{
			Seq: 1, BootID: "a", TS: now, Armed: true, Consts: c,
			Equity:   beat.Equity{AvailableUSDT: 63.2, PositionCost: 126.1, CapUSD: 8.62, CanArm: true, DailyPnL: -2.58, DailyLimit: -19.19},
			Round:    beat.Round{State: beat.RoundActive, EndsAt: now.Add(2 * time.Minute)},
			Exposure: beat.Exposure{Filled: 4, Open: 2, PendingCancel: 1, Cap: 8.62},
			Loop:     beat.Loop{WSLastDataAt: now.Add(-time.Second), RateLimitRemaining: 118},
			Skips:    map[beat.SkipReason]int{},
		},
	}
}

// findLevel 은 그 키의 알람 등급이다. 없으면 Info(=알람 없음).
func findLevel(fs []Finding, key string) Level {
	for _, f := range fs {
		if f.Key == key {
			return f.Level
		}
	}
	return Info
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./internal/beat/rule/ -v`
Expected: FAIL — 패키지 없음

- [ ] **Step 3: 구현**

임계는 상수로 모은다. 근거 없는 값은 그 사실을 주석에 적는다 — `internal/risk` 가 *"임계가 없는 조건은 두지 않는다"* 고 한 원칙의 짝이다.

```go
// Package rule 은 스냅샷 하나를 보고 알람을 낸다.
//
// 순수하다 — 네트워크도, 시계도, 전역 상태도 없다. 현재 시각과 직전 상태는
// 전부 Input 으로 받는다. 그래야 규칙 하나하나를 값 몇 개로 시험할 수 있고,
// 시험할 수 없는 감시 장치는 감시하지 않는 것과 같다.
//
// # 망가진 입력에서의 방향 — 알리는 쪽이다
//
// internal/risk 는 "거래하지 않는 쪽"으로 실패한다. 여기는 반대로 **알리는
// 쪽**이다. NaN 이나 nil 을 조용히 넘기면 "이상 없음" 으로 읽히는데, 감시
// 장치의 침묵은 곧 정상 신호다. 틀린 알람은 사람이 무시하면 되지만 없는
// 알람은 사람이 알 방법이 없다.
package rule

import (
	"fmt"
	"math"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

type Level int

const (
	Info Level = iota // 알람 아님
	Warn
	Crit
)

func (l Level) String() string {
	switch l {
	case Warn:
		return "⚠️"
	case Crit:
		return "🚨"
	}
	return "·"
}

// Finding 은 알람 하나다. Key 는 래치의 식별자이므로 안정적이어야 한다.
type Finding struct {
	Key     string
	Level   Level
	Message string
}

// 임계. **근거가 없는 값은 그렇게 적는다.**
const (
	// StaleAfter — beat 3초 주기의 6회 연속 실패. 네트워크 왕복이 끼므로
	// GLD-7 의 파일 mtime 15초보다 넉넉하다.
	StaleAfter = 20 * time.Second
	// WSDataStall — P4 실측 같은 호가창 p99 2.0s / 최대 9.0s. 30초면
	// 오탐 여지 없이 확실한 사망이다.
	WSDataStall = 30 * time.Second
	// CrashLoopChanges — 10분 내 재시작 횟수. 근거 없음(§11).
	CrashLoopChanges = 2
	// CancelBatchWarn — rest 의 배치 상한 100 에 대한 여유. Task 7 report §4(나).
	CancelBatchWarn = 80
	// EquityCeiling — 미확인 주문 상한이 cap/$1 이므로 이 위에서 배치 상한
	// 100 에 도달할 수 있다. $1 / 0.0455 ≈ $2,198.
	EquityCeiling = 2200.0
	// SampleRejectedConsec / FetchErrorConsec — 근거 없음(§11).
	SampleRejectedConsec = 5
	FetchErrorConsec     = 3
)

// Input 은 판정에 필요한 전부다.
type Input struct {
	Snap        *beat.Snapshot
	LastBeat    time.Time
	Now         time.Time
	Want        beat.Consts
	ConsecSkips map[beat.SkipReason]int
	BootChanges int // 최근 10분 재시작 횟수
}

// Evaluate 는 이 입력에서 나는 알람 전부다. 순서는 안정적이다.
func Evaluate(in Input) []Finding {
	var out []Finding
	add := func(key string, lv Level, format string, args ...any) {
		out = append(out, Finding{Key: key, Level: lv, Message: fmt.Sprintf(format, args...)})
	}

	if in.Snap == nil {
		add("broken", Crit, "스냅샷이 없다 — 모니터 배선을 확인하라")
		return out
	}
	s := in.Snap

	// 죽음
	if age := in.Now.Sub(in.LastBeat); age > StaleAfter {
		add("stale", Crit, "하트비트 %.0f초 무응답", age.Seconds())
	}
	if in.BootChanges >= CrashLoopChanges {
		add("crashloop", Crit, "10분 내 재시작 %d회 — 크래시루프", in.BootChanges)
	}

	// 망가진 값. 알람이 나야 하는 쪽이다.
	for name, v := range map[string]float64{
		"equity.available": s.Equity.AvailableUSDT, "equity.cap": s.Equity.CapUSD,
		"exposure.filled": s.Exposure.Filled, "exposure.open": s.Exposure.Open,
		"exposure.pending": s.Exposure.PendingCancel, "exposure.cap": s.Exposure.Cap,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			add("broken", Crit, "%s 가 %v 다 — 상류 조회가 망가졌다", name, v)
			break
		}
	}

	// 상수 대조
	if s.Consts != in.Want {
		add("consts", Crit, "봇 상수가 예상과 다르다: %+v (want %+v)", s.Consts, in.Want)
	}

	// 살아있는데 죽음
	if !s.Armed {
		add("disarmed", Crit, "무장이 해제됐다")
	}
	if !s.Loop.WSLastDataAt.IsZero() {
		if age := in.Now.Sub(s.Loop.WSLastDataAt); age > WSDataStall {
			add("ws_data", Crit, "WS 마켓데이터 %.0f초 정체 (하트비트는 살아 있다)", age.Seconds())
		}
	}
	if s.Equity.DailyPnL <= s.Equity.DailyLimit {
		add("daily_limit", Crit, "일손실 %.2f 가 한도 %.2f 에 닿았다", s.Equity.DailyPnL, s.Equity.DailyLimit)
	}
	if !s.Equity.CanArm {
		add("can_arm", Warn, "equity 가 무장 최소치 미만이다 (cap %.2f)", s.Equity.CapUSD)
	}

	// 노출·집행
	if total := s.Exposure.Filled + s.Exposure.Open + s.Exposure.PendingCancel; total >= s.Exposure.Cap {
		add("exposure", Crit, "노출 불변식 위반: %.2f >= cap %.2f", total, s.Exposure.Cap)
	}
	if n := len(s.Exposure.OpenOrders); n > CancelBatchWarn {
		add("cancel_batch", Crit, "미체결 %d건 — 배치 상한 100 을 넘으면 아무것도 취소되지 않는다", n)
	}
	if s.Equity.AvailableUSDT > EquityCeiling {
		add("equity_ceiling", Warn, "equity $%.0f — 취소 배치 상한에 도달할 수 있는 구간이다", s.Equity.AvailableUSDT)
	}
	if s.Exposure.Unaccounted > 0 {
		add("unaccounted", Warn, "식별자 없는 주문 명목 $%.2f — 취소할 수 없다", s.Exposure.Unaccounted)
	}
	if s.Loop.RateLimitRemaining <= 0 {
		add("ratelimit", Crit, "요청 예산이 0 이다 — Fills 스로틀을 확인하라")
	}

	// 스킵. conf_below 는 정상이므로 아무것도 하지 않는다.
	if s.Round.State == beat.RoundSkipped {
		out = append(out, skipFinding(s.Round.SkipReason, in.ConsecSkips[s.Round.SkipReason])...)
	}
	return out
}

// skipFinding 은 스킵 사유별 판정이다.
//
// **conf_below 는 여기서 조용하다.** confidence < 0.0172 로 건너뛰는 것은
// 옳은 동작이고, 그 상태가 몇 시간 이어질 수 있다. 이 봇에서 "안 하고 있음"
// 자체는 알람이 될 수 없고 오직 "왜" 만 근거가 된다.
func skipFinding(r beat.SkipReason, consec int) []Finding {
	key := "skip:" + string(r)
	f := func(lv Level, format string, args ...any) []Finding {
		return []Finding{{Key: key, Level: lv, Message: fmt.Sprintf(format, args...)}}
	}
	switch r {
	case beat.SkipConfBelow:
		return nil
	case beat.SkipSampleRejected:
		if consec >= SampleRejectedConsec {
			return f(Warn, "표본 미채택 %d회차 연속 — 봉 데이터를 확인하라", consec)
		}
	case beat.SkipEquity:
		return f(Warn, "equity 부족으로 회차를 건너뛴다")
	case beat.SkipDailyLimit:
		return f(Crit, "일손실 한도로 회차를 건너뛴다")
	case beat.SkipFetchError, beat.SkipPredictError:
		if consec >= FetchErrorConsec {
			return f(Crit, "%s %d회차 연속", r, consec)
		}
	default:
		return f(Crit, "알 수 없는 스킵 사유 %q — 봇이 새 사유를 보냈다", r)
	}
	return nil
}
```

- [ ] **Step 4: 통과 확인**

Run: `GOTOOLCHAIN=local go test ./internal/beat/rule/ -race -v`
Expected: PASS (13개)

- [ ] **Step 5: 변이 시험**

이 저장소의 관례다. 아래 변이를 하나씩 넣고 **각각을 죽이는 테스트가 있는지** 확인한다. 살아남는 변이가 있으면 테스트를 먼저 고친다.

| # | 변이 | 죽이는 테스트 |
|---|---|---|
| M01 | `skipFinding` 의 `SkipConfBelow` 를 `Warn` 으로 | `TestConfBelowSkipIsSilent` |
| M02 | `total >= Cap` → `total > Cap` | `TestExposureInvariantViolation` (경계 9.0 vs 8.62 로는 안 잡힘 → **경계 케이스 추가 필요**) |
| M03 | `n > CancelBatchWarn` → `n > 100` | `TestCancelBatchCeilingApproach` |
| M04 | `s.Consts != in.Want` 를 삭제 | `TestConstMismatchIsCritical` |
| M05 | NaN 검사 삭제 | `TestBrokenSnapshotAlerts` |
| M06 | `RateLimitRemaining <= 0` → `< 0` | `TestRateLimitExhaustionIsCritical` |
| M07 | `age > StaleAfter` → `age > 2*StaleAfter` | `TestStaleBeat` |
| M08 | `Evaluate` 가 항상 빈 슬라이스 | `TestHealthyIsSilent` 를 제외한 전부 |

M02 가 살아남으므로 경계 테스트를 추가한다:

```go
func TestExposureInvariantBoundary(t *testing.T) {
	in := healthy()
	// 정확히 cap 과 같으면 위반이다 — 제약이 강한 부등호(< cap)이기 때문이다.
	in.Snap.Exposure = beat.Exposure{Filled: 8.62, Cap: 8.62}
	if got := findLevel(Evaluate(in), "exposure"); got != Crit {
		t.Errorf("정확히 cap 인데 %v, want Crit — 제약은 강한 부등호다", got)
	}
	in.Snap.Exposure = beat.Exposure{Filled: 8.61, Cap: 8.62}
	if got := findLevel(Evaluate(in), "exposure"); got != Info {
		t.Errorf("cap 미만인데 알람이 났다: %v", got)
	}
}
```

- [ ] **Step 6: 커밋**

```bash
git add internal/beat/rule/
git commit -m "beat/rule: 판정 — '안 하고 있음'은 알람이 아니다

confidence < 0.0172 스킵은 옳은 동작이고 몇 시간 이어질 수 있다.
GLD-9(마켓메이커)용 rounds_15m==0 규칙을 그대로 옮기면 오탐이 폭발한다.
스킵은 사유로만 판단한다."
```

---

### Task 5: `internal/beat/rule` — 래치

같은 조건이 3초마다 알람을 내면 사람이 알림을 끈다. 알람 피로가 감시 장치의 실패 모드다.

**Files:**
- Create: `internal/beat/rule/latch.go`, `internal/beat/rule/latch_test.go`

**Interfaces:**
- Consumes: `rule.Finding`, `rule.Level` (Task 4)
- Produces: `rule.Latch`, `rule.NewLatch(sustain time.Duration) *Latch`, `(*Latch) Step(fs []Finding, now time.Time) (fire []Finding, resolved []string)`

- [ ] **Step 1: 실패 테스트**

```go
// 같은 알람은 한 번만 운다.
func TestLatchFiresOnce(t *testing.T) {
	l := NewLatch(0)
	now := time.Unix(1786000000, 0).UTC()
	f := []Finding{{Key: "exposure", Level: Crit, Message: "위반"}}

	fire, _ := l.Step(f, now)
	if len(fire) != 1 {
		t.Fatalf("첫 발생에 %d개, want 1", len(fire))
	}
	for i := 1; i < 5; i++ {
		if fire, _ := l.Step(f, now.Add(time.Duration(i)*time.Second)); len(fire) != 0 {
			t.Errorf("%d번째 반복에 또 울었다", i)
		}
	}
}

// 사라지면 복구를 한 번 알리고, 다시 나면 다시 운다.
func TestLatchResolvesAndRefires(t *testing.T) {
	l := NewLatch(0)
	now := time.Unix(1786000000, 0).UTC()
	f := []Finding{{Key: "ws_data", Level: Crit}}

	l.Step(f, now)
	_, resolved := l.Step(nil, now.Add(time.Second))
	if len(resolved) != 1 || resolved[0] != "ws_data" {
		t.Fatalf("복구 = %v, want [ws_data]", resolved)
	}
	if _, resolved := l.Step(nil, now.Add(2*time.Second)); len(resolved) != 0 {
		t.Error("복구가 두 번 났다")
	}
	if fire, _ := l.Step(f, now.Add(3*time.Second)); len(fire) != 1 {
		t.Error("재발에 울지 않았다")
	}
}

// 지속시간을 넘겨야 운다. 순간 스파이크로 울면 4시간 리포트를 늘린 의미가 없다.
func TestLatchRequiresSustain(t *testing.T) {
	l := NewLatch(10 * time.Second)
	now := time.Unix(1786000000, 0).UTC()
	f := []Finding{{Key: "can_arm", Level: Warn}}

	if fire, _ := l.Step(f, now); len(fire) != 0 {
		t.Error("첫 관측에 즉시 울었다")
	}
	if fire, _ := l.Step(f, now.Add(9*time.Second)); len(fire) != 0 {
		t.Error("9초에 울었다")
	}
	if fire, _ := l.Step(f, now.Add(11*time.Second)); len(fire) != 1 {
		t.Error("11초에 울지 않았다")
	}
}

// 지속 중에 끊기면 시계가 리셋된다.
func TestLatchSustainResets(t *testing.T) {
	l := NewLatch(10 * time.Second)
	now := time.Unix(1786000000, 0).UTC()
	f := []Finding{{Key: "can_arm", Level: Warn}}

	l.Step(f, now)
	l.Step(nil, now.Add(5*time.Second))
	l.Step(f, now.Add(6*time.Second))
	if fire, _ := l.Step(f, now.Add(12*time.Second)); len(fire) != 0 {
		t.Error("리셋됐어야 하는데 12초에 울었다")
	}
	if fire, _ := l.Step(f, now.Add(17*time.Second)); len(fire) != 1 {
		t.Error("리셋 뒤 17초에 울지 않았다")
	}
}

// Crit 은 지속을 기다리지 않는다. 노출 위반은 지금 돈이 나가는 중이다.
func TestLatchCriticalFiresImmediately(t *testing.T) {
	l := NewLatch(10 * time.Second)
	now := time.Unix(1786000000, 0).UTC()
	if fire, _ := l.Step([]Finding{{Key: "exposure", Level: Crit}}, now); len(fire) != 1 {
		t.Error("Crit 이 지속을 기다렸다")
	}
}

// 등급이 올라가면 다시 운다 — Warn 으로 울고 조용해진 사이에 Crit 이 되면
// 사람은 여전히 Warn 인 줄 안다.
func TestLatchRefiresOnEscalation(t *testing.T) {
	l := NewLatch(0)
	now := time.Unix(1786000000, 0).UTC()
	l.Step([]Finding{{Key: "skip:equity", Level: Warn}}, now)
	fire, _ := l.Step([]Finding{{Key: "skip:equity", Level: Crit}}, now.Add(time.Second))
	if len(fire) != 1 || fire[0].Level != Crit {
		t.Errorf("승격에 %v, want Crit 1건", fire)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./internal/beat/rule/ -run TestLatch -v`
Expected: FAIL — `NewLatch` 미정의

- [ ] **Step 3: 구현**

```go
package rule

import "time"

// Latch 는 알람의 에지를 잡는다 — 같은 조건이 3초마다 우는 것을 막고,
// 사라지면 복구를 한 번 알린다. GLD-7 하트비트 모니터의 Changed 패턴이다.
//
// 순수하다. 현재 시각은 인자로 받는다.
type Latch struct {
	sustain time.Duration
	state   map[string]*latchEntry
}

type latchEntry struct {
	since time.Time // 이 조건을 처음 본 시각
	fired bool
	level Level
}

func NewLatch(sustain time.Duration) *Latch {
	return &Latch{sustain: sustain, state: map[string]*latchEntry{}}
}

// Step 은 이번 판정을 넣고, 새로 울릴 것과 복구된 키를 돌려준다.
//
// **Crit 은 지속을 기다리지 않는다.** 노출 불변식 위반은 지금 이 순간 한도를
// 넘겨 베팅하고 있다는 뜻이고, 그것을 10초 확인하는 동안 더 나간다.
func (l *Latch) Step(fs []Finding, now time.Time) (fire []Finding, resolved []string) {
	seen := make(map[string]bool, len(fs))
	for _, f := range fs {
		seen[f.Key] = true
		e, ok := l.state[f.Key]
		if !ok {
			e = &latchEntry{since: now, level: f.Level}
			l.state[f.Key] = e
		}
		escalated := f.Level > e.level
		if escalated {
			e.level, e.fired = f.Level, false
		}
		if e.fired {
			continue
		}
		if f.Level == Crit || now.Sub(e.since) >= l.sustain {
			e.fired = true
			fire = append(fire, f)
		}
	}
	for key, e := range l.state {
		if seen[key] {
			continue
		}
		if e.fired {
			resolved = append(resolved, key)
		}
		delete(l.state, key)
	}
	return fire, resolved
}
```

> `resolved` 의 순서는 맵 순회라 비결정적이다. 테스트가 1건만 보므로 문제되지 않지만, 리포트에 여러 개를 실을 때는 호출자가 정렬한다.

- [ ] **Step 4: 통과 확인**

Run: `GOTOOLCHAIN=local go test ./internal/beat/rule/ -race -v`
Expected: PASS (20개)

- [ ] **Step 5: 커밋**

```bash
git add internal/beat/rule/latch.go internal/beat/rule/latch_test.go
git commit -m "beat/rule: 래치 — 에지 트리거·지속시간·복구 알림"
```

---

### Task 6: 봇 쪽 Reporter — 전송

**Files:**
- Create: `cmd/gld91/beat.go`, `cmd/gld91/beat_test.go`

**Interfaces:**
- Consumes: `beat.Snapshot`, `beat.Reply`, `beat.Sign` (Task 2·3)
- Produces: `reporter` 구조체, `newReporter(endpoint string, secret []byte, bootID string) *reporter`, `(*reporter) Publish(s beat.Snapshot)`, `(*reporter) Run(ctx context.Context)`, `(*reporter) Commands() <-chan beat.Command`, `(*reporter) ConsecFail() int`

- [ ] **Step 1: 실패 테스트**

```go
// Publish 는 exec 루프에서 불린다 — 회차당 6,000 바퀴, 기본 50ms. 여기서
// 네트워크를 기다리면 재호가가 밀리고, 밀린 재호가는 큐 위치를 잃는다.
func TestPublishNeverBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // 죽은 모니터를 흉내낸다
	}))
	defer srv.Close()

	rp := newReporter(srv.URL, []byte("s"), "boot")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)

	start := time.Now()
	for i := 0; i < 1000; i++ {
		rp.Publish(beat.Snapshot{Seq: uint64(i)})
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("Publish 1000회에 %v 걸렸다 — 블록하고 있다", d)
	}
}

// 최신값만 보낸다. 큐가 쌓이면 모니터가 과거를 현재로 읽는다.
func TestReporterSendsLatestOnly(t *testing.T) {
	var got []uint64
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s beat.Snapshot
		json.NewDecoder(r.Body).Decode(&s)
		mu.Lock()
		got = append(got, s.Seq)
		mu.Unlock()
		json.NewEncoder(w).Encode(beat.Reply{Command: beat.CmdNone})
	}))
	defer srv.Close()

	rp := newReporter(srv.URL, []byte("s"), "boot")
	rp.interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)

	for i := 1; i <= 100; i++ {
		rp.Publish(beat.Snapshot{Seq: uint64(i)})
	}
	time.Sleep(80 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for _, seq := range got {
		if seq < 90 {
			t.Errorf("오래된 스냅샷 seq=%d 가 전송됐다 — 큐가 쌓였다", seq)
		}
	}
}

// 서명이 붙는다.
func TestReporterSignsBody(t *testing.T) {
	secret := []byte("s3cret")
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !beat.Verify(secret, body, r.Header.Get(beat.SigHeader)) {
			t.Error("서명이 검증되지 않는다")
		}
		json.NewEncoder(w).Encode(beat.Reply{Command: beat.CmdNone})
		close(done)
	}))
	defer srv.Close()

	rp := newReporter(srv.URL, secret, "boot")
	rp.interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)
	rp.Publish(beat.Snapshot{Seq: 1})
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("요청이 오지 않았다")
	}
}

// POST 가 실패해도 Run 은 죽지 않는다. 부수 기능의 실패가 주 기능을 죽이면
// 안 된다 — internal/ledger 와 같은 원칙이다.
func TestReporterSurvivesFailure(t *testing.T) {
	rp := newReporter("http://127.0.0.1:1", []byte("s"), "boot") // 연결 거부
	rp.interval = 5 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	rp.Publish(beat.Snapshot{Seq: 1})
	rp.Run(ctx) // 패닉도 조기 반환도 없이 ctx 만료까지 돈다
	if rp.consecFail == 0 {
		t.Error("실패가 세어지지 않았다")
	}
}

// 같은 명령을 두 번 받아도 한 번만 흘린다. ack 전에 응답이 여러 번 오는 것은
// 정상이다(모니터는 봇이 ack 할 때까지 같은 명령을 답한다).
func TestCommandIsDeliveredOnce(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(beat.Reply{Command: beat.CmdShutdown, AckFor: 7})
	}))
	defer srv.Close()

	rp := newReporter(srv.URL, []byte("s"), "boot")
	rp.interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)
	rp.Publish(beat.Snapshot{Seq: 1})

	var got []beat.Command
	timeout := time.After(120 * time.Millisecond)
	for {
		select {
		case c := <-rp.Commands():
			got = append(got, c)
		case <-timeout:
			if len(got) != 1 {
				t.Errorf("명령 %d회 전달됐다, want 1", len(got))
			}
			return
		}
	}
}

// 알 수 없는 명령은 무시한다. 모니터가 오작동해 "restart" 를 보내도 봇은
// 자기가 아는 것만 한다.
func TestUnknownCommandIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"command":"restart","ack_for":1}`))
	}))
	defer srv.Close()

	rp := newReporter(srv.URL, []byte("s"), "boot")
	rp.interval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rp.Run(ctx)
	rp.Publish(beat.Snapshot{Seq: 1})

	select {
	case c := <-rp.Commands():
		t.Errorf("알 수 없는 명령 %q 가 전달됐다", c)
	case <-time.After(60 * time.Millisecond):
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91/ -run 'TestPublish|TestReporter|TestCommand|TestUnknown' -v`
Expected: FAIL — `newReporter` 미정의

- [ ] **Step 3: `internal/beat` 에 헤더 상수 추가**

`internal/beat/hmac.go` 에:

```go
// SigHeader 는 HMAC 서명이 실리는 헤더다.
const SigHeader = "X-Beat-Signature"
```

- [ ] **Step 4: 구현**

```go
package main

// 이 파일은 봇의 상태를 모니터로 밀어 보낸다.
//
// # 두 가지 제약이 이 파일의 전부다
//
//  1. **Publish 는 절대 블록하지 않는다.** exec 루프는 회차당 6,000 바퀴
//     (기본 50ms) 돌고, 그 안에서 네트워크를 기다리면 재호가가 밀린다.
//     밀린 재호가는 큐 위치를 잃는데, 최우선 호가에서 큐 위치는 체결을
//     지배한다. 그래서 Publish 는 포인터 하나를 원자적으로 바꾸고 끝난다.
//
//  2. **beat 실패는 거래를 멈추지 않는다.** internal/ledger 와 같은 원칙이다.
//     감시가 안 된다고 거래를 멈추면, 감시 장치의 장애가 거래 장애가 된다.
//     대신 실패를 세어 두었다가 사람에게 알린다(모니터가 죽으면 텔레그램이
//     조용해질 뿐이고, 그 침묵은 "이상 없음"과 구분되지 않는다).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

// defaultBeatInterval 은 beat 주기다. rule.StaleAfter(20초)의 6분의 1 —
// 여섯 번 연속 실패해야 모니터가 죽었다고 판정한다.
const defaultBeatInterval = 3 * time.Second

type reporter struct {
	endpoint string
	secret   []byte
	bootID   string
	interval time.Duration
	client   *http.Client

	// latest 는 가장 최근 스냅샷이다. 큐가 아닌 이유: 큐가 쌓이면 모니터가
	// 과거를 현재로 읽는다. 오래된 beat 는 보낼 가치가 없다.
	latest atomic.Pointer[beat.Snapshot]
	seq    atomic.Uint64
	cmds   chan beat.Command
	// lastAck 는 이미 흘려보낸 명령의 ack_for 다. 모니터는 봇이 ack 할 때까지
	// 같은 명령을 반복해 답하므로, 이것이 없으면 종료가 여러 번 시작된다.
	lastAck uint64
	// consecFail 은 연속 전송 실패 횟수다. 봇이 모니터의 사망을 아는 유일한 창구.
	consecFail int
}

func newReporter(endpoint string, secret []byte, bootID string) *reporter {
	return &reporter{
		endpoint: endpoint, secret: secret, bootID: bootID,
		interval: defaultBeatInterval,
		client:   &http.Client{Timeout: 2 * time.Second},
		cmds:     make(chan beat.Command, 4),
	}
}

func (r *reporter) Commands() <-chan beat.Command { return r.cmds }

// Publish 는 exec 루프가 부른다. 원자적 교체 하나가 전부다.
func (r *reporter) Publish(s beat.Snapshot) {
	s.BootID = r.bootID
	s.Seq = r.seq.Add(1)
	r.latest.Store(&s)
}

// Run 은 주기마다 최신 스냅샷을 보낸다. ctx 가 끝날 때까지 돈다.
func (r *reporter) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.send(ctx)
		}
	}
}

func (r *reporter) send(ctx context.Context) {
	s := r.latest.Load()
	if s == nil {
		return
	}
	snap := *s
	snap.TS = time.Now().UTC()
	body, err := json.Marshal(snap)
	if err != nil {
		r.consecFail++
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		r.consecFail++
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(beat.SigHeader, beat.Sign(r.secret, body))

	resp, err := r.client.Do(req)
	if err != nil {
		r.consecFail++
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil || resp.StatusCode != http.StatusOK {
		r.consecFail++
		return
	}
	r.consecFail = 0

	var reply beat.Reply
	if err := json.Unmarshal(raw, &reply); err != nil {
		return
	}
	// 알 수 없는 명령은 무시한다. 모니터가 오작동해도 봇은 자기가 아는 것만 한다.
	if !reply.Command.Valid() || reply.Command == beat.CmdNone {
		return
	}
	if reply.AckFor != 0 && reply.AckFor == r.lastAck {
		return // 이미 처리한 명령
	}
	select {
	case r.cmds <- reply.Command:
		r.lastAck = reply.AckFor
	default:
		// 채널이 찼다 = 앞선 명령을 아직 처리 중이다. 버린다 — 종료를
		// 두 번 시작하는 것보다 한 번 늦는 편이 낫다.
	}
}
```

- [ ] **Step 5: 통과 확인**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91/ -race -v`
Expected: PASS

- [ ] **Step 6: 커밋**

```bash
git add cmd/gld91/beat.go cmd/gld91/beat_test.go internal/beat/hmac.go
git commit -m "gld91: beat 리포터 — Publish 는 블록하지 않고 실패는 거래를 멈추지 않는다"
```

---

### Task 7: 봇 배선 — 스냅샷 조립·명령 소비·폴백 알림

Task 6 은 전송만 만들었다. **스냅샷을 조립하는 경로가 없고**(`exec.roundState` 는 unexported 라 봇이 자기 노출을 알려줄 수 없다), `Commands()` 를 소비하는 코드도, 모니터가 죽었을 때 봇이 알리는 경로도 없다.

**Files:**
- Modify: `internal/exec/exec.go` (관측 훅), `internal/exec/exec_test.go`
- Create: `cmd/gld91/snapshot.go`, `cmd/gld91/snapshot_test.go`

**Interfaces:**
- Consumes: `reporter` (Task 6), `beat.Snapshot`/`Round`/`Exposure`/`SkipReason` (Task 2), `risk.Exposure`, `live.Round`, `live.Frozen`
- Produces: `exec.Observation`, `Runner.Observe func(Observation)`, `func buildSnapshot(in snapshotInput) beat.Snapshot`, `type fallbackNotifier`

- [ ] **Step 1: `exec` 관측 훅의 실패 테스트**

`internal/exec/exec_test.go` 에 추가:

```go
// 봇이 자기 노출을 밖으로 알려주지 못하면 모니터는 노출 불변식을 감시할 수
// 없다. roundState 는 unexported 이고 그래야 한다 — 대신 값을 복사해 준다.
func TestRunnerObservesExposure(t *testing.T) {
	h := newHarness(t) // 기존 헬퍼 (exec_test.go:241)
	var got []Observation
	h.runner.Observe = func(o Observation) { got = append(got, o) }

	if err := h.run(); err != nil {
		t.Fatalf("RunRound: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("Observe 가 한 번도 불리지 않았다")
	}
	last := got[len(got)-1]
	if last.Exposure.Total() < 0 {
		t.Errorf("노출이 음수다: %+v", last.Exposure)
	}
	// 회차가 끝난 뒤의 마지막 관측은 깨끗해야 한다 — cancelEverything 이
	// 지나간 자리다. 여기 미체결이 남아 있으면 그것이 곧 사고다.
	if len(last.OpenIDs) != 0 {
		t.Errorf("회차 종료 뒤에도 미체결이 %d건 남았다: %v", len(last.OpenIDs), last.OpenIDs)
	}
}

// Observe 가 nil 이어도 회차는 정상이다. 관측은 부수 기능이고, 부수 기능의
// 부재가 거래를 막으면 안 된다.
func TestRunnerWorksWithoutObserve(t *testing.T) {
	h := newHarness(t)
	h.runner.Observe = nil
	if err := h.run(); err != nil {
		t.Fatalf("Observe 없이 실패했다: %v", err)
	}
}

// Observe 가 패닉해도 회차를 죽이지 않는다. 살아 있는 주문을 든 채 죽으면
// 취소도 못 한다 — exec 패키지 전체의 원칙이다.
func TestObservePanicDoesNotKillRound(t *testing.T) {
	h := newHarness(t)
	h.runner.Observe = func(Observation) { panic("관측자가 터졌다") }
	if err := h.run(); err != nil {
		t.Fatalf("관측자 패닉이 회차를 죽였다: %v", err)
	}
}

// 관측 훅이 회차 진행을 바꾸지 못한다. 설계서 §9 의 단방향 불변식을 반대
// 방향에서 지킨다 — 훅이 값을 돌려주면 언젠가 그 값이 판단에 쓰인다.
func TestObserveSignatureReturnsNothing(t *testing.T) {
	ft := reflect.TypeOf(Runner{}.Observe)
	if ft.NumOut() != 0 {
		t.Errorf("Observe 가 %d개를 돌려준다 — 관측 전용이어야 한다", ft.NumOut())
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./internal/exec/ -run 'TestRunnerObserves|TestRunnerWorksWithout|TestObservePanic' -v`
Expected: FAIL — `Observation` 미정의

- [ ] **Step 3: `exec` 에 훅 구현**

`internal/exec/exec.go` 의 타입 선언부에 추가. **`internal/beat` 를 임포트하지 않는다** — 이 패키지가 계약 타입을 알면 결합이 생기고, `beat` 가 바뀔 때 `exec` 의 테스트가 깨진다. 이미 쓰는 `risk.Exposure` 로 표현한다.

```go
// Observation 은 회차 한 바퀴의 우리 상태를 밖으로 복사한 것이다.
//
// roundState 를 그대로 내보내지 않는 이유: 그것은 포인터를 담고 있고, 밖에서
// 읽는 동안 루프가 고쳐 쓴다. 값 복사만 내보내면 경합이 없다.
//
// **이 훅은 관측 전용이다.** 여기서 돌려주는 값이 회차 진행에 영향을 주는
// 경로는 없다 — 있으면 모니터가 거래를 바꾸게 되고, 그것은 설계서 §9 의
// 단방향 불변식을 정반대로 깨는 일이다.
type Observation struct {
	Exposure      risk.Exposure
	OpenIDs       []string  // 지금 걸린 주문 + 취소 미확인
	OpenTicks     []int64   // OpenIDs 와 같은 순서
	OpenNotionals []float64 // OpenIDs 와 같은 순서
	Unaccounted   float64
	Reprices      int64
	LastRepriceAt time.Time
}

// observe 는 훅을 부른다. **패닉을 삼킨다** — 관측자가 터졌다고 살아 있는
// 주문을 든 채 죽으면 취소도 못 한다.
func (r *Runner) observe(st *roundState, reprices int64, lastReprice time.Time) {
	if r.Observe == nil {
		return
	}
	o := Observation{
		Exposure: st.exposure(), Unaccounted: st.unknownNotional,
		Reprices: reprices, LastRepriceAt: lastReprice,
	}
	appendOrder := func(od *openOrder) {
		o.OpenIDs = append(o.OpenIDs, od.id)
		o.OpenTicks = append(o.OpenTicks, od.tick)
		o.OpenNotionals = append(o.OpenNotionals, od.notional)
	}
	if st.live != nil {
		appendOrder(st.live)
	}
	for _, od := range st.pending {
		appendOrder(od)
	}
	defer func() { _ = recover() }()
	r.Observe(o)
}
```

`Runner` 구조체에 필드를 더한다:

```go
	// Observe 는 매 바퀴 우리 상태를 복사해 받는다. nil 이면 부르지 않는다.
	// 관측 전용이다 — 돌려주는 값이 없고 회차 진행에 영향을 주지 않는다.
	Observe func(Observation)
```

`loop` 의 각 바퀴 끝(다음 `Sleep` 직전)과 `RunRound` 의 `cancelEverything` 직후에 `r.observe(st, reprices, lastReprice)` 를 부른다. 회차 종료 뒤 한 번 더 부르는 것이 요점이다 — 그 마지막 관측이 "회차가 깨끗이 끝났는가"를 말한다.

- [ ] **Step 4: 스냅샷 조립의 실패 테스트**

`cmd/gld91/snapshot_test.go`:

```go
// 스킵 사유는 봇이 실제로 건너뛴 이유와 일치해야 한다. 여기가 틀리면
// 모니터의 분기 전체가 틀린 근거로 돈다.
func TestSnapshotSkipReasons(t *testing.T) {
	cases := []struct {
		name string
		in   snapshotInput
		want beat.SkipReason
	}{
		{"문턱 미달", snapshotInput{Frozen: live.Frozen{Eligible: false, Confidence: 0.001}, Equity: risk.Equity{AvailableUSDT: 200}}, beat.SkipConfBelow},
		{"표본 미채택", snapshotInput{SampleRejected: true, Equity: risk.Equity{AvailableUSDT: 200}}, beat.SkipSampleRejected},
		{"equity 부족", snapshotInput{Frozen: live.Frozen{Eligible: true}, Equity: risk.Equity{AvailableUSDT: 5}}, beat.SkipEquity},
		{"일손실", snapshotInput{Frozen: live.Frozen{Eligible: true}, Equity: risk.Equity{AvailableUSDT: 200}, DailyBreached: true}, beat.SkipDailyLimit},
		{"회차 조회 실패", snapshotInput{FetchErr: errors.New("x")}, beat.SkipFetchError},
		{"예측 실패", snapshotInput{PredictErr: errors.New("x")}, beat.SkipPredictError},
	}
	for _, c := range cases {
		got := buildSnapshot(c.in)
		if got.Round.State != beat.RoundSkipped {
			t.Errorf("%s: state = %q, want SKIPPED", c.name, got.Round.State)
		}
		if got.Round.SkipReason != c.want {
			t.Errorf("%s: reason = %q, want %q", c.name, got.Round.SkipReason, c.want)
		}
	}
}

// 심각한 사유가 우선한다. 일손실 한도에 걸렸는데 confidence 도 미달이면
// "문턱 미달"로 보고되어 조용해진다 — 정확히 반대로 읽혀야 한다.
func TestSnapshotSkipReasonPriority(t *testing.T) {
	in := snapshotInput{
		Frozen: live.Frozen{Eligible: false, Confidence: 0.001},
		Equity: risk.Equity{AvailableUSDT: 200}, DailyBreached: true,
	}
	if got := buildSnapshot(in).Round.SkipReason; got != beat.SkipDailyLimit {
		t.Errorf("reason = %q, want daily_limit — 심각한 쪽이 이겨야 한다", got)
	}
}

// 상수는 봇 패키지에서 그대로 온다. 리터럴을 적으면 상수를 바꿨을 때
// 모니터가 옛 값을 "정상"으로 확인해 준다.
func TestSnapshotConstsComeFromPackages(t *testing.T) {
	c := buildSnapshot(snapshotInput{}).Consts
	want := beat.Consts{
		CapFraction: risk.CapFraction, DailyFraction: risk.DefaultDailyFraction,
		ConfidenceThreshold: live.ConfidenceThreshold, MinOrderUSD: risk.MinOrderUSD,
	}
	if c != want {
		t.Errorf("consts = %+v, want %+v", c, want)
	}
}

// 관측이 미체결 목록으로 옮겨진다. 개인키 없는 모니터에게 이것이 유일한
// 정보원이므로 개수만 옮기면 안 된다.
func TestSnapshotCarriesOpenOrderList(t *testing.T) {
	in := snapshotInput{
		Frozen: live.Frozen{Eligible: true}, Equity: risk.Equity{AvailableUSDT: 200},
		Obs: exec.Observation{
			OpenIDs: []string{"0xa", "0xb"}, OpenTicks: []int64{487, 486},
			OpenNotionals: []float64{18.0, 4.5},
			Exposure:      risk.Exposure{OpenNotional: 22.5},
		},
	}
	got := buildSnapshot(in).Exposure.OpenOrders
	if len(got) != 2 || got[0].ID != "0xa" || got[0].Tick != 487 || got[1].Notional != 4.5 {
		t.Errorf("open_orders = %+v", got)
	}
}
```

- [ ] **Step 5: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91/ -run TestSnapshot -v`
Expected: FAIL — `buildSnapshot` 미정의

- [ ] **Step 6: `cmd/gld91/snapshot.go` 구현**

```go
package main

// 이 파일은 봇이 이미 들고 있는 값을 beat 스냅샷으로 옮긴다. **조회를 하지
// 않는다** — 레이트리밋 240 은 키 단위이고 봇이 그것을 독점해야 성립한다
// (스펙 §"예산은 키 단위다"). 감시를 위해 API 를 한 번 더 치는 순간 감시가
// 거래를 굶긴다.

import (
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
	"github.com/kdm000718/GLD-9.1/internal/exec"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// snapshotInput 은 조립에 필요한 전부다. 순수 함수로 두려고 구조체로 받는다.
type snapshotInput struct {
	Round          live.Round
	Frozen         live.Frozen
	Equity         risk.Equity
	Obs            exec.Observation
	Armed          bool
	Version        string
	SampleRejected bool
	DailyBreached  bool
	FetchErr       error
	PredictErr     error
	Active         bool // 지금 회차를 운용 중인가
	WSLastDataAt   time.Time
	FillsPollAt    time.Time
	RateRemaining  int
	Skips          map[beat.SkipReason]int
}

// buildSnapshot 은 순수 함수다. 시계도 네트워크도 타지 않는다 — TS 와 Seq 는
// reporter.Publish 가 채운다.
func buildSnapshot(in snapshotInput) beat.Snapshot {
	daily := risk.DailyLimit{}.Limit(in.Equity) // 아래 주 참고
	s := beat.Snapshot{
		Version: in.Version, Armed: in.Armed,
		Consts: beat.Consts{
			CapFraction:         risk.CapFraction,
			DailyFraction:       risk.DefaultDailyFraction,
			ConfidenceThreshold: live.ConfidenceThreshold,
			MinOrderUSD:         risk.MinOrderUSD,
		},
		Equity: beat.Equity{
			AvailableUSDT: in.Equity.AvailableUSDT, PositionCost: in.Equity.PositionCost,
			CapUSD: risk.Cap(in.Equity), CanArm: risk.CanArm(in.Equity),
			DailyLimit: daily,
		},
		Round: beat.Round{
			MarketID: in.Round.MarketID, Slug: in.Round.Slug, EndsAt: in.Round.EndsAt,
			PUp: in.Frozen.PUp, Confidence: in.Frozen.Confidence, Outcome: in.Frozen.Direction,
		},
		Exposure: beat.Exposure{
			Filled: in.Obs.Exposure.FilledNotional, Open: in.Obs.Exposure.OpenNotional,
			PendingCancel: in.Obs.Exposure.PendingCancel, Cap: risk.Cap(in.Equity),
			Unaccounted: in.Obs.Unaccounted,
		},
		Loop: beat.Loop{
			Reprices: in.Obs.Reprices, LastRepriceAt: in.Obs.LastRepriceAt,
			WSLastDataAt: in.WSLastDataAt, FillsPollAt: in.FillsPollAt,
			RateLimitRemaining: in.RateRemaining,
		},
		Skips: in.Skips,
	}
	for i, id := range in.Obs.OpenIDs {
		o := beat.OpenOrder{ID: id}
		if i < len(in.Obs.OpenTicks) {
			o.Tick = in.Obs.OpenTicks[i]
		}
		if i < len(in.Obs.OpenNotionals) {
			o.Notional = in.Obs.OpenNotionals[i]
		}
		s.Exposure.OpenOrders = append(s.Exposure.OpenOrders, o)
	}
	s.Round.State, s.Round.SkipReason = roundState(in)
	return s
}

// roundState 는 회차 상태와 스킵 사유를 정한다.
//
// **심각한 사유가 이긴다.** 일손실 한도에 걸린 회차는 confidence 도 미달일
// 수 있는데, 그때 "문턱 미달"로 보고하면 모니터가 조용해진다 — 정확히
// 반대로 읽혀야 하는 경우다. internal/risk 의 Action 이 심각한 것부터 보는
// 것과 같은 이유다.
func roundState(in snapshotInput) (beat.RoundState, beat.SkipReason) {
	switch {
	case in.PredictErr != nil:
		return beat.RoundSkipped, beat.SkipPredictError
	case in.FetchErr != nil:
		return beat.RoundSkipped, beat.SkipFetchError
	case in.DailyBreached:
		return beat.RoundSkipped, beat.SkipDailyLimit
	case !risk.CanArm(in.Equity):
		return beat.RoundSkipped, beat.SkipEquity
	case in.SampleRejected:
		return beat.RoundSkipped, beat.SkipSampleRejected
	case !in.Frozen.Eligible:
		return beat.RoundSkipped, beat.SkipConfBelow
	case in.Active:
		return beat.RoundActive, ""
	}
	return beat.RoundIdle, ""
}
```

> `risk.DailyLimit` 에 한도를 값으로 돌려주는 메서드가 없으면(`Breached` 만 있다면) `in.Equity.Total() * -risk.DefaultDailyFraction` 로 계산하고 그 사실을 주석에 적는다. 배선에서 리터럴 `0.10` 을 쓰지 않는 것이 요점이다.

- [ ] **Step 7: 명령 소비와 폴백 알림의 실패 테스트**

```go
// 종료 명령이 오면 신규 회차 진입을 멈춘다. 이 봇은 매도를 내지 않으므로
// 청산할 것이 없고, 미정산 포지션은 정산에 맡긴다.
func TestShutdownStopsNewRounds(t *testing.T) {
	g := &runGate{}
	g.apply(beat.CmdShutdown)
	if g.MayEnterRound() {
		t.Error("종료 명령 뒤에 신규 회차에 진입한다")
	}
	if !g.ShouldExit() {
		t.Error("종료 명령 뒤에 종료하지 않는다")
	}
}

// halt 는 재기동해도 유지되는 래치다. 종료와 달리 "다시 켜지 마라"는 뜻이다.
func TestHaltIsSticky(t *testing.T) {
	g := &runGate{}
	g.apply(beat.CmdHalt)
	g.apply(beat.CmdNone)
	if g.MayEnterRound() {
		t.Error("halt 가 none 으로 풀렸다")
	}
}

// 모니터가 죽으면 텔레그램이 조용해진다. 그 침묵은 "이상 없음"과 구분되지
// 않으므로, 봇이 직접 한 번 알린다. GLD-7 에는 이 경로가 없었다.
func TestFallbackNotifiesOnceThenRecovers(t *testing.T) {
	var msgs []string
	fb := &fallbackNotifier{
		threshold: 20 * time.Second,
		send:      func(m string) error { msgs = append(msgs, m); return nil },
	}
	now := time.Unix(1786000000, 0).UTC()
	fb.Step(0, now)                        // 정상
	fb.Step(7, now.Add(21*time.Second))    // 21초째 실패 → 1회 알림
	fb.Step(9, now.Add(30*time.Second))    // 계속 실패 → 조용
	fb.Step(0, now.Add(40*time.Second))    // 복구 → 1회 알림
	fb.Step(0, now.Add(50*time.Second))    // 조용
	if len(msgs) != 2 {
		t.Fatalf("알림 %d회, want 2: %v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0], "모니터") || !strings.Contains(msgs[1], "복구") {
		t.Errorf("메시지가 부정확하다: %v", msgs)
	}
}

// 알림 전송이 실패해도 봇은 죽지 않는다.
func TestFallbackSurvivesSendFailure(t *testing.T) {
	fb := &fallbackNotifier{
		threshold: time.Second,
		send:      func(string) error { return errors.New("텔레그램 다운") },
	}
	now := time.Unix(1786000000, 0).UTC()
	fb.Step(5, now)
	fb.Step(5, now.Add(2*time.Second)) // 패닉하지 않는다
}
```

- [ ] **Step 8: 구현**

```go
// runGate 는 모니터 명령이 회차 진입에 미치는 영향의 전부다.
//
// **이 봇은 매도를 내지 않는다**(스펙 §10). 그래서 종료는 "청산 후 종료"가
// 아니라 "신규 진입 중단 + 미체결 취소 + 정산에 맡기고 종료"다. 미체결
// 취소는 exec 의 cancelEverything 이 회차 끝에서 이미 한다.
type runGate struct {
	halted bool
	exit   bool
}

func (g *runGate) apply(c beat.Command) {
	switch c {
	case beat.CmdShutdown:
		g.exit = true
	case beat.CmdHalt:
		// **래치다.** 재기동해도 유지되어야 "다시 켜지 마라"가 성립한다.
		// 모니터가 첫 beat 에 다시 halt 를 내려 주므로 파일에 쓸 필요가 없다.
		g.halted, g.exit = true, true
	}
}

func (g *runGate) MayEnterRound() bool { return !g.halted && !g.exit }
func (g *runGate) ShouldExit() bool    { return g.exit }

// fallbackNotifier 는 모니터가 죽었을 때 봇이 직접 알리는 경로다.
//
// GLD-7 은 B서버가 죽으면 아무도 모른다 — 텔레그램의 침묵이 "이상 없음"과
// 구분되지 않는다. 봇은 POST 실패를 직접 아니까 그것을 알릴 수 있고,
// 텔레그램 토큰은 자금 권한과 무관하므로 "모니터는 개인키를 갖지 않는다"는
// 결정과 충돌하지 않는다.
type fallbackNotifier struct {
	threshold time.Duration
	send      func(string) error
	interval  time.Duration // beat 주기. 0 이면 defaultBeatInterval

	firstFail time.Time
	notified  bool
}

// Step 은 연속 실패 횟수를 받아 필요하면 알린다. 순수에 가깝다 — 시각을
// 인자로 받고, 부수효과는 send 하나다.
func (f *fallbackNotifier) Step(consecFail int, now time.Time) {
	iv := f.interval
	if iv == 0 {
		iv = defaultBeatInterval
	}
	if consecFail == 0 {
		if f.notified {
			f.emit("✅ 모니터 연결이 복구됐습니다.")
		}
		f.firstFail, f.notified = time.Time{}, false
		return
	}
	if f.firstFail.IsZero() {
		f.firstFail = now.Add(-time.Duration(consecFail) * iv)
	}
	if !f.notified && now.Sub(f.firstFail) >= f.threshold {
		f.emit("⚠️ 모니터가 응답하지 않습니다 — 봇은 계속 거래 중이지만 감시가 없습니다.")
		f.notified = true
	}
}

// emit 은 전송 실패를 삼킨다. 알림이 안 나갔다고 거래를 멈추면, 감시 장치의
// 장애가 거래 장애가 된다.
func (f *fallbackNotifier) emit(msg string) {
	if f.send == nil {
		return
	}
	_ = f.send(msg)
}
```

`main.go` 의 회차 루프에 배선한다:

```go
	rp := newReporter(cfg.BeatEndpoint, cfg.BeatSecret, bootID)
	go rp.Run(ctx)

	gate := &runGate{}
	fb := &fallbackNotifier{threshold: 60 * time.Second, send: botTelegram.Send}

	// 명령 소비. 회차 루프를 블록하지 않게 별도 고루틴에서 게이트만 바꾼다.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-rp.Commands():
				gate.apply(c)
				log.Printf("모니터 명령: %s", c)
			}
		}
	}()

	// 폴백 알림 티커.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				fb.Step(rp.ConsecFail(), time.Now())
			}
		}
	}()
```

회차 루프는 매 바퀴 `if !gate.MayEnterRound() { break }` 로 진입을 막고, `runner.Observe` 를 `rp.Publish(buildSnapshot(...))` 에 연결한다.

> `runGate` 는 두 고루틴이 읽고 쓴다. 뮤텍스나 `atomic.Bool` 두 개로 감싼다 — `go test -race` 가 잡는다.

- [ ] **Step 9: 통과 확인**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91/ ./internal/exec/ -race -v`
Expected: PASS

- [ ] **Step 10: 커밋**

```bash
git add internal/exec/exec.go internal/exec/exec_test.go cmd/gld91/snapshot.go cmd/gld91/snapshot_test.go cmd/gld91/main.go
git commit -m "gld91: 스냅샷 조립·명령 소비·모니터 사망 폴백

exec 에 관측 훅을 뒀다 — roundState 는 unexported 로 두되 값 복사를
내보낸다. 훅은 관측 전용이고 패닉을 삼킨다(살아 있는 주문을 든 채 죽으면
취소도 못 한다). 스킵 사유는 심각한 쪽이 이긴다 — 일손실 한도에 걸린
회차를 'confidence 미달'로 보고하면 모니터가 조용해진다."
```

---

### Task 8: 모니터 — beat 수신 서버

**Files:**
- Create: `cmd/gld91-monitor/server.go`, `cmd/gld91-monitor/server_test.go`

**Interfaces:**
- Consumes: `beat.Snapshot`, `beat.Reply`, `beat.Verify`, `beat.Gate` (Task 2·3)
- Produces: `type state struct`, `newState(want beat.Consts, secret []byte) *state`, `(*state) handleBeat(w http.ResponseWriter, r *http.Request)`, `(*state) Pending() beat.Command`, `(*state) SetPending(beat.Command)`, `(*state) Latest() (*beat.Snapshot, time.Time)`

- [ ] **Step 1: 실패 테스트**

```go
// 서명이 없거나 틀리면 404 다. 401 은 "여기 뭔가 있다"를 알려준다 —
// 공인 엔드포인트는 스캐너가 먼저 찾는다.
func TestUnsignedRequestIs404(t *testing.T) {
	st := newState(beat.Consts{}, []byte("s3cret"))
	for _, sig := range []string{"", "deadbeef", beat.Sign([]byte("wrong"), []byte("{}"))} {
		req := httptest.NewRequest(http.MethodPost, "/beat", strings.NewReader("{}"))
		if sig != "" {
			req.Header.Set(beat.SigHeader, sig)
		}
		w := httptest.NewRecorder()
		st.handleBeat(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("서명 %q 에 %d, want 404", sig, w.Code)
		}
	}
}

func TestValidBeatAccepted(t *testing.T) {
	secret := []byte("s3cret")
	st := newState(beat.Consts{}, secret)
	snap := beat.Snapshot{Seq: 1, BootID: "a", TS: time.Now().UTC()}
	body, _ := json.Marshal(snap)

	req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
	req.Header.Set(beat.SigHeader, beat.Sign(secret, body))
	w := httptest.NewRecorder()
	st.handleBeat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var reply beat.Reply
	json.Unmarshal(w.Body.Bytes(), &reply)
	if reply.Command != beat.CmdNone {
		t.Errorf("command = %q, want none", reply.Command)
	}
	if got, _ := st.Latest(); got == nil || got.Seq != 1 {
		t.Error("스냅샷이 저장되지 않았다")
	}
}

// 재전송은 거부하되, 봇 재시작(BootID 변화)은 seq 가 1 로 돌아가는 것이
// 정상이므로 받아들인다. 이것을 막으면 재시작 뒤 모든 beat 가 거부되어
// 모니터가 봇을 영원히 죽은 것으로 본다.
func TestReplayRejectedButRestartAccepted(t *testing.T) {
	secret := []byte("s")
	st := newState(beat.Consts{}, secret)
	post := func(s beat.Snapshot) int {
		body, _ := json.Marshal(s)
		req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
		req.Header.Set(beat.SigHeader, beat.Sign(secret, body))
		w := httptest.NewRecorder()
		st.handleBeat(w, req)
		return w.Code
	}
	now := time.Now().UTC()
	post(beat.Snapshot{Seq: 10, BootID: "a", TS: now})
	if code := post(beat.Snapshot{Seq: 10, BootID: "a", TS: now}); code != http.StatusConflict {
		t.Errorf("재전송에 %d, want 409", code)
	}
	if code := post(beat.Snapshot{Seq: 1, BootID: "b", TS: now}); code != http.StatusOK {
		t.Errorf("재시작 뒤 seq 1 에 %d, want 200", code)
	}
}

// 명령은 봇이 ack 할 때까지 반복해서 실린다. 한 번만 싣고 그 응답이 유실되면
// 종료 요청이 조용히 사라진다.
func TestPendingCommandRepeatsUntilAck(t *testing.T) {
	secret := []byte("s")
	st := newState(beat.Consts{}, secret)
	st.SetPending(beat.CmdShutdown)
	post := func(seq uint64) beat.Reply {
		body, _ := json.Marshal(beat.Snapshot{Seq: seq, BootID: "a", TS: time.Now().UTC()})
		req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
		req.Header.Set(beat.SigHeader, beat.Sign(secret, body))
		w := httptest.NewRecorder()
		st.handleBeat(w, req)
		var r beat.Reply
		json.Unmarshal(w.Body.Bytes(), &r)
		return r
	}
	for seq := uint64(1); seq <= 3; seq++ {
		if r := post(seq); r.Command != beat.CmdShutdown {
			t.Errorf("seq %d 응답이 %q 다", seq, r.Command)
		}
	}
}

// 본문 크기 상한. 공인 엔드포인트에 무한 본문을 보내면 메모리가 나간다.
func TestOversizedBodyRejected(t *testing.T) {
	st := newState(beat.Consts{}, []byte("s"))
	req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(make([]byte, maxBeatBody+1)))
	req.Header.Set(beat.SigHeader, "x")
	w := httptest.NewRecorder()
	st.handleBeat(w, req)
	if w.Code == http.StatusOK {
		t.Error("과대 본문이 통과했다")
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91-monitor/ -v`
Expected: FAIL — 패키지 없음

- [ ] **Step 3: 구현**

```go
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
	"github.com/kdm000718/GLD-9.1/internal/beat/rule"
)

// maxBeatBody 는 본문 상한이다. 공인 엔드포인트이므로 상한 없이 읽으면
// 아무나 메모리를 채울 수 있다. 실제 스냅샷은 미체결 목록을 다 실어도 수 KB다.
const maxBeatBody = 64 << 10

// state 는 모니터가 아는 전부다.
type state struct {
	want   beat.Consts
	secret []byte

	mu       sync.Mutex
	latest   *beat.Snapshot
	lastBeat time.Time
	bootID   string
	gate     beat.Gate
	pending  beat.Command
	// bootChanges 는 최근 재시작 시각들이다. 크래시루프 판정에 쓴다.
	bootChanges []time.Time
	latch       *rule.Latch
	consecSkips map[beat.SkipReason]int
}

func newState(want beat.Consts, secret []byte) *state {
	return &state{
		want: want, secret: secret,
		gate:        beat.Gate{Skew: time.Minute},
		latch:       rule.NewLatch(30 * time.Second),
		consecSkips: map[beat.SkipReason]int{},
	}
}

func (s *state) Latest() (*beat.Snapshot, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest, s.lastBeat
}

func (s *state) SetPending(c beat.Command) {
	s.mu.Lock()
	s.pending = c
	s.mu.Unlock()
}

func (s *state) Pending() beat.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == "" {
		return beat.CmdNone
	}
	return s.pending
}

// handleBeat 은 beat 한 건을 받는다.
//
// **서명 실패는 404 다.** 401 은 "여기 뭔가 있다" 를 알려주고, 이 엔드포인트는
// 공인이라 스캐너가 먼저 찾는다. 존재하지 않는 것처럼 보이는 편이 낫다.
func (s *state) handleBeat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBeatBody+1))
	if err != nil || len(body) > maxBeatBody {
		http.NotFound(w, r)
		return
	}
	if !beat.Verify(s.secret, body, r.Header.Get(beat.SigHeader)) {
		http.NotFound(w, r)
		return
	}
	var snap beat.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		http.NotFound(w, r)
		return
	}

	now := time.Now().UTC()
	s.mu.Lock()
	// 봇이 재시작하면 seq 가 1 로 돌아간다. 그것을 재전송으로 막으면
	// 재시작 뒤 모든 beat 가 거부되어 모니터가 스스로 눈을 감는다.
	if snap.BootID != s.bootID {
		if s.bootID != "" {
			s.bootChanges = append(s.bootChanges, now)
		}
		s.bootID = snap.BootID
		s.gate.Reset()
	}
	admitErr := s.gate.Admit(snap.Seq, snap.TS, now)
	if admitErr == nil {
		s.latest, s.lastBeat = &snap, now
		s.trackSkipLocked(snap)
	}
	cmd := s.pending
	s.mu.Unlock()

	if admitErr != nil {
		http.Error(w, "", http.StatusConflict)
		return
	}
	if cmd == "" {
		cmd = beat.CmdNone
	}
	w.Header().Set("Content-Type", "application/json")
	// **명령은 ack 될 때까지 반복해서 싣는다.** 한 번만 싣고 그 응답이
	// 유실되면 종료 요청이 조용히 사라진다. 봇 쪽 lastAck 이 중복 처리를 막는다.
	json.NewEncoder(w).Encode(beat.Reply{Command: cmd, AckFor: snap.Seq})
}

// trackSkipLocked 은 연속 스킵을 센다. 사유가 바뀌면 리셋한다 — 서로 다른
// 사유가 번갈아 나는 것은 "같은 문제가 지속" 이 아니다.
func (s *state) trackSkipLocked(snap beat.Snapshot) {
	if snap.Round.State != beat.RoundSkipped {
		s.consecSkips = map[beat.SkipReason]int{}
		return
	}
	r := snap.Round.SkipReason
	n := s.consecSkips[r]
	s.consecSkips = map[beat.SkipReason]int{r: n + 1}
}

// bootChangesSince 는 그 시각 이후 재시작 횟수다.
func (s *state) bootChangesSince(t time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.bootChanges {
		if c.After(t) {
			n++
		}
	}
	return n
}
```

- [ ] **Step 4: 통과 확인**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91-monitor/ -race -v`
Expected: PASS (5개)

- [ ] **Step 5: 커밋**

```bash
git add cmd/gld91-monitor/
git commit -m "monitor: beat 수신 — 서명 실패는 404, 재시작은 재전송이 아니다"
```

---

### Task 9: 모니터 — 텔레그램·명령·배선

**Files:**
- Create: `cmd/gld91-monitor/telegram.go`, `cmd/gld91-monitor/main.go`, `cmd/gld91-monitor/config.go`, `cmd/gld91-monitor/config_test.go`, `cmd/gld91-monitor/telegram_test.go`

**Interfaces:**
- Consumes: `state` (Task 8), `rule.Evaluate`, `rule.Latch` (Task 4·5)
- Produces: `monitorConfig`, `loadMonitorConfig(getenv func(string) string) (monitorConfig, error)`, `tgClient`, `routeCommand(text string, s *state) (reply string, handled bool)`

- [ ] **Step 1: 실패 테스트**

```go
// 모니터가 개인키를 읽으면 설계 전체가 무의미하다. 소스 스캔으로 막는다 —
// 런타임 가드는 잊히지만 이 테스트는 잊히지 않는다.
func TestMonitorNeverReadsPrivateKey(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("WALLET_PRIVATE_KEY")) {
			t.Errorf("%s 가 개인키 환경변수를 참조한다 — 모니터는 서명하지 않는다", f)
		}
	}
}

// 봇과 같은 API 키면 기동하지 않는다. 스펙 §"예산은 키 단위다" — 같은 키를
// 쓴 프로세스들이 14초 만에 240 을 소진시킨 실측이 있다.
func TestMonitorRejectsSharedAPIKey(t *testing.T) {
	env := map[string]string{
		"MONITOR_BEAT_SECRET": "s", "MONITOR_LISTEN": ":8443",
		"TELEGRAM_BOT_TOKEN": "t", "TELEGRAM_CHAT_ID": "42",
		"MONITOR_API_KEY": "same", "PREDICT_API_KEY": "same",
	}
	if _, err := loadMonitorConfig(func(k string) string { return env[k] }); err == nil {
		t.Error("봇과 같은 API 키인데 기동이 허용됐다")
	}
	env["MONITOR_API_KEY"] = "different"
	if _, err := loadMonitorConfig(func(k string) string { return env[k] }); err != nil {
		t.Errorf("다른 키인데 거부됐다: %v", err)
	}
}

func TestMonitorRequiresSecrets(t *testing.T) {
	full := map[string]string{
		"MONITOR_BEAT_SECRET": "s", "MONITOR_LISTEN": ":8443",
		"TELEGRAM_BOT_TOKEN": "t", "TELEGRAM_CHAT_ID": "42", "MONITOR_API_KEY": "k",
	}
	for _, drop := range []string{"MONITOR_BEAT_SECRET", "TELEGRAM_BOT_TOKEN", "TELEGRAM_CHAT_ID", "MONITOR_API_KEY"} {
		env := map[string]string{}
		for k, v := range full {
			env[k] = v
		}
		delete(env, drop)
		if _, err := loadMonitorConfig(func(k string) string { return env[k] }); err == nil {
			t.Errorf("%s 없이 기동이 허용됐다", drop)
		}
	}
}

// ack 된 뒤에는 종료를 되돌릴 수 없다. 파일 플래그라면 지우면 그만이지만
// 명령은 이미 전달됐다. 조용히 실패하지 말고 명시적으로 거절한다.
func TestCancelShutdownAfterAckIsRefused(t *testing.T) {
	s := newState(beat.Consts{}, []byte("x"))
	s.SetPending(beat.CmdShutdown)
	s.markAcked()
	reply, handled := routeCommand("/cancel_shutdown", s)
	if !handled {
		t.Fatal("명령이 처리되지 않았다")
	}
	if !strings.Contains(reply, "재기동") {
		t.Errorf("거절 사유가 불명확하다: %q", reply)
	}
	if s.Pending() != beat.CmdShutdown {
		t.Error("ack 된 종료가 취소됐다")
	}
}

func TestCancelShutdownBeforeAckWorks(t *testing.T) {
	s := newState(beat.Consts{}, []byte("x"))
	s.SetPending(beat.CmdShutdown)
	if _, handled := routeCommand("/cancel_shutdown", s); !handled {
		t.Fatal("처리되지 않았다")
	}
	if s.Pending() != beat.CmdNone {
		t.Errorf("pending = %q, want none", s.Pending())
	}
}

// /why 는 이 봇에서 가장 자주 나올 질문에 답한다. GLD-7 에는 그 명령이 없어
// 매번 SSH 로 로그를 봐야 했다.
func TestWhyReportsSkipReason(t *testing.T) {
	s := newState(beat.Consts{}, []byte("x"))
	s.mu.Lock()
	s.latest = &beat.Snapshot{
		Round: beat.Round{State: beat.RoundSkipped, SkipReason: beat.SkipConfBelow, Confidence: 0.0031},
	}
	s.consecSkips = map[beat.SkipReason]int{beat.SkipConfBelow: 12}
	s.mu.Unlock()

	reply, handled := routeCommand("/why", s)
	if !handled {
		t.Fatal("처리되지 않았다")
	}
	for _, want := range []string{"conf_below", "0.0031", "12"} {
		if !strings.Contains(reply, want) {
			t.Errorf("응답에 %q 가 없다: %q", want, reply)
		}
	}
}

func TestUnauthorizedChatRejected(t *testing.T) {
	if _, handled := routeCommand("/status", nil); handled {
		t.Error("nil 상태에서 명령이 처리됐다")
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91-monitor/ -run 'TestMonitor|TestCancel|TestWhy|TestUnauthorized' -v`
Expected: FAIL — `loadMonitorConfig` 미정의

- [ ] **Step 3: `state` 에 ack 추적 추가**

`server.go` 의 `state` 에 필드와 메서드를 더한다:

```go
	// acked 는 봇이 pending 명령을 받아 갔다는 뜻이다. 그 뒤에는 되돌릴 수 없다.
	acked bool
```

`handleBeat` 의 `cmd := s.pending` 뒤에 `if cmd != "" && cmd != beat.CmdNone { s.acked = true }` 를 넣고, `SetPending` 에서 `s.acked = false` 로 되돌린다. 그리고:

```go
func (s *state) markAcked() { s.mu.Lock(); s.acked = true; s.mu.Unlock() }
func (s *state) Acked() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.acked }
```

- [ ] **Step 4: `config.go` 구현**

```go
package main

import (
	"fmt"
	"strconv"
	"strings"
)

// 환경변수. 개인키는 여기 없다 — 모니터는 서명하지 않는다.
const (
	EnvBeatSecret = "MONITOR_BEAT_SECRET"
	EnvListen     = "MONITOR_LISTEN"
	EnvTGToken    = "TELEGRAM_BOT_TOKEN"
	EnvTGChat     = "TELEGRAM_CHAT_ID"
	EnvMonAPIKey  = "MONITOR_API_KEY"
	EnvBotAPIKey  = "PREDICT_API_KEY" // 같은 값 금지 확인용으로만 읽는다
)

type monitorConfig struct {
	BeatSecret []byte
	Listen     string
	TGToken    string
	TGChat     int64
	APIKey     string
}

func loadMonitorConfig(getenv func(string) string) (monitorConfig, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	c := monitorConfig{
		BeatSecret: []byte(get(EnvBeatSecret)),
		Listen:     get(EnvListen),
		TGToken:    get(EnvTGToken),
		APIKey:     get(EnvMonAPIKey),
	}
	var missing []string
	for k, v := range map[string]string{
		EnvBeatSecret: get(EnvBeatSecret), EnvTGToken: c.TGToken,
		EnvTGChat: get(EnvTGChat), EnvMonAPIKey: c.APIKey,
	} {
		if v == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return monitorConfig{}, fmt.Errorf("환경변수가 없다: %s", strings.Join(missing, ", "))
	}
	if c.Listen == "" {
		c.Listen = ":8443"
	}
	chat, err := strconv.ParseInt(get(EnvTGChat), 10, 64)
	if err != nil {
		return monitorConfig{}, fmt.Errorf("%s 가 정수가 아니다: %w", EnvTGChat, err)
	}
	c.TGChat = chat

	// 스펙 §"예산은 키 단위다": 같은 키를 쓴 프로세스들이 창이 열린 뒤 14초
	// 만에 240 을 소진시켜 봇 몫이 0 이 된 실측이 있다. 감시 장치가 감시
	// 대상을 죽이는 형태의 고장이고, 그 고장은 조용하다.
	if bot := get(EnvBotAPIKey); bot != "" && bot == c.APIKey {
		return monitorConfig{}, fmt.Errorf(
			"%s 가 봇의 %s 와 같다 — 레이트리밋 240 은 키 단위이므로 모니터가 봇의 예산을 먹는다",
			EnvMonAPIKey, EnvBotAPIKey)
	}
	return c, nil
}
```

- [ ] **Step 5: `telegram.go` 의 명령 라우팅 구현**

```go
// routeCommand 는 텔레그램 명령 하나를 처리한다. 순수에 가깝다 — state 만
// 읽고 쓰며 네트워크를 타지 않으므로 값으로 시험된다.
func routeCommand(text string, s *state) (string, bool) {
	if s == nil {
		return "", false
	}
	switch strings.Fields(strings.TrimSpace(text))[0] {
	case "/status":
		return formatStatus(s), true
	case "/why":
		return formatWhy(s), true
	case "/shutdown":
		s.SetPending(beat.CmdShutdown)
		return "🛑 종료 요청됨. 봇이 신규 회차 진입을 멈추고 미체결을 전량 취소합니다.", true
	case "/halt":
		s.SetPending(beat.CmdHalt)
		return "⛔ halt 걸림. 재기동해도 즉시 다시 종료됩니다. /resume 으로 풉니다.", true
	case "/resume":
		s.SetPending(beat.CmdNone)
		return "▶️ halt 해제됨.", true
	case "/cancel_shutdown":
		// **ack 된 뒤에는 되돌릴 수 없다.** 파일 플래그라면 지우면 그만이지만
		// 명령은 이미 봇에게 전달됐다. 조용히 실패시키면 사용자는 취소된 줄 안다.
		if s.Acked() {
			return "⚠️ 이미 봇이 종료 명령을 받아 갔습니다 — 되돌릴 수 없습니다. 계속하려면 재기동하세요.", true
		}
		s.SetPending(beat.CmdNone)
		return "↩️ 종료 요청 취소됨 (봇이 아직 받아 가기 전).", true
	case "/help":
		return "📖 /status /why /pnl /shutdown /cancel_shutdown /halt /resume /help", true
	}
	return "", false
}

func formatWhy(s *state) string {
	snap, _ := s.Latest()
	if snap == nil {
		return "아직 beat 를 받지 못했습니다."
	}
	s.mu.Lock()
	consec := map[beat.SkipReason]int{}
	for k, v := range s.consecSkips {
		consec[k] = v
	}
	s.mu.Unlock()

	if snap.Round.State != beat.RoundSkipped {
		return fmt.Sprintf("지금 회차 %s 참여 중 (p_up %.4f, confidence %.4f)",
			snap.Round.Slug, snap.Round.PUp, snap.Round.Confidence)
	}
	r := snap.Round.SkipReason
	note := ""
	if r == beat.SkipConfBelow {
		note = "  ← 정상입니다. 문턱 미달은 아무것도 하지 않는 것이 옳습니다."
	}
	return fmt.Sprintf("건너뜀: %s (연속 %d회차)\nconfidence %.4f < %.4f%s",
		r, consec[r], snap.Round.Confidence, snap.Consts.ConfidenceThreshold, note)
}
```

같은 파일에 텔레그램 클라이언트를 둔다:

```go
// tgClient 는 텔레그램 봇 API 다. 라이브러리를 쓰지 않는다 — 필요한 것이
// send 와 getUpdates 둘뿐이고, 새 의존성은 이 저장소가 늘리지 않는 것이다.
type tgClient struct {
	token  string
	chatID int64
	http   *http.Client
}

func newTG(token string, chatID int64) *tgClient {
	// getUpdates 를 30초 롱폴하므로 클라이언트 시한이 그보다 넉넉해야 한다.
	return &tgClient{token: token, chatID: chatID, http: &http.Client{Timeout: 45 * time.Second}}
}

// Send 는 인가된 채팅에만 보낸다. 실패는 로그로만 남긴다 — 알림이 안 나갔다고
// 모니터가 죽으면 그 뒤의 모든 알림도 사라진다.
func (c *tgClient) Send(msg string) error {
	body, _ := json.Marshal(map[string]any{"chat_id": c.chatID, "text": msg})
	resp, err := c.http.Post(
		"https://api.telegram.org/bot"+c.token+"/sendMessage",
		"application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("telegram send: %v", err)
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: %s", resp.Status)
	}
	return nil
}

// Poll 은 명령을 롱폴한다.
//
// 두 가지가 중요하다. **기동 이전 메시지를 무시한다** — 모니터를 재시작할
// 때마다 밀린 /shutdown 이 되살아나면 안 된다. 그리고 **인가되지 않은 채팅은
// 거부한다** — 봇 토큰을 아는 누구나 말을 걸 수 있다.
func (c *tgClient) Poll(ctx context.Context, st *state) {
	var offset int64
	startup := time.Now().Unix()
	for ctx.Err() == nil {
		updates, err := c.getUpdates(ctx, offset, 30)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("getUpdates: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Message == nil || u.Message.Date < startup {
				continue
			}
			if u.Message.Chat.ID != c.chatID {
				c.sendTo(u.Message.Chat.ID, "⛔ 인가되지 않은 사용자입니다.")
				continue
			}
			// 폴 루프 밖에서 처리한다. /pnl 은 정산 조회를 여러 번 하므로
			// 여기서 기다리면 긴급한 /shutdown 이 그 뒤에 밀린다.
			go func(text string) {
				reply, handled := routeCommand(text, st)
				if !handled {
					reply = "📖 /status /why /pnl /shutdown /cancel_shutdown /halt /resume /help"
				}
				c.Send(reply)
			}(u.Message.Text)
		}
	}
}
```

`getUpdates`·`sendTo`·업데이트 JSON 타입(`UpdateID`, `Message{Date, Text, Chat{ID}}`)은 텔레그램 Bot API 문서의 최소 부분집합만 정의한다. `formatStatus` 는 `state.Latest()` 의 스냅샷을 설계서 §7 형식으로 찍되, **beat 를 아직 못 받았으면 그렇게 말한다** — 빈 값을 0 으로 찍으면 "equity 0" 이 진짜 잔고로 읽힌다.

- [ ] **Step 6: `main.go` 배선**

`state` 를 만들고 `POST /beat` 를 붙이고, 세 고루틴을 띄운다: 판정 티커(3초, `rule.Evaluate` → `latch.Step` → 알람 전송), 텔레그램 롱폴, 4시간 리포트 티커.

```go
func main() {
	cfg, err := loadMonitorConfig(os.Getenv)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	want := beat.Consts{
		CapFraction:         risk.CapFraction,
		DailyFraction:       risk.DefaultDailyFraction,
		ConfidenceThreshold: live.ConfidenceThreshold,
		MinOrderUSD:         risk.MinOrderUSD,
	}
	st := newState(want, cfg.BeatSecret)
	tg := newTG(cfg.TGToken, cfg.TGChat)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 판정 티커. beat 주기와 같이 3초 — 판정 자체는 순수 함수라 싸다.
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				now := time.Now().UTC()
				snap, last := st.Latest()
				st.mu.Lock()
				consec := maps.Clone(st.consecSkips)
				st.mu.Unlock()
				fs := rule.Evaluate(rule.Input{
					Snap: snap, LastBeat: last, Now: now, Want: want,
					ConsecSkips: consec,
					BootChanges: st.bootChangesSince(now.Add(-10 * time.Minute)),
				})
				fire, resolved := st.latch.Step(fs, now)
				for _, f := range fire {
					tg.Send(f.Level.String() + " " + f.Message)
				}
				sort.Strings(resolved)
				for _, key := range resolved {
					tg.Send("✅ 복구: " + key)
				}
			}
		}
	}()

	go tg.Poll(ctx, st)       // 텔레그램 롱폴 → routeCommand
	go runReportTicker(ctx, st, tg, 4*time.Hour)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           http.HandlerFunc(st.handleBeat),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { <-ctx.Done(); srv.Shutdown(context.Background()) }()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("listen: %v", err)
	}
}
```

> 핸들러를 `/beat` 경로로 좁히지 않고 전부 받는 것이 의도다. 서명 없는 요청은
> 어느 경로든 404 이므로, 경로 자체가 정보를 주지 않는다.

> `want` 를 봇과 같은 상수에서 가져오는 것이 요점이다. 리터럴 `0.0455` 를 적으면 봇 상수를 바꿨을 때 모니터가 조용히 틀린 값을 기대한다.

- [ ] **Step 7: 통과 확인**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91-monitor/ -race -v && GOTOOLCHAIN=local go vet ./...`
Expected: PASS

- [ ] **Step 8: 커밋**

```bash
git add cmd/gld91-monitor/
git commit -m "monitor: 텔레그램·명령·배선

모니터는 개인키를 읽지 않고(소스 스캔으로 고정), 봇과 같은 API 키면
기동하지 않는다. ack 된 종료는 되돌릴 수 없으므로 명시적으로 거절한다."
```

---

### Task 10: 모니터 — 정산 관측과 리포트

설계서 §9. 봇은 자기 손익을 모른다 — `exec` 에 정산 조회 경로가 없고 `TestExecNeverWritesSettlement` 가 그 자리를 지킨다. 모니터는 거래 결정을 하지 않으므로 같은 정보를 알아도 오염이 없다.

**Files:**
- Create: `cmd/gld91-monitor/settle.go`, `cmd/gld91-monitor/settle_test.go`, `cmd/gld91-monitor/report.go`, `cmd/gld91-monitor/report_test.go`

**Interfaces:**
- Consumes: `beat.Snapshot`, `rest.Client`(모니터 전용 키), `ledger.OutcomeUp`/`OutcomeDown`
- Produces: `type settlement struct{ MarketID int64; Won bool; SettledAt time.Time; ChainlinkUp bool; BinanceUp bool }`, `type tally struct`, `func accumulate(ss []settlement, entries []entry) tally`, `func formatReport(t tally, snap *beat.Snapshot) string`

- [ ] **Step 1: 실패 테스트 — 집계**

```go
// 적중률에는 반드시 n 과 신뢰구간이 붙는다. 61 표본의 54.1% 는 그 자체로
// 아무 말도 하지 않는다. 이 줄이 없으면 운 좋은 하루를 전략의 성공으로 읽는다.
func TestReportIncludesSampleSizeAndInterval(t *testing.T) {
	tl := tally{Participated: 61, Hits: 33, PnL: -2.58, AvgEntry: 0.487, RebateShares: 0.31}
	out := formatReport(tl, healthySnapshot())
	for _, want := range []string{"n=61", "54.1%", "±"} {
		if !strings.Contains(out, want) {
			t.Errorf("리포트에 %q 가 없다:\n%s", want, out)
		}
	}
}

// 손익분기 승률 = 평균 진입가. 리베이트는 그것을 낮춘다 — 리베이트는 반대편
// 주식이라 우리가 **질 때만** 값이 붙기 때문이다(ledger 패키지 문서).
func TestBreakevenLoweredByRebate(t *testing.T) {
	plain := breakeven(0.487, 0)
	withRebate := breakeven(0.487, 0.005)
	if !(withRebate < plain) {
		t.Errorf("리베이트 포함 손익분기 %.4f 가 미포함 %.4f 보다 낮지 않다 — 방향이 뒤집혔다", withRebate, plain)
	}
	if math.Abs(plain-0.487) > 1e-9 {
		t.Errorf("리베이트 없으면 손익분기 = 평균 진입가여야 한다, got %.6f", plain)
	}
}

// 표본이 0 이면 비율을 계산하지 않는다. 0/0 은 NaN 이고, NaN 이 리포트에
// 실리면 사람은 그것을 손실로 읽는다.
func TestZeroSampleDoesNotProduceNaN(t *testing.T) {
	out := formatReport(tally{}, healthySnapshot())
	if strings.Contains(out, "NaN") {
		t.Errorf("리포트에 NaN 이 있다:\n%s", out)
	}
}

// G2 가 가정한 d≈0.30%. 이 어긋남은 실거래에서만 드러난다.
func TestSettlementMismatchCounted(t *testing.T) {
	ss := []settlement{
		{MarketID: 1, ChainlinkUp: true, BinanceUp: true},
		{MarketID: 2, ChainlinkUp: false, BinanceUp: true}, // 불일치
		{MarketID: 3, ChainlinkUp: true, BinanceUp: true},
	}
	tl := accumulate(ss, nil)
	if tl.Mismatch != 1 || tl.Settled != 3 {
		t.Errorf("불일치 %d/%d, want 1/3", tl.Mismatch, tl.Settled)
	}
}

// 정산되지 않은 회차는 적중률 분모에 들어가면 안 된다. 들어가면 미정산이
// 전부 패배로 계산되어 승률이 조용히 낮아진다.
func TestUnsettledExcludedFromHitRate(t *testing.T) {
	tl := accumulate([]settlement{
		{MarketID: 1, Won: true, SettledAt: time.Unix(1786000000, 0)},
		{MarketID: 2}, // 미정산
	}, nil)
	if tl.Settled != 1 || tl.Hits != 1 {
		t.Errorf("정산 %d 적중 %d, want 1/1", tl.Settled, tl.Hits)
	}
}

// --- 공용 테스트 헬퍼 ---
//
// Task 11 의 integration_test.go 도 이 둘을 쓴다. 같은 패키지이므로 여기
// 한 벌만 둔다 — 두 벌로 두면 한쪽만 고쳐지고, 갈린 쪽이 틀렸을 때 알
// 방법이 없다.

func wantConsts() beat.Consts {
	return beat.Consts{
		CapFraction:         risk.CapFraction,
		DailyFraction:       risk.DefaultDailyFraction,
		ConfidenceThreshold: live.ConfidenceThreshold,
		MinOrderUSD:         risk.MinOrderUSD,
	}
}

// healthySnapshot 은 아무 알람도 나지 않아야 하는 스냅샷이다.
func healthySnapshot() *beat.Snapshot {
	now := time.Now().UTC()
	return &beat.Snapshot{
		BootID: "a", TS: now, Version: "test", Armed: true, Consts: wantConsts(),
		Equity: beat.Equity{
			AvailableUSDT: 63.2, PositionCost: 126.1, CapUSD: 8.62,
			CanArm: true, DailyPnL: -2.58, DailyLimit: -19.19,
		},
		Round:    beat.Round{State: beat.RoundActive, EndsAt: now.Add(2 * time.Minute)},
		Exposure: beat.Exposure{Filled: 4, Open: 2, PendingCancel: 1, Cap: 8.62},
		Loop:     beat.Loop{WSLastDataAt: now, RateLimitRemaining: 118},
		Skips:    map[beat.SkipReason]int{},
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91-monitor/ -run 'TestReport|TestBreakeven|TestZero|TestSettlement|TestUnsettled' -v`
Expected: FAIL — `tally` 미정의

- [ ] **Step 3: 구현**

```go
// breakeven 은 손익분기 승률이다.
//
// 주당 p 에 사서 이기면 $1 을 받으므로, 리베이트가 없으면 손익분기는 정확히
// p 다. 리베이트는 **반대편 주식**으로 오고 우리가 질 때만 값이 붙으므로
// (ledger 패키지 문서) 지는 쪽의 손실을 줄인다 — 즉 손익분기를 낮춘다.
// 방향을 뒤집으면 pmmm-go 에서 +40 을 −90 으로 보고한 그 사고가 된다.
func breakeven(avgEntry, rebateRate float64) float64 {
	if avgEntry <= 0 || avgEntry >= 1 {
		return math.NaN()
	}
	return (avgEntry - rebateRate) / (1 - rebateRate)
}

// wilson 은 이항비율의 95% 신뢰구간 반폭이다.
//
// 정규근사(±1.96·√(p(1−p)/n))를 쓰지 않는 이유: n 이 작거나 p 가 0·1 에
// 가까우면 구간이 [0,1] 밖으로 나가고, 실거래 첫날의 n 은 작다.
func wilson(hits, n int) (lo, hi float64) {
	if n <= 0 {
		return math.NaN(), math.NaN()
	}
	const z = 1.96 // 95%
	p := float64(hits) / float64(n)
	den := 1 + z*z/float64(n)
	centre := (p + z*z/(2*float64(n))) / den
	half := z * math.Sqrt(p*(1-p)/float64(n)+z*z/(4*float64(n)*float64(n))) / den
	return centre - half, centre + half
}
```

`tally` 와 집계:

```go
type tally struct {
	Participated int     // 진입한 회차 수
	Settled      int     // 그중 정산이 끝난 것
	Hits         int     // 정산된 것 중 적중
	Mismatch     int     // Chainlink 와 바이낸스 봉의 방향이 다른 회차
	PnL          float64 // USDT
	AvgEntry     float64 // 평균 진입가 (주당)
	RebateShares float64
}

// entry 는 원장 CSV 에서 읽은 우리 체결 하나다.
type entry struct {
	MarketID int64
	Notional float64
	Shares   float64
}

// accumulate 는 정산과 체결을 합쳐 집계한다.
//
// **미정산 회차는 적중률 분모에 들어가지 않는다.** 들어가면 아직 결과를
// 모르는 회차가 전부 패배로 계산되어 승률이 조용히 낮아지고, 5분 회차라
// 언제나 몇 건은 미정산이다.
func accumulate(ss []settlement, entries []entry) tally {
	var t tally
	var cost, shares float64
	for _, e := range entries {
		cost += e.Notional
		shares += e.Shares
	}
	if shares > 0 {
		t.AvgEntry = cost / shares
	}
	seen := map[int64]bool{}
	for _, s := range ss {
		if !seen[s.MarketID] {
			seen[s.MarketID] = true
			t.Participated++
		}
		if s.ChainlinkUp != s.BinanceUp {
			t.Mismatch++
		}
		if s.SettledAt.IsZero() {
			continue // 미정산
		}
		t.Settled++
		if s.Won {
			t.Hits++
		}
	}
	return t
}
```

> `Mismatch` 는 **정산 여부와 무관하게** 센다 — 두 데이터원의 방향은 정산이
> 끝나기 전에도 비교할 수 있고, 그 불일치가 G2 가 잰 `d≈0.30%` 다.
> `TestSettlementMismatchCounted` 의 기대값(1/3)이 이 규칙을 고정한다.

`formatReport` 는 설계서 §7 의 형식을 만든다. 표본 0 에서는 **비율 줄을 통째로 생략한다** — `0/0` 을 계산하지 않는 것이 NaN 을 나중에 걸러내는 것보다 낫다:

```go
func formatReport(t tally, snap *beat.Snapshot) string {
	var b strings.Builder
	// ... 자본·참여·스킵 줄 (snap 에서 그대로 옮긴다)
	if t.Settled > 0 {
		lo, hi := wilson(t.Hits, t.Settled)
		rate := 100 * float64(t.Hits) / float64(t.Settled)
		be := breakeven(t.AvgEntry, rebateRate)
		fmt.Fprintf(&b, "적중 %d/%d = %.1f%%  n=%d, 95%% CI [%.1f%%, %.1f%%]\n",
			t.Hits, t.Settled, rate, t.Settled, 100*lo, 100*hi)
		fmt.Fprintf(&b, "평균 진입가 %.3f → 손익분기 승률 %.1f%%\n", t.AvgEntry, 100*be)
	}
	return b.String()
}
```

`accumulate` 는 `SettledAt` 이 제로가 아닌 것만 세고, `formatReport` 는 설계서 §7 의 형식을 만든다. **표본 0 에서는 비율 줄을 통째로 생략한다** — `0/0` 을 계산하지 않는 것이 NaN 을 걸러내는 것보다 낫다.

- [ ] **Step 4: 정산 조회 실측**

Run: `GOTOOLCHAIN=local go run ./cmd/probe -settled-round <marketID>`
Expected: `/markets/{id}` 또는 `/markets/oracle/{oracle_question_id}` 중 승패를 직접 주는 쪽을 확인하고, 응답 필드 이름을 `settle.go` 주석에 기록한다.

설계서 §11 이 이것을 미확정으로 남겼다 — P4 가 "REST 에 정산 조회가 없다"고 판단한 것이 주문 경로에 없다는 뜻인지 정말 어디에도 없다는 뜻인지 확인되지 않았다. **둘 다 아니면** `positions/{address}` 의 잔고 변화로 승패를 역산하고, 그 방법을 주석에 명시한다.

- [ ] **Step 5: 통과 확인 + 커밋**

```bash
GOTOOLCHAIN=local go test ./cmd/gld91-monitor/ -race
git add cmd/gld91-monitor/settle.go cmd/gld91-monitor/settle_test.go cmd/gld91-monitor/report.go cmd/gld91-monitor/report_test.go
git commit -m "monitor: 정산 관측과 리포트

봇은 자기 손익을 모른다(exec 에 정산 조회 경로가 없고 그 자리를 테스트가
지킨다). 모니터는 거래 결정을 하지 않으므로 같은 정보를 알아도 오염이 없다.
적중률에는 항상 n 과 신뢰구간을 붙인다."
```

---

### Task 11: 통합과 소크

**Files:**
- Create: `cmd/gld91-monitor/integration_test.go`
- Modify: `Makefile`

**Interfaces:**
- Consumes: 전 태스크

- [ ] **Step 1: 골든 시퀀스 테스트**

```go
// 가짜 봇이 스냅샷 시퀀스를 재생하고, 기대 알람 시퀀스와 대조한다.
// 규칙 하나하나는 Task 4 가 시험했다. 여기서 보는 것은 **배선**이다 —
// 수신·판정·래치·알림이 실제로 이어져 있는지.
func TestEndToEndAlertSequence(t *testing.T) {
	var sent []string
	st := newState(wantConsts(), []byte("s"))
	srv := httptest.NewServer(http.HandlerFunc(st.handleBeat))
	defer srv.Close()

	steps := []struct {
		name  string
		mut   func(*beat.Snapshot)
		wants []string // 이 beat 뒤에 새로 울려야 하는 키
	}{
		{"건강", func(s *beat.Snapshot) {}, nil},
		{"confidence 스킵", func(s *beat.Snapshot) {
			s.Round.State, s.Round.SkipReason = beat.RoundSkipped, beat.SkipConfBelow
		}, nil}, // 조용해야 한다
		{"노출 위반", func(s *beat.Snapshot) {
			s.Exposure = beat.Exposure{Filled: 9, Cap: 8.62}
		}, []string{"exposure"}},
		{"노출 위반 반복", func(s *beat.Snapshot) {
			s.Exposure = beat.Exposure{Filled: 9, Cap: 8.62}
		}, nil}, // 래치가 잡는다
		{"복구", func(s *beat.Snapshot) {}, nil},
		{"상수 변조", func(s *beat.Snapshot) { s.Consts.CapFraction = 0.09 }, []string{"consts"}},
	}
	secret := []byte("s")
	seq := uint64(0)
	for _, step := range steps {
		seq++
		snap := healthySnapshot()
		snap.Seq, snap.BootID, snap.TS = seq, "a", time.Now().UTC()
		step.mut(snap)

		body, _ := json.Marshal(snap)
		req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
		req.Header.Set(beat.SigHeader, beat.Sign(secret, body))
		w := httptest.NewRecorder()
		st.handleBeat(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: code = %d", step.name, w.Code)
		}

		now := time.Now().UTC()
		got, _ := st.Latest()
		fs := rule.Evaluate(rule.Input{
			Snap: got, LastBeat: now, Now: now, Want: wantConsts(),
			ConsecSkips: st.consecSkips,
		})
		fire, _ := st.latch.Step(fs, now)

		sent = sent[:0]
		for _, f := range fire {
			sent = append(sent, f.Key)
		}
		sort.Strings(sent)
		want := append([]string(nil), step.wants...)
		sort.Strings(want)
		if !reflect.DeepEqual(sent, want) && !(len(sent) == 0 && len(want) == 0) {
			t.Errorf("%s: 울린 알람 %v, want %v", step.name, sent, want)
		}
	}
}

// wantConsts 와 healthySnapshot 은 Task 10 의 report_test.go 에서 만들었다.
// 같은 패키지이므로 그대로 쓴다 — 두 벌로 두면 한쪽만 고쳐진다.
```

참고로 Task 10 이 만든 것:

```go
func wantConsts() beat.Consts {
	return beat.Consts{
		CapFraction:         risk.CapFraction,
		DailyFraction:       risk.DefaultDailyFraction,
		ConfidenceThreshold: live.ConfidenceThreshold,
		MinOrderUSD:         risk.MinOrderUSD,
	}
}

// healthySnapshot 은 아무 알람도 나지 않아야 하는 스냅샷이다.
// 포인터를 돌려주는 이유: 각 단계가 mut 으로 한 곳만 고쳐 쓴다.
func healthySnapshot() *beat.Snapshot {
	now := time.Now().UTC()
	return &beat.Snapshot{
		BootID: "a", TS: now, Version: "test", Armed: true, Consts: wantConsts(),
		Equity: beat.Equity{
			AvailableUSDT: 63.2, PositionCost: 126.1, CapUSD: 8.62,
			CanArm: true, DailyPnL: -2.58, DailyLimit: -19.19,
		},
		Round:    beat.Round{State: beat.RoundActive, EndsAt: now.Add(2 * time.Minute)},
		Exposure: beat.Exposure{Filled: 4, Open: 2, PendingCancel: 1, Cap: 8.62},
		Loop:     beat.Loop{WSLastDataAt: now, RateLimitRemaining: 118},
		Skips:    map[beat.SkipReason]int{},
	}
}
```

- [ ] **Step 2: 실행**

Run: `GOTOOLCHAIN=local go test ./cmd/gld91-monitor/ -race -run TestEndToEnd -v`
Expected: PASS

- [ ] **Step 3: Makefile 타겟**

```makefile
monitor:
	GOTOOLCHAIN=local go build -o bin/gld91-monitor ./cmd/gld91-monitor

monitor-soak:
	GOTOOLCHAIN=local go test ./cmd/gld91-monitor/ ./internal/beat/... -race -count=1
```

- [ ] **Step 4: DRY-RUN 소크 (60분)**

Run:
```bash
GOTOOLCHAIN=local go run ./cmd/gld91-monitor &
GOTOOLCHAIN=local go run ./cmd/gld91 -dry-run-minutes 60
```

확인할 것 — **전부 눈으로 본다. 코드가 강제하지 못한다:**

1. beat 가 3초 간격으로 도착한다(모니터 로그의 `seq` 가 끊기지 않는다)
2. `exec` 의 재호가 지연이 리포터를 붙이기 **전과 같다** — Publish 가 실제로 블록하지 않는지는 여기서만 확인된다
3. confidence 미달 회차에서 텔레그램이 **조용하다**
4. 모니터를 죽였다 살리면 봇이 "모니터 무응답"을 1회 알리고 복구를 1회 알린다
5. 봇을 죽이면 20초 안에 🚨 가 오고, 마지막 스냅샷의 미체결 목록이 첨부된다
6. `/why` 가 스킵 사유와 연속 횟수를 정확히 답한다
7. 모니터의 `ratelimit-remaining` 이 봇의 것과 **독립적으로** 움직인다(키 분리 확인)

- [ ] **Step 5: 커밋**

```bash
git add cmd/gld91-monitor/integration_test.go Makefile
git commit -m "monitor: 통합 테스트와 60분 소크"
```

---

## 자기 검토 결과

설계서 각 절을 태스크에 대응시켜 확인했다. **초안에서 세 군데가 비어 있었고 Task 7 로 메웠다:**

- 스냅샷을 조립하는 경로가 없었다. `exec.roundState` 는 unexported 이고 그래야 하므로, 값을 복사해 내보내는 관측 훅이 필요했다.
- `reporter.Commands()` 를 소비하는 코드가 없었다. 명령이 도착해도 봇이 아무것도 하지 않았다.
- 설계서 §3 의 "모니터 사망은 봇이 알린다"를 구현하는 태스크가 없었다. Task 6 은 실패를 세기만 했다.

대응표: §1·§2(가)→Task 1 · §2(나)→Task 9 config · §3→Task 7·8·9 · §4→Task 6 · §5→Task 2·7 · §6→Task 4·5 · §7→Task 10 · §8→Task 9 · §9→Task 2(응답 타입 고정)·10 · §11 미확정→Task 1 Step 5, Task 10 Step 4.

아래는 **이 계획이 의도적으로 하지 않는 것**이다.

- **자본곡선 PNG.** 텍스트 리포트로 시작한다(사용자 결정).
- **모니터의 이중화.** 모니터가 죽으면 봇이 알린다(§3). 모니터를 감시하는 모니터는 만들지 않는다.
- **`/pnl 7d` 의 장기 보관.** 정산 관측 결과를 어디에 쌓을지 정하지 않았다. Task 9 는 메모리에만 두고 재시작하면 잃는다. 7일 조회가 필요해지면 별도 계획으로 SQLite 나 CSV 를 붙인다 — 지금 넣으면 검증되지 않은 스키마가 굳는다.
- **TLS 인증서 발급.** `MONITOR_LISTEN` 앞에 리버스 프록시를 두거나 자체서명 + 봇 쪽 핀닝을 쓴다. 배포(P6) 절차의 몫이다.

**Task 1 이 Step 5 에서 막히면 나머지 전부가 여전히 유효하다.** 만료를 거래소가 받지 않는다면 설계서 §2(가)를 갱신하고 §8 의 "무응답 시 대응"이 최종 형태가 된다 — 모니터는 그 경우에도 마지막 스냅샷의 미체결 목록을 첨부해 사람이 지갑으로 확인할 근거를 준다.

**임계에 근거가 없는 것들**(`CrashLoopChanges`, `SampleRejectedConsec`, `FetchErrorConsec`, 래치 `sustain` 30초)은 `rule.go` 주석과 설계서 §11 에 그 사실을 적어 두었다. DRY-RUN 24시간과 실거래 첫 주의 오탐률을 보고 조정한다.
