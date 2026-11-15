package pool

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	hash "github.com/bsv-blockchain/go-sdk/primitives/hash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/icellan/runar/compilers/go/compiler"
	runar "github.com/icellan/runar/packages/runar-go"
)

// Adversarial tests for the pool contract. Everything runs through the go-sdk
// interpreter on a real two-input transaction; the helpers prefixed adv are a
// more flexible sibling of fixture.run (other sighash types, a signature made
// for another transaction, extra inputs, raw method pushes, extra pushes).

// --- runner ---

type advOpts struct {
	preFlag  sighash.Flag             // type of the pushed txPreimage; 0 = ALL|FORKID
	sigFlag  sighash.Flag             // sighash type of the signatures; 0 = ALL|FORKID
	sigTx    *transaction.Transaction // signatures are made over this tx instead of the real one
	salt     byte                     // xored into the pool outpoint txid of the real tx
	extraIns int                      // additional inputs after the funding input
	method   []byte                   // raw push replacing the method index
	leading  [][]byte                 // pushes below the _codePart
	trailing [][]byte                 // pushes after the method index
	codePart []byte                   // replaces the _codePart
	chPkh    []byte                   // raw push replacing _changePKH
	chAmt    []byte                   // raw push replacing _changeAmount
	sigTweak func([]byte) []byte      // applied to each signature (DER + type byte) before pushing
	noPre    bool                     // omit the preimage push
	// noChronicle runs the interpreter without the Chronicle upgrade (legacy sighash off).
	noChronicle bool
}

func advFlag(f, def sighash.Flag) sighash.Flag {
	if f == 0 {
		return def
	}
	return f
}

func (f *fixture) advTx(p pool, c call, o advOpts) (*transaction.Transaction, *transaction.TransactionOutput, []byte) {
	lock := p.lock
	if lock == nil {
		lock = f.poolScript(p)
	}
	prevOut := &transaction.TransactionOutput{Satoshis: uint64(p.bsv), LockingScript: script.NewFromBytes(lock)}
	tx := transaction.NewTransaction()
	h := bytes.Repeat([]byte{0x11}, 32)
	h[0] ^= o.salt
	prevTxid, _ := chainhash.NewHash(h)
	fundTxid, _ := chainhash.NewHashFromHex(strings.Repeat("22", 32))
	poolIn := &transaction.TransactionInput{SourceTXID: prevTxid, SourceTxOutIndex: 0, SequenceNumber: 0xffffffff}
	poolIn.SetSourceTxOutput(prevOut)
	fundIn := &transaction.TransactionInput{SourceTXID: fundTxid, SourceTxOutIndex: 0, SequenceNumber: 0xffffffff}
	if c.poolIndex == 0 {
		tx.AddInput(poolIn)
		tx.AddInput(fundIn)
	} else {
		tx.AddInput(fundIn)
		tx.AddInput(poolIn)
	}
	for i := 0; i < o.extraIns; i++ {
		id, _ := chainhash.NewHash(bytes.Repeat([]byte{byte(0x23 + i)}, 32))
		in := &transaction.TransactionInput{SourceTXID: id, SourceTxOutIndex: 0, SequenceNumber: 0xffffffff}
		tx.AddInput(in)
	}
	for _, out := range c.outs {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: uint64(out.sats), LockingScript: script.NewFromBytes(out.script)})
	}
	return tx, prevOut, lock
}

// advDigest is the sighash the interpreter computes for input pi under flag,
// the scriptCode being the pool's script after its OP_CODESEPARATOR.
func advDigest(t *testing.T, tx *transaction.Transaction, pi int, lock []byte, sats uint64, flag sighash.Flag) []byte {
	t.Helper()
	cp := tx.ShallowClone()
	cp.Inputs[pi].SetSourceTxOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: script.NewFromBytes(separatorScriptCode(lock))})
	var pre []byte
	var err error
	if flag&sighash.ForkID != 0 {
		pre, err = cp.CalcInputPreimage(uint32(pi), flag)
	} else {
		pre, err = cp.CalcInputPreimageLegacy(uint32(pi), flag)
	}
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(pre)
	d := sha256.Sum256(h[:])
	return d[:]
}

func (f *fixture) advUnlock(t *testing.T, tx *transaction.Transaction, pi int, lock []byte, sats uint64, c call, o advOpts) []byte {
	t.Helper()
	digestTx := tx
	if o.sigTx != nil {
		digestTx = o.sigTx
	}
	sdig := advDigest(t, digestTx, pi, lock, sats, advFlag(o.sigFlag, sighash.AllForkID))
	args := append([][]byte{}, c.args...)
	for i, k := range c.signers {
		sig, err := k.Sign(sdig)
		if err != nil {
			t.Fatal(err)
		}
		b := append(sig.Serialize(), byte(advFlag(o.sigFlag, sighash.AllForkID)))
		if o.sigTweak != nil {
			b = o.sigTweak(b)
		}
		args[c.sigSlots[i]] = pushData(b)
	}
	pdig := advDigestPre(t, tx, pi, lock, sats, advFlag(o.preFlag, sighash.AllForkID))

	var u []byte
	for _, l := range o.leading {
		u = append(u, l...)
	}
	code := f.code
	if o.codePart != nil {
		code = o.codePart
	}
	u = append(u, pushData(code)...)
	for _, a := range args {
		u = append(u, a...)
	}
	if c.method != methodClose {
		changePkh, change := make([]byte, 20), int64(0)
		if c.change != nil {
			changePkh, change = c.change.pkh, c.change.sats
		}
		if o.chPkh != nil {
			u = append(u, o.chPkh...)
		} else {
			u = append(u, pushData(changePkh)...)
		}
		if o.chAmt != nil {
			u = append(u, o.chAmt...)
		} else {
			u = append(u, pushNum(change)...)
		}
	}
	if !o.noPre {
		u = append(u, pushData(pdig)...)
	}
	if o.method != nil {
		u = append(u, o.method...)
	} else {
		u = append(u, pushNum(int64(c.method))...)
	}
	for _, tr := range o.trailing {
		u = append(u, tr...)
	}
	return u
}

// advDigestPre is the preimage itself (not hashed) under flag.
func advDigestPre(t *testing.T, tx *transaction.Transaction, pi int, lock []byte, sats uint64, flag sighash.Flag) []byte {
	t.Helper()
	cp := tx.ShallowClone()
	cp.Inputs[pi].SetSourceTxOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: script.NewFromBytes(separatorScriptCode(lock))})
	pre, err := cp.CalcInputPreimage(uint32(pi), flag)
	if err != nil {
		t.Fatal(err)
	}
	return pre
}

func advExecute(tx *transaction.Transaction, pi int, prevOut *transaction.TransactionOutput) error {
	return advExecuteC(tx, pi, prevOut, true)
}

func advExecuteC(tx *transaction.Transaction, pi int, prevOut *transaction.TransactionOutput, chronicle bool) error {
	if !chronicle {
		return interpreter.NewEngine().Execute(
			interpreter.WithTx(tx, pi, prevOut),
			interpreter.WithForkID(),
			interpreter.WithAfterGenesis(),
		)
	}
	return interpreter.NewEngine().Execute(
		interpreter.WithTx(tx, pi, prevOut),
		interpreter.WithForkID(),
		interpreter.WithAfterGenesis(),
		interpreter.WithAfterChronicle(),
	)
}

func (f *fixture) advRun(t *testing.T, p pool, c call, o advOpts) error {
	t.Helper()
	tx, prevOut, lock := f.advTx(p, c, o)
	pi := c.poolIndex
	tx.Inputs[pi].UnlockingScript = script.NewFromBytes(f.advUnlock(t, tx, pi, lock, uint64(p.bsv), c, o))
	return advExecuteC(tx, pi, prevOut, !o.noChronicle)
}

// advRunUnlock runs a ready-made unlocking script against the tx c describes.
func (f *fixture) advRunUnlock(t *testing.T, p pool, c call, unlock []byte) error {
	t.Helper()
	tx, prevOut, _ := f.advTx(p, c, advOpts{})
	tx.Inputs[c.poolIndex].UnlockingScript = script.NewFromBytes(unlock)
	return advExecute(tx, c.poolIndex, prevOut)
}

func (f *fixture) advUnlockOf(t *testing.T, p pool, c call) []byte {
	t.Helper()
	tx, _, lock := f.advTx(p, c, advOpts{})
	return f.advUnlock(t, tx, c.poolIndex, lock, uint64(p.bsv), c, advOpts{})
}

// --- fixtures ---

type advCfg struct{ lp, v, c int64 }

var advCfgs = []advCfg{{30, 5, 10}, {0, 0, 0}}

var (
	advMu    sync.Mutex
	advCache = map[advCfg]*fixture{}
)

func advFix(t *testing.T, cfg advCfg) *fixture {
	advMu.Lock()
	defer advMu.Unlock()
	if f, ok := advCache[cfg]; ok {
		return f
	}
	f := newFixture(t, cfg.lp, cfg.v, cfg.c)
	advCache[cfg] = f
	return f
}

func advPool(t *testing.T) pool {
	return pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
}

// advEach runs fn for each fee config and each swap direction.
func advEach(t *testing.T, fn func(t *testing.T, f *fixture, p pool, bsvIn bool)) {
	for _, cfg := range advCfgs {
		for _, bsvIn := range []bool{true, false} {
			name := fmt.Sprintf("fees%d-%d-%d/%s", cfg.lp, cfg.v, cfg.c, map[bool]string{true: "bsv in", false: "tokens in"}[bsvIn])
			cfg, bsvIn := cfg, bsvIn
			t.Run(name, func(t *testing.T) {
				fn(t, advFix(t, cfg), advPool(t), bsvIn)
			})
		}
	}
}

// advEachCfg runs fn once per fee config.
func advEachCfg(t *testing.T, fn func(t *testing.T, f *fixture, p pool)) {
	for _, cfg := range advCfgs {
		cfg := cfg
		t.Run(fmt.Sprintf("fees%d-%d-%d", cfg.lp, cfg.v, cfg.c), func(t *testing.T) {
			fn(t, advFix(t, cfg), advPool(t))
		})
	}
}

func advValid(f *fixture, p pool, method int, bsvIn bool) call {
	switch method {
	case methodSwap:
		return f.swapCall(p, 20_000, bsvIn, f.expectSwap(p, 20_000, bsvIn))
	case methodClose:
		return f.closeCall(p)
	default:
		return f.rescindCall(p)
	}
}

// advAlt is a different, equally valid call of the same method (other outputs).
func advAlt(f *fixture, p pool, method int, bsvIn bool) call {
	switch method {
	case methodSwap:
		return f.swapCall(p, 21_000, bsvIn, f.expectSwap(p, 21_000, bsvIn))
	case methodClose:
		c := f.closeCall(p)
		c.outs = []out{{p.bsv - 700, p2pkh(pkh(key(nil, 63)))}, {1, tokenP2pkh(p.tokens, pkh(key(nil, 64)))}}
		return c
	default:
		c := f.rescindCall(p)
		c.change.sats = 49_001
		c.outs[1].sats = 49_001
		return c
	}
}

var advMethodNames = []string{"Swap", "Close", "Rescind"}

// advSigner swaps in the signer (or a raw push) at the signature slot.
func advSetSigner(c call, k *ec.PrivateKey) call {
	c.signers = []*ec.PrivateKey{k}
	return c
}

func advSetRaw(c call, raw []byte) call {
	c.signers = nil
	c.args = append([][]byte{}, c.args...)
	c.args[0] = raw
	return c
}

func advIsHead(t *testing.T) bool {
	t.Helper()
	art, err := compiler.CompileFromSource("Pool.runar.go")
	if err != nil {
		t.Fatal(err)
	}
	js, err := compiler.ArtifactToJSON(art)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Contains(js, []byte("codePartLenSlots"))
}

// advCode is the pool code with the given readonly args spliced in (as
// newFixture builds it, but with a choosable asset id).
func advCode(t *testing.T, asset []byte, lpBps, vBps, cBps int64) []byte {
	t.Helper()
	art, err := compiler.CompileFromSource("Pool.runar.go")
	if err != nil {
		t.Fatal(err)
	}
	js, _ := compiler.ArtifactToJSON(art)
	var sa runar.RunarArtifact
	if err := json.Unmarshal(js, &sa); err != nil {
		t.Fatal(err)
	}
	p := pool{tokens: 1000, lp: key(t, 1), validator: key(t, 2)}
	c := runar.NewRunarContract(&sa, []interface{}{
		p.tokens,
		hex.EncodeToString(p.lp.PubKey().Compressed()),
		hex.EncodeToString(p.validator.PubKey().Compressed()),
		hex.EncodeToString(validatorIdentity.PubKey().Compressed()),
		hex.EncodeToString(nonce),
		hex.EncodeToString(lpSatsPkh),
		hex.EncodeToString(lpTokenPkh),
		hex.EncodeToString(affirmation),
		hex.EncodeToString(asset),
		lpBps, vBps, cBps,
	})
	full, _ := hex.DecodeString(c.GetLockingScript())
	return full[:len(full)-1-len(p.state())]
}

// State layout offsets (see pool.state): tokens 0, LpPubKey 8, ValidatorPubKey
// 41, ValidatorIdentity 74, Nonce 107, LpSatsPkh 139, LpTokenPkh 159,
// Affirmation (length byte) 179.
const (
	offTokens   = 0
	offLp       = 8
	offVal      = 41
	offIdentity = 74
	offNonce    = 107
	offSats     = 139
	offTokenPkh = 159
	offAffLen   = 179
)

// advLock assembles a pool output script from its parts.
func advLock(prefix, code, state []byte) []byte {
	s := append(append([]byte{}, prefix...), code...)
	s = append(s, 0x6a)
	return append(s, state...)
}

func advHash160(b []byte) []byte { return hash.Hash160(b) }

func advRand(t *testing.T) *ec.PrivateKey { return key(t, 0x31) }

// advBigSwap is expectSwap for amounts whose products overflow int64.
func advBigSwap(f *fixture, p pool, amountIn int64, bsvIn bool) swapResult {
	r := swapResult{
		lpFee:         (amountIn*f.lpFeeBps + 9999) / 10000,
		validatorFee:  (amountIn*f.validatorBps + 9999) / 10000,
		commission:    (amountIn*f.commissionBps + 9999) / 10000,
		nextValidator: next(p.validator),
	}
	net := big.NewInt(amountIn - r.lpFee - r.validatorFee - r.commission)
	bsv, tok := big.NewInt(p.bsv), big.NewInt(p.tokens)
	o := new(big.Int)
	if bsvIn {
		o.Mul(net, tok)
		o.Div(o, new(big.Int).Add(bsv, net))
		r.out = o.Int64()
		r.newBsv, r.newTokens = p.bsv+net.Int64(), p.tokens-r.out
	} else {
		o.Mul(net, bsv)
		o.Div(o, new(big.Int).Add(tok, net))
		r.out = o.Int64()
		r.newBsv, r.newTokens = p.bsv-r.out, p.tokens+net.Int64()
	}
	return r
}

// --- 1. who can spend ---

func TestTheft_WhoCanSpend(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		rotated := f.expectSwap(p, 20_000, bsvIn).pool(p) // the pool after one swap; its validator is next(p.validator)
		for method := 0; method < 3; method++ {
			method := method
			mn := advMethodNames[method]
			type wrong struct {
				name string
				k    *ec.PrivateKey
				raw  []byte
			}
			var ws []wrong
			if method == methodSwap || method == methodRescind {
				ws = append(ws, wrong{"LP key", p.lp, nil})
			} else {
				ws = append(ws, wrong{"validator key", p.validator, nil})
			}
			ws = append(ws,
				wrong{"next validator key (early)", next(p.validator), nil},
				wrong{"ValidatorIdentity key", key(t, 0x7f), nil},
				wrong{"random key", advRand(t), nil},
				wrong{"empty signature", nil, []byte{0x00}},
				wrong{"33-byte pubkey in the signature slot", nil, pushData(p.lp.PubKey().Compressed())},
				wrong{"valid DER with a flipped byte", nil, nil},
			)
			for _, w := range ws {
				w := w
				t.Run(mn+": "+w.name, func(t *testing.T) {
					c := advValid(f, p, method, bsvIn)
					o := advOpts{}
					switch {
					case w.k != nil:
						c = advSetSigner(c, w.k)
					case w.raw != nil:
						c = advSetRaw(c, w.raw)
					default:
						o.sigTweak = func(b []byte) []byte { b[10] ^= 0x01; return b }
					}
					mustFail(t, f.advRun(t, p, c, o))
				})
			}
			t.Run(mn+": right key, signature made for a different transaction (replay)", func(t *testing.T) {
				c := advValid(f, p, method, bsvIn)
				alt, _, _ := f.advTx(p, advAlt(f, p, method, bsvIn), advOpts{})
				mustFail(t, f.advRun(t, p, c, advOpts{sigTx: alt}))
			})
			t.Run(mn+": right key, signature made for another pool outpoint", func(t *testing.T) {
				c := advValid(f, p, method, bsvIn)
				other, _, _ := f.advTx(p, c, advOpts{salt: 1})
				mustFail(t, f.advRun(t, p, c, advOpts{sigTx: other}))
			})
			t.Run(mn+": right key, own outpoint control", func(t *testing.T) {
				mustPass(t, f.advRun(t, p, advValid(f, p, method, bsvIn), advOpts{}))
			})
			t.Run(mn+": the previous validator key after rotation", func(t *testing.T) {
				c := advValid(f, rotated, method, bsvIn)
				if method == methodClose {
					// Close wants the LP: the old validator key is not it.
					mustFail(t, f.advRun(t, rotated, advSetSigner(c, p.validator), advOpts{}))
					return
				}
				mustFail(t, f.advRun(t, rotated, advSetSigner(c, p.validator), advOpts{}))
				mustPass(t, f.advRun(t, rotated, c, advOpts{})) // the current key still works
			})
		}
	})
}

// --- 2. sighash ---

var advFlags = []struct {
	name string
	flag sighash.Flag
}{
	{"NONE|FORKID", 0x42},
	{"SINGLE|FORKID", 0x43},
	{"ALL|ANYONECANPAY|FORKID", 0xC1},
	{"NONE|ANYONECANPAY|FORKID", 0xC2},
	{"SINGLE|ANYONECANPAY|FORKID", 0xC3},
	{"ALL without FORKID", 0x01},
}

// The in-script OP_PUSH_TX signature is fixed to 0x41, so a preimage of any
// other type has the wrong hash.
func TestTheft_PreimageSighashType(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		for method := 0; method < 3; method++ {
			for _, fl := range advFlags {
				method, fl := method, fl
				t.Run(advMethodNames[method]+": preimage "+fl.name, func(t *testing.T) {
					mustFail(t, f.advRun(t, p, advValid(f, p, method, bsvIn), advOpts{preFlag: fl.flag}))
				})
			}
		}
	})
}

// What the script does when the signer's own signature uses another sighash
// type (the preimage stays ALL|FORKID). Observed on both Rúnar versions, in
// the go-sdk interpreter (node policy was not tested): every type that carries
// FORKID is accepted; ALL without FORKID (the legacy sighash) is accepted only
// with the Chronicle upgrade enabled in the engine, refused without it.
func TestTheft_SignatureSighashType(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		for method := 0; method < 3; method++ {
			for _, fl := range advFlags {
				method, fl := method, fl
				t.Run(advMethodNames[method]+": signature "+fl.name, func(t *testing.T) {
					c := advValid(f, p, method, bsvIn)
					err := f.advRun(t, p, c, advOpts{sigFlag: fl.flag})
					t.Logf("%s sig %s: accepted=%v (%v)", advMethodNames[method], fl.name, err == nil, err)
					if err != nil {
						t.Fatalf("expected accepted (the signer's own choice), got %v", err)
					}
					if fl.flag&sighash.ForkID == 0 {
						errNC := f.advRun(t, p, c, advOpts{sigFlag: fl.flag, noChronicle: true})
						t.Logf("%s sig %s without Chronicle: %v", advMethodNames[method], fl.name, errNC)
						if errNC == nil {
							t.Fatal("expected the legacy sighash to be refused without Chronicle")
						}
					}
				})
			}
		}
		// Swap and Rescind pin their outputs through the preimage whatever the
		// signature covers: a NONE signature does not let a third party change them.
		for _, method := range []int{methodSwap, methodRescind} {
			method := method
			mn := advMethodNames[method]
			t.Run(mn+": NONE|FORKID signature: pool outputs stay pinned, the change outputs do not", func(t *testing.T) {
				thief := pkh(key(t, 0x66))
				c := advValid(f, p, method, bsvIn)
				if method == methodSwap {
					c = f.withChange(c, 7_777, pkh(key(t, 70)), 12_345)
				}
				signed, _, _ := f.advTx(p, c, advOpts{})
				// A third party redirects only the outputs the contract takes from unlocking
				// arguments: Rúnar's sats change (_changePKH, _changeAmount) and, on a
				// Swap, the taker's token change (tokenChangePkh).
				d := c
				d.outs = advCopy(c.outs)
				d.args = append([][]byte{}, c.args...)
				d.change = &struct {
					pkh  []byte
					sats int64
				}{thief, c.change.sats - 1000}
				d.outs[len(d.outs)-1] = out{c.change.sats - 1000, p2pkh(thief)}
				if method == methodSwap {
					d.args[6] = pushData(thief)
					d.outs[len(d.outs)-2] = out{1, tokenP2pkh(7_777, thief)}
				}
				// control: an ALL|FORKID signature binds them
				mustFail(t, f.advRun(t, p, d, advOpts{sigTx: signed}))
				// NONE does not: the change outputs are redirectable by a third party
				mustPass(t, f.advRun(t, p, d, advOpts{sigFlag: 0x42, sigTx: signed}))
				t.Logf("%s: with a NONE|FORKID validator signature a third party redirected the change output(s)", mn)
				// ... but not the pool's own outputs
				drain := c
				drain.outs = advCopy(c.outs)
				drain.outs[0] = out{1, f.poolScript(pool{bsv: 1, tokens: p.tokens, lp: p.lp, validator: next(p.validator)})}
				drain.outs[1] = out{p.bsv - 1, p2pkh(thief)}
				mustFail(t, f.advRun(t, p, drain, advOpts{sigFlag: 0x42, sigTx: signed}))
			})
			t.Run(mn+": ALL|ANYONECANPAY|FORKID signature with an input added", func(t *testing.T) {
				// The validator's signature then no longer binds the input set it vetted:
				// a third party can add inputs (outputs stay pinned by the contract).
				c := advValid(f, p, method, bsvIn)
				signed, _, _ := f.advTx(p, c, advOpts{})
				err := f.advRun(t, p, c, advOpts{sigFlag: 0xC1, sigTx: signed, extraIns: 1})
				t.Logf("%s with an ANYONECANPAY signature and an extra input: accepted=%v", mn, err == nil)
				mustPass(t, err)
			})
			t.Run(mn+": ALL|FORKID signature with an input added", func(t *testing.T) {
				c := advValid(f, p, method, bsvIn)
				signed, _, _ := f.advTx(p, c, advOpts{})
				mustFail(t, f.advRun(t, p, c, advOpts{sigTx: signed, extraIns: 1}))
			})
		}
	})
}

// Close commits to no outputs. If the LP signs with a type that leaves outputs
// out of the signature, a third party who sees the signed Close can rebuild the
// transaction with other outputs and reuse the signature. That is the LP's own
// choice (wallets sign ALL|FORKID); this documents the consequence.
//   - NONE: every output replaceable. SINGLE: all but the one at the pool's
//     input index. ALL|ANYONECANPAY: outputs fixed, inputs addable.
//     NONE|ANYONECANPAY: everything but the pool outpoint replaceable.
func TestTheft_CloseWithPartialSighashSignature(t *testing.T) {
	f := advFix(t, advCfg{30, 5, 10})
	p := advPool(t)
	thief := pkh(key(t, 0x66))
	signed := f.closeCall(p)
	signedTx, _, _ := f.advTx(p, signed, advOpts{})

	cases := []struct {
		name       string
		flag       sighash.Flag
		allOuts    bool // every output replaced
		firstOnly  bool // only output 0 (the pool's input index) kept, rest replaced
		replaceOne bool // output 0 replaced, others kept
		extraIn    bool // an input added
		wantPass   bool
	}{
		{"ALL: all outputs replaced", 0x41, true, false, false, false, false},
		{"ALL: an input added", 0x41, false, false, false, true, false},
		{"NONE: all outputs replaced", 0x42, true, false, false, false, true},
		{"NONE: an input added", 0x42, false, false, false, true, false},
		{"SINGLE: outputs other than 0 replaced", 0x43, false, true, false, false, true},
		{"SINGLE: output 0 replaced", 0x43, false, false, true, false, false},
		{"ALL|ANYONECANPAY: all outputs replaced", 0xC1, true, false, false, false, false},
		{"ALL|ANYONECANPAY: an input added", 0xC1, false, false, false, true, true},
		{"NONE|ANYONECANPAY: all outputs replaced", 0xC2, true, false, false, false, true},
		{"NONE|ANYONECANPAY: outputs replaced and an input added", 0xC2, true, false, false, true, true},
		{"SINGLE|ANYONECANPAY: outputs other than 0 replaced, an input added", 0xC3, false, true, false, true, true},
		{"SINGLE|ANYONECANPAY: output 0 replaced", 0xC3, false, false, true, false, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			c := f.closeCall(p)
			steal := []out{{p.bsv - 500, p2pkh(thief)}, {1, tokenP2pkh(p.tokens, thief)}}
			switch {
			case tc.allOuts:
				c.outs = steal
			case tc.firstOnly:
				c.outs = []out{signed.outs[0], steal[1], {1, p2pkh(thief)}}
			case tc.replaceOne:
				c.outs = []out{steal[0], signed.outs[1]}
			}
			o := advOpts{sigFlag: tc.flag, sigTx: signedTx}
			if tc.extraIn {
				o.extraIns = 1
			}
			err := f.advRun(t, p, c, o)
			if tc.wantPass {
				if err != nil {
					t.Fatalf("expected the third party's rebuilt Close to be accepted: %v", err)
				}
				t.Logf("third party redirected the pool's funds with the LP's %s signature", tc.name)
			} else {
				mustFail(t, err)
			}
		})
	}
}

// --- 3. output-set attacks on Swap ---

func advCopy(o []out) []out { return append([]out{}, o...) }

func TestTheft_SwapOutputSet(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		r := f.expectSwap(p, 20_000, bsvIn)
		base := f.swapCall(p, 20_000, bsvIn, r)
		attacker := pkh(key(t, 0x66))
		try := func(name string, outs []out) {
			t.Run(name, func(t *testing.T) {
				c := base
				c.outs = outs
				mustFail(t, f.run(t, p, c))
			})
		}
		extra := out{1, p2pkh(attacker)}
		generic := func(prefix string, base call) {
			try := func(name string, outs []out) {
				t.Run(prefix+name, func(t *testing.T) {
					c := base
					c.outs = outs
					mustFail(t, f.run(t, p, c))
				})
			}
			n := len(base.outs)
			for i := 0; i <= n; i++ {
				o := advCopy(base.outs)
				o = append(o[:i], append([]out{extra}, o[i:]...)...)
				try(fmt.Sprintf("extra output inserted at %d (of %d)", i, n), o)
			}
			try("extra OP_RETURN output appended", append(advCopy(base.outs), out{0, []byte{0x6a, 0x01, 0x00}}))
			try("extra token output appended", append(advCopy(base.outs), out{1, tokenP2pkh(5, attacker)}))
			for i := 0; i < n; i++ {
				o := advCopy(base.outs)
				try(fmt.Sprintf("output %d removed", i), append(o[:i], o[i+1:]...))
			}
			for i := 0; i+1 < n; i++ {
				o := advCopy(base.outs)
				o[i], o[i+1] = o[i+1], o[i]
				try(fmt.Sprintf("outputs %d and %d swapped", i, i+1), o)
			}
		}
		generic("", base)
		generic("with token and sats change: ", f.withChange(base, 7_777, pkh(key(t, 70)), 12_345))
		try("pool continuation moved last", append(advCopy(base.outs)[1:], base.outs[0]))

		// payout
		payout := base.outs[1]
		split := func(a, b out) []out {
			o := advCopy(base.outs)
			return append(append(o[:1], a, b), o[2:]...)
		}
		if bsvIn {
			try("payout split in two token outputs summing to out", split(out{1, tokenP2pkh(r.out/2, f.userPkh)}, out{1, tokenP2pkh(r.out-r.out/2, f.userPkh)}))
			try("payout to another pkh than userPkh", append([]out{base.outs[0], {1, tokenP2pkh(r.out, attacker)}}, base.outs[2:]...))
			try("payout locked by a bare script after the token prefix", append([]out{base.outs[0], {1, append(tokenPrefix(r.out), 0x51)}}, base.outs[2:]...))
			try("payout of 2 sats", append([]out{base.outs[0], {2, payout.script}}, base.outs[2:]...))
			try("payout of 0 sats", append([]out{base.outs[0], {0, payout.script}}, base.outs[2:]...))
			try("payout without the token prefix (plain sats)", append([]out{base.outs[0], {1, p2pkh(f.userPkh)}}, base.outs[2:]...))
			try("payout of a larger token amount", append([]out{base.outs[0], {1, tokenP2pkh(r.out+1, f.userPkh)}}, base.outs[2:]...))
		} else {
			try("payout split in two outputs summing to out", split(out{r.out / 2, p2pkh(f.userPkh)}, out{r.out - r.out/2, p2pkh(f.userPkh)}))
			try("payout to another pkh than userPkh", append([]out{base.outs[0], {r.out, p2pkh(attacker)}}, base.outs[2:]...))
			try("payout as a bare OP_TRUE script", append([]out{base.outs[0], {r.out, []byte{0x51}}}, base.outs[2:]...))
			try("payout as bare pubkey script", append([]out{base.outs[0], {r.out, append(append([]byte{33}, key(t, 0x66).PubKey().Compressed()...), 0xac)}}, base.outs[2:]...))
			try("payout with a token prefix (BSV expected)", append([]out{base.outs[0], {r.out, tokenP2pkh(r.out, f.userPkh)}}, base.outs[2:]...))
			try("payout of a token output of 1 sat", append([]out{base.outs[0], {1, tokenP2pkh(r.out, f.userPkh)}}, base.outs[2:]...))
		}

		// fees
		var feeAmts []int64
		var feeTo [][]byte
		for _, x := range []struct {
			a  int64
			to []byte
		}{{r.lpFee, p.lpFeePkh(bsvIn)}, {r.validatorFee, pkh(p.validator)}, {r.commission, f.commissionPkh}} {
			if x.a > 0 {
				feeAmts, feeTo = append(feeAmts, x.a), append(feeTo, x.to)
			}
		}
		for i := range feeAmts {
			i := i
			o := advCopy(base.outs)
			if bsvIn {
				o[2+i] = out{1, tokenP2pkh(feeAmts[i], feeTo[i])}
			} else {
				o[2+i] = out{feeAmts[i], p2pkh(feeTo[i])}
			}
			try(fmt.Sprintf("fee %d paid in the wrong asset", i), o)
			o = advCopy(base.outs)
			o[2+i].sats += 1
			if !bsvIn {
				o[2+i].sats = 2 // a token fee of more than 1 sat
			}
			try(fmt.Sprintf("fee %d output carries more value", i), o)
		}
		if len(feeAmts) >= 2 {
			var sum int64
			for _, a := range feeAmts {
				sum += a
			}
			merged := advCopy(base.outs[:2])
			if bsvIn {
				merged = append(merged, out{sum, p2pkh(feeTo[0])})
			} else {
				merged = append(merged, out{1, tokenP2pkh(sum, feeTo[0])})
			}
			try("all fees merged into one output to the LP fee address", merged)
		}

		// continuation
		try("continuation with 1 sat fewer", append([]out{{base.outs[0].sats - 1, base.outs[0].script}}, base.outs[1:]...))
		try("continuation with 1 sat more", append([]out{{base.outs[0].sats + 1, base.outs[0].script}}, base.outs[1:]...))
		prefixLen := len(tokenPrefix(r.newTokens))
		codeStart := prefixLen
		stride := len(f.code) / 23
		for off := 0; off < len(f.code); off += stride {
			off := off
			cont := advCopy(base.outs)
			s := append([]byte{}, cont[0].script...)
			s[codeStart+off] ^= 0x01
			cont[0].script = s
			try(fmt.Sprintf("continuation code byte %d changed", off), cont)
		}
		// state of the continuation
		post := r.pool(p)
		st := post.state()
		for _, fld := range []struct {
			name      string
			off, size int
		}{{"TokenReserve", offTokens, 8}, {"LpPubKey", offLp, 33}, {"ValidatorPubKey", offVal, 33}, {"ValidatorIdentity", offIdentity, 33},
			{"Nonce", offNonce, 32}, {"LpSatsPkh", offSats, 20}, {"LpTokenPkh", offTokenPkh, 20}, {"Affirmation", offAffLen + 1, len(affirmation)}} {
			for _, which := range []string{"first", "last"} {
				fld, which := fld, which
				s2 := append([]byte{}, st...)
				idx := fld.off
				if which == "last" {
					idx = fld.off + fld.size - 1
				}
				s2[idx] ^= 0x01
				cont := advCopy(base.outs)
				cont[0].script = advLock(tokenPrefix(r.newTokens), f.code, s2)
				try(fmt.Sprintf("continuation state: %s %s byte flipped", fld.name, which), cont)
			}
		}
		for _, v := range []struct {
			name string
			k    []byte
		}{{"another real key", key(t, 0x31).PubKey().Compressed()}, {"33 zero bytes", make([]byte, 33)}, {"the current key (not rotated)", p.validator.PubKey().Compressed()}} {
			v := v
			s2 := append([]byte{}, st...)
			copy(s2[offVal:], v.k)
			cont := advCopy(base.outs)
			cont[0].script = advLock(tokenPrefix(r.newTokens), f.code, s2)
			try("continuation ValidatorPubKey != nextValidatorPubKey: "+v.name, cont)
		}
		for _, v := range []struct {
			name string
			s    []byte
		}{
			{"Affirmation one byte shorter", append(append([]byte{}, st[:offAffLen]...), append([]byte{byte(len(affirmation) - 1)}, affirmation[:len(affirmation)-1]...)...)},
			{"Affirmation one byte longer", append(append([]byte{}, st[:offAffLen]...), append([]byte{byte(len(affirmation) + 1)}, append(append([]byte{}, affirmation...), 0x00)...)...)},
			{"a trailing byte after the state", append(append([]byte{}, st...), 0x00)},
			{"state with its last byte missing", st[:len(st)-1]},
		} {
			v := v
			cont := advCopy(base.outs)
			cont[0].script = advLock(tokenPrefix(r.newTokens), f.code, v.s)
			try("continuation state: "+v.name, cont)
		}
		// another pool's code
		for _, v := range []struct {
			name string
			code []byte
			pre  []byte
		}{
			{"another AssetId in the code (prefix kept)", advCode(t, bytes.Repeat([]byte{0xb6}, 32), f.lpFeeBps, f.validatorBps, f.commissionBps), tokenPrefix(r.newTokens)},
			{"another AssetId in code and prefix", advCode(t, bytes.Repeat([]byte{0xb6}, 32), f.lpFeeBps, f.validatorBps, f.commissionBps),
				append(append([]byte{0x20}, bytes.Repeat([]byte{0xb6}, 32)...), append(pushNum(r.newTokens), 0x6d)...)},
			{"another LpFeeBps", advCode(t, assetID, f.lpFeeBps+1, f.validatorBps, f.commissionBps), tokenPrefix(r.newTokens)},
			{"another ValidatorFeeBps", advCode(t, assetID, f.lpFeeBps, f.validatorBps+1, f.commissionBps), tokenPrefix(r.newTokens)},
			{"another CommissionBps", advCode(t, assetID, f.lpFeeBps, f.validatorBps, f.commissionBps+1), tokenPrefix(r.newTokens)},
			{"all fees zero", advCode(t, assetID, 0, 0, 0), tokenPrefix(r.newTokens)},
		} {
			v := v
			if bytes.Equal(v.code, f.code) {
				continue // the all-zero config is the pool's own code
			}
			cont := advCopy(base.outs)
			cont[0].script = advLock(v.pre, v.code, st)
			try("continuation is another pool: "+v.name, cont)
		}
	})
}

// --- 4. drain attempts without the validator key ---

func TestTheft_TakerCannotDrain(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		taker := advRand(t)
		takerPkh := pkh(taker)

		// drain: everything but one sat to the taker, a 1-sat continuation.
		drainOuts := func(pp pool) []out {
			cont := pool{bsv: 1, tokens: pp.tokens, lp: pp.lp, validator: next(pp.validator)}
			return []out{{1, f.poolScript(cont)}, {pp.bsv - 1, p2pkh(takerPkh)}}
		}
		t.Run("Swap drain signed by the taker", func(t *testing.T) {
			c := f.swapCall(p, 20_000, bsvIn, f.expectSwap(p, 20_000, bsvIn))
			c.outs = drainOuts(p)
			c.args = append([][]byte{}, c.args...)
			c.args[1] = pushData(taker.PubKey().Compressed())
			mustFail(t, f.advRun(t, p, advSetSigner(c, taker), advOpts{}))
		})
		t.Run("Swap with a huge amountIn signed by the taker", func(t *testing.T) {
			c := f.swapCall(p, 20_000, bsvIn, f.expectSwap(p, 20_000, bsvIn))
			c.args = append([][]byte{}, c.args...)
			c.args[2] = pushNum(5_000_000_000)
			mustFail(t, f.advRun(t, p, advSetSigner(c, taker), advOpts{}))
		})
		t.Run("Swap with no signature at all", func(t *testing.T) {
			c := f.swapCall(p, 20_000, bsvIn, f.expectSwap(p, 20_000, bsvIn))
			mustFail(t, f.advRun(t, p, advSetRaw(c, []byte{0x00}), advOpts{}))
		})
		t.Run("the validator's published signature reused on a drain transaction", func(t *testing.T) {
			honest := f.swapCall(p, 20_000, bsvIn, f.expectSwap(p, 20_000, bsvIn))
			unlock := f.advUnlockOf(t, p, honest)
			mustPass(t, f.advRunUnlock(t, p, honest, unlock)) // control: the honest tx
			drain := honest
			drain.outs = drainOuts(p)
			mustFail(t, f.advRunUnlock(t, p, drain, unlock))
			// Same outputs, one more input: the preimage commits to the input set.
			tx, prevOut, _ := f.advTx(p, honest, advOpts{extraIns: 1})
			tx.Inputs[0].UnlockingScript = script.NewFromBytes(unlock)
			mustFail(t, advExecute(tx, 0, prevOut))
		})
		t.Run("the validator's Swap signature offered to Close and Rescind", func(t *testing.T) {
			// A validator signature over the honest Swap tx, pushed as the sig of the
			// other methods (the sighash differs per tx, and the keys differ).
			honest := f.swapCall(p, 20_000, bsvIn, f.expectSwap(p, 20_000, bsvIn))
			tx, _, lock := f.advTx(p, honest, advOpts{})
			dig := advDigest(t, tx, 0, lock, uint64(p.bsv), sighash.AllForkID)
			sig, _ := p.validator.Sign(dig)
			raw := pushData(append(sig.Serialize(), 0x41))
			cl := advSetRaw(f.closeCall(p), raw)
			cl.outs = honest.outs
			mustFail(t, f.advRun(t, p, cl, advOpts{}))
			rs := advSetRaw(f.rescindCall(p), raw)
			mustFail(t, f.advRun(t, p, rs, advOpts{}))
		})
		t.Run("Close and Rescind signed by the taker", func(t *testing.T) {
			cl := advSetSigner(f.closeCall(p), taker)
			cl.outs = []out{{p.bsv, p2pkh(takerPkh)}, {1, tokenP2pkh(p.tokens, takerPkh)}}
			mustFail(t, f.advRun(t, p, cl, advOpts{}))
			mustFail(t, f.advRun(t, p, advSetSigner(f.rescindCall(p), taker), advOpts{}))
		})
		t.Run("an unlocking script with no arguments", func(t *testing.T) {
			for _, u := range [][]byte{{0x51}, {0x00}, {}, pushData(f.code), append(pushData(f.code), 0x51)} {
				mustFail(t, f.advRunUnlock(t, p, f.closeCall(p), u))
			}
		})
		t.Run("the taker's arguments with every method index and the taker's key", func(t *testing.T) {
			for m := 0; m < 3; m++ {
				c := advValid(f, p, m, bsvIn)
				c = advSetSigner(c, taker)
				for sel := int64(0); sel < 3; sel++ {
					mustFail(t, f.advRun(t, p, c, advOpts{method: pushNum(sel)}))
				}
			}
		})
	})
}

// --- 5. trust model (by design) ---

// TestTrustModel_ValidatorDrainsWithFakeTokenInput: BY DESIGN. The script cannot
// see token inputs, so a token-in Swap signed by the validator key with a huge
// amountIn and no real token input is accepted, and pays out nearly the whole BSV
// reserve. The off-chain check carrying the weight: the validator verifies that
// the swap's token inputs are valid and cover amountIn before it signs.
func TestTrustModel_ValidatorDrainsWithFakeTokenInput(t *testing.T) {
	advEachCfg(t, func(t *testing.T, f *fixture, p pool) {
		for _, mult := range []int64{10, 1000, 1_000_000_000} {
			mult := mult
			t.Run(fmt.Sprintf("amountIn = %dx the token reserve", mult), func(t *testing.T) {
				amountIn := mult * p.tokens
				r := advBigSwap(f, p, amountIn, false)
				// The transaction has no token input at all (run adds one plain input).
				mustPass(t, f.run(t, p, f.swapCall(p, amountIn, false, r)))
				t.Logf("amountIn %d tokens (%dx reserve): script pays out %d of %d sats = %.9f of the BSV reserve; %d sat stays",
					amountIn, mult, r.out, p.bsv, float64(r.out)/float64(p.bsv), r.newBsv)
			})
		}
	})
}

// TestTrustModel_ValidatorNamesUnusableNextKey: BY DESIGN. nextValidatorPubKey is
// not checked: not a curve point, or a key nobody holds, is accepted. Afterwards
// no Swap or Rescind is possible; the LP can still Close. Off-chain: the
// validator derives the next key (BRC-42) and the lookup lists no pool whose key
// breaks the derivation.
func TestTrustModel_ValidatorNamesUnusableNextKey(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		for _, kc := range []struct {
			name string
			k    []byte
		}{
			{"not a curve point (33 x 0x05)", bytes.Repeat([]byte{0x05}, 33)},
			{"33 zero bytes", make([]byte, 33)},
			{"a valid key nobody holds", func() []byte { k := key(nil, 0x45); return k.PubKey().Compressed() }()},
		} {
			kc := kc
			t.Run(kc.name, func(t *testing.T) {
				r := f.expectSwap(p, 20_000, bsvIn)
				c := f.swapCall(p, 20_000, bsvIn, r)
				c.args = append([][]byte{}, c.args...)
				c.args[1] = pushData(kc.k)
				c.outs = advCopy(c.outs)
				s := append([]byte{}, c.outs[0].script...)
				copy(s[len(s)-len(r.pool(p).state())+offVal:], kc.k)
				c.outs[0].script = s
				mustPass(t, f.run(t, p, c))

				stuck := p
				stuck.bsv, stuck.tokens = r.newBsv, r.newTokens
				stuck.lock = s
				// no one signs a Swap or Rescind
				for _, k := range []*ec.PrivateKey{p.validator, r.nextValidator, p.lp, advRand(t)} {
					sw := f.swapCall(stuck, 20_000, bsvIn, f.expectSwap(stuck, 20_000, bsvIn))
					mustFail(t, f.advRun(t, stuck, advSetSigner(sw, k), advOpts{}))
					mustFail(t, f.advRun(t, stuck, advSetSigner(f.rescindCall(stuck), k), advOpts{}))
				}
				mustFail(t, f.advRun(t, stuck, advSetRaw(f.rescindCall(stuck), []byte{0x00}), advOpts{}))
				// the LP still closes
				cl := f.closeCall(stuck)
				mustPass(t, f.advRun(t, stuck, cl, advOpts{}))
			})
		}
	})
}

// TestTheft_NextKeyLength_PinnedRunarAccepts records what happens when the next
// key is not 33 bytes. Not a by-design case: on the pinned Rúnar a validator-signed
// Swap with such a key is ACCEPTED and writes a continuation that is not the
// pool's code. Rúnar main checks the parameter's length and refuses it. On
// the pin the continuation's state differs in length from the spent one, and
// poolScript recovers the code by subtracting the NEW state's length, so the
// continuation's code part is off by that many bytes. The test then tries to
// spend that continuation (the LP's Close among others): every attempt is
// refused, so the pool's funds are stuck.
func TestTheft_NextKeyLength_PinnedRunarAccepts(t *testing.T) {
	f := advFix(t, advCfg{30, 5, 10})
	p := advPool(t)
	r := f.expectSwap(p, 20_000, true)
	head := advIsHead(t)
	t.Logf("Rúnar HEAD (codePartLenSlots present): %v", head)
	for _, n := range []int{0, 1, 32, 34, 65} {
		n := n
		t.Run(fmt.Sprintf("next key of %d bytes", n), func(t *testing.T) {
			k := bytes.Repeat([]byte{0x05}, n)
			c := f.swapCall(p, 20_000, true, r)
			c.args = append([][]byte{}, c.args...)
			c.args[1] = pushData(k)
			// The continuation as the contract would build it: state with the key
			// at its slot, code = spent scriptCode minus the new state's length.
			st := r.pool(p).state()
			ns := append(append(append([]byte{}, st[:offVal]...), k...), st[offVal+33:]...)
			// try both candidate shapes; log which (if any) is accepted
			var accepted []string
			cands := map[string][]byte{}
			cands["code+state(len adjusted)"] = advLock(tokenPrefix(r.newTokens), f.code, ns)
			full := append(append([]byte{}, f.code...), 0x6a)
			full = append(full, p.state()...)
			body := full[:len(full)-len(ns)]
			cands["scriptCode minus new state length"] = append(append(append([]byte{}, tokenPrefix(r.newTokens)...), body...), ns...)
			for name, s := range cands {
				cc := c
				cc.outs = advCopy(c.outs)
				cc.outs[0].script = s
				err := f.advRun(t, p, cc, advOpts{})
				t.Logf("  shape %q: %v", name, err)
				if err == nil {
					accepted = append(accepted, name)
				}
			}
			t.Logf("next key %d bytes: accepted shapes = %v", n, accepted)
			// Observed. Rúnar HEAD: refused (OP_NUMEQUALVERIFY, a length check on the
			// PubKey parameter). Pinned Rúnar: no check; the continuation is then the
			// spent scriptCode minus the NEW state's length, i.e. a pool whose code is
			// not the pool's code (truncated or extended). Validator-only.
			if head {
				if len(accepted) > 0 {
					t.Errorf("HEAD: a %d-byte next key was accepted: %v", n, accepted)
				}
			} else if len(accepted) != 1 || accepted[0] != "scriptCode minus new state length" {
				t.Errorf("pinned: expected the shifted-code continuation to be accepted, got %v", accepted)
			}
			if head || len(accepted) == 0 {
				return
			}
			// Spend the mis-shaped pool: its lock is prefix || code' || ns.
			bad := pool{bsv: r.newBsv, tokens: r.newTokens, lp: p.lp, validator: r.nextValidator, lock: cands[accepted[0]]}
			clip := func(b []byte) []byte { return b[:len(b)-len(ns)] }
			shaped := clip(cands[accepted[0]][len(tokenPrefix(r.newTokens)):])
			for _, cp := range []struct {
				name string
				code []byte
			}{{"original code part", nil}, {"the mis-shaped lock's own code", shaped}} {
				for _, who := range []struct {
					name string
					k    *ec.PrivateKey
				}{{"LP", p.lp}, {"random key", advRand(t)}, {"first validator", p.validator}} {
					err := f.advRun(t, bad, advSetSigner(advCloseTo(bad, pkh(who.k)), who.k), advOpts{codePart: cp.code})
					t.Logf("  mis-shaped pool, Close by %s, %s: accepted=%v (%v)", who.name, cp.name, err == nil, err)
					// Observed: no one, the LP included, spends it with these attempts.
					if err == nil {
						t.Errorf("mis-shaped pool spent by %s (%s)", who.name, cp.name)
					}
				}
			}
		})
	}
}

// TestTrustModel_ValidatorRescindsAnytime: BY DESIGN. The validator can Rescind
// at any time (it withdraws its attestation; nothing is taken from the pool),
// and the LP still closes. Off-chain: the lookup delists a rescinded pool.
func TestTrustModel_ValidatorRescindsAnytime(t *testing.T) {
	advEachCfg(t, func(t *testing.T, f *fixture, p pool) {
		cur := p
		for i := 0; i < 3; i++ {
			r := f.expectSwap(cur, 20_000, i%2 == 0)
			mustPass(t, f.run(t, cur, f.swapCall(cur, 20_000, i%2 == 0, r)))
			cur = r.pool(cur)
		}
		mustPass(t, f.advRun(t, cur, f.rescindCall(cur), advOpts{}))
		res := cur
		res.rescinded = true
		mustPass(t, f.advRun(t, res, f.closeCall(res), advOpts{}))
	})
}

// TestTrustModel_RelayAndTakerNameAnyAddress: BY DESIGN. commissionPkh is the
// relay's choice and userPkh the taker's; the contract only checks the outputs
// match whatever the validator signed. Off-chain: the validator signs only what
// the taker requested.
func TestTrustModel_RelayAndTakerNameAnyAddress(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		for i, k := range []byte{0x50, 0x51, 0x7f} {
			g := *f
			g.commissionPkh = pkh(key(t, k))
			g.userPkh = pkh(key(t, k+1))
			t.Run(fmt.Sprintf("arbitrary commissionPkh and userPkh #%d", i), func(t *testing.T) {
				mustPass(t, g.run(t, p, g.swapCall(p, 20_000, bsvIn, g.expectSwap(p, 20_000, bsvIn))))
			})
		}
		t.Run("userPkh equal to the pool's own validator pkh", func(t *testing.T) {
			g := *f
			g.userPkh = pkh(p.validator)
			mustPass(t, g.run(t, p, g.swapCall(p, 20_000, bsvIn, g.expectSwap(p, 20_000, bsvIn))))
		})
		t.Run("userPkh and commissionPkh 20 zero bytes", func(t *testing.T) {
			g := *f
			g.userPkh, g.commissionPkh = make([]byte, 20), make([]byte, 20)
			mustPass(t, g.run(t, p, g.swapCall(p, 20_000, bsvIn, g.expectSwap(p, 20_000, bsvIn))))
		})
	})
}

// --- 6. Close always works ---

func advCloseTo(p pool, to []byte) call {
	return call{
		method:   methodClose,
		args:     [][]byte{nil},
		outs:     []out{{p.bsv, p2pkh(to)}, {1, tokenP2pkh(p.tokens, to)}},
		signers:  []*ec.PrivateKey{p.lp},
		sigSlots: []int{0},
	}
}

func TestTheft_CloseAlwaysWorks(t *testing.T) {
	advEachCfg(t, func(t *testing.T, f *fixture, p0 pool) {
		afterSwaps := p0
		for i := 0; i < 4; i++ {
			r := f.expectSwap(afterSwaps, 15_000+int64(i)*1000, i%2 == 0)
			amt := 15_000 + int64(i)*1000
			mustPass(t, f.run(t, afterSwaps, f.swapCall(afterSwaps, amt, i%2 == 0, r)))
			afterSwaps = r.pool(afterSwaps)
		}
		rescinded := p0
		rescinded.rescinded = true
		afterSwapsThenRescind := afterSwaps
		afterSwapsThenRescind.rescinded = true

		states := []struct {
			name string
			p    pool
			idx  int
			outs []out
		}{
			{"fresh pool", p0, 0, nil},
			{"after 4 swaps", afterSwaps, 0, nil},
			{"after Rescind", rescinded, 0, nil},
			{"after swaps and Rescind", afterSwapsThenRescind, 0, nil},
			{"bsv reserve 1 sat", pool{bsv: 1, tokens: 5_000_000, lp: p0.lp, validator: p0.validator}, 0, nil},
			{"token reserve 1", pool{bsv: 1_000_000, tokens: 1, lp: p0.lp, validator: p0.validator}, 0, nil},
			{"bsv 1 sat and token 1", pool{bsv: 1, tokens: 1, lp: p0.lp, validator: p0.validator}, 0, nil},
			{"pool at input index 1", p0, 1, nil},
			{"one OP_RETURN output only", p0, 0, []out{{0, []byte{0x6a}}}},
			{"one unrelated 1-sat output", p0, 0, []out{{1, p2pkh(pkh(key(t, 0x62)))}}},
			{"many outputs", p0, 0, func() []out {
				var o []out
				for i := 0; i < 60; i++ {
					o = append(o, out{1, p2pkh(pkh(key(t, byte(0x80+i))))})
				}
				return o
			}()},
		}
		for _, s := range states {
			s := s
			mk := func() call {
				c := advCloseTo(s.p, pkh(s.p.lp))
				c.poolIndex = s.idx
				if s.outs != nil {
					c.outs = s.outs
				}
				return c
			}
			t.Run(s.name+": the LP closes", func(t *testing.T) {
				mustPass(t, f.advRun(t, s.p, mk(), advOpts{}))
			})
			for _, w := range []struct {
				name string
				k    *ec.PrivateKey
			}{{"validator", s.p.validator}, {"first validator", p0.validator}, {"identity", key(t, 0x7f)}, {"random", advRand(t)}} {
				w := w
				t.Run(s.name+": "+w.name+" cannot close", func(t *testing.T) {
					mustFail(t, f.advRun(t, s.p, advSetSigner(mk(), w.k), advOpts{}))
				})
			}
			t.Run(s.name+": empty signature cannot close", func(t *testing.T) {
				mustFail(t, f.advRun(t, s.p, advSetRaw(mk(), []byte{0x00}), advOpts{}))
			})
		}
	})
}

// --- 7. method selector and argument confusion ---

// The selector is compared as a script number: any encoding of the method's
// value dispatches to it (non-minimal ones, PUSHDATA forms, a leading sign
// byte that leaves the value alone); negative values never match. Observed.
func TestTheft_MethodSelector(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		t.Run("layout x selector matrix: only the matching selector runs", func(t *testing.T) {
			for m := 0; m < 3; m++ {
				for _, v := range []int64{0, 1, 2, 3, -1, 100, 16} {
					m, v := m, v
					err := f.advRun(t, p, advValid(f, p, m, bsvIn), advOpts{method: pushNum(v)})
					if int64(m) == v && err != nil {
						t.Errorf("%s layout, selector %d: %v", advMethodNames[m], v, err)
					}
					if int64(m) != v && err == nil {
						t.Errorf("%s layout dispatched by selector %d", advMethodNames[m], v)
					}
				}
			}
		})
		t.Run("encodings of a value dispatch by their numeric value", func(t *testing.T) {
			type encoding struct {
				name string
				push []byte
				val  int64
			}
			for m := 0; m < 3; m++ {
				v := byte(m)
				encs := []encoding{
					{"OP_PUSHBYTES_1 v", []byte{0x01, v}, int64(m)},
					{"two-byte v 00", []byte{0x02, v, 0x00}, int64(m)},
					{"PUSHDATA1 v", []byte{0x4c, 0x01, v}, int64(m)},
					{"PUSHDATA2 v", []byte{0x4d, 0x01, 0x00, v}, int64(m)},
					{"negative zero (80)", []byte{0x01, 0x80}, 0},
					{"two-byte negative zero (00 80)", []byte{0x02, 0x00, 0x80}, 0},
					{"v with the sign bit set (-v)", []byte{0x02, v, 0x80}, -int64(m)},
					{"empty push", []byte{0x00}, 0},
					{"minimal", pushNum(int64(m)), int64(m)},
					{"boolean-ish OP_TRUE", []byte{0x51}, 1},
					{"boolean-ish OP_FALSE", []byte{0x00}, 0},
				}
				for _, e := range encs {
					err := f.advRun(t, p, advValid(f, p, m, bsvIn), advOpts{method: e.push})
					want := e.val == int64(m)
					if want != (err == nil) {
						t.Errorf("%s layout, selector %s (%x, value %d): accepted=%v, want %v (%v)", advMethodNames[m], e.name, e.push, e.val, err == nil, want, err)
					}
				}
			}
		})
		t.Run("a second push after the selector, every layout", func(t *testing.T) {
			for m := 0; m < 3; m++ {
				for v := int64(0); v < 3; v++ {
					err := f.advRun(t, p, advValid(f, p, m, bsvIn), advOpts{trailing: [][]byte{pushNum(v)}})
					if err == nil {
						t.Errorf("%s layout accepted with a trailing push of %d", advMethodNames[m], v)
					}
				}
			}
		})
		t.Run("a missing preimage", func(t *testing.T) {
			for m := 0; m < 3; m++ {
				mustFail(t, f.advRun(t, p, advValid(f, p, m, bsvIn), advOpts{noPre: true}))
			}
		})
	})
}

// Extra pushes before the declared arguments, with the right and the wrong
// signer, for every method.
func TestTheft_ExtraLeadingPushes(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		junk := [][]byte{pushData(bytes.Repeat([]byte{0xee}, 33)), pushNum(1), pushNum(0)}
		for method := 0; method < 3; method++ {
			method := method
			mn := advMethodNames[method]
			// signatures by the other parties over the real tx, usable as the extra pushes
			txr, _, lock := f.advTx(p, advValid(f, p, method, bsvIn), advOpts{})
			dig := advDigest(t, txr, 0, lock, uint64(p.bsv), sighash.AllForkID)
			sigOf := func(k *ec.PrivateKey) []byte {
				s, _ := k.Sign(dig)
				return pushData(append(s.Serialize(), 0x41))
			}
			wrongKey := p.lp
			if method == methodClose {
				wrongKey = p.validator
			}
			rightKey := p.validator
			if method == methodClose {
				rightKey = p.lp
			}
			for n := 1; n <= 3; n++ {
				n := n
				t.Run(fmt.Sprintf("%s: right signer, %d junk pushes below the arguments (recorded)", mn, n), func(t *testing.T) {
					err := f.advRun(t, p, advValid(f, p, method, bsvIn), advOpts{leading: junk[:n]})
					t.Logf("%s with %d leading junk pushes: accepted=%v (%v)", mn, n, err == nil, err)
					if err != nil {
						t.Errorf("honest spend with junk below the arguments rejected: %v", err)
					}
				})
				t.Run(fmt.Sprintf("%s: wrong signer, %d junk pushes", mn, n), func(t *testing.T) {
					mustFail(t, f.advRun(t, p, advSetSigner(advValid(f, p, method, bsvIn), wrongKey), advOpts{leading: junk[:n]}))
				})
			}
			t.Run(mn+": the right key's signature pushed below, a wrong signature in the slot", func(t *testing.T) {
				c := advSetSigner(advValid(f, p, method, bsvIn), wrongKey)
				mustFail(t, f.advRun(t, p, c, advOpts{leading: [][]byte{sigOf(rightKey)}}))
				mustFail(t, f.advRun(t, p, c, advOpts{leading: [][]byte{sigOf(rightKey), sigOf(wrongKey)}}))
				mustFail(t, f.advRun(t, p, c, advOpts{leading: [][]byte{sigOf(wrongKey), sigOf(rightKey)}}))
			})
			t.Run(mn+": a pubkey as the signature, the right signature below", func(t *testing.T) {
				c := advSetRaw(advValid(f, p, method, bsvIn), pushData(p.validator.PubKey().Compressed()))
				mustFail(t, f.advRun(t, p, c, advOpts{leading: [][]byte{sigOf(rightKey)}}))
			})
		}
		// Layout confusion: one method's arguments dispatched as another, with extra pushes
		// to line the stack up for the other layout.
		t.Run("the validator's Swap/Rescind signature in the Close layout, padded", func(t *testing.T) {
			for _, pad := range [][][]byte{nil, {pushNum(0)}, {pushNum(0), pushNum(0)}, {pushData(make([]byte, 20)), pushNum(0)}} {
				c := advSetSigner(f.closeCall(p), p.validator)
				mustFail(t, f.advRun(t, p, c, advOpts{leading: pad}))
				c2 := advValid(f, p, methodRescind, bsvIn)
				mustFail(t, f.advRun(t, p, c2, advOpts{method: pushNum(methodClose), leading: pad}))
			}
		})
		t.Run("the LP's signature in the Swap/Rescind layouts, padded", func(t *testing.T) {
			for _, pad := range [][][]byte{nil, {pushNum(0)}, {pushNum(0), pushNum(0)}} {
				for _, m := range []int{methodSwap, methodRescind} {
					c := advSetSigner(advValid(f, p, m, bsvIn), p.lp)
					mustFail(t, f.advRun(t, p, c, advOpts{leading: pad}))
					// and dispatched as Close with the Close layout's output freedom
					cl := advSetSigner(f.closeCall(p), p.lp)
					mustPass(t, f.advRun(t, p, cl, advOpts{leading: pad})) // the LP's own Close is fine
				}
			}
		})
		t.Run("Swap layout with Close selector, validator and LP signatures", func(t *testing.T) {
			c := advValid(f, p, methodSwap, bsvIn)
			for _, k := range []*ec.PrivateKey{p.validator, p.lp} {
				mustFail(t, f.advRun(t, p, advSetSigner(c, k), advOpts{method: pushNum(methodClose)}))
				mustFail(t, f.advRun(t, p, advSetSigner(c, k), advOpts{method: pushNum(methodRescind)}))
			}
			cr := advValid(f, p, methodRescind, bsvIn)
			for _, k := range []*ec.PrivateKey{p.validator, p.lp} {
				mustFail(t, f.advRun(t, p, advSetSigner(cr, k), advOpts{method: pushNum(methodClose)}))
				mustFail(t, f.advRun(t, p, advSetSigner(cr, k), advOpts{method: pushNum(methodSwap)}))
			}
			cc := f.closeCall(p)
			for _, k := range []*ec.PrivateKey{p.validator, p.lp} {
				mustFail(t, f.advRun(t, p, advSetSigner(cc, k), advOpts{method: pushNum(methodSwap)}))
				mustFail(t, f.advRun(t, p, advSetSigner(cc, k), advOpts{method: pushNum(methodRescind)}))
			}
		})
	})
}

// --- 8. unlocking-script malleability ---

// With a validator-signed Swap in hand, a third party can edit the unlocking
// script (it is not signed). Each single mutation is tried; the outputs are
// pinned by the contract's own hashOutputs check, so the transaction's outputs
// are the same in every accepted variant. Expected acceptances: free bytes
// (commissionPkh when the commission is 0, tokenChangePkh when tokenChange is 0,
// _changePKH when _changeAmount is 0) and alternative encodings of the same
// value. Anything else accepted fails the test.
func TestTheft_UnlockingMalleability(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		r := f.expectSwap(p, 20_000, bsvIn)
		base := f.swapCall(p, 20_000, bsvIn, r)
		other20 := pushData(bytes.Repeat([]byte{0xcd}, 20))
		type mut struct {
			name     string
			wantPass bool
			apply    func(c *call, o *advOpts)
		}
		setArg := func(i int, v []byte) func(c *call, o *advOpts) {
			return func(c *call, o *advOpts) {
				c.args = append([][]byte{}, c.args...)
				c.args[i] = v
			}
		}
		noCommission := f.commissionBps == 0
		muts := []mut{
			{"nextValidatorPubKey: another key", false, setArg(1, pushData(key(t, 0x31).PubKey().Compressed()))},
			{"amountIn + 1", false, setArg(2, pushNum(20_001))},
			{"amountIn - 1", false, setArg(2, pushNum(19_999))},
			{"amountIn with a padded zero byte (same value)", true, setArg(2, pushData(append(scriptNum(20_000), 0x00)))},
			{"amountIn as PUSHDATA1 (same value)", true, setArg(2, append([]byte{0x4c, byte(len(scriptNum(20_000)))}, scriptNum(20_000)...))},
			{"bsvIn flipped", false, setArg(3, pushBool(!bsvIn))},
			{"bsvIn as 0x01 0x00 (a flip when bsv in; a non-minimal false, rejected, when tokens in)", false, setArg(3, []byte{0x01, 0x00})},
			{"bsvIn as 0x01 0x01", bsvIn, setArg(3, []byte{0x01, 0x01})},
			{"bsvIn as PUSHDATA1 of the same bool (accepted when true; non-minimal false is rejected)", bsvIn, setArg(3, map[bool][]byte{true: {0x4c, 0x01, 0x01}, false: {0x4c, 0x01, 0x00}}[bsvIn])},
			{"bsvIn as 0x02 (truthy, not 1)", false, setArg(3, []byte{0x01, 0x02})},
			{"userPkh changed", false, setArg(4, other20)},
			{"commissionPkh changed", noCommission, setArg(5, other20)},
			{"tokenChangePkh changed (tokenChange is 0)", true, setArg(6, other20)},
			{"tokenChange 0 -> 1", false, setArg(7, pushNum(1))},
			{"tokenChange 0 as 0x01 0x00", true, setArg(7, []byte{0x01, 0x00})},
			{"tokenChange 0 as negative zero", true, setArg(7, []byte{0x01, 0x80})},
			{"tokenChange -1", false, setArg(7, pushNum(-1))},
			{"_changePKH changed (_changeAmount is 0)", true, func(c *call, o *advOpts) { o.chPkh = other20 }},
			{"_changeAmount 0 -> 1", false, func(c *call, o *advOpts) { o.chAmt = pushNum(1) }},
			{"_changeAmount 0 as 0x01 0x00", true, func(c *call, o *advOpts) { o.chAmt = []byte{0x01, 0x00} }},
			{"_codePart with one byte changed", false, func(c *call, o *advOpts) {
				cp := append([]byte{}, f.code...)
				cp[len(cp)/2] ^= 1
				o.codePart = cp
			}},
			{"_codePart replaced by an empty push", false, func(c *call, o *advOpts) { o.codePart = []byte{} }},
			{"signature with S replaced by n-S (high-S; accepted by the go-sdk interpreter, node policy not tested)", true, func(c *call, o *advOpts) {
				o.sigTweak = func(b []byte) []byte {
					der := b[:len(b)-1]
					s, err := ec.ParseDERSignature(der)
					if err != nil {
						t.Fatal(err)
					}
					n := ec.S256().N
					s.S = new(big.Int).Sub(n, s.S)
					return append(s.Serialize(), b[len(b)-1])
				}
			}},
			{"signature hashtype byte 0x41 -> 0x42", false, func(c *call, o *advOpts) {
				o.sigTweak = func(b []byte) []byte { b[len(b)-1] = 0x42; return b }
			}},
			{"junk push below the arguments", true, func(c *call, o *advOpts) { o.leading = [][]byte{pushNum(7)} }},
		}
		for _, m := range muts {
			m := m
			t.Run(m.name, func(t *testing.T) {
				c := base
				o := advOpts{}
				m.apply(&c, &o)
				err := f.advRun(t, p, c, o)
				t.Logf("accepted=%v (%v)", err == nil, err)
				if m.wantPass && err != nil {
					t.Fatalf("expected the mutation to still pass: %v", err)
				}
				if !m.wantPass && err == nil {
					t.Fatal("a mutation that should invalidate the spend was accepted")
				}
			})
		}
	})
}

// --- 9. rescinded pool ---

func TestTheft_RescindedPool(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		res := p
		res.rescinded = true
		zero := make([]byte, 33)

		// The Swap the contract would accept from a pool whose key were k: fees to
		// hash160(33 zero bytes), continuation carrying the next key.
		swapFor := func(nextKey []byte) call {
			r := f.expectSwap(res, 20_000, bsvIn)
			c := f.swapCall(res, 20_000, bsvIn, r)
			c.args = append([][]byte{}, c.args...)
			c.args[1] = pushData(nextKey)
			c.outs = advCopy(c.outs)
			if r.validatorFee > 0 {
				i := 3
				if bsvIn {
					c.outs[i] = out{r.validatorFee, p2pkh(advHash160(zero))}
				} else {
					c.outs[i] = out{1, tokenP2pkh(r.validatorFee, advHash160(zero))}
				}
			}
			cont := r.pool(res)
			s := f.poolScript(cont)
			copy(s[len(s)-len(cont.state())+offVal:], nextKey)
			c.outs[0].script = s
			return c
		}
		for _, k := range []struct {
			name string
			k    *ec.PrivateKey
		}{{"first validator key", p.validator}, {"LP key", p.lp}, {"identity key", key(t, 0x7f)}, {"random key", advRand(t)}, {"next validator key", next(p.validator)}} {
			k := k
			t.Run("Swap signed by "+k.name, func(t *testing.T) {
				c := advSetSigner(swapFor(next(p.validator).PubKey().Compressed()), k.k)
				mustFail(t, f.advRun(t, res, c, advOpts{}))
			})
		}
		t.Run("Swap with an empty signature", func(t *testing.T) {
			mustFail(t, f.advRun(t, res, advSetRaw(swapFor(next(p.validator).PubKey().Compressed()), []byte{0x00}), advOpts{}))
		})
		t.Run("Swap with a 33-zero-byte 'signature'", func(t *testing.T) {
			mustFail(t, f.advRun(t, res, advSetRaw(swapFor(zero), pushData(zero)), advOpts{}))
		})
		t.Run("Swap with a 33-zero-byte next key, any signer", func(t *testing.T) {
			for _, k := range []*ec.PrivateKey{p.validator, p.lp, advRand(t)} {
				mustFail(t, f.advRun(t, res, advSetSigner(swapFor(zero), k), advOpts{}))
			}
		})
		t.Run("Swap with a 1-byte and 72-byte garbage 'signature'", func(t *testing.T) {
			for _, g := range [][]byte{{0x41}, bytes.Repeat([]byte{0x41}, 72), {0x30, 0x06, 0x02, 0x01, 0x00, 0x02, 0x01, 0x00, 0x41}} {
				mustFail(t, f.advRun(t, res, advSetRaw(swapFor(zero), pushData(g)), advOpts{}))
			}
		})
		t.Run("a second Rescind, any signer", func(t *testing.T) {
			for _, k := range []*ec.PrivateKey{p.validator, p.lp, advRand(t)} {
				mustFail(t, f.advRun(t, res, advSetSigner(f.rescindCall(res), k), advOpts{}))
			}
			mustFail(t, f.advRun(t, res, advSetRaw(f.rescindCall(res), []byte{0x00}), advOpts{}))
		})
		t.Run("Close by the LP passes", func(t *testing.T) {
			mustPass(t, f.advRun(t, res, f.closeCall(res), advOpts{}))
		})
		t.Run("Close by anyone else fails", func(t *testing.T) {
			for _, k := range []*ec.PrivateKey{p.validator, advRand(t)} {
				mustFail(t, f.advRun(t, res, advSetSigner(f.closeCall(res), k), advOpts{}))
			}
		})
	})
}

// --- 10. pool input position and two pools in one transaction ---

func TestTheft_PoolInputIndex(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pool, bsvIn bool) {
		t.Run("Swap with the pool at input 1", func(t *testing.T) {
			c := advValid(f, p, methodSwap, bsvIn)
			c.poolIndex = 1
			mustPass(t, f.run(t, p, c))
		})
		t.Run("Rescind with the pool at input 1", func(t *testing.T) {
			c := f.rescindCall(p)
			c.poolIndex = 1
			mustPass(t, f.run(t, p, c))
		})
		t.Run("wrong signer with the pool at input 1", func(t *testing.T) {
			c := advSetSigner(advValid(f, p, methodSwap, bsvIn), p.lp)
			c.poolIndex = 1
			mustFail(t, f.run(t, p, c))
		})
	})
}

// advTwoPools builds one tx with pool A at input 0 and pool B at input 1, the
// given outputs, and runs both pools' unlocking (method m, the call callA/callB
// for argument layout and signers).
func (f *fixture) advTwoPools(t *testing.T, a, b pool, ca, cb call, outs []out) (errA, errB error) {
	t.Helper()
	la, lb := f.poolScript(a), f.poolScript(b)
	oa := &transaction.TransactionOutput{Satoshis: uint64(a.bsv), LockingScript: script.NewFromBytes(la)}
	ob := &transaction.TransactionOutput{Satoshis: uint64(b.bsv), LockingScript: script.NewFromBytes(lb)}
	tx := transaction.NewTransaction()
	ia := &transaction.TransactionInput{SourceTXID: mustHash(0x11), SourceTxOutIndex: 0, SequenceNumber: 0xffffffff}
	ia.SetSourceTxOutput(oa)
	ib := &transaction.TransactionInput{SourceTXID: mustHash(0x12), SourceTxOutIndex: 0, SequenceNumber: 0xffffffff}
	ib.SetSourceTxOutput(ob)
	tx.AddInput(ia)
	tx.AddInput(ib)
	for _, o := range outs {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: uint64(o.sats), LockingScript: script.NewFromBytes(o.script)})
	}
	ca.outs, cb.outs = outs, outs
	tx.Inputs[0].UnlockingScript = script.NewFromBytes(f.advUnlock(t, tx, 0, la, uint64(a.bsv), ca, advOpts{}))
	cb.poolIndex = 1
	tx.Inputs[1].UnlockingScript = script.NewFromBytes(f.advUnlock(t, tx, 1, lb, uint64(b.bsv), cb, advOpts{}))
	return advExecute(tx, 0, oa), advExecute(tx, 1, ob)
}

func mustHash(b byte) *chainhash.Hash {
	h, _ := chainhash.NewHash(bytes.Repeat([]byte{b}, 32))
	return h
}

// Two pools spent in one transaction, both Swap. Each demands its own
// continuation at output 0 (hashOutputs is over the whole output list), so two
// distinct pools cannot both be satisfied. Two byte-identical pools (same code,
// state and sats, different outpoints) demand the same output list and CAN both
// be satisfied: the tx then carries one continuation for two pools' sats, the
// second pool's sats going to the miner. Both inputs need the validator's
// signature (same key), so this is validator-only, but it is accepted at script
// level and recorded here.
func TestTheft_TwoPoolsOneTransaction(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, a pool, bsvIn bool) {
		b := pool{bsv: 2_000_000, tokens: 7_000_000, lp: key(t, 11), validator: key(t, 21)}
		ra, rb := f.expectSwap(a, 20_000, bsvIn), f.expectSwap(b, 20_000, bsvIn)
		ca, cb := f.swapCall(a, 20_000, bsvIn, ra), f.swapCall(b, 20_000, bsvIn, rb)
		t.Run("distinct pools: neither output list satisfies both", func(t *testing.T) {
			for name, outs := range map[string][]out{"A's outputs": ca.outs, "B's outputs": cb.outs} {
				ea, eb := f.advTwoPools(t, a, b, ca, cb, outs)
				t.Logf("%s: pool A err=%v; pool B err=%v", name, ea, eb)
				if ea == nil && eb == nil {
					t.Fatalf("%s satisfied both pools", name)
				}
			}
			// A's continuation first and B's second, each pool's payout etc. after.
			mix := append([]out{ca.outs[0], cb.outs[0]}, append(advCopy(ca.outs[1:]), cb.outs[1:]...)...)
			ea, eb := f.advTwoPools(t, a, b, ca, cb, mix)
			if ea == nil && eb == nil {
				t.Fatal("a merged output list satisfied both pools")
			}
		})
		t.Run("identical pools at two outpoints: one continuation, the second pool's value redirected", func(t *testing.T) {
			// Observed: both inputs verify against ONE output list (same code, state and sats
			// give the same expected outputs). The change outputs are unlocking arguments
			// both inputs agree on, so the validator (who must sign both inputs) can send
			// the second pool's sats and a token change to any address.
			ea, eb := f.advTwoPools(t, a, a, ca, ca, ca.outs)
			if ea != nil || eb != nil {
				t.Fatalf("identical pools, one shared output list: input 0 err=%v; input 1 err=%v", ea, eb)
			}
			thief := pkh(key(t, 0x66))
			cc := f.withChange(ca, 3_000_000, thief, a.bsv)
			cc.args = append([][]byte{}, cc.args...)
			cc.args[6] = pushData(thief)
			cc.outs = advCopy(cc.outs)
			cc.outs[len(cc.outs)-2] = out{1, tokenP2pkh(3_000_000, thief)}
			ea, eb = f.advTwoPools(t, a, a, cc, cc, cc.outs)
			t.Logf("identical pools, change of %d sats and 3000000 tokens to one address: input 0 err=%v; input 1 err=%v", a.bsv, ea, eb)
			if ea != nil || eb != nil {
				t.Fatalf("expected both inputs to verify: %v / %v", ea, eb)
			}
		})
	})
}
