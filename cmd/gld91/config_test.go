package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/exec"
)

// mainSource 는 main.go 원문이다.
//
// **호출부를 지키는 유일한 수단이라서 있다.** 이 저장소가 반복해서 당한
// 고장은 "함수는 맞는데 부르는 자리가 없다" 이고, 그 자리가 배선(main.go)에
// 있으면 값 시험으로는 절대 잡히지 않는다 — loop() 를 통째로 돌리려면 WS·
// REST·모델이 전부 필요하기 때문이다.
func mainSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean("main.go"))
	if err != nil {
		t.Fatalf("main.go: %v", err)
	}
	return string(b)
}

// LIVE_ARM 이 **정확히** 그 값일 때만 전송한다. 오타·빈 값·"true"·소문자·
// 앞뒤 공백은 전부 DRY-RUN 이다.
//
// 공백 트림을 일부러 하지 않는다. 트림하면 " true " 를 다듬어 주는 습관이
// 생기고, 그 습관은 언젠가 대소문자나 접두사에도 적용된다.
func TestArmedOnlyWithExactValue(t *testing.T) {
	for _, v := range []string{
		"", "true", "TRUE", "1", "yes", "on",
		"I_UNDERSTAND", "i_understand_the_risk", "I_understand_the_risk",
		" I_UNDERSTAND_THE_RISK", "I_UNDERSTAND_THE_RISK ", "\tI_UNDERSTAND_THE_RISK\n",
		"I_UNDERSTAND_THE_RISK_", "XI_UNDERSTAND_THE_RISK",
	} {
		if Armed(v) {
			t.Errorf("%q 로 무장됐다", v)
		}
	}
	if !Armed("I_UNDERSTAND_THE_RISK") {
		t.Error("정확한 값인데 무장되지 않았다")
	}
	// 상수와 판정이 같은 값을 쓰는지. 상수만 바뀌고 판정이 안 바뀌면
	// 문서와 코드가 갈린다.
	if !Armed(LiveArmValue) {
		t.Error("LiveArmValue 로 무장되지 않았다")
	}
}

// armLabel 은 값을 그대로 찍지 않는다 — 로그가 보고서에 붙고 저장소는
// GitHub 에 올라간다.
func TestArmLabelNeverEchoesTheValue(t *testing.T) {
	const secretish = "I_UNDERSTAND_THE_RISK"
	if got := armLabel(secretish); strings.Contains(got, secretish) {
		t.Errorf("armLabel 이 값을 그대로 찍었다: %q", got)
	}
	if got := armLabel("oops-typo"); strings.Contains(got, "oops-typo") {
		t.Errorf("armLabel 이 값을 그대로 찍었다: %q", got)
	}
	if got := armLabel(""); !strings.Contains(got, "DRY-RUN") {
		t.Errorf("미설정인데 DRY-RUN 이라고 말하지 않는다: %q", got)
	}
}

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// 셋 다 있어야 한다. 하나씩 알려주면 세 번 실행해야 하므로 한꺼번에 말한다.
func TestLoadSecretsRequiresAllThree(t *testing.T) {
	full := map[string]string{
		EnvAccount: "0xAAAAbbbbCCCCddddEEEEffff0000111122223333",
		EnvKey:     strings.Repeat("a", 64),
		EnvAPIKey:  "api-key-placeholder",
	}
	if _, err := LoadSecrets(env(full)); err != nil {
		t.Fatalf("셋 다 있는데 실패했다: %v", err)
	}

	for _, missing := range []string{EnvAccount, EnvKey, EnvAPIKey} {
		m := map[string]string{}
		for k, v := range full {
			m[k] = v
		}
		m[missing] = ""
		_, err := LoadSecrets(env(m))
		if err == nil {
			t.Fatalf("%s 가 없는데 통과했다", missing)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("%s 가 빠졌는데 에러가 그 이름을 담지 않았다: %v", missing, err)
		}
	}

	// 공백만 있는 값은 없는 것이다. `export X=` 뒤에 공백이 남는 경우가
	// 실제로 있고, 그 값으로 조회하면 엉뚱한 곳에서 실패한다.
	m := map[string]string{EnvAccount: "   ", EnvKey: full[EnvKey], EnvAPIKey: full[EnvAPIKey]}
	if _, err := LoadSecrets(env(m)); err == nil {
		t.Error("공백만 있는 계정 주소가 통과했다")
	}

	// 셋 다 없으면 셋 다 보고한다.
	_, err := LoadSecrets(env(map[string]string{}))
	if err == nil {
		t.Fatal("전부 없는데 통과했다")
	}
	for _, name := range []string{EnvAccount, EnvKey, EnvAPIKey} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("에러가 %s 를 빠뜨렸다: %v", name, err)
		}
	}
}

// **에러에 값이 실리면 안 된다.** 키가 로그로 새는 가장 흔한 경로가 "친절한"
// 에러 메시지다.
func TestLoadSecretsErrorNeverContainsValues(t *testing.T) {
	const key = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	const api = "super-secret-api-key"
	_, err := LoadSecrets(env(map[string]string{EnvKey: key, EnvAPIKey: api}))
	if err == nil {
		t.Fatal("계정이 없는데 통과했다")
	}
	if strings.Contains(err.Error(), key) || strings.Contains(err.Error(), api) {
		t.Fatalf("에러에 값이 실렸다: %v", err)
	}
}

func baseConfig() *Config {
	c := Flags(flag.NewFlagSet("test", flag.ContinueOnError))
	return c
}

// **가짜 자본으로 진짜 주문을 내는 것은 이 봇이 할 수 있는 가장 나쁜 일이다.**
// 조용히 무시하지 않고 기동을 실패시킨다 — 무시하면 운영자는 자기가 준 값이
// 반영됐다고 믿는다.
func TestCheckConfigRefusesFakeEquityWhenArmed(t *testing.T) {
	c := baseConfig()
	c.Arm = LiveArmValue
	c.DryRunEquityUSDT = 1000
	err := checkConfig(c)
	if err == nil {
		t.Fatal("무장 + 가짜 자본이 통과했다")
	}
	if !strings.Contains(err.Error(), "dry-run-equity") {
		t.Errorf("에러가 원인을 짚지 않는다: %v", err)
	}

	// DRY-RUN 에서는 허용된다 — 그러라고 있는 값이다.
	c.Arm = ""
	if err := checkConfig(c); err != nil {
		t.Errorf("DRY-RUN 인데 가짜 자본을 막았다: %v", err)
	}
}

func TestCheckConfigRejectsBadValues(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"stale-after 0 (설정 안 됨)", func(c *Config) { c.StaleAfter = 0 }, "stale-after"},
		{"stale-after 음수", func(c *Config) { c.StaleAfter = -time.Second }, "stale-after"},
		{"poll 0", func(c *Config) { c.Poll = 0 }, "poll"},
		{"round-poll 0", func(c *Config) { c.RoundPoll = 0 }, "round-poll"},
		{"minutes 음수", func(c *Config) { c.Minutes = -1 }, "minutes"},
		{"max-join-late 음수", func(c *Config) { c.MaxJoinLate = -time.Second }, "max-join-late"},
		// **0 은 '창 없음'이 아니라 '설정 안 됨'이다.** 그 값이면 어떤 회차도
		// 잡히지 않아 봇이 조용히 아무것도 하지 않는다.
		{"max-join-late 0", func(c *Config) { c.MaxJoinLate = 0 }, "max-join-late"},
		{"가짜 자본 음수", func(c *Config) { c.DryRunEquityUSDT = -1 }, "dry-run-equity"},
		{"다른 심볼", func(c *Config) { c.Symbol = "eth" }, "symbol"},
		{"exchange 가 주소가 아니다", func(c *Config) { c.Exchange = "0xdead" }, "exchange"},
		{"validator 가 주소가 아니다", func(c *Config) { c.Validator = "" }, "validator"},
		{"chain-id 0", func(c *Config) { c.ChainID = 0 }, "chain-id"},
		// **모르는 체인은 기동에서 잡는다.** 통과시키면 회차마다 Exchange 를
		// 고르는 경로가 매번 실패하고, 증상은 "회차마다 주문 실패" 로만 보인다.
		{"모르는 chain-id", func(c *Config) { c.ChainID = 1 }, "chain-id"},
		{"fills-poll 0", func(c *Config) { c.FillsPoll = 0 }, "fills-poll"},
		{"fills-poll 음수", func(c *Config) { c.FillsPoll = -time.Second }, "fills-poll"},
		{"fills-poll 상한 초과", func(c *Config) { c.FillsPoll = MaxFillsPollInterval + time.Second }, "fills-poll"},
		{"원장 경로가 비었다", func(c *Config) { c.LedgerPath = "" }, "ledger"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := baseConfig()
			tc.mut(c)
			err := checkConfig(c)
			if err == nil {
				t.Fatal("통과했다")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("에러가 %q 를 담지 않았다: %v", tc.want, err)
			}
		})
	}
}

// 기본값 그대로는 통과해야 한다. 통과하지 못하면 봇이 아예 못 뜬다.
func TestCheckConfigAcceptsDefaults(t *testing.T) {
	if err := checkConfig(baseConfig()); err != nil {
		t.Fatalf("기본 설정이 통과하지 못했다: %v", err)
	}
}

// 잠금 파일은 원장과 짝이어야 한다 — 잠그는 대상이 프로세스가 아니라 원장이다.
func TestLockPathFollowsLedger(t *testing.T) {
	c := baseConfig()
	c.LedgerPath = "/tmp/x/ledger.csv"
	if got := c.lockPath(); got != "/tmp/x/ledger.csv.lock" {
		t.Errorf("lockPath = %q", got)
	}
	c.LockPath = "/tmp/other.lock"
	if got := c.lockPath(); got != "/tmp/other.lock" {
		t.Errorf("명시한 잠금 경로를 무시했다: %q", got)
	}
}

// **-exchange 는 비어 있는 것이 기본이자 정상이다.** 예전에는 CTF_EXCHANGE
// 주소가 기본값으로 박혀 있었는데, 그 상태는 거래소가 이 상품을 negRisk 로
// 옮기는 날 조용히 틀린 계약에 서명한다 — 상수가 아니라 회차 데이터가
// 정해야 하는 값이다(order.ExchangeFor).
func TestExchangeDefaultsToEmptySoTheRoundDecides(t *testing.T) {
	c := baseConfig()
	if c.Exchange != "" {
		t.Fatalf("-exchange 기본값이 %q 다 — 비어 있어야 회차마다 고른다", c.Exchange)
	}
	if err := checkConfig(c); err != nil {
		t.Fatalf("빈 -exchange 를 막았다: %v", err)
	}
	// 값을 주면 주소여야 한다(탈출구는 남기되 오타는 막는다).
	c.Exchange = fixtureExchange
	if err := checkConfig(c); err != nil {
		t.Errorf("유효한 -exchange 를 막았다: %v", err)
	}
}

// 체결 조회 주기의 기본값이 곧 노출 갱신 지연이다. 기본값이 상한 안에 있어야
// 봇이 아예 뜬다.
func TestFillsPollDefault(t *testing.T) {
	c := baseConfig()
	if c.FillsPoll != DefaultFillsPollInterval {
		t.Errorf("-fills-poll 기본값이 %s 다", c.FillsPoll)
	}
	if DefaultFillsPollInterval <= 0 || DefaultFillsPollInterval > MaxFillsPollInterval {
		t.Errorf("기본 주기 %s 가 스스로 상한을 벗어난다", DefaultFillsPollInterval)
	}
}

// **설정의 시간 값이 집행자에게 실제로 닿는지 본다.**
//
// 특히 EntryWindow 다. 그 값 하나가 "회차 중간에는 걸지 않는다"를 정하는데,
// 구조체 리터럴 안에 있는 동안에는 어떤 시험도 그것을 보지 못했다 — 10분으로
// 바꿔 놓아도 전부 통과했다.
func TestTimingReachesTheRunner(t *testing.T) {
	cfg := &Config{
		StaleAfter:  7 * time.Second,
		MaxJoinLate: 11 * time.Second,
		Poll:        13 * time.Millisecond,
	}
	var r exec.Runner
	applyTiming(&r, cfg)

	if r.EntryWindow != cfg.MaxJoinLate {
		t.Errorf("EntryWindow = %s, want %s — 회차 중간 진입을 막는 값이다", r.EntryWindow, cfg.MaxJoinLate)
	}
	if r.StaleAfter != cfg.StaleAfter {
		t.Errorf("StaleAfter = %s, want %s", r.StaleAfter, cfg.StaleAfter)
	}
	if r.Poll != cfg.Poll {
		t.Errorf("Poll = %s, want %s", r.Poll, cfg.Poll)
	}
}

// 기본값도 못 박는다. 실측(291회차)에서 첫 주문의 p90 이 0.60초였고 10초를
// 넘긴 것은 재시작 뒤 늦은 합류 2건뿐이다 — 기본이 조용히 늘어나면 그 2건이
// 다시 정상 주문처럼 원장에 들어온다.
func TestDefaultEntryWindowIsTight(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c := Flags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	if c.MaxJoinLate != exec.DefaultEntryWindow {
		t.Errorf("-max-join-late 기본 %s, want %s", c.MaxJoinLate, exec.DefaultEntryWindow)
	}
	if c.MaxJoinLate > 10*time.Second {
		t.Errorf("-max-join-late 기본이 %s 다 — 회차 중간 진입을 허용하는 값이다", c.MaxJoinLate)
	}
}

// applyTiming 이 값을 옮기는 것과, main.go 가 **그것을 부르는 것**은 다른
// 문제다. 호출부가 사라지고 리터럴이 돌아오면 위 시험은 그대로 통과한다.
func TestRunnerIsBuiltThroughApplyTiming(t *testing.T) {
	if !strings.Contains(mainSource(t), "applyTiming(runner, cfg)") {
		t.Error("main.go 가 applyTiming(runner, cfg) 를 부르지 않는다 — EntryWindow 가 배선되지 않으면 회차 중간에도 걸린다")
	}
}
