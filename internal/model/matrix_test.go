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
