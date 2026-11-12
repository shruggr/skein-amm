/**
 * The LP wallet catches up one hop at a time from the pool output it holds.
 * Each hop files that transaction's continuation into `amm-pools` and, on a
 * swap, takes its LP fee into the wallet itself (0.55.0, agent-env
 * plans/lp-fee-addresses-settled-2026-11-12.md): a sats fee as a BRC-29
 * "wallet payment", a token fee as a `p color` receipt into the token's value
 * basket, both derived from the pool's LP keyID (`lpFeeDerivation`), the
 * sender the LP's own identity key. One internalizeAction per hop. Close and
 * rebalance run the same walk first. 0.54.0: the walk starts from the pool's
 * spendable `amm-pools` row or, when there is none, from the row the caller
 * names (the deploy-history row `myPositions` reports); it ends caught up at a
 * `close` and at a hop whose LpPubKey is no longer this position's (a
 * rebalance).
 */
import { Beef, Transaction, Utils, type InternalizeOutput, type WalletInterface, type WalletOutput, type WalletProtocol } from "@bsv/sdk";
import type { PoolEvent } from "../lib/overlay.js";
import { parseOutpoint, sdkTokenId, tokenIdOfWire } from "../lib/tokenId.js";
import { PoolTemplate } from "../pool/index.js";
import { POOLS_BASKET, receiptOutput } from "../wallet/color.js";
import { WALLET_PAYMENT, identityKeyOf } from "../wallet/brc29.js";
import { errorText } from "../errors.js";
import { POOL_TAG, lpFeeDerivation, poolCustomInstructions } from "./poolDeploy.js";
import { basketRows } from "./wallet.js";

export interface SyncIo {
  /** History strictly after this outpoint (`txid_vout`). The page's `next` is not read. */
  history: (outpoint: string) => Promise<{ events: PoolEvent[] }>;
  readTx: (txid: string) => Promise<number[] | null>;
}

/** The key a filed output's customInstructions names, or null. */
export function storedKey(customInstructions: string | undefined): { protocolID: WalletProtocol; keyID: string; counterparty: string; poolId?: string; id?: string } | null {
  try {
    const ci = JSON.parse(customInstructions ?? "") as { protocolID?: unknown; keyID?: unknown; counterparty?: unknown; poolId?: unknown; id?: unknown };
    if (!Array.isArray(ci.protocolID) || ci.protocolID.length < 2 || typeof ci.protocolID[1] !== "string" || typeof ci.keyID !== "string") return null;
    const level = Number(ci.protocolID[0]);
    if (!Number.isInteger(level)) return null;
    return {
      protocolID: [level, ci.protocolID[1]] as WalletProtocol,
      keyID: ci.keyID,
      counterparty: typeof ci.counterparty === "string" ? ci.counterparty : "self",
      ...(typeof ci.poolId === "string" ? { poolId: ci.poolId } : {}),
      ...(typeof ci.id === "string" ? { id: ci.id } : {}),
    };
  } catch {
    return null;
  }
}

/** `<txid>_<vout>`, txid lowercased, or null. */
export function serverOutpoint(text: string): string | null {
  const op = parseOutpoint(text);
  return op ? `${op.txid}_${op.vout}` : null;
}

function pointKey(txid: string, vout: number): string {
  return `${txid.toLowerCase()}.${vout}`;
}

function rowKey(row: { outpoint: string }): string | null {
  const op = parseOutpoint(row.outpoint);
  return op ? `${op.txid}.${op.vout}` : null;
}

/**
 * The `amm-pools` row of `poolId`. A row that names `poolId` matches that.
 * A row with no pool id matches when its own outpoint is that id (still the deploy).
 */
export function heldPoolRow(rows: WalletOutput[], poolId: string): WalletOutput | undefined {
  const want = serverOutpoint(poolId) ?? poolId.toLowerCase();
  for (const row of rows) {
    let named: string | null = null;
    try {
      const ci = JSON.parse(row.customInstructions ?? "") as { poolId?: unknown };
      if (typeof ci.poolId === "string") named = serverOutpoint(ci.poolId) ?? ci.poolId.toLowerCase();
    } catch {
      /* not JSON: match by outpoint only */
    }
    const self = serverOutpoint(row.outpoint);
    if (named ? named === want : self === want) return row;
  }
  return undefined;
}

/**
 * A continuation's customInstructions (0.48.0, D9): the key the held row names and the pool id —
 * `{protocolID, keyID, counterparty, poolId}`, whatever else a row filed before carried. A held row
 * naming no key: its customInstructions as they are, the pool id added.
 */
function withPoolId(customInstructions: string | undefined, poolId: string): string {
  const key = storedKey(customInstructions);
  if (key) return poolCustomInstructions({ protocolID: key.protocolID, keyID: key.keyID, poolId: key.poolId ?? poolId });
  let ci: Record<string, unknown> = {};
  try {
    const parsed = JSON.parse(customInstructions ?? "{}") as unknown;
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) ci = parsed as Record<string, unknown>;
  } catch {
    ci = {};
  }
  if (typeof ci.poolId !== "string") ci.poolId = poolId;
  return JSON.stringify(ci);
}

function txOf(beef: Beef, txid: string): Transaction | undefined {
  const found = beef.findTxid(txid) ?? beef.findTxid(txid.toLowerCase());
  if (!found) return undefined;
  if (found.tx) return found.tx;
  return found.rawTx ? Transaction.fromBinary(found.rawTx) : undefined;
}

function spendsHeld(tx: Transaction, held: WalletOutput): boolean {
  const op = parseOutpoint(held.outpoint);
  if (!op) return false;
  return tx.inputs.some((i) => {
    const id = (i.sourceTXID ?? i.sourceTransaction?.id("hex") ?? "").toLowerCase();
    return id === op.txid && i.sourceOutputIndex === op.vout;
  });
}

function continuationOf(held: WalletOutput, tx: Transaction, poolId: string): WalletOutput {
  const out = tx.outputs[0]!;
  return {
    ...held,
    outpoint: `${tx.id("hex")}.0`,
    satoshis: out.satoshis ?? 0,
    lockingScript: out.lockingScript.toHex(),
    customInstructions: withPoolId(held.customInstructions, poolId),
  };
}

/**
 * How a walk ended (0.48.2): caught up (the pool's history has nothing after the output the wallet
 * holds), or stopped at a hop it could not take in, with the reason. A stopped walk leaves the
 * wallet's row where it was: Close must not spend it (the pool has moved past it).
 */
export type SyncResult =
  | {
      caughtUp: true;
      /** 0.54.0: the walk ended at the transaction that closed this position (a `close`, or a hop whose LpPubKey is another's: a rebalance). */
      closed?: string;
      /** 0.54.0: internalizeActions this walk made (absent: none). */
      filed?: number;
    }
  | { caughtUp: false; reason: string; filed?: number };

/**
 * Walk `poolId` from the output the wallet holds. One internalize per hop whose continuation is
 * not filed yet (its LP fee, when it pays one, in the same call; 0.55.0: output 2 a wallet payment
 * on a sats-in Swap, a `p color` receipt on a token-in Swap), each with that transaction's Atomic BEEF. Stops (and
 * says why) when the next event's transaction cannot be read, does not spend that output, has no
 * pool at output 0, or the wallet refuses it. 0.54.0: starts from the pool's spendable row, else
 * from `start` (the deploy-history row); neither: nothing to walk. Ends caught up, filing nothing
 * more, at a `close` event and at a hop whose pool names another LpPubKey (a rebalance).
 */
export async function syncPool(wallet: WalletInterface, poolId: string, io: SyncIo, start?: WalletOutput): Promise<SyncResult> {
  const pools = await basketRows(wallet, POOLS_BASKET);
  const opening = heldPoolRow(pools.filter((r) => r.spendable !== false), poolId) ?? start;
  if (!opening) return { caughtUp: true };
  let current: WalletOutput = opening;
  const lpPubKey = opening.lockingScript ? PoolTemplate.decode(opening.lockingScript)?.state.lpPubKey.toLowerCase() : undefined;
  let filed = 0;
  const done = (closed?: string): SyncResult => ({ caughtUp: true, ...(closed ? { closed } : {}), ...(filed > 0 ? { filed } : {}) });
  const knownPools = new Set(pools.map(rowKey).filter((k): k is string => k !== null));
  // 0.55.0: the sender of the fee payments to self, read once, when a fee is first taken in.
  let me: string | undefined;
  let cursor = serverOutpoint(current.outpoint);
  if (!cursor) return { caughtUp: false, reason: `the wallet's row names no outpoint (${current.outpoint})` };

  const apply = async (event: PoolEvent): Promise<{ stop: string } | { next: string } | { closed: string }> => {
    if (event.type === "close") return { closed: event.txid };
    const raw = await io.readTx(event.txid);
    if (!raw) return { stop: `the transaction ${event.txid} cannot be read from the exchange` };
    let beef: Beef;
    try {
      beef = Beef.fromBinary(raw);
    } catch {
      return { stop: `the transaction ${event.txid}'s BEEF does not parse` };
    }
    const tx = txOf(beef, event.txid);
    if (!tx) return { stop: `the BEEF the exchange served for ${event.txid} does not hold it` };
    if (!spendsHeld(tx, current)) return { stop: `${event.txid} does not spend the pool output the wallet holds (${current.outpoint})` };
    const out0 = tx.outputs[0];
    const pool = out0?.lockingScript ? PoolTemplate.decode(out0.lockingScript) : null;
    if (!pool) return { stop: `${event.txid}'s output 0 is not a pool` };
    if (lpPubKey && pool.state.lpPubKey.toLowerCase() !== lpPubKey) return { closed: event.txid };
    const rescind = event.type === "rescind" || PoolTemplate.rescinded(pool);
    const wantFee = event.type === "swap" && (event.feeLp ?? 0n) > 0n && !rescind;
    if (wantFee && !tx.outputs[2]) return { stop: `${event.txid} names an LP fee and has no output 2` };
    const txid = tx.id("hex").toLowerCase();
    // 0.55.0: the continuation and its fee go in in one call, so a filed continuation is a taken-in fee.
    if (!knownPools.has(pointKey(txid, 0))) {
      const outputs: InternalizeOutput[] = [
        {
          outputIndex: 0,
          protocol: "basket insertion",
          insertionRemittance: { basket: POOLS_BASKET, tags: [POOL_TAG], customInstructions: withPoolId(current.customInstructions, poolId) },
        },
      ];
      if (wantFee) {
        const key = storedKey(current.customInstructions);
        if (!key) return { stop: "the pool output names no key" };
        try {
          me ??= await identityKeyOf(wallet);
        } catch (e) {
          return { stop: `the wallet's identity key cannot be read: ${errorText(e)}` };
        }
        let derivation: { derivationPrefix: string; derivationSuffix: string };
        try {
          derivation = lpFeeDerivation(key.keyID);
        } catch (e) {
          return { stop: errorText(e) };
        }
        // The Swap's direction, from the transaction: a sats-in Swap raises the pool's BSV reserve.
        if ((tx.outputs[0]!.satoshis ?? 0) > current.satoshis) {
          outputs.push({ outputIndex: 2, protocol: WALLET_PAYMENT, paymentRemittance: { ...derivation, senderIdentityKey: me } });
        } else {
          const assetId = sdkTokenId(tokenIdOfWire(Utils.toArray(pool.args.assetId, "hex")));
          outputs.push(receiptOutput(2, { assetId, ...derivation, senderIdentityKey: me }));
        }
      }
      try {
        // 0.48.2: the transaction's Atomic BEEF (internalizeAction takes nothing else).
        await wallet.internalizeAction({ tx: beef.toBinaryAtomic(txid), outputs, description: "AMM pool output filed" });
      } catch (e) {
        return { stop: `the wallet refused ${txid}: ${errorText(e)}` };
      }
      filed++;
      knownPools.add(pointKey(txid, 0));
    }
    current = continuationOf(current, tx, poolId);
    return { next: `${txid}_0` };
  };

  for (;;) {
    let page: { events: PoolEvent[] };
    try {
      page = await io.history(cursor);
    } catch (e) {
      return { caughtUp: false, reason: `the pool's history cannot be read: ${errorText(e)}` };
    }
    if (page.events.length === 0) return done();
    for (const event of page.events) {
      const r = await apply(event);
      if ("stop" in r) return { caughtUp: false, reason: r.stop, ...(filed > 0 ? { filed } : {}) };
      if ("closed" in r) return done(r.closed);
      cursor = r.next;
    }
  }
}
