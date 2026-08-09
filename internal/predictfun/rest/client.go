// Package rest 는 predict.fun REST API 의 읽기 전용 클라이언트다.
package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// UserAgent 는 명시해야 한다. 기본 Go User-Agent 는 WAF 가 403 으로 막는다.
const UserAgent = "gld91/0.1 (+https://github.com/kdm000718/GLD-9.1)"

// 레이트리밋은 키당 240 req/min = 4 req/s. 여유를 두고 3 req/s 로 간다.
const minInterval = 333 * time.Millisecond

type Client struct {
	BaseURL string
	apiKey  string
	http    *http.Client

	// mu 는 last 를 보호한다. 마켓메이킹 루프는 이 클라이언트를 여러 고루틴에서
	// 동시에 부르므로 뮤텍스가 없으면 데이터 레이스이면서 동시에 레이트리밋이
	// 무력화된다 — 동시 호출자가 모두 같은 last 를 읽고 한 창에 다 나간다.
	mu   sync.Mutex
	last time.Time
}

func New(apiKey string) *Client {
	return &Client{
		BaseURL: "https://api.predict.fun",
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Get 은 path 로 GET 하고 응답 JSON 을 out 에 디코딩한다.
// 에러 메시지에 API 키를 절대 넣지 않는다.
func (c *Client) Get(ctx context.Context, path string, q url.Values, out any) error {
	if err := c.throttle(ctx); err != nil {
		return err
	}

	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("요청 생성 실패 %s: %w", path, err)
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("User-Agent", UserAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("요청 실패 %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("응답 디코딩 실패 %s: %w", path, err)
	}
	return nil
}

// throttle 은 직전 요청이 나간 시각으로부터 minInterval 이 지날 때까지 기다린다.
//
// 대기 자체를 락 안에서 하는 것이 핵심이다. 락을 놓고 기다리면 동시 호출자가 전부
// 같은 last 를 읽은 뒤 같은 순간에 깨어나므로 직렬화가 되지 않는다. 락 안에서
// 기다리면 호출 N개가 반드시 (N−1)×minInterval 이상 걸린다.
//
// ctx 취소로 빠져나갈 때는 last 를 갱신하지 않는다 — 요청을 보내지 않았으므로
// 다음 호출자가 그만큼 덜 기다려도 실제 발신 간격은 유지된다.
func (c *Client) throttle(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := minInterval - time.Since(c.last); wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.last = time.Now()
	return nil
}
