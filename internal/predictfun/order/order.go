// Package order 는 CTF Exchange 주문의 값 타입과 EIP-712 서명을 담당한다.
//
// 가격은 전부 정수 틱으로 다룬다. 틱 크기는 마켓의 decimalPrecision 에서 오고
// 실측상 2 와 3 이 공존하므로 전역 상수로 두지 않는다.
package order

import (
	"fmt"
	"math/big"
)

// Tick 은 정수 틱 가격이다. 0.5 = 50틱(정밀도 2) 또는 500틱(정밀도 3).
//
// 정밀도를 값과 함께 들고 다니는 이유: 틱 수만으로는 가격을 복원할 수 없고,
// 정밀도가 다른 두 마켓의 틱을 실수로 섞으면 10배 틀린 가격이 된다.
type Tick struct {
	V         int64
	Precision int
}

// NewTick 은 생성 시점에 precision 을 1..18 로 강제한다 — 그래야 생성자를
// 거친 모든 Tick 이 유효하고, Float·Add·앞으로 생길 메서드가 각자 가드를
// 다시 넣을 필요 없이 한 번에 안전해진다. precision 0 이하나 음수는
// pow10 이 1 을 돌려주는 사양 때문에 Float() 이 아무 경고 없이 자릿수가
// 통째로 틀린 값(0.49 대신 49)을 낸다 — Ceiling/WeiPerShare 의 가드를
// 사용처가 아니라 생성처로 옮긴 것이다.
func NewTick(v int64, precision int) Tick {
	if precision < 1 || precision > weiDecimals {
		panic(fmt.Sprintf("order: NewTick 의 precision 은 1..%d 이어야 한다 (받은 값 %d)", weiDecimals, precision))
	}
	return Tick{V: v, Precision: precision}
}

func (t Tick) Float() float64 { return float64(t.V) / float64(pow10(t.Precision)) }

// Add 는 같은 정밀도를 유지한 채 n 틱 움직인다. 관통 방지의 "best_ask − 1틱" 이 이것이다.
func (t Tick) Add(n int64) Tick { return Tick{V: t.V + n, Precision: t.Precision} }

// Ceiling 은 0.5 미만의 최대 틱이다. 정밀도 2 면 49, 3 이면 499.
// 리터럴 0.499 를 박지 않는 이유: 정밀도 2 인 마켓에서 그 가격은 표현 불가능하다.
//
// precision 은 1..18 만 허용한다. 0 이하면 상한이 음수 틱(예: precision 0 이면
// 10^0/2−1 = −1)이 되어 관통 방지가 "가격 −1"이라는 터무니없는 값으로 조용히
// 무너진다. precision 은 회차 시작 시점에 마켓 메타데이터에서 한 번 읽는
// 값이므로, 주문 루프 한복판이 아니라 여기서 시끄럽게 패닉하는 편이 낫다.
func Ceiling(precision int) Tick {
	if precision < 1 || precision > weiDecimals {
		panic(fmt.Sprintf("order: Ceiling 의 precision 은 1..%d 이어야 한다 (받은 값 %d)", weiDecimals, precision))
	}
	return Tick{V: pow10(precision)/2 - 1, Precision: precision}
}

// Full 은 가격 1.00 에 해당하는 틱 수다 — 정밀도 2 면 100, 3 이면 1000.
//
// 이 값이 필요한 이유는 **CTF 이진 시장에서 두 결과의 가격 합이 정확히 1** 이기
// 때문이다. predict.fun 의 오더북은 마켓당 한 벌이고 Up 기준으로 표현된다
// (2026-08-12 실측: `predictTrades` 의 outcomeIndex 1 과 2 의 가격 합이 정확히
// 1.00 이고, 책의 매수측이 outcomeIndex 1 이다). 그래서 Down 을 살 때의 호가는
// Up 호가를 이 값에서 빼서 얻는다:
//
//	Down 최우선 매수호가 = Full − Up 최우선 매도호가
//	Down 최우선 매도호가 = Full − Up 최우선 매수호가
//
// precision 가드는 Ceiling 과 같다 — 범위 밖이면 거울이 음수 틱을 만들어 관통
// 방지가 조용히 무너진다.
func Full(precision int) int64 {
	if precision < 1 || precision > weiDecimals {
		panic(fmt.Sprintf("order: Full 의 precision 은 1..%d 이어야 한다 (받은 값 %d)", weiDecimals, precision))
	}
	return pow10(precision)
}

// weiPerShareExponent 는 18 decimals wei 로 스케일하는 데 필요한 지수다.
// USDT 는 BSC 상에서 18 decimals 다 (이더리움의 6 이 아니다).
const weiDecimals = 18

// WeiPerShare 는 틱 가격을 18 decimals wei 로 바꾼다: V × 10^(18−Precision).
//
// big.Int 정수 연산으로만 계산한다 — float64(V)/10^Precision*1e18 을 거치면
// 부동소수점 반올림으로 저가(예: 0.07)에서 몇 wei 어긋난다. makerAmount 가
// 그 몇 wei 만큼 바뀌면 EIP-712 다이제스트가 통째로 바뀐다.
//
// Precision 은 1..18 만 허용한다. 18 을 넘으면 지수(18−Precision)가 음수가
// 되고, big.Int.Exp 는 음수 지수 + nil modulus 에서 0.1 이 아니라 1 을
// 돌려주는 사양이라 V 그대로가 나온다 — 에러 없이 조용히 틀린 금액이 된다.
// 0 이하도 Ceiling 과 같은 이유로 막는다.
//
// V 도 0 < V < 10^Precision 으로 막는다. 여기가 어떤 경로로 만들어진 틱이든
// 주문 금액이 되기 전에 반드시 지나는 유일한 길목이다 — NewTick 은 생성
// 시점에 Precision 만 강제하고 V 는 보지 않고, Add 는 중간 계산이라 일시적
// 범위 이탈을 허용해야 한다(관통 방지 계산 도중 실측: Add(-100) 이면
// V 가 음수, Add(1000) 이면 10^Precision 을 넘는다). V 가 범위를 벗어난 채
// 여기까지 오면 makerAmount 가 음수 wei 이거나 가격이 1 을 넘는 주문이
// 만들어지는데, EIP-712 다이제스트는 그런 값도 멀쩡히 서명한다 — 서명이
// 거부되는 것보다 나쁜, 의도와 다른 주문이 체결되는 부류다. 목표 틱을
// 이 범위 안에서 만드는 책임은 P5 의 호가 로직에 있고, 이 가드는 그걸
// 놓쳤을 때 조용히 넘어가지 않게 하는 최후 방어선이다.
func (t Tick) WeiPerShare() *big.Int {
	if t.Precision < 1 || t.Precision > weiDecimals {
		panic(fmt.Sprintf("order: WeiPerShare 의 Precision 은 1..%d 이어야 한다 (받은 값 %d)", weiDecimals, t.Precision))
	}
	if maxV := pow10(t.Precision); t.V <= 0 || t.V >= maxV {
		panic(fmt.Sprintf("order: WeiPerShare 의 V 는 0 < V < %d(=10^%d) 이어야 한다 (받은 값 V=%d, Precision=%d, 가격≈%v)",
			maxV, t.Precision, t.V, t.Precision, t.Float()))
	}
	exp := weiDecimals - t.Precision
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exp)), nil)
	return new(big.Int).Mul(big.NewInt(t.V), scale)
}

func pow10(n int) int64 {
	v := int64(1)
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}

// Order 는 CTF Exchange 의 EIP-712 Order 구조체다. 필드 순서는 문서화 목적일
// 뿐 해시에 영향을 주지 않는다 — 해시는 sign.go 의 타입 정의 순서를 따른다.
type Order struct {
	Salt          *big.Int
	Maker         string
	Signer        string
	Taker         string
	TokenID       *big.Int
	MakerAmount   *big.Int
	TakerAmount   *big.Int
	Expiration    *big.Int
	Nonce         *big.Int
	FeeRateBps    *big.Int
	Side          uint8
	SignatureType uint8
}

// Domain 은 Order 서명에 쓰는 EIP-712 도메인이다.
type Domain struct {
	Name              string
	Version           string
	ChainID           int64
	VerifyingContract string
}

const (
	// minQuantityWei 는 SDK 가 거부하는 최소 수량이다. 문서 주석은 1e18 이라
	// 적혀 있으나 실제 코드는 1e16 이다 (dist/OrderBuilder.js).
	priceSignificantDigits = 3
	qtySignificantDigits   = 5
)

var minQuantityWei = new(big.Int).SetUint64(10_000_000_000_000_000) // 1e16

// RetainSignificantDigits 는 n 을 10진 자릿수 digits 자리로 절단한다(반올림이
// 아니다). 자릿수는 유효숫자가 아니라 10진 표현 전체 길이로 센다 — SDK
// dist/internal/Utils.js:38-53 의 magnitude = absNum.toString().length 규칙.
func RetainSignificantDigits(n *big.Int, digits int) *big.Int {
	if n.Sign() == 0 {
		return new(big.Int)
	}
	magnitude := len(n.Text(10))
	if n.Sign() < 0 {
		magnitude-- // 부호는 자릿수가 아니다. Order 금액은 항상 양수이지만 방어적으로 둔다.
	}
	excess := magnitude - digits
	if excess <= 0 {
		return new(big.Int).Set(n)
	}
	div := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(excess)), nil)
	truncated := new(big.Int).Div(n, div)
	return truncated.Mul(truncated, div)
}

// AmountsForBuy 는 SDK getLimitOrderAmounts(BUY) 를 그대로 재현한다.
//
//	price(3자리 절단) × qty(5자리 절단) / 1e18 = makerAmount (지불할 USDT wei)
//	qty(5자리 절단)                             = takerAmount (받을 주식 wei)
//
// takerAmount 는 절단된 수량이다 — 넘긴 quantityWei 원본이 아니다
// (SDK dist/OrderBuilder.js:654-679). 독자적인 반올림은 넣지 않는다.
func AmountsForBuy(priceWei, quantityWei *big.Int) (makerAmount, takerAmount *big.Int, err error) {
	if quantityWei.Cmp(minQuantityWei) < 0 {
		return nil, nil, fmt.Errorf("수량이 최소치 미만이다: %s < %s", quantityWei, minQuantityWei)
	}
	price := RetainSignificantDigits(priceWei, priceSignificantDigits)
	qty := RetainSignificantDigits(quantityWei, qtySignificantDigits)

	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(weiDecimals), nil)
	maker := new(big.Int).Mul(price, qty)
	maker.Div(maker, e18)

	return maker, qty, nil
}
