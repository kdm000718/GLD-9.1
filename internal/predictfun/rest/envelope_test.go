package rest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 이 파일은 **우리가 실제로 전선에 내보내는 JSON** 을 본다.
//
// 2026-08-11 첫 무장에서 주문 182건이 전부 400 으로 거부됐다:
//
//	Expected input type "CreateOrderData", found null.
//
// 스펙의 `CreateOrderRequest` 는 `required: [data]` 이고 그 안이
// `CreateOrderData` 인데, 우리는 CreateOrderData 를 최상위에 그대로 보내고
// 있었다. 취소 경로(`RemoveOrdersRequest`)도 같았다.
//
// **기존 시험이 이것을 놓친 이유가 중요하다.** 시험들은 우리가 만든 map 을
// 검사하거나 응답 파싱을 검사했지 — 그 사이, 즉 http.Client 가 보내는 바이트를
// 검사하지 않았다. 그래서 봉투가 통째로 빠져 있어도 전부 초록이었다.
// 여기서는 서버가 받은 본문을 그대로 읽어서 본다.

func capture(t *testing.T, status int, reply string) (*Client, *[]byte) {
	t.Helper()
	var got []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = b
		w.WriteHeader(status)
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	c := New("k")
	c.BaseURL = srv.URL
	c.SetTokenSource(tokenFunc(func(context.Context) (string, error) { return "tok", nil }))
	return c, &got
}

type tokenFunc func(context.Context) (string, error)

func (f tokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

func TestCreateOrderWrapsBodyInData(t *testing.T) {
	c, got := capture(t, 200, `{"success":true,"data":{"orderId":"o1"}}`)
	inner := map[string]any{"pricePerShare": "470000000000000000", "strategy": "LIMIT"}
	_, _ = c.CreateOrder(context.Background(), inner)

	var sent map[string]json.RawMessage
	if err := json.Unmarshal(*got, &sent); err != nil {
		t.Fatalf("보낸 본문이 JSON 이 아니다: %s", *got)
	}
	raw, ok := sent["data"]
	if !ok {
		t.Fatalf("최상위에 data 가 없다 — 거래소는 400 으로 답한다. 보낸 것: %s", *got)
	}
	var inside map[string]any
	if err := json.Unmarshal(raw, &inside); err != nil {
		t.Fatalf("data 가 객체가 아니다: %s", raw)
	}
	if inside["pricePerShare"] != "470000000000000000" || inside["strategy"] != "LIMIT" {
		t.Fatalf("data 안이 CreateOrderData 가 아니다: %v", inside)
	}
	// 봉투를 두 번 씌우는 것도 막는다 — data.data 는 같은 400 을 부른다.
	if _, doubled := inside["data"]; doubled {
		t.Fatalf("data 를 두 번 씌웠다: %s", *got)
	}
}

func TestRemoveOrdersWrapsIdsInData(t *testing.T) {
	// 취소가 400 이면 살아 있는 주문을 거둘 수 없다. 노출 불변식 전체가
	// 취소가 도달한다는 전제 위에 서 있다.
	c, got := capture(t, 200, `{"success":true,"data":{"removed":["a"],"noop":[],"rejected":[]}}`)
	_, _ = c.RemoveOrders(context.Background(), []string{"a", "b"})

	var sent struct {
		Data *struct {
			IDs []string `json:"ids"`
		} `json:"data"`
		IDs []string `json:"ids"` // 최상위에 있으면 옛 형태다
	}
	if err := json.Unmarshal(*got, &sent); err != nil {
		t.Fatalf("보낸 본문이 JSON 이 아니다: %s", *got)
	}
	if sent.Data == nil {
		t.Fatalf("최상위에 data 가 없다 — 취소가 거부되고 주문이 살아남는다. 보낸 것: %s", *got)
	}
	if len(sent.IDs) != 0 {
		t.Errorf("ids 가 최상위에도 있다(옛 형태가 남았다): %s", *got)
	}
	if len(sent.Data.IDs) != 2 || sent.Data.IDs[0] != "a" || sent.Data.IDs[1] != "b" {
		t.Fatalf("data.ids = %v, want [a b]", sent.Data.IDs)
	}
}
