package crdt

import (
	"testing"
)

// newTestUndoManager creates an UndoManager scoped to arr with a 500ms capture timeout
// and yjs's default trackedOrigins ({null}), so nil-origin (normal local) transactions
// are tracked.
func newTestUndoManager(arr *YArray) *UndoManager {
	return newUndoManager(arr, 500, func(_ *itemStruct) bool { return true }, defaultTrackedOrigins())
}

// TestUndoStackShrinksAfterUndo verifies that UndoStack is empty after undoing the only
// tracked change. The bug: popStackItem took stack []*StackItem by value, so the
// re-slice inside only mutated the local copy and u.UndoStack never shrank.
func TestUndoStackShrinksAfterUndo(t *testing.T) {
	doc := newDoc("guid", false, nil, nil, false)
	arr := doc.GetArray("test")
	um := newTestUndoManager(arr)

	arr.Push(ArrayAny{"a"})

	if len(um.UndoStack) != 1 {
		t.Fatalf("expected 1 undo stack item after push, got %d", len(um.UndoStack))
	}

	um.Undo()

	if len(um.UndoStack) != 0 {
		t.Errorf("UndoStack should be empty after undo, got %d items", len(um.UndoStack))
	}
}

// TestUndoTwoIndependentChanges verifies that two changes captured in separate StackItems
// can each be independently undone. The bug: without the stack pointer fix, the second
// Undo() re-attempted the already-tombstoned top item and returned nil, so the first
// change was never reverted.
func TestUndoTwoIndependentChanges(t *testing.T) {
	doc := newDoc("guid", false, nil, nil, false)
	ymap := doc.GetMap("test")
	um := newUndoManager(ymap, 500, func(_ *itemStruct) bool { return true }, defaultTrackedOrigins())

	doc.Transact(func(_ *Transaction) {
		ymap.Set("a", "1")
	}, nil)
	um.StopCapturing()
	doc.Transact(func(_ *Transaction) {
		ymap.Set("b", "2")
	}, nil)

	if len(um.UndoStack) != 2 {
		t.Fatalf("expected 2 undo stack items, got %d", len(um.UndoStack))
	}

	// First undo: reverts the "b" set.
	um.Undo()
	if ymap.Get("b") != nil {
		t.Errorf("first undo: expected b=nil, got %v", ymap.Get("b"))
	}
	if ymap.Get("a") != "1" {
		t.Errorf("first undo: expected a=1, got %v", ymap.Get("a"))
	}
	if len(um.UndoStack) != 1 {
		t.Errorf("first undo: expected 1 remaining stack item, got %d", len(um.UndoStack))
	}

	// Second undo: reverts the "a" set.
	um.Undo()
	if ymap.Get("a") != nil {
		t.Errorf("second undo: expected a=nil, got %v", ymap.Get("a"))
	}
	if len(um.UndoStack) != 0 {
		t.Errorf("second undo: expected empty stack, got %d items", len(um.UndoStack))
	}
}

// TestRedoRestoresYMapFieldsAfterUndo verifies that undoing and then redoing the insertion
// of a Y.Map into a Y.Array restores the map's field values. The bug: ContentType.Copy
// used copystructure.Copy, which follows Left/Right item pointers and deep-copies the
// entire document item graph — causing the test to hang on any integrated Y.Map. When it
// did complete, the copied items retained their tombstoned state from the undo, so the
// redo'd map appeared empty. The fix reads values from tombstoned items and creates fresh
// items via Set, without touching the item linked list.
func TestRedoRestoresYMapFieldsAfterUndo(t *testing.T) {
	doc := newDoc("guid", false, nil, nil, false)
	arr := doc.GetArray("test")
	um := newTestUndoManager(arr)

	// PrelimContent is applied inside Integrate, which runs inside the same transaction
	// as the Push — so the field item is in the same StackItem as the array insertion.
	arr.Push(ArrayAny{NewYMap(map[string]interface{}{"k": "v"})})

	if arr.GetLength() != 1 {
		t.Fatalf("expected length 1 after push, got %d", arr.GetLength())
	}

	// Undo: tombstones both the array item and the field item.
	result := um.Undo()
	if result == nil {
		t.Fatal("Undo returned nil — nothing was undone")
	}
	if arr.GetLength() != 0 {
		t.Fatalf("expected length 0 after undo, got %d", arr.GetLength())
	}

	// Redo: should restore the array item and its field values.
	result = um.Redo()
	if result == nil {
		t.Fatal("Redo returned nil — nothing was redone")
	}
	if arr.GetLength() != 1 {
		t.Fatalf("expected length 1 after redo, got %d", arr.GetLength())
	}

	restored, ok := arr.Get(0).(*YMap)
	if !ok {
		t.Fatalf("expected *YMap at index 0, got %T", arr.Get(0))
	}
	if restored.Get("k") != "v" {
		t.Errorf("after redo: expected k=v, got %v", restored.Get("k"))
	}
}
