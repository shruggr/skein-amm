/**
 * The LP wallet catches up one hop at a time from the pool output it holds.
 * History is asked with that outpoint. The page's `next` is not read. One
 * internalize files the continuation and, on a swap, takes the LP fee in
 * (0.55.0: a sats-in swap's as a BRC-29 wallet payment, a token-in swap's as a
 * `p color` receipt, both derived from the pool's LP keyID, sender the LP's
 * own identity key). Close-shaped output 0 and a transaction that does not
 * spend the held output stop the walk.
 */
import { Beef, LockingScript, P2PKH, PrivateKey, Transaction, UnlockingScript, Utils, type WalletInterface, type WalletProtocol } from "@bsv/sdk";
import { describe, expect, it } from "vitest";
import { assetBasket } from "@bsv/color-permission-module";
import type { PoolEvent } from "../src/lib/overlay";
import { PoolTemplate, RESCINDED_KEY, oneColorValuePrefix } from "../src/pool";
import { POOL_TAG, lpFeeDerivation } from "../src/lp/poolDeploy";
import { syncPool, type SyncIo } from "../src/lp/sync";
import { poolCustomInstructions } from "../src/lp/poolDeploy";
import { POOLS_BASKET } from "../src/wallet/color";
import v from "./fixtures/amm-topic-vectors.json";

const tokenDeploy = Transaction.fromHex(v.token_deploy);
const poolDeploy = Transaction.fromHex(v.pool_deploy);
const TOKEN_ID = `${tokenDeploy.id("hex")}_0`;
const POOL_ID = `${poolDeploy.id("hex")}_0`;
const payee = new PrivateKey(11);
const PUB = payee.toPublicKey().toString();
const pool = PoolTemplate.decode(poolDeploy.outputs[0]!.lockingScript)!;
const payout = new P2PKH().lock(payee.toAddress());
/** 0.55.0: a sats-in swap's LP fee output, to the pool's LpSatsPkh. */
const feeLock = LockingScript.fromHex(`76a914${pool.state.lpSatsPkh}88ac`);
/** 0.55.0: a token-in swap's LP fee output of `amount`, to the pool's LpTokenPkh. */
const tokenFeeLock = (amount: bigint) => Utils.toHex(oneColorValuePrefix(Utils.toArray(pool.args.assetId, "hex"), amount)) + `76a914${pool.state.lpTokenPkh}88ac`;

const HELD = {
  id: TOKEN_ID,
  op: "amm-pool",
  protocolID: [1, "openex fees"] as [number, string],
  keyID: Utils.toBase64(Array.from({ length: 16 }, (_, n) => n + 1)),
  counterparty: "self",
};

interface Row {
  spendable?: boolean;
  outpoint: string;
  satoshis: number;
  lockingScript?: string;
  customInstructions?: string;
  tags?: string[];
}

function deployRow(): Row {
  return {
    outpoint: `${poolDeploy.id("hex")}.0`,
    satoshis: poolDeploy.outputs[0]!.satoshis!,
    lockingScript: poolDeploy.outputs[0]!.lockingScript.toHex(),
    customInstructions: JSON.stringify(HELD),
    tags: [POOL_TAG],
  };
}

function spend(source: Transaction, vout: number, outputs: { satoshis: number; lockingScript: LockingScript | string }[]): Transaction {
  const tx = new Transaction();
  tx.addInput({
    sourceTXID: source.id("hex"),
    sourceOutputIndex: vout,
    sourceTransaction: source,
    unlockingScript: new UnlockingScript(),
    sequence: 0xffffffff,
  });
  for (const o of outputs) {
    tx.addOutput({
      satoshis: o.satoshis,
      lockingScript: typeof o.lockingScript === "string" ? LockingScript.fromHex(o.lockingScript) : o.lockingScript,
    });
  }
  return tx;
}

function rawBeef(tx: Transaction, parents: Transaction[]): number[] {
  const b = new Beef();
  for (const p of parents) b.mergeRawTx(p.toBinary());
  b.mergeRawTx(tx.toBinary());
  return b.toBinary();
}

function event(tx: Transaction, type: PoolEvent["type"], extra: Partial<PoolEvent> = {}): PoolEvent {
  return {
    type,
    txid: tx.id("hex"),
    poolId: POOL_ID,
    lpPubKey: pool.state.lpPubKey,
    liquidityFeeBps: 30n,
    validationFeeBps: 5n,
    commissionBps: 10n,
    validatorIdentityKey: pool.state.validatorIdentity,
    at: 1,
    swaps: 1,
    ...extra,
  };
}

function continuationScript(sourceScript: LockingScript, state = pool.state): LockingScript {
  const decoded = PoolTemplate.decode(sourceScript)!;
  return PoolTemplate.lockContinuation(decoded, state);
}

interface WalletBag {
  wallet: WalletInterface;
  calls: { method: string; args: unknown }[];
  pools: Row[];
}

function syncWallet(initial: { pools?: Row[] } = {}): WalletBag {
  const pools = [...(initial.pools ?? [])];
  const calls: { method: string; args: unknown }[] = [];
  const wallet = {
    async listOutputs(args: { basket?: string; include?: string }) {
      calls.push({ method: "listOutputs", args });
      const rows = args.basket === POOLS_BASKET ? pools : [];
      return { outputs: rows, totalOutputs: rows.length };
    },
    async internalizeAction(args: { tx: number[]; outputs: { outputIndex: number; protocol: string; insertionRemittance?: { basket: string; tags?: string[]; customInstructions?: string } }[]; description: string; labels?: string[] }) {
      calls.push({ method: "internalizeAction", args });
      const beef = Beef.fromBinary(args.tx);
      const subject = beef.txs[beef.txs.length - 1];
      const tx = subject?.tx ?? (subject?.rawTx ? Transaction.fromBinary(subject.rawTx) : undefined);
      if (tx) {
        const txid = tx.id("hex");
        for (const o of args.outputs) {
          const rem = o.insertionRemittance;
          if (o.protocol !== "basket insertion" || !rem || rem.basket !== POOLS_BASKET) continue;
          const out = tx.outputs[o.outputIndex];
          const row: Row = {
            outpoint: `${txid}.${o.outputIndex}`,
            satoshis: out?.satoshis ?? 0,
            lockingScript: out?.lockingScript.toHex(),
            customInstructions: rem.customInstructions,
            ...(rem.tags ? { tags: rem.tags } : {}),
          };
          pools.push(row);
        }
      }
      return { accepted: true };
    },
    async getPublicKey(args: { identityKey?: boolean }) {
      calls.push({ method: "getPublicKey", args });
      return { publicKey: PUB };
    },
  };
  return { wallet: wallet as unknown as WalletInterface, calls, pools };
}

function ioOf(pages: Map<string, PoolEvent[]>, txs: Map<string, number[]>): SyncIo & { asked: string[] } {
  const asked: string[] = [];
  return {
    asked,
    async history(outpoint) {
      asked.push(outpoint);
      return { events: pages.get(outpoint) ?? [] };
    },
    async readTx(txid) {
      return txs.get(txid.toLowerCase()) ?? null;
    },
  };
}

function internalized(bag: WalletBag) {
  return bag.calls.filter((c) => c.method === "internalizeAction").map((c) => c.args as { tx: number[]; outputs: { outputIndex: number; protocol: string; insertionRemittance?: { basket?: string; tags?: string[]; customInstructions?: string }; paymentRemittance?: { derivationPrefix: string; derivationSuffix: string; senderIdentityKey: string } }[]; description: string; labels?: string[] });
}

/** 0.55.0: a sats-in swap's LP fee as the walk declares it: a wallet payment from the pool's keyID, the sender the LP's own identity. */
function satsFee(keyID: string) {
  return { outputIndex: 2, protocol: "wallet payment", paymentRemittance: { ...lpFeeDerivation(keyID), senderIdentityKey: PUB } };
}

describe("syncPool", () => {
  const cont = continuationScript(poolDeploy.outputs[0]!.lockingScript);
  // A sats-in swap: the pool's BSV reserve rises.
  const swap = spend(poolDeploy, 0, [
    { satoshis: poolDeploy.outputs[0]!.satoshis! + 1000, lockingScript: cont },
    { satoshis: 1000, lockingScript: payout },
    { satoshis: 60, lockingScript: feeLock },
  ]);
  const swapBeef = rawBeef(swap, [poolDeploy]);
  const swapCursor = `${swap.id("hex")}_0`;

  function oneHop() {
    const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(swap, "swap", { feeLp: 60n })]]]);
    const txs = new Map<string, number[]>([[swap.id("hex"), swapBeef]]);
    return { pages, txs, io: ioOf(pages, txs) };
  }

  it("files the continuation and takes the satoshi fee in (a wallet payment) in one internalize, then a second walk files nothing", async () => {
    const bag = syncWallet({ pools: [deployRow()] });
    const { io } = oneHop();
    expect(await syncPool(bag.wallet, POOL_ID, io)).toEqual({ caughtUp: true, filed: 1 });
    const filed = internalized(bag);
    expect(filed).toHaveLength(1);
    expect(filed[0]!.description).toBe("AMM pool output filed");
    expect(filed[0]!).not.toHaveProperty("labels");
    // 0.48.2: the transaction's Atomic BEEF (the toolbox refuses any other).
    expect(Beef.fromBinary(filed[0]!.tx).atomicTxid).toBe(swap.id("hex"));
    const [continuation, fee] = filed[0]!.outputs;
    expect(continuation).toMatchObject({ outputIndex: 0, insertionRemittance: { basket: POOLS_BASKET, tags: [POOL_TAG] } });
    const contCi = JSON.parse(continuation!.insertionRemittance!.customInstructions!);
    // 0.48.0 (D9): the key and the pool id only, whatever the held row carried.
    expect(contCi).toEqual({ protocolID: HELD.protocolID, keyID: HELD.keyID, counterparty: "self", poolId: POOL_ID });
    // 0.55.0: the fee a wallet payment, prefix and suffix the pool's keyID, sender the LP's identity key (read once).
    expect(fee).toEqual(satsFee(HELD.keyID));
    // David 2026-11-12: the keyID's 16 bytes split in half, each base64.
    expect(lpFeeDerivation(HELD.keyID)).toEqual({ derivationPrefix: Utils.toBase64([1, 2, 3, 4, 5, 6, 7, 8]), derivationSuffix: Utils.toBase64([9, 10, 11, 12, 13, 14, 15, 16]) });
    expect(() => lpFeeDerivation("YW1tLWxw abc=")).toThrow(/not 16 bytes of base64/);
    expect(() => lpFeeDerivation(Utils.toBase64([1, 2, 3]))).toThrow(/not 16 bytes of base64/);
    expect(bag.calls.filter((c) => c.method === "getPublicKey").map((c) => c.args)).toEqual([{ identityKey: true }]);
    // No fees basket: only amm-pools is listed.
    expect(bag.calls.filter((c) => c.method === "listOutputs").map((c) => (c.args as { basket: string }).basket)).toEqual([POOLS_BASKET]);
    expect(io.asked).toEqual([POOL_ID, swapCursor]);

    const before = internalized(bag).length;
    await syncPool(bag.wallet, POOL_ID, io);
    expect(internalized(bag)).toHaveLength(before);
    expect(io.asked.slice(-2)).toEqual([POOL_ID, swapCursor]);
  });

  it("applies a page of two swaps in order and asks next for the last continuation, not a server cursor", async () => {
    const mid = PoolTemplate.decode(cont)!;
    const cont2 = PoolTemplate.lockContinuation(mid, mid.state);
    const swap2 = spend(swap, 0, [
      { satoshis: swap.outputs[0]!.satoshis! + 900, lockingScript: cont2 },
      { satoshis: 900, lockingScript: payout },
      { satoshis: 40, lockingScript: feeLock },
    ]);
    const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(swap, "swap", { feeLp: 60n }), event(swap2, "swap", { feeLp: 40n, swaps: 2 })]]]);
    const txs = new Map<string, number[]>([
      [swap.id("hex"), swapBeef],
      [swap2.id("hex"), rawBeef(swap2, [swap])],
    ]);
    const io = ioOf(pages, txs);
    const bag = syncWallet({ pools: [deployRow()] });
    await syncPool(bag.wallet, POOL_ID, io);
    expect(internalized(bag)).toHaveLength(2);
    expect(internalized(bag).map((a) => a.outputs.map((o) => o.outputIndex))).toEqual([[0, 2], [0, 2]]);
    expect(internalized(bag).map((a) => a.outputs[1])).toEqual([satsFee(HELD.keyID), satsFee(HELD.keyID)]);
    // The identity key is read once per walk.
    expect(bag.calls.filter((c) => c.method === "getPublicKey")).toHaveLength(1);
    expect(io.asked).toEqual([POOL_ID, `${swap2.id("hex")}_0`]);
  });

  it("a rescind hop files output 0 and no fee", async () => {
    const rescinded = PoolTemplate.lockContinuation(pool, { ...pool.state, validatorPubKey: RESCINDED_KEY });
    const tx = spend(poolDeploy, 0, [
      { satoshis: poolDeploy.outputs[0]!.satoshis!, lockingScript: rescinded },
      { satoshis: 1, lockingScript: payout },
      { satoshis: 60, lockingScript: feeLock },
    ]);
    const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(tx, "rescind", { feeLp: 60n })]]]);
    const txs = new Map([[tx.id("hex"), rawBeef(tx, [poolDeploy])]]);
    const bag = syncWallet({ pools: [deployRow()] });
    await syncPool(bag.wallet, POOL_ID, ioOf(pages, txs));
    const filed = internalized(bag);
    expect(filed).toHaveLength(1);
    expect(filed[0]!.outputs.map((o) => o.outputIndex)).toEqual([0]);
    expect(filed[0]!.outputs[0]!.insertionRemittance!.basket).toBe(POOLS_BASKET);
  });

  it("takes a token fee in from output 2 as a `p color` receipt into the pool's token's value basket (its token the pool's, not the row's)", async () => {
    const tokenFee = tokenFeeLock(7n);
    // A token-in swap: the pool's BSV reserve falls.
    const tx = spend(poolDeploy, 0, [
      { satoshis: poolDeploy.outputs[0]!.satoshis! - 1000, lockingScript: cont },
      { satoshis: 1, lockingScript: payout },
      { satoshis: 1, lockingScript: tokenFee },
    ]);
    const held = { ...HELD, id: "not-the-token" };
    const row = { ...deployRow(), customInstructions: JSON.stringify(held) };
    const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(tx, "swap", { feeLp: 7n, side: "sell" })]]]);
    const txs = new Map([[tx.id("hex"), rawBeef(tx, [poolDeploy])]]);
    const bag = syncWallet({ pools: [row] });
    await syncPool(bag.wallet, POOL_ID, ioOf(pages, txs));
    const fee = internalized(bag)[0]!.outputs[1]!;
    expect(fee).toMatchObject({ outputIndex: 2, protocol: "basket insertion", insertionRemittance: { basket: assetBasket(TOKEN_ID, "value") } });
    expect(JSON.parse(fee.insertionRemittance!.customInstructions!)).toEqual({ version: 1, assetId: TOKEN_ID, ...lpFeeDerivation(HELD.keyID), senderIdentityKey: PUB });
  });

  it("stops when the event does not spend the output the wallet holds", async () => {
    const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(poolDeploy, "deploy"), event(swap, "swap", { feeLp: 60n })]]]);
    const txs = new Map<string, number[]>([
      [poolDeploy.id("hex"), rawBeef(poolDeploy, [])],
      [swap.id("hex"), swapBeef],
    ]);
    const bag = syncWallet({ pools: [deployRow()] });
    const io = ioOf(pages, txs);
    const r = await syncPool(bag.wallet, POOL_ID, io);
    expect(r).toMatchObject({ caughtUp: false });
    expect((r as { reason: string }).reason).toContain("does not spend the pool output the wallet holds");
    expect(internalized(bag)).toHaveLength(0);
    expect(io.asked).toEqual([POOL_ID]);
  });

  it("stops when output 0 is not a pool", async () => {
    const tx = spend(poolDeploy, 0, [
      { satoshis: 1000, lockingScript: payout },
      { satoshis: 1, lockingScript: payout },
    ]);
    const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(tx, "swap", { feeLp: 60n })]]]);
    const txs = new Map([[tx.id("hex"), rawBeef(tx, [poolDeploy])]]);
    const bag = syncWallet({ pools: [deployRow()] });
    expect(await syncPool(bag.wallet, POOL_ID, ioOf(pages, txs))).toEqual({ caughtUp: false, reason: `${tx.id("hex")}'s output 0 is not a pool` });
    expect(internalized(bag)).toHaveLength(0);
  });

  it("stops without throwing when the transaction cannot be read", async () => {
    const missing = "ab".repeat(32);
    const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(swap, "swap", { feeLp: 60n, txid: missing })]]]);
    const bag = syncWallet({ pools: [deployRow()] });
    await expect(syncPool(bag.wallet, POOL_ID, ioOf(pages, new Map()))).resolves.toEqual({ caughtUp: false, reason: `the transaction ${missing} cannot be read from the exchange` });
    expect(internalized(bag)).toHaveLength(0);
  });

  it("a swap that names an LP fee and has no output 2 stops the walk before the known-skip", async () => {
    const tx = spend(poolDeploy, 0, [
      { satoshis: poolDeploy.outputs[0]!.satoshis!, lockingScript: cont },
      { satoshis: 1000, lockingScript: payout },
    ]);
    const txid = tx.id("hex");
    const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(tx, "swap", { feeLp: 60n })]]]);
    const txs = new Map([[txid, rawBeef(tx, [poolDeploy])]]);
    const bag = syncWallet({
      pools: [deployRow(), { outpoint: `${txid}.0`, satoshis: 1, lockingScript: "00", customInstructions: "{}" }],
    });
    expect(await syncPool(bag.wallet, POOL_ID, ioOf(pages, txs))).toEqual({ caughtUp: false, reason: `${txid} names an LP fee and has no output 2` });
    expect(internalized(bag)).toHaveLength(0);
  });

  it("0.55.0: the fee is the transaction's — an event naming no fee, the tx paying the pool's LP fee at output 2: the fee is declared", async () => {
    const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(swap, "swap")]]]);
    const bag = syncWallet({ pools: [deployRow()] });
    expect(await syncPool(bag.wallet, POOL_ID, ioOf(pages, new Map([[swap.id("hex"), swapBeef]])))).toEqual({ caughtUp: true, filed: 1 });
    expect(internalized(bag)[0]!.outputs[1]).toEqual(satsFee(HELD.keyID));
  });

  it("0.55.0: an event naming a fee whose tx pays another script at output 2 (the LP key's, or the other direction's address) stops the walk, nothing filed", async () => {
    for (const other of [new P2PKH().lock(payee.toAddress()), LockingScript.fromHex(`76a914${pool.state.lpTokenPkh}88ac`)]) {
      const tx = spend(poolDeploy, 0, [
        { satoshis: poolDeploy.outputs[0]!.satoshis! + 1000, lockingScript: cont },
        { satoshis: 1000, lockingScript: payout },
        { satoshis: 60, lockingScript: other },
      ]);
      const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(tx, "swap", { feeLp: 60n })]]]);
      const bag = syncWallet({ pools: [deployRow()] });
      expect(await syncPool(bag.wallet, POOL_ID, ioOf(pages, new Map([[tx.id("hex"), rawBeef(tx, [poolDeploy])]])))).toEqual({
        caughtUp: false,
        reason: `${tx.id("hex")} names an LP fee and its output 2 is not the pool's LP fee output`,
      });
      expect(internalized(bag)).toHaveLength(0);
    }
  });

  it("0.55.0: a token-in swap whose output 2 is a token output to the sats address (not the pool's LP fee output) and an event naming no fee: the continuation alone", async () => {
    const wrong = Utils.toHex(oneColorValuePrefix(Utils.toArray(pool.args.assetId, "hex"), 7n)) + `76a914${pool.state.lpSatsPkh}88ac`;
    const tx = spend(poolDeploy, 0, [
      { satoshis: poolDeploy.outputs[0]!.satoshis! - 1000, lockingScript: cont },
      { satoshis: 1000, lockingScript: payout },
      { satoshis: 1, lockingScript: wrong },
    ]);
    const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(tx, "swap")]]]);
    const bag = syncWallet({ pools: [deployRow()] });
    await syncPool(bag.wallet, POOL_ID, ioOf(pages, new Map([[tx.id("hex"), rawBeef(tx, [poolDeploy])]])));
    expect(internalized(bag)[0]!.outputs.map((o) => o.outputIndex)).toEqual([0]);
  });

  it("stops, nothing filed, when the pool's keyID does not split into the fee derivation (not 16 bytes)", async () => {
    const row = { ...deployRow(), customInstructions: JSON.stringify({ ...HELD, keyID: Utils.toBase64([1, 2, 3]) }) };
    const bag = syncWallet({ pools: [row] });
    const r = await syncPool(bag.wallet, POOL_ID, oneHop().io);
    expect(r).toMatchObject({ caughtUp: false, reason: expect.stringMatching(/not 16 bytes of base64/) });
    expect(internalized(bag)).toHaveLength(0);
  });

  it("stops, with the wallet's reason, when the wallet refuses the hop", async () => {
    const bag = syncWallet({ pools: [deployRow()] });
    (bag.wallet as unknown as { internalizeAction: () => Promise<never> }).internalizeAction = async () => {
      throw new Error("must be a complete, exactly framed Atomic BEEF");
    };
    const r = await syncPool(bag.wallet, POOL_ID, oneHop().io);
    expect(r).toEqual({ caughtUp: false, reason: `the wallet refused ${swap.id("hex")}: must be a complete, exactly framed Atomic BEEF` });
  });

  it("does nothing when the wallet holds no output of the pool", async () => {
    const bag = syncWallet();
    const io = oneHop().io;
    expect(await syncPool(bag.wallet, POOL_ID, io)).toEqual({ caughtUp: true });
    expect(io.asked).toEqual([]);
    expect(internalized(bag)).toHaveLength(0);
  });
});

describe("syncPool (0.54.0): the start, a close, a rebalance hop", () => {
  const cont = continuationScript(poolDeploy.outputs[0]!.lockingScript);
  const swap = spend(poolDeploy, 0, [
    { satoshis: poolDeploy.outputs[0]!.satoshis! + 1000, lockingScript: cont },
    { satoshis: 1000, lockingScript: payout },
    { satoshis: 60, lockingScript: feeLock },
  ]);
  const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(swap, "swap", { feeLp: 60n })]]]);
  const txs = new Map<string, number[]>([[swap.id("hex"), rawBeef(swap, [poolDeploy])]]);
  const otherKeyID = Utils.toBase64(Array.from({ length: 16 }, (_, n) => 200 - n));
  /** The deploy-history row `historyPoolRows` builds from the deploy output's description. */
  const historyRow = (protocolID: [number, string], keyID: string): Row => ({
    outpoint: `${poolDeploy.id("hex")}.0`,
    satoshis: poolDeploy.outputs[0]!.satoshis!,
    lockingScript: poolDeploy.outputs[0]!.lockingScript.toHex(),
    tags: [POOL_TAG],
    customInstructions: poolCustomInstructions({ protocolID: protocolID as WalletProtocol, keyID, poolId: POOL_ID }),
  });

  it.each([
    ["openex fees", [1, "openex fees"] as [number, string], HELD.keyID],
  ])("walks from the deploy-history row when the basket's row is listed non-spendable (the NANNER case), %s key", async (_era, protocolID, keyID) => {
    const stale = { ...historyRow(protocolID, keyID), spendable: false };
    const bag = syncWallet({ pools: [stale] });
    const io = ioOf(pages, txs);
    expect(await syncPool(bag.wallet, POOL_ID, io, historyRow(protocolID, keyID) as never)).toEqual({ caughtUp: true, filed: 1 });
    expect(io.asked).toEqual([POOL_ID, `${swap.id("hex")}_0`]);
    const [cont0, fee] = internalized(bag)[0]!.outputs;
    const want = { protocolID, keyID, counterparty: "self", poolId: POOL_ID };
    expect(JSON.parse(cont0!.insertionRemittance!.customInstructions!)).toEqual(want);
    expect(fee).toEqual(satsFee(keyID));
    // The stale row is left alone: not rewritten, still listed as it was.
    expect(bag.pools[0]).toEqual(stale);
  });

  it("a spendable basket row wins over the start", async () => {
    const bag = syncWallet({ pools: [deployRow()] });
    await syncPool(bag.wallet, POOL_ID, ioOf(pages, txs), historyRow([1, "openex fees"], otherKeyID) as never);
    expect(internalized(bag)[0]!.outputs[1]).toEqual(satsFee(HELD.keyID));
  });

  it("a close event ends the walk caught up, filing nothing past it", async () => {
    const closer = spend(poolDeploy, 0, [{ satoshis: 1000, lockingScript: payout }]);
    const io = ioOf(new Map([[POOL_ID, [event(closer, "close")]]]), new Map());
    const bag = syncWallet({ pools: [deployRow()] });
    expect(await syncPool(bag.wallet, POOL_ID, io)).toEqual({ caughtUp: true, closed: closer.id("hex") });
    expect(internalized(bag)).toHaveLength(0);
  });

  it("a hop whose pool names another LpPubKey (a rebalance) ends the walk caught up, filing nothing; the hops before it are filed", async () => {
    const other = new PrivateKey(99).toPublicKey().toString();
    const moved = PoolTemplate.lockContinuation(PoolTemplate.decode(cont)!, { ...pool.state, lpPubKey: other });
    const rebalance = spend(swap, 0, [{ satoshis: 2000, lockingScript: moved }]);
    const io = ioOf(
      new Map([[POOL_ID, [event(swap, "swap", { feeLp: 60n }), event(rebalance, "swap", { feeLp: 60n })]]]),
      new Map([
        [swap.id("hex"), rawBeef(swap, [poolDeploy])],
        [rebalance.id("hex"), rawBeef(rebalance, [swap])],
      ]),
    );
    const bag = syncWallet({ pools: [deployRow()] });
    expect(await syncPool(bag.wallet, POOL_ID, io)).toEqual({ caughtUp: true, closed: rebalance.id("hex"), filed: 1 });
    expect(internalized(bag).map((a) => Beef.fromBinary(a.tx).atomicTxid)).toEqual([swap.id("hex")]);
  });
});
