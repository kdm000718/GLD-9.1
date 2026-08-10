package timing

import (
	"sync"
	"testing"
	"time"
)

// NextSeq는 단조 증가해야 한다. 서버 타임스탬프가 ms 해상도라 같은 ms 안에
// 여러 레코드가 들어와도 이 값으로 순서를 매긴다.
func TestNextSeqMonotonic(t *testing.T) {
	first := NextSeq()
	for i := 0; i < 100; i++ {
		next := NextSeq()
		if next <= first {
			t.Fatalf("NextSeq 가 감소하거나 중복됐다: first=%d next=%d", first, next)
		}
		first = next
	}
}

// 여러 goroutine 에서 동시에 불러도 값이 겹치면 안 된다.
func TestNextSeqConcurrentUnique(t *testing.T) {
	const n = 1000
	seen := make([]uint64, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			seen[i] = NextSeq()
		}(i)
	}
	wg.Wait()

	set := make(map[uint64]bool, n)
	for _, v := range seen {
		if set[v] {
			t.Fatalf("NextSeq 중복 값: %d", v)
		}
		set[v] = true
	}
}

// Stamp 는 벽시계와 단조시계를 함께 돌려주고, 둘 다 시간이 지나면 증가해야 한다.
func TestStampIncreases(t *testing.T) {
	unixNs1, monoNs1 := Stamp()
	time.Sleep(time.Millisecond)
	unixNs2, monoNs2 := Stamp()

	if unixNs2 <= unixNs1 {
		t.Errorf("unixNs 가 증가하지 않았다: %d -> %d", unixNs1, unixNs2)
	}
	if monoNs2 <= monoNs1 {
		t.Errorf("monoNs 가 증가하지 않았다: %d -> %d", monoNs1, monoNs2)
	}
}

// Uptime 은 프로세스 기동 후 경과 시간이므로 항상 0보다 크고, 시간이 지나면 늘어난다.
func TestUptimeIncreases(t *testing.T) {
	u1 := Uptime()
	if u1 <= 0 {
		t.Fatalf("Uptime 이 0 이하다: %v", u1)
	}
	time.Sleep(time.Millisecond)
	u2 := Uptime()
	if u2 <= u1 {
		t.Errorf("Uptime 이 증가하지 않았다: %v -> %v", u1, u2)
	}
}
