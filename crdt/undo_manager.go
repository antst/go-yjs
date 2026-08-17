package crdt

import (
	"reflect"
)

type StackItem struct {
	insertions *deleteSet
	deletions  *deleteSet
	Meta       map[interface{}]interface{} // Use this to save and restore metadata like selection range
}

// UndoManager is a port of yjs@13.6.31 src/utils/UndoManager.js. The scope may be a
// shared type or the whole *Doc; trackedOrigins and the capture-coalescing window
// match upstream. See the per-gap notes inline.
type UndoManager struct {
	*Observable
	// scopes holds the tracked shared-type roots. Kept as []IAbstractType (the
	// original, type-safe public type); a whole-document scope (yjs's `Doc` scope
	// entry) is represented by docScoped rather than boxing a *Doc into this slice.
	scopes         []abstractType
	deleteFilter   func(item *itemStruct) bool
	TrackedOrigins Set
	UndoStack      []*StackItem
	RedoStack      []*StackItem

	// Whether the client is currently undoing (calling UndoManager.undo)
	Undoing    bool
	Redoing    bool
	LastChange Number

	// G4 options/fields (yjs UndoManagerOptions):
	CaptureTimeout         Number                        // merge window (ms); default 500
	CaptureTransaction     func(trans *Transaction) bool // skip a transaction's capture if false
	IgnoreRemoteMapChanges bool                          // allow undo to overwrite a concurrent remote map write
	CurrStackItem          *StackItem                    // the stack item being built in the current capture

	doc               *Doc             // resolved from the scope; the undo manager's document
	docScoped         bool             // whole-document scope (a *Doc was added) — tracks any change
	afterHandler      *ObserverHandler // afterTransaction listener (unregistered in Destroy)
	docDestroyHandler *ObserverHandler // doc 'destroy' listener (unregistered in Destroy)
}

func (u *UndoManager) GetDoc() *Doc {
	return u.doc
}

// trackedByScope reports whether the transaction touched something in scope: a
// tracked type that changed, or (for a doc-level scope) any change. Matches yjs
// `scope.some(type => changedParentTypes.has(type) || type === doc)`.
func (u *UndoManager) trackedByScope(trans *Transaction) bool {
	if u.docScoped {
		return true
	}
	for _, t := range u.scopes {
		if _, changed := trans.changedParentTypes[t]; changed {
			return true
		}
	}
	return false
}

// scopeContains reports whether item is within scope: under a tracked type, or any
// item when a doc-level scope is present. Matches yjs `type === doc || isParentOf`.
func (u *UndoManager) scopeContains(item *itemStruct) bool {
	if u.docScoped {
		return true
	}
	for _, t := range u.scopes {
		if isParentOf(t, item) {
			return true
		}
	}
	return false
}

// AddToScope adds tracked roots, deduping. Each argument is an IAbstractType (a
// shared-type scope) or a *Doc (whole-document scope -> docScoped). (G7)
func (u *UndoManager) AddToScope(scopes ...interface{}) {
	for _, ytype := range scopes {
		if _, ok := ytype.(*Doc); ok {
			// Whole-document scope. yjs warns here when the doc is not its own
			// ([yjs#509]) and then proceeds anyway; we proceed identically. The
			// warning itself is dropped because this package has no logging
			// channel and must never write to a consumer's stdout.
			u.docScoped = true
			continue
		}
		// Normalize slices HERE, in the shared primitive, not only in the constructor. The
		// reference's addToScope does the flattening itself, so a caller passing a slice to this
		// method got no scopes and no error — a silent drop (FR-014a).
		if rv := reflect.ValueOf(ytype); rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			expanded := make([]interface{}, 0, rv.Len())
			for i := 0; i < rv.Len(); i++ {
				expanded = append(expanded, rv.Index(i).Interface())
			}
			u.AddToScope(expanded...)
			continue
		}
		t, ok := ytype.(abstractType)
		if !ok {
			continue // ignore anything that is neither a type nor a doc
		}
		dup := false
		for _, existing := range u.scopes {
			if existing == t {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		// Same [yjs#509] case as the *Doc arm above: yjs warns on a foreign doc
		// and still adds the scope.
		u.scopes = append(u.scopes, t)
	}
}

// AddTrackedOrigin / RemoveTrackedOrigin manage the tracked-origin set. (G7)
func (u *UndoManager) AddTrackedOrigin(origin interface{}) { u.TrackedOrigins.Add(origin) }
func (u *UndoManager) RemoveTrackedOrigin(origin interface{}) {
	u.TrackedOrigins.Delete(origin)
}

// CanUndo / CanRedo report whether a step is available. (G6)
func (u *UndoManager) CanUndo() bool { return len(u.UndoStack) > 0 }
func (u *UndoManager) CanRedo() bool { return len(u.RedoStack) > 0 }

// clearUndoManagerStackItem unpins (keepItem false) the deleted structs a stack
// item kept alive, so discarding it lets them be gc'd. (yjs clearUndoManagerStackItem)
func clearUndoManagerStackItem(trans *Transaction, u *UndoManager, stackItem *StackItem) {
	iterateDeletedStructs(trans, stackItem.deletions, func(s abstractStruct) {
		if it, ok := s.(*itemStruct); ok && u.scopeContains(it) {
			keepItem(it, false)
		}
	})
}

// Clear selectively clears the undo and/or redo stacks, unpinning their kept items
// so GC can reclaim them, and emits 'stack-cleared'. (G6, replaces the old no-arg
// Clear + the raw `RedoStack = nil` discard that leaked kept items.)
func (u *UndoManager) Clear(clearUndoStack, clearRedoStack bool) {
	if (clearUndoStack && u.CanUndo()) || (clearRedoStack && u.CanRedo()) {
		u.GetDoc().Transact(func(trans *Transaction) {
			if clearUndoStack {
				for _, item := range u.UndoStack {
					clearUndoManagerStackItem(trans, u, item)
				}
				u.UndoStack = nil
			}
			if clearRedoStack {
				for _, item := range u.RedoStack {
					clearUndoManagerStackItem(trans, u, item)
				}
				u.RedoStack = nil
			}
			u.Emit("stack-cleared", MakeObject("undoStackCleared", clearUndoStack, "redoStackCleared", clearRedoStack))
		}, nil)
	}
}

// UndoManager merges Undo-StackItem if they are created within time-gap
// smaller than `options.captureTimeout`. Call `um.stopCapturing()` so that the next
// StackItem won't be merged.
func (u *UndoManager) StopCapturing() {
	u.LastChange = 0
}

// Undo last changes on type.
func (u *UndoManager) Undo() *StackItem {
	u.Undoing = true
	// Reset via defer so a panic mid-pop still clears the flag (yjs try/finally).
	defer func() { u.Undoing = false }()
	return popStackItem(u, &u.UndoStack, "undo")
}

// Redo last undo operation.
func (u *UndoManager) Redo() *StackItem {
	u.Redoing = true
	defer func() { u.Redoing = false }()
	return popStackItem(u, &u.RedoStack, "redo")
}

// Destroy unregisters the afterTransaction + doc-'destroy' listeners and removes self
// from tracked origins (yjs UndoManager.destroy). (G7)
//
// It deliberately does NOT Clear/unpin the stacks: KeepItem is a boolean flag, not a
// refcount, so unpinning here would clear keep on a struct a SIBLING UndoManager
// (overlapping scope, same captured deletion) still needs, letting GC collect it and
// turning the sibling's redo into a no-op. yjs avoids exactly this by not unpinning on
// destroy (kept structs are released when the Doc is dropped). The stacks are released
// when this manager is GC'd. (The doc-'destroy' off is a Go-specific leak fix — yjs
// leaves that listener, accumulating on a long-lived Doc across create/destroy cycles.)
func (u *UndoManager) Destroy() {
	u.TrackedOrigins.Delete(u)
	if u.afterHandler != nil {
		u.doc.Off("afterTransaction", u.afterHandler)
	}
	if u.docDestroyHandler != nil {
		u.doc.Off("destroy", u.docDestroyHandler)
	}
	u.Observable.Destroy()
}

func newStackItem(deletes *deleteSet, insertions *deleteSet) *StackItem {
	return &StackItem{
		deletions:  deletes,
		insertions: insertions,
		Meta:       make(map[interface{}]interface{}),
	}
}

// isDeletedByUndoStack reports whether id is deleted by any stack item's deletions
// (yjs isDeletedByUndoStack). Used by RedoItem's conflict detection to tell a
// concurrent remote change apart from one this manager is itself undoing.
func isDeletedByUndoStack(stack []*StackItem, id *ID) bool {
	for _, s := range stack {
		if isDeleted(s.deletions, id) {
			return true
		}
	}
	return false
}

func popStackItem(undoManager *UndoManager, stack *[]*StackItem, eventType string) *StackItem {
	// Whether a change happened
	var result *StackItem

	// Keep a reference to the transaction so we can fire the event with the changedParentTypes
	var tr *Transaction
	doc := undoManager.GetDoc()
	Transact(doc, func(trans *Transaction) {
		for len(*stack) > 0 && result == nil {
			store := doc.store
			stackItem := (*stack)[len(*stack)-1]
			*stack = (*stack)[:len(*stack)-1]
			itemsToRedo := NewSet()
			// itemsToRedoList holds the redo items in a DETERMINISTIC order. A Go `for
			// range` over the Set is randomized, and IterateDeletedStructs itself ranges
			// the delete set's client map (randomized cross-client). Since RedoItem
			// mutates shared state (Redone pointers, integrations), a random order yields
			// non-deterministic redo results run-to-run. Collect here, then sort by ID
			// order. (Keep the Set for RedoItem's membership checks.)
			//
			// Order now FOLLOWS THE REFERENCE rather than being locally canonical. It comes from
			// StructStore first-insertion order, which is where yjs's originates: its
			// StructStore.clients Map -> getStateVector -> afterState -> the insertions delete set
			// -> iterateDeletedStructs (research R1).
			//
			// The previous (client, clock) sort was accepted partly on the grounds that "the
			// cross-impl gate does not exercise undo". TestUndoDiff removes that premise, and a
			// deviation justified by absent coverage is re-opened the moment the coverage arrives.
			var itemsToRedoList []*itemStruct
			var itemsToDelete []*itemStruct
			performedChange := false

			iterateDeletedStructs(trans, stackItem.insertions, func(s abstractStruct) {
				it, ok := s.(*itemStruct)
				if ok {
					if it.redone != nil {
						item, diff := followRedone(store, it.id)
						if diff > 0 {
							item = getItemCleanStart(trans, GenID(item.id.Client, item.id.Clock+diff))
						}
						// GetItemCleanStart can return nil for a missing target (yjs
						// getItemCleanStart throws); skip this struct rather than
						// nil-panic on it.Deleted(). Unreachable normally (redone items
						// are pinned); guards a corrupted/partially-synced chain.
						if item == nil {
							return
						}
						it = item
					}

					if !it.isDeleted() && undoManager.scopeContains(it) {
						itemsToDelete = append(itemsToDelete, it)
					}
				}
			})

			iterateDeletedStructs(trans, stackItem.deletions, func(s abstractStruct) {
				it, ok := s.(*itemStruct)
				if ok && undoManager.scopeContains(it) && !isDeleted(stackItem.insertions, s.getID()) {
					// Never redo structs in stackItem.insertions because they were created and deleted in the same capture interval.
					if !itemsToRedo.Has(it) {
						itemsToRedo.Add(it)
						itemsToRedoList = append(itemsToRedoList, it)
					}
				}
			})

			// No sort here. The list already arrives in the reference's order, because the stack
			// item's insertions were built in StructStore first-insertion order and
			// IterateDeletedStructs walks the delete set in that order. Sorting by (client, clock)
			// would OVERRIDE the reference's order with a locally-chosen canonical one — which is
			// what this code used to do, and what FR-001a exists to undo.
			for _, it := range itemsToRedoList {
				// G1: 6-arg redoItem with the stack item's insertions as itemsToDelete,
				// the ignoreRemoteMapChanges flag, and the manager (for the undo/redo
				// stack conflict check).
				if redoItem(trans, it, itemsToRedo, stackItem.insertions, undoManager.IgnoreRemoteMapChanges, undoManager) != nil {
					performedChange = true
				}
			}

			// We want to delete in reverse order so that children are deleted before
			// parents, so we have more information available when items are filtered.
			for i := len(itemsToDelete) - 1; i >= 0; i-- {
				item := itemsToDelete[i]
				if undoManager.deleteFilter(item) {
					item.deleteItemStruct(trans)
					performedChange = true
				}
			}

			if performedChange {
				result = stackItem
			} else {
				result = nil
			}
		}

		for t, subProps := range trans.changedTypesInternal() {
			// Clear search markers if necessary (yjs popStackItem: `if (subProps.has(null)
			// && type._searchMarker) type._searchMarker.length = 0`). Array items use ""
			// as ParentSub in Go (vs null/undefined in JS). Clear to EMPTY (keep the slice
			// non-nil = ENABLED); setting it nil would disable markers permanently under
			// the nil-slice=disabled semantics, unlike yjs's length=0.
			if subProps.Has("") {
				sm := t.getSearchMarker()
				if *sm != nil {
					t.setSearchMarker((*sm)[:0])
				}
			}
		}

		tr = trans
	}, undoManager, true)

	// Reset the in-flight capture sentinel after the pop, matching yjs popStackItem
	// (currStackItem = null). The afterTransaction handler sets it during the pop's
	// own transaction; leaving it set would dangle at a stale opposite-stack item.
	undoManager.CurrStackItem = nil

	if result != nil {
		changedParentTypes := tr.changedParentTypes
		obj := newObject()
		obj.Set("stackItem", result)
		obj.Set("type", eventType)
		obj.Set("changedParentTypes", changedParentTypes)
		// yjs emits { stackItem, type, changedParentTypes, origin: undoManager } —
		// include origin so consumers can recognize the manager as the change source
		// (consistent with the stack-item-added/updated events, which carry origin).
		obj.Set("origin", undoManager)
		undoManager.Emit("stack-item-popped", obj, undoManager)
	}

	return result
}

// newUndoManager constructs an UndoManager. typeScope may be an IAbstractType or a
// *Doc (whole-document scope). New options (IgnoreRemoteMapChanges,
// CaptureTransaction) default off/permissive and may be set on the returned manager.
func newUndoManager(typeScope interface{}, captureTimeout Number, deleteFilter func(item *itemStruct) bool, trackedOrigins Set) *UndoManager {
	u := &UndoManager{}
	u.Observable = NewObservable()
	// Default the delete filter to allow-all, matching yjs's `deleteFilter=()=>true`
	// option default — popStackItem calls it unconditionally, so a nil filter would
	// panic on the first undo/redo that deletes anything.
	if deleteFilter == nil {
		deleteFilter = func(*itemStruct) bool { return true }
	}
	u.deleteFilter = deleteFilter
	// Default a nil tracked-origins set to yjs's `new Set([null])` — a nil arg means
	// "use the default", so seed nil so ordinary nil-origin local edits ARE tracked.
	// An empty set would silently track nothing (the origin guard skips a nil-origin
	// transaction unless nil is in the set), and Add would also panic on a nil map.
	if trackedOrigins == nil {
		trackedOrigins = NewSet()
		trackedOrigins.Add(nil)
	}
	// An EMPTY (non-nil) set is deliberately left alone: it is not the default, it
	// means "track nothing", and the manager then captures no ordinary local edits
	// because those carry a nil origin.
	trackedOrigins.Add(u)
	u.TrackedOrigins = trackedOrigins
	u.CaptureTimeout = captureTimeout
	u.CaptureTransaction = func(*Transaction) bool { return true }

	// Normalize the scope into individual roots. yjs accepts
	// `AbstractType | Array<AbstractType> | Doc`; support the array form too (a bare
	// slice would otherwise be added as a single opaque scope entry and leave doc nil).
	var scopes []interface{}
	switch ts := typeScope.(type) {
	case []abstractType:
		for _, t := range ts {
			scopes = append(scopes, t)
		}
	case []interface{}:
		scopes = ts
	default:
		// Go slice invariance: a concrete typed slice (e.g. []*YMap) matches neither
		// case above and would otherwise be wrapped as one opaque scope, leaving doc
		// unresolved. Detect ANY slice/array via reflection and expand its elements so
		// every typed-slice form of yjs's `Array<AbstractType>` works.
		if rv := reflect.ValueOf(typeScope); rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array {
			for i := 0; i < rv.Len(); i++ {
				scopes = append(scopes, rv.Index(i).Interface())
			}
		} else {
			scopes = []interface{}{typeScope}
		}
	}
	// Resolve the doc from the first type/doc root (yjs: array ? scope[0].doc : ...).
	for _, s := range scopes {
		if d, ok := s.(*Doc); ok {
			u.doc = d
			break
		}
		if t, ok := s.(abstractType); ok {
			u.doc = t.getDoc()
			break
		}
	}
	u.AddToScope(scopes...)
	if u.doc == nil {
		// Fail fast with a clear message rather than a later nil-deref at u.doc.On.
		// The scope must yield a doc (a *Doc, an IAbstractType, or a non-empty array
		// of types); yjs likewise requires a resolvable doc for the manager.
		panic("NewUndoManager: scope must contain a *Doc or an IAbstractType")
	}

	u.Undoing = false
	u.Redoing = false
	u.LastChange = 0

	u.afterHandler = NewObserverHandler(func(v ...interface{}) {
		if len(v) == 0 {
			return
		}
		trans := v[0].(*Transaction)

		// Only track certain transactions (yjs guard): captureTransaction, scope,
		// and tracked origin.
		if u.CaptureTransaction != nil && !u.CaptureTransaction(trans) {
			return
		}
		if !u.trackedByScope(trans) {
			return
		}
		// Tracked-origin guard, faithful to yjs: skip a transaction whose origin is not
		// tracked. A nil origin is tracked only if nil is in the set (yjs defaults
		// trackedOrigins to {null}); a non-nil origin is tracked if the value OR its
		// type name is in the set. (The previous Go code always tracked nil-origin
		// transactions regardless of the set — a divergence that made it impossible to
		// exclude untracked local edits the way yjs allows.)
		if !u.TrackedOrigins.Has(trans.origin) &&
			(trans.origin == nil || !u.TrackedOrigins.Has(reflect.TypeOf(trans.origin).String())) {
			return
		}

		undoing := u.Undoing
		redoing := u.Redoing

		var stack *[]*StackItem
		if undoing {
			stack = &u.RedoStack
		} else {
			stack = &u.UndoStack
		}

		// Build in the store's client FIRST-INSERTION order, matching the reference: yjs iterates
		// afterState, a JS Map whose order comes from StructStore.clients, and addToDeleteSet
		// inserts on first touch — so the delete set inherits that order and iterateDeletedStructs
		// later walks it. Ranging trans.AfterState here would use Go's randomised map order and
		// lose it (research R1).
		insertions := newDeleteSet()
		for _, client := range trans.doc.store.orderedClients() {
			endClock, ok := trans.afterState[client]
			if !ok {
				continue
			}
			startClock := trans.beforeState[client]
			length := endClock - startClock
			if length > 0 {
				addToDeleteSet(insertions, client, startClock, length)
			}
		}

		// Skip transactions with nothing to undo: no insertions AND no deletions. A
		// tracked but structurally-empty transaction (notably UndoManager.Clear()'s
		// internal keep-bit-only transaction, which still passes the scope/origin guards
		// for a doc-scoped manager) would otherwise push a phantom StackItem — leaving
		// CanUndo()/CanRedo() true with nothing to revert. (yjs pushes it regardless; we
		// diverge here deliberately to keep the stacks accurate. Real edits always carry
		// an insertion or a deletion, so no genuine undo step is dropped.)
		//
		// This MUST come before the redo-stack clear below: an empty transaction has to be
		// COMPLETELY inert. When it sat after the clear, a no-op tracked transaction wiped
		// the redo stack while adding no undo item, silently destroying a redo the caller
		// could still legitimately perform.
		if len(insertions.clients) == 0 && (trans.deleteSet == nil || len(trans.deleteSet.clients) == 0) {
			return
		}

		if undoing {
			u.StopCapturing() // next undo should not be appended to last stack item
		} else if !redoing {
			// neither undoing nor redoing: clear the redo stack (G6 — selective Clear,
			// which unpins the discarded items so they can be gc'd).
			u.Clear(false, true)
		}

		now := getUnixTime() // ms
		didAdd := false
		// G3: the `lastChange > 0` guard (yjs) — without it a first change after
		// StopCapturing could mis-coalesce.
		if u.LastChange > 0 && Number(now)-u.LastChange < u.CaptureTimeout && len(*stack) > 0 && !undoing && !redoing {
			// append change to last stack op
			lastOp := (*stack)[len(*stack)-1]
			lastOp.deletions = mergeDeleteSets([]*deleteSet{lastOp.deletions, trans.deleteSet})
			lastOp.insertions = mergeDeleteSets([]*deleteSet{lastOp.insertions, insertions})
		} else {
			// create a new stack op
			*stack = append(*stack, newStackItem(trans.deleteSet, insertions))
			didAdd = true
		}

		if !undoing && !redoing {
			u.LastChange = Number(now)
		}

		// make sure that deleted structs are not gc'd
		iterateDeletedStructs(trans, trans.deleteSet, func(s abstractStruct) {
			if it, ok := s.(*itemStruct); ok && u.scopeContains(it) {
				keepItem(it, true)
			}
		})

		u.CurrStackItem = (*stack)[len(*stack)-1]
		obj := newObject()
		obj.Set("stackItem", u.CurrStackItem)
		obj.Set("origin", trans.origin)
		if undoing {
			obj.Set("type", "redo")
		} else {
			obj.Set("type", "undo")
		}
		obj.Set("changedParentTypes", trans.changedParentTypes)
		// G5: emit 'stack-item-updated' on the coalesce/merge branch, not always
		// 'stack-item-added'.
		if didAdd {
			u.Emit("stack-item-added", obj, u)
		} else {
			u.Emit("stack-item-updated", obj, u)
		}
	})
	u.doc.On("afterTransaction", u.afterHandler)
	// Keep a reference to the doc 'destroy' handler so Destroy can unregister it too
	// (yjs UndoManager.destroy offs BOTH afterTransaction and destroy). Without this,
	// a Destroy()'d manager's closure lingers in the doc's 'destroy' observer set for
	// the doc's lifetime, leaking the manager and the structs its stacks reference.
	u.docDestroyHandler = NewObserverHandler(func(v ...interface{}) {
		u.Destroy()
	})
	u.doc.On("destroy", u.docDestroyHandler)

	return u
}

// NewUndoManager constructs an undo manager without exposing the internal item
// representation through a delete-filter callback. The reference-compatible
// allow-all filter is used; scope, capture window, and tracked origins remain
// caller-controlled.
func NewUndoManager(typeScope interface{}, captureTimeout Number, trackedOrigins Set) *UndoManager {
	return newUndoManager(typeScope, captureTimeout, nil, trackedOrigins)
}
