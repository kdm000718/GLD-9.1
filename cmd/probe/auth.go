package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/accounts"

	predictauth "github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// runAuth는 Step 3(인증 왕복)을 수행한다.
//
// 개인키는 WALLET_PRIVATE_KEY 환경변수로만 받는다 — 소스에 박지 않는다.
// **실키가 아직 ~/.config/predictfun/env 에 없는 상태에서 이 스텝을 도는
// 방법**: go-ethereum 문서에 실린 공개 테스트 키(주소
// 0x27000F84214f79B0600aa86841958b13ac98242a, cmd/g3check가 같은 키를 씀)를
// 실행 시점에만 환경변수로 넘긴다. 그 키는 잔고가 없고 자금을 보내면 안 된다.
// 로그인만 하는 것이므로 잔고 없이도 /v1/auth/jwt 응답 형태를 확정할 수 있다.
//
// 토큰 값 자체는 어떤 경로로도 출력하지 않는다 — 길이와 만료만 찍는다.
func runAuth(ctx context.Context, rc *rest.Client, log *slog.Logger) error {
	hexKey := os.Getenv("WALLET_PRIVATE_KEY")
	if hexKey == "" {
		return fmt.Errorf("WALLET_PRIVATE_KEY가 비어 있다 — 실키가 없으면 go-ethereum 문서의 공개 테스트 키를 임시로 넘겨라 (자금 없음, 소스에 박지 말 것)")
	}
	signer, err := predictauth.NewSigner(hexKey)
	if err != nil {
		return fmt.Errorf("서명자 생성 실패: %w", err)
	}
	fmt.Println("[Step 3] 서명자:", signer) // Signer.String()은 주소만 찍는다.

	// --- 1) 원문 응답 구조를 직접 뜯어 필드명을 확정한다 ---
	//
	// auth.Authenticator는 이미 map[string]any + 후보 키 목록으로 왕복에
	// 성공하지만(Task 7), 실제 필드명이 무엇인지는 이 진단에서 원문을 봐야
	// 안다. describeJSON은 문자열 값의 "내용"은 절대 찍지 않고 길이만
	// 찍는다 — message/signature/token이 전부 문자열이므로 이 규칙 하나로
	// 시크릿이 새는 경로를 원천 차단한다. 숫자·불린(예: expiresIn, success)은
	// 비밀이 아니므로 값 그대로 찍는다.
	addr := signer.Address().Hex()
	var msgResp map[string]any
	if err := rc.Get(ctx, "/v1/auth/message", url.Values{"address": {addr}}, &msgResp); err != nil {
		return fmt.Errorf("인증 메시지 요청 실패: %w", err)
	}
	fmt.Println("[진단] GET /v1/auth/message 응답 구조:")
	for _, line := range describeJSON("resp", msgResp) {
		fmt.Println("   ", line)
	}

	msg, ok := extractString(msgResp, "data", "message")
	if !ok {
		msg, ok = extractString(msgResp, "message")
	}
	if !ok {
		return fmt.Errorf("응답에서 서명할 메시지를 찾지 못했다 (키: %v)", topKeys(msgResp))
	}

	sig, err := signer.SignHash(accounts.TextHash([]byte(msg)))
	if err != nil {
		return fmt.Errorf("서명 실패: %w", err)
	}

	var jwtResp map[string]any
	t0 := time.Now()
	// 실측(2026-08-09): 경로는 /v1/auth/jwt가 아니라 /v1/auth이고, 요청 본문의
	// 주소 필드명은 "address"가 아니라 "signer"다 — /docs에 노출된 OpenAPI
	// 스펙(PostAuthRequest)으로 확정했다. 계획서·Task 7 초안의 "/v1/auth/jwt"는
	// 추측이었다(HTTP 404로 드러남).
	if err := rc.Post(ctx, "/v1/auth", map[string]any{
		"signer":    addr,
		"message":   msg,
		"signature": "0x" + hex.EncodeToString(sig),
	}, &jwtResp); err != nil {
		return fmt.Errorf("JWT 발급 실패: %w", err)
	}
	elapsed := time.Since(t0)
	fmt.Println("[진단] POST /v1/auth 응답 구조:")
	for _, line := range describeJSON("resp", jwtResp) {
		fmt.Println("   ", line)
	}

	// --- 2) 프로덕션 경로(auth.Authenticator)도 같은 키로 실제 동작하는지 확인 ---
	//
	// Task 7이 남긴 후보 키 목록(token/jwt/accessToken)이 실제 응답과 맞는지는
	// 이 왕복이 성공하는지로 증명된다. 토큰 값 자체는 변수에만 남고 출력하지
	// 않는다.
	a := &predictauth.Authenticator{Rest: rc, Signer: signer}
	t1 := time.Now()
	tok, err := a.Token(ctx)
	if err != nil {
		return fmt.Errorf("Authenticator.Token 왕복 실패 — pickString 후보 키가 실제 필드명과 안 맞을 수 있다: %w", err)
	}
	fmt.Printf("[Step 3] Authenticator.Token 왕복 성공 (message 요청 %s, jwt 요청 %s)\n", elapsed.Round(time.Millisecond), time.Since(t1).Round(time.Millisecond))
	fmt.Printf("[Step 3] 토큰 길이: %d자 (값은 출력하지 않는다)\n", len(tok))
	fmt.Printf("[Step 3] 만료 시각: %s (지금부터 %s)\n", a.ExpiresAt().Format(time.RFC3339), time.Until(a.ExpiresAt()).Round(time.Second))

	log.Info("Step 3 완료")
	return nil
}

// describeJSON은 임의로 중첩된 JSON 값을 "경로: 타입(부가정보)" 줄들로
// 펼친다. 문자열 리프는 절대 값을 찍지 않고 길이만 찍는다 — 이 응답에
// 토큰·서명·메시지가 전부 문자열로 온다는 사실 하나로 시크릿 비노출을
// 보장하려는 설계다. 숫자·불린은 비밀이 아니므로 값을 그대로 찍는다.
func describeJSON(prefix string, v any) []string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out []string
		for _, k := range keys {
			out = append(out, describeJSON(prefix+"."+k, t[k])...)
		}
		return out
	case []any:
		out := []string{fmt.Sprintf("%s: array(len=%d)", prefix, len(t))}
		if len(t) > 0 {
			out = append(out, describeJSON(prefix+"[0]", t[0])...)
		}
		return out
	case string:
		return []string{fmt.Sprintf("%s: string(len=%d)", prefix, len(t))}
	case float64:
		return []string{fmt.Sprintf("%s: number(%v)", prefix, t)}
	case bool:
		return []string{fmt.Sprintf("%s: bool(%v)", prefix, t)}
	case nil:
		return []string{fmt.Sprintf("%s: null", prefix)}
	default:
		return []string{fmt.Sprintf("%s: %T", prefix, t)}
	}
}

// extractString은 path를 따라 중첩 맵을 내려가 마지막 키의 문자열 값을 찾는다.
func extractString(m map[string]any, path ...string) (string, bool) {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = mm[p]
		if !ok {
			return "", false
		}
	}
	s, ok := cur.(string)
	return s, ok
}

func topKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
