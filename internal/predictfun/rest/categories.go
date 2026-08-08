package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Round 는 정산이 끝난 한 회차다.
type Round struct {
	Slug       string
	StartUnix  int64   // 회차 시작 유닉스초 (5분 경계)
	StartPrice float64 // Chainlink 기준 시작가
	EndPrice   float64 // Chainlink 기준 종료가
	Symbol     string  // 예: BTCUSDT
}

// ParseSlugStart 는 "btc-updown-5m-1786190100" 에서 시작 유닉스초를 뽑는다.
// 5분 경계가 아니면 회차 슬러그로 보지 않는다.
func ParseSlugStart(slug string) (int64, bool) {
	i := strings.LastIndex(slug, "-")
	if i < 0 || i == len(slug)-1 {
		return 0, false
	}
	v, err := strconv.ParseInt(slug[i+1:], 10, 64)
	if err != nil || v <= 0 || v%300 != 0 {
		return 0, false
	}
	return v, true
}

// categoryPage 는 /v1/categories 응답이다. 모르는 필드는 무시한다 — API 는 베타다.
type categoryPage struct {
	Success bool    `json:"success"`
	Cursor  *string `json:"cursor"`
	Data    []struct {
		Slug        string `json:"slug"`
		Status      string `json:"status"`
		VariantData struct {
			// 가격은 문자열로 오기도 하고 숫자로 오기도 한다.
			StartPrice json.Number `json:"startPrice"`
			EndPrice   json.Number `json:"endPrice"`
			Symbol     string      `json:"priceFeedSymbol"`
		} `json:"variantData"`
	} `json:"data"`
}

// FetchResolvedRounds 는 정산된 CRYPTO_UP_DOWN 회차를 최신순으로 모은다.
// symbolPrefix 는 슬러그 앞부분("btc" 등)으로 거른다.
// sinceUnix 보다 오래된 회차를 만나면 멈춘다 — 없으면 무한히 페이지를 넘긴다.
func FetchResolvedRounds(ctx context.Context, c *Client, symbolPrefix string, sinceUnix int64) ([]Round, error) {
	var out []Round
	cursor := ""
	for page := 0; ; page++ {
		q := url.Values{}
		q.Set("marketVariant", "CRYPTO_UP_DOWN")
		q.Set("status", "RESOLVED")
		q.Set("sort", "PUBLISHED_AT_DESC")
		q.Set("first", "50")
		if cursor != "" {
			q.Set("after", cursor)
		}
		var pg categoryPage
		if err := c.Get(ctx, "/v1/categories", q, &pg); err != nil {
			return out, fmt.Errorf("%d번째 페이지: %w", page, err)
		}
		if len(pg.Data) == 0 {
			return out, nil
		}
		for _, row := range pg.Data {
			start, ok := ParseSlugStart(row.Slug)
			if !ok {
				continue
			}
			if start < sinceUnix {
				return out, nil // 최신순이므로 여기서 끝
			}
			if !strings.HasPrefix(row.Slug, symbolPrefix+"-") {
				continue
			}
			sp, err1 := row.VariantData.StartPrice.Float64()
			ep, err2 := row.VariantData.EndPrice.Float64()
			if err1 != nil || err2 != nil || sp <= 0 || ep <= 0 {
				continue // 기준가가 없는 회차는 비교 불가
			}
			out = append(out, Round{
				Slug:       row.Slug,
				StartUnix:  start,
				StartPrice: sp,
				EndPrice:   ep,
				Symbol:     row.VariantData.Symbol,
			})
		}
		if pg.Cursor == nil || *pg.Cursor == "" {
			return out, nil
		}
		cursor = *pg.Cursor
	}
}
