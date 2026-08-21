package crdt

import (
	"maps"
	"slices"
	"sort"
	"sync/atomic"
)

const maxYMapCachedKeys = 4096
const maxYMapCachedEntries = 2048
const yMapEntriesCacheThreshold = 8

type yMapKeysCache struct {
	keys []string
}

type yMapEntriesCache struct {
	entries map[string]interface{}
}

type yMapJSONCache struct {
	value Object
}

// YMapEvent describes the changes on a YMap.
type YMapEvent struct {
	YEvent
	KeysChanged ChangedSubs
}

func newYMapEvent(ymap *YMap, trans *Transaction, subs ChangedSubs) *YMapEvent {
	return &YMapEvent{
		YEvent:      *newYEvent(ymap, trans),
		KeysChanged: subs,
	}
}

// YMap is a shared Map implementation.
type YMap struct {
	abstractTypeBase
	prelimContent map[string]interface{}
	size          Number
	keysCache     atomic.Pointer[yMapKeysCache]
	keysPrimed    atomic.Bool
	entriesCache  atomic.Pointer[yMapEntriesCache]
	entriesReads  atomic.Uint32
	jsonCache     atomic.Pointer[yMapJSONCache]
	jsonReads     atomic.Uint32
}

func adjustYMapSize(parent abstractType, delta Number) {
	if ymap, ok := parent.(*YMap); ok {
		ymap.size += delta
	}
}

func (y *YMap) recountSize() {
	y.size = 0
	for _, item := range y.typeMap {
		if !item.isDeleted() {
			y.size++
		}
	}
}

func invalidateYMapReadCache(t abstractType) {
	if ymap, ok := t.(*YMap); ok {
		invalidateYMapReadCacheValue(ymap)
	}
}

func invalidateYMapReadCacheValue(ymap *YMap) {
	if ymap.keysCache.Load() != nil {
		ymap.keysCache.Store(nil)
	}
	if ymap.keysPrimed.Load() {
		ymap.keysPrimed.Store(false)
	}
	if ymap.entriesCache.Load() != nil {
		ymap.entriesCache.Store(nil)
	}
	if ymap.entriesReads.Load() != 0 {
		ymap.entriesReads.Store(0)
	}
	if ymap.jsonCache.Load() != nil {
		ymap.jsonCache.Store(nil)
	}
	if ymap.jsonReads.Load() != 0 {
		ymap.jsonReads.Store(0)
	}
}

// integrate this type into the Yjs instance.
//
//	Save this struct in the os
//	This type is sent to other client
//	Observer functions are fired
func (y *YMap) integrate(doc *Doc, item *itemStruct) {
	y.abstractTypeBase.integrate(doc, item)
	// Flush in SORTED key order. yjs iterates a JS Map, which is insertion-ordered and so
	// deterministic; Go map iteration is randomised per run, which made the integrated item
	// clocks — and therefore EncodeStateAsUpdate — differ between runs for the identical
	// locally-built map (measured: 4 distinct byte streams across 40 identical builds). Two
	// peers building the same prelim structure would emit different CRDT ids for the same
	// logical edit. PrelimContent is a Go map, so true insertion order is already lost at
	// the API boundary; sorted order is the strongest determinism available here, and
	// matches the existing precedent (writeStateVector's client sort, popStackItem's).
	keys := make([]string, 0, len(y.prelimContent))
	for key := range y.prelimContent {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		y.Set(key, y.prelimContent[key])
	}

	y.prelimContent = make(map[string]interface{})
}

func (y *YMap) copyType() abstractType {
	return NewYMap(nil)
}

func (y *YMap) cloneType() abstractType {
	m := NewYMap(nil)

	y.ForEach(func(key string, value interface{}, yMap *YMap) {
		v, ok := value.(abstractType)
		if ok {
			m.Set(key, v.cloneType())
		} else {
			m.Set(key, value)
		}
	})

	return m
}

// Clone returns an independent YMap with cloned nested shared values.
func (y *YMap) Clone() *YMap { return y.cloneType().(*YMap) }

// Creates YMapEvent and calls observers.
func (y *YMap) callObserver(trans *Transaction, parentSubs ChangedSubs) {
	if hasTypeObservers(y) {
		callTypeObservers(y, trans, newYMapEvent(y, trans, parentSubs))
	}
}

// Transforms this Shared Type to a JSON object.
func (y *YMap) toJSONValue() interface{} { return y.ToJSON() }

func (y *YMap) ToJSON() interface{} {
	if cached := y.jsonCache.Load(); cached != nil {
		return cached.value.ShallowClone()
	}
	m := newObjectWithCapacity(len(y.typeMap))
	cacheable := true
	for key, item := range y.typeMap {
		if !item.isDeleted() {
			v := mapItemValue(item)
			t, ok := v.(abstractType)
			if ok {
				cacheable = false
				m.Set(key, t.toJSONValue())
			} else {
				if _, undefined := v.(UndefinedType); !undefined {
					m.Set(key, v)
				} else {
					// ToJSON omits undefined-valued keys while Keys and Entries retain them. Such
					// a projection cannot safely serve all three read APIs.
					cacheable = false
				}
			}
		}
	}
	// A 2k-entry ordered Object retains about 192 KiB. Build it only for a sustained primitive-map
	// read pattern and cap its width. Nested shared types are deliberately excluded: their ToJSON
	// result is newly materialised and may be mutated by a caller, so a shallow cached clone would
	// let that nested mutation poison later reads. The JSON and Entries projections are mutually
	// exclusive; together with Keys this bounds retained map read state to roughly one projection.
	if cacheable && y.doc != nil && y.doc.readCacheEnabled && m.Len() <= maxYMapCachedEntries &&
		y.jsonReads.Add(1) >= yMapEntriesCacheThreshold {
		y.keysCache.Store(nil)
		y.keysPrimed.Store(false)
		y.entriesCache.Store(nil)
		y.entriesReads.Store(0)
		y.jsonCache.Store(&yMapJSONCache{value: m.ShallowClone()})
	}
	return m
}

// GetSize returns the size of the YMap (count of key/value pairs).
func (y *YMap) GetSize() Number {
	if y.doc == nil {
		return len(y.prelimContent)
	}
	return y.size
}

// Keys returns the keys for each element in the YMap Type.
func (y *YMap) Keys() []string {
	if cached := y.jsonCache.Load(); cached != nil {
		return objectKeysClone(cached.value)
	}
	if cached := y.keysCache.Load(); cached != nil {
		return slices.Clone(cached.keys)
	}
	capacity := len(y.typeMap)
	if y.doc != nil {
		capacity = y.size
	}
	keys := make([]string, 0, capacity)
	for key, item := range y.typeMap {
		if !item.isDeleted() {
			keys = append(keys, key)
		}
	}
	if y.doc != nil && y.doc.readCacheEnabled && len(keys) <= maxYMapCachedKeys && y.keysPrimed.Swap(true) {
		y.keysCache.Store(&yMapKeysCache{keys: slices.Clone(keys)})
	}
	return keys
}

// AppendKeys appends the keys for each element in the YMap to dst and returns
// the extended slice. The caller owns the returned slice and may safely reuse or
// mutate it; cached key storage is never exposed. Reusing the returned slice as
// dst[:0] avoids the allocation that Keys must perform for its ownership guarantee;
// passing nil or a newly allocated destination does not.
func (y *YMap) AppendKeys(dst []string) []string {
	if cached := y.jsonCache.Load(); cached != nil {
		return appendObjectKeys(dst, cached.value)
	}
	if cached := y.keysCache.Load(); cached != nil {
		return append(dst, cached.keys...)
	}
	start := len(dst)
	capacity := len(y.typeMap)
	if y.doc != nil {
		capacity = y.size
	}
	dst = slices.Grow(dst, capacity)
	for key, item := range y.typeMap {
		if !item.isDeleted() {
			dst = append(dst, key)
		}
	}
	if y.doc != nil && y.doc.readCacheEnabled && len(dst)-start <= maxYMapCachedKeys && y.keysPrimed.Swap(true) {
		y.keysCache.Store(&yMapKeysCache{keys: slices.Clone(dst[start:])})
	}
	return dst
}

// Values returns the values for each element in the YMap Type.
func (y *YMap) Values() []interface{} {
	values := make([]interface{}, 0, len(y.typeMap))
	for _, item := range y.typeMap {
		if !item.isDeleted() {
			values = append(values, mapItemValue(item))
		}
	}
	return values
}

// Entries returns an iterator of [key, value] pairs.
func (y *YMap) Entries() map[string]interface{} {
	if cached := y.jsonCache.Load(); cached != nil {
		return objectMapClone(cached.value)
	}
	if cached := y.entriesCache.Load(); cached != nil {
		return maps.Clone(cached.entries)
	}
	m := make(map[string]interface{}, len(y.typeMap))
	for key, item := range y.typeMap {
		if !item.isDeleted() {
			m[key] = mapItemValue(item)
		}
	}
	// A 2k-entry cached projection retains about 160 KiB. Require sustained use
	// before paying that memory and cap the cache entirely for wider maps. Once
	// established, runtime's bulk map clone is about three times faster than
	// re-hashing every CRDT entry for each caller-owned result.
	if y.doc != nil && y.doc.readCacheEnabled && len(m) <= maxYMapCachedEntries &&
		y.entriesReads.Add(1) >= yMapEntriesCacheThreshold {
		y.jsonCache.Store(nil)
		y.jsonReads.Store(0)
		y.entriesCache.Store(&yMapEntriesCache{entries: maps.Clone(m)})
	}
	return m
}

func objectKeysClone(object Object) []string {
	if object.d == nil {
		return nil
	}
	if object.d.large == nil {
		keys := make([]string, object.d.smallLen)
		copy(keys, object.d.smallKeys[:object.d.smallLen])
		return keys
	}
	return slices.Clone(object.d.large.keys)
}

func appendObjectKeys(dst []string, object Object) []string {
	if object.d == nil {
		return dst
	}
	if object.d.large == nil {
		return append(dst, object.d.smallKeys[:object.d.smallLen]...)
	}
	return append(dst, object.d.large.keys...)
}

func objectMapClone(object Object) map[string]interface{} {
	if object.d == nil {
		return map[string]interface{}{}
	}
	if object.d.large == nil {
		entries := make(map[string]interface{}, object.d.smallLen)
		for i := 0; i < int(object.d.smallLen); i++ {
			entries[object.d.smallKeys[i]] = object.d.smallValues[i]
		}
		return entries
	}
	return maps.Clone(object.d.large.m)
}

// ForEach executes a provided function once on every key-value pair.
func (y *YMap) ForEach(f func(string, interface{}, *YMap)) Object {
	m := newObject()
	for key, item := range y.typeMap {
		if !item.isDeleted() {
			f(key, mapItemValue(item), y)
		}
	}
	return m
}

func (y *YMap) Range(f func(key string, val interface{})) {
	entries := y.Entries()
	for key, value := range entries {
		f(key, value)
	}
}

// Delete removes a specified element from this YMap.
func (y *YMap) Delete(key string) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			typeMapDelete(trans, y, key)
		})
	} else {
		delete(y.prelimContent, key)
	}
}

// Set adds or updates an element with a specified key and value.
func (y *YMap) Set(key string, value interface{}) interface{} {
	if y.doc != nil {
		trans, initialCall := beginTransact(y.doc, nil, true, true)
		if !y.setFreshPrimitive(trans, key, value) {
			_ = typeMapSet(trans, y, key, value)
		}
		finishTransact(y.doc, initialCall)
	} else {
		y.prelimContent[key] = value
	}

	return value
}

func (y *YMap) setFreshPrimitive(trans *Transaction, key string, value interface{}) bool {
	if y.typeMap[key] != nil || value != nil && !isAnyEncodable(value) {
		return false
	}
	y.setFreshPrimitiveKnown(trans, key, value)
	return true
}

func (y *YMap) setFreshPrimitiveKnown(trans *Transaction, key string, value interface{}) {
	doc := trans.doc
	clientStructs, _ := doc.store.clientStructs(doc.ClientID)
	clock := Number(0)
	if clientStructs != nil {
		lastStruct := clientStructs.lastValue()
		clock = lastStruct.getID().Clock + lastStruct.structLength()
	}
	storage := doc.allocateMapItemStorage()
	storage.value[0] = value
	storage.content.arr = storage.value[:]
	storage.item = itemStruct{
		abstractStructBase: abstractStructBase{id: GenID(doc.ClientID, clock), length: 1},
		parent:             y,
		parentSub:          key,
		content:            &storage.content,
		info:               bit2,
	}

	y.typeMap[key] = &storage.item
	y.size++
	if clientStructs == nil {
		doc.store.appendClientStruct(doc.ClientID, &storage.item)
	} else {
		clientStructs.appendValue(&storage.item)
	}
	addChangedYMapToTransaction(trans, y, key)
	if y.item != nil && y.item.isDeleted() {
		storage.item.deleteItemStruct(trans)
	}
}

// Get returns a specified element from this YMap.
func (y *YMap) Get(key string) interface{} {
	return typeMapGet(y, key)
}

func (y *YMap) Has(key string) bool {
	return typeMapHas(y, key)
}

func (y *YMap) canBulkClearPrimitive() bool {
	for _, item := range y.typeMap {
		if item.isDeleted() {
			continue
		}
		if _, ok := item.content.(*contentAny); !ok {
			return false
		}
	}
	return true
}

func (y *YMap) bulkClearPrimitive(trans *Transaction) {
	invalidateYMapReadCache(y)
	trans.doc.store.markPrimitiveMapDeleted(trans, y)
	y.size = 0
}

// Clear removes all elements from this YMap.
func (y *YMap) Clear() {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			// With no observer/undo/GC-visible transaction state and primitive
			// values, delete in struct-store clock order. This emits contiguous
			// delete ranges directly instead of allocating and then sorting one
			// record per key. ContentAny.Delete is deliberately a no-op.
			if trans.compactState && trans.changedTypes == nil && !hasTypeObservers(y) && y.canBulkClearPrimitive() {
				y.bulkClearPrimitive(trans)
				return
			}
			trans.reserveDeleteItems(y.size)
			trans.reserveDeleteSetClient(y.doc.ClientID, y.size)
			for _, item := range y.typeMap {
				if !item.isDeleted() {
					item.deleteItemStruct(trans)
				}
			}
		})
	} else {
		y.prelimContent = make(map[string]interface{})
	}
}

func (y *YMap) writeType(encoder updateEncoder) {
	encoder.writeTypeRef(yMapRefID)
}

func NewYMap(entries map[string]interface{}) *YMap {
	ymap := &YMap{
		abstractTypeBase: abstractTypeBase{
			typeMap: make(map[string]*itemStruct),
		},
	}

	if entries == nil {
		ymap.prelimContent = make(map[string]interface{})
	} else {
		ymap.prelimContent = entries
	}

	return ymap
}

func newYMapType() SharedType {
	return NewYMap(nil)
}

func mapItemValue(item *itemStruct) interface{} {
	if content, ok := item.content.(*contentAny); ok {
		return content.arr[len(content.arr)-1]
	}
	content := item.content.contentValues()
	return content[len(content)-1]
}
