//go:build integration

package regtest

import (
	"bytes"
	"fmt"
	"testing"
)

// The pool's state is fixed-width, so the artifact has no codePartLenSlots:
// the deployed code is the template with its constructor args spliced in and
// nothing else. A builder that does that without the SDK (constructor slots
// replaced by the args' pushes, and an older Rúnar's codeSepIndex slots set to
// OP_1) gets the SDK's exact bytes, whatever the fee rates' push widths. This
// test builds such code for {0, 0, 0}, {17, 0, 0} (the first rate pushed in
// two bytes) and {30, 5, 10}, asserts it equals the SDK's code, deploys a pair
// from the spliced bytes and spends it through Swap both ways, Rescind and
// Close.
func TestSplicedTemplateEqualsSDK(t *testing.T) {
	t.Parallel()
	c := compiled(t)
	t.Logf("artifact template %d bytes; %d constructor slots, %d codeSepIndex slots, %d codePartLenSlots",
		len(c.raw), len(c.tmpl.ConstructorSlots), len(c.tmpl.CodeSepIndexSlots), len(c.tmpl.CodePartLenSlots))
	if len(c.tmpl.CodePartLenSlots) != 0 {
		t.Fatalf("the artifact has codePartLenSlots: the state is not fixed-width for this compiler")
	}
	for _, fees := range [][3]int64{{0, 0, 0}, {17, 0, 0}, {30, 5, 10}} {
		t.Run(fmt.Sprintf("fees %d-%d-%d", fees[0], fees[1], fees[2]), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			pp, st := newParams(t, fees[0], fees[1], fees[2]), newPairState(t)
			code := handCode(t, c, pp)
			sdk := sdkCode(t, c, pp, st)
			if !bytes.Equal(code, sdk) {
				t.Fatalf("the spliced template (%d bytes) is not the SDK's code (%d bytes)", len(code), len(sdk))
			}
			pr := e.deployCode(pp, st, code, "deploy spliced template")
			e.swap(pr, planSwap(t, pr, 20_000, true))
			e.swap(pr, planSwap(t, pr, 20_000, false))
			e.rescind(pr, 0)
			e.close(pr)
			t.Logf("spliced template, %d bytes, equals the SDK's code and spends", len(code))
		})
	}
}
