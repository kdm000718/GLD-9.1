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
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/bars"
)

// MSThreshold 보다 큰 타임스탬프는 마이크로초로 본다.
const MSThreshold = 1e14

const baseURL = "https://data.binance.vision/data/spot"

// firstYear/firstMonth 는 BTCUSDT 현물 상장 시점이다.
const firstYear, firstMonth = 2017, 8

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
	if err := writeCacheAtomic(cache, encodeCache(arr)); err != nil {
		return nil, err
	}
	return arr, nil
}

// cacheMagic 뒤에 int64 행 수가 온다. 헤더가 있어야 잘린 파일을 알아본다 —
// 88 배수 검사만으로는 행 하나가 통째로 날아간 파일을 구분하지 못한다.
const cacheMagic = "GLD9BIN1"

func encodeCache(arr [][11]float64) []byte {
	out := make([]byte, 0, len(cacheMagic)+8+len(arr)*88)
	out = append(out, cacheMagic...)
	var hdr [8]byte
	binary.LittleEndian.PutUint64(hdr[:], uint64(len(arr)))
	out = append(out, hdr[:]...)
	var buf [8]byte
	for _, r := range arr {
		for _, v := range r {
			binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
			out = append(out, buf[:]...)
		}
	}
	return out
}

func decodeCache(b []byte) ([][11]float64, error) {
	const head = len(cacheMagic) + 8
	if len(b) < head || string(b[:len(cacheMagic)]) != cacheMagic {
		return nil, fmt.Errorf("캐시 형식이 아니다 (%d바이트)", len(b))
	}
	n := int(binary.LittleEndian.Uint64(b[len(cacheMagic):head]))
	want := head + n*88
	if len(b) != want {
		return nil, fmt.Errorf("캐시가 잘렸다: %d바이트, 헤더가 말하는 %d행이면 %d바이트여야 한다",
			len(b), n, want)
	}
	out := make([][11]float64, n)
	off := head
	for i := 0; i < n; i++ {
		for j := 0; j < 11; j++ {
			out[i][j] = math.Float64frombits(binary.LittleEndian.Uint64(b[off:]))
			off += 8
		}
	}
	return out, nil
}

// writeCacheAtomic 은 같은 디렉터리에 임시 파일로 쓴 뒤 rename 한다.
// rename 은 같은 파일시스템 안에서 원자적이므로, 쓰기 도중 죽어도 목적지에는
// 온전한 파일이거나 아무것도 없거나 둘 중 하나만 남는다.
func writeCacheAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // rename 이 성공하면 대상이 없으므로 무해하다
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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
	sortRows(chunks)
	dedup := dedupRows(chunks)
	out := toBars(dedup)

	// close_time 이상 스캔 — 보고만 하고 데이터는 그대로 둔다.
	// clock.cut 의 이진탐색이 close_time 단조성을 전제하므로, 실행할 때마다
	// 그 전제가 깨지는 지점을 드러낸다.
	if an := bars.FindAnomalies(out); len(an) > 0 {
		log("    [주의] 타임스탬프 이상 %d건 — 데이터는 그대로 둔다", len(an))
		for i, a := range an {
			if i >= 5 {
				log("    ... 외 %d건", len(an)-5)
				break
			}
			log("      idx %d %s: %s", a.Index, a.Kind, a.Msg)
		}
	}

	return out, nil
}

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
