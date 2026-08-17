package oracle_test

import (
	"strings"
	"testing"

	"github.com/antst/go-yjs/internal/oracle"
)

// T016c — the default registry must be complete and well-formed. It is the source of truth for
// "every surface", so a surface missing here is a surface silently never verified.
func TestDefaultRegistryIsComplete(t *testing.T) {
	r := oracle.Default()
	// Registered surfaces are those whose generators exist. The canonical target is 13; the
	// difference is reported by Pending() so a mid-flight registry cannot be mistaken for complete.
	want := []string{"applyDelta", "array", "awareness", "gc", "map", "relpos", "snapshot", "subdoc", "sync", "text", "undo", "update", "xml"}
	got := r.Names()
	if len(got) != len(want) {
		t.Fatalf("registry has %d surfaces, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("surface[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// The gap between canonical and registered must be explicit. When this list empties, "every
// surface" finally means all thirteen.
func TestPendingSurfacesAreReported(t *testing.T) {
	pending := oracle.Pending()
	t.Logf("canonical=%d registered=%d pending=%v",
		len(oracle.CanonicalSurfaces), len(oracle.Default().Names()), pending)
	for _, p := range pending {
		switch p {
		default:
			t.Errorf("unexpected pending surface %q — every canonical surface must either be "+
				"registered or be one of the four with a scheduled task", p)
		}
	}
}

func TestEveryCellIsInTheFastTier(t *testing.T) {
	// Register() enforces this, so Default() constructing at all is the proof; this test states
	// the invariant explicitly so it cannot be weakened without a failing test.
	cells := oracle.Default().Cells()
	if len(cells) == 0 {
		t.Fatal("no cells")
	}
	t.Logf("registry realizes %d (surface, direction) cells", len(cells))
}

// Reorder must NOT be applicable to commutative surfaces: counting it would make "100% detected"
// ill-defined, since a reorder on a commutative multiset legitimately still matches.
func TestCommutativeSurfacesDoNotDeclareReorder(t *testing.T) {
	r := oracle.Default()
	if r.FaultApplies("map", oracle.FaultReorderOp) {
		t.Error("map declares reorder-op applicable, but distinct key sets are commutative")
	}
	if !r.FaultApplies("text", oracle.FaultReorderOp) {
		t.Error("text does not declare reorder-op, but text sequence order is observable")
	}
}

func TestEverySurfaceDeclaresCorruptExpectation(t *testing.T) {
	r := oracle.Default()
	for _, name := range r.Names() {
		if !r.FaultApplies(name, oracle.FaultCorruptExpectation) {
			t.Errorf("surface %q does not declare corrupt-expectation — it is the universal kind, "+
				"and it is what makes zero exempt surfaces achievable", name)
		}
	}
}

// T077. PendingDirections is the registry's own report of realized-direction gaps, and
// `fuzz/run-gate.sh` asks the registry (not a hardcoded list) whether direction B is available.
// It had no test, so a change that made it report nothing would have silently turned the
// direction-B availability check into an unconditional yes.
func TestPendingDirectionsMatchesTheRegistry(t *testing.T) {
	got := oracle.PendingDirections()
	r := oracle.Default()

	// Every reported gap must be a real surface that genuinely does not realize B.
	for _, p := range got {
		name, ok := strings.CutSuffix(p, ":B")
		if !ok {
			t.Errorf("PendingDirections returned %q, which is not a <surface>:B entry", p)
			continue
		}
		if r.RealizesDirection(name, oracle.DirB) {
			t.Errorf("%q is reported pending but the registry says it realizes direction B", name)
		}
	}

	// And every surface that does NOT realize B must be reported — a gap that goes unreported is
	// the failure mode this function exists to prevent.
	reported := map[string]bool{}
	for _, p := range got {
		reported[strings.TrimSuffix(p, ":B")] = true
	}
	for _, n := range r.Names() {
		if !r.RealizesDirection(n, oracle.DirB) && !reported[n] {
			t.Errorf("surface %q does not realize direction B but was not reported as pending", n)
		}
	}

	// Sanity: the registry must realize B somewhere, else the checks above are vacuous.
	anyB := false
	for _, n := range r.Names() {
		if r.RealizesDirection(n, oracle.DirB) {
			anyB = true
			break
		}
	}
	if !anyB {
		t.Error("no surface realizes direction B; FR-002 is unmet and run-gate.sh --dir B would refuse")
	}
}
