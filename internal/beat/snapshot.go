// Package beat 는 봇과 모니터가 주고받는 계약이다.
//
// 봇(`cmd/gld91`)도 모니터(`cmd/gld91-monitor`)도 이 타입을 임포트한다.
// 계약을 두 벌로 두면 갈리고, 갈린 쪽이 틀렸을 때 알 방법이 없다 —
// `internal/live/rounds.go` 가 회차 선택을 한 곳으로 모은 것과 같은 이유다.
// 모니터를 별도 저장소로 두지 않은 것도 그래서다.
//
// 이 패키지는 순수하다. 네트워크도, 시계도, 전역 상태도 없다.
package beat

import "time"

// Command 는 모니터가 봇에게 시키는 것의 전부다.
type Command string

const (
	// CmdNone 은 아무것도 하지 않는다. 명령 없음을 빈 문자열이 아니라 값으로
	// 두는 이유: 빈 문자열은 "명령 없음"과 "필드가 빠졌다"를 구분하지 못한다.
	CmdNone Command = "none"
	// CmdShutdown 은 신규 회차 진입을 멈추고 종료한다. 이 봇은 매도 주문을
	// 내지 않으므로(스펙 §10) 청산 단계가 없다 — 미체결은 회차 끝에 취소되고
	// 미정산 포지션은 정산에 맡긴다.
	CmdShutdown Command = "shutdown"
	// CmdHalt 은 종료에 더해 **재기동해도 다시 멈추라**는 뜻이다. 봇이 다시
	// 올라와 첫 beat 를 보내면 모니터가 이것을 다시 내려 준다.
	CmdHalt Command = "halt"
)

// Valid 는 아는 명령인지 본다. 봇은 이것을 통과한 값만 처리한다 — 모니터가
// 오작동해 모르는 값을 보내도 봇은 자기가 아는 것만 한다.
func (c Command) Valid() bool {
	return c == CmdNone || c == CmdShutdown || c == CmdHalt
}

// Reply 는 beat 응답이다.
//
// **필드를 늘리지 마라.** 모니터는 정산 결과를 알지만 봇은 몰라야 한다 —
// `exec` 의 `TestExecNeverWritesSettlement` 가 지키는 벽이고, 그 벽이 있는
// 이유는 G2 가 잰 정산 불일치 `d≈0.30%` 때문이다. 모니터가 계산한 승패가
// 여기 실리면 그 벽에 뒷문이 생기고, 뒷문은 원래의 벽보다 찾기 어렵다.
// `snapshot_test.go` 의 TestReplyCarriesOnlyCommands 가 리플렉션으로 고정한다.
//
// # 왜 상관관계 식별자가 없는가
//
// **명령은 사건이 아니라 상태다.** 모니터는 봇이 받아 갈 때까지 같은 명령을
// 반복해 답하고(한 번만 싣고 그 응답이 유실되면 종료 요청이 조용히 사라진다),
// 봇은 값이 **바뀔 때만** 처리한다. 그래서 "이 응답이 몇 번 beat 에 대한
// 것인가" 를 담을 필요가 없다.
//
// 초안에는 `AckFor uint64` 가 있었고 봇이 그것으로 중복을 걸렀는데, 그 값은
// 매 beat 의 seq 라서 **반복 응답마다 달라진다** — 중복 제거가 전혀 동작하지
// 않았다. 상수 seq 를 쓰는 테스트가 그 사실을 가리고 있었다.
//
// 수신 확인은 반대 방향으로 흐른다 — [Snapshot.AckedCommand] 를 보라.
type Reply struct {
	Command Command `json:"command"`
}

// SkipReason 은 회차를 건너뛴 이유다.
//
// **사유 없는 스킵 카운터는 무가치하다.** 이 봇은 `confidence` 문턱
// (`live.ConfidenceThreshold` = 0.0172) 미달로 회차를 건너뛰는 것이 정상
// 동작이고, 그 상태가 몇 시간 이어질 수 있다. 그래서 "안 하고 있음" 자체는
// 알람이 될 수 없고, 오직 "왜 안 하는지"만 판단 근거가 된다.
type SkipReason string

const (
	// SkipConfBelow — confidence 문턱 미달. **정상이고 알람이 아니다.**
	SkipConfBelow SkipReason = "conf_below"
	// SkipSampleRejected — 표본이 채택되지 않았다(워밍업 부족·결측·도지).
	SkipSampleRejected SkipReason = "sample_rejected"
	// SkipOutsideHours — 거래 시간대가 아니다(cmd/gld91/hours.go).
	// **정상이고 알람이 아니다** — 하루의 절반은 이 사유로 조용하다.
	SkipOutsideHours SkipReason = "outside_hours"
	// SkipEquity — risk.CanArm 이 false 다(equity 가 약 $22 미만).
	SkipEquity SkipReason = "equity"
	// SkipDailyLimit — 일손실 한도.
	SkipDailyLimit SkipReason = "daily_limit"
	// SkipFetchError — 회차 메타데이터 조회 실패.
	SkipFetchError SkipReason = "fetch_error"
	// SkipPredictError — p_up 계산 실패(모델·피처·봉 조회).
	SkipPredictError SkipReason = "predict_error"
)

// Valid 는 아는 사유인지 본다. 모니터는 모르는 사유를 conf_below 처럼
// 조용히 넘기지 않고 알람으로 다룬다 — 봇이 새 사유를 추가했는데 모니터가
// 모르면, 그 사유로 멈춘 봇이 조용해진다.
func (s SkipReason) Valid() bool {
	switch s {
	case SkipConfBelow, SkipSampleRejected, SkipOutsideHours, SkipEquity,
		SkipDailyLimit, SkipFetchError, SkipPredictError:
		return true
	}
	return false
}

// RoundState 는 지금 회차에 무엇을 하고 있는지다.
type RoundState string

const (
	RoundActive  RoundState = "ACTIVE"  // 운용 중
	RoundSkipped RoundState = "SKIPPED" // 건너뜀 — 사유는 Round.SkipReason
	RoundIdle    RoundState = "IDLE"    // 열린 회차가 없다
)

// Consts 는 봇 바이너리에 박힌 상수다.
//
// 모니터가 파생 임계를 따로 계산하지 않고 이것을 받아 **예상값과 다르면
// 알린다.** 다르다는 것은 배포된 바이너리가 우리가 아는 그 바이너리가
// 아니라는 뜻이다. 모니터가 리터럴을 들고 있으면 봇 상수를 바꿨을 때 조용히
// 어긋나고, 그 어긋남은 아무도 모른다.
type Consts struct {
	CapFraction         float64 `json:"cap_fraction"`
	DailyFraction       float64 `json:"daily_fraction"`
	ConfidenceThreshold float64 `json:"confidence_threshold"`
	MinOrderUSD         float64 `json:"min_order_usd"`
}

// Equity 는 자본 상태다. 값은 전부 봇의 원장이다 — 모니터는 이것을
// 재계산하지 않는다(두 곳에서 계산하면 갈리고, 갈린 쪽이 틀렸을 때 알
// 방법이 없다).
type Equity struct {
	AvailableUSDT float64 `json:"available_usdt"`
	PositionCost  float64 `json:"position_cost"`
	CapUSD        float64 `json:"cap_usd"`
	CanArm        bool    `json:"can_arm"`
	DailyPnL      float64 `json:"daily_pnl"`
	// DailyLimit 은 **음수**다(손실 한도). 부호를 뒤집으면 판정이 통째로
	// 뒤집히므로 여기 적어 둔다 — `~/kdm/pmmm-go` 에서 부호 하나로 +40 인
	// 전략이 −90 으로 보고된 전례가 있다.
	DailyLimit float64 `json:"daily_limit"`
}

// Round 는 지금 회차다.
type Round struct {
	MarketID   int64      `json:"market_id"`
	Slug       string     `json:"slug"`
	EndsAt     time.Time  `json:"ends_at"`
	State      RoundState `json:"state"`
	PUp        float64    `json:"p_up"`
	Confidence float64    `json:"confidence"`
	Outcome    string     `json:"outcome"` // ledger.OutcomeUp | ledger.OutcomeDown
	SkipReason SkipReason `json:"skip_reason,omitempty"`
}

// OpenOrder 는 걸려 있는 주문 하나다.
//
// **개수가 아니라 목록으로 싣는다.** 개인키 없는 모니터는
// `GET /v1/orders` 를 부를 수 없어(JWT 필요) 미체결을 독립으로 관측할
// 수단이 없다. 봇이 죽으면 마지막 beat 의 이 목록이 유일한 정보원이고,
// 사람이 지갑으로 확인할 근거가 된다.
type OpenOrder struct {
	ID       string  `json:"id"`
	Tick     int64   `json:"tick"`
	Notional float64 `json:"notional"`
}

// Exposure 는 `exec` 의 노출 불변식 그대로다:
//
//	Filled + Open + PendingCancel < Cap
//
// PendingCancel 이 `exec` 에만 있는 항이다 — 취소를 요청했지만 거래소가
// 사라졌다고 확인해 주지 않은 주문은 아직 체결될 수 있다.
type Exposure struct {
	Filled float64 `json:"filled"`
	// FilledShares 는 체결 주수다. 명목과 별개로 싣는 이유: 이기면 주당
	// $1 이므로 배당이 주수로만 정해지고, **모니터는 봇의 원장을 읽을 수
	// 없다**(다른 호스트다). 이 값이 없으면 손익을 계산할 수 없다.
	FilledShares  float64     `json:"filled_shares"`
	Open          float64     `json:"open"`
	PendingCancel float64     `json:"pending_cancel"`
	Cap           float64     `json:"cap"`
	OpenOrders    []OpenOrder `json:"open_orders"`
	// Unaccounted 는 생성 결과를 모르고 식별자도 없어 취소조차 할 수 없는
	// 주문의 명목이다(`exec` 의 roundState.unknownNotional). 회차가 끝날
	// 때까지 노출에 남는다.
	Unaccounted float64 `json:"unaccounted"`
}

// Loop 는 집행 루프의 살아있음이다. 여기 값들이 정체하면 프로세스는 멀쩡한데
// 아무것도 하지 않는 상태다 — mtime 하트비트가 절대 잡지 못하는 고장이다.
type Loop struct {
	// Reprices 는 재호가 횟수다.
	//
	// **2026-08-12 이후로는 언제나 0 이다.** 봇이 회차마다 한 가격에 한 번만
	// 걸고 주문을 옮기지 않게 바뀌었다(exec 패키지 문서). 필드를 남겨 두는
	// 이유는 이것이 별도로 배포된 모니터와의 계약이기 때문이다 — 예전 스냅샷을
	// 읽을 때도 뜻이 같아야 한다.
	Reprices int64 `json:"reprices"`
	// LastActionAt 은 이 회차의 주문을 건 시각이다. 회차당 한 번만 움직인다.
	LastActionAt time.Time `json:"last_action_at"`
	// LastLoopAt 은 집행 루프가 마지막으로 한 바퀴를 돈 시각이다.
	//
	// **정체를 판정하는 값은 이쪽이다.** 위의 LastActionAt 은 정상 회차에서도
	// 멈춘다 — 한 번 걸고 군중이 안 움직이면 재호가할 이유가 없다. 실거래
	// 첫 회차가 그것을 바로 보여 줬다(2026-08-11 07:46, 91초 무행동 뒤 회차
	// 경계에서 복구). 프로세스가 멀쩡한데 루프만 멎은 고장은 이 값으로만
	// 보인다.
	//
	// 회차마다 제로에서 시작한다 — 제로는 "이 회차의 첫 바퀴가 아직" 이다.
	LastLoopAt time.Time `json:"last_loop_at"`
	// WSLastDataAt 은 마지막 **마켓 데이터** 수신 시각이다. 서버 하트비트는
	// 계속 오는데 데이터만 끊기는 고장이 실재한다(`ws/conn.go` 참고) —
	// 하트비트가 읽기 데드라인을 갱신하므로 소켓은 건강해 보인다.
	WSLastDataAt time.Time `json:"ws_last_data_at"`
	FillsPollAt  time.Time `json:"fills_poll_at"`
	// RateLimitRemaining 은 응답 헤더가 준 남은 예산이다. 240 req/min 은
	// 키 단위이고 봇이 그것을 독점해야 이 설계가 성립한다.
	RateLimitRemaining int `json:"ratelimit_remaining"`
}

// Snapshot 은 한 번의 beat 다.
type Snapshot struct {
	Seq uint64 `json:"seq"`
	// BootID 는 프로세스마다 새로 만드는 값이다.
	//
	// **mtime 하트비트는 크래시루프를 완벽히 건강하게 보고한다** — 3초마다
	// 죽고 살아나도 파일은 계속 신선하기 때문이다. 이 값의 변화가 재시작을
	// 드러내는 유일한 신호다.
	BootID  string    `json:"boot_id"`
	TS      time.Time `json:"ts"`
	Version string    `json:"version"`
	// AckedCommand 는 봇이 **실제로 받아서 처리한** 마지막 명령이다.
	//
	// 수신 확인이 이 방향으로 흐르는 이유: 모니터는 명령을 200 응답에 실을
	// 뿐 봇이 그것을 읽었는지 알 수 없다. 그런데 `/cancel_shutdown` 에
	// 정직하게 답하려면 그 구분이 필요하다 — 아직 안 받아 갔으면 취소할 수
	// 있고, 받아 갔으면 되돌릴 수 없다. 데이터가 봇→모니터로만 흐르므로
	// [Reply] 의 단방향 규약도 깨지 않는다.
	AckedCommand Command            `json:"acked_command,omitempty"`
	Armed        bool               `json:"armed"`
	Consts       Consts             `json:"consts"`
	Equity       Equity             `json:"equity"`
	Round        Round              `json:"round"`
	Exposure     Exposure           `json:"exposure"`
	Loop         Loop               `json:"loop"`
	Skips        map[SkipReason]int `json:"skips"`
}
