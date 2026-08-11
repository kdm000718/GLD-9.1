package order

import "fmt"

// 이 파일은 **어느 계약에 서명하는가**를 정한다.
//
// predict.fun 은 Exchange 를 네 개 운영한다. 어느 것인지는 마켓이 정하고,
// 그 값이 EIP-712 도메인의 verifyingContract 가 된다. 틀리면 다이제스트가
// 통째로 달라지므로 거래소가 서명을 거부한다 — 그건 그나마 나은 경우다.
// 나쁜 경우는 그 주소가 **실재하는 다른 Exchange** 라는 것이다. 그러면 우리는
// 의도하지 않은 계약에 유효한 서명을 실어 보낸다.
//
// # 왜 상수로 박지 않는가
//
// 2026-08-10 메인넷 실측에서 진행 중인 btc-updown-5m 마켓
// (`btc-updown-5m-1786377600`, marketId 1266089)은 isNegRisk=false,
// isYieldBearing=false 였다 — 즉 CTF_EXCHANGE 다. 그러나 그 사실을 상수로
// 박으면, 거래소가 어느 날 이 상품을 negRisk 로 옮기는 순간 코드는 **조용히**
// 틀린 계약에 서명한다. 에러도 로그도 없다. 그래서 회차마다 마켓 응답의 두
// 불린을 읽어 여기서 고른다.
//
// 매핑은 SDK `@predictdotfun/sdk@1.3.8` 의
// `OrderBuilder.getExchangeIdentifier(isNegRisk, isYieldBearing)`
// (dist/OrderBuilder.js:166-173) 와 **같은 분기**다. 주소는 설계서 §6 계약
// 표와 SDK `dist/Constants.js` 가 일치하는 값이다.

// DomainName·DomainVersion 은 EIP-712 도메인의 앞 두 필드다.
//
// **네 변종이 이름을 공유한다.** SDK 는 이것을 단일 상수 PROTOCOL_NAME
// (dist/Constants.js:66)으로 두고 verifyingContract 만 바꾼다 — 변종별로
// 이름이 갈린다고 착각해 name 을 손대면 네 경우 모두 서명이 깨진다.
const (
	DomainName    = "predict.fun CTF Exchange"
	DomainVersion = "1"
)

// 지원하는 체인. EIP-712 도메인 chainId 이면서 계약 표의 열이기도 하다.
const (
	ChainIDMainnet int64 = 56
	ChainIDTestnet int64 = 97
)

// exchangeKey 는 마켓이 말하는 두 불린이다. 불린 두 개를 그대로 맵 키로
// 쓰는 이유: 네 조합이 전부 표에 있어야 하고, 하나가 빠지면 컴파일이 아니라
// 조회 실패로 드러나야 하기 때문이다(조용한 제로값이 되지 않는다).
type exchangeKey struct{ negRisk, yieldBearing bool }

// exchangeAddrs 는 체인별 네 변종의 Exchange 주소다.
//
// 주소는 전부 **공개 계약 주소**다(설계서 §6 표, SDK Constants). 지갑 주소가
// 아니다.
var exchangeAddrs = map[int64]map[exchangeKey]string{
	ChainIDMainnet: {
		{negRisk: false, yieldBearing: false}: "0x8BC070BEdAB741406F4B1Eb65A72bee27894B689", // CTF_EXCHANGE
		{negRisk: true, yieldBearing: false}:  "0x365fb81bd4A24D6303cd2F19c349dE6894D8d58A", // NEG_RISK_CTF_EXCHANGE
		{negRisk: false, yieldBearing: true}:  "0x6bEb5a40C032AFc305961162d8204CDA16DECFa5", // YIELD_BEARING_CTF_EXCHANGE
		{negRisk: true, yieldBearing: true}:   "0x8A289d458f5a134bA40015085A8F50Ffb681B41d", // YIELD_BEARING_NEG_RISK_CTF_EXCHANGE
	},
	ChainIDTestnet: {
		{negRisk: false, yieldBearing: false}: "0x2A6413639BD3d73a20ed8C95F634Ce198ABbd2d7",
		{negRisk: true, yieldBearing: false}:  "0xd690b2bd441bE36431F6F6639D7Ad351e7B29680",
		{negRisk: false, yieldBearing: true}:  "0x8a6B4Fa700A1e310b106E7a48bAFa29111f66e89",
		{negRisk: true, yieldBearing: true}:   "0x95D5113bc50eD201e319101bbca3e0E250662fCC",
	},
}

// ExchangeName 은 변종의 이름이다. 로그와 에러에만 쓴다 — 주소는 찍지 않고
// 이름만 찍으면 "어느 계약에 서명했는가"가 로그에 남으면서도 표를 그대로
// 흘리지 않는다.
func ExchangeName(negRisk, yieldBearing bool) string {
	switch {
	case negRisk && yieldBearing:
		return "YIELD_BEARING_NEG_RISK_CTF_EXCHANGE"
	case negRisk:
		return "NEG_RISK_CTF_EXCHANGE"
	case yieldBearing:
		return "YIELD_BEARING_CTF_EXCHANGE"
	}
	return "CTF_EXCHANGE"
}

// ExchangeFor 는 마켓의 두 불린으로 verifyingContract 를 고른다.
//
// 모르는 체인이면 **에러다.** 기본값으로 메인넷을 돌려주면 테스트넷 설정으로
// 메인넷 계약에 서명하게 되는데, 그 서명은 테스트넷에서 거부되는 것이 아니라
// 메인넷에서 유효하다.
func ExchangeFor(chainID int64, negRisk, yieldBearing bool) (string, error) {
	byVariant, ok := exchangeAddrs[chainID]
	if !ok {
		return "", fmt.Errorf("order: chainId %d 의 Exchange 주소를 모른다 (아는 체인: %d, %d)",
			chainID, ChainIDMainnet, ChainIDTestnet)
	}
	addr, ok := byVariant[exchangeKey{negRisk: negRisk, yieldBearing: yieldBearing}]
	if !ok {
		// 네 조합이 전부 표에 있으므로 도달할 수 없다. 그래도 남긴다 —
		// 표에서 한 줄이 지워지면 조용한 빈 문자열이 아니라 여기서 걸린다.
		return "", fmt.Errorf("order: chainId %d 에 %s 주소가 표에 없다",
			chainID, ExchangeName(negRisk, yieldBearing))
	}
	return addr, nil
}

// DomainFor 는 회차 하나의 EIP-712 도메인이다.
func DomainFor(chainID int64, negRisk, yieldBearing bool) (Domain, error) {
	addr, err := ExchangeFor(chainID, negRisk, yieldBearing)
	if err != nil {
		return Domain{}, err
	}
	return Domain{
		Name:              DomainName,
		Version:           DomainVersion,
		ChainID:           chainID,
		VerifyingContract: addr,
	}, nil
}
