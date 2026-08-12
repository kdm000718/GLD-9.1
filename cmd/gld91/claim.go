package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/claim"
	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
)

// Auto-Claim 배선. 조립·서명·전송·영수증은 전부 `internal/claim` 에 있고,
// 이 파일은 그것을 **회차가 끝날 때마다** 한 바퀴 돌린다.
//
// # 왜 배경에서 도는가
//
// 회수 한 건은 조회 → nonce → 후원 → 서명 → 전송 → 영수증까지 가고, 영수증
// 폴링만 최대 90초다. 회차 종료 직후에 그것을 기다리면 **다음 회차의 첫
// 호가가 그만큼 늦는다.** 이 봇은 회차 시작에 한 번 거는 것이 전부이므로
// (exec 패키지 문서) 그 지연이 곧 큐 위치이고, 큐 위치가 체결을 지배한다.
//
// # 겹치지 않는다
//
// 회수 자체는 멱등이다 — 이미 회수된 포지션은 redeem 이 0 을 준다. 그러나
// 같은 계정에 UserOperation 두 개를 동시에 띄우면 **nonce 가 겹쳐 하나는
// 반드시 실패한다**(claim.Bundler.Nonce 가 같은 key 의 다음 시퀀스를 준다).
// 그래서 앞 바퀴가 아직 돌고 있으면 이번 회차는 건너뛴다 — 회차는 5분마다
// 오므로 다음 기회는 곧 온다.
//
// # 거래를 막지 않는다
//
// 회수 실패는 로그로만 남는다. 무장 해제도, 회차 중단도 아니다. Auto-Claim
// 은 자금 회수를 빠르게 할 뿐이고(docs/auto-claim-spec.md 「순서에 대한
// 경고」), 그것이 실패했다고 거래를 멈추면 감시 장치의 고장이 거래 장애가
// 되는 것과 같은 실수다.

const (
	// EnvZeroDevRPC 는 번들러+페이마스터 엔드포인트다. **없으면 Auto-Claim 을
	// 켜지 않는다** — 번들러 없이는 UserOperation 을 보낼 수 없다.
	//
	// 이 값에는 ZeroDev 프로젝트 ID 가 들어 있다. 로그에 찍을 때는 반드시
	// [claim.Redact] 를 거친다.
	EnvZeroDevRPC = "ZERODEV_RPC"

	// EnvClaimArm 은 회수 **전송**을 여는 환경변수다. cmd/gld91-claim 이 쓰는
	// 것과 같은 이름이고, 값도 [LiveArmValue] 와 같은 문자열이다.
	//
	// **LIVE_ARM 과 별개인 것이 의도다.** 주문 전송은 새 위험을 만들지만 회수는
	// 이미 정산된 포지션을 담보로 되돌릴 뿐이다. DRY-RUN 으로 도는 봇이 밀린
	// 회수를 처리하는 것은 말이 되고, 그 반대(무장했으니 회수도 자동으로 켜짐)는
	// 말이 안 된다.
	EnvClaimArm = "CLAIM_ARM"
)

// claimRunTimeout 은 한 바퀴의 상한이다.
//
// 회차 간격(300초)보다 짧아야 한다. 길면 한 바퀴가 다음 회차의 기회까지
// 먹어 버리고, 멈춘 바퀴 하나가 회수를 영원히 막는다.
const claimRunTimeout = 4 * time.Minute

// claimArmed 는 이 값이 회수 전송을 뜻하는지다. 문자열 완전 일치 하나뿐 —
// [Armed] 와 같은 규약이고, 같은 이유로 공백을 다듬지 않는다.
func claimArmed(v string) bool { return v == LiveArmValue }

// autoClaim 은 회차 종료마다 회수 한 바퀴를 배경에서 돌린다.
//
// nil 이어도 모든 메서드가 안전하다 — Auto-Claim 이 꺼진 것이 거래를 막으면
// 안 된다(beatWire 와 같은 규약).
type autoClaim struct {
	claimer *claim.Claimer
	log     func(string, ...any)
	armed   bool

	// busy 는 단일 비행 표시다. 앞 바퀴가 돌고 있으면 새 바퀴를 띄우지 않는다.
	busy atomic.Bool
	wg   sync.WaitGroup
}

// newAutoClaim 은 배선을 만든다. 켤 수 없으면 **이유를 찍고** nil 을 돌려준다 —
// 조용히 꺼지면 운영자는 회수가 되고 있다고 믿는다.
//
// 기동 자가 점검(키가 이 계정의 등록 서명자인가)은 여기서 다시 하지 않는다.
// run() 의 자가 점검 2/5 가 cmd/signercheck·cmd/gld91-claim 과 **글자 그대로
// 같은 함수**로 이미 했고, 그것이 실패하면 이 코드까지 오지 못한다.
func newAutoClaim(cfg *Config, account string, signer *auth.Signer, led *ledger.Ledger,
	getenv func(string) string, log func(string, ...any)) *autoClaim {

	if !cfg.AutoClaim {
		log("Auto-Claim: 꺼짐 (-auto-claim=false) — 정산된 포지션은 계정에 그대로 남는다")
		return nil
	}
	zerodev := strings.TrimSpace(getenv(EnvZeroDevRPC))
	if zerodev == "" {
		log("Auto-Claim: 꺼짐 — %s 가 없다. 번들러 없이는 UserOperation 을 보낼 수 없다."+
			" 정산된 포지션은 계정에 남고, `make claim` 으로 손으로 회수해야 한다", EnvZeroDevRPC)
		return nil
	}
	armed := claimArmed(getenv(EnvClaimArm))

	a := &autoClaim{
		claimer: &claim.Claimer{
			Account: account,
			Signer:  signer,
			Bundler: claim.Bundler{RPC: zerodev, ChainRPC: cfg.RPC},
			Ledger:  led,
			Send:    armed,
		},
		log:   log,
		armed: armed,
	}
	log("Auto-Claim: 켜짐 — 회차 종료마다 한 바퀴. 번들러 %s, 전송 %s",
		claim.Redact(zerodev), claimArmLabel(armed))
	return a
}

func claimArmLabel(armed bool) string {
	if armed {
		return "켜짐 (실제로 회수한다)"
	}
	return "꺼짐 (조립·서명까지만 — " + EnvClaimArm + " 미설정)"
}

// after 는 회차가 끝난 뒤 한 바퀴를 **배경에서** 띄운다. 곧바로 돌아온다.
func (a *autoClaim) after(ctx context.Context, slug string) {
	if a == nil {
		return
	}
	if !a.busy.CompareAndSwap(false, true) {
		// 앞 바퀴가 아직 돈다. 겹쳐 띄우면 nonce 가 겹친다.
		a.log("Auto-Claim [%s]: 앞 바퀴가 아직 돌고 있다 — 이번 회차는 건너뛴다", slug)
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer a.busy.Store(false)
		a.once(ctx, slug)
	}()
}

// once 는 한 바퀴다. **에러를 밖으로 내보내지 않는다** — 회수 실패는 로그다.
func (a *autoClaim) once(ctx context.Context, slug string) {
	cctx, cancel := context.WithTimeout(ctx, claimRunTimeout)
	defer cancel()

	res, err := a.claimer.Run(cctx)
	if err != nil {
		if ctx.Err() != nil {
			return // 종료 중이다. 이것은 실패가 아니다.
		}
		a.log("Auto-Claim [%s]: 실패 — %v", slug, err)
		return
	}
	a.report(slug, res)
}

// report 는 한 바퀴의 결과를 **로그 몇 줄로** 줄인다.
//
// cmd/gld91-claim 의 report 와 달리 시장마다 여러 줄을 찍지 않는다. 이쪽은
// 회차마다(=5분마다) 불리므로, 같은 분량으로 찍으면 회차 로그가 회수 보고에
// 묻힌다. 자세히 봐야 하면 `make claim` 이 있다.
func (a *autoClaim) report(slug string, res claim.Result) {
	if len(res.Markets) == 0 && len(res.Skipped) == 0 {
		return // 회수 대상 없음. 대부분의 회차가 이쪽이고, 조용한 것이 맞다.
	}
	for _, s := range res.Skipped {
		a.log("Auto-Claim [%s]: 건너뜀 — %s (%s, %.6f주): %s",
			slug, s.Position.Title, s.Position.OutcomeName, s.Position.Shares, s.Reason)
	}
	for _, m := range res.Markets {
		switch {
		case m.Err != nil:
			// **실패는 시장마다 찍는다.** 요약에 묻으면 어느 시장이 왜 막혔는지
			// 알 수 없고, 그 상태는 다음 회차에 그대로 반복된다.
			a.log("Auto-Claim [%s]: 실패 — %s: %v", slug, m.Title, m.Err)
		case !a.armed:
			a.log("Auto-Claim [%s]: 조립 완료(전송 안 함) — %s, userOpHash %s."+
				" 보내려면 %s=%s 로 재기동하라", slug, m.Title, m.UserOpHash, EnvClaimArm, LiveArmValue)
		default:
			line := fmt.Sprintf("Auto-Claim [%s]: 회수 완료 — %s, tx %s", slug, m.Title, m.TxHash)
			if m.LedgerNote != "" {
				line += " (" + m.LedgerNote + ")"
			}
			a.log("%s", line)
		}
	}
	if a.armed {
		a.log("Auto-Claim [%s]: 요약 — 회수 %d, 실패 %d, 건너뜀 %d",
			slug, res.Claimed(), res.Failed(), len(res.Skipped))
	}
}

// wait 는 도는 바퀴가 끝나기를 기다린다. **종료 경로에서 ctx 를 먼저 취소한
// 뒤에 불러야 한다** — 안 그러면 영수증 폴링 90초를 그대로 기다린다.
//
// 기다리는 이유는 원장이다. 이것을 안 기다리면 회수에 성공한 바퀴가 이미
// 닫힌 원장에 정산 행을 쓰려다 실패하고, 돈은 돌아왔는데 기록만 사라진다.
func (a *autoClaim) wait() {
	if a == nil {
		return
	}
	a.wg.Wait()
}
