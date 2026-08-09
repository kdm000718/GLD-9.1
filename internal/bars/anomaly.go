package bars

import "fmt"

// Anomaly 는 시계열에서 발견된 이상 지점이다. 데이터를 고치지 않고 보고만 한다.
type Anomaly struct {
	Index int
	Kind  string // "close_not_increasing" | "close_before_open"
	Msg   string
}

// FindAnomalies 는 CloseTime 의 단조성과 close/open 순서를 검사한다.
// 이진탐색(clock.cut)이 CloseTime 의 단조성을 전제하므로, 그 전제가
// 깨지는 지점을 실행할 때마다 드러내기 위한 것이다.
// 데이터는 절대 수정하지 않는다 — Python 참조 구현과 동일한 입력을 유지해야
// 9년 재현이 성립한다.
func FindAnomalies(b Bars) []Anomaly {
	var out []Anomaly
	n := b.Len()
	for i := 0; i < n; i++ {
		if b.CloseTime[i] <= b.OpenTime[i] {
			out = append(out, Anomaly{
				Index: i, Kind: "close_before_open",
				Msg: fmt.Sprintf("close_time=%d <= open_time=%d", b.CloseTime[i], b.OpenTime[i]),
			})
		}
		if i+1 < n && b.CloseTime[i] >= b.CloseTime[i+1] {
			out = append(out, Anomaly{
				Index: i, Kind: "close_not_increasing",
				Msg: fmt.Sprintf("close_time[%d]=%d >= close_time[%d]=%d", i, b.CloseTime[i], i+1, b.CloseTime[i+1]),
			})
		}
	}
	return out
}
