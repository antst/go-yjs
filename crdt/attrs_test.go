package crdt

import (
	"reflect"
	"testing"
)

// Regression guard for the US2 value-rep consolidation: pins GetOrNull's three-state behavior,
// Object insertion-order iteration, and — critically — the shallow-vs-deep comparator split
// (collapsing them would break Y.Text wire-parity and re-introduce the 002 awareness bug).

func TestAttrsGetOrNull(t *testing.T) {
	o := newObject()
	o.Set("present", 5)
	o.Set("explicitNull", Null)

	// absent → Null (yjs `o.get(k) ?? null` where get === undefined)
	if got := o.GetOrNull("absent"); !isNull(got) {
		t.Errorf("GetOrNull(absent) = %v, want Null", got)
	}
	// present-with-null → Null
	if got := o.GetOrNull("explicitNull"); !isNull(got) {
		t.Errorf("GetOrNull(explicitNull) = %v, want Null", got)
	}
	// present-with-value → value
	if got := o.GetOrNull("present"); got != 5 {
		t.Errorf("GetOrNull(present) = %v, want 5", got)
	}
	// GetOr is the DISTINCT accessor: absent → Go nil, NOT the Null sentinel (must not conflate).
	if got := o.GetOr("absent"); got != nil {
		t.Errorf("GetOr(absent) = %v, want Go nil", got)
	}
	if isNull(o.GetOr("absent")) {
		t.Error("GetOr(absent) must NOT be the Null sentinel — it is a different accessor from GetOrNull")
	}
}

func TestAttrsInsertionOrder(t *testing.T) {
	o := newObject()
	o.Set("z", 1)
	o.Set("a", 2)
	o.Set("m", 3)
	o.Set("a", 9) // updating an existing key must NOT change its position
	var keys []string
	o.Range(func(k string, _ any) { keys = append(keys, k) })
	if want := []string{"z", "a", "m"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("Range order = %v, want insertion order %v", keys, want)
	}
}

func TestEqualAttrsShallowVsDeep(t *testing.T) {
	inner1 := newObject()
	inner1.Set("x", 1)
	inner2 := newObject()
	inner2.Set("x", 1) // structurally equal to inner1, different backing store (different ref)

	a := newObject()
	a.Set("k", inner1)
	bRef := newObject()
	bRef.Set("k", inner1) // same nested ref as a
	bVal := newObject()
	bVal.Set("k", inner2) // structurally-equal but different nested ref

	// SHALLOW equalAttrs (Y.Text format attrs): nested compared by reference identity.
	if !equalAttrs(a, bRef) {
		t.Error("shallow equalAttrs: same nested ref must be equal")
	}
	if equalAttrs(a, bVal) {
		t.Error("shallow equalAttrs: different nested refs must be UNEQUAL (reference identity) — " +
			"collapsing to deep here breaks the Y.Text item chain")
	}

	// DEEP equalAttrsDeep (awareness): nested compared structurally.
	if !equalAttrsDeep(a, bVal) {
		t.Error("deep equalAttrsDeep: structurally-equal nested must be equal — " +
			"using shallow here re-introduces the 002 spurious-awareness-change bug")
	}

	// Null/absent semantics common to both.
	if !equalAttrs(Null, Null) {
		t.Error("equalAttrs(Null, Null) must be true")
	}
	if equalAttrs(Null, 1) {
		t.Error("equalAttrs(Null, 1) must be false")
	}
}
