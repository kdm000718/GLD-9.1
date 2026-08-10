package ws

import (
	"encoding/json"
	"testing"
)

// 하트비트 응답에는 requestId와 params가 없어야 한다.
// 여분의 필드가 붙으면 서버가 무효로 보고 다음 프로브에서 연결을 끊는다.
func TestHeartbeatReplyShape(t *testing.T) {
	b, err := json.Marshal(HeartbeatReply(1736696400000))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"method":"heartbeat","data":1736696400000}`
	if string(b) != want {
		t.Errorf("하트비트 응답 = %s, want %s", b, want)
	}

	// data가 0인 경우에도 필드가 사라지면 안 된다 (omitempty 함정).
	b0, _ := json.Marshal(HeartbeatReply(0))
	var m map[string]any
	if err := json.Unmarshal(b0, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["data"]; !ok {
		t.Error("data가 0일 때 필드가 누락됐다")
	}
}

// 서버 하트비트 프로브를 인식하고 타임스탬프를 그대로 꺼낼 수 있어야 한다.
func TestParseHeartbeatProbe(t *testing.T) {
	raw := `{"type":"M","topic":"heartbeat","data":1736696400000}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if !m.IsHeartbeat() {
		t.Fatal("하트비트로 인식되지 않았다")
	}
	var ts int64
	if err := json.Unmarshal(m.Data, &ts); err != nil {
		t.Fatal(err)
	}
	if ts != 1736696400000 {
		t.Errorf("ts = %d", ts)
	}
}

// 마켓 데이터는 하트비트로 오인되면 안 된다.
func TestMarketMessageIsNotHeartbeat(t *testing.T) {
	raw := `{"type":"M","topic":"predictOrderbook/123","data":{"marketId":123}}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	if m.IsHeartbeat() {
		t.Error("마켓 데이터가 하트비트로 인식됐다")
	}
}

// params에는 토픽이 하나만 들어가야 한다.
// 여러 개를 넣으면 첫 개만 구독되고 나머지는 조용히 무시된다.
func TestSubscribeRequestSingleTopic(t *testing.T) {
	b, err := json.Marshal(SubscribeRequest(7, "predictOrderbook/123"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"method":"subscribe","requestId":7,"params":["predictOrderbook/123"]}`
	if string(b) != want {
		t.Errorf("구독 요청 = %s, want %s", b, want)
	}
}

// 3개 토픽에 더해, 문서에 없지만 실재하는 predictTrades를 구독한다.
func TestTopics(t *testing.T) {
	got := Topics(12345)
	want := []string{
		"predictOrderbook/12345",
		"predictTradingStatus/12345",
		"predictMarketStatus/12345",
		"predictTrades/12345",
	}
	if len(got) != len(want) {
		t.Fatalf("토픽 %d개, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("토픽[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseTopic(t *testing.T) {
	tests := []struct {
		in       string
		kind     string
		marketID int64
		ok       bool
	}{
		{"predictOrderbook/12345", "predictOrderbook", 12345, true},
		{"predictTradingStatus/1", "predictTradingStatus", 1, true},
		{"heartbeat", "", 0, false},
		{"predictOrderbook/abc", "", 0, false},
		{"", "", 0, false},
		{"predictOrderbook/", "", 0, false},
	}
	for _, tc := range tests {
		kind, id, ok := ParseTopic(tc.in)
		if kind != tc.kind || id != tc.marketID || ok != tc.ok {
			t.Errorf("ParseTopic(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tc.in, kind, id, ok, tc.kind, tc.marketID, tc.ok)
		}
	}
}

// 알 수 없는 필드가 있어도 파싱이 죽으면 안 된다 (관대한 파싱).
func TestLenientParsing(t *testing.T) {
	raw := `{"type":"M","topic":"predictOrderbook/1","data":{"marketId":1},
	         "brandNewField":{"nested":[1,2,3]},"anotherOne":"x"}`
	var m Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unknown field 때문에 파싱이 실패했다: %v", err)
	}
	if m.Topic != "predictOrderbook/1" {
		t.Errorf("topic = %q", m.Topic)
	}
}
