package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/kernel"
	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// --- 자가 점검 2: 서명자 대조 -------------------------------------------------

// 자가 점검 2번은 cmd/signercheck 와 **같은 함수**를 부른다. 여기서 보는 것은
// 그 함수에 우리가 유도한 주소가 실제로 흘러가는가다 — 배선이 엉뚱한 주소를
// 넘기면 kernel 의 테스트는 전부 통과하면서 봇만 틀린다.
func TestVerifySignerUsesTheKeysOwnAddress(t *testing.T) {
	s, err := auth.NewSigner(testSignerHex)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ToLower(strings.TrimPrefix(s.Address().Hex(), "0x"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"0x%s%s"}`, strings.Repeat("0", 24), want)
	}))
	defer srv.Close()

	v := kernel.Verifier{RPC: srv.URL}
	if err := verifySigner(context.Background(), v, fixtureAccount, s); err != nil {
		t.Fatalf("등록 서명자와 같은데 실패했다: %v", err)
	}

	// 체인이 다른 주소를 돌려주면 즉시 실패해야 한다. P4 실측: 그 상태로
	// 진행하면 주문 서명이 전부 거부된다.
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":"0x%s%s"}`, strings.Repeat("0", 24), strings.Repeat("9", 40))
	}))
	defer other.Close()
	err = verifySigner(context.Background(), kernel.Verifier{RPC: other.URL}, fixtureAccount, s)
	var mm *kernel.MismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("불일치를 못 잡았다: %v", err)
	}
}

func TestVerifySignerRejectsMissingSigner(t *testing.T) {
	if err := verifySigner(context.Background(), kernel.Verifier{}, fixtureAccount, nil); err == nil {
		t.Error("서명자 없이 통과했다")
	}
}

// --- 자가 점검 3: 모델 ---------------------------------------------------------

// 저장소의 models.json 이 지금 코드의 피처와 맞는지. 맞지 않으면 봇이 뜨지
// 못하는데, 그 사실은 기동 시점에 드러나야 한다.
func TestLoadModelAcceptsRepositoryModel(t *testing.T) {
	m, err := loadModel("../../models.json")
	if err != nil {
		t.Fatalf("저장소 모델을 못 읽는다: %v", err)
	}
	if len(m.Coef) != len(features.FeatureNames) {
		t.Errorf("계수 %d개, 피처 %d개", len(m.Coef), len(features.FeatureNames))
	}
}

// 피처 이름이 어긋난 모델은 거부해야 한다. 통과시키면 계수가 엉뚱한 피처에
// 붙고, 확률은 여전히 0~1 사이라 눈에 띄지 않는다.
func TestLoadModelRejectsRenamedFeature(t *testing.T) {
	dir := t.TempDir()

	write := func(name string, names []string) string {
		coef := make([]float64, len(names))
		mu := make([]float64, len(names))
		sd := make([]float64, len(names))
		for i := range sd {
			sd[i] = 1
		}
		body := fmt.Sprintf(`{"feature_names":%s,"coef":%s,"mu":%s,"sd":%s,"intercept":0}`,
			jsonStrings(names), jsonFloats(coef), jsonFloats(mu), jsonFloats(sd))
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// **대조군이 먼저다.** 같은 손수 만든 픽스처가 이름만 맞으면 통과해야
	// 한다 — 그러지 않으면 아래 실패가 "이름이 달라서" 인지 "픽스처가
	// 애초에 못 읽히는 모양이라서" 인지 구분되지 않는다.
	if _, err := loadModel(write("ok.json", features.FeatureNames)); err != nil {
		t.Fatalf("이름이 맞는 픽스처가 통과하지 못했다 — 이 테스트는 아무것도 검증하지 못한다: %v", err)
	}

	names := make([]string, len(features.FeatureNames))
	copy(names, features.FeatureNames)
	names[0] = "이건_없는_피처"
	_, err := loadModel(write("bad.json", names))
	if err == nil {
		t.Fatal("피처 이름이 다른 모델이 통과했다")
	}
	if !strings.Contains(err.Error(), "이건_없는_피처") {
		t.Errorf("에러가 어긋난 이름을 짚지 않는다: %v", err)
	}
}

func jsonStrings(ss []string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = `"` + s + `"`
	}
	return "[" + strings.Join(out, ",") + "]"
}

func jsonFloats(fs []float64) string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = fmt.Sprintf("%v", f)
	}
	return "[" + strings.Join(out, ",") + "]"
}

// --- 원장 읽기 ------------------------------------------------------------------

func TestReadLedgerOnMissingFileIsEmptyNotError(t *testing.T) {
	st, err := readLedger(filepath.Join(t.TempDir(), "없음.csv"))
	if err != nil {
		t.Fatalf("첫 실행인데 에러다: %v", err)
	}
	if st.Rows != 0 || st.FillShares != 0 {
		t.Errorf("빈 상태가 아니다: %+v", st)
	}
}

// **열은 위치가 아니라 이름으로 찾는다.** 위치로 읽으면 열이 하나 추가되는
// 순간 수량 열에서 가격을 읽는다. 진짜 ledger 패키지가 쓴 파일로 확인한다 —
// 손으로 만든 픽스처는 그 계약을 검증하지 못한다.
func TestReadLedgerCountsFillsFromRealLedgerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1786275000, 0).UTC()
	for _, f := range []ledger.Fill{
		{RoundStart: 1786275000, MarketID: 70, Outcome: ledger.OutcomeUp, Shares: 20, PriceUSD: 0.47, At: now},
		{RoundStart: 1786275000, MarketID: 70, Outcome: ledger.OutcomeUp, Shares: 5.5, PriceUSD: 0.46, At: now},
	} {
		if err := l.RecordFill(f); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.RecordRebate(ledger.Rebate{RoundStart: 1786275000, Shares: 0.1275, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := readLedger(path)
	if err != nil {
		t.Fatalf("원장을 못 읽는다: %v", err)
	}
	if st.Rows != 3 {
		t.Errorf("행 %d개, 기대 3", st.Rows)
	}
	// **리베이트 주식을 매수 누적에 더하면 안 된다.** 리베이트는 반대편
	// 주식이라 우리 포지션이 아니고, 더하면 거래소 포지션과의 대조가 항상
	// 느슨해진다(우리 원장이 실제보다 커 보인다).
	if st.FillShares != 25.5 {
		t.Errorf("매수 누적 %v, 기대 25.5 (리베이트 0.1275 를 더하면 안 된다)", st.FillShares)
	}
	if st.SettledRounds != 0 {
		t.Errorf("정산 %d건 — exec 는 정산을 쓰지 않는다", st.SettledRounds)
	}
}

// 읽지 못한 원장을 "비었다" 로 다루면 이전 포지션을 통째로 잊는다.
func TestReadLedgerRejectsCorruptFile(t *testing.T) {
	cases := []struct{ name, body string }{
		{"헤더에 shares 열이 없다", "at_utc,record,round_start\n2026-01-01T00:00:00Z,fill,1\n"},
		{"모르는 record 종류", "record,round_start,shares\nmystery,1,5\n"},
		{"shares 가 숫자가 아니다", "record,round_start,shares\nfill,1,abc\n"},
		{"shares 가 NaN", "record,round_start,shares\nfill,1,NaN\n"},
		{"줄이 잘렸다", "record,round_start,shares\nfill,1\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger.csv")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if st, err := readLedger(path); err == nil {
				t.Fatalf("깨진 원장이 통과했다: %+v", st)
			}
		})
	}
}

// --- 자가 점검 5: 크래시 복구 대조 -----------------------------------------------

// accountServer 는 예약잔고와 포지션을 돌려주는 가짜 predict.fun 이다.
func accountServer(t *testing.T, reservedWei, positions string, status int) *rest.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/account/reserved-balances"):
			if reservedWei == "" {
				fmt.Fprint(w, `{"success":true,"data":[]}`)
				return
			}
			fmt.Fprintf(w, `{"success":true,"data":[{"type":"USDT","amount":%q}]}`, reservedWei)
		case strings.HasPrefix(r.URL.Path, "/v1/positions"):
			if positions == "" {
				fmt.Fprint(w, `{"success":true,"data":[]}`)
				return
			}
			fmt.Fprintf(w, `{"success":true,"data":%s}`, positions)
		default:
			t.Errorf("예상치 못한 경로: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c := rest.New("api-key-placeholder")
	c.BaseURL = srv.URL
	c.SetTokenSource(staticToken{})
	return c
}

// 빈 계정 + 빈 원장은 정상이다 — 첫 실행의 모습이다.
func TestReconcileAcceptsCleanStart(t *testing.T) {
	rc := accountServer(t, "", "", 0)
	if err := reconcile(context.Background(), rc, filepath.Join(t.TempDir(), "ledger.csv")); err != nil {
		t.Fatalf("깨끗한 시작인데 실패했다: %v", err)
	}
}

// **열린 주문이 남아 있으면 멈춘다.** 이 봇은 회차 종료 시 미체결을 전량
// 취소하므로, 기동 시점에 예약된 USDT 가 있다는 것은 이전 인스턴스가 주문을
// 든 채 죽었다는 뜻이다. 추측으로 이어가지 않는다.
func TestReconcileStopsWhenOrdersAreStillOpen(t *testing.T) {
	rc := accountServer(t, "5000000000000000000", "", 0) // 5 USDT 예약
	err := reconcile(context.Background(), rc, filepath.Join(t.TempDir(), "ledger.csv"))
	if !errors.Is(err, ErrReconcileMismatch) {
		t.Fatalf("불일치로 판정하지 않았다: %v", err)
	}
}

// 거래소가 우리 원장보다 많은 주식을 알고 있으면 체결이 안 적힌 것이다.
func TestReconcileStopsWhenExchangeKnowsMoreSharesThanLedger(t *testing.T) {
	// 10주 보유, 원장은 비었다.
	positions := `[{"amount":"10000000000000000000","averageBuyPriceUsd":"0.47","valueUsd":"5","pnlUsd":"0"}]`
	rc := accountServer(t, "", positions, 0)
	err := reconcile(context.Background(), rc, filepath.Join(t.TempDir(), "ledger.csv"))
	if !errors.Is(err, ErrReconcileMismatch) {
		t.Fatalf("불일치로 판정하지 않았다: %v", err)
	}
}

// 반대 방향은 정상이다 — 정산된 회차의 포지션은 사라지지만 원장 줄은 남는다.
// 이걸 불일치로 보면 봇은 두 번째 날부터 영원히 못 뜬다.
func TestReconcileAllowsLedgerAheadOfPositions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = l.RecordFill(ledger.Fill{
		RoundStart: 1786275000, MarketID: 70, Outcome: ledger.OutcomeUp,
		Shares: 100, PriceUSD: 0.47, At: time.Unix(1786275000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	positions := `[{"amount":"10000000000000000000","averageBuyPriceUsd":"0.47","valueUsd":"5","pnlUsd":"0"}]`
	rc := accountServer(t, "", positions, 0)
	if err := reconcile(context.Background(), rc, path); err != nil {
		t.Fatalf("원장이 더 많은데 실패했다: %v", err)
	}
}

// 조회 실패는 "다르다" 가 아니라 "모른다" 다. 호출자가 둘을 구분해야
// DRY-RUN 을 계속 돌릴지 정할 수 있다.
func TestReconcileDistinguishesQueryFailureFromMismatch(t *testing.T) {
	rc := accountServer(t, "", "", http.StatusInternalServerError)
	err := reconcile(context.Background(), rc, filepath.Join(t.TempDir(), "ledger.csv"))
	if err == nil {
		t.Fatal("조회가 실패했는데 통과했다")
	}
	if errors.Is(err, ErrReconcileMismatch) {
		t.Errorf("조회 실패를 불일치로 판정했다: %v", err)
	}
}

// 깨진 원장은 조회 실패가 아니라 불일치다 — 우리 파일이고, 읽히지 않는다면
// 그 자체가 어긋난 상태다.
func TestReconcileTreatsCorruptLedgerAsMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv")
	if err := os.WriteFile(path, []byte("record,round_start,shares\nmystery,1,5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rc := accountServer(t, "", "", 0)
	err := reconcile(context.Background(), rc, path)
	if !errors.Is(err, ErrReconcileMismatch) {
		t.Fatalf("깨진 원장을 불일치로 보지 않았다: %v", err)
	}
}

// --- 무장 차단 사유 --------------------------------------------------------------

// 지금 남은 차단은 **자금·승인 하나뿐이다.** 셋(체결 조회, 서명 자리,
// Exchange 변종)은 P5 Task 9 가 닫았고 그 근거는 armingBlockers 주석에 있다.
//
// 이 테스트가 지키는 것은 두 방향이다.
//
//   - 목록이 비면 안 된다. 비었다는 것은 자금 없이 무장이 열렸다는 뜻이다.
//   - 목록이 다시 늘어도 안 된다. 닫은 사유가 되살아나면 그 자리에 근거 없는
//     문자열이 하나 더 생긴 것이고, 그때는 왜 되살아났는지를 사람이 봐야 한다.
func TestOnlyFundingBlocksArmingNow(t *testing.T) {
	bs := armingBlockers()
	if len(bs) != 1 {
		t.Fatalf("무장 차단 사유가 %d건이다 — 지금 남아야 할 것은 자금·승인 하나뿐이다: %v", len(bs), bs)
	}
	if strings.TrimSpace(bs[0]) == "" {
		t.Fatal("사유가 비었다")
	}
	if !strings.Contains(bs[0], "승인") || !strings.Contains(bs[0], "자금") {
		t.Errorf("남은 사유가 자금·승인이 아니다: %q", bs[0])
	}
}
