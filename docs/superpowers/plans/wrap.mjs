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
