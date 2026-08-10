package model

import "fmt"

// Matrix 는 행 우선 평면 float32 행렬이다.
// 100만 행 × 60 피처 규모에서 [][]float64 는 메모리와 GC 압력이 과하다.
type Matrix struct {
	Rows int
	Cols int
	Data []float32
}

func NewMatrix(rows, cols int) *Matrix {
	return &Matrix{Rows: rows, Cols: cols, Data: make([]float32, rows*cols)}
}

// Row 는 i 번째 행을 돌려준다.
//
// 범위를 명시적으로 검사하는 이유: 슬라이스 재슬라이싱의 상한은 len 이 아니라 cap
// 이다. Truncate 로 Rows 를 줄여도 Data 의 cap 은 그대로이므로, 검사가 없으면
// Truncate(2) 뒤의 Row(4) 가 패닉 없이 '지워진' 행을 돌려준다. 학습·추론에서
// 그것은 조용히 틀린 결과가 된다.
func (m *Matrix) Row(i int) []float32 {
	if i < 0 || i >= m.Rows {
		panic(fmt.Sprintf("Matrix.Row: 행 %d 는 범위 밖이다 (Rows=%d)", i, m.Rows))
	}
	return m.Data[i*m.Cols : (i+1)*m.Cols]
}

func (m *Matrix) SetRow(i int, v []float64) {
	if i < 0 || i >= m.Rows {
		panic(fmt.Sprintf("Matrix.SetRow: 행 %d 는 범위 밖이다 (Rows=%d)", i, m.Rows))
	}
	if len(v) != m.Cols {
		panic(fmt.Sprintf("Matrix.SetRow: 값 %d개, 기대 %d개", len(v), m.Cols))
	}
	row := m.Data[i*m.Cols : (i+1)*m.Cols]
	for j, x := range v {
		row[j] = float32(x)
	}
}

// Truncate 는 앞 n행만 남긴다. 표본을 미리 크게 잡고 실제 개수만큼 줄일 때 쓴다.
//
// n 이 현재 Rows 보다 크면 거부한다: Data 는 원래 크기의 cap 을 그대로 갖고
// 있으므로, 다시 키우는 걸 허용하면 Truncate(2) 뒤 Truncate(5) 가 잘려나갔던
// 행을 조용히 되살린다 — Row/SetRow 의 범위 검사가 지키는 Rows 자체가 그때
// 오염되므로 그 검사도 무력화된다. Truncate 는 줄이는 용도로만 쓴다.
func (m *Matrix) Truncate(n int) {
	if n < 0 || n > m.Rows {
		panic(fmt.Sprintf("Matrix.Truncate: %d 행으로 줄일 수 없다 (현재 Rows=%d) — Truncate 는 줄이기만 한다", n, m.Rows))
	}
	m.Rows = n
	m.Data = m.Data[:n*m.Cols]
}
