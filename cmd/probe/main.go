// Command probe는 predict.fun testnet 왕복을 실측 확인하는 진단 도구다
// (Task 10, P4).
//
// **이 패스(2026-08-09)의 범위는 -mode read 와 -mode auth 뿐이다.** 온체인
// 승인(Step 4)과 실주문 왕복(Step 5)은 이번 패스에서 만들지 않는다 — 자금이
// 준비되면 별도로 만든다. 이 바이너리는 LIVE_ARM 을 읽지 않고, 온체인
// 트랜잭션을 보내지 않고, 주문을 전송하지 않는다.
//
//	go run ./cmd/probe -env testnet -mode read -minutes 60
//	go run ./cmd/probe -env testnet -mode auth
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

const (
	restTestnetURL = "https://api-testnet.predict.fun"
	restMainnetURL = "https://api.predict.fun"
	wsURL          = "wss://ws.predict.fun/ws"
)

func main() {
	env := flag.String("env", "testnet", "testnet | mainnet")
	mode := flag.String("mode", "read", "read | auth")
	minutes := flag.Float64("minutes", 60, "mode=read일 때 실행 시간(분)")
	symbol := flag.String("symbol", "btc", "mode=read일 때 추적할 심볼 접두사 (예: btc, eth)")
	pollSec := flag.Int("poll-sec", 20, "mode=read일 때 카테고리 재조회 주기(초)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	baseURL, err := restBaseURL(*env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(2)
	}

	// testnet은 API 키가 없어야 정상이다 — 실측 확인(2026-08-09):
	// 키 없이 GET /v1/categories가 HTTP 200을 준다. mainnet에서만
	// PREDICT_API_KEY를 쓴다. testnet 실행에 실수로 mainnet 키를 흘리지 않는다.
	apiKey := ""
	if *env == "mainnet" {
		apiKey = os.Getenv("PREDICT_API_KEY")
		if apiKey == "" {
			fmt.Fprintln(os.Stderr, "실패: -env mainnet에는 PREDICT_API_KEY가 필요하다")
			os.Exit(2)
		}
	} else if os.Getenv("PREDICT_API_KEY") != "" {
		log.Warn("PREDICT_API_KEY가 설정돼 있지만 testnet에서는 쓰지 않는다 — 무시한다")
	}

	rc := rest.New(apiKey)
	rc.BaseURL = baseURL

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch *mode {
	case "read":
		err = runRead(ctx, rc, *symbol, *minutes, time.Duration(*pollSec)*time.Second, log)
	case "auth":
		err = runAuth(ctx, rc, log)
	default:
		fmt.Fprintf(os.Stderr, "실패: -mode는 read 또는 auth여야 한다 (받은 값 %q)\n", *mode)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(1)
	}
}

func restBaseURL(env string) (string, error) {
	switch env {
	case "testnet":
		return restTestnetURL, nil
	case "mainnet":
		return restMainnetURL, nil
	default:
		return "", fmt.Errorf("-env는 testnet 또는 mainnet이어야 한다 (받은 값 %q)", env)
	}
}
