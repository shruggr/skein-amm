//go:build integration

package regtest

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The checks Swap makes beyond the curve and the outputs: address lengths,
// the next key's type byte, the token change's range and encoding, no
// negative fee rate. Each bad Swap carries the outputs the contract itself
// would write for its bad input, so the refusal is the check's and not a
// hashOutputs mismatch. Every refusal is traced: the log names the opcodes
// the pool input stopped at.

func TestStrictSwapRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	free := e.deploySDK(newParams(t, 0, 0, 0), newPoolState(t))
	h19, h21 := bytes.Repeat([]byte{0x9c}, 19), bytes.Repeat([]byte{0x9c}, 21)

	cases := []struct {
		name  string
		p     *pool
		build func(t *testing.T, p *pool) *spend
	}{
		{"userPkh of 19 bytes (sats-in)", p, func(t *testing.T, p *pool) *spend {
			r := planSwap(t, p, 20_000, true)
			r.userPkh = h19 // the arg and the payout output
			return swapSpend(p, r)
		}},
		{"userPkh of 21 bytes (token-in)", p, func(t *testing.T, p *pool) *spend {
			r := planSwap(t, p, 20_000, false)
			r.userPkh = h21
			return swapSpend(p, r)
		}},
		{"commissionPkh of 19 bytes, with a commission", p, func(t *testing.T, p *pool) *spend {
			r := planSwap(t, p, 20_000, true)
			r.commissionPkh = h19 // the arg and the commission output
			return swapSpend(p, r)
		}},
		{"commissionPkh of 19 bytes, no commission (zero-fee pool)", free, func(t *testing.T, p *pool) *spend {
			r := planSwap(t, p, 20_000, true)
			r.commissionPkh = h19 // the arg only: there is no commission output
			return swapSpend(p, r)
		}},
		{"tokenChangePkh of 21 bytes, with a token change", p, func(t *testing.T, p *pool) *spend {
			r := planSwap(t, p, 20_000, false)
			r.tokenChange, r.tokenChangePkh = 777, h21 // the arg and the change output
			return swapSpend(p, r)
		}},
		{"next key of 33 zero bytes", p, func(t *testing.T, p *pool) *spend { return withNextKey(t, p, make([]byte, 33)) }},
		{"next key 04 + 32 bytes", p, func(t *testing.T, p *pool) *spend {
			return withNextKey(t, p, append([]byte{0x04}, bytes.Repeat([]byte{0x5a}, 32)...))
		}},
		{"next key 07 + 32 bytes", p, func(t *testing.T, p *pool) *spend {
			return withNextKey(t, p, append([]byte{0x07}, bytes.Repeat([]byte{0x5a}, 32)...))
		}},
		{"tokenChange 2^64, with its output", p, func(t *testing.T, p *pool) *spend {
			r := planSwap(t, p, 20_000, false)
			s := swapSpend(p, r)
			push := append([]byte{0x09}, 0, 0, 0, 0, 0, 0, 0, 0, 0x01) // 2^64, minimal
			s.args[argTokenChange] = push
			s.outs = append(s.outs, out{1, append(tokenPrefixPush(p.p.assetID, push), p2pkhScript(r.tokenChangePkh)...)})
			return s
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := e.sub(t)
			s := c.build(t, c.p)
			s.label = "BAD swap: " + c.name
			e.reject(s, consensusReject, 0)
			e.swap(c.p, planSwap(t, c.p, 20_000, true)) // the honest spend of the same pool
		})
	}
	e.close(p)
	e.close(free)
}

// withNextKey is an honest sats-in Swap on p naming the raw next key k: the
// argument, and the continuation's state carrying k.
func withNextKey(t *testing.T, p *pool, k []byte) *spend {
	r := planSwap(t, p, 20_000, true)
	r.nextRaw = k
	return swapSpend(p, r)
}

// The next key's type byte is all Swap checks of it: 02 + any 32 bytes is
// accepted. No one can sign for that key, so the pool can then neither Swap
// nor Rescind; the LP still closes it.
func TestStrictNextKeyTypeByteOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	k := append([]byte{0x02}, bytes.Repeat([]byte{0x5a}, 32)...)
	r := planSwap(t, p, 20_000, true)
	r.nextRaw = k
	s := swapSpend(p, r)
	s.label = "swap: next key 02 + 32 arbitrary bytes"
	e.swapWith(p, r, s)
	if !bytes.Equal(p.st.validatorPub(), k) {
		t.Fatalf("the pool's validator key is %x, want %x", p.st.validatorPub(), k)
	}

	t.Run("a Swap signed by the key the harness generated", func(t *testing.T) {
		s := swapSpend(p, planSwap(t, p, 20_000, true)) // fee to Hash160(k), signer r.next
		s.label = "BAD swap: after a next key of 02 + 32 bytes"
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("a Rescind signed by the key the harness generated", func(t *testing.T) {
		s := rescindSpend(t, p, 0)
		s.label = "BAD rescind: after a next key of 02 + 32 bytes"
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("the LP's Close", func(t *testing.T) { e.sub(t).close(p) })
}

// tokenChange of 2^63, a nine-byte push (BRC-162 allows up to 2^64-1): the
// Swap is accepted and the mined change output carries that nine-byte amount.
func TestStrictTokenChange2Pow63(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	r := planSwap(t, p, 20_000, false)
	s := swapSpend(p, r)
	n := make([]byte, 9)
	binary.LittleEndian.PutUint64(n, 1<<63) // 00..00 80 00: the sign bit needs the ninth byte
	push := append([]byte{0x09}, n...)
	s.args[argTokenChange] = push
	want := append(tokenPrefixPush(p.p.assetID, push), p2pkhScript(r.tokenChangePkh)...)
	s.outs = append(s.outs, out{1, want})
	s.label = "swap: tokenChange 2^63 (nine-byte push)"
	_, got := e.swapWithTx(p, r, s)
	if last := *got.Outputs[len(got.Outputs)-1].LockingScript; !bytes.Equal(last, want) {
		t.Fatalf("the mined change output is %x, want %x", last, want)
	}
	e.close(p)
}

// policyNonMinimal is the node's reason for a non-minimally encoded number
// under its standard (policy) flags, when the consensus flags alone pass:
// "non-mandatory", not a consensus failure.
const policyNonMinimal = "non-mandatory-script-verify-flag (Non-minimally encoded script number)"

// A padded tokenChange push (17 as 02 11 00) whose change output carries the
// minimal encoding, which is what the contract writes. On consensus rules the
// pool input passes (the interpreter pre-flight), but this node's mempool
// policy (MINIMALDATA, enforced by regtest.sh's acceptnonstdtxn=0) refuses
// the push before the script's re-encoding matters: an honest spend refused
// by policy, kept as such. The same Swap with the minimal push is accepted,
// its mined change output the minimal form.
func TestStrictTokenChangeNonMinimalPush(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	minimalOut := func(r *swapPlan) []byte {
		m := append(tokenPrefixPush(p.p.assetID, []byte{0x01, 0x11}), p2pkhScript(r.tokenChangePkh)...)
		if !bytes.Equal(m, tokenP2pkh(p.p.assetID, 17, r.tokenChangePkh)) {
			t.Fatal("the minimal form of 17 is not 01 11")
		}
		return m
	}
	t.Run("pushed as 02 11 00, the output minimal: refused by policy only", func(t *testing.T) {
		r := planSwap(t, p, 20_000, false)
		s := swapSpend(p, r)
		s.label = "POLICY-REFUSED honest swap: tokenChange 02 11 00, output 01 11"
		s.args[argTokenChange] = []byte{0x02, 0x11, 0x00}
		s.outs = append(s.outs, out{1, minimalOut(r)})
		reason := e.sub(t).reject(s, policyNonMinimal) // no pool input fails on consensus rules
		t.Logf("HONEST SPEND REFUSED BY POLICY (consensus rules pass the pool input): %s", reason)
	})
	t.Run("pushed minimally, the same change: accepted, the output minimal", func(t *testing.T) {
		r := planSwap(t, p, 20_000, false)
		r.tokenChange = 17
		s := swapSpend(p, r)
		want := minimalOut(r)
		_, got := e.sub(t).swapWithTx(p, r, s)
		if last := *got.Outputs[len(got.Outputs)-1].LockingScript; !bytes.Equal(last, want) {
			t.Fatalf("the mined change output is %x, want the minimal %x", last, want)
		}
	})
	e.close(p)
}

// A pool deployed with a negative fee rate: Swap refused with the outputs the
// script's arithmetic gives (an LP fee of -119 is no output, and amountIn +
// 119 enters the pool); the LP still closes.
func TestStrictNegativeFeeRate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, -30, 0, 0), newPoolState(t))
	r := planSwap(t, p, 40_000, true)
	if r.lpFee != -119 || r.net != 40_119 {
		t.Fatalf("the script's arithmetic gives an LP fee of -119 and net 40119, the plan %d and %d", r.lpFee, r.net)
	}
	s := swapSpend(p, r)
	if len(s.outs) != 2 {
		t.Fatalf("a negative fee and two zero fees: want the pool and the payout, have %d outputs", len(s.outs))
	}
	s.label = "BAD swap: LpFeeBps -30"
	e.reject(s, consensusReject, 0)
	e.close(p)
}

// The trust model: Rúnar does not check the length of _changePKH
// (icellan/runar#213), and its change output is serialized with a fixed
// 0x19 length byte, so a _changePKH of
//
//	<20-byte hash> 88 ac <8-byte value> <varint len> <script minus its last two bytes>
//
// (the script ending 88 ac, which Rúnar's suffix supplies) makes the hashed
// outputs read as the change output followed by one more output. The script
// accepts such a Swap; the contract's header makes refusing it the
// validator's rule. This asserts what the node does with it on chain, under
// this node's policy, and that the extra output is mined.
func TestTrustModelLongChangePKH(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	r := planSwap(t, p, 20_000, true)
	s := swapSpend(p, r)
	changeTo, extraTo := pkh(newKey(t)), pkh(newKey(t))
	const changeSats, extraSats = 12_345, 54_321
	extra := p2pkhScript(extraTo)
	long := append([]byte{}, changeTo...)
	long = append(long, 0x88, 0xac)
	long = binary.LittleEndian.AppendUint64(long, extraSats)
	long = append(long, byte(len(extra)))
	long = append(long, extra[:len(extra)-2]...)
	s.changePkh, s.changeSats = long, changeSats
	s.outs = append(s.outs, out{changeSats, p2pkhScript(changeTo)}, out{extraSats, extra})
	s.label = "TRUST MODEL: swap with a 54-byte _changePKH carrying an extra output"
	_, got := e.swapWithTx(p, r, s)
	t.Logf("NODE ACCEPTED a Swap with an output past Rúnar's change (%d outputs mined; the last %d sats to %x): the script does not stop it, the validator must", len(got.Outputs), got.Outputs[len(got.Outputs)-1].Satoshis, extraTo)
	e.close(p)
}
