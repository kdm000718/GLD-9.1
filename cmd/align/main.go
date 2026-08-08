// Command align 은 predict.fun 정산 방향과 바이낸스 5분봉 방향의 불일치율을 잰다.
// 이것이 판정 게이트 G2 다. 결과가 나쁘면 나머지 구현을 착수하지 않는다.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/klines"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

func main() {
	days := flag.Int("days", 30, "몇 일치 회차를 검사할지")
	symbol := flag.String("symbol", "BTCUSDT", "바이낸스 심볼")
	prefix := flag.String("prefix", "btc", "회차 슬러그 접두사")
	flag.Parse()

	key := os.Getenv("PREDICT_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "PREDICT_API_KEY 환경변수가 필요합니다")
		os.Exit(2)
	}
	if err := run(key, *days, *symbol, *prefix); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(1)
	}
}

func run(key string, days int, symbol, prefix string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	since := time.Now().Unix() - int64(days)*86400
	fmt.Printf("[수집] predict.fun 정산 회차 (최근 %d일)\n", days)
	rounds, err := rest.FetchResolvedRounds(ctx, rest.New(key), prefix, since)
	if err != nil {
		return err
	}
	if len(rounds) == 0 {
		return fmt.Errorf("회차를 하나도 못 받았습니다")
	}
	lo, hi := rounds[len(rounds)-1].StartUnix, rounds[0].StartUnix
	fmt.Printf("  회차 %d개  %s ~ %s\n", len(rounds), iso(lo), iso(hi))

	fmt.Printf("[수집] 바이낸스 %s 5분봉\n", symbol)
	ks, err := klines.Fetch(ctx, symbol, "5m", lo*1000, (hi+300)*1000)
	if err != nil {
		return err
	}
	byOpen := make(map[int64]klines.Kline, len(ks))
	for _, k := range ks {
		byOpen[k.OpenTime/1000] = k
	}
	fmt.Printf("  봉 %d개\n", len(ks))

	var n, disagree, chainFlat, binFlat, missing int
	for _, r := range rounds {
		k, ok := byOpen[r.StartUnix]
		if !ok {
			missing++
			continue
		}
		chainUp := sign(r.EndPrice - r.StartPrice)
		binUp := sign(k.Close - k.Open)
		if chainUp == 0 {
			chainFlat++
		}
		if binUp == 0 {
			binFlat++
		}
		if chainUp == 0 || binUp == 0 {
			continue // 도지·무변동은 방향 비교 대상이 아니다
		}
		n++
		if chainUp != binUp {
			disagree++
		}
	}
	if n == 0 {
		return fmt.Errorf("비교 가능한 회차가 없습니다")
	}

	d := float64(disagree) / float64(n)
	se := math.Sqrt(d * (1 - d) / float64(n))
	fmt.Println()
	fmt.Println("==================== G2 정산 정합 ====================")
	fmt.Printf("  비교 회차     : %d개  (봉 결측 %d, chainlink 무변동 %d, 바이낸스 도지 %d)\n",
		n, missing, chainFlat, binFlat)
	fmt.Printf("  불일치        : %d개\n", disagree)
	fmt.Printf("  불일치율 d    : %.4f%%  ±%.4f%%p (1SE)\n", d*100, se*100)
	fmt.Println()

	const baseEdge = 2.270  // %p — 문턱 0.0172 실측 52.270%
	const dojiBias = -0.282 // %p — 도지 제외 낙관 편향
	const rebate = 0.500    // %p — 메이커 리베이트
	effective := baseEdge * (1 - 2*d)
	total := effective + dojiBias + rebate

	fmt.Printf("  기대값 분해 (%%p)\n")
	fmt.Printf("    기준 엣지          %+7.3f\n", baseEdge)
	fmt.Printf("    정산 불일치 반영   %+7.3f   (× (1 − 2d))\n", effective-baseEdge)
	fmt.Printf("    도지 제외 편향     %+7.3f\n", dojiBias)
	fmt.Printf("    메이커 리베이트    %+7.3f\n", rebate)
	fmt.Printf("    ─────────────────────────\n")
	fmt.Printf("    합계               %+7.3f\n", total)
	fmt.Println()
	if total <= 0 {
		fmt.Println("  판정: 실패 — 기대값이 0 이하다. P4 이후를 착수하지 않는다.")
		return nil
	}
	fmt.Println("  판정: 통과 — 다음 단계로 간다.")
	return nil
}

func sign(x float64) int {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}

func iso(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04 UTC")
}
