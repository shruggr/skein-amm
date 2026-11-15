//go:build integration

// On-node (bitcoin-sv regtest) tests of pool/Pool.runar.go, compiled from
// source at test time by whichever Rúnar this module resolves (the checkout
// through go.local.mod, the skein-amm pin through go.pinned.mod; see
// README.md).
//
// WHAT THIS PROVES: the compiled pool's locking and unlocking scripts are
// valid under a real node's consensus rules and its standardness policy (as
// configured by Rúnar's integration/regtest.sh), every spend's outputs are the
// ones the contract computes (byte for byte, re-read from the mined chain),
// and the satoshi value flow of each spend (who pays the miner, what enters
// and leaves the pool). Negative cases show the node itself refuses them, and
// the go-sdk interpreter names the input and the opcodes it stopped at.
//
// WHAT THIS DOES NOT PROVE: token conservation. The node knows nothing about
// BRC-162: a "token output" is just a script with a
// `0x20 <assetId> <push amount> OP_2DROP` prefix, and the "token inputs" here
// are ordinary 1-sat outputs carrying a token-prefixed P2PKH script, made by a
// plain wallet transaction. Nothing on chain checks a token amount; the
// validator (which signs every token-bringing spend) and the overlays do.
package regtest

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	crypto "github.com/bsv-blockchain/go-sdk/primitives/hash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/icellan/runar/compilers/go/compiler"
	runar "github.com/icellan/runar/packages/runar-go"

	"github.com/shruggr/skein-amm/pool/regtest/internal/node"
)

// The contract's public methods in ABI order (checked in compiled()).
const (
	methodSwap = iota
	methodClose
	methodRescind
)

// Swap's declared args before allPrevouts, by index in spend.args.
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

// feeRate is what every spend in this suite pays the miner, in sat/kB.
// FEE_RATE_SAT_PER_KB overrides it.
var feeRate = func() int64 {
	if v := os.Getenv("FEE_RATE_SAT_PER_KB"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			panic(err)
		}
		return n
	}
	return 100
}()

// ---------------------------------------------------------------------------
// The compiled contract
// ---------------------------------------------------------------------------

// template is the artifact fields the independent build reads, parsed from
// the artifact JSON (not runar.RunarArtifact, so the pinned Rúnar's SDK, which
// may predate a field, builds the same suite).
type template struct {
	Script            string
	ConstructorSlots  []struct{ ParamIndex, ByteOffset int }
	CodeSepIndexSlots []struct{ ByteOffset, CodeSepIndex int }
	CodePartLenSlots  []struct{ ByteOffset int }
	ABI               struct {
		Constructor struct{ Params []struct{ Name string } }
		Methods     []struct {
			Name     string
			IsPublic bool
		}
	}
}

type compiledPool struct {
	sdk  runar.RunarArtifact
	tmpl template
	raw  []byte // template script bytes
}

var compileOnce struct {
	sync.Once
	c   *compiledPool
	err error
}

// poolSource is ../Pool.runar.go unless POOL_SOURCE names another file.
func poolSource() string {
	if s := os.Getenv("POOL_SOURCE"); s != "" {
		return s
	}
	return "../Pool.runar.go"
}

func compiled(t *testing.T) *compiledPool {
	t.Helper()
	compileOnce.Do(func() {
		compileOnce.c, compileOnce.err = compilePool()
	})
	if compileOnce.err != nil {
		t.Fatalf("compiling %s: %v", poolSource(), compileOnce.err)
	}
	return compileOnce.c
}

func compilePool() (*compiledPool, error) {
	art, err := compiler.CompileFromSource(poolSource())
	if err != nil {
		return nil, err
	}
	js, err := compiler.ArtifactToJSON(art)
	if err != nil {
		return nil, err
	}
	c := &compiledPool{}
	if err := json.Unmarshal(js, &c.sdk); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(js, &c.tmpl); err != nil {
		return nil, err
	}
	if c.raw, err = hex.DecodeString(c.tmpl.Script); err != nil {
		return nil, err
	}
	var params, methods []string
	for _, p := range c.tmpl.ABI.Constructor.Params {
		params = append(params, p.Name)
	}
	for _, m := range c.tmpl.ABI.Methods {
		if m.IsPublic {
			methods = append(methods, m.Name)
		}
	}
	if got, want := strings.Join(params, ","), "tokenReserve,lpPubKey,validatorPubKey,validatorIdentity,nonce,lpSatsPkh,lpTokenPkh,affirmationR,affirmationS,assetId,lpFeeBps,validatorFeeBps,commissionBps"; got != want {
		return nil, fmt.Errorf("constructor params changed: %s", got)
	}
	if got := strings.Join(methods, ","); got != "swap,close,rescind" {
		return nil, fmt.Errorf("public methods changed: %s", got)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// Script building (independent of the SDK; mirrors the contract)
// ---------------------------------------------------------------------------

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

// pushNum is a minimal push of a number.
func pushNum(n int64) []byte {
	switch {
	case n == -1:
		return []byte{0x4f}
	case n < 0:
		b := scriptNum(-n)
		if b[len(b)-1]&0x80 != 0 {
			b = append(b, 0x80)
		} else {
			b[len(b)-1] |= 0x80
		}
		return append([]byte{byte(len(b))}, b...)
	case n == 0:
		return []byte{0x00}
	case n <= 16:
		return []byte{byte(0x50 + n)}
	}
	b := scriptNum(n)
	return append([]byte{byte(len(b))}, b...)
}

// pushData is a minimal data push.
func pushData(b []byte) []byte {
	switch n := len(b); {
	case n == 0:
		return []byte{0x00}
	case n == 1 && b[0] >= 1 && b[0] <= 16:
		return []byte{0x50 + b[0]}
	case n == 1 && b[0] == 0x81:
		return []byte{0x4f}
	case n < 0x4c:
		return append([]byte{byte(n)}, b...)
	case n <= 0xff:
		return append([]byte{0x4c, byte(n)}, b...)
	case n <= 0xffff:
		return append([]byte{0x4d, byte(n), byte(n >> 8)}, b...)
	default:
		h := []byte{0x4e, 0, 0, 0, 0}
		binary.LittleEndian.PutUint32(h[1:], uint32(n))
		return append(h, b...)
	}
}

func pushBool(v bool) []byte {
	if v {
		return []byte{0x51}
	}
	return []byte{0x00}
}

// p2pkhScript is 76 a9 14 <h> 88 ac, whatever h's length (as the contract's
// p2pkh, which writes the 0x14 regardless).
func p2pkhScript(h []byte) []byte {
	return append(append([]byte{0x76, 0xa9, 0x14}, h...), 0x88, 0xac)
}

func pkh(k *ec.PrivateKey) []byte { return k.PubKey().Hash() }

// tokenPrefix is the BRC-162 value-output prefix with no payload.
func tokenPrefix(assetID []byte, amount int64) []byte {
	return tokenPrefixPush(assetID, pushNum(amount))
}

// tokenPrefixPush is a token prefix with a raw amount push.
func tokenPrefixPush(assetID, push []byte) []byte {
	p := append([]byte{0x20}, assetID...)
	return append(append(p, push...), 0x6d)
}

func tokenP2pkh(assetID []byte, amount int64, to []byte) []byte {
	return append(tokenPrefix(assetID, amount), p2pkhScript(to)...)
}

// params is what a pool is deployed with and carries for its life.
type params struct {
	lpBps, validatorBps, commissionBps                      int64
	assetID, nonce, lpSatsPkh, lpTokenPkh, affirmR, affirmS []byte
	identity                                                *ec.PrivateKey
}

// poolState is what changes across spends.
type poolState struct {
	bsv, tokens   int64
	lp, validator *ec.PrivateKey
	rescinded     bool   // ValidatorPubKey cleared (33 zero bytes)
	validatorRaw  []byte // overrides the validator pubkey bytes (malformed-key cases)
}

func (s poolState) validatorPub() []byte {
	switch {
	case s.validatorRaw != nil:
		return s.validatorRaw
	case s.rescinded:
		return make([]byte, 33)
	}
	return s.validator.PubKey().Compressed()
}

// stateBytes is TokenReserve (8 LE) || LpPubKey || ValidatorPubKey ||
// ValidatorIdentity (33 each) || Nonce (32) || LpSatsPkh || LpTokenPkh (20
// each) || AffirmationR || AffirmationS (32 each): 243 bytes, every field
// fixed-width.
func stateBytes(p params, s poolState) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(s.tokens))
	b = append(b, s.lp.PubKey().Compressed()...)
	b = append(b, s.validatorPub()...)
	b = append(b, p.identity.PubKey().Compressed()...)
	b = append(b, p.nonce...)
	b = append(b, p.lpSatsPkh...)
	b = append(b, p.lpTokenPkh...)
	b = append(b, p.affirmR...)
	return append(b, p.affirmS...)
}

// poolScript is the on-chain locking script: prefix || code || OP_RETURN || state.
func poolScript(p params, code []byte, s poolState) []byte {
	out := append(tokenPrefix(p.assetID, s.tokens), code...)
	out = append(out, 0x6a)
	return append(out, stateBytes(p, s)...)
}

// sdkCode deploys through the Rúnar Go SDK (NewRunarContract +
// GetLockingScript, which builds code || OP_RETURN || state) and returns the
// code part, after checking the SDK's state section equals stateBytes.
func sdkCode(t *testing.T, c *compiledPool, p params, s poolState) []byte {
	t.Helper()
	ct := runar.NewRunarContract(&c.sdk, []interface{}{
		s.tokens,
		hex.EncodeToString(s.lp.PubKey().Compressed()),
		hex.EncodeToString(s.validatorPub()),
		hex.EncodeToString(p.identity.PubKey().Compressed()),
		hex.EncodeToString(p.nonce),
		hex.EncodeToString(p.lpSatsPkh),
		hex.EncodeToString(p.lpTokenPkh),
		hex.EncodeToString(p.affirmR),
		hex.EncodeToString(p.affirmS),
		hex.EncodeToString(p.assetID),
		p.lpBps, p.validatorBps, p.commissionBps,
	})
	full, err := hex.DecodeString(ct.GetLockingScript())
	if err != nil {
		t.Fatal(err)
	}
	tail := append([]byte{0x6a}, stateBytes(p, s)...)
	if !bytes.HasSuffix(full, tail) {
		t.Fatalf("SDK state section differs from the independent layout:\n sdk  %x\n want %x", full[max(0, len(full)-len(tail)):], tail)
	}
	return full[:len(full)-len(tail)]
}

// handCode builds the pool code from the artifact template WITHOUT the SDK:
// each constructor slot's OP_0 placeholder replaced by the arg's minimal push.
// The fixed-width state leaves the artifact no codeSepIndexSlots and no
// codePartLenSlots; if a compiler emits either, the template is no longer
// constructor slots alone and this fails, naming them.
func handCode(t *testing.T, c *compiledPool, p params) []byte {
	t.Helper()
	if n, m := len(c.tmpl.CodeSepIndexSlots), len(c.tmpl.CodePartLenSlots); n != 0 || m != 0 {
		t.Fatalf("the artifact has %d codeSepIndexSlots and %d codePartLenSlots: a template splice of the constructor slots alone is not the deployed code", n, m)
	}
	type sub struct {
		off int
		b   []byte
	}
	var subs []sub
	for _, s := range c.tmpl.ConstructorSlots {
		var b []byte
		switch s.ParamIndex {
		case 9:
			b = pushData(p.assetID)
		case 10:
			b = pushNum(p.lpBps)
		case 11:
			b = pushNum(p.validatorBps)
		case 12:
			b = pushNum(p.commissionBps)
		default:
			t.Fatalf("constructor slot for param %d: only the readonly args (9..12) are expected in the code", s.ParamIndex)
		}
		subs = append(subs, sub{s.ByteOffset, b})
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].off < subs[j].off })
	var code []byte
	pos := 0
	for _, s := range subs {
		if c.raw[s.off] != 0x00 {
			t.Fatalf("template byte %d is %#x, not the OP_0 placeholder", s.off, c.raw[s.off])
		}
		code = append(code, c.raw[pos:s.off]...)
		code = append(code, s.b...)
		pos = s.off + 1
	}
	return append(code, c.raw[pos:]...)
}

// separatorScriptCode is the locking script after the pool's OP_CODESEPARATOR
// (the prologue `61 ab` right after the token prefix): the BIP-143 scriptCode.
func separatorScriptCode(t *testing.T, lock []byte) []byte {
	t.Helper()
	off := 1 + 32
	switch op := lock[off]; {
	case op == 0x00 || (op >= 0x51 && op <= 0x60):
		off++
	default:
		off += 1 + int(op)
	}
	off++ // OP_2DROP
	if lock[off] != 0x61 || lock[off+1] != 0xab {
		t.Fatalf("separator not where expected in %x...", lock[:min(len(lock), off+4)])
	}
	return lock[off+2:]
}

// ---------------------------------------------------------------------------
// Keys and fixtures
// ---------------------------------------------------------------------------

func newKey(t *testing.T) *ec.PrivateKey {
	t.Helper()
	k, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// newParams is a pool's deploy-time values; the affirmation is 64 random
// bytes as r and s (the contract never checks it; TestAffirmationCarried
// uses a real signature).
func newParams(t *testing.T, lpBps, validatorBps, commissionBps int64) params {
	return params{
		lpBps: lpBps, validatorBps: validatorBps, commissionBps: commissionBps,
		assetID:    randBytes(t, 32),
		nonce:      randBytes(t, 32),
		lpSatsPkh:  pkh(newKey(t)),
		lpTokenPkh: pkh(newKey(t)),
		affirmR:    randBytes(t, 32),
		affirmS:    randBytes(t, 32),
		identity:   newKey(t),
	}
}

// newPoolState is a fresh pool's state: 1,000,000 sats against 5,000,000
// tokens (realistic magnitudes: the swap products exceed 4-byte numbers).
func newPoolState(t *testing.T) poolState {
	return poolState{bsv: 1_000_000, tokens: 5_000_000, lp: newKey(t), validator: newKey(t)}
}

// ---------------------------------------------------------------------------
// Report (printed by TestMain)
// ---------------------------------------------------------------------------

type txStat struct {
	test, label string
	size, fee   int64
	unlockLen   int
	accepted    bool
	reason      string
}

var report struct {
	sync.Mutex
	rows []txStat
}

func record(r txStat) {
	report.Lock()
	report.rows = append(report.rows, r)
	report.Unlock()
}

// ---------------------------------------------------------------------------
// Coins, the funder, transactions
// ---------------------------------------------------------------------------

type out struct {
	sats   int64
	script []byte
}

// coin is a spendable output. txid "" means "not minted yet": the funder
// creates it right before the spend that uses it.
type coin struct {
	txid string
	vout int
	sats int64
	lock []byte
	key  *ec.PrivateKey // P2PKH (or token-prefixed P2PKH) signer
}

func hashOf(t *testing.T, txid string) *chainhash.Hash {
	t.Helper()
	h, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// env is one test's funder: a fresh key holding one big coin from the node
// wallet. Every test makes its own, so tests share nothing.
type env struct {
	t *testing.T
	f *funder
}

type funder struct {
	key  *ec.PrivateKey
	coin *coin
}

// sub is e for a subtest, sharing the funder (subtests using it run in order).
func (e *env) sub(t *testing.T) *env { return &env{t: t, f: e.f} }

func newEnv(t *testing.T) *env {
	t.Helper()
	k := newKey(t)
	addr, err := script.NewAddressFromPublicKey(k.PubKey(), false)
	if err != nil {
		t.Fatal(err)
	}
	const sats = 50_000_000
	txid, err := node.SendToAddress(addr.AddressString, sats)
	if err != nil {
		t.Fatalf("sendtoaddress: %v", err)
	}
	tx := fetchTx(t, txid)
	for i, o := range tx.Outputs {
		if bytes.Equal(*o.LockingScript, p2pkhScript(pkh(k))) {
			return &env{t: t, f: &funder{key: k, coin: &coin{txid: txid, vout: i, sats: int64(o.Satoshis), lock: p2pkhScript(pkh(k)), key: k}}}
		}
	}
	t.Fatalf("funding tx %s has no output to the funder", txid)
	return nil
}

func fetchTx(t *testing.T, txid string) *transaction.Transaction {
	t.Helper()
	h, err := node.RawTransactionHex(txid)
	if err != nil {
		t.Fatalf("getrawtransaction %s: %v", txid, err)
	}
	tx, err := transaction.NewTransactionFromHex(h)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

// confirm mines until txid is in a block. bitcoin-sv's block assembler picks
// up a just-accepted transaction asynchronously, so under parallel tests a
// block mined right after a broadcast can miss it; mining another is harmless.
func confirm(t *testing.T, txid string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if err := node.Mine(1); err != nil {
			t.Fatalf("mine: %v", err)
		}
		n, _, err := node.Confirmations(txid)
		if err != nil {
			t.Fatalf("getrawtransaction %s: %v", txid, err)
		}
		if n >= 1 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s still unconfirmed after 50 blocks", txid)
}

// pay spends the funder's coin to outs plus change back to the funder, signs,
// broadcasts and returns the txid. The funder's coin becomes the change.
func (e *env) pay(outs []out) string {
	t := e.t
	t.Helper()
	const fee = 500 // these are small P2PKH transactions
	var total int64
	for _, o := range outs {
		total += o.sats
	}
	change := e.f.coin.sats - total - fee
	if change < 1000 {
		t.Fatalf("funder exhausted: %d sats left", e.f.coin.sats)
	}
	tx := transaction.NewTransaction()
	addInput(t, tx, e.f.coin)
	for _, o := range outs {
		addOutput(tx, o)
	}
	addOutput(tx, out{change, e.f.coin.lock})
	signP2pkh(t, tx, 0, e.f.coin.key)
	txid, err := node.SendRawTransaction(tx.Hex())
	if err != nil {
		t.Fatalf("funder broadcast: %v", err)
	}
	e.f.coin = &coin{txid: txid, vout: len(outs), sats: change, lock: e.f.coin.lock, key: e.f.key}
	return txid
}

// mint creates the unminted coins in one funder transaction.
func (e *env) mint(coins []*coin) {
	var outs []out
	var need []*coin
	for _, c := range coins {
		if c.txid == "" {
			outs = append(outs, out{c.sats, c.lock})
			need = append(need, c)
		}
	}
	if len(need) == 0 {
		return
	}
	txid := e.pay(outs)
	for i, c := range need {
		c.txid, c.vout = txid, i
	}
}

func addInput(t *testing.T, tx *transaction.Transaction, c *coin) {
	t.Helper()
	txid := c.txid
	if txid == "" {
		txid = strings.Repeat("22", 32) // a size-measuring draft
	}
	in := &transaction.TransactionInput{SourceTXID: hashOf(t, txid), SourceTxOutIndex: uint32(c.vout), SequenceNumber: 0xffffffff}
	in.SetSourceTxOutput(&transaction.TransactionOutput{Satoshis: uint64(c.sats), LockingScript: script.NewFromBytes(c.lock)})
	tx.AddInput(in)
}

func addOutput(tx *transaction.Transaction, o out) {
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: uint64(o.sats), LockingScript: script.NewFromBytes(o.script)})
}

func signP2pkh(t *testing.T, tx *transaction.Transaction, idx int, k *ec.PrivateKey) {
	t.Helper()
	u, err := p2pkh.Unlock(k, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := u.Sign(tx, uint32(idx))
	if err != nil {
		t.Fatal(err)
	}
	tx.Inputs[idx].UnlockingScript = s
}

// ---------------------------------------------------------------------------
// Pools and spends
// ---------------------------------------------------------------------------

type pool struct {
	p    params
	code []byte // deployed code: prologue .. last code byte, readonly args in
	st   poolState
	txid string // current UTXO
	vout int
	lock []byte // its locking script as on chain
}

func (p *pool) coin() *coin {
	return &coin{txid: p.txid, vout: p.vout, sats: p.st.bsv, lock: p.lock}
}

// deploySDK deploys a pool whose code the Rúnar SDK built, after checking it
// equals the independent build from the template.
func (e *env) deploySDK(p params, st poolState) *pool {
	t := e.t
	t.Helper()
	c := compiled(t)
	code := sdkCode(t, c, p, st)
	if hc := handCode(t, c, p); !bytes.Equal(hc, code) {
		t.Fatalf("independently built code (%d bytes) differs from the SDK's (%d bytes)", len(hc), len(code))
	}
	return e.deployCode(p, st, code, "deploy")
}

// deployCode deploys tokenPrefix || code || OP_RETURN || state holding st.bsv,
// mines it and checks the chain holds exactly that output.
func (e *env) deployCode(p params, st poolState, code []byte, label string) *pool {
	t := e.t
	t.Helper()
	lock := poolScript(p, code, st)
	txid := e.pay([]out{{st.bsv, lock}})
	confirm(t, txid)
	tx := fetchTx(t, txid)
	if got := tx.Outputs[0]; int64(got.Satoshis) != st.bsv || !bytes.Equal(*got.LockingScript, lock) {
		t.Fatalf("deployed output differs from the expected pool script")
	}
	record(txStat{test: t.Name(), label: label, size: int64(len(tx.Bytes())), fee: 500, accepted: true})
	t.Logf("%s: %s, pool script %d bytes (code %d), %d sats", label, txid, len(lock), len(code), st.bsv)
	return &pool{p: p, code: code, st: st, txid: txid, vout: 0, lock: lock}
}

// spend is one transaction spending one pool (or, for the twin-pool cases,
// several byte-identical pools with the same unlocking arguments).
type spend struct {
	label     string
	method    int
	pool      *pool
	poolIndex int
	// twins are further pool inputs placed right after pool, each unlocked
	// with the same method, args, signer and change as pool.
	twins []*pool
	// others are the non-pool inputs in order (the pools are inserted at
	// poolIndex). Unminted coins are minted by the funder first; payer (one of
	// them) is set to exactly what the outputs need beyond every other input,
	// plus the fee.
	others []*coin
	payer  *coin
	outs   []out
	// feeFromOut >= 0 takes the fee from that output instead (no payer).
	feeFromOut int
	// args are the declared args before allPrevouts (Swap and Rescind) or the
	// preimage (Close); args[0] is the signature slot.
	args   [][]byte
	signer *ec.PrivateKey
	// sigFlag is the signer's sighash type: it signs that type's digest and
	// appends that byte. 0: ALL|FORKID.
	sigFlag sighash.Flag
	// prevouts is allPrevouts (Swap, Rescind); nil: the transaction's real
	// input outpoints.
	prevouts   []byte
	changePkh  []byte // Rúnar change (_changePKH, _changeAmount); nil: none
	changeSats int64
}

func (s *spend) pools() []*pool { return append([]*pool{s.pool}, s.twins...) }

type built struct {
	tx        *transaction.Transaction
	outs      []out // the outputs as built (fee taken)
	fee       int64
	unlockLen int   // the first pool input's unlocking script
	poolIdx   []int // the pool inputs' indices
}

// preimage is the BIP-143 preimage of input idx with the given scriptCode.
func preimage(t *testing.T, tx *transaction.Transaction, idx int, sats int64, scriptCode []byte, flag sighash.Flag) []byte {
	t.Helper()
	cp := tx.ShallowClone()
	in := tx.Inputs[idx]
	cp.Inputs[idx] = &transaction.TransactionInput{SourceTXID: in.SourceTXID, SourceTxOutIndex: in.SourceTxOutIndex, SequenceNumber: in.SequenceNumber}
	cp.Inputs[idx].SetSourceTxOutput(&transaction.TransactionOutput{Satoshis: uint64(sats), LockingScript: script.NewFromBytes(scriptCode)})
	pre, err := cp.CalcInputPreimage(uint32(idx), flag)
	if err != nil {
		t.Fatal(err)
	}
	return pre
}

func (e *env) build(s *spend, fee int64) built {
	t := e.t
	t.Helper()
	var outsTotal, fixed int64
	outs := append([]out{}, s.outs...)
	if s.feeFromOut >= 0 {
		outs[s.feeFromOut].sats -= fee
	}
	for _, o := range outs {
		outsTotal += o.sats
	}
	for _, p := range s.pools() {
		fixed += p.st.bsv
	}
	for _, c := range s.others {
		if c != s.payer {
			fixed += c.sats
		}
	}
	switch {
	case s.payer != nil && s.payer.txid == "":
		s.payer.sats = outsTotal - fixed + fee
		if fee > 0 && s.payer.sats < 1 { // a fee-0 draft may hold 0
			t.Fatalf("%s: payer would hold %d sats", s.label, s.payer.sats)
		}
	case s.payer != nil: // minted already: its value fixes the fee
		fee = s.payer.sats + fixed - outsTotal
	}

	tx := transaction.NewTransaction()
	ins := append([]*coin{}, s.others[:s.poolIndex]...)
	var poolIdx []int
	for _, p := range s.pools() {
		poolIdx = append(poolIdx, len(ins))
		ins = append(ins, p.coin())
	}
	ins = append(ins, s.others[s.poolIndex:]...)
	for _, c := range ins {
		addInput(t, tx, c)
	}
	for _, o := range outs {
		addOutput(tx, o)
	}
	var real []byte
	for _, in := range tx.Inputs {
		real = append(real, in.SourceTXID.CloneBytes()...)
		real = binary.LittleEndian.AppendUint32(real, in.SourceTxOutIndex)
	}
	prevouts := s.prevouts
	if prevouts == nil {
		prevouts = real
	}
	sigFlag := s.sigFlag
	if sigFlag == 0 {
		sigFlag = sighash.AllForkID
	}

	var unlockLen int
	for j, p := range s.pools() {
		i := poolIdx[j]
		sc := separatorScriptCode(t, p.lock)
		pre := preimage(t, tx, i, p.st.bsv, sc, sighash.AllForkID) // the txPreimage: Rúnar requires ALL|FORKID
		h1 := sha256.Sum256(preimage(t, tx, i, p.st.bsv, sc, sigFlag))
		digest := sha256.Sum256(h1[:])
		sig, err := s.signer.Sign(digest[:])
		if err != nil {
			t.Fatal(err)
		}
		var u []byte
		if s.method != methodClose { // _codePart: the methods that write a continuation
			u = append(u, pushData(p.code)...)
		}
		u = append(u, pushData(append(sig.Serialize(), byte(sigFlag)))...)
		for _, a := range s.args[1:] {
			u = append(u, a...)
		}
		if s.method != methodClose { // allPrevouts and Rúnar's change params
			u = append(u, pushData(prevouts)...)
			chPkh, chSats := make([]byte, 20), int64(0)
			if s.changePkh != nil {
				chPkh, chSats = s.changePkh, s.changeSats
			}
			u = append(u, pushData(chPkh)...)
			u = append(u, pushNum(chSats)...)
		}
		u = append(u, pushData(pre)...)
		u = append(u, pushNum(int64(s.method))...)
		tx.Inputs[i].UnlockingScript = script.NewFromBytes(u)
		if j == 0 {
			unlockLen = len(u)
		}
	}
	isPool := map[int]bool{}
	for _, i := range poolIdx {
		isPool[i] = true
	}
	for i, c := range ins {
		if !isPool[i] {
			signP2pkh(t, tx, i, c.key)
		}
	}
	return built{tx: tx, outs: outs, fee: fee, unlockLen: unlockLen, poolIdx: poolIdx}
}

// fund measures the spend at fee 0, sets the fee for that size at feeRate
// (+4 bytes of signature-length slack) and mints its unminted coins (the
// payer at exactly its share). Once minted, the payer's value fixes the fee.
func (e *env) fund(s *spend) int64 {
	draft := e.build(s, 0)
	size := int64(len(draft.tx.Bytes())) + 4
	fee := (size*feeRate + 999) / 1000
	e.build(s, fee) // sets the payer's value
	e.mint(s.others)
	return fee
}

// prepare funds the spend and builds it for real.
func (e *env) prepare(s *spend) built {
	return e.build(s, e.fund(s))
}

// interpret runs every pool input of b through the go-sdk interpreter
// (Genesis + Chronicle rules, no policy flags) and returns each one's verdict
// by input index.
func interpret(s *spend, b built) map[int]error {
	v := map[int]error{}
	for j, p := range s.pools() {
		i := b.poolIdx[j]
		v[i] = interpreter.NewEngine().Execute(
			interpreter.WithTx(b.tx, i, &transaction.TransactionOutput{Satoshis: uint64(p.st.bsv), LockingScript: script.NewFromBytes(p.lock)}),
			interpreter.WithForkID(),
			interpreter.WithAfterGenesis(),
			interpreter.WithAfterChronicle(),
		)
	}
	return v
}

func (e *env) logVerdicts(v map[int]error) {
	var idx []int
	for i := range v {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		verdict := "passes"
		if v[i] != nil {
			verdict = "FAILS: " + v[i].Error()
		}
		e.t.Logf("  interpreter: pool input %d %s", i, verdict)
	}
}

// accept broadcasts a spend the node must take, mines it, and re-reads the
// chain: the tx is confirmed, it spends exactly the pool's UTXO at poolIndex,
// that UTXO is gone, and every output (value and script) is as expected.
func (e *env) accept(s *spend) (string, *transaction.Transaction) {
	t := e.t
	t.Helper()
	if s.feeFromOut >= 0 && s.payer != nil {
		t.Fatal("spend has both a payer and feeFromOut")
	}
	b := e.prepare(s)
	size := int64(len(b.tx.Bytes()))
	txid, err := node.SendRawTransaction(b.tx.Hex())
	if err != nil {
		record(txStat{test: t.Name(), label: s.label, size: size, fee: b.fee, unlockLen: b.unlockLen, reason: err.Error()})
		e.logVerdicts(interpret(s, b))
		t.Fatalf("HONEST SPEND REJECTED by the node: %s (%d bytes): %v", s.label, size, err)
	}
	record(txStat{test: t.Name(), label: s.label, size: size, fee: b.fee, unlockLen: b.unlockLen, accepted: true})
	t.Logf("%s: %s, %d bytes (unlock %d), fee %d sats", s.label, txid, size, b.unlockLen, b.fee)
	confirm(t, txid)
	got := fetchTx(t, txid)
	if len(got.Outputs) != len(b.outs) {
		t.Fatalf("%s: %d outputs on chain, want %d", s.label, len(got.Outputs), len(b.outs))
	}
	for i, o := range b.outs {
		g := got.Outputs[i]
		if int64(g.Satoshis) != o.sats || !bytes.Equal(*g.LockingScript, o.script) {
			t.Fatalf("%s: output %d on chain is %d sats %x..., want %d sats %x...", s.label, i, g.Satoshis, firstN(*g.LockingScript, 40), o.sats, firstN(o.script, 40))
		}
	}
	for j, p := range s.pools() {
		in := got.Inputs[b.poolIdx[j]]
		if in.SourceTXID.String() != p.txid || int(in.SourceTxOutIndex) != p.vout {
			t.Fatalf("%s: input %d spends %s:%d, not the pool %s:%d", s.label, b.poolIdx[j], in.SourceTXID, in.SourceTxOutIndex, p.txid, p.vout)
		}
		if u, err := node.TxOut(p.txid, p.vout); err != nil || u != nil {
			t.Fatalf("%s: the spent pool UTXO is still unspent (%v)", s.label, err)
		}
	}
	return txid, got
}

// consensusReject is the node's prefix for a script failure under the
// consensus (mandatory) flags. A "non-mandatory-script-verify-flag" reason
// would be a policy-only refusal and fails the assertion.
const consensusReject = "mandatory-script-verify-flag-failed"

// reject broadcasts a spend the node must refuse. Before broadcasting, the
// go-sdk interpreter must fail exactly the pool inputs at failing (so a
// harness defect cannot pass as a rejection); each failing input is traced
// (the checksigs it ran, the opcodes before its error). The refusal must be
// the node's own (an RPC error) containing want, and every pool the spend
// touches must still be unspent afterwards. Returns the node's reason.
func (e *env) reject(s *spend, want string, failing ...int) string {
	t := e.t
	t.Helper()
	b := e.prepare(s)
	size := int64(len(b.tx.Bytes()))
	v := interpret(s, b)
	var gotFail []int
	for i, err := range v {
		if err != nil {
			gotFail = append(gotFail, i)
		}
	}
	sort.Ints(gotFail)
	sort.Ints(failing)
	if fmt.Sprint(gotFail) != fmt.Sprint(failing) {
		t.Errorf("%s: the interpreter fails pool inputs %v, the case expects %v", s.label, gotFail, failing)
	}
	e.logVerdicts(v)
	for _, i := range gotFail {
		d := trace(s, b, i)
		t.Logf("  trace: input %d: checksigs %v; %v after %v", i, d.checksigs, d.err, d.recent)
	}

	txid, err := node.SendRawTransaction(b.tx.Hex())
	if err == nil {
		record(txStat{test: t.Name(), label: s.label, size: size, fee: b.fee, unlockLen: b.unlockLen, accepted: true, reason: "ACCEPTED (expected a rejection)"})
		t.Fatalf("NODE ACCEPTED a spend the design says it must reject: %s: %s", s.label, txid)
	}
	var rpcErr *node.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("%s: not a node rejection (transport failure?): %v", s.label, err)
	}
	record(txStat{test: t.Name(), label: s.label, size: size, fee: b.fee, unlockLen: b.unlockLen, reason: rpcErr.Message})
	t.Logf("%s: rejected: %s", s.label, rpcErr.Message)
	if !strings.Contains(rpcErr.Message, want) {
		t.Errorf("%s: rejection %q does not contain %q", s.label, rpcErr.Message, want)
	}
	for _, p := range s.pools() {
		if u, err := node.TxOut(p.txid, p.vout); err != nil || u == nil {
			t.Fatalf("%s: the pool UTXO %s:%d is no longer unspent after the rejection (%v)", s.label, p.txid, p.vout, err)
		}
	}
	return rpcErr.Message
}

func firstN(b []byte, n int) []byte { return b[:min(len(b), n)] }

// ---------------------------------------------------------------------------
// The three methods
// ---------------------------------------------------------------------------

// swapPlan is the reference math for one Swap (mirrors the contract; Go's
// integer division truncates toward zero as OP_DIV does, which matters for a
// negative fee rate).
type swapPlan struct {
	amountIn                                  int64
	bsvIn                                     bool
	lpFee, validatorFee, commission, net, out int64
	newBsv, newTokens                         int64
	next                                      *ec.PrivateKey
	nextRaw                                   []byte // the next key's bytes, when not next's
	user                                      *ec.PrivateKey
	userPkh, commissionPkh, tokenChangePkh    []byte
	tokenChange                               int64
	satsChangePkh                             []byte
	satsChange                                int64
}

func planSwap(t *testing.T, p *pool, amountIn int64, bsvIn bool) *swapPlan {
	t.Helper()
	user := newKey(t)
	r := &swapPlan{
		amountIn: amountIn, bsvIn: bsvIn,
		lpFee:          (amountIn*p.p.lpBps + 9999) / 10000,
		validatorFee:   (amountIn*p.p.validatorBps + 9999) / 10000,
		commission:     (amountIn*p.p.commissionBps + 9999) / 10000,
		next:           newKey(t),
		user:           user,
		userPkh:        pkh(user),
		commissionPkh:  pkh(newKey(t)),
		tokenChangePkh: pkh(newKey(t)),
	}
	r.net = amountIn - r.lpFee - r.validatorFee - r.commission
	if bsvIn {
		r.out = r.net * p.st.tokens / (p.st.bsv + r.net)
		r.newBsv, r.newTokens = p.st.bsv+r.net, p.st.tokens-r.out
	} else {
		r.out = r.net * p.st.bsv / (p.st.tokens + r.net)
		r.newBsv, r.newTokens = p.st.bsv-r.out, p.st.tokens+r.net
	}
	if r.net <= 0 || r.out <= 0 {
		t.Fatalf("degenerate swap plan %+v", r)
	}
	return r
}

func (r *swapPlan) nextState(p *pool) poolState {
	return poolState{bsv: r.newBsv, tokens: r.newTokens, lp: p.st.lp, validator: r.next, validatorRaw: r.nextRaw}
}

func (r *swapPlan) nextPub() []byte {
	if r.nextRaw != nil {
		return r.nextRaw
	}
	return r.next.PubKey().Compressed()
}

// validatorPkh is Hash160 of the state's ValidatorPubKey: where the
// validator fee goes.
func validatorPkh(st poolState) []byte { return crypto.Hash160(st.validatorPub()) }

// swapSpend is the honest Swap for plan r: outputs as the contract builds
// them; a sats-in swap's funding input pays amountIn + the token payout's sat
// + the miner fee; a token-in swap brings a 1-sat token-prefixed input (the
// node does not read its amount) plus a funding input for the fee and the
// 1-sat token outputs. The pool is input 0.
func swapSpend(p *pool, r *swapPlan) *spend {
	pp := p.p
	outs := []out{{r.newBsv, poolScript(pp, p.code, r.nextState(p))}}
	if r.bsvIn {
		outs = append(outs, out{1, tokenP2pkh(pp.assetID, r.out, r.userPkh)})
	} else {
		outs = append(outs, out{r.out, p2pkhScript(r.userPkh)})
	}
	lpPkh := pp.lpTokenPkh
	if r.bsvIn {
		lpPkh = pp.lpSatsPkh
	}
	fee := func(amount int64, to []byte) {
		if amount <= 0 { // the contract writes a fee output only when it is > 0
			return
		}
		if r.bsvIn {
			outs = append(outs, out{amount, p2pkhScript(to)})
		} else {
			outs = append(outs, out{1, tokenP2pkh(pp.assetID, amount, to)})
		}
	}
	fee(r.lpFee, lpPkh)
	fee(r.validatorFee, validatorPkh(p.st))
	fee(r.commission, r.commissionPkh)
	if r.tokenChange > 0 {
		outs = append(outs, out{1, tokenP2pkh(pp.assetID, r.tokenChange, r.tokenChangePkh)})
	}
	s := &spend{
		label:  fmt.Sprintf("swap %s %d", map[bool]string{true: "sats-in", false: "token-in"}[r.bsvIn], r.amountIn),
		method: methodSwap, pool: p, feeFromOut: -1,
		args: [][]byte{nil, pushData(r.nextPub()), pushNum(r.amountIn), pushBool(r.bsvIn),
			pushData(r.userPkh), pushData(r.commissionPkh), pushData(r.tokenChangePkh), pushNum(r.tokenChange)},
		signer: p.st.validator,
	}
	if r.satsChange > 0 {
		outs = append(outs, out{r.satsChange, p2pkhScript(r.satsChangePkh)})
		s.changePkh, s.changeSats = r.satsChangePkh, r.satsChange
	}
	s.outs = outs
	if !r.bsvIn {
		s.others = append(s.others, &coin{sats: 1, lock: tokenP2pkh(pp.assetID, r.amountIn+r.tokenChange, pkh(r.user)), key: r.user})
	}
	s.payer = &coin{lock: p2pkhScript(pkh(r.user)), key: r.user}
	s.others = append(s.others, s.payer)
	return s
}

// swap runs an honest Swap, checks the value flow, and moves p to the
// continuation.
func (e *env) swap(p *pool, r *swapPlan) string {
	e.t.Helper()
	return e.swapWith(p, r, swapSpend(p, r))
}

func (e *env) swapWith(p *pool, r *swapPlan, s *spend) string {
	txid, _ := e.swapWithTx(p, r, s)
	return txid
}

func (e *env) swapWithTx(p *pool, r *swapPlan, s *spend) (string, *transaction.Transaction) {
	t := e.t
	t.Helper()
	before := p.st.bsv
	txid, got := e.accept(s)
	after := int64(got.Outputs[0].Satoshis)
	if r.bsvIn {
		if after-before != r.net {
			t.Fatalf("sats-in: the pool gained %d sats, want exactly net %d", after-before, r.net)
		}
	} else {
		if before-after != r.out || int64(got.Outputs[1].Satoshis) != r.out {
			t.Fatalf("token-in: the pool lost %d sats and the payout is %d, want exactly out %d", before-after, got.Outputs[1].Satoshis, r.out)
		}
	}
	p.txid, p.vout, p.st, p.lock = txid, 0, r.nextState(p), s.outs[0].script
	return txid, got
}

// rescindSpend is the validator's honest Rescind: the pool written back with
// the same sats and its key cleared; a funding input of the validator's pays
// the fee, with Rúnar change when change > 0.
func rescindSpend(t *testing.T, p *pool, change int64) *spend {
	r := p.st
	r.rescinded, r.validatorRaw = true, nil
	payKey := newKey(t)
	s := &spend{
		label: "rescind", method: methodRescind, pool: p, feeFromOut: -1,
		outs:   []out{{p.st.bsv, poolScript(p.p, p.code, r)}},
		args:   [][]byte{nil},
		signer: p.st.validator,
	}
	if change > 0 {
		s.changePkh, s.changeSats = pkh(newKey(t)), change
		s.outs = append(s.outs, out{change, p2pkhScript(s.changePkh)})
	}
	s.payer = &coin{lock: p2pkhScript(pkh(payKey)), key: payKey}
	s.others = []*coin{s.payer}
	return s
}

func (e *env) rescind(p *pool, change int64) string {
	s := rescindSpend(e.t, p, change)
	txid, got := e.accept(s)
	if int64(got.Outputs[0].Satoshis) != p.st.bsv {
		e.t.Fatalf("rescind moved the pool's sats: %d -> %d", p.st.bsv, got.Outputs[0].Satoshis)
	}
	p.txid, p.vout, p.lock = txid, 0, s.outs[0].script
	p.st.rescinded, p.st.validatorRaw = true, nil
	return txid
}

// closeSpend is the LP's Close sweeping the pool: its sats (less the miner
// fee) to a fresh P2PKH, its tokens to a token output of 1 sat.
func closeSpend(t *testing.T, p *pool) *spend {
	return &spend{
		label: "close", method: methodClose, pool: p, feeFromOut: 0,
		outs: []out{
			{p.st.bsv - 1, p2pkhScript(pkh(newKey(t)))},
			{1, tokenP2pkh(p.p.assetID, p.st.tokens, pkh(newKey(t)))},
		},
		args:   [][]byte{nil},
		signer: p.st.lp,
	}
}

func (e *env) close(p *pool) string {
	txid, _ := e.accept(closeSpend(e.t, p))
	p.txid = ""
	return txid
}

// atIndex1 moves s's pool to input 1, behind a funding input: the fee payer
// itself when s has one, else a fresh payer.
func atIndex1(t *testing.T, s *spend) *spend {
	if s.payer == nil {
		k := newKey(t)
		s.payer = &coin{lock: p2pkhScript(pkh(k)), key: k}
		s.others = append([]*coin{s.payer}, s.others...)
		s.feeFromOut = -1
	} else {
		rest := []*coin{s.payer}
		for _, c := range s.others {
			if c != s.payer {
				rest = append(rest, c)
			}
		}
		s.others = rest
	}
	s.poolIndex = 1
	return s
}

// ---------------------------------------------------------------------------
// Tracing a failing input (go-sdk interpreter debugger)
// ---------------------------------------------------------------------------

type tracer struct {
	checksigs []checksig
	recent    []string // the last opcodes executed in the locking script
	err       error
	pending   *checksig
}

type checksig struct {
	op     string
	pubKey []byte
	ok     bool
}

func (c checksig) String() string { return fmt.Sprintf("%s(%x..)=%v", c.op, firstN(c.pubKey, 4), c.ok) }

func executing(s *interpreter.State) bool {
	return s.ScriptIdx == 1 && (len(s.CondStack) == 0 || s.CondStack[len(s.CondStack)-1] == 1)
}

func (d *tracer) BeforeExecute(*interpreter.State) {}
func (d *tracer) AfterExecute(*interpreter.State)  {}
func (d *tracer) BeforeStep(*interpreter.State)    {}
func (d *tracer) AfterStep(*interpreter.State)     {}
func (d *tracer) BeforeExecuteOpcode(s *interpreter.State) {
	d.pending = nil
	if !executing(s) {
		return
	}
	op := s.Opcode()
	name := op.Name()
	if len(op.Data) > 0 && len(op.Data) <= 4 {
		name = fmt.Sprintf("%s(%x)", name, op.Data)
	}
	d.recent = append(d.recent, name)
	if len(d.recent) > 10 {
		d.recent = d.recent[1:]
	}
	if n := op.Name(); n == "OP_CHECKSIG" || n == "OP_CHECKSIGVERIFY" {
		d.pending = &checksig{op: n, pubKey: append([]byte{}, s.DataStack[len(s.DataStack)-1]...)}
	}
}
func (d *tracer) AfterExecuteOpcode(s *interpreter.State) {
	if d.pending == nil {
		return
	}
	c := *d.pending
	d.pending = nil
	c.ok = true // a failed OP_CHECKSIGVERIFY errors out instead
	if c.op == "OP_CHECKSIG" {
		top := s.DataStack[len(s.DataStack)-1]
		c.ok = len(top) > 0 && !bytes.Equal(top, []byte{0})
	}
	d.checksigs = append(d.checksigs, c)
}
func (d *tracer) BeforeScriptChange(*interpreter.State)      {}
func (d *tracer) AfterScriptChange(*interpreter.State)       {}
func (d *tracer) AfterSuccess(*interpreter.State)            {}
func (d *tracer) AfterError(_ *interpreter.State, err error) { d.err = err }
func (d *tracer) BeforeStackPush(*interpreter.State, []byte) {}
func (d *tracer) AfterStackPush(*interpreter.State, []byte)  {}
func (d *tracer) BeforeStackPop(*interpreter.State)          {}
func (d *tracer) AfterStackPop(*interpreter.State, []byte)   {}

// trace re-runs pool input idx of b with the tracer attached.
func trace(s *spend, b built, idx int) *tracer {
	var p *pool
	for j, q := range s.pools() {
		if b.poolIdx[j] == idx {
			p = q
		}
	}
	d := &tracer{}
	err := interpreter.NewEngine().Execute(
		interpreter.WithTx(b.tx, idx, &transaction.TransactionOutput{Satoshis: uint64(p.st.bsv), LockingScript: script.NewFromBytes(p.lock)}),
		interpreter.WithForkID(),
		interpreter.WithAfterGenesis(),
		interpreter.WithAfterChronicle(),
		interpreter.WithDebugger(d),
	)
	if d.err == nil {
		d.err = err
	}
	return d
}
