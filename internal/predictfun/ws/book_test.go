package ws

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"
)

// 가격은 전부 정수 틱으로 다룬다. 정밀도 2 면 1틱 = 0.01 이므로 0.49 는 49틱이다.
// 수량은 Shares(고정소수점) 단위다 — setForTest 는 파싱을 거치지 않으므로
// 여기 숫자는 자연 단위가 아니라 이미 Shares 로 취급되는 추상적인 값이다.
func TestBestExcludesOwnOrders(t *testing.T) {
	b := NewBook(2)
	b.setForTest(map[int64]Shares{47: 100, 48: 50}, map[int64]Shares{52: 80})

	bid, okBid := b.BestBid(nil)
	ask, okAsk := b.BestAsk(nil)
	if !okBid || bid != 48 {
		t.Fatalf("제외 없음: bid=%d ok=%v, 기대 48/true", bid, okBid)
	}
	if !okAsk || ask != 52 {
		t.Fatalf("제외 없음: ask=%d ok=%v, 기대 52/true", ask, okAsk)
	}

	// 48틱의 50수량이 전부 우리 것이면 최우선 매수호가는 47 로 내려간다.
	bid, okBid = b.BestBid(map[int64]Shares{48: 50})
	if !okBid || bid != 47 {
		t.Fatalf("자기 주문 제외 후 bid=%d ok=%v, 기대 47/true", bid, okBid)
	}
	ask, okAsk = b.BestAsk(map[int64]Shares{48: 50})
	if !okAsk || ask != 52 {
		t.Errorf("매수 쪽 제외가 매도 쪽을 바꿨다: ask=%d ok=%v", ask, okAsk)
	}
}

func TestBestPartialOwnQuantityKeepsLevel(t *testing.T) {
	b := NewBook(2)
	b.setForTest(map[int64]Shares{48: 50}, map[int64]Shares{52: 10})
	// 50 중 30 만 우리 것이면 그 층은 남는다.
	bid, ok := b.BestBid(map[int64]Shares{48: 30})
	if !ok || bid != 48 {
		t.Fatalf("부분 제외 후 bid=%d ok=%v, 기대 48/true (20 이 남아 있다)", bid, ok)
	}
	// 정확히 전량이 우리 것이면 그 층은 사라진다 — 경계값.
	if _, ok := b.BestBid(map[int64]Shares{48: 50}); ok {
		t.Error("전량이 우리 것인데 그 층이 남아 있다")
	}
}

// 한쪽만 비었을 때 그쪽 ok 가 반드시 false 여야 한다. 이것을 놓치면 호출자가
// 틱 0(가격 0.00)을 최우선 매수호가로 읽고 그 값으로 주문을 낸다.
func TestBestEmptyBidSideDoesNotLeakPhantomTick(t *testing.T) {
	b := NewBook(2)
	b.setForTest(nil, map[int64]Shares{52: 10})

	if _, ok := b.BestBid(nil); ok {
		t.Error("매수 층이 하나도 없는데 BestBid 가 ok=true 를 냈다 — 유령 호가")
	}
	ask, ok := b.BestAsk(nil)
	if !ok || ask != 52 {
		t.Fatalf("매도 쪽까지 죽었다: ask=%d ok=%v, 기대 52/true", ask, ok)
	}
}

// 반대 방향도 같은 규칙이다.
func TestBestEmptyAskSideDoesNotLeakPhantomTick(t *testing.T) {
	b := NewBook(2)
	b.setForTest(map[int64]Shares{48: 10}, nil)

	if _, ok := b.BestAsk(nil); ok {
		t.Error("매도 층이 하나도 없는데 BestAsk 가 ok=true 를 냈다 — 유령 호가")
	}
	bid, ok := b.BestBid(nil)
	if !ok || bid != 48 {
		t.Fatalf("매수 쪽까지 죽었다: bid=%d ok=%v, 기대 48/true", bid, ok)
	}
}

func TestStaleAfterNoUpdate(t *testing.T) {
	const sec = int64(time.Second) // 단조시계는 나노초다
	b := NewBook(2)
	b.setLastRecvForTest(1_000 * sec)
	if b.Stale(1_002*sec, 3*sec) {
		t.Error("2초 경과인데 3초 문턱에서 stale 로 봤다")
	}
	if !b.Stale(1_004*sec, 3*sec) {
		t.Error("4초 경과인데 stale 이 아니라고 봤다 — 오래된 호가로 재주문하게 된다")
	}
}

// 정확히 문턱에 걸린 순간은 아직 stale 이 아니다. 경계를 고정해 두지 않으면
// 나중에 > 와 >= 를 바꿔 써도 위 테스트가 통과한다.
func TestStaleExactThresholdIsNotStale(t *testing.T) {
	const sec = int64(time.Second)
	b := NewBook(2)
	b.setLastRecvForTest(1_000 * sec)
	if b.Stale(1_003*sec, 3*sec) {
		t.Error("정확히 3초에서 stale 로 봤다 — 경계는 아직 신선이다")
	}
	if !b.Stale(1_003*sec+1, 3*sec) {
		t.Error("3초를 1나노초 넘겼는데 stale 이 아니라고 봤다")
	}
}

// 한 번도 프레임을 받지 않은 호가창은 항상 stale 이어야 한다. 0 으로 초기화된
// lastRecv 를 그대로 쓰면 now - 0 이 거대한 값이라 우연히 맞지만, 그 우연에
// 기대면 안 된다 — 명시적으로 고정한다.
// **핵심은 아래 두 케이스다.** 위 1,000초 케이스는 가드가 없어도
// `1000s - 0 > 3s` 로 통과하므로 주석이 부정한 바로 그 우연에 기댄다.
// nowMonoNs 가 문턱보다 **작은** 시점(= 기동 직후)이라야 가드가 실제로
// 판정을 뒤집는다. nowMonoNs 는 timing.Uptime(), 즉 프로세스 기동 후
// 경과다 — 가드가 없으면 기동 후 afterNs 동안은 프레임을 한 건도 못 받은
// 책이 "신선" 으로 보고된다. 5분 회차의 처음 몇 초는 이 봇이 p_up 을
// 고정하고 첫 호가를 내는 구간이라 정확히 그때가 위험하다.
func TestStaleBeforeAnyFrame(t *testing.T) {
	const sec = int64(time.Second)
	b := NewBook(2)
	if !b.Stale(1_000*sec, 3*sec) {
		t.Error("프레임을 한 번도 안 받았는데 신선하다고 봤다")
	}
	if !b.Stale(1*sec, 3*sec) {
		t.Error("기동 1초 시점에 프레임을 한 건도 안 받은 책을 신선하다고 봤다" +
			" — lastRecv==0 가드가 없으면 1s-0 > 3s 가 거짓이라 여기서 걸린다")
	}
	if !b.Stale(0, 3*sec) {
		t.Error("기동 0초 시점에 프레임을 한 건도 안 받은 책을 신선하다고 봤다")
	}
}

func TestTickRoundsNotTruncates(t *testing.T) {
	cases := []struct {
		price     float64
		precision int
		want      int64
	}{
		{0.29, 2, 29}, // 절단하면 28 — float64 로 28.999999999999996 이다
		{0.58, 2, 58}, // 절단하면 57
		{0.07, 2, 7},
		{0.49, 2, 49},
		{0.499, 3, 499},
	}
	for _, c := range cases {
		if got := Tick(c.price, c.precision); got != c.want {
			t.Errorf("Tick(%v, %d) = %d, 기대 %d — 반올림이 아니라 절단하고 있다",
				c.price, c.precision, got, c.want)
		}
	}
}

// --- 아래는 브리프의 Step 1 목록에 없는, 이 태스크에서 추가한 테스트다. ---
// team-lead 요청: Apply 의 역전 가드와 교체(병합 아님) 규칙이 실제로 강제되는지
// 돌연변이로 확인하고, 잡히지 않으면 테스트를 추가하라는 지시에 따른 것이다.

// 실제 predict.fun 오더북 스냅샷 하나(팀리드가 수집기 레코드에서 확인).
// bids 가 빈 배열이다 — 만기 직전 정상 상황이며 에러가 아니다.
const realOrderbookPayload = `{"asks":[[0.01,553.337],[0.04,100.2],[0.88,55.0],[0.9,100.0],[0.95,3.0],
	[0.98,855.0],[0.99,1528.0]],
 "bids":[],
 "marketId":1047104,
 "orderCount":22,
 "lastOrderSettled":{"id":"1468347097","kind":"LIMIT","marketId":1047104,
	"outcome":"No","price":"0.05","side":"Ask"},
 "settlementsPending":{"asks":[[0.05,0.071]],"bids":[[0.01,2.0142]]},
 "updateTimestampMs":1785441600079,
 "version":1}`

func frameWithData(t *testing.T, raw string, recvMonoNs int64) Frame {
	t.Helper()
	msg := Message{Type: TypeMessage, Topic: "predictOrderbook/1047104", Data: json.RawMessage(raw)}
	return Frame{RecvMonoNs: recvMonoNs, Raw: []byte(raw), Msg: msg}
}

// 실제 페이로드를 그대로 적용했을 때 매수 쪽이 비고, 매도 쪽 최우선호가가
// 올바른 틱(정밀도 2 기준 1틱 = 0.01, 0.01 → 1틱)으로 잡히는지 확인한다.
func TestApplyRealPayloadEmptyBidsAndAsksTick(t *testing.T) {
	b := NewBook(2)
	if _, err := b.Apply(frameWithData(t, realOrderbookPayload, 1_000)); err != nil {
		t.Fatalf("Apply 실패: %v", err)
	}

	if _, ok := b.BestBid(nil); ok {
		t.Error("실제 페이로드의 bids 가 빈 배열인데 BestBid 가 ok=true 를 냈다")
	}
	ask, ok := b.BestAsk(nil)
	if !ok || ask != 1 {
		t.Fatalf("BestAsk = %d/%v, 기대 1/true (0.01 → 1틱)", ask, ok)
	}
	if got := b.LastRecvMonoNs(); got != 1_000 {
		t.Errorf("LastRecvMonoNs = %d, 기대 1000", got)
	}
}

// 정산 대기 물량은 파싱은 되지만 최우선 호가 계산에 들어가지 않는다.
func TestApplyParsesSettlementsPendingWithoutAffectingBest(t *testing.T) {
	b := NewBook(2)
	if _, err := b.Apply(frameWithData(t, realOrderbookPayload, 1_000)); err != nil {
		t.Fatalf("Apply 실패: %v", err)
	}
	pendBids, pendAsks := b.PendingLevels()
	if len(pendBids) != 1 || pendBids[0].Price != 0.01 || pendBids[0].Size != 2.0142 {
		t.Errorf("pendingBids = %+v, 기대 [{0.01 2.0142}]", pendBids)
	}
	if len(pendAsks) != 1 || pendAsks[0].Price != 0.05 || pendAsks[0].Size != 0.071 {
		t.Errorf("pendingAsks = %+v, 기대 [{0.05 0.071}]", pendAsks)
	}
	// settlementsPending 은 실제 asks/bids 와 무관 — 매도 최우선호가는 여전히 1틱이다.
	if ask, ok := b.BestAsk(nil); !ok || ask != 1 {
		t.Errorf("정산 대기 파싱이 매도 최우선호가를 바꿨다: ask=%d ok=%v", ask, ok)
	}
}

// Apply 는 전체 스냅샷을 통째로 교체해야 한다 — 병합하면 이번 스냅샷에 없는
// 호가가 영원히 남는다(유령 유동성). 두 번째 프레임에는 47틱 매수호가가
// 없으므로, 적용 후 47틱은 사라져야 한다.
func TestApplyReplacesNotMerges(t *testing.T) {
	b := NewBook(2)
	first := `{"asks":[],"bids":[[0.47,10],[0.48,20]],"updateTimestampMs":1000}`
	second := `{"asks":[],"bids":[[0.48,20]],"updateTimestampMs":2000}`

	if _, err := b.Apply(frameWithData(t, first, 1)); err != nil {
		t.Fatalf("첫 프레임 Apply 실패: %v", err)
	}
	if bid, ok := b.BestBid(nil); !ok || bid != 48 {
		t.Fatalf("첫 프레임 후 bid=%d ok=%v, 기대 48/true", bid, ok)
	}

	if _, err := b.Apply(frameWithData(t, second, 2)); err != nil {
		t.Fatalf("두 번째 프레임 Apply 실패: %v", err)
	}
	// 47틱이 남아 있으면 안 된다 — 47을 우리 주문 없이 제외해도 여전히 최우선은 48이어야
	// 하고, exclude 로 48을 통째로 빼면 47이 살아있는지 직접 드러난다.
	if bid, ok := b.BestBid(map[int64]Shares{48: Qty(20)}); ok {
		t.Fatalf("48틱을 전량 제외했는데 bid=%d ok=%v — 47틱(이전 스냅샷)이 병합돼 남아 있다", bid, ok)
	}
}

// updateTimestampMs 가 현재 값 이하인 프레임은 버려야 한다. 재접속 전후로
// 오래된 스냅샷이 새 것 위에 덮이면 호가창이 과거로 되돌아간다.
func TestApplyDropsStaleOrEqualTimestamp(t *testing.T) {
	b := NewBook(2)
	newer := `{"asks":[],"bids":[[0.48,20]],"updateTimestampMs":2000}`
	older := `{"asks":[],"bids":[[0.47,10]],"updateTimestampMs":1000}`
	same := `{"asks":[],"bids":[[0.47,10]],"updateTimestampMs":2000}`

	applied, err := b.Apply(frameWithData(t, newer, 10))
	if err != nil {
		t.Fatalf("Apply 실패: %v", err)
	}
	if !applied {
		t.Fatal("정상 프레임인데 applied=false 를 냈다")
	}

	applied, err = b.Apply(frameWithData(t, older, 20))
	if err != nil {
		t.Fatalf("Apply 실패: %v", err)
	}
	// applied 를 err 와 따로 보는 이유: 버림은 에러가 아니라 err 가 nil 이다.
	// 호출자가 err==nil 만 보고 "갱신됨" 으로 세면 버려진 프레임이 수신
	// 카운터·마지막 갱신 시각·간격 분포를 전부 오염시킨다(Book.Apply 주석).
	if applied {
		t.Error("과거 타임스탬프 프레임을 버렸으면서 applied=true 를 냈다 — 호출자가 갱신으로 센다")
	}
	if bid, ok := b.BestBid(nil); !ok || bid != 48 {
		t.Fatalf("과거 타임스탬프 프레임이 적용됐다: bid=%d ok=%v, 기대 48/true", bid, ok)
	}
	if got := b.LastRecvMonoNs(); got != 10 {
		t.Errorf("버려진 프레임이 LastRecvMonoNs 를 갱신했다: %d, 기대 10", got)
	}

	// 같은 타임스탬프도 버려야 한다 — 재구독 직후 같은 스냅샷 재전송은 정상이고,
	// 그때 lastRecvMonoNs 를 갱신하면 멈춘 호가창이 계속 신선해 보인다.
	applied, err = b.Apply(frameWithData(t, same, 30))
	if err != nil {
		t.Fatalf("Apply 실패: %v", err)
	}
	if applied {
		t.Error("같은 타임스탬프 프레임을 버렸으면서 applied=true 를 냈다")
	}
	if bid, ok := b.BestBid(nil); !ok || bid != 48 {
		t.Fatalf("같은 타임스탬프 프레임이 적용됐다: bid=%d ok=%v, 기대 48/true", bid, ok)
	}
	if got := b.LastRecvMonoNs(); got != 10 {
		t.Errorf("같은 타임스탬프의 버려진 프레임이 LastRecvMonoNs 를 갱신했다: %d, 기대 10", got)
	}
}

// --- fix 라운드 1: 수량을 정수로 반올림하면 소수 수량 층이 사라진다는 지적에
// 따라 Shares(고정소수점) 로 바꿨다. 아래 4개는 team-lead 지시로 추가했다. ---

// 실측 페이로드의 소수 수량이 Qty/Float 왕복 후에도 보존돼야 한다. 정수
// 반올림으로 되돌리면(Shares 를 int64 로 되돌려 구현하면) 0.071 같은 값이
// 0 이 되어 이 테스트가 FAIL 한다.
func TestQtyPreservesFractionalShares(t *testing.T) {
	cases := []float64{0.071, 2.0142, 553.337, 100.2}
	for _, f := range cases {
		if got := Qty(f).Float(); math.Abs(got-f) > 1e-9 {
			t.Errorf("Qty(%v).Float() = %v, 차이가 1e-9 를 넘는다 — 소수 수량이 손실됐다", f, got)
		}
	}
}

// 0.5 미만인(정수로 반올림하면 0이 되는) 매수 층이 사라지면 안 된다. 정수
// 반올림 구현에서는 이 층이 통째로 없어져 BestBid 가 ok=false 를 낸다 —
// 실재하는 군중 유동성을 없는 것으로 착각하는 것이다.
func TestApplySubOneQuantityLevelSurvives(t *testing.T) {
	b := NewBook(2)
	frame := `{"asks":[],"bids":[[0.48,0.071]],"updateTimestampMs":1000}`
	if _, err := b.Apply(frameWithData(t, frame, 1)); err != nil {
		t.Fatalf("Apply 실패: %v", err)
	}
	bid, ok := b.BestBid(nil)
	if !ok || bid != 48 {
		t.Fatalf("소수 수량(0.071) 층이 사라졌다: bid=%d ok=%v, 기대 48/true", bid, ok)
	}
}

// 층 합계가 우리 2.04주 + 군중 0.4주 = 2.44주일 때, 우리 몫만 정확히 빼면
// 군중의 0.4주가 남아야 한다. 정수 반올림 구현에서는 합계가 2로 잘리고
// exclude 도 2로 잘려 2-2<=0 이 되어 이 층이(군중 몫까지) 통째로 사라진다 —
// 이 수정이 고치려는 핵심 시나리오다.
func TestApplyFractionalPartialExcludeKeepsLevel(t *testing.T) {
	b := NewBook(2)
	frame := `{"asks":[],"bids":[[0.48,2.44]],"updateTimestampMs":1000}`
	if _, err := b.Apply(frameWithData(t, frame, 1)); err != nil {
		t.Fatalf("Apply 실패: %v", err)
	}
	exclude := map[int64]Shares{48: Qty(2.04)}
	bid, ok := b.BestBid(exclude)
	if !ok || bid != 48 {
		t.Fatalf("우리 2.04주를 뺐는데 군중의 0.4주까지 사라졌다: bid=%d ok=%v, 기대 48/true", bid, ok)
	}
}

// 위 테스트와 짝이 되는 경계값 — 층 전체(2.04주)가 정확히 우리 것이면 그
// 층은 사라져야 한다.
func TestApplyFractionalFullExcludeRemovesLevel(t *testing.T) {
	b := NewBook(2)
	frame := `{"asks":[],"bids":[[0.48,2.04]],"updateTimestampMs":1000}`
	if _, err := b.Apply(frameWithData(t, frame, 1)); err != nil {
		t.Fatalf("Apply 실패: %v", err)
	}
	exclude := map[int64]Shares{48: Qty(2.04)}
	if bid, ok := b.BestBid(exclude); ok {
		t.Fatalf("전량(2.04주)이 우리 것인데 bid=%d ok=%v — 층이 남아 있다", bid, ok)
	}
}

// --- precision 가드 (order.NewTick 과 같은 규약: 1..18, 위반 시 패닉) ---

// precision 0 은 decimalPrecision 필드가 없어지거나 이름이 바뀌었을 때
// encoding/json 이 넣는 제로값이다. 가드가 없으면 0.47/0.48/0.49 세 층이
// 전부 틱 0 으로 접혀, 책에 층이 가득한데도 BestBid 가 ok=true 와 함께
// 틱 0(가격 0.00)을 돌려준다 — ok 인터록이 통째로 무력화된다.
func TestNewBookRejectsOutOfRangePrecision(t *testing.T) {
	for _, p := range []int{0, -1, MaxPrecision + 1, 100} {
		t.Run(fmt.Sprint(p), func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("NewBook(%d) 이 패닉하지 않았다 — order 패키지는 같은 값에서 패닉한다", p)
				}
			}()
			NewBook(p)
		})
	}
}

// 경계는 통과해야 한다 — 가드가 정상 값까지 막으면 그게 더 나쁘다.
// 실측 decimalPrecision 은 2 와 3 이 공존한다.
func TestNewBookAcceptsValidPrecision(t *testing.T) {
	for _, p := range []int{1, 2, 3, MaxPrecision} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("NewBook(%d) 이 패닉했다: %v", p, r)
				}
			}()
			NewBook(p)
		}()
	}
}

// Tick/FromTick 은 Book 없이도 부를 수 있는 공개 함수라 자기 가드가 필요하다.
// FromTick 이 특히 조용한 경로다 — precision 0 이면 FromTick(0,0)=0.00 이
// 아무 소리 없이 나와 로그·알림에 "가격 0.00" 이 정상값처럼 찍힌다.
func TestTickFuncsRejectOutOfRangePrecision(t *testing.T) {
	for _, p := range []int{0, -1, MaxPrecision + 1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Tick(0.49, %d) 이 패닉하지 않았다", p)
				}
			}()
			Tick(0.49, p)
		}()
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("FromTick(49, %d) 이 패닉하지 않았다", p)
				}
			}()
			FromTick(49, p)
		}()
	}
}

// setForTest는 파싱을 거치지 않고 호가창 상태를 직접 주입한다. 테스트 전용.
func (b *Book) setForTest(bids, asks map[int64]Shares) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bids = bids
	b.asks = asks
}

// setLastRecvForTest는 Stale 판정에 쓰는 마지막 수신 시각을 직접 주입한다. 테스트 전용.
func (b *Book) setLastRecvForTest(monoNs int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastRecvMonoNs = monoNs
}
