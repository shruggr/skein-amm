/**
 * Token ids and outpoints, written and read in one place (skein-amm 0.6.3,
 * 0.7.1).
 *
 * A token id is written `<txid>_<vout>` for every token, 1Color (BRC-162
 * binary) and legacy BSV-21 (BRC-161 JSON) alike, `_0` included: BRC-162
 * "Token identification" ("For display and APIs, the string form is
 * `<txid>_<vout>` ... fixed for BSV-21 and Mandala ... for a 32-byte id,
 * appends `_0`"). David, 2026-10-07 (shruggr/skein#120; supersedes 0.6.3's
 * bare txid). The bare 32-byte txid is the wire form only.
 * On input every form names the same token: `<txid>`, `<txid>_<vout>`,
 * `<txid>.<vout>` (no vout = vout 0).
 *
 * An outpoint is shown `<txid>.<vout>`. David, 2026-10-08: "Outpoints are
 * always to be shown in the txid.vout format, not underscores. The ONLY
 * remnant of underscores is for legacy tokenIds".
 *
 * The wallet's BRC-207 module's asset id, 1sat-sdk's `OneColor` string id and
 * the exchange's own filings take the same `<txid>_<vout>` (`sdkTokenId`); the
 * module's baskets name it by its asset tag, `p color <txid>v<vout>`
 * (src/wallet/color.ts); the Zig programs' outpoint fields
 * (amm-lookup's `{outpoint}`, the relay's) and the LP key derivations keep
 * `<txid>_<vout>` as before.
 */

export interface TokenRef {
  /** 64 lowercase hex, display order. */
  txid: string;
  vout: number;
}

const FORM = /^([0-9a-fA-F]{64})(?:[._](\d+))?$/;

function parse(text: string, needVout: boolean): TokenRef | null {
  const m = FORM.exec(text.trim());
  if (!m || (needVout && m[2] === undefined)) return null;
  const vout = m[2] === undefined ? 0 : Number(m[2]);
  if (!Number.isSafeInteger(vout) || vout > 0xffffffff) return null;
  return { txid: m[1]!.toLowerCase(), vout };
}

/** Any token id form (`<txid>`, `<txid>_<vout>`, `<txid>.<vout>`) → its deploy outpoint; null for anything else. */
export function parseTokenId(text: string): TokenRef | null {
  return parse(text, false);
}

/** An outpoint (`<txid>.<vout>` or `<txid>_<vout>`) → txid and vout; null for anything else. */
export function parseOutpoint(text: string): TokenRef | null {
  return parse(text, true);
}

/** BRC-207's two lookup queries (wallet/0207.md "Overlay spendability contract"). */
export type Brc207Type = "color-spendability" | "color-admission";

/** A BRC-24 lookup request: its service and query. */
export interface LookupRequest {
  service: string;
  query: Record<string, unknown>;
}

/**
 * A BRC-207 query for outpoints of token `tokenId` at a token instance (0.20.1): service
 * `ls_color_<assetId>`, topic `tm_color_<assetId>`, exactly BRC-207's shape. A token instance
 * serves no `ls_color`.
 */
export function colorQuery(type: Brc207Type, tokenId: string, outpoints: string[]): LookupRequest {
  const ref = parseTokenId(tokenId);
  if (!ref) throw new Error(`not a token id: ${tokenId}`);
  const assetId = tokenIdText(ref);
  return { service: `ls_color_${assetId}`, query: { type, version: 1, assetId, topic: `tm_color_${assetId}`, outpoints: outpoints.map(outpointText) } };
}

/** The discovery lookup (0.20.0): `ls_color`, color-lookup over the discovery topic `tm_color`. */
export const DISCOVERY_LOOKUP = "ls_color";

/**
 * A token's deploy asked of the discovery lookup (0.20.1; David, 2026-10-29: BRC-207's form):
 * `{service: "ls_color", query: {type: "color-admission", version: 1, assetId: "<txid>_<vout>",
 * topic: "tm_color", outpoints: ["<txid>.<vout>"]}}` — the token id IS the deploy's outpoint.
 */
export function deployQuery(tokenId: string): LookupRequest {
  const ref = parseTokenId(tokenId);
  if (!ref) throw new Error(`not a token id: ${tokenId}`);
  return { service: DISCOVERY_LOOKUP, query: { type: "color-admission", version: 1, assetId: tokenIdText(ref), topic: "tm_color", outpoints: [`${ref.txid}.${ref.vout}`] } };
}

/** The token id of a deploy outpoint: `<txid>_<vout>`, `_0` included (BRC-162 "Token identification"). */
export function tokenIdText(ref: TokenRef): string {
  return `${ref.txid.toLowerCase()}_${ref.vout}`;
}

/**
 * The token id a 1Color output names by its BRC-162 wire id (internal byte
 * order): 32 bytes is `<txid>_0`, 36 bytes (a legacy BRC-161 token at
 * vout > 0) `<txid>_<vout>`; as 1sat-sdk's `OneColor` prints it.
 */
export function tokenIdOfWire(idBytes: Uint8Array | number[]): string {
  const b = Uint8Array.from(idBytes);
  if (b.length !== 32 && b.length !== 36) throw new Error(`a token's wire id is 32 or 36 bytes, not ${b.length}`);
  const txid = Array.from(b.subarray(0, 32))
    .reverse()
    .map((x) => x.toString(16).padStart(2, "0"))
    .join("");
  const vout = b.length === 32 ? 0 : new DataView(b.buffer, b.byteOffset + 32, 4).getUint32(0, true);
  return tokenIdText({ txid, vout });
}

function must(text: string): TokenRef {
  const r = parseTokenId(text);
  if (!r) throw new Error(`not a token id (<txid>, <txid>_<vout> or <txid>.<vout>): ${text}`);
  return r;
}

/** Any token id form → the token id string, `<txid>_<vout>` (a `<txid>` as `<txid>_0`): the form shown and the BRC-207 asset id (the module's receipts, `OneColor`'s string id). */
export function sdkTokenId(text: string): string {
  return tokenIdText(must(text));
}

/** Whether two token id strings, in any form, name the same token. */
export function sameToken(a: string, b: string): boolean {
  const x = parseTokenId(a);
  const y = parseTokenId(b);
  return !!x && !!y && x.txid === y.txid && x.vout === y.vout;
}

/** An outpoint for display: `<txid>.<vout>`; anything that is not an outpoint as given. */
export function outpointText(op: string): string {
  const r = parseOutpoint(op);
  return r ? `${r.txid}.${r.vout}` : op;
}

const shortTxid = (txid: string) => `${txid.slice(0, 8)}…${txid.slice(-8)}`;

/** An outpoint shortened for display (the txid to its first and last 8, as skein-1color 0.7.4 shows ids): `abcdef01…456789ab.0`. */
export function shortOutpoint(op: string): string {
  const r = parseOutpoint(op);
  return r ? `${shortTxid(r.txid)}.${r.vout}` : op;
}

/** A token id shortened for display, in the form it is written: `abcdef01…456789ab_0`; a bare txid (not a token id) as `abcdef01…456789ab`. */
export function shortTokenId(id: string): string {
  const m = FORM.exec(id);
  if (!m) return id;
  return m[2] === undefined ? shortTxid(m[1]!.toLowerCase()) : `${shortTxid(m[1]!.toLowerCase())}_${m[2]}`;
}
