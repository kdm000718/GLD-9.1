package live

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/sha3"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// testAccount 는 자리표시자 주소다. **글자가 섞여 있어야 한다** — 0x1111…
// 같은 값은 대소문자 변환이 아무것도 바꾸지 않아 정규화 테스트를 무력화한다
// (P4 에서 실제로 그랬다).
const testAccount = "0xAbCd12Ef34aB56cD78eF90Ab12Cd34Ef56aB78Cd"

// testAccountLower 는 calldata 에 실려야 하는 모습이다.
const testAccountLower = "abcd12ef34ab56cd78ef90ab12cd34ef56ab78cd"

// staticToken 은 테스트용 TokenSource 다. 실제 JWT 는 auth.Authenticator 가 준다.
type staticToken struct{}

func (staticToken) Token(ctx context.Context) (string, error) { return "test-jwt-token", nil }

// 하드코딩한 셀렉터를 실제 keccak 과 대조한다. 이 상수가 틀리면 eth_call 이
// 빈 결과나 엉뚱한 값을 돌려주는데, 그 실패는 "잔고가 0" 과 구분되지 않는다.
func TestBalanceOfSelectorMatchesKeccak(t *testing.T) {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte("balanceOf(address)"))
	want := hex.EncodeToString(h.Sum(nil)[:4])
	if balanceOfSelector != want {
		t.Errorf("셀렉터 = %s, keccak 앞 4바이트는 %s", balanceOfSelector, want)
	}
}

// BSC 의 USDT 는 **18 decimals** 다. 이더리움의 6 이 아니다.
// 이 상수가 6 으로 바뀌면 잔고가 10^12 배로 보이고 한도가 사실상 사라진다.
func TestUSDTDecimalsIsEighteen(t *testing.T) {
	if USDTDecimals != 18 {
		t.Fatalf("USDTDecimals = %d — BSC 의 USDT 는 18 이다", USDTDecimals)
	}
	// 1 USDT = 10^18 wei 인지 실제 변환으로 확인한다. 상수만 보면 변환식이
	// 다른 지수를 쓰도록 바뀌어도 통과한다.
	one, _ := new(big.Int).SetString("1000000000000000000", 10)
	got, err := weiToUSDT(one)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Errorf("10^18 wei = %v USDT, 기대 1", got)
	}
}

// --- 온체인 조회 ---

// rpcServer 는 eth_call 요청을 기록하고 정해진 결과를 돌려준다.
type rpcCall struct {
	To   string `json:"to"`
	Data string `json:"data"`
}

func rpcServer(t *testing.T, result string, rpcErr string) (url string, calls *[]rpcCall) {
	t.Helper()
	var got []rpcCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("RPC 요청이 JSON 이 아니다: %s", b)
		}
		if req.Method != "eth_call" {
			t.Errorf("method = %q, 기대 eth_call", req.Method)
		}
		if len(req.Params) > 0 {
			var c rpcCall
			_ = json.Unmarshal(req.Params[0], &c)
			got = append(got, c)
		}
		w.Header().Set("Content-Type", "application/json")
		if rpcErr != "" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":%q}}`, rpcErr)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%q}`, result)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &got
}

// word 는 정수를 32바이트 hex 워드로 만든다.
func word(v *big.Int) string {
	return "0x" + fmt.Sprintf("%064s", v.Text(16))
}

// predictServer 는 예약잔고와 포지션을 돌려주는 가짜 predict.fun 이다.
func predictServer(t *testing.T, reservedWei string, positions string) *rest.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	c := rest.New("test-api-key-placeholder")
	c.BaseURL = srv.URL
	c.SetTokenSource(staticToken{})
	return c
}

func source(t *testing.T, rpcURL string, rc *rest.Client) *EquitySource {
	t.Helper()
	return &EquitySource{Rest: rc, RPC: rpcURL, Account: testAccount}
}

// calldata 배치: 셀렉터 + 왼쪽 0 패딩 24자 + 소문자 주소 40자.
// 대소문자가 섞인 주소를 그대로 실으면 노드에 따라 조회가 어긋난다.
func TestReadBuildsBalanceOfCall(t *testing.T) {
	rpc, calls := rpcServer(t, word(big.NewInt(0)), "")
	rc := predictServer(t, "", "")

	if _, err := source(t, rpc, rc).Read(context.Background()); err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("eth_call %d회, 기대 1회", len(*calls))
	}
	c := (*calls)[0]
	wantData := "0x" + balanceOfSelector + strings.Repeat("0", 24) + testAccountLower
	if c.Data != wantData {
		t.Errorf("calldata = %s\n기대       = %s", c.Data, wantData)
	}
	if c.To != strings.ToLower(USDTBSC) {
		t.Errorf("to = %s, 기대 %s", c.To, strings.ToLower(USDTBSC))
	}
}

// 가용 잔고 = 온체인 잔고 − 예약. 예약을 빼지 않으면 같은 돈을 두 번 건다.
func TestReadSubtractsReserved(t *testing.T) {
	balance, _ := new(big.Int).SetString("250000000000000000000", 10) // 250 USDT
	rpc, _ := rpcServer(t, word(balance), "")
	rc := predictServer(t, "40000000000000000000", "") // 40 USDT 예약

	e, err := source(t, rpc, rc).Read(context.Background())
	if err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if e.AvailableUSDT != 210 {
		t.Errorf("가용 %v USDT, 기대 210 (250 − 40)", e.AvailableUSDT)
	}
	if e.PositionCost != 0 {
		t.Errorf("IncludePositions 가 false 인데 취득원가 %v", e.PositionCost)
	}
}

// IncludePositions 면 취득원가(주식 수 × 평균 매입가)가 더해진다.
func TestReadIncludesPositionCost(t *testing.T) {
	balance, _ := new(big.Int).SetString("100000000000000000000", 10) // 100 USDT
	rpc, _ := rpcServer(t, word(balance), "")
	// 20주 × 0.3 = 6 USDT
	rc := predictServer(t, "", `[{"amount":"20000000000000000000","averageBuyPriceUsd":"0.3","valueUsd":"7","pnlUsd":"1"}]`)

	s := source(t, rpc, rc)
	s.IncludePositions = true
	e, err := s.Read(context.Background())
	if err != nil {
		t.Fatalf("조회 실패: %v", err)
	}
	if e.AvailableUSDT != 100 {
		t.Errorf("가용 %v, 기대 100", e.AvailableUSDT)
	}
	if e.PositionCost != 6 {
		t.Errorf("취득원가 %v, 기대 6", e.PositionCost)
	}
}

// **읽지 못하면 0 이 아니라 에러다.** 0 이면 risk.CanArm 이 false 라 우연히
// 막히지만, 그 우연에 기대는 가드는 이 저장소가 반복해서 값을 치른 자리다.
func TestReadFailsInsteadOfReturningZero(t *testing.T) {
	balance := word(big.NewInt(1))
	cases := []struct {
		name   string
		rpcRes string
		rpcErr string
		rest   func(t *testing.T) *rest.Client
		want   string
	}{
		{
			// eth_call 은 컨트랙트가 아닌 주소나 revert 에서 "0x" 를 준다.
			// 0 으로 읽으면 "잔고 없음" 과 "주소가 틀렸다" 가 같아진다.
			name: "빈 워드(0x)", rpcRes: "0x",
			want: "32바이트 워드가 아니다",
		},
		{name: "result 필드가 비었다", rpcRes: "", want: "RPC 결과가 비었다"},
		{name: "짧은 워드", rpcRes: "0x1234", want: "32바이트 워드가 아니다"},
		{name: "hex 가 아니다", rpcRes: "0x" + strings.Repeat("z", 64), want: "hex 가 아니다"},
		{name: "RPC 에러", rpcRes: balance, rpcErr: "execution reverted", want: "RPC 에러"},
		{
			name: "예약잔고 조회 실패", rpcRes: balance,
			rest: func(t *testing.T) *rest.Client {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusInternalServerError)
				}))
				t.Cleanup(srv.Close)
				c := rest.New("test-api-key-placeholder")
				c.BaseURL = srv.URL
				c.SetTokenSource(staticToken{})
				return c
			},
			want: "예약잔고 조회 실패",
		},
		{
			// 금액 필드 이름이 바뀌면 rest.ReservedUSDT 가 에러를 낸다.
			// 그것을 0 으로 흘려보내면 자유 자금을 과대평가한다.
			name: "예약잔고 금액 필드를 모름", rpcRes: balance,
			rest: func(t *testing.T) *rest.Client {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					fmt.Fprint(w, `{"success":true,"data":[{"type":"USDT","qty":"1"}]}`)
				}))
				t.Cleanup(srv.Close)
				c := rest.New("test-api-key-placeholder")
				c.BaseURL = srv.URL
				c.SetTokenSource(staticToken{})
				return c
			},
			want: "금액 필드를 못 찾았다",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rpc, _ := rpcServer(t, tc.rpcRes, tc.rpcErr)
			rc := predictServer(t, "", "")
			if tc.rest != nil {
				rc = tc.rest(t)
			}
			e, err := source(t, rpc, rc).Read(context.Background())
			if err == nil {
				t.Fatalf("에러가 없다 — equity %+v 를 돌려줬다", e)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("에러가 %q 를 담지 않았다: %v", tc.want, err)
			}
			if e.AvailableUSDT != 0 || e.PositionCost != 0 {
				t.Errorf("에러인데 equity 가 채워졌다: %+v", e)
			}
		})
	}
}

// 예약이 잔고보다 크면 우리가 세는 방식이 거래소와 다르다는 뜻이다.
// 0 으로 깎아 넘기면 다음에 부호나 단위가 틀렸을 때 아무도 모른다.
func TestReadRejectsReservedExceedingBalance(t *testing.T) {
	rpc, _ := rpcServer(t, word(big.NewInt(1_000_000_000_000_000_000)), "") // 1 USDT
	rc := predictServer(t, "2000000000000000000", "")                       // 2 USDT 예약

	e, err := source(t, rpc, rc).Read(context.Background())
	if err == nil {
		t.Fatalf("에러가 없다: %+v", e)
	}
	// **센티널이어야 한다.** 배선(cmd/gld91)이 이 실패를 네트워크 실패와
	// 구분해서 세야 24시간 DRY-RUN 에서 "가정이 틀렸다"가 로그로 드러난다.
	// 문자열 매칭으로 두면 메시지를 다듬는 순간 그 구분이 조용히 사라진다.
	if !errors.Is(err, ErrReservedExceedsBalance) {
		t.Errorf("ErrReservedExceedsBalance 가 아니다: %v", err)
	}
}

// rest.Client 가 없으면 예약잔고를 모른다 — 모르는 채로 자유 자금을 세지 않는다.
func TestReadRequiresRestClient(t *testing.T) {
	rpc, _ := rpcServer(t, word(big.NewInt(0)), "")
	s := &EquitySource{RPC: rpc, Account: testAccount}
	if _, err := s.Read(context.Background()); err == nil {
		t.Error("rest.Client 없이 통과했다")
	}
}

// 주소가 이상하면 조회 자체를 하지 않는다. 잘못된 주소로 부른 balanceOf 는
// 0 을 돌려주고, 그 0 은 "돈이 없다" 와 구분되지 않는다.
func TestReadRejectsBadAddresses(t *testing.T) {
	cases := []struct{ name, account, token string }{
		{"계정이 비었다", "", ""},
		{"계정이 짧다", "0xabcd", ""},
		{"계정이 hex 가 아니다", "0x" + strings.Repeat("z", 40), ""},
		{"계정이 0 주소", "0x" + strings.Repeat("0", 40), ""},
		{"토큰이 짧다", testAccount, "0x1234"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rpc, calls := rpcServer(t, word(big.NewInt(0)), "")
			rc := predictServer(t, "", "")
			s := &EquitySource{Rest: rc, RPC: rpc, Account: tc.account, USDTToken: tc.token}
			if _, err := s.Read(context.Background()); err == nil {
				t.Fatal("에러가 없다")
			}
			if len(*calls) != 0 {
				t.Errorf("주소가 이상한데 RPC 를 %d회 불렀다", len(*calls))
			}
		})
	}
}

// **소수 자리 가정이 틀린 방향을 잡는 마지막 가드.**
// 18 대신 6 이라고 믿으면 잔고가 10^12 배로 보인다. 그 값으로 만든 cap 은
// 사실상 무한대이고, 한도 없는 봇이 된다.
func TestReadRejectsImplausibleBalance(t *testing.T) {
	// 10^31 wei = 10^13 USDT — USDT 총발행량(1.5×10^11)의 60배가 넘는다.
	huge, _ := new(big.Int).SetString("1"+strings.Repeat("0", 31), 10)
	rpc, _ := rpcServer(t, word(huge), "")
	rc := predictServer(t, "", "")
	if e, err := source(t, rpc, rc).Read(context.Background()); err == nil {
		t.Fatalf("에러가 없다: %+v", e)
	}
}

// uint256 최대값도 +Inf 가 아니라 에러로 떨어진다 — float64 로 먼저 바꾸면
// +Inf 가 되고, +Inf × 0.0455 는 여전히 +Inf 다.
func TestWeiToUSDTHandlesMaxUint256(t *testing.T) {
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	v, err := weiToUSDT(max)
	if err == nil {
		t.Fatalf("에러가 없다 (= %v USDT)", v)
	}
	if v != 0 {
		t.Errorf("에러인데 %v 를 돌려줬다", v)
	}
}

// 잔고 0 은 에러가 아니다 — "돈이 없다" 는 우리가 아는 사실이다.
// (무장하지 않는 판단은 risk.CanArm 이 한다.)
func TestReadAllowsZeroBalance(t *testing.T) {
	rpc, _ := rpcServer(t, word(big.NewInt(0)), "")
	rc := predictServer(t, "", "")
	e, err := source(t, rpc, rc).Read(context.Background())
	if err != nil {
		t.Fatalf("잔고 0 이 에러가 됐다: %v", err)
	}
	if e.AvailableUSDT != 0 {
		t.Errorf("가용 %v, 기대 0", e.AvailableUSDT)
	}
}
