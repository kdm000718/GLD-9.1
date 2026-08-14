package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 이 파일이 지키는 것 넷:
//
//	(1) 적은 것이 **받은 바이트 그대로**다 — 파싱하지 않는다
//	(2) 큐가 차도 **막히지 않는다** — WS 읽기 고루틴이 디스크를 기다리면 안 된다
//	(3) 버린 줄을 **센다** — 조용한 유실은 "완전한 기록" 이라는 착각을 만든다
//	(4) 삭제는 **우리 파일에만** 닿는다

func readAll(t *testing.T, path string) []recLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("열기: %v", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer zr.Close()
	var out []recLine
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var l recLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatalf("줄 파싱: %v (%s)", err, sc.Text())
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("스캔: %v", err)
	}
	return out
}

// **원본 그대로 적는다.** predictTrades 는 스키마를 실측하지 않은 토픽이라,
// 지금 구조체를 만들면 그 추측이 기록에 굳는다. 바이트가 보존되면 스키마를
// 나중에 알아내도 과거 기록을 읽을 수 있다.
func TestRecorderWritesTheRawPayload(t *testing.T) {
	dir := t.TempDir()
	r, err := newRecorder(dir, nil)
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}
	go r.run()

	// 우리가 모르는 필드가 섞인 payload. 파싱했다면 살아남지 못한다.
	raw := json.RawMessage(`{"weirdField":[1,2,{"deep":"값"}],"price":"0.48","ts":1786695000123}`)
	at := time.Date(2026, 8, 14, 8, 10, 0, 123_000_000, time.UTC)
	r.observe(recTrade, 1354047, 2, at, raw)
	r.observe(recBook, 1354047, 2, at.Add(time.Second), json.RawMessage(`{"asks":[[0.55,10]],"bids":[[0.52,3]]}`))
	r.close()

	lines := readAll(t, filepath.Join(dir, recPrefix+"20260814"+recSuffix))
	if len(lines) != 2 {
		t.Fatalf("%d줄, 기대 2줄", len(lines))
	}
	if got := string(lines[0].Data); got != string(raw) {
		t.Errorf("payload = %s\n기대     %s — 바이트가 바뀌었다", got, raw)
	}
	if lines[0].Kind != recTrade || lines[1].Kind != recBook {
		t.Errorf("종류 = %q, %q", lines[0].Kind, lines[1].Kind)
	}
	if lines[0].MarketID != 1354047 || lines[0].Precision != 2 {
		t.Errorf("맥락이 빠졌다: %+v", lines[0])
	}
	if want := at.UnixMilli(); lines[0].RecvMS != want {
		t.Errorf("수신 시각 %d, 기대 %d", lines[0].RecvMS, want)
	}
	if w, d := r.stats(); w != 2 || d != 0 {
		t.Errorf("기록 %d / 버림 %d, 기대 2 / 0", w, d)
	}
}

// **호출자의 버퍼를 복사한다.** data 가 프레임 버퍼를 가리키는데 큐에 실린 뒤
// 다음 프레임이 그 자리를 덮으면 기록이 조용히 뒤섞인다 — 에러도 없이.
func TestRecorderCopiesTheCallersBuffer(t *testing.T) {
	dir := t.TempDir()
	r, err := newRecorder(dir, nil)
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}
	go r.run()

	buf := []byte(`{"v":1}`)
	at := time.Date(2026, 8, 14, 8, 10, 0, 0, time.UTC)
	r.observe(recBook, 1, 2, at, buf)
	copy(buf, []byte(`{"v":9}`)) // 호출자가 같은 자리를 덮어쓴다
	r.close()

	lines := readAll(t, filepath.Join(dir, recPrefix+"20260814"+recSuffix))
	if len(lines) != 1 || string(lines[0].Data) != `{"v":1}` {
		t.Errorf("기록 = %s, 기대 {\"v\":1} — 호출자 버퍼를 그대로 들고 있었다", lines[0].Data)
	}
}

// **막히면 안 된다.** 소비자가 없어도 observe 는 즉시 돌아와야 한다. WS 읽기
// 고루틴이 여기서 멈추면 호가창 갱신이 통째로 밀리고, 그건 거래에 닿는 피해다.
func TestObserveNeverBlocks(t *testing.T) {
	r := &recorder{ch: make(chan recLine, 2), done: make(chan struct{})} // run 을 돌리지 않는다
	at := time.Date(2026, 8, 14, 8, 10, 0, 0, time.UTC)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			r.observe(recBook, 1, 2, at, json.RawMessage(`{"v":1}`))
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("observe 가 막혔다 — 큐가 가득 차면 버려야 한다")
	}
	// 버린 것을 세야 한다. 조용히 버리면 기록이 완전하다고 믿고 분석하게 된다.
	if _, d := r.stats(); d != 98 {
		t.Errorf("버림 %d건, 기대 98건 (큐 2 + 버림 98)", d)
	}
}

// nil 기록기는 아무 일도 하지 않는다 — 기록을 못 켠 봇은 예전처럼 돈다.
func TestNilRecorderIsSafe(t *testing.T) {
	var r *recorder
	r.observe(recBook, 1, 2, time.Now(), json.RawMessage(`{}`))
	r.run()
	r.close()
	r.sweep("20260814")
	if w, d := r.stats(); w != 0 || d != 0 {
		t.Errorf("nil 인데 %d/%d", w, d)
	}
}

// 날짜가 바뀌면 파일도 바뀐다. 한 파일에 몰아 적으면 보관 삭제가 통째로
// 불가능해진다.
func TestRecorderRotatesByUTCDay(t *testing.T) {
	dir := t.TempDir()
	r, err := newRecorder(dir, nil)
	if err != nil {
		t.Fatalf("newRecorder: %v", err)
	}
	go r.run()
	// 23:59:59 UTC 와 00:00:01 UTC. 서버는 도쿄(UTC+9)이므로 로컬로 자르면
	// 두 줄이 같은 파일에 들어간다.
	r.observe(recBook, 1, 2, time.Date(2026, 8, 14, 23, 59, 59, 0, time.UTC), json.RawMessage(`{"a":1}`))
	r.observe(recBook, 1, 2, time.Date(2026, 8, 15, 0, 0, 1, 0, time.UTC), json.RawMessage(`{"a":2}`))
	r.close()
	for _, day := range []string{"20260814", "20260815"} {
		lines := readAll(t, filepath.Join(dir, recPrefix+day+recSuffix))
		if len(lines) != 1 {
			t.Errorf("%s: %d줄, 기대 1줄 — UTC 로 자르지 않았다", day, len(lines))
		}
	}
}

// ---------------------------------------------------------------------------
// 삭제 — 남의 파일에 손대면 안 된다
// ---------------------------------------------------------------------------

func TestRecFileDayAcceptsOnlyOurNames(t *testing.T) {
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"book-20260814.jsonl.gz", true},
		{"book-20251231.jsonl.gz", true},
		{"ledger.csv", false},
		{"gld91.log", false},
		{"book-2026081.jsonl.gz", false},   // 7자리
		{"book-202608144.jsonl.gz", false}, // 9자리
		{"book-abcdefgh.jsonl.gz", false},  // 날짜가 아니다
		{"book-20261301.jsonl.gz", false},  // 13월
		{"book-20260814.jsonl", false},     // 접미사 불일치
		{"book-20260814.jsonl.gz.bak", false},
		// **접두사만 보면 통과해 지워지는 이름들.** 접미사 검사가 있어야
		// 걸린다 — 길이가 recSuffix 와 같아서 가운데가 정확히 날짜가 된다.
		{"book-20260814.jsonl.GZ", false}, // 대소문자만 다르다
		{"book-20260814.JSONL.GZ", false},
		{"book-20260814ABCDEFGHI", false},
		// **접미사만 보면 통과하는 이름들.** 접두사 검사가 잡는다 — 앞 5자만
		// 다르고 나머지가 우리 형식과 똑같아서, 접두사를 안 보면 남의 파일이
		// 우리 기록으로 통과해 지워진다.
		{"dump-20260814.jsonl.gz", false},
		{"snap-20260814.jsonl.gz", false},
		{"trades-0260814.jsonl.gz", false},
		{"20260814.jsonl.gz", false}, // 접두사 없음
		{"", false},
	} {
		_, ok := recFileDay(c.name)
		if ok != c.ok {
			t.Errorf("%q: ok=%v, 기대 %v", c.name, ok, c.ok)
		}
	}
}

// 보관 기간을 넘긴 **우리 파일만** 지운다. 이 디렉터리에는 원장도 로그도
// 없지만, 삭제 코드가 그 가정에 기대면 경로가 바뀌는 날 지워지는 것이 달라진다.
func TestSweepRemovesOnlyOldOwnFiles(t *testing.T) {
	dir := t.TempDir()
	today := "20260814"
	files := map[string]bool{ // 이름 → 남아야 하나
		recPrefix + "20260814" + recSuffix: true,  // 오늘
		recPrefix + "20260716" + recSuffix: true,  // 29일 전 — 경계 안
		recPrefix + "20260715" + recSuffix: true,  // 30일 전 정각 — 안 지운다
		recPrefix + "20260714" + recSuffix: false, // 31일 전
		recPrefix + "20250101" + recSuffix: false, // 한참 전
		"ledger.csv":                       true,  // 남의 파일
		"book-notadate.jsonl.gz":           true,  // 이름이 안 맞는다
		"book-20260101.txt":                true,  // 접미사가 안 맞는다
		"dump-20250101.jsonl.gz":           true,  // 접두사가 안 맞는다 (충분히 오래됐다)
		"book-20250101.jsonl.GZ":           true,  // 대소문자만 다르다
	}
	for name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := &recorder{dir: dir}
	r.sweep(today)

	for name, want := range files {
		_, err := os.Stat(filepath.Join(dir, name))
		got := err == nil
		if got != want {
			t.Errorf("%s: 존재=%v, 기대 %v", name, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 배선 — 함수가 맞아도 호출부가 없으면 아무 일도 일어나지 않는다
// ---------------------------------------------------------------------------

func TestRecorderIsWired(t *testing.T) {
	src := mainSource(t)
	for _, want := range []struct{ needle, why string }{
		{`rt.rec.observe(recBook, marketID, t.round.Precision, recv, f.Msg.Data)`,
			"onFrame 이 오더북을 기록하지 않는다"},
		{`rt.rec.observe(recTrade, marketID, t.round.Precision, recv, f.Msg.Data)`,
			"onFrame 이 체결 스트림을 기록하지 않는다"},
		{`go rec.run()`, "기록 고루틴이 돌지 않는다 — 큐만 차다가 버려진다"},
		{`defer rec.close()`, "종료 시 버퍼가 비워지지 않는다 — 마지막 몇 분이 사라진다"},
		{`rt.rec = rec`, "기록기가 라우터에 꽂히지 않았다"},
	} {
		if !strings.Contains(src, want.needle) {
			t.Errorf("%s (찾는 것: %s)", want.why, want.needle)
		}
	}
	// **기록은 거래 판단에 닿지 않는다.** rec 를 읽어 무언가를 정하는 경로가
	// 생기면 그것은 다른 종류의 변경이다.
	for _, bad := range []string{"rt.rec.stats()", "rec.written", "rec.dropped"} {
		if strings.Contains(src, bad) {
			t.Errorf("main.go 가 기록기 상태를 읽는다 (%s) — 기록은 관측이지 조건이 아니다", bad)
		}
	}
}

// 체결 스트림은 기록기가 있을 때만 구독한다. 쓰지 않을 프레임을 받는 것은
// 대역폭이 아니라 잡음이다.
func TestTradesTopicOnlyWhenRecording(t *testing.T) {
	rt := newRouter()
	if got := rt.topics(7); len(got) != 1 || !strings.Contains(got[0], "predictOrderbook/7") {
		t.Errorf("기록기 없이 토픽 %v — 오더북 하나여야 한다", got)
	}
	rt.rec = &recorder{}
	got := rt.topics(7)
	if len(got) != 2 || !strings.Contains(got[0], "predictOrderbook/7") || !strings.Contains(got[1], "predictTrades/7") {
		t.Errorf("기록 중 토픽 %v — 오더북 + 체결이어야 한다", got)
	}
}

// 기록 디렉터리는 원장 옆이다. 둘이 갈리면 사후에 짝을 맞추는 사람이 경로를
// 두 개 알아야 한다.
func TestRecordDirSitsBesideTheLedger(t *testing.T) {
	c := &Config{LedgerPath: "/home/ubuntu/gld91/out/ledger.csv"}
	if got, want := c.recordDir(), "/home/ubuntu/gld91/out/book"; got != want {
		t.Errorf("recordDir = %q, 기대 %q", got, want)
	}
}
