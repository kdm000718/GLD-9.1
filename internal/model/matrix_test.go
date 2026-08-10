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

// Truncate 는 줄이기만 해야 한다. 다시 키우면 Rows 가 자신이 지키는 대상인
// Data 의 실제 유효 범위와 어긋나 Row/SetRow 의 범위 검사를 무력화한다.
func TestTruncateCannotGrowBack(t *testing.T) {
	m := NewMatrix(5, 2)
	m.Truncate(2)
	defer func() {
		if recover() == nil {
			t.Fatal("Truncate(2) 뒤 Truncate(5) 가 패닉하지 않았다 — 잘려나간 행이 다시 살아난다")
		}
	}()
	m.Truncate(5)
}

// 경계값 i == m.Rows 를 명시적으로 고정한다. 여분 용량이 있는 행렬에서 검사해야
// 내장 슬라이스 패닉에 가려지지 않고, 가드를 '>' 로 잘못 써도 이 테스트가 잡는다.
func TestRowPanicsAtExactBoundary(t *testing.T) {
	m := NewMatrix(10, 2)
	m.Truncate(2)
	defer func() {
		if recover() == nil {
			t.Fatal("Row(m.Rows) 가 패닉하지 않았다")
		}
	}()
	_ = m.Row(m.Rows)
}

func TestSetRowPanicsAtExactBoundary(t *testing.T) {
	m := NewMatrix(10, 2)
	m.Truncate(2)
	defer func() {
		if recover() == nil {
			t.Fatal("SetRow(m.Rows, ...) 가 패닉하지 않았다")
		}
	}()
	m.SetRow(m.Rows, []float64{1, 2})
}

// Truncate(n) 에서 n 이 현재 Rows 와 정확히 같은 경계. 줄이는 것도 늘리는
// 것도 아닌 no-op 이므로 패닉해서는 안 되고, Rows·Data 길이·행 내용이 전부
// 그대로 남아야 한다. 패닉 여부만 보면 Truncate 가 no-op 을 어기고 데이터를
// 건드려도 통과하므로 내용까지 확인한다. 여분 용량이 있는 행렬에서 검사해야
// 이 경계를 지나는 유일한 기존 테스트(TestMatrixTruncate 는 Rows=10→4,
// TestTruncateCannotGrowBack 은 Rows=2→5)와 겹치지 않는다.
func TestTruncateNoOpAtCurrentRows(t *testing.T) {
	m := NewMatrix(10, 2)
	m.Truncate(2)
	m.SetRow(0, []float64{7, 8})
	m.SetRow(1, []float64{9, 10})

	m.Truncate(2)

	if m.Rows != 2 {
		t.Fatalf("Rows = %d, 기대 2", m.Rows)
	}
	if len(m.Data) != 4 {
		t.Fatalf("Data 길이 %d, 기대 4", len(m.Data))
	}
	if m.Row(0)[0] != 7 || m.Row(0)[1] != 8 {
		t.Errorf("Row(0) = %v, 기대 [7 8]", m.Row(0))
	}
	if m.Row(1)[0] != 9 || m.Row(1)[1] != 10 {
		t.Errorf("Row(1) = %v, 기대 [9 10]", m.Row(1))
	}
}
