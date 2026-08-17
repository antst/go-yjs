package crdt

import "testing"

// Push interleaved with front-deletion, which is the shape the split fix does
// NOT obviously help and could in principle hurt.
//
// WHY THIS EXISTS. spliceArrayAny capacity-bounds the left half so it cannot be
// appended through into the right. The tail fast path in
// typeListInsertGenericsAfter grows the tail item's contentAny with append, so a
// bounded right half would force that append to reallocate the whole run. In a workload that
// only deletes, that never happens; in one that alternates, every push after a
// delete could pay O(run). That is a plausible route back to quadratic through
// the very bound that removed it, and asserting it away without measuring is
// exactly the kind of claim that turns out to be wrong.
//
// IT WAS NOT HYPOTHETICAL. The first version of the split bounded BOTH halves,
// and on this workload a memory profile put 99.46% of all allocated bytes in
// TypeListInsertGenericsAfter — the quadratic had moved from the delete path to
// the push path and got slightly worse, while the delete-only benchmark showed a
// 100x win and reported nothing. Only the tail is left unbounded now.
//
// The shape is a scene: add an element, remove an old one, repeat, on a document
// that already holds n elements. Measured on arm64, medians of three at
// -benchtime=5x, both arms interleaved in one session:
//
//	n        before        after      B/op before     B/op after
//	2,000    6.36 ms      0.71 ms      32,996,968        276,812
//	4,000   18.43        1.58         131,558,496        575,280
//	8,000   94.29        2.82         525,300,790      1,336,134
//	16,000 566.99        5.03       2,099,202,915      2,677,667
//
// Quadratic to linear, same as the delete-only row: after the fix both time and
// bytes grow about 2x per doubling.
func benchPushDeleteAlternating(b *testing.B, n int) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := NewDoc("alt", WithGC(false), WithClientID(1))
		arr := doc.GetArray("a")
		for j := 0; j < n; j++ {
			arr.Push([]any{j})
		}
		b.StartTimer()
		for j := 0; j < n/2; j++ {
			arr.Push([]any{n + j})
			arr.Delete(0, 1)
		}
	}
}

func BenchmarkPushDeleteAlternating2000(b *testing.B)  { benchPushDeleteAlternating(b, 2000) }
func BenchmarkPushDeleteAlternating4000(b *testing.B)  { benchPushDeleteAlternating(b, 4000) }
func BenchmarkPushDeleteAlternating8000(b *testing.B)  { benchPushDeleteAlternating(b, 8000) }
func BenchmarkPushDeleteAlternating16000(b *testing.B) { benchPushDeleteAlternating(b, 16000) }

// Push-only on an aged document, to separate "append is slow because the run was
// split" from "append is slow". No deletes, so no split ever happens and the tail
// content keeps its spare capacity.
func benchPushOnlyAged(b *testing.B, n int) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := NewDoc("alt", WithGC(false), WithClientID(1))
		arr := doc.GetArray("a")
		for j := 0; j < n; j++ {
			arr.Push([]any{j})
		}
		b.StartTimer()
		for j := 0; j < n/2; j++ {
			arr.Push([]any{n + j})
		}
	}
}

func BenchmarkPushOnlyAged2000(b *testing.B)  { benchPushOnlyAged(b, 2000) }
func BenchmarkPushOnlyAged4000(b *testing.B)  { benchPushOnlyAged(b, 4000) }
func BenchmarkPushOnlyAged8000(b *testing.B)  { benchPushOnlyAged(b, 8000) }
func BenchmarkPushOnlyAged16000(b *testing.B) { benchPushOnlyAged(b, 16000) }
