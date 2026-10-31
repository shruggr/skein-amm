/** The main nav's Settings link (0.27.0): root, or any connected wallet while the exchange is unclaimed (the claim card is on Settings). */
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";

let claimed: boolean | null = false;
let asked: boolean[] = [];
vi.mock("../src/data/exchange", async (orig) => ({
  ...(await orig<typeof import("../src/data/exchange")>()),
  useClaimStatus: (enabled: boolean) => {
    asked.push(enabled);
    return { data: !enabled || claimed === null ? null : { claimed, identity: `02${"5a".repeat(32)}` }, error: null, loading: claimed === null, reload: () => {} };
  },
}));
const { Nav } = await import("../src/App");
const route = { page: "landing" } as never;
const settingsLink = (html: string) => />Settings</.test(html);

describe("Nav: the Settings link", () => {
  it("connected, not root, claimed:false → Settings shows", () => {
    claimed = false;
    asked = [];
    expect(settingsLink(renderToStaticMarkup(<Nav route={route} connected root={false} />))).toBe(true);
    expect(asked).toEqual([true]);
  });
  it("connected, not root, claimed:true → hidden", () => {
    claimed = true;
    expect(settingsLink(renderToStaticMarkup(<Nav route={route} connected root={false} />))).toBe(false);
  });
  it("not root, the claim read pending → hidden, as before", () => {
    claimed = null;
    expect(settingsLink(renderToStaticMarkup(<Nav route={route} connected root={false} />))).toBe(false);
  });
  it("root → shows; the claim status is not read", () => {
    claimed = true;
    asked = [];
    const html = renderToStaticMarkup(<Nav route={route} connected root />);
    expect(settingsLink(html)).toBe(true);
    expect(asked).toEqual([false]);
    for (const l of ["Tokens", "Swap", "Liquidity", "Your tokens"]) expect(html).toContain(`>${l}<`);
  });
});
