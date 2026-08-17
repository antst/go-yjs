// Command surfaces prints the registry so shell callers derive their surface list from it rather
// than keeping a parallel copy.
//
// This exists because `run-gate.sh` hardcoded `ALL_SURFACES="text array map xml applyDelta update"`.
// A shell literal cannot list surfaces added later, so registering a surface while the entrypoint
// silently never ran it was possible — the feature-003 hollow gate in a new place. Deriving the
// list makes that drift impossible rather than merely noticeable.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/antst/go-yjs/internal/oracle"
)

func main() {
	if code := run(os.Args[1:], os.Stdout, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// run is main's body with the process boundary factored out, so the flag handling is reachable from
// a test. `go run` from a shell script covers only the paths that script happens to take, which is
// how the CLI ended up with none of its branches exercised by `go test`.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("surfaces", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cells := fs.Bool("cells", false, "print realized (surface, direction) cells instead of surface names")
	tier := fs.String("tier", "", "restrict to surfaces/cells running in this tier (fast|full|scale)")
	pending := fs.Bool("pending", false, "print canonical surfaces that have no generator yet")
	checkVolume := fs.Int("check-volume", -1, "exit non-zero if this per-cell seed volume is below the -tier floor (SC-001)")
	floor := fs.Bool("floor", false, "print the per-cell seed floor for -tier")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *floor {
		_, _ = fmt.Fprintln(stdout, oracle.TierFloor(oracle.Tier(*tier)))
		return 0
	}
	if *checkVolume >= 0 {
		if err := oracle.CheckCellVolume(oracle.Tier(*tier), *checkVolume); err != nil {
			_, _ = fmt.Fprintln(stderr, "ERROR:", err)
			return 1
		}
		return 0
	}
	if *pending {
		for _, p := range oracle.Pending() {
			_, _ = fmt.Fprint(stdout, p, " ")
		}
		return 0
	}

	r := oracle.Default()
	if *cells {
		for _, c := range r.Cells() {
			if *tier != "" && !r.CellInTier(c, oracle.Tier(*tier)) {
				continue
			}
			_, _ = fmt.Fprintf(stdout, "%s:%s\n", c.Surface, c.Direction)
		}
		return 0
	}
	for _, n := range r.Names() {
		_, _ = fmt.Fprintln(stdout, n)
	}
	return 0
}
