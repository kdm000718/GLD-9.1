package main

import (
	"strings"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/risk"
)

func fullEnv() map[string]string {
	return map[string]string{
		EnvBeatSecret: "s3cret",
		EnvListen:     ":9443",
		EnvTGToken:    "tg-token",
		EnvTGChat:     "42",
		EnvMonAPIKey:  "monitor-key",
		EnvBotAPIKey:  "bot-key",
	}
}

func lookup(env map[string]string) func(string) string {
	return func(k string) string { return env[k] }
}

// **레이트리밋 240 req/min 은 API 키 단위다.** 스펙 §"예산은 키 단위다" 가
// 실측 사고를 기록했다 — 같은 키를 쓴 수집기 3개가 14초 만에 240 을 소진시켜
// 봇 몫이 0 이 됐고 G2 게이트조차 못 돌았다.
//
// 사용자 지시로 봇 키 공유를 허용하되, **공유한다는 사실을 SharedKey 로
// 표시한다** — settle.go 가 그때 페이지 상한을 걸고 봇의 남은 예산이 낮으면
// 조회를 건너뛴다. 표시가 없으면 그 방어가 통째로 꺼진다.
func TestSharedAPIKeyIsFlagged(t *testing.T) {
	env := fullEnv()
	env[EnvMonAPIKey] = "same"
	env[EnvBotAPIKey] = "same"
	c, err := loadMonitorConfig(lookup(env))
	if err != nil {
		t.Fatalf("공유 키가 거부됐다: %v", err)
	}
	if !c.SharedKey {
		t.Error("같은 키인데 SharedKey 가 서지 않았다 — 보수적 조회가 꺼진다")
	}

	env[EnvMonAPIKey] = "different"
	c, err = loadMonitorConfig(lookup(env))
	if err != nil {
		t.Fatalf("다른 키가 거부됐다: %v", err)
	}
	if c.SharedKey {
		t.Error("다른 키인데 SharedKey 가 섰다 — 불필요하게 조회가 느려진다")
	}
}

// 전용 키가 없으면 봇 키로 떨어진다(사용자 지시). 그때도 공유 표시가 선다.
func TestFallsBackToBotKey(t *testing.T) {
	env := fullEnv()
	delete(env, EnvMonAPIKey)
	c, err := loadMonitorConfig(lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != env[EnvBotAPIKey] {
		t.Errorf("APIKey = %q, want 봇 키", c.APIKey)
	}
	if !c.SharedKey {
		t.Error("봇 키로 떨어졌는데 SharedKey 가 서지 않았다")
	}
}

// 키가 하나도 없으면 정산 관측만 꺼지고 기동은 된다 — 하트비트 감시가
// 키 없이도 돌아야 한다.
func TestNoKeyStillStarts(t *testing.T) {
	env := fullEnv()
	delete(env, EnvMonAPIKey)
	delete(env, EnvBotAPIKey)
	c, err := loadMonitorConfig(lookup(env))
	if err != nil {
		t.Fatalf("키 없이 기동이 거부됐다: %v", err)
	}
	if c.APIKey != "" || c.SharedKey {
		t.Errorf("APIKey = %q SharedKey = %v, want 빈값/false", c.APIKey, c.SharedKey)
	}
}

// 없는 것을 한꺼번에 보고한다. 하나씩 알려주면 네 번 실행해야 한다.
func TestMonitorRequiresSecrets(t *testing.T) {
	for _, drop := range []string{EnvBeatSecret, EnvTGToken, EnvTGChat} {
		env := fullEnv()
		delete(env, drop)
		_, err := loadMonitorConfig(lookup(env))
		if err == nil {
			t.Errorf("%s 없이 기동이 허용됐다", drop)
			continue
		}
		if !strings.Contains(err.Error(), drop) {
			t.Errorf("%s 가 없는데 에러가 그것을 안 말한다: %v", drop, err)
		}
	}

	// 여럿이 빠지면 전부 한 번에 말한다.
	env := fullEnv()
	delete(env, EnvBeatSecret)
	delete(env, EnvTGToken)
	_, err := loadMonitorConfig(lookup(env))
	if err == nil {
		t.Fatal("둘이 빠졌는데 통과했다")
	}
	for _, k := range []string{EnvBeatSecret, EnvTGToken} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("에러가 %s 를 안 말한다: %v", k, err)
		}
	}
}

// 공백만 있는 값은 없는 것으로 본다. `export X=` 뒤에 주석을 달다 공백이 남는
// 경우가 실제로 있고, 그 값으로 기동하면 엉뚱한 곳에서 실패한다.
func TestBlankIsMissing(t *testing.T) {
	env := fullEnv()
	env[EnvBeatSecret] = "   "
	if _, err := loadMonitorConfig(lookup(env)); err == nil {
		t.Error("공백만 있는 비밀로 기동했다")
	}
}

func TestChatIDMustBeInteger(t *testing.T) {
	env := fullEnv()
	env[EnvTGChat] = "not-a-number"
	if _, err := loadMonitorConfig(lookup(env)); err == nil {
		t.Error("chat id 가 정수가 아닌데 통과했다")
	}
}

func TestDefaultsApplied(t *testing.T) {
	env := fullEnv()
	delete(env, EnvListen)
	c, err := loadMonitorConfig(lookup(env))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != defaultListen {
		t.Errorf("Listen = %q, want %q", c.Listen, defaultListen)
	}
	if c.ReportInterval != defaultReportInterval {
		t.Errorf("ReportInterval = %v, want %v", c.ReportInterval, defaultReportInterval)
	}
	if c.TGChat != 42 {
		t.Errorf("TGChat = %d", c.TGChat)
	}
}

// **모니터의 예상 상수는 봇 패키지에서 와야 한다.** 리터럴을 적어 두면 봇의
// CapFraction 을 바꿨을 때 모니터가 옛 값을 "정상"으로 확인해 준다 — 상수
// 대조 규칙이 정확히 그 상황을 잡으려고 있는데 그 규칙 자체가 눈이 먼다.
func TestWantConstsComeFromBotPackages(t *testing.T) {
	c := wantConsts()
	if c.CapFraction != risk.CapFraction {
		t.Errorf("CapFraction = %v, want risk.CapFraction(%v)", c.CapFraction, risk.CapFraction)
	}
	if c.DailyFraction != risk.DefaultDailyFraction {
		t.Errorf("DailyFraction = %v, want %v", c.DailyFraction, risk.DefaultDailyFraction)
	}
	if c.ConfidenceThreshold != live.ConfidenceThreshold {
		t.Errorf("ConfidenceThreshold = %v, want %v", c.ConfidenceThreshold, live.ConfidenceThreshold)
	}
	if c.MinOrderUSD != risk.MinOrderUSD {
		t.Errorf("MinOrderUSD = %v, want %v", c.MinOrderUSD, risk.MinOrderUSD)
	}
}
