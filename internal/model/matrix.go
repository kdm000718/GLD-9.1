package model

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

func (m *Matrix) Row(i int) []float32 { return m.Data[i*m.Cols : (i+1)*m.Cols] }

func (m *Matrix) SetRow(i int, v []float64) {
	dst := m.Row(i)
	for j := range dst {
		dst[j] = float32(v[j])
	}
}

// Truncate 는 앞 n행만 남긴다. 표본을 미리 크게 잡고 실제 개수만큼 줄일 때 쓴다.
func (m *Matrix) Truncate(n int) {
	m.Rows = n
	m.Data = m.Data[:n*m.Cols]
}
