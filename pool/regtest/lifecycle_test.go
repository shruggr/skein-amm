//go:build integration

package regtest

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

// Honest spends the node must accept. Every accepted spend is mined, re-read
// from the chain and checked output by output (see accept), and each spend's
// pool input is the previous spend's continuation.

// The first round trip: an SDK-deployed pool, closed by its LP.
func TestDeployAndClose(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	e.close(p)
}

// The fee pool {LP 30, validator 5, commission 10} through every honest path:
// sats-in, token-in with token change, a Swap with Rúnar sats change, 10
// chained swaps alternating direction (the validator key rotating on each),
// Rescind, then the LP's Close sweeping to a P2PKH.
func TestLifecycleFeePool(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))

	t.Run("sats-in swap", func(t *testing.T) {
		r := planSwap(t, p, 20_000, true)
		if r.lpFee == 0 || r.validatorFee == 0 || r.commission == 0 {
			t.Fatalf("plan should pay all three fees: %+v", r)
		}
		e.sub(t).swap(p, r)
	})
	t.Run("token-in swap with a token input and token change", func(t *testing.T) {
		r := planSwap(t, p, 50_000, false)
		r.tokenChange = 7_777
		e.sub(t).swap(p, r)
	})
	t.Run("sats-in swap with Rúnar sats change", func(t *testing.T) {
		r := planSwap(t, p, 30_000, true)
		r.satsChangePkh, r.satsChange = pkh(newKey(t)), 12_345
		e.sub(t).swap(p, r)
	})
	t.Run("10 chained swaps alternating direction, validator key rotating", func(t *testing.T) {
		e := e.sub(t)
		for i := 0; i < 10; i++ {
			prevKey := p.st.validator
			r := planSwap(t, p, int64(10_000+i*1_000), i%2 == 0)
			e.swap(p, r)
			if p.st.validator == prevKey || p.st.validator != r.next {
				t.Fatalf("swap %d: the validator key did not rotate", i)
			}
		}
	})
	t.Run("rescind", func(t *testing.T) { e.sub(t).rescind(p, 5_000) })
	t.Run("close by the LP, swept to a P2PKH", func(t *testing.T) { e.sub(t).close(p) })
}

// The zero-fee pool {0, 0, 0}: no fee outputs at all, both directions, then
// Close.
func TestZeroFeePool(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 0, 0, 0), newPoolState(t))
	for _, bsvIn := range []bool{true, false} {
		r := planSwap(t, p, 20_000, bsvIn)
		if r.lpFee != 0 || r.validatorFee != 0 || r.commission != 0 || r.net != 20_000 {
			t.Fatalf("zero rates should mean zero fees: %+v", r)
		}
		s := swapSpend(p, r)
		if len(s.outs) != 2 {
			t.Fatalf("a zero-fee swap should have only the pool and the payout: %d outputs", len(s.outs))
		}
		e.swapWith(p, r, s)
	}
	e.close(p)
}

func TestCloseFreshPool(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.close(e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t)))
}

func TestCloseAfterSwapsWithoutRescind(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	e.swap(p, planSwap(t, p, 40_000, true))
	e.swap(p, planSwap(t, p, 90_000, false))
	e.close(p)
}

// The pool at input 1 (a funding input at 0): Swap and Rescind are refused
// there (requireInputZero), each followed by the same spend at input 0;
// Close, which does not look, is accepted at input 1.
func TestPoolAtInputIndex1(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10), newPoolState(t))
	t.Run("swap at input 1: refused", func(t *testing.T) {
		for _, bsvIn := range []bool{true, false} {
			s := atIndex1(t, swapSpend(p, planSwap(t, p, 20_000, bsvIn)))
			s.label = "BAD swap: pool at input 1, " + map[bool]string{true: "sats-in", false: "token-in"}[bsvIn]
			e.sub(t).reject(s, consensusReject, 1)
			e.sub(t).swap(p, planSwap(t, p, 20_000, bsvIn))
		}
	})
	t.Run("rescind at input 1: refused", func(t *testing.T) {
		s := atIndex1(t, rescindSpend(t, p, 0))
		s.label = "BAD rescind: pool at input 1"
		e.sub(t).reject(s, consensusReject, 1)
		e.sub(t).rescind(p, 0)
	})
	t.Run("close at input 1: accepted", func(t *testing.T) {
		s := atIndex1(t, closeSpend(t, p))
		s.label = "close: pool at input 1"
		e.sub(t).accept(s)
	})
}

// A pool affirmed with a real signature (r and s of an ECDSA signature by the
// validator's first key over sha256 of a message): both fields are carried
// byte for byte through every continuation (Swap both ways, Rescind), and
// still verify at the end.
func TestAffirmationCarried(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	st := newPoolState(t)
	digest := sha256.Sum256([]byte("skein-amm pool affirmation (regtest)"))
	sig, err := st.validator.Sign(digest[:])
	if err != nil {
		t.Fatal(err)
	}
	pp := newParams(t, 30, 5, 10)
	pp.affirmR, pp.affirmS = make([]byte, 32), make([]byte, 32)
	sig.R.FillBytes(pp.affirmR)
	sig.S.FillBytes(pp.affirmS)
	affirmer := st.validator.PubKey()

	carried := func(lock []byte) {
		t.Helper()
		tail := lock[len(lock)-64:]
		if !bytes.Equal(tail[:32], pp.affirmR) || !bytes.Equal(tail[32:], pp.affirmS) {
			t.Fatalf("the pool output's state does not end in AffirmationR || AffirmationS: %x", tail)
		}
	}
	p := e.deploySDK(pp, st)
	carried(p.lock)
	e.swap(p, planSwap(t, p, 25_000, true))
	carried(p.lock)
	e.swap(p, planSwap(t, p, 60_000, false))
	carried(p.lock)
	e.rescind(p, 0)
	carried(p.lock)
	if !sig.Verify(digest[:], affirmer) {
		t.Fatal("the affirmation does not verify")
	}
	e.close(p)
}
