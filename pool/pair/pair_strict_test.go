package pair

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	runar "github.com/icellan/runar/packages/runar-go"
)

// The checks Swap makes beyond the curve and the outputs: address lengths,
// the next key's type, the token change's size and encoding, and what a deploy
// must have got right (two different tokens, no negative fee rate). Each case builds
// the outputs the contract itself would write for the bad input, so the
// refusal is the check's and not a hashOutputs mismatch.

// rawP2pkh is the script the contract writes for an address of any length.
func rawP2pkh(h []byte) []byte {
	return append(append([]byte{0x76, 0xa9, 0x14}, h...), 0x88, 0xac)
}

// withArg is s with one argument (and its outputs) replaced.
func withArg(s spend, i int, push []byte) spend {
	c := s
	c.outs = clone(s.outs)
	c.args = append([][]byte{}, s.args...)
	c.args[i] = push
	return c
}

const (
	argNextKey = iota
	argAmountIn
	argAIn
	argUserPkh
	argCommissionPkh
	argTokenChangePkh
	argTokenChange
)

func TestStrictAddressLengths(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	for _, aIn := range []bool{true, false} {
		s, r := f.swap(p, 40_000, aIn)
		inAsset, outAsset := assetA, assetB
		if !aIn {
			inAsset, outAsset = assetB, assetA
		}
		mustPass(t, f.run(t, s))
		for _, n := range []int{0, 1, 19, 21, 32} {
			h := bytes.Repeat([]byte{0x9c}, n)
			t.Run(fmt.Sprintf("aIn=%v: userPkh of %d bytes", aIn, n), func(t *testing.T) {
				c := withArg(s, argUserPkh, pushData(h))
				c.outs[2].script = append(tokenPrefix(outAsset, r.out), rawP2pkh(h)...)
				mustFailPool(t, f.run(t, c))
			})
			t.Run(fmt.Sprintf("aIn=%v: commissionPkh of %d bytes", aIn, n), func(t *testing.T) {
				c := withArg(s, argCommissionPkh, pushData(h))
				c.outs[5].script = append(tokenPrefix(inAsset, r.com), rawP2pkh(h)...)
				mustFailPool(t, f.run(t, c))
			})
			t.Run(fmt.Sprintf("aIn=%v: tokenChangePkh of %d bytes, with a token change", aIn, n), func(t *testing.T) {
				c := withArg(s, argTokenChangePkh, pushData(h))
				c.args[argTokenChange] = pushNum(777)
				c.outs = append(c.outs, out{1, append(tokenPrefix(inAsset, 777), rawP2pkh(h)...)})
				mustFailPool(t, f.run(t, c))
			})
			// The length is checked whether or not the address is used: it is
			// no longer a free byte string in a Swap with no token change.
			t.Run(fmt.Sprintf("aIn=%v: tokenChangePkh of %d bytes, no token change", aIn, n), func(t *testing.T) {
				mustFailPool(t, f.run(t, withArg(s, argTokenChangePkh, pushData(h))))
			})
		}
		t.Run(fmt.Sprintf("aIn=%v: control, a token change to a 20-byte address", aIn), func(t *testing.T) {
			c := withArg(s, argTokenChange, pushNum(777))
			c.outs = append(c.outs, out{1, tokenP2pkh(inAsset, 777, changePkh)})
			mustPass(t, f.run(t, c))
		})
	}

	// With no commission there is no commission output; the address's length
	// is checked all the same.
	free := newFixture(t, 0, 0, 0)
	s, _ := free.swap(p, 40_000, true)
	mustPass(t, free.run(t, s))
	for _, n := range []int{0, 19, 21} {
		t.Run(fmt.Sprintf("no commission: commissionPkh of %d bytes", n), func(t *testing.T) {
			mustFailPool(t, free.run(t, withArg(s, argCommissionPkh, pushData(bytes.Repeat([]byte{0x9c}, n)))))
		})
	}
}

// withNextKey is the honest Swap s naming the raw next key k: the argument,
// and the continuation's state with k in place of the honest next key.
func withNextKey(s spend, honest, k []byte) spend {
	c := withArg(s, argNextKey, pushData(k))
	if n := bytes.Count(c.outs[0].script, honest); n != 1 {
		panic(fmt.Sprintf("the honest next key occurs %d times in the continuation", n))
	}
	c.outs[0].script = bytes.Replace(c.outs[0].script, honest, k, 1)
	return c
}

func TestStrictNextKeyType(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	s, r := f.swap(p, 40_000, true)
	honest := r.after.validator.PubKey().Compressed()
	mustPass(t, f.run(t, s))

	body := bytes.Repeat([]byte{0x5a}, 32)
	for _, first := range []byte{0x00, 0x01, 0x04, 0x05, 0x06, 0x07, 0x12, 0x82, 0xff} {
		t.Run(fmt.Sprintf("first byte %02x", first), func(t *testing.T) {
			mustFailPool(t, f.run(t, withNextKey(s, honest, append([]byte{first}, body...))))
		})
	}
	t.Run("33 zero bytes: a Swap cannot write the rescinded key", func(t *testing.T) {
		mustFailPool(t, f.run(t, withNextKey(s, honest, make([]byte, 33))))
	})
	t.Run("an uncompressed key's first 33 bytes", func(t *testing.T) {
		u := r.after.validator.PubKey().Uncompressed()
		mustFailPool(t, f.run(t, withNextKey(s, honest, u[:33])))
	})
	// The type byte is all the script checks: 02 or 03 and any 32 bytes
	// pass, a point on the curve or not. The key's derivation stays the
	// validator's and the lookup's.
	for _, first := range []byte{0x02, 0x03} {
		t.Run(fmt.Sprintf("first byte %02x and any 32 bytes: accepted", first), func(t *testing.T) {
			mustPass(t, f.run(t, withNextKey(s, honest, append([]byte{first}, body...))))
		})
	}
}

func TestStrictTokenChange(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	s, _ := f.swap(p, 40_000, true)
	mustPass(t, f.run(t, s))

	// prefixOf is token A's prefix with a raw amount push.
	prefixOf := func(push []byte) []byte {
		pre := append([]byte{0x20}, assetA...)
		return append(append(pre, push...), 0x6d)
	}
	changeOut := func(push []byte) out { return out{1, append(prefixOf(push), p2pkh(changePkh)...)} }
	le := func(v uint64, n int) []byte {
		b := make([]byte, n)
		for i := 0; i < 8 && i < n; i++ {
			b[i] = byte(v >> (8 * i))
		}
		return b
	}

	// 17 pushed as two bytes: the change output carries the minimal number,
	// not the argument's bytes.
	t.Run("a non-minimal push of 17: written minimally", func(t *testing.T) {
		c := withArg(s, argTokenChange, []byte{0x02, 0x11, 0x00})
		c.outs = append(c.outs, changeOut([]byte{0x01, 0x11}))
		mustPass(t, f.run(t, c))
	})
	t.Run("a non-minimal push of 17: not written as given", func(t *testing.T) {
		c := withArg(s, argTokenChange, []byte{0x02, 0x11, 0x00})
		c.outs = append(c.outs, changeOut([]byte{0x02, 0x11, 0x00}))
		mustFailPool(t, f.run(t, c))
	})
	t.Run("a padded push of 5: written as OP_5", func(t *testing.T) {
		c := withArg(s, argTokenChange, []byte{0x03, 0x05, 0x00, 0x00})
		c.outs = append(c.outs, changeOut([]byte{0x55}))
		mustPass(t, f.run(t, c))
	})
	t.Run("a padded push of 0: no change output", func(t *testing.T) {
		mustPass(t, f.run(t, withArg(s, argTokenChange, []byte{0x02, 0x00, 0x00})))
	})
	t.Run("negative zero: no change output", func(t *testing.T) {
		mustPass(t, f.run(t, withArg(s, argTokenChange, []byte{0x01, 0x80})))
	})

	// BRC-162 caps an output's amount at 2^64-1.
	for name, push := range map[string][]byte{
		"2^63-1": append([]byte{0x08}, le(1<<63-1, 8)...),
		"2^63":   append([]byte{0x09}, le(1<<63, 9)...), // nine bytes: the sign bit clear
		"2^64-1": append([]byte{0x09}, le(1<<64-1, 9)...),
	} {
		t.Run(name+": a token amount", func(t *testing.T) {
			c := withArg(s, argTokenChange, push)
			c.outs = append(c.outs, changeOut(push))
			mustPass(t, f.run(t, c))
		})
	}
	over := append([]byte{0x09}, append(make([]byte, 8), 0x01)...) // 2^64
	t.Run("2^64: refused", func(t *testing.T) {
		c := withArg(s, argTokenChange, over)
		c.outs = append(c.outs, changeOut(over))
		mustFailPool(t, f.run(t, c))
	})
	huge := append([]byte{0x11}, append(make([]byte, 16), 0x01)...) // 2^128
	t.Run("2^128: refused", func(t *testing.T) {
		c := withArg(s, argTokenChange, huge)
		c.outs = append(c.outs, changeOut(huge))
		mustFailPool(t, f.run(t, c))
	})
	t.Run("-1: refused", func(t *testing.T) {
		mustFailPool(t, f.run(t, withArg(s, argTokenChange, []byte{0x4f})))
	})
	t.Run("-2^63: refused", func(t *testing.T) {
		neg := append([]byte{0x09}, append(le(1<<63, 8), 0x80)...)
		mustFailPool(t, f.run(t, withArg(s, argTokenChange, neg)))
	})
}

// A pair of one token with itself: Swap refuses it; the validator can still
// rescind it and the LP close it.
func TestStrictSameToken(t *testing.T) {
	f := newFixtureIDs(t, assetA, assetA, 30, 5, 10)
	p := fresh()
	for _, aIn := range []bool{true, false} {
		t.Run(fmt.Sprintf("Swap aIn=%v: refused", aIn), func(t *testing.T) {
			s, r := f.swap(p, 40_000, aIn)
			// Both tokens are assetA here: every token output carries it.
			s.outs[2].script = tokenP2pkh(assetA, r.out, userPkh)
			s.outs[3].script = tokenP2pkh(assetA, r.lpFee, map[bool][]byte{true: lpFeeAPkh, false: lpFeeBPkh}[aIn])
			s.outs[4].script = tokenP2pkh(assetA, r.valFee, pkh(p.validator))
			s.outs[5].script = tokenP2pkh(assetA, r.com, commissionPkh)
			mustFailPool(t, f.run(t, s))
		})
	}
	t.Run("Rescind: allowed", func(t *testing.T) {
		s, _ := f.rescind(p)
		mustPass(t, f.run(t, s))
	})
	t.Run("Close: allowed", func(t *testing.T) {
		s := f.close(p)
		s.outs = []out{{1, tokenP2pkh(assetA, p.a+p.b, pkh(p.lp))}}
		mustPass(t, f.run(t, s))
	})
}

// scriptFee is the contract's fee for a rate: (amountIn*bps + 9999) / 10000,
// the division truncating toward zero as OP_DIV does (Go's does too).
func scriptFee(amountIn, bps int64) int64 { return (amountIn*bps + 9999) / 10000 }

// Fee rates a deploy should never carry. Without the check a negative rate is
// a negative fee: no fee output, and more than amountIn credited to the pool.
func TestStrictFeeRates(t *testing.T) {
	const amountIn = 40_000
	for _, bps := range [][3]int64{
		{-30, 0, 0}, {0, -1, 0}, {0, 0, -10_000}, {-1, -1, -1}, {6000, 6000, -3000},
	} {
		f := newFixture(t, bps[0], bps[1], bps[2])
		p := fresh()
		t.Run(fmt.Sprintf("bps %v: Swap refused", bps), func(t *testing.T) {
			// The outputs the script's arithmetic gives: each positive fee
			// its own output, the negative ones none.
			fees := [3]int64{scriptFee(amountIn, bps[0]), scriptFee(amountIn, bps[1]), scriptFee(amountIn, bps[2])}
			net := amountIn - fees[0] - fees[1] - fees[2]
			if net <= 0 {
				t.Fatalf("net %d: the case does not isolate the fee check", net)
			}
			paid := net * p.b / (p.a + net)
			s := shapedSwap(f, p, pushNum(amountIn), true, net, paid)
			for i, h := range [][]byte{lpFeeAPkh, pkh(p.validator), commissionPkh} {
				if fees[i] > 0 {
					s.outs = append(s.outs, out{1, tokenP2pkh(assetA, fees[i], h)})
				}
			}
			mustFailPool(t, f.run(t, s))
		})
		t.Run(fmt.Sprintf("bps %v: Rescind allowed", bps), func(t *testing.T) {
			s, _ := f.rescind(p)
			mustPass(t, f.run(t, s))
		})
		t.Run(fmt.Sprintf("bps %v: Close allowed", bps), func(t *testing.T) {
			mustPass(t, f.run(t, f.close(p)))
		})
	}

	// Rates summing to 100% or more: no check of their own. A positive
	// amountIn leaves no net, and a negative one (TestSwapRejects) is refused
	// as such.
	for _, bps := range [][3]int64{{5000, 5000, 0}, {10_000, 0, 0}, {3334, 3333, 3333}, {9999, 9999, 0}} {
		f := newFixture(t, bps[0], bps[1], bps[2])
		p := fresh()
		t.Run(fmt.Sprintf("bps %v: Swap refused", bps), func(t *testing.T) {
			s, _ := f.swap(p, amountIn, true)
			mustFailPool(t, f.run(t, s))
		})
		t.Run(fmt.Sprintf("bps %v: Close allowed", bps), func(t *testing.T) {
			mustPass(t, f.run(t, f.close(p)))
		})
	}
	t.Run("bps [9999 0 0]: the highest single rate swaps", func(t *testing.T) {
		f := newFixture(t, 9999, 0, 0)
		s, r := f.swap(fresh(), 4_000_000, true)
		if !r.ok {
			t.Fatal("the model rejects it")
		}
		mustPass(t, f.run(t, s))
	})
}

// An asset id is Rúnar's fixed 32-byte type: Rúnar v1's SDK refuses to build
// a pool with an id of any other length. (No script checks the width: a pool
// built another way with such an id is the lookup's to refuse.)
func TestStrictAssetIdWidth(t *testing.T) {
	build := func(idA, idB []byte) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		vaultHash := sha256.Sum256([]byte{0x51})
		p := fresh()
		c := runar.NewRunarContract(artifact(t, "PairPool.runar.go"), []interface{}{
			p.a, p.b,
			hex.EncodeToString(p.lp.PubKey().Compressed()),
			hex.EncodeToString(p.validator.PubKey().Compressed()),
			hex.EncodeToString(validatorIdentity.PubKey().Compressed()),
			hex.EncodeToString(nonce),
			hex.EncodeToString(lpFeeAPkh),
			hex.EncodeToString(lpFeeBPkh),
			hex.EncodeToString(affirmationR),
			hex.EncodeToString(affirmationS),
			hex.EncodeToString(idA),
			hex.EncodeToString(idB),
			hex.EncodeToString(vaultHash[:]),
			int64(30), int64(5), int64(10),
		})
		if c.GetLockingScript() == "" {
			return fmt.Errorf("no locking script")
		}
		return nil
	}
	if err := build(assetA, assetB); err != nil {
		t.Fatalf("32-byte ids: %v", err)
	}
	for _, n := range []int{0, 31, 33, 36} {
		for _, which := range []string{"AssetIdA", "AssetIdB"} {
			a, b := assetA, assetB
			if which == "AssetIdA" {
				a = bytes.Repeat([]byte{0xa5}, n)
			} else {
				b = bytes.Repeat([]byte{0xb6}, n)
			}
			err := build(a, b)
			t.Logf("%s of %d bytes: %v", which, n, err)
			if advIsHead(t) && err == nil {
				t.Errorf("%s of %d bytes: built", which, n)
			}
		}
	}
}
