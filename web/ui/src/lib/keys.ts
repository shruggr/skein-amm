import { KeyDeriver, PublicKey, type WalletProtocol } from "@bsv/sdk";

/**
 * Validator key derivation (docs/notes.md, "Validator derivation" and
 * "Overlay and communication layer on skein" / "Identity"):
 *
 * BRC-43 security level 1, protocol "amm pool" (BRC-43 needs >= 5
 * characters, so not "amm"), key ID `<txid display hex>_<vout>`, anyone
 * counterparty: invoice `1-amm pool-<txid>_<vout>`.
 *
 * The pool's rotating `ValidatorPubKey` is always a child of the
 * validator's own stable `ValidatorIdentity`. The first (a deploy's,
 * skein-amm 0.13.0) is keyed by the validator's nonce, 64 lowercase hex
 * digits (`deriveFirstValidatorPubKey`): the validator picks it at
 * `amm.pool.affirm` and the pool's state carries it. Each Swap's next key is
 * keyed by the pool outpoint the Swap spends (`deriveValidatorPubKey`).
 * (Before 0.13.0 the first key was keyed by the deploy's first token input.)
 *
 * Because the counterparty is "anyone", this is a public one-way function
 * of the validator's identity public key and the key ID: anyone holding
 * `validatorIdentity` (published, part of every pool's state) can compute
 * the derived child public key without the validator's private key: the
 * page checks the validator's answer with it, and the lookup lists a pool
 * only when it holds.
 */
export const AMM_POOL_PROTOCOL: WalletProtocol = [1, "amm pool"];

/** `<txid display hex>_<vout>` key ID for a spent outpoint, per the convention above. */
export function validatorKeyId(outpoint: { txid: string; vout: number }): string {
  return `${outpoint.txid}_${outpoint.vout}`;
}

/**
 * Derives the rotating validator signing public key (hex, compressed) as a
 * child of `validatorIdentityHex`, keyed by the outpoint the validator is
 * about to sign for (or, at deploy, the LP's first token-deposit input).
 * Callable by anyone: only `validatorIdentityHex` (public) is needed.
 */
export function deriveValidatorPubKey(
  validatorIdentityHex: string,
  outpoint: { txid: string; vout: number },
): string {
  const anyone = new KeyDeriver("anyone");
  const identity = PublicKey.fromString(validatorIdentityHex);
  const keyId = validatorKeyId(outpoint);
  return anyone
    .derivePublicKey(AMM_POOL_PROTOCOL, keyId, identity, false)
    .toString();
}

/** A pool's first validator key (0.13.0): the identity's anyone-child keyed by the nonce (64 hex digits). */
export function deriveFirstValidatorPubKey(validatorIdentityHex: string, nonceHex: string): string {
  const anyone = new KeyDeriver("anyone");
  return anyone.derivePublicKey(AMM_POOL_PROTOCOL, nonceHex.toLowerCase(), PublicKey.fromString(validatorIdentityHex), false).toString();
}
