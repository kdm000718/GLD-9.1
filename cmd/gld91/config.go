package main

import (
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/exec"
	"github.com/kdm000718/GLD-9.1/internal/kernel"
	"github.com/kdm000718/GLD-9.1/internal/live"
	"github.com/kdm000718/GLD-9.1/internal/predictfun/order"
)

// LiveArmValue 는 무장에 필요한 `LIVE_ARM` 의 **정확한** 값이다.
//
// 정확히 이 값일 때만 주문이 나간다. `"true"`·`"1"`·소문자·앞뒤 공백은 전부
// DRY-RUN 이다. 공백을 트림하지 않는 것이 의도다 — 트림하면 " true " 같은
// 값을 다듬어 주는 습관이 생기고, 그 습관은 언젠가 대소문자나 접두사에도
// 적용된다. 무장은 실수로 켜질 수 없어야 한다.
const LiveArmValue = "I_UNDERSTAND_THE_RISK"

// Armed 는 이 값이 무장을 뜻하는지 본다. 문자열 완전 일치 하나뿐이다.
func Armed(v string) bool { return v == LiveArmValue }

// 환경변수 이름. 리터럴을 여기저기 적으면 오타 하나가 "설정 안 됨"으로 보인다.
const (
	EnvAccount = "PREDICT_ACCOUNT"
	EnvKey     = "WALLET_PRIVATE_KEY"
	EnvAPIKey  = "PREDICT_API_KEY"
	EnvLiveArm = "LIVE_ARM"
)

// Secrets 는 환경변수로만 들어오는 값이다.
//
// **String() 도 Format 도 만들지 않는다.** 만들면 언젠가 로그 한 줄에 통째로
// 실린다. 이 구조체를 통째로 찍는 코드가 생기면 %+v 가 키를 그대로 뱉는데,
// 그것을 막는 유일한 방법은 애초에 찍을 일이 없게 두는 것이다.
type Secrets struct {
	Account    string
	PrivateKey string
	APIKey     string
}

// LoadSecrets 는 자가 점검 1번이다 — 세 환경변수가 **전부** 있어야 한다.
//
// 공백만 있는 값은 없는 것으로 본다. `export PREDICT_ACCOUNT=` 뒤에 주석을
// 달다 공백이 남는 경우가 실제로 있고, 그 값으로 조회하면 "40자리 hex 가
// 아니다" 라는 엉뚱한 곳에서 실패한다.
//
// 세 개를 **한꺼번에** 보고한다. 하나씩 알려주면 세 번 실행해야 한다.
func LoadSecrets(getenv func(string) string) (Secrets, error) {
	s := Secrets{
		Account:    strings.TrimSpace(getenv(EnvAccount)),
		PrivateKey: strings.TrimSpace(getenv(EnvKey)),
		APIKey:     strings.TrimSpace(getenv(EnvAPIKey)),
	}
	var missing []string
	if s.Account == "" {
		missing = append(missing, EnvAccount)
	}
	if s.PrivateKey == "" {
		missing = append(missing, EnvKey)
	}
	if s.APIKey == "" {
		missing = append(missing, EnvAPIKey)
	}
	if len(missing) > 0 {
		return Secrets{}, fmt.Errorf("환경변수가 비어 있다: %s — `set -a; . ~/.config/predictfun/env; set +a` 를 먼저 실행하라",
			strings.Join(missing, ", "))
	}
	return s, nil
}

// 기본 엔드포인트·계약. 메인넷 값이다.
const (
	defaultRestURL = "https://api.predict.fun"
	defaultWSURL   = "wss://ws.predict.fun/ws"

	// defaultChainID 는 BNB 메인넷이다. EIP-712 도메인에 들어가므로 틀리면
	// 서명이 통째로 거부된다.
	defaultChainID = order.ChainIDMainnet
)

// Config 는 이 실행의 전부다. 값은 플래그와 환경변수에서만 온다.
type Config struct {
	Secrets

	// Arm 은 `LIVE_ARM` 의 원본 값이다. 판정은 Armed 가 한다.
	Arm string

	RestBaseURL string
	WSURL       string
	RPC         string
	Validator   string
	ChainID     int64

	// Exchange 는 EIP-712 도메인 verifyingContract 의 **수동 지정**이다.
	//
	// **비어 있는 것이 정상이고 기본값이다.** 비어 있으면 회차마다 마켓
	// 응답의 isNegRisk/isYieldBearing 으로 고른다(order.ExchangeFor).
	// 예전에는 여기 CTF_EXCHANGE 주소가 기본값으로 박혀 있었는데, 그 상태는
	// 거래소가 상품을 negRisk 로 옮기는 날 조용히 틀린 계약에 서명한다 —
	// 상수가 아니라 회차 데이터가 정해야 하는 값이다.
	Exchange string

	ModelPath  string
	LedgerPath string
	LockPath   string

	Symbol string

	// StaleAfter 는 오더북 신선도 문턱이다(스펙 기본 3초).
	//
	// 거래를 막지 않는다 — 가격이 상수라 호가창은 어떤 결정에도 들어가지
	// 않는다. 주문 로그에 "이 시장 기록은 낡았다"를 붙일지만 정한다
	// (exec.Runner.StaleAfter).
	StaleAfter time.Duration
	// Poll 은 exec 루프 주기다.
	Poll time.Duration
	// FillsPoll 은 체결 조회가 REST 를 다시 치기까지의 최소 간격이다.
	// 이 값이 곧 **노출 갱신 지연**이다 — 근거는 DefaultFillsPollInterval.
	FillsPoll time.Duration
	// RoundPoll 은 회차 목록 재조회 주기다.
	RoundPoll time.Duration
	// MaxJoinLate 는 "이미 시작한 회차에 늦게 합류해도 되는" 상한이다.
	MaxJoinLate time.Duration

	// Minutes 는 실행 시간 상한이다. 0 이면 무제한.
	Minutes float64

	// IncludePositions 는 미정산 포지션 취득원가를 equity 에 더할지다.
	IncludePositions bool

	// DryRunEquityUSDT 는 **DRY-RUN 전용** 가짜 가용 잔고다.
	//
	// 계정에 자금이 없으면 cap 이 0 이라 호가·사이징·서명 경로가 한 번도
	// 실행되지 않는다 — 그러면 DRY-RUN 이 "회차를 잡았다"까지만 증명하고
	// 정작 증명해야 할 주문 경로는 건드리지 못한다. 이 값은 그 경로를 돌리기
	// 위한 것이고, **무장 상태에서 0 이 아니면 기동이 실패한다**(checkConfig).
	// 가짜 자본으로 진짜 주문을 내는 것은 이 봇이 할 수 있는 가장 나쁜 일이다.
	DryRunEquityUSDT float64
}

// Flags 는 플래그를 fs 에 등록하고 채워질 Config 를 돌려준다. 테스트가 같은
// 파싱을 쓸 수 있도록 flag.CommandLine 을 직접 건드리지 않는다.
func Flags(fs *flag.FlagSet) *Config {
	c := &Config{}
	fs.StringVar(&c.RestBaseURL, "rest-url", defaultRestURL, "predict.fun REST 베이스 URL")
	fs.StringVar(&c.WSURL, "ws-url", defaultWSURL, "predict.fun WebSocket URL")
	fs.StringVar(&c.RPC, "rpc", kernel.DefaultRPC, "BSC JSON-RPC 엔드포인트")
	fs.StringVar(&c.Validator, "validator", kernel.ECDSAValidator, "ECDSA_VALIDATOR 컨트랙트 주소")
	fs.Int64Var(&c.ChainID, "chain-id", defaultChainID, "EIP-712 도메인 chainId")
	fs.StringVar(&c.Exchange, "exchange", "",
		"EIP-712 도메인 verifyingContract 수동 지정. 비우면 회차마다 마켓의 isNegRisk/isYieldBearing 으로 고른다(권장)")
	fs.StringVar(&c.ModelPath, "model", "models.json", "모델 파일")
	fs.StringVar(&c.LedgerPath, "ledger", "out/ledger.csv", "CSV 원장 경로")
	fs.StringVar(&c.LockPath, "lock", "", "인스턴스 잠금 파일 (비면 <ledger>.lock)")
	fs.StringVar(&c.Symbol, "symbol", live.SlugSymbol, "거래 심볼 접두사")
	fs.DurationVar(&c.StaleAfter, "stale-after", 3*time.Second,
		"오더북 신선도 문턱. 거래를 막지 않고 주문 로그의 시장 기록에 '낡음' 표시만 붙인다")
	fs.DurationVar(&c.Poll, "poll", exec.DefaultPoll, "집행 루프 주기")
	fs.DurationVar(&c.FillsPoll, "fills-poll", DefaultFillsPollInterval,
		"체결 조회(REST) 최소 간격. 이 값이 곧 노출 갱신 지연이다")
	fs.DurationVar(&c.RoundPoll, "round-poll", 20*time.Second, "회차 목록 재조회 주기")
	fs.DurationVar(&c.MaxJoinLate, "max-join-late", 120*time.Second, "이미 시작한 회차에 합류할 수 있는 최대 경과")
	fs.Float64Var(&c.Minutes, "minutes", 0, "실행 시간 상한(분). 0 이면 무제한")
	fs.BoolVar(&c.IncludePositions, "include-positions", false,
		"미정산 포지션 취득원가를 equity 에 더한다 (기본 false = 보수적)")
	fs.Float64Var(&c.DryRunEquityUSDT, "dry-run-equity", 0,
		"DRY-RUN 전용 가짜 가용 잔고(USDT). 무장 상태에서 0 이 아니면 기동 실패")
	return c
}

// checkConfig 는 값이 서로 모순되지 않는지 본다. **주문이 하나라도 나가기
// 전에** 전부 본다 — 살아 있는 주문을 든 채로 설정 오류를 발견하면 늦다.
func checkConfig(c *Config) error {
	armed := Armed(c.Arm)

	if c.DryRunEquityUSDT < 0 {
		return fmt.Errorf("-dry-run-equity 가 음수다 (%v)", c.DryRunEquityUSDT)
	}
	// **가짜 자본으로 진짜 주문을 내는 것은 이 봇이 할 수 있는 가장 나쁜
	// 일이다.** 조용히 무시하지 않고 기동을 실패시킨다 — 무시하면 운영자는
	// 자기가 준 값이 반영됐다고 믿는다.
	if armed && c.DryRunEquityUSDT != 0 {
		return fmt.Errorf("무장 상태에서 -dry-run-equity=%v 가 켜져 있다 — 가짜 자본으로 진짜 주문을 낼 수 없다", c.DryRunEquityUSDT)
	}
	if c.StaleAfter <= 0 {
		return fmt.Errorf("-stale-after 가 %s 다 — 제로값은 '문턱 없음'이 아니라 '설정 안 됨'이다", c.StaleAfter)
	}
	if c.Poll <= 0 {
		return fmt.Errorf("-poll 이 %s 다", c.Poll)
	}
	if c.RoundPoll <= 0 {
		return fmt.Errorf("-round-poll 이 %s 다", c.RoundPoll)
	}
	if c.MaxJoinLate < 0 {
		return fmt.Errorf("-max-join-late 가 음수다 (%s)", c.MaxJoinLate)
	}
	if c.Minutes < 0 {
		return fmt.Errorf("-minutes 가 음수다 (%v)", c.Minutes)
	}
	// 이 봇은 BTC 5분 회차만 거래한다 — 모델이 그것으로 학습됐다. live.FetchLive
	// 도 막지만 그쪽 에러는 폴링 주기마다 반복될 뿐이라 기동에서 잡는다.
	if c.Symbol != live.SlugSymbol {
		return fmt.Errorf("-symbol 이 %q 다 — 이 봇은 %q 5분 회차만 거래한다(모델이 그것으로 학습됐다)", c.Symbol, live.SlugSymbol)
	}
	// -exchange 는 비어 있는 것이 정상이다(회차마다 고른다). 값이 있으면
	// 주소여야 하고, 그때는 회차의 변종 판정을 통째로 무시하므로 기동 로그가
	// 그 사실을 시끄럽게 알린다(run 참고).
	if c.Exchange != "" {
		if _, err := kernel.NormalizeAddress(c.Exchange); err != nil {
			return fmt.Errorf("-exchange: %w", err)
		}
	}
	// **체결 조회 주기는 노출 갱신 지연이다.** 크게 잡을수록 "체결을 세지
	// 않는 것"에 가까워지므로 상한을 둔다. 0 이하는 매 바퀴 REST 를 치라는
	// 뜻이고, 그건 회차당 6,000 요청이다.
	if c.FillsPoll <= 0 || c.FillsPoll > MaxFillsPollInterval {
		return fmt.Errorf("-fills-poll 이 %s 다 (0 초과 %s 이하여야 한다 — 이 값이 곧 노출 갱신 지연이다)",
			c.FillsPoll, MaxFillsPollInterval)
	}
	// EIP-712 도메인 chainId 이면서 Exchange 주소 표의 열이다. 모르는 체인을
	// 통과시키면 회차마다 도메인을 고르는 경로가 매번 실패한다 — 기동에서 잡는다.
	if c.Exchange == "" {
		if _, err := order.ExchangeFor(c.ChainID, false, false); err != nil {
			return fmt.Errorf("-chain-id: %w (또는 -exchange 로 직접 지정하라)", err)
		}
	}
	if _, err := kernel.NormalizeAddress(c.Validator); err != nil {
		return fmt.Errorf("-validator: %w", err)
	}
	if c.ChainID <= 0 {
		return fmt.Errorf("-chain-id 가 %d 다", c.ChainID)
	}
	if c.LedgerPath == "" {
		return fmt.Errorf("-ledger 가 비었다 — 원장 없이 돌면 체결이 어디에도 남지 않는다")
	}
	return nil
}

// lockPath 는 인스턴스 잠금 파일 경로다. 원장과 짝이어야 한다 — 잠그는
// 대상이 "이 프로세스" 가 아니라 "이 원장" 이기 때문이다. 원장이 다르면 다른
// 봇이고, 같으면 같은 봇이다.
func (c *Config) lockPath() string {
	if c.LockPath != "" {
		return c.LockPath
	}
	return c.LedgerPath + ".lock"
}
