package crdt

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
	"unsafe"
)

func requireClientStructTreeMatches(t *testing.T, tree *clientStructTree, oracle *flatClientStructOracle) {
	t.Helper()
	if err := tree.Validate(); err != nil {
		t.Fatalf("tree invariant: %v", err)
	}
	got := tree.Snapshot(nil)
	if len(got) != len(oracle.items) || tree.Len() != len(oracle.items) {
		t.Fatalf("snapshot/tree/oracle lengths=%d/%d/%d", len(got), tree.Len(), len(oracle.items))
	}
	for i := range got {
		if got[i] != oracle.items[i] {
			t.Fatalf("snapshot %d=%p (%v), want %p (%v)", i, got[i], got[i].getID(), oracle.items[i], oracle.items[i].getID())
		}
	}

	forward := 0
	for cursor, ok := tree.First(); ok; cursor, ok = cursor.Next() {
		if cursor.Value() != oracle.items[forward] {
			t.Fatalf("forward cursor %d=%p, want %p", forward, cursor.Value(), oracle.items[forward])
		}
		forward++
	}
	if forward != len(oracle.items) {
		t.Fatalf("forward cursor count=%d, want %d", forward, len(oracle.items))
	}

	reverse := len(oracle.items) - 1
	for cursor, ok := tree.Last(); ok; cursor, ok = cursor.Prev() {
		if cursor.Value() != oracle.items[reverse] {
			t.Fatalf("reverse cursor %d=%p, want %p", reverse, cursor.Value(), oracle.items[reverse])
		}
		reverse--
	}
	if reverse != -1 {
		t.Fatalf("reverse cursor stopped at %d", reverse)
	}
}

func TestClientStructTreeMatchesIndependentFlatOracleAfterEveryPrimitive(t *testing.T) {
	const (
		seeds = 40
		steps = 800
	)
	var exercised [6]int
	for seed := int64(0); seed < seeds; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			tree := newClientStructTree(4, 3)
			oracle := &flatClientStructOracle{}
			first := &abstractStructBase{id: GenID(Number(seed+1), 0), length: 8}
			tree.Append(first)
			if err := oracle.insert(first); err != nil {
				t.Fatal(err)
			}
			requireClientStructTreeMatches(t, tree, oracle)

			for step := 0; step < steps; step++ {
				switch rng.Intn(len(exercised)) {
				case 0: // append at the semantic end
					last := oracle.items[len(oracle.items)-1]
					value := &abstractStructBase{
						id:     GenID(first.id.Client, last.getID().Clock+last.structLength()),
						length: Number(1 + rng.Intn(8)),
					}
					tree.Append(value)
					if err := oracle.insert(value); err != nil {
						t.Fatal(err)
					}
					exercised[0]++

				case 1: // split a semantic range
					var candidates []abstractStruct
					for _, value := range oracle.items {
						if value.structLength() > 1 {
							candidates = append(candidates, value)
						}
					}
					if len(candidates) == 0 {
						continue
					}
					left := candidates[rng.Intn(len(candidates))]
					cursor, err := tree.Find(left.getID().Clock)
					if err != nil {
						t.Fatal(err)
					}
					oldLength := left.structLength()
					offset := Number(1 + rng.Intn(int(oldLength-1)))
					left.setStructLength(offset)
					right := &abstractStructBase{id: GenID(left.getID().Client, left.getID().Clock+offset), length: oldLength - offset}
					tree.InsertAfter(cursor, right)
					if err := oracle.insert(right); err != nil {
						t.Fatal(err)
					}
					exercised[1]++

				case 2: // merge neighbours and remove the absorbed right side
					if len(oracle.items) < 2 {
						continue
					}
					index := rng.Intn(len(oracle.items) - 1)
					left, right := oracle.items[index], oracle.items[index+1]
					rightCursor, err := tree.Find(right.getID().Clock)
					if err != nil {
						t.Fatal(err)
					}
					left.setStructLength(left.structLength() + right.structLength())
					tree.Remove(rightCursor, rightCursor)
					if err := oracle.remove(right); err != nil {
						t.Fatal(err)
					}
					exercised[2]++

				case 3: // replace without moving the semantic range
					old := oracle.items[rng.Intn(len(oracle.items))]
					cursor, err := tree.Find(old.getID().Clock)
					if err != nil {
						t.Fatal(err)
					}
					replacement := &abstractStructBase{id: *old.getID(), length: old.structLength()}
					tree.Replace(cursor, replacement)
					if err := oracle.replace(replacement); err != nil {
						t.Fatal(err)
					}
					exercised[3]++

				case 4: // independently derived point lookup
					last := oracle.items[len(oracle.items)-1]
					end := last.getID().Clock + last.structLength()
					clock := Number(rng.Intn(int(end)))
					want, found := oracle.find(clock)
					cursor, err := tree.Find(clock)
					if found != (err == nil) {
						t.Fatalf("Find(%d) found=%v err=%v", clock, found, err)
					}
					if found && cursor.Value() != want {
						t.Fatalf("Find(%d)=%p, want %p", clock, cursor.Value(), want)
					}
					exercised[4]++

				case 5: // remove an inclusive cross-leaf-capable run
					if len(oracle.items) < 3 {
						continue
					}
					leftIndex := rng.Intn(len(oracle.items) - 2)
					removeCount := 2 + rng.Intn(minNumber(5, len(oracle.items)-leftIndex-1)-1)
					left := oracle.items[leftIndex]
					removed := append([]abstractStruct(nil), oracle.items[leftIndex+1:leftIndex+1+removeCount]...)
					firstCursor, err := tree.Find(removed[0].getID().Clock)
					if err != nil {
						t.Fatal(err)
					}
					lastCursor, err := tree.Find(removed[len(removed)-1].getID().Clock)
					if err != nil {
						t.Fatal(err)
					}
					for _, value := range removed {
						left.setStructLength(left.structLength() + value.structLength())
					}
					tree.Remove(firstCursor, lastCursor)
					for _, value := range removed {
						if err := oracle.remove(value); err != nil {
							t.Fatal(err)
						}
					}
					exercised[5]++
				}
				requireClientStructTreeMatches(t, tree, oracle)
			}
		})
	}
	for operation, count := range exercised {
		if count < 100 {
			t.Fatalf("primitive %d exercised only %d times", operation, count)
		}
	}
}

func TestClientStructTreeBuildAndCursorContract(t *testing.T) {
	values := []abstractStruct{&abstractStructBase{id: GenID(5, 0), length: 2}}
	for clock := Number(2); clock < 81; clock++ {
		values = append(values, &abstractStructBase{id: GenID(5, clock), length: 1})
	}
	tree := newClientStructTreeFromFlat(values, 4, 3)
	oracle := &flatClientStructOracle{items: append([]abstractStruct(nil), values...)}
	requireClientStructTreeMatches(t, tree, oracle)

	first, _ := tree.First()
	for clock := Number(81); clock < 161; clock++ {
		value := &abstractStructBase{id: GenID(5, clock), length: 1}
		tree.Append(value)
		if err := oracle.insert(value); err != nil {
			t.Fatal(err)
		}
	}
	if !first.Valid() || first.Value() != values[0] {
		t.Fatal("right-edge leaf/branch splits invalidated an append-stable cursor")
	}
	replacement := &abstractStructBase{id: GenID(5, 0), length: 2}
	tree.Replace(first, replacement)
	if !first.Valid() || first.Value() != replacement {
		t.Fatal("replace invalidated a position-stable cursor")
	}
	if err := oracle.replace(replacement); err != nil {
		t.Fatal(err)
	}

	inserted := &abstractStructBase{id: GenID(5, 1), length: 1}
	currentFirst, _ := tree.First()
	currentFirst.Value().setStructLength(1)
	tree.InsertAfter(currentFirst, inserted)
	if currentFirst.Valid() {
		t.Fatal("InsertAfter left an old cursor valid")
	}
	requireClientStructCursorPanic(t, func() { currentFirst.Value() })
	if err := tree.Validate(); err != nil {
		t.Fatal(err)
	}

	snapshot := tree.Snapshot(nil)
	snapshot[0] = inserted
	firstAfterSplit, _ := tree.First()
	if firstAfterSplit.Value() != replacement {
		t.Fatal("mutating a flattened tree snapshot changed leaf storage")
	}
}

func TestClientStructTreeOrdinalAndChunkCursors(t *testing.T) {
	values := make([]abstractStruct, 37)
	for i := range values {
		values[i] = &abstractStructBase{id: GenID(18, Number(i)), length: 1}
	}
	tree := newClientStructTreeFromFlat(values, 4, 3)
	for position, want := range values {
		cursor, ok := tree.At(position)
		if !ok || cursor.Value() != want || tree.Index(cursor) != position {
			t.Fatalf("At/Index(%d) cursor=%v value=%p index=%d", position, ok, cursor.Value(), tree.Index(cursor))
		}
	}
	if _, ok := tree.At(-1); ok {
		t.Fatal("At(-1) succeeded")
	}
	if _, ok := tree.At(len(values)); ok {
		t.Fatal("At(len) succeeded")
	}

	start, _ := tree.At(3)
	var got []abstractStruct
	chunks := 0
	for chunk, ok := tree.ChunkFrom(start), true; ok; chunk, ok = chunk.Next() {
		got = append(got, chunk.Values()...)
		chunks++
	}
	if chunks < 2 || len(got) != len(values)-3 {
		t.Fatalf("chunks=%d values=%d, want multiple/%d", chunks, len(got), len(values)-3)
	}
	for i := range got {
		if got[i] != values[i+3] {
			t.Fatalf("chunk value %d=%p, want %p", i, got[i], values[i+3])
		}
	}
}

func TestClientStructTreePreservesMalformedEqualClockOrder(t *testing.T) {
	values := []abstractStruct{
		&abstractStructBase{id: GenID(6, 0), length: 1},
		&abstractStructBase{id: GenID(6, 1), length: 1},
		&abstractStructBase{id: GenID(6, 1), length: 1},
		&abstractStructBase{id: GenID(6, 2), length: 1},
		&abstractStructBase{id: GenID(6, 3), length: 1},
	}
	tree := newClientStructTreeFromFlat(values, 2, 3)
	if err := tree.Validate(); err != nil {
		t.Fatal(err)
	}
	got := tree.Snapshot(nil)
	for i := range values {
		if got[i] != values[i] {
			t.Fatalf("equal-clock order %d=%p, want %p", i, got[i], values[i])
		}
	}
}

func TestClientStructTreeBorrowMergeAndRootCollapse(t *testing.T) {
	makeTree := func(count int) (*clientStructTree, []*abstractStructBase) {
		values := make([]*abstractStructBase, count)
		abstract := make([]abstractStruct, count)
		for i := range values {
			values[i] = &abstractStructBase{id: GenID(8, Number(i)), length: 1}
			abstract[i] = values[i]
		}
		return newClientStructTreeFromFlat(abstract, 4, 3), values
	}
	remove := func(t *testing.T, tree *clientStructTree, first, last Number) {
		t.Helper()
		left, err := tree.Find(first)
		if err != nil {
			t.Fatal(err)
		}
		right, err := tree.Find(last)
		if err != nil {
			t.Fatal(err)
		}
		tree.Remove(left, right)
		if err := tree.Validate(); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("borrow-right", func(t *testing.T) {
		tree, _ := makeTree(6) // leaves 3/3, minimum 2
		remove(t, tree, 0, 1)  // left 1 borrows from right 3 -> 2/2
		if tree.first.used != 2 || tree.last.used != 2 || tree.first == tree.last {
			t.Fatalf("borrow-right leaf occupancies=%d/%d", tree.first.used, tree.last.used)
		}
	})

	t.Run("borrow-left", func(t *testing.T) {
		tree, _ := makeTree(6)
		remove(t, tree, 4, 5) // right 1 borrows from left 3 -> 2/2
		if tree.first.used != 2 || tree.last.used != 2 || tree.first == tree.last {
			t.Fatalf("borrow-left leaf occupancies=%d/%d", tree.first.used, tree.last.used)
		}
	})

	t.Run("merge", func(t *testing.T) {
		tree, _ := makeTree(6)
		remove(t, tree, 0, 0) // 2/3
		remove(t, tree, 5, 5) // 2/2
		remove(t, tree, 1, 1) // 1/2 merges and collapses root
		if tree.root == nil || tree.root.leaf == nil || tree.first != tree.last || tree.Len() != 3 {
			t.Fatalf("merged root=%p first/last=%p/%p len=%d", tree.root, tree.first, tree.last, tree.Len())
		}
	})

	t.Run("multi-level-collapse", func(t *testing.T) {
		tree, _ := makeTree(80)
		if tree.root.branch == nil || tree.root.branch.children[0].branch == nil {
			t.Fatal("fixture did not build a multi-level tree")
		}
		remove(t, tree, 0, 76)
		if tree.root == nil || tree.root.leaf == nil || tree.Len() != 3 {
			t.Fatalf("multi-level collapse root=%p leaf=%p branch=%p first/last=%d/%d len=%d",
				tree.root, tree.root.leaf, tree.root.branch, tree.first.used, tree.last.used, tree.Len())
		}
	})
}

func TestClientStructTreeRefreshesMergedPredecessorAcrossLeafBoundary(t *testing.T) {
	values := make([]abstractStruct, 6)
	for i := range values {
		values[i] = &abstractStructBase{id: GenID(9, Number(i)), length: 1}
	}
	tree := newClientStructTreeFromFlat(values, 4, 3) // leaves 3/3
	left := values[2]
	right := values[3]
	if tree.first.items[tree.first.used-1] != left || tree.first.next.items[0] != right {
		t.Fatal("fixture does not put the merged pair across a leaf boundary")
	}

	rightCursor, err := tree.Find(right.getID().Clock)
	if err != nil {
		t.Fatal(err)
	}
	left.setStructLength(left.structLength() + right.structLength())
	tree.Remove(rightCursor, rightCursor)
	if err := tree.Validate(); err != nil {
		t.Fatalf("predecessor aggregate stayed stale after cross-leaf merge: %v", err)
	}
	if tree.first.node.endClock != 4 {
		t.Fatalf("predecessor leaf endClock=%d, want 4", tree.first.node.endClock)
	}
}

func TestClientStructTreeDeletedRunCrossesLeafAsOneWireRange(t *testing.T) {
	const client = Number(12)
	values := []abstractStruct{
		&abstractStructBase{id: GenID(client, 0), length: 1},
		newGC(GenID(client, 1), 1),
		newGC(GenID(client, 2), 1),
		newGC(GenID(client, 3), 1),
		newGC(GenID(client, 4), 1),
		newGC(GenID(client, 5), 1),
		&abstractStructBase{id: GenID(client, 6), length: 1},
	}
	tree := newClientStructTreeFromFlat(values, 4, 3)
	if tree.first == tree.last || tree.first.items[tree.first.used-1].getID().Clock != 3 || tree.first.next.items[0].getID().Clock != 4 {
		t.Fatal("fixture does not place one deleted run across a leaf boundary")
	}
	if got := tree.deletedRangeCount(); got != 1 {
		t.Fatalf("deletedRangeCount=%d, want 1", got)
	}
	ranges := tree.appendDeletedRanges(nil)
	if len(ranges) != 1 || ranges[0].clock != 1 || ranges[0].length != 5 {
		t.Fatalf("deleted ranges=%v, want [{Clock:1 Length:5}]", ranges)
	}

	tests := []struct {
		name string
		new  func() dsEncoder
		out  func(dsEncoder) []byte
	}{
		{
			name: "v1",
			new:  func() dsEncoder { return newUpdateEncoderV1() },
			out:  func(encoder dsEncoder) []byte { return encoder.(*updateEncoderV1).toBytes() },
		},
		{
			name: "v2",
			new:  func() dsEncoder { return newDefaultUpdateEncoderV2() },
			out:  func(encoder dsEncoder) []byte { return encoder.(*updateEncoderV2).toBytes() },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			flatEncoder := test.new()
			if err := writeDeletedStructRanges(flatEncoder, client, values, deletedStructRangeCount(values)); err != nil {
				t.Fatal(err)
			}
			treeEncoder := test.new()
			if err := tree.writeDeletedRanges(treeEncoder, client, tree.deletedRangeCount()); err != nil {
				t.Fatal(err)
			}
			flatBytes, treeBytes := test.out(flatEncoder), test.out(treeEncoder)
			if !bytes.Equal(flatBytes, treeBytes) {
				t.Fatalf("tree bytes=%x, flat=%x", treeBytes, flatBytes)
			}
		})
	}
}

func TestClientStructTreeValidatorRejectsGlobalCorruption(t *testing.T) {
	makeTree := func() *clientStructTree {
		values := make([]abstractStruct, 20)
		for i := range values {
			values[i] = &abstractStructBase{id: GenID(13, Number(i)), length: 1}
		}
		return newClientStructTreeFromFlat(values, 4, 3)
	}
	tests := []struct {
		name    string
		corrupt func(*clientStructTree)
	}{
		{name: "separator", corrupt: func(tree *clientStructTree) { tree.root.branch.children[0].endClock++ }},
		{name: "parent", corrupt: func(tree *clientStructTree) { tree.root.branch.children[0].parent = nil }},
		{name: "unused-slot", corrupt: func(tree *clientStructTree) { tree.last.items[tree.last.used] = tree.first.items[0] }},
		{name: "two-child-root-collapsed", corrupt: func(tree *clientStructTree) {
			root := tree.root.branch
			root.children[1].parent = nil
			root.children[1] = nil
			root.used = 1
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tree := makeTree()
			test.corrupt(tree)
			if err := tree.Validate(); err == nil {
				t.Fatal("validator accepted corrupted tree")
			}
		})
	}
}

func TestClientStructTreeFullRemovalDetachesStorage(t *testing.T) {
	values := make([]abstractStruct, 40)
	for i := range values {
		values[i] = &abstractStructBase{id: GenID(14, Number(i)), length: 1}
	}
	tree := newClientStructTreeFromFlat(values, 4, 3)
	var leaves []*clientStructTreeLeaf
	for leaf := tree.first; leaf != nil; leaf = leaf.next {
		leaves = append(leaves, leaf)
	}
	var branches []*clientStructTreeBranch
	var collectBranches func(*clientStructTreeNode)
	collectBranches = func(node *clientStructTreeNode) {
		if node == nil || node.branch == nil {
			return
		}
		branch := node.branch
		branches = append(branches, branch)
		for i := 0; i < branch.used; i++ {
			collectBranches(branch.children[i])
		}
	}
	collectBranches(tree.root)

	first, _ := tree.First()
	last, _ := tree.Last()
	if next, ok := tree.Remove(first, last); ok || next.Valid() || tree.Len() != 0 {
		t.Fatalf("full remove next=%#v ok=%v len=%d", next, ok, tree.Len())
	}
	if err := tree.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, leaf := range leaves {
		if leaf.tree != nil || leaf.prev != nil || leaf.next != nil || leaf.node.parent != nil {
			t.Fatalf("detached leaf %p retains tree/links/parent", leaf)
		}
		for slot, value := range leaf.items {
			if value != nil {
				t.Fatalf("detached leaf %p retains item in slot %d", leaf, slot)
			}
		}
	}
	for _, branch := range branches {
		if branch.used != 0 || branch.node.parent != nil {
			t.Fatalf("detached branch %p used=%d parent=%p", branch, branch.used, branch.node.parent)
		}
		for slot, child := range branch.children {
			if child != nil {
				t.Fatalf("detached branch %p retains child in slot %d", branch, slot)
			}
		}
	}
}

func TestClientStructTreeNodeLayouts(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("layout report is for the 64-bit production targets")
	}
	t.Logf("node=%d leaf=%d branch=%d bytes",
		unsafe.Sizeof(clientStructTreeNode{}),
		unsafe.Sizeof(clientStructTreeLeaf{}),
		unsafe.Sizeof(clientStructTreeBranch{}),
	)
}

var clientStructTreeLayoutSink any

func BenchmarkClientStructTreeNodeAllocations(b *testing.B) {
	b.Cleanup(func() { clientStructTreeLayoutSink = nil })
	b.Run("leaf", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			clientStructTreeLayoutSink = &clientStructTreeLeaf{}
		}
	})
	b.Run("branch", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			clientStructTreeLayoutSink = &clientStructTreeBranch{}
		}
	})
}

// One tree build must serve many timed removals, or the benchmark cannot finish.
//
// The previous shape rebuilt the whole tree inside b.StopTimer()/b.StartTimer()
// on every iteration. StopTimer removes that work from ns/op but not from the
// clock, and Go sizes b.N from the timed body alone — about 650 ns — so it aimed
// at roughly a million iterations and dragged a million O(count) rebuilds behind
// them. At 64,000 structs the row never completed: it consumed a 120-minute
// package timeout and discarded the whole Go leg of a cross-implementation
// comparison. A benchmark that reports a fast per-op number while being unbounded
// in wall clock is worse than a slow one, because nothing in the output says so.
//
// Amortising the build over a batch bounds the untimed work at O(count) per batch
// rather than per iteration. The batch is also capped at a sixteenth of the tree
// so the structure stays representative of its nominal size: successive removals
// shrink it by at most 6.25% before it is rebuilt.
//
// NOT COMPARABLE to the old RemoveMiddle figures. Locating the middle now happens
// inside the timed region — with one build serving many removals there is nowhere
// to put an untimed lookup that does not cost more in timer overhead than the
// lookup itself. Both halves are O(log n), so the asymptotic claim this row exists
// to defend is unaffected, but the constant is not the same number.
const clientStructTreeRemovalsPerBuild = 512

func BenchmarkClientStructTreeFindAndRemoveMiddle(b *testing.B) {
	for _, count := range []int{1_000, 8_000, 64_000} {
		b.Run(fmt.Sprintf("structs-%d", count), func(b *testing.B) {
			b.ReportAllocs()
			values := make([]abstractStruct, count)
			for i := range values {
				values[i] = &abstractStructBase{id: GenID(21, Number(i)), length: 1}
			}
			batch := min(clientStructTreeRemovalsPerBuild, count/16)

			var tree *clientStructTree
			remaining := 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if remaining == 0 {
					b.StopTimer()
					tree = newClientStructTreeFromFlat(values, 64, 32)
					remaining = batch
					b.StartTimer()
				}
				cursor, ok := tree.At(tree.Len() / 2)
				if !ok {
					b.Fatal("no cursor at the middle position")
				}
				tree.Remove(cursor, cursor)
				remaining--
			}
		})
	}
}
