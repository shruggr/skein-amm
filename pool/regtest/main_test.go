//go:build integration

package regtest

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/shruggr/skein-amm/pool/regtest/internal/node"
)

// TestMain refuses anything but a regtest node, mines past coinbase maturity
// on a fresh chain, runs the suite, then prints every transaction the suite
// sent: size, fee and the node's verdict.
func TestMain(m *testing.M) {
	if err := node.EnsureRegtest(); err != nil {
		fmt.Fprintf(os.Stderr, "regtest node not reachable at %s: %v\nStart it with: <runar>/integration/regtest.sh start skein-amm (or ./run.sh)\n", node.URL(), err)
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
	fmt.Fprintf(&b, "\n=== transactions sent (fee rate %d sat/kB) ===\n", feeRate)
	fmt.Fprintf(&b, "%-58s %-46s %7s %6s %7s %s\n", "test", "tx", "bytes", "unlock", "fee", "node")
	for _, r := range rows {
		verdict := "accepted"
		if !r.accepted || r.reason != "" {
			verdict = "REJECTED: " + r.reason
			if r.accepted {
				verdict = r.reason
			}
		}
		fmt.Fprintf(&b, "%-58s %-46s %7d %6d %7d %s\n", trunc(r.test, 58), trunc(r.label, 46), r.size, r.unlockLen, r.fee, verdict)
	}
	fmt.Fprint(os.Stderr, b.String())
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "~"
}
