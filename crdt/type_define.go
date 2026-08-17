package crdt

import (
	"bytes"
	"fmt"
	"maps"
	"reflect"
	"slices"
)

var (
	Null      = NullType{}
	Undefined = UndefinedType{}
)

// js Number
type Number = int

// Object is the Go analogue of a JS plain object: a string->any map that
// PRESERVES key insertion order.
//
// Order matters for byte-parity with lib0/Yjs. lib0's writeAny emits object keys
// in JS insertion order (Object.keys), and JSON.stringify likewise serializes
// keys in insertion order. A Go map[string]any randomizes iteration order and
// json.Marshal sorts keys, so encoding a multi-key object through either would
// diverge from the JS byte stream (Y.Map / Y.Array plain-object values, multi-key
// format attributes, ContentJson content, ContentDoc.Opts). Object therefore
// carries an explicit ordered key slice alongside the value map; every encoder
// (WriteObject/WriteAny, the order-preserving JSON emitter) and decoder
// (ReadObject/ReadAny, the order-preserving JSON parser) walks keys in this order,
// and decoding a JS-produced update then re-encoding it reproduces identical
// bytes.
//
// Object is a thin handle around a shared *objectData pointer, so copying an
// Object value (assignment, passing it to a function, storing it in a map) shares
// the same backing data — RESTORING the reference semantics the previous
// `type Object = map[string]any` alias had. Mutations via Set/Delete are visible
// through every copy, exactly as map mutations were. The zero Object has a nil
// handle (IsNil); newObject() allocates one.
type Object struct {
	d *objectData
}

// objectData is the shared backing store of an Object: the value map plus the
// insertion-order key slice.
type objectData struct {
	smallKeys   [2]string
	smallValues [2]any
	smallLen    uint8
	large       *objectLargeData
}

type objectLargeData struct {
	keys []string
	m    map[string]any
}

// Set inserts or updates key. A new key is appended to the order; updating an
// existing key keeps its original position (matching JS object semantics, where
// re-assigning a property does not move it). Calling Set on a zero Object lazily
// allocates its backing store, so the zero value behaves like an empty object the
// moment it is written.
func (o *Object) Set(key string, value any) {
	if o.d == nil {
		o.d = &objectData{}
	}
	if o.d.large == nil {
		for i := 0; i < int(o.d.smallLen); i++ {
			if o.d.smallKeys[i] == key {
				o.d.smallValues[i] = value
				return
			}
		}
		if o.d.smallLen < uint8(len(o.d.smallKeys)) {
			i := o.d.smallLen
			o.d.smallKeys[i] = key
			o.d.smallValues[i] = value
			o.d.smallLen++
			return
		}
		// Promote in insertion order. Re-setting an inline key above deliberately did not move it.
		large := &objectLargeData{
			keys: make([]string, 0, int(o.d.smallLen)+1),
			m:    make(map[string]any, int(o.d.smallLen)+1),
		}
		for i := 0; i < int(o.d.smallLen); i++ {
			k := o.d.smallKeys[i]
			large.keys = append(large.keys, k)
			large.m[k] = o.d.smallValues[i]
			o.d.smallKeys[i] = ""
			o.d.smallValues[i] = nil
		}
		large.keys = append(large.keys, key)
		large.m[key] = value
		o.d.smallLen = 0
		o.d.large = large
		return
	}
	if _, exist := o.d.large.m[key]; !exist {
		o.d.large.keys = append(o.d.large.keys, key)
	}
	o.d.large.m[key] = value
}

// appendNew inserts a key that the caller knows is absent. It preserves the
// same insertion order as Set while avoiding an existence lookup when building
// a fresh Object from a source whose keys are unique (for example, a Go map).
// Keep this package-private: using it with a duplicate key would violate the
// Object invariant by appending the key twice.
func (o *Object) appendNew(key string, value any) {
	if o.d == nil {
		o.d = &objectData{}
	}
	if o.d.large == nil {
		if o.d.smallLen < uint8(len(o.d.smallKeys)) {
			i := o.d.smallLen
			o.d.smallKeys[i] = key
			o.d.smallValues[i] = value
			o.d.smallLen++
			return
		}
		// This path is only expected when a caller starts from newObject rather
		// than newObjectWithCapacity. Let Set perform the ordered promotion.
		o.Set(key, value)
		return
	}
	o.d.large.keys = append(o.d.large.keys, key)
	o.d.large.m[key] = value
}

// Get returns the value for key and whether it is present.
func (o Object) Get(key string) (any, bool) {
	if o.d == nil {
		return nil, false
	}
	if o.d.large == nil {
		for i := 0; i < int(o.d.smallLen); i++ {
			if o.d.smallKeys[i] == key {
				return o.d.smallValues[i], true
			}
		}
		return nil, false
	}
	v, ok := o.d.large.m[key]
	return v, ok
}

// GetOr returns the value for key, or nil if absent (the ergonomic accessor for
// the common `obj[key]` read where a missing key should read as nil/zero).
func (o Object) GetOr(key string) any {
	if o.d == nil {
		return nil
	}
	if o.d.large == nil {
		for i := 0; i < int(o.d.smallLen); i++ {
			if o.d.smallKeys[i] == key {
				return o.d.smallValues[i]
			}
		}
		return nil
	}
	return o.d.large.m[key]
}

// GetOrNull returns the value for key, coalesced to the Null sentinel when the key is absent
// (or holds Go nil) — matching yjs `o.get(key) ?? null`. This is the canonical accessor for
// attribute/format reads, where a missing key must compare equal to an explicit JS null (so
// equalAttrs works); distinct from GetOr, which returns Go nil. (Consolidates the former
// y_text.go attrOrNull helper — the single `?? null` accessor, FR-009/SC-004.)
func (o Object) GetOrNull(key string) any {
	if v, ok := o.Get(key); ok && v != nil {
		// JS `?? null` coalesces both `null` AND `undefined`. A missing key reads as
		// Go nil (handled above); an explicit UndefinedType (yjs `get` -> undefined)
		// must also collapse to Null rather than leak through.
		if _, isUndefined := v.(UndefinedType); !isUndefined {
			return v
		}
	}
	return Null
}

// Has reports whether key is present.
func (o Object) Has(key string) bool {
	if o.d == nil {
		return false
	}
	if o.d.large == nil {
		for i := 0; i < int(o.d.smallLen); i++ {
			if o.d.smallKeys[i] == key {
				return true
			}
		}
		return false
	}
	_, ok := o.d.large.m[key]
	return ok
}

// Delete removes key (and its position in the order) if present.
func (o Object) Delete(key string) {
	if o.d == nil {
		return
	}
	if o.d.large == nil {
		for i := 0; i < int(o.d.smallLen); i++ {
			if o.d.smallKeys[i] == key {
				last := int(o.d.smallLen) - 1
				copy(o.d.smallKeys[i:last], o.d.smallKeys[i+1:last+1])
				copy(o.d.smallValues[i:last], o.d.smallValues[i+1:last+1])
				o.d.smallKeys[last] = ""
				o.d.smallValues[last] = nil
				o.d.smallLen--
				return
			}
		}
		return
	}
	if _, ok := o.d.large.m[key]; !ok {
		return
	}
	delete(o.d.large.m, key)
	for i, k := range o.d.large.keys {
		if k == key {
			o.d.large.keys = append(o.d.large.keys[:i], o.d.large.keys[i+1:]...)
			break
		}
	}
}

// Len returns the number of keys.
func (o Object) Len() int {
	if o.d == nil {
		return 0
	}
	if o.d.large == nil {
		return int(o.d.smallLen)
	}
	return len(o.d.large.m)
}

// IsNil reports whether the Object is the uninitialized zero value (its backing
// handle is nil), as opposed to an explicitly-constructed empty object
// (newObject(), whose handle is non-nil). It is the sentinel the awareness layer
// uses to tell an ABSENT / cleared (JS null) state apart from a present-but-empty
// {} state — a distinction a `== nil` comparison gave for free when Object was a
// map alias.
func (o Object) IsNil() bool {
	return o.d == nil
}

// sameRef reports whether o and other share the same backing store — the Go
// analogue of JS reference identity (===) for objects. Two independently
// constructed/decoded Objects with equal contents are NOT sameRef, matching
// `{} === {}` being false in JS. Used by attribute equality (equalAttrs), which
// must compare nested object values by reference, not structurally. Two zero
// (IsNil) Objects share a nil handle and so count as the same reference.
//
// THIS BLOCKS THE OBVIOUS COPY-ON-WRITE. The cached read projections spend
// essentially all their time copying to hand the caller its own storage
// (perf_bench_ops_test.go: ToJson 46,701 ns against a 2.140 ns floor), and the
// natural fix is for ShallowClone to share storage and copy only on write. Done
// naively that makes a clone share o.d with its source, so sameRef flips to true
// and attrStrictEqual starts calling two JS-distinct objects ===. Measured, not
// assumed: relaxing sameRef to structural equality fails five tests, including
// TestEqualAttrsMatchesYjsEqualFlat and TestFormatAttributeObjectValueFollowsYjsVerdict,
// both of which pin Y.Text formatting against yjs. So the naive version is a
// wire-visible parity break, not a refactor.
//
// A working shape has to separate IDENTITY from STORAGE: a per-handle header
// (Object holding *objectHandle, the handle holding *objectData) lets sameRef
// compare handles while the data is shared copy-on-write. ShallowClone then
// allocates one small handle instead of copying every key and the whole map, and
// a mutation through any handle installs a private copy before writing. It also
// happens to fix Delete, which today mutates shared state through a value
// receiver and could not otherwise install a copy the caller would see.
func (o Object) sameRef(other Object) bool {
	return o.d == other.d
}

// Keys returns the keys in insertion order. The returned slice is a copy, safe to
// mutate without affecting the Object.
func (o Object) Keys() []string {
	if o.d == nil {
		return nil
	}
	if o.d.large == nil {
		if o.d.smallLen == 0 {
			return []string{}
		}
		out := make([]string, o.d.smallLen)
		copy(out, o.d.smallKeys[:o.d.smallLen])
		return out
	}
	out := make([]string, len(o.d.large.keys))
	copy(out, o.d.large.keys)
	return out
}

// Range calls f for each key/value pair in insertion order.
func (o Object) Range(f func(key string, value any)) {
	if o.d == nil {
		return
	}
	if o.d.large == nil {
		for i := 0; i < int(o.d.smallLen); i++ {
			f(o.d.smallKeys[i], o.d.smallValues[i])
		}
		return
	}
	for _, k := range o.d.large.keys {
		f(k, o.d.large.m[k])
	}
}

// ToMap returns an unordered map[string]any copy of the contents. For callers
// (state assertions, JSON-as-map interop) that do not care about order.
func (o Object) ToMap() map[string]any {
	if o.d == nil {
		return map[string]any{}
	}
	if o.d.large == nil {
		out := make(map[string]any, o.d.smallLen)
		for i := 0; i < int(o.d.smallLen); i++ {
			out[o.d.smallKeys[i]] = o.d.smallValues[i]
		}
		return out
	}
	out := make(map[string]any, len(o.d.large.m))
	for _, k := range o.d.large.keys {
		out[k] = o.d.large.m[k]
	}
	return out
}

// MarshalJSON makes Object a first-class encoding/json value that serializes its
// keys in INSERTION order (matching JS JSON.stringify), instead of the empty `{}`
// stdlib json would emit for a struct with only unexported fields. This means
// json.Marshal(anObject) — and json.Marshal of any value transitively containing
// Objects — produces byte-identical output to JSON.stringify for our value domain.
// (marshalJSONOrdered delegates here; this method is what makes plain json.Marshal
// correct too, e.g. ToJson() round-trips.)
func (o Object) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	if err := marshalOrderedObject(&buf, o); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalJSON decodes a JSON object into this Object, preserving the on-wire key
// order. A JSON `null` leaves the Object as its zero value (IsNil); a non-object
// JSON value is an error (Object only represents JS plain objects).
func (o *Object) UnmarshalJSON(data []byte) error {
	v, err := unmarshalJSONOrdered(data)
	if err != nil {
		return err
	}
	// JSON null (decoded as lib0 Null) or a bare nil leaves the Object as its zero
	// value (IsNil).
	if v == nil {
		*o = Object{}
		return nil
	}
	if _, isNull := v.(NullType); isNull {
		*o = Object{}
		return nil
	}
	parsed, ok := v.(Object)
	if !ok {
		return fmt.Errorf("cannot unmarshal %T into Object", v)
	}
	*o = parsed
	return nil
}

// ShallowClone returns a copy of the Object with its OWN top-level backing store
// (a fresh key slice + value map) but SHARING every nested value by reference —
// the Go analogue of JS object.assign({}, o) / lib0 map.copy. Top-level Set/Delete
// on the clone do not affect the original (and vice-versa), but a nested Object /
// []any value is the SAME reference in both.
//
// This is what the Y.Text formatting cleanup needs (cleanupFormattingGap's
// startAttributes / deleteText's endAttributes): Yjs builds those via a shallow
// object.assign, so a nested-object-valued format attribute compares == (same
// reference) against the active attribute and a redundant ContentFormat marker is
// correctly dropped. A deep Clone() would give the nested value a fresh handle, so
// the reference-strict equalAttrs reported it unequal and Go kept a redundant
// marker — diverging the item chain / ToDelta from JS. Use ShallowClone there;
// use Clone() only where an independent deep copy is genuinely required.
func (o Object) ShallowClone() Object {
	if o.d == nil {
		return Object{}
	}
	if o.d.large == nil {
		return Object{d: &objectData{
			smallKeys:   o.d.smallKeys,
			smallValues: o.d.smallValues,
			smallLen:    o.d.smallLen,
		}}
	}
	// Bulk copy (Yjs object.assign / lib0 map.copy): a fresh keys slice + a fresh
	// map with the value references SHARED — no per-key existence probe and no
	// incremental keys-slice regrowth.
	return Object{d: &objectData{large: &objectLargeData{
		keys: slices.Clone(o.d.large.keys),
		m:    maps.Clone(o.d.large.m),
	}}}
}

// Deep copying an Object lives in data_clone.go (cloneDataObject), which already
// walks this same value domain without reflection and REJECTS values it does not
// recognize. Object.Clone used to be a second, reflection-based deep copy built
// on copystructure, and it was the library's only remaining module dependency.
//
// Two implementations of one operation is the smaller problem. The larger one is
// that they disagreed: copystructure could not traverse Object's unexported
// fields at all (reflectwalk skips them), so a nested Object needed a bespoke
// copier registered in init() just to avoid silently cloning to an EMPTY object;
// and on an unsupported value it fell back to sharing the original, handing the
// caller something that looks owned but is not. cloneDataObject returns an error
// there instead, which is the contract that ownership boundaries actually need.
//
// So Object.Clone is gone rather than reimplemented, and cloneDataObject is the
// single deep copy. Measured on the same five-key attribute object, dropping
// reflection took it from 9,981 ns / 8,272 B / 237 allocs to 576 ns / 1,128 B /
// 14 allocs. 237 allocations to copy five keys is what reflection cost here.

// MakeObject builds an Object from an even-length (key, value, key, value, ...)
// argument list, preserving the given order. It is the ordered replacement for
// the old `Object{"k": v, ...}` composite literal. Panics on an odd argument
// count or a non-string key, both of which are programmer errors.
func MakeObject(kv ...any) Object {
	if len(kv)%2 != 0 {
		panic("MakeObject: odd number of arguments")
	}
	o := newObject()
	for i := 0; i < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			panic("MakeObject: key is not a string")
		}
		o.Set(key, kv[i+1])
	}
	return o
}

// js Array<any>
type ArrayAny = []any

// js undefined
type UndefinedType struct {
}

// js null
type NullType struct {
}

// js Set<any>
type Set map[any]bool

// Add adds the given element to the set.
func (s Set) Add(e any) {
	s[e] = true
}

// Has returns true if the given element is in the set.
func (s Set) Has(e any) bool {
	_, exist := s[e]
	return exist
}

// Delete deletes the given element from the set.
func (s Set) Delete(e any) {
	delete(s, e)
}

// Range calls the given function for each element in the set.
func (s Set) Range(f func(element any)) {
	for el := range s {
		f(el)
	}
}

// ChangedSubs is the set of parent-sub keys changed on one shared type during a
// transaction. ParentSub is always a string in this implementation; list changes
// use the empty string where Yjs uses null. Keeping that fact in the type avoids
// boxing every changed map key into an interface.
type ChangedSubs map[string]struct{}

// newChangedSubs constructs an empty changed-parent-sub set.
func newChangedSubs() ChangedSubs { return make(ChangedSubs) }

// Add records key as changed.
func (s ChangedSubs) Add(key string) { s[key] = struct{}{} }

// Has reports whether key was changed.
func (s ChangedSubs) Has(key string) bool {
	_, ok := s[key]
	return ok
}

// Delete removes key from the changed set.
func (s ChangedSubs) Delete(key string) { delete(s, key) }

// Range calls f once for every changed key.
func (s ChangedSubs) Range(f func(string)) {
	for key := range s {
		f(key)
	}
}

// js numberSlice
type numberSlice []Number

// Len returns the length of the slice.
func (ns numberSlice) Len() int {
	return len(ns)
}

// Less returns true if the element at index i is less than the element at index j.
func (ns numberSlice) Less(i, j int) bool {
	return ns[i] < ns[j]
}

// Swap swaps the elements at index i and j.
func (ns numberSlice) Swap(i, j int) {
	ns[i], ns[j] = ns[j], ns[i]
}

// Filter returns a new slice containing all elements for which the given function returns true.
func (ns numberSlice) Filter(cond func(number Number) bool) numberSlice {
	var r numberSlice
	for _, n := range ns {
		if cond(n) {
			r = append(r, n)
		}
	}

	return r
}

// newObject returns a new, empty insertion-ordered Object with its own backing
// store (so it is non-nil per IsNil).
func newObject() Object {
	return Object{d: &objectData{}}
}

func newObjectWithCapacity(capacity int) Object {
	if capacity <= len((objectData{}).smallKeys) {
		return newObject()
	}
	return Object{d: &objectData{large: &objectLargeData{
		keys: make([]string, 0, capacity),
		m:    make(map[string]any, capacity),
	}}}
}

// NewSet returns a new set.
func NewSet() Set {
	return make(Set)
}

// isUndefined returns true if the given object is undefined.
// In javascript, undefined indicate that the variable has not been initialized.
// In golang, a nil any(=interface{}) value indicates that the variable has not been initialized.
// So, we define an object is undefined if its value is nil or its type is Undefined.
func isUndefined(obj any) bool {
	return obj == nil || reflect.TypeOf(obj) == reflect.TypeOf(Undefined)
}

// isNull returns true if the given object is null.
// In javascript, null indicate that the variable has been initialized and the value is null.
// In golang, we define an object is null if the object is a pointer kind and the value is nil or its type is Null.
func isNull(obj any) bool {
	if _, ok := obj.(NullType); ok {
		return true
	}
	v := reflect.ValueOf(obj)
	return v.IsValid() && v.Kind() == reflect.Pointer && v.IsNil()
}

// isGCPtr returns true if the given object is a pointer to a GC.
func isGCPtr(obj interface{}) bool {
	return reflect.TypeOf(obj) == reflect.TypeOf(&gcStruct{})
}

// isItemPtr returns true if the given object is a pointer to an Item.
func isItemPtr(obj interface{}) bool {
	return reflect.TypeOf(obj) == reflect.TypeOf(&itemStruct{})
}

// isIDPtr returns true if the given object is a pointer to an ID.
func isIDPtr(obj interface{}) bool {
	return reflect.TypeOf(obj) == reflect.TypeOf(&ID{})
}

// isSameType returns true if the given two objects are the same type.
func isSameType(a interface{}, b interface{}) bool {
	return reflect.TypeOf(a) == reflect.TypeOf(b)
}

// isYString returns true if the given object is a yString.
func isYString(obj interface{}) bool {
	return reflect.TypeOf(obj) == reflect.TypeOf(&yString{})
}

// isAbstractType returns true if the given object is an IAbstractType.
func isAbstractType(a interface{}) bool {
	if a == nil {
		return false
	}

	_, ok := a.(abstractType)
	return ok
}
