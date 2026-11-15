//go:build ignore

// Vault holds a pair pool's second token (B) reserve. It is a BRC-162 value
// output with no payload, 1 sat, at output 1 of the transaction whose output 0
// is the pool:
//
//	0x20 <assetIdB:32> <push reserveB> OP_2DROP || code
//
// It has no state, no arguments and no key: one compiled script, the same in
// every pool. Its one rule: it is input 1 of a transaction whose input 0 is
// output 0 of the vault's own transaction (the pool). Whatever spends the pool
// releases the vault, and the pool's methods decide the outputs (Swap and
// Rescind write a new vault at output 1; Close leaves them free).
package pair

import runar "github.com/icellan/runar/packages/runar-go"

type Vault struct {
	runar.SmartContract
}

// Release lets the vault go with its pool. allPrevouts is the spending
// transaction's input outpoints, 36 bytes each, in order.
func (c *Vault) Release(allPrevouts runar.ByteString, txPreimage runar.SigHashPreimage) {
	runar.Assert(runar.CheckPreimage(txPreimage))
	// A stateless contract gets no sighash pin from the compiler, and
	// ANYONECANPAY zeroes hashPrevouts, the field read here. Rúnar's
	// CheckPreimage already signs with ALL|FORKID, so only such a preimage
	// passes it; this states the requirement in the contract itself.
	runar.Assert(runar.ExtractSigHashType(txPreimage) == 0x41)
	runar.Assert(runar.Hash256(allPrevouts) == runar.ExtractHashPrevouts(txPreimage))

	me := runar.ExtractOutpoint(txPreimage)
	txid := runar.Substr(me, 0, 32)
	runar.Assert(me == runar.Cat(txid, runar.ByteString("\x01\x00\x00\x00")))
	runar.Assert(runar.Substr(allPrevouts, 0, 36) == runar.Cat(txid, runar.ByteString("\x00\x00\x00\x00")))
	runar.Assert(runar.Substr(allPrevouts, 36, 36) == me)
}
