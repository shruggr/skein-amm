import { describe, expect, it } from "vitest";
import {
  outpointText,
  parseOutpoint,
  parseTokenId,
  sameToken,
  sdkTokenId,
  shortOutpoint,
  shortTokenId,
  tokenIdOfWire,
  tokenIdText,
} from "../src/lib/tokenId";
import { parseTokenTopic, withListedIds } from "../src/lib/overlay";
import { mandalaBasket } from "../src/lp/wallet";
import { assetIdHex } from "../src/lp/poolDeploy";
import { assetIdOf } from "../src/market/swapAction";
import { fullId, shortId } from "../src/components/Id";

const TX = "ab".repeat(16) + "cd".repeat(16);
const UP = TX.toUpperCase();

describe("token ids by origin (David 2026-10-08)", () => {
  it("reads every form as the same token: <txid>, <txid>_<vout>, <txid>.<vout> (no vout = 0)", () => {
    expect(parseTokenId(TX)).toEqual({ txid: TX, vout: 0 });
    expect(parseTokenId(`${TX}_0`)).toEqual({ txid: TX, vout: 0 });
    expect(parseTokenId(`${UP}.0`)).toEqual({ txid: TX, vout: 0 });
    expect(parseTokenId(`${TX}_3`)).toEqual({ txid: TX, vout: 3 });
    expect(parseTokenId(`${TX}.3`)).toEqual({ txid: TX, vout: 3 });
    expect(parseTokenId("tm_" + TX)).toBeNull();
    expect(parseTokenId(TX.slice(2))).toBeNull();
    expect(sameToken(TX, `${TX}_0`)).toBe(true);
    expect(sameToken(`${TX}.0`, `${TX}_0`)).toBe(true);
    expect(sameToken(TX, `${TX}_1`)).toBe(false);
  });

  it("writes a Mandala token as the bare txid, a legacy BSV-21 token as <txid>_<vout> (_0 included)", () => {
    expect(tokenIdText({ txid: TX, vout: 0 }, "mandala")).toBe(TX);
    expect(tokenIdText({ txid: TX, vout: 2 }, "mandala")).toBe(`${TX}_2`);
    expect(tokenIdText({ txid: TX, vout: 0 }, "bsv21")).toBe(`${TX}_0`);
    expect(tokenIdText({ txid: TX, vout: 5 }, "bsv21")).toBe(`${TX}_5`);
  });

  it("names a Mandala output's token by its wire id: 32 bytes the bare txid, 36 bytes <txid>_<vout>", () => {
    const internal = Array.from({ length: 32 }, (_, i) => parseInt(TX.slice(62 - 2 * i, 64 - 2 * i), 16));
    expect(tokenIdOfWire(internal)).toBe(TX);
    expect(tokenIdOfWire([...internal, 7, 0, 0, 0])).toBe(`${TX}_7`);
    expect(() => tokenIdOfWire(internal.slice(1))).toThrow(/32 or 36/);
  });

  it("hands 1sat-sdk <txid>_<vout> (a bare id as <txid>_0) and names the token's basket `mandala <txid> <vout>`", () => {
    expect(sdkTokenId(TX)).toBe(`${TX}_0`);
    expect(sdkTokenId(`${TX}.3`)).toBe(`${TX}_3`);
    expect(() => sdkTokenId("nope")).toThrow(/not a token id/);
    expect(mandalaBasket(TX)).toBe(`mandala ${TX} 0`);
    expect(mandalaBasket(`${TX}_0`)).toBe(`mandala ${TX} 0`);
    expect(mandalaBasket(`${UP}.2`)).toBe(`mandala ${TX} 2`);
  });

  it("pools take a 32-byte id in any form, the bare txid first", () => {
    const wire = assetIdHex(TX);
    expect(assetIdHex(`${TX}_0`)).toBe(wire);
    expect(assetIdOf(TX)).toBe(wire);
    expect(assetIdOf(`${TX}.0`)).toBe(wire);
    expect(() => assetIdHex(`${TX}_1`)).toThrow(/32-byte id/);
    expect(() => assetIdOf(`${TX}_1`)).toThrow(/32-byte id/);
  });

  it("a topic name gives tm_<txid> → <txid>, tm_<txid>_<vout> → <txid>_<vout>; the Mandala token list overrides by origin", () => {
    const native = parseTokenTopic(`tm_${TX}`)!;
    const legacy = parseTokenTopic(`tm_${TX}_4`)!;
    expect(native).toMatchObject({ kind: "native", tokenId: TX });
    expect(legacy).toMatchObject({ kind: "legacy", tokenId: `${TX}_4` });
    // a legacy BSV-21 token deployed at output 0: the list writes it `<txid>_0`
    const listed = [{ tokenId: `${TX}_0`, topic: `tm_${TX}`, sym: "OLD", dec: 0, txid: TX, vout: 0 }];
    expect(withListedIds([native, legacy], listed).map((t) => t.tokenId)).toEqual([`${TX}_0`, `${TX}_4`]);
    expect(withListedIds([native], undefined)[0]!.tokenId).toBe(TX);
    // an entry that names a different token for the topic is not taken
    expect(withListedIds([native], [{ tokenId: `${TX}_1`, topic: `tm_${TX}` }])[0]!.tokenId).toBe(TX);
  });
});

describe("outpoints are shown <txid>.<vout> (David 2026-10-08)", () => {
  it("prints the dot form from either form; anything else as given", () => {
    expect(parseOutpoint(`${TX}_1`)).toEqual({ txid: TX, vout: 1 });
    expect(parseOutpoint(TX)).toBeNull();
    expect(outpointText(`${TX}_1`)).toBe(`${TX}.1`);
    expect(outpointText(`${UP}.0`)).toBe(`${TX}.0`);
    expect(outpointText("ordfs/some-path")).toBe("ordfs/some-path");
    expect(shortOutpoint(`${TX}_0`)).toBe(`${TX.slice(0, 8)}…${TX.slice(-8)}.0`);
  });

  it("shortens a token id in the form it is written", () => {
    expect(shortTokenId(TX)).toBe(`${TX.slice(0, 8)}…${TX.slice(-8)}`);
    expect(shortTokenId(`${TX}_3`)).toBe(`${TX.slice(0, 8)}…${TX.slice(-8)}_3`);
  });

  it("the <Id> shows and copies outpoints dotted, token ids and txids as written", () => {
    expect(fullId(`${TX}_2`, "outpoint")).toBe(`${TX}.2`);
    expect(shortId(`${TX}_2`, "outpoint")).toBe(`${TX.slice(0, 8)}…${TX.slice(-8)}.2`);
    expect(fullId(`${TX}_2`, "token")).toBe(`${TX}_2`);
    expect(fullId(TX, "txid")).toBe(TX);
    expect(shortId(TX, "txid")).toBe(`${TX.slice(0, 8)}…${TX.slice(-8)}`);
    expect(shortId(`tm_${TX}_1`, "topic")).toBe(`tm_${TX.slice(0, 8)}…${TX.slice(-8)}_1`);
    expect(fullId(`tm_${TX}`, "topic")).toBe(`tm_${TX}`);
  });
});
