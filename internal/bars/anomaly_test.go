package bars

import "testing"

func TestFindAnomaliesCleanSeriesReturnsNone(t *testing.T) {
	b := sample(10)
	if an := FindAnomalies(b); len(an) != 0 {
		t.Errorf("정상 시계열인데 이상 %d건 검출: %+v", len(an), an)
	}
}

func TestFindAnomaliesReportsNonIncreasingTransition(t *testing.T) {
	b := sample(10)
	// 5번의 close_time 을 6번과 같게 만든다 — 5→6 전환이 엄격증가가 아니다
	b.CloseTime[5] = b.CloseTime[6]
	an := FindAnomalies(b)
	found := false
	for _, a := range an {
		if a.Kind == "close_not_increasing" && a.Index == 5 {
			found = true
		}
	}
	if !found {
		t.Errorf("idx 5 의 close_not_increasing 이 검출되지 않았다: %+v", an)
	}
}

func TestFindAnomaliesReportsCloseBeforeOpen(t *testing.T) {
	b := sample(10)
	b.CloseTime[3] = b.OpenTime[3] - 1
	an := FindAnomalies(b)
	found := false
	for _, a := range an {
		if a.Kind == "close_before_open" && a.Index == 3 {
			found = true
		}
	}
	if !found {
		t.Errorf("idx 3 의 close_before_open 이 검출되지 않았다: %+v", an)
	}
}

func TestFindAnomaliesReportsBoth(t *testing.T) {
	b := sample(10)
	b.CloseTime[2] = b.OpenTime[2] - 1 // close_before_open at 2
	b.CloseTime[7] = b.CloseTime[6]    // close_not_increasing at 6
	an := FindAnomalies(b)
	kinds := map[string]bool{}
	for _, a := range an {
		kinds[a.Kind] = true
	}
	if !kinds["close_before_open"] || !kinds["close_not_increasing"] {
		t.Errorf("두 종류가 모두 검출되지 않았다: %+v", an)
	}
}

func TestFindAnomaliesHandlesEmptyAndSingle(t *testing.T) {
	if an := FindAnomalies(Bars{}); len(an) != 0 {
		t.Errorf("빈 시계열인데 이상 %d건 검출", len(an))
	}
	b := sample(1)
	if an := FindAnomalies(b); len(an) != 0 {
		t.Errorf("봉 1개인데 이상 %d건 검출", len(an))
	}
}

func TestFindAnomaliesReturnsIndexOrder(t *testing.T) {
	b := sample(10)
	b.CloseTime[2] = b.OpenTime[2] - 1
	b.CloseTime[7] = b.CloseTime[6]
	an := FindAnomalies(b)
	for i := 1; i < len(an); i++ {
		if an[i].Index < an[i-1].Index {
			t.Errorf("인덱스 순서가 아니다: %+v", an)
		}
	}
}
