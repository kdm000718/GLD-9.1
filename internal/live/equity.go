package live

// 이 파일은 **"가용 USDT 를 어디서 읽는가"의 가정을 전부 담는다.**
//
// # 왜 한 파일인가
//
// 실주문 1건(Task 10 Step 4~5)이 아직 안 돌았다. 그래서 "봇이 굴릴 수 있는
// 돈"의 출처가 확정되지 않았다. 지금 쓰는 가정은 이것이다:
//
//	가용 USDT = 온체인 USDT.balanceOf(PREDICT_ACCOUNT) − 미체결 주문이 묶어 둔 USDT
//
// 실주문이 진짜 답을 확정하면(예: predict.fun 이 별도의 내부 잔고를 갖고
// 있다면) **이 파일만 고친다.** 다른 파일이 이 가정을 알면 격리가 깨지고,
// 그때 고쳐야 할 자리가 흩어진다 — 이 저장소가 미확정 값을 다루는 방식이다.
//
// # 실패 방향
//
// **읽지 못하면 0 이 아니라 에러다.** 0 을 돌려주면 risk.CanArm 이 false 라
// 우연히 막히지만, 우연에 기대는 가드는 이 저장소가 반복해서 값을 치른
// 자리다. 반대로 예약잔고를 0 으로 잘못 읽으면 자유 자금을 과대평가해
// 한도를 넘겨 주문한다 — 실제 돈이 나가는 방향이다.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

// ErrReservedExceedsBalance 는 예약잔고가 온체인 잔고보다 크다는 뜻이다.
//
// **센티널로 둔 이유는 로그에서 구분하기 위해서다.** 이것은 일시적 실패가
// 아니라 "우리가 세는 방식이 거래소와 다르다"는 신호다 — 예약잔고 응답의
// 단위(wei vs USDT)나 필드 해석이 틀렸을 때의 모습이고, 그렇다면 자유 자금
// 계산 전체가 틀렸다는 뜻이다. 24시간 DRY-RUN 에서 이 에러가 **반복되면**
// 가정이 틀린 것이므로, 배선(cmd/gld91)이 네트워크 실패와 섞지 않고 따로
// 세어 보고할 수 있어야 한다. 문자열 매칭으로 구분하게 두면 메시지를 다듬는
// 순간 그 구분이 조용히 사라진다.
var ErrReservedExceedsBalance = errors.New("예약잔고가 온체인 잔고보다 크다")

const (
	// USDTBSC 는 BSC 메인넷 USDT(BSC-USD) 컨트랙트다.
	USDTBSC = "0x55d398326f99059fF775485246999027B3197955"

	// USDTDecimals 는 **18** 이다. 6 이 아니다 — 이더리움 USDT 는 6 이지만
	// BSC 의 USDT 는 18 이고, 이 저장소는 그것을 이미 확인했다
	// (order.weiDecimals 도 같은 값으로 주문 금액을 만든다).
	//
	// 이 값이 틀리면 잔고가 10^12 배 어긋난다. 6 으로 잘못 잡으면 잔고가
	// 1조 배로 보여 한도가 사실상 사라진다 — 아래 maxPlausibleUSDT 가
	// 그 방향을 막는 마지막 가드다.
	USDTDecimals = 18

	// DefaultRPC 는 BSC 공개 노드다. 설정으로 바꿀 수 있다(EquitySource.RPC).
	DefaultRPC = "https://bsc-dataseed.binance.org"

	// balanceOfSelector 는 keccak256("balanceOf(address)") 의 앞 4바이트다.
	// equity_test.go 의 TestBalanceOfSelectorMatchesKeccak 이 실제 keccak 과
	// 대조한다 — 상수만 두면 틀렸을 때 "잔고 0" 과 구분되지 않는다.
	balanceOfSelector = "70a08231"
)

// maxPlausibleUSDT 는 받아들일 수 있는 잔고의 상한이다.
//
// USDT 총발행량은 1.5×10^11 규모다. 그 몇 배를 넘는 값이 나왔다면 잔고가
// 아니라 **단위가 틀린 것**이다(가장 그럴듯한 원인: 소수 자리 가정이 18 이
// 아니게 바뀜, 또는 토큰 주소가 다른 컨트랙트를 가리킴). 그 값으로 계산한
// cap 은 사실상 무한대이고, 봇은 한도 없이 베팅한다.
const maxPlausibleUSDT = 1e12

// rpcTimeout 은 eth_call 한 번의 상한이다. 회차 시작에 한 번 부르는 호출이라
// 길게 잡을 이유가 없다 — 늦게 오는 답은 이미 늦은 회차의 답이다.
const rpcTimeout = 20 * time.Second

// EquitySource 는 risk.Equity 를 만들어 주는 조회기다.
type EquitySource struct {
	// Rest 는 예약잔고·포지션 조회에 쓴다. nil 이면 에러다 — 예약잔고를
	// 모르는 채 0 으로 두면 자유 자금을 과대평가한다.
	Rest *rest.Client
	// RPC 는 BSC JSON-RPC 엔드포인트다. 비면 DefaultRPC.
	RPC string
	// Account 는 잔고를 읽을 주소다(PREDICT_ACCOUNT — 스마트계정).
	// **서명자 EOA 가 아니다.** GET /v1/account 의 address 로 유도하면
	// 자금 없는 EOA 의 잔고를 읽는다(P4 실측).
	Account string
	// USDTToken 은 잔고를 읽을 ERC20 이다. 비면 USDTBSC.
	USDTToken string
	// IncludePositions 가 true 면 미정산 포지션의 취득원가를 equity 에 더한다.
	//
	// 기본값 false 가 보수적인 쪽이다: PositionCost 는 cap 을 **키우는** 항이라
	// (cap = (가용+취득원가) × 4.55%), 빼면 덜 걸고 더하면 더 건다. 포지션
	// 조회가 확정되기 전까지는 덜 거는 쪽에 둔다.
	IncludePositions bool
	// HTTP 는 테스트가 주입한다. nil 이면 rpcTimeout 짜리 기본 클라이언트.
	HTTP *http.Client
}

// Read 는 지금의 equity 를 만든다.
//
// 세 조회가 전부 성공해야 값이 나온다(IncludePositions 가 false 면 둘).
// 하나라도 실패하면 에러다 — 부분적으로 성공한 equity 는 그것이 부분적이라는
// 사실을 담을 자리가 없고, 그 사실이 사라진 값으로 베팅 크기가 정해진다.
func (s *EquitySource) Read(ctx context.Context) (risk.Equity, error) {
	if s == nil {
		return risk.Equity{}, fmt.Errorf("equity 조회: EquitySource 가 nil 이다")
	}
	if s.Rest == nil {
		return risk.Equity{}, fmt.Errorf("equity 조회: rest.Client 가 배선되지 않았다 — 예약잔고를 모르는 채로 자유 자금을 셀 수 없다")
	}

	balance, err := s.onChainUSDT(ctx)
	if err != nil {
		return risk.Equity{}, err
	}

	// 미체결 주문이 묶어 둔 USDT 는 아직 지갑에 있다(오더북이 오프체인이다).
	// 빼지 않으면 같은 돈을 두 번 걸 수 있다고 믿게 된다.
	reserved, err := s.Rest.ReservedUSDT(ctx)
	if err != nil {
		return risk.Equity{}, fmt.Errorf("equity 조회: %w", err)
	}
	if reserved < 0 {
		return risk.Equity{}, fmt.Errorf("equity 조회: 예약잔고가 음수다 (%v)", reserved)
	}

	available := balance - reserved
	if !finite(available) {
		return risk.Equity{}, fmt.Errorf("equity 조회: 가용 잔고가 유한하지 않다 (잔고 %v, 예약 %v)", balance, reserved)
	}
	// 예약이 잔고보다 크다는 것은 우리가 세는 방식이 거래소와 다르다는 뜻이다.
	// 0 으로 깎아 넘기면 risk 가 "돈이 없다"로 읽어 우연히 멈추지만, 그 우연에
	// 기대면 다음에 부호나 단위가 틀렸을 때 아무도 모른다.
	if available < 0 {
		return risk.Equity{}, fmt.Errorf("equity 조회: %w (예약 %v, 잔고 %v) — 예약잔고 응답의 단위나 필드 해석을 의심하라",
			ErrReservedExceedsBalance, reserved, balance)
	}

	e := risk.Equity{AvailableUSDT: available}
	if s.IncludePositions {
		cost, err := s.positionCost(ctx)
		if err != nil {
			return risk.Equity{}, err
		}
		e.PositionCost = cost
	}
	return e, nil
}

// positionCost 는 미정산 포지션의 **취득원가** 합이다. 시가 평가가 아니다 —
// risk.Equity 의 주석이 그 이유를 적고 있다(정산되면 0 아니면 1 이 될 값을
// 얇은 호가창의 중간값으로 재면 cap 이 흔들린다).
func (s *EquitySource) positionCost(ctx context.Context) (float64, error) {
	ps, err := s.Rest.Positions(ctx)
	if err != nil {
		return 0, fmt.Errorf("equity 조회: %w", err)
	}
	total := 0.0
	for i, p := range ps {
		c := p.CostUSD()
		if !finite(c) || c < 0 {
			return 0, fmt.Errorf("equity 조회: %d번째 포지션의 취득원가가 %v 다", i, c)
		}
		total += c
	}
	if !finite(total) || total < 0 {
		return 0, fmt.Errorf("equity 조회: 취득원가 합이 %v 다", total)
	}
	if total > maxPlausibleUSDT {
		return 0, fmt.Errorf("equity 조회: 취득원가 합 %v USDT 가 비현실적이다 — 포지션 응답의 단위를 의심하라", total)
	}
	return total, nil
}

// onChainUSDT 는 USDT.balanceOf(Account) 를 USDT 단위 float64 로 돌려준다.
func (s *EquitySource) onChainUSDT(ctx context.Context) (float64, error) {
	account, err := normalizeAddress(s.Account)
	if err != nil {
		return 0, fmt.Errorf("equity 조회: PREDICT_ACCOUNT: %w", err)
	}
	token := s.USDTToken
	if token == "" {
		token = USDTBSC
	}
	tokenAddr, err := normalizeAddress(token)
	if err != nil {
		return 0, fmt.Errorf("equity 조회: USDT 토큰 주소: %w", err)
	}
	rpc := s.RPC
	if rpc == "" {
		rpc = DefaultRPC
	}

	// ABI 인코딩에서 address 는 32바이트로 왼쪽을 0 으로 채운다.
	data := "0x" + balanceOfSelector + strings.Repeat("0", 24) + account
	raw, err := s.ethCall(ctx, rpc, "0x"+tokenAddr, data)
	if err != nil {
		return 0, fmt.Errorf("equity 조회: %w", err)
	}
	wei, err := decodeUint256(raw)
	if err != nil {
		return 0, fmt.Errorf("equity 조회: balanceOf 응답 해석: %w", err)
	}
	return weiToUSDT(wei)
}

// weiToUSDT 는 18 decimals 정수를 USDT 단위 float64 로 바꾼다.
//
// big.Float 을 거치는 이유: 잔고는 최대 2^256−1 이고 그것을 float64 로 먼저
// 바꾸면 +Inf 가 된다. 나눗셈을 큰 정밀도에서 하고 마지막에 한 번만 좁힌다.
func weiToUSDT(wei *big.Int) (float64, error) {
	if wei.Sign() < 0 {
		// uint256 을 그대로 읽으므로 음수가 나올 수 없다. 나왔다면 디코딩이
		// 부호 있는 정수로 바뀐 것이고, 그 값으로는 아무것도 계산하지 않는다.
		return 0, fmt.Errorf("잔고가 음수다 (%s wei)", wei.String())
	}
	scale := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(USDTDecimals), nil))
	f := new(big.Float).SetPrec(300).SetInt(wei)
	f.Quo(f, scale)
	v, _ := f.Float64()
	if !finite(v) {
		return 0, fmt.Errorf("잔고가 float64 로 표현되지 않는다 (%s wei)", wei.String())
	}
	if v > maxPlausibleUSDT {
		return 0, fmt.Errorf("잔고 %v USDT 가 비현실적이다 (%s wei) — 토큰 주소나 소수 자리(%d) 가정을 의심하라",
			v, wei.String(), USDTDecimals)
	}
	return v, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// normalizeAddress 는 0x 접두사를 떼고 소문자 40자리 hex 로 만든다.
// EIP-55 체크섬 표기와 소문자 표기는 같은 주소를 가리키지만 문자열로는 다르다.
func normalizeAddress(s string) (string, error) {
	t := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	if len(t) != 40 {
		return "", fmt.Errorf("40자리 hex 여야 한다 (0x 제외 %d자)", len(t))
	}
	if _, err := hex.DecodeString(t); err != nil {
		return "", fmt.Errorf("hex 가 아니다: %w", err)
	}
	if strings.Trim(t, "0") == "" {
		return "", fmt.Errorf("0 주소다")
	}
	return strings.ToLower(t), nil
}

// decodeUint256 은 eth_call 이 돌려준 32바이트 워드를 정수로 읽는다.
//
// **빈 결과("0x")는 0 이 아니라 에러다.** eth_call 은 컨트랙트가 아닌 주소나
// revert 에서 "0x" 를 준다. 그걸 0 으로 읽으면 "잔고 없음"과 "주소가 틀렸다"가
// 같은 값이 되고, 봇은 조용히 무장하지 않은 채 원인을 남기지 않는다.
func decodeUint256(result string) (*big.Int, error) {
	t := strings.TrimPrefix(strings.TrimSpace(result), "0x")
	if len(t) != 64 {
		return nil, fmt.Errorf("32바이트 워드가 아니다 (0x 제외 %d자) — 토큰 주소나 셀렉터를 의심하라", len(t))
	}
	if _, err := hex.DecodeString(t); err != nil {
		return nil, fmt.Errorf("hex 가 아니다: %w", err)
	}
	v, ok := new(big.Int).SetString(t, 16)
	if !ok {
		return nil, fmt.Errorf("16진수로 읽지 못했다")
	}
	return v, nil
}

// ethCall 은 JSON-RPC eth_call 한 번이다.
//
// cmd/signercheck 의 같은 이름 함수와 규약이 같다(HTTP 200 이어도 error 필드가
// 있으면 실패, 빈 result 는 실패). 그쪽은 진단 바이너리라 공유하지 않는다 —
// 대신 이 파일 하나만 고치면 되게 두는 쪽을 택했다.
func (s *EquitySource) ethCall(ctx context.Context, rpc, to, data string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_call",
		"params": []any{map[string]string{"to": to, "data": data}, "latest"},
	})
	if err != nil {
		return "", fmt.Errorf("RPC 요청 인코딩: %w", err)
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: rpcTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpc, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("RPC 요청 생성: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("RPC 요청: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("RPC 응답 읽기: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("RPC HTTP %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var out struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("RPC 응답이 JSON 이 아니다: %s", truncate(string(b), 200))
	}
	if out.Error != nil {
		return "", fmt.Errorf("RPC 에러: %s", truncate(out.Error.Message, 200))
	}
	if out.Result == "" {
		return "", fmt.Errorf("RPC 결과가 비었다: %s", truncate(string(b), 200))
	}
	return out.Result, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
