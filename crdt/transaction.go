package crdt

import (
	"fmt"
	"maps"
	"sort"
)

/*
 * A transaction is created for every change on the Yjs model. It is possible
 * to bundle changes on the Yjs model in a single transaction to
 * minimize the number on messages sent and the number of observer calls.
 * If possible the user of this library should bundle as many changes as
 * possible. Here is an example to illustrate the advantages of bundling:
 *
 * @example
 * ----------------------------------------------------------------------------
 *	 const map = y.define('map', YMap)
 *	 // Log content when change is triggered
 *	 map.observe(() => {
 *	   console.Log('change triggered')
 *	 })
 *	 // Each change on the map type triggers a Log message:
 * 	 map.set('a', 0) // => "change triggered"
 *	 map.set('b', 0) // => "change triggered"
 *	 // When put in a transaction, it will trigger the Log after the transaction:
 *	 y.transact(() => {
 *	   map.set('a', 1)
 *	   map.set('b', 1)
 *	 }) // => "change triggered"
 * ----------------------------------------------------------------------------
 */

// noCopy is recognized by go vet's copylocks analyzer. Transaction already contains pointers to
// its own inline storage, so copying it by value would leave the copy referring to the original.
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

type Transaction struct {
	noCopy noCopy

	// The yjs instance
	doc *Doc

	// Describes the set of deleted items by ids
	deleteSet *deleteSet
	// deleteSetStorage backs DeleteSet for the common newly-created transaction. Keeping the
	// public pointer preserves the API while avoiding a second heap object beside Transaction.
	deleteSetStorage deleteSet
	// Most local deletion transactions record at most two ranges. Keep those DeleteItems in
	// the transaction so the reusable, unobserved mutation path does not allocate them.
	deleteItemStorage [2]deleteItem
	// deletePointerStorage backs DeleteSet.Clients for those same compact transactions. Keeping
	// the pointer slice beside its DeleteItems removes a separate one- or two-element allocation
	// without enlarging Transaction: the boolean fields below are packed to reclaim its padding.
	deletePointerStorage [2]*deleteItem
	// Pointers into deleteItemArena are published through DeleteSet.Clients. Reusing the arena via
	// [:0] is sound only because an unobserved package-owned DeleteSet cannot outlive its completed
	// transaction; public/observed transactions are never put back in Doc.mutationTransaction. If
	// DeleteSets ever become retainable after commit, this arena must stop being reused.
	deleteItemArena []deleteItem
	deleteItemCount int

	// Holds the state before the transaction started
	beforeState map[Number]Number

	// Holds the state after the transaction
	afterState map[Number]Number

	// changedTypes holds the materialized changed-type set exposed through ChangedTypes.
	// changedJournal records the same information more compactly until a caller or observer
	// actually needs keyed lookup. Grouping consecutive changes to one type is important for
	// batched map edits: it stores the keys in one string slice instead of hashing each key into
	// a map that an unobserved transaction never reads.
	changedTypes   map[abstractType]ChangedSubs
	changedJournal []changedTypeJournalEntry

	// Stores the events for the types that observe also child elements.
	// It is mainly used by `observeDeep`.
	changedParentTypes map[interface{}][]IEventType

	// Stores the events for the types that observe also child elements.
	// It is mainly used by `observeDeep`.
	mergeStructs []abstractStruct

	origin interface{}

	// Stores meta information on the transaction
	meta map[interface{}]Set

	subdocsAdded   Set
	subdocsRemoved Set
	subdocsLoaded  Set

	// cleanupQueueReusable is true only for an unobserved package-owned
	// mutation with no GC callback. Any path that materializes the public
	// transaction shape or touches subdocuments clears it, preserving the old
	// completed queue for callbacks that may retain it.
	cleanupQueueReusable bool

	// Package-owned, unobserved mutations touch only the local client's clock. Keep that before/
	// after pair inline instead of allocating two complete state-vector maps for every operation.
	// If an observer can receive the transaction, materializeObservableFields reconstructs the
	// complete before-state from the current store and restores this client's original clock.
	compactState bool
	trackChanges bool
	// Whether this change originates from this doc.
	local              bool
	compactStateClient Number
	compactBeforeClock Number
	compactAfterClock  Number
}

type changedTypeJournalEntry struct {
	t    abstractType
	subs []string
}

// Document returns the document whose mutation this transaction records.
func (trans *Transaction) Document() *Doc { return trans.doc }

// Origin returns the caller-supplied transaction origin used for echo
// suppression and undo tracking.
func (trans *Transaction) Origin() any { return trans.origin }

// IsLocal reports whether this transaction originated on the local document.
func (trans *Transaction) IsLocal() bool { return trans.local }

// BeforeState returns a caller-owned snapshot of the state vector immediately
// before the transaction. Observers may retain or modify it without changing
// transaction cleanup state.
func (trans *Transaction) BeforeState() map[Number]Number {
	return maps.Clone(trans.beforeState)
}

// AfterState returns a caller-owned snapshot of the state vector immediately
// after the transaction.
func (trans *Transaction) AfterState() map[Number]Number {
	return maps.Clone(trans.afterState)
}

// Meta returns the transaction-scoped metadata map. Unlike the state and
// subdocument accessors, this map is intentionally live: observers use it to
// pass application metadata between callbacks for the same transaction.
func (trans *Transaction) Meta() map[interface{}]Set { return trans.meta }

// SubdocsAdded returns a caller-owned snapshot of subdocuments added by the
// transaction.
func (trans *Transaction) SubdocsAdded() Set { return maps.Clone(trans.subdocsAdded) }

// SubdocsRemoved returns a caller-owned snapshot of subdocuments removed by
// the transaction.
func (trans *Transaction) SubdocsRemoved() Set { return maps.Clone(trans.subdocsRemoved) }

// SubdocsLoaded returns a caller-owned snapshot of subdocuments requested for
// loading by the transaction.
func (trans *Transaction) SubdocsLoaded() Set { return maps.Clone(trans.subdocsLoaded) }

func newTransaction(doc *Doc, origin interface{}, local bool, materialize bool) *Transaction {
	var trans *Transaction
	if !materialize && !doc.GC && doc.mutationTransaction != nil {
		trans = doc.mutationTransaction
		doc.mutationTransaction = nil
		deleteClients := trans.deleteSetStorage.clients
		if len(deleteClients) != 0 {
			clear(deleteClients)
		}
		deleteClientOrder := trans.deleteSetStorage.clientOrder[:0]
		clear(trans.mergeStructs)
		mergeStructs := trans.mergeStructs[:0]
		deleteItemArena := trans.deleteItemArena[:0]
		*trans = Transaction{}
		trans.deleteSetStorage.clients = deleteClients
		trans.deleteSetStorage.clientOrder = deleteClientOrder
		trans.mergeStructs = mergeStructs
		trans.deleteItemArena = deleteItemArena
	} else {
		trans = &Transaction{}
	}
	trans.doc = doc
	trans.origin = origin
	trans.local = local
	trans.cleanupQueueReusable = !materialize && !doc.GC
	trans.deleteSet = &trans.deleteSetStorage
	if materialize {
		trans.beforeState = getStateVector(doc.store)
		trans.trackChanges = true
		// Remote cleanup must enumerate every changed type even without listeners so
		// YText can remove redundant formatting markers. Materialize immediately:
		// journaling first would pay for both representations on every applied update.
		if !local {
			trans.changedTypes = make(map[abstractType]ChangedSubs)
		}
		trans.materializeObservableFields()
	} else {
		trans.compactState = true
		trans.compactStateClient = doc.ClientID
		trans.compactBeforeClock = getState(doc.store, doc.ClientID)
	}
	return trans
}

// materializeObservableFields ensures the transaction has the same writable,
// non-nil maps and sets exposed by newTransaction. Package-owned mutations may
// defer these allocations, but they call this before any listener can receive
// the transaction. The changed-type set is materialized independently by
// ChangedTypes, because hashing every changed key is unnecessary when neither a
// caller nor an observer reads it.
func (trans *Transaction) materializeObservableFields() {
	trans.cleanupQueueReusable = false
	if trans.beforeState == nil {
		trans.beforeState = getStateVector(trans.doc.store)
		if trans.compactState {
			trans.beforeState[trans.compactStateClient] = trans.compactBeforeClock
		}
	}
	if trans.afterState == nil {
		trans.afterState = make(map[Number]Number)
	}
	if trans.changedParentTypes == nil {
		trans.changedParentTypes = make(map[interface{}][]IEventType)
	}
	if trans.meta == nil {
		trans.meta = make(map[interface{}]Set)
	}
	if trans.subdocsAdded == nil {
		trans.subdocsAdded = NewSet()
	}
	if trans.subdocsRemoved == nil {
		trans.subdocsRemoved = NewSet()
	}
	if trans.subdocsLoaded == nil {
		trans.subdocsLoaded = NewSet()
	}
	if trans.deleteSet.clients == nil {
		trans.deleteSet.clients = make(map[Number][]*deleteItem)
	}
}

// changedTypesInternal returns the writable set of types directly modified by this
// transaction. New types are not included. Each value contains the changed
// parentSub keys; the empty string represents Yjs's null parentSub for list
// changes.
//
// The set is materialized lazily. Mutating the returned map has the same effect
// as mutating the former public Changed field, and later document mutations are
// recorded directly into that same map.
func (trans *Transaction) changedTypesInternal() map[abstractType]ChangedSubs {
	if trans.changedTypes == nil {
		trans.changedTypes = make(map[abstractType]ChangedSubs)
		for _, entry := range trans.changedJournal {
			subs := trans.changedTypes[entry.t]
			if subs == nil {
				subs = newChangedSubs()
				trans.changedTypes[entry.t] = subs
			}
			for _, sub := range entry.subs {
				subs.Add(sub)
			}
		}
		trans.changedJournal = nil
	}
	trans.trackChanges = true
	return trans.changedTypes
}

// ChangedTypes returns a caller-owned projection of the shared types changed by
// this transaction. Both the map and each ChangedSubs set are independent of
// the transaction's internal observer journal.
func (trans *Transaction) ChangedTypes() map[SharedType]ChangedSubs {
	internal := trans.changedTypesInternal()
	result := make(map[SharedType]ChangedSubs, len(internal))
	for shared, subs := range internal {
		result[asSharedType(shared)] = maps.Clone(subs)
	}
	return result
}

func (trans *Transaction) recordChangedType(t abstractType, parentSub string) {
	if trans.changedTypes != nil {
		subs := trans.changedTypes[t]
		if subs == nil {
			subs = newChangedSubs()
			trans.changedTypes[t] = subs
		}
		subs.Add(parentSub)
		return
	}

	n := len(trans.changedJournal)
	if n == 0 || trans.changedJournal[n-1].t != t {
		trans.changedJournal = append(trans.changedJournal, changedTypeJournalEntry{t: t})
		n++
	}
	entry := &trans.changedJournal[n-1]
	entry.subs = append(entry.subs, parentSub)
}

func (trans *Transaction) deleteChangedType(t abstractType) {
	delete(trans.changedTypes, t)
	if len(trans.changedJournal) == 0 {
		return
	}
	kept := trans.changedJournal[:0]
	for _, entry := range trans.changedJournal {
		if entry.t != t {
			kept = append(kept, entry)
		}
	}
	trans.changedJournal = kept
}

func (trans *Transaction) needsChangedTypeDispatch() bool {
	if !trans.local || trans.changedTypes != nil || trans.doc.HasObservers() {
		return true
	}
	for _, entry := range trans.changedJournal {
		if hasTypeObservers(entry.t) {
			return true
		}
	}
	return false
}

func (trans *Transaction) beforeClock(client Number) Number {
	if trans.beforeState != nil {
		return trans.beforeState[client]
	}
	if trans.compactState && client == trans.compactStateClient {
		return trans.compactBeforeClock
	}
	// A package-owned local mutation can only advance its own client's state. Every other
	// client's current clock is therefore still its before-transaction clock.
	return getState(trans.doc.store, client)
}

func (trans *Transaction) addToDeleteSet(client Number, clock Number, length Number) {
	ds := trans.deleteSet
	if ds.clients == nil {
		ds.clients = make(map[Number][]*deleteItem)
	}
	ds.noteClient(client)

	var item *deleteItem
	if trans.deleteItemCount < len(trans.deleteItemStorage) {
		item = &trans.deleteItemStorage[trans.deleteItemCount]
	} else {
		index := trans.deleteItemCount - len(trans.deleteItemStorage)
		if index >= len(trans.deleteItemArena) {
			trans.deleteItemArena = append(trans.deleteItemArena, deleteItem{})
		}
		item = &trans.deleteItemArena[index]
	}
	trans.deleteItemCount++
	*item = deleteItem{clock: clock, length: length}
	deletes := ds.clients[client]
	useCompactPointers := trans.compactState && client == trans.compactStateClient &&
		trans.deleteItemCount <= len(trans.deletePointerStorage) &&
		((trans.deleteItemCount == 1 && cap(deletes) == 0) ||
			(trans.deleteItemCount > 1 && len(deletes) > 0 &&
				&deletes[0] == &trans.deletePointerStorage[0]))
	if useCompactPointers {
		trans.deletePointerStorage[trans.deleteItemCount-1] = item
		ds.clients[client] = trans.deletePointerStorage[:trans.deleteItemCount]
	} else {
		ds.clients[client] = append(deletes, item)
	}
}

func (trans *Transaction) reserveDeleteItems(count int) {
	overflow := count - len(trans.deleteItemStorage)
	if overflow <= 0 || len(trans.deleteItemArena) >= overflow {
		return
	}
	if cap(trans.deleteItemArena) >= overflow {
		trans.deleteItemArena = trans.deleteItemArena[:overflow]
		return
	}
	trans.deleteItemArena = make([]deleteItem, overflow)
}

func (trans *Transaction) reserveDeleteSetClient(client, count Number) {
	if count <= 0 {
		return
	}
	ds := trans.deleteSet
	if ds.clients == nil {
		ds.clients = make(map[Number][]*deleteItem)
	}
	if cap(ds.clients[client]) < count {
		ds.clients[client] = make([]*deleteItem, 0, count)
	}
}

func (trans *Transaction) addSubdocAdded(doc *Doc) {
	trans.cleanupQueueReusable = false
	if trans.subdocsAdded == nil {
		trans.subdocsAdded = NewSet()
	}
	trans.subdocsAdded.Add(doc)
}

func (trans *Transaction) addSubdocRemoved(doc *Doc) {
	trans.cleanupQueueReusable = false
	if trans.subdocsRemoved == nil {
		trans.subdocsRemoved = NewSet()
	}
	trans.subdocsRemoved.Add(doc)
}

func (trans *Transaction) addSubdocLoaded(doc *Doc) {
	trans.cleanupQueueReusable = false
	if trans.subdocsLoaded == nil {
		trans.subdocsLoaded = NewSet()
	}
	trans.subdocsLoaded.Add(doc)
}

// writeUpdateMessageFromTransaction serializes the transaction's structs and
// delete set into encoder. The bool reports whether there was anything to write;
// the error reports a content/delete-set encode failure (so the emit path does
// not broadcast a silently-truncated update).
func writeUpdateMessageFromTransaction(encoder updateEncoder, trans *Transaction) (bool, error) {
	if len(trans.deleteSet.clients) == 0 && !mapAny(trans.afterState, func(client, clock Number) bool {
		return trans.beforeState[client] != clock
	}) {
		return false, nil
	}

	sortAndMergeDeleteSet(trans.deleteSet)
	if err := writeStructsFromTransaction(encoder, trans); err != nil {
		return true, fmt.Errorf("write update from transaction: %w", err)
	}
	// The transaction's delete set is built from real deletions (length >= 1), so
	// this cannot error; surface it rather than swallow it silently.
	if err := writeDeleteSet(encoder, trans.deleteSet); err != nil {
		return true, fmt.Errorf("write update from transaction: delete set encode failed: %w", err)
	}
	return true, nil
}

// If `type.parent` was added in current transaction, `type` technically
// did not change, it was just added and we should not fire events for `type`.
func addChangedTypeToTransaction(trans *Transaction, t abstractType, parentSub string) {
	// Plain-text rendering is cached only for fragmented YText/YXmlText values.
	// Every integrated item mutation flows through this boundary, including
	// remote apply and undo. Invalidate before the unobserved fast return.
	invalidateTextStringCache(t)
	invalidateYMapReadCache(t)
	invalidateYXmlSliceCache(t)
	invalidateListReadIndex(t)
	addChangedTypeAfterInvalidation(trans, t, t.getItem(), parentSub)
}

func addChangedYMapToTransaction(trans *Transaction, y *YMap, parentSub string) {
	invalidateYMapReadCacheValue(y)
	addChangedTypeAfterInvalidation(trans, y, y.item, parentSub)
}

// addChangedMapKeyToTransaction invalidates only projections that depend on map-key values.
// YText/YXmlText string and delta caches, XML child slices, and list indexes depend on list
// content, not root attributes. Custom IAbstractType implementations retain the conservative
// all-cache invalidation performed by addChangedTypeToTransaction.
func addChangedMapKeyToTransaction(trans *Transaction, t abstractType, parentSub string) {
	switch value := t.(type) {
	case *YMap:
		invalidateYMapReadCacheValue(value)
	case *YText, *YXmlText, *YXmlElement:
		// These built-in types expose map attributes, but none of their read projections include them.
	default:
		addChangedTypeToTransaction(trans, t, parentSub)
		return
	}
	addChangedTypeAfterInvalidation(trans, t, t.getItem(), parentSub)
}

func addChangedTypeAfterInvalidation(trans *Transaction, t abstractType, item *itemStruct, parentSub string) {
	if item == nil || (item.id.Clock < trans.beforeClock(item.id.Client) && !item.isDeleted()) {
		if !trans.trackChanges {
			if !hasTypeObservers(t) {
				return
			}
			trans.trackChanges = true
		}
		trans.recordChangedType(t, parentSub)
	}
}

func tryGcDeleteSet(ds *deleteSet, store *structStore, gcFilter func(item *itemStruct) bool) {
	for client, deleteItems := range ds.clients {
		if structs, ok := store.clientStructs(client); ok {
			structs.tryGcDeleteItems(deleteItems, store, gcFilter)
		}
	}
}

func tryMergeDeleteSet(ds *deleteSet, store *structStore) {
	// try to merge deleted / gc'd items
	// merge from right to left for better efficiecy and so we don't miss any merge targets
	for client, deleteItems := range ds.clients {
		if structs, ok := store.clientStructs(client); ok {
			structs.tryMergeDeleteItems(deleteItems)
		}
	}
}

func mergeTransactionClientStructs(store *structStore, client, beforeClock, clock Number) {
	if beforeClock == clock {
		return
	}
	if structs, ok := store.clientStructs(client); ok {
		structs.mergeTransactionRange(beforeClock, clock)
	}
}

func canMergeContentAnyPair(left, right *itemStruct) bool {
	_, leftAny := left.content.(*contentAny)
	_, rightAny := right.content.(*contentAny)
	return leftAny && rightAny && right.origin != nil &&
		right.origin.Client == left.id.Client &&
		right.origin.Clock == left.id.Clock+left.length-1 &&
		left.right == right && CompareIDs(left.rightOrigin, right.rightOrigin) &&
		left.id.Client == right.id.Client && left.id.Clock+left.length == right.id.Clock &&
		left.isDeleted() == right.isDeleted() && left.redone == nil && right.redone == nil
}

func canMergeContentStringPair(left, right *itemStruct) bool {
	_, leftString := left.content.(*contentString)
	_, rightString := right.content.(*contentString)
	return leftString && rightString && left.canMergeWith(right)
}

func cleanupTransactions(transactionCleanups []*Transaction, i Number) {
	if i < len(transactionCleanups) {
		trans := transactionCleanups[i]
		doc := trans.doc
		store := doc.store
		ds := trans.deleteSet
		mergeStructs := trans.mergeStructs

		sortAndMergeDeleteSet(ds)
		dispatchChangedTypes := trans.needsChangedTypeDispatch()
		if trans.beforeState != nil || dispatchChangedTypes {
			trans.materializeObservableFields()
			trans.afterState = getStateVector(trans.doc.store)
		} else {
			trans.compactAfterClock = getState(store, trans.compactStateClient)
		}
		doc.trans = nil
		// A listener may have been registered while a package-owned mutation was
		// in progress. Upgrade before the first document-level callback so it sees
		// the same writable transaction fields as exported Transact provides.
		if doc.HasObservers() {
			trans.materializeObservableFields()
		}
		if doc.HasObserver("beforeObserverCalls") {
			trans.materializeObservableFields()
			doc.Emit("beforeObserverCalls", trans, doc)
		}

		// Snapshot before dispatch. The previous callback queue captured every
		// entry before invoking the first observer, so a callback that directly
		// mutates the map returned by ChangedTypes cannot add/drop callbacks in this pass.
		type changedEntry struct {
			t    abstractType
			subs ChangedSubs
		}
		var changedEntries []changedEntry
		if dispatchChangedTypes {
			changedTypes := trans.changedTypesInternal()
			changedEntries = make([]changedEntry, 0, len(changedTypes))
			for itemType, subs := range changedTypes {
				changedEntries = append(changedEntries, changedEntry{t: itemType, subs: subs})
			}
		}
		for _, entry := range changedEntries {
			t := entry.t
			if t.getItem() == nil || !t.getItem().isDeleted() {
				t.callObserver(trans, entry.subs)
			}
		}

		// Deep-observer dispatch follows direct observers, matching the callback
		// queue order in Yjs. The former implementation first wrapped every call
		// in a heap-allocated closure and then immediately drained that slice;
		// direct iteration has the same ordering and panic behaviour in Go.
		type parentEventEntry struct {
			t      abstractType
			events []IEventType
		}
		parentEntries := make([]parentEventEntry, 0, len(trans.changedParentTypes))
		for rawType, events := range trans.changedParentTypes {
			parentEntries = append(parentEntries, parentEventEntry{t: rawType.(abstractType), events: events})
		}
		for _, entry := range parentEntries {
			t, events := entry.t, entry.events
			// A user may transform the document in a direct observer.
			if t.getItem() != nil && t.getItem().isDeleted() {
				continue
			}
			events = arrayFilter(events, func(e IEventType) bool {
				target := e.targetType()
				return target.getItem() == nil || !target.getItem().isDeleted()
			})
			for _, event := range events {
				event.setCurrentTarget(t)
			}
			// Top-level events are fired before events from nested types.
			sort.Slice(events, func(i, j int) bool {
				return len(events[i].Path()) < len(events[j].Path())
			})
			callEventHandlerListeners(t.getDeepEventHandler(), events, trans)
		}

		if doc.HasObserver("afterTransaction") {
			trans.materializeObservableFields()
			doc.Emit("afterTransaction", trans, doc)
		}

		// Replace deleted items with ItemDeleted / GC.
		// This is where content is actually remove from the Yjs Doc.
		if doc.GC {
			tryGcDeleteSet(ds, store, doc.gcFilter)
		}
		tryMergeDeleteSet(ds, store)

		// On all affected store.clients props, try to merge.
		if trans.afterState != nil {
			for client, clock := range trans.afterState {
				mergeTransactionClientStructs(store, client, trans.beforeClock(client), clock)
			}
		} else {
			mergeTransactionClientStructs(store, trans.compactStateClient, trans.compactBeforeClock, trans.compactAfterClock)
		}

		// try to merge mergeStructs
		// @todo: it makes more sense to transform mergeStructs to a DS, sort it, and merge from right to left
		//        but at the moment DS does not handle duplicates
		for i := 0; i < len(mergeStructs); i++ {
			id := mergeStructs[i].getID()
			if structs, ok := store.clientStructs(id.Client); ok {
				structs.mergeAround(id.Clock)
			}
		}

		if !trans.local && trans.afterState[doc.ClientID] != trans.beforeClock(doc.ClientID) {
			doc.ClientID = generateNewClientID()
		}

		// @todo Merge all the transactions into one and provide send the data as a single update message
		if doc.HasObserver("afterTransactionCleanup") {
			trans.materializeObservableFields()
			doc.Emit("afterTransactionCleanup", trans, doc)
		}
		if doc.HasObserver("update") {
			trans.materializeObservableFields()
			encoder := newUpdateEncoderV1()
			hasContent, err := writeUpdateMessageFromTransaction(encoder, trans)
			if err != nil {
				// Do not broadcast a truncated update on encode failure.
			} else if hasContent {
				out := encoder.toBytes()
				if encErr := encoder.encodeError(); encErr != nil {
				} else {
					doc.Emit("update", out, trans.origin, doc, trans)
				}
			}
		}

		if doc.HasObserver("updateV2") {
			trans.materializeObservableFields()
			encoderV2 := newDefaultUpdateEncoderV2()
			hasContent, err := writeUpdateMessageFromTransaction(encoderV2, trans)
			if err != nil {
			} else if hasContent {
				out := encoderV2.toBytes()
				if encErr := encoderV2.encodeError(); encErr != nil {
				} else {
					doc.Emit("updateV2", out, trans.origin, doc, trans)
				}
			}
		}

		// Only run the subdoc add/delete/emit/destroy block when something actually
		// changed, matching yjs cleanupTransactions which gates the whole block on
		// `subdocsAdded.size || subdocsRemoved.size || subdocsLoaded.size > 0`. Without
		// this, a plain edit (e.g. a Y.Text insert) fires a spurious empty 'subdocs'
		// event every transaction — relevant now that subdocs are functional.
		if len(trans.subdocsAdded) > 0 || len(trans.subdocsRemoved) > 0 || len(trans.subdocsLoaded) > 0 {
			for subdoc := range trans.subdocsAdded {
				// yjs assigns the integrated subdoc the PARENT's clientID
				// (Transaction.js cleanupTransactions: `subdoc.clientID = doc.clientID`),
				// so edits authored inside the subdoc match the parent's author id (wire
				// parity with Yjs). (yjs also inherits collectionid; the Go Doc has no such
				// field, so that part is N/A.)
				subdoc.(*Doc).ClientID = doc.ClientID
				doc.subDocs.Add(subdoc)
			}

			for subdoc := range trans.subdocsRemoved {
				doc.subDocs.Delete(subdoc)
			}

			// yjs emits ['subdocs', [{loaded,added,removed}, doc, transaction]] —
			// pass doc+trans like every sibling event so consumers porting a yjs
			// (changes, doc, transaction) handler get the same args.
			doc.Emit("subdocs", MakeObject(
				"loaded", trans.subdocsLoaded,
				"added", trans.subdocsAdded,
				"removed", trans.subdocsRemoved), doc, trans)

			for subdoc := range trans.subdocsRemoved {
				subdoc.(*Doc).Destroy()
			}
		}

		// Re-read the LIVE field: a nested Transact appends to doc.TransCleanup, and Go's
		// append may reallocate, so the slice VALUE passed into this call would not observe
		// the new entry (JS arrays are references, which is why yjs can just read
		// transactionCleanups.length here). Reading the field is what makes the queueing
		// work instead of the nested transaction being dropped or re-entered.
		pending := doc.transCleanup
		if len(pending) <= i+1 {
			hasAfterAllTransactions := doc.HasObserver("afterAllTransactions")
			if hasAfterAllTransactions {
				trans.materializeObservableFields()
			}
			if !trans.cleanupQueueReusable {
				// Keep the completed slice independent: an afterAllTransactions
				// listener may open a new transaction before it returns.
				doc.transCleanup = nil
			} else {
				// Reuse the single-transaction queue on the common unobserved path.
				// Its length is zero, matching an empty cleanup queue.
				doc.transCleanup = pending[:0]
				doc.mutationTransaction = trans
			}
			if hasAfterAllTransactions {
				doc.Emit("afterAllTransactions", doc, pending)
			}
		} else {
			cleanupTransactions(pending, i+1)
		}
	}
}

// beginTransact enters a transaction and returns whether the caller owns its
// cleanup. Keeping entry/exit separate lets hot package mutations execute their
// operation directly instead of allocating a capturing closure merely to call
// the synchronous transaction core.
func beginTransact(doc *Doc, origin interface{}, local, lean bool) (*Transaction, bool) {
	initialCall := false

	if doc.trans == nil {
		initialCall = true
		// A document observer can see the transaction immediately through the
		// beforeTransaction event, so construct the full public shape in that
		// case. Package-owned, unobserved mutations defer the otherwise-unused
		// event/meta/subdoc maps until an observer actually needs them.
		doc.trans = newTransaction(doc, origin, local, !lean || doc.HasObservers())
		// Append to the SHARED doc.TransCleanup field (yjs `transactionCleanups.push`,
		// where transactionCleanups IS doc._transactionCleanups). The previous code
		// appended to a LOCAL copy that was never written back — and since the field is
		// only ever assigned nil, the read above always yielded nil, so every nested
		// transaction looked like the first one. That made `transactionCleanups[0] ==
		// doc.Trans` trivially true and cleanupTransactions RECURSE instead of queueing
		// behind the outer drain: an observer opening a transaction produced
		// beforeAllTransactions=2 / afterAllTransactions=2 / depth=2 where yjs gives
		// 1/1/1, and the inner transaction's `update` was broadcast to peers BEFORE the
		// update of the transaction that caused it.
		doc.transCleanup = append(doc.transCleanup, doc.trans)
		if len(doc.transCleanup) == 1 {
			if doc.HasObserver("beforeAllTransactions") {
				doc.trans.materializeObservableFields()
				doc.Emit("beforeAllTransactions", doc)
			}
		}

		if doc.HasObserver("beforeTransaction") {
			doc.trans.materializeObservableFields()
			doc.Emit("beforeTransaction", doc.trans, doc)
		}
	}
	return doc.trans, initialCall
}

func finishTransact(doc *Doc, initialCall bool) {
	if !initialCall {
		return
	}
	// yjs computes finishCleanup against transactionCleanups[0] and nulls doc._transaction
	// BEFORE dispatching, so a nested transact (opened by an observer during cleanup) is
	// merely queued — the outer drain picks it up at i+1.
	finishCleanup := len(doc.transCleanup) > 0 && doc.transCleanup[0] == doc.trans
	doc.trans = nil
	if finishCleanup {
		// The first transaction ended, now process observer calls.
		// Observer call may create new transactions for which we need to call the observers and do cleanup.
		// We don't want to nest these calls, so we execute these calls one after
		// another.
		// Also we need to ensure that all cleanups are called, even if the
		// observes throw errors.
		// This file is full of hacky try {} finally {} blocks to ensure that an
		// event can throw errors and also that the cleanup is called.
		cleanupTransactions(doc.transCleanup, 0)
	}
}

// Implements the functionality of `y.transact(()=>{..})`
//
// default parameters: origin = nil, local = true
func transact(doc *Doc, f func(trans *Transaction), origin interface{}, local, lean bool) {
	trans, initialCall := beginTransact(doc, origin, local, lean)
	f(trans)
	finishTransact(doc, initialCall)
}

// transactMutation is for package-owned mutation closures. Unlike exported
// Transact, it may defer observer-only transaction fields while the document is
// unobserved; nested calls reuse whichever transaction the outer caller opened.
func transactMutation(doc *Doc, f func(trans *Transaction), origin interface{}, local bool) {
	transact(doc, f, origin, local, true)
}

// Transact implements the public transaction API with a fully materialized
// Transaction value, including writable empty Meta and subdocument sets.
func Transact(doc *Doc, f func(trans *Transaction), origin interface{}, local bool) {
	transact(doc, f, origin, local, false)
}
