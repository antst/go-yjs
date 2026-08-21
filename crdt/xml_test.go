package crdt

import (
	"math"
	"testing"
)

// ---------------------------------------------------------------- from xml_float_hook_parity_test.go
// MAX-gateway finding (MAX-J): xmlAttrValueString sent float64 to %v, diverging
// from JS Number::toString for large/small magnitudes, and a yXMLHook child was
// dropped to "" (no ToString) where yjs renders "[object Object]". Both expected
// values are captured from yjs@13.6.31. Teeth: pre-fix %v gives "1e+20"/"1e-07"
// and the hook child vanishes.

func TestXmlFloatAttrStringParity(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{math.Copysign(0, -1), "0"},
		{1e20, "100000000000000000000"},
		{1e-7, "1e-7"},
		{1e-6, "0.000001"},
		{1.5, "1.5"},
		{123456789012345680, "123456789012345680"},
		{1e21, "1e+21"},
		{0.1, "0.1"},
		{100, "100"},
		{1e-21, "1e-21"},
		{12345.678, "12345.678"},
	}
	for _, c := range cases {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		frag := doc.GetXMLFragment("x")
		el := NewYXmlElement("e")
		frag.Insert(0, ArrayAny{el})
		el.SetAttribute("a", c.v)
		got := el.ToString()
		want := `<e a="` + c.want + `"></e>`
		if got != want {
			t.Errorf("ToString for %g = %q, want %q (yjs)", c.v, got, want)
		}
	}
}

func TestXmlHookChildToStringParity(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	frag := doc.GetXMLFragment("x")
	el := NewYXmlElement("div")
	frag.Insert(0, ArrayAny{el})
	el.Insert(0, ArrayAny{newYXmlHook("myhook")})

	// yjs: a hook child renders via the default Object string coercion.
	if got, want := el.ToString(), "<div>[object Object]</div>"; got != want {
		t.Errorf("element-with-hook ToString = %q, want %q (yjs)", got, want)
	}
}

// ---------------------------------------------------------------- from xml_text_event_childlist_test.go
// ChildListChanged on both event types was permanently FALSE before the changed-key set was typed
// as strings.
//
// yjs stores parentSub as `string | null` and sets childListChanged when it sees null. Go's
// parentSub is a plain string using "" for a list change, so the ported `element == nil` test could
// never fire: a non-nil interface holding "" is not nil. Every child insert was therefore reported
// as an ATTRIBUTE change named "" instead, on YXmlEvent and YTextEvent alike.
//
// The differential oracle cannot see this. Observer events never reach the wire, so byte
// comparison at any seed count is blind to it -- the same blind spot that hid the deep-vs-shallow
// content copy. It was found only because typing the set as map[string]struct{} made the impossible
// nil comparison fail to compile.
//
// Pinned here because the fix arrived as a side effect of a performance refactor. Nothing else
// asserts it, so a future reshape of the changed-key representation would silently restore the bug.
func TestChildListChangedFiresForListChanges(t *testing.T) {
	t.Run("xml element", func(t *testing.T) {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		f := doc.GetXMLFragment("x")
		el := NewYXmlElement("div")
		f.Insert(0, ArrayAny{el})

		var childList bool
		var attrs []string
		el.Observe(func(e interface{}, _ interface{}) {
			if ev, ok := e.(*YXmlEvent); ok {
				childList = childList || ev.ChildListChanged
				ev.AttributesChanged.Range(func(s string) { attrs = append(attrs, s) })
			}
		})
		el.Insert(0, ArrayAny{NewYXmlElement("span")}) // pure child change

		if !childList {
			t.Errorf("pure child insert did not set ChildListChanged; attributes seen: %#v", attrs)
		}
		for _, a := range attrs {
			if a == "" {
				t.Errorf(`a list change leaked into AttributesChanged as ""`)
			}
		}
	})

	t.Run("xml attribute still reported as an attribute", func(t *testing.T) {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		f := doc.GetXMLFragment("x")
		el := NewYXmlElement("div")
		f.Insert(0, ArrayAny{el})

		var childList bool
		var attrs []string
		el.Observe(func(e interface{}, _ interface{}) {
			if ev, ok := e.(*YXmlEvent); ok {
				childList = childList || ev.ChildListChanged
				ev.AttributesChanged.Range(func(s string) { attrs = append(attrs, s) })
			}
		})
		el.SetAttribute("id", "v") // pure attribute change

		if childList {
			t.Error("an attribute change wrongly set ChildListChanged")
		}
		if len(attrs) != 1 || attrs[0] != "id" {
			t.Errorf("AttributesChanged = %#v, want [id]", attrs)
		}
	})

	t.Run("text", func(t *testing.T) {
		doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
		txt := doc.GetText("t")
		txt.Insert(0, "seed", Object{})

		var childList bool
		var fired int
		txt.Observe(func(e interface{}, _ interface{}) {
			fired++
			if ev, ok := e.(*YTextEvent); ok && ev.ChildListChanged {
				childList = true
			}
		})
		txt.Insert(0, "x", Object{})

		if fired == 0 {
			t.Fatal("no event fired; fixture is wrong")
		}
		if !childList {
			t.Error("text content change did not set ChildListChanged")
		}
	})
}

// ---------------------------------------------------------------- from xml_text_internal_delta_cache_test.go
func TestYXmlTextInternalDeltaCacheIsPrivateAndValidated(t *testing.T) {
	doc := newDoc("xml-text-internal-delta", false, defaultGCFilter, nil, false, WithClientID(1))
	fragment := doc.GetXMLFragment("x")
	text := NewYXmlText()
	fragment.Insert(0, ArrayAny{text})
	bold := newObject()
	bold.Set("bold", true)
	text.Insert(0, "x", bold)

	// Prime the validated delta cache used by the internal XML renderer.
	if got := text.ToString(); got != "<bold>x</bold>" {
		t.Fatalf("first render = %q", got)
	}
	if got := text.ToString(); got != "<bold>x</bold>" {
		t.Fatalf("cached render = %q", got)
	}

	// Public ToDelta must still return an isolated clone rather than the canonical
	// operators borrowed internally by ToString.
	public := text.ToDelta(nil, nil, nil)
	public[0].Attributes.Set("evil", true)
	if got := text.ToString(); got != "<bold>x</bold>" {
		t.Fatalf("public delta mutation leaked into internal render: %q", got)
	}

	// ContentFormat fields are exported. Direct mutation bypasses normal cache
	// invalidation, so the internal borrowed-cache path must validate before use.
	for item := text.start; item != nil; item = item.right {
		if format, ok := item.content.(*contentFormat); ok && format.key == "bold" {
			format.key = "italic"
		}
	}
	if got := text.ToString(); got != "<italic>x</italic>" {
		t.Fatalf("render reused stale internal delta after exported content mutation: %q", got)
	}
}

// ---------------------------------------------------------------- from xml_tostring_parity_test.go
// US8 / FR-030 (work item 1.7B). XML ToString must render attribute values,
// formatting marks, and embeds byte-identically to yjs, via the shared
// xmlAttrValueString helper. Expected strings encode yjs's JS string coercion
// (`'' + value`) and node-emission rules, verified against yjs@13.6.31
// src/types/YXmlElement.js + YXmlText.js toString.

// Code-review finding: a nested Y type embedded in an XmlText (reachable via
// ApplyDelta with an IAbstractType insert) must coerce via JS string coercion in
// ToString, not as a Go %v struct dump. Verified against yjs@13.6.31: an embedded
// Y.Map -> "[object Object]" (xt.toString() of [A, Y.Map, B] is "A[object Object]B"),
// while a type that overrides toString (Y.XmlElement) renders through it.
func TestXmlTextToStringEmbeddedNestedType(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	frag := doc.GetXMLFragment("x")
	xt := NewYXmlText()
	frag.Insert(0, ArrayAny{xt})
	xt.ApplyDelta([]EventOperator{
		NewTextDeltaOp("A", Object{}),
		NewValueDeltaOp(NewYMap(map[string]interface{}{"k": "v"}), Object{}),
		NewTextDeltaOp("B", Object{}),
	}, false)

	if got, want := xt.ToString(), "A[object Object]B"; got != want {
		t.Errorf("XmlText.ToString() with embedded Y.Map = %q, want %q", got, want)
	}
}

// A nested Y type with its own ToString (Y.XmlElement) coerces through it, not to
// "[object Object]" — so a blanket all-IAbstractType->"[object Object]" would be wrong.
func TestXmlAttrValueStringDispatchesNestedXmlElement(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	frag := doc.GetXMLFragment("x")
	el := NewYXmlElement("b")
	frag.Insert(0, ArrayAny{el})
	if got, want := xmlAttrValueString(el), "<b></b>"; got != want {
		t.Errorf("xmlAttrValueString(Y.XmlElement) = %q, want %q", got, want)
	}
}

// Full-review finding: a typed-nil Y-type pointer reaching xmlAttrValueString (as an
// XML attribute value or text embed) must coerce like JS null -> "null", not panic
// (calling ToString on a nil receiver). The per-type dispatch introduced this risk.
func TestXmlAttrValueStringTypedNil(t *testing.T) {
	var nilText *YText
	var nilXMLEl *YXmlElement
	var nilMap *YMap // hits the IAbstractType catch-all path
	cases := []any{nilText, nilXMLEl, nilMap}
	for _, c := range cases {
		if got := xmlAttrValueString(c); got != "null" {
			t.Errorf("xmlAttrValueString(typed-nil %T) = %q, want \"null\"", c, got)
		}
	}
}

func TestXmlAttrValueString(t *testing.T) {
	obj := newObject()
	obj.Set("r", float64(1))
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string", "hi", "hi"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"int", 42, "42"},
		{"float", 1.5, "1.5"},
		{"nil", nil, "null"},
		{"Null sentinel", Null, "null"},
		{"Undefined", Undefined, "undefined"},
		{"array", ArrayAny{float64(1), float64(2), float64(3)}, "1,2,3"},
		{"array with null", ArrayAny{float64(1), nil, float64(2)}, "1,,2"},
		{"nested array", ArrayAny{float64(1), ArrayAny{float64(2), float64(3)}}, "1,2,3"},
		{"object", obj, "[object Object]"},
		{"raw map", map[string]any{"a": float64(1)}, "[object Object]"},
	}
	for _, c := range cases {
		if got := xmlAttrValueString(c.in); got != c.want {
			t.Errorf("%s: xmlAttrValueString(%v) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestYXmlElementToStringMixedAttrs(t *testing.T) {
	doc := newDoc("guid", false, nil, nil, false, WithClientID(1))
	frag := doc.GetXMLFragment("x")
	el := NewYXmlElement("DIV")
	frag.Insert(0, ArrayAny{el})

	el.SetAttribute("s", "hi")
	el.SetAttribute("n", float64(42))
	el.SetAttribute("b", true)
	el.SetAttribute("arr", ArrayAny{float64(1), float64(2)})

	// attrs sorted: arr, b, n, s; nodeName lower-cased; values JS-coerced.
	want := `<div arr="1,2" b="true" n="42" s="hi"></div>`
	if got := el.ToString(); got != want {
		t.Errorf("YXmlElement.ToString() = %q, want %q", got, want)
	}
}

// B2: a boolean/string formatting mark must emit its wrapper node (was dropped
// because the value wasn't an Object).
func TestYXmlTextToStringBooleanMark(t *testing.T) {
	doc := newDoc("guid", false, nil, nil, false, WithClientID(1))
	frag := doc.GetXMLFragment("x")
	xt := NewYXmlText()
	frag.Insert(0, ArrayAny{xt})

	bold := newObject()
	bold.Set("bold", true)
	xt.Insert(0, "hi", bold)

	want := `<bold>hi</bold>`
	if got := xt.ToString(); got != want {
		t.Errorf("YXmlText.ToString() = %q, want %q (boolean mark wrapper dropped?)", got, want)
	}
}

// An object-valued mark enumerates its properties as wrapper-node attributes
// (sorted) — covers the Object branch of YXmlText.ToString. Reference confirmed
// against yjs@13.6.31: <comment id="c1" k="v">x</comment>.
func TestYXmlTextToStringObjectMark(t *testing.T) {
	doc := newDoc("guid", false, nil, nil, false, WithClientID(1))
	frag := doc.GetXMLFragment("x")
	xt := NewYXmlText()
	frag.Insert(0, ArrayAny{xt})

	inner := newObject()
	inner.Set("id", "c1")
	inner.Set("k", "v")
	mark := newObject()
	mark.Set("comment", inner)
	xt.Insert(0, "x", mark)

	want := `<comment id="c1" k="v">x</comment>`
	if got := xt.ToString(); got != want {
		t.Errorf("YXmlText.ToString() = %q, want %q", got, want)
	}
}
