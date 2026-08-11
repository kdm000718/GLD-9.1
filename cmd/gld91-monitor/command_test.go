package main

import (
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
	for _, want := range []string{"conf_below", "0.0031", "0.0172", "정상"} {
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

// 손익 집계가 없다는 것을 숨기지 않는다. 없는 것을 0 으로 찍으면 "손익 없음"
// 으로 읽히고, 그건 조용한 오답이다.
func TestStatusAdmitsMissingSettlement(t *testing.T) {
	st := stateWith(t, nil)
	reply, _ := routeCommand("/status", st, at())
	if !strings.Contains(reply, "아직 배선되지 않") {
		t.Errorf("정산 미배선을 숨긴다:\n%s", reply)
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
