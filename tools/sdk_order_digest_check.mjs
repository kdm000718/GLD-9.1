import { TypedDataEncoder } from "ethers";

const PROTOCOL_NAME = "predict.fun CTF Exchange";
const PROTOCOL_VERSION = "1";
const ORDER_STRUCTURE = [
  { name: "salt", type: "uint256" }, { name: "maker", type: "address" },
  { name: "signer", type: "address" }, { name: "taker", type: "address" },
  { name: "tokenId", type: "uint256" }, { name: "makerAmount", type: "uint256" },
  { name: "takerAmount", type: "uint256" }, { name: "expiration", type: "uint256" },
  { name: "nonce", type: "uint256" }, { name: "feeRateBps", type: "uint256" },
  { name: "side", type: "uint8" }, { name: "signatureType", type: "uint8" },
];

const ACCT = "0x1111111111111111111111111111111111111111";
const order = {
  salt: "12345", maker: ACCT, signer: ACCT,
  taker: "0x0000000000000000000000000000000000000000",
  tokenId: "88888888888888888888888888888888888888",
  makerAmount: "980000000000000000",   // 0.98 USDT
  takerAmount: "2000000000000000000",  // 2 주
  expiration: "0", nonce: "0", feeRateBps: "20",
  side: 0, signatureType: 0,
};

for (const [label, verifying, chainId] of [
  ["CTF/56",      "0x8BC070BEdAB741406F4B1Eb65A72bee27894B689", 56],
  ["NEG_RISK/56", "0x365fb81bd4A24D6303cd2F19c349dE6894D8d58A", 56],
  ["CTF/97",      "0x2A6413639BD3d73a20ed8C95F634Ce198ABbd2d7", 97],
]) {
  const domain = { name: PROTOCOL_NAME, version: PROTOCOL_VERSION, chainId, verifyingContract: verifying };
  const h = TypedDataEncoder.hash(domain, { Order: ORDER_STRUCTURE }, order);
  console.log(`${label.padEnd(12)} orderDigest=${h}`);
}
