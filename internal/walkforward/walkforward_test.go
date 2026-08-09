package walkforward

import "testing"

func TestTrainableBeforeExcludesUnresolvedLabels(t *testing.T) {
	// 5분봉이 5분마다 하나씩. cutoff 시점에 정답이 확정된 것만 학습 가능하다.
	var cs []int64
	for i := 0; i < 2000; i++ {
		cs = append(cs, int64(i)*FiveMinMS)
	}
	cutoff := int64(1000) * FiveMinMS
	got := TrainableBefore(cs, cutoff, 100*DayMS)

	inSet := map[int]bool{}
	for _, i := range got {
		inSet[i] = true
	}
	// cs[999] 는 999*5분 에 열려 1000*5분 에 닫힌다 → 정확히 cutoff 에 확정. 포함.
	if !inSet[999] {
		t.Error("cs[999] 는 cutoff 에 정답이 확정되므로 학습 가능해야 한다")
	}
	// cs[1000] 은 cutoff 이후에 닫힌다 → 제외.
	if inSet[1000] {
		t.Error("cs[1000] 은 정답이 확정되지 않았는데 학습에 포함됐다")
	}
	if inSet[1500] {
		t.Error("미래 표본이 학습에 포함됐다")
	}
}

func TestTrainableBeforeRespectsWindow(t *testing.T) {
	var cs []int64
	for i := 0; i < 2000; i++ {
		cs = append(cs, int64(i)*FiveMinMS)
	}
	cutoff := int64(1000) * FiveMinMS
	window := int64(10) * FiveMinMS
	got := TrainableBefore(cs, cutoff, window)
	for _, i := range got {
		if cs[i] < cutoff-window {
			t.Fatalf("창 밖의 표본 %d (cs=%d, 하한=%d)", i, cs[i], cutoff-window)
		}
	}
	if len(got) != 10 {
		t.Errorf("표본 %d개, 기대 10개", len(got))
	}
}

func TestTrainableBeforeEmptyWhenNothingResolved(t *testing.T) {
	cs := []int64{100 * FiveMinMS, 101 * FiveMinMS}
	if got := TrainableBefore(cs, 0, DayMS); len(got) != 0 {
		t.Errorf("cutoff 가 0 인데 %d개를 돌려줬다", len(got))
	}
}
