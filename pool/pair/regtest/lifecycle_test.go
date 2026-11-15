//go:build integration

package regtest

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"testing"
)

// Honest spends the node must accept. Every accepted spend is
// mined, re-read from the chain and checked output by output (see accept),
// each spend's pool and vault inputs are the previous spend's outputs 0 and
// 1, and the value flow is checked (checkPairCarried, checkPayerPaysMiner).

// The first round trip: an SDK-deployed pair, closed by its LP.
func TestDeployAndClose(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	e.close(pr)
}

// The fee pair {LP 30, validator 5, commission 10} through every
// honest path: A-in, B-in, a Swap with token change and Rúnar sats change,
// 10 chained swaps alternating direction (the validator key rotating on
// each), Rescind, then the LP's Close sweeping both tokens.
func TestLifecycleFeePair(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))

	t.Run("A-in swap", func(t *testing.T) {
		r := planSwap(t, pr, 20_000, true)
		if r.lpFee == 0 || r.valFee == 0 || r.com == 0 {
			t.Fatalf("plan should pay all three fees: %+v", r)
		}
		e.sub(t).swap(pr, r)
	})
	t.Run("B-in swap", func(t *testing.T) {
		e.sub(t).swap(pr, planSwap(t, pr, 50_000, false))
	})
	t.Run("swap with token change and Rúnar sats change", func(t *testing.T) {
		r := planSwap(t, pr, 30_000, true)
		r.tokenChange = 7_777
		r.satsChangePkh, r.satsChange = pkh(newKey(t)), 12_345
		s := swapSpend(pr, r)
		if n := len(s.outs); n != 8 {
			t.Fatalf("pool, vault, payout, 3 fees, token change, sats change: want 8 outputs, have %d", n)
		}
		e.sub(t).swapWith(pr, r, s)
	})
	t.Run("10 chained swaps alternating direction, validator key rotating", func(t *testing.T) {
		e := e.sub(t)
		for i := 0; i < 10; i++ {
			prevKey, prevTx := pr.st.validator, pr.txid
			r := planSwap(t, pr, int64(10_000+i*1_000), i%2 == 0)
			s := swapSpend(pr, r)
			s.label = fmt.Sprintf("chained swap %d %s", i, s.label)
			e.swapWith(pr, r, s)
			if pr.st.validator == prevKey || pr.st.validator != r.next {
				t.Fatalf("swap %d: the validator key did not rotate", i)
			}
			if pr.txid == prevTx {
				t.Fatalf("swap %d: the pair did not move", i)
			}
		}
	})
	t.Run("rescind (with Rúnar change)", func(t *testing.T) {
		e.sub(t).rescind(pr, 5_000)
	})
	t.Run("close by the LP, both tokens swept to token-prefixed P2PKH", func(t *testing.T) {
		e.sub(t).close(pr)
	})
}

// {0, 0, 0} fees (no fee outputs at all), both
// directions, then Close.
func TestZeroFeePair(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 0, 0, 0), newPairState(t))
	for _, aIn := range []bool{true, false} {
		r := planSwap(t, pr, 20_000, aIn)
		if r.lpFee != 0 || r.valFee != 0 || r.com != 0 || r.net != 20_000 {
			t.Fatalf("zero rates should mean zero fees: %+v", r)
		}
		s := swapSpend(pr, r)
		if len(s.outs) != 3 {
			t.Fatalf("a zero-fee swap should have only the pool, the vault and the payout: %d outputs", len(s.outs))
		}
		e.swapWith(pr, r, s)
	}
	e.close(pr)
}

// Close on a fresh pair, and Close after swaps without Rescind.
func TestCloseFreshPair(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	e.close(pr)
}

func TestCloseAfterSwapsWithoutRescind(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pr := e.deploySDK(newParams(t, 30, 5, 10), newPairState(t))
	e.swap(pr, planSwap(t, pr, 40_000, true))
	e.swap(pr, planSwap(t, pr, 90_000, false))
	e.close(pr)
}

// The affirmation is a real signature's r and s (the validator identity's
// signature over sha256(assetIdA || assetIdB || nonce), a stand-in for what
// the validator signs), 32 bytes each, deploy -> Swap -> Rescind -> Close.
// Every continuation, as mined, ends with exactly those 64 bytes.
func TestRealAffirmation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	pp := newParams(t, 30, 5, 10)
	msg := sha256.Sum256(append(append(append([]byte{}, pp.assetA...), pp.assetB...), pp.nonce...))
	sig, err := pp.identity.Sign(msg[:])
	if err != nil {
		t.Fatal(err)
	}
	if !sig.Verify(msg[:], pp.identity.PubKey()) {
		t.Fatal("the affirmation does not verify")
	}
	pp.affR, pp.affS = sig.R.FillBytes(make([]byte, 32)), sig.S.FillBytes(make([]byte, 32))
	want := append(append([]byte{}, pp.affR...), pp.affS...)
	t.Logf("affirmation r %x s %x", pp.affR, pp.affS)
	pr := e.deploySDK(pp, newPairState(t))
	carried := func(step string) {
		tx := fetchTx(t, pr.txid)
		if got := *tx.Outputs[0].LockingScript; !bytes.HasSuffix(got, want) {
			t.Fatalf("%s: the mined pool script does not end with the affirmation's r || s: ...%x", step, got[max(0, len(got)-64):])
		}
	}
	carried("deploy")
	e.swap(pr, planSwap(t, pr, 25_000, true))
	carried("swap")
	e.rescind(pr, 0)
	carried("rescind")
	e.close(pr)
}

// a pool deployed with more than 1 sat keeps exactly that through
// Swaps both ways (one with Rúnar change) and Rescind; Close sweeps it.
func TestPairWithMoreSats(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	st := newPairState(t)
	st.sats = 5_000
	pr := e.deploySDK(newParams(t, 30, 5, 10), st)
	e.swap(pr, planSwap(t, pr, 20_000, true))
	r := planSwap(t, pr, 20_000, false)
	r.satsChangePkh, r.satsChange = pkh(newKey(t)), 1_000
	e.swap(pr, r)
	e.rescind(pr, 0)
	if pr.st.sats != 5_000 {
		t.Fatalf("the pool's sats moved: %d", pr.st.sats)
	}
	e.close(pr)
}
