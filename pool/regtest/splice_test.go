//go:build integration

package regtest

import (
	"bytes"
	"fmt"
	"testing"
)

// The artifact template spliced without the SDK (each constructor slot's OP_0
// replaced by its arg's minimal push: the asset id and the three fee rates)
// is byte for byte the code the SDK deploys, and a pool deployed from it
// spends through every method. The fixed-width state leaves the template no
// deploy-time slot besides the constructor args (no codeSepIndexSlots, no
// codePartLenSlots). The fee sets cover each push form: OP_0, OP_5 and OP_10,
// and the one-byte pushes of 17 and 30.
func TestTemplateSpliceEqualsSDK(t *testing.T) {
	t.Parallel()
	c := compiled(t)
	t.Logf("artifact: template %d bytes, %d constructor slots, %d codeSepIndexSlots, %d codePartLenSlots",
		len(c.raw), len(c.tmpl.ConstructorSlots), len(c.tmpl.CodeSepIndexSlots), len(c.tmpl.CodePartLenSlots))
	for _, fees := range [][3]int64{{0, 0, 0}, {17, 0, 0}, {30, 5, 10}} {
		t.Run(fmt.Sprintf("fees %d-%d-%d", fees[0], fees[1], fees[2]), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			pp, st := newParams(t, fees[0], fees[1], fees[2]), newPoolState(t)
			code := handCode(t, c, pp)
			if sdk := sdkCode(t, c, pp, st); !bytes.Equal(code, sdk) {
				t.Fatalf("the template splice (%d bytes) is not the SDK's code (%d bytes)", len(code), len(sdk))
			}
			p := e.deployCode(pp, st, code, "deploy spliced template")
			e.swap(p, planSwap(t, p, 20_000, true))
			e.swap(p, planSwap(t, p, 20_000, false))
			e.rescind(p, 0)
			e.close(p)
		})
	}
}
