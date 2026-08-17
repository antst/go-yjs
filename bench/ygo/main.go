// ygo benchmarks defined to match this project's Go suite operation-for-operation, so the fourth
// implementation can be compared on the same hardware in the same session.
//
// This is a SEPARATE Go module on purpose. reearth/ygo pulls in SQLite, Redis and a WebSocket
// library; making it a dependency of the library under test would put all of that into our go.sum
// for the sake of a benchmark. Nothing here is importable from the root module.
//
// Why a harness at all: ygo publishes numbers in its own BENCHMARKS.md, but those were measured on
// an Apple M4 Max while ours are on an M1 Max, so the two tables cannot be compared. The only way
// to get a defensible ratio is to run both on one machine, which is what this enables.
//
// Same 32-bit LCG as the Go, JS and Rust harnesses, so every implementation draws the IDENTICAL
// index sequence rather than merely being both-random. Setup is excluded from timed regions.
//
// One structural note that shapes every mutation below: ygo requires an EXPLICIT *Transaction on
// every mutating call — there is no implicit-transaction convenience method. yjs (and this library)
// create an implicit transaction per operation, so the faithful mapping is one Transact per
// operation, which is what these benchmarks do. Batching them into one transaction would measure a
// different workload than the other three harnesses run.
package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/reearth/ygo/crdt"
)

type lcg struct{ s uint32 }

func newLCG() *lcg { return &lcg{s: 42} }
func (r *lcg) next() uint32 {
	r.s = r.s*1664525 + 1013904223
	return r.s
}
func (r *lcg) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint32(n))
}

func newDoc() *crdt.Doc {
	return crdt.New(crdt.WithClientID(1), crdt.WithGC(false))
}

func ascii(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return string(b)
}

func report(name string, iters int, d time.Duration) {
	fmt.Printf("%-28s%14.0f ns/op   (iters=%d)\n", name, float64(d.Nanoseconds())/float64(iters), iters)
}

// bench times f over iters whole iterations; f builds and mutates its own document.
func bench(name string, iters int, f func()) {
	f() // warmup
	start := time.Now()
	for i := 0; i < iters; i++ {
		f()
	}
	report(name, iters, time.Since(start))
}

func textAppend(n int) {
	doc := newDoc()
	t := doc.GetText("t")
	for j := 0; j < n; j++ {
		doc.Transact(func(txn *crdt.Transaction) { t.Insert(txn, j, "x", nil) })
	}
}

func textInsertRandom(n int) {
	rng := newLCG()
	doc := newDoc()
	t := doc.GetText("t")
	for j := 0; j < n; j++ {
		idx := rng.intn(j + 1)
		doc.Transact(func(txn *crdt.Transaction) { t.Insert(txn, idx, "y", nil) })
	}
}

func builtDoc(n int) *crdt.Doc {
	rng := newLCG()
	doc := newDoc()
	t := doc.GetText("t")
	for j := 0; j < n; j++ {
		idx := rng.intn(j + 1)
		doc.Transact(func(txn *crdt.Transaction) { t.Insert(txn, idx, "z", nil) })
	}
	return doc
}

// sink defeats dead-code elimination in the callback-driven read benchmarks. The Go suite hit
// exactly this: an empty callback let the compiler prove a traversal had no observable effect and
// delete it, reporting 0.98ns for walking 2000 elements.
var sink int

// mapKey matches the Go, JS and Rust harnesses so every side builds keys of identical length and
// distribution rather than one getting cheap short keys.
func mapKey(j int) string {
	return "k" + string(rune('a'+j%26)) + string(rune('a'+(j/26)%26)) + string(rune('a'+(j/676)%26))
}

// Keys built ONCE, as fixtures -- see the Go suite's perfKeys for why.
var perfKeys = func() []string {
	k := make([]string, 2000)
	for i := range k {
		k[i] = "k" + strconv.Itoa(i)
	}
	return k
}()

func main() {
	fmt.Fprintln(os.Stderr, "ygo — matched workloads, same LCG as the Go, JS and Rust suites")

	bench("TextAppendSmall", 20, func() { textAppend(2000) })
	bench("TextAppendLarge", 5, func() { textAppend(10000) })
	bench("TextInsertRandomSmall", 20, func() { textInsertRandom(2000) })
	bench("TextInsertRandomLarge", 5, func() { textInsertRandom(10000) })

	bench("TextDeleteRandom", 10, func() {
		doc := newDoc()
		t := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) { t.Insert(txn, 0, ascii(10000), nil) })
		rng := newLCG()
		for j := 0; j < 2000; j++ {
			l := t.Len()
			if l < 2 {
				break
			}
			idx := rng.intn(l - 1)
			doc.Transact(func(txn *crdt.Transaction) { t.Delete(txn, idx, 1) })
		}
	})

	bench("TextFormatChurn", 20, func() {
		doc := newDoc()
		t := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) { t.Insert(txn, 0, ascii(2000), nil) })
		rng := newLCG()
		for j := 0; j < 1000; j++ {
			var a crdt.Attributes
			switch j % 3 {
			case 0:
				a = crdt.Attributes{"bold": true}
			case 1:
				a = crdt.Attributes{"italic": true}
			default:
				a = crdt.Attributes{"bold": nil}
			}
			start := rng.intn(2000 - 20)
			doc.Transact(func(txn *crdt.Transaction) { t.Format(txn, start, 20, a) })
		}
	})

	// ToDelta: setup outside the timed region.
	func() {
		doc := newDoc()
		t := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) { t.Insert(txn, 0, ascii(2000), nil) })
		rng := newLCG()
		for j := 0; j < 500; j++ {
			a := crdt.Attributes{"bold": j%2 == 0}
			s := rng.intn(2000 - 20)
			doc.Transact(func(txn *crdt.Transaction) { t.Format(txn, s, 20, a) })
		}
		iters := 2000
		start := time.Now()
		for i := 0; i < iters; i++ {
			_ = t.ToDelta()
		}
		report("TextToDelta", iters, time.Since(start))
	}()

	bench("ArrayInsertSequential", 20, func() {
		doc := newDoc()
		a := doc.GetArray("a")
		for j := 0; j < 2000; j++ {
			doc.Transact(func(txn *crdt.Transaction) { a.Insert(txn, j, []any{j}) })
		}
	})

	bench("MapSet", 20, func() {
		doc := newDoc()
		m := doc.GetMap("m")
		for j := 0; j < 2000; j++ {
			k := perfKeys[j]
			doc.Transact(func(txn *crdt.Transaction) { m.Set(txn, k, j) })
		}
	})

	// Batched variants: identical work, ONE Transact instead of N. This is ygo's IDIOMATIC shape,
	// since its API requires an explicit transaction on every mutation.
	bench("TextAppendLargeBatched", 5, func() {
		doc := newDoc()
		t := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) {
			for j := 0; j < 10000; j++ {
				t.Insert(txn, j, "x", nil)
			}
		})
	})
	bench("ArrayInsertBatched", 20, func() {
		doc := newDoc()
		a := doc.GetArray("a")
		doc.Transact(func(txn *crdt.Transaction) {
			for j := 0; j < 2000; j++ {
				a.Insert(txn, j, []any{j})
			}
		})
	})
	bench("MapSetBatched", 20, func() {
		doc := newDoc()
		m := doc.GetMap("m")
		doc.Transact(func(txn *crdt.Transaction) {
			for j := 0; j < 2000; j++ {
				m.Set(txn, perfKeys[j], j)
			}
		})
	})

	// ---- coverage cases ------------------------------------------------------------------
	// Mirrors perf_bench_ops_test.go / perf_bench_xml_test.go so the read paths have a third
	// reference point. Operations ygo does not expose (Map.Values, Map.Clear, Map size,
	// Text.GetAttributes, XML selectors, XML tree walker, Array.Map) are declared not-applicable
	// in bench/status.py with their reason rather than silently omitted.

	bench("ArrayPush", 20, func() {
		doc := newDoc()
		a := doc.GetArray("a")
		for j := 0; j < 2000; j++ {
			doc.Transact(func(txn *crdt.Transaction) { a.Push(txn, []any{j}) })
		}
	})
	bench("ArrayPushWithTombstones", 20, func() {
		doc := newDoc()
		a := doc.GetArray("a")
		for j := 0; j < 2000; j++ {
			doc.Transact(func(txn *crdt.Transaction) { a.Push(txn, []any{j}) })
			if j%2 == 1 && a.Len() > 0 {
				doc.Transact(func(txn *crdt.Transaction) { a.Delete(txn, a.Len()-1, 1) })
			}
		}
	})
	bench("ArrayInsertEndWithTombstones", 20, func() {
		doc := newDoc()
		a := doc.GetArray("a")
		for j := 0; j < 2000; j++ {
			doc.Transact(func(txn *crdt.Transaction) { a.Insert(txn, a.Len(), []any{j}) })
			if j%2 == 1 && a.Len() > 0 {
				doc.Transact(func(txn *crdt.Transaction) { a.Delete(txn, a.Len()-1, 1) })
			}
		}
	})
	// ygo has no unshift; insert-at-0 is the same operation.
	bench("ArrayUnshift", 20, func() {
		doc := newDoc()
		a := doc.GetArray("a")
		for j := 0; j < 2000; j++ {
			doc.Transact(func(txn *crdt.Transaction) { a.Insert(txn, 0, []any{j}) })
		}
	})

	// Read paths: fixture built once, outside the timed regions.
	func() {
		doc := newDoc()
		a := doc.GetArray("a")
		doc.Transact(func(txn *crdt.Transaction) {
			for j := 0; j < 2000; j++ {
				a.Insert(txn, a.Len(), []any{j})
			}
		})
		iters := 2000

		start := time.Now()
		for i := 0; i < iters; i++ {
			_ = a.ToSlice()
		}
		report("ArrayToArray", iters, time.Since(start))

		start = time.Now()
		for i := 0; i < iters; i++ {
			_, _ = a.ToJSON()
		}
		report("ArrayToJson", iters, time.Since(start))

		start = time.Now()
		for i := 0; i < iters; i++ {
			n := 0
			a.ForEach(func(int, any) { n++ })
			sink += n
		}
		report("ArrayForEach", iters, time.Since(start))

		rng := newLCG()
		start = time.Now()
		for i := 0; i < iters; i++ {
			for j := 0; j < 2000; j++ {
				_ = a.Get(rng.intn(2000))
			}
		}
		report("ArrayGetRandom", iters, time.Since(start))

		start = time.Now()
		for i := 0; i < iters; i++ {
			_ = a.Slice(0, 1999)
		}
		report("ArraySplice", iters, time.Since(start))
	}()

	// Map reads.
	func() {
		doc := newDoc()
		m := doc.GetMap("m")
		doc.Transact(func(txn *crdt.Transaction) {
			for j := 0; j < 2000; j++ {
				m.Set(txn, mapKey(j), j)
			}
		})
		iters := 2000

		start := time.Now()
		for i := 0; i < iters; i++ {
			_ = m.Keys()
		}
		report("MapKeys", iters, time.Since(start))

		start = time.Now()
		for i := 0; i < iters; i++ {
			_ = m.Entries()
		}
		report("MapEntries", iters, time.Since(start))

		start = time.Now()
		for i := 0; i < iters; i++ {
			_, _ = m.ToJSON()
		}
		report("MapToJson", iters, time.Since(start))

		start = time.Now()
		for i := 0; i < iters; i++ {
			for j := 0; j < 2000; j++ {
				if m.Has(mapKey(j)) {
					sink++
				}
			}
		}
		report("MapHas", iters, time.Since(start))
	}()

	// Text reads.
	func() {
		doc := newDoc()
		t := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) { t.Insert(txn, 0, ascii(2000), nil) })
		iters := 2000

		start := time.Now()
		for i := 0; i < iters; i++ {
			_ = t.ToString()
		}
		report("TextToString", iters, time.Since(start))

		start = time.Now()
		for i := 0; i < iters; i++ {
			_, _ = t.ToJSON()
		}
		report("TextToJson", iters, time.Since(start))
	}()

	// Formatted rendering: the state a rich-text consumer is actually in.
	func() {
		doc := newDoc()
		t := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) { t.Insert(txn, 0, ascii(2000), nil) })
		rng := newLCG()
		for j := 0; j < 500; j++ {
			a := crdt.Attributes{"bold": j%2 == 0}
			s := rng.intn(2000 - 20)
			doc.Transact(func(txn *crdt.Transaction) { t.Format(txn, s, 20, a) })
		}
		iters := 2000
		start := time.Now()
		for i := 0; i < iters; i++ {
			_ = t.ToString()
		}
		report("TextToStringFormatted", iters, time.Since(start))
	}()

	bench("TextInsertEmbed", 20, func() {
		doc := newDoc()
		t := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) { t.Insert(txn, 0, ascii(2000), nil) })
		for j := 0; j < 200; j++ {
			doc.Transact(func(txn *crdt.Transaction) {
				t.InsertEmbed(txn, j, map[string]any{"img": "x"}, nil)
			})
		}
	})

	// XML: the subset ygo exposes.
	func() {
		doc := newDoc()
		f := doc.GetXmlFragment("x")
		doc.Transact(func(txn *crdt.Transaction) {
			for i := 0; i < 500; i++ {
				name := "div"
				if i%3 == 0 {
					name = "span"
				}
				el := crdt.NewYXmlElement(name)
				el.SetAttribute(txn, "id", mapKey(i))
				el.SetAttribute(txn, "class", "row")
				f.InsertElement(txn, f.Len(), el)
			}
		})
		iters := 2000

		start := time.Now()
		for i := 0; i < iters; i++ {
			_ = f.ToXML()
		}
		report("XmlToString", iters, time.Since(start))

		start = time.Now()
		for i := 0; i < iters; i++ {
			if c := f.Children(); len(c) > 0 {
				sink++
			}
		}
		report("XmlGetFirstChild", iters, time.Since(start))
	}()

	// Codec on a 10k-op document, built once outside the timed regions.
	func() {
		doc := builtDoc(10000)
		v1 := doc.EncodeStateAsUpdate()
		v2 := crdt.EncodeStateAsUpdateV2(doc, nil)
		iters := 2000

		start := time.Now()
		for i := 0; i < iters; i++ {
			_ = doc.EncodeStateAsUpdate()
		}
		report("EncodeV1", iters, time.Since(start))

		start = time.Now()
		for i := 0; i < iters; i++ {
			_ = crdt.EncodeStateAsUpdateV2(doc, nil)
		}
		report("EncodeV2", iters, time.Since(start))

		start = time.Now()
		for i := 0; i < iters; i++ {
			_ = crdt.ApplyUpdateV1(newDoc(), v1, nil)
		}
		report("ApplyV1", iters, time.Since(start))

		start = time.Now()
		for i := 0; i < iters; i++ {
			_ = crdt.ApplyUpdateV2(newDoc(), v2, nil)
		}
		report("ApplyV2", iters, time.Since(start))
	}()

	// Concurrent merge: two independently-edited documents applied into one.
	func() {
		mk := func(client uint64, tag string) []byte {
			d := crdt.New(crdt.WithClientID(crdt.ClientID(client)), crdt.WithGC(false))
			t := d.GetText("t")
			rng := newLCG()
			for j := 0; j < 2000; j++ {
				idx := rng.intn(j + 1)
				d.Transact(func(txn *crdt.Transaction) { t.Insert(txn, idx, tag, nil) })
			}
			return d.EncodeStateAsUpdate()
		}
		u1, u2 := mk(1, "a"), mk(2, "b")
		iters := 500
		start := time.Now()
		for i := 0; i < iters; i++ {
			d := newDoc()
			_ = crdt.ApplyUpdateV1(d, u1, nil)
			_ = crdt.ApplyUpdateV1(d, u2, nil)
		}
		report("ConcurrentMerge", iters, time.Since(start))
	}()

	// ygo-shaped: ONE random single-char insert into a ~100k-char text, at fixed counts. This is
	// the benchmark ygo publishes; only the fixed-10 point is comparable to their figure.
	for _, iters := range []int{10, 1000, 10000} {
		doc := newDoc()
		t := doc.GetText("t")
		doc.Transact(func(txn *crdt.Transaction) { t.Insert(txn, 0, ascii(100000), nil) })
		rng := newLCG()
		start := time.Now()
		for i := 0; i < iters; i++ {
			idx := rng.intn(t.Len())
			doc.Transact(func(txn *crdt.Transaction) { t.Insert(txn, idx, "x", nil) })
		}
		report("YText_RandomInsert_100k", iters, time.Since(start))
	}
}
