package klines

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchParsesAndPaginates(t *testing.T) {
	// 첫 페이지는 limit 만큼 채워서 돌려주고, 두 번째는 짧게 줘서 종료시킨다.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Write([]byte(`[[60000,"1.0","2.0","0.5","1.5","10.0",119999,"15.0",7,"6.0","9.0","0"]]`))
			return
		}
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	old := BaseURL
	BaseURL = srv.URL
	defer func() { BaseURL = old }()

	got, err := Fetch(context.Background(), "BTCUSDT", "1m", 60000, 120000)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("봉 %d개, 기대 1개", len(got))
	}
	k := got[0]
	if k.OpenTime != 60000 || k.CloseTime != 119999 {
		t.Errorf("타임스탬프 오류: %+v", k)
	}
	if k.Open != 1.0 || k.High != 2.0 || k.Low != 0.5 || k.Close != 1.5 {
		t.Errorf("OHLC 오류: %+v", k)
	}
	if k.Volume != 10.0 || k.QuoteVolume != 15.0 || k.Trades != 7 || k.TakerBuyBase != 6.0 {
		t.Errorf("거래량 오류: %+v", k)
	}
}
