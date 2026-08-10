// Package timing은 레코드 타임스탬프의 공통 기준점을 제공한다.
package timing

import (
	"sync/atomic"
	"time"
)

// start는 프로세스 시작 시각이다. RecvMono의 기준점.
var start = time.Now()

// seq는 프로세스 전역 단조 증가 시퀀스다.
//
// 서버 타임스탬프는 ms 해상도라 한 밀리초 안에 여러 업데이트가 들어온다.
// updateTimestampMs만으로는 순서 결정도 중복 제거도 불가능하므로
// 모든 레코드에 이 시퀀스를 붙인다.
var seq uint64

// NextSeq는 다음 시퀀스 번호를 돌려준다. 여러 goroutine에서 안전하다.
func NextSeq() uint64 { return atomic.AddUint64(&seq, 1) }

// Stamp는 지금의 벽시계(ns)와 프로세스 기준 단조 경과(ns)를 함께 돌려준다.
//
// 반드시 소켓에서 프레임을 읽은 직후에 호출한다. JSON 언마샬 뒤에 호출하면
// 파싱 시간이 지연 측정에 섞인다.
func Stamp() (unixNs, monoNs int64) {
	now := time.Now()
	return now.UnixNano(), now.Sub(start).Nanoseconds()
}

// Uptime은 프로세스 기동 후 경과 시간이다. NTP 점프에 영향받지 않는다.
func Uptime() time.Duration { return time.Since(start) }
