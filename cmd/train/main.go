// Command train 은 Vision 전체 이력으로 학습해 models.json 을 만든다.
//
// 워크포워드를 먼저 돌려 성능을 확인하고, 그다음 가장 최근 train-days 창으로
// 학습한 모델 하나를 저장한다. 워크포워드 모델들은 블록마다 다르므로 저장 대상이
// 아니다 — 실거래에 필요한 것은 최신 데이터까지 반영한 단일 모델이다.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/metrics"
	"github.com/kdm000718/GLD-9.1/internal/model"
	"github.com/kdm000718/GLD-9.1/internal/sample"
	"github.com/kdm000718/GLD-9.1/internal/vision"
	"github.com/kdm000718/GLD-9.1/internal/walkforward"
)

const fiveMS = walkforward.FiveMinMS

func main() {
	symbol := flag.String("symbol", "BTCUSDT", "심볼")
	cache := flag.String("cache", "data", "Vision 캐시 디렉터리")
	trainDays := flag.Float64("train-days", 180, "학습 창 (일)")
	refitDays := flag.Float64("refit-days", 30, "워크포워드 재학습 주기 (일)")
	l2 := flag.Float64("l2", 10, "L2 세기")
	out := flag.String("out", "models.json", "출력 경로")
	skipWF := flag.Bool("skip-walkforward", false, "워크포워드 검증을 건너뛴다")
	flag.Parse()

	if *trainDays <= 0 || *refitDays <= 0 {
		fmt.Fprintf(os.Stderr, "실패: -train-days 와 -refit-days 는 0 보다 커야 한다\n")
		os.Exit(2)
	}
	if err := run(*symbol, *cache, *trainDays, *refitDays, *l2, *out, *skipWF); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(1)
	}
}

func run(symbol, cache string, trainDays, refitDays, l2 float64, out string, skipWF bool) error {
	ctx := context.Background()
	t0 := time.Now()

	fmt.Println("[데이터] Binance Vision 수집")
	b1, err := vision.LoadFullHistory(ctx, symbol, "1m", cache, logf)
	if err != nil {
		return err
	}
	b5, err := vision.LoadFullHistory(ctx, symbol, "5m", cache, logf)
	if err != nil {
		return err
	}
	fmt.Printf("  1분봉 %d / 5분봉 %d  (%.0fs)\n", b1.Len(), b5.Len(), time.Since(t0).Seconds())

	fmt.Println("\n[표본] +0분 피처 생성")
	tSample := time.Now()
	cs, mat, y, counts := sample.Build(b1, b5, func(kept int) {
		fmt.Printf("    ... %d개 (%.0fs)\n", kept, time.Since(tSample).Seconds())
	})
	fmt.Printf("    표본 %d개  제외: 도지 %d / 워밍업 %d / 결측 %d  (%.0fs)\n",
		counts.Kept, counts.Doji, counts.Warmup, counts.Gap, time.Since(tSample).Seconds())
	// cmd/backtest 와 달리 개수 하드 단언은 없다 — train 은 최신 데이터로 돌아야
	// 하므로 개수가 날마다 달라지는 것이 정상이다. 대신 합계 정합성만 본다.
	if got := counts.Kept + counts.Doji + counts.Warmup + counts.Gap; got != b5.Len() {
		return fmt.Errorf("제외 개수 합계 %d 가 5분봉 수 %d 와 다르다", got, b5.Len())
	}
	if len(cs) == 0 {
		return fmt.Errorf("표본이 없다")
	}

	if !skipWF {
		testStart := cs[0] + int64(trainDays*walkforward.DayMS)
		fmt.Printf("\n[검증] 워크포워드 %g일마다 직전 %g일\n", refitDays, trainDays)
		prob, nFit, err := walkforward.Run(cs, mat, y, features.FeatureNames,
			testStart, refitDays, trainDays, l2, logf)
		if err != nil {
			return err
		}
		var ey, ep []float64
		for i := range prob {
			if !math.IsNaN(prob[i]) {
				ey = append(ey, y[i])
				ep = append(ep, prob[i])
			}
		}
		// 검증 표본이 0개면 여기서 죽는다 — 아래로 내려보내면 안 된다.
		//
		// -train-days 를 데이터 기간보다 크게 주면 testStart 가 데이터 끝을
		// 넘어 walkforward.Run 이 한 번도 Fit 하지 않는다. 그러면 prob 이 전부
		// NaN → ey/ep 가 빈 슬라이스 → 0/0 으로 정확도·AUC·ECE 가 전부 NaN 이
		// 된다. 종전에는 그 NaN 을 찍고 **그대로 다음 단계로 갔다**.
		//
		// 그 다음 단계가 문제다. TrainableBefore 는 창이 거대하므로 전 표본을
		// 돌려주고, len(rows) < 5000 가드를 여유롭게 통과하고, Fit 도 Validate 도
		// 계수가 멀쩡하니 통과한다 — **워크포워드를 한 번도 통과하지 못한 모델이
		// models.json 으로 저장된다.** cmd/train 은 실거래에 나갈 모델을 만드는
		// 유일한 도구이고, cmd/backtest 와 달리 하드 개수 단언이 의도적으로
		// 빠져 있어(위 주석 참고) 이 경로를 잡을 것이 아무것도 없다.
		//
		// 아래 len(rows) < 5000 가드는 **학습** 표본만 본다 — 검증 표본은
		// 보지 않는다. 그래서 별도로 여기서 막는다. -skip-walkforward 로
		// 명시적으로 건너뛰는 것과 "돌렸는데 표본이 0개" 는 다른 사건이다.
		if len(ey) == 0 {
			return fmt.Errorf("워크포워드 검증 표본이 0개다 — 정확도·AUC·ECE 가 전부 NaN 이라 "+
				"검증되지 않은 모델을 저장하게 된다. -train-days %g 가 데이터 기간(%s ~ %s)보다 "+
				"길어 재학습이 한 번도 일어나지 않았을 가능성이 높다(재학습 %d회). "+
				"-train-days 를 줄이거나, 검증 없이 저장할 의도라면 -skip-walkforward 를 명시하라",
				trainDays, iso(cs[0]), iso(cs[len(cs)-1]), nFit)
		}

		correct := 0
		for i := range ey {
			p := 0.0
			if ep[i] >= 0.5 {
				p = 1.0
			}
			if p == ey[i] {
				correct++
			}
		}
		fmt.Printf("  표본 %d / 재학습 %d회\n", len(ey), nFit)
		fmt.Printf("  정확도 %.3f%%  AUC %.4f  ECE %.4f\n",
			float64(correct)/float64(len(ey))*100, metrics.AUC(ey, ep), metrics.ECE(ey, ep, 10))
	}

	// 저장할 모델: 가장 최근 trainDays 창. 정답이 확정된 표본만 쓴다.
	last := cs[len(cs)-1] + fiveMS
	rows := walkforward.TrainableBefore(cs, last, int64(trainDays*walkforward.DayMS))
	fmt.Printf("\n[학습] 최근 %g일, 표본 %d개  (~%s)\n", trainDays, len(rows), iso(last))
	if len(rows) < 5000 {
		return fmt.Errorf("학습 표본이 %d개뿐이다 — 5,000개 미만이면 신뢰할 수 없다", len(rows))
	}
	lr, err := model.Fit(mat, rows, y, features.FeatureNames, l2)
	if err != nil {
		return err
	}
	if err := lr.Validate(features.FeatureNames); err != nil {
		return fmt.Errorf("학습 직후 검증 실패: %w", err)
	}
	if err := model.Save(out, lr); err != nil {
		return err
	}
	fmt.Printf("  → %s (n_train=%d, l2=%g)\n", out, lr.NTrain, lr.L2)

	// 저장한 것을 다시 읽어 같은 확률을 내는지 확인한다.
	back, err := model.Load(out, features.FeatureNames)
	if err != nil {
		return fmt.Errorf("저장 직후 재로드 실패: %w", err)
	}
	x := mat.Row(rows[len(rows)-1])
	if a, b := lr.Prob(x), back.Prob(x); a != b {
		return fmt.Errorf("왕복 후 확률이 다르다: %v vs %v", a, b)
	}
	fmt.Printf("  왕복 확인: 같은 표본에서 확률 일치 (%.6f)\n", back.Prob(x))
	fmt.Printf("\n총 소요 %.0fs\n", time.Since(t0).Seconds())
	return nil
}

func iso(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04 UTC") }

func logf(format string, args ...any) { fmt.Printf("    "+format+"\n", args...) }
