package pool

// Behaviour deltas between the pinned Rúnar (go.mod, Sept 2026) and Rúnar
// main (go.work), executed through the real interpreter with the harness in
// pool_test.go. Every test passes on both toolchains: where the two differ the
// test asserts each one's behaviour explicitly, or logs it on a greppable
// `DELTA-RECORD` line (`go test -v -run TestDelta ./pool/ | grep DELTA-RECORD`).
//
// The toolchain is detected at runtime from the artifact: Rúnar main's state
// writer asserts each fixed-width field's size before Swap's and Rescind's
// continuation state (OP_SIZE <n> OP_NUMEQUALVERIFY), the pinned one writes
// the fields raw; and only Rúnar main's artifact JSON has a rawScriptSpans
// key. The two facts must agree.
//
// The state is fixed-width (243 bytes), so on both toolchains the code part is
// authenticated by its remainder: Swap and Rescind take _codePart and require
// the script after it to be exactly OP_RETURN and 243 bytes; Close takes no
// code part and reads the state as the script's last 243 bytes.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/icellan/runar/compilers/go/compiler"
	runar "github.com/icellan/runar/packages/runar-go"
)

// --- the artifact, as JSON (compiles on both toolchains) ---

type deltaSlot struct {
	ParamIndex int `json:"paramIndex"`
	ByteOffset int `json:"byteOffset"`
}

type deltaParam struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type deltaArtifact struct {
	Script             string       `json:"script"`
	ASM                string       `json:"asm"`
	ConstructorSlots   []deltaSlot  `json:"constructorSlots"`
	CodeSeparatorIndex *int         `json:"codeSeparatorIndex"`
	StateFields        []deltaParam `json:"stateFields"`
	ABI                struct {
		Methods []struct {
			Name         string       `json:"name"`
			Params       []deltaParam `json:"params"`
			IsPublic     bool         `json:"isPublic"`
			UsesCodePart bool         `json:"usesCodePart"`
		} `json:"methods"`
	} `json:"abi"`

	main     bool            // Rúnar main (see the file comment)
	keys     map[string]bool // the artifact JSON's top-level keys
	template []byte
	sa       runar.RunarArtifact
}

var (
	deltaArtOnce sync.Once
	deltaArtVal  *deltaArtifact
	deltaArtErr  error
)

// deltaWidthAsserts counts the state writer's field-size asserts: OP_SIZE
// <33|32|20> OP_NUMEQUALVERIFY.
func deltaWidthAsserts(asm string) map[int]int {
	n := map[int]int{}
	for w, hx := range map[int]string{33: "21", 32: "20", 20: "14"} {
		n[w] = strings.Count(asm, "OP_SIZE <"+hx+"> OP_NUMEQUALVERIFY")
	}
	return n
}

func deltaArt(t *testing.T) *deltaArtifact {
	t.Helper()
	deltaArtOnce.Do(func() {
		art, err := compiler.CompileFromSource("Pool.runar.go")
		if err != nil {
			deltaArtErr = err
			return
		}
		js, err := compiler.ArtifactToJSON(art)
		if err != nil {
			deltaArtErr = err
			return
		}
		a := &deltaArtifact{keys: map[string]bool{}}
		if deltaArtErr = json.Unmarshal(js, a); deltaArtErr != nil {
			return
		}
		var keys map[string]json.RawMessage
		if deltaArtErr = json.Unmarshal(js, &keys); deltaArtErr != nil {
			return
		}
		for k := range keys {
			a.keys[k] = true
		}
		w := deltaWidthAsserts(a.ASM)
		a.main = w[33]+w[32]+w[20] > 0
		if a.main != a.keys["rawScriptSpans"] {
			deltaArtErr = fmt.Errorf("toolchain detection disagrees: width asserts %v, rawScriptSpans key %v", w, a.keys["rawScriptSpans"])
			return
		}
		if a.template, deltaArtErr = hex.DecodeString(a.Script); deltaArtErr != nil {
			return
		}
		deltaArtErr = json.Unmarshal(js, &a.sa)
		deltaArtVal = a
	})
	if deltaArtErr != nil {
		t.Fatal(deltaArtErr)
	}
	return deltaArtVal
}

// runarMain reports whether the contract was compiled by Rúnar main (go.work)
// rather than the pinned Rúnar (GOWORK=off).
func runarMain(t *testing.T) bool { return deltaArt(t).main }

func deltaToolchain(t *testing.T) string {
	if runarMain(t) {
		return "Rúnar main"
	}
	return "pinned"
}

// deltaFieldSize is a fixed-width state field's size by its artifact type.
func deltaFieldSize(t *testing.T, typ string) int {
	switch typ {
	case "bigint":
		return 8
	case "PubKey":
		return 33
	case "Sha256":
		return 32
	case "Addr":
		return 20
	}
	t.Fatalf("state field type %q is not fixed-width", typ)
	return 0
}

// deltaStateLen is the state's length, from the artifact's state fields.
func deltaStateLen(t *testing.T) int {
	n := 0
	for _, f := range deltaArt(t).StateFields {
		n += deltaFieldSize(t, f.Type)
	}
	return n
}

// --- expectations ---

type deltaWant int

const (
	deltaPass   deltaWant = iota // must pass
	deltaFail                    // must fail
	deltaRecord                  // log the outcome only
)

// deltaCheck asserts err against main's or pinned's expectation, by
// toolchain, and logs a DELTA-RECORD line either way.
func deltaCheck(t *testing.T, name string, err error, main, pinned deltaWant) {
	t.Helper()
	want := pinned
	if runarMain(t) {
		want = main
	}
	outcome := "pass"
	if err != nil {
		outcome = "fail (" + err.Error() + ")"
	}
	t.Logf("DELTA-RECORD %s | %s | %s", deltaToolchain(t), name, outcome)
	switch want {
	case deltaPass:
		mustPass(t, err)
	case deltaFail:
		mustFail(t, err)
	}
}

// --- state and locks built through the SDK ---

type deltaState struct {
	tokens                                    int64
	lp, val, id, nonce, sats, tok, affR, affS []byte
}

// deltaStateOf is the state of a harness pool.
func deltaStateOf(p pool) deltaState {
	val := make([]byte, 33)
	if !p.rescinded {
		val = p.validator.PubKey().Compressed()
	}
	sats, tok := p.feePkhs()
	return deltaState{
		tokens: p.tokens, lp: p.lp.PubKey().Compressed(), val: val,
		id: validatorIdentity.PubKey().Compressed(), nonce: nonce, sats: sats, tok: tok,
		affR: affirmationR, affS: affirmationS,
	}
}

// raw hand-concatenates the state, every field at whatever width it has.
func (s deltaState) raw() []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(s.tokens))
	for _, f := range [][]byte{s.lp, s.val, s.id, s.nonce, s.sats, s.tok, s.affR, s.affS} {
		b = append(b, f...)
	}
	return b
}

// deltaSDKScript is the SDK's locking script (code || OP_RETURN || state) for
// this state, asset id and fees; an SDK panic comes back as an error.
func deltaSDKScript(t *testing.T, s deltaState, asset []byte, fees [3]int64) (out []byte, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("SDK panic: %v", r)
		}
	}()
	sa := deltaArt(t).sa
	h := hex.EncodeToString
	c := runar.NewRunarContract(&sa, []interface{}{
		s.tokens, h(s.lp), h(s.val), h(s.id), h(s.nonce), h(s.sats), h(s.tok), h(s.affR), h(s.affS),
		h(asset), fees[0], fees[1], fees[2],
	})
	out, err = hex.DecodeString(c.GetLockingScript())
	return out, err
}

func (f *fixture) deltaFees() [3]int64 { return [3]int64{f.lpFeeBps, f.validatorBps, f.commissionBps} }

// deltaTail is OP_RETURN || state as the SDK frames it for f's pool.
func (f *fixture) deltaTail(t *testing.T, s deltaState) []byte {
	t.Helper()
	full, err := deltaSDKScript(t, s, assetID, f.deltaFees())
	if err != nil {
		t.Fatal(err)
	}
	code := deltaCode(t, assetID, f.deltaFees())
	if !bytes.HasPrefix(full, code) || full[len(code)] != 0x6a {
		t.Fatalf("SDK script does not start with its own code part (%d vs %d bytes)", len(full), len(code))
	}
	return full[len(code):]
}

// deltaLock is the pool output's full locking script for s (prefix, code, tail).
func (f *fixture) deltaLock(t *testing.T, s deltaState) []byte {
	t.Helper()
	return append(append(tokenPrefix(s.tokens), f.code...), f.deltaTail(t, s)...)
}

// deltaCode is the SDK's code part for a pool of this asset id and fees.
func deltaCode(t *testing.T, asset []byte, fees [3]int64) []byte {
	t.Helper()
	p := pool{tokens: 1000, lp: key(t, 1), validator: key(t, 2)}
	s := deltaStateOf(p)
	full, err := deltaSDKScript(t, s, asset, fees)
	if err != nil {
		t.Fatal(err)
	}
	tail := append([]byte{0x6a}, s.raw()...)
	if !bytes.HasSuffix(full, tail) {
		t.Fatal("SDK tail is not OP_RETURN || state")
	}
	return full[:len(full)-len(tail)]
}

// deltaRunCode runs c with _codePart replaced by codePart; p.lock (default
// f.poolScript(p)) is the real spent output either way.
func (f *fixture) deltaRunCode(t *testing.T, p pool, c call, codePart []byte) error {
	t.Helper()
	if p.lock == nil {
		p.lock = f.poolScript(p)
	}
	g := *f
	g.code = codePart
	return g.run(t, p, c)
}

type deltaCall struct {
	name string
	c    call
}

// deltaCalls are the three methods on p, each with output 0 the continuation
// the SDK frames for the state with affirmation (r, s). Swap is bsv in,
// 20,000 sats.
func (f *fixture) deltaCalls(t *testing.T, p pool, r, s []byte) []deltaCall {
	t.Helper()
	with := func(q pool) deltaState {
		st := deltaStateOf(q)
		st.affR, st.affS = r, s
		return st
	}
	res := f.expectSwap(p, 20_000, true)
	sw := f.swapCall(p, 20_000, true, res)
	sw.outs[0].script = f.deltaLock(t, with(res.pool(p)))
	rs := f.rescindCall(p)
	cleared := p
	cleared.rescinded = true
	rs.outs[0].script = f.deltaLock(t, with(cleared))
	return []deltaCall{{"Swap", sw}, {"Close", f.closeCall(p)}, {"Rescind", rs}}
}

// deltaKeyWithByte is a deterministic private key whose compressed public key
// has byte v at index i.
func deltaKeyWithByte(t *testing.T, i int, v byte) *ec.PrivateKey {
	t.Helper()
	for n := 0; n < 1<<16; n++ {
		seed := sha256.Sum256([]byte(fmt.Sprintf("delta-key-%d", n)))
		k, _ := ec.PrivateKeyFromBytes(seed[:])
		if k.PubKey().Compressed()[i] == v {
			return k
		}
	}
	t.Fatalf("no key with %#x at %d", v, i)
	return nil
}

// --- the hand-spliced template ---

// deltaHandSplice instantiates the template the way an off-chain builder that
// only knows constructorSlots does (the pages' vendored runar-sdk,
// web/ui/src/vendor): each constructor slot's OP_0 replaced by the arg's push,
// nothing else touched.
func deltaHandSplice(t *testing.T, asset []byte, fees [3]int64) []byte {
	t.Helper()
	a := deltaArt(t)
	type sub struct {
		off int
		enc []byte
	}
	var subs []sub
	for _, s := range a.ConstructorSlots {
		if a.template[s.ByteOffset] != 0x00 {
			t.Fatalf("constructor slot %+v is not an OP_0 placeholder", s)
		}
		var enc []byte
		switch s.ParamIndex {
		case 9:
			enc = pushData(asset)
		case 10, 11, 12:
			enc = pushNum(fees[s.ParamIndex-10])
		default:
			t.Fatalf("unexpected constructor slot param %d", s.ParamIndex)
		}
		subs = append(subs, sub{s.ByteOffset, enc})
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].off > subs[j].off })
	code := append([]byte{}, a.template...)
	for _, s := range subs {
		code = append(append(append([]byte{}, code[:s.off]...), s.enc...), code[s.off+1:]...)
	}
	return code
}

// deltaTemplateMatches mirrors src/pool.zig `parse`'s code match: the built
// code equals the template everywhere outside the constructor slots. A Go
// mirror of the Zig, not the Zig.
func deltaTemplateMatches(t *testing.T, code []byte) bool {
	t.Helper()
	a := deltaArt(t)
	tpl := a.template
	tpos, pos := 0, 0
	for _, s := range a.ConstructorSlots { // ascending by offset
		seg := tpl[tpos:s.ByteOffset]
		if len(code)-pos < len(seg) || !bytes.Equal(code[pos:pos+len(seg)], seg) {
			return false
		}
		pos += len(seg)
		tpos = s.ByteOffset + 1
		op := code[pos]
		switch {
		case op >= 0x01 && op <= 0x4b:
			pos += 1 + int(op)
		default:
			pos++ // OP_0, OP_1..OP_16
		}
	}
	return bytes.Equal(code[pos:], tpl[tpos:])
}

var deltaFeeRows = []struct {
	name string
	fees [3]int64
}{
	{"bps 0/0/0", [3]int64{0, 0, 0}},
	{"bps 1/1/1", [3]int64{1, 1, 1}},
	{"bps 16/16/16", [3]int64{16, 16, 16}},
	{"bps 17/17/17", [3]int64{17, 17, 17}},
	{"bps 30/5/10", [3]int64{30, 5, 10}},
	{"bps 17/16/1 (mixed push widths)", [3]int64{17, 16, 1}},
	{"bps 10000/0/0", [3]int64{10000, 0, 0}},
}

// --- the toolchains' shape ---

// deltaLengthChecks finds code-part length checks in a template: 04 <LE len>
// OP_BIN2NUM then OP_NUMEQUAL OP_VERIFY or OP_GREATERTHANOREQUAL OP_VERIFY,
// with len near the template's length (what a variable-length state made
// Rúnar emit).
func deltaLengthChecks(tpl []byte) int {
	n := 0
	for i := 0; i+8 <= len(tpl); i++ {
		if tpl[i] != 0x04 || tpl[i+5] != 0x81 || tpl[i+7] != 0x69 || (tpl[i+6] != 0x9c && tpl[i+6] != 0xa2) {
			continue
		}
		if v := int(binary.LittleEndian.Uint32(tpl[i+1:])); v >= len(tpl)/2 && v <= 2*len(tpl) {
			n++
		}
	}
	return n
}

// The template sizes and the OP_PUSH_TX span sizes are those of the two
// compilers these tests were written against: the pin in go.mod, and Rúnar
// main at 6bd5e29f4. A later Rúnar, or an edit to the contract, moves them:
// update them here. Everything else is derived from the artifact.
func TestDeltaToolchainShape(t *testing.T) {
	a := deltaArt(t)
	t.Logf("DELTA-RECORD %s | template %d bytes, top-level keys %v", deltaToolchain(t), len(a.template), a.keys)

	t.Run("template size", func(t *testing.T) {
		want := map[bool]int{true: 3690, false: 3650}[a.main]
		if len(a.template) != want {
			t.Errorf("%s template %d bytes, expected %d", deltaToolchain(t), len(a.template), want)
		}
	})
	t.Run("prologue is OP_NOP OP_CODESEPARATOR and the separator index is 1", func(t *testing.T) {
		if !bytes.HasPrefix(a.template, []byte{0x61, 0xab}) {
			t.Fatalf("template starts %x", a.template[:2])
		}
		if a.CodeSeparatorIndex == nil || *a.CodeSeparatorIndex != 1 {
			t.Fatalf("codeSeparatorIndex %v", a.CodeSeparatorIndex)
		}
	})
	t.Run("the state is fixed-width and is the harness's", func(t *testing.T) {
		p := pool{tokens: 1, lp: key(t, 1), validator: key(t, 2)}
		if n := deltaStateLen(t); n != stateLen || len(p.state()) != n {
			t.Fatalf("state %d bytes by the artifact, %d by the harness, offsets assume %d", n, len(p.state()), stateLen)
		}
	})
	t.Run("no slots to patch at deploy and no code-part length check", func(t *testing.T) {
		for _, k := range []string{"codePartLenSlots", "codeSepIndexSlots"} {
			if a.keys[k] {
				t.Errorf("artifact has %s", k)
			}
		}
		if n := deltaLengthChecks(a.template); n != 0 {
			t.Errorf("%d code-part length checks", n)
		}
	})
	t.Run("Swap and Rescind take a code part, pinned by its remainder; Close takes none", func(t *testing.T) {
		var uses []string
		for _, m := range a.ABI.Methods {
			if !m.IsPublic {
				continue
			}
			var names []string
			for _, p := range m.Params {
				names = append(names, p.Name)
			}
			t.Logf("%s(%s) usesCodePart=%v", m.Name, strings.Join(names, ", "), m.UsesCodePart)
			if m.UsesCodePart {
				uses = append(uses, m.Name)
				if tail := strings.Join(names[len(names)-4:], ","); tail != "allPrevouts,_changePKH,_changeAmount,txPreimage" {
					t.Errorf("%s ends %s", m.Name, tail)
				}
			} else if strings.Join(names, ",") != "lpSig,txPreimage" {
				t.Errorf("%s takes %v", m.Name, names)
			}
		}
		if strings.Join(uses, ",") != "swap,rescind" {
			t.Fatalf("methods with a code part: %v", uses)
		}
		// The remainder after the code part: OP_RETURN and the state.
		rest := 1 + deltaStateLen(t)
		pin := fmt.Sprintf("OP_SIZE <%x> OP_NUMEQUALVERIFY OP_1 OP_SPLIT OP_DROP <6a> OP_EQUALVERIFY <61ab> OP_SWAP OP_CAT OP_EQUALVERIFY", scriptNum(int64(rest)))
		if n := strings.Count(a.ASM, pin); n != len(uses) {
			t.Fatalf("%d remainder pins (%d bytes), want %d", n, rest, len(uses))
		}
	})
	t.Run("OP_PUSH_TX raw span, one per public method", func(t *testing.T) {
		want := map[bool]string{true: "<raw 422 bytes>", false: "<raw 428 bytes>"}[a.main]
		if n := strings.Count(a.ASM, want); n != 3 {
			t.Fatalf("%q appears %d times", want, n)
		}
	})
	t.Run("state writer's field-size asserts", func(t *testing.T) {
		// Rúnar main: each method writing a continuation asserts every
		// fixed-width byte field's size (TokenReserve is written by
		// Num2Bin(_, 8)). Pinned: none.
		want := map[int]int{}
		for _, f := range a.StateFields {
			if w := deltaFieldSize(t, f.Type); f.Type != "bigint" && a.main {
				want[w] += 2 // Swap and Rescind
			}
		}
		got := deltaWidthAsserts(a.ASM)
		for _, w := range []int{33, 32, 20} {
			if got[w] != want[w] {
				t.Errorf("%d-byte asserts: %d, want %d", w, got[w], want[w])
			}
		}
		t.Logf("DELTA-RECORD %s | field-size asserts %v", deltaToolchain(t), got)
	})
}

// With no length to patch, a builder that only splices the constructor args
// produces the SDK's code byte for byte, on both toolchains, whatever the fee
// pushes' widths; the overlay's template match recognises it, and it spends.
func TestDeltaHandSplicedTemplateIsTheSDKCode(t *testing.T) {
	for _, row := range deltaFeeRows {
		row := row
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t, row.fees[0], row.fees[1], row.fees[2])
			hand := deltaHandSplice(t, assetID, row.fees)
			if !bytes.Equal(hand, f.code) {
				t.Fatalf("hand splice (%d bytes) differs from the SDK code (%d bytes)", len(hand), len(f.code))
			}
			if !deltaTemplateMatches(t, f.code) {
				t.Fatal("zig parse mirror: the SDK code does not match the template")
			}
			other := bytes.Repeat([]byte{0xb6}, 32)
			if !bytes.Equal(deltaHandSplice(t, other, row.fees), deltaCode(t, other, row.fees)) {
				t.Fatal("hand splice differs from the SDK code for another asset id")
			}
			g := *f
			g.code = hand
			p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
			for _, m := range g.deltaCalls(t, p, affirmationR, affirmationS) {
				err := g.run(t, p, m.c)
				if m.name == "Swap" && row.fees[0] == 10000 {
					mustFail(t, err) // a 100% fee leaves net 0
					continue
				}
				deltaCheck(t, row.name+" hand-spliced "+m.name, err, deltaPass, deltaPass)
			}
		})
	}
}

// --- the code part ---

// _codePart tampering on an SDK-built pool, Swap and Rescind (Close takes no
// code part). Every cut the authentication's byte checks could accept —
// _codePart = script[:k] with script[k] == 0x6a, in the code or the state — is
// refused on both toolchains: the remainder must be exactly 1 + 243 bytes.
func TestDeltaCodePartWitnessTampering(t *testing.T) {
	a := deltaArt(t)
	f := newFixture(t, 30, 5, 10)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	p.lock = f.deltaLock(t, deltaStateOf(p))
	var calls []deltaCall
	for _, m := range f.deltaCalls(t, p, affirmationR, affirmationS) {
		if m.name != "Close" {
			calls = append(calls, m)
		}
	}
	t.Run("control: the honest code part passes", func(t *testing.T) {
		for _, m := range calls {
			mustPass(t, f.deltaRunCode(t, p, m.c, f.code))
		}
	})
	other := bytes.Repeat([]byte{0xb6}, 32)
	for _, tc := range []struct {
		name string
		cp   []byte
	}{
		{"truncated by 1 byte", f.code[:len(f.code)-1]},
		{"extended by 0x00", append(append([]byte{}, f.code...), 0x00)},
		{"extended by 0x6a", append(append([]byte{}, f.code...), 0x6a)},
		{"first byte flipped", deltaFlip(f.code, 0)},
		{"middle byte flipped", deltaFlip(f.code, len(f.code)/2)},
		{"last byte flipped", deltaFlip(f.code, len(f.code)-1)},
		{"empty", []byte{}},
		{"the template itself", a.template},
		{"another pool's code (other AssetId)", deltaCode(t, other, f.deltaFees())},
		{"another pool's code (other fees)", deltaCode(t, assetID, [3]int64{31, 5, 10})},
	} {
		for _, m := range calls {
			tc, m := tc, m
			t.Run(tc.name+": "+m.name, func(t *testing.T) {
				deltaCheck(t, "codePart "+tc.name+" "+m.name, f.deltaRunCode(t, p, m.c, tc.cp), deltaFail, deltaFail)
			})
		}
	}
	body := p.lock[len(tokenPrefix(p.tokens)):]
	signers := map[string]*ec.PrivateKey{"validator": p.validator, "identity": validatorIdentity}
	cuts := 0
	for k := 1; k < len(body); k++ {
		if body[k] != 0x6a || k == len(f.code) {
			continue
		}
		cuts++
		where := fmt.Sprintf("code byte %d", k)
		if k > len(f.code) {
			where = fmt.Sprintf("state byte %d", k-len(f.code)-1)
		}
		for _, m := range calls {
			for sn, sk := range signers {
				c := m.c
				c.signers = []*ec.PrivateKey{sk}
				if err := f.deltaRunCode(t, p, c, body[:k]); err == nil {
					t.Errorf("DELTA-RECORD %s | ACCEPTED: %s with _codePart cut at %d (%s), signed by %s", deltaToolchain(t), m.name, k, where, sn)
				}
			}
		}
	}
	t.Logf("DELTA-RECORD %s | %d cuts at a 0x6a byte, Swap and Rescind, validator and identity signers: all refused", deltaToolchain(t), cuts)
	t.Run("Close: a code part pushed below its argument is ignored", func(t *testing.T) {
		// Close's only argument is the LP's signature: an extra push below it
		// is never read, so no code part can change what Close checks.
		for _, cp := range [][]byte{f.code, body[:len(f.code)+1+offVal]} {
			mustPass(t, f.advRun(t, p, f.closeCall(p), advOpts{leading: [][]byte{pushData(cp)}}))
			mustFail(t, f.advRun(t, p, advSetSigner(f.closeCall(p), p.validator), advOpts{leading: [][]byte{pushData(cp)}}))
		}
	})
}

func deltaFlip(b []byte, i int) []byte {
	c := append([]byte{}, b...)
	c[i] ^= 0x01
	return c
}

// The code part run into the state: a cut at a 0x6a state byte would move
// every field j+1 bytes for a cut at state byte j (with a variable-length
// state the pinned Rúnar let Close take such a cut: an LpPubKey whose byte 24
// was 0x6a let the ValidatorPubKey holder Close the LP's pool; a
// ValidatorPubKey whose byte 24 was 0x6a let the ValidatorIdentity holder).
// Now Close takes no code part and reads the state from the script's end, and
// Swap and Rescind pin the remainder's length: every such spend is refused on
// both toolchains. Pools with 0x6a at bytes 16 and 24 of either key, every
// 0x6a state byte cut.
func TestDeltaCodePartRunIntoState(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	type pl struct {
		name string
		p    pool
	}
	pools := []pl{{"fresh pool", pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}}}
	for _, b := range []int{16, 24} {
		k := deltaKeyWithByte(t, b, 0x6a)
		pools = append(pools,
			pl{fmt.Sprintf("LpPubKey byte %d is 0x6a", b), pool{bsv: 1_000_000, tokens: 5_000_000, lp: k, validator: key(t, 20)}},
			pl{fmt.Sprintf("ValidatorPubKey byte %d is 0x6a", b), pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: k}})
	}
	runs := 0
	for _, x := range pools {
		x := x
		t.Run(x.name, func(t *testing.T) {
			p := x.p
			p.lock = f.deltaLock(t, deltaStateOf(p))
			body := p.lock[len(tokenPrefix(p.tokens)):]
			// Close by whoever holds the key one field along, the honest LP control.
			mustPass(t, f.run(t, p, f.closeCall(p)))
			for _, thief := range []*ec.PrivateKey{p.validator, validatorIdentity} {
				steal := advCloseTo(p, pkh(thief))
				steal.signers = []*ec.PrivateKey{thief}
				deltaCheck(t, x.name+": Close by a key one field along", f.run(t, p, steal), deltaFail, deltaFail)
			}
			st := p.state()
			for j := range st {
				if st[j] != 0x6a {
					continue
				}
				k := len(f.code) + 1 + j
				for _, m := range f.deltaCalls(t, p, affirmationR, affirmationS) {
					if m.name == "Close" {
						continue
					}
					for _, sk := range []*ec.PrivateKey{p.validator, validatorIdentity} {
						runs++
						c := m.c
						c.signers = []*ec.PrivateKey{sk}
						deltaCheck(t, fmt.Sprintf("%s: %s with _codePart cut at state byte %d", x.name, m.name, j), f.deltaRunCode(t, p, c, body[:k]), deltaFail, deltaFail)
					}
				}
			}
		})
	}
	t.Logf("DELTA-RECORD %s | %d pools, %d Swap/Rescind runs with a cut in the state: all refused", deltaToolchain(t), len(pools), runs)
}

// The attack the variable-length state allowed on the pinned Rúnar: the
// validator signs one ordinary Swap naming a next key with 0x6a at byte 24,
// then Closes with ValidatorIdentity and a code part run 66 bytes into the
// state. The Swap is honest on both toolchains; the Close is refused on both
// (no code part; the state is read from the end), and so is a Rescind with
// that cut.
func TestDeltaValidatorGrindsNextKeyThenCannotStealViaClose(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	r := f.expectSwap(p, 20_000, true)
	r.nextValidator = deltaKeyWithByte(t, 24, 0x6a)
	mustPass(t, f.run(t, p, f.swapCall(p, 20_000, true, r)))

	after := r.pool(p)
	after.lock = f.poolScript(after)
	body := after.lock[len(tokenPrefix(after.tokens)):]
	k := len(f.code) + 1 + offVal + 24
	if body[k] != 0x6a {
		t.Fatalf("byte %d is %#x", k, body[k])
	}
	steal := advCloseTo(after, pkh(validatorIdentity))
	steal.signers = []*ec.PrivateKey{validatorIdentity}
	mustPass(t, f.run(t, after, f.closeCall(after)))
	deltaCheck(t, "identity Close of the pool after a Swap to a ground next key", f.run(t, after, steal), deltaFail, deltaFail)
	deltaCheck(t, "identity Close with the cut pushed as an extra argument", f.advRun(t, after, steal, advOpts{leading: [][]byte{pushData(body[:k])}}), deltaFail, deltaFail)
	rs := advSetSigner(f.rescindCall(after), validatorIdentity)
	deltaCheck(t, "identity Rescind with _codePart cut at state byte 65", f.deltaRunCode(t, after, rs, body[:k]), deltaFail, deltaFail)
}

// Truncations inside the code. With an AssetId of 32 0x6a bytes every
// AssetId byte is a cut the authentication's byte checks accept; the
// remainder's length refuses every one, on both toolchains. Rescind and Swap.
func TestDeltaCodePartTruncatedInsideCode(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	asset := bytes.Repeat([]byte{0x6a}, 32)
	g := *f
	g.code = deltaCode(t, asset, f.deltaFees())
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	lock := func(q pool) []byte {
		pre := append(append([]byte{0x20}, asset...), append(pushNum(q.tokens), 0x6d)...)
		return append(append(append(pre, g.code...), 0x6a), q.state()...)
	}
	p.lock = lock(p)
	rs := g.rescindCall(p)
	cleared := p
	cleared.rescinded = true
	rs.outs[0].script = lock(cleared)
	r := g.expectSwap(p, 20_000, true)
	sw := g.swapCall(p, 20_000, true, r)
	sw.outs[0].script = lock(r.pool(p))
	// The honest spends' fee outputs carry assetID's prefix; rebuild them for
	// this asset id.
	for i := 1; i < len(sw.outs); i++ {
		if s := sw.outs[i].script; s[0] == 0x20 {
			sw.outs[i].script = append(append([]byte{0x20}, asset...), s[33:]...)
		}
	}
	mustPass(t, g.run(t, p, rs))
	mustPass(t, g.run(t, p, sw))
	n, maxK := 0, 0
	for k := 1; k < len(g.code); k++ {
		if g.code[k] != 0x6a {
			continue
		}
		n++
		maxK = k
		for _, c := range []call{rs, sw} {
			if err := g.deltaRunCode(t, p, c, g.code[:k]); err == nil {
				t.Errorf("DELTA-RECORD %s | method %d passes with _codePart truncated to %d bytes", deltaToolchain(t), c.method, k)
			}
		}
	}
	t.Logf("DELTA-RECORD %s | %d in-code cuts tried, the last at %d of %d: all refused", deltaToolchain(t), n, maxK, len(g.code))
	if n < 7*32 {
		t.Fatalf("%d cuts", n)
	}
}

// Extended by one byte where it can matter: TokenReserve's low byte 0x6a, so
// _codePart = code || 0x6a passes the byte checks; its remainder is one byte
// short.
func TestDeltaCodePartExtendedByOneByte(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := pool{bsv: 1_000_000, tokens: 0x4c4b6a, lp: key(t, 10), validator: key(t, 20)}
	p.lock = f.deltaLock(t, deltaStateOf(p))
	cp := append(append([]byte{}, f.code...), 0x6a)
	for _, m := range f.deltaCalls(t, p, affirmationR, affirmationS) {
		if m.name == "Close" {
			continue
		}
		m := m
		t.Run(m.name, func(t *testing.T) {
			mustPass(t, f.deltaRunCode(t, p, m.c, f.code))
			deltaCheck(t, "codePart code||6a with reserve low byte 0x6a "+m.name, f.deltaRunCode(t, p, m.c, cp), deltaFail, deltaFail)
		})
	}
}

// The continuation's two prologue bytes are the contract's hard-coded
// \x61\xab; any other pair is not the output Swap or Rescind built.
func TestDeltaContinuationPrologue(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	if !bytes.HasPrefix(f.code, []byte{0x61, 0xab}) {
		t.Fatalf("code starts %x", f.code[:2])
	}
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	pre := len(tokenPrefix(5_000_000))
	for _, m := range f.deltaCalls(t, p, affirmationR, affirmationS) {
		if m.name == "Close" {
			continue
		}
		m := m
		t.Run(m.name, func(t *testing.T) {
			mustPass(t, f.run(t, p, m.c))
			for _, alt := range [][2]byte{{0x61, 0x61}, {0x00, 0xab}, {0xab, 0x61}, {0x6a, 0xab}} {
				c := m.c
				c.outs = append([]out{}, m.c.outs...)
				s := append([]byte{}, c.outs[0].script...)
				off := pre
				if m.name == "Swap" {
					off = len(tokenPrefix(f.expectSwap(p, 20_000, true).newTokens))
				}
				s[off], s[off+1] = alt[0], alt[1]
				c.outs[0].script = s
				deltaCheck(t, fmt.Sprintf("%s continuation prologue %x", m.name, alt), f.run(t, p, c), deltaFail, deltaFail)
			}
		})
	}
}

// --- the state writer's widths ---

// deltaWide is a next key of w bytes starting with first: from 66 bytes on in
// the shape the wide-key theft needed (TestTheft_WideNextKeyCannotHandThePool
// ToTheValidator): held's key at bytes w-66..w-33, where Close would read
// LpPubKey, and OP_ENDIF x3 and OP_RETURN at the end; 0x05 bytes otherwise.
func deltaWide(w int, first byte, held *ec.PrivateKey) []byte {
	if w == 0 {
		return []byte{}
	}
	if w < 66 {
		return append([]byte{first}, bytes.Repeat([]byte{0x05}, w-1)...)
	}
	k := append(bytes.Repeat([]byte{0x61}, w-66), advWideKey(held)...)
	k[0] = first
	return k
}

// Swap with a nextValidatorPubKey that is not 33 bytes, with the continuation
// poolScript would build for it (the spent scriptCode minus the NEW state's
// length, then the new state) and with the naive one (code || 6a || state).
// Swap's own length check refuses every width on both toolchains. (Rúnar
// main's state writer checks the width as well; the pinned Rúnar's does not,
// and before the contract checked it there a validator-signed Swap wrote a
// pool its LP could not Close, from 66 bytes on one the validator could.)
func TestDeltaSwapNextValidatorKeyWidth(t *testing.T) {
	f := newFixture(t, 30, 5, 0)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	held := key(t, 0x5d)
	r := f.expectSwap(p, 20_000, true)

	t.Run("control: 33 bytes", func(t *testing.T) {
		mustPass(t, f.run(t, p, f.swapCall(p, 20_000, true, r)))
	})
	for _, w := range []int{0, 1, 2, 32, 34, 64, 65, 66, 67, 100} {
		for _, first := range []byte{0x02, 0x03} {
			w, first := w, first
			t.Run(fmt.Sprintf("%d bytes, %02x first", w, first), func(t *testing.T) {
				c, written := f.advWideKeySwap(p, deltaWide(w, first, held))
				deltaCheck(t, fmt.Sprintf("Swap nextValidatorPubKey %d bytes (%02x), continuation as poolScript builds it", w, first), f.run(t, p, c), deltaFail, deltaFail)
				naive := c
				naive.outs = advCopy(c.outs)
				ns := written.lock[len(written.lock)-(stateLen-33+w):]
				naive.outs[0].script = append(append(append(tokenPrefix(r.newTokens), f.code...), 0x6a), ns...)
				deltaCheck(t, fmt.Sprintf("Swap nextValidatorPubKey %d bytes (%02x), continuation code || 6a || state", w, first), f.run(t, p, naive), deltaFail, deltaFail)
			})
		}
	}
	var accepted []int
	for w := 0; w <= 100; w++ {
		if w == 33 {
			continue
		}
		for _, first := range []byte{0x02, 0x03} {
			if c, _ := f.advWideKeySwap(p, deltaWide(w, first, held)); f.run(t, p, c) == nil {
				accepted = append(accepted, w)
			}
		}
	}
	t.Logf("DELTA-RECORD %s | widths 0..100 but 33, 02 and 03 first, the continuation poolScript builds: accepted for %v", deltaToolchain(t), accepted)
	if len(accepted) != 0 {
		t.Errorf("next keys of %v bytes accepted", accepted)
	}
}

// Rescind writes ValidatorPubKey as exactly 33 zero bytes.
func TestDeltaRescindWritesThirtyThreeZeroBytes(t *testing.T) {
	f := newFixture(t, 30, 5, 0)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	for _, w := range []int{33, 32, 34} {
		w := w
		t.Run(fmt.Sprintf("%d zero bytes", w), func(t *testing.T) {
			c := f.rescindCall(p)
			s := deltaStateOf(p)
			s.val = make([]byte, w)
			c.outs[0].script = append(append(append(tokenPrefix(p.tokens), f.code...), 0x6a), s.raw()...)
			want := deltaFail
			if w == 33 {
				want = deltaPass
			}
			deltaCheck(t, fmt.Sprintf("Rescind writing %d zero bytes", w), f.run(t, p, c), want, want)
		})
	}
}

// A pool deployed with a wrong-width state field. Rúnar main's SDK refuses to
// build one. A hand-built one reads wrong on both toolchains: Close reads the
// state as the script's last 243 bytes, so LpPubKey is not the LP's key (every
// field sits behind TokenReserve), and Swap and Rescind's code part no longer
// leaves a 244-byte remainder. Nobody spends it: the overlay must refuse such a
// deploy (the deploy is the deployer's own mistake).
func TestDeltaDeployedStateWithWrongWidthField(t *testing.T) {
	f := newFixture(t, 30, 5, 0)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	cases := []struct {
		name string
		edit func(s *deltaState)
	}{
		{"LpPubKey 32 bytes", func(s *deltaState) { s.lp = s.lp[:32] }},
		{"Nonce 31 bytes", func(s *deltaState) { s.nonce = s.nonce[:31] }},
		{"LpSatsPkh 21 bytes", func(s *deltaState) { s.sats = append(append([]byte{}, s.sats...), 0x66) }},
		{"AffirmationR 33 bytes", func(s *deltaState) { s.affR = append(append([]byte{}, s.affR...), 0x72) }},
		{"AffirmationS 31 bytes", func(s *deltaState) { s.affS = s.affS[:31] }},
		{"AffirmationS empty", func(s *deltaState) { s.affS = nil }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s := deltaStateOf(p)
			tc.edit(&s)
			_, sdkErr := deltaSDKScript(t, s, assetID, f.deltaFees())
			deltaCheck(t, tc.name+": the SDK builds it", sdkErr, deltaFail, deltaPass)

			q := p
			q.lock = append(append(append(tokenPrefix(p.tokens), f.code...), 0x6a), s.raw()...)
			deltaCheck(t, tc.name+": LP Close", f.run(t, q, f.closeCall(q)), deltaFail, deltaFail)
			r := f.expectSwap(q, 20_000, true)
			sw := f.swapCall(q, 20_000, true, r)
			ns := s
			ns.tokens, ns.val = r.newTokens, r.nextValidator.PubKey().Compressed()
			sw.outs[0].script = append(append(append(tokenPrefix(r.newTokens), f.code...), 0x6a), ns.raw()...)
			deltaCheck(t, tc.name+": Swap", f.run(t, q, sw), deltaFail, deltaFail)
			rs := f.rescindCall(q)
			rsS := s
			rsS.val = make([]byte, 33)
			rs.outs[0].script = append(append(append(tokenPrefix(q.tokens), f.code...), 0x6a), rsS.raw()...)
			deltaCheck(t, tc.name+": Rescind", f.run(t, q, rs), deltaFail, deltaFail)
		})
	}
}

// Bytes after the state on the spent pool (a deploy the writer never makes).
// Close reads the last 243 bytes, LpPubKey moved by the extra bytes; Swap and
// Rescind find no 244-byte remainder behind an honest code part. Every method
// is refused on both toolchains: the overlay must refuse such a deploy.
func TestDeltaTrailingBytesAfterState(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	base := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	for _, g := range []struct {
		name  string
		extra []byte
	}{{"one 0x00 byte", []byte{0x00}}, {"an extra push", []byte{0x02, 0xbe, 0xef}}, {"an OP_RETURN", []byte{0x6a}}} {
		g := g
		t.Run(g.name, func(t *testing.T) {
			p := base
			clean := f.deltaLock(t, deltaStateOf(p))
			p.lock = append(append([]byte{}, clean...), g.extra...)
			body := p.lock[len(tokenPrefix(p.tokens)):]
			for _, m := range f.deltaCalls(t, base, affirmationR, affirmationS) {
				deltaCheck(t, fmt.Sprintf("trailing %s: %s", g.name, m.name), f.run(t, p, m.c), deltaFail, deltaFail)
				if m.name == "Close" {
					continue
				}
				// The cut that leaves a 244-byte remainder, where it starts with 0x6a.
				if k := len(body) - 1 - stateLen; body[k] == 0x6a {
					deltaCheck(t, fmt.Sprintf("trailing %s: %s with the code part ending 244 bytes before the end", g.name, m.name), f.deltaRunCode(t, p, m.c, body[:k]), deltaFail, deltaFail)
				}
			}
		})
	}
}

// A hand-built lock carrying a second OP_RETURN and state after the first:
// prefix || code || 6a || state A || 6a || state B. The script reads only the
// last 243 bytes, so on both toolchains it is state B's pool: B's LP Closes it
// and A's cannot, and a Swap (its code part running through state A, the
// remainder 6a || state B) writes state B on, carrying state A inside the
// continuation's code region. Not a spend of an honest pool (the writer never
// makes this script): the overlay must read the state as the script's last
// 243 bytes, or require the lock to be exactly prefix || code || 6a || 243
// bytes, before it lists a pool as A's.
func TestDeltaSecondStateAfterTheState(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	a := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	b := a
	b.lp = key(t, 0x0b)
	stA := f.deltaLock(t, deltaStateOf(a))[len(tokenPrefix(a.tokens))+len(f.code):] // 6a || state A
	lock := append(append(append(tokenPrefix(a.tokens), f.code...), stA...), append([]byte{0x6a}, b.state()...)...)
	p := b
	p.lock = lock
	deltaCheck(t, "Close by state B's LP", f.run(t, p, f.closeCall(p)), deltaPass, deltaPass)
	deltaCheck(t, "Close by state A's LP", f.run(t, p, advSetSigner(f.closeCall(p), a.lp)), deltaFail, deltaFail)

	r := f.expectSwap(b, 20_000, true)
	sw := f.swapCall(b, 20_000, true, r)
	codePart := append(append([]byte{}, f.code...), stA...)
	sw.outs[0].script = append(append(tokenPrefix(r.newTokens), codePart...), append([]byte{0x6a}, r.pool(b).state()...)...)
	deltaCheck(t, "Swap with the code part through state A, continuation as poolScript builds it", f.deltaRunCode(t, p, sw, codePart), deltaPass, deltaPass)
	deltaCheck(t, "Swap with the honest code part", f.deltaRunCode(t, p, f.swapCall(b, 20_000, true, r), f.code), deltaFail, deltaFail)
}

// --- the affirmation ---

// deltaAffPatterns are affirmation values as r and s; the last is a real
// signature's.
func deltaAffPatterns(t *testing.T) []struct {
	name string
	r, s []byte
} {
	sig, err := key(t, 0x7f).Sign(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	be32 := func(b []byte) []byte { return append(make([]byte, 32-len(b)), b...) }
	return []struct {
		name string
		r, s []byte
	}{
		{"the stand-in (0x72.., 0x73..)", affirmationR, affirmationS},
		{"all 0x6a", bytes.Repeat([]byte{0x6a}, 32), bytes.Repeat([]byte{0x6a}, 32)},
		{"all zero", make([]byte, 32), make([]byte, 32)},
		{"all 0xff", bytes.Repeat([]byte{0xff}, 32), bytes.Repeat([]byte{0xff}, 32)},
		{"push opcodes (0x4c, 0x4d, 0x4e)", bytes.Repeat([]byte{0x4c}, 32), append(bytes.Repeat([]byte{0x4d}, 16), bytes.Repeat([]byte{0x4e}, 16)...)},
		{"a real signature's r and s", be32(sig.R.Bytes()), be32(sig.S.Bytes())},
	}
}

// The affirmation is r and s, 32 bytes each, raw at the end of the state: the
// SDK writes them unframed, and refuses any other width at Rúnar main.
func TestDeltaAffirmationSDKLayout(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	for _, a := range deltaAffPatterns(t) {
		s := deltaStateOf(p)
		s.affR, s.affS = a.r, a.s
		tail := f.deltaTail(t, s)
		if len(tail) != 1+stateLen || !bytes.HasSuffix(tail, append(append([]byte{}, a.r...), a.s...)) {
			t.Errorf("%s: tail %d bytes, does not end r || s", a.name, len(tail))
		}
	}
	for _, n := range []int{0, 1, 31, 33, 64, 72} {
		for _, which := range []string{"AffirmationR", "AffirmationS"} {
			s := deltaStateOf(p)
			b := bytes.Repeat([]byte{0x30}, n)
			if which == "AffirmationR" {
				s.affR = b
			} else {
				s.affS = b
			}
			_, err := deltaSDKScript(t, s, assetID, f.deltaFees())
			deltaCheck(t, fmt.Sprintf("the SDK builds a pool with a %d-byte %s", n, which), err, deltaFail, deltaPass)
		}
	}
}

// Swap (both directions) -> second Swap -> Rescind -> Close, and Close
// directly, for each affirmation pattern; each continuation framed by the SDK.
// Every byte value is carried as is, on both toolchains.
func TestDeltaAffirmationLifecycle(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	for _, a := range deltaAffPatterns(t) {
		a := a
		at := func(p pool) pool {
			s := deltaStateOf(p)
			s.affR, s.affS = a.r, a.s
			p.lock = f.deltaLock(t, s)
			return p
		}
		t.Run(a.name, func(t *testing.T) {
			p := at(pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)})
			deltaCheck(t, a.name+": Close", f.run(t, p, f.closeCall(p)), deltaPass, deltaPass)
			for _, bsvIn := range []bool{true, false} {
				dir := map[bool]string{true: "bsv in", false: "tokens in"}[bsvIn]
				r1 := f.expectSwap(p, 20_000, bsvIn)
				p2 := at(r1.pool(p))
				c1 := f.swapCall(p, 20_000, bsvIn, r1)
				c1.outs[0].script = p2.lock
				deltaCheck(t, a.name+": Swap "+dir, f.run(t, p, c1), deltaPass, deltaPass)
				r2 := f.expectSwap(p2, 7_000, !bsvIn)
				p3 := at(r2.pool(p2))
				c2 := f.swapCall(p2, 7_000, !bsvIn, r2)
				c2.outs[0].script = p3.lock
				deltaCheck(t, a.name+": "+dir+" then a second Swap", f.run(t, p2, c2), deltaPass, deltaPass)
				c3 := f.rescindCall(p3)
				p4 := p3
				p4.rescinded = true
				p4 = at(p4)
				c3.outs[0].script = p4.lock
				deltaCheck(t, a.name+": "+dir+", Swap, Rescind", f.run(t, p3, c3), deltaPass, deltaPass)
				deltaCheck(t, a.name+": "+dir+", Swap, Rescind, Close", f.run(t, p4, f.closeCall(p4)), deltaPass, deltaPass)
			}
		})
	}
}

// The affirmation is carried unchanged: a continuation with it altered is not
// what Swap or Rescind wrote.
func TestDeltaAffirmationChangedInContinuation(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	p.lock = f.deltaLock(t, deltaStateOf(p))
	for _, alt := range []struct {
		name string
		r, s []byte
	}{
		{"r: one byte changed", deltaFlip(affirmationR, 10), affirmationS},
		{"s: one byte changed", affirmationR, deltaFlip(affirmationS, 31)},
		{"r and s swapped", affirmationS, affirmationR},
		{"r and s zeroed", make([]byte, 32), make([]byte, 32)},
	} {
		alt := alt
		honest := f.deltaCalls(t, p, affirmationR, affirmationS)
		bad := f.deltaCalls(t, p, alt.r, alt.s)
		for i, m := range honest {
			if m.name == "Close" {
				continue
			}
			i, m := i, m
			t.Run(alt.name+": "+m.name, func(t *testing.T) {
				mustPass(t, f.run(t, p, m.c))
				deltaCheck(t, "affirmation "+alt.name+" in the "+m.name+" continuation", f.run(t, p, bad[i].c), deltaFail, deltaFail)
			})
		}
	}
}
