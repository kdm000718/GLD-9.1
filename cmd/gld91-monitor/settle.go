package main

// 이 파일은 **정산 결과를 관측한다.**
//
// # 왜 모니터가 이걸 하는가
//
// 봇은 자기 손익을 모른다. `internal/exec` 문서가 그 이유를 적어 두었다 —
// `ledger.Settlement.Won` 은 거래소 정산 결과에서만 와야 하고, 바이낸스 봉으로
// 계산하면 안 된다(G2 가 잰 정산 불일치 `d≈0.30%` 가 정확히 그 둘의 차이다).
// 그 조회 경로가 봇에 없어서 원장에는 체결 행만 쌓인다.
//
// 모니터는 **거래 결정을 하지 않으므로** 같은 정보를 알아도 오염이 없다.
// 그리고 그 값이 봇으로 돌아가지 않는다 — beat 응답에는 명령만 실린다
// (`beat.Reply` 의 리플렉션 테스트가 그것을 고정한다).
//
// # 정산 결과는 어디에 있나
//
// `GET /v1/categories?status=RESOLVED` 의 `markets[].resolution` 이다.
// **거래소 자신의 답**이고, `status:"WON"` 인 outcome 이 승자다.
//
// 스펙에 이름 충돌 함정이 있다: `components.schemas` 의 `"Resolution"` 은
// 정산과 무관한 **시계열 버킷 크기**(`1m`/`5m`/`1h`/…) enum 이다. 이름으로
// 찾으면 그것이 먼저 나오고, 그것만 보면 "정산 결과를 주는 것이 없다" 는
// 결론에 도달한다. 스키마 이름이 아니라 실물 응답을 봐야 한다.
//
// # 예산
//
// 레이트리밋 240 req/min 은 API 키 단위다. 봇과 키를 공유하면(SharedKey)
// 이 조회가 봇의 예산을 먹는다. 그래서 페이지 상한을 걸고, 봇이 beat 로
// 보고한 남은 예산이 낮으면 그 주기를 통째로 건너뛴다.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

const (
	// settlePoll 은 정산 조회 주기다. 회차가 5분이므로 그보다 촘촘할 이유가
	// 없다. 정산이 조금 늦어도 다음 주기에 잡힌다.
	settlePoll = 2 * time.Minute
	// settleMaxPages 는 커서 페이지 상한이다. **무제한 페이지네이션이 스펙에
	// 기록된 그 사고의 원인이었다** — 창이 열린 뒤 14초 만에 240 을 소진시켰다.
	//
	// 한 페이지 50건이면 5분 회차 기준 약 4시간이다. 미정산 회차는 길어야
	// 몇 개이므로 정상 경로에서는 1페이지에서 끝난다.
	settleMaxPages = 3
	// settlePageGap 은 페이지 사이 간격이다. **연속으로 치지 않는다** —
	// 상한이 낮아도 등이 붙은 요청은 순간 버스트이고, 그 순간 봇의 재호가가
	// 밀린다. 예산은 분당으로 재지만 봇이 느끼는 것은 그 순간의 지연이다.
	settlePageGap = 3 * time.Second
	// settlePageSize 는 한 페이지 항목 수다.
	settlePageSize = 50
	// budgetFloor 는 이 아래로 떨어지면 조회를 건너뛰는 남은 예산이다.
	// 봇이 재호가를 계속해야 하고, 감시가 그것을 굶기면 안 된다.
	budgetFloor = 60
)

// settlement 는 한 회차의 정산 결과다.
type settlement struct {
	Slug string
	// WonName 은 이긴 outcome 이름("Up"/"Down")이다.
	WonName string
	// WonIndexSet 은 그 outcome 의 indexSet 이다. 이름보다 이쪽으로 비교하는
	// 편이 안전하다 — 대소문자·표기 변화에 흔들리지 않는다.
	WonIndexSet int
	SettledAt   time.Time
	// StartPrice·EndPrice 는 거래소가 기록한 Chainlink 가격이다. 정산 방향과
	// 별개로 남긴다 — G2 가 잰 불일치를 실거래에서 다시 재는 근거다.
	StartPrice float64
	EndPrice   float64
}

// resolvedPage 는 `/v1/categories?status=RESOLVED` 응답 중 우리가 쓰는 부분이다.
//
// `internal/predictfun/rest` 의 파서는 Chainlink 가격만 뽑도록 좁혀져 있어
// `markets[].resolution` 이 없다. `internal/live` 가 같은 이유로 자기 타입을
// 둔 것과 같은 선택이다 — 남의 파서를 넓히면 그쪽 용도가 흔들린다.
type resolvedPage struct {
	Success bool    `json:"success"`
	Cursor  *string `json:"cursor"`
	Data    []struct {
		Slug        string `json:"slug"`
		Status      string `json:"status"`
		VariantData struct {
			StartPrice json.Number `json:"startPrice"`
			EndPrice   json.Number `json:"endPrice"`
			Symbol     string      `json:"priceFeedSymbol"`
		} `json:"variantData"`
		Markets []struct {
			Resolution *struct {
				IndexSet int    `json:"indexSet"`
				Name     string `json:"name"`
				Status   string `json:"status"`
			} `json:"resolution"`
		} `json:"markets"`
	} `json:"data"`
}

// fetchSettlements 는 최근 정산 회차를 모은다.
//
// 정렬은 `PUBLISHED_AT_DESC` 다 — 과거로 거슬러 가는 용도이기 때문이다.
// `ASC` 로 뒤집으면 9년 전부터 훑는다.
func fetchSettlements(ctx context.Context, c *rest.Client, symbolPrefix string, sinceUnix int64) ([]settlement, error) {
	var out []settlement
	seen := map[string]bool{}
	cursor := ""

	for page := 0; page < settleMaxPages; page++ {
		// 첫 페이지가 아니면 쉬었다 간다. 연속 요청은 버스트다.
		if page > 0 && !sleepCtx(ctx, settlePageGap) {
			return out, ctx.Err()
		}
		q := url.Values{}
		q.Set("status", "RESOLVED")
		q.Set("sort", "PUBLISHED_AT_DESC")
		q.Set("limit", fmt.Sprint(settlePageSize))
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var pg resolvedPage
		if err := c.Get(ctx, "/v1/categories", q, &pg); err != nil {
			return out, fmt.Errorf("정산 조회: %w", err)
		}

		reachedOld := false
		for _, d := range pg.Data {
			if !strings.HasPrefix(d.Slug, symbolPrefix) {
				continue
			}
			start, ok := rest.ParseSlugStart(d.Slug)
			if !ok {
				continue // 5분 상품이 아니다
			}
			if start < sinceUnix {
				reachedOld = true
				continue
			}
			if seen[d.Slug] {
				continue // 커서가 항목을 겹쳐 줄 수 있다
			}
			s, ok := settlementFrom(d.Slug, start, d.Markets, d.VariantData.StartPrice, d.VariantData.EndPrice)
			if !ok {
				continue // 아직 resolution 이 없다(정산 지연)
			}
			seen[d.Slug] = true
			out = append(out, s)
		}
		if reachedOld || pg.Cursor == nil || *pg.Cursor == "" || *pg.Cursor == cursor {
			break
		}
		cursor = *pg.Cursor
	}
	return out, nil
}

// settlementFrom 은 한 회차의 `markets[].resolution` 에서 승자를 고른다.
//
// **`status:"WON"` 인 것만 승자다.** 이름이나 indexSet 만 보고 고르면 진 쪽을
// 승자로 읽는다 — 두 outcome 이 모두 resolution 객체를 갖고 하나만 WON 이다.
func settlementFrom(slug string, startUnix int64, markets []struct {
	Resolution *struct {
		IndexSet int    `json:"indexSet"`
		Name     string `json:"name"`
		Status   string `json:"status"`
	} `json:"resolution"`
}, startPrice, endPrice json.Number) (settlement, bool) {
	s := settlement{Slug: slug, SettledAt: time.Unix(startUnix+300, 0).UTC()}
	s.StartPrice, _ = startPrice.Float64()
	s.EndPrice, _ = endPrice.Float64()

	found := false
	for _, m := range markets {
		r := m.Resolution
		if r == nil || !strings.EqualFold(r.Status, "WON") {
			continue
		}
		if found {
			// 승자가 둘이면 우리 해석이 틀린 것이다. 틀린 값을 쓰느니 이
			// 회차를 버린다 — 조용히 하나를 고르면 손익이 절반쯤 틀린다.
			return settlement{}, false
		}
		s.WonName, s.WonIndexSet, found = r.Name, r.IndexSet, true
	}
	return s, found
}

// runSettleWatcher 는 정산을 주기적으로 관측해 상태에 채운다.
func runSettleWatcher(ctx context.Context, st *state, c *rest.Client, shared bool) {
	if c == nil {
		log.Printf("⚠️ 정산 관측 꺼짐 — API 키가 없다 (%s 또는 %s)", EnvMonAPIKey, EnvBotAPIKey)
		return
	}
	if shared {
		log.Printf("⚠️ 정산 관측이 봇과 **같은 API 키**를 쓴다 — 레이트리밋 240/분을 나눠 쓴다. "+
			"페이지 상한 %d, 남은 예산 %d 미만이면 그 주기를 건너뛴다", settleMaxPages, budgetFloor)
	}
	t := time.NewTicker(settlePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// **봇의 예산이 빠듯하면 물러난다.** 감시가 감시 대상을 굶기면
			// 안 된다. 봇이 beat 로 남은 예산을 알려 주므로 공짜로 판단한다.
			if shared {
				if snap, _ := st.Latest(); snap != nil && snap.Loop.RateLimitRemaining < budgetFloor {
					log.Printf("정산 조회 건너뜀 — 봇의 남은 예산 %d < %d", snap.Loop.RateLimitRemaining, budgetFloor)
					continue
				}
			}
			// **맞출 대상이 없으면 아예 조회하지 않는다.**
			//
			// ApplySettlements 는 우리가 건 회차만 맞춘다. 캐시가 비어 있으면
			// 무엇을 받아 오든 버려지므로, 그 조회는 봇의 예산만 먹는다.
			// 기동 직후가 정확히 그 상태다.
			since, want := st.OldestUnsettled()
			if want == 0 {
				continue
			}
			ss, err := fetchSettlements(ctx, c, "btc", since)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("정산 조회 실패 — 이 주기를 건너뛴다: %v", err)
				continue
			}
			// **반영 결과를 남긴다.** 성공이 조용하면 정산 경로가 실제로
			// 도는지 확인할 방법이 없다 — 알람 로깅 때와 같은 이유다.
			applied := st.ApplySettlements(ss)
			log.Printf("정산 조회: 미정산 %d건 대기, 받은 정산 %d건, 반영 %d건", want, len(ss), applied)
		}
	}
}
