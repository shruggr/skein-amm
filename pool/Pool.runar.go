//go:build ignore

// Pool is a single-LP constant-product AMM between BSV and one BSV-21 binary
// token (BRC-162).
//
// On-chain locking script (a BRC-162 value output with no payload):
//
//	0x20 <assetId:32> <push tokenReserve> OP_2DROP || code || OP_RETURN || state
//
// The BSV reserve is the output's satoshi value. The token reserve lives in
// the token prefix and is duplicated in state (TokenReserve), because the
// prefix sits before Rúnar's OP_CODESEPARATOR and is not covered by the
// preimage. The same goes for the asset id, a readonly property compiled into
// the code. The contract never reads the input's prefix; it always writes the
// continuation's prefix from its own copies. Overlays must check prefix ==
// state (and a 32-byte AssetId) once, when a pool is first admitted.
//
// Token outputs (payouts and fees) are BRC-162 value outputs locked with
// P2PKH, holding 1 sat.
//
// Fees (each fixed at deploy, in basis points of amountIn, rounded up, taken
// in the input asset): LpFeeBps to the LP, ValidatorFeeBps to the validator,
// and CommissionBps to whoever relays the swap (a market, or the user's own
// overlay), at the address it names per call. The contract builds every
// output of a Swap itself; a swap carries no arbitrary outputs.
// The script cannot see token inputs, so every method that brings tokens in
// requires the validator's signature: the validator's role is to confirm the
// token inputs are valid and cover the token outputs. Three methods
// (skein-amm 0.13.0, David Case 2026-10-22, docs/AMM.md "The pool rule"):
// Swap, the validator's; Close, the LP's alone, its outputs free (the LP
// spends the LP's own funds); Rescind, the validator's, writing the pool back
// with its ValidatorPubKey cleared (33 zero bytes), which ends its swaps.
//
// The validator's affirmation lives in state (0.13.0): Nonce, AffirmationR and
// AffirmationS, never assigned, so every continuation carries them. Every pool
// output is self-attesting: ValidatorPubKey is the BRC-42 child of
// ValidatorIdentity keyed by Nonce (first key), and the affirmation is that
// key's signature over sha256 of the message (docs/AMM.md "The deploy and the
// affirmation"), here as its r and s, 32 bytes each. The contract never checks
// either; the lookup does.
//
// The state is fixed-width, every field: the affirmation is r and s (each
// big-endian, left-padded to 32 bytes) and not the DER encoding. A variable-length field is what makes Rúnar patch the
// code's length into the script at deploy (a pool deployed without the patch
// is unspendable), lets a deploy frame the field in a way the writer does not
// (the continuation then no longer parses, and the pool is stuck), and costs
// every method a parser.
//
// What the script cannot check, the lookup must when a pool is admitted: the
// lock is exactly prefix, code, OP_RETURN and the state, nothing after it
// (Close reads the state as the script's last bytes: a second state appended
// is the one Close obeys), with the prefix equal to the state and the code,
// as before. And the validator must, before it signs a Swap or a Rescind: the token inputs cover the token outputs; _changePKH is 20 bytes
// and the transaction has no output past the contract's. (Rúnar does not
// check the length of _changePKH, and a longer one carries further outputs
// behind the change: icellan/runar#213. The contract cannot reach that
// argument, so "a swap carries no arbitrary outputs" is the validator's rule
// as much as the script's.)
package pool

import runar "github.com/icellan/runar/packages/runar-go"

type Pool struct {
	runar.StatefulSmartContract

	TokenReserve runar.Bigint
	// The validator's key rotates: it names its next key on every Swap it
	// signs; its fee goes to Hash160 of the current key. Next keys are derived
	// off-chain so wallets can find their payouts from overlay data. The LP's key
	// is the pool's for its life: it signs Close (0.55.0: the LP fee goes to
	// LpSatsPkh / LpTokenPkh, not to this key).
	LpPubKey        runar.PubKey
	ValidatorPubKey runar.PubKey

	// ValidatorIdentity is state only so the compiler keeps it: Rúnar drops
	// unreferenced readonly fields, and the Go DSL has no way to opt out (see
	// docs/notes.md, upstream candidate #6). It is never assigned, so every
	// continuation carries it forward unchanged.
	//
	// It is the validator's stable identity key: who a taker
	// contacts to execute a swap (BRC-103 auth; resolving it to an address is
	// deferred), and the BRC-42 root of the rotating ValidatorPubKey, derived
	// with the `anyone` counterparty: keyed by Nonce for the deploy's key, by
	// the spent pool outpoint for each Swap's next key. The contract never
	// checks the derivation: the validator won't sign a spend that breaks it,
	// and the lookup lists no pool that breaks it.
	ValidatorIdentity runar.PubKey

	// Nonce (32 bytes, the validator's choice at affirm) keys the pool's first
	// validator key; AffirmationR and AffirmationS (below) are the validator's
	// signature of the pool (0.13.0, docs/AMM.md "The deploy and the
	// affirmation"): its r and s, big-endian, 32 bytes each. State only so the
	// compiler keeps them (as ValidatorIdentity); never assigned, so every
	// continuation carries them unchanged. All three are Rúnar's fixed 32-byte
	// type (raw in the state, no framing).
	Nonce runar.Sha256Digest

	// LpSatsPkh and LpTokenPkh (0.55.0, David Case 2026-11-12, agent-env
	// plans/lp-fee-addresses-settled-2026-11-12.md): where Swap pays the LP fee,
	// LpSatsPkh on a sats-in Swap (the fee in sats), LpTokenPkh on a token-in
	// Swap (the fee in tokens). Fixed at deploy, never assigned, so every
	// continuation carries them unchanged (as the Nonce). The LP's wallet derives
	// both from the pool's key id: a BRC-29 key for the sats (taken in as a
	// wallet payment), a 1Color module receiving key for the tokens (taken in as
	// a `p color` receipt).
	LpSatsPkh  runar.Addr
	LpTokenPkh runar.Addr

	AffirmationR runar.Sha256Digest
	AffirmationS runar.Sha256Digest

	// AssetId is the token's canonical 32-byte BRC-162 id (the deploy txid;
	// binary deploys are at output 0). 36-byte legacy ids are not supported.
	// Rúnar's fixed 32-byte type (as the Nonce): Rúnar v1's SDK refuses to
	// build a pool with an id of any other length. No script checks it (the
	// lookup does).
	AssetId         runar.Sha256Digest `runar:"readonly"`
	LpFeeBps        runar.Bigint       `runar:"readonly"`
	ValidatorFeeBps runar.Bigint       `runar:"readonly"`
	// CommissionBps is the relay's commission. Markets filter pools by it.
	CommissionBps runar.Bigint `runar:"readonly"`
}

// Swap trades amountIn of one asset for the other along x·y = k.
// bsvIn selects the direction. Three fees are taken from amountIn in the
// input asset: the LP fee, the validator fee and the commission, paid to
// commissionPkh (named by the relay per call). Only amountIn minus fees
// enters the pool. A CommissionBps of 0 means no commission output.
//
// Outputs: 0 pool, 1 user payout, then LP fee (0.55.0: to LpSatsPkh on a
// sats-in Swap, to LpTokenPkh on a token-in Swap), validator fee and
// commission, each only when nonzero; then (skein-amm 0.40.0) the taker's token change,
// tokenChange tokens to tokenChangePkh (1 sat), only when nonzero; then
// Rúnar's sats change.
//
// What Swap checks beyond the curve and the outputs: the validator's
// signature is SIGHASH_ALL|FORKID; the pool is the transaction's input 0
// (allPrevouts is the transaction's input outpoints, 36 bytes each, in
// order); no fee rate is negative; the next key is a compressed public key by
// its length and first byte; each address is 20 bytes; amountIn and the token
// change are at most 2^64-1 (BRC-162's limit on an amount), and the change is
// written as a minimal number.
func (c *Pool) Swap(validatorSig runar.Sig, nextValidatorPubKey runar.PubKey, amountIn runar.Bigint, bsvIn runar.Bool, userPkh runar.Addr, commissionPkh runar.Addr, tokenChangePkh runar.Addr, tokenChange runar.Bigint, allPrevouts runar.ByteString) {
	runar.Assert(runar.CheckSig(validatorSig, c.ValidatorPubKey))
	c.requireSighashAll(validatorSig)
	c.requireInputZero(allPrevouts)
	// A negative rate is a negative fee: more than amountIn credited to the
	// pool, and no fee output. Checked here alone, so a pool deployed with one
	// can still be closed by its LP (and rescinded).
	runar.Assert(c.LpFeeBps >= 0)
	runar.Assert(c.ValidatorFeeBps >= 0)
	runar.Assert(c.CommissionBps >= 0)
	// amountIn and the taker's change are at most 2^64-1, BRC-162's limit on
	// an amount. Every other token amount in a prefix follows: a fee is at
	// most amountIn, and the reserve and the payout are under 2^63 by the
	// state's 8 bytes. The change is written as a minimal number below,
	// whatever push the argument came in; the others are the script's own
	// arithmetic, minimal by construction.
	runar.Assert(amountIn > 0)
	runar.Assert(amountIn <= 18446744073709551615)
	runar.Assert(tokenChange >= 0)
	runar.Assert(tokenChange <= 18446744073709551615)
	change := runar.Bin2Num(runar.Num2Bin(tokenChange, 9))
	// The next key is a compressed public key by its length and first byte:
	// the cleared key of a rescinded pool is written by Rescind alone. (Rúnar
	// v1 checks the 33 bytes itself when the state is written. An older Rúnar
	// does not, and there a key of another length writes a continuation the
	// LP cannot Close; one long enough to carry script of its own, a
	// continuation the validator's key can.)
	runar.Assert(runar.Len(nextValidatorPubKey) == 33)
	keyType := runar.Substr(nextValidatorPubKey, 0, 1)
	runar.Assert(keyType == runar.ByteString("\x02") || keyType == runar.ByteString("\x03"))
	// Every address is 20 bytes: any other length makes the output it is
	// written into a script that is not P2PKH.
	runar.Assert(runar.Len(userPkh) == 20)
	runar.Assert(runar.Len(commissionPkh) == 20)
	runar.Assert(runar.Len(tokenChangePkh) == 20)
	// 0.55.0: the LP fee's address by direction (read here, before writePool's
	// GetStateScript: a property read after it compiles to the slot value).
	lpPkh := c.LpSatsPkh
	if !bsvIn {
		lpPkh = c.LpTokenPkh
	}
	validatorPkh := runar.Hash160(c.ValidatorPubKey)

	bsvReserve := runar.ExtractAmount(c.TxPreimage)
	// Fees round up, in the pool's favour: a nonzero rate always takes >= 1.
	lpFee := (amountIn*c.LpFeeBps + 9999) / 10000
	validatorFee := (amountIn*c.ValidatorFeeBps + 9999) / 10000
	commission := (amountIn*c.CommissionBps + 9999) / 10000
	net := amountIn - lpFee - validatorFee - commission
	runar.Assert(net > 0)

	out := net * bsvReserve / (c.TokenReserve + net)
	newBsv := bsvReserve - out
	newTokens := c.TokenReserve + net
	if bsvIn {
		out = net * c.TokenReserve / (bsvReserve + net)
		newBsv = bsvReserve + net
		newTokens = c.TokenReserve - out
	}
	runar.Assert(out > 0)

	c.TokenReserve = newTokens
	c.ValidatorPubKey = nextValidatorPubKey
	c.writePool(newBsv)
	c.AddRawOutput(c.payoutSats(out, !bsvIn), c.payoutScript(out, userPkh, !bsvIn))
	// Scripts computed outside the conditionals: a conditional output whose
	// argument inlines nested branches (payoutScript -> tokenPrefix) trips
	// a Rúnar codegen error. A single level of branching is fine.
	lpScript := c.payoutScript(lpFee, lpPkh, bsvIn)
	validatorScript := c.payoutScript(validatorFee, validatorPkh, bsvIn)
	commissionScript := c.payoutScript(commission, commissionPkh, bsvIn)
	tokenChangeScript := c.tokenP2pkh(change, tokenChangePkh)
	if lpFee > 0 {
		c.AddRawOutput(c.payoutSats(lpFee, bsvIn), lpScript)
	}
	if validatorFee > 0 {
		c.AddRawOutput(c.payoutSats(validatorFee, bsvIn), validatorScript)
	}
	if commission > 0 {
		c.AddRawOutput(c.payoutSats(commission, bsvIn), commissionScript)
	}
	if change > 0 {
		c.AddRawOutput(1, tokenChangeScript)
	}
}

// Close ends the pool, the LP's alone (skein-amm 0.13.0, David Case
// 2026-10-22): a plain signature by the LP key, and the outputs are free. The
// LP spends the LP's own funds: the LP's wallet decides where the sats and the
// tokens go (a covenant would protect no one). A position is deployed or
// closed, never resized in place (2026-10-09). No state is written and no
// output is added, so the compiler commits to no output (no hashOutputs check,
// no change parameters).
//
// The LP's signature is SIGHASH_ALL|FORKID (requireSighashAll): with the
// outputs free, a signature of any other type would let a third party
// rewrite them.
func (c *Pool) Close(lpSig runar.Sig) {
	c.requireSighashAll(lpSig)
	// Last: a public method must end on an assert.
	runar.Assert(runar.CheckSig(lpSig, c.LpPubKey))
}

// Rescind is the validator's withdrawal of its attestation (0.13.0, David
// Case 2026-10-22; it replaces 0.11.0's validator Close): signed by the current
// ValidatorPubKey, it writes the pool back at output 0 unchanged (the same
// sats, TokenReserve, LpPubKey, ValidatorIdentity, Nonce, LpSatsPkh, LpTokenPkh
// and the affirmation) with
// ValidatorPubKey cleared to 33 zero bytes. No Swap is possible after it (no
// key can sign against the zero key, nor can a second Rescind), and the lookup
// delists the pool on seeing it; the LP closes it as any pool. The validator
// pays the miner from an input of its own, its change Rúnar's change output:
// nothing is taken from the pool.
//
// As Swap: the signature is SIGHASH_ALL|FORKID and the pool is input 0.
func (c *Pool) Rescind(validatorSig runar.Sig, allPrevouts runar.ByteString) {
	runar.Assert(runar.CheckSig(validatorSig, c.ValidatorPubKey))
	c.requireSighashAll(validatorSig)
	c.requireInputZero(allPrevouts)
	sats := runar.ExtractAmount(c.TxPreimage)
	c.ValidatorPubKey = runar.Num2Bin(0, 33)
	c.writePool(sats)
}

// requireSighashAll: the signer's signature is SIGHASH_ALL|FORKID. The
// preimage is that type by Rúnar's own check, which pins this transaction's
// outputs for the script; the signature's type decides what the signer has
// agreed to. Any other type would let a third party reuse it over outputs or
// inputs of their own.
func (c *Pool) requireSighashAll(sig runar.Sig) {
	runar.Assert(runar.Substr(sig, runar.Len(sig)-1, 1) == runar.ByteString("\x41"))
}

// requireInputZero: this pool is the transaction's first input. Each pool
// builds every output from output 0 on, so two pools with the same script and
// sats in one transaction would otherwise both accept one output list, and
// the second pool's reserve would leave through the change.
func (c *Pool) requireInputZero(allPrevouts runar.ByteString) {
	runar.Assert(runar.Hash256(allPrevouts) == runar.ExtractHashPrevouts(c.TxPreimage))
	runar.Assert(runar.Substr(allPrevouts, 0, 36) == runar.ExtractOutpoint(c.TxPreimage))
}

// writePool emits the pool continuation holding bsvReserve.
func (c *Pool) writePool(bsvReserve runar.Bigint) {
	runar.Assert(bsvReserve > 0)
	runar.Assert(c.TokenReserve > 0)
	c.AddRawOutput(bsvReserve, c.poolScript())
}

// poolScript is the pool's locking script for the current state: token
// prefix for TokenReserve, this contract's code (recovered from the preimage
// scriptCode), and the state.
func (c *Pool) poolScript() runar.ByteString {
	// scriptCode = varint || code[2:] || OP_RETURN || state, where state is
	// TokenReserve (8) + LpPubKey, ValidatorPubKey, ValidatorIdentity (33
	// each) + Nonce (32) + LpSatsPkh, LpTokenPkh (20 each, 0.55.0) +
	// AffirmationR, AffirmationS (32 each). Every field is fixed-width, so the
	// new state is exactly as long as the spent one, and the script is under
	// 0x10000 bytes (the varint below is 3 bytes; 1 is kept for a script
	// under 253), having no state field a deploy could make long.
	//
	// The prefix is built before GetStateScript: Rúnar's codegen treats
	// GetStateScript as the state properties' last use, and a property read
	// after it compiles to the constructor's value (a slot), not the state's.
	prefix := c.tokenPrefix(c.TokenReserve)
	state := c.GetStateScript()
	sc := runar.ExtractScriptCode(c.TxPreimage)
	first := runar.Bin2Num(runar.Cat(runar.Substr(sc, 0, 1), runar.Num2Bin(0, 1)))
	varintLen := runar.Bigint(1)
	if first == 253 {
		varintLen = 3
	}
	body := runar.Substr(sc, varintLen, runar.Len(sc)-varintLen-runar.Len(state))

	// Re-add the two prologue bytes (OP_NOP OP_CODESEPARATOR) that sit
	// before the separator and are therefore not in scriptCode.
	script := runar.Cat(prefix, runar.Cat(runar.ByteString("\x61\xab"), body))
	return runar.Cat(script, state)
}

// payoutSats is the output value for paying amount of BSV (isBsv) or tokens.
func (c *Pool) payoutSats(amount runar.Bigint, isBsv runar.Bool) runar.Bigint {
	sats := runar.Bigint(1)
	if isBsv {
		sats = amount
	}
	return sats
}

// payoutScript pays amount of BSV (isBsv) or tokens to pkh.
func (c *Pool) payoutScript(amount runar.Bigint, pkh runar.Addr, isBsv runar.Bool) runar.ByteString {
	s := p2pkh(pkh)
	if !isBsv {
		s = c.tokenP2pkh(amount, pkh)
	}
	return s
}

// tokenP2pkh is a BRC-162 value output of amount to pkh (no payload).
func (c *Pool) tokenP2pkh(amount runar.Bigint, pkh runar.Addr) runar.ByteString {
	return runar.Cat(c.tokenPrefix(amount), p2pkh(pkh))
}

// tokenPrefix is the BRC-162 value-output prefix push(assetId) push(amount)
// OP_2DROP, with no payload. The id push length is fixed at 32 bytes; the
// amount is minimally pushed (OP_1..OP_16 for small values). Callers only
// emit it for amount > 0 (a zero amount would be an authority output);
// zero-amount scripts are built but never output.
func (c *Pool) tokenPrefix(amount runar.Bigint) runar.ByteString {
	push := runar.Num2Bin(runar.Clamp(amount, 0, 16)+80, 1) // OP_1..OP_16
	if amount > 16 {
		amt := runar.Pack(amount)
		push = runar.Cat(runar.Num2Bin(runar.Len(amt), 1), amt)
	}
	prefix := runar.Cat(runar.ByteString("\x20"), c.AssetId)
	return runar.Cat(prefix, runar.Cat(push, runar.ByteString("\x6d")))
}

func p2pkh(pkh runar.Addr) runar.ByteString {
	return runar.Cat(runar.ByteString("\x76\xa9\x14"), runar.Cat(pkh, runar.ByteString("\x88\xac")))
}
