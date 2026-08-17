package crdt

import "testing"

// Correctness 5 (second review): sameSliceRef's len==0 guard returned `true` for
// two distinct empty []any, but lib0 equalFlat compares array values by
// REFERENCE (JS `[] === []` is FALSE). So an empty-array-valued format attribute
// got its ContentFormat marker DROPPED where Yjs KEEPS it — a cross-impl
// item-chain / ToDelta divergence. The guard now returns `false` for the len==0
// case (matching ===), without panicking on the empty (no-index) slice.
//
// (Originally added by Copilot 8e6e93a to prove sameSliceRef does not PANIC on a
// non-nil empty []any; that no-panic guarantee is retained, only the equality
// verdict is corrected to be Yjs-faithful.)
func TestEqualAttrsEmptySliceValueNotEqual(t *testing.T) {
	a := MakeObject("arr", []any{})
	b := MakeObject("arr", []any{})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("equalAttrs panicked on empty []any value: %v", r)
		}
	}()
	// Two empty []any attribute values are NOT === in JS, so equalFlat reports
	// them unequal. (Faithful to lib0; the old code wrongly returned equal.)
	if equalAttrs(a, b) {
		t.Fatal("two empty []any attribute values must NOT be equal (lib0 [] === [] is false)")
	}

	// A populated vs empty must differ (and not panic).
	c := MakeObject("arr", []any{1})
	if equalAttrs(a, c) {
		t.Fatal("empty vs non-empty array must not be equal")
	}

	// nil vs nil (absent array) is still the same reference (the nil/nil branch,
	// not the len==0 branch) — exercised here to confirm the change is scoped to
	// the non-nil empty case only.
	d := MakeObject("arr", []any(nil))
	e := MakeObject("arr", []any(nil))
	if !equalAttrs(d, e) {
		t.Fatal("two nil []any attribute values should compare equal (nil === nil)")
	}
}
