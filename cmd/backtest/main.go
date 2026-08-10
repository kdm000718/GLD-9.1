// Command backtest 는 전 구간 워크포워드를 돌려 Python 결과를 재현한다 (게이트 G1').
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/bars"
	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/metrics"
	"github.com/kdm000718/GLD-9.1/internal/model"
	"github.com/kdm000718/GLD-9.1/internal/sample"
	"github.com/kdm000718/GLD-9.1/internal/vision"
	"github.com/kdm000718/GLD-9.1/internal/walkforward"
)

// refPath 는 -ref 로 덮어쓴다.
var refPath string

func main() {
	symbol := flag.String("symbol", "BTCUSDT", "심볼")
	cache := flag.String("cache", "data", "Vision 캐시 디렉터리")
	trainDays := flag.Float64("train-days", 180, "학습 창 (일)")
	refitDays := flag.Float64("refit-days", 30, "재학습 주기 (일)")
	l2 := flag.Float64("l2", 10, "L2 세기")
	// Python 참조 실행과 입력을 맞추기 위한 절단 기준. 이것 없이는 실행일에 따라
	// 데이터가 늘어나 표본 수가 어긋난다.
	endFlag := flag.String("end", "2026-08-06T23:55:00Z", "마지막 5분봉 시작 시각 (RFC3339)")
	refFlag := flag.String("ref", "data/reference/py_predictions_full.bin",
		"Python 참조 예측 바이너리")
	flag.Parse()
	refPath = *refFlag

	// walkforward.Run 의 블록 루프는 start += step 으로 전진한다. step <= 0 이면
	// 영원히 끝나지 않는다. Python 은 range() 가 step=0 에서 즉시 에러를 내고
	// step<0 이면 아무것도 안 도는데, Go 에는 그런 방어가 없다 — 호출부인 여기서 막는다.
	if *refitDays <= 0 || *trainDays <= 0 {
		fmt.Fprintf(os.Stderr, "실패: -refit-days 와 -train-days 는 0 보다 커야 한다 (받은 값 %g, %g)\n",
			*refitDays, *trainDays)
		os.Exit(2)
	}

	if err := run(*symbol, *cache, *trainDays, *refitDays, *l2, *endFlag); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(1)
	}
}

func run(symbol, cache string, trainDays, refitDays, l2 float64, endFlag string) error {
	ctx := context.Background()
	t0 := time.Now()

	fmt.Println("[데이터] Binance Vision 수집 (SHA256 검증)")
	b1, err := vision.LoadFullHistory(ctx, symbol, "1m", cache, logf)
	if err != nil {
		return err
	}
	b5, err := vision.LoadFullHistory(ctx, symbol, "5m", cache, logf)
	if err != nil {
		return err
	}
	fmt.Printf("  1분봉 %d개  %s ~ %s\n", b1.Len(), iso(b1.OpenTime[0]), iso(b1.OpenTime[b1.Len()-1]))
	fmt.Printf("  5분봉 %d개  %s ~ %s  (%.0fs)\n", b5.Len(),
		iso(b5.OpenTime[0]), iso(b5.OpenTime[b5.Len()-1]), time.Since(t0).Seconds())

	// Python 참조 실행과 입력 구간을 맞춘다. LoadFullHistory 는 실행일 기준 "어제까지"를
	// 받으므로 그대로 두면 실행할 때마다 표본 수가 달라진다.
	end, err := time.Parse(time.RFC3339, endFlag)
	if err != nil {
		return fmt.Errorf("-end 파싱 실패: %w", err)
	}
	endMS := end.UnixMilli()
	// 경계는 타임프레임마다 다르다. -end 는 마지막 5분봉의 '시작' 시각이므로
	// 5분봉은 그대로 자르고, 1분봉은 그 봉을 구성하는 마지막 분(+4분)까지 남긴다.
	// 같은 경계를 두 곳에 쓰면 1분봉이 정확히 4개 모자란다 (실측 확인).
	b1 = truncateTo(b1, endMS+4*60_000)
	b5 = truncateTo(b5, endMS)
	fmt.Printf("  절단 후 1분봉 %d / 5분봉 %d  (기준 %s)\n", b1.Len(), b5.Len(), endFlag)
	if b1.Len() != 4_710_079 || b5.Len() != 942_025 {
		return fmt.Errorf("절단 후 봉 수가 Python 참조와 다르다: 1분봉 %d(기대 4710079) / 5분봉 %d(기대 942025)",
			b1.Len(), b5.Len())
	}

	fmt.Println("\n[표본] +0분 피처 생성")
	cs, mat, y, err := buildMatrix(b1, b5)
	if err != nil {
		return err
	}

	testStart := cs[0] + int64(trainDays*walkforward.DayMS)
	fmt.Printf("\n[워크포워드] %g일마다 직전 %g일로 재학습, 평가 시작 %s\n",
		refitDays, trainDays, iso(testStart))
	prob, nFit, err := walkforward.Run(cs, mat, y, features.FeatureNames,
		testStart, refitDays, trainDays, l2, logf)
	if err != nil {
		return err
	}

	// 평가된 표본만 남긴다. candle_start 도 같이 모은다 — 참조 대조에 필요하다.
	var ey, ep []float64
	var ecs []int64
	for i := range prob {
		if !math.IsNaN(prob[i]) {
			ey = append(ey, y[i])
			ep = append(ep, prob[i])
			ecs = append(ecs, cs[i])
		}
	}
	if len(ey) == 0 {
		return fmt.Errorf("평가된 표본이 없습니다")
	}
	correct := 0
	for i := range ey {
		pred := 0.0
		if ep[i] >= 0.5 {
			pred = 1.0
		}
		if pred == ey[i] {
			correct++
		}
	}
	acc := float64(correct) / float64(len(ey))
	auc := metrics.AUC(ey, ep)
	ece := metrics.ECE(ey, ep, 10)

	fmt.Println("\n========================================================")
	fmt.Printf("전체 결과  n=%d\n", len(ey))
	fmt.Println("========================================================")
	fmt.Printf("  정확도  : %.3f%%   (p=%.4g)\n", acc*100, metrics.BinomTestNormal(correct, len(ey)))
	fmt.Printf("  AUC     : %.4f\n", auc)
	fmt.Printf("  ECE     : %.4f\n", ece)
	fmt.Printf("  재학습  : %d회\n", nFit)
	fmt.Printf("  소요    : %.0f초\n", time.Since(t0).Seconds())

	// ---- G1' 판정 ----
	const (
		wantAcc = 0.52773
		wantAUC = 0.5408
		wantN   = 888_525
		wantFit = 104
		// ±0.03%p. 이 중 ~0.005%p 는 최적화기 차이라 줄일 수 없다 — 참조 실행의
		// scipy 는 |g|max 0.57~1.04 에서 멈추는데 gonum 은 최적점까지 간다(실측).
		// 요약 정확도는 그만큼 느슨하게 두고, 대신 아래 표본별 대조로 조인다.
		tolAcc = 0.0003
		tolAUC = 0.0005
	)
	fmt.Println("\n==================== G1' Python 재현 ====================")
	pass := true
	check := func(name string, got, want, tol float64) {
		ok := math.Abs(got-want) <= tol
		mark := "통과"
		if !ok {
			mark, pass = "실패", false
		}
		fmt.Printf("  %-10s go=%.5f  py=%.5f  차이 %+.5f  (허용 %.5f)  %s\n",
			name, got, want, got-want, tol, mark)
	}
	check("정확도", acc, wantAcc, tolAcc)
	check("AUC", auc, wantAUC, tolAUC)
	if len(ey) != wantN {
		fmt.Printf("  %-10s go=%d  py=%d  ■ 완전 일치 필요 — 절단·연속성 로직이 다르다\n",
			"표본 수", len(ey), wantN)
		pass = false
	} else {
		fmt.Printf("  %-10s go=%d  py=%d  통과\n", "표본 수", len(ey), wantN)
	}
	if nFit != wantFit {
		fmt.Printf("  %-10s go=%d  py=%d  ■ 불일치\n", "재학습", nFit, wantFit)
		pass = false
	}
	ref, refErr := loadReference(refPath)
	if refErr != nil {
		fmt.Printf("\n  ■ %v\n", refErr)
		pass = false
	} else {
		// 참조 파일의 정체를 로그에 남긴다 — 6개월 뒤 재현하는 사람이 같은 파일을
		// 대조했는지 확인할 방법이 이것 말고는 없다(파일 자체는 gitignore 됨).
		fmt.Printf("  참조 파일 %s  SHA256 %s  n=%d\n", refPath, ref.SHA256, len(ref.CS))
		if !checkMetricsOnReference(ref) {
			pass = false
		}
		if !compareReference(ref, ecs, ey, ep) {
			pass = false
		}
	}

	fmt.Println()
	if !pass {
		return fmt.Errorf("G1' 실패 — 포팅이 틀렸다")
	}
	fmt.Println("  판정: 통과 — P0~P1 완료")
	return nil
}

// truncateTo 는 open_time 이 endMS 를 넘는 봉을 잘라낸다.
func truncateTo(b bars.Bars, endMS int64) bars.Bars {
	hi := sort.Search(b.Len(), func(i int) bool { return b.OpenTime[i] > endMS })
	return b.Slice(0, hi)
}

func iso(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04 UTC") }

func logf(format string, args ...any) { fmt.Printf(format+"\n", args...) }

// buildMatrix 는 +0분 표본을 행렬에 직접 채운다.
// 표본이 100만 개 규모라 객체를 쌓지 않고 미리 잡은 행렬에 바로 쓴다.
func buildMatrix(b1, b5 bars.Bars) ([]int64, *model.Matrix, []float64, error) {
	t0 := time.Now()
	cs, mat, y, counts := sample.Build(b1, b5, func(kept int) {
		fmt.Printf("    ... %d개 (%.0fs)\n", kept, time.Since(t0).Seconds())
	})
	kept, skipDoji, skipWarmup, skipGap := counts.Kept, counts.Doji, counts.Warmup, counts.Gap
	fmt.Printf("    표본 %d개  제외: 도지 %d / 워밍업 %d / 결측 %d  (%.0fs)\n",
		kept, skipDoji, skipWarmup, skipGap, time.Since(t0).Seconds())

	// 도지 제외 수는 open==close 만 보므로 정확히 셀 수 있다. 팀리드가 실데이터에서
	// 세어 확인했다 — 어긋나면 입력 구간이나 도지 판정이 다르다는 뜻이다.
	//
	// 주의: 여기서 세는 kept 는 '유지된 표본' 이고, Python 이 발표한 888,525 는
	// '평가된 표본' 이다. 앞쪽 180일치는 학습에만 쓰여 유지되지만 평가되지 않는다.
	// 둘을 같은 값으로 보면 안 된다 — 팀리드가 한 번 혼동해 잘못된 단언을 넣었다가
	// 지웠다. 표본 선택이 맞는지는 아래 candle_start 수열 대조가 판정한다.
	if skipDoji != 5_039 {
		return nil, nil, nil, fmt.Errorf(
			"도지 제외 %d개, 기대 5,039 — open==close 판정이 다르거나 입력 구간이 다르다", skipDoji)
	}

	// 워밍업·결측·kept 도 단언한다. 아래 네 값은 팀리드가 Python build_matrix 를
	// (스텁이 아니라 Bars.slice 를 쓰는 진짜 구현으로) 같은 절단 구간에 독립적으로
	// 돌려 실측한 값이다(806초 소요) — Go 자신의 출력을 되돌려 대조하는 게 아니다.
	//
	//   kept 932,491 / 도지 5,039 / 워밍업 37 / 결측 4,458  (합계 = 5분봉 수 942,025)
	//
	// 왜 필요한가: G1' 의 표본별 대조(candle_start 완전 일치, compareReference)는
	// walkforward 가 실제로 평가한 888,525개만 본다. kept 932,491개 중 앞쪽 180일치
	// 43,966개는 학습에만 쓰이고 평가되지 않으므로 표본별로 대조할 방법이 없다 —
	// 그런데 전체 제외의 약 2/3 이 그 구간에서 일어난다. 워밍업·결측 판정이 그
	// 구간에서만 Python 과 어긋나면 게이트 1~3 층을 그대로 통과하고, 5층에서도
	// 허용치 안의 사소한 변화로만 보여 드러나지 않는다. 범위 전체에 대한 이 세
	// 개수 단언이 그 사각지대를 닫는다.
	//
	// 순서: 워밍업·결측을 먼저 봐서 실패 메시지가 구체적인 규칙을 짚게 하고,
	// kept 는 마지막에 전체 정합성 확인용으로 둔다(셋 다 맞으면 kept 도 자동으로
	// 맞지만, 계산 실수를 잡는 이중 확인으로 남겨둔다).
	if skipWarmup != 37 {
		return nil, nil, nil, fmt.Errorf(
			"워밍업 제외 %d개, 기대 37 — clock.New 의 거부(t 이전에 마감된 봉이 없는 "+
				"데이터 시작부), Bars1m/Bars5m 길이 하한(req1m=%d, req5m=%d), "+
				"features.Build 의 거부 중 하나가 Python 과 다르다", skipWarmup, sample.Req1m, sample.Req5m)
	}
	if skipGap != 4_458 {
		return nil, nil, nil, fmt.Errorf(
			"결측 제외 %d개, 기대 4,458 — 연속성 조건 세 가지(1분봉 마지막 시각이 "+
				"t-1분인지, 최근 %d개 1분봉이 끊김없이 이어지는지, 최근 %d개 5분봉이 "+
				"끊김없이 이어지는지) 중 하나가 Python 과 다르다", skipGap, sample.Req1m, sample.Req5m)
	}
	if kept != 932_491 {
		return nil, nil, nil, fmt.Errorf(
			"유지 표본 %d개, 기대 932,491 — 도지/워밍업/결측 판정 중 하나가 Python 과 다르다", kept)
	}

	return cs, mat, y, nil
}

// refPredictions 는 Python 참조 실행의 표본별 예측이다.
// tools/export_py_predictions.py 가 만든다.
type refPredictions struct {
	CS     []int64
	Prob   []float64
	Label  []int8
	SHA256 string // 원본 파일 전체의 해시. 로그에 남겨 참조 파일의 정체를 확인할 수 있게 한다.
}

const refMagic = "GLD9PRED"

func loadReference(path string) (*refPredictions, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("참조 예측을 읽지 못했다: %w\n"+
			"  tools/export_py_predictions.py 로 만든다", err)
	}
	if len(raw) < 16 || string(raw[:8]) != refMagic {
		return nil, fmt.Errorf("참조 예측 형식이 아니다: %s", path)
	}
	n := int(binary.LittleEndian.Uint64(raw[8:16]))
	if n <= 0 {
		return nil, fmt.Errorf("참조 예측이 비었다")
	}
	// cs(int64) + prob(float64) + label(int8)
	want := 16 + n*8 + n*8 + n
	if len(raw) != want {
		return nil, fmt.Errorf("참조 예측 크기가 맞지 않는다: %d 바이트, 기대 %d", len(raw), want)
	}
	sum := sha256.Sum256(raw)
	r := &refPredictions{
		CS:     make([]int64, n),
		Prob:   make([]float64, n),
		Label:  make([]int8, n),
		SHA256: hex.EncodeToString(sum[:]),
	}
	off := 16
	for i := 0; i < n; i++ {
		r.CS[i] = int64(binary.LittleEndian.Uint64(raw[off+i*8:]))
	}
	off += n * 8
	for i := 0; i < n; i++ {
		v := math.Float64frombits(binary.LittleEndian.Uint64(raw[off+i*8:]))
		// NaN/Inf 가 섞이면 metrics.AUC·ECE 가 설계대로 NaN 을 돌려주는데, 아래
		// 판정 코드가 NaN 과 비교하면 "차이 없음"으로 오독한다(Go 에서 NaN > tol
		// 은 항상 false). 검증이 무력화되기 전에 여기서 막는다.
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("참조 예측 확률이 유한하지 않다: idx=%d 값=%v", i, v)
		}
		r.Prob[i] = v
	}
	off += n * 8
	for i := 0; i < n; i++ {
		l := int8(raw[off+i])
		if l != 0 && l != 1 {
			return nil, fmt.Errorf("참조 라벨이 0/1 이 아니다: idx=%d 값=%d", i, l)
		}
		r.Label[i] = l
	}
	return r, nil
}

// checkMetricsOnReference 는 Go 의 지표 구현을 Python 의 확률값에 그대로 적용해
// Python 이 발표한 값과 맞는지 본다.
//
// 이렇게 하면 두 가지 실패가 분리된다. 같은 입력에서 지표가 어긋나면 metrics 패키지가
// 틀린 것이고, 지표는 맞는데 전체 AUC 가 다르면 모델·피처 쪽이 틀린 것이다.
// 이 구분이 없으면 G1' 실패 하나를 놓고 어디를 봐야 할지 알 수 없다.
//
// 입력이 완전히 같으므로 차이는 부동소수점 누산 순서뿐이다(numpy 는 pairwise,
// Go 는 순차). 허용 1e-9 는 그 차이보다 여섯 자릿수 크고, 모델 허용치보다는
// 여섯 자릿수 작다 — 지표 버그는 반드시 걸린다.
func checkMetricsOnReference(ref *refPredictions) bool {
	const (
		pyAcc = 0.5277274134098646
		pyAUC = 0.5408063898566969
		pyECE = 0.007969097421415923
		tol   = 1e-9
	)
	y := make([]float64, len(ref.Label))
	correct := 0
	for i, l := range ref.Label {
		y[i] = float64(l)
		pred := 0.0
		if ref.Prob[i] >= 0.5 {
			pred = 1.0
		}
		if pred == y[i] {
			correct++
		}
	}
	acc := float64(correct) / float64(len(y))
	auc := metrics.AUC(y, ref.Prob)
	ece := metrics.ECE(y, ref.Prob, 10)

	fmt.Println("\n======== G1' 지표 구현 검증 (Python 확률값 입력) ========")
	ok := true
	chk := func(name string, got, want float64) {
		d := math.Abs(got - want)
		mark := "통과"
		// d > tol 로 쓰면 d 가 NaN 일 때 (Go 에서 NaN 과의 비교는 항상 false 이므로)
		// 조용히 통과로 읽힌다. !(d <= tol) 은 NaN 을 실패로 잡는다.
		if !(d <= tol) {
			mark, ok = "실패", false
		}
		fmt.Printf("  %-8s go=%.16f  py=%.16f  차이 %.3e  %s\n", name, got, want, d, mark)
	}
	chk("정확도", acc, pyAcc)
	chk("AUC", auc, pyAUC)
	chk("ECE", ece, pyECE)
	if !ok {
		fmt.Println("  → metrics 패키지가 Python 과 다르다. 모델이 아니라 여기를 고칠 것.")
	}
	return ok
}

// compareReference 는 Go 의 표본별 예측을 Python 참조와 대조한다.
//
// 허용치는 팀리드가 실측한 최적화기 차이에서 나왔다. 참조 실행의 scipy 는
// 기본 ftol 때문에 |g|max 0.57~1.04 에서 멈추는데 Go 는 최적점까지 간다.
// 그 차이가 만드는 폭이 아래 실측값이고, 허용치는 거기에 여유를 둔 것이다.
//
//	max|Δp|   실측 4.253e-3 → 허용 0.01  (2.35배, 9년 전 구간 실측)
//	중앙값    실측 4.3e-5  → 허용 1e-3   (23배)
//	뒤집힘    실측 0.055%  → 허용 0.5%   (9배)
//
// 진짜 포팅 오류는 이 폭을 훨씬 넘으므로 최적화기 잡음과 구분된다.
func compareReference(ref *refPredictions, ecs []int64, ey, ep []float64) bool {
	fmt.Println("\n============ G1' 표본별 대조 (Python 참조) ============")
	if len(ref.CS) != len(ecs) {
		fmt.Printf("  ■ 표본 수 go=%d py=%d — 완전 일치가 필요하다\n", len(ecs), len(ref.CS))
		return false
	}
	for i := range ecs {
		if ecs[i] != ref.CS[i] {
			fmt.Printf("  ■ candle_start 가 %d 번째에서 갈린다: go=%s py=%s\n",
				i, iso(ecs[i]), iso(ref.CS[i]))
			fmt.Println("     표본 선택 로직(도지·워밍업·연속성)이 다르다.")
			return false
		}
	}
	fmt.Printf("  표본 수·candle_start 수열 %d개 완전 일치\n", len(ecs))

	// 정답도 대조한다. 라벨이 어긋나면 정확도가 맞아도 의미가 없다.
	badLabel := 0
	for i := range ey {
		if int8(ey[i]) != ref.Label[i] {
			badLabel++
		}
	}
	if badLabel > 0 {
		fmt.Printf("  ■ 정답 라벨 불일치 %d개 — 봉 방향 판정이 다르다\n", badLabel)
		return false
	}
	fmt.Println("  정답 라벨 전부 일치")

	d := make([]float64, len(ep))
	flips := 0
	for i := range ep {
		d[i] = math.Abs(ep[i] - ref.Prob[i])
		if (ep[i] >= 0.5) != (ref.Prob[i] >= 0.5) {
			flips++
		}
	}
	sort.Float64s(d)
	maxD := d[len(d)-1]
	medD := d[len(d)/2]
	i999 := int(float64(len(d)) * 0.999)
	if i999 >= len(d) {
		i999 = len(d) - 1
	}
	flipRate := float64(flips) / float64(len(ep))

	ok := true
	chk := func(name string, got, tol float64) {
		mark := "통과"
		// got > tol 로 쓰면 got 이 NaN 일 때 조용히 통과로 읽힌다(Go 의 NaN 비교는
		// 항상 false). !(got <= tol) 로 뒤집어야 NaN 을 실패로 잡는다.
		if !(got <= tol) {
			mark, ok = "실패", false
		}
		fmt.Printf("  %-12s %.3e  (허용 %.3e)  %s\n", name, got, tol, mark)
	}
	chk("max|Δp|", maxD, 0.01)
	chk("중앙값|Δp|", medD, 1e-3)
	fmt.Printf("  %-12s %.3e  (참고)\n", "99.9%|Δp|", d[i999])

	mark := "통과"
	if !(flipRate <= 0.005) {
		mark, ok = "실패", false
	}
	fmt.Printf("  %-12s %d개 = %.4f%%  (허용 0.5000%%)  %s\n",
		"뒤집힘", flips, flipRate*100, mark)
	return ok
}
