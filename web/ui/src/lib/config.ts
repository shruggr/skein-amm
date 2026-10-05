/**
 * The skein instance this page talks to, and nothing else: every request the
 * page makes goes to `AMM_OVERLAY` (plain `fetch`, src/lib/overlay.ts), or to
 * the connected BRC-100 wallet. There are no DEV keys or DEV services.
 *
 *   AMM_OVERLAY        the AMM app's base URL: `https://<handle>.<host>/<app>`
 *                      or a host's dev form `<host>/@<handle>/<app>` (GET
 *                      listTopicManagers, GET listLookupServiceProviders,
 *                      POST lookup, POST submit, GET live, POST call under
 *                      it). The pages are the app's `www/`, served at
 *                      `<base>/` (index.html), so the base is the page's own
 *                      directory (`appBaseOf`); `VITE_AMM_OVERLAY` overrides
 *                      it (a dev server on another origin)
 *   VITE_AMM_PEER_OVERLAY  another instance's AMM base URL (no default): the
 *                      Validator page reads this instance's liveness (and
 *                      peer ID) from the peer's GET live (a node never hears
 *                      itself)
 *   VITE_AMM_OWNER_IDENTITY  optional: the instance owner's identity key
 *                      (public), when known from the deploy. No route
 *                      exposes the owner to a non-owner; the Validator page
 *                      only uses it to say whether the connected wallet is
 *                      the owner before trying
 *   VITE_FEE_RATE      the miner fee rate, sats per 1000 bytes, the Swap page
 *                      applies to the swap transaction it builds (default
 *                      100). The swap's fee is paid from the exact funding
 *                      output, so the rate is fixed here, not by the wallet
 *
 * From a dev server (serve.sh) the instance is called cross-origin: it sends
 * permissive CORS headers.
 */
const env = (import.meta as { env?: Record<string, string | undefined> }).env;

/**
 * The app's base URL from the page's own URL: the directory the page is
 * served from (`https://alice.skein.nexus/amm/` → `https://alice.skein.nexus/amm`,
 * `http://127.0.0.1:8100/@alice/amm/index.html` → `http://127.0.0.1:8100/@alice/amm`).
 * Undefined for a page at an origin's root (a dev server), which names no app.
 */
export function appBaseOf(href: string): string | undefined {
  const u = new URL(href);
  const path = u.pathname.replace(/[^/]*$/, "").replace(/\/+$/, "");
  return path ? u.origin + path : undefined;
}

/** The app's name: the last segment of its base URL (`…/amm` → `amm`); undefined for an origin. */
export function appName(base: string): string | undefined {
  try {
    const segs = new URL(base).pathname.split("/").filter((x) => x !== "");
    const last = segs[segs.length - 1];
    return last && !last.startsWith("@") ? decodeURIComponent(last) : undefined;
  } catch {
    return undefined;
  }
}

function pageHref(): string | undefined {
  try {
    return typeof location === "undefined" ? undefined : location.href;
  } catch {
    return undefined;
  }
}

/** No trailing slash. `VITE_AMM_OVERLAY`, else the page's own directory, else a local host's dev form. */
export const AMM_OVERLAY = (
  env?.VITE_AMM_OVERLAY ?? (pageHref() ? appBaseOf(pageHref()!) : undefined) ?? "http://127.0.0.1:8100/@amm/amm"
).replace(/\/+$/, "");

/** How often the Swap page re-reads the instance (topics, pools, live validators). `VITE_AMM_REFRESH_MS`. */
export const REFRESH_MS = Number(env?.VITE_AMM_REFRESH_MS ?? 10_000);

/** The peer instance whose `GET live` tells us whether our heartbeat is heard. `VITE_AMM_PEER_OVERLAY`; unset or "" disables. */
export const AMM_PEER_OVERLAY = (env?.VITE_AMM_PEER_OVERLAY ?? "").replace(/\/+$/, "");

/** The owner's identity key if configured (public key, compressed hex), else "". `VITE_AMM_OWNER_IDENTITY`. */
export const AMM_OWNER_IDENTITY = (env?.VITE_AMM_OWNER_IDENTITY ?? "").trim().toLowerCase();

/**
 * Miner fee rate for the swap transaction the page builds, sats per 1000
 * bytes (`VITE_FEE_RATE`, default 100). The funding output is sized to the
 * swap's inputs and outputs plus this fee over the swap's size with the pool
 * input at `PoolTemplate.maxCallUnlockLength`; the contract allows no change.
 */
export const FEE_RATE_SATS_PER_KB = Number(env?.VITE_FEE_RATE ?? 100);
