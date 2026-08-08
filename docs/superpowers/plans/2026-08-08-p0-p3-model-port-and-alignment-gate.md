# GLD-9.1 P0~P3 — 모델 포팅과 정합 게이트 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** predict.fun 정산 기준이 바이낸스 5분봉과 얼마나 어긋나는지 먼저 판정하고, 통과하면 검증된 Python +0분 예측 모델을 Go로 전체 포팅해 9년치 결과를 재현한다.

**Architecture:** `internal/` 아래 순수 계산 패키지들이 Python 패키지와 1:1로 대응한다. 각 패키지는 아래쪽 패키지만 의존하고 위를 모른다: `bars` → `indicators`/`divergence` → `clock` → `features` → `model` → `walkforward`. `cmd/` 아래 실행 바이너리가 이들을 조립한다. 미래참조 차단은 `clock.MarketView`가 데이터를 물리적으로 잘라내는 것으로 구조가 보장하며, 피처 코드는 잘린 데이터만 볼 수 있다.

**Tech Stack:** Go 1.22.2 (`GOTOOLCHAIN=local`), `gonum.org/v1/gonum/optimize` (LBFGS), 표준 라이브러리만 그 외. 외부 데이터는 Binance Vision(ZIP 덤프)과 predict.fun REST.

## Global Constraints

- Go 모듈 경로는 `github.com/kdm000718/GLD-9.1`. Go 1.22.2 로컬 툴체인으로 빌드되어야 하며 `GOTOOLCHAIN=local`을 쓴다. go 1.23+ 를 요구하는 의존성은 넣지 않는다.
- 주석·로그·에러 메시지는 한국어로 쓴다. 기존 저장소들과 같은 관례다.
- 시크릿은 환경변수로만 읽는다: `PREDICT_API_KEY`. 소스·로그·에러 메시지 어디에도 값을 남기지 않는다.
- predict.fun REST 는 `x-api-key` 헤더와 명시적 `User-Agent`가 둘 다 필요하다. 기본 Go User-Agent 는 WAF 가 403 으로 막는다.
- predict.fun REST 레이트리밋은 API 키당 240 req/min 이다. 클라이언트는 이 한도 아래로 스스로 조절한다.
- 가격 비교에 float 를 쓰지 않는다. 가격은 마켓의 `decimalPrecision`(BTC 5분 마켓 실측값 2)으로 정규화한 정수 틱으로 다룬다.
- 피처 순서는 `FeatureNames` 슬라이스가 유일한 근거다. Python `btc5m/features.py:FEATURE_NAMES` 와 원소·순서가 완전히 같아야 한다.
- 부동소수점 비교 허용오차: 피처 `1e-9`, 정확도 `±0.01%p`, AUC `±0.0005`. 표본 수는 허용오차 없이 완전 일치.
- 참조 구현은 `/home/kdm00/kdm/btc5m_prediction_agent` 이다. 동작이 애매하면 추측하지 말고 그 코드를 읽는다.

---

## 파일 구조

| 파일 | 책임 |
|---|---|
| `go.mod` | 모듈 정의, gonum 의존성 |
| `internal/klines/client.go` | Binance REST kline 조회(페이지네이션) |
| `internal/predictfun/rest/client.go` | predict.fun REST — 레이트리밋·헤더·커서 페이지네이션 |
| `internal/predictfun/rest/categories.go` | RESOLVED 카테고리 조회, `variantData` 파싱 |
| `cmd/align/main.go` | **G2** — Chainlink 정산 방향 vs 바이낸스 봉 방향 불일치율 |
| `internal/bars/bars.go` | OHLCV 값 타입, 슬라이스·검색 |
| `internal/vision/vision.go` | Binance Vision 전체 이력 로더(ZIP·SHA256·µs 정규화·캐시) |
| `internal/indicators/indicators.go` | 인과적 지표 — EMA·RSI·ATR·수익률·변동성·z점수 |
| `internal/divergence/divergence.go` | 확정 피벗 RSI 다이버전스 |
| `internal/clock/clock.go` | `MarketView` — 미래참조 물리 차단 |
| `internal/features/features.go` | 60 피처 생성 |
| `internal/features/names.go` | `FeatureNames` 순서 정의 |
| `internal/model/logreg.go` | L2 로지스틱 회귀 — 추론·학습·직렬화 |
| `internal/model/matrix.go` | float32 평면 행렬 (100만 행 규모 메모리 대응) |
| `internal/metrics/metrics.go` | 정확도·AUC·ECE·교정표·이항검정 |
| `internal/walkforward/walkforward.go` | 블록 생성, 학습 가능 표본 선별 |
| `cmd/goldencheck/main.go` | **G1** — Python 골든 벡터 대조 |
| `cmd/backtest/main.go` | **G1'** — 전 구간 워크포워드 재현 |
| `tools/export_golden.py` | Python 쪽에서 골든 벡터 내보내기 |

## 스펙 대비 범위 재조정 두 가지

**G2 를 맨 앞으로 옮겼다.** 스펙은 정합 측정을 P3 에 뒀지만, 이 측정은 모델과 아무
의존관계가 없는 순수 데이터 비교다. 판정 게이트를 앞에 두면 실패했을 때 포팅을 한 줄도
쓰지 않고 멈출 수 있다. Task 1 이 그것이다.

**P2(predict.fun WS 오더북 이식)는 이 계획에서 뺐다.** 스펙에서 P2 를 둔 이유는 G2 에
오더북 이력이 필요할 것으로 봤기 때문인데, REST 가 `variantData.startPrice`/`endPrice`
를 직접 주므로 오더북 없이 정합을 잴 수 있다는 것이 확인됐다. WS 스트림은 이제 실행
절반(마켓메이킹 루프)에만 필요하므로 P4~P6 계획으로 옮긴다. 이 계획은 REST 읽기 경로만
만든다.

---

### Task 1: 저장소 부트스트랩 + G2 정합 게이트

**이 태스크가 판정 게이트다.** 결과가 나쁘면 Task 2 이후를 착수하지 않는다.

**Files:**
- Create: `go.mod`, `Makefile`
- Create: `internal/klines/client.go`, `internal/klines/client_test.go`
- Create: `internal/predictfun/rest/client.go`, `internal/predictfun/rest/client_test.go`
- Create: `internal/predictfun/rest/categories.go`, `internal/predictfun/rest/categories_test.go`
- Create: `cmd/align/main.go`

**Interfaces:**
- Produces: `klines.Kline{OpenTime, Open, High, Low, Close, Volume, CloseTime, QuoteVolume, Trades, TakerBuyBase, TakerBuyQuote}`, `klines.Fetch(ctx, symbol, interval string, startMS, endMS int64) ([]Kline, error)`
- Produces: `rest.Client` with `New(apiKey string) *Client`, `(*Client).Get(ctx context.Context, path string, q url.Values, out any) error`
- Produces: `rest.Round{Slug string, StartUnix int64, StartPrice, EndPrice float64, Symbol string}`, `rest.FetchResolvedRounds(ctx context.Context, c *Client, symbolPrefix string, sinceUnix int64) ([]Round, error)`

- [ ] **Step 1: 모듈과 Makefile 생성**

`go.mod`:

```
module github.com/kdm000718/GLD-9.1

go 1.22

require gonum.org/v1/gonum v0.15.1
```

`Makefile`:

```makefile
export GOTOOLCHAIN := local

build:
	go build ./...

test:
	go test -race ./...

vet:
	go vet ./...

align:
	go run ./cmd/align -days 30
```

- [ ] **Step 2: 슬러그 파서 실패 테스트를 쓴다**

`internal/predictfun/rest/categories_test.go`:

```go
package rest

import "testing"

func TestParseSlugStart(t *testing.T) {
	cases := []struct {
		slug string
		want int64
		ok   bool
	}{
		{"btc-updown-5m-1786190100", 1786190100, true},
		{"eth-updown-5m-1786192500", 1786192500, true},
		{"btc-updown-15m-1786190100", 1786190100, true},
		{"btc-updown-5m-notanumber", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseSlugStart(c.slug)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseSlugStart(%q) = (%d, %v), 기대 (%d, %v)", c.slug, got, ok, c.want, c.ok)
		}
	}
}

func TestParseSlugStartRejectsUnaligned(t *testing.T) {
	// 5분 경계가 아닌 값은 회차 슬러그일 수 없다
	if _, ok := ParseSlugStart("btc-updown-5m-1786190123"); ok {
		t.Error("5분 경계가 아닌 슬러그를 받아들였다")
	}
}
```

- [ ] **Step 3: 테스트가 실패하는지 확인**

Run: `go test ./internal/predictfun/rest/ -run TestParseSlug -v`
Expected: FAIL — `undefined: ParseSlugStart`

- [ ] **Step 4: 슬러그 파서 구현**

`internal/predictfun/rest/categories.go`:

```go
package rest

import (
	"context"
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
```

- [ ] **Step 5: 테스트 통과 확인**

Run: `go test ./internal/predictfun/rest/ -run TestParseSlug -v`
Expected: PASS

- [ ] **Step 6: 커밋**

```bash
git add go.mod Makefile internal/predictfun/rest/
git commit -m "회차 슬러그 파서 — 5분 경계 검증 포함"
```

- [ ] **Step 7: REST 클라이언트 테스트를 쓴다**

`internal/predictfun/rest/client_test.go`:

```go
package rest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientSendsApiKeyAndUserAgent(t *testing.T) {
	var gotKey, gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		gotUA = r.Header.Get("User-Agent")
		json.NewEncoder(w).Encode(map[string]any{"success": true})
	}))
	defer srv.Close()

	c := New("test-key")
	c.BaseURL = srv.URL
	var out struct {
		Success bool `json:"success"`
	}
	if err := c.Get(context.Background(), "/v1/ping", nil, &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if gotKey != "test-key" {
		t.Errorf("x-api-key = %q, 기대 %q", gotKey, "test-key")
	}
	if gotUA == "" || gotUA == "Go-http-client/1.1" {
		t.Errorf("User-Agent 가 기본값이다: %q — WAF 가 403 으로 막는다", gotUA)
	}
	if !out.Success {
		t.Error("응답 디코딩 실패")
	}
}

func TestClientReportsHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := New("bad")
	c.BaseURL = srv.URL
	err := c.Get(context.Background(), "/v1/markets/1", nil, &struct{}{})
	if err == nil {
		t.Fatal("401 인데 에러가 없다")
	}
}

func TestClientNeverLogsKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := New("super-secret-key")
	c.BaseURL = srv.URL
	err := c.Get(context.Background(), "/v1/x", nil, &struct{}{})
	if err == nil {
		t.Fatal("500 인데 에러가 없다")
	}
	if got := err.Error(); contains(got, "super-secret-key") {
		t.Errorf("에러 메시지에 API 키가 새어나갔다: %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
```

- [ ] **Step 8: 테스트 실패 확인**

Run: `go test ./internal/predictfun/rest/ -run TestClient -v`
Expected: FAIL — `undefined: New`

- [ ] **Step 9: REST 클라이언트 구현**

`internal/predictfun/rest/client.go`:

```go
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
```

- [ ] **Step 10: 테스트 통과 확인**

Run: `go test ./internal/predictfun/rest/ -v`
Expected: PASS (4개 테스트)

- [ ] **Step 11: 커밋**

```bash
git add internal/predictfun/rest/
git commit -m "predict.fun REST 클라이언트 — 레이트리밋·UA·키 미노출"
```

- [ ] **Step 12: 회차 수집 테스트를 쓴다**

`internal/predictfun/rest/categories_test.go` 에 추가:

```go
func TestFetchResolvedRoundsPaginatesAndFilters(t *testing.T) {
	pages := []string{
		`{"success":true,"cursor":"P2","data":[
		  {"slug":"btc-updown-5m-1786190100","status":"RESOLVED",
		   "variantData":{"startPrice":"100.5","endPrice":"101.0","priceFeedSymbol":"BTCUSDT"}},
		  {"slug":"eth-updown-5m-1786190100","status":"RESOLVED",
		   "variantData":{"startPrice":"1.0","endPrice":"2.0","priceFeedSymbol":"ETHUSDT"}}
		]}`,
		`{"success":true,"cursor":null,"data":[
		  {"slug":"btc-updown-5m-1786189800","status":"RESOLVED",
		   "variantData":{"startPrice":"99.0","endPrice":"98.0","priceFeedSymbol":"BTCUSDT"}}
		]}`,
	}
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n >= len(pages) {
			t.Errorf("페이지를 %d개보다 많이 요청했다", len(pages))
			w.WriteHeader(500)
			return
		}
		w.Write([]byte(pages[n]))
		n++
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	got, err := FetchResolvedRounds(context.Background(), c, "btc", 0)
	if err != nil {
		t.Fatalf("FetchResolvedRounds: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("회차 %d개, 기대 2개 (eth 는 걸러져야 한다)", len(got))
	}
	if got[0].StartPrice != 100.5 || got[0].EndPrice != 101.0 {
		t.Errorf("가격 파싱 실패: %+v", got[0])
	}
	if got[0].StartUnix != 1786190100 {
		t.Errorf("StartUnix = %d, 기대 1786190100", got[0].StartUnix)
	}
}

func TestFetchResolvedRoundsStopsAtSince(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"cursor":"NEXT","data":[
		  {"slug":"btc-updown-5m-1786190100","status":"RESOLVED",
		   "variantData":{"startPrice":"1","endPrice":"2","priceFeedSymbol":"BTCUSDT"}},
		  {"slug":"btc-updown-5m-1000000200","status":"RESOLVED",
		   "variantData":{"startPrice":"1","endPrice":"2","priceFeedSymbol":"BTCUSDT"}}
		]}`))
	}))
	defer srv.Close()

	c := New("k")
	c.BaseURL = srv.URL
	// sinceUnix 보다 오래된 회차를 만나면 멈춰야 한다 — 안 그러면 무한 페이지네이션
	got, err := FetchResolvedRounds(context.Background(), c, "btc", 1786000000)
	if err != nil {
		t.Fatalf("FetchResolvedRounds: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("회차 %d개, 기대 1개 — sinceUnix 에서 멈춰야 한다", len(got))
	}
}
```

- [ ] **Step 13: 테스트 실패 확인**

Run: `go test ./internal/predictfun/rest/ -run TestFetchResolved -v`
Expected: FAIL — `undefined: FetchResolvedRounds`

- [ ] **Step 14: 회차 수집 구현**

`internal/predictfun/rest/categories.go` 에 추가:

```go
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
```

`categories.go` 의 import 에 `"encoding/json"` 을 추가한다.

- [ ] **Step 15: 테스트 통과 확인**

Run: `go test ./internal/predictfun/rest/ -v`
Expected: PASS (6개 테스트)

- [ ] **Step 16: 커밋**

```bash
git add internal/predictfun/rest/
git commit -m "정산 회차 수집 — 커서 페이지네이션, sinceUnix 정지 조건"
```

- [ ] **Step 17: Binance kline 클라이언트 테스트를 쓴다**

`internal/klines/client_test.go`:

```go
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
```

- [ ] **Step 18: 테스트 실패 확인**

Run: `go test ./internal/klines/ -v`
Expected: FAIL — `undefined: Fetch`

- [ ] **Step 19: kline 클라이언트 구현**

`internal/klines/client.go`:

```go
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
//        quoteVolume, trades, takerBuyBase, takerBuyQuote, ignore]
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
```

- [ ] **Step 20: 테스트 통과 확인**

Run: `go test ./internal/klines/ -v && go vet ./...`
Expected: PASS, vet 무경고

- [ ] **Step 21: 커밋**

```bash
git add internal/klines/
git commit -m "Binance kline REST 클라이언트 — 페이지네이션·재시도"
```

- [ ] **Step 22: G2 러너 작성**

`cmd/align/main.go`:

```go
// Command align 은 predict.fun 정산 방향과 바이낸스 5분봉 방향의 불일치율을 잰다.
// 이것이 판정 게이트 G2 다. 결과가 나쁘면 나머지 구현을 착수하지 않는다.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/klines"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

func main() {
	days := flag.Int("days", 30, "몇 일치 회차를 검사할지")
	symbol := flag.String("symbol", "BTCUSDT", "바이낸스 심볼")
	prefix := flag.String("prefix", "btc", "회차 슬러그 접두사")
	flag.Parse()

	key := os.Getenv("PREDICT_API_KEY")
	if key == "" {
		fmt.Fprintln(os.Stderr, "PREDICT_API_KEY 환경변수가 필요합니다")
		os.Exit(2)
	}
	if err := run(key, *days, *symbol, *prefix); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(1)
	}
}

func run(key string, days int, symbol, prefix string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	since := time.Now().Unix() - int64(days)*86400
	fmt.Printf("[수집] predict.fun 정산 회차 (최근 %d일)\n", days)
	rounds, err := rest.FetchResolvedRounds(ctx, rest.New(key), prefix, since)
	if err != nil {
		return err
	}
	if len(rounds) == 0 {
		return fmt.Errorf("회차를 하나도 못 받았습니다")
	}
	lo, hi := rounds[len(rounds)-1].StartUnix, rounds[0].StartUnix
	fmt.Printf("  회차 %d개  %s ~ %s\n", len(rounds), iso(lo), iso(hi))

	fmt.Printf("[수집] 바이낸스 %s 5분봉\n", symbol)
	ks, err := klines.Fetch(ctx, symbol, "5m", lo*1000, (hi+300)*1000)
	if err != nil {
		return err
	}
	byOpen := make(map[int64]klines.Kline, len(ks))
	for _, k := range ks {
		byOpen[k.OpenTime/1000] = k
	}
	fmt.Printf("  봉 %d개\n", len(ks))

	var n, disagree, chainFlat, binFlat, missing int
	for _, r := range rounds {
		k, ok := byOpen[r.StartUnix]
		if !ok {
			missing++
			continue
		}
		chainUp := sign(r.EndPrice - r.StartPrice)
		binUp := sign(k.Close - k.Open)
		if chainUp == 0 {
			chainFlat++
		}
		if binUp == 0 {
			binFlat++
		}
		if chainUp == 0 || binUp == 0 {
			continue // 도지·무변동은 방향 비교 대상이 아니다
		}
		n++
		if chainUp != binUp {
			disagree++
		}
	}
	if n == 0 {
		return fmt.Errorf("비교 가능한 회차가 없습니다")
	}

	d := float64(disagree) / float64(n)
	se := math.Sqrt(d * (1 - d) / float64(n))
	fmt.Println()
	fmt.Println("==================== G2 정산 정합 ====================")
	fmt.Printf("  비교 회차     : %d개  (봉 결측 %d, chainlink 무변동 %d, 바이낸스 도지 %d)\n",
		n, missing, chainFlat, binFlat)
	fmt.Printf("  불일치        : %d개\n", disagree)
	fmt.Printf("  불일치율 d    : %.4f%%  ±%.4f%%p (1SE)\n", d*100, se*100)
	fmt.Println()

	const baseEdge = 2.270  // %p — 문턱 0.0172 실측 52.270%
	const dojiBias = -0.282 // %p — 도지 제외 낙관 편향
	const rebate = 0.500    // %p — 메이커 리베이트
	effective := baseEdge * (1 - 2*d)
	total := effective + dojiBias + rebate

	fmt.Println("  기대값 분해 (%p)")
	fmt.Printf("    기준 엣지          %+7.3f\n", baseEdge)
	fmt.Printf("    정산 불일치 반영   %+7.3f   (× (1 − 2d))\n", effective-baseEdge)
	fmt.Printf("    도지 제외 편향     %+7.3f\n", dojiBias)
	fmt.Printf("    메이커 리베이트    %+7.3f\n", rebate)
	fmt.Printf("    ─────────────────────────\n")
	fmt.Printf("    합계               %+7.3f\n", total)
	fmt.Println()
	if total <= 0 {
		fmt.Println("  판정: 실패 — 기대값이 0 이하다. P4 이후를 착수하지 않는다.")
		return nil
	}
	fmt.Println("  판정: 통과 — 다음 단계로 간다.")
	return nil
}

func sign(x float64) int {
	switch {
	case x > 0:
		return 1
	case x < 0:
		return -1
	}
	return 0
}

func iso(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04 UTC")
}
```

- [ ] **Step 23: 빌드하고 G2 를 실행한다**

```bash
go build ./... && go vet ./...
PREDICT_API_KEY=<키> go run ./cmd/align -days 30 2>&1 | tee out/g2-align.log
```

Expected: 불일치율 `d` 와 기대값 합계가 출력된다. **합계가 0 이하이면 여기서 멈추고 사람에게 보고한다.**

- [ ] **Step 24: 결과를 커밋**

```bash
mkdir -p docs/results && cp out/g2-align.log docs/results/g2-align.log
git add cmd/align/ docs/results/g2-align.log
git commit -m "G2 정합 게이트 — Chainlink 정산 vs 바이낸스 봉 불일치율 측정"
```

---

**여기서 멈추고 G2 결과를 검토한다. 기대값 합계가 양수일 때만 Task 2 로 넘어간다.**

---

### Task 2: bars 패키지

**Files:**
- Create: `internal/bars/bars.go`, `internal/bars/bars_test.go`

**Interfaces:**
- Produces: `bars.Bars` 구조체와 `Len() int`, `Slice(lo, hi int) Bars`, `Last(n int) Bars`, `IndexOfOpenTime(t int64) int`

- [ ] **Step 1: 실패 테스트를 쓴다**

`internal/bars/bars_test.go`:

```go
package bars

import "testing"

func sample(n int) Bars {
	b := Bars{
		OpenTime: make([]int64, n), CloseTime: make([]int64, n),
		Open: make([]float64, n), High: make([]float64, n),
		Low: make([]float64, n), Close: make([]float64, n),
		Volume: make([]float64, n), QuoteVolume: make([]float64, n),
		Trades: make([]int64, n), TakerBuyBase: make([]float64, n),
		TakerBuyQuote: make([]float64, n),
	}
	for i := 0; i < n; i++ {
		b.OpenTime[i] = int64(i) * 60_000
		b.CloseTime[i] = b.OpenTime[i] + 59_999
		b.Close[i] = float64(i)
	}
	return b
}

func TestLenAndSlice(t *testing.T) {
	b := sample(10)
	if b.Len() != 10 {
		t.Fatalf("Len = %d, 기대 10", b.Len())
	}
	s := b.Slice(2, 5)
	if s.Len() != 3 {
		t.Fatalf("Slice(2,5).Len = %d, 기대 3", s.Len())
	}
	if s.OpenTime[0] != 120_000 || s.Close[2] != 4 {
		t.Errorf("슬라이스 내용 오류: %v %v", s.OpenTime, s.Close)
	}
}

func TestSliceCoversEveryField(t *testing.T) {
	// 필드를 하나라도 빠뜨리면 길이가 어긋난다 — 조용한 버그를 여기서 잡는다
	b := sample(10)
	s := b.Slice(3, 7)
	lens := []int{
		len(s.OpenTime), len(s.CloseTime), len(s.Open), len(s.High), len(s.Low),
		len(s.Close), len(s.Volume), len(s.QuoteVolume), len(s.Trades),
		len(s.TakerBuyBase), len(s.TakerBuyQuote),
	}
	for i, l := range lens {
		if l != 4 {
			t.Errorf("필드 %d 의 길이가 %d 다 — Slice 가 그 필드를 빠뜨렸다", i, l)
		}
	}
}

func TestLastClampsToLength(t *testing.T) {
	b := sample(5)
	if got := b.Last(10).Len(); got != 5 {
		t.Errorf("Last(10).Len = %d, 기대 5", got)
	}
	if got := b.Last(3); got.Len() != 3 || got.Close[0] != 2 {
		t.Errorf("Last(3) 오류: len=%d close[0]=%v", got.Len(), got.Close[0])
	}
}

func TestIndexOfOpenTime(t *testing.T) {
	b := sample(5)
	if got := b.IndexOfOpenTime(180_000); got != 3 {
		t.Errorf("IndexOfOpenTime(180000) = %d, 기대 3", got)
	}
	if got := b.IndexOfOpenTime(180_001); got != -1 {
		t.Errorf("없는 시각인데 %d 를 돌려줬다", got)
	}
	if got := b.IndexOfOpenTime(999_999_999); got != -1 {
		t.Errorf("범위 밖인데 %d 를 돌려줬다", got)
	}
}
```

- [ ] **Step 2: 테스트 실패 확인**

Run: `go test ./internal/bars/ -v`
Expected: FAIL — `undefined: Bars`

- [ ] **Step 3: 구현**

`internal/bars/bars.go`:

```go
// Package bars 는 정렬·중복제거된 OHLCV 시계열을 담는다.
// 시점 절단(미래참조 차단)은 internal/clock 의 책임이지 여기가 아니다.
package bars

import "sort"

// Bars 의 모든 슬라이스는 길이가 같다.
type Bars struct {
	OpenTime      []int64   // ms, 봉 시작
	CloseTime     []int64   // ms, 봉의 마지막 순간 (OpenTime + 간격 − 1)
	Open          []float64
	High          []float64
	Low           []float64
	Close         []float64
	Volume        []float64
	QuoteVolume   []float64
	Trades        []int64
	TakerBuyBase  []float64
	TakerBuyQuote []float64
}

func (b Bars) Len() int { return len(b.OpenTime) }

// Slice 는 [lo, hi) 구간을 돌려준다. Go 슬라이스는 백킹 배열을 공유하므로 복사가 없다.
func (b Bars) Slice(lo, hi int) Bars {
	return Bars{
		OpenTime:      b.OpenTime[lo:hi],
		CloseTime:     b.CloseTime[lo:hi],
		Open:          b.Open[lo:hi],
		High:          b.High[lo:hi],
		Low:           b.Low[lo:hi],
		Close:         b.Close[lo:hi],
		Volume:        b.Volume[lo:hi],
		QuoteVolume:   b.QuoteVolume[lo:hi],
		Trades:        b.Trades[lo:hi],
		TakerBuyBase:  b.TakerBuyBase[lo:hi],
		TakerBuyQuote: b.TakerBuyQuote[lo:hi],
	}
}

// Last 는 마지막 n개. n 이 길이보다 크면 있는 만큼 돌려준다.
func (b Bars) Last(n int) Bars {
	if n >= b.Len() {
		return b
	}
	return b.Slice(b.Len()-n, b.Len())
}

// IndexOfOpenTime 은 OpenTime == t 인 봉의 인덱스. 없으면 -1.
func (b Bars) IndexOfOpenTime(t int64) int {
	i := sort.Search(b.Len(), func(i int) bool { return b.OpenTime[i] >= t })
	if i < b.Len() && b.OpenTime[i] == t {
		return i
	}
	return -1
}
```

- [ ] **Step 4: 테스트 통과 확인**

Run: `go test ./internal/bars/ -v`
Expected: PASS (4개 테스트)

- [ ] **Step 5: 커밋**

```bash
git add internal/bars/
git commit -m "bars 패키지 — OHLCV 값 타입과 슬라이스"
```

---

계획이 길어 나머지 태스크는 이어지는 문서에 있다. Task 3 이후는
`2026-08-08-p0-p3-model-port-and-alignment-gate-part2.md` 를 본다.
