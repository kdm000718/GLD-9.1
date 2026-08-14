// Package rule 은 스냅샷 하나를 보고 알람을 낸다.
//
// 순수하다 — 네트워크도, 시계도, 전역 상태도 없다. 현재 시각과 직전 상태는
// 전부 [Input] 으로 받는다. `internal/risk` 가 결정을
// 순수 함수로 떼어낸 것과 같은 이유다: 규칙 하나하나를 값 몇 개로 시험할 수
// 있어야 하고, **시험할 수 없는 감시 장치는 감시하지 않는 것과 같다.**
//
// # 망가진 입력에서의 방향 — 알리는 쪽이다
//
// `internal/risk` 는 "거래하지 않는 쪽"으로 실패한다. 여기는 반대로 **알리는
// 쪽**이다. NaN 이나 nil 을 조용히 넘기면 "이상 없음"으로 읽히는데, 감시
// 장치의 침묵은 곧 정상 신호다. 틀린 알람은 사람이 무시하면 되지만 없는
// 알람은 사람이 알 방법이 없다.
//
// # "안 하고 있음"은 알람이 아니다
//
// 이 봇은 `confidence = 2×|p_up−0.5|` 가 `live.ConfidenceThreshold`(0.0172)
// 미만이면 그 회차를 통째로 건너뛴다. 그것이 **옳은 동작**이고 몇 시간
// 이어질 수 있다. 그래서 참여율이나 "최근 N분 회차 수" 같은 지표로는 판정할
// 수 없고, 오직 스킵 **사유**로만 판정한다([skipFinding]).
package rule

import (
	"fmt"
	"math"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

// Level 은 알람의 심각도다. 값이 클수록 심각하다 — 래치가 승격을 판정할 때
// 이 순서에 기댄다.
type Level int

const (
	// Info 는 알람이 아니다. "이 키에 대해 할 말이 없다"는 뜻이고, 조회
	// 헬퍼의 제로값이기도 하다.
	Info Level = iota
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

// Finding 은 알람 하나다.
//
// Key 는 래치의 식별자다. **안정적이어야 한다** — 같은 조건이 매번 다른 키로
// 나오면 래치가 매번 새 알람으로 보고 3초마다 운다.
type Finding struct {
	Key     string
	Level   Level
	Message string
}

// 임계. **근거가 없는 값은 그렇게 적는다** — `internal/risk` 가 "임계가 없는
// 조건은 두지 않는다. 숫자가 없으면 그 가드는 구현되지 않은 것과 같다"고 한
// 원칙의 짝이다. 숫자는 박되 어디서 왔는지 숨기지 않는다.
const (
	// StaleAfter — beat 3초 주기의 6회 연속 실패. GLD-7 의 파일 mtime 15초
	// 보다 넉넉한 이유는 네트워크 왕복이 끼기 때문이다.
	StaleAfter = 20 * time.Second
	// WSDataStall — P4 실측: 같은 호가창 p99 2.0s, 최대 9.0s. 30초면 오탐
	// 여지 없이 확실한 사망이다.
	WSDataStall = 30 * time.Second
	// CrashLoopChanges — 이 횟수 이상 재시작하면 크래시루프로 본다.
	// **근거 없음** — DRY-RUN 24시간의 오탐률을 보고 조정한다.
	CrashLoopChanges = 2
	// CancelBatchWarn — `rest` 의 취소 배치 상한 100 에 대한 여유.
	// Task 7 report §4(나): 100 을 넘기면 요청 자체가 거절되어 **아무것도**
	// 취소되지 않는다.
	CancelBatchWarn = 80
	// EquityCeiling — 미확인 주문 상한이 cap/$1 이므로 이 위에서 배치 상한
	// 100 에 도달할 수 있다. $1 / 0.0455 ≈ $21.98 당 1건 → 100건이면 약 $2,198.
	EquityCeiling = 2200.0
	// SampleRejectedConsec / FetchErrorConsec — **근거 없음**(위와 같다).
	SampleRejectedConsec = 5
	FetchErrorConsec     = 3
	// LoopStall — 회차를 운용 중인데 집행 루프가 이만큼 한 바퀴도 돌지
	// 못했으면 멎은 것이다.
	//
	// # 왜 "무행동" 이 아니라 "무회전" 인가
	//
	// 처음에는 `LastActionAt` 이 90초 멈추면 정체로 봤다. **실거래 첫 회차가
	// 그것을 반증했다**: 07:45:21 에 한 번 걸고 회차가 끝나는 07:50:00 까지
	// 아무 행동이 없었고, 알람은 07:46:52 에 울려 회차 경계까지 3분을
	// 버텼다. 군중이 안 움직이면 quote 는 DoNothing 을 돌려주고 우리 호가는
	// 이미 옳은 자리에 있다 — 고장이 아니라 정상 회차의 모습이다.
	// `beat.Loop.LastActionAt` 주석이 그것을 이미 적어 두었는데도 규칙이
	// 반대로 갔다. 회차마다 울리는 Crit 은 진짜 Crit 을 묻는다.
	//
	// # 왜 120초인가
	//
	// REST 클라이언트 타임아웃이 30초다(`rest/client.go`). 한 바퀴 안에서
	// 주문 생성과 취소 스윕이 각각 그 시한까지 붙들릴 수 있으므로, 막힌 호출
	// 하나가 닿을 수 없는 거리에 둔다. 멎은 루프는 영원히 멎어 있으니
	// 늦게 잡아도 잡히고, 오경보는 그렇지 않다.
	LoopStall = 120 * time.Second
)

// Input 은 판정에 필요한 전부다.
type Input struct {
	// Snap 은 마지막으로 받은 스냅샷이다. nil 이면 아직 아무것도 받지 못했거나
	// 배선이 끊긴 것이고, 둘 다 알람이다.
	Snap *beat.Snapshot
	// LastBeat 은 그 스냅샷을 받은 시각이다(스냅샷 안의 TS 가 아니다 —
	// 그것은 봇의 시계이고, 봇의 시계는 봇이 죽으면 멈추지 않는다).
	LastBeat time.Time
	Now      time.Time
	// Want 는 우리가 아는 봇 상수다. 리터럴이 아니라 봇과 같은 패키지에서
	// 온 값이어야 한다.
	Want beat.Consts
	// ConsecSkips 는 같은 사유로 연속 건너뛴 회차 수다.
	ConsecSkips map[beat.SkipReason]int
	// BootChanges 는 최근 10분 재시작 횟수다.
	BootChanges int
}

// Evaluate 는 이 입력에서 나는 알람 전부다. 순서는 결정적이다 — 맵을 순회
// 하지 않는다(래치와 리포트가 순서에 기대지는 않지만, 테스트가 기댄다).
func Evaluate(in Input) []Finding {
	var out []Finding
	add := func(key string, lv Level, format string, args ...any) {
		out = append(out, Finding{Key: key, Level: lv, Message: fmt.Sprintf(format, args...)})
	}

	if in.Snap == nil {
		add("broken", Crit, "스냅샷이 없다 — 모니터 배선이나 봇의 첫 기동을 확인하라")
		return out
	}
	s := in.Snap

	// ── 죽음 ────────────────────────────────────────────────────────────
	if age := in.Now.Sub(in.LastBeat); age > StaleAfter {
		// **마지막으로 본 미체결 주문을 붙인다.**
		//
		// 개인키 없는 모니터는 `GET /v1/orders` 를 부를 수 없다(JWT 필요).
		// 봇이 조용해진 뒤로는 이 목록이 존재하는 유일한 정보원이고, 사람이
		// 지갑으로 확인할 근거다. 여기서 빼면 알람은 "무응답" 만 말하고,
		// 받은 사람은 걸린 주문이 있는지조차 모른 채 판단해야 한다.
		add("stale", Crit, "하트비트 %.0f초 무응답%s", age.Seconds(), openOrdersNote(s))
	}
	if in.BootChanges >= CrashLoopChanges {
		add("crashloop", Crit, "10분 내 재시작 %d회 — 크래시루프", in.BootChanges)
	}

	// ── 망가진 값 ───────────────────────────────────────────────────────
	// NaN·Inf 는 상류(온체인 잔고 조회, 원장 집계)가 망가졌을 때만 나온다.
	// 조용히 넘기면 "이상 없음"으로 읽히는데, 그 상태의 봇은 risk 의 실패
	// 방향 덕에 거래를 멈춰 있을 가능성이 높다 — 즉 조용히 멈춰 있다.
	// 순서를 고정하려고 슬라이스로 돈다(맵이면 어느 필드가 보고될지 갈린다).
	for _, f := range []struct {
		name string
		v    float64
	}{
		{"equity.available", s.Equity.AvailableUSDT},
		{"equity.cap", s.Equity.CapUSD},
		{"equity.daily_pnl", s.Equity.DailyPnL},
		{"exposure.filled", s.Exposure.Filled},
		{"exposure.open", s.Exposure.Open},
		{"exposure.pending_cancel", s.Exposure.PendingCancel},
		{"exposure.cap", s.Exposure.Cap},
	} {
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) {
			add("broken", Crit, "%s 가 %v 다 — 상류 조회가 망가졌다", f.name, f.v)
			break
		}
	}

	// ── 상수 대조 ───────────────────────────────────────────────────────
	if s.Consts != in.Want {
		add("consts", Crit, "봇 상수가 예상과 다르다: %+v (want %+v) — 배포된 바이너리를 확인하라",
			s.Consts, in.Want)
	}

	// ── 살아있는데 죽음 ─────────────────────────────────────────────────
	if !s.Armed {
		add("disarmed", Crit, "무장이 해제됐다 — 서명만 하고 주문을 내지 않는다")
	}
	if !s.Loop.WSLastDataAt.IsZero() {
		if age := in.Now.Sub(s.Loop.WSLastDataAt); age > WSDataStall {
			add("ws_data", Crit, "WS 마켓데이터 %.0f초 정체 (서버 하트비트는 살아 있다)", age.Seconds())
		}
	}
	// 한도는 음수다. `<=` 로 비교하는 것이 부호 규약을 그대로 쓰는 방법이다.
	if s.Equity.DailyLimit < 0 && s.Equity.DailyPnL <= s.Equity.DailyLimit {
		add("daily_limit", Crit, "일손실 %.2f 가 한도 %.2f 에 닿았다", s.Equity.DailyPnL, s.Equity.DailyLimit)
	}
	// **거래 시간대가 아니면 자본을 재지 않는다.** 그 회차의 스냅샷에 실린
	// equity 는 "0 이다" 가 아니라 "안 봤다" 이므로, 그것으로 경보하면 하루
	// 절반이 오경보가 된다(2026-08-14 실측 144회/일). 봇은 미리 받아 둔 값을
	// 실어 보내지만 기동 직후에는 그것도 비어 있다.
	//
	// 놓치는 것은 없다. 자본이 정말 말랐다면 **다음 거래 시간대의 실제
	// 조회**가 그 자리에서 울린다 — 그리고 걸지 않는 시간대의 자본 부족은
	// 그때 조치할 일이 아니다.
	if !s.Equity.CanArm && s.Round.SkipReason != beat.SkipOutsideHours {
		add("can_arm", Warn, "equity 가 무장 최소치 미만이다 (회차상한 %.2f)", s.Equity.CapUSD)
	}

	// ── 노출·집행 ───────────────────────────────────────────────────────
	// exec 의 근본 불변식이다. 제약이 **강한 부등호**(< cap)이므로 같아도 위반이다.
	if s.Exposure.Cap > 0 {
		if total := s.Exposure.Filled + s.Exposure.Open + s.Exposure.PendingCancel; total >= s.Exposure.Cap {
			add("exposure", Crit, "노출 불변식 위반: %.2f >= cap %.2f", total, s.Exposure.Cap)
		}
	}
	if n := len(s.Exposure.OpenOrders); n > CancelBatchWarn {
		add("cancel_batch", Crit,
			"미체결 %d건 — 취소 배치 상한 100 을 넘으면 요청이 거절되어 **아무것도** 취소되지 않는다", n)
	}
	if s.Equity.AvailableUSDT > EquityCeiling {
		add("equity_ceiling", Warn,
			"equity $%.0f — 미확인 주문이 취소 배치 상한에 도달할 수 있는 구간이다", s.Equity.AvailableUSDT)
	}
	if s.Exposure.Unaccounted > 0 {
		add("unaccounted", Warn,
			"식별자 없는 주문 명목 $%.2f — 취소할 수 없고 회차 끝까지 노출에 남는다", s.Exposure.Unaccounted)
	}
	if s.Loop.RateLimitRemaining <= 0 {
		add("ratelimit", Crit, "요청 예산이 0 이다 — Fills 스로틀을 확인하라")
	}

	// **회차를 운용 중인데 집행 루프가 멎은 상태.**
	//
	// 프로세스는 멀쩡하고 beat 도 신선하다 — 발행은 별도 고루틴이라 루프가
	// 멎어도 스냅샷은 계속 나간다. mtime 하트비트가 절대 잡지 못하는 고장이고,
	// 그 고장을 드러내는 값은 LastLoopAt 하나뿐이다.
	//
	// 제로는 알람이 아니다: 회차마다 제로에서 시작하므로 "아직 첫 바퀴 전"
	// 이라는 뜻이다. 그것을 정체로 읽으면 **모든 회차가 시작하자마자** 울린다.
	// 건너뛴 회차(SKIPPED)에는 루프 자체가 없으므로 보지 않는다.
	if s.Round.State == beat.RoundActive && !s.Loop.LastLoopAt.IsZero() {
		if age := in.Now.Sub(s.Loop.LastLoopAt); age > LoopStall {
			add("loop_stall", Crit, "운용 중인 회차에서 집행 루프가 %.0f초째 한 바퀴도 돌지 않았다", age.Seconds())
		}
	}

	// ── 스킵 ────────────────────────────────────────────────────────────
	if s.Round.State == beat.RoundSkipped {
		out = append(out, skipFinding(s.Round.SkipReason, in.ConsecSkips[s.Round.SkipReason])...)
	}
	return out
}

// skipFinding 은 스킵 사유별 판정이다.
//
// **conf_below 는 여기서 조용하다.** confidence 문턱 미달로 건너뛰는 것은
// 옳은 동작이고, 그 상태가 몇 시간 이어질 수 있다. 이 봇에서 "안 하고 있음"
// 자체는 알람이 될 수 없고 오직 "왜" 만 근거가 된다.
//
// 모르는 사유는 **알람이다.** 봇이 새 사유를 추가했는데 모니터가 모르면,
// 그 사유로 멈춘 봇이 조용해진다 — 정확히 이 패키지가 막으려는 상태다.
func skipFinding(r beat.SkipReason, consec int) []Finding {
	f := func(lv Level, format string, args ...any) []Finding {
		return []Finding{{
			Key: "skip:" + string(r), Level: lv,
			Message: fmt.Sprintf(format, args...),
		}}
	}
	switch r {
	case beat.SkipConfBelow:
		return nil
	case beat.SkipOutsideHours:
		// 거래 시간대가 아니다. 12시간 연속으로 이 사유가 나오는 것이 정상이라
		// 연속 횟수로도 알람을 올리지 않는다.
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
		return f(Crit, "알 수 없는 스킵 사유 %q — 봇이 모니터가 모르는 사유를 보냈다", r)
	}
	return nil
}

// openOrdersMax 는 알람에 적는 미체결 주문 수 상한이다.
//
// 상한이 필요한 이유: 미확인 주문은 cap/$1 개까지 늘 수 있고(equity_ceiling
// 주석 참고) 텔레그램 한 통은 4096자다. 상한이 없으면 사고가 큰 순간에
// 알람이 전송 실패로 통째로 사라진다 — 가장 필요한 때에.
const openOrdersMax = 8

// openOrdersNote 는 마지막으로 본 미체결 주문을 한 줄로 적는다. 없으면 빈
// 문자열이라 알람 문구가 그대로 유지된다.
func openOrdersNote(s *beat.Snapshot) string {
	os := s.Exposure.OpenOrders
	if len(os) == 0 {
		return ""
	}
	msg := fmt.Sprintf(" · 마지막으로 본 미체결 %d건:", len(os))
	for i, o := range os {
		if i >= openOrdersMax {
			msg += fmt.Sprintf(" 외 %d건", len(os)-openOrdersMax)
			break
		}
		msg += fmt.Sprintf(" %s(tick %d, $%.2f)", o.ID, o.Tick, o.Notional)
	}
	return msg
}
