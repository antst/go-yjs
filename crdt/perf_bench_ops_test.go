package crdt

import "testing"

// Coverage benchmarks for operations the main suite never measured.
//
// WHY. perf_bench_test.go measured 8 of the 36 operations the oracle's coverage mapping tracks --
// 22%. Everything optimised so far came from that 22%, which meant the remaining 28 operations were
// unmeasured rather than fast. That is the same mistake feature 004 was written to correct, one
// layer over: coverage is a property of the CASES, not of how carefully the cases are run.
//
// It found something immediately. YArray.Push is 4.7x slower than Insert(length) once tombstones
// are present, with the delete pattern held identical between the two -- see the pair below. The
// main suite could not see it because it appends with Insert and never calls Push, even though Push
// is the idiomatic append.
//
// Sizes are deliberately the same perfSmall/perfLarge the main suite uses, so numbers here sit on
// the same scale and can be read against it.
//
// READ THE MAP/ARRAY READ-PROJECTION ROWS AS GC MEASUREMENTS. On the rows that return a freshly
// allocated projection, most of the time is garbage collection driven by that allocation, not the
// work of building the result. Measured by sweeping GOGC on arm64 (ns/op):
//
//	row            GOGC=100  GOGC=1000  GOGC=off   GC share    B/op  allocs
//	MapToJson         46872      13408     11538        71%  196936      13
//	MapEntries        35596      11157      9223        69%  164053      10
//	MapKeys            5747       1851      1840        68%   32768       1
//	ArrayToArray       4056       1819         -        55%   32792       2
//	ArrayToJson        7834       5608         -        28%   32792       2
//	MapValues         27445      23969         -        13%   32792       2
//	TextToString      2.272      2.276         -         0%       0       0
//
// TextToString allocates nothing and does not move, which is the control. For MapKeys the
// GOGC=1000 and GOGC=off columns agree (1851 vs 1840), so this is collection happening inside the
// measured window, not work deferred past it -- at 580k iterations x 32 KB the run allocates ~19 GB.
//
// Two consequences. Removing an allocation on these rows is worth roughly three times its direct
// cost, because it also removes the collection it forces: an append-style variant writing into a
// caller-reused buffer measures ~400 ns against MapKeys' 5,376, where subtracting only the copy
// predicts ~1,900. And the reference implementations these rows are compared against have no
// garbage collector at all (yrs) or a very different one (yjs), so a bad ratio HERE is a
// story about allocation rather than about the algorithm. Check the allocs column before
// optimising the traversal.
//
// WHAT IS LEFT IF THE COPY GOES. These rows are already cached; the cache makes the PROJECTION
// free and everything measured is the defensive copy that hands the caller its own storage.
// Replacing the cached-path return with the cached value itself (unsafe — callers could then
// mutate the cache — and done only to price the copy), same process, same session:
//
//	row           with copy   copy removed   copy share
//	MapToJson       46701 ns       2.140 ns      99.995%
//	MapEntries      34816 ns       1.992 ns      99.99%
//	MapKeys          5559 ns       2.061 ns      99.96%
//
// So the target on this surface is the OWNERSHIP MODEL, not the traversal, the cache or the clone
// algorithm — copy-on-write shared storage would collect essentially all of it. Object can support
// that because it is our type; Keys returns []string and Entries returns map[string]interface{},
// which a caller can mutate with no hook, so those two need either an owned wrapper type or an
// append-style API where the caller supplies the storage.

// ---------------------------------------------------------------- array

func BenchmarkArrayPush(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a := perfDoc().GetArray("a")
		for j := 0; j < perfSmall; j++ {
			a.Push(ArrayAny{j})
		}
	}
}

// BenchmarkArrayPushWithTombstones and BenchmarkArrayInsertEndWithTombstones are a MATCHED PAIR and
// must be read together. Identical work and an identical delete pattern; the only difference is
// Push versus Insert(length). Any gap between them is attributable to the append call alone.
//
// The gap is real: Push walks the item chain to its true last element, tombstones included, and a
// deleted item cannot merge with a live one (MergeWith requires equal Deleted()), so the chain
// grows to N and every Push traverses it. Plain Push is fast only because an all-live run merges
// down to a single item and the walk is trivial.
//
// This is inherited, not a porting defect: our typeListPushGenerics starts from the highest-index
// search marker exactly as yjs's typeListPushGenerics does, and yjs measures the same 4.2x
// degradation on the same shapes. It is an opportunity rather than a regression -- Push never
// PERSISTS a marker, only reads one, so a pure-append workload never builds the cache that would
// make it cheap. Markers are not wire-observable, so that is fixable without touching the format.
func BenchmarkArrayPushWithTombstones(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a := perfDoc().GetArray("a")
		for j := 0; j < perfSmall; j++ {
			a.Push(ArrayAny{j})
			if j%2 == 1 && a.GetLength() > 0 {
				a.Delete(a.GetLength()-1, 1)
			}
		}
	}
}

func BenchmarkArrayInsertEndWithTombstones(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a := perfDoc().GetArray("a")
		for j := 0; j < perfSmall; j++ {
			a.Insert(a.GetLength(), ArrayAny{j})
			if j%2 == 1 && a.GetLength() > 0 {
				a.Delete(a.GetLength()-1, 1)
			}
		}
	}
}

func BenchmarkArrayUnshift(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		a := perfDoc().GetArray("a")
		for j := 0; j < perfSmall; j++ {
			a.Unshift(ArrayAny{j})
		}
	}
}

func benchArray() *YArray {
	n := perfSmall
	a := perfDoc().GetArray("a")
	for j := 0; j < n; j++ {
		a.Insert(a.GetLength(), ArrayAny{j})
	}
	return a
}

func BenchmarkArrayToArray(b *testing.B) {
	a := benchArray()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = a.ToArray()
	}
}

func BenchmarkArrayToJson(b *testing.B) {
	a := benchArray()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = a.ToJSON()
	}
}

func BenchmarkArrayForEach(b *testing.B) {
	a := benchArray()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		a.ForEach(func(interface{}, Number, *YArray) { n++ })
		benchSink += n
	}
}

// Random Get, the read a list consumer actually performs. Sequential Get would ride the search
// marker and measure the marker rather than the lookup.
func BenchmarkArrayGetRandom(b *testing.B) {
	a := benchArray()
	rng := perfRand()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < perfSmall; j++ {
			benchSinkAny = a.Get(rng.intn(perfSmall))
		}
	}
}

// ---------------------------------------------------------------- map

func benchMap() *YMap {
	n := perfSmall
	m := perfDoc().GetMap("m")
	for j := 0; j < n; j++ {
		m.Set(mapKey(j), j)
	}
	return m
}

func mapKey(j int) string {
	return "k" + string(rune('a'+j%26)) + string(rune('a'+(j/26)%26)) + string(rune('a'+(j/676)%26))
}

func BenchmarkMapKeys(b *testing.B) {
	m := benchMap()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkStrs = m.Keys()
	}
}

func BenchmarkMapValues(b *testing.B) {
	m := benchMap()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = m.Values()
	}
}

func BenchmarkMapEntries(b *testing.B) {
	m := benchMap()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = m.Entries()
	}
}

func BenchmarkMapToJson(b *testing.B) {
	m := benchMap()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = m.ToJSON()
	}
}

// A full sweep of perfSmall lookups, not one. A single Has is far below timer resolution, and the
// JS and Rust counterparts sweep too -- timing one call here against a 2000-call sweep there
// produced a meaningless 4000x, which is the kind of number that looks like a triumph and is
// actually a harness defect.
func BenchmarkMapHas(b *testing.B) {
	m := benchMap()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < perfSmall; j++ {
			benchSinkBool = m.Has(mapKey(j))
		}
	}
}

func BenchmarkMapClear(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		m := benchMap()
		b.StartTimer()
		m.Clear()
	}
}

// ---------------------------------------------------------------- text reads

func benchText() *YText {
	n := perfSmall
	t := perfDoc().GetText("t")
	t.Insert(0, perfString(n), Object{})
	return t
}

// ToString and ToJSON are what a consumer calls on every render, and neither was measured. ToDelta
// was -- and it is one of only three rows in the whole comparison where we lose -- so its untested
// siblings are the first place to look for the same cost.
func BenchmarkTextToString(b *testing.B) {
	t := benchText()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkString = t.ToString()
	}
}

func BenchmarkTextToJson(b *testing.B) {
	t := benchText()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = t.ToJSON()
	}
}

// Formatted, so ToString walks a fragmented item chain rather than one merged run -- the state a
// rich-text consumer is actually in.
func BenchmarkTextToStringFormatted(b *testing.B) {
	t := benchText()
	rng := perfRand()
	for j := 0; j < 500; j++ {
		attr := newObject()
		attr.Set("bold", j%2 == 0)
		t.Format(rng.intn(perfSmall-20), 20, attr)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkString = t.ToString()
	}
}

func BenchmarkTextInsertEmbed(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		t := benchText()
		for j := 0; j < 200; j++ {
			embed := newObject()
			embed.Set("img", "x")
			t.InsertEmbed(j, embed, Object{})
		}
	}
}
