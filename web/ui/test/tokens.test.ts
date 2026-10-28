/**
 * The config instance's reads (0.17.0, shruggr/skein-amm#4): the token list
 * (`GET <config>/tokens`, each enabled token with its own instance's url) and
 * the defaults (`GET <config>/defaults`); a token instance's AMM base from its
 * url (`tokenBase`).
 */
import { describe, expect, it } from "vitest";
import { readDefaults, readDiscovery, readTokens } from "../src/lib/skein";
import { tokenBase } from "../src/lib/config";
import { discoveryText } from "../src/ox/Settings";

const CONFIG = "https://ox.example/amm";
const T = `${"ab".repeat(32)}_0`;
const U = `${"cd".repeat(32)}_0`;

const answering = (body: unknown, seen: string[] = []) =>
  (async (url: string | URL | Request) => {
    seen.push(String(url));
    return new Response(JSON.stringify(body), { headers: { "content-type": "application/json" } });
  }) as typeof fetch;

describe("the config instance's reads", () => {
  it("readTokens: GET <config>/tokens — every row with its status, its url once created, the delivery's verdict; a row of another shape dropped", async () => {
    const seen: string[] = [];
    const rows = [
      { tokenId: T, topic: `tm_color_${T}`, sym: "AMM", dec: 5, icon: "data:image/png;base64,AA==", status: "enabled", url: `https://ox.example/@${T}`, since: 20, delivered: { verdict: "accept" } },
      { tokenId: U, topic: `tm_color_${U}`, sym: "", dec: 0, status: "failed", since: 10, reason: "handle taken" },
      { tokenId: "x", status: "bogus" },
      { status: "enabled" },
    ];
    const ts = await readTokens(CONFIG, answering(rows, seen));
    expect(seen).toEqual([`${CONFIG}/tokens`]);
    expect(ts).toEqual([
      { tokenId: T, topic: `tm_color_${T}`, sym: "AMM", dec: 5, icon: "data:image/png;base64,AA==", status: "enabled", url: `https://ox.example/@${T}`, since: 20, delivered: { verdict: "accept" } },
      { tokenId: U, topic: `tm_color_${U}`, sym: "", dec: 0, status: "failed", since: 10, reason: "handle taken" },
    ]);
  });

  it("readDefaults: GET <config>/defaults — terms and market when set, none otherwise", async () => {
    expect(await readDefaults(CONFIG, answering({}))).toEqual({});
    const d = { terms: { lpFeeBps: 30, validatorFeeBps: 5, commissionBps: 25 }, market: { minCommissionBps: 1 } };
    expect(await readDefaults(CONFIG, answering(d))).toEqual(d);
    expect(await readDefaults(CONFIG, answering({ terms: { lpFeeBps: 30 } }))).toEqual({});
  });

  it("readDiscovery (0.18.0): GET <config>/discovery — {on: false} until created; the record's status, url and reason; another shape off", async () => {
    const seen: string[] = [];
    expect(await readDiscovery(CONFIG, answering({ on: false }, seen))).toEqual({ on: false });
    expect(seen).toEqual([`${CONFIG}/discovery`]);
    const rec = { on: true, status: "enabled", since: 5, url: "https://ox.example/@discovery" };
    expect(await readDiscovery(CONFIG, answering(rec))).toEqual(rec);
    expect(await readDiscovery(CONFIG, answering({ on: true, status: "failed", since: 5, reason: "no image" }))).toEqual({ on: true, status: "failed", since: 5, reason: "no image" });
    expect(await readDiscovery(CONFIG, answering({ on: true, status: "bogus" }))).toEqual({ on: false });
    expect(await readDiscovery(CONFIG, answering([]))).toEqual({ on: false });
  });

  it("the Discovery card's text (0.18.0): off, settling, on, stopped and failed with their reasons", () => {
    expect(discoveryText({ on: false })).toMatch(/^Off: no discovery instance/);
    expect(discoveryText({ on: true, status: "creating" })).toMatch(/being created/);
    expect(discoveryText({ on: true, status: "starting" })).toMatch(/starting/);
    expect(discoveryText({ on: true, status: "enabled" })).toMatch(/^On: .*tm_color.*ls_color_deploys/);
    expect(discoveryText({ on: false, status: "stopping" })).toBe("Turning off…");
    expect(discoveryText({ on: false, status: "stopped", reason: "x" })).toBe("Off: its instance is stopped (x).");
    expect(discoveryText({ on: true, status: "failed", reason: "no image" })).toBe("Could not turn on: no image.");
  });

  it("tokenBase: a token instance's AMM base, <url>/amm; none before its instance is up", () => {
    expect(tokenBase({ url: `https://ox.example/@${T}` })).toBe(`https://ox.example/@${T}/amm`);
    expect(tokenBase({ url: `https://ox.example/@${T}/` })).toBe(`https://ox.example/@${T}/amm`);
    expect(tokenBase({})).toBeUndefined();
  });
});
