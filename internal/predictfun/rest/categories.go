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

// Timeframe 은 이 수집기가 다루는 유일한 상품이다. predict.fun 은 같은
// CRYPTO_UP_DOWN 변종 아래에 15m 등 다른 타임프레임도 함께 내려준다.
const Timeframe = "5m"

// ParseSlugStart 는 "btc-updown-5m-1786190100" 에서 시작 유닉스초를 뽑는다.
// 5분 상품이 아니면 거부한다.
//
// 타임프레임은 슬러그의 세그먼트로 직접 확인해야 한다. 나눗셈으로 추론하면
// 안 된다 — 15분 경계는 전부 5분 경계이기도 하므로 v%300==0 검사만으로는
// "btc-updown-15m-..." 이 그대로 통과한다. 그렇게 섞여 들어온 15분 회차는
//
//	(1) StartPrice~EndPrice 가 15분을 걸치므로 5분봉과 방향을 비교하는 것
//	    자체가 무의미하고,
//	(2) 5분 회차와 StartUnix 가 같아 슬롯이 충돌한다.
//
// G2 로 발표된 앞의 두 숫자가 이것 때문에 오염됐다. 7일 구간에서 5분 슬롯
// 2,016개에 회차 2,686개가 잡힌 것은 15분 상품 672개가 섞였기 때문이다.
// v%300 검사는 이제 타임프레임 판별이 아니라 정합성 확인으로 남긴다.
func ParseSlugStart(slug string) (int64, bool) {
	i := strings.LastIndex(slug, "-")
	if i < 0 || i == len(slug)-1 {
		return 0, false
	}
	if !strings.HasSuffix(slug[:i], "-"+Timeframe) {
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

// maxPages 는 커서 페이지네이션의 상한이다. 50건/페이지 × 2,000 = 10만 회차로
// 실제 조회 구간(30일 ≈ 8,600 회차/심볼)을 크게 넘는다. 정상 경로에서는 절대
// 닿지 않고, 베타 API 가 종료 조건 네 개를 전부 어겼을 때만 여기서 멈춘다.
//
// const 가 아니라 var 인 이유는 하나뿐이다 — 상한 자체를 시험하려면 2,000 페이지를
// 3 req/s 로 받아야 해서(11분) 테스트가 값을 낮춰야 한다. 프로덕션 코드는 쓰지 않는다.
var maxPages = 2000

// FetchResolvedRounds 는 정산된 CRYPTO_UP_DOWN 회차를 최신순으로 모은다.
// symbolPrefix 는 슬러그 앞부분("btc" 등)으로, ParseSlugStart 는 타임프레임으로
// 거른다 — 5분 상품만 남는다. sinceUnix 보다 오래된 회차를 만나면 멈춘다.
//
// 같은 슬러그는 한 번만 담는다. 커서 페이지네이션이 항목을 겹쳐서 돌려줄 수
// 있고, 그것을 독립 표본으로 세면 표본 수가 부풀려져 표준오차가 과소평가된다.
//
// 커서가 전진하지 않거나 maxPages 를 넘으면 에러다 — 3 req/s 무한 루프를 도는
// 것보다 낫다.
func FetchResolvedRounds(ctx context.Context, c *Client, symbolPrefix string, sinceUnix int64) ([]Round, error) {
	var out []Round
	seen := make(map[string]bool)
	cursor := ""
	for page := 0; page < maxPages; page++ {
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
			if seen[row.Slug] {
				continue // 페이지가 겹쳐서 돌려준 같은 회차
			}
			seen[row.Slug] = true
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
		if *pg.Cursor == cursor {
			// 커서가 제자리다. 종료 조건 네 개(빈 데이터 / sinceUnix 미만 /
			// 빈 커서 / 에러) 중 어느 것도 이 경우를 잡지 못하므로 여기서 끊는다.
			return out, fmt.Errorf("%d번째 페이지: 커서가 전진하지 않는다", page)
		}
		cursor = *pg.Cursor
	}
	return out, fmt.Errorf("페이지 상한 %d 를 넘었다 — 커서가 순환하는 것으로 보인다", maxPages)
}
