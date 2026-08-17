package crdt

import (
	"errors"
	"fmt"
)

// errNoMissingDependency is the normal GetMissing result once every dependency
// has been resolved. Callers distinguish it only from nil, so reuse one value
// instead of allocating an error for every integrated struct.
var errNoMissingDependency = errors.New("not found creator clientID")

// integrationItemSet is the tiny-set shape used while resolving insertion conflicts. Nearly every
// integration sees only one or two candidates; keeping those pointers inline avoids constructing
// two generic interface hash maps (and two first buckets) per remote item. Pathological/conflict-
// heavy inputs promote to a map, retaining bounded lookup cost.
type integrationItemSet struct {
	inline   [4]*itemStruct
	length   uint8
	overflow map[*itemStruct]struct{}
	// reusable is an empty overflow map retained by the surrounding remote-update
	// integration. It is copied in by value and returned from Release so the
	// scratch owner stays on the stack; storing a pointer to the owner's map field
	// makes even conflict-free Apply calls allocate.
	reusable map[*itemStruct]struct{}
}

// integrationItemScratch amortizes the two conflict-set maps across every Item
// in one integrateStructs call. The maps are cleared before being returned here
// and the scratch itself dies with the update, so it neither retains Items after
// Apply nor adds state to the public Transaction.
type integrationItemScratch struct {
	conflicting map[*itemStruct]struct{}
	before      map[*itemStruct]struct{}
	// borrowed marks the maps as lent to an in-flight integration, so a
	// re-entrant borrower allocates its own rather than sharing one that is still
	// being read. See lend.
	borrowed bool
}

// lend hands the retained maps to one integration's conflict sets, and reports
// whether it did.
//
// It declines while the maps are already lent. The window between lend and
// restore is straight-line today -- it walks o = o.Right calling Add/Has/Reset,
// and the content integration that could re-enter runs after the maps go back --
// so a second borrower cannot currently occur. But nothing except this flag keeps
// that true, and the failure mode if it stops being true is the worst available
// here: a re-entrant borrower would share a map the outer call is still reading,
// corrupting conflict resolution in a way that surfaces only as a rare divergence,
// far from the edit that caused it.
//
// Declining costs one map allocation and yields exactly the behaviour this
// optimization replaced, so the degraded path is the already-correct one rather
// than a new one. Keeping the decision here rather than inline is what makes the
// invariant testable at all: no ordinary input can produce a second borrower, so
// a test has to ask this function directly.
func (s *integrationItemScratch) lend(conflicting, before *integrationItemSet) bool {
	if s == nil || s.borrowed {
		return false
	}
	s.borrowed = true
	conflicting.reusable = s.conflicting
	before.reusable = s.before
	return true
}

// restore takes the cleared maps back and reopens the scratch for lending.
func (s *integrationItemScratch) restore(conflicting, before *integrationItemSet) {
	s.conflicting = conflicting.Release()
	s.before = before.Release()
	s.borrowed = false
}

func (s *integrationItemSet) Add(item *itemStruct) {
	if s.overflow != nil {
		s.overflow[item] = struct{}{}
		return
	}
	for i := 0; i < int(s.length); i++ {
		if s.inline[i] == item {
			return
		}
	}
	if s.length < uint8(len(s.inline)) {
		s.inline[s.length] = item
		s.length++
		return
	}
	if s.reusable != nil {
		s.overflow = s.reusable
	} else {
		s.overflow = make(map[*itemStruct]struct{}, len(s.inline)+1)
	}
	for _, existing := range s.inline {
		s.overflow[existing] = struct{}{}
	}
	s.overflow[item] = struct{}{}
}

func (s *integrationItemSet) Has(item *itemStruct) bool {
	if s.overflow != nil {
		_, ok := s.overflow[item]
		return ok
	}
	for i := 0; i < int(s.length); i++ {
		if s.inline[i] == item {
			return true
		}
	}
	return false
}

func (s *integrationItemSet) Reset() {
	if s.overflow != nil {
		clear(s.overflow)
		s.length = 0
		return
	}
	clear(s.inline[:])
	s.length = 0
}

func (s *integrationItemSet) Release() map[*itemStruct]struct{} {
	reusable := s.reusable
	if s.overflow != nil {
		clear(s.overflow)
		reusable = s.overflow
		s.overflow = nil
	}
	clear(s.inline[:])
	s.length = 0
	s.reusable = nil
	return reusable
}

// Abstract class that represents any content.
type itemStruct struct {
	abstractStructBase
	origin      *ID         // The item that was originally to the left of this item.
	left        *itemStruct // The item that is currently to the left of this item.
	right       *itemStruct // The item that is currently to the right of this item.
	rightOrigin *ID         // The item that was originally to the right of this item.

	// Is a type if integrated, is null if it is possible to copy parent from
	// left or right, is ID before integration to search for it.
	parent interface{} // AbstractType<any> | ID

	// If the parent refers to this item with some kind of key (e.g. YMap, the
	// key is specified here. The key is then used to refer to the list in which
	// to insert this item. If `parentSub = null` type._start is the list in
	// which to insert to. Otherwise it is `parent._map`.
	parentSub string

	// If this type's effect is reundone this type refers to the type that undid
	// this operation.
	redone *ID

	content itemContent

	// bit1..bit4 are public item state. bit5..bit7 are local-only string/index provenance;
	// Item.Write rebuilds the wire byte rather than encoding info directly.
	info uint8
}

const itemInfoStringSplitBacking = bit5

// This is used to mark the item as an indexed fast-search marker
func (item *itemStruct) marker() bool {
	return item.info&bit4 > 0
}

// If true, do not garbage collect this Item.
func (item *itemStruct) keep() bool {
	return item.info&bit1 > 0
}

func (item *itemStruct) countable() bool {
	return item.info&bit2 > 0
}

// Whether this item was deleted or not.
func (item *itemStruct) isDeleted() bool {
	return item.info&bit3 > 0
}

func (item *itemStruct) setMarker(marked bool) {
	item.setInfo(bit4, marked)
}

func (item *itemStruct) setKeep(keep bool) {
	item.setInfo(bit1, keep)
}

func (item *itemStruct) markDeleted() {
	item.info |= bit3
}

func (item *itemStruct) setInfo(pos uint8, on bool) {
	state := item.info&pos > 0
	if state != on {
		item.info ^= pos
	}
}

// Return the creator clientID of the missing op or define missing items and return null.
func (item *itemStruct) missingClient(trans *Transaction, store *structStore) (Number, error) {
	if item.origin != nil && item.origin.Client != item.id.Client && item.origin.Clock >= getState(store, item.origin.Client) {
		return item.origin.Client, nil
	}

	if item.rightOrigin != nil && item.rightOrigin.Client != item.id.Client && item.rightOrigin.Clock >= getState(store, item.rightOrigin.Client) {
		return item.rightOrigin.Client, nil
	}

	// Defense in depth: a typed-nil *ID parent (interface non-nil, pointer nil)
	// passes the old `isIDPtr(item.Parent)` type check yet dereferences to a
	// SIGSEGV. Both struct readers now reject such a parent before it reaches
	// here, but guard with comma-ok + non-nil so no future producer can crash.
	if p, ok := item.parent.(*ID); ok && p != nil && item.id.Client != p.Client && p.Clock >= getState(store, p.Client) {
		return p.Client, nil
	}

	// We have all missing ids, now find the items

	if item.origin != nil {
		item.left = getItemCleanEnd(trans, store, *item.origin)
		if item.left != nil {
			item.origin = item.left.lastID()
		} else {
			item.origin = nil
			item.parent = nil
		}
	}

	if item.rightOrigin != nil {
		item.right = getItemCleanStart(trans, *item.rightOrigin)
		if item.right != nil {
			item.rightOrigin = item.right.getID()
		} else {
			item.rightOrigin = nil
			item.parent = nil
		}
	}

	// yjs源码中，有效；golang版中，无效，因为item.Left 和 item.Right 明确为 *Item 类型
	if (item.left != nil && isGCPtr(item.left)) || (item.right != nil && isGCPtr(item.right)) {
		item.parent = nil
	}

	// only set parent if this shouldn't be garbage collected
	if item.parent == nil {
		if item.left != nil && isItemPtr(item.left) {
			item.parent = item.left.parent
			item.parentSub = item.left.parentSub
		}

		if item.right != nil && isItemPtr(item.right) {
			item.parent = item.right.parent
			item.parentSub = item.right.parentSub
		}
	} else if p, ok := item.parent.(*ID); ok && p != nil {
		// Comma-ok + non-nil (mirrors the guard at line ~95): isIDPtr returns true
		// for a typed-nil *ID (interface non-nil, pointer nil), and *item.Parent.(*ID)
		// then derefs nil -> SIGSEGV. Completes the K1 defense (A#2): latent today
		// since the decoders no longer emit a typed-nil parent, but no future
		// producer can crash here.
		parentItem := getStruct(store, *p)
		// if isGCPtr(parentItem) {
		if !isItemPtr(parentItem) {
			item.parent = nil
		} else {
			contentType, ok := parentItem.(*itemStruct).content.(*contentType)
			if ok {
				item.parent = contentType.value
			} else {
				item.parent = nil
			}
		}
	}

	return 0, errNoMissingDependency
}

func (item *itemStruct) integrateStruct(trans *Transaction, offset Number) error {
	return item.integrateWithScratch(trans, offset, nil)
}

// integrateWithScratch is the remote-update form of Integrate. Direct/local
// callers preserve the standalone behavior through Integrate above; the bulk
// reader passes one update-local scratch to avoid rebuilding large conflict maps
// for each independently integrated Item.
func (item *itemStruct) integrateWithScratch(trans *Transaction, offset Number, scratch *integrationItemScratch) error {
	if offset > 0 {
		contentLength := item.length
		item.id.Clock += offset
		item.left = getItemCleanEnd(trans, trans.doc.store, GenID(item.id.Client, item.id.Clock-1))
		if item.left != nil {
			item.origin = item.left.lastID()
		}
		item.content = spliceContentWithLength(item.content, offset, contentLength)
		item.length -= offset
	}

	// set o to the first conflicting item
	if item.parent != nil {
		if (item.left == nil && (item.right == nil || item.right.left != nil)) ||
			(item.left != nil && item.left.right != item.right) {
			left := item.left

			var o *itemStruct
			if left != nil {
				o = left.right
			} else if item.parentSub != "" {
				o = item.parent.(abstractType).getMap()[item.parentSub]
				for o != nil && o.left != nil {
					o = o.left
				}
			} else {
				o = item.parent.(abstractType).startItem()
			}

			conflictingItems := integrationItemSet{}
			itemsBeforeOrigin := integrationItemSet{}
			lent := scratch.lend(&conflictingItems, &itemsBeforeOrigin)

			// Let c in conflictingItems, b in itemsBeforeOrigin
			// ***{origin}bbbb{this}{c,b}{c,b}{o}***
			// Note that conflictingItems is a subset of itemsBeforeOrigin
			for o != nil && o != item.right {
				itemsBeforeOrigin.Add(o)
				conflictingItems.Add(o)

				if CompareIDs(item.origin, o.origin) {
					// case 1
					if o.id.Client < item.id.Client {
						left = o
						conflictingItems.Reset()
					} else if CompareIDs(item.rightOrigin, o.rightOrigin) {
						// this and o are conflicting and point to the same
						// integration points. The id decides which item comes first.
						// Since this is to the left of o, we can break here
						break
					}
					//} else if o.Origin != nil && itemsBeforeOrigin.Has(GetItem(trans.Doc.Store, *o.Origin)) {
				} else if o.origin != nil {
					// else, o might be integrated before an item that this conflicts with.
					// If so, we will find it in the next iterations
					itemTmp, ok := getStruct(trans.doc.store, *o.origin).(*itemStruct)
					if !ok || !itemsBeforeOrigin.Has(itemTmp) {
						break
					}

					// case 2
					if !conflictingItems.Has(itemTmp) {
						left = o
						conflictingItems.Reset()
					}
				} else {
					break
				}

				o = o.right
			}
			if lent {
				scratch.restore(&conflictingItems, &itemsBeforeOrigin)
			} else {
				conflictingItems.Release()
				itemsBeforeOrigin.Release()
			}
			item.left = left
		}

		// reconnect left/right + update parent map/start if necessary
		if item.left != nil {
			right := item.left.right
			item.right = right
			item.left.right = item
		} else {
			var r *itemStruct
			if item.parentSub != "" {
				r = item.parent.(abstractType).getMap()[item.parentSub]
				for r != nil && r.left != nil {
					r = r.left
				}
			} else {
				r = item.parent.(abstractType).startItem()
				item.parent.(abstractType).setStartItem(item)
			}
			item.right = r
		}

		if item.right != nil {
			item.right.left = item
		} else if item.parentSub != "" {
			// set as current parent value if right === null and this is parentSub
			parent := item.parent.(abstractType)
			parentMap := parent.getMap()
			previous := parentMap[item.parentSub]
			parentMap[item.parentSub] = item
			if previous == nil || previous.isDeleted() {
				adjustYMapSize(parent, 1)
			}
			if item.left != nil {
				// this is the current attribute value of parent. delete right
				item.left.deleteItemStruct(trans)
			}
		}
		if item.parentSub == "" {
			// Count linked nodes, including tombstones and non-countable format Items: both the
			// marker lookup and its bookkeeping walk the physical list rather than visible values.
			updateListItemCount(item.parent.(abstractType), 1)
		}
		// adjust length of parent
		if item.parentSub == "" && item.countable() && !item.isDeleted() {
			item.parent.(abstractType).updateLength(item.length)
		}

		if err := addStruct(trans.doc.store, item); err != nil {
			return err
		}

		item.content.integrateContent(trans, item)
		if item.parentSub == "" && trans.doc.positionIndexes != nil {
			updateListPositionIndexAfterIntegrate(item.parent.(abstractType), item)
		}

		// add parent to transaction.changed
		addChangedTypeToTransaction(trans, item.parent.(abstractType), item.parentSub)
		if item.parent.(abstractType).getItem() != nil && item.parent.(abstractType).getItem().isDeleted() || item.parentSub != "" && item.right != nil {
			// delete if parent is deleted or if this is not the current attribute value of parent
			item.deleteItemStruct(trans)
		}
	} else {
		// parent is not defined. Integrate GC struct instead
		gc := newGC(item.id, item.length)
		if err := gc.integrateStruct(trans, 0); err != nil {
			return err
		}
	}
	return nil
}

// integrateNewMapKey is the conflict-free subset of Integrate for a map key that has
// never existed. There is no left/right chain to resolve, and the item is uncountable,
// so repeating the generic list-position search only performs redundant map lookups.
func (item *itemStruct) integrateNewMapKey(
	trans *Transaction,
	parent abstractType,
	parentMap map[string]*itemStruct,
	clientStructs *clientStructList,
) {
	parentMap[item.parentSub] = item
	adjustYMapSize(parent, 1)
	if clientStructs == nil {
		trans.doc.store.appendClientStruct(item.id.Client, item)
	} else {
		clientStructs.appendValue(item)
	}

	item.content.integrateContent(trans, item)
	addChangedMapKeyToTransaction(trans, parent, item.parentSub)
	if parentItem := parent.getItem(); parentItem != nil && parentItem.isDeleted() {
		item.deleteItemStruct(trans)
	}
}

// integrateNewPrimitiveMapKey is the same fresh-key path for ContentAny. Its
// Integrate hook is intentionally empty, so avoid an interface dispatch for every
// scalar map assignment while retaining the generic hook above for nested types.
func (item *itemStruct) integrateNewPrimitiveMapKey(
	trans *Transaction,
	parent abstractType,
	parentMap map[string]*itemStruct,
	clientStructs *clientStructList,
) {
	parentMap[item.parentSub] = item
	adjustYMapSize(parent, 1)
	if clientStructs == nil {
		trans.doc.store.appendClientStruct(item.id.Client, item)
	} else {
		clientStructs.appendValue(item)
	}

	addChangedMapKeyToTransaction(trans, parent, item.parentSub)
	if parentItem := parent.getItem(); parentItem != nil && parentItem.isDeleted() {
		item.deleteItemStruct(trans)
	}
}

// integratePrimitiveMapOverwrite is the conflict-free subset of Integrate for replacing the
// current tail of a primitive map-key chain. The caller proves that Left is still the current map
// value and has no Right conflict. ContentAny.Integrate is empty, so the remaining work is only
// list linkage, retiring the previous value, storing this struct and recording the changed key.
func (item *itemStruct) integratePrimitiveMapOverwrite(
	trans *Transaction,
	parent abstractType,
	parentMap map[string]*itemStruct,
	clientStructs *clientStructList,
) {
	left := item.left
	left.right = item
	parentMap[item.parentSub] = item
	if left.isDeleted() {
		adjustYMapSize(parent, 1)
	}
	left.deleteItemStruct(trans)

	if clientStructs == nil {
		trans.doc.store.appendClientStruct(item.id.Client, item)
	} else {
		clientStructs.appendValue(item)
	}

	addChangedMapKeyToTransaction(trans, parent, item.parentSub)
	if parentItem := parent.getItem(); parentItem != nil && parentItem.isDeleted() {
		item.deleteItemStruct(trans)
	}
}

// Returns the next non-deleted item
func (item *itemStruct) nextItem() *itemStruct {
	nextItem := item.right
	for nextItem != nil && nextItem.isDeleted() {
		nextItem = nextItem.right
	}

	return nextItem
}

// Returns the previous non-deleted item
func (item *itemStruct) prevItem() *itemStruct {
	prevItem := item.left
	for prevItem != nil && prevItem.isDeleted() {
		prevItem = prevItem.left
	}

	return prevItem
}

// Computes the last content address of this Item
func (item *itemStruct) lastID() *ID {
	// allocating ids is pretty costly because of the amount of ids created, so we try to reuse whenever possible
	if item.length == 1 {
		return &item.id
	}

	id := GenID(item.id.Client, item.id.Clock+item.length-1)
	return &id
}

// Try to merge two items
func (item *itemStruct) mergeStructWith(right abstractStruct) bool {
	indexed := item.info&itemInfoListPositionIndexed != 0
	if !item.mergeWithWithoutReadIndexInvalidation(right) {
		return false
	}
	// Merging removes right from the live list and grows item. A read performed earlier in this
	// transaction may have published immutable positions after the ordinary mutation boundary
	// invalidated them; cleanup must invalidate again at the exact list-surgery point. The mutable
	// search-marker fixup in the merge core repairs the same hazard in place.
	if parent, ok := item.parent.(abstractType); ok {
		if indexed && item.parentSub == "" {
			updateListPositionIndexBeforeMerge(parent, item, right.(*itemStruct))
		}
		invalidateListReadIndex(parent)
	}
	return true
}

func (item *itemStruct) mergeWithWithoutReadIndexInvalidation(right abstractStruct) bool {
	r, ok := right.(*itemStruct)
	if ok &&
		r.origin != nil &&
		r.origin.Client == item.id.Client &&
		r.origin.Clock == item.id.Clock+item.length-1 &&
		item.right == r &&
		CompareIDs(item.rightOrigin, r.rightOrigin) &&
		item.id.Client == r.id.Client &&
		item.id.Clock+item.length == r.id.Clock &&
		item.isDeleted() == r.isDeleted() &&
		item.redone == nil &&
		r.redone == nil &&
		isSameType(item.content, r.content) &&
		item.mergeContentWith(r) {

		parent, ok := item.parent.(abstractType)
		if ok {
			searchMarker := parent.getSearchMarker()
			// Gate on the SLICE (yjs `if (searchMarker)`): nil = markers disabled. The
			// pointer is always non-nil; matches the findMarker/updateMarkerChanges convention.
			if *searchMarker != nil {
				for _, marker := range *searchMarker {
					if marker.p == right {
						// right is going to be "forgotten" so we need to update the marker
						marker.p = item

						// adjust marker index
						if !item.isDeleted() && item.countable() {
							marker.index -= item.length
						}
					}
				}
			}
		}

		if r.keep() {
			item.setKeep(true)
		}
		item.right = r.right
		if item.right != nil {
			item.right.left = item
		}

		item.length += r.length
		if item.parentSub == "" {
			updateListItemCount(parent, -1)
		}
		return true
	}

	return false
}

// mergeContentWith recognizes the one content relationship that can be merged
// without copying: a right-hand string fragment created by splitItem from the
// same immutable backing string. itemInfoStringSplitBacking is internal
// provenance only; Item.Write reconstructs wire info from content/origin metadata
// and never serializes it. The bit is required even though mergeSplitRight also
// checks pointer adjacency: independent heap allocations may be neighbours.
func (item *itemStruct) mergeContentWith(right *itemStruct) bool {
	if right.info&itemInfoStringSplitBacking != 0 {
		leftString, leftOK := item.content.(*contentString)
		rightString, rightOK := right.content.(*contentString)
		if leftOK && rightOK && leftString.mergeSplitRight(rightString, item.length, right.length) {
			return true
		}
	}
	return item.content.mergeContentWith(right.content)
}

func (item *itemStruct) canMergeWith(r *itemStruct) bool {
	return r != nil &&
		r.origin != nil &&
		r.origin.Client == item.id.Client &&
		r.origin.Clock == item.id.Clock+item.length-1 &&
		item.right == r &&
		CompareIDs(item.rightOrigin, r.rightOrigin) &&
		item.id.Client == r.id.Client &&
		item.id.Clock+item.length == r.id.Clock &&
		item.isDeleted() == r.isDeleted() &&
		item.redone == nil &&
		r.redone == nil &&
		isSameType(item.content, r.content)
}

// completeMergeWith applies the metadata and marker half of MergeWith after the content has
// already been combined. Keeping this separate lets transaction cleanup coalesce a run of string
// contents once instead of repeatedly copying the growing left prefix.
func (item *itemStruct) completeMergeWith(r *itemStruct) {
	parent, ok := item.parent.(abstractType)
	if ok {
		// This bulk-string path bypasses MergeWith after combining content, but performs the
		// same list surgery and therefore owns the same read-index invalidation obligation.
		invalidateListReadIndex(parent)
		searchMarker := parent.getSearchMarker()
		// Gate on the SLICE (yjs `if (searchMarker)`): nil = markers disabled. The
		// pointer is always non-nil; matches the findMarker/updateMarkerChanges convention.
		if *searchMarker != nil {
			for _, marker := range *searchMarker {
				if marker.p == r {
					// right is going to be "forgotten" so we need to update the marker
					marker.p = item

					// adjust marker index
					if !item.isDeleted() && item.countable() {
						marker.index -= item.length
					}
				}
			}
		}
	}

	if r.keep() {
		item.setKeep(true)
	}
	item.right = r.right
	if item.right != nil {
		item.right.left = item
	}

	item.length += r.length
	if item.parentSub == "" && ok {
		updateListItemCount(parent, -1)
	}
}

// Mark this Item as deleted.
func (item *itemStruct) deleteItemStruct(trans *Transaction) {
	if !item.isDeleted() {
		parent := item.parent
		oldVisibleLength := itemVisibleLength(item)
		_, wasFormat := item.content.(*contentFormat)

		// adjust the length of parent
		if item.countable() && item.parentSub == "" {
			parent.(abstractType).updateLength(-item.length)
		} else if item.parentSub != "" {
			parentType := parent.(abstractType)
			if parentType.getMap()[item.parentSub] == item {
				adjustYMapSize(parentType, -1)
			}
		}
		item.markDeleted()
		if item.parentSub == "" && trans.doc.positionIndexes != nil {
			if wasFormat {
				destroyListPositionIndex(parent.(abstractType))
			} else if oldVisibleLength > 0 {
				updateListPositionIndexAfterDelete(parent.(abstractType), item, oldVisibleLength)
			}
		}
		trans.addToDeleteSet(item.id.Client, item.id.Clock, item.length)
		if item.parentSub == "" {
			addChangedTypeToTransaction(trans, parent.(abstractType), "")
		} else {
			addChangedMapKeyToTransaction(trans, parent.(abstractType), item.parentSub)
		}
		item.content.deleteContent(trans)
	}
}

func (item *itemStruct) gcItem(store *structStore, parentGCd bool) {
	if !item.isDeleted() {
		return
	}

	item.content.gcContent(store)
	if parentGCd {
		_ = replaceStruct(store, item, newGC(item.id, item.length))
	} else {
		item.content = newContentDeleted(item.length)
	}
}

// Transform the properties of this type to binary and write it to an
// BinaryEncoder.
//
// This is called when this Item is sent to a remote peer.
func (item *itemStruct) writeStruct(encoder updateEncoder, offset Number) error {
	origin := item.origin
	if offset > 0 {
		id := GenID(item.id.Client, item.id.Clock+offset-1)
		origin = &id
	}

	rightOrigin := item.rightOrigin
	parentSub := item.parentSub
	info := item.content.contentRef() & bits5
	if origin != nil {
		info |= bit8
	}
	if rightOrigin != nil {
		info |= bit7
	}
	if parentSub != "" {
		info |= bit6
	}
	encoder.writeInfo(info)
	if origin != nil {
		encoder.writeLeftID(origin)
	}

	if rightOrigin != nil {
		encoder.writeRightID(rightOrigin)
	}

	if origin == nil && rightOrigin == nil {
		parent := item.parent

		if isAbstractType(parent) && !isYString(parent) && !isIDPtr(parent) {
			parentItem := parent.(abstractType).getItem()
			if parentItem == nil {
				// parent type on y._map
				// find the correct key
				ykey := findRootTypeKey(parent.(abstractType))
				encoder.writeParentInfo(true)
				if err := encoder.writeStringValue(ykey); err != nil {
					return err
				}
			} else {
				encoder.writeParentInfo(false)
				encoder.writeLeftID(&parentItem.id)
			}
		} else if isYString(parent) {
			encoder.writeParentInfo(true)
			if err := encoder.writeStringValue(parent.(*yString).str); err != nil {
				return err
			}
		} else if isIDPtr(parent) && parent.(*ID) != nil {
			// Guard against a TYPED-NIL *ID parent: isIDPtr matches on type, so a
			// nil *ID (e.g. from a swallowed decode error upstream) would pass the
			// type check and WriteLeftID would deref a nil id → SIGSEGV. The lazy
			// reader now fails such structs before they reach encode; this guard is
			// defense in depth so a nil *ID parent is never dereferenced here.
			encoder.writeParentInfo(false)
			encoder.writeLeftID(parent.(*ID))
		} else {
			// origin==nil && rightOrigin==nil REQUIRES a parent (root type / yString /
			// non-nil *ID). Reaching here means an invalid/nil parent, which would
			// otherwise silently emit a MISALIGNED frame (no parent info written) that
			// corrupts the decoder's struct stream. Fail loudly instead.
			return fmt.Errorf("write struct: item %v has nil origin/rightOrigin and invalid parent %T", item.id, parent)
		}

		if parentSub != "" {
			if err := encoder.writeStringValue(parentSub); err != nil {
				return err
			}
		}
	}

	// Surface a content-encode failure (any/buf/json/key write error) instead of
	// dropping it and emitting a silently-truncated item.
	return item.content.writeContent(encoder, offset)
}

func newItem(id ID, left *itemStruct, origin *ID, right *itemStruct, rightOrigin *ID,
	parent interface{}, parentSub string, content itemContent) *itemStruct {
	if content == nil {
		return nil
	}
	return newItemWithLength(id, left, origin, right, rightOrigin, parent, parentSub, content, content.contentLength())
}

func newItemWithLength(id ID, left *itemStruct, origin *ID, right *itemStruct, rightOrigin *ID,
	parent interface{}, parentSub string, content itemContent, length Number) *itemStruct {
	return initItemWithLength(&itemStruct{}, id, left, origin, right, rightOrigin, parent, parentSub, content, length)
}

func initItemWithLength(item *itemStruct, id ID, left *itemStruct, origin *ID, right *itemStruct, rightOrigin *ID,
	parent interface{}, parentSub string, content itemContent, length Number) *itemStruct {

	if content == nil {
		return nil
	}

	info := uint8(0)
	if content.isCountable() {
		info = bit2
	}

	*item = itemStruct{
		abstractStructBase: abstractStructBase{
			id:     id,
			length: length,
		},
		left:        left,
		origin:      origin,
		right:       right,
		rightOrigin: rightOrigin,
		parent:      parent,
		parentSub:   parentSub,
		content:     content,
		info:        info,
	}
	return item
}

func spliceContentWithLength(content itemContent, offset, length Number) itemContent {
	if str, ok := content.(*contentString); ok {
		return str.spliceWithLength(offset, length)
	}
	return content.spliceContent(offset)
}

// This should return several items
func followRedone(store *structStore, id ID) (*itemStruct, Number) {
	nextID := &id
	diff := 0
	var item *itemStruct
	for {
		if diff > 0 {
			newID := GenID(nextID.Client, nextID.Clock+diff)
			nextID = &newID
		}

		it, ok := getStruct(store, *nextID).(*itemStruct)
		if !ok {
			break
		}

		item = it
		diff = nextID.Clock - item.id.Clock
		nextID = item.redone

		if nextID == nil {
			break
		}
	}

	return item, diff
}

// Make sure that neither item nor any of its parents is ever deleted.
//
// This property does not persist when storing it into a database or when
// sending it to other peers
func keepItem(item *itemStruct, keep bool) {
	for item != nil && item.keep() != keep {
		item.setKeep(keep)
		item = item.parent.(abstractType).getItem()
	}
}

// Split leftItem into two items.
func splitItem(trans *Transaction, leftItem *itemStruct, diff Number) *itemStruct {
	client, clock := leftItem.id.Client, leftItem.id.Clock

	var parent abstractType
	if leftItem.parent == nil {
		parent = nil
	} else {
		parent = leftItem.parent.(abstractType)
	}

	rightLength := leftItem.length - diff
	var rightItem *itemStruct
	if leftString, ok := leftItem.content.(*contentString); ok {
		storage := trans.doc.allocateStringItemStorage()
		origin := trans.doc.allocateItemOriginStorage()
		*origin = GenID(client, clock+diff-1)
		rightContent, sharesSplitBacking := leftString.spliceWithLengthIntoBacking(
			diff, leftItem.length, &storage.content,
		)
		rightItem = initItemWithLength(&storage.item, GenID(client, clock+diff), leftItem, origin,
			leftItem.right, leftItem.rightOrigin, parent, leftItem.parentSub, rightContent, rightLength)
		if sharesSplitBacking {
			rightItem.info |= itemInfoStringSplitBacking
		}
	} else {
		originID := GenID(client, clock+diff-1)
		rightContent := spliceContentWithLength(leftItem.content, diff, leftItem.length)
		rightItem = newItemWithLength(GenID(client, clock+diff), leftItem, &originID,
			leftItem.right, leftItem.rightOrigin, parent, leftItem.parentSub, rightContent, rightLength)
	}

	if leftItem.isDeleted() {
		rightItem.markDeleted()
	}

	if leftItem.keep() {
		rightItem.setKeep(true)
	}

	if leftItem.redone != nil {
		id := GenID(leftItem.redone.Client, leftItem.redone.Clock+diff)
		rightItem.redone = &id
	}

	// update left (do not set leftItem.rightOrigin as it will lead to problems when syncing)
	leftItem.right = rightItem

	// update right
	if rightItem.right != nil {
		rightItem.right.left = rightItem
	}
	// right is more specific.
	trans.mergeStructs = append(trans.mergeStructs, rightItem)

	// update parent._map
	if rightItem.parentSub != "" && rightItem.right == nil {
		rightItem.parent.(abstractType).getMap()[rightItem.parentSub] = rightItem
	}

	leftItem.length = diff
	if parent != nil && rightItem.parentSub == "" {
		updateListItemCount(parent, 1)
		if trans.doc.positionIndexes != nil {
			updateListPositionIndexAfterSplit(parent, rightItem)
		}
	}
	return rightItem
}

// Redoes the effect of this operation.
// redoItem re-creates a previously-undone item, a faithful port of yjs@13.6.31
// src/structs/Item.js redoItem (6-arg). The map branch uses itemsToDelete + the
// undo/redo stacks (isDeletedByUndoStack) to tell a concurrent REMOTE map write
// (which must block the redo, preserving the remote change) apart from one this
// manager is itself undoing (which the redo may overwrite). ignoreRemoteMapChanges
// opts into overwriting remote map writes. The #757 cross-parent-left drop and the
// parent-redone tracing match upstream; the old "seed-left" placeholder hack is
// gone (replaced by this faithful logic).
func redoItem(trans *Transaction, item *itemStruct, redoItems Set, itemsToDelete *deleteSet, ignoreRemoteMapChanges bool, um *UndoManager) *itemStruct {
	doc := trans.doc
	store := doc.store
	ownClientID := doc.ClientID
	redone := item.redone
	if redone != nil {
		return getItemCleanStart(trans, *redone)
	}

	parentItem := item.parent.(abstractType).getItem()
	var left *itemStruct
	var right *itemStruct

	// make sure that parent is redone
	if parentItem != nil && parentItem.isDeleted() {
		// try to undo parent if it will be undone anyway
		if parentItem.redone == nil && (!redoItems.Has(parentItem) || redoItem(trans, parentItem, redoItems, itemsToDelete, ignoreRemoteMapChanges, um) == nil) {
			return nil
		}
		for parentItem.redone != nil {
			parentItem = getItemCleanStart(trans, *parentItem.redone)
			// Go's GetItemCleanStart returns nil for a missing target (yjs
			// getItemCleanStart throws); bail gracefully instead of nil-panicking on
			// the next Redone read. Unreachable in normal operation (redone targets are
			// KeepItem-pinned), but guards a corrupted/partially-synced redone chain.
			if parentItem == nil {
				return nil
			}
		}
	}

	var parentType abstractType
	if parentItem == nil {
		parentType = item.parent.(abstractType)
	} else if pc, ok := parentItem.content.(*contentType); ok {
		parentType = pc.value
	} else {
		// Parent content isn't a type; cannot redo (defensive — shouldn't happen for
		// a well-formed local structure).
		return nil
	}

	if item.parentSub == "" {
		// Array item: insert at the old position, tracing redone chains until the
		// neighbor's parent matches the (possibly redone) parent.
		left = item.left
		right = item
		for left != nil {
			leftTrace := left
			for leftTrace != nil && leftTrace.parent.(abstractType).getItem() != parentItem {
				if leftTrace.redone == nil {
					leftTrace = nil
				} else {
					leftTrace = getItemCleanStart(trans, *leftTrace.redone)
				}
			}
			if leftTrace != nil && leftTrace.parent.(abstractType).getItem() == parentItem {
				left = leftTrace
				break
			}
			left = left.left
		}
		for right != nil {
			rightTrace := right
			for rightTrace != nil && rightTrace.parent.(abstractType).getItem() != parentItem {
				if rightTrace.redone == nil {
					rightTrace = nil
				} else {
					rightTrace = getItemCleanStart(trans, *rightTrace.redone)
				}
			}
			if rightTrace != nil && rightTrace.parent.(abstractType).getItem() == parentItem {
				right = rightTrace
				break
			}
			right = right.right
		}
	} else {
		// Map item: insert as the current value for the key.
		right = nil
		if item.right != nil && !ignoreRemoteMapChanges {
			// Walk right past items that are going to be deleted anyway (in
			// itemsToDelete, or deleted by this manager's undo/redo stacks). If a live
			// remote item remains to the right, redoing would clobber it — bail.
			left = item
			for left != nil && left.right != nil &&
				(left.right.redone != nil ||
					isDeleted(itemsToDelete, left.right.getID()) ||
					isDeletedByUndoStack(um.UndoStack, left.right.getID()) ||
					isDeletedByUndoStack(um.RedoStack, left.right.getID())) {
				left = left.right
				for left.redone != nil {
					left = getItemCleanStart(trans, *left.redone)
					if left == nil {
						return nil // missing redone target (see parent-redone loop)
					}
				}
			}
			if left != nil && left.right != nil {
				// conflicts with a change from another client
				return nil
			}
		} else {
			left = parentType.getMap()[item.parentSub] // nil if absent
		}
		// #757: drop a cross-parent left so its origin doesn't mislead the remote.
		if left != nil && left.parent.(abstractType).getItem() != parentItem {
			left = parentType.getMap()[item.parentSub]
		}
	}

	nextClock := getState(store, ownClientID)
	nextID := GenID(ownClientID, nextClock)
	redoneItem := newItem(nextID, left, getItemLastID(left), right, getItemID(right), parentType, item.parentSub, item.content.copyContent())
	item.redone = &redoneItem.id
	keepItem(redoneItem, true)
	if err := redoneItem.integrateStruct(trans, 0); err != nil {
		return nil
	}
	return redoneItem
}

func getItemID(item *itemStruct) *ID {
	if item != nil {
		return &item.id
	}

	return nil
}

func getItemLastID(item *itemStruct) *ID {
	if item != nil {
		return item.lastID()
	}

	return nil
}
