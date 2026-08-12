package main

// 이 파일은 참여 이력을 디스크에 남긴다.
//
// # 왜 붙였나
//
// report.go 는 이력을 메모리에만 두고 "7일 조회 같은 것이 필요해지면 그때
// 저장소를 붙인다" 고 적어 두었다. 그 때가 왔다 — 누적 참여 회차와 누적 승률을
// /status 에 싣기 시작했는데, 메모리에만 있으면 **모니터를 재기동할 때마다
// 누적이 0 으로 돌아간다.** 감시 장치를 고치는 배포가 그 감시 장치가 세던 숫자를
// 지우는 셈이고, 그 사실은 화면에 나타나지 않는다("누적 참여 1회차" 는 재기동
// 직후에도 멀쩡해 보인다).
//
// # 스키마를 굳히지 않는다
//
// 저장하는 것은 participation 그대로다. 별도의 저장용 타입을 만들지 않았다 —
// 두 벌이 되면 갈린 쪽이 틀렸을 때 알 방법이 없다(main.go 의 계약 논리와 같다).
// 대신 모르는 필드는 무시하고 읽는다: 필드가 늘어난 뒤 예전 파일을 읽어도
// 이력을 통째로 버리지 않게 한다.
//
// # 읽기 실패는 치명적이지 않다
//
// 파일이 없거나 깨졌으면 **빈 이력으로 시작하고 로그를 남긴다.** 여기서 죽으면
// 감시 장치가 통째로 안 뜬다. 이력은 편의이고 하트비트 감시가 본체다 — 편의가
// 본체를 죽이면 안 된다(Auto-Claim 이 거래를 막지 않는 것과 같은 규약).
//
// 다만 **조용히 비우지는 않는다.** 깨진 파일을 말없이 버리면 누적이 0 이 된
// 이유를 아무도 모른다.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// store 는 참여 이력의 저장소다. path 가 비면 아무것도 하지 않는다 —
// 저장 경로를 주지 않은 것은 "저장하지 마라" 는 뜻이고, 그 경우에도 모니터는
// 예전처럼 메모리만으로 돈다.
type store struct {
	path string
	logf func(string, ...any)
}

func (t *store) enabled() bool { return t != nil && t.path != "" }

func (t *store) log(format string, args ...any) {
	if t == nil || t.logf == nil {
		return
	}
	t.logf(format, args...)
}

// load 는 저장된 이력을 읽는다. 없거나 깨졌으면 빈 슬라이스다.
func (t *store) load() []participation {
	if !t.enabled() {
		return nil
	}
	b, err := os.ReadFile(t.path)
	if err != nil {
		if !os.IsNotExist(err) {
			t.log("이력 읽기 실패 — 빈 이력으로 시작한다 (%s): %v", t.path, err)
			return nil
		}
		t.log("이력 파일이 없다 — 빈 이력으로 시작한다 (%s)", t.path)
		return nil
	}
	var ps []participation
	if err := json.Unmarshal(b, &ps); err != nil {
		// **파일을 지우지 않는다.** 사람이 열어 볼 수 있어야 한다.
		t.log("이력 파일이 깨졌다 — 빈 이력으로 시작한다. 파일은 그대로 둔다 (%s): %v", t.path, err)
		return nil
	}
	// 슬러그 없는 항목은 버린다 — 맞출 열쇠가 없어 정산이 절대 붙지 않고,
	// 누적 참여 수만 늘린다.
	out := ps[:0]
	for _, p := range ps {
		if p.Slug != "" {
			out = append(out, p)
		}
	}
	t.log("이력 %d회차를 읽었다 (%s)", len(out), t.path)
	return out
}

// save 는 이력을 원자적으로 쓴다.
//
// **같은 디렉터리에 임시 파일을 만들고 rename 한다.** 제자리에서 덮어쓰면
// 쓰는 도중에 죽었을 때 반쪽짜리 JSON 이 남고, 다음 기동은 그것을 "깨진 파일"
// 로 읽어 이력을 통째로 잃는다. rename 은 같은 파일시스템 안에서 원자적이므로
// 임시 파일은 반드시 대상과 같은 디렉터리에 만든다(/tmp 에 만들면 다른
// 파일시스템일 수 있고 그때 rename 은 원자성을 잃는다).
func (t *store) save(ps []participation) error {
	if !t.enabled() {
		return nil
	}
	b, err := json.Marshal(ps)
	if err != nil {
		return fmt.Errorf("이력 인코딩: %w", err)
	}
	dir := filepath.Dir(t.path)
	f, err := os.CreateTemp(dir, ".rounds-*.tmp")
	if err != nil {
		return fmt.Errorf("이력 임시 파일: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // rename 이 성공하면 사라진 이름이라 무해하다

	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("이력 쓰기: %w", err)
	}
	// **fsync 하고 닫는다.** rename 이 원자적이어도 내용이 디스크에 없으면
	// 호스트가 갑자기 죽었을 때 이름만 있고 알맹이가 없는 파일이 남는다.
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("이력 동기화: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("이력 파일 닫기: %w", err)
	}
	if err := os.Rename(tmp, t.path); err != nil {
		return fmt.Errorf("이력 교체: %w", err)
	}
	return nil
}
