package oracle

import (
	"fmt"
	"sort"
	"strings"
)

// FaultKind is a deliberate corruption injected to prove a surface would report a divergence.
type FaultKind string

const (
	FaultDropOp      FaultKind = "drop-op"
	FaultDupOp       FaultKind = "dup-op"
	FaultReorderOp   FaultKind = "reorder-op"
	FaultPermuteAttr FaultKind = "permute-attr"
	// FaultCorruptExpectation perturbs the expected artefact itself. It is the universal kind:
	// anything the oracle compares — encoded bytes, rendered text, a delta, an event payload — can
	// be perturbed, which is what makes "zero exempt surfaces" achievable rather than aspirational.
	FaultCorruptExpectation FaultKind = "corrupt-expectation"
)

// AllFaultKinds is the canonical list. `data-model.md` mirrors it; this is the source of truth.
var AllFaultKinds = []FaultKind{
	FaultDropOp, FaultDupOp, FaultReorderOp, FaultPermuteAttr, FaultCorruptExpectation,
}

// FaultResult records one injection attempt.
//
// Applicable is declared per (surface, kind), not inferred. Some kinds are genuinely meaningless on
// some surfaces — a reorder on a commutative multiset legitimately still matches — so counting such
// a case as a detection failure would be wrong, and counting it as a pass without saying so would
// inflate the score. Recording applicability explicitly keeps "100% detected" well defined.
type FaultResult struct {
	Surface    string
	Kind       FaultKind
	Applicable bool
	Detected   bool
}

// FaultResults is one run's injections across every surface.
type FaultResults []FaultResult

// Validate fails when the run proves less than it appears to.
func (rs FaultResults) Validate() error {
	var blind []string
	applicableBySurface := map[string]int{}
	seenSurface := map[string]bool{}

	for _, r := range rs {
		seenSurface[r.Surface] = true
		if !r.Applicable {
			continue // skipped explicitly — never counted as a detection
		}
		applicableBySurface[r.Surface]++
		if !r.Detected {
			blind = append(blind, fmt.Sprintf("%s/%s", r.Surface, r.Kind))
		}
	}

	// An applicable fault that went undetected is a proven blind spot.
	if len(blind) > 0 {
		sort.Strings(blind)
		return fmt.Errorf("undetected applicable faults (proven blind spots): %s",
			strings.Join(blind, ", "))
	}

	// A surface whose every kind was inapplicable has proven NOTHING, and would otherwise score
	// 0/0 and read as a pass. FR-008 removed the exempt category, so this fails.
	var empty []string
	for s := range seenSurface {
		if applicableBySurface[s] == 0 {
			empty = append(empty, s)
		}
	}
	if len(empty) > 0 {
		sort.Strings(empty)
		return fmt.Errorf("surfaces with no applicable fault kinds: %s — 0/0 is not a pass; an "+
			"unfaultable surface is an underbuilt harness (FR-008)", strings.Join(empty, ", "))
	}
	return nil
}

// Score returns (applicable, detected). The denominator is the APPLICABLE set, so an inapplicable
// kind can neither inflate nor deflate it.
func (rs FaultResults) Score() (applicable, detected int) {
	for _, r := range rs {
		if !r.Applicable {
			continue
		}
		applicable++
		if r.Detected {
			detected++
		}
	}
	return applicable, detected
}
