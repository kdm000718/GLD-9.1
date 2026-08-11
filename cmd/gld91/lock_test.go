package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 두 인스턴스가 같은 원장을 열면 헤더가 두 번 쓰이고, 더 나쁘게는 서로의
// 노출을 모른 채 각자 cap 까지 걸어 실제 노출이 두 배가 된다.
func TestLockRefusesSecondInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv.lock")

	first, err := lockInstance(path)
	if err != nil {
		t.Fatalf("첫 잠금 실패: %v", err)
	}
	defer func() { _ = first.Release() }()

	if second, err := lockInstance(path); err == nil {
		_ = second.Release()
		t.Fatal("두 번째 인스턴스가 같은 원장을 잡았다")
	} else if !strings.Contains(err.Error(), "다른 gld91 인스턴스") {
		t.Errorf("에러가 원인을 짚지 않는다: %v", err)
	}
}

// 놓으면 다음 인스턴스가 잡을 수 있어야 한다 — 재시작이 막히면 운영자는
// "일단 지우고 뜨자" 를 배우고, 그 습관이 진짜 중복 실행을 통과시킨다.
func TestLockIsReusableAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.csv.lock")

	first, err := lockInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("놓기 실패: %v", err)
	}
	// 두 번 놓아도 에러가 아니다 — defer 와 정상 경로에서 둘 다 놓는 배선이 흔하다.
	if err := first.Release(); err != nil {
		t.Errorf("두 번째 Release 가 에러다: %v", err)
	}

	second, err := lockInstance(path)
	if err != nil {
		t.Fatalf("놓은 뒤에도 못 잡는다: %v", err)
	}
	_ = second.Release()

	// 파일은 남아 있어야 한다. 지우면 "다른 프로세스가 잡고 있는 inode" 와
	// "새로 만든 inode" 가 갈려 둘이 동시에 도는 경로가 열린다.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("잠금 파일이 사라졌다: %v", err)
	}
}

// 디렉터리가 없으면 잠글 수 없다. 조용히 넘어가면 잠금 없이 도는 봇이 된다.
func TestLockFailsWhenPathIsUnusable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "없는디렉터리", "x.lock")
	if l, err := lockInstance(path); err == nil {
		_ = l.Release()
		t.Fatal("없는 디렉터리에 잠금을 걸었다")
	}
}
