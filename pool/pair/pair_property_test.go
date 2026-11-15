package pair

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"
	"math/rand"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// Property tests for the pair pool: an independent math/big model (propModel,
// cross-checked against fixture.model), random swaps and chains executed
// through the interpreter on both contract inputs, rounding edges, token
// amount push boundaries, limits, OP_PUSH_TX robustness and Rúnar's sats
// change. Every run goes through advRun (pair_adversarial_test.go), so the
// stranding invariant is checked here too. Seeds are fixed and logged.

// --- big-number helpers ---

func propBig(n int64) *big.Int { return big.NewInt(n) }

func propPow2(e uint) *big.Int { return new(big.Int).Lsh(big.NewInt(1), e) }

func propPow2m1(e uint) *big.Int { return new(big.Int).Sub(propPow2(e), big.NewInt(1)) }

func propAdd(a, b *big.Int) *big.Int { return new(big.Int).Add(a, b) }
func propSub(a, b *big.Int) *big.Int { return new(big.Int).Sub(a, b) }
func propMul(a, b *big.Int) *big.Int { return new(big.Int).Mul(a, b) }

var propMax63 = propPow2m1(63)

// propScriptNum is n's minimal script-number encoding (sign-magnitude, LE).
func propScriptNum(n *big.Int) []byte {
	if n.Sign() == 0 {
		return nil
	}
	m := new(big.Int).Abs(n).Bytes()
	le := make([]byte, len(m))
	for i := range m {
		le[i] = m[len(m)-1-i]
	}
	if le[len(le)-1]&0x80 != 0 {
		le = append(le, 0)
	}
	if n.Sign() < 0 {
		le[len(le)-1] |= 0x80
	}
	return le
}

// propPushBig pushes n minimally.
func propPushBig(n *big.Int) []byte {
	switch {
	case n.Sign() == 0:
		return []byte{0x00}
	case n.IsInt64() && n.Int64() >= 1 && n.Int64() <= 16:
		return []byte{byte(0x50 + n.Int64())}
	case n.IsInt64() && n.Int64() == -1:
		return []byte{0x4f}
	}
	return pushData(propScriptNum(n))
}

// propTokenPrefix is a BRC-162 prefix for an amount > 0 of any size.
func propTokenPrefix(asset []byte, amt *big.Int) []byte {
	p := append([]byte{0x20}, asset...)
	p = append(p, propPushBig(amt)...)
	return append(p, 0x6d)
}

func propTokenOut(asset []byte, amt *big.Int, h []byte) []byte {
	return append(propTokenPrefix(asset, amt), p2pkh(h)...)
}

// propLE8 is the low 64 bits of x, little-endian (the state's fixed width).
func propLE8(x *big.Int) []byte {
	m := new(big.Int).And(x, propPow2m1(64)).Bytes()
	b := make([]byte, 8)
	for i := range m {
		b[i] = m[len(m)-1-i]
	}
	return b
}

// propPoolLock is the pool output for p's other fields with the A prefix
// prefixA and the state reserves stateA, stateB.
func propPoolLock(f *fixture, p pair, prefixA, stateA, stateB *big.Int) []byte {
	st := p.state()
	copy(st[0:8], propLE8(stateA))
	copy(st[8:16], propLE8(stateB))
	s := append(propTokenPrefix(assetA, prefixA), f.code...)
	s = append(s, 0x6a)
	return append(s, st...)
}

func propVaultLock(f *fixture, b *big.Int) []byte {
	return append(propTokenPrefix(assetB, b), f.vault...)
}

// --- the model ---

type propVals struct {
	lp, val, com, net, out, newA, newB *big.Int
	ok                                 bool
	why                                string
}

func propCeilFee(in *big.Int, bps int64) *big.Int {
	x := propMul(in, propBig(bps))
	x.Add(x, propBig(9999))
	return x.Quo(x, propBig(10000)) // Quo truncates toward zero, as the script's OP_DIV does
}

// propModel is the swap as the header comment states it, from scratch.
func propModel(bps [3]int64, ra, rb, in *big.Int, aIn bool) propVals {
	z := propBig(0)
	v := propVals{lp: z, val: z, com: z, net: z, out: z, newA: ra, newB: rb}
	if in.Sign() <= 0 {
		v.why = "amountIn <= 0"
		return v
	}
	v.lp, v.val, v.com = propCeilFee(in, bps[0]), propCeilFee(in, bps[1]), propCeilFee(in, bps[2])
	v.net = propSub(propSub(propSub(in, v.lp), v.val), v.com)
	if v.net.Sign() <= 0 {
		v.why = "fees take the whole amount"
		return v
	}
	rin, rout := ra, rb
	if !aIn {
		rin, rout = rb, ra
	}
	v.out = new(big.Int).Quo(propMul(v.net, rout), propAdd(rin, v.net))
	newIn, newOut := propAdd(rin, v.net), propSub(rout, v.out)
	v.newA, v.newB = newIn, newOut
	if !aIn {
		v.newA, v.newB = newOut, newIn
	}
	switch {
	case in.Cmp(propPow2m1(64)) > 0:
		v.why = "amountIn past 2^64-1"
	case v.out.Sign() <= 0:
		v.why = "output rounds to 0"
	case newOut.Sign() <= 0:
		v.why = "output reserve emptied"
	case newIn.Cmp(propMax63) > 0:
		v.why = "input reserve past 2^63-1"
	default:
		v.ok = true
	}
	return v
}

// propInvariants: k never decreases, the payout is below its reserve, fees
// round up.
func propInvariants(t testing.TB, bps [3]int64, ra, rb, in *big.Int, aIn bool, v propVals) {
	t.Helper()
	if !v.ok {
		return
	}
	if propMul(v.newA, v.newB).Cmp(propMul(ra, rb)) < 0 {
		t.Fatalf("k decreased: %v*%v < %v*%v", v.newA, v.newB, ra, rb)
	}
	rout := rb
	if !aIn {
		rout = ra
	}
	if v.out.Cmp(rout) >= 0 {
		t.Fatalf("payout %v >= reserve %v", v.out, rout)
	}
	for i, fee := range []*big.Int{v.lp, v.val, v.com} {
		exact := propMul(in, propBig(bps[i]))
		lo := propMul(fee, propBig(10000))
		if lo.Cmp(exact) < 0 || propSub(lo, propBig(10000)).Cmp(exact) >= 0 || (bps[i] > 0 && fee.Sign() <= 0) {
			t.Fatalf("fee %d: %v is not ceil(%v*%d/10000)", i, fee, in, bps[i])
		}
	}
}

// propCrossCheck compares propModel with fixture.model where both apply.
func propCrossCheck(t testing.TB, f *fixture, p pair, in int64, aIn bool, v propVals) {
	t.Helper()
	m := f.model(p, in, aIn, key(99))
	if m.ok != v.ok {
		t.Fatalf("models disagree on validity: a=%d b=%d in=%d aIn=%v fixture.model %v, propModel %v (%s)", p.a, p.b, in, aIn, m.ok, v.ok, v.why)
	}
	if !m.ok {
		return
	}
	if m.out != v.out.Int64() || m.lpFee != v.lp.Int64() || m.valFee != v.val.Int64() || m.com != v.com.Int64() ||
		m.after.a != v.newA.Int64() || m.after.b != v.newB.Int64() {
		t.Fatalf("models disagree: a=%d b=%d in=%d aIn=%v: %+v vs %+v", p.a, p.b, in, aIn, m, v)
	}
}

// propSpend is the Swap the model describes (built for any v, valid or not).
func propSpend(f *fixture, p pair, in *big.Int, aIn bool, tc *big.Int, next *ec.PrivateKey) (spend, propVals) {
	v := propModel(f.bps, propBig(p.a), propBig(p.b), in, aIn)
	if next == nil {
		next = advNext(p.validator)
	}
	if tc == nil {
		tc = propBig(0)
	}
	after := p
	after.validator = next
	inAsset, outAsset, lpPkh := assetA, assetB, lpFeeAPkh
	if !aIn {
		inAsset, outAsset, lpPkh = assetB, assetA, lpFeeBPkh
	}
	outs := []out{
		{p.sats, propPoolLock(f, after, v.newA, v.newA, v.newB)},
		{1, propVaultLock(f, v.newB)},
		{1, propTokenOut(outAsset, v.out, userPkh)},
	}
	for _, fe := range []struct {
		amt *big.Int
		h   []byte
	}{{v.lp, lpPkh}, {v.val, pkh(p.validator)}, {v.com, commissionPkh}} {
		if fe.amt.Sign() > 0 {
			outs = append(outs, out{1, propTokenOut(inAsset, fe.amt, fe.h)})
		}
	}
	if tc.Sign() > 0 {
		outs = append(outs, out{1, propTokenOut(inAsset, tc, changePkh)})
	}
	return spend{
		ins: f.ins(p), outs: outs, poolIdx: 0, vaultIdx: 1,
		method: methodSwap, signer: p.validator,
		args: [][]byte{
			pushData(next.PubKey().Compressed()),
			propPushBig(in),
			pushBool(aIn),
			pushData(userPkh),
			pushData(commissionPkh),
			pushData(changePkh),
			propPushBig(tc),
		},
	}, v
}

func propPoolOnly(s spend) spend {
	s = advCopy(s)
	s.vaultIdx = -1
	return s
}

// propMutations are the rejected neighbours of an honest swap (s, v).
func propMutations(f *fixture, p pair, aIn bool, s spend, v propVals) map[string]spend {
	outAsset, inAsset := assetB, assetA
	if !aIn {
		outAsset, inAsset = assetA, assetB
	}
	one := propBig(1)
	after := p
	after.validator = advNext(p.validator)
	m := map[string]spend{}
	edit := func(name string, fn func(c *spend)) {
		c := advCopy(s)
		fn(&c)
		m[name] = propPoolOnly(c)
	}
	edit("payout + 1", func(c *spend) { c.outs[2].script = propTokenOut(outAsset, propAdd(v.out, one), userPkh) })
	nA, nB := v.newA, v.newB
	if aIn {
		nB = propSub(nB, one)
	} else {
		nA = propSub(nA, one)
	}
	if nA.Sign() > 0 && nB.Sign() > 0 {
		edit("pool one poorer in the paid-out token, payout + 1", func(c *spend) {
			c.outs[0].script = propPoolLock(f, after, nA, nA, nB)
			c.outs[1].script = propVaultLock(f, nB)
			c.outs[2].script = propTokenOut(outAsset, propAdd(v.out, one), userPkh)
		})
	}
	idx := 3
	for i, fe := range []*big.Int{v.lp, v.val, v.com} {
		if fe.Sign() == 0 {
			continue
		}
		at, fee, name := idx, fe, []string{"LP fee", "validator fee", "commission"}[i]
		edit(name+" - 1", func(c *spend) {
			less := propSub(fee, one)
			if less.Sign() == 0 {
				c.outs = append(c.outs[:at], c.outs[at+1:]...)
				return
			}
			old := c.outs[at].script
			c.outs[at].script = append(propTokenPrefix(inAsset, less), old[len(propTokenPrefix(inAsset, fee)):]...)
		})
		idx++
	}
	edit("vault + 1", func(c *spend) { c.outs[1].script = propVaultLock(f, propAdd(v.newB, one)) })
	if v.newB.Cmp(one) > 0 {
		edit("vault - 1", func(c *spend) { c.outs[1].script = propVaultLock(f, propSub(v.newB, one)) })
	}
	for _, d := range []int64{-1, 1} {
		d := propBig(d)
		edit(fmt.Sprintf("state ReserveA %+d", d), func(c *spend) {
			c.outs[0].script = propPoolLock(f, after, v.newA, propAdd(v.newA, d), v.newB)
		})
		edit(fmt.Sprintf("state ReserveB %+d", d), func(c *spend) {
			c.outs[0].script = propPoolLock(f, after, v.newA, v.newA, propAdd(v.newB, d))
		})
	}
	return m
}

// propLog is a log-uniform positive int64 below 2^bits.
func propLog(rng *rand.Rand, bits int) int64 {
	return 1 + rng.Int63n(int64(1)<<uint(1+rng.Intn(bits)))
}

// --- random swaps ---

var propCfgs = [][3]int64{{0, 0, 0}, {30, 5, 0}, {30, 5, 10}, {1, 1, 1}, {2500, 2500, 2500}, {9999, 0, 0}}

func TestPropSwapDifferential(t *testing.T) {
	const seed, perCfg = 20261116, 260
	t.Logf("seed %d, %d valid swaps per fee config", seed, perCfg)
	rng := rand.New(rand.NewSource(seed))
	total := 0
	for _, bps := range propCfgs {
		f := advFix(t, bps)
		valid, invalidRun, attempts := 0, 0, 0
		for valid < perCfg {
			attempts++
			if attempts > 200_000 {
				t.Fatalf("bps %v: only %d valid swaps in %d attempts", bps, valid, attempts)
			}
			p := pair{sats: 1, a: propLog(rng, 50), b: propLog(rng, 50), lp: key(10), validator: key(20)}
			if p.a > 1<<50 || p.b > 1<<50 {
				continue
			}
			in := propLog(rng, 52)
			if bps[0] == 9999 && rng.Intn(2) == 0 {
				in += 10_000
			}
			aIn := rng.Intn(2) == 0
			s, v := propSpend(f, p, propBig(in), aIn, nil, nil)
			propCrossCheck(t, f, p, in, aIn, v)
			propInvariants(t, bps, propBig(p.a), propBig(p.b), propBig(in), aIn, v)
			if !v.ok {
				// the model's rejections, sampled: the script rejects the
				// outputs the arithmetic would give
				if invalidRun < 25 && v.net.Sign() > 0 && v.newA.Sign() > 0 && v.newB.Sign() > 0 {
					invalidRun++
					if r := advRun(t, f, propPoolOnly(s), advOpts{}); r.pool == nil {
						t.Fatalf("bps %v a=%d b=%d in=%d aIn=%v: model rejects (%s), script accepts", bps, p.a, p.b, in, aIn, v.why)
					}
				}
				continue
			}
			valid++
			r, tx := advRunTx(t, f, s, advOpts{})
			if !r.ok() {
				t.Fatalf("HONEST SWAP REJECTED bps %v a=%d b=%d in=%d aIn=%v: pool %v vault %v\ntx %s", bps, p.a, p.b, in, aIn, r.pool, r.vault, advTxHex(tx))
			}
			if valid%8 != 0 {
				continue // every mutation, on every eighth swap (about 30 per fee config)
			}
			for name, m := range propMutations(f, p, aIn, s, v) {
				if advRun(t, f, m, advOpts{}).pool == nil {
					t.Fatalf("bps %v a=%d b=%d in=%d aIn=%v: mutation %q ACCEPTED", bps, p.a, p.b, in, aIn, name)
				}
			}
		}
		total += valid
		t.Logf("bps %v: %d valid swaps verified on both inputs (%d attempts), %d model rejections run and rejected", bps, valid, attempts, invalidRun)
	}
	if total < 1500 {
		t.Fatalf("%d swaps", total)
	}
}

// --- chains ---

// 200 swaps, each continuation the next swap's input, the validator key
// rotated at every step; then Close. And a chain ending Rescind -> Close.
func TestPropSwapChain(t *testing.T) {
	const seed = 4242
	t.Logf("seed %d", seed)
	rng := rand.New(rand.NewSource(seed))
	for ci, bps := range [][3]int64{{30, 5, 10}, {0, 0, 0}, {2500, 2500, 2500}} {
		f := advFix(t, bps)
		p := pair{sats: 1 + rng.Int63n(1000), a: 1 + rng.Int63n(1<<40), b: 1 + rng.Int63n(1<<40), lp: key(10), validator: key(30)}
		k0 := propMul(propBig(p.a), propBig(p.b))
		for i := 0; i < 200; i++ {
			aIn := rng.Intn(2) == 0
			rin := p.a
			if !aIn {
				rin = p.b
			}
			in := 1 + rng.Int63n(rin/10+1) + rin/1000
			next := key(byte(31 + (i+1)%200)) // 31..230, never the LP's 10
			if next.Serialize()[0] == p.validator.Serialize()[0] {
				t.Fatal("chain key did not rotate")
			}
			s, v := propSpend(f, p, propBig(in), aIn, nil, next)
			if !v.ok {
				i--
				continue
			}
			propInvariants(t, bps, propBig(p.a), propBig(p.b), propBig(in), aIn, v)
			r, tx := advRunTx(t, f, s, advOpts{})
			if !r.ok() {
				t.Fatalf("chain %d step %d: honest swap rejected: pool %v vault %v\ntx %s", ci, i, r.pool, r.vault, advTxHex(tx))
			}
			if i%25 == 0 && i > 0 {
				// p.validator is key(31 + i%200); the one before it key(31 + (i-1)%200)
				prev := key(byte(31 + (i-1)%200))
				if prev.Serialize()[0] == p.validator.Serialize()[0] {
					t.Fatal("previous key equals the current one")
				}
				old := s
				old.signer = prev
				if advRun(t, f, propPoolOnly(old), advOpts{}).pool == nil {
					t.Fatalf("chain %d step %d: a previous validator key signed", ci, i)
				}
			}
			p.a, p.b, p.validator = v.newA.Int64(), v.newB.Int64(), next
		}
		if propMul(propBig(p.a), propBig(p.b)).Cmp(k0) < 0 {
			t.Fatalf("chain %d: k decreased over the chain", ci)
		}
		mustPass(t, advH(t, f, f.close(p)))
		rs, after := f.rescind(p)
		mustPass(t, advH(t, f, rs))
		mustPass(t, advH(t, f, f.close(after)))
		t.Logf("bps %v: 200 swaps, final a=%d b=%d, then Close; and Rescind -> Close", bps, p.a, p.b)
	}
}

// --- rounding edges ---

// propExpect runs the swap the model describes and asserts the script agrees
// with it: an honest valid swap passes on both inputs, an invalid one fails.
func propExpect(t *testing.T, f *fixture, p pair, in *big.Int, aIn bool, tc *big.Int) propVals {
	t.Helper()
	s, v := propSpend(f, p, in, aIn, tc, nil)
	r, tx := advRunTx(t, f, s, advOpts{})
	if v.ok && !r.ok() {
		t.Fatalf("HONEST SWAP REJECTED a=%d b=%d in=%v aIn=%v: pool %v vault %v\ntx %s", p.a, p.b, in, aIn, r.pool, r.vault, advTxHex(tx))
	}
	if !v.ok && r.pool == nil {
		t.Fatalf("a=%d b=%d in=%v aIn=%v: model rejects (%s), script accepts", p.a, p.b, in, aIn, v.why)
	}
	if !v.ok {
		t.Logf("a=%d b=%d in=%v %s: model %q, script %v", p.a, p.b, in, advDir(aIn), v.why, advErrS(r.pool))
	}
	return v
}

func TestPropRoundingSmallestAmount(t *testing.T) {
	for _, bps := range propCfgs {
		f := advFix(t, bps)
		for _, rs := range [][2]int64{{1000, 1_000_000}, {1_000_000, 1000}, {1, 1}, {1, 2}, {7, 3}} {
			for _, aIn := range []bool{true, false} {
				p := pair{sats: 1, a: rs[0], b: rs[1], lp: key(10), validator: key(20)}
				rin, rout := p.a, p.b
				if !aIn {
					rin, rout = p.b, p.a
				}
				if rout < 2 {
					// out = floor(net*1/(rin+net)) = 0 for every net
					for _, in := range []int64{1, 1000, 1 << 40} {
						if propExpect(t, f, p, propBig(in), aIn, nil).ok {
							t.Fatal("a reserve of 1 paid out")
						}
					}
					t.Logf("bps %v reserves %v %s: output reserve 1, no amountIn passes", bps, rs, advDir(aIn))
					continue
				}
				// out >= 1 needs net >= N = ceil(rin/(rout-1)), and net <=
				// in*(10000-sum)/10000 (fees round up): no amountIn below lo passes.
				sum := bps[0] + bps[1] + bps[2]
				nNeed := (rin + rout - 2) / (rout - 1)
				lo := (nNeed*10000 + (10000 - sum) - 1) / (10000 - sum)
				for in := int64(1); in < lo && in < 5000; in++ {
					if propModel(bps, propBig(p.a), propBig(p.b), propBig(in), aIn).ok {
						t.Fatalf("bound wrong: %d < %d passes", in, lo)
					}
				}
				min := int64(-1)
				for in := lo; in < lo+100; in++ {
					if propModel(bps, propBig(p.a), propBig(p.b), propBig(in), aIn).ok {
						min = in
						break
					}
				}
				if min < 0 {
					t.Fatalf("bps %v reserves %v %s: nothing passes in [%d, %d)", bps, rs, advDir(aIn), lo, lo+100)
				}
				if v := propExpect(t, f, p, propBig(min), aIn, nil); !v.ok {
					t.Fatal("smallest amount not valid")
				}
				if min > 1 {
					propExpect(t, f, p, propBig(min-1), aIn, nil)
				}
				t.Logf("bps %v reserves %v %s: smallest passing amountIn %d", bps, rs, advDir(aIn), min)
			}
		}
	}
}

func propGcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Fee boundaries: amountIn*bps exactly divisible by 10000, one below, one above.
func TestPropRoundingFeeBoundaries(t *testing.T) {
	for _, bps := range propCfgs {
		f := advFix(t, bps)
		p := pair{sats: 1, a: 1 << 40, b: 1 << 40, lp: key(10), validator: key(20)}
		for i, b := range bps {
			if b == 0 {
				continue
			}
			step := 10000 / propGcd(b, 10000)
			for _, mlt := range []int64{1, 2, 7, 1000} {
				in0 := step * mlt
				for _, in := range []int64{in0 - 1, in0, in0 + 1} {
					if in <= 0 {
						continue
					}
					for _, aIn := range []bool{true, false} {
						v := propExpect(t, f, p, propBig(in), aIn, nil)
						if !v.ok {
							continue
						}
						// the fee one short is refused
						s, _ := propSpend(f, p, propBig(in), aIn, nil, nil)
						muts := propMutations(f, p, aIn, s, v)
						name := []string{"LP fee", "validator fee", "commission"}[i] + " - 1"
						if m, ok := muts[name]; ok && advRun(t, f, m, advOpts{}).pool == nil {
							t.Fatalf("bps %v in %d: %s accepted", bps, in, name)
						}
					}
				}
			}
		}
	}
}

// Output division exact, one below and one above, and the output rounding to
// 0 both ways.
func TestPropRoundingOutput(t *testing.T) {
	f := advFix(t, [3]int64{0, 0, 0})
	for _, aIn := range []bool{true, false} {
		// rin 1000, rout 2000: net 1000 -> out exactly 1000
		p := pair{sats: 1, a: 1000, b: 2000, lp: key(10), validator: key(20)}
		if !aIn {
			p.a, p.b = 2000, 1000
		}
		for _, in := range []int64{999, 1000, 1001} {
			v := propExpect(t, f, p, propBig(in), aIn, nil)
			t.Logf("%s rin 1000 rout 2000 net %d: out %v", advDir(aIn), in, v.out)
		}
		// out rounds to 0: rout 10, rin 2^40
		q := pair{sats: 1, a: 1 << 40, b: 10, lp: key(10), validator: key(20)}
		if !aIn {
			q.a, q.b = 10, 1<<40
		}
		// out = floor(net*10/(2^40+net)) >= 1 iff 9*net >= 2^40
		edge := (int64(1)<<40 + 8) / 9
		for _, in := range []int64{1, 1000, edge - 1, edge, edge + 1} {
			v := propExpect(t, f, q, propBig(in), aIn, nil)
			t.Logf("%s rin 2^40 rout 10 net %d: out %v ok %v", advDir(aIn), in, v.out, v.ok)
		}
		if propModel(f.bps, propBig(q.a), propBig(q.b), propBig(edge-1), aIn).ok || !propModel(f.bps, propBig(q.a), propBig(q.b), propBig(edge), aIn).ok {
			t.Fatal("the out-rounds-to-zero edge is not where computed")
		}
	}
}

// --- token amount push boundaries ---

var propBoundaries = []struct {
	amount  *big.Int
	pushHex string // hand-written
}{
	{propBig(1), "51"},
	{propBig(16), "60"},
	{propBig(17), "0111"},
	{propBig(127), "017f"},
	{propBig(128), "028000"},
	{propBig(255), "02ff00"},
	{propBig(256), "020001"},
	{propBig(32767), "02ff7f"},
	{propBig(32768), "03008000"},
	{propPow2m1(31), "04ffffff7f"},
	{propPow2(31), "050000008000"},
	{propPow2(32), "050000000001"},
	{propPow2(53), "0700000000000020"},
	{propPow2(62), "080000000000000040"},
	{propPow2m1(63), "08ffffffffffffff7f"},
	{propPow2(63), "09000000000000008000"},
	{propPow2m1(64), "09ffffffffffffffff00"},
}

// propNonMinimal: other encodings of the same amount (and negative zero for
// 128's low byte), none of which the contract writes.
func propNonMinimal(a *big.Int, minimal []byte) [][]byte {
	var alts [][]byte
	if a.Cmp(propBig(16)) <= 0 {
		alts = append(alts, []byte{0x01, byte(a.Int64())}, []byte{0x02, byte(a.Int64()), 0x00}, []byte{0x4c, 0x01, byte(a.Int64())})
	} else {
		d := minimal[1:]
		alts = append(alts, append([]byte{byte(len(d) + 1)}, append(append([]byte{}, d...), 0x00)...))
		alts = append(alts, append([]byte{0x4c, byte(len(d))}, d...))
	}
	if a.Cmp(propBig(128)) == 0 {
		alts = append(alts, []byte{0x01, 0x80})
	}
	return alts
}

type propLoc struct {
	name  string
	asset func(aIn bool) []byte
	// build returns the fixture, pool, amountIn, direction, token change and
	// the output index holding the amount; skip says why it is unreachable.
	build func(t *testing.T, a *big.Int) (f *fixture, p pair, in *big.Int, aIn bool, tc *big.Int, idx int, skip string)
}

func propPairOf(a, b int64) pair { return pair{sats: 1, a: a, b: b, lp: key(10), validator: key(20)} }

func propFeeLoc(name string, which int) propLoc {
	return propLoc{name, func(aIn bool) []byte { return map[bool][]byte{true: assetA, false: assetB}[aIn] },
		func(t *testing.T, a *big.Int) (*fixture, pair, *big.Int, bool, *big.Int, int, string) {
			for _, b := range []int64{9999, 5000} {
				var cfg [3]int64
				cfg[which] = b
				f := advFix(t, cfg)
				p := propPairOf(1_000_000_000, 1)
				c0 := new(big.Int).Quo(propMul(a, propBig(10000)), propBig(b))
				if c0.Cmp(propPow2m1(64)) > 0 {
					continue
				}
				for d := int64(-3); d <= 3; d++ {
					in := propAdd(c0, propBig(d))
					if in.Sign() <= 0 || propCeilFee(in, b).Cmp(a) != 0 {
						continue
					}
					if propModel(cfg, propBig(p.a), propBig(p.b), in, false).ok {
						return f, p, in, false, nil, 3, ""
					}
				}
			}
			// A fee is at most ceil((2^64-1) * bps / 10000) (amountIn <= 2^64-1):
			// with 9999 bps, about 2^64 * 0.9999, so 2^64-1 itself is out of reach.
			return nil, pair{}, nil, false, nil, 0, "unreachable: this fee needs amountIn above 2^64-1"
		}}
}

var propLocs = []propLoc{
	{"pool prefix (ReserveA)", func(bool) []byte { return assetA }, func(t *testing.T, a *big.Int) (*fixture, pair, *big.Int, bool, *big.Int, int, string) {
		if a.Cmp(propMax63) > 0 {
			return nil, pair{}, nil, false, nil, 0, "unreachable: a reserve is at most 2^63-1 (its 8 state bytes)"
		}
		f := advFix(t, [3]int64{0, 0, 0})
		if a.Cmp(propBig(1)) == 0 { // B in: a 2, b 1, net 1 -> out 1, newA 1
			return f, propPairOf(2, 1), propBig(1), false, nil, 0, ""
		}
		return f, propPairOf(1, 1_000_000), propSub(a, propBig(1)), true, nil, 0, ""
	}},
	{"vault prefix (ReserveB)", func(bool) []byte { return assetB }, func(t *testing.T, a *big.Int) (*fixture, pair, *big.Int, bool, *big.Int, int, string) {
		if a.Cmp(propMax63) > 0 {
			return nil, pair{}, nil, false, nil, 0, "unreachable: a reserve is at most 2^63-1 (its 8 state bytes)"
		}
		f := advFix(t, [3]int64{0, 0, 0})
		if a.Cmp(propBig(1)) == 0 {
			return f, propPairOf(1, 2), propBig(1), true, nil, 1, ""
		}
		return f, propPairOf(1_000_000, 1), propSub(a, propBig(1)), false, nil, 1, ""
	}},
	{"payout", func(aIn bool) []byte { return map[bool][]byte{true: assetB, false: assetA}[aIn] }, func(t *testing.T, a *big.Int) (*fixture, pair, *big.Int, bool, *big.Int, int, string) {
		if a.Cmp(propMax63) >= 0 {
			return nil, pair{}, nil, false, nil, 0, "unreachable: payout < reserve <= 2^63-1"
		}
		// rin 1, rout a+1, net a: out = floor(a(a+1)/(a+1)) = a
		return advFix(t, [3]int64{0, 0, 0}), propPairOf(1, propAdd(a, propBig(1)).Int64()), a, true, nil, 2, ""
	}},
	propFeeLoc("LP fee", 0),
	propFeeLoc("validator fee", 1),
	propFeeLoc("commission", 2),
	{"token change", func(aIn bool) []byte { return map[bool][]byte{true: assetA, false: assetB}[aIn] }, func(t *testing.T, a *big.Int) (*fixture, pair, *big.Int, bool, *big.Int, int, string) {
		return advFix(t, [3]int64{0, 0, 0}), propPairOf(1_000_000, 1_000_000), propBig(100), true, a, 3, ""
	}},
}

func TestPropTokenPushBoundaries(t *testing.T) {
	for _, loc := range propLocs {
		loc := loc
		t.Run(loc.name, func(t *testing.T) {
			for _, bd := range propBoundaries {
				a := bd.amount
				f, p, in, aIn, tc, idx, skip := loc.build(t, a)
				if skip != "" {
					t.Logf("amount %v: %s", a, skip)
					continue
				}
				s, v := propSpend(f, p, in, aIn, tc, nil)
				if !v.ok {
					t.Fatalf("amount %v: scenario invalid in the model: %s", a, v.why)
				}
				asset := loc.asset(aIn)
				want, _ := hex.DecodeString("20" + hex.EncodeToString(asset) + bd.pushHex + "6d")
				old := s.outs[idx].script
				if !bytes.HasPrefix(old, want) {
					t.Fatalf("amount %v: output %d starts %x, the table says %x", a, idx, old[:len(want)], want)
				}
				if a.IsInt64() && !bytes.Equal(tokenPrefix(asset, a.Int64()), want) {
					t.Fatalf("amount %v: the harness's tokenPrefix disagrees with the table", a)
				}
				r, tx := advRunTx(t, f, s, advOpts{})
				if !r.ok() {
					t.Fatalf("HONEST SWAP REJECTED, amount %v at %s: pool %v vault %v\ntx %s", a, loc.name, r.pool, r.vault, advTxHex(tx))
				}
				push, _ := hex.DecodeString(bd.pushHex)
				rest := old[len(want):]
				for _, alt := range propNonMinimal(a, push) {
					c := advCopy(s)
					pre := append(append([]byte{0x20}, asset...), alt...)
					c.outs[idx].script = append(append(pre, 0x6d), rest...)
					if advRun(t, f, propPoolOnly(c), advOpts{}).pool == nil {
						t.Errorf("amount %v at %s: non-minimal push %x ACCEPTED", a, loc.name, alt)
					}
				}
			}
		})
	}
}

// --- limits ---

func TestPropReserveLimits(t *testing.T) {
	f := advFix(t, [3]int64{0, 0, 0})
	type tc struct {
		name  string
		a, b  int64
		in    *big.Int
		aIn   bool
		valid bool
	}
	q := int64(1) << 62
	max := propMax63.Int64()
	cases := []tc{
		{"ReserveA lands on 2^63-1", q, q, propPow2m1(62), true, true},
		{"ReserveA one past 2^63-1", q, q, propPow2(62), true, false},
		{"ReserveA lands on 2^63-1 from below", max - (1 << 40), q, propPow2(40), true, true},
		{"ReserveA one past, from below", max - (1 << 40) + 1, q, propPow2(40), true, false},
		{"ReserveB lands on 2^63-1", q, q, propPow2m1(62), false, true},
		{"ReserveB one past 2^63-1", q, q, propPow2(62), false, false},
		{"ReserveA at 2^63-1, B in", max, 1000, propBig(1000), false, true},
		{"ReserveB at 2^63-1, A in", 1000, max, propBig(1000), true, true},
		{"reserves 1 and 1, A in: out rounds to 0", 1, 1, propBig(1_000_000), true, false},
		{"reserves 1 and 1, B in: out rounds to 0", 1, 1, propBig(1 << 50), false, false},
		{"reserves 1 and 2, A in: out 1, leaves 1", 1, 2, propBig(1), true, true},
		{"reserves 1 and 2, A in, big amount: still leaves 1", 1, 2, propBig(1 << 50), true, true},
		{"reserves 2 and 1, B in: out 1, leaves 1", 2, 1, propBig(1), false, true},
		{"reserves 2 and 2, A in 1: out rounds to 0", 2, 2, propBig(1), true, false},
		{"reserves 2 and 2, A in 2: out 1", 2, 2, propBig(2), true, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			v := propExpect(t, f, propPairOf(c.a, c.b), c.in, c.aIn, nil)
			if v.ok != c.valid {
				t.Fatalf("model says ok=%v (%s), the case expects %v", v.ok, v.why, c.valid)
			}
		})
	}
}

func TestPropAmountInDomain(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := propPairOf(5_000_000, 2_000_000)
	for _, aIn := range []bool{true, false} {
		base, v := propSpend(f, p, propBig(40_000), aIn, nil, nil)
		if !v.ok {
			t.Fatal(v.why)
		}
		mustPass(t, advH(t, f, base))
		for _, bad := range []*big.Int{propBig(0), propBig(-1), propBig(-2), new(big.Int).Neg(propPow2(31)), new(big.Int).Neg(propPow2(63))} {
			c := advCopy(base)
			c.args[1] = propPushBig(bad)
			if advRun(t, f, c, advOpts{}).pool == nil {
				t.Errorf("%s: amountIn %v ACCEPTED", advDir(aIn), bad)
			}
			// and with the outputs the arithmetic would give
			s, _ := propSpend(f, p, bad, aIn, nil, nil)
			if advRun(t, f, s, advOpts{}).pool == nil {
				t.Errorf("%s: amountIn %v with its own outputs ACCEPTED", advDir(aIn), bad)
			}
		}
	}
	f9 := advFix(t, [3]int64{9999, 0, 0})
	fz := advFix(t, [3]int64{0, 0, 0})
	big15 := int64(1_000_000_000_000_000)
	for _, h := range []struct {
		name  string
		in    *big.Int
		f     *fixture
		aIn   bool
		valid bool
	}{
		{"2^63-1, 9999 bps", propPow2m1(63), f9, true, true},
		{"2^64-1, 9999 bps (the largest amountIn)", propPow2m1(64), f9, false, true},
		{"2^64, 9999 bps (amountIn past 2^64-1)", propPow2(64), f9, false, false},
		{"2^100, 9999 bps (net past 2^63-1)", propPow2(100), f9, true, false},
		{"2^63-1, no fees (reserve past 2^63-1)", propPow2m1(63), fz, false, false},
		{"2^64, no fees", propPow2(64), fz, true, false},
		{"2^100, no fees", propPow2(100), fz, false, false},
	} {
		h := h
		t.Run("amountIn "+h.name, func(t *testing.T) {
			v := propExpect(t, h.f, propPairOf(big15, big15), h.in, h.aIn, nil)
			if v.ok != h.valid {
				t.Fatalf("model ok=%v (%s), expected %v", v.ok, v.why, h.valid)
			}
		})
	}
	for _, aIn := range []bool{true, false} {
		for _, bad := range []*big.Int{propBig(-1), propBig(-5), new(big.Int).Neg(propPow2(63))} {
			c, _ := propSpend(f, p, propBig(40_000), aIn, nil, nil)
			c.args[6] = propPushBig(bad)
			if advRun(t, f, c, advOpts{}).pool == nil {
				t.Errorf("%s: tokenChange %v ACCEPTED", advDir(aIn), bad)
			}
		}
		// BRC-162's amounts up to 2^64-1 pass (2^63 and above as nine-byte
		// pushes); 2^64 and above are refused, the change output written for
		// them or not.
		for _, tcv := range []*big.Int{propPow2m1(63), propPow2(63), propPow2m1(64)} {
			c, v := propSpend(f, p, propBig(40_000), aIn, tcv, nil)
			if !v.ok {
				t.Fatal(v.why)
			}
			if r := advH(t, f, c); !r.ok() {
				t.Errorf("%s: honest tokenChange %v rejected: %v / %v", advDir(aIn), tcv, r.pool, r.vault)
			}
			c.outs = c.outs[:len(c.outs)-1]
			if advRun(t, f, c, advOpts{}).pool == nil {
				t.Errorf("%s: tokenChange %v without its output ACCEPTED", advDir(aIn), tcv)
			}
		}
		for _, tcv := range []*big.Int{propPow2(64), propAdd(propPow2(64), propBig(1)), propPow2m1(71), propPow2(100)} {
			c, _ := propSpend(f, p, propBig(40_000), aIn, tcv, nil)
			if advRun(t, f, c, advOpts{}).pool == nil {
				t.Errorf("%s: tokenChange %v with its output ACCEPTED", advDir(aIn), tcv)
			}
			c.outs = c.outs[:len(c.outs)-1]
			if advRun(t, f, c, advOpts{}).pool == nil {
				t.Errorf("%s: tokenChange %v without an output ACCEPTED", advDir(aIn), tcv)
			}
		}
	}
}

// --- OP_PUSH_TX robustness ---

func propRandScript(rng *rand.Rand) []byte {
	b := make([]byte, 20)
	rng.Read(b)
	switch rng.Intn(4) {
	case 0:
		return p2pkh(b)
	case 1:
		return tokenP2pkh(assetA, 1+rng.Int63n(1<<40), b)
	case 2:
		return tokenP2pkh(assetB, 1+rng.Int63n(1<<40), b)
	}
	s := make([]byte, 1+rng.Intn(80))
	rng.Read(s)
	return s
}

// propRandIns: the pair at (T, 0) and (T, 1) for a random T, then 1..4
// funding inputs at random outpoints.
func propRandIns(rng *rand.Rand, f *fixture, p pair) []in {
	tx := byte(1 + rng.Intn(250))
	ins := []in{{txid: tx, vout: 0, sats: p.sats, lock: f.poolLock(p)}, {txid: tx, vout: 1, sats: 1, lock: f.vaultLock(p)}}
	for i := 0; i <= rng.Intn(4); i++ {
		ins = append(ins, in{txid: byte(rng.Intn(256)), vout: uint32(rng.Intn(1000)), sats: 1 + rng.Int63n(1<<40)})
	}
	return ins
}

func propRandPair(rng *rand.Rand) pair {
	lp := byte(1 + rng.Intn(250))
	val := byte(1 + rng.Intn(250))
	return pair{sats: 1 + rng.Int63n(1<<40), a: 1 + rng.Int63n(1<<50), b: 1 + rng.Int63n(1<<50), lp: key(lp), validator: key(val)}
}

func TestPropPushTxRobustness(t *testing.T) {
	const n, seed = 500, 77
	f := advFix(t, [3]int64{30, 5, 10})
	rng := rand.New(rand.NewSource(seed))
	t.Logf("seed %d: %d Close and %d Rescind spends, both inputs verified", seed, n, n)
	var fails []string
	record := func(kind string, i int, r result, tx string) {
		fails = append(fails, fmt.Sprintf("%s #%d: pool %v vault %v\n  tx %s", kind, i, r.pool, r.vault, tx))
	}
	opts := func() advOpts {
		return advOpts{version: uint32(1 + rng.Intn(3)), lockTime: uint32(rng.Intn(3)) * uint32(rng.Int31())}
	}
	for i := 0; i < n; i++ {
		p := propRandPair(rng)
		s := f.close(p)
		s.ins = propRandIns(rng, f, p)
		s.outs = nil
		for j := 0; j <= rng.Intn(4); j++ {
			s.outs = append(s.outs, out{rng.Int63n(1 << 40), propRandScript(rng)})
		}
		if r, tx := advRunTx(t, f, s, opts()); !r.ok() {
			record("Close", i, r, advTxHex(tx))
		}
	}
	for i := 0; i < n; i++ {
		p := propRandPair(rng)
		s, _ := f.rescind(p)
		s.ins = propRandIns(rng, f, p)
		ch := make([]byte, 20)
		rng.Read(ch)
		sats := rng.Int63n(1 << 40)
		if sats > 0 {
			s.outs = append(s.outs, out{sats, p2pkh(ch)})
		}
		s.change = &out{sats, ch}
		if r, tx := advRunTx(t, f, s, opts()); !r.ok() {
			record("Rescind", i, r, advTxHex(tx))
		}
	}
	if len(fails) > 0 {
		show := fails
		if len(show) > 5 {
			show = show[:5]
		}
		t.Errorf("%d of %d honest spends rejected. First %d:\n%s", len(fails), 2*n, len(show), strings.Join(show, "\n"))
	}
}

// --- Rúnar's sats change on Swap and Rescind ---

func TestPropSwapSatsChange(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	ch, other := pkh(key(70)), pkh(key(71))
	for _, aIn := range []bool{true, false} {
		base, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: aIn})
		with := func(sats int64) spend {
			s, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: aIn, change: &out{sats, ch}})
			return s
		}
		name := advDir(aIn)
		t.Run(name+": change 0, no output", func(t *testing.T) { mustPass(t, advH(t, f, base)) })
		for _, s := range []int64{1, 546, 1_000_000_000_000} {
			s := s
			t.Run(fmt.Sprintf("%s: change %d", name, s), func(t *testing.T) { mustPass(t, advH(t, f, with(s))) })
		}
		mut := func(n string, edit func(c *spend)) {
			t.Run(name+": "+n, func(t *testing.T) {
				c := advCopy(with(546))
				edit(&c)
				mustFailPool(t, advH(t, f, c))
			})
		}
		mut("output present, amount argument 0", func(c *spend) { c.change = &out{0, ch} })
		mut("output present, no change arguments", func(c *spend) { c.change = nil })
		mut("arguments set, output missing", func(c *spend) { c.outs = c.outs[:len(c.outs)-1] })
		mut("two change outputs", func(c *spend) { c.outs = append(c.outs, c.outs[len(c.outs)-1]) })
		mut("change pkh mismatch", func(c *spend) { c.outs[len(c.outs)-1].script = p2pkh(other) })
		mut("change amount mismatch", func(c *spend) { c.outs[len(c.outs)-1].sats = 547 })
		mut("change output before the token outputs", func(c *spend) {
			last := c.outs[len(c.outs)-1]
			c.outs = append(c.outs[:2], append([]out{last}, c.outs[2:len(c.outs)-1]...)...)
		})
		t.Run(name+": negative change amount", func(t *testing.T) {
			c := advCopy(base)
			c.change = &out{0, ch}
			mustFailPool(t, advRun(t, f, c, advOpts{chAmt: propPushBig(propBig(-546))}))
		})
	}
}

func TestPropRescindSatsChange(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	ch, other := pkh(key(70)), pkh(key(71))
	base, _ := f.rescind(p)
	with := func(sats int64) spend {
		s := advCopy(base)
		s.outs = append(s.outs, out{sats, p2pkh(ch)})
		s.change = &out{sats, ch}
		return s
	}
	t.Run("change 0, no output", func(t *testing.T) { mustPass(t, advH(t, f, base)) })
	for _, s := range []int64{1, 546, 1_000_000_000_000} {
		s := s
		t.Run(fmt.Sprintf("change %d", s), func(t *testing.T) { mustPass(t, advH(t, f, with(s))) })
	}
	mut := func(n string, edit func(c *spend)) {
		t.Run(n, func(t *testing.T) {
			c := with(546)
			edit(&c)
			mustFailPool(t, advH(t, f, c))
		})
	}
	mut("output present, amount argument 0", func(c *spend) { c.change = &out{0, ch} })
	mut("arguments set, output missing", func(c *spend) { c.outs = c.outs[:2] })
	mut("two change outputs", func(c *spend) { c.outs = append(c.outs, c.outs[2]) })
	mut("change pkh mismatch", func(c *spend) { c.outs[2].script = p2pkh(other) })
	mut("change amount mismatch", func(c *spend) { c.outs[2].sats = 547 })
	mut("change output before the vault", func(c *spend) { c.outs[1], c.outs[2] = c.outs[2], c.outs[1] })
	t.Run("negative change amount", func(t *testing.T) {
		c := advCopy(base)
		c.change = &out{0, ch}
		mustFailPool(t, advRun(t, f, c, advOpts{chAmt: propPushBig(propBig(-546))}))
	})
}
