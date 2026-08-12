package main

// 이 파일은 모니터의 설정이다.
//
// **개인키가 여기 없는 것이 설계다.** predict.fun 에는 읽기 전용 키가 없어서
// 잔고·미체결 조회나 취소를 하려면 거래용 EOA 개인키 사본을 이 호스트에 둬야
// 한다. 바이낸스처럼 인출 끄기·IP 화이트리스트로 권한을 깎을 수단도 없다.
// server.go 의 소스 스캔 테스트가 그 환경변수 이름조차 못 쓰게 막는다.

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 환경변수 이름. 리터럴을 여기저기 적으면 오타 하나가 "설정 안 됨"으로 보인다.
const (
	EnvBeatSecret = "MONITOR_BEAT_SECRET"
	EnvListen     = "MONITOR_LISTEN"
	EnvTGToken    = "TELEGRAM_BOT_TOKEN"
	EnvTGChat     = "TELEGRAM_CHAT_ID"
	EnvMonAPIKey  = "MONITOR_API_KEY"
	// EnvBotAPIKey 는 봇의 키다. 전용 키(EnvMonAPIKey)가 없으면 이것으로
	// 떨어지고, 그때 예산을 나눠 쓰게 되므로 조회가 보수적으로 바뀐다.
	EnvBotAPIKey = "PREDICT_API_KEY"
	// EnvStatePath 는 참여 이력을 남길 파일이다. **비면 저장하지 않는다** —
	// 그때는 예전처럼 메모리에만 두고, 재기동하면 누적이 0 부터 다시 센다.
	EnvStatePath = "MONITOR_STATE_FILE"
)

// defaultListen 은 기본 수신 주소다.
const defaultListen = ":8443"

// defaultReportInterval 은 정기 리포트 주기다.
//
// 4시간이면 48회차다. 회차가 5분이라 1시간(12회차)마다 보내면 알림이 흔해져
// 사람이 보지 않게 되는데, 이 봇은 confidence 문턱 미달로 조용한 것이 정상이라
// 리포트 대부분이 "특별한 일 없음"이 된다. 대신 알람이 사실상 유일한 감지
// 경로가 되므로 rule 쪽 규칙이 촘촘해야 한다.
const defaultReportInterval = 4 * time.Hour

type monitorConfig struct {
	BeatSecret []byte
	Listen     string
	TGToken    string
	TGChat     int64
	APIKey     string
	// SharedKey 는 봇과 같은 API 키를 쓴다는 뜻이다. 정산 조회가 봇의 예산을
	// 나눠 쓰므로 보수적으로 움직여야 한다(settle.go).
	SharedKey      bool
	ReportInterval time.Duration
	// StatePath 는 참여 이력 파일이다. 비면 저장하지 않는다.
	StatePath string
}

// LoadConfig 는 환경변수에서 설정을 읽는다. 테스트는 맵 조회를 넘긴다.
func loadMonitorConfig(getenv func(string) string) (monitorConfig, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }

	c := monitorConfig{
		BeatSecret:     []byte(get(EnvBeatSecret)),
		Listen:         get(EnvListen),
		StatePath:      get(EnvStatePath),
		TGToken:        get(EnvTGToken),
		APIKey:         get(EnvMonAPIKey),
		ReportInterval: defaultReportInterval,
	}

	// 없는 것을 **한꺼번에** 보고한다. 하나씩 알려주면 세 번 실행해야 한다.
	//
	// EnvMonAPIKey 는 여기 없다 — 정산 관측을 켤 때만 필요하다. 안 쓰는 키를
	// 기동 조건으로 걸면 사용자가 검사만 통과하려고 **봇 키를 붙여넣는다**.
	// 그것이 정확히 아래 검사가 막으려는 실패다.
	var missing []string
	for _, k := range []string{EnvBeatSecret, EnvTGToken, EnvTGChat} {
		if get(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return monitorConfig{}, fmt.Errorf("환경변수가 없다: %s", strings.Join(missing, ", "))
	}

	if c.Listen == "" {
		c.Listen = defaultListen
	}
	chat, err := strconv.ParseInt(get(EnvTGChat), 10, 64)
	if err != nil {
		return monitorConfig{}, fmt.Errorf("%s 가 정수가 아니다: %w", EnvTGChat, err)
	}
	c.TGChat = chat

	// **레이트리밋 240 req/min 은 API 키 단위다.** 스펙 §"예산은 키 단위다" 가
	// 실측 사고를 기록해 두었다 — 같은 키를 쓴 수집기 3개가 창이 열린 뒤 14초
	// 만에 240 을 전부 소진시켰고 나머지 45초는 전부 429 였다. 봇 몫이 사실상
	// 0 이라 G2 게이트조차 못 돌렸다.
	//
	// 전용 키가 없으면 봇 키로 떨어진다(사용자 지시). 그 사고의 원인은
	// **무제한 페이지네이션**이었고 정산 조회는 5분에 한 번 수준이라 규모가
	// 다르지만, 같은 예산을 나눠 쓰는 것은 사실이다. 그래서 키를 공유할 때는
	// SharedKey 를 세워 두고 settle.go 가 보수적으로 조회한다 —
	// 페이지 상한을 걸고, 봇이 보고한 남은 예산이 낮으면 그 주기를 건너뛴다.
	if c.APIKey == "" {
		c.APIKey = get(EnvBotAPIKey)
		c.SharedKey = c.APIKey != ""
	} else if c.APIKey == get(EnvBotAPIKey) {
		c.SharedKey = true
	}
	return c, nil
}
