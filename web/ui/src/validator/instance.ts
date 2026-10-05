/**
 * The Validator page's view of "this instance" (pure, plus plain fetches):
 *
 * - origin: the origin of `VITE_AMM_OVERLAY` (the instance answers its own
 *   routes, the messagebox and `/.well-known/auth` there);
 * - handle: the first label of the origin's host (`amm2.localhost:8300` →
 *   `amm2` at `localhost:8300`), or the `/@<handle>/` dev prefix;
 * - identity key: the router's BRC-169 answer for that handle
 *   (src/lp/validators.ts `resolveHandle`: `/manifest.json` → resolve);
 * - peer ID and liveness: what the OTHER node's `GET /amm/live` reports for
 *   that identity. A node never hears its own heartbeat (GossipSub
 *   `emitSelf: false`), so this instance's own `/amm/live` cannot say.
 *
 * And the pools this instance serves as a validator: the lookup's pools whose
 * `validatorIdentityKey` is this instance's identity.
 */
import type { PoolState } from "@amm-poc/matching-engine";
import type { LiveAnswer, LiveValidator, TokenTopic } from "../lib/overlay";
import { parseLiveAnswer } from "../lib/overlay";
import { findLive, resolveHandle, type FetchLike, type ResolvedHandle } from "../lp/validators";

export interface InstanceAddress {
  /** `http://amm2.localhost:8300` */
  origin: string;
  /** `amm2`, when the origin names one. */
  name?: string;
  /** `localhost:8300`: where the handle's router answers BRC-169. */
  domain?: string;
}

/** The instance's origin and handle from its AMM base URL (`http://amm2.localhost:8300/amm`). */
export function instanceAddress(base: string): InstanceAddress {
  const u = new URL(base);
  const origin = u.origin;
  // Dev form: http://<host>:<port>/@<handle>/…
  const at = /^\/@([A-Za-z0-9._-]+)(?:\/|$)/.exec(u.pathname);
  if (at) return { origin: `${origin}/@${at[1]}`, name: at[1]!, domain: u.host };
  const labels = u.hostname.split(".");
  const ip = /^\d+\.\d+\.\d+\.\d+$/.test(u.hostname) || u.hostname.startsWith("[");
  if (labels.length < 2 || ip) return { origin };
  return { origin, name: labels[0]!, domain: labels.slice(1).join(".") + (u.port ? `:${u.port}` : "") };
}

/** `amm3.localhost:8400` for `http://amm3.localhost:8400/amm`. */
export function hostLabel(base: string): string {
  try {
    return new URL(base).host;
  } catch {
    return base;
  }
}

export interface PeerView {
  /** The peer's AMM base. */
  base: string;
  /** `amm3.localhost:8400` */
  label: string;
  /** The peer's whole live list (null when it could not be read). */
  live: LiveAnswer | null;
  /** The peer's entry for our identity, if it has heard our heartbeat. */
  entry?: LiveValidator;
  error?: string;
}

export interface ThisInstance {
  address: InstanceAddress;
  /** `amm2@localhost:8300` when known. */
  handle?: string;
  resolved?: ResolvedHandle;
  /** Why the identity is unknown. */
  resolveError?: string;
  identityKey?: string;
  /** From the peer's live list; absent until a peer sees us. */
  peerId?: string;
  peer?: PeerView;
}

const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

async function getLive(base: string, fetchFn: FetchLike): Promise<LiveAnswer> {
  const res = await fetchFn(`${base}/live`, { headers: { accept: "application/json" } });
  if (!res.ok) throw new Error(`GET ${base}/live: ${res.status}`);
  return parseLiveAnswer(await res.json());
}

/**
 * Reads what "this instance" is: handle → identity (BRC-169), then the
 * peer's live list for that identity's peer ID and last heartbeat.
 * `peerBase` "" skips the peer.
 */
export async function loadThisInstance(base: string, peerBase: string, fetchFn: FetchLike = fetch): Promise<ThisInstance> {
  const address = instanceAddress(base);
  const out: ThisInstance = { address };
  if (address.name && address.domain) {
    out.handle = `${address.name}@${address.domain}`;
    try {
      out.resolved = await resolveHandle(address.name, address.domain, fetchFn);
      out.identityKey = out.resolved.identityKey;
    } catch (e) {
      out.resolveError = errText(e);
    }
  } else {
    out.resolveError = `the origin ${address.origin} names no handle (expected <handle>.<domain> or /@<handle>/)`;
  }
  if (peerBase) {
    const peer: PeerView = { base: peerBase, label: hostLabel(peerBase), live: null };
    try {
      peer.live = await getLive(peerBase, fetchFn);
      const entry = out.identityKey ? findLive(out.identityKey, peer.live) : undefined;
      if (entry) {
        peer.entry = entry;
        if (entry.peerId) out.peerId = entry.peerId;
      }
    } catch (e) {
      peer.error = errText(e);
    }
    out.peer = peer;
  }
  return out;
}

/** The liveness line: "last heartbeat seen by amm3.localhost:8400: 12 s ago (live)", or why not. */
export function livenessLine(t: ThisInstance, ago: (ms: number) => string): string {
  const p = t.peer;
  if (!p) return "no peer configured (VITE_AMM_PEER_OVERLAY): liveness unknown";
  if (p.error) return `could not read ${p.label}/live: ${p.error}`;
  if (!t.identityKey) return "unknown: this instance's identity key is unknown";
  if (!p.entry) return `${p.label} has not seen a heartbeat from this identity`;
  return `last heartbeat seen by ${p.label}: ${ago(p.entry.ageMs)} (${p.entry.live ? "live" : "offline"})`;
}

export interface ServedPool {
  topic: TokenTopic;
  pool: PoolState;
}

/**
 * The pools whose validator identity is `identityKey`, across the token
 * topics, from each topic's lookup answer (an Error answer is skipped and
 * reported by the caller).
 */
export function poolsServedBy(
  identityKey: string,
  topics: TokenTopic[],
  pools: Map<string, PoolState[] | Error>,
): ServedPool[] {
  const k = identityKey.toLowerCase();
  const out: ServedPool[] = [];
  for (const topic of topics) {
    const answer = pools.get(topic.tokenId);
    if (!answer || answer instanceof Error) continue;
    for (const pool of answer) if (pool.validatorIdentityKey.toLowerCase() === k) out.push({ topic, pool });
  }
  return out;
}
