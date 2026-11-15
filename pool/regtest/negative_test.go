//go:build integration

package regtest

import (
	"testing"

	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
)

// Spends the NODE must refuse. Each is the honest spend with one defect
// (fully funded and value-balanced, every other input signed), so the node's
// reason is the script's, not bad-txns-in-belowout. Before each broadcast the
// interpreter must fail exactly the expected pool input; after each rejection
// the pool UTXO is checked unspent, and the honest spend of that same UTXO is
// broadcast and must be accepted: the harness is not the cause.

func TestSwapRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))

	cases := []struct {
		name   string
		bsvIn  bool
		mutate func(t *testing.T, s *spend, r *swapPlan)
	}{
		{"payout +1 sat (token-in: the pool pays out one more)", false, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs[1].sats++
			s.outs[0].sats--
		}},
		{"continuation -1 sat (sats-in)", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs[0].sats--
		}},
		{"signed by a key that is not the validator's", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.signer = newKey(t)
		}},
		{"validator fee output missing", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs = append(s.outs[:3], s.outs[4:]...)
		}},
		{"an extra output after the contract's", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs = append(s.outs, out{1_000, p2pkhScript(pkh(newKey(t)))})
		}},
		{"LP fee to the LP key instead of LpSatsPkh", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs[2].script = p2pkhScript(pkh(p.st.lp))
		}},
		{"continuation prefix says newTokens+1, state says newTokens", true, func(t *testing.T, s *spend, r *swapPlan) {
			st := r.nextState(p)
			lock := append(tokenPrefix(p.p.assetID, st.tokens+1), p.code...)
			lock = append(lock, 0x6a)
			s.outs[0].script = append(lock, stateBytes(p.p, st)...)
		}},
		// The continuation carries the 32-byte key in state (one byte shorter):
		// this shows the node refuses this spend, not that no continuation
		// could pass.
		{"nextValidatorPubKey of 32 bytes", true, func(t *testing.T, s *spend, r *swapPlan) {
			short := r.next.PubKey().Compressed()[:32]
			s.args[argNextKey] = pushData(short)
			st := r.nextState(p)
			st.validatorRaw = short
			s.outs[0].script = poolScript(p.p, p.code, st)
		}},
		{"continuation with AffirmationR changed by one byte", true, func(t *testing.T, s *spend, r *swapPlan) {
			bad := p.p
			bad.affirmR = append([]byte{}, p.p.affirmR...)
			bad.affirmR[7] ^= 0x01
			s.outs[0].script = poolScript(bad, p.code, r.nextState(p))
		}},
		{"continuation with AffirmationS changed by one byte", false, func(t *testing.T, s *spend, r *swapPlan) {
			bad := p.p
			bad.affirmS = append([]byte{}, p.p.affirmS...)
			bad.affirmS[31] ^= 0x80
			s.outs[0].script = poolScript(bad, p.code, r.nextState(p))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := e.sub(t)
			r := planSwap(t, p, 20_000, c.bsvIn)
			s := swapSpend(p, r)
			s.label = "BAD swap: " + c.name
			c.mutate(t, s, r)
			e.reject(s, consensusReject, 0)
			e.swap(p, planSwap(t, p, 20_000, c.bsvIn)) // the honest spend of the same UTXO
		})
	}
	e.close(p)
}

func TestCloseRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	t.Run("signed by the validator", func(t *testing.T) {
		s := closeSpend(t, p)
		s.label, s.signer = "BAD close: validator-signed", p.st.validator
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("signed by a random key", func(t *testing.T) {
		s := closeSpend(t, p)
		s.label, s.signer = "BAD close: random key", newKey(t)
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("the LP's Close of the same UTXO", func(t *testing.T) {
		e.sub(t).close(p)
	})
}

func TestRescindRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	t.Run("signed by the LP", func(t *testing.T) {
		s := rescindSpend(t, p, 0)
		s.label, s.signer = "BAD rescind: LP-signed", p.st.lp
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("keeping the validator key", func(t *testing.T) {
		s := rescindSpend(t, p, 0)
		s.label = "BAD rescind: key kept"
		s.outs[0].script = poolScript(p.p, p.code, p.st)
		e.sub(t).reject(s, consensusReject, 0)
	})
	rescinded := false
	t.Run("the honest Rescind of the same UTXO", func(t *testing.T) {
		e.sub(t).rescind(p, 0)
		rescinded = true
	})
	oldValidator := p.st.validator
	t.Run("a second Rescind (old validator key)", func(t *testing.T) {
		if !rescinded {
			t.Skip("needs the honest Rescind subtest before it")
		}
		s := rescindSpend(t, p, 0)
		s.label, s.signer = "BAD rescind: second rescind", oldValidator
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("a Swap after Rescind (old validator key)", func(t *testing.T) {
		if !rescinded {
			t.Skip("needs the honest Rescind subtest before it")
		}
		// swapSpend pays the validator fee to Hash160 of the state's key: the
		// cleared one, as the script would.
		s := swapSpend(p, planSwap(t, p, 20_000, true))
		s.label, s.signer = "BAD swap after rescind", oldValidator
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("the LP's Close of the rescinded pool", func(t *testing.T) {
		e.sub(t).close(p)
	})
}

// Each method signed by its rightful key with a valid signature of another
// sighash type: NONE|FORKID and ALL|ANYONECANPAY|FORKID. The signature
// verifies (the node checks it against that type's digest); requireSighashAll
// refuses it.
func TestSighashTypeRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	flags := []struct {
		name string
		flag sighash.Flag
	}{
		{"NONE|FORKID", sighash.NoneForkID},
		{"ALL|ANYONECANPAY|FORKID", sighash.AllForkID | sighash.AnyOneCanPay},
	}
	methods := []struct {
		name   string
		honest func(t *testing.T) *spend
		after  func(e *env)
	}{
		{"Swap", func(t *testing.T) *spend { return swapSpend(p, planSwap(t, p, 20_000, true)) },
			func(e *env) { e.swap(p, planSwap(e.t, p, 20_000, true)) }},
		{"Rescind", func(t *testing.T) *spend { return rescindSpend(t, p, 0) }, func(e *env) { e.rescind(p, 0) }},
		{"Close", func(t *testing.T) *spend { return closeSpend(t, p) }, func(e *env) { e.close(p) }},
	}
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			for _, f := range flags {
				s := m.honest(t)
				s.label = "BAD " + m.name + ": signer's sighash " + f.name
				s.sigFlag = f.flag
				e.sub(t).reject(s, consensusReject, 0)
			}
			m.after(e.sub(t)) // the honest spend of the same UTXO
		})
	}
}

// allPrevouts that is not the transaction's input outpoints: one byte
// changed, and truncated (the last input's 36 bytes dropped). Swap and
// Rescind; each followed by the honest spend.
func TestAllPrevoutsRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	// real is the allPrevouts the honest build of s would carry.
	real := func(t *testing.T, s *spend) []byte {
		b := e.sub(t).build(s, 0)
		var pv []byte
		for _, in := range b.tx.Inputs {
			pv = append(pv, in.SourceTXID.CloneBytes()...)
			pv = append(pv, byte(in.SourceTxOutIndex), byte(in.SourceTxOutIndex>>8), byte(in.SourceTxOutIndex>>16), byte(in.SourceTxOutIndex>>24))
		}
		return pv
	}
	bad := []struct {
		name string
		f    func(pv []byte) []byte
	}{
		{"one byte changed", func(pv []byte) []byte { pv[len(pv)-1] ^= 0x01; return pv }},
		{"truncated (last input dropped)", func(pv []byte) []byte { return pv[:len(pv)-36] }},
	}
	for _, b := range bad {
		t.Run("swap: allPrevouts "+b.name, func(t *testing.T) {
			s := swapSpend(p, planSwap(t, p, 20_000, true))
			e.sub(t).fund(s) // the payer's outpoint must be real for allPrevouts
			s.prevouts = b.f(real(t, s))
			s.label = "BAD swap: allPrevouts " + b.name
			e.sub(t).reject(s, consensusReject, 0)
			e.sub(t).swap(p, planSwap(t, p, 20_000, true))
		})
	}
	for _, b := range bad {
		t.Run("rescind: allPrevouts "+b.name, func(t *testing.T) {
			s := rescindSpend(t, p, 0)
			e.sub(t).fund(s)
			s.prevouts = b.f(real(t, s))
			s.label = "BAD rescind: allPrevouts " + b.name
			e.sub(t).reject(s, consensusReject, 0)
		})
	}
	e.rescind(p, 0)
	e.close(p)
}

// Two pools with byte-identical scripts and sats in one transaction, both
// spent by the same method against one output list (the outputs pool 0's
// spend writes), the second pool's reserve leaving through Rúnar's change:
// the case requireInputZero closes. The interpreter must pass input 0 and
// fail input 1; the node must refuse the transaction. Then each pool is
// spent honestly on its own and closed.
func TestTwinPoolsRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	twins := func(t *testing.T) (*pool, *pool) {
		pp, st := newParams(t, 30, 5, 10), newPoolState(t)
		a := e.sub(t).deploySDK(pp, st)
		b := e.sub(t).deployCode(pp, st, a.code, "deploy twin")
		if string(a.lock) != string(b.lock) || a.st.bsv != b.st.bsv {
			t.Fatal("the twins differ")
		}
		return a, b
	}
	t.Run("both swapping", func(t *testing.T) {
		a, b := twins(t)
		s := swapSpend(a, planSwap(t, a, 20_000, true))
		s.twins = []*pool{b}
		s.changePkh, s.changeSats = pkh(newKey(t)), b.st.bsv
		s.outs = append(s.outs, out{b.st.bsv, p2pkhScript(s.changePkh)})
		s.label = "BAD twin pools: both Swap against one output list"
		e.sub(t).reject(s, consensusReject, 1)
		e.sub(t).swap(a, planSwap(t, a, 20_000, true))
		e.sub(t).swap(b, planSwap(t, b, 20_000, true))
		e.sub(t).close(a)
		e.sub(t).close(b)
	})
	t.Run("both rescinding", func(t *testing.T) {
		a, b := twins(t)
		s := rescindSpend(t, a, 0)
		s.twins = []*pool{b}
		s.changePkh, s.changeSats = pkh(newKey(t)), b.st.bsv
		s.outs = append(s.outs, out{b.st.bsv, p2pkhScript(s.changePkh)})
		s.label = "BAD twin pools: both Rescind against one output list"
		e.sub(t).reject(s, consensusReject, 1)
		e.sub(t).rescind(a, 0)
		e.sub(t).rescind(b, 0)
		e.sub(t).close(a)
		e.sub(t).close(b)
	})
}
