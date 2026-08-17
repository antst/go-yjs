package oracle_test

import (
	"strings"
	"testing"

	"github.com/antst/go-yjs/internal/oracle"
)

// T004 — registry invariants. Both encode a feature-003 failure: a surface counted as covered
// without being exercised, and a gate enforced only nightly. Neither may be expressible.

func TestRegisterRejectsEmptyFaultSet(t *testing.T) {
	r := oracle.NewRegistry()
	err := r.Register(oracle.Surface{
		Name:       "text",
		Directions: []oracle.Direction{oracle.DirA},
		Faults:     nil, // the defect: no applicable fault kinds
		Tiers:      map[oracle.Direction]oracle.TierSet{oracle.DirA: {oracle.TierFast}},
	})
	if err == nil {
		t.Fatal("Register accepted a surface with an empty fault set; FR-008 removed the " +
			"'unproven but accepted' state — an unfaultable surface is an underbuilt harness")
	}
}

// Tier membership is keyed PER CELL, not per surface. A per-surface check passes while a
// direction is silently nightly-only, which is precisely the 003 failure mode.
func TestRegisterRejectsCellMissingFastTier(t *testing.T) {
	r := oracle.NewRegistry()
	err := r.Register(oracle.Surface{
		Name:       "text",
		Directions: []oracle.Direction{oracle.DirA, oracle.DirB},
		Faults:     []oracle.FaultKind{oracle.FaultCorruptExpectation},
		Tiers: map[oracle.Direction]oracle.TierSet{
			oracle.DirA: {oracle.TierFast},
			oracle.DirB: {oracle.TierScale}, // the defect: B is nightly-only
		},
	})
	if err == nil {
		t.Fatal("Register accepted a realized cell without the fast tier; SC-001a requires EVERY " +
			"(surface, direction) cell in the fast tier, and a per-surface check would have passed here")
	}
}

func TestRegisterRejectsRealizedDirectionWithNoTiers(t *testing.T) {
	r := oracle.NewRegistry()
	err := r.Register(oracle.Surface{
		Name:       "text",
		Directions: []oracle.Direction{oracle.DirA, oracle.DirB},
		Faults:     []oracle.FaultKind{oracle.FaultCorruptExpectation},
		Tiers:      map[oracle.Direction]oracle.TierSet{oracle.DirA: {oracle.TierFast}}, // B absent entirely
	})
	if err == nil {
		t.Fatal("Register accepted a realized direction with no tier entry at all")
	}
}

func TestRegisterAcceptsWellFormedSurface(t *testing.T) {
	r := oracle.NewRegistry()
	if err := r.Register(oracle.Surface{
		Name:       "text",
		Directions: []oracle.Direction{oracle.DirA, oracle.DirB},
		Faults:     []oracle.FaultKind{oracle.FaultCorruptExpectation, oracle.FaultDropOp},
		Tiers: map[oracle.Direction]oracle.TierSet{
			oracle.DirA: {oracle.TierFast, oracle.TierFull, oracle.TierScale},
			oracle.DirB: {oracle.TierFast, oracle.TierFull, oracle.TierScale},
		},
	}); err != nil {
		t.Fatalf("well-formed surface rejected: %v", err)
	}
	if got := len(r.Cells()); got != 2 {
		t.Errorf("Cells() = %d, want 2 — the registry enumerates (surface, direction) cells", got)
	}
}

// T005 — corpus guard. 003 closed the ABSENT case and left SET-BUT-EMPTY open, where a corpus
// yielding zero cases reported `cases=0` and PASSED with every strict flag set.

func TestCorpusGuardRejectsZeroCases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cases int
		path  string
	}{
		{"zero cases from a present file", 0, "/tmp/present-but-empty.ndjson"},
		{"absent path", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := oracle.CheckCorpus("text", tc.path, tc.cases); err == nil {
				t.Fatalf("CheckCorpus accepted %d cases (%q) — the gate would report green having "+
					"compared nothing", tc.cases, tc.path)
			}
		})
	}
}

func TestCorpusGuardNamesTheSurface(t *testing.T) {
	err := oracle.CheckCorpus("applyDelta", "/tmp/x.ndjson", 0)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !contains(err.Error(), "applyDelta") {
		t.Errorf("error %q does not name the surface; a failure that does not say WHICH surface "+
			"compared nothing is hard to action", err)
	}
}

func TestCorpusGuardAcceptsNonEmpty(t *testing.T) {
	if err := oracle.CheckCorpus("text", "/tmp/x.ndjson", 1); err != nil {
		t.Fatalf("CheckCorpus rejected a non-empty corpus: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// T071b / SC-001. The per-cell seed floor must be a mechanical invariant, not prose: before this
// the scale tier could run at any volume and still report success.
func TestTierFloorAndCellVolume(t *testing.T) {
	if got := oracle.TierFloor(oracle.TierScale); got != 10000 {
		t.Errorf("scale floor = %d, want 10000 (SC-001)", got)
	}
	// fast/full deliberately have no floor: fast is bounded by a wall-clock ceiling (SC-001a),
	// which a seed floor would directly contradict.
	for _, tier := range []oracle.Tier{oracle.TierFast, oracle.TierFull} {
		if got := oracle.TierFloor(tier); got != 0 {
			t.Errorf("oracle.TierFloor(%q) = %d, want 0 — a floor here would fight the fast-tier ceiling", tier, got)
		}
		if err := oracle.CheckCellVolume(tier, 1); err != nil {
			t.Errorf("oracle.CheckCellVolume(%q, 1) = %v, want nil", tier, err)
		}
	}
	if err := oracle.CheckCellVolume(oracle.TierScale, 9999); err == nil {
		t.Error("scale tier accepted 9999 seeds per cell; the floor is not enforced")
	}
	if err := oracle.CheckCellVolume(oracle.TierScale, 10000); err != nil {
		t.Errorf("scale tier rejected exactly the floor: %v", err)
	}
}

// T077. Register's rejections and the registry's lookup predicates were reachable but untested.
// Each rejection encodes a defect that actually shipped, so an accidentally-relaxed guard needs to
// fail here rather than surface as a silently hollow gate months later.
func TestRegisterRejectsMalformedSurfaces(t *testing.T) {
	valid := func() oracle.Surface {
		return oracle.Surface{
			Name:       "s",
			Directions: []oracle.Direction{oracle.DirA},
			Faults:     []oracle.FaultKind{oracle.FaultDropOp},
			Tiers: map[oracle.Direction]oracle.TierSet{
				oracle.DirA: {oracle.TierFast, oracle.TierFull, oracle.TierScale},
			},
		}
	}
	if err := oracle.NewRegistry().Register(valid()); err != nil {
		t.Fatalf("the control case must register cleanly, else the rejections below prove nothing: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*oracle.Surface)
		want   string
	}{
		{"no name", func(s *oracle.Surface) { s.Name = "" }, "no name"},
		{"no directions", func(s *oracle.Surface) { s.Directions = nil }, "realizes no directions"},
		{"no faults", func(s *oracle.Surface) { s.Faults = nil }, "fault"},
		{"cell missing the fast tier", func(s *oracle.Surface) {
			s.Tiers = map[oracle.Direction]oracle.TierSet{oracle.DirA: {oracle.TierScale}}
		}, "fast"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := valid()
			tc.mutate(&s)
			err := oracle.NewRegistry().Register(s)
			if err == nil {
				t.Fatalf("Register accepted a surface with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	// Registering the same name twice must fail: a silent overwrite would drop a surface's
	// generators while the count still looked right.
	r := oracle.NewRegistry()
	if err := r.Register(valid()); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(valid()); err == nil {
		t.Error("Register accepted a duplicate surface name")
	}
}

// The lookup predicates all answer "unknown surface" the same way — false, never a panic and never
// an accidental true. run-gate.sh and the CI membership check both branch on them.
func TestRegistryPredicatesOnUnknownSurface(t *testing.T) {
	r := oracle.Default()
	if r.FaultApplies("no-such-surface", oracle.FaultDropOp) {
		t.Error("FaultApplies said a fault applies to an unknown surface")
	}
	if r.RealizesDirection("no-such-surface", oracle.DirA) {
		t.Error("RealizesDirection said an unknown surface realizes direction A")
	}
	if r.CellInTier(oracle.Cell{Surface: "no-such-surface", Direction: oracle.DirA}, oracle.TierFast) {
		t.Error("CellInTier placed an unknown surface in the fast tier")
	}

	// And they must answer correctly for a REAL surface, so the checks above are not passing
	// merely because everything returns false.
	if !r.RealizesDirection("text", oracle.DirA) {
		t.Error("text does not realize direction A")
	}
	if r.RealizesDirection("undo", oracle.DirB) {
		t.Error("undo claims direction B, which no generator realizes")
	}
	if !r.CellInTier(oracle.Cell{Surface: "text", Direction: oracle.DirA}, oracle.TierFast) {
		t.Error("text:A is not in the fast tier")
	}
}

// The ultimate tier is 10x scale, for rare deep runs. Its floor must be enforced like the scale
// tier's, and every realized cell must run in it — a deep run that silently skipped a surface would
// be the most expensive way possible to learn nothing.
func TestUltimateTier(t *testing.T) {
	if got := oracle.TierFloor(oracle.TierUltimate); got != 100000 {
		t.Errorf("ultimate floor = %d, want 100000 (10x scale)", got)
	}
	if err := oracle.CheckCellVolume(oracle.TierUltimate, 99999); err == nil {
		t.Error("ultimate tier accepted a volume below its floor")
	}
	if err := oracle.CheckCellVolume(oracle.TierUltimate, 100000); err != nil {
		t.Errorf("ultimate tier rejected exactly its floor: %v", err)
	}
	r := oracle.Default()
	for _, c := range r.Cells() {
		if !r.CellInTier(c, oracle.TierUltimate) {
			t.Errorf("cell %s:%s does not run in the ultimate tier", c.Surface, c.Direction)
		}
	}
}
