import { keccak256, concat, hexlify, AbiCoder, TypedDataEncoder } from "ethers";

function hashKernelMessage(messageHash) {
  const codec = new AbiCoder();
  const value = new TextEncoder().encode("Kernel(bytes32 hash)");
  return keccak256(codec.encode(["bytes32","bytes32"], [keccak256(hexlify(value)), messageHash]));
}
function eip712WrapHash(messageHash, domain) {
  const domainSeparator = TypedDataEncoder.hashDomain(domain);
  return keccak256(concat(["0x1901", domainSeparator, hashKernelMessage(messageHash)]));
}

const digest = "0x" + Array.from({length:32},(_,i)=>i.toString(16).padStart(2,"0")).join("");
for (const [chainId, acct] of [[56,"0x1111111111111111111111111111111111111111"],
                                [56,"0x2222222222222222222222222222222222222222"],
                                [97,"0x1111111111111111111111111111111111111111"]]) {
  const d = { name:"Kernel", version:"0.3.1", chainId, verifyingContract: acct };
  console.log(`chain=${chainId} acct=${acct.slice(0,10)} kernelDigest=${eip712WrapHash(digest, d)}`);
}
console.log("orderDigest=" + digest);

// 이 스크립트는 Task 8 의 order.KernelDigest 가 SDK 의 eip712WrapHash 와
// 같은 값을 내는지 확인하는 대조용이다. 2026-08-09 실행 결과 세 벡터 비트 일치:
//   chain=56 acct=0x11111111  0xaf803f2abd4c257d6146ea9dfab747be742c77a531cb40a535b09aaf83d3b4eb
//   chain=56 acct=0x22222222  0xb6c89ab506db96f8b16813a1a302f314c012b47f8271279114eba9d7bc86d3c6
//   chain=97 acct=0x11111111  0xca19f3573eaeb97a07416265492f193b30de3246fef541ecf56d025f147a4c94
// 실행: cd tools && npm i ethers && node sdk_kernel_digest_check.mjs
