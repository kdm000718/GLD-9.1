// Package quote 는 "어느 가격에 주문을 걸지"와 "걸린 주문을 그대로 둘지
// 옮길지"를 정한다. 순수 함수만 있다 — 네트워크도, 시계도, 전역 상태도
// 없고 현재 시각조차 인자로 받는다.
//
// 결정을 집행(internal/exec)에서 떼어낸 이유: 집행은 오더북·주문·시계에
// 얽혀 있어서 모의 서버 없이는 시험할 수 없다. 반면 돈을 잃는 판단은 전부
// 여기에 있다 — 관통 방지, 동일가 무동작, 재호가 쿨다운. 이것들을 값 몇
// 개로 전부 시험할 수 있게 하는 것이 이 패키지의 존재 이유다.
//
// 가격은 전부 정수 틱이다. 실수 가격은 이 패키지에 들어오지 않는다.
package quote

import (
	"fmt"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
)

// Book 은 결정에 필요한 오더북 상태다. ws.Book 을 임포트하지 않고 값으로
// 받는 이유: ws.Book 은 뮤텍스를 든 살아 있는 상태라 테스트에서 원하는
// 모양을 만들려면 프레임을 흘려 넣어야 한다. 여기 필요한 것은 BestBid/
// BestAsk 의 결과뿐이고, 그 결과는 int64 두 개와 bool 두 개다.
//
// BestBid/BestAsk 는 **우리 주문을 제외한** 값이어야 한다. 우리 호가를
// 그대로 두면 자기 자신을 쫓는 순환이 생긴다(ws.Book.BestBid 의 exclude).
//
// Has* 가 false 면 대응하는 틱 값은 의미가 없다 — 이 패키지도 읽지 않는다.
type Book struct {
	BestBid   int64 // 우리 주문 제외
	HasBid    bool
	BestAsk   int64
	HasAsk    bool
	Precision int
}

// Open 은 지금 걸려 있는 우리 주문이다. Live 가 false 면 Tick 과 Placed 는
// 의미가 없다.
type Open struct {
	Tick   int64
	Placed time.Time // 이 주문을 낸 시각
	Live   bool      // 걸린 주문이 있는가
}

// Dwell 은 **목표가가 잠깐 스쳤을 뿐인지**를 가린다.
//
// # 왜 쿨다운으로는 못 막는가
//
// 재호가는 한 바퀴에 끝나지 않는다. `Reprice` 판정이 나오면 집행자는 취소만
// 하고, 재주문은 거래소가 취소를 확인해 준 **다음 바퀴**에 일어난다(그래야
// 노출이 두 배가 되지 않는다). 그 사이가 실측 51ms 다.
//
// 문제는 취소가 되돌릴 수 없다는 것이다. 51ms 뒤 재주문 시점에 목표가는 다시
// 계산되고, 그동안 군중이 제자리로 돌아왔으면 **방금 버린 그 자리에 다시
// 선다.** 큐 맨 뒤에서. 쿨다운은 "우리가 마지막으로 주문을 낸 뒤 얼마나
// 지났는가"를 재므로 이것을 보지 못한다 — 실제로 쿨다운 500ms 는 정상
// 작동하는 채로 이 일이 재호가의 46.8% 에서 일어났다(2026-08-11, 27,860건).
//
// # 임계값의 근거
//
// 같은 날 로그에서 군중 최우선 매수호가의 변화 41,182건을 재니 **76.6% 가
// 5초 안에 직전 값으로 되돌아왔다.** 되돌아오기까지의 분포는 두 봉우리로
// 갈렸다:
//
//	≤60ms      40.3%   ← 한 바퀴짜리 깜빡임
//	60~400ms    0.0%   ← 비어 있다
//	400~560ms  40.4%   ← 약 0.5초 진동
//	560~1100ms  4.2%
//	>1100ms    15.1%
//
// 600ms 는 두 봉우리를 모두 덮고 그 뒤의 빈 구간에 선다. 560~800ms 어디를
// 골라도 걸러지는 잡음이 81~83% 로 평평해서 값에 민감하지 않다 — 근거 없이
// 고른 임계가 아니다.
//
// # 이 패키지에서 유일하게 상태를 가진 것이다
//
// 나머지는 전부 순수 함수다. 그럼에도 여기 두는 이유는, 이것을 집행자에게
// 맡기면 **틱 비교가 exec 으로 샌다.** 그 경계가 무너지면 가격 판단을 모의
// 호가창 몇 개로 시험할 수 없게 된다(패키지 문서 참고). 상태는 회차 하나
// 동안만 살고, 집행자는 이 값을 들여다보지 않는다.
type Dwell struct {
	// Need 는 목표가가 유지돼야 하는 시간이다. 0 이면 검사하지 않는다.
	Need time.Duration

	tick  int64
	since time.Time
	has   bool
}

// Observe 는 지금 목표가를 기록하고, 그것이 Need 만큼 유지됐는지 돌려준다.
//
// **매 바퀴 불러야 한다.** 재호가 직전에만 부르면 `since` 가 전진하지 않아
// 조건이 영영 성립하지 않는다.
//
// 경계(경과 == Need)는 통과다. 쿨다운이 경계를 허용하는 것과 같은 규약이다.
func (d *Dwell) Observe(tick int64, now time.Time) bool {
	if d == nil {
		return true
	}
	if !d.has || d.tick != tick {
		d.tick, d.since, d.has = tick, now, true
	}
	if d.Need <= 0 {
		return true
	}
	// 시계가 뒤로 갔으면(Sub 이 음수) 유지된 것으로 치지 않는다. 이 봇의
	// now 는 단조시계에서 오지만, 그 보장이 깨지는 배선이 생겨도 여기서
	// "충분히 유지됐다" 로 읽히면 안 된다.
	return now.Sub(d.since) >= d.Need
}

// Action 은 집행자가 할 일이다.
type Action int

const (
	DoNothing  Action = iota // 조건 미달 또는 같은 가격
	Place                    // 신규 주문
	Reprice                  // 취소 후 재주문
	CancelOnly               // 주문 불가 상태인데 걸린 것이 있다
)

func (a Action) String() string {
	switch a {
	case DoNothing:
		return "do-nothing"
	case Place:
		return "place"
	case Reprice:
		return "reprice"
	case CancelOnly:
		return "cancel-only"
	}
	return fmt.Sprintf("action(%d)", int(a))
}

// Decision 은 한 번의 판단 결과다.
//
// Tick 은 Action 이 Place 또는 Reprice 일 때만 유효하고, 그 밖에는 항상 0
// 이다. 주문을 내지 않는 결정에 가격을 실어 보내면 호출자가 그것을 목표가로
// 오해할 여지가 생긴다 — 0 은 order.Tick.WeiPerShare 의 가드에 걸려 죽는다.
type Decision struct {
	Action Action
	Tick   int64
	Why    string // 로그용. 판단 근거를 사람이 읽을 수 있게.
}

// Ceiling 은 0.5 미만의 최대 틱이다. 정밀도 2 면 49(0.49), 3 이면 499(0.499).
//
// order.Ceiling 에 위임한다 — 같은 공식을 두 군데 두면 언젠가 한쪽만 고친다.
// 리터럴 0.49/0.499 를 박지 않는 이유: 정밀도 2 인 마켓에서 0.499 는 표현
// 불가능한 가격이고, 정밀도 3 인 마켓에서 0.49 는 0.009 만큼 손해 보는
// 가격이다. 상한은 반드시 틱에서 유도한다.
//
// precision 은 1..18 만 허용하고 벗어나면 패닉한다(order.NewTick 과 같은
// 규약). precision 은 회차 시작에 마켓 메타데이터에서 한 번 읽는 값이므로,
// 주문 루프 한복판에서 조용히 틀린 가격을 내는 것보다 여기서 죽는 편이 낫다.
func Ceiling(precision int) int64 { return order.Ceiling(precision).V }

// Target 은 지금 걸어야 할 목표 틱이다. ok=false 면 걸 수 있는 유효한
// 가격이 없다 — tick 은 의미가 없으니 쓰지 마라.
//
// 순서:
//  1. 군중의 최우선 매수호가가 0.5 미만이면 거기에 붙고, 아니면 상한으로 간다.
//  2. 관통 방지: 목표가 최우선 매도호가 **이상**이면 한 틱 아래로 내린다.
//     같은 가격도 관통이다 — 매도호가와 같은 자리에 매수를 걸면 즉시 체결되어
//     테이커 수수료를 문다. 우리 엣지는 메이커 쪽에 있으므로 이 부등호가
//     뒤집히면 엣지가 통째로 사라진다.
//  3. 남은 틱이 0 이하면 주문할 수 없다.
func Target(b Book) (tick int64, ok bool) {
	t, _, ok := target(b)
	return t, ok
}

// target 은 Target 에 판단 근거를 얹은 내부 버전이다. Decide 의 Why 를 위해
// 존재한다 — 근거 문자열을 Decide 에서 다시 만들면 목표가 계산 로직이 두
// 벌이 되고, 그 두 벌은 언젠가 갈라진다.
func target(b Book) (tick int64, why string, ok bool) {
	// 상한을 먼저 구한다. 이 호출이 이 패키지의 유일한 precision 가드
	// 지점이고, Target 의 모든 경로가 여기를 지난다. 군중 추종 경로가
	// 상한을 건너뛰면 가드도 함께 건너뛰는데, 예를 들어 precision 20 이면
	// 10^20 이 int64 를 넘어 감싸면서 half 가 엉뚱한 양수가 되고 어떤
	// 매수호가든 "0.5 미만"으로 통과해 버린다.
	ceiling := Ceiling(b.Precision)
	half := ceiling + 1 // 10^precision / 2, 즉 0.5

	switch {
	case b.HasBid && b.BestBid < half:
		tick = b.BestBid
		why = fmt.Sprintf("군중 %d 추종", b.BestBid)
	case b.HasBid:
		tick = ceiling
		why = fmt.Sprintf("군중 %d 는 0.5 이상 → 상한 %d", b.BestBid, ceiling)
	default:
		tick = ceiling
		why = fmt.Sprintf("매수호가 없음 → 상한 %d", ceiling)
	}

	if b.HasAsk && tick >= b.BestAsk {
		tick = b.BestAsk - 1
		why += fmt.Sprintf(", ask %d 관통 방지 → %d", b.BestAsk, tick)
	}

	if tick <= 0 {
		return 0, why + ", 유효 틱 없음", false
	}
	return tick, why, true
}

// Decide 는 지금 무엇을 할지 정한다. now 는 인자로 받는다 — 이 패키지는
// 시계를 읽지 않는다.
//
// 순서(앞의 조건이 뒤를 이긴다):
//  1. stale: 오래된 호가창을 보고 판단하면 안 된다. 걸린 것은 취소하고
//     새로 걸지 않는다.
//  2. 목표가 없음: 위와 같다.
//  3. 걸린 주문 없음: 신규. 쿨다운은 보지 않는다 — 큐에 아무것도 없으므로
//     잃을 큐 위치가 없다.
//  4. 같은 가격: 아무것도 하지 않는다. 취소·재주문하면 큐 맨 뒤로 밀리는데,
//     최우선 호가에서는 큐 위치가 체결을 지배한다.
//  5. 쿨다운 안: 미룬다. 경계(경과 == cooldown)는 허용한다.
//  6. 재호가.
//  7. 드웰: 목표가가 아직 흔들리는 중이면 미룬다. 취소는 되돌릴 수 없으므로
//     큐 위치를 거는 판단은 목표가 굳은 뒤에 한다([Dwell] 참고).
//
// dwell 은 nil 이어도 된다 — 그때는 7번이 없다.
func Decide(b Book, open Open, now time.Time, cooldown time.Duration, stale bool, dwell *Dwell) Decision {
	// stale 경로는 target 을 호출하지 않고 빠져나가므로 precision 가드도
	// 건너뛴다. 그러면 설정이 망가진 회차가 stale 인 동안 조용히 지나가다가
	// stale 이 풀리는 임의의 시점에 터진다 — 틀렸다는 사실은 첫 판단에서
	// 드러나야 한다.
	Ceiling(b.Precision)

	if stale {
		if open.Live {
			return Decision{Action: CancelOnly, Why: "호가창 stale — 기존 주문 취소"}
		}
		return Decision{Action: DoNothing, Why: "호가창 stale — 신규 주문 보류"}
	}

	tgt, why, ok := target(b)
	if !ok {
		if open.Live {
			return Decision{Action: CancelOnly, Why: why + " — 기존 주문 취소"}
		}
		return Decision{Action: DoNothing, Why: why + " — 대기"}
	}

	// **매 바퀴 관측한다.** 아래 어느 가지로 빠지든 목표가의 나이는 계속
	// 세어야 한다 — 재호가 직전에만 세면 `since` 가 전진하지 않는다.
	held := dwell.Observe(tgt, now)

	if !open.Live {
		// 신규는 드웰을 보지 않는다. 큐에 아무것도 없으므로 잃을 큐 위치가
		// 없고, 회차 초반을 통째로 비우면 그 회차의 엣지를 통째로 버린다.
		return Decision{Action: Place, Tick: tgt, Why: fmt.Sprintf("신규 %d: %s", tgt, why)}
	}

	if open.Tick == tgt {
		return Decision{Action: DoNothing, Why: fmt.Sprintf("동일가 %d 유지 — 큐 위치 보존", tgt)}
	}

	if elapsed := now.Sub(open.Placed); elapsed < cooldown {
		return Decision{Action: DoNothing, Why: fmt.Sprintf("쿨다운 %v/%v — %d→%d 보류", elapsed, cooldown, open.Tick, tgt)}
	}

	if !held {
		return Decision{Action: DoNothing, Why: fmt.Sprintf(
			"목표 %d 가 아직 굳지 않았다(%v/%v) — %d 의 큐 위치를 걸지 않는다",
			tgt, now.Sub(dwell.since).Truncate(time.Millisecond), dwell.Need, open.Tick)}
	}

	return Decision{Action: Reprice, Tick: tgt, Why: fmt.Sprintf("재호가 %d→%d: %s", open.Tick, tgt, why)}
}
