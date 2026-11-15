//go:build integration

package regtest

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/shruggr/skein-amm/pool/pair/regtest/internal/node"
)

// TestMain refuses anything but a regtest node, mines past coinbase maturity
// on a fresh chain, runs the suite, then prints every transaction the suite
// sent: size, unlocking sizes of the pool and vault inputs, fee and the
// node's verdict.
func TestMain(m *testing.M) {
	if err := node.EnsureRegtest(); err != nil {
		fmt.Fprintf(os.Stderr, "regtest node not reachable at %s: %v\nStart it with: <runar>/integration/regtest.sh start skein-pair (or ./run.sh)\n", node.URL(), err)
		os.Exit(1)
	}
	h, err := node.BlockCount()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if h < 300 {
		fmt.Fprintf(os.Stderr, "mining %d blocks (height %d)\n", 300-h, h)
		if err := node.Mine(300 - h); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	printReport()
	os.Exit(code)
}

func printReport() {
	report.Lock()
	defer report.Unlock()
	rows := append([]txStat{}, report.rows...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].test < rows[j].test })
	var b strings.Builder
	accepted, rejected, unexpected := 0, 0, 0
	fmt.Fprintf(&b, "\n=== transactions sent (fee rate %d sat/kB) ===\n", feeRate)
	fmt.Fprintf(&b, "%-56s %-52s %7s %6s %6s %6s %s\n", "test", "tx", "bytes", "pool", "vault", "fee", "node")
	for _, r := range rows {
		verdict := "accepted"
		switch {
		case r.accepted && r.reason != "":
			verdict = r.reason
			unexpected++
		case r.accepted:
			accepted++
		default:
			verdict = "REJECTED: " + r.reason
			rejected++
		}
		fmt.Fprintf(&b, "%-56s %-52s %7d %6d %6d %6d %s\n", trunc(r.test, 56), trunc(r.label, 52), r.size, r.poolUnlock, r.vaultUnlock, r.fee, verdict)
	}
	fmt.Fprintf(&b, "=== %d accepted, %d rejected, %d accepted although a rejection was expected ===\n", accepted, rejected, unexpected)
	fmt.Fprint(os.Stderr, b.String())
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "~"
}
