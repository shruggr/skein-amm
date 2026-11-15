//go:build integration

package regtest

import (
	"bytes"
	"fmt"
	"sort"
	"testing"
)

// Item 10: a pool whose code was built WITHOUT the SDK, from the artifact
// template with the constructor slots spliced and the codeSepIndex slots set
// to OP_1. That is the template the project carries (gen/vectors' writeArtifact
// emits it to src/fixtures/pool_artifact.zig, and src/pool.zig's parse matches
// locks against it). Both predate Rúnar's codePartLenSlots (N-051):
// the template's verify_code_part_len fields hold a compile-time lower bound,
// and only the SDK writes the deployed code length into them. A pool deployed
// from such code pins SIZE(_codePart) to a length its real code part does not
// have.
//
// Each method gets its own freshly deployed hand-spliced pool, so one result
// cannot confound another, under two fee sets (the fee pushes' widths move
// the code's bytes). For each, two spends are tried:
//
//   - the honest spend (_codePart = the deployed code), and
//   - the recovery probe: _codePart = the deployed code truncated to the pinned
//     length. Rúnar authenticates _codePart as 61ab || scriptCode[0:SIZE-2], so
//     at the pinned SIZE this genuine prefix is the ONLY _codePart that can
//     pass; if the node refuses it too, no _codePart unlocks the method.
//
// Only when both are refused for Swap, Rescind and Close is the deposit
// unrecoverable; the test then passes and logs FUNDS LOCKED.
func TestHandSplicedTemplate_FundLock(t *testing.T) {
	t.Parallel()
	c := compiled(t)
	if len(c.tmpl.CodePartLenSlots) == 0 {
		// A Rúnar without N-051: the template has no deploy-time length field,
		// so the hand splice must be the SDK's code, and it spends.
		t.Logf("this Rúnar's artifact has no codePartLenSlots: the hand splice should equal the SDK")
		e := newEnv(t)
		pp, st := newParams(t, 30, 5, 10, 71), newPoolState(t)
		code := handCode(t, c, pp, false)
		if sdk := sdkCode(t, c, pp, st); !bytes.Equal(code, sdk) {
			t.Fatalf("hand-spliced code differs from the SDK's although there are no codePartLenSlots")
		}
		p := e.deployCode(pp, st, code, "deploy hand-spliced")
		e.swap(p, planSwap(t, p, 20_000, true))
		e.rescind(p, 0)
		e.close(p)
		return
	}
	t.Logf("artifact codePartLenSlots (template offsets): %+v", c.tmpl.CodePartLenSlots)

	for _, fees := range [][3]int64{{30, 5, 10}, {0, 0, 0}} {
		feeName := fmt.Sprintf("fees %d-%d-%d", fees[0], fees[1], fees[2])
		locked := map[string]bool{}
		for _, method := range []string{"swap", "rescind", "close"} {
			t.Run("unpatched/"+feeName+"/"+method, func(t *testing.T) {
				e := newEnv(t)
				pp, st := newParams(t, fees[0], fees[1], fees[2], 71), newPoolState(t)
				code := handCode(t, c, pp, false)
				sdk := sdkCode(t, c, pp, st)
				offs, pins := templateLenPins(t, c, pp)
				if len(code) != len(sdk) || bytes.Equal(code, sdk) {
					t.Fatalf("expected the hand splice to differ from the SDK's code only in the length fields (%d vs %d bytes)", len(code), len(sdk))
				}
				for i := range code {
					if code[i] != sdk[i] && !inField(i, offs) {
						t.Fatalf("hand splice differs from the SDK's code at byte %d, outside the length fields %v", i, offs)
					}
				}
				t.Logf("deployed code is %d bytes; the template's %d verify_code_part_len fields (at %v) pin %v", len(code), len(offs), offs, pins)

				p := e.deployCode(pp, st, code, "deploy hand-spliced (unpatched)")
				honest := handSplicedSpend(t, p, method)
				honest.label = fmt.Sprintf("hand-spliced %s, honest _codePart (%d bytes)", method, len(code))
				reason := e.reject(honest, consensusReject)
				t.Logf("HAND-SPLICED %s: honest spend REJECTED: %s", method, reason)

				for _, pin := range distinct(pins) {
					if pin >= len(code) {
						t.Logf("pin %d >= the code's %d bytes: no genuine prefix of that length exists", pin, len(code))
						continue
					}
					sc := separatorScriptCode(t, p.lock)
					probe := handSplicedSpend(t, p, method)
					probe.codePart = append([]byte{}, code[:pin]...)
					probe.label = fmt.Sprintf("hand-spliced %s, recovery probe _codePart = code[:%d]", method, pin)
					t.Logf("probe: scriptCode byte after the claimed code part is %#x (0x6a would satisfy the split check)", sc[pin-2])
					reason := e.reject(probe, consensusReject)
					t.Logf("HAND-SPLICED %s: recovery probe (_codePart truncated to the pinned %d bytes) REJECTED: %s", method, pin, reason)
				}
				if !t.Failed() {
					locked[method] = true
				}
			})
		}
		if locked["swap"] && locked["rescind"] && locked["close"] {
			t.Logf("FUNDS LOCKED (%s): a pool deployed from the hand-spliced template (codePartLenSlots left as the template has them) "+
				"cannot be spent by Swap, Rescind or Close, with the honest _codePart or the only other _codePart the pin admits; its deposit is unrecoverable", feeName)
		} else {
			t.Errorf("hand-spliced pool (%s): not every method was refused (locked: %v); see the subtests", feeName, locked)
		}
	}

	t.Run("patched with the deployed length: the SDK's bytes, and it spends", func(t *testing.T) {
		e := newEnv(t)
		pp, st := newParams(t, 30, 5, 10, 71), newPoolState(t)
		code := handCode(t, c, pp, true)
		if sdk := sdkCode(t, c, pp, st); !bytes.Equal(code, sdk) {
			t.Fatalf("hand splice + length patch (%d bytes) is not the SDK's code (%d bytes)", len(code), len(sdk))
		}
		p := e.deployCode(pp, st, code, "deploy hand-spliced + patched")
		e.swap(p, planSwap(t, p, 20_000, true))
		e.swap(p, planSwap(t, p, 20_000, false))
		e.rescind(p, 0)
		e.close(p)
	})
}

// handSplicedSpend is the honest spend of a pool by method name.
func handSplicedSpend(t *testing.T, p *pool, method string) *spend {
	switch method {
	case "swap":
		return swapSpend(p, planSwap(t, p, 20_000, true))
	case "rescind":
		return rescindSpend(t, p, 0)
	default:
		return closeSpend(t, p)
	}
}

func inField(i int, offs []int) bool {
	for _, o := range offs {
		if i >= o && i < o+4 {
			return true
		}
	}
	return false
}

func distinct(xs []int) []int {
	m := map[int]bool{}
	var out []int
	for _, x := range xs {
		if !m[x] {
			m[x] = true
			out = append(out, x)
		}
	}
	sort.Ints(out)
	return out
}
