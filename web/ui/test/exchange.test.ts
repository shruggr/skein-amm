/**
 * src/data/exchange.ts (Open Exchange, fixtures): the shapes the pages rely
 * on, and that the fixtures agree with each other. When the overlay session
 * wires real data these shape checks should still hold.
 */
import { beforeEach, describe, expect, it } from "vitest";
import {
  closePosition,
  deployPosition,
  deregisterToken,
  discoveryTokens,
  hostedTokens,
  idKindOf,
  listingRequests,
  lookupOf,
  myPositions,
  myTokens,
  quoteSwap,
  registerToken,
  registeredTokens,
  requestListing,
  resetFixtures,
  topicOf,
  validatorTerms,
} from "../src/data/exchange";
import { compactSats, fmtAmount, fmtChange, fmtPrice, parseInput } from "../src/ox/format";
import { parseRoute, routeHref } from "../src/ox/route";

const TXID = /^[0-9a-f]{64}$/;
const OUTPOINT = /^[0-9a-f]{64}\.\d+$/;

beforeEach(() => resetFixtures());

describe("exchange.ts shapes", () => {
  it("hosted tokens: bare-txid ids, a marginal price from the summed reserves, depth below the sats", async () => {
    const ts = await hostedTokens();
    expect(ts.length).toBeGreaterThan(1);
    for (const t of ts) {
      expect(t.tokenId).toMatch(TXID);
      expect(idKindOf(t.tokenId)).toBe("txid");
      expect(t.pools).toBeGreaterThan(0);
      expect(t.marginalPrice).toBeCloseTo(Number(t.reserves.sats) / (Number(t.reserves.tokens) / 10 ** t.dec), 6);
      expect(t.depth!.sats).toBeGreaterThan(0n);
      expect(t.depth!.sats).toBeLessThan(t.reserves.sats);
    }
    expect(idKindOf(`${"ab".repeat(32)}_1`)).toBe("token");
  });

  it("a buy quote routes over the pools: legs sum to the amount, effective price above marginal, impact in bps", async () => {
    const [gold] = await hostedTokens();
    const q = await quoteSwap({ tokenId: gold!.tokenId, side: "buy", amountIn: 250_000n, maxSlippageBps: 50, allowPartial: true });
    expect(q.legs.length).toBeGreaterThan(1);
    expect(q.legs.reduce((a, l) => a + l.amountIn, 0n)).toBe(250_000n);
    expect(q.legs.reduce((a, l) => a + l.amountOut, 0n)).toBe(q.amountOut);
    expect(q.legs.reduce((a, l) => a + l.shareBps, 0)).toBeGreaterThanOrEqual(9_990);
    for (const l of q.legs) expect(l.poolOutpoint).toMatch(OUTPOINT);
    expect(q.effectivePrice).toBeGreaterThan(q.marginalPrice);
    expect(q.impactBps).toBe(Math.round(((q.effectivePrice - q.marginalPrice) / q.marginalPrice) * 10_000));
    expect(q.minAmountOut).toBe((q.amountOut * 9_950n) / 10_000n);
  });

  it("a sell quote pays sats below marginal", async () => {
    const [gold] = await hostedTokens();
    const q = await quoteSwap({ tokenId: gold!.tokenId, side: "sell", amountIn: 10_000n, maxSlippageBps: 50, allowPartial: false });
    expect(q.effectivePrice).toBeLessThan(q.marginalPrice);
  });

  it("positions: outpoints; close returns the pool less bsvFee; deploy adds one", async () => {
    const ps = await myPositions();
    expect(ps.length).toBeGreaterThan(0);
    const p = ps[0]!;
    expect(p.outpoint).toMatch(OUTPOINT);
    const r = await closePosition({ outpoint: p.outpoint, bsvFee: 200n });
    expect(r.sats).toBe(p.sats - 200n);
    expect(r.tokens).toBe(p.tokens);
    expect((await myPositions()).some((x) => x.outpoint === p.outpoint)).toBe(false);
    await expect(closePosition({ outpoint: p.outpoint, bsvFee: 0n })).rejects.toThrow();

    const tea = (await myTokens()).find((t) => t.sym === "TEA")!;
    const d = await deployPosition({ tokenId: tea.tokenId, tokens: 100n, sats: 8_800n });
    expect(d.outpoint).toMatch(OUTPOINT);
    expect((await myPositions()).some((x) => x.outpoint === d.outpoint)).toBe(true);
    const terms = await validatorTerms();
    expect(terms.fees).toEqual({ lpBps: 30, validatorBps: 5 });
  });

  it("your tokens, listing requests, register and deregister move a token between discovery and registered", async () => {
    const moss = (await myTokens()).find((t) => t.sym === "MOSS")!;
    expect(moss.onExchange).toBe(false);
    const fern = (await myTokens()).find((t) => t.sym === "FERN")!;
    await requestListing(fern.tokenId);
    expect((await listingRequests())!.map((r) => r.sym)).toContain("FERN");
    expect((await discoveryTokens()).map((t) => t.tokenId)).toContain(fern.tokenId);
    await registerToken(fern.tokenId);
    expect((await registeredTokens()).map((t) => t.tokenId)).toContain(fern.tokenId);
    expect((await discoveryTokens()).map((t) => t.tokenId)).not.toContain(fern.tokenId);
    expect((await listingRequests())!.map((r) => r.sym)).not.toContain("FERN");
    await deregisterToken(fern.tokenId);
    expect((await registeredTokens()).map((t) => t.tokenId)).not.toContain(fern.tokenId);
  });

  it("topic and lookup names are derived from the token id (BRC-207: `_0` for a bare txid)", () => {
    const t = "cd".repeat(32);
    expect(topicOf(t)).toBe(`tm_mandala_${t}_0`);
    expect(lookupOf(t)).toBe(`ls_mandala_${t}_0`);
    expect(topicOf(`${t}_2`)).toBe(`tm_mandala_${t}_2`);
  });
});

describe("formats and routes", () => {
  it("formats amounts, sats, prices and changes", () => {
    expect(fmtAmount(120_450n, 2)).toBe("1,204.5");
    expect(fmtAmount(5_000n, 0)).toBe("5,000");
    expect(compactSats(1_240_000n)).toBe("1.24M");
    expect(compactSats(480_000n)).toBe("480k");
    expect(compactSats(80_000n)).toBe("80,000");
    expect(fmtPrice(1250)).toBe("1,250");
    expect(fmtPrice(411.96)).toBe("412.0");
    expect(fmtChange(0.018)).toBe("+1.8%");
    expect(fmtChange(-0.004)).toBe("−0.4%");
    expect(fmtChange(0)).toBe("0.0%");
    expect(parseInput("250,000", 0)).toBe(250_000n);
    expect(parseInput("1.5", 0)).toBeNull();
  });

  it("hash routes round-trip", () => {
    const id = "ef".repeat(32);
    expect(parseRoute(routeHref({ page: "swap", tokenId: id }))).toEqual({ page: "swap", tokenId: id });
    expect(parseRoute("#/liquidity")).toEqual({ page: "liquidity" });
    expect(parseRoute("")).toEqual({ page: "landing" });
    expect(parseRoute("#/nope")).toEqual({ page: "landing" });
  });
});
