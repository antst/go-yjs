package crdt

import "testing"

// Append-then-delete on a YArray. This was quadratic in BYTES; it is now linear,
// and this comment records both the defect and what removed it, because the shape
// is the one a scene or a log has and nothing else in the suite covers it — the
// array benchmarks push, or delete from a document built for that delete alone,
// so none of them lets cost accumulate across many deletes on one document.
//
// Measured on arm64, building n elements untimed and then deleting n/2 from the
// front. Medians of three at -benchtime=5x, both arms in one session:
//
//	n        before        after      B/op before      B/op after
//	2,000    3.58 ms      0.54 ms      25,522,873         219,632
//	4,000   14.18         0.94        104,770,249         469,168
//	8,000   67.19         1.82        401,468,652         976,432
//	16,000 387.39         3.38      1,570,506,832       1,958,192
//
// Before: bytes grew ~4.1x per doubling — quadratic — while the allocation COUNT
// stayed near 2.5 per delete, so the defect was allocation SIZE, not frequency:
// 25 KB per delete at n=2,000 and 196 KB per delete at n=16,000. After: both time
// and bytes grow ~2x per doubling, which is linear in n for n/2 deletes.
//
// ROOT CAUSE, from a memory profile at n=8,000: slices.Clone of []interface{} was
// 99.35% of all bytes allocated, reached through
//
//	YArray.Delete -> typeListDelete -> GetItemCleanStart -> FindIndexCleanStart
//	  -> SplitItem -> spliceContentWithLength -> contentAny.Splice
//
// Sequential pushes from one client coalesce into a few large contentAny runs.
// Deleting one element splits the run that contains it, and contentAny.Splice
// cloned BOTH halves, so each delete copied the whole run. Run length grows with
// the document, hence quadratic.
//
// WHY THE CLONE WAS THERE, which is the part that took reading yjs to see. yjs
// grows a content array with concat (contentAny.js mergeWith) and splits it with
// slice, and BOTH always allocate — so in yjs no content can ever be written
// through another content's array reference, and sharing is safe by construction.
// This port grows it with append, which writes in place whenever spare capacity
// exists. Cloning both halves was what stood in for that guarantee. Bounding
// capacity everywhere a content array becomes shared restores the guarantee
// structurally, so the split can reslice instead: see spliceArrayAny in
// content_slice_split.go, and the alias tests in content_copy_identity_test.go
// that pin it. Only the LEFT half is bounded: the right half is the tail that the
// append fast path grows, and bounding it as well merely relocates the quadratic
// to the push path — see perf_bench_alternating_test.go, which is the row that
// caught exactly that and is why it exists. The fix also closed a latent aliasing bug in Copy that predates
// the performance work and had nothing to do with it.
//
// BOTH REFERENCES STILL DO THE QUADRATIC THING, so this is now an improvement
// OVER the family rather than a parity repair. yjs 13.6.31
// src/structs/contentAny.js splits with
//
//	const right = new contentAny(this.arr.slice(offset))
//	this.arr = this.arr.slice(0, offset)
//
// and yrs 0.21.3 src/block.rs ItemContent::splice does the same in Rust — it takes
// two BORROWED slices and then copies both of them:
//
//	let (left, right) = value.split_at(offset);
//	let left = left.to_vec();
//	let right = right.to_vec();
//
// identically for ItemContent::JSON.
//
// Measured on pinned yjs with the identical workload — individual pushes so the
// run coalesces, front deletes so each split lands mid-run, structural
// precondition verified as one struct / one contentAny run / max run length n —
// the copied element SLOTS are deterministic: 1,500,500 at n=2,000, then
// 6,001,000, 24,002,000 and 96,004,000. Exactly 4x per doubling. Wall clock:
//
//	n        yjs      Go before   Go after   after vs yjs
//	2,000   11.79 ms    0.35x      0.54 ms     22x faster
//	4,000   20.86       0.68x      0.94        22x faster
//	8,000   49.39       1.36x      1.82        27x faster
//	16,000 122.53       3.22x      3.38        36x faster
//
// We used to cross over and lose to yjs above n≈6,000 — same asymptotics, worse
// constant, because an []interface{} slot is 16 bytes and pointer-bearing so 96M
// copied slots is ~1.5 GB of scannable heap where V8 copies a packed array of
// small integers. The asymptotics now differ, so the margin widens with n rather
// than closing.
//
// The text path was always the opposite story and we are ahead there for a
// different reason. yrs splits strings into SmallString via SmallString::from_str,
// copying both halves, and its map_utf16_offset walks the string character by
// character to convert a UTF-16 offset into a byte offset. Our
// spliceWithLengthInto is O(1) for the validated-ASCII case, because the cached
// length proves the UTF-16 offset is the byte offset.
//
// Not to be confused with the flat-store suffix shift that motivates the B+ tree
// work: instrumenting both primitives on this exact workload shows total suffix
// elements moved across every removal is about ONE per removal, and InsertAfter
// fires once per delete. The struct store was never the cost here.
func benchAppendThenDelete(b *testing.B, n int) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		doc := newDoc("delete-scale", false, defaultGCFilter, nil, false, WithClientID(1))
		arr := doc.GetArray("a")
		for j := 0; j < n; j++ {
			arr.Push([]any{j})
		}
		b.StartTimer()
		for j := 0; j < n/2; j++ {
			arr.Delete(0, 1)
		}
	}
}

func BenchmarkAppendThenDelete2000(b *testing.B)  { benchAppendThenDelete(b, 2000) }
func BenchmarkAppendThenDelete4000(b *testing.B)  { benchAppendThenDelete(b, 4000) }
func BenchmarkAppendThenDelete8000(b *testing.B)  { benchAppendThenDelete(b, 8000) }
func BenchmarkAppendThenDelete16000(b *testing.B) { benchAppendThenDelete(b, 16000) }

// The split in isolation, which is the primitive the row above pays per delete.
//
// BOTH PATHS ARE MEASURED SEPARATELY AND THAT IS THE POINT. spliceArrayAny
// reslices when each half holds at least half of what it pins and copies the half
// out otherwise, so a fixture's CAPACITY — not just its length — decides which
// path runs. An earlier version of this benchmark built the run with
// slices.Clone, whose size-class rounding leaves cap 1024 for len 1000; a
// balanced split of that is 500 against a pinned 1024, which trips the copy-out
// rule. It measured the copy path exclusively, and its numbers did not move when
// the split became O(1) — a fix worth 100x looked like it did nothing here.
//
// So the fixtures are explicit. "balanced" is len == cap, which is the shape a
// coalesced run has and the path essentially every mid-run item split takes:
// allocation-free and flat in n. "copyout" pins four times what each half holds,
// forcing the retention fallback: O(n) by design, and it is what bounds retained
// memory at 2x rather than letting one live element hold a whole run alive.
//
//	n          balanced              copy-out
//	1,000       47.8 ns  24 B/1     3,041 ns   16,408 B/3
//	8,000       23.4 ns  24 B/1    30,749 ns  131,096 B/3
//	32,000      23.3 ns  24 B/1   128,005 ns  524,313 B/3
//
// The balanced column was previously recorded as 260/673/721 ns. That was the
// per-iteration fixture rebuild bleeding into the timed region through cache
// pressure, not the split; with one fixture reused it is flat at ~23 ns from
// 8,000 elements up, which is what O(1) should look like.
//
// The balanced column is flat and its 24 bytes are the returned *contentAny and
// nothing else — no element is copied at any size. The copy-out column is linear,
// as it must be. What used to be a single row averaging these two behaviours now
// names which one it measured.
func benchContentAnySplit(b *testing.B, n int, pinFactor int) {
	arr := make(ArrayAny, n, n*pinFactor)
	for i := range arr {
		arr[i] = i
	}
	content := &contentAny{}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Resetting the view is O(1) and stays INSIDE the timed region on purpose.
		// Splice never writes an element — it reslices or clones — so one fixture
		// array serves every iteration, and a slice-header assignment costs about
		// nothing against a 260 ns body. Rebuilding the fixture per iteration under
		// StopTimer instead is what made this benchmark uncollectable: see the
		// comment above.
		content.arr = arr[: n : n*pinFactor]
		benchSinkAny = content.spliceContent(n / 2)
	}
}

func BenchmarkContentAnySplitBalanced1000(b *testing.B)  { benchContentAnySplit(b, 1000, 1) }
func BenchmarkContentAnySplitBalanced8000(b *testing.B)  { benchContentAnySplit(b, 8000, 1) }
func BenchmarkContentAnySplitBalanced32000(b *testing.B) { benchContentAnySplit(b, 32000, 1) }

func BenchmarkContentAnySplitCopyOut1000(b *testing.B)  { benchContentAnySplit(b, 1000, 4) }
func BenchmarkContentAnySplitCopyOut8000(b *testing.B)  { benchContentAnySplit(b, 8000, 4) }
func BenchmarkContentAnySplitCopyOut32000(b *testing.B) { benchContentAnySplit(b, 32000, 4) }
