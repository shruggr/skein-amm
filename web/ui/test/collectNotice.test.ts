/** The Liquidity page after a collect (0.54.1): a partial collect says what completed and the wallet's real text; "Available to collect" is re-read after every attempt. */
import { describe, expect, it, vi } from "vitest";
import { collectNotice, runCollect } from "../src/ox/Liquidity";
import type { CollectResult } from "../src/data/exchange";

const TOKEN = `${"ab".repeat(32)}_0`;
const metaOf = (tokenId: string) => (tokenId === TOKEN ? { sym: "TST", dec: 0 } : undefined);
const partial: CollectResult = {
  txid: "11".repeat(32),
  txids: ["11".repeat(32)],
  pages: 2,
  refused: { page: 2, error: "thrown: undefined (no error value)" },
  remaining: { sats: 8486n, tokens: [], outputs: 7 },
  sats: 2100n,
  tokens: [{ tokenId: TOKEN, amount: 6n }],
};

describe("collectNotice", () => {
  it("a partial collect: what completed, the next page's real text, what is still available", () => {
    expect(collectNotice(partial, metaOf)).toEqual({
      kind: "bad",
      text: "Collected 2,100 sats and your token fees in 1 transaction. The next one failed: thrown: undefined (no error value). 8,486 sats is still available to collect.",
    });
  });
  it("a partial collect whose re-read failed: the rest is still available", () => {
    const { remaining: _r, ...noRemaining } = partial;
    expect(collectNotice(noRemaining, metaOf).text).toMatch(/The next one failed: .*\. The rest is still available to collect\.$/);
  });
  it("every page done; nothing to collect", () => {
    const { refused: _x, ...done } = partial;
    expect(collectNotice({ ...done, txids: ["11".repeat(32), "22".repeat(32)] }, metaOf)).toEqual({ kind: "ok", text: "Collected 2,100 sats and your token fees in 2 transactions." });
    expect(collectNotice({ txids: [], pages: 0, sats: 0n, tokens: [] }, metaOf)).toEqual({ kind: "ok", text: "No fees to collect." });
  });
});

describe("runCollect", () => {
  it("a page-1 failure that threw a non-Error: its real text, never 'Unknown error'; available re-read", async () => {
    const reload = vi.fn();
    const n = await runCollect(() => Promise.reject({ code: "ERR_INTERNAL" }), reload, metaOf);
    expect(n).toEqual({ kind: "bad", text: 'Object: {"code":"ERR_INTERNAL"}' });
    expect(reload).toHaveBeenCalledTimes(1);
    const u = await runCollect(() => Promise.reject(undefined), reload, metaOf);
    expect(u.text).toBe("thrown: undefined (no error value)");
    expect(reload).toHaveBeenCalledTimes(2);
  });
  it("a partial collect and a full one: available re-read after each", async () => {
    const reload = vi.fn();
    expect((await runCollect(() => Promise.resolve(partial), reload, metaOf)).kind).toBe("bad");
    expect(reload).toHaveBeenCalledTimes(1);
    const { refused: _x, ...done } = partial;
    expect((await runCollect(() => Promise.resolve(done), reload, metaOf)).kind).toBe("ok");
    expect(reload).toHaveBeenCalledTimes(2);
  });
});
