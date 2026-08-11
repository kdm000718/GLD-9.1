package rest

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 이 파일은 거래소가 응답 헤더로 알려주는 **남은 요청 예산**을 붙잡아 둔다.
//
// # 왜 필요한가 — 감시 장치가 이 값 없이는 거짓말을 한다
//
// `internal/beat/rule` 은 `Loop.RateLimitRemaining <= 0` 이면 Crit 을 낸다.
// 봇이 이 값을 채우지 못하면 스냅샷에는 0 이 실리고, 규칙은 **매 하트비트마다**
// "요청 예산이 0 이다" 를 외친다. 늘 울리는 경보는 아무도 보지 않는 경보이고,
// 그러면 그 옆에 있는 진짜 Crit(노출 불변식 위반·취소 배치 초과)도 같이
// 묻힌다. `cmd/gld91-monitor/settle.go` 는 한 발 더 나가서 이 값이 바닥이면
// 정산 조회를 **건너뛴다** — 0 이 고정이면 정산을 영원히 조회하지 않는다.
//
// 즉 이 값이 비면 감시 장치가 조용히 고장 난다. 그 고장은 봇이 멀쩡할 때는
// 드러나지 않고, 봇이 실제로 아플 때 "경보가 원래 저래" 로 넘어가는 형태로만
// 드러난다.
//
// # 비2xx 에서도 기록한다
//
// 기록 시점은 상태 코드 검사 **앞**이다. 429 야말로 남은 예산을 가장 알고
// 싶은 순간인데, 2xx 에서만 읽으면 정확히 그때 값이 갱신되지 않고 마지막
// 성공 응답의 낙관적인 숫자가 남는다.
//
// # 모르는 것과 가득 찬 것을 구별하지 않는다
//
// 첫 요청 전에는 헤더를 본 적이 없다. 그때 [Client.RateLimitRemaining] 은
// [RateLimitPerMin] 을 돌려준다 — 추측이 아니라 사실이다. 아직 아무 요청도
// 보내지 않았으면 예산은 실제로 가득 차 있다.

// RateLimitPerMin 은 API 키당 분당 요청 한도다. 거래소가 `ratelimit-limit`
// 헤더로 같은 값을 준다(2026-08-11 확인: `ratelimit-policy: 240;w=60`).
//
// **[minInterval] 과 독립이다.** 그쪽은 우리가 스스로 거는 간격(3 req/s)이고
// 이쪽은 거래소가 정한 한도다. 둘을 한 상수로 묶으면 우리 간격을 조정할 때
// 거래소 한도까지 따라 움직인다.
const RateLimitPerMin = 240

// rateLimitHeader 는 남은 예산이 실리는 응답 헤더다. 소문자로 오지만
// http.Header.Get 은 대소문자를 맞춰 주므로 그대로 쓴다.
const rateLimitHeader = "ratelimit-remaining"

// noteRateLimit 은 응답 헤더의 남은 예산을 기록한다.
//
// 헤더가 없거나 숫자가 아니면 **직전 값을 유지한다.** 0 으로 떨어뜨리면
// 헤더를 주지 않는 프록시 하나가 감시 규칙에 Crit 을 만들어낸다.
func (c *Client) noteRateLimit(h http.Header) {
	v := strings.TrimSpace(h.Get(rateLimitHeader))
	if v == "" {
		return
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return
	}
	c.rlMu.Lock()
	defer c.rlMu.Unlock()
	c.rlRemaining = n
	c.rlAt = time.Now()
}

// RateLimitRemaining 은 마지막 응답이 알려준 남은 요청 예산이다. 아직 어떤
// 응답도 헤더를 주지 않았으면 [RateLimitPerMin] 이다.
func (c *Client) RateLimitRemaining() int {
	c.rlMu.Lock()
	defer c.rlMu.Unlock()
	return c.rlRemaining
}

// RateLimitAt 은 [Client.RateLimitRemaining] 이 마지막으로 갱신된 시각이다.
// 제로값이면 헤더를 한 번도 보지 못했다는 뜻이다 — 그 구별이 필요한 호출자만
// 쓴다(예: "예산이 가득한 것"과 "요청을 한 번도 못 보낸 것"을 가르는 자가
// 점검).
func (c *Client) RateLimitAt() time.Time {
	c.rlMu.Lock()
	defer c.rlMu.Unlock()
	return c.rlAt
}
