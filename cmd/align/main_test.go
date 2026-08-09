package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/klines"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

func round(slug string, start int64, sp, ep float64) rest.Round {
	return rest.Round{Slug: slug, StartUnix: start, StartPrice: sp, EndPrice: ep, Symbol: "BTCUSDT"}
}

func bar(start int64, o, c float64) klines.Kline {
	return klines.Kline{OpenTime: start * 1000, Open: o, Close: c}
}

// 5분 상품만 남긴 뒤로 슬롯 충돌은 정상 경로에 없다. 그래도 compare 는 마지막
// 방어선이다 — n 은 표준오차의 분모라서, 한 슬롯을 두 번 세면 표본이 실제보다
// 커 보이고 ±SE 가 과소평가된다. G2 로 발표된 앞의 두 숫자가 그 경우였다.
func TestCompareCountsEachSlotOnce(t *testing.T) {
	byOpen := map[int64]klines.Kline{
		300: bar(300, 100, 101), // 상승
		600: bar(600, 100, 99),  // 하락
	}
	rounds := []rest.Round{
		round("btc-updown-5m-600", 600, 100, 99),  // 일치
		round("btc-updown-5m-300", 300, 100, 101), // 일치
		round("btc-updown-5m-300-b", 300, 100, 101),
		round("btc-updown-5m-300-c", 300, 100, 101),
	}

	st := compare(rounds, byOpen)
	if st.n != 2 {
		t.Errorf("n = %d, 기대 2 — 슬롯 2개뿐인데 회차 4개를 다 셌다", st.n)
	}
	if st.dupSlot != 2 {
		t.Errorf("dupSlot = %d, 기대 2", st.dupSlot)
	}
	if st.dupConflict != 0 {
		t.Errorf("dupConflict = %d, 기대 0 — 방향이 전부 같다", st.dupConflict)
	}
	if st.disagree != 0 {
		t.Errorf("disagree = %d, 기대 0", st.disagree)
	}
}

// 충돌이 나면 호출부가 경고를 찍는다. 경고에 내용이 있으려면 예시가 필요하다 —
// 어느 슬롯의 어느 슬러그였는지 없이 "충돌 2건"만 보면 조사할 수가 없다.
func TestCompareRecordsCollisionSamples(t *testing.T) {
	byOpen := map[int64]klines.Kline{300: bar(300, 100, 101)}
	rounds := []rest.Round{
		round("btc-updown-5m-300", 300, 100, 101),   // 상승 — 채택
		round("btc-updown-5m-300-b", 300, 100, 98),  // 하락 — 충돌이면서 방향도 다름
		round("btc-updown-5m-300-c", 300, 100, 101), // 상승 — 충돌이지만 방향 일치
	}
	st := compare(rounds, byOpen)
	if st.n != 1 {
		t.Fatalf("n = %d, 기대 1", st.n)
	}
	if st.dupSlot != 2 || st.dupConflict != 1 {
		t.Errorf("dupSlot=%d dupConflict=%d, 기대 2/1", st.dupSlot, st.dupConflict)
	}
	if len(st.dupSample) != 2 {
		t.Fatalf("예시 %d건, 기대 2건 — 경고에 실을 내용이 없다", len(st.dupSample))
	}
	// 채택된 슬러그와 버린 슬러그가 둘 다 보여야 조사할 수 있다
	if !strings.Contains(st.dupSample[0], "btc-updown-5m-300") ||
		!strings.Contains(st.dupSample[0], "btc-updown-5m-300-b") {
		t.Errorf("예시에 슬러그가 빠졌다: %q", st.dupSample[0])
	}
}

// 예시는 무한정 쌓이면 안 된다. 충돌이 수백 건이면 로그를 덮어버린다.
func TestCompareCapsCollisionSamples(t *testing.T) {
	byOpen := map[int64]klines.Kline{300: bar(300, 100, 101)}
	rounds := []rest.Round{round("btc-updown-5m-300", 300, 100, 101)}
	for i := 0; i < dupSampleMax*3; i++ {
		rounds = append(rounds, round(fmt.Sprintf("btc-updown-5m-300-%d", i), 300, 100, 101))
	}
	st := compare(rounds, byOpen)
	if st.dupSlot != dupSampleMax*3 {
		t.Errorf("dupSlot = %d, 기대 %d", st.dupSlot, dupSampleMax*3)
	}
	if len(st.dupSample) != dupSampleMax {
		t.Errorf("예시 %d건, 상한 %d건을 지켜야 한다", len(st.dupSample), dupSampleMax)
	}
}

// 채택되는 것은 최신순 첫 회차다. 뒤에 오는 중복이 판정을 바꾸면 안 된다.
func TestCompareKeepsFirstRoundPerSlot(t *testing.T) {
	byOpen := map[int64]klines.Kline{300: bar(300, 100, 101)} // 바이낸스 상승
	rounds := []rest.Round{
		round("btc-updown-5m-300", 300, 100, 98),    // chainlink 하락 → 불일치
		round("btc-updown-5m-300-b", 300, 100, 101), // chainlink 상승 → 일치
	}
	st := compare(rounds, byOpen)
	if st.n != 1 || st.disagree != 1 {
		t.Errorf("n=%d disagree=%d, 기대 1/1 — 첫 회차를 남겨야 한다", st.n, st.disagree)
	}
}

// 도지·무변동은 방향 비교 대상이 아니고, 봉이 없으면 결측이다.
// 중복 제거를 넣으면서 이 분기들이 상하지 않았는지 같이 본다.
func TestCompareExcludesFlatAndMissing(t *testing.T) {
	byOpen := map[int64]klines.Kline{
		300: bar(300, 100, 100), // 바이낸스 도지
		600: bar(600, 100, 101),
		900: bar(900, 100, 101),
	}
	rounds := []rest.Round{
		round("btc-updown-5m-300", 300, 100, 101),   // 봉이 도지
		round("btc-updown-5m-600", 600, 100, 100),   // chainlink 무변동
		round("btc-updown-5m-900", 900, 100, 101),   // 정상 비교
		round("btc-updown-5m-1200", 1200, 100, 101), // 봉 없음
	}
	st := compare(rounds, byOpen)
	if st.n != 1 {
		t.Errorf("n = %d, 기대 1", st.n)
	}
	if st.binFlat != 1 || st.chainFlat != 1 || st.missing != 1 {
		t.Errorf("binFlat=%d chainFlat=%d missing=%d, 기대 1/1/1", st.binFlat, st.chainFlat, st.missing)
	}
}
