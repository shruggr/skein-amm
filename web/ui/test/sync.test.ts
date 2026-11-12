/**
 * The LP wallet catches up one hop at a time from the pool output it holds.
 * History is asked with that outpoint. The page's `next` is not read. One
 * internalize files the continuation and, on a swap, the LP fee. Close-shaped
 * output 0 and a transaction that does not spend the held output stop the walk.
 */
import { Beef, LockingScript, P2PKH, PrivateKey, Transaction, UnlockingScript, Utils, type CreateActionArgs, type WalletInterface, type WalletProtocol } from "@bsv/sdk";
import { describe, expect, it } from "vitest";
import { ColorPlan } from "@bsv/color-permission-module";
import type { PoolEvent } from "../src/lib/overlay";
import { PoolTemplate, RESCINDED_KEY } from "../src/pool";
import { POOL_TAG, tokenP2pkhScript } from "../src/lp/poolDeploy";
import { COLLECT_PAGE_TOKEN_INPUTS, FEES_BASKET, availableFees, collectBasket, composePages, estimatePageFee, syncPool, type FeeInput, type SyncIo } from "../src/lp/sync";
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
const feeLock = new P2PKH().lock(payee.toAddress());

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
  fees: Row[];
}

function syncWallet(initial: { pools?: Row[]; fees?: Row[]; feeTxs?: Transaction[] } = {}): WalletBag {
  const pools = [...(initial.pools ?? [])];
  const fees = [...(initial.fees ?? [])];
  const feeTxs = initial.feeTxs ?? [];
  const calls: { method: string; args: unknown }[] = [];
  const wallet = {
    async listOutputs(args: { basket?: string; include?: string }) {
      calls.push({ method: "listOutputs", args });
      if (args.include === "entire transactions") {
        const b = new Beef();
        for (const tx of feeTxs) b.mergeRawTx(tx.toBinary());
        return feeTxs.length === 0 ? { outputs: [], totalOutputs: 0 } : { outputs: [], totalOutputs: fees.length, BEEF: b.toBinary() };
      }
      const rows = args.basket === FEES_BASKET ? fees : args.basket === POOLS_BASKET ? pools : [];
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
          if (o.protocol !== "basket insertion" || !rem) continue;
          const out = tx.outputs[o.outputIndex];
          const row: Row = {
            outpoint: `${txid}.${o.outputIndex}`,
            satoshis: out?.satoshis ?? 0,
            lockingScript: out?.lockingScript.toHex(),
            customInstructions: rem.customInstructions,
            ...(rem.tags ? { tags: rem.tags } : {}),
          };
          if (rem.basket === FEES_BASKET) fees.push(row);
          else pools.push(row);
        }
      }
      return { accepted: true };
    },
    async getPublicKey(args: { identityKey?: boolean }) {
      calls.push({ method: "getPublicKey", args });
      return { publicKey: PUB };
    },
    async createAction(args: CreateActionArgs) {
      calls.push({ method: "createAction", args });
      return {};
    },
    async abortAction(args: unknown) {
      calls.push({ method: "abortAction", args });
      return { aborted: true };
    },
  };
  return { wallet: wallet as unknown as WalletInterface, calls, pools, fees };
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
  return bag.calls.filter((c) => c.method === "internalizeAction").map((c) => c.args as { tx: number[]; outputs: { outputIndex: number; insertionRemittance?: { basket?: string; tags?: string[]; customInstructions?: string } }[]; description: string; labels?: string[] });
}

describe("syncPool", () => {
  const cont = continuationScript(poolDeploy.outputs[0]!.lockingScript);
  const swap = spend(poolDeploy, 0, [
    { satoshis: poolDeploy.outputs[0]!.satoshis!, lockingScript: cont },
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

  it("files the continuation and the satoshi fee in one internalize, then a second walk files nothing", async () => {
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
    expect(fee).toMatchObject({ outputIndex: 2, insertionRemittance: { basket: FEES_BASKET } });
    expect(fee!.insertionRemittance).not.toHaveProperty("tags");
    const feeCi = JSON.parse(fee!.insertionRemittance!.customInstructions!);
    expect(feeCi).toEqual({ protocolID: HELD.protocolID, keyID: HELD.keyID, counterparty: "self", poolId: POOL_ID });
    expect(feeCi).not.toHaveProperty("op");
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
      { satoshis: swap.outputs[0]!.satoshis!, lockingScript: cont2 },
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

  it("files a token fee from output 2 (its token that output's script's, no id in the row)", async () => {
    const tokenFee = tokenP2pkhScript(TOKEN_ID, 7n, PUB);
    const tx = spend(poolDeploy, 0, [
      { satoshis: poolDeploy.outputs[0]!.satoshis!, lockingScript: cont },
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
    expect(fee.outputIndex).toBe(2);
    // 0.48.0 (D9): no `id` — the token fee's token is its own script's.
    expect(JSON.parse(fee.insertionRemittance!.customInstructions!)).not.toHaveProperty("id");
    expect(JSON.parse(fee.insertionRemittance!.customInstructions!)).not.toHaveProperty("op");
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
      fees: [{ outpoint: `${txid}.2`, satoshis: 1, lockingScript: "00", customInstructions: "{}" }],
    });
    expect(await syncPool(bag.wallet, POOL_ID, ioOf(pages, txs))).toEqual({ caughtUp: false, reason: `${txid} names an LP fee and has no output 2` });
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

describe("collectBasket", () => {
  const script = new P2PKH().lock(payee.toAddress()).toHex();
  const feeTx = new Transaction();
  feeTx.addInput({ sourceTXID: "44".repeat(32), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
  feeTx.addOutput({ satoshis: 5000, lockingScript: LockingScript.fromHex(script) });
  const feeOutpoint = `${feeTx.id("hex")}.0`;
  const specs = JSON.stringify({ protocolID: [1, "openex fees"], keyID: HELD.keyID, counterparty: "self", poolId: POOL_ID });

  function feeRow(over: Partial<Row> = {}): Row {
    return { outpoint: feeOutpoint, satoshis: 5000, lockingScript: script, customInstructions: specs, ...over };
  }

  it("an empty basket spends nothing", async () => {
    const bag = syncWallet();
    await expect(collectBasket(bag.wallet)).resolves.toEqual({ txids: [], pages: 0, sats: 0n, tokens: [] });
    expect(bag.calls.some((c) => c.method === "createAction" || c.method === "getPublicKey")).toBe(false);
  });

  it("a fee output with no key throws before the wallet is asked for a key", async () => {
    const bag = syncWallet({ fees: [feeRow({ customInstructions: "{}" })] });
    await expect(collectBasket(bag.wallet)).rejects.toThrow("the fee output names no key");
    expect(bag.calls.some((c) => c.method === "getPublicKey")).toBe(false);
  });

  it("a fee output with no locking script throws", async () => {
    const bag = syncWallet({ fees: [feeRow({ lockingScript: undefined, satoshis: 0 })] });
    await expect(collectBasket(bag.wallet)).rejects.toThrow("the fee output has no locking script");
  });

  it("a row whose sats and tokens are both zero returns without an action, after the key check", async () => {
    const bag = syncWallet({ fees: [feeRow({ satoshis: 0 })] });
    await expect(collectBasket(bag.wallet)).resolves.toEqual({ txids: [], pages: 0, sats: 0n, tokens: [] });
    expect(bag.calls.some((c) => c.method === "createAction" || c.method === "getPublicKey")).toBe(false);
    const missing = syncWallet({ fees: [feeRow({ satoshis: 0, customInstructions: "{}" })] });
    await expect(collectBasket(missing.wallet)).rejects.toThrow("the fee output names no key");
    expect(missing.calls.some((c) => c.method === "getPublicKey")).toBe(false);
  });

  it("0.54.0: a sats-only page names its fee rows and NO output (the wallet keeps the sats as change); page 1 not handed back throws", async () => {
    const bag = syncWallet({ fees: [feeRow()], feeTxs: [feeTx] });
    await expect(collectBasket(bag.wallet)).rejects.toThrow("the wallet did not hand the transaction back for the page's signatures");
    const created = bag.calls.find((c) => c.method === "createAction")!.args as CreateActionArgs;
    expect(created.description).toBe("Collect pool fees");
    expect(created.options).toEqual({ randomizeOutputs: false, acceptDelayedBroadcast: false });
    expect(created).not.toHaveProperty("labels");
    expect(created.inputs).toEqual([{ outpoint: feeOutpoint, unlockingScriptLength: 108, inputDescription: "A pool fee" }]);
    expect(created.outputs).toEqual([]);
    expect(bag.calls.some((c) => c.method === "internalizeAction")).toBe(false);
  });

  it("spends a token fee to one p color output", async () => {
    const tokenScript = tokenP2pkhScript(TOKEN_ID, 7n, PUB);
    const tx = new Transaction();
    tx.addInput({ sourceTXID: "45".repeat(32), sourceOutputIndex: 1, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
    tx.addOutput({ satoshis: 1, lockingScript: LockingScript.fromHex(tokenScript) });
    const row: Row = {
      outpoint: `${tx.id("hex")}.0`,
      satoshis: 1,
      lockingScript: tokenScript,
      customInstructions: specs,
    };
    const bag = syncWallet({ fees: [row], feeTxs: [tx] });
    await expect(collectBasket(bag.wallet)).rejects.toThrow("the wallet did not hand the transaction back for the page's signatures");
    const created = bag.calls.find((c) => c.method === "createAction")!.args as CreateActionArgs;
    expect(created.description).toBe("Collect pool fees");
    expect(created.options).toEqual({ randomizeOutputs: false, acceptDelayedBroadcast: false });
    expect(created.outputs).toEqual([{ lockingScript: tokenP2pkhScript(TOKEN_ID, 7n, PUB), satoshis: 1, outputDescription: "Pool fees in tokens" }]);
    expect(Array.isArray(created.labels)).toBe(true);
    expect(created.inputs?.[0]).toMatchObject({ outpoint: `${tx.id("hex")}.0`, unlockingScriptLength: 108, inputDescription: "A pool fee" });
  });
});

describe("syncPool (0.54.0): the start, a close, a rebalance hop", () => {
  const cont = continuationScript(poolDeploy.outputs[0]!.lockingScript);
  const swap = spend(poolDeploy, 0, [
    { satoshis: poolDeploy.outputs[0]!.satoshis!, lockingScript: cont },
    { satoshis: 1000, lockingScript: payout },
    { satoshis: 60, lockingScript: feeLock },
  ]);
  const pages = new Map<string, PoolEvent[]>([[POOL_ID, [event(swap, "swap", { feeLp: 60n })]]]);
  const txs = new Map<string, number[]>([[swap.id("hex"), rawBeef(swap, [poolDeploy])]]);
  const brc29KeyID = `${Utils.toBase64(Utils.toArray("amm-lp", "utf8"))} ${Utils.toBase64(Array.from({ length: 16 }, (_, n) => 200 - n))}`;
  /** The deploy-history row `historyPoolRows` builds from the deploy output's description. */
  const historyRow = (protocolID: [number, string], keyID: string): Row => ({
    outpoint: `${poolDeploy.id("hex")}.0`,
    satoshis: poolDeploy.outputs[0]!.satoshis!,
    lockingScript: poolDeploy.outputs[0]!.lockingScript.toHex(),
    tags: [POOL_TAG],
    customInstructions: poolCustomInstructions({ protocolID: protocolID as WalletProtocol, keyID, poolId: POOL_ID }),
  });

  it.each([
    ["BRC-29", [2, "3241645161d8"] as [number, string], brc29KeyID],
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
    expect(fee).toMatchObject({ outputIndex: 2, insertionRemittance: { basket: FEES_BASKET } });
    expect(JSON.parse(fee!.insertionRemittance!.customInstructions!)).toEqual(want);
    // The stale row is left alone: not rewritten, still listed as it was.
    expect(bag.pools[0]).toEqual(stale);
  });

  it("a spendable basket row wins over the start", async () => {
    const bag = syncWallet({ pools: [deployRow()] });
    await syncPool(bag.wallet, POOL_ID, ioOf(pages, txs), historyRow([2, "3241645161d8"], brc29KeyID) as never);
    expect(JSON.parse(internalized(bag)[0]!.outputs[1]!.insertionRemittance!.customInstructions!).keyID).toBe(HELD.keyID);
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

/** A fee input of `kind` for the page rule (no wallet needed). */
function feeInput(n: number, o: { satoshis?: number; tokenId?: string; amount?: bigint }): FeeInput {
  const token = o.tokenId !== undefined;
  return {
    txid: n.toString(16).padStart(64, "0"),
    vout: 0,
    satoshis: o.satoshis ?? (token ? 1 : 0),
    lockingScript: "00",
    protocolID: [1, "openex fees"],
    keyID: HELD.keyID,
    counterparty: "self",
    kind: token ? "token" : "sats",
    ...(token ? { tokenId: o.tokenId, amount: o.amount ?? 1n } : {}),
  };
}

describe("composePages (0.54.0: the page rule)", () => {
  const A = `${"0a".repeat(32)}_0`;
  const B = `${"0b".repeat(32)}_0`;
  const script = (tokenId: string, amount: bigint) => tokenP2pkhScript(tokenId, amount, PUB);
  const satsOf = (p: { inputs: FeeInput[] }) => p.inputs.filter((i) => i.kind === "sats");
  const tokensOf = (p: { inputs: FeeInput[] }) => p.inputs.filter((i) => i.kind === "token");
  /** The page's sats cover its estimate, and without its last (smallest) sats row they would not. */
  function coversMinimally(p: { inputs: FeeInput[]; tokens: { tokenId: string; amount: bigint }[] }, rate: number) {
    const sats = satsOf(p).reduce((a, i) => a + i.satoshis, 0);
    const scripts = p.tokens.map((t) => script(t.tokenId, t.amount));
    expect(sats).toBeGreaterThanOrEqual(estimatePageFee(p.inputs.length, scripts, rate));
    expect(sats - satsOf(p).at(-1)!.satoshis).toBeLessThan(estimatePageFee(p.inputs.length - 1, scripts, rate));
  }

  it("250 token rows first (by token id), each page with as many sats rows (largest first) as cover its estimate; the sats left in sats-only pages of 250", () => {
    expect(COLLECT_PAGE_TOKEN_INPUTS).toBe(250);
    const fees = [
      ...Array.from({ length: 60 }, (_, n) => feeInput(n, { tokenId: B, amount: 2n })),
      ...Array.from({ length: 200 }, (_, n) => feeInput(100 + n, { tokenId: A, amount: 3n })),
      ...Array.from({ length: 600 }, (_, n) => feeInput(1000 + n, { satoshis: 1000 + n })),
    ];
    const pages = composePages(fees, 200, script);
    expect(pages.map((p) => tokensOf(p).length)).toEqual([250, 10, 0, 0, 0]);
    expect(pages[0]!.tokens).toEqual([{ tokenId: A, amount: 600n }, { tokenId: B, amount: 100n }]);
    expect(pages[1]!.tokens).toEqual([{ tokenId: B, amount: 20n }]);
    coversMinimally(pages[0]!, 200);
    coversMinimally(pages[1]!, 200);
    // Largest first: page 1 took the largest sats rows.
    expect(satsOf(pages[0]!)[0]!.satoshis).toBe(1599);
    const used = satsOf(pages[0]!).length + satsOf(pages[1]!).length;
    expect(pages.slice(2).map((p) => p.inputs.length)).toEqual([250, 250, 600 - used - 500]);
    expect(pages.slice(2).every((p) => p.tokens.length === 0 && tokensOf(p).length === 0)).toBe(true);
    expect(pages.flatMap((p) => p.inputs)).toHaveLength(860);
  });

  it("sats run out before tokens: the later pages are tokens only", () => {
    const fees = [...Array.from({ length: 600 }, (_, n) => feeInput(n, { tokenId: A })), feeInput(9999, { satoshis: 100_000 })];
    const pages = composePages(fees, 200, script);
    expect(pages.map((p) => [tokensOf(p).length, satsOf(p).length])).toEqual([[250, 1], [250, 0], [100, 0]]);
  });

  it("the fee rate decides how many sats rows a token page takes", () => {
    const fees = [...Array.from({ length: 250 }, (_, n) => feeInput(n, { tokenId: A })), ...Array.from({ length: 100 }, (_, n) => feeInput(1000 + n, { satoshis: 1000 }))];
    const at = (rate: number) => satsOf(composePages(fees, rate, script)[0]!).length;
    expect(at(100)).toBeLessThan(at(200));
    expect(at(200)).toBeLessThan(at(400));
    coversMinimally(composePages(fees, 400, script)[0]!, 400);
  });

  it("only sats: sats-only pages, largest first; rows carrying nothing are not spent", () => {
    const fees = [feeInput(1, { satoshis: 5 }), feeInput(2, { satoshis: 0 }), feeInput(3, { satoshis: 50 }), feeInput(4, { tokenId: A, amount: 0n })];
    expect(composePages(fees, 200, script).map((p) => p.inputs.map((i) => i.satoshis))).toEqual([[50, 5]]);
  });
});

describe("collectBasket (0.54.0): pages in sequence", () => {
  const OTHER_TOKEN = `${"ab".repeat(32)}_0`;
  const satsScript = new P2PKH().lock(payee.toAddress()).toHex();
  const specs = JSON.stringify({ protocolID: [1, "openex fees"], keyID: HELD.keyID, counterparty: "self", poolId: POOL_ID });
  // One source transaction: a token fee (7 units) and three sats fees.
  const src = new Transaction();
  src.addInput({ sourceTXID: "46".repeat(32), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
  src.addOutput({ satoshis: 1, lockingScript: LockingScript.fromHex(tokenP2pkhScript(TOKEN_ID, 7n, PUB)) });
  for (const sats of [5000, 4000, 3000]) src.addOutput({ satoshis: sats, lockingScript: LockingScript.fromHex(satsScript) });
  const rowsOf = (): Row[] => src.outputs.map((o, n) => ({ outpoint: `${src.id("hex")}.${n}`, satoshis: o.satoshis!, lockingScript: o.lockingScript.toHex(), customInstructions: specs }));

  /** The signing wallet: createAction hands back a transaction of the named inputs and outputs; signAction "broadcasts" it, the spent rows leave the basket. `refuse`: the createAction call that throws. */
  function collectWallet(refuse?: number) {
    const bag = syncWallet({ fees: rowsOf(), feeTxs: [src] });
    const created: CreateActionArgs[] = [];
    let pending: { tx: Transaction; args: CreateActionArgs } | undefined;
    Object.assign(bag.wallet, {
      async createAction(args: CreateActionArgs) {
        created.push(args);
        if (created.length === refuse) throw new Error("The user denied the request: color_send TST");
        const beef = Beef.fromBinary(Array.from(args.inputBEEF!));
        const tx = new Transaction();
        for (const i of args.inputs ?? []) {
          const [txid, vout] = i.outpoint.split(".");
          tx.addInput({ sourceTXID: txid!, sourceOutputIndex: Number(vout), sourceTransaction: beef.findTxid(txid!)!.tx!, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
        }
        for (const o of args.outputs ?? []) tx.addOutput({ satoshis: o.satoshis, lockingScript: LockingScript.fromHex(o.lockingScript) });
        pending = { tx, args };
        return { signableTransaction: { tx: tx.toAtomicBEEF(true), reference: `ref-${created.length}` } };
      },
      async createSignature() {
        return { signature: [0x30, 0x06, 0x02, 0x01, 0x01, 0x02, 0x01, 0x01] };
      },
      async signAction() {
        const { tx, args } = pending!;
        const spent = new Set((args.inputs ?? []).map((i) => i.outpoint));
        bag.fees.splice(0, bag.fees.length, ...bag.fees.filter((r) => !spent.has(r.outpoint)));
        return { txid: tx.id("hex"), tx: tx.toAtomicBEEF(true) };
      },
    });
    return { ...bag, created };
  }

  it("a token page (its sats rows cover the estimate) then a sats-only page; no sats output on any page; the gross amounts and txids", async () => {
    const bag = collectWallet();
    const r = await collectBasket(bag.wallet);
    expect(bag.created).toHaveLength(2);
    const [p1, p2] = bag.created;
    expect(p1!.inputs!.map((i) => i.outpoint)).toEqual([`${src.id("hex")}.0`, `${src.id("hex")}.1`]);
    expect(p1!.outputs).toEqual([{ lockingScript: tokenP2pkhScript(TOKEN_ID, 7n, PUB), satoshis: 1, outputDescription: "Pool fees in tokens" }]);
    expect(ColorPlan.fromLabels(p1!.labels)).toMatchObject({ assets: [TOKEN_ID], operations: [{ assetId: TOKEN_ID, operation: "transfer" }] });
    expect(p2!.inputs!.map((i) => i.outpoint)).toEqual([`${src.id("hex")}.2`, `${src.id("hex")}.3`]);
    expect(p2!.outputs).toEqual([]);
    expect(p2).not.toHaveProperty("labels");
    // No sats output from us on any page, and no take-in of one.
    for (const page of bag.created) for (const o of page.outputs ?? []) expect(o.satoshis).toBe(1);
    expect(bag.calls.some((c) => c.method === "internalizeAction")).toBe(false);
    expect(r.txids).toHaveLength(2);
    expect(r).toMatchObject({ txid: r.txids[1], pages: 2, sats: 12000n, tokens: [{ tokenId: TOKEN_ID, amount: 7n }], remaining: { sats: 0n, tokens: [], outputs: 0 } });
    expect(r).not.toHaveProperty("refused");
  });

  it("page 2 refused: page 1's result comes back with `refused` (the wallet's text) and `remaining`; nothing thrown", async () => {
    const bag = collectWallet(2);
    const r = await collectBasket(bag.wallet);
    expect(r.txids).toHaveLength(1);
    expect(r).toMatchObject({
      txid: r.txids[0],
      pages: 2,
      refused: { page: 2, error: "The user denied the request: color_send TST" },
      sats: 5000n,
      tokens: [{ tokenId: TOKEN_ID, amount: 7n }],
      remaining: { sats: 7000n, tokens: [], outputs: 2 },
    });
  });

  it("page 1 refused: thrown, nothing spent", async () => {
    const bag = collectWallet(1);
    await expect(collectBasket(bag.wallet)).rejects.toThrow("The user denied the request");
    expect(bag.created).toHaveLength(1);
  });

  it("availableFees: ONE listOutputs of the fees basket; sats rows summed, token rows per token id, every row counted", async () => {
    const rows = [
      ...rowsOf(),
      { outpoint: `${"47".repeat(32)}.0`, satoshis: 1, lockingScript: tokenP2pkhScript(TOKEN_ID, 5n, PUB), customInstructions: specs },
      { outpoint: `${"48".repeat(32)}.0`, satoshis: 1, lockingScript: tokenP2pkhScript(OTHER_TOKEN, 9n, PUB), customInstructions: specs },
    ];
    const bag = syncWallet({ fees: rows });
    const sorted = [{ tokenId: TOKEN_ID, amount: 12n }, { tokenId: OTHER_TOKEN, amount: 9n }].sort((a, b) => (a.tokenId < b.tokenId ? -1 : 1));
    expect(await availableFees(bag.wallet)).toEqual({ sats: 12000n, tokens: sorted, outputs: 6 });
    expect(bag.calls).toEqual([{ method: "listOutputs", args: expect.objectContaining({ basket: FEES_BASKET }) }]);
  });
});
