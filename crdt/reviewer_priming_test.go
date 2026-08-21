package crdt

import (
	"testing"
	"unsafe"
)

// The two sentinels must be distinct POINTERS, and that is a property of listReadIndex being
// non-zero-sized rather than of the declarations. Go is permitted to give two allocations of a
// zero-size type the same address, so if listReadIndex ever lost its slice field the priming and
// building sentinels would alias, `case buildingListReadIndex` would swallow the primed state, and
// every large list would return nil forever — a silent permanent loss of the index with no crash,
// no failing test, and no wrong answer. Pin it here rather than trusting the struct never changes.
func TestReadIndexSentinelsAreDistinct(t *testing.T) {
	if primedListReadIndex == buildingListReadIndex {
		t.Fatal("priming and building sentinels are the same pointer; the state machine cannot " +
			"distinguish them and large lists will never build an index")
	}
	if unsafe.Sizeof(listReadIndex{}) == 0 {
		t.Fatal("listReadIndex is zero-sized, so Go may alias the two sentinels at the same address")
	}
}

// The deferral state machine, stated as behaviour rather than internals.
func TestLargeListDefersIndexUntilSecondRead(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	rng := markerLCG(11)
	for i := 0; i < deferListReadIndexBuildItems+2_000; i++ {
		arr.Insert(rng(arr.GetLength()+1), ArrayAny{i})
	}
	if listItemCount(arr) < deferListReadIndexBuildItems {
		t.Fatalf("fixture has %d items, need >= %d", listItemCount(arr), deferListReadIndexBuildItems)
	}

	want := arr.ToArray()
	check := func(label string) {
		t.Helper()
		for _, i := range []int{1, 7, len(want) / 2, len(want) - 1} {
			if got := arr.Get(i); got != want[i] {
				t.Fatalf("%s: Get(%d)=%v want %v", label, i, got, want[i])
			}
		}
	}

	// First indexed read primes only.
	_ = arr.Get(len(want) / 3)
	if got := arr.readIndex.Load(); got != primedListReadIndex {
		t.Fatalf("after first read, cache = %v, want the priming sentinel", got)
	}
	check("primed")

	// Second builds the dense snapshot.
	_ = arr.Get(len(want) / 3)
	idx := arr.readIndex.Load()
	if idx == primedListReadIndex || idx == buildingListReadIndex || idx == nil {
		t.Fatalf("after second read, cache = %v, want a published snapshot", idx)
	}
	if len(idx.positions) == 0 {
		t.Fatal("published snapshot is empty")
	}
	check("built")

	// A mutation must clear the snapshot, and the next single read must NOT rebuild it — that is
	// the whole point of the deferral, and the property the allocation budget defends.
	arr.Insert(rng(arr.GetLength()+1), ArrayAny{999})
	if got := arr.readIndex.Load(); got != nil {
		t.Fatalf("mutation left cache = %v, want nil", got)
	}
	want = arr.ToArray()
	_ = arr.Get(len(want) / 3)
	if got := arr.readIndex.Load(); got != primedListReadIndex {
		t.Fatalf("read after mutation left cache = %v, want the priming sentinel (a rebuild here "+
			"is the allocation regression this design exists to avoid)", got)
	}
	check("after mutation")
}

// Small lists must keep building immediately: the deferral is only justified where the snapshot is
// large, and silently applying it everywhere would slow every ordinary document's second read.
func TestSmallListBuildsIndexImmediately(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	rng := markerLCG(12)
	for i := 0; i < 3_000; i++ {
		arr.Insert(rng(arr.GetLength()+1), ArrayAny{i})
	}
	_ = arr.Get(500)
	idx := arr.readIndex.Load()
	if idx == nil || idx == primedListReadIndex || idx == buildingListReadIndex {
		t.Fatalf("small list did not build on first read: cache = %v", idx)
	}
}
