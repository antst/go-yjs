package crdt

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
)

// ---------------------------------------------------------------- from abstract_content.go
var contentRefs = []func(updateDecoder) (itemContent, error){
	func(decoder updateDecoder) (itemContent, error) {
		return nil, errors.New("unexpected case")
	}, // GC is not ItemContent
	readContentDeleted, // 1
	readContentJSON,    // 2
	readContentBinary,  // 3
	readContentString,  // 4
	readContentEmbed,   // 5
	readContentFormat,  // 6
	readContentType,    // 7
	readContentAny,     // 8
	readContentDoc,     // 9
	func(decoder updateDecoder) (itemContent, error) { // 10 - Skip is not ItemContent
		return nil, errors.New("unexpected case")
	},
}

type itemContent interface {
	contentLength() Number
	contentValues() ArrayAny
	isCountable() bool
	copyContent() itemContent
	spliceContent(offset Number) itemContent
	mergeContentWith(right itemContent) bool
	integrateContent(trans *Transaction, item *itemStruct)
	deleteContent(trans *Transaction)
	gcContent(store *structStore)
	writeContent(encoder updateEncoder, offset Number) error
	contentRef() uint8
}

// ---------------------------------------------------------------- from abstract_struct.go
type abstractStruct interface {
	getID() *ID
	structLength() Number
	setStructLength(length Number)
	isDeleted() bool
	mergeStructWith(right abstractStruct) bool
	// writeStruct serializes the struct to the encoder. It returns an error when the
	// struct's content cannot be serialized (e.g. an any-encode failure), so a
	// content-encode failure surfaces as an error up the encode chain instead of
	// silently truncating the update.
	writeStruct(encoder updateEncoder, offset Number) error
	integrateStruct(trans *Transaction, offset Number) error
	missingClient(trans *Transaction, store *structStore) (Number, error)
}

type abstractStructBase struct {
	id     ID
	length Number
}

func (s *abstractStructBase) getID() *ID {
	return &s.id
}

func (s *abstractStructBase) structLength() Number {
	return s.length
}

func (s *abstractStructBase) setStructLength(length Number) {
	s.length = length
}

func (s *abstractStructBase) isDeleted() bool {
	return false
}

// Merge this struct with the item to the right.
// This method is already assuming that `this.id.clock + this.length === this.id.clock`.
// Also this method does *not* remove right from StructStore!
// @param {AbstractStruct} right
// @return {boolean} whether this merged with right
func (s *abstractStructBase) mergeStructWith(right abstractStruct) bool {
	return false
}

func (s *abstractStructBase) writeStruct(encoder updateEncoder, offset Number) error {
	return nil
}

func (s *abstractStructBase) integrateStruct(trans *Transaction, offset Number) error {
	return nil
}

func (s *abstractStructBase) missingClient(trans *Transaction, store *structStore) (Number, error) {
	return 0, nil
}

// ---------------------------------------------------------------- from abstract_type.go
// SharedType is the sealed public handle for a heterogeneous Yjs shared type.
// The unexported marker deliberately prevents independent external
// implementations: callers may pass and compare the shared types this package
// creates, but cannot use the interface to reopen the private object graph.
type SharedType interface {
	isSharedType()
}

// TypeConstructor constructs a shared type for Doc.Get.
type TypeConstructor func() SharedType

type abstractType interface {
	GetLength() Number
	getItem() *itemStruct
	getMap() map[string]*itemStruct
	startItem() *itemStruct
	setStartItem(item *itemStruct)
	getDoc() *Doc
	updateLength(n Number)
	setSearchMarker(mark []*arraySearchMarker)
	parentType() abstractType
	integrate(doc *Doc, item *itemStruct)
	copyType() abstractType
	cloneType() abstractType
	writeType(encoder updateEncoder)
	firstItem() *itemStruct
	callObserver(trans *Transaction, parentSubs ChangedSubs)
	Observe(f func(interface{}, interface{}))
	ObserveDeep(f func(interface{}, interface{}))
	Unobserve(f func(interface{}, interface{}))
	UnobserveDeep(f func(interface{}, interface{}))
	ToJson() interface{}
	getDeepEventHandler() *eventHandler
	getEventHandler() *eventHandler
	setMap(map[string]*itemStruct)
	setLength(number Number)
	getSearchMarker() *[]*arraySearchMarker
}

const maxSearchMarker = 80

// Large unformatted sequences need more than the reference's fixed 80 markers.
// With N fragmented items and C markers, each lookup pays O(C) to scan/update
// markers plus O(N/C) to walk from the nearest one. C proportional to sqrt(N)
// minimizes that representation's cost; either a fixed C or C proportional to N
// becomes quadratic over a growing random-insert workload. Keep the reference's
// 80-marker footprint below 16k items, then double C whenever N quadruples.
func searchMarkerLimit(itemCount Number) int {
	limit := maxSearchMarker
	for threshold := Number(16_000); itemCount >= threshold; {
		limit *= 2
		// The next threshold is outside this list, and multiplying by four
		// could overflow for a synthetic near-MaxInt item count.
		if threshold > itemCount/4 {
			break
		}
		threshold *= 4
	}
	return limit
}

// globalSearchMarkerTimestamp orders marker eviction: marker insertion replaces the marker with
// the lowest timestamp once a type holds its current searchMarkerLimit.
//
// ATOMIC because it is package-global and every list mutation touches it, so two goroutines editing
// two ENTIRELY SEPARATE documents raced on it -- breaking document independence, which is a far
// more basic guarantee than concurrent access to one document. The race detector reports it from
// any concurrent workload; it was found by a concurrent encode stress test and predates all of the
// current performance work.
//
// The lost increments a torn read produces are harmless on the wire -- markers are a pure index
// cache and a stale timestamp only evicts a suboptimal marker -- but a data race is undefined
// behaviour in Go regardless of how benign the value is, and it fails -race for any consumer inside
// OUR library rather than theirs.
//
// Kept global rather than per-type deliberately: the counter's only job is to order evictions
// WITHIN one type's marker list, so a per-type counter would be equivalent, but it would change
// three exported signatures in a file under active optimization for no behavioural gain. An
// uncontended atomic add is negligible beside the marker walk it accompanies.
var globalSearchMarkerTimestamp atomic.Int64

// A unique timestamp that identifies each marker.
// Time is relative,.. this is more like an ever-increasing clock.
type arraySearchMarker struct {
	p         *itemStruct
	index     Number
	timestamp Number
}

// listReadIndex is an immutable position cache for indexed reads. Writers keep using the
// reference-compatible search-marker cache above; readers publish one evenly-spaced snapshot and
// never mutate either cache. The immutable split is what makes concurrent reads of a quiescent
// document race-free without putting a lock on the mutation path.
type listReadIndex struct {
	positions []listReadPosition
}

type listReadPosition struct {
	p     *itemStruct
	index Number
}

// Immutable read indexes do not pay updateMarkerChanges on every mutation, so the mutable
// marker cache's sqrt(N) optimum does not apply. Sampling every four physical Items was the
// steady-read latency knee at 100k Items. Large lists defer the O(N) build until a second indexed
// read: a write/read loop then performs one uncached walk instead of allocating a snapshot that
// the next write immediately discards, while quiescent repeated reads retain the dense index.
const listReadIndexStride = 4

// Below this point the dense snapshot is at most about 64KB on 64-bit platforms; above it, defer
// until reuse so a single read after every mutation never creates larger transient data.
const deferListReadIndexBuildItems = 16_000

// A large list's first indexed read publishes primedListReadIndex and performs an uncached walk.
// Its second read replaces that sentinel with buildingListReadIndex and builds the dense snapshot.
// Other readers that encounter the build sentinel perform one uncached walk instead of duplicating
// a full-list scan and allocation.
var primedListReadIndex = &listReadIndex{}
var buildingListReadIndex = &listReadIndex{}

type abstractTypeBase struct {
	item             *itemStruct
	typeMap          map[string]*itemStruct
	start            *itemStruct
	doc              *Doc
	length           Number
	itemCount        Number
	eventHandler     *eventHandler // event handlers
	deepEventHandler *eventHandler // deep event handlers
	searchMarker     []*arraySearchMarker
}

func (*abstractTypeBase) isSharedType() {}

func asAbstractType(shared SharedType) abstractType {
	return shared.(abstractType)
}

func asSharedType(internal abstractType) SharedType {
	return internal.(SharedType)
}

func (t *abstractTypeBase) GetLength() Number {
	return t.length
}

func (t *abstractTypeBase) setLength(number Number) {
	t.length = number
}

func (t *abstractTypeBase) getItem() *itemStruct {
	return t.item
}

func (t *abstractTypeBase) getMap() map[string]*itemStruct {
	if t.typeMap == nil {
		t.typeMap = make(map[string]*itemStruct)
	}
	return t.typeMap
}

func (t *abstractTypeBase) getSearchMarker() *[]*arraySearchMarker {
	return &t.searchMarker
}

func (t *abstractTypeBase) setMap(m map[string]*itemStruct) {
	t.typeMap = m
}

func (t *abstractTypeBase) startItem() *itemStruct {
	return t.start
}

func (t *abstractTypeBase) setStartItem(item *itemStruct) {
	if item == nil {
		// Clearing the linked-list head discards every node at once (currently nested-type GC).
		// Keep that structural invariant local so future callers cannot leave a stale count.
		t.destroyOwnedListPositionIndex()
		t.start = nil
		t.itemCount = 0
		return
	}
	t.start = item
}

func (t *abstractTypeBase) linkedItemCount() Number {
	return t.itemCount
}

func (t *abstractTypeBase) updateLinkedItemCount(delta Number) {
	t.itemCount += delta
}

func (t *abstractTypeBase) setLinkedItemCount(count Number) {
	t.itemCount = count
}

type linkedItemCounter interface {
	linkedItemCount() Number
	updateLinkedItemCount(delta Number)
	setLinkedItemCount(count Number)
}

func listItemCount(t abstractType) Number {
	if counter, ok := t.(linkedItemCounter); ok {
		return counter.linkedItemCount()
	}
	// IAbstractType is public, so retain correct marker sizing for an external implementation
	// that cannot implement this package-private fast path.
	count := Number(0)
	for item := t.startItem(); item != nil; item = item.right {
		count++
	}
	return count
}

func updateListItemCount(t abstractType, delta Number) {
	if counter, ok := t.(linkedItemCounter); ok {
		counter.updateLinkedItemCount(delta)
	}
}

func setListItemCount(t abstractType, count Number) {
	if counter, ok := t.(linkedItemCounter); ok {
		counter.setLinkedItemCount(count)
	}
}

func (t *abstractTypeBase) getDoc() *Doc {
	return t.doc
}

func (t *abstractTypeBase) updateLength(n Number) {
	t.length += n
}

func (t *abstractTypeBase) setSearchMarker(marker []*arraySearchMarker) {
	if marker == nil {
		t.destroyOwnedListPositionIndex()
	}
	t.searchMarker = marker
}

func (t *abstractTypeBase) parentType() abstractType {
	if t.item == nil || t.item.parent == nil {
		return nil
	}

	return t.item.parent.(abstractType)
}

// integrate this type into the Yjs instance.
//
// * Save this struct in the os
// * This type is sent to other client
// * Observer functions are fired
func (t *abstractTypeBase) integrate(y *Doc, item *itemStruct) {
	t.doc = y
	t.item = item
}

func (t *abstractTypeBase) copyType() abstractType {
	return nil
}

func (t *abstractTypeBase) cloneType() abstractType {
	return nil
}

func (t *abstractTypeBase) writeType(encoder updateEncoder) {

}

// The first non-deleted item
func (t *abstractTypeBase) firstItem() *itemStruct {
	item := t.start
	for item != nil && item.isDeleted() {
		item = item.right
	}

	return item
}

// Creates YEvent and calls all type observers.
// Must be implemented by each type.
func (t *abstractTypeBase) callObserver(trans *Transaction, parentSubs ChangedSubs) {
	// yjs: `if (!transaction.local && this._searchMarker) { this._searchMarker.length = 0 }`
	// — clear the markers but keep the slice non-nil (still ENABLED). Setting it nil
	// would disable markers permanently (findMarker treats nil as disabled).
	if !trans.local && t.searchMarker != nil {
		t.searchMarker = t.searchMarker[:0]
	}
}

// Observe all events that are created on this type.
func (t *abstractTypeBase) Observe(f func(interface{}, interface{})) {
	if t.eventHandler == nil {
		t.eventHandler = newEventHandler()
	}
	addEventHandlerListener(t.eventHandler, f)
}

// Observe all events that are created by this type and its children.
func (t *abstractTypeBase) ObserveDeep(f func(interface{}, interface{})) {
	if t.deepEventHandler == nil {
		t.deepEventHandler = newEventHandler()
	}
	addEventHandlerListener(t.deepEventHandler, f)
}

// Unregister an observer function.
func (t *abstractTypeBase) Unobserve(f func(interface{}, interface{})) {
	removeEventHandlerListener(t.eventHandler, f)
}

// Unregister an observer function.
func (t *abstractTypeBase) UnobserveDeep(f func(interface{}, interface{})) {
	removeEventHandlerListener(t.deepEventHandler, f)
}

func (t *abstractTypeBase) ToJson() interface{} {
	return nil
}

func (t *abstractTypeBase) getDeepEventHandler() *eventHandler {
	if t.deepEventHandler == nil {
		t.deepEventHandler = newEventHandler()
	}
	return t.deepEventHandler
}

func (t *abstractTypeBase) getEventHandler() *eventHandler {
	if t.eventHandler == nil {
		t.eventHandler = newEventHandler()
	}
	return t.eventHandler
}

func newArraySearchMarker(p *itemStruct, index Number) *arraySearchMarker {
	return &arraySearchMarker{
		p:     p,
		index: index,
	}
}

func refreshMarkerTimestamp(marker *arraySearchMarker) {
	marker.timestamp = Number(globalSearchMarkerTimestamp.Add(1))
}

// This is rather complex so this function is the only thing that should overwrite a marker
func overwriteMarker(marker *arraySearchMarker, p *itemStruct, index Number) {
	marker.p.setMarker(false)

	p.setMarker(true)
	marker.p = p

	marker.index = index
	marker.timestamp = Number(globalSearchMarkerTimestamp.Add(1))
}

func markPosition(searchMarker *[]*arraySearchMarker, p *itemStruct, index Number) *arraySearchMarker {
	return markPositionWithLimit(searchMarker, p, index, maxSearchMarker)
}

func markPositionWithLimit(searchMarker *[]*arraySearchMarker, p *itemStruct, index Number, limit int) *arraySearchMarker {
	if len(*searchMarker) >= limit {
		// override oldest marker (we don't want to create more objects)
		marker := (*searchMarker)[0]
		for _, m := range *searchMarker {
			if m.timestamp < marker.timestamp {
				marker = m
			}
		}

		overwriteMarker(marker, p, index)
		return marker
	} else {
		// create new marker
		pm := newArraySearchMarker(p, index)
		*searchMarker = append(*searchMarker, pm)
		return pm
	}
}

// Search marker help us to find positions in the associative array faster.
//
// They speed up the process of finding a position without much bookkeeping.
//
// Direct markPosition calls retain the reference's maxSearchMarker cap. findMarker grows that cap
// as sqrt(linked Item count), using the same limit for spacing and eviction. Visible length is the
// wrong unit: one large paste is a single ContentString Item, while the cache walks list nodes.
//
// This function always returns a refreshed marker (updated timestamp)
func findMarker(yarray abstractType, index Number) *arraySearchMarker {
	if yarray.startItem() == nil || index == 0 || *yarray.getSearchMarker() == nil {
		return nil
	}
	return findMarkerWithItemCount(yarray, index, listItemCount(yarray))
}

func findMarkerWithItemCount(yarray abstractType, index, itemCount Number) *arraySearchMarker {
	// yjs: `if (_searchMarker === null) return null` — markers are DISABLED (nil
	// slice) for types that don't support them and for a YText once it gains
	// formatting (ContentFormat.integrate sets it nil). GetSearchMarker returns a
	// pointer to the field (never nil), so the disabled state is the nil SLICE.
	if yarray.startItem() == nil || index == 0 || *yarray.getSearchMarker() == nil {
		return nil
	}

	var marker *arraySearchMarker
	if len(*yarray.getSearchMarker()) > 0 {
		marker = (*yarray.getSearchMarker())[0]
		markerDistance := index - marker.index
		if markerDistance < 0 {
			markerDistance = -markerDistance
		}
		for _, m := range *yarray.getSearchMarker() {
			distance := index - m.index
			if distance < 0 {
				distance = -distance
			}
			if distance < markerDistance {
				marker = m
				markerDistance = distance
			}
		}
	}

	p := yarray.startItem()
	pindex := 0

	if marker != nil {
		p = marker.p
		pindex = marker.index
		refreshMarkerTimestamp(marker) // we used it, we might need to use it again
	}

	// iterate to right if possible
	for p.right != nil && pindex < index {
		if !p.isDeleted() && p.countable() {
			if index < pindex+p.length {
				break
			}
			pindex += p.length
		}
		p = p.right
	}

	// iterate to left if necessary (might be that pindex > index)
	for p.left != nil && pindex > index {
		p = p.left
		if !p.isDeleted() && p.countable() {
			pindex -= p.length
		}
	}

	// A marker may point at the right side of a pair that later becomes mergeable.
	// Item.MergeWith rewrites every such marker to the surviving left item and
	// adjusts its index at the exact moment the right item is removed. Keeping the
	// marker at the position found above is therefore safe, and avoids walking an
	// entire same-client clock run merely to choose a cache location.

	markerLimit := searchMarkerLimit(itemCount)
	if len(*yarray.getSearchMarker()) > markerLimit {
		// A type that grew large and was later shortened may retain its larger cache.
		// Do not churn those still-valid markers merely because the current target is
		// smaller; use the actual count when judging cache density.
		markerLimit = len(*yarray.getSearchMarker())
	}
	if marker != nil && Number(math.Abs(float64(marker.index-pindex))) < p.parent.(abstractType).GetLength()/Number(markerLimit) {
		// adjust existing marker
		overwriteMarker(marker, p, pindex)
		return marker
	} else {
		// create new marker
		return markPositionWithLimit(yarray.getSearchMarker(), p, pindex, markerLimit)
	}
}

func listReadIndexPointer(t abstractType) *atomic.Pointer[listReadIndex] {
	switch typed := t.(type) {
	case *YArray:
		return &typed.readIndex
	case *YXmlFragment:
		return &typed.readIndex
	case *YXmlElement:
		return &typed.readIndex
	default:
		return nil
	}
}

func invalidateListReadIndex(t abstractType) {
	if cache := listReadIndexPointer(t); cache != nil && cache.Load() != nil {
		cache.Store(nil)
	}
}

func buildListReadIndex(t abstractType) *listReadIndex {
	itemCount := listItemCount(t)
	positions := make([]listReadPosition, 0, itemCount/listReadIndexStride)
	visibleIndex := Number(0)
	physicalIndex := Number(0)
	nextPhysicalIndex := Number(listReadIndexStride)
	for p := t.startItem(); p != nil; p = p.right {
		live := !p.isDeleted() && p.countable()
		if live && physicalIndex >= nextPhysicalIndex {
			positions = append(positions, listReadPosition{p: p, index: visibleIndex})
			nextPhysicalIndex = physicalIndex + listReadIndexStride
		}
		if live {
			visibleIndex += p.length
		}
		physicalIndex++
	}
	return &listReadIndex{positions: positions}
}

// listReadPositions returns an immutable, evenly-spaced index. A freshly decoded document has no
// write markers at all; merely declining to mutate that cache made repeated random Get calls on a
// 100k fragmented array roughly 100x slower. Building a separate read index retains that workload
// while keeping concurrent readers free of timestamp, slice, and Item marker-bit writes.
func listReadPositions(t abstractType) []listReadPosition {
	cache := listReadIndexPointer(t)
	doc := t.getDoc()
	if cache == nil || doc == nil || !doc.readCacheEnabled || t.startItem() == nil || t.startItem().right == nil {
		return nil
	}
	for {
		index := cache.Load()
		switch index {
		case buildingListReadIndex:
			return nil
		case nil:
			if listItemCount(t) >= deferListReadIndexBuildItems {
				if cache.CompareAndSwap(nil, primedListReadIndex) {
					return nil
				}
				continue
			}
			if !cache.CompareAndSwap(nil, buildingListReadIndex) {
				continue
			}
		case primedListReadIndex:
			if !cache.CompareAndSwap(primedListReadIndex, buildingListReadIndex) {
				continue
			}
		default:
			return index.positions
		}
		built := buildListReadIndex(t)
		if cache.CompareAndSwap(buildingListReadIndex, built) {
			return built.positions
		}
		// A writer invalidated the cache while it was being built. Concurrent mutation of one
		// document is not supported, but never publish the stale snapshot even in that case.
		return nil
	}
}

func nearestReadPosition(positions []listReadPosition, index Number) (listReadPosition, bool) {
	if len(positions) == 0 {
		return listReadPosition{}, false
	}
	left, right := 0, len(positions)
	first, last := positions[0].index, positions[len(positions)-1].index
	if span := last - first; span > 0 && index >= first && index <= last {
		// Physical samples are nearly uniform in the common fragmented-list case. An
		// interpolation probe therefore usually brackets the target immediately; binary search
		// remains the bounded fallback for variable-sized content and tombstone-heavy lists.
		probe := int(float64(index-first) / float64(span) * float64(len(positions)-1))
		switch {
		case positions[probe].index <= index &&
			(probe+1 == len(positions) || positions[probe+1].index > index):
			left = probe + 1
			right = left
		case positions[probe].index <= index:
			left = probe + 1
		case probe == 0 || positions[probe-1].index <= index:
			left = probe
			right = left
		default:
			right = probe
		}
	}
	// Upper-bound search makes an exact-boundary entry the predecessor, so a lookup at a sampled
	// item's start walks forward from that item rather than backward from the next sample.
	for left < right {
		middle := int(uint(left+right) >> 1)
		if positions[middle].index <= index {
			left = middle + 1
		} else {
			right = middle
		}
	}
	if left == 0 {
		return positions[0], true
	}
	if left == len(positions) {
		return positions[len(positions)-1], true
	}
	before, after := positions[left-1], positions[left]
	if index-before.index <= after.index-index {
		return before, true
	}
	return after, true
}

// walkToIndex advances (or retreats) from an item's known start index to the item covering the
// target. Immutable read-index entries are evenly spaced and the nearest one may be on either side.
func walkToIndex(p *itemStruct, pindex, index Number) (*itemStruct, Number) {
	if p == nil {
		return nil, pindex
	}
	for p.right != nil && pindex < index {
		if !p.isDeleted() && p.countable() {
			if index < pindex+p.length {
				break
			}
			pindex += p.length
		}
		p = p.right
	}
	for p.left != nil && pindex > index {
		p = p.left
		if !p.isDeleted() && p.countable() {
			pindex -= p.length
		}
	}
	return p, pindex
}

// Update markers when a change happened.
// This should be called before doing a deletion!
func updateMarkerChanges(searchMarker *[]*arraySearchMarker, index Number, length Number) {
	// Inserting strictly after every marker whose item is already live and countable cannot alter
	// any marker: the item walk is a no-op and the index adjustment condition is false. Sequential
	// appends hit this case once markers exist; avoid repeatedly toggling the same item marker bits.
	if length > 0 {
		unchanged := true
		for _, marker := range *searchMarker {
			if marker.p == nil || marker.p.isDeleted() || !marker.p.countable() || index <= marker.index {
				unchanged = false
				break
			}
		}
		if unchanged {
			return
		}
	}
	for i := len(*searchMarker) - 1; i >= 0; i-- {
		m := (*searchMarker)[i]
		if length > 0 {
			p := m.p
			p.setMarker(false)

			// Ideally we just want to do a simple position comparison, but this will only work if
			// search markers don't point to deleted items for formats.
			// Iterate marker to prev undeleted countable position so we know what to do when updating a position
			for p != nil && (p.isDeleted() || !p.countable()) {
				p = p.left
				if p != nil && !p.isDeleted() && p.countable() {
					// adjust position. the loop should break now
					m.index -= p.length
				}
			}

			if p == nil || p.marker() {
				// remove search marker if updated position is null or if position is already marked
				*searchMarker = append((*searchMarker)[:i], (*searchMarker)[i+1:]...)
				continue
			}

			p.setMarker(true)
			m.p = p
		}

		// a simple index <= m.index check would actually suffice
		if index < m.index || length > 0 && index == m.index {
			m.index = maxNumber(index, m.index+length)
		}
	}
}

// Call event listeners with an event. This will also add an event to all
// parents (for `.observeDeep` handlers).
func callTypeObservers(t abstractType, trans *Transaction, event IEventType) {
	trans.materializeObservableFields()
	changedType := t
	changedParentTypes := trans.changedParentTypes

	for {
		_, exist := changedParentTypes[t]
		if !exist {
			changedParentTypes[t] = append(changedParentTypes[t], event)
		}

		if t.getItem() == nil {
			break
		}

		t = t.getItem().parent.(abstractType)
	}
	callEventHandlerListeners(changedType.getEventHandler(), event, trans)
}

// hasTypeObservers reports whether an event from t can be observed directly or
// by a deep observer on t or one of its ancestors. It is evaluated during
// cleanup, so an observer registered after a mutation but before transaction
// cleanup still receives the event.
func hasTypeObservers(t abstractType) bool {
	if eh, _ := existingTypeEventHandlers(t); eh != nil && len(eh.listeners) > 0 {
		return true
	}
	for p := t; p != nil; p = p.parentType() {
		if _, deh := existingTypeEventHandlers(p); deh != nil && len(deh.listeners) > 0 {
			return true
		}
	}
	// ChangedParentTypes is observable through document callbacks and is used
	// by UndoManager's afterTransaction listener. Preserve the complete event
	// graph whenever any document listener is present.
	if doc := t.getDoc(); doc != nil && doc.HasObservers() {
		return true
	}
	return false
}

func existingTypeEventHandlers(t abstractType) (*eventHandler, *eventHandler) {
	// A concrete type switch avoids calling the public lazy accessors on the hot
	// unobserved mutation path. The fallback preserves support for third-party
	// IAbstractType implementations, whose handlers are owned by that type.
	switch value := t.(type) {
	case *YArray:
		return value.eventHandler, value.deepEventHandler
	case *YMap:
		return value.eventHandler, value.deepEventHandler
	case *YText:
		return value.eventHandler, value.deepEventHandler
	case *YXmlFragment:
		return value.eventHandler, value.deepEventHandler
	case *YXmlElement:
		return value.eventHandler, value.deepEventHandler
	case *YXmlText:
		return value.eventHandler, value.deepEventHandler
	case *yXmlHook:
		return value.eventHandler, value.deepEventHandler
	case *yString:
		return value.eventHandler, value.deepEventHandler
	}
	return t.getEventHandler(), t.getDeepEventHandler()
}

func newAbstractType() SharedType {
	return &abstractTypeBase{
		typeMap: make(map[string]*itemStruct),
	}
}

func typeListSlice(t abstractType, start, end Number) ArrayAny {
	if start < 0 {
		start = t.GetLength() + start
	}

	if end < 0 {
		end = t.GetLength() + end
	}

	length := end - start
	capacity := minNumber(maxNumber(length, 0), t.GetLength())
	cs := make(ArrayAny, capacity)
	written := 0
	n := t.startItem()
	for n != nil && length > 0 {
		if n.countable() && !n.isDeleted() {
			switch content := n.content.(type) {
			case *contentAny:
				contentLength := len(content.arr)
				if contentLength <= start {
					start -= contentLength
					break
				}
				take := minNumber(contentLength-start, length)
				written += copy(cs[written:], content.arr[start:start+take])
				length -= take
				start = 0
			case *contentType:
				if start > 0 {
					start--
					break
				}
				cs[written] = content.value
				written++
				length--
			default:
				c := content.contentValues()
				if len(c) <= start {
					start -= len(c)
					break
				}
				take := minNumber(len(c)-start, length)
				written += copy(cs[written:], c[start:start+take])
				length -= take
				start = 0
			}
		}
		n = n.right
	}

	return cs[:written]
}

func typeListToArray(t abstractType) ArrayAny {
	if t.GetLength() == 0 {
		return nil
	}
	cs := make(ArrayAny, 0, t.GetLength())
	n := t.startItem()
	for n != nil {
		if n.countable() && !n.isDeleted() {
			if content, ok := n.content.(*contentAny); ok {
				cs = append(cs, content.arr...)
			} else {
				c := n.content.contentValues()
				cs = append(cs, c...)
			}
		}
		n = n.right
	}

	return cs
}

// TypeListToArraySnapshot returns the visible list contents of t at snapshot.
// The result is caller-owned. SharedType keeps the snapshot capability public
// without reopening the internal Item/AbstractType object graph.
func TypeListToArraySnapshot(t SharedType, snapshot *Snapshot) ArrayAny {
	internal := asAbstractType(t)
	var result ArrayAny
	for item := internal.startItem(); item != nil; item = item.right {
		if item.countable() && isVisible(item, snapshot) {
			result = append(result, item.content.contentValues()...)
		}
	}
	return result
}

// Executes a provided function on once on overy element of this YArray.
func typeListForEach(t abstractType, f func(interface{}, Number, abstractType)) {
	index := 0
	n := t.startItem()
	for n != nil {
		if n.countable() && !n.isDeleted() {
			c := n.content.contentValues()
			for i := 0; i < len(c); i++ {
				f(c[i], index, t)
				index++
			}
		}
		n = n.right
	}
}

func typeListMap(t abstractType, f func(c interface{}, i Number, _ abstractType) interface{}) ArrayAny {
	if t.GetLength() == 0 {
		return nil
	}
	result := make(ArrayAny, 0, t.GetLength())
	index := 0
	for n := t.startItem(); n != nil; n = n.right {
		if n.countable() && !n.isDeleted() {
			content := n.content.contentValues()
			for i := 0; i < len(content); i++ {
				result = append(result, f(content[i], index, t))
				index++
			}
		}
	}
	return result
}

// func TypeListCreateIterator(t IAbstractType) {
// }

func typeListGet(t abstractType, index Number) interface{} {
	n := t.startItem()
	pindex := Number(0)
	if index != 0 {
		if position, ok := nearestReadPosition(listReadPositions(t), index); ok {
			n = position.p
			pindex = position.index
		}
	}
	n, pindex = walkToIndex(n, pindex, index)
	index -= pindex

	for ; n != nil; n = n.right {
		if !n.isDeleted() && n.countable() {
			if index < n.length {
				return n.content.contentValues()[index]
			}
			index -= n.length
		}
	}

	return nil
}

// isAnyEncodable reports whether v is a primitive/any-encodable value that WriteAny routes to
// ContentAny — every JS-number integer width, int64, float32/64, bool, string, Object, ArrayAny,
// and the null/undefined sentinels. Types with their own content kind ([]uint8→Binary, *Doc→Doc,
// IAbstractType→Type) are NOT any-encodable and are handled separately. Single source of truth for
// the set that Y.Array insert (typeListInsertGenericsAfter) and Y.Map set (typeMapSet) classify.
func isAnyEncodable(v any) bool {
	switch v.(type) {
	case Number, int8, int16, int32, int64, uint8, uint16, uint32, float32, float64, Object, bool, ArrayAny, string, NullType, UndefinedType:
		return true
	default:
		return false
	}
}

// typeListPushGenerics appends content to the END OF THE ITEM LIST, tombstones included.
//
// This is NOT the same as inserting at the visible length, and the difference is observable on the
// wire. yjs typeListPushGenerics (src/types/AbstractType.js) starts from the highest search marker,
// walks `right` to the last item — deleted ones too — and inserts AFTER it. Resolving a visible
// INDEX instead lands BEFORE any trailing tombstones, which gives the new item a right origin where
// the reference gives it a left origin: a one-byte difference in the item info byte (0x48 vs 0x88).
//
// Found by the differential once `push` was added to the generators: `unshift([40]); delete(0,1);
// push(["z"])` diverged, because after the delete the visible length is 0 while a tombstone still
// occupies the list.
func typeListPushGenerics(trans *Transaction, parent abstractType, content ArrayAny) error {
	// Start from the marker with the highest index, exactly as the reference does — it is an
	// optimisation, not a semantic choice, but starting elsewhere would walk from the wrong place
	// when markers are enabled.
	n := parent.startItem()
	nIndex := Number(0)
	var bestMarker *arraySearchMarker
	if sm := parent.getSearchMarker(); sm != nil && *sm != nil {
		best := -1
		for _, marker := range *sm {
			if marker.index > best {
				best = marker.index
				n = marker.p
				nIndex = marker.index
				bestMarker = marker
			}
		}
	}
	if n != nil {
		for n.right != nil {
			if !n.isDeleted() && n.countable() {
				nIndex += n.length
			}
			n = n.right
		}
		// Push consumes the marker cache but historically never refreshed it, so an
		// append-only caller kept restarting from the same old item. Persist the tail
		// reached above for the next call. The marker describes the start index of n;
		// it is a pure cache and is not encoded on the wire.
		if searchMarkers := parent.getSearchMarker(); searchMarkers != nil && *searchMarkers != nil {
			if bestMarker != nil {
				overwriteMarker(bestMarker, n, nIndex)
			} else {
				n.setMarker(true)
				markPosition(searchMarkers, n, nIndex)
			}
		}
	}
	return typeListInsertGenericsAfter(trans, parent, n, content)
}

func typeListInsertGenericsAfter(trans *Transaction, parent abstractType, referenceItem *itemStruct, content ArrayAny) error {
	left := referenceItem
	doc := trans.doc
	ownClientId := doc.ClientID
	store := doc.store

	var right *itemStruct
	if referenceItem == nil {
		right = parent.startItem()
	} else {
		right = referenceItem.right
	}

	// Cleanup normally merges a locally-appended ContentAny item straight back into its left
	// neighbour. On a package-owned transaction with no possible observer, perform that guaranteed
	// merge directly: the resulting struct, clocks, list links, and encoded bytes are identical,
	// while the short-lived Item, origin ID, ContentAny, and copied one-element staging slice never
	// need to exist. Observed/public transactions retain the ordinary item boundary until callbacks
	// finish, and every non-tail/conflicting/deleted/nested-parent case uses the general algorithm.
	// The strict tail guards are also why existing marker start-indices need no adjustment; loosening
	// them requires routing through updateMarkerChanges.
	if trans.compactState && trans.changedTypes == nil && right == nil && left != nil &&
		left.right == nil && left.parentSub == "" && !left.isDeleted() && left.countable() &&
		left.redone == nil && left.rightOrigin == nil && left.id.Client == ownClientId &&
		left.parent == parent && (parent.getItem() == nil || !parent.getItem().isDeleted()) &&
		!hasTypeObservers(parent) {
		clientStructs, exists := store.clientStructs(ownClientId)
		if exists {
			lastStruct := clientStructs.lastValue()
			if lastStruct.getID().Clock+lastStruct.structLength() == left.id.Clock+left.length {
				allAny := len(content) > 0
				for _, value := range content {
					if !isAnyEncodable(value) {
						allAny = false
						break
					}
				}
				if leftContent, ok := left.content.(*contentAny); ok && allAny {
					invalidateYXmlSliceCache(parent)
					leftContent.arr = append(leftContent.arr, content...)
					left.length += len(content)
					clientStructs.refreshValue(left)
					parent.updateLength(len(content))
					if doc.positionIndexes != nil {
						updateListPositionIndexAfterTailGrowth(parent, left, Number(len(content)))
					}
					return nil
				}
			}
		}
	}
	// Nested types are overwhelmingly inserted one at a time (XML nodes and Y.Array children).
	// Bypass the mixed-content staging closure and its repeated content classification for that
	// exact shape; integration and item origins are identical to the generic loop below.
	if len(content) == 1 {
		if nested, ok := content[0].(abstractType); ok {
			storage := doc.allocateTypeItemStorage()
			storage.content.value = nested
			left = initItemWithLength(&storage.item,
				GenID(ownClientId, getState(store, ownClientId)), left, getItemLastID(left),
				right, getItemID(right), parent, "", &storage.content, 1)
			if err := left.integrateStruct(trans, 0); err != nil {
				return err
			}
			return nil
		}
		if isAnyEncodable(content[0]) {
			storage := doc.allocateMapItemStorage()
			storage.value[0] = content[0]
			storage.content.arr = storage.value[:]
			left = initItemWithLength(&storage.item,
				GenID(ownClientId, getState(store, ownClientId)), left, getItemLastID(left),
				right, getItemID(right), parent, "", &storage.content, 1)
			if err := left.integrateStruct(trans, 0); err != nil {
				return err
			}
			return nil
		}
	}

	jsonContent := ArrayAny{}
	packJsonContent := func() error {
		if len(jsonContent) > 0 {
			left = newItem(GenID(ownClientId, getState(store, ownClientId)), left, getItemLastID(left), right, getItemID(right), parent, "", newContentAny(jsonContent))
			if err := left.integrateStruct(trans, 0); err != nil {
				return err
			}
			jsonContent = nil
		}
		return nil
	}

	for _, c := range content {
		// Primitive any-encodable values batch into one ContentAny (see isAnyEncodable); types
		// with their own content kind ([]uint8→Binary, *Doc→Doc, IAbstractType→Type) flush the
		// batch and integrate individually.
		if isAnyEncodable(c) {
			jsonContent = append(jsonContent, c)
			continue
		}
		if err := packJsonContent(); err != nil {
			return err
		}
		switch c := c.(type) {
		case []uint8, string:
			left = newItem(GenID(ownClientId, getState(store, ownClientId)), left, getItemLastID(left), right, getItemID(right), parent, "", newContentBinary(c.([]uint8)))
			if err := left.integrateStruct(trans, 0); err != nil {
				return err
			}
		case *Doc:
			left = newItem(GenID(ownClientId, getState(store, ownClientId)), left, getItemLastID(left), right, getItemID(right), parent, "", newContentDoc(c))
			if err := left.integrateStruct(trans, 0); err != nil {
				return err
			}
		default:
			if isAbstractType(c) {
				storage := doc.allocateTypeItemStorage()
				storage.content.value = c.(abstractType)
				left = initItemWithLength(&storage.item,
					GenID(ownClientId, getState(store, ownClientId)), left, getItemLastID(left),
					right, getItemID(right), parent, "", &storage.content, 1)
				if err := left.integrateStruct(trans, 0); err != nil {
					return err
				}
			} else {
				return errors.New("unexpected content type in insert operation")
			}
		}
	}

	if err := packJsonContent(); err != nil {
		return err
	}
	return nil
}

func typeListInsertGenerics(trans *Transaction, parent abstractType, index Number, content ArrayAny) error {
	if index > parent.GetLength() {
		return errors.New("[crdt] length exceeded")
	}

	if index == 0 {
		if *parent.getSearchMarker() != nil { // yjs: if (parent._searchMarker) — markers ENABLED (non-nil slice)
			updateMarkerChanges(parent.getSearchMarker(), index, len(content))
		}
		return typeListInsertGenericsAfter(trans, parent, nil, content)
	}

	// A sequentially-built list commonly consists of one merged ContentAny item.
	// Its visible end is unambiguous, so marker lookup/update can add no information:
	// inserting after that sole live item is exactly the general path's result.
	if index == parent.GetLength() {
		start := parent.startItem()
		if start != nil && start.right == nil && !start.isDeleted() && start.countable() && start.length == index {
			return typeListInsertGenericsAfter(trans, parent, start, content)
		}
	}

	startIndex := index
	n := parent.startItem()
	positionItem, positionStart, positioned := findMutationPosition(parent, index)
	if positioned {
		n = positionItem
		index -= positionStart
	}
	if positioned {
		// we need to iterate one to the left so that the algorithm works
		if index == 0 {
			// @todo refactor this as it actually doesn't consider formats
			n = n.prevItem() // important! get the left undeleted item so that we can actually decrease index
			if n != nil && n.countable() && !n.isDeleted() {
				index += n.length
			}
		}
	}

	for ; n != nil; n = n.right {
		if !n.isDeleted() && n.countable() {
			if index <= n.length {
				if index < n.length {
					// insert in-between
					getItemCleanStart(trans, GenID(n.id.Client, n.id.Clock+index))
				}
				break
			}
			index -= n.length
		}
	}

	if *parent.getSearchMarker() != nil { // yjs: if (parent._searchMarker) — markers ENABLED (non-nil slice)
		updateMarkerChanges(parent.getSearchMarker(), startIndex, len(content))
	}

	return typeListInsertGenericsAfter(trans, parent, n, content)
}

func typeListDelete(trans *Transaction, parent abstractType, index Number, length Number) error {
	if length == 0 {
		return nil
	}

	startIndex := index
	startLength := length
	n := parent.startItem()
	if positionItem, positionStart, ok := findMutationPosition(parent, index); ok {
		n = positionItem
		index -= positionStart
	}

	// compute the first item to be deleted
	for ; n != nil && index > 0; n = n.right {
		if !n.isDeleted() && n.countable() {
			if index < n.length {
				getItemCleanStart(trans, GenID(n.id.Client, n.id.Clock+index))
			}
			index -= n.length
		}
	}

	// delete all items until done
	for length > 0 && n != nil {
		if !n.isDeleted() {
			if length < n.length {
				getItemCleanStart(trans, GenID(n.id.Client, n.id.Clock+length))
			}
			n.deleteItemStruct(trans)
			length -= n.length
		}
		n = n.right
	}

	if length > 0 {
		return errors.New("length exceeded")
	}

	if *parent.getSearchMarker() != nil { // yjs: if (parent._searchMarker) — markers ENABLED (non-nil slice)
		updateMarkerChanges(parent.getSearchMarker(), startIndex, -startLength+length) // in case we remove the above exception
	}

	return nil
}

func typeMapDelete(trans *Transaction, parent abstractType, key string) {
	c, exist := parent.getMap()[key]
	if exist {
		c.deleteItemStruct(trans)
	}
}

type itemWithSingleAny struct {
	item    itemStruct
	content contentAny
	value   [1]any
}

type itemWithContentType struct {
	item    itemStruct
	content contentType
}

type itemWithContentString struct {
	item    itemStruct
	content contentString
}

type itemWithContentFormat struct {
	item    itemStruct
	content contentFormat
}

type itemWithContentEmbed struct {
	item    itemStruct
	content contentEmbed
}

func typeMapSet(trans *Transaction, parent abstractType, key string, value interface{}) error {
	parentMap := parent.getMap()
	left := parentMap[key]
	doc := trans.doc
	ownClientId := doc.ClientID
	clientStructs, _ := doc.store.clientStructs(ownClientId)
	clock := getState(doc.store, ownClientId)
	// nil and primitive any-encodable values batch into ContentAny (see isAnyEncodable); types
	// with their own content kind ([]uint8→Binary, *Doc→Doc, IAbstractType→Type) get their own.
	if value == nil || isAnyEncodable(value) {
		if left == nil {
			if ymap, ok := parent.(*YMap); ok {
				ymap.setFreshPrimitiveKnown(trans, key, value)
				return nil
			}
		}
		storage := doc.allocateMapItemStorage()
		storage.value[0] = value
		storage.content.arr = storage.value[:]
		if left == nil {
			storage.item = itemStruct{
				abstractStructBase: abstractStructBase{id: GenID(ownClientId, clock), length: 1},
				parent:             parent,
				parentSub:          key,
				content:            &storage.content,
				info:               bit2,
			}
			storage.item.integrateNewPrimitiveMapKey(trans, parent, parentMap, clientStructs)
		} else {
			item := initItemWithLength(&storage.item, GenID(ownClientId, clock),
				left, getItemLastID(left), nil, nil, parent, key, &storage.content, 1)
			if left.right == nil {
				item.integratePrimitiveMapOverwrite(trans, parent, parentMap, clientStructs)
			} else {
				if err := item.integrateStruct(trans, 0); err != nil {
					return err
				}
			}
		}
		return nil
	}

	var content itemContent
	switch value := value.(type) {
	case []uint8:
		content = newContentBinary(value)
	case *Doc:
		content = newContentDoc(value)
	default:
		if isAbstractType(value) {
			storage := doc.allocateTypeItemStorage()
			storage.content.value = value.(abstractType)
			item := initItemWithLength(&storage.item, GenID(ownClientId, clock), left,
				getItemLastID(left), nil, nil, parent, key, &storage.content, 1)
			if left == nil {
				item.integrateNewMapKey(trans, parent, parentMap, clientStructs)
			} else {
				if err := item.integrateStruct(trans, 0); err != nil {
					return err
				}
			}
			return nil
		} else {
			return errors.New("unexpected content type")
		}
	}

	item := newItem(GenID(ownClientId, clock), left, getItemLastID(left), nil, nil, parent, key, content)
	if left == nil {
		item.integrateNewMapKey(trans, parent, parentMap, clientStructs)
	} else {
		if err := item.integrateStruct(trans, 0); err != nil {
			return err
		}
	}
	return nil
}

func typeMapGet(parent abstractType, key string) interface{} {
	val, exist := parent.getMap()[key]
	if !exist || val.isDeleted() {
		return nil
	}

	return val.content.contentValues()[val.length-1]
}

func typeMapGetAll(parent abstractType) Object {
	items := parent.getMap()
	if len(items) == 1 {
		for key, item := range items {
			if !item.isDeleted() {
				return Object{d: &objectData{
					smallKeys: [2]string{key}, smallValues: [2]any{mapItemValue(item)}, smallLen: 1,
				}}
			}
		}
		return newObject()
	}
	res := newObjectWithCapacity(len(items))
	for key, item := range items {
		if !item.isDeleted() {
			// Each source-map key is visited exactly once, so no duplicate check is
			// needed while constructing this fresh ordered Object.
			res.appendNew(key, mapItemValue(item))
		}
	}
	return res
}

func typeMapHas(parent abstractType, key string) bool {
	val, exist := parent.getMap()[key]
	return exist && !val.isDeleted()
}

func typeMapGetSnapshot(parent abstractType, key string, snapshot *Snapshot) interface{} {
	v := parent.getMap()[key]
	hasClient := func(client Number) bool {
		_, exist := snapshot.stateVector[client]
		return exist
	}
	for v != nil && (!hasClient(v.id.Client) || v.id.Clock >= snapshot.stateVector[v.id.Client]) {
		v = v.left
	}

	if v != nil && isVisible(v, snapshot) {
		return v.content.contentValues()[v.length-1]
	}

	return nil
}

// TypeMapGetSnapshot reads key from a heterogeneous map-like shared type as it
// existed at snapshot.
func TypeMapGetSnapshot(parent SharedType, key string, snapshot *Snapshot) interface{} {
	if parent == nil {
		return nil
	}
	return typeMapGetSnapshot(asAbstractType(parent), key, snapshot)
}

// typeMapGetAllSnapshot reads every key of a map-like type AS OF a snapshot, walking each key's
// item chain back to the last version the snapshot can see.
//
// Faithful to yjs typeMapGetAllSnapshot (src/types/AbstractType.js): for each key, walk left while
// the item is not in the snapshot's state vector, then include it only if it is visible there. A
// key whose every version postdates the snapshot is absent from the result entirely.
//
// This is the Y.Map counterpart of ToDelta-with-a-snapshot, which was found in this feature to have
// never once executed — the same history/time-travel area, so it is covered by the differential
// oracle and not only by a unit test.
//
// Key order: the reference iterates a JS Map in insertion order; Go map iteration is randomised, so
// keys are emitted SORTED. The result is an ordered Object either way, and a caller reading it back
// deserves a deterministic order rather than one that changes per run.
func typeMapGetAllSnapshot(parent abstractType, snapshot *Snapshot) Object {
	res := newObject()
	if parent == nil || snapshot == nil {
		return res
	}
	m := parent.getMap()
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		v := m[key]
		for v != nil {
			clock, tracked := snapshot.stateVector[v.id.Client]
			if tracked && v.id.Clock < clock {
				break
			}
			v = v.left
		}
		if v != nil && isVisible(v, snapshot) {
			content := v.content.contentValues()
			if v.length > 0 && len(content) >= v.length {
				res.Set(key, content[v.length-1])
			}
		}
	}
	return res
}

// TypeMapGetAllSnapshot returns every visible key/value pair from a
// heterogeneous map-like shared type as it existed at snapshot.
func TypeMapGetAllSnapshot(parent SharedType, snapshot *Snapshot) Object {
	if parent == nil {
		return newObject()
	}
	return typeMapGetAllSnapshot(asAbstractType(parent), snapshot)
}

// logType renders a type's children for debugging: every child in order, tombstones included, then
// the content of the undeleted ones.
//
// Faithful to yjs logType (src/utils/logging.js), which console.logs the child list and then the
// content of the non-deleted children. Returning a string rather than writing to stdout is the Go
// adaptation: a library that prints unconditionally is unusable inside a consumer's own logging,
// and the reference's own docstring warns the output can be immense.
func logType(t abstractType) string {
	if t == nil {
		return "<nil type>"
	}
	var b strings.Builder
	var children []*itemStruct
	for n := t.startItem(); n != nil; n = n.right {
		children = append(children, n)
	}
	fmt.Fprintf(&b, "children: %d\n", len(children))
	for i, n := range children {
		fmt.Fprintf(&b, "  [%d] id=%d:%d len=%d deleted=%t content=%T\n",
			i, n.id.Client, n.id.Clock, n.length, n.isDeleted(), n.content)
	}
	b.WriteString("content:\n")
	for _, n := range children {
		if n.isDeleted() {
			continue
		}
		fmt.Fprintf(&b, "  %v\n", n.content.contentValues())
	}
	return b.String()
}
