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
func Decide(b Book, open Open, now time.Time, cooldown time.Duration, stale bool) Decision {
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

	if !open.Live {
		return Decision{Action: Place, Tick: tgt, Why: fmt.Sprintf("신규 %d: %s", tgt, why)}
	}

	if open.Tick == tgt {
		return Decision{Action: DoNothing, Why: fmt.Sprintf("동일가 %d 유지 — 큐 위치 보존", tgt)}
	}

	if elapsed := now.Sub(open.Placed); elapsed < cooldown {
		return Decision{Action: DoNothing, Why: fmt.Sprintf("쿨다운 %v/%v — %d→%d 보류", elapsed, cooldown, open.Tick, tgt)}
	}

	return Decision{Action: Reprice, Tick: tgt, Why: fmt.Sprintf("재호가 %d→%d: %s", open.Tick, tgt, why)}
}
