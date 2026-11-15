package pair

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"math/rand"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/icellan/runar/compilers/go/compiler"
	runar "github.com/icellan/runar/packages/runar-go"
)

// These tests hand-build transactions and run BOTH contract inputs, the pool
// (input 0) and its vault (input 1), through the go-sdk interpreter with the
// real BIP-143 preimages. The harness mirrors ../pool_test.go.

const (
	methodSwap = iota
	methodClose
	methodRescind
)

var (
	assetA = bytes.Repeat([]byte{0xa5}, 32)
	assetB = bytes.Repeat([]byte{0xb6}, 32)
	nonce  = bytes.Repeat([]byte{0x4e}, 32)
	// The affirmation every pool carries in state: stand-ins for the
	// validator's signature's r and s (the contract never checks them).
	affirmationR      = bytes.Repeat([]byte{0x72}, 32)
	affirmationS      = bytes.Repeat([]byte{0x73}, 32)
	validatorIdentity = key(0x7f)
	lpFeeAPkh         = bytes.Repeat([]byte{0x66}, 20)
	lpFeeBPkh         = bytes.Repeat([]byte{0x77}, 20)
	userPkh           = bytes.Repeat([]byte{0x33}, 20)
	commissionPkh     = bytes.Repeat([]byte{0x44}, 20)
	changePkh         = bytes.Repeat([]byte{0x55}, 20)
)

func key(n byte) *ec.PrivateKey {
	k, _ := ec.PrivateKeyFromBytes(bytes.Repeat([]byte{n}, 32))
	return k
}

func pkh(k *ec.PrivateKey) []byte { return k.PubKey().Hash() }

func scriptNum(n int64) []byte {
	var b []byte
	for n > 0 {
		b = append(b, byte(n&0xff))
		n >>= 8
	}
	if len(b) > 0 && b[len(b)-1]&0x80 != 0 {
		b = append(b, 0)
	}
	return b
}

func pushNum(n int64) []byte {
	switch {
	case n == 0:
		return []byte{0x00}
	case n > 0 && n <= 16:
		return []byte{byte(0x50 + n)}
	case n < 0:
		panic("negative push")
	}
	b := scriptNum(n)
	return append([]byte{byte(len(b))}, b...)
}

func pushData(b []byte) []byte {
	s, _ := hex.DecodeString(runar.EncodePushData(hex.EncodeToString(b)))
	return s
}

func pushBool(v bool) []byte {
	if v {
		return []byte{0x51}
	}
	return []byte{0x00}
}

// tokenPrefix mirrors the contract's: amount 0 is the byte 0x50 there
// (Clamp(amount, 0, 16) + 80), a script it builds but never outputs.
func tokenPrefix(asset []byte, amount int64) []byte {
	p := append([]byte{0x20}, asset...)
	if amount == 0 {
		p = append(p, 0x50)
	} else {
		p = append(p, pushNum(amount)...)
	}
	return append(p, 0x6d)
}

func p2pkh(h []byte) []byte {
	return append(append([]byte{0x76, 0xa9, 0x14}, h...), 0x88, 0xac)
}

func tokenP2pkh(asset []byte, amount int64, h []byte) []byte {
	return append(tokenPrefix(asset, amount), p2pkh(h)...)
}

// pair is a pool and its vault.
type pair struct {
	sats          int64 // the pool output's sats
	a, b          int64
	lp, validator *ec.PrivateKey
	rescinded     bool
}

func (p pair) state() []byte {
	s := make([]byte, 16)
	binary.LittleEndian.PutUint64(s, uint64(p.a))
	binary.LittleEndian.PutUint64(s[8:], uint64(p.b))
	s = append(s, p.lp.PubKey().Compressed()...)
	if p.rescinded {
		s = append(s, make([]byte, 33)...)
	} else {
		s = append(s, p.validator.PubKey().Compressed()...)
	}
	s = append(s, validatorIdentity.PubKey().Compressed()...)
	s = append(s, nonce...)
	s = append(s, lpFeeAPkh...)
	s = append(s, lpFeeBPkh...)
	s = append(s, affirmationR...)
	return append(s, affirmationS...)
}

type fixture struct {
	code  []byte // the pool's code with its readonly args (no prefix, no state)
	vault []byte // the vault's code
	bps   [3]int64
	// the two asset ids compiled into the code (assetA and assetB unless a
	// test deploys others)
	idA, idB []byte
}

func artifact(t testing.TB, src string) *runar.RunarArtifact {
	t.Helper()
	art, err := compiler.CompileFromSource(src)
	if err != nil {
		t.Fatal(err)
	}
	js, err := compiler.ArtifactToJSON(art)
	if err != nil {
		t.Fatal(err)
	}
	var sa runar.RunarArtifact
	if err := json.Unmarshal(js, &sa); err != nil {
		t.Fatal(err)
	}
	return &sa
}

func newFixture(t testing.TB, lpBps, valBps, comBps int64) *fixture {
	t.Helper()
	return newFixtureIDs(t, assetA, assetB, lpBps, valBps, comBps)
}

// newFixtureIDs is newFixture for a pair of the two given asset ids.
func newFixtureIDs(t testing.TB, idA, idB []byte, lpBps, valBps, comBps int64) *fixture {
	t.Helper()
	vault, _ := hex.DecodeString(runar.NewRunarContract(artifact(t, "Vault.runar.go"), nil).GetLockingScript())
	vaultHash := sha256.Sum256(vault)
	p := pair{sats: 1, a: 1000, b: 1000, lp: key(1), validator: key(2)}
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
		lpBps, valBps, comBps,
	})
	full, _ := hex.DecodeString(c.GetLockingScript())
	tail := append([]byte{0x6a}, p.state()...)
	if !bytes.HasSuffix(full, tail) {
		t.Fatalf("SDK state layout differs from pair.state(): %x", full[len(full)-len(tail):])
	}
	return &fixture{code: full[:len(full)-len(tail)], vault: vault, bps: [3]int64{lpBps, valBps, comBps}, idA: idA, idB: idB}
}

func (f *fixture) poolLock(p pair) []byte {
	s := append(tokenPrefix(f.idA, p.a), f.code...)
	s = append(s, 0x6a)
	return append(s, p.state()...)
}

func (f *fixture) vaultLock(p pair) []byte {
	return append(tokenPrefix(f.idB, p.b), f.vault...)
}

type out struct {
	sats   int64
	script []byte
}

// in is one transaction input. lock nil: not a contract input here.
type in struct {
	txid byte // the outpoint's txid is 32 of this byte
	vout uint32
	sats int64
	lock []byte
}

// spend is one transaction: its inputs, outputs and how the pool input
// (ins[poolIdx]) and the vault input (ins[vaultIdx]) are unlocked. An index
// of -1 skips that execution.
type spend struct {
	ins      []in
	outs     []out
	poolIdx  int
	vaultIdx int

	method   int
	signer   *ec.PrivateKey // signs the pool input; its sig is args[0]
	args     [][]byte       // the method's declared args after the signature and before allPrevouts
	change   *out           // Rúnar's change (script = the pkh), Swap and Rescind
	prevouts []byte         // allPrevouts for the pool; nil: the real ones
	vaultArg []byte         // the vaultCode argument (Swap, Rescind); nil: the vault's code
	sigFlag  sighash.Flag   // the pool signature's sighash type; 0: ALL|FORKID

	vaultPrevouts []byte       // allPrevouts for the vault; nil: the real ones
	vaultSighash  sighash.Flag // 0: ALL|FORKID
	// forgeVaultPre, when set, replaces the vault's real preimage.
	forgeVaultPre func(real []byte) []byte
}

// afterSeparator is a contract lock's scriptCode: what follows its token
// prefix and the two bytes ending in OP_CODESEPARATOR (pool: 61 ab; vault:
// 76 ab).
func afterSeparator(lock []byte) []byte {
	off := 1 + 32
	if op := lock[off]; op >= 0x51 && op <= 0x60 {
		off++
	} else {
		off += 1 + int(op)
	}
	off++ // OP_2DROP
	if lock[off+1] != 0xab {
		panic("separator not where expected")
	}
	return lock[off+2:]
}

func outpoint(i in) []byte {
	b := bytes.Repeat([]byte{i.txid}, 32)
	return binary.LittleEndian.AppendUint32(b, i.vout)
}

type result struct{ pool, vault error }

func (r result) ok() bool { return r.pool == nil && r.vault == nil }

func (f *fixture) run(t testing.TB, s spend) result {
	t.Helper()
	tx := transaction.NewTransaction()
	var real []byte
	for _, i := range s.ins {
		h, _ := chainhash.NewHash(bytes.Repeat([]byte{i.txid}, 32))
		tx.AddInput(&transaction.TransactionInput{SourceTXID: h, SourceTxOutIndex: i.vout, SequenceNumber: 0xffffffff})
		real = append(real, outpoint(i)...)
	}
	for _, o := range s.outs {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: uint64(o.sats), LockingScript: script.NewFromBytes(o.script)})
	}
	preimage := func(idx int, flag sighash.Flag) []byte {
		cp := tx.ShallowClone()
		i := s.ins[idx]
		cp.Inputs[idx].SetSourceTxOutput(&transaction.TransactionOutput{Satoshis: uint64(i.sats), LockingScript: script.NewFromBytes(afterSeparator(i.lock))})
		pre, err := cp.CalcInputPreimage(uint32(idx), flag)
		if err != nil {
			t.Fatal(err)
		}
		return pre
	}
	exec := func(idx int) error {
		i := s.ins[idx]
		return interpreter.NewEngine().Execute(
			interpreter.WithTx(tx, idx, &transaction.TransactionOutput{Satoshis: uint64(i.sats), LockingScript: script.NewFromBytes(i.lock)}),
			interpreter.WithForkID(),
			interpreter.WithAfterGenesis(),
			interpreter.WithAfterChronicle(),
		)
	}

	var r result
	if s.poolIdx >= 0 {
		pre := preimage(s.poolIdx, sighash.AllForkID)
		// The signer signs the digest of its own sighash type (the script's
		// OP_CHECKSIG hashes by the signature's last byte).
		sigFlag := s.sigFlag
		if sigFlag == 0 {
			sigFlag = sighash.AllForkID
		}
		h := sha256.Sum256(preimage(s.poolIdx, sigFlag))
		d := sha256.Sum256(h[:])
		sig, err := s.signer.Sign(d[:])
		if err != nil {
			t.Fatal(err)
		}
		prevouts := s.prevouts
		if prevouts == nil {
			prevouts = real
		}
		var u []byte
		if s.method != methodClose {
			u = pushData(f.code) // _codePart: the methods that write a continuation
		}
		u = append(u, pushData(append(sig.Serialize(), byte(sigFlag)))...)
		for _, a := range s.args {
			u = append(u, a...)
		}
		u = append(u, pushData(prevouts)...)
		if s.method != methodClose {
			vaultArg := s.vaultArg
			if vaultArg == nil {
				vaultArg = f.vault
			}
			u = append(u, pushData(vaultArg)...)
			cp, ca := make([]byte, 20), int64(0)
			if s.change != nil {
				cp, ca = s.change.script, s.change.sats
			}
			u = append(u, pushData(cp)...)
			u = append(u, pushNum(ca)...)
		}
		u = append(u, pushData(pre)...)
		u = append(u, pushNum(int64(s.method))...)
		tx.Inputs[s.poolIdx].UnlockingScript = script.NewFromBytes(u)
		r.pool = exec(s.poolIdx)
	}
	if s.vaultIdx >= 0 {
		flag := s.vaultSighash
		if flag == 0 {
			flag = sighash.AllForkID
		}
		prevouts := s.vaultPrevouts
		if prevouts == nil {
			prevouts = real
		}
		pre := preimage(s.vaultIdx, flag)
		if s.forgeVaultPre != nil {
			pre = s.forgeVaultPre(pre)
		}
		u := pushData(prevouts)
		u = append(u, pushData(pre)...)
		tx.Inputs[s.vaultIdx].UnlockingScript = script.NewFromBytes(u)
		r.vault = exec(s.vaultIdx)
	}
	return r
}

// --- the reference model (math/big; independent of the contract) ---

type swapResult struct {
	out, lpFee, valFee, com int64
	after                   pair
	ok                      bool // the model accepts the swap
}

func ceilFee(amountIn, bps int64) *big.Int {
	x := new(big.Int).Mul(big.NewInt(amountIn), big.NewInt(bps))
	x.Add(x, big.NewInt(9999))
	return x.Quo(x, big.NewInt(10000)) // truncating toward zero, as the script's division
}

func (f *fixture) model(p pair, amountIn int64, aIn bool, nextValidator *ec.PrivateKey) swapResult {
	lp, val, com := ceilFee(amountIn, f.bps[0]), ceilFee(amountIn, f.bps[1]), ceilFee(amountIn, f.bps[2])
	net := new(big.Int).Sub(big.NewInt(amountIn), lp)
	net.Sub(net, val).Sub(net, com)
	rin, rout := big.NewInt(p.a), big.NewInt(p.b)
	if !aIn {
		rin, rout = rout, rin
	}
	r := swapResult{lpFee: lp.Int64(), valFee: val.Int64(), com: com.Int64(), after: p}
	if net.Sign() <= 0 {
		return r
	}
	o := new(big.Int).Mul(net, rout)
	o.Div(o, new(big.Int).Add(rin, net))
	newIn, newOut := new(big.Int).Add(rin, net), new(big.Int).Sub(rout, o)
	if o.Sign() <= 0 || newOut.Sign() <= 0 || !newIn.IsInt64() {
		return r
	}
	// k never decreases.
	if new(big.Int).Mul(newIn, newOut).Cmp(new(big.Int).Mul(rin, rout)) < 0 {
		panic("model: k decreased")
	}
	r.out, r.ok = o.Int64(), true
	r.after.a, r.after.b = newIn.Int64(), newOut.Int64()
	if !aIn {
		r.after.a, r.after.b = newOut.Int64(), newIn.Int64()
	}
	r.after.validator = nextValidator
	return r
}

// honest inputs: the pool at (0x11, 0), its vault at (0x11, 1), the taker's
// funding at (0x22, 0).
func (f *fixture) ins(p pair) []in {
	return []in{
		{txid: 0x11, vout: 0, sats: p.sats, lock: f.poolLock(p)},
		{txid: 0x11, vout: 1, sats: 1, lock: f.vaultLock(p)},
		{txid: 0x22, vout: 0, sats: 100_000},
	}
}

func (f *fixture) swap(p pair, amountIn int64, aIn bool) (spend, swapResult) {
	r := f.model(p, amountIn, aIn, key(p.validator.Serialize()[0]+1))
	inAsset, outAsset, lpPkh := assetA, assetB, lpFeeAPkh
	if !aIn {
		inAsset, outAsset, lpPkh = assetB, assetA, lpFeeBPkh
	}
	outs := []out{
		{p.sats, f.poolLock(r.after)},
		{1, f.vaultLock(r.after)},
		{1, tokenP2pkh(outAsset, r.out, userPkh)},
	}
	if r.lpFee > 0 {
		outs = append(outs, out{1, tokenP2pkh(inAsset, r.lpFee, lpPkh)})
	}
	if r.valFee > 0 {
		outs = append(outs, out{1, tokenP2pkh(inAsset, r.valFee, pkh(p.validator))})
	}
	if r.com > 0 {
		outs = append(outs, out{1, tokenP2pkh(inAsset, r.com, commissionPkh)})
	}
	return spend{
		ins: f.ins(p), outs: outs, poolIdx: 0, vaultIdx: 1,
		method: methodSwap, signer: p.validator,
		args: [][]byte{
			pushData(r.after.validator.PubKey().Compressed()),
			pushNum(amountIn),
			pushBool(aIn),
			pushData(userPkh),
			pushData(commissionPkh),
			pushData(changePkh),
			pushNum(0),
		},
	}, r
}

func (f *fixture) rescind(p pair) (spend, pair) {
	after := p
	after.rescinded = true
	return spend{
		ins: f.ins(p), poolIdx: 0, vaultIdx: 1,
		outs:   []out{{p.sats, f.poolLock(after)}, {1, f.vaultLock(after)}},
		method: methodRescind, signer: p.validator,
	}, after
}

func (f *fixture) close(p pair) spend {
	return spend{
		ins: f.ins(p), poolIdx: 0, vaultIdx: 1,
		outs: []out{
			{1, tokenP2pkh(assetA, p.a, pkh(p.lp))},
			{1, tokenP2pkh(assetB, p.b, pkh(p.lp))},
		},
		method: methodClose, signer: p.lp,
	}
}

func mustPass(t *testing.T, r result) {
	t.Helper()
	if r.pool != nil {
		t.Fatalf("pool input rejected: %v", r.pool)
	}
	if r.vault != nil {
		t.Fatalf("vault input rejected: %v", r.vault)
	}
}

func mustFailPool(t *testing.T, r result) {
	t.Helper()
	if r.pool == nil {
		t.Fatal("pool input accepted, expected a rejection")
	}
}

func mustFailVault(t *testing.T, r result) {
	t.Helper()
	if r.vault == nil {
		t.Fatal("vault input accepted, expected a rejection")
	}
}

func fresh() pair {
	return pair{sats: 1, a: 5_000_000, b: 2_000_000, lp: key(10), validator: key(20)}
}

func clone(o []out) []out {
	c := make([]out, len(o))
	for i := range o {
		c[i] = out{o[i].sats, append([]byte{}, o[i].script...)}
	}
	return c
}

// --- tests ---

func TestSwap(t *testing.T) {
	for _, bps := range [][3]int64{{30, 5, 10}, {0, 0, 0}} {
		f := newFixture(t, bps[0], bps[1], bps[2])
		for _, aIn := range []bool{true, false} {
			name := fmt.Sprintf("bps %v aIn=%v", bps, aIn)
			p := fresh()
			s, r := f.swap(p, 40_000, aIn)
			if !r.ok {
				t.Fatal("model rejects the fixture swap")
			}
			inAsset, outAsset := assetA, assetB
			if !aIn {
				inAsset, outAsset = assetB, assetA
			}
			t.Run(name+": pays out along the curve, both inputs verify", func(t *testing.T) {
				mustPass(t, f.run(t, s))
			})
			t.Run(name+": token change and sats change", func(t *testing.T) {
				c := s
				c.outs = append(clone(s.outs), out{1, tokenP2pkh(inAsset, 777, changePkh)}, out{4_321, p2pkh(changePkh)})
				c.args = append([][]byte{}, s.args...)
				c.args[6] = pushNum(777)
				c.change = &out{4_321, changePkh}
				mustPass(t, f.run(t, c))
			})
			mut := func(name string, edit func(c *spend)) {
				t.Run(name, func(t *testing.T) {
					c := s
					c.outs = clone(s.outs)
					edit(&c)
					mustFailPool(t, f.run(t, c))
				})
			}
			mut(name+": payout one more", func(c *spend) { c.outs[2].script = tokenP2pkh(outAsset, r.out+1, userPkh) })
			mut(name+": payout in the input token", func(c *spend) { c.outs[2].script = tokenP2pkh(inAsset, r.out, userPkh) })
			mut(name+": payout to another key", func(c *spend) { c.outs[2].script = tokenP2pkh(outAsset, r.out, changePkh) })
			mut(name+": payout of more than 1 sat", func(c *spend) { c.outs[2].sats = 2 })
			mut(name+": vault one token short", func(c *spend) {
				a := r.after
				a.b--
				c.outs[1].script = f.vaultLock(a)
			})
			mut(name+": vault in token A", func(c *spend) { c.outs[1].script = append(tokenPrefix(assetA, r.after.b), f.vault...) })
			mut(name+": vault with other code", func(c *spend) { c.outs[1].script = tokenP2pkh(assetB, r.after.b, pkh(p.validator)) })
			mut(name+": vault code one byte off", func(c *spend) { c.outs[1].script[len(c.outs[1].script)-1] ^= 1 })
			mut(name+": vault of 2 sats", func(c *spend) { c.outs[1].sats = 2 })
			mut(name+": vault and pool swapped", func(c *spend) { c.outs[0], c.outs[1] = c.outs[1], c.outs[0] })
			mut(name+": vault missing", func(c *spend) { c.outs = append(c.outs[:1], c.outs[2:]...) })
			mut(name+": pool prefix one token short", func(c *spend) {
				a := r.after
				a.a--
				lock := f.poolLock(r.after)
				c.outs[0].script = append(tokenPrefix(assetA, a.a), lock[len(tokenPrefix(assetA, r.after.a)):]...)
			})
			mut(name+": ReserveB in state one short", func(c *spend) {
				a := r.after
				a.b--
				c.outs[0].script = f.poolLock(a)
			})
			mut(name+": pool keeps the LP's key changed", func(c *spend) {
				a := r.after
				a.lp = key(99)
				c.outs[0].script = f.poolLock(a)
			})
			mut(name+": an extra output", func(c *spend) { c.outs = append(c.outs, out{1, p2pkh(userPkh)}) })
			mut(name+": signed by the LP", func(c *spend) { c.signer = p.lp })
			mut(name+": signed by the next validator key", func(c *spend) { c.signer = r.after.validator })
			if bps[0] > 0 {
				mut(name+": LP fee to the other token's address", func(c *spend) {
					other := lpFeeBPkh
					if !aIn {
						other = lpFeeAPkh
					}
					c.outs[3].script = tokenP2pkh(inAsset, r.lpFee, other)
				})
				mut(name+": LP fee in the output token", func(c *spend) {
					lp := lpFeeAPkh
					if !aIn {
						lp = lpFeeBPkh
					}
					c.outs[3].script = tokenP2pkh(outAsset, r.lpFee, lp)
				})
				mut(name+": validator fee one short", func(c *spend) { c.outs[4].script = tokenP2pkh(inAsset, r.valFee-1, pkh(p.validator)) })
				mut(name+": commission missing", func(c *spend) { c.outs = c.outs[:5] })
			}
		}
	}
}

// The pool's sats are carried, not reset: none can leave through the change.
func TestSwapCarriesThePoolsSats(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	p.sats = 5_000
	s, _ := f.swap(p, 40_000, true)
	mustPass(t, f.run(t, s))
	c := s
	c.outs = clone(s.outs)
	c.outs[0].sats = 1
	c.outs = append(c.outs, out{4_999, p2pkh(changePkh)})
	c.change = &out{4_999, changePkh}
	mustFailPool(t, f.run(t, c))
}

func TestSwapDifferential(t *testing.T) {
	rng := rand.New(rand.NewSource(20261115))
	for _, bps := range [][3]int64{{0, 0, 0}, {30, 5, 10}, {1, 1, 1}, {2500, 2500, 2500}} {
		f := newFixture(t, bps[0], bps[1], bps[2])
		valid := 0
		for i := 0; i < 150; i++ {
			p := pair{sats: 1, a: 1 + rng.Int63n(1<<uint(1+rng.Intn(45))), b: 1 + rng.Int63n(1<<uint(1+rng.Intn(45))), lp: key(10), validator: key(20)}
			amountIn := 1 + rng.Int63n(1<<uint(1+rng.Intn(45)))
			aIn := rng.Intn(2) == 0
			s, r := f.swap(p, amountIn, aIn)
			if !r.ok {
				continue // covered by TestSwapRejects
			}
			valid++
			if res := f.run(t, s); !res.ok() {
				t.Fatalf("bps %v a=%d b=%d in=%d aIn=%v: %v / %v", bps, p.a, p.b, amountIn, aIn, res.pool, res.vault)
			}
			outAsset := assetB
			if !aIn {
				outAsset = assetA
			}
			c := s
			c.outs = clone(s.outs)
			c.outs[2].script = tokenP2pkh(outAsset, r.out+1, userPkh)
			if f.run(t, c).pool == nil {
				t.Fatalf("bps %v a=%d b=%d in=%d aIn=%v: payout+1 accepted", bps, p.a, p.b, amountIn, aIn)
			}
			// The pool one unit poorer in the token paid out, the user one richer.
			a := r.after
			if aIn {
				a.b--
			} else {
				a.a--
			}
			if a.a > 0 && a.b > 0 {
				c.outs[0].script, c.outs[1].script = f.poolLock(a), f.vaultLock(a)
				if f.run(t, c).pool == nil {
					t.Fatalf("bps %v a=%d b=%d in=%d aIn=%v: pool-1 with payout+1 accepted", bps, p.a, p.b, amountIn, aIn)
				}
			}
		}
		t.Logf("bps %v: %d valid swaps verified on both inputs", bps, valid)
	}
}

// shapedSwap builds the transaction the script's own arithmetic gives for
// net entering and paid leaving, with no fee outputs: the outputs the
// contract itself would write, so a refusal is the guard's and not a
// hashOutputs mismatch. amountIn is the raw push.
func shapedSwap(f *fixture, p pair, amountIn []byte, aIn bool, net, paid int64) spend {
	after := p
	after.validator = key(p.validator.Serialize()[0] + 1)
	outAsset := assetB
	if aIn {
		after.a, after.b = p.a+net, p.b-paid
	} else {
		after.a, after.b = p.a-paid, p.b+net
		outAsset = assetA
	}
	return spend{
		ins: f.ins(p), poolIdx: 0, vaultIdx: 1,
		outs: []out{
			{p.sats, f.poolLock(after)},
			{1, f.vaultLock(after)},
			{1, tokenP2pkh(outAsset, paid, userPkh)},
		},
		method: methodSwap, signer: p.validator,
		args: [][]byte{
			pushData(after.validator.PubKey().Compressed()),
			amountIn,
			pushBool(aIn),
			pushData(userPkh),
			pushData(commissionPkh),
			pushData(changePkh),
			pushNum(0),
		},
	}
}

// Swaps the contract must refuse, with the outputs the contract itself would
// build for them (so the refusal is the guard's, not a hashOutputs mismatch).
func TestSwapRejects(t *testing.T) {
	free := newFixture(t, 0, 0, 0)
	t.Run("output rounds to zero, A in", func(t *testing.T) {
		p := pair{sats: 1, a: 1 << 40, b: 10, lp: key(10), validator: key(20)}
		mustFailPool(t, free.run(t, shapedSwap(free, p, pushNum(1000), true, 1000, 0)))
	})
	t.Run("output rounds to zero, B in", func(t *testing.T) {
		p := pair{sats: 1, a: 10, b: 1 << 40, lp: key(10), validator: key(20)}
		mustFailPool(t, free.run(t, shapedSwap(free, p, pushNum(1000), false, 1000, 0)))
	})
	t.Run("control: the same shape with a payout of 1 passes", func(t *testing.T) {
		p := pair{sats: 1, a: 1000, b: 10, lp: key(10), validator: key(20)}
		// net 1000: out = 1000*10/2000 = 5
		mustPass(t, free.run(t, shapedSwap(free, p, pushNum(1000), true, 1000, 5)))
	})

	// Fee rates summing over 100%: a negative amountIn has negative fees
	// (the script's division truncates toward zero) and a positive net.
	// Only amountIn > 0 refuses it.
	over := newFixture(t, 9999, 9999, 0)
	t.Run("negative amountIn where the fees sum over 100%", func(t *testing.T) {
		p := fresh()
		// amountIn -10000: each fee (-10000*9999+9999)/10000 = -9998; net 9996.
		const net = 9996
		outB := net * p.b / (p.a + net)
		mustFailPool(t, over.run(t, shapedSwap(over, p, []byte{0x02, 0x10, 0xa7}, true, net, outB)))
	})

	f := newFixture(t, 30, 5, 10)
	t.Run("fees take the whole swap", func(t *testing.T) {
		s, r := f.swap(fresh(), 3, true)
		if r.ok {
			t.Fatal("the model accepts it")
		}
		mustFailPool(t, f.run(t, s))
	})
}

func TestLifecycle(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	for i := 0; i < 20; i++ {
		s, r := f.swap(p, int64(10_000+7919*i), i%2 == 0)
		mustPass(t, f.run(t, s))
		t.Run(fmt.Sprintf("swap %d: the previous validator key no longer signs", i), func(t *testing.T) {
			if i == 0 {
				t.Skip()
			}
			c := s
			c.signer = key(p.validator.Serialize()[0] - 1)
			mustFailPool(t, f.run(t, c))
		})
		p = r.after
	}
	t.Run("the LP closes after swaps", func(t *testing.T) { mustPass(t, f.run(t, f.close(p))) })
	rs, rescinded := f.rescind(p)
	t.Run("the validator rescinds", func(t *testing.T) { mustPass(t, f.run(t, rs)) })
	t.Run("rescinded: no swap", func(t *testing.T) {
		for _, k := range []*ec.PrivateKey{p.validator, p.lp, validatorIdentity} {
			q := rescinded
			q.validator = k
			s, _ := f.swap(q, 10_000, true)
			q.rescinded = true
			s.ins = f.ins(q)
			mustFailPool(t, f.run(t, s))
		}
	})
	t.Run("rescinded: no second rescind", func(t *testing.T) {
		s, _ := f.rescind(rescinded)
		mustFailPool(t, f.run(t, s))
	})
	t.Run("rescinded: the LP closes", func(t *testing.T) { mustPass(t, f.run(t, f.close(rescinded))) })
}

func TestRescind(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	s, after := f.rescind(p)
	mustPass(t, f.run(t, s))
	t.Run("with Rúnar's change", func(t *testing.T) {
		c := s
		c.outs = append(clone(s.outs), out{900, p2pkh(changePkh)})
		c.change = &out{900, changePkh}
		mustPass(t, f.run(t, c))
	})
	mut := func(name string, edit func(c *spend)) {
		t.Run(name, func(t *testing.T) {
			c := s
			c.outs = clone(s.outs)
			edit(&c)
			mustFailPool(t, f.run(t, c))
		})
	}
	mut("signed by the LP", func(c *spend) { c.signer = p.lp })
	mut("the key kept", func(c *spend) { c.outs[0].script = f.poolLock(p) })
	mut("the vault dropped", func(c *spend) { c.outs = c.outs[:1] })
	mut("the vault one token short", func(c *spend) {
		a := after
		a.b--
		c.outs[1].script = f.vaultLock(a)
	})
	mut("the vault to a key", func(c *spend) { c.outs[1].script = tokenP2pkh(assetB, p.b, pkh(p.validator)) })
	mut("ReserveA changed", func(c *spend) {
		a := after
		a.a--
		c.outs[0].script = f.poolLock(a)
	})
	mut("an extra output", func(c *spend) { c.outs = append(c.outs, out{1, p2pkh(userPkh)}) })
}

func TestClose(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	t.Run("the LP: outputs of the LP's choosing", func(t *testing.T) { mustPass(t, f.run(t, f.close(p))) })
	t.Run("the LP: a single unrelated output", func(t *testing.T) {
		s := f.close(p)
		s.outs = []out{{1, []byte{0x6a}}}
		mustPass(t, f.run(t, s))
	})
	for name, k := range map[string]*ec.PrivateKey{"the validator": p.validator, "the identity key": validatorIdentity, "another key": key(77)} {
		t.Run(name+": refused", func(t *testing.T) {
			s := f.close(p)
			s.signer = k
			mustFailPool(t, f.run(t, s))
		})
	}
}

// The pool and its vault cannot be parted, by any method.
func TestPairing(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	sw, _ := f.swap(p, 40_000, true)
	rs, _ := f.rescind(p)
	methods := map[string]spend{"Swap": sw, "Rescind": rs, "Close": f.close(p)}

	other := fresh()
	other.b = p.b // a foreign vault holding the same amount
	for name, s := range methods {
		t.Run(name+": honest", func(t *testing.T) { mustPass(t, f.run(t, s)) })

		t.Run(name+": the pool without its vault (a funding input at 1)", func(t *testing.T) {
			c := s
			c.ins = []in{s.ins[0], s.ins[2]}
			c.vaultIdx = -1
			mustFailPool(t, f.run(t, c))
		})
		t.Run(name+": the pool alone", func(t *testing.T) {
			c := s
			c.ins = s.ins[:1]
			c.vaultIdx = -1
			mustFailPool(t, f.run(t, c))
		})
		t.Run(name+": a foreign vault at input 1", func(t *testing.T) {
			c := s
			c.ins = []in{s.ins[0], {txid: 0x33, vout: 1, sats: 1, lock: f.vaultLock(other)}, s.ins[2]}
			r := f.run(t, c)
			mustFailPool(t, r)
			mustFailVault(t, r)
		})
		t.Run(name+": its vault at input 2", func(t *testing.T) {
			c := s
			c.ins = []in{s.ins[0], s.ins[2], s.ins[1]}
			c.vaultIdx = 2
			r := f.run(t, c)
			mustFailPool(t, r)
			mustFailVault(t, r)
		})
		t.Run(name+": the vault first, the pool second", func(t *testing.T) {
			c := s
			c.ins = []in{s.ins[1], s.ins[0], s.ins[2]}
			c.poolIdx, c.vaultIdx = 1, 0
			r := f.run(t, c)
			mustFailPool(t, r)
			mustFailVault(t, r)
		})
		t.Run(name+": the pool not at output 0 of its transaction", func(t *testing.T) {
			c := s
			c.ins = []in{{txid: 0x11, vout: 2, sats: p.sats, lock: f.poolLock(p)}, s.ins[1], s.ins[2]}
			r := f.run(t, c)
			mustFailPool(t, r)
			mustFailVault(t, r)
		})
		t.Run(name+": the pool told false prevouts", func(t *testing.T) {
			c := s
			c.ins = []in{s.ins[0], s.ins[2]}
			c.vaultIdx = -1
			// The prevouts an honest spend would show, not this transaction's.
			c.prevouts = append(append(outpoint(s.ins[0]), outpoint(s.ins[1])...), outpoint(s.ins[2])...)
			mustFailPool(t, f.run(t, c))
		})
		t.Run(name+": the pool told truncated prevouts", func(t *testing.T) {
			c := s
			c.prevouts = append(outpoint(s.ins[0]), outpoint(s.ins[1])...)
			mustFailPool(t, f.run(t, c))
		})
	}
}

// The vault moves only with its pool.
func TestVault(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	base := f.close(p)
	vault := base.ins[1]
	steal := []out{{1, tokenP2pkh(assetB, p.b, userPkh)}}

	t.Run("alone", func(t *testing.T) {
		mustFailVault(t, f.run(t, spend{ins: []in{vault}, outs: steal, poolIdx: -1, vaultIdx: 0}))
	})
	t.Run("at input 0 with a funding input", func(t *testing.T) {
		mustFailVault(t, f.run(t, spend{ins: []in{vault, base.ins[2]}, outs: steal, poolIdx: -1, vaultIdx: 0}))
	})
	t.Run("at input 1 after a funding input", func(t *testing.T) {
		mustFailVault(t, f.run(t, spend{ins: []in{base.ins[2], vault}, outs: steal, poolIdx: -1, vaultIdx: 1}))
	})
	t.Run("at input 1 after another transaction's output 0", func(t *testing.T) {
		mustFailVault(t, f.run(t, spend{ins: []in{{txid: 0x33, vout: 0, sats: 1}, vault}, outs: steal, poolIdx: -1, vaultIdx: 1}))
	})
	t.Run("at input 1 after its own transaction's output 2", func(t *testing.T) {
		mustFailVault(t, f.run(t, spend{ins: []in{{txid: 0x11, vout: 2, sats: 1}, vault}, outs: steal, poolIdx: -1, vaultIdx: 1}))
	})
	t.Run("a vault script at output 0 or 2 of a transaction is unspendable with that transaction's output 0", func(t *testing.T) {
		for _, vout := range []uint32{0, 2} {
			v := vault
			v.vout = vout
			mustFailVault(t, f.run(t, spend{ins: []in{{txid: 0x11, vout: 0, sats: 1}, v}, outs: steal, poolIdx: -1, vaultIdx: 1}))
		}
	})
	t.Run("told the honest prevouts in another transaction", func(t *testing.T) {
		s := spend{ins: []in{base.ins[2], vault}, outs: steal, poolIdx: -1, vaultIdx: 1}
		s.vaultPrevouts = append(outpoint(base.ins[0]), outpoint(vault)...)
		mustFailVault(t, f.run(t, s))
	})
	// The preimage is checked against the spending transaction: one from
	// another transaction, however honest its prevouts, is refused.
	var honestPre []byte
	grab := base
	grab.forgeVaultPre = func(real []byte) []byte { honestPre = real; return real }
	mustPass(t, f.run(t, grab))
	t.Run("alone, with the preimage and prevouts of the honest Close", func(t *testing.T) {
		s := spend{ins: []in{base.ins[2], vault}, outs: steal, poolIdx: -1, vaultIdx: 1}
		s.vaultPrevouts = append(append(outpoint(base.ins[0]), outpoint(vault)...), outpoint(base.ins[2])...)
		s.forgeVaultPre = func([]byte) []byte { return honestPre }
		mustFailVault(t, f.run(t, s))
	})
	t.Run("with its pool, one preimage byte flipped", func(t *testing.T) {
		for _, i := range []int{0, 4, 40, 68, 104, len(honestPre) - 1} {
			s := base
			s.forgeVaultPre = func(real []byte) []byte {
				c := append([]byte{}, real...)
				c[i] ^= 1
				return c
			}
			mustFailVault(t, f.run(t, s))
		}
	})
	// A preimage of any type but ALL|FORKID is refused (ANYONECANPAY would
	// zero hashPrevouts, the vault's one rule with it).
	for name, flag := range map[string]sighash.Flag{
		"ALL|ANYONECANPAY":    sighash.AllForkID | sighash.AnyOneCanPay,
		"NONE":                sighash.NoneForkID,
		"SINGLE":              sighash.SingleForkID,
		"NONE|ANYONECANPAY":   sighash.NoneForkID | sighash.AnyOneCanPay,
		"SINGLE|ANYONECANPAY": sighash.SingleForkID | sighash.AnyOneCanPay,
		"ALL without FORKID":  sighash.All,
	} {
		t.Run("preimage "+name+": with the pool", func(t *testing.T) {
			s := base
			s.vaultSighash = flag
			mustFailVault(t, f.run(t, s))
		})
		t.Run("preimage "+name+": alone, told the prevouts the type hashes", func(t *testing.T) {
			s := spend{ins: []in{base.ins[2], vault}, outs: steal, poolIdx: -1, vaultIdx: 1, vaultSighash: flag}
			mustFailVault(t, f.run(t, s))
			s.vaultPrevouts = []byte{}
			mustFailVault(t, f.run(t, s))
		})
	}
	t.Run("with its pool, by Close: released to outputs of the LP's choosing", func(t *testing.T) {
		mustPass(t, f.run(t, base))
	})
}

// Pool's twin-pool case (two identical pools sharing one output list) cannot
// arise: a pool must be input 0.
func TestTwoPoolsInOneTransaction(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	s, _ := f.swap(p, 40_000, true)
	twin := []in{
		s.ins[0], s.ins[1],
		{txid: 0x33, vout: 0, sats: p.sats, lock: f.poolLock(p)},
		{txid: 0x33, vout: 1, sats: 1, lock: f.vaultLock(p)},
	}
	first := s
	first.ins = twin
	mustPass(t, f.run(t, first))
	second := s
	second.ins = twin
	second.poolIdx, second.vaultIdx = 2, 3
	r := f.run(t, second)
	mustFailPool(t, r)
	mustFailVault(t, r)
}

// The vault's script is an argument, checked against VaultCodeHash: any other
// bytes are refused, whatever output 1 then holds.
func TestVaultCodeArgument(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	sw, r := f.swap(p, 40_000, true)
	rs, after := f.rescind(p)
	for name, tc := range map[string]struct {
		s     spend
		after pair
	}{"Swap": {sw, r.after}, "Rescind": {rs, after}} {
		t.Run(name+": honest", func(t *testing.T) { mustPass(t, f.run(t, tc.s)) })
		other := tokenP2pkh(assetB, tc.after.b, pkh(p.validator))[len(tokenPrefix(assetB, tc.after.b)):]
		for alt, code := range map[string][]byte{
			"a P2PKH to the validator":  other,
			"empty":                     {},
			"the vault's, one byte off": append(append([]byte{}, f.vault[:len(f.vault)-1]...), f.vault[len(f.vault)-1]^1),
			"the vault's, one byte on":  append(append([]byte{}, f.vault...), 0x61),
		} {
			t.Run(name+": vaultCode "+alt+", output 1 built from it", func(t *testing.T) {
				c := tc.s
				c.outs = clone(tc.s.outs)
				c.vaultArg = code
				c.outs[1].script = append(tokenPrefix(assetB, tc.after.b), code...)
				mustFailPool(t, f.run(t, c))
			})
			t.Run(name+": vaultCode "+alt+", output 1 the real vault", func(t *testing.T) {
				c := tc.s
				c.vaultArg = code
				mustFailPool(t, f.run(t, c))
			})
		}
	}
}

// The validator's and the LP's signatures must be ALL|FORKID: a valid
// signature of any other type is refused by every method.
func TestSignatureSighashType(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	sw, _ := f.swap(p, 40_000, true)
	rs, _ := f.rescind(p)
	for name, s := range map[string]spend{"Swap": sw, "Rescind": rs, "Close": f.close(p)} {
		t.Run(name+": ALL|FORKID", func(t *testing.T) { mustPass(t, f.run(t, s)) })
		for flagName, flag := range map[string]sighash.Flag{
			"NONE":                sighash.NoneForkID,
			"SINGLE":              sighash.SingleForkID,
			"ALL|ANYONECANPAY":    sighash.AllForkID | sighash.AnyOneCanPay,
			"NONE|ANYONECANPAY":   sighash.NoneForkID | sighash.AnyOneCanPay,
			"SINGLE|ANYONECANPAY": sighash.SingleForkID | sighash.AnyOneCanPay,
		} {
			t.Run(name+": "+flagName, func(t *testing.T) {
				c := s
				c.sigFlag = flag
				mustFailPool(t, f.run(t, c))
			})
		}
	}
}

func TestSizes(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := fresh()
	t.Logf("vault code %d bytes; pool code with its args %d bytes; pool output script %d bytes; vault output script %d bytes",
		len(f.vault), len(f.code), len(f.poolLock(p)), len(f.vaultLock(p)))
}
