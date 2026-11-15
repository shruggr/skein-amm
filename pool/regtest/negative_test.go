//go:build integration

package regtest

import (
	"testing"

	crypto "github.com/bsv-blockchain/go-sdk/primitives/hash"
)

// Items 7-9: spends the NODE must refuse. Each is the honest spend with one
// defect (fully funded and value-balanced, every other input signed), so the
// node's reason is the script's, not bad-txns-in-belowout. After each
// rejection the pool UTXO is checked unspent, and the honest spend of that
// same UTXO is broadcast and must be accepted: the harness is not the cause.

// consensusReject is the node's prefix for a script failure under the
// consensus (mandatory) flags. A "non-mandatory-script-verify-flag" reason
// would be a policy-only refusal and fails the assertion.
const consensusReject = "mandatory-script-verify-flag-failed"

func TestSwapRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10, 71), newPoolState(t))

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
		// The continuation carries the 32-byte key in state (one byte
		// shorter). That is not necessarily the script's own continuation
		// (poolScript cuts the code by Len(state)), so this shows the node
		// refuses this spend, not that no continuation could pass.
		{"nextValidatorPubKey of 32 bytes", true, func(t *testing.T, s *spend, r *swapPlan) {
			short := r.next.PubKey().Compressed()[:32]
			s.args[1] = pushData(short)
			st := r.nextState(p)
			st.validatorRaw = short
			s.outs[0].script = poolScript(p.p, p.code, st)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := e.sub(t)
			r := planSwap(t, p, 20_000, c.bsvIn)
			s := swapSpend(p, r)
			s.label = "BAD swap: " + c.name
			c.mutate(t, s, r)
			e.reject(s, consensusReject)
			e.swap(p, planSwap(t, p, 20_000, c.bsvIn)) // the honest spend of the same UTXO
		})
	}
	e.close(p)
}

func TestCloseRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10, 71), newPoolState(t))
	t.Run("signed by the validator", func(t *testing.T) {
		s := closeSpend(t, p)
		s.label, s.signer = "BAD close: validator-signed", p.st.validator
		e.sub(t).reject(s, consensusReject)
	})
	t.Run("signed by a random key", func(t *testing.T) {
		s := closeSpend(t, p)
		s.label, s.signer = "BAD close: random key", newKey(t)
		e.sub(t).reject(s, consensusReject)
	})
	t.Run("the LP's Close of the same UTXO", func(t *testing.T) {
		e.sub(t).close(p)
	})
}

func TestRescindRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10, 71), newPoolState(t))
	t.Run("signed by the LP", func(t *testing.T) {
		s := rescindSpend(t, p, 0)
		s.label, s.signer = "BAD rescind: LP-signed", p.st.lp
		e.sub(t).reject(s, consensusReject)
	})
	t.Run("keeping the validator key", func(t *testing.T) {
		s := rescindSpend(t, p, 0)
		s.label = "BAD rescind: key kept"
		s.outs[0].script = poolScript(p.p, p.code, p.st)
		e.sub(t).reject(s, consensusReject)
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
		e.sub(t).reject(s, consensusReject)
	})
	t.Run("a Swap after Rescind (old validator key)", func(t *testing.T) {
		if !rescinded {
			t.Skip("needs the honest Rescind subtest before it")
		}
		r := planSwap(t, p, 20_000, true)
		s := swapSpend(p, r)
		s.label, s.signer = "BAD swap after rescind", oldValidator
		// The validator fee as the script would pay it: Hash160 of the cleared key.
		s.outs[3].script = p2pkhScript(crypto.Hash160(make([]byte, 33)))
		e.sub(t).reject(s, consensusReject)
	})
	t.Run("the LP's Close of the rescinded pool", func(t *testing.T) {
		e.sub(t).close(p)
	})
}
