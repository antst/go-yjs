package crdt

import (
	"errors"
	"strings"
	"sync/atomic"
	"unicode/utf8"
	"unsafe"
)

// deltaTextAccumulator coalesces adjacent ContentString values without allocating
// for a single-item segment. Fragmented segments share one append-only byte arena,
// so a formatted delta pays for the text storage once rather than once per op.
// Strings already returned by Take only cover the immutable prefix written for that
// segment; later appends never mutate those bytes, and a grow leaves the old backing
// array alive through the returned string.
type deltaTextAccumulator struct {
	one          string
	buf          []byte
	segmentStart int
	capacityHint int
	count        int
}

func (a *deltaTextAccumulator) Add(s string) {
	switch a.count {
	case 0:
		a.one = s
	case 1:
		if a.buf == nil && a.capacityHint > 0 {
			a.buf = make([]byte, 0, a.capacityHint)
		}
		a.segmentStart = len(a.buf)
		a.buf = append(a.buf, a.one...)
		a.buf = append(a.buf, s...)
		a.one = ""
	default:
		a.buf = append(a.buf, s...)
	}
	a.count++
}

func (a *deltaTextAccumulator) Empty() bool { return a.count == 0 }

func (a *deltaTextAccumulator) Take() string {
	if a.count == 0 {
		return ""
	}
	var result string
	if a.count == 1 {
		result = a.one
	} else {
		segment := a.buf[a.segmentStart:]
		result = unsafe.String(unsafe.SliceData(segment), len(segment))
	}
	a.one = ""
	a.segmentStart = len(a.buf)
	a.count = 0
	return result
}

type itemTextListPosition struct {
	left              *itemStruct
	right             *itemStruct
	index             Number
	currentAttributes Object
}

func (it *itemTextListPosition) updateCurrentAttributes(format *contentFormat) {
	if it.currentAttributes.IsNil() {
		it.currentAttributes = newObject()
	}
	updateCurrentAttributes(it.currentAttributes, format)
}

// Only call this if you know that this.right is defined
func (it *itemTextListPosition) forward() error {
	right := it.right
	if right == nil {
		return errors.New("unexpected case")
	}

	// yjs forward() (src/types/YText.js): `case ContentFormat` / `default`. Every content
	// kind that is not a format advances the index by its length — a whitelist here left
	// non-whitelisted kinds uncounted and mis-located every later position (FR-014d).
	if format, ok := right.content.(*contentFormat); ok {
		if right.info&bit3 == 0 {
			it.updateCurrentAttributes(format)
		}
	} else if right.info&bit3 == 0 {
		it.index += right.length
	}

	it.left = right
	it.right = right.right

	return nil
}

func findNextPosition(trans *Transaction, pos *itemTextListPosition, count Number) {
	for right := pos.right; right != nil && count > 0; right = pos.right {
		// yjs findNextPosition: `case ContentFormat` / `default`. All non-format content
		// advances the walk by its length (FR-014d).
		if format, ok := right.content.(*contentFormat); ok {
			if right.info&bit3 == 0 {
				pos.updateCurrentAttributes(format)
			}
		} else if right.info&bit3 == 0 {
			if count < right.length {
				// split right
				getItemCleanStart(trans, GenID(right.id.Client, right.id.Clock+count))
			}
			pos.index += right.length
			count -= right.length
		}

		pos.left = right
		pos.right = right.right

		// pos.forward() - we don't forward because that would halve the performance because we already do the checks above
	}
}

// findPosition resolves the itemTextListPosition at index. When useSearchMarker is
// false it walks from the start, so CurrentAttributes accumulates the formatting
// active at index — required by format() and attributed insert() to negate
// correctly (yjs findPosition's useSearchMarker arg). With a search marker the walk
// can start mid-run and CurrentAttributes would be incomplete, producing a wrong
// negated attribute value.
func findPositionValue(trans *Transaction, parent abstractType, index Number, useSearchMarker bool, currentAttributes Object) itemTextListPosition {
	if useSearchMarker {
		// Formatting disables the reference marker cache because a marker knows its visible index but
		// not the attributes active there. At large physical sizes the block index reconstructs that
		// prefix from live ContentFormat Items before returning the same bounded linked walk.
		markers := parent.getSearchMarker()
		if markers != nil && *markers == nil {
			itemCount := listItemCount(parent)
			if itemCount >= buildFormattedListPositionIndexItems {
				item, start, attributes, ok := indexedFormattedTextPosition(
					parent, index, itemCount, currentAttributes,
				)
				if ok {
					pos := itemTextListPosition{
						left: item.left, right: item, index: start, currentAttributes: attributes,
					}
					findNextPosition(trans, &pos, index-start)
					return pos
				}
			}
		}
		if item, start, ok := findMutationPosition(parent, index); ok {
			pos := itemTextListPosition{left: item.left, right: item, index: start, currentAttributes: currentAttributes}
			findNextPosition(trans, &pos, index-start)
			return pos
		}
	}
	pos := itemTextListPosition{right: parent.startItem(), currentAttributes: currentAttributes}
	findNextPosition(trans, &pos, index)
	return pos
}

func findPositionWithCurrentAttributes(trans *Transaction, parent abstractType, index Number, useSearchMarker bool, currentAttributes Object) *itemTextListPosition {
	pos := findPositionValue(trans, parent, index, useSearchMarker, currentAttributes)
	return &pos
}

func findPosition(trans *Transaction, parent abstractType, index Number, useSearchMarker bool) *itemTextListPosition {
	// Allocate the ordered attribute object only if the walk encounters a live
	// ContentFormat. Plain-text positioning carries the zero object read-only.
	return findPositionWithCurrentAttributes(trans, parent, index, useSearchMarker, Object{})
}

// isNullAttr mirrors yjs `x === null` for an attribute/format value, which the Go
// port represents as either the Null sentinel or (defensively) a Go nil.
func isNullAttr(v any) bool { return v == nil || isNull(v) }

const localFormatItemArenaThreshold = 8

type insertTextObjectScratch struct {
	current    objectData
	attributes objectData
	negated    objectData
}

func acquireTextMutationScratch(doc *Doc) *insertTextObjectScratch {
	if doc.textMutationScratch == nil {
		doc.textMutationScratch = &insertTextObjectScratch{}
	} else {
		*doc.textMutationScratch = insertTextObjectScratch{}
	}
	return doc.textMutationScratch
}

func newLocalFormatItem(trans *Transaction, left, right *itemStruct, parent abstractType, key string, value any) *itemStruct {
	doc := trans.doc
	storage := doc.allocateFormatItemStorage()
	storage.content = contentFormat{key: key, value: value}
	var origin *ID
	if left != nil {
		if left.length == 1 {
			origin = &left.id
		} else {
			origin = doc.allocateItemOriginStorage()
			*origin = GenID(left.id.Client, left.id.Clock+left.length-1)
		}
	}
	return initItemWithLength(&storage.item, GenID(doc.ClientID, getState(doc.store, doc.ClientID)),
		left, origin, right, getItemID(right), parent, "", &storage.content, 1)
}

// Negate applied formats
func insertNegatedAttributes(trans *Transaction, parent abstractType, currPos *itemTextListPosition, negatedAttributes Object) {
	// check if we really need to remove attributes.
	// NB: GetOr (→ Go nil) is deliberate here, NOT GetOrNull (→ Null sentinel) — it mirrors yjs
	// `negatedAttributes.get(key)` (a bare get, no `?? null`). Switching to GetOrNull "for
	// consistency" with the other format reads would diverge from Yjs on the negation pre-pass.
	for currPos.right != nil && (currPos.right.isDeleted() ||
		(isSameType(currPos.right.content, &contentFormat{}) &&
			equalAttrs(negatedAttributes.GetOr(currPos.right.content.(*contentFormat).key), currPos.right.content.(*contentFormat).value))) {
		if !currPos.right.isDeleted() {
			negatedAttributes.Delete(currPos.right.content.(*contentFormat).key)
		}
		_ = currPos.forward()
	}

	negatedAttributes.Range(func(key string, value any) {
		left := currPos.left
		right := currPos.right
		nextFormat := newLocalFormatItem(trans, left, right, parent, key, value)
		_ = nextFormat.integrateStruct(trans, 0)
		currPos.right = nextFormat
		_ = currPos.forward()
	})
}

func updateCurrentAttributes(currentAttributes Object, format *contentFormat) {
	key, value := format.key, format.value
	// A null value clears the attribute (yjs updateCurrentAttributes: value===null
	// → delete). The negation pre-pass / insertAttributes encode a cleared attribute
	// with the package Null sentinel (NullType{}), which is NOT Go nil — so both must
	// count as "delete", else a negated attribute (e.g. bold:null) would leak into
	// the computed delta instead of resetting the formatting.
	if value == nil || isNull(value) {
		currentAttributes.Delete(key)
	} else {
		currentAttributes.Set(key, value)
	}
}

func minimizeAttributeChanges(currPos *itemTextListPosition, attributes Object) {
	// go right while attributes[right.key] === right.value (or right is deleted)
	if currPos.right == nil {
		return
	}

	isEqual := func(_ itemContent, attributes Object) bool {
		cf, ok := currPos.right.content.(*contentFormat)
		if !ok {
			return false
		}

		// yjs: equalAttrs(attributes[key] ?? null, value) — coalesce absent to Null.
		return equalAttrs(attributes.GetOrNull(cf.key), cf.value)
	}

	for currPos.right != nil && (currPos.right.isDeleted() || isEqual(currPos.right.content, attributes)) {
		_ = currPos.forward()
	}
}

func insertAttributes(trans *Transaction, parent abstractType, currPos *itemTextListPosition, attributes Object) Object {
	return insertAttributesWithScratch(trans, parent, currPos, attributes, nil)
}

func insertAttributesWithScratch(trans *Transaction, parent abstractType, currPos *itemTextListPosition,
	attributes Object, negatedData *objectData) Object {
	negatedAttributes := Object{}
	// insert format-start items
	attributes.Range(func(key string, val any) {
		// yjs: const currentVal = currPos.currentAttributes.get(key) ?? null. Coalesce
		// absent to Null so equalAttrs matches yjs equalAttrs(null, null) — else
		// formatting key->null on text lacking key inserts a redundant null marker.
		currentVal := currPos.currentAttributes.GetOrNull(key)
		if val == nil {
			val = Null // normalize a Go-nil attribute value to the Null sentinel
		}

		if !equalAttrs(currentVal, val) {
			// yjs: negatedAttributes.set(key, currentVal) (currentVal already ?? null)
			if negatedAttributes.IsNil() {
				if negatedData == nil {
					negatedAttributes = newObject()
				} else {
					*negatedData = objectData{}
					negatedAttributes = Object{d: negatedData}
				}
			}
			negatedAttributes.Set(key, currentVal)
			left, right := currPos.left, currPos.right

			currPos.right = newLocalFormatItem(trans, left, right, parent, key, val)
			_ = currPos.right.integrateStruct(trans, 0)
			_ = currPos.forward()
		}
	})

	return negatedAttributes
}

func insertText(trans *Transaction, parent abstractType, currPos *itemTextListPosition, text interface{}, attributes Object) {
	insertTextWithScratch(trans, parent, currPos, text, attributes, nil)
}

func insertTextWithScratch(trans *Transaction, parent abstractType, currPos *itemTextListPosition,
	text interface{}, attributes Object, scratch *insertTextObjectScratch) {
	// yjs YText.js insertText: a String becomes ContentString, an AbstractType
	// becomes ContentType (a nested type embedded in the text), else ContentEmbed.
	switch t := text.(type) {
	case string:
		insertTextString(trans, parent, currPos, t, attributes, scratch)
		return
	case abstractType:
		insertTextContent(trans, parent, currPos, newContentType(t), attributes, scratch)
		return
	default:
		storage := trans.doc.allocateEmbedItemStorage()
		storage.content.embed = text
		insertTextContentWithItem(trans, parent, currPos, &storage.content, &storage.item, attributes, scratch)
		return
	}
}

func insertTextString(trans *Transaction, parent abstractType, currPos *itemTextListPosition, text string,
	attributes Object, scratch *insertTextObjectScratch) {
	storage := trans.doc.allocateStringItemStorage()
	storage.content = contentString{value: text}
	insertTextContentWithItem(trans, parent, currPos, &storage.content, &storage.item, attributes, scratch)
}

func insertTextContent(trans *Transaction, parent abstractType, currPos *itemTextListPosition,
	content itemContent, attributes Object, scratch *insertTextObjectScratch) {
	insertTextContentWithItem(trans, parent, currPos, content, nil, attributes, scratch)
}

func insertTextContentWithItem(trans *Transaction, parent abstractType, currPos *itemTextListPosition,
	content itemContent, itemStorage *itemStruct, attributes Object, scratch *insertTextObjectScratch) {
	// Clone before the in-place negation pre-pass below. yjs mutates the caller's
	// attributes object too, but JS callers pass a fresh literal per op; Go Objects
	// share backing storage, so reusing one across inserts/delta ops would carry the
	// synthetic Null clears into later operations. Cloning is parity-neutral (the same
	// ops run on the copy) and isolates the caller's object.
	if !attributes.IsNil() {
		if scratch != nil && attributes.d.large == nil {
			scratch.attributes = *attributes.d
			attributes = Object{d: &scratch.attributes}
		} else {
			attributes = attributes.ShallowClone()
		}
	}
	// Negation pre-pass (yjs src/types/YText.js insertText): for every attribute
	// active at the cursor but NOT named in the incoming attributes, force it to the
	// Null sentinel so the inserted content RESETS that formatting instead of
	// inheriting it. Without this, an unattributed insert inside a formatted run
	// (e.g. bold) bleeds the surrounding formatting — reachable via ApplyDelta /
	// InsertEmbed, which bypass the top-level Insert's masking.
	currPos.currentAttributes.Range(func(key string, val any) {
		if _, ok := attributes.Get(key); !ok {
			attributes.Set(key, Null)
		}
	})

	doc := trans.doc
	ownClientID := doc.ClientID
	minimizeAttributeChanges(currPos, attributes)
	var negatedData *objectData
	if scratch != nil {
		negatedData = &scratch.negated
	}
	negatedAttributes := insertAttributesWithScratch(trans, parent, currPos, attributes, negatedData)

	left, right, index := currPos.left, currPos.right, currPos.index
	if *parent.getSearchMarker() != nil { // yjs: if (parent._searchMarker) — markers ENABLED (non-nil slice)
		updateMarkerChanges(parent.getSearchMarker(), currPos.index, content.contentLength())
	}

	if itemStorage == nil {
		right = newItem(GenID(ownClientID, getState(doc.store, ownClientID)), left, getItemLastID(left), right, getItemID(right), parent, "", content)
	} else {
		right = initItemWithLength(itemStorage, GenID(ownClientID, getState(doc.store, ownClientID)),
			left, getItemLastID(left), right, getItemID(right), parent, "", content, content.contentLength())
	}
	_ = right.integrateStruct(trans, 0)
	currPos.right = right
	currPos.index = index
	_ = currPos.forward()
	insertNegatedAttributes(trans, parent, currPos, negatedAttributes)
}

func formatText(trans *Transaction, parent abstractType, currPos *itemTextListPosition, length Number, attributes Object) {
	doc := trans.doc
	ownClientID := doc.ClientID
	minimizeAttributeChanges(currPos, attributes)
	negatedAttributes := insertAttributes(trans, parent, currPos, attributes)

	// iterate until first non-format or null is found
	// delete all formats with attributes[format.key] != null
	// also check the attributes after the first non-format as we do not want to
	// insert redundant negated attributes there (yjs formatText iterationLoop):
	// keep walking past the formatted span while negatedAttributes still has
	// entries and the right item is deleted or a ContentFormat.
iterationLoop:
	for currPos.right != nil &&
		(length > 0 ||
			(negatedAttributes.Len() > 0 &&
				(currPos.right.isDeleted() || isSameType(currPos.right.content, &contentFormat{})))) {
		if !currPos.right.isDeleted() {
			switch cf := currPos.right.content.(type) {
			case *contentFormat:
				key, value := cf.key, cf.value
				attr, exist := attributes.Get(key)
				if exist {
					// Normalize a Go-nil attribute to the Null sentinel before comparing.
					// ContentFormat stores Null for "cleared", so a caller-supplied Go nil
					// (e.g. from a JSON-decoded delta) compared UNEQUAL to it — Go took the
					// else branch and inserted a spurious ContentFormat(key, null) where yjs
					// `equalAttrs(null, null)` is true and it deletes the negation instead.
					if isNullAttr(attr) {
						attr = Null
					}
					if equalAttrs(attr, value) {
						negatedAttributes.Delete(key)
					} else {
						if length == 0 {
							// no need to further extend negatedAttributes
							break iterationLoop
						}
						negatedAttributes.Set(key, value)
					}

					currPos.right.deleteItemStruct(trans)
				} else {
					// yjs: a format key not in `attributes` stays active — record it in
					// currentAttributes so later negation/equality decisions see it.
					currPos.currentAttributes.Set(key, value)
				}
			// yjs formatText: `default` — every non-format kind consumes the format span
			// by its length. A whitelist here made Format over-run past any other kind
			// and format too far (FR-014d).
			default:
				if length < currPos.right.length {
					getItemCleanStart(trans, GenID(currPos.right.id.Client, currPos.right.id.Clock+length))
				}
				length -= currPos.right.length
			}
		}

		_ = currPos.forward()
	}

	// Quill just assumes that the editor starts with a newline and that it always
	// ends with a newline. We only insert that newline when a new newline is
	// inserted - i.e when length is bigger than type.length
	if length > 0 {
		newlines := strings.Repeat("\n", length)

		currPos.right = newItem(GenID(ownClientID, getState(doc.store, ownClientID)), currPos.left, getItemLastID(currPos.left), currPos.right, getItemID(currPos.right), parent, "", newContentString(newlines))
		_ = currPos.right.integrateStruct(trans, 0)
		_ = currPos.forward()
	}

	insertNegatedAttributes(trans, parent, currPos, negatedAttributes)
}

// cleanupFormattingGap removes redundant formatting Items after content has been
// deleted. Faithful port of yjs 13.6.31 cleanupFormattingGap(transaction, start,
// curr, startAttributes, currAttributes): build endFormats (the LAST live
// ContentFormat per key in the gap, by REFERENCE), then delete each format that is
// not its key's canonical end-format or is already implied by startAttributes, while
// mutating currAttributes for the still-active region (before curr). `curr` is the
// position the deletion stopped at (for reachedCurr); the exclusive end is computed
// by walking from start to the next live countable item.
func cleanupFormattingGap(trans *Transaction, start *itemStruct, curr *itemStruct, startAttributes Object, currAttributes Object) Number {
	end := start
	endFormats := make(map[string]*contentFormat)
	for end != nil && (!end.countable() || end.isDeleted()) {
		if !end.isDeleted() {
			if cf, ok := end.content.(*contentFormat); ok {
				endFormats[cf.key] = cf
			}
		}
		end = end.right
	}

	cleanups := 0
	reachedCurr := false
	for start != end {
		if curr == start {
			reachedCurr = true
		}
		if !start.isDeleted() {
			if cf, ok := start.content.(*contentFormat); ok {
				key, value := cf.key, cf.value
				// yjs `startAttributes.get(key) ?? null` and strict `===` comparisons.
				startAttrValue := startAttributes.GetOrNull(key)
				if endFormats[key] != cf || attrStrictEqual(startAttrValue, value) {
					// Either this format is overwritten (not the canonical end-format for
					// its key) or it is redundant because startAttributes already had it.
					start.deleteItemStruct(trans)
					cleanups++
					if !reachedCurr && attrStrictEqual(currAttributes.GetOrNull(key), value) && !attrStrictEqual(startAttrValue, value) {
						if isNullAttr(startAttrValue) {
							currAttributes.Delete(key)
						} else {
							currAttributes.Set(key, startAttrValue)
						}
					}
				}
				if !reachedCurr && !start.isDeleted() {
					updateCurrentAttributes(currAttributes, cf)
				}
			}
		}

		start = start.right
	}

	return cleanups
}

func cleanupContextlessFormattingGap(trans *Transaction, item *itemStruct) {
	// iterate until item.right is null or content (yjs: deleted || !countable).
	// Countable() treats a nested ContentType as content (it is countable), so the
	// boundary walk stops at it instead of skipping past.
	for item != nil && item.right != nil && (item.right.isDeleted() || !item.right.countable()) {
		item = item.right
	}

	attrs := NewSet()

	// iterate back until a content item is found (yjs: deleted || !countable).
	for item != nil && (item.isDeleted() || !item.countable()) {
		if !item.isDeleted() && isSameType(item.content, &contentFormat{}) {
			key := item.content.(*contentFormat).key
			if attrs.Has(key) {
				item.deleteItemStruct(trans)
			} else {
				attrs.Add(key)
			}
		}
		item = item.left
	}
}

// This function is experimental and subject to change / be removed.
//
// Ideally, we don't need this function at all. Formatting attributes should be cleaned up
// automatically after each change. This function iterates twice over the complete YText type
// and removes unnecessary formatting attributes. This is also helpful for testing.
//
// This function won't be exported anymore as soon as there is confidence that the YText type works as intended.
func cleanupYTextFormatting(t *YText) Number {
	res := 0
	Transact(t.doc, func(trans *Transaction) {
		start := t.start
		end := t.start
		startAttributes := newObject()
		currentAttributes := newObject()
		for end != nil {
			if !end.isDeleted() {
				switch c := end.content.(type) {
				case *contentFormat:
					updateCurrentAttributes(currentAttributes, c)
				// yjs cleanupYTextFormatting: `default` — every non-format kind is a
				// content boundary, so formatting gaps around it are cleaned up rather
				// than skipped (FR-014d).
				default:
					res += cleanupFormattingGap(trans, start, end, startAttributes, currentAttributes)
					// Correctness 6: SHALLOW copy (object.assign), matching Yjs
					// map.copy — share nested values by reference so a
					// nested-object-valued format marker compares == against the active
					// attribute and a redundant marker is dropped. A deep copy gave the
					// nested value a fresh handle and the reference-strict equalAttrs
					// kept the redundant marker, diverging the item chain / ToDelta.
					startAttributes = currentAttributes.ShallowClone()
					start = end
				}
			}
			end = end.right
		}
	}, nil, true)
	return res
}

// CleanupYTextFormatting removes redundant formatting markers from t and
// returns the number of removed markers.
func CleanupYTextFormatting(t *YText) Number {
	return cleanupYTextFormatting(t)
}

func deleteText(trans *Transaction, currPos *itemTextListPosition, length Number) {
	startLength := length
	// Correctness 6: SHALLOW copy (Yjs map.copy), sharing nested values by
	// reference, so cleanupFormattingGap's reference-strict equalAttrs drops a
	// redundant nested-object format marker exactly as Yjs does (a deep copy broke
	// the reference match and kept the marker).
	startAttrs := currPos.currentAttributes.ShallowClone()
	start := currPos.right

	for length > 0 && currPos.right != nil {
		if !currPos.right.isDeleted() {
			switch currPos.right.content.(type) {
			case *contentEmbed, *contentString, *contentType:
				if length < currPos.right.length {
					getItemCleanStart(trans, GenID(currPos.right.id.Client, currPos.right.id.Clock+length))
				}
				length -= currPos.right.length
				currPos.right.deleteItemStruct(trans)
			}
		}
		_ = currPos.forward()
	}

	if start != nil {
		// yjs deleteText: cleanupFormattingGap(..., startAttrs, currPos.currentAttributes)
		// — the LIVE currentAttributes, so cleanupFormattingGap's currAttributes
		// mutations (restore/clear the active attribute before curr) PERSIST on currPos
		// for subsequent ApplyDelta ops that share it. Passing a throwaway clone dropped
		// those mutations, diverging multi-op ApplyDelta from yjs.
		cleanupFormattingGap(trans, start, currPos.right, startAttrs, currPos.currentAttributes)
	}

	// Both nil means the text is empty (findPosition on an empty Y.Text yields
	// {Left: nil, Right: y.Start == nil}), so nothing was deleted and there is no parent
	// to resolve. yjs throws here too — `(currPos.left || currPos.right).parent` on two
	// nulls — but a JS throw is catchable while a Go nil deref kills the goroutine, so a
	// stray `Delete(0, 1)` or an ApplyDelta carrying a delete op would take down the
	// process. Return the unchanged position instead.
	if currPos.left == nil && currPos.right == nil {
		return
	}

	var parent abstractType
	if currPos.left != nil {
		parent = currPos.left.parent.(abstractType)
	} else {
		parent = currPos.right.parent.(abstractType)
	}

	if *parent.getSearchMarker() != nil { // yjs: if (parent._searchMarker) — markers ENABLED (non-nil slice)
		updateMarkerChanges(parent.getSearchMarker(), currPos.index, -startLength+length)
	}
}

/*
 * The Quill Delta format represents changes on a text document with
 * formatting information. For mor information visit {@link https://quilljs.com/docs/delta/|Quill Delta}
 *
 * @example
 *   {
 *     ops: [
 *       { insert: 'Gandalf', attributes: { bold: true } },
 *       { insert: ' the ' },
 *       { insert: 'Grey', attributes: { color: '#cccccc' } }
 *     ]
 *   }
 *
 */

/*
 * Attributes that can be assigned to a selection of text.
 *
 * @example
 *   {
 *     bold: true,
 *     font-size: '40px'
 *   }
 *
 * @typedef {Object} TextAttributes
 */

// YTextEvent describes the changes on a YText type.
type YTextEvent struct {
	YEvent
	ChildListChanged bool        // Whether the children changed.
	KeysChanged      ChangedSubs // Set of all changed attributes.
}

func (y *YMapEvent) GetChanges() Object {
	if y.Changes.IsNil() || y.Changes.Len() == 0 {
		// Compute keys before GetDelta. GetDelta routes through the embedded
		// YEvent.GetChanges and therefore also populates y.Keys; keeping this as a
		// separate statement makes this override's own lazy-computation obligation
		// explicit instead of accidentally depending on argument evaluation order.
		keys := y.GetKeys()
		changes := MakeObject(
			"keys", keys,
			"delta", y.GetDelta(),
			"added", NewSet(),
			"deleted", NewSet(),
		)

		y.Changes = changes
	}

	return y.Changes
}

// GetDelta computes the changes in the delta format.
// A {@link https://quilljs.com/docs/delta/|Quill delta}) that represents the changes on the document.
func (y *YTextEvent) GetDelta() []EventOperator {
	if y.delta == nil {
		doc := y.target.getDoc()
		var delta []EventOperator

		Transact(doc, func(trans *Transaction) {
			currentAttributes := newObject() // saves all current attributes for insert
			oldAttributes := newObject()

			item := y.target.startItem()
			action := ""
			attributes := newObject() // counts added or removed new attributes for retain
			var insert interface{} = ""
			var insertText deltaTextAccumulator
			retain := 0
			deleteLen := 0

			addOp := func() {
				if action == "" {
					return
				}
				var op EventOperator
				emitted := false

				switch action {
				case "delete":
					// yjs: only emit when deleteLen > 0 (no spurious {delete:0})
					if deleteLen > 0 {
						op = NewDeleteDeltaOp(deleteLen)
						emitted = true
					}
					deleteLen = 0
				case "insert":
					// yjs: only emit when the insert is non-empty — a non-string
					// payload (embed or nested type) always counts as non-empty.
					if !insertText.Empty() {
						s := insertText.Take()
						if len(s) > 0 {
							op = NewTextDeltaOp(s, Object{})
							emitted = true
						}
					} else if s, isStr := insert.(string); !isStr || len(s) > 0 {
						if isStr {
							op = NewTextDeltaOp(s, Object{})
						} else {
							op = NewValueDeltaOp(insert, Object{})
						}
						emitted = true
					}
					if emitted && currentAttributes.Len() > 0 {
						attr := newObject()
						currentAttributes.Range(func(key string, value any) {
							if value != nil && !isNull(value) {
								attr.Set(key, value)
							}
						})
						op.Attributes = attr
					}
					insert = ""
				case "retain":
					// yjs: only emit when retain > 0 (no spurious {retain:0})
					if retain > 0 {
						op = NewRetainDeltaOp(retain, Object{})
						if attributes.Len() > 0 {
							attr := newObject()
							attributes.Range(func(key string, value any) {
								attr.Set(key, value)
							})
							op.Attributes = attr
						}
						emitted = true
					}
					retain = 0
				}

				if emitted {
					delta = append(delta, op)
				}
				action = ""
			}

			for item != nil {
				switch c := item.content.(type) {
				case *contentEmbed, *contentType:
					switch {
					case y.addsStruct(item):
						if !y.deletesStruct(item) {
							addOp()
							action = "insert"
							insert = item.content.contentValues()[0]
							addOp()
						}
					case y.deletesStruct(item):
						if action != "delete" {
							addOp()
							action = "delete"
						}
						deleteLen += 1
					case !item.isDeleted():
						if action != "retain" {
							addOp()
							action = "retain"
						}
						retain += 1
					}

				case *contentString:
					switch {
					case y.addsStruct(item):
						if !y.deletesStruct(item) {
							if action != "insert" {
								addOp()
								action = "insert"
							}
							insertText.Add(c.value)
						}
					case y.deletesStruct(item):
						if action != "delete" {
							addOp()
							action = "delete"
						}
						deleteLen += item.length
					case !item.isDeleted():
						if action != "retain" {
							addOp()
							action = "retain"
						}
						retain += item.length
					}
				case *contentFormat:
					key, value := c.key, c.value
					switch {
					case y.addsStruct(item):
						if !y.deletesStruct(item) {
							curVal := currentAttributes.GetOrNull(key)
							if !equalAttrs(curVal, value) {
								if action == "retain" {
									addOp()
								}

								if equalAttrs(value, oldAttributes.GetOrNull(key)) {
									attributes.Delete(key)
								} else {
									attributes.Set(key, value)
								}
							} else if !isNullAttr(value) {
								// yjs: `else if (value !== null) { item.delete(transaction) }`
								// — a redundant non-null format is dropped, a null one is kept.
								item.deleteItemStruct(trans)
							}
						}
					case y.deletesStruct(item):
						oldAttributes.Set(key, value)
						// yjs: const curVal = currentAttributes.get(key) ?? null.
						curVal := currentAttributes.GetOrNull(key)

						if !equalAttrs(curVal, value) {
							if action == "retain" {
								addOp()
							}
							attributes.Set(key, curVal)
						}
					case !item.isDeleted():
						oldAttributes.Set(key, value)
						// yjs uses `attr !== undefined` (present, even if null), so test
						// presence via Get's ok, not GetOr != nil (which collapses null).
						attr, exist := attributes.Get(key)
						if exist {
							// Same Go-nil -> Null normalization as formatText above.
							if isNullAttr(attr) {
								attr = Null
							}
							if !equalAttrs(attr, value) {
								if action == "retain" {
									addOp()
								}

								// yjs: `if (value === null) delete attributes[key] else attributes[key] = value`.
								if isNullAttr(value) {
									attributes.Delete(key)
								} else {
									attributes.Set(key, value)
								}
							} else if !isNullAttr(attr) {
								// yjs: `else if (attr !== null) { item.delete(transaction) }`.
								item.deleteItemStruct(trans)
							}
						}
					}

					if !item.isDeleted() {
						if action == "insert" {
							addOp()
						}

						updateCurrentAttributes(currentAttributes, item.content.(*contentFormat))
					}
				}

				item = item.right
			}

			addOp()
			for len(delta) > 0 {
				lastOp := delta[len(delta)-1]
				if lastOp.Kind == EventOperatorRetain && !lastOp.HasAttributes() {
					// retain delta's if they don't assign attributes
					delta = delta[:len(delta)-1]
				} else {
					break
				}
			}
		}, nil, true)

		y.delta = delta
	}

	return y.delta
}

func newYTextEvent(ytext *YText, trans *Transaction, subs ChangedSubs) *YTextEvent {
	yTextEvent := &YTextEvent{
		YEvent:           *newYEvent(ytext, trans),
		ChildListChanged: false,
		KeysChanged:      newChangedSubs(),
	}

	subs.Range(func(element string) {
		if element == "" {
			yTextEvent.ChildListChanged = true
		} else {
			yTextEvent.KeysChanged.Add(element)
		}
	})

	return yTextEvent
}

// YText represents text with formatting information.
//
// This type replaces y-richtext as this implementation is able to handle
// block formats (format information on a paragraph), embeds (complex elements
// like pictures and videos), and text formats (**bold**, *italic*).
type YText struct {
	abstractTypeBase
	pending     []func()
	tailAppend  *yTextTailAppendState
	stringCache atomic.Pointer[yTextStringCache]
	deltaCache  atomic.Pointer[yTextDeltaCache]
	// deltaCachePrimed defers cache construction until a second unchanged read.
	// Editors commonly mutate then read a delta once; that cold path must not pay
	// to preserve a canonical result it will never reuse.
	deltaCachePrimed atomic.Bool
}

// yTextTailAppendState owns spare capacity for the sole-item local append fast
// path. contentString.value always exposes the complete current value. Appending
// writes only after that string's length, so an internal reader that retained an
// older value observes an immutable shorter view; when the buffer grows, that old
// backing is kept alive by the retained string. The pointer/length check rejects
// reassignment of value before reusing the buffer.
type yTextTailAppendState struct {
	content *contentString
	buf     []byte
}

func (y *YText) appendToSoleString(content *contentString, str string) {
	state := y.tailAppend
	if state == nil {
		state = &yTextTailAppendState{}
		y.tailAppend = state
	}
	validBuffer := state.content == content && len(state.buf) == len(content.value) &&
		(len(state.buf) == 0 || unsafe.SliceData(state.buf) == unsafe.StringData(content.value))
	if !validBuffer {
		needed := len(content.value) + len(str)
		growth := len(content.value) / 4
		growth = minNumber(maxNumber(growth, 64), 1<<20)
		capacity := maxNumber(needed, len(content.value)+growth)
		state.content = content
		state.buf = make([]byte, len(content.value), capacity)
		copy(state.buf, content.value)
	}

	state.buf = append(state.buf, str...)
	content.value = unsafe.String(unsafe.SliceData(state.buf), len(state.buf))
	content.utf16Index = nil
}

type yTextStringCache struct {
	value     string
	fragments []yTextStringFragment
}

type yTextStringFragment struct {
	item    *itemStruct
	content *contentString
	value   string
}

type yTextDeltaFormatState struct {
	item    *itemStruct
	content *contentFormat
	key     string
	value   any
}

type yTextDeltaEmbedState struct {
	item    *itemStruct
	content *contentEmbed
	value   any
}

type yTextDeltaTypeState struct {
	item    *itemStruct
	content *contentType
	value   abstractType
}

// A published entry is IMMUTABLE. deltaForInternalRead hands its ops directly to package-internal
// readers instead of cloning, which is only safe while invalidation replaces the atomic pointer
// rather than rebuilding an entry in place — a reader holding the old entry must keep seeing the
// document as it was when it borrowed. Rebuilding in place to save an allocation would corrupt
// every in-flight internal read.
type yTextDeltaCache struct {
	ops                 []EventOperator
	smallAttributeCount int
	strings             []yTextStringFragment
	formats             []yTextDeltaFormatState
	embeds              []yTextDeltaEmbedState
	types               []yTextDeltaTypeState
}

func cacheValueEqual(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case int:
		bv, ok := b.(int)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	case NullType:
		_, ok := b.(NullType)
		return ok
	case UndefinedType:
		_, ok := b.(UndefinedType)
		return ok
	default:
		return attrStrictEqual(a, b)
	}
}

func (cache *yTextDeltaCache) valid() bool {
	// Library mutations clear deltaCache before they can change the item chain.
	// These checks defend the mutable exported content fields as well: retaining
	// the original content pointers makes pointer reuse impossible while cached.
	if !cachedYTextStringFragmentsUnchanged(cache.strings) {
		return false
	}
	for _, state := range cache.formats {
		content, ok := state.item.content.(*contentFormat)
		if !ok || content != state.content || content.key != state.key || !cacheValueEqual(content.value, state.value) {
			return false
		}
	}
	for _, state := range cache.embeds {
		content, ok := state.item.content.(*contentEmbed)
		if !ok || content != state.content || !cacheValueEqual(content.embed, state.value) {
			return false
		}
	}
	for _, state := range cache.types {
		content, ok := state.item.content.(*contentType)
		if !ok || content != state.content || content.value != state.value {
			return false
		}
	}
	return true
}

func countSmallYTextDeltaAttributes(ops []EventOperator) int {
	smallAttributes := 0
	for i := range ops {
		if ops[i].HasAttributes() && ops[i].Attributes.d.large == nil {
			smallAttributes++
		}
	}
	return smallAttributes
}

func cloneYTextDeltaKnownSmallAttributes(ops []EventOperator, smallAttributes int) []EventOperator {
	if len(ops) == 0 {
		return []EventOperator{}
	}
	cloned := make([]EventOperator, len(ops))
	copy(cloned, ops)
	attributeData := make([]objectData, smallAttributes)
	attributeIndex := 0
	for i := range cloned {
		if !cloned[i].HasAttributes() {
			continue
		}
		if cloned[i].Attributes.d.large == nil {
			attributeData[attributeIndex] = *cloned[i].Attributes.d
			cloned[i].Attributes = Object{d: &attributeData[attributeIndex]}
			attributeIndex++
		} else {
			cloned[i].Attributes = cloned[i].Attributes.ShallowClone()
		}
	}
	return cloned
}

func cachedYTextStringFragmentsUnchanged(fragments []yTextStringFragment) bool {
	for _, fragment := range fragments {
		content, ok := fragment.item.content.(*contentString)
		if !ok || content != fragment.content || content.value != fragment.value {
			return false
		}
	}
	return true
}

func invalidateTextStringCache(t abstractType) {
	switch text := t.(type) {
	case *YText:
		text.tailAppend = nil
		text.stringCache.Store(nil)
		text.deltaCache.Store(nil)
		text.deltaCachePrimed.Store(false)
	case *YXmlText:
		text.tailAppend = nil
		text.stringCache.Store(nil)
		text.deltaCache.Store(nil)
		text.deltaCachePrimed.Store(false)
	}
}

func (y *YText) Length() Number {
	return y.length
}

func (y *YText) integrate(doc *Doc, item *itemStruct) {
	y.abstractTypeBase.integrate(doc, item)
	for _, f := range y.pending {
		f()
	}

	y.pending = nil
}

func (y *YText) copyType() abstractType {
	return NewYText("")
}

func (y *YText) cloneType() abstractType {
	text := NewYText("")
	text.ApplyDelta(y.ToDelta(nil, nil, nil), true)
	return text
}

// Clone returns an independent YText with the same delta.
func (y *YText) Clone() *YText { return y.cloneType().(*YText) }

// isObservedText reports whether item's parent is the YText currently being observed.
// YXmlText embeds YText, so a YXmlText's items reference the *YXmlText while
// CallObserver's receiver is the embedded *YText (&xt.YText) — a plain `parent == y`
// misses them. Handle both so the remote cleanup runs for YXmlText too.
func isObservedText(parent interface{}, y *YText) bool {
	switch p := parent.(type) {
	case *YText:
		return p == y
	case *YXmlText:
		return &p.YText == y
	}
	return false
}

// Creates YTextEvent and calls observers.
func (y *YText) callObserver(trans *Transaction, parentSubs ChangedSubs) {
	y.abstractTypeBase.callObserver(trans, parentSubs)
	doc := trans.doc

	if hasTypeObservers(y) {
		callTypeObservers(y, trans, newYTextEvent(y, trans, parentSubs))
	}

	// If a remote change happened, clean up potential formatting duplicates for THIS
	// text. yjs cleanupYTextAfterTransaction makes a PER-PARENT needFullCleanup
	// decision: a text gets a full cleanup only if IT had a ContentFormat inserted or
	// deleted this transaction; otherwise a contextless cleanup of its own deleted
	// items suffices. The earlier code used a GLOBAL formatting flag (and ran once per
	// client), so a format change in ANY text forced a full cleanup of EVERY observed
	// text — over-deleting redundant markers other texts should keep, diverging from
	// yjs on multi-text remote transactions. (Go runs this inline per observed text
	// rather than once-after-transaction; the per-text decision makes the result
	// identical to yjs.)
	if !trans.local {
		needFull := false
		// A ContentFormat INSERTED into THIS text this transaction?
		for client, afterClock := range trans.afterState {
			clock := trans.beforeState[client]
			if afterClock == clock {
				continue
			}
			structs, ok := doc.store.clientStructs(client)
			if !ok {
				continue
			}
			iterateStructs(trans, structs, clock, afterClock, func(s abstractStruct) {
				item, ok := s.(*itemStruct)
				if ok && !item.isDeleted() && isSameType(item.content, &contentFormat{}) && isObservedText(item.parent, y) {
					needFull = true
				}
			})
		}
		// A ContentFormat DELETED from THIS text? (yjs adds the parent to
		// needFullCleanup in the deleted-structs pass.)
		if !needFull {
			iterateDeletedStructs(trans, trans.deleteSet, func(s abstractStruct) {
				if isSameType(s, &gcStruct{}) {
					return
				}
				item, ok := s.(*itemStruct)
				if ok && isSameType(item.content, &contentFormat{}) && isObservedText(item.parent, y) {
					needFull = true
				}
			})
		}

		Transact(doc, func(t *Transaction) {
			if needFull {
				// A formatting item touched this text: clean the whole type.
				cleanupYTextFormatting(y)
			} else {
				// No formatting touched this text: contextless cleanup of its deleted
				// items (no need to compute currentAttributes for the position).
				iterateDeletedStructs(t, trans.deleteSet, func(s abstractStruct) {
					if isSameType(s, &gcStruct{}) {
						return
					}
					item, ok := s.(*itemStruct)
					if ok && isObservedText(item.parent, y) {
						cleanupContextlessFormattingGap(t, item)
					}
				})
			}
		}, nil, true)
	}
}

// ToString returns the unformatted string representation of this YText type.
func (y *YText) ToString() string {
	if n := y.start; n != nil && n.right == nil && !n.isDeleted() && n.countable() {
		if content, ok := n.content.(*contentString); ok {
			return content.value
		}
	}
	if cached := y.stringCache.Load(); cached != nil && cachedYTextStringFragmentsUnchanged(cached.fragments) {
		return cached.value
	}
	text := make([]byte, 0, y.length)
	fragments := make([]yTextStringFragment, 0, 32)
	// YText.Length is measured in UTF-16 code units. It is exact for ASCII and a
	// useful lower bound for UTF-8 text, avoiding the formatted-text path's former
	// allocation and full-prefix copy for every ContentString item.
	n := y.start
	for n != nil {
		if !n.isDeleted() && n.countable() {
			if content, ok := n.content.(*contentString); ok {
				text = append(text, content.value...)
				fragments = append(fragments, yTextStringFragment{item: n, content: content, value: content.value})
			}
		}
		n = n.right
	}
	if len(text) == 0 {
		if y.doc != nil && y.doc.readCacheEnabled {
			y.stringCache.Store(&yTextStringCache{fragments: fragments})
		}
		return ""
	}
	// text is a fresh, function-local allocation that is never mutated after
	// return. Reinterpreting it avoids a second full-buffer copy while preserving
	// the immutable string contract exposed to callers.
	value := unsafe.String(unsafe.SliceData(text), len(text))
	if y.doc != nil && y.doc.readCacheEnabled {
		y.stringCache.Store(&yTextStringCache{value: value, fragments: fragments})
	}
	return value
}

// Returns the unformatted string representation of this YText type.
func (y *YText) toJSONValue() interface{} { return y.ToJSON() }

func (y *YText) ToJSON() interface{} {
	return y.ToString()
}

// ApplyDelta applies a delta on this shared YText type.
// sanitize = true
func (y *YText) ApplyDelta(delta []EventOperator, sanitize bool) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			currPos := newItemTextListPosition(nil, y.start, 0, newObject())
			var scratch *insertTextObjectScratch
			if len(delta) > localFormatItemArenaThreshold {
				scratch = &insertTextObjectScratch{}
			}
			reserveStructs, reserveFormats := 0, 0
			if y.start == nil && len(delta) > localFormatItemArenaThreshold {
				reserveStructs, reserveFormats = freshApplyDeltaCounts(delta, sanitize)
				y.doc.reserveFormatItemStorage(reserveFormats)
				if reserveFormats > 0 {
					reserveStrings, reserveOrigins := freshApplyDeltaTextStorageCounts(delta, sanitize)
					if reserveStrings > 0 {
						y.doc.reserveStringItemStorage(reserveStrings)
					}
					if reserveOrigins > 0 {
						y.doc.reserveItemOriginStorage(reserveOrigins)
					}
				}
			}
			reservedStructs := reserveStructs == 0
			clientStructBase := y.doc.store.clientLength(y.doc.ClientID)
			for i := 0; i < len(delta); i++ {
				op := delta[i]
				switch op.Kind {
				case EventOperatorInsertText:
					// Quill assumes that the content starts with an empty paragraph.
					// Yjs/Y.Text assumes that it starts empty. We always hide that
					// there is a newline at the end of the content.
					// If we omit this step, clients will see a different number of
					// paragraphs, but nothing bad will happen.
					// yjs: !sanitize && typeof op.insert==='string' && i===last &&
					// currPos.right===null && op.insert.slice(-1)==='\n' → drop trailing \n.
					// (Inserts are plain strings, and the boundary is currPos.Right==nil.)
					strInsert := op.InsertText
					if !sanitize && (i == len(delta)-1) && currPos.right == nil && len(strInsert) > 0 && strInsert[len(strInsert)-1] == '\n' {
						strInsert = strInsert[:len(strInsert)-1]
					}
					if len(strInsert) > 0 {
						insertTextString(trans, y, currPos, strInsert, op.Attributes, scratch)
					}
				case EventOperatorInsertValue:
					insertTextWithScratch(trans, y, currPos, op.Insert, op.Attributes, scratch)
				case EventOperatorRetain:
					formatText(trans, y, currPos, op.Length, op.Attributes)
				case EventOperatorDelete:
					deleteText(trans, currPos, op.Length)
				}
				if !reservedStructs {
					reservedStructs = reserveClientStructCapacity(y.doc.store, y.doc.ClientID,
						clientStructBase+reserveStructs)
				}
			}
		})
	} else {
		y.pending = append(y.pending, func() {
			y.ApplyDelta(delta, true)
		})
	}
}

func freshApplyDeltaTextStorageCounts(delta []EventOperator, sanitize bool) (strings int, origins int) {
	for i, op := range delta {
		if op.Kind != EventOperatorInsertText {
			return 0, 0
		}
		str := op.InsertText
		if !sanitize && i == len(delta)-1 && len(str) > 0 && str[len(str)-1] == '\n' {
			str = str[:len(str)-1]
		}
		if len(str) == 0 {
			return 0, 0
		}
		if !utf8.ValidString(str) {
			// The insertion path normalizes malformed Go byte strings to the
			// WHATWG replacement sequence. Fall back to the bounded arenas rather
			// than reserving from pre-normalization UTF-16 widths.
			return 0, 0
		}
		formatted := false
		op.Attributes.Range(func(_ string, value any) {
			if !isNullAttr(value) {
				formatted = true
			}
		})
		if !formatted {
			// Adjacent ContentStrings merge at cleanup. Reserving one exact block for a
			// run containing an unformatted string could retain all merged-away slots
			// through one survivor, so keep mixed runs on the bounded arenas.
			return 0, 0
		}
		strings++
		// Every string has an opening and closing format marker, so the physical tail
		// before each opening is one clock wide. Only a multi-clock string makes the
		// first closing marker allocate a standalone origin ID.
		if stringUTF16LengthExceedsOne(str) {
			origins++
		}
	}
	return strings, origins
}

func stringUTF16LengthExceedsOne(str string) bool {
	units := 0
	for _, r := range str {
		units++
		if r >= 0x10000 {
			units++
		}
		if units > 1 {
			return true
		}
	}
	return false
}

func freshApplyDeltaCounts(delta []EventOperator, sanitize bool) (structs int, formats int) {
	for i, op := range delta {
		if !op.IsInsert() {
			return 0, 0
		}
		if op.Kind == EventOperatorInsertText {
			str := op.InsertText
			if !sanitize && i == len(delta)-1 && len(str) > 0 && str[len(str)-1] == '\n' {
				str = str[:len(str)-1]
			}
			if len(str) == 0 {
				continue
			}
		}
		structs++
		op.Attributes.Range(func(_ string, value any) {
			if !isNullAttr(value) {
				structs += 2
				formats += 2
			}
		})
	}
	return structs, formats
}

// ToDelta returns the delta representation of this YText type.
func (y *YText) ToDelta(snapshot *Snapshot, prevSnapshot *Snapshot, computeYChange func(string, *ID) Object) []EventOperator {
	plainRead := snapshot == nil && prevSnapshot == nil && computeYChange == nil
	if plainRead {
		if cached := y.deltaCache.Load(); cached != nil && cached.valid() {
			return cloneYTextDeltaKnownSmallAttributes(cached.ops, cached.smallAttributeCount)
		}
	}
	cacheable := plainRead && y.doc != nil && y.doc.readCacheEnabled
	buildCache := cacheable && y.deltaCachePrimed.Swap(true)
	opCapacity := 1
	if y.searchMarker == nil {
		opCapacity = minNumber(maxNumber(y.length/16, 16), 128)
	}
	ops := make([]EventOperator, 0, opCapacity)
	currentAttributes := newObject()
	doc := y.doc
	stringStateCapacity := 0
	formatStateCapacity := 0
	embedStateCapacity := 0
	typeStateCapacity := 0
	if buildCache {
		// Exact capacities keep the cold path honest: formatting creates hundreds
		// of items, and geometric growth of four pointer-rich validation slices
		// otherwise costs more than constructing the delta itself.
		for item := y.start; item != nil; item = item.right {
			if item.isDeleted() {
				continue
			}
			switch item.content.(type) {
			case *contentString:
				stringStateCapacity++
			case *contentFormat:
				formatStateCapacity++
			case *contentEmbed:
				embedStateCapacity++
			case *contentType:
				typeStateCapacity++
			}
		}
	}
	// Attribute-bearing delta ops need independent top-level Objects, but formatted
	// text overwhelmingly uses one or two keys. Allocate those small backing stores
	// in blocks instead of one heap object per op; nested values remain shared, just
	// like Object.ShallowClone.
	const attributeBlockSize = 64
	var attributeBlock *[attributeBlockSize]objectData
	attributeOffset := attributeBlockSize
	snapshotAttributes := func() Object {
		if currentAttributes.d == nil || currentAttributes.d.large != nil {
			return currentAttributes.ShallowClone()
		}
		if attributeOffset == attributeBlockSize {
			attributeBlock = new([attributeBlockSize]objectData)
			attributeOffset = 0
		}
		d := &attributeBlock[attributeOffset]
		attributeOffset++
		*d = *currentAttributes.d
		return Object{d: d}
	}
	var text deltaTextAccumulator
	text.capacityHint = minNumber(y.length, 4096)
	deltaStrings := make([]yTextStringFragment, 0, stringStateCapacity)
	formatStates := make([]yTextDeltaFormatState, 0, formatStateCapacity)
	embedStates := make([]yTextDeltaEmbedState, 0, embedStateCapacity)
	typeStates := make([]yTextDeltaTypeState, 0, typeStateCapacity)
	n := y.start
	ychangePresent := false
	packStr := func() {
		if !text.Empty() {
			// pack str with attributes to ops
			op := NewTextDeltaOp(text.Take(), Object{})

			if currentAttributes.Len() > 0 {
				op.Attributes = snapshotAttributes()
				// A non-nil Object distinguishes present attributes from an omitted field.
			}

			ops = append(ops, op)
		}
	}

	computeDelta := func() {
		for n != nil {
			visibleNow := !n.isDeleted()
			if snapshot != nil {
				visibleNow = isVisible(n, snapshot)
			}
			visibleBefore := prevSnapshot != nil && isVisible(n, prevSnapshot)
			if visibleNow || visibleBefore {
				switch content := n.content.(type) {
				case *contentString:
					if buildCache {
						deltaStrings = append(deltaStrings, yTextStringFragment{item: n, content: content, value: content.value})
					}
					var currentChange Object
					if ychangePresent {
						currentChange, _ = currentAttributes.GetOr("ychange").(Object)
					}
					switch {
					case snapshot != nil && !visibleNow:
						if currentChange.IsNil() || currentChange.GetOr("user") != n.id.Client || currentChange.GetOr("type") != "removed" {
							packStr()
							if computeYChange != nil {
								currentAttributes.Set("ychange", computeYChange("removed", &n.id))
							} else {
								currentAttributes.Set("ychange", MakeObject(
									"type", "removed",
								))
							}
							ychangePresent = true
						}
					case prevSnapshot != nil && !visibleBefore:
						if currentChange.IsNil() || currentChange.GetOr("user") != n.id.Client || currentChange.GetOr("type") != "added" {
							packStr()
							if computeYChange != nil {
								currentAttributes.Set("ychange", computeYChange("added", &n.id))
							} else {
								currentAttributes.Set("ychange", MakeObject(
									"type", "added",
								))
							}
							ychangePresent = true
						}
					case ychangePresent:
						packStr()
						currentAttributes.Delete("ychange")
						ychangePresent = false
					}

					text.Add(content.value)
				case *contentEmbed:
					if buildCache {
						embedStates = append(embedStates, yTextDeltaEmbedState{item: n, content: content, value: content.embed})
					}
					packStr()
					op := NewValueDeltaOp(content.embed, Object{})

					if currentAttributes.Len() > 0 {
						op.Attributes = snapshotAttributes()
					}
					ops = append(ops, op)
				case *contentType:
					if buildCache {
						typeStates = append(typeStates, yTextDeltaTypeState{item: n, content: content, value: content.value})
					}
					packStr()
					op := NewValueDeltaOp(content.value, Object{})
					if currentAttributes.Len() > 0 {
						op.Attributes = snapshotAttributes()
					}
					ops = append(ops, op)
				case *contentFormat:
					if buildCache {
						formatStates = append(formatStates, yTextDeltaFormatState{
							item: n, content: content, key: content.key, value: content.value,
						})
					}
					if visibleNow {
						packStr()
						updateCurrentAttributes(currentAttributes, content)
						if content.key == "ychange" {
							value, exists := currentAttributes.Get("ychange")
							_, undefined := value.(UndefinedType)
							ychangePresent = exists && !undefined
						}
					}
				}
			}
			n = n.right
		}
		packStr()
	}

	if snapshot != nil || prevSnapshot != nil {
		// snapshots are merged again after the transaction, so we keep the
		// transaction alive until we are done (yjs toDelta).
		Transact(doc, func(trans *Transaction) {
			if snapshot != nil {
				splitSnapshotAffectedStructs(trans, snapshot)
			}
			if prevSnapshot != nil {
				splitSnapshotAffectedStructs(trans, prevSnapshot)
			}
			computeDelta()
		}, "cleanup", true) // yjs toDelta uses the 'cleanup' transaction origin
	} else {
		// yjs: with no snapshot, computeDelta runs directly with NO transaction — a
		// plain toDelta is a pure read and must not open a transaction (which would
		// fire before/afterTransaction events and cleanup side effects).
		computeDelta()
	}

	if buildCache {
		smallAttributeCount := countSmallYTextDeltaAttributes(ops)
		y.deltaCache.Store(&yTextDeltaCache{
			ops: ops, smallAttributeCount: smallAttributeCount,
			strings: deltaStrings, formats: formatStates,
			embeds: embedStates, types: typeStates,
		})
		return cloneYTextDeltaKnownSmallAttributes(ops, smallAttributeCount)
	}
	return ops
}

// deltaForInternalRead returns a plain delta for synchronous package-internal
// consumption. A validated cache entry is immutable and is never exposed here,
// so internal renderers can borrow it without paying ToDelta's caller-isolation
// clone. Public ToDelta keeps returning independent operators and Objects.
func (y *YText) deltaForInternalRead() []EventOperator {
	if cached := y.deltaCache.Load(); cached != nil && cached.valid() {
		return cached.ops
	}
	ops := y.ToDelta(nil, nil, nil)
	// The second unchanged read may have published the cache. Prefer its canonical
	// ops; the owned clone returned above remains local and is simply discarded.
	if cached := y.deltaCache.Load(); cached != nil && cached.valid() {
		return cached.ops
	}
	return ops
}

// tryAppendSoleString performs the result cleanup would otherwise produce after
// an unattributed local tail append. The strict guards are also what make direct
// length mutation and leaving search markers untouched safe.
func (y *YText) tryAppendSoleString(trans *Transaction, index Number, text string, attributes Object) bool {
	doc := y.doc
	if !trans.compactState || trans.changedTypes != nil || !attributes.IsNil() || index != y.length ||
		y.searchMarker == nil || y.start == nil || y.start.right != nil ||
		y.start.isDeleted() || !y.start.countable() || y.start.length != y.length || y.start.parentSub != "" ||
		y.start.redone != nil || y.start.rightOrigin != nil || y.start.id.Client != doc.ClientID ||
		y.start.parent != y || (y.item != nil && y.item.isDeleted()) {
		return false
	}
	clientStructs, exists := doc.store.clientStructs(doc.ClientID)
	if !exists {
		return false
	}
	lastStruct := clientStructs.lastValue()
	if lastStruct.getID().Clock+lastStruct.structLength() != y.start.id.Clock+y.start.length || hasTypeObservers(y) {
		return false
	}
	content, ok := y.start.content.(*contentString)
	if !ok {
		return false
	}
	addedLength := Number(1)
	if len(text) != 1 || text[0] >= utf8.RuneSelf {
		if isASCIIText(text) {
			addedLength = len(text)
		} else {
			text, addedLength = normalizeNonASCIITextUTF8WithLength(text)
		}
	}
	y.appendToSoleString(content, text)
	y.start.length += addedLength
	clientStructs.refreshValue(y.start)
	y.length += addedLength
	y.stringCache.Store(nil)
	y.deltaCache.Store(nil)
	y.deltaCachePrimed.Store(false)
	return true
}

// Insert text at a given index.
func (y *YText) Insert(index Number, text string, attributes Object) {
	if len(text) == 0 {
		return
	}

	doc := y.doc
	if doc != nil {
		trans, initialCall := beginTransact(doc, nil, true, true)
		if !y.tryAppendSoleString(trans, index, text, attributes) {
			var scratch *insertTextObjectScratch
			currentAttributes := Object{}
			// Plain unattributed text uses the mutable marker path and never accumulates
			// CurrentAttributes. Keep that dominant path allocation-identical; formatted or
			// explicitly attributed inserts need the reusable ordered-object storage.
			if y.searchMarker == nil || !attributes.IsNil() {
				scratch = acquireTextMutationScratch(doc)
				currentAttributes = Object{d: &scratch.current}
			}
			// yjs insert: findPosition(..., !attributes) — walk from start (track
			// CurrentAttributes) when attributes are given so insertText negates
			// correctly; use the search marker only for an unattributed insert.
			pos := findPositionValue(trans, y, index, attributes.IsNil(), currentAttributes)
			if pos.currentAttributes.Len() == 0 {
				// Preserve the zero Object's absent-vs-present distinction for an unformatted cursor.
				pos.currentAttributes = Object{}
			}
			if attributes.IsNil() {
				// insertTextContentWithItem clones into scratch.attributes before its in-place
				// negation pass, so a second caller-isolation clone here is redundant.
				attributes = pos.currentAttributes
			}
			insertTextString(trans, y, &pos, text, attributes, scratch)
		}
		finishTransact(doc, initialCall)
	} else {
		y.pending = append(y.pending, func() {
			y.Insert(index, text, attributes)
		})
	}
}

// InsertEmbed inserts an embed at an index.
func (y *YText) InsertEmbed(index Number, embed Object, attributes Object) {
	doc := y.doc
	if doc != nil { // yjs: `if (y !== null)` where y = this.doc — guard on the DOC, not the receiver
		transactMutation(doc, func(trans *Transaction) {
			// yjs insertEmbed: findPosition(..., !attributes).
			pos := findPosition(trans, y, index, attributes.IsNil())
			insertText(trans, y, pos, embed, attributes)
		})
	} else {
		y.pending = append(y.pending, func() {
			y.InsertEmbed(index, embed, attributes)
		})
	}
}

// Delete deletes text starting from an index.
func (y *YText) Delete(index Number, length Number) {
	if length == 0 {
		return
	}

	doc := y.doc
	if doc != nil { // yjs: `if (y !== null)` where y = this.doc — guard on the DOC, not the receiver
		transactMutation(doc, func(trans *Transaction) {
			// yjs delete: findPosition(..., true) — the search marker is fine, delete
			// needs no CurrentAttributes.
			deleteText(trans, findPosition(trans, y, index, true), length)
		})
	} else {
		y.pending = append(y.pending, func() {
			y.Delete(index, length)
		})
	}
}

// Format assigns properties to a range of text.
func (y *YText) Format(index Number, length Number, attributes Object) {
	if length == 0 {
		return
	}

	doc := y.doc
	if doc != nil { // yjs: `if (y !== null)` where y = this.doc — guard on the DOC, not the receiver
		transactMutation(doc, func(trans *Transaction) {
			// yjs format: findPosition(..., false) — NEVER use the search marker;
			// format must accumulate CurrentAttributes from the start so the negated
			// attributes restore the run's prior formatting (not Null).
			scratch := acquireTextMutationScratch(doc)
			pos := findPositionWithCurrentAttributes(trans, y, index, false, Object{d: &scratch.current})
			if pos.right == nil {
				return
			}
			formatText(trans, y, pos, length, attributes)
		})
	} else {
		y.pending = append(y.pending, func() {
			y.Format(index, length, attributes)
		})
	}
}

// RemoveAttribute removes an attribute.
func (y *YText) RemoveAttribute(attributeName string) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			typeMapDelete(trans, y, attributeName)
		})
	} else {
		y.pending = append(y.pending, func() {
			y.RemoveAttribute(attributeName)
		})
	}
}

// SetAttribute sets or updates an attribute.
func (y *YText) SetAttribute(attributeName string, attributeValue interface{}) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			_ = typeMapSet(trans, y, attributeName, attributeValue)
		})
	} else {
		y.pending = append(y.pending, func() {
			y.SetAttribute(attributeName, attributeValue)
		})
	}
}

// GetAttribute returns the attribute value that belongs to the attribute name.
func (y *YText) GetAttribute(attributeName string) interface{} {
	return typeMapGet(y, attributeName)
}

// GetAttributes returns all attribute name/value pairs in a JSON Object.
func (y *YText) GetAttributes(snapshot *Snapshot) Object {
	return typeMapGetAll(y)
}

func (y *YText) writeType(encoder updateEncoder) {
	encoder.writeTypeRef(yTextRefID)
}

func NewYText(text string) *YText {
	yText := &YText{}
	// yjs YText constructor: `this._searchMarker = []` (markers ENABLED). They are
	// disabled (set nil) once formatting is integrated (ContentFormat.Integrate), so
	// formatted text resolves positions by walking from start with accurate
	// CurrentAttributes — required for correct negation/inherit.
	yText.searchMarker = []*arraySearchMarker{}

	if text != "" {
		yText.pending = append(yText.pending, func() {
			// Zero Object (IsNil) => no attributes; Insert fills them from context.
			yText.Insert(0, text, Object{})
		})
	}

	return yText
}

func NewDefaultYText() *YText {
	return &YText{}
}

func newYTextType() SharedType {
	return NewYText("")
}

func newItemTextListPosition(left, right *itemStruct, index Number, currentAttributes Object) *itemTextListPosition {
	return &itemTextListPosition{
		left:              left,
		right:             right,
		index:             index,
		currentAttributes: currentAttributes,
	}
}
