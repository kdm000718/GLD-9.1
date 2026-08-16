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

// MaxStakeUSD 는 CapFraction 을 대체하지 않는다. 그 둘의 관계를 쓰는 쪽은
// exec 이지만, 어느 자본에서 어느 쪽이 이기는지는 여기 적어 두는 것이 낫다 —
// 이 상수를 만지는 사람이 보는 파일이기 때문이다.
func TestMaxStakeAndCapCrossOver(t *testing.T) {
	// 자본이 MaxStakeUSD/CapFraction 보다 작으면 cap 이 이긴다.
	cross := MaxStakeUSD / CapFraction // 219.78 USDT
	if got := Cap(Equity{AvailableUSDT: cross}); math.Abs(got-MaxStakeUSD) > 1e-9 {
		t.Errorf("자본 %v 에서 cap %v, 기대 %v", cross, got, MaxStakeUSD)
	}
	if got := Cap(Equity{AvailableUSDT: 65}); got >= MaxStakeUSD {
		t.Errorf("자본 65 의 cap 이 %v 다 — 지금 계좌 규모에서는 cap 이 이겨야 한다", got)
	}
}
