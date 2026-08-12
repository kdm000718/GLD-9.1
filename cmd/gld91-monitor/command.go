package main

// 이 파일은 사람이 내리는 명령을 상태 변화로 옮긴다.
//
// **네트워크를 타지 않는다.** 전송은 telegram.go 의 몫이고, 여기 있는 것은
// 문자열 하나를 받아 상태를 바꾸고 답할 문자열을 돌려주는 함수뿐이다 —
// `internal/risk` 가 결정을 순수 함수로 떼어낸 것과 같은
// 이유다. 시험할 수 없는 명령 처리는 위험하다: 종료가 안 걸리는 것을 실거래
// 중에 발견하게 된다.

import (
	"fmt"
	"strings"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

const helpText = "📖 /status /round /why /pnl /shutdown /cancel_shutdown /halt /resume /help"

// routeCommand 는 명령 하나를 처리한다.
//
// handled 가 false 면 우리가 아는 명령이 아니다 — 호출자가 도움말을 보낸다.
// 빈 상태(nil)에서는 아무것도 처리하지 않는다: 배선이 덜 된 채로 명령을
// 받으면 패닉하는 대신 조용히 무시하는 편이 낫다.
func routeCommand(text string, s *state, now time.Time) (reply string, handled bool) {
	if s == nil {
		return "", false
	}
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return "", false
	}
	switch stripBotMention(fields[0]) {
	case "/status":
		return formatStatus(s, now), true
	case "/round":
		return formatRound(s, now), true
	case "/why":
		return formatWhy(s), true
	case "/pnl":
		snap, _ := s.Latest()
		return formatReport(accumulate(s.Participations()), snap, "누적"), true

	case "/shutdown":
		s.SetPending(beat.CmdShutdown)
		return "🛑 종료 요청됨. 봇이 신규 회차 진입을 멈추고 미체결을 전량 취소합니다.\n" +
			"미정산 포지션은 청산하지 않고 정산까지 보유합니다 — 이 봇은 매도 주문을 내지 않습니다.", true

	case "/halt":
		s.SetPending(beat.CmdHalt)
		return "⛔ halt 걸림. 재기동해도 첫 beat 에서 즉시 다시 멈춥니다. 풀려면 /resume.", true

	case "/resume":
		s.SetPending(beat.CmdNone)
		return "▶️ 명령 해제됨. 봇을 다시 기동하면 정상 운용합니다.", true

	case "/cancel_shutdown":
		// **ack 된 뒤에는 되돌릴 수 없다.**
		//
		// GLD-7 은 shutdown.flag 파일을 지우면 그만이었지만 여기서는 명령이
		// 이미 봇에게 전달됐다. 조용히 실패시키면 사용자는 취소된 줄 알고,
		// 몇 분 뒤 봇이 종료된 것을 보고 그제야 안다.
		if s.PendingAcked() {
			return "⚠️ 이미 봇이 명령을 받아 갔습니다 — 되돌릴 수 없습니다.\n" +
				"계속 운용하려면 봇을 재기동하세요. (halt 가 걸려 있다면 /resume 먼저)", true
		}
		if s.Pending() == beat.CmdNone {
			return "걸린 명령이 없습니다.", true
		}
		prev := s.Pending()
		s.SetPending(beat.CmdNone)
		return fmt.Sprintf("↩️ %s 취소됨 (봇이 아직 받아 가기 전).", prev), true

	case "/help":
		return helpText, true
	}
	return "", false
}

// formatStatus 는 마지막 스냅샷의 요약이다.
//
// 정산·손익은 모니터가 관측해 채운다(settle.go). 봇은 자기 실현손익을 모른다 —
// 정산 결과는 거래소에만 있고 봇에 조회 경로가 없다. 없는 것을 0 으로 찍지 않고
// 없다고 말하는 원칙은 그대로다: 표본이 0 이면 비율을 만들지 않는다.
func formatStatus(s *state, now time.Time) string {
	snap, last := s.Latest()
	if snap == nil {
		msg := "아직 봇의 beat 를 받지 못했습니다. 봇이 기동됐는지, MONITOR_BEAT_SECRET 이 양쪽에서 같은지 확인하세요."
		// **이력은 beat 와 독립이다.** 디스크에서 읽어 왔으므로 봇이 죽어
		// 있어도 답할 수 있고, 하필 그때가 지난 성적을 가장 보고 싶은
		// 순간이다. 이력이 비어 있으면 아무 말도 보태지 않는다.
		if t := accumulate(s.Participations()); t.Participated > 0 {
			msg += "\n\n" + formatCumulative(t)
		}
		return msg
	}
	var b strings.Builder
	fmt.Fprintf(&b, "💰 가용 %.2f / 미정산 취득원가 %.2f USDT\n", snap.Equity.AvailableUSDT, snap.Equity.PositionCost)
	// **일손실과 한도는 싣지 않는다.**
	//
	// 그 한도는 집행되지 않는다. `risk.DailyLimit.Breached` 는 시험 밖에서
	// 불리는 곳이 없고, `snapshotInput.DailyBreached` 를 참으로 만드는 곳도
	// 없다. 봇은 하루에 얼마를 잃든 멈추지 않는다.
	//
	// 값 자체도 없다. `DailyPnL` 은 buildSnapshot 이 채우지 않아 늘 0 인데,
	// 봇이 자기 실현손익을 모르기 때문이다 — 정산 결과는 거래소에만 있고
	// 봇에 조회 경로가 없다(모니터가 그 구멍을 메운 이유다).
	//
	// 그래서 이 줄은 "0 손실 / 한도 −10.03" 을 찍고 있었다. **보호받고 있다는
	// 인상만 주고 실제로는 아무것도 막지 않는다** — 없는 것을 0 으로 찍지
	// 않는다는 이 함수의 원칙(위 주석)을 그 줄이 스스로 어기고 있었다.
	// 한도를 집행하게 되면 그때 되살린다.
	fmt.Fprintf(&b, "   회차상한 %.2f\n", snap.Equity.CapUSD)
	fmt.Fprintf(&b, "📊 회차 %s (%s)\n", roundLabel(snap), snap.Round.Slug)
	fmt.Fprintf(&b, "   노출 %.2f = 체결 %.2f + 미체결 %.2f + 취소미확인 %.2f / cap %.2f\n",
		snap.Exposure.Filled+snap.Exposure.Open+snap.Exposure.PendingCancel,
		snap.Exposure.Filled, snap.Exposure.Open, snap.Exposure.PendingCancel, snap.Exposure.Cap)
	fmt.Fprintf(&b, "🛡 %s · 예산 %d/240 · 재호가 %d\n",
		armedLabel(snap.Armed), snap.Loop.RateLimitRemaining, snap.Loop.Reprices)
	fmt.Fprintf(&b, "💓 마지막 beat %.0f초 전 (seq %d, %s)\n", now.Sub(last).Seconds(), snap.Seq, snap.Version)
	b.WriteString(formatCumulative(accumulate(s.Participations())))
	if c := s.Pending(); c != beat.CmdNone && c != "" {
		fmt.Fprintf(&b, "\n📮 걸린 명령: %s (봇 수신 %v)", c, s.PendingAcked())
	}
	return b.String()
}

// formatCumulative 는 누적 참여 회차와 누적 승률 한 줄이다.
//
// **분모는 정산 건수다. 참여 건수가 아니다.** 참여로 나누면 아직 결과를 모르는
// 회차가 전부 패배로 계산되어 승률이 조용히 낮아진다 — 회차가 5분이라 언제나 몇
// 건은 미정산이므로 그 실수는 상시 켜져 있게 된다(accumulate 문서와 같은 이유).
//
// **표본이 0 이면 비율을 만들지 않는다.** 0/0 은 NaN 이고, NaN 이 리포트에 실리면
// 사람은 그것을 0% 로 읽는다 — 이 파일이 일손실 한도 줄을 뺀 것과 같은 원칙이다.
//
// **누적이 언제부터인지를 적는다.** 구간을 빼면 "누적 참여 1회차" 가 전체 기간의
// 값으로 읽히고, 그 오해는 승률이 낮을 때 가장 비싸다. 이력이 디스크에 남게 된
// 뒤로는 "모니터 기동 이후" 라고 쓸 수 없다 — 재기동해도 이어지므로 그 말이
// 거짓이 된다(store.go). 그래서 이력에 실제로 들어 있는 가장 오래된 회차를
// 그대로 적는다. 시각은 UTC 다. 봇의 원장·모니터 로그가 전부 UTC 이므로
// 여기만 현지시로 찍으면 대조할 때 다섯 시간을 손으로 빼야 한다.
func formatCumulative(t tally) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📈 누적 참여 %d회차", t.Participated)
	if !t.Since.IsZero() {
		fmt.Fprintf(&b, " (%s UTC 부터)", t.Since.UTC().Format("01-02 15:04"))
	}
	if t.Settled > 0 {
		fmt.Fprintf(&b, "\n   적중 %d/%d = %.1f%% · 대기 %d · 손익 %+.4f USDT",
			t.Hits, t.Settled, 100*float64(t.Hits)/float64(t.Settled), t.Pending, t.PnL)
	} else if t.Participated > 0 {
		fmt.Fprintf(&b, "\n   아직 정산된 회차가 없습니다 (대기 %d)", t.Pending)
	}
	return b.String()
}

// formatRound 는 지금 회차의 실황이다.
func formatRound(s *state, now time.Time) string {
	snap, _ := s.Latest()
	if snap == nil {
		return "아직 봇의 beat 를 받지 못했습니다."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "회차 %s — %s\n", snap.Round.Slug, roundLabel(snap))
	if !snap.Round.EndsAt.IsZero() {
		fmt.Fprintf(&b, "종료까지 %.0f초\n", snap.Round.EndsAt.Sub(now).Seconds())
	}
	fmt.Fprintf(&b, "p_up %.4f · confidence %.4f (문턱 %.4f) · 방향 %s\n",
		snap.Round.PUp, snap.Round.Confidence, snap.Consts.ConfidenceThreshold, snap.Round.Outcome)
	if n := len(snap.Exposure.OpenOrders); n > 0 {
		fmt.Fprintf(&b, "미체결 %d건:\n", n)
		for _, o := range snap.Exposure.OpenOrders {
			fmt.Fprintf(&b, "  틱 %d · $%.2f · %s\n", o.Tick, o.Notional, o.ID)
		}
	} else {
		b.WriteString("미체결 없음\n")
	}
	if snap.Exposure.Unaccounted > 0 {
		fmt.Fprintf(&b, "⚠️ 식별자 없는 주문 명목 $%.2f — 취소할 수 없습니다\n", snap.Exposure.Unaccounted)
	}
	return b.String()
}

// formatWhy 는 "왜 안 하고 있냐" 에 답한다.
//
// 이 봇에서 가장 자주 나올 질문이다 — confidence 문턱 미달로 몇 시간 조용한
// 것이 정상 동작이기 때문이다. GLD-7 에는 그 답을 줄 명령이 없어서 매번
// SSH 로 로그를 봐야 했다.
func formatWhy(s *state) string {
	snap, _ := s.Latest()
	if snap == nil {
		return "아직 봇의 beat 를 받지 못했습니다."
	}
	consec := s.ConsecSkips()

	switch snap.Round.State {
	case beat.RoundActive:
		return fmt.Sprintf("지금 회차 %s 를 운용 중입니다.\np_up %.4f · confidence %.4f ≥ 문턱 %.4f",
			snap.Round.Slug, snap.Round.PUp, snap.Round.Confidence, snap.Consts.ConfidenceThreshold)
	case beat.RoundIdle:
		return "열린 회차가 없습니다."
	}

	r := snap.Round.SkipReason
	var b strings.Builder
	fmt.Fprintf(&b, "건너뜀: %s (같은 사유 연속 %d회차)\n", r, consec[r])
	switch r {
	case beat.SkipConfBelow:
		fmt.Fprintf(&b, "confidence %.4f < 문턱 %.4f\n", snap.Round.Confidence, snap.Consts.ConfidenceThreshold)
		b.WriteString("← 정상입니다. 문턱 미달이면 아무것도 하지 않는 것이 옳습니다.")
	case beat.SkipSampleRejected:
		b.WriteString("표본이 채택되지 않았습니다 (워밍업 부족·결측·도지). 봉 데이터를 확인하세요.")
	case beat.SkipEquity:
		fmt.Fprintf(&b, "회차상한 %.2f 가 최소 주문 %.2f 이하입니다.", snap.Equity.CapUSD, snap.Consts.MinOrderUSD)
	case beat.SkipDailyLimit:
		fmt.Fprintf(&b, "일손실 %.2f 가 한도 %.2f 에 닿았습니다.", snap.Equity.DailyPnL, snap.Equity.DailyLimit)
	case beat.SkipFetchError:
		b.WriteString("회차 메타데이터 조회에 실패했습니다.")
	case beat.SkipPredictError:
		b.WriteString("p_up 계산에 실패했습니다 (모델·피처·봉 조회).")
	default:
		b.WriteString("⚠️ 모니터가 모르는 사유입니다 — 봇이 새 사유를 추가했는지 확인하세요.")
	}
	return b.String()
}

// stripBotMention 은 `/status@GLD9_1bot` 에서 `@…` 를 떼어낸다.
//
// **그룹 채팅에서는 클라이언트가 봇 이름을 붙여 보낸다.** 그것을 그대로
// 비교하면 그룹에서 명령이 하나도 먹지 않는다 — 1:1 채팅에서만 시험하면
// 절대 드러나지 않고, 정작 긴급할 때 /shutdown 이 도움말만 돌려준다.
func stripBotMention(cmd string) string {
	if i := strings.IndexByte(cmd, '@'); i >= 0 {
		return cmd[:i]
	}
	return cmd
}

func roundLabel(snap *beat.Snapshot) string {
	if snap.Round.State == beat.RoundSkipped {
		return "건너뜀 (" + string(snap.Round.SkipReason) + ")"
	}
	return string(snap.Round.State)
}

func armedLabel(armed bool) string {
	if armed {
		return "ARMED"
	}
	return "DRY-RUN"
}
