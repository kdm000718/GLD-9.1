// Package ws는 predict.fun WebSocket 연결을 관리한다.
//
// 구조: readLoop goroutine 1개, writeLoop goroutine 1개.
// 소켓 쓰기는 전부 writeLoop 하나로 직렬화한다 — Go WebSocket 라이브러리는
// 동시 write를 허용하지 않아 panic이 난다.
package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/kdm000718/GLD-9.1/internal/timing"
)

// defaultUserAgent는 Options.UserAgent가 비어 있을 때 쓴다.
// 기본 Go User-Agent는 WAF가 403으로 막는다.
const defaultUserAgent = "gld91/0.1 (+https://github.com/kdm000718/GLD-9.1)"

// readLimit은 오더북 전체 스냅샷을 받기 위한 프레임 크기 상한이다.
// coder/websocket의 기본값 32KiB는 만기 직전 두꺼운 호가에서 부족하다.
const readLimit = 8 << 20 // 8 MiB

// Frame은 소켓에서 읽은 원문과 수신 시각이다.
type Frame struct {
	Seq        uint64
	RecvUnixNs int64
	RecvMonoNs int64
	Raw        []byte
	Msg        Message
}

// Sender는 연결에 프레임을 보낼 수 있는 대상이다.
type Sender interface {
	Send(ctx context.Context, req Request) error
}

type Options struct {
	URL       string
	APIKey    string
	UserAgent string
	Logger    *slog.Logger

	// ReadTimeout은 읽기 데드라인이다. 하트비트 15초 × 2 + 여유.
	// 초과하면 연결을 죽이고 재접속한다.
	ReadTimeout time.Duration
	// StaleTimeout은 마지막 메시지 수신 후 강제 재접속까지의 시간이다.
	StaleTimeout time.Duration
	MaxBackoff   time.Duration

	// OnConnect는 연결 직후 호출된다. 여기서 전체 재구독을 해야 한다 —
	// 서버는 구독 상태를 기억하지 않는다.
	OnConnect func(ctx context.Context, s Sender) error
	// OnFrame은 하트비트를 제외한 모든 수신 프레임에 대해 호출된다.
	OnFrame func(f Frame)
	// OnGap은 연결이 끊겨 데이터 공백이 생겼을 때 호출된다.
	OnGap func(start, end time.Time, reason string)
}

type Client struct {
	opt Options
	log *slog.Logger

	mu   sync.Mutex
	conn *conn // 현재 활성 연결. 없으면 nil.

	// connects는 성공한 연결 확립 횟수다. **최초 접속도 포함한다** — 그래서
	// 재접속 횟수는 connects-1 이다(Reconnects 참고). 이 둘을 같은 값으로 쓰면
	// 한 번도 안 끊긴 실행이 "재접속 1회"로 보고된다.
	connects int64
}

func New(o Options) *Client {
	if o.ReadTimeout <= 0 {
		o.ReadTimeout = 35 * time.Second
	}
	if o.StaleTimeout <= 0 {
		o.StaleTimeout = 60 * time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = 30 * time.Second
	}
	if o.UserAgent == "" {
		o.UserAgent = defaultUserAgent
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Client{opt: o, log: o.Logger}
}

// conn은 단일 물리 연결과 그 전용 write 채널이다.
type conn struct {
	ws *websocket.Conn
	// writeCh는 일반 프레임(구독/해제)용이다.
	writeCh chan []byte
	// hbCh는 하트비트 전용이다. 구독 폭주 중에도 하트비트가 밀리면
	// 연결이 끊기므로 별도 채널로 우선 처리한다.
	hbCh chan []byte
	done chan struct{}
}

// Send는 프레임을 write 큐에 넣는다. 여러 goroutine에서 호출해도 안전하다.
func (c *Client) Send(ctx context.Context, req Request) error {
	c.mu.Lock()
	cn := c.conn
	c.mu.Unlock()
	if cn == nil {
		return errors.New("연결이 없다")
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ch := cn.writeCh
	if req.Method == "heartbeat" {
		ch = cn.hbCh
	}
	select {
	case ch <- b:
		return nil
	case <-cn.done:
		return errors.New("연결이 닫혔다")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reconnects는 지금까지의 **재**접속 횟수다 — 최초 접속은 세지 않는다.
// 끊김 없이 끝난 실행은 0을 돌려준다. 한 번도 접속하지 못한 실행도 0이다
// (connects=0 → 음수가 되지 않도록 막는다).
func (c *Client) Reconnects() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connects == 0 {
		return 0
	}
	return c.connects - 1
}

// ForceReconnect는 현재 연결을 강제로 끊어 재접속을 유발한다.
//
// 하트비트는 계속 오는데 마켓 데이터만 끊긴 경우(재구독 누락)를 복구하는 용도다.
// 하트비트 트래픽이 읽기 데드라인을 계속 갱신하므로 데이터 고갈은
// 별도 워치독으로만 잡을 수 있다.
func (c *Client) ForceReconnect(reason string) {
	c.mu.Lock()
	cn := c.conn
	c.mu.Unlock()
	if cn == nil {
		return
	}
	c.log.Warn("강제 재접속", "reason", reason)
	cn.ws.Close(websocket.StatusGoingAway, "forced reconnect")
}

// Connected는 현재 연결 여부다.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Run은 끊기면 재접속하며 ctx가 끝날 때까지 계속 돈다.
func (c *Client) Run(ctx context.Context) error {
	backoff := time.Second
	var downSince time.Time

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		err := c.runOnce(ctx, &downSince)

		if ctx.Err() != nil {
			return ctx.Err()
		}
		if downSince.IsZero() {
			downSince = time.Now()
		}
		c.log.Warn("ws 연결 종료, 재접속한다", "err", err, "backoff", backoff)

		// 지수 백오프 + 지터 ±20%.
		jitter := time.Duration((rand.Float64()*0.4 - 0.2) * float64(backoff))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff + jitter):
		}
		if backoff *= 2; backoff > c.opt.MaxBackoff {
			backoff = c.opt.MaxBackoff
		}
	}
}

func (c *Client) runOnce(ctx context.Context, downSince *time.Time) error {
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	h := http.Header{}
	h.Set("User-Agent", c.opt.UserAgent)
	if c.opt.APIKey != "" {
		h.Set("x-api-key", c.opt.APIKey)
	}

	ws, resp, err := websocket.Dial(dialCtx, c.opt.URL, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		// 핸드셰이크 실패는 HTTP 상태코드로 온다.
		if resp != nil {
			return fmt.Errorf("핸드셰이크 실패 HTTP %d: %w", resp.StatusCode, err)
		}
		return fmt.Errorf("dial 실패: %w", err)
	}
	ws.SetReadLimit(readLimit)

	cn := &conn{
		ws:      ws,
		writeCh: make(chan []byte, 256),
		hbCh:    make(chan []byte, 8),
		done:    make(chan struct{}),
	}

	c.mu.Lock()
	c.conn = cn
	c.connects++
	first := c.connects == 1
	c.mu.Unlock()

	if !first && !downSince.IsZero() && c.opt.OnGap != nil {
		c.opt.OnGap(*downSince, time.Now(), "reconnect")
	}
	*downSince = time.Time{}
	c.log.Info("ws 연결됨", "url", c.opt.URL, "connect", c.connects)

	// 연결 수명 전체를 감싸는 컨텍스트. 어느 한 루프가 죽으면 전부 정리한다.
	connCtx, connCancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.writeLoop(connCtx, cn)
	}()

	// 재구독은 write 루프가 살아난 뒤에 해야 한다.
	var onConnectErr error
	if c.opt.OnConnect != nil {
		if err := c.opt.OnConnect(connCtx, c); err != nil {
			c.log.Warn("재구독 실패", "err", err)
			onConnectErr = err
		}
	}

	var readErr error
	if onConnectErr != nil {
		// 재구독에 실패한 연결은 데이터가 오지 않는 채로 붙어만 있는 것이
		// 최악이다. 읽기를 시작하지 않고 바로 버려 재접속을 유발한다.
		readErr = fmt.Errorf("OnConnect 실패로 연결을 버린다: %w", onConnectErr)
	} else {
		readErr = c.readLoop(connCtx, cn)
	}

	connCancel()
	close(cn.done)
	ws.Close(websocket.StatusNormalClosure, "")
	wg.Wait()

	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
	*downSince = time.Now()

	return readErr
}

// writeLoop만이 소켓에 쓴다. 하트비트를 항상 우선한다.
func (c *Client) writeLoop(ctx context.Context, cn *conn) {
	write := func(b []byte) bool {
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := cn.ws.Write(wctx, websocket.MessageText, b)
		cancel()
		if err != nil {
			c.log.Warn("ws write 실패", "err", err)
			cn.ws.Close(websocket.StatusInternalError, "write failed")
			return false
		}
		return true
	}

	for {
		// 하트비트를 먼저 비운다. 구독 프레임이 큐에 쌓여 있어도
		// 하트비트가 지연되면 연결이 끊긴다.
		select {
		case b := <-cn.hbCh:
			if !write(b) {
				return
			}
			continue
		default:
		}

		select {
		case <-ctx.Done():
			return
		case b := <-cn.hbCh:
			if !write(b) {
				return
			}
		case b := <-cn.writeCh:
			if !write(b) {
				return
			}
		}
	}
}

func (c *Client) readLoop(ctx context.Context, cn *conn) error {
	lastMsg := time.Now()

	for {
		// 읽기 데드라인. 초과하면 연결을 죽이고 재접속한다.
		rctx, cancel := context.WithTimeout(ctx, c.opt.ReadTimeout)
		_, raw, err := cn.ws.Read(rctx)
		cancel()

		// 프레임을 읽은 직후에 시각을 각인한다. 파싱 뒤에 하면 안 된다.
		recvNs, monoNs := timing.Stamp()

		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("read 실패 (마지막 메시지 %s 전): %w",
				time.Since(lastMsg).Truncate(time.Millisecond), err)
		}
		lastMsg = time.Now()

		var msg Message
		if err := json.Unmarshal(raw, &msg); err != nil {
			// 관대한 파싱: 깨진 프레임 하나로 죽지 않는다.
			c.log.Warn("ws 프레임 파싱 실패", "err", err, "raw", truncate(raw, 200))
			continue
		}

		if msg.IsHeartbeat() {
			c.replyHeartbeat(ctx, msg)
			continue
		}

		if c.opt.OnFrame != nil {
			c.opt.OnFrame(Frame{
				Seq:        timing.NextSeq(),
				RecvUnixNs: recvNs,
				RecvMonoNs: monoNs,
				Raw:        raw,
				Msg:        msg,
			})
		}
	}
}

// replyHeartbeat은 받은 타임스탬프를 그대로 되돌린다.
// 값을 바꾸거나 늦게 보내면 다음 프로브에서 끊긴다.
func (c *Client) replyHeartbeat(ctx context.Context, msg Message) {
	var ts int64
	if err := json.Unmarshal(msg.Data, &ts); err != nil {
		c.log.Warn("하트비트 data 파싱 실패", "err", err, "raw", truncate(msg.Data, 100))
		return
	}
	if err := c.Send(ctx, HeartbeatReply(ts)); err != nil {
		c.log.Warn("하트비트 회신 실패", "err", err)
		return
	}
	c.log.Debug("하트비트 회신", "ts", ts)
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
