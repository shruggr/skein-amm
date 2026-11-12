/**
 * The LP wallet catches up one hop at a time from the pool output it holds.
 * Each hop files that transaction's continuation into `amm-pools` and, on a
 * swap, its LP fee into `openex fees`. Close and collect run the same walk,
 * then spend what the wallet holds. 0.54.0: the walk starts from the pool's
 * spendable `amm-pools` row or, when there is none, from the row the caller
 * names (the deploy-history row `myPositions` reports); it ends caught up at a
 * `close` and at a hop whose LpPubKey is no longer this position's (a
 * rebalance). Collect spends the basket in pages (`collectBasket`).
 */
import { Beef, Transaction, Utils, type CreateActionOutput, type InternalizeOutput, type WalletInterface, type WalletOutput, type WalletProtocol } from "@bsv/sdk";
import { ColorPlan } from "@bsv/color-permission-module";
import type { PoolEvent } from "../lib/overlay.js";
import { parseOutpoint, sdkTokenId, tokenIdOfWire } from "../lib/tokenId.js";
import { PoolTemplate } from "../pool/index.js";
import { decodeOneColor } from "../pool/oneColor.js";
import { signP2pkhWithWallet } from "../market/swapAction.js";
import { inputIndexOf, runAppSignedAction } from "../wallet/action.js";
import { FEES_BASKET, POOLS_BASKET, newReceivingKey } from "../wallet/color.js";
import { identityKeyOf } from "../wallet/brc29.js";
import { POOL_TAG, poolCustomInstructions, tokenP2pkhScript } from "./poolDeploy.js";
import { basketBeef, basketRows } from "./wallet.js";

export { FEES_BASKET };

/** A P2PKH unlock is a signature push and a public-key push (BIP-143, 107–108 bytes). */
const P2PKH_UNLOCK_LENGTH = 108;

export interface SyncIo {
  /** History strictly after this outpoint (`txid_vout`). The page's `next` is not read. */
  history: (outpoint: string) => Promise<{ events: PoolEvent[] }>;
  readTx: (txid: string) => Promise<number[] | null>;
}

/** Token inputs a collect page names at most (0.54.0, David 2026-11-12: "maybe even two hundred and fifty inputs"). */
export const COLLECT_PAGE_TOKEN_INPUTS = 250;
/** The miner fee rate a collect page is sized with when the caller gives none, sats per 1000 bytes (the network minimum is ~100). */
export const COLLECT_FEE_RATE_SATS_PER_KB = 200;
/** The margin over a page's estimated miner fee its sats fee rows must cover. */
export const COLLECT_FEE_MARGIN = 1.25;
/** A transaction's version, lock time and two counts, bytes. */
const TX_OVERHEAD_BYTES = 10;
/** An input with a P2PKH unlock: outpoint 36, script length 1, the unlock, sequence 4. */
const INPUT_BYTES = 36 + 1 + P2PKH_UNLOCK_LENGTH + 4;

/** What the fees basket holds (`availableFees`): sats rows summed, token rows summed per token id. */
export interface AvailableFees {
  sats: bigint;
  tokens: { tokenId: string; amount: bigint }[];
  /** Every row of `openex fees`. */
  outputs: number;
}

export interface CollectOptions {
  /** The rate a page's miner fee is estimated at, sats per 1000 bytes (default `COLLECT_FEE_RATE_SATS_PER_KB`). The wallet sets the real fee. */
  feeRateSatsPerKb?: number;
}

export interface CollectResult {
  /** The LAST completed page's txid; absent when nothing was spent. */
  txid?: string;
  /** 0.54.0: every completed page's txid, in order. */
  txids: string[];
  /** 0.54.0: the pages this collect composed (`txids.length` of them completed). */
  pages: number;
  /** 0.48.2: the pools whose walk stopped before catching up (their later fees are not in this collect); 0.54.0: also pools on an old contract and pools whose token is not enabled. */
  stopped?: { poolId: string; reason: string }[];
  /** 0.54.0: the page that was refused or failed (1-based) and the wallet's error text; the pages after it were not run. */
  refused?: { page: number; error: string };
  /** 0.54.0: what is still in the fees basket after the collect. */
  remaining?: AvailableFees;
  /** Gross sats of the fee rows the completed pages spent (the wallet keeps them as its change, less the miner fees). */
  sats: bigint;
  /** Per token id, the units the completed pages moved to the wallet's module. */
  tokens: { tokenId: string; amount: bigint }[];
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
 * Walk `poolId` from the output the wallet holds. One internalize per hop that
 * still has a continuation or a fee to file, each with that transaction's Atomic BEEF. Stops (and
 * says why) when the next event's transaction cannot be read, does not spend that output, has no
 * pool at output 0, or the wallet refuses it. 0.54.0: starts from the pool's spendable row, else
 * from `start` (the deploy-history row); neither: nothing to walk. Ends caught up, filing nothing
 * more, at a `close` event and at a hop whose pool names another LpPubKey (a rebalance).
 */
export async function syncPool(wallet: WalletInterface, poolId: string, io: SyncIo, start?: WalletOutput): Promise<SyncResult> {
  const [pools, fees] = await Promise.all([basketRows(wallet, POOLS_BASKET), basketRows(wallet, FEES_BASKET)]);
  const opening = heldPoolRow(pools.filter((r) => r.spendable !== false), poolId) ?? start;
  if (!opening) return { caughtUp: true };
  let current: WalletOutput = opening;
  const lpPubKey = opening.lockingScript ? PoolTemplate.decode(opening.lockingScript)?.state.lpPubKey.toLowerCase() : undefined;
  let filed = 0;
  const done = (closed?: string): SyncResult => ({ caughtUp: true, ...(closed ? { closed } : {}), ...(filed > 0 ? { filed } : {}) });
  const knownPools = new Set(pools.map(rowKey).filter((k): k is string => k !== null));
  const knownFees = new Set(fees.map(rowKey).filter((k): k is string => k !== null));
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
    const contKnown = knownPools.has(pointKey(txid, 0));
    const feeKnown = !wantFee || knownFees.has(pointKey(txid, 2));
    if (!contKnown || !feeKnown) {
      const outputs: InternalizeOutput[] = [];
      if (!contKnown) {
        outputs.push({
          outputIndex: 0,
          protocol: "basket insertion",
          insertionRemittance: { basket: POOLS_BASKET, tags: [POOL_TAG], customInstructions: withPoolId(current.customInstructions, poolId) },
        });
      }
      if (wantFee && !feeKnown) {
        const key = storedKey(current.customInstructions);
        if (!key) return { stop: "the pool output names no key" };
        // 0.48.0 (D9): the key and the pool id only; a token fee's token is its script's.
        outputs.push({
          outputIndex: 2,
          protocol: "basket insertion",
          insertionRemittance: {
            basket: FEES_BASKET,
            customInstructions: JSON.stringify({ protocolID: key.protocolID, keyID: key.keyID, counterparty: key.counterparty, poolId }),
          },
        });
      }
      try {
        // 0.48.2: the transaction's Atomic BEEF (internalizeAction takes nothing else).
        await wallet.internalizeAction({ tx: beef.toBinaryAtomic(txid), outputs, description: "AMM pool output filed" });
      } catch (e) {
        return { stop: `the wallet refused ${txid}: ${e instanceof Error ? e.message : String(e)}` };
      }
      filed++;
      if (!contKnown) knownPools.add(pointKey(txid, 0));
      if (wantFee && !feeKnown) knownFees.add(pointKey(txid, 2));
    }
    current = continuationOf(current, tx, poolId);
    return { next: `${txid}_0` };
  };

  for (;;) {
    let page: { events: PoolEvent[] };
    try {
      page = await io.history(cursor);
    } catch (e) {
      return { caughtUp: false, reason: `the pool's history cannot be read: ${e instanceof Error ? e.message : String(e)}` };
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

/** One `openex fees` row as collect spends it: its outpoint, its key, and what it carries. */
export interface FeeInput {
  txid: string;
  vout: number;
  satoshis: number;
  lockingScript: string;
  protocolID: WalletProtocol;
  keyID: string;
  counterparty: string;
  kind: "sats" | "token";
  tokenId?: string;
  amount?: bigint;
}

/** A fee row's value: a 1Color value output is a token row (as the swap basket's totals read it), anything else sats. */
function feeValueOf(lockingScript: string | undefined): { kind: "sats" } | { kind: "token"; tokenId: string; amount: bigint } {
  const color = lockingScript ? decodeOneColor(Utils.toArray(lockingScript, "hex")) : null;
  if (color && color.role === "value" && color.idBytes) return { kind: "token", tokenId: tokenIdOfWire(color.idBytes), amount: color.amount };
  return { kind: "sats" };
}

function byTokenId<T extends { tokenId: string }>(a: T, b: T): number {
  return a.tokenId < b.tokenId ? -1 : a.tokenId > b.tokenId ? 1 : 0;
}

function tokenSums(rows: { tokenId?: string; amount?: bigint }[]): { tokenId: string; amount: bigint }[] {
  const by = new Map<string, bigint>();
  for (const r of rows) if (r.tokenId && (r.amount ?? 0n) > 0n) by.set(r.tokenId, (by.get(r.tokenId) ?? 0n) + r.amount!);
  return [...by.entries()].map(([tokenId, amount]) => ({ tokenId, amount })).sort(byTokenId);
}

/** What the fees basket holds (0.54.0): ONE `listOutputs` of `openex fees`, nothing else read. */
export async function availableFees(wallet: WalletInterface): Promise<AvailableFees> {
  return feeTotals(await basketRows(wallet, FEES_BASKET));
}

function feeTotals(rows: WalletOutput[]): AvailableFees {
  let sats = 0n;
  const tokens: { tokenId: string; amount: bigint }[] = [];
  for (const row of rows) {
    const v = feeValueOf(row.lockingScript);
    if (v.kind === "token") tokens.push(v);
    else sats += BigInt(Number.isSafeInteger(row.satoshis) && row.satoshis > 0 ? row.satoshis : 0);
  }
  return { sats, tokens: tokenSums(tokens), outputs: rows.length };
}

/** The basket's rows as fee inputs; a row with no script, key or outpoint throws (nothing is spent). */
export function feeInputsOf(rows: WalletOutput[]): FeeInput[] {
  const fees: FeeInput[] = [];
  for (const row of rows) {
    if (!row.lockingScript) throw new Error("the fee output has no locking script");
    const key = storedKey(row.customInstructions);
    if (!key) throw new Error("the fee output names no key");
    if (!Number.isSafeInteger(row.satoshis) || row.satoshis < 0) throw new Error("the fee output's satoshis are not a safe integer");
    const listed = /^([0-9a-fA-F]{64})[._](\d+)$/.exec(row.outpoint);
    if (!listed) throw new Error("the fee output names no outpoint");
    const v = feeValueOf(row.lockingScript);
    fees.push({
      txid: listed[1]!,
      vout: Number(listed[2]),
      satoshis: row.satoshis,
      lockingScript: row.lockingScript,
      protocolID: key.protocolID,
      keyID: key.keyID,
      counterparty: key.counterparty,
      kind: v.kind,
      ...(v.kind === "token" ? { tokenId: v.tokenId, amount: v.amount } : {}),
    });
  }
  return fees;
}

/** A transaction output's bytes: value 8, the script's length, the script. */
function outputBytes(scriptHex: string): number {
  const n = scriptHex.length / 2;
  return 8 + (n < 0xfd ? 1 : n <= 0xffff ? 3 : 5) + n;
}

/** A page's estimated miner fee: its inputs at the known unlock size and its outputs, at `rate`, times the margin. */
export function estimatePageFee(inputs: number, outputScripts: string[], rate: number): number {
  const bytes = TX_OVERHEAD_BYTES + inputs * INPUT_BYTES + outputScripts.reduce((a, s) => a + outputBytes(s), 0);
  return Math.ceil(((bytes * rate) / 1000) * COLLECT_FEE_MARGIN);
}

/** One collect page: its fee rows (tokens first) and its tokens' sums (one output each). */
export interface CollectPagePlan {
  inputs: FeeInput[];
  tokens: { tokenId: string; amount: bigint }[];
}

/**
 * The pages (0.54.0, David 2026-11-12): token rows first, by token id, up to
 * `COLLECT_PAGE_TOKEN_INPUTS` per page, each page with as many sats rows (largest first) as cover its
 * estimated miner fee; the sats rows left in sats-only pages of up to the same count. Sats run out
 * first: the later token pages have none (the wallet funds them). `scriptOf` is a token output's
 * script for the estimate. Rows that carry nothing (0 sats, 0 units) are not spent.
 */
export function composePages(fees: FeeInput[], rate: number, scriptOf: (tokenId: string, amount: bigint) => string): CollectPagePlan[] {
  const tokenRows = fees.filter((f) => f.kind === "token" && (f.amount ?? 0n) > 0n).sort((a, b) => byTokenId(a as { tokenId: string }, b as { tokenId: string }) || (a.txid < b.txid ? -1 : a.txid > b.txid ? 1 : a.vout - b.vout));
  const satsRows = fees.filter((f) => f.kind === "sats" && f.satoshis > 0).sort((a, b) => b.satoshis - a.satoshis);
  const pages: CollectPagePlan[] = [];
  let s = 0;
  for (let t = 0; t < tokenRows.length; t += COLLECT_PAGE_TOKEN_INPUTS) {
    const inputs = tokenRows.slice(t, t + COLLECT_PAGE_TOKEN_INPUTS);
    const tokens = tokenSums(inputs);
    const scripts = tokens.map((x) => scriptOf(x.tokenId, x.amount));
    let covered = 0;
    while (s < satsRows.length && covered < estimatePageFee(inputs.length, scripts, rate)) {
      covered += satsRows[s]!.satoshis;
      inputs.push(satsRows[s++]!);
    }
    pages.push({ inputs, tokens });
  }
  for (; s < satsRows.length; s += COLLECT_PAGE_TOKEN_INPUTS) pages.push({ inputs: satsRows.slice(s, s + COLLECT_PAGE_TOKEN_INPUTS), tokens: [] });
  return pages;
}

/**
 * Collect (0.54.0): the `openex fees` basket into the wallet's own balance, page by page
 * (`composePages`), each page ONE wallet createAction with its fee rows as inputs (the page signs
 * them, `runAppSignedAction`) and one `p color` output per token on the page, NO sats output: the
 * wallet keeps the sats as its change at its own fee rate (the shape of `moveBackSats`). A refusal
 * or a failure on page 1 throws (nothing spent); on a later page it stops the loop and the pages
 * done are returned with `refused`.
 */
export async function collectBasket(wallet: WalletInterface, options: CollectOptions = {}): Promise<CollectResult> {
  const rate = options.feeRateSatsPerKb ?? COLLECT_FEE_RATE_SATS_PER_KB;
  if (!Number.isFinite(rate) || rate <= 0) throw new Error("feeRateSatsPerKb must be a positive number");
  const rows = await basketRows(wallet, FEES_BASKET);
  if (rows.length === 0) return { txids: [], pages: 0, sats: 0n, tokens: [] };
  const fees = feeInputsOf(rows);
  const me = fees.some((f) => f.kind === "token" && (f.amount ?? 0n) > 0n) ? await identityKeyOf(wallet) : "";
  const pages = composePages(fees, rate, (tokenId, amount) => tokenP2pkhScript(sdkTokenId(tokenId), amount, me));
  if (pages.length === 0) return { txids: [], pages: 0, sats: 0n, tokens: [] };
  const beef = await basketBeef(wallet, FEES_BASKET);
  if (!beef) throw new Error("the fee outputs' transactions are not in the wallet");
  const inputBEEF = beef.toBinary();

  const txids: string[] = [];
  const spent: FeeInput[] = [];
  let refused: { page: number; error: string } | undefined;
  for (let n = 0; n < pages.length; n++) {
    const page = pages[n]!;
    try {
      txids.push(await collectPage(wallet, page, inputBEEF, me));
      spent.push(...page.inputs);
    } catch (e) {
      if (n === 0) throw e;
      refused = { page: n + 1, error: e instanceof Error ? e.message : String(e) };
      break;
    }
  }
  const remaining = feeTotals(await basketRows(wallet, FEES_BASKET));
  const sats = spent.reduce((a, f) => (f.kind === "sats" ? a + BigInt(f.satoshis) : a), 0n);
  return {
    ...(txids.length ? { txid: txids[txids.length - 1]! } : {}),
    txids,
    pages: pages.length,
    ...(refused ? { refused } : {}),
    remaining,
    sats,
    tokens: tokenSums(spent),
  };
}

/** One page's createAction → the page's signatures → signAction. → its txid. */
async function collectPage(wallet: WalletInterface, page: CollectPagePlan, inputBEEF: number[], me: string): Promise<string> {
  const outputs: CreateActionOutput[] = [];
  const owned: { outputIndex: number; assetId: string; derivationPrefix: string; derivationSuffix: string; senderIdentityKey: string }[] = [];
  for (const token of page.tokens) {
    const assetId = sdkTokenId(token.tokenId);
    const k = await newReceivingKey(wallet, assetId, me);
    owned.push({ outputIndex: outputs.length, ...k.receipt });
    outputs.push({ lockingScript: tokenP2pkhScript(assetId, token.amount, k.publicKey), satoshis: 1, outputDescription: "Pool fees in tokens" });
  }
  const labels = page.tokens.length > 0
    ? ColorPlan.toLabels({
        version: 1,
        assets: page.tokens.map((t) => sdkTokenId(t.tokenId)),
        ownedOutputs: owned,
        operations: page.tokens.map((t) => ({ assetId: sdkTokenId(t.tokenId), operation: "transfer" as const })),
        inputPositions: [],
        changePositions: [],
        sighashTypes: [],
      })
    : undefined;
  const signed = await runAppSignedAction(
    wallet,
    {
      description: "Collect pool fees",
      inputBEEF,
      inputs: page.inputs.map((fee) => ({ outpoint: `${fee.txid}.${fee.vout}`, unlockingScriptLength: P2PKH_UNLOCK_LENGTH, inputDescription: "A pool fee" })),
      outputs,
      ...(labels ? { labels } : {}),
      options: { randomizeOutputs: false, acceptDelayedBroadcast: false },
    },
    async (tx) => {
      const spends: Record<number, { unlockingScript: string }> = {};
      for (const fee of page.inputs) {
        const n = inputIndexOf(tx, fee.txid, fee.vout);
        if (n < 0) throw new Error("the wallet did not place a fee input");
        const unlock = await signP2pkhWithWallet(wallet, tx, n, fee);
        spends[n] = { unlockingScript: unlock.toHex() };
      }
      return spends;
    },
  );
  return signed.txid;
}
