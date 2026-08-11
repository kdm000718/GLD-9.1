package beat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// SigHeader 는 HMAC 서명이 실리는 헤더다.
const SigHeader = "X-Beat-Signature"

var (
	// ErrReplay 는 seq 가 전진하지 않았다는 뜻이다.
	ErrReplay = errors.New("beat: seq 가 전진하지 않았다")
	// ErrSkew 는 타임스탬프가 허용창 밖이라는 뜻이다.
	ErrSkew = errors.New("beat: 타임스탬프가 허용창 밖이다")
)

// Sign 은 본문의 HMAC-SHA256 을 hex 로 돌려준다.
//
// 본문 **바이트 그대로**를 서명한다. 구조체를 다시 마샬해서 서명하면
// 필드 순서나 부동소수 표기가 달라져 검증이 실패할 수 있다 — 검증하는 쪽도
// 받은 바이트를 그대로 넣어야 한다.
func Sign(secret, body []byte) string {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// Verify 는 서명이 맞는지 본다.
//
// **hmac.Equal 을 쓰는 것이 요점이다.** `==` 로 바꾸면 바이트별 조기 반환이
// 타이밍으로 정답을 한 바이트씩 흘린다. 이 엔드포인트는 공인이라 공격자가
// 원하는 만큼 시도할 수 있다.
func Verify(secret, body []byte, sig string) bool {
	want, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return hmac.Equal(m.Sum(nil), want)
}

// Gate 는 재전송을 막는다. 모니터가 봇 세션마다 하나 들고, 봇이 재시작하면
// (BootID 가 바뀌면) Reset 한다.
//
// 순수하다 — 현재 시각을 인자로 받는다.
type Gate struct {
	// Skew 는 봇과 모니터 시계 차이의 허용치다. 0 이면 어떤 beat 도 통과하지
	// 못하므로 배선이 반드시 채워야 한다.
	Skew time.Duration

	// last 는 마지막으로 받아들인 seq 다. 제로값 0 이 곧 "아직 없음"이다 —
	// 별도의 started 플래그를 두지 않는 이유는 **봇의 seq 가 항상 1 부터
	// 시작하기 때문이다**(reporter 가 atomic.Uint64.Add(1) 로 만든다).
	// 그래서 seq 0 은 정상 봇이 보낼 수 없는 값이고, 거부하는 것이 옳다.
	// 플래그를 두면 그 구분을 시험할 수 없는 코드가 한 줄 늘 뿐이다.
	last uint64
}

// Reset 은 seq 추적을 처음으로 되돌린다.
//
// **봇이 재시작하면 seq 가 1 로 돌아간다.** 그것을 재전송으로 막으면 재시작
// 뒤 모든 beat 가 거부되어 모니터가 봇을 영원히 죽은 것으로 본다 — 감시
// 장치가 스스로 눈을 감는 형태의 고장이고, 하필 봇이 방금 크래시한 순간에
// 일어난다.
func (g *Gate) Reset() { g.last = 0 }

// Admit 은 이 beat 를 받아도 되는지 본다.
//
// **모든 검사를 통과한 뒤에만 상태를 바꾼다.** 검사 중간에 last 를 올리면,
// 미래 타임스탬프를 단 요청 하나가 seq 를 크게 올려 놓고 그 뒤의 정상 beat
// 가 전부 재전송으로 막힌다 — 공격자가 아니라 봇의 시계가 잠깐 튀어도
// 그렇게 되고, 그때 모니터는 살아 있는 봇을 죽은 것으로 본다.
// (검사 **순서**는 무관하다. 어느 쪽을 먼저 보든 할당이 끝에 있으면 된다.)
func (g *Gate) Admit(seq uint64, ts, now time.Time) error {
	if d := now.Sub(ts); d > g.Skew || d < -g.Skew {
		return fmt.Errorf("%w: %s", ErrSkew, d)
	}
	if seq <= g.last {
		return fmt.Errorf("%w: %d <= %d", ErrReplay, seq, g.last)
	}
	g.last = seq
	return nil
}
