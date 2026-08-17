package crdt

import (
	"runtime"
	"testing"
)

// Allocation budget for workloads that cross the position-index activation threshold.
//
// WHY THIS SHAPE, AND WHY IT IS NOT REDUNDANT WITH THE INVARIANT TESTS. The aggregate check in the
// position index is a correctness safety valve: when a mutation path changes lengths without
// maintaining the tree, the check notices, discards the tree and rebuilds it. The document stays
// right. Only the cost explodes — and it explodes as (operations past the threshold) x O(items),
// which is quadratic in the tail.
//
// That means the failure is invisible to every correctness gate we have, and it stays invisible to
// a unit test built below the threshold. It was found by profiling, not by testing, and it had two
// independent causes at once: the compact tail append grew lengths without updating the owning
// block, and the ContentAny coalescer discarded the whole tree for an ordinary two-Item merge where
// the ContentString coalescer declines to. Either alone reproduced it. A third path did the same
// thing for visible-end resolution past trailing tombstones.
//
// Three causes in one afternoon is the argument for a guard on the PROPERTY rather than on the
// three instances. The teeth tests alongside this one pin each mechanism; this pins the outcome, so
// a fourth path that bypasses index maintenance fails here even though nobody thought to write a
// test for it.
//
// Allocation rather than time: it is deterministic, so this reads the same on a loaded laptop as on
// an idle server. The regression it defends was 269.66 MB against 2.13 MB — a factor of 127, which
// no plausible measurement noise reaches.

// alternatingPushDeleteAlloc runs the fixture that produced the cliff: push, and delete every OTHER
// element. Deleting every element instead lets consecutive same-client tombstones merge, which
// collapses the physical count and never activates the index at all — four fixtures of mine failed
// that way before the shape was pinned down, so it is spelled out here rather than left to be
// rediscovered.
func alternatingPushDeleteAlloc(t *testing.T, n int) (bytesPerOp float64, items Number) {
	t.Helper()
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < n; i++ {
		arr.Push(ArrayAny{i})
		if i%2 == 1 {
			arr.Delete(arr.GetLength()-1, 1)
		}
	}
	runtime.ReadMemStats(&after)
	return float64(after.TotalAlloc-before.TotalAlloc) / float64(n), listItemCount(arr)
}

func TestPositionIndexDoesNotThrashAcrossActivation(t *testing.T) {
	// Below the threshold the index never activates, so this is the honest baseline for what the
	// workload costs without any index at all.
	belowPerOp, belowItems := alternatingPushDeleteAlloc(t, 8_000)
	if belowItems >= buildListPositionIndexItems {
		t.Fatalf("the 8k fixture reached %d items and activates the index; it can no longer serve "+
			"as the un-indexed baseline", belowItems)
	}

	abovePerOp, aboveItems := alternatingPushDeleteAlloc(t, 20_000)
	if aboveItems < buildListPositionIndexItems {
		t.Fatalf("the 20k fixture only reached %d items, below the %d activation threshold — this "+
			"test is vacuous and would pass against a fully thrashing index",
			aboveItems, buildListPositionIndexItems)
	}

	// Crossing the threshold must not change the per-operation cost by an order of magnitude. The
	// bound is deliberately loose: an index that maintains itself lands near parity, while one that
	// rebuilds per operation was 60x the baseline per operation at this size.
	ratio := abovePerOp / belowPerOp
	t.Logf("INDEX_THRASH below=%.0f B/op (%d items) above=%.0f B/op (%d items) ratio=%.2f",
		belowPerOp, belowItems, abovePerOp, aboveItems, ratio)
	if ratio > 4.0 {
		t.Errorf("per-operation allocation grew %.1fx when the workload crossed the position-index "+
			"activation threshold (%.0f -> %.0f B/op).\nA mutation path is changing lengths without "+
			"maintaining the tree, so the aggregate check discards and rebuilds it. The document "+
			"stays correct and only the cost explodes, which is why no correctness gate sees this.",
			ratio, belowPerOp, abovePerOp)
	}
}

// The same property stated against document size rather than against the threshold: past
// activation, doubling the work must not square the cost.
func TestPositionIndexCostStaysLinearPastActivation(t *testing.T) {
	small, smallItems := alternatingPushDeleteAlloc(t, 20_000)
	large, largeItems := alternatingPushDeleteAlloc(t, 32_000)
	if smallItems < buildListPositionIndexItems || largeItems < buildListPositionIndexItems {
		t.Fatalf("both fixtures must activate the index; got %d and %d items against a threshold "+
			"of %d", smallItems, largeItems, buildListPositionIndexItems)
	}

	ratio := large / small
	t.Logf("INDEX_LINEARITY 20k=%.0f B/op 32k=%.0f B/op ratio=%.2f", small, large, ratio)
	// Per-operation allocation should be roughly flat in document size. It was 5.2x here when the
	// index rebuilt on every operation, because each rebuild is O(items).
	if ratio > 2.0 {
		t.Errorf("per-operation allocation grew %.1fx for a 1.6x larger document (%.0f -> %.0f "+
			"B/op); the index cost is scaling with document size rather than staying amortised",
			ratio, small, large)
	}
}
