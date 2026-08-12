package claim

// testdata/claimable_response.json 은 HAR 에 담긴 **실제**
// GetAccountClaimablePositions 응답이다. 스키마를 손으로 옮겨 적은 것이
// 아니라 서버가 준 것을 그대로 둔 것이므로, 필드 이름을 잘못 짐작했으면
// 여기서 드러난다 — 짐작한 이름은 조용히 빈 값이 되고 그것은 "회수할 것이
// 없다"라는 거짓말이 된다(rest.Position 이 같은 이유로 필드를 넷으로 줄였다).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func graphQLServing(t *testing.T, status int, body string) GraphQL {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Origin"); got != "https://predict.fun" {
			t.Errorf("Origin 헤더가 %q 다 — HAR 의 실제 요청과 다르다", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return GraphQL{Endpoint: srv.URL, HTTP: srv.Client()}
}

// TestClaimableParsesRealResponse 는 실물 응답에서 회수에 필요한 값이 전부
// 나오는지 본다. conditionId 가 이 경로의 존재 이유다 — REST 는 주지 않는다.
func TestClaimableParsesRealResponse(t *testing.T) {
	body, err := os.ReadFile("testdata/claimable_response.json")
	if err != nil {
		t.Fatalf("응답 표본 읽기: %v", err)
	}
	g := graphQLServing(t, http.StatusOK, string(body))

	pos, err := g.Claimable(context.Background(), testAccount)
	if err != nil {
		t.Fatalf("Claimable: %v", err)
	}
	if len(pos) != 2 {
		t.Fatalf("포지션 %d개, 2개여야 한다: %+v", len(pos), pos)
	}

	// 순서: 이긴 것(claimable) 먼저, 진 것(clearable) 나중. 골든 벡터의
	// indexSets 순서([2] 다음 [1])가 이 순서에서 나온다.
	won, lost := pos[0], pos[1]
	if !won.Won || lost.Won {
		t.Fatalf("순서가 claimable→clearable 이 아니다: [0].Won=%v [1].Won=%v", won.Won, lost.Won)
	}
	if won.OutcomeIndex != 2 || won.OutcomeName != "Down" {
		t.Errorf("이긴 결과가 index=%d name=%q 다, 2/Down 이어야 한다", won.OutcomeIndex, won.OutcomeName)
	}
	if lost.OutcomeIndex != 1 || lost.OutcomeName != "Up" {
		t.Errorf("진 결과가 index=%d name=%q 다, 1/Up 이어야 한다", lost.OutcomeIndex, lost.OutcomeName)
	}

	const wantCond = "0xbb499dbafe8564165e3fc10c4b156194f40947cd9060e04bc755fa7b7aa16f30"
	for i, p := range pos {
		if p.ConditionID != wantCond {
			t.Errorf("%d번째 conditionId %q, want %q", i, p.ConditionID, wantCond)
		}
		if p.CategoryID != "btc-updown-5m-1786471200" {
			t.Errorf("%d번째 categoryId %q", i, p.CategoryID)
		}
		if p.IsNegRisk || p.IsYieldBearing {
			t.Errorf("%d번째 시장 변종 플래그가 참이다", i)
		}
		if why := p.Blocker(); why != "" {
			t.Errorf("%d번째가 막혔다: %s", i, why)
		}
	}
	if got := won.Shares; got < 1.97 || got > 1.9706 {
		t.Errorf("이긴 주식 %.9f, 약 1.9705686 이어야 한다", got)
	}
	if got := lost.Shares; got < 3.95 || got > 3.97 {
		t.Errorf("진 주식 %.9f, 약 3.96 이어야 한다", got)
	}
}

// TestClaimableFailsClosedOnMissingAccount 는 응답에 account 가 없을 때
// **빈 목록으로 접지 않는지** 본다. 접으면 회수가 조용히 멈추고, 그 침묵은
// "회수할 것이 없다"와 구분되지 않는다.
func TestClaimableFailsClosedOnMissingAccount(t *testing.T) {
	for name, body := range map[string]string{
		"data 없음":    `{}`,
		"account 널":  `{"data":{"account":null}}`,
		"GraphQL 오류": `{"errors":[{"message":"rate limited"}]}`,
		"JSON 아님":    `<html>502</html>`,
	} {
		t.Run(name, func(t *testing.T) {
			g := graphQLServing(t, http.StatusOK, body)
			if _, err := g.Claimable(context.Background(), testAccount); err == nil {
				t.Fatal("에러가 아니라 빈 목록으로 접혔다")
			}
		})
	}
}

// TestClaimableFailsClosedOnBrokenNode 는 항목에 빠진 필드가 있으면 그 항목을
// 조용히 건너뛰지 않고 실패하는지 본다. 건너뛰면 회수해야 할 돈이 남는다.
func TestClaimableFailsClosedOnBrokenNode(t *testing.T) {
	for name, body := range map[string]string{
		"market 없음":  `{"data":{"account":{"claimablePositions":{"edges":[{"node":{"shares":"1","outcome":{"index":2,"name":"Down"}}}]},"clearablePositions":{"edges":[]}}}}`,
		"outcome 없음": `{"data":{"account":{"claimablePositions":{"edges":[{"node":{"shares":"1","market":{"id":"1","conditionId":"0x00","category":{"id":"x"}}}}]},"clearablePositions":{"edges":[]}}}}`,
		"shares 없음":  `{"data":{"account":{"claimablePositions":{"edges":[{"node":{"outcome":{"index":2,"name":"Down"},"market":{"id":"1","conditionId":"0x00","category":{"id":"x"}}}}]},"clearablePositions":{"edges":[]}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			g := graphQLServing(t, http.StatusOK, body)
			if _, err := g.Claimable(context.Background(), testAccount); err == nil {
				t.Fatal("깨진 항목이 통과했다")
			}
		})
	}
}

// TestClaimableEmptyIsNotAnError 는 회수할 것이 정말 없을 때는 실패가 아닌지
// 본다 — 정상 경로의 대부분이 이것이다.
func TestClaimableEmptyIsNotAnError(t *testing.T) {
	g := graphQLServing(t, http.StatusOK,
		`{"data":{"account":{"claimablePositions":{"edges":[]},"clearablePositions":{"edges":[]}}}}`)
	pos, err := g.Claimable(context.Background(), testAccount)
	if err != nil {
		t.Fatalf("Claimable: %v", err)
	}
	if len(pos) != 0 {
		t.Fatalf("포지션 %d개, 0개여야 한다", len(pos))
	}
}

// TestClaimableRejectsBadAddress 는 주소가 아닌 값으로 조회하지 않는지 본다.
func TestClaimableRejectsBadAddress(t *testing.T) {
	g := graphQLServing(t, http.StatusOK, `{"data":{"account":null}}`)
	if _, err := g.Claimable(context.Background(), "not-an-address"); err == nil {
		t.Fatal("잘못된 주소가 통과했다")
	}
}

// TestClaimableFailsOnHTTPError 는 HTTP 오류를 빈 목록으로 접지 않는지 본다.
func TestClaimableFailsOnHTTPError(t *testing.T) {
	g := graphQLServing(t, http.StatusInternalServerError, `{"data":{"account":null}}`)
	if _, err := g.Claimable(context.Background(), testAccount); err == nil {
		t.Fatal("HTTP 500 이 통과했다")
	}
}

// TestRedactHidesProjectID 는 ZERODEV_RPC 를 로그에 통째로 찍지 않는지 본다 —
// URL 의 프로젝트 ID 가 곧 인증이다.
//
// **프로젝트 ID 는 지어낸 값이다.** 진짜 값을 픽스처로 박으면 "이 값을 로그에
// 남기지 말자"는 시험이 그 값을 소스에 영구히 남긴다. 이 저장소는 GitHub 에
// 올라가고, 인증은 URL 하나면 끝난다. 모양만 같으면 Redact 를 시험하는 데
// 충분하다.
func TestRedactHidesProjectID(t *testing.T) {
	const rpc = "https://rpc.zerodev.app/api/v3/00000000-1111-2222-3333-444444444444/chain/56?provider=ULTRA_RELAY"
	got := Redact(rpc)
	if got == rpc {
		t.Fatal("원문을 그대로 돌려줬다")
	}
	for _, secret := range []string{"00000000", "444444444444"} {
		if contains(got, secret) {
			t.Errorf("가린 값에 프로젝트 ID 조각 %q 가 남아 있다: %s", secret, got)
		}
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
