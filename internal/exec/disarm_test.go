package exec

import (
	"errors"
	"fmt"
	"testing"
)

// 이 파일이 지키는 것 하나: **"무장을 풀라"는 판정이 배선에 도달하는가.**
//
// 2026-08-11 실거래에서 exec 이 "무장을 풀고 사람이 확인해야 한다" 를 찍은
// **직후에** 배선이 다음 회차를 잡고 주문을 냈다. 그 문구는 평범한
// fmt.Errorf 였고, 배선은 모든 회차 에러를 똑같이 로그만 찍었다. 말과 행동이
// 갈리면 진짜인 쪽은 행동이다.

// TestUnresolvedCancelIsADisarm 은 회차 종료 시 취소를 확인하지 못한 상태가
// 배선이 가려낼 수 있는 종류인지 본다.
//
// 이 상태에서 남은 주문은 "거래소에 살아 있을 수 있는데 우리가 모르는" 주문
// 이다. 그대로 다음 회차를 시작하면 노출이 두 회차에 걸쳐 겹친다.
func TestUnresolvedCancelIsADisarm(t *testing.T) {
	st := &roundState{pending: []*openOrder{{id: "2020494954", notional: 4.41}}}
	err := (&Runner{}).leftovers(st)
	if err == nil {
		t.Fatal("미확인 주문이 남았는데 에러가 없다")
	}
	if !IsDisarm(err) {
		t.Fatalf("IsDisarm=false — 배선이 일시적 실패와 구분할 수 없다: %v", err)
	}
	if !isDisarm(err) {
		t.Fatal("내부 판정과 외부 판정이 갈렸다")
	}
}

// TestUnknownNotionalIsADisarm 은 식별자조차 없는 주문이 남은 경우다.
// 취소할 수단이 없으므로 더 나쁘다.
func TestUnknownNotionalIsADisarm(t *testing.T) {
	st := &roundState{unknownNotional: 4.41}
	err := (&Runner{}).leftovers(st)
	if !IsDisarm(err) {
		t.Fatalf("IsDisarm=false: %v", err)
	}
}

// TestOrdinaryErrorsAreNotDisarms 는 반대 방향을 고정한다. 모든 실패를
// 중단으로 다루면 조회가 한 번 흔들릴 때마다 봇이 멈추고, 그러면 운영자는
// 중단 신호를 무시하게 된다 — 그 순간 이 장치는 없는 것과 같다.
func TestOrdinaryErrorsAreNotDisarms(t *testing.T) {
	for _, err := range []error{
		errors.New("일시적 조회 실패"),
		fmt.Errorf("회차 메타데이터: %w", errors.New("timeout")),
		nil,
	} {
		if IsDisarm(err) {
			t.Errorf("IsDisarm(%v)=true — 일시적 실패를 중단으로 읽었다", err)
		}
	}
}

// TestDisarmSurvivesWrapping 은 배선까지 오는 길에 %w 로 한 겹 더 감싸여도
// 판정이 유지되는지 본다. runRound 가 감싸는 경로가 실제로 있다.
func TestDisarmSurvivesWrapping(t *testing.T) {
	inner := &disarmError{err: errors.New("취소 미확인")}
	if !IsDisarm(fmt.Errorf("회차 운용: %w", inner)) {
		t.Fatal("한 겹 감싸니 판정이 사라졌다")
	}
}
