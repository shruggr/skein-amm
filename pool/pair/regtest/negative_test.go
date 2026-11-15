//go:build integration

package regtest

import (
	"bytes"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
)

// Spends the NODE must refuse. Each is an honest spend with one
// defect (fully funded and value-balanced, every coin input signed), so the
// node's reason is a script's, not bad-txns-in-belowout. Before each
// broadcast the go-sdk interpreter must name exactly the contract inputs the
// case expects to fail (reject's failing indices). After each rejection the
// pool and vault UTXOs of every pair involved are checked unspent, and the
// honest spend of that same pair is broadcast and must be accepted: the
// harness is not the cause.

// Swap defects. Each fails the pool (input 0) alone; the vault
// (input 1) has nothing to object to, its pool being input 0.
func TestSwapRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))

	cases := []struct {
		name   string
		aIn    bool
		mutate func(t *testing.T, s *spend, r *swapPlan)
	}{
		{"payout +1 token", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs[2].script = tokenP2pkh(pr.p.assetB, r.out+1, pkh(r.user))
		}},
		{"payout +1 token (B-in)", false, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs[2].script = tokenP2pkh(pr.p.assetA, r.out+1, pkh(r.user))
		}},
		{"vault one token short", true, func(t *testing.T, s *spend, r *swapPlan) {
			a := r.after
			a.b--
			s.outs[1].script = vaultScript(pr.p, pr.vault, a)
		}},
		{"vault replaced by a token-B P2PKH", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs[1].script = tokenP2pkh(pr.p.assetB, r.after.b, pkh(newKey(t)))
		}},
		{"vault output missing", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs = append(s.outs[:1], s.outs[2:]...)
		}},
		{"pool and vault outputs swapped", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs[0], s.outs[1] = s.outs[1], s.outs[0]
		}},
		{"signed by a key that is not the validator's", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.ins[0].signer = newKey(t)
		}},
		{"validator fee output missing", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs = append(s.outs[:4], s.outs[5:]...)
		}},
		{"an extra output after the contract's", true, func(t *testing.T, s *spend, r *swapPlan) {
			s.outs = append(s.outs, out{1_000, p2pkhScript(pkh(newKey(t)))})
		}},
		{"ReserveB in state one more than the vault's prefix", true, func(t *testing.T, s *spend, r *swapPlan) {
			a := r.after
			a.b++
			s.outs[0].script = poolScript(pr.p, pr.code, a)
		}},
		// The continuation carries the 32-byte key in state (one byte
		// shorter). That is not necessarily the script's own continuation
		// (poolScript cuts the code by Len(state)), so this shows the node
		// refuses this spend, not that no continuation could pass.
		{"nextValidatorPubKey of 32 bytes", true, func(t *testing.T, s *spend, r *swapPlan) {
			short := r.next.PubKey().Compressed()[:32]
			s.ins[0].args[0] = pushData(short)
			a := r.after
			a.validatorRaw = short
			s.outs[0].script = poolScript(pr.p, pr.code, a)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := e.sub(t)
			r := planSwap(t, pr, 20_000, c.aIn)
			s := swapSpend(pr, r)
			s.label = "BAD swap: " + c.name
			c.mutate(t, s, r)
			e.reject(s, consensusReject, 0)
			e.swap(pr, planSwap(t, pr, 20_000, c.aIn)) // the honest spend of the same pair
		})
	}
	e.close(pr)
}

// a pool deployed with 5,000 sats; the continuation one
// sat short, that sat routed to Rúnar's change (with a matching
// _changeAmount), so the transaction balances and only the pool's rule
// refuses it.
func TestSwapRejected_PoolSatSkimmed(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	st := newPairState(t)
	st.sats = 5_000
	pr := e.deploySDK(newParams(t, 30, 5, 10), st)
	plan := func() (*swapPlan, *spend) {
		r := planSwap(t, pr, 20_000, true)
		r.satsChangePkh, r.satsChange = pkh(newKey(t)), 1_000
		return r, swapSpend(pr, r)
	}
	_, s := plan()
	s.label = "BAD swap: continuation 1 sat fewer (to Rúnar change)"
	s.outs[0].sats--
	last := len(s.outs) - 1
	s.outs[last].sats++
	s.ins[0].changeSats++
	e.reject(s, consensusReject, 0)
	r, honest := plan()
	e.swapWith(pr, r, honest)
	e.close(pr)
}

// pairingCases are the four ways of parting a pool from its vault, applied to
// an honest spend of pr (pool at 0, vault at 1, coins after): the contract
// inputs each is expected to fail, by index.
func pairingCases(foreign *pair) []struct {
	name    string
	edit    func(s *spend)
	failing []int
} {
	return []struct {
		name    string
		edit    func(s *spend)
		failing []int
	}{
		{"without the vault input (a coin at input 1)", func(s *spend) {
			s.ins = append(s.ins[:1], s.ins[2:]...)
		}, []int{0}},
		{"a foreign vault (another deployed pair's) at input 1", func(s *spend) {
			s.ins[1] = vaultIn(foreign)
		}, []int{0, 1}},
		{"its vault at input 2", func(s *spend) {
			s.ins = append([]*input{s.ins[0], s.ins[2], s.ins[1]}, s.ins[3:]...)
		}, []int{0, 2}},
		{"the pool at input 1 (its vault at 0)", func(s *spend) {
			s.ins[0], s.ins[1] = s.ins[1], s.ins[0]
		}, []int{0, 1}},
	}
}

// each of Swap, Rescind and Close with the pool and its vault
// parted, on its own pair; a second deployed pair supplies the foreign vault.
func TestPairingRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	foreign := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	methods := []struct {
		name   string
		honest func(t *testing.T, pr *pair) *spend
		after  func(e *env, pr *pair)
	}{
		{"Swap", func(t *testing.T, pr *pair) *spend { return swapSpend(pr, planSwap(t, pr, 20_000, true)) },
			func(e *env, pr *pair) { e.swap(pr, planSwap(e.t, pr, 20_000, true)); e.close(pr) }},
		{"Rescind", func(t *testing.T, pr *pair) *spend { return rescindSpend(t, pr, 0) },
			func(e *env, pr *pair) { e.rescind(pr, 0); e.close(pr) }},
		{"Close", func(t *testing.T, pr *pair) *spend { return closeSpend(t, pr) },
			func(e *env, pr *pair) { e.close(pr) }},
	}
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			e := e.sub(t)
			pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
			for _, c := range pairingCases(foreign) {
				t.Run(c.name, func(t *testing.T) {
					s := m.honest(t, pr)
					s.label = "BAD " + m.name + ": " + c.name
					s.watch = []*pair{pr, foreign}
					c.edit(s)
					e.sub(t).reject(s, consensusReject, c.failing...)
				})
			}
			t.Run("the honest "+m.name+" of the same pair", func(t *testing.T) {
				m.after(e.sub(t), pr)
			})
		})
	}
	e.close(foreign)
}

// attempts to take a vault (token B) without its own pool. The victim
// pair V and the attacker's pair X are both deployed here.
func TestVaultTheftRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	x := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	thief := newKey(t)
	steal := out{1, tokenP2pkh(v.p.assetB, v.st.b, pkh(thief))}

	t.Run("the vault spent alone", func(t *testing.T) {
		// Its only sat is the fee; token B burned to a data output. The
		// outputs do not matter to the vault's script, only its inputs.
		s := &spend{
			label: "BAD theft: the vault alone",
			ins:   []*input{vaultIn(v)},
			outs:  []out{{0, []byte{0x00, 0x6a}}},
			watch: []*pair{v},
		}
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("the vault at input 1 after a plain funding input", func(t *testing.T) {
		payer := &coin{lock: p2pkhScript(pkh(thief)), key: thief}
		s := &spend{
			label: "BAD theft: funding input at 0, the vault at 1",
			ins:   []*input{coinIn(payer), vaultIn(v)},
			outs:  []out{steal},
			payer: payer,
			watch: []*pair{v},
		}
		e.sub(t).reject(s, consensusReject, 1)
	})
	t.Run("another pair's pool honestly Closed at input 0, its vault at 1, the victim vault at 2", func(t *testing.T) {
		s := closeSpend(t, x)
		s.label = "BAD theft: X closed + victim vault at 2"
		s.ins = []*input{s.ins[0], s.ins[1], vaultIn(v), s.ins[2]}
		s.outs = append(s.outs, steal)
		s.watch = []*pair{v}
		e.sub(t).reject(s, consensusReject, 2)
	})
	t.Run("another pair's pool Closed at input 0 with the victim vault at 1 in place of its own", func(t *testing.T) {
		s := closeSpend(t, x)
		s.label = "BAD theft: X closed with the victim vault at 1"
		s.ins[1] = vaultIn(v)
		s.outs[1] = steal
		s.watch = []*pair{v, x}
		e.sub(t).reject(s, consensusReject, 0, 1)
	})
	t.Run("the victim's honest Close but the vault's preimage ALL|ANYONECANPAY", func(t *testing.T) {
		s := closeSpend(t, v)
		s.label = "BAD close: vault preimage ALL|ANYONECANPAY|FORKID"
		s.ins[1].flag = sighash.AllForkID | sighash.AnyOneCanPay
		e.sub(t).reject(s, consensusReject, 1)
	})
	t.Run("the attacker's honest Close of its own pair", func(t *testing.T) {
		e.sub(t).close(x)
	})
	t.Run("the victim's honest Close", func(t *testing.T) {
		e.sub(t).close(v)
	})
}

// Close by anyone but the LP; Rescind by the LP; a second Rescind and
// a Swap after Rescind (both by the old validator key).
func TestCloseRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	for name, k := range map[string]func() *ec.PrivateKey{
		"signed by the validator": func() *ec.PrivateKey { return pr.st.validator },
		"signed by a random key":  func() *ec.PrivateKey { return newKey(t) },
	} {
		t.Run(name, func(t *testing.T) {
			s := closeSpend(t, pr)
			s.label = "BAD close: " + name
			s.ins[0].signer = k()
			e.sub(t).reject(s, consensusReject, 0)
		})
	}
	t.Run("the LP's Close of the same pair", func(t *testing.T) {
		e.sub(t).close(pr)
	})
}

func TestRescindRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	t.Run("signed by the LP", func(t *testing.T) {
		s := rescindSpend(t, pr, 0)
		s.label = "BAD rescind: LP-signed"
		s.ins[0].signer = pr.st.lp
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("the honest Rescind of the same pair", func(t *testing.T) {
		e.sub(t).rescind(pr, 0)
	})
	t.Run("a second Rescind (old validator key)", func(t *testing.T) {
		s := rescindSpend(t, pr, 0) // signed by pr.st.validator: the key before Rescind
		s.label = "BAD rescind: second rescind"
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("a Swap after Rescind (old validator key)", func(t *testing.T) {
		// swapSpend signs with the old key and pays the validator fee where the
		// script would: Hash160 of the cleared key. The signature is the only
		// defect.
		s := swapSpend(pr, planSwap(t, pr, 20_000, true))
		s.label = "BAD swap after rescind"
		e.sub(t).reject(s, consensusReject, 0)
	})
	t.Run("the LP's Close of the rescinded pair", func(t *testing.T) {
		e.sub(t).close(pr)
	})
}

// Swap and Rescind take the vault's code as an argument, checked against
// VaultCodeHash. Two wrong codes, each with output 1 built from it and with
// output 1 the real vault. Only the pool input fails.
func TestVaultCodeArgumentRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	codes := []struct {
		name string
		code func() []byte
	}{
		{"the vault's code, last byte flipped", func() []byte {
			c := append([]byte{}, pr.vault...)
			c[len(c)-1] ^= 1
			return c
		}},
		{"a P2PKH to a stranger", func() []byte { return p2pkhScript(pkh(newKey(t))) }},
	}
	// variants applies a wrong code to an honest Swap or Rescind whose output 1
	// would hold b tokens.
	variants := func(name string, honest func() (*spend, int64), done func(e *env)) {
		t.Run(name, func(t *testing.T) {
			for _, c := range codes {
				for _, fromWrong := range []bool{true, false} {
					s, b := honest()
					code := c.code()
					s.ins[0].vaultCode = code
					s.label = "BAD " + name + ": vaultCode " + c.name + ", output 1 the real vault"
					if fromWrong {
						s.outs[1].script = append(tokenPrefix(pr.p.assetB, b), code...)
						s.label = "BAD " + name + ": vaultCode " + c.name + ", output 1 built from it"
					}
					s.trace = true
					e.sub(t).reject(s, consensusReject, 0)
					if name == "Swap" {
						e.sub(t).swap(pr, planSwap(t, pr, 20_000, true)) // the honest spend of the same pair
					}
				}
			}
			done(e.sub(t))
		})
	}
	variants("Swap", func() (*spend, int64) {
		r := planSwap(t, pr, 20_000, true)
		return swapSpend(pr, r), r.after.b
	}, func(*env) {})
	variants("Rescind", func() (*spend, int64) { return rescindSpend(t, pr, 0), pr.st.b }, func(e *env) { e.rescind(pr, 0) })
	e.close(pr)
}

// The validator's (Swap, Rescind) and the LP's (Close) signature must be
// ALL|FORKID. Each case signs over the real digest of its own type, so the
// signature verifies (build checks it) and only requireSighashAll can refuse
// it; the trace shows where the pool input stops.
func TestSignerSighashTypeRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	flags := []struct {
		name string
		flag sighash.Flag
	}{
		{"NONE|FORKID", sighash.NoneForkID},
		{"ALL|ANYONECANPAY|FORKID", sighash.AllForkID | sighash.AnyOneCanPay},
	}
	methods := []struct {
		name   string
		honest func() *spend
		after  func(e *env) // the honest spend once a method's cases are done (Swap: after each)
		each   bool
	}{
		{"Swap", func() *spend { return swapSpend(pr, planSwap(t, pr, 20_000, true)) },
			func(e *env) { e.swap(pr, planSwap(e.t, pr, 20_000, true)) }, true},
		{"Rescind", func() *spend { return rescindSpend(t, pr, 0) }, func(e *env) { e.rescind(pr, 0) }, false},
		{"Close", func() *spend { return closeSpend(t, pr) }, func(e *env) { e.close(pr) }, false},
	}
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			for _, f := range flags {
				s := m.honest()
				s.label = "BAD " + m.name + ": signer's sighash " + f.name
				s.ins[0].sigFlag = f.flag
				s.trace = true
				e.sub(t).reject(s, consensusReject, 0)
				checkSighashAllRefused(t, m.name, s.traces[0], s.ins[0].signer.PubKey().Compressed())
				if m.each {
					m.after(e.sub(t))
				}
			}
			if !m.each {
				m.after(e.sub(t))
			}
		})
	}
}

// checkSighashAllRefused: the pool input stopped at requireSighashAll's
// compare with 0x41 (`... OP_DATA_1(41) OP_EQUALVERIFY`). In Swap and Rescind
// CheckSig runs first, so the signer's own OP_CHECKSIG(VERIFY), against the
// signer's key, must have executed and succeeded before that: the signature
// verifies and only the type rule refuses it. In Close requireSighashAll runs
// first, so the signer's key must not have been reached.
func checkSighashAllRefused(t *testing.T, method string, d *tracer, signerPub []byte) {
	t.Helper()
	if d == nil {
		t.Fatalf("%s: no trace of the pool input", method)
	}
	n := len(d.recent)
	if n < 2 || d.recent[n-1] != "OP_EQUALVERIFY" || d.recent[n-2] != "OP_DATA_1(41)" {
		t.Errorf("%s: the pool input did not stop at the 0x41 compare: %v after %v", method, d.err, d.recent)
	}
	signerChecked := false
	for _, c := range d.checksigs {
		if bytes.Equal(c.pubKey, signerPub) {
			signerChecked = true
			if !c.ok {
				t.Errorf("%s: the signer's %s failed: %v", method, c.op, d.checksigs)
			}
		}
	}
	switch {
	case method != "Close" && !signerChecked:
		t.Errorf("%s: the signer's key was never checked before the refusal: %v", method, d.checksigs)
	case method == "Close" && signerChecked:
		t.Errorf("Close: the LP's key was checked before requireSighashAll: %v", d.checksigs)
	}
	t.Logf("%s: the signer's signature check %s before requireSighashAll refused it", method, map[bool]string{true: "SUCCEEDED", false: "was not reached"}[signerChecked])
}

// The affirmation (AffirmationR, AffirmationS) is carried unchanged: a
// continuation with either field one byte off is refused (the pool fails).
func TestAffirmationCarriedRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	// flip changes one byte of output 0's last 64 bytes: AffirmationR at
	// fromEnd 64..33, AffirmationS at 32..1.
	flip := func(s *spend, fromEnd int) {
		sc := append([]byte{}, s.outs[0].script...)
		sc[len(sc)-fromEnd] ^= 0x01
		s.outs[0].script = sc
	}
	for _, c := range []struct {
		name    string
		fromEnd int
	}{{"AffirmationR, first byte", 64}, {"AffirmationS, last byte", 1}} {
		t.Run("Swap: "+c.name+" off by one bit", func(t *testing.T) {
			s := swapSpend(pr, planSwap(t, pr, 20_000, true))
			s.label = "BAD swap: continuation " + c.name + " changed"
			flip(s, c.fromEnd)
			s.trace = true
			e.sub(t).reject(s, consensusReject, 0)
			e.sub(t).swap(pr, planSwap(t, pr, 20_000, true))
		})
	}
	t.Run("Rescind: AffirmationR, last byte off by one bit", func(t *testing.T) {
		s := rescindSpend(t, pr, 0)
		s.label = "BAD rescind: continuation AffirmationR changed"
		flip(s, 33)
		s.trace = true
		e.sub(t).reject(s, consensusReject, 0)
		e.sub(t).rescind(pr, 0)
	})
	e.close(pr)
}

// A Close carrying a _codePart push first, as Close did while the state had a
// variable-length field. Close takes no such witness now, so the push stays
// on the stack under the method's result. On consensus rules both inputs
// pass (the interpreter pre-flight: post-Genesis, CLEANSTACK is not a
// consensus rule); the node refuses it on policy (CLEANSTACK, enforced at
// mempool entry with acceptnonstdtxn=0). The pair is untouched, and the
// LP's Close without the push is accepted.
func TestCloseWithStrayCodePart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	s := closeSpend(t, pr)
	s.label = "POLICY-REFUSED close: a stray _codePart push"
	s.ins[0].strayCodePart = true
	reason := e.reject(s, "non-mandatory-script-verify-flag (Script did not clean its stack)") // no input fails on consensus rules
	t.Logf("a stray _codePart on Close: consensus rules pass both inputs; the node refuses it on policy: %s", reason)
	e.close(pr)
}
