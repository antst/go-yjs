package crdt

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
)

// Concurrency matrix across every operation that has been optimized.
//
// WHY A MATRIX RATHER THAN MORE OF THE SAME. The existing concurrency tests exercise reads and
// encodes. Optimization has since touched marker persistence, tail-append fast paths, coallocated
// item blocks, a V2 encoder pool, a text-rendering cache and a delta cache -- each introducing
// state that outlives a single call. Testing one operation kind at a time can only find sharing
// WITHIN that kind. The defect this file exists to catch is sharing BETWEEN kinds: a write path
// that mutates something a read path is caching, or two different fast paths meeting on the same
// global.
//
// That is not hypothetical. The global search-marker timestamp race was found exactly this way --
// by an array insert in one goroutine meeting a text insert in another, on two unrelated documents.
// Neither operation alone was suspicious.
//
// THE CONCURRENCY CONTRACT BEING ASSERTED, stated because the library documents none:
//
//   Concurrent READS of a quiescent document          -- MUST be safe and agree.
//   Concurrent WRITES to SEPARATE documents           -- MUST be safe. Document independence.
//   Reads on one document while another is written    -- MUST be safe. Same reason.
//   Concurrent WRITES to the SAME document            -- NOT claimed, NOT tested. The reference is
//                                                        single-threaded; a Go consumer serialises
//                                                        its own mutations. Asserting it here would
//                                                        promise something we do not implement.
//
// Run with -race. The value assertions hold without it, but the race detector is the point.

func matrixWorkers() int {
	if v := os.Getenv("MATRIX_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 16
}

func matrixRounds() int {
	if v := os.Getenv("MATRIX_ROUNDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 300
}

// ---------------------------------------------------------------- operation inventory

// writeOps is every optimized mutating operation. Each acts on the caller's OWN document, so
// concurrency here tests document independence rather than unsupported same-document writes.
var writeOps = []struct {
	name string
	fn   func(doc *Doc, seed int)
}{
	{"TextInsertTail", func(d *Doc, s int) { t := d.GetText("t"); t.Insert(t.Length(), "x", Object{}) }},
	{"TextInsertMid", func(d *Doc, s int) {
		t := d.GetText("t")
		if t.Length() > 1 {
			t.Insert(s%t.Length(), "m", Object{})
		}
	}},
	{"TextDelete", func(d *Doc, s int) {
		t := d.GetText("t")
		if t.Length() > 2 {
			t.Delete(s%(t.Length()-1), 1)
		}
	}},
	{"TextFormat", func(d *Doc, s int) {
		t := d.GetText("t")
		if t.Length() > 4 {
			a := newObject()
			a.Set("bold", s%2 == 0)
			t.Format(s%(t.Length()-3), 3, a)
		}
	}},
	{"TextInsertEmbed", func(d *Doc, s int) {
		t := d.GetText("t")
		e := newObject()
		e.Set("img", "x")
		t.InsertEmbed(minNumber(s%8, t.Length()), e, Object{})
	}},
	{"TextApplyDelta", func(d *Doc, s int) {
		t := d.GetText("t")
		a := newObject()
		a.Set("bold", s%2 == 0)
		t.ApplyDelta([]EventOperator{NewTextDeltaOp("d", a)}, true)
	}},
	{"ArrayPush", func(d *Doc, s int) { d.GetArray("a").Push(ArrayAny{s % 100}) }},
	{"ArrayUnshift", func(d *Doc, s int) { d.GetArray("a").Unshift(ArrayAny{s % 100}) }},
	{"ArrayInsertTail", func(d *Doc, s int) { a := d.GetArray("a"); a.Insert(a.GetLength(), ArrayAny{s}) }},
	{"ArrayDelete", func(d *Doc, s int) {
		a := d.GetArray("a")
		if a.GetLength() > 1 {
			a.Delete(s%a.GetLength(), 1)
		}
	}},
	{"ArrayNestedInsert", func(d *Doc, s int) {
		n := NewYMap(nil)
		n.Set("k", s%10)
		a := d.GetArray("a")
		a.Insert(a.GetLength(), ArrayAny{n})
	}},
	{"MapSet", func(d *Doc, s int) {
		if m := d.GetMap("m"); m != nil {
			m.Set(fmt.Sprintf("k%d", s%12), s)
		}
	}},
	{"MapDelete", func(d *Doc, s int) {
		if m := d.GetMap("m"); m != nil {
			m.Delete(fmt.Sprintf("k%d", s%12))
		}
	}},
	{"XmlSetAttribute", func(d *Doc, s int) {
		if f := d.GetXmlFragment("x"); f != nil && f.GetLength() > 0 {
			if el, ok := f.Get(0).(*YXmlElement); ok {
				el.SetAttribute("id", strconv.Itoa(s%50))
			}
		}
	}},
	{"BatchedTransaction", func(d *Doc, s int) {
		Transact(d, func(*Transaction) {
			t := d.GetText("t")
			for i := 0; i < 5; i++ {
				t.Insert(t.Length(), "b", Object{})
			}
			a := d.GetArray("a")
			for i := 0; i < 5; i++ {
				a.Push(ArrayAny{i})
			}
		}, nil, true)
	}},
}

// readOps is every optimized read.
//
// KNOWN EXCEPTION, and the reason this comment no longer claims they are all safe: an INDEXED read
var readOps = []struct {
	name string
	fn   func(doc *Doc) string
}{
	{"TextToString", func(d *Doc) string { return d.GetText("t").ToString() }},
	{"TextToJson", func(d *Doc) string { return canonOf(d.GetText("t").ToJson()) }},
	{"TextToDelta", func(d *Doc) string { return deltaSemantic(d.GetText("t").ToDelta(nil, nil, nil)) }},
	{"TextGetAttributes", func(d *Doc) string { return canonOf(d.GetText("t").GetAttributes(nil)) }},
	{"ArrayToArray", func(d *Doc) string { return canonOf(d.GetArray("a").ToArray()) }},
	{"ArrayToJson", func(d *Doc) string { return canonOf(d.GetArray("a").ToJson()) }},
	{"ArrayForEach", func(d *Doc) string {
		n := 0
		d.GetArray("a").ForEach(func(interface{}, Number, *YArray) { n++ })
		return strconv.Itoa(n)
	}},
	{"ArrayGet", func(d *Doc) string {
		a := d.GetArray("a")
		if a.GetLength() == 0 {
			return ""
		}
		// Index zero bypasses position lookup. Keep this above zero so the concurrency matrix
		// exercises the immutable indexed-read cache rather than its trivial early return.
		return canonOf(a.Get(minNumber(1, a.GetLength()-1)))
	}},
	{"ArraySplice", func(d *Doc) string {
		a := d.GetArray("a")
		if a.GetLength() < 2 {
			return ""
		}
		return canonOf(a.Splice(0, a.GetLength()-1))
	}},
	{"ArrayMap", func(d *Doc) string {
		return canonOf(d.GetArray("a").Map(
			func(v interface{}, _ Number, _ *YArray) interface{} { return v }))
	}},
	{"ArrayRange", func(d *Doc) string {
		n := 0
		d.GetArray("a").rangeItems(func(it *itemStruct) { n += it.length })
		return strconv.Itoa(n)
	}},
	{"MapKeys", func(d *Doc) string { return fmt.Sprintf("%d", len(d.GetMap("m").Keys())) }},
	{"MapValues", func(d *Doc) string { return fmt.Sprintf("%d", len(d.GetMap("m").Values())) }},
	{"MapEntries", func(d *Doc) string { return fmt.Sprintf("%d", len(d.GetMap("m").Entries())) }},
	{"MapToJson", func(d *Doc) string { return canonOf(d.GetMap("m").ToJson()) }},
	{"MapGetSize", func(d *Doc) string { return strconv.Itoa(d.GetMap("m").GetSize()) }},
	{"MapHas", func(d *Doc) string { return fmt.Sprintf("%v", d.GetMap("m").Has("k1")) }},
	{"XmlToString", func(d *Doc) string { return d.GetXmlFragment("x").ToString() }},
	{"XmlQuerySelectorAll", func(d *Doc) string {
		return strconv.Itoa(len(d.GetXmlFragment("x").QuerySelectorAll("div")))
	}},
	{"XmlTreeWalker", func(d *Doc) string {
		w := d.GetXmlFragment("x").CreateTreeWalker(func(SharedType) bool { return true })
		n := 0
		for x := w.Next(); x != nil; x = w.Next() {
			n++
		}
		return strconv.Itoa(n)
	}},
	{"EncodeV1", func(d *Doc) string {
		b, err := EncodeStateAsUpdate(d, nil)
		if err != nil {
			return "ERR"
		}
		return strconv.Itoa(len(b))
	}},
	{"EncodeV2", func(d *Doc) string {
		b, err := EncodeStateAsUpdateV2(d, nil)
		if err != nil {
			return "ERR"
		}
		return strconv.Itoa(len(b))
	}},
}

// canonOf renders any read result BY VALUE. fmt.Sprintf("%v") must never be used for a result that
// may contain an Object: Object's backing store is a pointer, so %v prints an allocation ADDRESS,
// and two semantically identical results print differently every time. That mistake reported
// sixteen concurrent readers as diverging on identical content -- twice -- so the rule is encoded
// here rather than left to discipline at each call site.
func canonOf(v interface{}) string {
	wrap := newObject()
	wrap.Set("v", v)
	cn, err := fuzzCanon(wrap)
	if err != nil {
		return "ERR:" + err.Error()
	}
	return cn
}

func buildMatrixDoc(client int) *Doc {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(Number(client)))
	txt := doc.GetText("t")
	txt.Insert(0, "seed text for the matrix", Object{})
	a := newObject()
	a.Set("bold", true)
	txt.Format(0, 4, a)
	arr := doc.GetArray("a")
	m := doc.GetMap("m")
	f := doc.GetXmlFragment("x")
	for i := 0; i < 12; i++ {
		arr.Insert(arr.GetLength(), ArrayAny{i})
		m.Set(fmt.Sprintf("k%d", i), i)
		el := NewYXmlElement("div")
		el.SetAttribute("id", strconv.Itoa(i))
		f.Insert(f.GetLength(), ArrayAny{el})
	}
	return doc
}

// TestConcurrentAllReadsRacing runs EVERY optimized read concurrently against one quiescent
// document. Different read kinds interleave, so a cache populated by one is observed by another.
func TestConcurrentAllReadsRacing(t *testing.T) {
	doc := buildMatrixDoc(1)
	want := make([]string, len(readOps))
	for i, op := range readOps {
		want[i] = op.fn(doc) // single-threaded baseline, before any concurrency
	}

	workers, rounds := matrixWorkers(), matrixRounds()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var bad []string

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				i := (w + r) % len(readOps) // each worker starts at a different op
				if got := readOps[i].fn(doc); got != want[i] {
					mu.Lock()
					bad = append(bad, fmt.Sprintf("%s: want %q got %q", readOps[i].name, want[i], got))
					mu.Unlock()
					return
				}
			}
		}(w)
	}
	wg.Wait()
	for i, b := range bad {
		if i < 3 {
			t.Error(b)
		}
	}
	t.Logf("MATRIX_READS ops=%d workers=%d rounds=%d failures=%d",
		len(readOps), workers, rounds, len(bad))
}

// TestConcurrentAllWritesIndependentDocs runs EVERY optimized write concurrently, each worker on
// its OWN document. This is the document-independence assertion, and it is the shape that exposed
// the global marker-timestamp race: different write kinds meeting on shared package state.
func TestConcurrentAllWritesIndependentDocs(t *testing.T) {
	workers, rounds := matrixWorkers(), matrixRounds()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var bad []string

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			doc := buildMatrixDoc(w + 1)
			for r := 0; r < rounds; r++ {
				op := writeOps[(w+r)%len(writeOps)]
				op.fn(doc, w*991+r)
			}
			// The document must still encode and round-trip to its own content.
			enc, err := EncodeStateAsUpdateV2(doc, nil)
			if err != nil {
				mu.Lock()
				bad = append(bad, fmt.Sprintf("worker %d: encode: %v", w, err))
				mu.Unlock()
				return
			}
			fresh := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(Number(900+w)))
			_ = ApplyUpdateV2(fresh, enc, nil)
			if got, want := fresh.GetText("t").ToString(), doc.GetText("t").ToString(); got != want {
				mu.Lock()
				bad = append(bad, fmt.Sprintf("worker %d: round-trip text differs\n want %q\n got  %q",
					w, want, got))
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()
	for i, b := range bad {
		if i < 3 {
			t.Error(b)
		}
	}
	t.Logf("MATRIX_WRITES ops=%d workers=%d rounds=%d failures=%d",
		len(writeOps), workers, rounds, len(bad))
}

// TestConcurrentMixedOpsRacing is the widest interleaving: some workers read a shared quiescent
// document while others write their own, so read paths and write paths run simultaneously on
// different documents. Any state shared between a read fast path and a write fast path -- a global,
// a pool, a cache keyed on something process-wide -- surfaces here and in none of the single-kind
// tests above.
func TestConcurrentMixedOpsRacing(t *testing.T) {
	shared := buildMatrixDoc(1)
	want := make([]string, len(readOps))
	for i, op := range readOps {
		want[i] = op.fn(shared)
	}

	workers, rounds := matrixWorkers(), matrixRounds()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var bad []string

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			if w%2 == 0 { // reader: shared document, must never diverge
				for r := 0; r < rounds; r++ {
					i := (w + r) % len(readOps)
					if got := readOps[i].fn(shared); got != want[i] {
						mu.Lock()
						bad = append(bad, fmt.Sprintf("reader %d %s: want %q got %q",
							w, readOps[i].name, want[i], got))
						mu.Unlock()
						return
					}
				}
				return
			}
			// writer: its own document, mutating while the readers above are reading
			doc := buildMatrixDoc(w + 1)
			for r := 0; r < rounds; r++ {
				writeOps[(w+r)%len(writeOps)].fn(doc, w*773+r)
			}
		}(w)
	}
	wg.Wait()
	for i, b := range bad {
		if i < 3 {
			t.Error(b)
		}
	}
	t.Logf("MATRIX_MIXED readOps=%d writeOps=%d workers=%d rounds=%d failures=%d",
		len(readOps), len(writeOps), workers, rounds, len(bad))
}
