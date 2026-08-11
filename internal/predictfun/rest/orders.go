package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 이 파일은 계정 스코프 엔드포인트(주문 생성·취소, 포지션, 예약잔고)를 붙인다.
// 넷 다 Bearer JWT 가 필요하다 — x-api-key 만으로는 401 이다(P4 실측).
//
// # 응답 파싱이 이 파일의 진짜 위험이다
//
// 이 저장소는 "계측·파싱 도구가 대상보다 먼저 틀린다"를 다섯 번 밟았다.
// 응답 파싱이 틀리면 거래소가 무엇을 말했는지 자체를 모르게 되고, 그 상태로
// 주문을 계속 낸다. 그래서 여기서는 세 가지를 지킨다.
//
//  1. 봉투(`{"data":…,"success":true}`)는 받은 자리에서 한 번만 벗긴다.
//  2. `success:false` 는 HTTP 200 이어도 실패다. 상태 코드만 보면 안 된다.
//  3. 필드가 없거나 타입이 다르면 **조용한 제로값이 아니라 에러**다.
//     단 하나의 예외가 주문 생성이다(아래 OrderError 참고).

// ----------------------------------------------------------------- 공통 봉투

// envelopeMeta 는 모든 응답이 공유하는 봉투 바깥쪽이다. 각 응답 타입이 이걸
// 묻어 두고 Data 만 자기 모양으로 선언한다.
//
// Error/Message 를 string 이 아니라 RawMessage 로 받는 이유: 이 필드가 객체로
// 오는 순간 string 선언은 디코딩 전체를 실패시킨다. 거부 사유를 담은 필드가
// 거부 사유를 지우는 원인이 되면 안 된다.
type envelopeMeta struct {
	Success *bool           `json:"success"`
	Message json.RawMessage `json:"message"`
	Error   json.RawMessage `json:"error"`
}

// verdict 는 HTTP 2xx 안에 숨은 실패를 잡는다. success 필드가 아예 없으면
// 판단하지 않는다 — 있는데 false 인 것만 실패다.
func (m envelopeMeta) verdict() error {
	if m.Success != nil && !*m.Success {
		return fmt.Errorf("거래소가 success:false 를 돌려줬다%s", m.detail())
	}
	return nil
}

func (m envelopeMeta) detail() string {
	var parts []string
	if s := rawText(m.Error); s != "" {
		parts = append(parts, "error="+s)
	}
	if s := rawText(m.Message); s != "" {
		parts = append(parts, "message="+s)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, " ") + ")"
}

func rawText(r json.RawMessage) string {
	s := strings.TrimSpace(string(r))
	if s == "" || s == "null" {
		return ""
	}
	return truncate(s, 200)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// flexString 은 문자열로도 숫자로도 오는 식별자를 받는다.
//
// 이게 없으면 `orderId` 가 숫자로 오는 순간 디코딩이 통째로 실패하고, **이미
// 생성된 주문의 ID 를 잃는다** — 취소할 수 없는 주문이 남는다는 뜻이다.
// 관대함이 정당화되는 드문 자리다.
type flexString string

func (s *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case len(b) == 0 || string(b) == "null":
		*s = ""
		return nil
	case b[0] == '"':
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = flexString(v)
		return nil
	case b[0] == '-' || (b[0] >= '0' && b[0] <= '9'):
		var n json.Number
		if err := json.Unmarshal(b, &n); err != nil {
			return err
		}
		*s = flexString(n.String())
		return nil
	}
	return fmt.Errorf("문자열도 숫자도 아니다: %s", truncate(string(b), 40))
}

// ------------------------------------------------------------------ 주문 생성

// OrderErrorKind 는 주문 생성 실패를 재시도 관점에서 셋으로 나눈다.
//
// 이 구분이 없으면 호출자는 모든 실패를 똑같이 다루고, "보냈는데 응답을 못
// 받은" 상태를 실패로 단정해 재시도한다 — 주문이 둘 들어가고 노출이 두 배가
// 된다. 다른 패키지(risk·ledger)는 애매하면 거래하지 않는 쪽으로 실패하지만,
// 주문 생성만은 "실패로 단정"이 곧 이중 주문이라 그 규칙이 뒤집힌다.
type OrderErrorKind int

const (
	// OrderNotSent — 요청이 네트워크에 나가지 않았다. 주문은 존재하지 않는다.
	OrderNotSent OrderErrorKind = iota
	// OrderRejected — 거래소가 명시적으로 거부했다. 주문은 존재하지 않는다.
	OrderRejected
	// OrderUnknown — 요청은 나갔는데 결과를 모른다. **재시도하면 안 된다.**
	// 조회(GET /v1/orders/{hash})로 대조하는 것이 유일하게 안전한 조치다.
	OrderUnknown
)

func (k OrderErrorKind) String() string {
	switch k {
	case OrderNotSent:
		return "전송 안 됨"
	case OrderRejected:
		return "거래소 거부"
	case OrderUnknown:
		return "결과 불명"
	}
	return "알 수 없는 분류"
}

type OrderError struct {
	Kind OrderErrorKind
	Err  error
}

func (e *OrderError) Error() string {
	return fmt.Sprintf("주문 생성 실패(%s): %v", e.Kind, e.Err)
}
func (e *OrderError) Unwrap() error { return e.Err }

// SafeToRetry 는 같은 주문을 다시 보내도 **이중 주문이 나지 않는** 경우에만
// true 다. "재시도하면 성공한다"는 뜻이 아니다 — 거래소가 거부한 주문은 다시
// 보내도 또 거부되지만, 적어도 둘이 들어가지는 않는다.
func (e *OrderError) SafeToRetry() bool { return e.Kind != OrderUnknown }

// CreateOrderResult 는 POST /v1/orders 의 CreateOrderResponseData 다.
type CreateOrderResult struct {
	Code      string
	OrderID   string
	OrderHash string

	// RemovalLockedUntil 은 이 시각 전에는 취소가 거부되는 잠금 창의 끝이다.
	// **잠금 길이를 가정하지 않는다** — 응답값을 그대로 읽어 지킨다.
	RemovalLockedUntil time.Time

	// RemovalLockUnknown 은 잠금 시각을 못 읽었다는 뜻이다(필드 없음, null,
	// 빈 값, RFC3339 아님).
	//
	// 이 플래그가 없으면 "잠금 없음"과 "모름"이 둘 다 제로시각으로 보인다.
	// 호출자는 곧바로 취소를 시도하고 rejected 를 받는데, 그 경로가 안전망으로
	// 동작하는지는 호출자가 rejected 를 재시도로 다루는지에 달려 있다. 모른다는
	// 사실 자체를 값에 담아 그 결정을 호출자에게 넘긴다.
	RemovalLockUnknown bool
}

type createOrderResponse struct {
	envelopeMeta
	Data struct {
		Code               flexString `json:"code"`
		OrderID            flexString `json:"orderId"`
		OrderHash          flexString `json:"orderHash"`
		RemovalLockedUntil flexString `json:"removalLockedUntil"`
	} `json:"data"`
}

// CreateOrder 는 서명된 주문을 POST /v1/orders 로 보낸다.
//
// body 는 CreateOrderData(pricePerShare, strategy, order(ContractOrder) 등)를
// 그대로 담은 값이다. 이 패키지는 그 내용을 해석하지 않는다 — **특히
// maker/signer 를 여기서 채우지 않는다.** 그 값은 설정(PREDICT_ACCOUNT)에서만
// 오고, `GET /v1/account` 의 address 는 스마트계정이 아니라 서명자 EOA 라서
// 그것으로 유도하면 자금 없는 EOA 로 주문이 나간다(P4 실측).
//
// 에러는 항상 *OrderError 다. 호출자는 SafeToRetry() 를 보고 재시도를 정한다.
func (c *Client) CreateOrder(ctx context.Context, body any) (CreateOrderResult, error) {
	if body == nil {
		return CreateOrderResult{}, &OrderError{Kind: OrderNotSent, Err: errors.New("주문 본문이 nil 이다")}
	}

	var resp createOrderResponse
	// **`data` 봉투는 여기서 씌운다.** 스펙의 `CreateOrderRequest` 는
	// `required: [data]` 이고 그 안이 `CreateOrderData` 다. 호출자는
	// CreateOrderData 만 만들면 되고, 이 경로가 어떤 봉투를 요구하는지는
	// 엔드포인트를 아는 이 층이 안다.
	//
	// 이걸 빠뜨리면 거래소는 400 으로 답한다:
	//   Expected input type "CreateOrderData", found null.
	// 2026-08-11 첫 무장에서 실제로 그랬고, 주문 182건이 전부 거부됐다.
	// 손실은 없었지만 — **거부는 조용한 실패가 아니어서 다행이었다.**
	// 같은 결함이 취소 경로(RemoveOrders)에도 있었고, 그쪽이 훨씬 위험했다.
	if err := c.PostAuth(ctx, "/v1/orders", map[string]any{"data": body}, &resp); err != nil {
		return CreateOrderResult{}, classifyCreate(err)
	}
	if err := resp.verdict(); err != nil {
		// 200 인데 success:false 다. 거래소가 명시적으로 아니라고 한 것이므로
		// 거부로 다룬다.
		return CreateOrderResult{}, &OrderError{Kind: OrderRejected, Err: err}
	}

	res := CreateOrderResult{
		Code:      string(resp.Data.Code),
		OrderID:   string(resp.Data.OrderID),
		OrderHash: string(resp.Data.OrderHash),
	}
	if t, ok := parseRFC3339(string(resp.Data.RemovalLockedUntil)); ok {
		res.RemovalLockedUntil = t
	} else {
		res.RemovalLockUnknown = true
	}

	if res.OrderID == "" {
		// 주문이 생성됐는지 아닌지를 우리는 모르고, 식별자가 없으니 취소도 못
		// 한다. 해시라도 있으면 대조에 쓸 수 있게 에러에 싣는다.
		detail := "응답에 orderId 가 없다"
		if res.OrderHash != "" {
			detail += " (orderHash=" + res.OrderHash + " — 이 해시로 대조해야 한다)"
		}
		return res, &OrderError{Kind: OrderUnknown, Err: errors.New(detail)}
	}
	return res, nil
}

// classifyCreate 는 전송 계층의 실패를 재시도 안전성으로 옮긴다.
//
// 기본값이 OrderUnknown 인 것이 핵심이다. 분류하지 못한 에러를 "안전"으로
// 떨어뜨리면 새로운 실패 모드가 생길 때마다 이중 주문 위험이 열린다.
func classifyCreate(err error) *OrderError {
	var te *transportError
	if errors.As(err, &te) {
		if te.Sent {
			return &OrderError{Kind: OrderUnknown, Err: err}
		}
		return &OrderError{Kind: OrderNotSent, Err: err}
	}
	var he *HTTPError
	if errors.As(err, &he) && he.Status >= 400 && he.Status <= 499 {
		// 4xx 는 거래소가 요청을 받아 보고 거부한 것이다(서명 불량, 잔고 부족,
		// 레이트리밋). 주문은 생성되지 않았다.
		return &OrderError{Kind: OrderRejected, Err: err}
	}
	// 5xx·3xx·디코딩 실패·그 밖 전부: 주문이 들어갔을 수 있다.
	return &OrderError{Kind: OrderUnknown, Err: err}
}

func parseRFC3339(s string) (time.Time, bool) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ------------------------------------------------------------------ 주문 취소

// MaxRemoveIDs 는 POST /v1/orders/remove 의 배치 상한이다(스펙).
const MaxRemoveIDs = 100

// RemoveResult 는 취소 요청의 결과를 ID 별로 가른다.
type RemoveResult struct {
	Removed  []string // 취소됨
	Noop     []string // 이미 끝난 주문 — 취소할 것이 없었다
	Rejected []string // 취소 잠금 창 안이라 거부됨 — 잠금이 풀리면 재시도 대상

	// Unaccounted 는 우리가 취소를 요청했는데 세 바구니 어디에도 나타나지 않은
	// ID 다. 서버가 준 값이 아니라 여기서 계산한다.
	//
	// 이게 없으면 응답에서 조용히 빠진 ID 가 "거부되지 않음"과 구별되지 않아,
	// 호출자는 재시도를 멈추고 살아 있는 주문을 잊는다. 잊힌 매수 주문은
	// 체결되고, 그 노출은 어떤 한도에도 잡히지 않는다.
	Unaccounted []string
}

type removeOrdersResponse struct {
	envelopeMeta
	Data struct {
		Removed  []flexString `json:"removed"`
		Noop     []flexString `json:"noop"`
		Rejected []flexString `json:"rejected"`
	} `json:"data"`
}

// RemoveOrders 는 주문 ID 들을 한 요청으로 취소한다.
//
// 상한(100개)을 넘기면 요청을 보내지 않고 에러다. 알아서 쪼개지 않는 이유:
// 한 번의 호출이 몇 개의 요청으로 나갈지 호출자가 모르면 레이트리밋 예산이
// 보이지 않게 새고, 중간 배치가 실패했을 때 무엇이 취소됐는지도 흐려진다.
// 이 봇은 회차당 미체결 주문이 많아야 1건이라 상한에 닿지 않는다.
func (c *Client) RemoveOrders(ctx context.Context, ids []string) (RemoveResult, error) {
	if len(ids) == 0 {
		// 취소할 것이 없으면 레이트리밋 예산을 쓰지 않는다.
		return RemoveResult{}, nil
	}
	if len(ids) > MaxRemoveIDs {
		return RemoveResult{}, fmt.Errorf("취소 배치 상한 초과: %d개 (최대 %d개) — 호출자가 나눠야 한다", len(ids), MaxRemoveIDs)
	}

	var resp removeOrdersResponse
	// **`data` 봉투.** 스펙의 `RemoveOrdersRequest` 도 `required: [data]` 이고
	// 그 안이 `RemoveOrdersData{ids}` 다. 생성 경로와 같은 함정인데 이쪽이
	// 훨씬 위험하다 — 생성이 400 이면 주문이 없어서 아무 일도 안 일어나지만,
	// **취소가 400 이면 살아 있는 주문을 거둘 수 없다.** 노출 불변식 전체가
	// 취소가 도달한다는 전제 위에 서 있다.
	//
	// 2026-08-11 첫 무장에서 생성 쪽 400 이 먼저 터지는 바람에 주문이 하나도
	// 만들어지지 않았고, 그래서 이 결함이 드러나지 않은 채 지나갈 뻔했다.
	if err := c.PostAuth(ctx, "/v1/orders/remove", map[string]any{
		"data": map[string]any{"ids": ids},
	}, &resp); err != nil {
		return RemoveResult{}, fmt.Errorf("주문 취소 실패: %w", err)
	}
	if err := resp.verdict(); err != nil {
		return RemoveResult{}, fmt.Errorf("주문 취소 실패: %w", err)
	}

	res := RemoveResult{
		Removed:  flatten(resp.Data.Removed),
		Noop:     flatten(resp.Data.Noop),
		Rejected: flatten(resp.Data.Rejected),
	}
	seen := make(map[string]bool, len(ids))
	for _, group := range [][]string{res.Removed, res.Noop, res.Rejected} {
		for _, id := range group {
			seen[id] = true
		}
	}
	dup := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !seen[id] && !dup[id] {
			dup[id] = true
			res.Unaccounted = append(res.Unaccounted, id)
		}
	}
	return res, nil
}

func flatten(in []flexString) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = string(v)
	}
	return out
}

// -------------------------------------------------------------------- 포지션

// Position 은 GET /v1/positions 의 PositionData 한 건이다.
//
// **필드를 넷으로 줄인 것은 의도적이다.** P4 가 스펙에서 확정한 이름은
// amount / valueUsd / averageBuyPriceUsd / pnlUsd 넷뿐이다. 마켓·토큰 식별자의
// 스펙상 이름은 확인되지 않았고, 추측한 이름은 조용히 빈 값이 되어 "그 회차에
// 포지션이 없다"는 거짓말이 된다. 사이징이 필요로 하는 것은 취득원가 합이므로
// 지금은 이 넷으로 충분하다.
type Position struct {
	// AmountWei 는 응답 그대로의 보유 주식 수(10진 문자열)다.
	AmountWei string
	// AmountShares 는 AmountWei / 1e18 이다. 18 decimals 가정은 서명 경로가
	// 이미 쓰는 것과 같다(order.AmountsForBuy 의 takerAmount).
	AmountShares       float64
	ValueUSD           float64
	AverageBuyPriceUSD float64
	PnLUSD             float64
}

// CostUSD 는 취득원가다 — 주식 수 × 평균 매입가. risk.Equity.PositionCost 가
// 이 값들의 합이다(시가 평가가 아니다).
func (p Position) CostUSD() float64 { return p.AmountShares * p.AverageBuyPriceUSD }

type positionsPage struct {
	envelopeMeta
	Cursor     *string `json:"cursor"`
	NextCursor *string `json:"nextCursor"`
	Data       []struct {
		Amount             json.Number `json:"amount"`
		ValueUSD           json.Number `json:"valueUsd"`
		AverageBuyPriceUSD json.Number `json:"averageBuyPriceUsd"`
		PnLUSD             json.Number `json:"pnlUsd"`
	} `json:"data"`
}

// positionsPageSize 는 한 페이지에 요청할 건수다.
const positionsPageSize = 50

// maxPositionPages 는 커서 페이지네이션의 상한이다. 정상 경로에서는 닿지
// 않는다 — 이 봇의 포지션은 회차당 한 자리 수다.
var maxPositionPages = 200

// Positions 는 계정의 포지션 전체를 페이지네이션으로 모은다.
//
// **페이징 파라미터는 first/after 다. limit/cursor 가 아니다.** 서버는 모르는
// 파라미터를 400 으로 거절하지 않고 조용히 무시하므로, 틀리면 매번 1페이지만
// 받으면서 아무 에러도 나지 않는다(P4 실측).
func (c *Client) Positions(ctx context.Context) ([]Position, error) {
	var out []Position
	cursor := ""
	for page := 0; page < maxPositionPages; page++ {
		q := url.Values{}
		q.Set("first", strconv.Itoa(positionsPageSize))
		if cursor != "" {
			q.Set("after", cursor)
		}

		var pg positionsPage
		if err := c.GetAuth(ctx, "/v1/positions", q, &pg); err != nil {
			return nil, fmt.Errorf("포지션 조회 %d번째 페이지: %w", page, err)
		}
		if err := pg.verdict(); err != nil {
			return nil, fmt.Errorf("포지션 조회 %d번째 페이지: %w", page, err)
		}

		for i, row := range pg.Data {
			amt, err := requiredNumber(row.Amount, "amount")
			if err != nil {
				return nil, fmt.Errorf("포지션 %d번째 항목: %w", i, err)
			}
			if amt < 0 {
				return nil, fmt.Errorf("포지션 %d번째 항목: amount 가 음수다 (%v) — 취득원가가 노출을 깎는다", i, amt)
			}
			avg, err := requiredNumber(row.AverageBuyPriceUSD, "averageBuyPriceUsd")
			if err != nil {
				return nil, fmt.Errorf("포지션 %d번째 항목: %w", i, err)
			}
			if avg < 0 {
				return nil, fmt.Errorf("포지션 %d번째 항목: averageBuyPriceUsd 가 음수다 (%v)", i, avg)
			}
			val, err := optionalNumber(row.ValueUSD, "valueUsd")
			if err != nil {
				return nil, fmt.Errorf("포지션 %d번째 항목: %w", i, err)
			}
			pnl, err := optionalNumber(row.PnLUSD, "pnlUsd")
			if err != nil {
				return nil, fmt.Errorf("포지션 %d번째 항목: %w", i, err)
			}
			out = append(out, Position{
				AmountWei:          row.Amount.String(),
				AmountShares:       amt / weiPerUnit,
				ValueUSD:           val,
				AverageBuyPriceUSD: avg,
				PnLUSD:             pnl,
			})
		}

		next := firstNonEmpty(pg.Cursor, pg.NextCursor)
		if next == "" {
			if len(pg.Data) >= positionsPageSize {
				// 페이지가 가득 찼는데 커서가 없다. 커서 필드 이름이 바뀌었을
				// 때의 모습이 정확히 이것이고, 그대로 두면 포지션 일부를 못 본
				// 채 조용히 성공한다.
				return nil, fmt.Errorf("포지션 조회 %d번째 페이지: %d건이 가득 찼는데 커서가 없다 — 커서 필드 이름이 바뀐 것으로 보인다", page, len(pg.Data))
			}
			return out, nil
		}
		if next == cursor {
			return nil, fmt.Errorf("포지션 조회 %d번째 페이지: 커서가 전진하지 않는다", page)
		}
		cursor = next
	}
	return nil, fmt.Errorf("포지션 조회: 페이지 상한 %d 를 넘었다 — 커서가 순환하는 것으로 보인다", maxPositionPages)
}

func firstNonEmpty(vals ...*string) string {
	for _, v := range vals {
		if v != nil && *v != "" {
			return *v
		}
	}
	return ""
}

// ------------------------------------------------------------------ 예약잔고

// weiPerUnit 은 18 decimals 스케일이다. USDT 는 BSC 에서 18 decimals 다.
const weiPerUnit = 1e18

// reservedBalancesPath 아래 넷은 **실물로 확정됐다(2026-08-10, P5 Task 8).**
//
// P4 는 요청 배열의 키 이름을 확인하지 못해 `"queries"` 로 추측해 뒀는데,
// **그것이 틀렸다.** 메인넷 DRY-RUN 첫 기동에서 HTTP 400 이 왔다:
//
//	parse request payload error: Expected input type "list_AssetQuery",
//	found null. (occurred while parsing "ReservedBalancesRequest")
//
// `/docs` 의 OpenAPI 스펙에서 `ReservedBalancesRequest` 를 직접 읽어 확정했다 —
// required 필드는 `assets` 하나이고 항목은 `AssetQuery`(type 으로 판별,
// USDT|SHARE)다. 응답 `ReservedBalanceData` 의 금액 필드는 `amount` 이고
// wei 단위 10진 문자열이다(reservedAmountKeys 의 첫 후보와 같다).
//
// **이 값이 틀리면 무슨 일이 일어나는가**: 예약잔고 조회가 통째로 실패하고,
// equity 를 못 읽어 봇이 영원히 무장하지 못한다. 조용한 실패가 아니라
// 시끄러운 실패라서 다행이었다 — 만약 서버가 모르는 키를 무시하고 200 에
// 빈 배열을 줬다면 예약잔고가 0 으로 보여 자유 자금을 과대평가했을 것이다.
const (
	reservedBalancesPath = "/v1/account/reserved-balances/query"
	reservedQueryKey     = "assets" // 확정 (OpenAPI ReservedBalancesRequest.required)
	reservedTypeUSDT     = "USDT"   // 확정 (스펙 enum)
	reservedTypeShare    = "SHARE"  // 확정 (스펙 enum)
)

// reservedAmountKeys 는 금액 필드 이름의 후보다. 여러 후보를 순서대로 보는
// 방식은 이 계층에 이미 있는 패턴이다(auth.pickString).
var reservedAmountKeys = []string{"amount", "balance", "reservedAmount", "reserved", "value"}

type reservedBalancesResponse struct {
	envelopeMeta
	Data []map[string]any `json:"data"`
}

// ReservedUSDT 는 지금 미체결 주문이 묶어 두고 있는 USDT 를 돌려준다.
//
// **모르면 0 이 아니라 에러다.** 예약잔고를 0 으로 잘못 읽으면 봇은 자유 자금이
// 실제보다 많다고 믿고 한도를 넘겨 주문한다 — 실제 돈이 나가는 방향의 실패다.
// 그래서 형태를 못 알아본 응답은 전부 에러로 떨어뜨린다. 반대로 빈 목록은
// 정상이다 — 예약된 것이 없다는 뜻이고, 그건 우리가 아는 사실이다.
//
// 스펙 주의: 이 값은 point-in-time 스냅샷이라 방금 체결/취소된 주문이 반영되지
// 않을 수 있다. 그래서 이 값만으로 노출 불변식을 대체하지 않는다.
func (c *Client) ReservedUSDT(ctx context.Context) (float64, error) {
	body := map[string]any{
		reservedQueryKey: []map[string]any{{"type": reservedTypeUSDT}},
	}
	var resp reservedBalancesResponse
	if err := c.PostAuth(ctx, reservedBalancesPath, body, &resp); err != nil {
		return 0, fmt.Errorf("예약잔고 조회 실패: %w", err)
	}
	if err := resp.verdict(); err != nil {
		return 0, fmt.Errorf("예약잔고 조회 실패: %w", err)
	}

	total := 0.0
	for i, item := range resp.Data {
		typ, ok := item["type"].(string)
		if !ok || typ == "" {
			return 0, fmt.Errorf("예약잔고 %d번째 항목: type 필드가 없다 — 무엇의 잔고인지 모르는 값을 더할 수 없다", i)
		}
		switch {
		case strings.EqualFold(typ, reservedTypeShare):
			continue // 주식 예약은 USDT 노출이 아니다
		case !strings.EqualFold(typ, reservedTypeUSDT):
			return 0, fmt.Errorf("예약잔고 %d번째 항목: 모르는 type %q — USDT 만 물었는데 다른 것이 왔다", i, typ)
		}

		wei, err := pickAmount(item)
		if err != nil {
			return 0, fmt.Errorf("예약잔고 %d번째 항목: %w", i, err)
		}
		if wei < 0 {
			return 0, fmt.Errorf("예약잔고 %d번째 항목: 금액이 음수다 (%v)", i, wei)
		}
		total += wei / weiPerUnit
	}
	if math.IsNaN(total) || math.IsInf(total, 0) {
		return 0, fmt.Errorf("예약잔고 합계가 유한하지 않다 (%v)", total)
	}
	return total, nil
}

// pickAmount 는 후보 키 중 처음 나타나는 것을 wei 값으로 읽는다. 후보가 하나도
// 없거나 값이 숫자가 아니면 에러다 — 0 을 돌려주지 않는다.
func pickAmount(item map[string]any) (float64, error) {
	for _, k := range reservedAmountKeys {
		v, ok := item[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			f, err := strconv.ParseFloat(t, 64)
			if err != nil {
				return 0, fmt.Errorf("%s 를 숫자로 읽지 못했다: %w", k, err)
			}
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return 0, fmt.Errorf("%s 가 유한하지 않다", k)
			}
			return f, nil
		case float64:
			if math.IsNaN(t) || math.IsInf(t, 0) {
				return 0, fmt.Errorf("%s 가 유한하지 않다", k)
			}
			return t, nil
		default:
			return 0, fmt.Errorf("%s 가 문자열도 숫자도 아니다 (%T)", k, v)
		}
	}
	return 0, fmt.Errorf("금액 필드를 못 찾았다 (본 키: %v, 응답 키: %v) — 필드 이름이 바뀐 것으로 보인다",
		reservedAmountKeys, keysOf(item))
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ------------------------------------------------------------------ 숫자 파싱

// requiredNumber 는 필드가 없으면 에러다. json.Number 는 JSON 숫자와
// 따옴표 친 숫자를 둘 다 받고(실측), 숫자가 아닌 문자열·불리언은 디코딩
// 단계에서 이미 걸린다. 여기까지 와서 빈 값인 것은 "필드가 없었다"뿐이다.
func requiredNumber(n json.Number, name string) (float64, error) {
	if n.String() == "" {
		return 0, fmt.Errorf("%s 필드가 없다", name)
	}
	return finiteNumber(n, name)
}

// optionalNumber 는 필드가 없으면 0 이다. 사이징에 쓰이지 않는 필드에만 쓴다.
func optionalNumber(n json.Number, name string) (float64, error) {
	if n.String() == "" {
		return 0, nil
	}
	return finiteNumber(n, name)
}

// finiteNumber 의 NaN/Inf 검사는 **현재 도달할 수 없다** — 변이 시험으로 확인했다.
// encoding/json 은 "NaN"/"Infinity" 를 json.Number 로 받아주지 않고(유효한 숫자
// 리터럴이 아니다), 범위를 넘는 1e400 은 Float64() 가 ErrRange 로 먼저 걸린다.
// 그런데도 남겨 두는 이유: 이 자리가 json.Number 가 아닌 파서로 바뀌면 곧바로
// 뚫린다. 실제로 바로 아래 pickAmount 는 strconv.ParseFloat 을 직접 쓰는데,
// 그쪽은 "NaN"/"Inf"/"infinity" 를 **에러 없이** 통과시킨다(실측).
func finiteNumber(n json.Number, name string) (float64, error) {
	f, err := n.Float64()
	if err != nil {
		return 0, fmt.Errorf("%s 를 숫자로 읽지 못했다: %w", name, err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("%s 가 유한하지 않다 (%v)", name, f)
	}
	return f, nil
}
