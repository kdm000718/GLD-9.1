package main

// 이 파일은 텔레그램 Bot API 다. 라이브러리를 쓰지 않는다 — 필요한 것이
// send 와 getUpdates 둘뿐이고, 새 의존성은 이 저장소가 늘리지 않는 것이다.
//
// # 봇 토큰은 자금 권한과 무관하다
//
// 모니터가 개인키를 갖지 않는다는 결정과 충돌하지 않는다. 토큰이 유출되면
// 알림이 새고 남이 명령을 보낼 수 있지만(그래서 채팅 화이트리스트가 있다),
// 자금은 움직이지 않는다.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// longPollSec 는 getUpdates 롱폴 시간이다. 클라이언트 시한은 이보다 넉넉해야
// 한다 — 짧으면 매번 시한 초과로 끊기고 명령이 지연된다.
const longPollSec = 30

// maxTGBody 는 응답 본문 상한이다.
const maxTGBody = 1 << 20

type tgClient struct {
	token  string
	chatID int64
	http   *http.Client
	base   string // 테스트가 갈아 끼운다

	// handler 는 명령 하나를 처리한다. 테스트가 느린 처리를 주입해
	// **폴 루프가 그것에 막히지 않는지** 확인하는 이음매다 — 그 성질은
	// 주입 없이는 관측할 수 없다(지금 명령이 전부 빠르기 때문이다).
	// nil 이면 handle 을 쓴다.
	handler func(text string, st *state)
}

func newTG(token string, chatID int64) *tgClient {
	return &tgClient{
		token:  token,
		chatID: chatID,
		http:   &http.Client{Timeout: (longPollSec + 15) * time.Second},
		base:   "https://api.telegram.org",
	}
}

// Send 는 인가된 채팅에 보낸다.
//
// **실패를 삼키고 로그만 남긴다.** 알림이 안 나갔다고 모니터가 죽으면 그
// 뒤의 모든 알림도 사라진다 — 감시 장치가 자기 실패로 감시를 멈추는 형태다.
func (c *tgClient) Send(msg string) {
	if err := c.sendTo(c.chatID, msg); err != nil {
		log.Printf("telegram send: %v", err)
	}
}

func (c *tgClient) sendTo(chatID int64, msg string) error {
	body, err := json.Marshal(map[string]any{"chat_id": chatID, "text": msg})
	if err != nil {
		return err
	}
	resp, err := c.http.Post(c.base+"/bot"+c.token+"/sendMessage",
		"application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxTGBody))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: %s", resp.Status)
	}
	return nil
}

// tgUpdate 는 우리가 쓰는 필드만 담은 최소 부분집합이다.
type tgUpdate struct {
	UpdateID int64 `json:"update_id"`
	Message  *struct {
		Date int64  `json:"date"`
		Text string `json:"text"`
		Chat struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

func (c *tgClient) getUpdates(ctx context.Context, offset int64) ([]tgUpdate, error) {
	q := url.Values{}
	q.Set("timeout", strconv.Itoa(longPollSec))
	if offset > 0 {
		q.Set("offset", strconv.FormatInt(offset, 10))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/bot"+c.token+"/getUpdates?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTGBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram getUpdates: %s", resp.Status)
	}
	var out struct {
		OK     bool       `json:"ok"`
		Result []tgUpdate `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if !out.OK {
		return nil, fmt.Errorf("telegram getUpdates: ok=false")
	}
	return out.Result, nil
}

// Poll 은 명령을 롱폴한다.
//
// 두 가지가 중요하다.
//
// **기동 이전 메시지를 무시한다.** 모니터를 재시작할 때마다 밀려 있던
// /shutdown 이 되살아나면 안 된다 — 재시작은 대개 문제를 고친 직후이고,
// 그때 옛 종료 명령이 실행되면 방금 고친 봇이 다시 멈춘다.
//
// **인가되지 않은 채팅은 거부한다.** 봇 토큰을 아는 누구나 말을 걸 수 있다.
func (c *tgClient) Poll(ctx context.Context, st *state) {
	var offset int64
	startup := time.Now().Unix()

	for ctx.Err() == nil {
		updates, err := c.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return // 취소다, 실패가 아니다
			}
			log.Printf("telegram getUpdates: %v", err)
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Message == nil || u.Message.Date < startup {
				continue
			}
			if u.Message.Chat.ID != c.chatID {
				_ = c.sendTo(u.Message.Chat.ID, "⛔ 인가되지 않은 사용자입니다.")
				log.Printf("인가되지 않은 채팅 %d 의 명령을 거부했다", u.Message.Chat.ID)
				continue
			}
			// **폴 루프 밖에서 처리한다.** 조회 명령이 길어지면 긴급한
			// /shutdown 이 그 뒤에 밀린다 — Task 10 이 정산 조회를 붙이면
			// 실제로 길어진다.
			h := c.handler
			if h == nil {
				h = c.handle
			}
			go h(u.Message.Text, st)
		}
	}
}

func (c *tgClient) handle(text string, st *state) {
	reply, handled := routeCommand(text, st, time.Now().UTC())
	if !handled {
		reply = helpText
	}
	c.Send(reply)
}

// sleepCtx 는 d 만큼 기다리되 ctx 가 끝나면 false 를 돌려준다.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
