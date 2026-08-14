package main

// 이 파일은 봇의 상태를 모니터로 밀어 보내고 명령을 받아 온다.
//
// # 두 가지 제약이 이 파일의 전부다
//
//  1. **Publish 는 절대 블록하지 않는다.** `exec` 루프는 회차당 6,000 바퀴
//     (기본 50ms) 돌고, 그 안에서 네트워크를 기다리면 재호가가 밀린다. 밀린
//     재호가는 큐 위치를 잃는데, 최우선 호가에서 큐 위치는 체결을 지배한다.
//     그래서 Publish 는 포인터 하나를 원자적으로 바꾸고 끝난다.
//
//  2. **beat 실패는 거래를 멈추지 않는다.** `internal/ledger` 와 같은
//     원칙이다 — 감시가 안 된다고 거래를 멈추면 감시 장치의 장애가 그대로
//     거래 장애가 된다. 대신 실패를 세어 두고, 배선이 그 값을 보고 사람에게
//     알린다(모니터가 죽으면 텔레그램이 조용해질 뿐이고, 그 침묵은 "이상
//     없음"과 구분되지 않는다).
//
// # 큐가 아니라 최신값 하나다
//
// 보내지 못한 스냅샷을 쌓아 두면 모니터가 과거를 현재로 읽는다. 3초 전
// 스냅샷으로 "노출 위반 없음"을 판정하는 것은 판정하지 않는 것보다 나쁘다 —
// 조용한 오답이기 때문이다. 오래된 beat 는 보낼 가치가 없다.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

// defaultBeatInterval 은 beat 주기다. `rule.StaleAfter`(20초)의 6분의 1 —
// 여섯 번 연속 실패해야 모니터가 죽었다고 판정한다.
const defaultBeatInterval = 3 * time.Second

// beatTimeout 은 한 번의 POST 시한이다. beat 주기보다 짧아야 한다 — 길면
// 요청이 겹치고, 겹친 요청은 seq 역전으로 모니터에서 거부된다.
const beatTimeout = 2 * time.Second

// maxReplyBody 는 응답 본문 상한이다. 모니터가 오작동해 거대한 응답을 보내도
// 봇의 메모리를 먹지 못하게 한다.
const maxReplyBody = 4 << 10

type reporter struct {
	endpoint string
	secret   []byte
	bootID   string
	interval time.Duration
	client   *http.Client

	// latest 는 가장 최근 스냅샷이다. 큐가 아닌 이유는 파일 상단 참고.
	latest atomic.Pointer[beat.Snapshot]
	seq    atomic.Uint64
	cmds   chan beat.Command

	// consecFail 은 연속 전송 실패 횟수다. 봇이 모니터의 사망을 아는 유일한
	// 창구이므로 배선이 읽는다 — 원자적으로 다룬다.
	consecFail atomic.Int64

	// refresh 는 **전송 직전에** 살아 있는 탐침을 다시 읽는 훅이다. nil 이면
	// 저장된 스냅샷을 그대로 보낸다.
	//
	// # 왜 Publish 가 아니라 send 인가 (2026-08-14 실거래가 가르쳤다)
	//
	// WS 마지막 수신 시각·체결 조회 시각·레이트 잔량은 스냅샷의 **내용**이
	// 아니라 그 순간의 **관측**이다. Publish 시점에 박아 두면 그 값의 신선도가
	// 곧 "Publish 가 얼마나 자주 불리는가" 가 된다.
	//
	// 실제로 그 일이 났다. Publish 는 exec 루프(50ms)와 회차 경계에서만
	// 불리는데, 시간대 관문이 생기면서 걸지 않는 회차는 exec 루프를 아예 돌지
	// 않는다. 그러면 5분 내내 같은 WSLastDataAt 이 3초마다 재전송되고, 모니터는
	// 마켓데이터가 300초 정체됐다고 읽는다 — 호가는 5 Hz 로 멀쩡히 오는데.
	// 하루 절반이 그 상태였다.
	//
	// 전송 주기(3초)마다 다시 읽으면 신선도가 거래 활동과 무관해진다.
	refresh func(*beat.Snapshot)

	// lastCmd 는 이미 흘려보낸 명령이다. **명령은 사건이 아니라 상태이므로
	// 값이 바뀔 때만 처리한다.** 모니터는 봇이 받아 갈 때까지 같은 명령을
	// 반복해 답하는데(한 번만 싣고 그 응답이 유실되면 종료 요청이 조용히
	// 사라진다), 그것을 매번 처리하면 종료가 3초마다 다시 시작된다.
	//
	// send 고루틴만 만지지만 Acked 가 밖에서 읽으므로 원자적으로 다룬다.
	lastCmd atomic.Value // beat.Command
}

func newReporter(endpoint string, secret []byte, bootID string) *reporter {
	return &reporter{
		endpoint: endpoint,
		secret:   secret,
		bootID:   bootID,
		interval: defaultBeatInterval,
		client:   &http.Client{Timeout: beatTimeout},
		// 버퍼 1 이면 충분하다. 명령은 드물고, 앞선 명령을 아직 처리하지
		// 못했다면 새 명령을 쌓는 것보다 버리는 편이 낫다 — 종료를 두 번
		// 시작하는 것보다 한 번 늦는 쪽이 안전하다.
		cmds: make(chan beat.Command, 1),
	}
}

// Commands 는 모니터가 내린 명령을 흘린다.
func (r *reporter) Commands() <-chan beat.Command { return r.cmds }

// ConsecFail 은 연속 전송 실패 횟수다.
func (r *reporter) ConsecFail() int { return int(r.consecFail.Load()) }

// Publish 는 `exec` 루프가 부른다. 원자적 교체 하나가 전부다.
//
// BootID 는 여기서 채운다 — 호출자가 채우게 하면 언젠가 빠뜨린다.
// **Seq 는 여기서 채우지 않는다.** 그 이유는 send 에 적었다.
func (r *reporter) Publish(s beat.Snapshot) {
	s.BootID = r.bootID
	r.latest.Store(&s)
}

// Run 은 주기마다 최신 스냅샷을 보낸다. ctx 가 끝날 때까지 돈다.
func (r *reporter) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.send(ctx)
		}
	}
}

// send 는 한 번의 왕복이다. **어떤 실패도 밖으로 던지지 않는다** — 이 함수의
// 실패는 거래와 무관하고, 에러를 돌려주면 호출자가 언젠가 그것으로 무언가를
// 판단하게 된다.
func (r *reporter) send(ctx context.Context) {
	s := r.latest.Load()
	if s == nil {
		return // 아직 아무것도 발행되지 않았다 — 실패가 아니다
	}
	snap := *s
	// **살아 있는 탐침을 여기서 다시 읽는다.** 이유는 refresh 필드 주석 참고.
	if r.refresh != nil {
		r.refresh(&snap)
	}
	// **Seq 와 TS 는 전송 시점에 찍는다. 스냅샷이 아니라 요청의 것이다.**
	//
	// 처음에는 Seq 를 Publish 에서 찍었다. 실거래가 그것을 반증했다
	// (2026-08-11 11:15~11:20): confidence 미달로 건너뛴 회차는 `exec` 루프가
	// 아예 돌지 않아 Publish 가 한 번도 불리지 않는다. 그 5분 동안 같은 seq 를
	// 3초마다 재전송했고, 모니터의 재전송 방지가 전부 거부해
	// `하트비트 무응답` 이 회차 내내 울렸다.
	//
	// **이 봇에서 건너뛴 회차는 가장 흔한 정상 상태다.** rule 패키지는
	// "안 하고 있음은 알람이 아니다" 를 지키려고 통째로 설계됐는데, 전송
	// 계층이 그것을 무너뜨리고 있었다 — 규칙이 아무리 옳아도 스냅샷이
	// 도착하지 못하면 소용이 없다.
	//
	// 재전송 방지는 그대로다. Gate 가 막으려는 것은 **붙잡은 요청의 재생**
	// 이고, 그 카운터는 요청에 속한다. 캡처한 본문을 다시 보내면 그 seq 는
	// 이미 지나간 값이라 여전히 거부된다.
	snap.Seq = r.seq.Add(1)
	snap.TS = time.Now().UTC()

	body, err := json.Marshal(snap)
	if err != nil {
		r.consecFail.Add(1)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		r.consecFail.Add(1)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(beat.SigHeader, beat.Sign(r.secret, body))

	resp, err := r.client.Do(req)
	if err != nil {
		r.consecFail.Add(1)
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReplyBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		r.consecFail.Add(1)
		return
	}
	r.consecFail.Store(0)

	var reply beat.Reply
	if err := json.Unmarshal(raw, &reply); err != nil {
		// 서버는 살아 있으니 실패로 세지 않는다. 명령만 못 읽었을 뿐이다.
		return
	}
	r.dispatch(reply)
}

// Acked 는 봇이 실제로 받아서 흘려보낸 마지막 명령이다. 스냅샷 조립이
// 이것을 실어 보내고, 모니터는 그것으로 `/cancel_shutdown` 에 정직하게
// 답한다 — 아직 안 받아 갔으면 취소할 수 있고, 받아 갔으면 되돌릴 수 없다.
func (r *reporter) Acked() beat.Command {
	c, _ := r.lastCmd.Load().(beat.Command)
	if c == "" {
		return beat.CmdNone
	}
	return c
}

// dispatch 는 응답의 명령을 채널로 흘린다.
//
// **알 수 없는 명령은 무시한다.** 모니터가 오작동하거나 버전이 앞서 나가도
// 봇은 자기가 아는 것만 한다 — 모르는 문자열을 "일단 멈춤"으로 해석하는
// 것이 안전해 보이지만, 그러면 모니터의 버그가 거래 중단이 된다.
//
// **값이 바뀔 때만 흘린다.** 모니터는 같은 명령을 매 beat 반복해 답하므로,
// 그것을 매번 처리하면 종료가 3초마다 다시 시작된다.
func (r *reporter) dispatch(reply beat.Reply) {
	if !reply.Command.Valid() {
		return
	}
	if reply.Command == beat.CmdNone {
		// 명령이 거둬졌다. 같은 명령이 다시 오면 그때는 새 명령이다.
		r.lastCmd.Store(beat.CmdNone)
		return
	}
	if reply.Command == r.Acked() {
		return // 이미 처리했다
	}
	select {
	case r.cmds <- reply.Command:
		r.lastCmd.Store(reply.Command)
	default:
		// 채널이 찼다 = 앞선 명령을 아직 처리 중이다. 버린다 — 모니터는
		// 봇이 받아 갈 때까지 같은 명령을 반복해 답하므로 다음 beat 에
		// 다시 온다. **lastCmd 는 갱신하지 않는다** — 갱신하면 흘리지도
		// 못한 명령을 처리했다고 보고하게 된다.
	}
}
