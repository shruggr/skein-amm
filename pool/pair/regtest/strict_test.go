//go:build integration

package regtest

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The checks Swap makes beyond the curve and the outputs: address lengths,
// the next key's type byte, the token change's size and encoding, and what a
// deploy must have got right (two different tokens, no negative fee rate).
// Each bad Swap carries the outputs the contract itself would write for its
// bad input, so the refusal is the check's and not a hashOutputs mismatch
// (except where a case says so). Every refusal is traced: the log names the
// opcodes the pool input stopped at.

// The pool's Swap args after the signature, by index (swapSpend's order).
const (
	argNextKey = iota
	argAmountIn
	argAIn
	argUserPkh
	argCommissionPkh
	argTokenChangePkh
	argTokenChange
)

// tokenPrefixPush is a token prefix with a raw amount push.
func tokenPrefixPush(assetID, push []byte) []byte {
	p := append([]byte{0x20}, assetID...)
	return append(append(p, push...), 0x6d)
}

// inAsset is the input token of a plan on pr.
func inAsset(pr *pair, r *swapPlan) []byte {
	if r.aIn {
		return pr.p.assetA
	}
	return pr.p.assetB
}

func outAsset(pr *pair, r *swapPlan) []byte {
	if r.aIn {
		return pr.p.assetB
	}
	return pr.p.assetA
}

func TestStrictSwapRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	free := e.deploySDK(newParams(t, 0, 0, 0), newPairState(t))
	h19, h21 := bytes.Repeat([]byte{0x9c}, 19), bytes.Repeat([]byte{0x9c}, 21)

	// Each case builds its bad Swap from a fresh plan on its pair.
	cases := []struct {
		name  string
		pr    *pair
		build func(t *testing.T, pr *pair) *spend
	}{
		{"userPkh of 19 bytes", pr, func(t *testing.T, pr *pair) *spend {
			r := planSwap(t, pr, 20_000, true)
			s := swapSpend(pr, r)
			s.ins[0].args[argUserPkh] = pushData(h19)
			s.outs[2].script = append(tokenPrefix(outAsset(pr, r), r.out), p2pkhScript(h19)...)
			return s
		}},
		{"userPkh of 21 bytes", pr, func(t *testing.T, pr *pair) *spend {
			r := planSwap(t, pr, 20_000, false)
			s := swapSpend(pr, r)
			s.ins[0].args[argUserPkh] = pushData(h21)
			s.outs[2].script = append(tokenPrefix(outAsset(pr, r), r.out), p2pkhScript(h21)...)
			return s
		}},
		{"commissionPkh of 19 bytes, with a commission", pr, func(t *testing.T, pr *pair) *spend {
			r := planSwap(t, pr, 20_000, true)
			r.commissionPkh = h19 // the arg and the commission output
			return swapSpend(pr, r)
		}},
		{"commissionPkh of 19 bytes, no commission (zero-fee pair)", free, func(t *testing.T, pr *pair) *spend {
			r := planSwap(t, pr, 20_000, true)
			r.commissionPkh = h19 // the arg only: there is no commission output
			return swapSpend(pr, r)
		}},
		{"tokenChangePkh of 21 bytes, with a token change", pr, func(t *testing.T, pr *pair) *spend {
			r := planSwap(t, pr, 20_000, true)
			r.tokenChange, r.tokenChangePkh = 777, h21 // the arg and the change output
			return swapSpend(pr, r)
		}},
		{"next key of 33 zero bytes", pr, func(t *testing.T, pr *pair) *spend { return withNextKey(t, pr, make([]byte, 33)) }},
		{"next key 04 + 32 bytes", pr, func(t *testing.T, pr *pair) *spend {
			return withNextKey(t, pr, append([]byte{0x04}, bytes.Repeat([]byte{0x5a}, 32)...))
		}},
		{"next key 07 + 32 bytes", pr, func(t *testing.T, pr *pair) *spend {
			return withNextKey(t, pr, append([]byte{0x07}, bytes.Repeat([]byte{0x5a}, 32)...))
		}},
		{"tokenChange 2^64 (one past BRC-162's largest amount), with its output", pr, func(t *testing.T, pr *pair) *spend {
			_, s := withChangePush(t, pr, minimalPush(0, 1)) // 00 x8 01
			return s
		}},
		// The output carries the argument's bytes; the contract writes the
		// minimal number, so this is refused by the output compare.
		{"tokenChange 17 pushed as 02 11 00, the output carrying that push", pr, func(t *testing.T, pr *pair) *spend {
			r := planSwap(t, pr, 20_000, true)
			s := swapSpend(pr, r)
			push := []byte{0x02, 0x11, 0x00}
			s.ins[0].args[argTokenChange] = push
			s.outs = append(s.outs, out{1, append(tokenPrefixPush(inAsset(pr, r), push), p2pkhScript(r.tokenChangePkh)...)})
			return s
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := e.sub(t)
			s := c.build(t, c.pr)
			s.label = "BAD swap: " + c.name
			s.trace = true
			e.reject(s, consensusReject, 0)
			e.swap(c.pr, planSwap(t, c.pr, 20_000, true)) // the honest spend of the same pair
		})
	}
	e.close(pr)
	e.close(free)
}

// minimalPush is the 9-byte push of lo + hi*2^64 (hi < 0x80): minimal for
// any amount from 2^63 up, which needs the ninth byte.
func minimalPush(lo uint64, hi byte) []byte {
	b := make([]byte, 9)
	binary.LittleEndian.PutUint64(b, lo)
	b[8] = hi
	return append([]byte{0x09}, b...)
}

// withChangePush is an honest A-in Swap on pr whose tokenChange argument is
// the raw push, with the change output carrying that push.
func withChangePush(t *testing.T, pr *pair, push []byte) (*swapPlan, *spend) {
	r := planSwap(t, pr, 20_000, true)
	s := swapSpend(pr, r)
	s.ins[0].args[argTokenChange] = push
	s.outs = append(s.outs, out{1, append(tokenPrefixPush(inAsset(pr, r), push), p2pkhScript(r.tokenChangePkh)...)})
	return r, s
}

// The token change's bound is BRC-162's: 2^63 and 2^64-1 are amounts like any
// other (pushed minimally, as the node's policy wants), each accepted with its
// change output mined carrying exactly that push.
func TestStrictTokenChangeLargeAmounts(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	for _, c := range []struct {
		name string
		push []byte
	}{
		{"2^63", minimalPush(1<<63, 0)},        // 00 x7 80 00
		{"2^64-1", minimalPush(^uint64(0), 0)}, // ff x8 00
	} {
		t.Run(c.name, func(t *testing.T) {
			r, s := withChangePush(t, pr, c.push)
			s.label = "swap: tokenChange " + c.name
			want := s.outs[len(s.outs)-1].script
			_, got := e.sub(t).swapWithTx(pr, r, s)
			if last := *got.Outputs[len(got.Outputs)-1].LockingScript; !bytes.Equal(last, want) {
				t.Fatalf("the mined change output is %x, want %x", last, want)
			}
		})
	}
	e.close(pr)
}

// withNextKey is an honest A-in Swap on pr naming the raw next key k: the
// argument, and the continuation's state carrying k.
func withNextKey(t *testing.T, pr *pair, k []byte) *spend {
	r := planSwap(t, pr, 20_000, true)
	r.after.validatorRaw = k
	s := swapSpend(pr, r)
	s.ins[0].args[argNextKey] = pushData(k)
	return s
}

// policyNonMinimal is the node's reason for a non-minimally encoded number
// under its standard (policy) flags, MINIMALDATA, when the consensus flags
// alone pass: "non-mandatory", not a consensus failure.
const policyNonMinimal = "non-mandatory-script-verify-flag (Non-minimally encoded script number)"

// A non-minimal tokenChange push whose change output carries the MINIMAL
// encoding (what the contract writes). On consensus rules both inputs pass
// (the interpreter pre-flight), but this node's mempool policy (MINIMALDATA,
// enforced by regtest.sh's acceptnonstdtxn=0) refuses the push before the
// script's re-encoding matters: AN HONEST SPEND REFUSED BY POLICY, kept as
// such. The pair is untouched, and the same Swap with the minimal push is
// accepted, its mined change output byte for byte the minimal form.
func TestStrictTokenChangeNonMinimalPush(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	minimalOut := func(r *swapPlan) []byte {
		m := append(tokenPrefixPush(inAsset(pr, r), []byte{0x01, 0x11}), p2pkhScript(r.tokenChangePkh)...)
		if !bytes.Equal(m, tokenP2pkh(inAsset(pr, r), 17, r.tokenChangePkh)) {
			t.Fatal("the minimal form of 17 is not 01 11")
		}
		return m
	}

	t.Run("pushed as 02 11 00, the output minimal: refused by policy only", func(t *testing.T) {
		r := planSwap(t, pr, 20_000, true)
		s := swapSpend(pr, r)
		s.label = "POLICY-REFUSED honest swap: tokenChange 02 11 00, output 01 11"
		s.ins[0].args[argTokenChange] = []byte{0x02, 0x11, 0x00}
		s.outs = append(s.outs, out{1, minimalOut(r)})
		reason := e.sub(t).reject(s, policyNonMinimal) // no contract input fails on consensus rules
		t.Logf("HONEST SPEND REFUSED BY POLICY (consensus rules pass both inputs): %s", reason)
	})
	t.Run("pushed minimally, the same change: accepted, the output minimal", func(t *testing.T) {
		r := planSwap(t, pr, 20_000, true)
		r.tokenChange = 17
		s := swapSpend(pr, r)
		want := minimalOut(r)
		_, got := e.sub(t).swapWithTx(pr, r, s)
		if last := *got.Outputs[len(got.Outputs)-1].LockingScript; !bytes.Equal(last, want) {
			t.Fatalf("the mined change output is %x, want the minimal %x", last, want)
		}
	})
	e.close(pr)
}

// The next key's type byte is all Swap checks: 02 + any 32 bytes is accepted.
// No one can sign for that key, so the pair can then neither Swap nor
// Rescind; the LP still closes it.
func TestStrictNextKeyTypeByteOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	k := append([]byte{0x02}, bytes.Repeat([]byte{0x5a}, 32)...)
	r := planSwap(t, pr, 20_000, true)
	r.after.validatorRaw = k
	s := swapSpend(pr, r)
	s.label = "swap: next key 02 + 32 arbitrary bytes"
	s.ins[0].args[argNextKey] = pushData(k)
	e.swapWith(pr, r, s)
	if !bytes.Equal(pr.st.validatorPub(), k) {
		t.Fatalf("the pair's validator key is %x, want %x", pr.st.validatorPub(), k)
	}

	t.Run("a Swap signed by the key the harness generated", func(t *testing.T) {
		s := swapSpend(pr, planSwap(t, pr, 20_000, true)) // fee to Hash160(k), signer r.next
		s.label = "BAD swap: after a next key of 02 + 32 bytes"
		s.trace = true
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("a Rescind signed by the last validator key", func(t *testing.T) {
		s := rescindSpend(t, pr, 0)
		s.label = "BAD rescind: after a next key of 02 + 32 bytes"
		s.trace = true
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("the LP's Close", func(t *testing.T) { e.sub(t).close(pr) })
}

// A pair of one token with itself: Swap refused both ways; the validator
// still rescinds and the LP closes.
func TestStrictSameToken(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pp := newParams(t, 30, 5, 10)
	pp.assetB = pp.assetA
	pr := e.deploySDK(pp, newPairState(t))
	for _, aIn := range []bool{true, false} {
		s := swapSpend(pr, planSwap(t, pr, 20_000, aIn)) // every token output carries the one id
		s.label = "BAD swap: AssetIdA == AssetIdB, " + map[bool]string{true: "A-in", false: "B-in"}[aIn]
		s.trace = true
		e.reject(s, consensusReject, 0)
	}
	e.rescind(pr, 0)
	e.close(pr)
}

// A pair deployed with a negative fee rate: Swap refused with the outputs the
// script's arithmetic gives (the negative fee is no output, and amountIn plus
// 119 enters the pool); the LP still closes.
func TestStrictNegativeFeeRate(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, -30, 0, 0), newPairState(t))
	r := planSwap(t, pr, 40_000, true)
	if r.lpFee != -119 || r.net != 40_119 {
		t.Fatalf("the script's arithmetic gives an LP fee of -119 and net 40119, the plan %d and %d", r.lpFee, r.net)
	}
	s := swapSpend(pr, r)
	if len(s.outs) != 3 {
		t.Fatalf("a negative fee and two zero fees: want the pool, the vault and the payout, have %d outputs", len(s.outs))
	}
	s.label = "BAD swap: LpFeeBps -30"
	s.trace = true
	e.reject(s, consensusReject, 0)
	e.close(pr)
}

// TRUST-MODEL CASE: Rúnar does not check the length of _changePKH
// (icellan/runar#213), and the contract cannot reach that argument. Rúnar
// serializes the change output as <amount:8> 19 76 a9 14 <_changePKH> 88 ac,
// so a _changePKH of <20-byte hash> 88 ac <value:8> <varint len> <script
// minus its last two bytes>, for a script ending 88 ac, commits the
// transaction to the change output AND one more output after it. The script
// accepts that; the validator must refuse to sign it (its duty in the
// contract's header). This test asserts what the node does with it.
func TestTrustModel_ChangePKHCarriesAnExtraOutput(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	r := planSwap(t, pr, 20_000, true)
	s := swapSpend(pr, r)
	changeTo, extraTo := pkh(newKey(t)), pkh(newKey(t))
	const changeSats, extraSats = 1_000, 777
	extra := p2pkhScript(extraTo) // ends 88 ac
	payload := append(append([]byte{}, changeTo...), 0x88, 0xac)
	payload = binary.LittleEndian.AppendUint64(payload, extraSats)
	payload = append(payload, byte(len(extra)))
	payload = append(payload, extra[:len(extra)-2]...)
	s.ins[0].changePkh, s.ins[0].changeSats = payload, changeSats
	s.outs = append(s.outs, out{changeSats, p2pkhScript(changeTo)}, out{extraSats, extra})
	s.label = "TRUST-MODEL swap: a 54-byte _changePKH carrying an extra output"
	t.Logf("_changePKH is %d bytes: %x", len(payload), payload)
	_, got := e.swapWithTx(pr, r, s)
	if g := got.Outputs[len(got.Outputs)-1]; g.Satoshis != extraSats || !bytes.Equal(*g.LockingScript, extra) {
		t.Fatalf("the extra output is not on chain as built")
	}
	t.Logf("ACCEPTED by the node and mined: the change output and, after it, an output of %d sats the contract never wrote; only the validator's refusal to sign prevents this", extraSats)
	e.close(pr)
}
