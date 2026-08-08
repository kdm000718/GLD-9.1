// Package rest 는 predict.fun REST API 의 읽기 전용 클라이언트다.
package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	last    time.Time
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
	if wait := minInterval - time.Since(c.last); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.last = time.Now()

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
