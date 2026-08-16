package risk

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// 확신도별 크기 (stake.go)
// ---------------------------------------------------------------------------

// 표의 값을 그대로 못 박는다. 이 숫자들은 2019~2024 106,951회차 교정에서 나온
// 것이고, 손으로 고치려면 그 계산을 다시 해야 한다는 것을 이 시험이 알린다.
func TestConfidenceWeightTable(t *testing.T) {
	cases := []struct {
		conf float64
		want float64
	}{
		{0.10, 0.0792},
		{0.11, 0.0792},
		{0.12, 0.1891},
		{0.135, 0.1891},
		{0.14, 0.3479},
		{0.16, 0.4525},
		{0.18, 0.5657},
		{0.20, 0.8545},
		{0.24, 0.9296},
		{0.30, 1.0000},
		{0.50, 1.0000},
		{1.00, 1.0000},
	}
	for _, tc := range cases {
		if got := ConfidenceWeight(tc.conf); got != tc.want {
			t.Errorf("ConfidenceWeight(%v) = %v, 기대 %v", tc.conf, got, tc.want)
		}
	}
}

// 첫 칸 아래는 0 이다 — 그 구간은 교정 승률이 손익분기 아래라 켈리가 0 이다.
//
// 경계는 **포함**이다(`>=`). live.ConfidenceThreshold 의 부등호와 같은 방향이
// 아니면, 문턱을 통과한 회차가 크기 0 을 받아 조용히 아무것도 안 하는 상태가
// 된다.
func TestConfidenceWeightIsZeroBelowTheFirstBin(t *testing.T) {
	if got := ConfidenceWeight(confBins[0]); got != confWeights[0] {
		t.Errorf("첫 칸의 하한이 기각됐다 (%v) — 부등호가 > 로 바뀌었다", got)
	}
	if got := ConfidenceWeight(math.Nextafter(confBins[0], 0)); got != 0 {
		t.Errorf("첫 칸 바로 아래가 %v 를 받았다, 기대 0", got)
	}
	for _, c := range []float64{0, 0.0172, 0.05, 0.0999} {
		if got := ConfidenceWeight(c); got != 0 {
			t.Errorf("ConfidenceWeight(%v) = %v, 기대 0", c, got)
		}
	}
}

// 망가진 입력에서는 걸지 않는다 — 이 패키지 전체의 규칙이다.
func TestConfidenceWeightRefusesBrokenInput(t *testing.T) {
	for _, c := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, -0.5} {
		if got := ConfidenceWeight(c); got != 0 {
			t.Errorf("ConfidenceWeight(%v) = %v, 기대 0", c, got)
		}
		if got := StakeTarget(c); got != 0 {
			t.Errorf("StakeTarget(%v) = %v, 기대 0", c, got)
		}
	}
	// +Inf 는 "무한히 확신한다"로 읽히면 최대 크기가 된다. 그 경로가 열려
	// 있으면 안 된다 — p_up 이 0 이나 1 로 망가지는 경우가 실제로 있었다.
	if got := StakeTarget(math.Inf(1)); got != 0 {
		t.Errorf("confidence=+Inf 가 %v 를 받았다 — 고장이 최대 확신으로 읽혔다", got)
	}
}

// 확신이 커질수록 크게 건다. 뒤집히면 모델이 덜 확신하는 자리에 더 크게 거는
// 봇이 되고, 그것은 에러 없이 매 회차 조용히 손해를 키운다.
func TestConfidenceWeightIsMonotone(t *testing.T) {
	prev := -1.0
	for i, w := range confWeights {
		if w <= prev {
			t.Errorf("%d번째 가중치 %v 가 앞칸 %v 보다 크지 않다", i, w, prev)
		}
		prev = w
	}
	if last := confWeights[len(confWeights)-1]; last != 1.0 {
		t.Errorf("마지막 가중치가 %v 다 — 정규화가 깨졌다 (최고 칸이 1.0 이어야 상한이 상한이다)", last)
	}
	for i := 1; i < len(confBins); i++ {
		if confBins[i] <= confBins[i-1] {
			t.Errorf("%d번째 칸 하한 %v 가 오름차순이 아니다", i, confBins[i])
		}
	}
	if len(confBins) != len(confWeights) {
		t.Fatalf("칸 %d개, 가중치 %d개", len(confBins), len(confWeights))
	}
}

// StakeTarget 은 어떤 confidence 에서도 MaxStakeUSD 를 넘지 않는다.
func TestStakeTargetNeverExceedsMax(t *testing.T) {
	for c := 0.0; c <= 1.5; c += 0.001 {
		if got := StakeTarget(c); got > MaxStakeUSD {
			t.Fatalf("StakeTarget(%v) = %v > %v", c, got, MaxStakeUSD)
		}
	}
	if got := StakeTarget(0.30); got != MaxStakeUSD {
		t.Errorf("최고 칸이 %v, 기대 %v — 상한을 다 쓰지 못하고 있다", got, MaxStakeUSD)
	}
}

// **실효 문턱이 어디인지 못 박는다.**
//
// 목표가 [MinOrderUSD] 미만인 칸은 주문이 성립하지 않는다. MaxStakeUSD 를
// 바꾸면 실효 문턱이 함께 움직이므로, 그 사실이 시험에 남아 있어야 한다.
// 지금 값(10 USDT)에서는 첫 칸만 잘리고 실효 문턱은 0.12 다.
func TestSmallStakesFallBelowTheMinimumOrder(t *testing.T) {
	if got := StakeTarget(0.10); got >= MinOrderUSD {
		t.Errorf("conf 0.10 의 목표가 %v 다 — 최소 주문 이상이면 실효 문턱이 0.10 이라는 뜻이고, "+
			"이 시험의 전제가 바뀌었다", got)
	}
	if got := StakeTarget(0.12); got < MinOrderUSD {
		t.Errorf("conf 0.12 의 목표가 %v 로 최소 주문 미만이다 — 실효 문턱이 0.14 로 올라갔다", got)
	}
}

// StakeRemaining 은 목표에서 노출을 뺀다. 부등호와 실패 방향은 [Remaining] 과
// 같아야 한다 — 그 자리를 대신하기 때문이다.
func TestStakeRemainingSubtractsExposure(t *testing.T) {
	e := Equity{AvailableUSDT: 1000}
	const conf = 0.30 // 최고 칸 → 목표 = MaxStakeUSD
	if got := StakeRemaining(e, Exposure{}, conf); got != MaxStakeUSD {
		t.Errorf("노출 0: %v, 기대 %v", got, MaxStakeUSD)
	}
	if got := StakeRemaining(e, Exposure{FilledNotional: 4}, conf); got != MaxStakeUSD-4 {
		t.Errorf("노출 4: %v, 기대 %v", got, MaxStakeUSD-4)
	}
	// 취소 미확인분도 센다 — 아직 체결될 수 있는 주문이다.
	if got := StakeRemaining(e, Exposure{OpenNotional: 3, PendingCancel: 3}, conf); got != MaxStakeUSD-6 {
		t.Errorf("미체결 3 + 취소대기 3: %v, 기대 %v", got, MaxStakeUSD-6)
	}
	// 정확히 목표면 0 이다. 목표는 도달해도 되는 선이 아니다.
	if got := StakeRemaining(e, Exposure{FilledNotional: MaxStakeUSD}, conf); got != 0 {
		t.Errorf("노출이 목표와 같을 때 %v, 기대 0", got)
	}
	if got := StakeRemaining(e, Exposure{FilledNotional: MaxStakeUSD + 1}, conf); got != 0 {
		t.Errorf("노출이 목표를 넘었을 때 %v, 기대 0", got)
	}
}

// **가용잔고가 마지막 끈이다.** equity 비례 상한이 빠진 뒤 자본과 주문 크기를
// 잇는 것은 이것뿐이다. 이 검사가 사라지면 잔고 3 USDT 로 10 USDT 주문을 낸다.
func TestStakeRemainingNeverExceedsTheBalance(t *testing.T) {
	const conf = 0.30
	for _, avail := range []float64{0, 0.5, 1, 3, 9.99} {
		if got := StakeRemaining(Equity{AvailableUSDT: avail}, Exposure{}, conf); got > avail {
			t.Errorf("잔고 %v 에서 %v 를 내줬다", avail, got)
		}
	}
	if got := StakeRemaining(Equity{AvailableUSDT: 3}, Exposure{}, conf); got != 3 {
		t.Errorf("잔고 3: %v, 기대 3", got)
	}
	// PositionCost 는 보지 않는다 — 이미 나간 돈이고 지금 쓸 수 있는 것이 아니다.
	if got := StakeRemaining(Equity{AvailableUSDT: 3, PositionCost: 1000}, Exposure{}, conf); got != 3 {
		t.Errorf("미정산 포지션이 잔고를 부풀렸다: %v, 기대 3", got)
	}
}

// 망가진 입력에서는 0 이다.
func TestStakeRemainingRefusesBrokenInput(t *testing.T) {
	const conf = 0.30
	ok := Equity{AvailableUSDT: 1000}
	for _, e := range []Equity{
		{AvailableUSDT: math.NaN()},
		{AvailableUSDT: math.Inf(1)},
		{AvailableUSDT: -1},
	} {
		if got := StakeRemaining(e, Exposure{}, conf); got != 0 {
			t.Errorf("잔고 %v: %v, 기대 0", e.AvailableUSDT, got)
		}
	}
	for _, x := range []Exposure{
		{FilledNotional: -1},
		{OpenNotional: math.NaN()},
		{PendingCancel: math.Inf(1)},
	} {
		if got := StakeRemaining(ok, x, conf); got != 0 {
			t.Errorf("노출 %+v: %v, 기대 0", x, got)
		}
	}
	// 첫 칸 아래는 잔고가 아무리 많아도 0 이다.
	if got := StakeRemaining(ok, Exposure{}, 0.05); got != 0 {
		t.Errorf("conf 0.05: %v, 기대 0", got)
	}
}

// **equity 비례 상한(CapFraction)은 크기 결정에서 빠졌다** (2026-08-17 사용자
// 결정). 그 사실을 여기 못 박는다 — 되살리는 것은 전략 변경이다.
//
// [Cap]·[CanArm] 은 남아 있지만 무장 판정과 진단에만 쓰인다. 자본 65 USDT 의
// cap 은 2.96 이고, 그 값이 크기에 다시 개입하면 확신도별 차등이 위쪽 다섯
// 칸에서 통째로 사라진다.
func TestCapNoLongerBoundsTheStake(t *testing.T) {
	e := Equity{AvailableUSDT: 65} // cap 2.9575
	if c := Cap(e); c >= StakeTarget(0.30) {
		t.Fatalf("전제가 깨졌다: cap %v 가 목표 %v 이상이다", c, StakeTarget(0.30))
	}
	if got := StakeRemaining(e, Exposure{}, 0.30); got != StakeTarget(0.30) {
		t.Errorf("StakeRemaining = %v, 기대 %v — cap 이 크기 결정에 되살아났다",
			got, StakeTarget(0.30))
	}
}
