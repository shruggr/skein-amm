/**
 * The LP page's pool half: the pool deploy plan against amm-topic's fixture
 * pool deploy; the deploy (0.12.0: the validator's affirmation, then ONE
 * createAction through the wallet's BRC-207 module, the pool filed in
 * `amm-pools`) against a fake wallet and a fake `/call`; and Close (the fee
 * from the pool or a broadcast funding, page-built close validated against
 * the fixture pool, submit, internalize + relinquish).
 */
import {
  BigNumber,
  Beef,
  ECDSA,
  LockingScript,
  P2PKH,
  PrivateKey,
  PublicKey,
  Spend,
  Transaction,
  UnlockingScript,
  Utils,
  type CreateActionArgs,
  type WalletInterface,
} from "@bsv/sdk";
import { P1SAT_PROTOCOL } from "@1sat/actions";
import { ColorPlan, colorProtocolID } from "@bsv/color-permission-module";
import { afterEach, describe, expect, it, vi } from "vitest";
import { PoolTemplate } from "../src/pool";
import { type SignedFetch } from "../src/lib/overlay";
import {
  POOL_TAG,
  assetIdHex,
  deployPool,
  isPoolRow,
  legacyLpKeyId,
  lpKeyId,
  planPoolDeploy,
  selectKeyInput,
  tokenP2pkhScript,
  valueOutputsOf,
  type BasketRow,
} from "../src/lp/poolDeploy";
import { LEGACY_LP_KEY, awaitAdmitted, completeClose, prepareClose, steakText, submitToOverlay } from "../src/lp/close";
import { dagBytes, readBytes, type AuthFetchLike } from "../src/market/relay";
import { BRC29, brc29Side, isBrc29, type KeyArgs } from "./brc29Wallet";
import { tokenInputsOf } from "../src/market/swapAction";
import v from "./fixtures/amm-topic-vectors.json";

// Keys and transactions of programs/amm-topic/gen/main.go.
const key = (n: number) => new PrivateKey(n.toString(16).padStart(2, "0").repeat(32), 16);
const lpKey = key(10);
const pub = (k: PrivateKey) => k.toPublicKey().toString();
const fund = Transaction.fromHex(v.fund);
const tokenDeploy = Transaction.fromHex(v.token_deploy);
const poolDeploy = Transaction.fromHex(v.pool_deploy);
/** The pool output before the validator's affirmation (what `amm.pool.affirm` is asked about). */
const unaffirmedPool = Transaction.fromHex(v.pool_deploy_unaffirmed).outputs[0]!.lockingScript.toHex();
const swap2 = Transaction.fromHex(v.swap_tokens_in);
const DEPLOY_TXID = tokenDeploy.id("hex");
// The page's token id and the module's asset id are the same `<txid>_0` (BRC-162 Token identification, David 2026-10-07).
const TOKEN_ID = `${DEPLOY_TXID}_0`;
const SDK_ID = TOKEN_ID;
const P1SAT = P1SAT_PROTOCOL as unknown as [number, string];
const walletRoot = key(50); // the fake wallet's root: identity key and honest derivations

afterEach(() => vi.unstubAllGlobals());

/** The wallet's held deploy output, as `p color assets` lists it (10,000,000 units). */
const heldDeploy = { outpoint: `${DEPLOY_TXID}.0`, satoshis: 1, lockingScript: tokenDeploy.outputs[0]!.lockingScript.toHex(), available: true };

describe("pool deploy: plan", () => {
  it("the fixture's key input gives the fixture's pool output, byte for byte (before and after the affirmation)", () => {
    const outs = valueOutputsOf([heldDeploy], TOKEN_ID);
    expect(outs).toEqual([{ outpoint: `${DEPLOY_TXID}.0`, txid: DEPLOY_TXID, vout: 0, amount: 10_000_000n }]);
    const sel = selectKeyInput(outs, 5_000_000n)!;
    expect(sel.rest).toBe(5_000_000n); // no claim's unit any more (0.11.0)
    const plan = planPoolDeploy({
      tokenId: TOKEN_ID,
      keyInput: sel.input,
      tokens: 5_000_000n,
      sats: 1_000_000n,
      lpFeeBps: BigInt(v.lpFeeBps),
      validatorFeeBps: BigInt(v.validatorFeeBps),
      commissionBps: BigInt(v.commissionBps),
      lpPubKey: pub(lpKey),
      validator: { identityKey: v.identity },
    });
    expect(plan.lockingScript).toBe(unaffirmedPool);
    expect(PoolTemplate.lockDeploy(plan.args, plan.state, v.affirmation).toHex()).toBe(poolDeploy.outputs[0]!.lockingScript.toHex());
    expect(plan.state.validatorPubKey).toBe(v.pool0.validator);
    expect(plan.keyInput).toBe(`${DEPLOY_TXID}_0`);
    expect(plan.validatorKeyId).toBe(`${DEPLOY_TXID}_0`);
    // BRC-29 LP key: prefix base64("amm-lp"), suffix base64("<key input>").
    expect(plan.lpKeyId).toBe(`YW1tLWxw ${Utils.toBase64(Utils.toArray(`${DEPLOY_TXID}_0`, "utf8"))}`);
    expect(legacyLpKeyId(`${DEPLOY_TXID}.0`)).toBe(`amm-lp-${DEPLOY_TXID}_0`);
    expect(plan.args.assetId).toBe(Utils.toHex(tokenDeploy.hash() as number[]));
    expect(plan.topic).toBe(`tm_color_${DEPLOY_TXID}_0`);
    expect(plan.price).toBe("0.2");
    expect(PoolTemplate.decode(plan.lockingScript)!.state).toEqual(plan.state);
  });

  it("refusals: legacy id, zero deposits, fees", () => {
    expect(() => assetIdHex(`${DEPLOY_TXID}_1`)).toThrow(/32-byte id/);
    const base = { tokenId: TOKEN_ID, keyInput: { txid: DEPLOY_TXID, vout: 0 }, tokens: 1n, sats: 1n, lpFeeBps: 30n, validatorFeeBps: 5n, lpPubKey: pub(lpKey), validator: { identityKey: v.identity } };
    expect(() => planPoolDeploy({ ...base, tokens: 0n })).toThrow(/token deposit/);
    expect(() => planPoolDeploy({ ...base, sats: 0n })).toThrow(/sats deposit/);
    expect(() => planPoolDeploy({ ...base, lpFeeBps: 10_000n })).toThrow(/LP fee/);
    expect(() => planPoolDeploy({ ...base, commissionBps: -1n })).toThrow(/commission/);
    expect(() => planPoolDeploy({ ...base, lpFeeBps: 5_000n, commissionBps: 5_000n })).toThrow(/below 100%/);
    // Commission defaults to 0, and is the constructor's param 7.
    expect(planPoolDeploy(base).args.commissionBps).toBe(0n);
    expect(PoolTemplate.decode(planPoolDeploy({ ...base, commissionBps: 25n }).lockingScript)!.args.commissionBps).toBe(25n);
  });

  it("the key input: the smallest output that covers the deposit (the rest comes back), else the largest (the module selects the rest); null when short; only available value outputs of the token", () => {
    const mk = (amount: bigint, n: number) => ({ outpoint: `${"ab".repeat(32)}.${n}`, txid: "ab".repeat(32), vout: n, amount });
    const c = [mk(5n, 0), mk(3n, 1), mk(2n, 2)];
    expect(selectKeyInput(c, 4n)).toEqual({ input: c[0], rest: 1n });
    expect(selectKeyInput(c, 3n)).toEqual({ input: c[1], rest: 0n });
    expect(selectKeyInput(c, 1n)).toEqual({ input: c[2], rest: 1n });
    expect(selectKeyInput(c, 7n)).toEqual({ input: c[0], rest: 0n });
    expect(selectKeyInput(c, 10n)).toEqual({ input: c[0], rest: 0n });
    expect(selectKeyInput(c, 11n)).toBeNull();
    expect(selectKeyInput(c, 0n)).toBeNull();
    // Reserved outputs, other tokens and pool outputs are not offered.
    const pool = { outpoint: `${poolDeploy.id("hex")}.0`, satoshis: 1_000_000, lockingScript: poolDeploy.outputs[0]!.lockingScript.toHex(), available: true };
    const other = { ...heldDeploy, outpoint: `${"cd".repeat(32)}.0`, lockingScript: tokenP2pkhScript(`${"ef".repeat(32)}_0`, 9n, pub(lpKey)) };
    expect(valueOutputsOf([{ ...heldDeploy, available: false }, other], TOKEN_ID)).toEqual([]);
    expect(valueOutputsOf([pool], TOKEN_ID)).toEqual([]);
  });

  it("pool rows (amm-pools) are never token inputs", () => {
    const poolRow: BasketRow = {
      outpoint: `${poolDeploy.id("hex")}.0`,
      satoshis: 1_000_000,
      lockingScript: poolDeploy.outputs[0]!.lockingScript.toHex(),
      tags: [POOL_TAG],
      customInstructions: JSON.stringify({ id: SDK_ID, op: "amm-pool", protocolID: BRC29, keyID: lpKeyId(`${DEPLOY_TXID}_0`), counterparty: "self" }),
    };
    expect(isPoolRow(poolRow)).toBe(true);
    expect(tokenInputsOf([poolRow], TOKEN_ID)).toEqual([]);
  });
});

/**
 * A fake BRC-100 wallet over the fixture keys. getPublicKey / createSignature:
 * a funding key (`amm-funding-…`) is derived with a real KeyDeriver over
 * `walletRoot` (./brc29Wallet.ts); any other key is the one `keys` names
 * (default lpKey; `honest` derives BRC-29 keys the same real way). The
 * identity key is walletRoot's; internalizeAction checks a wallet payment as
 * wallet-toolbox does. createAction: one input fund:`fundVout` (lpKey's), the
 * requested outputs in order, then a P2PKH change (200 sats of fee); with
 * `signAndProcess: false` it waits for signAction, otherwise it signs and
 * "broadcasts" at once. listOutputs answers the token deploy's BEEF. Records
 * every call.
 */
const side = brc29Side(walletRoot);
/** BRC-29 keys as the wallet derives them; any other request gets `other`. */
const honest = (other: (a: KeyArgs) => PrivateKey = () => key(99)) => (a: KeyArgs) => (isBrc29(a.protocolID) ? side.privateKey(a) : other(a));

function fakeWallet(fundVout: number, keys: (a: KeyArgs) => PrivateKey = () => lpKey) {
  const calls: { method: string; args: unknown }[] = [];
  const keyOf = (a: KeyArgs) => (a.keyID.startsWith("amm-funding-") ? side.privateKey(a) : keys(a));
  let pending: Transaction | undefined;
  const signFunding = async (tx: Transaction) => {
    tx.inputs[0]!.unlockingScript = await new P2PKH().unlock(lpKey).sign(tx, 0);
    for (let n = 1; n < tx.inputs.length; n++) tx.inputs[n]!.unlockingScript = await new P2PKH().unlock(lpKey).sign(tx, n);
    return { txid: tx.id("hex"), tx: tx.toAtomicBEEF(true) };
  };
  const w = {
    async getPublicKey(args: KeyArgs & { identityKey?: boolean }) {
      calls.push({ method: "getPublicKey", args });
      if (args.identityKey) return { publicKey: side.identityKey };
      return { publicKey: pub(keyOf(args)) };
    },
    async createSignature(args: KeyArgs & { hashToDirectlySign: number[] }) {
      calls.push({ method: "createSignature", args });
      return { signature: ECDSA.sign(new BigNumber(args.hashToDirectlySign), keyOf(args), true).toDER() as number[] };
    },
    async createAction(args: CreateActionArgs) {
      calls.push({ method: "createAction", args });
      const tx = new Transaction();
      // The caller's explicit inputs first (as wallet-toolbox places them), then the funding.
      for (const i of args.inputs ?? []) {
        const [txid, vout] = i.outpoint.split(".");
        tx.addInput({ sourceTXID: txid!, sourceOutputIndex: Number(vout), sourceTransaction: sources.get(txid!) ?? tokenDeploy, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
      }
      tx.addInput({ sourceTXID: fund.id("hex"), sourceOutputIndex: fundVout, sourceTransaction: fund, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
      for (const o of args.outputs ?? []) tx.addOutput({ satoshis: o.satoshis, lockingScript: LockingScript.fromHex(o.lockingScript) });
      const outSats = tx.outputs.reduce((a, o) => a + o.satoshis!, 0);
      tx.addOutput({ satoshis: Math.max(1, fund.outputs[fundVout]!.satoshis! - outSats - 200), lockingScript: new P2PKH().lock(lpKey.toAddress()) });
      if (args.options?.signAndProcess === false || (args.inputs ?? []).length > 0) {
        pending = tx;
        return { signableTransaction: { tx: tx.toAtomicBEEF(true), reference: "ref-1" } };
      }
      return signFunding(tx);
    },
    async signAction(args: unknown) {
      calls.push({ method: "signAction", args });
      return signFunding(pending!);
    },
    async listOutputs(args: unknown) {
      calls.push({ method: "listOutputs", args });
      const b = new Beef();
      b.mergeTransaction(tokenDeploy);
      return { totalOutputs: 1, outputs: [], BEEF: b.toBinary() };
    },
    async abortAction(args: unknown) {
      calls.push({ method: "abortAction", args });
      return { aborted: true };
    },
    async relinquishOutput(args: unknown) {
      calls.push({ method: "relinquishOutput", args });
      return { relinquished: true };
    },
    async internalizeAction(args: never) {
      calls.push({ method: "internalizeAction", args });
      return side.internalizeAction(args);
    },
  };
  return { wallet: w as unknown as WalletInterface, calls };
}

function verify(tx: Transaction, i: number): boolean {
  const inp = tx.inputs[i]!;
  const src = inp.sourceTransaction!.outputs[inp.sourceOutputIndex]!;
  return new Spend({
    sourceTXID: inp.sourceTXID!,
    sourceOutputIndex: inp.sourceOutputIndex,
    sourceSatoshis: src.satoshis!,
    lockingScript: src.lockingScript,
    transactionVersion: tx.version,
    otherInputs: tx.inputs.filter((_, j) => j !== i),
    outputs: tx.outputs,
    inputIndex: i,
    unlockingScript: inp.unlockingScript!,
    inputSequence: inp.sequence!,
    lockTime: tx.lockTime,
  }).validate();
}

const RATE = 100;

/** Source transactions of explicit inputs the fake wallet holds besides the token deploy. */
const sources = new Map<string, Transaction>();

function atomic(tx: Transaction, ancestors: Transaction[]): number[] {
  const b = new Beef();
  for (const a of ancestors) b.mergeRawTx(a.toBinary());
  b.mergeRawTx(tx.toBinary());
  return b.toBinaryAtomic(tx.id("hex"));
}

/** The wallet's honest keys, the fixture's LP key for the deposit's BRC-29 LP keyID (so the pool output is the fixture's byte for byte). */
const deployKeys = (a: KeyArgs) => (a.keyID === lpKeyId(`${DEPLOY_TXID}_0`) ? lpKey : side.privateKey(a));

/**
 * A fake `/call` for `amm.pool.affirm`: answers the fixture's affirmation for the fixture's
 * proposed pool (or `answer`), records every request.
 */
function fakeAffirm(answer?: (args: Record<string, unknown>) => { status: number; body: unknown }) {
  const calls: { fn: string; args: Record<string, unknown> }[] = [];
  const af: AuthFetchLike = {
    async fetch(_url, config) {
      const b = JSON.parse(config!.body!) as { fn: string; args: Record<string, unknown> };
      calls.push(b);
      const a = answer ? answer(b.args) : { status: 200, body: { fn: b.fn, result: { signature: dagBytes(Utils.toArray(v.affirmation, "hex")) } } };
      return { status: a.status, text: async () => JSON.stringify(a.body) };
    },
  };
  return { af, calls };
}

async function deploy(w = fakeWallet(0, deployKeys), af = fakeAffirm().af, submitted: { topic: string; beef: number[] }[] = []) {
  const d = await deployPool({
    wallet: w.wallet,
    af,
    base: "http://x/amm",
    tokenId: TOKEN_ID,
    tokens: 5_000_000n,
    sats: 1_000_000n,
    fees: { lpFeeBps: 30n, validatorFeeBps: 5n, commissionBps: BigInt(v.commissionBps) },
    validator: { identityKey: v.identity },
    held: [heldDeploy],
    meta: { sym: "TST", dec: 0 },
    submit: async (topic, beef) => (submitted.push({ topic, beef }), { steak: { [topic]: { outputsToAdmit: [0, 1], coinsToRetain: [], coinsRemoved: [0] } } }),
  });
  return { ...w, d, submitted };
}

describe("pool deploy (0.12.0): the validator's affirmation, then one createAction through the wallet's BRC-207 module", () => {
  it("amm.pool.affirm {pool: the unaffirmed script, satoshis, keyInput}; the affirmed pool at output 0, the key input first, the rest to a module key; a p color plan; filed in amm-pools; submitted to the token topic", async () => {
    const { af, calls: http } = fakeAffirm();
    const submitted: { topic: string; beef: number[] }[] = [];
    const { d, calls } = await deploy(fakeWallet(0, deployKeys), af, submitted);
    expect(http).toEqual([{ fn: "amm.pool.affirm", args: { pool: dagBytes(Utils.toArray(unaffirmedPool, "hex")), satoshis: 1_000_000, keyInput: `${DEPLOY_TXID}_0` } }]);
    expect(d.affirmation).toBe(v.affirmation);

    // The wallet sequence: LP key, identity, the rest's module key; the action; its signing; the filing.
    expect(calls.map((c) => c.method)).toEqual(["getPublicKey", "getPublicKey", "getPublicKey", "createAction", "signAction", "internalizeAction"]);
    const create = calls.find((c) => c.method === "createAction")!.args as CreateActionArgs;
    expect(create.inputs).toEqual([{ outpoint: `${DEPLOY_TXID}.0`, unlockingScriptLength: 108, inputDescription: "TST: the pool's key input" }]);
    expect(create.outputs![0]).toEqual({ lockingScript: poolDeploy.outputs[0]!.lockingScript.toHex(), satoshis: 1_000_000, outputDescription: "AMM pool: TST / BSV" });
    expect(create.options).toEqual({ randomizeOutputs: false, acceptDelayedBroadcast: false });
    expect(create.labels!.at(-1)).toBe("amm-pool-deploy");
    const plan = ColorPlan.fromLabels(create.labels)!;
    expect(plan).toMatchObject({ version: 1, assets: [SDK_ID], operations: [{ assetId: SDK_ID, operation: "transfer" }], inputPositions: [{ outpoint: `${DEPLOY_TXID}.0`, index: 0 }], changePositions: [], sighashTypes: [] });
    // The key input's rest (5,000,000) back to a fresh module key of the user's own: the plan's owned output 1.
    expect(plan.ownedOutputs).toHaveLength(1);
    const own = plan.ownedOutputs[0]!;
    expect(own).toMatchObject({ outputIndex: 1, assetId: SDK_ID, senderIdentityKey: side.identityKey });
    const ownKey = side.publicKey({ protocolID: colorProtocolID(own.assetId, own.derivationPrefix, own.derivationSuffix), keyID: "color", counterparty: side.identityKey, forSelf: true });
    expect(create.outputs![1]).toEqual({ lockingScript: tokenP2pkhScript(SDK_ID, 5_000_000n, ownKey), satoshis: 1, outputDescription: "TST kept back from the pool" });

    // The broadcast deploy: output 0 the affirmed pool; listed (amm-lookup's rule).
    expect(PoolTemplate.affirmed(Transaction.fromAtomicBEEF(d.tx).outputs[0]!.lockingScript, 1_000_000n)).toBe(true);
    expect(d.outpoint).toBe(`${d.txid}.0`);
    expect(d.errors).toEqual([]);
    expect(submitted.map((x) => x.topic)).toEqual([`tm_color_${SDK_ID}`]);
    expect(d.submitted).toBe(`admitted under tm_color_${SDK_ID} (outputs 0, 1)`);

    // The position row: output 0 into amm-pools, the LP key and the validator's terms, no claim.
    const ia = calls.find((c) => c.method === "internalizeAction")!.args as { tx: number[]; outputs: { insertionRemittance: { basket: string; tags: string[]; customInstructions: string } }[]; labels: string[] };
    expect(ia.tx).toEqual(d.tx);
    expect(ia.labels).toEqual(["amm-pool-deploy"]);
    expect(ia.outputs).toEqual([{ outputIndex: 0, protocol: "basket insertion", insertionRemittance: { basket: "amm-pools", tags: ["amm-pool"], customInstructions: d.filing.customInstructions } }]);
    expect(JSON.parse(d.filing.customInstructions)).toEqual({
      id: SDK_ID,
      op: "amm-pool",
      sym: "TST",
      dec: "0",
      protocolID: BRC29,
      keyID: lpKeyId(`${DEPLOY_TXID}_0`),
      counterparty: "self",
      amm: { role: "lp", validatorIdentity: v.identity, lpFeeBps: "30", validatorFeeBps: "5", commissionBps: String(v.commissionBps) },
    });
    expect(d.filed).toBe(true);
    expect(calls.some((c) => c.method === "abortAction" || c.method === "relinquishOutput")).toBe(false);
  });

  it("an exact key input: no owned output; a signature that does not affirm the proposed pool is refused before the wallet's action", async () => {
    // The validator's identity of gen/vectors: it affirms any proposed pool with its child of the key input.
    const identity = key(0x7f);
    expect(pub(identity)).toBe(v.identity);
    const signing = fakeAffirm((args) => {
      const pool = PoolTemplate.decode(readBytes(args.pool)!)!;
      const [txid, vout] = String(args.keyInput).split("_");
      const child = identity.deriveChild(new PrivateKey(1).toPublicKey(), `1-amm pool-${txid}_${vout}`);
      const sig = child.sign(PoolTemplate.affirmationMessage(pool, BigInt(args.satoshis as number)));
      return { status: 200, body: { fn: "amm.pool.affirm", result: { signature: dagBytes(sig.toDER() as number[]) } } };
    });
    const exactTx = new Transaction();
    exactTx.addInput({ sourceTXID: DEPLOY_TXID, sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
    exactTx.addOutput({ satoshis: 1, lockingScript: LockingScript.fromHex(tokenP2pkhScript(SDK_ID, 5_000_000n, pub(lpKey))) });
    const X = exactTx.id("hex");
    sources.set(X, exactTx);
    const exact = { ...heldDeploy, outpoint: `${X}.0`, lockingScript: exactTx.outputs[0]!.lockingScript.toHex() };
    const w = fakeWallet(0, deployKeys);
    const d = await deployPool({ wallet: w.wallet, af: signing.af, base: "http://x/amm", tokenId: TOKEN_ID, tokens: 5_000_000n, sats: 1_000_000n, fees: { lpFeeBps: 30n, validatorFeeBps: 5n, commissionBps: 0n }, validator: { identityKey: v.identity }, held: [heldDeploy, exact], submit: async () => ({}) });
    expect(signing.calls[0]!.args.keyInput).toBe(`${X}_0`);
    const create = w.calls.find((c) => c.method === "createAction")!.args as CreateActionArgs;
    expect(create.outputs).toHaveLength(1);
    expect(ColorPlan.fromLabels(create.labels)!.ownedOutputs).toEqual([]);
    expect(create.inputs![0]!.outpoint).toBe(`${X}.0`);
    expect(d.errors).toEqual([]);
    expect(d.submitted).toBe("not decided yet");

    // The fixture's signature is over the pool keyed by deploy:0, not this one.
    const w2 = fakeWallet(0, deployKeys);
    const refused = deployPool({ wallet: w2.wallet, af: fakeAffirm().af, base: "http://x/amm", tokenId: TOKEN_ID, tokens: 5_000_000n, sats: 1_000_000n, fees: { lpFeeBps: 30n, validatorFeeBps: 5n, commissionBps: 0n }, validator: { identityKey: v.identity }, held: [heldDeploy, exact] });
    await expect(refused).rejects.toThrow(/does not affirm this pool/);
    expect(w2.calls.some((c) => c.method === "createAction")).toBe(false);
  });

  it("the validator's refusal (fees_unacceptable, wrong_validator_key …) stops the deploy before the wallet's action", async () => {
    const w = fakeWallet(0, deployKeys);
    const { af } = fakeAffirm(() => ({ status: 500, body: { fn: "amm.pool.affirm", error: { code: "failed", message: "fees_unacceptable: lpFeeBps 30 is not 25" } } }));
    await expect(deploy(w, af)).rejects.toThrow(/fees_unacceptable/);
    expect(w.calls.some((c) => c.method === "createAction")).toBe(false);
  });

  it("a wallet that refuses the action: the error, nothing filed; a refused signing aborts the action", async () => {
    const w = fakeWallet(0, deployKeys);
    (w.wallet as unknown as { signAction: () => Promise<never> }).signAction = async () => {
      w.calls.push({ method: "signAction", args: {} });
      throw new Error("BRC-207: the user declined the color_send request");
    };
    await expect(deploy(w)).rejects.toThrow(/declined the color_send/);
    expect(w.calls.map((c) => c.method).slice(-2)).toEqual(["signAction", "abortAction"]);
    expect(w.calls.some((c) => c.method === "internalizeAction")).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Close (0.9.0)
// ---------------------------------------------------------------------------

describe("close: the whole pool to the LP, the fee from the pool or a broadcast funding; submit, internalize, relinquish", () => {
  // A pool like the fixture's after swap2, whose LP key is the wallet's BRC-29 LP key for some deposit outpoint.
  const depositOp = `${"33".repeat(32)}_0`;
  const lpRef = { protocolID: BRC29 as never, keyID: lpKeyId(depositOp), counterparty: "self" };
  const lpPub = side.publicKey({ ...lpRef, forSelf: true });
  const base = PoolTemplate.decode(swap2.outputs[0]!.lockingScript)!;
  const poolTx = new Transaction();
  poolTx.addInput({ sourceTXID: "44".repeat(32), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
  poolTx.addOutput({ satoshis: swap2.outputs[0]!.satoshis!, lockingScript: PoolTemplate.lockContinuation(base, { ...base.state, lpPubKey: lpPub }) });
  const poolOutput = () => ({ beef: atomic(poolTx, []), outputIndex: 0 });
  const sats = BigInt(poolTx.outputs[0]!.satoshis!);
  const [derivationPrefix, derivationSuffix] = lpRef.keyID.split(" ") as [string, string];

  it("bsvFee 0: a funding output broadcast by the wallet (no noSend, exact: the token output's sat + fee); [pool, funding] → [every sat, every token] to the LP key; both inputs validate; completeClose files both and relinquishes the funding and the pool row", async () => {
    const { wallet, calls } = fakeWallet(3, honest());
    const s = await prepareClose({ wallet, tokenId: TOKEN_ID, meta: { sym: "TST", dec: 0 }, poolOutput: poolOutput(), lpKey: lpRef, bsvFee: 0n, satsPerKb: RATE, now: 1_000 });
    expect(calls.map((c) => c.method)).toEqual(["getPublicKey", "getPublicKey", "getPublicKey", "createAction", "createSignature", "createSignature", "getPublicKey"]);
    const create = calls.find((c) => c.method === "createAction")!.args as CreateActionArgs;
    expect(create.options).toEqual({ randomizeOutputs: false, acceptDelayedBroadcast: false });
    expect(JSON.parse(create.outputs![0]!.customInstructions!).amm).toEqual({ close: `${poolTx.id("hex")}_0`, expires: 121_000 });
    const f = s.funding!;
    expect(f.outputs).toBe(1);
    expect(f.satoshis).toBe(1 + f.fee);
    const tx = s.tx;
    expect(tx.inputs.map((i) => `${i.sourceTXID}.${i.sourceOutputIndex}`)).toEqual([`${poolTx.id("hex")}.0`, f.outpoint]);
    expect(tx.outputs.map((o) => o.satoshis)).toEqual([Number(sats), 1]);
    expect(tx.outputs[0]!.lockingScript.toHex()).toBe(new P2PKH().lock(PublicKey.fromString(lpPub).toAddress()).toHex());
    for (const i of [0, 1]) expect(verify(tx, i)).toBe(true);
    const ins = tx.inputs.reduce((a, i) => a + i.sourceTransaction!.outputs[i.sourceOutputIndex]!.satoshis!, 0);
    expect(ins - tx.outputs.reduce((a, o) => a + o.satoshis!, 0)).toBe(f.fee);
    expect(s.sats).toBe(sats);
    expect(s.tokenAmount).toBe(base.state.tokenReserve);
    expect(s.payout).toEqual({ outputIndex: 0, satoshis: Number(sats), lockingScript: tx.outputs[0]!.lockingScript.toHex(), remittance: { derivationPrefix, derivationSuffix, senderIdentityKey: side.identityKey } });
    // The tokens (0.12.0): the contract pays them to the LP key, so they go to the active swap basket under it (Move back).
    expect(JSON.parse(s.tokens.customInstructions)).toEqual({ assetId: SDK_ID, amount: String(base.state.tokenReserve), protocolID: BRC29, keyID: lpRef.keyID, counterparty: "self" });
    expect(s.topic).toBe(`tm_color_${DEPLOY_TXID}_0`);

    const before = calls.length;
    const c = await completeClose(wallet, s, `${DEPLOY_TXID}.0`);
    expect(c).toEqual({ internalized: true, relinquished: [f.outpoint, `${DEPLOY_TXID}.0`], errors: [] });
    const after = calls.slice(before);
    expect(after.map((x) => x.method)).toEqual(["internalizeAction", "relinquishOutput", "relinquishOutput"]);
    const ia = after[0]!.args as { tx: number[]; outputs: unknown[]; labels: string[] };
    expect(ia.tx).toEqual(s.beef);
    expect(ia.labels).toEqual(["amm-close"]);
    expect(ia.outputs).toEqual([
      { outputIndex: 0, protocol: "wallet payment", paymentRemittance: s.payout!.remittance },
      { outputIndex: 1, protocol: "basket insertion", insertionRemittance: { basket: "amm-swap", tags: [], customInstructions: s.tokens.customInstructions } },
    ]);
    expect(after.slice(1).map((x) => x.args)).toEqual([
      { basket: "1sat-deposit", output: f.outpoint },
      { basket: "amm-pools", output: `${DEPLOY_TXID}.0` },
    ]);
    // The close's tokens are an amm-swap input the legs and Move back can spend (the LP key signs).
    const row = { outpoint: `${s.txid}.1`, satoshis: 1, lockingScript: s.tx.outputs[1]!.lockingScript.toHex(), customInstructions: s.tokens.customInstructions };
    expect(tokenInputsOf([row], TOKEN_ID)).toEqual([expect.objectContaining({ amount: base.state.tokenReserve, protocolID: BRC29, keyID: lpRef.keyID, counterparty: "self" })]);
  });

  it("bsvFee > 0: no funding (the wallet makes no action); [pool] → [sats − bsvFee, every token]; validates; a fee too small for the miner and the token's sat, or over the pool, is refused", async () => {
    const { wallet, calls } = fakeWallet(3, honest());
    const s = await prepareClose({ wallet, tokenId: TOKEN_ID, poolOutput: poolOutput(), lpKey: lpRef, bsvFee: 1_000n, satsPerKb: RATE });
    expect(s.funding).toBeNull();
    expect(calls.some((c) => c.method === "createAction")).toBe(false);
    expect(s.tx.inputs).toHaveLength(1);
    expect(s.tx.outputs.map((o) => o.satoshis)).toEqual([Number(sats) - 1_000, 1]);
    expect(verify(s.tx, 0)).toBe(true);
    await expect(prepareClose({ wallet, tokenId: TOKEN_ID, poolOutput: poolOutput(), lpKey: lpRef, bsvFee: 2n, satsPerKb: RATE })).rejects.toThrow(/at least/);
    await expect(prepareClose({ wallet, tokenId: TOKEN_ID, poolOutput: poolOutput(), lpKey: lpRef, bsvFee: sats + 1n, satsPerKb: RATE })).rejects.toThrow(/0 to the pool's/);
    // bsvFee = every sat: the tokens only, no wallet payment.
    const all = await prepareClose({ wallet, tokenId: TOKEN_ID, poolOutput: poolOutput(), lpKey: lpRef, bsvFee: sats, satsPerKb: RATE });
    expect(all.payout).toBeNull();
    expect(all.tx.outputs).toHaveLength(1);
    expect(all.tokens.outputIndex).toBe(0);
  });

  it("refuses a wallet key that is not the pool's LP key, and a pre-BRC-29 LP key with a payout, before any funding", async () => {
    const a = fakeWallet(3, () => key(99));
    await expect(prepareClose({ wallet: a.wallet, tokenId: TOKEN_ID, poolOutput: poolOutput(), lpKey: { protocolID: P1SAT as never, keyID: "x", counterparty: "self" }, bsvFee: sats, satsPerKb: RATE })).rejects.toThrow(/not this pool's LP key/);
    expect(a.calls.some((c) => c.method === "createAction")).toBe(false);
    const b = fakeWallet(3);
    await expect(prepareClose({ wallet: b.wallet, tokenId: TOKEN_ID, poolOutput: poolOutput(), lpKey: { protocolID: P1SAT as never, keyID: "lp", counterparty: "self" }, bsvFee: 0n, satsPerKb: RATE })).rejects.toThrow(LEGACY_LP_KEY);
    expect(b.calls).toEqual([]);
  });

  it("the submit (BRC-22, plain fetch): the STEAK, or nothing decided (503); awaitAdmitted asks ls_color", async () => {
    const { wallet } = fakeWallet(3, honest());
    const s = await prepareClose({ wallet, tokenId: TOKEN_ID, poolOutput: poolOutput(), lpKey: lpRef, bsvFee: 1_000n, satsPerKb: RATE });
    const STEAK = { [s.topic]: { outputsToAdmit: [1], coinsToRetain: [], coinsRemoved: [0] } };
    const seen: { url: string; init: RequestInit }[] = [];
    const fetchFn = (async (url: string, init: RequestInit) => {
      seen.push({ url, init });
      if (url.endsWith("/submit")) return new Response(JSON.stringify(STEAK), { status: 200 });
      const q = JSON.parse(init.body as string) as { service: string; query: { txid: string; outputIndex: number } };
      const found = q.service === "ls_color" && q.query.txid === s.txid && seen.length > 2;
      return new Response(JSON.stringify({ type: "output-list", outputs: found ? [{ beef: [], outputIndex: q.query.outputIndex }] : [] }), { status: 200 });
    }) as unknown as SignedFetch["fetch"];
    const r = await submitToOverlay("http://x/amm", s.topic, s.beef, fetchFn as never);
    expect(seen[0]!.url).toBe("http://x/amm/submit");
    expect(seen[0]!.init.headers).toEqual({ "content-type": "application/octet-stream", "x-topics": s.topic });
    expect(r).toEqual({ steak: STEAK });
    expect(steakText(r)).toBe(`admitted under ${s.topic} (outputs 1)`);
    const later = (async () => new Response('{"status":"error"}', { status: 503 })) as unknown as SignedFetch["fetch"];
    expect(await submitToOverlay("http://x/amm", s.topic, s.beef, later as never)).toEqual({});
    expect(await awaitAdmitted({ fetch: fetchFn }, "http://x/amm", s.txid, 1, { intervalMs: 1 })).toBe(true);
  });
});

