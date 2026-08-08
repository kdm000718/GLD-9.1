// Package klines 는 Binance 현물 kline 을 REST 로 가져온다.
// 전체 이력은 internal/vision 을 쓴다 — 여기는 짧은 구간용이다.
package klines

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var BaseURL = "https://api.binance.com/api/v3/klines"

const maxLimit = 1000

var intervalMS = map[string]int64{
	"1m": 60_000, "3m": 180_000, "5m": 300_000, "15m": 900_000, "1h": 3_600_000,
}

type Kline struct {
	OpenTime      int64
	Open          float64
	High          float64
	Low           float64
	Close         float64
	Volume        float64
	CloseTime     int64
	QuoteVolume   float64
	Trades        int64
	TakerBuyBase  float64
	TakerBuyQuote float64
}

// Fetch 는 [startMS, endMS] 구간의 봉을 페이지네이션으로 모두 가져온다.
func Fetch(ctx context.Context, symbol, interval string, startMS, endMS int64) ([]Kline, error) {
	step, ok := intervalMS[interval]
	if !ok {
		return nil, fmt.Errorf("모르는 인터벌: %s", interval)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var out []Kline
	for cursor := startMS; cursor <= endMS; {
		q := url.Values{}
		q.Set("symbol", symbol)
		q.Set("interval", interval)
		q.Set("startTime", strconv.FormatInt(cursor, 10))
		q.Set("endTime", strconv.FormatInt(endMS, 10))
		q.Set("limit", strconv.Itoa(maxLimit))

		rows, err := getRows(ctx, client, BaseURL+"?"+q.Encode())
		if err != nil {
			return out, err
		}
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			k, err := parseRow(r)
			if err != nil {
				return out, err
			}
			out = append(out, k)
		}
		if len(rows) < maxLimit {
			break
		}
		cursor = out[len(out)-1].OpenTime + step
	}
	return out, nil
}

// getRows 는 재시도 루프만 담당하고 한 번의 요청은 getRowsOnce 에 맡긴다.
// 한 함수에 defer 와 재시도를 섞으면 응답 본문을 언제 닫는지가 흐려진다.
func getRows(ctx context.Context, c *http.Client, u string) ([]json.RawMessage, error) {
	var last error
	for attempt := 0; attempt < 5; attempt++ {
		rows, err := getRowsOnce(ctx, c, u)
		if err == nil {
			return rows, nil
		}
		last = err
		select {
		case <-time.After(time.Duration(attempt+1) * 1500 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("Binance 요청 실패: %w", last)
}

func getRowsOnce(ctx context.Context, c *http.Client, u string) ([]json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "gld91/0.1")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Binance HTTP %d", resp.StatusCode)
	}
	var rows []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// parseRow 는 Binance kline 배열 한 줄을 파싱한다.
// 형식: [openTime, open, high, low, close, volume, closeTime,
//
//	quoteVolume, trades, takerBuyBase, takerBuyQuote, ignore]
func parseRow(raw json.RawMessage) (Kline, error) {
	var a []json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return Kline{}, err
	}
	if len(a) < 11 {
		return Kline{}, fmt.Errorf("kline 필드가 %d개뿐이다", len(a))
	}
	num := func(i int) (float64, error) {
		var s string
		if err := json.Unmarshal(a[i], &s); err == nil {
			return strconv.ParseFloat(s, 64)
		}
		var f float64
		err := json.Unmarshal(a[i], &f)
		return f, err
	}
	ival := func(i int) (int64, error) {
		var v int64
		err := json.Unmarshal(a[i], &v)
		return v, err
	}
	var k Kline
	var err error
	if k.OpenTime, err = ival(0); err != nil {
		return k, err
	}
	if k.Open, err = num(1); err != nil {
		return k, err
	}
	if k.High, err = num(2); err != nil {
		return k, err
	}
	if k.Low, err = num(3); err != nil {
		return k, err
	}
	if k.Close, err = num(4); err != nil {
		return k, err
	}
	if k.Volume, err = num(5); err != nil {
		return k, err
	}
	if k.CloseTime, err = ival(6); err != nil {
		return k, err
	}
	if k.QuoteVolume, err = num(7); err != nil {
		return k, err
	}
	if k.Trades, err = ival(8); err != nil {
		return k, err
	}
	if k.TakerBuyBase, err = num(9); err != nil {
		return k, err
	}
	if k.TakerBuyQuote, err = num(10); err != nil {
		return k, err
	}
	return k, nil
}
