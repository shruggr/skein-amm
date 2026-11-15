//go:build integration

package regtest

import (
	"fmt"
	"testing"
)

// Items 1-6: honest spends the node must accept. Every accepted spend is
// mined, re-read from the chain and checked output by output (see accept),
// and each spend's pool input is the previous spend's continuation.

// The first round trip: an SDK-deployed pool, closed by its LP.
func TestDeployAndClose(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10, 70), newPoolState(t))
	e.close(p)
}

// Item 2: the fee pool {LP 30, validator 5, commission 10} through every
// honest path: sats-in, token-in with token change, a Swap with Rúnar sats
// change, 10 chained swaps alternating direction (the validator key rotating
// on each), Rescind, then the LP's Close sweeping to a P2PKH.
func TestLifecycleFeePool(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10, 70), newPoolState(t))

	t.Run("sats-in swap", func(t *testing.T) {
		e := e.sub(t)
		r := planSwap(t, p, 20_000, true)
		if r.lpFee == 0 || r.validatorFee == 0 || r.commission == 0 {
			t.Fatalf("plan should pay all three fees: %+v", r)
		}
		e.swap(p, r)
	})
	t.Run("token-in swap with a token input and token change", func(t *testing.T) {
		e := e.sub(t)
		r := planSwap(t, p, 50_000, false)
		r.tokenChange = 7_777
		e.swap(p, r)
	})
	t.Run("sats-in swap with Rúnar sats change", func(t *testing.T) {
		e := e.sub(t)
		r := planSwap(t, p, 30_000, true)
		r.satsChangePkh, r.satsChange = pkh(newKey(t)), 12_345
		e.swap(p, r)
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
	t.Run("rescind", func(t *testing.T) {
		e.sub(t).rescind(p, 5_000)
	})
	t.Run("close by the LP, swept to a P2PKH", func(t *testing.T) {
		e.sub(t).close(p)
	})
}

// Item 1, second pool: {0, 0, 0} fees (no fee outputs at all), both
// directions, then Close.
func TestZeroFeePool(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 0, 0, 0, 70), newPoolState(t))
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

// Item 3: Close on a fresh pool, and Close after swaps without Rescind.
func TestCloseFreshPool(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10, 71), newPoolState(t))
	e.close(p)
}

func TestCloseAfterSwapsWithoutRescind(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10, 71), newPoolState(t))
	e.swap(p, planSwap(t, p, 40_000, true))
	e.swap(p, planSwap(t, p, 90_000, false))
	e.close(p)
}

// Item 4: the pool at input index 1 (a funding input at 0): Swap, Rescind
// and Close.
func TestPoolAtInputIndex1(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	p := e.deploySDK(newParams(t, 30, 5, 10, 71), newPoolState(t))
	for _, bsvIn := range []bool{true, false} {
		r := planSwap(t, p, 20_000, bsvIn)
		s := swapSpend(p, r)
		s.poolIndex = 1
		e.swapWith(p, r, s)
	}
	{
		s := rescindSpend(t, p, 0)
		s.poolIndex = 1
		txid, _ := e.accept(s)
		p.txid, p.lock, p.st.rescinded = txid, s.outs[0].script, true
	}
	s := closeSpend(t, p)
	s.poolIndex = 1
	s.feeFromOut = -1
	payKey := newKey(t)
	s.payer = &coin{lock: p2pkhScript(pkh(payKey)), key: payKey}
	s.others = []*coin{s.payer}
	e.accept(s)
}

// Item 5: affirmations of 0, 71 and 72 bytes (the state's only
// variable-length field), each deploy -> Swap -> Rescind -> Close.
func TestAffirmationSizes(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, 71, 72} {
		t.Run(fmt.Sprintf("%d bytes", n), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			pp := newParams(t, 30, 5, 10, n)
			if len(pp.affirmation) != n {
				t.Fatalf("affirmation is %d bytes", len(pp.affirmation))
			}
			p := e.deploySDK(pp, newPoolState(t))
			e.swap(p, planSwap(t, p, 25_000, n%2 == 0))
			e.rescind(p, 0)
			e.close(p)
		})
	}
}
