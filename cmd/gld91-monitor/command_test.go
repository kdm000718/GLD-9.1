package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

func at() time.Time { return time.Unix(1786000000, 0).UTC() }

// stateWith 는 스냅샷 하나를 받은 모니터다.
func stateWith(t *testing.T, mut func(*beat.Snapshot)) *state {
	t.Helper()
	st := newTestState()
	s := healthySnapshot()
	s.Seq, s.BootID = 1, "a"
	if mut != nil {
		mut(s)
	}
	if code, _ := post(t, st, *s); code != 200 {
		t.Fatalf("스냅샷 주입 실패: %d", code)
	}
	return st
}

func TestUnknownCommandIsNotHandled(t *testing.T) {
	st := stateWith(t, nil)
	for _, text := range []string{"", "   ", "hello", "/nope", "status"} {
		if reply, handled := routeCommand(text, st, at()); handled {
			t.Errorf("%q 가 처리됐다: %q", text, reply)
		}
	}
}

// nil 상태에서 패닉하지 않는다 — 배선이 덜 된 채로 명령이 오면 조용히
// 무시하는 편이 낫다.
func TestNilStateIsNotHandled(t *testing.T) {
	if _, handled := routeCommand("/status", nil, at()); handled {
		t.Error("nil 상태에서 명령이 처리됐다")
	}
}

func TestShutdownAndHaltSetPending(t *testing.T) {
	st := stateWith(t, nil)
	if _, handled := routeCommand("/shutdown", st, at()); !handled {
		t.Fatal("/shutdown 이 처리되지 않았다")
	}
	if st.Pending() != beat.CmdShutdown {
		t.Errorf("pending = %q", st.Pending())
	}
	if _, handled := routeCommand("/halt", st, at()); !handled {
		t.Fatal("/halt 이 처리되지 않았다")
	}
	if st.Pending() != beat.CmdHalt {
		t.Errorf("pending = %q", st.Pending())
	}
	routeCommand("/resume", st, at())
	if st.Pending() != beat.CmdNone {
		t.Errorf("resume 뒤 pending = %q", st.Pending())
	}
}

// **ack 된 뒤에는 되돌릴 수 없다.** 조용히 실패시키면 사용자는 취소된 줄
// 알고, 몇 분 뒤 봇이 종료된 것을 보고 그제야 안다.
func TestCancelShutdownAfterAckIsRefused(t *testing.T) {
	st := stateWith(t, nil)
	routeCommand("/shutdown", st, at())

	// 봇이 받아 갔다고 보고한다.
	s := healthySnapshot()
	s.Seq, s.BootID, s.AckedCommand = 2, "a", beat.CmdShutdown
	post(t, st, *s)

	reply, handled := routeCommand("/cancel_shutdown", st, at())
	if !handled {
		t.Fatal("처리되지 않았다")
	}
	if !strings.Contains(reply, "되돌릴 수 없") || !strings.Contains(reply, "재기동") {
		t.Errorf("거절 사유가 불명확하다: %q", reply)
	}
	if st.Pending() != beat.CmdShutdown {
		t.Errorf("ack 된 종료가 취소됐다: pending = %q", st.Pending())
	}
}

func TestCancelShutdownBeforeAckWorks(t *testing.T) {
	st := stateWith(t, nil)
	routeCommand("/shutdown", st, at())

	reply, handled := routeCommand("/cancel_shutdown", st, at())
	if !handled {
		t.Fatal("처리되지 않았다")
	}
	if st.Pending() != beat.CmdNone {
		t.Errorf("pending = %q, want none", st.Pending())
	}
	if !strings.Contains(reply, "취소") {
		t.Errorf("응답이 취소를 말하지 않는다: %q", reply)
	}
}

func TestCancelWithNothingPending(t *testing.T) {
	st := stateWith(t, nil)
	reply, handled := routeCommand("/cancel_shutdown", st, at())
	if !handled {
		t.Fatal("처리되지 않았다")
	}
	if !strings.Contains(reply, "걸린 명령이 없") {
		t.Errorf("응답 = %q", reply)
	}
}

// /why 가 이 봇에서 가장 자주 나올 질문에 답한다. confidence 미달은
// **정상이라고 말해야 한다** — 그러지 않으면 사용자가 매번 고장으로 읽는다.
func TestWhyExplainsConfBelowAsNormal(t *testing.T) {
	st := stateWith(t, func(s *beat.Snapshot) {
		s.Round.State = beat.RoundSkipped
		s.Round.SkipReason = beat.SkipConfBelow
		s.Round.Confidence = 0.0031
	})
	reply, handled := routeCommand("/why", st, at())
	if !handled {
		t.Fatal("처리되지 않았다")
	}
	// 문턱 값은 스냅샷이 실어 오는 것이므로 여기서 리터럴로 박지 않는다 —
	// live.ConfidenceThreshold 가 바뀔 때마다 이 시험이 깨지면 안 된다.
	for _, want := range []string{"conf_below", "0.0031", "문턱", "정상"} {
		if !strings.Contains(reply, want) {
			t.Errorf("응답에 %q 가 없다:\n%s", want, reply)
		}
	}
}

func TestWhyCountsConsecutiveSkips(t *testing.T) {
	st := newTestState()
	for seq := uint64(1); seq <= 12; seq++ {
		s := healthySnapshot()
		s.Seq, s.BootID = seq, "a"
		s.Round.State, s.Round.SkipReason = beat.RoundSkipped, beat.SkipConfBelow
		post(t, st, *s)
	}
	reply, _ := routeCommand("/why", st, at())
	if !strings.Contains(reply, "12") {
		t.Errorf("연속 12회차가 안 보인다:\n%s", reply)
	}
}

// 사유마다 다른 설명이 나와야 한다. 전부 같은 문장이면 /why 가 아무것도
// 알려주지 않는다.
func TestWhyExplainsEachReason(t *testing.T) {
	cases := map[beat.SkipReason]string{
		beat.SkipSampleRejected: "표본",
		beat.SkipEquity:         "최소 주문",
		beat.SkipDailyLimit:     "일손실",
		beat.SkipFetchError:     "회차 메타데이터",
		beat.SkipPredictError:   "p_up",
		beat.SkipReason("zzz"):  "모르는 사유",
	}
	for reason, want := range cases {
		st := stateWith(t, func(s *beat.Snapshot) {
			s.Round.State = beat.RoundSkipped
			s.Round.SkipReason = reason
		})
		reply, _ := routeCommand("/why", st, at())
		if !strings.Contains(reply, want) {
			t.Errorf("%s 설명에 %q 가 없다:\n%s", reason, want, reply)
		}
	}
}

func TestWhyOnActiveRound(t *testing.T) {
	st := stateWith(t, nil) // healthySnapshot 은 ACTIVE 다
	reply, _ := routeCommand("/why", st, at())
	if !strings.Contains(reply, "운용 중") {
		t.Errorf("응답 = %q", reply)
	}
}

// beat 를 못 받았으면 그렇게 말한다. 빈 값을 0 으로 찍으면 "equity 0" 이
// 진짜 잔고로 읽힌다.
func TestCommandsBeforeFirstBeat(t *testing.T) {
	st := newTestState()
	for _, cmd := range []string{"/status", "/round", "/why"} {
		reply, handled := routeCommand(cmd, st, at())
		if !handled {
			t.Fatalf("%s 가 처리되지 않았다", cmd)
		}
		if !strings.Contains(reply, "받지 못했") {
			t.Errorf("%s 응답이 beat 부재를 말하지 않는다: %q", cmd, reply)
		}
	}
}

// **「정산·손익 집계는 아직 배선되지 않았습니다」는 되살아나면 안 된다.**
//
// 그 문장은 settle.go 와 **같은 커밋**(496cb12)에 들어왔다 — 배선이 끝난 시점에
// 이미 거짓이었다. 운영에서도 모니터가 `정산 조회: … 반영 1건` 을 찍는다.
// 배선된 것을 미배선이라 적으면 사람은 화면의 승률을 자리표시자로 읽고 무시한다.
//
// 원래의 원칙(없는 것을 0 으로 찍지 않는다)은 버리지 않았다. 표본이 0 일 때
// 비율을 만들지 않는 것으로 지킨다 — TestStatusWinRateAbsentWhenNothingSettled.
func TestStatusDoesNotClaimSettlementIsUnwired(t *testing.T) {
	st := stateWith(t, nil)
	reply, _ := routeCommand("/status", st, at())
	if strings.Contains(reply, "아직 배선되지 않") {
		t.Errorf("배선이 끝난 정산을 미배선이라고 말한다:\n%s", reply)
	}
}

// **집행되지 않는 한도를 보여주면 안 된다.**
//
// `risk.DailyLimit.Breached` 는 시험 밖에서 불리는 곳이 없고 `DailyBreached` 를
// 참으로 만드는 곳도 없다 — 봇은 하루에 얼마를 잃든 멈추지 않는다. 게다가
// `DailyPnL` 은 스냅샷에 채워지지 않아 늘 0 이다(봇은 자기 실현손익을 모른다).
// 그 둘을 나란히 찍으면 "0 손실 / 한도 −10.03" 이 되어 **보호받고 있다는
// 인상만 준다.** 한도를 실제로 집행하게 되면 그때 되살린다.
func TestStatusHidesUnenforcedDailyLimit(t *testing.T) {
	st := stateWith(t, func(s *beat.Snapshot) {
		s.Equity.DailyPnL, s.Equity.DailyLimit = 0, -10.03
	})
	reply, _ := routeCommand("/status", st, at())
	for _, bad := range []string{"일손실", "한도", "-10.03"} {
		if strings.Contains(reply, bad) {
			t.Errorf("집행되지 않는 한도를 %q 로 보여준다:\n%s", bad, reply)
		}
	}
	// 회차상한은 남아야 한다 — 이쪽은 실제로 집행된다(exec 의 노출 불변식).
	if !strings.Contains(reply, "회차상한") {
		t.Errorf("실제로 집행되는 회차상한까지 사라졌다:\n%s", reply)
	}
}

// 노출 3항이 전부 보여야 한다. 취소미확인이 빠지면 exec 의 불변식 중 그 봇에만
// 있는 항을 사람이 볼 수 없다.
func TestStatusShowsAllThreeExposureTerms(t *testing.T) {
	st := stateWith(t, func(s *beat.Snapshot) {
		s.Exposure = beat.Exposure{Filled: 1.5, Open: 2.25, PendingCancel: 0.75, Cap: 8.62}
	})
	reply, _ := routeCommand("/status", st, at())
	for _, want := range []string{"1.50", "2.25", "0.75", "8.62", "취소미확인"} {
		if !strings.Contains(reply, want) {
			t.Errorf("응답에 %q 가 없다:\n%s", want, reply)
		}
	}
}

// 걸린 명령과 그 수신 여부가 /status 에 보인다.
func TestStatusShowsPendingCommand(t *testing.T) {
	st := stateWith(t, nil)
	routeCommand("/shutdown", st, at())
	reply, _ := routeCommand("/status", st, at())
	if !strings.Contains(reply, "shutdown") {
		t.Errorf("걸린 명령이 안 보인다:\n%s", reply)
	}
}

// /round 는 미체결 목록을 실제로 나열한다. 개인키 없는 모니터에게 이것이
// 유일한 미체결 정보원이므로 개수만 보여주면 안 된다.
func TestRoundListsOpenOrders(t *testing.T) {
	st := stateWith(t, func(s *beat.Snapshot) {
		s.Exposure.OpenOrders = []beat.OpenOrder{
			{ID: "0xaaa", Tick: 47, Notional: 9.4},
			{ID: "0xbbb", Tick: 46, Notional: 4.6},
		}
		s.Exposure.Unaccounted = 1.25
	})
	reply, _ := routeCommand("/round", st, at())
	for _, want := range []string{"0xaaa", "0xbbb", "47", "9.40", "1.25", "취소할 수 없"} {
		if !strings.Contains(reply, want) {
			t.Errorf("응답에 %q 가 없다:\n%s", want, reply)
		}
	}
}

func TestHelpListsEveryCommand(t *testing.T) {
	st := stateWith(t, nil)
	reply, handled := routeCommand("/help", st, at())
	if !handled {
		t.Fatal("처리되지 않았다")
	}
	// 도움말에 적힌 명령은 전부 실제로 처리돼야 한다 — 없는 명령을 안내하면
	// 사용자가 그것을 믿고 긴급 상황에서 친다.
	for _, cmd := range strings.Fields(reply) {
		if !strings.HasPrefix(cmd, "/") {
			continue
		}
		if _, ok := routeCommand(cmd, st, at()); !ok {
			t.Errorf("도움말이 안내한 %s 가 처리되지 않는다", cmd)
		}
	}
}

// **그룹 채팅에서는 클라이언트가 `/status@GLD9_1bot` 로 보낸다.** 그것을
// 그대로 비교하면 그룹에서 명령이 하나도 먹지 않는다 — 1:1 채팅에서만
// 시험하면 절대 드러나지 않고, 정작 긴급할 때 /shutdown 이 도움말만 돌려준다.
func TestGroupChatCommandsWithBotMention(t *testing.T) {
	for _, text := range []string{
		"/status@GLD9_1bot",
		"/why@GLD9_1bot",
		"/shutdown@GLD9_1bot",
		"/help@GLD9_1bot extra args",
	} {
		st := stateWith(t, nil)
		if _, handled := routeCommand(text, st, at()); !handled {
			t.Errorf("%q 가 처리되지 않았다 — 그룹에서 명령이 먹지 않는다", text)
		}
	}
	// 봇 이름이 붙어도 실제 동작이 같아야 한다.
	st := stateWith(t, nil)
	if _, ok := routeCommand("/shutdown@GLD9_1bot", st, at()); !ok {
		t.Fatal("처리되지 않았다")
	}
	if st.Pending() != beat.CmdShutdown {
		t.Errorf("pending = %q, want shutdown", st.Pending())
	}
}

// 명령이 아닌 것에 @ 가 있어도 오작동하지 않는다.
func TestMentionStripDoesNotInventCommands(t *testing.T) {
	st := stateWith(t, nil)
	for _, text := range []string{"@GLD9_1bot", "hello@world", "/nope@GLD9_1bot"} {
		if _, handled := routeCommand(text, st, at()); handled {
			t.Errorf("%q 가 명령으로 처리됐다", text)
		}
	}
}

// ---------------------------------------------------------------------------
// 누적 참여·승률
// ---------------------------------------------------------------------------

// playRounds 는 회차 n 개를 걸고 그중 앞의 settled 개를 정산시킨다.
// hits 개가 적중이다. 슬러그는 5분 격자 위에 있어야 한다(rest.ParseSlugStart).
func playRounds(t *testing.T, st *state, n, settled, hits int) {
	t.Helper()
	var ss []settlement
	for i := 0; i < n; i++ {
		start := int64(1786500000 + i*300)
		slug := fmt.Sprintf("btc-updown-5m-%d", start)
		s := healthySnapshot()
		s.Seq, s.BootID = uint64(i+1), "a"
		s.Round.Slug = slug
		// **종료 시각을 슬러그와 맞춘다.** healthySnapshot 은 EndsAt 을 "지금
		// +2분" 으로 두는데, 그러면 누적 구간의 시작이 회차와 무관한 값이 된다.
		s.Round.EndsAt = time.Unix(start+300, 0).UTC()
		s.Round.Outcome = "Up"
		s.Exposure.Filled, s.Exposure.FilledShares = 3.29, 7
		if code, _ := post(t, st, *s); code != 200 {
			t.Fatalf("%s 주입 실패: %d", slug, code)
		}
		if i < settled {
			won := "Down" // 빗나감
			if i < hits {
				won = "Up"
			}
			ss = append(ss, settlement{Slug: slug, WonName: won, SettledAt: at()})
		}
	}
	if got := st.ApplySettlements(ss); got != settled {
		t.Fatalf("반영된 정산 %d건, want %d", got, settled)
	}
}

// /status 는 누적 참여 회차와 누적 승률을 싣는다.
func TestStatusShowsCumulativeParticipationAndWinRate(t *testing.T) {
	st := newTestState()
	playRounds(t, st, 5, 4, 3)

	reply, handled := routeCommand("/status", st, at())
	if !handled {
		t.Fatal("/status 가 처리되지 않았다")
	}
	for _, want := range []string{"누적 참여 5회차", "적중 3/4", "75.0%"} {
		if !strings.Contains(reply, want) {
			t.Errorf("%q 가 없다:\n%s", want, reply)
		}
	}
}

// **미정산 회차는 승률 분모에 들어가지 않는다.** 들어가면 아직 결과를 모르는
// 회차가 전부 패배로 계산되어 승률이 조용히 낮아진다 — 5분 회차라 언제나 몇
// 건은 미정산이므로 이 실수는 상시 켜져 있게 된다.
func TestStatusWinRateDenominatorIsSettledNotParticipated(t *testing.T) {
	st := newTestState()
	playRounds(t, st, 10, 2, 1)

	reply, _ := routeCommand("/status", st, at())
	if !strings.Contains(reply, "적중 1/2") {
		t.Errorf("분모가 정산 건수가 아니다:\n%s", reply)
	}
	if strings.Contains(reply, "/10") || strings.Contains(reply, "10.0%") {
		t.Errorf("참여 건수가 분모로 쓰였다:\n%s", reply)
	}
}

// 정산된 회차가 하나도 없으면 비율을 계산하지 않는다. 0/0 은 NaN 이고,
// NaN 이 실리면 사람은 그것을 0% 로 읽는다.
func TestStatusWinRateAbsentWhenNothingSettled(t *testing.T) {
	st := newTestState()
	playRounds(t, st, 3, 0, 0)

	reply, _ := routeCommand("/status", st, at())
	if !strings.Contains(reply, "누적 참여 3회차") {
		t.Errorf("참여 회차가 없다:\n%s", reply)
	}
	if strings.Contains(strings.ToLower(reply), "nan") || strings.Contains(reply, "적중 0/0") {
		t.Errorf("표본 0 에서 비율을 계산했다:\n%s", reply)
	}
}

// 한 건도 걸지 않았어도 /status 는 그 사실을 말한다 — 줄이 통째로 사라지면
// "집계가 배선되지 않은 것" 과 "아직 안 건 것" 이 구분되지 않는다.
func TestStatusShowsZeroParticipation(t *testing.T) {
	st := stateWith(t, nil) // healthySnapshot 은 Slug 가 비어 이력에 남지 않는다
	reply, _ := routeCommand("/status", st, at())
	if !strings.Contains(reply, "누적 참여 0회차") {
		t.Errorf("참여 0 이 표시되지 않았다:\n%s", reply)
	}
}

// **누적이 언제부터인지 적는다.** 구간을 빼면 "누적 참여 1회차" 가 전체
// 기간의 값으로 읽힌다.
//
// 「모니터 기동 이후」라고 쓰면 안 된다 — 이력이 디스크에 남게 된 뒤로는
// 재기동해도 이어지므로 그 말이 거짓이다(store.go).
func TestStatusSaysWhenCumulativeStarts(t *testing.T) {
	st := newTestState()
	playRounds(t, st, 2, 1, 1)

	reply, _ := routeCommand("/status", st, at())
	// playRounds 의 첫 회차는 1786500000 (2026-08-12 02:00Z), 종료는 +300초다.
	if !strings.Contains(reply, "08-12 02:05 UTC 부터") {
		t.Errorf("누적의 시작 시각이 적히지 않았다:\n%s", reply)
	}
	if strings.Contains(reply, "기동 이후") {
		t.Errorf("이력이 재기동을 넘어 남는데 「기동 이후」라고 말한다:\n%s", reply)
	}
}
