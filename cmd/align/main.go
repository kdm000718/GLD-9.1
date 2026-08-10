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
	// 리베이트 가치는 체결 가격에 따라 달라진다. 봇은 0.5 미만에서만 사므로
	// 0.49 를 대표값으로 둔다. 가정이 드러나도록 상수가 아니라 플래그다.
	fillPrice := flag.Float64("fill-price", 0.49, "리베이트 환산에 쓸 대표 체결가 (0<p<0.5)")
	flag.Parse()

	if *fillPrice <= 0 || *fillPrice >= 0.5 {
		fmt.Fprintf(os.Stderr, "-fill-price 는 0 초과 0.5 미만이어야 한다 (받은 값 %g)\n", *fillPrice)
		os.Exit(2)
	}

	key := os.Getenv("PREDICT_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "PREDICT_API_KEY 환경변수가 필요합니다")
		os.Exit(2)
	}
	if err := run(key, *days, *symbol, *prefix, *fillPrice); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(1)
	}
}

func run(key string, days int, symbol, prefix string, fillPrice float64) error {
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

	st := compare(rounds, byOpen)
	if st.n == 0 {
		return fmt.Errorf("비교 가능한 회차가 없습니다")
	}

	// 구간 안의 5분 슬롯 이론값. 수집 회차가 이 값을 넘으면 5분 아닌 상품이
	// 섞인 것이다 — 그것이 정확히 앞의 두 숫자를 오염시킨 경로다.
	slots := (hi-lo)/300 + 1

	d := float64(st.disagree) / float64(st.n)
	se := math.Sqrt(d * (1 - d) / float64(st.n))
	fmt.Println()
	fmt.Println("==================== G2 정산 정합 ====================")
	fmt.Println("  측정 기준")
	fmt.Printf("    타임프레임    : %s-updown-%s-* 만 (15m 등 다른 상품 제외)\n", prefix, rest.Timeframe)
	fmt.Printf("    구간          : %s ~ %s  (최근 %d일)\n", iso(lo), iso(hi), days)
	fmt.Printf("    5분 슬롯      : 이론값 %d개 / 수집 회차 %d개 / 바이낸스 봉 %d개\n",
		slots, len(rounds), len(ks))
	fmt.Printf("    슬롯 충돌     : %d개  (그중 방향이 서로 다른 슬롯 %d개)\n", st.dupSlot, st.dupConflict)
	fmt.Println()
	fmt.Printf("  비교 슬롯     : %d개  (봉 결측 %d, chainlink 무변동 %d, 바이낸스 도지 %d)\n",
		st.n, st.missing, st.chainFlat, st.binFlat)
	fmt.Printf("  불일치        : %d개\n", st.disagree)
	fmt.Printf("  불일치율 d    : %.4f%%  ±%.4f%%p (1SE)\n", d*100, se*100)
	fmt.Println()

	if st.dupSlot > 0 {
		// 5분 상품만 남긴 뒤로 슬롯 충돌은 일어나지 않아야 한다. 일어났다면
		// 전제가 깨진 것이므로 조용히 넘기지 않는다 — 앞의 두 숫자가 바로
		// 이 충돌을 아무도 보지 않은 채로 발표됐다.
		fmt.Printf("  !!!!! 경고 — 5분 상품만 남겼는데도 슬롯 충돌이 %d건 있다 !!!!!\n", st.dupSlot)
		fmt.Println("        한 슬롯에 회차가 둘 이상이면 이 표본은 신뢰할 수 없다.")
		fmt.Println("        아래 예시의 슬러그를 확인할 것:")
		for _, s := range st.dupSample {
			fmt.Printf("          %s\n", s)
		}
		if st.dupSlot > len(st.dupSample) {
			fmt.Printf("          ... 외 %d건\n", st.dupSlot-len(st.dupSample))
		}
		fmt.Println()
	}

	const baseEdge = 2.270  // %p — 문턱 0.0172 실측 52.270%
	const dojiBias = -0.282 // %p — 도지 제외 낙관 편향

	// 메이커 리베이트는 USDT 가 아니라 **반대편 주식**으로 지급된다
	// (2026-08-09 사용자 확인). 확정된 규칙:
	//   · 0.5% 는 **주식 수** 기준이다 — 명목 USDT 기준이 아니다.
	//   · 체결 즉시 지급된다.
	//   · 받은 주식은 회차 정산 때 함께 정산된다.
	//
	// 그래서 YES 를 가격 p 에 N 주 사면 NO 를 R = 0.005·N 주 받는다. CTF 이진
	// 시장에서 YES+NO=1 이므로 NO 는 우리가 **질 때만** 값이 붙는다. 기대
	// 기여분은 R·(1−q) = 0.005·N·(1−q) 이고, 명목 N·p 대비로 환산하면
	//
	//     0.005 · (1 − q) / p
	//
	// 현금 리베이트 가정(0.5% 를 그대로 더하기)보다 **낮다.** 우리 승률 q 가
	// 시장가 p 가 함축하는 것보다 높기 때문이다 — 엣지가 좋을수록 리베이트를
	// 덜 받는다. q=0.5227, p=0.49 에서 0.487%p 로 현금 가정 0.500 보다 0.013 낮다.
	//
	// 대신 손익과 음의 상관이라 드로다운을 줄이는 부분 헤지로 작동한다.
	// 그 효과는 여기 기대값 표에 안 잡힌다(분산 이야기다).
	const winRate = 0.5227        // 문턱 0.0172 실측 승률. baseEdge 와 같은 출처다.
	const rebateShareFrac = 0.005 // 체결 주식 수의 0.5%
	rebate := 100 * rebateShareFrac * (1 - winRate) / fillPrice

	effective := baseEdge * (1 - 2*d)
	withoutRebate := effective + dojiBias
	total := withoutRebate + rebate

	fmt.Printf("  기대값 분해 (%%p)\n")
	fmt.Printf("    기준 엣지          %+7.3f\n", baseEdge)
	fmt.Printf("    정산 불일치 반영   %+7.3f   (× (1 − 2d))\n", effective-baseEdge)
	fmt.Printf("    도지 제외 편향     %+7.3f\n", dojiBias)
	fmt.Printf("    ─────────────────────────\n")
	fmt.Printf("    리베이트 제외 소계 %+7.3f   ← 실측 기반\n", withoutRebate)
	fmt.Printf("    메이커 리베이트    %+7.3f   ← 반대편 주식 0.5%%(주식 수), 체결가 %.3f 가정\n",
		rebate, fillPrice)
	fmt.Printf("                                 (현금 리베이트였다면 +0.500 — 질 때만 받으므로 낮다)\n")
	fmt.Printf("    ─────────────────────────\n")
	fmt.Printf("    합계               %+7.3f\n", total)
	fmt.Println()
	if total <= 0 {
		fmt.Println("  판정: 실패 — 기대값이 0 이하다. P4 이후를 착수하지 않는다.")
		return nil
	}
	if withoutRebate <= 0 {
		fmt.Println("  판정: 조건부 통과 — 리베이트 가정에 **의존한다.**")
		fmt.Printf("        리베이트를 빼면 %+.3f%%p 로 음수다. Task 10 이 리베이트\n", withoutRebate)
		fmt.Println("        형태를 실측하기 전에는 실거래를 켜지 않는다.")
		return nil
	}
	fmt.Println("  판정: 통과 — 다음 단계로 간다.")
	fmt.Printf("        리베이트를 0 으로 놓아도 %+.3f%%p 라 부호가 안 뒤집힌다.\n", withoutRebate)
	return nil
}

// stats 는 회차·봉 대조 결과다.
type stats struct {
	n           int // 방향을 실제로 비교한 슬롯 수 — 이 값이 표준오차의 분모다
	disagree    int
	chainFlat   int
	binFlat     int
	missing     int
	dupSlot     int
	dupConflict int
	dupSample   []string // 충돌한 슬러그 예시 (최대 dupSampleMax 쌍)
}

// dupSampleMax 는 경고에 찍을 슬롯 충돌 예시의 최대 개수다.
const dupSampleMax = 5

// compare 는 회차를 5분 슬롯에 붙여 방향 불일치를 센다.
//
// 슬롯 충돌은 이제 정상 경로에 없어야 한다. rest 가 슬러그 중복과 타임프레임을
// 둘 다 거르므로, 5분 상품 하나가 한 슬롯을 차지한다. 그래도 세고 예시를
// 남기는 이유는 이것이 조용히 틀리는 방식으로 이미 두 번 당했기 때문이다 —
// 15분 상품이 섞여 들어와 슬롯을 공유했고, n 이 5분 슬롯 수를 넘는데도 아무도
// 알아채지 못했다. 충돌이 하나라도 나오면 호출부가 크게 경고한다.
//
// 충돌 시에는 최신순 첫 회차를 남긴다 — 세는 것을 멈추지 않되, 그 표본이
// 의심스럽다는 사실이 로그에 남아야 한다.
func compare(rounds []rest.Round, byOpen map[int64]klines.Kline) stats {
	var st stats
	type kept struct {
		slug string
		dir  int
	}
	usedSlot := make(map[int64]kept, len(rounds))
	for _, r := range rounds {
		chainUp := sign(r.EndPrice - r.StartPrice)
		if prev, ok := usedSlot[r.StartUnix]; ok {
			st.dupSlot++
			if prev.dir != chainUp {
				// 같은 슬롯의 두 회차가 서로 다른 방향으로 정산됐다.
				// 같은 사건의 중복 반환이 아니라 서로 다른 사건이라는 신호다.
				st.dupConflict++
			}
			if len(st.dupSample) < dupSampleMax {
				st.dupSample = append(st.dupSample,
					fmt.Sprintf("%s: %s (채택) vs %s (버림)", iso(r.StartUnix), prev.slug, r.Slug))
			}
			continue
		}
		usedSlot[r.StartUnix] = kept{slug: r.Slug, dir: chainUp}

		k, ok := byOpen[r.StartUnix]
		if !ok {
			st.missing++
			continue
		}
		binUp := sign(k.Close - k.Open)
		if chainUp == 0 {
			st.chainFlat++
		}
		if binUp == 0 {
			st.binFlat++
		}
		if chainUp == 0 || binUp == 0 {
			continue // 도지·무변동은 방향 비교 대상이 아니다
		}
		st.n++
		if chainUp != binUp {
			st.disagree++
		}
	}
	return st
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
