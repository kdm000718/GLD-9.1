package sample

import (
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/bars"
)

// 도지(open == close)는 제외되고 그 사유로 세어진다.
func TestDojiExcluded(t *testing.T) {
	b1, b5 := synthetic(t, 400)
	i := b5.Len() - 1
	b5.Close[i] = b5.Open[i]

	_, _, _, c := Build(b1, b5, nil)
	if c.Doji != 1 {
		t.Fatalf("도지 %d개, 기대 1", c.Doji)
	}
}

// 1분봉 하나를 빼면 연속성이 깨져 결측으로 세어진다. 워밍업이 아니다 —
// 두 사유가 섞이면 backtest 의 개수 단언이 엉뚱한 규칙을 지목한다.
func TestGapCountedAsGapNotWarmup(t *testing.T) {
	b1, b5 := synthetic(t, 400)
	before := func() Counts { _, _, _, c := Build(b1, b5, nil); return c }()

	b1 = drop1mBar(b1, b1.Len()-30)
	after := func() Counts { _, _, _, c := Build(b1, b5, nil); return c }()

	if after.Gap <= before.Gap {
		t.Fatalf("1분봉을 뺐는데 결측이 늘지 않았다: %d → %d", before.Gap, after.Gap)
	}
	if after.Warmup != before.Warmup {
		t.Errorf("결측이 워밍업으로 세어졌다: %d → %d", before.Warmup, after.Warmup)
	}
}

// **직전 1분봉 인접 조건(sample.go 의 `ot1[l1-1] != t-minMS`)만이 잡는 케이스.**
//
// 위 TestGapCountedAsGapNotWarmup 은 창 한가운데(Len()-30) 봉을 지운다 —
// 그건 두 번째 조건(`ot1[l1-1]-ot1[l1-Req1m] != 59*minMS`, 60봉 창의 폭)이
// 잡는다. 그래서 첫 조건을 통째로 무력화해도 그 테스트는 통과한다(돌연변이로
// 확인했다).
//
// 여기서는 **마지막** 1분봉만 지운다. 그러면 ot1[l1-1] 이 t-2분이 되지만
// 그 앞 60봉은 여전히 연속이라 두 번째·세 번째 조건을 전부 통과한다 —
// 첫 조건이 없으면 아무도 못 잡는다.
//
// 왜 중요한가: 첫 조건이 떨어지면 go build·vet·gofmt·go test 가 전부
// 통과하고, cmd/backtest 의 개수 단언만이 잡는데 그건 9년치 476MB 데이터와
// 3분이 필요해 go test 에 들어 있지 않다. cmd/train 에는 설계상 하드 단언이
// 없어 잡을 것이 아예 없다. 결과는 t 직전 1분봉이 결측인 시점의 표본이
// 학습에 섞이는 것이다 — 그 시점 피처는 2분 이상 묵은 봉으로 계산된 값이라
// 미래참조는 아니지만 **실거래 시점에는 절대 재현되지 않는 입력 분포**다.
// 그 모델이 models.json 으로 나간다.
func TestMissingImmediatelyPrecedingBarIsGap(t *testing.T) {
	b1, b5 := synthetic(t, 400)
	before := func() Counts { _, _, _, c := Build(b1, b5, nil); return c }()

	// **지울 봉을 정확히 골라야 한다.** 리뷰가 제안한 b1.Len()-1(진짜 마지막
	// 1분봉)은 효과가 없다 — 그 봉은 마지막 5분봉 *안*에 있어서 어떤 5분
	// 경계 t 의 "직전 봉" 도 아니다(그 뒤에 5분봉이 없다). clock.New 는 t
	// 이전에 마감된 봉만 주므로 애초에 창에 들어오지도 않는다.
	//
	// 첫 조건을 건드리려면 **어떤 5분 경계 t 바로 앞의 1분봉**을 지워야
	// 한다. 마지막 5분봉의 t 는 b1.OpenTime[len-5] 이므로 그 직전 봉은
	// len-6 이다. 지우면 ot1[l1-1] 이 t-2분이 되지만 그 앞 60봉은 여전히
	// 연속이라 두 번째·세 번째 조건은 통과한다 — 첫 조건만이 잡는다.
	b1 = drop1mBar(b1, b1.Len()-6)
	after := func() Counts { _, _, _, c := Build(b1, b5, nil); return c }()

	if after.Gap <= before.Gap {
		t.Fatalf("직전 1분봉이 결측인데 결측으로 세지 않았다: %d → %d"+
			" — sample.go 의 `ot1[l1-1] != t-minMS` 조건이 없어졌을 수 있다", before.Gap, after.Gap)
	}
	if after.Warmup != before.Warmup {
		t.Errorf("결측이 워밍업으로 세어졌다: %d → %d", before.Warmup, after.Warmup)
	}
}

// 모든 표본은 정확히 한 사유로만 세어진다 — 합계가 입력 봉 수와 같아야 한다.
func TestCountsPartitionInput(t *testing.T) {
	b1, b5 := synthetic(t, 400)
	_, _, _, c := Build(b1, b5, nil)
	if got := c.Kept + c.Doji + c.Warmup + c.Gap; got != b5.Len() {
		t.Fatalf("합계 %d != 입력 5분봉 %d — 어떤 봉이 두 번 세어지거나 누락됐다",
			got, b5.Len())
	}
}

// Truncate 후 반환된 세 값의 길이가 서로 맞아야 한다.
func TestReturnedLengthsAgree(t *testing.T) {
	b1, b5 := synthetic(t, 400)
	cs, mat, y, c := Build(b1, b5, nil)
	if len(cs) != c.Kept || len(y) != c.Kept || mat.Rows != c.Kept {
		t.Fatalf("길이 불일치: cs %d / y %d / mat.Rows %d, Kept %d",
			len(cs), len(y), mat.Rows, c.Kept)
	}
}

// synthetic 은 경계에 맞는 1분봉 n*5개와 5분봉 n개를 만든다. 가격은 완만한
// 상승 추세로 채우고 open != close 를 유지해 도지가 우연히 섞이지 않게 한다.
func synthetic(t *testing.T, n int) (bars.Bars, bars.Bars) {
	t.Helper()
	const base int64 = 1_600_000_000_000 // 5분 경계에 맞춘 임의의 시각

	n1 := n * 5
	b1 := bars.Bars{
		OpenTime: make([]int64, n1), CloseTime: make([]int64, n1),
		Open: make([]float64, n1), High: make([]float64, n1),
		Low: make([]float64, n1), Close: make([]float64, n1),
		Volume: make([]float64, n1), QuoteVolume: make([]float64, n1),
		Trades:       make([]int64, n1),
		TakerBuyBase: make([]float64, n1), TakerBuyQuote: make([]float64, n1),
	}
	for i := 0; i < n1; i++ {
		ot := base + int64(i)*60_000
		o := 100.0 + float64(i)*0.01
		c := o + 0.02 // open != close, 항상 양봉
		b1.OpenTime[i] = ot
		b1.CloseTime[i] = ot + 59_999
		b1.Open[i] = o
		b1.Close[i] = c
		b1.High[i] = c + 0.01
		b1.Low[i] = o - 0.01
		b1.Volume[i] = 10.0
		b1.QuoteVolume[i] = 1_000.0
		b1.Trades[i] = 5
		b1.TakerBuyBase[i] = 5.0
		b1.TakerBuyQuote[i] = 500.0
	}

	b5 := bars.Bars{
		OpenTime: make([]int64, n), CloseTime: make([]int64, n),
		Open: make([]float64, n), High: make([]float64, n),
		Low: make([]float64, n), Close: make([]float64, n),
		Volume: make([]float64, n), QuoteVolume: make([]float64, n),
		Trades:       make([]int64, n),
		TakerBuyBase: make([]float64, n), TakerBuyQuote: make([]float64, n),
	}
	for i := 0; i < n; i++ {
		lo, hi := i*5, i*5+5
		b5.OpenTime[i] = b1.OpenTime[lo]
		b5.CloseTime[i] = b1.OpenTime[lo] + 5*60_000 - 1
		b5.Open[i] = b1.Open[lo]
		b5.Close[i] = b1.Close[hi-1]
		mh, ml := b1.High[lo], b1.Low[lo]
		var vol, qvol, taker float64
		var trades int64
		for j := lo; j < hi; j++ {
			if b1.High[j] > mh {
				mh = b1.High[j]
			}
			if b1.Low[j] < ml {
				ml = b1.Low[j]
			}
			vol += b1.Volume[j]
			qvol += b1.QuoteVolume[j]
			trades += b1.Trades[j]
			taker += b1.TakerBuyBase[j]
		}
		b5.High[i] = mh
		b5.Low[i] = ml
		b5.Volume[i] = vol
		b5.QuoteVolume[i] = qvol
		b5.Trades[i] = trades
		b5.TakerBuyBase[i] = taker
		b5.TakerBuyQuote[i] = qvol * 0.5 // 근사치, 테스트에서 정확한 값이 필요하지 않다
	}
	return b1, b5
}

// drop1mBar 는 idx 번째 1분봉을 제거해 연속성을 깬다.
func drop1mBar(b bars.Bars, idx int) bars.Bars {
	return bars.Bars{
		OpenTime:      dropI64(b.OpenTime, idx),
		CloseTime:     dropI64(b.CloseTime, idx),
		Open:          dropF64(b.Open, idx),
		High:          dropF64(b.High, idx),
		Low:           dropF64(b.Low, idx),
		Close:         dropF64(b.Close, idx),
		Volume:        dropF64(b.Volume, idx),
		QuoteVolume:   dropF64(b.QuoteVolume, idx),
		Trades:        dropI64(b.Trades, idx),
		TakerBuyBase:  dropF64(b.TakerBuyBase, idx),
		TakerBuyQuote: dropF64(b.TakerBuyQuote, idx),
	}
}

func dropI64(x []int64, idx int) []int64 {
	out := make([]int64, 0, len(x)-1)
	out = append(out, x[:idx]...)
	return append(out, x[idx+1:]...)
}

func dropF64(x []float64, idx int) []float64 {
	out := make([]float64, 0, len(x)-1)
	out = append(out, x[:idx]...)
	return append(out, x[idx+1:]...)
}
