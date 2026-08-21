package crdt

import (
	"slices"
	"strings"
	"testing"
)

func sameOrderedAttributes(a, b Object) bool {
	aKeys, bKeys := a.Keys(), b.Keys()
	if !slices.Equal(aKeys, bKeys) {
		return false
	}
	for _, key := range aKeys {
		if !equalAttrs(a.GetOr(key), b.GetOr(key)) {
			return false
		}
	}
	return true
}

func assertIndexedBoldRun(t *testing.T, doc *Doc, text *YText, want string) {
	t.Helper()
	state, index := ownedListPositionIndex(text)
	if state == nil || index == nil {
		t.Fatal("formatted fixture did not activate the writer position index")
	}
	if text.searchMarker != nil {
		t.Fatal("formatted fixture unexpectedly retained mutable search markers")
	}
	validateListPositionTree(t, index, text)
	validateDocPositionIndexEntries(t, doc)

	delta := text.ToDelta(nil, nil, nil)
	if len(delta) != 1 || delta[0].InsertValue() != want || delta[0].Attributes.GetOr("bold") != true {
		t.Fatalf("formatted run delta = %s, want one bold insert of length %d", deltaString(delta), len(want))
	}
}

func assertIndexedFormattedRoundTrips(t *testing.T, doc *Doc, want string) {
	t.Helper()
	for _, codec := range []struct {
		name   string
		encode func(*Doc, []uint8) ([]uint8, error)
		apply  func(*Doc, []uint8, interface{}) error
	}{
		{name: "v1", encode: EncodeStateAsUpdate, apply: ApplyUpdate},
		{name: "v2", encode: EncodeStateAsUpdateV2, apply: ApplyUpdateV2},
	} {
		t.Run(codec.name, func(t *testing.T) {
			update, err := codec.encode(doc, nil)
			if err != nil {
				t.Fatal(err)
			}
			fresh := newDoc("formatted-index", false, defaultGCFilter, nil, false, WithClientID(2))
			if err := codec.apply(fresh, update, nil); err != nil {
				t.Fatal(err)
			}
			text := fresh.GetText("t")
			if got := text.ToString(); got != want {
				t.Fatalf("round trip text length=%d, want %d", len(got), len(want))
			}
			// A decoded formatted list starts without an index. The next inherited insert must build
			// one, reconstruct bold from ContentFormat, and preserve the wire-derived semantics.
			text.Insert(text.Length()/2, "R", Object{})
			if _, index := ownedListPositionIndex(text); index == nil {
				t.Fatal("decoded formatted mutation did not activate the writer position index")
			}
			found := false
			for _, op := range text.ToDelta(nil, nil, nil) {
				if s, ok := op.InsertValue().(string); ok && strings.Contains(s, "R") {
					found = true
					if op.Attributes.GetOr("bold") != true {
						t.Fatalf("decoded inherited insert lost bold: %s", deltaString(text.ToDelta(nil, nil, nil)))
					}
				}
			}
			if !found {
				t.Fatal("decoded inherited insert is absent from delta")
			}
		})
	}
}

func TestFormattedPositionIndexInheritsAndRoundTrips(t *testing.T) {
	doc := newDoc("formatted-index", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, "x", boldAttr(true))
	model := []byte{'x'}
	rng := markerLCG(0xf04a7)
	for len(model) < buildListPositionIndexItems+500 {
		// Stay inside or at the end of the bold run. Position zero intentionally has different
		// boundary inheritance semantics and is covered by the ordinary YText tests.
		at := 1 + rng(len(model))
		text.Insert(at, "x", Object{})
		model = append(model, 0)
		copy(model[at+1:], model[at:])
		model[at] = 'x'
	}
	assertIndexedBoldRun(t, doc, text, string(model))
	assertIndexedFormattedRoundTrips(t, doc, string(model))
}

func TestFormattedPositionIndexMatchesFullAttributeWalkAtEveryPosition(t *testing.T) {
	doc := newDoc("formatted-index-every-position", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, "x", boldAttr(true))
	rng := markerLCG(0xa771)
	for i := 1; i < buildFormattedListPositionIndexItems+700; i++ {
		at := 1 + rng(text.Length())
		text.Insert(at, "x", Object{})
	}
	italic := newObject()
	italic.Set("italic", true)
	text.Format(100, 700, italic)
	clearBold := newObject()
	clearBold.Set("bold", Null)
	text.Format(300, 250, clearBold)
	color := newObject()
	color.Set("color", "red")
	text.Format(600, 400, color)

	Transact(doc, func(trans *Transaction) {
		for target := Number(0); target <= text.Length(); target++ {
			linked := findPositionValue(trans, text, target, false, Object{})
			itemCount := listItemCount(text) // The full walk may split a multi-code-unit Item.
			active := activeListPositionIndex(text, itemCount, false)
			if active == nil {
				t.Fatalf("indexed lookup %d lost active tree: items=%d typeLength=%d", target, itemCount, text.Length())
			}
			item, start, attributes, ok := active.findFormattedPosition(target, Object{})
			if !ok {
				t.Fatalf("indexed lookup %d failed: items=%d treeVisible=%d treeFormats=%d", target,
					itemCount, active.root.subtreeVisible, active.root.subtreeLiveFormats)
			}
			indexed := itemTextListPosition{
				left: item.left, right: item, index: start, currentAttributes: attributes,
			}
			findNextPosition(trans, &indexed, target-start)
			if indexed.left != linked.left || indexed.right != linked.right || indexed.index != linked.index ||
				!sameOrderedAttributes(indexed.currentAttributes, linked.currentAttributes) {
				t.Fatalf("position %d differs: indexed=(left=%p right=%p index=%d attrs=%v) "+
					"linked=(left=%p right=%p index=%d attrs=%v)", target,
					indexed.left, indexed.right, indexed.index, indexed.currentAttributes.ToMap(),
					linked.left, linked.right, linked.index, linked.currentAttributes.ToMap())
			}
		}
	}, nil, true)
	_, index := ownedListPositionIndex(text)
	if index == nil {
		t.Fatal("position comparison discarded the formatted index")
	}
	validateListPositionTree(t, index, text)
}

func TestFormattedPositionIndexHandlesNullAndUndoRedo(t *testing.T) {
	doc := newDoc("formatted-index-null", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	text.Insert(0, strings.Repeat("x", buildListPositionIndexItems+500), boldAttr(true))

	// Fragment the ContentString without changing the formatting. This reaches the indexed shape
	// cheaply while retaining the opening bold marker and its Null closing marker.
	rng := markerLCG(0x7eed)
	for i := 0; i < buildListPositionIndexItems; i++ {
		at := 1 + rng(text.Length()-1)
		text.Insert(at, "x", Object{})
	}
	if _, index := ownedListPositionIndex(text); index == nil {
		t.Fatal("fixture did not activate the formatted position index")
	}

	clearBold := newObject()
	clearBold.Set("bold", Null)
	text.Format(5_000, 5_000, clearBold)
	if _, index := ownedListPositionIndex(text); index != nil {
		t.Fatal("format change retained an index with stale live-format aggregates")
	}

	// Each nil-attribute insert must replay both the true and Null format values through
	// updateCurrentAttributes. A Null value deletes bold; treating it as a stored value is wrong.
	for _, insert := range []struct {
		at       Number
		value    string
		wantBold bool
	}{
		{at: 2_500, value: "L", wantBold: true},
		{at: 7_501, value: "M", wantBold: false},
		{at: 12_002, value: "N", wantBold: true},
	} {
		text.Insert(insert.at, insert.value, Object{})
		found := false
		for _, op := range text.ToDelta(nil, nil, nil) {
			s, ok := op.InsertValue().(string)
			if !ok || !strings.Contains(s, insert.value) {
				continue
			}
			found = true
			_, bold := op.Attributes.Get("bold")
			if bold != insert.wantBold {
				t.Fatalf("insert %q bold=%t, want %t; delta=%s", insert.value, bold, insert.wantBold,
					deltaString(text.ToDelta(nil, nil, nil)))
			}
		}
		if !found {
			t.Fatalf("insert %q absent from delta", insert.value)
		}
	}
	_, index := ownedListPositionIndex(text)
	if index == nil {
		t.Fatal("inherited insert did not rebuild the formatted position index")
	}
	validateListPositionTree(t, index, text)

	manager := newUndoManager(text, 0, func(*itemStruct) bool { return true }, defaultTrackedOrigins())
	italic := newObject()
	italic.Set("italic", true)
	text.Format(1_000, 100, italic)
	if _, active := ownedListPositionIndex(text); active != nil {
		t.Fatal("format mutation retained the writer position index")
	}
	if _, _, _, ok := indexedFormattedTextPosition(text, text.Length()/2, listItemCount(text), Object{}); !ok {
		t.Fatal("could not rebuild formatted index before undo")
	}
	if manager.Undo() == nil {
		t.Fatal("format undo produced no stack item")
	}
	if _, active := ownedListPositionIndex(text); active != nil {
		t.Fatal("format undo retained an index with stale live-format aggregates")
	}
	if _, _, _, ok := indexedFormattedTextPosition(text, text.Length()/2, listItemCount(text), Object{}); !ok {
		t.Fatal("could not rebuild formatted index before redo")
	}
	if manager.Redo() == nil {
		t.Fatal("format redo produced no stack item")
	}
	if _, active := ownedListPositionIndex(text); active != nil {
		t.Fatal("format redo retained an index with stale live-format aggregates")
	}
}
