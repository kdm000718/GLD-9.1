package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 호가창·체결 기록기 — 거래에 닿지 않는 순수 관측 경로
// ---------------------------------------------------------------------------
//
// # 왜 — 가격을 배포하지 않고 시험하기 위해서다
//
// 이 봇의 유일한 가격 결정은 [exec.LimitPrice] 하나이고, 그 값이 옳은지는
// **체결률**이 정한다. 그런데 체결률은 회차 시작의 호가 한 장으로는 알 수 없다 —
// 5분 동안 시장이 그 가격까지 내려왔는가, 내려왔을 때 우리 앞에 몇 주가 있었는가
// 하는 **경로**의 문제다.
//
// 그것을 몰라서 실제로 돈을 잃었다. 2026-08-14 에 지정가를 0.47 에서 0.46 으로
// 한 틱 내렸더니 체결분 승률이 49.5% 에서 36.0% 로 무너졌다(99회차, -42 USDT).
// 경로 기록이 있었다면 그 답을 한 푼도 잃지 않고 알 수 있었다.
//
// 기록이 쌓이면 **한 표본에서 모든 가격을 동시에 시험한다** — 0.44 부터 0.50 까지
// 각각의 체결률과 체결분 승률을. 배포 한 번에 값 하나씩 실측하는 것과 비교하면
// 회차 수가 아니라 가격 개수만큼 빨라진다.
//
// # 왜 원본 그대로 적는가
//
// 파싱하지 않는다. 프레임의 payload 를 **받은 바이트 그대로** 적는다.
//
// 이 저장소는 필드 이름을 잘못 짚는 실패를 이미 겪었다(POST /v1/auth/jwt 의
// "address", 오더북의 decimalPrecision). `predictTrades` 는 문서에 없는 토픽이라
// 스키마를 아직 실측하지 않았다 — 지금 구조체를 만들면 그 추측이 그대로 기록에
// 굳는다. 원본을 적어 두면 스키마를 나중에 알아내도 과거 기록을 다시 읽을 수 있다.
//
// 오더북도 같은 이유로 원본이다. 게다가 **봇이 본 것과 기록이 갈릴 여지가 없다** —
// 파싱을 두 번 하면 두 결과가 다른 날이 온다.
//
// # 거래를 절대 방해하지 않는다
//
// 셋을 지킨다:
//
//	비차단      [recorder.observe] 는 버퍼가 가득 차면 **버린다**. WS 읽기
//	            고루틴이 디스크를 기다리는 일은 없다.
//	무해한 실패  파일을 못 열어도 봇은 그대로 돈다. 기록기가 nil 이어도 모든
//	            메서드가 안전하다.
//	무관         회차 판단·주문·노출 어디에서도 이 파일의 값을 읽지 않는다.
//
// 버린 프레임 수는 세어서 기동·종료에 남긴다. **조용히 버리면 기록이 완전하다고
// 착각한 채 분석하게 된다** — 그것이 이 파일이 막으려는 바로 그 실패다.

const (
	// recQueue 는 쓰기 대기 버퍼다. 프레임은 초당 한두 개이므로 이 크기면
	// 디스크가 몇 분 멈춰도 버리지 않는다.
	recQueue = 4096

	// recRetainDays 는 보관 일수다. 하루 25MB 안팎을 예상하므로 30일이면
	// 1GB 미만이다. 서버 여유는 9GB 이상이다(2026-08-14 실측).
	recRetainDays = 30

	// recPrefix·recSuffix 는 우리가 만든 파일을 알아보는 표식이다.
	// **삭제는 이 두 표식이 모두 맞는 파일에만 한다.**
	recPrefix = "book-"
	recSuffix = ".jsonl.gz"
)

// recFlushEvery 는 gzip 버퍼를 비우는 주기다.
//
// **없으면 파일이 프로세스가 죽을 때까지 읽히지 않는다.** gzip.Writer 는
// 수만 바이트를 물고 있다가 Close 에서야 내놓는다. 그 상태에서 봇이 크래시하면
// 마지막 버퍼가 통째로 사라지고, 살아 있는 동안에도 `zcat` 이 "unexpected end
// of file" 로 끝난다 — 오늘 치를 오늘 분석할 수 없다는 뜻이다.
//
// 5초면 크래시 손실이 5초치(십여 줄)로 묶인다. 압축률은 조금 떨어지지만,
// 하루 십만 줄에서 몇 %다.
//
// var 인 것은 **시험이 줄이기 위해서**다. 운영 코드는 이 값을 바꾸지 않는다.
var recFlushEvery = 5 * time.Second

// recKind 는 한 줄이 무엇인지다. 짧은 것이 요점이다 — 하루 십만 줄이다.
const (
	recBook  = "b" // predictOrderbook 스냅샷 (전체 스냅샷이다, 델타가 아니다)
	recTrade = "x" // predictTrades (스키마 미실측 — 원본 그대로)
)

// recLine 은 큐에 실리는 한 줄이다.
type recLine struct {
	Kind      string          `json:"t"`
	RecvMS    int64           `json:"ms"`
	MarketID  int64           `json:"m"`
	Precision int             `json:"p,omitempty"`
	Data      json.RawMessage `json:"d"`
}

// recorder 는 프레임을 날짜별 gzip JSONL 로 적는다.
//
// 제로값은 쓰지 않는다 — [newRecorder] 를 쓰거나 nil 이어야 한다. nil 은
// "기록하지 않는다" 이고 모든 메서드가 안전하다.
type recorder struct {
	dir string
	ch  chan recLine
	log func(string, ...any)

	dropped atomic.Int64
	written atomic.Int64

	done chan struct{}
}

// newRecorder 는 dir 에 기록하는 기록기를 만든다.
//
// dir 을 만들 수 없으면 **nil 과 에러**를 돌려준다. 호출자는 그 에러를 로그로만
// 다뤄야 한다 — 기록은 관측이지 거래 조건이 아니다.
func newRecorder(dir string, log func(string, ...any)) (*recorder, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("기록 디렉터리 %s: %w", dir, err)
	}
	return &recorder{
		dir:  dir,
		ch:   make(chan recLine, recQueue),
		log:  log,
		done: make(chan struct{}),
	}, nil
}

func (r *recorder) logf(format string, args ...any) {
	if r != nil && r.log != nil {
		r.log(format, args...)
	}
}

// observe 는 프레임 한 장을 큐에 넣는다. **절대 막히지 않는다.**
//
// 큐가 가득 차면 버리고 센다. WS 읽기 고루틴이 여기서 멈추면 호가창 갱신이
// 통째로 밀리고, 그건 거래에 닿는 피해다 — 기록 하나 잃는 것과 비교할 수 없다.
func (r *recorder) observe(kind string, marketID int64, precision int, recv time.Time, data json.RawMessage) {
	if r == nil || len(data) == 0 {
		return
	}
	// **복사한다.** data 는 프레임 버퍼를 가리킬 수 있고, 큐에 실린 뒤 다음
	// 프레임이 그 자리를 덮으면 기록이 조용히 뒤섞인다.
	cp := make(json.RawMessage, len(data))
	copy(cp, data)
	line := recLine{Kind: kind, RecvMS: recv.UTC().UnixMilli(), MarketID: marketID, Precision: precision, Data: cp}
	select {
	case r.ch <- line:
	default:
		r.dropped.Add(1)
	}
}

// run 은 큐를 파일로 흘린다. ctx 가 아니라 [recorder.close] 로 끝낸다 —
// 종료 시 버퍼에 남은 줄을 마저 적어야 하기 때문이다.
func (r *recorder) run() {
	if r == nil {
		return
	}
	defer close(r.done)
	var (
		f    *os.File
		zw   *gzip.Writer
		day  string
		errs int
	)
	closeCur := func() {
		if zw != nil {
			_ = zw.Close()
			zw = nil
		}
		if f != nil {
			_ = f.Close()
			f = nil
		}
	}
	defer closeCur()

	tick := time.NewTicker(recFlushEvery)
	defer tick.Stop()

	for {
		var line recLine
		select {
		case <-tick.C:
			// 큐가 조용한 사이에 버퍼를 내보낸다. **여기서 실패해도 계속
			// 간다** — 다음 쓰기가 같은 실패를 다시 만난다.
			if zw != nil {
				_ = zw.Flush()
			}
			continue
		case l, ok := <-r.ch:
			if !ok {
				return
			}
			line = l
		}
		d := time.UnixMilli(line.RecvMS).UTC().Format("20060102")
		if d != day {
			closeCur()
			path := filepath.Join(r.dir, recPrefix+d+recSuffix)
			nf, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				// 오늘 치를 못 열면 이 줄은 버린다. 다음 줄에서 다시 시도한다 —
				// 디스크가 잠깐 찼다가 비는 경우가 있다.
				if errs < 3 {
					r.logf("기록 파일 열기 실패 (%s): %v", path, err)
				}
				errs++
				r.dropped.Add(1)
				continue
			}
			f, zw, day, errs = nf, gzip.NewWriter(nf), d, 0
			r.logf("호가 기록: %s", path)
			r.sweep(d)
		}
		b, err := json.Marshal(line)
		if err != nil {
			r.dropped.Add(1)
			continue
		}
		if _, err := zw.Write(append(b, '\n')); err != nil {
			if errs < 3 {
				r.logf("기록 쓰기 실패: %v", err)
			}
			errs++
			r.dropped.Add(1)
			continue
		}
		r.written.Add(1)
	}
}

// sweep 은 보관 기간을 넘긴 기록을 지운다.
//
// **우리가 만든 이름에만 손댄다.** 접두사와 접미사가 모두 맞고, 가운데가 정확히
// 8자리 날짜여야 한다. 이 디렉터리에는 원장도 로그도 없지만, 삭제 코드가 그
// 가정에 기대면 안 된다 — 경로 하나가 바뀌는 날 지워지는 것이 달라진다.
func (r *recorder) sweep(today string) {
	if r == nil {
		return
	}
	cutoff, err := time.Parse("20060102", today)
	if err != nil {
		return
	}
	cutoff = cutoff.AddDate(0, 0, -recRetainDays)
	ents, err := os.ReadDir(r.dir)
	if err != nil {
		return
	}
	var old []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		d, ok := recFileDay(e.Name())
		if !ok || !d.Before(cutoff) {
			continue
		}
		old = append(old, e.Name())
	}
	sort.Strings(old)
	for _, name := range old {
		if err := os.Remove(filepath.Join(r.dir, name)); err != nil {
			r.logf("오래된 기록 삭제 실패 (%s): %v", name, err)
			continue
		}
		r.logf("오래된 기록 삭제: %s (보관 %d일)", name, recRetainDays)
	}
}

// recFileDay 는 파일 이름이 우리 기록인지 보고, 맞으면 그 날짜를 돌려준다.
//
// [recorder.sweep] 이 삭제 대상을 고르는 유일한 판정이다. **여기가 느슨하면
// 남의 파일이 지워진다.** 그래서 접두사와 접미사를 **둘 다** 본다:
//
//	접두사만 보면   "book-20260814.jsonl.GZ"(대소문자만 다른 파일) 가 우리
//	                기록으로 통과해 지워진다. 실제로 대소문자를 구분하지 않는
//	                파일시스템에서 복사해 오면 그런 이름이 생긴다.
//	접미사만 보면   남이 만든 "trades-20260814.jsonl.gz" 가 통과한다.
//
// 가운데는 time.Parse 가 판정한다. 형식 "20060102" 는 **정확히 8자리**만
// 받는다 — 7자리는 "cannot parse", 9자리는 "extra text", 13월은 "month out
// of range" 다(2026-08-14 실측). 그래서 길이 검사를 따로 두지 않는다:
// 지울 수 있는 코드는 지운다.
func recFileDay(name string) (time.Time, bool) {
	if !strings.HasPrefix(name, recPrefix) || !strings.HasSuffix(name, recSuffix) {
		return time.Time{}, false
	}
	d, err := time.Parse("20060102", name[len(recPrefix):len(name)-len(recSuffix)])
	if err != nil {
		return time.Time{}, false
	}
	return d, true
}

// close 는 큐를 닫고 남은 줄이 다 적힐 때까지 기다린다.
func (r *recorder) close() {
	if r == nil {
		return
	}
	close(r.ch)
	<-r.done
	w, d := r.written.Load(), r.dropped.Load()
	r.logf("호가 기록 종료 — %d줄 기록, %d줄 버림", w, d)
	if d > 0 {
		// **버림은 조용히 넘어가면 안 된다.** 기록이 완전하다고 믿고 분석하는
		// 것이 이 파일이 막으려는 실패다.
		r.logf("⚠️ 버린 줄이 있다 — 그 구간의 체결률 분석은 불완전하다")
	}
}

// stats 는 감시·로그용 누적이다.
func (r *recorder) stats() (written, dropped int64) {
	if r == nil {
		return 0, 0
	}
	return r.written.Load(), r.dropped.Load()
}
