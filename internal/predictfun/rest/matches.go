package rest

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"context"
	"encoding/json"
)

// 이 파일은 `GET /v1/orders/matches` 를 붙인다 — **우리 체결을 세는 유일한
// 경로다.** 체결을 못 세면 노출 불변식의 첫 항을 모르고, 그 상태로 재호가를
// 반복하면 한도를 몇 배로 넘겨 베팅한다.
//
// # 최상위 amountFilled 를 우리 체결로 세면 안 된다
//
// 스펙 원문: *"A match event describes a whole transaction, so filtering by a
// maker order also includes the other fills settled in the same transaction."*
// 즉 한 MatchData 는 **트랜잭션 하나 전체**이고, 최상위 `amountFilled` /
// `priceExecuted` 는 **테이커 기준 전체 수치**다. 2026-08-10 메인넷 실측에서
// 한 트랜잭션에 메이커가 5명 붙은 매치를 봤다 — 그 5명분 전부를 우리 체결로
// 세면 노출이 5배로 잡힌다.
//
// 그래서 이 파일은 최상위 수치를 **파싱하지 않는다.** 아예 타입에 자리를 두지
// 않았다 — 자리가 있으면 언젠가 누가 쓴다. 우리 체결은 `taker`/`makers[]` 중
// signer 가 우리인 원소뿐이고, 그 판별은 호출자(cmd/gld91)가 한다.
//
// # 인증
//
// 스펙의 security 는 이 엔드포인트에 `ApiKeyAuth` 하나만 건다 — Bearer JWT 가
// 필요 없다. 그래서 GetAuth 가 아니라 Get 이다. 반대로 그 말은 **필터를
// 걸지 않으면 남의 체결도 전부 온다**는 뜻이라, 우리 것을 고르는 일이 더
// 중요해진다.

// matchesPath 는 엔드포인트 경로다.
const matchesPath = "/v1/orders/matches"

// MatchesPageSize 는 한 페이지에 요청할 매치 수다.
const MatchesPageSize = 100

// maxMatchPages 는 한 번의 Matches 호출이 넘길 수 있는 페이지 수다.
//
// **상한에 닿으면 에러다 — 잘라서 돌려주지 않는다.** 잘라 돌려주면 체결
// 일부를 못 본 채 "성공"이 되고, 그게 정확히 노출을 과소계상하는 모습이다.
// 우리 signer 로 필터한 매치는 회차당 한 자리 수라 정상 경로에서는 닿지 않는다.
var maxMatchPages = 10

// QuoteTypeBid·QuoteTypeAsk 는 스펙의 QuoteType enum 이다.
// 우리 매수 주문은 언제나 Bid 다.
const (
	QuoteTypeBid = "Bid"
	QuoteTypeAsk = "Ask"
)

// 수수료가 무엇으로 걷혔는지. 스펙의 FeeType enum 이다.
const (
	FeeTypeCollateral = "COLLATERAL"
	FeeTypeShares     = "SHARES"
)

// MatchFill 은 매치 안의 참가자 한 명의 체결이다(스펙 OrderFillData).
//
// 필드 일곱이 스펙상 전부 required 다. 하나라도 없으면 파싱 에러다 —
// 조용한 제로값은 "0주 체결" 이나 "수수료 0" 으로 보이고, 둘 다 우리가
// 사실로 읽어서는 안 되는 값이다.
type MatchFill struct {
	// QuoteType 은 이 참가자 주문의 방향이다("Bid" 매수 / "Ask" 매도).
	QuoteType string
	// Shares 는 이 참가자가 이 매치에서 체결한 주식 수다(amount / 1e18).
	Shares float64
	// PriceUSD 는 체결가다(price / 1e18).
	PriceUSD float64
	// OutcomeOnChainID 는 체결된 outcome 의 CTF 토큰 ID(10진 문자열)다.
	// **이름(`Up`/`Down`)이 아니라 이 값으로 방향을 정한다** — 이름은 표기가
	// 바뀔 수 있지만 토큰 ID 는 회차 메타데이터가 준 값과 같아야 한다.
	OutcomeOnChainID string
	// OutcomeName 은 응답이 준 이름이다. 에러 메시지에만 쓴다.
	OutcomeName string
	// Signer 는 이 주문을 서명한 주소다. 우리 것인지 가르는 값이다.
	Signer string
	// Hash 는 이 참가자 **주문**의 해시다. 멱등 키의 일부다.
	Hash string
	// FeeAmount 는 수수료다. 단위는 FeeType 이 정한다 — COLLATERAL 이면
	// USDT, SHARES 면 주식이다. **여기서 USD 로 환산하지 않는다**: 주식
	// 수수료를 USD 로 바꾸려면 체결가를 곱해야 하고, 그 환산 규칙은 원장의
	// 문제이지 파서의 문제가 아니다.
	FeeAmount float64
	FeeType   string
}

// Match 는 매치 이벤트 하나 = **트랜잭션 하나**다.
type Match struct {
	MarketID        int64
	Taker           MatchFill
	Makers          []MatchFill
	TransactionHash string
	// SettlementID 는 이 매치를 만든 오프체인 정산의 식별자다. 스펙상
	// optional 이라 빈 문자열일 수 있다.
	SettlementID string
	ExecutedAt   time.Time
}

// MatchQuery 는 조회 조건이다.
type MatchQuery struct {
	// MarketID 가 0 보다 크면 그 마켓으로 좁힌다. 회차 하나 = 마켓 하나이므로
	// 이 값이 곧 회차 필터다.
	MarketID int64
	// SignerAddress 가 비어 있지 않으면 그 주소가 참가한 매치만 받는다.
	// **서버가 걸러 주는 것이 중요하다** — 이 마켓의 전체 체결은 5분에 수백
	// 건이라(2026-08-10 실측: 3분에 100건 이상, 커서가 남았다) 클라이언트에서
	// 거르려면 매 폴링마다 여러 페이지를 받아야 한다.
	//
	// 주의: 이 필터를 걸어도 응답은 **트랜잭션 전체**다. 같은 매치에 있는
	// 남의 체결이 함께 온다.
	SignerAddress string
	// ExecutedAfterMS 가 0 보다 크면 그 시각(밀리초 유닉스) **이후**의 매치만
	// 받는다. 스펙상 배타적이다.
	ExecutedAfterMS int64
	// First 는 페이지 크기다. 0 이면 MatchesPageSize.
	First int
}

type matchesPage struct {
	envelopeMeta
	Cursor *string `json:"cursor"`
	Data   []struct {
		Market struct {
			ID *int64 `json:"id"`
		} `json:"market"`
		Taker           matchFillJSON   `json:"taker"`
		Makers          []matchFillJSON `json:"makers"`
		TransactionHash string          `json:"transactionHash"`
		SettlementID    string          `json:"settlementId"`
		ExecutedAt      string          `json:"executedAt"`
	} `json:"data"`
}

type matchFillJSON struct {
	QuoteType string `json:"quoteType"`
	// Amount·Price 는 wei 10진 문자열이다. json.Number 로 받는 이유는
	// 서버가 따옴표를 빼고 숫자로 보내도 깨지지 않게 하기 위해서다.
	Amount  json.Number `json:"amount"`
	Price   json.Number `json:"price"`
	Outcome *struct {
		Name      string `json:"name"`
		OnChainID string `json:"onChainId"`
	} `json:"outcome"`
	Signer string `json:"signer"`
	Hash   string `json:"hash"`
	Fee    *struct {
		Amount json.Number `json:"amount"`
		Type   string      `json:"type"`
	} `json:"fee"`
}

// Matches 는 조건에 맞는 매치를 커서 끝까지 모아 돌려준다.
//
// **정렬은 executedAt DESC 다**(스펙). 이 함수는 순서를 바꾸지 않는다 —
// 호출자가 순서에 의존하지 않게 멱등 키로 다루는 것이 전제다.
func (c *Client) Matches(ctx context.Context, q MatchQuery) ([]Match, error) {
	first := q.First
	if first <= 0 {
		first = MatchesPageSize
	}
	var out []Match
	cursor := ""
	for page := 0; page < maxMatchPages; page++ {
		v := url.Values{}
		v.Set("first", strconv.Itoa(first))
		if q.MarketID > 0 {
			v.Set("marketId", strconv.FormatInt(q.MarketID, 10))
		}
		if q.SignerAddress != "" {
			v.Set("signerAddress", q.SignerAddress)
		}
		if q.ExecutedAfterMS > 0 {
			v.Set("executedAfter", strconv.FormatInt(q.ExecutedAfterMS, 10))
		}
		if cursor != "" {
			v.Set("after", cursor)
		}

		var pg matchesPage
		// Bearer 가 아니라 x-api-key 다 — 스펙의 security 가 ApiKeyAuth 뿐이다.
		if err := c.Get(ctx, matchesPath, v, &pg); err != nil {
			return nil, fmt.Errorf("체결 조회 %d번째 페이지: %w", page, err)
		}
		if err := pg.verdict(); err != nil {
			return nil, fmt.Errorf("체결 조회 %d번째 페이지: %w", page, err)
		}

		for i, row := range pg.Data {
			if row.Market.ID == nil {
				return nil, fmt.Errorf("체결 조회 %d번째 페이지 %d번째 매치: market.id 가 없다 — 어느 마켓의 체결인지 모르는 값을 셀 수 없다", page, i)
			}
			if strings.TrimSpace(row.TransactionHash) == "" {
				return nil, fmt.Errorf("체결 조회 %d번째 페이지 %d번째 매치: transactionHash 가 비었다 — 멱등 키를 만들 수 없다", page, i)
			}
			at, err := time.Parse(time.RFC3339, row.ExecutedAt)
			if err != nil {
				return nil, fmt.Errorf("체결 조회 %d번째 페이지 %d번째 매치: executedAt 을 읽지 못했다 (%q)", page, i, row.ExecutedAt)
			}
			taker, err := toMatchFill(row.Taker)
			if err != nil {
				return nil, fmt.Errorf("체결 조회 %d번째 페이지 %d번째 매치의 taker: %w", page, i, err)
			}
			makers := make([]MatchFill, 0, len(row.Makers))
			for j, mk := range row.Makers {
				f, err := toMatchFill(mk)
				if err != nil {
					return nil, fmt.Errorf("체결 조회 %d번째 페이지 %d번째 매치의 %d번째 maker: %w", page, i, j, err)
				}
				makers = append(makers, f)
			}
			out = append(out, Match{
				MarketID:        *row.Market.ID,
				Taker:           taker,
				Makers:          makers,
				TransactionHash: row.TransactionHash,
				SettlementID:    row.SettlementID,
				ExecutedAt:      at,
			})
		}

		next := ""
		if pg.Cursor != nil {
			next = *pg.Cursor
		}
		if next == "" {
			if len(pg.Data) >= first {
				// 페이지가 가득 찼는데 커서가 없다. 커서 필드 이름이 바뀌면
				// 정확히 이 모습이고, 그대로 두면 체결 일부를 못 본 채
				// 조용히 성공한다 — 노출을 과소계상하는 방향이다.
				return nil, fmt.Errorf("체결 조회 %d번째 페이지: %d건이 가득 찼는데 커서가 없다 — 커서 필드 이름이 바뀐 것으로 보인다", page, len(pg.Data))
			}
			return out, nil
		}
		if next == cursor {
			return nil, fmt.Errorf("체결 조회 %d번째 페이지: 커서가 전진하지 않는다", page)
		}
		cursor = next
	}
	return nil, fmt.Errorf("체결 조회: 페이지 상한 %d 를 넘었다 — 체결 일부를 못 본 채 성공으로 돌려주지 않는다", maxMatchPages)
}

// toMatchFill 은 OrderFillData 한 건을 옮긴다. required 필드가 하나라도
// 없으면 에러다.
func toMatchFill(in matchFillJSON) (MatchFill, error) {
	if in.QuoteType == "" {
		return MatchFill{}, fmt.Errorf("quoteType 이 없다")
	}
	if strings.TrimSpace(in.Signer) == "" {
		return MatchFill{}, fmt.Errorf("signer 가 없다 — 누구의 체결인지 모르는 값을 셀 수 없다")
	}
	if strings.TrimSpace(in.Hash) == "" {
		return MatchFill{}, fmt.Errorf("hash 가 없다 — 멱등 키를 만들 수 없다")
	}
	if in.Outcome == nil || in.Outcome.OnChainID == "" {
		return MatchFill{}, fmt.Errorf("outcome.onChainId 가 없다 — 어느 방향의 체결인지 모른다")
	}
	amtWei, err := requiredNumber(in.Amount, "amount")
	if err != nil {
		return MatchFill{}, err
	}
	priceWei, err := requiredNumber(in.Price, "price")
	if err != nil {
		return MatchFill{}, err
	}
	if amtWei < 0 {
		return MatchFill{}, fmt.Errorf("amount 가 음수다 (%v)", amtWei)
	}
	if priceWei < 0 {
		return MatchFill{}, fmt.Errorf("price 가 음수다 (%v)", priceWei)
	}
	// **수수료는 required 다.** 없는 것을 0 으로 읽으면 비용이 사라지고,
	// 그건 적자를 흑자로 보이게 하는 방향이다(pmmm-go 사고와 같은 방향).
	if in.Fee == nil {
		return MatchFill{}, fmt.Errorf("fee 가 없다 — 수수료를 0 으로 세면 비용이 사라진다")
	}
	if in.Fee.Type == "" {
		return MatchFill{}, fmt.Errorf("fee.type 이 없다 — 무엇으로 걷힌 수수료인지 모른다")
	}
	feeWei, err := requiredNumber(in.Fee.Amount, "fee.amount")
	if err != nil {
		return MatchFill{}, err
	}
	if feeWei < 0 {
		return MatchFill{}, fmt.Errorf("fee.amount 가 음수다 (%v)", feeWei)
	}

	f := MatchFill{
		QuoteType:        in.QuoteType,
		Shares:           amtWei / weiPerUnit,
		PriceUSD:         priceWei / weiPerUnit,
		OutcomeOnChainID: in.Outcome.OnChainID,
		OutcomeName:      in.Outcome.Name,
		Signer:           in.Signer,
		Hash:             in.Hash,
		FeeAmount:        feeWei / weiPerUnit,
		FeeType:          in.Fee.Type,
	}
	// requiredNumber 가 NaN/Inf 를 이미 막지만, 나눗셈 뒤에 한 번 더 본다 —
	// 여기서 나간 값이 그대로 원장과 노출 회계에 들어간다.
	for name, v := range map[string]float64{"amount": f.Shares, "price": f.PriceUSD, "fee.amount": f.FeeAmount} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return MatchFill{}, fmt.Errorf("%s 가 유한하지 않다 (%v)", name, v)
		}
	}
	return f, nil
}
