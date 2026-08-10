package vision

import (
	"archive/zip"
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"
)

const msRow = "1622505600000,37253.82,37260.00,37100.00,37124.60,110.29,1622505659999,4101355.91,3386,32.16,1195493.64,0"
const usRow = "1780272000000000,73674.39,73685.50,73654.06,73684.43,3.76,1780272059999999,277179.25,2892,2.24,165600.35,0"
const header = "open_time,open,high,low,close,volume,close_time,quote_volume,count,taker_buy_volume,taker_buy_quote_volume,ignore"

func zipOf(text string) []byte {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	f, _ := w.Create("x.csv")
	f.Write([]byte(text))
	w.Close()
	return buf.Bytes()
}

func TestMillisecondFileIsLeftAlone(t *testing.T) {
	arr, err := ParseZip(zipOf(msRow + "\n"))
	if err != nil {
		t.Fatalf("ParseZip: %v", err)
	}
	if arr[0][0] != 1622505600000 || arr[0][6] != 1622505659999 {
		t.Errorf("ms 파일이 변형됐다: %v %v", arr[0][0], arr[0][6])
	}
}

func TestMicrosecondFileIsNormalisedToMilliseconds(t *testing.T) {
	arr, err := ParseZip(zipOf(usRow + "\n"))
	if err != nil {
		t.Fatalf("ParseZip: %v", err)
	}
	if arr[0][0] != 1780272000000 {
		t.Errorf("open_time = %v, 기대 1780272000000", arr[0][0])
	}
	// close_time 은 봉의 마지막 순간이므로 내림해야 다음 봉과 겹치지 않는다
	if arr[0][6] != 1780272059999 {
		t.Errorf("close_time = %v, 기대 1780272059999", arr[0][6])
	}
	if arr[0][6] >= arr[0][0]+60000 {
		t.Error("close_time 이 다음 봉을 침범한다")
	}
}

func TestHeaderRowIsSkipped(t *testing.T) {
	arr, err := ParseZip(zipOf(header + "\n" + msRow + "\n"))
	if err != nil {
		t.Fatalf("ParseZip: %v", err)
	}
	if len(arr) != 1 || arr[0][0] != 1622505600000 {
		t.Errorf("헤더 처리 실패: %d행 %v", len(arr), arr)
	}
}

func TestPricesSurviveTheTimestampConversion(t *testing.T) {
	arr, _ := ParseZip(zipOf(usRow + "\n"))
	want := []float64{73674.39, 73685.50, 73654.06, 73684.43, 3.76}
	for i, w := range want {
		if math.Abs(arr[0][i+1]-w) > 1e-9 {
			t.Errorf("필드 %d = %v, 기대 %v", i+1, arr[0][i+1], w)
		}
	}
}

func TestNormalisedTimestampsAreBelowThreshold(t *testing.T) {
	for _, row := range []string{msRow, usRow} {
		arr, _ := ParseZip(zipOf(row + "\n"))
		if arr[0][0] >= MSThreshold {
			t.Errorf("정규화 후에도 임계 초과: %v", arr[0][0])
		}
	}
}

func TestEmptyFileIsNotAnError(t *testing.T) {
	arr, err := ParseZip(zipOf(""))
	if err != nil {
		t.Fatalf("빈 파일에서 에러: %v", err)
	}
	if len(arr) != 0 {
		t.Errorf("빈 파일인데 %d행", len(arr))
	}
}

func TestToBarsAndDedup(t *testing.T) {
	// LoadFullHistory 의 정렬·중복제거 로직을 toBars 앞단까지 흉내낸다
	rows := [][11]float64{
		{2000, 2, 2, 2, 2, 1, 2059, 1, 1, 1, 1},
		{1000, 1, 1, 1, 1, 1, 1059, 1, 1, 1, 1},
		{2000, 9, 9, 9, 9, 9, 2059, 9, 9, 9, 9}, // 중복 open_time
	}
	sortRows(rows)
	rows = dedupRows(rows)
	if len(rows) != 2 {
		t.Fatalf("중복 제거 후 %d행, 기대 2행", len(rows))
	}
	b := toBars(rows)
	if b.OpenTime[0] != 1000 || b.OpenTime[1] != 2000 {
		t.Errorf("정렬 실패: %v", b.OpenTime)
	}
	if b.Open[1] != 2 {
		t.Errorf("중복 제거가 먼저 나온 행을 남기지 않았다: %v", b.Open[1])
	}
}

func TestDecodeCacheRejectsTruncatedFile(t *testing.T) {
	rows := [][11]float64{{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, {12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22}}
	full := encodeCache(rows)
	// 한 행을 통째로 잘라낸다. 길이는 여전히 88 의 배수라 기존 검사를 통과한다.
	short := full[:len(full)-88]
	got, err := decodeCache(short)
	if err != nil {
		return // 길이 헤더가 있으면 여기서 걸린다 — 원하는 동작
	}
	if len(got) != len(rows) {
		t.Fatalf("잘린 캐시를 %d행으로 읽었다(원본 %d행) — 에러 없이 통과했다", len(got), len(rows))
	}
}

func TestWriteCacheAtomicLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monthly-2020-01.bin")
	rows := [][11]float64{{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}}
	if err := writeCacheAtomic(path, encodeCache(rows)); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "monthly-2020-01.bin" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("임시 파일이 남았다: %v", names)
	}
}
