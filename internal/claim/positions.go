package claim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GraphQLEndpoint 는 predict.fun 의 GraphQL 이다.
//
// **인증이 없다.** HAR 의 요청에는 Authorization 도 쿠키도 없고, 주소만으로
// 조회된다(2026-08-12 실측 재확인). 그래서 이 조회는 REST 키의 레이트리밋
// 예산(240 req/min)을 **먹지 않는다** — 봇과 키를 공유해도 재호가가 밀리지
// 않는다는 뜻이고, 이것이 REST `/v1/positions` 대신 여기를 쓰는 이유 중 하나다.
const GraphQLEndpoint = "https://graphql.predict.fun/graphql"

// 나머지 이유가 더 중요하다: **REST 는 conditionId 를 주지 않는다.**
// `rest.Position` 은 amount/valueUsd/averageBuyPriceUsd/pnlUsd 넷뿐이고,
// redeemPositions 에 넣을 conditionId 가 없다. 포지션 토큰 ID 에서 역산하는
// 것은 불가능하다(keccak 은 단방향). 명세가 "유일하게 미확정인 값" 이라고
// 적은 것이 이것인데, HAR 이 답을 갖고 있었다 — 웹은 GraphQL 의
// `market.conditionId` 를 그대로 쓴다.

const graphQLTimeout = 20 * time.Second

// claimablePositionsQuery 는 HAR 의 GetAccountClaimablePositions 에서 우리가
// 쓰는 필드만 남긴 것이다.
//
// claimable 과 clearable 을 **둘 다** 받는다. claimable 은 이긴 주식(주당 $1),
// clearable 은 진 주식(주당 $0)이다. 진 주식도 회수하는 이유는 실물이 그렇게
// 했기 때문이다 — HAR 의 UserOperation 은 두 건을 한 배치에 묶었다. 회수하지
// 않으면 지갑에 휴지 토큰이 남아 다음 회차의 포지션 조회를 어지럽힌다.
const claimablePositionsQuery = `query GetAccountClaimablePositions($address: Address!) {
  account(address: $address) {
    claimablePositions { edges { node { ...P } } }
    clearablePositions { edges { node { ...P } } }
  }
}

fragment P on Position {
  shares
  outcome { index name }
  market {
    id
    conditionId
    title
    category { id isNegRisk isYieldBearing }
  }
}`

// Position 은 회수 대상 한 건이다.
//
// rest.Position 과 이름이 같지만 다른 것이다 — 이쪽은 온체인 회수에 필요한
// 식별자(conditionId·결과 인덱스)를 담고, 저쪽은 사이징에 필요한 금액을 담는다.
type Position struct {
	MarketID    string
	ConditionID string
	Title       string
	// CategoryID 는 `btc-updown-5m-<유닉스초>` 형태다. live.ParseRoundStart 가
	// 여기서 회차 시작 시각을 뽑는다.
	CategoryID   string
	OutcomeIndex int
	OutcomeName  string
	// SharesWei 는 응답 그대로의 주식 수(10진 문자열, 18 decimals)다.
	SharesWei string
	// Shares 는 SharesWei / 1e18 이다.
	Shares float64
	// Won 은 이 포지션이 이겼는가다. claimable=true, clearable=false.
	Won bool
	// IsNegRisk·IsYieldBearing 은 시장 변종이다. 우리에겐 이 둘이 모두 false 인
	// 경로의 골든 벡터밖에 없다 — [Position.Blocker] 참조.
	IsNegRisk      bool
	IsYieldBearing bool
}

// Blocker 는 이 포지션을 회수하면 안 되는 이유다. 빈 문자열이면 회수 가능.
//
// **에러가 아니라 문자열이다.** internal/onchain 이 같은 선택을 한 이유와
// 같다 — 에러로 돌려주면 호출자가 언젠가 그것을 "일시적 장애니 통과"로
// 다루게 된다. 여기서 막힌 것은 통과시킬 방법이 없어야 한다.
func (p Position) Blocker() string {
	switch {
	case p.ConditionID == "":
		return "conditionId 가 비었다"
	case p.OutcomeIndex < 1 || p.OutcomeIndex > 8:
		return fmt.Sprintf("결과 인덱스가 범위 밖이다 (%d)", p.OutcomeIndex)
	case p.SharesWei == "" || p.SharesWei == "0":
		return "보유 주식이 0 이다"
	case p.IsNegRisk:
		// 골든 벡터가 없는 경로다. negRisk 시장은 CTF 가 아니라 어댑터를
		// 거치므로 대상 주소도 calldata 도 다르다. 확인하지 않은 것을 보내는
		// 것이 이 저장소의 실패 방식이었다.
		return "negRisk 시장이다 — 이 경로의 실물 대조 기준이 없다"
	case p.IsYieldBearing:
		return "yieldBearing 시장이다 — 이 경로의 실물 대조 기준이 없다"
	}
	if _, err := normalizeWord(p.ConditionID); err != nil {
		return "conditionId 가 32바이트 hex 가 아니다: " + err.Error()
	}
	return ""
}

// PositionSource 는 회수 대상 조회다. 시험이 갈아끼운다.
type PositionSource interface {
	Claimable(ctx context.Context, address string) ([]Position, error)
}

// GraphQL 은 predict.fun GraphQL 로 회수 대상을 읽는다.
type GraphQL struct {
	// Endpoint 는 비면 GraphQLEndpoint.
	Endpoint string
	// HTTP 는 시험이 주입한다.
	HTTP *http.Client
}

// Claimable 은 회수 대상을 **실물이 보낸 순서 그대로** 돌려준다:
// 이긴 것(claimable) 먼저, 진 것(clearable) 나중.
//
// 순서를 지키는 이유는 골든 벡터가 그 순서였기 때문이다(indexSets [2] 다음
// [1]). 회수 결과는 순서에 의존하지 않지만, 순서를 바꾸면 우리가 만드는
// callData 가 실물과 달라져 **대조할 기준을 잃는다.**
func (g GraphQL) Claimable(ctx context.Context, address string) ([]Position, error) {
	if _, err := normalizeAddress(address); err != nil {
		return nil, fmt.Errorf("계정 주소: %w", err)
	}

	type node struct {
		Shares  string `json:"shares"`
		Outcome *struct {
			Index int    `json:"index"`
			Name  string `json:"name"`
		} `json:"outcome"`
		Market *struct {
			ID          string `json:"id"`
			ConditionID string `json:"conditionId"`
			Title       string `json:"title"`
			Category    *struct {
				ID             string `json:"id"`
				IsNegRisk      bool   `json:"isNegRisk"`
				IsYieldBearing bool   `json:"isYieldBearing"`
			} `json:"category"`
		} `json:"market"`
	}
	type edges struct {
		Edges []struct {
			Node node `json:"node"`
		} `json:"edges"`
	}
	var out struct {
		Data *struct {
			Account *struct {
				Claimable edges `json:"claimablePositions"`
				Clearable edges `json:"clearablePositions"`
			} `json:"account"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := g.post(ctx, map[string]any{
		"query":         claimablePositionsQuery,
		"operationName": "GetAccountClaimablePositions",
		"variables":     map[string]any{"address": address},
	}, &out); err != nil {
		return nil, err
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("GraphQL 오류: %s", clip(out.Errors[0].Message, 300))
	}
	if out.Data == nil || out.Data.Account == nil {
		// **빈 목록으로 접지 않는다.** account 가 nil 인 것은 "회수할 것이
		// 없다"가 아니라 "응답이 우리가 아는 모양이 아니다"이고, 그것을
		// 0건으로 읽으면 회수를 조용히 건너뛴다.
		return nil, fmt.Errorf("응답에 account 가 없다 — 스키마가 바뀌었는지 확인하라")
	}

	var pos []Position
	for _, group := range []struct {
		won bool
		e   edges
	}{{true, out.Data.Account.Claimable}, {false, out.Data.Account.Clearable}} {
		for i, ed := range group.e.Edges {
			n := ed.Node
			if n.Market == nil || n.Outcome == nil || n.Market.Category == nil {
				return nil, fmt.Errorf("%s %d번째 항목에 market·outcome·category 중 빠진 것이 있다", groupName(group.won), i)
			}
			shares, err := sharesFromWei(n.Shares)
			if err != nil {
				return nil, fmt.Errorf("%s %d번째 항목의 shares: %w", groupName(group.won), i, err)
			}
			pos = append(pos, Position{
				MarketID:       n.Market.ID,
				ConditionID:    strings.ToLower(n.Market.ConditionID),
				Title:          n.Market.Title,
				CategoryID:     n.Market.Category.ID,
				OutcomeIndex:   n.Outcome.Index,
				OutcomeName:    n.Outcome.Name,
				SharesWei:      n.Shares,
				Shares:         shares,
				Won:            group.won,
				IsNegRisk:      n.Market.Category.IsNegRisk,
				IsYieldBearing: n.Market.Category.IsYieldBearing,
			})
		}
	}
	return pos, nil
}

func groupName(won bool) string {
	if won {
		return "claimablePositions"
	}
	return "clearablePositions"
}

// sharesFromWei 는 18 decimals 문자열을 float 으로 바꾼다. 정밀도는 표시·원장용
// 이고 온체인 회수에는 쓰이지 않는다 — redeemPositions 는 수량을 받지 않고
// 보유분 전부를 회수한다.
func sharesFromWei(s string) (float64, error) {
	if strings.TrimSpace(s) == "" {
		return 0, fmt.Errorf("비었다")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("정수가 아니다: %q", s)
	}
	return f / 1e18, nil
}

func (g GraphQL) post(ctx context.Context, body any, out any) error {
	ep := g.Endpoint
	if ep == "" {
		ep = GraphQLEndpoint
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("GraphQL 요청 인코딩: %w", err)
	}
	client := g.HTTP
	if client == nil {
		client = &http.Client{Timeout: graphQLTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("GraphQL 요청 생성: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/graphql-response+json, application/json")
	// Origin 을 붙인다 — HAR 의 실제 요청이 그랬다. 없어도 응답이 오지만,
	// 서버가 언제든 이것으로 거를 수 있고 그때의 실패는 "포지션이 없다"로
	// 보인다.
	req.Header.Set("Origin", "https://predict.fun")
	req.Header.Set("Referer", "https://predict.fun/")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("GraphQL 요청: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("GraphQL 응답 읽기: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GraphQL HTTP %d: %s", resp.StatusCode, clip(string(b), 300))
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("GraphQL 응답이 JSON 이 아니다: %s", clip(string(b), 300))
	}
	return nil
}
