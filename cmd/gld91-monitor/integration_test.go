package main

// 이 파일은 **배선**을 시험한다. 규칙 하나하나는 `internal/beat/rule` 의
// 시험이 맡는다 — 여기서 다시 그것을 확인하면 같은 것을 두 곳에서 세게 되고,
// 규칙이 바뀔 때 두 곳이 갈린다.
//
// 여기서만 보이는 것: 수신(handleBeat) → 상태 → 판정(rule.Evaluate) →
// 에지(latch) → 알림이 실제로 한 줄로 이어져 있는가. 각 조각이 옳아도 배선이
// 끊겨 있으면 감시는 조용하고, 그 침묵은 "이상 없음" 과 구분되지 않는다.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

// postBeat 은 서명된 beat 하나를 보낸다. 실제 봇이 하는 것과 같은 경로다.
func postBeat(t *testing.T, st *state, secret []byte, snap *beat.Snapshot) int {
	t.Helper()
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
	req.Header.Set(beat.SigHeader, beat.Sign(secret, body))
	w := httptest.NewRecorder()
	st.handleBeat(w, req)
	return w.Code
}

// 골든 시퀀스. 각 단계가 **새로 울려야 하는 키**를 정확히 적는다.
//
// 빈 목록이 의미 있는 단계들이 이 시험의 핵심이다 — confidence 미달 스킵과
// 무행동 회차는 이 봇에서 가장 흔한 정상 상태이고, 그때 알람이 나면 사람이
// 알람을 보지 않게 된다.
func TestEndToEndAlertSequence(t *testing.T) {
	secret := []byte("s")
	st := newState(wantConsts(), secret)

	steps := []struct {
		name  string
		mut   func(*beat.Snapshot)
		wants []string // 이 beat 뒤에 **새로** 울려야 하는 키
	}{
		{"건강", func(*beat.Snapshot) {}, nil},
		{"confidence 스킵", func(s *beat.Snapshot) {
			s.Round.State, s.Round.SkipReason = beat.RoundSkipped, beat.SkipConfBelow
		}, nil}, // 조용해야 한다 — 안 하는 것이 이 봇의 정상이다
		{"한 번 걸고 군중이 조용한 회차", func(s *beat.Snapshot) {
			s.Loop.LastActionAt = s.TS.Add(-4 * time.Minute)
		}, nil}, // 조용해야 한다 — 루프는 돌고 있다
		{"노출 위반", func(s *beat.Snapshot) {
			s.Exposure = beat.Exposure{Filled: 9, Cap: 8.62}
		}, []string{"exposure"}},
		{"노출 위반 반복", func(s *beat.Snapshot) {
			s.Exposure = beat.Exposure{Filled: 9, Cap: 8.62}
		}, nil}, // 래치가 잡는다 — 같은 알람을 3초마다 다시 보내지 않는다
		{"복구", func(*beat.Snapshot) {}, nil},
		{"집행 루프 정지", func(s *beat.Snapshot) {
			s.Loop.LastLoopAt = s.TS.Add(-3 * time.Minute)
		}, []string{"loop_stall"}},
		{"루프 복귀", func(*beat.Snapshot) {}, nil},
		{"상수 변조", func(s *beat.Snapshot) { s.Consts.CapFraction = 0.09 }, []string{"consts"}},
	}

	for i, step := range steps {
		snap := healthySnapshot()
		snap.Seq, snap.BootID = uint64(i+1), "a"
		now := time.Now().UTC()
		snap.TS = now
		snap.Loop.LastLoopAt = now
		step.mut(snap)

		if code := postBeat(t, st, secret, snap); code != http.StatusOK {
			t.Fatalf("%s: code = %d, want 200", step.name, code)
		}

		// **runAlarms 와 같은 함수를 부른다.** 여기서 세 줄을 따로 적으면
		// 배선이 끊겨도 이 시험은 통과한다.
		fire, _ := st.Step(now)
		got := make([]string, 0, len(fire))
		for _, f := range fire {
			got = append(got, f.Key)
		}
		sort.Strings(got)
		want := append([]string(nil), step.wants...)
		sort.Strings(want)
		if !equalKeys(got, want) {
			t.Errorf("%s: 울린 알람 %v, want %v", step.name, got, want)
		}
	}
}

// 알람이 복구까지 한 바퀴를 도는지 본다. 울리기만 하고 복구를 못 알리면
// 사람은 고쳐진 것을 모르고, 다음 알람을 옛 알람의 연장으로 읽는다.
func TestEndToEndResolveReachesNotifier(t *testing.T) {
	secret := []byte("s")
	st := newState(wantConsts(), secret)
	now := time.Now().UTC()

	bad := healthySnapshot()
	bad.Seq, bad.BootID, bad.TS = 1, "a", now
	bad.Loop.LastLoopAt = now
	bad.Exposure = beat.Exposure{Filled: 9, Cap: 8.62}
	if code := postBeat(t, st, secret, bad); code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if fire, _ := st.Step(now); len(fire) != 1 || fire[0].Key != "exposure" {
		t.Fatalf("울린 알람 = %+v, want exposure 1건", fire)
	}

	now = now.Add(3 * time.Second)
	ok := healthySnapshot()
	ok.Seq, ok.BootID, ok.TS = 2, "a", now
	ok.Loop.LastLoopAt = now
	if code := postBeat(t, st, secret, ok); code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	fire, resolved := st.Step(now)
	if len(fire) != 0 {
		t.Errorf("복구 beat 에 알람이 났다: %+v", fire)
	}
	if len(resolved) != 1 || resolved[0] != "exposure" {
		t.Errorf("복구 = %v, want [exposure]", resolved)
	}
}

// 봇이 조용해지면 알람이 나야 한다. **마지막 스냅샷의 미체결 목록이 붙어야
// 한다** — 개인키 없는 모니터는 `GET /v1/orders` 를 못 부르므로 그 목록이
// 사람이 지갑으로 확인할 유일한 근거다.
func TestEndToEndSilenceCarriesOpenOrders(t *testing.T) {
	secret := []byte("s")
	st := newState(wantConsts(), secret)
	now := time.Now().UTC()

	snap := healthySnapshot()
	snap.Seq, snap.BootID, snap.TS = 1, "a", now
	snap.Loop.LastLoopAt = now
	snap.Exposure.OpenOrders = []beat.OpenOrder{{ID: "0xabc", Tick: 41, Notional: 2.05}}
	if code := postBeat(t, st, secret, snap); code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	if fire, _ := st.Step(now); len(fire) != 0 {
		t.Fatalf("건강한 beat 에 알람이 났다: %+v", fire)
	}

	// 봇이 죽는다. 판정 시각만 흐르고 새 beat 는 오지 않는다.
	silent := now.Add(25 * time.Second)
	fire, _ := st.Step(silent)
	found := false
	for _, f := range fire {
		if f.Key == "stale" {
			found = true
			if !bytes.Contains([]byte(f.Message), []byte("0xabc")) {
				t.Errorf("무응답 알람에 미체결 주문이 없다: %q", f.Message)
			}
		}
	}
	if !found {
		t.Errorf("25초 무응답에 stale 이 울리지 않았다: %+v", fire)
	}
}

func equalKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- 거절 기록 ---

// recorder 는 거절 기록을 붙잡는다.
type recorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *recorder) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func (r *recorder) all() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.msgs, "\n")
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.msgs)
}

// **거절이 조용하면 운영자가 보는 것은 "스냅샷이 없다" 뿐이다.** 그 문구로는
// 비밀키 불일치·시계 어긋남·방화벽 차단이 전부 같아 보인다.
func TestRejectionsAreLogged(t *testing.T) {
	cases := []struct {
		name string
		req  func(*testing.T, *state) *http.Request
		want string
	}{
		{"서명 없음", func(t *testing.T, st *state) *http.Request {
			return httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader([]byte(`{}`)))
		}, "BEAT_SECRET"},
		{"틀린 비밀키로 서명", func(t *testing.T, st *state) *http.Request {
			body := []byte(`{"seq":1}`)
			req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
			req.Header.Set(beat.SigHeader, beat.Sign([]byte("틀린키"), body))
			return req
		}, "BEAT_SECRET"},
		{"서명은 맞는데 JSON 이 아니다", func(t *testing.T, st *state) *http.Request {
			body := []byte(`이건 JSON 이 아니다`)
			req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
			req.Header.Set(beat.SigHeader, beat.Sign([]byte("s"), body))
			return req
		}, "배포본이 갈렸다"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{}
			st := newState(wantConsts(), []byte("s"))
			st.logf = rec.logf
			st.handleBeat(httptest.NewRecorder(), c.req(t, st))
			if got := rec.all(); !strings.Contains(got, c.want) {
				t.Errorf("기록에 %q 가 없다: %q", c.want, got)
			}
		})
	}
}

// 재전송과 시계 어긋남은 대응이 완전히 다르다. 사유가 기록에 남아야 한다.
func TestReplayRejectionIsLoggedWithReason(t *testing.T) {
	rec := &recorder{}
	secret := []byte("s")
	st := newState(wantConsts(), secret)
	st.logf = rec.logf

	snap := healthySnapshot()
	snap.Seq, snap.BootID, snap.TS = 5, "a", time.Now().UTC()
	if code := postBeat(t, st, secret, snap); code != http.StatusOK {
		t.Fatalf("첫 beat code = %d", code)
	}
	if rec.count() != 0 {
		t.Fatalf("정상 beat 가 거절로 기록됐다: %q", rec.all())
	}
	// 같은 seq 를 다시 보낸다.
	if code := postBeat(t, st, secret, snap); code != http.StatusConflict {
		t.Fatalf("재전송 code = %d, want 409", code)
	}
	if got := rec.all(); got == "" {
		t.Error("재전송이 기록되지 않았다")
	}
}

// **스캐너가 저널을 채우면 진짜 사고의 기록이 회전으로 밀려나간다.**
// 이 엔드포인트는 공인 IP 에 열려 있다.
func TestRejectionLoggingIsRateLimited(t *testing.T) {
	rec := &recorder{}
	st := newState(wantConsts(), []byte("s"))
	st.logf = rec.logf
	for i := 0; i < 200; i++ {
		req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader([]byte(`{}`)))
		st.handleBeat(httptest.NewRecorder(), req)
	}
	if n := rec.count(); n != 1 {
		t.Errorf("같은 사유 200건이 %d줄로 기록됐다, want 1", n)
	}
}

// 간격이 지나면 다시 기록해야 한다. 한 번만 기록하고 영원히 침묵하면
// 지속되는 문제가 처음 1분에만 보인다.
func TestRejectionLoggingResumesAfterInterval(t *testing.T) {
	rec := &recorder{}
	st := newState(wantConsts(), []byte("s"))
	st.logf = rec.logf
	now := time.Now().UTC()
	st.noteReject(now, "1.2.3.4:1", "같은 사유")
	st.noteReject(now.Add(rejectLogEvery-time.Second), "1.2.3.4:1", "같은 사유")
	if rec.count() != 1 {
		t.Fatalf("간격 안인데 %d줄이다", rec.count())
	}
	st.noteReject(now.Add(rejectLogEvery+time.Second), "1.2.3.4:1", "같은 사유")
	if rec.count() != 2 {
		t.Errorf("간격이 지났는데 %d줄이다, want 2", rec.count())
	}
}

// 사유가 다르면 서로의 상한에 걸리지 않는다. 스캐너의 서명 실패가 봇의
// 시계 어긋남을 가려 버리면 안 된다.
func TestRejectionRateLimitIsPerReason(t *testing.T) {
	rec := &recorder{}
	st := newState(wantConsts(), []byte("s"))
	st.logf = rec.logf
	now := time.Now().UTC()
	st.noteReject(now, "1.2.3.4:1", "서명 불일치")
	st.noteReject(now, "10.0.0.1:2", "시계 어긋남")
	if rec.count() != 2 {
		t.Errorf("서로 다른 사유가 %d줄로 합쳐졌다, want 2", rec.count())
	}
}

// **비밀도 본문도 남기지 않는다.** 로그는 저널에 오래 남고, 남기는 순간
// 환경변수만 보호하던 것이 무의미해진다.
func TestRejectionLogNeverLeaksSecretOrBody(t *testing.T) {
	rec := &recorder{}
	secret := []byte("아주비밀스러운키")
	st := newState(wantConsts(), secret)
	st.logf = rec.logf
	body := []byte(`{"seq":1,"보안":"이본문은로그에없어야한다"}`)
	req := httptest.NewRequest(http.MethodPost, "/beat", bytes.NewReader(body))
	req.Header.Set(beat.SigHeader, beat.Sign([]byte("틀린키"), body))
	st.handleBeat(httptest.NewRecorder(), req)

	got := rec.all()
	if got == "" {
		t.Fatal("기록이 없다")
	}
	for _, bad := range []string{string(secret), "이본문은로그에없어야한다", "틀린키"} {
		if strings.Contains(got, bad) {
			t.Errorf("기록에 %q 가 새어 나왔다: %q", bad, got)
		}
	}
}

// **건너뛴 회차를 재현한다.**
//
// confidence 미달 회차는 `exec` 루프가 돌지 않으므로 봇이 새 스냅샷을 만들지
// 않는다. 그래도 봇은 3초마다 같은 내용을 계속 보낸다 — 살아 있다는 신호는
// 계속 와야 하기 때문이다. 모니터는 그것을 전부 받아들여야 한다. 실거래에서
// 여기가 무너져 회차 내내 🚨 가 울렸다(2026-08-11 11:15~11:20): 봇이 seq 를
// 발행 시점에 찍고 있어서 5분 내내 같은 seq 를 재전송했고, 재전송 방지가
// 그것을 전부 거부해 수신 시각이 갱신되지 않았다.
//
// 수신 시각은 handleBeat 이 실제 시계로 찍으므로 여기서 5분을 흉내 낼 수는
// 없다. 대신 **그 5분 동안 벌어지는 일**을 그대로 넣는다: 내용이 완전히 같은
// 스냅샷 100건, 달라지는 것은 요청에 속한 seq 와 TS 뿐.
func TestSkippedRoundBeatsStayAccepted(t *testing.T) {
	secret := []byte("s")
	st := newState(wantConsts(), secret)
	rec := &recorder{}
	st.logf = rec.logf

	before, _ := st.Latest()
	if before != nil {
		t.Fatal("시작 상태가 비어 있지 않다")
	}
	for i := 0; i < 100; i++ {
		snap := healthySnapshot()
		snap.BootID = "a"
		snap.TS = time.Now().UTC()
		snap.Seq = uint64(i + 1)
		snap.Loop.LastLoopAt = time.Time{} // 루프가 안 돈다 — 건너뛴 회차다
		snap.Round.State, snap.Round.SkipReason = beat.RoundSkipped, beat.SkipConfBelow

		if code := postBeat(t, st, secret, snap); code != http.StatusOK {
			t.Fatalf("%d번째 beat 가 %d 로 거부됐다 — 건너뛴 회차가 무응답으로 읽힌다", i, code)
		}
	}
	if got := rec.all(); got != "" {
		t.Errorf("거절 기록이 남았다: %q", got)
	}
	// 수신 시각이 갱신됐어야 한다. 갱신되지 않으면 20초 뒤 stale 이 운다.
	_, last := st.Latest()
	if age := time.Since(last); age > time.Second {
		t.Errorf("마지막 수신 시각이 %v 전이다 — 거부된 beat 는 시각을 갱신하지 않는다", age)
	}
	if fire, _ := st.Step(time.Now().UTC()); len(fire) != 0 {
		t.Errorf("건너뛴 회차에 알람이 났다: %+v", fire)
	}
	// 그리고 **연속 스킵이 세어져 있어야 한다** — 거부된 beat 는 세지 않는다.
	if n := st.ConsecSkips()[beat.SkipConfBelow]; n != 100 {
		t.Errorf("연속 스킵 %d회, want 100 — 받아들여지지 않은 beat 가 있다", n)
	}
}
