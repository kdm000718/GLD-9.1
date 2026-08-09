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

스펙이 "추측하면 서명이 조용히 거부되거나, 더 나쁘게는 의도와 다른 금액이 나간다"
고 적은 항목들이다. Task 10 이 실측으로 답을 내고 `docs/results/p4-findings.md` 에 남긴다.

1. Up/Down 마켓이 `CTF_EXCHANGE` 와 `NEG_RISK_CTF_EXCHANGE` 중 무엇을 쓰는가 (verifyingContract 가 갈린다)
2. testnet 의 chainId 와 계약 주소 (스펙에는 메인넷 값만 있다)
3. 주식 수량 단위 — 정수인가 소수 허용인가, `shareThreshold` 의 의미는 무엇인가
4. 주문 배치(batch) 또는 수정(amend) 엔드포인트가 있는가 (있으면 재호가 요청 수가 절반이 되어 500ms 쿨다운을 완화할 수 있다)

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

형식이 바뀌었으므로 **기존 `data/vision` 캐시 950MB 가 전부 못 읽힌다.** 이것은
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

### Task 4: cmd/train — models.json 생성

봇이 쓸 모델을 만든다. 워크포워드로 성능을 확인한 뒤 **가장 최근 창으로 학습한 모델
하나**를 저장한다. 워크포워드는 검증용이고 저장 대상이 아니다 — 실거래에는 최신
데이터까지 쓴 단일 모델이 필요하다.

**Files:**
- Create: `cmd/train/main.go`

**Interfaces:**
- Consumes: `vision.LoadFullHistory`, `clock.New`, `features.Build`, `features.FeatureNames`, `model.NewMatrix/Fit/Save`, `walkforward.Run`, `metrics.AUC/ECE`
- Produces: `models.json` (기본 경로 `models.json`), `model.Save` 형식

- [ ] **Step 1: 러너를 쓴다**

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

	"github.com/kdm000718/GLD-9.1/internal/bars"
	"github.com/kdm000718/GLD-9.1/internal/clock"
	"github.com/kdm000718/GLD-9.1/internal/features"
	"github.com/kdm000718/GLD-9.1/internal/metrics"
	"github.com/kdm000718/GLD-9.1/internal/model"
	"github.com/kdm000718/GLD-9.1/internal/vision"
	"github.com/kdm000718/GLD-9.1/internal/walkforward"
)

const (
	req1m  = 60
	req5m  = 12
	minMS  = 60_000
	fiveMS = walkforward.FiveMinMS
)

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
	cs, mat, y, err := buildMatrix(b1, b5)
	if err != nil {
		return err
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

- [ ] **Step 2: `buildMatrix` 를 같은 파일에 이어 쓴다**

`cmd/backtest/main.go` 의 `buildMatrix` 와 **같은 제외 규칙**을 쓰되, 학습용이므로
개수 하드 단언은 넣지 않는다 — `cmd/train` 은 최신 데이터로도 돌아야 한다.

```go
// buildMatrix 는 +0분 표본을 행렬에 채운다. 제외 규칙은 cmd/backtest 와 같다:
// 도지 / 워밍업 / 최근 구간 연속성. 다만 개수 단언은 넣지 않는다 —
// backtest 는 Python 대조 전용이라 스냅샷에 못 박지만, train 은 최신 데이터로
// 돌아야 하므로 개수가 날마다 달라지는 것이 정상이다.
func buildMatrix(b1, b5 bars.Bars) ([]int64, *model.Matrix, []float64, error) {
	n := b5.Len()
	mat := model.NewMatrix(n, len(features.FeatureNames))
	cs := make([]int64, n)
	y := make([]float64, n)
	kept, skipDoji, skipWarmup, skipGap := 0, 0, 0, 0
	t0 := time.Now()

	for i := 0; i < n; i++ {
		t := b5.OpenTime[i]
		o, c := b5.Open[i], b5.Close[i]
		if c == o {
			skipDoji++
			continue
		}
		v, err := clock.New(t, b1, b5, t)
		if err != nil {
			skipWarmup++
			continue
		}
		if v.Bars1m.Len() < req1m || v.Bars5m.Len() < req5m {
			skipWarmup++
			continue
		}
		ot1, ot5 := v.Bars1m.OpenTime, v.Bars5m.OpenTime
		l1, l5 := len(ot1), len(ot5)
		if ot1[l1-1] != t-minMS ||
			ot1[l1-1]-ot1[l1-req1m] != int64(req1m-1)*minMS ||
			ot5[l5-1]-ot5[l5-req5m] != int64(req5m-1)*fiveMS {
			skipGap++
			continue
		}
		vals, ok := features.Build(v)
		if !ok {
			skipWarmup++
			continue
		}
		mat.SetRow(kept, vals)
		cs[kept] = t
		if c > o {
			y[kept] = 1
		}
		kept++
	}
	fmt.Printf("    표본 %d개  제외: 도지 %d / 워밍업 %d / 결측 %d  (%.0fs)\n",
		kept, skipDoji, skipWarmup, skipGap, time.Since(t0).Seconds())
	mat.Truncate(kept)
	return cs[:kept], mat, y[:kept], nil
}
```

- [ ] **Step 3: 빌드하고 실행한다**

```bash
GOTOOLCHAIN=local go build ./... && GOTOOLCHAIN=local go vet ./... && gofmt -l .
GOTOOLCHAIN=local go run ./cmd/train 2>&1 | tail -20
```
Expected: 워크포워드 정확도가 52.7% 근처, 마지막에 `왕복 확인: ... 확률 일치`.
소요 약 4분(워크포워드 포함).

**워크포워드 수치가 G1' 와 정확히 같을 필요는 없다** — `cmd/train` 은 절단 없이
최신 데이터까지 쓰므로 표본 수가 다르다. 52.7% 근처면 정상이고, 51% 아래거나
54% 위면 무언가 잘못된 것이니 보고한다.

- [ ] **Step 4: models.json 을 커밋하지 않는지 확인**

`models.json` 은 학습 산출물이고 데이터에 따라 바뀐다. `.gitignore` 에 넣는다.

```bash
grep -q '^/models.json$' .gitignore || echo '/models.json' >> .gitignore
git check-ignore models.json && echo "무시됨 — 정상"
```

- [ ] **Step 5: 커밋**

```bash
git add cmd/train/ .gitignore
git commit -m "cmd/train — 워크포워드 검증 후 최근 창 모델을 models.json 으로 저장"
```

---

### Task 5: predictfun/ws — WS 연결과 프로토콜

`~/kdm/prediction_market/binance_prediction_data/internal/wsclient` 를 이식한다.
**새로 짜지 않는다.** 그 코드는 하트비트 회신, 재접속 후 전체 재구독, 기본
User-Agent WAF 차단 같은 함정을 이미 밟아본 것이다.

**Files:**
- Create: `internal/predictfun/ws/protocol.go`, `internal/predictfun/ws/conn.go`, `internal/predictfun/ws/protocol_test.go`

**Interfaces:**
- Consumes: `github.com/coder/websocket v1.8.12`
- Produces:
  - `ws.Options{URL, APIKey, UserAgent, OnFrame func(Frame), OnReconnect func()}`
  - `ws.New(o Options) *Client`
  - `(*Client).Run(ctx) error` — 재접속 포함, ctx 취소까지 돈다
  - `(*Client).Send(ctx, Request) error`
  - `(*Client).Connected() bool`, `(*Client).Reconnects() int64`
  - `ws.SubscribeRequest(id uint64, topic string) Request`, `ws.UnsubscribeRequest`, `ws.HeartbeatReply(ts int64) Request`
  - `ws.Frame{Topic string, Data []byte, RecvMS int64}`

- [ ] **Step 1: 원본을 읽는다**

```bash
sed -n '1,80p'  ~/kdm/prediction_market/binance_prediction_data/internal/wsclient/protocol.go
sed -n '1,120p' ~/kdm/prediction_market/binance_prediction_data/internal/wsclient/conn.go
cat ~/kdm/prediction_market/binance_prediction_data/internal/wsclient/protocol_test.go
```

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
	"sync"
	"testing"
	"time"
)

// 서버가 끊었을 때 재접속하고, OnReconnect 가 불려 호출자가 전체 재구독할 기회를
// 얻는지 본다. 서버는 구독 상태를 기억하지 않으므로 이 콜백이 없으면 재접속 후
// 아무 데이터도 오지 않는다 — 에러 없이 조용히.
func TestReconnectInvokesOnReconnect(t *testing.T) {
	srv := newFlakyServer(t, 1) // 첫 연결을 즉시 끊는다
	defer srv.Close()

	var mu sync.Mutex
	var reconnects int
	c := New(Options{
		URL:       srv.URL(),
		UserAgent: "gld91-test",
		OnReconnect: func() {
			mu.Lock()
			reconnects++
			mu.Unlock()
		},
		OnFrame: func(Frame) {},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go c.Run(ctx)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := reconnects
		mu.Unlock()
		if n >= 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	n := reconnects
	mu.Unlock()
	t.Fatalf("재접속 후 OnReconnect 가 %d회만 불렸다 — 최소 2회여야 한다", n)
}
```

`newFlakyServer` 는 `httptest` 로 만든 WS 서버로, 지정한 횟수만큼 연결을 즉시
끊고 그 뒤로는 유지한다. 원본 저장소에 비슷한 헬퍼가 있으면 그것을 쓴다.

- [ ] **Step 6: 커밋**

```bash
GOTOOLCHAIN=local go test -race ./internal/predictfun/ws/ -v
git add internal/predictfun/ws/ go.mod go.sum
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
  - `(*Book).Apply(f Frame) error`
  - `(*Book).Best(exclude map[int64]int64) (bidTick, askTick int64, ok bool)` — `exclude` 는 틱→수량, 우리 주문을 뺀 최우선 호가
  - `(*Book).LastUpdateMS() int64`
  - `(*Book).Stale(nowMS, afterMS int64) bool`

- [ ] **Step 1: 실패 테스트를 쓴다**

`internal/predictfun/ws/book_test.go`:

```go
package ws

import "testing"

// 가격은 전부 정수 틱으로 다룬다. 정밀도 2 면 1틱 = 0.01 이므로 0.49 는 49틱이다.
func TestBestExcludesOwnOrders(t *testing.T) {
	b := NewBook(2)
	b.setForTest(map[int64]int64{47: 100, 48: 50}, map[int64]int64{52: 80})

	bid, ask, ok := b.Best(nil)
	if !ok || bid != 48 || ask != 52 {
		t.Fatalf("제외 없음: bid=%d ask=%d ok=%v, 기대 48/52", bid, ask, ok)
	}

	// 48틱의 50수량이 전부 우리 것이면 최우선 매수호가는 47 로 내려간다.
	bid, ask, ok = b.Best(map[int64]int64{48: 50})
	if !ok || bid != 47 {
		t.Fatalf("자기 주문 제외 후 bid=%d, 기대 47", bid)
	}
	if ask != 52 {
		t.Errorf("매도 쪽이 바뀌었다: %d", ask)
	}
}

func TestBestPartialOwnQuantityKeepsLevel(t *testing.T) {
	b := NewBook(2)
	b.setForTest(map[int64]int64{48: 50}, map[int64]int64{52: 10})
	// 50 중 30 만 우리 것이면 그 층은 남는다.
	bid, _, ok := b.Best(map[int64]int64{48: 30})
	if !ok || bid != 48 {
		t.Fatalf("부분 제외 후 bid=%d, 기대 48 (20 이 남아 있다)", bid)
	}
}

func TestBestEmptyBidSide(t *testing.T) {
	b := NewBook(2)
	b.setForTest(nil, map[int64]int64{52: 10})
	bid, _, ok := b.Best(nil)
	if ok && bid != 0 {
		t.Fatalf("매수호가가 없는데 bid=%d ok=%v", bid, ok)
	}
}

func TestStaleAfterNoUpdate(t *testing.T) {
	b := NewBook(2)
	b.setLastUpdateForTest(1_000_000)
	if b.Stale(1_002_000, 3_000) {
		t.Error("2초 경과인데 3초 문턱에서 stale 로 봤다")
	}
	if !b.Stale(1_004_000, 3_000) {
		t.Error("4초 경과인데 stale 이 아니라고 봤다 — 오래된 호가로 재주문하게 된다")
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

프레임 형식(스냅샷 대 델타, `Seq` 단조성 처리)을 그대로 따른다. `Seq` 가 역행하면
전체 재구독을 요청해야 한다 — 원본이 그렇게 한다면 같게 한다.

- [ ] **Step 4: book.go 를 구현한다**

`Apply` 는 원본의 파생 규칙을 옮기고, `Best` 만 새로 만든다. `Best` 가 새 코드인
이유는 수집기에는 '우리 주문' 개념이 없었기 때문이다.

```go
// Best 는 우리 주문을 뺀 최우선 호가를 틱으로 돌려준다.
//
// 우리 주문을 빼는 이유: 목표가를 우리 호가에 맞추면 자기 자신을 쫓는 순환이
// 생긴다. 군중을 따라가야 한다. exclude 는 틱→우리 수량이고, 그만큼 뺀 뒤에도
// 수량이 남은 층만 유효한 호가로 본다.
func (b *Book) Best(exclude map[int64]int64) (bidTick, askTick int64, ok bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	bidTick, askTick = 0, 0
	haveBid, haveAsk := false, false
	for tick, qty := range b.bids {
		if qty-exclude[tick] <= 0 {
			continue
		}
		if !haveBid || tick > bidTick {
			bidTick, haveBid = tick, true
		}
	}
	for tick, qty := range b.asks {
		if qty-exclude[tick] <= 0 {
			continue
		}
		if !haveAsk || tick < askTick {
			askTick, haveAsk = tick, true
		}
	}
	return bidTick, askTick, haveBid || haveAsk
}

// Stale 은 마지막 갱신 후 afterMS 를 넘었는지 본다. 넘으면 호출자는 신규 주문을
// 멈추고 기존 주문을 취소해야 한다 — 오래된 호가를 보고 재주문하는 것이 최악이다.
func (b *Book) Stale(nowMS, afterMS int64) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return nowMS-b.lastUpdateMS > afterMS
}
```

`setForTest`/`setLastUpdateForTest` 는 같은 패키지의 테스트 전용 헬퍼로 둔다.

- [ ] **Step 5: 통과 확인과 돌연변이**

```bash
GOTOOLCHAIN=local go test -race ./internal/predictfun/ws/ -v
```

`Best` 의 `qty-exclude[tick] <= 0` 을 `qty <= 0` 으로 바꿔 `TestBestExcludesOwnOrders`
가 실패하는지 확인하고 되돌린다. 어느 돌연변이를 썼는지 리포트에 적는다.

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
  - `auth.Signer` — `crypto.Sign` 을 감싼 것. `auth.NewSigner(hexKey string) (*Signer, error)`, `(*Signer).Address() common.Address`, `(*Signer).SignHash(h []byte) ([]byte, error)`
  - `auth.Authenticator{Rest *rest.Client, Signer *Signer}`
  - `(*Authenticator).Token(ctx) (string, error)` — 캐시하고 만료 전에 갱신

- [ ] **Step 1: 의존성을 고정해 추가한다**

```bash
GOTOOLCHAIN=local go get github.com/ethereum/go-ethereum@v1.15.0
grep ethereum go.mod
GOTOOLCHAIN=local go build ./...
```
Expected: `github.com/ethereum/go-ethereum v1.15.0`, 빌드 성공.

**v1.15.0 에서 벗어나면 안 된다.** v1.17.5 는 `go >= 1.24.0` 을 요구해 로컬
툴체인으로 빌드가 깨진다. `go get -u` 를 쓰지 말 것.

- [ ] **Step 2: 실패 테스트를 쓴다**

`internal/predictfun/auth/auth_test.go`:

```go
package auth

import (
	"strings"
	"testing"
)

// 테스트 전용 키. 잔고가 없는 폐기 키이며 실거래에 쓰지 않는다.
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
func TestSignerNeverExposesKey(t *testing.T) {
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{s.String(), s.Address().Hex()} {
		if strings.Contains(strings.ToLower(out), testKey[:16]) {
			t.Fatalf("출력에 개인키가 들어 있다: %s", out)
		}
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
  - `order.Tick{V int64, Precision int}` — 정수 틱 가격과 그 마켓의 정밀도. `order.NewTick(v int64, precision int) Tick`, `(Tick).Float() float64`, `(Tick).Add(n int64) Tick`, `order.Ceiling(precision int) Tick` (0.5 미만 최대 틱)
  - `order.Order{Salt, Maker, Signer, Taker, TokenID, MakerAmount, TakerAmount, Expiration, Nonce, FeeRateBps *big.Int, Side, SignatureType uint8}`
  - `order.Domain{Name, Version string, ChainID int64, VerifyingContract string}`
  - `order.Hash(o Order, d Domain) ([]byte, error)`
  - `order.Sign(o Order, d Domain, s *auth.Signer) ([]byte, error)`
  - `order.AmountsForBuy(price Tick, usdtWei *big.Int) (makerAmount, takerAmount *big.Int)`

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

// BSC USDT 는 18 decimals 다. 6 으로 착각하면 10^12 배 주문이 나간다.
func TestAmountsForBuyUses18Decimals(t *testing.T) {
	// 1 USDT = 10^18 wei 를 0.49 에 산다 → 주식 수 = 1/0.49
	one := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	maker, taker := AmountsForBuy(NewTick(49, 2), one)
	if maker.Cmp(one) != 0 {
		t.Fatalf("makerAmount %s, 기대 %s", maker, one)
	}
	// 가격 = maker/taker 여야 하므로 taker = maker/0.49 = maker*100/49
	want := new(big.Int).Div(new(big.Int).Mul(one, big.NewInt(100)), big.NewInt(49))
	if taker.Cmp(want) != 0 {
		t.Fatalf("takerAmount %s, 기대 %s", taker, want)
	}
}

// 주식 수는 내림해야 한다. 올림하면 같은 USDT 로 더 많은 주식을 요구하게 되어
// 실효 가격이 목표가보다 낮아지고, 거래소가 거부하거나 체결이 안 된다.
func TestAmountsForBuyRoundsDownShares(t *testing.T) {
	// 3 wei 를 0.49 에 산다 → 3/0.49 = 6.12... → 내림 6
	maker, taker := AmountsForBuy(NewTick(49, 2), big.NewInt(3))
	if maker.Cmp(big.NewInt(3)) != 0 {
		t.Fatalf("makerAmount %s, 기대 3", maker)
	}
	if taker.Cmp(big.NewInt(6)) != 0 {
		t.Fatalf("takerAmount %s, 기대 6 (내림) — 올림하면 7 이 된다", taker)
	}
	// 실효 가격이 목표가 이상이어야 한다: maker/taker >= 0.49
	if new(big.Rat).SetFrac(maker, taker).Cmp(big.NewRat(49, 100)) < 0 {
		t.Errorf("실효 가격이 목표가보다 낮다: %s/%s", maker, taker)
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
`가격 = makerAmount / takerAmount` 다. 주식 수는 **내림**한다 — 올림하면 지불액이
한도를 넘는다.

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

- [ ] **Step 5: 통과 확인과 커밋**

```bash
GOTOOLCHAIN=local go test -race ./internal/predictfun/order/ -v
GOTOOLCHAIN=local go vet ./... && gofmt -l .
git add internal/predictfun/order/
git commit -m "predictfun/order — Order 값 타입, 틱 정규화, EIP-712 해싱"
```

---

### Task 9: G3 — TS SDK 서명 동등성 게이트

**여기서 실패하면 Task 8 로 돌아간다.** 서명이 어긋나면 거래소가 거부하거나, 더
나쁘게는 의도와 다른 주문이 체결된다.

**Files:**
- Create: `tools/export_sdk_vectors.ts`, `tools/package.json`, `testdata/sdk_signatures.json`, `cmd/g3check/main.go`
- Create: `docs/results/g3-signature.log`

**Interfaces:**
- Consumes: `order.Hash`, `order.Sign`, `auth.NewSigner`
- Produces: `testdata/sdk_signatures.json` — 한 줄이 아니라 배열. 각 원소는
  `{"label", "domain":{...}, "order":{...}, "digest":"0x...", "signature":"0x..."}`

- [ ] **Step 1: SDK 로 골든 벡터를 만든다**

`tools/package.json`:

```json
{
  "name": "gld91-sdk-vectors",
  "private": true,
  "type": "module",
  "dependencies": {
    "@predictdotfun/sdk": "1.3.8",
    "ethers": "^6.13.0"
  }
}
```

`tools/export_sdk_vectors.ts` 는 알려진 입력 여러 벌에 대해 SDK 가 계산하는
EIP-712 다이제스트와 서명을 뽑는다. 최소 다음 경우를 넣는다:

1. 정밀도 2, 가격 0.49, 1 USDT, CTF_EXCHANGE
2. 정밀도 3, 가격 0.499, 1 USDT, CTF_EXCHANGE
3. 같은 주문, `NEG_RISK_CTF_EXCHANGE` — verifyingContract 가 해시를 바꾸는지 고정
4. 큰 금액 (1000 USDT) — 18 decimals 자릿수 회귀
5. `salt` 가 다른 두 주문 — salt 가 해시에 들어가는지 고정

서명 키는 테스트 전용 폐기 키
`4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3` 를 쓴다.
**실지갑 키를 쓰지 말 것** — 골든 파일은 커밋된다.

```bash
cd tools && npm install --no-audit --no-fund 2>&1 | tail -2
npx tsx export_sdk_vectors.ts > ../testdata/sdk_signatures.json
cd .. && python3 -c "
import json; d=json.load(open('testdata/sdk_signatures.json'))
print(f'벡터 {len(d)}개'); [print(' ', v['label'], v['digest'][:18]) for v in d]"
```
Expected: 5개, 다이제스트가 서로 다름(3번과 1번이 특히)

- [ ] **Step 2: G3 러너를 쓴다**

`cmd/g3check/main.go` 는 골든을 읽어 각 벡터에 대해 Go 의 `order.Hash` 와
`order.Sign` 을 돌리고 대조한다. 판정 규칙:

- 다이제스트는 **완전 일치**여야 한다. 1비트라도 다르면 실패.
- 서명도 완전 일치여야 한다 — secp256k1 서명은 결정적(RFC 6979)이므로 같은 키·같은 해시면 같은 서명이 나온다.
- 벡터를 0개 비교하고 통과하지 않는다.
- 다이제스트가 전부 서로 같으면 실패한다 — 입력이 해시에 안 들어간 것이다.

```go
if compared == 0 {
    return fmt.Errorf("대조한 벡터가 없다")
}
if len(distinct) < 2 {
    return fmt.Errorf("다이제스트가 %d종뿐이다 — 입력이 해시에 반영되지 않는다", len(distinct))
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
git add tools/package.json tools/export_sdk_vectors.ts testdata/sdk_signatures.json \
        cmd/g3check/ docs/results/g3-signature.log
git status --short   # tools/node_modules 가 섞이면 안 된다
git commit -m "G3 서명 동등성 게이트 — TS SDK 골든 벡터와 다이제스트·서명 대조"
```

`tools/node_modules` 를 `.gitignore` 에 넣는다.

---

### Task 10: cmd/probe — testnet 왕복과 실측 확인

**이 태스크는 지갑에 가스와 잔고가 필요하다.** 준비가 안 됐으면 여기서 멈추고
보고한다 — Task 1~9 만으로 G3 까지는 끝난 상태다.

**Files:**
- Create: `cmd/probe/main.go`, `docs/results/p4-findings.md`

**Interfaces:**
- Consumes: `rest.Client`, `auth.Authenticator`, `order.Sign`, `ws.Client`, `ws.Book`
- Produces: `docs/results/p4-findings.md` — 계획 서두의 실측 4건에 대한 답

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
승인 대상 주소가 CTF 인지 NEG_RISK 인지는 Step 5 에서 확정되므로, 먼저 마켓
메타데이터로 어느 Exchange 인지 확인한 뒤 그 주소에만 승인한다.

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
