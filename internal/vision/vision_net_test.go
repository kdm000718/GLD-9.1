package vision

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFetchChunkDownloadsAndCaches(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: 네트워크 테스트를 건너뛴다")
	}
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	arr, err := fetchChunk(ctx, "BTCUSDT", "5m", "monthly", "2021-06", dir)
	if err != nil {
		t.Fatalf("fetchChunk: %v", err)
	}
	if len(arr) == 0 {
		t.Fatal("2021-06 5분봉이 비어 있다")
	}
	// 6월은 30일 × 288 = 8,640 봉
	if len(arr) != 8640 {
		t.Errorf("봉 %d개, 기대 8640개", len(arr))
	}
	cache := filepath.Join(dir, "vision", "BTCUSDT", "5m", "monthly-2021-06.bin")
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("캐시 파일이 없다: %v", err)
	}

	// 두 번째 호출은 네트워크 없이 캐시에서 와야 하고 값이 같아야 한다
	again, err := fetchChunk(ctx, "BTCUSDT", "5m", "monthly", "2021-06", dir)
	if err != nil {
		t.Fatalf("캐시 재읽기: %v", err)
	}
	if len(again) != len(arr) || again[0] != arr[0] || again[len(again)-1] != arr[len(arr)-1] {
		t.Error("캐시 왕복에서 값이 달라졌다")
	}
}

func TestFetchChunkReturnsNilForMissingPeriod(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: 네트워크 테스트를 건너뛴다")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// 상장 이전이라 404 여야 하고, 404 는 오류가 아니다
	arr, err := fetchChunk(ctx, "BTCUSDT", "5m", "monthly", "2010-01", t.TempDir())
	if err != nil {
		t.Fatalf("404 를 오류로 처리했다: %v", err)
	}
	if arr != nil {
		t.Errorf("없는 기간인데 %d행을 돌려줬다", len(arr))
	}
}
