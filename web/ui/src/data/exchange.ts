/**
 * Open Exchange: every read and every action the pages make, in one typed
 * module (shruggr/skein#147). The pages import from here and nowhere else in
 * the data layer (they still use `components/Id`, `lp/amounts`, the wallet
 * provider and `ox/*`).
 *
 * EVERY FUNCTION HERE RETURNS FIXTURE DATA. Each doc comment names the existing
 * logic the overlay session should call to make it real, or the new contract
 * piece it needs ("NEW:"). The fixtures are consistent with each other: the
 * hosted tokens' prices come from their pools, a quote routes over those
 * pools at their fees, a swap, deploy or close changes them, and the price
 * subscription pushes the change.
 *
 * Ids (David, 2026-10-08/09): a Mandala token id is the bare txid; an outpoint
 * is `<txid>.<vout>`; an underscore appears only in a legacy standard id
 * (`<txid>_<vout>`). `idKindOf` picks how `<Id>` shows a token id.
 *
 * Amounts are bigints in base units (sats; a token's base units, `dec`
 * places). Prices are numbers: sats per whole token.
 *
 * Fixture-only switch for previews and screenshots: `?fixture=wallet` (a
 * connected wallet) or `?fixture=root` (connected as root) in the page URL.
 * Remove it with the fixtures.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { useConnectDialog, useWallet } from "../wallet/AppWalletProvider";
import type { IdKind } from "../components/Id";

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

/** A Mandala token id (bare txid) or a legacy standard id (`<txid>_<vout>`). */
export type TokenId = string;

/** How `<Id>` shows a token id: a bare txid as a txid, a legacy `<txid>_<vout>` as a token id. */
export function idKindOf(tokenId: TokenId): IdKind {
  return tokenId.includes("_") ? "token" : "txid";
}

/** What a swap of 2% of the price costs: the sats a buy needs to move the marginal price up by `bps`. */
export interface Depth {
  bps: number;
  sats: bigint;
}

/** A token this overlay hosts (registered here), with its pools summed. */
export interface HostedToken {
  tokenId: TokenId;
  sym: string;
  dec: number;
  /** An image URL (a data: URL of the deploy's embedded icon, or an ORDFS URL). */
  icon?: string;
  /** Sats per whole token across the token's pools: what the next small swap pays. Null with no open pool. */
  marginalPrice: number | null;
  depth: Depth | null;
  /** Number of open pools. */
  pools: number;
  /** All pools summed. */
  reserves: { sats: bigint; tokens: bigint };
  /** Price change over 24 h, a fraction (0.018 = +1.8%). Absent when not known. */
  change24h?: number;
}

/** One open pool of a token, as a quote routes over it. */
export interface PoolView {
  /** `<txid>.<vout>` */
  outpoint: string;
  tokenId: TokenId;
  sats: bigint;
  tokens: bigint;
  validator: ValidatorRef;
}

export interface ValidatorRef {
  identityKey: string;
  live: boolean;
}

/** A price push from the host's subscription (skein#148). */
export interface PriceUpdate {
  tokenId: TokenId;
  marginalPrice: number | null;
  depth: Depth | null;
  pools: number;
  reserves: { sats: bigint; tokens: bigint };
  at: number;
}

/** "buy": pay sats, receive the token. "sell": pay the token, receive sats. */
export type Side = "buy" | "sell";

export interface QuoteRequest {
  tokenId: TokenId;
  side: Side;
  /** Base units of what is paid (sats on a buy, token base units on a sell). */
  amountIn: bigint;
  /** Max slippage against the quoted amount out, basis points. */
  maxSlippageBps: number;
  allowPartial: boolean;
}

export interface QuoteLeg {
  /** The pool, `<txid>.<vout>`. */
  poolOutpoint: string;
  amountIn: bigint;
  amountOut: bigint;
  /** Share of the amount in, basis points (the legs sum to 10000). */
  shareBps: number;
  /** The pool's reserves before the leg. */
  reserves: { sats: bigint; tokens: bigint };
  validator: ValidatorRef;
}

export interface Quote {
  request: QuoteRequest;
  legs: QuoteLeg[];
  amountOut: bigint;
  /** The least amount out the swap accepts: amountOut less max slippage. */
  minAmountOut: bigint;
  /** Sats per whole token this swap pays (fees included). */
  effectivePrice: number;
  /** Sats per whole token before the swap. */
  marginalPrice: number;
  /** |effective − marginal| / marginal, basis points. */
  impactBps: number;
  /** The validator's fees applied to every leg. */
  fees: Fees;
}

export interface LegResult {
  poolOutpoint: string;
  status: "filled" | "failed";
  txid?: string;
  error?: string;
}

export interface SwapResult {
  status: "filled" | "partial" | "failed";
  legs: LegResult[];
}

/** The validator's fees, basis points of the amount in. The LP accepts them; there is no fee input. */
export interface Fees {
  lpBps: number;
  validatorBps: number;
}

/** The validator a new position is deployed with, and its fees (read-only on the page). */
export interface ValidatorTerms {
  validator: ValidatorRef;
  /** "this exchange's" when the validator is this skein. */
  isThisExchange: boolean;
  fees: Fees;
}

/** One of the user's pools. A position is only deployed or closed (no add, no partial remove). */
export interface Position {
  /** The pool, `<txid>.<vout>`. */
  outpoint: string;
  tokenId: TokenId;
  sym: string;
  dec: number;
  icon?: string;
  sats: bigint;
  tokens: bigint;
  /** Fees the pool has earned the LP, when it can be told. */
  feesEarnedSats?: bigint;
  validator: ValidatorRef;
}

export interface DeployRequest {
  tokenId: TokenId;
  /** Token base units. */
  tokens: bigint;
  sats: bigint;
}

export interface DeployResult {
  /** The new pool, `<txid>.<vout>`. */
  outpoint: string;
  txid: string;
}

export interface CloseRequest {
  outpoint: string;
  /** The miner fee left from the pool, sats. 0 = the fee is funded from another wallet input. */
  bsvFee: bigint;
}

export interface CloseResult {
  txid: string;
  /** Returned to the wallet. */
  sats: bigint;
  tokens: bigint;
}

/** One of the user's own Mandala tokens. */
export interface WalletToken {
  tokenId: TokenId;
  sym: string;
  dec: number;
  icon?: string;
  balance: bigint;
  /** How many wallet outputs hold it. */
  outputs: number;
  /** Registered on this exchange. */
  onExchange: boolean;
  /** The user asked this exchange to list it. */
  requested: boolean;
}

/** A token seen on the discovery topic (`tm_mandala`). */
export interface DiscoveryToken {
  tokenId: TokenId;
  sym: string;
  dec: number;
  icon?: string;
  /** When discovery first saw it, ms. */
  seenAt?: number;
}

/** A token registered on this exchange, as Settings lists it. */
export interface RegisteredToken {
  tokenId: TokenId;
  sym: string;
  icon?: string;
}

/** A holder's "Add to this exchange". */
export interface ListingRequest {
  tokenId: TokenId;
  sym: string;
  /** The requester's identity key. */
  from: string;
  at: number;
}

export interface Session {
  status: "disconnected" | "connecting" | "connected";
  identityKey: string | null;
  /** Opens the wallet connect dialog. */
  connect: () => void;
  disconnect: () => void;
}

// ---------------------------------------------------------------------------
// Links
// ---------------------------------------------------------------------------

/** The Mandala deploy page the app carries (www/mandala/deploy/, skein-mandala). */
export const MANDALA_DEPLOY_HREF = "mandala/deploy/";
/** The skein's management site, at its own app path (skein#147). */
export const SITE_HREF = "/site/";

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

/** A deterministic 64-hex string from a seed (fixture ids only). */
function hex64(seed: string): string {
  let h = 2166136261 >>> 0; // FNV-1a over the whole seed
  for (let i = 0; i < seed.length; i++) h = Math.imul(h ^ seed.charCodeAt(i), 16777619) >>> 0;
  let out = "";
  for (let k = 0; k < 8; k++) {
    let x = (h ^ Math.imul(k + 1, 0x9e3779b9)) >>> 0; // murmur3 fmix32 per 8-hex chunk
    x = Math.imul(x ^ (x >>> 16), 0x85ebca6b) >>> 0;
    x = Math.imul(x ^ (x >>> 13), 0xc2b2ae35) >>> 0;
    x = (x ^ (x >>> 16)) >>> 0;
    out += x.toString(16).padStart(8, "0");
  }
  return out;
}

const ROOT_KEY = "02" + hex64("root");
const USER_KEY = "03" + hex64("user");
const THIS_VALIDATOR = "03" + hex64("validator-this");
const OTHER_VALIDATOR = "02" + hex64("validator-other");
const REQUESTER = "02" + hex64("requester");

const FEES: Fees = { lpBps: 30, validatorBps: 5 };
const HOUR = 3_600_000;

interface FixtureToken {
  tokenId: TokenId;
  sym: string;
  dec: number;
  icon?: string;
  change24h?: number;
  registered: boolean;
  discoveredAt: number;
}

interface FixtureState {
  tokens: FixtureToken[];
  pools: (PoolView & { mine: boolean; feesEarnedSats?: bigint })[];
  wallet: { sats: bigint; tokens: Map<TokenId, { balance: bigint; outputs: number }> };
  requests: ListingRequest[];
  nextTx: number;
}

const T = {
  GOLD: hex64("GOLD"),
  KNOT: hex64("KNOT"),
  WOOL: hex64("WOOL"),
  TEA: hex64("TEA"),
  MOSS: hex64("MOSS"),
  FERN: hex64("FERN"),
  REED: hex64("REED"),
};

function fixtureState(now = Date.now()): FixtureState {
  const pool = (seed: string, tokenId: TokenId, sats: number, tokens: bigint, validator: string, mine: boolean, feesEarnedSats?: bigint) => ({
    outpoint: `${hex64(seed)}.0`,
    tokenId,
    sats: BigInt(sats),
    tokens,
    validator: { identityKey: validator, live: true },
    mine,
    ...(feesEarnedSats !== undefined ? { feesEarnedSats } : {}),
  });
  return {
    tokens: [
      { tokenId: T.GOLD, sym: "GOLD", dec: 2, change24h: 0.018, registered: true, discoveredAt: now - 40 * 24 * HOUR },
      { tokenId: T.KNOT, sym: "KNOT", dec: 0, change24h: -0.004, registered: true, discoveredAt: now - 30 * 24 * HOUR },
      { tokenId: T.WOOL, sym: "WOOL", dec: 0, change24h: 0, registered: true, discoveredAt: now - 12 * 24 * HOUR },
      { tokenId: T.TEA, sym: "TEA", dec: 0, change24h: 0.061, registered: true, discoveredAt: now - 6 * 24 * HOUR },
      { tokenId: T.MOSS, sym: "MOSS", dec: 0, registered: false, discoveredAt: now - 2 * 24 * HOUR },
      { tokenId: T.FERN, sym: "FERN", dec: 1, registered: false, discoveredAt: now - 9 * HOUR },
      { tokenId: T.REED, sym: "REED", dec: 0, registered: false, discoveredAt: now - 40 * 60_000 },
    ],
    pools: [
      // GOLD (dec 2): three pools near 412 sats per GOLD.
      pool("pool-gold-1", T.GOLD, 1_240_000, 301_000n, THIS_VALIDATOR, true, 4_120n),
      pool("pool-gold-2", T.GOLD, 480_000, 116_500n, THIS_VALIDATOR, false),
      pool("pool-gold-3", T.GOLD, 205_000, 49_760n, OTHER_VALIDATOR, false),
      // KNOT: two pools at 1,250.
      pool("pool-knot-1", T.KNOT, 12_500_000, 10_000n, THIS_VALIDATOR, true, 38_400n),
      pool("pool-knot-2", T.KNOT, 2_500_000, 2_000n, OTHER_VALIDATOR, false),
      // WOOL, TEA: one each.
      pool("pool-wool-1", T.WOOL, 80_000, 2_162n, THIS_VALIDATOR, false),
      pool("pool-tea-1", T.TEA, 22_000, 250n, THIS_VALIDATOR, false),
    ],
    wallet: {
      sats: 1_820_400n,
      tokens: new Map([
        [T.GOLD, { balance: 120_450n, outputs: 3 }],
        [T.TEA, { balance: 300n, outputs: 1 }],
        [T.MOSS, { balance: 5_000n, outputs: 1 }],
        [T.FERN, { balance: 900n, outputs: 2 }],
      ]),
    },
    requests: [{ tokenId: T.MOSS, sym: "MOSS", from: REQUESTER, at: now - 5 * HOUR }],
    nextTx: 1,
  };
}

let state = fixtureState();

/** Reset the fixtures (tests). */
export function resetFixtures(): void {
  state = fixtureState();
  emit();
}

const listeners = new Set<() => void>();
function emit(): void {
  for (const l of listeners) l();
}
/** Calls `fn` after any action changes data (swap, deploy, close, register…). Returns the unsubscribe. */
export function onDataChange(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

const delay = (ms: number) => new Promise<void>((r) => setTimeout(r, ms));
const LATENCY = 120;

function newTxid(): string {
  return hex64(`tx-${state.nextTx++}`);
}

function tokenOf(id: TokenId): FixtureToken {
  const t = state.tokens.find((x) => x.tokenId === id);
  if (!t) throw new Error(`unknown token ${id}`);
  return t;
}

function poolsOf(id: TokenId): FixtureState["pools"] {
  return state.pools.filter((p) => p.tokenId === id);
}

const pow10 = (dec: number) => 10 ** dec;

/** Sats per whole token of reserves. */
function priceOf(sats: bigint, tokens: bigint, dec: number): number | null {
  if (tokens === 0n) return null;
  return Number(sats) / (Number(tokens) / pow10(dec));
}

/** The sats a buy needs to move the price of a constant-product pool up by `bps`: x·(√(1+bps/10⁴) − 1). */
function depthOf(sats: bigint, bps = 200): Depth | null {
  if (sats === 0n) return null;
  return { bps, sats: BigInt(Math.round(Number(sats) * (Math.sqrt(1 + bps / 10_000) - 1))) };
}

function summary(id: TokenId): Omit<PriceUpdate, "at"> {
  const t = tokenOf(id);
  const ps = poolsOf(id);
  const sats = ps.reduce((a, p) => a + p.sats, 0n);
  const tokens = ps.reduce((a, p) => a + p.tokens, 0n);
  return { tokenId: id, marginalPrice: priceOf(sats, tokens, t.dec), depth: depthOf(sats), pools: ps.length, reserves: { sats, tokens } };
}

function hostedOf(t: FixtureToken): HostedToken {
  const s = summary(t.tokenId);
  return {
    tokenId: t.tokenId,
    sym: t.sym,
    dec: t.dec,
    ...(t.icon ? { icon: t.icon } : {}),
    marginalPrice: s.marginalPrice,
    depth: s.depth,
    pools: s.pools,
    reserves: s.reserves,
    ...(t.change24h !== undefined ? { change24h: t.change24h } : {}),
  };
}

// ---------------------------------------------------------------------------
// Session and root
// ---------------------------------------------------------------------------

function fixtureMode(): "wallet" | "root" | null {
  try {
    const v = new URLSearchParams(location.search).get("fixture");
    return v === "wallet" || v === "root" ? v : null;
  } catch {
    return null;
  }
}

/**
 * The connected wallet. Real: `useWallet()` (status, identityKey) and
 * `useConnectDialog()` from wallet/AppWalletProvider (@1sat/react), which this
 * already uses; only the `?fixture=` override is fixture.
 */
export function useSession(): Session {
  const w = useWallet();
  const { openConnectDialog } = useConnectDialog();
  const [fixtureOff, setFixtureOff] = useState(false);
  const fx = fixtureMode();
  if (fx && !fixtureOff) {
    return { status: "connected", identityKey: fx === "root" ? ROOT_KEY : USER_KEY, connect: () => setFixtureOff(false), disconnect: () => setFixtureOff(true) };
  }
  const status = w.status === "connected" && w.identityKey ? "connected" : w.status === "disconnected" ? "disconnected" : "connecting";
  return { status, identityKey: status === "connected" ? w.identityKey : null, connect: openConnectDialog, disconnect: w.disconnect };
}

/**
 * Whether this identity holds the role `root` on this skein (it may register
 * and deregister tokens; Settings shows).
 *
 * NEW (skein#143 grants): the skein has no read that tells a page a key's
 * roles. Needed: a read of the roles granted to the requesting key (signed,
 * so the skein knows who asks), or of the grants list. Fixture: root under
 * `?fixture=root` only.
 */
export async function isRoot(identityKey: string | null): Promise<boolean> {
  await delay(LATENCY / 2);
  return identityKey !== null && identityKey === ROOT_KEY && fixtureMode() === "root";
}

// ---------------------------------------------------------------------------
// Landing: hosted tokens and live prices
// ---------------------------------------------------------------------------

/**
 * The tokens this overlay hosts, each with its pools summed, its marginal
 * price and depth. No wallet needed.
 *
 * Real: lib/overlay.ts `listTokenTopics(base)` (the registered topics,
 * `tm_mandala_<txid>_0` by `parseTokenTopic`), then per token
 * `queryPools(null, base, tokenId)` (ls_amm, unsigned) and
 * `fetchLiveByToken(base, topics)` for each pool's validator liveness;
 * market/view.ts `buildMarketView` / `marginalPrice` for the prices. Depth is
 * new arithmetic over the reserves (x·(√1.02 − 1) for ±2%, as here).
 * NEW: sym, dec and icon for a token the wallet does not hold (today the
 * pages read metadata only from the connected wallet's deploy output, README
 * "Not built"): from the token's deploy output through its lookup
 * `ls_mandala_<txid>_0` or the discovery topic. NEW: change24h needs a price
 * history; leave it out until the host keeps one.
 */
export async function hostedTokens(): Promise<HostedToken[]> {
  await delay(LATENCY);
  return state.tokens.filter((t) => t.registered).map(hostedOf);
}

/**
 * Live prices: calls `onUpdate` with a token's new summary whenever a trade,
 * deploy or close lands in one of its pools. Returns the unsubscribe.
 *
 * NEW (skein#148): a host subscription on the token topics' admitted outputs
 * (pool continuations), pushed to the page (SSE or a socket from the host);
 * each push re-summed as `hostedTokens` sums. Until it lands, polling
 * `queryPools` every lib/config.ts `REFRESH_MS` (as pages/Swap.tsx does) is
 * the stand-in. Fixture: actions here push, and a small simulated trade
 * lands every 6 s.
 */
export function subscribePrices(onUpdate: (u: PriceUpdate) => void): () => void {
  const push = () => {
    for (const t of state.tokens) if (t.registered) onUpdate({ ...summary(t.tokenId), at: Date.now() });
  };
  const off = onDataChange(push);
  const tick = setInterval(() => {
    // A small trade in a random pool: ±0.1% of its sats, constant product.
    const live = state.pools.filter((p) => tokenOf(p.tokenId).registered);
    const p = live[Math.floor(Math.random() * live.length)];
    if (!p) return;
    const k = p.sats * p.tokens;
    const d = (p.sats * BigInt(Math.random() < 0.5 ? -10 : 10)) / 10_000n;
    p.sats += d;
    p.tokens = k / p.sats;
    onUpdate({ ...summary(p.tokenId), at: Date.now() });
  }, 6_000);
  return () => {
    off();
    clearInterval(tick);
  };
}

/** The hosted tokens with live prices merged in: `hostedTokens()` once, then `subscribePrices()`. */
export function useHostedTokens(): { tokens: HostedToken[] | null; error: string | null; live: boolean; reload: () => void } {
  const r = useData(hostedTokens, []);
  const [prices, setPrices] = useState<Map<TokenId, PriceUpdate>>(new Map());
  const [live, setLive] = useState(false);
  useEffect(() => {
    setLive(true);
    const off = subscribePrices((u) => setPrices((m) => new Map(m).set(u.tokenId, u)));
    return () => {
      setLive(false);
      off();
    };
  }, []);
  const tokens = r.data?.map((t) => {
    const u = prices.get(t.tokenId);
    return u ? { ...t, marginalPrice: u.marginalPrice, depth: u.depth, pools: u.pools, reserves: u.reserves } : t;
  }) ?? null;
  return { tokens, error: r.error, live, reload: r.reload };
}

/**
 * A token's open pools (the Route panel's reserves and validators).
 *
 * Real: lib/overlay.ts `queryPools(null, base, tokenId)` and `fetchLive`;
 * market/plan.ts `livePools` keeps the pools whose validator is live.
 */
export async function tokenPools(tokenId: TokenId): Promise<PoolView[]> {
  await delay(LATENCY / 2);
  return poolsOf(tokenId).map(({ mine: _m, feesEarnedSats: _f, ...p }) => p);
}

// ---------------------------------------------------------------------------
// Swap
// ---------------------------------------------------------------------------

/**
 * A quote for one token, routed across its pools: legs, amount out,
 * effective and marginal price, price impact against marginal.
 *
 * Real: market/plan.ts `buildPlanRequest` + `quote(request, live, dec)` (the
 * matching engine's plan over `livePools`; `planView` gives the legs), with
 * the pools from `queryPools`. `minAmountOut` from `maxSlippageBps`, and
 * `allowPartial`, go into the plan request (the engine's slippage and
 * partial-fill options). Fixture: the amount is split across the pools in
 * proportion to their input-side reserves and each leg is constant product
 * after the validator's fees.
 */
export async function quoteSwap(req: QuoteRequest): Promise<Quote> {
  await delay(LATENCY);
  const t = tokenOf(req.tokenId);
  const ps = poolsOf(req.tokenId).filter((p) => p.validator.live);
  if (ps.length === 0) throw new Error(`${t.sym} has no open pool`);
  if (req.amountIn <= 0n) throw new Error("Enter an amount");
  const buy = req.side === "buy";
  const reserveIn = (p: PoolView) => (buy ? p.sats : p.tokens);
  const reserveOut = (p: PoolView) => (buy ? p.tokens : p.sats);
  const total = ps.reduce((a, p) => a + reserveIn(p), 0n);
  const feeBps = BigInt(FEES.lpBps + FEES.validatorBps);
  let left = req.amountIn;
  const legs: QuoteLeg[] = ps.map((p, i) => {
    const amountIn = i === ps.length - 1 ? left : (req.amountIn * reserveIn(p)) / total;
    left -= amountIn;
    const inAfterFee = (amountIn * (10_000n - feeBps)) / 10_000n;
    const amountOut = (reserveOut(p) * inAfterFee) / (reserveIn(p) + inAfterFee);
    return {
      poolOutpoint: p.outpoint,
      amountIn,
      amountOut,
      shareBps: Number((amountIn * 10_000n) / req.amountIn),
      reserves: { sats: p.sats, tokens: p.tokens },
      validator: p.validator,
    };
  });
  const used = legs.filter((l) => l.amountIn > 0n);
  const amountOut = used.reduce((a, l) => a + l.amountOut, 0n);
  if (amountOut === 0n) throw new Error("The amount is too small to receive anything");
  const s = summary(req.tokenId);
  const marginal = s.marginalPrice ?? 0;
  const sats = buy ? req.amountIn : amountOut;
  const tokens = buy ? amountOut : req.amountIn;
  const effective = Number(sats) / (Number(tokens) / pow10(t.dec));
  return {
    request: req,
    legs: used,
    amountOut,
    minAmountOut: (amountOut * BigInt(10_000 - req.maxSlippageBps)) / 10_000n,
    effectivePrice: effective,
    marginalPrice: marginal,
    impactBps: marginal ? Math.round((Math.abs(effective - marginal) / marginal) * 10_000) : 0,
    fees: FEES,
  };
}

/**
 * Runs a quoted swap with the connected wallet: one transaction per leg,
 * relayed to the leg's validator.
 *
 * Real: market/swapAction.ts `prepareSwap` (funding, the leg transactions,
 * the payouts) per leg, market/swapFlow.ts `relaySwap` → `settle` /
 * `checkAgain` over market/relay.ts `submitSwap` / `awaitSwap`
 * (`amm.swap.submit` on `/call`), wallet/pendingPayouts.ts for a payout the
 * wallet does not take in at once. Re-quote first and stop if
 * `goneFromLookup` reports a planned pool gone. With `allowPartial` a failed
 * leg leaves the others standing (status "partial").
 */
export async function executeSwap(quote: Quote): Promise<SwapResult> {
  await delay(900);
  const buy = quote.request.side === "buy";
  const legs: LegResult[] = quote.legs.map((l) => {
    const p = state.pools.find((x) => x.outpoint === l.poolOutpoint);
    if (!p) return { poolOutpoint: l.poolOutpoint, status: "failed", error: "pool spent since the quote" };
    if (buy) {
      p.sats += l.amountIn;
      p.tokens -= l.amountOut;
    } else {
      p.tokens += l.amountIn;
      p.sats -= l.amountOut;
    }
    const txid = newTxid();
    p.outpoint = `${txid}.0`; // the pool's continuation
    return { poolOutpoint: l.poolOutpoint, status: "filled", txid };
  });
  const w = state.wallet;
  const held = w.tokens.get(quote.request.tokenId) ?? { balance: 0n, outputs: 0 };
  if (buy) {
    w.sats -= quote.request.amountIn;
    w.tokens.set(quote.request.tokenId, { balance: held.balance + quote.amountOut, outputs: held.outputs + quote.legs.length });
  } else {
    w.sats += quote.amountOut;
    w.tokens.set(quote.request.tokenId, { ...held, balance: held.balance - quote.request.amountIn });
  }
  emit();
  const filled = legs.filter((l) => l.status === "filled").length;
  return { status: filled === legs.length ? "filled" : filled === 0 ? "failed" : "partial", legs };
}

/**
 * The connected wallet's spendable sats (the Swap page's "Wallet:" line).
 *
 * Real: the wallet's balance (`listOutputs` on the default basket, or
 * 1sat-sdk's balance action); lp/wallet.ts `loadWalletAssets` for token
 * outputs.
 */
export async function walletSats(): Promise<bigint> {
  await delay(LATENCY / 2);
  return state.wallet.sats;
}

// ---------------------------------------------------------------------------
// Liquidity
// ---------------------------------------------------------------------------

/**
 * The connected wallet's positions (its pools).
 *
 * Real: lp/myPools.ts `findMyPools` (the wallet's LP rows, lp/poolRows.ts
 * `isPoolRow`, matched to `queryPools` answers by lp/myPools.ts `matchPool`).
 * `feesEarnedSats` needs the deposit the pool opened with (from the deploy's
 * history, `historyOutpoints`); leave it out where that is not known.
 */
export async function myPositions(): Promise<Position[]> {
  await delay(LATENCY);
  return state.pools
    .filter((p) => p.mine)
    .map((p) => {
      const t = tokenOf(p.tokenId);
      return {
        outpoint: p.outpoint,
        tokenId: p.tokenId,
        sym: t.sym,
        dec: t.dec,
        ...(t.icon ? { icon: t.icon } : {}),
        sats: p.sats,
        tokens: p.tokens,
        ...(p.feesEarnedSats !== undefined ? { feesEarnedSats: p.feesEarnedSats } : {}),
        validator: p.validator,
      };
    });
}

/**
 * The validator a new position is deployed with, and its fees. Read-only:
 * the LP accepts the validator's fees (no fee input).
 *
 * Real: validator/control.ts `readAppPolicy` (the fees in the app record's
 * `config.amm`) and this skein's identity key; liveness from `fetchLive`.
 * lp/poolDeploy.ts `DEFAULT_LP_FEE_BPS` / `DEFAULT_VALIDATOR_FEE_BPS` are the
 * defaults.
 */
export async function validatorTerms(): Promise<ValidatorTerms> {
  await delay(LATENCY / 2);
  return { validator: { identityKey: THIS_VALIDATOR, live: true }, isThisExchange: true, fees: FEES };
}

/**
 * Deploys a new position: the wallet funds a pool with `tokens` and `sats`
 * at the validator's fees.
 *
 * Real: lp/poolDeploy.ts `planPoolDeploy` → `preparePoolDeploy` (the funding
 * and the pool output; `deriveLpKey` for the LP's key) and
 * `completePoolDeploy` / `abandonPoolDeploy`; lp/deployFlow.ts today relays
 * it (`relayPoolDeploy`, `amm.pool.submit`).
 * NEW: the deploy is delivered to the skein, which adds its claim and
 * broadcasts (no longer a relay to a validator that signs); the message and
 * its answer are the overlay session's to define.
 */
export async function deployPosition(req: DeployRequest): Promise<DeployResult> {
  await delay(900);
  const t = tokenOf(req.tokenId);
  const held = state.wallet.tokens.get(req.tokenId);
  if (!held || held.balance < req.tokens) throw new Error(`Your wallet holds fewer ${t.sym} than that`);
  if (state.wallet.sats < req.sats) throw new Error("Your wallet holds fewer sats than that");
  if (req.tokens <= 0n || req.sats <= 0n) throw new Error("Both amounts must be more than 0");
  const txid = newTxid();
  const outpoint = `${txid}.0`;
  state.pools.push({ outpoint, tokenId: req.tokenId, sats: req.sats, tokens: req.tokens, validator: { identityKey: THIS_VALIDATOR, live: true }, mine: true, feesEarnedSats: 0n });
  state.wallet.sats -= req.sats;
  state.wallet.tokens.set(req.tokenId, { ...held, balance: held.balance - req.tokens });
  emit();
  return { outpoint, txid };
}

/**
 * Closes a position: everything in the pool returns to the wallet, less
 * `bsvFee` (the miner fee left from the pool; 0 = funded from another wallet
 * input).
 *
 * Real: lp/removeLiquidity.ts `prepareRemoveLiquidity` /
 * `completeRemoveLiquidity` / `submitToOverlay` (today a remove of a share,
 * with the LP key from lp/myPools.ts).
 * NEW: the contract's close with `bsvFee` (the whole pool out, the fee taken
 * from the pool's sats or, at 0, from a wallet input).
 */
export async function closePosition(req: CloseRequest): Promise<CloseResult> {
  await delay(900);
  const i = state.pools.findIndex((p) => p.outpoint === req.outpoint && p.mine);
  if (i < 0) throw new Error("No such position");
  const p = state.pools[i]!;
  if (req.bsvFee < 0n || req.bsvFee >= p.sats) throw new Error("The fee must be at least 0 and less than the pool's sats");
  state.pools.splice(i, 1);
  const sats = p.sats - req.bsvFee;
  state.wallet.sats += sats;
  const held = state.wallet.tokens.get(p.tokenId) ?? { balance: 0n, outputs: 0 };
  state.wallet.tokens.set(p.tokenId, { balance: held.balance + p.tokens, outputs: held.outputs + 1 });
  emit();
  return { txid: newTxid(), sats, tokens: p.tokens };
}

// ---------------------------------------------------------------------------
// Your tokens
// ---------------------------------------------------------------------------

/**
 * The connected wallet's own Mandala tokens with balance, each marked on this
 * exchange or not.
 *
 * Real: lp/wallet.ts `loadWalletAssets` + lp/inventory.ts `buildInventory`
 * (balances, sym/dec, the deploy's icon), `onExchange` against
 * lib/overlay.ts `listTokenTopics`.
 */
export async function myTokens(): Promise<WalletToken[]> {
  await delay(LATENCY);
  const asked = new Set(state.requests.map((r) => r.tokenId));
  return [...state.wallet.tokens.entries()]
    .filter(([, h]) => h.balance > 0n)
    .map(([id, h]) => {
      const t = tokenOf(id);
      return { tokenId: id, sym: t.sym, dec: t.dec, ...(t.icon ? { icon: t.icon } : {}), balance: h.balance, outputs: h.outputs, onExchange: t.registered, requested: asked.has(id) };
    });
}

/**
 * "Add to this exchange": a holder asks root to list a token. No billing yet
 * (a paid route later, skein#147).
 *
 * NEW: a message from the holder to the app (e.g. a box `amm/requests`,
 * recorded for root; skein#143 roles: anyone may send, nothing runs) and the
 * read Settings lists them from.
 */
export async function requestListing(tokenId: TokenId): Promise<void> {
  await delay(LATENCY * 2);
  const t = tokenOf(tokenId);
  if (!state.requests.some((r) => r.tokenId === tokenId)) state.requests.push({ tokenId, sym: t.sym, from: USER_KEY, at: Date.now() });
  emit();
}

// ---------------------------------------------------------------------------
// Settings (root)
// ---------------------------------------------------------------------------

/**
 * The tokens on the discovery topic (`tm_mandala`, always on) that this
 * exchange does not serve yet.
 *
 * Real: a lookup on the discovery service (`ls_mandala`) as the Mandala
 * Token topics page reads it (www/mandala/tokens/, skein-mandala), less
 * `listTokenTopics`.
 */
export async function discoveryTokens(): Promise<DiscoveryToken[]> {
  await delay(LATENCY);
  return state.tokens
    .filter((t) => !t.registered)
    .map((t) => ({ tokenId: t.tokenId, sym: t.sym, dec: t.dec, ...(t.icon ? { icon: t.icon } : {}), seenAt: t.discoveredAt }));
}

/** The tokens registered on this exchange. Real: lib/overlay.ts `listTokenTopics(base)`. */
export async function registeredTokens(): Promise<RegisteredToken[]> {
  await delay(LATENCY);
  return state.tokens.filter((t) => t.registered).map((t) => ({ tokenId: t.tokenId, sym: t.sym, ...(t.icon ? { icon: t.icon } : {}) }));
}

/** The topic a token's pools are admitted under (BRC-207; skein-amm 0.8.0): derived, never typed. */
export function topicOf(tokenId: TokenId): string {
  return `tm_mandala_${tokenId.includes("_") ? tokenId : `${tokenId}_0`}`;
}

/** The lookup service of a token (BRC-207): derived, never typed. */
export function lookupOf(tokenId: TokenId): string {
  return `ls_mandala_${tokenId.includes("_") ? tokenId : `${tokenId}_0`}`;
}

/**
 * Registers a token: its topic, then its lookup (names from `topicOf` /
 * `lookupOf`). Root only.
 *
 * Real: the register path the Mandala Token topics page sends today
 * (www/mandala/tokens/, skein-mandala; README.md step 3): root's signed
 * messages to the box `amm/register`, `{fn: "register", args: {topic,
 * program: "mandala-topic"}}` then `{fn: "registerLookup", args: {service,
 * program: "mandala-lookup", topics: [topic]}}`. A listing request for the
 * token is settled by it.
 */
export async function registerToken(tokenId: TokenId): Promise<void> {
  await delay(LATENCY * 2);
  tokenOf(tokenId).registered = true;
  state.requests = state.requests.filter((r) => r.tokenId !== tokenId);
  emit();
}

/**
 * Deregisters a token: its lookup, then its topic. Root only.
 *
 * Real: `{fn: "deregisterLookup", args: {service}}` then `{fn: "deregister",
 * args: {topic}}` to `amm/register` (README.md step 3), as the Mandala Token
 * topics page sends them.
 */
export async function deregisterToken(tokenId: TokenId): Promise<void> {
  await delay(LATENCY * 2);
  tokenOf(tokenId).registered = false;
  emit();
}

/**
 * Holders' requests to list a token, newest first; null when the skein
 * offers no such read (Settings then hides the panel).
 *
 * NEW: the read of the requests `requestListing` records.
 */
export async function listingRequests(): Promise<ListingRequest[] | null> {
  await delay(LATENCY);
  return [...state.requests].sort((a, b) => b.at - a.at);
}

// ---------------------------------------------------------------------------
// Hooks over the functions above
// ---------------------------------------------------------------------------

export interface Data<T> {
  data: T | null;
  error: string | null;
  loading: boolean;
  reload: () => void;
}

/** Runs `load` on mount, when `deps` change and after any action (`onDataChange`). */
export function useData<T>(load: () => Promise<T>, deps: unknown[], enabled = true): Data<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(enabled);
  const [n, setN] = useState(0);
  const loadRef = useRef(load);
  loadRef.current = load;
  const reload = useCallback(() => setN((x) => x + 1), []);
  useEffect(() => onDataChange(reload), [reload]);
  useEffect(() => {
    if (!enabled) {
      setLoading(false);
      return;
    }
    let live = true;
    setLoading(true);
    loadRef.current().then(
      (d) => {
        if (!live) return;
        setData(d);
        setError(null);
        setLoading(false);
      },
      (e: unknown) => {
        if (!live) return;
        setError(e instanceof Error ? e.message : String(e));
        setLoading(false);
      },
    );
    return () => {
      live = false;
    };
  }, [...deps, n, enabled]);
  return { data, error, loading, reload };
}

export const useMyPositions = (enabled: boolean) => useData(myPositions, [], enabled);
export const useMyTokens = (enabled: boolean) => useData(myTokens, [], enabled);
export const useValidatorTerms = () => useData(validatorTerms, []);
export const useWalletSats = (enabled: boolean) => useData(walletSats, [], enabled);
export const useDiscoveryTokens = (enabled: boolean) => useData(discoveryTokens, [], enabled);
export const useRegisteredTokens = (enabled: boolean) => useData(registeredTokens, [], enabled);
export const useListingRequests = (enabled: boolean) => useData(listingRequests, [], enabled);
export const useTokenPools = (tokenId: TokenId | undefined) =>
  useData(() => (tokenId ? tokenPools(tokenId) : Promise.resolve([])), [tokenId]);

/** Whether the session's key is root (`isRoot`), false while disconnected. */
export function useIsRoot(session: Session): boolean {
  const r = useData(() => isRoot(session.identityKey), [session.identityKey], session.status === "connected");
  return session.status === "connected" && r.data === true;
}
