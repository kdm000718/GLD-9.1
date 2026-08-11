package onchain

import (
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	// **실제 계정 주소를 쓰지 않는다.** 이 저장소는 GitHub 에 올라가고, 주소는
	// 지갑을 지목한다. 글자가 섞인 값이라야 대소문자 정규화 시험이 실제로
	// 무언가를 검사한다(숫자만 있는 자리표시자는 그 시험을 조용히 무력화한다 —
	// 2026-08-10 cmd/signercheck 에서 실제로 그랬다).
	acct  = "0xAbCdEf0123456789aBcDeF0123456789AbCdEf01"
	spdrA = "0x8BC070BEdAB741406F4B1Eb65A72bee27894B689"
	spdrB = "0x365fb81bd4A24D6303cd2F19c349dE6894D8d58A"
)

func four() map[string]string {
	return map[string]string{"CTF": spdrA, "NEG_RISK": spdrB}
}

// hexWord 는 정수를 32바이트 hex 결과로 만든다.
func hexWord(v string) string {
	n, _ := new(big.Int).SetString(v, 10)
	return "0x" + strings.Repeat("0", 64-len(n.Text(16))) + n.Text(16)
}

// stub 은 data 접두(셀렉터+인자)로 응답을 고른다.
func stub(t *testing.T, reply func(data string) (string, bool)) (*Funding, *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		_ = json.Unmarshal(b, &req)
		var call struct {
			To   string `json:"to"`
			Data string `json:"data"`
		}
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params[0], &call)
		}
		seen = append(seen, call.Data)
		res, ok := reply(call.Data)
		if !ok {
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"message":"boom"}}`))
			return
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + res + `"}`))
	}))
	t.Cleanup(srv.Close)
	return &Funding{RPC: srv.URL, HTTP: srv.Client()}, &seen
}

func TestBlockersEmptyWhenFundedAndApproved(t *testing.T) {
	f, _ := stub(t, func(data string) (string, bool) {
		if strings.HasPrefix(data, selBalanceOf) {
			return hexWord("100000000000000000000"), true // 100 USDT
		}
		return hexWord("115792089237316195423570985008687907853269984665640564039457584007913129639935"), true
	})
	if got := f.Blockers(context.Background(), acct, four(), Units(1)); len(got) != 0 {
		t.Fatalf("차단 사유가 없어야 한다, got %v", got)
	}
}

// TestUnapprovedVariantBlocks 는 이 패키지의 핵심이다. 하나만 빠져도 막아야
// 한다 — 그 변종이 오는 날 주문은 서명까지 되고 정산에서 실패한다.
func TestUnapprovedVariantBlocks(t *testing.T) {
	wordB, _ := word(spdrB)
	f, _ := stub(t, func(data string) (string, bool) {
		switch {
		case strings.HasPrefix(data, selBalanceOf):
			return hexWord("100000000000000000000"), true
		case strings.Contains(data, wordB):
			return hexWord("0"), true // NEG_RISK 만 미승인
		}
		return hexWord("100000000000000000000"), true
	})
	got := f.Blockers(context.Background(), acct, four(), Units(1))
	if len(got) != 1 || !strings.Contains(got[0], "NEG_RISK") {
		t.Fatalf("미승인 변종을 이름으로 지목해야 한다, got %v", got)
	}
	if strings.Contains(got[0], "CTF,") || strings.Contains(got[0], ", CTF") {
		t.Fatalf("승인된 변종을 미승인으로 셌다: %v", got)
	}
}

func TestInsufficientBalanceBlocks(t *testing.T) {
	f, _ := stub(t, func(data string) (string, bool) {
		if strings.HasPrefix(data, selBalanceOf) {
			return hexWord("500000000000000000"), true // 0.5 USDT
		}
		return hexWord("100000000000000000000"), true
	})
	got := f.Blockers(context.Background(), acct, four(), Units(1))
	if len(got) != 1 || !strings.Contains(got[0], "담보가 부족") {
		t.Fatalf("잔고 부족을 막아야 한다, got %v", got)
	}
}

// TestRPCFailureBlocks 는 실패 방향을 고정한다. 조회가 안 되면 "승인됐는지
// 모른다" 이고, 모르면 무장하지 않는다.
func TestRPCFailureBlocks(t *testing.T) {
	f, _ := stub(t, func(string) (string, bool) { return "", false })
	got := f.Blockers(context.Background(), acct, four(), Units(1))
	if len(got) == 0 {
		t.Fatal("RPC 실패는 차단이어야 한다 — 통과시키면 자금 없이 무장한다")
	}
}

// TestEmptyResultIsNotZero 는 빈 결과를 0 으로 읽지 않는지 본다. 0 으로 읽으면
// "승인이 없다"와 "노드가 이상하다"가 같은 값이 되고, 전자는 사용자가 할 일이
// 있는데 후자는 없다.
func TestEmptyResultIsNotZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":""}`))
	}))
	defer srv.Close()
	f := Funding{RPC: srv.URL, HTTP: srv.Client()}
	got := f.Blockers(context.Background(), acct, four(), Units(1))
	if len(got) != 1 || !strings.Contains(got[0], "조회하지 못했다") {
		t.Fatalf("빈 결과는 조회 실패여야 한다, got %v", got)
	}
}

// TestAllVariantsAreQueried 는 넷을 전부 묻는지 본다. 하나라도 건너뛰면 그
// 변종의 미승인이 조용히 통과한다.
func TestAllVariantsAreQueried(t *testing.T) {
	f, seen := stub(t, func(data string) (string, bool) {
		if strings.HasPrefix(data, selBalanceOf) {
			return hexWord("100000000000000000000"), true
		}
		return hexWord("100000000000000000000"), true
	})
	f.Blockers(context.Background(), acct, four(), Units(1))
	for name, addr := range four() {
		w, _ := word(addr)
		found := false
		for _, d := range *seen {
			if strings.HasPrefix(d, selAllowance) && strings.Contains(d, w) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s 의 승인액을 조회하지 않았다", name)
		}
	}
}

// TestUsesEighteenDecimals 는 6자리로 읽는 회귀를 막는다. 이더리움 USDT 는
// 6자리이고 BSC 는 18자리다 — 6 으로 읽으면 잔고가 10^12 배로 보여 어떤
// 최소 담보 검사도 통과한다.
func TestUsesEighteenDecimals(t *testing.T) {
	if USDTDecimals != 18 {
		t.Fatalf("USDTDecimals = %d, want 18 (BSC USDT)", USDTDecimals)
	}
	if got := Units(1).String(); got != "1000000000000000000" {
		t.Fatalf("Units(1) = %s, want 1e18", got)
	}
}

func TestWordRejectsBadAddress(t *testing.T) {
	for _, bad := range []string{"", "0x", "0xzz", "0x1234", strings.Repeat("a", 41)} {
		if _, err := word(bad); err == nil {
			t.Errorf("%q 를 주소로 받았다", bad)
		}
	}
	got, err := word("0xABCDEF0123456789ABCDEF0123456789ABCDEF01")
	if err != nil {
		t.Fatalf("대문자 주소: %v", err)
	}
	if len(got) != 64 || !strings.HasSuffix(got, "abcdef0123456789abcdef0123456789abcdef01") {
		t.Fatalf("좌측 0 패딩·소문자화가 안 됐다: %s", got)
	}
}
