package crdt

import (
	"strconv"
	"testing"
)

// Cross-implementation performance suite.
//
// Every benchmark here has a byte-for-byte counterpart in `fuzz/perf_bench.mjs` running the same
// workload against yjs@13.6.31, so the numbers are comparable rather than merely collected. The
// scenarios are chosen to hit the places CRDT ports are known to regress — the same shapes that
// have bitten y-rs — rather than to flatter an average case:
//
//   - sequential append: the search-marker fast path. If markers are broken or absent this is
//     O(n^2) and diverges catastrophically with size, which is why it is run at two sizes.
//   - random-position insert: marker-HOSTILE. Every op lands far from the cached marker, so this
//     measures the raw item-list walk.
//   - format churn: overlapping format ranges, driving cleanupYTextFormatting and the negation
//     pre-pass — the most allocation-heavy path in the library.
//   - encode / decode / merge at size: the codec throughput that dominates real sync workloads.
//   - map and array at size: the non-text containers, whose costs are usually forgotten.
//
// Deterministic input everywhere (fixed seed, pinned ClientID) so runs are comparable across
// machines and across the two implementations.

const (
	perfSmall = 2000
	perfLarge = 10000
)

// perfLCG is a 32-bit LCG reimplemented identically in `fuzz/perf_bench.mjs`, so both
// implementations draw the SAME index sequence. Go's math/rand is not reproducible across
// languages, so using it would leave the two suites merely "both random" rather than running the
// same workload — and random-position insert costs depend heavily on WHERE the indices land.
type perfLCG struct{ s uint32 }

func newPerfLCG() *perfLCG { return &perfLCG{s: 42} }

func (r *perfLCG) next() uint32 {
	r.s = r.s*1664525 + 1013904223
	return r.s
}

// intn mirrors the JS side's `rng() % n`, including its modulo bias — matching the reference
// harness exactly matters more here than uniformity, since both sides must pick the same indices.
func (r *perfLCG) intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint32(n))
}

func perfRand() *perfLCG { return newPerfLCG() }

func perfDoc() *Doc {
	return newDoc("bench", false, defaultGCFilter, nil, false, WithClientID(1))
}

// perfSinkDoc is the RECEIVING document for the apply/merge benchmarks. It must not share a
// ClientID with the update being applied: the library then reassigns the client id and logs
// "[crdt] Changed the client-id…", so the timed region would include a reassignment and an I/O
// write that a real consumer never pays. Matching the JS side, which uses a fresh Y.Doc with its
// own random clientID.
func perfSinkDoc() *Doc {
	return newDoc("bench", false, defaultGCFilter, nil, false, WithClientID(999))
}

// ---------------------------------------------------------------- text: sequential append

// workloadTextAppend is shared with the allocation anchor test so the number
// that test pins is the number this benchmark measures. Two copies of a
// workload would let the anchor certify code the benchmark no longer runs.
func workloadTextAppend(n int) {
	doc := perfDoc()
	txt := doc.GetText("t")
	for j := 0; j < n; j++ {
		txt.Insert(txt.Length(), "x", Object{})
	}
}

func benchAppend(b *testing.B, n int) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		workloadTextAppend(n)
	}
}

func BenchmarkTextAppendSmall(b *testing.B) { benchAppend(b, perfSmall) }
func BenchmarkTextAppendLarge(b *testing.B) { benchAppend(b, perfLarge) }

// ---------------------------------------------------------------- text: random-position insert

func benchInsertRandom(b *testing.B, n int) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rng := perfRand()
		doc := perfDoc()
		txt := doc.GetText("t")
		for j := 0; j < n; j++ {
			idx := 0
			if l := txt.Length(); l > 0 {
				idx = rng.intn(l + 1)
			}
			txt.Insert(idx, "y", Object{})
		}
	}
}

func BenchmarkTextInsertRandomSmall(b *testing.B) { benchInsertRandom(b, perfSmall) }
func BenchmarkTextInsertRandomLarge(b *testing.B) { benchInsertRandom(b, perfLarge) }

// ---------------------------------------------------------------- text: random delete

func BenchmarkTextDeleteRandom(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := perfDoc()
		txt := doc.GetText("t")
		txt.Insert(0, string(make([]byte, 0, perfLarge))+perfString(perfLarge), Object{})
		rng := perfRand()
		b.StartTimer()
		for j := 0; j < perfSmall; j++ {
			l := txt.Length()
			if l < 2 {
				break
			}
			txt.Delete(rng.intn(l-1), 1)
		}
	}
}

// ---------------------------------------------------------------- text: format churn

func BenchmarkTextFormatChurn(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		doc := perfDoc()
		txt := doc.GetText("t")
		txt.Insert(0, perfString(perfSmall), Object{})
		rng := perfRand()
		for j := 0; j < 1000; j++ {
			attr := newObject()
			switch j % 3 {
			case 0:
				attr.Set("bold", true)
			case 1:
				attr.Set("italic", true)
			case 2:
				attr.Set("bold", Null)
			}
			start := rng.intn(perfSmall - 20)
			txt.Format(start, 20, attr)
		}
	}
}

// ---------------------------------------------------------------- text: ToDelta read path

func BenchmarkTextToDelta(b *testing.B) {
	doc := perfDoc()
	txt := doc.GetText("t")
	txt.Insert(0, perfString(perfSmall), Object{})
	rng := perfRand()
	for j := 0; j < 500; j++ {
		attr := newObject()
		attr.Set("bold", j%2 == 0)
		txt.Format(rng.intn(perfSmall-20), 20, attr)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkOps = txt.ToDelta(nil, nil, nil)
	}
}

// ---------------------------------------------------------------- array / map at size

func workloadArrayInsertSequential() {
	doc := perfDoc()
	arr := doc.GetArray("a")
	for j := 0; j < perfSmall; j++ {
		arr.Insert(arr.GetLength(), ArrayAny{j})
	}
}

func BenchmarkArrayInsertSequential(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		workloadArrayInsertSequential()
	}
}

// perfKeys is the 2000 map keys, built ONCE outside every timed region. Constructing keys inside
// the loop made the MapSet rows partly a comparison of each language's string formatting: Go and
// ygo used fmt.Sprintf, yrs used format!, and yjs used direct concatenation, which is far cheaper.
// Roughly 8-12% of Go's row was formatting rather than CRDT work. Inputs are fixtures.
var perfKeys = func() []string {
	k := make([]string, perfSmall)
	for i := range k {
		k[i] = "k" + strconv.Itoa(i)
	}
	return k
}()

func workloadMapSet() {
	doc := perfDoc()
	m := doc.GetMap("m")
	for j := 0; j < perfSmall; j++ {
		m.Set(perfKeys[j], j)
	}
}

func BenchmarkMapSet(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		workloadMapSet()
	}
}

// ---------------------------------------------------------------- codec throughput

func perfBuiltDoc() *Doc {
	n := perfLarge
	doc := perfDoc()
	txt := doc.GetText("t")
	rng := perfRand()
	for j := 0; j < n; j++ {
		idx := 0
		if l := txt.Length(); l > 0 {
			idx = rng.intn(l + 1)
		}
		txt.Insert(idx, "z", Object{})
	}
	return doc
}

func BenchmarkEncodeV1(b *testing.B) {
	doc := perfBuiltDoc()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := EncodeStateAsUpdate(doc, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeV2(b *testing.B) {
	doc := perfBuiltDoc()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := EncodeStateAsUpdateV2(doc, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkApplyV1(b *testing.B) {
	benchReleaseSinks(b)
	upd, err := EncodeStateAsUpdate(perfBuiltDoc(), nil)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkErr = ApplyUpdate(perfSinkDoc(), upd, nil)
	}
}

func BenchmarkApplyV2(b *testing.B) {
	benchReleaseSinks(b)
	upd, err := EncodeStateAsUpdateV2(perfBuiltDoc(), nil)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkErr = ApplyUpdateV2(perfSinkDoc(), upd, nil)
	}
}

// Concurrent merge: two independently-edited documents exchanged both ways. This is the shape a
// real sync session has, and it exercises integration against an existing item list rather than
// the empty-document fast path the apply benchmarks above measure.
func BenchmarkConcurrentMerge(b *testing.B) {
	benchReleaseSinks(b)
	mk := func(client Number, tag string) []uint8 {
		doc := newDoc("bench", false, defaultGCFilter, nil, false, WithClientID(client))
		txt := doc.GetText("t")
		rng := perfRand()
		for j := 0; j < perfSmall; j++ {
			idx := 0
			if l := txt.Length(); l > 0 {
				idx = rng.intn(l + 1)
			}
			txt.Insert(idx, tag, Object{})
		}
		u, err := EncodeStateAsUpdate(doc, nil)
		if err != nil {
			b.Fatal(err)
		}
		return u
	}
	u1, u2 := mk(1, "a"), mk(2, "b")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := perfSinkDoc()
		benchSinkErr = ApplyUpdate(d, u1, nil)
		benchSinkErr = ApplyUpdate(d, u2, nil)
	}
}

func perfString(n int) string {
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte('a' + i%26)
	}
	return string(buf)
}

// Batched append: the SAME 10 000 inserts, but inside one transaction instead of 10 000 of them.
//
// The allocation profile of BenchmarkTextAppendLarge attributes 98% of allocations to Transact,
// 47% to cleanupTransactions alone — i.e. the dominant cost is per-TRANSACTION, not per-character.
// This benchmark isolates that: if the two differ by a large factor, the headline "insert" cost is
// really transaction overhead and a consumer batching its edits does not pay it.
func BenchmarkTextAppendLargeBatched(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		doc := perfDoc()
		txt := doc.GetText("t")
		Transact(doc, func(trans *Transaction) {
			for j := 0; j < perfLarge; j++ {
				txt.Insert(txt.Length(), "x", Object{})
			}
		}, nil, true)
	}
}

// ---------------------------------------------------------------- reearth/ygo comparison
//
// Two benchmarks defined to match reearth/ygo's BENCHMARKS.md exactly, so the numbers are
// comparable to a published third Go implementation rather than only to the JS reference:
//
//	YText_RandomInsert_100k — one single-character insert at a uniformly random position into a
//	                          ~100k-character Y.Text, wrapped in a transaction.
//	YArray_RandomGet_100k   — one positional Get at a uniformly random index into a 100k-element
//	                          Y.Array, no transaction.
//
// Both do ONE operation per b.N iteration, with the document built in untimed setup. ygo published
// theirs on an Apple M4 Max; these run on an M1 Max, so ygo's hardware is materially faster and a
// raw number-to-number comparison understates us. `fuzz/perf_bench.mjs` carries the same two
// against yjs so at least the Go-vs-JS half is same-machine.
const perfHundredK = 100000

func BenchmarkYText_RandomInsert_100k(b *testing.B) {
	doc := perfDoc()
	txt := doc.GetText("t")
	txt.Insert(0, perfString(perfHundredK), Object{})
	rng := perfRand()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		txt.Insert(rng.intn(txt.Length()), "x", Object{})
	}
}

func BenchmarkYArray_RandomGet_100k(b *testing.B) {
	doc := perfDoc()
	arr := doc.GetArray("a")
	vals := make(ArrayAny, perfHundredK)
	for i := range vals {
		vals[i] = i
	}
	arr.Insert(0, vals)
	rng := perfRand()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchSinkAny = arr.Get(rng.intn(perfHundredK))
	}
}

// Batched variants of the container benchmarks. Same total work as their per-op counterparts,
// wrapped in ONE transaction instead of N.
//
// This is not a micro-optimisation question, it is a comparison-fairness one. ygo requires an
// explicit transaction on every mutation and therefore steers consumers toward batching, so its
// per-op numbers measure a usage its own API discourages. Measuring both shapes on all four
// implementations is the only way to say which cost belongs to the design and which to the calling
// convention.
//
// Note the Yjs family's counter-intuitive result here: batching is SLOWER, because adjacent items
// merge at transaction commit, so one long transaction leaves the item list unmerged for every
// subsequent walk within it.

func BenchmarkArrayInsertBatched(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		doc := perfDoc()
		arr := doc.GetArray("a")
		Transact(doc, func(trans *Transaction) {
			for j := 0; j < perfSmall; j++ {
				arr.Insert(arr.GetLength(), ArrayAny{j})
			}
		}, nil, true)
	}
}

func BenchmarkMapSetBatched(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		doc := perfDoc()
		m := doc.GetMap("m")
		Transact(doc, func(trans *Transaction) {
			for j := 0; j < perfSmall; j++ {
				m.Set(perfKeys[j], j)
			}
		}, nil, true)
	}
}
