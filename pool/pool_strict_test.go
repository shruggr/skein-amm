package pool

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"
	"testing"

	runar "github.com/icellan/runar/packages/runar-go"
)

// The checks Swap, Close and Rescind make beyond the curve and the outputs:
// address lengths, the next key's length and type, amountIn's and the token
// change's size, the token change's encoding, no negative fee rate, the pool at input 0 (requireInputZero) and the
// signers' sighash type (requireSighashAll); and the reserve asserts the pool
// is written back under. Each case builds the outputs the contract itself
// would write for the bad input, so the refusal is the check's and not a
// hashOutputs mismatch.

// Swap's declared arguments, in ABI order (allPrevouts follows; run pushes it).
const (
	argSig = iota
	argNextKey
	argAmountIn
	argBsvIn
	argUserPkh
	argCommissionPkh
	argTokenChangePkh
	argTokenChange
)

// strictArg is c with argument i replaced and its outputs copied.
func strictArg(c call, i int, push []byte) call {
	c.args = append([][]byte{}, c.args...)
	c.args[i] = push
	c.outs = advCopy(c.outs)
	return c
}

// strictFeeOut is a fee or payout of amount in the input asset (sats when
// bsv), as the contract writes it, to a hash of any length.
func strictFeeOut(bsv bool, amount int64, h []byte) out {
	if bsv {
		return out{amount, p2pkh(h)}
	}
	return out{1, tokenP2pkh(amount, h)}
}

func strictDir(bsvIn bool) string { return map[bool]string{true: "bsv in", false: "tokens in"}[bsvIn] }

func TestStrictAddressLengths(t *testing.T) {
	f := advFix(t, advCfg{30, 5, 10})
	free := advFix(t, advCfg{0, 0, 0})
	p := advPool(t)
	for _, bsvIn := range []bool{true, false} {
		bsvIn := bsvIn
		d := strictDir(bsvIn)
		r := f.expectSwap(p, 20_000, bsvIn)
		base := f.swapCall(p, 20_000, bsvIn, r)
		if len(base.outs) != 5 {
			t.Fatalf("expected pool, payout and three fees: %d outputs", len(base.outs))
		}
		rf := free.expectSwap(p, 20_000, bsvIn)
		freeBase := free.swapCall(p, 20_000, bsvIn, rf)
		withChange := func(c call, h []byte) call {
			c = strictArg(c, argTokenChange, pushNum(777))
			c.args[argTokenChangePkh] = pushData(h)
			c.outs = append(c.outs, out{1, tokenP2pkh(777, h)})
			return c
		}
		t.Run(d+": controls, other 20-byte addresses", func(t *testing.T) {
			h := bytes.Repeat([]byte{0x9c}, 20)
			c := strictArg(base, argUserPkh, pushData(h))
			c.outs[1] = strictFeeOut(!bsvIn, r.out, h)
			mustPass(t, f.run(t, p, c))
			c = strictArg(base, argCommissionPkh, pushData(h))
			c.outs[4] = strictFeeOut(bsvIn, r.commission, h)
			mustPass(t, f.run(t, p, c))
			mustPass(t, f.run(t, p, withChange(base, h)))
			mustPass(t, free.run(t, p, strictArg(freeBase, argCommissionPkh, pushData(h))))
			mustPass(t, f.run(t, p, strictArg(base, argTokenChangePkh, pushData(h))))
		})
		for n := 0; n <= 40; n++ {
			if n == 20 {
				continue
			}
			n := n
			h := bytes.Repeat([]byte{0x9c}, n)
			t.Run(fmt.Sprintf("%s: userPkh of %d bytes", d, n), func(t *testing.T) {
				c := strictArg(base, argUserPkh, pushData(h))
				c.outs[1] = strictFeeOut(!bsvIn, r.out, h)
				mustFail(t, f.run(t, p, c))
			})
			t.Run(fmt.Sprintf("%s: commissionPkh of %d bytes, with its output", d, n), func(t *testing.T) {
				c := strictArg(base, argCommissionPkh, pushData(h))
				c.outs[4] = strictFeeOut(bsvIn, r.commission, h)
				mustFail(t, f.run(t, p, c))
			})
			// The length is checked whether or not the address is used: it is
			// no longer a free byte string in a Swap with no commission.
			t.Run(fmt.Sprintf("%s: commissionPkh of %d bytes, no commission", d, n), func(t *testing.T) {
				mustFail(t, free.run(t, p, strictArg(freeBase, argCommissionPkh, pushData(h))))
			})
			t.Run(fmt.Sprintf("%s: tokenChangePkh of %d bytes, with a token change", d, n), func(t *testing.T) {
				mustFail(t, f.run(t, p, withChange(base, h)))
			})
			t.Run(fmt.Sprintf("%s: tokenChangePkh of %d bytes, no token change", d, n), func(t *testing.T) {
				mustFail(t, f.run(t, p, strictArg(base, argTokenChangePkh, pushData(h))))
			})
		}
	}
}

// strictNextKey is the honest Swap naming the raw next key k: the argument,
// and the continuation's state with k at ValidatorPubKey (k is 33 bytes).
func strictNextKey(f *fixture, p pool, k []byte) call {
	r := f.expectSwap(p, 20_000, true)
	c := strictArg(f.swapCall(p, 20_000, true, r), argNextKey, pushData(k))
	s := append([]byte{}, c.outs[0].script...)
	copy(s[len(s)-stateLen+offVal:], k)
	c.outs[0].script = s
	return c
}

func TestStrictNextKeyType(t *testing.T) {
	f := advFix(t, advCfg{30, 5, 10})
	p := advPool(t)
	body := bytes.Repeat([]byte{0x5a}, 32)
	for b := 0; b < 256; b++ {
		b := byte(b)
		ok := b == 0x02 || b == 0x03
		t.Run(fmt.Sprintf("first byte %02x: accepted %v", b, ok), func(t *testing.T) {
			err := f.run(t, p, strictNextKey(f, p, append([]byte{b}, body...)))
			if ok {
				mustPass(t, err)
			} else {
				mustFail(t, err)
			}
		})
	}
	t.Run("33 zero bytes: a Swap cannot write the rescinded key", func(t *testing.T) {
		mustFail(t, f.run(t, p, strictNextKey(f, p, make([]byte, 33))))
	})
	u := next(p.validator).PubKey().Uncompressed()
	t.Run("an uncompressed key's first 33 bytes", func(t *testing.T) {
		mustFail(t, f.run(t, p, strictNextKey(f, p, u[:33])))
	})
	t.Run("an uncompressed key, all 65 bytes", func(t *testing.T) {
		c, _ := f.advWideKeySwap(p, u)
		mustFail(t, f.run(t, p, c))
	})
	t.Run("the compressed key of a real point (control)", func(t *testing.T) {
		mustPass(t, f.run(t, p, strictNextKey(f, p, key(t, 0x45).PubKey().Compressed())))
	})
	// Every other length is refused by the contract's own check, on both
	// toolchains, whatever the first byte, with the continuation poolScript
	// would build for it (TestDeltaSwapNextValidatorKeyWidth sweeps 0..100;
	// TestTheft_WideNextKeyCannotHandThePoolToTheValidator is what the check
	// closes on the pinned Rúnar).
	for _, w := range []int{0, 1, 2, 32, 34, 64, 65, 66} {
		for _, first := range []byte{0x02, 0x03, 0x04, 0x00} {
			w, first := w, first
			t.Run(fmt.Sprintf("%d bytes, %02x first: refused", w, first), func(t *testing.T) {
				c, _ := f.advWideKeySwap(p, deltaWide(w, first, key(t, 0x5d)))
				mustFail(t, f.run(t, p, c))
			})
		}
	}
}

// strictPush is a push of v: minimal when n is 0, else its magnitude padded
// with zero bytes to n bytes, the sign in the top bit of the last. negZero
// pushes negative zero (v is 0).
func strictPush(v *big.Int, n int, negZero bool) []byte {
	if n == 0 {
		if negZero {
			return []byte{0x01, 0x80}
		}
		return propPushBig(v)
	}
	be := new(big.Int).Abs(v).Bytes()
	le := make([]byte, n)
	for i := range be {
		le[i] = be[len(be)-1-i]
	}
	if v.Sign() < 0 || negZero {
		le[n-1] |= 0x80
	}
	return append([]byte{byte(n)}, le...)
}

func TestStrictTokenChange(t *testing.T) {
	f := advFix(t, advCfg{30, 5, 10})
	p := advPool(t)
	max := propPow2m1(64)
	values := []struct {
		name    string
		v       *big.Int
		negZero bool
	}{
		{"0", propBig(0), false},
		{"5", propBig(5), false},
		{"17", propBig(17), false},
		{"128", propBig(128), false},
		{"2^63-1", propPow2m1(63), false},
		{"2^63", propPow2(63), false},
		{"2^64-1", max, false},
		{"2^64", propPow2(64), false},
		{"2^100", propPow2(100), false},
		{"-1", propBig(-1), false},
		{"-2^63", new(big.Int).Neg(propPow2(63)), false},
		{"negative zero", propBig(0), true},
	}
	changeOut := func(push []byte) out {
		pre := append(append([]byte{0x20}, assetID...), push...)
		return out{1, append(append(pre, 0x6d), p2pkh(f.changePkh)...)}
	}
	for _, bsvIn := range []bool{true, false} {
		bsvIn := bsvIn
		base := f.swapCall(p, 20_000, bsvIn, f.expectSwap(p, 20_000, bsvIn))
		for _, x := range values {
			x := x
			ok := x.v.Sign() >= 0 && x.v.Cmp(max) <= 0
			minLen := len(strictPush(x.v, 0, false)) - 1
			for _, n := range []int{0, 9, 10, 16} {
				if n != 0 && n < minLen {
					continue // no padding that short
				}
				n := n
				push := strictPush(x.v, n, x.negZero)
				enc := "minimal"
				if n != 0 {
					enc = fmt.Sprintf("padded to %d bytes", n)
				}
				t.Run(fmt.Sprintf("%s: %s, %s (%x): accepted %v", strictDir(bsvIn), x.name, enc, push, ok), func(t *testing.T) {
					c := strictArg(base, argTokenChange, push)
					// The contract writes the change output when the value is
					// positive, carrying its minimal push.
					if x.v.Sign() > 0 {
						c.outs = append(c.outs, out{1, append(propTokenPrefix(x.v), p2pkh(f.changePkh)...)})
					}
					err := f.run(t, p, c)
					if ok {
						mustPass(t, err)
					} else {
						mustFail(t, err)
					}
					if ok && x.v.Sign() > 0 && !bytes.Equal(push, propPushBig(x.v)) {
						// Not written as given: the argument's own bytes in the output.
						c.outs[len(c.outs)-1] = changeOut(push)
						mustFail(t, f.run(t, p, c))
					}
				})
			}
		}
	}
	t.Run("a zero change with an output anyway", func(t *testing.T) {
		base := f.swapCall(p, 20_000, true, f.expectSwap(p, 20_000, true))
		for _, push := range [][]byte{{0x00}, {0x01, 0x80}, {0x09, 0, 0, 0, 0, 0, 0, 0, 0, 0}} {
			c := strictArg(base, argTokenChange, push)
			c.outs = append(c.outs, changeOut([]byte{0x00}))
			mustFail(t, f.run(t, p, c))
		}
	})
}

// strictShapedSwap is the Swap of amountIn with the outputs the script's own
// arithmetic gives for f's rates, whatever their sign: each fee
// (amountIn*bps + 9999) / 10000, the division truncating toward zero as
// OP_DIV does (Go's does too); an output only for a positive fee.
func strictShapedSwap(f *fixture, p pool, amountIn int64, bsvIn bool) (call, int64, int64) {
	fee := func(bps int64) int64 { return (amountIn*bps + 9999) / 10000 }
	r := swapResult{lpFee: fee(f.lpFeeBps), validatorFee: fee(f.validatorBps), commission: fee(f.commissionBps), nextValidator: next(p.validator)}
	net := amountIn - r.lpFee - r.validatorFee - r.commission
	if bsvIn {
		if d := p.bsv + net; d != 0 {
			r.out = net * p.tokens / d
		}
		r.newBsv, r.newTokens = p.bsv+net, p.tokens-r.out
	} else {
		if d := p.tokens + net; d != 0 {
			r.out = net * p.bsv / d
		}
		r.newBsv, r.newTokens = p.bsv-r.out, p.tokens+net
	}
	outs := []out{{r.newBsv, f.poolScript(r.pool(p))}, strictFeeOut(!bsvIn, r.out, f.userPkh)}
	for _, x := range []struct {
		a  int64
		to []byte
	}{{r.lpFee, p.lpFeePkh(bsvIn)}, {r.validatorFee, pkh(p.validator)}, {r.commission, f.commissionPkh}} {
		if x.a > 0 {
			outs = append(outs, strictFeeOut(bsvIn, x.a, x.to))
		}
	}
	c := f.swapCall(p, 20_000, bsvIn, f.expectSwap(p, 20_000, bsvIn))
	c = strictArg(c, argAmountIn, pushNum(amountIn))
	c.outs = outs
	return c, net, r.out
}

// Fee rates a deploy should never carry. Without the check a negative rate is
// a negative fee: no fee output, and more than amountIn credited to the pool.
// Checked in Swap alone: such a pool can still be rescinded and closed.
func TestStrictFeeRates(t *testing.T) {
	p := advPool(t)
	for _, bps := range [][3]int64{
		{-1, 0, 0}, {0, -1, 0}, {0, 0, -1}, {-1, -1, -1}, {-30, 5, 10},
		{-10_000, 0, 0}, {0, -10_000, 0}, {0, 0, -1_000_000}, {6000, 6000, -3000},
	} {
		bps := bps
		f := advFix(t, advCfg{bps[0], bps[1], bps[2]})
		for _, bsvIn := range []bool{true, false} {
			bsvIn := bsvIn
			t.Run(fmt.Sprintf("bps %v %s: Swap refused", bps, strictDir(bsvIn)), func(t *testing.T) {
				c, net, paid := strictShapedSwap(f, p, 20_000, bsvIn)
				if net <= 0 || paid <= 0 {
					t.Fatalf("net %d, out %d: the case does not isolate the rate check", net, paid)
				}
				mustFail(t, f.run(t, p, c))
			})
		}
		t.Run(fmt.Sprintf("bps %v: Rescind allowed", bps), func(t *testing.T) {
			mustPass(t, f.run(t, p, f.rescindCall(p)))
		})
		t.Run(fmt.Sprintf("bps %v: Close allowed", bps), func(t *testing.T) {
			mustPass(t, f.run(t, p, f.closeCall(p)))
		})
	}

	// Rates summing to 100% or more, none negative: no check of their own.
	// Each fee rounds up, so the fees sum to at least amountIn and no
	// positive amountIn leaves a net, so no Swap here is ever
	// accepted. A negative amountIn can leave a positive net (each fee is then
	// negative, truncated toward zero): amountIn > 0 refuses it.
	amounts := []int64{1, 2, 3, 7, 100, 9_999, 10_000, 10_001, 123_457, 1_000_000, 1 << 40}
	for _, bps := range [][3]int64{{5000, 5000, 0}, {10_000, 0, 0}, {0, 0, 10_000}, {3334, 3333, 3333}, {1, 0, 9999}, {9999, 9999, 0}, {10_000, 10_000, 10_000}} {
		bps := bps
		f := advFix(t, advCfg{bps[0], bps[1], bps[2]})
		for _, bsvIn := range []bool{true, false} {
			bsvIn := bsvIn
			t.Run(fmt.Sprintf("bps %v %s: no positive amountIn swaps", bps, strictDir(bsvIn)), func(t *testing.T) {
				for _, a := range amounts {
					c, net, _ := strictShapedSwap(f, p, a, bsvIn)
					if net > 0 {
						t.Fatalf("amountIn %d: net %d", a, net)
					}
					if err := f.run(t, p, c); err == nil {
						t.Errorf("amountIn %d ACCEPTED", a)
					}
				}
			})
			t.Run(fmt.Sprintf("bps %v %s: a negative amountIn", bps, strictDir(bsvIn)), func(t *testing.T) {
				for _, a := range []int64{-20_000, -1_000_000} {
					c, net, paid := strictShapedSwap(f, p, a, bsvIn)
					t.Logf("amountIn %d: net %d, out %d", a, net, paid)
					mustFail(t, f.run(t, p, c))
				}
			})
		}
		t.Run(fmt.Sprintf("bps %v: Close allowed", bps), func(t *testing.T) {
			mustPass(t, f.run(t, p, f.closeCall(p)))
		})
	}
	t.Run("bps [10000 10000 0]: a negative amountIn leaves a positive net, refused", func(t *testing.T) {
		// The case amountIn > 0 alone refuses: without it the pool would take
		// net 19998 and pay out along the curve, no one having paid in.
		f := advFix(t, advCfg{10_000, 10_000, 0})
		for _, bsvIn := range []bool{true, false} {
			c, net, paid := strictShapedSwap(f, p, -20_000, bsvIn)
			if net != 19_998 || paid <= 0 {
				t.Fatalf("net %d, out %d", net, paid)
			}
			mustFail(t, f.run(t, p, c))
		}
	})
	t.Run("bps [9999 0 0]: the highest single rate swaps", func(t *testing.T) {
		f := advFix(t, advCfg{9999, 0, 0})
		c, net, paid := strictShapedSwap(f, p, 4_000_000, true)
		if net <= 0 || paid <= 0 {
			t.Fatalf("net %d, out %d", net, paid)
		}
		mustPass(t, f.run(t, p, c))
	})
}

// amountIn is a BRC-162 amount: at most 2^64-1, so no fee output can exceed
// it. In the token-in direction it bounds tokens, in the sats-in direction
// sats (far above any real value). The fee rates are chosen so the rest of
// the swap is valid at the bound: 9999 bps leaves a net that fits the 8-byte
// reserve; three 3333 bps fees each fit a sats output. The outputs are the
// ones the script's arithmetic writes, at the bound and above it.
func TestStrictAmountInBound(t *testing.T) {
	p := pool{bsv: 1_000_000_000_000_000_000, tokens: 1_000_000_000_000_000_000, lp: key(t, 10), validator: key(t, 20)}
	for _, x := range []struct {
		name  string
		f     *fixture
		bsvIn bool
	}{
		{"tokens in, 9999 bps", advFix(t, advCfg{9999, 0, 0}), false},
		{"tokens in, 3333 bps x 3", advFix(t, advCfg{3333, 3333, 3333}), false},
		{"bsv in, 3333 bps x 3", advFix(t, advCfg{3333, 3333, 3333}), true},
	} {
		x := x
		for _, a := range []struct {
			name string
			v    *big.Int
			ok   bool
		}{
			{"2^64-1", propPow2m1(64), true},
			{"2^64", propPow2(64), false},
			{"2^64+1", propAdd(propPow2(64), propBig(1)), false},
			{"2^65", propPow2(65), false},
		} {
			a := a
			t.Run(fmt.Sprintf("%s: amountIn %s: accepted %v", x.name, a.name, a.ok), func(t *testing.T) {
				c, v := propMake(x.f, p, a.v, x.bsvIn, nil)
				if v.net.Sign() <= 0 || v.out.Sign() <= 0 || v.newTokens.Cmp(propMaxInt64) > 0 || v.newBsv.Cmp(propMaxInt64) > 0 {
					t.Fatalf("the case does not isolate the bound: %+v", v)
				}
				err := x.f.run(t, p, c)
				if a.ok {
					mustPass(t, err)
				} else {
					mustFail(t, err)
				}
			})
		}
	}
}

// writePool's asserts: the pool is written back with a positive BSV reserve
// and a positive TokenReserve. Pools deployed with a zero reserve.
func TestStrictReserveAsserts(t *testing.T) {
	f := advFix(t, advCfg{30, 5, 10})
	t.Run("a pool with TokenReserve 0: a tokens-in Swap would pay out the whole BSV reserve", func(t *testing.T) {
		// out = net * bsv / (0 + net) = bsv: the continuation would hold 0 sats.
		p := pool{bsv: 1_000_000, tokens: 0, lp: key(t, 10), validator: key(t, 20)}
		c, net, paid := strictShapedSwap(f, p, 20_000, false)
		if paid != p.bsv || net <= 0 {
			t.Fatalf("net %d, out %d", net, paid)
		}
		mustFail(t, f.run(t, p, c))
	})
	t.Run("a pool with TokenReserve 0: Rescind refused, Close allowed", func(t *testing.T) {
		p := pool{bsv: 1_000_000, tokens: 0, lp: key(t, 10), validator: key(t, 20)}
		// The continuation as the contract writes it: its tokenPrefix pushes
		// a zero amount as Clamp(0, 0, 16) + 80, the byte 0x50.
		rs := f.rescindCall(p)
		cleared := p
		cleared.rescinded = true
		pre := append(append([]byte{0x20}, assetID...), 0x50, 0x6d)
		rs.outs[0].script = append(append(append(pre, f.code...), 0x6a), cleared.state()...)
		mustFail(t, f.run(t, p, rs))
		mustPass(t, f.run(t, p, f.closeCall(p)))
	})
	t.Run("a pool of 0 sats: Rescind refused, Close allowed", func(t *testing.T) {
		p := pool{bsv: 0, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
		mustFail(t, f.run(t, p, f.rescindCall(p)))
		c := f.closeCall(p)
		c.outs = []out{{0, []byte{0x6a}}, {1, tokenP2pkh(p.tokens, pkh(p.lp))}}
		mustPass(t, f.run(t, p, c))
	})
}

// An asset id is Rúnar's fixed 32-byte type: at Rúnar main the SDK refuses to
// build a pool with an id of any other length. The pinned SDK is recorded.
func TestStrictAssetIdWidth(t *testing.T) {
	p := advPool(t)
	build := func(id []byte) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		sa := deltaArt(t).sa
		c := runar.NewRunarContract(&sa, []interface{}{
			p.tokens,
			hex.EncodeToString(p.lp.PubKey().Compressed()),
			hex.EncodeToString(p.validator.PubKey().Compressed()),
			hex.EncodeToString(validatorIdentity.PubKey().Compressed()),
			hex.EncodeToString(nonce),
			hex.EncodeToString(lpSatsPkh),
			hex.EncodeToString(lpTokenPkh),
			hex.EncodeToString(affirmationR),
			hex.EncodeToString(affirmationS),
			hex.EncodeToString(id),
			int64(30), int64(5), int64(10),
		})
		if c.GetLockingScript() == "" {
			return fmt.Errorf("no locking script")
		}
		return nil
	}
	if err := build(assetID); err != nil {
		t.Fatalf("a 32-byte id: %v", err)
	}
	for _, n := range []int{0, 1, 31, 33, 36} {
		deltaCheck(t, fmt.Sprintf("the SDK builds a pool with a %d-byte AssetId", n), build(bytes.Repeat([]byte{0xa5}, n)), deltaFail, deltaPass)
	}
}

// requireInputZero, on the two methods that write a continuation: allPrevouts
// must hash to the preimage's hashPrevouts, and its first outpoint must be
// the pool's own. Each refusal is signed over its transaction's real digest.
func TestStrictInputZero(t *testing.T) {
	f := advFix(t, advCfg{30, 5, 10})
	p := advPool(t)
	for _, m := range []int{methodSwap, methodRescind} {
		m := m
		mn := advMethodNames[m]
		base := advValid(f, p, m, true)
		tx, _, _ := f.advTx(p, base, advOpts{})
		real := advPrevouts(tx) // pool || funding
		pool0, fund := real[:36], real[36:]
		other := append(bytes.Repeat([]byte{0x44}, 32), 2, 0, 0, 0)
		flip := func(b []byte, i int) []byte { return deltaFlip(b, i) }
		cases := []struct {
			name string
			c    call
			o    advOpts
			ok   bool
		}{
			{"control: the real outpoints", base, advOpts{}, true},
			{"the pool at input 1, allPrevouts the real ones (pool second)", func() call { c := base; c.poolIndex = 1; return c }(), advOpts{}, false},
			{"the pool at input 1, allPrevouts naming it first (not the transaction's)", func() call { c := base; c.poolIndex = 1; return c }(), advOpts{prevouts: append(append([]byte{}, pool0...), fund...)}, false},
			{"the pool at input 3 (two inputs before it)", base, advOpts{extraBefore: 2}, false},
			{"the pool at input 0, two more inputs after the funding", base, advOpts{extraIns: 2}, true},
			{"the pool's outpoint first, the funding outpoint wrong", base, advOpts{prevouts: append(append([]byte{}, pool0...), flip(fund, 0)...)}, false},
			{"the pool's outpoint first, then another outpoint", base, advOpts{prevouts: append(append([]byte{}, pool0...), other...)}, false},
			{"the outpoints in the other order", base, advOpts{prevouts: append(append([]byte{}, fund...), pool0...)}, false},
			{"the pool's outpoint alone", base, advOpts{prevouts: pool0}, false},
			{"35 bytes", base, advOpts{prevouts: real[:35]}, false},
			{"one byte short", base, advOpts{prevouts: real[:len(real)-1]}, false},
			{"one byte more", base, advOpts{prevouts: append(append([]byte{}, real...), 0x00)}, false},
			{"a third outpoint more", base, advOpts{prevouts: append(append([]byte{}, real...), other...)}, false},
			{"empty", base, advOpts{prevouts: []byte{}}, false},
			{"the pool's txid with a flipped byte", base, advOpts{prevouts: flip(real, 5)}, false},
			{"the pool's output index changed", base, advOpts{prevouts: flip(real, 32)}, false},
		}
		for _, tc := range cases {
			tc := tc
			t.Run(mn+": "+tc.name, func(t *testing.T) {
				err := f.advRun(t, p, tc.c, tc.o)
				if tc.ok {
					mustPass(t, err)
				} else {
					mustFail(t, err)
				}
			})
		}
		t.Run(mn+": the pool at input 0 of three, allPrevouts listing two", func(t *testing.T) {
			mustFail(t, f.advRun(t, p, base, advOpts{extraIns: 1, prevouts: real}))
		})
	}
	t.Run("Close: no allPrevouts, any input", func(t *testing.T) {
		for _, o := range []advOpts{{}, {extraBefore: 1}, {extraBefore: 3, extraIns: 2}} {
			mustPass(t, f.advRun(t, p, f.closeCall(p), o))
		}
		c := f.closeCall(p)
		c.poolIndex = 1
		mustPass(t, f.advRun(t, p, c, advOpts{}))
	})
}

// requireSighashAll on each method: the right signer's valid signature of any
// type but ALL|FORKID is refused (the wider matrix and the bypass attempts
// are TestTheft_SignatureSighashType and TestTheft_SignatureSighashTypeBypass).
func TestStrictSighashAll(t *testing.T) {
	f := advFix(t, advCfg{30, 5, 10})
	p := advPool(t)
	for m := 0; m < 3; m++ {
		m := m
		base := advValid(f, p, m, true)
		t.Run(advMethodNames[m]+": ALL|FORKID", func(t *testing.T) { mustPass(t, f.advRun(t, p, base, advOpts{})) })
		for _, fl := range advSigFlags {
			fl := fl
			t.Run(advMethodNames[m]+": "+fl.name, func(t *testing.T) {
				mustFail(t, f.advRun(t, p, base, advOpts{sigFlag: fl.flag}))
			})
		}
	}
}

// TestTrustModel_ChangePkhIsNotLengthChecked: Rúnar does not check the length
// of _changePKH (icellan/runar#213), the change output's address. It writes
// the change as amount || 19 76a914 || _changePKH || 88ac, so a _changePKH of
// <20-byte hash> 88ac <8-byte value> <varint> <a script minus its final 88ac>
// serialises as the change output and one more output, any script that ends
// 88ac (a P2PKH, a token output). The contract cannot reach the argument:
// the validator must refuse it before signing a Swap or a Rescind. Shorter
// and other lengths are not a valid output list and are refused.
func TestTrustModel_ChangePkhIsNotLengthChecked(t *testing.T) {
	f := advFix(t, advCfg{30, 5, 10})
	p := advPool(t)
	h := pkh(key(t, 0x70))
	thief := pkh(key(t, 0x66))
	const sats = 4_321
	hidden := func(value int64, script []byte) []byte {
		b := append(append([]byte{}, h...), 0x88, 0xac)
		v := make([]byte, 8)
		for i := 0; i < 8; i++ {
			v[i] = byte(value >> (8 * i))
		}
		b = append(append(b, v...), byte(len(script)))
		return append(b, script[:len(script)-2]...)
	}
	withChange := func(c call, chPkh []byte, outs ...out) (call, advOpts) {
		c.outs = append(advCopy(c.outs), outs...)
		c.change = &struct {
			pkh  []byte
			sats int64
		}{h, sats}
		return c, advOpts{chPkh: pushData(chPkh)}
	}
	for _, m := range []int{methodSwap, methodRescind} {
		m := m
		mn := advMethodNames[m]
		base := advValid(f, p, m, true)
		if m == methodRescind {
			base.outs, base.change = base.outs[:1], nil
		}
		t.Run(mn+": control, a 20-byte change address", func(t *testing.T) {
			c, o := withChange(base, h, out{sats, p2pkh(h)})
			mustPass(t, f.advRun(t, p, c, o))
		})
		for _, x := range []struct {
			name string
			o    out
		}{
			{"a P2PKH of 1,000,000 sats to a third party", out{1_000_000, p2pkh(thief)}},
			{"a token output of 5,000,000 tokens to a third party", out{1, tokenP2pkh(5_000_000, thief)}},
		} {
			x := x
			t.Run(mn+": an extra output behind the change, "+x.name+": accepted", func(t *testing.T) {
				c, o := withChange(base, hidden(x.o.sats, x.o.script), out{sats, p2pkh(h)}, x.o)
				mustPass(t, f.advRun(t, p, c, o))
			})
		}
		for _, n := range []int{19, 21} {
			n := n
			t.Run(fmt.Sprintf("%s: a %d-byte change address, its output as written: refused", mn, n), func(t *testing.T) {
				hn := bytes.Repeat([]byte{0x70}, n)
				c, o := withChange(base, hn, out{sats, p2pkh(hn)})
				mustFail(t, f.advRun(t, p, c, o))
			})
		}
	}
}
