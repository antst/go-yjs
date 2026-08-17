package crdt

import "testing"

// The remaining 16 of the 36 tracked operations: the XML surface, text attributes, ApplyDelta, and
// the array/map traversal variants. With these the benchmark suite covers every operation the
// oracle's coverage mapping tracks, so "we are faster" can be a claim about the library rather than
// about the subset that happened to have benchmarks.
//
// XML had ZERO benchmark coverage before this file. It is also the surface where the read paths are
// most elaborate -- QuerySelectorAll and CreateTreeWalker walk the whole tree -- so it is the least
// safe place to have been assuming.

const xmlNodes = 500

// benchXmlTree builds a fragment of xmlNodes elements, each carrying two attributes and a text
// child, so selector and walker benchmarks traverse real structure rather than a flat list.
func benchXmlTree() *YXmlFragment {
	doc := perfDoc()
	f := doc.GetXmlFragment("x")
	for i := 0; i < xmlNodes; i++ {
		el := NewYXmlElement("div")
		if i%3 == 0 {
			el = NewYXmlElement("span")
		}
		el.SetAttribute("id", mapKey(i))
		el.SetAttribute("class", "row")
		txt := NewYXmlText()
		txt.Insert(0, "cell", Object{})
		el.Insert(0, ArrayAny{txt})
		f.Insert(f.GetLength(), ArrayAny{el})
	}
	return f
}

func BenchmarkXmlQuerySelector(b *testing.B) {
	f := benchXmlTree()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = f.QuerySelector("span")
	}
}

func BenchmarkXmlQuerySelectorAll(b *testing.B) {
	f := benchXmlTree()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = f.QuerySelectorAll("div")
	}
}

func BenchmarkXmlCreateTreeWalker(b *testing.B) {
	f := benchXmlTree()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := f.CreateTreeWalker(func(SharedType) bool { return true })
		for n := w.Next(); n != nil; n = w.Next() {
			_ = n
		}
	}
}

func BenchmarkXmlToString(b *testing.B) {
	f := benchXmlTree()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkString = f.ToString()
	}
}

func BenchmarkXmlGetFirstChild(b *testing.B) {
	f := benchXmlTree()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = f.GetFirstChild()
	}
}

func BenchmarkXmlSlice(b *testing.B) {
	f := benchXmlTree()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = f.Slice(0, xmlNodes/2)
	}
}

func BenchmarkXmlInsertAfter(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		f := perfDoc().GetXmlFragment("x")
		first := NewYXmlElement("div")
		f.Insert(0, ArrayAny{first})
		b.StartTimer()
		ref := f.GetFirstChild()
		for j := 0; j < 200; j++ {
			f.InsertAfter(ref, ArrayAny{NewYXmlElement("div")})
		}
	}
}

// ---------------------------------------------------------------- xml element attributes

func benchXmlElement() *YXmlElement {
	f := perfDoc().GetXmlFragment("x")
	el := NewYXmlElement("div")
	f.Insert(0, ArrayAny{el})
	for i := 0; i < 50; i++ {
		el.SetAttribute(mapKey(i), "v")
	}
	return el
}

// xmlSetAttributeOverwrites is how many times the measured operation replaces the same key. It must
// stay identical in fuzz/perf_bench.mjs and bench/yrs/src/main.rs — the number itself is arbitrary,
// but its EQUALITY across the harnesses is what makes the row comparable.
const xmlSetAttributeOverwrites = 100

// BenchmarkXmlSetAttribute measures repeated replacement of one attribute on a FRESH 50-attribute
// element.
//
// The fixture must be rebuilt per measured operation. Replacing a key does not overwrite anything
// in a CRDT: it appends a new item and tombstones the old one, so the element's history grows with
// every call. The previous version built one element and then overwrote `id` b.N times, which made
// the measured workload a function of how many iterations the harness chose to run — Go autoscaled
// into millions of accumulated items, the JS harness ran until its time budget expired, and the yrs
// harness stopped at a fixed 2000. Three different history depths were being compared as though
// they were three speeds, and the row moved when the time budget changed rather than when the code
// did.
//
// Timing a SINGLE replacement per iteration would fix comparability but put a ~600ns measurement
// next to b.StopTimer/b.StartTimer overhead. A fixed count inside the timed region pins the history
// depth identically everywhere and amortises that overhead, so one reported op is
// xmlSetAttributeOverwrites replacements — the unit only has to match across implementations, and
// it now does.
func BenchmarkXmlSetAttribute(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		el := benchXmlElement()
		b.StartTimer()
		for j := 0; j < xmlSetAttributeOverwrites; j++ {
			el.SetAttribute("id", "x")
		}
	}
}

func BenchmarkXmlGetAttribute(b *testing.B) {
	el := benchXmlElement()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 50; j++ {
			benchSinkAny = el.GetAttribute(mapKey(j))
		}
	}
}

func BenchmarkXmlGetAttributes(b *testing.B) {
	el := benchXmlElement()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkObj = el.GetAttributes()
	}
}

func BenchmarkXmlRemoveAttribute(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		el := benchXmlElement()
		b.StartTimer()
		for j := 0; j < 50; j++ {
			el.RemoveAttribute(mapKey(j))
		}
	}
}

// ---------------------------------------------------------------- text delta / attributes

// ApplyDelta is the entry point every rich-text binding (Quill, ProseMirror) drives, and it had no
// benchmark at all despite being the busiest write path a real editor uses.
func BenchmarkTextApplyDelta(b *testing.B) {
	delta := make([]EventOperator, 0, 200)
	for j := 0; j < 200; j++ {
		attrs := newObject()
		attrs.Set("bold", j%2 == 0)
		delta = append(delta, NewTextDeltaOp("chunk", attrs))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		t := perfDoc().GetText("t")
		b.StartTimer()
		t.ApplyDelta(delta, true)
	}
}

func BenchmarkTextGetAttributes(b *testing.B) {
	t := benchText(perfSmall)
	t.SetAttribute("lang", "en")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkObj = t.GetAttributes(nil)
	}
}

// ---------------------------------------------------------------- array traversal variants

func BenchmarkArraySplice(b *testing.B) {
	a := benchArray(perfSmall)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = a.Splice(0, perfSmall-1)
	}
}

func BenchmarkArrayMap(b *testing.B) {
	a := benchArray(perfSmall)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = a.Map(func(v interface{}, _ Number, _ *YArray) interface{} { return v })
	}
}

// benchSink defeats dead-code elimination. An empty callback lets the compiler prove the traversal
// has no observable effect and delete it outright -- BenchmarkArrayRange first reported 0.98 ns/op
// for walking 2000 items, which is not a fast traversal, it is no traversal. Every callback-driven
// benchmark below accumulates into this.
var benchSink int

// Range is ITEM-level (its callback takes *Item), not element-level like ForEach, and it is a
// Go-only convenience with no yjs or yrs counterpart -- so it is reported without a reference
// column rather than shown as a gap.
//
// The fixture is built with random-position inserts on purpose. Sequential inserts merge into a
// SINGLE item, so Range would visit exactly one item however long the array is; the first version
// of this benchmark did precisely that and reported 2.1 ns/op for a 2000-element array, which
// measured merging rather than traversal.
func BenchmarkArrayRange(b *testing.B) {
	a := perfDoc().GetArray("a")
	rng := perfRand()
	for j := 0; j < perfSmall; j++ {
		a.Insert(rng.intn(a.GetLength()+1), ArrayAny{j})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		a.rangeItems(func(it *itemStruct) { n += it.length })
		benchSink += n
	}
}

func BenchmarkArrayFrom(b *testing.B) {
	items := make(ArrayAny, perfSmall)
	for j := 0; j < perfSmall; j++ {
		items[j] = j
	}
	a := NewYArray()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = a.From(items)
	}
}

func BenchmarkMapGetSize(b *testing.B) {
	m := benchMap(perfSmall)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSink += m.GetSize()
	}
}
