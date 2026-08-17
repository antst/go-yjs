package crdt

import "testing"

// equal_attrs_review_test.go reproduces FINDING 3 from the full code-review of
// the Go Yjs v2 codec (PR antst/y-crdt#2): equalAttrs / equalObjects diverged
// from Yjs's equalAttrs.
//
// Yjs (src/types/YText.js):
//
//	const equalAttrs = (a, b) =>
//	  a === b ||
//	  (typeof a === 'object' && typeof b === 'object' && a && b && object.equalFlat(a, b))
//
// and lib0 object.equalFlat is SHALLOW:
//
//	equalFlat = (a, b) => a === b ||
//	  (size(a) === size(b) && every(a, (val, key) =>
//	     (val !== undefined || hasProperty(b, key)) && equalityTrait.equals(b[key], val)))
//
// where equalityTrait.equals is strict === (no recursion) for the JSON value
// domain. So:
//   - primitives compare by value;
//   - a flat object compares order-INDEPENDENTLY (same keyset, each value ===);
//   - a NESTED object / array value compares by === — i.e. REFERENCE identity,
//     NOT deep structural equality. Two distinct-but-equal nested instances are
//     NOT equal in Yjs;
//   - a TOP-LEVEL array compares element-wise (===), so distinct arrays of equal
//     primitives ARE equal, but distinct arrays of nested objects are NOT.
//
// The old Go equalObjects instead (a) recursed structurally into nested Objects
// and (b) routed nested-objects-inside-slices through reflect.DeepEqual — both
// deep, order-sensitive in the slice case — three inconsistent rules. For an
// Object-valued format attribute this flips formatText / minimizeAttributeChanges'
// marker-drop decision vs Yjs, producing a different Y.Text item chain.
//
// The verdicts asserted below were captured from real lib0 equalAttrs (node).

func TestEqualAttrsMatchesYjsEqualFlat(t *testing.T) {
	// shared backing instances for the same-reference (===) cases.
	sharedInner := MakeObject("x", 1)
	sharedArr := []any{1, 2, 3}

	cases := []struct {
		name string
		a, b any
		want bool
	}{
		// --- primitives: value equality -------------------------------------
		{"string equal", "red", "red", true},
		{"string differ", "red", "blue", false},
		{"bool equal", true, true, true},
		{"bool differ", true, false, false},
		{"int equal", 5, 5, true},
		{"int differ", 5, 6, false},
		{"null equal", Null, Null, true},
		{"nil equal", nil, nil, true},

		// --- flat object: order-INDEPENDENT shallow -------------------------
		{"flat obj same order", MakeObject("a", 1, "b", 2), MakeObject("a", 1, "b", 2), true},
		// {a:1,b:2} vs {b:2,a:1} are SET-equal at the top level (the canonical
		// order-independence case the prompt calls out).
		{"flat obj diff order", MakeObject("a", 1, "b", 2), MakeObject("b", 2, "a", 1), true},
		{"flat obj diff value", MakeObject("a", 1, "b", 2), MakeObject("a", 1, "b", 3), false},
		{"flat obj diff size", MakeObject("a", 1), MakeObject("a", 1, "b", 2), false},

		// --- nested object-valued attribute: Yjs === is REFERENCE identity ---
		// Distinct {x:1} instances are NOT equal in Yjs (the core divergence:
		// the old Go deep-recursion returned true here).
		{"nested obj-valued attr, distinct inner", MakeObject("nested", MakeObject("x", 1)), MakeObject("nested", MakeObject("x", 1)), false},
		// Same inner reference IS equal.
		{"nested obj-valued attr, same inner ref", MakeObject("nested", sharedInner), MakeObject("nested", sharedInner), true},

		// --- array nested in object: === reference, distinct slices NOT equal -
		{"array-in-obj, distinct slices", MakeObject("rgb", []any{1, 2, 3}), MakeObject("rgb", []any{1, 2, 3}), false},
		{"array-in-obj, same slice ref", MakeObject("rgb", sharedArr), MakeObject("rgb", sharedArr), true},

		// --- TOP-LEVEL array: element-wise shallow (===) --------------------
		// Distinct arrays of equal PRIMITIVES are equal (each elem === ).
		{"top-level array primitives, distinct", []any{1, 2, 3}, []any{1, 2, 3}, true},
		{"top-level array primitives, differ", []any{1, 2, 3}, []any{1, 2, 4}, false},
		{"top-level array diff length", []any{1, 2}, []any{1, 2, 3}, false},
		// Distinct arrays whose elements are nested objects are NOT equal
		// (element === is reference identity).
		{"top-level array of nested objs, distinct", []any{MakeObject("x", 1)}, []any{MakeObject("x", 1)}, false},

		// --- type mismatch ---------------------------------------------------
		{"obj vs primitive", MakeObject("a", 1), "a", false},
		{"primitive vs obj", 1, MakeObject("a", 1), false},
	}

	for _, c := range cases {
		if got := equalAttrs(c.a, c.b); got != c.want {
			t.Errorf("equalAttrs(%v, %v) = %v, want %v (must match Yjs equalAttrs) [%s]", c.a, c.b, got, c.want, c.name)
		}
	}
}

// Nil/zero-Object safety: the zero Object (IsNil) must compare cleanly against a
// present Object and against itself without panicking.
func TestEqualAttrsZeroObjectSafe(t *testing.T) {
	var zero Object // IsNil
	present := MakeObject("a", 1)

	// A zero Object vs a present one: different size (0 vs 1) -> not equal.
	if equalAttrs(zero, present) {
		t.Errorf("zero Object should not equal a present 1-key object")
	}
	if equalAttrs(present, zero) {
		t.Errorf("present 1-key object should not equal a zero Object")
	}
	// Two zero Objects: both empty (size 0) -> equal, no panic.
	var zero2 Object
	if !equalAttrs(zero, zero2) {
		t.Errorf("two zero Objects should compare equal")
	}
}

// End-to-end: pin the format-attribute equality verdict to Yjs behavior through
// the real Y.Text format path. An Object-valued attribute re-applied with a
// DISTINCT (but structurally identical) instance must NOT be treated as a no-op
// the way two === references would be — Yjs's equalAttrs returns false for
// distinct nested objects, so the second format inserts a fresh marker rather
// than collapsing. We observe the verdict via equalAttrs on the two values the
// format path compares (a stored ContentFormat.Value vs a freshly-built one).
func TestFormatAttributeObjectValueFollowsYjsVerdict(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	txt := doc.GetText("t")
	Transact(doc, func(trans *Transaction) {
		txt.Insert(0, "hello", newObject())
	}, nil, false)

	// Format [0,3) with an object-valued attribute.
	attrA := MakeObject("style", MakeObject("weight", "bold"))
	Transact(doc, func(trans *Transaction) {
		txt.Format(0, 3, attrA)
	}, nil, false)

	// A distinct instance with identical structure: Yjs equalAttrs(...) === false
	// for the nested object (reference identity), so this is NOT equal.
	attrBDistinct := MakeObject("style", MakeObject("weight", "bold"))
	if equalAttrs(attrA, attrBDistinct) {
		t.Fatalf("FINDING 3: distinct nested-object-valued attributes compared equal; " +
			"Yjs equalAttrs treats nested objects by reference (===), so they must differ")
	}

	// The SAME reference must still compare equal (intra-transaction marker drop).
	if !equalAttrs(attrA, attrA) {
		t.Fatalf("a nested-object attribute must equal itself by reference")
	}
}
