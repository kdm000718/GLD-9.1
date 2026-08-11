package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

// --- 헬퍼 ---

var testSecret = []byte("s3cret")

func newTestState() *state { return newState(wantConsts(), testSecret) }

// post 는 서명된 beat 한 건을 보낸다.
func post(t *testing.T, st *state, snap beat.Snapshot) (int, beat.Reply) {
	t.Helper()
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
	req.Header.Set(beat.SigHeader, beat.Sign(testSecret, body))
	w := httptest.NewRecorder()
	st.handleBeat(w, req)

	var reply beat.Reply
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatalf("응답 파싱: %v (%q)", err, w.Body.String())
		}
	}
	return w.Code, reply
}

// --- 테스트 ---

// 서명이 없거나 틀리면 404 다. 401 은 "여기 뭔가 있다" 를 알려주는데, 이
// 엔드포인트는 공인이라 스캐너가 먼저 찾는다.
func TestUnsignedRequestIs404(t *testing.T) {
	st := newTestState()
	body := []byte(`{"seq":1}`)
	for name, sig := range map[string]string{
		"헤더 없음":  "",
		"hex 아님": "zz",
		"다른 비밀":  beat.Sign([]byte("wrong"), body),
		"본문 불일치": beat.Sign(testSecret, []byte(`{"seq":2}`)),
	} {
		req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
		if sig != "" {
			req.Header.Set(beat.SigHeader, sig)
		}
		w := httptest.NewRecorder()
		st.handleBeat(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, w.Code)
		}
	}
}

// 서명이 맞아도 JSON 이 아니면 404 다 — 정상 봇은 그런 것을 보내지 않는다.
func TestMalformedBodyIs404(t *testing.T) {
	st := newTestState()
	body := []byte(`not json`)
	req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
	req.Header.Set(beat.SigHeader, beat.Sign(testSecret, body))
	w := httptest.NewRecorder()
	st.handleBeat(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestValidBeatAccepted(t *testing.T) {
	st := newTestState()
	snap := healthySnapshot()
	snap.Seq, snap.BootID = 1, "a"

	code, reply := post(t, st, *snap)
	if code != http.StatusOK {
		t.Fatalf("code = %d, want 200", code)
	}
	if reply.Command != beat.CmdNone {
		t.Errorf("command = %q, want none", reply.Command)
	}
	got, at := st.Latest()
	if got == nil || got.Seq != 1 {
		t.Fatal("스냅샷이 저장되지 않았다")
	}
	if at.IsZero() {
		t.Error("수신 시각이 기록되지 않았다")
	}
}

// 수신 시각은 **우리 시계**여야 한다. 봇이 찍은 TS 를 그대로 쓰면 죽은 봇의
// 마지막 TS 로 신선도를 재게 되고, 그러면 죽은 봇이 영원히 신선해 보인다.
func TestLastBeatUsesReceiveTimeNotBotClock(t *testing.T) {
	st := newTestState()
	snap := healthySnapshot()
	snap.Seq, snap.BootID = 1, "a"
	snap.TS = time.Now().UTC().Add(-30 * time.Second) // 허용창 안, 그러나 과거

	if code, _ := post(t, st, *snap); code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	_, at := st.Latest()
	if d := time.Since(at); d > 5*time.Second {
		t.Errorf("수신 시각이 %v 전이다 — 봇의 TS 를 쓰고 있다", d)
	}
}

// 재전송은 거부하되, 봇 재시작(BootID 변화)은 seq 가 1 로 돌아가는 것이
// 정상이므로 받아들인다. 이것을 막으면 재시작 뒤 모든 beat 가 거부되어
// 모니터가 봇을 영원히 죽은 것으로 본다.
func TestReplayRejectedButRestartAccepted(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()

	s.Seq, s.BootID = 10, "a"
	if code, _ := post(t, st, *s); code != http.StatusOK {
		t.Fatalf("첫 beat: %d", code)
	}
	if code, _ := post(t, st, *s); code != http.StatusConflict {
		t.Errorf("재전송에 %d, want 409", code)
	}
	s.Seq, s.BootID = 1, "b" // 재시작
	if code, _ := post(t, st, *s); code != http.StatusOK {
		t.Errorf("재시작 뒤 seq 1 에 %d, want 200", code)
	}
}

// 재시작은 세어진다 — mtime 하트비트가 절대 못 잡는 크래시루프의 근거다.
func TestBootChangesCounted(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()

	s.Seq, s.BootID = 1, "a"
	post(t, st, *s)
	// 첫 관측은 재시작이 아니다 — 그 전을 모르기 때문이다.
	if n := st.BootChangesSince(time.Now().Add(-time.Hour)); n != 0 {
		t.Errorf("첫 beat 에 재시작 %d회로 세어졌다", n)
	}
	for i, boot := range []string{"b", "c"} {
		s.Seq, s.BootID = uint64(i+1), boot
		post(t, st, *s)
	}
	if n := st.BootChangesSince(time.Now().Add(-time.Hour)); n != 2 {
		t.Errorf("재시작 %d회, want 2", n)
	}
	// 창 밖은 세지 않는다.
	if n := st.BootChangesSince(time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("창 밖인데 %d회로 세어졌다", n)
	}
}

// 명령은 봇이 받아 갈 때까지 반복해서 실린다. 한 번만 싣고 그 응답이
// 유실되면 종료 요청이 조용히 사라진다.
func TestPendingCommandRepeats(t *testing.T) {
	st := newTestState()
	st.SetPending(beat.CmdShutdown)
	s := healthySnapshot()
	s.BootID = "a"

	for seq := uint64(1); seq <= 3; seq++ {
		s.Seq = seq
		code, reply := post(t, st, *s)
		if code != http.StatusOK {
			t.Fatalf("seq %d: code %d", seq, code)
		}
		if reply.Command != beat.CmdShutdown {
			t.Errorf("seq %d 응답이 %q 다", seq, reply.Command)
		}
	}
}

// **수신 확인은 봇이 스냅샷으로 되돌려 줄 때 비로소 안다.** 모니터는 명령을
// 200 응답에 실을 뿐 봇이 그것을 읽었는지 알 수 없다 — /cancel_shutdown 이
// 정직하려면 이 구분이 필요하다.
func TestPendingAckedOnlyWhenBotReportsIt(t *testing.T) {
	st := newTestState()
	st.SetPending(beat.CmdShutdown)
	s := healthySnapshot()
	s.BootID = "a"

	s.Seq = 1
	post(t, st, *s)
	if st.PendingAcked() {
		t.Error("봇이 아직 보고하지 않았는데 받아 간 것으로 본다")
	}

	s.Seq, s.AckedCommand = 2, beat.CmdShutdown
	post(t, st, *s)
	if !st.PendingAcked() {
		t.Error("봇이 보고했는데 받아 가지 않은 것으로 본다")
	}
}

// 다른 명령을 ack 한 것은 이 명령의 ack 가 아니다. halt 를 받아 간 봇에게
// 새로 내린 shutdown 을 "이미 받아 갔다" 고 읽으면 취소가 막힌다.
func TestPendingAckedIsPerCommand(t *testing.T) {
	st := newTestState()
	st.SetPending(beat.CmdShutdown)
	s := healthySnapshot()
	s.BootID, s.Seq, s.AckedCommand = "a", 1, beat.CmdHalt
	post(t, st, *s)
	if st.PendingAcked() {
		t.Error("halt 를 ack 한 것이 shutdown 의 ack 로 읽혔다")
	}
}

func TestNoPendingIsNeverAcked(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.BootID, s.Seq, s.AckedCommand = "a", 1, beat.CmdNone
	post(t, st, *s)
	if st.PendingAcked() {
		t.Error("걸린 명령이 없는데 ack 로 읽혔다")
	}
}

// 연속 스킵은 사유별로 센다. 사유가 바뀌면 리셋한다 — 서로 다른 사유가
// 번갈아 나는 것은 "같은 문제가 지속" 이 아니다.
func TestConsecSkipsResetOnReasonChange(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.BootID = "a"
	s.Round.State = beat.RoundSkipped

	seq := uint64(0)
	send := func(r beat.SkipReason) {
		seq++
		s.Seq, s.Round.SkipReason = seq, r
		post(t, st, *s)
	}
	send(beat.SkipConfBelow)
	send(beat.SkipConfBelow)
	send(beat.SkipConfBelow)
	if got := st.ConsecSkips()[beat.SkipConfBelow]; got != 3 {
		t.Errorf("conf_below 연속 %d, want 3", got)
	}
	send(beat.SkipEquity)
	c := st.ConsecSkips()
	if c[beat.SkipEquity] != 1 {
		t.Errorf("equity 연속 %d, want 1", c[beat.SkipEquity])
	}
	if c[beat.SkipConfBelow] != 0 {
		t.Errorf("사유가 바뀌었는데 conf_below 가 %d 로 남았다", c[beat.SkipConfBelow])
	}
}

// 참여한 회차가 오면 카운터가 통째로 리셋된다.
func TestConsecSkipsResetOnActiveRound(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.BootID = "a"

	s.Seq, s.Round.State, s.Round.SkipReason = 1, beat.RoundSkipped, beat.SkipConfBelow
	post(t, st, *s)
	s.Seq, s.Round.State, s.Round.SkipReason = 2, beat.RoundActive, ""
	post(t, st, *s)
	if n := len(st.ConsecSkips()); n != 0 {
		t.Errorf("참여 회차 뒤에 스킵 카운터가 %d개 남았다", n)
	}
}

// 본문 크기 상한. 공인 엔드포인트에 무한 본문을 보내면 메모리가 나간다.
//
// **본문이 유효한 JSON 이어야 이 테스트가 의미가 있다.** 쓰레기 바이트를
// 보내면 크기 검사가 없어도 JSON 파싱에서 404 가 나고, 테스트는 통과하면서
// 아무것도 지키지 않는다 — 초안이 실제로 그랬고 변이(상한 제거)가 살아남아
// 드러났다.
func TestOversizedBodyRejected(t *testing.T) {
	st := newTestState()
	huge := healthySnapshot()
	huge.Seq, huge.BootID = 1, "a"
	huge.Version = strings.Repeat("x", maxBeatBody+1) // 유효 JSON, 상한 초과

	body, err := json.Marshal(huge)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= maxBeatBody {
		t.Fatalf("본문이 %d바이트로 상한(%d)을 안 넘는다 — 테스트가 무의미하다", len(body), maxBeatBody)
	}
	req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
	req.Header.Set(beat.SigHeader, beat.Sign(testSecret, body))
	w := httptest.NewRecorder()
	st.handleBeat(w, req)

	if w.Code == http.StatusOK {
		t.Error("과대 본문이 통과했다")
	}
	if got, _ := st.Latest(); got != nil {
		t.Error("과대 본문이 상태로 저장됐다")
	}
}

// **상한 검사가 실제로 결정을 내리는 유일한 경우다.**
//
// 메모리 보호는 io.LimitReader 가 한다. 훨씬 큰 본문은 잘려서 JSON 파싱에
// 실패하므로 길이 검사가 없어도 404 다 — 즉 그런 입력으로는 이 검사를
// 시험할 수 없다(변이가 살아남아 드러났다).
//
// 검사가 갈리는 지점은 **정확히 상한+1 바이트의 유효한 JSON** 하나뿐이다.
// LimitReader 가 그만큼은 온전히 읽어 주므로, 검사가 없으면 파싱에 성공해
// 그대로 받아들여진다.
func TestBodyExactlyOverLimitRejected(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.Seq, s.BootID = 1, "a"

	// 패딩을 조절해 직렬화 길이를 정확히 maxBeatBody+1 로 맞춘다.
	base, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	// Version 은 이미 값이 들어 있으므로 그 길이를 되돌려 더한다.
	pad := maxBeatBody + 1 - len(base) + len(s.Version)
	if pad <= 0 {
		t.Fatalf("기본 스냅샷이 이미 %d바이트다", len(base))
	}
	s.Version = strings.Repeat("x", pad)
	body, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != maxBeatBody+1 {
		t.Fatalf("본문 %d바이트, want %d — 패딩 계산이 틀렸다", len(body), maxBeatBody+1)
	}
	// 잘리지 않았다면 유효한 JSON 이라는 것을 먼저 확인한다.
	var probe beat.Snapshot
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("본문이 유효한 JSON 이 아니다 — 테스트가 무의미하다: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
	req.Header.Set(beat.SigHeader, beat.Sign(testSecret, body))
	w := httptest.NewRecorder()
	st.handleBeat(w, req)

	if w.Code == http.StatusOK {
		t.Error("상한+1 바이트 본문이 통과했다")
	}
}

// 서명 없는 요청은 상태를 바꾸지 않는다.
func TestUnsignedBeatDoesNotMutateState(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.BootID, s.Seq = "a", 5
	post(t, st, *s)

	bad, _ := json.Marshal(beat.Snapshot{Seq: 9999, BootID: "evil", TS: time.Now().UTC()})
	req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(bad))
	w := httptest.NewRecorder()
	st.handleBeat(w, req)

	got, _ := st.Latest()
	if got.Seq != 5 {
		t.Errorf("서명 없는 요청이 상태를 바꿨다: seq = %d", got.Seq)
	}
	s.Seq = 6
	if code, _ := post(t, st, *s); code != http.StatusOK {
		t.Errorf("정상 beat 가 %d 로 막혔다", code)
	}
}

// **재전송으로 거부된 beat 도 상태를 바꾸지 않는다.**
//
// 서명은 맞지만 seq 가 전진하지 않은 요청이다 — 서명 없는 요청은 그 전에
// 404 로 막히므로 이 경로에 닿지도 않는다. 초안은 그것만 시험해서 admitErr
// 분기가 통째로 시험되지 않았다.
//
// 거부된 내용이 저장되면 모니터가 옛 스냅샷을 현재로 읽는다. 재전송을
// 흘려 보낸 쪽이 그 값을 고를 수 있으므로 조용한 오답을 심을 수 있다.
func TestReplayedBeatDoesNotMutateState(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.BootID, s.Seq = "a", 5
	if code, _ := post(t, st, *s); code != http.StatusOK {
		t.Fatalf("첫 beat: %d", code)
	}

	// 같은 seq, 다른 내용 — 받아들여지면 안 된다.
	replay := healthySnapshot()
	replay.BootID, replay.Seq = "a", 5
	replay.Armed = false
	replay.Equity.AvailableUSDT = 999999
	replay.Round.State = beat.RoundSkipped
	replay.Round.SkipReason = beat.SkipDailyLimit

	if code, _ := post(t, st, *replay); code != http.StatusConflict {
		t.Fatalf("재전송이 409 로 거부되지 않았다")
	}
	got, _ := st.Latest()
	if !got.Armed || got.Equity.AvailableUSDT == 999999 {
		t.Error("재전송된 내용이 저장됐다")
	}
	if n := st.ConsecSkips()[beat.SkipDailyLimit]; n != 0 {
		t.Errorf("재전송이 스킵 카운터를 %d 로 올렸다", n)
	}
}

// Evaluate 가 판정에 필요한 것을 모두 모아 넘긴다 — 여기가 비면 rule 이
// 아무리 옳아도 알람이 나지 않는다.
func TestEvaluateGathersInput(t *testing.T) {
	st := newTestState()
	s := healthySnapshot()
	s.BootID, s.Seq = "a", 1
	s.Round.State, s.Round.SkipReason = beat.RoundSkipped, beat.SkipEquity
	post(t, st, *s)

	in := st.Evaluate(time.Now().UTC())
	if in.Snap == nil {
		t.Fatal("스냅샷이 안 실렸다")
	}
	if in.Want != wantConsts() {
		t.Error("예상 상수가 안 실렸다 — 상수 대조가 통째로 죽는다")
	}
	if in.ConsecSkips[beat.SkipEquity] != 1 {
		t.Error("연속 스킵이 안 실렸다")
	}
	if in.LastBeat.IsZero() {
		t.Error("수신 시각이 안 실렸다 — 신선도 판정이 죽는다")
	}
}

// 모니터가 개인키를 읽으면 이 설계 전체가 무의미하다. 소스 스캔으로 막는다 —
// 런타임 가드는 잊히지만 이 테스트는 잊히지 않는다.
func TestMonitorNeverReadsPrivateKey(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("스캔할 파일이 없다 — 이 테스트가 아무것도 지키지 않는다")
	}
	scanned := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for _, banned := range []string{
			"WALLET_PRIVATE_KEY",    // 개인키 환경변수
			"auth.NewSigner",        // 서명자 생성
			"SignForPredictAccount", // 주문 서명
			"predictfun/auth",       // 인증 패키지 자체
		} {
			if bytes.Contains(src, []byte(banned)) {
				t.Errorf("%s 가 %q 를 참조한다 — 모니터는 서명하지 않는다", f, banned)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("비테스트 파일을 하나도 못 읽었다")
	}
}
