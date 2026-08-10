# P4 — 인증·서명·오더북 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** predict.fun 에 주문을 낼 수 있는 상태까지 간다 — 오더북을 실시간으로 읽고,
EOA 로 인증하고, EIP-712 주문에 서명한다. 게이트는 **G3(TS SDK 와 서명 해시 일치)** 다.

**Architecture:** P0~P3 이 만든 모델·피처 파이프라인 위에 실행 경로를 얹는다. 먼저
서빙 경로(디스크에서 모델을 읽어 같은 확률을 내는가)를 닫고, 그다음 오더북 WS 를
기존 수집기에서 이식하고, 마지막에 인증과 주문 서명을 새로 만든다. **자금이 필요한
단계는 전부 뒤로 미룬다** — G3 는 오프라인이라 지갑에 잔고가 없어도 통과할 수 있다.

**Tech Stack:** Go 1.22 (로컬 툴체인 고정) · `github.com/ethereum/go-ethereum v1.15.0`
(EIP-712) · `github.com/coder/websocket v1.8.12` (WS) · Node 25 + `@predictdotfun/sdk@1.3.8`
(G3 골든 벡터 생성 전용)

## Global Constraints

- Go 모듈 경로는 `github.com/kdm000718/GLD-9.1`. Go 1.22.2 로컬 툴체인으로 빌드되어야 하며 `GOTOOLCHAIN=local` 을 쓴다.
- **`go-ethereum` 은 `v1.15.0` 에 고정한다.** 최신 `v1.17.5` 는 `go >= 1.24.0` 을 요구해 빌드가 깨진다. `go get -u` 나 무인 `go mod tidy` 로 올리지 말 것. 팀리드가 v1.15.0 에서 EIP-712 해싱·서명이 Go 1.22.2 로 동작함을 실측 확인했다.
- 주석·로그·에러 메시지는 한국어로 쓴다.
- 시크릿은 환경변수로만 읽는다: `PREDICT_API_KEY`, `WALLET_PRIVATE_KEY`. 소스·로그·에러 메시지 어디에도 값을 남기지 않는다. **개인키는 어떤 경로로도 출력하지 않는다** — 주소만 찍는다.
- **`PREDICT_ACCOUNT`(사용자의 predictAccount 주소)도 환경변수로만 읽는다.** 비밀은 아니지만 이 저장소는 GitHub 에 올라가고, 주소는 그 사람의 거래 계정과 잔고·체결 내역 전체를 공개 체인에서 조회할 수 있게 하는 식별자다. 소스·테스트·골든 파일에 **실주소를 박지 마라.** 테스트에는 `0x1111…`/`0x2222…` 같은 자리표시자를 쓴다(Task 9 의 골든 벡터가 이미 그렇게 되어 있다).
- 실행 시 시크릿은 `~/.config/predictfun/env`(mode 600)에서 읽는다: `set -a; . ~/.config/predictfun/env; set +a`. 이 파일은 저장소 밖이다.
- `LIVE_ARM='I_UNDERSTAND_THE_RISK'` 가 없으면 서명은 하되 전송하지 않는 DRY-RUN 으로 돈다.
- 호가창 가격과 주문 가격은 float 로 비교하지 않는다. 마켓의 `decimalPrecision` 으로 정규화한 정수 틱으로 다룬다. 틱을 전역 상수로 두지 않는다 — 마켓마다 다르고 실측상 2 와 3 이 공존한다.
- **BSC USDT 는 18 decimals** 다(이더리움의 6 이 아니다). `makerAmount` 자릿수를 틀리면 10¹² 배 주문이 나간다.
- predict.fun REST 는 `x-api-key` 헤더와 명시적 `User-Agent` 가 둘 다 필요하다. 레이트리밋은 API 키당 240 req/min 이다.
- 참조 구현: `~/kdm/prediction_market/binance_prediction_data` (WS·오더북, 동작 검증됨). 동작이 애매하면 추측하지 말고 그 코드를 읽는다.

## 선행 조건과 현재 상태

| 항목 | 상태 | 영향 |
|---|---|---|
| EOA 개인키 | **보유** | Task 7~9 에 필요 |
| BSC 가스(BNB) | 미확인 | Task 10 의 온체인 승인에만 필요 |
| predict.fun USDT 잔고 | 미확인 | Task 10 의 실주문에만 필요. equity > $22 여야 무장 |
| 수집기 중지 여부 | 미확인 | Task 10 의 testnet 왕복에 240 req/min 필요 |

**Task 1~9 는 자금 없이 끝난다.** G3 까지가 그 범위다. Task 10 만 지갑에 가스와
잔고가 필요하므로, 준비가 안 되면 Task 9 에서 멈추고 보고한다.

## 실물로 확인해야 하는 것 (Task 10 산출물)

**2026-08-09 에 SDK `@predictdotfun/sdk@1.3.8` 을 직접 뜯어 상당수가 확정됐다.**
남은 것만 Task 10 이 실측한다.

1. ~~Up/Down 마켓이 네 변종 중 무엇인가~~ — **답 나왔다. `CTF_EXCHANGE` 다.**
   2026-08-09 팀리드가 읽기 전용 조회로 확인했다. `/v1/categories?marketVariant=CRYPTO_UP_DOWN&status=OPEN`
   의 `btc-updown-5m-*` 회차에서 **testnet·mainnet 양쪽 모두**
   `isNegRisk: false`, `isYieldBearing: false` 다. 따라서 `NEG_RISK` 도
   `YIELD_BEARING` 도 아니다.

   같은 조회로 함께 확정된 것(양쪽 동일):

   | 필드 | 값 |
   |---|---|
   | `decimalPrecision` | **2** (1틱 = 0.01, 0.5 미만 상한 = 49틱) |
   | `feeRateBps` | **200** (2%) |
   | `shareThreshold` | 100 |
   | `spreadThreshold` | 0.06 |
   | `resolutionProvider` | `CHAINLINK` |
   | `outcomes` | `Up`(indexSet 1) / `Down`(indexSet 2), 각각 `onChainId` = CTF 토큰 ID |

   `outcomes[].onChainId` 가 `Order.TokenID` 에 들어갈 값이다.

   **주의:** `decimalPrecision` 이 2 로 관측됐다고 전역 상수로 두지 마라. 전역
   제약이 금지하고 있고, 실측상 2 와 3 이 공존한다. 회차마다 메타데이터에서 읽는다.

2. **`shareThreshold` 의 의미** — 아직 미확정이나 **강한 가설이 생겼다.**
   `shareThreshold: 100` 이 `spreadThreshold: 0.06` 및 `rewards` 구조와 나란히
   있다. 이것은 메이커 보상 **자격 조건**의 전형적인 모양이다 — 중간가에서 일정
   폭 안에, 최소 수량 이상으로 걸어야 보상을 받는 구조(다른 예측시장도 같은 설계다).

   **사실이면 봇 경제성에 직접 영향이 간다.** 가격 0.49 에서 100주는 명목 약 $49
   이고, 회차당 최대 명목이 equity 의 4.55% 미만이므로 **equity 가 약 $1,077
   이상이어야 자격을 채울 수 있다.** 그 아래에서는 리베이트 +0.487%p 가 0 이 되고
   엣지는 +1.974%p 로 떨어진다(여전히 양수다 — 치명적이지 않다).

   Task 10 이 확인할 것: 100주 미만 주문에도 리베이트가 붙는가, 아니면
   `shareThreshold` 가 자격선인가. `spreadThreshold: 0.06` 이 중간가 기준인지
   최우선호가 기준인지도 함께.

   **`rewards` 는 별개일 수 있다.** mainnet 은 `rewards.schedule` 에
   `{startsAt, endsAt, hourlyRate: 3000}` 이 회차 5분 창에 맞춰 채워져 있고
   testnet 은 비어 있다. 이것이 사용자가 말한 "반대편 주식 0.5%" 와 같은 것인지
   별도의 유동성 보상 프로그램인지 **구분되지 않았다.** 둘을 섞어 세지 마라.

3. 주문 배치·수정 엔드포인트 유무
4. 인증 응답(`/v1/auth/message`, `/v1/auth/jwt`)의 실제 필드명
5. **잔고 API 가 미정산 주식을 어떻게 보여주는가** — USDT 와 별도 필드인가.

   배경: 메이커 리베이트는 USDT 가 아니라 **반대편 주식**으로 지급된다.
   2026-08-09 사용자 확인으로 규칙 셋이 확정됐다 — **주식 수 기준 0.5%**,
   **체결 즉시 지급**, **회차 정산 때 함께 정산**. 그래서 회차 중 equity 는
   "USDT + 미정산 주식"이고, **최대 4.55% 사이징의 분모**가 이 API 표현에 따라
   갈린다. 남은 미지수는 이것 하나다.

   기대값 영향은 이미 반영했다 — `0.005·(1−q)/p` 로 `q=0.5227, p=0.49` 에서
   +0.487%p(현금 가정보다 0.013 낮다. 질 때만 받기 때문이다). 자세한 것은
   스펙 §4 의 "메이커 리베이트는 USDT 가 아니라 반대편 주식이다" 절.

**SDK 로 확정된 것 — 더 이상 추측하지 않는다.** 스펙 §6 에 표로 있다.
chainId 56/97 · 양쪽 체인 전체 주소표 · USDT 18 decimals · 최소 수량 `1e16` ·
가격 유효숫자 3자리·수량 5자리 절단 · Kernel 서명 봉투 · `MAX_SALT = 2^31`.

## 자금 계정과 서명 지갑

사용자 계정은 **ZeroDev Kernel 스마트 계정**(`predictAccount` = 입금 주소)이고,
서명은 계정 설정에서 내보낸 **Privy 지갑**이 한다. 주문의 `maker` 와 `signer` 는
**둘 다 predictAccount** 이며 Privy 주소가 아니다. 자세한 것은 스펙 §6 의
"자금 계정과 서명 지갑은 다르다" 절.

이 때문에 **주문 서명이 Order 다이제스트를 그대로 쓰지 않고 Kernel 도메인으로 한 겹
감싼다.** Task 8 이 그 경로를 구현하고 Task 9 가 SDK 와 대조한다.

## 파일 구조

```
cmd/train/main.go                    Vision 로드 → 최근 180일 학습 → models.json
cmd/g3check/main.go                  G3 게이트 — TS SDK 골든 서명과 대조
cmd/probe/main.go                    testnet 왕복 + 위 실측 4건

internal/model/matrix.go             (수정) Row·SetRow 범위 검사
internal/model/serialize.go          (신규) models.json 로더 + FeatureNames 대조
internal/vision/vision.go            (수정) 캐시 원자적 쓰기

internal/predictfun/ws/conn.go       WS 연결·하트비트·재접속 후 전체 재구독
internal/predictfun/ws/protocol.go   구독/해지/하트비트 프레임
internal/predictfun/ws/book.go       오더북 상태 — 우리 주문을 제외한 best bid/ask
internal/predictfun/auth/auth.go     GET /v1/auth/message → EOA 서명 → POST /v1/auth/jwt
internal/predictfun/order/order.go   Order 값 타입, 틱 정규화, 금액 변환
internal/predictfun/order/sign.go    EIP-712 타입드데이터 · 해시 · 서명

tools/export_sdk_vectors.ts          @predictdotfun/sdk 로 G3 골든 벡터 생성
testdata/sdk_signatures.json         G3 골든 (커밋한다)
```

---

### Task 1: Matrix 범위 검사

최종 리뷰가 P4 전 처리로 지목한 항목이다. 슬라이스 재슬라이싱 상한은 `len` 이 아니라
`cap` 이므로 `Truncate(2)` 후 `Row(4)` 가 **패닉 없이 낡은 행을 돌려준다.** 지금 안전한
것은 `buildMatrix` 가 채우기·자르기·읽기를 한꺼번에 하기 때문이고, P4 의 서빙 경로는
행렬을 재사용하므로 그 우연이 깨진다.

**Files:**
- Modify: `internal/model/matrix.go`
- Test: `internal/model/matrix_test.go`

**Interfaces:**
- Consumes: 없음
- Produces: `(*Matrix).Row(i int) []float32` 가 `i` 범위 밖이면 패닉(메시지 포함), `(*Matrix).SetRow(i int, v []float64)` 도 동일

- [ ] **Step 1: 실패 테스트를 쓴다**

`internal/model/matrix_test.go` 에 이어 붙인다:

```go
func TestRowPanicsOutsideTruncatedRange(t *testing.T) {
	m := NewMatrix(5, 2)
	for i := 0; i < 5; i++ {
		m.SetRow(i, []float64{float64(i), float64(i)})
	}
	m.Truncate(2)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("Truncate(2) 뒤 Row(4) 가 패닉하지 않았다 — 낡은 행이 조용히 돌아온다")
		}
	}()
	_ = m.Row(4)
}

func TestSetRowPanicsOutsideRange(t *testing.T) {
	m := NewMatrix(2, 2)
	defer func() {
		if recover() == nil {
			t.Fatal("SetRow(5, ...) 가 패닉하지 않았다")
		}
	}()
	m.SetRow(5, []float64{1, 2})
}

func TestSetRowRejectsWrongLength(t *testing.T) {
	m := NewMatrix(2, 3)
	defer func() {
		if recover() == nil {
			t.Fatal("길이가 다른 행을 받아들였다")
		}
	}()
	m.SetRow(0, []float64{1, 2})
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./internal/model/ -run 'TestRow|TestSetRow' -v`
Expected: `TestRowPanicsOutsideTruncatedRange` 가 "패닉하지 않았다" 로 FAIL

- [ ] **Step 3: 구현**

`internal/model/matrix.go` 의 `Row`·`SetRow` 를 다음으로 바꾼다:

```go
// Row 는 i 번째 행을 돌려준다.
//
// 범위를 명시적으로 검사하는 이유: 슬라이스 재슬라이싱의 상한은 len 이 아니라 cap
// 이다. Truncate 로 Rows 를 줄여도 Data 의 cap 은 그대로이므로, 검사가 없으면
// Truncate(2) 뒤의 Row(4) 가 패닉 없이 '지워진' 행을 돌려준다. 학습·추론에서
// 그것은 조용히 틀린 결과가 된다.
func (m *Matrix) Row(i int) []float32 {
	if i < 0 || i >= m.Rows {
		panic(fmt.Sprintf("Matrix.Row: 행 %d 는 범위 밖이다 (Rows=%d)", i, m.Rows))
	}
	return m.Data[i*m.Cols : (i+1)*m.Cols]
}

func (m *Matrix) SetRow(i int, v []float64) {
	if i < 0 || i >= m.Rows {
		panic(fmt.Sprintf("Matrix.SetRow: 행 %d 는 범위 밖이다 (Rows=%d)", i, m.Rows))
	}
	if len(v) != m.Cols {
		panic(fmt.Sprintf("Matrix.SetRow: 값 %d개, 기대 %d개", len(v), m.Cols))
	}
	row := m.Data[i*m.Cols : (i+1)*m.Cols]
	for j, x := range v {
		row[j] = float32(x)
	}
}
```

`import "fmt"` 를 추가한다.

- [ ] **Step 4: 통과 확인 + 회귀 확인**

```bash
GOTOOLCHAIN=local go test ./internal/model/ -v
GOTOOLCHAIN=local go test -count=1 -race ./...
```
Expected: 전부 PASS. **`cmd/backtest` 가 여전히 도는지도 본다** — `buildMatrix` 가
`Truncate` 전에 `SetRow(kept, ...)` 를 부르므로 `kept < Rows` 여야 한다.

```bash
GOTOOLCHAIN=local go run ./cmd/backtest 2>&1 | tail -3
```
Expected: `판정: 통과`

- [ ] **Step 5: 커밋**

```bash
git add internal/model/matrix.go internal/model/matrix_test.go
git commit -m "Matrix 범위 검사 — Truncate 뒤 낡은 행이 조용히 돌아오는 것을 막는다"
```

---

### Task 2: Vision 캐시 원자적 쓰기

최종 리뷰 항목. `os.WriteFile` 은 원자적이지 않고 `decodeCache` 는 `len%88` 만 보므로,
쓰기 도중 프로세스가 죽어 길이가 88 배수로 남으면 **행 수가 모자란 캐시를 에러 없이
읽는다.** 게이트는 봉 수 단언으로 잡지만 봇의 기동 로드에는 그 단언이 없다.

**Files:**
- Modify: `internal/vision/vision.go`
- Test: `internal/vision/vision_test.go`

**Interfaces:**
- Consumes: 없음
- Produces: 캐시 쓰기가 원자적이 된다. 기존 공개 API 는 그대로.

- [ ] **Step 1: 실패 테스트를 쓴다**

`internal/vision/vision_test.go` 에 이어 붙인다:

```go
func TestDecodeCacheRejectsTruncatedFile(t *testing.T) {
	rows := [][11]float64{{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}, {12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22}}
	full := encodeCache(rows)
	// 한 행을 통째로 잘라낸다. 길이는 여전히 88 의 배수라 기존 검사를 통과한다.
	short := full[:len(full)-88]
	got, err := decodeCache(short)
	if err != nil {
		return // 길이 헤더가 있으면 여기서 걸린다 — 원하는 동작
	}
	if len(got) != len(rows) {
		t.Fatalf("잘린 캐시를 %d행으로 읽었다(원본 %d행) — 에러 없이 통과했다", len(got), len(rows))
	}
}

func TestWriteCacheAtomicLeavesNoPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monthly-2020-01.bin")
	rows := [][11]float64{{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}}
	if err := writeCacheAtomic(path, encodeCache(rows)); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "monthly-2020-01.bin" {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("임시 파일이 남았다: %v", names)
	}
}
```

`import` 에 `"os"`, `"path/filepath"` 가 없으면 추가한다.

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./internal/vision/ -run 'TestDecodeCacheRejects|TestWriteCacheAtomic' -v`
Expected: `writeCacheAtomic` 미정의로 컴파일 실패, 그리고 `TestDecodeCacheRejectsTruncatedFile` 이 "에러 없이 통과했다" 로 FAIL

- [ ] **Step 3: 구현**

`internal/vision/vision.go` 의 `encodeCache`/`decodeCache` 에 행 수 헤더를 넣고
원자적 쓰기를 추가한다.

```go
// cacheMagic 뒤에 int64 행 수가 온다. 헤더가 있어야 잘린 파일을 알아본다 —
// 88 배수 검사만으로는 행 하나가 통째로 날아간 파일을 구분하지 못한다.
const cacheMagic = "GLD9BIN1"

func encodeCache(arr [][11]float64) []byte {
	out := make([]byte, 0, len(cacheMagic)+8+len(arr)*88)
	out = append(out, cacheMagic...)
	var hdr [8]byte
	binary.LittleEndian.PutUint64(hdr[:], uint64(len(arr)))
	out = append(out, hdr[:]...)
	var buf [8]byte
	for _, r := range arr {
		for _, v := range r {
			binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
			out = append(out, buf[:]...)
		}
	}
	return out
}

func decodeCache(b []byte) ([][11]float64, error) {
	const head = len(cacheMagic) + 8
	if len(b) < head || string(b[:len(cacheMagic)]) != cacheMagic {
		return nil, fmt.Errorf("캐시 형식이 아니다 (%d바이트)", len(b))
	}
	n := int(binary.LittleEndian.Uint64(b[len(cacheMagic):head]))
	want := head + n*88
	if len(b) != want {
		return nil, fmt.Errorf("캐시가 잘렸다: %d바이트, 헤더가 말하는 %d행이면 %d바이트여야 한다",
			len(b), n, want)
	}
	out := make([][11]float64, n)
	off := head
	for i := 0; i < n; i++ {
		for j := 0; j < 11; j++ {
			out[i][j] = math.Float64frombits(binary.LittleEndian.Uint64(b[off:]))
			off += 8
		}
	}
	return out, nil
}

// writeCacheAtomic 은 같은 디렉터리에 임시 파일로 쓴 뒤 rename 한다.
// rename 은 같은 파일시스템 안에서 원자적이므로, 쓰기 도중 죽어도 목적지에는
// 온전한 파일이거나 아무것도 없거나 둘 중 하나만 남는다.
func writeCacheAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // rename 이 성공하면 대상이 없으므로 무해하다
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
```

`fetchChunk` 안의 `os.WriteFile(cache, encodeCache(arr), 0o644)` 를
`writeCacheAtomic(cache, encodeCache(arr))` 로 바꾼다.

- [ ] **Step 4: 통과 확인 — 기존 캐시가 무효화된다**

형식이 바뀌었으므로 **기존 `data/vision` 캐시 476MB 가 전부 못 읽힌다.** 이것은
의도된 것이다(옛 형식에는 헤더가 없다). 에러 메시지가 그 사실을 알려주는지 확인한다.

```bash
GOTOOLCHAIN=local go test ./internal/vision/ -v
GOTOOLCHAIN=local go run ./cmd/backtest 2>&1 | head -5
```
Expected: 테스트 PASS. `cmd/backtest` 는 "캐시 형식이 아니다" 로 실패한다.

캐시를 지우고 다시 받는다(2~3분):

```bash
rm -rf data/vision && GOTOOLCHAIN=local go run ./cmd/backtest 2>&1 | tail -3
```
Expected: 재다운로드 후 `판정: 통과`. **G1' 수치가 이전과 완전히 같아야 한다** —
정확도 0.52772 / n=888,525 / 뒤집힘 421. 다르면 캐시 인코딩이 값을 바꾼 것이다.

- [ ] **Step 5: 커밋**

```bash
git add internal/vision/vision.go internal/vision/vision_test.go docs/results/g1prime-fullhistory.log
git commit -m "Vision 캐시 원자적 쓰기와 행 수 헤더 — 잘린 캐시를 조용히 읽지 않는다"
```

---

### Task 3: 모델 직렬화 왕복과 FeatureNames 대조

최종 리뷰가 "서빙 경로에 게이트가 없다" 고 지적한 항목이다. 지금은 학습한 모델을
디스크에 쓰고 다시 읽어 같은 확률을 내는지 확인하는 코드가 없고, **로드된 모델의
`FeatureNames` 를 `features.FeatureNames` 와 대조하는 코드도 없다** — "피처 순서는
유일한 근거" 라는 불변식이 예측 시점에는 강제되지 않는다.

**Files:**
- Create: `internal/model/serialize.go`, `internal/model/serialize_test.go`

**Interfaces:**
- Consumes: `LogReg` (`internal/model/logreg.go`), `features.FeatureNames`
- Produces:
  - `model.Save(path string, m *LogReg) error`
  - `model.Load(path string, wantNames []string) (*LogReg, error)` — `wantNames` 와 어긋나면 에러
  - `(*LogReg).Validate(wantNames []string) error`

- [ ] **Step 1: 실패 테스트를 쓴다**

`internal/model/serialize_test.go`:

```go
package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleModel() *LogReg {
	return &LogReg{
		L2: 10, Coef: []float64{0.25, -1.5}, Intercept: 0.125,
		Mu: []float64{1, 2}, Sd: []float64{3, 4}, NTrain: 1234,
		FeatureNames: []string{"a", "b"},
	}
}

func TestSaveLoadRoundTripIsExact(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	orig := sampleModel()
	if err := Save(p, orig); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	x := []float32{7, 9}
	if got.Logit(x) != orig.Logit(x) {
		t.Fatalf("왕복 후 logit 이 다르다: %v vs %v", got.Logit(x), orig.Logit(x))
	}
	if got.NTrain != orig.NTrain || got.L2 != orig.L2 {
		t.Errorf("메타데이터가 왕복하지 않았다")
	}
}

func TestLoadRejectsFeatureNameMismatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	if err := Save(p, sampleModel()); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p, []string{"a", "c"})
	if err == nil {
		t.Fatal("피처 이름이 다른데 로드가 성공했다 — 유일한 근거 불변식이 안 지켜진다")
	}
	if !strings.Contains(err.Error(), "1") {
		t.Errorf("어느 위치가 다른지 알려주지 않는다: %v", err)
	}
}

// Python 의 models.json 을 그대로 주면 encoding/json 이 모르는 필드를 무시해
// 전 필드가 제로값이 된다 — 에러 없이. 원인을 짚는 메시지가 나와야 한다.
func TestLoadRejectsPythonMapShape(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	body := `{"k0":{"l2":10,"coef":[1,2],"intercept":0,"mu":[0,0],"sd":[1,1],` +
		`"n_train":1,"feature_names":["a","b"]},"k3":{}}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p, []string{"a", "b"})
	if err == nil {
		t.Fatal("Python 맵 형태를 받아들였다 — 전 필드가 제로값인 모델이 통과했다")
	}
	if !strings.Contains(err.Error(), "k0") {
		t.Errorf("원인을 짚지 않는다: %v", err)
	}
}

func TestLoadRejectsFeatureCountMismatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	if err := Save(p, sampleModel()); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, []string{"a", "b", "c"}); err == nil {
		t.Fatal("피처 개수가 다른데 로드가 성공했다")
	}
}

func TestLoadRejectsInconsistentArrayLengths(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	// coef 2개인데 mu 1개 — Logit 이 인덱스 범위를 넘는다
	body := `{"l2":10,"coef":[1,2],"intercept":0,"mu":[1],"sd":[1,1],"n_train":1,"feature_names":["a","b"]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, []string{"a", "b"}); err == nil {
		t.Fatal("coef 와 mu 길이가 다른데 로드가 성공했다")
	}
}

func TestLoadRejectsZeroSd(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.json")
	body := `{"l2":10,"coef":[1,1],"intercept":0,"mu":[0,0],"sd":[1,0],"n_train":1,"feature_names":["a","b"]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p, []string{"a", "b"}); err == nil {
		t.Fatal("sd 에 0 이 있는데 로드가 성공했다 — Logit 이 Inf 를 낸다")
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./internal/model/ -run 'TestSaveLoad|TestLoadRejects' -v`
Expected: `Save`/`Load` 미정의로 컴파일 실패

- [ ] **Step 3: 구현**

`internal/model/serialize.go`:

```go
// Package model 의 직렬화 경로.
//
// Python `engines/logreg.py` 의 to_dict/from_dict 와 키가 같아야 한다. 학습은 Go 로
// 하지만, Python 이 만든 models.json 을 읽어 대조할 수 있어야 진단이 된다.
package model

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// Validate 는 로드된 모델이 실제로 쓸 수 있는 상태인지 본다.
//
// wantNames 대조가 핵심이다. 피처 순서는 features.FeatureNames 가 유일한 근거인데,
// 그 불변식은 학습 시점에만 강제되고 예측 시점에는 아무도 확인하지 않았다. 모델을
// 새로 학습한 뒤 피처를 하나 추가하면, 옛 models.json 이 조용히 로드되어 60개 계수를
// 엉뚱한 피처에 곱한다. 확률은 여전히 0~1 사이라 눈에 띄지 않는다.
func (m *LogReg) Validate(wantNames []string) error {
	if len(m.FeatureNames) == 0 {
		// Python 의 models.json 은 {"k0": {...}, "k3": {...}} 형태의 맵이다.
		// 그것을 LogReg 로 언마샬하면 encoding/json 이 모르는 필드를 조용히 무시해
		// 전 필드가 제로값이 된다. 에러가 안 나므로 여기서 짚어준다.
		return fmt.Errorf("모델이 비었다 — Python 의 models.json 은 " +
			`{"k0": {...}, "k3": {...}} 형태의 맵이라 그대로 읽히지 않는다. ` +
			"cmd/train 으로 만들거나 k0 항목만 꺼내서 저장할 것")
	}
	if len(m.FeatureNames) != len(wantNames) {
		return fmt.Errorf("피처 개수 %d, 기대 %d — 모델과 코드의 피처 집합이 다르다",
			len(m.FeatureNames), len(wantNames))
	}
	for i := range wantNames {
		if m.FeatureNames[i] != wantNames[i] {
			return fmt.Errorf("피처 이름이 %d번째에서 다르다: 모델 %q, 코드 %q",
				i, m.FeatureNames[i], wantNames[i])
		}
	}
	p := len(wantNames)
	if len(m.Coef) != p || len(m.Mu) != p || len(m.Sd) != p {
		return fmt.Errorf("배열 길이가 어긋난다: coef %d / mu %d / sd %d, 기대 %d",
			len(m.Coef), len(m.Mu), len(m.Sd), p)
	}
	for i, s := range m.Sd {
		if s == 0 || math.IsNaN(s) || math.IsInf(s, 0) {
			return fmt.Errorf("sd[%d] = %v — 표준화에서 0 나눗셈이나 비유한 값이 된다", i, s)
		}
	}
	for i, v := range m.Coef {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("coef[%d] 가 비유한 값이다: %v", i, v)
		}
	}
	for i, v := range m.Mu {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("mu[%d] 가 비유한 값이다: %v", i, v)
		}
	}
	if math.IsNaN(m.Intercept) || math.IsInf(m.Intercept, 0) {
		return fmt.Errorf("intercept 가 비유한 값이다: %v", m.Intercept)
	}
	return nil
}

// Save 는 원자적으로 쓴다 — 봇이 읽는 중에 학습이 덮어쓸 수 있다.
func Save(path string, m *LogReg) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-model-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load 는 읽은 즉시 Validate 를 건다. 검증 없이 모델을 돌려주는 경로는 두지 않는다.
func Load(path string, wantNames []string) (*LogReg, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("모델을 읽지 못했다: %w\n  cmd/train 으로 만든다", err)
	}
	var m LogReg
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("모델 JSON 파싱 실패 (%s): %w", path, err)
	}
	if err := m.Validate(wantNames); err != nil {
		return nil, fmt.Errorf("모델 검증 실패 (%s): %w", path, err)
	}
	return &m, nil
}
```

- [ ] **Step 4: 통과 확인**

```bash
GOTOOLCHAIN=local go test ./internal/model/ -v
GOTOOLCHAIN=local go vet ./... && gofmt -l .
```
Expected: 전부 PASS, vet·gofmt 무출력

- [ ] **Step 5: 돌연변이로 판별력 확인**

`Validate` 의 이름 비교 루프를 통째로 지우고 테스트가 실패하는지 본다. 실패하면
되돌린다. 어느 돌연변이를 썼고 무엇이 실패했는지 리포트에 적는다.

- [ ] **Step 6: 커밋**

```bash
git add internal/model/serialize.go internal/model/serialize_test.go
git commit -m "모델 직렬화 — 로드 시 FeatureNames 대조로 서빙 경로 불변식을 강제한다"
```

---

### Task 4: 표본 생성 공용화와 cmd/train — models.json 생성

봇이 쓸 모델을 만든다. 워크포워드로 성능을 확인한 뒤 **가장 최근 창으로 학습한 모델
하나**를 저장한다. 워크포워드는 검증용이고 저장 대상이 아니다 — 실거래에는 최신
데이터까지 쓴 단일 모델이 필요하다.

**먼저 표본 제외 규칙을 공용 패키지로 뺀다.** 이 계획서의 원래 초안은 `cmd/train` 이
`cmd/backtest` 의 `buildMatrix` 를 복제하도록 지시했다. 그것은 틀렸다 — 복제되는 쪽이
실거래 모델을 만드는 코드이기 때문이다. 두 사본이 어긋나면 봇이 G1' 게이트가 검증한
적 없는 표본 집합으로 학습되는데, **게이트는 `cmd/backtest` 사본만 돌리므로 여전히
통과한다.** 조용히 틀리는 정확히 그 부류다. 규칙은 한 곳에만 둔다.

**Files:**
- Create: `internal/sample/sample.go`, `internal/sample/sample_test.go`, `cmd/train/main.go`
- Modify: `cmd/backtest/main.go:29-34`(상수 블록), `cmd/backtest/main.go:223-330`(`buildMatrix`)
- Modify: `internal/model/logreg.go:51-56`(`Fit` 에 이름 개수 검사), `internal/model/logreg_test.go`(테스트 추가)

**Interfaces:**
- Consumes: `vision.LoadFullHistory`, `clock.New`, `features.Build`, `features.FeatureNames`, `model.NewMatrix/Fit/Save/Load`, `(*LogReg).Validate`, `walkforward.Run/TrainableBefore/DayMS/FiveMinMS`, `metrics.AUC/ECE`
- Produces:
  - `sample.Req1m = 60`, `sample.Req5m = 12` (상수, backtest 의 단언 메시지가 쓴다)
  - `sample.Counts` — `{Kept, Doji, Warmup, Gap int}`
  - `sample.Build(b1, b5 bars.Bars, progress func(kept int)) ([]int64, *model.Matrix, []float64, Counts)`
  - `models.json` (기본 경로 `models.json`), `model.Save` 형식

- [ ] **Step 1: 공용 패키지를 만든다**

`internal/sample/sample.go`. 아래는 `cmd/backtest/main.go:223-274` 의 루프를 **동작을
바꾸지 않고** 옮긴 것이다. 개수 하드 단언은 옮기지 않는다 — 그것은 G1' 게이트에만
속하고 `cmd/train` 은 최신 데이터로도 돌아야 한다.

```go
// Package sample 은 5분봉마다 +0분 시점 표본을 만든다.
//
// 제외 규칙(도지 / 워밍업 / 연속성)은 이 패키지에만 있다. cmd/backtest 는 G1'
// 게이트 러너이고 cmd/train 은 실거래 모델을 만드는데, 둘이 서로 다른 표본 집합을
// 보면 게이트가 검증한 적 없는 모델이 실거래에 나간다 — 게이트는 backtest 쪽만
// 돌리므로 어긋나도 통과한다. 그래서 규칙을 한 곳에 둔다.
package sample

import (
	"time"

	"github.com/kdm000718/GLD-9.1/internal/bars"
	"github.com/kdm000718/GLD-9.1/internal/clock"
	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/model"
	"github.com/kdm000718/GLD-9.1/internal/walkforward"
)

// 피처가 요구하는 최소 봉 수. cmd/backtest 의 단언 메시지가 이 값을 인용하므로
// 내보낸다 — 두 곳에 따로 적으면 메시지와 실제 판정이 어긋날 수 있다.
const (
	Req1m  = 60
	Req5m  = 12
	minMS  = 60_000
	fiveMS = walkforward.FiveMinMS
)

// Counts 는 제외 사유별 개수다. 합계 + Kept 가 입력 5분봉 수와 같아야 한다.
type Counts struct {
	Kept   int
	Doji   int
	Warmup int
	Gap    int
}

// Build 는 +0분 표본을 행렬에 채우고 제외 개수를 함께 돌려준다.
//
// progress 는 nil 이어도 된다. nil 이 아니면 유지 표본이 200,000개 늘 때마다
// 부른다 — 9년치는 15분 가까이 걸리므로 진행이 보여야 한다.
func Build(b1, b5 bars.Bars, progress func(kept int)) ([]int64, *model.Matrix, []float64, Counts) {
	n := b5.Len()
	mat := model.NewMatrix(n, len(features.FeatureNames))
	cs := make([]int64, n)
	y := make([]float64, n)
	var c Counts

	for i := 0; i < n; i++ {
		t := b5.OpenTime[i]
		o, cl := b5.Open[i], b5.Close[i]
		if cl == o {
			c.Doji++
			continue
		}
		v, err := clock.New(t, b1, b5, t)
		if err != nil {
			// t 이전에 마감된 봉이 아예 없다 (데이터 시작부)
			c.Warmup++
			continue
		}
		if v.Bars1m.Len() < Req1m || v.Bars5m.Len() < Req5m {
			c.Warmup++
			continue
		}
		ot1, ot5 := v.Bars1m.OpenTime, v.Bars5m.OpenTime
		l1, l5 := len(ot1), len(ot5)
		if ot1[l1-1] != t-minMS ||
			ot1[l1-1]-ot1[l1-Req1m] != int64(Req1m-1)*minMS ||
			ot5[l5-1]-ot5[l5-Req5m] != int64(Req5m-1)*fiveMS {
			c.Gap++
			continue
		}
		vals, ok := features.Build(v)
		if !ok {
			c.Warmup++
			continue
		}
		mat.SetRow(c.Kept, vals)
		cs[c.Kept] = t
		if cl > o {
			y[c.Kept] = 1
		} else {
			y[c.Kept] = 0
		}
		c.Kept++
		if progress != nil && c.Kept%200_000 == 0 {
			progress(c.Kept)
		}
	}
	mat.Truncate(c.Kept)
	return cs[:c.Kept], mat, y[:c.Kept], c
}

var _ = time.Now // 옮기는 과정에서 남으면 지울 것
```

마지막 `var _ = time.Now` 줄은 **지워라.** 원본이 루프 안에서 경과시간을 찍었는데
그 출력은 호출자로 옮겼으므로 `time` 임포트가 필요 없다. 일부러 남겨 둔 함정이다 —
쓰지 않는 임포트를 그대로 옮기지 않았는지 확인하는 것이다.

- [ ] **Step 2: 공용 함수에 테스트를 쓴다**

`internal/sample/sample_test.go`. 9년치를 돌릴 수는 없으므로 합성 봉으로 각 제외
사유가 실제로 그 사유로 세어지는지 본다.

```go
package sample

import "testing"

// 도지(open == close)는 제외되고 그 사유로 세어진다.
func TestDojiExcluded(t *testing.T) {
	b1, b5 := synthetic(t, 400)
	i := b5.Len() - 1
	b5.Close[i] = b5.Open[i]

	_, _, _, c := Build(b1, b5, nil)
	if c.Doji != 1 {
		t.Fatalf("도지 %d개, 기대 1", c.Doji)
	}
}

// 1분봉 하나를 빼면 연속성이 깨져 결측으로 세어진다. 워밍업이 아니다 —
// 두 사유가 섞이면 backtest 의 개수 단언이 엉뚱한 규칙을 지목한다.
func TestGapCountedAsGapNotWarmup(t *testing.T) {
	b1, b5 := synthetic(t, 400)
	before := func() Counts { _, _, _, c := Build(b1, b5, nil); return c }()

	b1 = drop1mBar(b1, b1.Len()-30)
	after := func() Counts { _, _, _, c := Build(b1, b5, nil); return c }()

	if after.Gap <= before.Gap {
		t.Fatalf("1분봉을 뺐는데 결측이 늘지 않았다: %d → %d", before.Gap, after.Gap)
	}
	if after.Warmup != before.Warmup {
		t.Errorf("결측이 워밍업으로 세어졌다: %d → %d", before.Warmup, after.Warmup)
	}
}

// 모든 표본은 정확히 한 사유로만 세어진다 — 합계가 입력 봉 수와 같아야 한다.
func TestCountsPartitionInput(t *testing.T) {
	b1, b5 := synthetic(t, 400)
	_, _, _, c := Build(b1, b5, nil)
	if got := c.Kept + c.Doji + c.Warmup + c.Gap; got != b5.Len() {
		t.Fatalf("합계 %d != 입력 5분봉 %d — 어떤 봉이 두 번 세어지거나 누락됐다",
			got, b5.Len())
	}
}

// Truncate 후 반환된 세 값의 길이가 서로 맞아야 한다.
func TestReturnedLengthsAgree(t *testing.T) {
	b1, b5 := synthetic(t, 400)
	cs, mat, y, c := Build(b1, b5, nil)
	if len(cs) != c.Kept || len(y) != c.Kept || mat.Rows != c.Kept {
		t.Fatalf("길이 불일치: cs %d / y %d / mat.Rows %d, Kept %d",
			len(cs), len(y), mat.Rows, c.Kept)
	}
}
```

`synthetic(t, n)` 과 `drop1mBar` 는 같은 파일의 헬퍼로 만든다. `synthetic` 은
경계에 맞는 1분봉 `n*5`개와 5분봉 `n`개를 만들되 **`open != close`** 로 채워
도지가 우연히 섞이지 않게 한다(안 그러면 `TestDojiExcluded` 가 1을 기대할 수
없다). `bars.Bars` 의 실제 필드 구성은 `internal/bars` 를 읽고 맞춘다.

- [ ] **Step 3: cmd/backtest 를 공용 함수로 바꾸고 G1' 를 다시 돌린다**

`cmd/backtest/main.go` 의 상수 블록에서 `req1m`/`req5m`/`minMS`/`fiveMS` 를 지우고,
`buildMatrix` 의 루프를 `sample.Build` 호출로 바꾼다. **개수 하드 단언 네 개
(도지 5,039 / 워밍업 37 / 결측 4,458 / kept 932,491)와 그 위의 주석은 그대로
남긴다** — 그것이 G1' 게이트의 일부다. 단언 메시지 안의 `req1m`/`req5m` 는
`sample.Req1m`/`sample.Req5m` 로 바꾼다.

요약 출력 한 줄과 200,000개마다의 진행 출력은 호출자에 남긴다. 출력 문구를 바꾸지
마라 — `docs/results/g1prime-fullhistory.log` 와 대조할 것이다.

```bash
GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go vet ./... && gofmt -d . | wc -l
GOTOOLCHAIN=local go run ./cmd/backtest 2>&1 | tee /tmp/g1prime-after.log | tail -30
```

**이것이 이 스텝의 게이트다.** 다음 값이 하나라도 움직이면 추출이 동작을 바꾼
것이므로 받아들이지 말고 보고하라:

| 값 | 기대 |
|---|---|
| 유지 표본 | 932,491 |
| 도지 / 워밍업 / 결측 | 5,039 / 37 / 4,458 |
| 평가 표본 | 888,525 |
| 정확도 | 52.772% |
| 판정 뒤집힘 | 421 (0.0474%) |
| 재학습 | 104회 |

`docs/results/g1prime-fullhistory.log` 와 `diff` 로 대조하라. 소요 시간 줄과
날짜 때문에 생기는 차이(일별 파일 개수, 1분봉 총수)만 허용된다.

- [ ] **Step 4: `model.Fit` 이 이름 개수를 검사하게 한다**

Task 3 리뷰가 남긴 후속이다. `Fit` (`internal/model/logreg.go:51`)은 `p = mat.Cols`
로 `Coef`/`Mu`/`Sd` 를 만들면서 `names` 는 길이를 보지 않고 그대로 `FeatureNames` 에
복사한다(`:123`). 둘이 다르면 **에러 없이** `len(Coef) != len(FeatureNames)` 인
모순된 모델이 나온다.

Task 3 의 `Validate` 가 로드 시점에 이걸 잡지만, 생성 시점에는 아무도 안 본다 —
`walkforward.Run` 은 `Fit` 을 104회 부르면서 `Validate` 를 한 번도 부르지 않는다.
불변식은 만들어지는 자리에서 강제하는 것이 맞다.

`Fit` 의 `n == 0` 검사 바로 뒤에 넣는다:

```go
	if len(names) != p {
		return nil, fmt.Errorf(
			"피처 이름 %d개, 행렬 열 %d개 — 이름과 열이 어긋나면 계수가 엉뚱한 "+
				"피처에 붙는다", len(names), p)
	}
```

테스트를 `internal/model/logreg_test.go` 에 추가한다:

```go
// 이름 개수와 행렬 열 수가 다르면 Fit 이 거부해야 한다. 통과시키면
// len(Coef) != len(FeatureNames) 인 모델이 만들어지고, 그 모순은 나중에
// Load 시점에야 드러난다 — 그때는 이미 그 모델로 학습이 끝난 뒤다.
func TestFitRejectsNameCountMismatch(t *testing.T) {
	mat := NewMatrix(10, 3)
	rows := make([]int, 10)
	for i := range rows {
		rows[i] = i
		mat.SetRow(i, []float64{float64(i), 1, 2})
	}
	y := make([]float64, 10)
	for i := range y {
		y[i] = float64(i % 2)
	}
	if _, err := Fit(mat, rows, y, []string{"a", "b"}, 10); err == nil {
		t.Fatal("이름 2개 / 열 3개인데 Fit 이 성공했다")
	}
}
```

**G1' 에 영향이 없음을 확인하라.** 게이트 경로는 `names = features.FeatureNames`,
`mat.Cols = len(features.FeatureNames)` 라 이 검사에 걸리지 않는다. Step 3 에서
이미 G1' 를 돌렸다면 이 변경 후 다시 돌릴 필요는 없다 — 대신 `go test -race ./...`
전체가 통과하는지 보고, 특히 `internal/walkforward` 테스트가 그대로인지 확인하라.
하나라도 깨지면 그 호출부가 실제로 어긋난 이름을 넘기고 있었다는 뜻이니 보고하라.

돌연변이: 새 검사를 지우면 `TestFitRejectsNameCountMismatch` 가 FAIL 해야 한다.

- [ ] **Step 5: 러너를 쓴다**

`cmd/train/main.go`:

```go
// Command train 은 Vision 전체 이력으로 학습해 models.json 을 만든다.
//
// 워크포워드를 먼저 돌려 성능을 확인하고, 그다음 가장 최근 train-days 창으로
// 학습한 모델 하나를 저장한다. 워크포워드 모델들은 블록마다 다르므로 저장 대상이
// 아니다 — 실거래에 필요한 것은 최신 데이터까지 반영한 단일 모델이다.
package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/metrics"
	"github.com/kdm000718/GLD-9.1/internal/model"
	"github.com/kdm000718/GLD-9.1/internal/sample"
	"github.com/kdm000718/GLD-9.1/internal/vision"
	"github.com/kdm000718/GLD-9.1/internal/walkforward"
)

const fiveMS = walkforward.FiveMinMS

func main() {
	symbol := flag.String("symbol", "BTCUSDT", "심볼")
	cache := flag.String("cache", "data", "Vision 캐시 디렉터리")
	trainDays := flag.Float64("train-days", 180, "학습 창 (일)")
	refitDays := flag.Float64("refit-days", 30, "워크포워드 재학습 주기 (일)")
	l2 := flag.Float64("l2", 10, "L2 세기")
	out := flag.String("out", "models.json", "출력 경로")
	skipWF := flag.Bool("skip-walkforward", false, "워크포워드 검증을 건너뛴다")
	flag.Parse()

	if *trainDays <= 0 || *refitDays <= 0 {
		fmt.Fprintf(os.Stderr, "실패: -train-days 와 -refit-days 는 0 보다 커야 한다\n")
		os.Exit(2)
	}
	if err := run(*symbol, *cache, *trainDays, *refitDays, *l2, *out, *skipWF); err != nil {
		fmt.Fprintln(os.Stderr, "실패:", err)
		os.Exit(1)
	}
}

func run(symbol, cache string, trainDays, refitDays, l2 float64, out string, skipWF bool) error {
	ctx := context.Background()
	t0 := time.Now()

	fmt.Println("[데이터] Binance Vision 수집")
	b1, err := vision.LoadFullHistory(ctx, symbol, "1m", cache, logf)
	if err != nil {
		return err
	}
	b5, err := vision.LoadFullHistory(ctx, symbol, "5m", cache, logf)
	if err != nil {
		return err
	}
	fmt.Printf("  1분봉 %d / 5분봉 %d  (%.0fs)\n", b1.Len(), b5.Len(), time.Since(t0).Seconds())

	fmt.Println("\n[표본] +0분 피처 생성")
	tSample := time.Now()
	cs, mat, y, counts := sample.Build(b1, b5, func(kept int) {
		fmt.Printf("    ... %d개 (%.0fs)\n", kept, time.Since(tSample).Seconds())
	})
	fmt.Printf("    표본 %d개  제외: 도지 %d / 워밍업 %d / 결측 %d  (%.0fs)\n",
		counts.Kept, counts.Doji, counts.Warmup, counts.Gap, time.Since(tSample).Seconds())
	// cmd/backtest 와 달리 개수 하드 단언은 없다 — train 은 최신 데이터로 돌아야
	// 하므로 개수가 날마다 달라지는 것이 정상이다. 대신 합계 정합성만 본다.
	if got := counts.Kept + counts.Doji + counts.Warmup + counts.Gap; got != b5.Len() {
		return fmt.Errorf("제외 개수 합계 %d 가 5분봉 수 %d 와 다르다", got, b5.Len())
	}
	if len(cs) == 0 {
		return fmt.Errorf("표본이 없다")
	}

	if !skipWF {
		testStart := cs[0] + int64(trainDays*walkforward.DayMS)
		fmt.Printf("\n[검증] 워크포워드 %g일마다 직전 %g일\n", refitDays, trainDays)
		prob, nFit, err := walkforward.Run(cs, mat, y, features.FeatureNames,
			testStart, refitDays, trainDays, l2, logf)
		if err != nil {
			return err
		}
		var ey, ep []float64
		for i := range prob {
			if !math.IsNaN(prob[i]) {
				ey = append(ey, y[i])
				ep = append(ep, prob[i])
			}
		}
		correct := 0
		for i := range ey {
			p := 0.0
			if ep[i] >= 0.5 {
				p = 1.0
			}
			if p == ey[i] {
				correct++
			}
		}
		fmt.Printf("  표본 %d / 재학습 %d회\n", len(ey), nFit)
		fmt.Printf("  정확도 %.3f%%  AUC %.4f  ECE %.4f\n",
			float64(correct)/float64(len(ey))*100, metrics.AUC(ey, ep), metrics.ECE(ey, ep, 10))
	}

	// 저장할 모델: 가장 최근 trainDays 창. 정답이 확정된 표본만 쓴다.
	last := cs[len(cs)-1] + fiveMS
	rows := walkforward.TrainableBefore(cs, last, int64(trainDays*walkforward.DayMS))
	fmt.Printf("\n[학습] 최근 %g일, 표본 %d개  (~%s)\n", trainDays, len(rows), iso(last))
	if len(rows) < 5000 {
		return fmt.Errorf("학습 표본이 %d개뿐이다 — 5,000개 미만이면 신뢰할 수 없다", len(rows))
	}
	lr, err := model.Fit(mat, rows, y, features.FeatureNames, l2)
	if err != nil {
		return err
	}
	if err := lr.Validate(features.FeatureNames); err != nil {
		return fmt.Errorf("학습 직후 검증 실패: %w", err)
	}
	if err := model.Save(out, lr); err != nil {
		return err
	}
	fmt.Printf("  → %s (n_train=%d, l2=%g)\n", out, lr.NTrain, lr.L2)

	// 저장한 것을 다시 읽어 같은 확률을 내는지 확인한다.
	back, err := model.Load(out, features.FeatureNames)
	if err != nil {
		return fmt.Errorf("저장 직후 재로드 실패: %w", err)
	}
	x := mat.Row(rows[len(rows)-1])
	if a, b := lr.Prob(x), back.Prob(x); a != b {
		return fmt.Errorf("왕복 후 확률이 다르다: %v vs %v", a, b)
	}
	fmt.Printf("  왕복 확인: 같은 표본에서 확률 일치 (%.6f)\n", back.Prob(x))
	fmt.Printf("\n총 소요 %.0fs\n", time.Since(t0).Seconds())
	return nil
}

func iso(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02 15:04 UTC") }

func logf(format string, args ...any) { fmt.Printf("    "+format+"\n", args...) }
```

- [ ] **Step 6: 빌드하고 실행한다**

```bash
GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go vet ./... && gofmt -l .
GOTOOLCHAIN=local go run ./cmd/train 2>&1 | tail -20
```
Expected: 워크포워드 정확도가 52.7% 근처, 마지막에 `왕복 확인: ... 확률 일치`.
소요 약 4분(워크포워드 포함).

**워크포워드 수치가 G1' 와 정확히 같을 필요는 없다** — `cmd/train` 은 절단 없이
최신 데이터까지 쓰므로 표본 수가 다르다. 52.7% 근처면 정상이고, 51% 아래거나
54% 위면 무언가 잘못된 것이니 보고한다.

- [ ] **Step 7: models.json 을 커밋하지 않는지 확인**

`models.json` 은 학습 산출물이고 데이터에 따라 바뀐다. `.gitignore` 에 넣는다.

```bash
grep -q '^/models.json$' .gitignore || echo '/models.json' >> .gitignore
git check-ignore models.json && echo "무시됨 — 정상"
```

- [ ] **Step 8: 커밋**

세 갈래로 나눠 커밋한다. 추출은 동작 보존 리팩터, `Fit` 가드는 불변식 강화,
러너는 새 기능이다. 나중에 G1' 가 흔들렸을 때 어느 쪽인지 바로 짚을 수 있어야 한다.

```bash
git add internal/sample/ cmd/backtest/main.go
git commit -m "표본 제외 규칙을 internal/sample 로 공용화 — cmd/train 이 게이트와 같은 표본을 본다"

git add internal/model/logreg.go internal/model/logreg_test.go
git commit -m "Fit 이 이름 개수와 행렬 열 수를 대조한다 — 모순된 모델이 조용히 만들어지지 않도록"

git add cmd/train/ .gitignore
git commit -m "cmd/train — 워크포워드 검증 후 최근 창 모델을 models.json 으로 저장"
```

---

### Task 5: predictfun/ws — WS 연결과 프로토콜

`~/kdm/prediction_market/binance_prediction_data/internal/wsclient` 를 이식한다.
**새로 짜지 않는다.** 그 코드는 하트비트 회신, 재접속 후 전체 재구독, 기본
User-Agent WAF 차단 같은 함정을 이미 밟아본 것이다.

**이 절의 인터페이스는 원본을 실측해 정정한 것이다.** 초안은 `OnReconnect func()`,
`Frame{Topic, Data, RecvMS}` 처럼 원본에 **없는** API 를 적어 두고 동시에 "원본에 없는
기능을 넣지 말라" 고 지시하는 모순이 있었다. 아래는 실제 원본
(`~/kdm/prediction_market/binance_prediction_data/internal/wsclient`)에서 읽은 것이다.

특히 초안의 `OnReconnect func()` 는 **재구독을 할 수 없다** — `Sender` 도 `ctx` 도
받지 않는다. 그런데 같은 절이 "재접속 후 전체 재구독" 을 필수라고 적었다. 원본의
`OnConnect func(ctx, Sender) error` 가 맞는 모양이고, 에러를 돌려줄 수 있어야
재구독 실패 시 그 연결을 버리고 다시 붙을 수 있다.

**Files:**
- Create: `internal/timing/timing.go`, `internal/timing/timing_test.go`
- Create: `internal/predictfun/ws/protocol.go`, `internal/predictfun/ws/conn.go`, `internal/predictfun/ws/protocol_test.go`, `internal/predictfun/ws/conn_test.go`

**Interfaces:**
- Consumes: `github.com/coder/websocket v1.8.12`
- Produces:
  - `timing.Stamp() (unixNs, monoNs int64)`, `timing.NextSeq() uint64`
  - `ws.Frame{Seq uint64, RecvUnixNs int64, RecvMonoNs int64, Raw []byte, Msg Message}`
  - `ws.Sender` — `Send(ctx context.Context, req Request) error`
  - `ws.Options{URL, APIKey, UserAgent string, Logger *slog.Logger, ReadTimeout, StaleTimeout, MaxBackoff time.Duration, OnConnect func(ctx context.Context, s Sender) error, OnFrame func(Frame), OnGap func(start, end time.Time, reason string)}`
  - `ws.New(o Options) *Client` — 기본값 `ReadTimeout 35s`, `StaleTimeout 60s`, `MaxBackoff 30s`
  - `(*Client).Run(ctx) error` — 재접속 포함, ctx 취소까지 돈다
  - `(*Client).Send(ctx, Request) error`, `(*Client).Connected() bool`, `(*Client).Reconnects() int64`, `(*Client).ForceReconnect(reason string)`
  - `ws.SubscribeRequest(id uint64, topic string) Request`, `ws.UnsubscribeRequest`, `ws.HeartbeatReply(ts int64) Request`
  - `ws.TopicOrderbook(marketID int64) string`, `ws.ParseTopic(t string) (kind string, marketID int64, ok bool)`
  - `ws.Message`, `(*Message).IsHeartbeat() bool`

**기본값 셋을 지우지 마라.** `ReadTimeout 35s` 는 하트비트 15초 × 2 + 여유이고,
없으면 반쯤 끊긴 TCP 연결에서 읽기가 영원히 매달린다. 마켓메이킹 봇에서 그것은
**갱신이 멈춘 호가창을 보고 주문을 방치하는 것**과 같다. 원본이 실운용에서 밟은 값이다.

#### 원본과 **일부러** 다르게 가는 한 곳

`OnConnect` 가 에러를 돌려주면 **그 연결을 버리고 재접속한다.** 원본은 그러지
않는다 — `c.log.Warn("재구독 실패", ...)` 만 찍고 그대로 `readLoop` 으로 들어간다
(`conn.go:240-244`).

원본이 옳은 이유가 있다. 그쪽은 읽기 전용 수집기라 구독 몇 개를 놓쳐도 공백을
`OnGap` 으로 기록하고 계속 도는 편이 낫다.

**이 봇에서는 그 선택이 위험하다.** 재구독이 실패하면 시장 데이터가 한 건도
안 오는데, **서버 하트비트는 계속 오므로 35초 읽기 데드라인이 발동하지 않는다.**
연결은 영원히 "살아 있고" 호가창은 영원히 멈춘다. Task 6 의 `Stale` 이 신규 주문을
막아 주지만, 아무것도 재접속을 유발하지 않으므로 봇은 그 상태로 눈이 먼 채
남는다. 그래서 여기서는 버린다.

`runOnce` 가 에러를 돌려주게만 하면 된다 — `Run` 의 재접속 루프(1s → 2s → … →
`MaxBackoff`, 지터 ±20%)를 그대로 타므로 조밀한 재시도가 되지 않는다.

**이 절은 팀리드가 원본을 실측한 뒤 남긴 것이다.** 이 계획서의 이 태스크는 "동작을
개선하지 마라" 라고 적어 두고서 이 한 곳만 개선하라고 요구하는 셈인데, 그것이
의도다. 나중에 원본과 대조하다 이 차이를 발견해도 되돌리지 마라.

- [ ] **Step 1: 원본을 읽는다**

```bash
cat ~/kdm/prediction_market/binance_prediction_data/internal/timing/timing.go
cat ~/kdm/prediction_market/binance_prediction_data/internal/wsclient/protocol.go
cat ~/kdm/prediction_market/binance_prediction_data/internal/wsclient/conn.go
cat ~/kdm/prediction_market/binance_prediction_data/internal/wsclient/protocol_test.go
```

**전부 읽어라.** 초안은 앞부분만 보라고 했는데, 재접속·백오프·`OnGap` 계산은
`runOnce`(191행 이후)와 `readLoop`(302행 이후)에 있다. 거기를 안 보고 옮기면
껍데기만 옮겨진다.

`internal/timing` 도 함께 옮긴다 — `readLoop` 이 프레임을 읽은 **직후** `timing.Stamp()`
으로 시각을 각인하고 `timing.NextSeq()` 로 순번을 붙인다. 파싱 뒤에 찍으면 파싱
시간이 지연에 섞인다(원본 주석 §3.1). 벽시계(`RecvUnixNs`)와 단조시계(`RecvMonoNs`)를
따로 들고 다니는 이유는, NTP 가 시계를 뒤로 당기면 벽시계 기준 신선도 판정이
거꾸로 뒤집히기 때문이다. Task 6 의 `Stale` 이 이 값을 쓴다.

읽고 나서 이식한다. 바꿔야 할 것은 패키지 이름과 모듈 경로뿐이다. **동작을
'개선' 하지 말 것** — 하트비트 응답 형식이나 재구독 순서를 바꾸면 서버가 조용히
구독을 끊는다.

- [ ] **Step 2: 의존성을 추가한다**

```bash
GOTOOLCHAIN=local go get github.com/coder/websocket@v1.8.12
grep coder go.mod
GOTOOLCHAIN=local go list -m -f '{{.GoVersion}}' github.com/coder/websocket
```
Expected: go.mod 에 `v1.8.12`, GoVersion 이 1.22 이하

- [ ] **Step 3: protocol.go 와 conn.go 를 이식한다**

패키지 선언을 `package ws` 로, import 경로의 `predictfun-updown-feed/internal/...` 를
`github.com/kdm000718/GLD-9.1/internal/...` 로 바꾼다. 원본에 없는 기능을 넣지 않는다.

**반드시 그대로 옮겨야 하는 것:**
- 하트비트 프레임을 받으면 `HeartbeatReply` 를 되돌려보내는 경로 — 안 하면 서버가 끊는다
- 재접속 후 **전체 재구독** — 서버는 구독 상태를 기억하지 않는다
- 명시적 `User-Agent` — 기본값은 WAF 가 403 으로 막는다
- `x-api-key` 헤더

- [ ] **Step 4: protocol_test.go 를 이식하고 통과 확인**

```bash
GOTOOLCHAIN=local go test ./internal/predictfun/ws/ -v
```
Expected: 원본 테스트가 전부 PASS

- [ ] **Step 5: 재구독 회귀 테스트를 추가한다**

원본에 없다면 넣는다. 이 동작이 깨지면 재접속 후 조용히 데이터가 안 온다.

`internal/predictfun/ws/conn_test.go`:

```go
package ws

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// 서버가 끊었을 때 재접속하고, OnConnect 가 다시 불려 호출자가 전체 재구독할
// 기회를 얻는지 본다. 서버는 구독 상태를 기억하지 않으므로 이 콜백이 없으면
// 재접속 후 아무 데이터도 오지 않는다 — 에러 없이 조용히.
//
// 횟수만 세지 않고 **넘겨받은 Sender 로 실제 전송이 되는지**까지 본다. 콜백이
// 불리기만 하고 그 자리에서 보낼 수 없으면 재구독은 여전히 불가능하다.
func TestOnConnectCalledAgainAfterDropAndCanSend(t *testing.T) {
	srv := newFlakyServer(t, 1) // 첫 연결을 즉시 끊는다
	defer srv.Close()

	var mu sync.Mutex
	var calls int
	var sendErrs []error
	c := New(Options{
		URL:       srv.URL(),
		UserAgent: "gld91-test",
		OnConnect: func(ctx context.Context, s Sender) error {
			// 토픽 구분자는 슬래시다. 콜론이 아니다 — 원본
			// protocol.go 의 topic() 과 ParseTopic 이 "predictOrderbook/123"
			// 형식을 쓴다(실제 수집 레코드로 확인).
			err := s.Send(ctx, SubscribeRequest(1, "predictOrderbook/1"))
			mu.Lock()
			calls++
			sendErrs = append(sendErrs, err)
			mu.Unlock()
			return nil
		},
		OnFrame: func(Frame) {},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n, errs := calls, append([]error(nil), sendErrs...)
		mu.Unlock()
		if n >= 2 {
			for i, err := range errs {
				if err != nil {
					t.Fatalf("%d번째 OnConnect 에서 재구독 전송이 실패했다: %v", i, err)
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	t.Fatalf("재접속 후 OnConnect 가 %d회만 불렸다 — 최소 2회여야 한다", n)
}

// OnConnect 가 에러를 돌려주면 그 연결을 쓰지 않고 다시 붙어야 한다.
// 재구독이 실패한 연결로 계속 도는 것이 최악이다 — 붙어는 있는데 데이터가 없다.
func TestOnConnectErrorForcesReconnect(t *testing.T) {
	srv := newFlakyServer(t, 0) // 서버는 정상, 실패는 콜백에서 낸다
	defer srv.Close()

	var mu sync.Mutex
	var calls int
	c := New(Options{
		URL:       srv.URL(),
		UserAgent: "gld91-test",
		OnConnect: func(ctx context.Context, s Sender) error {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 1 {
				return errors.New("일부러 실패")
			}
			return nil
		},
		OnFrame: func(Frame) {},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("OnConnect 가 에러를 냈는데 재접속하지 않았다 — 재구독 실패한 연결로 계속 돈다")
}
```

`newFlakyServer(t, n)` 은 `httptest` 로 만든 WS 서버로, 처음 `n`개 연결을 즉시
끊고 그 뒤로는 유지한다(`n == 0` 이면 항상 유지). 원본 저장소에 비슷한 헬퍼가
있으면 그것을 쓴다.

**두 테스트 모두 실시간 대기가 들어 있다.** `-race` 와 함께 돌리고, `-count=5` 로
반복해 안정적인지 확인하라 — 한 번 통과한 타이밍 테스트는 증거가 약하다.

- [ ] **Step 6: 커밋**

```bash
GOTOOLCHAIN=local go test -race -count=5 ./internal/timing/ ./internal/predictfun/ws/
git add internal/timing/ internal/predictfun/ws/ go.mod go.sum
git commit -m "predictfun/ws — 수집기에서 WS 연결·하트비트·재구독 이식"
```

---

### Task 6: predictfun/ws — 오더북 상태

**Files:**
- Create: `internal/predictfun/ws/book.go`, `internal/predictfun/ws/book_test.go`

**Interfaces:**
- Consumes: `ws.Frame`
- Produces:
  - `ws.Book` — 한 마켓의 호가창
  - `ws.NewBook(precision int) *Book`
  - `ws.Tick(price float64, precision int) int64` — `round(price × 10^precision)`. **절단 금지** (Step 3b)
  - `ws.FromTick(tick int64, precision int) float64` — 출력 시점에만 쓴다
  - `ws.QtyDecimals = 6`, `type ws.Shares int64`, `ws.Qty(f float64) Shares`, `(Shares).Float() float64`

    **수량은 소수다 — 정수로 반올림하지 마라.** 초안이 수량 단위를 아예 적지
    않아 구현자가 정수 반올림을 골랐고, 실측 데이터에서 바로 깨졌다. 관측된
    수량: `553.337`, `100.2`, `2.0142`, `0.071`. `0.071` 은 0 으로 반올림되어
    **그 층이 호가창에서 통째로 사라진다.**

    더 심각한 것은 핵심 동작이 뒤집히는 쪽이다. 우리가 `2.04` 주를 걸고 군중이
    `0.4` 주를 건 층은 합계 `2.44` → 반올림 `2`, `exclude` `2` → `2-2 <= 0` 이라
    그 층을 버린다. **군중이 실제로 있는 가격을 비어 있다고 판단한다** — 이 봇의
    전제("우리 주문은 빼되 군중은 따라간다")와 정확히 반대다.

    **단위를 `int64` 로 두지 말고 별도 타입으로 둔다.** 호가창 수량과 `exclude`
    수량의 단위가 어긋나면 제외가 조용히 무효가 되는데, 둘 다 `int64` 면 그
    실수가 컴파일된다. `Shares` 타입과 `Qty()` 생성자를 강제하면 컴파일 에러가
    된다. 구현자가 실제로 이 혼동을 한 번 밟았다(자연 단위로 넣은 `exclude` 가
    배율 단위 저장과 안 맞아 제외가 전혀 안 됐다).

  - `(*Book).BestBid(exclude map[int64]Shares) (tick int64, ok bool)` — `exclude` 값은 반드시 `Qty()` 를 거친 것
  - `(*Book).BestAsk(exclude map[int64]Shares) (tick int64, ok bool)`
  - `(*Book).Apply(f Frame) error` — 프레임은 전체 스냅샷이므로 **통째로 교체**한다. `updateTimestampMs` 가 현재 값 이하면 버린다 (Step 3)
  **한쪽씩 나눠 돌려주는 이유.** 두 호가를 하나의 `ok` 로 묶으면 한쪽만 비었을 때
  `ok=true` 인데 그쪽 틱이 0 인 상태가 만들어진다. 0틱은 가격 0.00 이고, 이 봇은
  최우선 매수호가를 실시간으로 따라가므로 그 값이 그대로 주문 가격이 된다.
  Go 의 표준 `(값, ok)` 형태로 한쪽씩 돌려주면 호출자가 유령 호가를 집을 방법이
  구조적으로 없다.
  - `(*Book).LastRecvMonoNs() int64`
  - `(*Book).Stale(nowMonoNs, afterNs int64) bool`

  **벽시계가 아니라 단조시계로 재는 이유.** Task 5 의 `Frame` 은 `RecvUnixNs` 와
  `RecvMonoNs` 를 따로 들고 온다. 신선도를 벽시계로 재면 NTP 가 시계를 뒤로
  당기는 순간 `now - last` 가 음수가 되어 **오래된 호가창이 갓 갱신된 것처럼
  보인다.** 이 봇은 그 판정으로 주문을 낼지 말지 정하므로 단조시계를 쓴다.
  호출자는 `timing.Stamp()` 의 두 번째 값을 `nowMonoNs` 로 넘긴다.

- [ ] **Step 1: 실패 테스트를 쓴다**

`internal/predictfun/ws/book_test.go`:

```go
package ws

import "testing"

// 가격은 전부 정수 틱으로 다룬다. 정밀도 2 면 1틱 = 0.01 이므로 0.49 는 49틱이다.
//
// 아래 픽스처의 수량 리터럴(50, 30, 100 …)은 Shares 고정소수점 값이지 자연
// 단위 주식 수가 아니다. 이 테스트들은 상대적 크기만 보므로 그대로 둬도
// 동작하지만, **새로 쓰는 테스트에서는 Qty(2.04) 처럼 생성자를 써라** —
// 날숫자를 쓰는 습관이 자연 단위와 배율 단위를 섞는 실수로 이어진다.
// (구현자가 실제로 한 번 밟았다.)
func TestBestExcludesOwnOrders(t *testing.T) {
	b := NewBook(2)
	b.setForTest(map[int64]Shares{47: 100, 48: 50}, map[int64]Shares{52: 80})

	bid, okBid := b.BestBid(nil)
	ask, okAsk := b.BestAsk(nil)
	if !okBid || bid != 48 {
		t.Fatalf("제외 없음: bid=%d ok=%v, 기대 48/true", bid, okBid)
	}
	if !okAsk || ask != 52 {
		t.Fatalf("제외 없음: ask=%d ok=%v, 기대 52/true", ask, okAsk)
	}

	// 48틱의 50수량이 전부 우리 것이면 최우선 매수호가는 47 로 내려간다.
	bid, okBid = b.BestBid(map[int64]Shares{48: 50})
	if !okBid || bid != 47 {
		t.Fatalf("자기 주문 제외 후 bid=%d ok=%v, 기대 47/true", bid, okBid)
	}
	ask, okAsk = b.BestAsk(map[int64]Shares{48: 50})
	if !okAsk || ask != 52 {
		t.Errorf("매수 쪽 제외가 매도 쪽을 바꿨다: ask=%d ok=%v", ask, okAsk)
	}
}

func TestBestPartialOwnQuantityKeepsLevel(t *testing.T) {
	b := NewBook(2)
	b.setForTest(map[int64]Shares{48: 50}, map[int64]Shares{52: 10})
	// 50 중 30 만 우리 것이면 그 층은 남는다.
	bid, ok := b.BestBid(map[int64]Shares{48: 30})
	if !ok || bid != 48 {
		t.Fatalf("부분 제외 후 bid=%d ok=%v, 기대 48/true (20 이 남아 있다)", bid, ok)
	}
	// 정확히 전량이 우리 것이면 그 층은 사라진다 — 경계값.
	if _, ok := b.BestBid(map[int64]Shares{48: 50}); ok {
		t.Error("전량이 우리 것인데 그 층이 남아 있다")
	}
}

// 한쪽만 비었을 때 그쪽 ok 가 반드시 false 여야 한다. 이것을 놓치면 호출자가
// 틱 0(가격 0.00)을 최우선 매수호가로 읽고 그 값으로 주문을 낸다.
func TestBestEmptyBidSideDoesNotLeakPhantomTick(t *testing.T) {
	b := NewBook(2)
	b.setForTest(nil, map[int64]Shares{52: 10})

	if _, ok := b.BestBid(nil); ok {
		t.Error("매수 층이 하나도 없는데 BestBid 가 ok=true 를 냈다 — 유령 호가")
	}
	ask, ok := b.BestAsk(nil)
	if !ok || ask != 52 {
		t.Fatalf("매도 쪽까지 죽었다: ask=%d ok=%v, 기대 52/true", ask, ok)
	}
}

// 반대 방향도 같은 규칙이다.
func TestBestEmptyAskSideDoesNotLeakPhantomTick(t *testing.T) {
	b := NewBook(2)
	b.setForTest(map[int64]Shares{48: 10}, nil)

	if _, ok := b.BestAsk(nil); ok {
		t.Error("매도 층이 하나도 없는데 BestAsk 가 ok=true 를 냈다 — 유령 호가")
	}
	bid, ok := b.BestBid(nil)
	if !ok || bid != 48 {
		t.Fatalf("매수 쪽까지 죽었다: bid=%d ok=%v, 기대 48/true", bid, ok)
	}
}

func TestStaleAfterNoUpdate(t *testing.T) {
	const sec = int64(time.Second) // 단조시계는 나노초다
	b := NewBook(2)
	b.setLastRecvForTest(1_000 * sec)
	if b.Stale(1_002*sec, 3*sec) {
		t.Error("2초 경과인데 3초 문턱에서 stale 로 봤다")
	}
	if !b.Stale(1_004*sec, 3*sec) {
		t.Error("4초 경과인데 stale 이 아니라고 봤다 — 오래된 호가로 재주문하게 된다")
	}
}

// 정확히 문턱에 걸린 순간은 아직 stale 이 아니다. 경계를 고정해 두지 않으면
// 나중에 > 와 >= 를 바꿔 써도 위 테스트가 통과한다.
func TestStaleExactThresholdIsNotStale(t *testing.T) {
	const sec = int64(time.Second)
	b := NewBook(2)
	b.setLastRecvForTest(1_000 * sec)
	if b.Stale(1_003*sec, 3*sec) {
		t.Error("정확히 3초에서 stale 로 봤다 — 경계는 아직 신선이다")
	}
	if !b.Stale(1_003*sec+1, 3*sec) {
		t.Error("3초를 1나노초 넘겼는데 stale 이 아니라고 봤다")
	}
}

// 한 번도 프레임을 받지 않은 호가창은 항상 stale 이어야 한다. 0 으로 초기화된
// lastRecv 를 그대로 쓰면 now - 0 이 거대한 값이라 우연히 맞지만, 그 우연에
// 기대면 안 된다 — 명시적으로 고정한다.
func TestStaleBeforeAnyFrame(t *testing.T) {
	const sec = int64(time.Second)
	b := NewBook(2)
	if !b.Stale(1_000*sec, 3*sec) {
		t.Error("프레임을 한 번도 안 받았는데 신선하다고 봤다")
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./internal/predictfun/ws/ -run TestBest -v`
Expected: `NewBook` 미정의로 컴파일 실패

- [ ] **Step 3: 원본의 오더북 파생 로직을 읽는다**

```bash
sed -n '1,120p' ~/kdm/prediction_market/binance_prediction_data/internal/book/derive.go
```

**프레임은 전체 스냅샷이다 — 델타가 아니다.** 초안은 "스냅샷 대 델타, `Seq` 역행 시
전체 재구독" 을 따르라고 적었는데 그것은 추측이었다. 팀리드가 수집기가 실제로 저장한
레코드를 열어 확인했다:

```json
{"kind":"orderbook","topic":"predictOrderbook/1047104","market_id":1047104,
 "raw":{"asks":[[0.01,553.337],[0.04,100.2],[0.88,55.0],[0.9,100.0],[0.95,3.0],
                [0.98,855.0],[0.99,1528.0]],
        "bids":[],
        "marketId":1047104,"orderCount":22,
        "settlementsPending":{"asks":[[0.05,0.071]],"bids":[[0.01,2.0142]]},
        "updateTimestampMs":1785441600079,"version":1}}
```

`asks`/`bids` 가 매번 **완전한 층 배열**로 온다. 따라서 `Apply` 는 **통째로
교체**한다 — 병합하지 마라. 층을 지우는 델타가 따로 없으므로, 이전 상태를 남기면
사라진 호가가 영원히 남는다.

대신 **순서 역전을 막아라.** 재접속 전후로 오래된 스냅샷이 새 것 위에 덮이면
호가창이 과거로 되돌아간다. `updateTimestampMs` 가 현재 값보다 **작거나 같으면
그 프레임을 버린다.** 같은 값도 버리는 이유는 재구독 직후 같은 스냅샷이 다시
오는 것이 정상이고, 그때 `lastRecvMonoNs` 를 갱신하면 실제로는 멈춘 호가창이
계속 신선해 보이기 때문이다.

**한쪽이 빈 경우가 흔하다.** 위 실제 레코드도 `"bids":[]` 다(만기 직전). 초반과
만기 직전에 정상적으로 발생하므로 에러가 아니다 — `BestBid` 가 `ok=false` 를
돌려주는 경로가 바로 이것이다.

- [ ] **Step 3b: 가격을 틱으로 바꿀 때 반드시 반올림한다**

`Tick(price, precision) = round(price × 10^precision)` 이다. 원본
(`internal/book/derive.go:13`)이 `math.Round` 를 쓴다. **절단하면 안 된다.**
팀리드가 실측한 값:

| 가격 | `price × 100` (float64) | 절단 | 반올림 |
|---|---|---|---|
| 0.29 | `28.999999999999996` | **28** | 29 |
| 0.58 | `57.99999999999999` | **57** | 58 |
| 0.07 | `7.000000000000001` | 7 | 7 |
| 0.49 | `49.0` | 49 | 49 |

한 틱 어긋나면 이 봇에서 무슨 일이 나는지가 중요하다. 최우선 매수호가를 28 로
읽으면 군중이 29 에 있는데 그 뒤에 줄을 서서 영원히 안 체결되고, 매도 쪽을
잘못 읽으면 관통한다. **JSON 의 float 를 그대로 비교하거나 `int64(p*f)` 로
자르지 마라.**

`Complement`(YES↔NO 가격)도 순진한 `1 - p` 가 아니라 마켓 정밀도 기준
`(f - round(p*f)) / f` 다 — 백엔드 반올림과 어긋나지 않게 하려는 것이다.

테스트에 위 표의 네 값을 그대로 넣어라:

```go
func TestTickRoundsNotTruncates(t *testing.T) {
	cases := []struct {
		price     float64
		precision int
		want      int64
	}{
		{0.29, 2, 29}, // 절단하면 28 — float64 로 28.999999999999996 이다
		{0.58, 2, 58}, // 절단하면 57
		{0.07, 2, 7},
		{0.49, 2, 49},
		{0.499, 3, 499},
	}
	for _, c := range cases {
		if got := Tick(c.price, c.precision); got != c.want {
			t.Errorf("Tick(%v, %d) = %d, 기대 %d — 반올림이 아니라 절단하고 있다",
				c.price, c.precision, got, c.want)
		}
	}
}
```

돌연변이: `math.Round` 를 지우고 `int64(price * factor)` 로 바꾸면 0.29 와 0.58
케이스가 FAIL 해야 한다.

- [ ] **Step 4: book.go 를 구현한다**

`Apply` 는 원본의 파생 규칙을 옮기고, `Best` 만 새로 만든다. `Best` 가 새 코드인
이유는 수집기에는 '우리 주문' 개념이 없었기 때문이다.

```go
// BestBid 는 우리 주문을 뺀 최우선 매수호가를 틱으로 돌려준다.
//
// 우리 주문을 빼는 이유: 목표가를 우리 호가에 맞추면 자기 자신을 쫓는 순환이
// 생긴다. 군중을 따라가야 한다. exclude 는 틱→우리 수량이고, 그만큼 뺀 뒤에도
// 수량이 남은 층만 유효한 호가로 본다.
//
// ok=false 면 tick 은 의미가 없다 — 0 을 가격으로 쓰지 마라. 매수와 매도를
// 하나의 ok 로 묶지 않는 이유가 이것이다. 한쪽만 비었을 때 묶인 ok 는 true 가
// 되고, 빈 쪽의 틱 0(가격 0.00)이 그대로 주문 가격으로 흘러간다.
func (b *Book) BestBid(exclude map[int64]Shares) (tick int64, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for t, qty := range b.bids {
		if qty-exclude[t] <= 0 {
			continue
		}
		if !ok || t > tick {
			tick, ok = t, true
		}
	}
	return tick, ok
}

// BestAsk 는 BestBid 와 같은 규칙으로 최우선 매도호가를 돌려준다.
func (b *Book) BestAsk(exclude map[int64]Shares) (tick int64, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for t, qty := range b.asks {
		if qty-exclude[t] <= 0 {
			continue
		}
		if !ok || t < tick {
			tick, ok = t, true
		}
	}
	return tick, ok
}

// Stale 은 마지막 갱신 후 afterNs 를 넘었는지 본다. 넘으면 호출자는 신규 주문을
// 멈추고 기존 주문을 취소해야 한다 — 오래된 호가를 보고 재주문하는 것이 최악이다.
//
// 단조시계 기준이다(Frame.RecvMonoNs). 벽시계로 재면 NTP 보정 한 번에 판정이
// 뒤집힌다. 프레임을 한 번도 못 받았으면 항상 stale 이다.
func (b *Book) Stale(nowMonoNs, afterNs int64) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.lastRecvMonoNs == 0 {
		return true
	}
	return nowMonoNs-b.lastRecvMonoNs > afterNs
}
```

`setForTest`/`setLastRecvForTest` 는 같은 패키지의 테스트 전용 헬퍼로 둔다.
`book_test.go` 는 `time` 을 임포트한다(`int64(time.Second)` 로 나노초를 쓴다).

- [ ] **Step 5: 통과 확인과 돌연변이**

```bash
GOTOOLCHAIN=local go test -race ./internal/predictfun/ws/ -v
```

돌연변이 셋을 각각 걸고 어느 테스트가 잡는지 확인한 뒤 되돌린다. 컴파일되는
것만 유효하다. 잡히지 않는 것이 있으면 테스트를 추가해서 닫고, 무엇을 왜
추가했는지 리포트에 적는다.

1. `BestBid` 의 `qty-exclude[t] <= 0` 을 `qty <= 0` 으로 — 제외가 무력화된다.
2. `BestBid` 의 `qty-exclude[t] <= 0` 을 `qty-exclude[t] < 0` 으로 — 전량이 우리
   것인 층이 살아남는다. 경계값 하나만 어긋나는 돌연변이다.
3. `BestBid` 의 반환을 `return tick, true` 로 — 빈 쪽에서 유령 틱 0 이 샌다.
   이 태스크가 계획서에서 정정된 결함이 정확히 이것이므로 반드시 확인한다.

- [ ] **Step 6: 커밋**

```bash
git add internal/predictfun/ws/book.go internal/predictfun/ws/book_test.go
git commit -m "predictfun/ws — 오더북 상태와 우리 주문을 제외한 최우선 호가"
```

---

### Task 7: predictfun/auth — EIP-712 인증

`GET /v1/auth/message` → EOA 로 서명 → `POST /v1/auth/jwt` → `Authorization: Bearer`.
API 키(`x-api-key`)는 별도로 계속 붙인다 — 키는 읽기·레이트리밋용이고 주문 권한과
무관하다.

**Files:**
- Create: `internal/predictfun/auth/auth.go`, `internal/predictfun/auth/auth_test.go`

**Interfaces:**
- Consumes: `rest.Client` (`internal/predictfun/rest`), `github.com/ethereum/go-ethereum/crypto`
- Produces:
  - `auth.Signer` — `crypto.Sign` 을 감싼 것. `auth.NewSigner(hexKey string) (*Signer, error)`, `(*Signer).Address() common.Address`, `(*Signer).SignHash(h []byte) ([]byte, error)`, `(*Signer).String() string`

    `String()` 은 테스트가 부르므로 반드시 있어야 한다. **주소만 찍는다** —
    개인키를 들고 있는 타입에 `String()` 을 다는 이유가 바로 `%v` 가 기본
    구조체 출력으로 떨어지는 것을 막기 위해서다.

    `crypto.Sign` 은 `v` 를 **0 또는 1** 로 돌려준다(27/28 이 아니다). 이더리움
    관례로 맞추려면 65번째 바이트에 27 을 더해야 하고, 테스트가 그것을 요구한다.
    안 더하면 컨트랙트가 서명을 조용히 거부한다.
  - `auth.Authenticator{Rest *rest.Client, Signer *Signer}`
  - `(*Authenticator).Token(ctx) (string, error)` — 캐시하고 만료 전에 갱신

- [ ] **Step 1: 의존성을 고정해 추가한다**

```bash
GOTOOLCHAIN=local go get github.com/ethereum/go-ethereum@v1.15.0
grep -E "^go |ethereum" go.mod
GOTOOLCHAIN=local go build ./...
```
Expected: `github.com/ethereum/go-ethereum v1.15.0`, 빌드 성공.

**v1.15.0 에서 벗어나면 안 된다.** v1.17.5 는 `go >= 1.24.0` 을 요구해 로컬
툴체인(go1.22.2)으로 빌드가 깨진다. `go get -u` 를 쓰지 말 것.

**`go` 지시자가 `go 1.22` 에서 `go 1.22.0` 으로 바뀐다 — 정상이다.** 팀리드가
스크래치 모듈에서 실측했다. 패치 버전이 붙을 뿐 최소 요구는 그대로이고,
`coder/websocket v1.8.12` 와 공존하며 go1.22.2 로 빌드·실행된다. **되돌리지 마라.**
막아야 하는 것은 `1.23` 이나 `1.24` 로 **올라가는** 경우다. 그때는 무엇이 끌어올렸는지
`go mod graph` 로 찾아 보고하라.

**`apitypes.TypedDataDomain.ChainId` 의 타입에 주의하라.** `*math.HexOrDecimal256`
(`github.com/ethereum/go-ethereum/common/math`)이고 `apitypes` 안에 있지 않다.
팀리드가 여기서 한 번 헛짚었다. `math.NewHexOrDecimal256(56)` 로 만든다.

실측으로 확인된 것(스크래치 모듈, 저장소 무변경):

```
주소: 0x27000F84214f79B0600aa86841958b13ac98242a
apitypes.TypedDataAndHash 정상 동작, err=nil
websocket 과 동시 링크됨
```

주소가 Step 2 의 `TestNewSignerDerivesAddress` 기대값과 같다 — 그 테스트는
이 라이브러리로 통과한다.

- [ ] **Step 2: 실패 테스트를 쓴다**

`internal/predictfun/auth/auth_test.go`:

```go
package auth

import (
	"fmt"
	"math/big"
	"strings"
	"testing"
)

// 테스트 전용 키.
//
// **이 키는 go-ethereum 문서에 실린 공개 키다.** 인터넷 어디서나 볼 수 있고
// 대응 주소 0x27000F84214f79B0600aa86841958b13ac98242a 의 개인키를 누구나
// 안다. 절대 자금을 보내지 마라 — 보내는 즉시 사라진다.
const testKey = "4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3"

func TestNewSignerDerivesAddress(t *testing.T) {
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	got := s.Address().Hex()
	if got != "0x27000F84214f79B0600aa86841958b13ac98242a" {
		t.Fatalf("주소 %s, 기대 0x27000F84214f79B0600aa86841958b13ac98242a", got)
	}
}

func TestNewSignerAcceptsZeroXPrefix(t *testing.T) {
	a, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSigner("0x" + testKey)
	if err != nil {
		t.Fatalf("0x 접두사를 거부했다: %v", err)
	}
	if a.Address() != b.Address() {
		t.Error("0x 접두사 유무로 주소가 달라졌다")
	}
}

func TestNewSignerRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "0x", "zzzz", testKey[:60]} {
		if _, err := NewSigner(bad); err == nil {
			t.Errorf("%q 를 받아들였다", bad)
		}
	}
}

// 개인키가 어떤 경로로도 새지 않아야 한다.
//
// **16진수만 찾으면 안 된다.** ecdsa.PrivateKey 는 비밀 스칼라를 D *big.Int 로
// 들고 있고, fmt 는 그것을 **십진수**로 찍는다. 팀리드가 실측했다 —
// fmt.Sprintf("%v", privKey) 한 줄이 `D:+3439081988824039...` 로 키 전체를
// 출력하는데, testKey 의 16진수 부분문자열을 찾는 검사는 여기서 절대 걸리지
// 않는다. 통과하면서 유출을 놓치는 검사가 된다.
//
// 그래서 금지 바늘을 세 가지로 만든다: 소문자 16진수, 대문자 16진수, 십진수.
// keyLeaks 는 out 에 개인키가 들어 있으면 걸린 바늘을, 없으면 빈 문자열을
// 돌려준다. **판정과 실패 보고를 분리한 이유**는 아래 자기검사 테스트가 이
// 함수를 직접 부를 수 있어야 하기 때문이다. t.Fatalf 를 안에서 부르면
// runtime.Goexit 이 걸려 자기검사 자체가 죽는다.
func keyLeaks(out string) string {
	d, ok := new(big.Int).SetString(testKey, 16)
	if !ok {
		panic("테스트 키를 big.Int 로 못 읽었다")
	}
	needles := []string{
		strings.ToLower(testKey)[:16],
		strings.ToUpper(testKey)[:16],
		d.String()[:16], // 십진수 표현 — fmt 가 실제로 찍는 형태
	}
	lower := strings.ToLower(out)
	for _, n := range needles {
		if strings.Contains(out, n) || strings.Contains(lower, strings.ToLower(n)) {
			return n
		}
	}
	return ""
}

func assertNoKeyLeak(t *testing.T, label, out string) {
	t.Helper()
	if n := keyLeaks(out); n != "" {
		t.Fatalf("%s 에 개인키가 들어 있다 (바늘 %q): %s", label, n, out)
	}
}

func TestSignerNeverExposesKey(t *testing.T) {
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}

	// Signer 가 만들어낼 수 있는 모든 문자열을 훑는다.
	assertNoKeyLeak(t, "String()", s.String())
	assertNoKeyLeak(t, "Address().Hex()", s.Address().Hex())
	assertNoKeyLeak(t, `Sprintf("%v")`, fmt.Sprintf("%v", s))
	assertNoKeyLeak(t, `Sprintf("%+v")`, fmt.Sprintf("%+v", s))
	assertNoKeyLeak(t, `Sprintf("%#v")`, fmt.Sprintf("%#v", s))

	// 실패 경로의 에러 메시지도 본다 — 여기서 새는 것이 가장 흔하다.
	if _, err := s.SignHash(make([]byte, 31)); err != nil {
		assertNoKeyLeak(t, "SignHash 에러", err.Error())
	}
}

// 탐지기 자체가 유효한지 확인한다. 바늘 셋을 만들어 놓고도 여전히 아무것도
// 못 잡을 수 있다 — 원래 계획서의 검사가 정확히 그랬다(16진수만 찾는데 fmt 는
// 십진수로 찍는다).
func TestKeyLeakDetectorActuallyDetects(t *testing.T) {
	d, _ := new(big.Int).SetString(testKey, 16)

	// fmt 가 실제로 만들어내는 형태.
	if keyLeaks(fmt.Sprintf("어쩌다 찍힌 값: D:+%s", d.String())) == "" {
		t.Error("십진수로 유출된 키를 못 잡았다 — 이 검사는 무의미하다")
	}
	// 16진수 두 형태도 잡아야 한다.
	if keyLeaks("key="+strings.ToLower(testKey)) == "" {
		t.Error("소문자 16진수 유출을 못 잡았다")
	}
	if keyLeaks("key="+strings.ToUpper(testKey)) == "" {
		t.Error("대문자 16진수 유출을 못 잡았다")
	}
	// 오탐도 없어야 한다 — 주소와 서명은 정상 출력이다.
	if n := keyLeaks("0x27000F84214f79B0600aa86841958b13ac98242a"); n != "" {
		t.Errorf("주소를 유출로 오탐했다 (바늘 %q)", n)
	}
}

func TestSignHashProduces65Bytes(t *testing.T) {
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	h := make([]byte, 32)
	for i := range h {
		h[i] = byte(i)
	}
	sig, err := s.SignHash(h)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 65 {
		t.Fatalf("서명 %d바이트, 기대 65", len(sig))
	}
	if sig[64] != 27 && sig[64] != 28 {
		t.Errorf("v = %d — 이더리움 관례는 27/28 이다", sig[64])
	}
}

func TestSignHashRejectsWrongLength(t *testing.T) {
	s, _ := NewSigner(testKey)
	if _, err := s.SignHash(make([]byte, 31)); err == nil {
		t.Error("31바이트 해시를 받아들였다")
	}
}
```

- [ ] **Step 3: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./internal/predictfun/auth/ -v`
Expected: `NewSigner` 미정의로 컴파일 실패

- [ ] **Step 4: signer 를 구현한다**

```go
// Package auth 는 EOA 서명과 predict.fun JWT 발급을 담당한다.
//
// 개인키는 환경변수로만 들어오고 이 패키지 밖으로 나가지 않는다. String() 을
// 포함해 어떤 출력 경로에도 키가 실리지 않게 한다 — 로그 한 줄로 자금을 잃는다.
package auth

import (
	"crypto/ecdsa"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

type Signer struct {
	key  *ecdsa.PrivateKey
	addr common.Address
}

func NewSigner(hexKey string) (*Signer, error) {
	k := strings.TrimPrefix(strings.TrimSpace(hexKey), "0x")
	if len(k) != 64 {
		return nil, fmt.Errorf("개인키는 64자리 16진수여야 한다 (받은 길이 %d)", len(k))
	}
	key, err := crypto.HexToECDSA(k)
	if err != nil {
		// err 에 키가 실릴 수 있으므로 감싸지 않는다.
		return nil, fmt.Errorf("개인키를 파싱하지 못했다")
	}
	return &Signer{key: key, addr: crypto.PubkeyToAddress(key.PublicKey)}, nil
}

func (s *Signer) Address() common.Address { return s.addr }

// String 은 주소만 돌려준다. fmt 로 Signer 를 찍어도 키가 새지 않게 한다.
func (s *Signer) String() string { return "Signer(" + s.addr.Hex() + ")" }

// SignHash 는 32바이트 해시에 secp256k1 서명을 하고 65바이트를 돌려준다.
// go-ethereum 은 v 를 0/1 로 주므로 이더리움 관례인 27/28 로 올린다.
func (s *Signer) SignHash(h []byte) ([]byte, error) {
	if len(h) != 32 {
		return nil, fmt.Errorf("해시는 32바이트여야 한다 (받은 길이 %d)", len(h))
	}
	sig, err := crypto.Sign(h, s.key)
	if err != nil {
		return nil, fmt.Errorf("서명 실패: %w", err)
	}
	sig[64] += 27
	return sig, nil
}
```

- [ ] **Step 5: Authenticator 를 구현한다**

`rest.Client` 에는 `Get(ctx, path, q, out)` 만 있다. 같은 스로틀을 쓰는 `Post` 를
`internal/predictfun/rest/client.go` 에 추가한다 — **스로틀을 우회하는 별도 경로를
만들지 말 것.** 레이트리밋은 키 단위이고 인증 요청도 그 예산에서 나간다. `Get` 의
헤더 설정(`x-api-key`, `User-Agent`)과 `throttle(ctx)` 호출을 그대로 따른다.

```go
func (c *Client) Post(ctx context.Context, path string, body, out any) error
```

응답 필드명은 실물을 봐야 확정된다. 그래서 이 단계에서는 `map[string]any` 로 받고
후보 키를 순서대로 훑는다. Task 10 의 testnet 왕복에서 실제 필드를 확인한 뒤
구조체로 바꾸고 이 주석을 지운다.

```go
type Authenticator struct {
	Rest   *rest.Client
	Signer *Signer

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Token 은 JWT 를 돌려준다. 만료 60초 전부터 갱신한다.
//
// 흐름: GET /v1/auth/message 로 서명할 메시지를 받고, EOA 로 서명해
// POST /v1/auth/jwt 에 보내면 Bearer 토큰이 온다. x-api-key 는 두 요청 모두에
// 계속 붙는다 — 키는 읽기·레이트리밋용이고 주문 권한과 무관하다.
func (a *Authenticator) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Until(a.expires) > 60*time.Second {
		return a.token, nil
	}

	var msgResp map[string]any
	if err := a.Rest.Get(ctx, "/v1/auth/message",
		url.Values{"address": {a.Signer.Address().Hex()}}, &msgResp); err != nil {
		return "", fmt.Errorf("인증 메시지 요청 실패: %w", err)
	}
	msg, err := pickString(msgResp, "message", "data", "nonce")
	if err != nil {
		return "", fmt.Errorf("인증 메시지 응답에서 서명할 문자열을 못 찾았다 (키: %v)", keysOf(msgResp))
	}

	// personal_sign 관례: "Ethereum Signed Message:
<len><msg>" 를 keccak 한다.
	sig, err := a.Signer.SignHash(accounts.TextHash([]byte(msg)))
	if err != nil {
		return "", err
	}

	var jwtResp map[string]any
	if err := a.Rest.Post(ctx, "/v1/auth/jwt", map[string]any{
		"address":   a.Signer.Address().Hex(),
		"message":   msg,
		"signature": "0x" + hex.EncodeToString(sig),
	}, &jwtResp); err != nil {
		return "", fmt.Errorf("JWT 발급 실패: %w", err)
	}
	tok, err := pickString(jwtResp, "token", "jwt", "accessToken")
	if err != nil {
		return "", fmt.Errorf("JWT 응답에서 토큰을 못 찾았다 (키: %v)", keysOf(jwtResp))
	}

	a.token = tok
	// 만료를 못 읽으면 보수적으로 10분만 쓴다. 실제 만료는 Task 10 에서 확정한다.
	a.expires = time.Now().Add(10 * time.Minute)
	if secs, ok := jwtResp["expiresIn"].(float64); ok && secs > 0 {
		a.expires = time.Now().Add(time.Duration(secs) * time.Second)
	}
	return tok, nil
}

// pickString 은 후보 키를 순서대로 찾는다. 응답 스키마가 확정되기 전까지의 임시 조치다.
func pickString(m map[string]any, keys ...string) (string, error) {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v, nil
		}
		if inner, ok := m[k].(map[string]any); ok {
			if v, err := pickString(inner, keys...); err == nil {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("없음")
}

// keysOf 는 에러 메시지용이다. 값은 찍지 않는다 — 토큰이 로그에 남으면 안 된다.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
```

`accounts.TextHash` 는 `github.com/ethereum/go-ethereum/accounts` 에 있다. 서버가
`personal_sign` 이 아니라 EIP-712 구조체 서명을 요구할 수도 있으므로, Task 10 에서
거부당하면 그때 바꾼다 — 그 경우 `order.Hash` 와 같은 경로를 쓴다.

- [ ] **Step 6: 통과 확인과 커밋**

```bash
GOTOOLCHAIN=local go test -race ./internal/predictfun/... -v
GOTOOLCHAIN=local go vet ./... && gofmt -l .
git add internal/predictfun/auth/ internal/predictfun/rest/ go.mod go.sum
git commit -m "predictfun/auth — EOA 서명과 JWT 발급 (go-ethereum v1.15.0 고정)"
```

---

### Task 8: predictfun/order — Order 타입과 EIP-712 해싱

**Files:**
- Create: `internal/predictfun/order/order.go`, `internal/predictfun/order/sign.go`, `internal/predictfun/order/order_test.go`

**Interfaces:**
- Consumes: `auth.Signer`, `go-ethereum/signer/core/apitypes`
- Produces:
  - `order.Tick{V int64, Precision int}` — 정수 틱 가격과 그 마켓의 정밀도. `order.NewTick(v int64, precision int) Tick`, `(Tick).Float() float64`, `(Tick).Add(n int64) Tick`, `order.Ceiling(precision int) Tick` (0.5 미만 최대 틱), `(Tick).WeiPerShare() *big.Int` (틱 → 18 decimals wei)

    **`WeiPerShare` 는 float 를 거치면 안 된다.** `V × 10^(18−Precision)` 을
    `big.Int` 정수 연산으로만 계산하라. `float64(V)/10^Precision × 1e18` 로
    가면 값이 어긋난다 — 팀리드 실측:

    | 틱/정밀도 | 정수 경로 | float 경로 | 차이 |
    |---|---|---|---|
    | 49/2 (0.49) | `490000000000000000` | 같음 | 0 |
    | **7/2 (0.07)** | `70000000000000000` | `70000000000000008` | **+8 wei** |
    | 499/3 (0.499) | `499000000000000000` | 같음 | 0 |

    8 wei 는 돈으로는 아무것도 아니지만 **`makerAmount` 가 바뀌면 EIP-712
    다이제스트가 바뀐다.** Task 9 의 G3 는 SDK 와 **비트 일치**를 요구하므로
    그대로 실패하고, 거래소가 금액을 재유도해 대조한다면 주문도 거부된다.
    그리고 0.07 같은 저가는 실제 호가창에 흔하다(실측 레코드에
    `[0.01, 553.337]`, `[0.04, 100.2]` 층이 있다).

    `Float()` 는 **로그·표시 전용**이다. 금액 계산 경로에서 부르지 마라.

    `Ceiling(precision)` 도 float 로 유도하지 마라 — 정밀도 2 면 49, 3 이면
    499 다. `10^precision/2 − 1` 로 계산하면 정수로 끝난다.
  - `order.Order` — 필드 타입이 섞여 있으니 아래대로 쓴다. 초안은 Go 의 필드
    나열 축약으로 적혀 있어 `Maker`/`Signer`/`Taker` 까지 `*big.Int` 로
    읽힐 수 있었다(구현자가 실제로 그렇게 오독할 뻔했고, Step 4 예시 코드와
    Step 5b 골든 일치로 바로잡았다):

    ```go
    type Order struct {
        Salt          *big.Int
        Maker         string // 주소. predictAccount 를 쓰면 Maker == Signer == predictAccount
        Signer        string // 주소
        Taker         string // 주소. 공개 주문이면 0x000…0
        TokenID       *big.Int
        MakerAmount   *big.Int
        TakerAmount   *big.Int
        Expiration    *big.Int
        Nonce         *big.Int
        FeeRateBps    *big.Int
        Side          uint8
        SignatureType uint8
    }
    ```
  - `order.Domain{Name, Version string, ChainID int64, VerifyingContract string}`
  - `order.Hash(o Order, d Domain) ([]byte, error)` — 표준 Order EIP-712 다이제스트
  - `order.RetainSignificantDigits(n *big.Int, digits int) *big.Int`
  - `order.AmountsForBuy(priceWei, quantityWei *big.Int) (makerAmount, takerAmount *big.Int, err error)`

    **`takerAmount` 는 절단된 수량이다** — 넘긴 `quantityWei` 원본이 아니다.
    SDK `dist/OrderBuilder.js:654-679` 를 직접 읽어 확인했다. 원본을 그대로
    돌려주면 같은 의도의 주문이 SDK 와 다른 금액으로 나간다.

    `retainSignificantDigits` 는 반올림이 아니라 **절단**이고, 자릿수는
    유효숫자가 아니라 **10진 자릿수 전체**로 센다(`dist/internal/Utils.js:38-53`:
    `magnitude = absNum.toString().length`). 그래서 `490000000000000000` 은
    18자리라 3자리 기준 `excess=15` 로 계산되지만 나눴다 곱하면 값이 그대로다.

    SDK 를 다시 읽어야 하면 `npm i @predictdotfun/sdk@1.3.8` 후
    `node_modules/@predictdotfun/sdk/dist/` 를 본다.
  - `order.KernelDigest(orderDigest []byte, chainID int64, predictAccount common.Address) ([]byte, error)`
  - `order.SignForPredictAccount(orderDigest []byte, chainID int64, predictAccount, validator common.Address, s *auth.Signer) ([]byte, error)` — 86바이트 봉투
  - `order.SignEOA(o Order, d Domain, s *auth.Signer) ([]byte, error)` — predictAccount 를 쓰지 않는 경우

- [ ] **Step 1: 틱과 금액 변환 테스트를 쓴다**

`internal/predictfun/order/order_test.go`:

```go
package order

import (
	"math/big"
	"testing"
)

// 상한을 0.499 로 박으면 정밀도 2 인 마켓에서 표현 불가능한 가격이 된다.
// 틱에서 유도해야 한다.
func TestCeilingDependsOnPrecision(t *testing.T) {
	if got := Ceiling(2); got.V != 49 || got.Precision != 2 {
		t.Errorf("정밀도 2 의 상한 %+v, 기대 {V:49 Precision:2} (0.49)", got)
	}
	if got := Ceiling(3); got.V != 499 || got.Precision != 3 {
		t.Errorf("정밀도 3 의 상한 %+v, 기대 {V:499 Precision:3} (0.499)", got)
	}
}

func TestTickFloat(t *testing.T) {
	if got := NewTick(49, 2).Float(); got != 0.49 {
		t.Errorf("49틱(정밀도 2) = %v, 기대 0.49", got)
	}
	if got := NewTick(499, 3).Float(); got != 0.499 {
		t.Errorf("499틱(정밀도 3) = %v, 기대 0.499", got)
	}
}

// WeiPerShare 는 정수 연산이어야 한다. float 를 거치면 저가에서 어긋나고,
// 그 차이가 makerAmount 를 바꿔 EIP-712 다이제스트를 바꾼다 — G3 가 SDK 와
// 비트 일치를 요구하므로 그대로 실패한다.
//
// 0.07 케이스가 이 테스트의 핵심이다: float64(7)/100*1e18 은
// 70000000000000008 이 되어 정확값보다 8 wei 크다(팀리드 실측).
func TestWeiPerShareIsExactIntegerMath(t *testing.T) {
	cases := []struct {
		tick      int64
		precision int
		want      string
	}{
		{49, 2, "490000000000000000"},   // 0.49
		{7, 2, "70000000000000000"},     // 0.07 — float 경로면 ...008 이 된다
		{1, 2, "10000000000000000"},     // 0.01 — 실측 호가창의 최저 층
		{4, 2, "40000000000000000"},     // 0.04
		{499, 3, "499000000000000000"},  // 0.499
		{333, 3, "333000000000000000"},  // 0.333
	}
	for _, c := range cases {
		got := NewTick(c.tick, c.precision).WeiPerShare()
		want := mustBig(t, c.want)
		if got.Cmp(want) != 0 {
			t.Errorf("NewTick(%d, %d).WeiPerShare() = %s, 기대 %s — float 를 거치고 있다",
				c.tick, c.precision, got, want)
		}
	}
}

// 0.5 미만 최대 틱도 정수로 유도한다.
func TestCeilingIsIntegerDerived(t *testing.T) {
	if got := Ceiling(2); got.V != 49 {
		t.Errorf("Ceiling(2).V = %d, 기대 49", got.V)
	}
	if got := Ceiling(3); got.V != 499 {
		t.Errorf("Ceiling(3).V = %d, 기대 499", got.V)
	}
	// 상한은 0.5 미만이어야 한다 — 같거나 넘으면 관통 방지가 무너진다.
	for _, p := range []int{2, 3} {
		if f := Ceiling(p).Float(); f >= 0.5 {
			t.Errorf("Ceiling(%d) = %v — 0.5 미만이어야 한다", p, f)
		}
	}
}

// SDK getLimitOrderAmounts(BUY) 를 그대로 따른다. 팀리드가 SDK 소스를 직접 읽어
// 확인했다 (`dist/OrderBuilder.js:654-679`, `dist/internal/Utils.js:38-53`):
//
//	if (quantityWei < 1e16) throw
//	price = retainSignificantDigits(pricePerShareWei, 3)
//	qty   = retainSignificantDigits(quantityWei, 5)
//	makerAmount = (price * qty) / 1e18
//	takerAmount = qty          ← **절단된** qty 다. 원본이 아니다.
//
// **기대값을 구현과 같은 식으로 계산하지 마라.** 초안의 이 테스트가 그랬고,
// 게다가 0.49 / 2주는 유효숫자가 각각 2·1자리라 절단이 아예 발동하지 않는
// 입력이었다 — 절단 코드를 통째로 빠뜨려도 통과한다. 아래 기대값은 팀리드가
// SDK 규칙을 손으로 돌려 얻은 상수다.
func TestAmountsForBuyTruncatesQuantityToFiveDigits(t *testing.T) {
	price := mustBig(t, "490000000000000000")  // 0.49 — 절단해도 안 변한다
	qty := mustBig(t, "1234567890123456789")   // 유효숫자 19자리 — 절단이 발동한다

	maker, taker, err := AmountsForBuy(price, qty)
	if err != nil {
		t.Fatal(err)
	}
	// takerAmount 는 절단된 수량이다. 원본 1234567890123456789 이 아니다.
	wantTaker := mustBig(t, "1234500000000000000")
	if taker.Cmp(wantTaker) != 0 {
		t.Errorf("takerAmount %s, 기대 %s — 절단된 수량이어야 한다(원본 %s 가 아니다)",
			taker, wantTaker, qty)
	}
	// makerAmount 는 절단된 가격 × 절단된 수량 / 1e18 이다.
	wantMaker := mustBig(t, "604905000000000000")
	if maker.Cmp(wantMaker) != 0 {
		t.Errorf("makerAmount %s, 기대 %s", maker, wantMaker)
	}
}

func TestAmountsForBuyTruncatesPriceToThreeDigits(t *testing.T) {
	price := mustBig(t, "456700000000000000") // 0.4567 — 절단되어 0.456 이 된다
	qty := mustBig(t, "2000000000000000000")  // 2주 — 절단 안 됨

	maker, taker, err := AmountsForBuy(price, qty)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustBig(t, "2000000000000000000"); taker.Cmp(want) != 0 {
		t.Errorf("takerAmount %s, 기대 %s", taker, want)
	}
	// 0.456 × 2 = 0.912. 절단을 안 하면 0.9134 가 나온다.
	if want := mustBig(t, "912000000000000000"); maker.Cmp(want) != 0 {
		t.Errorf("makerAmount %s, 기대 %s — 가격이 3자리로 절단되지 않았다", maker, want)
	}
}

// 절단이 발동하지 않는 평범한 경우도 남긴다 — 절단 코드가 멀쩡한 입력을
// 망가뜨리지 않는지 본다.
func TestAmountsForBuyLeavesCleanInputsAlone(t *testing.T) {
	price := mustBig(t, "490000000000000000") // 0.49
	qty := mustBig(t, "2000000000000000000")  // 2주

	maker, taker, err := AmountsForBuy(price, qty)
	if err != nil {
		t.Fatal(err)
	}
	if want := mustBig(t, "2000000000000000000"); taker.Cmp(want) != 0 {
		t.Errorf("takerAmount %s, 기대 %s", taker, want)
	}
	if want := mustBig(t, "980000000000000000"); maker.Cmp(want) != 0 {
		t.Errorf("makerAmount %s, 기대 %s (0.98 USDT)", maker, want)
	}
}

func mustBig(t *testing.T, s string) *big.Int {
	t.Helper()
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("%q 를 big.Int 로 못 읽었다", s)
	}
	return v
}

func TestAmountsForBuyRejectsBelowMinQuantity(t *testing.T) {
	e18 := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	price := new(big.Int).Div(e18, big.NewInt(2))
	// 1e16 미만은 거부. 문서 주석은 1e18 이라 적혀 있으나 코드는 1e16 이다.
	if _, _, err := AmountsForBuy(price, big.NewInt(9_999_999_999_999_999)); err == nil {
		t.Fatal("1e16 미만 수량을 받아들였다")
	}
	if _, _, err := AmountsForBuy(price, big.NewInt(10_000_000_000_000_000)); err != nil {
		t.Fatalf("정확히 1e16 을 거부했다: %v", err)
	}
}

// 절단은 반올림이 아니다. 초과 자릿수만큼 나눴다가 다시 곱한다.
func TestRetainSignificantDigits(t *testing.T) {
	cases := []struct{ in string; digits int; want string }{
		{"123456789", 3, "123000000"},
		{"999999999", 3, "999000000"},   // 반올림이면 1000000000 이 된다
		{"12", 5, "12"},                 // 자릿수가 모자라면 그대로
		{"0", 3, "0"},
		{"490000000000000000", 3, "490000000000000000"},
	}
	for _, c := range cases {
		in, _ := new(big.Int).SetString(c.in, 10)
		want, _ := new(big.Int).SetString(c.want, 10)
		if got := RetainSignificantDigits(in, c.digits); got.Cmp(want) != 0 {
			t.Errorf("RetainSignificantDigits(%s, %d) = %s, 기대 %s", c.in, c.digits, got, c.want)
		}
	}
}

func TestHashIsDeterministic(t *testing.T) {
	o, d := fixtureOrder(), fixtureDomain()
	a, err := Hash(o, d)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Hash(o, d)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatal("같은 입력에서 해시가 다르다")
	}
	if len(a) != 32 {
		t.Fatalf("해시 %d바이트, 기대 32", len(a))
	}
}

func TestHashChangesWithVerifyingContract(t *testing.T) {
	o := fixtureOrder()
	d1 := fixtureDomain()
	d2 := fixtureDomain()
	d2.VerifyingContract = "0x365fb81bd4A24D6303cd2F19c349dE6894D8d58A" // NEG_RISK
	a, _ := Hash(o, d1)
	b, _ := Hash(o, d2)
	if string(a) == string(b) {
		t.Fatal("verifyingContract 가 달라도 해시가 같다 — 계약 구분이 서명에 안 들어갔다")
	}
}
```

`TestAmountsForBuyRoundsDownShares` 는 본문을 채운다 — 주식 수는 내림해서 항상
한도 아래에 있어야 한다. `fixtureOrder`/`fixtureDomain` 도 같은 파일에 만든다.

- [ ] **Step 2: 실패 확인**

Run: `GOTOOLCHAIN=local go test ./internal/predictfun/order/ -v`
Expected: 미정의로 컴파일 실패

- [ ] **Step 3: order.go 를 구현한다**

```go
// Package order 는 CTF Exchange 주문의 값 타입과 EIP-712 서명을 담당한다.
//
// 가격은 전부 정수 틱으로 다룬다. 틱 크기는 마켓의 decimalPrecision 에서 오고
// 실측상 2 와 3 이 공존하므로 전역 상수로 두지 않는다.
package order

// Tick 은 정수 틱 가격이다. 0.5 = 50틱(정밀도 2) 또는 500틱(정밀도 3).
//
// 정밀도를 값과 함께 들고 다니는 이유: 틱 수만으로는 가격을 복원할 수 없고,
// 정밀도가 다른 두 마켓의 틱을 실수로 섞으면 10배 틀린 가격이 된다.
type Tick struct {
	V         int64
	Precision int
}

func NewTick(v int64, precision int) Tick { return Tick{V: v, Precision: precision} }

func (t Tick) Float() float64 { return float64(t.V) / float64(pow10(t.Precision)) }

// Add 는 같은 정밀도를 유지한 채 n 틱 움직인다. 관통 방지의 "best_ask − 1틱" 이 이것이다.
func (t Tick) Add(n int64) Tick { return Tick{V: t.V + n, Precision: t.Precision} }

// Ceiling 은 0.5 미만의 최대 틱이다. 정밀도 2 면 49, 3 이면 499.
// 리터럴 0.499 를 박지 않는 이유: 정밀도 2 인 마켓에서 그 가격은 표현 불가능하다.
func Ceiling(precision int) Tick {
	return Tick{V: pow10(precision)/2 - 1, Precision: precision}
}

func pow10(n int) int64 {
	v := int64(1)
	for i := 0; i < n; i++ {
		v *= 10
	}
	return v
}
```

`AmountsForBuy` 는 `makerAmount` = 지불할 USDT wei, `takerAmount` = 받을 주식 수이고
`가격 = makerAmount / takerAmount` 다.

**"내림" 이 무엇을 내림하는지 헷갈리지 마라 — 정수 주식으로 내리는 것이 아니다.**
주식 수량은 소수다(실측 호가창에 `553.337`, `2.0142`, `0.071` 층이 있고, Task 6 이
그래서 `Shares` 고정소수점을 도입했다). `takerAmount` 도 18 decimals wei 단위이므로
1주 미만 단위가 정상이다.

내림은 두 곳에서 일어나고 둘 다 **절단**이다:

- `retainSignificantDigits` 가 가격을 3자리, 수량을 5자리로 자른다(SDK 규칙).
  둘 다 값을 줄이므로 `makerAmount` 도 줄어든다 — **지불액이 한도를 넘는 방향이
  아니다.**
- 사이징이 USDT 예산에서 주식 수를 낼 때 내림한다. 최대 명목이 equity 의 4.55%
  **미만**(등호 없는 부등호)이라 올림하면 한도를 넘긴다. **이것은 P5 의 몫이고
  이 태스크 범위가 아니다.**

이 태스크에서 `AmountsForBuy` 가 할 일은 SDK 규칙을 그대로 재현하는 것뿐이다.
독자적인 반올림을 넣지 마라.

- [ ] **Step 4: sign.go 를 구현한다**

Task 7 이전에 팀리드가 v1.15.0 에서 동작을 확인한 코드다. 타입 정의 12개 필드의
**순서가 SDK 와 같아야 한다** — EIP-712 타입 해시는 필드 순서에 의존한다.

```go
func typedData(o Order, d Domain) apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain": {
				{Name: "name", Type: "string"},
				{Name: "version", Type: "string"},
				{Name: "chainId", Type: "uint256"},
				{Name: "verifyingContract", Type: "address"},
			},
			"Order": {
				{Name: "salt", Type: "uint256"},
				{Name: "maker", Type: "address"},
				{Name: "signer", Type: "address"},
				{Name: "taker", Type: "address"},
				{Name: "tokenId", Type: "uint256"},
				{Name: "makerAmount", Type: "uint256"},
				{Name: "takerAmount", Type: "uint256"},
				{Name: "expiration", Type: "uint256"},
				{Name: "nonce", Type: "uint256"},
				{Name: "feeRateBps", Type: "uint256"},
				{Name: "side", Type: "uint8"},
				{Name: "signatureType", Type: "uint8"},
			},
		},
		PrimaryType: "Order",
		Domain: apitypes.TypedDataDomain{
			Name:              d.Name,
			Version:           d.Version,
			ChainId:           math.NewHexOrDecimal256(d.ChainID),
			VerifyingContract: d.VerifyingContract,
		},
		Message: apitypes.TypedDataMessage{
			"salt": o.Salt.String(), "maker": o.Maker, "signer": o.Signer, "taker": o.Taker,
			"tokenId": o.TokenID.String(), "makerAmount": o.MakerAmount.String(),
			"takerAmount": o.TakerAmount.String(), "expiration": o.Expiration.String(),
			"nonce": o.Nonce.String(), "feeRateBps": o.FeeRateBps.String(),
			"side": fmt.Sprint(o.Side), "signatureType": fmt.Sprint(o.SignatureType),
		},
	}
}

func Hash(o Order, d Domain) ([]byte, error) {
	h, _, err := apitypes.TypedDataAndHash(typedData(o, d))
	return h, err
}
```

- [ ] **Step 5: Kernel 서명 봉투를 구현한다**

**여기가 이 태스크의 핵심이다.** `predictAccount` 를 쓰면 Order 다이제스트를 그대로
서명하지 않는다. Kernel 도메인으로 한 겹 감싼 뒤 `personal_sign` 하고, 검증자 주소를
앞에 붙인 86바이트 봉투를 만든다. 스펙 §6 의 "predictAccount 주문의 서명은 한 겹 더
감싼다" 를 읽고 시작한다.

```go
// KernelDigest 는 Order 다이제스트를 Kernel 도메인으로 감싼다.
//
// SDK 의 eip712WrapHash + hashKernelMessage 를 옮긴 것이다. 세 군데가 함정이다:
// Kernel 도메인의 verifyingContract 는 predictAccount 이지 Kernel 구현 주소가 아니고,
// 도메인에 salt 는 없으며, inner 는 Order 다이제스트를 Kernel(bytes32 hash) 구조체에
// 넣어 다시 해시한 값이다.
func KernelDigest(orderDigest []byte, chainID int64, predictAccount common.Address) ([]byte, error) {
	if len(orderDigest) != 32 {
		return nil, fmt.Errorf("Order 다이제스트는 32바이트여야 한다 (받은 길이 %d)", len(orderDigest))
	}
	typeHash := crypto.Keccak256([]byte("Kernel(bytes32 hash)"))
	inner := crypto.Keccak256(typeHash, orderDigest) // abi.encode(bytes32,bytes32) == 단순 연결

	kd := apitypes.TypedData{
		Types: apitypes.Types{"EIP712Domain": {
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
			{Name: "chainId", Type: "uint256"},
			{Name: "verifyingContract", Type: "address"},
		}},
		PrimaryType: "EIP712Domain",
		Domain: apitypes.TypedDataDomain{
			Name: "Kernel", Version: "0.3.1",
			ChainId:           math.NewHexOrDecimal256(chainID),
			VerifyingContract: predictAccount.Hex(),
		},
	}
	sep, err := kd.HashStruct("EIP712Domain", kd.Domain.Map())
	if err != nil {
		return nil, err
	}
	return crypto.Keccak256([]byte{0x19, 0x01}, sep, inner), nil
}

// SignForPredictAccount 는 Kernel 계정용 서명 봉투를 만든다.
//
// 마지막 서명이 signTypedData 가 아니라 signMessage 다 — 즉 32바이트 다이제스트에
// "\x19Ethereum Signed Message:\n32" 접두사가 한 번 더 붙는다. 이것을 빠뜨리면
// 서명이 조용히 거부된다.
func SignForPredictAccount(orderDigest []byte, chainID int64,
	predictAccount, validator common.Address, s *auth.Signer) ([]byte, error) {

	d, err := KernelDigest(orderDigest, chainID, predictAccount)
	if err != nil {
		return nil, err
	}
	sig, err := s.SignHash(accounts.TextHash(d))
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+20+65)
	out = append(out, 0x01)
	out = append(out, validator.Bytes()...)
	out = append(out, sig...)
	return out, nil
}
```

`abi.encode(bytes32, bytes32)` 가 두 32바이트 워드의 단순 연결이라는 점을 이용했다.
동적 타입이 없으므로 패딩이 필요 없다. **의심되면 SDK 와 대조하는 Task 9 가 잡는다.**

- [ ] **Step 5b: 팀리드가 SDK 와 대조해 둔 기준값으로 즉시 자가 검증한다**

**Task 9 의 G3 까지 기다리지 마라.** 아래 여섯 값은 팀리드가 SDK(ethers)와 Go
(go-ethereum)로 각각 계산해 **비트 일치**를 확인해 둔 것이다. 여기서 어긋나면
지금 잡는 게 훨씬 싸다 — G3 에서 터지면 어느 단계가 범인인지부터 좁혀야 한다.

두 개의 독립된 테스트로 박아라(`order_test.go`).

**(가) 표준 Order 다이제스트.** 입력:

```
salt          12345
maker=signer  0x1111111111111111111111111111111111111111
taker         0x0000000000000000000000000000000000000000
tokenId       88888888888888888888888888888888888888
makerAmount   980000000000000000        (0.98 USDT)
takerAmount   2000000000000000000       (2 주)
expiration    0
nonce         0
feeRateBps    20
side          0
signatureType 0
도메인        name "predict.fun CTF Exchange", version "1"
```

| 라벨 | verifyingContract / chainId | 기대 다이제스트 |
|---|---|---|
| CTF/56 | `0x8BC070BEdAB741406F4B1Eb65A72bee27894B689` / 56 | `0xaff4afe0cee0b54a087cf7d0146adb548a3e0866e665ceed658e01faac0f5916` |
| NEG_RISK/56 | `0x365fb81bd4A24D6303cd2F19c349dE6894D8d58A` / 56 | `0xb9bb92930fb3be4df91a5ab03f5b585175adbabd771040299332848f031f8857` |
| CTF/97 | `0x2A6413639BD3d73a20ed8C95F634Ce198ABbd2d7` / 97 | `0xb68f4cd30d28c08206a3351743bad88eced96b0387fdfeb53226a521b7e7ff60` |

**(나) Kernel 래핑 다이제스트.** 입력은 합성 다이제스트
`0x000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f`
(0~31 바이트를 그대로 이어붙인 것)이고, Kernel 도메인은
`name "Kernel", version "0.3.1", verifyingContract = predictAccount` 다.

| chainId / predictAccount | 기대 다이제스트 |
|---|---|
| 56 / `0x1111…1111` | `0xaf803f2abd4c257d6146ea9dfab747be742c77a531cb40a535b09aaf83d3b4eb` |
| 56 / `0x2222…2222` | `0xb6c89ab506db96f8b16813a1a302f314c012b47f8271279114eba9d7bc86d3c6` |
| 97 / `0x1111…1111` | `0xca19f3573eaeb97a07416265492f193b30de3246fef541ecf56d025f147a4c94` |

**세 값이 서로 다르다는 것도 함께 단언하라.** 계정과 체인이 다이제스트에 실제로
들어가는지가 그것으로 증명된다 — 전부 같으면 입력이 해시에 반영되지 않은 것이고,
그 상태로도 "결정적이다" 류의 테스트는 통과한다.

대조 스크립트가 저장소에 있다: `tools/sdk_kernel_digest_check.mjs`,
`tools/sdk_order_digest_check.mjs`. 값이 어긋나면 **조정하지 말고 보고하라.**
어느 표가 어긋났는지가 범인을 좁혀준다 — (가)만 틀리면 타입 정의 순서나 도메인
필드이고, (나)만 틀리면 Kernel 도메인 구성이나 `abi.encode` 쪽이다.

- [ ] **Step 6: 봉투 형식 테스트**

```go
func TestSignForPredictAccountEnvelope(t *testing.T) {
	s, err := auth.NewSigner("4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3")
	if err != nil {
		t.Fatal(err)
	}
	acct := common.HexToAddress("0x1111111111111111111111111111111111111111")
	val := common.HexToAddress("0x845ADb2C711129d4f3966735eD98a9F09fC4cE57")
	digest := make([]byte, 32)
	for i := range digest {
		digest[i] = byte(i)
	}
	sig, err := SignForPredictAccount(digest, 56, acct, val, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != 86 {
		t.Fatalf("봉투 %d바이트, 기대 86 (1 + 20 + 65)", len(sig))
	}
	if sig[0] != 0x01 {
		t.Errorf("첫 바이트 0x%02x, 기대 0x01", sig[0])
	}
	if got := common.BytesToAddress(sig[1:21]); got != val {
		t.Errorf("검증자 주소 %s, 기대 %s", got, val)
	}
}

// predictAccount 가 다르면 다이제스트가 달라야 한다 — 계정이 서명에 안 들어가면
// 남의 계정 주문에 내 서명이 통한다.
func TestKernelDigestDependsOnAccount(t *testing.T) {
	d := make([]byte, 32)
	a, _ := KernelDigest(d, 56, common.HexToAddress("0x1111111111111111111111111111111111111111"))
	b, _ := KernelDigest(d, 56, common.HexToAddress("0x2222222222222222222222222222222222222222"))
	if string(a) == string(b) {
		t.Fatal("predictAccount 가 달라도 Kernel 다이제스트가 같다")
	}
	c, _ := KernelDigest(d, 97, common.HexToAddress("0x1111111111111111111111111111111111111111"))
	if string(a) == string(c) {
		t.Fatal("chainId 가 달라도 Kernel 다이제스트가 같다 — 테스트넷 서명이 메인넷에 통한다")
	}
}
```

- [ ] **Step 7: 통과 확인과 커밋**

```bash
GOTOOLCHAIN=local go test -race ./internal/predictfun/order/ -v
GOTOOLCHAIN=local go vet ./... && gofmt -l .
git add internal/predictfun/order/
git commit -m "predictfun/order — Order 타입, 금액 계산, EIP-712 해싱, Kernel 서명 봉투"
```

---

### Task 9: G3 — TS SDK 서명 동등성 게이트

**여기서 실패하면 Task 8 로 돌아간다.** 서명이 어긋나면 거래소가 거부하거나, 더
나쁘게는 의도와 다른 주문이 체결된다.

**Files:**
- Create: `tools/export_sdk_vectors.ts`, `tools/package.json`, `testdata/sdk_signatures.json`, `cmd/g3check/main.go`
- Create: `docs/results/g3-signature.log`

**Interfaces:**
- Consumes: `order.Hash`, `order.KernelDigest`, `order.SignEOA`, `order.SignForPredictAccount`, `auth.NewSigner`

  (초안은 `order.Sign` 이라고 적었는데 그런 이름은 Task 8 에 없다. Task 8 이
  내놓는 것은 `SignEOA` 와 `SignForPredictAccount` 둘이고, G3 는 둘 다 대조한다.)

- Produces: `testdata/sdk_signatures.json` — 배열이고 **원소 9개**. 모든 원소가
  같은 스키마를 쓴다:

  ```json
  {
    "label": "eoa/ctf/prec2",
    "kind": "eoa",                    // "eoa" 또는 "predictAccount"
    "domain": {"name": "...", "version": "1", "chainId": 56, "verifyingContract": "0x..."},
    "order": {"salt": "...", "maker": "0x...", ...},
    "orderDigest": "0x...",           // 표준 Order EIP-712 다이제스트 — 항상 있다
    "predictAccount": null,           // kind=="predictAccount" 일 때만 주소
    "kernelDigest": null,             // kind=="predictAccount" 일 때만 0x...
    "signature": "0x..."              // EOA 는 65바이트, predictAccount 는 86바이트
  }
  ```

  **`digest` 라는 이름을 쓰지 마라.** 초안이 그 이름을 썼는데, predictAccount
  벡터에는 다이제스트가 둘(`orderDigest`, `kernelDigest`)이라 어느 쪽인지
  모호해진다. 모든 원소가 `orderDigest` 를 갖고, Kernel 경로만 `kernelDigest` 를
  추가로 갖는다. EOA 원소에서는 `predictAccount` 와 `kernelDigest` 가 `null` 이다.

- [ ] **Step 1: SDK 로 골든 벡터를 만든다**

`tools/package.json`:

```json
{
  "name": "gld91-sdk-vectors",
  "private": true,
  "type": "module",
  "dependencies": {
    "@predictdotfun/sdk": "1.3.8",
    "ethers": "^6.15.0"
  }
}
```

`ethers` 범위를 `^6.15.0` 으로 적는다. 초안은 `^6.13.0` 이었는데 SDK 의
`peerDependencies` 가 `^6.15.0` 이다(팀리드가 설치해서 확인). `^6.13.0` 도
npm 이 최신(6.17.x)을 고르므로 실무상 통하지만, peer 범위와 어긋나게 적어 둘
이유가 없다.

**골든 벡터 생성은 네트워크 없이 끝난다 — 체인에 붙지 마라.** 팀리드가 SDK 를
설치해 확인한 것:

- `OrderBuilder` 생성자의 `contracts?: MulticallContracts` 와 `signer?` 가 둘 다
  **옵셔널**이다. 온체인 조회가 필요한 메서드를 안 부르면 `contracts` 없이
  만들어도 된다.
- `buildTypedDataHash(typedData): string` 은 **`Promise` 가 아니다.** 동기 함수라
  RPC 를 타지 않는다.
- `signPredictAccountMessage` 는 `this.signer` 와 `this.predictAccount` 만 쓴다
  (`dist/OrderBuilder.js:634-645`). `ecdsaValidatorStorage` 를 조회하지 않는다.
  provider 없는 `new ethers.Wallet(privKey)` 로 충분하다.

그 구현이 우리 Go 경로와 같은 모양임도 대조됐다 — Kernel 도메인의
`verifyingContract` 가 `predictAccount` 이고, `signer.signMessage(32바이트)` 라
**EIP-191 접두사가 한 번 더 붙으며**(Go 쪽은 `accounts.TextHash`), 봉투가
`concat("0x01", ECDSA_VALIDATOR, sig65)` 다.

#### 상수는 SDK 에서 임포트하라 — 우리 표를 베끼지 마라

SDK 가 필요한 상수를 전부 내보낸다(`dist/Constants.js`):
`AddressesByChainId`, `KernelDomainByChainId`, `PROTOCOL_NAME`,
`PROTOCOL_VERSION`, `ORDER_STRUCTURE`, `MAX_SALT`.

**골든 생성 스크립트는 이것들을 임포트해서 써라.** 스펙 §6 의 주소표를 손으로
베껴 넣으면, 그 표가 틀려도 골든이 같은 틀린 값을 담게 되어 **G3 가 통과한다.**
SDK 상수를 쓰면 우리 Go 쪽 표와 SDK 표를 대조하는 셈이 되어 표의 오류까지 잡힌다.

팀리드가 지금 한 번 대조해 뒀다 — **8개 계약 × 2체인 = 16개 주소 전부 일치**
(`CTF_EXCHANGE`, `NEG_RISK_CTF_EXCHANGE`, `YIELD_BEARING_*` 둘,
`CONDITIONAL_TOKENS`, `USDT`, `KERNEL`, `ECDSA_VALIDATOR`).
`MAX_SALT = 2147483648`, `PROTOCOL_NAME = "predict.fun CTF Exchange"`,
`PROTOCOL_VERSION = "1"`, `KernelDomainByChainId[56] = {name:"Kernel",
version:"0.3.1", chainId:56}` 도 확인됐다. 지금 맞다는 것과 앞으로 계속 맞다는
것은 다르므로, 스크립트가 SDK 를 임포트하게 두는 것이 그 대조를 유지하는 방법이다.

`cmd/g3check` 쪽은 반대로 **우리 Go 상수를 써야 한다.** 양쪽이 같은 출처를 쓰면
대조가 아무것도 증명하지 않는다.

`tools/export_sdk_vectors.ts` 는 알려진 입력 여러 벌에 대해 SDK 가 계산하는
다이제스트와 서명을 뽑는다. **두 경로를 모두 덮어야 한다** — 평범한 EOA 서명과,
우리가 실제로 쓸 `predictAccount` Kernel 봉투 서명.

평범한 EOA 경로 (`OrderBuilder.make(chainId, signer)`):

1. 정밀도 2, 가격 0.49, 2주, CTF_EXCHANGE
2. 정밀도 3, 가격 0.499, 2주, CTF_EXCHANGE
3. 같은 주문, `NEG_RISK_CTF_EXCHANGE` — verifyingContract 가 해시를 바꾸는지 고정
4. 큰 금액 (1000주) — 18 decimals 와 유효숫자 절단 회귀
5. `salt` 가 다른 두 주문 — salt 가 해시에 들어가는지 고정

**predictAccount 경로** (`OrderBuilder.make(chainId, signer, {predictAccount})`) — 실제
운용 경로다. `ecdsaValidatorStorage` 를 조회하지 않도록 `signTypedDataOrder` 대신
`buildTypedDataHash` + `signPredictAccountMessage` 를 직접 부르거나, 체인에 붙지 않는
경로를 쓴다:

6. chainId 56, predictAccount A — Order 다이제스트와 **Kernel 래핑 다이제스트를 둘 다** 기록
7. 같은 주문, predictAccount B — 계정이 다이제스트를 바꾸는지 고정
8. 같은 주문, chainId 97 — 체인이 다이제스트를 바꾸는지 고정 (테스트넷 서명이 메인넷에 통하면 안 된다)
9. 최종 86바이트 봉투 — `0x01 || ECDSA_VALIDATOR || sig65` 형식 고정

각 원소에 `orderDigest`(표준) 와 `kernelDigest`(래핑), `signature`(최종 봉투)를
따로 기록한다. Go 쪽이 어느 단계에서 갈리는지 짚으려면 중간값이 필요하다.

서명 키는 테스트 전용 폐기 키
`4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3` 를 쓴다.
이것은 go-ethereum 예제에 실려 널리 공개된 키이고 주소는
`0x27000F84214f79B0600aa86841958b13ac98242a` 다. **이 주소로 자금을 보내지 말 것** —
개인키를 아무나 갖고 있으므로 즉시 인출된다. 골든 파일이 커밋되므로 실지갑 키는
어떤 경우에도 쓰지 않는다.

```bash
cd tools && npm install --no-audit --no-fund 2>&1 | tail -2
npx tsx export_sdk_vectors.ts > ../testdata/sdk_signatures.json
cd .. && python3 - <<'PY'
import json, sys
d = json.load(open('testdata/sdk_signatures.json'))
eoa = [v for v in d if v['kind'] == 'eoa']
pa  = [v for v in d if v['kind'] == 'predictAccount']
print(f'벡터 {len(d)}개  (eoa {len(eoa)} / predictAccount {len(pa)})')
for v in d:
    k = v.get('kernelDigest') or '-'
    print(f"  {v['kind']:<15} {v['label']:<24} order={v['orderDigest'][:18]} kernel={k[:18]}")

bad = []
if len(d) != 9:                    bad.append(f'원소 {len(d)}개, 기대 9')
if len(eoa) != 5:                  bad.append(f'eoa {len(eoa)}개, 기대 5')
if len(pa) != 4:                   bad.append(f'predictAccount {len(pa)}개, 기대 4')
if len({v['orderDigest'] for v in d}) < 2:
    bad.append('orderDigest 가 전부 같다 — 입력이 해시에 안 들어갔다')
if len({v['kernelDigest'] for v in pa}) != len(pa):
    bad.append('kernelDigest 에 중복이 있다 — 계정·체인이 반영 안 됐다')
for v in pa:
    if not v.get('kernelDigest') or not v.get('predictAccount'):
        bad.append(f"{v['label']}: predictAccount 벡터인데 kernelDigest/predictAccount 가 비었다")
    if len(bytes.fromhex(v['signature'][2:])) != 86:
        bad.append(f"{v['label']}: 서명 {len(bytes.fromhex(v['signature'][2:]))}바이트, 기대 86")
for v in eoa:
    if v.get('kernelDigest') or v.get('predictAccount'):
        bad.append(f"{v['label']}: eoa 벡터에 kernel 필드가 있다")
    if len(bytes.fromhex(v['signature'][2:])) != 65:
        bad.append(f"{v['label']}: 서명 {len(bytes.fromhex(v['signature'][2:]))}바이트, 기대 65")
if bad:
    print('\n실패:'); [print('  -', b) for b in bad]; sys.exit(1)
print('\n골든 파일 형태 확인됨')
PY
```

Expected: `벡터 9개 (eoa 5 / predictAccount 4)` 와 `골든 파일 형태 확인됨`.

**초안은 여기서 `Expected: 5개` 라고 적었다.** 그것은 정확히 EOA 벡터 수라,
predictAccount 경로를 통째로 빼먹어도 이 검사가 통과한다 — Step 2 가 "실제 운용
경로를 하나도 안 봤으면 실패" 라고 못 박은 바로 그 상태를 Step 1 이 승인하게
된다. 위 검사는 두 종류의 개수와 서명 길이(65 대 86)를 따로 센다.

- [ ] **Step 2: G3 러너를 쓴다**

`cmd/g3check/main.go` 는 골든을 읽어 각 벡터에 대해 Go 의 `order.Hash` 와
서명 함수를 돌려 대조한다 — `kind` 가 `"eoa"` 면 `order.SignEOA`, `"predictAccount"`
면 `order.KernelDigest` + `order.SignForPredictAccount` 다. 판정 규칙:

- `orderDigest` 는 **완전 일치**여야 한다. 1비트라도 다르면 실패.
- `kernelDigest` 도 완전 일치여야 한다. 여기서 갈리면 `KernelDigest` 의 abi 인코딩이나 도메인 구성이 틀린 것이다.
- 서명도 완전 일치여야 한다 — secp256k1 서명은 결정적(RFC 6979)이므로 같은 키·같은 해시면 같은 서명이 나온다.
- 벡터를 0개 비교하고 통과하지 않는다.
- 다이제스트가 전부 서로 같으면 실패한다 — 입력이 해시에 안 들어간 것이다.
- **평범한 EOA 벡터만 통과하고 predictAccount 벡터를 하나도 안 봤으면 실패한다.** 그것이 실제 운용 경로다.

판정 규칙을 산문으로만 두지 말고 코드로 강제한다. 특히 마지막 것 — "실제 운용
경로를 하나도 안 봤으면 실패" — 은 세지 않으면 지켜지지 않는다.

```go
if compared == 0 {
    return fmt.Errorf("대조한 벡터가 없다")
}
if len(distinct) < 2 {
    return fmt.Errorf("다이제스트가 %d종뿐이다 — 입력이 해시에 반영되지 않는다", len(distinct))
}
// predictAccount 경로가 실제 운용 경로다. EOA 벡터만 통과하고 여기가 0 이면
// 게이트가 아무것도 지키지 않은 것이다.
if comparedKernel == 0 {
    return fmt.Errorf("predictAccount 벡터를 하나도 대조하지 않았다 — "+
        "골든에 %d개가 있어야 한다. 실제 운용 경로가 검증되지 않았다", 4)
}
// 86바이트 봉투 형식도 여기서 고정한다. 길이만 맞고 앞 21바이트가 틀리면
// 체인에서야 드러난다.
for _, v := range kernelVectors {
    if len(v.gotSig) != 86 {
        return fmt.Errorf("%s: 봉투 %d바이트, 기대 86", v.label, len(v.gotSig))
    }
    if v.gotSig[0] != 0x01 {
        return fmt.Errorf("%s: 봉투 첫 바이트 0x%02x, 기대 0x01", v.label, v.gotSig[0])
    }
    if got := common.BytesToAddress(v.gotSig[1:21]); got != ecdsaValidator {
        return fmt.Errorf("%s: 봉투의 validator %s, 기대 %s", v.label, got, ecdsaValidator)
    }
}
```

- [ ] **Step 3: 실행하고 로그를 남긴다**

```bash
mkdir -p out
GOTOOLCHAIN=local go run ./cmd/g3check 2>&1 | tee out/g3-signature.log
cp out/g3-signature.log docs/results/g3-signature.log
```
Expected: `판정: 통과 — G3`

**어긋나면 조정하지 말고 보고한다.** 다이제스트 불일치는 타입 정의 순서, 도메인
필드, 또는 `uint8` 인코딩 중 하나다. 어느 벡터가 어긋났는지가 범인을 좁혀준다.

- [ ] **Step 4: 돌연변이로 게이트가 무는지 확인한다**

`sign.go` 의 타입 정의에서 `salt` 와 `maker` 순서를 바꾸고 G3 가 실패하는지 본다.
실패하면 되돌리고 `git diff` 로 원복을 확인한다. 결과를 리포트에 적는다.

- [ ] **Step 5: 커밋**

```bash
git add tools/package.json tools/package-lock.json tools/export_sdk_vectors.ts \
        testdata/sdk_signatures.json cmd/g3check/ docs/results/g3-signature.log
git status --short   # tools/node_modules 가 섞이면 안 된다 (.gitignore 에 있다)
git commit -m "G3 서명 동등성 게이트 — TS SDK 골든 벡터와 다이제스트·서명 대조"
```

`tools/node_modules` 를 `.gitignore` 에 넣는다.

---

### Task 10: cmd/probe — testnet 왕복과 실측 확인

> **이번 패스의 범위는 Step 1~3 이다** (2026-08-09 사용자 결정).
> Step 4~5(온체인 승인 · 실주문 왕복)는 **하지 않는다.** Step 3 을 끝내고
> Step 4 앞에서 멈춰 보고한다. 자금이 준비되면 별도로 승인받아 진행한다.
>
> 그 결정의 배경: Step 4~5 는 testnet 인데 BNB 는 faucet 으로 받아도
> **predict.fun testnet USDT(`0xB32171ecD878607FFc4F8FC0bCcE6852BB3149E0`)는
> 자체 발행 토큰이라 조달 경로가 불분명하다.** 막힐 수 있는 것을 기다리며
> 진도를 세울 이유가 없고, Step 1~3 만으로도 남은 실측 둘이 확정된다.

**자금이 필요한 것은 Step 4~5 뿐이다.** Step 1~3(설정 확인 · 읽기 경로 60분 ·
인증 왕복)은 가스도 잔고도 없이 끝나고, 실측 4건 중 둘(`decimalPrecision`,
인증 응답 필드)을 여기서 확정한다.

| 스텝 | 필요한 것 |
|---|---|
| 1~3 | 없음 (testnet 은 API 키도 비어 있어야 정상) |
| 4 | Privy 지갑에 BNB 소액 (가스) |
| 5 | predictAccount 에 USDT 잔고 |

Step 2 는 **수집기가 꺼져 있어야** 한다 — API 키당 240 req/min 을 공유한다.
`~/kdm/prediction_market/binance_prediction_data/restart_collectors.sh` 가 되살리는
그 프로세스들이다. 켜져 있으면 끄고, 끝난 뒤 되살릴지는 팀리드에게 묻는다.

**Files:**
- Create: `cmd/probe/main.go`, `docs/results/p4-findings.md`

**Interfaces:**
- Consumes: `rest.Client`, `auth.Authenticator`, `order.Hash`, `order.SignEOA`, `order.SignForPredictAccount`, `ws.Client`, `ws.Book`

  (초안은 `order.Sign` 이라 적었으나 Task 8 에 그런 이름은 없다 — Task 9 에서
  같은 오류를 고쳤다.)

- Produces: `docs/results/p4-findings.md` — 계획 서두의 실측 4건에 대한 답

- [ ] **Step 0: 계정 종류와 서명자를 체인에서 먼저 확인한다 (키·자금 불필요)**

서명을 한 번도 시도하기 전에 **읽기 전용 RPC 세 번**으로 두 가지를 확정할 수 있다 —
이 계정이 Kernel 스마트 계정인지 평범한 EOA 인지, 그리고 **어느 EOA 가 서명자로
등록돼 있는지**. 개인키도 자금도 필요 없다. 팀리드가 2026-08-09 에 실행해 확인했다.

```bash
ACCT=$PREDICT_ACCOUNT          # ~/.config/predictfun/env 에서 온다
RPC=https://bsc-dataseed.binance.org
IMPL_SLOT=0x360894a13ba1a3210667c828492db98dca3e2076cc3735a920a3ca505d382bbc

# (1) 바이트코드가 있는가 — 있으면 컨트랙트 계정이다
curl -sS -X POST -H 'Content-Type: application/json' \
  --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"eth_getCode\",\"params\":[\"$ACCT\",\"latest\"]}" $RPC

# (2) ERC-1967 구현 슬롯 — KERNEL 주소가 나와야 Kernel 계정이다
curl -sS -X POST -H 'Content-Type: application/json' \
  --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"eth_getStorageAt\",\"params\":[\"$ACCT\",\"$IMPL_SLOT\",\"latest\"]}" $RPC

# (3) 등록된 서명자 — ecdsaValidatorStorage(address), 셀렉터 0x20709efc
curl -sS -X POST -H 'Content-Type: application/json' \
  --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"eth_call\",\"params\":[{\"to\":\"0x845ADb2C711129d4f3966735eD98a9F09fC4cE57\",\"data\":\"0x20709efc000000000000000000000000${ACCT#0x}\"},\"latest\"]}" $RPC
```

판정:

| 결과 | 뜻 |
|---|---|
| (1) 이 `0x` | 컨트랙트가 아니다 — **EOA 이거나 아직 미배포 스마트 계정.** 아래 참조 |
| (2) 가 `0xBAC849bB641841b44E965fB01A4Bf5F074f84b4D` | **Kernel 계정 확정.** 86바이트 봉투 경로가 맞다 |
| (2) 가 다른 주소 | Kernel 이 아닌 다른 스마트 계정 — **멈추고 보고하라.** 서명 경로가 통째로 다르다 |
| (3) 의 하위 20바이트 | **이 계정에 등록된 서명자 EOA.** `WALLET_PRIVATE_KEY` 가 이 주소의 것이어야 한다 |

**(3) 을 반드시 `WALLET_PRIVATE_KEY` 의 유도 주소와 대조하라.** 다르면 그 키로 만든
서명은 이 계정에 무효이고 주문이 전부 거부되는데, 에러 메시지는 "서명이 틀렸다" 가
아니라 애매한 거부로 온다. **주소만 찍고 키는 어떤 경로로도 출력하지 마라.**

(1) 이 `0x` 인 경우가 애매하다 — 아직 한 번도 안 쓴 Kernel 계정은 반사실적
(counterfactual)이라 배포 전이다. 그때는 (2)·(3) 도 비어 나오므로, 입금을 한 번
한 뒤 다시 확인하거나 predict.fun 계정 설정에서 확인한다. **판별이 안 된 채로
Step 4~5 로 넘어가지 마라.**

- [ ] **Step 1: testnet 설정을 확인한다**

testnet 베이스 URL 은 `https://api-testnet.predict.fun` 이고 **API 키가 없어야
정상**이다(수집기 설정 주석: "APIKey 는 mainnet 전용, testnet 에서는 비어 있어야
정상"). chainId 와 계약 주소는 스펙에 없으므로 실측한다.

- [ ] **Step 2: 읽기 경로부터 확인한다 — 주문 없이**

마켓 조회 → `decimalPrecision` 확인 → WS 구독 → 오더북 60분 무중단. 이것이
스펙의 P2 통과 조건이다.

```bash
GOTOOLCHAIN=local go run ./cmd/probe -env testnet -mode read -minutes 60 2>&1 | tail -20
```
Expected: 재접속이 있어도 구독이 복구되고, 60분 동안 오더북 갱신이 끊기지 않는다.
`decimalPrecision` 실측값을 기록한다.

- [ ] **Step 3: 인증 왕복**

```bash
GOTOOLCHAIN=local go run ./cmd/probe -env testnet -mode auth 2>&1 | tail -10
```
Expected: JWT 를 받는다. **토큰 값을 로그에 찍지 않는다** — 길이와 만료만 찍는다.
Task 7 에서 `map[string]any` 로 남겨둔 응답 필드를 여기서 확정하고 구조체로 바꾼다.

- [ ] **Step 4: 온체인 승인 (가스 필요)**

USDT `approve`, ConditionalTokens `setApprovalForAll`. **최초 1회다.**

`predictAccount` 가 설정되면 SDK 는 모든 승인을 **`Kernel.execute` 로 라우팅한다**
(README: "every step routes through `Kernel.execute` when a `predictAccount` is
configured"). 승인 주체는 predictAccount 이고 트랜잭션 가스는 **Privy 지갑**이 낸다.
Privy 지갑에 BNB 소액이 있어야 한다.

승인 대상은 마켓이 쓰는 Exchange 주소다. Step 5 의 마켓 메타데이터로 네 변종 중
무엇인지 확정한 뒤 **그 주소에만** 승인한다.

SDK 의 `getApprovalSteps` 는 서명 없이 순수하게 필요한 승인 목록을 돌려준다.
Go 로 옮기기 전에 그 목록을 먼저 뽑아 무엇을 승인하게 되는지 눈으로 확인한다.

- [ ] **Step 5: 최소 금액 주문 왕복 (잔고 필요)**

`LIVE_ARM` 없이 먼저 DRY-RUN 으로 서명까지만 하고 페이로드를 찍는다. 확인 후
`LIVE_ARM='I_UNDERSTAND_THE_RISK'` 로 실제 전송한다.

```bash
GOTOOLCHAIN=local go run ./cmd/probe -env testnet -mode order -usdt 1 2>&1 | tail -20
LIVE_ARM='I_UNDERSTAND_THE_RISK' GOTOOLCHAIN=local go run ./cmd/probe -env testnet -mode order -usdt 1
```

확인할 것: 주문이 받아들여지는가 · 주식 수량이 정수인가 소수인가 ·
`shareThreshold` 가 무엇을 뜻하는가 · 취소가 되는가 · 배치/수정 엔드포인트가 있는가.

- [ ] **Step 6: 실측 결과를 문서로 남긴다**

`docs/results/p4-findings.md` 에 계획 서두의 4건에 대한 답을 적는다. 각 항목에
**무엇을 실행해서 무엇을 봤는지**를 함께 적는다 — 결론만 있으면 다음에 의심이
생겼을 때 재확인할 방법이 없다.

- [ ] **Step 7: 커밋**

```bash
git add cmd/probe/ docs/results/p4-findings.md
git commit -m "cmd/probe — testnet 왕복과 P4 실측 4건 확정"
```

---

## 자기 검토

**스펙 대응.** §6(EIP-712) → Task 8·9. §7 의 오더북 신선도 → Task 6 의 `Stale`.
§9(시크릿) → Task 7 의 `Signer` 와 전역 제약. §8 테스트표의 `order` 행 → Task 9.
모듈 경계표의 `predictfun/ws`·`auth`·`order`·`cmd/train`·`cmd/probe` → Task 4~10.

**이 계획에 없는 것:** `exec`(마켓메이킹 루프)·`risk`(사이저)·`ledger` 는 P5 이고,
도쿄 배포는 P6 다. Task 10 이 주식 수량 단위와 배치 엔드포인트 유무를 확정한 뒤에
그 계획을 쓴다 — 지금 쓰면 사이저와 재호가 루프가 추측 위에 올라간다.

**미해결로 남기는 것:** Task 7 의 인증 응답 필드는 Task 10 전까지 `map[string]any`
다. 이것은 자리표시자가 아니라 **의도적인 유예**이며, 확정 시점과 지울 주석까지
Task 7 Step 5 와 Task 10 Step 3 에 명시했다.
