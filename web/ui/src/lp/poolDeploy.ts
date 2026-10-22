/**
 * Creating a pool from a token in the wallet (skein-amm 0.12.0, David Case
 * 2026-10-21/22: pool deploys move to the BRC-207 wallet module): ONE
 * wallet-built createAction, affirmed first by this skein's validator. The
 * page holds no keys: every key and signature comes from the wallet.
 *
 *   1. The key input: one of the wallet's own value outputs of the token
 *      (`p color assets`), named as the deploy's input 0. The pool's first
 *      ValidatorPubKey is the validator's child of the deploy's first token
 *      input (docs/AMM.md "Keys rotate"), so the page must know that input
 *      before the pool script exists; the module's own selection is not
 *      known in advance. The smallest output that covers the deposit, else
 *      the largest (the module selects the rest).
 *   2. `amm.pool.terms` (the validator and its fees), then `amm.pool.affirm
 *      {pool, satoshis, keyInput}`: the proposed pool output without its
 *      payload, its sats and the key input → the validator's signature,
 *      which becomes the pool output's BRC-162 payload
 *      (`PoolTemplate.lockDeploy(args, state, affirmation)`), checked here
 *      (`PoolTemplate.affirmed`) before the wallet sees it.
 *   3. `createAction`: the key input (`inputPositions` index 0), output 0
 *      the pool (the deposit's tokens to the pool script: a transfer to a
 *      foreign script in the `p color plan`), and, when the key input
 *      carries more than the deposit, output 1 the rest back to a fresh
 *      module key (the plan's `ownedOutputs`); `randomizeOutputs: false`.
 *      The module adds any further token inputs and their token change; the
 *      wallet funds the sats, signs and broadcasts; the module submits to
 *      `tm_color_<assetId>`. The page submits it there too (plain fetch, a
 *      repeat is harmless).
 *   4. The position row: output 0 filed in the exchange's own basket
 *      `amm-pools` (basket insertion; customInstructions the LP key, the
 *      validator and its fees).
 *
 * Keys:
 *  - LP key: a BRC-29 key (src/wallet/brc29.ts) — `getPublicKey({protocolID:
 *    [2, "3241645161d8"], keyID: "<derivationPrefix> <derivationSuffix>",
 *    counterparty: "self", forSelf: true})` with derivationPrefix =
 *    base64("amm-lp") and derivationSuffix = base64("<txid>_<vout>") of the
 *    key input. Close pays the pool's BSV to Hash160 of it, so the payout is
 *    a wallet payment the wallet internalizes (sender = the user's own
 *    identity). Pools created before have a 1sat-sdk LP key (`P1SAT_PROTOCOL`,
 *    keyID "amm-lp-<txid>_<vout>", `legacyLpKeyId`).
 *  - Validator key: the anyone-child of the validator's identity for
 *    `1-amm pool-<key input>` (src/lib/keys.ts), a public derivation.
 */
import { P2PKH, PublicKey, Transaction, Utils, type CreateActionOutput, type InternalizeOutput, type WalletInterface, type WalletProtocol } from "@bsv/sdk";
import type { ColorRemittance } from "@bsv/color-permission-module";
import { PoolTemplate, decodeOneColor, oneColorValuePrefix, type PoolArgs, type PoolFields } from "../pool";
import { deriveValidatorPubKey, validatorKeyId } from "../lib/keys";
import { priceOf } from "../market/view";
import { callApp, dagBytes, readBytes, type AuthFetchLike } from "../market/relay";
import { BRC29_PROTOCOL, brc29KeyID, identityKeyOf } from "../wallet/brc29";
import { POOLS_BASKET, newReceivingKey, runTokenAction, transferPlanLabels, type HeldOutput } from "../wallet/color";
import { POOL_OP, POOL_TAG, isPoolRow } from "./poolRows";
import { parseOutpoint, parseTokenId, sdkTokenId, tokenIdOfWire, tokenIdText } from "../lib/tokenId";
import { submitToOverlay, steakText } from "./close";

export { POOL_OP, POOL_TAG, isPoolRow };

/** The validator a pool names: its identity key (compressed, lowercase hex). */
export interface ValidatorChoice {
  identityKey: string;
}

/** The keyID prefix of the pre-BRC-29 LP keys (`P1SAT_PROTOCOL`). */
export const LP_KEY_PREFIX = "amm-lp-";
/** The LP key's protocol: BRC-29 (see the module comment). */
export const LP_KEY_PROTOCOL: WalletProtocol = BRC29_PROTOCOL;
const LP_DERIVATION_PREFIX = Utils.toBase64(Utils.toArray("amm-lp", "utf8"));
/** The validator's default terms (amm-validator's, `config.amm.ammValidator`); a deploy takes the skein's own from `amm.pool.terms`. */
export const DEFAULT_LP_FEE_BPS = 30n;
export const DEFAULT_VALIDATOR_FEE_BPS = 5n;
/** `amm.pool.affirm` (amm-p2p, 0.11.0). */
export const POOL_AFFIRM_FN = "amm.pool.affirm";
/** A P2PKH unlocking script's upper bound: push(DER ≤ 72 + sighash byte) + push(33-byte key). */
const P2PKH_UNLOCK_LENGTH = 108;

const toHex = (b: number[] | Uint8Array) => Utils.toHex(Array.from(b));

function describe(text: string): string {
  return text.length <= 50 ? text : `${text.slice(0, 49)}…`;
}

/** The LP key's BRC-29 derivation for a pool output created by a transaction whose key input spends `outpoint` (`<txid>_<vout>` or `txid.vout`). */
export function lpDerivation(outpoint: string): { derivationPrefix: string; derivationSuffix: string } {
  const [txid, vout] = outpoint.split(/[._]/);
  return { derivationPrefix: LP_DERIVATION_PREFIX, derivationSuffix: Utils.toBase64(Utils.toArray(`${txid}_${vout}`, "utf8")) };
}

/** The LP key's keyID (`LP_KEY_PROTOCOL`) for `outpoint`: `"<derivationPrefix> <derivationSuffix>"`. */
export function lpKeyId(outpoint: string): string {
  return brc29KeyID(lpDerivation(outpoint));
}

/** The pre-BRC-29 LP keyID (`P1SAT_PROTOCOL`): `amm-lp-<txid>_<vout>`. */
export function legacyLpKeyId(outpoint: string): string {
  const [txid, vout] = outpoint.split(/[._]/);
  return `${LP_KEY_PREFIX}${txid}_${vout}`;
}

/** A basket row as the wallet lists it. */
export interface BasketRow {
  outpoint: string;
  satoshis: number;
  lockingScript?: string;
  tags?: string[];
  customInstructions?: string;
}

// ---------------------------------------------------------------------------
// The key input
// ---------------------------------------------------------------------------

/** One of the wallet's own value outputs of a token, as the module holds it. */
export interface KeyInput {
  /** `txid.vout` */
  outpoint: string;
  txid: string;
  vout: number;
  amount: bigint;
}

/** The wallet's available value outputs of `tokenId` (a fixed-supply deploy output among them), from `p color assets`. */
export function valueOutputsOf(held: HeldOutput[], tokenId: string): KeyInput[] {
  const want = tokenIdText(parseTokenId(tokenId) ?? { txid: "", vout: -1 });
  const out: KeyInput[] = [];
  for (const h of held) {
    if (!h.available) continue;
    const op = parseOutpoint(h.outpoint);
    if (!op) continue;
    // A pool output carries the same 1Color prefix; it is never a key input.
    if (PoolTemplate.decode(h.lockingScript)) continue;
    const t = decodeOneColor(Utils.toArray(h.lockingScript, "hex"));
    if (!t || t.amount <= 0n) continue;
    const id = t.role === "deploy" ? tokenIdText(op) : t.role === "value" && t.idBytes ? tokenIdOfWire(t.idBytes) : null;
    if (id !== want) continue;
    out.push({ outpoint: `${op.txid}.${op.vout}`, txid: op.txid, vout: op.vout, amount: t.amount });
  }
  return out;
}

/**
 * The key input for a deposit of `amount`: the smallest output carrying at
 * least `amount` (its rest comes back as the plan's owned output), else the
 * largest (the module selects more inputs and adds token change). Null when
 * the outputs together carry less.
 */
export function selectKeyInput(outputs: KeyInput[], amount: bigint): { input: KeyInput; rest: bigint } | null {
  if (amount <= 0n || outputs.length === 0) return null;
  if (outputs.reduce((a, o) => a + o.amount, 0n) < amount) return null;
  const sorted = [...outputs].sort((a, b) => (a.amount < b.amount ? -1 : a.amount > b.amount ? 1 : 0));
  const cover = sorted.find((o) => o.amount >= amount);
  if (cover) return { input: cover, rest: cover.amount - amount };
  return { input: sorted[sorted.length - 1]!, rest: 0n };
}

// ---------------------------------------------------------------------------
// The plan (pure)
// ---------------------------------------------------------------------------

export interface PoolDeployForm {
  tokenId: string;
  /** The deploy's input 0 (`selectKeyInput`): the pool's first validator key is keyed by it. */
  keyInput: { txid: string; vout: number };
  tokens: bigint;
  sats: bigint;
  lpFeeBps: bigint;
  validatorFeeBps: bigint;
  /** The relay's commission (the contract's CommissionBps), fixed at deploy. Default 0. */
  commissionBps?: bigint;
  lpPubKey: string;
  validator: ValidatorChoice;
  dec?: number;
}

export interface PoolDeployPlan {
  tokenId: string;
  /** `tm_color_<assetId>`, `tm_color_<txid>_0`: the token's topic (BRC-207). */
  topic: string;
  args: PoolArgs;
  state: PoolFields;
  /** Output 0's script without the affirmation, hex: what `amm.pool.affirm` is asked about. */
  lockingScript: string;
  /** The key input, `<txid>_<vout>`. */
  keyInput: string;
  /** `1-amm pool-<keyInput>`'s key ID. */
  validatorKeyId: string;
  lpKeyId: string;
  sats: bigint;
  /** Initial marginal price, sats per token (display units when `dec` is known). */
  price: string;
}

export class PoolDeployError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "PoolDeployError";
  }
}

/** A token with a 32-byte id (`<txid>_0`; `<txid>` / `<txid>.0` read the same) → the wire id (internal byte order), hex. */
export function assetIdHex(tokenId: string): string {
  const r = parseTokenId(tokenId);
  if (!r || r.vout !== 0) throw new PoolDeployError(`pools exist only for 1Color tokens with a 32-byte id (<txid>_0), not ${tokenId}`);
  return toHex((Utils.toArray(r.txid, "hex") as number[]).reverse());
}

export function planPoolDeploy(f: PoolDeployForm): PoolDeployPlan {
  const assetId = assetIdHex(f.tokenId);
  if (f.tokens <= 0n) throw new PoolDeployError("the token deposit must be positive");
  if (f.sats <= 0n || f.sats > BigInt(Number.MAX_SAFE_INTEGER)) throw new PoolDeployError("the sats deposit must be positive");
  const commissionBps = f.commissionBps ?? 0n;
  for (const [name, bps] of [["LP fee", f.lpFeeBps], ["validator fee", f.validatorFeeBps], ["commission", commissionBps]] as const) {
    if (bps < 0n || bps >= 10_000n) throw new PoolDeployError(`${name}: 0-9999 bps`);
  }
  if (f.lpFeeBps + f.validatorFeeBps + commissionBps >= 10_000n) throw new PoolDeployError("the fees together must stay below 100%");
  const key = { txid: f.keyInput.txid, vout: f.keyInput.vout };
  const args: PoolArgs = { assetId, lpFeeBps: f.lpFeeBps, validatorFeeBps: f.validatorFeeBps, commissionBps };
  const state: PoolFields = {
    tokenReserve: f.tokens,
    lpPubKey: PublicKey.fromString(f.lpPubKey).toString(),
    validatorPubKey: deriveValidatorPubKey(f.validator.identityKey, key),
    validatorIdentity: PublicKey.fromString(f.validator.identityKey).toString(),
  };
  return {
    tokenId: f.tokenId,
    topic: `tm_color_${sdkTokenId(f.tokenId)}`,
    args,
    state,
    lockingScript: PoolTemplate.lockDeploy(args, state).toHex(),
    keyInput: validatorKeyId(key),
    validatorKeyId: validatorKeyId(key),
    lpKeyId: lpKeyId(`${key.txid}_${key.vout}`),
    sats: f.sats,
    price: priceOf(f.sats, f.tokens, f.dec),
  };
}

/** A 1Color value output of `amount` to `pubKeyHash` (P2PKH), as Pool.runar.go's `tokenP2pkh` writes it. */
export function tokenP2pkhScript(tokenId: string, amount: bigint, pubKey: string): string {
  const prefix = oneColorValuePrefix(Utils.toArray(assetIdHex(tokenId), "hex"), amount);
  return toHex(prefix) + new P2PKH().lock(PublicKey.fromString(pubKey).toAddress()).toHex();
}

/** customInstructions of a pool output the wallet holds as LP (`amm-pools`). */
export function poolCustomInstructions(o: {
  tokenId: string;
  sym?: string;
  dec?: number;
  protocolID: WalletProtocol;
  keyID: string;
  args: PoolArgs;
  validatorIdentity: string;
}): string {
  return JSON.stringify({
    id: o.tokenId,
    op: POOL_OP,
    ...(o.sym ? { sym: o.sym } : {}),
    ...(o.dec !== undefined ? { dec: String(o.dec) } : {}),
    protocolID: o.protocolID,
    keyID: o.keyID,
    counterparty: "self",
    amm: { role: "lp", validatorIdentity: o.validatorIdentity, lpFeeBps: String(o.args.lpFeeBps), validatorFeeBps: String(o.args.validatorFeeBps), commissionBps: String(o.args.commissionBps) },
  });
}

// ---------------------------------------------------------------------------
// The affirmation
// ---------------------------------------------------------------------------

/**
 * `amm.pool.affirm {pool, satoshis, keyInput}` (docs/AMM.md "The deploy and
 * the affirmation") → the pool output's script with the validator's
 * signature as its payload, checked against the plan before it is used.
 */
export async function affirmPool(af: AuthFetchLike, base: string, plan: PoolDeployPlan): Promise<{ affirmation: string; lockingScript: string }> {
  const r = (await callApp(af, base, POOL_AFFIRM_FN, {
    pool: dagBytes(Utils.toArray(plan.lockingScript, "hex")),
    satoshis: Number(plan.sats),
    keyInput: plan.keyInput,
  })) as Record<string, unknown> | null;
  const sig = readBytes(r?.signature);
  if (!sig || sig.length === 0) throw new PoolDeployError(`${POOL_AFFIRM_FN}: no signature in the answer`);
  const affirmation = toHex(sig);
  const lockingScript = PoolTemplate.lockDeploy(plan.args, plan.state, affirmation).toHex();
  if (!PoolTemplate.affirmed(lockingScript, plan.sats)) throw new PoolDeployError(`${POOL_AFFIRM_FN}: the signature does not affirm this pool (not the ValidatorPubKey's over its parameters)`);
  return { affirmation, lockingScript };
}

// ---------------------------------------------------------------------------
// The wallet sequence
// ---------------------------------------------------------------------------

/** The LP key for a deposit whose key input is `keyInput`. */
export async function deriveLpKey(wallet: WalletInterface, keyInput: { txid: string; vout: number }): Promise<{ protocolID: WalletProtocol; keyID: string; publicKey: string }> {
  const protocolID = LP_KEY_PROTOCOL;
  const keyID = lpKeyId(`${keyInput.txid}_${keyInput.vout}`);
  const { publicKey } = await wallet.getPublicKey({ protocolID, keyID, counterparty: "self", forSelf: true });
  return { protocolID, keyID, publicKey };
}

export class DeployShapeError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "DeployShapeError";
  }
}

/** How an output of ours is taken into a basket after the fact (`internalizeAction` basket insertion). */
export interface BasketFiling {
  outputIndex: number;
  basket: string;
  tags: string[];
  customInstructions: string;
}

export interface DeployPoolInput {
  wallet: WalletInterface;
  /** The wallet's BRC-104 client (`amm.pool.affirm` on `/call`). */
  af: AuthFetchLike;
  /** The AMM app's base URL. */
  base: string;
  tokenId: string;
  tokens: bigint;
  sats: bigint;
  fees: { lpFeeBps: bigint; validatorFeeBps: bigint; commissionBps: bigint };
  validator: ValidatorChoice;
  /** The wallet's held token outputs (`p color assets`). */
  held: HeldOutput[];
  meta?: { sym?: string; dec?: number };
  /** The submit to this skein (default: `POST <base>/submit`, plain fetch). */
  submit?: (topic: string, beef: number[]) => Promise<{ steak?: unknown }>;
}

export interface DeployedPool {
  plan: PoolDeployPlan;
  lpKey: { protocolID: WalletProtocol; keyID: string; publicKey: string };
  affirmation: string;
  /** The createAction arguments the wallet was given. */
  createArgs: Parameters<WalletInterface["createAction"]>[0];
  txid: string;
  /** The deploy as the wallet's AtomicBEEF. */
  tx: number[];
  /** `<txid>.0` */
  outpoint: string;
  /** The position row (`amm-pools`). */
  filing: BasketFiling;
  filed: boolean;
  /** The page's own submit, in words (src/lp/close.ts `steakText`), or its error. */
  submitted: string;
  /** Filing or submit errors (the deploy itself is broadcast). */
  errors: string[];
}

/** Steps 1-4 (see the module comment). Throws before the wallet's action exists; after it, errors are collected. */
export async function deployPool(i: DeployPoolInput): Promise<DeployedPool> {
  const { wallet } = i;
  const meta = i.meta ?? {};
  const sdkId = sdkTokenId(i.tokenId);
  assetIdHex(i.tokenId);
  // 1. The key input.
  const sel = selectKeyInput(valueOutputsOf(i.held, i.tokenId), i.tokens);
  if (!sel) throw new PoolDeployError(`Your wallet holds fewer than ${i.tokens} base units of that token`);
  const lpKey = await deriveLpKey(wallet, sel.input);
  const plan = planPoolDeploy({
    tokenId: i.tokenId,
    keyInput: sel.input,
    tokens: i.tokens,
    sats: i.sats,
    ...i.fees,
    lpPubKey: lpKey.publicKey,
    validator: i.validator,
    ...(meta.dec !== undefined ? { dec: meta.dec } : {}),
  });

  // 2. The affirmation.
  const { affirmation, lockingScript } = await affirmPool(i.af, i.base, plan);

  // 3. The deploy: the pool, and the key input's rest to a fresh module key of our own.
  const sym = meta.sym ?? "token";
  const outputs: CreateActionOutput[] = [{ lockingScript, satoshis: Number(plan.sats), outputDescription: describe(`AMM pool: ${sym} / BSV`) }];
  const ownedOutputs: ColorRemittance[] = [];
  if (sel.rest > 0n) {
    const me = await identityKeyOf(wallet);
    const k = await newReceivingKey(wallet, sdkId, me);
    outputs.push({ lockingScript: tokenP2pkhScript(sdkId, sel.rest, k.publicKey), satoshis: 1, outputDescription: describe(`${sym} kept back from the pool`) });
    ownedOutputs.push({ outputIndex: 1, ...k.receipt });
  }
  const createArgs = {
    description: describe(`AMM pool deploy: ${sym}`),
    labels: [...transferPlanLabels(sdkId, { ownedOutputs, inputPositions: [{ outpoint: sel.input.outpoint, index: 0 }] }), "amm-pool-deploy"],
    inputs: [{ outpoint: sel.input.outpoint, unlockingScriptLength: P2PKH_UNLOCK_LENGTH, inputDescription: describe(`${sym}: the pool's key input`) }],
    outputs,
    options: { randomizeOutputs: false, acceptDelayedBroadcast: false },
  };
  const { txid, tx } = await runTokenAction(wallet, createArgs);

  const out: DeployedPool = {
    plan,
    lpKey,
    affirmation,
    createArgs,
    txid,
    tx,
    outpoint: `${txid}.0`,
    filing: {
      outputIndex: 0,
      basket: POOLS_BASKET,
      tags: [POOL_TAG],
      customInstructions: poolCustomInstructions({ tokenId: sdkId, ...meta, protocolID: lpKey.protocolID, keyID: lpKey.keyID, args: plan.args, validatorIdentity: plan.state.validatorIdentity }),
    },
    filed: false,
    submitted: "not submitted",
    errors: [],
  };
  const final = Transaction.fromAtomicBEEF(tx);
  const in0 = final.inputs[0];
  if (final.outputs[0]?.lockingScript.toHex() !== lockingScript) out.errors.push("the wallet's deploy has another output 0 than the affirmed pool");
  if (!in0 || `${in0.sourceTXID}.${in0.sourceOutputIndex}` !== sel.input.outpoint) out.errors.push(`the wallet's deploy does not spend the key input ${sel.input.outpoint} first: the validator will not sign this pool's swaps (Close it)`);

  // The page's own submit to this skein (the module submitted at the wallet's overlay).
  try {
    const submit = i.submit ?? ((topic: string, beef: number[]) => submitToOverlay(i.base, topic, beef));
    out.submitted = steakText((await submit(plan.topic, tx)) as never);
  } catch (err) {
    out.submitted = "the submit failed";
    out.errors.push(`submit: ${err instanceof Error ? err.message : String(err)}`);
  }

  // 4. The position row.
  const row: InternalizeOutput = { outputIndex: 0, protocol: "basket insertion", insertionRemittance: { basket: out.filing.basket, tags: out.filing.tags, customInstructions: out.filing.customInstructions } };
  try {
    const r = await wallet.internalizeAction({ tx, outputs: [row], description: describe(`AMM pool deploy: ${sym}`), labels: ["amm-pool-deploy"] });
    out.filed = r.accepted;
  } catch (err) {
    out.errors.push(`internalizeAction: ${err instanceof Error ? err.message : String(err)}`);
  }
  return out;
}
