package main

import (
	"bytes"
	"strings"
	"testing"
)

// run() is the CLI's whole behaviour, and `fuzz/run-gate.sh`, `.githooks/pre-push` and
// `.github/workflows/oracle.yml` all derive their surface lists and their seed-floor decisions from
// it. A wrong exit code or a silently empty list there makes the GATE wrong, not just the CLI, so
// the branches are tested rather than exercised incidentally by whichever flags a shell happens to
// pass.
func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCLIListsSurfacesAndCells(t *testing.T) {
	code, out, errs := runCLI(t)
	if code != 0 {
		t.Fatalf("bare invocation exit=%d stderr=%s", code, errs)
	}
	names := strings.Fields(out)
	if len(names) == 0 {
		t.Fatal("no surfaces listed; every caller derives its list from this and would silently run nothing")
	}

	code, cellsOut, _ := runCLI(t, "-cells")
	if code != 0 {
		t.Fatalf("-cells exit=%d", code)
	}
	cells := strings.Fields(cellsOut)
	if len(cells) < len(names) {
		t.Errorf("%d cells for %d surfaces; every surface realizes at least one direction", len(cells), len(names))
	}
	for _, c := range cells {
		if !strings.Contains(c, ":") {
			t.Errorf("cell %q is not surface:direction", c)
		}
	}
}

// SC-001a: every realized cell must run in the fast tier. This is the same property CI and the
// pre-push hook assert, checked here against the CLI they both call.
func TestCLIFastTierCoversEveryRealizedCell(t *testing.T) {
	_, all, _ := runCLI(t, "-cells")
	_, fast, _ := runCLI(t, "-cells", "-tier", "fast")
	if strings.Fields(all) == nil {
		t.Fatal("no cells at all")
	}
	if all != fast {
		t.Errorf("fast tier does not cover every realized cell:\n all=%v\nfast=%v",
			strings.Fields(all), strings.Fields(fast))
	}
}

func TestCLIFloorAndVolumeCheck(t *testing.T) {
	code, out, _ := runCLI(t, "-floor", "-tier", "scale")
	if code != 0 || strings.TrimSpace(out) != "10000" {
		t.Errorf("-floor -tier scale = %q (exit %d), want 10000", strings.TrimSpace(out), code)
	}
	if code, _, _ := runCLI(t, "-floor", "-tier", "fast"); code != 0 {
		t.Errorf("-floor -tier fast exit=%d", code)
	}

	// Below the floor must be a NON-ZERO exit: run-gate.sh keys off the exit code, so a
	// zero here would let an under-volume scale run report success.
	code, _, errs := runCLI(t, "-check-volume", "9999", "-tier", "scale")
	if code == 0 {
		t.Error("-check-volume 9999 -tier scale exited 0; the floor would not be enforced")
	}
	if !strings.Contains(errs, "floor") {
		t.Errorf("failure message does not mention the floor: %q", errs)
	}
	if code, _, _ := runCLI(t, "-check-volume", "10000", "-tier", "scale"); code != 0 {
		t.Errorf("exactly the floor was rejected (exit %d)", code)
	}
	// Tiers without a floor must accept any volume.
	if code, _, _ := runCLI(t, "-check-volume", "1", "-tier", "fast"); code != 0 {
		t.Errorf("fast tier rejected volume 1 (exit %d); only scale has a floor", code)
	}
}

func TestCLIPendingAndBadFlag(t *testing.T) {
	code, out, _ := runCLI(t, "-pending")
	if code != 0 {
		t.Fatalf("-pending exit=%d", code)
	}
	// Not asserting emptiness — that is surfaces_test's job. Asserting the CLI reports whatever
	// the registry says without failing, since run-gate.sh treats output here as informational.
	_ = out

	if code, _, _ := runCLI(t, "-not-a-flag"); code == 0 {
		t.Error("an unknown flag exited 0; a typo in a CI script would silently list everything")
	}
}
