package ws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// flakyServer는 httptest 기반 WS 서버다. 처음 n개 연결은 수락 직후 바로
// 끊고, 그 뒤로는 유지한다(n == 0이면 항상 유지). 재접속 후 OnConnect가
// 다시 불리는지 보는 회귀 테스트 전용이다.
type flakyServer struct {
	srv     *httptest.Server
	dropN   int64
	dropped int64
}

func newFlakyServer(t *testing.T, n int) *flakyServer {
	t.Helper()
	fs := &flakyServer{dropN: int64(n)}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		if fs.dropN > 0 && atomic.AddInt64(&fs.dropped, 1) <= fs.dropN {
			c.Close(websocket.StatusGoingAway, "flaky: 즉시 끊음")
			return
		}
		// 연결을 유지하며 클라이언트가 보내는 프레임을 읽어 버린다.
		// writeLoop이 구독 프레임을 write 큐에 넣을 수 있어야 하므로
		// 서버 쪽에서 계속 읽어줘야 한다(안 그러면 TCP 버퍼가 찬다).
		ctx := r.Context()
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	return fs
}

func (fs *flakyServer) URL() string {
	return "ws" + fs.srv.URL[len("http"):]
}

func (fs *flakyServer) Close() { fs.srv.Close() }

// 서버가 끊었을 때 재접속하고, OnConnect 가 다시 불려 호출자가 전체 재구독할
// 기회를 얻는지 본다. 서버는 구독 상태를 기억하지 않으므로 이 콜백이 없으면
// 재접속 후 아무 데이터도 오지 않는다 — 에러 없이 조용히.
//
// 횟수만 세지 않고 **넘겨받은 Sender 로 실제 전송이 되는지**까지 본다. 콜백이
// 불리기만 하고 그 자리에서 보낼 수 없으면 재구독은 여전히 불가능하다.
func TestOnConnectCalledAgainAfterDropAndCanSend(t *testing.T) {
	srv := newFlakyServer(t, 1) // 첫 연결을 즉시 끊는다
	defer srv.Close()

	var mu sync.Mutex
	var calls int
	var sendErrs []error
	c := New(Options{
		URL:       srv.URL(),
		UserAgent: "gld91-test",
		OnConnect: func(ctx context.Context, s Sender) error {
			// 토픽 구분자는 슬래시다. 콜론이 아니다 — 원본
			// protocol.go 의 topic() 과 ParseTopic 이 "predictOrderbook/123"
			// 형식을 쓴다(실제 수집 레코드로 확인).
			err := s.Send(ctx, SubscribeRequest(1, "predictOrderbook/1"))
			mu.Lock()
			calls++
			sendErrs = append(sendErrs, err)
			mu.Unlock()
			return nil
		},
		OnFrame: func(Frame) {},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n, errs := calls, append([]error(nil), sendErrs...)
		mu.Unlock()
		if n >= 2 {
			for i, err := range errs {
				if err != nil {
					t.Fatalf("%d번째 OnConnect 에서 재구독 전송이 실패했다: %v", i, err)
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	t.Fatalf("재접속 후 OnConnect 가 %d회만 불렸다 — 최소 2회여야 한다", n)
}

// OnConnect 가 에러를 돌려주면 그 연결을 쓰지 않고 다시 붙어야 한다.
// 재구독이 실패한 연결로 계속 도는 것이 최악이다 — 붙어는 있는데 데이터가 없다.
func TestOnConnectErrorForcesReconnect(t *testing.T) {
	srv := newFlakyServer(t, 0) // 서버는 정상, 실패는 콜백에서 낸다
	defer srv.Close()

	var mu sync.Mutex
	var calls int
	c := New(Options{
		URL:       srv.URL(),
		UserAgent: "gld91-test",
		OnConnect: func(ctx context.Context, s Sender) error {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return errors.New("일부러 실패")
			}
			return nil
		},
		OnFrame: func(Frame) {},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("OnConnect 가 에러를 냈는데 재접속하지 않았다 — 재구독 실패한 연결로 계속 돈다")
}

// waitForConnects는 OnConnect 가 최소 n회 불릴 때까지 기다린다.
func waitForConnects(t *testing.T, mu *sync.Mutex, calls *int, n int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := *calls
		mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	got := *calls
	mu.Unlock()
	t.Fatalf("OnConnect 가 %d회만 불렸다 — %d회를 기다렸다", got, n)
}

// Reconnects()는 **재**접속만 센다. 한 번도 안 끊긴 실행은 0이어야 한다.
//
// 이 테스트가 없어서 카운터가 최초 접속까지 세는 채로 소크 두 번을 돌았다.
// 그 결과 "재접속 1회"가 매번 보고됐고, 호출자들이 -1 보정을 각자 넣어
// 정의가 두 군데로 갈렸다.
func TestReconnectsIsZeroAfterCleanFirstConnect(t *testing.T) {
	srv := newFlakyServer(t, 0) // 끊지 않는 서버
	defer srv.Close()

	var mu sync.Mutex
	var calls int
	c := New(Options{
		URL:       srv.URL(),
		UserAgent: "gld91-test",
		OnConnect: func(ctx context.Context, s Sender) error {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil
		},
		OnFrame: func(Frame) {},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	waitForConnects(t, &mu, &calls, 1)
	if got := c.Reconnects(); got != 0 {
		t.Errorf("끊김 없는 실행에서 Reconnects() = %d, 기대 0", got)
	}
}

// 두 번 끊긴 실행은 정확히 2다 — 3회 접속했지만 최초 1회는 재접속이 아니다.
func TestReconnectsCountsOnlyReconnects(t *testing.T) {
	srv := newFlakyServer(t, 2) // 처음 두 연결을 즉시 끊는다
	defer srv.Close()

	var mu sync.Mutex
	var calls int
	c := New(Options{
		URL:       srv.URL(),
		UserAgent: "gld91-test",
		OnConnect: func(ctx context.Context, s Sender) error {
			mu.Lock()
			calls++
			mu.Unlock()
			return nil
		},
		OnFrame: func(Frame) {},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go c.Run(ctx)

	waitForConnects(t, &mu, &calls, 3)
	if got := c.Reconnects(); got != 2 {
		t.Errorf("두 번 끊긴 실행에서 Reconnects() = %d, 기대 2", got)
	}
}

// 한 번도 접속하지 못한 클라이언트는 0이다 — connects-1 이 음수로 새지 않는다.
func TestReconnectsBeforeAnyConnectIsZero(t *testing.T) {
	c := New(Options{URL: "ws://127.0.0.1:1", OnFrame: func(Frame) {}})
	if got := c.Reconnects(); got != 0 {
		t.Errorf("접속 전 Reconnects() = %d, 기대 0", got)
	}
}
