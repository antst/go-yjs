package crdt

import (
	"fmt"
	"testing"
)

func validateDocPositionIndexEntries(t *testing.T, doc *Doc) {
	t.Helper()
	reachable := make(map[*abstractTypeBase]struct{})
	var visit func(abstractType)
	visit = func(typ abstractType) {
		state := abstractTypeState(typ)
		if state == nil {
			return
		}
		if _, seen := reachable[state]; seen {
			return
		}
		reachable[state] = struct{}{}
		visitItem := func(item *itemStruct) {
			// Tombstoned ContentType Items remain reachable when GC is disabled, but their nested
			// types can no longer be positioned. Their accelerators must therefore be gone.
			if item.isDeleted() {
				return
			}
			if content, ok := item.content.(*contentType); ok {
				visit(content.value)
			}
		}
		for item := typ.startItem(); item != nil; item = item.right {
			visitItem(item)
		}
		for _, current := range typ.getMap() {
			for item := current; item != nil; item = item.left {
				visitItem(item)
			}
		}
	}
	for _, root := range doc.share {
		visit(root)
	}

	for state := range reachable {
		index := doc.positionIndexes[state]
		indexedItems := Number(0)
		for item := state.start; item != nil; item = item.right {
			if item.info&itemInfoListPositionIndexed != 0 {
				indexedItems++
			}
		}
		if index == nil && indexedItems != 0 {
			t.Fatalf("reachable type %p has %d indexed Items but no side-table entry", state, indexedItems)
		}
		if index != nil && indexedItems != index.items {
			t.Fatalf("reachable type %p side table has %d Items, membership bits mark %d", state, index.items, indexedItems)
		}
	}
	for state, index := range doc.positionIndexes {
		if _, ok := reachable[state]; !ok {
			t.Fatalf("side table retains unreachable type %p (index %p)", state, index)
		}
		if state.doc != doc {
			t.Fatalf("side-table type %p belongs to Doc %p, table belongs to %p", state, state.doc, doc)
		}
	}
}

func validateListPositionTree(t *testing.T, index *listPositionIndex, parent abstractType) {
	t.Helper()

	physical := Number(0)
	visible := Number(0)
	liveFormats := Number(0)
	seenAnchors := make(map[*itemStruct]*listPositionNode)
	var current *listPositionNode
	var inBlock Number
	var formatsInBlock Number
	for item := parent.startItem(); item != nil; item = item.right {
		if item.info&itemInfoListBlockAnchor != 0 {
			if current != nil && inBlock != current.items {
				t.Fatalf("block %p walked %d Items, records %d", current, inBlock, current.items)
			}
			if current != nil && formatsInBlock != Number(current.liveFormats) {
				t.Fatalf("block %p walked %d live formats, records %d", current, formatsInBlock, current.liveFormats)
			}
			current = index.anchors[item]
			if current == nil {
				t.Fatalf("Item %v carries an anchor bit but has no anchor-map entry", item.id)
			}
			if current.first != item {
				t.Fatalf("anchor map points to block first %p, want %p", current.first, item)
			}
			if _, duplicate := seenAnchors[item]; duplicate {
				t.Fatalf("duplicate anchor for Item %v", item.id)
			}
			seenAnchors[item] = current
			inBlock = 0
			formatsInBlock = 0
		}
		if current == nil {
			t.Fatalf("Item %v appears before the first block anchor", item.id)
		}
		if owner := index.blockFor(item); owner != current {
			t.Fatalf("Item %v resolves to block %p, want %p", item.id, owner, current)
		}
		inBlock++
		physical++
		visible += itemVisibleLength(item)
		formatCount := Number(itemFormatCount(item))
		formatsInBlock += formatCount
		liveFormats += formatCount
	}
	if current != nil && inBlock != current.items {
		t.Fatalf("last block %p walked %d Items, records %d", current, inBlock, current.items)
	}
	if current != nil && formatsInBlock != Number(current.liveFormats) {
		t.Fatalf("last block %p walked %d live formats, records %d", current, formatsInBlock, current.liveFormats)
	}
	if len(seenAnchors) != len(index.anchors) {
		t.Fatalf("walk found %d anchors, map holds %d", len(seenAnchors), len(index.anchors))
	}
	for item, node := range index.anchors {
		if seenAnchors[item] != node {
			t.Fatalf("anchor map contains non-block-first Item %v", item.id)
		}
	}

	var validateNode func(*listPositionNode, *listPositionNode) (Number, Number, Number, int8)
	validateNode = func(node, parentNode *listPositionNode) (Number, Number, Number, int8) {
		if node == nil {
			return 0, 0, 0, 0
		}
		if node.parent != parentNode {
			t.Fatalf("node %p parent=%p, want %p", node, node.parent, parentNode)
		}
		leftItems, leftVisible, leftFormats, leftHeight := validateNode(node.left, node)
		rightItems, rightVisible, rightFormats, rightHeight := validateNode(node.right, node)
		wantItems := leftItems + node.items + rightItems
		wantVisible := leftVisible + node.visible + rightVisible
		wantFormats := leftFormats + Number(node.liveFormats) + rightFormats
		wantHeight := leftHeight
		if rightHeight > wantHeight {
			wantHeight = rightHeight
		}
		wantHeight++
		if node.subtreeItems != wantItems || node.subtreeVisible != wantVisible ||
			node.subtreeLiveFormats != wantFormats || node.height != wantHeight {
			t.Fatalf("node %p metadata=(%d,%d,%d,%d), want=(%d,%d,%d,%d)", node,
				node.subtreeItems, node.subtreeVisible, node.subtreeLiveFormats, node.height,
				wantItems, wantVisible, wantFormats, wantHeight)
		}
		balance := int(leftHeight - rightHeight)
		if balance < -1 || balance > 1 {
			t.Fatalf("node %p AVL balance=%d", node, balance)
		}
		return wantItems, wantVisible, wantFormats, wantHeight
	}
	treeItems, treeVisible, treeFormats, _ := validateNode(index.root, nil)
	if treeItems != physical || index.items != physical || physical != listItemCount(parent) {
		t.Fatalf("physical Items tree/index/walk/counter=%d/%d/%d/%d", treeItems, index.items, physical, listItemCount(parent))
	}
	if treeVisible != visible || visible != parent.GetLength() {
		t.Fatalf("visible length tree/walk/type=%d/%d/%d", treeVisible, visible, parent.GetLength())
	}
	if treeFormats != liveFormats {
		t.Fatalf("live formats tree/walk=%d/%d", treeFormats, liveFormats)
	}
}

func TestListPositionIndexBuildAndLookup(t *testing.T) {
	doc := newDoc("position-index", false, defaultGCFilter, nil, false, WithClientID(1))
	array := doc.GetArray("a")
	rng := newPerfLCG()
	model := make(ArrayAny, 0, 1200)
	for value := 0; value < 1200; value++ {
		at := rng.intn(len(model) + 1)
		array.Insert(at, ArrayAny{value})
		model = append(model, nil)
		copy(model[at+1:], model[at:])
		model[at] = value
	}
	for i := 0; i < 250; i++ {
		at := rng.intn(len(model))
		array.Delete(at, 1)
		model = append(model[:at], model[at+1:]...)
	}

	index := buildListPositionIndex(array)
	defer index.destroy()
	validateListPositionTree(t, index, array)

	for target := Number(0); target <= array.GetLength(); target++ {
		item, start, ok := index.findPosition(target)
		if !ok {
			t.Fatalf("lookup %d returned no position", target)
		}
		walkStart := Number(0)
		found := false
		for current := array.startItem(); current != nil; current = current.right {
			if current == item {
				found = true
				break
			}
			walkStart += itemVisibleLength(current)
		}
		if !found || start != walkStart {
			t.Fatalf("lookup %d returned Item %p at %d, independent walk says found=%v start=%d",
				target, item, start, found, walkStart)
		}
		if target < array.GetLength() {
			current, currentStart := walkToIndex(item, start, target)
			offset := target - currentStart
			for current != nil {
				if !current.isDeleted() && current.countable() {
					if offset < current.length {
						got := current.content.contentValues()[offset]
						if got != model[target] {
							t.Fatalf("lookup %d got %v, want %v", target, got, model[target])
						}
						break
					}
					offset -= current.length
				}
				current = current.right
			}
		}
	}
}

func TestListPositionIndexRejectsPreviousBlocksAnchor(t *testing.T) {
	doc := newDoc("missing-anchor", false, defaultGCFilter, nil, false, WithClientID(1))
	array := doc.GetArray("a")
	for i := 0; i < 400; i++ {
		array.Insert(i/2, ArrayAny{i})
	}
	index := buildListPositionIndex(array)
	defer index.destroy()

	first := array.startItem()
	for i := 0; i < listPositionBlockItems; i++ {
		first = first.right
	}
	missing := index.anchors[first]
	if missing == nil {
		t.Fatal("fixture did not create a second block")
	}
	delete(index.anchors, first)
	first.setInfo(itemInfoListBlockAnchor, false)
	probe := first
	for i := 0; i < listPositionBlockItems/2; i++ {
		probe = probe.right
	}
	if owner := index.blockFor(probe); owner != nil {
		t.Fatalf("probe crossed the missing boundary and resolved to previous block %p", owner)
	}

	// Restore the deliberately damaged anchor so deferred cleanup clears every local Info bit.
	first.setInfo(itemInfoListBlockAnchor, true)
	index.anchors[first] = missing
}

func TestSplitItemDoesNotCopyListBlockAnchor(t *testing.T) {
	doc := perfDoc()
	text := doc.GetText("t")
	text.Insert(0, "abcdefgh", Object{})
	left := text.start
	left.setInfo(itemInfoListBlockAnchor, true)
	Transact(doc, func(trans *Transaction) {
		right := splitItem(trans, left, 4)
		if right.info&itemInfoListBlockAnchor != 0 {
			t.Fatal("SplitItem copied the local block-anchor bit to its right half")
		}
	}, nil, true)
}

func TestListPositionIndexInvariantsAfterEveryMutation(t *testing.T) {
	doc := newDoc("position-invariants", false, defaultGCFilter, nil, false, WithClientID(1))
	array := doc.GetArray("a")
	rng := newPerfLCG()
	for value := 0; value < buildListPositionIndexItems+500; value++ {
		array.Insert(rng.intn(array.GetLength()+1), ArrayAny{value})
	}
	if _, index := ownedListPositionIndex(array); index == nil {
		t.Fatal("fixture did not activate the writer position index")
	}

	for op := 0; op < 240; op++ {
		if op%3 == 0 {
			array.Delete(rng.intn(array.GetLength()), 1)
		} else {
			array.Insert(rng.intn(array.GetLength()+1), ArrayAny{op})
		}
		_, index := ownedListPositionIndex(array)
		if index == nil {
			t.Fatalf("operation %d discarded the position index", op)
		}
		validateListPositionTree(t, index, array)
		validateDocPositionIndexEntries(t, doc)
	}

	// Remote integration uses the generic Item path rather than the local insertion helpers. Keep
	// an already-active index live while a concurrent client's update splices into the same list.
	remote := newDoc("position-invariants", false, defaultGCFilter, nil, false, WithClientID(2))
	remoteArray := remote.GetArray("a")
	for value := 0; value < 240; value++ {
		remoteArray.Insert(rng.intn(remoteArray.GetLength()+1), ArrayAny{-value})
	}
	update, err := EncodeStateAsUpdateV2(remote, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(doc, update, nil)
	_, index := ownedListPositionIndex(array)
	if index == nil {
		t.Fatal("remote integration discarded the active position index")
	}
	validateListPositionTree(t, index, array)
	validateDocPositionIndexEntries(t, doc)
}

func alternatingTombstoneArray(t *testing.T) (*YArray, *listPositionIndex) {
	t.Helper()
	doc := newDoc("position-tail-growth", false, defaultGCFilter, nil, false, WithClientID(1))
	array := doc.GetArray("a")
	// Deleting every append would merge the tombstones and never activate the index. Keeping every
	// other value produces alternating live/deleted Items, so physical count grows with the loop.
	for value := 0; value < buildListPositionIndexItems+3; value++ {
		array.Push(ArrayAny{value})
		if value%2 == 1 {
			array.Delete(array.GetLength()-1, 1)
		}
	}
	if items := listItemCount(array); items < buildListPositionIndexItems {
		t.Fatalf("fixture has %d physical Items, need at least %d to activate the index", items, buildListPositionIndexItems)
	}
	_, index := ownedListPositionIndex(array)
	if index == nil {
		t.Fatal("fixture crossed the activation threshold without building the position index")
	}
	validateListPositionTree(t, index, array)
	return array, index
}

func TestCompactTailAppendMaintainsPositionIndex(t *testing.T) {
	array, before := alternatingTombstoneArray(t)
	itemsBefore := listItemCount(array)

	// The tail is a live ContentAny Item, so this uses the compact append that extends the Item
	// directly instead of routing through Item.Integrate. That fast path must maintain the writer
	// position index just as the ordinary mutation boundary does.
	array.Push(ArrayAny{"tail"})
	_, after := ownedListPositionIndex(array)
	if after != before {
		t.Fatal("compact tail append discarded or rebuilt the active position index")
	}
	if items := listItemCount(array); items != itemsBefore {
		t.Fatalf("compact append changed physical Item count from %d to %d", itemsBefore, items)
	}
	validateListPositionTree(t, after, array)

	array.Delete(array.GetLength()-1, 1)
	_, afterDelete := ownedListPositionIndex(array)
	if afterDelete != before {
		t.Fatal("delete after compact tail growth discarded or rebuilt the active position index")
	}
	validateListPositionTree(t, afterDelete, array)
}

func TestListPositionIndexFindsVisibleEndBeforeTrailingTombstones(t *testing.T) {
	doc := newDoc("position-trailing-tombstones", false, defaultGCFilter, nil, false, WithClientID(1))
	array := doc.GetArray("a")
	array.Push(ArrayAny{0})
	// Insert(length) lands before trailing tombstones. Deleting that new last-visible value then
	// grows the physical tombstone suffix in reverse clock order, so it cannot merge away.
	for value := 1; value <= 128; value++ {
		array.Insert(array.GetLength(), ArrayAny{value})
		array.Delete(array.GetLength()-1, 1)
	}
	if tail := array.startItem(); tail == nil {
		t.Fatal("fixture has no Items")
	} else {
		for tail.right != nil {
			tail = tail.right
		}
		if !tail.isDeleted() {
			t.Fatal("fixture does not end in a tombstone")
		}
	}
	index := buildListPositionIndex(array)
	defer index.destroy()

	item, start, ok := index.findPosition(array.GetLength())
	if !ok {
		t.Fatal("active position index could not resolve the valid visible-end boundary")
	}
	linearStart := Number(0)
	for cursor := array.startItem(); cursor != nil && cursor != item; cursor = cursor.right {
		linearStart += itemVisibleLength(cursor)
	}
	if start != linearStart {
		t.Fatalf("visible-end block starts at %d, linear walk says %d", start, linearStart)
	}
	if start >= array.GetLength() {
		t.Fatalf("visible-end lookup returned empty trailing block at %d (length %d)", start, array.GetLength())
	}
	validateListPositionTree(t, index, array)
}

func TestTwoItemContentAnyCleanupKeepsPositionIndex(t *testing.T) {
	doc := newDoc("position-pair-cleanup", false, defaultGCFilter, nil, false, WithClientID(1))
	array := doc.GetArray("a")
	// Unlike Push, Insert(length) lands before the growing tombstone suffix. Consecutive inserts
	// there form an ordinary two-Item merge, while deleting every other value keeps physical Items
	// above the tree activation threshold.
	for value := 0; value < buildListPositionIndexItems+3; value++ {
		array.Insert(array.GetLength(), ArrayAny{value})
		if value%2 == 1 {
			array.Delete(array.GetLength()-1, 1)
		}
	}
	if items := listItemCount(array); items < buildListPositionIndexItems {
		t.Fatalf("fixture has %d physical Items, need at least %d", items, buildListPositionIndexItems)
	}
	_, index := ownedListPositionIndex(array)
	if index == nil {
		t.Fatal("fixture did not activate the position index")
	}
	validateListPositionTree(t, index, array)

	for value := 0; value < 32; value++ {
		itemsBefore := listItemCount(array)
		array.Insert(array.GetLength(), ArrayAny{value})
		if items := listItemCount(array); value == 0 && items != itemsBefore {
			t.Fatalf("step %d left %d Items after an ordinary pair merge, want %d", value, items, itemsBefore)
		}
		_, afterInsert := ownedListPositionIndex(array)
		if afterInsert != index {
			t.Fatalf("two-Item cleanup discarded or rebuilt the position index at step %d", value)
		}
		validateListPositionTree(t, afterInsert, array)
		if got := array.Get(array.GetLength() - 1); got != value {
			t.Fatalf("last value after step %d = %v, want %d", value, got, value)
		}

		array.Delete(array.GetLength()-1, 1)
		_, afterDelete := ownedListPositionIndex(array)
		if afterDelete != index {
			t.Fatalf("delete after two-Item cleanup discarded or rebuilt the index at step %d", value)
		}
		validateListPositionTree(t, afterDelete, array)
	}
	if items := listItemCount(array); items < buildListPositionIndexItems {
		t.Fatalf("fixture fell below activation threshold: %d Items", items)
	}
}

func TestListPositionIndexEntryRemovedWhenNestedTypeIsGCd(t *testing.T) {
	doc := newDoc("position-lifetime", false, defaultGCFilter, nil, false, WithClientID(1))
	root := doc.GetArray("root")
	nested := NewYArray()
	root.Insert(0, ArrayAny{nested})
	rng := newPerfLCG()
	for value := 0; value < buildListPositionIndexItems+100; value++ {
		nested.Insert(rng.intn(nested.GetLength()+1), ArrayAny{value})
	}
	state, index := ownedListPositionIndex(nested)
	if state == nil || index == nil {
		t.Fatal("nested fixture did not activate the writer position index")
	}
	validateDocPositionIndexEntries(t, doc)

	content, ok := root.start.content.(*contentType)
	if !ok || content.value != nested {
		t.Fatal("root fixture does not own the nested array")
	}
	content.gcContent(doc.store)
	if doc.positionIndexes[state] != nil {
		t.Fatal("ContentType.GC retained the nested type's side-table entry")
	}
	validateDocPositionIndexEntries(t, doc)
}

func TestListPositionIndexEntryDiesWithDeletedNestedType(t *testing.T) {
	for _, gc := range []bool{true, false} {
		t.Run(fmt.Sprintf("gc=%t", gc), func(t *testing.T) {
			doc := newDoc("position-delete-lifetime", gc, defaultGCFilter, nil, false, WithClientID(1))
			root := doc.GetArray("root")
			nested := NewYArray()
			root.Insert(0, ArrayAny{nested})
			rng := newPerfLCG()
			for value := 0; value < buildListPositionIndexItems+100; value++ {
				nested.Insert(rng.intn(nested.GetLength()+1), ArrayAny{value})
			}
			state, index := ownedListPositionIndex(nested)
			if state == nil || index == nil {
				t.Fatal("nested fixture did not activate the writer position index")
			}

			root.Delete(0, 1)
			if doc.positionIndexes[state] != nil {
				t.Fatal("deleting the nested type retained its side-table entry")
			}
			validateDocPositionIndexEntries(t, doc)
		})
	}
}

func TestSubdocumentDestroyDropsItsPositionIndexes(t *testing.T) {
	parent := newDoc("position-subdoc-parent", true, defaultGCFilter, nil, false, WithClientID(1))
	subdoc := newDoc("position-subdoc", false, defaultGCFilter, nil, false, WithClientID(2))
	array := subdoc.GetArray("a")
	rng := newPerfLCG()
	for value := 0; value < buildListPositionIndexItems+100; value++ {
		array.Insert(rng.intn(array.GetLength()+1), ArrayAny{value})
	}
	state, index := ownedListPositionIndex(array)
	if state == nil || index == nil {
		t.Fatal("subdocument fixture did not activate the writer position index")
	}

	root := parent.GetArray("root")
	root.Insert(0, ArrayAny{subdoc})
	root.Delete(0, 1)
	if subdoc.positionIndexes != nil || subdoc.positionIndexes[state] != nil {
		t.Fatal("destroyed subdocument retained writer position indexes")
	}
	validateDocPositionIndexEntries(t, subdoc)

	// Destroy releases only the accelerator. The CRDT remains readable and can lazily rebuild it.
	if got := array.GetLength(); got != buildListPositionIndexItems+100 {
		t.Fatalf("destroyed subdocument content length = %d, want %d", got, buildListPositionIndexItems+100)
	}
	array.Insert(array.GetLength()/2, ArrayAny{-1})
	if _, rebuilt := ownedListPositionIndex(array); rebuilt == nil {
		t.Fatal("mutation after Destroy did not rebuild the writer position index")
	}
}

func TestFormattingDestroysPositionIndexWithStaleFormatCounts(t *testing.T) {
	doc := newDoc("position-format", false, defaultGCFilter, nil, false, WithClientID(1))
	text := doc.GetText("t")
	rng := newPerfLCG()
	for i := 0; i < buildListPositionIndexItems+100; i++ {
		text.Insert(rng.intn(text.Length()+1), "x", Object{})
	}
	if _, index := ownedListPositionIndex(text); index == nil {
		t.Fatal("fixture did not activate the writer position index")
	}
	attrs := newObject()
	attrs.Set("bold", true)
	text.Format(10, 100, attrs)
	if _, index := ownedListPositionIndex(text); index != nil {
		t.Fatal("format mutation retained an index with stale live-format aggregates")
	}
	if text.searchMarker != nil {
		t.Fatal("formatted text did not disable mutable search markers")
	}
	validateDocPositionIndexEntries(t, doc)
}

func TestPositionIndexAggregateMismatchFallsBackToLinkedList(t *testing.T) {
	doc := newDoc("position-fallback", false, defaultGCFilter, nil, false, WithClientID(1))
	array := doc.GetArray("a")
	rng := newPerfLCG()
	model := make([]int, 0, buildListPositionIndexItems+100)
	for value := 0; value < buildListPositionIndexItems+100; value++ {
		at := rng.intn(len(model) + 1)
		array.Insert(at, ArrayAny{value})
		model = append(model, 0)
		copy(model[at+1:], model[at:])
		model[at] = value
	}
	_, index := ownedListPositionIndex(array)
	if index == nil {
		t.Fatal("fixture did not activate the writer position index")
	}
	index.root.subtreeVisible++
	at := rng.intn(len(model) + 1)
	array.Insert(at, ArrayAny{-1})
	model = append(model, 0)
	copy(model[at+1:], model[at:])
	model[at] = -1
	if _, index := ownedListPositionIndex(array); index != nil {
		t.Fatal("aggregate mismatch left the accelerator active")
	}
	got := array.ToArray()
	for i, want := range model {
		value, ok := toModelInt(got[i])
		if !ok || value != want {
			t.Fatalf("fallback element %d = %v, want %d", i, got[i], want)
		}
	}
	validateDocPositionIndexEntries(t, doc)
}

func TestListPositionNodeArenaKeepsPublishedPointersStable(t *testing.T) {
	index := &listPositionIndex{nodeBlockCapacity: 2}
	first := index.allocateNode()
	first.items = 11
	second := index.allocateNode()
	second.items = 22
	third := index.allocateNode() // forces a second backing block
	third.items = 33

	if len(index.nodeBlocks) != 2 {
		t.Fatalf("node blocks = %d, want 2", len(index.nodeBlocks))
	}
	if first.items != 11 || second.items != 22 || third.items != 33 {
		t.Fatalf("published node pointers changed across block growth: %d/%d/%d",
			first.items, second.items, third.items)
	}
	if first == second || first == third || second == third {
		t.Fatal("node arena handed out duplicate addresses")
	}
	index.destroy()
	if index.nodeBlocks != nil || index.nodeBlockUsed != 0 || index.nodeBlockCapacity != 0 {
		t.Fatal("destroy retained node-arena storage")
	}
}
