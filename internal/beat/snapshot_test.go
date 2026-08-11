package beat

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// 설계서 §9 의 불변식: **모니터 → 봇 채널에 데이터는 흐르지 않는다.**
//
// 모니터는 정산 결과를 알지만 봇은 몰라야 한다 — exec 의
// TestExecNeverWritesSettlement 가 지키는 벽이다. 그 값이 beat 응답에 실리면
// 벽에 뒷문이 생기고, 뒷문은 원래의 벽보다 찾기 어렵다. 그래서 필드 자체를
// 금지하고 리플렉션으로 고정한다 — 주석은 잊히지만 이 테스트는 잊히지 않는다.
func TestReplyCarriesOnlyCommands(t *testing.T) {
	allowed := map[string]bool{"Command": true}
	rt := reflect.TypeOf(Reply{})
	for i := 0; i < rt.NumField(); i++ {
		if name := rt.Field(i).Name; !allowed[name] {
			t.Errorf("Reply 에 %q 가 있다 — 응답은 명령만 싣는다", name)
		}
	}
	if rt.NumField() != len(allowed) {
		t.Errorf("Reply 필드 %d개, want %d — 허용 목록도 같이 고쳤는지 확인하라",
			rt.NumField(), len(allowed))
	}
}

func TestCommandsAreClosedSet(t *testing.T) {
	for _, c := range []Command{CmdNone, CmdShutdown, CmdHalt} {
		if !c.Valid() {
			t.Errorf("%q 가 유효하지 않다", c)
		}
	}
	// 빈 값이 통과하면 "필드가 빠진 응답"이 명령으로 읽힌다. 대소문자·공백을
	// 다듬어 주지 않는 것도 의도다 — cmd/gld91 의 LiveArmValue 와 같은 원칙이다.
	for _, c := range []Command{"", "restart", "SHUTDOWN", "none ", " none"} {
		if c.Valid() {
			t.Errorf("%q 가 유효로 통과했다", c)
		}
	}
}

// 스킵 사유는 닫힌 집합이다. 새 사유가 문자열로 슬며시 들어오면 모니터의
// 분기가 그것을 conf_below 와 같이 조용히 취급한다 — 그 사유로 멈춘 봇이
// 아무 알람도 내지 않게 된다.
func TestSkipReasonsAreClosedSet(t *testing.T) {
	all := []SkipReason{
		SkipConfBelow, SkipSampleRejected, SkipEquity,
		SkipDailyLimit, SkipFetchError, SkipPredictError,
	}
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
	for _, s := range []SkipReason{"", "unknown", "CONF_BELOW"} {
		if s.Valid() {
			t.Errorf("%q 가 유효로 통과했다", s)
		}
	}
}

// 스냅샷은 JSON 으로만 오간다. 필드가 조용히 빠지면 모니터가 제로값을 진짜
// 값으로 읽는다 — equity 0 은 "무장 불가"로, 노출 0 은 "위반 없음"으로
// 읽히고 둘 다 조용하다. 그래서 채워진 값 전체를 왕복시킨다.
func TestSnapshotRoundTrip(t *testing.T) {
	in := Snapshot{
		Seq: 42, BootID: "9f2c", TS: time.Unix(1786000000, 0).UTC(),
		Version: "gld91 abc1234", AckedCommand: CmdShutdown, Armed: true,
		Consts: Consts{
			CapFraction: 0.0455, DailyFraction: 0.10,
			ConfidenceThreshold: 0.0172, MinOrderUSD: 1.0,
		},
		Equity: Equity{
			AvailableUSDT: 63.2, PositionCost: 126.1, CapUSD: 8.62,
			CanArm: true, DailyPnL: -2.58, DailyLimit: -19.19,
		},
		Round: Round{
			MarketID: 70, Slug: "btc-updown-5m-1786275000",
			EndsAt: time.Unix(1786000300, 0).UTC(), State: RoundActive,
			PUp: 0.5314, Confidence: 0.0628, Outcome: "UP",
		},
		Exposure: Exposure{
			Filled: 41.2, Open: 18.0, PendingCancel: 6.1, Cap: 8.62,
			OpenOrders:  []OpenOrder{{ID: "0xabc", Tick: 487, Notional: 18.0}},
			Unaccounted: 1.5,
		},
		Loop: Loop{
			Reprices: 1840, LastActionAt: time.Unix(1786000010, 0).UTC(),
			WSLastDataAt:       time.Unix(1786000011, 0).UTC(),
			FillsPollAt:        time.Unix(1786000012, 0).UTC(),
			RateLimitRemaining: 118,
		},
		Skips: map[SkipReason]int{SkipConfBelow: 31, SkipEquity: 2},
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

// 스킵 사유가 없을 때 skip_reason 키가 아예 빠지는지 본다(omitempty).
// 빈 문자열이 실려 오면 모니터의 Valid() 검사가 "모르는 사유"로 읽어
// ACTIVE 회차마다 알람이 난다.
func TestSkipReasonOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(Snapshot{Round: Round{State: RoundActive}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	round := m["round"].(map[string]any)
	if _, ok := round["skip_reason"]; ok {
		t.Errorf("skip_reason 이 비었는데 실렸다: %v", round["skip_reason"])
	}
}
