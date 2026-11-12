// 0.45.0 (David 2026-11-07): the swap's funding BEEF — raw outside the token's topic, txid-only inside (skein 0.3.3's
// door admits a txid-only entry for a transaction the instance holds).
import { Beef, MerklePath, P2PKH, PrivateKey, Transaction, UnlockingScript, Utils } from "@bsv/sdk";
import { describe, expect, it } from "vitest";
import { PoolTemplate } from "../src/pool";
import { fundingBeef, submissionBeef, topicAdmitted, topicBeef } from "../src/market/swapAction";
import { submitToOverlay } from "../src/lp/close";

const ASSET = "ab".repeat(32);
const OTHER = "cd".repeat(32);
const k = PrivateKey.fromRandom();
const pay = new P2PKH().lock(k.toPublicKey().toAddress());
const poolScript = (asset: string) =>
  PoolTemplate.lockDeploy({ assetId: asset, lpFeeBps: 30n, validatorFeeBps: 5n, commissionBps: 10n }, { tokenReserve: 1000n, lpPubKey: k.toPublicKey().toString(), validatorPubKey: k.toPublicKey().toString(), validatorIdentity: k.toPublicKey().toString(), nonce: "00".repeat(32), lpSatsPkh: "66".repeat(20), lpTokenPkh: "77".repeat(20), affirmation: "" });

function tx(srcs: [Transaction | string, number][], outs: { lockingScript: ReturnType<typeof poolScript> | typeof pay; satoshis: number }[]): Transaction {
  const t = new Transaction();
  for (const [s, i] of srcs) {
    if (typeof s === "string") t.addInput({ sourceTXID: s, sourceOutputIndex: i, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
    else t.addInput({ sourceTransaction: s, sourceOutputIndex: i, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
  }
  for (const o of outs) t.addOutput(o);
  return t;
}
function entries(bytes: number[]) {
  const b = Beef.fromBinary(bytes);
  return { atomic: Utils.toHex(bytes.slice(4, 36).reverse()), txs: b.txs.map((t) => ({ txid: t.txid, txidOnly: t.isTxidOnly })) };
}

describe("fundingBeef (0.45.0)", () => {
  // root (mined elsewhere: txid only) → deploy (a pool of ASSET) → swap1 (pool + sats change) → the leg spends swap1's
  // pool output and its change; a plain sats transaction (outside the topic) funds it too; another token's swap is outside.
  const root = "11".repeat(32);
  const deploy = tx([[root, 0]], [{ lockingScript: poolScript(ASSET), satoshis: 1_000_000 }]);
  const swap1 = tx([[deploy, 0], [root, 1]], [{ lockingScript: poolScript(ASSET), satoshis: 1_000_100 }, { lockingScript: pay, satoshis: 5_000 }]);
  const plain = tx([[root, 2]], [{ lockingScript: pay, satoshis: 9_000 }]);
  const otherSwap = tx([[root, 3]], [{ lockingScript: poolScript(OTHER), satoshis: 1 }, { lockingScript: pay, satoshis: 7_000 }]);
  const wallet = new Beef();
  wallet.mergeTxidOnly(root);
  for (const t of [deploy, swap1, plain, otherSwap]) wallet.mergeRawTx(t.toBinary());

  it("a swap the page took in and the pool's transaction are txid-only; their ancestry goes; a plain transaction stays raw", () => {
    const pool = swap1.id("hex");
    const got = entries(fundingBeef(wallet, plain.id("hex"), [plain.id("hex"), swap1.id("hex")], { poolTxid: pool, admitted: topicAdmitted(ASSET, pool) }));
    expect(got.atomic).toBe(plain.id("hex"));
    const by = new Map(got.txs.map((t) => [t.txid, t.txidOnly]));
    expect(by.get(swap1.id("hex"))).toBe(true);
    expect(by.get(plain.id("hex"))).toBe(false);
    expect(by.get(root)).toBe(true);
    expect(by.has(deploy.id("hex"))).toBe(false); // only swap1 needed it
  });

  it("another token's swap is outside this topic: raw with its ancestry; the pool's transaction is added by txid when absent", () => {
    const pool = deploy.id("hex");
    const got = entries(fundingBeef(wallet, otherSwap.id("hex"), [otherSwap.id("hex")], { poolTxid: pool, admitted: topicAdmitted(ASSET, pool) }));
    const by = new Map(got.txs.map((t) => [t.txid, t.txidOnly]));
    expect(by.get(otherSwap.id("hex"))).toBe(false);
    expect(by.get(pool)).toBe(true);
    expect(by.get(root)).toBe(true);
  });

  it("no predicate: every source raw with its ancestry (0.44 shape)", () => {
    const got = entries(fundingBeef(wallet, plain.id("hex"), [plain.id("hex"), swap1.id("hex")]));
    expect(got.txs.filter((t) => !t.txidOnly).map((t) => t.txid).sort()).toEqual([deploy.id("hex"), swap1.id("hex"), plain.id("hex")].sort());
  });
});

// 0.51.1 (David 2026-11-11): every submission the client makes — raw outside the token's topic, txid-only inside;
// the subject always raw; a proven entry keeps its BUMP. Applied once, in `submitToOverlay`.
describe("topicBeef / submissionBeef (0.51.1)", () => {
  // a pool asset id that is not its own byte reverse: the topic names the token by its display txid.
  const A2 = Array.from({ length: 32 }, (_, n) => n.toString(16).padStart(2, "0")).join("");
  const root = "22".repeat(32);
  const minedParent = "33".repeat(32);
  const deploy = tx([[root, 0]], [{ lockingScript: poolScript(A2), satoshis: 1_000_000 }]);
  const swap1 = tx([[deploy, 0], [root, 1]], [{ lockingScript: poolScript(A2), satoshis: 1_000_100 }, { lockingScript: pay, satoshis: 5_000 }]);
  // swap2: the pool's next state and the bot's payout (output 1).
  const swap2 = tx([[swap1, 0], [swap1, 1]], [{ lockingScript: poolScript(A2), satoshis: 1_000_200 }, { lockingScript: pay, satoshis: 4_000 }]);
  // mined: a proven sats transaction (its BUMP); fund: plain sats outside the topic, unmined, spending it.
  const mined = tx([[minedParent, 0]], [{ lockingScript: pay, satoshis: 50_000 }]);
  const fund = tx([[mined, 0]], [{ lockingScript: pay, satoshis: 40_000 }]);
  // stage: the staging transaction, spending the swap's payout and the funding.
  const stage = tx([[swap2, 1], [fund, 0]], [{ lockingScript: pay, satoshis: 1 }, { lockingScript: pay, satoshis: 43_000 }]);
  // rebal: a subject carrying a pool output of this token (a deploy / rebalance), spending swap2's pool output.
  const rebal = tx([[swap2, 0], [fund, 0]], [{ lockingScript: poolScript(A2), satoshis: 1_000_200 }]);
  const bump = new MerklePath(900_000, [[{ offset: 0, hash: mined.id("hex"), txid: true }, { offset: 1, hash: "44".repeat(32) }]]);
  const wallet = new Beef();
  wallet.mergeTxidOnly(root);
  wallet.mergeRawTx(mined.toBinary(), wallet.mergeBump(bump));
  for (const t of [deploy, swap1, swap2, fund, stage, rebal]) wallet.mergeRawTx(t.toBinary());
  const ids = { root, deploy: deploy.id("hex"), swap1: swap1.id("hex"), swap2: swap2.id("hex"), mined: mined.id("hex"), fund: fund.id("hex"), stage: stage.id("hex"), rebal: rebal.id("hex") };
  const TOPIC = `tm_color_${Utils.toHex(Utils.toArray(A2, "hex").reverse())}_0`;
  const atomicOf = (txid: string) => Beef.fromBinary(wallet.toBinaryAtomic(txid)).toBinaryAtomic(txid);

  it("(a) a staging transaction spending a swap payout: the staging transaction raw, the swap txid-only, the pool's chain behind it absent", () => {
    const got = entries(submissionBeef(TOPIC, atomicOf(ids.stage)));
    expect(got.atomic).toBe(ids.stage);
    const by = new Map(got.txs.map((t) => [t.txid, t.txidOnly]));
    expect(by.get(ids.stage)).toBe(false);
    expect(by.get(ids.swap2)).toBe(true);
    for (const gone of [ids.swap1, ids.deploy, ids.root]) expect(by.has(gone)).toBe(false);
  });

  it("(b) a sats funding transaction outside the topic: raw, with its ancestry back to a proof (the proven one raw, nothing behind it)", () => {
    const got = entries(submissionBeef(TOPIC, atomicOf(ids.stage)));
    const by = new Map(got.txs.map((t) => [t.txid, t.txidOnly]));
    expect(by.get(ids.fund)).toBe(false);
    expect(by.get(ids.mined)).toBe(false);
    expect(by.has(minedParent)).toBe(false);
    expect(got.txs.map((t) => t.txid).sort()).toEqual([ids.stage, ids.swap2, ids.fund, ids.mined].sort());
  });

  it("(c) a proven ancestor keeps its BUMP", () => {
    const b = Beef.fromBinary(submissionBeef(TOPIC, atomicOf(ids.stage)));
    expect(b.bumps).toHaveLength(1);
    expect(b.bumps[0]!.blockHeight).toBe(900_000);
    const e = b.findTxid(ids.mined)!;
    expect(e.bumpIndex).toBe(0);
    expect(b.bumps[0]!.computeRoot(ids.mined)).toBe(bump.computeRoot(ids.mined));
  });

  it("(d) the subject is raw even when it carries a pool output of this token (a deploy, a rebalance)", () => {
    const got = entries(submissionBeef(TOPIC, atomicOf(ids.rebal)));
    expect(got.atomic).toBe(ids.rebal);
    const by = new Map(got.txs.map((t) => [t.txid, t.txidOnly]));
    expect(by.get(ids.rebal)).toBe(false);
    expect(by.get(ids.swap2)).toBe(true);
    expect(by.has(ids.swap1)).toBe(false);
    // the deploy itself as the subject: raw.
    const d = entries(topicBeef(wallet, [ids.deploy], topicAdmitted(A2, ""), { atomic: ids.deploy }));
    expect(d.txs).toEqual([{ txid: root, txidOnly: true }, { txid: ids.deploy, txidOnly: false }]);
  });

  it("another topic, or a BEEF that is not Atomic: sent as given", () => {
    const a = atomicOf(ids.stage);
    expect(submissionBeef("tm_other", a)).toBe(a);
    const v2 = wallet.toBinary();
    expect(submissionBeef(TOPIC, v2)).toBe(v2);
  });

  it("submitToOverlay sends the trimmed body; a door's refusal reaches the caller in full", async () => {
    const bodies: number[][] = [];
    const ok = (async (_u: string, init: RequestInit) => (bodies.push(Array.from(init.body as Uint8Array)), new Response("{}", { status: 200 }))) as never;
    await submitToOverlay("http://x/amm", TOPIC, atomicOf(ids.stage), ok);
    expect(bodies[0]).toEqual(submissionBeef(TOPIC, atomicOf(ids.stage)));
    const description = `UnknownTxidOnly: a txid-only entry for a transaction this instance does not hold (only a held one may be named by txid alone) ${"x".repeat(400)}`;
    const refusal = JSON.stringify({ status: "error", code: "ERR_REFUSED", description });
    const no = (async () => new Response(refusal, { status: 400 })) as never;
    await expect(submitToOverlay("http://x/amm", TOPIC, atomicOf(ids.stage), no)).rejects.toThrow(`POST http://x/submit: 400 ${refusal}`);
  });
});

// (e) `fundingBeef` over `topicBeef` gives the bytes 0.45.0's own walk gave, for its cases above.
describe("fundingBeef: the same bytes as 0.45.0's walk (0.51.1)", () => {
  function fundingBeef045(beef: Beef, fundingTxid: string, sources: string[], o: { poolTxid?: string; admitted?: (txid: string, tx?: Transaction) => boolean } = {}): number[] {
    const admitted = o.admitted ?? (() => false);
    const all = new Beef();
    for (const txid of new Set([fundingTxid, ...sources])) all.mergeBeef(beef.toBinaryAtomic(txid));
    const out = new Beef();
    const done = new Set<string>();
    const visit = (txid: string) => {
      if (done.has(txid)) return;
      done.add(txid);
      const e = all.findTxid(txid);
      const tx = e?.tx ?? (e?.rawTx ? Transaction.fromBinary(e.rawTx) : undefined);
      if (!e || e.isTxidOnly || !tx || admitted(txid, tx)) { out.mergeTxidOnly(txid); return; }
      if (e.bumpIndex !== undefined) { out.mergeRawTx(tx.toBinary(), out.mergeBump(all.bumps[e.bumpIndex]!)); return; }
      for (const i of tx.inputs) if (i.sourceTXID) visit(i.sourceTXID);
      out.mergeRawTx(tx.toBinary());
    };
    for (const txid of new Set([fundingTxid, ...sources])) visit(txid);
    if (o.poolTxid && !done.has(o.poolTxid)) { done.add(o.poolTxid); out.mergeTxidOnly(o.poolTxid); }
    return [...[1, 1, 1, 1], ...Utils.toArray(fundingTxid, "hex").reverse(), ...out.toBinary()];
  }
  const root = "11".repeat(32);
  const deploy = tx([[root, 0]], [{ lockingScript: poolScript(ASSET), satoshis: 1_000_000 }]);
  const swap1 = tx([[deploy, 0], [root, 1]], [{ lockingScript: poolScript(ASSET), satoshis: 1_000_100 }, { lockingScript: pay, satoshis: 5_000 }]);
  const mined = tx([["55".repeat(32), 0]], [{ lockingScript: pay, satoshis: 9_500 }]);
  const plain = tx([[mined, 0]], [{ lockingScript: pay, satoshis: 9_000 }]);
  const otherSwap = tx([[root, 3]], [{ lockingScript: poolScript(OTHER), satoshis: 1 }, { lockingScript: pay, satoshis: 7_000 }]);
  const wallet = new Beef();
  wallet.mergeTxidOnly(root);
  wallet.mergeRawTx(mined.toBinary(), wallet.mergeBump(new MerklePath(800_000, [[{ offset: 0, hash: mined.id("hex"), txid: true }, { offset: 1, hash: "66".repeat(32) }]])));
  for (const t of [deploy, swap1, plain, otherSwap]) wallet.mergeRawTx(t.toBinary());
  const cases: [string, string, string[], { poolTxid?: string; admitted?: (txid: string, tx?: Transaction) => boolean }][] = [
    ["admitted swap + pool, a plain one raw (proven ancestor)", plain.id("hex"), [plain.id("hex"), swap1.id("hex")], { poolTxid: swap1.id("hex"), admitted: topicAdmitted(ASSET, swap1.id("hex")) }],
    ["another token's swap; the pool added by txid", otherSwap.id("hex"), [otherSwap.id("hex")], { poolTxid: deploy.id("hex"), admitted: topicAdmitted(ASSET, deploy.id("hex")) }],
    ["no predicate", plain.id("hex"), [plain.id("hex"), swap1.id("hex")], {}],
  ];
  for (const [name, f, s, o] of cases) it(name, () => expect(fundingBeef(wallet, f, s, o)).toEqual(fundingBeef045(wallet, f, s, o)));
});
