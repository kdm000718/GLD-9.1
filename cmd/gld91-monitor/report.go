package main

// 이 파일은 참여한 회차를 모아 손익을 낸다.
//
// # 모니터가 회차 이력을 들고 있어야 하는 이유
//
// **모니터는 봇의 원장을 읽을 수 없다** — 다른 호스트에 있고, 개인키 없이는
// 조회 API 도 못 쓴다. 그래서 beat 로 지나가는 회차를 여기서 누적한다.
// 프로세스가 죽으면 이력을 잃는다(디스크에 쓰지 않는다). 7일 조회 같은 것이
// 필요해지면 그때 저장소를 붙인다 — 지금 넣으면 검증되지 않은 스키마가 굳는다.
//
// # 손익 공식
//
// `internal/ledger` 의 규약 그대로다:
//
//	순손익 = −FillCost + SettlementProceeds + RebateValue
//
// 이기면 주당 $1 이므로 배당은 **주수**로만 정해진다. 리베이트는 여기서 빼
// 두었다 — 반대편 outcome 주식으로 들어오고 우리가 **질 때만** 값이 붙는데
// (ledger 문서), 그 지급을 관측할 경로가 아직 없다. 없는 것을 0 으로 세지
// 않고 리포트에 그렇게 적는다.

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// participation 은 우리가 실제로 건 회차 하나다.
type participation struct {
	Slug     string
	Outcome  string // "Up" | "Down"
	Cost     float64
	Shares   float64
	EndsAt   time.Time
	Settled  bool
	Won      bool
	WonName  string
	SettleAt time.Time
	// ChainlinkUp 은 거래소가 기록한 가격으로 본 방향이다. 정산 방향과
	// 비교해 G2 의 불일치를 실거래에서 다시 잰다.
	ChainlinkUp  *bool
	PriceMissing bool
}

// PnL 은 정산된 회차의 순손익이다. 미정산이면 (0,false).
func (p participation) PnL() (float64, bool) {
	if !p.Settled {
		return 0, false
	}
	proceeds := 0.0
	if p.Won {
		proceeds = p.Shares // 주당 $1
	}
	return proceeds - p.Cost, true
}

// tally 는 집계 결과다.
type tally struct {
	Participated int
	Settled      int
	Hits         int
	PnL          float64
	Cost         float64
	Shares       float64
	Mismatch     int
	MismatchOf   int
	Pending      int
	// Since 는 이력에서 가장 오래된 회차의 종료 시각이다. 누적이 **언제부터**
	// 인지를 말하는 값이다 — 이력이 디스크에 남게 된 뒤로는 "모니터 기동
	// 이후" 가 더 이상 참이 아니다.
	Since time.Time
}

// AvgEntry 는 평균 진입가(주당)다. 주수가 0 이면 계산하지 않는다 — 0/0 은
// NaN 이고, NaN 이 리포트에 실리면 사람은 그것을 손실로 읽는다.
func (t tally) AvgEntry() (float64, bool) {
	if t.Shares <= 0 {
		return 0, false
	}
	return t.Cost / t.Shares, true
}

// breakeven 은 손익분기 승률이다.
//
// 주당 p 에 사서 이기면 $1 을 받으므로 리베이트가 없으면 손익분기는 정확히
// p 다. 리베이트는 **반대편 주식**으로 오고 우리가 질 때만 값이 붙으므로
// (ledger 문서) 지는 쪽 손실을 줄인다 — 즉 손익분기를 **낮춘다**. 방향을
// 뒤집으면 `~/kdm/pmmm-go` 에서 +40 을 −90 으로 보고한 그 사고가 된다.
func breakeven(avgEntry, rebateRate float64) float64 {
	if avgEntry <= 0 || avgEntry >= 1 || rebateRate < 0 || rebateRate >= 1 {
		return math.NaN()
	}
	return (avgEntry - rebateRate) / (1 - rebateRate)
}

// wilson 은 이항비율의 95% 신뢰구간이다.
//
// 정규근사를 쓰지 않는 이유: n 이 작거나 p 가 0·1 에 가까우면 구간이 [0,1]
// 밖으로 나가는데, **실거래 첫날의 n 은 작다.**
func wilson(hits, n int) (lo, hi float64) {
	if n <= 0 {
		return math.NaN(), math.NaN()
	}
	const z = 1.96
	fn := float64(n)
	p := float64(hits) / fn
	den := 1 + z*z/fn
	centre := (p + z*z/(2*fn)) / den
	half := z * math.Sqrt(p*(1-p)/fn+z*z/(4*fn*fn)) / den
	return centre - half, centre + half
}

// accumulate 는 참여 회차들을 집계한다.
//
// **미정산 회차는 적중률 분모에 들어가지 않는다.** 들어가면 아직 결과를
// 모르는 회차가 전부 패배로 계산되어 승률이 조용히 낮아지고, 5분 회차라
// 언제나 몇 건은 미정산이다.
func accumulate(ps []participation) tally {
	var t tally
	for _, p := range ps {
		t.Participated++
		if !p.EndsAt.IsZero() && (t.Since.IsZero() || p.EndsAt.Before(t.Since)) {
			t.Since = p.EndsAt
		}
		t.Cost += p.Cost
		t.Shares += p.Shares
		if p.ChainlinkUp != nil && !p.PriceMissing {
			t.MismatchOf++
			// 거래소 정산 방향과 거래소 기록 가격의 방향이 어긋나는지 본다.
			if p.Settled && (*p.ChainlinkUp) != strings.EqualFold(p.WonName, "Up") {
				t.Mismatch++
			}
		}
		if !p.Settled {
			t.Pending++
			continue
		}
		t.Settled++
		if p.Won {
			t.Hits++
		}
		if pnl, ok := p.PnL(); ok {
			t.PnL += pnl
		}
	}
	return t
}

// formatReport 는 사람이 읽을 리포트다.
func formatReport(t tally, snap *beat.Snapshot, window string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📄 GLD-9.1 리포트 (%s)\n", window)

	if snap != nil {
		fmt.Fprintf(&b, "💰 가용 %.2f / 미정산 취득원가 %.2f USDT · 회차상한 %.2f\n",
			snap.Equity.AvailableUSDT, snap.Equity.PositionCost, snap.Equity.CapUSD)
		fmt.Fprintf(&b, "🛡 %s · 예산 %d/240\n", armedLabel(snap.Armed), snap.Loop.RateLimitRemaining)
	}

	fmt.Fprintf(&b, "📊 참여 %d회차 (정산 %d · 대기 %d)\n", t.Participated, t.Settled, t.Pending)

	// **표본이 0 이면 비율을 계산하지 않는다.** 0/0 은 NaN 이고, NaN 이 리포트에
	// 실리면 사람은 그것을 손실로 읽는다.
	if t.Settled > 0 {
		lo, hi := wilson(t.Hits, t.Settled)
		rate := 100 * float64(t.Hits) / float64(t.Settled)
		fmt.Fprintf(&b, "🎯 적중 %d/%d = %.1f%%  (n=%d, 95%% CI %.1f~%.1f%%)\n",
			t.Hits, t.Settled, rate, t.Settled, 100*lo, 100*hi)
		fmt.Fprintf(&b, "💵 손익 %+.4f USDT\n", t.PnL)
	} else {
		b.WriteString("🎯 아직 정산된 회차가 없습니다.\n")
	}

	if avg, ok := t.AvgEntry(); ok {
		be := breakeven(avg, 0)
		fmt.Fprintf(&b, "📈 평균 진입가 %.4f → 손익분기 승률 %.1f%%\n", avg, 100*be)
		if t.Settled > 0 {
			rate := float64(t.Hits) / float64(t.Settled)
			fmt.Fprintf(&b, "   여유 %+.1f%%p (적중률 − 손익분기)\n", 100*(rate-be))
		}
	}

	if t.MismatchOf > 0 {
		fmt.Fprintf(&b, "🔬 정산 정합: 불일치 %d/%d (G2 가정 d≈0.30%%)\n", t.Mismatch, t.MismatchOf)
	}

	// 없는 것을 0 으로 세지 않는다.
	b.WriteString("ℹ️ 리베이트는 집계에 없습니다 — 반대편 주식으로 지급되고 관측 경로가 아직 없습니다.\n")
	if t.Settled > 0 && t.Settled < 30 {
		b.WriteString("⚠️ 표본이 작습니다. 이 숫자로 전략을 판단하지 마세요.")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// 상태에 붙는 부분
// ---------------------------------------------------------------------------

// observeRoundLocked 는 beat 하나에서 참여 회차를 갱신한다.
//
// 회차마다 계속 덮어쓴다 — 마지막 값이 그 회차의 최종 체결이다.
//
// **바뀐 것이 있을 때만 참을 돌려준다.** beat 는 3초마다 오는데 한 회차의
// 체결은 몇 번 변하고 만다. 매 beat 마다 참을 돌려주면 디스크에 이력을 3초마다
// 쓰게 되고, 그 쓰기는 아무것도 새로 담지 않는다.
func (s *state) observeRoundLocked(snap beat.Snapshot) (changed bool) {
	slug := snap.Round.Slug
	if slug == "" || snap.Exposure.FilledShares <= 0 {
		return false // 걸지 않은 회차는 이력에 남기지 않는다
	}
	p, ok := s.rounds[slug]
	if !ok {
		p = &participation{Slug: slug}
		s.rounds[slug] = p
		changed = true
	}
	if p.Outcome != snap.Round.Outcome || p.Cost != snap.Exposure.Filled ||
		p.Shares != snap.Exposure.FilledShares || !p.EndsAt.Equal(snap.Round.EndsAt) {
		changed = true
	}
	p.Outcome = snap.Round.Outcome
	p.Cost = snap.Exposure.Filled
	p.Shares = snap.Exposure.FilledShares
	p.EndsAt = snap.Round.EndsAt
	return changed
}

// attachStore 는 저장소를 붙이고 저장된 이력을 읽어 들인다.
//
// **기동 때 한 번만 부른다.** 읽어 들인 회차는 메모리의 같은 슬러그를 덮지
// 않는다 — beat 로 방금 본 것이 파일보다 새롭다.
func (s *state) attachStore(t *store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = t
	for _, p := range t.load() {
		if _, ok := s.rounds[p.Slug]; ok {
			continue
		}
		cp := p
		s.rounds[p.Slug] = &cp
	}
}

// persist 는 지금 이력을 디스크에 쓴다.
//
// **잠금을 잡은 채로 부르면 안 된다.** 파일 쓰기는 잠금 밖에서 한다 — 안
// 그러면 디스크가 느린 순간에 beat 수신이 통째로 막힌다.
func (s *state) persist() {
	s.mu.Lock()
	t := s.store
	s.mu.Unlock()
	if !t.enabled() {
		return
	}
	if err := t.save(s.Participations()); err != nil {
		// 저장 실패는 감시를 멈추지 않는다. 다만 조용하면 안 된다 —
		// 다음 재기동에서 누적이 0 이 되는 이유가 여기에 있다.
		t.log("이력 저장 실패 — 재기동하면 이만큼을 잃는다: %v", err)
	}
}

// ApplySettlements 는 관측한 정산 결과를 참여 회차에 맞춘다.
//
// **indexSet 이 아니라 이름으로 맞추는 이유**: 봇이 스냅샷에 싣는 방향은
// `ledger.OutcomeUp`/`OutcomeDown` 문자열이고, indexSet 은 그 회차의 outcomes
// 배열에서만 의미가 있다(모니터는 그 배열을 안 받는다). 대소문자는 무시한다.
// 돌려주는 것은 **실제로 반영된 건수**다. 받은 건수가 아니라 이것이 정산
// 경로가 도는지를 말해 준다 — 우리가 걸지 않은 회차는 아무리 받아도 0 이다.
// **저장은 이 함수가 스스로 한다.** 호출자가 기억해야 하는 구조로 두면 새
// 호출자가 생겼을 때 정산이 조용히 디스크에 안 남고, 그 사실은 다음 재기동에서
// 승률이 낮아지는 모습으로만 나타난다.
func (s *state) ApplySettlements(ss []settlement) int {
	applied := s.applySettlementsLocked(ss)
	if applied > 0 {
		s.persist() // 잠금 밖이다
	}
	return applied
}

func (s *state) applySettlementsLocked(ss []settlement) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	applied := 0
	for _, x := range ss {
		p, ok := s.rounds[x.Slug]
		if !ok {
			continue // 우리가 걸지 않은 회차다
		}
		if !p.Settled {
			applied++
		}
		p.Settled = true
		p.WonName = x.WonName
		p.Won = strings.EqualFold(p.Outcome, x.WonName)
		p.SettleAt = x.SettledAt
		if x.StartPrice > 0 && x.EndPrice > 0 {
			up := x.EndPrice > x.StartPrice
			p.ChainlinkUp = &up
		} else {
			p.PriceMissing = true
		}
	}
	return applied
}

// OldestUnsettled 는 아직 정산되지 않은 참여 회차 중 가장 오래된 것의 시작
// 유닉스초와 그 개수다. 개수가 0 이면 조회할 이유가 없다.
//
// **조회 구간을 고정 24시간으로 잡으면 안 된다.** 맞출 회차가 두 개뿐인데
// 하루치를 훑으면 페이지가 늘고, 그만큼 봇의 예산을 먹는다. 필요한 만큼만
// 거슬러 간다.
func (s *state) OldestUnsettled() (sinceUnix int64, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldest := int64(0)
	for slug, p := range s.rounds {
		if p.Settled {
			continue
		}
		start, ok := rest.ParseSlugStart(slug)
		if !ok {
			continue
		}
		count++
		if oldest == 0 || start < oldest {
			oldest = start
		}
	}
	return oldest, count
}

// Participations 는 이력의 복사본이다. 정렬해 돌려준다 — 리포트가 결정적이어야
// 로그 대조가 된다.
func (s *state) Participations() []participation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]participation, 0, len(s.rounds))
	for _, p := range s.rounds {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}
