package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

func boolp(b bool) *bool { return &b }

// **손익분기 승률 = 평균 진입가.** 리베이트는 반대편 주식으로 오고 우리가
// 질 때만 값이 붙으므로 손익분기를 **낮춘다** — 방향을 뒤집으면 pmmm-go 에서
// +40 을 −90 으로 보고한 그 사고가 된다.
func TestBreakevenLoweredByRebate(t *testing.T) {
	plain := breakeven(0.487, 0)
	if math.Abs(plain-0.487) > 1e-9 {
		t.Errorf("리베이트 없으면 손익분기 = 평균 진입가여야 한다, got %.6f", plain)
	}
	withRebate := breakeven(0.487, 0.005)
	if !(withRebate < plain) {
		t.Errorf("리베이트 포함 %.6f 가 미포함 %.6f 보다 낮지 않다 — 방향이 뒤집혔다", withRebate, plain)
	}
	for _, bad := range [][2]float64{{0, 0}, {1, 0}, {-0.1, 0}, {0.5, 1}} {
		if !math.IsNaN(breakeven(bad[0], bad[1])) {
			t.Errorf("잘못된 입력 %v 에 값이 나왔다", bad)
		}
	}
}

// 이기면 주당 $1. 지면 0. 미정산이면 계산하지 않는다.
func TestParticipationPnL(t *testing.T) {
	win := participation{Cost: 9.4, Shares: 20, Settled: true, Won: true}
	if pnl, ok := win.PnL(); !ok || math.Abs(pnl-10.6) > 1e-9 {
		t.Errorf("이긴 회차 손익 = %v (ok=%v), want +10.6", pnl, ok)
	}
	lose := participation{Cost: 9.4, Shares: 20, Settled: true, Won: false}
	if pnl, _ := lose.PnL(); math.Abs(pnl+9.4) > 1e-9 {
		t.Errorf("진 회차 손익 = %v, want -9.4", pnl)
	}
	if _, ok := (participation{Cost: 9.4, Shares: 20}).PnL(); ok {
		t.Error("미정산 회차가 손익을 냈다")
	}
}

// **미정산 회차는 적중률 분모에 들어가면 안 된다.** 들어가면 아직 결과를
// 모르는 회차가 전부 패배로 계산되어 승률이 조용히 낮아진다.
func TestUnsettledExcludedFromHitRate(t *testing.T) {
	tl := accumulate([]participation{
		{Slug: "a", Cost: 5, Shares: 10, Settled: true, Won: true},
		{Slug: "b", Cost: 5, Shares: 10}, // 미정산
	})
	if tl.Settled != 1 || tl.Hits != 1 {
		t.Errorf("정산 %d 적중 %d, want 1/1", tl.Settled, tl.Hits)
	}
	if tl.Pending != 1 {
		t.Errorf("대기 %d, want 1", tl.Pending)
	}
	if tl.Participated != 2 {
		t.Errorf("참여 %d, want 2", tl.Participated)
	}
	// 미정산의 손익은 더해지지 않는다.
	if math.Abs(tl.PnL-5) > 1e-9 {
		t.Errorf("손익 %v, want +5", tl.PnL)
	}
}

// G2 가 잰 정산 불일치를 실거래에서 다시 잰다 — 거래소 정산 방향과 거래소가
// 기록한 가격의 방향이 어긋나는 경우다.
func TestSettlementMismatchCounted(t *testing.T) {
	tl := accumulate([]participation{
		{Slug: "a", Settled: true, WonName: "Up", ChainlinkUp: boolp(true)},
		{Slug: "b", Settled: true, WonName: "Down", ChainlinkUp: boolp(true)}, // 불일치
		{Slug: "c", Settled: true, WonName: "Down", ChainlinkUp: boolp(false)},
	})
	if tl.Mismatch != 1 || tl.MismatchOf != 3 {
		t.Errorf("불일치 %d/%d, want 1/3", tl.Mismatch, tl.MismatchOf)
	}
	// 가격이 없으면 분모에도 안 들어간다 — 모르는 것을 일치로 세면 안 된다.
	tl = accumulate([]participation{{Slug: "a", Settled: true, WonName: "Up", PriceMissing: true}})
	if tl.MismatchOf != 0 {
		t.Errorf("가격 없는 회차가 분모에 들어갔다: %d", tl.MismatchOf)
	}
}

// 적중률에는 반드시 n 과 신뢰구간이 붙는다. 61 표본의 54.1% 는 그 자체로
// 아무 말도 하지 않는다.
func TestReportIncludesSampleSizeAndInterval(t *testing.T) {
	var ps []participation
	for i := 0; i < 61; i++ {
		p := participation{Slug: string(rune('a' + i%26)), Cost: 4.87, Shares: 10, Settled: true}
		p.Won = i < 33
		ps = append(ps, p)
	}
	out := formatReport(accumulate(ps), nil, "누적")
	for _, want := range []string{"n=61", "54.1%", "CI", "손익분기"} {
		if !strings.Contains(out, want) {
			t.Errorf("리포트에 %q 가 없다:\n%s", want, out)
		}
	}
}

// **표본이 0 이면 비율을 계산하지 않는다.** 0/0 은 NaN 이고, NaN 이 리포트에
// 실리면 사람은 그것을 손실로 읽는다.
func TestZeroSampleProducesNoNaN(t *testing.T) {
	for _, ps := range [][]participation{nil, {{Slug: "a"}}} {
		out := formatReport(accumulate(ps), nil, "누적")
		if strings.Contains(out, "NaN") {
			t.Errorf("리포트에 NaN 이 있다:\n%s", out)
		}
	}
}

// 리베이트가 집계에 없다는 것을 숨기지 않는다 — 없는 것을 0 으로 세면
// "리베이트 0" 이 사실처럼 읽힌다.
func TestReportAdmitsMissingRebate(t *testing.T) {
	if out := formatReport(accumulate(nil), nil, "누적"); !strings.Contains(out, "리베이트는 집계에 없") {
		t.Errorf("리베이트 부재를 숨긴다:\n%s", out)
	}
}

// 작은 표본에는 경고가 붙는다. 이 봇의 실거래 첫날 n 은 작다.
func TestSmallSampleWarned(t *testing.T) {
	ps := []participation{{Slug: "a", Cost: 5, Shares: 10, Settled: true, Won: true}}
	if out := formatReport(accumulate(ps), nil, "누적"); !strings.Contains(out, "표본이 작") {
		t.Errorf("작은 표본 경고가 없다:\n%s", out)
	}
}

// --- 상태에 붙는 부분 ---

// **걸지 않은 회차는 이력에 남기지 않는다.** confidence 미달로 건너뛴 회차가
// 참여로 세어지면 참여율과 적중률 분모가 통째로 틀린다.
func TestOnlyFilledRoundsRecorded(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.Seq, s.BootID = 1, "a"
	s.Round.Slug, s.Round.State, s.Round.SkipReason = "r-skip", beat.RoundSkipped, beat.SkipConfBelow
	s.Exposure.Filled, s.Exposure.FilledShares = 0, 0
	post(t, st, *s)

	s.Seq, s.Round.Slug, s.Round.State, s.Round.SkipReason = 2, "r-fill", beat.RoundActive, ""
	s.Round.Outcome = "Up"
	s.Exposure.Filled, s.Exposure.FilledShares = 9.4, 20
	post(t, st, *s)

	ps := st.Participations()
	if len(ps) != 1 || ps[0].Slug != "r-fill" {
		t.Fatalf("이력 = %+v, want r-fill 하나", ps)
	}
	if ps[0].Shares != 20 || ps[0].Cost != 9.4 || ps[0].Outcome != "Up" {
		t.Errorf("이력 값이 틀리다: %+v", ps[0])
	}
}

// 정산 결과가 우리가 건 방향과 맞으면 승리다. 이름 비교는 대소문자를 무시한다.
func TestApplySettlementsMatchesDirection(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.Seq, s.BootID, s.Round.Slug, s.Round.Outcome = 1, "a", "r1", "Up"
	s.Exposure.Filled, s.Exposure.FilledShares = 9.4, 20
	post(t, st, *s)

	st.ApplySettlements([]settlement{
		{Slug: "r1", WonName: "UP", StartPrice: 100, EndPrice: 101},
		{Slug: "없는회차", WonName: "Down"},
	})
	ps := st.Participations()
	if len(ps) != 1 {
		t.Fatalf("이력 %d건", len(ps))
	}
	if !ps[0].Settled || !ps[0].Won {
		t.Errorf("정산/승리가 안 잡혔다: %+v", ps[0])
	}
	if ps[0].ChainlinkUp == nil || !*ps[0].ChainlinkUp {
		t.Errorf("가격 방향이 안 잡혔다: %+v", ps[0].ChainlinkUp)
	}
}

func TestApplySettlementsLoss(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.Seq, s.BootID, s.Round.Slug, s.Round.Outcome = 1, "a", "r1", "Up"
	s.Exposure.Filled, s.Exposure.FilledShares = 9.4, 20
	post(t, st, *s)

	st.ApplySettlements([]settlement{{Slug: "r1", WonName: "Down", StartPrice: 101, EndPrice: 100}})
	ps := st.Participations()
	if !ps[0].Settled || ps[0].Won {
		t.Errorf("졌는데 이긴 것으로 잡혔다: %+v", ps[0])
	}
	if pnl, _ := ps[0].PnL(); math.Abs(pnl+9.4) > 1e-9 {
		t.Errorf("진 회차 손익 = %v, want -9.4", pnl)
	}
}

// 가격이 없으면 방향 비교를 하지 않는다 — 모르는 것을 일치로 세면 G2 검증이
// 거짓 안심을 준다.
func TestApplySettlementsMissingPrices(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.Seq, s.BootID, s.Round.Slug, s.Round.Outcome = 1, "a", "r1", "Up"
	s.Exposure.Filled, s.Exposure.FilledShares = 9.4, 20
	post(t, st, *s)

	st.ApplySettlements([]settlement{{Slug: "r1", WonName: "Up"}})
	ps := st.Participations()
	if !ps[0].PriceMissing || ps[0].ChainlinkUp != nil {
		t.Errorf("가격 없는데 방향이 잡혔다: %+v", ps[0])
	}
	if n := accumulate(ps).MismatchOf; n != 0 {
		t.Errorf("가격 없는 회차가 정합 분모에 들어갔다: %d", n)
	}
}

func TestParticipationsAreSorted(t *testing.T) {
	st := newTestState()
	for i, slug := range []string{"r-c", "r-a", "r-b"} {
		s := healthySnapshot()
		s.Seq, s.BootID, s.Round.Slug = uint64(i+1), "a", slug
		s.Exposure.Filled, s.Exposure.FilledShares = 1, 2
		post(t, st, *s)
	}
	ps := st.Participations()
	for i := 1; i < len(ps); i++ {
		if ps[i-1].Slug > ps[i].Slug {
			t.Fatalf("정렬되지 않았다: %v", ps)
		}
	}
}

// /pnl 이 실제로 응답한다.
func TestPnlCommand(t *testing.T) {
	st := stateWith(t, nil)
	reply, handled := routeCommand("/pnl", st, time.Now())
	if !handled {
		t.Fatal("/pnl 이 처리되지 않았다")
	}
	if !strings.Contains(reply, "리포트") {
		t.Errorf("응답 = %q", reply)
	}
}

// ApplySettlements 는 **실제로 반영된 건수**를 돌려준다. 받은 건수가 아니라
// 이것이 정산 경로가 도는지를 말해 준다 — 우리가 걸지 않은 회차는 아무리
// 받아도 0 이다. 성공이 조용하면 그 경로를 확인할 방법이 없다.
func TestApplySettlementsReportsAppliedCount(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.Seq, s.BootID, s.Round.Slug, s.Round.Outcome = 1, "a", "r1", "Up"
	s.Exposure.Filled, s.Exposure.FilledShares = 9.4, 20
	post(t, st, *s)

	// 우리가 걸지 않은 회차만 오면 반영 0 이다.
	if n := st.ApplySettlements([]settlement{{Slug: "남의회차", WonName: "Up"}}); n != 0 {
		t.Errorf("남의 회차에 반영 %d, want 0", n)
	}
	if n := st.ApplySettlements([]settlement{{Slug: "r1", WonName: "Up"}}); n != 1 {
		t.Errorf("반영 %d, want 1", n)
	}
	// 이미 정산된 것을 다시 받아도 새로 반영되지 않는다.
	if n := st.ApplySettlements([]settlement{{Slug: "r1", WonName: "Up"}}); n != 0 {
		t.Errorf("중복 반영 %d, want 0", n)
	}
}
