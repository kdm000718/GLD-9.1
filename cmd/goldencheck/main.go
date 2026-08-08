// Command goldencheck 는 Go 피처가 Python 골든 벡터와 일치하는지 판정한다 (게이트 G1).
//
// 골든 벡터에는 두 종류의 줄이 있다.
//   - 성공 표본: Python 이 만든 60개 값. Go 도 같은 값을 내야 한다.
//   - 거부 표본: Python 의 build() 가 None 을 돌려준 시점. Go 도 거부해야 한다.
//     값 대조만으로는 결측 가드의 어긋남이 드러나지 않기 때문에 따로 기록해 둔 것이다.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"

	"github.com/kdm000718/GLD-9.1/internal/clock"
	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/vision"
)

type golden struct {
	CandleStart int64     `json:"candle_start"`
	T           int64     `json:"t"`
	K           int       `json:"k"`
	Rejected    bool      `json:"rejected"`
	Values      []float64 `json:"values"`
}

func main() {
	path := flag.String("golden", "testdata/golden_features.jsonl", "골든 벡터 파일")
	cache := flag.String("cache", "data", "Vision 캐시 디렉터리")
	tol := flag.Float64("tol", 1e-9, "허용오차")
	flag.Parse()

	if err := run(*path, *cache, *tol); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(1)
	}
}

func run(path, cache string, tol float64) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	var rows []golden
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 1<<20), 1<<22)
	for sc.Scan() {
		var g golden
		if err := json.Unmarshal(sc.Bytes(), &g); err != nil {
			return err
		}
		rows = append(rows, g)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	fmt.Printf("[골든] %d개 시점\n", len(rows))

	ctx := context.Background()
	fmt.Println("[데이터] Binance Vision 로드")
	b1, err := vision.LoadFullHistory(ctx, "BTCUSDT", "1m", cache, nil)
	if err != nil {
		return err
	}
	b5, err := vision.LoadFullHistory(ctx, "BTCUSDT", "5m", cache, nil)
	if err != nil {
		return err
	}
	fmt.Printf("  1분봉 %d / 5분봉 %d\n", b1.Len(), b5.Len())

	var checked, mismatch, buildFail int
	var rejAgree, rejDisagree int
	byK := map[int]int{}
	worst := 0.0
	worstName := ""
	// 피처별 최대 절대차. 불일치가 나면 어느 피처인지 이름으로 짚어준다.
	worstBy := make([]float64, len(features.FeatureNames))

	for _, g := range rows {
		v, err := clock.New(g.T, b1, b5, g.CandleStart)
		if err != nil {
			// clock.New 가 거부하면 Build 까지 가지 못한다. 거부 표본이면 그것도 거부이므로
			// 일치로 센다. 성공 표본이면 실패다.
			if g.Rejected {
				rejAgree++
			} else {
				buildFail++
				fmt.Printf("  clock.New 실패 cs=%d k=%d: %v\n", g.CandleStart, g.K, err)
			}
			continue
		}
		got, ok := features.Build(v)

		if g.Rejected {
			if ok {
				rejDisagree++
				fmt.Printf("  거부 불일치 cs=%d k=%d — Python 은 거부, Go 는 수용\n",
					g.CandleStart, g.K)
			} else {
				rejAgree++
			}
			continue
		}
		if !ok {
			buildFail++
			fmt.Printf("  Build 실패 cs=%d k=%d — Python 은 성공했다\n", g.CandleStart, g.K)
			continue
		}
		if len(g.Values) != len(got) {
			return fmt.Errorf("피처 개수 불일치: 골든 %d vs Go %d", len(g.Values), len(got))
		}
		checked++
		byK[g.K]++
		bad := false
		for i := range got {
			d := math.Abs(got[i] - g.Values[i])
			if math.IsNaN(got[i]) != math.IsNaN(g.Values[i]) {
				d = math.Inf(1)
			}
			if d > worstBy[i] {
				worstBy[i] = d
			}
			if d > worst {
				worst, worstName = d, features.FeatureNames[i]
			}
			if d > tol {
				bad = true
			}
		}
		if bad {
			mismatch++
			if mismatch <= 3 {
				fmt.Printf("  불일치 cs=%d k=%d\n", g.CandleStart, g.K)
				for i := range got {
					if d := math.Abs(got[i] - g.Values[i]); d > tol {
						fmt.Printf("    %-20s go=%+.12g py=%+.12g  차이 %.3g\n",
							features.FeatureNames[i], got[i], g.Values[i], d)
					}
				}
			}
		}
	}

	ks := make([]int, 0, len(byK))
	for k := range byK {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	dist := ""
	for _, k := range ks {
		dist += fmt.Sprintf("k=%d:%d  ", k, byK[k])
	}

	fmt.Println()
	fmt.Println("==================== G1 피처 동등성 ====================")
	fmt.Printf("  대조 %d개 (Build 실패 %d개)\n", checked, buildFail)
	fmt.Printf("  k 분포 %s\n", dist)
	fmt.Printf("  거부 대조 일치 %d / 불일치 %d\n", rejAgree, rejDisagree)
	fmt.Printf("  불일치 %d개\n", mismatch)
	fmt.Printf("  최대 절대차 %.3g (%s), 허용 %.3g\n", worst, worstName, tol)
	if mismatch > 0 {
		return fmt.Errorf("G1 실패 — Task 7 로 돌아간다")
	}
	if buildFail > 0 {
		return fmt.Errorf("G1 실패 — Python 이 성공한 시점에서 Go 의 Build 가 %d개 실패했다", buildFail)
	}
	if rejDisagree > 0 {
		return fmt.Errorf("G1 실패 — 결측 가드가 어긋난다 (%d개)", rejDisagree)
	}
	if checked == 0 {
		return fmt.Errorf("대조한 시점이 하나도 없다")
	}
	// k>=1 표본이 실제로 대조됐는지 확인한다. 이 게이트를 넓힌 이유가 그것이다.
	partial := 0
	for k, n := range byK {
		if k >= 1 {
			partial += n
		}
	}
	if partial == 0 {
		return fmt.Errorf("k>=1 표본이 하나도 대조되지 않았다 — 골든 파일이 잘못됐다")
	}
	fmt.Println("  판정: 통과")
	return nil
}
