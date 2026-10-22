/**
 * The active swap basket (0.12.0, src/market/activeSwap.ts): the one module
 * send into `amm-swap` before a tokens-in swap's legs, and "Move back" to a
 * module receiving key, against a fake wallet whose keys are a real
 * KeyDeriver (./brc29Wallet.ts) and whose internalizeAction checks a
 * `p color` receipt as the BRC-207 module does.
 */
import { BigNumber, Beef, ECDSA, LockingScript, P2PKH, PrivateKey, Transaction, UnlockingScript, type CreateActionArgs, type WalletInterface } from "@bsv/sdk";
import { colorReceiptInstructions } from "@1sat/actions";
import { ColorPlan, colorProtocolID } from "@bsv/color-permission-module";
import { describe, expect, it } from "vitest";
import { brc29Side, type KeyArgs } from "./brc29Wallet";
import { moveBack, sendToSwapBasket, swapTotals, p2pkhTxSize } from "../src/market/activeSwap";
import { spendValid, tokenInputsOf } from "../src/market/swapAction";
import { tokenP2pkhScript } from "../src/lp/poolDeploy";
import { PendingPayoutStore, internalizeNow, type KV } from "../src/wallet/pendingPayouts";
import v from "./fixtures/amm-topic-vectors.json";

const key = (n: number) => new PrivateKey(n.toString(16).padStart(2, "0").repeat(32), 16);
const fund = Transaction.fromHex(v.fund);
const fundKey = key(10); // the fixture's funds (output 0)
const tokenDeploy = Transaction.fromHex(v.token_deploy);
const TOKEN = `${tokenDeploy.id("hex")}_0`;
const TOPIC = `tm_color_${TOKEN}`;
const side = brc29Side(key(60));
const SWAP: [2, string] = [2, "amm swap"];

/** An `amm-swap` row of `amount`, its source transaction, under keyID `keyID`. */
function swapRow(amount: bigint, keyID: string) {
  const tx = new Transaction();
  tx.addInput({ sourceTXID: "77".repeat(32), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
  tx.addOutput({ satoshis: 1, lockingScript: LockingScript.fromHex(tokenP2pkhScript(TOKEN, amount, side.publicKey({ protocolID: SWAP, keyID, counterparty: "self", forSelf: true }))) });
  return {
    tx,
    row: {
      outpoint: `${tx.id("hex")}.0`,
      satoshis: 1,
      spendable: true,
      lockingScript: tx.outputs[0]!.lockingScript.toHex(),
      customInstructions: JSON.stringify({ assetId: TOKEN, amount: String(amount), protocolID: SWAP, keyID, counterparty: "self" }),
    },
  };
}

/** A fake wallet: honest keys; `amm-swap` holds `rows`; createAction broadcasts at once (the module's send, or a funding). */
function fakeWallet(rows: ReturnType<typeof swapRow>[] = []) {
  const calls: { method: string; args: unknown }[] = [];
  const w = {
    async getPublicKey(a: KeyArgs & { identityKey?: boolean }) {
      calls.push({ method: "getPublicKey", args: a });
      return { publicKey: a.identityKey ? side.identityKey : side.publicKey(a) };
    },
    async createSignature(a: KeyArgs & { hashToDirectlySign: number[] }) {
      calls.push({ method: "createSignature", args: a });
      return { signature: ECDSA.sign(new BigNumber(a.hashToDirectlySign), side.privateKey(a), true).toDER() as number[] };
    },
    async createAction(args: CreateActionArgs) {
      calls.push({ method: "createAction", args });
      const tx = new Transaction();
      tx.addInput({ sourceTXID: fund.id("hex"), sourceOutputIndex: 0, sourceTransaction: fund, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
      for (const o of args.outputs ?? []) tx.addOutput({ satoshis: o.satoshis, lockingScript: LockingScript.fromHex(o.lockingScript) });
      tx.addOutput({ satoshis: 500, lockingScript: new P2PKH().lock(fundKey.toAddress()) });
      tx.inputs[0]!.unlockingScript = await new P2PKH().unlock(fundKey).sign(tx, 0);
      return { txid: tx.id("hex"), tx: tx.toAtomicBEEF(true) };
    },
    async listOutputs(args: { basket: string; include?: string }) {
      calls.push({ method: "listOutputs", args });
      if (args.basket !== "amm-swap") return { totalOutputs: 0, outputs: [] };
      const b = new Beef();
      for (const r of rows) b.mergeTransaction(r.tx);
      return { totalOutputs: rows.length, outputs: rows.map((r) => r.row), ...(args.include === "entire transactions" ? { BEEF: b.toBinary() } : {}) };
    },
    async internalizeAction(args: never) {
      calls.push({ method: "internalizeAction", args });
      return side.internalizeAction(args);
    },
    async relinquishOutput(args: unknown) {
      calls.push({ method: "relinquishOutput", args });
      return { relinquished: true };
    },
    async abortAction(args: unknown) {
      calls.push({ method: "abortAction", args });
      return { aborted: true };
    },
  };
  return { wallet: w as unknown as WalletInterface, calls };
}

const memKV = (): KV => {
  const m = new Map<string, string>();
  return { getItem: (k) => m.get(k) ?? null, setItem: (k, val) => void m.set(k, val) };
};

describe("into the swap basket: ONE module send, one output per uncovered leg, filed in amm-swap", () => {
  it("keys under [2, \"amm swap\"] (self, a fresh keyID each); the plan a transfer out; the page's submit; each output filed with its key", async () => {
    const { wallet, calls } = fakeWallet();
    const submitted: string[] = [];
    const s = await sendToSwapBasket(wallet, "http://x/amm", TOKEN, [30n, 12n], { sym: "TST" }, async (topic) => (submitted.push(topic), {}));
    const keys = calls.filter((c) => c.method === "getPublicKey").map((c) => c.args as KeyArgs);
    expect(keys.map((k) => [k.protocolID, k.counterparty, k.forSelf])).toEqual([[SWAP, "self", true], [SWAP, "self", true]]);
    expect(new Set(keys.map((k) => k.keyID)).size).toBe(2);
    for (const k of keys) expect(k.keyID).toMatch(/^amm-swap-[0-9a-f]{16}$/);

    const create = calls.find((c) => c.method === "createAction")!.args as CreateActionArgs;
    expect(create.inputs).toBeUndefined(); // the module selects the token inputs
    expect(create.options).toEqual({ randomizeOutputs: false, acceptDelayedBroadcast: false });
    expect(create.labels!.at(-1)).toBe("amm-swap");
    expect(ColorPlan.fromLabels(create.labels)).toEqual({ version: 1, assets: [TOKEN], ownedOutputs: [], operations: [{ assetId: TOKEN, operation: "transfer" }], inputPositions: [], changePositions: [], sighashTypes: [] });
    expect(create.outputs!.map((o) => [o.satoshis, o.lockingScript])).toEqual(
      keys.map((k, n) => [1, tokenP2pkhScript(TOKEN, [30n, 12n][n]!, side.publicKey({ ...k }))]),
    );
    expect(create.outputs!.every((o) => o.basket === undefined)).toBe(true); // a destination, not a p color basket
    expect(submitted).toEqual([TOPIC]);

    expect(s.holdings.map((h) => [h.outpoint, h.amount, h.keyID])).toEqual([
      [`${s.txid}.0`, 30n, keys[0]!.keyID],
      [`${s.txid}.1`, 12n, keys[1]!.keyID],
    ]);
    const ia = calls.find((c) => c.method === "internalizeAction")!.args as { tx: number[]; outputs: { outputIndex: number; insertionRemittance: { basket: string; customInstructions: string } }[]; labels: string[] };
    expect(ia.tx).toEqual(s.tx);
    expect(ia.labels).toEqual(["amm-swap"]);
    expect(ia.outputs.map((o) => [o.outputIndex, o.insertionRemittance.basket])).toEqual([[0, "amm-swap"], [1, "amm-swap"]]);
    expect(JSON.parse(ia.outputs[1]!.insertionRemittance.customInstructions)).toEqual({ assetId: TOKEN, amount: "12", protocolID: SWAP, keyID: keys[1]!.keyID, counterparty: "self" });
    expect(s).toMatchObject({ filed: true, submitted: "not decided yet", errors: [] });

    // Filed rows read back as the legs' token inputs, and as the page's totals.
    const tx = Transaction.fromAtomicBEEF(s.tx);
    const rows = ia.outputs.map((o) => ({ outpoint: `${s.txid}.${o.outputIndex}`, satoshis: 1, lockingScript: tx.outputs[o.outputIndex]!.lockingScript.toHex(), customInstructions: o.insertionRemittance.customInstructions }));
    expect(tokenInputsOf(rows, TOKEN).map((t) => [t.amount, t.protocolID, t.keyID])).toEqual([[30n, SWAP, keys[0]!.keyID], [12n, SWAP, keys[1]!.keyID]]);
    expect(await swapTotals(fakeWallet([{ tx, row: { ...rows[0]!, spendable: true } }]).wallet)).toEqual(new Map([[TOKEN, { amount: 30n, outputs: 1 }]]));
  });

  it("nothing to send, or a wallet that refuses: an error, nothing filed", async () => {
    const { wallet, calls } = fakeWallet();
    await expect(sendToSwapBasket(wallet, "http://x/amm", TOKEN, [])).rejects.toThrow(/nothing to send/);
    (wallet as unknown as { createAction: () => Promise<never> }).createAction = async () => {
      throw new Error("BRC-207: the user declined the color_send request");
    };
    await expect(sendToSwapBasket(wallet, "http://x/amm", TOKEN, [5n], {}, async () => ({}))).rejects.toThrow(/declined/);
    expect(calls.some((c) => c.method === "internalizeAction")).toBe(false);
  });
});

describe("move back: every amm-swap output of the token to a module receiving key of the user's own", () => {
  it("[holdings, funding] → one 1Color output to the receiving key; inputs valid; submitted; taken in with the receipt (sender: the user); spent outputs relinquished", async () => {
    const a = swapRow(30n, "amm-swap-a");
    const b = swapRow(12n, "amm-swap-b");
    const { wallet, calls } = fakeWallet([a, b]);
    const store = new PendingPayoutStore(memKV());
    const submitted: { topic: string; beef: number[] }[] = [];
    const r = await moveBack({
      wallet,
      af: null,
      base: "http://x/amm",
      tokenId: TOKEN,
      meta: { sym: "TST" },
      satsPerKb: 100,
      payouts: store,
      submit: async (topic, beef) => (submitted.push({ topic, beef }), { steak: { [topic]: { outputsToAdmit: [0], coinsToRetain: [], coinsRemoved: [0, 1] } } }),
    });
    expect(r.amount).toBe(42n);
    const tx = Transaction.fromAtomicBEEF(r.beef);
    expect(tx.id("hex")).toBe(r.txid);
    expect(tx.inputs.map((i) => `${i.sourceTXID}.${i.sourceOutputIndex}`).slice(0, 2)).toEqual([a.row.outpoint, b.row.outpoint]);
    expect(tx.inputs).toHaveLength(3);
    for (let n = 0; n < 3; n++) expect(spendValid(tx, n)).toBe(true);
    expect(tx.outputs).toHaveLength(1);
    // The funding pays the fee beyond the two inputs' sats; the output keeps 1 sat.
    const fee = Math.ceil((p2pkhTxSize([{ script: tx.outputs[0]!.lockingScript.toHex() }], 3) * 100) / 1000);
    const create = calls.find((c) => c.method === "createAction")!.args as CreateActionArgs;
    expect(create.outputs![0]!.satoshis).toBe(Math.max(1, 1 + fee - 2));
    expect(create.options).toEqual({ randomizeOutputs: false, acceptDelayedBroadcast: false });
    expect(submitted.map((x) => x.topic)).toEqual([TOPIC]);
    expect(r.submitted).toBe(`admitted under ${TOPIC} (outputs 0)`);

    const ia = calls.find((c) => c.method === "internalizeAction")!.args as { outputs: { outputIndex: number; protocol: string; insertionRemittance: { basket: string; customInstructions: string } }[] };
    const receipt = JSON.parse(ia.outputs[0]!.insertionRemittance.customInstructions) as { assetId: string; derivationPrefix: string; derivationSuffix: string; senderIdentityKey: string };
    expect(ia.outputs).toEqual([{ outputIndex: 0, protocol: "basket insertion", insertionRemittance: { basket: `p color ${TOKEN.slice(0, 64)}v0`, customInstructions: colorReceiptInstructions({ assetId: TOKEN, derivationPrefix: receipt.derivationPrefix, derivationSuffix: receipt.derivationSuffix, senderIdentityKey: side.identityKey }) } }]);
    const recv = side.publicKey({ protocolID: colorProtocolID(TOKEN, receipt.derivationPrefix, receipt.derivationSuffix), keyID: "color", counterparty: side.identityKey, forSelf: true });
    expect(tx.outputs[0]!.lockingScript.toHex()).toBe(tokenP2pkhScript(TOKEN, 42n, recv));
    expect(r.internalized).toBe(true);
    expect(calls.filter((c) => c.method === "relinquishOutput").map((c) => c.args)).toEqual([
      { basket: "amm-swap", output: a.row.outpoint },
      { basket: "amm-swap", output: b.row.outpoint },
      { basket: "1sat-deposit", output: `${tx.inputs[2]!.sourceTXID}.0` },
    ]);
    expect(r.errors).toEqual([]);
    expect(store.list()).toEqual([]);
  });

  it("not admitted yet: kept as a pending payout (its receipt and the BEEF), taken in on a retry", async () => {
    const a = swapRow(9n, "amm-swap-c");
    const { wallet, calls } = fakeWallet([a]);
    const store = new PendingPayoutStore(memKV());
    const r = await moveBack({ wallet, af: null, base: "http://x/amm", tokenId: TOKEN, satsPerKb: 100, payouts: store, submit: async () => ({}), admitted: async () => false });
    expect(r.internalized).toBe(false);
    expect(r.errors).toEqual(["the token topic has not admitted the move back yet: your wallet takes it in on a retry"]);
    expect(calls.some((c) => c.method === "internalizeAction")).toBe(false);
    const [rec] = store.list();
    expect(rec).toMatchObject({ kind: "move-back", txid: r.txid, vout: 0, final: true, tokenId: TOKEN, receipt: { assetId: TOKEN, senderIdentityKey: side.identityKey } });
    expect(rec!.remittance).toBeUndefined();
    // The retry: the BEEF the record holds, no lookup; the module's receive with the receipt.
    expect(await internalizeNow(wallet, store, null, "http://x/amm", rec!)).toEqual({ accepted: true, txid: r.txid });
    const ia = calls.find((c) => c.method === "internalizeAction")!.args as { outputs: { insertionRemittance: { customInstructions: string } }[] };
    expect(ia.outputs[0]!.insertionRemittance.customInstructions).toBe(colorReceiptInstructions(rec!.receipt!));
    expect(store.list()).toEqual([]);
  });

  it("nothing in the swap basket: an error before any action", async () => {
    const { wallet, calls } = fakeWallet([]);
    await expect(moveBack({ wallet, af: null, base: "http://x/amm", tokenId: TOKEN, satsPerKb: 100 })).rejects.toThrow(/holds no token/);
    expect(calls.some((c) => c.method === "createAction")).toBe(false);
  });
});
