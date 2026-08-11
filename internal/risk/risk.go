// Package risk 는 "얼마를 걸지"를 정한다. 순수 함수만 있다 — 네트워크도,
// 시계도, 전역 상태도 없다.
//
// 이 패키지가 지키는 것은 사용자가 정한 한 줄이다: 회차당 최대 명목은
// equity × 0.0455 **미만**. 그 한 줄이 지켜지지 않으면 나머지 코드가 아무리
// 옳아도 손실 한도가 없는 봇이 된다. 그래서 한도를 넘길 수 있는 모든 경로 —
// 노출 누락, 올림, 시가 평가, 망가진 입력 — 를 여기서 한꺼번에 막는다.
//
// # 망가진 입력에서의 방향
//
// 모든 함수는 **거래하지 않는 쪽**으로 실패한다. NaN·Inf·음수 같은 값이
// 들어오면 사이저는 0 을, 무장 판정은 false 를, 일손실 한도는 true(차단)를
// 돌려준다. 이유: 이 값들은 상류(온체인 잔고 조회, 원장 집계)가 망가졌을 때만
// 나오는데, 망가진 숫자로 계산한 주문 크기는 틀린 정도가 아니라 무한대가 될
// 수 있다(equity 가 +Inf 면 cap 도 +Inf 고 주식 수도 +Inf 다). 조용히 0 을
// 돌려주면 봇은 주문을 내지 않을 뿐이고, 그 사실은 무장 게이트와 로그에서
// 곧바로 보인다.
//
// 패닉하지 않는 이유: 이 함수들은 회차 중간 재호가 루프에서 수백 번 불린다.
// 살아 있는 주문을 들고 있는 상태에서 패닉하면 취소도 못 하고 죽는다.
package risk

import "math"

// CapFraction 은 회차당 최대 명목의 equity 대비 비율이다. 사용자가 정한
// 값이고 이 패키지 밖에서 바꾸지 않는다.
const CapFraction = 0.0455

// MinOrderUSD 는 거래소 최소 주문 금액이다. 이 밑으로는 주문 자체가 성립하지
// 않으므로 주식 수 0 을 돌려준다.
const MinOrderUSD = 1.0

// DefaultDailyFraction 은 일손실 한도 기본값이다(시작 equity 의 10%).
// 배선 코드에 0.10 리터럴을 흩뿌리지 않으려고 여기에 둔다.
const DefaultDailyFraction = 0.10

// maxExactShares 는 float64 가 정수를 하나도 빠뜨리지 않고 셀 수 있는
// 상한(2^53)이다. 이 위에서는 n-1 이 n 과 같아지므로 아래 Shares 의 보정이
// 성립하지 않고, 애초에 이만한 주식 수는 어떤 하류 정수 변환에서도 살아남지
// 못한다. 그런 값이 나왔다면 가격이 망가진 것이므로 주문하지 않는다.
const maxExactShares = 1 << 53

// finite 는 값이 NaN 도 ±Inf 도 아닌지 본다.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Equity 는 지금 굴리는 자본이다.
//
// PositionCost 가 **취득원가**인 것이 핵심이다. 미정산 포지션을 중간 호가로
// 평가하면 안 된다 — 정산되면 0 아니면 1 이 될 값이고, 그 사이 얇은 호가창의
// 중간값은 한 틱만 흔들려도 몇 %씩 움직인다. equity 가 흔들리면 cap 이
// 흔들리고 베팅 크기가 따라 흔들린다. 취득원가는 회차가 끝날 때까지 상수다.
type Equity struct {
	AvailableUSDT float64 // 가용 잔고
	PositionCost  float64 // 미정산 포지션 취득원가 합
}

// Total 은 두 항의 단순 합이다. 검사하지 않는다 — 로그·진단에서 원래 값을
// 그대로 보려고 쓰는 자리가 있기 때문이다.
//
// **진단·로그용이다. 사이징은 반드시 Cap/Remaining 을 지난다.** 이름이
// "검사되지 않은 날것의 합"이라는 것을 드러내지 않으므로 여기 적어 둔다 —
// 사이징 경로에서 이것을 직접 쓰면 이 패키지의 입력 검사를 통째로 우회한다.
func (e Equity) Total() float64 { return e.AvailableUSDT + e.PositionCost }

// Exposure 는 이 회차에 이미 걸린 명목이다.
//
// PendingCancel 이 별도 항인 이유: 취소 요청을 보냈지만 거래소가 확인해 주기
// 전까지 그 주문은 **아직 체결될 수 있다**. 이 봇은 회차당 수백 번 재호가하고,
// 재호가는 언제나 취소와 신규 주문 사이에 창을 만든다. 그 창에서 취소 미확인
// 주문을 0 으로 세면 신규 주문이 그만큼 더 나가고, 옛 주문이 체결되는 순간
// 노출이 두 배가 된다. 취소가 확인된 뒤에 이 항에서 빼는 것이 호출자의 책임이다.
type Exposure struct {
	FilledNotional float64 // 체결 누적 명목
	OpenNotional   float64 // 미체결 주문 명목
	PendingCancel  float64 // 취소 확인 전 주문 명목
}

// Total 은 세 항의 단순 합이다. Equity.Total 과 같은 이유로 검사하지 않는다.
//
// **진단·로그용이다. 사이징은 반드시 Cap/Remaining 을 지난다.**
func (x Exposure) Total() float64 { return x.FilledNotional + x.OpenNotional + x.PendingCancel }

// Cap 은 이 회차에 걸 수 있는 명목의 상한이다.
//
// 음수 항을 0 으로 만드는 대신 통째로 0 을 돌려주는 이유: AvailableUSDT 가
// 음수라는 것은 잔고 조회가 망가졌다는 뜻이고, 그 상태에서 PositionCost 만
// 믿고 베팅 크기를 정할 근거가 없다.
func Cap(e Equity) float64 {
	if !finite(e.AvailableUSDT) || e.AvailableUSDT < 0 {
		return 0
	}
	if !finite(e.PositionCost) || e.PositionCost < 0 {
		return 0
	}
	c := e.Total() * CapFraction
	// 두 항이 유한해도 합과 곱은 넘칠 수 있다(MaxFloat64 두 개면 +Inf).
	if !finite(c) || c < 0 {
		return 0
	}
	return c
}

// Remaining 은 지금 추가로 걸 수 있는 명목이다. Cap 에서 이미 걸린 것을 뺀다.
//
// 노출이 cap 과 정확히 같으면 0 이다. cap 은 넘을 수 없는 선이지 도달해도
// 되는 선이 아니다.
//
// 여기서 돌려주는 값은 "여기까지 쓸 수 있다"가 아니라 "여기 **미만**으로만
// 쓸 수 있다"이다. 그 강한 부등호는 Shares 가 지킨다 — 잔여를 정확히 다 쓰면
// 노출이 cap 과 같아지고 그것은 사용자 제약 위반이다.
//
// 노출 항이 하나라도 음수거나 유한하지 않으면 0 이다. 음수 노출은 산술적으로
// "여유가 더 있다"로 읽히는데, 그것이야말로 한도를 넘기는 경로다 — 부호를
// 잘못 넣은 집계 하나가 cap 을 무력화하면 안 된다.
func Remaining(e Equity, x Exposure) float64 {
	c := Cap(e)
	if c <= 0 {
		return 0
	}
	if !finite(x.FilledNotional) || x.FilledNotional < 0 {
		return 0
	}
	if !finite(x.OpenNotional) || x.OpenNotional < 0 {
		return 0
	}
	if !finite(x.PendingCancel) || x.PendingCancel < 0 {
		return 0
	}
	// 위 세 검사를 통과한 항의 합은 넘칠 때만 유한하지 않다(+Inf). 그때는
	// 아래 r 이 −Inf 가 되어 클램프에도 걸리므로 이 검사를 지워도 결과는
	// 같다 — 그래도 두는 이유는 "노출을 못 세면 주문하지 않는다"를 클램프의
	// 부수효과가 아니라 명시된 규칙으로 남기기 위해서다.
	t := x.Total()
	if !finite(t) {
		return 0
	}
	r := c - t
	if r <= 0 {
		return 0
	}
	return r
}

// CanArm 은 이 자본으로 유효한 주문을 낼 수 있는지 본다.
//
// 부등호가 강한 것이 요점이다. cap 이 정확히 $1 이면(equity =
// 1/0.0455 = 21.978…) $1 짜리 주문 하나가 노출을 cap 과 같게 만드는데,
// 한도는 "미만"이므로 그 주문은 낼 수 없다. 즉 그 자본으로는 낼 수 있는
// 주문이 하나도 없다. `>=` 로 쓰면 무장은 되는데 주문은 못 내는 상태가 되고,
// 그것은 봇이 조용히 아무것도 안 하는 가장 알아채기 어려운 고장이다.
func CanArm(e Equity) bool { return Cap(e) > MinOrderUSD }

// Shares 는 remaining 달러로 priceUSD 짜리 주식을 몇 주 살 수 있는지 센다.
//
// 보장하는 것은 `n × priceUSD < remaining` 이다 — **강한 부등호**다. 잔여를
// 정확히 다 쓰는 것도 허용하지 않는다. remaining 은 cap 에서 유도된 값이므로
// (Remaining = Cap − 기존 노출) 잔여를 다 쓰면 노출이 cap 과 정확히 같아지고,
// 사용자가 정한 제약은 `회차당 최대 명목 < equity × 0.0455` 이기 때문이다.
// 이 부등호 덕에 노출은 항상 cap 미만이다:
//
//	노출_새 = 노출 + n×price < 노출 + remaining = 노출 + (cap − 노출) = cap
//
// 그래서 잔여가 정확히 $1 이면 0 주다 — 명목이 $1 이상이면서 동시에 $1
// 미만일 수는 없다. 이것은 CanArm 이 cap 정확히 $1 에서 false 인 것과 같은
// 규칙의 두 얼굴이다.
//
// 반드시 내림한다. 올리면 명목이 remaining 을 넘는다.
//
// 내림한 뒤 한 번 더 곱해서 확인하는 이유: math.Floor(remaining/price) 는
// 나눗셈이 먼저 반올림되므로 결과가 참값보다 클 수 있다. 실제 틱 가격에서
// 나온다 — 예를 들어 price=0.072, remaining 이 26734×0.072 바로 아래 1 ulp 면
// 나눗셈이 정확히 26734.0 으로 반올림되고, 26734×0.072 는 remaining 보다
// 크다. 금액으로는 1e-13 달러지만 "한도를 넘지 않는다"에 예외를 두기
// 시작하면 그 예외의 크기를 아무도 다시 확인하지 않는다.
//
// 보정은 단발이다. 2^53 위 구간을 이미 거절했으므로 n-- 은 반드시 값을
// 바꾸고, 한 번 줄이면 반드시 한 눈금 아래 배수가 된다 — 루프로 만들면
// 그 구간에서 n-- 이 무효가 되어 영원히 돌 위험만 생긴다.
//
// 가격이 0 이하거나 유한하지 않으면 0 이다.
func Shares(remaining, priceUSD float64) float64 {
	if !finite(remaining) || remaining < MinOrderUSD {
		return 0
	}
	if !finite(priceUSD) || priceUSD <= 0 {
		return 0
	}

	n := math.Floor(remaining / priceUSD)
	if n >= maxExactShares {
		return 0
	}
	if n > 0 && n*priceUSD >= remaining {
		n--
	}
	if n <= 0 || n*priceUSD >= remaining {
		return 0
	}
	// 주식 수를 내림하고 나면 명목이 최소 주문 밑으로 떨어질 수 있다
	// (remaining 1.01, 가격 0.49 → 2 주 → 0.98). 그런 주문은 거래소가
	// 거절하므로 여기서 0 으로 만든다.
	if n*priceUSD < MinOrderUSD {
		return 0
	}
	return n
}

// DailyLimit 은 하루 손실 한도다. StartEquity 는 UTC 자정 시점 값이다.
type DailyLimit struct {
	StartEquity float64 // UTC 자정 시점
	Fraction    float64 // 기본 0.10
}

// Breached 는 오늘 실현손익이 한도에 닿았는지 본다. 정확히 −Fraction 은
// 차단이다(`<=`) — 한도는 넘지 않아야 할 선이 아니라 닿으면 멈추는 선이다.
//
// 설정이 망가졌으면 차단으로 읽는다. 이 함수만 실패 방향이 true 인데,
// 이 패키지의 규칙은 "망가지면 거래하지 않는다"이고 여기서 거래하지 않는
// 것은 true 이기 때문이다. 특히 제로값 DailyLimit{} 은 "한도 없음"이 아니라
// "설정 안 됨"이다 — 설정을 빠뜨린 배선이 무한 손실 허용으로 읽히면 안 된다.
// Fraction > 1 도 같은 이유로 오설정이다(자본 전부보다 더 잃어야 멈추는 한도).
func (d DailyLimit) Breached(realizedPnL float64) bool {
	if !finite(d.StartEquity) || d.StartEquity <= 0 {
		return true
	}
	if !finite(d.Fraction) || d.Fraction <= 0 || d.Fraction > 1 {
		return true
	}
	if !finite(realizedPnL) {
		return true
	}
	return realizedPnL <= -d.StartEquity*d.Fraction
}
