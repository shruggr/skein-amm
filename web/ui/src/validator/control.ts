/**
 * The owner's controls over this instance as a validator, over HTTP, signed
 * by the connected BRC-100 wallet through `@bsv/sdk`'s `AuthFetch` (BRC-103
 * handshake at `<origin>/.well-known/auth`, every request BRC-104-signed):
 *
 * - heartbeat start / stop: a message into the app's box `amm/amm-p2p`
 *   (the manifest's `"amm-p2p"`, relative to the app: shruggr/skein#128)
 *   through the stock BRC-33 messagebox route, `POST <origin>/sendMessage`
 *   (skein docs/MESSAGES.md "The messagebox"; programs/messagebox
 *   `sendMessage`):
 *
 *     {message: {recipient: <the instance's identity key, hex>,
 *                messageBox: "amm/amm-p2p",
 *                body: {kind: "amm-p2p-start"} | {kind: "amm-p2p-stop"}}}
 *
 *   Start asks the host for a beacon per served token topic (`tm_<txid>-live`,
 *   skein-amm 0.2.0); stop ends them.
 *
 *   The sender is the key the session proved. For the instance's own key as
 *   recipient the messagebox admits the message when the subscription table
 *   has an entry for (sender, box), else 403 `ERR_NOT_SUBSCRIBED`. v2's
 *   genesis subscribes `{box: "amm-p2p", handler: "amm-p2p"}` with no
 *   sender (anyone: it also carries peers' admitted heartbeats and the cron
 *   ticks), so the messagebox admits anyone's start/stop (200) and amm-p2p
 *   itself refuses a sender that is neither `in.owner` nor the cron
 *   provider by erroring its step (`NotTheOwner`). That refusal is on the
 *   instance's thread, not in the HTTP answer: a 200 means "admitted", and
 *   only the peer's `/amm/live` shows whether the heartbeat runs.
 *
 * - the explorer (`GET <origin>/explore…`, read op `explore`, the owner only
 *   by the stock reads table; others get 403): the genesis (log entry 0,
 *   `{kind: "genesis", owner, identity, defaults: {ammP2p, ammValidator, …}}`)
 *   gives the owner key and the validator's policy.
 */

/** What the page needs of `AuthFetch` (and what the tests fake). */
export interface AuthFetchLike {
  fetch(
    url: string,
    config?: { method?: string; headers?: Record<string, string>; body?: string },
  ): Promise<Pick<Response, "status" | "text">>;
}

export const AMM_P2P_BOX = "amm/amm-p2p";

export type HeartbeatAction = "start" | "stop";

export interface SendMessageRequest {
  message: {
    recipient: string;
    messageBox: string;
    body: { kind: "amm-p2p-start" } | { kind: "amm-p2p-stop" };
  };
}

/**
 * The BRC-33 sendMessage body: start (the beacons) or stop (every beacon
 * ended). Neither names `jobs`, which is amm-p2p's cron fallback.
 */
export function heartbeatRequest(action: HeartbeatAction, instanceIdentityKey: string): SendMessageRequest {
  return {
    message: {
      recipient: instanceIdentityKey.toLowerCase(),
      messageBox: AMM_P2P_BOX,
      body: action === "start" ? { kind: "amm-p2p-start" } : { kind: "amm-p2p-stop" },
    },
  };
}

export interface HeartbeatResult {
  action: HeartbeatAction;
  url: string;
  request: SendMessageRequest;
  status: number;
  /** The answer as JSON when it is JSON, else its text. */
  answer: unknown;
  /** 200 with `status: "success"`: the messagebox admitted the message. */
  admitted: boolean;
  /** The admitted mail record's CID (`id`). */
  id?: string;
  /** The refusal's code / description (403 `ERR_NOT_SUBSCRIBED`, 401, …). */
  refusal?: string;
}

function parseBody(text: string): unknown {
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

/** `POST <instance>/sendMessage` through the wallet-backed AuthFetch. */
export async function sendHeartbeatControl(
  authFetch: AuthFetchLike,
  instanceUrl: string,
  instanceIdentityKey: string,
  action: HeartbeatAction,
): Promise<HeartbeatResult> {
  const url = `${instanceUrl.replace(/\/+$/, "")}/sendMessage`;
  const request = heartbeatRequest(action, instanceIdentityKey);
  const res = await authFetch.fetch(url, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(request),
  });
  const answer = parseBody(await res.text());
  const a = (answer && typeof answer === "object" ? answer : {}) as Record<string, unknown>;
  const admitted = res.status === 200 && a.status === "success";
  const out: HeartbeatResult = { action, url, request, status: res.status, answer, admitted };
  if (typeof a.id === "string") out.id = a.id;
  if (!admitted) {
    const code = typeof a.code === "string" ? a.code : `HTTP ${res.status}`;
    const desc = typeof a.description === "string" ? a.description : typeof answer === "string" ? answer.slice(0, 200) : "";
    out.refusal = desc ? `${code}: ${desc}` : code;
  }
  return out;
}

/** The by-hand fallback from the v2 deploy README (run on the host, as the cron provider). */
export function byHandCommand(handleName: string | undefined): string {
  const h = handleName ?? "<handle>";
  const home = h === "amm3" ? "$PWD/deploy/.run/amm3/home" : "$PWD/deploy/.run/home";
  return `# in the deploy's checkout (v2: ~/Work/agent-env/amm/amm-poc)\nSKEIN_HOME=${home} ~/Work/agent-env/skein/bin/skein-host event ${h} amm-p2p '{"kind":"amm-p2p-start"}'`;
}

// ---------------------------------------------------------------------------
// The explorer (owner only): the genesis, its owner and defaults
// ---------------------------------------------------------------------------

export interface ValidatorPolicy {
  minValidatorFeeBps?: number;
  maxLpFeeBps?: number;
  heartbeatSeconds?: number;
  offlineSeconds?: number;
  peerId?: string;
}

export interface GenesisRead {
  ok: boolean;
  status: number;
  /** The genesis's owner key (hex), when read. */
  owner?: string;
  /** The genesis's identity key (hex), when read. */
  identity?: string;
  policy?: ValidatorPolicy;
  /** Why it could not be read. */
  error?: string;
}

/** DAG-JSON bytes `{"/": {"bytes": "<base64, unpadded>"}}` → hex. */
export function dagBytesHex(v: unknown): string | undefined {
  const b = (v as { "/"?: { bytes?: unknown } } | null)?.["/"]?.bytes;
  if (typeof b !== "string") return typeof v === "string" && /^[0-9a-f]+$/i.test(v) ? v.toLowerCase() : undefined;
  const s = b.replace(/-/g, "+").replace(/_/g, "/");
  const padded = s + "=".repeat((4 - (s.length % 4)) % 4);
  try {
    const bin = atob(padded);
    let hex = "";
    for (let i = 0; i < bin.length; i++) hex += bin.charCodeAt(i).toString(16).padStart(2, "0");
    return hex;
  } catch {
    return undefined;
  }
}

/** A DAG-JSON link's CID, else undefined. */
function linkOf(v: unknown): string | undefined {
  const l = (v as { "/"?: unknown } | null)?.["/"];
  return typeof l === "string" ? l : undefined;
}

/** A genesis default: a JSON string (as the genesis keeps `ammP2p` / `ammValidator`) or an object. */
function jsonDefault(v: unknown): Record<string, unknown> | undefined {
  if (typeof v === "string") {
    try {
      const p = JSON.parse(v) as unknown;
      return p && typeof p === "object" ? (p as Record<string, unknown>) : undefined;
    } catch {
      return undefined;
    }
  }
  return v && typeof v === "object" ? (v as Record<string, unknown>) : undefined;
}

const num = (v: unknown) => (typeof v === "number" ? v : undefined);

/**
 * The policy from a genesis record's `defaults` (JSON strings), or from an
 * installed app record's `config.amm` (`{kind: "app", config: {amm:
 * {ammValidator, ammP2p}}}`), which the programs read first.
 */
export function policyOf(rec: Record<string, unknown>): ValidatorPolicy {
  const amm = rec.kind === "app" ? (((rec.config ?? {}) as Record<string, unknown>).amm ?? {}) : undefined;
  const d = (amm ?? rec.defaults ?? {}) as Record<string, unknown>;
  const v = jsonDefault(d.ammValidator) ?? {};
  const p = jsonDefault(d.ammP2p) ?? {};
  const out: ValidatorPolicy = {};
  const set = <K extends keyof ValidatorPolicy>(k: K, x: ValidatorPolicy[K] | undefined) => {
    if (x !== undefined) out[k] = x;
  };
  set("minValidatorFeeBps", num(v.minValidatorFeeBps));
  set("maxLpFeeBps", num(v.maxLpFeeBps));
  set("heartbeatSeconds", num(p.heartbeatSeconds));
  set("offlineSeconds", num(p.offlineSeconds));
  set("peerId", typeof p.peerId === "string" ? p.peerId : undefined);
  return out;
}

function isGenesis(v: unknown): v is Record<string, unknown> {
  return !!v && typeof v === "object" && (v as { kind?: unknown }).kind === "genesis";
}

/**
 * Reads the genesis through the explorer as the connected wallet:
 * `GET <instance>/explore/log?before=1&limit=1` (log entry 0, with its record
 * as DAG-JSON); a record that only links the genesis is followed through
 * `GET <instance>/explore/record/<cid>` (at most two hops). A 403 is the
 * reads table refusing a caller that is not the owner.
 */
export async function readGenesis(authFetch: AuthFetchLike, instanceUrl: string): Promise<GenesisRead> {
  const base = instanceUrl.replace(/\/+$/, "");
  const get = async (path: string) => {
    const res = await authFetch.fetch(`${base}${path}`, { method: "GET" });
    return { status: res.status, body: parseBody(await res.text()) };
  };
  const first = await get("/explore/log?before=1&limit=1");
  if (first.status !== 200) {
    const b = (first.body ?? {}) as Record<string, unknown>;
    const why =
      first.status === 403
        ? "refused (403): the explorer answers the instance's owner only, and the connected wallet is not it"
        : `HTTP ${first.status}${typeof b.description === "string" ? `: ${b.description}` : ""}`;
    return { ok: false, status: first.status, error: why };
  }
  const entries = ((first.body as { entries?: unknown[] } | null)?.entries ?? []) as Record<string, unknown>[];
  let candidate: unknown = entries.find((e) => e.n === 0) ?? entries[0];
  for (let hop = 0; hop < 3 && candidate; hop++) {
    const c = candidate as Record<string, unknown>;
    const found = [c, c.record, c.genesis].find(isGenesis);
    if (found) {
      const owner = dagBytesHex(found.owner);
      const identity = dagBytesHex(found.identity);
      return {
        ok: true,
        status: 200,
        ...(owner ? { owner } : {}),
        ...(identity ? { identity } : {}),
        policy: policyOf(found),
      };
    }
    const rec = c.record as Record<string, unknown> | undefined;
    const link = linkOf(c.genesis) ?? linkOf(rec?.genesis) ?? (rec === undefined ? linkOf(c.entry) : undefined);
    if (!link) break;
    const r = await get(`/explore/record/${link}`);
    if (r.status !== 200) return { ok: false, status: r.status, error: `explore/record/${link}: HTTP ${r.status}` };
    candidate = { record: r.body };
  }
  return { ok: false, status: 200, error: "the explorer answered, but log entry 0 is not a genesis record this page recognizes" };
}

/**
 * The installed app's policy through the explorer (the owner's read): the
 * head `<app>/app` (`GET <instance>/explore/head/<app>/app` → `{tree: <the
 * app record's CID>}`), then the record (`/explore/record/<cid>`), its
 * `config.amm`. Undefined when the head or the record cannot be read.
 */
export async function readAppPolicy(authFetch: AuthFetchLike, instanceUrl: string, app: string): Promise<ValidatorPolicy | undefined> {
  const base = instanceUrl.replace(/\/+$/, "");
  const get = async (path: string) => {
    const res = await authFetch.fetch(`${base}${path}`, { method: "GET" });
    return res.status === 200 ? parseBody(await res.text()) : undefined;
  };
  const head = (await get(`/explore/head/${app}/app`)) as { tree?: unknown } | undefined;
  const cid = linkOf(head?.tree);
  if (!cid) return undefined;
  const rec = await get(`/explore/record/${cid}`);
  return rec && typeof rec === "object" && (rec as { kind?: unknown }).kind === "app" ? policyOf(rec as Record<string, unknown>) : undefined;
}
