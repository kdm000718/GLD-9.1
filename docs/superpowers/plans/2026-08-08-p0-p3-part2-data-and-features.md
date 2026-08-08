# GLD-9.1 P0~P3 계획 2부 — 데이터 로더와 피처

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans. 1부(`2026-08-08-p0-p3-model-port-and-alignment-gate.md`)의 Global Constraints 가 여기에도 그대로 적용된다. Task 번호는 1부에서 이어진다.

**선행 조건:** 1부 Task 1(G2 게이트)이 통과했고 Task 2(bars)가 끝나 있어야 한다.

---

### Task 3: Binance Vision 전체 이력 로더

Python `btc5m/vision.py` 의 포팅이다. 함정이 둘 있고 둘 다 테스트로 고정한다:
2025 이후 파일은 타임스탬프가 **마이크로초**이고, 이번 달은 월별 파일이 없어 **일별
파일**로 채워야 한다.

**Files:**
- Create: `internal/vision/vision.go`, `internal/vision/vision_test.go`

**Interfaces:**
- Consumes: `bars.Bars` (Task 2)
- Produces: `vision.ParseZip(raw []byte) ([][11]float64, error)`, `vision.LoadFullHistory(ctx context.Context, symbol, interval, cacheDir string, log func(string, ...any)) (bars.Bars, error)`, 상수 `vision.MSThreshold = 1e14`

- [ ] **Step 1: 파서 실패 테스트를 쓴다**

`internal/vision/vision_test.go`:

```go
package vision

import (
	"archive/zip"
	"bytes"
	"math"
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
```

- [ ] **Step 2: 테스트 실패 확인**

Run: `go test ./internal/vision/ -v`
Expected: FAIL — `undefined: ParseZip`

- [ ] **Step 3: 파서 구현**

`internal/vision/vision.go`:

```go
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
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/vision/ -v`
Expected: PASS (6개 테스트)

- [ ] **Step 5: 커밋**

```bash
git add internal/vision/
git commit -m "Vision ZIP 파서 — 마이크로초 정규화, 헤더 행 처리"
```

- [ ] **Step 6: 다운로더와 캐시 구현**

`internal/vision/vision.go` 에 추가:

```go
import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/bars"
)

const baseURL = "https://data.binance.vision/data/spot"

// firstYear/firstMonth 는 BTCUSDT 현물 상장 시점이다.
const firstYear, firstMonth = 2017, 8

// get 은 404 면 (nil, nil) — 그 기간 파일이 없다는 뜻이고 오류가 아니다.
func get(ctx context.Context, url string) ([]byte, error) {
	var last error
	for attempt := 0; attempt < 4; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "gld91/0.1")
		resp, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
		if err == nil {
			body, rerr := io.ReadAll(resp.Body)
			code := resp.StatusCode
			resp.Body.Close()
			if code == http.StatusNotFound {
				return nil, nil
			}
			if code == http.StatusOK && rerr == nil {
				return body, nil
			}
			last = fmt.Errorf("HTTP %d", code)
			if rerr != nil {
				last = rerr
			}
		} else {
			last = err
		}
		select {
		case <-time.After(time.Duration(attempt+1) * 1500 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("다운로드 실패 %s: %w", url, last)
}

// fetchChunk 는 월(kind="monthly") 또는 일(kind="daily") 하나를 받아 캐시한다.
func fetchChunk(ctx context.Context, symbol, interval, kind, stamp, cacheDir string) ([][11]float64, error) {
	dir := filepath.Join(cacheDir, "vision", symbol, interval)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cache := filepath.Join(dir, kind+"-"+stamp+".bin")
	if b, err := os.ReadFile(cache); err == nil {
		return decodeCache(b)
	}

	name := fmt.Sprintf("%s-%s-%s.zip", symbol, interval, stamp)
	url := fmt.Sprintf("%s/%s/klines/%s/%s/%s", baseURL, kind, symbol, interval, name)
	raw, err := get(ctx, url)
	if err != nil || raw == nil {
		return nil, err
	}
	chk, err := get(ctx, url+".CHECKSUM")
	if err != nil {
		return nil, err
	}
	if chk != nil {
		want := strings.Fields(string(chk))
		if len(want) > 0 {
			sum := sha256.Sum256(raw)
			if got := hex.EncodeToString(sum[:]); got != want[0] {
				return nil, fmt.Errorf("체크섬 불일치 %s\n  기대 %s\n  실제 %s", name, want[0], got)
			}
		}
	}
	arr, err := ParseZip(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := os.WriteFile(cache, encodeCache(arr), 0o644); err != nil {
		return nil, err
	}
	return arr, nil
}

func encodeCache(arr [][11]float64) []byte {
	buf := make([]byte, 8*11*len(arr))
	for i, row := range arr {
		for j, v := range row {
			binary.LittleEndian.PutUint64(buf[(i*11+j)*8:], math.Float64bits(v))
		}
	}
	return buf
}

func decodeCache(b []byte) ([][11]float64, error) {
	if len(b)%(8*11) != 0 {
		return nil, fmt.Errorf("캐시 파일이 손상됐다 (%d 바이트)", len(b))
	}
	n := len(b) / (8 * 11)
	out := make([][11]float64, n)
	for i := 0; i < n; i++ {
		for j := 0; j < 11; j++ {
			out[i][j] = math.Float64frombits(binary.LittleEndian.Uint64(b[(i*11+j)*8:]))
		}
	}
	return out, nil
}

// LoadFullHistory 는 상장일부터 어제까지 전 구간을 하나의 Bars 로 합친다.
func LoadFullHistory(ctx context.Context, symbol, interval, cacheDir string, log func(string, ...any)) (bars.Bars, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	today := time.Now().UTC()
	var chunks [][11]float64
	var missing []string

	y, m := firstYear, firstMonth
	monthly := 0
	for y < today.Year() || (y == today.Year() && m < int(today.Month())) {
		stamp := fmt.Sprintf("%04d-%02d", y, m)
		arr, err := fetchChunk(ctx, symbol, interval, "monthly", stamp, cacheDir)
		if err != nil {
			return bars.Bars{}, err
		}
		if arr == nil {
			missing = append(missing, stamp)
		} else if len(arr) > 0 {
			chunks = append(chunks, arr...)
			monthly++
			if monthly%24 == 0 {
				log("    ... %s 까지 %d행", stamp, len(chunks))
			}
		}
		m++
		if m == 13 {
			y, m = y+1, 1
		}
	}

	// 이번 달은 일별 파일로 — 오늘 파일은 아직 완성되지 않는다
	daily := 0
	for d := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC); d.Before(today.Truncate(24 * time.Hour)); d = d.AddDate(0, 0, 1) {
		arr, err := fetchChunk(ctx, symbol, interval, "daily", d.Format("2006-01-02"), cacheDir)
		if err != nil {
			return bars.Bars{}, err
		}
		if len(arr) > 0 {
			chunks = append(chunks, arr...)
			daily++
		}
	}

	if len(chunks) == 0 {
		return bars.Bars{}, fmt.Errorf("%s %s: 받은 데이터가 없습니다", symbol, interval)
	}
	if len(missing) > 0 {
		log("    누락된 월: %s", strings.Join(missing, ", "))
	}
	log("    월별 %d개 + 일별 %d개 파일", monthly, daily)

	// 안정 정렬 후 open_time 중복 제거 (먼저 나온 것을 남긴다)
	sort.SliceStable(chunks, func(i, j int) bool { return chunks[i][0] < chunks[j][0] })
	dedup := chunks[:0]
	var prev float64 = -1
	for _, r := range chunks {
		if r[0] == prev {
			continue
		}
		prev = r[0]
		dedup = append(dedup, r)
	}
	return toBars(dedup), nil
}

func toBars(rows [][11]float64) bars.Bars {
	n := len(rows)
	b := bars.Bars{
		OpenTime: make([]int64, n), CloseTime: make([]int64, n),
		Open: make([]float64, n), High: make([]float64, n), Low: make([]float64, n),
		Close: make([]float64, n), Volume: make([]float64, n),
		QuoteVolume: make([]float64, n), Trades: make([]int64, n),
		TakerBuyBase: make([]float64, n), TakerBuyQuote: make([]float64, n),
	}
	for i, r := range rows {
		b.OpenTime[i] = int64(r[0])
		b.Open[i], b.High[i], b.Low[i], b.Close[i] = r[1], r[2], r[3], r[4]
		b.Volume[i] = r[5]
		b.CloseTime[i] = int64(r[6])
		b.QuoteVolume[i] = r[7]
		b.Trades[i] = int64(r[8])
		b.TakerBuyBase[i], b.TakerBuyQuote[i] = r[9], r[10]
	}
	return b
}
```

- [ ] **Step 7: 중복제거 테스트를 추가**

`internal/vision/vision_test.go` 에 추가:

```go
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
```

`vision.go` 에서 정렬·중복제거를 `sortRows`/`dedupRows` 헬퍼로 추출하고
`LoadFullHistory` 가 그것을 호출하게 바꾼다:

```go
func sortRows(rows [][11]float64) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i][0] < rows[j][0] })
}

func dedupRows(rows [][11]float64) [][11]float64 {
	out := rows[:0]
	var prev float64 = -1
	for _, r := range rows {
		if r[0] == prev {
			continue
		}
		prev = r[0]
		out = append(out, r)
	}
	return out
}
```

- [ ] **Step 8: 테스트 통과 확인**

Run: `go test ./internal/vision/ -v && go vet ./...`
Expected: PASS (7개 테스트)

- [ ] **Step 9: 실제 다운로드 한 덩어리로 왕복을 확인한다**

전 구간 다운로드는 Task 12 에서 한다. 여기서는 월 하나만 받아 네트워크·체크섬·
캐시 경로가 실제로 동작하는지 본다. 네트워크가 필요하므로 `-short` 면 건너뛴다.

`internal/vision/vision_net_test.go`:

```go
package vision

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFetchChunkDownloadsAndCaches(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: 네트워크 테스트를 건너뛴다")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	arr, err := fetchChunk(ctx, "BTCUSDT", "5m", "monthly", "2021-06", dir)
	if err != nil {
		t.Fatalf("fetchChunk: %v", err)
	}
	if len(arr) == 0 {
		t.Fatal("2021-06 5분봉이 비어 있다")
	}
	// 6월은 30일 × 288 = 8,640 봉
	if len(arr) != 8640 {
		t.Errorf("봉 %d개, 기대 8640개", len(arr))
	}
	cache := filepath.Join(dir, "vision", "BTCUSDT", "5m", "monthly-2021-06.bin")
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("캐시 파일이 없다: %v", err)
	}

	// 두 번째 호출은 네트워크 없이 캐시에서 와야 하고 값이 같아야 한다
	again, err := fetchChunk(ctx, "BTCUSDT", "5m", "monthly", "2021-06", dir)
	if err != nil {
		t.Fatalf("캐시 재읽기: %v", err)
	}
	if len(again) != len(arr) || again[0] != arr[0] || again[len(again)-1] != arr[len(arr)-1] {
		t.Error("캐시 왕복에서 값이 달라졌다")
	}
}

func TestFetchChunkReturnsNilForMissingPeriod(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: 네트워크 테스트를 건너뛴다")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// 상장 이전이라 404 여야 하고, 404 는 오류가 아니다
	arr, err := fetchChunk(ctx, "BTCUSDT", "5m", "monthly", "2010-01", t.TempDir())
	if err != nil {
		t.Fatalf("404 를 오류로 처리했다: %v", err)
	}
	if arr != nil {
		t.Errorf("없는 기간인데 %d행을 돌려줬다", len(arr))
	}
}
```

Run: `go test ./internal/vision/ -v` (네트워크 포함) 및 `go test -short ./internal/vision/`
Expected: 둘 다 PASS. `-short` 에서는 네트워크 테스트 2개가 skip 된다.

- [ ] **Step 10: 커밋**

```bash
git add internal/vision/
git commit -m "Vision 전체 이력 로더 — SHA256 검증, 월별+일별, 바이너리 캐시"
```

---

### Task 4: 인과적 지표

Python `btc5m/indicators.py` 의 포팅. `scipy.signal.lfilter` 로 하던 재귀 평활은
정의 그대로 단순 루프로 옮긴다 — lfilter 를 쓴 이유는 속도뿐이었다.

**핵심 성질: `f(x)[:m] == f(x[:m])`.** 지표 값이 뒤를 보지 않는다는 것이고, 테스트가
이를 직접 단언한다.

**Files:**
- Create: `internal/indicators/indicators.go`, `internal/indicators/indicators_test.go`

**Interfaces:**
- Produces: `RecursiveSmooth(x []float64, alpha, seed float64, start int) []float64`, `EMA(x []float64, period int) []float64`, `RSI(close []float64, period int) []float64`, `ATR(high, low, close []float64, period int) []float64`, `LogReturn(close []float64, lag int) float64`, `RealizedVol(close []float64, window int) float64`, `ZScore(values []float64, window int) float64`, `StdSample(x []float64) float64`

- [ ] **Step 1: 인과성 테스트를 쓴다**

`internal/indicators/indicators_test.go`:

```go
package indicators

import (
	"math"
	"math/rand"
	"testing"
)

func series(n int, seed int64) []float64 {
	r := rand.New(rand.NewSource(seed))
	out := make([]float64, n)
	p := 100.0
	for i := range out {
		p *= 1 + (r.Float64()-0.5)*0.01
		out[i] = p
	}
	return out
}

// 인과성: 앞부분만 넣고 계산한 값이 전체를 넣고 계산한 앞부분과 같아야 한다.
func TestCausality(t *testing.T) {
	x := series(300, 1)
	hi := make([]float64, len(x))
	lo := make([]float64, len(x))
	for i, v := range x {
		hi[i], lo[i] = v*1.002, v*0.998
	}

	fns := map[string]func(n int) []float64{
		"EMA9":  func(n int) []float64 { return EMA(x[:n], 9) },
		"EMA21": func(n int) []float64 { return EMA(x[:n], 21) },
		"RSI14": func(n int) []float64 { return RSI(x[:n], 14) },
		"ATR14": func(n int) []float64 { return ATR(hi[:n], lo[:n], x[:n], 14) },
	}
	for name, fn := range fns {
		full := fn(len(x))
		for _, m := range []int{120, 200, 260} {
			part := fn(m)
			for i := 0; i < m; i++ {
				a, b := full[i], part[i]
				if math.IsNaN(a) && math.IsNaN(b) {
					continue
				}
				if math.Abs(a-b) > 1e-12 {
					t.Fatalf("%s 가 미래를 본다: m=%d i=%d full=%v part=%v", name, m, i, a, b)
				}
			}
		}
	}
}

func TestEMAWarmupIsNaN(t *testing.T) {
	x := series(50, 2)
	e := EMA(x, 9)
	for i := 0; i < 8; i++ {
		if !math.IsNaN(e[i]) {
			t.Errorf("EMA[%d] = %v, 워밍업 구간은 NaN 이어야 한다", i, e[i])
		}
	}
	if math.IsNaN(e[8]) {
		t.Error("EMA[period-1] 은 시드값이어야 한다")
	}
	// 시드는 앞 period 개의 단순평균
	var s float64
	for i := 0; i < 9; i++ {
		s += x[i]
	}
	if math.Abs(e[8]-s/9) > 1e-12 {
		t.Errorf("EMA 시드 = %v, 기대 %v", e[8], s/9)
	}
}

func TestEMAShorterThanPeriodIsAllNaN(t *testing.T) {
	e := EMA(series(5, 3), 9)
	for i, v := range e {
		if !math.IsNaN(v) {
			t.Errorf("EMA[%d] = %v, 전부 NaN 이어야 한다", i, v)
		}
	}
}

func TestRSIBoundsAndFlatSeries(t *testing.T) {
	x := series(100, 4)
	r := RSI(x, 14)
	for i := 14; i < len(r); i++ {
		if math.IsNaN(r[i]) {
			t.Fatalf("RSI[%d] 가 NaN 이다", i)
		}
		if r[i] < 0 || r[i] > 100 {
			t.Fatalf("RSI[%d] = %v, 0..100 을 벗어났다", i, r[i])
		}
	}
	// 완전히 평평한 시계열: 상승도 하락도 0 → 50
	flat := make([]float64, 100)
	for i := range flat {
		flat[i] = 42.0
	}
	rf := RSI(flat, 14)
	if math.Abs(rf[99]-50.0) > 1e-9 {
		t.Errorf("평평한 시계열 RSI = %v, 기대 50", rf[99])
	}
	// 단조 상승: 하락이 0 → 100
	up := make([]float64, 100)
	for i := range up {
		up[i] = float64(i + 1)
	}
	ru := RSI(up, 14)
	if math.Abs(ru[99]-100.0) > 1e-9 {
		t.Errorf("단조 상승 RSI = %v, 기대 100", ru[99])
	}
}

func TestZScoreExcludesLastValue(t *testing.T) {
	// 마지막 값 자신은 분포에서 빠져야 한다
	x := []float64{1, 1, 1, 1, 1, 5}
	got := ZScore(x, 5)
	if math.IsNaN(got) || math.IsInf(got, 0) {
		// 앞 5개가 전부 같아 표준편차 0 → 0.0 을 돌려주는 규약
		t.Fatalf("ZScore = %v, 표준편차 0 이면 0 이어야 한다", got)
	}
	if got != 0.0 {
		t.Errorf("ZScore = %v, 기대 0 (sd == 0 규약)", got)
	}
}

func TestLogReturnAndRealizedVol(t *testing.T) {
	x := []float64{100, 110}
	if got := LogReturn(x, 1); math.Abs(got-math.Log(1.1)) > 1e-12 {
		t.Errorf("LogReturn = %v, 기대 %v", got, math.Log(1.1))
	}
	if got := LogReturn(x, 5); !math.IsNaN(got) {
		t.Errorf("lag 가 길이보다 큰데 %v 를 돌려줬다", got)
	}
	if got := RealizedVol([]float64{1, 2}, 5); !math.IsNaN(got) {
		t.Errorf("창보다 짧은데 %v 를 돌려줬다", got)
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

Run: `go test ./internal/indicators/ -v`
Expected: FAIL — `undefined: EMA`

- [ ] **Step 3: 구현**

`internal/indicators/indicators.go`:

```go
// Package indicators 는 왼쪽만 보는 인과적 지표를 계산한다.
//
// x[i] 의 값은 오직 x[0..i] 만으로 결정된다. 워밍업이 부족한 앞부분은 NaN 이다.
// (피벗 탐지만 오른쪽 k봉을 쓰는데, 그건 확정 시점 자체가 뒤로 밀리므로
//  미래참조가 아니다 — internal/divergence 참고.)
package indicators

import "math"

func nan() float64 { return math.NaN() }

func filled(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}

// RecursiveSmooth 는 out[start-1]=seed, out[i]=alpha*x[i]+(1-alpha)*out[i-1] 이다.
func RecursiveSmooth(x []float64, alpha, seed float64, start int) []float64 {
	out := filled(len(x))
	if start-1 < 0 || start-1 >= len(x) {
		return out
	}
	out[start-1] = seed
	for i := start; i < len(x); i++ {
		out[i] = alpha*x[i] + (1-alpha)*out[i-1]
	}
	return out
}

// EMA 의 시드는 앞 period 개의 단순평균이다.
func EMA(x []float64, period int) []float64 {
	if len(x) < period || period <= 0 {
		return filled(len(x))
	}
	alpha := 2.0 / (float64(period) + 1.0)
	return RecursiveSmooth(x, alpha, mean(x[:period]), period)
}

// RSI 는 Wilder 방식이다.
func RSI(close []float64, period int) []float64 {
	n := len(close)
	out := filled(n)
	if n <= period || period <= 0 {
		return out
	}
	gain := make([]float64, n-1)
	loss := make([]float64, n-1)
	for i := 1; i < n; i++ {
		d := close[i] - close[i-1]
		if d > 0 {
			gain[i-1] = d
		} else if d < 0 {
			loss[i-1] = -d
		}
	}
	alpha := 1.0 / float64(period)
	ag := RecursiveSmooth(gain, alpha, mean(gain[:period]), period)
	al := RecursiveSmooth(loss, alpha, mean(loss[:period]), period)

	// delta[i] 는 close[i+1] 기준이므로 한 칸 밀어 정렬한다.
	for i := 0; i < n-1; i++ {
		var v float64
		switch {
		case math.IsNaN(ag[i]) || math.IsNaN(al[i]):
			v = math.NaN()
		case al[i] == 0:
			if ag[i] > 0 {
				v = 100.0
			} else {
				v = 50.0
			}
		default:
			v = 100.0 - 100.0/(1.0+ag[i]/al[i])
		}
		out[i+1] = v
	}
	for i := 0; i < period && i < n; i++ {
		out[i] = math.NaN()
	}
	return out
}

// ATR 은 Wilder 평활을 쓴다. 시드는 tr[1..period] 의 평균이다.
func ATR(high, low, close []float64, period int) []float64 {
	n := len(close)
	out := filled(n)
	if n <= period || period <= 0 {
		return out
	}
	tr := make([]float64, n)
	for i := 0; i < n; i++ {
		prev := close[0]
		if i > 0 {
			prev = close[i-1]
		}
		a := high[i] - low[i]
		b := math.Abs(high[i] - prev)
		c := math.Abs(low[i] - prev)
		tr[i] = math.Max(a, math.Max(b, c))
	}
	return RecursiveSmooth(tr, 1.0/float64(period), mean(tr[1:period+1]), period+1)
}

// LogReturn 은 마지막 봉 기준 lag 개 전 대비 로그수익률이다.
func LogReturn(close []float64, lag int) float64 {
	n := len(close)
	if n <= lag || close[n-1-lag] <= 0 {
		return nan()
	}
	return math.Log(close[n-1] / close[n-1-lag])
}

// RealizedVol 은 마지막 window 개 로그수익률의 표본표준편차다.
func RealizedVol(close []float64, window int) float64 {
	if len(close) < window+1 {
		return nan()
	}
	seg := close[len(close)-(window+1):]
	r := make([]float64, len(seg)-1)
	for i := 1; i < len(seg); i++ {
		r[i-1] = math.Log(seg[i] / seg[i-1])
	}
	if len(r) <= 1 {
		return nan()
	}
	return StdSample(r)
}

// ZScore 는 마지막 값의 직전 window 구간 대비 z점수다. 마지막 값 자신은 분포에서 뺀다.
func ZScore(values []float64, window int) float64 {
	if len(values) < window+1 {
		return nan()
	}
	ref := values[len(values)-(window+1) : len(values)-1]
	sd := StdSample(ref)
	if sd <= 0 {
		return 0.0
	}
	return (values[len(values)-1] - mean(ref)) / sd
}

// StdSample 은 ddof=1 표본표준편차다.
func StdSample(x []float64) float64 {
	if len(x) < 2 {
		return nan()
	}
	m := mean(x)
	var s float64
	for _, v := range x {
		d := v - m
		s += d * d
	}
	return math.Sqrt(s / float64(len(x)-1))
}

func mean(x []float64) float64 {
	if len(x) == 0 {
		return nan()
	}
	var s float64
	for _, v := range x {
		s += v
	}
	return s / float64(len(x))
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/indicators/ -v`
Expected: PASS (6개 테스트). 특히 `TestCausality` 가 통과해야 한다.

- [ ] **Step 5: 커밋**

```bash
git add internal/indicators/
git commit -m "인과적 지표 — EMA·RSI·ATR·변동성, 인과성 테스트 포함"
```

---

### Task 5: RSI 다이버전스

Python `btc5m/divergence.py` 의 포팅. 핵심은 **확정 피벗만** 쓰는 것이다 — 인덱스 `i`
가 피벗인지 알려면 `i+k` 봉까지 필요하므로 `i <= n-1-k` 인 것만 인정한다.

NaN 처리가 미묘하다. numpy 는 창에 NaN 이 하나라도 있으면 `max` 가 NaN 이 되고
`center >= NaN` 이 False 라 자동으로 배제됐다. Go 에서는 이를 명시적으로 쓴다.

**Files:**
- Create: `internal/divergence/divergence.go`, `internal/divergence/divergence_test.go`

**Interfaces:**
- Produces: `divergence.Signal{RegularBear, RegularBull, HiddenBear, HiddenBull, BarsSinceHighPivot, BarsSinceLowPivot float64}`, `(Signal).Score() float64`, `FindPivots(values []float64, k int, kind string) []int`, `Detect(high, low, rsiValues []float64, k, minSep, maxSpan int, recencyHalflife float64) Signal`

- [ ] **Step 1: 실패 테스트를 쓴다**

`internal/divergence/divergence_test.go`:

```go
package divergence

import (
	"math"
	"testing"
)

func TestFindPivotsHigh(t *testing.T) {
	//        0  1  2  3  4  5  6  7  8
	v := []float64{1, 2, 5, 2, 1, 2, 6, 2, 1}
	got := FindPivots(v, 2, "high")
	want := []int{2, 6}
	if len(got) != len(want) {
		t.Fatalf("피벗 %v, 기대 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("피벗 %v, 기대 %v", got, want)
		}
	}
}

func TestFindPivotsRejectsUnconfirmedTail(t *testing.T) {
	// 마지막 봉이 최고점이어도 오른쪽 k봉이 없으면 확정 피벗이 아니다
	v := []float64{1, 2, 3, 4, 9}
	for _, i := range FindPivots(v, 2, "high") {
		if i > len(v)-1-2 {
			t.Errorf("미확정 피벗 %d 를 인정했다 (n=%d, k=2)", i, len(v))
		}
	}
}

func TestFindPivotsSkipsWindowsWithNaN(t *testing.T) {
	v := []float64{1, math.NaN(), 5, 2, 1, 2, 6, 2, 1}
	for _, i := range FindPivots(v, 2, "high") {
		if i == 2 {
			t.Error("NaN 이 포함된 창을 피벗으로 인정했다")
		}
	}
}

func TestFindPivotsLow(t *testing.T) {
	v := []float64{9, 8, 1, 8, 9, 8, 0, 8, 9}
	got := FindPivots(v, 2, "low")
	if len(got) != 2 || got[0] != 2 || got[1] != 6 {
		t.Fatalf("저점 피벗 %v, 기대 [2 6]", got)
	}
}

func TestFindPivotsTooShort(t *testing.T) {
	if got := FindPivots([]float64{1, 2, 3}, 2, "high"); len(got) != 0 {
		t.Errorf("길이가 2k+1 미만인데 %v 를 돌려줬다", got)
	}
}

func TestRegularBearDivergence(t *testing.T) {
	// 가격 고점은 올라가는데 RSI 고점은 내려간다 → 정규 하락 다이버전스
	n := 40
	high := make([]float64, n)
	low := make([]float64, n)
	rsi := make([]float64, n)
	for i := range high {
		high[i], low[i], rsi[i] = 100, 90, 50
	}
	high[10], rsi[10] = 120, 80 // 첫 고점
	high[25], rsi[25] = 130, 65 // 더 높은 고점, 더 낮은 RSI
	s := Detect(high, low, rsi, 3, 5, 60, 15.0)
	if s.RegularBear <= 0 {
		t.Fatalf("정규 하락 다이버전스를 못 잡았다: %+v", s)
	}
	if s.Score() >= 0 {
		t.Errorf("Score = %v, 하락 신호이므로 음수여야 한다", s.Score())
	}
}

func TestRegularBullDivergence(t *testing.T) {
	n := 40
	high := make([]float64, n)
	low := make([]float64, n)
	rsi := make([]float64, n)
	for i := range high {
		high[i], low[i], rsi[i] = 100, 90, 50
	}
	low[10], rsi[10] = 80, 20  // 첫 저점
	low[25], rsi[25] = 70, 35  // 더 낮은 저점, 더 높은 RSI
	s := Detect(high, low, rsi, 3, 5, 60, 15.0)
	if s.RegularBull <= 0 {
		t.Fatalf("정규 상승 다이버전스를 못 잡았다: %+v", s)
	}
	if s.Score() <= 0 {
		t.Errorf("Score = %v, 상승 신호이므로 양수여야 한다", s.Score())
	}
}

func TestStrengthAndDecayBounds(t *testing.T) {
	if got := strength(10); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("strength(10) = %v, 기대 0.5", got)
	}
	if got := strength(0); got != 0 {
		t.Errorf("strength(0) = %v, 기대 0", got)
	}
	if got := decay(15, 15); math.Abs(got-0.5) > 1e-12 {
		t.Errorf("반감기만큼 지났으면 0.5 여야 한다: %v", got)
	}
	if got := decay(0, 15); math.Abs(got-1.0) > 1e-12 {
		t.Errorf("decay(0) = %v, 기대 1", got)
	}
}

func TestEmptyInputIsZeroSignal(t *testing.T) {
	s := Detect(nil, nil, nil, 3, 5, 60, 10)
	if s.Score() != 0 {
		t.Errorf("빈 입력인데 Score = %v", s.Score())
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

Run: `go test ./internal/divergence/ -v`
Expected: FAIL — `undefined: FindPivots`

- [ ] **Step 3: 구현**

`internal/divergence/divergence.go`:

```go
// Package divergence 는 가격 스윙과 RSI 스윙을 비교해 다이버전스를 판정한다.
//
// 미래참조에 대한 해명: 스윙 피벗은 좌우 k봉을 비교해 판정하므로 인덱스 i 가
// 피벗인지 알려면 i+k 봉까지 필요하다. 여기서는 확정된 피벗만 쓴다 —
// 가시 구간의 마지막 인덱스를 n-1 이라 할 때 i <= n-1-k 인 것만 인정한다.
// 판정에 쓰인 봉이 전부 가시 구간 안에 있으므로 미래를 보지 않는다.
// 대가는 지연이다: 피벗은 최소 k봉 늦게 확정된다. 실거래와 같은 조건이다.
package divergence

import "math"

// Signal 은 한 타임프레임의 다이버전스 상태다.
type Signal struct {
	RegularBear        float64 // 고점 상승 + RSI 고점 하락 → 하락 신호 강도 (0..1)
	RegularBull        float64 // 저점 하락 + RSI 저점 상승 → 상승 신호 강도 (0..1)
	HiddenBear         float64
	HiddenBull         float64
	BarsSinceHighPivot float64
	BarsSinceLowPivot  float64
}

// Score 는 부호 있는 종합 점수다. + 는 상승, − 는 하락.
func (s Signal) Score() float64 {
	return (s.RegularBull + s.HiddenBull) - (s.RegularBear + s.HiddenBear)
}

// FindPivots 는 확정된 스윙 피벗 인덱스를 오래된 것부터 돌려준다.
// kind 가 "high" 면 좌우 k봉보다 크거나 같은 지점, "low" 면 작거나 같은 지점이다.
func FindPivots(values []float64, k int, kind string) []int {
	n := len(values)
	if k <= 0 || n < 2*k+1 {
		return nil
	}
	var out []int
	for i := k; i <= n-1-k; i++ {
		// 창에 NaN 이 하나라도 있으면 피벗이 아니다.
		// (numpy 는 max 가 NaN 이 되고 비교가 False 라 같은 결과였다.)
		bad := false
		for j := i - k; j <= i+k; j++ {
			if math.IsNaN(values[j]) {
				bad = true
				break
			}
		}
		if bad {
			continue
		}
		c := values[i]
		ok := true
		if kind == "high" {
			for j := i - k; j <= i+k; j++ {
				if values[j] > c {
					ok = false
					break
				}
			}
			ok = ok && c > values[i-1] && c >= values[i+1]
		} else {
			for j := i - k; j <= i+k; j++ {
				if values[j] < c {
					ok = false
					break
				}
			}
			ok = ok && c < values[i-1] && c <= values[i+1]
		}
		if ok {
			out = append(out, i)
		}
	}
	return out
}

// dedupeClose 는 너무 가까이 붙은 피벗 중 최근 것만 남긴다.
func dedupeClose(pivots []int, minSep int) []int {
	var kept []int
	for _, p := range pivots {
		if len(kept) > 0 && p-kept[len(kept)-1] < minSep {
			kept[len(kept)-1] = p
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// Detect 는 마지막 두 개의 확정 피벗을 비교해 다이버전스를 판정한다.
// 강도는 RSI 격차를 0..1 로 스케일하고 피벗이 오래될수록 지수 감쇠시킨다.
func Detect(high, low, rsiValues []float64, k, minSep, maxSpan int, recencyHalflife float64) Signal {
	n := len(high)
	if n == 0 || len(rsiValues) != n || len(low) != n {
		return Signal{}
	}
	var s Signal
	s.BarsSinceHighPivot = math.NaN()
	s.BarsSinceLowPivot = math.NaN()

	hiPiv := dedupeClose(FindPivots(high, k, "high"), minSep)
	if len(hiPiv) >= 2 {
		p1, p2 := hiPiv[len(hiPiv)-2], hiPiv[len(hiPiv)-1]
		s.BarsSinceHighPivot = float64(n - 1 - p2)
		span := p2 - p1
		if span > 0 && span <= maxSpan && !math.IsNaN(rsiValues[p1]) && !math.IsNaN(rsiValues[p2]) {
			dPrice := high[p2] - high[p1]
			dRSI := rsiValues[p2] - rsiValues[p1]
			d := decay(float64(n-1-p2), recencyHalflife)
			switch {
			case dPrice > 0 && dRSI < 0:
				s.RegularBear = strength(math.Abs(dRSI)) * d
			case dPrice < 0 && dRSI > 0:
				s.HiddenBear = strength(math.Abs(dRSI)) * d
			}
		}
	}

	loPiv := dedupeClose(FindPivots(low, k, "low"), minSep)
	if len(loPiv) >= 2 {
		q1, q2 := loPiv[len(loPiv)-2], loPiv[len(loPiv)-1]
		s.BarsSinceLowPivot = float64(n - 1 - q2)
		span := q2 - q1
		if span > 0 && span <= maxSpan && !math.IsNaN(rsiValues[q1]) && !math.IsNaN(rsiValues[q2]) {
			dPrice := low[q2] - low[q1]
			dRSI := rsiValues[q2] - rsiValues[q1]
			d := decay(float64(n-1-q2), recencyHalflife)
			switch {
			case dPrice < 0 && dRSI > 0:
				s.RegularBull = strength(math.Abs(dRSI)) * d
			case dPrice > 0 && dRSI < 0:
				s.HiddenBull = strength(math.Abs(dRSI)) * d
			}
		}
	}
	return s
}

// strength 는 RSI 격차를 0..1 강도로 만든다. 10포인트 차이에서 0.5 다.
func strength(rsiGap float64) float64 { return rsiGap / (rsiGap + 10.0) }

func decay(ageBars, halflife float64) float64 {
	if halflife <= 0 {
		return 1.0
	}
	return math.Exp(-math.Ln2 * ageBars / halflife)
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/divergence/ -v`
Expected: PASS (9개 테스트)

- [ ] **Step 5: 커밋**

```bash
git add internal/divergence/
git commit -m "RSI 다이버전스 — 확정 피벗만 사용, NaN 창 배제"
```

---

### Task 6: MarketView — 미래참조 물리 차단

이 패키지가 틀리면 백테스트 전체가 무의미해진다. Python `btc5m/clock.py` 의 포팅이다.

**Files:**
- Create: `internal/clock/clock.go`, `internal/clock/clock_test.go`

**Interfaces:**
- Consumes: `bars.Bars` (Task 2)
- Produces: `clock.LookaheadError` (error 타입), `clock.MarketView{T, CandleStart int64, Bars1m, Bars5m bars.Bars}`, `clock.New(t int64, b1, b5 bars.Bars, candleStart int64) (*MarketView, error)`, `(*MarketView).ElapsedMin() int`, `(*MarketView).LastPrice() float64`

- [ ] **Step 1: 실패 테스트를 쓴다**

`internal/clock/clock_test.go`:

```go
package clock

import (
	"errors"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/bars"
)

func mk(n int, stepMS int64) bars.Bars {
	b := bars.Bars{
		OpenTime: make([]int64, n), CloseTime: make([]int64, n),
		Open: make([]float64, n), High: make([]float64, n), Low: make([]float64, n),
		Close: make([]float64, n), Volume: make([]float64, n),
		QuoteVolume: make([]float64, n), Trades: make([]int64, n),
		TakerBuyBase: make([]float64, n), TakerBuyQuote: make([]float64, n),
	}
	for i := 0; i < n; i++ {
		b.OpenTime[i] = int64(i) * stepMS
		b.CloseTime[i] = b.OpenTime[i] + stepMS - 1
		b.Close[i] = float64(i)
	}
	return b
}

func TestCutRemovesEverythingAtOrAfterT(t *testing.T) {
	b1, b5 := mk(600, 60_000), mk(120, 300_000)
	target := int64(300) * 60_000 // 1분봉 300번째의 시작
	v, err := New(target, b1, b5, target)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n := v.Bars1m.Len(); n != 300 {
		t.Errorf("1분봉 %d개, 기대 300개", n)
	}
	last := v.Bars1m.CloseTime[v.Bars1m.Len()-1]
	if last >= target {
		t.Errorf("마지막 close_time %d 가 t=%d 이상이다 — 미래가 남았다", last, target)
	}
	for i := 0; i < v.Bars5m.Len(); i++ {
		if v.Bars5m.CloseTime[i] >= target {
			t.Fatalf("5분봉 %d 의 close_time 이 t 이상이다", i)
		}
	}
}

func TestTargetCandleIsNeverVisible(t *testing.T) {
	// 대상 5분봉 자신은 close_time = candle_start+299999 이므로 k<=4 면 안 보인다
	b1, b5 := mk(600, 60_000), mk(120, 300_000)
	cs := int64(60) * 300_000
	for k := int64(0); k <= 4; k++ {
		v, err := New(cs+k*60_000, b1, b5, cs)
		if err != nil {
			t.Fatalf("k=%d New: %v", k, err)
		}
		if idx := v.Bars5m.IndexOfOpenTime(cs); idx >= 0 {
			t.Errorf("k=%d 인데 대상 5분봉이 보인다 (idx=%d)", k, idx)
		}
		if got := v.ElapsedMin(); got != int(k) {
			t.Errorf("ElapsedMin = %d, 기대 %d", got, k)
		}
	}
}

func TestElapsedOutOfRangeIsError(t *testing.T) {
	b1, b5 := mk(600, 60_000), mk(120, 300_000)
	cs := int64(60) * 300_000
	if _, err := New(cs+300_000, b1, b5, cs); err == nil {
		t.Error("경과 5분인데 에러가 없다")
	}
	if _, err := New(cs-1, b1, b5, cs); err == nil {
		t.Error("t 가 candle_start 보다 이른데 에러가 없다")
	}
}

func TestNoVisibleBarsIsLookaheadError(t *testing.T) {
	b1, b5 := mk(600, 60_000), mk(120, 300_000)
	var le *LookaheadError
	_, err := New(0, b1, b5, 0)
	if err == nil {
		t.Fatal("t=0 이면 볼 수 있는 봉이 없는데 에러가 없다")
	}
	if !errors.As(err, &le) {
		t.Errorf("에러 타입이 LookaheadError 가 아니다: %T", err)
	}
}

func TestLastPriceIsLastClosedBar(t *testing.T) {
	b1, b5 := mk(600, 60_000), mk(120, 300_000)
	target := int64(300) * 60_000
	v, _ := New(target, b1, b5, target)
	if got := v.LastPrice(); got != 299 {
		t.Errorf("LastPrice = %v, 기대 299 (299번 봉의 종가)", got)
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

Run: `go test ./internal/clock/ -v`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: 구현**

`internal/clock/clock.go`:

```go
// Package clock 은 미래참조 편향을 구조적으로 차단한다.
//
// 핵심 규칙: 의사결정 시각 t 에서 볼 수 있는 것은 close_time < t 인 봉뿐이다.
//
// MarketView 는 생성 시점에 미래 구간을 물리적으로 잘라내고, 잘라낸 결과에
// 미래 봉이 하나도 없음을 확인한다. 따라서 피처 코드가 실수로 미래를
// 참조하려 해도 그 데이터가 객체 안에 아예 없다.
//
// 봉 중간 예측: 대상 5분봉의 시작을 candleStart, 의사결정 시각을 t 라 하면
// t = candleStart + k분 (k=0..4) 이다. k>=1 이면 그 5분봉 안에서 이미 마감된
// 1분봉 k개가 보인다. 반면 5분봉 자신은 close_time = candleStart+299999 이므로
// k<=4 인 한 절대 보이지 않는다 — 같은 절단 규칙이 그대로 적용된다.
package clock

import (
	"fmt"
	"sort"

	"github.com/kdm000718/GLD-9.1/internal/bars"
)

// LookaheadError 는 미래 데이터 접근이 감지되었을 때다.
type LookaheadError struct{ Msg string }

func (e *LookaheadError) Error() string { return e.Msg }

// MarketView 는 시각 t 에서 관측 가능한 시장 상태다.
type MarketView struct {
	T           int64 // 의사결정 시각 (ms)
	CandleStart int64 // 대상 5분봉의 시작 (ms)
	Bars1m      bars.Bars
	Bars5m      bars.Bars
}

// New 는 t 이후의 데이터를 잘라낸 뷰를 만든다.
func New(t int64, b1, b5 bars.Bars, candleStart int64) (*MarketView, error) {
	if d := t - candleStart; d < 0 || d >= 300_000 {
		return nil, &LookaheadError{fmt.Sprintf(
			"의사결정 시각이 대상 5분봉 밖입니다: t=%d, candleStart=%d", t, candleStart)}
	}
	c1, err := cut(b1, t, "1m")
	if err != nil {
		return nil, err
	}
	c5, err := cut(b5, t, "5m")
	if err != nil {
		return nil, err
	}
	return &MarketView{T: t, CandleStart: candleStart, Bars1m: c1, Bars5m: c5}, nil
}

// ElapsedMin 은 대상 5분봉이 시작된 뒤 경과한 분(0..4)이다.
func (v *MarketView) ElapsedMin() int { return int((v.T - v.CandleStart) / 60_000) }

// LastPrice 는 t 직전에 마감된 1분봉의 종가다.
func (v *MarketView) LastPrice() float64 { return v.Bars1m.Close[v.Bars1m.Len()-1] }

// cut 은 close_time < t 인 봉만 남긴다.
func cut(b bars.Bars, t int64, label string) (bars.Bars, error) {
	// close_time 은 단조증가 → 이진탐색으로 close_time >= t 인 첫 인덱스를 찾는다.
	hi := sort.Search(b.Len(), func(i int) bool { return b.CloseTime[i] >= t })
	c := b.Slice(0, hi)
	if c.Len() > 0 && c.CloseTime[c.Len()-1] >= t {
		return bars.Bars{}, &LookaheadError{fmt.Sprintf(
			"%s: 절단 실패 — close_time=%d >= t=%d", label, c.CloseTime[c.Len()-1], t)}
	}
	if c.Len() == 0 {
		return bars.Bars{}, &LookaheadError{fmt.Sprintf("%s: t=%d 이전에 마감된 봉이 없습니다", label, t)}
	}
	return c, nil
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/clock/ -v`
Expected: PASS (5개 테스트)

- [ ] **Step 5: 커밋**

```bash
git add internal/clock/
git commit -m "MarketView — close_time >= t 물리 절단으로 미래참조 차단"
```

---

계획이 길어 Task 7(features)·Task 8(G1 골든 벡터)과 Task 9~12 는 3부
`2026-08-08-p0-p3-part3-features-and-model.md` 에 있다.
