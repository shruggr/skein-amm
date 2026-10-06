/**
 * The instance client against the skein instance's AMM app routes, all
 * under one base URL (`AMM_OVERLAY`, src/lib/config.ts: the
 * app's base URL, `https://<handle>.<host>/amm` or `<host>/@<handle>/amm`):
 *
 *   GET  <base>/listTopicManagers            {tm_<txid>: {name, shortDescription}, …}: the registered token topics
 *   GET  <base>/listLookupServiceProviders   {ls_amm: …, ls_mandala: …, ls_mandala_deploys: …}
 *   POST <base>/lookup   {service: "ls_amm", query: {tokenId, …}}   BRC-24
 *   GET  <base>/live                          {now, thresholdMs, validators: [...]}
 *
 * A skein takes no unsigned HTTP but GET/HEAD: a POST without a BRC-104
 * session is 401. The GETs are plain `fetch`; every POST (the lookups here,
 * the submit in src/lp/removeLiquidity.ts) goes through the connected
 * wallet's `AuthFetch` (src/wallet/authFetch.ts), a `SignedFetch`.
 *
 * Why not the stock `@bsv/sdk` `LookupResolver` / `TopicBroadcaster`:
 *  - `LookupResolver.query`/`queryDetailed` merge output-list answers only;
 *    a freeform answer (amm-lookup's `{}` and `{outpoint}`) is dropped.
 *  - `TopicBroadcaster` refuses any topic not matching `^tm_[a-z]+(_[a-z]+)*$`
 *    of at most 50 characters, before any request; `tm_<64 hex>` fails both.
 * The page never submits anything (it broadcasts nothing), so there is no
 * submit client here.
 *
 * Names (skein-mandala docs/MANDALA.md "The topic"; programs/amm-lookup/README.md
 * "Queries"): a token deployed at output 0 is the topic `tm_<txid>`, a BRC-161
 * token deployed at a non-zero output `tm_<txid>_<vout>`. One lookup service,
 * `ls_amm`, answers for every token topic the overlay serves; each query
 * names its token (`tokenId`: `<txid>`, `<txid>_<vout>` or `<txid>.<vout>`).
 */
import type { PoolState } from "@amm-poc/matching-engine";

// ---------------------------------------------------------------------------
// Names
// ---------------------------------------------------------------------------

export interface TokenTopic {
  /** `tm_<txid>` or `tm_<txid>_<vout>`. */
  topic: string;
  /** Deploy txid, 64 lowercase hex, display order. */
  txid: string;
  vout: number;
  kind: "native" | "legacy";
  /** `<txid>_<vout>`: the wallet's (and 1sat-sdk's) token id form; what an `ls_amm` query names. */
  tokenId: string;
}

/** The AMM pool lookup service (programs/amm-lookup): one service over every token topic. */
export const AMM_LOOKUP_SERVICE = "ls_amm";

const TOPIC = /^tm_([0-9a-f]{64})(?:_(\d+))?$/;

/** A token topic name, or null for anything else (`tm_demo`, a `-live` topic, ...). */
export function parseTokenTopic(name: string): TokenTopic | null {
  const m = TOPIC.exec(name);
  if (!m) return null;
  const txid = m[1]!;
  const legacy = m[2] !== undefined;
  const vout = legacy ? Number(m[2]) : 0;
  return { topic: name, txid, vout, kind: legacy ? "legacy" : "native", tokenId: `${txid}_${vout}` };
}

// ---------------------------------------------------------------------------
// Wire shapes
// ---------------------------------------------------------------------------

export type ServiceListing = Record<string, { name: string; shortDescription?: string }>;

/**
 * The lookup's pool record on the wire (programs/amm-lookup/README.md, "The
 * engine's PoolState shape"): JSON numbers, so they become bigints here. A
 * reserve above 2^53 would already have lost precision in `JSON.parse`; that
 * is the instance's JSON encoding (amm-lookup README), not fixed here.
 */
interface WirePoolState {
  outpoint: string;
  bsvReserve: number;
  tokenReserve: number;
  liquidityFeeBps: number;
  validationFeeBps: number;
  /** The pool's CommissionBps. Absent from a lookup that predates the commission: read as 0 (planSwap re-checks the leg against the script). */
  commissionBps?: number;
  validatorIdentityKey: string;
  lastSeen?: number;
}

export function toPoolState(raw: WirePoolState): PoolState {
  return {
    outpoint: raw.outpoint,
    bsvReserve: BigInt(raw.bsvReserve),
    tokenReserve: BigInt(raw.tokenReserve),
    liquidityFeeBps: BigInt(raw.liquidityFeeBps),
    validationFeeBps: BigInt(raw.validationFeeBps),
    commissionBps: BigInt(raw.commissionBps ?? 0),
    validatorIdentityKey: raw.validatorIdentityKey,
    ...(raw.lastSeen === undefined || raw.lastSeen === null ? {} : { lastSeen: raw.lastSeen }),
  };
}

/**
 * A `POST /lookup` freeform answer as `PoolState[]`: `{}` answers
 * `{type: "freeform", result: PoolState[]}`, `{outpoint}` answers
 * `{type: "freeform", result: {hops, current}}` (one element here). Anything
 * else is `[]`.
 */
export function parseLookupAnswer(answer: unknown): PoolState[] {
  const a = answer as { type?: string; result?: unknown } | unknown[];
  const result = Array.isArray(a) ? a : a && typeof a === "object" && a.type === "freeform" ? a.result : undefined;
  if (Array.isArray(result)) return (result as WirePoolState[]).map(toPoolState);
  if (result && typeof result === "object" && "current" in result) {
    return [toPoolState((result as { current: WirePoolState }).current)];
  }
  return [];
}

/** One output of an `output-list` answer: the BEEF holding it and its index. */
export interface LookupOutput {
  beef: number[];
  outputIndex: number;
}

/** The `{outpoint, beef: true}` answer: `{type: "output-list", outputs: [{beef: number[], outputIndex}]}`. */
export function parseOutputList(answer: unknown): LookupOutput[] {
  const a = answer as { type?: string; outputs?: unknown };
  if (!a || a.type !== "output-list" || !Array.isArray(a.outputs)) return [];
  return (a.outputs as { beef?: unknown; outputIndex?: unknown }[])
    .filter((o) => Array.isArray(o.beef) && typeof o.outputIndex === "number")
    .map((o) => ({ beef: o.beef as number[], outputIndex: o.outputIndex as number }));
}

/** A validator from `GET /amm/live` (programs/amm-p2p `liveHttp`). */
export interface LiveValidator {
  identityKey: string;
  /** libp2p peer ID, text form. */
  peerId: string;
  /** Last heartbeat, ms epoch. */
  at: number;
  ageMs: number;
  /** Last heartbeat within the instance's threshold. */
  live: boolean;
}

export interface LiveAnswer {
  now: number;
  thresholdMs: number;
  validators: LiveValidator[];
}

/** `{now, thresholdMs, validators: [{identityKey, peerId, at, ageMs, live}]}`; tolerant of missing fields. */
export function parseLiveAnswer(answer: unknown): LiveAnswer {
  const a = (answer ?? {}) as { now?: unknown; thresholdMs?: unknown; validators?: unknown };
  const now = typeof a.now === "number" ? a.now : Date.now();
  const thresholdMs = typeof a.thresholdMs === "number" ? a.thresholdMs : 90_000;
  const validators: LiveValidator[] = [];
  for (const v of Array.isArray(a.validators) ? (a.validators as Record<string, unknown>[]) : []) {
    if (typeof v.identityKey !== "string") continue;
    const at = typeof v.at === "number" ? v.at : 0;
    const ageMs = typeof v.ageMs === "number" ? v.ageMs : Math.max(0, now - at);
    validators.push({
      identityKey: v.identityKey.toLowerCase(),
      peerId: typeof v.peerId === "string" ? v.peerId : typeof v.peerIdText === "string" ? v.peerIdText : "",
      at,
      ageMs,
      live: typeof v.live === "boolean" ? v.live : ageMs <= thresholdMs,
    });
  }
  return { now, thresholdMs, validators };
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

/** The connected wallet's BRC-104 client (`@bsv/sdk` `AuthFetch`, or a test's fake): every POST to the instance. */
export interface SignedFetch {
  fetch(url: string, config?: { method?: string; headers?: Record<string, string>; body?: unknown }): Promise<Response>;
}

/** The signed client, or an error saying a wallet is needed (null: none connected). */
export function needSigned(af: SignedFetch | null | undefined): SignedFetch {
  if (!af) throw new Error("connect a wallet: a lookup or submit is a POST, which the instance takes only signed (BRC-104)");
  return af;
}

async function getJson(url: string, init?: RequestInit): Promise<unknown> {
  const res = await fetch(url, init);
  if (!res.ok) throw new Error(`${init?.method ?? "GET"} ${url}: ${res.status} ${(await res.text()).slice(0, 200)}`);
  return res.json();
}

export async function listTopicManagers(base: string): Promise<ServiceListing> {
  return (await getJson(`${base}/listTopicManagers`)) as ServiceListing;
}

export async function listLookupServiceProviders(base: string): Promise<ServiceListing> {
  return (await getJson(`${base}/listLookupServiceProviders`)) as ServiceListing;
}

/** The token topics the instance serves, in listing order (non-token topics skipped). */
export async function listTokenTopics(base: string): Promise<TokenTopic[]> {
  const topics = await listTopicManagers(base);
  return Object.keys(topics).map(parseTokenTopic).filter((t): t is TokenTopic => t !== null);
}

/** `POST <base>/lookup {service, query}`, signed. */
export async function lookup(af: SignedFetch | null, base: string, service: string, query: Record<string, unknown>): Promise<unknown> {
  const url = `${base}/lookup`;
  const res = await needSigned(af).fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ service, query }),
  });
  if (!res.ok) throw new Error(`POST ${url}: ${res.status} ${(await res.text()).slice(0, 200)}`);
  return res.json();
}

/**
 * Every live pool of a token (`{tokenId}`) or the newest continuation of an
 * outpoint (`{tokenId, outpoint}`), from `ls_amm`. `tokenId` is the token's
 * id (`<txid>_<vout>`, `<txid>`).
 */
export async function queryPools(
  af: SignedFetch | null,
  base: string,
  tokenId: string,
  query: { outpoint: string } | Record<string, never> = {},
): Promise<PoolState[]> {
  return parseLookupAnswer(await lookup(af, base, AMM_LOOKUP_SERVICE, { tokenId, ...query }));
}

/** The pool output (or its newest continuation) with its BEEF: `{tokenId, outpoint, beef: true}`. */
export async function lookupPoolOutput(af: SignedFetch | null, base: string, tokenId: string, outpoint: string): Promise<LookupOutput> {
  const outs = parseOutputList(await lookup(af, base, AMM_LOOKUP_SERVICE, { tokenId, outpoint, beef: true }));
  if (outs.length === 0) throw new Error(`lookup ${AMM_LOOKUP_SERVICE} ${tokenId}: no output for ${outpoint}`);
  return outs[0]!;
}

export async function fetchLive(base: string): Promise<LiveAnswer> {
  return parseLiveAnswer(await getJson(`${base}/live`));
}
