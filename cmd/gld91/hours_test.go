package main

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// 이 파일이 지키는 것 셋:
//
//	(1) 켜진 시각이 **정확히** 사용자가 고른 12개다
//	(2) 판정이 UTC 로 이뤄진다 — 서버는 도쿄(UTC+9)에 있다
//	(3) 관문이 배선돼 있다 — 함수가 맞아도 호출부가 없으면 아무 일도 안 난다

// 사용자가 고른 12개를 여기 한 번 더 적는다. hours.go 의 배열을 그대로 읽어
// 비교하면 아무것도 검증하지 못한다 — 두 곳이 같은 실수를 하려면 사람이 두 번
// 고쳐야 한다.
var chosen = []int{2, 3, 6, 8, 11, 12, 15, 16, 17, 18, 21, 22}

// errFake 는 스킵 사유 우선순위 시험에서 "무언가 실패했다" 를 나타낸다.
var errFake = errors.New("가짜 실패")

func TestTradingHoursAreExactlyWhatTheUserChose(t *testing.T) {
	var on []int
	for h := 0; h < 24; h++ {
		if tradingHours[h] {
			on = append(on, h)
		}
	}
	sort.Ints(on)
	if len(on) != len(chosen) {
		t.Fatalf("켜진 시각 %v (%d개), 기대 %v (%d개)", on, len(on), chosen, len(chosen))
	}
	for i := range on {
		if on[i] != chosen[i] {
			t.Fatalf("켜진 시각 %v, 기대 %v", on, chosen)
		}
	}
}

// 켜진 시각은 걸고, 꺼진 시각은 걸지 않는다. 24시간 전부를 밟는다 —
// 표본을 고르면 고르지 않은 자리의 오타가 살아남는다.
func TestTradingHourCoversEveryHour(t *testing.T) {
	want := map[int]bool{}
	for _, h := range chosen {
		want[h] = true
	}
	for h := 0; h < 24; h++ {
		at := time.Date(2026, 8, 14, h, 0, 0, 0, time.UTC)
		if got := tradingHour(at); got != want[h] {
			t.Errorf("UTC %02d시: tradingHour=%v, 기대 %v", h, got, want[h])
		}
	}
}

// **시(hour) 안쪽은 전부 같은 판정이다.** 5분봉이므로 한 시각에 회차가 12개
// 들어온다 — 그중 하나만 다르게 판정되면 그 회차만 조용히 빠지거나 끼어든다.
func TestEveryRoundInsideAnHourAgrees(t *testing.T) {
	for _, h := range []int{2, 5} { // 켜진 시각 하나, 꺼진 시각 하나
		want := tradingHour(time.Date(2026, 8, 14, h, 0, 0, 0, time.UTC))
		for m := 0; m < 60; m += 5 {
			at := time.Date(2026, 8, 14, h, m, 0, 0, time.UTC)
			if got := tradingHour(at); got != want {
				t.Errorf("%02d:%02d → %v, 같은 시각의 다른 회차는 %v", h, m, got, want)
			}
		}
		// 시의 마지막 순간까지도 같아야 한다.
		if got := tradingHour(time.Date(2026, 8, 14, h, 59, 59, 999_999_999, time.UTC)); got != want {
			t.Errorf("%02d:59:59.999999999 → %v, 기대 %v", h, got, want)
		}
	}
}

// **UTC 로 잰다.** 서버는 도쿄(UTC+9)에 있다. 로컬 시로 재면 정책이 통째로
// 9시간 밀리고, 그 사실은 로그만 봐서는 드러나지 않는다 — 봇은 조용히
// "11시" 에 걸고 있다고 말하면서 실제로는 02시에 건다.
func TestTradingHourIgnoresTheLocation(t *testing.T) {
	tokyo := time.FixedZone("JST", 9*3600)
	// **로컬 시로 쟀을 때와 답이 갈리는 시각만 고른다.** 둘이 같은 시각을
	// 고르면 UTC 로 재지 않아도 통과한다.
	for _, c := range []struct {
		jst  int
		utc  int
		want bool
	}{
		{jst: 20, utc: 11, want: true},  // 로컬 20시는 꺼짐, UTC 11시는 켜짐
		{jst: 1, utc: 16, want: true},   // 로컬 01시는 꺼짐, UTC 16시는 켜짐
		{jst: 0, utc: 15, want: true},   // 로컬 00시는 꺼짐, UTC 15시는 켜짐
		{jst: 7, utc: 22, want: true},   // 로컬 07시는 꺼짐, UTC 22시는 켜짐
		{jst: 11, utc: 2, want: true},   // 둘 다 켜짐 — 아래에서 걸러진다
		{jst: 22, utc: 13, want: false}, // 로컬 22시는 켜짐, UTC 13시는 꺼짐
		{jst: 12, utc: 3, want: true},   // 로컬 12시는 켜짐, UTC 03시도 켜짐
		{jst: 2, utc: 17, want: true},   // 로컬 02시는 켜짐, UTC 17시도 켜짐
		{jst: 18, utc: 9, want: false},  // 로컬 18시는 켜짐, UTC 09시는 꺼짐
	} {
		at := time.Date(2026, 8, 14, c.jst, 30, 0, 0, tokyo)
		if got := at.UTC().Hour(); got != c.utc {
			t.Fatalf("%02d:30 JST 의 UTC 시각이 %02d 다 — 표가 틀렸다", c.jst, got)
		}
		if got := tradingHour(at); got != c.want {
			t.Errorf("%02d:30 JST (= UTC %02d시): %v, 기대 %v — UTC 로 재지 않았다",
				c.jst, c.utc, got, c.want)
		}
	}
	// 위 표에 **판정이 갈리는 시각이 실제로 들어 있는지** 확인한다. 전부
	// 겹치는 시각뿐이면 이 시험은 아무것도 막지 못한다.
	split := 0
	for _, c := range []struct{ jst, utc int }{
		{20, 11}, {1, 16}, {0, 15}, {7, 22}, {11, 2}, {22, 13}, {12, 3}, {2, 17}, {18, 9},
	} {
		if tradingHours[c.jst] != tradingHours[c.utc] {
			split++
		}
	}
	if split < 4 {
		t.Fatalf("로컬/UTC 판정이 갈리는 표본이 %d개뿐이다 — 표를 고쳐라", split)
	}
}

// ---------------------------------------------------------------------------
// 배선 — 함수가 맞아도 호출부가 없으면 아무 일도 일어나지 않는다
// ---------------------------------------------------------------------------

// runRound 가 관문을 지나야 한다. 이 호출이 빠지면 위 시험들은 전부 통과하고
// 봇은 24시간 내내 건다.
func TestRunRoundPassesTheHourGate(t *testing.T) {
	src := mainSource(t)
	if !strings.Contains(src, "if !tradingHour(r.StartsAt) {") {
		t.Fatal("runRound 이 tradingHour 를 부르지 않는다 — 시간대 관문이 배선되지 않았다")
	}
	// **회차 시작 시각으로 재야 한다.** time.Now() 로 재면 회차 경계에서
	// 앞뒤 회차가 갈리고, 늦게 잡은 회차는 다음 시각으로 판정된다.
	if strings.Contains(src, "tradingHour(time.Now())") {
		t.Error("관문이 time.Now() 를 쓴다 — 회차 시작(r.StartsAt)으로 재야 한다")
	}
	// equity 조회보다 먼저여야 한다. 뒤에 있으면 걸지도 않을 회차에
	// REST 왕복을 쓴다.
	gate := strings.Index(src, "if !tradingHour(r.StartsAt) {")
	read := strings.Index(src, "ahead.read(ctx, equitySrc, cfg, time.Now())")
	if read >= 0 && gate > read {
		t.Error("관문이 equity 조회 뒤에 있다 — 걸지 않을 회차에 REST 를 쓴다")
	}
	// 감시에 사유가 올라가야 한다. 안 올리면 모니터가 "조용한 봇" 과
	// 구분하지 못한다 — 이 봇의 실패 모드가 정확히 그것이다.
	if !strings.Contains(src, "OutsideHours: true") {
		t.Error("관문이 감시에 사유를 올리지 않는다 — 모니터가 고장과 구분하지 못한다")
	}
}

// 스킵 사유가 감시 계약에 실려야 한다. equity 는 이 경로에서 조회하지 않으므로
// 제로값인데, 순서가 뒤면 그것이 "equity 부족" 으로 보고된다 — 정상 스킵이
// 경고로 둔갑한다.
func TestOutsideHoursOutranksTheEmptyEquity(t *testing.T) {
	in := snapshotInput{Round: live.Round{}, Frozen: live.Frozen{Eligible: true}, OutsideHours: true}
	state, why := roundState(in)
	if state != beat.RoundSkipped {
		t.Errorf("상태 %q, 기대 SKIPPED", state)
	}
	if why != beat.SkipOutsideHours {
		t.Errorf("사유 %q, 기대 %q — equity 제로값이 이겼다면 순서가 틀렸다", why, beat.SkipOutsideHours)
	}
}

// 더 심각한 사유는 시간대를 이긴다. 일손실 한도에 걸린 회차가 마침 꺼진
// 시각이면 "거래 시간대가 아니다" 로 보고되는데, 그건 정상이라는 뜻이라
// 모니터가 조용해진다.
func TestSeriousReasonsBeatTheHourGate(t *testing.T) {
	for _, c := range []struct {
		name string
		in   snapshotInput
		want beat.SkipReason
	}{
		{"일손실 한도", snapshotInput{OutsideHours: true, DailyBreached: true, Equity: armable()}, beat.SkipDailyLimit},
		{"p_up 실패", snapshotInput{OutsideHours: true, PredictErr: errFake}, beat.SkipPredictError},
		{"회차 조회 실패", snapshotInput{OutsideHours: true, FetchErr: errFake}, beat.SkipFetchError},
	} {
		if _, why := roundState(c.in); why != c.want {
			t.Errorf("%s: 사유 %q, 기대 %q", c.name, why, c.want)
		}
	}
}

// 모니터가 아는 사유여야 한다. 모르면 "⚠️ 모니터가 모르는 사유" 로 떨어지고,
// 하루의 절반이 그 상태가 된다.
func TestOutsideHoursIsAKnownReason(t *testing.T) {
	if !beat.SkipOutsideHours.Valid() {
		t.Fatal("SkipOutsideHours 가 Valid() 가 아니다 — 모니터가 알람으로 다룬다")
	}
}

// equity 제로값이 실제로 CanArm 을 못 넘는지 본다. 넘어 버리면 위
// TestOutsideHoursOutranksTheEmptyEquity 가 순서와 무관하게 통과한다.
func TestEmptyEquityWouldOtherwiseReportEquitySkip(t *testing.T) {
	if risk.CanArm(risk.Equity{}) {
		t.Skip("제로 equity 로도 무장 가능하다 — 이 시험의 전제가 사라졌다")
	}
	if _, why := roundState(snapshotInput{Frozen: live.Frozen{Eligible: true}}); why != beat.SkipEquity {
		t.Fatalf("사유 %q, 기대 %q — 순서 시험의 전제가 깨졌다", why, beat.SkipEquity)
	}
}
