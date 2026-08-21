package crdt

import (
	"sort"
)

// Read-path sweeps, the Go half of `fuzz/harness/reads.mjs`.
//
// The op streams drive only the MUTATING half of each type's API — measured, 18 of 50 derived
// operations. Every read and query operation was reachable by unit tests alone, and two defects
// this feature found (a text rendering that dropped children of unexpected kinds, a delta that
// omitted its attribute-presence flag) lived exactly there. Reads cannot be driven as ops, since
// they change nothing, but they can be compared as observations.
//
// These live in the non-test build rather than a _test.go file so the same bundles are available to
// any consumer wanting to diff two documents, and so the differential tests in this package and the
// gate share one definition instead of two that can drift.
//
// Both sides must produce byte-identical canonical output: values are projected through ToJSON (a
// nested type is a live object with parent back-pointers), keys are sorted by the canonicaliser, and
// probe keys/queries are FIXED so the two implementations ask the same questions.

// mapProbeKeys and xmlProbeTags mirror the constants in fuzz/harness/reads.mjs. Fixed, not random:
// random probes would ask the two implementations different questions.
var (
	mapProbeKeys = []string{"a", "b", "c", "d", "e", "missing"}
	xmlProbeTags = []string{"div", "span", "p", "nosuchtag"}
)

// projectReadValue renders a read value the way the JS side's `proj` does.
func projectReadValue(v interface{}) interface{} {
	if t, ok := v.(abstractType); ok && t != nil {
		return t.toJSONValue()
	}
	if v == nil {
		return Null
	}
	return v
}

// readsArray reports every Y.Array read operation's result.
func readsArray(a *YArray) Object {
	o := newObject()
	length := a.GetLength()
	o.Set("len", length)
	o.Set("toJSON", a.ToJSON())

	arr := a.ToArray()
	projected := make(ArrayAny, 0, len(arr))
	for _, v := range arr {
		projected = append(projected, projectReadValue(v))
	}
	o.Set("toArray", projected)

	gets := make(ArrayAny, 0, 3)
	for i := 0; i < 3 && i < length; i++ {
		gets = append(gets, projectReadValue(a.Get(i)))
	}
	o.Set("gets", gets)

	o.Set("mapIdx", a.Map(func(_ interface{}, i Number, _ *YArray) interface{} { return i }))

	forEachCount := 0
	a.ForEach(func(interface{}, Number, *YArray) { forEachCount++ })
	o.Set("forEachCount", forEachCount)

	// Range walks non-deleted ITEMS (not elements), which is what the JS side counts too.
	itemCount := 0
	a.rangeItems(func(*itemStruct) { itemCount++ })
	o.Set("itemCount", itemCount)
	return o
}

// readsMap reports every Y.Map read operation's result.
func readsMap(m *YMap) Object {
	o := newObject()
	o.Set("size", m.GetSize())
	o.Set("toJSON", m.ToJSON())

	keys := m.Keys()
	sort.Strings(keys)
	appendedKeys := m.AppendKeys(nil)
	sort.Strings(appendedKeys)
	keyList := make(ArrayAny, 0, len(keys))
	appendedKeyList := make(ArrayAny, 0, len(appendedKeys))
	values := make(ArrayAny, 0, len(keys))
	for _, k := range keys {
		keyList = append(keyList, k)
		values = append(values, projectReadValue(m.Get(k)))
	}
	for _, k := range appendedKeys {
		appendedKeyList = append(appendedKeyList, k)
	}
	o.Set("keys", keyList)
	o.Set("appendKeys", appendedKeyList)
	o.Set("values", values)

	entries := newObject()
	for k, v := range m.Entries() {
		entries.Set(k, projectReadValue(v))
	}
	o.Set("entries", entries)

	has := newObject()
	gets := newObject()
	for _, k := range mapProbeKeys {
		present := m.Has(k)
		has.Set(k, present)
		if present {
			gets.Set(k, projectReadValue(m.Get(k)))
		} else {
			gets.Set(k, Null)
		}
	}
	o.Set("has", has)
	o.Set("gets", gets)

	forEachCount := 0
	m.ForEach(func(string, interface{}, *YMap) { forEachCount++ })
	o.Set("forEachCount", forEachCount)
	return o
}

// xmlNodeString renders one XML child the way the JS side's `.toString()` does.
func xmlNodeString(v interface{}) interface{} {
	if x, ok := v.(xmlType); ok {
		return x.ToString()
	}
	return xmlAttrValueString(v)
}

// readsXML reports every Y.XmlFragment read and query operation's result.
func readsXML(x *YXmlFragment) Object {
	o := newObject()
	length := x.GetLength()
	o.Set("len", length)
	o.Set("toString", x.ToString())
	o.Set("toJSON", x.ToJSON())

	toArray := make(ArrayAny, 0, length)
	for _, n := range x.ToArray() {
		toArray = append(toArray, xmlNodeString(n))
	}
	o.Set("toArray", toArray)

	end := 2
	if length < end {
		end = length
	}
	slice := make(ArrayAny, 0, end)
	for _, n := range x.Slice(0, end) {
		slice = append(slice, xmlNodeString(n))
	}
	o.Set("slice", slice)

	if length > 0 {
		o.Set("get0", xmlNodeString(x.Get(0)))
	} else {
		o.Set("get0", Null)
	}
	if first := x.GetFirstChild(); first != nil {
		o.Set("firstChild", xmlNodeString(first))
	} else {
		o.Set("firstChild", Null)
	}

	qs := newObject()
	qsa := newObject()
	for _, tag := range xmlProbeTags {
		if one := x.QuerySelector(tag); one != nil {
			qs.Set(tag, xmlNodeString(one))
		} else {
			qs.Set(tag, Null)
		}
		qsa.Set(tag, len(x.QuerySelectorAll(tag)))
	}
	o.Set("querySelector", qs)
	o.Set("querySelectorAllCount", qsa)

	// A walker that silently returns nothing is exactly what the Go stub used to do, so the node
	// count is compared rather than merely the walk not panicking.
	walked := 0
	if w := x.CreateTreeWalker(func(SharedType) bool { return true }); w != nil {
		for n := w.Next(); n != nil; n = w.Next() {
			walked++
		}
	}
	o.Set("treeWalkerCount", walked)
	return o
}

// readsText reports every Y.Text read operation's result.
func readsText(t *YText) Object {
	o := newObject()
	o.Set("toString", t.ToString())
	o.Set("toJSON", t.ToJSON())
	o.Set("toDelta", deltaReadShape(t.ToDelta(nil, nil, nil)))

	attrs := t.GetAttributes(nil)
	if attrs.IsNil() {
		attrs = newObject()
	}
	o.Set("attributes", attrs)

	probes := newObject()
	for _, k := range []string{"x", "lang", "missing"} {
		v := t.GetAttribute(k)
		if v == nil {
			probes.Set(k, Null)
		} else {
			probes.Set(k, v)
		}
	}
	o.Set("getAttribute", probes)
	return o
}

// deltaReadShape renders a ToDelta result as the reference's toDelta() serializes: `attributes` is
// omitted entirely when absent, never emitted as an empty object.
func deltaReadShape(ops []EventOperator) ArrayAny {
	out := make(ArrayAny, 0, len(ops))
	for _, op := range ops {
		entry := newObject()
		if op.IsInsert() {
			entry.Set("insert", projectReadValue(op.InsertValue()))
		}
		if op.HasAttributes() && op.Attributes.Len() > 0 {
			entry.Set("attributes", op.Attributes)
		}
		out = append(out, entry)
	}
	return out
}
