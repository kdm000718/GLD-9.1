package order

import (
	"strings"
	"testing"
)

// 네 조합이 **서로 다른 주소**여야 한다. 두 칸이 같으면 그 둘을 구분한
// 이유가 사라지고, 복붙 실수가 정확히 그 모습이다.
func TestFourVariantsAreFourDistinctAddresses(t *testing.T) {
	for _, chainID := range []int64{ChainIDMainnet, ChainIDTestnet} {
		seen := map[string]string{}
		for _, neg := range []bool{false, true} {
			for _, yb := range []bool{false, true} {
				addr, err := ExchangeFor(chainID, neg, yb)
				if err != nil {
					t.Fatalf("chainId %d, negRisk=%v, yieldBearing=%v: %v", chainID, neg, yb, err)
				}
				name := ExchangeName(neg, yb)
				if prev, dup := seen[strings.ToLower(addr)]; dup {
					t.Errorf("chainId %d: %s 와 %s 의 주소가 같다", chainID, prev, name)
				}
				seen[strings.ToLower(addr)] = name
			}
		}
		if len(seen) != 4 {
			t.Errorf("chainId %d: 서로 다른 주소가 %d개다 (기대 4개)", chainID, len(seen))
		}
	}
}

// 메인넷과 테스트넷이 겹치면 안 된다. 겹치면 chainId 를 잘못 줘도 조용히
// 같은 계약에 서명하게 되고, 그 실수가 드러나지 않는다.
func TestMainnetAndTestnetDoNotShareAddresses(t *testing.T) {
	for _, neg := range []bool{false, true} {
		for _, yb := range []bool{false, true} {
			m, err := ExchangeFor(ChainIDMainnet, neg, yb)
			if err != nil {
				t.Fatal(err)
			}
			t2, err := ExchangeFor(ChainIDTestnet, neg, yb)
			if err != nil {
				t.Fatal(err)
			}
			if strings.EqualFold(m, t2) {
				t.Errorf("%s: 메인넷과 테스트넷 주소가 같다", ExchangeName(neg, yb))
			}
		}
	}
}

// **모르는 체인은 에러다.** 기본값으로 메인넷을 돌려주면 테스트넷 설정으로
// 메인넷 계약에 유효한 서명을 만든다.
func TestUnknownChainIsAnError(t *testing.T) {
	for _, id := range []int64{0, -1, 1, 137, 8453} {
		if addr, err := ExchangeFor(id, false, false); err == nil {
			t.Errorf("chainId %d 에 주소를 돌려줬다: %s", id, addr)
		}
	}
	if _, err := DomainFor(1, false, false); err == nil {
		t.Error("DomainFor 가 모르는 체인을 통과시켰다")
	}
}

// 실측 고정: btc-updown-5m 은 isNegRisk=false / isYieldBearing=false 다
// (2026-08-10, marketId 1266089). 그 조합이 CTF_EXCHANGE 여야 한다.
func TestPlainMarketPicksCtfExchange(t *testing.T) {
	addr, err := ExchangeFor(ChainIDMainnet, false, false)
	if err != nil {
		t.Fatal(err)
	}
	const wantCTF = "0x8BC070BEdAB741406F4B1Eb65A72bee27894B689"
	if !strings.EqualFold(addr, wantCTF) {
		t.Errorf("CTF_EXCHANGE = %s, 기대 %s", addr, wantCTF)
	}
	if n := ExchangeName(false, false); n != "CTF_EXCHANGE" {
		t.Errorf("이름 = %s", n)
	}
}

// **네 변종이 도메인 이름을 공유한다.** SDK 는 PROTOCOL_NAME 하나만 쓰고
// verifyingContract 만 바꾼다 — 이름을 변종별로 가르면 네 경우 모두 서명이
// 깨진다.
func TestDomainNameIsSharedAcrossVariants(t *testing.T) {
	for _, neg := range []bool{false, true} {
		for _, yb := range []bool{false, true} {
			d, err := DomainFor(ChainIDMainnet, neg, yb)
			if err != nil {
				t.Fatal(err)
			}
			if d.Name != DomainName || d.Version != DomainVersion {
				t.Errorf("%s: 도메인 이름/버전이 다르다 (%q/%q)", ExchangeName(neg, yb), d.Name, d.Version)
			}
			if d.ChainID != ChainIDMainnet {
				t.Errorf("%s: chainId 가 %d 다", ExchangeName(neg, yb), d.ChainID)
			}
		}
	}
}

// 변종이 다르면 **다이제스트가 달라야 한다.** 이 테스트가 실패한다는 것은
// verifyingContract 가 서명에 들어가지 않았다는 뜻이고, 그러면 이 파일 전체가
// 아무것도 지키지 못한다.
func TestVariantChangesTheDigest(t *testing.T) {
	o := fixtureOrder()
	seen := map[string]bool{}
	for _, neg := range []bool{false, true} {
		for _, yb := range []bool{false, true} {
			d, err := DomainFor(ChainIDMainnet, neg, yb)
			if err != nil {
				t.Fatal(err)
			}
			h, err := Hash(o, d)
			if err != nil {
				t.Fatal(err)
			}
			key := string(h)
			if seen[key] {
				t.Errorf("%s 의 다이제스트가 다른 변종과 같다 — verifyingContract 가 서명에 들어가지 않는다",
					ExchangeName(neg, yb))
			}
			seen[key] = true
		}
	}
}
