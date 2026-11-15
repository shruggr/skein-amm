//go:build integration

// On-node (bitcoin-sv regtest) tests of pool/pair/PairPool.runar.go and
// pool/pair/Vault.runar.go, compiled from source at test time by whichever
// Rúnar this module resolves (Rúnar main through go.local.mod, the skein-amm pin
// through go.pinned.mod; see README.md).
//
// WHAT THIS PROVES: the compiled pool's and vault's locking and unlocking
// scripts are valid under a real node's consensus rules and its standardness
// policy (as configured by Rúnar's integration/regtest.sh); every spend's
// outputs are the ones the contract computes (byte for byte, re-read from the
// mined chain); each spend consumes the previous pair (pool and vault); and
// the satoshi value flow of each spend (the pool keeps its sats, the vault
// stays 1 sat, a funding input pays the miner). Negative cases show the node
// itself refuses them, and the go-sdk interpreter names which contract input
// fails.
//
// WHAT THIS DOES NOT PROVE: token conservation. The node knows nothing about
// BRC-162: a "token output" is just a script with a
// `0x20 <assetId> <push amount> OP_2DROP` prefix, and the "token inputs" here
// are ordinary 1-sat outputs carrying a token-prefixed P2PKH script, made by a
// plain wallet transaction. Nothing on chain checks a token amount; the
// validator (which signs every Swap) and the overlays do.
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
	"math/big"
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

	"github.com/shruggr/skein-amm/pool/pair/regtest/internal/node"
)

// The pool's public methods in ABI order (checked in compilePair()).
const (
	methodSwap = iota
	methodClose
	methodRescind
)

// feeRate is what every transaction in this suite pays the miner, in sat/kB.
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
// The compiled contracts
// ---------------------------------------------------------------------------

// template is the artifact fields the hand splice reads, parsed from the
// artifact JSON (not runar.RunarArtifact, so the pinned Rúnar's SDK, which may
// predate a field, builds the same suite).
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
			Params   []struct{ Name string }
		}
	}
}

type compiledPair struct {
	sdk   runar.RunarArtifact // the pool's
	tmpl  template            // the pool's
	raw   []byte              // the pool's template script bytes
	vault []byte              // the vault's code: no slots, so the template script itself
	// vaultHash is sha256(vault): the pool's VaultCodeHash constructor arg.
	// Swap and Rescind take the vault code itself as their last declared arg.
	vaultHash []byte
}

var compileOnce struct {
	sync.Once
	c   *compiledPair
	err error
}

// poolSource / vaultSource are ../PairPool.runar.go and ../Vault.runar.go
// unless POOL_SOURCE / VAULT_SOURCE name other files.
func poolSource() string {
	if s := os.Getenv("POOL_SOURCE"); s != "" {
		return s
	}
	return "../PairPool.runar.go"
}

func vaultSource() string {
	if s := os.Getenv("VAULT_SOURCE"); s != "" {
		return s
	}
	return "../Vault.runar.go"
}

func compiled(t *testing.T) *compiledPair {
	t.Helper()
	compileOnce.Do(func() {
		compileOnce.c, compileOnce.err = compilePair()
	})
	if compileOnce.err != nil {
		t.Fatalf("compiling %s / %s: %v", poolSource(), vaultSource(), compileOnce.err)
	}
	return compileOnce.c
}

func artifactJSON(src string) ([]byte, error) {
	art, err := compiler.CompileFromSource(src)
	if err != nil {
		return nil, err
	}
	return compiler.ArtifactToJSON(art)
}

func publicMethods(tm template) string {
	var m []string
	for _, x := range tm.ABI.Methods {
		if x.IsPublic {
			m = append(m, x.Name)
		}
	}
	return strings.Join(m, ",")
}

func compilePair() (*compiledPair, error) {
	js, err := artifactJSON(poolSource())
	if err != nil {
		return nil, err
	}
	c := &compiledPair{}
	if err := json.Unmarshal(js, &c.sdk); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(js, &c.tmpl); err != nil {
		return nil, err
	}
	if c.raw, err = hex.DecodeString(c.tmpl.Script); err != nil {
		return nil, err
	}
	var params []string
	for _, p := range c.tmpl.ABI.Constructor.Params {
		params = append(params, p.Name)
	}
	if got, want := strings.Join(params, ","), "reserveA,reserveB,lpPubKey,validatorPubKey,validatorIdentity,nonce,lpFeeAPkh,lpFeeBPkh,affirmationR,affirmationS,assetIdA,assetIdB,vaultCodeHash,lpFeeBps,validatorFeeBps,commissionBps"; got != want {
		return nil, fmt.Errorf("pool constructor params changed: %s", got)
	}
	if got := publicMethods(c.tmpl); got != "swap,close,rescind" {
		return nil, fmt.Errorf("pool public methods changed: %s", got)
	}
	// The args the unlocking scripts push, in order (build()). An artifact
	// may list the implicit ones too (_changePKH, _changeAmount, txPreimage);
	// then they must sit where build() pushes them.
	for _, m := range c.tmpl.ABI.Methods {
		if !m.IsPublic {
			continue
		}
		var names []string
		for _, p := range m.Params {
			names = append(names, p.Name)
		}
		declared := map[string]string{
			"swap":    "validatorSig,nextValidatorPubKey,amountIn,aIn,userPkh,commissionPkh,tokenChangePkh,tokenChange,allPrevouts,vaultCode",
			"close":   "lpSig,allPrevouts",
			"rescind": "validatorSig,allPrevouts,vaultCode",
		}[m.Name]
		implicit := ",_changePKH,_changeAmount,txPreimage"
		if m.Name == "close" {
			implicit = ",txPreimage"
		}
		if got := strings.Join(names, ","); got != declared && got != declared+implicit {
			return nil, fmt.Errorf("%s's args changed: %s", m.Name, got)
		}
	}
	if !bytes.HasPrefix(c.raw, []byte{0x61, 0xab}) {
		return nil, fmt.Errorf("pool code does not start with OP_NOP OP_CODESEPARATOR: %x", c.raw[:4])
	}

	vjs, err := artifactJSON(vaultSource())
	if err != nil {
		return nil, err
	}
	var vsdk runar.RunarArtifact
	var vt template
	if err := json.Unmarshal(vjs, &vsdk); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(vjs, &vt); err != nil {
		return nil, err
	}
	if got := publicMethods(vt); got != "release" {
		return nil, fmt.Errorf("vault public methods changed: %s", got)
	}
	if len(vt.ConstructorSlots)+len(vt.CodeSepIndexSlots)+len(vt.CodePartLenSlots) != 0 {
		return nil, fmt.Errorf("the vault artifact has slots; the suite assumes its code is the template script")
	}
	if c.vault, err = hex.DecodeString(vt.Script); err != nil {
		return nil, err
	}
	sdkVault, err := hex.DecodeString(runar.NewRunarContract(&vsdk, nil).GetLockingScript())
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(sdkVault, c.vault) {
		return nil, fmt.Errorf("the SDK's vault script (%d bytes) differs from the artifact script (%d bytes)", len(sdkVault), len(c.vault))
	}
	if !bytes.HasPrefix(c.vault, []byte{0x76, 0xab}) {
		return nil, fmt.Errorf("vault code does not start with OP_DUP OP_CODESEPARATOR: %x", c.vault[:4])
	}
	h := sha256.Sum256(c.vault)
	c.vaultHash = h[:]
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

// pushNum is a minimal push of a number (negative ones only as deploy-time
// fee rates).
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

func p2pkhScript(h []byte) []byte {
	return append(append([]byte{0x76, 0xa9, 0x14}, h...), 0x88, 0xac)
}

func pkh(k *ec.PrivateKey) []byte { return k.PubKey().Hash() }

// tokenPrefix is the BRC-162 value-output prefix with no payload.
func tokenPrefix(assetID []byte, amount int64) []byte {
	p := append([]byte{0x20}, assetID...)
	p = append(p, pushNum(amount)...)
	return append(p, 0x6d)
}

func tokenP2pkh(assetID []byte, amount int64, to []byte) []byte {
	return append(tokenPrefix(assetID, amount), p2pkhScript(to)...)
}

// params is what a pair is deployed with and carries for its life.
type params struct {
	lpBps, validatorBps, commissionBps int64
	assetA, assetB, nonce              []byte
	lpFeeAPkh, lpFeeBPkh               []byte
	affR, affS                         []byte // AffirmationR, AffirmationS: 32 bytes each
	identity                           *ec.PrivateKey
}

// pairState is what changes across spends.
type pairState struct {
	sats          int64 // the pool output's sats (the vault is always 1)
	a, b          int64 // ReserveA (pool prefix), ReserveB (vault prefix)
	lp, validator *ec.PrivateKey
	rescinded     bool   // ValidatorPubKey cleared (33 zero bytes)
	validatorRaw  []byte // overrides the validator pubkey bytes (malformed-key cases)
}

func (s pairState) validatorPub() []byte {
	switch {
	case s.validatorRaw != nil:
		return s.validatorRaw
	case s.rescinded:
		return make([]byte, 33)
	}
	return s.validator.PubKey().Compressed()
}

// stateBytes is ReserveA (8 LE) || ReserveB (8 LE) || LpPubKey ||
// ValidatorPubKey || ValidatorIdentity || Nonce || LpFeeAPkh || LpFeeBPkh ||
// AffirmationR || AffirmationS: fixed-width, 251 bytes.
func stateBytes(p params, s pairState) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint64(b, uint64(s.a))
	binary.LittleEndian.PutUint64(b[8:], uint64(s.b))
	b = append(b, s.lp.PubKey().Compressed()...)
	b = append(b, s.validatorPub()...)
	b = append(b, p.identity.PubKey().Compressed()...)
	b = append(b, p.nonce...)
	b = append(b, p.lpFeeAPkh...)
	b = append(b, p.lpFeeBPkh...)
	b = append(b, p.affR...)
	return append(b, p.affS...)
}

// poolScript is output 0: prefix(A, ReserveA) || code || OP_RETURN || state.
func poolScript(p params, code []byte, s pairState) []byte {
	out := append(tokenPrefix(p.assetA, s.a), code...)
	out = append(out, 0x6a)
	return append(out, stateBytes(p, s)...)
}

// vaultScript is output 1: prefix(B, ReserveB) || the vault code.
func vaultScript(p params, vault []byte, s pairState) []byte {
	return append(tokenPrefix(p.assetB, s.b), vault...)
}

// sdkCode deploys through the Rúnar Go SDK (NewRunarContract with the 16
// constructor args + GetLockingScript, which builds code || OP_RETURN ||
// state) and returns the code part, after checking the SDK's state section
// equals stateBytes.
func sdkCode(t *testing.T, c *compiledPair, p params, s pairState) []byte {
	t.Helper()
	ct := runar.NewRunarContract(&c.sdk, []interface{}{
		s.a, s.b,
		hex.EncodeToString(s.lp.PubKey().Compressed()),
		hex.EncodeToString(s.validatorPub()),
		hex.EncodeToString(p.identity.PubKey().Compressed()),
		hex.EncodeToString(p.nonce),
		hex.EncodeToString(p.lpFeeAPkh),
		hex.EncodeToString(p.lpFeeBPkh),
		hex.EncodeToString(p.affR),
		hex.EncodeToString(p.affS),
		hex.EncodeToString(p.assetA),
		hex.EncodeToString(p.assetB),
		hex.EncodeToString(c.vaultHash),
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
// each constructor slot's OP_0 placeholder replaced by the arg's push, each
// codeSepIndex slot (an older Rúnar's) set to OP_1 (0x51). A fixed-width
// state gives the artifact no codePartLenSlots; one that has them cannot be
// built by a splice alone, and the suite refuses it.
func handCode(t *testing.T, c *compiledPair, p params) []byte {
	t.Helper()
	if len(c.tmpl.CodePartLenSlots) != 0 {
		t.Fatalf("the artifact has codePartLenSlots %+v: the deployed code is no longer the template with its constructor args", c.tmpl.CodePartLenSlots)
	}
	type sub struct {
		off int
		b   []byte
	}
	var subs []sub
	for _, s := range c.tmpl.ConstructorSlots {
		var b []byte
		switch s.ParamIndex {
		case 10:
			b = pushData(p.assetA)
		case 11:
			b = pushData(p.assetB)
		case 12:
			b = pushData(c.vaultHash)
		case 13:
			b = pushNum(p.lpBps)
		case 14:
			b = pushNum(p.validatorBps)
		case 15:
			b = pushNum(p.commissionBps)
		default:
			t.Fatalf("constructor slot for param %d: only the readonly args (10..15) are expected in the code", s.ParamIndex)
		}
		subs = append(subs, sub{s.ByteOffset, b})
	}
	for _, s := range c.tmpl.CodeSepIndexSlots {
		if s.CodeSepIndex != 1 {
			t.Fatalf("codeSepIndex slot %+v: the separator is not at byte 1", s)
		}
		subs = append(subs, sub{s.ByteOffset, []byte{0x51}})
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

// scriptCodeAfter is a contract lock's BIP-143 scriptCode: what follows its
// token prefix and the two code bytes ending in OP_CODESEPARATOR (pool:
// 61 ab; vault: 76 ab).
func scriptCodeAfter(t *testing.T, lock []byte, first byte) []byte {
	t.Helper()
	off := 1 + 32
	switch op := lock[off]; {
	case op == 0x00 || (op >= 0x51 && op <= 0x60):
		off++
	default:
		off += 1 + int(op)
	}
	off++ // OP_2DROP
	if lock[off] != first || lock[off+1] != 0xab {
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

// newParams is a pair's deploy parameters with fresh ids and keys. The
// affirmation's r and s are random 32-byte stand-ins (the contract never
// checks them; TestRealAffirmation uses a real signature's).
func newParams(t *testing.T, lpBps, validatorBps, commissionBps int64) params {
	return params{
		lpBps: lpBps, validatorBps: validatorBps, commissionBps: commissionBps,
		assetA:    randBytes(t, 32),
		assetB:    randBytes(t, 32),
		nonce:     randBytes(t, 32),
		lpFeeAPkh: pkh(newKey(t)),
		lpFeeBPkh: pkh(newKey(t)),
		affR:      randBytes(t, 32),
		affS:      randBytes(t, 32),
		identity:  newKey(t),
	}
}

// newPairState is a fresh pair: a 1-sat pool holding 5,000,000 A against a
// vault holding 2,000,000 B (realistic magnitudes: the swap products exceed
// 4-byte numbers).
func newPairState(t *testing.T) pairState {
	return pairState{sats: 1, a: 5_000_000, b: 2_000_000, lp: newKey(t), validator: newKey(t)}
}

// ---------------------------------------------------------------------------
// Report (printed by TestMain)
// ---------------------------------------------------------------------------

type txStat struct {
	test, label             string
	size, fee               int64
	poolUnlock, vaultUnlock int
	accepted                bool
	reason                  string
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

// feeFor is the fee for a transaction of size bytes at feeRate, rounded up.
func feeFor(size int64) int64 { return (size*feeRate + 999) / 1000 }

// pay spends the funder's coin to outs plus change back to the funder, signs,
// broadcasts and returns the txid and the fee paid (sized at feeRate). The
// funder's coin becomes the change.
func (e *env) pay(outs []out) (string, int64) {
	t := e.t
	t.Helper()
	var total int64
	for _, o := range outs {
		total += o.sats
	}
	build := func(fee int64) *transaction.Transaction {
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
		return tx
	}
	fee := feeFor(int64(len(build(0).Bytes())) + 2)
	tx := build(fee)
	txid, err := node.SendRawTransaction(tx.Hex())
	if err != nil {
		t.Fatalf("funder broadcast: %v", err)
	}
	change := int64(tx.Outputs[len(outs)].Satoshis)
	e.f.coin = &coin{txid: txid, vout: len(outs), sats: change, lock: e.f.coin.lock, key: e.f.key}
	return txid, fee
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
	txid, _ := e.pay(outs)
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
	in.SetSourceTxOutput(&transaction.TransactionOutput{Satoshis: uint64(max(c.sats, 0)), LockingScript: script.NewFromBytes(c.lock)})
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
// Pairs and spends
// ---------------------------------------------------------------------------

// pair is a deployed pool (txid:0) and its vault (txid:1).
type pair struct {
	p                   params
	code                []byte // the pool's deployed code: prologue .. last code byte, readonly args in
	vault               []byte // the vault's code
	st                  pairState
	txid                string // "" once closed
	poolLock, vaultLock []byte // as on chain
}

func (pr *pair) poolCoin() *coin {
	return &coin{txid: pr.txid, vout: 0, sats: pr.st.sats, lock: pr.poolLock}
}

func (pr *pair) vaultCoin() *coin { return &coin{txid: pr.txid, vout: 1, sats: 1, lock: pr.vaultLock} }

// deploySDK deploys a pair whose pool code the Rúnar SDK built, after
// checking it equals the independent hand-built-and-patched code.
func (e *env) deploySDK(p params, st pairState) *pair {
	t := e.t
	t.Helper()
	c := compiled(t)
	code := sdkCode(t, c, p, st)
	if hc := handCode(t, c, p); !bytes.Equal(hc, code) {
		t.Fatalf("independently built code (%d bytes) differs from the SDK's (%d bytes)", len(hc), len(code))
	}
	return e.deployCode(p, st, code, "deploy")
}

// deployCode deploys the pair in ONE transaction, output 0 the pool
// (st.sats) and output 1 the vault (1 sat), mines it and checks the chain
// holds exactly those outputs.
func (e *env) deployCode(p params, st pairState, code []byte, label string) *pair {
	t := e.t
	t.Helper()
	c := compiled(t)
	pl, vl := poolScript(p, code, st), vaultScript(p, c.vault, st)
	txid, fee := e.pay([]out{{st.sats, pl}, {1, vl}})
	confirm(t, txid)
	tx := fetchTx(t, txid)
	for i, want := range []out{{st.sats, pl}, {1, vl}} {
		if got := tx.Outputs[i]; int64(got.Satoshis) != want.sats || !bytes.Equal(*got.LockingScript, want.script) {
			t.Fatalf("deployed output %d differs from the expected script", i)
		}
	}
	record(txStat{test: t.Name(), label: label, size: int64(len(tx.Bytes())), fee: fee, accepted: true})
	t.Logf("%s: %s, %d bytes, fee %d; pool script %d bytes (code %d), %d sats; vault script %d bytes", label, txid, len(tx.Bytes()), fee, len(pl), len(code), st.sats, len(vl))
	return &pair{p: p, code: code, vault: c.vault, st: st, txid: txid, poolLock: pl, vaultLock: vl}
}

type inKind int

const (
	kindCoin inKind = iota
	kindPool
	kindVault
)

// input is one transaction input: a plain coin (P2PKH or token-prefixed
// P2PKH, signed with ALL|FORKID), a pair's pool, or a pair's vault.
type input struct {
	kind inKind
	coin *coin // kindCoin
	pr   *pair // kindPool, kindVault

	// The pool's unlocking: [_codePart] (not Close), sig, args, allPrevouts, [vaultCode,
	// _changePKH, _changeAmount] (not Close), txPreimage (always ALL|FORKID,
	// Rúnar's own check), method index.
	method     int
	signer     *ec.PrivateKey
	sigFlag    sighash.Flag // the signer's sighash type (its digest and last byte); 0: ALL|FORKID
	args       [][]byte     // the method's args after the signature, before allPrevouts
	vaultCode  []byte       // the vaultCode arg (Swap, Rescind); nil: the pair's vault code
	changePkh  []byte       // Rúnar change; nil: none (zero PKH, amount 0)
	changeSats int64
	codePart   []byte // nil: the pair's code
	// strayCodePart: a Close that pushes a _codePart first anyway, as Close
	// did while the state had a variable-length field.
	strayCodePart bool
	prevouts      []byte // allPrevouts (pool or vault); nil: the transaction's own

	// The vault's unlocking: allPrevouts, txPreimage under flag (0: ALL|FORKID).
	flag sighash.Flag
}

func poolIn(pr *pair, method int, signer *ec.PrivateKey, args ...[]byte) *input {
	return &input{kind: kindPool, pr: pr, method: method, signer: signer, args: args}
}

func vaultIn(pr *pair) *input { return &input{kind: kindVault, pr: pr} }

func coinIn(c *coin) *input { return &input{kind: kindCoin, coin: c} }

func (in *input) src() *coin {
	switch in.kind {
	case kindPool:
		return in.pr.poolCoin()
	case kindVault:
		return in.pr.vaultCoin()
	}
	return in.coin
}

func (in *input) String() string {
	return [...]string{"coin", "pool", "vault"}[in.kind]
}

// spend is one transaction.
type spend struct {
	label string
	ins   []*input
	outs  []out
	// payer, when set, is one of the coin inputs' coins (unminted): its value
	// is set to exactly what the outputs need beyond every other input, plus
	// the fee. Without a payer the fee is whatever the inputs leave over.
	payer *coin
	// watch are pairs, beyond those spent here, whose UTXOs a rejection must
	// leave unspent.
	watch []*pair
	// trace: reject also traces each failing contract input (the checksigs it
	// ran and the opcodes before its error) into traces, by input index.
	trace  bool
	traces map[int]*tracer
}

type built struct {
	tx                      *transaction.Transaction
	fee                     int64 // inputs - outputs
	poolUnlock, vaultUnlock int   // the first pool / vault input's unlocking size
}

func (s *spend) coins() []*coin {
	var cs []*coin
	for _, in := range s.ins {
		if in.kind == kindCoin {
			cs = append(cs, in.coin)
		}
	}
	return cs
}

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
	var outsTotal, othersFixed int64
	for _, o := range s.outs {
		outsTotal += o.sats
	}
	for _, in := range s.ins {
		if c := in.src(); c != s.payer {
			othersFixed += c.sats
		}
	}
	if s.payer != nil {
		s.payer.sats = outsTotal - othersFixed + fee
		if fee > 0 && s.payer.sats < 1 { // a fee-0 draft may hold 0 or less
			t.Fatalf("%s: payer would hold %d sats", s.label, s.payer.sats)
		}
	}

	tx := transaction.NewTransaction()
	var inTotal int64
	for _, in := range s.ins {
		addInput(t, tx, in.src())
		inTotal += in.src().sats
	}
	for _, o := range s.outs {
		addOutput(tx, o)
	}
	var real []byte
	for _, in := range tx.Inputs {
		real = append(real, in.SourceTXID[:]...)
		real = binary.LittleEndian.AppendUint32(real, in.SourceTxOutIndex)
	}

	b := built{tx: tx, fee: inTotal - outsTotal}
	for i, in := range s.ins {
		prevouts := in.prevouts
		if prevouts == nil {
			prevouts = real
		}
		switch in.kind {
		case kindPool:
			sc := scriptCodeAfter(t, in.pr.poolLock, 0x61)
			pre := preimage(t, tx, i, in.pr.st.sats, sc, sighash.AllForkID)
			// The signer signs the digest of its own sighash type: the
			// script's OP_CHECKSIG hashes by the signature's last byte.
			sigFlag := in.sigFlag
			if sigFlag == 0 {
				sigFlag = sighash.AllForkID
			}
			h1 := sha256.Sum256(preimage(t, tx, i, in.pr.st.sats, sc, sigFlag))
			digest := sha256.Sum256(h1[:])
			sig, err := in.signer.Sign(digest[:])
			if err != nil {
				t.Fatal(err)
			}
			if !sig.Verify(digest[:], in.signer.PubKey()) {
				t.Fatalf("%s: the pool input's signature does not verify over its own sighash type's digest", s.label)
			}
			// _codePart: Swap and Rescind, which write a continuation; Close
			// takes none (strayCodePart pushes one anyway).
			var u []byte
			if in.method != methodClose || in.strayCodePart {
				codePart := in.codePart
				if codePart == nil {
					codePart = in.pr.code
				}
				u = pushData(codePart)
			}
			u = append(u, pushData(append(sig.Serialize(), byte(sigFlag)))...)
			for _, a := range in.args {
				u = append(u, a...)
			}
			u = append(u, pushData(prevouts)...)
			if in.method != methodClose { // Close writes no output: no vaultCode, no change parameters
				vaultCode := in.vaultCode
				if vaultCode == nil {
					vaultCode = in.pr.vault
				}
				u = append(u, pushData(vaultCode)...)
				chPkh, chSats := make([]byte, 20), int64(0)
				if in.changePkh != nil {
					chPkh, chSats = in.changePkh, in.changeSats
				}
				u = append(u, pushData(chPkh)...)
				u = append(u, pushNum(chSats)...)
			}
			u = append(u, pushData(pre)...)
			u = append(u, pushNum(int64(in.method))...)
			tx.Inputs[i].UnlockingScript = script.NewFromBytes(u)
			if b.poolUnlock == 0 {
				b.poolUnlock = len(u)
			}
		case kindVault:
			flag := in.flag
			if flag == 0 {
				flag = sighash.AllForkID
			}
			u := pushData(prevouts)
			u = append(u, pushData(preimage(t, tx, i, 1, scriptCodeAfter(t, in.pr.vaultLock, 0x76), flag))...)
			tx.Inputs[i].UnlockingScript = script.NewFromBytes(u)
			if b.vaultUnlock == 0 {
				b.vaultUnlock = len(u)
			}
		}
	}
	for i, in := range s.ins {
		if in.kind == kindCoin {
			signP2pkh(t, tx, i, in.coin.key)
		}
	}
	return b
}

// prepare measures the spend at fee 0, sets the fee for that size at feeRate
// (+2 bytes of signature-length slack per signature), mints its unminted coins
// and builds it for real.
func (e *env) prepare(s *spend) built {
	draft := e.build(s, 0)
	if s.payer == nil {
		e.mint(s.coins())
		return e.build(s, 0)
	}
	sigs := 0
	for _, in := range s.ins {
		if in.kind != kindVault {
			sigs++
		}
	}
	fee := feeFor(int64(len(draft.tx.Bytes())) + 2*int64(sigs))
	e.build(s, fee) // sets the payer's value
	e.mint(s.coins())
	return e.build(s, fee)
}

// interpret runs every contract input of b through the go-sdk interpreter
// (Genesis + Chronicle rules) and returns each one's verdict by input index.
func interpret(s *spend, b built) map[int]error {
	v := map[int]error{}
	for i, in := range s.ins {
		if in.kind == kindCoin {
			continue
		}
		c := in.src()
		v[i] = interpreter.NewEngine().Execute(
			interpreter.WithTx(b.tx, i, &transaction.TransactionOutput{Satoshis: uint64(c.sats), LockingScript: script.NewFromBytes(c.lock)}),
			interpreter.WithForkID(),
			interpreter.WithAfterGenesis(),
			interpreter.WithAfterChronicle(),
		)
	}
	return v
}

func (e *env) logVerdicts(s *spend, v map[int]error) {
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
		e.t.Logf("  interpreter: input %d (%s) %s", i, s.ins[i], verdict)
	}
}

// accept broadcasts a spend the node must take, mines it, and re-reads the
// chain: the tx is confirmed, every output (value and script) is as
// expected, each contract input spends its pair's pool (txid:0) or vault
// (txid:1), and those UTXOs are gone.
func (e *env) accept(s *spend) (string, *transaction.Transaction, built) {
	t := e.t
	t.Helper()
	b := e.prepare(s)
	size := int64(len(b.tx.Bytes()))
	txid, err := node.SendRawTransaction(b.tx.Hex())
	if err != nil {
		record(txStat{test: t.Name(), label: s.label, size: size, fee: b.fee, poolUnlock: b.poolUnlock, vaultUnlock: b.vaultUnlock, reason: err.Error()})
		e.logVerdicts(s, interpret(s, b))
		t.Fatalf("HONEST SPEND REJECTED by the node: %s (%d bytes): %v", s.label, size, err)
	}
	record(txStat{test: t.Name(), label: s.label, size: size, fee: b.fee, poolUnlock: b.poolUnlock, vaultUnlock: b.vaultUnlock, accepted: true})
	t.Logf("%s: %s, %d bytes (unlock pool %d, vault %d), fee %d sats", s.label, txid, size, b.poolUnlock, b.vaultUnlock, b.fee)
	confirm(t, txid)
	got := fetchTx(t, txid)
	if len(got.Outputs) != len(s.outs) {
		t.Fatalf("%s: %d outputs on chain, want %d", s.label, len(got.Outputs), len(s.outs))
	}
	for i, o := range s.outs {
		g := got.Outputs[i]
		if int64(g.Satoshis) != o.sats || !bytes.Equal(*g.LockingScript, o.script) {
			t.Fatalf("%s: output %d on chain is %d sats %x..., want %d sats %x...", s.label, i, g.Satoshis, firstN(*g.LockingScript, 40), o.sats, firstN(o.script, 40))
		}
	}
	for i, in := range s.ins {
		c := in.src()
		g := got.Inputs[i]
		if g.SourceTXID.String() != c.txid || int(g.SourceTxOutIndex) != c.vout {
			t.Fatalf("%s: input %d spends %s:%d, not the %s %s:%d", s.label, i, g.SourceTXID, g.SourceTxOutIndex, in, c.txid, c.vout)
		}
		if in.kind != kindCoin {
			if u, err := node.TxOut(c.txid, c.vout); err != nil || u != nil {
				t.Fatalf("%s: the spent %s UTXO %s:%d is still unspent (%v)", s.label, in, c.txid, c.vout, err)
			}
		}
	}
	return txid, got, b
}

// consensusReject is the node's prefix for a script failure under the
// consensus (mandatory) flags. A "non-mandatory-script-verify-flag" reason
// would be a policy-only refusal and fails the assertion.
const consensusReject = "mandatory-script-verify-flag-failed"

// reject broadcasts a spend the node must refuse. Before broadcasting, the
// go-sdk interpreter must find exactly the contract inputs failing (by input
// index), so a harness defect cannot pass as a rejection. The refusal must be
// the node's own (an RPC error) and contain want, and the pool and vault of
// every pair the spend touches or watches must still be unspent afterwards.
// Returns the node's reason.
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
		t.Errorf("%s: the interpreter fails contract inputs %v, the case expects %v", s.label, gotFail, failing)
	}
	e.logVerdicts(s, v)
	if s.trace {
		s.traces = map[int]*tracer{}
		for _, i := range gotFail {
			d := trace(s, b, i)
			s.traces[i] = d
			t.Logf("  trace: input %d (%s): checksigs %v; %v after %v", i, s.ins[i], d.checksigs, d.err, d.recent)
		}
	}

	txid, err := node.SendRawTransaction(b.tx.Hex())
	if err == nil {
		record(txStat{test: t.Name(), label: s.label, size: size, fee: b.fee, poolUnlock: b.poolUnlock, vaultUnlock: b.vaultUnlock, accepted: true, reason: "ACCEPTED (expected a rejection)"})
		t.Fatalf("NODE ACCEPTED a spend the design says it must reject: %s: %s", s.label, txid)
	}
	var rpcErr *node.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("%s: not a node rejection (transport failure?): %v", s.label, err)
	}
	record(txStat{test: t.Name(), label: s.label, size: size, fee: b.fee, poolUnlock: b.poolUnlock, vaultUnlock: b.vaultUnlock, reason: rpcErr.Message})
	t.Logf("%s: rejected: %s", s.label, rpcErr.Message)
	if !strings.Contains(rpcErr.Message, want) {
		t.Errorf("%s: rejection %q does not contain %q", s.label, rpcErr.Message, want)
	}
	pairs := append([]*pair{}, s.watch...)
	for _, in := range s.ins {
		if in.kind != kindCoin {
			pairs = append(pairs, in.pr)
		}
	}
	for _, pr := range pairs {
		for vout, what := range []string{"pool", "vault"} {
			if u, err := node.TxOut(pr.txid, vout); err != nil || u == nil {
				t.Fatalf("%s: the %s UTXO %s:%d is no longer unspent after the rejection (%v)", s.label, what, pr.txid, vout, err)
			}
		}
	}
	return rpcErr.Message
}

func firstN(b []byte, n int) []byte { return b[:min(len(b), n)] }

// ---------------------------------------------------------------------------
// The three methods
// ---------------------------------------------------------------------------

// swapPlan is the reference math for one Swap (math/big; mirrors the
// contract's integer arithmetic, independent of it).
type swapPlan struct {
	amountIn                      int64
	aIn                           bool
	lpFee, valFee, com, net, out  int64
	after                         pairState
	next, user                    *ec.PrivateKey
	commissionPkh, tokenChangePkh []byte
	tokenChange                   int64
	satsChangePkh                 []byte
	satsChange                    int64
}

// ceilFee is the contract's fee, (amountIn*bps + 9999) / 10000, the division
// truncating toward zero as OP_DIV does (it matters for a negative rate).
func ceilFee(amountIn, bps int64) int64 {
	x := new(big.Int).Mul(big.NewInt(amountIn), big.NewInt(bps))
	x.Add(x, big.NewInt(9999))
	return x.Quo(x, big.NewInt(10000)).Int64()
}

func planSwap(t *testing.T, pr *pair, amountIn int64, aIn bool) *swapPlan {
	t.Helper()
	r := &swapPlan{
		amountIn: amountIn, aIn: aIn,
		lpFee:          ceilFee(amountIn, pr.p.lpBps),
		valFee:         ceilFee(amountIn, pr.p.validatorBps),
		com:            ceilFee(amountIn, pr.p.commissionBps),
		next:           newKey(t),
		user:           newKey(t),
		commissionPkh:  pkh(newKey(t)),
		tokenChangePkh: pkh(newKey(t)),
	}
	r.net = amountIn - r.lpFee - r.valFee - r.com
	rin, rout := big.NewInt(pr.st.a), big.NewInt(pr.st.b)
	if !aIn {
		rin, rout = rout, rin
	}
	o := new(big.Int).Mul(big.NewInt(r.net), rout)
	o.Div(o, new(big.Int).Add(rin, big.NewInt(r.net)))
	r.out = o.Int64()
	if r.net <= 0 || r.out <= 0 || r.out >= rout.Int64() {
		t.Fatalf("degenerate swap plan %+v", r)
	}
	newIn, newOut := new(big.Int).Add(rin, big.NewInt(r.net)), new(big.Int).Sub(rout, o)
	if new(big.Int).Mul(newIn, newOut).Cmp(new(big.Int).Mul(rin, rout)) < 0 {
		t.Fatalf("model: k decreased")
	}
	r.after = pr.st
	r.after.validator, r.after.validatorRaw, r.after.rescinded = r.next, nil, false
	if aIn {
		r.after.a, r.after.b = newIn.Int64(), newOut.Int64()
	} else {
		r.after.a, r.after.b = newOut.Int64(), newIn.Int64()
	}
	return r
}

// validatorPkh is Hash160 of the state's ValidatorPubKey: where the
// validator fee goes.
func validatorPkh(st pairState) []byte { return crypto.Hash160(st.validatorPub()) }

// swapSpend is the honest Swap for plan r. Inputs: 0 the pool, 1 the vault,
// 2 the user's token input (a 1-sat token-prefixed P2PKH of the input token
// carrying amountIn + tokenChange; the node does not read the amount), 3 the
// user's funding input paying the 1-sat token outputs, any Rúnar sats change
// and the miner. Outputs as the contract builds them.
func swapSpend(pr *pair, r *swapPlan) *spend {
	pp := pr.p
	inAsset, outAsset, lpPkh := pp.assetA, pp.assetB, pp.lpFeeAPkh
	if !r.aIn {
		inAsset, outAsset, lpPkh = pp.assetB, pp.assetA, pp.lpFeeBPkh
	}
	outs := []out{
		{pr.st.sats, poolScript(pp, pr.code, r.after)},
		{1, vaultScript(pp, pr.vault, r.after)},
		{1, tokenP2pkh(outAsset, r.out, pkh(r.user))},
	}
	fee := func(amount int64, to []byte) {
		if amount > 0 {
			outs = append(outs, out{1, tokenP2pkh(inAsset, amount, to)})
		}
	}
	fee(r.lpFee, lpPkh)
	fee(r.valFee, validatorPkh(pr.st))
	fee(r.com, r.commissionPkh)
	fee(r.tokenChange, r.tokenChangePkh)
	pool := poolIn(pr, methodSwap, pr.st.validator,
		pushData(r.next.PubKey().Compressed()), pushNum(r.amountIn), pushBool(r.aIn),
		pushData(pkh(r.user)), pushData(r.commissionPkh), pushData(r.tokenChangePkh), pushNum(r.tokenChange))
	if r.satsChange > 0 {
		outs = append(outs, out{r.satsChange, p2pkhScript(r.satsChangePkh)})
		pool.changePkh, pool.changeSats = r.satsChangePkh, r.satsChange
	}
	tokenIn := &coin{sats: 1, lock: tokenP2pkh(inAsset, r.amountIn+r.tokenChange, pkh(r.user)), key: r.user}
	payer := &coin{lock: p2pkhScript(pkh(r.user)), key: r.user}
	dir := "A-in"
	if !r.aIn {
		dir = "B-in"
	}
	return &spend{
		label: fmt.Sprintf("swap %s %d", dir, r.amountIn),
		ins:   []*input{pool, vaultIn(pr), coinIn(tokenIn), coinIn(payer)},
		outs:  outs,
		payer: payer,
	}
}

// checkPairCarried is the value flow of a spend that writes the pair back:
// the continuation holds exactly the spent pool's sats, the new vault 1 sat,
// and the funding input (the payer) alone pays the miner: every other input
// is matched by an output of the same sats (pool -> pool, vault -> vault, the
// rest -> the 1-sat token outputs and Rúnar change).
func (e *env) checkPairCarried(pr *pair, s *spend, got *transaction.Transaction, b built) {
	t := e.t
	t.Helper()
	if g := int64(got.Outputs[0].Satoshis); g != pr.st.sats {
		t.Fatalf("%s: the pool continuation holds %d sats, the spent pool %d", s.label, g, pr.st.sats)
	}
	if g := got.Outputs[1].Satoshis; g != 1 {
		t.Fatalf("%s: the vault continuation holds %d sats, want 1", s.label, g)
	}
	e.checkPayerPaysMiner(s, got, b)
}

// checkPayerPaysMiner: inputs - outputs on chain equals the fee, and the
// payer's value is exactly that fee plus whatever outputs the other inputs do
// not cover.
func (e *env) checkPayerPaysMiner(s *spend, got *transaction.Transaction, b built) {
	t := e.t
	t.Helper()
	var outs, others int64
	for _, o := range got.Outputs {
		outs += int64(o.Satoshis)
	}
	for _, in := range s.ins {
		if c := in.src(); c != s.payer {
			others += c.sats
		}
	}
	if s.payer == nil {
		t.Fatalf("%s: no funding input", s.label)
	}
	if fee := others + s.payer.sats - outs; fee != b.fee || fee < 1 || s.payer.sats < fee || others > outs {
		t.Fatalf("%s: value flow: inputs %d + payer %d - outputs %d = fee %d (built %d)", s.label, others, s.payer.sats, outs, fee, b.fee)
	}
}

// swap runs an honest Swap, checks the value flow, and moves pr to the
// continuation.
func (e *env) swap(pr *pair, r *swapPlan) string {
	return e.swapWith(pr, r, swapSpend(pr, r))
}

func (e *env) swapWith(pr *pair, r *swapPlan, s *spend) string {
	txid, _ := e.swapWithTx(pr, r, s)
	return txid
}

// swapWithTx is swapWith also returning the mined transaction.
func (e *env) swapWithTx(pr *pair, r *swapPlan, s *spend) (string, *transaction.Transaction) {
	txid, got, b := e.accept(s)
	e.checkPairCarried(pr, s, got, b)
	pr.txid, pr.st, pr.poolLock, pr.vaultLock = txid, r.after, s.outs[0].script, s.outs[1].script
	return txid, got
}

// rescindSpend is the validator's honest Rescind: the pool and vault written
// back unchanged but for ValidatorPubKey cleared; a funding input pays the
// fee, with Rúnar change when change > 0.
func rescindSpend(t *testing.T, pr *pair, change int64) *spend {
	after := pr.st
	after.rescinded = true
	payKey := newKey(t)
	pool := poolIn(pr, methodRescind, pr.st.validator)
	outs := []out{{pr.st.sats, poolScript(pr.p, pr.code, after)}, {1, vaultScript(pr.p, pr.vault, after)}}
	if change > 0 {
		pool.changePkh, pool.changeSats = pkh(newKey(t)), change
		outs = append(outs, out{change, p2pkhScript(pool.changePkh)})
	}
	payer := &coin{lock: p2pkhScript(pkh(payKey)), key: payKey}
	return &spend{label: "rescind", ins: []*input{pool, vaultIn(pr), coinIn(payer)}, outs: outs, payer: payer}
}

func (e *env) rescind(pr *pair, change int64) string {
	s := rescindSpend(e.t, pr, change)
	txid, got, b := e.accept(s)
	e.checkPairCarried(pr, s, got, b)
	pr.txid, pr.poolLock, pr.vaultLock = txid, s.outs[0].script, s.outs[1].script
	pr.st.rescinded = true
	return txid
}

// closeSpend is the LP's Close sweeping both tokens to token-prefixed P2PKH
// outputs of 1 sat each (and, when the pool holds more than 1 sat, the rest
// to a P2PKH); the LP's funding input pays the miner.
func closeSpend(t *testing.T, pr *pair) *spend {
	outs := []out{
		{1, tokenP2pkh(pr.p.assetA, pr.st.a, pkh(newKey(t)))},
		{1, tokenP2pkh(pr.p.assetB, pr.st.b, pkh(newKey(t)))},
	}
	if pr.st.sats > 1 {
		outs = append(outs, out{pr.st.sats - 1, p2pkhScript(pkh(newKey(t)))})
	}
	payer := &coin{lock: p2pkhScript(pkh(pr.st.lp)), key: pr.st.lp}
	return &spend{label: "close", ins: []*input{poolIn(pr, methodClose, pr.st.lp), vaultIn(pr), coinIn(payer)}, outs: outs, payer: payer}
}

func (e *env) close(pr *pair) string {
	s := closeSpend(e.t, pr)
	txid, got, b := e.accept(s)
	e.checkPayerPaysMiner(s, got, b)
	pr.txid = ""
	return txid
}

// ---------------------------------------------------------------------------
// Tracing one contract input (which check refuses a spend)
// ---------------------------------------------------------------------------

// tracer is a go-sdk interpreter Debugger recording, for the locking script,
// every OP_CHECKSIG / OP_CHECKSIGVERIFY actually executed (not in a skipped
// branch) with the public key it checked against and its outcome, and the
// last executed opcodes before an error.
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

// trace runs input idx of b through the interpreter with a tracer.
func trace(s *spend, b built, idx int) *tracer {
	c := s.ins[idx].src()
	d := &tracer{}
	err := interpreter.NewEngine().Execute(
		interpreter.WithTx(b.tx, idx, &transaction.TransactionOutput{Satoshis: uint64(c.sats), LockingScript: script.NewFromBytes(c.lock)}),
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
