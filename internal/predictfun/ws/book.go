// Package ws의 Book은 한 마켓의 오더북 상태를 유지하고, 우리 주문을 뺀
// 최우선 호가를 계산한다.
//
// 가격은 항상 정수 틱으로 다룬다(SPEC.md §8-11과 동일한 규칙). float를 그대로
// 비교하거나 자르면 predict.fun 서버의 반올림과 어긋나 한 틱씩 밀린다.
package ws

import (
	"encoding/json"
	"fmt"
	"math"
	"sync"
)

// Level은 [가격, 수량] 쌍이다. settlementsPending처럼 최우선 호가 계산에
// 쓰지 않는 보조 데이터를 그대로 들고 있을 때만 쓴다. 호가창 본체(bids/asks)는
// 가격틱 → Shares 맵으로 다룬다.
type Level struct {
	Price float64
	Size  float64
}

// QtyDecimals는 수량 고정소수점(Shares)의 소수 자릿수다. 실측 수량이 소수이고
// (553.337 / 100.2 / 2.0142 / 0.071) 최대 4자리까지 관측됐다 — 6자리면 여유가 있다.
const QtyDecimals = 6

// Shares는 고정소수점 수량이다.
//
// 자연 단위 float를 그대로 담지 못하도록, 또는 다른 정밀도로 스케일된 int64와
// 섞이지 못하도록 별도 타입으로 둔다. 호가창 수량과 exclude 수량의 단위가
// 어긋나면(예: 한쪽은 원시 수량, 한쪽은 배율 적용) 뺄셈이 조용히 틀린 값을
// 내고, 봇이 자기 주문을 군중으로 착각하거나 군중의 소수 수량 층을 통째로
// 지운다. Qty로 만들고 Float로만 꺼낸다 — 원시 int64 리터럴을 직접 대입하면
// 그 값은 이미 QtyDecimals 배율이 적용된 것으로 취급된다는 뜻이니 주의한다.
type Shares int64

// qtyFactor는 자연 단위 ↔ Shares 변환에 쓰는 배율이다.
var qtyFactor = math.Pow(10, QtyDecimals)

// Qty는 자연 단위 수량(주 수)을 Shares로 바꾼다. exclude 맵의 값도 반드시
// 이 함수를 거쳐야 한다 — 그래야 호가창 수량과 같은 단위로 뺄셈이 된다.
func Qty(shares float64) Shares {
	return Shares(math.Round(shares * qtyFactor))
}

// Float는 Shares를 자연 단위 수량으로 되돌린다. 출력(로그)용이다 — 비교나
// 뺄셈에 다시 쓰면 float 오차가 되살아난다. 항상 Shares(정수)로 비교하라.
func (s Shares) Float() float64 {
	return float64(s) / qtyFactor
}

// Book은 한 마켓의 오더북 상태다. 동시 접근에 안전하다.
type Book struct {
	precision int

	mu   sync.RWMutex
	bids map[int64]Shares // 가격틱 → 수량(Shares). exclude 도 Shares 단위여야 한다.
	asks map[int64]Shares

	// updateTimestampMs는 마지막으로 적용한 프레임의 서버 타임스탬프다.
	// 순서 역전(재접속 전후로 오래된 스냅샷이 새 것 위에 덮이는 것)을 막는 데 쓴다.
	updateTimestampMs int64

	// lastRecvMonoNs는 Stale 판정에 쓰는 단조시계 값이다. Frame.RecvMonoNs를
	// 그대로 저장한다 — 벽시계가 아니다(NTP 보정에 흔들리면 안 된다).
	lastRecvMonoNs int64

	// 정산 대기 물량. 이 태스크에서는 파싱만 한다 — 의미 해석과 최우선 호가
	// 계산에의 반영은 Task 10이 실측한 뒤 정한다.
	pendingBids []Level
	pendingAsks []Level
}

// MaxPrecision은 허용하는 decimalPrecision 상한이다. order 패키지의
// weiDecimals(=18, USDT 가 BSC 에서 쓰는 자릿수)와 같은 값이어야 한다 —
// 그쪽 WeiPerShare 가 10^(18−Precision) 로 스케일하므로 18 을 넘는 틱은
// 애초에 주문 금액으로 바꿀 수 없다. ws 가 order 를 import 하지 않으려고
// 상수를 복제한다(의존 방향은 order → ws 도 ws → order 도 아니다).
const MaxPrecision = 18

// checkPrecision은 precision 을 1..MaxPrecision 으로 강제한다.
//
// **패닉인 이유 — order.NewTick/Ceiling/WeiPerShare 와 같은 규약이다.**
// 같은 값(마켓의 decimalPrecision)을 한쪽은 패닉으로, 한쪽은 에러나 무시로
// 다루면 규약이 갈리는 것 자체가 다음 결함이 된다. precision 은 회차 시작
// 시점에 마켓 메타데이터에서 한 번 읽는 값이므로, 호가 루프 한복판이 아니라
// 여기서 시끄럽게 죽는 편이 낫다.
//
// **0 을 막는 것이 핵심이다.** decimalPrecision 필드가 없어지거나 이름이
// 바뀌면 encoding/json 이 조용히 제로값 0 을 넣는다. precision 0 이면
// Tick(0.47)=Tick(0.48)=Tick(0.49)=0 으로 세 층이 전부 틱 0 에 접히고,
// 책에 층이 가득 있으므로 BestBid 가 ok=true 와 함께 틱 0(가격 0.00)을
// 돌려준다 — BestBid 주석이 지키려던 "빈 쪽의 틱 0 이 주문 가격으로
// 흘러가는 것" 을 막는 ok 인터록이 통째로 무력화된다. 유령 호가가 아니라
// "진짜 있는 층" 으로 보이므로 어떤 하류 검사에도 걸리지 않는다.
// 이 저장소는 필드 이름을 잘못 짚는 실패를 이미 두 번 겪었다
// (POST /v1/auth/jwt, "address").
func checkPrecision(where string, precision int) {
	if precision < 1 || precision > MaxPrecision {
		panic(fmt.Sprintf("ws: %s 의 precision 은 1..%d 이어야 한다 (받은 값 %d)", where, MaxPrecision, precision))
	}
}

// NewBook은 빈 호가창을 만든다. precision은 이 마켓의 decimalPrecision이다
// (가격을 틱으로 바꿀 때 쓴다). 범위 밖이면 패닉한다 — checkPrecision 참고.
func NewBook(precision int) *Book {
	checkPrecision("NewBook", precision)
	return &Book{precision: precision}
}

// Tick은 가격(또는 수량)을 정수 틱으로 정규화한다: round(v × 10^precision).
//
// 절단(int64(v*factor))을 쓰면 안 된다. IEEE754 오차로 0.29 × 100이
// 28.999999999999996이 되는 경우가 있어서, 자르면 28이 나오고 반올림하면
// 29가 나온다 — 한 틱 어긋나면 최우선 매수호가를 잘못 읽고 영원히 체결되지
// 않거나(매수), 관통해서 테이커 수수료를 문다(매도).
//
// precision 은 1..MaxPrecision 만 허용한다(패닉). NewBook 이 이미 막지만
// 이 함수는 Book 없이도 부를 수 있는 공개 함수라 자기 가드를 따로 둔다.
func Tick(price float64, precision int) int64 {
	checkPrecision("Tick", precision)
	factor := math.Pow(10, float64(precision))
	return int64(math.Round(price * factor))
}

// FromTick은 정수 틱을 실수 가격으로 되돌린다. 출력(로그, 주문 문자열화)
// 시점에만 쓴다 — 내부 비교/연산은 항상 틱으로 한다.
//
// 여기에도 같은 가드를 둔다. 이쪽이 특히 조용한 경로다 — precision 0 이면
// FromTick(0,0)=0.00 이 아무 소리 없이 나와서 로그·알림에 "가격 0.00" 이
// 정상값처럼 찍힌다.
func FromTick(tick int64, precision int) float64 {
	checkPrecision("FromTick", precision)
	factor := math.Pow(10, float64(precision))
	return float64(tick) / factor
}

// orderbookData는 predictOrderbook 토픽의 Msg.Data 페이로드다.
//
// 매번 완전한 스냅샷으로 온다 — 델타가 아니다. 층을 지우는 델타 메시지가
// 따로 없으므로 Apply는 이 페이로드로 이전 상태를 통째로 교체한다.
type orderbookData struct {
	Asks               [][2]float64 `json:"asks"`
	Bids               [][2]float64 `json:"bids"`
	SettlementsPending *pendingData `json:"settlementsPending"`
	UpdateTimestampMs  int64        `json:"updateTimestampMs"`
}

type pendingData struct {
	Bids [][2]float64 `json:"bids"`
	Asks [][2]float64 `json:"asks"`
}

// Apply는 프레임의 오더북 스냅샷을 호가창에 반영한다.
//
// 매번 전체 스냅샷이므로 이전 bids/asks를 통째로 교체한다 — 병합하면 이번
// 스냅샷에 없는(즉 사라진) 호가가 영원히 남아 유령 유동성이 된다.
//
// updateTimestampMs가 현재 값 이하면 프레임을 버린다(에러 아님). 작은 경우는
// 순서 역전(재접속 전후로 오래된 스냅샷이 늦게 도착)이고, 같은 경우는 재구독
// 직후 같은 스냅샷이 다시 오는 정상 상황이다 — 이때 lastRecvMonoNs를
// 갱신하면 실제로는 멈춘 호가창이 계속 신선해 보인다.
//
// **applied 를 err 와 따로 돌려주는 이유.** "버림" 은 에러가 아니므로 err 는
// nil 이다. 그런데 반환값이 error 하나뿐이면 호출자는 "적용됨" 과 "버림" 을
// 구분할 방법이 없어서 err==nil 을 "호가창이 갱신됐다" 로 읽는다. 그러면
// Apply 가 자기 안에서 막은 실패 모드가 호출자에서 그대로 되살아난다 —
// 버려진 프레임마다 수신 카운터가 늘고, 마지막 갱신 시각이 앞당겨져 **멈춘
// 호가창이 신선하다고 보고되고**, 간격 분포(P5 Stale 문턱의 실측 근거)에
// 실제로는 없었던 짧은 간격이 섞여 문턱이 너무 빡빡해진다.
//
// 특히 updateTimestampMs 필드 이름이 바뀌면 모든 프레임이 0 으로 파싱되어
// `0 <= 0` 으로 **영구히 전량 버려지는데**, 그동안 호출자는 "프레임 정상
// 수신" 을 보고한다. 비어 있는 호가창을 두고. 그 상태를 호출자가 볼 수
// 있어야 한다.
//
// applied==false 면 호가창은 전혀 바뀌지 않았다 — 호출자는 카운터·마지막
// 갱신 시각·간격 표본 어느 것도 건드리면 안 된다.
func (b *Book) Apply(f Frame) (applied bool, err error) {
	var d orderbookData
	if err := json.Unmarshal(f.Msg.Data, &d); err != nil {
		return false, fmt.Errorf("오더북 데이터 파싱 실패: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if d.UpdateTimestampMs <= b.updateTimestampMs {
		return false, nil
	}

	// 수량은 Qty로 고정소수점(Shares)으로 바꿔 저장한다. 정수로 반올림하면
	// 0.071 같은 소수 수량 층이 통째로 사라진다 — 실재하는 군중 유동성을
	// 없는 것으로 착각하게 된다.
	bids := make(map[int64]Shares, len(d.Bids))
	for _, lvl := range d.Bids {
		t := Tick(lvl[0], b.precision)
		bids[t] += Qty(lvl[1])
	}
	asks := make(map[int64]Shares, len(d.Asks))
	for _, lvl := range d.Asks {
		t := Tick(lvl[0], b.precision)
		asks[t] += Qty(lvl[1])
	}

	b.bids = bids
	b.asks = asks
	b.updateTimestampMs = d.UpdateTimestampMs
	b.lastRecvMonoNs = f.RecvMonoNs

	if d.SettlementsPending != nil {
		b.pendingBids = toLevels(d.SettlementsPending.Bids)
		b.pendingAsks = toLevels(d.SettlementsPending.Asks)
	} else {
		b.pendingBids = nil
		b.pendingAsks = nil
	}

	return true, nil
}

func toLevels(raw [][2]float64) []Level {
	if len(raw) == 0 {
		return nil
	}
	out := make([]Level, len(raw))
	for i, lvl := range raw {
		out[i] = Level{Price: lvl[0], Size: lvl[1]}
	}
	return out
}

// PendingLevels는 정산 대기 물량을 그대로 돌려준다. 이 값은 최우선 호가
// 계산에 쓰이지 않는다 — Apply의 주석대로 파싱만 한다.
func (b *Book) PendingLevels() (bids, asks []Level) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.pendingBids, b.pendingAsks
}

// BestBid는 우리 주문을 뺀 최우선 매수호가를 틱으로 돌려준다.
//
// 우리 주문을 빼는 이유: 목표가를 우리 호가에 맞추면 자기 자신을 쫓는 순환이
// 생긴다. 군중을 따라가야 한다. exclude는 틱→우리 수량(Shares)이고, 그만큼
// 뺀 뒤에도 수량이 남은 층만 유효한 호가로 본다. exclude 값은 반드시 Qty로
// 만들어야 한다 — 자연 단위 float를 그대로 넣으면 컴파일 에러가 나고,
// 다른 배율의 int64를 넣으면(그런 값이 있다면) 제외가 조용히 틀린다.
//
// ok=false면 tick은 의미가 없다 — 0을 가격으로 쓰지 마라. 매수와 매도를
// 하나의 ok로 묶지 않는 이유가 이것이다. 한쪽만 비었을 때 묶인 ok는 true가
// 되고, 빈 쪽의 틱 0(가격 0.00)이 그대로 주문 가격으로 흘러간다.
func (b *Book) BestBid(exclude map[int64]Shares) (tick int64, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for t, qty := range b.bids {
		if qty-exclude[t] <= 0 {
			continue
		}
		if !ok || t > tick {
			tick, ok = t, true
		}
	}
	return tick, ok
}

// BestAsk는 BestBid와 같은 규칙으로 최우선 매도호가를 돌려준다.
func (b *Book) BestAsk(exclude map[int64]Shares) (tick int64, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for t, qty := range b.asks {
		if qty-exclude[t] <= 0 {
			continue
		}
		if !ok || t < tick {
			tick, ok = t, true
		}
	}
	return tick, ok
}

// LastRecvMonoNs는 마지막으로 적용한 프레임의 단조시계 수신 시각이다.
// 한 번도 프레임을 받지 않았으면 0이다.
func (b *Book) LastRecvMonoNs() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.lastRecvMonoNs
}

// Stale은 마지막 갱신 후 afterNs를 넘었는지 본다. 넘으면 호출자는 신규 주문을
// 멈추고 기존 주문을 취소해야 한다 — 오래된 호가를 보고 재주문하는 것이 최악이다.
//
// 단조시계 기준이다(Frame.RecvMonoNs). 벽시계로 재면 NTP 보정 한 번에 판정이
// 뒤집힌다. 프레임을 한 번도 못 받았으면 항상 stale이다.
func (b *Book) Stale(nowMonoNs, afterNs int64) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.lastRecvMonoNs == 0 {
		return true
	}
	return nowMonoNs-b.lastRecvMonoNs > afterNs
}
