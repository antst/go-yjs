package crdt

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// ---------------------------------------------------------------- from y_xml_element.go
// jsNumberToString renders a float64 the way ECMAScript Number::toString(10) does,
// so XML attribute/embed string coercion matches yjs (empty-string-plus-value). JS
// uses fixed notation for 1e-6 <= |x| < 1e21 and exponential (with a leading-zero-free
// exponent, e.g. "1e-7" / "1e+21") outside that range; Go's %v differs (1e+20, 1e-07).
func jsNumberToString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0" // JS String(-0) === "0"
	}
	abs := math.Abs(f)
	if abs >= 1e21 || abs < 1e-6 {
		s := strconv.FormatFloat(f, 'e', -1, 64)
		// Go emits "1e-07" / "1.5e+20"; JS drops leading zeros in the exponent.
		if i := strings.IndexByte(s, 'e'); i >= 0 {
			mant, sign, exp := s[:i], s[i+1], s[i+2:]
			exp = strings.TrimLeft(exp, "0")
			if exp == "" {
				exp = "0"
			}
			s = mant + "e" + string(sign) + exp
		}
		return s
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

type YXmlElement struct {
	YXmlFragment

	prelimAttrs map[string]interface{}
	NodeName    string
}

// GetNextSibling return {YXmlElement|YXmlText|nil}
func (y *YXmlElement) GetNextSibling() SharedType {
	var n *itemStruct
	if y.item != nil {
		n = y.item.nextItem()
	}

	if n != nil {
		t, ok := n.content.(*contentType)
		if ok {
			return asSharedType(t.value)
		}
	}

	return nil
}

// GetPrevSibling return {YXmlElement|YXmlText|nil}
func (y *YXmlElement) GetPrevSibling() SharedType {
	var n *itemStruct
	if y.item != nil {
		n = y.item.prevItem()
	}

	if n != nil {
		t, ok := n.content.(*contentType)
		if ok {
			return asSharedType(t.value)
		}
	}

	return nil
}

// integrate this type into the Yjs instance.
//
//	Save this struct in the os
//	This type is sent to other client
//	Observer functions are fired
func (y *YXmlElement) integrate(doc *Doc, item *itemStruct) {
	y.abstractTypeBase.integrate(doc, item)

	// Sorted key order — see YMap.Integrate: Go map iteration is randomised, so a
	// locally-built element's attributes integrated in a different order each run and the
	// encoded bytes differed for an identical build.
	attrKeys := make([]string, 0, len(y.prelimAttrs))
	for key := range y.prelimAttrs {
		attrKeys = append(attrKeys, key)
	}
	sort.Strings(attrKeys)
	for _, key := range attrKeys {
		y.SetAttribute(key, y.prelimAttrs[key])
	}

	y.prelimAttrs = nil
}

// copyType Creates an Item with the same effect as this Item (without position effect)
func (y *YXmlElement) copyType() abstractType {
	return NewYXmlElement(y.NodeName)
}

func (y *YXmlElement) cloneType() abstractType {
	el := NewYXmlElement(y.NodeName)
	attrs := y.GetAttributes()

	attrs.Range(func(key string, value any) {
		el.SetAttribute(key, value)
	})

	var data []interface{}
	for _, element := range y.ToArray() {
		item, ok := element.(abstractType)
		if ok {
			data = append(data, item.cloneType())
		} else {
			data = append(data, element)
		}
	}

	el.Insert(0, data)
	return el
}

// Clone returns an independent XML element with cloned descendants.
func (y *YXmlElement) Clone() *YXmlElement { return y.cloneType().(*YXmlElement) }

// Returns the XML serialization of this YXmlElement.
// The attributes are ordered by attribute-name, so you can easily use this
// method to compare YXmlElements
//
// @return {string} The string representation of this type.
// xmlAttrValueString renders a value the way yjs serializes XML attribute values
// and text embeds — JavaScript string coercion (the empty-string-plus-value
// idiom): nil / the Null
// sentinel → "null", Undefined → "undefined", strings as-is, booleans →
// "true"/"false", arrays → comma-joined elements (null/undefined elements render
// empty, like Array.prototype.join), objects → "[object Object]", other primitives
// via %v. Shared by YXmlElement.ToString and YXmlText.ToString (DRY, Principle VII).
func xmlAttrValueString(v any) string {
	// Null sentinel OR a typed-nil pointer (e.g. (*YText)(nil) reaching here as an
	// attribute value / text embed) coerces like JS null -> "null". IsNull covers both;
	// without it a typed-nil would fall into a concrete *Y* case below and panic
	// calling ToString on a nil receiver. (Untyped nil is IsNull-false, handled by the
	// `case nil` below.)
	if isNull(v) {
		return "null"
	}
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return x
	case []uint8:
		// JS Array.prototype.toString joins with commas: String([1,2,3]) === "1,2,3". Go's default
		// slice formatting gives "[1 2 3]", which is a different attribute VALUE on the wire.
		parts := make([]string, len(x))
		for i, b := range x {
			parts[i] = strconv.Itoa(int(b))
		}
		return strings.Join(parts, ",")
	case float64:
		return jsNumberToString(x)
	case float32:
		return jsNumberToString(float64(x))
	case bool:
		if x {
			return "true"
		}
		return "false"
	case ArrayAny:
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil || isNull(e) || isUndefined(e) {
				parts[i] = ""
			} else {
				parts[i] = xmlAttrValueString(e)
			}
		}
		return strings.Join(parts, ",")
	case Object, map[string]any:
		return "[object Object]"
	// A nested Y type embedded as an XML attribute value or text embed coerces via JS
	// string coercion (`'' + value`), verified against yjs@13.6.31: the types that
	// override toString render through it (Y.XmlElement -> "<b></b>", Y.Text -> its
	// text), and the rest (Y.Map/Y.Array, Doc) have no toString and become
	// "[object Object]" (e.g. an embedded Y.Map in XmlText.toString -> "A[object Object]B").
	// Without this they fell through to %v and emitted a Go struct/pointer dump.
	case *YText:
		return x.ToString()
	case *YXmlText:
		return x.ToString()
	case *YXmlElement:
		return x.ToString()
	case *YXmlFragment:
		return x.ToString()
	case abstractType:
		return "[object Object]"
	default:
		if isUndefined(v) {
			return "undefined"
		}
		return fmt.Sprintf("%v", x)
	}
}

func (y *YXmlElement) ToString() string {
	var builder strings.Builder
	builder.Grow(32 + y.GetLength()*40)
	y.appendXMLTo(&builder)
	return builder.String()
}

func (y *YXmlElement) appendXMLTo(builder *strings.Builder) {
	nodeName := strings.ToLower(y.NodeName)
	builder.WriteByte('<')
	builder.WriteString(nodeName)

	// XML attribute order is canonical and wire-observable. Most elements carry
	// only a handful, so sort through stack storage and allocate only for the rare
	// wide element.
	var inlineKeys [8]string
	keys := inlineKeys[:0]
	if len(y.typeMap) > len(inlineKeys) {
		keys = make([]string, 0, len(y.typeMap))
	}
	for key, item := range y.typeMap {
		if !item.isDeleted() {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		builder.WriteByte(' ')
		builder.WriteString(key)
		builder.WriteString(`="`)
		builder.WriteString(xmlAttrValueString(mapItemValue(y.typeMap[key])))
		builder.WriteByte('"')
	}
	builder.WriteByte('>')
	y.YXmlFragment.appendXMLTo(builder)
	builder.WriteString(`</`)
	builder.WriteString(nodeName)
	builder.WriteByte('>')
}

// RemoveAttribute Removes an attribute from this YXmlElement.
func (y *YXmlElement) RemoveAttribute(attributeName string) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			typeMapDelete(trans, y, attributeName)
		}, nil, true)
	} else {
		delete(y.prelimAttrs, attributeName)
	}
}

// SetAttribute Sets or updates an attribute.
func (y *YXmlElement) SetAttribute(attributeName string, attributeValue interface{}) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			_ = typeMapSet(trans, y, attributeName, attributeValue)
		}, nil, true)
	} else {
		if y.prelimAttrs == nil {
			y.prelimAttrs = make(map[string]interface{})
		}
		y.prelimAttrs[attributeName] = attributeValue
	}
}

// GetAttribute Returns an attribute value that belongs to the attribute name.
func (y *YXmlElement) GetAttribute(attributeName string) interface{} {
	return typeMapGet(y, attributeName)
}

// HasAttribute Returns whether an attribute exists
func (y *YXmlElement) HasAttribute(attributeName string) bool {
	return typeMapHas(y, attributeName)
}

// GetAttributes Returns an attribute value that belongs to the attribute name.
func (y *YXmlElement) GetAttributes() Object {
	return typeMapGetAll(y)
}

func (y *YXmlElement) writeType(encoder updateEncoder) {
	encoder.writeTypeRef(yXmlElementRefID)
	if err := encoder.writeKey(y.NodeName); err != nil {
		// Unreachable: every updateEncoder writes into a bytes.Buffer, whose Write
		// never returns an error. writeType cannot report one, so silently emitting
		// a truncated element would corrupt the update. Fail loudly instead — a
		// panic here means an encoder was added that CAN fail and needs a real
		// error path, which is a defect to fix rather than to survive.
		panic("y-crdt: encoding an XML element name failed: " + err.Error())
	}
}

// NewYXmlElement creates a Y.XmlElement. Preliminary attributes and observer handlers are both
// allocated lazily: an element used only as an unobserved tree node needs neither.
func NewYXmlElement(nodeName string) *YXmlElement {
	return &YXmlElement{NodeName: nodeName}
}

// ---------------------------------------------------------------- from y_xml_event.go
// YXmlEvent An Event that describes changes on a YXml Element or Yxml Fragment
type YXmlEvent struct {
	YEvent
	ChildListChanged  bool        // Whether the children changed.
	AttributesChanged ChangedSubs // Set of all changed attributes.
}

func newYXmlEvent(target abstractType, subs ChangedSubs, trans *Transaction) *YXmlEvent {
	e := &YXmlEvent{
		YEvent:            *newYEvent(target, trans),
		ChildListChanged:  false,
		AttributesChanged: newChangedSubs(),
	}

	subs.Range(func(element string) {
		if element == "" {
			e.ChildListChanged = true
		} else {
			e.AttributesChanged.Add(element)
		}
	})

	return e
}

// ---------------------------------------------------------------- from y_xml_fragment.go
/**
 * Define the elements to which a set of CSS queries apply.
 * {@link https://developer.mozilla.org/en-US/docs/Web/CSS/CSS_Selectors|CSS_Selectors}
 *
 * @example
 *   query = '.classSelector'
 *   query = 'nodeSelector'
 *   query = '#idSelector'
 *
 * @typedef {string} CSS_Selector
 */

/**
 * Dom filter function.
 *
 * @callback domFilter
 * @param {string} nodeName The nodeName of the element
 * @param {Map} attributes The map of attributes.
 * @return {boolean} Whether to include the Dom node in the YXmlElement.
 */

/**
 * Represents a subset of the nodes of a YXmlElement / YXmlFragment and a
 * position within them.
 *
 * Can be created with {@link YXmlFragment#createTreeWalker}
 *
 * @public
 * @implements {Iterable<YXmlElement|YXmlText|YXmlElement|yXmlHook>}
 */

// YXmlTreeWalker is a depth-first walker over an XML subtree, faithful to yjs's YXmlTreeWalker
// (src/types/YXmlFragment.js). It was previously a struct whose constructor returned nil and whose
// Filter had the wrong signature — a stub that made CreateTreeWalker, QuerySelector and
// QuerySelectorAll all silently do nothing (Constitution IX: no misleading placeholders).
type YXmlTreeWalker struct {
	filter      func(abstractType) bool
	root        abstractType
	currentNode *itemStruct
	firstCall   bool
}

type xmlType interface {
	ToString() string
}

type YXmlFragment struct {
	abstractTypeBase
	prelimContent ArrayAny
	sliceCache    atomic.Pointer[yXmlSliceCache]
	slicePrimed   atomic.Bool
	readIndex     atomic.Pointer[listReadIndex]
}

const maxCachedXmlSliceLength = 4096

type yXmlSliceCache struct {
	values ArrayAny
}

func xmlFragmentReadCache(t abstractType) *YXmlFragment {
	switch xml := t.(type) {
	case *YXmlFragment:
		return xml
	case *YXmlElement:
		return &xml.YXmlFragment
	default:
		return nil
	}
}

func invalidateYXmlSliceCache(t abstractType) {
	if xml := xmlFragmentReadCache(t); xml != nil {
		xml.sliceCache.Store(nil)
		xml.slicePrimed.Store(false)
	}
}

func (y *YXmlFragment) GetFirstChild() SharedType {
	first := y.firstItem()
	if first == nil {
		return nil
	}
	switch content := first.content.(type) {
	case *contentType:
		return asSharedType(content.value)
	case *contentAny:
		if len(content.arr) > 0 {
			value, _ := content.arr[0].(SharedType)
			return value
		}
	default:
		values := content.contentValues()
		if len(values) > 0 {
			value, _ := values[0].(SharedType)
			return value
		}
	}
	return nil
}

// integrate this type into the Yjs instance.
//
// Save this struct in the os
// This type is sent to other client
// Observer functions are fired
func (y *YXmlFragment) integrate(doc *Doc, item *itemStruct) {
	y.abstractTypeBase.integrate(doc, item)
	y.Insert(0, y.prelimContent)
	y.prelimContent = nil
}

func (y *YXmlFragment) copyType() abstractType {
	return NewYXmlFragment()
}

func (y *YXmlFragment) cloneType() abstractType {
	el := NewYXmlFragment()

	var data []interface{}
	for _, element := range y.ToArray() {
		item, ok := element.(abstractType)
		if ok {
			data = append(data, item.cloneType())
		} else {
			data = append(data, element)
		}
	}

	el.Insert(0, data)
	return el
}

// Clone returns an independent XML fragment with cloned nested shared values.
func (y *YXmlFragment) Clone() *YXmlFragment { return y.cloneType().(*YXmlFragment) }

func (y *YXmlFragment) GetLength() Number {
	if y.prelimContent == nil {
		return y.length
	}

	return len(y.prelimContent)
}

func (y *YXmlFragment) CreateTreeWalker(filter func(SharedType) bool) *YXmlTreeWalker {
	return NewYXmlTreeWalker(y, filter)
}

// QuerySelector returns the first descendant whose node name matches query, case-insensitively,
// or nil. Faithful to yjs: uppercase the query and filter on nodeName (src/types/YXmlFragment.js).
func (y *YXmlFragment) QuerySelector(query string) SharedType {
	w := newYXmlTreeWalker(y, nil)
	filter := func(t abstractType) bool {
		return strings.EqualFold(xmlNodeName(t), query)
	}
	if result := w.next(filter); result != nil {
		return asSharedType(result)
	}
	return nil
}

// QuerySelectorAll returns every descendant whose node name matches query, case-insensitively.
func (y *YXmlFragment) QuerySelectorAll(query string) []SharedType {
	w := newYXmlTreeWalker(y, nil)
	filter := func(t abstractType) bool {
		return strings.EqualFold(xmlNodeName(t), query)
	}
	var out []SharedType
	for n := w.next(filter); n != nil; n = w.next(filter) {
		out = append(out, asSharedType(n))
	}
	return out
}

// xmlNodeName is the nodeName yjs matches on; types without one never match a query.
func xmlNodeName(t abstractType) string {
	if el, ok := t.(*YXmlElement); ok {
		return el.NodeName
	}
	return ""
}

// Creates YXmlEvent and calls observers.
func (y *YXmlFragment) callObserver(trans *Transaction, parentSubs ChangedSubs) {
	if hasTypeObservers(y) {
		callTypeObservers(y, trans, newYXmlEvent(y, parentSubs, trans))
	}
}

// Get the string representation of all the children of this YXmlFragment.
func (y *YXmlFragment) ToString() string {
	var builder strings.Builder
	// The benchmark tree averages roughly forty bytes per child. Grow is only a
	// hint (arbitrary embedded values may render larger), but it keeps ordinary
	// XML serialization to one output allocation rather than repeated doubling.
	builder.Grow(y.GetLength() * 40)
	y.appendXMLTo(&builder)
	return builder.String()
}

func (y *YXmlFragment) appendXMLTo(builder *strings.Builder) {
	for item := y.start; item != nil; item = item.right {
		if item.isDeleted() || !item.countable() {
			continue
		}
		if content, ok := item.content.(*contentAny); ok {
			for _, value := range content.arr {
				appendXMLValue(builder, value)
			}
			continue
		}
		if content, ok := item.content.(*contentType); ok {
			appendXMLValue(builder, content.value)
			continue
		}
		for _, value := range item.content.contentValues() {
			appendXMLValue(builder, value)
		}
	}
}

func appendXMLValue(builder *strings.Builder, value any) {
	switch value := value.(type) {
	case *YXmlElement:
		value.appendXMLTo(builder)
	case *YXmlText:
		value.appendXMLTextTo(builder)
	case *YXmlFragment:
		value.appendXMLTo(builder)
	case xmlType:
		builder.WriteString(value.ToString())
	default:
		// A child that is not an XML type is COERCED, not dropped. The reference builds its
		// string with `.map(xml => xml.toString()).join('')`, and JS coerces every value —
		// String(aMap) === "[object Object]", String(42) === "42".
		builder.WriteString(xmlAttrValueString(value))
	}
}

func (y *YXmlFragment) ToJson() interface{} {
	return y.ToString()
}

// not supported yet.

// Insert Inserts new content at an index.
//
// @example
//
//	// Insert character 'a' at position 0
//
// xml.insert(0, [new Y.XmlText('text')])
func (y *YXmlFragment) Insert(index Number, content ArrayAny) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			_ = typeListInsertGenerics(trans, y, index, content)
		}, nil, true)
	} else {
		spliceArray(&y.prelimContent, index, 0, content)
	}
}

// Inserts new content at an index.
//
// @example
//
//	// Insert character 'a' at position 0
//	xml.insert(0, [new Y.XmlText('text')])
func (y *YXmlFragment) InsertAfter(ref SharedType, content ArrayAny) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			var refItem *itemStruct
			if ref != nil {
				refItem = asAbstractType(ref).getItem()
			}

			_ = typeListInsertGenericsAfter(trans, y, refItem, content)
		}, nil, true)
	} else {
		index := 0

		if ref != nil {
			index = findIndex(y.prelimContent, func(e interface{}) bool {
				return e == ref
			}) + 1
		}

		if index == 0 && ref != nil {
			return
		}

		// Splice the FIELD, not a local copy. `pc := y.PrelimContent` copies the slice
		// header, and spliceArray either reassigns through its pointer (`*a = combine`) or
		// extends only the local length — either way y.PrelimContent was never written
		// back, so content added before integration was silently discarded and the element
		// later integrated empty. Matches the sibling Insert above.
		spliceArray(&y.prelimContent, index, 0, content)
	}
}

// Deletes elements starting from an index.
// Default: length = 1
func (y *YXmlFragment) Delete(index, length Number) {
	if y.doc != nil {
		transactMutation(y.doc, func(trans *Transaction) {
			_ = typeListDelete(trans, y, index, length)
		}, nil, true)
	} else {
		// @ts-ignore _prelimContent is defined because this is not yet integrated
		spliceArray(&y.prelimContent, index, length, nil)
	}
}

// Transforms this YArray to a JavaScript Array.
func (y *YXmlFragment) ToArray() ArrayAny {
	return typeListToArray(y)
}

// Push appends content to this fragment.
//
// Deliberately NOT routed through typeListPushGenerics, unlike YArray.Push. The reference is
// genuinely inconsistent here: YArray.push calls typeListPushGenerics (append after the last ITEM,
// tombstones included) while YXmlFragment.push calls `this.insert(this.length, content)` (append at
// the visible INDEX, before trailing tombstones). Making the two consistent would be a deviation.
// Verified by the differential: routing this through the push primitive diverged 1096/3000 seeds.
func (y *YXmlFragment) Push(content ArrayAny) {
	y.Insert(y.length, content)
}

// Preppends content to this YArray.
func (y *YXmlFragment) Unshift(content ArrayAny) {
	y.Insert(0, content)
}

// Returns the i-th element from a YArray.
func (y *YXmlFragment) Get(index Number) interface{} {
	return typeListGet(y, index)
}

// Transforms this YArray to a JavaScript Array.
// Default: start = 0
func (y *YXmlFragment) Slice(start, end Number) ArrayAny {
	length := y.GetLength()
	normalizedStart, normalizedEnd := start, end
	if normalizedStart < 0 {
		normalizedStart += length
	}
	if normalizedEnd < 0 {
		normalizedEnd += length
	}
	// The cached representation is useful only for ordinary bounded slices. Preserve the generic
	// walker for unusual/out-of-range arguments so its exact nil/empty/capacity behaviour remains
	// unchanged.
	if normalizedStart >= 0 && normalizedStart <= normalizedEnd && normalizedStart <= length {
		if cached := y.sliceCache.Load(); cached != nil {
			normalizedEnd = minNumber(normalizedEnd, len(cached.values))
			return slices.Clone(cached.values[normalizedStart:normalizedEnd])
		}
		if y.doc != nil && y.doc.readCacheEnabled && length > 0 && length <= maxCachedXmlSliceLength &&
			y.slicePrimed.CompareAndSwap(true, false) {
			values := typeListToArray(y)
			y.sliceCache.Store(&yXmlSliceCache{values: values})
			normalizedEnd = minNumber(normalizedEnd, len(values))
			return slices.Clone(values[normalizedStart:normalizedEnd])
		}
		if y.doc != nil && y.doc.readCacheEnabled && length > 0 && length <= maxCachedXmlSliceLength {
			y.slicePrimed.Store(true)
		}
	}
	return typeListSlice(y, start, end)
}

// Transform the properties of this type to binary and write it to an
// BinaryEncoder.
//
// This is called when this Item is sent to a remote peer.
//
// @param {UpdateEncoderV1 | UpdateEncoderV2} encoder The encoder to write data to.
func (y *YXmlFragment) writeType(encoder updateEncoder) {
	encoder.writeTypeRef(yXmlFragmentRefID)
}

func NewYXmlFragment() *YXmlFragment {
	return &YXmlFragment{
		abstractTypeBase: abstractTypeBase{
			typeMap: make(map[string]*itemStruct),
		},
	}
}

func newYXmlFragmentType() SharedType {
	return NewYXmlFragment()
}

// not supported yet.
func NewYXmlTreeWalker(root SharedType, f func(SharedType) bool) *YXmlTreeWalker {
	if root == nil {
		return nil
	}
	filter := func(value abstractType) bool {
		return f == nil || f(asSharedType(value))
	}
	w := newYXmlTreeWalker(asAbstractType(root), filter)
	return &w
}

// newYXmlTreeWalker returns a value so internal one-shot selector reads can keep
// the walk state on their stack. The public constructor still returns a pointer,
// as required by its iterator-style API.
func newYXmlTreeWalker(root abstractType, f func(abstractType abstractType) bool) YXmlTreeWalker {
	if f == nil {
		f = func(abstractType) bool { return true } // yjs default: `f = () => true`
	}
	return YXmlTreeWalker{filter: f, root: root, currentNode: root.startItem(), firstCall: true}
}

// Next returns the next matching type, or nil when the walk is done — the Go shape of yjs's
// iterator protocol (`{value, done}`).
func (w *YXmlTreeWalker) Next() SharedType {
	if w == nil {
		return nil
	}
	if result := w.next(w.filter); result != nil {
		return asSharedType(result)
	}
	return nil
}

// next accepts the filter as an argument so internal selectors can use a
// capturing predicate without storing it in an escaping walker. Public walks
// keep their configured filter through Next above.
func (w *YXmlTreeWalker) next(filter func(abstractType) bool) abstractType {
	if w == nil {
		return nil
	}
	n := w.currentNode

	// On the first call the current node is a candidate; afterwards, always advance first.
	if n != nil && (!w.firstCall || n.isDeleted() || !filter(contentTypeOf(n))) {
		for {
			t := contentTypeOf(n)
			if !n.isDeleted() && isXmlContainer(t) && t.startItem() != nil {
				n = t.startItem() // walk DOWN
			} else {
				for n != nil { // walk RIGHT, else UP
					if nxt := n.nextItem(); nxt != nil {
						n = nxt
						break
					} else if n.parent == w.root {
						n = nil
					} else {
						p, ok := n.parent.(abstractType)
						if !ok || p == nil {
							n = nil
						} else {
							n = p.getItem()
						}
					}
				}
			}
			if n == nil || (!n.isDeleted() && filter(contentTypeOf(n))) {
				break
			}
		}
	}

	w.firstCall = false
	if n == nil {
		return nil
	}
	w.currentNode = n
	return contentTypeOf(n)
}

// contentTypeOf returns the nested type an item carries, or nil when it carries something else.
func contentTypeOf(item *itemStruct) abstractType {
	if item == nil {
		return nil
	}
	ct, ok := item.content.(*contentType)
	if !ok || ct == nil {
		return nil
	}
	return ct.value
}

// isXmlContainer reports whether a type can hold children, matching yjs's
// `type.constructor === YXmlElement || type.constructor === YXmlFragment`.
func isXmlContainer(t abstractType) bool {
	switch t.(type) {
	case *YXmlElement, *YXmlFragment:
		return true
	}
	return false
}

// ---------------------------------------------------------------- from y_xml_hook.go
// embeddedYMap keeps YMap's method promotion without exposing a field on this
// package-private reference-only shared type.
type embeddedYMap = YMap

// You can manage binding to a custom type with yXmlHook.
type yXmlHook struct {
	embeddedYMap
	hookName string
}

// copyType Creates an Item with the same effect as this Item (without position effect)
func (y *yXmlHook) copyType() abstractType {
	return newYXmlHook(y.hookName)
}

// cloneType
func (y *yXmlHook) cloneType() abstractType {
	el := newYXmlHook(y.hookName)
	y.ForEach(func(key string, value interface{}, yMap *YMap) {
		el.Set(key, value)
	})
	return el
}

// ToString makes yXmlHook satisfy xmlType so it renders inside a parent
// fragment/element. yjs yXmlHook has no toString override (it extends YMap), so
// JS string coercion yields the default "[object Object]" (verified: a hook child
// of <div> renders "<div>[object Object]</div>"). Without this the xmlType
// assertion in YXmlFragment.ToString failed and the hook child was dropped ("").
func (y *yXmlHook) ToString() string {
	return "[object Object]"
}

// Transform the properties of this type to binary and write it to an
// BinaryEncoder.
//
// This is called when this Item is sent to a remote peer.
func (y *yXmlHook) writeType(encoder updateEncoder) {
	encoder.writeTypeRef(yXmlHookRefID)
	if err := encoder.writeKey(y.hookName); err != nil {
		// Unreachable for the same reason as YXmlElement.writeType above.
		panic("y-crdt: encoding an XML hook name failed: " + err.Error())
	}
}

func newYXmlHook(hookName string) *yXmlHook {
	h := &yXmlHook{
		embeddedYMap: *NewYMap(nil),
		hookName:     hookName,
	}
	return h
}

// ---------------------------------------------------------------- from y_xml_text.go
// YXmlText Represents text in a Dom Element. In the future this type will also handle
// simple formatting information like bold and italic.
type YXmlText struct {
	YText
}

func (y *YXmlText) GetNextSibling() SharedType {
	var n *itemStruct
	if y.item != nil {
		n = y.item.nextItem()
	}

	if n != nil {
		t, ok := n.content.(*contentType)
		if ok {
			return asSharedType(t.value)
		}
	}

	return nil
}

func (y *YXmlText) GetPreSibling() SharedType {
	var n *itemStruct
	if y.item != nil {
		n = y.item.prevItem()
	}

	if n != nil {
		t, ok := n.content.(*contentType)
		if ok {
			return asSharedType(t.value)
		}
	}

	return nil
}

func (y *YXmlText) copyType() abstractType {
	return NewYXmlText()
}

func (y *YXmlText) cloneType() abstractType {
	text := NewYXmlText()
	text.ApplyDelta(y.ToDelta(nil, nil, nil), true)
	return text
}

// Clone returns an independent XML text node with the same delta.
func (y *YXmlText) Clone() *YXmlText { return y.cloneType().(*YXmlText) }

// not supported yet.

func (y *YXmlText) ToString() string {
	var builder strings.Builder
	builder.Grow(maxNumber(y.GetLength(), 16))
	y.appendXMLTextTo(&builder)
	return builder.String()
}

func (y *YXmlText) appendXMLTextTo(builder *strings.Builder) {
	// ContentFormat disables search markers when it integrates. While markers are
	// enabled there can be no formatting wrappers, so render strings and embeds
	// directly instead of constructing a delta and a temporary DOM description.
	if y.searchMarker != nil {
		for item := y.start; item != nil; item = item.right {
			if item.isDeleted() || !item.countable() {
				continue
			}
			switch content := item.content.(type) {
			case *contentString:
				builder.WriteString(content.value)
			case *contentEmbed:
				builder.WriteString(xmlAttrValueString(content.embed))
			case *contentType:
				builder.WriteString(xmlAttrValueString(content.value))
			default:
				for _, value := range content.contentValues() {
					builder.WriteString(xmlAttrValueString(value))
				}
			}
		}
		return
	}
	y.appendFormattedXMLTo(builder)
}

func (y *YXmlText) appendFormattedXMLTo(builder *strings.Builder) {
	delta := y.deltaForInternalRead()
	// Local DOM-building records (never wire-encoded), so plain structs replace the
	// ad-hoc Object maps the previous map-alias implementation used.
	type domAttr struct {
		key   string
		value any
	}
	type domNode struct {
		nodeName string
		attrFrom int
		attrTo   int
	}
	// These arenas hold COPIES of the delta's key/value pairs, and that is load-bearing rather
	// than incidental: the ops above are BORROWED from the delta cache, so sorting or otherwise
	// mutating op.Attributes directly — an obvious way to avoid this copy — would corrupt the
	// cache and silently change what a later public ToDelta returns.
	var nestedNodes []domNode
	var attrs []domAttr
	for _, data := range delta {
		nestedNodes = nestedNodes[:0]
		attrs = attrs[:0]
		data.Attributes.Range(func(nodeName string, v any) {
			// yjs always pushes a node per attribute key (B2 fix — was guarded on the
			// value being an Object). The inner attrs come from iterating the value's
			// own properties, so a boolean/string mark (e.g. bold:true) renders as a
			// bare <bold> wrapper rather than being dropped.
			attrFrom := len(attrs)
			if nodeAttrs, ok := v.(Object); ok {
				nodeAttrs.Range(func(key string, val any) {
					attrs = append(attrs, domAttr{key: key, value: val})
				})
				// sort attributes to get a unique order
				slices.SortFunc(attrs[attrFrom:], func(a, b domAttr) int {
					return strings.Compare(a.key, b.key)
				})
			}
			nestedNodes = append(nestedNodes, domNode{nodeName: nodeName, attrFrom: attrFrom, attrTo: len(attrs)})
		})

		// sort node order to get a unique order
		slices.SortFunc(nestedNodes, func(a, b domNode) int {
			return strings.Compare(a.nodeName, b.nodeName)
		})

		// Convert directly into the caller's builder. Building a temporary string per
		// wrapper made fragmented formatted XML quadratic in copied bytes.
		for i := 0; i < len(nestedNodes); i++ {
			node := nestedNodes[i]
			builder.WriteByte('<')
			builder.WriteString(node.nodeName)

			for j := node.attrFrom; j < node.attrTo; j++ {
				attr := attrs[j]
				builder.WriteByte(' ')
				builder.WriteString(attr.key)
				builder.WriteString(`="`)
				builder.WriteString(xmlAttrValueString(attr.value))
				builder.WriteByte('"')
			}

			builder.WriteByte('>')
		}

		switch data.Kind {
		case EventOperatorInsertText:
			builder.WriteString(data.InsertText)
		case EventOperatorInsertValue:
			builder.WriteString(xmlAttrValueString(data.Insert))
		default:
			builder.WriteString(xmlAttrValueString(data.InsertValue()))
		}
		for i := len(nestedNodes) - 1; i >= 0; i-- {
			builder.WriteString(`</`)
			builder.WriteString(nestedNodes[i].nodeName)
			builder.WriteByte('>')
		}
	}
}

func (y *YXmlText) ToJSON() string {
	return y.ToString()
}

func (y *YXmlText) writeType(encoder updateEncoder) {
	encoder.writeTypeRef(yXmlTextRefID)
}

func NewYXmlText() *YXmlText {
	text := &YXmlText{}
	// yjs YXmlText extends YText, whose constructor sets _searchMarker = [] (markers
	// ENABLED; disabled on formatting via ContentFormat.Integrate). NewYXmlText builds
	// the embedded YText directly (not via NewYText), so mirror the init here.
	text.searchMarker = []*arraySearchMarker{}
	return text
}
