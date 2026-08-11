package main

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/kdm000718/GLD-9.1/internal/exec"
	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// testSignerHex 는 go-ethereum 문서에 실린 **공개** 테스트 키다(주소
// 0x27000F84214f79B0600aa86841958b13ac98242a). cmd/g3check 가 골든 대조에
// 쓰는 것과 같은 키다. 실지갑 키가 아니므로 자금을 보내면 안 된다.
const testSignerHex = "4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3"

// 자리표시자 주소들. 실계정과 무관하다.
const (
	fixtureAccount   = "0x1111222233334444555566667777888899990000"
	fixtureExchange  = "0x8BC070BEdAB741406F4B1Eb65A72bee27894B689"
	fixtureValidator = "0x845ADb2C711129d4f3966735eD98a9F09fC4cE57"
	// fixtureSignerEOA 는 시험용 키(go-ethereum 공개 예시 키)가 유도하는
	// 주소다. **fixtureAccount 와 달라야 한다** — 그 둘이 다르다는 것이
	// 이 봇의 계정 구조이고, 같게 만들면 401 을 부른 그 버그로 돌아간다.
	fixtureSignerEOA = "0x27000F84214f79B0600aa86841958b13ac98242a"
)

// forbiddenServer 는 불리면 테스트를 깨뜨리는 서버다. DRY-RUN 이 정말로
// 아무것도 보내지 않는지 보려면 "안 보냈다" 를 관측할 수 있어야 한다.
func forbiddenServer(t *testing.T) *rest.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("DRY-RUN 인데 %s %s 를 보냈다", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(srv.Close)
	c := rest.New("api-key-placeholder")
	c.BaseURL = srv.URL
	c.SetTokenSource(staticToken{})
	return c
}

type staticToken struct{}

func (staticToken) Token(context.Context) (string, error) { return "token-placeholder", nil }

func testSender(t *testing.T, armed bool, rc *rest.Client) *orderSender {
	t.Helper()
	s, err := auth.NewSigner(testSignerHex)
	if err != nil {
		t.Fatal(err)
	}
	return &orderSender{
		Rest:      rc,
		Signer:    s,
		Account:   common.HexToAddress(fixtureAccount),
		Validator: common.HexToAddress(fixtureValidator),
		ChainID:   order.ChainIDMainnet,
		// **override 는 비워 둔다.** 회차의 isNegRisk/isYieldBearing 으로
		// 고르는 경로가 기본이고, 테스트가 그 경로를 타야 의미가 있다.
		Armed: armed,
		Now:   func() time.Time { return time.Unix(1786275000, 0).UTC() },
		Salt:  func() (*big.Int, error) { return big.NewInt(12345), nil },
	}
}

func testRequest() exec.Request {
	return exec.Request{
		Round: live.Round{
			MarketID: 70, Slug: "btc-updown-5m-1786275000",
			StartsAt:  time.Unix(1786275000, 0).UTC(),
			EndsAt:    time.Unix(1786275300, 0).UTC(),
			Precision: 2, FeeRateBps: 200,
			UpTokenID: "1111111111111111111", DownTokenID: "2222222222222222222",
		},
		Outcome: ledger.OutcomeUp,
		TokenID: "1111111111111111111",
		Tick:    order.NewTick(47, 2),
		Shares:  20,
	}
}

// **DRY-RUN 에서도 서명은 한다.** 서명 경로가 실거래와 달라지면 DRY-RUN 이
// 아무것도 증명하지 못한다. 그리고 전송은 하지 않는다 — 서버가 불리면
// forbiddenServer 가 테스트를 깨뜨린다.
func TestDryRunStillSigns(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))
	req := testRequest()

	body, envelope, err := s.build(req)
	if err != nil {
		t.Fatalf("서명 실패: %v", err)
	}
	// 86바이트 봉투: 0x01 + validator(20) + 서명(65). G3 가 SDK 와 비트 단위로
	// 대조한 형식이다.
	if len(envelope) != 86 {
		t.Fatalf("봉투 %d바이트, 기대 86", len(envelope))
	}
	if envelope[0] != 0x01 {
		t.Errorf("봉투 첫 바이트 0x%02x, 기대 0x01", envelope[0])
	}
	if got := common.BytesToAddress(envelope[1:21]); got != common.HexToAddress(fixtureValidator) {
		t.Errorf("봉투의 validator 가 다르다: %s", got)
	}

	o, ok := body["order"].(map[string]any)
	if !ok {
		t.Fatalf("본문에 order 객체가 없다: %v", body)
	}
	if sig, _ := o["signature"].(string); sig != "0x"+hex.EncodeToString(envelope) {
		t.Errorf("본문의 서명이 봉투와 다르다")
	}
	// **maker == signer == PREDICT_ACCOUNT**, 그리고 인증 세션도 같은 주소다.
	// 거래소는 `maker == signer` 를 무조건 요구하고(signatureType 0·1·2 모두),
	// 동시에 signer 가 인증 주소와 같기를 요구한다. 담보가 스마트계정에 있으니
	// 셋 다 그 주소여야 한다(2026-08-11 실측).
	if o["maker"] != o["signer"] {
		t.Errorf("maker(%v) 와 signer(%v) 가 다르다 — 거래소가 400 으로 거부한다", o["maker"], o["signer"])
	}
	if !strings.EqualFold(o["maker"].(string), fixtureAccount) {
		t.Errorf("maker 가 PREDICT_ACCOUNT 가 아니다: %v — 자금은 스마트계정에 있다", o["maker"])
	}
	if o["side"] != uint8(0) {
		t.Errorf("side = %v, 기대 0(BUY) — 이 봇은 매도 주문을 내지 않는다", o["side"])
	}
	if o["feeRateBps"] != "200" {
		t.Errorf("feeRateBps = %v, 기대 회차의 200", o["feeRateBps"])
	}
	// price = 0.47 → 0.47e18 wei.
	if body["pricePerShare"] != "470000000000000000" {
		t.Errorf("pricePerShare = %v", body["pricePerShare"])
	}
	// takerAmount = 20주 = 20e18 wei, makerAmount = 0.47×20 = 9.4 USDT.
	if o["takerAmount"] != "20000000000000000000" {
		t.Errorf("takerAmount = %v", o["takerAmount"])
	}
	if o["makerAmount"] != "9400000000000000000" {
		t.Errorf("makerAmount = %v", o["makerAmount"])
	}

	// Create 도 같은 경로를 타고, 전송만 건너뛴다.
	res, err := s.Create(context.Background(), req)
	if err != nil {
		t.Fatalf("DRY-RUN Create 실패: %v", err)
	}
	// **식별자가 비면 안 된다.** exec 는 빈 식별자를 "취소할 수 없는 주문"
	// 으로 다루고 회차 끝에 에러를 낸다 — DRY-RUN 이 취소까지 포함한 상태
	// 기계를 돌리지 못하게 된다.
	if !strings.HasPrefix(res.ID, "dryrun-") {
		t.Errorf("DRY-RUN 식별자가 %q 다", res.ID)
	}
	res2, _ := s.Create(context.Background(), req)
	if res2.ID == res.ID {
		t.Errorf("DRY-RUN 식별자가 중복이다 (%q) — exec 가 중복 식별자를 추적 불가로 다룬다", res.ID)
	}

	// 취소도 전송하지 않는다.
	rr, err := s.Remove(context.Background(), []string{res.ID})
	if err != nil {
		t.Fatalf("DRY-RUN Remove 실패: %v", err)
	}
	if len(rr.Removed) != 1 || rr.Removed[0] != res.ID {
		t.Errorf("DRY-RUN 취소 결과 %+v", rr)
	}
}

// 서명 자체가 실패하면 요청은 나가지 않았다 — 다시 보내도 이중 주문이 되지
// 않는다. exec 가 이 분류를 못 읽으면 "보냈을 수 있다" 로 다뤄 명목을 회차
// 끝까지 노출에 남긴다(있지도 않은 주문 때문에 한도를 잃는다).
func TestBuildFailuresAreRetrySafe(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))
	bad := testRequest()
	bad.TokenID = "not-a-number"

	_, err := s.Create(context.Background(), bad)
	if err == nil {
		t.Fatal("토큰 ID 가 숫자가 아닌데 통과했다")
	}
	var oe *rest.OrderError
	if !errors.As(err, &oe) {
		t.Fatalf("*rest.OrderError 가 아니다: %v", err)
	}
	if !oe.SafeToRetry() {
		t.Error("서명 전 실패인데 재시도 불가로 분류했다")
	}
}

// 0.5 이상에는 절대 걸지 않는다(사용자 제약). quote 가 이미 지키지만 서명
// 직전이 주문이 되기 전 마지막 관문이다 — 여기가 뚫리면 상한을 넘긴 주문에
// 서명이 붙는다.
func TestBuildRefusesTicksAtOrAboveHalf(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))
	for _, tick := range []int64{50, 60, 99} {
		r := testRequest()
		r.Tick = order.NewTick(tick, 2)
		if _, _, err := s.build(r); err == nil {
			t.Errorf("틱 %d(0.5 이상)에 서명했다", tick)
		}
	}
	// 상한 자체(0.49)는 통과한다.
	r := testRequest()
	r.Tick = order.NewTick(49, 2)
	if _, _, err := s.build(r); err != nil {
		t.Errorf("상한 틱 49 를 막았다: %v", err)
	}
}

// 틱이 0 이하면 WeiPerShare 가 패닉한다. 살아 있는 주문을 든 채 죽으면
// 취소도 못 한다 — 패닉 전에 에러로 빠져나와야 한다.
func TestBuildNeverPanicsOnBadTick(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))
	for _, tick := range []int64{0, -1, -100} {
		r := testRequest()
		r.Tick = order.Tick{V: tick, Precision: 2} // NewTick 은 V 를 안 본다
		if _, _, err := s.build(r); err == nil {
			t.Errorf("틱 %d 에 서명했다", tick)
		}
	}
	for _, prec := range []int{0, 19, -1} {
		r := testRequest()
		r.Tick = order.Tick{V: 47, Precision: prec}
		if _, _, err := s.build(r); err == nil {
			t.Errorf("precision %d 에 서명했다", prec)
		}
	}
}

// **극단값 조사에서 나온 것이다.** SDK 는 가격을 유효숫자 3자리로 절단한다.
// precision 2·3 에서는 틱이 최대 3자리라 아무것도 안 바뀌지만, precision 4
// 이상에서는 0.4999 가 0.4990 이 된다 — quote 가 정한 가격과 거래소에 놓이는
// 가격이 달라지고, "같은 가격이면 재주문하지 않는다" 판정이 존재하지 않는
// 호가를 기준으로 돈다. 에러도 로그도 없이.
func TestBuildRefusesPricesThatSDKTruncationWouldChange(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))

	// 실측된 precision 2·3 은 전부 통과해야 한다. 통과하지 못하면 이 가드가
	// 정상 거래를 막는 것이다.
	for _, c := range []struct {
		v int64
		p int
	}{{47, 2}, {49, 2}, {7, 2}, {1, 2}, {499, 3}, {123, 3}, {5, 3}} {
		r := testRequest()
		r.Round.Precision = c.p
		r.Tick = order.NewTick(c.v, c.p)
		if _, _, err := s.build(r); err != nil {
			t.Errorf("V=%d p=%d 를 막았다: %v", c.v, c.p, err)
		}
	}

	// precision 4 이상에서 4자리 틱은 거부해야 한다.
	for _, c := range []struct {
		v int64
		p int
	}{{4999, 4}, {1234, 4}, {49999, 5}} {
		r := testRequest()
		r.Round.Precision = c.p
		r.Tick = order.NewTick(c.v, c.p)
		if _, _, err := s.build(r); err == nil {
			t.Errorf("V=%d p=%d 에 서명했다 — 실제로는 다른 가격에 걸린다", c.v, c.p)
		}
	}

	// 같은 precision 이라도 절단에 안 걸리는 틱은 통과한다 — precision 자체를
	// 막는 것이 아니라 값을 막는다.
	r := testRequest()
	r.Round.Precision = 4
	r.Tick = order.NewTick(4990, 4)
	if _, _, err := s.build(r); err != nil {
		t.Errorf("절단에 안 걸리는 틱(V=4990 p=4)을 막았다: %v", err)
	}
}

// 주식 수를 wei 로 바꿀 때 float64 곱셈을 그대로 int64 로 자르면 2^63 을 넘어
// 조용히 음수가 된다. 음수 수량은 makerAmount 를 음수로 만들고, EIP-712 는
// 그런 값도 멀쩡히 서명한다.
func TestSharesToWeiHandlesExtremes(t *testing.T) {
	got, err := sharesToWei(20)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != "20000000000000000000" {
		t.Errorf("20주 → %s", got)
	}
	// risk.Shares 는 2^53 미만을 통과시킨다. 그 값에 1e18 을 곱하면 int64
	// 범위 밖이지만 big.Int 는 정확히 표현한다 — 음수가 되면 안 된다.
	big53, err := sharesToWei(9.007199254740992e15)
	if err != nil {
		t.Fatalf("2^53 주에서 실패했다: %v", err)
	}
	if big53.Sign() <= 0 {
		t.Errorf("2^53 주가 %s 가 됐다 — 부호가 뒤집혔다", big53)
	}
	for _, bad := range []float64{0, -1} {
		if _, err := sharesToWei(bad); err == nil {
			t.Errorf("주식 수 %v 를 받아들였다", bad)
		}
	}
}

// 무장 상태에서는 실제로 전송한다. 게이트가 반대로 붙어 있으면(무장인데
// 전송 안 함) 봇이 조용히 아무것도 안 하는 상태가 되고, 그것은 가장
// 알아채기 어려운 고장이다.
func TestArmedActuallyTransmits(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"code":"OK","orderId":"abc","orderHash":"0xdead","removalLockedUntil":"2026-08-10T00:00:05Z"}}`))
	}))
	defer srv.Close()
	rc := rest.New("api-key-placeholder")
	rc.BaseURL = srv.URL
	rc.SetTokenSource(staticToken{})

	s := testSender(t, true, rc)
	res, err := s.Create(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("전송 실패: %v", err)
	}
	if path != "/v1/orders" {
		t.Errorf("경로 %q", path)
	}
	if res.ID != "abc" {
		t.Errorf("주문 ID %q", res.ID)
	}
	if res.RemovalLockUnknown {
		t.Error("removalLockedUntil 을 읽지 못했다")
	}
	if res.LockedUntil.IsZero() {
		t.Error("잠금 시각이 제로다")
	}
}

// ---------------------------------------------------------------------------
// 만료 서명 — exec 의 전량 취소와 독립인 두 번째 방어
// ---------------------------------------------------------------------------

// 봇이 죽어도 주문이 영원히 살아 있으면 안 된다. exec 의 전량 취소는
// 프로세스가 돌아야 일어나는 일이므로, 만료는 그것과 독립인 층이다.
func TestOrderExpiresAtRoundEnd(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))
	req := testRequest()
	req.Round.EndsAt = time.Unix(1786275300, 0).UTC()

	body, _, err := s.build(req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	got := body["order"].(map[string]any)["expiration"]
	want := req.Round.EndsAt.Add(expirationGrace).Unix()
	if got != want {
		t.Errorf("expiration = %v, want %d", got, want)
	}
}

// 만료 0 은 "영원히 유효" 다. 그 값이 다시 들어오는 것을 막는다.
func TestOrderNeverExpiresZero(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))
	body, _, err := s.build(testRequest())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	switch v := body["order"].(map[string]any)["expiration"].(type) {
	case int64:
		if v == 0 {
			t.Error("expiration 이 0 이다 — 봇이 죽으면 주문이 체인에 남는다")
		}
	default:
		t.Errorf("expiration 이 int64 가 아니다: %T", v)
	}
}

// 스펙의 ContractOrder 는 expiration 을 `type: integer, format: int64` 로
// 정의한다. 다른 큰 수 필드(salt·tokenId·makerAmount…)는 전부 string 이라
// 여기만 예외이고, 그래서 조용히 틀리기 쉽다.
func TestExpirationIsNumericPerSpec(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))
	body, _, err := s.build(testRequest())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	o := body["order"].(map[string]any)
	if _, isString := o["expiration"].(string); isString {
		t.Error("expiration 을 문자열로 보낸다 — 스펙은 integer(int64) 다")
	}
	// 대조군: 이들은 스펙상 string 이어야 한다. 한꺼번에 숫자로 바꾸는
	// 실수를 막는다.
	for _, k := range []string{"salt", "tokenId", "makerAmount", "takerAmount", "nonce", "feeRateBps"} {
		if _, isString := o[k].(string); !isString {
			t.Errorf("%s 가 문자열이 아니다 (%T) — 스펙은 string 이다", k, o[k])
		}
	}
}

// 회차 종료 시각이 없으면 주문을 만들지 않는다.
//
// **"에러가 났다"만 보면 이 테스트는 아무것도 증명하지 않는다.** 제로시각의
// Unix() 는 음수이고, 그 값은 검사를 지워도 order.Hash 의 EIP-712 인코더가
// "invalid negative value for unsigned type uint256" 로 막는다. 그 우연한
// 백스톱을 잡고 통과하면 정작 우리 검사는 시험되지 않는다 — 초안이 실제로
// 그랬고, 변이(검사 블록 삭제)가 살아남아서 드러났다.
//
// 그래서 **어느 경로로 막혔는지**를 본다. 우리 검사는 서명 전에 막고 이유를
// 사람 말로 말한다. 다이제스트 단계까지 내려가면 그것은 다른 고장이다.
func TestOrderRejectsZeroEndsAt(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))
	req := testRequest()
	req.Round.EndsAt = time.Time{}

	_, _, err := s.build(req)
	if err == nil {
		t.Fatal("EndsAt 이 제로인데 주문이 만들어졌다")
	}
	if !strings.Contains(err.Error(), "회차 종료 시각이 없다") {
		t.Errorf("서명 전 검사가 아니라 다른 경로로 막혔다: %v", err)
	}
}

// **만료가 서명에 실제로 들어가는지가 이 층의 전부다.** 본문에만 있고
// EIP-712 다이제스트에 없으면 거래소가 그 값을 무시해도 우리는 알 수 없고,
// 체인이 지켜 주지도 않는다. 종료 시각만 다른 두 주문의 봉투가 달라야 한다.
func TestExpirationIsCoveredBySignature(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))

	a := testRequest()
	a.Round.EndsAt = time.Unix(1786275300, 0).UTC()
	_, envA, err := s.build(a)
	if err != nil {
		t.Fatalf("build a: %v", err)
	}

	b := testRequest()
	b.Round.EndsAt = time.Unix(1786275600, 0).UTC() // 5분 뒤 회차
	_, envB, err := s.build(b)
	if err != nil {
		t.Fatalf("build b: %v", err)
	}

	if hex.EncodeToString(envA) == hex.EncodeToString(envB) {
		t.Error("종료 시각이 다른데 서명이 같다 — 만료가 서명에 실리지 않는다")
	}
}

// ---------------------------------------------------------------------------
// Exchange 변종 — 회차마다 고른다
// ---------------------------------------------------------------------------

// **변종이 다르면 서명이 달라야 한다.** verifyingContract 가 EIP-712 도메인에
// 들어가므로, 같은 주문이 네 변종에서 네 개의 서로 다른 봉투를 내야 한다.
// 여기서 하나라도 같다면 변종 판정이 서명 경로에 닿지 않는다는 뜻이고,
// 그러면 상수를 박은 것과 다를 바 없다.
func TestVariantChangesTheSignature(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))
	seen := map[string]string{}
	for _, tc := range []struct{ neg, yb bool }{
		{false, false}, {true, false}, {false, true}, {true, true},
	} {
		req := testRequest()
		req.Round.IsNegRisk = tc.neg
		req.Round.IsYieldBearing = tc.yb
		_, envelope, err := s.build(req)
		if err != nil {
			t.Fatalf("negRisk=%v yieldBearing=%v: %v", tc.neg, tc.yb, err)
		}
		name := order.ExchangeName(tc.neg, tc.yb)
		if prev, dup := seen[string(envelope)]; dup {
			t.Errorf("%s 의 서명이 %s 와 같다 — 변종 판정이 서명에 닿지 않는다", name, prev)
		}
		seen[string(envelope)] = name
	}
}

// 실측 고정: btc-updown-5m 은 isNegRisk=false / isYieldBearing=false 이므로
// CTF_EXCHANGE 도메인으로 서명된다. 그 주소를 -exchange 로 직접 준 것과
// 결과가 같아야 한다 — 다르면 표에서 고른 값이 틀린 것이다.
func TestPlainRoundSignsWithCtfExchange(t *testing.T) {
	req := testRequest() // 기본값이 false/false 다
	auto := testSender(t, false, forbiddenServer(t))
	_, autoEnv, err := auto.build(req)
	if err != nil {
		t.Fatal(err)
	}
	manual := testSender(t, false, forbiddenServer(t))
	manual.ExchangeOverride = fixtureExchange // CTF_EXCHANGE 메인넷 주소
	_, manualEnv, err := manual.build(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(autoEnv) != string(manualEnv) {
		t.Fatal("마켓에서 고른 Exchange 가 CTF_EXCHANGE 와 다른 서명을 냈다")
	}
}

// override 는 회차의 판정을 **덮어쓴다.** 그 사실이 코드로 확인돼야 한다 —
// 덮어쓰지 않으면 탈출구가 탈출구가 아니고, 조용히 덮어쓰면 위험하다
// (그래서 main 이 기동 로그로 경고한다).
func TestExchangeOverrideWins(t *testing.T) {
	req := testRequest()
	req.Round.IsNegRisk = true // 표대로면 NEG_RISK 로 서명될 회차

	auto := testSender(t, false, forbiddenServer(t))
	_, autoEnv, err := auto.build(req)
	if err != nil {
		t.Fatal(err)
	}
	manual := testSender(t, false, forbiddenServer(t))
	manual.ExchangeOverride = fixtureExchange // CTF_EXCHANGE 로 강제
	_, manualEnv, err := manual.build(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(autoEnv) == string(manualEnv) {
		t.Fatal("override 가 회차 판정을 덮어쓰지 않았다")
	}
}

// 모르는 체인이면 **주문을 만들지 않는다.** 기본값으로 메인넷을 돌려주면
// 테스트넷 설정으로 메인넷 계약에 유효한 서명을 만든다.
func TestUnknownChainRefusesToSign(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))
	s.ChainID = 1
	if _, _, err := s.build(testRequest()); err == nil {
		t.Fatal("모르는 chainId 로 주문을 서명했다")
	}
}

// 서명 문자열은 스펙의 maxLength 200 안이어야 한다. 86바이트 봉투는
// "0x"+172자 = 174자다.
func TestSignatureFitsTheSchemaLimit(t *testing.T) {
	s := testSender(t, false, forbiddenServer(t))
	body, envelope, err := s.build(testRequest())
	if err != nil {
		t.Fatal(err)
	}
	o := body["order"].(map[string]any)
	sig := o["signature"].(string)
	if want := 2 + 2*len(envelope); len(sig) != want {
		t.Fatalf("서명이 %d자다 (봉투 %d바이트면 %d자여야 한다)", len(sig), len(envelope), want)
	}
	if len(sig) > maxSignatureLen {
		t.Fatalf("서명 %d자가 스펙 상한 %d자를 넘는다", len(sig), maxSignatureLen)
	}
}
