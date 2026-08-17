package crdt

import "testing"

// US4 / FR-012..FR-019 (work item 1.6). UndoManager gaps G1–G7, ported from
// yjs@13.6.31 src/utils/UndoManager.js + src/structs/Item.js redoItem.

// defaultTrackedOrigins returns yjs's default trackedOrigins ({null}), so a
// transaction with a nil origin (a normal local edit) is tracked.
func defaultTrackedOrigins() Set {
	s := NewSet()
	s.Add(nil)
	return s
}

func mustUpdate(t *testing.T, doc *Doc) []uint8 {
	t.Helper()
	u, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatalf("EncodeStateAsUpdate: %v", err)
	}
	return u
}

// setupRemoteMapConflict builds the G1 scenario: doc1 sets m["k"]="A", an
// UndoManager starts tracking, doc1 sets "k"="B" (the tracked op), then a REMOTE
// client sets "k"="C" and syncs back (applied with a non-tracked origin so it is
// not captured). Returns doc1's map and the undo manager.
func setupRemoteMapConflict(t *testing.T, ignoreRemote bool) (*YMap, *UndoManager) {
	t.Helper()
	doc1 := newDoc("g", false, nil, nil, false, WithClientID(1))
	m1 := doc1.GetMap("m")
	m1.Set("k", "A")

	um := newUndoManager(m1, 500, func(*itemStruct) bool { return true }, defaultTrackedOrigins())
	um.IgnoreRemoteMapChanges = ignoreRemote
	m1.Set("k", "B") // tracked: deletes A, inserts B

	doc2 := newDoc("g", false, nil, nil, false, WithClientID(2))
	_ = ApplyUpdate(doc2, mustUpdate(t, doc1), "remote")
	doc2.GetMap("m").Set("k", "C")
	_ = ApplyUpdate(doc1, mustUpdate(t, doc2), "remote") // non-tracked origin → not captured

	if got := m1.Get("k"); got != "C" {
		t.Fatalf("setup: expected k=C after remote write, got %v", got)
	}
	return m1, um
}

// G1 default: undo must NOT clobber a concurrent remote map write.
func TestUndoPreservesConcurrentRemoteMapWrite(t *testing.T) {
	m1, um := setupRemoteMapConflict(t, false)
	um.Undo()
	if got := m1.Get("k"); got != "C" {
		t.Errorf("default undo clobbered the remote write: k=%v, want C (remote preserved)", got)
	}
}

// G1 + G4 opt-in: with IgnoreRemoteMapChanges the undo overwrites the remote write.
func TestUndoOverwritesRemoteWithIgnoreFlag(t *testing.T) {
	m1, um := setupRemoteMapConflict(t, true)
	um.Undo()
	if got := m1.Get("k"); got != "A" {
		t.Errorf("ignoreRemoteMapChanges undo restored %v, want A (the original local value)", got)
	}
}

// G2: a whole-document scope undoes a nested edit.
func TestUndoDocScope(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	um := newUndoManager(doc, 500, func(*itemStruct) bool { return true }, defaultTrackedOrigins())
	if um.GetDoc() != doc {
		t.Fatal("doc-scope GetDoc() mismatch")
	}
	m := doc.GetMap("m")
	m.Set("k", "v")
	if !um.CanUndo() {
		t.Fatal("doc-scope did not track the nested edit")
	}
	um.Undo()
	if got := m.Get("k"); got != nil {
		t.Errorf("doc-scope undo did not revert the nested edit: k=%v", got)
	}
}

// Regression: a doc-scoped UndoManager must not leave a PHANTOM stack item after
// Clear(). Clear()'s internal keep-bit-only transaction passes the scope/origin guards
// for a doc scope, so without the empty-transaction guard in the capture handler it
// pushed an empty StackItem — leaving CanUndo()/CanRedo() true with nothing to revert.
func TestUndoClearNoPhantomStackItem(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	um := newUndoManager(doc, 500, func(*itemStruct) bool { return true }, defaultTrackedOrigins())
	m := doc.GetMap("m")
	m.Set("k", "v")
	if !um.CanUndo() {
		t.Fatal("expected an undo step after the edit")
	}
	um.Clear(true, true)
	if um.CanUndo() || len(um.UndoStack) != 0 {
		t.Errorf("Clear left a phantom undo item: CanUndo=%v undoStack=%d", um.CanUndo(), len(um.UndoStack))
	}
	if um.CanRedo() || len(um.RedoStack) != 0 {
		t.Errorf("Clear left a phantom redo item: CanRedo=%v redoStack=%d", um.CanRedo(), len(um.RedoStack))
	}
}

// G5: first capture emits stack-item-added; a coalesced capture emits
// stack-item-updated; Clear emits stack-cleared.
func TestUndoStackEvents(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	um := newUndoManager(m, 500, func(*itemStruct) bool { return true }, defaultTrackedOrigins())

	var added, updated, cleared int
	um.On("stack-item-added", NewObserverHandler(func(...interface{}) { added++ }))
	um.On("stack-item-updated", NewObserverHandler(func(...interface{}) { updated++ }))
	um.On("stack-cleared", NewObserverHandler(func(...interface{}) { cleared++ }))

	m.Set("a", 1) // new stack item -> added
	m.Set("b", 2) // within captureTimeout -> coalesced -> updated

	if added != 1 {
		t.Errorf("stack-item-added fired %d times, want 1", added)
	}
	if updated < 1 {
		t.Errorf("stack-item-updated did not fire on coalesce (got %d)", updated)
	}

	um.Clear(true, true)
	if cleared != 1 {
		t.Errorf("stack-cleared fired %d times, want 1", cleared)
	}
}

// G6: CanUndo/CanRedo reflect the stacks.
func TestUndoCanUndoCanRedo(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	um := newUndoManager(m, 500, func(*itemStruct) bool { return true }, defaultTrackedOrigins())

	if um.CanUndo() || um.CanRedo() {
		t.Fatal("fresh manager should have nothing to undo/redo")
	}
	m.Set("a", 1)
	if !um.CanUndo() {
		t.Error("CanUndo false after an edit")
	}
	um.Undo()
	if !um.CanRedo() {
		t.Error("CanRedo false after undo")
	}
}

// FR-018: an undo that errors partway (here, a panicking delete filter inside the
// pop) must still reset the undoing flag — the defer in Undo, mirroring yjs's
// try/finally. Without the defer, undoing would stay true and wedge the manager.
func TestUndoResetsFlagOnError(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	um := newUndoManager(m, 500, func(*itemStruct) bool { panic("filter boom") }, defaultTrackedOrigins())

	m.Set("a", 1) // tracked insertion; undoing it invokes the panicking delete filter

	func() {
		defer func() { _ = recover() }()
		um.Undo()
	}()

	if um.Undoing {
		t.Error("undoing flag not reset after an error mid-pop (missing defer reset)")
	}
}

// G7: AddToScope (dedup + cross-doc warning), AddTrackedOrigin/RemoveTrackedOrigin.
func TestUndoAddToScopeAndTrackedOrigins(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	m1 := doc.GetMap("m1")
	um := newUndoManager(m1, 500, func(*itemStruct) bool { return true }, defaultTrackedOrigins())

	// dedup: re-adding an existing scope does not grow it
	n := len(um.scopes)
	um.AddToScope(m1)
	if len(um.scopes) != n {
		t.Errorf("AddToScope duplicated an existing scope: %d -> %d", n, len(um.scopes))
	}

	// extend tracking to a second type
	m2 := doc.GetMap("m2")
	um.AddToScope(m2)
	m2.Set("x", 1)
	if !um.CanUndo() {
		t.Error("AddToScope did not extend tracking to the new type")
	}

	// a type from a different doc triggers the [yjs#509] warning branch
	doc2 := newDoc("g2", false, nil, nil, false, WithClientID(2))
	um.AddToScope(doc2.GetMap("o"))

	// custom tracked origin captured; not captured after removal
	um.Clear(true, true)
	um.AddTrackedOrigin("svc")
	doc.Transact(func(*Transaction) { m1.Set("y", 1) }, "svc")
	if !um.CanUndo() {
		t.Error("AddTrackedOrigin: edit with tracked origin not captured")
	}
	um.Clear(true, true)
	um.RemoveTrackedOrigin("svc")
	doc.Transact(func(*Transaction) { m1.Set("z", 1) }, "svc")
	if um.CanUndo() {
		t.Error("RemoveTrackedOrigin: edit with removed origin still captured")
	}
}

// G6: Clear unpins the deleted structs a stack item kept alive (clearUndoManagerStackItem).
func TestUndoClearUnpinsDeletedItems(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	um := newTestUndoManager(arr)

	arr.Push(ArrayAny{"x"})
	um.StopCapturing()
	arr.Delete(0, 1) // tracked deletion -> stack item carries deletions

	um.Clear(true, true)
	if um.CanUndo() || um.CanRedo() {
		t.Error("Clear did not empty the stacks")
	}
}

// G1: redo of a nested array-of-maps exercises the array sibling tracing and the
// parent-redone field-restore path in RedoItem.
func TestUndoRedoNestedArrayOfMaps(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	um := newTestUndoManager(arr)

	arr.Push(ArrayAny{
		NewYMap(map[string]interface{}{"k1": "v1", "k2": "v2"}),
		NewYMap(map[string]interface{}{"k3": "v3"}),
	})
	um.Undo()
	if arr.GetLength() != 0 {
		t.Fatalf("undo: expected empty array, got len %d", arr.GetLength())
	}
	um.Redo()
	if arr.GetLength() != 2 {
		t.Fatalf("redo: expected len 2, got %d", arr.GetLength())
	}
	m0 := arr.Get(0).(*YMap)
	if m0.Get("k1") != "v1" || m0.Get("k2") != "v2" {
		t.Errorf("redo lost map[0] fields: k1=%v k2=%v", m0.Get("k1"), m0.Get("k2"))
	}
	if m1 := arr.Get(1).(*YMap); m1.Get("k3") != "v3" {
		t.Errorf("redo lost map[1] field: k3=%v", m1.Get("k3"))
	}
}

// Round-4 review: Destroy must clear the stacks (and unpin the KeepItem refs the
// captured items hold) so they can be GC'd, not leak until the Doc is dropped.
// yjs UndoManager.destroy does NOT clear/unpin the stacks (KeepItem is a bool, not a
// refcount, so unpinning could break a sibling manager's redo). Destroy only stops
// tracking; the stacks stay until the manager is GC'd. This test pins that deliberate
// yjs-faithful behavior.
func TestUndoManagerDestroyDoesNotClearStacks(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	um := newTestUndoManager(arr)

	arr.Push(ArrayAny{"x"})
	um.StopCapturing()
	arr.Delete(0, 1) // captured deletion -> stack item pins the deleted struct
	if !um.CanUndo() {
		t.Fatal("expected a non-empty undo stack")
	}

	um.Destroy()
	if !um.CanUndo() {
		t.Error("Destroy cleared the undo stack (yjs.destroy does not — avoids sibling unpin)")
	}
}

// Round-4 review: destroying via the doc 'destroy' handler (which calls um.Destroy ->
// Clear, a transaction) while the doc tears down must not panic.
func TestUndoManagerDocDestroyNoPanic(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	um := newTestUndoManager(arr)

	arr.Push(ArrayAny{"x"})
	um.StopCapturing()
	arr.Delete(0, 1)
	if !um.CanUndo() {
		t.Fatal("expected a non-empty undo stack")
	}

	doc.Destroy() // -> 'destroy' -> um.Destroy(); must not panic during teardown
	// yjs.destroy does not clear the stacks, so they remain after doc.Destroy; the
	// point of this test is that the destroy chain runs cleanly (no panic).
	if !um.CanUndo() {
		t.Error("doc.Destroy unexpectedly cleared the undo manager's stack")
	}
}

// Robustness + parity: a nil trackedOrigins must not panic AND must default to yjs's
// new Set([null]) — i.e. ordinary nil-origin local edits are tracked (an empty-set
// default would silently track nothing). Teeth: with an empty default the nil-origin
// edit is skipped and CanUndo() is false.
func TestUndoManagerNilTrackedOriginsDefaultsToNull(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	um := newUndoManager(m, 500, func(*itemStruct) bool { return true }, nil)
	m.Set("a", 1) // ordinary local edit, nil origin
	if !um.CanUndo() {
		t.Error("nil trackedOrigins did not default to {null}: nil-origin local edit not tracked")
	}
}

// yjs parity: the stack-item-popped event carries origin == the undo manager.
func TestUndoStackItemPoppedCarriesOrigin(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	um := newUndoManager(m, 500, func(*itemStruct) bool { return true }, defaultTrackedOrigins())

	var gotOrigin interface{}
	var sawEvent bool
	um.On("stack-item-popped", NewObserverHandler(func(v ...interface{}) {
		sawEvent = true
		if ev, ok := v[0].(Object); ok {
			gotOrigin = ev.GetOr("origin")
		}
	}))

	m.Set("a", 1)
	um.Undo()
	if !sawEvent {
		t.Fatal("stack-item-popped did not fire")
	}
	if gotOrigin != um {
		t.Errorf("stack-item-popped origin = %v, want the undo manager (yjs origin: undoManager)", gotOrigin)
	}
}

// Round-N review: Destroy must unregister BOTH the afterTransaction and the doc
// 'destroy' listeners (yjs UndoManager.destroy offs both). Otherwise each
// create+Destroy leaks the manager's 'destroy' closure into the doc's observer set.
func TestUndoManagerDestroyUnregistersDocDestroyHandler(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	m := doc.GetMap("m")

	base := len(doc.observers["destroy"])
	for i := 0; i < 20; i++ {
		um := newUndoManager(m, 500, func(*itemStruct) bool { return true }, defaultTrackedOrigins())
		um.Destroy()
	}
	if got := len(doc.observers["destroy"]); got > base {
		t.Errorf("doc 'destroy' observers leaked across create/Destroy: base=%d after=%d", base, got)
	}
}

// G7: after Destroy, the manager stops tracking edits (listener unregistered).
func TestUndoDestroyStopsTracking(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	m := doc.GetMap("m")
	um := newUndoManager(m, 500, func(*itemStruct) bool { return true }, defaultTrackedOrigins())

	m.Set("a", 1)
	n := len(um.UndoStack)
	if n == 0 {
		t.Fatal("edit not tracked before Destroy")
	}

	um.Destroy()  // unregisters the afterTransaction handler (does NOT clear stacks)
	m.Set("b", 2) // after Destroy -> not tracked, so the stack must not grow
	if len(um.UndoStack) != n {
		t.Errorf("edit tracked after Destroy: stack grew %d -> %d", n, len(um.UndoStack))
	}
}
