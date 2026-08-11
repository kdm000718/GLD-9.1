package rest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func matchesClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("api-key-placeholder-long-enough")
	c.BaseURL = srv.URL
	return c
}

// 2026-08-10 메인넷 실측 응답에서 서명자만 자리표시자로 바꾼 것이다.
// **필드 이름이 바뀌면 여기서 걸린다** — 응답 파싱이 틀리면 거래소가 무엇을
// 말했는지 자체를 모른다.
const measuredMatch = `{"success":true,"cursor":null,"data":[{
 "amountFilled":"2720000000000000000",
 "executedAt":"2026-08-10T16:03:21.000Z",
 "makers":[{"amount":"2720000000000000000","fee":{"amount":"0","type":"SHARES"},
   "hash":"0xbc9c6446d9186d61f74844075c395157af77afa32f333ff4ec15041df9a22252",
   "outcome":{"bestAsk":{"price":0.36,"size":76},"bestBid":{"price":0.33,"size":15.34},
     "indexSet":2,"name":"Down","onChainId":"91871696663301921083587431360492490125382917241199244025486810059445905826522",
     "status":null,"team":null,"variantData":null},
   "price":"330000000000000000","quoteType":"Bid","signer":"0xAbCd111122223333444455556666777788889900"}],
 "market":{"id":1266089,"isNegRisk":false,"isYieldBearing":false,"decimalPrecision":2},
 "priceExecuted":"330000000000000000",
 "settlementId":"019fec6a-0ea8-72b0-9596-7028b272871e",
 "taker":{"amount":"2720000000000000000","fee":{"amount":"16156800000000000","type":"COLLATERAL"},
   "hash":"0xd9f6917af43ea871a4b5da55a880a091a32e5893c464b51441e37c755c7f1aaa",
   "outcome":{"indexSet":2,"name":"Down","onChainId":"91871696663301921083587431360492490125382917241199244025486810059445905826522"},
   "price":"330000000000000000","quoteType":"Ask","signer":"0x9999888877776666555544443333222211110000"},
 "transactionHash":"0x7b78ae4e5d5471b8bc0443ca0aed96039b138efbcc1091110c742127b3b29ace"}]}`

func TestMatchesParsesTheMeasuredShape(t *testing.T) {
	c := matchesClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, measuredMatch)
	})
	got, err := c.Matches(context.Background(), MatchQuery{MarketID: 1266089})
	if err != nil {
		t.Fatalf("Matches: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("매치 %d건, 기대 1건", len(got))
	}
	m := got[0]
	if m.MarketID != 1266089 {
		t.Errorf("marketId %d", m.MarketID)
	}
	if m.SettlementID != "019fec6a-0ea8-72b0-9596-7028b272871e" {
		t.Errorf("settlementId %q", m.SettlementID)
	}
	if !strings.HasPrefix(m.TransactionHash, "0x7b78ae4e") {
		t.Errorf("transactionHash %q", m.TransactionHash)
	}
	if m.ExecutedAt.Unix() != 1786377801 {
		t.Errorf("executedAt %v", m.ExecutedAt)
	}
	if len(m.Makers) != 1 {
		t.Fatalf("메이커 %d명", len(m.Makers))
	}
	mk := m.Makers[0]
	if mk.Shares != 2.72 || mk.PriceUSD != 0.33 {
		t.Errorf("메이커 수량/가격 %v / %v, 기대 2.72 / 0.33", mk.Shares, mk.PriceUSD)
	}
	if mk.QuoteType != QuoteTypeBid {
		t.Errorf("메이커 quoteType %q", mk.QuoteType)
	}
	if mk.FeeAmount != 0 || mk.FeeType != FeeTypeShares {
		t.Errorf("메이커 수수료 %v %q, 기대 0 SHARES", mk.FeeAmount, mk.FeeType)
	}
	if !strings.HasPrefix(mk.OutcomeOnChainID, "918716966633") || mk.OutcomeName != "Down" {
		t.Errorf("메이커 outcome %q / %q", mk.OutcomeOnChainID, mk.OutcomeName)
	}
	if m.Taker.FeeType != FeeTypeCollateral || m.Taker.FeeAmount != 0.0161568 {
		t.Errorf("테이커 수수료 %v %q", m.Taker.FeeAmount, m.Taker.FeeType)
	}
	if m.Taker.QuoteType != QuoteTypeAsk {
		t.Errorf("테이커 quoteType %q", m.Taker.QuoteType)
	}
}

// **최상위 amountFilled/priceExecuted 는 타입에 자리가 없다.** 그것을 우리
// 체결로 세면 노출을 크게 과대계상한다 — 자리가 있으면 언젠가 누가 쓴다.
// 이 테스트는 그 자리가 생기면 컴파일 단계에서 알려주는 대신, 파싱된 값이
// 참가자 수치와 다르다는 사실을 고정한다.
func TestMatchesDoesNotCarryTransactionTotals(t *testing.T) {
	body := strings.Replace(measuredMatch,
		`"amountFilled":"2720000000000000000"`, `"amountFilled":"99000000000000000000"`, 1)
	c := matchesClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) })
	got, err := c.Matches(context.Background(), MatchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Makers[0].Shares != 2.72 {
		t.Errorf("메이커 수량이 %v 다 — 트랜잭션 전체 수치(99)가 새어 들어왔다", got[0].Makers[0].Shares)
	}
	if got[0].Taker.Shares != 2.72 {
		t.Errorf("테이커 수량이 %v 다", got[0].Taker.Shares)
	}
}

// 쿼리 파라미터 이름은 스펙 그대로여야 한다. **서버는 모르는 파라미터를
// 400 으로 거절하지 않고 조용히 무시한다**(P4 실측) — 이름이 틀리면 필터가
// 없는 것과 같고, 그러면 남의 체결이 잔뜩 섞여 온다.
func TestMatchesQueryParameterNames(t *testing.T) {
	var raw string
	c := matchesClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.RawQuery
		fmt.Fprint(w, `{"success":true,"data":[]}`)
	})
	_, err := c.Matches(context.Background(), MatchQuery{
		MarketID: 42, SignerAddress: "0xabc1234567890abc1234567890abc1234567890a",
		ExecutedAfterMS: 1786377600000, First: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"marketId=42", "first=25", "executedAfter=1786377600000",
		"signerAddress=0xabc1234567890abc1234567890abc1234567890a",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("쿼리에 %q 가 없다: %s", want, raw)
		}
	}
}

// 0 값 파라미터는 아예 보내지 않는다. `marketId=0` 을 보내면 서버가 어떻게
// 해석할지 우리가 모른다.
func TestMatchesOmitsZeroFilters(t *testing.T) {
	var raw string
	c := matchesClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.RawQuery
		fmt.Fprint(w, `{"success":true,"data":[]}`)
	})
	if _, err := c.Matches(context.Background(), MatchQuery{}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"marketId", "signerAddress", "executedAfter"} {
		if strings.Contains(raw, bad) {
			t.Errorf("빈 필터 %q 를 보냈다: %s", bad, raw)
		}
	}
	if !strings.Contains(raw, fmt.Sprintf("first=%d", MatchesPageSize)) {
		t.Errorf("기본 페이지 크기가 없다: %s", raw)
	}
}

// Bearer 를 요구하지 않는다 — 스펙의 security 가 ApiKeyAuth 뿐이다.
// TokenSource 를 배선하지 않은 클라이언트로도 되어야 한다.
func TestMatchesNeedsNoBearerToken(t *testing.T) {
	c := matchesClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("Bearer 를 보냈다: %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-api-key") == "" {
			t.Error("x-api-key 가 없다")
		}
		fmt.Fprint(w, `{"success":true,"data":[]}`)
	})
	if _, err := c.Matches(context.Background(), MatchQuery{}); err != nil {
		t.Fatalf("TokenSource 없이 실패했다: %v", err)
	}
}

// 커서를 따라간다. 안 따라가면 체결 일부를 못 본 채 조용히 성공한다.
func TestMatchesFollowsTheCursor(t *testing.T) {
	page := 0
	var afters []string
	c := matchesClient(t, func(w http.ResponseWriter, r *http.Request) {
		afters = append(afters, r.URL.Query().Get("after"))
		if page == 0 {
			page++
			fmt.Fprint(w, `{"success":true,"cursor":"c1","data":[`+oneMatch("0xtx1")+`]}`)
			return
		}
		fmt.Fprint(w, `{"success":true,"data":[`+oneMatch("0xtx2")+`]}`)
	})
	got, err := c.Matches(context.Background(), MatchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("매치 %d건, 기대 2건 — 커서를 따라가지 않았다", len(got))
	}
	if len(afters) != 2 || afters[0] != "" || afters[1] != "c1" {
		t.Errorf("after 파라미터 전개가 다르다: %v", afters)
	}
}

// 커서가 전진하지 않으면 무한 루프다. 에러로 끊는다.
func TestMatchesRejectsStuckCursor(t *testing.T) {
	c := matchesClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"cursor":"same","data":[`+oneMatch("0xtx")+`]}`)
	})
	if _, err := c.Matches(context.Background(), MatchQuery{}); err == nil {
		t.Fatal("멈춘 커서를 통과시켰다")
	}
}

// 페이지가 가득 찼는데 커서가 없다 — 커서 필드 이름이 바뀌면 이 모습이다.
// **조용히 자르지 않는다**: 자르면 체결 일부를 못 본 채 성공이 되고,
// 그게 노출을 과소계상하는 방향이다.
func TestMatchesRejectsFullPageWithoutCursor(t *testing.T) {
	c := matchesClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":true,"data":[`+oneMatch("0xtx")+`]}`)
	})
	if _, err := c.Matches(context.Background(), MatchQuery{First: 1}); err == nil {
		t.Fatal("가득 찬 페이지에 커서가 없는데 통과시켰다")
	}
}

// 페이지 상한을 넘으면 잘라서 돌려주지 않고 에러다.
func TestMatchesRejectsTooManyPages(t *testing.T) {
	n := 0
	c := matchesClient(t, func(w http.ResponseWriter, r *http.Request) {
		n++
		fmt.Fprintf(w, `{"success":true,"cursor":"c%d","data":[%s]}`, n, oneMatch("0xtx"))
	})
	if _, err := c.Matches(context.Background(), MatchQuery{}); err == nil {
		t.Fatal("페이지 상한을 넘겼는데 통과시켰다")
	}
	if n != maxMatchPages {
		t.Errorf("페이지를 %d개 받았다 — 상한은 %d 다", n, maxMatchPages)
	}
}

// 200 인데 success:false 는 실패다. 상태 코드만 보면 안 된다.
func TestMatchesTreatsSuccessFalseAsFailure(t *testing.T) {
	c := matchesClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":false,"error":{"code":"nope"},"data":[]}`)
	})
	if _, err := c.Matches(context.Background(), MatchQuery{}); err == nil {
		t.Fatal("success:false 를 성공으로 읽었다")
	}
}

// required 필드가 없으면 **조용한 제로값이 아니라 에러다.** 제로값은
// "0주 체결" 이나 "수수료 0" 으로 보이고 둘 다 사실로 읽어서는 안 된다.
func TestMatchesRejectsMissingRequiredFields(t *testing.T) {
	full := `{"market":{"id":1},"taker":` + oneFill() + `,"makers":[],` +
		`"transactionHash":"0xtx","executedAt":"2026-08-10T16:03:21.000Z"}`
	cases := []struct{ name, from, to string }{
		{"market.id", `"market":{"id":1}`, `"market":{}`},
		{"transactionHash", `"transactionHash":"0xtx"`, `"transactionHash":""`},
		{"executedAt", `"executedAt":"2026-08-10T16:03:21.000Z"`, `"executedAt":"어제"`},
		{"quoteType", `"quoteType":"Bid"`, `"quoteType":""`},
		{"signer", `"signer":"0xabc1234567890"`, `"signer":""`},
		{"hash", `"hash":"0xdef1234567890"`, `"hash":""`},
		{"outcome", `"outcome":{"name":"Up","indexSet":1,"onChainId":"111"}`, `"outcome":null`},
		{"outcome.onChainId", `"onChainId":"111"`, `"onChainId":""`},
		{"amount", `"amount":"2000000000000000000"`, `"amount":""`},
		{"price", `"price":"330000000000000000"`, `"price":""`},
		{"fee", `"fee":{"amount":"0","type":"SHARES"}`, `"fee":null`},
		{"fee.type", `"type":"SHARES"`, `"type":""`},
		{"fee.amount", `"fee":{"amount":"0","type":"SHARES"}`, `"fee":{"type":"SHARES"}`},
		{"음수 amount", `"amount":"2000000000000000000"`, `"amount":"-1"`},
		{"음수 fee", `"fee":{"amount":"0","type":"SHARES"}`, `"fee":{"amount":"-1","type":"SHARES"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(full, tc.from, tc.to, 1)
			if body == full {
				t.Fatalf("픽스처에서 %q 를 찾지 못했다 — 테스트가 아무것도 검사하지 않는다", tc.from)
			}
			c := matchesClient(t, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"success":true,"data":[`+body+`]}`)
			})
			if got, err := c.Matches(context.Background(), MatchQuery{}); err == nil {
				t.Fatalf("망가진 응답을 통과시켰다: %+v", got)
			}
		})
	}
}

func oneFill() string {
	return `{"quoteType":"Bid","amount":"2000000000000000000","price":"330000000000000000",` +
		`"outcome":{"name":"Up","indexSet":1,"onChainId":"111"},` +
		`"signer":"0xabc1234567890","hash":"0xdef1234567890","fee":{"amount":"0","type":"SHARES"}}`
}

func oneMatch(tx string) string {
	return `{"market":{"id":1},"taker":` + oneFill() + `,"makers":[],"transactionHash":"` + tx +
		`","settlementId":"s","executedAt":"2026-08-10T16:03:21.000Z"}`
}
