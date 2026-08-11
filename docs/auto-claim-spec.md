# Auto-Claim 구현 명세

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
