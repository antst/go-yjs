// Package oracle is the shared core of the differential verification harness.
//
// It is `internal/` deliberately: the harness is verification machinery, not library API, and a
// top-level package would publish it to consumers (Constitution II). It must NOT import the root
// package — the differential tests are `package y_crdt`, so a root import here would be an import
// cycle in the test binary. Callers pass what the harness needs to inspect.
//
// Its purpose is Principle VII: thirteen surfaces sharing one harness rather than thirteen
// near-duplicate harnesses.
package oracle

import (
	"fmt"
	"sort"
)

// Direction is which implementation generates and which verifies.
type Direction string

const (
	// DirA — the reference generates ops, this library replays them natively, encoded state compared.
	DirA Direction = "A"
	// DirB — this library generates and encodes, the reference decodes and re-encodes, bytes
	// compared. Not a mirror of DirA: it exercises this library's CONSTRUCTORS, which DirA
	// structurally cannot reach because DirA only ever applies encoded updates.
	DirB Direction = "B"
)

// Tier is when a cell runs.
type Tier string

const (
	TierFast  Tier = "fast"  // every change proposal
	TierFull  Tier = "full"  // before acceptance into the mainline
	TierScale Tier = "scale" // scheduled
	// TierUltimate is the rare, deliberately-invoked deep run: 10x the scale tier. Its value is
	// narrow and MEASURED, not assumed — see TierFloor's note. It exists for rare op interleavings,
	// not as a substitute for widening what the generators emit.
	TierUltimate Tier = "ultimate" // on demand
)

// TierSet is the set of tiers one cell runs in.
type TierSet []Tier

func (ts TierSet) has(t Tier) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

// Surface is a distinct behavioural area under comparison.
//
// Tiers is keyed PER DIRECTION, not a single per-surface set. That is load-bearing: at surface
// granularity a surface registered fast-tier for direction A only would satisfy the invariant while
// direction B ran nightly-only — the exact shape of the feature-003 hollow gate, in the very check
// written to prevent it.
type Surface struct {
	Name       string
	Directions []Direction
	Faults     []FaultKind
	Tiers      map[Direction]TierSet
}

// Cell is one realized (surface, direction) pair — the unit the seed floor and fast-tier
// membership are both measured in.
type Cell struct {
	Surface   string
	Direction Direction
}

// Registry holds every surface under comparison. Any check phrased "for every surface" iterates
// this rather than a local list, so a surface cannot be covered in one place and forgotten in
// another.
type Registry struct {
	surfaces map[string]Surface
	order    []string // registration order, so reports are stable
}

func NewRegistry() *Registry {
	return &Registry{surfaces: make(map[string]Surface)}
}

// Register validates and adds a surface. The two rejections below are not defensive coding; each
// encodes a defect that actually shipped.
func (r *Registry) Register(s Surface) error {
	if s.Name == "" {
		return fmt.Errorf("surface has no name")
	}
	if _, dup := r.surfaces[s.Name]; dup {
		return fmt.Errorf("surface %q registered twice", s.Name)
	}
	if len(s.Directions) == 0 {
		return fmt.Errorf("surface %q realizes no directions", s.Name)
	}

	// FR-008: there is no "unproven but accepted" state. A surface that resists fault injection
	// indicates an underbuilt harness, not an unfaultable surface — anything producing output
	// comparable against the reference can be faulted by corrupting the expected value.
	if len(s.Faults) == 0 {
		return fmt.Errorf("surface %q declares no applicable fault kinds: an unfaultable surface is "+
			"an underbuilt harness, not an exemption (FR-008)", s.Name)
	}

	// SC-001a: EVERY realized cell must run in the fast tier. Checked per cell precisely because a
	// per-surface check would pass while a direction was silently nightly-only.
	for _, d := range s.Directions {
		tiers, ok := s.Tiers[d]
		if !ok {
			return fmt.Errorf("surface %q realizes direction %s but declares no tiers for it", s.Name, d)
		}
		if !tiers.has(TierFast) {
			return fmt.Errorf("surface %q cell (direction %s) is not in the fast tier: a cell "+
				"enforced only in a slower tier is not enforced (FR-022/SC-001a)", s.Name, d)
		}
	}

	r.surfaces[s.Name] = s
	r.order = append(r.order, s.Name)
	return nil
}

// Cells enumerates every realized (surface, direction) pair in registration order.
func (r *Registry) Cells() []Cell {
	var out []Cell
	for _, name := range r.order {
		s := r.surfaces[name]
		for _, d := range s.Directions {
			out = append(out, Cell{Surface: name, Direction: d})
		}
	}
	return out
}

// Names returns the registered surface names, sorted.
func (r *Registry) Names() []string {
	out := append([]string(nil), r.order...)
	sort.Strings(out)
	return out
}

// FaultApplies reports whether a fault kind was declared applicable to a surface. Applicability is
// declared, never inferred, so an inapplicable kind is skipped explicitly rather than silently
// counted as a pass.
func (r *Registry) FaultApplies(surface string, k FaultKind) bool {
	s, ok := r.surfaces[surface]
	if !ok {
		return false
	}
	for _, f := range s.Faults {
		if f == k {
			return true
		}
	}
	return false
}

// CheckCorpus fails when a surface has nothing to compare.
//
// Feature 003 closed the ABSENT-corpus case and left set-but-empty open: a corpus yielding zero
// cases reported `cases=0 pass=0 fail=0` and PASSED, with ORACLE_REQUIRED=1 and every strict flag
// set. Both are the same defect — a gate reporting success having compared nothing — so both fail
// here, and the message names the surface so the failure is actionable.
func CheckCorpus(surface, path string, cases int) error {
	if cases > 0 {
		return nil
	}
	where := path
	if where == "" {
		where = "<no corpus path given>"
	}
	return fmt.Errorf("surface %q: corpus %s yielded %d cases — the gate compared NOTHING; "+
		"regenerate it rather than reading this run as green", surface, where, cases)
}

// CellInTier reports whether a realized cell runs in the given tier.
func (r *Registry) CellInTier(c Cell, t Tier) bool {
	s, ok := r.surfaces[c.Surface]
	if !ok {
		return false
	}
	return s.Tiers[c.Direction].has(t)
}

// RealizesDirection reports whether a surface declares the given direction realized.
func (r *Registry) RealizesDirection(surface string, d Direction) bool {
	s, ok := r.surfaces[surface]
	if !ok {
		return false
	}
	for _, x := range s.Directions {
		if x == d {
			return true
		}
	}
	return false
}

// TierFloor is the minimum number of seeds each realized cell must receive in a tier.
//
// SC-001 sets a >=10,000-seed floor per realized cell at the scale tier. Until T071b that floor had
// neither an in-code invariant nor a CI assertion — unlike fast-tier cell MEMBERSHIP, which has
// both — so it rested entirely on a human reading recorded volumes and noticing a shortfall. That
// is the same shape as the feature-003 failure: a gate that is green because nobody checked, not
// because the property holds.
//
// The fast and full tiers have no floor here on purpose: fast is bounded by a wall-clock ceiling
// (SC-001a) and tunes seed weights to fit it, so a floor would be in direct tension with it.
// The ultimate tier's floor is 10x the scale tier's. Be clear-eyed about what that buys: measured
// on the text surface, going from 1 000 to 10 000 seeds covered ZERO additional statements, and
// 1 000 to 100 000 covered exactly TWO (a search-marker fixup during integration — a genuinely rare
// interleaving). Seed volume samples the space the GENERATORS define; it does not widen it. This
// tier is for that long tail, and nothing else.
func TierFloor(t Tier) int {
	switch t {
	case TierScale:
		return 10000
	case TierUltimate:
		return 100000
	default:
		return 0
	}
}

// CheckCellVolume fails when a tier run would give a realized cell fewer seeds than its floor.
// perCell is the volume each selected cell actually receives, so a caller that narrows the surface
// selection (and thereby raises per-cell volume) is judged on what it really ran.
func CheckCellVolume(t Tier, perCell int) error {
	floor := TierFloor(t)
	if floor == 0 || perCell >= floor {
		return nil
	}
	return fmt.Errorf(
		"tier %q gives each realized cell %d seeds, below the %d-seed floor (SC-001); "+
			"raise --seeds or narrow --surface so every cell that RUNS meets the floor",
		t, perCell, floor)
}
