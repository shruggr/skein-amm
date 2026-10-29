/**
 * The active swap basket (skein-amm 0.12.0, David Case 2026-10-21/22): the
 * token outputs the swap legs spend, held outside the BRC-207 module in the
 * exchange's own basket `amm-swap` (src/wallet/color.ts).
 *
 *   - **In** (`sendToSwapBasket`): before the legs of a tokens-in swap, ONE
 *     wallet-built send through the module: one 1Color value output per leg
 *     not covered by what `amm-swap` already holds, each exactly the leg's
 *     amount in (the pool takes no token change, so a leg needs inputs that
 *     sum to its amount exactly), locked to a fresh key under `[2, "amm
 *     swap"]` (counterparty self), the plan naming them a transfer out. The
 *     module selects the inputs, adds change, asks once, signs and submits
 *     to `tm_color_<assetId>`; the page submits it to this skein too (a
 *     repeat is harmless), then files each output in `amm-swap` (basket
 *     insertion, customInstructions `{assetId, amount, protocolID, keyID,
 *     counterparty}`).
 *   - **Spent** by the legs exactly as the wallet's token inputs were before:
 *     app-built, `createSignature` under the key the customInstructions name
 *     (src/market/swapAction.ts), relinquished from `amm-swap` once a leg is
 *     accepted. An output a failed leg did not spend stays for a later swap.
 *   - **Move back** (`moveBack`, the recovery path): an app-built
 *     transaction spending every `amm-swap` output of the token (and a
 *     funding output the wallet broadcasts, for the fee) to one module
 *     receiving key (sender: the user's own identity), submitted to this
 *     skein's token topic, then taken in through the module (`internalizeAction`,
 *     basket insertion into `p color <assetTag>`, the receipt), and the spent
 *     outputs relinquished.
 */
import { DEPOSIT_BASKET } from "@1sat/types";
import { P1SAT_PROTOCOL } from "@1sat/actions";
import { Beef, LockingScript, Transaction, UnlockingScript, Utils, type InternalizeOutput, type WalletInterface, type WalletProtocol } from "@bsv/sdk";
import { decodeOneColor } from "../pool";
import { sdkTokenId, tokenIdOfWire } from "../lib/tokenId";
import type { SignedFetch } from "../lib/overlay";
import { basketBeef, basketRows } from "../lp/wallet";
import { tokenP2pkhScript } from "../lp/poolDeploy";
import { awaitAdmitted, steakText, submitToOverlay, type Steak } from "../lp/close";
import { SWAP_BASKET, SWAP_PROTOCOL, newReceivingKey, swapInstructions, receiptOutput, runTokenAction, transferPlanLabels } from "../wallet/color";
import { identityKeyOf } from "../wallet/brc29";
import { payoutId, type PendingPayoutStore } from "../wallet/pendingPayouts";
import { createFunding, signP2pkhWithWallet, spendValid, tokenInputsOf, type TokenInput } from "./swapAction";

/** The keyID prefix of an `amm-swap` key (`SWAP_PROTOCOL`, counterparty self). */
export const SWAP_KEY_PREFIX = "amm-swap-";

const P1SAT = P1SAT_PROTOCOL as WalletProtocol;

function describe(text: string): string {
  return text.length <= 50 ? text : `${text.slice(0, 49)}…`;
}

function randomHex(bytes = 8): string {
  return Utils.toHex(Array.from(crypto.getRandomValues(new Uint8Array(bytes))));
}

/** The `amm-swap` outputs of `tokenId` the legs can spend. */
export async function swapHoldings(wallet: WalletInterface, tokenId: string): Promise<TokenInput[]> {
  return tokenInputsOf(await basketRows(wallet, SWAP_BASKET), tokenId);
}

/** What `amm-swap` holds, per token id: the amount and the number of outputs. */
export async function swapTotals(wallet: WalletInterface): Promise<Map<string, { amount: bigint; outputs: number }>> {
  const totals = new Map<string, { amount: bigint; outputs: number }>();
  for (const r of await basketRows(wallet, SWAP_BASKET)) {
    if (!r.lockingScript) continue;
    const t = decodeOneColor(Utils.toArray(r.lockingScript, "hex"));
    if (!t || t.role !== "value" || !t.idBytes) continue;
    const id = tokenIdOfWire(t.idBytes);
    const s = totals.get(id) ?? { amount: 0n, outputs: 0 };
    s.amount += t.amount;
    s.outputs++;
    totals.set(id, s);
  }
  return totals;
}

/** The source transactions of `amm-swap`'s outputs (a leg's BEEF), with `extra` merged in. */
export async function swapHoldingBeef(wallet: WalletInterface, extra?: number[]): Promise<Beef> {
  const beef = (await basketBeef(wallet, SWAP_BASKET)) ?? new Beef();
  if (extra) beef.mergeBeef(Beef.fromBinary(extra));
  return beef;
}

export interface SentToSwapBasket {
  txid: string;
  /** The send as the wallet's AtomicBEEF. */
  tx: number[];
  /** The new `amm-swap` outputs, in the order of `amounts`. */
  holdings: TokenInput[];
  filed: boolean;
  submitted: string;
  errors: string[];
}

/**
 * "In" (see the module comment): one module send of `amounts` (each one
 * output) to fresh `amm-swap` keys, filed in `amm-swap`. Throws when the
 * wallet refuses or fails the send; filing and the page's own submit are
 * reported in `errors`.
 */
export async function sendToSwapBasket(
  wallet: WalletInterface,
  base: string,
  tokenId: string,
  amounts: bigint[],
  meta: { sym?: string } = {},
  submit: (topic: string, beef: number[]) => Promise<{ steak?: Steak }> = (topic, beef) => submitToOverlay(base, topic, beef),
): Promise<SentToSwapBasket> {
  if (amounts.length === 0 || amounts.some((a) => a <= 0n)) throw new Error("nothing to send to the swap basket");
  const assetId = sdkTokenId(tokenId);
  const sym = meta.sym ?? "token";
  const keys = await Promise.all(
    amounts.map(async (amount) => {
      const keyID = `${SWAP_KEY_PREFIX}${randomHex()}`;
      const { publicKey } = await wallet.getPublicKey({ protocolID: SWAP_PROTOCOL, keyID, counterparty: "self", forSelf: true });
      return { amount, keyID, publicKey, lockingScript: tokenP2pkhScript(assetId, amount, publicKey) };
    }),
  );
  const { txid, tx } = await runTokenAction(wallet, {
    description: describe(`AMM swap: ${sym} into the swap basket`),
    labels: [...transferPlanLabels(assetId), "amm-swap"],
    outputs: keys.map((k) => ({ lockingScript: k.lockingScript, satoshis: 1, outputDescription: describe(`${sym} for a swap leg`) })),
    options: { randomizeOutputs: false, acceptDelayedBroadcast: false },
  });
  const out: SentToSwapBasket = { txid, tx, holdings: [], filed: false, submitted: "not submitted", errors: [] };
  const final = Transaction.fromAtomicBEEF(tx);
  keys.forEach((k, vout) => {
    const o = final.outputs[vout];
    if (!o || o.lockingScript.toHex() !== k.lockingScript) {
      out.errors.push(`the wallet's send has no output ${vout} to the swap key`);
      return;
    }
    out.holdings.push({ outpoint: `${txid}.${vout}`, txid, vout, satoshis: o.satoshis!, lockingScript: k.lockingScript, amount: k.amount, protocolID: SWAP_PROTOCOL, keyID: k.keyID, counterparty: "self" });
  });
  try {
    out.submitted = steakText(await submit(`tm_color_${assetId}`, tx));
  } catch (err) {
    out.submitted = "the submit failed";
    out.errors.push(`submit: ${err instanceof Error ? err.message : String(err)}`);
  }
  const rows: InternalizeOutput[] = out.holdings.map((h) => ({
    outputIndex: h.vout,
    protocol: "basket insertion",
    insertionRemittance: { basket: SWAP_BASKET, customInstructions: swapInstructions({ assetId, amount: h.amount, protocolID: SWAP_PROTOCOL, keyID: h.keyID, counterparty: "self" }) },
  }));
  try {
    const r = await wallet.internalizeAction({ tx, outputs: rows, description: describe(`AMM swap: ${sym} into the swap basket`), labels: ["amm-swap"] });
    out.filed = r.accepted;
  } catch (err) {
    out.errors.push(`internalizeAction: ${err instanceof Error ? err.message : String(err)}`);
  }
  return out;
}

function varIntSize(n: number): number {
  return n < 0xfd ? 1 : n <= 0xffff ? 3 : n <= 0xffffffff ? 5 : 9;
}

/** A transaction of `inputs` P2PKH inputs (108-byte unlocking scripts) and `outputs`. */
export function p2pkhTxSize(outputs: { script: string }[], inputs: number): number {
  let size = 4 + varIntSize(inputs) + inputs * (36 + 1 + 108 + 4) + varIntSize(outputs.length);
  for (const o of outputs) {
    const len = o.script.length / 2;
    size += 8 + varIntSize(len) + len;
  }
  return size + 4;
}

export interface MovedBack {
  txid: string;
  amount: bigint;
  /** The transaction as AtomicBEEF (the funding its unproven parent). */
  beef: number[];
  submitted: string;
  internalized: boolean;
  relinquished: string[];
  errors: string[];
}

/**
 * "Move back" (see the module comment). Throws before anything is broadcast
 * (no outputs, a wallet error, an input that does not validate); after the
 * submit, errors are collected and an unfiled receipt is kept as a pending
 * payout (src/wallet/pendingPayouts.ts) for a later retry.
 */
export async function moveBack(i: {
  wallet: WalletInterface;
  af: SignedFetch | null;
  base: string;
  tokenId: string;
  meta?: { sym?: string };
  satsPerKb: number;
  payouts?: PendingPayoutStore;
  submit?: (topic: string, beef: number[]) => Promise<{ steak?: Steak }>;
  admitted?: (txid: string, vout: number) => Promise<boolean>;
}): Promise<MovedBack> {
  const { wallet } = i;
  const assetId = sdkTokenId(i.tokenId);
  const sym = i.meta?.sym ?? "token";
  const topic = `tm_color_${assetId}`;
  const holdings = await swapHoldings(wallet, assetId);
  if (holdings.length === 0) throw new Error(`the swap basket holds no ${sym}`);
  const amount = holdings.reduce((a, h) => a + h.amount, 0n);
  const sources = await swapHoldingBeef(wallet);
  for (const h of holdings) if (!sources.findTxid(h.txid)) throw new Error(`the wallet's BEEF lacks the swap basket output ${h.outpoint}`);

  const me = await identityKeyOf(wallet);
  const key = await newReceivingKey(wallet, assetId, me);
  const lockingScript = tokenP2pkhScript(assetId, amount, key.publicKey);
  const size = p2pkhTxSize([{ script: lockingScript }], holdings.length + 1);
  const fee = Math.ceil((size * i.satsPerKb) / 1000);
  const held = holdings.reduce((a, h) => a + h.satoshis, 0);
  const funding = await createFunding(wallet, {
    satoshis: Math.max(1, 1 + fee - held),
    expires: Date.now() + 2 * 60_000,
    amm: { moveBack: assetId },
    description: `AMM move back: ${sym}`,
    labels: ["amm-swap"],
    outputDescription: `AMM move back funding: ${sym}`,
    noSend: false,
  });

  const tx = new Transaction();
  tx.version = 1;
  tx.lockTime = 0;
  for (const h of holdings) {
    const src = sources.findAtomicTransaction(h.txid) ?? sources.findTxid(h.txid)!.tx!;
    tx.addInput({ sourceTXID: h.txid, sourceOutputIndex: h.vout, sourceTransaction: src, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
  }
  tx.addInput({ sourceTXID: funding.txid, sourceOutputIndex: 0, sourceTransaction: funding.tx, unlockingScript: new UnlockingScript(), sequence: 0xffffffff });
  tx.addOutput({ satoshis: 1, lockingScript: LockingScript.fromHex(lockingScript) });
  for (const [n, h] of holdings.entries()) tx.inputs[n]!.unlockingScript = await signP2pkhWithWallet(wallet, tx, n, h);
  const fi = holdings.length;
  tx.inputs[fi]!.unlockingScript = await signP2pkhWithWallet(wallet, tx, fi, { satoshis: funding.tx.outputs[0]!.satoshis!, lockingScript: funding.lockingScript, protocolID: P1SAT, keyID: funding.keyID, counterparty: "self" });
  for (let n = 0; n < tx.inputs.length; n++) {
    if (!spendValid(tx, n)) throw new Error(`input ${n} of the move back does not validate (the funding ${funding.outpoint} stays in ${DEPOSIT_BASKET})`);
  }
  const txid = tx.id("hex");
  const beef = tx.toAtomicBEEF(true);
  const steak = await (i.submit ?? ((t: string, b: number[]) => submitToOverlay(i.base, t, b)))(topic, beef);
  const out: MovedBack = { txid, amount, beef, submitted: steakText(steak), internalized: false, relinquished: [], errors: [] };

  // The module takes in only an admitted, unspent output.
  const took = steak.steak?.[topic]?.outputsToAdmit.includes(0) ?? false;
  const admitted = took || (await (i.admitted ?? ((t: string, v: number) => awaitAdmitted(i.af, i.base, `ls_color_${assetId}`, t, v)))(txid, 0).catch(() => false));
  const pending = {
    id: payoutId(txid, 0),
    kind: "move-back" as const,
    txid,
    vout: 0,
    satoshis: 1,
    lockingScript,
    receipt: key.receipt,
    final: true,
    tokenId: assetId,
    atomicBeef: Utils.toHex(beef),
    description: describe(`AMM move back: ${sym}`),
    createdAt: Date.now(),
  };
  i.payouts?.save(pending);
  if (!admitted) out.errors.push("the token topic has not admitted the move back yet: your wallet takes it in on a retry");
  else {
    try {
      const r = await wallet.internalizeAction({ tx: beef, outputs: [receiptOutput(0, key.receipt)], description: describe(`AMM move back: ${sym}`), labels: ["amm-swap"] });
      out.internalized = r.accepted;
      if (r.accepted) i.payouts?.remove(pending.id);
    } catch (err) {
      out.errors.push(`internalizeAction: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
  for (const s of [...holdings.map((h) => ({ basket: SWAP_BASKET, output: h.outpoint })), { basket: DEPOSIT_BASKET, output: funding.outpoint }]) {
    try {
      await wallet.relinquishOutput(s);
      out.relinquished.push(s.output);
    } catch (err) {
      out.errors.push(`relinquishOutput ${s.output}: ${err instanceof Error ? err.message : String(err)}`);
    }
  }
  return out;
}
