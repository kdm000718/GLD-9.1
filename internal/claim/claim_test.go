package claim

// 여기서 재는 것은 **조립과 안전장치**다. 바이트 정확성은 golden_test.go 가
// 실물과 대조해 이미 고정했으므로, 이 파일은 그 위에서 "언제 보내고 언제
// 안 보내는가" 를 잰다 — 그쪽이 자금을 움직이는 결정이다.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
)

// 이 키는 go-ethereum 문서의 예제 키다. 실제 자금과 무관하다.
const testKeyHex = "4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3"

const testAccount = "0xdd5A4da03F4f7b39F574A9bEDCD8D5f2e10bEB42"

func testSigner(t *testing.T) Signer {
	t.Helper()
	s, err := auth.NewSigner(testKeyHex)
	if err != nil {
		t.Fatalf("auth.NewSigner: %v", err)
	}
	return s
}

// fakeSource 는 회수 대상 조회를 대신한다.
type fakeSource struct {
	pos []Position
	err error
}

func (f fakeSource) Claimable(context.Context, string) ([]Position, error) { return f.pos, f.err }

func okPosition(won bool, outcomeIndex int, name string) Position {
	return Position{
		MarketID:     "1306744",
		ConditionID:  "0xbb499dbafe8564165e3fc10c4b156194f40947cd9060e04bc755fa7b7aa16f30",
		Title:        "Bitcoin Up or Down - August 11, 2PM-2:05PM ET",
		CategoryID:   "btc-updown-5m-1786471200",
		OutcomeIndex: outcomeIndex,
		OutcomeName:  name,
		SharesWei:    "1970568627450980392",
		Shares:       1.970568627450980392,
		Won:          won,
	}
}

// fakeBundler 는 ZeroDev·체인 RPC 를 한 서버로 흉내낸다. 보낸 UserOperation 을
// 기록해 시험이 들여다본다.
type fakeBundler struct {
	srv *httptest.Server
	// sent 는 eth_sendUserOperation 이 받은 본문이다.
	sent []map[string]any
	// hashOverride 가 비지 않으면 그 값을 userOpHash 로 돌려준다 —
	// "번들러가 우리와 다른 해시를 돌려주면 멈추는가"를 재려고 둔다.
	hashOverride string
	// receiptSuccess 는 영수증의 success 다.
	receiptSuccess bool
	// receiptMissing 이 참이면 영수증을 영원히 주지 않는다.
	receiptMissing bool
	// sponsorErr 가 비지 않으면 후원 단계에서 RPC 오류를 돌려준다.
	sponsorErr string
}

func newFakeBundler(t *testing.T) *fakeBundler {
	t.Helper()
	f := &fakeBundler{receiptSuccess: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBundler) bundler() Bundler {
	return Bundler{RPC: f.srv.URL, ChainRPC: f.srv.URL, HTTP: f.srv.Client()}
}

func (f *fakeBundler) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	_ = json.Unmarshal(body, &req)

	reply := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
	}

	switch req.Method {
	case "eth_call":
		// getNonce — 골든 벡터의 key 에 시퀀스 6.
		reply("0x0000845adb2c711129d4f3966735ed98a9f09fc4ce5727110000000000000006")
	case "zd_getUserOperationGasPrice":
		reply(map[string]any{
			"standard": map[string]string{"maxFeePerGas": "0x0", "maxPriorityFeePerGas": "0x0"},
		})
	case "zd_sponsorUserOperation":
		if f.sponsorErr != "" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 1,
				"error": map[string]any{"code": -32500, "message": f.sponsorErr},
			})
			return
		}
		reply(map[string]string{
			"preVerificationGas": "0x1324c", "verificationGasLimit": "0x17c6e", "callGasLimit": "0x3efe6",
		})
	case "eth_sendUserOperation":
		var op map[string]any
		_ = json.Unmarshal(req.Params[0], &op)
		f.sent = append(f.sent, op)
		if f.hashOverride != "" {
			reply(f.hashOverride)
			return
		}
		reply(recomputeHash(op))
	case "eth_getUserOperationReceipt":
		if f.receiptMissing {
			reply(nil)
			return
		}
		reply(map[string]any{
			"success": f.receiptSuccess,
			"receipt": map[string]string{"transactionHash": "0xfeed"},
		})
	default:
		http.Error(w, "모르는 메서드 "+req.Method, http.StatusBadRequest)
	}
}

// recomputeHash 는 가짜 번들러가 받은 본문으로 userOpHash 를 다시 계산한다.
// 실제 번들러가 하는 일과 같다 — 우리가 서명한 것과 보낸 것이 같아야만
// 두 해시가 맞는다.
func recomputeHash(op map[string]any) string {
	get := func(k string) *big.Int {
		s, _ := op[k].(string)
		x, _ := new(big.Int).SetString(strings.TrimPrefix(s, "0x"), 16)
		if x == nil {
			x = big.NewInt(0)
		}
		return x
	}
	cd, _ := op["callData"].(string)
	raw, _ := hex.DecodeString(strings.TrimPrefix(cd, "0x"))
	sender, _ := op["sender"].(string)
	u := UserOp{
		Sender: sender, Nonce: get("nonce"), CallData: raw,
		CallGasLimit: get("callGasLimit"), VerificationGasLimit: get("verificationGasLimit"),
		PreVerificationGas: get("preVerificationGas"),
		MaxFeePerGas:       get("maxFeePerGas"), MaxPriorityFeePerGas: get("maxPriorityFeePerGas"),
	}
	h, err := u.Hash(EntryPoint, ChainID)
	if err != nil {
		return "0x" + strings.Repeat("0", 64)
	}
	return "0x" + hex.EncodeToString(h)
}

func newClaimer(t *testing.T, f *fakeBundler, pos []Position, send bool) *Claimer {
	t.Helper()
	return &Claimer{
		Account: testAccount,
		Signer:  testSigner(t),
		Bundler: f.bundler(),
		Source:  fakeSource{pos: pos},
		Send:    send,
		Now:     func() time.Time { return time.Unix(1786471500, 0).UTC() },
	}
}

// TestDefaultDoesNotSend 는 이 패키지에서 가장 중요한 시험이다. 무장하지
// 않으면 조립까지만 하고 **전송하지 않는다.**
func TestDefaultDoesNotSend(t *testing.T) {
	f := newFakeBundler(t)
	c := newClaimer(t, f, []Position{okPosition(true, 2, "Down"), okPosition(false, 1, "Up")}, false)

	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.sent) != 0 {
		t.Fatalf("무장하지 않았는데 %d건을 보냈다", len(f.sent))
	}
	if len(res.Markets) != 1 {
		t.Fatalf("시장 %d개, 1개여야 한다", len(res.Markets))
	}
	m := res.Markets[0]
	if m.Err != nil {
		t.Fatalf("조립 실패: %v", m.Err)
	}
	if m.Sent {
		t.Error("Sent 가 참이다")
	}
	if len(m.CallData) == 0 || m.UserOpHash == "" {
		t.Error("조립 결과(callData·userOpHash)가 비었다 — 보내지 않아도 무엇을 보낼지는 보여야 한다")
	}
}

// TestSendsAssembledUserOperation 은 무장했을 때 실제로 보내는 바이트가
// 골든 벡터와 같은 구조인지 본다 — callData 는 실물과 **바이트 단위로**
// 같아야 한다(같은 conditionId, 같은 순서).
func TestSendsAssembledUserOperation(t *testing.T) {
	g := loadGolden(t)
	f := newFakeBundler(t)
	c := newClaimer(t, f, []Position{okPosition(true, 2, "Down"), okPosition(false, 1, "Up")}, true)

	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.sent) != 1 {
		t.Fatalf("전송 %d건, 1건이어야 한다", len(f.sent))
	}
	sent := f.sent[0]

	if got := sent["callData"].(string); !strings.EqualFold(got, g.UserOp.CallData) {
		t.Errorf("보낸 callData 가 골든 벡터와 다르다\n got %s\nwant %s", got, g.UserOp.CallData)
	}
	// nonce 는 시퀀스만 다르다(골든 5, 가짜 체인 6).
	if got, want := sent["nonce"].(string), "0x845adb2c711129d4f3966735ed98a9f09fc4ce5727110000000000000006"; got != want {
		t.Errorf("보낸 nonce %s, want %s", got, want)
	}
	// 서명은 우리 시험 키로 복구돼야 한다.
	sig, err := hex.DecodeString(strings.TrimPrefix(sent["signature"].(string), "0x"))
	if err != nil {
		t.Fatalf("서명 디코드: %v", err)
	}
	hash, err := hex.DecodeString(strings.TrimPrefix(res.Markets[0].UserOpHash, "0x"))
	if err != nil {
		t.Fatalf("해시 디코드: %v", err)
	}
	got, err := RecoverSigner(hash, sig)
	if err != nil {
		t.Fatalf("RecoverSigner: %v", err)
	}
	if want := SignerAddress(testSigner(t)); got != want {
		t.Errorf("보낸 서명이 %s 로 복구된다, want %s", got, want)
	}

	m := res.Markets[0]
	if m.Err != nil {
		t.Fatalf("회수 실패: %v", m.Err)
	}
	if !m.Sent || m.TxHash != "0xfeed" {
		t.Errorf("Sent=%v TxHash=%q", m.Sent, m.TxHash)
	}
	if res.Claimed() != 1 || res.Failed() != 0 {
		t.Errorf("요약이 틀렸다: 회수 %d, 실패 %d", res.Claimed(), res.Failed())
	}
}

// TestStopsWhenBundlerHashDiffers 는 번들러가 우리와 다른 userOpHash 를
// 돌려주면 성공으로 치지 않는지 본다. 두 해시가 다르다는 것은 우리가 서명한
// 것과 번들러가 받은 것이 다르다는 뜻이다.
func TestStopsWhenBundlerHashDiffers(t *testing.T) {
	f := newFakeBundler(t)
	f.hashOverride = "0x" + strings.Repeat("ab", 32)
	c := newClaimer(t, f, []Position{okPosition(true, 2, "Down")}, true)

	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	m := res.Markets[0]
	if m.Err == nil {
		t.Fatal("해시가 다른데 통과했다")
	}
	if !strings.Contains(m.Err.Error(), "userOpHash") {
		t.Errorf("실패 사유가 해시 불일치로 읽히지 않는다: %v", m.Err)
	}
	if res.Claimed() != 0 {
		t.Error("실패인데 회수로 셌다")
	}
}

// TestFailedReceiptIsNotSuccess 는 영수증이 실패로 실렸을 때 성공으로 치지
// 않는지 본다.
func TestFailedReceiptIsNotSuccess(t *testing.T) {
	f := newFakeBundler(t)
	f.receiptSuccess = false
	c := newClaimer(t, f, []Position{okPosition(true, 2, "Down")}, true)

	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Markets[0].Err == nil {
		t.Fatal("영수증이 실패인데 성공으로 쳤다")
	}
	if res.Claimed() != 0 {
		t.Error("실패인데 회수로 셌다")
	}
}

// TestMissingReceiptIsFailure 는 영수증을 못 봤을 때 성공으로 치지 않는지
// 본다. 명세의 규칙이다 — 실패로 두고 다시 시도하는 쪽이 안전하다(redeem 은
// 멱등이다).
func TestMissingReceiptIsFailure(t *testing.T) {
	f := newFakeBundler(t)
	f.receiptMissing = true
	c := &Claimer{
		Account: testAccount, Signer: testSigner(t), Bundler: f.bundler(),
		Source: fakeSource{pos: []Position{okPosition(true, 2, "Down")}}, Send: true,
	}
	// 시한을 짧게 만들려고 컨텍스트로 자른다.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	res, err := c.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Markets[0].Err == nil {
		t.Fatal("영수증이 없는데 성공으로 쳤다")
	}
	if res.Claimed() != 0 {
		t.Error("영수증을 못 봤는데 회수로 셌다")
	}
}

// TestSkipsUnverifiedMarketVariants 는 골든 벡터가 없는 시장 변종을
// 건너뛰는지 본다. negRisk 는 대상 컨트랙트부터 다르다.
func TestSkipsUnverifiedMarketVariants(t *testing.T) {
	neg := okPosition(true, 2, "Down")
	neg.IsNegRisk = true
	yb := okPosition(true, 2, "Down")
	yb.ConditionID = "0x" + strings.Repeat("11", 32)
	yb.IsYieldBearing = true

	f := newFakeBundler(t)
	c := newClaimer(t, f, []Position{neg, yb}, true)

	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.sent) != 0 {
		t.Fatalf("확인되지 않은 변종을 %d건 보냈다", len(f.sent))
	}
	if len(res.Skipped) != 2 {
		t.Fatalf("건너뛴 것 %d건, 2건이어야 한다: %+v", len(res.Skipped), res.Skipped)
	}
	if len(res.Markets) != 0 {
		t.Errorf("회수 시도가 %d건 생겼다", len(res.Markets))
	}
}

// TestSkipsBrokenPositions 는 응답이 깨진 포지션을 회수하지 않는지 본다.
func TestSkipsBrokenPositions(t *testing.T) {
	cases := map[string]func(*Position){
		"conditionId 없음": func(p *Position) { p.ConditionID = "" },
		"conditionId 짧음": func(p *Position) { p.ConditionID = "0xdeadbeef" },
		"결과 인덱스 0":       func(p *Position) { p.OutcomeIndex = 0 },
		"주식 0":           func(p *Position) { p.SharesWei = "0" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := okPosition(true, 2, "Down")
			mutate(&p)
			f := newFakeBundler(t)
			c := newClaimer(t, f, []Position{p}, true)
			res, err := c.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(f.sent) != 0 {
				t.Fatalf("깨진 포지션을 보냈다")
			}
			if len(res.Skipped) != 1 {
				t.Fatalf("건너뛰지 않았다: %+v", res)
			}
		})
	}
}

// TestSeparateUserOperationPerMarket 은 시장이 둘이면 UserOperation 도 둘인지
// 본다. 골든 벡터의 모양(시장 하나)을 유지하려는 결정이다.
func TestSeparateUserOperationPerMarket(t *testing.T) {
	a := okPosition(true, 2, "Down")
	b := okPosition(true, 2, "Down")
	b.ConditionID = "0x" + strings.Repeat("22", 32)
	b.Title = "다른 회차"
	b.CategoryID = "btc-updown-5m-1786471500"

	f := newFakeBundler(t)
	c := newClaimer(t, f, []Position{a, b}, true)

	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.sent) != 2 {
		t.Fatalf("전송 %d건, 2건이어야 한다", len(f.sent))
	}
	if len(res.Markets) != 2 {
		t.Fatalf("시장 결과 %d개, 2개여야 한다", len(res.Markets))
	}
	if res.Markets[0].ConditionID != a.ConditionID || res.Markets[1].ConditionID != b.ConditionID {
		t.Error("시장 순서가 조회 순서와 다르다")
	}
}

// TestSponsorFailureStopsBeforeSending 은 후원이 실패하면 전송하지 않는지
// 본다. 후원 없이 보내면 우리가 가스를 내야 하고, 잔고가 0 이라 실패한다.
func TestSponsorFailureStopsBeforeSending(t *testing.T) {
	f := newFakeBundler(t)
	f.sponsorErr = "AA33 paymaster deposit too low"
	c := newClaimer(t, f, []Position{okPosition(true, 2, "Down")}, true)

	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.sent) != 0 {
		t.Fatal("후원이 실패했는데 보냈다")
	}
	if res.Markets[0].Err == nil {
		t.Fatal("후원 실패가 결과에 없다")
	}
}

// TestSourceErrorIsNotEmptyResult 는 조회 실패를 "회수할 것이 없다"로 접지
// 않는지 본다. 접으면 회수가 조용히 멈춘다.
func TestSourceErrorIsNotEmptyResult(t *testing.T) {
	f := newFakeBundler(t)
	c := &Claimer{
		Account: testAccount, Signer: testSigner(t), Bundler: f.bundler(),
		Source: fakeSource{err: fmt.Errorf("네트워크 끊김")},
	}
	if _, err := c.Run(context.Background()); err == nil {
		t.Fatal("조회 실패가 에러로 올라오지 않았다")
	}
}

// TestRunRejectsMissingConfig 는 계정·서명자가 없으면 아무것도 하지 않는지
// 본다.
func TestRunRejectsMissingConfig(t *testing.T) {
	f := newFakeBundler(t)
	if _, err := (&Claimer{Signer: testSigner(t), Bundler: f.bundler()}).Run(context.Background()); err == nil {
		t.Error("계정이 없는데 통과했다")
	}
	if _, err := (&Claimer{Account: testAccount, Bundler: f.bundler()}).Run(context.Background()); err == nil {
		t.Error("서명자가 없는데 통과했다")
	}
}
