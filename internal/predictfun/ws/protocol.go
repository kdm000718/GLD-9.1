package ws

import (
	"encoding/json"
	"strconv"
	"strings"
)

// 메시지 타입.
const (
	TypeResponse = "R" // 구독/해제 응답
	TypeMessage  = "M" // 마켓 데이터 또는 하트비트
)

// TopicHeartbeat은 서버 하트비트 프로브의 topic 값이다.
const TopicHeartbeat = "heartbeat"

// 에러 코드.
const (
	ErrInvalidJSON        = "invalid_json"
	ErrInvalidTopic       = "invalid_topic"
	ErrInvalidCredentials = "invalid_credentials"
	ErrInternalServer     = "internal_server_error"
)

// Request는 클라이언트가 보내는 프레임이다.
//
// 하트비트 응답에는 requestId와 params가 없어야 하므로 omitempty가 필수다.
type Request struct {
	Method    string   `json:"method"`
	RequestID uint64   `json:"requestId,omitempty"`
	Params    []string `json:"params,omitempty"`
	Data      *int64   `json:"data,omitempty"`
}

// SubscribeRequest는 토픽 하나를 구독한다.
//
// params 배열의 첫 항목만 처리되므로 배치 구독은 불가능하다.
// 토픽 수만큼 프레임을 보내야 한다.
func SubscribeRequest(id uint64, topic string) Request {
	return Request{Method: "subscribe", RequestID: id, Params: []string{topic}}
}

func UnsubscribeRequest(id uint64, topic string) Request {
	return Request{Method: "unsubscribe", RequestID: id, Params: []string{topic}}
}

// HeartbeatReply는 서버가 보낸 타임스탬프를 그대로 되돌린다.
// 값이 없거나 오래됐거나 불일치하면 다음 프로브에서 연결이 끊긴다.
func HeartbeatReply(ts int64) Request {
	return Request{Method: "heartbeat", Data: &ts}
}

// WSError는 응답 프레임의 오류다.
type WSError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Message는 서버가 보내는 프레임이다.
//
// Data는 하트비트에서는 숫자, 마켓 데이터에서는 객체다. 관대한 파싱을 위해
// RawMessage로 받아 topic에 따라 분기한다.
type Message struct {
	Type      string          `json:"type"`
	Topic     string          `json:"topic"`
	RequestID uint64          `json:"requestId"`
	Success   *bool           `json:"success"`
	Error     *WSError        `json:"error"`
	Data      json.RawMessage `json:"data"`
}

// IsHeartbeat은 서버 하트비트 프로브인지 판별한다.
func (m *Message) IsHeartbeat() bool {
	return m.Type == TypeMessage && m.Topic == TopicHeartbeat
}

// 토픽 이름 생성. 마켓당 3개를 구독한다.
func TopicOrderbook(marketID int64) string     { return topic("predictOrderbook", marketID) }
func TopicTradingStatus(marketID int64) string { return topic("predictTradingStatus", marketID) }
func TopicMarketStatus(marketID int64) string  { return topic("predictMarketStatus", marketID) }

// TopicTrades는 체결 스트림이다.
//
// 공식 문서의 토픽 목록에는 없지만 mainnet에서 정상 동작한다 (실측 확인).
// 오더북의 lastOrderSettled와 달리 타임스탬프·수량·테이커/메이커가 모두 들어온다.
func TopicTrades(marketID int64) string { return topic("predictTrades", marketID) }

// Topics는 한 마켓에 대해 구독할 토픽 전체를 돌려준다 (마켓당 4개).
func Topics(marketID int64) []string {
	return []string{
		TopicOrderbook(marketID),
		TopicTradingStatus(marketID),
		TopicMarketStatus(marketID),
		TopicTrades(marketID),
	}
}

func topic(prefix string, marketID int64) string {
	return prefix + "/" + strconv.FormatInt(marketID, 10)
}

// ParseTopic은 "predictOrderbook/12345"를 종류와 마켓 ID로 분해한다.
func ParseTopic(t string) (kind string, marketID int64, ok bool) {
	i := strings.IndexByte(t, '/')
	if i < 0 {
		return "", 0, false
	}
	id, err := strconv.ParseInt(t[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return t[:i], id, true
}
