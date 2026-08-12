package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/beat"
)

func tempStore(t *testing.T) *store {
	t.Helper()
	return &store{path: filepath.Join(t.TempDir(), "rounds.json"), logf: func(string, ...any) {}}
}

// 저장한 것을 그대로 읽는다.
func TestStoreRoundTrip(t *testing.T) {
	st := tempStore(t)
	want := []participation{
		{Slug: "btc-updown-5m-1786500000", Outcome: "Up", Cost: 3.29, Shares: 7, Settled: true, Won: true, WonName: "Up"},
		{Slug: "btc-updown-5m-1786500300", Outcome: "Up", Cost: 3.29, Shares: 7},
	}
	if err := st.save(want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got := st.load()
	if len(got) != len(want) {
		t.Fatalf("%d회차를 읽었다, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Slug != want[i].Slug || got[i].Won != want[i].Won ||
			got[i].Settled != want[i].Settled || got[i].Shares != want[i].Shares {
			t.Errorf("%d번째가 다르다: %+v vs %+v", i, got[i], want[i])
		}
	}
}

// **경로가 비면 아무것도 하지 않는다.** 저장 경로를 주지 않은 것은 "저장하지
// 마라" 는 뜻이고, 그때 에러를 내면 모니터가 뜨지 않는다.
func TestStoreDisabledIsSilent(t *testing.T) {
	var st *store
	if err := st.save([]participation{{Slug: "x"}}); err != nil {
		t.Errorf("nil 저장소가 에러를 냈다: %v", err)
	}
	if got := st.load(); len(got) != 0 {
		t.Errorf("nil 저장소가 이력을 냈다: %v", got)
	}
	empty := &store{}
	if err := empty.save([]participation{{Slug: "x"}}); err != nil {
		t.Errorf("빈 경로가 에러를 냈다: %v", err)
	}
}

// 깨진 파일에서도 뜬다. **다만 파일을 지우지 않는다** — 사람이 열어 볼 수
// 있어야 하고, 지워 버리면 누적이 0 이 된 이유가 사라진다.
func TestStoreSurvivesCorruptFileWithoutDeletingIt(t *testing.T) {
	st := tempStore(t)
	if err := os.WriteFile(st.path, []byte("{이건 JSON 이 아니다"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	st.logf = func(f string, a ...any) { fmt.Fprintf(&logged, f, a...) }

	if got := st.load(); len(got) != 0 {
		t.Errorf("깨진 파일에서 이력이 나왔다: %v", got)
	}
	if _, err := os.Stat(st.path); err != nil {
		t.Errorf("깨진 파일을 지웠다: %v", err)
	}
	if !strings.Contains(logged.String(), "깨졌다") {
		t.Errorf("깨진 것을 조용히 넘겼다: %q", logged.String())
	}
}

// 슬러그 없는 항목은 버린다 — 맞출 열쇠가 없어 정산이 절대 붙지 않는데
// 누적 참여 수만 늘린다.
func TestStoreDropsEntriesWithoutSlug(t *testing.T) {
	st := tempStore(t)
	if err := st.save([]participation{{Slug: ""}, {Slug: "btc-updown-5m-1786500000"}}); err != nil {
		t.Fatal(err)
	}
	got := st.load()
	if len(got) != 1 || got[0].Slug == "" {
		t.Errorf("슬러그 없는 항목이 살아남았다: %+v", got)
	}
}

// **쓰다가 죽어도 예전 파일이 남는다.** 제자리에서 덮어쓰면 반쪽짜리 JSON 이
// 남고, 다음 기동은 그것을 깨진 파일로 읽어 이력을 통째로 잃는다.
func TestStoreWriteIsAtomic(t *testing.T) {
	st := tempStore(t)
	first := []participation{{Slug: "btc-updown-5m-1786500000", Shares: 7}}
	if err := st.save(first); err != nil {
		t.Fatal(err)
	}
	// 대상 경로를 디렉터리로 바꿔 rename 을 실패시킨다.
	if err := os.Remove(st.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(st.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := st.save([]participation{{Slug: "btc-updown-5m-1786500300"}}); err == nil {
		t.Fatal("실패해야 하는 저장이 성공했다")
	}
	// 임시 파일이 남아 디렉터리를 어지럽히면 안 된다.
	ents, err := os.ReadDir(filepath.Dir(st.path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("임시 파일이 남았다: %s", e.Name())
		}
	}
}

// ---------------------------------------------------------------------------
// 상태에 붙는 부분
// ---------------------------------------------------------------------------

// **재기동해도 누적이 이어진다.** 이것이 이 파일이 존재하는 이유다.
func TestCumulativeSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rounds.json")

	before := newTestState()
	before.attachStore(&store{path: path, logf: func(string, ...any) {}})
	playRounds(t, before, 4, 3, 2)

	// 새 프로세스가 뜬 것과 같다 — 메모리는 비었고 파일만 있다.
	after := newTestState()
	after.attachStore(&store{path: path, logf: func(string, ...any) {}})

	reply, _ := routeCommand("/status", after, at())
	if !strings.Contains(reply, "누적 참여 4회차") {
		t.Errorf("재기동에서 참여 회차를 잃었다:\n%s", reply)
	}
	if !strings.Contains(reply, "적중 2/3") {
		t.Errorf("재기동에서 승률을 잃었다:\n%s", reply)
	}
}

// 파일이 없어도 그냥 뜬다(첫 기동).
func TestAttachStoreWithNoFileStartsEmpty(t *testing.T) {
	st := newTestState()
	st.attachStore(&store{path: filepath.Join(t.TempDir(), "none.json"), logf: func(string, ...any) {}})
	reply, _ := routeCommand("/status", stateWithStore(t, st), at())
	if !strings.Contains(reply, "누적 참여 0회차") {
		t.Errorf("첫 기동이 0 으로 시작하지 않았다:\n%s", reply)
	}
}

// stateWithStore 는 이미 만든 상태에 스냅샷 하나를 넣어 /status 가 답하게 한다.
func stateWithStore(t *testing.T, st *state) *state {
	t.Helper()
	s := healthySnapshot()
	s.Seq, s.BootID = 1, "a"
	if code, _ := post(t, st, *s); code != 200 {
		t.Fatalf("스냅샷 주입 실패: %d", code)
	}
	return st
}

// **beat 로 방금 본 것이 파일보다 새롭다.** 파일이 덮으면 재기동 직후에
// 진행 중이던 회차의 체결이 옛 값으로 되돌아간다.
func TestAttachStoreDoesNotOverwriteFresherMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rounds.json")
	old := &store{path: path, logf: func(string, ...any) {}}
	if err := old.save([]participation{{Slug: "btc-updown-5m-1786500000", Shares: 1, Cost: 0.47}}); err != nil {
		t.Fatal(err)
	}
	st := newTestState()
	playRounds(t, st, 1, 0, 0) // 같은 슬러그를 7주로 관측
	st.attachStore(old)

	for _, p := range st.Participations() {
		if p.Slug == "btc-updown-5m-1786500000" && p.Shares != 7 {
			t.Errorf("파일이 새 관측을 덮었다: %+v", p)
		}
	}
}

// 이력이 실제로 디스크에 쓰인다 — beat 만으로 파일이 생겨야 한다.
func TestBeatPersistsParticipation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rounds.json")
	st := newTestState()
	st.attachStore(&store{path: path, logf: func(string, ...any) {}})

	s := healthySnapshot()
	s.Seq, s.BootID = 1, "a"
	s.Round.Slug = "btc-updown-5m-1786500000"
	s.Exposure.Filled, s.Exposure.FilledShares = 3.29, 7
	if code, _ := post(t, st, *s); code != 200 {
		t.Fatalf("주입 실패: %d", code)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("beat 뒤에 이력 파일이 없다: %v", err)
	}
	var ps []participation
	if err := json.Unmarshal(b, &ps); err != nil {
		t.Fatalf("이력 파일이 JSON 이 아니다: %v", err)
	}
	if len(ps) != 1 || ps[0].Shares != 7 {
		t.Errorf("디스크의 이력이 다르다: %+v", ps)
	}
}

// **걸지 않은 회차는 디스크에 쓰지 않는다.** 3초마다 오는 beat 마다 파일을
// 쓰면 아무것도 새로 담지 않는 쓰기가 하루 3만 번이다.
func TestUnchangedBeatDoesNotRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rounds.json")
	st := newTestState()
	st.attachStore(&store{path: path, logf: func(string, ...any) {}})

	// **스냅샷 하나를 만들어 seq 만 바꿔 보낸다.** healthySnapshot 은 부를
	// 때마다 EndsAt 을 새 시각으로 잡으므로, 매번 새로 만들면 내용이 실제로
	// 달라져 이 시험이 재기록을 잡지 못한다.
	base := healthySnapshot()
	base.BootID = "a"
	base.Round.Slug = "btc-updown-5m-1786500000"
	base.Exposure.Filled, base.Exposure.FilledShares = 3.29, 7
	send := func(seq uint64) {
		s := *base
		s.Seq = seq
		if code, _ := post(t, st, s); code != 200 {
			t.Fatalf("주입 실패: %d", code)
		}
	}
	send(1)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	first := fi.ModTime()

	// 같은 내용의 beat 를 더 보낸다. 파일이 다시 쓰이면 안 된다.
	for seq := uint64(2); seq <= 5; seq++ {
		send(seq)
	}
	fi2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi2.ModTime().Equal(first) {
		t.Error("바뀐 것이 없는데 이력을 다시 썼다")
	}
}

// 정산이 붙으면 그것도 디스크에 남는다 — 정산은 되돌아오지 않는다.
func TestSettlementPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rounds.json")
	st := newTestState()
	st.attachStore(&store{path: path, logf: func(string, ...any) {}})
	playRounds(t, st, 1, 0, 0)

	slug := "btc-updown-5m-1786500000"
	if got := st.ApplySettlements([]settlement{{Slug: slug, WonName: "Up", SettledAt: at()}}); got != 1 {
		t.Fatalf("반영 %d건", got)
	}
	st.persist()

	reloaded := &store{path: path, logf: func(string, ...any) {}}
	ps := reloaded.load()
	if len(ps) != 1 || !ps[0].Settled || !ps[0].Won {
		t.Errorf("정산이 디스크에 남지 않았다: %+v", ps)
	}
}

// 저장소가 없어도 예전처럼 돈다 — 이력은 편의이고 하트비트 감시가 본체다.
func TestMonitorWorksWithoutStore(t *testing.T) {
	st := newTestState()
	playRounds(t, st, 2, 1, 1)
	st.persist() // 패닉하지 않아야 한다
	reply, _ := routeCommand("/status", st, at())
	if !strings.Contains(reply, "누적 참여 2회차") {
		t.Errorf("저장소 없이 누적이 깨졌다:\n%s", reply)
	}
	var _ beat.Snapshot
}
