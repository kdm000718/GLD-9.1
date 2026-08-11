package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/exec"
	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// **이 테스트가 타입 가드의 요점이다.**
//
// noFills 는 "체결 없음" 을 돌려준다. DRY-RUN 에서는 그것이 정확하다 —
// 주문을 전송하지 않으므로 체결될 주문이 존재하지 않는다. 그러나 실거래
// 배선에 그대로 남으면 exec 의 노출 불변식 첫 항이 영원히 0 이고, 봇은
// 체결이 쌓여도 잔여 한도가 줄지 않는다고 믿는다 — cap 을 몇 배로 넘겨
// 베팅한다.
//
// 그래서 무장 경로는 armedFills 만 받는다. noFills 는 표식 메서드가 없으므로
// 그 인터페이스를 만족하지 않고, 넘기려는 코드는 **컴파일되지 않는다.**
// 이 테스트는 그 사실이 조용히 뒤집히지 않게 고정한다 — 누가 noFills 에
// safeWithRealOrders 를 달면 여기서 걸린다.
func TestNoFillsCannotBeUsedWhenArmed(t *testing.T) {
	var f exec.Fills = noFills{}
	if _, ok := f.(armedFills); ok {
		t.Fatal("noFills 가 armedFills 를 만족한다 — 무장 상태에서 체결을 0 으로 세게 되고 노출 상한이 사라진다")
	}
}

func TestChooseFillsGivesNoFillsWhenDisarmed(t *testing.T) {
	got, err := chooseFills(false, fillsDeps{})
	if err != nil {
		t.Fatalf("DRY-RUN 인데 실패했다: %v", err)
	}
	if _, ok := got.(noFills); !ok {
		t.Fatalf("DRY-RUN 구현이 %T 다", got)
	}
}

// 무장하면 **진짜 조회 구현**을 내준다. 그리고 그것은 noFills 가 아니어야
// 한다 — Task 8 이 세운 계약이 이것이다.
func TestChooseFillsGivesRealImplementationWhenArmed(t *testing.T) {
	got, err := chooseFills(true, testFillsDeps(t, nil))
	if err != nil {
		t.Fatalf("무장인데 체결 조회를 만들지 못했다: %v", err)
	}
	if _, ok := got.(noFills); ok {
		t.Fatal("무장인데 noFills 를 내줬다 — 노출 상한이 사라진다")
	}
	if _, ok := got.(armedFills); !ok {
		t.Fatalf("무장 구현이 armedFills 를 만족하지 않는다: %T", got)
	}
}

// **배선이 모자라면 기동이 실패해야 한다.** 기본값으로 메우면 그 메움이 곧
// "체결을 세지 않는 구현" 이 된다.
func TestArmedFillsRefuseIncompleteWiring(t *testing.T) {
	ok := testFillsDeps(t, nil)
	cases := []struct {
		name   string
		break_ func(*fillsDeps)
	}{
		{"rest 가 nil", func(d *fillsDeps) { d.Rest = nil }},
		{"계정이 비었다", func(d *fillsDeps) { d.Account = "" }},
		{"계정이 주소가 아니다", func(d *fillsDeps) { d.Account = "0xNotAnAddress" }},
		{"주기가 0", func(d *fillsDeps) { d.Interval = 0 }},
		{"주기가 음수", func(d *fillsDeps) { d.Interval = -time.Second }},
		{"주기가 상한을 넘는다", func(d *fillsDeps) { d.Interval = MaxFillsPollInterval + time.Second }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ok
			tc.break_(&d)
			got, err := chooseFills(true, d)
			if err == nil {
				t.Fatalf("배선이 모자란데 구현을 내줬다: %T", got)
			}
			if !errors.Is(err, errFillsNotWired) {
				t.Errorf("에러가 errFillsNotWired 가 아니다: %v", err)
			}
			if got != nil {
				t.Errorf("에러인데 구현을 함께 돌려줬다: %T", got)
			}
		})
	}
}

// **어떤 Fills 구현도 매 바퀴 REST 를 치면 안 된다.** noFills 는 I/O 를 전혀
// 하지 않으므로 그 제약을 구조적으로 만족한다 — 회차 한 번 분량을 그대로
// 돌려 본다.
//
// 취소된 컨텍스트로도 도는 것을 함께 고정한다: 컨텍스트를 보는 구현이라면
// 그 자체가 시계나 채널을 건드린다는 뜻이다.
func TestNoFillsDoesNoWorkPerTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var f noFills
	const ticksPerRound = 300_000 / 50 // 회차 300초 / 50ms
	for i := 0; i < ticksPerRound; i++ {
		fills, err := f.Poll(ctx, live.Round{})
		if err != nil {
			t.Fatalf("%d번째 바퀴에서 에러: %v", i, err)
		}
		if len(fills) != 0 {
			t.Fatalf("%d번째 바퀴에서 체결 %d건 — 전송한 주문이 없는데 체결이 있을 수 없다", i, len(fills))
		}
	}
}

// ---------------------------------------------------------------------------
// restFills — 실물 응답 모양으로
// ---------------------------------------------------------------------------

// 자리표시자 주소. 실계정과 무관하고 대소문자를 일부러 섞었다 — 응답이
// 체크섬 표기로 오는데 대소문자를 그대로 비교하면 우리 체결이 남의 체결로
// 보이고, 그 순간 노출이 0 으로 고정된다.
const (
	fixtureFillSigner = "0xAbCd1111222233334444555566667777888899Ff"
	fixtureOtherMaker = "0x9999888877776666555544443333222211110000"
)

// matchesServer 는 GET /v1/orders/matches 하나만 답하는 서버다. 요청 수를
// 세고, 쿼리를 그대로 기록한다.
type matchesServer struct {
	srv     *httptest.Server
	client  *rest.Client
	calls   atomic.Int64
	lastQry atomic.Value // url.Values 를 문자열로
	body    func(page int) string
}

func newMatchesServer(t *testing.T, body func(page int) string) *matchesServer {
	t.Helper()
	ms := &matchesServer{body: body}
	page := 0
	ms.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/orders/matches" {
			t.Errorf("예상 밖 경로 %s", r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		ms.calls.Add(1)
		ms.lastQry.Store(r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ms.body(page))
		page++
	}))
	t.Cleanup(ms.srv.Close)
	c := rest.New("api-key-placeholder-long-enough")
	c.BaseURL = ms.srv.URL
	ms.client = c
	return ms
}

func (m *matchesServer) query() string {
	v, _ := m.lastQry.Load().(string)
	return v
}

func testFillsDeps(t *testing.T, body func(page int) string) fillsDeps {
	t.Helper()
	if body == nil {
		body = func(int) string { return emptyMatches }
	}
	ms := newMatchesServer(t, body)
	return fillsDeps{Rest: ms.client, Account: fixtureFillSigner, Interval: time.Second}
}

const emptyMatches = `{"success":true,"data":[]}`

func testRound() live.Round {
	return live.Round{
		CategoryID: 7, MarketID: 1266089, Slug: "btc-updown-5m-1786377600",
		StartsAt: time.Unix(1786377600, 0).UTC(), EndsAt: time.Unix(1786377900, 0).UTC(),
		Precision: 2, FeeRateBps: 200,
		UpTokenID: "1111111111111111111", DownTokenID: "2222222222222222222",
	}
}

// matchJSON 은 2026-08-10 메인넷 실측 응답의 모양 그대로다.
func matchJSON(tx, settlement string, makers []string, takerSigner string) string {
	fill := func(signer, token, name, amountWei, priceWei, feeAmt, feeType, quote string) string {
		return fmt.Sprintf(`{"quoteType":%q,"amount":%q,"price":%q,
			"outcome":{"name":%q,"indexSet":2,"onChainId":%q},
			"signer":%q,"hash":"0x%040x","fee":{"amount":%q,"type":%q}}`,
			quote, amountWei, priceWei, name, token, signer, len(signer)+len(amountWei), feeAmt, feeType)
	}
	var mk []string
	for _, s := range makers {
		mk = append(mk, fill(s, "1111111111111111111", "Up", "2000000000000000000", "330000000000000000", "0", "SHARES", "Bid"))
	}
	return fmt.Sprintf(`{"market":{"id":1266089},
		"taker":%s,
		"amountFilled":"99000000000000000000","priceExecuted":"330000000000000000",
		"makers":[%s],"transactionHash":%q,"settlementId":%q,
		"executedAt":"2026-08-10T16:03:21.000Z"}`,
		fill(takerSigner, "1111111111111111111", "Up", "9000000000000000000", "330000000000000000", "16156800000000000", "COLLATERAL", "Ask"),
		strings.Join(mk, ","), tx, settlement)
}

func matchesBody(matches ...string) string {
	return `{"success":true,"data":[` + strings.Join(matches, ",") + `]}`
}

// **최상위 amountFilled 를 우리 체결로 세면 안 된다.** 스펙 원문: 매치는
// 트랜잭션 하나 전체이고 최상위 수치는 테이커 기준이다. 여기서는 메이커가
// 셋이고 그중 하나만 우리다 — 우리 체결은 2주뿐인데 최상위는 99주다.
func TestOnlyOurMakerEntryCounts(t *testing.T) {
	body := matchesBody(matchJSON("0xtx1", "s1",
		[]string{fixtureOtherMaker, fixtureFillSigner, fixtureOtherMaker}, fixtureOtherMaker))
	f := mustRestFills(t, func(int) string { return body })

	got, err := f.Poll(context.Background(), testRound())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("체결 %d건, 기대 1건 (남의 체결까지 셌다면 3건이 된다): %+v", len(got), got)
	}
	if got[0].Shares != 2 {
		t.Errorf("주식 수 %v, 기대 2 — 최상위 amountFilled(99)를 셌을 수 있다", got[0].Shares)
	}
	if got[0].Outcome != ledger.OutcomeUp {
		t.Errorf("방향 %q", got[0].Outcome)
	}
	if got[0].MarketID != 1266089 || got[0].RoundStart != 1786377600 {
		t.Errorf("회차 키가 다르다: %+v", got[0])
	}
	// 메이커 수수료는 SHARES 0 이라 USD 0 이다.
	if got[0].FeeUSD != 0 {
		t.Errorf("수수료 %v, 기대 0", got[0].FeeUSD)
	}
	// 원장이 받아들이는 값이어야 한다. 여기서 걸리면 exec 가 무장을 해제한다.
	if err := (&ledger.Ledger{}).RecordFill(got[0]); err != nil && !errors.Is(err, ledger.ErrNotOpen) {
		t.Errorf("원장이 이 체결을 거부한다: %v", err)
	}
}

// **같은 체결을 두 번 세면 노출이 두 배가 된다.** 같은 응답을 계속 돌려주는
// 서버로 여러 번 폴링해도 체결은 한 번만 나와야 한다.
func TestSameFillIsCountedOnce(t *testing.T) {
	body := matchesBody(matchJSON("0xtx1", "s1", []string{fixtureFillSigner}, fixtureOtherMaker))
	f := mustRestFills(t, func(int) string { return body })
	clk := time.Unix(1786377600, 0)
	f.now = func() time.Time { return clk }
	rd := testRound()

	total := 0
	for i := 0; i < 5; i++ {
		got, err := f.Poll(context.Background(), rd)
		if err != nil {
			t.Fatalf("%d번째 Poll: %v", i, err)
		}
		total += len(got)
		clk = clk.Add(2 * time.Second) // 주기를 넘겨 매번 실제로 조회하게 한다
	}
	if total != 1 {
		t.Fatalf("같은 체결을 %d번 셌다 — 노출이 그만큼 부풀려진다", total)
	}
}

// **같은 트랜잭션 안에 같은 주문 해시가 두 번 나오면 둘 다 세야 한다.**
// 멱등 키가 (settlementId, txHash, hash) 뿐이면 둘째 건이 중복으로 보여
// 조용히 사라진다 — 노출을 과소계상하는 방향이다.
func TestTwoFillsOfTheSameOrderInOneTransactionBothCount(t *testing.T) {
	body := matchesBody(matchJSON("0xtx1", "s1",
		[]string{fixtureFillSigner, fixtureFillSigner}, fixtureOtherMaker))
	f := mustRestFills(t, func(int) string { return body })

	got, err := f.Poll(context.Background(), testRound())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("체결 %d건, 기대 2건 — 같은 해시가 두 번 나오면 둘 다 우리 체결이다", len(got))
	}
}

// **우리가 테이커로 체결될 수도 있다.** 이 봇은 메이커로 걸지만 지정가가
// 최우선 매도호가에 닿으면 그 주문은 테이커로 체결된다. makers[] 만 보면
// 그 체결이 통째로 사라진다 — "한쪽만 막았다" 의 전형이다.
//
// 다만 우리 테이커 주문도 **매수(Bid)** 여야 한다. 실측 응답의 테이커는
// Ask 라 그대로 두면 매도가 되므로, 여기서는 Bid 로 바꾼 본문을 쓴다.
func TestOurTakerFillIsCounted(t *testing.T) {
	body := strings.Replace(
		matchesBody(matchJSON("0xtx1", "s1", []string{fixtureOtherMaker}, fixtureFillSigner)),
		`"quoteType":"Ask"`, `"quoteType":"Bid"`, 1)
	f := mustRestFills(t, func(int) string { return body })

	got, err := f.Poll(context.Background(), testRound())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("체결 %d건, 기대 1건 — 우리가 테이커인 체결을 놓쳤다", len(got))
	}
	if got[0].Shares != 9 {
		t.Errorf("주식 수 %v, 기대 9", got[0].Shares)
	}
	// 테이커 수수료는 COLLATERAL 이라 그대로 USD 다.
	if got[0].FeeUSD != 0.0161568 {
		t.Errorf("수수료 %v, 기대 0.0161568", got[0].FeeUSD)
	}
}

// **우리 서명으로 매도가 체결됐다면 그것은 우리가 이해하지 못하는 일이다.**
// 매수로 적으면 포지션 부호가 뒤집힌 채 크래시 복구가 돈다.
func TestOurAskFillIsAnError(t *testing.T) {
	body := matchesBody(matchJSON("0xtx1", "s1", []string{fixtureOtherMaker}, fixtureFillSigner))
	f := mustRestFills(t, func(int) string { return body })
	if _, err := f.Poll(context.Background(), testRound()); err == nil {
		t.Fatal("우리 서명의 Ask 체결을 통과시켰다 — 이 봇은 매도 주문을 내지 않는다")
	}
}

// **방향은 이름이 아니라 토큰 ID 로 정한다.** 이 회차의 Up/Down 어느 쪽도
// 아닌 토큰이 오면 에러다 — 다른 마켓의 체결이 섞였거나 파싱이 틀린 것이다.
func TestUnknownOutcomeTokenIsAnError(t *testing.T) {
	body := strings.ReplaceAll(
		matchesBody(matchJSON("0xtx1", "s1", []string{fixtureFillSigner}, fixtureOtherMaker)),
		`"onChainId":"1111111111111111111"`, `"onChainId":"3333333333333333333"`)
	f := mustRestFills(t, func(int) string { return body })
	if _, err := f.Poll(context.Background(), testRound()); err == nil {
		t.Fatal("이 회차의 것이 아닌 토큰 ID 를 통과시켰다")
	}
}

// **모르는 수수료 type 은 에러다.** 0 으로 두면 비용이 사라지고, 그것은
// 적자를 흑자로 보이게 하는 방향이다.
func TestUnknownFeeTypeIsAnError(t *testing.T) {
	body := strings.ReplaceAll(
		matchesBody(matchJSON("0xtx1", "s1", []string{fixtureFillSigner}, fixtureOtherMaker)),
		`"type":"SHARES"`, `"type":"BANANAS"`)
	f := mustRestFills(t, func(int) string { return body })
	if _, err := f.Poll(context.Background(), testRound()); err == nil {
		t.Fatal("모르는 수수료 type 을 통과시켰다")
	}
}

// **매 바퀴 REST 를 치면 안 된다.** 회차당 6,000 바퀴이고 레이트리밋은 키당
// 240 req/min 이다. 회차 한 번 분량을 50ms 씩 돌려서 요청 수가 주기로
// 설명되는 범위인지 본다.
// 요청 수는 **주기로만 설명돼야 한다.** 회차 한 번(300초)을 50ms 바퀴로
// 그대로 돌린다.
//
// 여기서 주기를 상한(30초)으로 잡는 이유는 오직 테스트 시간이다 —
// rest.Client 가 요청 사이에 333ms 를 강제하므로 기본 주기(2초, 150요청)로
// 돌리면 이 테스트 하나가 50초를 쓴다. 비율은 그대로다: 6,000 바퀴에 요청
// 10건이면 "매 바퀴 REST" 와는 600배 차이다. 기본 주기의 경계 동작은
// TestRestFillsRespectsTheConfiguredInterval 이 따로 고정한다.
func TestRestFillsDoesNotHitRestEveryTick(t *testing.T) {
	ms := newMatchesServer(t, func(int) string { return emptyMatches })
	f, err := newRestFills(ms.client, fixtureFillSigner, MaxFillsPollInterval, nil)
	if err != nil {
		t.Fatal(err)
	}
	clk := time.Unix(1786377600, 0)
	f.now = func() time.Time { return clk }
	rd := testRound()

	const ticks = 300_000 / 50 // 회차 300초 / 50ms
	for i := 0; i < ticks; i++ {
		if _, err := f.Poll(context.Background(), rd); err != nil {
			t.Fatalf("%d번째 바퀴: %v", i, err)
		}
		clk = clk.Add(50 * time.Millisecond)
	}
	got := ms.calls.Load()
	// 300초 / 30초 = 10 회. 경계 하나 정도의 오차만 허용한다.
	if got < 10 || got > 11 {
		t.Fatalf("회차 한 번(6,000 바퀴)에 REST 를 %d번 쳤다 — 기대 10 (주기 %s)", got, MaxFillsPollInterval)
	}
}

// 기본 주기의 경계 동작. 주기 직전에는 치지 않고, 주기에 닿으면 친다.
func TestRestFillsRespectsTheConfiguredInterval(t *testing.T) {
	ms := newMatchesServer(t, func(int) string { return emptyMatches })
	f, err := newRestFills(ms.client, fixtureFillSigner, DefaultFillsPollInterval, nil)
	if err != nil {
		t.Fatal(err)
	}
	clk := time.Unix(1786377600, 0)
	f.now = func() time.Time { return clk }
	rd := testRound()

	poll := func() {
		t.Helper()
		if _, err := f.Poll(context.Background(), rd); err != nil {
			t.Fatalf("Poll: %v", err)
		}
	}
	poll() // 회차의 첫 폴링은 반드시 실제로 조회한다
	if n := ms.calls.Load(); n != 1 {
		t.Fatalf("첫 폴링에서 REST 를 %d번 쳤다 — 기대 1번", n)
	}
	clk = clk.Add(DefaultFillsPollInterval - time.Millisecond)
	poll()
	if n := ms.calls.Load(); n != 1 {
		t.Fatalf("주기 직전인데 REST 를 %d번 쳤다 — 기대 1번", n)
	}
	clk = clk.Add(time.Millisecond)
	poll()
	if n := ms.calls.Load(); n != 2 {
		t.Fatalf("주기에 닿았는데 REST 를 %d번 쳤다 — 기대 2번", n)
	}
}

// **한 번도 성공하지 못한 회차에서는 빈 목록이 아니라 에러다.** 빈 목록은
// "새로 말할 것이 없다" 이지 "모른다" 가 아니다. 모르는 구간을 체결 없음으로
// 돌려주면 exec 가 그 근거로 신규 주문을 낸다.
func TestFirstFailureKeepsReportingError(t *testing.T) {
	fail := true
	ms := newMatchesServer(t, func(int) string { return emptyMatches })
	// 서버를 500 으로 바꾼다.
	ms.body = func(int) string {
		if fail {
			return "" // 아래 핸들러가 500 을 주지는 않으므로 파싱 실패로 만든다
		}
		return emptyMatches
	}
	f, err := newRestFills(ms.client, fixtureFillSigner, 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	clk := time.Unix(1786377600, 0)
	f.now = func() time.Time { return clk }
	rd := testRound()

	if _, err := f.Poll(context.Background(), rd); err == nil {
		t.Fatal("첫 조회가 실패했는데 에러가 아니다")
	}
	// 주기 안에서 다시 물어도 "체결 없음" 이 아니라 에러여야 한다.
	clk = clk.Add(50 * time.Millisecond)
	got, err := f.Poll(context.Background(), rd)
	if err == nil {
		t.Fatalf("성공한 적이 없는데 체결 %d건(=없음)을 돌려줬다", len(got))
	}
	// 그리고 그 사이에 REST 를 다시 치지 않았어야 한다 — 실패가 반복되면
	// 매 바퀴 요청이 나간다.
	if n := ms.calls.Load(); n != 1 {
		t.Errorf("주기 안에서 REST 를 %d번 쳤다 — 기대 1번", n)
	}

	// 회복되면 정상으로 돌아온다.
	fail = false
	clk = clk.Add(2 * time.Second)
	if _, err := f.Poll(context.Background(), rd); err != nil {
		t.Fatalf("회복 후에도 실패했다: %v", err)
	}
	clk = clk.Add(50 * time.Millisecond)
	if _, err := f.Poll(context.Background(), rd); err != nil {
		t.Fatalf("성공 뒤 주기 안 조회가 에러다: %v", err)
	}
}

// 회차가 바뀌면 멱등 키와 주기를 새로 시작한다. 안 그러면 seen 이 24시간
// 동안 계속 자라고, 새 회차의 첫 폴링이 이전 회차의 주기에 묶인다.
func TestRoundChangeResetsState(t *testing.T) {
	ms := newMatchesServer(t, func(int) string { return emptyMatches })
	f, err := newRestFills(ms.client, fixtureFillSigner, MaxFillsPollInterval, nil)
	if err != nil {
		t.Fatal(err)
	}
	clk := time.Unix(1786377600, 0)
	f.now = func() time.Time { return clk }

	rd1 := testRound()
	if _, err := f.Poll(context.Background(), rd1); err != nil {
		t.Fatal(err)
	}
	// 같은 회차에서는 주기에 묶여 다시 치지 않는다.
	clk = clk.Add(MaxFillsPollInterval / 2)
	if _, err := f.Poll(context.Background(), rd1); err != nil {
		t.Fatal(err)
	}
	if n := ms.calls.Load(); n != 1 {
		t.Fatalf("같은 회차에서 REST 를 %d번 쳤다 — 기대 1번", n)
	}
	// 회차가 바뀌면 곧바로 다시 친다.
	rd2 := rd1
	rd2.MarketID = 1266125
	rd2.Slug = "btc-updown-5m-1786377900"
	if _, err := f.Poll(context.Background(), rd2); err != nil {
		t.Fatal(err)
	}
	if n := ms.calls.Load(); n != 2 {
		t.Fatalf("새 회차에서 REST 를 %d번 쳤다 — 기대 2번", n)
	}
	if len(f.seen) != 0 {
		t.Errorf("새 회차인데 멱등 키가 %d개 남아 있다", len(f.seen))
	}
}

// 조회는 **마켓과 서명자로 서버에서 좁힌다.** 이 마켓 전체 체결은 5분에
// 수백 건이라(2026-08-10 실측) 클라이언트에서 거르면 매 폴링이 여러 페이지가
// 된다.
func TestQueryNarrowsByMarketAndSigner(t *testing.T) {
	ms := newMatchesServer(t, func(int) string { return emptyMatches })
	f, err := newRestFills(ms.client, fixtureFillSigner, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Poll(context.Background(), testRound()); err != nil {
		t.Fatal(err)
	}
	q := ms.query()
	if !strings.Contains(q, "marketId=1266089") {
		t.Errorf("쿼리에 marketId 가 없다: %s", q)
	}
	if !strings.Contains(strings.ToLower(q), strings.ToLower(strings.TrimPrefix(fixtureFillSigner, "0x"))) {
		t.Errorf("쿼리에 signerAddress 가 없다: %s", q)
	}
}

// **다른 마켓의 체결이 오면 에러다.** 필터가 무시된 것이고, 다른 회차의
// 체결을 이 회차 노출로 세면 한도 계산이 통째로 틀린다.
func TestMatchFromAnotherMarketIsAnError(t *testing.T) {
	body := strings.Replace(
		matchesBody(matchJSON("0xtx1", "s1", []string{fixtureFillSigner}, fixtureOtherMaker)),
		`"market":{"id":1266089}`, `"market":{"id":999}`, 1)
	f := mustRestFills(t, func(int) string { return body })
	if _, err := f.Poll(context.Background(), testRound()); err == nil {
		t.Fatal("다른 마켓의 체결을 통과시켰다")
	}
}

// 대소문자가 달라도 우리 체결이다. 응답은 체크섬 표기로 온다.
func TestSignerComparisonIsCaseInsensitive(t *testing.T) {
	body := matchesBody(matchJSON("0xtx1", "s1",
		[]string{strings.ToUpper(strings.TrimPrefix(fixtureFillSigner, "0x"))}, fixtureOtherMaker))
	body = strings.ReplaceAll(body, `"signer":"`+strings.ToUpper(strings.TrimPrefix(fixtureFillSigner, "0x"))+`"`,
		`"signer":"0x`+strings.ToUpper(strings.TrimPrefix(fixtureFillSigner, "0x"))+`"`)
	f := mustRestFills(t, func(int) string { return body })
	got, err := f.Poll(context.Background(), testRound())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("체결 %d건, 기대 1건 — 대소문자 때문에 우리 체결을 남의 것으로 봤다", len(got))
	}
}

// 응답이 우리 타입으로 읽히지 않으면 에러다. 조용한 제로값은 "체결 없음" 과
// 구별되지 않는다.
func TestMalformedResponseIsAnError(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"success:false", `{"success":false,"error":"nope","data":[]}`},
		{"signer 없음", `{"success":true,"data":[{"market":{"id":1266089},
			"taker":{"quoteType":"Bid","amount":"1","price":"1","outcome":{"name":"Up","onChainId":"1111111111111111111"},"hash":"0xa","fee":{"amount":"0","type":"SHARES"}},
			"makers":[],"transactionHash":"0xtx","executedAt":"2026-08-10T16:03:21.000Z"}]}`},
		{"fee 없음", `{"success":true,"data":[{"market":{"id":1266089},
			"taker":{"quoteType":"Bid","amount":"1","price":"1","outcome":{"name":"Up","onChainId":"1111111111111111111"},"signer":"0xa","hash":"0xa"},
			"makers":[],"transactionHash":"0xtx","executedAt":"2026-08-10T16:03:21.000Z"}]}`},
		{"executedAt 없음", `{"success":true,"data":[{"market":{"id":1266089},
			"taker":{"quoteType":"Bid","amount":"1","price":"1","outcome":{"name":"Up","onChainId":"1111111111111111111"},"signer":"0xa","hash":"0xa","fee":{"amount":"0","type":"SHARES"}},
			"makers":[],"transactionHash":"0xtx"}]}`},
		{"transactionHash 없음", `{"success":true,"data":[{"market":{"id":1266089},
			"taker":{"quoteType":"Bid","amount":"1","price":"1","outcome":{"name":"Up","onChainId":"1111111111111111111"},"signer":"0xa","hash":"0xa","fee":{"amount":"0","type":"SHARES"}},
			"makers":[],"executedAt":"2026-08-10T16:03:21.000Z"}]}`},
		{"market.id 없음", `{"success":true,"data":[{"market":{},
			"taker":{"quoteType":"Bid","amount":"1","price":"1","outcome":{"name":"Up","onChainId":"1111111111111111111"},"signer":"0xa","hash":"0xa","fee":{"amount":"0","type":"SHARES"}},
			"makers":[],"transactionHash":"0xtx","executedAt":"2026-08-10T16:03:21.000Z"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mustRestFills(t, func(int) string { return tc.body })
			if got, err := f.Poll(context.Background(), testRound()); err == nil {
				t.Fatalf("망가진 응답을 통과시켰다 (체결 %d건)", len(got))
			}
		})
	}
}

func mustRestFills(t *testing.T, body func(int) string) *restFills {
	t.Helper()
	ms := newMatchesServer(t, body)
	f, err := newRestFills(ms.client, fixtureFillSigner, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// 실측 응답 한 건을 그대로 넣어 본다. 필드 이름이 바뀌면 여기서 걸린다.
func TestParsesTheShapeWeMeasuredOnMainnet(t *testing.T) {
	// 2026-08-10 메인넷 실측 응답에서 서명자만 자리표시자로 바꾼 것이다.
	const measured = `{"success":true,"cursor":null,"data":[{
	 "amountFilled":"2720000000000000000",
	 "executedAt":"2026-08-10T16:03:21.000Z",
	 "makers":[{"amount":"2720000000000000000","fee":{"amount":"0","type":"SHARES"},
	   "hash":"0xbc9c6446d9186d61f74844075c395157af77afa32f333ff4ec15041df9a22252",
	   "outcome":{"bestAsk":{"price":0.36,"size":76},"bestBid":{"price":0.33,"size":15.34},
	     "indexSet":2,"name":"Down","onChainId":"2222222222222222222","status":null},
	   "price":"330000000000000000","quoteType":"Bid","signer":"` + fixtureFillSigner + `"}],
	 "market":{"id":1266089,"isNegRisk":false,"isYieldBearing":false,"decimalPrecision":2},
	 "priceExecuted":"330000000000000000",
	 "settlementId":"019fec6a-0ea8-72b0-9596-7028b272871e",
	 "taker":{"amount":"2720000000000000000","fee":{"amount":"16156800000000000","type":"COLLATERAL"},
	   "hash":"0xd9f6917af43ea871a4b5da55a880a091a32e5893c464b51441e37c755c7f1aaa",
	   "outcome":{"indexSet":2,"name":"Down","onChainId":"2222222222222222222"},
	   "price":"330000000000000000","quoteType":"Ask","signer":"` + fixtureOtherMaker + `"},
	 "transactionHash":"0x7b78ae4e5d5471b8bc0443ca0aed96039b138efbcc1091110c742127b3b29ace"}]}`

	// JSON 이 유효한지 먼저 본다 — 리터럴이 깨지면 테스트가 다른 이유로 통과한다.
	if !json.Valid([]byte(measured)) {
		t.Fatal("테스트 픽스처 JSON 이 유효하지 않다")
	}
	f := mustRestFills(t, func(int) string { return measured })
	got, err := f.Poll(context.Background(), testRound())
	if err != nil {
		t.Fatalf("실측 응답을 읽지 못했다: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("체결 %d건, 기대 1건", len(got))
	}
	if got[0].Outcome != ledger.OutcomeDown {
		t.Errorf("방향 %q, 기대 Down", got[0].Outcome)
	}
	if got[0].Shares != 2.72 {
		t.Errorf("주식 수 %v, 기대 2.72", got[0].Shares)
	}
	if got[0].PriceUSD != 0.33 {
		t.Errorf("가격 %v, 기대 0.33", got[0].PriceUSD)
	}
	if got[0].FeeUSD != 0 {
		t.Errorf("수수료 %v, 기대 0 (메이커는 SHARES 0)", got[0].FeeUSD)
	}
	if !got[0].At.Equal(time.Date(2026, 8, 10, 16, 3, 21, 0, time.UTC)) {
		t.Errorf("시각 %v", got[0].At)
	}
}

// **주식으로 걷힌 수수료는 체결가로 환산해야 한다.** 그대로 USD 로 세면
// 주당 $0.33 짜리 시장에서 수수료를 3배로 세고, 반대로 0 으로 두면 비용이
// 통째로 사라진다. 실측에서는 메이커 수수료가 늘 0 이라 이 경로가 조용히
// 틀린 채로 남기 쉽다.
func TestShareFeeIsValuedAtTheExecutionPrice(t *testing.T) {
	// 메이커 수수료를 0.5주로 바꾼다. 체결가 0.33 이므로 USD 로는 0.165 다.
	body := strings.Replace(
		matchesBody(matchJSON("0xtx1", "s1", []string{fixtureFillSigner}, fixtureOtherMaker)),
		`"fee":{"amount":"0","type":"SHARES"}`, `"fee":{"amount":"500000000000000000","type":"SHARES"}`, 1)
	f := mustRestFills(t, func(int) string { return body })
	got, err := f.Poll(context.Background(), testRound())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("체결 %d건", len(got))
	}
	if want := 0.5 * 0.33; got[0].FeeUSD != want {
		t.Fatalf("수수료 %v USD, 기대 %v (0.5주 × $0.33) — 주식 수수료를 USD 로 그대로 세거나 0 으로 뭉갰다",
			got[0].FeeUSD, want)
	}
}
