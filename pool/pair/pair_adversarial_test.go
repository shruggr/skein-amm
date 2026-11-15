package pair

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
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

// Adversarial tests for the pair pool and its vault. Everything runs through
// the go-sdk interpreter on a real transaction, both contract inputs executed
// with their real BIP-143 preimages. advRun is a more flexible sibling of
// fixture.run (pair_test.go): other sighash types for the pool's preimage and
// for its signer's signature, a signature made over another transaction, raw
// signature / selector / change / vaultCode pushes, extra pushes, another
// _codePart,
// plain P2PKH inputs signed and executed, version and locktime.
//
// Every run through advRun (and so every run in pair_property_test.go)
// enforces the stranding invariant: an accepted pool input always has its
// vault, output 1 of its own transaction, as input 1.

// --- runner ---

type advOpts struct {
	preFlag   sighash.Flag // the pool's txPreimage type; 0 = ALL|FORKID
	preBIP143 bool         // a non-FORKID preFlag still laid out as BIP-143 (default: the legacy serialisation)
	sigFlag   sighash.Flag // the pool signer's signature type; 0 = ALL|FORKID
	sigSpend  *spend       // the signature is made over the transaction this spend describes
	rawSig    []byte       // a raw push in place of the signature
	sigTweak  func([]byte) []byte
	method    []byte   // a raw push in place of the method index
	leading   [][]byte // pushes below _codePart
	trailing  [][]byte // pushes after the method index
	codePart  []byte   // replaces _codePart (nil: f.code); on Close, pushed as an extra first push
	chPkh     []byte   // a raw push in place of _changePKH
	chAmt     []byte   // a raw push in place of _changeAmount
	rawPrev   []byte   // a raw push in place of the pool's allPrevouts push
	rawCode   []byte   // a raw push in place of the _codePart push (on Close, an extra first push)
	rawVault  []byte   // a raw push in place of the vaultCode push (Swap, Rescind)
	noVault   bool     // omit the vaultCode push (Swap, Rescind)
	noPre     bool     // omit the pool's preimage push

	noChronicle bool                   // run the interpreter without the Chronicle upgrade
	p2pkh       map[int]*ec.PrivateKey // plain P2PKH inputs: signed (ALL|FORKID) and executed
	version     uint32
	lockTime    uint32
}

func advFlag(f sighash.Flag) sighash.Flag {
	if f == 0 {
		return sighash.AllForkID
	}
	return f
}

func advTx(s spend, o advOpts) *transaction.Transaction {
	tx := transaction.NewTransaction()
	if o.version != 0 {
		tx.Version = o.version
	}
	tx.LockTime = o.lockTime
	for _, i := range s.ins {
		h, _ := chainhash.NewHash(bytes.Repeat([]byte{i.txid}, 32))
		tx.AddInput(&transaction.TransactionInput{SourceTXID: h, SourceTxOutIndex: i.vout, SequenceNumber: 0xffffffff})
	}
	for _, x := range s.outs {
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: uint64(x.sats), LockingScript: script.NewFromBytes(x.script)})
	}
	return tx
}

// advPreimage is input idx's sighash preimage under flag, scriptCode sc.
func advPreimage(t testing.TB, tx *transaction.Transaction, idx int, sats int64, sc []byte, flag sighash.Flag, bip143 bool) []byte {
	t.Helper()
	cp := tx.ShallowClone()
	cp.Inputs[idx].SetSourceTxOutput(&transaction.TransactionOutput{Satoshis: uint64(sats), LockingScript: script.NewFromBytes(sc)})
	var pre []byte
	var err error
	if flag&sighash.ForkID != 0 || bip143 {
		pre, err = cp.CalcInputPreimage(uint32(idx), flag)
	} else {
		pre, err = cp.CalcInputPreimageLegacy(uint32(idx), flag)
	}
	if err != nil {
		t.Fatal(err)
	}
	return pre
}

// advScriptCode is afterSeparator for a contract lock with a separator after
// its prefix, and the whole lock for one without (a vault compiled without
// OP_PUSH_TX has none: no scriptCode is then signed or checked).
func advScriptCode(lock []byte) []byte {
	off := 1 + 32
	if op := lock[off]; op >= 0x51 && op <= 0x60 {
		off++
	} else {
		off += 1 + int(op)
	}
	off++ // OP_2DROP
	if off+1 >= len(lock) || lock[off+1] != 0xab {
		return lock
	}
	return lock[off+2:]
}

func advHash(pre []byte) []byte {
	h := sha256.Sum256(pre)
	d := sha256.Sum256(h[:])
	return d[:]
}

func advExec(tx *transaction.Transaction, idx int, i in, chronicle bool) error {
	opts := []interpreter.ExecutionOptionFunc{
		interpreter.WithTx(tx, idx, &transaction.TransactionOutput{Satoshis: uint64(i.sats), LockingScript: script.NewFromBytes(i.lock)}),
		interpreter.WithForkID(),
		interpreter.WithAfterGenesis(),
	}
	if chronicle {
		opts = append(opts, interpreter.WithAfterChronicle())
	}
	return interpreter.NewEngine().Execute(opts...)
}

// advAccepted counts accepted pool inputs the stranding invariant was checked on.
var advAccepted atomic.Int64

// advRunTx runs s like fixture.run, with o's deviations, and returns the
// transaction too.
func advRunTx(t testing.TB, f *fixture, s spend, o advOpts) (result, *transaction.Transaction) {
	t.Helper()
	tx := advTx(s, o)
	var real []byte
	for _, i := range s.ins {
		real = append(real, outpoint(i)...)
	}
	chron := !o.noChronicle
	for idx, k := range o.p2pkh {
		i := s.ins[idx]
		sig, err := k.Sign(advHash(advPreimage(t, tx, idx, i.sats, i.lock, sighash.AllForkID, false)))
		if err != nil {
			t.Fatal(err)
		}
		u := append(pushData(append(sig.Serialize(), 0x41)), pushData(k.PubKey().Compressed())...)
		tx.Inputs[idx].UnlockingScript = script.NewFromBytes(u)
	}

	var r result
	if s.poolIdx >= 0 {
		pi := s.poolIdx
		pin := s.ins[pi]
		pre := advPreimage(t, tx, pi, pin.sats, advScriptCode(pin.lock), advFlag(o.preFlag), o.preBIP143)
		sigPush := o.rawSig
		if sigPush == nil {
			sigTx, sigIn := tx, pin
			if o.sigSpend != nil {
				sigTx, sigIn = advTx(*o.sigSpend, o), o.sigSpend.ins[pi]
			}
			sf := advFlag(o.sigFlag)
			if o.sigFlag == 0 && s.sigFlag != 0 {
				sf = s.sigFlag
			}
			sig, err := s.signer.Sign(advHash(advPreimage(t, sigTx, pi, sigIn.sats, advScriptCode(sigIn.lock), sf, false)))
			if err != nil {
				t.Fatal(err)
			}
			b := append(sig.Serialize(), byte(sf))
			if o.sigTweak != nil {
				b = o.sigTweak(b)
			}
			sigPush = pushData(b)
		}
		var u []byte
		for _, l := range o.leading {
			u = append(u, l...)
		}
		// _codePart: Swap and Rescind take it (they write a continuation);
		// Close, with a fixed-width state, does not.
		code := f.code
		if o.codePart != nil {
			code = o.codePart
		}
		switch {
		case o.rawCode != nil:
			u = append(u, o.rawCode...)
		case s.method != methodClose || o.codePart != nil:
			u = append(u, pushData(code)...)
		}
		u = append(u, sigPush...)
		for _, a := range s.args {
			u = append(u, a...)
		}
		switch {
		case o.rawPrev != nil:
			u = append(u, o.rawPrev...)
		case s.prevouts != nil:
			u = append(u, pushData(s.prevouts)...)
		default:
			u = append(u, pushData(real)...)
		}
		if s.method != methodClose {
			switch {
			case o.noVault:
			case o.rawVault != nil:
				u = append(u, o.rawVault...)
			case s.vaultArg != nil:
				u = append(u, pushData(s.vaultArg)...)
			default:
				u = append(u, pushData(f.vault)...)
			}
			cp, ca := make([]byte, 20), int64(0)
			if s.change != nil {
				cp, ca = s.change.script, s.change.sats
			}
			if o.chPkh != nil {
				u = append(u, o.chPkh...)
			} else {
				u = append(u, pushData(cp)...)
			}
			if o.chAmt != nil {
				u = append(u, o.chAmt...)
			} else {
				u = append(u, pushNum(ca)...)
			}
		}
		if !o.noPre {
			u = append(u, pushData(pre)...)
		}
		if o.method != nil {
			u = append(u, o.method...)
		} else {
			u = append(u, pushNum(int64(s.method))...)
		}
		for _, tr := range o.trailing {
			u = append(u, tr...)
		}
		tx.Inputs[pi].UnlockingScript = script.NewFromBytes(u)
	}
	if s.vaultIdx >= 0 {
		vin := s.ins[s.vaultIdx]
		flag := s.vaultSighash
		if flag == 0 {
			flag = sighash.AllForkID
		}
		prevouts := s.vaultPrevouts
		if prevouts == nil {
			prevouts = real
		}
		vpre := advPreimage(t, tx, s.vaultIdx, vin.sats, advScriptCode(vin.lock), flag, false)
		if s.forgeVaultPre != nil {
			vpre = s.forgeVaultPre(vpre)
		}
		u := pushData(prevouts)
		u = append(u, pushData(vpre)...)
		tx.Inputs[s.vaultIdx].UnlockingScript = script.NewFromBytes(u)
	}
	for idx := range o.p2pkh {
		if err := advExec(tx, idx, s.ins[idx], chron); err != nil {
			t.Fatalf("harness: plain P2PKH input %d rejected: %v", idx, err)
		}
	}
	if s.poolIdx >= 0 {
		r.pool = advExec(tx, s.poolIdx, s.ins[s.poolIdx], chron)
	}
	if s.vaultIdx >= 0 {
		r.vault = advExec(tx, s.vaultIdx, s.ins[s.vaultIdx], chron)
	}
	advStrandingCheck(t, s, r)
	return r, tx
}

func advRun(t testing.TB, f *fixture, s spend, o advOpts) result {
	t.Helper()
	r, _ := advRunTx(t, f, s, o)
	return r
}

// advH is fixture.run's honest path through advRun (the stranding check included).
func advH(t testing.TB, f *fixture, s spend) result {
	t.Helper()
	return advRun(t, f, s, advOpts{})
}

// advStrandingCheck: an accepted pool input is input 0, output 0 of its
// transaction, and input 1 is output 1 of that same transaction; an accepted
// vault input is input 1 with input 0 at vout 0 of its own transaction.
func advStrandingCheck(t testing.TB, s spend, r result) {
	t.Helper()
	if s.poolIdx >= 0 && r.pool == nil {
		advAccepted.Add(1)
		p := s.ins[s.poolIdx]
		if s.poolIdx != 0 || len(s.ins) < 2 || p.vout != 0 || s.ins[1].txid != p.txid || s.ins[1].vout != 1 {
			t.Errorf("STRANDING: pool input %d (%x:%d) accepted without its vault as input 1 (inputs %+v)", s.poolIdx, p.txid, p.vout, s.ins)
		}
	}
	if s.vaultIdx >= 0 && r.vault == nil {
		v := s.ins[s.vaultIdx]
		if s.vaultIdx != 1 || v.vout != 1 || s.ins[0].txid != v.txid || s.ins[0].vout != 0 {
			t.Errorf("VAULT RULE: vault input %d (%x:%d) accepted outside its rule (inputs %+v)", s.vaultIdx, v.txid, v.vout, s.ins)
		}
	}
}

func advTxHex(tx *transaction.Transaction) string { return hex.EncodeToString(tx.Bytes()) }

// --- toolchain ---

type advSlot struct {
	ParamIndex   int `json:"paramIndex"`
	ByteOffset   int `json:"byteOffset"`
	CodeSepIndex int `json:"codeSepIndex"`
}

type advArtifact struct {
	Script            string    `json:"script"`
	ASM               string    `json:"asm"`
	ConstructorSlots  []advSlot `json:"constructorSlots"`
	CodeSepIndexSlots []advSlot `json:"codeSepIndexSlots"`
	CodePartLenSlots  []advSlot `json:"codePartLenSlots"`

	head     bool // compiled by Rúnar main (go.work), not the go.mod pin
	template []byte
	sa       runar.RunarArtifact
}

var (
	advArtOnce sync.Once
	advArtVal  *advArtifact
	advArtErr  error
)

// advArt is the PairPool artifact as JSON, and which compiler made it.
// advIsHead (the name pair_strict_test.go uses) means Rúnar main.
func advArt(t testing.TB) *advArtifact {
	t.Helper()
	advArtOnce.Do(func() {
		art, err := compiler.CompileFromSource("PairPool.runar.go")
		if err != nil {
			advArtErr = err
			return
		}
		js, err := compiler.ArtifactToJSON(art)
		if err != nil {
			advArtErr = err
			return
		}
		a := &advArtifact{}
		if advArtErr = json.Unmarshal(js, a); advArtErr != nil {
			return
		}
		// Two independent signals of Rúnar main, which must agree: its
		// artifact JSON has a rawScriptSpans key, and its state writer checks
		// each 33-byte key's size as OP_SIZE <21> OP_NUMEQUALVERIFY (the
		// contract's own Len == 33 compiles to OP_SIZE OP_NIP <21>
		// OP_NUMEQUALVERIFY on both compilers, which this does not match).
		var keys map[string]json.RawMessage
		if advArtErr = json.Unmarshal(js, &keys); advArtErr != nil {
			return
		}
		_, spans := keys["rawScriptSpans"]
		writer := strings.Count(a.ASM, "OP_SIZE <21> OP_NUMEQUALVERIFY")
		if spans != (writer > 0) {
			advArtErr = fmt.Errorf("toolchain detection: rawScriptSpans key %v but %d writer size checks", spans, writer)
			return
		}
		a.head = spans
		if a.template, advArtErr = hex.DecodeString(a.Script); advArtErr != nil {
			return
		}
		advArtErr = json.Unmarshal(js, &a.sa)
		advArtVal = a
	})
	if advArtErr != nil {
		t.Fatal(advArtErr)
	}
	return advArtVal
}

func advIsHead(t testing.TB) bool { return advArt(t).head }

func advToolchain(t testing.TB) string {
	if advIsHead(t) {
		return "Rúnar main"
	}
	return "pinned Rúnar"
}

type advWant int

const (
	advPass advWant = iota // must pass
	advFail                // the pool input must fail
)

// advCheck asserts r and logs an ADV-RECORD line naming the compiler
// (`go test -v ./pool/pair/ | grep ADV-RECORD`).
func advCheck(t *testing.T, name string, r result, want advWant) {
	t.Helper()
	t.Logf("ADV-RECORD %s | %s | pool=%v vault=%v", advToolchain(t), name, advErrS(r.pool), advErrS(r.vault))
	if want == advPass {
		mustPass(t, r)
	} else {
		mustFailPool(t, r)
	}
}

func advErrS(err error) string {
	if err == nil {
		return "pass"
	}
	s := err.Error()
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return "fail (" + s + ")"
}

// --- fixtures ---

var advCfgs = [][3]int64{{30, 5, 10}, {0, 0, 0}}

var advFixMu sync.Mutex

func advFix(t testing.TB, bps [3]int64) *fixture {
	t.Helper()
	return advFixIDs(t, assetA, assetB, bps)
}

var advFixIDCache = map[string]*fixture{}

// advFixIDs is advFix for a pair of the two given asset ids.
func advFixIDs(t testing.TB, idA, idB []byte, bps [3]int64) *fixture {
	t.Helper()
	advFixMu.Lock()
	defer advFixMu.Unlock()
	k := fmt.Sprintf("%x/%x/%v", idA, idB, bps)
	if f, ok := advFixIDCache[k]; ok {
		return f
	}
	f := newFixtureIDs(t, idA, idB, bps[0], bps[1], bps[2])
	advFixIDCache[k] = f
	return f
}

var advMethodNames = []string{"Swap", "Close", "Rescind"}

func advDir(aIn bool) string {
	if aIn {
		return "A in"
	}
	return "B in"
}

// advEach runs fn per fee config and direction.
func advEach(t *testing.T, fn func(t *testing.T, f *fixture, p pair, aIn bool)) {
	for _, bps := range advCfgs {
		for _, aIn := range []bool{true, false} {
			bps, aIn := bps, aIn
			t.Run(fmt.Sprintf("bps %d-%d-%d/%s", bps[0], bps[1], bps[2], advDir(aIn)), func(t *testing.T) {
				fn(t, advFix(t, bps), fresh(), aIn)
			})
		}
	}
}

// advNext is the next validator key fixture.swap names.
func advNext(k *ec.PrivateKey) *ec.PrivateKey { return key(k.Serialize()[0] + 1) }

func advValid(f *fixture, p pair, method int, aIn bool) spend {
	switch method {
	case methodSwap:
		s, _ := f.swap(p, 40_000, aIn)
		return s
	case methodClose:
		return f.close(p)
	default:
		s, _ := f.rescind(p)
		return s
	}
}

func advWithSigner(s spend, k *ec.PrivateKey) spend {
	s.signer = k
	return s
}

func advCopy(s spend) spend {
	s.outs = clone(s.outs)
	s.ins = append([]in{}, s.ins...)
	s.args = append([][]byte{}, s.args...)
	return s
}

// State offsets (pair.state, 251 bytes, every field fixed-width): ReserveA 0,
// ReserveB 8, LpPubKey 16, ValidatorPubKey 49, ValidatorIdentity 82, Nonce
// 115, LpFeeAPkh 147, LpFeeBPkh 167, AffirmationR 187, AffirmationS 219.
const (
	advOffA     = 0
	advOffB     = 8
	advOffLp    = 16
	advOffVal   = 49
	advOffId    = 82
	advOffNonce = 115
	advOffFeeA  = 147
	advOffFeeB  = 167
	advOffAffR  = 187
	advOffAffS  = 219
	advStateLen = 251
)

// advLockState is a pool output script with the given raw state.
func (f *fixture) advLockState(a int64, state []byte) []byte {
	s := append(append([]byte{}, tokenPrefix(f.idA, a)...), f.code...)
	s = append(s, 0x6a)
	return append(s, state...)
}

// advStateVal is p's state with the ValidatorPubKey field replaced by raw bytes.
func advStateVal(p pair, val []byte) []byte {
	s := p.state()
	return append(append(append([]byte{}, s[:advOffVal]...), val...), s[advOffVal+33:]...)
}

// advSwap describes a Swap fully; the zero values take fixture.swap's defaults.
type advSwap struct {
	amountIn int64
	aIn      bool
	next     *ec.PrivateKey // nil: advNext(p.validator)
	nextRaw  []byte         // raw nextValidatorPubKey bytes (overrides next in the state and the arg)
	user     []byte
	com      []byte
	tcPkh    []byte
	tc       int64
	change   *out // Rúnar's change: script = the pkh
}

func advBuildSwap(f *fixture, p pair, a advSwap) (spend, swapResult) {
	next := a.next
	if next == nil {
		next = advNext(p.validator)
	}
	user, com, tcPkh := userPkh, commissionPkh, changePkh
	if a.user != nil {
		user = a.user
	}
	if a.com != nil {
		com = a.com
	}
	if a.tcPkh != nil {
		tcPkh = a.tcPkh
	}
	r := f.model(p, a.amountIn, a.aIn, next)
	inAsset, outAsset, lpPkh := f.idA, f.idB, lpFeeAPkh
	if !a.aIn {
		inAsset, outAsset, lpPkh = f.idB, f.idA, lpFeeBPkh
	}
	nextBytes := next.PubKey().Compressed()
	poolOut := f.poolLock(r.after)
	if a.nextRaw != nil {
		nextBytes = a.nextRaw
		poolOut = f.advLockState(r.after.a, advStateVal(r.after, a.nextRaw))
	}
	outs := []out{{p.sats, poolOut}, {1, f.vaultLock(r.after)}, {1, tokenP2pkh(outAsset, r.out, user)}}
	if r.lpFee > 0 {
		outs = append(outs, out{1, tokenP2pkh(inAsset, r.lpFee, lpPkh)})
	}
	if r.valFee > 0 {
		outs = append(outs, out{1, tokenP2pkh(inAsset, r.valFee, pkh(p.validator))})
	}
	if r.com > 0 {
		outs = append(outs, out{1, tokenP2pkh(inAsset, r.com, com)})
	}
	if a.tc > 0 {
		outs = append(outs, out{1, tokenP2pkh(inAsset, a.tc, tcPkh)})
	}
	var change *out
	if a.change != nil {
		change = &out{a.change.sats, a.change.script}
		if a.change.sats > 0 {
			outs = append(outs, out{a.change.sats, p2pkh(a.change.script)})
		}
	}
	return spend{
		ins: f.ins(p), outs: outs, poolIdx: 0, vaultIdx: 1,
		method: methodSwap, signer: p.validator, change: change,
		args: [][]byte{
			pushData(nextBytes),
			pushNum(a.amountIn),
			pushBool(a.aIn),
			pushData(user),
			pushData(com),
			pushData(tcPkh),
			pushNum(a.tc),
		},
	}, r
}

// advKeyWithByte is a deterministic private key whose compressed public key
// has byte v at index i.
func advKeyWithByte(t testing.TB, i int, v byte) *ec.PrivateKey {
	t.Helper()
	for n := 0; n < 1<<16; n++ {
		seed := sha256.Sum256([]byte(fmt.Sprintf("adv-key-%d", n)))
		k, _ := ec.PrivateKeyFromBytes(seed[:])
		if k.PubKey().Compressed()[i] == v {
			return k
		}
	}
	t.Fatalf("no key with %#x at %d", v, i)
	return nil
}

var advFlags = []struct {
	name string
	flag sighash.Flag
}{
	{"NONE|FORKID", sighash.NoneForkID},
	{"SINGLE|FORKID", sighash.SingleForkID},
	{"ALL|ANYONECANPAY|FORKID", sighash.AllForkID | sighash.AnyOneCanPay},
	{"NONE|ANYONECANPAY|FORKID", sighash.NoneForkID | sighash.AnyOneCanPay},
	{"SINGLE|ANYONECANPAY|FORKID", sighash.SingleForkID | sighash.AnyOneCanPay},
	{"ALL without FORKID", sighash.All},
}

// The harness's own swap and advBuildSwap agree.
func TestAdvHarnessAgreement(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	for _, aIn := range []bool{true, false} {
		a, _ := f.swap(fresh(), 40_000, aIn)
		b, _ := advBuildSwap(f, fresh(), advSwap{amountIn: 40_000, aIn: aIn})
		if fmt.Sprint(a.outs) != fmt.Sprint(b.outs) || fmt.Sprint(a.args) != fmt.Sprint(b.args) {
			t.Fatalf("%s: advBuildSwap differs from fixture.swap", advDir(aIn))
		}
		mustPass(t, advH(t, f, b))
		mustPass(t, f.run(t, a))
	}
}

// --- who can spend ---

func TestAdv_WhoCanSpend(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pair, aIn bool) {
		_, r := f.swap(p, 40_000, aIn)
		rotated := r.after // validator advNext(p.validator)
		random := key(0x31)
		for method := 0; method < 3; method++ {
			method := method
			mn := advMethodNames[method]
			for _, pp := range []struct {
				name string
				p    pair
				prev *ec.PrivateKey // the key before the last rotation
			}{{"fresh", p, nil}, {"after one rotation", rotated, p.validator}} {
				pp := pp
				base := advValid(f, pp.p, method, aIn)
				right := pp.p.validator
				if method == methodClose {
					right = pp.p.lp
				}
				t.Run(mn+"/"+pp.name+": the right signer passes", func(t *testing.T) { mustPass(t, advH(t, f, base)) })
				wrong := map[string]*ec.PrivateKey{
					"the identity key":       validatorIdentity,
					"a random key":           random,
					"the next validator key": advNext(pp.p.validator),
				}
				if method == methodClose {
					wrong["the validator"] = pp.p.validator
				} else {
					wrong["the LP"] = pp.p.lp
				}
				if pp.prev != nil {
					wrong["the previous validator key"] = pp.prev
				}
				for name, k := range wrong {
					k := k
					t.Run(mn+"/"+pp.name+": signed by "+name, func(t *testing.T) {
						mustFailPool(t, advH(t, f, advWithSigner(base, k)))
					})
				}
				t.Run(mn+"/"+pp.name+": empty signature", func(t *testing.T) {
					mustFailPool(t, advRun(t, f, base, advOpts{rawSig: []byte{0x00}}))
				})
				t.Run(mn+"/"+pp.name+": a public key as the signature", func(t *testing.T) {
					mustFailPool(t, advRun(t, f, base, advOpts{rawSig: pushData(right.PubKey().Compressed())}))
				})
				t.Run(mn+"/"+pp.name+": the right signer's signature over a different transaction", func(t *testing.T) {
					other := advCopy(base)
					other.outs = append(other.outs, out{1, p2pkh(pkh(random))})
					mustFailPool(t, advRun(t, f, base, advOpts{sigSpend: &other}))
					// and the other way round: the honest outputs signed, a thief's outputs sent
					steal := advCopy(base)
					steal.outs[len(steal.outs)-1].script = p2pkh(pkh(random))
					mustFailPool(t, advRun(t, f, steal, advOpts{sigSpend: &base}))
				})
				t.Run(mn+"/"+pp.name+": the right signer's signature over a different pool outpoint", func(t *testing.T) {
					other := advCopy(base)
					other.ins[0].txid, other.ins[1].txid = 0x12, 0x12
					mustFailPool(t, advRun(t, f, base, advOpts{sigSpend: &other}))
				})
			}
		}
	})
}

// --- sighash types ---

// The pool's txPreimage under any type but ALL|FORKID: every method rejects.
func TestAdv_PreimageSighashType(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pair, aIn bool) {
		for method := 0; method < 3; method++ {
			for _, fl := range advFlags {
				method, fl := method, fl
				t.Run(advMethodNames[method]+": preimage "+fl.name, func(t *testing.T) {
					s := advValid(f, p, method, aIn)
					mustFailPool(t, advRun(t, f, s, advOpts{preFlag: fl.flag}))
					if fl.flag&sighash.ForkID == 0 {
						mustFailPool(t, advRun(t, f, s, advOpts{preFlag: fl.flag, preBIP143: true}))
					}
				})
			}
		}
	})
}

// The signer's own signature must be ALL|FORKID (requireSighashAll). Every
// signature here is a valid signature of its type, made over the digest the
// interpreter computes for that type (the legacy digest without FORKID).
// Swap and Rescind run CHECKSIG first: a wrong signer fails there, the right
// signer gets past it and fails the type check, a different error. Close runs
// the type check first. The same transaction signed ALL|FORKID passes, so the
// signature's type is the only difference.
func TestAdv_SignatureSighashType(t *testing.T) {
	flags := append(append([]struct {
		name string
		flag sighash.Flag
	}{}, advFlags...), struct {
		name string
		flag sighash.Flag
	}{"ALL|FORKID with an undefined bit (0x61)", 0x61})
	advEach(t, func(t *testing.T, f *fixture, p pair, aIn bool) {
		for method := 0; method < 3; method++ {
			method := method
			mn := advMethodNames[method]
			base := advValid(f, p, method, aIn)
			wrong := p.lp
			if method == methodClose {
				wrong = p.validator
			}
			t.Run(mn+": ALL|FORKID (control)", func(t *testing.T) { mustPass(t, advH(t, f, base)) })
			for _, fl := range flags {
				fl := fl
				t.Run(mn+": signature "+fl.name, func(t *testing.T) {
					r := advRun(t, f, base, advOpts{sigFlag: fl.flag})
					w := advRun(t, f, advWithSigner(base, wrong), advOpts{sigFlag: fl.flag})
					t.Logf("right signer: pool %v; wrong signer: pool %v", advErrS(r.pool), advErrS(w.pool))
					mustFailPool(t, r)
					mustFailPool(t, w)
					if method != methodClose && fl.flag != 0x61 && r.pool.Error() == w.pool.Error() {
						t.Fatalf("the right signer's %s signature failed where a wrong signer's does (%v): not the type check", fl.name, r.pool)
					}
					if fl.flag&sighash.ForkID == 0 {
						mustFailPool(t, advRun(t, f, base, advOpts{sigFlag: fl.flag, noChronicle: true}))
					}
				})
			}
		}
	})
}

// Attempts around the type check: a last byte of 0x41 on a signature made
// over another type's digest (CHECKSIG hashes by the last byte, so it is the
// ALL|FORKID digest that must verify), an empty signature, a 1-byte "41",
// bytes after the type byte. Every one is refused; the reason is logged.
func TestAdv_SignatureSighashTypeBypass(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	set41 := func(b []byte) []byte { b[len(b)-1] = 0x41; return b }
	for method := 0; method < 3; method++ {
		method := method
		mn := advMethodNames[method]
		base := advValid(f, p, method, true)
		cases := []struct {
			name string
			o    advOpts
		}{
			{"NONE|FORKID digest, last byte rewritten to 41", advOpts{sigFlag: sighash.NoneForkID, sigTweak: set41}},
			{"SINGLE|FORKID digest, last byte rewritten to 41", advOpts{sigFlag: sighash.SingleForkID, sigTweak: set41}},
			{"ALL|ANYONECANPAY|FORKID digest, last byte rewritten to 41", advOpts{sigFlag: 0xc1, sigTweak: set41}},
			{"legacy ALL digest, last byte rewritten to 41", advOpts{sigFlag: sighash.All, sigTweak: set41}},
			{"empty signature", advOpts{rawSig: []byte{0x00}}},
			{"the single byte 41", advOpts{rawSig: []byte{0x01, 0x41}}},
			{"the bytes 41 41", advOpts{rawSig: []byte{0x02, 0x41, 0x41}}},
			{"a valid ALL|FORKID signature with 41 appended", advOpts{sigTweak: func(b []byte) []byte { return append(b, 0x41) }}},
			{"a valid NONE|FORKID signature with 41 appended", advOpts{sigFlag: sighash.NoneForkID, sigTweak: func(b []byte) []byte { return append(b, 0x41) }}},
			{"a valid ALL|FORKID signature with 00 inserted before its type byte", advOpts{sigTweak: func(b []byte) []byte {
				return append(append(append([]byte{}, b[:len(b)-1]...), 0x00), 0x41)
			}}},
		}
		for _, c := range cases {
			c := c
			t.Run(mn+": "+c.name, func(t *testing.T) {
				r := advRun(t, f, base, c.o)
				t.Logf("pool %v", advErrS(r.pool))
				mustFailPool(t, r)
			})
		}
	}
}

// What the type check prevents: a signature that leaves outputs or inputs
// out, reused by a third party over a transaction of its own. Each rebuild
// below was accepted before requireSighashAll; now the signer's partial
// signature is refused outright, the untouched transaction included, and
// the same rebuild under the signer's ALL|FORKID signature fails CHECKSIG.
func TestAdv_PartialSighashSignatureCannotBeReused(t *testing.T) {
	thief := pkh(key(0x66))
	thiefKey := key(0x67)
	for _, bps := range advCfgs {
		f := advFix(t, bps)
		p := fresh()
		addIn := func(s spend) spend {
			s = advCopy(s)
			s.ins = append(s.ins, in{txid: 0x2a, vout: 0, sats: 5_000})
			return s
		}
		swapFunding := func(s spend) spend {
			s = advCopy(s)
			s.ins[2] = in{txid: 0x2b, vout: 3, sats: 7_000}
			return s
		}
		withOuts := func(s spend, o []out) spend {
			s = advCopy(s)
			s.outs = o
			return s
		}
		type rebuild struct {
			name   string
			flag   sighash.Flag
			signed spend // what the signer signed
			s      spend // what the third party sends
		}
		var cases []rebuild

		cl := f.close(p)
		steal := []out{{1, tokenP2pkh(assetA, p.a, thief)}, {1, tokenP2pkh(assetB, p.b, thief)}}
		cases = append(cases,
			rebuild{"Close, LP NONE: all outputs to the thief", 0x42, cl, withOuts(cl, steal)},
			rebuild{"Close, LP SINGLE: outputs other than 0 to the thief", 0x43, cl, withOuts(cl, []out{cl.outs[0], steal[1]})},
			rebuild{"Close, LP ALL|ANYONECANPAY: an input added", 0xc1, cl, addIn(cl)},
			rebuild{"Close, LP NONE|ANYONECANPAY: outputs to the thief, funding replaced", 0xc2, cl, swapFunding(withOuts(cl, steal))},
			rebuild{"Close, LP SINGLE|ANYONECANPAY: output 1 to the thief, an input added", 0xc3, cl, addIn(withOuts(cl, []out{cl.outs[0], steal[1]}))},
		)
		for _, aIn := range []bool{true, false} {
			base := advSwap{amountIn: 40_000, aIn: aIn, tc: 777, change: &out{4_321, changePkh}}
			signed, _ := advBuildSwap(f, p, base)
			mod := func(edit func(a *advSwap)) spend {
				a := base
				ch := *base.change
				a.change = &ch
				edit(&a)
				s, _ := advBuildSwap(f, p, a)
				return s
			}
			d := advDir(aIn)
			cases = append(cases,
				rebuild{"Swap " + d + ", validator NONE: payout to the thief", 0x42, signed, mod(func(a *advSwap) { a.user = thief })},
				rebuild{"Swap " + d + ", validator NONE: the thief as next validator", 0x42, signed, mod(func(a *advSwap) { a.next = thiefKey })},
				rebuild{"Swap " + d + ", validator NONE: amountIn doubled", 0x42, signed, mod(func(a *advSwap) { a.amountIn = 80_000 })},
				rebuild{"Swap " + d + ", validator NONE: direction flipped", 0x42, signed, mod(func(a *advSwap) { a.aIn = !aIn })},
				rebuild{"Swap " + d + ", validator SINGLE: payout to the thief", 0x43, signed, mod(func(a *advSwap) { a.user = thief })},
				rebuild{"Swap " + d + ", validator SINGLE: token change to the thief", 0x43, signed, mod(func(a *advSwap) { a.tcPkh = thief })},
				rebuild{"Swap " + d + ", validator SINGLE: sats change to the thief", 0x43, signed, mod(func(a *advSwap) { a.change.script = thief })},
				rebuild{"Swap " + d + ", validator ALL|ANYONECANPAY: funding replaced, an input added", 0xc1, signed, addIn(swapFunding(signed))},
			)
		}
		rs, _ := f.rescind(p)
		rs.outs = append(rs.outs, out{900, p2pkh(changePkh)})
		rs.change = &out{900, changePkh}
		red := advCopy(rs)
		red.outs[2].script = p2pkh(thief)
		red.change = &out{900, thief}
		cases = append(cases, rebuild{"Rescind, validator NONE: sats change to the thief", 0x42, rs, red})

		for _, c := range cases {
			c := c
			t.Run(fmt.Sprintf("bps %v: %s", bps, c.name), func(t *testing.T) {
				mustPass(t, advH(t, f, c.signed))
				orig := advRun(t, f, c.signed, advOpts{sigFlag: c.flag})
				reb := advRun(t, f, c.s, advOpts{sigFlag: c.flag, sigSpend: &c.signed})
				all := advRun(t, f, c.s, advOpts{sigSpend: &c.signed})
				t.Logf("signed tx under %#x: %v; rebuilt: %v; rebuilt under the ALL|FORKID signature: %v", byte(c.flag), advErrS(orig.pool), advErrS(reb.pool), advErrS(all.pool))
				mustFailPool(t, orig)
				mustFailPool(t, reb)
				mustFailPool(t, all)
			})
		}
	}
}

// --- pairing ---

func TestAdv_Pairing(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pair, aIn bool) {
		other := fresh()
		other.lp = key(11)
		for method := 0; method < 3; method++ {
			method := method
			mn := advMethodNames[method]
			base := advValid(f, p, method, aIn)
			pool, vault, fund := base.ins[0], base.ins[1], base.ins[2]
			fund2 := in{txid: 0x23, vout: 0, sats: 50_000}
			type layout struct {
				name      string
				ins       []in
				pi, vi    int
				wantVault bool // the vault input, if run, must fail too
			}
			layouts := []layout{
				{"vault missing (funding at 1)", []in{pool, fund}, 0, -1, false},
				{"the pool alone", []in{pool}, 0, -1, false},
				{"a foreign vault at 1", []in{pool, {txid: 0x33, vout: 1, sats: 1, lock: f.vaultLock(other)}, fund}, 0, 1, true},
				{"a vault script at vout 2 of the pool's transaction", []in{pool, {txid: 0x11, vout: 2, sats: 1, lock: vault.lock}, fund}, 0, 1, true},
				{"the pool at input 1", []in{fund, pool, vault}, 1, 2, true},
				{"the pool at input 2", []in{fund, fund2, pool, vault}, 2, 3, true},
				{"the pool at vout 3 (its vault at vout 1)", []in{{txid: 0x11, vout: 3, sats: pool.sats, lock: pool.lock}, vault, fund}, 0, 1, true},
				{"the pool at vout 1, the vault at vout 2", []in{{txid: 0x11, vout: 1, sats: pool.sats, lock: pool.lock}, {txid: 0x11, vout: 2, sats: 1, lock: vault.lock}, fund}, 0, 1, true},
				{"an input before the pool", []in{fund2, pool, vault, fund}, 1, 2, true},
				{"an input between the pool and its vault", []in{pool, fund2, vault, fund}, 0, 2, true},
			}
			for _, l := range layouts {
				l := l
				t.Run(mn+": "+l.name, func(t *testing.T) {
					s := advCopy(base)
					s.ins, s.poolIdx, s.vaultIdx = l.ins, l.pi, l.vi
					r := advH(t, f, s)
					mustFailPool(t, r)
					if l.vi >= 0 && l.wantVault {
						mustFailVault(t, r)
					}
				})
			}
			t.Run(mn+": inputs after the vault (control)", func(t *testing.T) {
				s := advCopy(base)
				s.ins = []in{pool, vault, fund, fund2, {txid: 0x24, vout: 7, sats: 3}}
				mustPass(t, advH(t, f, s))
			})

			real := append(append(outpoint(pool), outpoint(vault)...), outpoint(fund)...)
			wrongTail := append(append(outpoint(pool), outpoint(vault)...), outpoint(fund2)...)
			flipped := append([]byte{}, real...)
			flipped[80] ^= 1
			prevs := []struct {
				name string
				b    []byte
			}{
				{"36 bytes (the pool's outpoint)", real[:36]},
				{"71 bytes", real[:71]},
				{"72 bytes (pool and vault, no funding)", real[:72]},
				{"trailing bytes", append(append([]byte{}, real...), 0x00)},
				{"the right first 72 bytes, a wrong tail", wrongTail},
				{"one byte flipped in the tail", flipped},
				{"empty", []byte{}},
			}
			for _, pv := range prevs {
				pv := pv
				t.Run(mn+": pool allPrevouts "+pv.name, func(t *testing.T) {
					s := advCopy(base)
					s.prevouts = pv.b
					mustFailPool(t, advH(t, f, s))
				})
				t.Run(mn+": vault allPrevouts "+pv.name, func(t *testing.T) {
					s := advCopy(base)
					s.vaultPrevouts = pv.b
					mustFailVault(t, advH(t, f, s))
				})
			}
			t.Run(mn+": a one-input transaction told its true 36-byte prevouts", func(t *testing.T) {
				// hashPrevouts matches; Substr(allPrevouts, 36, 36) runs off the end.
				s := advCopy(base)
				s.ins, s.vaultIdx = []in{pool}, -1
				s.prevouts = outpoint(pool)
				mustFailPool(t, advH(t, f, s))
			})
		}
	})
}

// The vault moves only with its pool: every way to spend it without one.
func TestAdv_VaultWithoutItsPool(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	base := f.close(p)
	pool, vault, fund := base.ins[0], base.ins[1], base.ins[2]
	steal := []out{{1, tokenP2pkh(assetB, p.b, userPkh)}}
	foreignPool := in{txid: 0x33, vout: 0, sats: 1, lock: f.poolLock(p)}
	cases := []struct {
		name string
		ins  []in
		vi   int
	}{
		{"alone", []in{vault}, 0},
		{"at input 0, funding at 1", []in{vault, fund}, 0},
		{"at input 0, its pool at 1", []in{vault, pool}, 0},
		{"at input 1 after funding", []in{fund, vault}, 1},
		{"at input 1 after another transaction's vout 0", []in{{txid: 0x33, vout: 0, sats: 1}, vault}, 1},
		{"at input 1 after another pool (vout 0 of another tx)", []in{foreignPool, vault}, 1},
		{"at input 1 after its own transaction's vout 2", []in{{txid: 0x11, vout: 2, sats: 1}, vault}, 1},
		{"at input 2 after its pool and funding", []in{pool, fund, vault}, 2},
		{"at input 2 after funding and its pool", []in{fund, pool, vault}, 2},
		{"a vault script at vout 0 spent at input 1 after vout 0", []in{{txid: 0x11, vout: 0, sats: 1}, {txid: 0x11, vout: 0, sats: 1, lock: vault.lock}}, 1},
		{"a vault script at vout 2, input 0 is vout 0", []in{{txid: 0x11, vout: 0, sats: 1}, {txid: 0x11, vout: 2, sats: 1, lock: vault.lock}}, 1},
		{"a vault script at vout 0, at input 0", []in{{txid: 0x11, vout: 0, sats: 1, lock: vault.lock}, fund}, 0},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			mustFailVault(t, advH(t, f, spend{ins: c.ins, outs: steal, poolIdx: -1, vaultIdx: c.vi}))
		})
	}
	t.Run("without its pool, a forged preimage naming its pool's outpoint in hashPrevouts", func(t *testing.T) {
		// hashPrevouts replaced by hash256 of the prevouts an honest spend
		// would show; the outpoint field already the vault's own. Only
		// OP_PUSH_TX (CheckPreimage) ties the preimage to this transaction.
		fake := append(outpoint(pool), outpoint(vault)...)
		s := spend{ins: []in{fund, vault}, outs: steal, poolIdx: -1, vaultIdx: 1, vaultPrevouts: fake}
		s.forgeVaultPre = func(real []byte) []byte {
			c := append([]byte{}, real...)
			copy(c[4:36], advHash(fake))
			return c
		}
		mustFailVault(t, advH(t, f, s))
	})
	t.Run("told the honest prevouts in a transaction without the pool", func(t *testing.T) {
		s := spend{ins: []in{fund, vault}, outs: steal, poolIdx: -1, vaultIdx: 1}
		s.vaultPrevouts = append(outpoint(pool), outpoint(vault)...)
		mustFailVault(t, advH(t, f, s))
	})
	for _, fl := range advFlags {
		fl := fl
		t.Run("with its pool, vault preimage "+fl.name, func(t *testing.T) {
			s := base
			s.vaultSighash = fl.flag
			mustFailVault(t, advH(t, f, s))
		})
		t.Run("without its pool, vault preimage "+fl.name+", prevouts empty and real", func(t *testing.T) {
			s := spend{ins: []in{fund, vault}, outs: steal, poolIdx: -1, vaultIdx: 1, vaultSighash: fl.flag}
			mustFailVault(t, advH(t, f, s))
			s.vaultPrevouts = []byte{}
			mustFailVault(t, advH(t, f, s))
		})
	}
}

// TRUST MODEL. The vault checks only outpoints: input 0 is vout 0 of the
// vault's own transaction and the vault is input 1 at vout 1. It never sees
// input 0's script. A vault output created at vout 1 of a transaction whose
// vout 0 is NOT a pool (a P2PKH, or a pool of other code) is released by
// whoever can spend that vout 0. Off-chain check carrying the weight: the
// lookup accepts a pool only after checking the pair at deploy (vout 0 the
// canonical pool code with the canonical VaultCodeHash, vout 1 the canonical
// vault code, same transaction).
func TestTrustModel_VaultChecksOnlyTheOutpoint(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	thief := key(0x5a)
	vault := in{txid: 0x11, vout: 1, sats: 1, lock: f.vaultLock(p)}
	steal := []out{{1, tokenP2pkh(assetB, p.b, pkh(thief))}}

	t.Run("vout 0 is the thief's P2PKH: the vault is released, outputs free", func(t *testing.T) {
		s := spend{ins: []in{{txid: 0x11, vout: 0, sats: 1_000, lock: p2pkh(pkh(thief))}, vault}, outs: steal, poolIdx: -1, vaultIdx: 1}
		mustPass(t, advRun(t, f, s, advOpts{p2pkh: map[int]*ec.PrivateKey{0: thief}}))
	})
	// The thief's own pair of other readonly args (a legitimate {0,0,0}
	// pair, the canonical vault beside it): the vault releases with whatever
	// pool code sits at vout 0, so the lookup must check the pool's code too.
	t.Run("vout 0 is a pool of other code whose LP is the thief: its Close releases the vault", func(t *testing.T) {
		g := advFix(t, [3]int64{0, 0, 0}) // other readonly args: another pool's code
		q := fresh()
		q.lp = thief
		s := g.close(q)
		s.ins[1] = vault
		s.outs = append(s.outs, steal...)
		mustPass(t, advH(t, g, s))
	})
	t.Run("control: with the canonical pool at vout 0 only the LP's Close releases it", func(t *testing.T) {
		s := f.close(p)
		mustPass(t, advH(t, f, s))
		mustFailPool(t, advH(t, f, advWithSigner(s, thief)))
	})
}

// --- the vault code witness ---

// Swap and Rescind take the vault's script as the argument vaultCode and
// build output 1 from it, after checking Sha256(vaultCode) == VaultCodeHash.
// Every other script is refused, whether output 1 is built from it or is the
// real vault. A different script with the vault's hash cannot be tried: that
// would be a SHA-256 second preimage; the nearest attempt, the hash itself as
// the argument, is refused.
func TestAdv_VaultCodeWitness(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pair, aIn bool) {
		sw, r := f.swap(p, 40_000, aIn)
		rs, after := f.rescind(p)
		tail := tokenP2pkh(assetB, 1, pkh(p.validator))[len(tokenPrefix(assetB, 1)):]
		codes := []struct {
			name string
			code []byte
		}{
			{"a P2PKH to the validator", tail},
			{"empty", []byte{}},
			{"OP_TRUE", []byte{0x51}},
			{"the vault's code without its last byte (a prefix)", f.vault[:len(f.vault)-1]},
			{"the vault's code without its first byte", f.vault[1:]},
			{"the vault's code with 61 appended (an extension)", append(append([]byte{}, f.vault...), 0x61)},
			{"the vault's code with a P2PKH appended", append(append([]byte{}, f.vault...), tail...)},
			{"the vault's code with its middle byte flipped", advFlip(f.vault, len(f.vault)/2)},
			{"the vault's code twice", append(append([]byte{}, f.vault...), f.vault...)},
			{"the pool's code", f.code},
			{"VaultCodeHash itself (claiming the hash)", f.advVaultHash()},
		}
		for _, m := range []struct {
			name  string
			s     spend
			after pair
		}{{"Swap", sw, r.after}, {"Rescind", rs, after}} {
			m := m
			t.Run(m.name+": the vault's code (control)", func(t *testing.T) { mustPass(t, advH(t, f, m.s)) })
			for _, c := range codes {
				c := c
				t.Run(m.name+": vaultCode "+c.name+", output 1 built from it", func(t *testing.T) {
					s := advCopy(m.s)
					s.vaultArg = c.code
					s.outs[1].script = append(tokenPrefix(assetB, m.after.b), c.code...)
					mustFailPool(t, advH(t, f, s))
				})
				t.Run(m.name+": vaultCode "+c.name+", output 1 the real vault", func(t *testing.T) {
					s := advCopy(m.s)
					s.vaultArg = c.code
					mustFailPool(t, advH(t, f, s))
				})
			}
			t.Run(m.name+": vaultCode omitted", func(t *testing.T) {
				mustFailPool(t, advRun(t, f, m.s, advOpts{noVault: true}))
			})
			t.Run(m.name+": the vault's code pushed twice", func(t *testing.T) {
				mustFailPool(t, advRun(t, f, m.s, advOpts{rawVault: append(pushData(f.vault), pushData(f.vault)...)}))
			})
		}
		t.Run("Close: a vaultCode pushed after allPrevouts (Close takes none)", func(t *testing.T) {
			s := f.close(p)
			real := append(append(outpoint(s.ins[0]), outpoint(s.ins[1])...), outpoint(s.ins[2])...)
			mustFailPool(t, advRun(t, f, s, advOpts{rawPrev: append(pushData(real), pushData(f.vault)...)}))
		})
	})
}

// TRUST MODEL. The script accepts whatever VaultCodeHash the pool was
// deployed with: a pool whose hash is that of OP_TRUE writes its token B
// reserve, at the first Swap or Rescind, to an output anyone can spend. The
// canonical vault's code is then the one refused. Off-chain check carrying
// the weight: the lookup admits a pool only if VaultCodeHash is sha256 of the
// canonical Vault script.
func TestTrustModel_VaultCodeHashIsWhateverWasDeployed(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	opTrue := []byte{0x51}
	g := *f
	g.code = advCodeFor(t, assetA, assetB, advSha(opTrue), f.bps)
	p := fresh()
	sw, r := g.swap(p, 40_000, true)
	sw.vaultArg = opTrue
	sw.outs[1].script = append(tokenPrefix(assetB, r.after.b), opTrue...)
	rs, after := g.rescind(p)
	rs.vaultArg = opTrue
	rs.outs[1].script = append(tokenPrefix(assetB, after.b), opTrue...)
	for name, s := range map[string]spend{"Swap": sw, "Rescind": rs} {
		s := s
		t.Run(name+": token B written to an OP_TRUE output", func(t *testing.T) {
			mustPass(t, advH(t, &g, s))
			canon := advCopy(s)
			canon.vaultArg = nil
			mustFailPool(t, advH(t, &g, canon))
			// the output it wrote: spendable with an empty unlocking script
			free := spend{ins: []in{{txid: 0x44, vout: 1, sats: 1, lock: s.outs[1].script}}, outs: []out{{1, tokenP2pkh(assetB, 1, userPkh)}}, poolIdx: -1, vaultIdx: -1}
			tx := advTx(free, advOpts{})
			tx.Inputs[0].UnlockingScript = script.NewFromBytes([]byte{})
			if err := advExec(tx, 0, free.ins[0], true); err != nil {
				t.Fatalf("the OP_TRUE vault output is not freely spendable: %v", err)
			}
		})
	}
}

// --- addresses, token change, deploy checks ---

// Every address argument must be 20 bytes, checked whether or not an output
// uses it. Lengths 0 to 40, each with the outputs the contract would write
// for it and with the honest outputs; only 20 passes.
func TestAdv_AddressLengths(t *testing.T) {
	for _, bps := range advCfgs {
		f := advFix(t, bps)
		p := fresh()
		for _, aIn := range []bool{true, false} {
			if bps[0] == 0 && !aIn {
				continue // the no-fee pair in one direction is enough
			}
			inAsset, outAsset := f.idA, f.idB
			if !aIn {
				inAsset, outAsset = f.idB, f.idA
			}
			base, r := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: aIn})
			withTc, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: aIn, tc: 777})
			comIdx := -1
			if r.com > 0 {
				comIdx = len(base.outs) - 1
			}
			name := fmt.Sprintf("bps %v %s", bps, advDir(aIn))
			accepted := map[string][]int{}
			for n := 0; n <= 40; n++ {
				h := bytes.Repeat([]byte{0x9c}, n)
				type v struct {
					name string
					s    spend
				}
				var vs []v
				c := advCopy(base)
				c.args[3] = pushData(h)
				c.outs[2].script = append(tokenPrefix(outAsset, r.out), p2pkh(h)...)
				vs = append(vs, v{"userPkh, its output built from it", c})
				c = advCopy(base)
				c.args[4] = pushData(h)
				if comIdx >= 0 {
					c.outs[comIdx].script = append(tokenPrefix(inAsset, r.com), p2pkh(h)...)
					vs = append(vs, v{"commissionPkh, its output built from it", c})
				} else {
					vs = append(vs, v{"commissionPkh, no commission output", c})
				}
				c = advCopy(withTc)
				c.args[5] = pushData(h)
				c.outs[len(c.outs)-1].script = append(tokenPrefix(inAsset, 777), p2pkh(h)...)
				vs = append(vs, v{"tokenChangePkh, its output built from it", c})
				c = advCopy(base)
				c.args[5] = pushData(h)
				vs = append(vs, v{"tokenChangePkh, no token change", c})
				if n != 20 {
					for _, i := range []int{3, 4, 5} {
						c = advCopy(base)
						c.args[i] = pushData(h)
						vs = append(vs, v{fmt.Sprintf("argument %d, the honest outputs", i), c})
					}
				}
				for _, x := range vs {
					x.s.vaultIdx = -1
					if advRun(t, f, x.s, advOpts{}).pool == nil {
						accepted[x.name] = append(accepted[x.name], n)
					}
				}
			}
			for k, ns := range accepted {
				if len(ns) != 1 || ns[0] != 20 {
					t.Errorf("%s: %s accepted at lengths %v, want only 20", name, k, ns)
				}
			}
			for _, k := range []string{"userPkh, its output built from it", "tokenChangePkh, its output built from it", "tokenChangePkh, no token change"} {
				at20 := false
				for _, n := range accepted[k] {
					at20 = at20 || n == 20
				}
				if !at20 {
					t.Errorf("%s: %s not accepted at 20 bytes", name, k)
				}
			}
			t.Logf("%s: accepted lengths %v", name, accepted)
			for i, an := range []string{"userPkh", "commissionPkh", "tokenChangePkh"} {
				c := advCopy(base)
				c.args[3+i] = append([]byte{0x4c, 20}, [][]byte{userPkh, commissionPkh, changePkh}[i]...)
				r, tx := advRunTx(t, f, c, advOpts{})
				_, btx := advRunTx(t, f, base, advOpts{})
				mustPass(t, r)
				if len(tx.Outputs) != len(btx.Outputs) {
					t.Fatalf("%s as PUSHDATA1 changed the output count", an)
				}
				for j := range tx.Outputs {
					if !bytes.Equal(tx.Outputs[j].Bytes(), btx.Outputs[j].Bytes()) {
						t.Fatalf("%s as PUSHDATA1 moved output %d", an, j)
					}
				}
			}
		}
	}
}

// advNumPad encodes v as a script number of exactly n bytes (n >= its
// minimal length), the sign on the last byte; nil if it does not fit.
func advNumPad(v *big.Int, n int) []byte {
	m := propScriptNum(new(big.Int).Abs(v))
	if len(m) > n {
		return nil
	}
	b := append(append([]byte{}, m...), make([]byte, n-len(m))...)
	if v.Sign() < 0 {
		b[n-1] |= 0x80
	}
	return b
}

// tokenChange: 0 <= tokenChange <= 2^64-1 (BRC-162's largest amount),
// whatever its encoding, and the
// change output always carries the minimal encoding of its value (the
// contract writes Bin2Num(Num2Bin(tokenChange, 9))). Every value in minimal
// and padded (9, 10, 16 bytes) encodings, with the output the value's
// minimal push and, for a non-minimal argument, the output the bytes as given.
func TestAdv_TokenChangeEncoding(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	pow := func(e uint) *big.Int { return new(big.Int).Lsh(big.NewInt(1), e) }
	values := []struct {
		name string
		v    *big.Int
	}{
		{"0", big.NewInt(0)}, {"5", big.NewInt(5)}, {"17", big.NewInt(17)}, {"128", big.NewInt(128)},
		{"2^63-1", new(big.Int).Sub(pow(63), big.NewInt(1))}, {"2^63", pow(63)}, {"2^63+1", new(big.Int).Add(pow(63), big.NewInt(1))},
		{"2^64-1", new(big.Int).Sub(pow(64), big.NewInt(1))}, {"2^64", pow(64)}, {"2^64+1", new(big.Int).Add(pow(64), big.NewInt(1))},
		{"2^71-1 (the largest 9-byte number)", new(big.Int).Sub(pow(71), big.NewInt(1))},
		{"2^100", pow(100)}, {"-1", big.NewInt(-1)}, {"-2^63", new(big.Int).Neg(pow(63))},
	}
	max := new(big.Int).Sub(pow(64), big.NewInt(1))
	for _, aIn := range []bool{true, false} {
		inAsset := f.idA
		if !aIn {
			inAsset = f.idB
		}
		base, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: aIn})
		for _, val := range values {
			encs := []struct {
				name string
				data []byte
			}{{"minimal", propScriptNum(val.v)}}
			for _, n := range []int{9, 10, 16} {
				if b := advNumPad(val.v, n); b != nil && !bytes.Equal(b, propScriptNum(val.v)) {
					encs = append(encs, struct {
						name string
						data []byte
					}{fmt.Sprintf("padded to %d bytes", n), b})
				}
			}
			ok := val.v.Sign() >= 0 && val.v.Cmp(max) <= 0
			for _, e := range encs {
				val, e := val, e
				t.Run(fmt.Sprintf("%s: %s %s", advDir(aIn), val.name, e.name), func(t *testing.T) {
					push := pushData(e.data)
					c := advCopy(base)
					c.args[6] = push
					if val.v.Sign() > 0 {
						c.outs = append(c.outs, out{1, propTokenOut(inAsset, val.v, changePkh)})
					}
					r := advH(t, f, c)
					t.Logf("output the minimal push: pool %v", advErrS(r.pool))
					if ok {
						mustPass(t, r)
					} else {
						mustFailPool(t, r)
					}
					if val.v.Sign() > 0 && e.name != "minimal" {
						g := advCopy(base)
						g.args[6] = push
						pre := append(append([]byte{0x20}, inAsset...), push...)
						g.outs = append(g.outs, out{1, append(append(pre, 0x6d), p2pkh(changePkh)...)})
						rg := advH(t, f, g)
						t.Logf("output the push as given: pool %v", advErrS(rg.pool))
						mustFailPool(t, rg)
					}
					if val.v.Sign() < 0 {
						g := advCopy(base)
						g.args[6] = push
						g.outs = append(g.outs, out{1, propTokenOut(inAsset, new(big.Int).Abs(val.v), changePkh)})
						mustFailPool(t, advH(t, f, g))
					}
				})
			}
		}
		for _, nz := range [][]byte{{0x80}, {0x00, 0x80}, append(make([]byte, 8), 0x80), append(make([]byte, 15), 0x80)} {
			nz := nz
			t.Run(fmt.Sprintf("%s: negative zero %x: no change output", advDir(aIn), nz), func(t *testing.T) {
				c := advCopy(base)
				c.args[6] = pushData(nz)
				mustPass(t, advH(t, f, c))
				g := advCopy(c)
				g.outs = append(g.outs, out{1, tokenP2pkh(inAsset, 1, changePkh)})
				mustFailPool(t, advH(t, f, g))
			})
		}
	}
}

// advShaped is the Swap the script's own arithmetic gives for any fee rates
// (each fee (amountIn*bps + 9999) / 10000, truncated toward zero as OP_DIV
// does; a fee output only where the fee is positive), on a pair of f's ids.
// ok is false when no net enters or nothing is paid out.
func advShaped(f *fixture, p pair, amountIn *big.Int, aIn bool, tc int64) (spend, bool) {
	fee := func(bps int64) *big.Int {
		x := new(big.Int).Mul(amountIn, big.NewInt(bps))
		x.Add(x, big.NewInt(9999))
		return x.Quo(x, big.NewInt(10000))
	}
	fees := []*big.Int{fee(f.bps[0]), fee(f.bps[1]), fee(f.bps[2])}
	net := new(big.Int).Sub(amountIn, fees[0])
	net.Sub(net, fees[1]).Sub(net, fees[2])
	if net.Sign() <= 0 {
		return spend{}, false
	}
	rin, rout := big.NewInt(p.a), big.NewInt(p.b)
	inAsset, outAsset, lpPkh := f.idA, f.idB, lpFeeAPkh
	if !aIn {
		rin, rout = rout, rin
		inAsset, outAsset, lpPkh = f.idB, f.idA, lpFeeBPkh
	}
	paid := new(big.Int).Quo(new(big.Int).Mul(net, rout), new(big.Int).Add(rin, net))
	newIn := new(big.Int).Add(rin, net)
	if paid.Sign() <= 0 || newIn.Cmp(propMax63) > 0 {
		return spend{}, false
	}
	after := p
	after.validator = advNext(p.validator)
	after.a, after.b = newIn.Int64(), p.b-paid.Int64()
	if !aIn {
		after.a, after.b = p.a-paid.Int64(), newIn.Int64()
	}
	outs := []out{{p.sats, f.poolLock(after)}, {1, f.vaultLock(after)}, {1, propTokenOut(outAsset, paid, userPkh)}}
	for i, h := range [][]byte{lpPkh, pkh(p.validator), commissionPkh} {
		if fees[i].Sign() > 0 {
			outs = append(outs, out{1, propTokenOut(inAsset, fees[i], h)})
		}
	}
	if tc > 0 {
		outs = append(outs, out{1, tokenP2pkh(inAsset, tc, changePkh)})
	}
	return spend{
		ins: f.ins(p), outs: outs, poolIdx: 0, vaultIdx: 1, method: methodSwap, signer: p.validator,
		args: [][]byte{
			pushData(after.validator.PubKey().Compressed()), propPushBig(amountIn), pushBool(aIn),
			pushData(userPkh), pushData(commissionPkh), pushData(changePkh), pushNum(tc),
		},
	}, true
}

// A pool deployed wrong for swapping: one token on both sides, or a negative
// fee rate. Swap is refused whatever its arguments, with the outputs the
// contract's arithmetic would write; Rescind and Close still work, and
// Rescind then Close.
func TestAdv_UnswappableDeploys(t *testing.T) {
	type dep struct {
		name     string
		idA, idB []byte
		bps      [3]int64
	}
	deps := []dep{
		{"one token on both sides (A, A)", assetA, assetA, [3]int64{30, 5, 10}},
		{"one token on both sides (B, B), no fees", assetB, assetB, [3]int64{0, 0, 0}},
		{"LP fee -1", assetA, assetB, [3]int64{-1, 0, 0}},
		{"validator fee -1", assetA, assetB, [3]int64{0, -1, 0}},
		{"commission -1", assetA, assetB, [3]int64{0, 0, -1}},
		{"LP fee -10000", assetA, assetB, [3]int64{-10_000, 0, 0}},
		{"LP fee -10^12", assetA, assetB, [3]int64{-1_000_000_000_000, 0, 0}},
		{"commission -2^40", assetA, assetB, [3]int64{0, 0, -(1 << 40)}},
		{"6000, 6000, -3000 (a 90% total)", assetA, assetB, [3]int64{6000, 6000, -3000}},
		{"30, 5, -10", assetA, assetB, [3]int64{30, 5, -10}},
		{"-30, -5, -10", assetA, assetB, [3]int64{-30, -5, -10}},
	}
	for _, d := range deps {
		d := d
		t.Run(d.name, func(t *testing.T) {
			f := advFixIDs(t, d.idA, d.idB, d.bps)
			p := fresh()
			tried := 0
			for _, aIn := range []bool{true, false} {
				for _, in := range []int64{1, 3, 40_000, 1_000_000_000} {
					for _, tc := range []int64{0, 777} {
						s, ok := advShaped(f, p, big.NewInt(in), aIn, tc)
						if !ok {
							continue
						}
						tried++
						variants := map[string]spend{"as shaped": s}
						if in != 40_000 {
							if r := advH(t, f, s); r.pool == nil {
								t.Errorf("%s in %d tc %d: Swap ACCEPTED", advDir(aIn), in, tc)
							}
							continue
						}
						c := advCopy(s)
						c.change = &out{900, changePkh}
						c.outs = append(c.outs, out{900, p2pkh(changePkh)})
						variants["with sats change"] = c
						c = advCopy(s)
						c.args[3] = pushData(pkh(key(0x45)))
						c.outs[2].script = append(c.outs[2].script[:len(c.outs[2].script)-25], p2pkh(pkh(key(0x45)))...)
						variants["another payout address"] = c
						for vn, v := range variants {
							if r := advH(t, f, v); r.pool == nil {
								t.Errorf("%s in %d tc %d (%s): Swap ACCEPTED", advDir(aIn), in, tc, vn)
							}
						}
					}
				}
			}
			if tried == 0 {
				t.Fatal("no shaped Swap: the case does not reach the check")
			}
			rs, after := f.rescind(p)
			mustPass(t, advH(t, f, rs))
			mustPass(t, advH(t, f, f.close(p)))
			mustPass(t, advH(t, f, f.close(after)))
			t.Logf("%d shaped Swaps refused (and variants); Rescind, Close, Rescind then Close pass", tried)
		})
	}
	t.Run("control: the same shapes on a well-deployed pair pass", func(t *testing.T) {
		f := advFix(t, [3]int64{30, 5, 10})
		for _, aIn := range []bool{true, false} {
			s, ok := advShaped(f, fresh(), big.NewInt(40_000), aIn, 777)
			if !ok {
				t.Fatal("not shaped")
			}
			mustPass(t, advH(t, f, s))
		}
	})
}

// Fee rates summing to 10000 bps or more, none negative: the contract has no
// sum check, relying on a positive amountIn leaving no net (each fee rounds
// up, so their sum is at least amountIn). Searched over amounts 1..3000,
// powers of two to 2^62 and their neighbours, both directions: no amount may
// leave a positive net. A sample runs through the interpreter (refused), and a
// negative amountIn, which makes the truncated fees negative and the net
// positive, is refused by amountIn > 0.
func TestAdv_FeeRatesSummingToTheWholeAmount(t *testing.T) {
	for _, bps := range [][3]int64{{5000, 5000, 0}, {10_000, 0, 0}, {3334, 3333, 3333}, {9999, 1, 0}, {2500, 2500, 5000}, {10_000, 10_000, 10_000}, {1 << 40, 0, 0}} {
		bps := bps
		t.Run(fmt.Sprintf("bps %v", bps), func(t *testing.T) {
			f := advFix(t, bps)
			p := fresh()
			var amounts []*big.Int
			for i := int64(1); i <= 3000; i++ {
				amounts = append(amounts, big.NewInt(i))
			}
			for e := uint(12); e <= 62; e++ {
				x := new(big.Int).Lsh(big.NewInt(1), e)
				amounts = append(amounts, new(big.Int).Sub(x, big.NewInt(1)), x, new(big.Int).Add(x, big.NewInt(1)))
			}
			for _, in := range amounts {
				for _, aIn := range []bool{true, false} {
					if _, ok := advShaped(f, p, in, aIn, 0); ok {
						t.Errorf("amountIn %v %s: a positive net and a payout under rates summing to %d", in, advDir(aIn), bps[0]+bps[1]+bps[2])
					}
				}
			}
			for _, in := range []int64{1, 2, 3, 10_000, 40_000, 1 << 40} {
				s, _ := f.swap(p, in, true)
				mustFailPool(t, advH(t, f, s))
			}
			neg := big.NewInt(-10_000)
			if s, ok := advShaped(f, p, neg, true, 0); ok {
				r := advH(t, f, s)
				t.Logf("amountIn -10000: truncated fees leave a positive net; pool %v", advErrS(r.pool))
				mustFailPool(t, r)
			}
		})
	}
}

// TRUST MODEL. Rúnar does not check the length of _changePKH: the change
// output is serialised as <amount> 19 76a914 <_changePKH> 88ac, the length
// byte fixed at 25, so a _changePKH of <20-byte hash> 88ac <8-byte value>
// <length byte> <a script minus its last two bytes> (the script ending
// 88ac) makes the serialised outputs read as the change output and then one
// more output. A Swap and a Rescind with that extra output after the change
// are accepted by both inputs. The contract cannot reach the argument; by
// the compiler's design today the validator, before signing, must refuse a
// _changePKH that is not 20 bytes and any output past the contract's.
func TestTrustModel_ChangePkhCarriesAnExtraOutput(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	extra := p2pkh(pkh(key(0x5c)))
	const extraSats, changeSats = 5_000, 4_321
	ext := append(append([]byte{}, changePkh...), 0x88, 0xac)
	for i := 0; i < 8; i++ {
		ext = append(ext, byte(uint64(extraSats)>>(8*i)))
	}
	ext = append(ext, byte(len(extra)))
	ext = append(ext, extra[:len(extra)-2]...)
	sw, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true})
	rs, _ := f.rescind(p)
	for _, m := range []struct {
		name string
		s    spend
	}{{"Swap", sw}, {"Rescind", rs}} {
		m := m
		t.Run(m.name+": an extra output smuggled through _changePKH", func(t *testing.T) {
			c := advCopy(m.s)
			c.change = &out{changeSats, ext}
			c.outs = append(c.outs, out{changeSats, p2pkh(changePkh)}, out{extraSats, extra})
			mustPass(t, advH(t, f, c))
			t.Logf("%s accepted with an output of %d sats to %x after the change", m.name, extraSats, extra)
			// the same transaction without the extra output is refused
			d := advCopy(c)
			d.outs = d.outs[:len(d.outs)-1]
			mustFailPool(t, advH(t, f, d))
		})
		t.Run(m.name+": a 19-byte _changePKH", func(t *testing.T) {
			c := advCopy(m.s)
			c.change = &out{changeSats, changePkh[:19]}
			c.outs = append(c.outs, out{changeSats, p2pkh(changePkh[:19])})
			mustFailPool(t, advH(t, f, c))
		})
	}
}

// No fee output can exceed BRC-162's largest amount, 2^64-1: amountIn is at
// most 2^64-1, and a fee is at most ceil(amountIn * bps / 10000) with bps
// below 10000 for any Swap that leaves a net. At 9999 bps, amountIn 2^64 and
// the smallest amountIn whose fee would be 2^64 or more are refused, with the
// outputs the script's arithmetic gives; amountIn 2^64-1 at
// 9999 bps passes, its fee written as the minimal push of
// ceil((2^64-1) * 9999 / 10000), under 2^64.
func TestAdv_FeeCannotExceedTheTokenMaximum(t *testing.T) {
	max := propPow2m1(64)
	for which, name := range []string{"LP fee", "validator fee", "commission"} {
		var bps [3]int64
		bps[which] = 9999
		f := advFix(t, bps)
		p := propPairOf(1_000_000_000_000_000, 1_000_000_000_000_000)
		for _, aIn := range []bool{true, false} {
			// the bound itself, and the smallest amountIn whose fee would pass 2^64-1
			over := new(big.Int).Mul(propPow2(64), big.NewInt(10000))
			over.Add(over, big.NewInt(9998)).Quo(over, big.NewInt(9999))
			for _, c := range []struct {
				name string
				in   *big.Int
				big  bool // its fee is above 2^64-1
			}{{"amountIn 2^64", propPow2(64), false}, {"the smallest amountIn with a fee of 2^64 or more", over, true}} {
				c := c
				t.Run(fmt.Sprintf("%s, %s: %s refused", name, advDir(aIn), c.name), func(t *testing.T) {
					s, v := propSpend(f, p, c.in, aIn, nil, nil)
					if v.ok || v.net.Sign() <= 0 || v.out.Sign() <= 0 || v.newA.Cmp(propMax63) > 0 || v.newB.Cmp(propMax63) > 0 {
						t.Fatalf("not an amountIn-only refusal: ok %v net %v (%s)", v.ok, v.net, v.why)
					}
					fee := []*big.Int{v.lp, v.val, v.com}[which]
					if (fee.Cmp(max) > 0) != c.big {
						t.Fatalf("fee %v above 2^64-1: %v, want %v", fee, fee.Cmp(max) > 0, c.big)
					}
					mustFailPool(t, advH(t, f, s))
				})
			}
			t.Run(fmt.Sprintf("%s, %s: amountIn 2^64-1, the largest fee", name, advDir(aIn)), func(t *testing.T) {
				s, v := propSpend(f, p, max, aIn, nil, nil)
				if !v.ok {
					t.Fatal(v.why)
				}
				fee := []*big.Int{v.lp, v.val, v.com}[which]
				want := propCeilFee(max, 9999)
				if fee.Cmp(want) != 0 || fee.Cmp(max) > 0 {
					t.Fatalf("fee %v, want %v", fee, want)
				}
				in := f.idA
				if !aIn {
					in = f.idB
				}
				if !bytes.HasPrefix(s.outs[3].script, propTokenPrefix(in, fee)) {
					t.Fatal("fee output is not the fee's minimal push")
				}
				mustPass(t, advH(t, f, s))
				t.Logf("largest %s: %v (%x)", name, fee, propScriptNum(fee))
			})
		}
	}
}

// --- stranding ---

// No accepted spend of a pool leaves its vault behind. Every advRun in both
// files asserts it (advStrandingCheck); here every arrangement of up to four
// inputs drawn from {pool, its vault, funding, a foreign vault} is run for
// each method, and the pool input passes exactly when the arrangement starts
// pool, vault.
func TestAdv_NoStranding(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	other := fresh()
	other.lp = key(11)
	for method := 0; method < 3; method++ {
		base := advValid(f, p, method, true)
		items := map[byte]in{
			'P': base.ins[0],
			'V': base.ins[1],
			'F': base.ins[2],
			'X': {txid: 0x33, vout: 1, sats: 1, lock: f.vaultLock(other)},
		}
		var perms []string
		var gen func(prefix string, left string)
		gen = func(prefix, left string) {
			if strings.Contains(prefix, "P") {
				perms = append(perms, prefix)
			}
			for i := range left {
				gen(prefix+string(left[i]), left[:i]+left[i+1:])
			}
		}
		gen("", "PVFX")
		accepted := 0
		for _, perm := range perms {
			s := advCopy(base)
			s.ins = nil
			s.poolIdx, s.vaultIdx = -1, -1
			for i := range perm {
				s.ins = append(s.ins, items[perm[i]])
				switch perm[i] {
				case 'P':
					s.poolIdx = i
				case 'V':
					s.vaultIdx = i
				}
			}
			r := advH(t, f, s)
			want := strings.HasPrefix(perm, "PV")
			if (r.pool == nil) != want {
				t.Errorf("%s inputs %s: pool accepted=%v, want %v (%v)", advMethodNames[method], perm, r.pool == nil, want, r.pool)
			}
			if r.pool == nil {
				accepted++
				if r.vault != nil {
					t.Errorf("%s inputs %s: pool accepted but its vault rejected: %v", advMethodNames[method], perm, r.vault)
				}
			}
		}
		t.Logf("%s: %d input arrangements, %d accepted (all starting pool, vault)", advMethodNames[method], len(perms), accepted)
	}
	t.Run("Close without the vault is rejected, not a stranded token B reserve", func(t *testing.T) {
		s := f.close(p)
		for _, ins := range [][]in{{s.ins[0]}, {s.ins[0], s.ins[2]}, {s.ins[0], s.ins[2], s.ins[1]}} {
			c := advCopy(s)
			c.ins, c.vaultIdx = ins, -1
			mustFailPool(t, advH(t, f, c))
		}
	})
	t.Logf("accepted pool inputs checked against the stranding invariant so far in this run: %d", advAccepted.Load())
}

// --- output-set attacks on Swap ---

func advSha(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// advVaultHash is VaultCodeHash for f's vault: sha256 of the vault script.
func (f *fixture) advVaultHash() []byte { return advSha(f.vault) }

// advCodeFor is the pool code (no prefix, no state) for other readonly args.
func advCodeFor(t testing.TB, a, b, vaultHash []byte, bps [3]int64) []byte {
	t.Helper()
	sa := advArt(t).sa
	p := pair{sats: 1, a: 1000, b: 1000, lp: key(1), validator: key(2)}
	h := hex.EncodeToString
	c := runar.NewRunarContract(&sa, []interface{}{
		p.a, p.b,
		h(p.lp.PubKey().Compressed()), h(p.validator.PubKey().Compressed()), h(validatorIdentity.PubKey().Compressed()),
		h(nonce), h(lpFeeAPkh), h(lpFeeBPkh), h(affirmationR), h(affirmationS),
		h(a), h(b), h(vaultHash), bps[0], bps[1], bps[2],
	})
	full, _ := hex.DecodeString(c.GetLockingScript())
	tail := append([]byte{0x6a}, p.state()...)
	if !bytes.HasSuffix(full, tail) {
		t.Fatal("SDK tail is not OP_RETURN || state")
	}
	return full[:len(full)-len(tail)]
}

func TestAdv_SwapOutputSet(t *testing.T) {
	otherAsset := bytes.Repeat([]byte{0xc7}, 32)
	advEach(t, func(t *testing.T, f *fixture, p pair, aIn bool) {
		s, r := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: aIn, tc: 777, change: &out{4_321, changePkh}})
		mustPass(t, advH(t, f, s))
		inAsset, outAsset := assetA, assetB
		if !aIn {
			inAsset, outAsset = assetB, assetA
		}
		n := len(s.outs)
		fees := []struct {
			name string
			amt  int64
			pkh  []byte
		}{}
		lpPkh := lpFeeAPkh
		if !aIn {
			lpPkh = lpFeeBPkh
		}
		if r.lpFee > 0 {
			fees = append(fees, struct {
				name string
				amt  int64
				pkh  []byte
			}{"LP fee", r.lpFee, lpPkh})
		}
		if r.valFee > 0 {
			fees = append(fees, struct {
				name string
				amt  int64
				pkh  []byte
			}{"validator fee", r.valFee, pkh(p.validator)})
		}
		if r.com > 0 {
			fees = append(fees, struct {
				name string
				amt  int64
				pkh  []byte
			}{"commission", r.com, commissionPkh})
		}
		tcIdx := 3 + len(fees)
		mut := func(name string, edit func(c *spend)) {
			t.Run(name, func(t *testing.T) {
				c := advCopy(s)
				edit(&c)
				mustFailPool(t, advH(t, f, c))
			})
		}
		mut("an extra output at the end", func(c *spend) { c.outs = append(c.outs, out{1, p2pkh(userPkh)}) })
		mut("an extra output in the middle (after the payout)", func(c *spend) {
			c.outs = append(c.outs[:3], append([]out{{1, tokenP2pkh(outAsset, 1, userPkh)}}, c.outs[3:]...)...)
		})
		mut("the payout twice", func(c *spend) { c.outs = append(c.outs, c.outs[2]) })
		for i := 0; i < n; i++ {
			i := i
			mut(fmt.Sprintf("output %d removed", i), func(c *spend) { c.outs = append(c.outs[:i], c.outs[i+1:]...) })
		}
		for i := 0; i+1 < n; i++ {
			i := i
			mut(fmt.Sprintf("outputs %d and %d swapped", i, i+1), func(c *spend) { c.outs[i], c.outs[i+1] = c.outs[i+1], c.outs[i] })
		}
		mut("the payout split in two", func(c *spend) {
			h := r.out / 2
			c.outs = append(c.outs[:2], append([]out{{1, tokenP2pkh(outAsset, h, userPkh)}, {1, tokenP2pkh(outAsset, r.out-h, userPkh)}}, c.outs[3:]...)...)
		})
		mut("the payout in the input token", func(c *spend) { c.outs[2].script = tokenP2pkh(inAsset, r.out, userPkh) })
		mut("the payout in a third token", func(c *spend) { c.outs[2].script = tokenP2pkh(otherAsset, r.out, userPkh) })
		mut("the payout of 2 sats", func(c *spend) { c.outs[2].sats = 2 })
		mut("the payout of 0 sats", func(c *spend) { c.outs[2].sats = 0 })
		for i, fe := range fees {
			i, fe := i, fe
			mut(fe.name+" in the output token", func(c *spend) { c.outs[3+i].script = tokenP2pkh(outAsset, fe.amt, fe.pkh) })
			mut(fe.name+" of 2 sats", func(c *spend) { c.outs[3+i].sats = 2 })
			mut(fe.name+" one more", func(c *spend) { c.outs[3+i].script = tokenP2pkh(inAsset, fe.amt+1, fe.pkh) })
		}
		mut("token change in the output token", func(c *spend) { c.outs[tcIdx].script = tokenP2pkh(outAsset, 777, changePkh) })
		mut("token change of 2 sats", func(c *spend) { c.outs[tcIdx].sats = 2 })
		mut("pool and vault swapped", func(c *spend) { c.outs[0], c.outs[1] = c.outs[1], c.outs[0] })
		mut("pool continuation with one more sat", func(c *spend) { c.outs[0].sats++ })
		mut("vault of 2 sats", func(c *spend) { c.outs[1].sats = 2 })
		mut("vault of 0 sats", func(c *spend) { c.outs[1].sats = 0 })
		mut("vault replaced by a token P2PKH of the same amount", func(c *spend) { c.outs[1].script = tokenP2pkh(assetB, r.after.b, userPkh) })
		mut("vault replaced by a plain P2PKH", func(c *spend) { c.outs[1].script = p2pkh(userPkh) })
		mut("vault with the pool's code", func(c *spend) { c.outs[1].script = append(tokenPrefix(assetB, r.after.b), f.code...) })
		mut("vault with OP_TRUE as code", func(c *spend) { c.outs[1].script = append(tokenPrefix(assetB, r.after.b), 0x51) })
		mut("vault code with a byte appended", func(c *spend) { c.outs[1].script = append(f.vaultLock(r.after), 0x61) })
		mut("vault code with its first byte changed", func(c *spend) {
			v := f.vaultLock(r.after)
			v[len(tokenPrefix(assetB, r.after.b))] ^= 1
			c.outs[1].script = v
		})
		mut("vault in token A", func(c *spend) { c.outs[1].script = append(tokenPrefix(assetA, r.after.b), f.vault...) })
		mut("vault in a third token", func(c *spend) { c.outs[1].script = append(tokenPrefix(otherAsset, r.after.b), f.vault...) })

		// another pool's code for the continuation
		state := r.after.state()
		for _, oc := range []struct {
			name string
			code func() []byte
		}{
			{"other AssetIdA", func() []byte { return advCodeFor(t, otherAsset, assetB, f.advVaultHash(), f.bps) }},
			{"other AssetIdB", func() []byte { return advCodeFor(t, assetA, otherAsset, f.advVaultHash(), f.bps) }},
			{"AssetIdA and AssetIdB swapped", func() []byte { return advCodeFor(t, assetB, assetA, f.advVaultHash(), f.bps) }},
			{"other fees", func() []byte {
				return advCodeFor(t, assetA, assetB, f.advVaultHash(), [3]int64{f.bps[0] + 1, f.bps[1], f.bps[2]})
			}},
			{"other VaultCodeHash", func() []byte {
				return advCodeFor(t, assetA, assetB, advSha(append(append([]byte{}, f.vault...), 0x61)), f.bps)
			}},
		} {
			oc := oc
			mut("pool continuation with "+oc.name+" (another pool's code)", func(c *spend) {
				code := oc.code()
				if bytes.Equal(code, f.code) {
					t.Fatal("other readonly args gave the same code")
				}
				lock := append(append(tokenPrefix(assetA, r.after.a), code...), 0x6a)
				c.outs[0].script = append(lock, state...)
			})
		}
		fields := []struct {
			name     string
			off, len int
		}{
			{"ReserveA", advOffA, 8}, {"ReserveB", advOffB, 8}, {"LpPubKey", advOffLp, 33},
			{"ValidatorPubKey", advOffVal, 33}, {"ValidatorIdentity", advOffId, 33}, {"Nonce", advOffNonce, 32},
			{"LpFeeAPkh", advOffFeeA, 20}, {"LpFeeBPkh", advOffFeeB, 20},
			{"AffirmationR", advOffAffR, 32}, {"AffirmationS", advOffAffS, 32},
		}
		for _, fd := range fields {
			for _, at := range []int{0, fd.len - 1} {
				fd, at := fd, at
				mut(fmt.Sprintf("state %s byte %d flipped", fd.name, at), func(c *spend) {
					st := append([]byte{}, state...)
					st[fd.off+at] ^= 0x01
					c.outs[0].script = f.advLockState(r.after.a, st)
				})
			}
		}
		mut("ReserveA and ReserveB swapped in state", func(c *spend) {
			st := append([]byte{}, state...)
			copy(st[0:8], state[8:16])
			copy(st[8:16], state[0:8])
			c.outs[0].script = f.advLockState(r.after.a, st)
		})
		mut("ReserveA and ReserveB swapped everywhere (prefixes and state)", func(c *spend) {
			sw := r.after
			sw.a, sw.b = sw.b, sw.a
			c.outs[0].script, c.outs[1].script = f.poolLock(sw), f.vaultLock(sw)
		})
		for _, d := range []int64{-1, 1} {
			d := d
			mut(fmt.Sprintf("pool prefix (A) %+d, state right", d), func(c *spend) {
				c.outs[0].script = f.advLockState(r.after.a+d, state)
			})
			mut(fmt.Sprintf("vault prefix (B) %+d, state right", d), func(c *spend) {
				a := r.after
				a.b += d
				c.outs[1].script = f.vaultLock(a)
			})
			mut(fmt.Sprintf("state ReserveB %+d, vault prefix right", d), func(c *spend) {
				a := r.after
				a.b += d
				c.outs[0].script = f.poolLock(a)
			})
			mut(fmt.Sprintf("state ReserveA %+d, pool prefix right", d), func(c *spend) {
				a := r.after
				a.a += d
				c.outs[0].script = f.advLockState(r.after.a, a.state())
			})
		}
	})
}

// --- trust model ---

// TRUST MODEL. The script sees no token input in either direction. With its
// key alone the validator signs a Swap of any amountIn and takes nearly all
// of the other reserve: token B with a fake A-in Swap, token A with a fake
// B-in Swap. Off-chain check carrying the weight: the validator confirms the
// token inputs of both assets cover the token outputs before signing (and the
// BRC-162 overlay refuses outputs its inputs do not cover).
func TestTrustModel_ValidatorDrainsWithFakeTokenInput(t *testing.T) {
	for _, bps := range advCfgs {
		f := advFix(t, bps)
		p := fresh()
		for _, aIn := range []bool{true, false} {
			rin, rout, outName := p.a, p.b, "B"
			if !aIn {
				rin, rout, outName = p.b, p.a, "A"
			}
			for _, mult := range []int64{10, 1000, 1_000_000_000} {
				name := fmt.Sprintf("bps %v: %s, amountIn %d x the input reserve, no token input", bps, advDir(aIn), mult)
				t.Run(name, func(t *testing.T) {
					s, r := f.swap(p, rin*mult, aIn)
					if !r.ok {
						t.Fatal("model rejects")
					}
					mustPass(t, advH(t, f, s))
					t.Logf("drained %d of %d token %s (%.9f of the reserve)", r.out, rout, outName, float64(r.out)/float64(rout))
				})
			}
		}
	}
}

// TRUST MODEL. Swap checks only the next key's type byte (02 or 03) and,
// writing the state, its 33 bytes: a validator naming 02/03 and 32 bytes no
// one holds (not a point on the curve, or a point whose key is unknown) ends
// Swap and Rescind for good, and the LP still closes. Off-chain check: the
// lookup and the LP watch for a rotation to a key the validator cannot sign
// with (the pool is then only closable).
func TestTrustModel_ValidatorNamesUnusableNextKey(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	nobody := advSha([]byte("a point no one holds the key to"))
	for _, nk := range []struct {
		name string
		b    []byte
	}{
		{"02 || ff*32 (x not on the curve)", append([]byte{0x02}, bytes.Repeat([]byte{0xff}, 32)...)},
		{"03 || ff*32 (x not on the curve)", append([]byte{0x03}, bytes.Repeat([]byte{0xff}, 32)...)},
		{"02 || 32 zero bytes", append([]byte{0x02}, make([]byte, 32)...)},
		{"02 || a hash as x", append([]byte{0x02}, nobody...)},
		{"a real key with its parity byte flipped", func() []byte {
			b := key(50).PubKey().Compressed()
			b[0] ^= 0x01
			return b
		}()},
	} {
		nk := nk
		t.Run(nk.name, func(t *testing.T) {
			s, r := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true, nextRaw: nk.b})
			mustPass(t, advH(t, f, s))
			after := r.after
			lock := f.advLockState(after.a, advStateVal(after, nk.b))
			for _, k := range []*ec.PrivateKey{p.validator, advNext(p.validator), p.lp, validatorIdentity} {
				for _, m := range []int{methodSwap, methodRescind} {
					c := advCopy(advValid(f, after, m, false))
					c.ins[0].lock = lock
					mustFailPool(t, advH(t, f, advWithSigner(c, k)))
				}
			}
			c := advCopy(f.close(after))
			c.ins[0].lock = lock
			mustPass(t, advH(t, f, c))
		})
	}
}

// The next key's shape: only a first byte of 02 or 03 passes, and only at
// 33 bytes (the contract's own Len == 33, on both compilers). Each refused
// case carries the continuation the contract would write for that key, so
// the refusal is the check's and not the outputs check's.
func TestAdv_NextValidatorKeyShape(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	body := bytes.Repeat([]byte{0x5a}, 32)
	t.Run("every first byte 00..ff, 32 bytes after it", func(t *testing.T) {
		passed := []byte{}
		for b := 0; b < 256; b++ {
			k := append([]byte{byte(b)}, body...)
			s, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: b%2 == 0, nextRaw: k})
			r := advH(t, f, s)
			if r.pool == nil {
				passed = append(passed, byte(b))
			}
			if want := b == 2 || b == 3; want != (r.pool == nil) {
				t.Errorf("first byte %02x: accepted=%v (%v)", b, r.pool == nil, r.pool)
			}
		}
		t.Logf("accepted first bytes: %x", passed)
	})
	real := advNext(p.validator).PubKey().Compressed()
	for _, first := range []byte{0x02, 0x03} {
		x := real[1:]
		for _, c := range []struct {
			name string
			k    []byte
		}{
			{"a valid point's x", append([]byte{first}, x...)},
			{"x not on the curve (ff*32)", append([]byte{first}, bytes.Repeat([]byte{0xff}, 32)...)},
			{"all-zero body", append([]byte{first}, make([]byte, 32)...)},
		} {
			c := c
			t.Run(fmt.Sprintf("%02x, %s: accepted (33 bytes, the type byte right)", first, c.name), func(t *testing.T) {
				s, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true, nextRaw: c.k})
				mustPass(t, advH(t, f, s))
			})
		}
	}
	lock := f.poolLock(p)
	body0 := lock[len(tokenPrefix(f.idA, p.a)):]
	// Other widths with the type byte right: refused, with the continuation
	// the compiler's poolScript writes for a key of w bytes (the spent
	// script's first len(code)+1+33-w bytes, then the new state). A
	// continuation of code || 6a || state would be refused by the outputs
	// check alone, the width check aside, so it is not tried.
	for _, w := range []int{1, 2, 32, 34, 64, 65} {
		w := w
		t.Run(fmt.Sprintf("%d bytes starting 02", w), func(t *testing.T) {
			k := append([]byte{0x02}, bytes.Repeat([]byte{0x5a}, w-1)...)
			s, r := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true, nextRaw: k})
			c := advCopy(s)
			c.outs[0].script = append(append(tokenPrefix(f.idA, r.after.a), body0[:len(f.code)+1+33-w]...), advStateVal(r.after, k)...)
			advCheck(t, fmt.Sprintf("next key of %d bytes starting 02, continuation as poolScript builds it", w), advH(t, f, c), advFail)
		})
	}
	t.Run("an empty key", func(t *testing.T) {
		s, r := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true, nextRaw: []byte{}})
		mustFailPool(t, advH(t, f, s))
		c := advCopy(s)
		c.outs[0].script = append(append(tokenPrefix(f.idA, r.after.a), body0[:len(f.code)+1+33]...), advStateVal(r.after, nil)...)
		mustFailPool(t, advH(t, f, c))
	})
	t.Run("an uncompressed key (04 first)", func(t *testing.T) {
		s, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true, nextRaw: advNext(p.validator).PubKey().Uncompressed()})
		mustFailPool(t, advH(t, f, s))
	})
	t.Run("33 zero bytes (the rescinded key)", func(t *testing.T) {
		s, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true, nextRaw: make([]byte, 33)})
		mustFailPool(t, advH(t, f, s))
	})
	t.Run("the key as a PUSHDATA1 push (same bytes)", func(t *testing.T) {
		s, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true})
		s.args[0] = append([]byte{0x4c, 33}, real...)
		mustPass(t, advH(t, f, s))
	})
}

// TRUST MODEL. The validator rescinds whenever it likes; the LP closes after.
// Off-chain check: none needed for safety (the LP keeps its funds); the
// lookup stops quoting a rescinded pool.
func TestTrustModel_ValidatorRescindsAnytime(t *testing.T) {
	for _, bps := range advCfgs {
		f := advFix(t, bps)
		p := fresh()
		for i, q := range []pair{p, func() pair { _, r := f.swap(p, 40_000, true); return r.after }()} {
			rs, after := f.rescind(q)
			mustPass(t, advH(t, f, rs))
			mustPass(t, advH(t, f, f.close(after)))
			t.Logf("bps %v pool %d: rescinded, then closed by the LP", bps, i)
		}
	}
}

// TRUST MODEL. The relay names any commissionPkh (and the taker any payout
// and change address). Off-chain check: the validator's signature covers the
// arguments it vetted; who receives the commission is the relay's business.
func TestTrustModel_RelayNamesAnyCommissionPkh(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	for name, com := range map[string][]byte{
		"the validator's own pkh": pkh(p.validator), "the LP's pkh": pkh(p.lp), "the user's pkh": userPkh,
		"20 zero bytes": make([]byte, 20), "LpFeeAPkh": lpFeeAPkh,
	} {
		com := com
		t.Run(name, func(t *testing.T) {
			for _, aIn := range []bool{true, false} {
				s, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: aIn, com: com})
				mustPass(t, advH(t, f, s))
			}
		})
	}
}

// TRUST MODEL. The continuation vault is always 1 sat: a vault holding more
// is not carried, the excess goes wherever the transaction sends it (here
// Rúnar's change). Off-chain check: the lookup's rule that a pair is deployed
// with a 1-sat vault.
func TestTrustModel_VaultSatsAreNotCarried(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	s, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true, change: &out{999, changePkh}})
	s.ins[1].sats = 1_000
	mustPass(t, advH(t, f, s))
	t.Log("a 1000-sat vault: continuation vault 1 sat, 999 sats left to the change output")
}

// --- two pools in one transaction ---

// The second pool input of a transaction can never verify: a pool must be
// input 0. A transaction spending two pools is invalid whatever its outputs.
func TestAdv_TwoPoolsInOneTransaction(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	g := advFix(t, [3]int64{0, 0, 0})
	p := fresh()
	for _, tw := range []struct {
		name string
		g    *fixture
		q    pair
	}{
		{"identical twin", f, p},
		{"distinct pool (other fees, other LP)", g, func() pair { q := fresh(); q.lp = key(12); return q }()},
	} {
		tw := tw
		for method := 0; method < 3; method++ {
			method := method
			t.Run(tw.name+": "+advMethodNames[method], func(t *testing.T) {
				first := advValid(f, p, method, true)
				second := advValid(tw.g, tw.q, method, true)
				ins := []in{
					first.ins[0], first.ins[1],
					{txid: 0x33, vout: 0, sats: tw.q.sats, lock: second.ins[0].lock},
					{txid: 0x33, vout: 1, sats: 1, lock: second.ins[1].lock},
					first.ins[2],
				}
				outs := append(clone(first.outs), second.outs...)
				a := advCopy(first)
				a.ins, a.outs = ins, outs
				ra := advH(t, f, a)
				t.Logf("first pool (input 0) with both pools' outputs: pool %v, vault %v", advErrS(ra.pool), advErrS(ra.vault))
				b := advCopy(second)
				b.ins, b.outs, b.poolIdx, b.vaultIdx = ins, outs, 2, 3
				rb := advH(t, tw.g, b)
				mustFailPool(t, rb)
				mustFailVault(t, rb)
				// the second pool's own outputs first: still not input 0
				b.outs = append(clone(second.outs), first.outs...)
				rb = advH(t, tw.g, b)
				mustFailPool(t, rb)
				mustFailVault(t, rb)
			})
		}
	}
}

// --- method selector and layout confusion ---

func TestAdv_MethodSelector(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pair, aIn bool) {
		t.Run("layout x selector: only the matching selector runs", func(t *testing.T) {
			sel := []struct {
				name string
				push []byte
				v    int64
			}{{"0", []byte{0x00}, 0}, {"1", []byte{0x51}, 1}, {"2", []byte{0x52}, 2}, {"3", []byte{0x53}, 3}, {"-1", []byte{0x4f}, -1}, {"16", []byte{0x60}, 16}, {"100", []byte{0x01, 0x64}, 100}}
			for m := 0; m < 3; m++ {
				for _, v := range sel {
					r := advRun(t, f, advValid(f, p, m, aIn), advOpts{method: v.push})
					if int64(m) == v.v && !r.ok() {
						t.Errorf("%s layout, selector %s: %v", advMethodNames[m], v.name, r.pool)
					}
					if int64(m) != v.v && r.pool == nil {
						t.Errorf("%s layout dispatched by selector %s", advMethodNames[m], v.name)
					}
				}
			}
		})
		t.Run("encodings of a selector dispatch by numeric value (recorded)", func(t *testing.T) {
			for m := 0; m < 3; m++ {
				v := byte(m)
				for _, e := range []struct {
					name string
					push []byte
					val  int64
				}{
					{"OP_PUSHBYTES_1 v", []byte{0x01, v}, int64(m)},
					{"two bytes v 00", []byte{0x02, v, 0x00}, int64(m)},
					{"PUSHDATA1 v", []byte{0x4c, 0x01, v}, int64(m)},
					{"PUSHDATA2 v", []byte{0x4d, 0x01, 0x00, v}, int64(m)},
					{"PUSHDATA4 v", []byte{0x4e, 0x01, 0x00, 0x00, 0x00, v}, int64(m)},
					{"negative zero 80", []byte{0x01, 0x80}, 0},
					{"negative zero 00 80", []byte{0x02, 0x00, 0x80}, 0},
					{"v with the sign bit (-v)", []byte{0x02, v, 0x80}, -int64(m)},
					{"v + 256", []byte{0x02, v, 0x01}, int64(m) + 256},
				} {
					r := advRun(t, f, advValid(f, p, m, aIn), advOpts{method: e.push})
					want := e.val == int64(m)
					t.Logf("%s layout, selector %s (%x): pool %v", advMethodNames[m], e.name, e.push, advErrS(r.pool))
					if want != (r.pool == nil) {
						t.Errorf("%s layout, selector %s (%x, value %d): accepted=%v, want %v", advMethodNames[m], e.name, e.push, e.val, r.pool == nil, want)
					}
				}
			}
		})
		t.Run("a push after the selector", func(t *testing.T) {
			for m := 0; m < 3; m++ {
				for v := int64(0); v < 3; v++ {
					if advRun(t, f, advValid(f, p, m, aIn), advOpts{trailing: [][]byte{pushNum(v)}}).pool == nil {
						t.Errorf("%s layout accepted with a trailing push of %d", advMethodNames[m], v)
					}
				}
			}
		})
		t.Run("no preimage", func(t *testing.T) {
			for m := 0; m < 3; m++ {
				mustFailPool(t, advRun(t, f, advValid(f, p, m, aIn), advOpts{noPre: true}))
			}
		})
		t.Run("one method's layout under another's selector, either signer", func(t *testing.T) {
			for m := 0; m < 3; m++ {
				for sel := 0; sel < 3; sel++ {
					if sel == m {
						continue
					}
					for _, k := range []*ec.PrivateKey{p.validator, p.lp} {
						r := advRun(t, f, advWithSigner(advValid(f, p, m, aIn), k), advOpts{method: pushNum(int64(sel))})
						if r.pool == nil {
							t.Errorf("%s layout under the %s selector accepted", advMethodNames[m], advMethodNames[sel])
						}
					}
				}
			}
		})
	})
}

// Extra pushes below the arguments, and signatures in the wrong layout.
func TestAdv_LeadingPushesAndLayouts(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pair, aIn bool) {
		junk := [][]byte{pushData(bytes.Repeat([]byte{0xee}, 33)), pushNum(1), pushNum(0)}
		for method := 0; method < 3; method++ {
			method := method
			mn := advMethodNames[method]
			base := advValid(f, p, method, aIn)
			tx := advTx(base, advOpts{})
			dig := advHash(advPreimage(t, tx, 0, base.ins[0].sats, advScriptCode(base.ins[0].lock), sighash.AllForkID, false))
			sigOf := func(k *ec.PrivateKey) []byte {
				s, _ := k.Sign(dig)
				return pushData(append(s.Serialize(), 0x41))
			}
			right, wrong := p.validator, p.lp
			if method == methodClose {
				right, wrong = p.lp, p.validator
			}
			for n := 1; n <= 3; n++ {
				n := n
				t.Run(fmt.Sprintf("%s: right signer, %d junk pushes below (recorded)", mn, n), func(t *testing.T) {
					r := advRun(t, f, base, advOpts{leading: junk[:n]})
					t.Logf("%s with %d leading pushes: pool %v", mn, n, advErrS(r.pool))
				})
				t.Run(fmt.Sprintf("%s: wrong signer, %d junk pushes below", mn, n), func(t *testing.T) {
					mustFailPool(t, advRun(t, f, advWithSigner(base, wrong), advOpts{leading: junk[:n]}))
				})
			}
			t.Run(mn+": the right signature pushed below, a wrong one in the slot", func(t *testing.T) {
				c := advWithSigner(base, wrong)
				mustFailPool(t, advRun(t, f, c, advOpts{leading: [][]byte{sigOf(right)}}))
				mustFailPool(t, advRun(t, f, c, advOpts{leading: [][]byte{sigOf(right), sigOf(wrong)}}))
				mustFailPool(t, advRun(t, f, c, advOpts{leading: [][]byte{sigOf(wrong), sigOf(right)}}))
			})
			t.Run(mn+": the right signature after the selector", func(t *testing.T) {
				mustFailPool(t, advRun(t, f, advWithSigner(base, wrong), advOpts{trailing: [][]byte{sigOf(right)}}))
			})
		}
		t.Run("the validator's signature in the Close layout, padded", func(t *testing.T) {
			for _, pad := range [][][]byte{nil, {pushNum(0)}, {pushNum(0), pushNum(0)}, {pushData(make([]byte, 20)), pushNum(0)}} {
				mustFailPool(t, advRun(t, f, advWithSigner(f.close(p), p.validator), advOpts{leading: pad}))
				mustFailPool(t, advRun(t, f, advValid(f, p, methodRescind, aIn), advOpts{method: pushNum(methodClose), leading: pad}))
			}
		})
		t.Run("the LP's signature in the Swap and Rescind layouts, padded", func(t *testing.T) {
			for _, pad := range [][][]byte{nil, {pushNum(0)}, {pushNum(0), pushNum(0)}} {
				for _, m := range []int{methodSwap, methodRescind} {
					mustFailPool(t, advRun(t, f, advWithSigner(advValid(f, p, m, aIn), p.lp), advOpts{leading: pad}))
				}
			}
		})
	})
}

// --- unlocking malleability ---

// With a validator-signed Swap in hand a third party may edit the unlocking
// script (it is not signed). Every accepted variant must leave the outputs
// exactly as signed (they are fixed here by construction and checked by the
// contract's hashOutputs); the expected acceptances are bytes that build no
// output (commissionPkh at a zero commission, tokenChangePkh at tokenChange 0,
// _changePKH at _changeAmount 0) and other encodings of the same values.
func TestAdv_UnlockingMalleability(t *testing.T) {
	advEach(t, func(t *testing.T, f *fixture, p pair, aIn bool) {
		base, r := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: aIn})
		_, baseTx := advRunTx(t, f, base, advOpts{})
		other20 := pushData(bytes.Repeat([]byte{0xcd}, 20))
		noCom := r.com == 0
		amt := scriptNum(40_000)
		real := append(append(outpoint(base.ins[0]), outpoint(base.ins[1])...), outpoint(base.ins[2])...)
		type mut struct {
			name string
			pass bool
			edit func(s *spend, o *advOpts)
		}
		arg := func(i int, v []byte) func(s *spend, o *advOpts) {
			return func(s *spend, o *advOpts) { s.args[i] = v }
		}
		muts := []mut{
			{"nextValidatorPubKey: another key", false, arg(0, pushData(key(0x31).PubKey().Compressed()))},
			{"nextValidatorPubKey as PUSHDATA1 (same bytes)", true, arg(0, append([]byte{0x4c, 33}, advNext(p.validator).PubKey().Compressed()...))},
			{"amountIn + 1", false, arg(1, pushNum(40_001))},
			{"amountIn - 1", false, arg(1, pushNum(39_999))},
			{"amountIn with a padding zero byte (same value)", true, arg(1, pushData(append(append([]byte{}, amt...), 0x00)))},
			{"amountIn as PUSHDATA1 (same value)", true, arg(1, append([]byte{0x4c, byte(len(amt))}, amt...))},
			{"aIn flipped", false, arg(2, pushBool(!aIn))},
			{"aIn as 01 01", aIn, arg(2, []byte{0x01, 0x01})},
			{"aIn as 01 00 (not the empty false)", false, arg(2, []byte{0x01, 0x00})},
			{"aIn as 01 02", false, arg(2, []byte{0x01, 0x02})},
			{"aIn as PUSHDATA1 of the same byte string", true, arg(2, map[bool][]byte{true: {0x4c, 0x01, 0x01}, false: {0x4c, 0x00}}[aIn])},
			{"userPkh changed", false, arg(3, other20)},
			{"commissionPkh changed", noCom, arg(4, other20)},
			{"tokenChangePkh changed (tokenChange 0)", true, arg(5, other20)},
			{"commissionPkh of 19 bytes", false, arg(4, pushData(bytes.Repeat([]byte{0xcd}, 19)))},
			{"commissionPkh of 21 bytes", false, arg(4, pushData(bytes.Repeat([]byte{0xcd}, 21)))},
			{"tokenChangePkh of 19 bytes (tokenChange 0)", false, arg(5, pushData(bytes.Repeat([]byte{0xcd}, 19)))},
			{"tokenChangePkh of 21 bytes (tokenChange 0)", false, arg(5, pushData(bytes.Repeat([]byte{0xcd}, 21)))},
			{"userPkh as PUSHDATA1 (same bytes)", true, arg(3, append([]byte{0x4c, 20}, userPkh...))},
			{"tokenChange 0 -> 1", false, arg(6, pushNum(1))},
			{"tokenChange 0 as 01 00", true, arg(6, []byte{0x01, 0x00})},
			{"tokenChange 0 as negative zero", true, arg(6, []byte{0x01, 0x80})},
			{"tokenChange -1", false, arg(6, []byte{0x01, 0x81})},
			{"_changePKH changed (_changeAmount 0)", true, func(s *spend, o *advOpts) { o.chPkh = other20 }},
			{"_changeAmount 0 -> 1", false, func(s *spend, o *advOpts) { o.chAmt = pushNum(1) }},
			{"_changeAmount 0 as 01 00", true, func(s *spend, o *advOpts) { o.chAmt = []byte{0x01, 0x00} }},
			{"allPrevouts as PUSHDATA2 (same bytes)", true, func(s *spend, o *advOpts) {
				o.rawPrev = append([]byte{0x4d, byte(len(real)), byte(len(real) >> 8)}, real...)
			}},
			{"allPrevouts one byte changed", false, func(s *spend, o *advOpts) {
				b := append([]byte{}, real...)
				b[100] ^= 1
				o.rawPrev = pushData(b)
			}},
			{"vaultCode as PUSHDATA4 (same bytes)", true, func(s *spend, o *advOpts) {
				n := len(f.vault)
				o.rawVault = append([]byte{0x4e, byte(n), byte(n >> 8), 0, 0}, f.vault...)
			}},
			{"vaultCode one byte changed", false, func(s *spend, o *advOpts) { s.vaultArg = advFlip(f.vault, len(f.vault)/2) }},
			{"vaultCode with a byte appended", false, func(s *spend, o *advOpts) { s.vaultArg = append(append([]byte{}, f.vault...), 0x61) }},
			{"vaultCode omitted", false, func(s *spend, o *advOpts) { o.noVault = true }},
			{"_codePart as PUSHDATA4 (same bytes)", true, func(s *spend, o *advOpts) {
				n := len(f.code)
				o.rawCode = append([]byte{0x4e, byte(n), byte(n >> 8), 0, 0}, f.code...)
			}},
			{"_codePart one byte changed", false, func(s *spend, o *advOpts) {
				cp := append([]byte{}, f.code...)
				cp[len(cp)/2] ^= 1
				o.codePart = cp
			}},
			{"_codePart empty", false, func(s *spend, o *advOpts) { o.codePart = []byte{} }},
			{"signature with S replaced by n-S (high S; accepted by the go-sdk interpreter, node policy not tested)", true, func(s *spend, o *advOpts) {
				o.sigTweak = func(b []byte) []byte {
					sig, err := ec.ParseDERSignature(b[:len(b)-1])
					if err != nil {
						t.Fatal(err)
					}
					sig.S = new(big.Int).Sub(ec.S256().N, sig.S)
					return append(sig.Serialize(), b[len(b)-1])
				}
			}},
			{"signature hashtype byte 41 -> 42", false, func(s *spend, o *advOpts) {
				o.sigTweak = func(b []byte) []byte { b[len(b)-1] = 0x42; return b }
			}},
			{"signature hashtype byte 41 -> c1", false, func(s *spend, o *advOpts) {
				o.sigTweak = func(b []byte) []byte { b[len(b)-1] = 0xc1; return b }
			}},
		}
		for _, m := range muts {
			m := m
			t.Run(m.name, func(t *testing.T) {
				s := advCopy(base)
				o := advOpts{}
				m.edit(&s, &o)
				res, tx := advRunTx(t, f, s, o)
				t.Logf("pool %v", advErrS(res.pool))
				if m.pass {
					mustPass(t, res)
				} else {
					mustFailPool(t, res)
				}
				if res.ok() {
					for i := range baseTx.Outputs {
						if !bytes.Equal(baseTx.Outputs[i].Bytes(), tx.Outputs[i].Bytes()) || len(baseTx.Outputs) != len(tx.Outputs) {
							t.Fatalf("an accepted unlocking variant moved output %d", i)
						}
					}
				}
			})
		}
	})
}

// --- Close always works for the LP, and only for the LP ---

func TestAdv_CloseAlwaysWorks(t *testing.T) {
	for _, bps := range advCfgs {
		f := advFix(t, bps)
		p := fresh()
		swapped := p
		for i := 0; i < 5; i++ {
			s, r := f.swap(swapped, int64(30_000+1000*i), i%2 == 0)
			mustPass(t, advH(t, f, s))
			swapped = r.after
		}
		_, rescinded := f.rescind(p)
		tiny := pair{sats: 1, a: 1, b: 1, lp: key(10), validator: key(20)}
		rich := fresh()
		rich.sats = 5_000
		huge := pair{sats: 1, a: 1<<63 - 1, b: 1<<63 - 1, lp: key(10), validator: key(20)}
		var many []out
		for i := 0; i < 20; i++ {
			many = append(many, out{int64(i), p2pkh(bytes.Repeat([]byte{byte(i)}, 20))})
		}
		for _, c := range []struct {
			name string
			q    pair
			outs []out
		}{
			{"fresh", p, nil},
			{"after five swaps", swapped, nil},
			{"after Rescind", rescinded, nil},
			{"reserves of 1 and 1", tiny, nil},
			{"reserves of 2^63-1", huge, nil},
			{"a pool output of 5000 sats", rich, nil},
			{"twenty unrelated outputs", p, many},
			{"a single OP_FALSE OP_RETURN of 0 sats", p, []out{{0, []byte{0x00, 0x6a}}}},
			{"a single OP_RETURN of 1 sat", p, []out{{1, []byte{0x6a}}}},
			{"everything to a third party", p, []out{{1, tokenP2pkh(assetA, p.a, userPkh)}, {1, tokenP2pkh(assetB, p.b, userPkh)}}},
		} {
			c := c
			t.Run(fmt.Sprintf("bps %v: %s", bps, c.name), func(t *testing.T) {
				s := f.close(c.q)
				if c.outs != nil {
					s.outs = c.outs
				}
				mustPass(t, advH(t, f, s))
				for _, k := range []*ec.PrivateKey{c.q.validator, validatorIdentity, key(0x31), advNext(c.q.validator)} {
					mustFailPool(t, advH(t, f, advWithSigner(s, k)))
				}
			})
		}
	}
}

// --- the compiled shape, the code-part witness, the state's shape ---
//
// Every test here runs on both compilers: Rúnar main (go.work) and the Rúnar
// go.mod pins (GOWORK=off), with the same expectation on both;
// `go test -v ./pool/pair/ | grep ADV-RECORD` lists the outcomes by compiler.
//
// How Swap and Rescind authenticate the _codePart witness (Rúnar's
// emitCodePartAuthentication, compilers/go/codegen/stack.go), on both
// compilers: after OP_PUSH_TX has proven the preimage, its scriptCode is
// lockingScript[2:] = codePart[2:] || 0x6a || state. The script splits
// scriptCode at SIZE(codePart)-2 and requires (clause 8a) the remainder to be
// exactly 1 + 251 bytes, the separator and this contract's fixed-width state;
// then (8b) its first byte 0x6a; then 0x61ab || the claimed tail == codePart
// byte for byte. With the state's length a compile-time constant, 8a pins the
// split point, so no shorter or longer code part (a cut into the code or into
// the state) passes. Close takes no _codePart: it writes no continuation, and
// it reads the state as the scriptCode's last 251 bytes.

func advFlip(b []byte, i int) []byte {
	c := append([]byte{}, b...)
	c[i] ^= 0x01
	return c
}

type advState struct {
	a, b                                       int64
	lp, val, id, nonce, feeA, feeB, affR, affS []byte
}

func advStateOf(p pair) advState {
	val := make([]byte, 33)
	if !p.rescinded {
		val = p.validator.PubKey().Compressed()
	}
	return advState{a: p.a, b: p.b, lp: p.lp.PubKey().Compressed(), val: val,
		id: validatorIdentity.PubKey().Compressed(), nonce: nonce, feeA: lpFeeAPkh, feeB: lpFeeBPkh,
		affR: affirmationR, affS: affirmationS}
}

// raw concatenates the state's fields at whatever width they have.
func (s advState) raw() []byte {
	b := make([]byte, 16)
	for i := 0; i < 8; i++ {
		b[i], b[8+i] = byte(uint64(s.a)>>(8*i)), byte(uint64(s.b)>>(8*i))
	}
	for _, f := range [][]byte{s.lp, s.val, s.id, s.nonce, s.feeA, s.feeB, s.affR, s.affS} {
		b = append(b, f...)
	}
	return b
}

// advSDKScript is the SDK's locking script (code || OP_RETURN || state) for
// this state and f's readonly args; an SDK panic comes back as an error.
func (f *fixture) advSDKScript(t testing.TB, s advState) (full []byte, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("SDK panic: %v", r)
		}
	}()
	sa := advArt(t).sa
	h := hex.EncodeToString
	c := runar.NewRunarContract(&sa, []interface{}{
		s.a, s.b, h(s.lp), h(s.val), h(s.id), h(s.nonce), h(s.feeA), h(s.feeB), h(s.affR), h(s.affS),
		h(f.idA), h(f.idB), h(f.advVaultHash()), f.bps[0], f.bps[1], f.bps[2],
	})
	return hex.DecodeString(c.GetLockingScript())
}

// advHandSplice instantiates the template as an off-chain builder would:
// each constructor slot's OP_0 placeholder replaced by the argument's push,
// each codeSepIndex slot (none here) resolved to OP_1.
func advHandSplice(t testing.TB, f *fixture) []byte {
	t.Helper()
	a := advArt(t)
	type sub struct {
		off int
		enc []byte
	}
	var subs []sub
	for _, cs := range a.CodeSepIndexSlots {
		subs = append(subs, sub{cs.ByteOffset, []byte{0x51}})
	}
	for _, s := range a.ConstructorSlots {
		if a.template[s.ByteOffset] != 0x00 {
			t.Fatalf("constructor slot %+v is not an OP_0 placeholder", s)
		}
		var enc []byte
		switch s.ParamIndex {
		case 10:
			enc = pushData(f.idA)
		case 11:
			enc = pushData(f.idB)
		case 12:
			enc = pushData(f.advVaultHash())
		case 13, 14, 15:
			enc = pushNum(f.bps[s.ParamIndex-13])
		default:
			t.Fatalf("unexpected constructor slot param %d", s.ParamIndex)
		}
		subs = append(subs, sub{s.ByteOffset, enc})
	}
	for i := 1; i < len(subs); i++ { // descending by offset
		for j := i; j > 0 && subs[j].off > subs[j-1].off; j-- {
			subs[j], subs[j-1] = subs[j-1], subs[j]
		}
	}
	code := append([]byte{}, a.template...)
	for _, s := range subs {
		code = append(append(append([]byte{}, code[:s.off]...), s.enc...), code[s.off+1:]...)
	}
	return code
}

func TestAdv_CompiledShape(t *testing.T) {
	a := advArt(t)
	f := advFix(t, [3]int64{30, 5, 10})
	t.Logf("ADV-RECORD %s | template %d bytes, deployed code %d bytes (fees 30/5/10)", advToolchain(t), len(a.template), len(f.code))
	if !bytes.HasPrefix(a.template, []byte{0x61, 0xab}) {
		t.Fatalf("template starts %x", a.template[:2])
	}
	// No variable-length state field: nothing for a deployer to patch, and
	// the separator's position is fixed.
	if len(a.CodePartLenSlots) != 0 || len(a.CodeSepIndexSlots) != 0 {
		t.Fatalf("codePartLenSlots %v, codeSepIndexSlots %v", a.CodePartLenSlots, a.CodeSepIndexSlots)
	}
	// Clause 8a, once in each method that takes _codePart (Swap, Rescind).
	if n := strings.Count(a.ASM, "OP_SIZE <fc00> OP_NUMEQUALVERIFY"); n != 2 {
		t.Fatalf("remainder-length checks: %d, want 2", n)
	}
	if len(f.poolLock(fresh())) != len(tokenPrefix(f.idA, fresh().a))+len(f.code)+1+advStateLen {
		t.Fatal("pool output is not prefix || code || 6a || 251-byte state")
	}
}

// With no length slot and no separator slot, a template with the constructor
// arguments spliced in at constructorSlots is the SDK's code, for every fee
// configuration, and spends.
func TestAdv_HandSplicedTemplateIsTheSDKCode(t *testing.T) {
	for _, bps := range [][3]int64{{0, 0, 0}, {16, 16, 16}, {30, 5, 10}, {16, 17, 1}, {17, 0, 0}, {0, 0, 1000}, {9999, 0, 0}} {
		bps := bps
		t.Run(fmt.Sprintf("bps %v", bps), func(t *testing.T) {
			f := advFix(t, bps)
			hand := advHandSplice(t, f)
			if !bytes.Equal(hand, f.code) {
				t.Fatalf("hand splice (%d bytes) differs from the SDK code (%d bytes)", len(hand), len(f.code))
			}
			g := *f
			g.code = hand
			p := fresh()
			sw, _ := g.swap(p, 4_000_000, true)
			rs, _ := g.rescind(p)
			mustPass(t, advH(t, &g, sw))
			mustPass(t, advH(t, &g, g.close(p)))
			mustPass(t, advH(t, &g, rs))
		})
	}
}

// _codePart tampering. Swap and Rescind refuse every variant (clause 8a, 8b
// or the byte comparison). Close takes no code part: one pushed anyway is a
// push below the arguments that Close ignores, so the LP's Close still
// passes and every other signer's still fails.
func TestAdv_CodePartWitness(t *testing.T) {
	a := advArt(t)
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	sw, _ := f.swap(p, 40_000, true)
	rs, _ := f.rescind(p)
	other := advCodeFor(t, bytes.Repeat([]byte{0xc7}, 32), assetB, f.advVaultHash(), f.bps)
	variants := []struct {
		name string
		cp   []byte
	}{
		{"truncated by 1", f.code[:len(f.code)-1]},
		{"extended by 00", append(append([]byte{}, f.code...), 0x00)},
		{"extended by 6a", append(append([]byte{}, f.code...), 0x6a)},
		{"first byte flipped", advFlip(f.code, 0)},
		{"middle byte flipped", advFlip(f.code, len(f.code)/2)},
		{"last byte flipped", advFlip(f.code, len(f.code)-1)},
		{"empty", []byte{}},
		{"the two prologue bytes", []byte{0x61, 0xab}},
		{"the template", a.template},
		{"another pool's code (other AssetIdA)", other},
	}
	for _, m := range []struct {
		name string
		s    spend
	}{{"Swap", sw}, {"Rescind", rs}} {
		mustPass(t, advH(t, f, m.s))
		for _, v := range variants {
			advCheck(t, "codePart "+v.name+": "+m.name, advRun(t, f, m.s, advOpts{codePart: v.cp}), advFail)
		}
		advCheck(t, "codePart omitted: "+m.name, advRun(t, f, m.s, advOpts{rawCode: []byte{}}), advFail)
	}
	cl := f.close(p)
	for _, v := range append(variants, struct {
		name string
		cp   []byte
	}{"the real code", f.code}) {
		advCheck(t, "Close with a code part pushed first ("+v.name+"), the LP signing", advRun(t, f, cl, advOpts{codePart: v.cp}), advPass)
		for _, k := range []*ec.PrivateKey{p.validator, validatorIdentity} {
			advCheck(t, "Close with a code part pushed first ("+v.name+"), another key signing", advRun(t, f, advWithSigner(cl, k), advOpts{codePart: v.cp}), advFail)
		}
	}
}

// advCloseBy is the Close of the pool whose lock is lock, signed by signer
// and paying everything to signer, with push (if not nil) as an extra first
// push.
func advCloseBy(t *testing.T, f *fixture, p pair, lock []byte, signer *ec.PrivateKey, push []byte) result {
	t.Helper()
	s := advCopy(f.close(p))
	s.ins[0].lock = lock
	s.signer = signer
	to := pkh(signer)
	s.outs = []out{{1, tokenP2pkh(f.idA, p.a, to)}, {1, tokenP2pkh(f.idB, p.b, to)}}
	return advRun(t, f, s, advOpts{codePart: push})
}

// A code part cut into the state: _codePart = script[:k] with script[k] ==
// 0x6a past the code, so that a reader taking the state from script[k+1:]
// would see every field moved by j+1 bytes (a cut at state byte j); with 0x6a
// at byte 16 of a key, the next key would land on LpPubKey. Swap and Rescind
// refuse every such cut (clause 8a), signed by the validator or the identity
// key, at every 0x6a state byte of the fresh pool and of pools whose LpPubKey
// or ValidatorPubKey has 0x6a at byte 16 or 24. Close takes no code part; with
// the cut pushed first anyway, no key but the LP's closes, the holder of the
// moved LpPubKey window included.
func TestAdv_CodePartCutIntoTheState(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	type pl struct {
		name string
		p    pair
	}
	pools := []pl{{"the fresh pool", fresh()}}
	for _, b := range []int{16, 24} {
		kb := advKeyWithByte(t, b, 0x6a)
		q := fresh()
		q.lp = kb
		pools = append(pools, pl{fmt.Sprintf("LpPubKey byte %d = 6a", b), q})
		q = fresh()
		q.validator = kb
		pools = append(pools, pl{fmt.Sprintf("ValidatorPubKey byte %d = 6a", b), q})
	}
	cuts := 0
	for _, x := range pools {
		p := x.p
		known := map[string]*ec.PrivateKey{}
		for _, k := range []*ec.PrivateKey{p.lp, p.validator, validatorIdentity} {
			known[string(k.PubKey().Compressed())] = k
		}
		lock := f.poolLock(p)
		body := lock[len(tokenPrefix(f.idA, p.a)):]
		state := p.state()
		sw, _ := f.swap(p, 40_000, true)
		rs, _ := f.rescind(p)
		for j := range state {
			if state[j] != 0x6a {
				continue
			}
			cuts++
			k := len(f.code) + 1 + j
			for mi, m := range []spend{sw, rs} {
				for _, sg := range []*ec.PrivateKey{p.validator, validatorIdentity} {
					advCheck(t, fmt.Sprintf("%s: %s with _codePart cut at state byte %d, signed by key %x", x.name, []string{"Swap", "Rescind"}[mi], j, sg.Serialize()[0]),
						advRun(t, f, advWithSigner(m, sg), advOpts{codePart: body[:k]}), advFail)
				}
			}
			holder := "nobody"
			if sh := j + 1; sh+advOffLp+33 <= len(state) {
				if hk, ok := known[string(state[sh+advOffLp:sh+advOffLp+33])]; ok {
					holder = fmt.Sprintf("key %x", hk.Serialize()[0])
				}
			}
			for _, sg := range []*ec.PrivateKey{p.validator, validatorIdentity} {
				advCheck(t, fmt.Sprintf("%s: Close with the cut at state byte %d pushed first (moved LpPubKey held by %s), signed by key %x", x.name, j, holder, sg.Serialize()[0]),
					advCloseBy(t, f, p, lock, sg, body[:k]), advFail)
			}
			advCheck(t, fmt.Sprintf("%s: the LP's Close with the cut at state byte %d pushed first", x.name, j), advCloseBy(t, f, p, lock, p.lp, body[:k]), advPass)
		}
	}
	t.Logf("ADV-RECORD %s | %d pools, %d state cuts, none accepted for Swap or Rescind, no non-LP Close", advToolchain(t), len(pools), cuts)
}

// The validator rotates, with one ordinary Swap, to a next key whose byte 16
// is 0x6a (raw bytes or a ground real key; both pass the type and width
// checks). The identity key still cannot Close the pair, with or without
// bytes pushed first; the LP can.
func TestAdv_RotationToAKeyWith6aAtByte16(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	raw := append([]byte{0x02}, bytes.Repeat([]byte{0x11}, 32)...)
	raw[16] = 0x6a
	for _, nk := range []struct {
		name string
		b    []byte
	}{{"a ground real key", advKeyWithByte(t, 16, 0x6a).PubKey().Compressed()}, {"raw bytes", raw}} {
		nk := nk
		t.Run(nk.name, func(t *testing.T) {
			s, r := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true, nextRaw: nk.b})
			mustPass(t, advH(t, f, s))
			after := r.after
			lock := f.advLockState(after.a, advStateVal(after, nk.b))
			body := lock[len(tokenPrefix(f.idA, after.a)):]
			mustPass(t, advCloseBy(t, f, after, lock, p.lp, nil))
			mustFailPool(t, advCloseBy(t, f, after, lock, validatorIdentity, nil))
			mustFailPool(t, advCloseBy(t, f, after, lock, validatorIdentity, body[:len(f.code)+1+advOffVal+16]))
		})
	}
}

// Cuts inside the code: Swap and Rescind with _codePart = code[:k] at every
// 0x6a byte of the code (asset ids of 0x6a bytes multiply them), refused by
// clause 8a.
func TestAdv_CodePartCutInsideTheCode(t *testing.T) {
	six := bytes.Repeat([]byte{0x6a}, 32)
	f := advFixIDs(t, six, append(bytes.Repeat([]byte{0x6a}, 31), 0x6b), [3]int64{30, 5, 10})
	p := fresh()
	sw, _ := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true})
	rs, _ := f.rescind(p)
	mustPass(t, advH(t, f, sw))
	mustPass(t, advH(t, f, rs))
	n := 0
	for k := 1; k < len(f.code); k++ {
		if f.code[k] != 0x6a {
			continue
		}
		n++
		for _, m := range []spend{sw, rs} {
			if advRun(t, f, m, advOpts{codePart: f.code[:k]}).pool == nil {
				t.Errorf("ACCEPTED: %s with _codePart cut at code byte %d", advMethodNames[m.method], k)
			}
		}
	}
	t.Logf("ADV-RECORD %s | %d in-code 0x6a cuts, Swap and Rescind each refused", advToolchain(t), n)
}

// A cut at a 0x6a inside the affirmation (a deploy chooses its 64 bytes):
// AffirmationR starting 0x6a, and the rest of it and AffirmationS chosen as
// the head of a state naming the deployer's key. 64 bytes cannot hold a
// 251-byte state, and clause 8a refuses the cut regardless: Swap and Rescind
// refuse it. The deployer's key cannot Close the pair either.
func TestAdv_CodePartCutIntoTheAffirmation(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	thief := key(0x5b)
	head := append(append(make([]byte, 16), thief.PubKey().Compressed()...), make([]byte, 14)...) // 63 bytes
	st := advStateOf(p)
	st.affR = append([]byte{0x6a}, head[:31]...)
	st.affS = head[31:63]
	lock := f.advLockState(p.a, st.raw())
	sdk, err := f.advSDKScript(t, st)
	if err != nil || !bytes.Equal(append(tokenPrefix(f.idA, p.a), sdk...), lock) {
		t.Fatalf("SDK lock differs from the hand-built one (%v)", err)
	}
	mustPass(t, advCloseBy(t, f, p, lock, p.lp, nil))
	k := len(f.code) + 1 + advOffAffR
	body := lock[len(tokenPrefix(f.idA, p.a)):]
	sw, _ := f.swap(p, 40_000, true)
	sw.ins[0].lock = lock
	rs, _ := f.rescind(p)
	rs.ins[0].lock = lock
	for _, sg := range []*ec.PrivateKey{p.validator, thief} {
		advCheck(t, "Swap with _codePart cut at AffirmationR's 6a", advRun(t, f, advWithSigner(sw, sg), advOpts{codePart: body[:k]}), advFail)
		advCheck(t, "Rescind with _codePart cut at AffirmationR's 6a", advRun(t, f, advWithSigner(rs, sg), advOpts{codePart: body[:k]}), advFail)
	}
	advCheck(t, "Close by the affirmation's chooser, the cut pushed first", advCloseBy(t, f, p, lock, thief, body[:k]), advFail)
}

// Swap with a nextValidatorPubKey of any width but 33, whatever its first
// byte, is refused by the contract's own width check on both compilers. Each
// case carries the continuation the compiler's poolScript would write for a
// key of w bytes (the spent script's first len(code)+1+33-w bytes, then the
// state), the shape that once let a validator lock both reserves on the
// pinned compiler. (A continuation of code || 6a || state would be refused
// by the outputs check alone, so it is not tried.) Were one accepted, the
// LP's Close of what it wrote is tried and reported.
func TestAdv_NextValidatorKeyWidth(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 0})
	p := fresh()
	lock := f.poolLock(p)
	body := lock[len(tokenPrefix(f.idA, p.a)):]
	nk := advNext(p.validator)
	for _, first := range []byte{0x02, 0x03, 0x04, 0x00} {
		for _, w := range []int{0, 1, 2, 32, 34, 64, 65} {
			if w == 0 && first != 0x02 {
				continue
			}
			first, w := first, w
			t.Run(fmt.Sprintf("first byte %02x, %d bytes", first, w), func(t *testing.T) {
				b := append(append([]byte{}, nk.PubKey().Compressed()...), bytes.Repeat([]byte{0x5a}, 64)...)[:w]
				if w > 0 {
					b[0] = first
				}
				s, r := advBuildSwap(f, p, advSwap{amountIn: 40_000, aIn: true, nextRaw: b})
				c := advCopy(s)
				c.outs[0].script = append(append(tokenPrefix(f.idA, r.after.a), body[:len(f.code)+1+33-w]...), advStateVal(r.after, b)...)
				res := advH(t, f, c)
				if res.ok() {
					t.Errorf("LP Close of the pair a %02x / %d-byte next key wrote: pool %v", first, w, advErrS(advCloseBy(t, f, r.after, c.outs[0].script, p.lp, nil).pool))
				}
				advCheck(t, fmt.Sprintf("Swap next key %02x / %d bytes, continuation as poolScript builds it", first, w), res, advFail)
			})
		}
	}
}

// Rescind writes ValidatorPubKey as exactly 33 zero bytes.
func TestAdv_RescindWritesThirtyThreeZeroBytes(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	for _, w := range []int{33, 32, 34} {
		rs, after := f.rescind(p)
		rs.outs[0].script = f.advLockState(after.a, advStateVal(p, make([]byte, w)))
		want := advFail
		if w == 33 {
			want = advPass
		}
		advCheck(t, fmt.Sprintf("Rescind writing %d zero bytes", w), advH(t, f, rs), want)
	}
}

// advClosers runs the Close of the pool whose lock is lock by the LP, the
// validator and the identity key, each to its own outputs, and returns who
// passed.
func advClosers(t *testing.T, f *fixture, p pair, lock []byte) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	for name, k := range map[string]*ec.PrivateKey{"LP": p.lp, "validator": p.validator, "identity": validatorIdentity} {
		got[name] = advCloseBy(t, f, p, lock, k, nil).pool == nil
	}
	return got
}

// advSwapRescindRefused asserts Swap and Rescind of the pool whose lock is
// lock are refused (the validator signing, the honest continuation).
func advSwapRescindRefused(t *testing.T, f *fixture, p pair, lock []byte) {
	t.Helper()
	sw, _ := f.swap(p, 40_000, true)
	rs, _ := f.rescind(p)
	for _, m := range []spend{sw, rs} {
		c := advCopy(m)
		c.ins[0].lock = lock
		mustFailPool(t, advH(t, f, c))
	}
}

// TRUST MODEL. Close reads the state as the script's last 251 bytes, so
// bytes appended after the state move every field, whatever they are. With
// n filler bytes (0xee, no key) the LpPubKey Close checks is the spent
// state's bytes 16+n..48+n: 33 and it is ValidatorPubKey (the validator
// Closes, outputs free), 66 and it is ValidatorIdentity (the identity key
// Closes); at 1, 32, 34 and 99 it is bytes no one holds (no one Closes, the
// LP included: the pair is locked). Swap and Rescind refuse every such pool
// (clause 8a). A whole second state appended is the state Close reads: its
// LpPubKey Closes. Off-chain check carrying the weight: the lookup (and the
// deploy's builder) refuse a pool whose script does not end exactly at the
// 251-byte state.
func TestTrustModel_BytesAfterTheStateMoveEveryField(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	for _, tc := range []struct {
		n      int
		closer string // "" for nobody
	}{{1, ""}, {32, ""}, {33, "validator"}, {34, ""}, {66, "identity"}, {99, ""}} {
		tc := tc
		t.Run(fmt.Sprintf("%d filler bytes", tc.n), func(t *testing.T) {
			lock := append(f.poolLock(p), bytes.Repeat([]byte{0xee}, tc.n)...)
			got := advClosers(t, f, p, lock)
			t.Logf("ADV-RECORD %s | %d filler bytes after the state: Close by %v", advToolchain(t), tc.n, got)
			for _, who := range []string{"LP", "validator", "identity"} {
				if got[who] != (who == tc.closer) {
					t.Errorf("Close by the %s: passed=%v, want %v", who, got[who], who == tc.closer)
				}
			}
			advSwapRescindRefused(t, f, p, lock)
		})
	}
	t.Run("a second state appended, naming a thief as LP", func(t *testing.T) {
		thief := key(0x5d)
		fake := p
		fake.lp = thief
		lock := append(f.poolLock(p), fake.state()...)
		mustPass(t, advCloseBy(t, f, p, lock, thief, nil))
		mustFailPool(t, advCloseBy(t, f, p, lock, p.lp, nil))
		advSwapRescindRefused(t, f, p, lock)
	})
}

// TRUST MODEL. A deploy whose state has a field one byte short or long:
// the SDK refuses to build it at Rúnar main (the pinned SDK builds it,
// recorded). Built by hand, Close reads the last 251 bytes, every field
// before the misshapen one moved by a byte: no key Closes, the LP included,
// and Swap and Rescind are refused, so the pair is locked. Two fields
// swapped: the state is what it says, and whoever it names as LP Closes.
// Off-chain check carrying the weight: the lookup's, that every field is at
// its width and in its place.
func TestTrustModel_MisshapenStateLocksThePair(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	p := fresh()
	fields := []struct {
		name string
		get  func(s *advState) *[]byte
	}{
		{"LpPubKey", func(s *advState) *[]byte { return &s.lp }},
		{"ValidatorPubKey", func(s *advState) *[]byte { return &s.val }},
		{"ValidatorIdentity", func(s *advState) *[]byte { return &s.id }},
		{"Nonce", func(s *advState) *[]byte { return &s.nonce }},
		{"LpFeeAPkh", func(s *advState) *[]byte { return &s.feeA }},
		{"LpFeeBPkh", func(s *advState) *[]byte { return &s.feeB }},
		{"AffirmationR", func(s *advState) *[]byte { return &s.affR }},
		{"AffirmationS", func(s *advState) *[]byte { return &s.affS }},
	}
	for _, fd := range fields {
		for _, d := range []int{-1, 1} {
			fd, d := fd, d
			t.Run(fmt.Sprintf("%s %+d byte", fd.name, d), func(t *testing.T) {
				st := advStateOf(p)
				fp := fd.get(&st)
				if d < 0 {
					*fp = (*fp)[:len(*fp)-1]
				} else {
					*fp = append(append([]byte{}, *fp...), 0x99)
				}
				_, err := f.advSDKScript(t, st)
				t.Logf("ADV-RECORD %s | %s %+d byte: the SDK builds it: %v", advToolchain(t), fd.name, d, err == nil)
				if advIsHead(t) && err == nil {
					t.Errorf("Rúnar main's SDK built a state with %s %+d byte", fd.name, d)
				}
				lock := f.advLockState(p.a, st.raw())
				got := advClosers(t, f, p, lock)
				if got["LP"] || got["validator"] || got["identity"] {
					t.Errorf("Close passed for %v", got)
				}
				advSwapRescindRefused(t, f, p, lock)
			})
		}
	}
	for _, sw := range []struct {
		name  string
		edit  func(s *advState)
		names *ec.PrivateKey // the key the swapped state names as LP
	}{
		{"LpPubKey and ValidatorPubKey swapped", func(s *advState) { s.lp, s.val = s.val, s.lp }, p.validator},
		{"Nonce and AffirmationR swapped", func(s *advState) { s.nonce, s.affR = s.affR, s.nonce }, p.lp},
		{"LpFeeAPkh and LpFeeBPkh swapped", func(s *advState) { s.feeA, s.feeB = s.feeB, s.feeA }, p.lp},
	} {
		sw := sw
		t.Run(sw.name, func(t *testing.T) {
			st := advStateOf(p)
			sw.edit(&st)
			mustPass(t, advCloseBy(t, f, p, f.advLockState(p.a, st.raw()), sw.names, nil))
		})
	}
}

// The affirmation is 64 opaque bytes: any values deploy and carry through
// Swap, Swap, Rescind and Close unchanged; Rúnar main's SDK refuses either
// half at any width but 32 (the pinned SDK's behaviour is recorded).
func TestAdv_AffirmationIsOpaque(t *testing.T) {
	f := advFix(t, [3]int64{30, 5, 10})
	sig, _ := key(0x21).Sign(advSha([]byte("pool")))
	pad := func(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }
	for _, v := range []struct {
		name       string
		affR, affS []byte
	}{
		{"zeros", make([]byte, 32), make([]byte, 32)},
		{"ff", bytes.Repeat([]byte{0xff}, 32), bytes.Repeat([]byte{0xff}, 32)},
		{"0x6a everywhere", bytes.Repeat([]byte{0x6a}, 32), bytes.Repeat([]byte{0x6a}, 32)},
		{"a real signature's r and s", pad(sig.R.Bytes()), pad(sig.S.Bytes())},
	} {
		v := v
		t.Run(v.name, func(t *testing.T) {
			lockOf := func(p pair) []byte {
				st := advStateOf(p)
				st.affR, st.affS = v.affR, v.affS
				full, err := f.advSDKScript(t, st)
				if err != nil {
					t.Fatal(err)
				}
				return append(tokenPrefix(f.idA, p.a), full...)
			}
			at := func(s spend, p pair) spend {
				s = advCopy(s)
				s.ins[0].lock = lockOf(p)
				return s
			}
			p := fresh()
			s1, r1 := f.swap(p, 40_000, true)
			s1 = at(s1, p)
			s1.outs[0].script = lockOf(r1.after)
			mustPass(t, advH(t, f, s1))
			s2, r2 := f.swap(r1.after, 7_000, false)
			s2 = at(s2, r1.after)
			s2.outs[0].script = lockOf(r2.after)
			mustPass(t, advH(t, f, s2))
			s3, after := f.rescind(r2.after)
			s3 = at(s3, r2.after)
			s3.outs[0].script = lockOf(after)
			mustPass(t, advH(t, f, s3))
			mustPass(t, advH(t, f, at(f.close(after), after)))
			// carried unchanged: a continuation with the fixture's affirmation is refused
			bad := advCopy(s1)
			bad.outs[0].script = f.poolLock(r1.after)
			mustFailPool(t, advH(t, f, bad))
		})
	}
	for _, w := range []int{0, 31, 33, 64} {
		st := advStateOf(fresh())
		st.affR = bytes.Repeat([]byte{0x72}, w)
		_, err := f.advSDKScript(t, st)
		t.Logf("ADV-RECORD %s | AffirmationR of %d bytes: the SDK builds it: %v", advToolchain(t), w, err == nil)
		if advIsHead(t) && err == nil {
			t.Errorf("Rúnar main's SDK built AffirmationR of %d bytes", w)
		}
	}
}
