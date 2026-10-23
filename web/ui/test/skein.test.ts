/**
 * The token list read (`GET <base>/color/tokens`): skein-1color's route takes
 * `limit` 1..100 and `skip`, so the page reads it 100 at a time until a short page.
 */
import { describe, expect, it } from "vitest";
import { readTokenList } from "../src/lib/skein";

const BASE = "https://swap.example/amm";

function token(i: number) {
  return { tokenId: `${String(i).padStart(64, "0")}_0`, topic: `tm_color_${String(i).padStart(64, "0")}_0`, sym: `T${i}`, dec: 5 };
}

function instance(total: number) {
  const calls: URL[] = [];
  const fetchFn = (async (url: string | URL | Request) => {
    const u = new URL(String(url));
    calls.push(u);
    const limit = Number(u.searchParams.get("limit"));
    const skip = Number(u.searchParams.get("skip") ?? 0);
    if (!(limit >= 1 && limit <= 100)) return new Response(JSON.stringify({ status: "error", message: "the query is {limit?: 1..100, skip?: 0..100000}" }), { headers: { "content-type": "application/json" } });
    const rows = Array.from({ length: total }, (_, i) => token(i)).slice(skip, skip + limit);
    return new Response(JSON.stringify(rows), { headers: { "content-type": "application/json" } });
  }) as typeof fetch;
  return { calls, fetchFn };
}

describe("readTokenList", () => {
  it("reads one short page with a limit the route accepts", async () => {
    const inst = instance(2);
    const list = await readTokenList(BASE, inst.fetchFn);
    expect(list.map((t) => t.sym)).toEqual(["T0", "T1"]);
    expect(list[0]).toMatchObject({ dec: 5, topic: token(0).topic });
    expect(inst.calls.map((u) => u.pathname + u.search)).toEqual(["/amm/color/tokens?limit=100&skip=0"]);
  });

  it("pages past 100 tokens until a short page", async () => {
    const inst = instance(230);
    const list = await readTokenList(BASE, inst.fetchFn);
    expect(list).toHaveLength(230);
    expect(list.at(-1)?.sym).toBe("T229");
    expect(inst.calls.map((u) => u.searchParams.get("skip"))).toEqual(["0", "100", "200"]);
  });

  it("stops on a full last page only after the next empty one", async () => {
    const inst = instance(200);
    const list = await readTokenList(BASE, inst.fetchFn);
    expect(list).toHaveLength(200);
    expect(inst.calls).toHaveLength(3);
  });
});
