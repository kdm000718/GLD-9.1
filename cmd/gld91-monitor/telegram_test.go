package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// tgHarness 는 텔레그램 API 를 흉내낸다.
type tgHarness struct {
	mu   sync.Mutex
	sent []struct {
		chat int64
		text string
	}
	updates []tgUpdate
	srv     *httptest.Server
	client  *tgClient
}

func newTGHarness(t *testing.T, chatID int64) *tgHarness {
	t.Helper()
	h := &tgHarness{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			var body struct {
				ChatID int64  `json:"chat_id"`
				Text   string `json:"text"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			h.mu.Lock()
			h.sent = append(h.sent, struct {
				chat int64
				text string
			}{body.ChatID, body.Text})
			h.mu.Unlock()
			fmt.Fprint(w, `{"ok":true}`)

		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			h.mu.Lock()
			us := h.updates
			h.updates = nil
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": us})

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.srv.Close)

	h.client = newTG("token", chatID)
	h.client.base = h.srv.URL
	h.client.http = &http.Client{Timeout: 2 * time.Second}
	return h
}

func (h *tgHarness) push(chatID int64, id int64, text string, when time.Time) {
	u := tgUpdate{UpdateID: id}
	u.Message = &struct {
		Date int64  `json:"date"`
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	}{Date: when.Unix(), Text: text}
	u.Message.Chat.ID = chatID
	h.mu.Lock()
	h.updates = append(h.updates, u)
	h.mu.Unlock()
}

func (h *tgHarness) sentTexts() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.sent))
	for _, s := range h.sent {
		out = append(out, s.text)
	}
	return out
}

func (h *tgHarness) waitSent(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		got := len(h.sent)
		h.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("전송 %d건을 기다리다 시한 초과 (지금 %v)", n, h.sentTexts())
}

// --- 테스트 ---

func TestSendReachesAuthorizedChat(t *testing.T) {
	h := newTGHarness(t, 42)
	h.client.Send("안녕")
	h.waitSent(t, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sent[0].chat != 42 || h.sent[0].text != "안녕" {
		t.Errorf("전송 = %+v", h.sent[0])
	}
}

// 전송 실패가 모니터를 죽이면 그 뒤의 모든 알림도 사라진다 — 감시 장치가
// 자기 실패로 감시를 멈추는 형태다.
func TestSendSwallowsFailure(t *testing.T) {
	c := newTG("token", 42)
	c.base = "http://127.0.0.1:1" // 연결 거부
	c.http = &http.Client{Timeout: 200 * time.Millisecond}
	c.Send("아무거나") // 패닉하지 않는다
}

// **인가되지 않은 채팅은 거부한다.** 봇 토큰을 아는 누구나 말을 걸 수 있다.
func TestUnauthorizedChatRejected(t *testing.T) {
	h := newTGHarness(t, 42)
	st := stateWith(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.push(999, 1, "/shutdown", time.Now().Add(time.Second))
	go h.client.Poll(ctx, st)
	h.waitSent(t, 1)

	if got := h.sentTexts()[0]; !strings.Contains(got, "인가되지 않은") {
		t.Errorf("응답 = %q", got)
	}
	h.mu.Lock()
	chat := h.sent[0].chat
	h.mu.Unlock()
	if chat != 999 {
		t.Errorf("거부 응답이 %d 로 갔다", chat)
	}
	// **명령이 실행되면 안 된다.** 이게 이 테스트의 핵심이다.
	if p := st.Pending(); p != "none" && p != "" {
		t.Errorf("인가되지 않은 /shutdown 이 실행됐다: pending = %q", p)
	}
}

// 기동 이전 메시지를 무시한다. 모니터 재시작은 대개 문제를 고친 직후인데,
// 그때 밀려 있던 /shutdown 이 실행되면 방금 고친 봇이 다시 멈춘다.
func TestPreStartupMessagesIgnored(t *testing.T) {
	h := newTGHarness(t, 42)
	st := stateWith(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.push(42, 1, "/shutdown", time.Now().Add(-time.Hour)) // 기동 전
	h.push(42, 2, "/help", time.Now().Add(time.Second))    // 기동 후
	go h.client.Poll(ctx, st)
	h.waitSent(t, 1)

	if p := st.Pending(); p != "none" && p != "" {
		t.Errorf("기동 이전 /shutdown 이 실행됐다: pending = %q", p)
	}
	if got := h.sentTexts()[0]; !strings.Contains(got, "/status") {
		t.Errorf("기동 이후 /help 가 처리되지 않았다: %q", got)
	}
}

// 인가된 명령은 실행되고 답이 온다.
func TestAuthorizedCommandExecutes(t *testing.T) {
	h := newTGHarness(t, 42)
	st := stateWith(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.push(42, 1, "/shutdown", time.Now().Add(time.Second))
	go h.client.Poll(ctx, st)
	h.waitSent(t, 1)

	if st.Pending() != "shutdown" {
		t.Errorf("pending = %q, want shutdown", st.Pending())
	}
	if got := h.sentTexts()[0]; !strings.Contains(got, "종료 요청") {
		t.Errorf("응답 = %q", got)
	}
}

// 모르는 명령에는 도움말이 간다 — 침묵하면 사용자는 봇이 죽었는지 명령이
// 틀렸는지 구분할 수 없다.
func TestUnknownCommandGetsHelp(t *testing.T) {
	h := newTGHarness(t, 42)
	st := stateWith(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.push(42, 1, "/nonsense", time.Now().Add(time.Second))
	go h.client.Poll(ctx, st)
	h.waitSent(t, 1)

	if got := h.sentTexts()[0]; got != helpText {
		t.Errorf("응답 = %q, want 도움말", got)
	}
}

// ctx 가 끝나면 Poll 이 돌아온다 — 종료가 걸리지 않으면 프로세스가 안 죽는다.
func TestPollStopsOnContextCancel(t *testing.T) {
	h := newTGHarness(t, 42)
	st := stateWith(t, nil)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { h.client.Poll(ctx, st); close(done) }()
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 취소에도 Poll 이 돌아오지 않는다")
	}
}

// **폴 루프는 명령 처리에 막히지 않는다.**
//
// 지금은 모든 명령이 상태를 읽는 것뿐이라 빠르지만, Task 10 이 정산 조회를
// 붙이면 /pnl 이 수 초 걸린다. 그때 긴급한 /shutdown 이 그 뒤에 밀리면
// 사람이 봇을 못 세운다. 느린 처리를 주입해 그 성질을 고정한다.
func TestPollNotBlockedBySlowCommand(t *testing.T) {
	h := newTGHarness(t, 42)
	st := stateWith(t, nil)

	release := make(chan struct{})
	var started, finished sync.WaitGroup
	started.Add(1)
	finished.Add(1)
	var once sync.Once
	h.client.handler = func(text string, st *state) {
		if text == "/slow" {
			once.Do(started.Done)
			<-release
			finished.Done()
			return
		}
		h.client.handle(text, st)
	}
	defer func() { close(release); finished.Wait() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.push(42, 1, "/slow", time.Now().Add(time.Second))
	go h.client.Poll(ctx, st)
	started.Wait() // 느린 처리가 시작됐다

	// 느린 처리가 붙잡혀 있는 동안 긴급 명령이 처리돼야 한다.
	h.push(42, 2, "/shutdown", time.Now().Add(time.Second))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st.Pending() == "shutdown" {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("느린 명령이 붙잡고 있는 동안 /shutdown 이 처리되지 않았다 — 폴 루프가 막혔다")
}
