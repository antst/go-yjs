package crdt

import (
	"runtime"
	"testing"
)

// Node arena retention: nodes are allocated from fixed blocks and NEVER individually freed, and
// the tree has no node-removal path — splitBlock only ever adds. That is safe for pointer
// stability, but it means the node population can only grow, while `removeMergedRight` genuinely
// removes physical Items from blocks when cleanup merges them.
//
// So the question this asks is whether a merge-heavy workload leaves behind blocks that have shed
// most or all of their Items. Such nodes stay in the tree forever: they deepen every descent and
// hold arena memory proportional to peak rather than to live content. Nothing in the suite would
// notice, because the tree stays correct — just progressively emptier.

func countNodes(n *listPositionNode) (nodes int, items Number, empty int) {
	if n == nil {
		return 0, 0, 0
	}
	ln, li, le := countNodes(n.left)
	rn, ri, re := countNodes(n.right)
	e := le + re
	if n.items == 0 {
		e++
	}
	return ln + rn + 1, li + ri + n.items, e
}

func TestArenaDoesNotAccumulateEmptyBlocksUnderMerges(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	rng := markerLCG(0x5A5A)

	// Fragment past the plain activation threshold so the tree exists.
	for i := 0; i < buildListPositionIndexItems+4_000; i++ {
		arr.Insert(Number(rng(arr.GetLength()+1)), ArrayAny{i})
	}
	arr.Insert(Number(rng(arr.GetLength()+1)), ArrayAny{-1}) // force activation
	_, index := ownedListPositionIndex(arr)
	if index == nil {
		t.Fatal("no index activated; this test observes nothing")
	}
	n0, i0, e0 := countNodes(index.root)
	t.Logf("ARENA after growth: nodes=%d items=%d empty=%d itemsPerNode=%.1f", n0, i0, e0, float64(i0)/float64(n0))

	// Deletion-heavy phase. This is the shape that can actually empty a block: deleting a
	// contiguous range tombstones its Items, and cleanup then merges ADJACENT TOMBSTONES into one
	// physical Item. Physical count therefore falls, unlike the append case where it only rises.
	for round := 0; round < 30 && arr.GetLength() > 2_000; round++ {
		Transact(doc, func(*Transaction) {
			at := Number(rng(arr.GetLength() / 2))
			n := Number(400)
			if at+n >= arr.GetLength() {
				n = arr.GetLength() - at - 1
			}
			if n > 0 {
				arr.Delete(at, n)
			}
		}, nil, true)
		arr.Insert(Number(rng(arr.GetLength()+1)), ArrayAny{round}) // keep the tree in use
	}

	_, index = ownedListPositionIndex(arr)
	if index == nil {
		t.Skip("index was destroyed during the merge phase; retention question does not arise")
	}
	n1, i1, e1 := countNodes(index.root)
	t.Logf("ARENA after merges: nodes=%d items=%d empty=%d itemsPerNode=%.1f", n1, i1, e1, float64(i1)/float64(n1))

	if e1 > 0 {
		t.Errorf("%d tree nodes hold zero Items after a merge-heavy workload; nothing removes an "+
			"emptied block, so they deepen every descent and hold arena memory permanently", e1)
	}
	// A tree that has shed most of its content should not be carrying far more nodes than the
	// live population justifies. Target block size is 8, so ~8 Items per node is healthy.
	if perNode := float64(i1) / float64(n1); perNode < 2.0 {
		t.Errorf("tree averages %.1f Items per node (target block size is 8); blocks have shed "+
			"content without the node population shrinking", perNode)
	}
}

// The retention question they asked to have scrutinised: destroying an index must actually release
// its node blocks. The arena concentrates a tree's memory into a few large allocations, which makes
// a missed release bigger and quieter than before — one retained block is hundreds of KiB rather
// than a scatter of small nodes the collector would reclaim piecemeal.
//
// Measured by heap rather than by map size, because the side-table entry going away proves nothing
// about the blocks it pointed at.
func TestDestroyReleasesNodeArena(t *testing.T) {
	live := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}

	doc := newDoc("g", true, defaultGCFilter, nil, false, WithClientID(1))
	outer := doc.GetArray("a")
	rng := markerLCG(0xBEEF)

	base := live()
	var peak uint64
	for cycle := 0; cycle < 5; cycle++ {
		inner := NewYArray()
		outer.Insert(0, ArrayAny{inner})
		for i := 0; i < buildListPositionIndexItems+3_000; i++ {
			inner.Insert(Number(rng(inner.GetLength()+1)), ArrayAny{i})
		}
		inner.Insert(Number(rng(inner.GetLength()+1)), ArrayAny{-1}) // force activation
		if _, idx := ownedListPositionIndex(inner); idx == nil {
			t.Fatal("no index activated; this test observes nothing")
		}
		if h := live(); h > peak {
			peak = h
		}
		outer.Delete(0, 1)
	}
	after := live()

	// Each cycle builds a ~19k-Item tree. If destroy failed to drop the blocks, five cycles would
	// retain five trees' worth; the bound below is generous and still catches that.
	grown := int64(after) - int64(base)
	t.Logf("ARENA_RETENTION base=%dKiB peak=%dKiB after=%dKiB grown=%dKiB",
		base/1024, peak/1024, after/1024, grown/1024)
	if grown > 8<<20 {
		t.Errorf("heap grew %d KiB across 5 create/destroy cycles; node blocks are being retained "+
			"after the index was destroyed", grown/1024)
	}
}
