package oracle

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// CoverageReport records which public operations the generators actually invoke.
//
// Operations are DERIVED from the type's method set, never from a hand-maintained list: a curated
// list decays silently the moment someone adds a method, and nothing fails when it does. That decay
// is the exact failure mode FR-005a exists to prevent, so the report must not reproduce it.
//
// Derivation is by reflection over instances the CALLER passes in. This package must not import the
// root package — the differential tests are `package y_crdt`, so importing it here would be an
// import cycle in the test binary (research R7).
type CoverageReport struct {
	ops      map[string]map[string]bool // surface -> operation -> exercised
	surfaces []string
	mappings map[string]OpMapping // surface -> generator op name -> Go methods (FR-005a)
}

func NewCoverageReport() *CoverageReport {
	return &CoverageReport{ops: make(map[string]map[string]bool)}
}

// excludedOps is the inclusion predicate's negative half, stated in code so a method's inclusion is
// decidable rather than argued. These are exported but are not user-facing CONTENT operations:
// lifecycle, observation registration, and the internal content family.
var excludedOps = map[string]bool{
	// lifecycle
	"Integrate": true, "Destroy": true, "Copy": true, "Clone": true,
	// observation registration
	"Observe": true, "Unobserve": true, "ObserveDeep": true, "UnobserveDeep": true,
	"CallObserver": true, "On": true, "Off": true, "Emit": true, "HasObserver": true,
	// internal content / encoding family
	"Write": true, "Read": true, "GetRef": true, "Splice": true, "MergeWith": true,
	"IsCountable": true, "GetContent": true, "GetLength": true,
	// internal plumbing exposed on the abstract type
	"SetMap": true, "GetMap": true, "SetStartItem": true, "StartItem": true,
	"SetLength": true, "GetDoc": true, "GetItem": true, "GetSearchMarker": true,
	"SetSearchMarker": true, "GetParent": true, "SetParent": true,
	// Exported only to satisfy the interface across files, not for callers: these return
	// *Item / *EventHandler / internal structures rather than document content.
	"First": true, "Parent": true, "UpdateLength": true, "GetEH": true, "GetDEH": true,
	// DOM interop: meaningful only in a browser, so there is nothing to compare.
	"ToDOM": true,
}

// DeriveFrom reflects over an instance's method set and records the content operations it exposes.
func (c *CoverageReport) DeriveFrom(surface string, instance any) {
	if _, ok := c.ops[surface]; !ok {
		c.ops[surface] = make(map[string]bool)
		c.surfaces = append(c.surfaces, surface)
	}
	t := reflect.TypeOf(instance)
	if t == nil {
		return
	}
	for i := 0; i < t.NumMethod(); i++ {
		name := t.Method(i).Name
		if excludedOps[name] {
			continue
		}
		if _, seen := c.ops[surface][name]; !seen {
			c.ops[surface][name] = false
		}
	}
}

// Operations lists the derived operations for a surface, sorted.
func (c *CoverageReport) Operations(surface string) []string {
	out := make([]string, 0, len(c.ops[surface]))
	for op := range c.ops[surface] {
		out = append(out, op)
	}
	sort.Strings(out)
	return out
}

// MarkExercised records that a generator invoked an operation.
func (c *CoverageReport) MarkExercised(surface, op string) {
	if m, ok := c.ops[surface]; ok {
		if _, known := m[op]; known {
			m[op] = true
		}
	}
}

// Missing lists derived operations no generator invoked, as "surface.Operation".
func (c *CoverageReport) Missing() []string {
	var out []string
	for _, s := range c.surfaces {
		for op, done := range c.ops[s] {
			if !done {
				out = append(out, s+"."+op)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Validate fails while any derived operation is unexercised (SC-003).
func (c *CoverageReport) Validate() error {
	missing := c.Missing()
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("public operations never exercised by any generator: %s",
		strings.Join(missing, ", "))
}

// OpMapping declares how a GENERATOR's op name relates to the Go method it drives.
//
// FR-005a exists because `Exercised` previously had no defined source: the differential tests
// hand-listed which Go methods they believed the generators reached, and nothing checked that
// belief. A generator op renamed on the JS side, or a Go method the list simply forgot, both left
// the report claiming coverage that did not exist — the same shape as feature 003's green gate.
//
// The join is declared here and checked in BOTH directions:
//
//   - every generator op name must map to a Go method that actually exists on the surface's type
//     (an op driving nothing is a generator whose work the report silently ignored), and
//   - every derived Go method must be named by at least one mapping entry (a method no op maps to
//     is a public operation no generator can reach).
//
// One generator op may legitimately drive several Go methods (a single `insert` op exercises both
// Insert and the Length read that positions it), so the value is a list.
type OpMapping map[string][]string

// DeclareOpMapping attaches a generator-op → Go-method mapping to a surface.
func (c *CoverageReport) DeclareOpMapping(surface string, m OpMapping) {
	if c.mappings == nil {
		c.mappings = make(map[string]OpMapping)
	}
	c.mappings[surface] = m
}

// MarkExercisedByOp records a GENERATOR op name, resolving it through the declared mapping.
// It returns an error for an op the mapping does not know, rather than silently ignoring it as
// MarkExercised does for an unknown method — an unmapped op is precisely the drift FR-005a targets.
func (c *CoverageReport) MarkExercisedByOp(surface, genOp string) error {
	m, ok := c.mappings[surface]
	if !ok {
		return fmt.Errorf("surface %q has no declared op mapping; op %q cannot be attributed",
			surface, genOp)
	}
	methods, ok := m[genOp]
	if !ok {
		return fmt.Errorf("surface %q: generator op %q maps to no Go method — either the "+
			"generator gained an op the mapping does not know, or an op was renamed",
			surface, genOp)
	}
	for _, method := range methods {
		c.MarkExercised(surface, method)
	}
	return nil
}

// ValidateMapping checks the declared mapping against the DERIVED method set, in both directions.
func (c *CoverageReport) ValidateMapping(surface string) error {
	m, ok := c.mappings[surface]
	if !ok {
		return fmt.Errorf("surface %q has no declared op mapping", surface)
	}
	derived := make(map[string]bool, len(c.ops[surface]))
	for op := range c.ops[surface] {
		derived[op] = true
	}
	if len(derived) == 0 {
		return fmt.Errorf("surface %q has no derived operations; DeriveFrom was never called, "+
			"so this check would pass vacuously", surface)
	}

	var problems []string

	// Direction 1: a mapping entry naming a method that does not exist on the type.
	mapped := make(map[string]bool)
	for genOp, methods := range m {
		for _, method := range methods {
			if !derived[method] {
				problems = append(problems, fmt.Sprintf(
					"op %q maps to method %q, which is not a derived operation of %q",
					genOp, method, surface))
			}
			mapped[method] = true
		}
	}

	// Direction 2: a derived method no mapping entry names.
	for method := range derived {
		if !mapped[method] {
			problems = append(problems, fmt.Sprintf(
				"method %q is a public operation of %q that no generator op maps to",
				method, surface))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("op mapping for %q is not exhaustive:\n  %s",
			surface, strings.Join(problems, "\n  "))
	}
	return nil
}
