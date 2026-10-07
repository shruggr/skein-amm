/**
 * Token ids and outpoints, written and read in one place (skein-amm 0.6.3).
 *
 * A token id is written by the token's ORIGIN (BRC-162 "Token
 * identification"; skein-mandala src/name.zig `tokenIdText`). David,
 * 2026-10-08: "The token ids for mandala tokens are supposed to be just the
 * txid. ... The only ones which will have _vout are the legacy bsv21 tokens.
 * all mandala are just the txid."
 *  - a token that originated as a Mandala (BRC-162 binary) deploy: the bare
 *    `<txid>` (64 lowercase hex, display order);
 *  - a legacy BSV-21 (BRC-161 JSON inscription) token: `<txid>_<vout>`,
 *    `_0` included.
 * On input every form names the same token: `<txid>`, `<txid>_<vout>`,
 * `<txid>.<vout>` (no vout = vout 0).
 *
 * An outpoint is shown `<txid>.<vout>`. David, 2026-10-08: "Outpoints are
 * always to be shown in the txid.vout format, not underscores. The ONLY
 * remnant of underscores is for legacy tokenIds".
 *
 * Boundaries keep the receiver's form: 1sat-sdk's wallet filings
 * (`bsv21:<id>` tags, the customInstructions `id`) and `Mandala`'s string id
 * take `<txid>_<vout>` (`sdkTokenId`); the token's basket is `mandala <txid>
 * <vout>`; the Zig programs' outpoint fields (amm-lookup's `{outpoint}`, the
 * relay's) and the LP key derivations keep `<txid>_<vout>` as before.
 */

export interface TokenRef {
  /** 64 lowercase hex, display order. */
  txid: string;
  vout: number;
}

export type TokenOrigin = "mandala" | "bsv21";

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

/** The token id by origin: a Mandala token deployed at output 0 is the bare txid, anything else `<txid>_<vout>`. */
export function tokenIdText(ref: TokenRef, origin: TokenOrigin): string {
  const txid = ref.txid.toLowerCase();
  return origin === "mandala" && ref.vout === 0 ? txid : `${txid}_${ref.vout}`;
}

/**
 * The token id a Mandala output names by its BRC-162 wire id (internal byte
 * order): 32 bytes is the bare txid, 36 bytes (a legacy BRC-161 token at
 * vout > 0) `<txid>_<vout>`. What 1sat-sdk's `MandalaToken.tokenId` prints as
 * `<txid>_0` for a 32-byte id is the bare txid here.
 */
export function tokenIdOfWire(idBytes: Uint8Array | number[]): string {
  const b = Uint8Array.from(idBytes);
  if (b.length !== 32 && b.length !== 36) throw new Error(`a token's wire id is 32 or 36 bytes, not ${b.length}`);
  const txid = Array.from(b.subarray(0, 32))
    .reverse()
    .map((x) => x.toString(16).padStart(2, "0"))
    .join("");
  if (b.length === 32) return txid;
  return `${txid}_${new DataView(b.buffer, b.byteOffset + 32, 4).getUint32(0, true)}`;
}

function must(text: string): TokenRef {
  const r = parseTokenId(text);
  if (!r) throw new Error(`not a token id (<txid>, <txid>_<vout> or <txid>.<vout>): ${text}`);
  return r;
}

/** The form 1sat-sdk takes (wallet tags and customInstructions, `Mandala`'s string id): `<txid>_<vout>`, a bare id as `<txid>_0`. */
export function sdkTokenId(text: string): string {
  const { txid, vout } = must(text);
  return `${txid}_${vout}`;
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

/** An outpoint shortened for display (the txid to its first and last 8, as skein-mandala 0.7.4 shows ids): `abcdef01…456789ab.0`. */
export function shortOutpoint(op: string): string {
  const r = parseOutpoint(op);
  return r ? `${shortTxid(r.txid)}.${r.vout}` : op;
}

/** A token id shortened for display, in the form it is written: `abcdef01…456789ab` (Mandala) or `abcdef01…456789ab_3` (legacy). */
export function shortTokenId(id: string): string {
  const m = FORM.exec(id);
  if (!m) return id;
  return m[2] === undefined ? shortTxid(m[1]!.toLowerCase()) : `${shortTxid(m[1]!.toLowerCase())}_${m[2]}`;
}
