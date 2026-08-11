package sample

import (
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/bars"
)

// Features 와 Build 가 같은 표본을 채택해야 한다. 하나라도 어긋나면
// 게이트가 검증한 적 없는 입력으로 실거래 예측이 나간다.
//
// 픽스처를 두 벌 돌린다. 온전한 봉만 쓰면 Gap 분기가 한 번도 비교되지
// 않는다 — 결측이 있는 벌을 함께 돌려야 세 사유가 모두 대조된다.
func TestFeaturesAgreesWithBuild(t *testing.T) {
	cases := []struct {
		name  string
		build func(*testing.T) (bars.Bars, bars.Bars)
	}{
		{"온전", func(t *testing.T) (bars.Bars, bars.Bars) { return synthetic(t, 400) }},
		{"결측", func(t *testing.T) (bars.Bars, bars.Bars) {
			b1, b5 := synthetic(t, 400)
			return drop1mBar(b1, b1.Len()-30), b5
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b1, b5 := tc.build(t)
			cs, mat, _, counts := Build(b1, b5, nil)

			accepted := map[int64]bool{}
			for i := 0; i < counts.Kept; i++ {
				accepted[cs[i]] = true
			}
			for i := 0; i < b5.Len(); i++ {
				ts := b5.OpenTime[i]
				vals, reason := Features(b1, b5, ts)
				// 도지는 Build 만의 규칙(라벨이 없다)이므로 제외하고 비교한다.
				if b5.Close[i] == b5.Open[i] {
					continue
				}
				if (reason == Eligible) != accepted[ts] {
					t.Errorf("t=%d: Features reason=%v, Build 채택=%v", ts, reason, accepted[ts])
				}
				if reason == Eligible {
					r := indexOf(cs, counts.Kept, ts)
					if r < 0 {
						// 위 불일치 보고가 이미 원인을 말했다. 여기서
						// mat.Row(-1) 로 패닉을 내면 그 메시지가 묻힌다.
						continue
					}
					row := mat.Row(r)
					for j := range vals {
						if float32(vals[j]) != row[j] {
							t.Errorf("t=%d 피처[%d] 불일치: %v vs %v", ts, j, vals[j], row[j])
							break
						}
					}
				}
			}
		})
	}
}

// 채택되지 않은 시각은 vals 가 nil 이어야 한다. 사유만 보고 값을 쓰는
// 호출자가 채택 실패 시 0값 피처를 넘기는 일이 없어야 한다.
func TestFeaturesNilWhenNotEligible(t *testing.T) {
	b1, b5 := synthetic(t, 400)

	var sawWarmup bool
	for i := 0; i < b5.Len(); i++ {
		vals, reason := Features(b1, b5, b5.OpenTime[i])
		if reason == Warmup {
			sawWarmup = true
		}
		if reason != Eligible && vals != nil {
			t.Fatalf("t=%d: reason=%v 인데 vals 가 %d개 있다", b5.OpenTime[i], reason, len(vals))
		}
		if reason == Eligible && len(vals) == 0 {
			t.Fatalf("t=%d: 채택됐는데 vals 가 비었다", b5.OpenTime[i])
		}
	}
	if !sawWarmup {
		t.Fatal("워밍업 사유가 한 번도 나오지 않았다 — 픽스처가 이 경로를 못 밟는다")
	}
}

// indexOf 는 채택 시각 cs[:n] 에서 ts 의 행 번호를 찾는다.
func indexOf(cs []int64, n int, ts int64) int {
	for i := 0; i < n; i++ {
		if cs[i] == ts {
			return i
		}
	}
	return -1
}
