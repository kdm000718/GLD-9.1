package beat

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestVerifyRejectsWrongSignature(t *testing.T) {
	secret, body := []byte("s3cret"), []byte(`{"seq":1}`)
	sig := Sign(secret, body)
	if !Verify(secret, body, sig) {
		t.Fatal("올바른 서명이 거부됐다")
	}
	for _, bad := range []string{
		"",                          // 헤더 없음
		"zz",                        // hex 가 아니다
		sig + "00",                  // 길이 초과
		sig[:len(sig)-2],            // 길이 부족
		Sign([]byte("other"), body), // 다른 비밀
	} {
		if Verify(secret, body, bad) {
			t.Errorf("잘못된 서명 %q 가 통과했다", bad)
		}
	}
	// 본문이 바뀌면 서명은 더 이상 맞지 않아야 한다 — 그러지 않으면 서명이
	// "누가 보냈나" 만 말하고 "무엇을 보냈나" 는 말하지 않는다.
	if Verify(secret, []byte(`{"seq":2}`), sig) {
		t.Error("본문이 바뀌었는데 서명이 통과했다")
	}
}

// seq 는 단조증가여야 한다. 같은 값이나 역행은 재전송이다.
func TestGateRejectsReplay(t *testing.T) {
	g := &Gate{Skew: time.Minute}
	now := time.Unix(1786000000, 0).UTC()

	if err := g.Admit(10, now, now); err != nil {
		t.Fatalf("첫 beat 가 거부됐다: %v", err)
	}
	for _, seq := range []uint64{10, 9, 0} {
		if err := g.Admit(seq, now, now); !errors.Is(err, ErrReplay) {
			t.Errorf("seq %d 에서 err=%v, want ErrReplay", seq, err)
		}
	}
	// 거부된 요청이 상태를 바꾸지 않았어야 한다.
	if err := g.Admit(11, now, now); err != nil {
		t.Errorf("seq 11 이 거부됐다: %v", err)
	}
}

// 봇이 재시작하면 seq 가 1 로 돌아간다. 그것을 재전송으로 막으면 재시작 뒤
// 모든 beat 가 거부되어 모니터가 봇을 영원히 죽은 것으로 본다 — 감시 장치가
// 스스로 눈을 감는 고장이고, 하필 봇이 방금 크래시한 순간에 일어난다.
func TestGateResetAllowsLowerSeq(t *testing.T) {
	g := &Gate{Skew: time.Minute}
	now := time.Unix(1786000000, 0).UTC()

	if err := g.Admit(9999, now, now); err != nil {
		t.Fatalf("첫 beat: %v", err)
	}
	if err := g.Admit(1, now, now); !errors.Is(err, ErrReplay) {
		t.Fatalf("리셋 없이 seq 1 이 통과했다: %v", err)
	}
	g.Reset()
	if err := g.Admit(1, now, now); err != nil {
		t.Errorf("리셋 뒤 seq 1 이 거부됐다: %v", err)
	}
}

func TestGateRejectsSkew(t *testing.T) {
	now := time.Unix(1786000000, 0).UTC()
	for _, ts := range []time.Time{now.Add(-2 * time.Minute), now.Add(2 * time.Minute)} {
		g := &Gate{Skew: time.Minute}
		if err := g.Admit(1, ts, now); !errors.Is(err, ErrSkew) {
			t.Errorf("ts=%v 에서 err=%v, want ErrSkew", ts, err)
		}
	}
	// 경계 안쪽은 통과한다.
	g := &Gate{Skew: time.Minute}
	if err := g.Admit(1, now.Add(-59*time.Second), now); err != nil {
		t.Errorf("59초 차이가 거부됐다: %v", err)
	}
}

// **시계가 어긋난 요청은 seq 를 전진시키면 안 된다.** 그러지 않으면 미래
// 타임스탬프를 단 요청 하나가 seq 를 크게 올려 놓고, 그 뒤의 정상 beat 가
// 전부 재전송으로 막힌다 — 공격자가 아니라 봇의 시계가 잠깐 튀어도 그렇게 된다.
func TestSkewedBeatDoesNotAdvanceSeq(t *testing.T) {
	g := &Gate{Skew: time.Minute}
	now := time.Unix(1786000000, 0).UTC()

	if err := g.Admit(5, now, now); err != nil {
		t.Fatalf("첫 beat: %v", err)
	}
	if err := g.Admit(9999, now.Add(time.Hour), now); !errors.Is(err, ErrSkew) {
		t.Fatalf("스큐가 거부되지 않았다: %v", err)
	}
	if err := g.Admit(6, now, now); err != nil {
		t.Errorf("스큐 요청이 seq 를 전진시켰다 — seq 6 이 거부됐다: %v", err)
	}
}

// seq 0 은 정상 봇이 보낼 수 없는 값이다 — reporter 가 atomic.Uint64.Add(1)
// 로 만들므로 항상 1 부터다. 제로값이 통과하면 "필드가 빠진 요청"이 첫 beat
// 로 받아들여진다.
func TestGateRejectsZeroSeq(t *testing.T) {
	now := time.Unix(1786000000, 0).UTC()
	for _, name := range []string{"처음", "리셋 직후"} {
		g := &Gate{Skew: time.Minute}
		if name == "리셋 직후" {
			_ = g.Admit(5, now, now)
			g.Reset()
		}
		if err := g.Admit(0, now, now); !errors.Is(err, ErrReplay) {
			t.Errorf("%s: seq 0 에서 err=%v, want ErrReplay", name, err)
		}
	}
}

// **서명 비교는 반드시 상수시간이어야 한다.**
//
// 이것은 단위 테스트로 잡을 수 없다 — `==` 로 바꿔도 모든 입출력이 같고
// 타이밍만 달라진다. 실제로 변이 시험에서 그 변이가 살아남았다. 그래서
// exec 가 "존재하면 안 되는 것"을 막은 방식(소스 스캔)을 쓴다.
//
// 이 엔드포인트는 공인이라 공격자가 원하는 만큼 시도할 수 있고, 바이트별
// 조기 반환은 정답을 한 바이트씩 흘린다.
func TestVerifyUsesConstantTimeCompare(t *testing.T) {
	src, err := os.ReadFile("hmac.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	start := strings.Index(body, "func Verify(")
	if start < 0 {
		t.Fatal("Verify 를 찾지 못했다 — 이 테스트가 무엇을 지키는지 확인하라")
	}
	fn := body[start:]
	if end := strings.Index(fn, "\n}\n"); end >= 0 {
		fn = fn[:end]
	}
	if !strings.Contains(fn, "hmac.Equal(") {
		t.Error("Verify 가 hmac.Equal 을 쓰지 않는다 — 타이밍이 정답을 흘린다")
	}
	// 다이제스트를 다른 방법으로 비교하는 줄을 찾는다. `err != nil` 같은
	// 관계없는 비교까지 잡으면 오탐으로 이 테스트가 먼저 꺼진다.
	for i, line := range strings.Split(fn, "\n") {
		code := line
		if c := strings.Index(code, "//"); c >= 0 {
			code = code[:c]
		}
		if !strings.Contains(code, "want") && !strings.Contains(code, "Sum(") {
			continue
		}
		for _, bad := range []string{"==", "!=", "bytes.Equal(", "strings.Compare("} {
			if strings.Contains(code, bad) {
				t.Errorf("Verify %d번째 줄이 다이제스트를 %q 로 비교한다 — 상수시간이 아니다: %s",
					i+1, bad, strings.TrimSpace(line))
			}
		}
	}
}
