package crdt

import (
	"errors"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

var defaultGCFilter = func(item *itemStruct) bool {
	return true
}

// spliceStruct inserts elements into a slice at the specified start index,
// deleting deleteCount elements if deleteCount is greater than 0.
// It returns the deleted elements if deleteCount is greater than 0, otherwise it returns nil.
func spliceStruct(ss *[]abstractStruct, start Number, deleteCount Number, elements []abstractStruct) {
	// check if the capacity is enough to store the elements, if the deleted elements are greater than or equal to the elements to be inserted,
	// then the elements to be deleted can be directly overwritten, and the remaining elements can be moved forward to the position of the elements to be deleted.
	if deleteCount >= len(elements) {
		// overwrite the deleted elements with the new ones (write into *ss's backing array)
		copy((*ss)[start:], elements)

		// move the remaining elements forward to position of the elements to be deleted.
		first := start + len(elements)
		second := start + deleteCount
		newLength := len(*ss) - deleteCount + len(elements)
		copy((*ss)[first:], (*ss)[second:])
		clear((*ss)[newLength:])
		*ss = (*ss)[:newLength]
		return
	}

	// if the capacity is not enough to store the elements, then the elements need to be copied
	// and the capacity is expanded to the sum of the original capacity and the length of the elements to be inserted.
	if cap(*ss) < (len(*ss) + len(elements) - deleteCount) {
		spliceStructInner(ss, start, deleteCount, elements)
		return
	}

	// the capacity is enough to store the elements, then the elements need to be moved forward to the position of the elements to be deleted,
	// and then the elements to be inserted are appended to the slice.
	originLength := len(*ss)
	(*ss) = (*ss)[:len(*ss)+len(elements)-deleteCount]

	offset := len(elements) - deleteCount
	copy((*ss)[start+offset:], (*ss)[start:originLength])

	copy((*ss)[start:], elements)
}

// spliceStructInner copies the elements to be inserted into a new slice,
// and then appends the new slice to the original slice.
// It returns the deleted elements if deleteCount is greater than 0, otherwise it returns nil.
// The capacity of the original slice is not expanded.
func spliceStructInner(ss *[]abstractStruct, start Number, deleteCount Number, elements []abstractStruct) {
	newLength := len(*ss) + len(elements) - deleteCount
	newCapacity := cap(*ss) * 2
	if newCapacity < newLength {
		newCapacity = newLength
	}
	if newCapacity == 0 {
		newCapacity = 1
	}
	// Struct splitting inserts repeatedly into the same client's store. Grow geometrically like
	// append instead of allocating an exact-length replacement on every split; exact growth made
	// random deletes copy an O(n)-sized interface slice into a new allocation for every operation.
	combine := make([]abstractStruct, 0, newCapacity)
	combine = append(combine, (*ss)[:start]...)
	combine = append(combine, elements...)
	combine = append(combine, (*ss)[start+deleteCount:]...)
	*ss = combine
}

// spliceArray inserts elements into a slice at the specified start index,
// deleting deleteCount elements if deleteCount is greater than 0.
// It returns the deleted elements if deleteCount is greater than 0, otherwise it returns nil.
func spliceArray(a *ArrayAny, start Number, deleteCount Number, elements ArrayAny) ArrayAny {
	// check if the capacity is enough to store the elements, if the deleted elements are greater than or equal to the elements to be inserted,
	// then the elements to be deleted can be directly overwritten, and the remaining elements can be moved forward to the position of the elements to be deleted.
	if deleteCount >= len(elements) {
		// overwrite the deleted elements with the new ones (write into *a's backing array)
		copy((*a)[start:], elements)

		// move the remaining elements forward to position of the elements to be deleted.
		first := start + len(elements)
		second := start + deleteCount
		for i := second; i < len(*a); i++ {
			(*a)[first] = (*a)[second]
			first++
			second++
		}

		*a = (*a)[:first]
		return nil
	}

	// the capacity is not enough to store the elements, then the elements need to be copied
	// and the capacity is expanded to the sum of the original capacity and the length of the elements to be inserted.
	if cap(*a) < (len(*a) + len(elements) - deleteCount) {
		return spliceArrayInner(a, start, deleteCount, elements)
	}

	// the capacity is enough to store the elements, then the elements need to be moved forward to the position of the elements to be deleted,
	// and then the elements to be inserted are appended to the slice.
	originLength := len(*a)
	(*a) = (*a)[:len(*a)+len(elements)-deleteCount]

	offset := len(elements) - deleteCount
	for i := originLength - 1; i >= start; i-- {
		(*a)[i+offset] = (*a)[i]
	}

	copy((*a)[start:], elements)

	return nil
}

// spliceArrayInner copies the elements to be inserted into a new slice,
// and then appends the new slice to the original slice.
func spliceArrayInner(a *ArrayAny, start Number, deleteCount Number, elements ArrayAny) ArrayAny {
	var deleteElements ArrayAny
	if deleteCount > 0 {
		deleteElements = (*a)[start : start+deleteCount]
	}

	combine := make(ArrayAny, 0, len(*a)+len(elements)-deleteCount)
	combine = append(combine, (*a)[:start]...)
	combine = append(combine, elements...)
	combine = append(combine, (*a)[start+deleteCount:]...)
	*a = combine

	return deleteElements
}

// charCodeAt returns the Unicode code point of the character at the specified index in the given string.
// The index is the position of the character in the string, starting from 0.
// If the index is out of range, it returns an error.
func charCodeAt(str string, pos Number) (uint16, error) {
	if pos >= 0 {
		units := 0
		for _, r := range str {
			if r < 0x10000 {
				if units == pos {
					return uint16(r), nil
				}
				units++
				continue
			}

			// Supplementary runes occupy a UTF-16 surrogate pair. Derive the
			// two code units directly rather than encoding the whole string.
			r -= 0x10000
			if units == pos {
				return uint16(0xD800 + (r >> 10)), nil
			}
			if units+1 == pos {
				return uint16(0xDC00 + (r & 0x3FF)), nil
			}
			units += 2
		}
	}
	return 0, errors.New("index out of range")
}

// stringHeader returns the substring of the given string from the beginning to the offset.
func stringHeader(str string, offset Number) string {
	header, _ := splitStringUTF16(str, offset)
	return header
}

// stringTail returns the substring of the given string from the offset to the end.
func stringTail(str string, offset Number) string {
	_, tail := splitStringUTF16(str, offset)
	return tail
}

// splitStringUTF16 splits str at a JavaScript/Yjs UTF-16 code-unit offset.
// At an ordinary rune boundary the returned strings alias str's backing bytes;
// the only allocation is the parity-required U+FFFD replacement when offset
// bisects a supplementary rune (a two-unit surrogate pair in UTF-16).
func splitStringUTF16(str string, offset Number) (string, string) {
	left, right, _ := splitStringUTF16AtBoundary(str, offset)
	return left, right
}

// splitStringUTF16AtBoundary additionally reports whether both results still
// alias str. The only false case is a split inside a supplementary rune, where
// UTF-16 parity requires replacement strings on both sides.
func splitStringUTF16AtBoundary(str string, offset Number) (string, string, bool) {
	if offset < 0 {
		panic("offset out of range")
	}
	if offset == 0 {
		return "", str, true
	}

	units := 0
	for byteOffset, r := range str {
		if units == offset {
			return str[:byteOffset], str[byteOffset:], true
		}

		width := 1
		if r >= 0x10000 {
			width = 2
		}
		if units+width > offset {
			// The split bisects a surrogate pair. Go strings cannot represent a
			// lone surrogate, so match Yjs's encoding behaviour by replacing the
			// straddled rune with U+FFFD in both halves.
			_, runeBytes := utf8.DecodeRuneInString(str[byteOffset:])
			return str[:byteOffset] + "�", "�" + str[byteOffset+runeBytes:], false
		}
		units += width
	}

	// offset at or beyond the UTF-16 length leaves the whole string on the left.
	return str, "", true
}

// stringLength returns the length of the given string in utf16 code points.
func stringLength(str string) Number {
	n := 0
	for _, r := range str {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// mergeString merges two strings.
func mergeString(str1, str2 string) string {
	// if the length of the two strings is less than or equal to 10000,
	// the fastest way is to use + to merge two strings.
	if len(str1)+len(str2) <= 10000 {
		return str1 + str2
	}

	builder := strings.Builder{}
	builder.Grow(len(str1) + len(str2))
	builder.WriteString(str1)
	builder.WriteString(str2)
	return builder.String()
}

// generateNewClientID generates a new client ID.
func generateNewClientID() Number {
	return Number(rand.Int31())
}

// Check if `parent` is a parent of `child`.
func isParentOf(parent abstractType, child *itemStruct) bool {
	for child != nil {
		if child.parent == parent {
			return true
		}

		child = child.parent.(abstractType).getItem()
	}

	return false
}

// maxNumber returns the maximum value of a and b.
func maxNumber(a, b Number) Number {
	if a > b {
		return a
	}

	return b
}

// minNumber returns the minimum value of a and b.
func minNumber(a, b Number) Number {
	if a < b {
		return a
	}

	return b
}

// arrayLast returns the last element of the given array.
// If the array is empty, it returns an error.
func arrayLast(a ArrayAny) (interface{}, error) {
	if len(a) == 0 {
		return nil, errors.New("empty array")
	}

	return a[len(a)-1], nil
}

// arrayFilter filters the elements of the given array by the given filter function.
// It returns a new array that contains the elements that satisfy the filter function.
func arrayFilter(a []IEventType, filter func(e IEventType) bool) []IEventType {
	var targets []IEventType

	for _, e := range a {
		if filter(e) {
			targets = append(targets, e)
		}
	}

	return targets
}

// mapAny returns true if the given map satisfies the given function.
func mapAny(m map[Number]Number, f func(key, value Number) bool) bool {
	for key, value := range m {
		if f(key, value) {
			return true
		}
	}

	return false
}

// MergeSortedRange merges two sorted ranges of the given map. The isInc parameter determines the order of the ranges.
func mapSortedRange(m map[Number]Number, isInc bool, f func(key, value Number)) {
	var keys numberSlice
	for k := range m {
		keys = append(keys, k)
	}

	if isInc {
		sort.Sort(keys)
	} else {
		sort.Sort(sort.Reverse(keys))
	}

	for _, k := range keys {
		f(k, m[k])
	}
}

// equalAttrs reports attribute equality with the SAME semantics as Yjs's
// equalAttrs (src/types/YText.js):
//
//	const equalAttrs = (a, b) =>
//	  a === b ||
//	  (typeof a === 'object' && typeof b === 'object' && a && b && object.equalFlat(a, b))
//
// and lib0 object.equalFlat is SHALLOW (one level):
//
//	equalFlat = (a, b) => a === b ||
//	  (size(a) === size(b) && every(a, (val, key) =>
//	     (val !== undefined || hasProperty(b, key)) && equalityTrait.equals(b[key], val)))
//
// where equalityTrait.equals is strict === for the JSON value domain (no custom
// EqualityTrait on plain objects/arrays/primitives). So the rules are:
//
//   - a === b first: primitives compare BY VALUE; objects/arrays by reference.
//   - otherwise, if BOTH are non-null objects/arrays: compare them with one
//     SHALLOW pass — same key count (or array length), and each member compared
//     with strict === (value for primitives, REFERENCE for nested objects/arrays).
//
// This matters: a NESTED object/array value is compared by === (reference
// identity), NOT deep-structurally. Two distinct-but-equal nested instances are
// UNEQUAL in Yjs. The previous Go implementation recursed structurally into
// nested Objects and ran nested-objects-inside-slices through reflect.DeepEqual
// (order-sensitive), so it reported such pairs as equal — flipping formatText /
// minimizeAttributeChanges' marker-drop decision relative to Yjs and producing a
// different Y.Text item chain. Match equalFlat exactly instead.
func equalAttrs(a, b interface{}) bool {
	// a === b: value equality for primitives; reference identity for objects/arrays.
	if attrStrictEqual(a, b) {
		return true
	}
	// Both must be non-null objects/arrays for the equalFlat branch. (`a && b` in
	// JS rules out null/undefined; in Go that maps to nil interface and the
	// zero/IsNil Object, which is our absent/null sentinel.)
	switch av := a.(type) {
	case Object:
		bo, ok := b.(Object)
		if !ok || av.IsNil() || bo.IsNil() {
			return false
		}
		return equalFlatObject(av, bo)
	case []any:
		bs, ok := b.([]any)
		if !ok {
			return false
		}
		return equalFlatArray(av, bs)
	default:
		return false
	}
}

// equalFlatObject is object.equalFlat for two non-nil Objects: order-INDEPENDENT,
// same key count, each value strict-=== equal. Mirrors lib0 (size + every).
func equalFlatObject(a, b Object) bool {
	if a.Len() != b.Len() {
		return false
	}
	equal := true
	a.Range(func(key string, av any) {
		if !equal {
			return
		}
		bv, ok := b.Get(key)
		// every(a, (val,key) => (val !== undefined || hasProperty(b,key)) && ...):
		// a present key in `a` (we are ranging a's keys) must also be present in `b`
		// with a strict-=== equal value.
		if !ok || !attrStrictEqual(av, bv) {
			equal = false
		}
	})
	return equal
}

// equalFlatArray is object.equalFlat applied to two arrays (JS `typeof [] ===
// 'object'`, and Object.keys(arr) are the indices): same length, each element
// strict-=== equal. So distinct arrays of equal PRIMITIVES are equal, but
// distinct arrays of nested objects/arrays are NOT (element === is by reference).
func equalFlatArray(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !attrStrictEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

// attrStrictEqual models JS strict === for the attribute value domain:
//
//   - nested Object: REFERENCE identity (same backing store). Two distinct-but-
//     equal Objects are !==, exactly as in JS. NOT deep-structural.
//   - nested []any: reference identity (same slice header). Distinct slices are
//     !==, even with equal elements.
//   - primitives / NullType / UndefinedType / nil and everything else: value
//     equality via reflect.DeepEqual (safe — incomparable composite types are
//     already handled above, so no panic).
func attrStrictEqual(a, b any) bool {
	switch av := a.(type) {
	case Object:
		bo, ok := b.(Object)
		return ok && av.sameRef(bo)
	case []any:
		bs, ok := b.([]any)
		return ok && sameSliceRef(av, bs)
	default:
		// Guard against an Object/[]any on the b side paired with a primitive a:
		// those are different JS types, so !==.
		switch b.(type) {
		case Object, []any:
			return false
		}
		return reflect.DeepEqual(a, b)
	}
}

// sameSliceRef reports whether two []any are the SAME reference, the Go analogue
// of JS array reference identity (===). nil/nil counts as the same (JS
// null === null). Two DISTINCT non-nil empty slices are NOT the same reference —
// JS `[] === []` is false — so they compare unequal (Correctness 5): returning
// true here dropped an empty-array-valued ContentFormat marker that Yjs keeps,
// diverging the item chain / ToDelta cross-impl. The len==0 case cannot use
// &a[0] (no addressable element), so it is decided structurally: only nil/nil is
// "same", any non-nil empty is not. This is deliberately conservative even for a
// self-comparison sameSliceRef(x, x): Go cannot tell a shared empty slice from two
// distinct ones (both reference runtime.zerobase, and unsafe.SliceData is equally
// ambiguous), so we keep a possibly-redundant format marker rather than risk
// dropping a needed one. That is the safe divergence direction — the opposite
// choice would re-break the common distinct-empty case Correctness 5 fixed.
func sameSliceRef(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if len(a) == 0 {
		// Both non-nil and empty: there is no element to index (&a[0] would
		// panic), and two distinct empty arrays are !== in JS. Match === : not
		// the same reference.
		return false
	}
	return &a[0] == &b[0]
}

// equalAttrsDeep reports DEEP structural equality with the SAME semantics as lib0
// function.equalityDeep (used by y-protocols Awareness setLocalState /
// applyAwarenessUpdate to compute the change-event filteredUpdated):
//
//	export const equalityDeep = (a, b) => {
//	  if (a === b) return true
//	  if (a == null || b == null || a.constructor !== b.constructor) return false
//	  switch (a.constructor) {
//	    case Object:  // size(a) === size(b) && every key: equalityDeep(a[k], b[k])
//	    case Array:   // a.length === b.length && every i: equalityDeep(a[i], b[i])
//	    ...           // (Uint8Array/Set/Map handled there too)
//	  }
//	}
//
// Unlike equalAttrs (lib0 equalFlat — SHALLOW, nested values by === reference),
// this recurses into nested Objects/arrays and compares them BY VALUE. It is the
// correct comparator for AWARENESS state diffing, where a freshly-decoded
// structurally-identical state is a distinct instance (nested !== shallow) and
// must NOT be reported as changed. Do NOT use it for the YText format callers —
// those mirror Yjs equalAttrs/equalFlat and must stay shallow (equalAttrs).
//
// The value domain is the decoded-JSON awareness-state domain: Object, []any, and
// the scalars the ordered JSON parser yields (string, float64, bool, NullType).
// nil/zero-Object (IsNil) is the absent/cleared sentinel and compares safely.
func equalAttrsDeep(a, b interface{}) bool {
	switch av := a.(type) {
	case Object:
		bo, ok := b.(Object)
		if !ok {
			return false
		}
		// A nil/zero Object is the absent sentinel; two absent compare equal, an
		// absent vs a present (any keys) does not. newObject() with zero keys and a
		// zero Object both have Len()==0, so size comparison handles them uniformly.
		if av.IsNil() || bo.IsNil() {
			return av.Len() == bo.Len()
		}
		if av.Len() != bo.Len() {
			return false
		}
		equal := true
		av.Range(func(key string, aVal any) {
			if !equal {
				return
			}
			bVal, present := bo.Get(key)
			if !present || !equalAttrsDeep(aVal, bVal) {
				equal = false
			}
		})
		return equal
	case []any:
		bs, ok := b.([]any)
		if !ok {
			return false
		}
		if len(av) != len(bs) {
			return false
		}
		for i := range av {
			if !equalAttrsDeep(av[i], bs[i]) {
				return false
			}
		}
		return true
	default:
		// Primitives (string/float64/bool), NullType, nil, and anything else: value
		// equality. Guard against an Object/[]any on the b side paired with a
		// primitive a — those are different JS constructors, so not equal (and a
		// composite on b would otherwise be DeepEqual'd against a scalar as false
		// anyway, but be explicit to mirror the constructor check).
		switch b.(type) {
		case Object, []any:
			return false
		}
		return reflect.DeepEqual(a, b)
	}
}

// getUnixTime returns the current Unix time in milliseconds.
func getUnixTime() int64 {
	return time.Now().UnixNano() / 1e6
}

// findIndex returns the index of the first element in the given array that satisfies the given filter function.
func findIndex(a ArrayAny, filter func(e interface{}) bool) Number {
	for i, e := range a {
		if filter(e) {
			return i
		}
	}

	return -1
}

// The top types are mapped from y.share.get(keyname) => type.
// `type` does not store any information about the `keyname`.
// This function finds the correct `keyname` for `type` and throws otherwise.
func findRootTypeKey(t abstractType) string {
	// @ts-ignore _y must be defined, otherwise unexpected case
	for key, value := range t.getDoc().share {
		if value == t {
			return key
		}
	}

	return ""
}
