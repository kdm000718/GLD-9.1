// gld91-claim 은 정산이 끝난 포지션을 회수한다 (Auto-Claim).
//
// 조립·서명·전송·영수증 확인은 전부 `internal/claim` 에 있고, 이 바이너리는
// 설정을 읽어 그것을 돌리고 결과를 찍는다.
//
// # 기본값은 보내지 않는다
//
// `CLAIM_ARM` 이 **정확히** `I_UNDERSTAND_THE_RISK` 일 때만 전송한다. 그
// 전에는 조립·서명까지 하고 무엇을 보낼 것인지 찍는다 — cmd/gld91 의
// `LIVE_ARM` 과 같은 규약이다. 자금이 움직이는 경로의 기본값은 "안 한다"다.
//
// # 기동 자가 점검
//
// 보내기 전에 **키가 그 계정의 등록 서명자인지 체인에서 확인한다**
// (`internal/kernel` — cmd/signercheck 와 글자 그대로 같은 함수다). 이것이
// 틀리면 UserOperation 이 전부 거부되는데, 그 거부 사유는 인코딩 오류와
// 구분되지 않는다.
//
// # 사용법
//
//	set -a; . ~/.config/predictfun/env; set +a
//
//	# 무엇을 보낼지만 본다 (전송 없음)
//	GOTOOLCHAIN=local go run ./cmd/gld91-claim
//
//	# 실제로 회수한다
//	CLAIM_ARM=I_UNDERSTAND_THE_RISK GOTOOLCHAIN=local go run ./cmd/gld91-claim
//
//	# 주기적으로 돈다
//	CLAIM_ARM=I_UNDERSTAND_THE_RISK go run ./cmd/gld91-claim -interval 5m
//
// 종료 코드: 성공 0, 회수 실패가 있으면 1, 설정·기동 실패 2.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/claim"
	"github.com/kdm000718/GLD-9.1/internal/kernel"
	"github.com/kdm000718/GLD-9.1/internal/ledger"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
)

// EnvClaimArm 은 전송을 여는 환경변수다. 값이 정확히 [ArmValue] 여야 한다.
const (
	EnvClaimArm = "CLAIM_ARM"
	ArmValue    = "I_UNDERSTAND_THE_RISK"
)

func main() {
	interval := flag.Duration("interval", 0, "회수 주기. 0 이면 한 번만 돌고 끝난다")
	ledgerPath := flag.String("ledger", "", "정산 행을 적을 원장 CSV 경로. 비면 적지 않는다")
	chainRPC := flag.String("rpc", claim.DefaultChainRPC, "nonce·서명자 조회용 BSC JSON-RPC")
	flag.Parse()

	if err := run(*interval, *ledgerPath, *chainRPC); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(exitCode(err))
	}
}

// errClaimFailed 는 "설정은 맞았는데 회수 중 실패가 있었다"다. 설정 실패와
// 종료 코드를 나눠야 스크립트가 재시도할 것과 사람을 불러야 할 것을 구분한다.
var errClaimFailed = errors.New("회수 실패가 있었다")

func exitCode(err error) int {
	if errors.Is(err, errClaimFailed) {
		return 1
	}
	return 2
}

func run(interval time.Duration, ledgerPath, chainRPC string) error {
	account := os.Getenv("PREDICT_ACCOUNT")
	if account == "" {
		return fmt.Errorf("PREDICT_ACCOUNT 가 비어 있다 — `set -a; . ~/.config/predictfun/env; set +a` 를 먼저 실행하라")
	}
	keyHex := os.Getenv("WALLET_PRIVATE_KEY")
	if keyHex == "" {
		return fmt.Errorf("WALLET_PRIVATE_KEY 가 비어 있다")
	}
	zerodev := os.Getenv("ZERODEV_RPC")
	if zerodev == "" {
		return fmt.Errorf("ZERODEV_RPC 가 비어 있다 — 번들러 없이는 UserOperation 을 보낼 수 없다")
	}

	// 키에서 주소만 유도한다. 파싱 에러를 감싸지 않는 것은 cmd/signercheck 와
	// 같은 이유다 — 메시지에 키가 섞여 나갈 경로를 만들지 않는다.
	signer, err := auth.NewSigner(keyHex)
	if err != nil {
		return fmt.Errorf("WALLET_PRIVATE_KEY 를 키로 읽지 못했다 (64자리 hex 여야 한다)")
	}

	armed := os.Getenv(EnvClaimArm) == ArmValue
	fmt.Println("계정      :", account)
	fmt.Println("서명자    :", signer.Address().Hex())
	fmt.Println("번들러    :", claim.Redact(zerodev))
	fmt.Println("전송      :", armLabel(armed))

	// 자가 점검. 키가 이 계정의 등록 서명자가 아니면 아무것도 하지 않는다.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	v := kernel.Verifier{RPC: chainRPC}
	if err := v.Verify(ctx, account, signer.Address().Hex()); err != nil {
		return fmt.Errorf("기동 자가 점검(등록 서명자 대조): %w", err)
	}
	fmt.Println("자가 점검 : 통과 — 키가 이 계정의 등록 서명자다")

	var led *ledger.Ledger
	if ledgerPath != "" {
		led, err = ledger.Open(ledgerPath)
		if err != nil {
			return fmt.Errorf("원장 열기: %w", err)
		}
		defer led.Close()
		fmt.Println("원장      :", ledgerPath)
	}

	c := &claim.Claimer{
		Account: account,
		Signer:  signer,
		Bundler: claim.Bundler{RPC: zerodev, ChainRPC: chainRPC},
		Ledger:  led,
		Send:    armed,
	}

	if interval <= 0 {
		return once(ctx, c)
	}

	fmt.Printf("주기      : %s\n", interval)
	t := time.NewTicker(interval)
	defer t.Stop()
	// 첫 바퀴는 기다리지 않고 바로 돈다. 실패는 로그로 남기고 계속한다 —
	// 주기 모드에서 한 번의 실패로 죽으면 다음 회차 회수까지 멈춘다.
	var lastErr error
	for {
		if err := once(ctx, c); err != nil {
			if errors.Is(err, errClaimFailed) {
				fmt.Fprintln(os.Stderr, "이번 주기 실패:", err)
				lastErr = err
			} else {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return lastErr
		case <-t.C:
		}
	}
}

func once(ctx context.Context, c *claim.Claimer) error {
	res, err := c.Run(ctx)
	if err != nil {
		return err
	}
	report(res, c.Send)
	if res.Failed() > 0 {
		return fmt.Errorf("%w (%d개 시장)", errClaimFailed, res.Failed())
	}
	return nil
}

func report(res claim.Result, sent bool) {
	stamp := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if len(res.Markets) == 0 && len(res.Skipped) == 0 {
		fmt.Printf("[%s] 회수 대상 없음\n", stamp)
		return
	}
	for _, s := range res.Skipped {
		fmt.Printf("[%s] 건너뜀 — %s (%s, %.6f주): %s\n",
			stamp, s.Position.Title, s.Position.OutcomeName, s.Position.Shares, s.Reason)
	}
	for _, m := range res.Markets {
		var parts []string
		for _, p := range m.Positions {
			parts = append(parts, fmt.Sprintf("%s %.6f주(%s)", p.OutcomeName, p.Shares, wonLabel(p.Won)))
		}
		head := fmt.Sprintf("[%s] %s [%s]", stamp, m.Title, strings.Join(parts, ", "))
		switch {
		case m.Err != nil:
			fmt.Printf("%s\n  실패: %v\n", head, m.Err)
		case !sent:
			fmt.Printf("%s\n  조립 완료(전송 안 함) — userOpHash %s, callData %d바이트\n"+
				"  보내려면 %s=%s 로 다시 실행하라\n",
				head, m.UserOpHash, len(m.CallData), EnvClaimArm, ArmValue)
		default:
			fmt.Printf("%s\n  회수 완료 — tx %s\n", head, m.TxHash)
			if m.LedgerNote != "" {
				fmt.Printf("  %s\n", m.LedgerNote)
			}
		}
	}
	if sent {
		fmt.Printf("[%s] 요약 — 회수 %d, 실패 %d, 건너뜀 %d\n",
			stamp, res.Claimed(), res.Failed(), len(res.Skipped))
	}
}

func armLabel(armed bool) string {
	if armed {
		return "켜짐 — 실제로 회수한다"
	}
	return "꺼짐 — 조립까지만 한다 (" + EnvClaimArm + " 미설정)"
}

func wonLabel(won bool) string {
	if won {
		return "승"
	}
	return "패"
}
