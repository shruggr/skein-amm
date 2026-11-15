package pool

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
)

// Property / differential tests for the pool's Swap, Close and Rescind. The
// oracle is propModelSwap, an independent math/big model of the contract's
// math; the verdict always comes from running the real script via f.run.

// --- big-number helpers ---

func propBig(n int64) *big.Int { return big.NewInt(n) }

func propPow2(e uint) *big.Int { return new(big.Int).Lsh(big.NewInt(1), e) }

// propPow2m1 is 2^e - 1 (propPow2m1(63) is MaxInt64).
func propPow2m1(e uint) *big.Int { return new(big.Int).Sub(propPow2(e), big.NewInt(1)) }

func propAdd(a, b *big.Int) *big.Int { return new(big.Int).Add(a, b) }
func propSub(a, b *big.Int) *big.Int { return new(big.Int).Sub(a, b) }
func propMul(a, b *big.Int) *big.Int { return new(big.Int).Mul(a, b) }
func propQuo(a, b *big.Int) *big.Int {
	if b.Sign() == 0 { // the script fails on a zero divisor; the model reports 0 (out <= 0 follows)
		return new(big.Int)
	}
	return new(big.Int).Quo(a, b)
}

var propMaxInt64 = propPow2m1(63)

// propPushBig is a minimal script push of any integer (OP_0, OP_1NEGATE,
// OP_1..OP_16, else a sign-magnitude little-endian data push), written
// independently of pushNum so it reaches past int64.
func propPushBig(n *big.Int) []byte {
	switch {
	case n.Sign() == 0:
		return []byte{0x00}
	case n.Cmp(big.NewInt(-1)) == 0:
		return []byte{0x4f}
	case n.Sign() > 0 && n.Cmp(big.NewInt(16)) <= 0:
		return []byte{byte(0x50 + n.Int64())}
	}
	neg := n.Sign() < 0
	be := new(big.Int).Abs(n).Bytes()
	le := make([]byte, len(be))
	for i := range be {
		le[len(be)-1-i] = be[i]
	}
	if le[len(le)-1]&0x80 != 0 {
		if neg {
			le = append(le, 0x80)
		} else {
			le = append(le, 0x00)
		}
	} else if neg {
		le[len(le)-1] |= 0x80
	}
	if len(le) > 75 {
		panic("propPushBig: too long")
	}
	return append([]byte{byte(len(le))}, le...)
}

// propSats converts an amount to an output value; anything not representable
// in uint64 becomes -1 (such a case is always expected to fail).
func propSats(x *big.Int) int64 {
	if x.Sign() < 0 || !x.IsUint64() {
		return -1
	}
	return int64(x.Uint64())
}

func propTokenPrefix(amount *big.Int) []byte {
	p := append([]byte{0x20}, assetID...)
	p = append(p, propPushBig(amount)...)
	return append(p, 0x6d)
}

// propAssetOut is a payout of amount: a 1-sat BRC-162 output when tokens,
// else a plain P2PKH of that many sats.
func propAssetOut(tokens bool, amount *big.Int, to []byte) out {
	if tokens {
		return out{1, append(propTokenPrefix(amount), p2pkh(to)...)}
	}
	return out{propSats(amount), p2pkh(to)}
}

// propPoolScript is a pool output's script: prefix for prefixAmt, the code,
// and the state with its TokenReserve bytes forced to raw.
func propPoolScript(f *fixture, p pool, prefixAmt *big.Int, raw uint64) []byte {
	s := propTokenPrefix(prefixAmt)
	s = append(s, f.code...)
	s = append(s, 0x6a)
	st := p.state()
	binary.LittleEndian.PutUint64(st[:8], raw)
	return append(s, st...)
}

func propRaw(x *big.Int) uint64 {
	m := new(big.Int).And(x, propPow2m1(64))
	return m.Uint64()
}

// --- the reference model ---

type propVals struct {
	ok                bool
	why               string
	net, out          *big.Int
	lp, val, com      *big.Int
	newBsv, newTokens *big.Int
	has               [3]bool // which fee outputs exist (fee > 0)
}

func (v propVals) clone() propVals {
	c := v
	for _, x := range []**big.Int{&c.net, &c.out, &c.lp, &c.val, &c.com, &c.newBsv, &c.newTokens} {
		*x = new(big.Int).Set(*x)
	}
	return c
}

func propCeilFee(in *big.Int, bps int64) *big.Int {
	x := propAdd(propMul(in, propBig(bps)), propBig(9999))
	return propQuo(x, propBig(10000))
}

// propModelSwap is the contract's Swap math with the contract's side
// conditions, in math/big. ok also folds in what must be representable: the
// token reserve is an 8-byte signed state field, and the contract's sats
// outputs (Num2Bin(sats, 8)) are at most MaxInt64.
func propModelSwap(bps [3]int64, bsv, tokens, in *big.Int, bsvIn bool) propVals {
	v := propVals{ok: true}
	v.lp = propCeilFee(in, bps[0])
	v.val = propCeilFee(in, bps[1])
	v.com = propCeilFee(in, bps[2])
	v.has = [3]bool{v.lp.Sign() > 0, v.val.Sign() > 0, v.com.Sign() > 0}
	v.net = propSub(propSub(propSub(in, v.lp), v.val), v.com)
	if bsvIn {
		v.out = propQuo(propMul(v.net, tokens), propAdd(bsv, v.net))
		v.newBsv, v.newTokens = propAdd(bsv, v.net), propSub(tokens, v.out)
	} else {
		v.out = propQuo(propMul(v.net, bsv), propAdd(tokens, v.net))
		v.newBsv, v.newTokens = propSub(bsv, v.out), propAdd(tokens, v.net)
	}
	fail := func(why string) {
		if v.ok {
			v.ok, v.why = false, why
		}
	}
	if in.Sign() <= 0 {
		fail("amountIn <= 0")
	}
	if v.net.Sign() <= 0 {
		fail("net <= 0")
	}
	if v.out.Sign() <= 0 {
		fail("out <= 0")
	}
	if v.newBsv.Sign() <= 0 {
		fail("newBsv <= 0")
	}
	if v.newTokens.Sign() <= 0 {
		fail("newTokens <= 0")
	}
	if v.newTokens.Cmp(propMaxInt64) > 0 {
		fail("TokenReserve exceeds the 8-byte state field")
	}
	if v.newBsv.Cmp(propMaxInt64) > 0 {
		fail("newBsv exceeds MaxInt64")
	}
	satsAmts := []*big.Int{v.lp, v.val, v.com}
	if !bsvIn {
		satsAmts = []*big.Int{v.out}
	}
	for _, a := range satsAmts {
		if a.Cmp(propMaxInt64) > 0 {
			fail("a sats output exceeds MaxInt64")
		}
	}
	return v
}

// propCheckInvariants asserts the AMM invariants on a model result with a
// positive net: k never decreases and the pool never pays out its reserve.
func propCheckInvariants(t *testing.T, bsv, tokens *big.Int, bsvIn bool, v propVals) {
	t.Helper()
	if v.net.Sign() <= 0 {
		return
	}
	k0 := propMul(bsv, tokens)
	k1 := propMul(v.newBsv, v.newTokens)
	if k1.Cmp(k0) < 0 {
		t.Fatalf("model: k decreased %v -> %v (bsv %v tokens %v net %v out %v)", k0, k1, bsv, tokens, v.net, v.out)
	}
	reserveOut := bsv
	if bsvIn {
		reserveOut = tokens
	}
	if v.out.Cmp(reserveOut) >= 0 {
		t.Fatalf("model: payout %v >= reserve %v", v.out, reserveOut)
	}
}

// --- fixtures and call building ---

var propFixtures = map[[3]int64]*fixture{}

func propFix(t *testing.T, l, v, c int64) *fixture {
	t.Helper()
	k := [3]int64{l, v, c}
	if f, ok := propFixtures[k]; ok {
		return f
	}
	f := newFixture(t, l, v, c)
	propFixtures[k] = f
	return f
}

func (f *fixture) propBps() [3]int64 { return [3]int64{f.lpFeeBps, f.validatorBps, f.commissionBps} }

// propOuts builds the swap's outputs from vals: pool, payout, the fees that
// exist, in the contract's order.
func propOuts(f *fixture, p pool, bsvIn bool, v propVals) []out {
	np := pool{lp: p.lp, validator: next(p.validator), satsPkh: p.satsPkh, tokenPkh: p.tokenPkh}
	outs := []out{{propSats(v.newBsv), propPoolScript(f, np, v.newTokens, propRaw(v.newTokens))}}
	outs = append(outs, propAssetOut(bsvIn, v.out, f.userPkh))
	fees := [3]*big.Int{v.lp, v.val, v.com}
	to := [3][]byte{p.lpFeePkh(bsvIn), pkh(p.validator), f.commissionPkh}
	for i := range fees {
		if v.has[i] {
			outs = append(outs, propAssetOut(!bsvIn, fees[i], to[i]))
		}
	}
	return outs
}

// propCall is a full swap call: outputs from vals, args from in, bsvIn and
// the token change tc (nil = 0). A positive tc adds its output last.
func propCall(f *fixture, p pool, in *big.Int, bsvIn bool, v propVals, tc *big.Int) call {
	if tc == nil {
		tc = propBig(0)
	}
	outs := propOuts(f, p, bsvIn, v)
	if tc.Sign() > 0 {
		outs = append(outs, out{1, append(propTokenPrefix(tc), p2pkh(f.changePkh)...)})
	}
	nv := next(p.validator)
	return call{
		method: methodSwap,
		args: [][]byte{nil, pushData(nv.PubKey().Compressed()), propPushBig(in), pushBool(bsvIn),
			pushData(f.userPkh), pushData(f.commissionPkh), pushData(f.changePkh), propPushBig(tc)},
		outs:     outs,
		signers:  []*ec.PrivateKey{p.validator},
		sigSlots: []int{0},
	}
}

func propMake(f *fixture, p pool, in *big.Int, bsvIn bool, tc *big.Int) (call, propVals) {
	v := propModelSwap(f.propBps(), propBig(p.bsv), propBig(p.tokens), in, bsvIn)
	return propCall(f, p, in, bsvIn, v, tc), v
}

// propExpect runs the honest-looking swap of in and requires both the model
// and the script to agree with want.
func propExpect(t *testing.T, f *fixture, p pool, in *big.Int, bsvIn bool, tc *big.Int, want bool) error {
	t.Helper()
	c, v := propMake(f, p, in, bsvIn, tc)
	if v.ok != want {
		t.Fatalf("test expectation wrong: model ok=%v (%s), want %v (bsv %d tokens %d in %v bsvIn %v)", v.ok, v.why, want, p.bsv, p.tokens, in, bsvIn)
	}
	err := f.run(t, p, c)
	if want && err != nil {
		t.Errorf("model says valid but the script rejected (bsv %d tokens %d in %v bsvIn %v): %v", p.bsv, p.tokens, in, bsvIn, err)
	}
	if !want && err == nil {
		t.Errorf("model says invalid (%s) but the script ACCEPTED (bsv %d tokens %d in %v bsvIn %v)", v.why, p.bsv, p.tokens, in, bsvIn)
	}
	return err
}

func propPool(t *testing.T, bsv, tokens int64) pool {
	return pool{bsv: bsv, tokens: tokens, lp: key(t, 10), validator: key(t, 20)}
}

func propLog(rng *rand.Rand, bits int) *big.Int {
	b := uint(rng.Intn(bits) + 1)
	x := new(big.Int).Rand(rng, propPow2(b))
	return x.Add(x, big.NewInt(1))
}

// --- 1. randomized differential test ---

var propConfigs = [][3]int64{{0, 0, 0}, {30, 5, 0}, {30, 5, 10}, {1, 1, 1}, {2500, 2500, 2500}, {9999, 0, 0}}

func TestPropSwapDifferential(t *testing.T) {
	const seed = 20261113
	const perConfig = 250
	t.Logf("seed %d, %d iterations per fee config", seed, perConfig)
	total, valid := 0, 0
	for _, cfg := range propConfigs {
		cfg := cfg
		t.Run(fmt.Sprintf("fees_%d_%d_%d", cfg[0], cfg[1], cfg[2]), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed + cfg[0]*7 + cfg[1]*13 + cfg[2]*31))
			f := propFix(t, cfg[0], cfg[1], cfg[2])
			for i := 0; i < perConfig; i++ {
				bsv, tokens := propLog(rng, 50), propLog(rng, 50)
				bsvIn := rng.Intn(2) == 0
				resIn := tokens
				if bsvIn {
					resIn = bsv
				}
				var in *big.Int
				if rng.Intn(4) == 0 {
					in = propLog(rng, 7)
				} else {
					in = propLog(rng, resIn.BitLen()+2)
				}
				p := pool{bsv: bsv.Int64(), tokens: tokens.Int64(), lp: key(t, 10), validator: key(t, byte(20+rng.Intn(100)))}
				desc := fmt.Sprintf("iter %d bsv %v tokens %v in %v bsvIn %v", i, bsv, tokens, in, bsvIn)
				v := propModelSwap(cfg, bsv, tokens, in, bsvIn)
				total++
				c := propCall(f, p, in, bsvIn, v, nil)
				err := f.run(t, p, c)
				if !v.ok {
					if err == nil {
						t.Errorf("%s: model invalid (%s) but script ACCEPTED", desc, v.why)
					}
					continue
				}
				valid++
				propCheckInvariants(t, bsv, tokens, bsvIn, v)
				// cross-check the int64 helper where it cannot overflow
				if in.BitLen() < 31 && bsv.BitLen() < 31 && tokens.BitLen() < 31 {
					r := f.expectSwap(p, in.Int64(), bsvIn)
					if r.out != v.out.Int64() || r.newBsv != v.newBsv.Int64() || r.newTokens != v.newTokens.Int64() {
						t.Fatalf("%s: model and expectSwap disagree: %+v vs out %v", desc, r, v.out)
					}
				}
				if err != nil {
					t.Errorf("%s: honest model outputs rejected: %v", desc, err)
					continue
				}
				// mutations, each must be rejected
				reject := func(name string, mv propVals) {
					if e := f.run(t, p, propCall(f, p, in, bsvIn, mv, nil)); e == nil {
						t.Errorf("%s: mutation %q ACCEPTED", desc, name)
					}
				}
				one := propBig(1)
				m := v.clone()
				m.out = propAdd(m.out, one)
				reject("payout+1", m)

				m = v.clone()
				if bsvIn {
					m.newTokens = propSub(m.newTokens, one)
				} else {
					m.newBsv = propSub(m.newBsv, one)
				}
				reject("continuation-1 in the paid-out asset", m)

				for fi, name := range []string{"lpFee", "validatorFee", "commission"} {
					if !v.has[fi] {
						continue
					}
					m = v.clone()
					fp := []*big.Int{m.lp, m.val, m.com}[fi]
					fp.Sub(fp, one)
					reject(name+"-1", m)
				}

				m = v.clone() // pool keeps one unit less, the user gets it
				m.out = propAdd(m.out, one)
				if bsvIn {
					m.newTokens = propSub(m.newTokens, one)
				} else {
					m.newBsv = propSub(m.newBsv, one)
				}
				reject("pool keeps one unit less, user gets one more", m)
			}
		})
	}
	t.Logf("%d iterations, %d model-valid", total, valid)
	if valid < total/3 {
		t.Errorf("only %d of %d iterations were model-valid: generator too lopsided", valid, total)
	}
}

// --- 2. long chains ---

func TestPropSwapChain(t *testing.T) {
	for _, cfg := range [][3]int64{{30, 5, 10}, {1, 1, 1}} {
		cfg := cfg
		t.Run(fmt.Sprintf("fees_%d_%d_%d", cfg[0], cfg[1], cfg[2]), func(t *testing.T) {
			rng := rand.New(rand.NewSource(424242 + cfg[0]))
			f := propFix(t, cfg[0], cfg[1], cfg[2])
			p := propPool(t, 5_000_000_000_000, 3_000_000_000_000)
			k := propMul(propBig(p.bsv), propBig(p.tokens))
			for step := 0; step < 200; step++ {
				var in *big.Int
				var bsvIn bool
				var v propVals
				for try := 0; ; try++ {
					if try > 100 {
						t.Fatalf("step %d: no valid swap found", step)
					}
					bsvIn = rng.Intn(2) == 0
					res := p.tokens
					if bsvIn {
						res = p.bsv
					}
					in = propAdd(new(big.Int).Rand(rng, propBig(res/5+1)), propBig(1))
					v = propModelSwap(cfg, propBig(p.bsv), propBig(p.tokens), in, bsvIn)
					if v.ok {
						break
					}
				}
				propCheckInvariants(t, propBig(p.bsv), propBig(p.tokens), bsvIn, v)
				c := propCall(f, p, in, bsvIn, v, nil)
				if err := f.run(t, p, c); err != nil {
					t.Fatalf("step %d (bsv %d tokens %d in %v bsvIn %v): %v", step, p.bsv, p.tokens, in, bsvIn, err)
				}
				nk := propMul(v.newBsv, v.newTokens)
				if nk.Cmp(k) < 0 {
					t.Fatalf("step %d: k decreased", step)
				}
				k = nk
				p = pool{bsv: v.newBsv.Int64(), tokens: v.newTokens.Int64(), lp: p.lp, validator: next(p.validator)}
			}
			t.Logf("after 200 swaps: bsv %d tokens %d", p.bsv, p.tokens)
			t.Run("close", func(t *testing.T) { mustPass(t, f.run(t, p, f.closeCall(p))) })
			t.Run("rescind then close", func(t *testing.T) {
				mustPass(t, f.run(t, p, f.rescindCall(p)))
				r := p
				r.rescinded = true
				mustPass(t, f.run(t, r, f.closeCall(r)))
			})
		})
	}
}

// --- 3. rounding edges ---

func propGcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func TestPropRoundingFeeBoundaries(t *testing.T) {
	for _, cfg := range propConfigs[1:] {
		f := propFix(t, cfg[0], cfg[1], cfg[2])
		for _, bsvIn := range []bool{true, false} {
			p := propPool(t, 1_000_000, 1_000_000)
			for fi, b := range cfg {
				if b == 0 {
					continue
				}
				unit := 10000 / propGcd(b, 10000)
				for _, mult := range []int64{1, 2, 7} {
					for _, d := range []int64{-1, 0, 1} {
						in := propBig(unit*mult + d)
						if in.Sign() <= 0 {
							continue
						}
						name := fmt.Sprintf("cfg %v fee#%d bps %d in %v bsvIn %v", cfg, fi, b, in, bsvIn)
						v := propModelSwap(cfg, propBig(p.bsv), propBig(p.tokens), in, bsvIn)
						exact := propMul(in, propBig(b))
						fee := []*big.Int{v.lp, v.val, v.com}[fi]
						if d == 0 {
							if new(big.Int).Mod(exact, propBig(10000)).Sign() != 0 || fee.Cmp(propQuo(exact, propBig(10000))) != 0 {
								t.Fatalf("%s: exact-division case not exact: fee %v", name, fee)
							}
						} else if d == 1 && fee.Cmp(propAdd(propQuo(exact, propBig(10000)), propBig(1))) != 0 {
							t.Fatalf("%s: one-above fee %v not floor+1", name, fee)
						}
						err := f.run(t, p, propCall(f, p, in, bsvIn, v, nil))
						if v.ok && err != nil {
							t.Errorf("%s: model valid, script rejected: %v", name, err)
						}
						if !v.ok && err == nil {
							t.Errorf("%s: model invalid (%s), script ACCEPTED", name, v.why)
						}
					}
				}
			}
		}
	}
}

func TestPropRoundingSmallAmounts(t *testing.T) {
	for _, cfg := range propConfigs {
		f := propFix(t, cfg[0], cfg[1], cfg[2])
		for _, bsvIn := range []bool{true, false} {
			// pools where any net >= 1 pays out >= 1
			p := propPool(t, 1, 1000)
			if !bsvIn {
				p = propPool(t, 1000, 1)
			}
			smallest := int64(0) // by the model
			for in := int64(1); in <= 20000; in++ {
				if propModelSwap(cfg, propBig(p.bsv), propBig(p.tokens), propBig(in), bsvIn).ok {
					smallest = in
					break
				}
			}
			if smallest == 0 {
				t.Errorf("cfg %v bsvIn %v: nothing up to 20000 passes", cfg, bsvIn)
				continue
			}
			// the script must agree on 1, 2, 3 and around the smallest
			for _, in := range []int64{1, 2, 3, smallest - 1, smallest, smallest + 1} {
				if in < 1 {
					continue
				}
				v := propModelSwap(cfg, propBig(p.bsv), propBig(p.tokens), propBig(in), bsvIn)
				err := f.run(t, p, propCall(f, p, propBig(in), bsvIn, v, nil))
				if v.ok != (err == nil) {
					t.Errorf("cfg %v bsvIn %v in %d: model ok=%v (%s) script err=%v", cfg, bsvIn, in, v.ok, v.why, err)
				}
				if in < smallest && err == nil {
					t.Errorf("cfg %v bsvIn %v: amountIn %d below the smallest passing %d was accepted", cfg, bsvIn, in, smallest)
				}
			}
			t.Logf("cfg %v bsvIn %-5v: smallest amountIn that passes = %d", cfg, bsvIn, smallest)
		}
	}
}

func TestPropRoundingOutZero(t *testing.T) {
	f := propFix(t, 0, 0, 0)
	// tiny net against a huge reserve: the payout floors to 0
	propExpect(t, f, propPool(t, 5, 1<<50), propBig(1), false, nil, false) // tokens in
	propExpect(t, f, propPool(t, 1<<50, 5), propBig(1), true, nil, false)  // sats in
	propExpect(t, f, propPool(t, 5, 1<<50), propBig(1<<40), false, nil, false)
	propExpect(t, f, propPool(t, 1<<50, 5), propBig(1<<40), true, nil, false)
	// the same pools at an amount that does pay out
	propExpect(t, f, propPool(t, 5, 1<<50), propBig(1<<50), false, nil, true)
	propExpect(t, f, propPool(t, 1<<50, 5), propBig(1<<50), true, nil, true)
}

// --- 4. token amount push boundaries ---

var propBoundaries = []struct {
	amount  *big.Int
	pushHex string // hand-written: the minimal push of the amount
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
}

// propNonMinimal lists encodings of the same amount that a minimal-push
// implementation must not produce.
func propNonMinimal(a *big.Int, minimal []byte) [][]byte {
	var alts [][]byte
	if a.Cmp(big.NewInt(16)) <= 0 {
		alts = append(alts, []byte{0x01, byte(a.Int64())})
		alts = append(alts, []byte{0x02, byte(a.Int64()), 0x00})
	} else {
		d := minimal[1:]
		alts = append(alts, append([]byte{byte(len(d) + 1)}, append(append([]byte{}, d...), 0x00)...))
	}
	if a.Cmp(big.NewInt(128)) == 0 {
		alts = append(alts, []byte{0x01, 0x80})
	}
	return alts
}

type propLoc struct {
	name  string
	build func(t *testing.T, a *big.Int) (f *fixture, p pool, in *big.Int, bsvIn bool, tc *big.Int, idx int, skip string)
}

func propFeeLoc(name string, which int) propLoc {
	return propLoc{name, func(t *testing.T, a *big.Int) (*fixture, pool, *big.Int, bool, *big.Int, int, string) {
		for _, bps := range []int64{5000, 9999} {
			var cfg [3]int64
			cfg[which] = bps
			f := propFix(t, cfg[0], cfg[1], cfg[2])
			p := propPool(t, 1_000_000_000, 1)
			// solve ceil(in*bps/10000) == a
			c0 := propQuo(propMul(a, propBig(10000)), propBig(bps))
			for d := int64(-3); d <= 3; d++ {
				in := propAdd(c0, propBig(d))
				if in.Sign() <= 0 || propCeilFee(in, bps).Cmp(a) != 0 {
					continue
				}
				if v := propModelSwap(cfg, propBig(p.bsv), propBig(p.tokens), in, false); v.ok {
					return f, p, in, false, nil, 2, ""
				}
			}
		}
		return nil, pool{}, nil, false, nil, 0, "no valid swap yields this fee"
	}}
}

var propLocs = []propLoc{
	{"payout (token out of a sats-in swap)", func(t *testing.T, a *big.Int) (*fixture, pool, *big.Int, bool, *big.Int, int, string) {
		if a.Cmp(propPow2m1(63)) >= 0 {
			return nil, pool{}, nil, false, nil, 0, "unreachable: payout < reserve <= 2^63-1"
		}
		// B=1, T=a+1, net=a: out = floor(a*(a+1)/(a+1)) = a
		return propFix(t, 0, 0, 0), propPool(t, 1, propAdd(a, propBig(1)).Int64()), a, true, nil, 1, ""
	}},
	propFeeLoc("LP fee (token-in)", 0),
	propFeeLoc("validator fee (token-in)", 1),
	propFeeLoc("commission (token-in)", 2),
	{"token change", func(t *testing.T, a *big.Int) (*fixture, pool, *big.Int, bool, *big.Int, int, string) {
		return propFix(t, 0, 0, 0), propPool(t, 1_000_000, 1_000_000), propBig(100), true, a, 2, ""
	}},
	{"pool reserve prefix", func(t *testing.T, a *big.Int) (*fixture, pool, *big.Int, bool, *big.Int, int, string) {
		f := propFix(t, 0, 0, 0)
		if a.Cmp(propBig(1)) == 0 { // sats in: T=2, B=1, net=1 -> out 1, newTokens 1
			return f, propPool(t, 1, 2), propBig(1), true, nil, 0, ""
		}
		// token in: T=1, net=a-1 -> newTokens = a
		return f, propPool(t, 1_000_000_000, 1), propSub(a, propBig(1)), false, nil, 0, ""
	}},
}

func TestPropTokenPushBoundaries(t *testing.T) {
	for _, loc := range propLocs {
		loc := loc
		t.Run(loc.name, func(t *testing.T) {
			for _, bd := range propBoundaries {
				a := bd.amount
				f, p, in, bsvIn, tc, idx, skip := loc.build(t, a)
				if skip != "" {
					t.Logf("amount %v skipped: %s", a, skip)
					continue
				}
				c, v := propMake(f, p, in, bsvIn, tc)
				if !v.ok {
					t.Fatalf("amount %v: scenario invalid in the model: %s", a, v.why)
				}
				push, _ := hex.DecodeString(bd.pushHex)
				old := c.outs[idx].script
				prefixLen := 1 + 32 + len(propPushBig(a)) + 1
				if a.IsInt64() && !bytes.Equal(old[:prefixLen], tokenPrefix(a.Int64())) {
					t.Fatalf("amount %v: tokenPrefix helper disagrees with the contract-side build", a)
				}
				// the expected prefix from the hand-written table only
				idHex := "20" + strings.Repeat("a5", 32)
				want, _ := hex.DecodeString(idHex + bd.pushHex + "6d")
				if !bytes.Equal(old[:prefixLen], want) {
					t.Fatalf("amount %v: encoder %x differs from the table %x", a, old[:prefixLen], want)
				}
				_ = push
				rest := old[prefixLen:]
				withPrefix := func(pre []byte) call {
					cc := c
					cc.outs = append([]out{}, c.outs...)
					cc.outs[idx] = out{c.outs[idx].sats, append(append([]byte{}, pre...), rest...)}
					return cc
				}
				if err := f.run(t, p, withPrefix(want)); err != nil {
					t.Errorf("amount %v (%s): table encoding rejected: %v", a, bd.pushHex, err)
				}
				for _, alt := range propNonMinimal(a, push) {
					pre, _ := hex.DecodeString(idHex)
					pre = append(append(pre, alt...), 0x6d)
					if err := f.run(t, p, withPrefix(pre)); err == nil {
						t.Errorf("amount %v: non-minimal push %x ACCEPTED", a, alt)
					}
				}
			}
		})
	}
}

// --- 5. reserve limits ---

func TestPropReserveLimits(t *testing.T) {
	f := propFix(t, 0, 0, 0)
	type tc struct {
		name        string
		bsv, tokens int64
		in          *big.Int
		bsvIn       bool
		want        bool
	}
	cases := []tc{
		// sats in: the pool can never hand out its last token
		{"sats-in, tokens 1: can never pay (small)", 1, 1, propBig(1_000_000), true, false},
		{"sats-in, tokens 1: can never pay (huge)", 1, 1, propBig(1 << 50), true, false},
		{"sats-in, tokens 1, bsv 2^50: never pays", 1 << 50, 1, propBig(1 << 50), true, false},
		{"sats-in, tokens 2 bsv 1: leaves 1", 1, 2, propBig(1), true, true},
		{"sats-in, tokens 2 bsv 1, big in: still leaves 1", 1, 2, propBig(100), true, true},
		{"sats-in, tokens 2 bsv 2, in 1: out floors to 0", 2, 2, propBig(1), true, false},
		{"sats-in, tokens 2 bsv 2, in 2", 2, 2, propBig(2), true, true},
		// token in: newBsv = 1
		{"token-in leaves newBsv 1", 2, 1, propBig(1), false, true},
		{"token-in, bsv 1: payout floors to 0 (small)", 1, 1, propBig(1), false, false},
		{"token-in, bsv 1: payout floors to 0 (huge)", 1, 1, propBig(1 << 40), false, false},
		// TokenReserve vs the 8-byte state field
		{"token-in: reserve lands on 2^63-1", 1 << 62, 1 << 62, propPow2m1(62), false, true},
		{"token-in: reserve lands on 2^63", 1 << 62, 1 << 62, propPow2(62), false, false},
		{"token-in: from 2^63-1 by 2^62", 1 << 62, math.MaxInt64, propPow2(62), false, false},
		{"token-in: lands on 2^63-1 from below", 1 << 62, math.MaxInt64 - (1 << 40), propPow2(40), false, true},
		{"token-in: one past 2^63-1", 1 << 62, math.MaxInt64 - (1 << 40) + 1, propPow2(40), false, false},
		// bsv reserve magnitudes
		{"bsv 2^53-1 sats-in", 1<<53 - 1, 1_000_000_000_000, propBig(1_000_000_000), true, true},
		{"bsv 2^53 sats-in", 1 << 53, 1_000_000_000_000, propBig(1_000_000_000), true, true},
		{"bsv 2^53+1 sats-in", 1<<53 + 1, 1_000_000_000_000, propBig(1_000_000_000), true, true},
		{"bsv 21e14 sats-in", 21_000_000_000_000_00, 1_000_000_000_000, propBig(1_000_000_000), true, true},
		{"bsv 2^53-1 token-in", 1<<53 - 1, 1_000_000_000_000, propBig(1_000_000_000), false, true},
		{"bsv 2^53 token-in", 1 << 53, 1_000_000_000_000, propBig(1_000_000_000), false, true},
		{"bsv 2^53+1 token-in", 1<<53 + 1, 1_000_000_000_000, propBig(1_000_000_000), false, true},
		{"bsv 21e14 token-in", 21_000_000_000_000_00, 1_000_000_000_000, propBig(1_000_000_000), false, true},
		{"bsv 21e14 sats-in, in 2^52", 21_000_000_000_000_00, 1_000_000_000_000, propPow2(52), true, true},
		// bsv reserve 1
		{"bsv 1, token-in: out 0", 1, 1000, propBig(1000), false, false},
		{"bsv 1, sats-in: pays 999", 1, 1000, propBig(1000), true, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			propExpect(t, f, propPool(t, c.bsv, c.tokens), c.in, c.bsvIn, nil, c.want)
		})
	}
}

// ... and the sats-in swap that would move newBsv past MaxInt64.
func TestPropReserveOverflowSats(t *testing.T) {
	f := propFix(t, 0, 0, 0)
	p := propPool(t, 1<<62, 1<<62)
	c, v := propMake(f, p, propPow2(62), true, nil) // newBsv = 2^63
	if v.ok {
		t.Fatalf("model should reject: %s", v.why)
	}
	mustFail(t, f.run(t, p, c))
}

// --- 6. amountIn domain ---

func TestPropAmountInDomain(t *testing.T) {
	f := propFix(t, 30, 5, 10)
	for _, bsvIn := range []bool{true, false} {
		p := propPool(t, 1_000_000, 5_000_000)
		base, v := propMake(f, p, propBig(20_000), bsvIn, nil)
		if !v.ok {
			t.Fatal(v.why)
		}
		for _, bad := range []*big.Int{propBig(0), propBig(-1), propBig(-2), new(big.Int).Neg(propPow2(31))} {
			c := base
			c.args = append([][]byte{}, base.args...)
			c.args[2] = propPushBig(bad)
			if err := f.run(t, p, c); err == nil {
				t.Errorf("bsvIn %v: amountIn %v ACCEPTED", bsvIn, bad)
			}
		}
	}

	// huge amountIn, with reserves and a 9999 bps fee so only the net enters
	f9 := propFix(t, 9999, 0, 0)
	fz := propFix(t, 0, 0, 0)
	huge := []struct {
		name  string
		in    *big.Int
		f     *fixture
		bsvIn bool
		want  bool
	}{
		{"2^63-1 token-in, 9999bps", propPow2m1(63), f9, false, true},
		{"2^64 token-in, 9999bps", propPow2(64), f9, false, true},
		{"2^100 token-in, 9999bps (net overflows TokenReserve)", propPow2(100), f9, false, false},
		{"2^63-1 sats-in, 9999bps", propPow2m1(63), f9, true, true},
		{"2^64 sats-in, 9999bps (fee > MaxInt64 sats)", propPow2(64), f9, true, false},
		{"2^100 sats-in, 9999bps", propPow2(100), f9, true, false},
		{"2^63-1 token-in, no fees (TokenReserve overflows)", propPow2m1(63), fz, false, false},
		{"2^63-1 sats-in, no fees (newBsv overflows)", propPow2m1(63), fz, true, false},
		{"2^64 token-in, no fees", propPow2(64), fz, false, false},
		{"2^100 sats-in, no fees", propPow2(100), fz, true, false},
	}
	for _, h := range huge {
		h := h
		t.Run(h.name, func(t *testing.T) {
			p := propPool(t, 1_000_000_000_000_000, 1_000_000_000_000_000)
			p.tokens = 1_000_000_000_000_000
			err := propExpect(t, h.f, p, h.in, h.bsvIn, nil, h.want)
			if err != nil {
				t.Logf("rejected: %v", err)
			}
		})
	}

	// tokenChange
	p := propPool(t, 1_000_000, 5_000_000)
	for _, bsvIn := range []bool{true, false} {
		for _, tcv := range []*big.Int{propBig(-1), propBig(-5), new(big.Int).Neg(propPow2(63))} {
			base, _ := propMake(f, p, propBig(20_000), bsvIn, nil)
			base.args = append([][]byte{}, base.args...)
			base.args[7] = propPushBig(tcv)
			if err := f.run(t, p, base); err == nil {
				t.Errorf("bsvIn %v: negative tokenChange %v ACCEPTED", bsvIn, tcv)
			}
		}
		for _, tcv := range []*big.Int{propPow2m1(63), propPow2(64), propPow2(100)} {
			c, v := propMake(f, p, propBig(20_000), bsvIn, tcv)
			if !v.ok {
				t.Fatal(v.why)
			}
			if err := f.run(t, p, c); err != nil {
				t.Errorf("bsvIn %v: honest tokenChange %v with its output rejected: %v", bsvIn, tcv, err)
			}
			// its output missing
			c.outs = c.outs[:len(c.outs)-1]
			if err := f.run(t, p, c); err == nil {
				t.Errorf("bsvIn %v: tokenChange %v with no output ACCEPTED", bsvIn, tcv)
			}
		}
	}
}

// --- 7. OP_PUSH_TX robustness ---

func propOutsHex(outs []out) string {
	var parts []string
	for _, o := range outs {
		parts = append(parts, fmt.Sprintf("%d:%x", o.sats, o.script))
	}
	return strings.Join(parts, " | ")
}

func propRandScript(rng *rand.Rand) []byte {
	b := make([]byte, 20)
	rng.Read(b)
	switch rng.Intn(3) {
	case 0:
		return p2pkh(b)
	case 1:
		return tokenP2pkh(int64(rng.Intn(1<<30)+1), b)
	}
	s := make([]byte, 1+rng.Intn(80))
	rng.Read(s)
	return s
}

func TestPropPushTxRobustness(t *testing.T) {
	const n = 1000
	f := propFix(t, 30, 5, 10)
	rng := rand.New(rand.NewSource(77))
	t.Logf("%d Close and %d Rescind spends", n, n)

	var fails []string
	record := func(kind string, i int, p pool, outs []out, err error) {
		fails = append(fails, fmt.Sprintf("%s #%d: %v\n  pool bsv %d tokens %d lp %x validator %x\n  outputs (sats:script): %s",
			kind, i, err, p.bsv, p.tokens, p.lp.PubKey().Compressed(), p.validator.PubKey().Compressed(), propOutsHex(outs)))
	}
	for i := 0; i < n; i++ {
		p := pool{bsv: 1 + rng.Int63n(1<<50), tokens: 1 + rng.Int63n(1<<50),
			lp: key(t, byte(1+rng.Intn(200))), validator: key(t, byte(1+rng.Intn(200)))}
		var outs []out
		for j := 0; j <= rng.Intn(4); j++ {
			outs = append(outs, out{rng.Int63n(1 << 40), propRandScript(rng)})
		}
		c := f.closeCall(p)
		c.outs = outs
		if err := f.run(t, p, c); err != nil {
			record("close", i, p, outs, err)
		}
	}
	for i := 0; i < n; i++ {
		p := pool{bsv: 1 + rng.Int63n(1<<50), tokens: 1 + rng.Int63n(1<<50),
			lp: key(t, byte(1+rng.Intn(200))), validator: key(t, byte(1+rng.Intn(200)))}
		c := f.rescindCall(p)
		chPkh := make([]byte, 20)
		rng.Read(chPkh)
		sats := 1 + rng.Int63n(1<<40)
		c.outs = []out{c.outs[0], {sats, p2pkh(chPkh)}}
		c.change = &struct {
			pkh  []byte
			sats int64
		}{chPkh, sats}
		if err := f.run(t, p, c); err != nil {
			record("rescind", i, p, c.outs, err)
		}
	}
	if len(fails) > 0 {
		show := fails
		if len(show) > 5 {
			show = show[:5]
		}
		t.Errorf("%d of %d honest spends failed. First %d:\n%s", len(fails), 2*n, len(show), strings.Join(show, "\n"))
	}
}

// --- 8. Rúnar's sats change on Swap and Rescind ---

type propChange = struct {
	pkh  []byte
	sats int64
}

func TestPropSwapSatsChange(t *testing.T) {
	f := propFix(t, 30, 5, 10)
	p := propPool(t, 1_000_000, 5_000_000)
	chPkh := pkh(key(t, 70))
	other := pkh(key(t, 71))
	for _, bsvIn := range []bool{true, false} {
		name := map[bool]string{true: "bsv in", false: "tokens in"}[bsvIn]
		base, v := propMake(f, p, propBig(20_000), bsvIn, nil)
		if !v.ok {
			t.Fatal(v.why)
		}
		withOut := func(sats int64) call {
			c := base
			c.outs = append(append([]out{}, base.outs...), out{sats, p2pkh(chPkh)})
			c.change = &propChange{chPkh, sats}
			return c
		}
		t.Run(name+": change 0, no output", func(t *testing.T) { mustPass(t, f.run(t, p, base)) })
		for _, s := range []int64{1, 546, 1_000_000_000_000} {
			s := s
			t.Run(fmt.Sprintf("%s: change %d", name, s), func(t *testing.T) { mustPass(t, f.run(t, p, withOut(s))) })
		}
		t.Run(name+": output present, amount arg 0", func(t *testing.T) {
			c := base
			c.outs = append(append([]out{}, base.outs...), out{546, p2pkh(chPkh)})
			mustFail(t, f.run(t, p, c))
		})
		t.Run(name+": output present, amount arg 0, pkh arg set", func(t *testing.T) {
			c := base
			c.outs = append(append([]out{}, base.outs...), out{546, p2pkh(chPkh)})
			c.change = &propChange{chPkh, 0}
			mustFail(t, f.run(t, p, c))
		})
		t.Run(name+": change arg set, output missing", func(t *testing.T) {
			c := base
			c.change = &propChange{chPkh, 546}
			mustFail(t, f.run(t, p, c))
		})
		t.Run(name+": two change outputs", func(t *testing.T) {
			c := withOut(546)
			c.outs = append(c.outs, out{546, p2pkh(chPkh)})
			mustFail(t, f.run(t, p, c))
		})
		t.Run(name+": change pkh mismatch", func(t *testing.T) {
			c := withOut(546)
			c.outs[len(c.outs)-1].script = p2pkh(other)
			mustFail(t, f.run(t, p, c))
		})
		t.Run(name+": change amount mismatch", func(t *testing.T) {
			c := withOut(546)
			c.outs[len(c.outs)-1].sats = 547
			mustFail(t, f.run(t, p, c))
		})
	}
}

func TestPropRescindSatsChange(t *testing.T) {
	f := propFix(t, 30, 5, 10)
	p := propPool(t, 1_000_000, 5_000_000)
	chPkh := pkh(key(t, 70))
	other := pkh(key(t, 71))
	base := f.rescindCall(p)
	pooled := base.outs[0]
	with := func(sats int64) call {
		c := base
		c.outs = []out{pooled, {sats, p2pkh(chPkh)}}
		c.change = &propChange{chPkh, sats}
		return c
	}
	t.Run("change 0, no output", func(t *testing.T) {
		c := base
		c.outs, c.change = []out{pooled}, nil
		mustPass(t, f.run(t, p, c))
	})
	for _, s := range []int64{1, 546, 1_000_000_000_000} {
		s := s
		t.Run(fmt.Sprintf("change %d", s), func(t *testing.T) { mustPass(t, f.run(t, p, with(s))) })
	}
	t.Run("output present, amount arg 0", func(t *testing.T) {
		c := base
		c.outs, c.change = []out{pooled, {546, p2pkh(chPkh)}}, nil
		mustFail(t, f.run(t, p, c))
	})
	t.Run("output present, amount arg 0, pkh arg set", func(t *testing.T) {
		c := base
		c.outs, c.change = []out{pooled, {546, p2pkh(chPkh)}}, &propChange{chPkh, 0}
		mustFail(t, f.run(t, p, c))
	})
	t.Run("change arg set, output missing", func(t *testing.T) {
		c := base
		c.outs, c.change = []out{pooled}, &propChange{chPkh, 546}
		mustFail(t, f.run(t, p, c))
	})
	t.Run("two change outputs", func(t *testing.T) {
		c := with(546)
		c.outs = append(c.outs, out{546, p2pkh(chPkh)})
		mustFail(t, f.run(t, p, c))
	})
	t.Run("change pkh mismatch", func(t *testing.T) {
		c := with(546)
		c.outs[1].script = p2pkh(other)
		mustFail(t, f.run(t, p, c))
	})
	t.Run("change amount mismatch", func(t *testing.T) {
		c := with(546)
		c.outs[1].sats = 547
		mustFail(t, f.run(t, p, c))
	})
}
