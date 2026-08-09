package model

import "testing"

func TestMatrixRowRoundTrip(t *testing.T) {
	m := NewMatrix(3, 4)
	m.SetRow(1, []float64{1, 2, 3, 4})
	r := m.Row(1)
	if len(r) != 4 {
		t.Fatalf("행 길이 %d, 기대 4", len(r))
	}
	for i, want := range []float32{1, 2, 3, 4} {
		if r[i] != want {
			t.Errorf("Row(1)[%d] = %v, 기대 %v", i, r[i], want)
		}
	}
	// 다른 행은 건드리지 않는다
	for _, v := range m.Row(0) {
		if v != 0 {
			t.Errorf("Row(0) 가 오염됐다: %v", m.Row(0))
			break
		}
	}
}

func TestMatrixTruncate(t *testing.T) {
	m := NewMatrix(10, 3)
	m.SetRow(0, []float64{7, 8, 9})
	m.Truncate(4)
	if m.Rows != 4 {
		t.Fatalf("Rows = %d, 기대 4", m.Rows)
	}
	if len(m.Data) != 12 {
		t.Errorf("Data 길이 %d, 기대 12", len(m.Data))
	}
	if m.Row(0)[0] != 7 {
		t.Error("Truncate 가 남은 데이터를 훼손했다")
	}
}

func TestRowPanicsOutsideTruncatedRange(t *testing.T) {
	m := NewMatrix(5, 2)
	for i := 0; i < 5; i++ {
		m.SetRow(i, []float64{float64(i), float64(i)})
	}
	m.Truncate(2)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Truncate(2) 뒤 Row(4) 가 패닉하지 않았다 — 낡은 행이 조용히 돌아온다")
		}
	}()
	_ = m.Row(4)
}

// Truncate 로 줄인 뒤 여분 용량 안쪽 행에 쓰는 경우. 내장 슬라이스 패닉이
// 대신 걸리지 않으므로 SetRow 자신의 범위 검사만이 이것을 막는다.
func TestSetRowPanicsWithinCapacityAfterTruncate(t *testing.T) {
	m := NewMatrix(10, 2)
	m.Truncate(2)
	defer func() {
		if recover() == nil {
			t.Fatal("Truncate(2) 뒤 SetRow(5,...) 가 패닉하지 않았다 — 잘려나간 행에 조용히 쓴다")
		}
	}()
	m.SetRow(5, []float64{1, 2})
}

func TestSetRowRejectsWrongLength(t *testing.T) {
	m := NewMatrix(2, 3)
	defer func() {
		if recover() == nil {
			t.Fatal("길이가 다른 행을 받아들였다")
		}
	}()
	m.SetRow(0, []float64{1, 2})
}
