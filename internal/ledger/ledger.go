// Package ledger 는 체결·리베이트·정산을 CSV 로 기록하고, **손익 부호 규약을
// 한 곳에 못박는다.**
//
// # 왜 이 패키지가 존재하는가
//
// `~/kdm/pmmm-go` 에서 손익 부호를 뒤집어 실제 +40 인 전략이 −90 으로 보고된
// 전례가 있다. 백테스트가 흑자라고 말하는데 실거래가 적자인 상태가 그렇게
// 만들어진다. 부호는 코드 세 줄이지만 그 세 줄이 틀리면 나머지 전부가 무의미
// 하므로, 세 줄을 이 패키지에 모으고 각각에 회귀 테스트를 붙였다.
//
// 규약은 셋이다:
//
//   - [FillCost] — **양수 = 나간 돈.** 이 봇은 매수만 하므로 체결은 언제나 지출이다.
//   - [SettlementProceeds] — **양수 = 들어온 돈.** 이기면 주당 $1.0, 지면 0.
//   - [RebateValue] — **양수 = 들어온 돈.** 단, 방향이 직관과 반대다(아래).
//
// 세 함수 모두 "우리 현금 기준 절대값"을 돌려준다. 순손익은 호출자가
// `-FillCost + SettlementProceeds + RebateValue` 로 조립한다. 함수가 부호까지
// 섞어 돌려주면 어느 쪽이 이미 부호를 먹었는지 호출자가 헷갈리는데, 그 헷갈림이
// pmmm-go 에서 실제로 일어난 사고다.
//
// # 리베이트 방향이 직관과 반대인 이유
//
// predict.fun 의 메이커 리베이트는 USDT 가 아니라 **반대편 outcome 주식**으로
// 지급된다(2026-08-09 사용자 확인, 스펙 §4). Up 을 `N` 주 사면 Down 을
// `0.005·N` 주 받는다. CTF 이진 시장에서 Up+Down=1 이므로 그 Down 주식은
// **우리가 질 때만** 값이 붙는다:
//
//	우리가 이김 → 우리 Up 주식이 $1, 리베이트 Down 주식은 $0
//	우리가 짐   → 우리 Up 주식이 $0, 리베이트 Down 주식이 $1
//
// G2 게이트가 이 규칙으로 리베이트 기여를 `0.005·(1−q)/p` = +0.487%p 로
// 계산했다(현금 리베이트 가정 +0.500%p 보다 낮다 — 엣지가 좋을수록 리베이트를
// 덜 받는다). 방향을 뒤집으면 그 엣지 계산이 통째로 무너진다.
//
// # 망가진 입력에서의 방향 — 기록 경로와 계산 경로가 다르다
//
// 두 경로의 실패 방향이 의도적으로 다르다.
//
// **기록 경로(Record*)는 거절한다.** NaN·Inf·음수 수량 같은 값이 오면 줄을
// 쓰지 않고 [ErrInvalidRecord] 를 돌려준다. CSV 에 `NaN` 을 그대로 쓰면
// `strconv.ParseFloat("NaN")` 도 pandas 도 그것을 조용히 받아들여, 나중에 이
// 파일로 손익을 집계하는 쪽이 오염된 사실조차 모른 채 NaN 을 퍼뜨린다. 파일은
// 크래시 복구(Task 8)가 읽는 유일한 과거 기록이므로 깨끗하게 유지한다. 기록이
// 빠지면 복구 대조가 불일치를 보고 멈추는데, 그것은 **안전한 방향**이다 —
// 원장이 포지션을 실제보다 적게 말하면 봇은 덜 걸지 더 걸지 않는다.
//
// **계산 경로(FillCost 등)는 전파한다.** NaN 이 들어오면 NaN 이 나온다. 0 으로
// 삼키면 망가진 피드가 "손익 0" 으로 보이는데, 그것이야말로 pmmm-go 사고의
// 모양이다. NaN 은 하류에서 막힌다 — `risk.DailyLimit.Breached(NaN)` 는 true
// (거래 차단)를 돌려준다. 그 연결을 테스트로 고정해 두었다.
//
// # 패닉하지 않는다
//
// `internal/risk` 와 같은 원칙이다. 이 함수들은 살아 있는 주문을 들고 있는
// 상태에서 불린다. 기록에 실패했다고 패닉하면 취소도 못 하고 죽는다. 에러를
// 돌려주고 판단은 호출자(`internal/exec`)에게 맡긴다.
package ledger

import (
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"
)

// Outcome 값. 문자열 리터럴이 코드 여기저기에 흩어지면 "up" 과 "Up" 이 같은
// 파일에 섞여 회차 집계가 조용히 둘로 갈린다.
const (
	OutcomeUp   = "Up"
	OutcomeDown = "Down"
)

// RebateShareRate 는 메이커 리베이트 비율이다 — **주식 수 기준**이지 USDT
// 명목 기준이 아니다(스펙 §4). Up 을 N 주 사면 Down 을 RebateShareRate·N 주
// 받는다.
//
// 이 상수는 기대값 문서화용이다. 실제 지급 수량은 거래소가 정하므로
// [Rebate] 에는 **받은 수량을 그대로** 넣는다. 여기서 곱해서 만들어 넣으면
// 거래소가 실제로 준 것이 아니라 우리가 그럴 것이라 믿는 값을 원장에 쓰는
// 것이 된다.
//
// `shareThreshold`(리베이트 자격선) 가설은 사이징에 반영하지 않는다 —
// 계획서 "미확정 값" 절. 가설이 틀려도 리베이트를 조금 못 받을 뿐이고,
// 엣지는 리베이트 없이도 +1.974%p 양수다.
const RebateShareRate = 0.005

// 에러 분류. 호출자(`exec`)가 둘을 구분해야 한다 — 데이터가 망가진 것과
// 디스크가 망가진 것은 대응이 다르다. 전자는 그 레코드를 버리고 경보하면
// 되지만, 후자는 이후 모든 기록이 실패하므로 무장을 풀어야 한다.
//
// # 재시도에 대하여
//
// **분류가 곧 재시도 가능 여부다.** 아래 세 센티널 중 하나면 파일은 전혀
// 손대지 않았다 — 같은 값으로 다시 불러도 결과는 같고, 줄이 중복되지 않는다.
//
// 그 밖의 에러(디스크 가득 참, I/O 실패)는 **줄이 이미 들어갔을 수도 있다.**
// [Ledger] 는 append 전용이라 쓰다 만 것을 되돌릴 수단이 없고, 실패 지점이
// 커널 안이면 어디까지 나갔는지 우리가 알 방법도 없다. 그러니 그런 에러에서
// 같은 레코드를 다시 넣지 마라 — 중복된 체결 줄은 크래시 복구가 포지션을
// 실제보다 크게 읽게 만들고, 그것은 한도를 넘겨 베팅하는 방향이다. 대응은
// 재시도가 아니라 무장 해제다.
var (
	// ErrInvalidRecord 는 레코드 자체가 원장에 쓸 수 없는 값이라는 뜻이다.
	// 파일은 손대지 않았다 — 재시도해도 같은 결과다.
	ErrInvalidRecord = errors.New("ledger: 기록할 수 없는 레코드")
	// ErrClosed 는 이미 닫힌 원장에 쓰려 했다는 뜻이다.
	ErrClosed = errors.New("ledger: 이미 닫힌 원장")
	// ErrNotOpen 은 [Open] 을 지나지 않은 원장에 쓰려 했다는 뜻이다 —
	// 대개 nil 필드다. Task 7 의 Runner 는 원장을 포인터 필드로 들고
	// 있으므로, 배선이 그 필드를 빠뜨리면 여기로 들어온다. 그 경우에
	// nil 역참조로 죽는 대신 에러를 돌려준다: 살아 있는 주문을 들고 있는
	// 상태에서 패닉하면 취소도 못 하고 죽는다.
	ErrNotOpen = errors.New("ledger: 열리지 않은 원장")
)

// Fill 은 체결 한 건이다. 이 봇은 매수만 하므로 언제나 지출이다.
type Fill struct {
	RoundStart int64
	MarketID   int64
	Outcome    string // "Up" | "Down"
	Shares     float64
	PriceUSD   float64
	FeeUSD     float64 // 우리가 낸 수수료. 양수면 지출.
	At         time.Time
}

// Rebate 는 메이커 리베이트 지급 한 건이다.
//
// Shares 는 **반대편** outcome 주식 수다. 어느 쪽인지 적는 필드가 없는 것은
// 의도다 — 반대편은 같은 회차의 [Fill] 이 결정하므로, 여기에 또 적으면 두
// 기록이 어긋날 수 있는 자리가 하나 더 생긴다.
type Rebate struct {
	RoundStart int64
	Shares     float64 // 반대편 주식 수
	At         time.Time
}

// Settlement 는 회차 정산 결과다.
//
// Won 은 **우리** 포지션이 이겼는가다. 시장이 Up 으로 정산됐는가가 아니다 —
// 우리가 Down 을 들고 있으면 Up 정산은 패배다. [RebateValue] 의 두 번째 인자와
// 같은 값이다.
type Settlement struct {
	RoundStart int64
	Won        bool
	Shares     float64
	At         time.Time
}

// ---------------------------------------------------------------------------
// 부호 규약 — 이 세 함수가 이 패키지의 존재 이유다.
// ---------------------------------------------------------------------------

// FillCost 는 이 체결로 **나간** 돈이다. 양수 = 지출.
//
// 수수료는 **더한다**. 빼면 수수료가 수익으로 잡혀 손실 전략이 흑자로 보인다 —
// pmmm-go 에서 테이커 수수료 부호를 놓쳐 +40 이 −90 이 된 사고가 정확히 이
// 모양이었다.
//
// 검사하지 않는다. NaN 이 들어오면 NaN 이 나간다 — 패키지 문서의 "계산 경로는
// 전파한다" 참조.
func FillCost(f Fill) float64 { return f.Shares*f.PriceUSD + f.FeeUSD }

// SettlementProceeds 는 정산으로 **들어온** 돈이다. 양수 = 수입.
//
// 이긴 주식은 주당 정확히 $1.0 이고 진 주식은 $0 이다. 중간값은 없다 — CTF
// 이진 시장의 정의다. 매수 가격을 여기서 빼지 않는다. 그것은 이미
// [FillCost] 가 셌고, 여기서 또 빼면 두 번 차감된다.
func SettlementProceeds(s Settlement) float64 {
	if !s.Won {
		return 0
	}
	return s.Shares * 1.0
}

// RebateValue 는 리베이트 주식이 정산에서 만들어낸 **수입**이다. 양수 = 수입.
//
// weWon 은 **우리** 포지션이 이겼는가다. 계획서는 이 인자를 `settledUp` 이라고
// 적었지만 그 이름은 틀렸다 — 세 가지 근거가 있다:
//
//  1. 계획서 자신의 테스트(TestRebateOnlyPaysWhenWeLose)가 `true` 에 0 을
//     기대하면서 "우리가 이겼는데"라고 적는다.
//  2. 계획서의 같은 줄 주석이 "우리가 졌을 때 1.0, 이겼을 때 0" 이다.
//  3. [Rebate] 에는 우리가 어느 쪽을 샀는지 적는 필드가 없다. 그러니 이
//     함수는 "시장이 Up 으로 정산됐다"만 받아서는 답을 낼 수 없다 — 우리가
//     Down 을 들고 있었다면 Up 정산은 패배이고 리베이트(Up 주식)는 $1 이다.
//
// 즉 이름만 틀렸고 의미는 계획서 안에서 일관된다. 호출자는 [Settlement.Won] 을
// 그대로 넘기면 된다.
//
// **방향이 직관과 반대다.** 리베이트는 반대편 주식이므로 우리가 이기면 그
// 주식은 0 이 되고, 우리가 져야 주당 $1.0 이 된다. 뒤집으면 G2 의 +0.487%p
// 계산이 무너진다 — 패키지 문서 참조.
func RebateValue(r Rebate, weWon bool) float64 {
	if weWon {
		// 우리가 이겼다 = 반대편이 졌다 = 리베이트 주식은 휴지가 된다.
		return 0
	}
	return r.Shares * 1.0
}

// ---------------------------------------------------------------------------
// CSV 원장
// ---------------------------------------------------------------------------

// 레코드 종류. CSV 한 파일에 세 종류가 섞이므로 판별 열이 필요하다.
const (
	recordFill       = "fill"
	recordRebate     = "rebate"
	recordSettlement = "settlement"
)

// header 는 CSV 첫 줄이다. **새 파일에만** 쓴다.
//
// 열 순서가 이 패키지와 크래시 복구 사이의 계약이다. 읽는 쪽은 위치가 아니라
// 이 헤더 줄을 보고 열을 찾아야 한다 — 그러라고 헤더를 쓴다.
//
// 종류마다 안 쓰는 열은 빈 문자열이다. 0 을 쓰지 않는 이유: 0 은 "값이 0" 과
// 구별되지 않는다. 정산 행의 price_usd 가 0 으로 보이면 읽는 쪽이 그것을
// 공짜로 산 주식으로 집계할 수 있다.
var header = []string{
	"at_utc",      // RFC3339Nano, UTC 고정
	"record",      // fill | rebate | settlement
	"round_start", // 회차 시작 unix 초 — 세 종류를 묶는 키
	"market_id",   // fill 만
	"outcome",     // fill 만: Up | Down
	"shares",      //
	"price_usd",   // fill 만
	"fee_usd",     // fill 만. 양수 = 우리가 낸 돈
	"won",         // settlement 만: true | false (우리 포지션 기준)
}

// Ledger 는 append 전용 CSV 원장이다. 동시에 여러 고루틴이 써도 줄이 섞이지
// 않는다.
type Ledger struct {
	mu     sync.Mutex
	f      *os.File
	w      *csv.Writer
	closed bool
}

// Open 은 원장을 연다. 기존 파일이면 이어 쓰고, 새 파일에만 헤더를 쓴다.
//
// 헤더를 조건부로 쓰는 것이 핵심이다. 재시작마다 헤더를 또 쓰면 파서가 그
// 줄에서 깨지거나 — 더 나쁘게 — 헤더를 데이터 행으로 읽어 수량 열에서
// "shares" 를 파싱하려 든다.
//
// 새 파일인지는 열어 둔 핸들의 [os.File.Stat] 크기로 본다. 경로를 다시
// [os.Stat] 하면 그 사이 파일이 갈릴 수 있다 — 우리가 헤더를 쓰는 대상은
// 경로가 아니라 이 핸들이다.
//
// O_APPEND 로 여는 이유: 쓰기마다 커널이 파일 끝으로 위치를 잡아 준다. 크래시
// 뒤 남은 오프셋을 우리가 계산할 필요가 없고, 다른 프로세스가 같은 파일을
// 열어도 서로의 줄을 덮어쓰지 않는다.
//
// 권한 0600: 원장은 우리 포지션 전부를 드러낸다. 공유 박스에서 다른 사용자가
// 읽을 이유가 없다.
func Open(path string) (*Ledger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ledger: 열기 실패: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("ledger: stat 실패: %w", err)
	}
	l := &Ledger{f: f, w: csv.NewWriter(f)}
	if st.Size() == 0 {
		if err := l.writeRow(header); err != nil {
			f.Close()
			return nil, err
		}
	}
	return l, nil
}

// writeRow 는 한 줄을 쓰고 즉시 디스크까지 밀어 넣는다. 호출자가 l.mu 를
// 쥐고 있거나(Record*) 아직 아무도 이 원장을 모르는 상태여야 한다(Open).
//
// 반쯤 쓰인 줄이 남으면 안 되므로 세 단계를 모두 거친다:
//
//	Write  — csv.Writer 의 버퍼(기본 4096B)에 줄 전체를 담는다. 우리 줄은
//	         200B 를 넘지 않으므로 중간에 넘치지 않는다.
//	Flush  — 그 줄을 write(2) 한 번으로 내보낸다. O_APPEND 이므로 다른
//	         프로세스의 줄과 섞이지 않는다.
//	Sync   — 커널 버퍼까지 비운다. 프로세스 크래시는 Flush 로 충분하지만
//	         박스가 통째로 죽으면 페이지 캐시에 있던 줄이 반쯤 남을 수 있다.
//	         회차당 체결 몇 건이라 fsync 비용은 문제가 되지 않는다.
//
// csv.Writer 는 Flush 에서 삼킨 에러를 Error() 로만 알려준다. 확인하지 않으면
// 디스크가 꽉 찼는데도 기록이 성공한 것처럼 보인다.
func (l *Ledger) writeRow(row []string) error {
	if err := l.w.Write(row); err != nil {
		return fmt.Errorf("ledger: csv 쓰기 실패: %w", err)
	}
	l.w.Flush()
	if err := l.w.Error(); err != nil {
		return fmt.Errorf("ledger: flush 실패: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("ledger: sync 실패: %w", err)
	}
	return nil
}

// ready 는 이 원장에 쓸 수 있는 상태인지 본다.
//
// **반드시 락을 잡기 전에 부른다.** nil 리시버에 l.mu.Lock() 을 걸면 그
// 자리에서 nil 역참조로 죽는다 — 검사가 락 뒤에 있으면 검사 자체가 무의미하다.
func (l *Ledger) ready() error {
	if l == nil || l.f == nil || l.w == nil {
		return ErrNotOpen
	}
	return nil
}

// RecordFill 은 체결을 기록한다.
func (l *Ledger) RecordFill(f Fill) error {
	if err := l.ready(); err != nil {
		return err
	}
	if err := validateFill(f); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	return l.writeRow([]string{
		formatTime(f.At),
		recordFill,
		strconv.FormatInt(f.RoundStart, 10),
		strconv.FormatInt(f.MarketID, 10),
		f.Outcome,
		formatFloat(f.Shares),
		formatFloat(f.PriceUSD),
		formatFloat(f.FeeUSD),
		"",
	})
}

// RecordRebate 는 메이커 리베이트 지급을 기록한다.
//
// 값(USD)이 아니라 **주식 수**를 적는다. 리베이트 주식의 값은 정산 전에는
// 정해지지 않는다 — 같은 회차의 settlement 행이 결정한다. 지급 시점에 값을
// 추정해 적으면 그 추정이 파일에 굳는다.
func (l *Ledger) RecordRebate(r Rebate) error {
	if err := l.ready(); err != nil {
		return err
	}
	if err := validateRebate(r); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	return l.writeRow([]string{
		formatTime(r.At),
		recordRebate,
		strconv.FormatInt(r.RoundStart, 10),
		"",
		"",
		formatFloat(r.Shares),
		"",
		"",
		"",
	})
}

// RecordSettlement 은 회차 정산을 기록한다.
func (l *Ledger) RecordSettlement(s Settlement) error {
	if err := l.ready(); err != nil {
		return err
	}
	if err := validateSettlement(s); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return ErrClosed
	}
	return l.writeRow([]string{
		formatTime(s.At),
		recordSettlement,
		strconv.FormatInt(s.RoundStart, 10),
		"",
		"",
		formatFloat(s.Shares),
		"",
		"",
		strconv.FormatBool(s.Won),
	})
}

// Close 는 원장을 닫는다. 두 번 불러도 에러가 아니다 — defer 로 닫고 정상
// 경로에서도 닫는 배선이 흔하고, 그 중복이 봇을 멈출 이유는 없다.
//
// 각 레코드가 이미 Sync 까지 마쳤으므로 Close 는 아무것도 잃지 않는다.
func (l *Ledger) Close() error {
	// nil 원장을 닫는 것은 에러가 아니다. 배선이 원장을 못 만든 채
	// `defer l.Close()` 로 들어오는 경로에서 여기가 죽으면, 정작 봇이
	// 종료 처리(미체결 취소)를 못 한다.
	if l == nil || l.f == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	l.w.Flush()
	werr := l.w.Error()
	cerr := l.f.Close()
	if werr != nil {
		return fmt.Errorf("ledger: 닫는 중 flush 실패: %w", werr)
	}
	if cerr != nil {
		return fmt.Errorf("ledger: 닫기 실패: %w", cerr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 입력 검사 — 파일에 들어가기 전에 막는다
// ---------------------------------------------------------------------------

// finite 는 값이 NaN 도 ±Inf 도 아닌지 본다. `internal/risk` 와 같은 정의다.
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// checkAmount 는 원장에 쓸 수 있는 수량·금액인지 본다.
//
// NaN·Inf 를 여기서 잡는 것이 이 함수의 요점이다. strconv 는 NaN 을 "NaN"
// 으로 얌전히 찍고 ParseFloat 은 그것을 얌전히 되읽으므로, 막지 않으면 파일도
// 파서도 아무 불평 없이 오염된다.
func checkAmount(name string, v float64, allowZero bool) error {
	if !finite(v) {
		return fmt.Errorf("%w: %s 가 유한하지 않다 (%v)", ErrInvalidRecord, name, v)
	}
	if v < 0 {
		return fmt.Errorf("%w: %s 가 음수다 (%v)", ErrInvalidRecord, name, v)
	}
	if !allowZero && v == 0 {
		return fmt.Errorf("%w: %s 가 0 이다", ErrInvalidRecord, name)
	}
	return nil
}

// checkRound 는 회차 키와 시각을 본다.
//
// 셋 다 크래시 복구가 이 줄을 어느 회차에 붙일지 정하는 데 쓴다. 하나라도
// 비면 그 줄은 대조에 쓸 수 없는 줄이고, 대조에 쓸 수 없는 줄을 남기는 것은
// 아무것도 안 남기는 것보다 나쁘다 — 복구가 그것을 세면서 틀린 결론을 낸다.
func checkRound(roundStart int64, at time.Time) error {
	if roundStart <= 0 {
		return fmt.Errorf("%w: round_start 가 %d 다 (양수 unix 초여야 한다)", ErrInvalidRecord, roundStart)
	}
	if at.IsZero() {
		return fmt.Errorf("%w: at 이 제로값이다", ErrInvalidRecord)
	}
	return nil
}

func validateFill(f Fill) error {
	if err := checkRound(f.RoundStart, f.At); err != nil {
		return err
	}
	if f.MarketID <= 0 {
		return fmt.Errorf("%w: market_id 가 %d 다", ErrInvalidRecord, f.MarketID)
	}
	// 정확히 두 철자만 받는다. "up" 을 통과시키면 같은 파일에 두 철자가
	// 섞이고, 회차를 outcome 으로 묶는 집계가 조용히 둘로 갈린다.
	if f.Outcome != OutcomeUp && f.Outcome != OutcomeDown {
		return fmt.Errorf("%w: outcome 이 %q 다 (%q 또는 %q 여야 한다)",
			ErrInvalidRecord, f.Outcome, OutcomeUp, OutcomeDown)
	}
	// 0 주 체결은 체결이 아니다. 남기면 복구가 체결 건수를 잘못 센다.
	if err := checkAmount("shares", f.Shares, false); err != nil {
		return err
	}
	// 가격 상한 $1: CTF 이진 시장에서 주식은 최대 $1 로 정산된다. $1 을
	// 넘는 가격은 시장에 존재할 수 없으므로 필드가 망가졌거나 단위가
	// 어긋난 것이다(센트를 달러 칸에 넣는 실수가 이렇게 보인다). 봇은
	// 0.5 미만으로만 사지만 그 문턱은 quote 의 책임이라 여기서 겹쳐 막지
	// 않는다 — 원장은 "일어난 일"을 적는 곳이고, 실제로 0.5 이상에
	// 체결됐다면 그 사실이야말로 파일에 남아야 한다.
	if err := checkAmount("price_usd", f.PriceUSD, false); err != nil {
		return err
	}
	if f.PriceUSD > 1 {
		return fmt.Errorf("%w: price_usd 가 %v 다 (이진 시장 주식은 $1 을 넘지 않는다)",
			ErrInvalidRecord, f.PriceUSD)
	}
	// 수수료 0 은 정상이다(메이커). 음수는 정상이 아니다 — 리베이트는
	// USDT 가 아니라 주식으로 오므로 USDT 수수료가 마이너스일 경로가 없다.
	// 그리고 음수 수수료는 FillCost 를 줄여 **적자를 흑자로 보이게** 하는
	// 방향이다. pmmm-go 사고와 같은 방향이라 특히 통과시키지 않는다.
	if err := checkAmount("fee_usd", f.FeeUSD, true); err != nil {
		return err
	}
	return nil
}

func validateRebate(r Rebate) error {
	if err := checkRound(r.RoundStart, r.At); err != nil {
		return err
	}
	// 0 주 리베이트는 허용한다. Fill 과 달리 아무것도 왜곡하지 않고
	// ("리베이트가 0 이었다"는 그 자체로 기록할 가치가 있는 사실이다),
	// 실제로 0 이 나올 수 있다 — 거래소가 자격선 미달로 안 줄 수 있다.
	return checkAmount("shares", r.Shares, true)
}

func validateSettlement(s Settlement) error {
	if err := checkRound(s.RoundStart, s.At); err != nil {
		return err
	}
	// 0 주 정산도 허용한다. 그 회차에 아무것도 못 샀다는 사실을 남기는
	// 것이 파일에 구멍을 남기는 것보다 낫다.
	return checkAmount("shares", s.Shares, true)
}

// ---------------------------------------------------------------------------
// 포맷
// ---------------------------------------------------------------------------

// formatTime 은 UTC RFC3339Nano 로 찍는다.
//
// UTC 로 강제하는 이유: 로컬 타임존으로 찍으면 박스를 도쿄로 옮겼을 때 같은
// 파일 안에서 오프셋이 바뀌고, 회차 경계(UTC 기준)와 대조할 때 사람이 그
// 오프셋을 매번 암산해야 한다.
//
// unix 정수 대신 문자열인 이유: 크래시 뒤 사람이 먼저 읽는 파일이다. 사람이
// 읽을 수 있으면서 time.Parse 로 정확히 되읽을 수 있는 형식이 RFC3339Nano 다.
func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// formatFloat 은 되읽으면 정확히 같은 float64 가 나오는 최단 십진 표기를
// 만든다('f', -1 — 지수 표기 없음).
//
// 지수 표기를 피하는 이유는 사람이다. 0.49 가 4.9e-01 로 찍히면 크래시 뒤
// 원장을 눈으로 훑는 사람이 자릿수를 놓친다. 정밀도 -1 이므로 반올림 손실은
// 없다 — 0.1+0.2 는 0.30000000000000004 로 그대로 찍힌다.
func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
