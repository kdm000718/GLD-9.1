package rest

import (
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
