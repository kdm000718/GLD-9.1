// signercheck는 WALLET_PRIVATE_KEY가 PREDICT_ACCOUNT의 등록된 서명자인지를
// 체인에서 직접 확인한다.
//
// **왜 이 도구가 필요한가**: predict.fun 계정은 ZeroDev Kernel 스마트 계정이고,
// 자금을 들고 있는 계정 주소와 주문에 서명하는 EOA가 다르다. 키가 그 계정의
// 등록 서명자가 아니면 **주문 서명이 전부 거부된다** — 코드가 아무리 맞아도.
// 실제로 2026-08-10에 그 일이 있었다: 준비된 키의 EOA가 등록 서명자와 달랐고,
// 이 대조를 하지 않았다면 Step 5(실주문)에서야 알았을 것이다. 그때는 승인
// 트랜잭션에 이미 가스를 쓴 뒤다.
//
// **대조 자체는 internal/kernel 에 있다.** 이 바이너리와 cmd/gld91 의 기동
// 자가 점검이 **같은 함수**를 부른다 — 두 벌로 두면 갈리고, 갈린 쪽이 틀렸을
// 때 실거래가 그 틀린 쪽을 탄다(이 저장소가 회차 선택에서 이미 치른 값이다).
//
// 키는 어떤 경로로도 출력하지 않는다 — 유도된 주소만 찍는다.
//
// 사용법:
//
//	set -a; . ~/.config/predictfun/env; set +a
//	GOTOOLCHAIN=local go run ./cmd/signercheck
//
// 종료 코드: 일치 0, 불일치 1, 조회/설정 실패 2. 스크립트가 게이트로 쓸 수 있다.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/kdm000718/GLD-9.1/internal/kernel"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/auth"
)

func main() {
	rpc := flag.String("rpc", kernel.DefaultRPC, "BSC JSON-RPC 엔드포인트")
	validator := flag.String("validator", kernel.ECDSAValidator, "ECDSA_VALIDATOR 컨트랙트 주소")
	flag.Parse()

	if err := run(*rpc, *validator); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(exitCode(err))
	}
}

func exitCode(err error) int {
	var m *kernel.MismatchError
	if errors.As(err, &m) {
		return 1
	}
	return 2
}

func run(rpc, validator string) error {
	acct := os.Getenv("PREDICT_ACCOUNT")
	if acct == "" {
		return fmt.Errorf("PREDICT_ACCOUNT 가 비어 있다 — `set -a; . ~/.config/predictfun/env; set +a` 를 먼저 실행하라")
	}
	key := os.Getenv("WALLET_PRIVATE_KEY")
	if key == "" {
		return fmt.Errorf("WALLET_PRIVATE_KEY 가 비어 있다 — `set -a; . ~/.config/predictfun/env; set +a` 를 먼저 실행하라")
	}

	// 키에서 주소만 유도한다. 실패 메시지에 키가 섞여 나가지 않도록 원본
	// 에러를 그대로 올리지 않는다 — go-ethereum 의 파싱 에러는 입력을 담지
	// 않지만, 그 보장에 기대지 않는다.
	signer, err := auth.NewSigner(key)
	if err != nil {
		return fmt.Errorf("WALLET_PRIVATE_KEY 를 키로 읽지 못했다 (64자리 hex 여야 한다)")
	}
	derived, err := kernel.NormalizeAddress(signer.Address().Hex())
	if err != nil {
		return fmt.Errorf("유도된 주소: %w", err)
	}

	v := kernel.Verifier{RPC: rpc, Validator: validator}
	registered, err := v.RegisteredSigner(context.Background(), acct)

	fmt.Println("계정(PREDICT_ACCOUNT):", acct)
	fmt.Println("키에서 유도된 서명자 :", "0x"+derived)

	var none *kernel.NoSignerError
	if errors.As(err, &none) {
		fmt.Println("계정 등록 서명자     : (없음 — 0 주소)")
		return err
	}
	if err != nil {
		return err
	}
	fmt.Println("계정 등록 서명자     :", "0x"+registered)

	if !kernel.Match(derived, registered) {
		fmt.Println("판정: ❌ 불일치 — 이 키로 만든 주문 서명은 이 계정에서 거부된다")
		return &kernel.MismatchError{Derived: derived, Registered: registered}
	}
	fmt.Println("판정: ✅ 일치 — 이 키로 이 계정의 주문에 서명할 수 있다")
	return nil
}
