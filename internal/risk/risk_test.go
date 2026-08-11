package risk

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// 계획서 Step 1 의 테스트. 기대값은 계획서에 손으로 계산된 것을 그대로 쓴다.
// ---------------------------------------------------------------------------

// 강한 부등호다. 정확히 cap 인 주문은 허용하지 않는다.
func TestRemainingIsStrictlyBelowCap(t *testing.T) {
	e := Equity{AvailableUSDT: 1000}
	// cap = 1000 * 0.0455 = 45.5
	if got := Cap(e); got != 45.5 {
		t.Fatalf("cap = %v, 기대 45.5", got)
	}
	x := Exposure{FilledNotional: 45.5}
	if got := Remaining(e, x); got != 0 {
		t.Errorf("remaining = %v, 기대 0", got)
	}
}

// 취소 확인 전 주문도 살아있는 것으로 센다. 그러지 않으면 취소·재주문
// 경합 순간에 노출이 두 배가 된다.
func TestPendingCancelCountsAsExposure(t *testing.T) {
	e := Equity{AvailableUSDT: 1000} // cap 45.5
	x := Exposure{OpenNotional: 20, PendingCancel: 20}
	if got := Remaining(e, x); got != 5.5 {
		t.Errorf("remaining = %v, 기대 5.5 — 취소 미확인분을 빼먹었다", got)
	}
}

// equity 가 $22 아래면 cap 이 $1 미만이라 유효한 주문을 낼 수 없다.
func TestCanArmRequiresEquityAboveTwentyTwo(t *testing.T) {
	// 22 * 0.0455 = 1.001 > 1  → 무장 가능
	if !CanArm(Equity{AvailableUSDT: 22}) {
		t.Error("equity 22 에서 무장 불가로 판정했다 (cap 1.001 > 1)")
	}
	// 21 * 0.0455 = 0.9555 < 1 → 불가
	if CanArm(Equity{AvailableUSDT: 21}) {
		t.Error("equity 21 에서 무장 가능으로 판정했다 (cap 0.9555 < 1)")
	}
}

// 미정산 포지션은 취득원가로 센다(시가 아님).
func TestPositionCostCountsTowardEquity(t *testing.T) {
	e := Equity{AvailableUSDT: 100, PositionCost: 50}
	if got := Cap(e); got != 150*0.0455 {
		t.Errorf("cap = %v", got)
	}
}

func TestSharesRoundsDown(t *testing.T) {
	// 잔여 10 USD, 가격 0.49 → 20.408… 주 → 20 주
	if got := Shares(10, 0.49); got != 20 {
		t.Errorf("shares = %v, 기대 20", got)
	}
	// 나누어떨어져도 잔여를 다 쓰지 않는다. 20 주의 명목 9.8 은 잔여와
	// 정확히 같은데, 잔여를 다 쓰면 노출이 cap 과 같아져 사용자가 정한
	// 강한 부등호(`< equity × 0.0455`)를 위반한다. → 19 주.
	//
	// 계획서(2026-08-10-p5-trading-bot.md Task 3 Step 1)는 이 자리에서 20 을
	// 기대했다. 그 기대값이 사용자 제약과 충돌해서 기대값 쪽을 고쳤다.
	if got := Shares(9.8, 0.49); got != 19 {
		t.Errorf("shares = %v, 기대 19 — 잔여를 정확히 소진했다", got)
	}
}

func TestSharesRefusesBelowMinimum(t *testing.T) {
	if got := Shares(0.99, 0.49); got != 0 {
		t.Errorf("shares = %v, 기대 0 — 최소 주문 $1 미만", got)
	}
}

// ---------------------------------------------------------------------------
// 경계. `>` 와 `>=` 가 갈리는 자리마다 표본을 둔다.
// ---------------------------------------------------------------------------

// cap 이 정확히 $1 이 되는 equity 는 21.9780219780219781(= 1/0.0455)이다.
// 그 자본에서 낼 수 있는 유일한 유효 주문($1)은 노출을 cap 과 정확히 같게
// 만드는데 한도는 "미만"이므로 그 주문은 못 낸다 → 무장 불가.
func TestCanArmAtExactlyOneDollarCap(t *testing.T) {
	e := Equity{AvailableUSDT: 1 / CapFraction}
	if got := Cap(e); got != 1 {
		t.Fatalf("cap = %.20g, 기대 정확히 1 — 이 테스트의 전제가 깨졌다", got)
	}
	if CanArm(e) {
		t.Error("cap 이 정확히 $1 인데 무장 가능으로 판정했다 — `>` 가 아니라 `>=` 로 썼다")
	}
	// 한 눈금 위: 21.979 * 0.0455 = 1.0000445 > 1 → 가능
	if !CanArm(Equity{AvailableUSDT: 21.979}) {
		t.Error("equity 21.979 (cap 1.0000445) 에서 무장 불가로 판정했다")
	}
	// 한 눈금 아래: 21.978 * 0.0455 = 0.999999 < 1 → 불가
	if CanArm(Equity{AvailableUSDT: 21.978}) {
		t.Error("equity 21.978 (cap 0.999999) 에서 무장 가능으로 판정했다")
	}
}

// 최소 주문 문턱도 경계다. 명목이 정확히 $1 인 주문은 낸다.
func TestSharesAtExactMinimumNotional(t *testing.T) {
	// 잔여 1.50, 가격 0.50 → 3 주는 잔여를 정확히 소진하므로 안 되고,
	// 2 주의 명목은 정확히 $1.00 이다. 최소 주문은 경계를 포함하므로 허용.
	if got := Shares(1.5, 0.5); got != 2 {
		t.Errorf("shares = %v, 기대 2 — 명목 정확히 $1 은 최소 주문을 만족한다", got)
	}
	// 잔여 1.01, 가격 0.49 → 2 주 → 명목 0.98 < $1 → 거절
	if got := Shares(1.01, 0.49); got != 0 {
		t.Errorf("shares = %v, 기대 0 — 내림 후 명목 0.98 은 최소 주문 미달", got)
	}
	// 잔여가 정확히 최소 주문($1)이면 낼 수 있는 주문이 하나도 없다:
	// 명목이 $1 이상이면서 동시에 잔여 $1 미만일 수는 없기 때문이다.
	// CanArm 이 cap 정확히 $1 에서 false 인 것과 같은 규칙의 다른 얼굴이다.
	if got := Shares(1.0, 0.5); got != 0 {
		t.Errorf("shares = %v, 기대 0 — 잔여 $1 로는 강한 부등호를 만족하는 주문이 없다", got)
	}
	if got := Shares(1.0, 0.25); got != 0 {
		t.Errorf("shares = %v, 기대 0 — 잔여 $1 로는 강한 부등호를 만족하는 주문이 없다", got)
	}
}

// 잔여를 정확히 소진하는 입력에서 명목이 잔여보다 **작음**을 못박는다.
// 사용자 제약은 `회차당 최대 명목 < equity × 0.0455` 이고, 잔여를 다 쓰면
// 노출이 cap 과 정확히 같아져 그 강한 부등호를 위반한다.
//
// equity 1000 → cap 45.5, 노출 0 → 잔여 45.5. 가격 0.35 에서 45.5/0.35 은
// float64 에서도 정확히 130 이고 130×0.35 도 정확히 45.5 다 — 즉 반올림
// 오차에 기대지 않고 "정확히 cap" 에 도달하는 입력이다. 답은 129 주.
func TestSharesNeverExhaustsRemaining(t *testing.T) {
	e := Equity{AvailableUSDT: 1000}
	rem := Remaining(e, Exposure{})
	if rem != 45.5 {
		t.Fatalf("remaining = %v, 기대 45.5 — 이 테스트의 전제가 깨졌다", rem)
	}
	const price = 0.35

	got := Shares(rem, price)
	if got != 129 {
		t.Fatalf("shares = %v, 기대 129 — 130 주는 노출을 cap 과 정확히 같게 만든다", got)
	}
	if notional := got * price; notional >= rem {
		t.Errorf("명목 %v 가 잔여 %v 미만이 아니다", notional, rem)
	}
	if notional := got * price; notional >= Cap(e) {
		t.Errorf("명목 %v 가 cap %v 미만이 아니다", notional, Cap(e))
	}
}

// 노출이 cap 을 넘어서면 remaining 은 음수가 아니라 0 이다.
func TestRemainingClampsWhenOverExposed(t *testing.T) {
	e := Equity{AvailableUSDT: 1000} // cap 45.5
	x := Exposure{FilledNotional: 50}
	if got := Remaining(e, x); got != 0 {
		t.Errorf("remaining = %v, 기대 0 — 초과 노출에서 음수를 돌려줬다", got)
	}
}

// 노출 세 항이 전부 같은 무게로 빠지는지 하나씩 확인한다. 한 항만 빠뜨린
// 구현은 나머지 두 항의 테스트를 통과한다.
func TestRemainingCountsEveryExposureComponent(t *testing.T) {
	e := Equity{AvailableUSDT: 1000} // cap 45.5
	cases := []struct {
		name string
		x    Exposure
	}{
		{"filled", Exposure{FilledNotional: 5}},
		{"open", Exposure{OpenNotional: 5}},
		{"pendingCancel", Exposure{PendingCancel: 5}},
	}
	for _, tc := range cases {
		if got := Remaining(e, tc.x); got != 40.5 { // 45.5 − 5
			t.Errorf("%s: remaining = %v, 기대 40.5 — 이 항을 세지 않았다", tc.name, got)
		}
	}
}

// Total 은 항을 하나도 빠뜨리지 않는 단순 합이다.
func TestTotals(t *testing.T) {
	if got := (Equity{AvailableUSDT: 100, PositionCost: 50}).Total(); got != 150 {
		t.Errorf("Equity.Total = %v, 기대 150", got)
	}
	x := Exposure{FilledNotional: 1, OpenNotional: 2, PendingCancel: 4}
	if got := x.Total(); got != 7 {
		t.Errorf("Exposure.Total = %v, 기대 7", got)
	}
}

// ---------------------------------------------------------------------------
// 극단값. 상류(잔고 조회·원장 집계)가 망가졌을 때 사이저가 무한대를 내지
// 않는지 본다. 전부 "거래하지 않는" 쪽으로 실패해야 한다.
// ---------------------------------------------------------------------------

func TestCapRefusesBrokenEquity(t *testing.T) {
	inf := math.Inf(1)
	nan := math.NaN()
	cases := []struct {
		name string
		e    Equity
	}{
		{"available=+Inf", Equity{AvailableUSDT: inf}},
		{"available=-Inf", Equity{AvailableUSDT: math.Inf(-1)}},
		{"available=NaN", Equity{AvailableUSDT: nan}},
		{"available<0", Equity{AvailableUSDT: -1000}},
		{"positionCost=+Inf", Equity{AvailableUSDT: 1000, PositionCost: inf}},
		{"positionCost=NaN", Equity{AvailableUSDT: 1000, PositionCost: nan}},
		{"positionCost<0", Equity{AvailableUSDT: 1000, PositionCost: -1}},
		// 한 항이 음수인데 다른 항이 그것을 덮어 합은 양수인 경우. 합만
		// 보는 구현은 여기서 통과해 버린다 — 망가진 잔고 조회 위에서
		// 포지션 원가만 믿고 베팅 크기를 정하는 상태다.
		{"부호가 서로 상쇄", Equity{AvailableUSDT: -100, PositionCost: 1000}},
		// 두 항 모두 유한하지만 합이 넘친다.
		{"합이 오버플로", Equity{AvailableUSDT: math.MaxFloat64, PositionCost: math.MaxFloat64}},
	}
	for _, tc := range cases {
		if got := Cap(tc.e); got != 0 {
			t.Errorf("%s: cap = %v, 기대 0", tc.name, got)
		}
		// 같은 값이 들어오는 인접 함수도 같이 막혀 있어야 한다.
		if CanArm(tc.e) {
			t.Errorf("%s: CanArm 이 true — 망가진 equity 로 무장했다", tc.name)
		}
		if got := Remaining(tc.e, Exposure{}); got != 0 {
			t.Errorf("%s: remaining = %v, 기대 0", tc.name, got)
		}
	}
}

// equity 0 은 망가진 값이 아니라 정상적인 "돈 없음"이다. 결과는 같아야 한다.
func TestZeroEquityIsNoCapacity(t *testing.T) {
	e := Equity{}
	if got := Cap(e); got != 0 {
		t.Errorf("cap = %v, 기대 0", got)
	}
	if CanArm(e) {
		t.Error("equity 0 에서 무장 가능으로 판정했다")
	}
	if got := Remaining(e, Exposure{}); got != 0 {
		t.Errorf("remaining = %v, 기대 0", got)
	}
}

func TestRemainingRefusesBrokenExposure(t *testing.T) {
	e := Equity{AvailableUSDT: 1000} // cap 45.5
	inf := math.Inf(1)
	nan := math.NaN()
	cases := []struct {
		name string
		x    Exposure
	}{
		{"filled=NaN", Exposure{FilledNotional: nan}},
		{"filled=+Inf", Exposure{FilledNotional: inf}},
		// 음수 노출은 "여유가 더 있다"로 읽혀 한도를 넘기는 방향이다.
		{"filled<0", Exposure{FilledNotional: -100}},
		{"open=NaN", Exposure{OpenNotional: nan}},
		{"open<0", Exposure{OpenNotional: -100}},
		{"pendingCancel=NaN", Exposure{PendingCancel: nan}},
		{"pendingCancel<0", Exposure{PendingCancel: -100}},
		{"부호가 서로 상쇄", Exposure{FilledNotional: 1000, PendingCancel: -1000}},
		{"합이 오버플로", Exposure{FilledNotional: math.MaxFloat64, OpenNotional: math.MaxFloat64}},
	}
	for _, tc := range cases {
		if got := Remaining(e, tc.x); got != 0 {
			t.Errorf("%s: remaining = %v, 기대 0", tc.name, got)
		}
	}
}

func TestSharesRefusesBrokenInputs(t *testing.T) {
	inf := math.Inf(1)
	nan := math.NaN()
	cases := []struct {
		name             string
		remaining, price float64
	}{
		{"가격 0", 45.5, 0},
		{"가격 음수", 45.5, -0.49},
		{"가격 NaN", 45.5, nan},
		{"가격 +Inf", 45.5, inf},
		{"잔여 NaN", nan, 0.49},
		{"잔여 +Inf", inf, 0.49},
		{"잔여 -Inf", math.Inf(-1), 0.49},
		{"잔여 음수", -45.5, 0.49},
		// 가격이 터무니없이 작으면 주식 수가 float64 의 정수 정확도(2^53)를
		// 넘는다. 그 위에서는 n−1 이 n 과 같아져 위의 한도 초과 보정이
		// 성립하지 않고, 애초에 이만한 주식 수는 어떤 하류 정수 변환에서도
		// 살아남지 못한다.
		//
		// 2/1e-18 = 2e18 주는 명목(1.9999999999999998)이 잔여 2 보다
		// **작아서** 강한 부등호 검사도 통과한다 — 2^53 가드만 잡는다.
		{"주식 수가 2^53 을 넘음", 2, 1e-18},
		{"주식 수가 2^53 을 넘음(큰 잔여)", 45.5, 1e-15},
		{"가격이 비정상적으로 작음", 45.5, 1e-300},
		// 반대쪽 끝: 가격이 잔여보다 크면 한 주도 못 산다.
		{"가격이 잔여보다 큼", 45.5, 100},
		{"가격이 비정상적으로 큼", 45.5, 1e300},
	}
	for _, tc := range cases {
		got := Shares(tc.remaining, tc.price)
		if got != 0 {
			t.Errorf("%s: shares = %v, 기대 0", tc.name, got)
		}
	}
}

// 십진수로는 나누어떨어져 보이지만 float64 에서는 몫이 정수 바로 아래인
// 경우다. 여기서 올림하면 한 주 더 사게 되는데 그 한 주의 참값 원가는 잔여를
// 넘는다 — 넘는 폭이 곱셈 반올림에 묻혀서 사후 검사로는 잡히지 않는다.
// 그래서 반올림 방향 자체가 아래여야 한다.
//
//	잔여 8.1 은 float64 로 8.0999999999999996447,
//	가격 0.001 은 0.001000000000000000020816…
//	→ 8100 주의 참값 원가 8.1000000000000001686 > 잔여. 8100 주는 못 산다.
//	→ 답은 8099 주(원가 8.099).
func TestSharesRoundsDownWhenQuotientSitsJustBelowAnInteger(t *testing.T) {
	if got := Shares(8.1, 0.001); got != 8099 {
		t.Errorf("shares = %v, 기대 8099 — 올림했다", got)
	}
	// 정밀도 2 의 틱에서도 같은 일이 일어난다.
	// 140/0.28: 0.28 은 0.28000000000000002665 이라 500 주는 잔여를 넘는다.
	if got := Shares(140, 0.28); got != 499 {
		t.Errorf("shares = %v, 기대 499 — 올림했다", got)
	}
}

// 내림한 주식 수의 명목이 잔여를 넘지 않는지 본다. math.Floor 만으로는
// 부족하다 — 나눗셈이 먼저 반올림되므로 몫이 참값보다 클 수 있다.
// 실제 틱 가격(0.072)에서 나오는 사례다.
func TestSharesNeverExceedsRemaining(t *testing.T) {
	const price = 0.072
	const n = 26734.0
	// n 주의 명목보다 1 ulp 작은 잔여. n 주는 살 수 없고 n−1 주가 답이다.
	remaining := math.Nextafter(n*price, 0)

	got := Shares(remaining, price)
	if got != n-1 {
		t.Errorf("shares = %v, 기대 %v — 내림 결과가 잔여를 넘었다", got, n-1)
	}
	if got*price > remaining {
		t.Errorf("명목 %.20g 이 잔여 %.20g 를 넘었다", got*price, remaining)
	}
}

// 보정을 한 번 해도 명목이 여전히 잔여와 같으면 주문하지 않는다.
//
// 가격이 잔여의 1 ulp 보다 작으면 연속한 두 배수가 같은 float64 로 반올림되어
// n 주와 n−1 주의 명목이 **둘 다** 잔여와 같아진다. 이때 보정(n--)은 명목을
// 잔여 아래로 못 내린다. 그래서 마지막 방어선의 부등호도 `>` 가 아니라 `>=`
// 여야 한다 — `>` 면 명목이 잔여와 정확히 같은 주문을 통과시킨다.
//
//	잔여 514, 가격 3×2^-45 (둘 다 float64 에서 정확)
//	→ n = 6028255751219883 주 (2^53 = 9007199254740992 미만이라 위 가드에 안 걸림)
//	→ n 주도 n−1 주도 명목이 정확히 514 = 잔여 → 주문 없음
func TestSharesRefusesWhenCorrectionCannotGetBelowRemaining(t *testing.T) {
	const remaining = 514.0
	price := 3 * math.Ldexp(1, -45) // 3 × 2^-45

	n := math.Floor(remaining / price)
	if n >= 1<<53 {
		t.Fatalf("n = %g 이 2^53 이상이라 다른 가드가 먼저 잡는다 — 전제가 깨졌다", n)
	}
	if (n-1)*price != remaining {
		t.Fatalf("n−1 주의 명목이 잔여와 같지 않다 — 전제가 깨졌다")
	}

	if got := Shares(remaining, price); got != 0 {
		t.Errorf("shares = %v, 기대 0 — 명목이 잔여와 정확히 같은 주문을 통과시켰다", got)
	}
}

// ---------------------------------------------------------------------------
// 일손실 한도
// ---------------------------------------------------------------------------

func TestDailyLimitBoundary(t *testing.T) {
	d := DailyLimit{StartEquity: 1000, Fraction: 0.10} // 문턱 −100
	cases := []struct {
		name string
		pnl  float64
		want bool
	}{
		{"정확히 −10% 는 차단", -100, true},
		{"−10% 를 넘긴 손실", -100.01, true},
		{"−10% 직전", -99.99, false},
		{"손익 0", 0, false},
		{"이익", 50, false},
	}
	for _, tc := range cases {
		if got := d.Breached(tc.pnl); got != tc.want {
			t.Errorf("%s: Breached(%v) = %v, 기대 %v", tc.name, tc.pnl, got, tc.want)
		}
	}
}

// 설정이 망가졌으면 차단이다. 특히 제로값은 "한도 없음"이 아니다.
//
// pnl 은 케이스마다 **가드가 없으면 false 가 나오는 값**으로 골랐다. 이익
// 구간(+5)을 쓰지 않고 0 을 쓰면 −StartEquity×Fraction 이 0 이 되는 오설정
// 케이스들이 가드 없이도 우연히 true 가 되어 아무것도 검사하지 못한다.
func TestDailyLimitRefusesMisconfiguration(t *testing.T) {
	nan := math.NaN()
	inf := math.Inf(1)
	cases := []struct {
		name string
		d    DailyLimit
		pnl  float64
	}{
		{"제로값", DailyLimit{}, 5},
		{"Fraction 미설정", DailyLimit{StartEquity: 1000}, 5},
		{"StartEquity 미설정", DailyLimit{Fraction: 0.10}, 5},
		// 부호가 뒤집힌 설정은 문턱을 +100 으로 만든다. 이익 5 로는
		// 가드 없이도 true 가 나오므로 문턱 위의 200 을 쓴다.
		{"StartEquity 음수", DailyLimit{StartEquity: -1000, Fraction: 0.10}, 200},
		{"Fraction 음수", DailyLimit{StartEquity: 1000, Fraction: -0.10}, 200},
		{"StartEquity NaN", DailyLimit{StartEquity: nan, Fraction: 0.10}, 5},
		{"StartEquity +Inf", DailyLimit{StartEquity: inf, Fraction: 0.10}, 5},
		{"Fraction NaN", DailyLimit{StartEquity: 1000, Fraction: nan}, 5},
		{"Fraction 이 자본 전부보다 큼", DailyLimit{StartEquity: 1000, Fraction: 1.5}, 5},
		{"손익 NaN", DailyLimit{StartEquity: 1000, Fraction: 0.10}, nan},
		{"손익 +Inf", DailyLimit{StartEquity: 1000, Fraction: 0.10}, inf},
		// −Inf 는 가드가 없어도 차단으로 읽힌다. 동작을 못박는 케이스다.
		{"손익 -Inf", DailyLimit{StartEquity: 1000, Fraction: 0.10}, math.Inf(-1)},
	}
	for _, tc := range cases {
		if !tc.d.Breached(tc.pnl) {
			t.Errorf("%s: Breached = false, 기대 true — 망가진 설정을 한도 없음으로 읽었다", tc.name)
		}
	}
	// Fraction 1.0 은 오설정이 아니다(자본 전부를 잃으면 멈춘다).
	d := DailyLimit{StartEquity: 1000, Fraction: 1}
	if d.Breached(-999) {
		t.Error("Fraction 1.0, 손실 999 에서 차단했다")
	}
	if !d.Breached(-1000) {
		t.Error("Fraction 1.0, 손실 1000 에서 차단하지 않았다")
	}
}

// 기본값 상수가 스펙과 같은지 못박는다.
func TestConstants(t *testing.T) {
	if CapFraction != 0.0455 {
		t.Errorf("CapFraction = %v, 기대 0.0455", CapFraction)
	}
	if MinOrderUSD != 1.0 {
		t.Errorf("MinOrderUSD = %v, 기대 1.0", MinOrderUSD)
	}
	if DefaultDailyFraction != 0.10 {
		t.Errorf("DefaultDailyFraction = %v, 기대 0.10", DefaultDailyFraction)
	}
}
