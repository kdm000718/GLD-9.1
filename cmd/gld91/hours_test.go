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
//	(1) 켜진 시각이 **정확히** 사용자가 고른 24개다
//	(2) 판정이 UTC 로 이뤄진다 — 서버는 도쿄(UTC+9)에 있다
//	(3) 관문이 배선돼 있다 — 함수가 맞아도 호출부가 없으면 아무 일도 안 난다

// 사용자가 **제외한** 시각을 여기 한 번 더 적는다. hours.go 의 배열을 그대로
// 읽어 비교하면 아무것도 검증하지 못한다 — 두 곳이 같은 실수를 하려면 사람이
// 두 번 고쳐야 한다.
//
// 2026-08-16 사용자 결정으로 제외가 없다. 시간대 선택이 과최적화였다는 것이
// 확인됐다(hours.go 주석 참고). 다시 제한하려면 여기와 hours.go 를 함께 고쳐라.
var excluded = []int{}

// chosen 은 그 나머지 — 지금은 24개 전부다.
var chosen = func() []int {
	var out []int
	for h := 0; h < 24; h++ {
		skip := false
		for _, e := range excluded {
			if h == e {
				skip = true
			}
		}
		if !skip {
			out = append(out, h)
		}
	}
	return out
}()

// errFake 는 스킵 사유 우선순위 시험에서 "무언가 실패했다" 를 나타낸다.
var errFake = errors.New("가짜 실패")

func TestTradingHoursAreExactlyWhatTheUserChose(t *testing.T) {
	if len(chosen) != 24-len(excluded) {
		t.Fatalf("켜진 시각이 %d개여야 한다 (24 − 제외 %d)", 24-len(excluded), len(excluded))
	}
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
	// **24시간 전부를 밟는다.** 표가 균일한 동안에는 몇 시각만 골라도 통과하고,
	// 나중에 다시 시간대를 끄면 그 자리의 경계 오류가 살아남는다.
	for h := 0; h < 24; h++ {
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
// **첨자로 잰다.** 표가 24개 전부 true 인 동안에는 tradingHour 의 답만 봐서는
// UTC 로 쟀는지 알 수 없다 — 로컬 시로 재도 답이 같기 때문이다. 그래서
// [tradingHourIndex] 를 직접 본다. 이 시험은 표의 내용과 무관하게 유효하다.
func TestTradingHourIgnoresTheLocation(t *testing.T) {
	for _, z := range []struct {
		name   string
		offset int
	}{
		{"JST(도쿄, 서버 위치)", 9 * 3600},
		{"KST(서울)", 9 * 3600},
		{"EST", -5 * 3600},
		{"IST(+5:30, 30분 오프셋)", 5*3600 + 1800},
		{"UTC", 0},
	} {
		loc := time.FixedZone(z.name, z.offset)
		for h := 0; h < 24; h++ {
			at := time.Date(2026, 8, 14, h, 30, 0, 0, loc)
			want := at.UTC().Hour()
			if got := tradingHourIndex(at); got != want {
				t.Errorf("%s %02d:30 → 첨자 %d, 기대 %d — UTC 로 재지 않았다",
					z.name, h, got, want)
			}
			// 로컬 시를 그대로 쓰면 오프셋이 0 이 아닌 곳에서 반드시 갈린다.
			if z.offset != 0 && tradingHourIndex(at) == h && at.UTC().Hour() != h {
				t.Errorf("%s %02d:30 → 로컬 시 %d 를 그대로 썼다", z.name, h, h)
			}
		}
	}
	// 배선도 본다 — 첨자가 맞아도 tradingHour 가 그것을 쓰지 않으면 의미가 없다.
	tokyo := time.FixedZone("JST", 9*3600)
	for h := 0; h < 24; h++ {
		at := time.Date(2026, 8, 14, h, 30, 0, 0, tokyo)
		if got, want := tradingHour(at), tradingHours[at.UTC().Hour()]; got != want {
			t.Errorf("JST %02d:30: tradingHour=%v, 표의 UTC %02d시 칸은 %v",
				h, got, at.UTC().Hour(), want)
		}
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
	// **미리 받아 둔 자본도 함께 올려야 한다.** 비우면 감시가 cap 0.00 을
	// "자본이 말랐다" 로 읽고 하루 절반을 경고한다(2026-08-14 실측 144회/일).
	if !strings.Contains(src, "cachedEq, _, _ := ahead.cached(time.Now())") {
		t.Error("관문이 미리 받아 둔 자본을 감시에 올리지 않는다 — cap 0.00 오경보가 난다")
	}
	if !strings.Contains(src, "Equity: cachedEq, OutsideHours: true") {
		t.Error("보고에 Equity 가 실리지 않는다")
	}
	// 조회는 하지 않아야 한다 — 그것이 이 관문이 equity 앞에 있는 이유다.
	gate2 := strings.Index(src, "cachedEq, _, _ := ahead.cached(time.Now())")
	if r := strings.Index(src, "ahead.read(ctx, equitySrc, cfg, time.Now())"); r >= 0 && gate2 > r {
		t.Error("캐시 읽기가 동기 조회 뒤에 있다")
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
