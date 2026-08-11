// Package onchain 은 무장 전에 **체인에서** 확인해야 하는 것 하나를 본다:
// 담보가 있고, 거래소가 그것을 가져갈 수 있는가.
//
// # 왜 코드로 확인해야 하는가
//
// 2026-08-11 까지 `cmd/gld91` 의 무장 차단 목록은 문자열 하나를 **무조건**
// 돌려주고 있었다("USDT 승인과 자금 입금이 확인되지 않았다"). 자리표시자로는
// 옳았다 — 돈이 없는 동안은 어떤 값이든 그 답이 맞았으니까. 그러나 사용자가
// 실제로 승인을 마친 순간 그 문자열은 **거짓말이 된다.** 그리고 거짓말인 채로
// 무장을 막으므로, 고치는 방법이 "검사를 지운다" 로 보이기 시작한다. 그때
// 지우면 검사가 사라진 것이고, 자금 없이 무장하는 길이 열린다.
//
// 그래서 지우지 않고 **진짜 검사로 바꾼다.** 자리표시자를 실제 조회로 바꾸는
// 것이 이 패키지의 존재 이유 전부다.
//
// # 네 변종을 모두 본다
//
// 거래소 컨트랙트는 회차마다 `isNegRisk`·`isYieldBearing` 두 불린으로 넷 중
// 하나가 고른다(`internal/predictfun/order`). 어느 것이 올지는 회차 메타데이터를
// 받아야 알고, 그건 무장 판단보다 한참 뒤다. 하나만 승인돼 있으면 그 변종이
// 오는 날 주문이 서명까지 되고 정산에서 실패한다 — 가장 늦게, 가장 비싸게
// 드러나는 실패다. 그래서 호출자가 넘긴 **전부**를 요구한다.
//
// # 실패는 전부 차단이다
//
// RPC 가 안 되면 "승인됐는지 모른다" 이고, 모르면 무장하지 않는다. 이 패키지는
// 에러를 돌려주지 않고 **차단 사유 문자열**로 바꿔 돌려준다 — 호출자가 에러를
// 받으면 언젠가 그것을 "일시적 장애니 통과" 로 다루게 되기 때문이다.
package onchain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"
)

// USDT 는 BSC 메인넷 USDT 컨트랙트다. **소수점 18자리다** — 이더리움 쪽
// USDT(6자리)와 다르다. 6 으로 읽으면 잔고가 10^12 배로 보이고, 그 값은
// 어떤 최소 담보 검사도 통과한다.
const USDT = "0x55d398326f99059fF775485246999027B3197955"

// USDTDecimals 는 위 컨트랙트의 소수점 자리수다.
const USDTDecimals = 18

// DefaultRPC 는 BSC 공개 노드다.
const DefaultRPC = "https://bsc-dataseed.binance.org"

const rpcTimeout = 10 * time.Second

// ERC-20 셀렉터. keccak256 앞 4바이트다.
const (
	selBalanceOf = "0x70a08231" // balanceOf(address)
	selAllowance = "0xdd62ed3e" // allowance(address,address)
)

// Funding 은 자금·승인 조회다. 제로값으로 쓸 수 있다.
type Funding struct {
	// RPC 는 JSON-RPC 엔드포인트다. 비면 DefaultRPC.
	RPC string
	// Token 은 담보 토큰 컨트랙트다. 비면 USDT.
	Token string
	// HTTP 는 시험에서 갈아끼운다. 비면 rpcTimeout 짜리 기본 클라이언트.
	HTTP *http.Client
}

// Blockers 는 자금·승인 관점의 무장 차단 사유다. **빈 슬라이스면 통과다.**
//
// account 는 담보를 들고 있는 주소(=주문의 maker)다. spenders 는 이름→주소로,
// 넷 전부를 넘겨야 한다(패키지 문서 참고). minUnits 는 요구 최소 잔고를
// **최소 단위(wei)** 로 준다 — 실수로 받으면 18자리에서 정밀도가 깨진다.
func (f Funding) Blockers(ctx context.Context, account string, spenders map[string]string, minUnits *big.Int) []string {
	var out []string

	bal, err := f.balanceOf(ctx, account)
	if err != nil {
		return []string{fmt.Sprintf("담보 잔고를 조회하지 못했다 — 모르면 무장하지 않는다: %v", err)}
	}
	if minUnits != nil && bal.Cmp(minUnits) < 0 {
		out = append(out, fmt.Sprintf("담보가 부족하다: %s USDT (최소 %s)",
			format(bal), format(minUnits)))
	}

	// 이름순으로 돈다. map 순회 순서가 매번 달라지면 같은 상태에서 차단 사유
	// 목록의 순서가 바뀌고, 로그를 눈으로 대조할 수 없게 된다.
	names := make([]string, 0, len(spenders))
	for n := range spenders {
		names = append(names, n)
	}
	sort.Strings(names)

	var missing []string
	for _, n := range names {
		a, err := f.allowance(ctx, account, spenders[n])
		if err != nil {
			return []string{fmt.Sprintf("%s 승인액을 조회하지 못했다 — 모르면 무장하지 않는다: %v", n, err)}
		}
		// **0 만 미승인으로 본다.** 승인액이 잔고보다 작아도 그 금액까지는
		// 거래할 수 있고, 이 봇의 회차당 명목은 자본의 4.55% 다. 잔고 이상을
		// 요구하면 부분 승인이 전면 차단이 된다.
		if a.Sign() == 0 {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		out = append(out, fmt.Sprintf("USDT 승인이 없는 거래소 변종 %d개: %s",
			len(missing), strings.Join(missing, ", ")))
	}
	return out
}

func (f Funding) balanceOf(ctx context.Context, account string) (*big.Int, error) {
	w, err := word(account)
	if err != nil {
		return nil, err
	}
	return f.callUint(ctx, selBalanceOf+w)
}

func (f Funding) allowance(ctx context.Context, owner, spender string) (*big.Int, error) {
	wo, err := word(owner)
	if err != nil {
		return nil, err
	}
	ws, err := word(spender)
	if err != nil {
		return nil, err
	}
	return f.callUint(ctx, selAllowance+wo+ws)
}

// word 는 주소를 32바이트 워드로 좌측 0 패딩한다.
func word(addr string) (string, error) {
	h := strings.TrimPrefix(strings.TrimSpace(addr), "0x")
	if len(h) != 40 {
		return "", fmt.Errorf("주소가 40자리 hex 가 아니다 (길이 %d)", len(h))
	}
	for _, c := range h {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return "", fmt.Errorf("주소에 hex 가 아닌 문자가 있다")
		}
	}
	return strings.Repeat("0", 24) + strings.ToLower(h), nil
}

// callUint 는 eth_call 결과를 부호 없는 정수로 읽는다.
func (f Funding) callUint(ctx context.Context, data string) (*big.Int, error) {
	token := f.Token
	if token == "" {
		token = USDT
	}
	res, err := f.ethCall(ctx, token, data)
	if err != nil {
		return nil, err
	}
	h := strings.TrimPrefix(res, "0x")
	if h == "" {
		return nil, fmt.Errorf("결과가 비었다")
	}
	v, ok := new(big.Int).SetString(h, 16)
	if !ok {
		return nil, fmt.Errorf("결과가 hex 정수가 아니다: %s", truncate(res, 80))
	}
	return v, nil
}

// ethCall 은 JSON-RPC eth_call 한 번이다.
//
// HTTP 200 이어도 error 필드가 있으면 실패이고, 빈 result 도 실패다 — 빈
// 결과를 0 으로 읽으면 "승인이 없다"와 "노드가 이상하다"가 같은 값이 된다.
// 전자는 사용자가 할 일이 있고 후자는 없다.
func (f Funding) ethCall(ctx context.Context, to, data string) (string, error) {
	rpc := f.RPC
	if rpc == "" {
		rpc = DefaultRPC
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_call",
		"params": []any{map[string]string{"to": to, "data": data}, "latest"},
	})
	if err != nil {
		return "", fmt.Errorf("요청 인코딩: %w", err)
	}
	client := f.HTTP
	if client == nil {
		client = &http.Client{Timeout: rpcTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpc, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("요청 생성: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("요청: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("응답 읽기: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("응답이 JSON 이 아니다: %s", truncate(string(b), 200))
	}
	if out.Error != nil {
		return "", fmt.Errorf("RPC 에러: %s", truncate(out.Error.Message, 200))
	}
	if out.Result == "" {
		return "", fmt.Errorf("결과가 비었다: %s", truncate(string(b), 200))
	}
	return out.Result, nil
}

// Units 는 USDT 금액을 최소 단위로 바꾼다. 무장 검사의 임계값을 만들 때 쓴다.
func Units(usdt float64) *big.Int {
	f := new(big.Float).SetFloat64(usdt)
	f.Mul(f, new(big.Float).SetInt(pow10(USDTDecimals)))
	v, _ := f.Int(nil)
	return v
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// format 은 최소 단위를 사람이 읽는 USDT 문자열로 바꾼다. 표시 전용이다 —
// 비교는 언제나 정수로 한다.
func format(v *big.Int) string {
	q := new(big.Rat).SetFrac(v, pow10(USDTDecimals))
	return q.FloatString(4)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(잘림)"
}
