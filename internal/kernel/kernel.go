// Package kernel 은 predict.fun 계정(ZeroDev Kernel 스마트계정)의 **등록
// 서명자를 체인에서 확인**한다.
//
// # 왜 별도 패키지인가
//
// 이 대조는 원래 cmd/signercheck 안에 있었다. cmd/gld91 의 기동 자가 점검이
// "signercheck 와 같은 대조"를 해야 하는데, package main 의 함수는 다른
// 바이너리가 부를 수 없다. 복사하면 두 벌이 되고 두 벌은 갈린다 — 이 저장소는
// 회차 선택 규칙을 두 벌로 뒀다가 실제로 값을 치렀고(cmd/probe 의
// fetchLiveRounds 를 internal/live 로 승격한 이유), 같은 실수를 반복하지
// 않으려고 여기로 올린다. **진단 도구와 실거래 봇이 글자 그대로 같은 함수를
// 부른다.**
//
// # 이 대조가 없으면 무슨 일이 일어나는가
//
// predict.fun 계정은 자금을 든 스마트계정 주소와 주문에 서명하는 EOA 가
// 다르다. 키가 그 계정의 등록 서명자가 아니면 **주문 서명이 전부 거부된다** —
// 코드가 아무리 맞아도. 2026-08-10 에 실제로 그 상태였다. 이 대조 없이
// 진행하면 온체인 승인에 가스를 먼저 쓰고, 첫 주문이 거부되고 나서야 원인을
// 찾는다.
//
// # 키는 이 패키지에 들어오지 않는다
//
// 받는 것은 이미 유도된 주소 문자열뿐이다. 키에서 주소를 뽑는 일은
// auth.NewSigner 가 하고, 그 결과만 여기로 온다.
package kernel

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ECDSAValidator 는 predict.fun 의 ECDSA_VALIDATOR 컨트랙트다. BNB 메인넷과
// 테스트넷이 같은 주소를 쓴다(스펙 §6 계약 표).
const ECDSAValidator = "0x845ADb2C711129d4f3966735eD98a9F09fC4cE57"

// DefaultRPC 는 BSC 공개 노드다.
const DefaultRPC = "https://bsc-dataseed.binance.org"

// ecdsaValidatorStorageSelector 는 keccak256("ecdsaValidatorStorage(address)")
// 의 앞 4바이트다. Kernel 계정의 ECDSA 검증자가 그 계정에 대해 등록해 둔
// 소유자(= 주문에 서명할 수 있는 EOA)를 돌려주는 게터다.
//
// 값을 하드코딩하고 keccak 을 런타임에 돌리지 않는 이유: 이 상수가 틀리면
// eth_call 이 빈 결과나 엉뚱한 값을 돌려주는데, 그 실패는 "서명자가 없다"와
// 구분이 안 된다. kernel_test.go 의 TestSelectorMatchesKeccak 이 실제 keccak 과
// 대조해 고정한다 — 상수와 검증을 한 곳에 두면 둘이 같이 틀린다.
const ecdsaValidatorStorageSelector = "20709efc"

// rpcTimeout 은 eth_call 한 번의 상한이다. 기동에 한 번 부르는 호출이다.
const rpcTimeout = 20 * time.Second

// CallData 는 ecdsaValidatorStorage(account) 호출의 calldata 를 만든다.
// ABI 인코딩에서 address 는 32바이트로 왼쪽을 0 으로 채운다.
func CallData(account string) (string, error) {
	a, err := NormalizeAddress(account)
	if err != nil {
		return "", fmt.Errorf("계정 주소: %w", err)
	}
	return "0x" + ecdsaValidatorStorageSelector + strings.Repeat("0", 24) + a, nil
}

// NormalizeAddress 는 0x 접두사를 떼고 소문자 40자리 hex 로 만든다.
//
// 대소문자를 없애는 이유는 EIP-55 체크섬 표기와 소문자 표기가 같은 주소를
// 가리키는데 문자열로 비교하면 서로 다르다고 판정하기 때문이다 — 이 패키지의
// 유일한 목적이 두 주소의 일치 여부이므로 여기서 틀리면 전부 틀린다.
func NormalizeAddress(s string) (string, error) {
	t := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(s), "0x"), "0X")
	if len(t) != 40 {
		return "", fmt.Errorf("40자리 hex여야 한다 (0x 제외 %d자)", len(t))
	}
	if _, err := hex.DecodeString(t); err != nil {
		return "", fmt.Errorf("hex가 아니다: %w", err)
	}
	return strings.ToLower(t), nil
}

// DecodeAddressResult 는 eth_call 이 돌려준 32바이트 워드에서 주소를 뽑는다.
//
// 상위 12바이트가 0 이 아니면 거부한다. 그 경우는 반환값이 주소가 아니라는
// 뜻이고(잘못된 셀렉터·잘못된 컨트랙트·다른 타입), 뒤 20바이트만 잘라 쓰면
// 쓰레기를 "등록된 서명자"라고 보고하게 된다.
//
// 전부 0인 결과는 별도로 구분한다 — "등록된 서명자가 없다"는 사실이지
// 파싱 실패가 아니다.
func DecodeAddressResult(result string) (addr string, zero bool, err error) {
	t := strings.TrimPrefix(strings.TrimSpace(result), "0x")
	if len(t) != 64 {
		return "", false, fmt.Errorf("32바이트 워드가 아니다 (0x 제외 %d자) — 셀렉터나 컨트랙트 주소를 의심하라", len(t))
	}
	if _, err := hex.DecodeString(t); err != nil {
		return "", false, fmt.Errorf("hex가 아니다: %w", err)
	}
	if strings.Trim(t[:24], "0") != "" {
		return "", false, fmt.Errorf("상위 12바이트가 0이 아니다 (%s) — 주소 반환값이 아니다", "0x"+t[:24])
	}
	low := strings.ToLower(t[24:])
	if strings.Trim(low, "0") == "" {
		return "", true, nil
	}
	return low, false, nil
}

// Match 는 유도된 서명자 주소와 체인에 등록된 주소가 같은지다.
// 둘 다 NormalizeAddress 를 통과한 값이어야 한다.
//
// **빈 문자열 둘은 일치가 아니다.** 조회가 실패해 양쪽이 비었을 때 통과로
// 읽히면, 아무것도 확인하지 않고 확인했다고 믿게 된다.
func Match(derived, registered string) bool {
	return derived != "" && derived == registered
}

// MismatchError 는 "조회는 됐는데 두 주소가 다르다"를 다른 실패와 구분한다.
// 종료 코드가 달라야 스크립트가 "설정이 잘못됐다"와 "키가 틀렸다"를 나눠
// 다룰 수 있다.
type MismatchError struct {
	Derived    string
	Registered string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("키의 EOA(0x%s)가 계정의 등록 서명자(0x%s)와 다르다", e.Derived, e.Registered)
}

// NoSignerError 는 그 계정에 등록된 ECDSA 서명자가 아예 없다는 뜻이다.
// 불일치와 구분한다 — 계정 주소나 validator 주소가 틀렸을 때의 모습이기도
// 하고, 스마트계정이 아직 배포되지 않았을 때의 모습이기도 하다.
type NoSignerError struct{ Account string }

func (e *NoSignerError) Error() string {
	return "이 계정에 등록된 ECDSA 서명자가 없다 (0 주소) — 계정 주소나 validator 주소가 틀렸거나, 스마트계정이 아직 배포되지 않았다"
}

// Verifier 는 체인 조회 설정이다. 제로값이면 공개 노드와 기본 validator 를
// 쓴다.
type Verifier struct {
	// RPC 는 BSC JSON-RPC 엔드포인트다. 비면 DefaultRPC.
	RPC string
	// Validator 는 ECDSA_VALIDATOR 주소다. 비면 ECDSAValidator.
	Validator string
	// HTTP 는 테스트가 주입한다. nil 이면 rpcTimeout 짜리 기본 클라이언트.
	HTTP *http.Client
}

// RegisteredSigner 는 account 에 등록된 서명자 EOA 를 체인에서 읽는다.
// 등록된 것이 없으면 *NoSignerError 다.
func (v Verifier) RegisteredSigner(ctx context.Context, account string) (string, error) {
	data, err := CallData(account)
	if err != nil {
		return "", err
	}
	validator := v.Validator
	if validator == "" {
		validator = ECDSAValidator
	}
	if _, err := NormalizeAddress(validator); err != nil {
		return "", fmt.Errorf("validator 주소: %w", err)
	}
	rpc := v.RPC
	if rpc == "" {
		rpc = DefaultRPC
	}
	raw, err := v.ethCall(ctx, rpc, validator, data)
	if err != nil {
		return "", err
	}
	registered, zero, err := DecodeAddressResult(raw)
	if err != nil {
		return "", fmt.Errorf("ecdsaValidatorStorage 응답 해석: %w", err)
	}
	if zero {
		return "", &NoSignerError{Account: account}
	}
	return registered, nil
}

// Verify 는 derivedEOA 가 account 의 등록 서명자인지 본다.
//
// 일치하면 nil, 다르면 *MismatchError, 등록된 것이 없으면 *NoSignerError,
// 조회·파싱 실패면 그 밖의 에러다. **셋을 구분하는 것이 이 함수의 계약이다** —
// 호출자가 "키가 틀렸다"와 "조회가 안 됐다"를 같게 다루면, 네트워크가 잠깐
// 끊긴 것을 키 문제로 오진하고 멀쩡한 계정을 새로 만들게 된다.
func (v Verifier) Verify(ctx context.Context, account, derivedEOA string) error {
	derived, err := NormalizeAddress(derivedEOA)
	if err != nil {
		return fmt.Errorf("유도된 서명자 주소: %w", err)
	}
	registered, err := v.RegisteredSigner(ctx, account)
	if err != nil {
		return err
	}
	if !Match(derived, registered) {
		return &MismatchError{Derived: derived, Registered: registered}
	}
	return nil
}

// ethCall 은 JSON-RPC eth_call 한 번이다.
//
// HTTP 200 이어도 error 필드가 있으면 실패이고, 빈 result 도 실패다 — 빈
// 결과를 0 으로 읽으면 "서명자가 없다"와 "주소가 틀렸다"가 같은 값이 된다.
func (v Verifier) ethCall(ctx context.Context, rpc, to, data string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "eth_call",
		"params": []any{map[string]string{"to": to, "data": data}, "latest"},
	})
	if err != nil {
		return "", fmt.Errorf("RPC 요청 인코딩: %w", err)
	}
	client := v.HTTP
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
