package pool

// Behaviour deltas between the pinned Rúnar (go.mod, Sept 2026) and Rúnar
// HEAD (go.work), executed through the real interpreter with the harness in
// pool_test.go. Every test passes on both toolchains: where the two differ the
// test asserts each one's behaviour explicitly, or logs it on a greppable
// `DELTA-RECORD` line (`go test -v -run TestDelta ./pool/ | grep DELTA-RECORD`).
//
// The toolchain is detected at runtime from the artifact JSON: HEAD has a
// `codePartLenSlots` key (N-051), the pinned artifact type has no such field.

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
	ParamIndex   int `json:"paramIndex"`
	ByteOffset   int `json:"byteOffset"`
	CodeSepIndex int `json:"codeSepIndex"`
}

type deltaArtifact struct {
	Script             string      `json:"script"`
	ASM                string      `json:"asm"`
	ConstructorSlots   []deltaSlot `json:"constructorSlots"`
	CodeSepIndexSlots  []deltaSlot `json:"codeSepIndexSlots"`
	CodePartLenSlots   []deltaSlot `json:"codePartLenSlots"`
	CodeSeparatorIndex *int        `json:"codeSeparatorIndex"`

	head     bool // the JSON has a codePartLenSlots key
	template []byte
	sa       runar.RunarArtifact
}

var (
	deltaArtOnce sync.Once
	deltaArtVal  *deltaArtifact
	deltaArtErr  error
)

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
		a := &deltaArtifact{}
		if deltaArtErr = json.Unmarshal(js, a); deltaArtErr != nil {
			return
		}
		var keys map[string]json.RawMessage
		if deltaArtErr = json.Unmarshal(js, &keys); deltaArtErr != nil {
			return
		}
		_, a.head = keys["codePartLenSlots"]
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

func deltaToolchain(t *testing.T) string {
	if deltaArt(t).head {
		return "HEAD"
	}
	return "pinned"
}

// --- expectations ---

type deltaWant int

const (
	deltaPass   deltaWant = iota // must pass
	deltaFail                    // must fail
	deltaRecord                  // log the outcome only
)

// deltaCheck asserts err against head's or pinned's expectation, by toolchain,
// and logs a DELTA-RECORD line either way.
func deltaCheck(t *testing.T, name string, err error, head, pinned deltaWant) {
	t.Helper()
	want := pinned
	if deltaArt(t).head {
		want = head
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
	tokens                             int64
	lp, val, id, nonce, sats, tok, aff []byte
}

// deltaStateOf is the state of a harness pool with the given Affirmation.
func deltaStateOf(p pool, aff []byte) deltaState {
	val := make([]byte, 33)
	if !p.rescinded {
		val = p.validator.PubKey().Compressed()
	}
	sats, tok := p.feePkhs()
	return deltaState{
		tokens: p.tokens, lp: p.lp.PubKey().Compressed(), val: val,
		id: validatorIdentity.PubKey().Compressed(), nonce: nonce, sats: sats, tok: tok, aff: aff,
	}
}

// raw hand-concatenates the state: the fixed fields at whatever width they
// have, then the Affirmation as one length byte and the bytes (< 76 only).
func (s deltaState) raw() []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(s.tokens))
	for _, f := range [][]byte{s.lp, s.val, s.id, s.nonce, s.sats, s.tok} {
		b = append(b, f...)
	}
	if len(s.aff) >= 0x4c {
		panic("raw: Affirmation too long for one length byte")
	}
	return append(append(b, byte(len(s.aff))), s.aff...)
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
		s.tokens, h(s.lp), h(s.val), h(s.id), h(s.nonce), h(s.sats), h(s.tok), h(s.aff),
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
	// The SDK's code part does not depend on the state (f.code may be a
	// hand-spliced variant of it, of the same length).
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
	s := deltaStateOf(p, affirmation)
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

// deltaCalls are the three methods on p (its lock set), each with output 0
// the continuation the SDK frames for state with Affirmation aff. Swap is
// bsv in, 20,000 sats.
func (f *fixture) deltaCalls(t *testing.T, p pool, aff []byte) []struct {
	name string
	c    call
} {
	t.Helper()
	r := f.expectSwap(p, 20_000, true)
	sw := f.swapCall(p, 20_000, true, r)
	sw.outs[0].script = f.deltaLock(t, deltaStateOf(r.pool(p), aff))
	rs := f.rescindCall(p)
	cleared := p
	cleared.rescinded = true
	rs.outs[0].script = f.deltaLock(t, deltaStateOf(cleared, aff))
	return []struct {
		name string
		c    call
	}{{"Swap", sw}, {"Close", f.closeCall(p)}, {"Rescind", rs}}
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

// --- the hand-spliced template (a builder that predates codePartLenSlots) ---

// deltaHandSplice instantiates the template the way a builder that predates
// codePartLenSlots does (the pages' vendored runar-sdk, web/ui/src/vendor):
// each codeSepIndex slot resolved to OP_1, each constructor slot's OP_0
// replaced by the arg's push, the template's length fields left as compiled. It returns the code and the
// built-code offsets of the template's codePartLenSlots.
func deltaHandSplice(t *testing.T, asset []byte, fees [3]int64) (code []byte, pinOffsets []int) {
	t.Helper()
	a := deltaArt(t)
	type sub struct {
		off int
		enc []byte
	}
	var subs []sub
	for _, cs := range a.CodeSepIndexSlots {
		if cs.CodeSepIndex != 1 || a.template[cs.ByteOffset] != 0x00 {
			t.Fatalf("codeSepIndex slot %+v", cs)
		}
		subs = append(subs, sub{cs.ByteOffset, []byte{0x51}})
	}
	for _, s := range a.ConstructorSlots {
		if a.template[s.ByteOffset] != 0x00 {
			t.Fatalf("constructor slot %+v is not an OP_0 placeholder", s)
		}
		var enc []byte
		switch s.ParamIndex {
		case 8:
			enc = pushData(asset)
		case 9, 10, 11:
			enc = pushNum(fees[s.ParamIndex-9])
		default:
			t.Fatalf("unexpected constructor slot param %d", s.ParamIndex)
		}
		subs = append(subs, sub{s.ByteOffset, enc})
	}
	for _, pin := range a.CodePartLenSlots {
		shift := 0
		for _, s := range subs {
			if s.off < pin.ByteOffset {
				shift += len(s.enc) - 1
			}
		}
		pinOffsets = append(pinOffsets, pin.ByteOffset+shift)
	}
	sort.Slice(subs, func(i, j int) bool { return subs[i].off > subs[j].off })
	code = append([]byte{}, a.template...)
	for _, s := range subs {
		code = append(append(append([]byte{}, code[:s.off]...), s.enc...), code[s.off+1:]...)
	}
	return code, pinOffsets
}

// deltaTemplateMatches mirrors src/pool.zig `parse`'s code match: the
// built code equals the template (codeSepIndex slots as OP_1) everywhere
// outside the constructor slots. A Go mirror of the Zig, not the Zig.
func deltaTemplateMatches(t *testing.T, code []byte) bool {
	t.Helper()
	a := deltaArt(t)
	tpl := append([]byte{}, a.template...)
	for _, cs := range a.CodeSepIndexSlots {
		tpl[cs.ByteOffset] = 0x51
	}
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

// --- A. the code-part length pin ---

// The sizes and offsets below are those of the two compilers these tests were
// written against: the pin in go.mod, and Rúnar main at 6bd5e29f4. A later
// Rúnar, or an edit to the contract, moves them: update them here.
func TestDeltaToolchainShape(t *testing.T) {
	a := deltaArt(t)
	t.Logf("toolchain %s, template %d bytes", deltaToolchain(t), len(a.template))

	t.Run("prologue is OP_NOP OP_CODESEPARATOR and the separator index is 1", func(t *testing.T) {
		if !bytes.HasPrefix(a.template, []byte{0x61, 0xab}) {
			t.Fatalf("template starts %x", a.template[:2])
		}
		if a.CodeSeparatorIndex == nil || *a.CodeSeparatorIndex != 1 {
			t.Fatalf("codeSeparatorIndex %v", a.CodeSeparatorIndex)
		}
	})
	t.Run("one code-part length check per public method", func(t *testing.T) {
		// HEAD: 04 <LE len> OP_BIN2NUM OP_NUMEQUAL OP_VERIFY at each
		// codePartLenSlots entry, the template carrying its own length.
		// Pinned: 04 <LE len> OP_BIN2NUM OP_GREATERTHANOREQUAL OP_VERIFY,
		// a lower bound, with no slots.
		tl := make([]byte, 4)
		binary.LittleEndian.PutUint32(tl, uint32(len(a.template)))
		eq := append(append([]byte{0x04}, tl...), 0x81, 0x9c, 0x69)
		ge := append(append([]byte{0x04}, tl...), 0x81, 0xa2, 0x69)
		if a.head {
			if len(a.template) != 3879 {
				t.Errorf("HEAD template %d bytes, expected 3879", len(a.template))
			}
			var offs []int
			for _, s := range a.CodePartLenSlots {
				offs = append(offs, s.ByteOffset)
				if !bytes.Equal(a.template[s.ByteOffset-1:s.ByteOffset+7], eq) {
					t.Errorf("slot %d: %x", s.ByteOffset, a.template[s.ByteOffset-1:s.ByteOffset+7])
				}
			}
			if fmt.Sprint(offs) != "[500 2643 3317]" {
				t.Errorf("codePartLenSlots %v", offs)
			}
			if n := bytes.Count(a.template, eq); n != 3 || bytes.Contains(a.template, ge) {
				t.Errorf("equality pins %d, lower bound present %v", n, bytes.Contains(a.template, ge))
			}
		} else {
			if len(a.template) != 3773 {
				t.Errorf("pinned template %d bytes, expected 3773", len(a.template))
			}
			if n := bytes.Count(a.template, ge); n != 3 || bytes.Contains(a.template, eq) {
				t.Errorf("lower-bound pins %d, equality present %v", n, bytes.Contains(a.template, eq))
			}
		}
	})
	t.Run("OP_PUSH_TX raw span", func(t *testing.T) {
		want := map[bool]string{true: "<raw 422 bytes>", false: "<raw 428 bytes>"}[a.head]
		if n := strings.Count(a.ASM, want); n != 3 {
			t.Fatalf("%q appears %d times", want, n)
		}
	})
	t.Run("fixed-width state field size asserts before the continuation's state", func(t *testing.T) {
		// Swap and Rescind write the state: 6 fixed-width fields each.
		n := 0
		for _, w := range []string{"21", "20", "14"} {
			n += strings.Count(a.ASM, "OP_SIZE <"+w+"> OP_NUMEQUALVERIFY")
		}
		want := map[bool]int{true: 12, false: 0}[a.head]
		if n != want {
			t.Fatalf("%d size asserts, want %d", n, want)
		}
	})
	t.Run("ByteString state reader: unsigned length, all of the state consumed", func(t *testing.T) {
		// HEAD: three readers (Swap, Close, Rescind) each read the length
		// unsigned (<00> OP_CAT) and end with OP_0 OP_EQUALVERIFY.
		n := strings.Count(a.ASM, "OP_ENDIF OP_0 OP_EQUALVERIFY")
		pn := strings.Count(a.ASM, "OP_ENDIF OP_ENDIF OP_DROP")
		if a.head && n != 3 || !a.head && (n != 0 || pn < 3) {
			t.Fatalf("consumed-state asserts %d, trailing drops %d", n, pn)
		}
	})
}

func TestDeltaSDKPoolPinsItsDeployedCodeLength(t *testing.T) {
	for _, row := range deltaFeeRows {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t, row.fees[0], row.fees[1], row.fees[2])
			a := deltaArt(t)
			_, offs := deltaHandSplice(t, assetID, row.fees)
			if a.head {
				if len(offs) != 3 {
					t.Fatalf("%d pin offsets", len(offs))
				}
				for _, off := range offs {
					got := int(binary.LittleEndian.Uint32(f.code[off : off+4]))
					if f.code[off-1] != 0x04 || !bytes.Equal(f.code[off+4:off+7], []byte{0x81, 0x9c, 0x69}) || got != len(f.code) {
						t.Fatalf("pin at %d: %x (value %d, code %d bytes)", off, f.code[off-1:off+7], got, len(f.code))
					}
				}
				t.Logf("code %d bytes, pinned at %v", len(f.code), offs)
			}
			p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
			for _, m := range f.deltaCalls(t, p, affirmation) {
				err := f.run(t, p, m.c)
				if m.name == "Swap" && row.fees[0] == 10000 {
					// A 100% fee leaves net 0: no Swap passes on either toolchain.
					mustFail(t, err)
					continue
				}
				deltaCheck(t, row.name+" SDK pool "+m.name, err, deltaPass, deltaPass)
			}
		})
	}
}

// The pages deploy through the vendored runar-sdk (web/ui/src/vendor), which
// splices constructor args at constructorSlots and resolves codeSepIndexSlots
// and knows nothing of codePartLenSlots. At HEAD a pool built that way keeps
// the template's length (3879) in every method's pin, while its real code part
// is longer: every method fails, Close included. Funds sent to such a pool are
// locked for good. (gen/vectors builds through the Go SDK, which patches the
// fields; src/pool.zig parse only matches a lock against the template, and
// the mirror below shows a regenerated template would not match a patched
// pool.)
func TestDeltaHandSplicedHeadTemplateLocksFundsInAllThreeMethods(t *testing.T) {
	a := deltaArt(t)
	for _, row := range deltaFeeRows {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t, row.fees[0], row.fees[1], row.fees[2])
			hand, offs := deltaHandSplice(t, assetID, row.fees)
			if len(hand) != len(f.code) {
				t.Fatalf("hand-spliced %d bytes, SDK %d", len(hand), len(f.code))
			}
			var diff []int
			for i := range hand {
				if hand[i] != f.code[i] {
					diff = append(diff, i)
				}
			}
			patched := append([]byte{}, hand...)
			for _, off := range offs {
				binary.LittleEndian.PutUint32(patched[off:], uint32(len(patched)))
			}

			t.Run("bytes: the hand splice differs from the SDK only in the length fields", func(t *testing.T) {
				if !a.head {
					if len(diff) != 0 {
						t.Fatalf("pinned: hand splice differs at %v", diff)
					}
					return
				}
				var want []int
				for _, off := range offs {
					want = append(want, off, off+1, off+2, off+3)
				}
				// A field byte can coincide (0x00 high bytes, for instance).
				for _, d := range diff {
					found := false
					for _, w := range want {
						found = found || d == w
					}
					if !found {
						t.Fatalf("hand splice differs outside the length fields at %d", d)
					}
				}
				if len(diff) == 0 {
					t.Fatal("hand splice equals the SDK code: the pin is not patched?")
				}
				for _, off := range offs {
					if v := binary.LittleEndian.Uint32(hand[off:]); v != uint32(len(a.template)) {
						t.Fatalf("hand-spliced pin at %d holds %d, want the template's %d", off, v, len(a.template))
					}
				}
			})
			t.Run("bytes: patching the three length fields gives the SDK's code", func(t *testing.T) {
				if !bytes.Equal(patched, f.code) {
					t.Fatal("patched hand splice differs from the SDK code")
				}
			})
			t.Run("zig parse mirror: the template matches the hand splice, not the SDK code", func(t *testing.T) {
				if !deltaTemplateMatches(t, hand) {
					t.Fatal("hand splice does not match its own template")
				}
				// At HEAD the overlay's template match (pool.zig, mirrored
				// here) would not recognise an SDK-built pool as a pool.
				deltaCheck(t, row.name+" zig-mirror template match on SDK code",
					map[bool]error{true: nil, false: fmt.Errorf("no match")}[deltaTemplateMatches(t, f.code)],
					deltaFail, deltaPass)
			})

			g := *f
			g.code = hand
			p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
			for _, m := range g.deltaCalls(t, p, affirmation) {
				t.Run("hand-spliced pool: "+m.name, func(t *testing.T) {
					err := g.run(t, p, m.c)
					if m.name == "Swap" && row.fees[0] == 10000 {
						mustFail(t, err) // net 0 on either toolchain; not evidence
						return
					}
					deltaCheck(t, row.name+" hand-spliced "+m.name, err, deltaFail, deltaPass)
				})
			}
			t.Run("hand-spliced pool: Close with a _codePart of the template's length (the stale pin)", func(t *testing.T) {
				// The only _codePart a stale pin of 3879 accepts is the
				// script's first 3879 bytes, and only where the next byte
				// is 0x6a. Either way the LP cannot Close.
				n := len(a.template)
				lock := g.poolScript(p)
				body := lock[len(tokenPrefix(p.tokens)):]
				t.Logf("byte %d of the hand-spliced code is %#x", n, body[n])
				err := g.deltaRunCode(t, p, g.closeCall(p), body[:n])
				deltaCheck(t, row.name+" hand-spliced Close with a template-length code part", err, deltaFail, deltaRecord)
			})
			t.Run("patched pool: Close passes", func(t *testing.T) {
				h := *f
				h.code = patched
				mustPass(t, h.run(t, p, h.closeCall(p)))
			})
		})
	}
}

// _codePart tampering on a correct (SDK-built) pool, every method. HEAD: all
// rejected. Pinned: rejected too (these cuts fall below its lower bound or
// fail its byte checks).
func TestDeltaCodePartWitnessTampering(t *testing.T) {
	a := deltaArt(t)
	f := newFixture(t, 30, 5, 10)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	p.lock = f.deltaLock(t, deltaStateOf(p, affirmation))
	calls := f.deltaCalls(t, p, affirmation)

	t.Run("control: the honest code part passes every method", func(t *testing.T) {
		for _, m := range calls {
			mustPass(t, f.deltaRunCode(t, p, m.c, f.code))
		}
	})

	other := bytes.Repeat([]byte{0xb6}, 32)
	cases := []struct {
		name string
		cp   []byte
	}{
		{"truncated by 1 byte", f.code[:len(f.code)-1]},
		{"extended by 0x00", append(append([]byte{}, f.code...), 0x00)},
		{"first byte flipped", deltaFlip(f.code, 0)},
		{"middle byte flipped", deltaFlip(f.code, len(f.code)/2)},
		{"last byte flipped", deltaFlip(f.code, len(f.code)-1)},
		{"empty", []byte{}},
		{"the template itself", a.template},
		{"another pool's code (other AssetId)", deltaCode(t, other, f.deltaFees())},
		{"another pool's code (other fees)", deltaCode(t, assetID, [3]int64{31, 5, 10})},
	}
	for _, tc := range cases {
		for _, m := range calls {
			t.Run(tc.name+": "+m.name, func(t *testing.T) {
				deltaCheck(t, "codePart "+tc.name+" "+m.name, f.deltaRunCode(t, p, m.c, tc.cp), deltaFail, deltaFail)
			})
		}
	}

	// Every cut the pin block's byte checks accept: _codePart = script[:k]
	// with script[k] == 0x6a (the block requires 61ab || sc[:k-2] == _codePart
	// and the byte after to be OP_RETURN). Cuts in the code are truncations,
	// cuts past it extend into the state. HEAD's equality pin rejects them
	// all; pinned's lower bound (>= 3773) lets every k >= 3773 through the
	// pin, and the state is then read from script[k+1:].
	body := p.lock[len(tokenPrefix(p.tokens)):]
	signers := map[string]*ec.PrivateKey{"LP": p.lp, "validator": p.validator, "identity": validatorIdentity}
	for k := 1; k < len(body); k++ {
		if body[k] != 0x6a || k == len(f.code) {
			continue
		}
		where := fmt.Sprintf("code byte %d", k)
		if k > len(f.code) {
			where = fmt.Sprintf("state byte %d", k-len(f.code)-1)
		}
		for _, m := range calls {
			for sn, sk := range signers {
				if m.name != "Close" && sn == "LP" {
					continue
				}
				c := m.c
				c.signers = []*ec.PrivateKey{sk}
				name := fmt.Sprintf("cut at %d (%s, %s): %s signed by %s", k, where, map[bool]string{true: ">= 3773", false: "< 3773"}[k >= 3773], m.name, sn)
				t.Run(name, func(t *testing.T) {
					// Pinned: these keys have no 0x6a at an aligning byte
					// (see TestDeltaPinnedCodePartRunIntoState... for one that does).
					deltaCheck(t, name, f.deltaRunCode(t, p, c, body[:k]), deltaFail, deltaFail)
				})
			}
		}
	}
}

func deltaFlip(b []byte, i int) []byte {
	c := append([]byte{}, b...)
	c[i] ^= 0x01
	return c
}

// THEFT ON PINNED. On pinned, _codePart may run past the code into the state
// (any cut k >= 3773 with script[k] == 0x6a passes the lower bound), and the
// state is then read from script[k+1:], every field moved by j+1 bytes for a
// cut at state byte j. A cut at state byte 32 (LpPubKey's byte 24) moves them
// 33 bytes: the LpPubKey Close checks is the ValidatorPubKey, so the
// validator Closes the LP's pool, outputs free. A cut at state byte 65
// (ValidatorPubKey's byte 24) makes it ValidatorIdentity. Each needs 0x6a at
// byte 24 of one key (1 in 256) and a parse of the moved Affirmation framing
// that does not abort (the harness Affirmation's bytes do not). HEAD's
// equality pin rejects both.
func TestDeltaPinnedCodePartRunIntoStateLetsValidatorStealViaClose(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	k24 := deltaKeyWithByte(t, 24, 0x6a)
	for _, tc := range []struct {
		name  string
		lp    *ec.PrivateKey
		val   *ec.PrivateKey
		j     int
		thief func(p pool) *ec.PrivateKey
	}{
		{"LpPubKey byte 24 is 0x6a: the ValidatorPubKey holder Closes", k24, key(t, 20), 32, func(p pool) *ec.PrivateKey { return p.validator }},
		{"ValidatorPubKey byte 24 is 0x6a: the ValidatorIdentity holder Closes", key(t, 10), k24, 65, func(pool) *ec.PrivateKey { return validatorIdentity }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: tc.lp, validator: tc.val}
			p.lock = f.deltaLock(t, deltaStateOf(p, affirmation))
			body := p.lock[len(tokenPrefix(p.tokens)):]
			k := len(f.code) + 1 + tc.j
			if body[k] != 0x6a {
				t.Fatalf("state byte %d is %#x", tc.j, body[k])
			}
			thief := tc.thief(p)
			steal := f.closeCall(p)
			steal.outs = []out{{p.bsv - 500, p2pkh(pkh(thief))}, {1, tokenP2pkh(p.tokens, pkh(thief))}}
			steal.signers = []*ec.PrivateKey{thief}

			t.Run("control: the thief's Close with the honest code part", func(t *testing.T) {
				mustFail(t, f.deltaRunCode(t, p, steal, f.code))
			})
			t.Run("control: the LP's Close with the honest code part", func(t *testing.T) {
				mustPass(t, f.deltaRunCode(t, p, f.closeCall(p), f.code))
			})
			t.Run(fmt.Sprintf("the thief Closes with _codePart run %d bytes into the state", tc.j+1), func(t *testing.T) {
				deltaCheck(t, fmt.Sprintf("theft: Close via codePart cut at state byte %d (%s)", tc.j, tc.name), f.deltaRunCode(t, p, steal, body[:k]), deltaFail, deltaPass)
			})
		})
	}
}

// THEFT ON PINNED: the validator signs one ordinary Swap naming a next key
// with 0x6a at byte 24 (the contract never checks the derivation; ~256 tries
// to grind), then Closes the pool with ValidatorIdentity and a _codePart run
// 66 bytes into the state. The Swap is honest on both toolchains; the Close
// passes only on pinned, and only when the Affirmation's bytes parse as its
// framing once moved 66 bytes: the stand-in Affirmation here does; of real
// DER signatures about 2% do (TestDeltaPinnedTheftRateOverRealAffirmations).
// The validator signs the Affirmation, so it could pick one that does.
func TestDeltaPinnedValidatorGrindsNextKeyThenStealsViaClose(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	r := f.expectSwap(p, 20_000, true)
	r.nextValidator = deltaKeyWithByte(t, 24, 0x6a)
	mustPass(t, f.run(t, p, f.swapCall(p, 20_000, true, r)))

	after := r.pool(p)
	after.lock = f.poolScript(after)
	body := after.lock[len(tokenPrefix(after.tokens)):]
	k := len(f.code) + 1 + 65
	steal := f.closeCall(after)
	steal.outs = []out{{after.bsv - 500, p2pkh(pkh(validatorIdentity))}, {1, tokenP2pkh(after.tokens, pkh(validatorIdentity))}}
	steal.signers = []*ec.PrivateKey{validatorIdentity}
	mustFail(t, f.deltaRunCode(t, after, steal, f.code))
	mustPass(t, f.run(t, after, f.closeCall(after)))
	deltaCheck(t, "theft: Swap to a ground next key, then identity Close via codePart cut at state byte 65",
		f.deltaRunCode(t, after, steal, body[:k]), deltaFail, deltaPass)
}

// Truncations inside the code. With an AssetId of 32 0x6a bytes every
// AssetId byte is a cut the pin block's byte checks accept, but on pinned all
// of them lie below the 3773 lower bound (the last AssetId push ends before
// it), and HEAD's equality rejects every one: no in-code truncation passes on
// either toolchain. Close is the oracle (no continuation).
func TestDeltaCodePartTruncatedInsideCode(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	asset := bytes.Repeat([]byte{0x6a}, 32)
	g := *f
	g.code = deltaCode(t, asset, f.deltaFees())
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	p.lock = g.poolScript(p)
	mustPass(t, g.run(t, p, g.closeCall(p)))
	n, maxK := 0, 0
	for k := 1; k < len(g.code); k++ {
		if g.code[k] != 0x6a {
			continue
		}
		n++
		maxK = k
		for _, signer := range []*ec.PrivateKey{p.lp, p.validator, validatorIdentity} {
			c := g.closeCall(p)
			c.signers = []*ec.PrivateKey{signer}
			if err := g.deltaRunCode(t, p, c, g.code[:k]); err == nil {
				t.Errorf("DELTA-RECORD %s | Close passes with _codePart truncated to %d bytes", deltaToolchain(t), k)
			}
		}
	}
	t.Logf("DELTA-RECORD %s | %d in-code cuts tried, the last at %d (pinned lower bound 3773): all rejected", deltaToolchain(t), n, maxK)
	if n < 7*32 || maxK >= 3773 && !deltaArt(t).head {
		t.Fatalf("%d cuts, last at %d", n, maxK)
	}
}

// Extended by one byte where it can matter: TokenReserve's low byte 0x6a, so
// _codePart = code || 0x6a passes pinned's byte checks.
func TestDeltaCodePartExtendedByOneByte(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := pool{bsv: 1_000_000, tokens: 0x4c4b6a, lp: key(t, 10), validator: key(t, 20)}
	p.lock = f.deltaLock(t, deltaStateOf(p, affirmation))
	cp := append(append([]byte{}, f.code...), 0x6a)
	for _, m := range f.deltaCalls(t, p, affirmation) {
		t.Run(m.name, func(t *testing.T) {
			mustPass(t, f.deltaRunCode(t, p, m.c, f.code))
			// Pinned passes the pin and the byte checks here and fails at
			// CHECKSIG on a shifted, invalid key ("unsupported public key type").
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
	for _, m := range f.deltaCalls(t, p, affirmation) {
		if m.name == "Close" {
			continue
		}
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

// --- B. fixed-width state field size asserts ---

// Swap with a nextValidatorPubKey that is not 33 bytes. HEAD's writer asserts
// the width. Pinned writes it raw; poolScript takes the continuation's code as
// scriptCode minus the NEW state's length, so a width w moves 33-w bytes
// between the code region and the state.
func TestDeltaSwapNextValidatorKeyWidth(t *testing.T) {
	f := newFixture(t, 30, 5, 0)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	p.lock = f.poolScript(p)
	body := p.lock[len(tokenPrefix(p.tokens)):] // code || 6a || state
	r := f.expectSwap(p, 20_000, true)
	full := r.nextValidator.PubKey().Uncompressed()

	t.Run("control: 33 bytes", func(t *testing.T) {
		mustPass(t, f.run(t, p, f.swapCall(p, 20_000, true, r)))
	})
	for _, w := range []int{0, 1, 32, 34, 65} {
		t.Run(fmt.Sprintf("%d bytes", w), func(t *testing.T) {
			nk := full[:w]
			if w == 65 {
				nk = full
			} else if w == 34 {
				nk = append(append([]byte{}, r.nextValidator.PubKey().Compressed()...), 0x00)
			} else if w == 32 {
				nk = r.nextValidator.PubKey().Compressed()[:32]
			}
			ns := deltaStateOf(r.pool(p), affirmation)
			ns.val = nk
			newState := ns.raw()
			// What the script writes: prefix || script[:len(code)+1+33-w] || newState.
			keep := len(f.code) + 1 + 33 - w
			written := append(append(tokenPrefix(r.newTokens), body[:keep]...), newState...)
			naive := append(append(append(tokenPrefix(r.newTokens), f.code...), 0x6a), newState...)

			for _, shape := range []struct {
				name   string
				s      []byte
				pinned deltaWant
			}{
				{"continuation as poolScript builds it", written, deltaPass},
				{"continuation code || 6a || state", naive, deltaFail},
			} {
				c := f.swapCall(p, 20_000, true, r)
				c.args[1] = pushData(nk)
				c.outs[0].script = shape.s
				deltaCheck(t, fmt.Sprintf("Swap nextValidatorPubKey %d bytes, %s", w, shape.name), f.run(t, p, c), deltaFail, shape.pinned)
			}
			if deltaArt(t).head {
				return
			}
			// Pinned wrote it (validator-signed): can the LP still Close the
			// result? Its code region and state are misaligned by 33-w bytes.
			next := pool{bsv: r.newBsv, tokens: r.newTokens, lp: p.lp, validator: r.nextValidator, lock: written}
			deltaCheck(t, fmt.Sprintf("pinned: LP Close of the pool a %d-byte nextValidatorPubKey Swap wrote", w),
				f.run(t, next, f.closeCall(next)), deltaFail, deltaFail)
			// Nor with any _codePart cut the pin block's byte checks accept.
			nb := written[len(tokenPrefix(r.newTokens)):]
			for k := 3773; k < len(nb); k++ {
				if nb[k] == 0x6a && f.deltaRunCode(t, next, f.closeCall(next), nb[:k]) == nil {
					t.Errorf("DELTA-RECORD pinned | LP Close of the %d-byte-key pool passes with a %d-byte code part", w, k)
				}
			}
			t.Logf("DELTA-RECORD pinned | LP Close of the %d-byte-key pool: no code part works (LP funds stuck)", w)
		})
	}
}

// Rescind writes ValidatorPubKey as exactly 33 zero bytes.
func TestDeltaRescindWritesThirtyThreeZeroBytes(t *testing.T) {
	f := newFixture(t, 30, 5, 0)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	for _, w := range []int{33, 32, 34} {
		t.Run(fmt.Sprintf("%d zero bytes", w), func(t *testing.T) {
			c := f.rescindCall(p)
			s := deltaStateOf(p, affirmation)
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

// A pool deployed with a wrong-width fixed state field. HEAD's SDK refuses to
// build one; a hand-built one is spendable by whom? Close is the oracle (no
// continuation): it is signed with the key the reader forms at offset 8.
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := deltaStateOf(p, affirmation)
			tc.edit(&s)
			_, sdkErr := deltaSDKScript(t, s, assetID, f.deltaFees())
			deltaCheck(t, tc.name+": the SDK builds it", sdkErr, deltaFail, deltaPass)

			q := p
			q.lock = append(append(append(tokenPrefix(p.tokens), f.code...), 0x6a), s.raw()...)
			st := s.raw()
			formed := st[8:41]
			// Close signed by the LP; if the reader's LpPubKey is not the
			// LP's key no one can sign it.
			err := f.run(t, q, f.closeCall(q))
			if bytes.Equal(formed, p.lp.PubKey().Compressed()) {
				// HEAD: the reader's consumed-state check fails (the Affirmation
				// framing is read off by the width error). Pinned drops the rest.
				deltaCheck(t, tc.name+": LP Close", err, deltaFail, deltaPass)
			} else {
				t.Logf("reader's LpPubKey %x is not a key anyone holds", formed)
				deltaCheck(t, tc.name+": LP Close (reader's key is not the LP's)", err, deltaFail, deltaFail)
			}
			// Swap and Rescind with the continuation carrying the state as deployed.
			r := f.expectSwap(q, 20_000, true)
			sw := f.swapCall(q, 20_000, true, r)
			ns := s
			ns.tokens, ns.val = r.newTokens, r.nextValidator.PubKey().Compressed()
			sw.outs[0].script = append(append(append(tokenPrefix(r.newTokens), f.code...), 0x6a), ns.raw()...)
			deltaCheck(t, tc.name+": Swap", f.run(t, q, sw), deltaFail, deltaRecord)
			rs := f.rescindCall(q)
			rsS := s
			rsS.val = make([]byte, 33)
			rs.outs[0].script = append(append(append(tokenPrefix(q.tokens), f.code...), 0x6a), rsS.raw()...)
			deltaCheck(t, tc.name+": Rescind", f.run(t, q, rs), deltaFail, deltaRecord)
		})
	}
}

// --- C. Affirmation framing ---

func deltaAff(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(0x30 + i%7) // no 0x6a
	}
	return b
}

var deltaAffSizes = []int{0, 1, 70, 71, 72, 73, 75, 76, 127, 128, 129, 200, 255, 256, 600}

// The SDK's framing for an Affirmation of n bytes: a direct push below 76,
// PUSHDATA1 to 255, PUSHDATA2 above.
func TestDeltaAffirmationSDKFraming(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	for _, n := range deltaAffSizes {
		tail := f.deltaTail(t, deltaStateOf(p, deltaAff(n)))
		frame := tail[1+179 : len(tail)-n] // OP_RETURN, 179 bytes of fixed fields
		var want []byte
		switch {
		case n == 0:
			want = []byte{0x00}
		case n < 0x4c:
			want = []byte{byte(n)}
		case n <= 0xff:
			want = []byte{0x4c, byte(n)}
		default:
			want = []byte{0x4d, byte(n), byte(n >> 8)}
		}
		if !bytes.Equal(frame, want) || !bytes.HasSuffix(tail, deltaAff(n)) {
			t.Errorf("%d bytes: framing %x, want %x", n, frame, want)
		}
	}
}

// Swap (both directions) -> second Swap -> Rescind -> Close, and Close
// directly, for each Affirmation size; each continuation framed by the SDK.
func TestDeltaAffirmationSizesLifecycle(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	for _, n := range deltaAffSizes {
		aff := deltaAff(n)
		// HEAD: everything passes. Pinned reads the PUSHDATA1 length signed
		// (no <00> OP_CAT before OP_BIN2NUM): 0x80 is negative zero, so a
		// 128-byte Affirmation reads as empty (Close passes, Swap's
		// continuation no longer matches); 129..255 read negative and every
		// method aborts, Close included: the LP's funds are stuck.
		pinnedClose, pinnedSwap := deltaPass, deltaPass
		if n >= 128 && n <= 255 {
			pinnedSwap = deltaFail
			if n > 128 {
				pinnedClose = deltaFail
			}
		}
		at := func(p pool) pool {
			p.lock = f.deltaLock(t, deltaStateOf(p, aff))
			return p
		}
		t.Run(fmt.Sprintf("%d bytes", n), func(t *testing.T) {
			p := at(pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)})
			deltaCheck(t, fmt.Sprintf("Affirmation %d: Close", n), f.run(t, p, f.closeCall(p)), deltaPass, pinnedClose)
			for _, bsvIn := range []bool{true, false} {
				dir := map[bool]string{true: "bsv in", false: "tokens in"}[bsvIn]
				r1 := f.expectSwap(p, 20_000, bsvIn)
				p2 := at(r1.pool(p))
				c1 := f.swapCall(p, 20_000, bsvIn, r1)
				c1.outs[0].script = p2.lock
				if !bytes.HasSuffix(p2.lock, aff) || !bytes.HasSuffix(p.lock, aff) {
					t.Fatal("continuation does not end with the Affirmation")
				}
				err := f.run(t, p, c1)
				deltaCheck(t, fmt.Sprintf("Affirmation %d: Swap %s", n, dir), err, deltaPass, pinnedSwap)
				if err != nil {
					continue
				}
				r2 := f.expectSwap(p2, 7_000, !bsvIn)
				p3 := at(r2.pool(p2))
				c2 := f.swapCall(p2, 7_000, !bsvIn, r2)
				c2.outs[0].script = p3.lock
				deltaCheck(t, fmt.Sprintf("Affirmation %d: %s then a second Swap", n, dir), f.run(t, p2, c2), deltaPass, deltaPass)
				c3 := f.rescindCall(p3)
				p4 := p3
				p4.rescinded = true
				p4 = at(p4)
				c3.outs[0].script = p4.lock
				deltaCheck(t, fmt.Sprintf("Affirmation %d: %s, Swap, Rescind", n, dir), f.run(t, p3, c3), deltaPass, deltaPass)
				deltaCheck(t, fmt.Sprintf("Affirmation %d: %s, Swap, Rescind, Close", n, dir), f.run(t, p4, f.closeCall(p4)), deltaPass, deltaPass)
			}
		})
	}
}

// Bytes after the state on the spent pool: HEAD's reader asserts the state
// is consumed, so every method rejects. Pinned's reader drops the rest.
func TestDeltaTrailingBytesAfterState(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	base := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	for _, g := range []struct {
		name  string
		extra []byte
	}{{"one 0x00 byte", []byte{0x00}}, {"an extra push", []byte{0x02, 0xbe, 0xef}}} {
		t.Run(g.name, func(t *testing.T) {
			p := base
			clean := f.deltaLock(t, deltaStateOf(p, affirmation))
			p.lock = append(append([]byte{}, clean...), g.extra...)
			body := p.lock[len(tokenPrefix(p.tokens)):]
			for _, m := range f.deltaCalls(t, base, affirmation) {
				shapes := []struct {
					name   string
					s      []byte
					pinned deltaWant
				}{{"no continuation", nil, deltaPass}}
				if m.name != "Close" {
					// Pinned's poolScript: script[:len(code)+1+len(extra)] || new state.
					newState := m.c.outs[0].script[len(m.c.outs[0].script)-(len(clean)-len(tokenPrefix(p.tokens))-len(f.code)-1):]
					pre := m.c.outs[0].script[:len(m.c.outs[0].script)-len(newState)-len(f.code)-1]
					written := append(append(append([]byte{}, pre...), body[:len(f.code)+1+len(g.extra)]...), newState...)
					// Pinned writes the extra bytes into the continuation's
					// code region (scriptCode minus the new state's length).
					shapes = []struct {
						name   string
						s      []byte
						pinned deltaWant
					}{
						{"continuation as poolScript builds it", written, deltaPass},
						{"continuation without the extra bytes", m.c.outs[0].script, deltaFail},
					}
				}
				for _, sh := range shapes {
					c := m.c
					if sh.s != nil {
						c.outs = append([]out{}, m.c.outs...)
						c.outs[0].script = sh.s
					}
					name := fmt.Sprintf("trailing %s: %s, %s", g.name, m.name, sh.name)
					deltaCheck(t, name, f.run(t, p, c), deltaFail, sh.pinned)
				}
			}
		})
	}
}

// The Affirmation is carried unchanged: a continuation with it altered is
// not what Swap or Rescind wrote.
func TestDeltaAffirmationChangedInContinuation(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: key(t, 20)}
	p.lock = f.deltaLock(t, deltaStateOf(p, affirmation))
	flipped := append([]byte{}, affirmation...)
	flipped[10] ^= 0xff
	for _, alt := range []struct {
		name string
		aff  []byte
	}{
		{"one byte changed", flipped},
		{"one byte longer", append(append([]byte{}, affirmation...), 0x01)},
		{"one byte shorter", affirmation[:len(affirmation)-1]},
		{"emptied", []byte{}},
	} {
		for _, m := range f.deltaCalls(t, p, affirmation) {
			if m.name == "Close" {
				continue
			}
			t.Run(alt.name+": "+m.name, func(t *testing.T) {
				mustPass(t, f.run(t, p, m.c))
				bad := f.deltaCalls(t, p, alt.aff)
				for _, b := range bad {
					if b.name == m.name {
						deltaCheck(t, "Affirmation "+alt.name+" in "+m.name+" continuation", f.run(t, p, b.c), deltaFail, deltaFail)
					}
				}
			})
		}
	}
}

// How often the pinned validator theft (TestDeltaPinnedValidatorGrindsNextKey
// ThenStealsViaClose) works against an honestly affirmed pool: the cut at state
// byte 65 moves every field 66 bytes, so the Affirmation's framing is read
// from inside the real Affirmation, and whether that parses depends on the DER
// signature's bytes. 400 real DER signatures (the validator's, over random
// digests); the count is logged, and HEAD must reject every one.
func TestDeltaPinnedTheftRateOverRealAffirmations(t *testing.T) {
	f := newFixture(t, 30, 5, 10)
	val := deltaKeyWithByte(t, 24, 0x6a)
	const n = 400
	stolen := 0
	for i := 0; i < n; i++ {
		d := sha256.Sum256([]byte(fmt.Sprintf("affirmation %d", i)))
		sig, err := val.Sign(d[:])
		if err != nil {
			t.Fatal(err)
		}
		aff := sig.Serialize()
		p := pool{bsv: 1_000_000, tokens: 5_000_000, lp: key(t, 10), validator: val}
		p.lock = f.deltaLock(t, deltaStateOf(p, aff))
		body := p.lock[len(tokenPrefix(p.tokens)):]
		k := len(f.code) + 1 + 65
		steal := f.closeCall(p)
		steal.outs = []out{{p.bsv - 500, p2pkh(pkh(validatorIdentity))}, {1, tokenP2pkh(p.tokens, pkh(validatorIdentity))}}
		steal.signers = []*ec.PrivateKey{validatorIdentity}
		if f.deltaRunCode(t, p, steal, body[:k]) == nil {
			stolen++
		}
	}
	t.Logf("DELTA-RECORD %s | identity-key Close via codePart cut at state byte 65 over %d real DER affirmations | %d pass", deltaToolchain(t), n, stolen)
	if deltaArt(t).head && stolen != 0 {
		t.Fatalf("HEAD: %d of %d thefts passed", stolen, n)
	}
	// Deterministic (RFC 6979 signatures over fixed digests): 9 on the pin.
	if !deltaArt(t).head && stolen != 9 {
		t.Fatalf("pinned: %d of %d thefts passed, 9 expected", stolen, n)
	}
}
