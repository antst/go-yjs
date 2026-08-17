package oracle_test

import (
	"testing"

	"github.com/antst/go-yjs/internal/oracle"
)

// T007 — fault applicability. An inapplicable kind must be skipped EXPLICITLY, never silently
// counted as passing: "100% detected" is only well-defined if the denominator is the applicable
// set. A reorder on a commutative multiset legitimately still matches, so counting it as a
// detection failure would be wrong — and counting it as a pass without saying so would inflate
// the score.

func TestApplicabilityIsDeclaredPerSurfaceAndKind(t *testing.T) {
	r := oracle.NewRegistry()
	mustRegister(t, r, oracle.Surface{
		Name:       "map",
		Directions: []oracle.Direction{oracle.DirA},
		// A Y.Map key-set stream is a commutative multiset: reordering is not observable.
		Faults: []oracle.FaultKind{oracle.FaultCorruptExpectation, oracle.FaultDropOp},
		Tiers:  map[oracle.Direction]oracle.TierSet{oracle.DirA: {oracle.TierFast}},
	})

	if r.FaultApplies("map", oracle.FaultReorderOp) {
		t.Error("reorder-op reported applicable to a surface that did not declare it")
	}
	if !r.FaultApplies("map", oracle.FaultDropOp) {
		t.Error("drop-op reported inapplicable despite being declared")
	}
}

func TestUndetectedApplicableFaultIsAFailure(t *testing.T) {
	res := oracle.FaultResults{
		{Surface: "text", Kind: oracle.FaultDropOp, Applicable: true, Detected: true},
		{Surface: "text", Kind: oracle.FaultReorderOp, Applicable: true, Detected: false}, // blind spot
	}
	if err := res.Validate(); err == nil {
		t.Fatal("an APPLICABLE fault that went undetected must fail — that is a proven blind spot")
	}
}

func TestInapplicableFaultIsNotCountedAsDetection(t *testing.T) {
	res := oracle.FaultResults{
		{Surface: "map", Kind: oracle.FaultReorderOp, Applicable: false, Detected: false},
		{Surface: "map", Kind: oracle.FaultDropOp, Applicable: true, Detected: true},
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("an INAPPLICABLE fault must not fail the suite: %v", err)
	}
	applicable, detected := res.Score()
	if applicable != 1 || detected != 1 {
		t.Errorf("Score() = %d/%d, want 1/1 — the denominator is the APPLICABLE set, so an "+
			"inapplicable kind must not inflate it", detected, applicable)
	}
}

func TestSurfaceWithNoApplicableFaultsFails(t *testing.T) {
	res := oracle.FaultResults{
		{Surface: "snapshot", Kind: oracle.FaultReorderOp, Applicable: false, Detected: false},
	}
	if err := res.Validate(); err == nil {
		t.Fatal("a surface whose every fault kind is inapplicable has proven nothing; FR-008 " +
			"removed the exempt category, so this must fail rather than score 0/0 = pass")
	}
}

func mustRegister(t *testing.T, r *oracle.Registry, s oracle.Surface) {
	t.Helper()
	if err := r.Register(s); err != nil {
		t.Fatalf("register %s: %v", s.Name, err)
	}
}
