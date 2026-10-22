/**
 * Close through the user's wallet: the LP's alone, so nothing is relayed: the
 * page builds it, signs it and submits it to the overlay.
 *
 * Layout 2 (skein-amm 0.13.0, David Case 2026-10-22: `Close(lpSig)`, outputs
 * free): the LP's wallet chooses where everything goes — the sats (less the
 * fee) as a BRC-29 wallet payment to the LP key (to self), every token to a
 * NEW module receiving key of the user's own (`newReceivingKey`, sender the
 * user's identity), taken in through the module with its receipt
 * (`receiptOutput`): no `amm-swap`, no Move back.
 *
 * Layout 1 (a pool deployed before 0.13.0: `Close(sig, bsvFee)`): the
 * contract's outputs, everything to the LP key; the tokens, which no module key
 * holds, are filed in the active swap basket `amm-swap` and come back with Your
 * tokens' Move back (0.12.0, unchanged).
 *
 *   1. The pool's current output: followed from the position's deploy by the
 *      chain state (`GET <base>/spends`, src/lp/positions.ts), its BEEF from
 *      the token's 1Color lookup (`ls_color {txid, outputIndex}`): the
 *      listing (`ls_amm`) answers listed pools only (a rescinded pool too is
 *      closed from here).
 *   2. `PoolTemplate.planClose`: the outputs (the BSV payout, the pool's sats
 *      less `bsvFee`, when nonzero; every token, one sat) and the method args.
 *   3. `bsvFee` > 0: the miner fee and the token output's sat come from the
 *      pool (no other input; `bsvFee` must cover them). `bsvFee` = 0: a
 *      **funding transaction** (`createFunding`, src/market/swapAction.ts,
 *      broadcast by the wallet at once: `noSend: false`) with one exact output
 *      — the token output's sat plus the close's miner fee at the rate — and
 *      no change.
 *   4. **Close transaction**, built here: input 0 the pool (`callUnlock`; the
 *      LP's slot filled with `createSignature` under the pool's LP key), the
 *      funding output (`createSignature`, ALL|FORKID). Checked with `Spend`.
 *   5. `POST <base>/submit` (`x-topics: tm_color_<assetId>`), the close's
 *      AtomicBEEF (the funding as its unproven parent): BRC-22's STEAK.
 *   6. `completeClose`: the BSV payout (a BRC-29 wallet payment) and the
 *      tokens (layout 2: the module's receipt; layout 1: `amm-swap`) taken in,
 *      then `relinquishOutput` of the funding output and the pool row.
 *
 * A layout-1 pool whose LP key predates BRC-29 LP keys (`P1SAT_PROTOCOL`,
 * `amm-lp-…`) cannot take its BSV payout as a wallet payment (the contract
 * pays that key): refused before anything is signed.
 */
import { P1SAT_PROTOCOL } from "@1sat/actions";
import { DEPOSIT_BASKET } from "@1sat/types";
import { LockingScript, PublicKey, Transaction, UnlockingScript, type InternalizeOutput, type WalletInterface, type WalletProtocol } from "@bsv/sdk";
import { PoolTemplate, type CallPlan } from "../pool";
import { SWAP_TTL_MS, createFunding, poolUtxoFrom, signP2pkhWithWallet, spendValid, swapFunding, type Funding, type SwapFunding } from "../market/swapAction";
import { needSigned, type LookupOutput, type SignedFetch } from "../lib/overlay";
import { Utils } from "@bsv/sdk";
import type { BasketFiling } from "./poolDeploy";
import type { LpKeyRef } from "./positions";
import { WALLET_PAYMENT, identityKeyOf, isBrc29Protocol, splitBrc29KeyID, type Brc29Payout, type PaymentRemittance } from "../wallet/brc29";
import { sdkTokenId } from "../lib/tokenId";
import { POOLS_BASKET, SWAP_BASKET, newReceivingKey, receiptOutput, swapInstructions, type ColorReceipt } from "../wallet/color";
import { tokenP2pkhScript } from "./poolDeploy";
import { payoutId, type PendingPayoutStore } from "../wallet/pendingPayouts";

export class CloseShapeError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "CloseShapeError";
  }
}

export interface CloseInput {
  wallet: WalletInterface;
  /** `<txid>_0` (src/lib/tokenId.ts). */
  tokenId: string;
  meta?: { sym?: string; dec?: number };
  /** The pool's current output (step 1). */
  poolOutput: LookupOutput;
  lpKey: LpKeyRef;
  /** The miner fee left from the pool's sats; 0: funded from the wallet. */
  bsvFee: bigint;
  /** sats per 1000 bytes (`VITE_FEE_RATE`). */
  satsPerKb: number;
  now?: number;
  ttlMs?: number;
}

export interface PreparedClose {
  plan: CallPlan;
  /** The pool output spent, `txid.vout`. */
  poolOutpoint: string;
  /** Broadcast by the wallet; null when `bsvFee` pays the fee. */
  funding: (SwapFunding & Funding) | null;
  bsvFee: bigint;
  /** Every input signed. */
  tx: Transaction;
  txid: string;
  /** The close's AtomicBEEF: the `/submit` body. */
  beef: number[];
  /** `tm_color_<assetId>` */
  topic: string;
  /** The pool's layout: 2 (0.13.0, the LP's outputs) or 1 (the contract's). */
  version: 1 | 2;
  /** The BSV payout as a BRC-29 payment (null when bsvFee takes every sat). */
  payout: Brc29Payout | null;
  /** Layout 1: the tokens into `amm-swap`. */
  tokens: BasketFiling | null;
  /** Layout 2: the tokens to a module receiving key of the user's own, taken in with this receipt. */
  receipt: { outputIndex: number; receipt: ColorReceipt; lockingScript: string } | null;
  /** What returns to the wallet. */
  sats: bigint;
  tokenAmount: bigint;
}

/** The remittance of a payment to the LP key `lpKey` (a BRC-29 key), or null for a pre-BRC-29 LP key. */
export function lpRemittance(lpKey: LpKeyRef, senderIdentityKey: string): PaymentRemittance | null {
  if (!isBrc29Protocol(lpKey.protocolID) || lpKey.counterparty !== "self") return null;
  const parts = splitBrc29KeyID(lpKey.keyID);
  return parts ? { ...parts, senderIdentityKey } : null;
}

export const LEGACY_LP_KEY = "this pool's LP key predates BRC-29 LP keys: the contract pays its BSV to that key, which the wallet cannot take in as a payment";

const P1SAT = P1SAT_PROTOCOL as WalletProtocol;

/** The close's size: the pool input at its call's length (the LP's signature in), `others` P2PKH inputs, the contract's outputs. */
function closeSize(plan: CallPlan, others: number): number {
  return swapFunding(plan, [], 1000).size - (1 - others) * (36 + 1 + 108 + 4);
}

/** Steps 2-4 (see the module comment). */
export async function prepareClose(i: CloseInput): Promise<PreparedClose> {
  const { wallet } = i;
  const poolUtxo = poolUtxoFrom(i.poolOutput);
  const pool = PoolTemplate.decode(poolUtxo.script);
  if (!pool) throw new CloseShapeError("the output is not a pool");
  const sats = BigInt(poolUtxo.satoshis);
  if (i.bsvFee < 0n || i.bsvFee > sats) throw new CloseShapeError(`the fee must be 0 to the pool's ${sats} sats`);
  let remittance: PaymentRemittance | null = null;
  if (sats - i.bsvFee > 0n) {
    if (!isBrc29Protocol(i.lpKey.protocolID)) throw new CloseShapeError(LEGACY_LP_KEY);
    remittance = lpRemittance(i.lpKey, await identityKeyOf(wallet));
    if (!remittance) throw new CloseShapeError(`the LP key ${i.lpKey.keyID} is not a BRC-29 key to self`);
  }
  const lpArgs = { protocolID: i.lpKey.protocolID, keyID: i.lpKey.keyID, counterparty: i.lpKey.counterparty };
  const { publicKey: lpPub } = await wallet.getPublicKey({ ...lpArgs, forSelf: true });
  if (PublicKey.fromString(lpPub).toString() !== pool.state.lpPubKey) throw new CloseShapeError(`the wallet key ${i.lpKey.keyID} is not this pool's LP key`);

  const sdkId = sdkTokenId(i.tokenId);
  // Layout 2: the LP's outputs — the sats (less bsvFee) to the LP key, the tokens to a fresh
  // module key of the user's own. Layout 1: the contract's.
  let receipt: PreparedClose["receipt"] = null;
  let free: { satoshis: bigint; script: string }[] | undefined;
  if (pool.version === 2) {
    const k = await newReceivingKey(wallet, sdkId, await identityKeyOf(wallet));
    const tokenScript = tokenP2pkhScript(sdkId, pool.state.tokenReserve, k.publicKey);
    free = [];
    if (sats - i.bsvFee > 0n) free.push({ satoshis: sats - i.bsvFee, script: `76a914${Utils.toHex(PublicKey.fromString(lpPub).toHash() as number[])}88ac` });
    receipt = { outputIndex: free.length, receipt: k.receipt, lockingScript: tokenScript };
    free.push({ satoshis: 1n, script: tokenScript });
  }
  const plan = PoolTemplate.planClose({ pool: poolUtxo, bsvFee: i.bsvFee, ...(free ? { outputs: free } : {}) });
  let at = 0;
  const payout: Brc29Payout | null = sats - i.bsvFee > 0n ? { outputIndex: at, satoshis: Number(plan.outputs[at]!.satoshis), lockingScript: plan.outputs[at++]!.script, remittance: remittance! } : null;
  const tokens: BasketFiling | null =
    pool.version === 1
      ? {
          outputIndex: at,
          basket: SWAP_BASKET,
          tags: [],
          customInstructions: swapInstructions({ assetId: sdkId, amount: pool.state.tokenReserve, protocolID: i.lpKey.protocolID, keyID: i.lpKey.keyID, counterparty: i.lpKey.counterparty }),
        }
      : null;

  // Step 3: the fee from the pool, or a funding output.
  const expires = (i.now ?? Date.now()) + (i.ttlMs ?? SWAP_TTL_MS);
  let funding: (SwapFunding & Funding) | null = null;
  if (i.bsvFee === 0n) {
    const amounts = swapFunding(plan, [], i.satsPerKb);
    const f = await createFunding(wallet, {
      satoshis: amounts.satoshis,
      expires,
      amm: { close: `${poolUtxo.txid}_${poolUtxo.vout}` },
      description: `AMM close funding: ${i.meta?.sym ?? "token"}`,
      labels: ["amm-close"],
      outputDescription: `AMM close funding: ${i.meta?.sym ?? "token"}`,
      noSend: false,
    });
    funding = { ...amounts, ...f };
  } else {
    const need = Math.ceil((closeSize(plan, 0) * i.satsPerKb) / 1000) + 1;
    if (i.bsvFee < BigInt(need)) throw new CloseShapeError(`the fee must cover the miner fee and the token output's sat: at least ${need} sats`);
  }

  // Step 4: the close.
  const tx = new Transaction();
  tx.version = 1;
  tx.lockTime = 0;
  tx.addInput({ sourceTXID: poolUtxo.txid, sourceOutputIndex: poolUtxo.vout, sourceTransaction: poolUtxo.sourceTransaction, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
  if (funding) tx.addInput({ sourceTXID: funding.txid, sourceOutputIndex: 0, sourceTransaction: funding.tx, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
  for (const o of plan.outputs) tx.addOutput({ satoshis: Number(o.satoshis), lockingScript: LockingScript.fromHex(o.script) });
  tx.inputs[0]!.unlockingScript = PoolTemplate.callUnlock(plan, tx, null).unlockingScript;
  await PoolTemplate.signPoolInput(
    { tx, pool: plan.pool, method: "close", unsigned: ["lp"], sourceSatoshis: poolUtxo.satoshis },
    "lp",
    async (sighash) => (await wallet.createSignature({ ...lpArgs, hashToDirectlySign: sighash })).signature,
  );
  if (funding) tx.inputs[1]!.unlockingScript = await signP2pkhWithWallet(wallet, tx, 1, { satoshis: funding.satoshis, lockingScript: funding.lockingScript, protocolID: P1SAT, keyID: funding.keyID, counterparty: "self" });
  for (let n = 0; n < tx.inputs.length; n++) {
    if (!spendValid(tx, n)) throw new CloseShapeError(`input ${n} of the close does not validate${funding ? ` (the funding ${funding.outpoint} stays in ${DEPOSIT_BASKET})` : ""}`);
  }
  return {
    plan,
    poolOutpoint: `${poolUtxo.txid}.${poolUtxo.vout}`,
    funding,
    bsvFee: i.bsvFee,
    tx,
    txid: tx.id("hex"),
    beef: tx.toAtomicBEEF(true),
    topic: `tm_color_${sdkId}`,
    version: pool.version,
    payout,
    tokens,
    receipt,
    sats: sats - i.bsvFee,
    tokenAmount: pool.state.tokenReserve,
  };
}

export interface CompletedClose {
  internalized: boolean;
  relinquished: string[];
  errors: string[];
}

/**
 * Step 6, after a successful submit: the BSV payout (a BRC-29 wallet payment) and, layout 1,
 * the tokens (a basket insertion into `amm-swap`) in one `internalizeAction`; layout 2, the
 * tokens through the module (`receiptOutput`, a second `internalizeAction`: the module takes in
 * only an admitted output; kept in `payouts` when it does not, for a retry); then the funding
 * output and the pool row (`deployOutpoint`, the row the deploy filed) relinquished.
 */
export async function completeClose(wallet: WalletInterface, s: PreparedClose, deployOutpoint?: string, payouts?: PendingPayoutStore): Promise<CompletedClose> {
  const out: CompletedClose = { internalized: false, relinquished: [], errors: [] };
  const outputs: InternalizeOutput[] = [];
  if (s.payout) outputs.push({ outputIndex: s.payout.outputIndex, protocol: WALLET_PAYMENT, paymentRemittance: { ...s.payout.remittance } });
  if (s.tokens) outputs.push({ outputIndex: s.tokens.outputIndex, protocol: "basket insertion", insertionRemittance: { basket: s.tokens.basket, tags: s.tokens.tags, customInstructions: s.tokens.customInstructions } });
  let ok = true;
  if (outputs.length > 0) {
    try {
      const r = await wallet.internalizeAction({ tx: s.beef, outputs, description: "AMM close", labels: ["amm-close"] });
      ok = r.accepted;
    } catch (err) {
      ok = false;
      out.errors.push(`internalizeAction: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
  if (s.receipt) {
    const pending = {
      id: payoutId(s.txid, s.receipt.outputIndex),
      kind: "close" as const,
      txid: s.txid,
      vout: s.receipt.outputIndex,
      satoshis: 1,
      lockingScript: s.receipt.lockingScript,
      receipt: s.receipt.receipt,
      final: true,
      tokenId: s.receipt.receipt.assetId,
      atomicBeef: Utils.toHex(s.beef),
      description: "AMM close: the tokens",
      createdAt: Date.now(),
    };
    payouts?.save(pending);
    try {
      const r = await wallet.internalizeAction({ tx: s.beef, outputs: [receiptOutput(s.receipt.outputIndex, s.receipt.receipt)], description: "AMM close: the tokens", labels: ["amm-close"] });
      ok = ok && r.accepted;
      if (r.accepted) payouts?.remove(pending.id);
    } catch (err) {
      ok = false;
      out.errors.push(`internalizeAction (the tokens): ${err instanceof Error ? err.message : String(err)}`);
    }
  }
  out.internalized = ok;
  const rel = [...(s.funding ? [{ basket: DEPOSIT_BASKET, output: s.funding.outpoint }] : []), ...(deployOutpoint ? [{ basket: POOLS_BASKET, output: deployOutpoint }] : [])];
  for (const r of rel) {
    try {
      await wallet.relinquishOutput(r);
      out.relinquished.push(r.output);
    } catch (err) {
      out.errors.push(`relinquishOutput ${r.output}: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
  return out;
}

/** BRC-22's STEAK: per topic, the outputs admitted and the coins retained and removed. */
export type Steak = Record<string, { outputsToAdmit: number[]; coinsToRetain: number[]; coinsRemoved: number[] }>;

/**
 * Submit to the instance (BRC-22; skein-overlay 0.9.1, docs/OVERLAY.md "Submitting"): `POST
 * <base>/submit` (`x-topics` the token topic, the body the BEEF) waits on the submission and
 * answers the STEAK; 503 with Retry-After when nothing is decided within the host's bound (the
 * submission stands: `steak` absent). Plain `fetch`, unsigned (0.6.3; David 2026-10-08: "We
 * shouldn't be using authfetch for the submit http method."): the front door (shruggr/skein#135)
 * admits an unsigned POST at the overlay's `submit` row (sender `*`, filter `beef`), and
 * `@bsv/sdk`'s AuthFetch refuses the `x-topics` header client-side.
 */
export async function submitToOverlay(
  base: string,
  topic: string,
  beef: number[],
  fetchFn: (url: string, init: RequestInit) => Promise<Response> = (url, init) => fetch(url, init),
): Promise<{ steak?: Steak }> {
  const res = await fetchFn(`${base}/submit`, {
    method: "POST",
    headers: { "content-type": "application/octet-stream", "x-topics": topic },
    body: new Uint8Array(beef),
  });
  const text = await res.text();
  if (res.status === 503) return {};
  if (!res.ok) throw new Error(`POST ${base}/submit: ${res.status} ${text.slice(0, 300)}`);
  let steak: unknown;
  try {
    steak = JSON.parse(text);
  } catch {
    /* not JSON */
  }
  if (!steak || typeof steak !== "object" || Array.isArray(steak)) throw new Error(`POST ${base}/submit: want the STEAK, got ${text.slice(0, 300)}`);
  return { steak: steak as Steak };
}

/** The STEAK in words: the topics that admitted outputs, else "taken by no topic"; no STEAK, "not decided yet". */
export function steakText(r: { steak?: Steak }): string {
  if (!r.steak) return "not decided yet";
  const took = Object.entries(r.steak).filter(([, e]) => e.outputsToAdmit.length > 0).map(([t, e]) => `${t} (outputs ${e.outputsToAdmit.join(", ")})`);
  return took.length ? `admitted under ${took.join("; ")}` : "taken by no topic";
}

/**
 * Whether the overlay admitted `txid`: `ls_color {txid, outputIndex}` (one 1Color output, if
 * unspent; skein-1color docs/1COLOR.md) asked until it answers the output or `timeoutMs` passes.
 * The engine admits once the chain app accepts the transaction (its broadcast succeeded).
 */
export async function awaitAdmitted(
  af: SignedFetch | null,
  base: string,
  txid: string,
  outputIndex: number,
  o: { timeoutMs?: number; intervalMs?: number } = {},
): Promise<boolean> {
  const signed = needSigned(af);
  const deadline = Date.now() + (o.timeoutMs ?? 60_000);
  for (;;) {
    const res = await signed.fetch(`${base}/lookup`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ service: "ls_color", query: { txid, outputIndex } }),
    });
    if (res.ok) {
      const a = (await res.json()) as { outputs?: unknown[] };
      if (Array.isArray(a.outputs) && a.outputs.length > 0) return true;
    }
    if (Date.now() >= deadline) return false;
    await new Promise((r) => setTimeout(r, o.intervalMs ?? 2_000));
  }
}
