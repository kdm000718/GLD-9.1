package main

import (
	"fmt"
	"os"
	"syscall"
)

// 이 파일은 **봇 인스턴스가 하나뿐임을 보장한다.**
//
// # 왜 필요한가 (Task 4 가 Task 8 으로 넘긴 것)
//
// `ledger.Open` 은 파일 잠금을 걸지 않는다. 두 프로세스가 빈 원장을 동시에
// 열면 둘 다 "새 파일" 로 보고 헤더를 두 번 쓴다 — 파서가 그 줄에서 깨지거나,
// 더 나쁘게는 헤더를 데이터 행으로 읽어 수량 열에서 "shares" 를 파싱한다.
//
// 그것보다 나쁜 것이 따로 있다. 두 인스턴스는 **서로의 노출을 모른다.**
// 각자 cap 까지 걸면 실제 노출은 cap 의 두 배다. 사용자가 두 번 명시한
// `회차당 최대 명목 < equity × 0.0455` 가 그 순간 무의미해진다. 재시작할 때
// 옛 프로세스가 아직 안 죽은 상황은 흔하다.
//
// # 왜 flock 인가
//
// PID 파일은 크래시 뒤 남는다 — 죽은 프로세스의 PID 파일 때문에 봇이 못 뜨는
// 상태가 되고, 그러면 운영자가 "일단 지우고 뜨자" 를 배운다. 그 습관이
// 진짜 중복 실행을 통과시킨다. flock 은 프로세스가 죽으면 커널이 놓아 주므로
// 지울 것이 없다.
//
// **락은 프로세스 수명 내내 열린 파일 핸들에 붙어 있다.** 핸들을 닫으면 락이
// 풀리므로 instanceLock 을 살려 둬야 한다.

// instanceLock 은 잡고 있는 잠금이다. Release 로 놓는다.
type instanceLock struct{ f *os.File }

// lockInstance 는 path 에 배타 잠금을 건다. 이미 다른 프로세스가 잡고 있으면
// **기다리지 않고** 에러다 — 기다리면 두 인스턴스가 순서만 다르게 둘 다 도는
// 상태가 되고, 그것은 지금 막으려는 것과 같다.
func lockInstance(path string) (*instanceLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("잠금 파일을 열지 못했다 (%s): %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("이미 다른 gld91 인스턴스가 이 원장을 쓰고 있다 (%s): %w — "+
			"둘이 동시에 돌면 서로의 노출을 모르고 각자 cap 까지 걸어 실제 노출이 두 배가 된다", path, err)
	}
	return &instanceLock{f: f}, nil
}

// Release 는 잠금을 놓는다. 두 번 불러도 에러가 아니다.
//
// 파일을 지우지 않는다. 지우는 순간 "다른 프로세스가 잡고 있는 inode" 와
// "우리가 방금 만든 새 inode" 가 갈려서, 다음 프로세스가 새 파일에 락을 걸고
// 둘이 동시에 도는 경로가 열린다.
func (l *instanceLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
