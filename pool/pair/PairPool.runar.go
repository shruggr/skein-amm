//go:build ignore

// PairPool is a single-LP constant-product AMM between two BSV-21 binary
// tokens (BRC-162), A and B. It is Pool (../Pool.runar.go) with the BSV
// reserve replaced by a second token.
//
// One output holds one token, so a pair pool is two outputs, always spent and
// written together:
//
//	output 0, the pool:   0x20 <assetIdA:32> <push reserveA> OP_2DROP || code || OP_RETURN || state
//	output 1, the vault:  0x20 <assetIdB:32> <push reserveB> OP_2DROP || VaultCode
//
// The pool carries the state and every rule. The vault (Vault.runar.go) is
// stateless and keyless: it can only be spent as input 1 of a transaction
// whose input 0 is output 0 of its own transaction, the pool. The pool in turn
// requires that vault as input 1 in every method (requireVault), so the two
// can never be parted: a pool spent without its vault would leave the vault
// unspendable. Input 0 the pool and input 1 the vault is a script rule here,
// not a convention.
//
// Both reserves live in state (ReserveA, ReserveB) and in the two prefixes;
// the contract writes both prefixes from its own copies and reads neither.
// The vault is 1 sat. The pool keeps whatever sats it was deployed with (1 by
// the lookup's rule): every continuation carries the spent pool's sats, so
// none can be skimmed through the change output.
//
// All payouts and fees are BRC-162 value outputs locked with P2PKH, 1 sat.
// The miner's fee and those sats come from the taker's own inputs; Rúnar's
// change output returns the rest.
//
// Fees are as Pool's (basis points of amountIn, rounded up, in the input
// token): LpFeeBps to LpFeeAPkh on an A-in Swap and LpFeeBPkh on a B-in Swap,
// ValidatorFeeBps to Hash160 of the current ValidatorPubKey, CommissionBps to
// the commissionPkh the relay names.
//
// The validator's and the LP's signatures must be SIGHASH_ALL|FORKID
// (requireSighashAll).
//
// The script can see no token input, in either direction. Every Swap needs
// the validator's signature, and the validator is trusted to confirm the
// token inputs of both assets cover the token outputs. (Pool's sats-in
// direction is enforced by the script; here neither is.)
//
// Methods, as Pool's: Swap, the validator's; Close, the LP's alone, outputs
// free; Rescind, the validator's, writing the pair back with ValidatorPubKey
// cleared. The Nonce and the affirmation are carried unchanged, as Pool's.
//
// The state is fixed-width, every field. The validator's affirmation is its
// signature's r and s, 32 bytes each (AffirmationR, AffirmationS), not the
// DER encoding Pool carries: a variable-length field is what makes Rúnar
// patch the code's length into the script at deploy (a pool deployed without
// the patch is unspendable), lets a deploy frame the field in a way the
// writer does not (the continuation then no longer parses), and costs every
// method a parser. With none of that, the template is the same in every
// pool but for its constructor arguments.
//
// Compile with Rúnar v1 (main at 6bd5e29f4 or later): the tests assert that
// compiler's script. It checks each fixed-width state field's size when a
// continuation is written, and its SDK refuses to build a pool with a field
// of the wrong width; an older Rúnar does neither.
//
// What the scripts cannot check, and the lookup must when a pair is admitted:
//
//   - the pool at output 0 and the vault at output 1 of one transaction;
//   - the pool's lock exactly prefix, code, OP_RETURN and the 251-byte
//     state, each field at its width, nothing after it. The code is the
//     canonical template with this pool's constructor arguments. Close reads
//     the state as the script's last 251 bytes, so anything appended moves
//     every field: 33 bytes of anything and Close obeys ValidatorPubKey, 66
//     and it obeys ValidatorIdentity; a field of the wrong width and no
//     method can spend the pair;
//   - both prefixes equal to the reserves in state, the ids to AssetIdA and
//     AssetIdB, each 32 bytes (the two different: Swap refuses a pair of one
//     token), the reserves positive;
//   - the vault's script the canonical Vault and VaultCodeHash its sha256;
//   - the vault 1 sat (its sats are not carried: any more leave through a
//     Swap's change), and the pool 1 sat (its sats are carried, and are the
//     LP's again at Close);
//   - LpPubKey and ValidatorPubKey public keys, the validator's the child of
//     ValidatorIdentity the Nonce names, the affirmation its signature (as
//     Pool's lookup does).
//
// And the validator, before it signs a Swap or a Rescind: the token inputs of
// both tokens cover the token outputs; _changePKH is 20 bytes and the
// transaction has no output past the contract's. (Rúnar does not check the
// length of _changePKH, and a longer one carries further outputs behind the
// change: icellan/runar#213. The contract cannot reach that argument.)
//
// The deploy's builder must check the pair's two outputs before it
// broadcasts. Every method needs output 1 of the pool's transaction as input
// 1: if a wallet put its token change there, the pool can be spent only
// together with that output, and never once it is spent.
package pair

import runar "github.com/icellan/runar/packages/runar-go"

type PairPool struct {
	runar.StatefulSmartContract

	ReserveA runar.Bigint
	ReserveB runar.Bigint

	LpPubKey          runar.PubKey
	ValidatorPubKey   runar.PubKey
	ValidatorIdentity runar.PubKey
	Nonce             runar.Sha256Digest

	// Where Swap pays the LP fee: LpFeeAPkh on an A-in Swap (the fee in A),
	// LpFeeBPkh on a B-in Swap (the fee in B). Never assigned.
	LpFeeAPkh runar.Addr
	LpFeeBPkh runar.Addr

	// The validator's affirmation of the pool: its signature's r and s,
	// big-endian, 32 bytes each. State only so the compiler keeps them (as the
	// Nonce); never assigned.
	AffirmationR runar.Sha256Digest
	AffirmationS runar.Sha256Digest

	// The two tokens' canonical 32-byte BRC-162 ids. Rúnar's fixed 32-byte
	// type (as the Nonce): Rúnar v1's SDK refuses to build a pool with an id
	// of any other length. No script checks it (the lookup does).
	AssetIdA runar.Sha256Digest `runar:"readonly"`
	AssetIdB runar.Sha256Digest `runar:"readonly"`
	// VaultCodeHash is sha256 of the compiled Vault script, the same in every
	// pool; Swap and Rescind take the script itself as an argument. The lookup
	// checks it is the canonical one's.
	VaultCodeHash   runar.Sha256Digest `runar:"readonly"`
	LpFeeBps        runar.Bigint       `runar:"readonly"`
	ValidatorFeeBps runar.Bigint       `runar:"readonly"`
	CommissionBps   runar.Bigint       `runar:"readonly"`
}

// Swap trades amountIn of one token for the other along x·y = k. aIn selects
// the direction (true: A in, B out). Fees are taken from amountIn in the input
// token; only amountIn minus fees enters the pool.
//
// Outputs: 0 pool, 1 vault, 2 user payout (the output token), then LP fee,
// validator fee and commission (the input token), each only when nonzero;
// then the taker's token change (the input token), tokenChange to
// tokenChangePkh, only when nonzero; then Rúnar's sats change.
//
// allPrevouts is the transaction's input outpoints, 36 bytes each, in order;
// vaultCode the compiled Vault script (VaultCodeHash's preimage).
func (c *PairPool) Swap(validatorSig runar.Sig, nextValidatorPubKey runar.PubKey, amountIn runar.Bigint, aIn runar.Bool, userPkh runar.Addr, commissionPkh runar.Addr, tokenChangePkh runar.Addr, tokenChange runar.Bigint, allPrevouts runar.ByteString, vaultCode runar.ByteString) {
	runar.Assert(runar.CheckSig(validatorSig, c.ValidatorPubKey))
	c.requireSighashAll(validatorSig)
	c.requireSwappable()
	// amountIn and the taker's change are BRC-162 amounts: at most 2^64-1.
	// Every other amount in a prefix follows: a fee is at most amountIn, and
	// the reserves and the payout are under 2^63 by the state's 8 bytes. The
	// change is written as a minimal number below, whatever push the argument
	// came in; the others are the script's own arithmetic, minimal by
	// construction.
	runar.Assert(amountIn > 0)
	runar.Assert(amountIn <= 18446744073709551615)
	runar.Assert(tokenChange >= 0)
	runar.Assert(tokenChange <= 18446744073709551615)
	change := runar.Bin2Num(runar.Num2Bin(tokenChange, 9))
	// The next key is a compressed public key by its length and first byte:
	// the cleared key of a rescinded pool is written by Rescind alone. (Rúnar
	// v1 checks the 33 bytes itself when the state is written; an older Rúnar
	// does not, and a key of another length there writes a continuation no
	// method can spend.)
	runar.Assert(runar.Len(nextValidatorPubKey) == 33)
	keyType := runar.Substr(nextValidatorPubKey, 0, 1)
	runar.Assert(keyType == runar.ByteString("\x02") || keyType == runar.ByteString("\x03"))
	// Every address is 20 bytes: any other length makes the output it is
	// written into a script that is not P2PKH.
	runar.Assert(runar.Len(userPkh) == 20)
	runar.Assert(runar.Len(commissionPkh) == 20)
	runar.Assert(runar.Len(tokenChangePkh) == 20)
	c.requireVault(allPrevouts, runar.ExtractHashPrevouts(c.TxPreimage), runar.ExtractOutpoint(c.TxPreimage))

	// Read before writePair's GetStateScript (a state property read after it
	// compiles to the slot value).
	lpPkh := c.LpFeeAPkh
	if !aIn {
		lpPkh = c.LpFeeBPkh
	}
	inAsset := c.AssetIdA
	if !aIn {
		inAsset = c.AssetIdB
	}
	outAsset := c.AssetIdB
	if !aIn {
		outAsset = c.AssetIdA
	}
	validatorPkh := runar.Hash160(c.ValidatorPubKey)
	sats := runar.ExtractAmount(c.TxPreimage)

	// Fees round up, in the pool's favour: a nonzero rate always takes >= 1.
	lpFee := (amountIn*c.LpFeeBps + 9999) / 10000
	validatorFee := (amountIn*c.ValidatorFeeBps + 9999) / 10000
	commission := (amountIn*c.CommissionBps + 9999) / 10000
	net := amountIn - lpFee - validatorFee - commission
	runar.Assert(net > 0)

	out := net * c.ReserveA / (c.ReserveB + net)
	newA := c.ReserveA - out
	newB := c.ReserveB + net
	if aIn {
		out = net * c.ReserveB / (c.ReserveA + net)
		newA = c.ReserveA + net
		newB = c.ReserveB - out
	}
	runar.Assert(out > 0)

	c.ReserveA = newA
	c.ReserveB = newB
	c.ValidatorPubKey = nextValidatorPubKey
	c.writePair(sats, vaultCode)
	c.AddRawOutput(1, c.tokenP2pkh(outAsset, out, userPkh))
	// Scripts computed outside the conditionals (Pool's note: a conditional
	// output whose argument inlines nested branches trips a codegen error).
	lpScript := c.tokenP2pkh(inAsset, lpFee, lpPkh)
	validatorScript := c.tokenP2pkh(inAsset, validatorFee, validatorPkh)
	commissionScript := c.tokenP2pkh(inAsset, commission, commissionPkh)
	tokenChangeScript := c.tokenP2pkh(inAsset, change, tokenChangePkh)
	if lpFee > 0 {
		c.AddRawOutput(1, lpScript)
	}
	if validatorFee > 0 {
		c.AddRawOutput(1, validatorScript)
	}
	if commission > 0 {
		c.AddRawOutput(1, commissionScript)
	}
	if change > 0 {
		c.AddRawOutput(1, tokenChangeScript)
	}
}

// Close ends the pool, the LP's alone: a signature by the LP key, the outputs
// free. It requires the vault as input 1, so a Close that forgot the vault is
// a rejected transaction, not a stranded reserve. (With a fixed-width state
// Close takes no _codePart witness: Swap and Rescind do.)
func (c *PairPool) Close(lpSig runar.Sig, allPrevouts runar.ByteString) {
	c.requireSighashAll(lpSig)
	c.requireVault(allPrevouts, runar.ExtractHashPrevouts(c.TxPreimage), runar.ExtractOutpoint(c.TxPreimage))
	// Last: a public method must end on an assert.
	runar.Assert(runar.CheckSig(lpSig, c.LpPubKey))
}

// Rescind is the validator's withdrawal of its attestation: the pool and the
// vault written back unchanged at outputs 0 and 1, ValidatorPubKey cleared to
// 33 zero bytes. No Swap and no second Rescind after it; the LP closes.
func (c *PairPool) Rescind(validatorSig runar.Sig, allPrevouts runar.ByteString, vaultCode runar.ByteString) {
	runar.Assert(runar.CheckSig(validatorSig, c.ValidatorPubKey))
	c.requireSighashAll(validatorSig)
	c.requireVault(allPrevouts, runar.ExtractHashPrevouts(c.TxPreimage), runar.ExtractOutpoint(c.TxPreimage))
	sats := runar.ExtractAmount(c.TxPreimage)
	c.ValidatorPubKey = runar.Num2Bin(0, 33)
	c.writePair(sats, vaultCode)
}

// requireVault: this pool is input 0 and output 0 of its transaction, and
// input 1 is output 1 of that same transaction, the pool's vault.
// hashPrevouts and me (this input's outpoint) are the preimage's, read by the
// public method: a private method called before the method's own last use of
// the preimage cannot read c.TxPreimage itself (a Rúnar stack-lowering limit).
func (c *PairPool) requireVault(allPrevouts runar.ByteString, hashPrevouts runar.Sha256Digest, me runar.ByteString) {
	runar.Assert(runar.Hash256(allPrevouts) == hashPrevouts)
	txid := runar.Substr(me, 0, 32)
	runar.Assert(me == runar.Cat(txid, runar.ByteString("\x00\x00\x00\x00")))
	runar.Assert(runar.Substr(allPrevouts, 0, 36) == me)
	runar.Assert(runar.Substr(allPrevouts, 36, 36) == runar.Cat(txid, runar.ByteString("\x01\x00\x00\x00")))
}

// requireSwappable: what a deploy must have got right for a Swap to make
// sense, checked in Swap alone so that a pool deployed wrong can still be
// closed by its LP (and rescinded). The two tokens differ, and no fee rate is
// negative (a negative rate is a negative fee: more than amountIn credited to
// the pool, and no fee output). Rates summing to 100% or more need no check:
// they leave no net, which Swap refuses.
func (c *PairPool) requireSwappable() {
	runar.Assert(c.AssetIdA != c.AssetIdB)
	runar.Assert(c.LpFeeBps >= 0)
	runar.Assert(c.ValidatorFeeBps >= 0)
	runar.Assert(c.CommissionBps >= 0)
}

// requireSighashAll: the signer's signature is SIGHASH_ALL|FORKID. The
// preimage is that type by Rúnar's own check, which pins this transaction's
// outputs for the script; the signature's type decides what the signer has
// agreed to. Any other type would let a third party reuse it over outputs or
// inputs of their own (a NONE validator signature: a whole other Swap).
func (c *PairPool) requireSighashAll(sig runar.Sig) {
	runar.Assert(runar.Substr(sig, runar.Len(sig)-1, 1) == runar.ByteString("\x41"))
}

// writePair emits the pool continuation (output 0, holding sats) and its
// vault (output 1, 1 sat) for the current state.
func (c *PairPool) writePair(sats runar.Bigint, vaultCode runar.ByteString) {
	runar.Assert(c.ReserveA > 0)
	runar.Assert(c.ReserveB > 0)
	runar.Assert(runar.Sha256(vaultCode) == c.VaultCodeHash)
	// The vault script before poolScript's GetStateScript (it reads ReserveB).
	vault := runar.Cat(c.tokenPrefix(c.AssetIdB, c.ReserveB), vaultCode)
	c.AddRawOutput(sats, c.poolScript())
	c.AddRawOutput(1, vault)
}

// poolScript is the pool's locking script for the current state: token A's
// prefix for ReserveA, this contract's code (from the preimage scriptCode),
// and the state. Only ReserveA, ReserveB and ValidatorPubKey are ever
// assigned and all three are fixed-width, so the new state is exactly as long
// as the spent one. The varint is 1 or 3 bytes: the script is under 0x10000
// bytes, having no field a deploy could make long.
func (c *PairPool) poolScript() runar.ByteString {
	prefix := c.tokenPrefix(c.AssetIdA, c.ReserveA)
	state := c.GetStateScript()
	sc := runar.ExtractScriptCode(c.TxPreimage)
	first := runar.Bin2Num(runar.Cat(runar.Substr(sc, 0, 1), runar.Num2Bin(0, 1)))
	varintLen := runar.Bigint(1)
	if first == 253 {
		varintLen = 3
	}
	body := runar.Substr(sc, varintLen, runar.Len(sc)-varintLen-runar.Len(state))

	// Re-add the two prologue bytes (OP_NOP OP_CODESEPARATOR) that sit before
	// the separator and are therefore not in scriptCode.
	script := runar.Cat(prefix, runar.Cat(runar.ByteString("\x61\xab"), body))
	return runar.Cat(script, state)
}

// tokenP2pkh is a BRC-162 value output of amount of assetId to pkh.
func (c *PairPool) tokenP2pkh(assetId runar.Sha256Digest, amount runar.Bigint, pkh runar.Addr) runar.ByteString {
	return runar.Cat(c.tokenPrefix(assetId, amount), p2pkh(pkh))
}

// tokenPrefix is the BRC-162 value-output prefix push(assetId) push(amount)
// OP_2DROP, no payload (as Pool's). Callers only output it for amount > 0.
func (c *PairPool) tokenPrefix(assetId runar.Sha256Digest, amount runar.Bigint) runar.ByteString {
	push := runar.Num2Bin(runar.Clamp(amount, 0, 16)+80, 1) // OP_1..OP_16
	if amount > 16 {
		amt := runar.Pack(amount)
		push = runar.Cat(runar.Num2Bin(runar.Len(amt), 1), amt)
	}
	prefix := runar.Cat(runar.ByteString("\x20"), assetId)
	return runar.Cat(prefix, runar.Cat(push, runar.ByteString("\x6d")))
}

func p2pkh(pkh runar.Addr) runar.ByteString {
	return runar.Cat(runar.ByteString("\x76\xa9\x14"), runar.Cat(pkh, runar.ByteString("\x88\xac")))
}
