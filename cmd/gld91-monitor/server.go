package main

// 이 파일은 봇의 beat 를 받아 상태를 들고 있는다.
//
// # 모니터는 개인키를 갖지 않는다
//
// predict.fun 에는 읽기 전용 키가 없다 — 인증이 거래용 EOA 개인키 서명이라,
// 잔고·미체결 조회나 주문 취소를 하려면 개인키 사본을 이 호스트에 둬야 한다.
// 바이낸스처럼 인출 끄기·IP 화이트리스트로 권한을 깎을 수단도 없다.
//
// 그럴 값이 없다. 이 봇은 매도 주문을 내지 않으므로(스펙 §10) 모니터가 할 수
// 있는 개입은 원래 취소뿐이고, 미체결은 주문에 서명된 만료로 회차 종료 뒤
// 스스로 사라진다.
//
// **대가는 정직하게 적는다: 개인키 없는 모니터는 떠 있는 미체결 주문을
// 관측할 수 없다.** GET /v1/positions/{address} 는 API 키만으로 되지만
// GET /v1/orders 는 JWT 가 필요하다. 그래서 스냅샷이 미체결 목록을 싣는다.

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
	"github.com/kdm000718/GLD-9.1/internal/beat/rule"
)

// maxBeatBody 는 본문 상한이다. 공인 엔드포인트이므로 상한 없이 읽으면
// 아무나 메모리를 채울 수 있다. 실제 스냅샷은 미체결 목록을 다 실어도 수 KB다.
const maxBeatBody = 64 << 10

// beatSkew 는 봇과 모니터 시계 차이의 허용치다.
const beatSkew = time.Minute

// crashLoopWindow 는 재시작을 세는 창이다. rule.CrashLoopChanges 와 짝이다.
const crashLoopWindow = 10 * time.Minute

// latchSustain 은 Warn 급 알람이 울기까지 유지돼야 하는 시간이다. Crit 은
// 기다리지 않는다(rule.Latch 참고).
const latchSustain = 30 * time.Second

// state 는 모니터가 아는 전부다.
type state struct {
	want   beat.Consts
	secret []byte

	mu       sync.Mutex
	latest   *beat.Snapshot
	lastBeat time.Time
	bootID   string
	gate     beat.Gate
	pending  beat.Command
	// bootChanges 는 재시작을 관측한 시각들이다. 창 밖의 것은 관측할 때 버린다.
	bootChanges []time.Time
	consecSkips map[beat.SkipReason]int
	// rounds 는 우리가 실제로 건 회차의 이력이다. **모니터는 봇의 원장을 읽을
	// 수 없으므로**(다른 호스트다) beat 로 지나가는 것을 여기서 누적한다.
	rounds map[string]*participation

	latch *rule.Latch

	// rejectLog 는 사유별 마지막 거절 기록 시각이다(rejectLogEvery 참고).
	rejectLog map[string]time.Time
	// logf 는 거절 기록의 출력구다. 시험이 갈아 끼운다 — 로그를 표준 출력으로
	// 흘리면 "기록했는가" 를 확인할 방법이 없다.
	logf func(string, ...any)
}

func newState(want beat.Consts, secret []byte) *state {
	return &state{
		want:        want,
		secret:      secret,
		gate:        beat.Gate{Skew: beatSkew},
		pending:     beat.CmdNone,
		consecSkips: map[beat.SkipReason]int{},
		rounds:      map[string]*participation{},
		latch:       rule.NewLatch(latchSustain),
		rejectLog:   map[string]time.Time{},
		logf:        log.Printf,
	}
}

// rejectLogEvery 는 같은 사유의 거절을 다시 기록하기까지의 간격이다.
//
// **상한이 필요한 이유는 스캐너다.** 이 엔드포인트는 공인 IP 에 열려 있고,
// 서명 없는 요청 하나하나를 기록하면 저널이 남의 트래픽으로 채워진다 —
// 그러면 진짜 사고의 기록이 회전으로 밀려나간다. 봇은 3초마다 오므로
// 지속되는 문제는 이 간격으로도 충분히 드러난다.
const rejectLogEvery = time.Minute

// noteReject 는 거절을 남긴다. 사유별로 rejectLogEvery 마다 한 번씩만 남긴다.
//
// # 왜 거절이 조용하면 안 되는가
//
// 서명 실패도 재전송도 응답만 보내고 사라지면, 운영자가 보는 것은
// `broken`("스냅샷이 없다") 뿐이다. 그 문구로는 **비밀키 불일치·시계 어긋남·
// 방화벽 차단**이 전부 같아 보인다. 배포 때 실제로 그 상태에 빠졌다 —
// 8443 이 막혀 있었고, 로그에 아무 단서가 없어 추측으로 좁혀야 했다.
//
// 본문도 서명도 적지 않는다. 남기는 것은 사유와 보낸 쪽뿐이다.
func (s *state) noteReject(now time.Time, remote, reason string) {
	s.mu.Lock()
	last, seen := s.rejectLog[reason]
	if seen && now.Sub(last) < rejectLogEvery {
		s.mu.Unlock()
		return
	}
	s.rejectLog[reason] = now
	out := s.logf
	s.mu.Unlock()
	out("beat 거절 [%s] from %s — %s 마다 한 번만 기록한다", reason, remote, rejectLogEvery)
}

// Latest 는 마지막 스냅샷과 그것을 **받은** 시각이다.
//
// 봇이 찍은 TS 가 아니라 수신 시각인 것이 중요하다 — 봇의 시계는 봇이 죽어도
// 멈추지 않고, 죽은 봇의 마지막 TS 로 신선도를 재면 영원히 신선하다.
func (s *state) Latest() (*beat.Snapshot, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest, s.lastBeat
}

// SetPending 은 다음 beat 응답에 실을 명령을 정한다.
func (s *state) SetPending(c beat.Command) {
	s.mu.Lock()
	s.pending = c
	s.mu.Unlock()
}

func (s *state) Pending() beat.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

// PendingAcked 는 지금 걸린 명령을 봇이 **실제로 받아 갔는지**다.
//
// 모니터는 명령을 200 응답에 실을 뿐 봇이 그것을 읽었는지 알 수 없다. 봇이
// 다음 스냅샷의 AckedCommand 로 되돌려 줄 때 비로소 안다. `/cancel_shutdown`
// 이 정직하려면 이 구분이 필요하다 — 아직 안 받아 갔으면 취소할 수 있고,
// 받아 갔으면 되돌릴 수 없다.
func (s *state) PendingAcked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == beat.CmdNone || s.pending == "" || s.latest == nil {
		return false
	}
	return s.latest.AckedCommand == s.pending
}

// ConsecSkips 는 사유별 연속 스킵 횟수의 복사본이다.
func (s *state) ConsecSkips() map[beat.SkipReason]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[beat.SkipReason]int, len(s.consecSkips))
	for k, v := range s.consecSkips {
		out[k] = v
	}
	return out
}

// BootChangesSince 는 그 시각 이후 관측한 재시작 횟수다. 지나간 것은 버린다 —
// 프로세스가 오래 돌면 이 슬라이스가 계속 자란다.
func (s *state) BootChangesSince(t time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.bootChanges[:0]
	for _, c := range s.bootChanges {
		if c.After(t) {
			keep = append(keep, c)
		}
	}
	s.bootChanges = keep
	return len(keep)
}

// handleBeat 은 beat 한 건을 받는다.
//
// **서명 실패는 401 이 아니라 404 다.** 401 은 "여기 뭔가 있다" 를 알려주고,
// 이 엔드포인트는 공인이라 스캐너가 먼저 찾는다. 존재하지 않는 것처럼 보이는
// 편이 낫다. 경로로 좁히지 않는 것도 같은 이유다 — 어느 경로든 서명이 없으면
// 404 이므로 경로 자체가 정보를 주지 않는다.
func (s *state) handleBeat(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBeatBody+1))
	if err != nil || len(body) > maxBeatBody {
		s.noteReject(now, r.RemoteAddr, "본문이 크거나 읽을 수 없다")
		http.NotFound(w, r)
		return
	}
	if !beat.Verify(s.secret, body, r.Header.Get(beat.SigHeader)) {
		// 봇이라면 BEAT_SECRET 이 모니터의 MONITOR_BEAT_SECRET 과 다르다는
		// 뜻이다. 스캐너라면 그냥 남의 트래픽이고, 그 둘을 여기서 가릴 수는
		// 없다 — 보낸 쪽 주소를 남기는 것이 가릴 수 있는 전부다.
		s.noteReject(now, r.RemoteAddr, "서명 불일치 (BEAT_SECRET 확인)")
		http.NotFound(w, r)
		return
	}
	var snap beat.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		// 서명은 맞는데 파싱이 안 된다 — 비밀키를 아는 쪽이 보냈으므로 봇이고,
		// 계약이 갈렸다는 뜻이다(한쪽만 배포된 상태).
		s.noteReject(now, r.RemoteAddr, "서명은 맞는데 스냅샷을 읽을 수 없다 — 봇과 모니터의 배포본이 갈렸다")
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	// 봇이 재시작하면 seq 가 1 로 돌아간다. 그것을 재전송으로 막으면 재시작
	// 뒤 모든 beat 가 거부되어 모니터가 봇을 영원히 죽은 것으로 본다 —
	// 하필 봇이 방금 크래시한 순간에 눈을 감는 고장이다.
	if snap.BootID != s.bootID {
		if s.bootID != "" {
			s.bootChanges = append(s.bootChanges, now)
		}
		s.bootID = snap.BootID
		s.gate.Reset()
	}
	admitErr := s.gate.Admit(snap.Seq, snap.TS, now)
	if admitErr == nil {
		s.latest, s.lastBeat = &snap, now
		s.trackSkipLocked(snap)
		s.observeRoundLocked(snap)
	}
	cmd := s.pending
	s.mu.Unlock()

	if admitErr != nil {
		// Admit 이 거절하는 이유는 재전송(seq 역행)과 시계 어긋남 둘이고,
		// 그 둘은 대응이 완전히 다르다. 사유를 그대로 싣는다 — beat.Gate 가
		// 이미 사람이 읽을 문구로 돌려준다.
		s.noteReject(now, r.RemoteAddr, admitErr.Error())
		http.Error(w, "", http.StatusConflict)
		return
	}
	if !cmd.Valid() {
		cmd = beat.CmdNone
	}
	w.Header().Set("Content-Type", "application/json")
	// **명령은 봇이 받아 갈 때까지 반복해서 싣는다.** 한 번만 싣고 그 응답이
	// 유실되면 종료 요청이 조용히 사라진다. 봇은 값이 바뀔 때만 처리하므로
	// 반복이 중복 실행이 되지 않는다.
	_ = json.NewEncoder(w).Encode(beat.Reply{Command: cmd})
}

// trackSkipLocked 은 연속 스킵을 센다.
//
// 사유가 바뀌면 리셋한다 — 서로 다른 사유가 번갈아 나는 것은 "같은 문제가
// 지속" 이 아니고, 그것을 합쳐 세면 없는 지속을 만들어낸다.
func (s *state) trackSkipLocked(snap beat.Snapshot) {
	if snap.Round.State != beat.RoundSkipped {
		s.consecSkips = map[beat.SkipReason]int{}
		return
	}
	r := snap.Round.SkipReason
	n := s.consecSkips[r]
	s.consecSkips = map[beat.SkipReason]int{r: n + 1}
}

// Evaluate 는 지금 상태의 판정이다. 순수 함수인 rule.Evaluate 에 넘길 입력을
// 모으는 것이 전부다.
func (s *state) Evaluate(now time.Time) rule.Input {
	snap, last := s.Latest()
	return rule.Input{
		Snap:        snap,
		LastBeat:    last,
		Now:         now,
		Want:        s.want,
		ConsecSkips: s.ConsecSkips(),
		BootChanges: s.BootChangesSince(now.Add(-crashLoopWindow)),
	}
}

// Step 은 한 번의 판정이다 — 입력을 모으고, 판정하고, 에지를 가른다.
//
// **runAlarms 가 부르는 것과 같은 함수여야 한다.** 통합 시험이 같은 세 줄을
// 따로 적으면 배선이 바뀌어도 시험은 통과한다 — 시험이 확인하는 것이 배선
// 자체이므로 그 복제는 시험을 무의미하게 만든다. 시각을 인자로 받는 것도
// 같은 이유다: 고루틴과 티커 없이 이 경로를 그대로 돌릴 수 있어야 한다.
func (s *state) Step(now time.Time) (fire []rule.Finding, resolved []string) {
	return s.latch.Step(rule.Evaluate(s.Evaluate(now)), now)
}
