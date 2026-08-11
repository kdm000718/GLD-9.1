package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kdm000718/GLD-9.1/internal/predictfun/rest"
)

// 이 파일이 지키는 것: **세션을 어느 주소로 맺는가.**
//
// 거래소는 주문의 signer 가 인증된 주소와 같기를 요구하고, 동시에
// maker == signer 를 요구한다. 담보는 스마트계정에 있으므로 셋 다 그
// 주소여야 한다. 세션이 EOA 로 맺어지면 주문이 전부 401 로 거부된다
// (2026-08-11 실측: 무장 시도 240건이 전부 그것이었다).

const smartAccount = "0xAbCdEf0123456789aBcDeF0123456789AbCdEf01"

func authServer(t *testing.T) (*rest.Client, *map[string]any) {
	t.Helper()
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/message") {
			w.Write([]byte(`{"success":true,"data":{"message":"sign me. Timestamp: 1"}}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Write([]byte(`{"success":true,"data":{"token":"tok"}}`))
	}))
	t.Cleanup(srv.Close)
	c := rest.New("k")
	c.BaseURL = srv.URL
	return c, &body
}

func testSigner(t *testing.T) *Signer {
	t.Helper()
	// go-ethereum 문서의 **공개** 예시 키다. 실지갑이 아니다.
	s, err := NewSigner("4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuthUsesEOAWhenNoAccountConfigured(t *testing.T) {
	rc, body := authServer(t)
	s := testSigner(t)
	a := &Authenticator{Rest: rc, Signer: s}
	if _, err := a.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got := (*body)["signer"]; !strings.EqualFold(got.(string), s.Address().Hex()) {
		t.Fatalf("signer = %v, want EOA %s", got, s.Address().Hex())
	}
}

// TestAuthUsesAccountAndEnvelopeWhenConfigured 는 이 수정의 핵심이다.
func TestAuthUsesAccountAndEnvelopeWhenConfigured(t *testing.T) {
	rc, body := authServer(t)
	s := testSigner(t)
	called := false
	a := &Authenticator{
		Rest: rc, Signer: s, Account: smartAccount,
		SignMessage: func(textHash []byte) ([]byte, error) {
			called = true
			if len(textHash) != 32 {
				t.Errorf("textHash %d바이트, keccak 32 여야 한다", len(textHash))
			}
			return []byte{0x01, 0xAA, 0xBB}, nil // 봉투 흉내
		},
	}
	if _, err := a.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if !called {
		t.Fatal("SignMessage 를 부르지 않았다 — 평문 EOA 서명이 나갔다면 401 invalid signature 다")
	}
	if got := (*body)["signer"]; !strings.EqualFold(got.(string), smartAccount) {
		t.Fatalf("signer = %v, want 스마트계정 %s — 세션이 EOA 로 맺어지면 주문이 전부 401 이다",
			got, smartAccount)
	}
	if got := (*body)["signature"]; got != "0x01aabb" {
		t.Fatalf("signature = %v, want SignMessage 의 결과", got)
	}
}

// TestAuthRefusesHalfConfiguration 은 절반만 채운 배선을 막는다. 계정만 있고
// 서명 방법이 없으면 평문 서명이 나가 401 이 되는데, 그 401 은 "키가 틀렸다"
// 처럼 보여서 엉뚱한 곳을 찾게 만든다.
func TestAuthRefusesHalfConfiguration(t *testing.T) {
	rc, _ := authServer(t)
	s := testSigner(t)
	for _, tc := range []struct {
		name string
		a    *Authenticator
	}{
		{"계정만", &Authenticator{Rest: rc, Signer: s, Account: smartAccount}},
		{"서명함수만", &Authenticator{Rest: rc, Signer: s,
			SignMessage: func([]byte) ([]byte, error) { return []byte{1}, nil }}},
	} {
		if _, err := tc.a.Token(context.Background()); err == nil {
			t.Errorf("%s: 에러여야 한다", tc.name)
		}
	}
}
