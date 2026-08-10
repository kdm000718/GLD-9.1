package order

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
)

// 상한을 0.499 로 박으면 정밀도 2 인 마켓에서 표현 불가능한 가격이 된다.
// 틱에서 유도해야 한다.
func TestCeilingDependsOnPrecision(t *testing.T) {
	if got := Ceiling(2); got.V != 49 || got.Precision != 2 {
		t.Errorf("정밀도 2 의 상한 %+v, 기대 {V:49 Precision:2} (0.49)", got)
	}
	if got := Ceiling(3); got.V != 499 || got.Precision != 3 {
		t.Errorf("정밀도 3 의 상한 %+v, 기대 {V:499 Precision:3} (0.499)", got)
	}
}

func TestTickFloat(t *testing.T) {
	if got := NewTick(49, 2).Float(); got != 0.49 {
		t.Errorf("49틱(정밀도 2) = %v, 기대 0.49", got)
	}
	if got := NewTick(499, 3).Float(); got != 0.499 {
		t.Errorf("499틱(정밀도 3) = %v, 기대 0.499", got)
	}
}

// WeiPerShare 는 정수 연산이어야 한다. float 를 거치면 저가에서 어긋나고,
// 그 차이가 makerAmount 를 바꿔 EIP-712 다이제스트를 바꾼다 — G3 가 SDK 와
// 비트 일치를 요구하므로 그대로 실패한다.
//
// 0.07 케이스가 이 테스트의 핵심이다: float64(7)/100*1e18 은
// 70000000000000008 이 되어 정확값보다 8 wei 크다(팀리드 실측).
func TestWeiPerShareIsExactIntegerMath(t *testing.T) {
	cases := []struct {
		tick      int64
		precision int
		want      string
	}{
		{49, 2, "490000000000000000"},  // 0.49
		{7, 2, "70000000000000000"},    // 0.07 — float 경로면 ...008 이 된다
		{1, 2, "10000000000000000"},    // 0.01 — 실측 호가창의 최저 층
		{4, 2, "40000000000000000"},    // 0.04
		{499, 3, "499000000000000000"}, // 0.499
		{333, 3, "333000000000000000"}, // 0.333
	}
	for _, c := range cases {
		got := NewTick(c.tick, c.precision).WeiPerShare()
		want := mustBig(t, c.want)
		if got.Cmp(want) != 0 {
			t.Errorf("NewTick(%d, %d).WeiPerShare() = %s, 기대 %s — float 를 거치고 있다",
				c.tick, c.precision, got, want)
		}
	}
}

// 0.5 미만 최대 틱도 정수로 유도한다.
func TestCeilingIsIntegerDerived(t *testing.T) {
	if got := Ceiling(2); got.V != 49 {
		t.Errorf("Ceiling(2).V = %d, 기대 49", got.V)
	}
	if got := Ceiling(3); got.V != 499 {
		t.Errorf("Ceiling(3).V = %d, 기대 499", got.V)
	}
	// 상한은 0.5 미만이어야 한다 — 같거나 넘으면 관통 방지가 무너진다.
	for _, p := range []int{2, 3} {
		if f := Ceiling(p).Float(); f >= 0.5 {
			t.Errorf("Ceiling(%d) = %v — 0.5 미만이어야 한다", p, f)
		}
	}
}

// precision 0/음수/19 이상에서 Ceiling 은 음수 틱이나 표현 불가능한 값을
// 조용히 내는 대신 패닉해야 한다 — 마켓 메타데이터가 새면 관통 방지가
// "가격 −1" 로 무너지는 것보다 기동 시점에 시끄럽게 죽는 편이 낫다.
func TestCeilingPanicsOutsideValidRange(t *testing.T) {
	for _, p := range []int{0, -1, 19, 100} {
		func(precision int) {
			defer func() {
				if recover() == nil {
					t.Errorf("Ceiling(%d) 이 패닉하지 않았다", precision)
				}
			}()
			Ceiling(precision)
		}(p)
	}
}

// 허용 범위(1..18) 경계는 패닉하지 않고 정상값을 낸다 — 가드가 유효한
// 입력까지 막지 않는지 확인한다.
func TestCeilingAcceptsBoundaryPrecision(t *testing.T) {
	if got := Ceiling(1); got.V != 4 { // 10^1/2 − 1 = 4 (0.4)
		t.Errorf("Ceiling(1).V = %d, 기대 4", got.V)
	}
	if got := Ceiling(18); got.V != 499999999999999999 { // 10^18/2 − 1
		t.Errorf("Ceiling(18).V = %d, 기대 499999999999999999", got.V)
	}
}

// Precision 이 18 을 넘으면 big.Int.Exp 의 음수 지수 규약(nil modulus 에서
// 10^-n 대신 1 을 돌려준다) 때문에 WeiPerShare 가 V 그대로를 돌려줘 조용히
// 틀린 금액이 된다 — 에러 반환 대신 패닉으로 막는다.
//
// NewTick 이 아니라 Tick{} 구조체 리터럴로 만든다 — NewTick 을 거치면 이제
// 그쪽 가드가 먼저 패닉해서 여기서 확인하려는 WeiPerShare 자체의 가드(구조체
// 리터럴까지 잡는 이중 방어)를 못 보게 된다.
func TestWeiPerSharePanicsOutsideValidRange(t *testing.T) {
	for _, p := range []int{0, -1, 19, 30} {
		func(precision int) {
			defer func() {
				if recover() == nil {
					t.Errorf("WeiPerShare precision=%d 가 패닉하지 않았다", precision)
				}
			}()
			Tick{V: 5, Precision: precision}.WeiPerShare()
		}(p)
	}
}

func TestWeiPerShareAcceptsBoundaryPrecision(t *testing.T) {
	if got := NewTick(5, 1).WeiPerShare(); got.Cmp(mustBigNoT("500000000000000000")) != 0 {
		t.Errorf("NewTick(5,1).WeiPerShare() = %s, 기대 500000000000000000", got)
	}
	if got := NewTick(5, 18).WeiPerShare(); got.Cmp(mustBigNoT("5")) != 0 {
		t.Errorf("NewTick(5,18).WeiPerShare() = %s, 기대 5", got)
	}
}

// WeiPerShare 는 틱이 돈이 되는 유일한 길목이다 — NewTick 이든 Add 든 구조체
// 리터럴이든, 어떤 경로로 만들어진 틱이든 주문 금액이 되려면 여기를 지난다.
// V 가 0 < V < 10^Precision 밖이면(가격이 0 이하이거나 1 이상) 조용히 넘어가는
// 대신 패닉해야 한다 — Add(-100)/Add(1000) 처럼 관통 방지 계산 도중 실측된
// 범위 이탈이 그대로 주문 금액이 되는 것을 막는 최후 방어선이다.
func TestWeiPerSharePanicsOnInvalidV(t *testing.T) {
	cases := []struct {
		name string
		tick Tick
	}{
		{"V=0(가격 0)", Tick{V: 0, Precision: 2}},
		{"V=-51(Add(-100) 실측 재현, 음수 가격)", Tick{V: -51, Precision: 2}},
		{"V=100(precision 2 에서 정확히 1.00, 상계 배제)", Tick{V: 100, Precision: 2}},
		{"V=1049(Add(1000) 실측 재현, 1 초과)", Tick{V: 1049, Precision: 2}},
		{"V=1000(precision 3 에서 정확히 1.00, 상계 배제)", Tick{V: 1000, Precision: 3}},
	}
	for _, c := range cases {
		func(name string, tk Tick) {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: WeiPerShare 가 패닉하지 않았다", name)
				}
			}()
			tk.WeiPerShare()
		}(c.name, c.tick)
	}
}

// 경계 바로 안쪽(0.99, 0.999)은 유효하다 — 위 상계 배제 케이스와 짝을 이뤄
// 경계선이 정확히 그 자리인지(느슨하게 V<=maxV 가 되어 있지 않은지) 고정한다.
func TestWeiPerShareAcceptsValidVBoundary(t *testing.T) {
	if got := NewTick(99, 2).WeiPerShare(); got.Cmp(mustBigNoT("990000000000000000")) != 0 {
		t.Errorf("NewTick(99,2).WeiPerShare() = %s, 기대 990000000000000000 (0.99)", got)
	}
	if got := NewTick(999, 3).WeiPerShare(); got.Cmp(mustBigNoT("999000000000000000")) != 0 {
		t.Errorf("NewTick(999,3).WeiPerShare() = %s, 기대 999000000000000000 (0.999)", got)
	}
}

// NewTick 에서 precision 을 강제한다 — Ceiling/WeiPerShare 각각에 흩어져
// 있던 가드를 생성 시점 하나로 모았다. 이러면 생성자를 거친 모든 Tick 이
// 유효해지고, Float·Add·앞으로 생길 메서드가 각자 가드를 다시 넣을 필요가
// 없다. 0 이하는 pow10 이 1 을 돌려주는 사양 때문에 Float() 이 자릿수가
// 통째로 틀린 값(0.49 대신 49)을 조용히 낸다 — 그 지점을 생성 시점으로
// 옮겨 막는다.
func TestNewTickPanicsOutsideValidRange(t *testing.T) {
	for _, p := range []int{0, -3, 19} {
		func(precision int) {
			defer func() {
				if recover() == nil {
					t.Errorf("NewTick(49, %d) 가 패닉하지 않았다", precision)
				}
			}()
			NewTick(49, precision)
		}(p)
	}
}

// 허용 범위(1..18) 경계는 패닉하지 않아야 한다. Float() 이 이제 안전하다는
// 것도 함께 고정한다 — NewTick 을 거친 틱은 항상 올바른 자릿수를 낸다.
// (Tick{V:49, Precision:0} 처럼 구조체 리터럴로 만든 것까지 고치지는
// 못한다 — 그건 concerns 로 리포트에 남긴다.)
func TestNewTickAcceptsBoundaryPrecision(t *testing.T) {
	if got := NewTick(1, 1); got.V != 1 || got.Precision != 1 {
		t.Errorf("NewTick(1,1) = %+v, 기대 {V:1 Precision:1}", got)
	}
	if got := NewTick(1, 18); got.V != 1 || got.Precision != 18 {
		t.Errorf("NewTick(1,18) = %+v, 기대 {V:1 Precision:18}", got)
	}
	if got := NewTick(49, 2).Float(); got != 0.49 {
		t.Errorf("NewTick(49,2).Float() = %v, 기대 0.49", got)
	}
	if got := NewTick(499, 3).Float(); got != 0.499 {
		t.Errorf("NewTick(499,3).Float() = %v, 기대 0.499", got)
	}
}

// SDK getLimitOrderAmounts(BUY) 를 그대로 따른다. 팀리드가 SDK 소스를 직접 읽어
// 확인했다 (`dist/OrderBuilder.js:654-679`, `dist/internal/Utils.js:38-53`):
//
//	if (quantityWei < 1e16) throw
//	price = retainSignificantDigits(pricePerShareWei, 3)
//	qty   = retainSignificantDigits(quantityWei, 5)
//	makerAmount = (price * qty) / 1e18
//	takerAmount = qty          ← **절단된** qty 다. 원본이 아니다.
//
// **기대값을 구현과 같은 식으로 계산하지 마라.** 초안의 이 테스트가 그랬고,
// 게다가 0.49 / 2주는 유효숫자가 각각 2·1자리라 절단이 아예 발동하지 않는
// 입력이었다 — 절단 코드를 통째로 빠뜨려도 통과한다. 아래 기대값은 팀리드가
// SDK 규칙을 손으로 돌려 얻은 상수다.
func TestAmountsForBuyTruncatesQuantityToFiveDigits(t *testing.T) {
	price := mustBig(t, "490000000000000000") // 0.49 — 절단해도 안 변한다
	qty := mustBig(t, "1234567890123456789")  // 유효숫자 19자리 — 절단이 발동한다

	maker, taker, err := AmountsForBuy(price, qty)
	if err != nil {
		t.Fatal(err)
	}
	// takerAmount 는 절단된 수량이다. 원본 1234567890123456789 이 아니다.
	wantTaker := mustBig(t, "1234500000000000000")
	if taker.Cmp(wantTaker) != 0 {
		t.Errorf("takerAmount %s, 기대 %s — 절단된 수량이어야 한다(원본 %s 가 아니다)",
			taker, wantTaker, qty)
	}
	// makerAmount 는 절단된 가격 × 절단된 수량 / 1e18 이다.
	wantMaker := mustBig(t, "604905000000000000")
	if maker.Cmp(wantMaker) != 0 {
		t.Errorf("makerAmount %s, 기대 %s", maker, wantMaker)
	}
}

func TestAmountsForBuyTruncatesPriceToThreeDigits(t *testing.T) {
	price := mustBig(t, "456700000000000000") // 0.4567 — 절단되어 0.456 이 된다
	qty := mustBig(t, "2000000000000000000")  // 2주 — 절단 안 됨

	maker, taker, err := AmountsForBuy(price, qty)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustBig(t, "2000000000000000000"); taker.Cmp(want) != 0 {
		t.Errorf("takerAmount %s, 기대 %s", taker, want)
	}
	// 0.456 × 2 = 0.912. 절단을 안 하면 0.9134 가 나온다.
	if want := mustBig(t, "912000000000000000"); maker.Cmp(want) != 0 {
		t.Errorf("makerAmount %s, 기대 %s — 가격이 3자리로 절단되지 않았다", maker, want)
	}
}

// 절단이 발동하지 않는 평범한 경우도 남긴다 — 절단 코드가 멀쩡한 입력을
// 망가뜨리지 않는지 본다.
func TestAmountsForBuyLeavesCleanInputsAlone(t *testing.T) {
	price := mustBig(t, "490000000000000000") // 0.49
	qty := mustBig(t, "2000000000000000000")  // 2주

	maker, taker, err := AmountsForBuy(price, qty)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustBig(t, "2000000000000000000"); taker.Cmp(want) != 0 {
		t.Errorf("takerAmount %s, 기대 %s", taker, want)
	}
	if want := mustBig(t, "980000000000000000"); maker.Cmp(want) != 0 {
		t.Errorf("makerAmount %s, 기대 %s (0.98 USDT)", maker, want)
	}
}

// 절단은 내림이다 — takerAmount(주식 수)는 절단으로 인해 원본 quantityWei 를
// 절대 넘지 않는다. 넘긴다면 의도한 예산보다 더 많은 주식을 사겠다는 주문이
// 나가 사이징 한도를 넘길 수 있다(반올림이면 넘을 수 있다).
func TestAmountsForBuyRoundsDownShares(t *testing.T) {
	price := mustBig(t, "490000000000000000") // 0.49 — 절단해도 안 변한다
	cases := []string{
		"1234567890123456789", // 유효숫자 19자리 — 절단이 크게 발동
		"999999999999999999",  // 18개의 9 — 올림이면 자릿수가 하나 늘어난다
		"10000000000000001",   // 최소치 바로 위, 절단이 발동하는 경계
		"20000000000000000",   // 5자리로 정확히 떨어짐 — 절단이 no-op 이어야 함
	}
	for _, qs := range cases {
		qty := mustBig(t, qs)
		maker, taker, err := AmountsForBuy(price, qty)
		if err != nil {
			t.Fatalf("qty=%s: %v", qs, err)
		}
		if taker.Cmp(qty) > 0 {
			t.Errorf("qty=%s: takerAmount %s 가 원본을 초과했다 — 내림이 아니라 올림됐다", qs, taker)
		}
		// makerAmount 도 "절단된 가격 × 절단된 수량"을 넘지 않는다 — 지불액이
		// 한도를 넘는 방향으로 절단이 작동하지 않는지를 함께 본다.
		naive := new(big.Int).Mul(price, qty)
		naive.Div(naive, mustBig(t, "1000000000000000000"))
		if maker.Cmp(naive) > 0 {
			t.Errorf("qty=%s: makerAmount %s 가 절단 없는 계산값 %s 를 초과했다", qs, maker, naive)
		}
	}
}

func mustBig(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("%q 를 big.Int 로 못 읽었다", s)
	}
	return v
}

func TestAmountsForBuyRejectsBelowMinQuantity(t *testing.T) {
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	price := new(big.Int).Div(e18, big.NewInt(2))
	// 1e16 미만은 거부. 문서 주석은 1e18 이라 적혀 있으나 코드는 1e16 이다.
	if _, _, err := AmountsForBuy(price, big.NewInt(9_999_999_999_999_999)); err == nil {
		t.Fatal("1e16 미만 수량을 받아들였다")
	}
	if _, _, err := AmountsForBuy(price, big.NewInt(10_000_000_000_000_000)); err != nil {
		t.Fatalf("정확히 1e16 을 거부했다: %v", err)
	}
}

// 절단은 반올림이 아니다. 초과 자릿수만큼 나눴다가 다시 곱한다.
func TestRetainSignificantDigits(t *testing.T) {
	cases := []struct {
		in     string
		digits int
		want   string
	}{
		{"123456789", 3, "123000000"},
		{"999999999", 3, "999000000"}, // 반올림이면 1000000000 이 된다
		{"12", 5, "12"},               // 자릿수가 모자라면 그대로
		{"0", 3, "0"},
		{"490000000000000000", 3, "490000000000000000"},
	}
	for _, c := range cases {
		in, _ := new(big.Int).SetString(c.in, 10)
		want, _ := new(big.Int).SetString(c.want, 10)
		if got := RetainSignificantDigits(in, c.digits); got.Cmp(want) != 0 {
			t.Errorf("RetainSignificantDigits(%s, %d) = %s, 기대 %s", c.in, c.digits, got, c.want)
		}
	}
}

func TestHashIsDeterministic(t *testing.T) {
	o, d := fixtureOrder(), fixtureDomain()
	a, err := Hash(o, d)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Hash(o, d)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("같은 입력에서 해시가 다르다")
	}
	if len(a) != 32 {
		t.Fatalf("해시 %d바이트, 기대 32", len(a))
	}
}

func TestHashChangesWithVerifyingContract(t *testing.T) {
	o := fixtureOrder()
	d1 := fixtureDomain()
	d2 := fixtureDomain()
	d2.VerifyingContract = "0x365fb81bd4A24D6303cd2F19c349dE6894D8d58A" // NEG_RISK
	a, _ := Hash(o, d1)
	b, _ := Hash(o, d2)
	if string(a) == string(b) {
		t.Fatal("verifyingContract 가 달라도 해시가 같다 — 계약 구분이 서명에 안 들어갔다")
	}
}

// Step 5b 골든의 표준 Order 입력은 taker=0x0, expiration=0, nonce=0, side=0,
// signatureType=0 으로 다섯 필드가 전부 zero-value 인 한 벌뿐이다 — 그 필드가
// 인코딩에서 통째로 빠져도 zero-value 라 같은 다이제스트가 나올 수 있다.
// 기준 주문에서 12개 필드를 하나씩만 바꿔, 13개(기준 + 12) 다이제스트가
// 전부 서로 다른지 본다. 하나라도 기준과 같으면 그 필드가 해시에 반영되지
// 않는다는 뜻이다.
func TestOrderDigestChangesWithEachField(t *testing.T) {
	d := fixtureDomain()
	base := fixtureOrder()
	baseHash, err := Hash(base, d)
	if err != nil {
		t.Fatal(err)
	}

	// 자리표시자 — 실계정 주소를 쓰지 않는다.
	const altAddr = "0x2222222222222222222222222222222222222222"

	variants := []struct {
		name string
		o    Order
	}{
		{"salt", withOrder(base, func(o *Order) { o.Salt = big.NewInt(99999) })},
		{"maker", withOrder(base, func(o *Order) { o.Maker = altAddr })},
		{"signer", withOrder(base, func(o *Order) { o.Signer = altAddr })},
		{"taker", withOrder(base, func(o *Order) { o.Taker = altAddr })},
		{"tokenId", withOrder(base, func(o *Order) { o.TokenID = mustBigNoT("77777777777777777777777777777777777777") })},
		{"makerAmount", withOrder(base, func(o *Order) { o.MakerAmount = mustBigNoT("970000000000000000") })},
		{"takerAmount", withOrder(base, func(o *Order) { o.TakerAmount = mustBigNoT("3000000000000000000") })},
		{"expiration", withOrder(base, func(o *Order) { o.Expiration = big.NewInt(1_700_000_000) })},
		{"nonce", withOrder(base, func(o *Order) { o.Nonce = big.NewInt(1) })},
		{"feeRateBps", withOrder(base, func(o *Order) { o.FeeRateBps = big.NewInt(21) })},
		{"side", withOrder(base, func(o *Order) { o.Side = 1 })},
		{"signatureType", withOrder(base, func(o *Order) { o.SignatureType = 1 })},
	}

	seen := map[string]string{string(baseHash): "기준"}
	for _, v := range variants {
		h, err := Hash(v.o, d)
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if string(h) == string(baseHash) {
			t.Errorf("%s 필드를 바꿔도 다이제스트가 기준과 같다 — 이 필드가 해시 인코딩에서 빠졌다", v.name)
			continue
		}
		if other, dup := seen[string(h)]; dup {
			t.Errorf("%s 필드 변경 다이제스트가 %s 변경의 다이제스트와 겹친다", v.name, other)
		}
		seen[string(h)] = v.name
	}
}

// withOrder 는 base 를 복사한 뒤 mutate 로 한 필드만 바꿔 돌려준다. base 자체는
// 건드리지 않는다 — Order 필드는 포인터라 원본을 in-place 로 고치면 다른
// 변형과 서로 오염될 수 있다.
func withOrder(base Order, mutate func(*Order)) Order {
	o := base
	mutate(&o)
	return o
}

// fixtureOrder 는 해시 결정성·차이 테스트에 쓰는 자리표시자 주문이다.
// 실계정 주소를 쓰지 않는다 — 저장소 규칙.
func fixtureOrder() Order {
	return Order{
		Salt:          big.NewInt(12345),
		Maker:         "0x1111111111111111111111111111111111111111",
		Signer:        "0x1111111111111111111111111111111111111111",
		Taker:         "0x0000000000000000000000000000000000000000",
		TokenID:       mustBigNoT("88888888888888888888888888888888888888"),
		MakerAmount:   mustBigNoT("980000000000000000"),
		TakerAmount:   mustBigNoT("2000000000000000000"),
		Expiration:    big.NewInt(0),
		Nonce:         big.NewInt(0),
		FeeRateBps:    big.NewInt(20),
		Side:          0,
		SignatureType: 0,
	}
}

func fixtureDomain() Domain {
	return Domain{
		Name:              "predict.fun CTF Exchange",
		Version:           "1",
		ChainID:           56,
		VerifyingContract: "0x8BC070BEdAB741406F4B1Eb65A72bee27894B689", // CTF_EXCHANGE
	}
}

// mustBigNoT 는 t 없이 쓰는 mustBig 이다 — 픽스처는 테스트 함수 밖에서도 쓰인다.
func mustBigNoT(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("fixture: " + s + " 를 big.Int 로 못 읽었다")
	}
	return v
}

// --- Step 5b: 팀리드가 SDK(ethers)와 대조해 둔 기준값. Task 9 의 G3 를 기다리지
// 않고 여기서 즉시 비트 일치를 확인한다. tools/sdk_order_digest_check.mjs 로
// 재확인 가능하다.

func TestOrderDigestMatchesSDKGolden(t *testing.T) {
	o := fixtureOrder()
	cases := []struct {
		label             string
		verifyingContract string
		chainID           int64
		want              string
	}{
		{"CTF/56", "0x8BC070BEdAB741406F4B1Eb65A72bee27894B689", 56,
			"aff4afe0cee0b54a087cf7d0146adb548a3e0866e665ceed658e01faac0f5916"},
		{"NEG_RISK/56", "0x365fb81bd4A24D6303cd2F19c349dE6894D8d58A", 56,
			"b9bb92930fb3be4df91a5ab03f5b585175adbabd771040299332848f031f8857"},
		{"CTF/97", "0x2A6413639BD3d73a20ed8C95F634Ce198ABbd2d7", 97,
			"b68f4cd30d28c08206a3351743bad88eced96b0387fdfeb53226a521b7e7ff60"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		d := Domain{
			Name: "predict.fun CTF Exchange", Version: "1",
			ChainID: c.chainID, VerifyingContract: c.verifyingContract,
		}
		got, err := Hash(o, d)
		if err != nil {
			t.Fatalf("%s: %v", c.label, err)
		}
		want := common.HexToHash(c.want)
		if !bytes.Equal(got, want.Bytes()) {
			t.Errorf("%s: Order 다이제스트 = 0x%x, 기대 0x%s — SDK(ethers) 기준값과 어긋난다",
				c.label, got, c.want)
		}
		seen[string(got)] = true
	}
	if len(seen) != len(cases) {
		t.Fatal("verifyingContract/chainId 가 달라도 Order 다이제스트가 겹친다 — 계약·체인 구분이 서명에 안 들어갔다")
	}
}

func TestKernelDigestMatchesSDKGolden(t *testing.T) {
	digest := make([]byte, 32)
	for i := range digest {
		digest[i] = byte(i)
	}
	cases := []struct {
		chainID int64
		acct    string
		want    string
	}{
		{56, "0x1111111111111111111111111111111111111111",
			"af803f2abd4c257d6146ea9dfab747be742c77a531cb40a535b09aaf83d3b4eb"},
		{56, "0x2222222222222222222222222222222222222222",
			"b6c89ab506db96f8b16813a1a302f314c012b47f8271279114eba9d7bc86d3c6"},
		{97, "0x1111111111111111111111111111111111111111",
			"ca19f3573eaeb97a07416265492f193b30de3246fef541ecf56d025f147a4c94"},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		got, err := KernelDigest(digest, c.chainID, common.HexToAddress(c.acct))
		if err != nil {
			t.Fatalf("chain=%d acct=%s: %v", c.chainID, c.acct, err)
		}
		want := common.HexToHash(c.want)
		if !bytes.Equal(got, want.Bytes()) {
			t.Errorf("chain=%d acct=%s: Kernel 다이제스트 = 0x%x, 기대 0x%s — SDK(ethers) 기준값과 어긋난다",
				c.chainID, c.acct, got, c.want)
		}
		seen[string(got)] = true
	}
	if len(seen) != len(cases) {
		t.Fatal("predictAccount/chainId 가 달라도 Kernel 다이제스트가 겹친다 — 계정·체인 구분이 서명에 안 들어갔다")
	}
}

// --- Step 6: 봉투 형식 테스트

func TestSignForPredictAccountEnvelope(t *testing.T) {
	s, err := auth.NewSigner("4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3")
	if err != nil {
		t.Fatal(err)
	}
	acct := common.HexToAddress("0x1111111111111111111111111111111111111111")
	val := common.HexToAddress("0x845ADb2C711129d4f3966735eD98a9F09fC4cE57")
	digest := make([]byte, 32)
	for i := range digest {
		digest[i] = byte(i)
	}
	sig, err := SignForPredictAccount(digest, 56, acct, val, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 86 {
		t.Fatalf("봉투 %d바이트, 기대 86 (1 + 20 + 65)", len(sig))
	}
	if sig[0] != 0x01 {
		t.Errorf("첫 바이트 0x%02x, 기대 0x01", sig[0])
	}
	if got := common.BytesToAddress(sig[1:21]); got != val {
		t.Errorf("검증자 주소 %s, 기대 %s", got, val)
	}
	// sig65 의 마지막 바이트(v, 봉투 인덱스 85)는 27/28 이어야 한다. auth.SignHash
	// 가 이미 보정하므로 SignForPredictAccount 가 다시 더하면(예: 54/55) 여기서
	// 잡혀야 한다 — 길이·첫 바이트·검증자 주소만 보는 검사로는 v 이중 가산을
	// 놓친다(리뷰에서 실측: 이중 가산을 주입해도 길이 기반 테스트는 전부 초록불).
	if v := sig[85]; v != 27 && v != 28 {
		t.Errorf("서명 v 바이트 0x%02x, 기대 27 또는 28 — SignHash 가 보정한 값 위에 재가산됐을 수 있다", v)
	}
}

// SignEOA 는 predictAccount 를 쓰지 않는 EOA 직접 서명 경로다. Kernel 래핑이
// 없다는 점만 다르고 나머지 불변식(길이·v 값·결정성·입력 의존성)은
// SignForPredictAccount 와 같다. secp256k1 서명은 RFC 6979 로 결정적이므로
// 같은 키·같은 Order 는 항상 같은 바이트열을 낸다.
func TestSignEOA(t *testing.T) {
	s, err := auth.NewSigner("4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3")
	if err != nil {
		t.Fatal(err)
	}
	o, d := fixtureOrder(), fixtureDomain()

	sig1, err := SignEOA(o, d, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig1) != 65 {
		t.Fatalf("SignEOA 서명 %d바이트, 기대 65", len(sig1))
	}
	if v := sig1[64]; v != 27 && v != 28 {
		t.Errorf("SignEOA 서명 v 바이트 0x%02x, 기대 27 또는 28", v)
	}

	sig2, err := SignEOA(o, d, s)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sig1, sig2) {
		t.Error("같은 입력에서 SignEOA 서명이 다르다 — 재현 불가능하면 재시도·검증 로직이 깨진다")
	}

	changed := withOrder(o, func(oo *Order) { oo.Nonce = big.NewInt(1) })
	sig3, err := SignEOA(changed, d, s)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(sig1, sig3) {
		t.Error("Order 를 바꿔도 SignEOA 서명이 같다 — 다이제스트가 서명에 반영되지 않는다")
	}
}

// predictAccount 가 다르면 다이제스트가 달라야 한다 — 계정이 서명에 안 들어가면
// 남의 계정 주문에 내 서명이 통한다.
func TestKernelDigestDependsOnAccount(t *testing.T) {
	d := make([]byte, 32)
	a, _ := KernelDigest(d, 56, common.HexToAddress("0x1111111111111111111111111111111111111111"))
	b, _ := KernelDigest(d, 56, common.HexToAddress("0x2222222222222222222222222222222222222222"))
	if string(a) == string(b) {
		t.Fatal("predictAccount 가 달라도 Kernel 다이제스트가 같다")
	}
	c, _ := KernelDigest(d, 97, common.HexToAddress("0x1111111111111111111111111111111111111111"))
	if string(a) == string(c) {
		t.Fatal("chainId 가 달라도 Kernel 다이제스트가 같다 — 테스트넷 서명이 메인넷에 통한다")
	}
}
