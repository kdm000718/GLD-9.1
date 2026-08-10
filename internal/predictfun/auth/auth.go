// Package auth 는 EOA 서명과 predict.fun JWT 발급을 담당한다.
//
// 개인키는 환경변수로만 들어오고 이 패키지 밖으로 나가지 않는다. String() 을
// 포함해 어떤 출력 경로에도 키가 실리지 않게 한다 — 로그 한 줄로 자금을 잃는다.
package auth

import (
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

type Signer struct {
	key  *ecdsa.PrivateKey
	addr common.Address
}

func NewSigner(hexKey string) (*Signer, error) {
	k := strings.TrimPrefix(strings.TrimSpace(hexKey), "0x")
	if len(k) != 64 {
		return nil, fmt.Errorf("개인키는 64자리 16진수여야 한다 (받은 길이 %d)", len(k))
	}
	key, err := crypto.HexToECDSA(k)
	if err != nil {
		// err 에 키가 실릴 수 있으므로 감싸지 않는다.
		return nil, fmt.Errorf("개인키를 파싱하지 못했다")
	}
	return &Signer{key: key, addr: crypto.PubkeyToAddress(key.PublicKey)}, nil
}

func (s *Signer) Address() common.Address { return s.addr }

// String 은 주소만 돌려준다. fmt 로 Signer 를 찍어도 키가 새지 않게 한다.
func (s *Signer) String() string { return "Signer(" + s.addr.Hex() + ")" }

// SignHash 는 32바이트 해시에 secp256k1 서명을 하고 65바이트를 돌려준다.
// go-ethereum 은 v 를 0/1 로 주므로 이더리움 관례인 27/28 로 올린다.
func (s *Signer) SignHash(h []byte) ([]byte, error) {
	if len(h) != 32 {
		return nil, fmt.Errorf("해시는 32바이트여야 한다 (받은 길이 %d)", len(h))
	}
	sig, err := crypto.Sign(h, s.key)
	if err != nil {
		return nil, fmt.Errorf("서명 실패: %w", err)
	}
	sig[64] += 27
	return sig, nil
}

// Authenticator 는 EOA 서명으로 predict.fun JWT 를 받아 캐시한다.
type Authenticator struct {
	Rest   *rest.Client
	Signer *Signer

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token 은 JWT 를 돌려준다. 만료 60초 전부터 갱신한다.
//
// 흐름: GET /v1/auth/message 로 서명할 메시지를 받고, EOA 로 서명해
// POST /v1/auth 에 보내면 Bearer 토큰이 온다. x-api-key 는 두 요청 모두에
// 계속 붙는다 — 키는 읽기·레이트리밋용이고 주문 권한과 무관하다.
//
// **실측(2026-08-09, testnet)**: `/v1/auth/message` 응답은
// `{"data":{"message":"…"},"success":true}` 형태이고, 서명 대상은 EIP-712
// 타입드데이터가 아니라 **평문 문자열**이다 (계획서 제목의 "EIP-712" 는 인증
// 방식 이름일 뿐, 실제 서명은 personal_sign 이다) — 그래서 아래 `accounts.TextHash`
// 를 쓰는 것이 맞다. 또한 이 메시지에는 `Timestamp: …` 가 들어 있어 호출마다
// 값이 달라진다. 그래서 메시지는 캐시하지 않고 갱신할 때마다 새로 받는다 —
// 캐시되는 것은 발급받은 JWT 뿐이다.
//
// **Task 10 실측(2026-08-09, testnet, cmd/probe -mode auth)**: 계획서와
// 이전 초안이 적었던 경로 `POST /v1/auth/jwt`는 존재하지 않는다(HTTP 404) —
// `/v1/auth/message`의 GET에 이끌려 유추한 이름이었다. `/docs`에 노출된 실제
// OpenAPI 스펙(PostAuthRequest 스키마)으로 확정한 진짜 경로와 필드는:
// 경로는 `POST /v1/auth`이고, 요청 본문의 지갑주소 필드명은 `"address"`가
// 아니라 `"signer"`다. 응답은 `{"data":{"token":"…"},"success":true}`
// 형태이고, `AuthTokenData` 스키마에 선언된 필드는 `token` 하나뿐이다
// (`jwt`/`accessToken`은 실물에 없었다 — 스펙에도, 실제 응답에도).
//
// **락을 두 네트워크 왕복이 끝날 때까지 쥐고 있는 것은 의도적이다 — 얕게 만들지
// 말 것.** 동시에 여러 고루틴이 Token() 을 부르면, 캐시 검사만 락으로 보호하고
// 왕복은 락 밖에서 하는 "얕은 락"은 호출자 수만큼 인증을 중복으로 내보낸다.
// 레이트리밋은 API 키 단위(240 req/min)이고 인증 요청도 같은 예산에서 나가므로,
// 중복 인증은 단순한 낭비가 아니라 주문을 넣을 예산을 깎아먹는다. 왕복 전체를
// 락 안에 두면 먼저 도착한 호출자가 갱신하는 동안 나머지는 대기했다가 갱신된
// 캐시를 그대로 받는다 — 이게 여기서 원하는 동작이다.
func (a *Authenticator) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Until(a.expires) > 60*time.Second {
		return a.token, nil
	}

	var msgResp map[string]any
	if err := a.Rest.Get(ctx, "/v1/auth/message",
		url.Values{"address": {a.Signer.Address().Hex()}}, &msgResp); err != nil {
		return "", fmt.Errorf("인증 메시지 요청 실패: %w", err)
	}
	// 봉투는 응답을 받은 직후 한 번만 벗긴다 — 아래의 모든 필드 접근은 평평한
	// 맵을 대상으로 한다. unwrapEnvelope 문서 참고.
	msgResp = unwrapEnvelope(msgResp)
	// 후보를 "message" 하나로 좁혔다 — AuthMessageData 스키마의 필수 필드는
	// message 뿐이고("nonce"라는 이름은 실물에 없었다), Task 10이 /docs의
	// OpenAPI 스펙으로 확정했다.
	msg, err := pickString(msgResp, "message")
	if err != nil {
		return "", fmt.Errorf("인증 메시지 응답에서 서명할 문자열을 못 찾았다 (키: %v)", keysOf(msgResp))
	}

	// personal_sign 관례: "Ethereum Signed Message:\n<len><msg>" 를 keccak 한다.
	sig, err := a.Signer.SignHash(accounts.TextHash([]byte(msg)))
	if err != nil {
		return "", err
	}

	var jwtResp map[string]any
	// 경로 /v1/auth, 필드명 signer — Task 10 실측(위 Token() 주석 참고).
	if err := a.Rest.Post(ctx, "/v1/auth", map[string]any{
		"signer":    a.Signer.Address().Hex(),
		"message":   msg,
		"signature": "0x" + hex.EncodeToString(sig),
	}, &jwtResp); err != nil {
		return "", fmt.Errorf("JWT 발급 실패: %w", err)
	}
	jwtResp = unwrapEnvelope(jwtResp)
	// 후보를 "token" 하나로 좁혔다 — AuthTokenData 스키마의 필수 필드는 token
	// 뿐이고, 실측 응답도 그것 하나만 왔다("jwt"/"accessToken"은 실물에 없었다).
	tok, err := pickString(jwtResp, "token")
	if err != nil {
		return "", fmt.Errorf("JWT 응답에서 토큰을 못 찾았다 (키: %v)", keysOf(jwtResp))
	}

	a.token = tok
	// **Task 10 실측(2026-08-09, 이어서)**: expiresIn 필드는 여전히 없다(스펙
	// AuthTokenData에도, 실제 응답에도) — 하지만 토큰 자체가 JWT이고, 페이로드에
	// 표준 클레임 exp(Unix초)가 들어 있다. 서명 검증 없이(검증은 서버 몫이고
	// 우리는 만료 시각만 읽는다) 그 값을 최우선으로 쓴다.
	//
	// **실측된 실제 수명: 86,400초 = 24시간**(exp − iat, 토큰 두 개로 재현
	// 확인). 이전에 하드코딩했던 10분 기본값은 안전판치고는 크게 짧은 쪽이었다
	// — 위험하진 않지만(짧게 잡을수록 만료된 토큰을 캐시하는 실패 모드에서
	// 멀어진다), 60초 전 갱신 로직 때문에 실제로는 24시간짜리 토큰을 10분마다
	// 불필요하게 재발급받고 있었을 것이다. 인증 요청도 240 req/min 예산을
	// 같이 쓰므로 그건 낭비다.
	//
	// exp 클레임을 못 읽으면(JWT가 아니거나 형식이 바뀌면) expiresIn 필드를
	// 다음으로 시도하고, 그마저 없으면 보수적 기본값 10분으로 떨어진다.
	if exp, ok := jwtExpiry(tok, time.Now()); ok {
		a.expires = exp
	} else {
		a.expires = time.Now().Add(10 * time.Minute)
		if secs, ok := jwtResp["expiresIn"].(float64); ok && secs > 0 {
			a.expires = time.Now().Add(time.Duration(secs) * time.Second)
		}
	}
	return tok, nil
}

// jwtExpiry는 토큰을 JWT로 보고 페이로드(가운데 세그먼트)의 exp 클레임(Unix초)
// 을 서명 검증 없이 디코딩한다. 서명 검증은 서버가 이미 했고, 우리는 그 결과로
// 받은 토큰이 자체 신고하는 만료 시각만 읽으면 된다 — HMAC/RSA 키가 없어도
// 되는 이유다.
//
// 세 세그먼트(header.payload.signature)가 아니거나, payload가 base64url로
// 안 풀리거나, JSON이 아니거나, exp가 없거나 0 이하이면 ok=false다 — 호출자는
// 이 경우 다른 소스(expiresIn 필드, 최종적으로 기본값)로 넘어간다.
//
// **상한 검사**: exp가 now로부터 maxTokenLifetime(7일)보다 멀면 거부한다.
// 이 값은 우리가 검증하지 않은 클레임이다 — 서버가 바뀌거나 응답이 뒤바뀌어
// 터무니없이 먼 만료(예: 10년)가 들어오면, 그걸 그대로 믿는 캐시는 실제로는
// 죽은 토큰을 영원히 재사용하고 모든 요청이 401로 떨어진다. 실측 수명이
// 24시간이므로 7일이면 서버가 수명을 늘려도 여유가 있으면서 그 실패 모드는
// 막는다. 거부하면 호출자가 보수적 기본값(10분)으로 떨어지므로, 틀렸을 때의
// 대가는 "조금 자주 재발급"뿐이다.
func jwtExpiry(token string, now time.Time) (time.Time, bool) {
	const maxTokenLifetime = 7 * 24 * time.Hour

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	exp := time.Unix(int64(claims.Exp), 0)
	if exp.After(now.Add(maxTokenLifetime)) {
		return time.Time{}, false
	}
	return exp, true
}

// ExpiresAt은 현재 캐시된 토큰의 만료 시각을 돌려준다. 토큰을 한 번도 발급
// 받지 않았으면 영시각(time.Time 제로값)이다. 진단·로깅 용도로 토큰 값 없이
// 만료만 보고 싶을 때 쓴다 — cmd/probe의 Step 3가 이걸로 만료를 찍는다.
func (a *Authenticator) ExpiresAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.expires
}

// unwrapEnvelope 는 predict.fun 의 `{"data": …, "success": true}` 봉투를 벗긴다.
// 실측(2026-08-09, testnet)으로 이 API 는 모든 REST 응답을 이 형태로 감싼다.
//
// **응답을 받은 직후 한 번만 부르고, 그 뒤의 모든 필드 접근(pickString,
// expiresIn 등)은 평평한 맵에서 한다.** 처음에는 pickString 안에서 후보 키가
// "data" 를 포함할 때만 봉투를 벗겼는데, 그 후보 목록에 없는 호출부(JWT 응답의
// token/jwt/accessToken)에서는 봉투가 전혀 안 벗겨져 조용히 실패했다. 그래서
// 후보 키 목록과 분리해 pickString 밖으로 뺐는데도, 바로 옆의 `expiresIn` 읽기는
// pickString 을 안 거쳐서 여전히 원본(봉투 안) 맵을 읽는 채로 남아 있었다 —
// 같은 실수가 접근자 단위로 반복된 것이다. 그래서 접근자마다 봉투를 알게 하지
// 않고, 응답을 받은 자리에서 한 번만 벗겨 그 뒤로는 전부 평평한 맵을 쓴다.
//
// "data" 가 맵인 동안 반복해서 벗긴다(이중 봉투 대비) — 무한 루프 방지로 상한을
// 둔다. "data" 가 배열이거나 없으면 원본을 그대로 돌려준다. 근거 없는 봉투
// 이름을 늘리지 않는다 — 실측된 것은 "data" 뿐이다.
func unwrapEnvelope(m map[string]any) map[string]any {
	const maxDepth = 4
	for i := 0; i < maxDepth; i++ {
		inner, ok := m["data"].(map[string]any)
		if !ok {
			return m
		}
		m = inner
	}
	return m
}

// pickString 은 이미 봉투가 벗겨진 평평한 맵에서 후보 키를 순서대로 문자열로
// 찾는다. 봉투 처리는 하지 않는다 — 호출자가 unwrapEnvelope 를 먼저 거쳐야
// 한다.
//
// **필드명은 Task 10(2026-08-09, testnet)에서 확정됐다** — message/token
// 각각 정확히 하나뿐이고, Token()의 두 호출부는 이제 그 한 후보만 넘긴다.
// 함수 자체는 여러 후보를 받을 수 있게 그대로 둔다(불필요한 축소가 아니라,
// map[string]any 로 관대하게 받는 이 계층 전체의 설계와 일관되게 두는 것).
func pickString(m map[string]any, keys ...string) (string, error) {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("없음")
}

// keysOf 는 에러 메시지용이다. 값은 찍지 않는다 — 토큰이 로그에 남으면 안 된다.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
