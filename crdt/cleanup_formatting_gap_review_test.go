package crdt

import "testing"

// cleanup_formatting_gap_review_test.go pins cleanupYTextFormatting / deleteText
// shallow-copy (Object.ShallowClone) of the attribute maps: yjs uses map.copy
// (object.assign — a SHALLOW clone), sharing nested values by reference so the
// reference-strict equality (attrStrictEqual / equalAttrs) sees ==. A deep copy
// would give nested values a fresh handle and break that reference match.
//
// NOTE: cleanupFormattingGap was ported to the faithful yjs@13.6.31 algorithm
// (endFormats by reference + reachedCurr + currAttributes mutation). The synthetic
// chain below ([Fmt(val)] "a" [Fmt(val) same-ref] "b") was VERIFIED against
// yjs Y.cleanupYTextFormatting: yjs KEEPS both markers (cleanups=0). The earlier
// value-based Go algorithm over-deleted the second marker; this test now pins the
// faithful (yjs-matching) behavior.

// integrateFormat inserts a ContentFormat(key, value) marker at currPos using the
// same primitive insertAttributes uses, and updates currentAttributes by
// reference (mirroring updateCurrentAttributes). Returns the new position.
func integrateFormat(trans *Transaction, y *YText, currPos *itemTextListPosition, key string, value any) {
	doc := trans.doc
	left, right := currPos.left, currPos.right
	currPos.right = newItem(GenID(doc.ClientID, getState(doc.store, doc.ClientID)), left, getItemLastID(left), right, getItemID(right), y, "", newContentFormat(key, value))
	_ = currPos.right.integrateStruct(trans, 0)
	_ = currPos.forward()
}

// integrateString inserts a ContentString at currPos. Returns the new position.
func integrateString(trans *Transaction, y *YText, currPos *itemTextListPosition, s string) {
	doc := trans.doc
	left, right := currPos.left, currPos.right
	currPos.right = newItem(GenID(doc.ClientID, getState(doc.store, doc.ClientID)), left, getItemLastID(left), right, getItemID(right), y, "", newContentString(s))
	_ = currPos.right.integrateStruct(trans, 0)
	_ = currPos.forward()
}

func formatMarkerCount(y *YText) int {
	n := 0
	for it := y.start; it != nil; it = it.right {
		if _, ok := it.content.(*contentFormat); ok && !it.isDeleted() {
			n++
		}
	}
	return n
}

func TestCleanupFormattingGapMatchesYjsOnRedundantMarker(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false)
	doc.ClientID = 1
	y := doc.GetText("t")

	// A doubly-nested format value: the attribute "fmt" -> {style:{weight:700}}.
	// The SAME reference is reused for the start marker and the redundant marker —
	// exactly what a single Yjs format op produces (the value flows into
	// currentAttributes by reference, then the trailing marker reuses it).
	val := MakeObject("style", MakeObject("weight", 700))

	Transact(doc, func(trans *Transaction) {
		pos := findPosition(trans, y, 0, false)
		// Chain: [Fmt(val)] "a" [Fmt(val) — REDUNDANT, same ref] "b"
		integrateFormat(trans, y, pos, "fmt", val)
		integrateString(trans, y, pos, "a")
		integrateFormat(trans, y, pos, "fmt", val) // redundant: "fmt" already == val
		integrateString(trans, y, pos, "b")
	}, nil, true)

	if got := y.ToString(); got != "ab" {
		t.Fatalf("setup text = %q, want \"ab\"", got)
	}
	before := formatMarkerCount(y)
	if before != 2 {
		t.Fatalf("setup should have 2 format markers, got %d", before)
	}

	cleanups := cleanupYTextFormatting(y)

	after := formatMarkerCount(y)
	// VERIFIED against yjs@13.6.31 Y.cleanupYTextFormatting on this exact chain
	// (built with the same low-level Item/ContentFormat integration): cleanups=0,
	// BOTH markers kept. cleanupFormattingGap deletes a format only when it is not
	// its key's canonical end-format in the gap (endFormats by reference) or is
	// implied by startAttributes; here each marker is its gap's end-format and the
	// second is never in an examined gap (cleanupYTextFormatting advances `start`
	// past the countable "a"). The OLD value-based Go algorithm over-deleted the
	// second marker (cleanups>=1) — this asserts the faithful behavior (teeth: the
	// old algorithm fails here).
	if cleanups != 0 {
		t.Fatalf("cleanupYTextFormatting over-cleaned (cleanups=%d, want 0); yjs keeps both markers for this chain", cleanups)
	}
	if after != 2 {
		t.Fatalf("after cleanup expected 2 markers (yjs keeps both), got %d (cleanups=%d)", after, cleanups)
	}
}

// TestObjectShallowCloneSharesNestedReference is the focused unit guard for the
// helper the fix relies on: ShallowClone gives a fresh TOP-level store but SHARES
// nested values by reference (so equalAttrs sees ==), unlike the deep Clone.
func TestObjectShallowCloneSharesNestedReference(t *testing.T) {
	inner := MakeObject("weight", 700)
	val := MakeObject("style", inner)
	current := MakeObject("fmt", val)

	shallow := current.ShallowClone()
	// Top-level is independent: deleting from the clone does not touch the original.
	shallow.Delete("fmt")
	if !current.Has("fmt") {
		t.Fatal("ShallowClone must not mutate the original on a top-level Delete")
	}

	// The nested value is the SAME reference, so equalAttrs (reference-strict for
	// nested Objects) reports == against the original value.
	shallow2 := current.ShallowClone()
	if !equalAttrs(shallow2.GetOr("fmt"), val) {
		t.Fatal("ShallowClone must share the nested-object reference (equalAttrs ==)")
	}

	// A deep copy, by contrast, gives the nested value a fresh handle whose inner
	// object is also fresh — so a value containing a nested OBJECT compares != under
	// the reference-strict equalFlat.
	deep := mustCloneDataObject(current)
	if equalAttrs(deep.GetOr("fmt"), val) {
		t.Fatal("deep Clone should NOT share the nested-object reference (this is the bug the fix avoids)")
	}
}
