/**
 * BRC-162 (1Color) prefix for the pool template, over 1sat-sdk's `OneColor`
 * (`@1sat/templates` 0.1.0, the template formerly `Mandala`). One decoder:
 * this file only adapts `OneColor.decode` / `OneColor.value(...).prefix()` to
 * the byte-array shape `template.ts` works in.
 *
 *   <push id | OP_0> <push amount | OP_0> OP_2DROP [<push payload> OP_DROP] <lock>
 */
import { LockingScript, Script } from "@bsv/sdk";
import { OneColor, ONE_COLOR_MAX_AMOUNT, type OneColorRole } from "@1sat/templates";

export { ONE_COLOR_MAX_AMOUNT, type OneColorRole };

export interface OneColorPrefix {
  role: OneColorRole;
  /** Wire id bytes (32, or 36 for a legacy BRC-161 id), absent on a deploy. */
  idBytes?: Uint8Array;
  amount: bigint;
  payload?: Uint8Array;
  /** Byte length of the prefix (and payload), i.e. where the inner lock starts. */
  length: number;
}

/** The 1Color prefix at the start of `script`, or null when it has none (`OneColor.decode`). */
export function decodeOneColor(script: Uint8Array | number[]): OneColorPrefix | null {
  const bytes = Array.from(script);
  const t = OneColor.decode(Script.fromBinary(bytes));
  if (!t) return null;
  return {
    role: t.role,
    idBytes: t.idBytes,
    amount: t.amount,
    payload: t.payload,
    length: bytes.length - t.lock.toBinary().length,
  };
}

/**
 * The value-output prefix `<push id> <push amount> OP_2DROP` with no payload,
 * as pool/Pool.runar.go's `tokenPrefix` writes it (`OneColor.value(...).prefix()`).
 * The pool's asset id is always the 32-byte form.
 */
export function oneColorValuePrefix(idBytes: Uint8Array | number[], amount: bigint): number[] {
  const id = Uint8Array.from(idBytes);
  if (id.length !== 32) throw new Error(`oneColorValuePrefix: the id must be 32 bytes, got ${id.length}`);
  if (amount <= 0n || amount > ONE_COLOR_MAX_AMOUNT) {
    throw new Error("oneColorValuePrefix: amount must be between 1 and 2^64-1");
  }
  return OneColor.value(id, amount, { lock: new LockingScript() }).prefix().toBinary();
}
