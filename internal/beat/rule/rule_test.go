package rule

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

// --- 헬퍼 ---

// healthy 는 아무 알람도 나지 않아야 하는 입력이다.
func healthy() Input {
	now := time.Unix(1786000000, 0).UTC()
	c := beat.Consts{
		CapFraction: 0.0455, DailyFraction: 0.10,
		ConfidenceThreshold: 0.0172, MinOrderUSD: 1.0,
	}
	return Input{
		Now: now, LastBeat: now.Add(-3 * time.Second), Want: c,
		ConsecSkips: map[beat.SkipReason]int{},
		Snap: &beat.Snapshot{
			Seq: 1, BootID: "a", TS: now, Armed: true, Consts: c,
			Equity: beat.Equity{
				AvailableUSDT: 63.2, PositionCost: 126.1, CapUSD: 8.62,
				CanArm: true, DailyPnL: -2.58, DailyLimit: -19.19,
			},
			Round:    beat.Round{State: beat.RoundActive, EndsAt: now.Add(2 * time.Minute)},
			Exposure: beat.Exposure{Filled: 4, Open: 2, PendingCancel: 1, Cap: 8.62},
			Loop:     beat.Loop{WSLastDataAt: now.Add(-time.Second), LastActionAt: now.Add(-2 * time.Second), LastLoopAt: now.Add(-50 * time.Millisecond), RateLimitRemaining: 118},
			Skips:    map[beat.SkipReason]int{},
		},
	}
}

// findLevel 은 그 키의 알람 등급이다. 없으면 Info(= 알람 없음).
func findLevel(fs []Finding, key string) Level {
	for _, f := range fs {
		if f.Key == key {
			return f.Level
		}
	}
	return Info
}

// findMessage 는 그 키의 알람 문구다. 없으면 빈 문자열.
func findMessage(fs []Finding, key string) string {
	for _, f := range fs {
		if f.Key == key {
			return f.Message
		}
	}
	return ""
}

// --- 테스트 ---

// **이것이 없으면 아래 테스트 전부가 "항상 모든 알람을 낸다"는 구현으로도
// 통과한다.** 건강한 봇에 대한 침묵이 나머지 규칙의 의미를 만든다.
func TestHealthyIsSilent(t *testing.T) {
	if f := Evaluate(healthy()); len(f) != 0 {
		t.Errorf("건강한 스냅샷에 알람이 났다: %+v", f)
	}
}

// 이 봇에서 "안 하고 있음"은 알람이 아니다. confidence < 0.0172 스킵은 옳은
// 동작이고 몇 시간 이어질 수 있다 — GLD-9(마켓메이커)용으로 초안했던
// "최근 N분 회차 수 0 → 알람" 규칙은 여기서 명백히 틀렸다.
func TestConfBelowSkipIsSilent(t *testing.T) {
	in := healthy()
	in.Snap.Round.State = beat.RoundSkipped
	in.Snap.Round.SkipReason = beat.SkipConfBelow
	in.ConsecSkips = map[beat.SkipReason]int{beat.SkipConfBelow: 200}
	if f := Evaluate(in); len(f) != 0 {
		t.Errorf("confidence 미달 스킵 200회차에 알람이 났다: %+v", f)
	}
}

func TestOtherSkipReasonsAlert(t *testing.T) {
	cases := []struct {
		reason beat.SkipReason
		consec int
		want   Level
	}{
		{beat.SkipSampleRejected, 5, Warn},
		{beat.SkipSampleRejected, 4, Info}, // 임계 미달이면 조용하다
		{beat.SkipEquity, 1, Warn},
		{beat.SkipDailyLimit, 1, Crit},
		{beat.SkipFetchError, 3, Crit},
		{beat.SkipFetchError, 2, Info},
		{beat.SkipPredictError, 3, Crit},
	}
	for _, c := range cases {
		in := healthy()
		in.Snap.Round.State = beat.RoundSkipped
		in.Snap.Round.SkipReason = c.reason
		in.ConsecSkips = map[beat.SkipReason]int{c.reason: c.consec}
		if got := findLevel(Evaluate(in), "skip:"+string(c.reason)); got != c.want {
			t.Errorf("%s×%d → %v, want %v", c.reason, c.consec, got, c.want)
		}
	}
}

// 봇이 모니터가 모르는 사유를 보내면 알람이다. 조용히 넘기면 그 사유로 멈춘
// 봇이 아무 알람도 내지 않는다 — 정확히 이 패키지가 막으려는 상태다.
func TestUnknownSkipReasonAlerts(t *testing.T) {
	in := healthy()
	in.Snap.Round.State = beat.RoundSkipped
	in.Snap.Round.SkipReason = beat.SkipReason("model_stale")
	if got := findLevel(Evaluate(in), "skip:model_stale"); got != Crit {
		t.Errorf("모르는 사유에 %v, want Crit", got)
	}
}

// 상수가 다르면 우리가 아는 그 바이너리가 아니다.
func TestConstMismatchIsCritical(t *testing.T) {
	for name, mut := range map[string]func(*beat.Consts){
		"cap":        func(c *beat.Consts) { c.CapFraction = 0.05 },
		"daily":      func(c *beat.Consts) { c.DailyFraction = 0.20 },
		"confidence": func(c *beat.Consts) { c.ConfidenceThreshold = 0.01 },
		"minOrder":   func(c *beat.Consts) { c.MinOrderUSD = 5 },
	} {
		in := healthy()
		mut(&in.Snap.Consts)
		if got := findLevel(Evaluate(in), "consts"); got != Crit {
			t.Errorf("%s 불일치가 %v 로 났다, want Crit", name, got)
		}
	}
}

// exec 의 근본 불변식. 제약이 강한 부등호(< cap)이므로 **같아도 위반**이다.
func TestExposureInvariantViolation(t *testing.T) {
	in := healthy()
	in.Snap.Exposure = beat.Exposure{Filled: 5, Open: 3, PendingCancel: 1, Cap: 8.62}
	if got := findLevel(Evaluate(in), "exposure"); got != Crit {
		t.Errorf("9.0 >= 8.62 인데 %v 다, want Crit", got)
	}
}

func TestExposureInvariantBoundary(t *testing.T) {
	in := healthy()
	in.Snap.Exposure = beat.Exposure{Filled: 8.62, Cap: 8.62}
	if got := findLevel(Evaluate(in), "exposure"); got != Crit {
		t.Errorf("정확히 cap 인데 %v, want Crit — 제약은 강한 부등호다", got)
	}
	in.Snap.Exposure = beat.Exposure{Filled: 8.61, Cap: 8.62}
	if got := findLevel(Evaluate(in), "exposure"); got != Info {
		t.Errorf("cap 미만인데 알람이 났다: %v", got)
	}
}

// cap 0 은 "회차가 없다"이지 "노출 위반"이 아니다. IDLE 상태에서 매 beat
// 알람이 나면 그 알람은 곧 무시된다.
func TestZeroCapIsNotViolation(t *testing.T) {
	in := healthy()
	in.Snap.Exposure = beat.Exposure{Cap: 0}
	if got := findLevel(Evaluate(in), "exposure"); got != Info {
		t.Errorf("cap 0(회차 없음)에 %v, want Info", got)
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
	in.Snap.Exposure.OpenOrders = make([]beat.OpenOrder, 80)
	if got := findLevel(Evaluate(in), "cancel_batch"); got != Info {
		t.Errorf("미체결 80건(임계)에 알람이 났다: %v", got)
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

func TestUnaccountedNotionalWarns(t *testing.T) {
	in := healthy()
	in.Snap.Exposure.Unaccounted = 4.5
	if got := findLevel(Evaluate(in), "unaccounted"); got != Warn {
		t.Errorf("식별자 없는 주문에 %v, want Warn", got)
	}
}

// beat 가 안 오면 봇이 죽은 것이다. 봇의 TS 가 아니라 **우리가 받은 시각**을
// 기준으로 잰다 — 봇의 시계는 봇이 죽어도 멈추지 않는다.
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
	in.BootChanges = 1
	if got := findLevel(Evaluate(in), "crashloop"); got != Info {
		t.Errorf("재시작 1회에 크래시루프 알람이 났다: %v", got)
	}
}

// 하트비트는 오는데 마켓데이터만 끊긴 경우 — ws/conn.go 가 이미 아는 고장이다.
// 서버 하트비트가 읽기 데드라인을 계속 갱신하므로 소켓은 건강해 보인다.
func TestWSDataStall(t *testing.T) {
	in := healthy()
	in.Snap.Loop.WSLastDataAt = in.Now.Add(-31 * time.Second)
	if got := findLevel(Evaluate(in), "ws_data"); got != Crit {
		t.Errorf("WS 데이터 31초 정체에 %v, want Crit", got)
	}
}

// 제로시각은 "아직 한 번도 못 받았다"이고, 기동 직후가 그렇다. 그것을
// 1970년부터 정체한 것으로 읽으면 매 기동마다 오탐이 난다.
func TestWSDataZeroTimeIsNotStall(t *testing.T) {
	in := healthy()
	in.Snap.Loop.WSLastDataAt = time.Time{}
	if got := findLevel(Evaluate(in), "ws_data"); got != Info {
		t.Errorf("WS 제로시각에 %v, want Info", got)
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

	in = healthy()
	in.Snap.Equity.CanArm = false
	if got := findLevel(Evaluate(in), "can_arm"); got != Warn {
		t.Errorf("CanArm false 에 %v, want Warn", got)
	}
}

// 한도는 음수다. 부호를 뒤집어 양수로 들어오면 이익이 나도 한도로 읽힌다 —
// pmmm-go 에서 부호 하나로 +40 이 −90 이 된 전례가 있다.
func TestPositiveDailyLimitDoesNotFireOnProfit(t *testing.T) {
	in := healthy()
	in.Snap.Equity.DailyLimit = 19.19 // 부호가 뒤집힌 값
	in.Snap.Equity.DailyPnL = 5.0     // 이익
	if got := findLevel(Evaluate(in), "daily_limit"); got != Info {
		t.Errorf("이익 중인데 일손실 알람이 났다: %v", got)
	}
}

// 망가진 입력은 "이상 없음"이 아니다. 조용한 정상이 감시 장치의 실패 모드다.
func TestBrokenSnapshotAlerts(t *testing.T) {
	for name, mut := range map[string]func(*beat.Snapshot){
		"nan-equity":  func(s *beat.Snapshot) { s.Equity.AvailableUSDT = math.NaN() },
		"inf-cap":     func(s *beat.Snapshot) { s.Exposure.Cap = math.Inf(1) },
		"nan-filled":  func(s *beat.Snapshot) { s.Exposure.Filled = math.NaN() },
		"nan-pnl":     func(s *beat.Snapshot) { s.Equity.DailyPnL = math.NaN() },
		"neginf-open": func(s *beat.Snapshot) { s.Exposure.Open = math.Inf(-1) },
		"nan-cap-usd": func(s *beat.Snapshot) { s.Equity.CapUSD = math.NaN() },
		"nan-pending": func(s *beat.Snapshot) { s.Exposure.PendingCancel = math.NaN() },
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
	if f[0].Level != Crit {
		t.Errorf("nil 스냅샷에 %v, want Crit", f[0].Level)
	}
}

// Key 는 래치의 식별자다. 같은 조건이 매번 다른 키로 나오면 래치가 매번 새
// 알람으로 보고 3초마다 운다.
func TestKeysAreStableAcrossCalls(t *testing.T) {
	in := healthy()
	in.Snap.Armed = false
	in.Snap.Loop.RateLimitRemaining = 0

	first := Evaluate(in)
	in.Now = in.Now.Add(time.Minute)
	in.LastBeat = in.LastBeat.Add(time.Minute)
	in.Snap.Loop.WSLastDataAt = in.Snap.Loop.WSLastDataAt.Add(time.Minute)
	second := Evaluate(in)

	if len(first) != len(second) {
		t.Fatalf("알람 수가 달라졌다: %d → %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Key != second[i].Key {
			t.Errorf("%d번째 키가 %q → %q 로 바뀌었다", i, first[i].Key, second[i].Key)
		}
	}
}

// 프로세스는 멀쩡하고 beat 도 신선한데 집행 루프만 멎은 상태다 — 발행이
// 별도 고루틴이라 스냅샷은 계속 나가고, mtime 하트비트도 이것을 못 잡는다.
func TestLoopStallOnActiveRound(t *testing.T) {
	in := healthy()
	in.Snap.Loop.LastLoopAt = in.Now.Add(-121 * time.Second)
	if got := findLevel(Evaluate(in), "loop_stall"); got != Crit {
		t.Errorf("121초 무회전에 %v, want Crit", got)
	}
	in.Snap.Loop.LastLoopAt = in.Now.Add(-119 * time.Second)
	if got := findLevel(Evaluate(in), "loop_stall"); got != Info {
		t.Errorf("119초에 알람이 났다: %v", got)
	}
}

// **정상 회차가 알람을 내면 안 된다.**
//
// 이 시험이 이 규칙의 존재 이유다. 실거래 첫 회차(2026-08-11 07:45)가
// 정확히 이 모양이었다 — 한 번 걸고 회차 끝까지 재호가가 없었는데, 옛
// 규칙(LastActionAt 90초)이 Crit 을 내고 3분을 버텼다. 군중이 안 움직이면
// 우리 호가는 이미 옳은 자리에 있다.
func TestLoopSpinningWithoutActionIsHealthy(t *testing.T) {
	in := healthy()
	in.Snap.Loop.LastActionAt = in.Now.Add(-4 * time.Minute) // 회차 내내 무행동
	in.Snap.Loop.LastLoopAt = in.Now.Add(-50 * time.Millisecond)
	for _, f := range Evaluate(in) {
		if f.Level >= Warn {
			t.Errorf("돌고 있는데 안 걸었을 뿐인 회차에 알람이 났다: %s %s", f.Key, f.Message)
		}
	}
}

// 건너뛴 회차에는 루프 자체가 없다. 그것을 정체로 읽으면 confidence 미달로
// 조용한 시간 내내 알람이 울린다 — 이 봇에서 가장 흔한 정상 상태다.
func TestLoopStallIgnoredOnSkippedRound(t *testing.T) {
	in := healthy()
	in.Snap.Round.State = beat.RoundSkipped
	in.Snap.Round.SkipReason = beat.SkipConfBelow
	in.Snap.Loop.LastLoopAt = in.Now.Add(-time.Hour)
	if got := findLevel(Evaluate(in), "loop_stall"); got != Info {
		t.Errorf("건너뛴 회차에 정체 알람이 났다: %v", got)
	}
}

// 제로시각은 "이 회차의 첫 바퀴가 아직" 이다. 회차마다 관측이 제로로
// 리셋되므로(beatWire.Report), 1970년부터 정체한 것으로 읽으면 **모든 회차가
// 시작하자마자** 울린다.
func TestLoopStallZeroTimeIsNotStall(t *testing.T) {
	in := healthy()
	in.Snap.Loop.LastLoopAt = time.Time{}
	if got := findLevel(Evaluate(in), "loop_stall"); got != Info {
		t.Errorf("제로시각에 %v, want Info", got)
	}
}

// 옛 규칙이 되살아나지 않게 못을 박는다. LastActionAt 이 아무리 오래돼도
// 그것만으로는 알람이 아니다.
func TestStaleActionAloneNeverAlarms(t *testing.T) {
	in := healthy()
	in.Snap.Loop.LastActionAt = in.Now.Add(-time.Hour)
	in.Snap.Loop.LastLoopAt = in.Now
	for _, f := range Evaluate(in) {
		if f.Level >= Warn {
			t.Errorf("무행동만으로 알람이 났다: %s %s", f.Key, f.Message)
		}
	}
}

// 봇이 조용해진 뒤로는 마지막 스냅샷의 미체결 목록이 존재하는 유일한
// 정보원이다 — 개인키 없는 모니터는 GET /v1/orders 를 부를 수 없다.
func TestStaleAlarmCarriesOpenOrders(t *testing.T) {
	in := healthy()
	in.LastBeat = in.Now.Add(-25 * time.Second)
	in.Snap.Exposure.OpenOrders = []beat.OpenOrder{
		{ID: "0xabc", Tick: 41, Notional: 2.05},
		{ID: "0xdef", Tick: 42, Notional: 1.10},
	}
	msg := findMessage(Evaluate(in), "stale")
	for _, want := range []string{"0xabc", "0xdef", "41", "2.05"} {
		if !strings.Contains(msg, want) {
			t.Errorf("무응답 알람에 %q 가 없다: %q", want, msg)
		}
	}
}

// 미체결이 없으면 문구를 붙이지 않는다. 빈 목록에 "미체결 0건" 을 붙이면
// 사람이 그것을 사고로 읽는다.
func TestStaleAlarmWithoutOpenOrdersStaysPlain(t *testing.T) {
	in := healthy()
	in.LastBeat = in.Now.Add(-25 * time.Second)
	in.Snap.Exposure.OpenOrders = nil
	if msg := findMessage(Evaluate(in), "stale"); strings.Contains(msg, "미체결") {
		t.Errorf("미체결이 없는데 목록 문구가 붙었다: %q", msg)
	}
}

// **상한이 없으면 사고가 큰 순간에 알람이 통째로 사라진다.** 텔레그램 한 통은
// 4096자이고, 미확인 주문은 cap/$1 개까지 늘 수 있다.
func TestStaleAlarmTruncatesLongOpenOrderList(t *testing.T) {
	in := healthy()
	in.LastBeat = in.Now.Add(-25 * time.Second)
	for i := 0; i < 100; i++ {
		in.Snap.Exposure.OpenOrders = append(in.Snap.Exposure.OpenOrders,
			beat.OpenOrder{ID: fmt.Sprintf("0x%064d", i), Tick: int64(i), Notional: 1})
	}
	msg := findMessage(Evaluate(in), "stale")
	if len(msg) > 2000 {
		t.Errorf("알람이 %d자다 — 텔레그램 한 통에 들어가지 않는다", len(msg))
	}
	if !strings.Contains(msg, "미체결 100건") {
		t.Errorf("전체 건수가 없다 — 잘린 목록만 보면 사고 규모를 오판한다: %q", msg)
	}
	if !strings.Contains(msg, "외 92건") {
		t.Errorf("생략 표시가 없다 — 잘린 사실이 숨는다: %q", msg)
	}
}
