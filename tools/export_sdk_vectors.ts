// export_sdk_vectors.ts — G3 게이트용 골든 벡터 생성기.
//
// @predictdotfun/sdk 를 실제로 호출해 Order EIP-712 다이제스트와 서명을 뽑는다.
// 네트워크에 붙지 않는다: OrderBuilder.make() 는 signer + predictAccount 를 함께
// 넘기면 ecdsaValidatorStorage 를 온체인 조회하므로, 여기서는 OrderBuilder 를
// `new` 로 직접 만들어 그 경로를 피한다 (팀리드가 소스 대조로 확인한 우회로).
//
// 상수(주소·프로토콜 이름·체인 도메인)는 전부 SDK 에서 임포트한다 — 스펙 §6 의
// 표를 손으로 베끼면 표가 틀려도 골든이 같은 틀린 값을 담아 G3 가 통과해버린다.
import { Wallet } from "ethers";
import {
  OrderBuilder,
  AddressesByChainId,
  KernelDomainByChainId,
  PROTOCOL_NAME,
  PROTOCOL_VERSION,
} from "@predictdotfun/sdk";
// eip712WrapHash 는 공개 API(index.d.ts) 밖이지만 패키지에 "exports" 제한이
// 없어 딥 임포트가 된다. signPredictAccountMessage 내부에서 쓰는 바로 그
// 함수라, kernelDigest 를 별도로 기록하려면 이걸 직접 불러야 한다.
import { eip712WrapHash } from "@predictdotfun/sdk/dist/internal/Utils.js";

// go-ethereum 문서에 실린 공개 테스트 키. 주소 0x27000F84214f79B0600aa86841958b13ac98242a.
// 골든 파일이 커밋되므로 실지갑 키는 어떤 경우에도 쓰지 않는다.
const TEST_PRIV_KEY = "4c0883a69102937d6231471b5dbb6204fe512961708279f2e3e8a5d4b8e3e3e3";
const wallet = new Wallet(TEST_PRIV_KEY); // provider 없음 — signTypedData/signMessage 는 RPC 를 타지 않는다

// 실계정 주소를 golden 에 박지 않는다 — 자리표시자만 쓴다.
const PA_A = "0x1111111111111111111111111111111111111111";
const PA_B = "0x2222222222222222222222222222222222222222";

const noopLogger = { debug() {}, info() {}, warn() {}, error() {} };

type OrderFields = {
  salt: string;
  maker: string;
  signer: string;
  taker: string;
  tokenId: string;
  makerAmount: string;
  takerAmount: string;
  expiration: string;
  nonce: string;
  feeRateBps: string;
  side: number;
  signatureType: number;
};

// weiPerShare 는 Go 쪽 order.Tick.WeiPerShare() 와 같은 공식이다: V × 10^(18−precision).
function weiPerShare(v: bigint, precision: number): bigint {
  return v * 10n ** BigInt(18 - precision);
}

const ZERO_ADDRESS = "0x0000000000000000000000000000000000000000";
const TOKEN_ID = "123456789012345678901234567890123456789012345678901234567890";

function makeOrder(fields: Pick<OrderFields, "salt" | "maker" | "signer" | "makerAmount" | "takerAmount">): OrderFields {
  return {
    taker: ZERO_ADDRESS,
    tokenId: TOKEN_ID,
    expiration: "0",
    nonce: "0",
    feeRateBps: "0",
    side: 0, // BUY
    signatureType: 0, // EOA (predictAccount 도 EIP-1271 을 EOA 타입으로 함께 지원한다 — 스펙 §6)
    ...fields,
  };
}

// newBuilder 는 OrderBuilder.make() 를 거치지 않고 생성자를 직접 부른다.
// signer 와 predictAccount 를 함께 넘기면 make() 가 ecdsaValidatorStorage 를
// 온체인 조회하기 때문이다 — buildTypedData/buildTypedDataHash/signTypedDataOrder
// 는 이 필드들만 쓰고 this.contracts 는 건드리지 않으므로 undefined 로 둬도 된다.
function newBuilder(chainId: number, predictAccount?: string): any {
  const addresses = (AddressesByChainId as any)[chainId];
  return new (OrderBuilder as any)(
    chainId,
    10n ** 18n, // precision 스케일 — getLimitOrderAmounts 를 안 쓰므로 값 자체는 안 쓰인다
    addresses,
    () => "0", // salt 생성기 — 매 벡터마다 salt 를 명시하므로 안 쓰인다
    noopLogger,
    wallet,
    predictAccount,
    undefined,
  );
}

type Vector = {
  label: string;
  kind: "eoa" | "predictAccount";
  domain: { name: string; version: string; chainId: number; verifyingContract: string };
  order: OrderFields;
  orderDigest: string;
  predictAccount: string | null;
  kernelDigest: string | null;
  signature: string;
};

async function eoaVector(
  label: string,
  chainId: number,
  exchangeKey: "CTF_EXCHANGE" | "NEG_RISK_CTF_EXCHANGE",
  order: OrderFields,
): Promise<Vector> {
  const builder = newBuilder(chainId);
  const verifyingContract = (AddressesByChainId as any)[chainId][exchangeKey];
  const isNegRisk = exchangeKey === "NEG_RISK_CTF_EXCHANGE";
  const typedData = builder.buildTypedData(order, { isNegRisk, isYieldBearing: false });
  const orderDigest: string = builder.buildTypedDataHash(typedData);
  const signed = await builder.signTypedDataOrder(typedData);
  return {
    label,
    kind: "eoa",
    domain: { name: PROTOCOL_NAME, version: PROTOCOL_VERSION, chainId, verifyingContract },
    order,
    orderDigest,
    predictAccount: null,
    kernelDigest: null,
    signature: signed.signature,
  };
}

async function predictAccountVector(
  label: string,
  chainId: number,
  predictAccount: string,
  order: OrderFields,
): Promise<Vector> {
  const builder = newBuilder(chainId, predictAccount);
  const verifyingContract = (AddressesByChainId as any)[chainId].CTF_EXCHANGE;
  const typedData = builder.buildTypedData(order, { isNegRisk: false, isYieldBearing: false });
  const orderDigest: string = builder.buildTypedDataHash(typedData);
  // signPredictAccountMessage 내부와 정확히 같은 호출 — 중간값을 별도로 기록하려고 여기서도 부른다.
  // name/version 은 하드코딩하지 않는다 — KernelDomainByChainId 를 그대로 써야
  // SDK 가 Kernel 버전을 올렸을 때 이 필드도 같이 움직인다. 하드코딩해두면
  // kernelDigest 는 옛 값에 고정된 채 "일치"해버리고, 실패가 signature 에서만
  // 드러나 어느 단계가 범인인지 짚어주는 진단 기능을 잃는다.
  const kernelDigest: string = eip712WrapHash(orderDigest, {
    ...(KernelDomainByChainId as any)[chainId],
    verifyingContract: predictAccount,
  });
  const signed = await builder.signTypedDataOrder(typedData);
  return {
    label,
    kind: "predictAccount",
    domain: { name: PROTOCOL_NAME, version: PROTOCOL_VERSION, chainId, verifyingContract },
    order,
    orderDigest,
    predictAccount,
    kernelDigest,
    signature: signed.signature,
  };
}

async function main() {
  const WALLET_ADDR = wallet.address;
  const vectors: Vector[] = [];

  // --- EOA 경로 (5개) ---

  // 1. 정밀도 2, 가격 0.49, 2주, CTF_EXCHANGE
  const order1 = makeOrder({
    salt: "10001",
    maker: WALLET_ADDR,
    signer: WALLET_ADDR,
    makerAmount: (weiPerShare(49n, 2) * 2n).toString(), // 0.49 * 2 = 0.98 USDT wei
    takerAmount: (2n * 10n ** 18n).toString(),
  });
  vectors.push(await eoaVector("eoa/ctf/prec2", 56, "CTF_EXCHANGE", order1));

  // 2. 정밀도 3, 가격 0.499, 2주, CTF_EXCHANGE
  const order2 = makeOrder({
    salt: "10002",
    maker: WALLET_ADDR,
    signer: WALLET_ADDR,
    makerAmount: (weiPerShare(499n, 3) * 2n).toString(), // 0.499 * 2 = 0.998 USDT wei
    takerAmount: (2n * 10n ** 18n).toString(),
  });
  vectors.push(await eoaVector("eoa/ctf/prec3", 56, "CTF_EXCHANGE", order2));

  // 3. order1 과 완전히 같은 주문, NEG_RISK_CTF_EXCHANGE — verifyingContract 만 바뀐다
  vectors.push(await eoaVector("eoa/negrisk/prec2", 56, "NEG_RISK_CTF_EXCHANGE", order1));

  // 4. 큰 금액 (1000주) — 18 decimals 절단 회귀
  const order4 = makeOrder({
    salt: "10004",
    maker: WALLET_ADDR,
    signer: WALLET_ADDR,
    makerAmount: (weiPerShare(49n, 2) * 1000n).toString(), // 0.49 * 1000 = 490 USDT wei
    takerAmount: (1000n * 10n ** 18n).toString(),
  });
  vectors.push(await eoaVector("eoa/ctf/large1000", 56, "CTF_EXCHANGE", order4));

  // 5. order1 과 salt 만 다른 주문 — salt 가 해시에 들어가는지 고정
  const order5 = { ...order1, salt: "99999" };
  vectors.push(await eoaVector("eoa/ctf/altsalt", 56, "CTF_EXCHANGE", order5));

  // --- predictAccount 경로 (4개, 실제 운용 경로) ---

  // 6. chainId 56, predictAccount A
  const orderPA = makeOrder({
    salt: "20001",
    maker: PA_A,
    signer: PA_A,
    makerAmount: (weiPerShare(49n, 2) * 2n).toString(),
    takerAmount: (2n * 10n ** 18n).toString(),
  });
  vectors.push(await predictAccountVector("predictAccount/chain56/acctA", 56, PA_A, orderPA));

  // 7. 같은 주문, predictAccount B — 계정이 다이제스트를 바꾸는지 고정
  const orderPA_B = { ...orderPA, maker: PA_B, signer: PA_B };
  vectors.push(await predictAccountVector("predictAccount/chain56/acctB", 56, PA_B, orderPA_B));

  // 8. 같은 주문·계정, chainId 97 — 체인이 다이제스트를 바꾸는지 고정
  vectors.push(await predictAccountVector("predictAccount/chain97/acctA", 97, PA_A, orderPA));

  // 9. 최종 86바이트 봉투 형식 고정 — 다른 금액의 주문으로 kernelDigest 도 새로 확보한다
  const orderPA_large = makeOrder({
    salt: "20009",
    maker: PA_A,
    signer: PA_A,
    makerAmount: (weiPerShare(49n, 2) * 1000n).toString(),
    takerAmount: (1000n * 10n ** 18n).toString(),
  });
  vectors.push(await predictAccountVector("predictAccount/chain56/acctA-envelope", 56, PA_A, orderPA_large));

  console.log(JSON.stringify(vectors, null, 2));
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
