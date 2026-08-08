// Package vision 은 data.binance.vision 의 월별/일별 ZIP 덤프로 전체 이력을 받는다.
//
// 함정 두 가지:
//  1. 2025 이후 파일은 open_time/close_time 이 마이크로초다. 밀리초로 정규화한다.
//  2. 월별 파일은 완료된 달까지만 있다. 이번 달은 일별 파일로 채운다.
//
// SHA256 체크섬을 항상 검증한다 — 조용히 깨진 데이터로 백테스트하는 것보다 낫다.
package vision

import (
	"archive/zip"
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// MSThreshold 보다 큰 타임스탬프는 마이크로초로 본다.
const MSThreshold = 1e14

// ParseZip 은 ZIP 안의 CSV 를 (n, 11) 배열로 만든다. 타임스탬프는 ms 로 정규화한다.
func ParseZip(raw []byte) ([][11]float64, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("zip 열기 실패: %w", err)
	}
	if len(zr.File) == 0 {
		return nil, fmt.Errorf("zip 이 비어 있다")
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil {
		return nil, err
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n\r"), "\n")
	if len(lines) == 0 || (len(lines) == 1 && strings.TrimSpace(lines[0]) == "") {
		return nil, nil
	}
	// 2025 이후 일부 파일에 헤더가 있다 — 첫 글자가 숫자가 아니면 건너뛴다.
	if c := strings.TrimSpace(lines[0]); c == "" || c[0] < '0' || c[0] > '9' {
		lines = lines[1:]
	}

	out := make([][11]float64, 0, len(lines))
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 11 {
			return nil, fmt.Errorf("%d번째 줄 필드가 %d개뿐이다", i, len(parts))
		}
		var row [11]float64
		for j := 0; j < 11; j++ {
			v, err := strconv.ParseFloat(strings.TrimSpace(parts[j]), 64)
			if err != nil {
				return nil, fmt.Errorf("%d번째 줄 %d번째 필드 파싱 실패: %w", i, j, err)
			}
			row[j] = v
		}
		if row[0] > MSThreshold { // 마이크로초 → 밀리초
			row[0] /= 1000.0
			row[6] = math.Floor(row[6] / 1000.0)
		}
		out = append(out, row)
	}
	return out, nil
}
