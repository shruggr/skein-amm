/**
 * The Validator page: the heartbeat start/stop request through a fake
 * AuthFetch, the explorer's genesis read, the this-instance view from mocked
 * manifest / resolve / live answers, and pools-served filtering.
 */
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { parseLookupAnswer, parseTokenTopic } from "../src/lib/overlay";
import type { FetchLike } from "../src/lp/validators";
import { ago } from "../src/lp/validators";
import { hostLabel, instanceAddress, livenessLine, loadThisInstance, poolsServedBy } from "../src/validator/instance";
import {
  dagBytesHex,
  heartbeatRequest,
  policyOf,
  readGenesis,
  sendHeartbeatControl,
  type AuthFetchLike,
} from "../src/validator/control";

const fixture = (name: string) => JSON.parse(readFileSync(new URL(`./fixtures/instance-v2/${name}`, import.meta.url), "utf8")) as unknown;

const AMM2 = "02f2607898feca297bec05cc510475ff896fb3e9a1d3c0528f6ce2c4382eff472a";
const AMM3 = "03cdee31ef0446ffb95aeae00353d9ab4c26a8555d597a9930b0ddd4f4cc1ae0d0";
const AMM2_PEER = "16Uiu2HAmL3ee25zUdPHFTUVToTuzBgBjt8yoxAd952pYyvfbmDT9";

/** A fake AuthFetch: records calls, answers from a function. */
function fakeAuthFetch(answer: (url: string, config?: { method?: string; body?: string }) => { status: number; body: unknown }) {
  const calls: { url: string; config?: { method?: string; headers?: Record<string, string>; body?: string } }[] = [];
  const af: AuthFetchLike = {
    async fetch(url, config) {
      calls.push({ url, ...(config ? { config } : {}) });
      const a = answer(url, config);
      return { status: a.status, text: async () => (typeof a.body === "string" ? a.body : JSON.stringify(a.body)) };
    },
  };
  return { af, calls };
}

describe("heartbeat start / stop (BRC-33 sendMessage)", () => {
  it("builds the message into the instance's own box amm-p2p", () => {
    expect(heartbeatRequest("start", AMM2.toUpperCase())).toEqual({
      message: { recipient: AMM2, messageBox: "amm-p2p", body: { kind: "amm-p2p-start" } },
    });
    expect(heartbeatRequest("stop", AMM2)).toEqual({
      message: { recipient: AMM2, messageBox: "amm-p2p", body: { kind: "amm-p2p-stop", jobs: ["heartbeat"] } },
    });
  });

  it("POSTs it to <origin>/sendMessage through AuthFetch, JSON, and reads an admission", async () => {
    const { af, calls } = fakeAuthFetch(() => ({
      status: 200,
      body: { status: "success", message: "Your message has been sent to 1 recipient(s).", results: [{ recipient: AMM2, messageId: "bafy1" }], id: "bafy1" },
    }));
    const r = await sendHeartbeatControl(af, "http://amm2.localhost:8300/", AMM2, "start");
    expect(calls).toHaveLength(1);
    expect(calls[0]!.url).toBe("http://amm2.localhost:8300/sendMessage");
    expect(calls[0]!.config?.method).toBe("POST");
    expect(calls[0]!.config?.headers).toEqual({ "content-type": "application/json" });
    expect(JSON.parse(calls[0]!.config!.body!)).toEqual({
      message: { recipient: AMM2, messageBox: "amm-p2p", body: { kind: "amm-p2p-start" } },
    });
    expect(r.admitted).toBe(true);
    expect(r.id).toBe("bafy1");
    expect(r.refusal).toBeUndefined();
  });

  it("reports the subscription table's refusal", async () => {
    const { af } = fakeAuthFetch(() => ({
      status: 403,
      body: { status: "error", code: "ERR_NOT_SUBSCRIBED", description: "This instance takes no messages from you in that box." },
    }));
    const r = await sendHeartbeatControl(af, "http://amm2.localhost:8300", AMM2, "stop");
    expect(r.admitted).toBe(false);
    expect(r.status).toBe(403);
    expect(r.refusal).toBe("ERR_NOT_SUBSCRIBED: This instance takes no messages from you in that box.");
    expect(r.request).toEqual(heartbeatRequest("stop", AMM2));
  });

  it("a non-JSON answer is a refusal with its text", async () => {
    const { af } = fakeAuthFetch(() => ({ status: 401, body: "unauthorized" }));
    const r = await sendHeartbeatControl(af, "http://amm2.localhost:8300", AMM2, "start");
    expect(r.admitted).toBe(false);
    expect(r.refusal).toBe("HTTP 401: unauthorized");
  });
});

describe("the explorer (owner only): genesis owner and policy", () => {
  const ownerHex = "027c21b23e13472d370821454a34874d934ee5721bb3327ceb489c38d1a4b0f21b";
  const b64 = (hex: string) => Buffer.from(hex, "hex").toString("base64").replace(/=+$/, "");
  const genesis = {
    kind: "genesis",
    owner: { "/": { bytes: b64(ownerHex) } },
    identity: { "/": { bytes: b64(AMM2) } },
    defaults: {
      ammP2p: JSON.stringify({ topics: ["tm_x"], peerId: AMM2_PEER, heartbeatSeconds: 30, offlineSeconds: 90 }),
      ammValidator: '{"minValidatorFeeBps":5,"maxLpFeeBps":100}',
    },
  };

  it("decodes DAG-JSON bytes", () => {
    expect(dagBytesHex({ "/": { bytes: b64(ownerHex) } })).toBe(ownerHex);
    expect(dagBytesHex(ownerHex.toUpperCase())).toBe(ownerHex);
    expect(dagBytesHex(42)).toBeUndefined();
  });

  it("reads the policy from the genesis defaults (JSON strings)", () => {
    expect(policyOf(genesis)).toEqual({ minValidatorFeeBps: 5, maxLpFeeBps: 100, heartbeatSeconds: 30, offlineSeconds: 90, peerId: AMM2_PEER });
    expect(policyOf({ kind: "genesis" })).toEqual({});
  });

  it("log entry 0 with its record", async () => {
    const { af, calls } = fakeAuthFetch(() => ({ status: 200, body: { entries: [{ n: 0, entry: { "/": "bafyg" }, record: genesis }] } }));
    const g = await readGenesis(af, "http://amm2.localhost:8300");
    expect(calls.map((c) => [c.url, c.config?.method])).toEqual([["http://amm2.localhost:8300/explore/log?before=1&limit=1", "GET"]]);
    expect(g).toMatchObject({ ok: true, owner: ownerHex, identity: AMM2, policy: { minValidatorFeeBps: 5, maxLpFeeBps: 100, heartbeatSeconds: 30 } });
  });

  it("follows a record that links the genesis", async () => {
    const { af, calls } = fakeAuthFetch((url) =>
      url.endsWith("/explore/record/bafyg")
        ? { status: 200, body: genesis }
        : { status: 200, body: { entries: [{ n: 0, entry: { "/": "bafye" }, record: { kind: "log", genesis: { "/": "bafyg" } } }] } },
    );
    const g = await readGenesis(af, "http://amm2.localhost:8300");
    expect(calls.map((c) => c.url)).toEqual([
      "http://amm2.localhost:8300/explore/log?before=1&limit=1",
      "http://amm2.localhost:8300/explore/record/bafyg",
    ]);
    expect(g.ok).toBe(true);
    expect(g.owner).toBe(ownerHex);
  });

  it("a non-owner's 403", async () => {
    const { af } = fakeAuthFetch(() => ({ status: 403, body: { status: "error" } }));
    const g = await readGenesis(af, "http://amm2.localhost:8300");
    expect(g.ok).toBe(false);
    expect(g.status).toBe(403);
    expect(g.error).toMatch(/owner only/);
  });
});

describe("this instance", () => {
  it("takes the handle from the origin", () => {
    expect(instanceAddress("http://amm2.localhost:8300/amm")).toEqual({ origin: "http://amm2.localhost:8300", name: "amm2", domain: "localhost:8300" });
    expect(instanceAddress("https://val.example.com/amm")).toEqual({ origin: "https://val.example.com", name: "val", domain: "example.com" });
    expect(instanceAddress("http://localhost:8300/@amm2/amm")).toEqual({ origin: "http://localhost:8300/@amm2", name: "amm2", domain: "localhost:8300" });
    expect(instanceAddress("http://127.0.0.1:8300/amm")).toEqual({ origin: "http://127.0.0.1:8300" });
    expect(hostLabel("http://amm3.localhost:8400/amm")).toBe("amm3.localhost:8400");
  });

  /** The router (manifest, resolve) and both instances' /amm/live, as recorded 2026-10-01. */
  function fakeFetch(amm3Live: unknown): { fetchFn: FetchLike; urls: string[] } {
    const urls: string[] = [];
    const ok = (body: unknown) => ({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });
    const fetchFn: FetchLike = async (url) => {
      urls.push(url);
      if (url === "http://localhost:8300/manifest.json") return ok({ metanet: { handles: { resolve: "http://127.0.0.1:8300/.well-known/metanet-handles/resolve" } } });
      if (url === "http://127.0.0.1:8300/.well-known/metanet-handles/resolve?handle=amm2%40localhost")
        return ok({ handle: "amm2", domain: "localhost", identityKey: AMM2, messagebox: "http://amm2.localhost:8300" });
      if (url === "http://amm3.localhost:8400/amm/live") return ok(amm3Live);
      return { ok: false, status: 404, json: async () => ({}), text: async () => "not found" };
    };
    return { fetchFn, urls };
  }

  it("identity from resolve, peer ID and liveness from the peer's /amm/live", async () => {
    const amm3Live = {
      now: 1790846296911,
      thresholdMs: 90000,
      validators: [{ identityKey: AMM2, peerId: AMM2_PEER, at: 1790846271647, ageMs: 25264, live: true }],
    };
    const { fetchFn, urls } = fakeFetch(amm3Live);
    const t = await loadThisInstance("http://amm2.localhost:8300/amm", "http://amm3.localhost:8400/amm", fetchFn);
    expect(urls).toEqual([
      "http://localhost:8300/manifest.json",
      "http://127.0.0.1:8300/.well-known/metanet-handles/resolve?handle=amm2%40localhost",
      "http://amm3.localhost:8400/amm/live",
    ]);
    expect(t.address.origin).toBe("http://amm2.localhost:8300");
    expect(t.handle).toBe("amm2@localhost:8300");
    expect(t.identityKey).toBe(AMM2);
    expect(t.peerId).toBe(AMM2_PEER);
    expect(t.peer?.entry?.live).toBe(true);
    expect(livenessLine(t, ago)).toBe("last heartbeat seen by amm3.localhost:8400: 25 s ago (live)");
  });

  it("unknown until a peer sees us (our own /amm/live never lists us)", async () => {
    // amm2's own live list (the recorded fixture) names only amm3: a node never hears itself.
    const { fetchFn } = fakeFetch(fixture("live.json"));
    const t = await loadThisInstance("http://amm2.localhost:8300/amm", "http://amm3.localhost:8400/amm", fetchFn);
    expect(t.identityKey).toBe(AMM2);
    expect(t.peerId).toBeUndefined();
    expect(livenessLine(t, ago)).toBe("amm3.localhost:8400 has not seen a heartbeat from this identity");
  });

  it("a resolve failure and an unreachable peer are reported, not thrown", async () => {
    const { fetchFn } = fakeFetch({});
    const t = await loadThisInstance("http://amm9.localhost:8300/amm", "http://amm4.localhost:8500/amm", fetchFn);
    expect(t.identityKey).toBeUndefined();
    expect(t.resolveError).toMatch(/404/);
    expect(t.peer?.error).toMatch(/404/);
    expect(livenessLine(t, ago)).toMatch(/^could not read amm4.localhost:8500\/live/);
    const noPeer = await loadThisInstance("http://amm2.localhost:8300/amm", "", fetchFn);
    expect(noPeer.peer).toBeUndefined();
    expect(livenessLine(noPeer, ago)).toMatch(/no peer configured/);
  });
});

describe("pools served", () => {
  const topics = Object.keys(fixture("listTopicManagers.json") as object).map((n) => parseTokenTopic(n)!);
  const pools = parseLookupAnswer(fixture("lookup-all.json"));

  it("on v2 today: none for amm2 (the seeded pool names the fixture validator)", () => {
    const m = new Map([[topics[0]!.tokenId, pools]]);
    expect(poolsServedBy(AMM2, topics, m)).toEqual([]);
    const fixtureValidator = pools[0]!.validatorIdentityKey;
    expect(poolsServedBy(fixtureValidator.toUpperCase(), topics, m).map((s) => s.pool.outpoint)).toEqual([pools[0]!.outpoint]);
  });

  it("filters by validator identity across topics, skipping failed lookups", () => {
    const t2 = parseTokenTopic(`tm_${"ab".repeat(32)}`)!;
    const t3 = parseTokenTopic(`tm_${"cd".repeat(32)}_1`)!;
    const mine = { ...pools[0]!, outpoint: `${"11".repeat(32)}_0`, validatorIdentityKey: AMM2 };
    const theirs = { ...pools[0]!, outpoint: `${"22".repeat(32)}_0`, validatorIdentityKey: AMM3 };
    const m = new Map<string, (typeof pools)[number][] | Error>([
      [topics[0]!.tokenId, pools],
      [t2.tokenId, [mine, theirs]],
      [t3.tokenId, new Error("lookup failed")],
    ]);
    const served = poolsServedBy(AMM2, [topics[0]!, t2, t3], m);
    expect(served).toEqual([{ topic: t2, pool: mine }]);
  });
});
