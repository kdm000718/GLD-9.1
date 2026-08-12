package claim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// DefaultChainRPC 는 eth_call 용 BSC 공개 노드다. 번들러 엔드포인트와 분리한
// 이유는 nonce 조회가 실패했을 때 원인을 나누기 위해서다 — 같은 주소를 쓰면
// "번들러가 죽었다"와 "체인 조회가 안 된다"가 한 덩어리가 된다.
const DefaultChainRPC = "https://bsc-dataseed.binance.org"

// ChainID 는 BSC 메인넷이다.
const ChainID = 56

const bundlerTimeout = 30 * time.Second

// Bundler 는 ZeroDev 번들러+페이마스터 엔드포인트다.
//
// URL 의 프로젝트 ID 가 곧 인증이다 — 별도 헤더가 없다. 그래서 이 값은
// 비밀이고 로그에 찍지 않는다([Redact] 참조).
type Bundler struct {
	// RPC 는 ZERODEV_RPC 다.
	RPC string
	// ChainRPC 는 eth_call 용 노드다. 비면 DefaultChainRPC.
	ChainRPC string
	// HTTP 는 시험이 주입한다.
	HTTP *http.Client
}

// GasPrice 는 zd_getUserOperationGasPrice 의 standard 등급이다.
//
// predict.fun 의 엔드포인트는 세 등급 모두 0 을 돌려준다(HAR 실측) —
// ULTRA_RELAY 가 번들러 층에서 후원하기 때문이다. 그래도 하드코딩하지 않고
// 매번 묻는다. 후원이 끊기면 0 이 아닌 값이 오고, 그때 우리는 BNB 가 없어
// 실패해야 한다. 0 을 못 박아 두면 그 변화가 보이지 않는다.
func (b Bundler) GasPrice(ctx context.Context) (maxFee, maxPriority *big.Int, err error) {
	var out struct {
		Standard struct {
			MaxFeePerGas         string `json:"maxFeePerGas"`
			MaxPriorityFeePerGas string `json:"maxPriorityFeePerGas"`
		} `json:"standard"`
	}
	if err := b.call(ctx, b.RPC, "zd_getUserOperationGasPrice", []any{}, &out); err != nil {
		return nil, nil, err
	}
	maxFee, err = parseHexInt(out.Standard.MaxFeePerGas, "maxFeePerGas")
	if err != nil {
		return nil, nil, err
	}
	maxPriority, err = parseHexInt(out.Standard.MaxPriorityFeePerGas, "maxPriorityFeePerGas")
	if err != nil {
		return nil, nil, err
	}
	return maxFee, maxPriority, nil
}

// GasEstimate 는 zd_sponsorUserOperation 이 채워 준 가스 한도들이다.
type GasEstimate struct {
	PreVerificationGas   *big.Int
	VerificationGasLimit *big.Int
	CallGasLimit         *big.Int
}

// Sponsor 는 zd_sponsorUserOperation 이다. **이것을 빠뜨리면 가스를 우리가
// 낸다** — 계정에 BNB 가 0 이라 실패한다.
//
// 서명 자리에는 [DummySignature] 를 넣는다. 이 단계는 서명을 검증하지 않지만
// 길이는 세기 때문이다.
func (b Bundler) Sponsor(ctx context.Context, op UserOp) (GasEstimate, error) {
	req := map[string]any{
		"chainId": ChainID,
		"userOp": map[string]any{
			"sender":               op.Sender,
			"nonce":                hexInt(op.Nonce),
			"callData":             "0x" + hexStr(op.CallData),
			"maxFeePerGas":         hexInt(op.MaxFeePerGas),
			"maxPriorityFeePerGas": hexInt(op.MaxPriorityFeePerGas),
			"signature":            DummySignature,
		},
		"entryPointAddress": EntryPoint,
		"shouldOverrideFee": false,
		"shouldConsume":     true,
	}
	var out struct {
		PreVerificationGas   string `json:"preVerificationGas"`
		VerificationGasLimit string `json:"verificationGasLimit"`
		CallGasLimit         string `json:"callGasLimit"`
	}
	if err := b.call(ctx, b.RPC, "zd_sponsorUserOperation", []any{req}, &out); err != nil {
		return GasEstimate{}, err
	}
	var est GasEstimate
	var err error
	if est.PreVerificationGas, err = parseHexInt(out.PreVerificationGas, "preVerificationGas"); err != nil {
		return GasEstimate{}, err
	}
	if est.VerificationGasLimit, err = parseHexInt(out.VerificationGasLimit, "verificationGasLimit"); err != nil {
		return GasEstimate{}, err
	}
	if est.CallGasLimit, err = parseHexInt(out.CallGasLimit, "callGasLimit"); err != nil {
		return GasEstimate{}, err
	}
	for name, v := range map[string]*big.Int{
		"preVerificationGas": est.PreVerificationGas, "verificationGasLimit": est.VerificationGasLimit,
		"callGasLimit": est.CallGasLimit,
	} {
		if v.Sign() <= 0 {
			return GasEstimate{}, fmt.Errorf("후원 응답의 %s 가 0 이다 — 그대로 보내면 검증 중 가스가 떨어진다", name)
		}
	}
	return est, nil
}

// Send 는 eth_sendUserOperation 이고 번들러가 계산한 userOpHash 를 돌려준다.
//
// 돌려받은 해시를 **우리가 계산한 해시와 대조하는 것은 호출자의 몫이다**
// ([Claimer.claimOne] 이 한다). 다르면 우리가 서명한 것과 번들러가 받은 것이
// 다르다는 뜻이고, 그 UserOperation 은 검증에서 떨어진다.
func (b Bundler) Send(ctx context.Context, op UserOp) (string, error) {
	if len(op.Signature) != 65 {
		return "", fmt.Errorf("서명이 65바이트가 아니다 (%d) — 서명하지 않은 UserOperation 을 보내려 한다", len(op.Signature))
	}
	body := map[string]any{
		"sender":                        op.Sender,
		"nonce":                         hexInt(op.Nonce),
		"callData":                      "0x" + hexStr(op.CallData),
		"callGasLimit":                  hexInt(op.CallGasLimit),
		"verificationGasLimit":          hexInt(op.VerificationGasLimit),
		"preVerificationGas":            hexInt(op.PreVerificationGas),
		"maxFeePerGas":                  hexInt(op.MaxFeePerGas),
		"maxPriorityFeePerGas":          hexInt(op.MaxPriorityFeePerGas),
		"paymasterVerificationGasLimit": "0x0",
		"paymasterPostOpGasLimit":       "0x0",
		"signature":                     "0x" + hexStr(op.Signature),
	}
	var out string
	if err := b.call(ctx, b.RPC, "eth_sendUserOperation", []any{body, EntryPoint}, &out); err != nil {
		return "", err
	}
	if len(strings.TrimPrefix(out, "0x")) != 64 {
		return "", fmt.Errorf("번들러가 돌려준 userOpHash 가 32바이트가 아니다: %q", out)
	}
	return out, nil
}

// Receipt 는 UserOperation 영수증이다. 아직 없으면 Found=false 다 —
// **에러가 아니다.** 둘을 섞으면 "아직 안 실렸다"와 "조회가 깨졌다"가 같은
// 값이 되고, 그러면 성공한 회수를 실패로 보고 다시 보내게 된다.
type Receipt struct {
	Found   bool
	Success bool
	TxHash  string
}

// Receipt 는 eth_getUserOperationReceipt 한 번이다.
func (b Bundler) Receipt(ctx context.Context, userOpHash string) (Receipt, error) {
	var out *struct {
		Success bool `json:"success"`
		Receipt struct {
			TransactionHash string `json:"transactionHash"`
		} `json:"receipt"`
		Logs []struct {
			TransactionHash string `json:"transactionHash"`
		} `json:"logs"`
	}
	if err := b.call(ctx, b.RPC, "eth_getUserOperationReceipt", []any{userOpHash}, &out); err != nil {
		return Receipt{}, err
	}
	if out == nil {
		return Receipt{Found: false}, nil
	}
	tx := out.Receipt.TransactionHash
	if tx == "" && len(out.Logs) > 0 {
		tx = out.Logs[0].TransactionHash
	}
	return Receipt{Found: true, Success: out.Success, TxHash: tx}, nil
}

// WaitReceipt 는 영수증이 나올 때까지 폴링한다.
//
// 시한을 넘기면 **에러**다. "못 봤다"를 성공으로 올리면 원장에 없는 회수가
// 기록되고, 반대로 실패로 두면 다음 주기에 다시 시도한다 — redeem 은
// 멱등이므로(이미 회수된 포지션은 0 을 준다) 재시도가 안전한 쪽이다.
func (b Bundler) WaitReceipt(ctx context.Context, userOpHash string, timeout, gap time.Duration) (Receipt, error) {
	deadline := time.Now().Add(timeout)
	for {
		r, err := b.Receipt(ctx, userOpHash)
		if err == nil && r.Found {
			return r, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return Receipt{}, fmt.Errorf("영수증 폴링이 시한(%s)을 넘겼다, 마지막 오류: %w", timeout, err)
			}
			return Receipt{}, fmt.Errorf("영수증이 시한(%s) 안에 나오지 않았다 (userOpHash %s) — 성공으로 치지 않는다", timeout, userOpHash)
		}
		select {
		case <-ctx.Done():
			return Receipt{}, ctx.Err()
		case <-time.After(gap):
		}
	}
}

// Nonce 는 EntryPoint.getNonce(sender, key) 를 체인에서 읽는다.
//
// key 는 [NonceKey] 가 만든 24바이트다. 시퀀스는 EntryPoint 가 그 key 에 대해
// 관리하므로 우리가 셀 필요가 없고, 세어서도 안 된다 — 우리가 모르는 사이에
// 다른 경로가 같은 계정으로 UserOperation 을 보내면 우리 카운터는 어긋난다.
func (b Bundler) Nonce(ctx context.Context, sender string, key []byte) (*big.Int, error) {
	data, err := GetNonceCalldata(sender, key)
	if err != nil {
		return nil, err
	}
	rpc := b.ChainRPC
	if rpc == "" {
		rpc = DefaultChainRPC
	}
	var raw string
	params := []any{map[string]string{"to": EntryPoint, "data": data}, "latest"}
	if err := b.call(ctx, rpc, "eth_call", params, &raw); err != nil {
		return nil, fmt.Errorf("getNonce: %w", err)
	}
	t := strings.TrimPrefix(raw, "0x")
	if len(t) != 64 {
		return nil, fmt.Errorf("getNonce 응답이 32바이트 워드가 아니다: %q", raw)
	}
	n, ok := new(big.Int).SetString(t, 16)
	if !ok {
		return nil, fmt.Errorf("getNonce 응답이 hex 가 아니다: %q", raw)
	}
	// 돌려받은 nonce 의 앞 24바이트가 우리가 넣은 key 와 같아야 한다.
	// 다르면 인자 패딩이 틀렸다는 뜻이고, 그 nonce 로 만든 UserOperation 은
	// 검증되지 않는 경로로 들어간다.
	full := make([]byte, 32)
	nb := n.Bytes()
	copy(full[32-len(nb):], nb)
	if !bytes.Equal(full[:24], key) {
		return nil, fmt.Errorf("getNonce 가 돌려준 key(0x%x)가 요청한 key(0x%x)와 다르다 — 인자 인코딩을 의심하라", full[:24], key)
	}
	return n, nil
}

// Redact 는 로그용이다. ZERODEV_RPC 의 프로젝트 ID 가 곧 인증이므로 통째로
// 찍으면 안 된다.
func Redact(rpc string) string {
	if rpc == "" {
		return "(없음)"
	}
	if i := strings.Index(rpc, "/api/"); i > 0 {
		return rpc[:i] + "/api/…"
	}
	return "(설정됨)"
}

// ------------------------------------------------------------------ JSON-RPC

func (b Bundler) call(ctx context.Context, rpc, method string, params []any, out any) error {
	if strings.TrimSpace(rpc) == "" {
		return fmt.Errorf("%s: RPC 주소가 비었다", method)
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return fmt.Errorf("%s 요청 인코딩: %w", method, err)
	}
	client := b.HTTP
	if client == nil {
		client = &http.Client{Timeout: bundlerTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpc, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%s 요청 생성: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s 요청: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s 응답 읽기: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s HTTP %d: %s", method, resp.StatusCode, clip(string(raw), 300))
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s 응답이 JSON 이 아니다: %s", method, clip(string(raw), 300))
	}
	if env.Error != nil {
		return fmt.Errorf("%s RPC 오류 %d: %s", method, env.Error.Code, clip(env.Error.Message, 300))
	}
	if len(env.Result) == 0 {
		return fmt.Errorf("%s 응답에 result 가 없다: %s", method, clip(string(raw), 300))
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("%s result 해석: %w", method, err)
	}
	return nil
}

func parseHexInt(s, name string) (*big.Int, error) {
	t := strings.TrimPrefix(strings.TrimSpace(s), "0x")
	if t == "" {
		return nil, fmt.Errorf("%s 가 비었다", name)
	}
	x, ok := new(big.Int).SetString(t, 16)
	if !ok {
		return nil, fmt.Errorf("%s 가 hex 정수가 아니다: %q", name, s)
	}
	return x, nil
}

func hexInt(x *big.Int) string {
	if x == nil || x.Sign() == 0 {
		return "0x0"
	}
	return "0x" + x.Text(16)
}

func hexStr(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
