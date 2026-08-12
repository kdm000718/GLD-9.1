# Auto-Claim 구현 명세

> **2026-08-12 구현 완료.** 아래는 구현 **전에** 쓴 계획이고 두 곳이 틀렸다.
> 골든 벡터가 그것을 잡았다 — 문서 끝 「구현 결과」를 먼저 읽을 것.
> 코드는 `internal/claim`, 바이너리는 `cmd/gld91-claim`, 게이트는 `make claimcheck`.

정산이 끝난 포지션을 봇이 스스로 회수한다. 2026-08-11~12 실측으로 필요한 값을
전부 확보했고, 남은 것은 조립뿐이다.

**가스는 필요 없다.** predict.fun 이 쓰는 ZeroDev 페이마스터가 후원한다
(`maxFeePerGas: 0x0` 로 실제 전송된 것을 확인했다).

## 확보된 값

```
ZERODEV_RPC   ~/.config/predictfun/env 에 있음
              https://rpc.zerodev.app/api/v3/<프로젝트ID>/chain/56?provider=ULTRA_RELAY
              인증 헤더 없음 — URL 의 프로젝트 ID 가 곧 인증
EntryPoint    0x0000000071727De22E5E9d8BAf0edAc6f37da032   (v0.7)
CTF           0x22da1810b194ca018378464a58f6ac2b10c9d244
collateral    0x55d398326f99059fF775485246999027B3197955   (USDT, 18 decimals)
sender        PREDICT_ACCOUNT (Kernel 스마트계정)
ECDSA_VALIDATOR 0x845ADb2C711129d4f3966735eD98a9F09fC4cE57
```

## 실물 대조 기준 — 이것이 이 작업의 핵심이다

`C:\Users\kdm00\Downloads\Telegram Desktop\predict.fun.har` 에 **성공한 claim 의
UserOperation 전문**이 들어 있다. 우리가 조립한 것과 **바이트 단위로 대조**할 수
있는 유일한 기준이다. 이 파일을 지우면 안 된다.

작업 순서를 이렇게 잡는다:

1. HAR 의 UserOperation 을 파싱해 골든 벡터로 박는다
2. 같은 입력(같은 회차·conditionId·nonce)으로 우리 코드가 **같은 바이트**를
   만드는지 시험한다
3. 그것이 통과한 뒤에야 실제 전송을 붙인다

**추측으로 만든 UserOperation 에 유효한 서명을 붙여 보내면 안 된다.** 이 저장소가
2026-08-11 하루에 결함 9개를 낸 방식이 전부 "확인하지 않고 맞다고 가정한 것"
이었다.

## 조립 순서

### 1. 정산된 포지션 감지

`GET /v1/positions` 로 보유분을 읽고, 그 회차가 RESOLVED 인지 본다. 모니터가 이미
정산을 관측하므로 그쪽 상태를 쓰거나, 봇에서 `categories?status=RESOLVED` 를
직접 본다.

### 2. conditionId 획득 — **유일하게 미확정인 값**

포지션 토큰 ID 에서 역산은 불가능하다(keccak 단방향). 후보:

- CTF 컨트랙트의 `ConditionResolution` 이벤트를 정산 시점에 조회
- 마켓 메타데이터에 실려 있는지 확인
- HAR 의 실제 호출에서 어떤 경로로 얻었는지 역추적

실측 예: `0xc97c998bfc9d621a605d49b67240702317476537429236a13b1180a5e5c71cb5`

### 3. redeemPositions calldata

```
redeemPositions(address collateralToken, bytes32 parentCollectionId,
                bytes32 conditionId, uint256[] indexSets)
셀렉터 0x01b7037c
parentCollectionId = 0x00…00
indexSets = [1] 또는 [2]  (Up/Down. 실측에서는 두 건을 한 UserOp 에 묶었다)
```

### 4. Kernel execute 로 감싸기

```
셀렉터 0xe9ae5c53  — execute(bytes32 execMode, bytes executionCalldata)
```

실측 callData 가 `0xe9ae5c5301000…` 로 시작한다. execMode 첫 바이트 `0x01` 은
배치 실행이다 — redeem 두 건을 한 번에 보냈다. HAR 의 callData 를 디코드해
정확한 인코딩을 확인할 것.

### 5. UserOperation 조립

**nonce 가 함정이다.** 실측값:

```
0x845adb2c711129d4f3966735ed98a9f09fc4ce5727110000000000000005
  └─ ECDSA_VALIDATOR 주소 ─────────────────┘└타입┘└─ 시퀀스 ─┘
```

Kernel v3 는 **검증자 주소를 nonce key 에 인코딩**한다. 그냥 0 을 넣으면 안 된다.
`EntryPoint.getNonce(sender, key)` 로 받되 key 를 이 규칙대로 만들어야 한다.

나머지 가스 필드는 3단계 RPC 가 채워 준다.

### 6. 서명

EntryPoint v0.7 의 userOpHash 를 계산하고, **Kernel 봉투**로 감싼다 —
`order.SignForPredictAccount` 를 그대로 쓸 수 있다(오늘 인증에서 검증됨).
평문 65바이트 서명은 거부된다.

### 7. RPC 4단계

```
1. zd_getUserOperationGasPrice        가스 가격
2. zd_sponsorUserOperation            페이마스터 후원 — 이것이 가스를 0 으로 만든다
3. eth_sendUserOperation              전송 (userOpHash 반환)
4. eth_getUserOperationReceipt        폴링해 성공 확인
```

2번을 빠뜨리면 우리가 가스를 내야 하고, 잔고가 0 이라 실패한다.

## 실패 방향

이 저장소의 규칙을 그대로 따른다 — **애매하면 하지 않는다.**

- conditionId 를 확신할 수 없으면 redeem 하지 않는다
- 조립한 UserOp 가 골든 벡터와 다르면 보내지 않는다
- 전송 결과를 확인하지 못하면 **성공으로 치지 않는다.** 실패로 치고 다시
  시도하는 쪽이 안전하다 — redeem 은 멱등이다(이미 회수된 포지션은 0 을 준다)

## 회수 뒤

- 원장에 정산 행을 적는다 (`ledger.Settlement`, 지금은 호출부가 없다)
- 리베이트 주식도 함께 회수된다 — `reconcile` 의 리베이트 여유(0.5%)를
  없앨 수 있는지 그때 재검토한다

## 순서에 대한 경고

Auto-Claim 은 **자금 회수를 빠르게 할 뿐**이다. 재무장 전에 먼저 고쳐야 하는 것:

1. **노출 2배** — 회차당 상한(자본의 4.55%)을 위반한다. 취소 확인 뒤 도착하는
   체결이 이중 계상된다. 2026-08-11 실측: 4회차 중 2회차가 상한의 약 1.9배
2. **2% 테이커 수수료** — 체결 일부에 명목의 2% 가 붙는다. 관통 방지가 뚫린
   것이라면 건당 엣지 5%p 에서 2%를 잃는다

둘 다 손익에 직접 영향이 있고, Auto-Claim 은 그렇지 않다.

---

## 구현 결과 (2026-08-12)

작업 순서는 명세대로 지켰다: HAR 파싱 → 골든 벡터 고정 → 바이트 대조 통과 →
그 뒤에 전송. **전송 코드는 대조가 통과한 뒤에 붙였다.**

### 골든 벡터

`internal/claim/testdata/golden_userop.json` 이 그것이다. 2026-08-11T18:05:32Z
에 성공한 claim 의 UserOperation 전문(BSC tx `0x83a80081…`, receipt
`success: true`). `internal/claim/testdata/claimable_response.json` 은 같은
HAR 의 실제 GraphQL 응답이다. **HAR 자체는 재취득 불가이므로 지우지 말 것.**

`make claimcheck` 가 네 가지를 대조한다. 넷 다 통과한다.

| 대조 | 결과 |
|---|---|
| callData (execute 배치 3층 인코딩) | 832바이트 전부 일치 |
| nonce key | `0x0000845adb…ce572711` 일치 |
| userOpHash (EntryPoint v0.7 패킹) | `0x6e04a546…dca2` 일치 |
| 서명 형식 | 65바이트, 복구 EOA = `0x701f6b98…` 일치 |

음성 대조도 했다 — callData 3니블을 바꾸면 게이트가 잡는다.

### 명세가 틀렸던 곳 둘

**§6 서명.** 명세는 "Kernel 봉투로 감싼다, `order.SignForPredictAccount` 를
그대로 쓸 수 있다, 평문 65바이트는 거부된다"고 적었다. **반대였다.** 실물이
보낸 것은 봉투 없는 65바이트이고, EIP-191 개인서명 해시로 복구하면 우리 키의
EOA 가 나온다. HAR 에도 `personal_sign(userOpHash)` 호출이 그대로 남아 있다.

이유는 nonce key 에 있다: 앞 2바이트가 검증 모드 `0x00` + 검증 타입 `0x00`
(루트)이라 Kernel 이 서명을 봉투로 해석하지 않고 루트 검증자에게 원문 그대로
넘긴다. 주문 서명(EIP-712 + Kernel 봉투)과는 다른 경로다.

명세대로 만들었다면 보내는 것마다 거부됐을 것이고, 그 거부는 "키가 틀렸다"와
구분되지 않았을 것이다. **이 한 건이 골든 벡터 우선 순서의 값을 다 갚았다.**

**§2 conditionId — "유일하게 미확정인 값" 이 아니었다.** HAR 이 답을 갖고
있었다. predict.fun 웹은 GraphQL `market.conditionId` 를 그대로 쓴다
(`GetAccountClaimablePositions`, 무인증·주소만으로 조회). 온체인 이벤트 조회도
메타데이터 역추적도 필요 없었다. 명세가 "HAR 의 실제 호출에서 어떤 경로로
얻었는지 역추적" 이라고 적어 둔 후보가 맞았다.

부수 효과 하나: 이 조회는 REST 키를 쓰지 않으므로 **레이트리밋 240 req/min
예산을 먹지 않는다.** 봇과 키를 공유해도 재호가가 밀리지 않는다.
(REST `/v1/positions` 는 애초에 conditionId 를 주지 않아 쓸 수 없다.)

### 확인만 하고 넘어간 값 하나

nonce key 끝 2바이트 `0x2711` 이 무엇에서 왔는지는 **모른다.** 지어내지 않고
관측된 그대로 쓴다 — 그 key 가 유효하다는 것은 실물이 증명했고,
`EntryPoint.getNonce` 가 같은 key 의 다음 시퀀스를 준다(2026-08-12 온체인
확인: 골든이 시퀀스 5, 지금 6). key 마다 시퀀스가 독립이므로 다른 값을 쓰면
검증되지 않은 경로로 들어간다.

### 하지 않는 것

- **negRisk·yieldBearing 시장은 회수하지 않는다.** 대상 컨트랙트도 calldata 도
  다른데 그 경로의 실물 기준이 없다. 건너뛰고 사유를 찍는다.
- 조립한 userOpHash 와 번들러가 돌려준 것이 다르면 성공으로 치지 않는다.
- 서명에서 복구한 EOA 가 키의 EOA 와 다르면 보내지 않는다.
- 영수증을 못 보면 성공으로 치지 않는다(멱등이므로 재시도가 안전한 쪽이다).
- 기동 때 `internal/kernel` 로 키가 그 계정의 등록 서명자인지 체인에서
  확인한다. 아니면 아무것도 하지 않는다.

### 기본값은 보내지 않는다

`CLAIM_ARM=I_UNDERSTAND_THE_RISK` 일 때만 전송한다. 그 전에는 조립·서명까지
하고 무엇을 보낼지만 찍는다 — `LIVE_ARM` 과 같은 규약이다.

### 원장

회수에 성공하면 `ledger.RecordSettlement` 로 정산 행을 적는다(`-ledger` 로
경로를 주면). 한 시장에서 이긴 주식과 진 주식을 함께 들고 있을 수 있으므로,
이긴 것이 있으면 그 주식 수로 `Won=true` 를 적는다 — `SettlementProceeds` 가
이긴 주식만 주당 $1 로 세기 때문이다. **원장 기록이 실패해도 회수는 뒤집지
않는다.** 뒤집으면 다음 주기가 이미 회수된 것을 다시 보낸다.

### 위 「순서에 대한 경고」는 그대로 유효하다

Auto-Claim 은 재무장의 전제가 아니다. 노출 2배와 2% 테이커 수수료가 먼저다.

> **2026-08-12 추가:** 그 둘은 고쳤다(커밋 `4c65970`, `8c645d1`). 지금 재무장을
> 막는 것은 다른 항목이다 — `docs/results/` 와 [[gld91-arming-decisions]] 를 볼 것.

---

## 봇 안에서 회차 종료마다 돈다 (2026-08-12)

`cmd/gld91` 이 **회차가 끝날 때마다** 회수 한 바퀴를 돌린다(`cmd/gld91/claim.go`).
`cmd/gld91-claim` 은 그대로 남아 있고, 손으로 돌려볼 때(`make claim`)와
봇 없이 회수할 때 쓴다.

### 배경에서 돈다

회수 한 건은 조회 → nonce → 후원 → 서명 → 전송 → 영수증까지 가고 영수증
폴링만 최대 90초다. 회차 종료 직후에 그것을 기다리면 **다음 회차의 첫 호가가
그만큼 늦는다.** 2026-08-12 전략 개정으로 봇은 회차 시작에 한 번 거는 것이
전부이므로(설계서 §1) 그 지연이 곧 큐 위치다.

그래서 `after` 는 고루틴을 띄우고 곧바로 돌아온다. 한 바퀴의 상한은 4분 —
회차 간격 5분보다 짧아야 멈춘 바퀴 하나가 회수를 영원히 막지 않는다.

### 겹치지 않는다

회수 자체는 멱등이지만 같은 계정에 UserOperation 두 개를 동시에 띄우면
**nonce 가 겹쳐 하나는 반드시 실패한다.** 앞 바퀴가 아직 돌고 있으면 이번
회차는 건너뛰고 사유를 찍는다. 다음 기회는 5분 뒤다.

### 게이트 셋

| 게이트 | 기본 | 없으면 |
|---|---|---|
| `-auto-claim` | `true` | 회수 경로를 아예 돌리지 않는다 |
| `ZERODEV_RPC` | 필수 | 켜지지 않는다. 사유를 기동 로그에 찍는다 |
| `CLAIM_ARM` | 미설정 | 조립·서명까지만 하고 **보내지 않는다** |

`CLAIM_ARM` 은 `LIVE_ARM` 과 **별개다.** 주문 전송은 새 위험을 만들지만 회수는
이미 정산된 포지션을 담보로 되돌릴 뿐이다. DRY-RUN 으로 도는 봇이 밀린 회수를
처리하는 것은 말이 되고, 그 반대(무장했으니 회수도 자동으로 켜짐)는 말이 안 된다.

`CLAIM_ARM` 없이도 조립까지는 매 회차 돈다 — `LIVE_ARM` 없이도 주문 서명을
하는 것과 같은 이유다. 경로가 실거래와 달라지면 DRY-RUN 이 아무것도 증명하지
못한다.

### 거래를 막지 않는다

회수 실패는 로그다. 무장 해제도, 회차 중단도 아니다. 한 번 실패해도 다음
회차가 다시 시도한다.

### 서버에 아직 없는 것

`~/.gld91/env` 에 **`ZERODEV_RPC` 가 없다**(2026-08-12 확인). 그대로 배포하면
Auto-Claim 은 켜지지 않고 기동 로그가 그 사실을 찍는다. 켜려면 로컬
`~/.config/predictfun/env` 의 그 줄을 서버 env(600, 디렉터리 700)에 추가해야
한다.
