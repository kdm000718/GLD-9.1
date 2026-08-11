package ledger

import (
	"encoding/csv"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// ---------------------------------------------------------------------------
// 계획서 Step 1 의 테스트. 기대값은 계획서에 손으로 계산된 것을 그대로 쓴다.
// ---------------------------------------------------------------------------

// 매수는 돈이 나간다. 부호가 뒤집히면 손익이 통째로 뒤집힌다.
func TestFillCostIsPositiveOutflow(t *testing.T) {
	f := Fill{Shares: 100, PriceUSD: 0.49, FeeUSD: 0.2}
	if got := FillCost(f); got != 49.2 {
		t.Errorf("FillCost = %v, 기대 49.2 (49 + 0.2)", got)
	}
}

// 이기면 주당 1.0 이 들어온다. 지면 0.
func TestSettlementProceeds(t *testing.T) {
	if got := SettlementProceeds(Settlement{Won: true, Shares: 100}); got != 100 {
		t.Errorf("이겼는데 %v", got)
	}
	if got := SettlementProceeds(Settlement{Won: false, Shares: 100}); got != 0 {
		t.Errorf("졌는데 %v", got)
	}
}

// **리베이트는 반대편 주식이다** — 우리가 이기면 그 주식은 0 이 되고,
// 우리가 져야 1.0 이 된다. 이 방향을 뒤집으면 엣지 계산이 무너진다
// (G2 가 이 규칙으로 +0.487%p 를 계산했다).
func TestRebateOnlyPaysWhenWeLose(t *testing.T) {
	r := Rebate{Shares: 5}
	if got := RebateValue(r, true); got != 0 {
		t.Errorf("우리가 이겼는데 리베이트 %v, 기대 0 — 반대편 주식은 0 이 된다", got)
	}
	if got := RebateValue(r, false); got != 5 {
		t.Errorf("우리가 졌는데 리베이트 %v, 기대 5", got)
	}
}

// ---------------------------------------------------------------------------
// 부호 규약 — 계획서 밖에서 추가로 못박는 것들
// ---------------------------------------------------------------------------

// 수수료는 비용에 **더해진다**. 위 계획서 테스트의 49.2 는 49 − 0.2 = 48.8
// 과 0.4 밖에 차이가 나지 않아, 부호가 뒤집혔을 때 "가까운 값"으로 보인다.
// 수수료를 크게 잡아 뺄셈이면 결과가 **음수**가 되게 만든다 — 음수 비용은
// 곧 "사면 돈이 들어온다"이고 그것이 pmmm-go 사고의 모양이다.
func TestFillCostAddsFeeNeverSubtracts(t *testing.T) {
	// 10 × 0.4 = 4 지출, 수수료 6 → 10. 뺄셈이면 −2 다.
	f := Fill{Shares: 10, PriceUSD: 0.4, FeeUSD: 6}
	got := FillCost(f)
	if got != 10 {
		t.Fatalf("FillCost = %v, 기대 10 (4 + 6)", got)
	}
	if got < 0 {
		t.Errorf("비용이 음수다 (%v) — 사면 돈이 들어온다는 뜻이 된다", got)
	}
}

// 수수료 0(메이커 정상)에서도 비용은 명목 그대로다.
func TestFillCostWithZeroFee(t *testing.T) {
	if got := FillCost(Fill{Shares: 200, PriceUSD: 0.25}); got != 50 {
		t.Errorf("FillCost = %v, 기대 50", got)
	}
}

// 정산 수입은 매수가를 다시 빼지 않는다. 빼면 비용이 두 번 차감된다.
func TestSettlementProceedsDoesNotSubtractCost(t *testing.T) {
	// 100 주를 0.49 에 샀고 이겼다 → 수입은 100(원가 49 를 빼지 않는다).
	if got := SettlementProceeds(Settlement{Won: true, Shares: 100}); got != 100 {
		t.Errorf("정산 수입 = %v, 기대 100 — 여기서 원가를 빼면 이중 차감이다", got)
	}
}

// 회차 하나를 통째로 조립해 순손익 부호를 고정한다. 세 함수 중 하나라도
// 방향이 틀리면 여기서 죽는다.
//
// 손으로 계산한 시나리오: Up 100 주를 0.49 에 매수(메이커라 수수료 0),
// 리베이트로 Down 0.005 × 100 = 0.5 주를 받는다.
//
//	이긴 경우: −49(매수) + 100(정산) + 0(리베이트 휴지)      = +51
//	진 경우:   −49(매수) +   0(정산) + 0.5(리베이트가 값을 가짐) = −48.5
//
// 리베이트가 **지는 쪽에서만** 들어오는 것이 요점이다. 방향을 뒤집으면
// 이긴 경우가 +51.5, 진 경우가 −49 로 나와 "이길수록 리베이트를 더 받는"
// 세계가 되고, G2 의 0.005·(1−q)/p 계산이 무너진다.
func TestRoundNetPnLSigns(t *testing.T) {
	f := Fill{Shares: 100, PriceUSD: 0.49, FeeUSD: 0}
	r := Rebate{Shares: RebateShareRate * 100}
	if r.Shares != 0.5 {
		t.Fatalf("리베이트 주식 = %v, 기대 0.5", r.Shares)
	}

	net := func(weWon bool) float64 {
		s := Settlement{Won: weWon, Shares: f.Shares}
		return -FillCost(f) + SettlementProceeds(s) + RebateValue(r, weWon)
	}

	if got := net(true); got != 51 {
		t.Errorf("이긴 회차 순손익 = %v, 기대 +51 (−49 + 100 + 0)", got)
	}
	if got := net(false); got != -48.5 {
		t.Errorf("진 회차 순손익 = %v, 기대 −48.5 (−49 + 0 + 0.5)", got)
	}
	if net(true) <= 0 {
		t.Errorf("이겼는데 순손익이 %v 다 — 부호가 뒤집혔다", net(true))
	}
	if net(false) >= 0 {
		t.Errorf("졌는데 순손익이 %v 다 — 부호가 뒤집혔다", net(false))
	}
	// 리베이트는 지는 회차의 손실을 **줄인다**(부분 헤지). 리베이트가 없었다면
	// −49 였을 손실이 −48.5 다.
	if net(false) <= -49 {
		t.Errorf("진 회차 손실 %v 가 리베이트 없을 때(−49)보다 크거나 같다", net(false))
	}
}

// ---------------------------------------------------------------------------
// 망가진 값 — 계산 경로는 전파하고, 하류가 막는다
// ---------------------------------------------------------------------------

// NaN 을 0 으로 삼키면 망가진 피드가 "손익 0" 으로 보인다. 전파시켜서
// 하류(risk)가 거래를 막게 한다. 이 연결이 끊기면 NaN 손익으로 계속 거래한다.
func TestNaNPropagatesAndRiskBlocks(t *testing.T) {
	f := Fill{Shares: math.NaN(), PriceUSD: 0.49}
	cost := FillCost(f)
	if !math.IsNaN(cost) {
		t.Fatalf("FillCost(NaN) = %v — 전파되지 않았다. 0 으로 삼키면 망가진 값이 손익 0 으로 보인다", cost)
	}
	// 실현손익으로 −cost 가 흘러가고, risk 는 유한하지 않은 손익을
	// **한도 위반(거래 차단)** 으로 읽는다.
	d := risk.DailyLimit{StartEquity: 1000, Fraction: risk.DefaultDailyFraction}
	if !d.Breached(-cost) {
		t.Error("NaN 손익인데 risk 가 거래를 막지 않았다")
	}
}

// ±Inf 도 같다.
func TestInfPropagates(t *testing.T) {
	f := Fill{Shares: math.Inf(1), PriceUSD: 0.49}
	if got := FillCost(f); !math.IsInf(got, 1) {
		t.Errorf("FillCost(+Inf) = %v, 기대 +Inf", got)
	}
}

// ---------------------------------------------------------------------------
// 파일 — 헤더·append·flush
// ---------------------------------------------------------------------------

func fixedTime() time.Time {
	return time.Date(2026, 8, 10, 12, 34, 56, 789000000, time.UTC)
}

func sampleFill(round int64) Fill {
	return Fill{
		RoundStart: round,
		MarketID:   4242,
		Outcome:    OutcomeUp,
		Shares:     100,
		PriceUSD:   0.49,
		FeeUSD:     0,
		At:         fixedTime(),
	}
}

// readRows 는 원장을 CSV 로 되읽는다. 헤더 포함 전 행을 돌려준다.
// csv.Reader 는 기본적으로 열 개수가 첫 행과 다르면 에러를 내므로, 이 함수를
// 지나는 것만으로 "모든 행의 열 수가 헤더와 같다"가 확인된다.
func readRows(t *testing.T, path string) [][]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("원장 읽기 실패: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(string(b))).ReadAll()
	if err != nil {
		t.Fatalf("원장이 CSV 로 파싱되지 않는다: %v\n내용:\n%s", err, b)
	}
	return rows
}

// 새 파일에는 헤더가 있어야 한다.
func TestOpenWritesHeaderOnNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rows := readRows(t, path)
	if len(rows) != 1 {
		t.Fatalf("행 수 = %d, 기대 1(헤더만): %v", len(rows), rows)
	}
	if len(rows[0]) != len(header) {
		t.Fatalf("헤더 열 수 = %d, 기대 %d", len(rows[0]), len(header))
	}
	for i, c := range header {
		if rows[0][i] != c {
			t.Errorf("헤더[%d] = %q, 기대 %q", i, rows[0][i], c)
		}
	}
}

// **기존 파일에 다시 열면 이어 쓰고 헤더는 다시 쓰지 않는다.**
//
// 크래시 복구가 이 파일을 파싱하므로 헤더가 중간에 또 나오면 파서가 그 줄에서
// 깨지거나 헤더를 데이터로 읽는다. 그리고 이어 쓰지 못하고 덮어쓰면 재시작
// 한 번에 과거 체결이 통째로 사라진다.
func TestReopenAppendsAndWritesHeaderOnlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")

	l1, err := Open(path)
	if err != nil {
		t.Fatalf("첫 Open: %v", err)
	}
	if err := l1.RecordFill(sampleFill(1000)); err != nil {
		t.Fatalf("RecordFill: %v", err)
	}
	if err := l1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 재시작을 흉내낸다.
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("두 번째 Open: %v", err)
	}
	if err := l2.RecordFill(sampleFill(2000)); err != nil {
		t.Fatalf("RecordFill: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rows := readRows(t, path)
	if len(rows) != 3 {
		t.Fatalf("행 수 = %d, 기대 3(헤더 1 + 체결 2) — append 되지 않았다: %v", len(rows), rows)
	}

	headerCount := 0
	for _, r := range rows {
		if r[0] == header[0] {
			headerCount++
		}
	}
	if headerCount != 1 {
		t.Errorf("헤더 줄이 %d 개다, 기대 1 — 기존 파일에 헤더를 또 썼다", headerCount)
	}

	// 첫 Open 의 체결이 살아 있고 순서가 보존됐다.
	if rows[1][2] != "1000" {
		t.Errorf("두 번째 행 round_start = %q, 기대 \"1000\" — 첫 세션 기록이 사라졌다", rows[1][2])
	}
	if rows[2][2] != "2000" {
		t.Errorf("세 번째 행 round_start = %q, 기대 \"2000\"", rows[2][2])
	}
}

// 세 번 열어도 헤더는 하나다 — 재시작이 반복돼도 같다.
func TestManyReopensKeepSingleHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	for i := 0; i < 5; i++ {
		l, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		if err := l.RecordFill(sampleFill(int64(1000 + i))); err != nil {
			t.Fatalf("RecordFill #%d: %v", i, err)
		}
		if err := l.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i, err)
		}
	}
	rows := readRows(t, path)
	if len(rows) != 6 {
		t.Fatalf("행 수 = %d, 기대 6(헤더 1 + 체결 5): %v", len(rows), rows)
	}
	for i, r := range rows[1:] {
		if want := strconv.Itoa(1000 + i); r[2] != want {
			t.Errorf("행 %d round_start = %q, 기대 %q", i+1, r[2], want)
		}
	}
}

// **레코드마다 디스크에 밀어 넣는다.** Close 를 기다리지 않는다.
//
// 봇은 정상 종료하지 않는다 — 크래시하거나 kill 당한다. 버퍼에 남은 줄은
// 그때 통째로 사라지고, 크래시 복구는 실제로는 체결된 포지션을 원장에서
// 못 찾는다.
func TestRecordIsOnDiskBeforeClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	if err := l.RecordFill(sampleFill(1000)); err != nil {
		t.Fatalf("RecordFill: %v", err)
	}

	// Close 하지 않은 채로 다른 핸들이 읽는다.
	rows := readRows(t, path)
	if len(rows) != 2 {
		t.Fatalf("Close 전 행 수 = %d, 기대 2 — 레코드가 버퍼에 남아 있다: %v", len(rows), rows)
	}

	if err := l.RecordRebate(Rebate{RoundStart: 1000, Shares: 0.5, At: fixedTime()}); err != nil {
		t.Fatalf("RecordRebate: %v", err)
	}
	if rows := readRows(t, path); len(rows) != 3 {
		t.Fatalf("두 번째 레코드 뒤 행 수 = %d, 기대 3", len(rows))
	}
}

// 반쯤 쓰인 줄이 남으면 안 된다 — 모든 줄이 개행으로 끝난다.
func TestNoPartialLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	if err := l.RecordFill(sampleFill(1000)); err != nil {
		t.Fatalf("RecordFill: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("읽기: %v", err)
	}
	if len(b) == 0 || b[len(b)-1] != '\n' {
		t.Errorf("파일이 개행으로 끝나지 않는다 — 반쯤 쓰인 줄이다:\n%q", b)
	}
}

// ---------------------------------------------------------------------------
// 파일 — 세 종류가 모두 되읽힌다
// ---------------------------------------------------------------------------

// 쓴 것을 그대로 되읽을 수 있어야 한다. 크래시 복구가 이 파일만 보고 포지션을
// 재구성한다.
func TestAllRecordTypesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	at := fixedTime()
	if err := l.RecordFill(Fill{
		RoundStart: 1754822100, MarketID: 4242, Outcome: OutcomeDown,
		Shares: 37, PriceUSD: 0.47, FeeUSD: 0.02, At: at,
	}); err != nil {
		t.Fatalf("RecordFill: %v", err)
	}
	if err := l.RecordRebate(Rebate{RoundStart: 1754822100, Shares: 0.185, At: at}); err != nil {
		t.Fatalf("RecordRebate: %v", err)
	}
	if err := l.RecordSettlement(Settlement{RoundStart: 1754822100, Won: false, Shares: 37, At: at}); err != nil {
		t.Fatalf("RecordSettlement: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rows := readRows(t, path)
	if len(rows) != 4 {
		t.Fatalf("행 수 = %d, 기대 4: %v", len(rows), rows)
	}

	// 열 이름 → 인덱스는 헤더에서 찾는다. 크래시 복구도 그렇게 해야 한다.
	idx := map[string]int{}
	for i, c := range rows[0] {
		idx[c] = i
	}
	get := func(row []string, col string) string {
		i, ok := idx[col]
		if !ok {
			t.Fatalf("헤더에 %q 열이 없다", col)
		}
		return row[i]
	}

	fill, rebate, settle := rows[1], rows[2], rows[3]

	if get(fill, "record") != "fill" || get(rebate, "record") != "rebate" || get(settle, "record") != "settlement" {
		t.Fatalf("record 열이 종류를 구분하지 못한다: %q %q %q",
			get(fill, "record"), get(rebate, "record"), get(settle, "record"))
	}

	// 시각은 UTC RFC3339Nano 로 정확히 되읽힌다.
	back, err := time.Parse(time.RFC3339Nano, get(fill, "at_utc"))
	if err != nil {
		t.Fatalf("at_utc 파싱 실패 (%q): %v", get(fill, "at_utc"), err)
	}
	if !back.Equal(at) {
		t.Errorf("at_utc 되읽기 = %v, 기대 %v", back, at)
	}

	// 체결 행: 수치가 정확히 되읽힌다.
	if got := get(fill, "round_start"); got != "1754822100" {
		t.Errorf("round_start = %q", got)
	}
	if got := get(fill, "market_id"); got != "4242" {
		t.Errorf("market_id = %q", got)
	}
	if got := get(fill, "outcome"); got != OutcomeDown {
		t.Errorf("outcome = %q, 기대 %q", got, OutcomeDown)
	}
	mustFloat := func(row []string, col string, want float64) {
		t.Helper()
		v, err := strconv.ParseFloat(get(row, col), 64)
		if err != nil {
			t.Fatalf("%s 파싱 실패 (%q): %v", col, get(row, col), err)
		}
		if v != want {
			t.Errorf("%s = %v, 기대 %v", col, v, want)
		}
	}
	mustFloat(fill, "shares", 37)
	mustFloat(fill, "price_usd", 0.47)
	mustFloat(fill, "fee_usd", 0.02)

	// 되읽은 행으로 비용을 다시 계산하면 손으로 계산한 값과 같다.
	// 37 × 0.47 = 17.39, + 0.02 = 17.41
	shares, _ := strconv.ParseFloat(get(fill, "shares"), 64)
	price, _ := strconv.ParseFloat(get(fill, "price_usd"), 64)
	fee, _ := strconv.ParseFloat(get(fill, "fee_usd"), 64)
	if got := FillCost(Fill{Shares: shares, PriceUSD: price, FeeUSD: fee}); math.Abs(got-17.41) > 1e-12 {
		t.Errorf("되읽은 행의 FillCost = %v, 기대 17.41", got)
	}

	// 리베이트 행: 값(USD)이 아니라 주식 수를 적는다. 값은 정산이 정한다.
	mustFloat(rebate, "shares", 0.185)
	if got := get(rebate, "price_usd"); got != "" {
		t.Errorf("리베이트 행 price_usd = %q, 기대 빈 값 — 0 을 쓰면 공짜 매수로 읽힌다", got)
	}
	if got := get(rebate, "won"); got != "" {
		t.Errorf("리베이트 행 won = %q, 기대 빈 값", got)
	}

	// 정산 행: won 이 우리 포지션 기준이다.
	if got := get(settle, "won"); got != "false" {
		t.Errorf("won = %q, 기대 \"false\"", got)
	}
	mustFloat(settle, "shares", 37)
}

// 로컬 타임존으로 들어와도 파일에는 UTC 로 적힌다. 도쿄 박스로 옮겼을 때
// 같은 파일 안에서 오프셋이 바뀌면 회차 경계 대조가 사람 암산에 의존한다.
func TestTimestampIsUTC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	tokyo := time.FixedZone("JST", 9*3600)
	local := fixedTime().In(tokyo)
	f := sampleFill(1000)
	f.At = local
	if err := l.RecordFill(f); err != nil {
		t.Fatalf("RecordFill: %v", err)
	}

	rows := readRows(t, path)
	got := rows[1][0]
	if !strings.HasSuffix(got, "Z") {
		t.Errorf("at_utc = %q — UTC 로 정규화되지 않았다", got)
	}
	back, err := time.Parse(time.RFC3339Nano, got)
	if err != nil {
		t.Fatalf("파싱 실패: %v", err)
	}
	if !back.Equal(local) {
		t.Errorf("시각이 바뀌었다: %v vs %v", back, local)
	}
}

// ---------------------------------------------------------------------------
// 망가진 값 — 기록 경로는 거절하고 파일을 건드리지 않는다
// ---------------------------------------------------------------------------

// **NaN 을 CSV 에 그대로 쓰면 읽는 쪽이 조용히 오염된다.**
//
// 이 테스트는 그 위험이 실재함을 먼저 보이고(ParseFloat 이 "NaN" 을 군말 없이
// 받는다), 그래서 원장이 그 줄을 아예 쓰지 않는다는 것을 고정한다.
func TestNaNIsRejectedAndFileUntouched(t *testing.T) {
	// 전제: 막지 않으면 왕복이 성립해 버린다 — 파일도 파서도 불평하지 않는다.
	if v, err := strconv.ParseFloat(formatFloat(math.NaN()), 64); err != nil || !math.IsNaN(v) {
		t.Fatalf("전제가 깨졌다: NaN 왕복 = %v, %v", v, err)
	}

	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()
	if err := l.RecordFill(sampleFill(1000)); err != nil {
		t.Fatalf("RecordFill: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("읽기: %v", err)
	}

	bad := sampleFill(1001)
	bad.Shares = math.NaN()
	err = l.RecordFill(bad)
	if err == nil {
		t.Fatal("NaN 수량이 기록됐다")
	}
	if !errors.Is(err, ErrInvalidRecord) {
		t.Errorf("에러가 ErrInvalidRecord 가 아니다: %v — 호출자가 디스크 고장과 구분할 수 없다", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("읽기: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("거절된 레코드가 파일을 바꿨다:\n이전:\n%s\n이후:\n%s", before, after)
	}
	if strings.Contains(string(after), "NaN") {
		t.Errorf("파일에 NaN 이 들어갔다:\n%s", after)
	}

	// 거절 뒤에도 원장은 계속 쓸 수 있어야 한다. 한 건의 나쁜 데이터가
	// 원장을 통째로 못 쓰게 만들면 봇이 그 뒤 모든 체결을 못 남긴다.
	if err := l.RecordFill(sampleFill(1002)); err != nil {
		t.Fatalf("거절 뒤 정상 기록 실패: %v", err)
	}
	if rows := readRows(t, path); len(rows) != 3 {
		t.Errorf("행 수 = %d, 기대 3(헤더 + 정상 2건)", len(rows))
	}
}

// 거절 규칙 전체. 한 곳만 막고 인접 경로를 놓치는 것이 이 저장소가 반복해서
// 밟은 함정이라, 세 Record* 의 모든 수치 필드를 표로 훑는다.
func TestRecordRejectsBrokenValues(t *testing.T) {
	nan, pinf, ninf := math.NaN(), math.Inf(1), math.Inf(-1)

	fillCases := []struct {
		name string
		mut  func(*Fill)
	}{
		{"shares NaN", func(f *Fill) { f.Shares = nan }},
		{"shares +Inf", func(f *Fill) { f.Shares = pinf }},
		{"shares -Inf", func(f *Fill) { f.Shares = ninf }},
		{"shares 음수", func(f *Fill) { f.Shares = -1 }},
		{"shares 0", func(f *Fill) { f.Shares = 0 }},
		{"price NaN", func(f *Fill) { f.PriceUSD = nan }},
		{"price +Inf", func(f *Fill) { f.PriceUSD = pinf }},
		{"price -Inf", func(f *Fill) { f.PriceUSD = ninf }},
		{"price 음수", func(f *Fill) { f.PriceUSD = -0.1 }},
		{"price 0", func(f *Fill) { f.PriceUSD = 0 }},
		{"price 1 초과", func(f *Fill) { f.PriceUSD = 49 }}, // 센트를 달러 칸에 넣은 모양
		{"fee NaN", func(f *Fill) { f.FeeUSD = nan }},
		{"fee +Inf", func(f *Fill) { f.FeeUSD = pinf }},
		{"fee -Inf", func(f *Fill) { f.FeeUSD = ninf }},
		{"fee 음수", func(f *Fill) { f.FeeUSD = -1 }}, // 비용을 줄여 적자를 흑자로 보이게 한다
		{"round_start 0", func(f *Fill) { f.RoundStart = 0 }},
		{"round_start 음수", func(f *Fill) { f.RoundStart = -1 }},
		{"market_id 0", func(f *Fill) { f.MarketID = 0 }},
		{"market_id 음수", func(f *Fill) { f.MarketID = -1 }},
		{"outcome 빈 값", func(f *Fill) { f.Outcome = "" }},
		{"outcome 소문자", func(f *Fill) { f.Outcome = "up" }},
		{"outcome 미지", func(f *Fill) { f.Outcome = "Sideways" }},
		{"at 제로값", func(f *Fill) { f.At = time.Time{} }},
	}

	for _, tc := range fillCases {
		t.Run("fill/"+tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.csv")
			l, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer l.Close()
			before, _ := os.ReadFile(path)

			f := sampleFill(1000)
			tc.mut(&f)
			err = l.RecordFill(f)
			if err == nil {
				after, _ := os.ReadFile(path)
				t.Fatalf("기록됐다. 파일:\n%s", after)
			}
			if !errors.Is(err, ErrInvalidRecord) {
				t.Errorf("에러가 ErrInvalidRecord 가 아니다: %v", err)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Errorf("거절인데 파일이 바뀌었다:\n%s", after)
			}
		})
	}

	rebateCases := []struct {
		name string
		r    Rebate
	}{
		{"shares NaN", Rebate{RoundStart: 1000, Shares: nan, At: fixedTime()}},
		{"shares +Inf", Rebate{RoundStart: 1000, Shares: pinf, At: fixedTime()}},
		{"shares -Inf", Rebate{RoundStart: 1000, Shares: ninf, At: fixedTime()}},
		{"shares 음수", Rebate{RoundStart: 1000, Shares: -0.5, At: fixedTime()}},
		{"round_start 0", Rebate{RoundStart: 0, Shares: 0.5, At: fixedTime()}},
		{"at 제로값", Rebate{RoundStart: 1000, Shares: 0.5}},
	}
	for _, tc := range rebateCases {
		t.Run("rebate/"+tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.csv")
			l, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer l.Close()
			before, _ := os.ReadFile(path)
			if err := l.RecordRebate(tc.r); err == nil {
				t.Fatal("기록됐다")
			} else if !errors.Is(err, ErrInvalidRecord) {
				t.Errorf("에러가 ErrInvalidRecord 가 아니다: %v", err)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Errorf("거절인데 파일이 바뀌었다:\n%s", after)
			}
		})
	}

	settleCases := []struct {
		name string
		s    Settlement
	}{
		{"shares NaN", Settlement{RoundStart: 1000, Shares: nan, At: fixedTime()}},
		{"shares +Inf", Settlement{RoundStart: 1000, Shares: pinf, At: fixedTime()}},
		{"shares -Inf", Settlement{RoundStart: 1000, Shares: ninf, At: fixedTime()}},
		{"shares 음수", Settlement{RoundStart: 1000, Shares: -1, At: fixedTime()}},
		{"round_start 0", Settlement{RoundStart: 0, Shares: 1, At: fixedTime()}},
		{"at 제로값", Settlement{RoundStart: 1000, Shares: 1}},
	}
	for _, tc := range settleCases {
		t.Run("settlement/"+tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.csv")
			l, err := Open(path)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer l.Close()
			before, _ := os.ReadFile(path)
			if err := l.RecordSettlement(tc.s); err == nil {
				t.Fatal("기록됐다")
			} else if !errors.Is(err, ErrInvalidRecord) {
				t.Errorf("에러가 ErrInvalidRecord 가 아니다: %v", err)
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Errorf("거절인데 파일이 바뀌었다:\n%s", after)
			}
		})
	}
}

// 거절 규칙이 정상 값까지 막으면 원장이 비어 버린다. 통과해야 하는 것들.
func TestRecordAcceptsLegitimateEdgeValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	// 수수료 0 은 메이커의 정상 상태다.
	f := sampleFill(1000)
	f.FeeUSD = 0
	if err := l.RecordFill(f); err != nil {
		t.Errorf("수수료 0 이 거절됐다: %v", err)
	}
	// Down 도 정상이다.
	f = sampleFill(1000)
	f.Outcome = OutcomeDown
	if err := l.RecordFill(f); err != nil {
		t.Errorf("Down 이 거절됐다: %v", err)
	}
	// 가격 정확히 1.0 은 산술적으로 가능하다(정산 직전).
	f = sampleFill(1000)
	f.PriceUSD = 1
	if err := l.RecordFill(f); err != nil {
		t.Errorf("가격 1.0 이 거절됐다: %v", err)
	}
	// 소수 주식 수.
	f = sampleFill(1000)
	f.Shares = 0.185
	if err := l.RecordFill(f); err != nil {
		t.Errorf("소수 주식 수가 거절됐다: %v", err)
	}
	// 리베이트 0 주는 있을 수 있다 — 자격선 미달. Fill 과 달리 아무것도
	// 왜곡하지 않으므로 기록한다.
	if err := l.RecordRebate(Rebate{RoundStart: 1000, Shares: 0, At: fixedTime()}); err != nil {
		t.Errorf("0 주 리베이트가 거절됐다: %v", err)
	}
	// 아무것도 못 산 회차의 정산도 기록한다.
	if err := l.RecordSettlement(Settlement{RoundStart: 1000, Won: false, Shares: 0, At: fixedTime()}); err != nil {
		t.Errorf("0 주 정산이 거절됐다: %v", err)
	}

	if rows := readRows(t, path); len(rows) != 7 {
		t.Errorf("행 수 = %d, 기대 7(헤더 + 6건)", len(rows))
	}
}

// ---------------------------------------------------------------------------
// 실패해도 죽지 않는다
// ---------------------------------------------------------------------------

// 닫힌 원장에 써도 패닉하지 않는다. 살아 있는 주문을 들고 있는 상태에서
// 패닉하면 취소도 못 하고 죽는다.
func TestRecordAfterCloseErrorsWithoutPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := l.RecordFill(sampleFill(1000)); !errors.Is(err, ErrClosed) {
		t.Errorf("RecordFill 에러 = %v, 기대 ErrClosed", err)
	}
	if err := l.RecordRebate(Rebate{RoundStart: 1000, Shares: 0.5, At: fixedTime()}); !errors.Is(err, ErrClosed) {
		t.Errorf("RecordRebate 에러 = %v, 기대 ErrClosed", err)
	}
	if err := l.RecordSettlement(Settlement{RoundStart: 1000, Shares: 1, At: fixedTime()}); !errors.Is(err, ErrClosed) {
		t.Errorf("RecordSettlement 에러 = %v, 기대 ErrClosed", err)
	}
	// 두 번 닫아도 에러가 아니다 — defer 와 정상 경로가 겹치는 배선이 흔하다.
	if err := l.Close(); err != nil {
		t.Errorf("두 번째 Close = %v, 기대 nil", err)
	}
}

// **nil 원장에 써도 패닉하지 않는다.**
//
// Task 7 의 Runner 는 원장을 `Ledger *ledger.Ledger` 로 들고 있다. 배선이
// 그 필드를 빠뜨리면 첫 체결에서 nil 역참조로 봇이 죽는데, 그 시점에 우리는
// 이미 살아 있는 주문을 들고 있다 — 취소도 못 하고 죽는 것이 최악이다.
func TestNilLedgerErrorsWithoutPanic(t *testing.T) {
	var l *Ledger // 배선이 빠뜨린 상태

	if err := l.RecordFill(sampleFill(1000)); !errors.Is(err, ErrNotOpen) {
		t.Errorf("RecordFill 에러 = %v, 기대 ErrNotOpen", err)
	}
	if err := l.RecordRebate(Rebate{RoundStart: 1000, Shares: 0.5, At: fixedTime()}); !errors.Is(err, ErrNotOpen) {
		t.Errorf("RecordRebate 에러 = %v, 기대 ErrNotOpen", err)
	}
	if err := l.RecordSettlement(Settlement{RoundStart: 1000, Shares: 1, At: fixedTime()}); !errors.Is(err, ErrNotOpen) {
		t.Errorf("RecordSettlement 에러 = %v, 기대 ErrNotOpen", err)
	}
	// defer l.Close() 경로. 여기서 죽으면 종료 처리(미체결 취소)를 못 한다.
	if err := l.Close(); err != nil {
		t.Errorf("nil Close = %v, 기대 nil", err)
	}

	// Open 을 지나지 않은 제로값도 같다.
	var z Ledger
	if err := z.RecordFill(sampleFill(1000)); !errors.Is(err, ErrNotOpen) {
		t.Errorf("제로값 RecordFill 에러 = %v, 기대 ErrNotOpen", err)
	}
	if err := z.Close(); err != nil {
		t.Errorf("제로값 Close = %v, 기대 nil", err)
	}
}

// 망가진 레코드는 파일을 건드리지 않으므로 같은 값으로 다시 불러도 안전하다.
// (그 밖의 I/O 에러는 그렇지 않다 — 패키지 문서 "재시도에 대하여" 참조.)
func TestInvalidRecordIsSafeToRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer l.Close()

	bad := sampleFill(1000)
	bad.Shares = math.NaN()
	for i := 0; i < 3; i++ {
		if err := l.RecordFill(bad); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("%d 번째 에러 = %v, 기대 ErrInvalidRecord", i, err)
		}
	}
	if rows := readRows(t, path); len(rows) != 1 {
		t.Errorf("행 수 = %d, 기대 1(헤더만) — 거절이 반복 가능하지 않다", len(rows))
	}
}

// 열 수 없는 경로에서 에러를 돌려준다. 패닉하지 않는다.
func TestOpenFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	// 디렉터리를 원장 경로로 주면 열리지 않는다.
	if l, err := Open(dir); err == nil {
		l.Close()
		t.Error("디렉터리를 원장으로 열었다")
	}
	// 없는 디렉터리 아래 경로.
	if l, err := Open(filepath.Join(dir, "없는디렉터리", "ledger.csv")); err == nil {
		l.Close()
		t.Error("없는 디렉터리에 원장을 열었다")
	}
}

// 동시에 여러 고루틴이 써도 줄이 섞이지 않는다. `exec` 는 체결 콜백과
// 정산 처리를 다른 고루틴에서 돌릴 수 있다. -race 로 돌린다.
func TestConcurrentRecordsDoNotInterleave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := l.RecordFill(sampleFill(int64(1000 + i))); err != nil {
				t.Errorf("RecordFill %d: %v", i, err)
			}
			if err := l.RecordRebate(Rebate{RoundStart: int64(1000 + i), Shares: 0.5, At: fixedTime()}); err != nil {
				t.Errorf("RecordRebate %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rows := readRows(t, path)
	if len(rows) != 2*n+1 {
		t.Fatalf("행 수 = %d, 기대 %d", len(rows), 2*n+1)
	}
	// 모든 round_start 가 정확히 두 번씩 나온다 — 줄이 섞이면 파싱이
	// 먼저 깨지지만, 깨지지 않더라도 값이 어긋난다.
	seen := map[string]int{}
	for _, r := range rows[1:] {
		seen[r[2]]++
	}
	for i := 0; i < n; i++ {
		k := strconv.Itoa(1000 + i)
		if seen[k] != 2 {
			t.Errorf("round_start %s 가 %d 번 나왔다, 기대 2", k, seen[k])
		}
	}
}
