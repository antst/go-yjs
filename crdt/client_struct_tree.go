package crdt

import (
	"fmt"
	"sort"
)

const (
	clientStructTreeLeafStorage   = 65 // production maximum (64) plus one split slot
	clientStructTreeBranchStorage = 33 // production maximum (32) plus one split slot
)

// clientStructTree is the large-client representation behind clientStructList.
// The build-selected hybrid policy controls whether it can activate; callers do
// not observe or retain its leaves directly.
type clientStructTree struct {
	root        *clientStructTreeNode
	first       *clientStructTreeLeaf
	last        *clientStructTreeLeaf
	structs     int
	generation  uint64
	leafLimit   int
	branchLimit int
}

// clientStructTreeNode is the common, pointer-stable header embedded in leaves
// and branches. Branches store one header pointer per child; the leaf/branch
// back-pointers form a tagged union without a per-child interface value.
type clientStructTreeNode struct {
	parent   *clientStructTreeBranch
	endClock Number // exclusive maximum clock in this subtree
	structs  int
	leaf     *clientStructTreeLeaf
	branch   *clientStructTreeBranch
}

type clientStructTreeLeaf struct {
	node  clientStructTreeNode
	tree  *clientStructTree
	prev  *clientStructTreeLeaf
	next  *clientStructTreeLeaf
	used  int
	items [clientStructTreeLeafStorage]abstractStruct
}

type clientStructTreeBranch struct {
	node     clientStructTreeNode
	used     int
	children [clientStructTreeBranchStorage]*clientStructTreeNode
}

type clientStructTreeCursor struct {
	tree       *clientStructTree
	leaf       *clientStructTreeLeaf
	slot       int
	generation uint64
}

// clientStructTreeChunkCursor exposes one caller-borrowed leaf segment at a
// time. It keeps bulk encoders and scans O(leaves) without leaking leaf storage
// through the clientStructList API.
type clientStructTreeChunkCursor struct {
	tree       *clientStructTree
	leaf       *clientStructTreeLeaf
	start      int
	generation uint64
}

func newClientStructTree(leafLimit, branchLimit int) *clientStructTree {
	if leafLimit < 2 || leafLimit >= clientStructTreeLeafStorage {
		panic("clientStructTree: leaf limit outside fixed storage")
	}
	if branchLimit < 3 || branchLimit >= clientStructTreeBranchStorage {
		panic("clientStructTree: branch limit outside fixed storage")
	}
	return &clientStructTree{leafLimit: leafLimit, branchLimit: branchLimit}
}

func newClientStructTreeFromFlat(values []abstractStruct, leafLimit, branchLimit int) *clientStructTree {
	tree := newClientStructTree(leafLimit, branchLimit)
	if len(values) == 0 {
		return tree
	}

	leafSizes := clientStructTreePartition(len(values), leafLimit)
	nodes := make([]*clientStructTreeNode, 0, len(leafSizes))
	offset := 0
	for _, size := range leafSizes {
		leaf := tree.newLeaf()
		copy(leaf.items[:size], values[offset:offset+size])
		leaf.used = size
		if tree.last == nil {
			tree.first = leaf
		} else {
			tree.last.next = leaf
			leaf.prev = tree.last
		}
		tree.last = leaf
		tree.refreshLeaf(leaf)
		nodes = append(nodes, &leaf.node)
		offset += size
	}

	for len(nodes) > 1 {
		groupSizes := clientStructTreePartition(len(nodes), branchLimit)
		parents := make([]*clientStructTreeNode, 0, len(groupSizes))
		offset = 0
		for _, size := range groupSizes {
			branch := tree.newBranch()
			branch.used = size
			copy(branch.children[:size], nodes[offset:offset+size])
			for i := 0; i < size; i++ {
				branch.children[i].parent = branch
			}
			tree.refreshBranch(branch)
			parents = append(parents, &branch.node)
			offset += size
		}
		nodes = parents
	}

	tree.root = nodes[0]
	tree.root.parent = nil
	tree.structs = len(values)
	return tree
}

func clientStructTreePartition(total, maximum int) []int {
	if total <= 0 {
		return nil
	}
	groups := (total + maximum - 1) / maximum
	base, extra := total/groups, total%groups
	sizes := make([]int, groups)
	for i := range sizes {
		sizes[i] = base
		if i < extra {
			sizes[i]++
		}
	}
	return sizes
}

func (t *clientStructTree) newLeaf() *clientStructTreeLeaf {
	leaf := &clientStructTreeLeaf{tree: t}
	leaf.node.leaf = leaf
	return leaf
}

func (t *clientStructTree) newBranch() *clientStructTreeBranch {
	branch := &clientStructTreeBranch{}
	branch.node.branch = branch
	return branch
}

func (t *clientStructTree) Len() int {
	if t == nil {
		return 0
	}
	return t.structs
}

func (t *clientStructTree) Snapshot(dst []abstractStruct) []abstractStruct {
	dst = dst[:0]
	if t == nil || t.structs == 0 {
		return dst
	}
	if cap(dst) < t.structs {
		dst = make([]abstractStruct, 0, t.structs)
	}
	for leaf := t.first; leaf != nil; leaf = leaf.next {
		dst = append(dst, leaf.items[:leaf.used]...)
	}
	return dst
}

func (t *clientStructTree) First() (clientStructTreeCursor, bool) {
	if t == nil || t.first == nil {
		return clientStructTreeCursor{}, false
	}
	return t.cursorAt(t.first, 0), true
}

func (t *clientStructTree) Last() (clientStructTreeCursor, bool) {
	if t == nil || t.last == nil {
		return clientStructTreeCursor{}, false
	}
	return t.cursorAt(t.last, t.last.used-1), true
}

func (t *clientStructTree) Find(clock Number) (clientStructTreeCursor, error) {
	leaf := t.findLeaf(clock)
	for leaf != nil {
		index, err := findIndexSS(leaf.items[:leaf.used], clock)
		if err == nil {
			return t.cursorAt(leaf, index), nil
		}
		leaf = leaf.next
		if leaf == nil || leaf.used == 0 || leaf.items[0].getID().Clock > clock {
			break
		}
	}
	return clientStructTreeCursor{}, errStructNotFound
}

func (t *clientStructTree) At(position int) (clientStructTreeCursor, bool) {
	if t == nil || position < 0 || position >= t.structs {
		return clientStructTreeCursor{}, false
	}
	node := t.root
	for node.branch != nil {
		branch := node.branch
		child := 0
		for ; child < branch.used; child++ {
			if position < branch.children[child].structs {
				break
			}
			position -= branch.children[child].structs
		}
		node = branch.children[child]
	}
	return t.cursorAt(node.leaf, position), true
}

func (t *clientStructTree) Index(cursor clientStructTreeCursor) int {
	t.requireCursor(cursor)
	position := cursor.slot
	node := &cursor.leaf.node
	for node.parent != nil {
		parent := node.parent
		index := parent.indexOf(node)
		for i := 0; i < index; i++ {
			position += parent.children[i].structs
		}
		node = &parent.node
	}
	return position
}

func (t *clientStructTree) ChunkFrom(cursor clientStructTreeCursor) clientStructTreeChunkCursor {
	t.requireCursor(cursor)
	return clientStructTreeChunkCursor{
		tree: t, leaf: cursor.leaf, start: cursor.slot, generation: t.generation,
	}
}

func (c clientStructTreeChunkCursor) Values() []abstractStruct {
	if c.tree == nil || c.leaf == nil || c.generation != c.tree.generation || c.leaf.tree != c.tree || c.start < 0 || c.start >= c.leaf.used {
		panic("clientStructTreeChunkCursor.Values: invalid cursor")
	}
	return c.leaf.items[c.start:c.leaf.used]
}

func (c clientStructTreeChunkCursor) Next() (clientStructTreeChunkCursor, bool) {
	if c.tree == nil || c.leaf == nil {
		return clientStructTreeChunkCursor{}, false
	}
	_ = c.Values()
	if c.leaf.next == nil {
		return clientStructTreeChunkCursor{}, false
	}
	return clientStructTreeChunkCursor{
		tree: c.tree, leaf: c.leaf.next, generation: c.generation,
	}, true
}

func (t *clientStructTree) findLeaf(clock Number) *clientStructTreeLeaf {
	node := t.root
	for node != nil && node.branch != nil {
		branch := node.branch
		index := sort.Search(branch.used, func(i int) bool {
			return branch.children[i].endClock > clock
		})
		if index == branch.used {
			return nil
		}
		node = branch.children[index]
	}
	if node == nil {
		return nil
	}
	return node.leaf
}

func (t *clientStructTree) findValue(value abstractStruct) (clientStructTreeCursor, bool) {
	if value == nil {
		return clientStructTreeCursor{}, false
	}
	clock := value.getID().Clock
	leaf := t.findLeaf(clock)
	for leaf != nil {
		for slot := 0; slot < leaf.used; slot++ {
			candidate := leaf.items[slot]
			start := candidate.getID().Clock
			if start > clock {
				return clientStructTreeCursor{}, false
			}
			if candidate == value {
				return t.cursorAt(leaf, slot), true
			}
		}
		leaf = leaf.next
	}
	return clientStructTreeCursor{}, false
}

func (t *clientStructTree) Append(value abstractStruct) clientStructTreeCursor {
	if t.root == nil {
		leaf := t.newLeaf()
		leaf.items[0] = value
		leaf.used = 1
		t.first, t.last, t.root, t.structs = leaf, leaf, &leaf.node, 1
		t.refreshLeaf(leaf)
		return t.cursorAt(leaf, 0)
	}

	leaf := t.last
	if leaf.used < t.leafLimit {
		slot := leaf.used
		leaf.items[slot] = value
		leaf.used++
		t.structs++
		t.refreshUp(&leaf.node)
		return t.cursorAt(leaf, slot)
	}

	// Never split the existing rightmost leaf on append: cursors into it remain
	// valid because neither their leaf pointer nor slot changes.
	right := t.newLeaf()
	right.items[0] = value
	right.used = 1
	right.prev = leaf
	leaf.next = right
	t.last = right
	t.structs++
	t.refreshLeaf(right)
	t.insertSiblingAfter(&leaf.node, &right.node)
	return t.cursorAt(right, 0)
}

func (t *clientStructTree) InsertAfter(cursor clientStructTreeCursor, value abstractStruct) clientStructTreeCursor {
	t.requireCursor(cursor)
	t.generation++
	leaf := cursor.leaf
	slot := cursor.slot + 1
	copy(leaf.items[slot+1:leaf.used+1], leaf.items[slot:leaf.used])
	leaf.items[slot] = value
	leaf.used++
	t.structs++

	if leaf.used <= t.leafLimit {
		t.refreshUp(&leaf.node)
		return t.cursorAt(leaf, slot)
	}

	noteClientStructTreeLeafSplit()
	split := leaf.used / 2
	right := t.newLeaf()
	right.used = leaf.used - split
	copy(right.items[:right.used], leaf.items[split:leaf.used])
	for i := split; i < leaf.used; i++ {
		leaf.items[i] = nil
	}
	leaf.used = split
	right.next = leaf.next
	right.prev = leaf
	if leaf.next != nil {
		leaf.next.prev = right
	} else {
		t.last = right
	}
	leaf.next = right
	t.refreshLeaf(leaf)
	t.refreshLeaf(right)
	t.insertSiblingAfter(&leaf.node, &right.node)
	if slot < split {
		return t.cursorAt(leaf, slot)
	}
	return t.cursorAt(right, slot-split)
}

// Remove deletes an inclusive range. Raw leaf compaction happens before any
// rebalance, so both endpoint pointers remain meaningful for the whole range.
func (t *clientStructTree) Remove(first, last clientStructTreeCursor) (clientStructTreeCursor, bool) {
	t.requireCursor(first)
	t.requireCursor(last)
	removed, successor, ok := t.rangeLengthAndSuccessor(first, last)
	if !ok {
		panic("clientStructTree.Remove: reversed or disconnected cursor range")
	}
	t.generation++

	var touchedStorage [9]*clientStructTreeLeaf
	touched := touchedStorage[:0]
	// MergeWith grows the predecessor before removing the absorbed range. If
	// that predecessor sits in the previous leaf, its end-clock aggregate also
	// changed even though raw compaction starts in first.leaf.
	if first.slot == 0 && first.leaf.prev != nil {
		touched = append(touched, first.leaf.prev)
	}
	leaf := first.leaf
	for {
		start, end := 0, leaf.used
		if leaf == first.leaf {
			start = first.slot
		}
		if leaf == last.leaf {
			end = last.slot + 1
		}
		copy(leaf.items[start:], leaf.items[end:leaf.used])
		newUsed := leaf.used - (end - start)
		for i := newUsed; i < leaf.used; i++ {
			leaf.items[i] = nil
		}
		leaf.used = newUsed
		touched = append(touched, leaf)
		if leaf == last.leaf {
			break
		}
		leaf = leaf.next
	}
	t.structs -= removed

	if t.structs == 0 {
		t.detach(t.root)
		for leaf := t.first; leaf != nil; {
			next := leaf.next
			leaf.tree, leaf.prev, leaf.next = nil, nil, nil
			leaf.node.parent = nil
			leaf = next
		}
		t.root, t.first, t.last = nil, nil, nil
		return clientStructTreeCursor{}, false
	}

	// Raw compaction changes only these leaves. Refresh their unique ancestor
	// paths bottom-up; rebuilding the whole tree here would make a one-element
	// removal linear in the total client history.
	t.refreshTouchedLeaves(touched)
	var queueStorage [24]*clientStructTreeLeaf
	queue := queueStorage[:0]
	for _, candidate := range touched {
		queue = append(queue, candidate.prev, candidate, candidate.next)
	}
	for len(queue) > 0 {
		candidate := queue[0]
		queue = queue[1:]
		if !t.leafAttached(candidate) || candidate.node.parent == nil || candidate.used >= t.minLeaf() {
			continue
		}
		survivor := t.rebalanceLeaf(candidate)
		if survivor != nil {
			queue = append(queue, survivor.prev, survivor, survivor.next)
		}
	}
	if successor == nil {
		return clientStructTreeCursor{}, false
	}
	next, found := t.findValue(successor)
	if !found {
		panic("clientStructTree.Remove: successor disappeared during rebalance")
	}
	return next, true
}

func (t *clientStructTree) refreshTouchedLeaves(leaves []*clientStructTreeLeaf) {
	var currentStorage [8]*clientStructTreeBranch
	current := currentStorage[:0]
	var previous *clientStructTreeBranch
	for _, leaf := range leaves {
		t.refreshLeaf(leaf)
		if parent := leaf.node.parent; parent != nil && parent != previous {
			current = append(current, parent)
			previous = parent
		}
	}

	var nextStorage [8]*clientStructTreeBranch
	next := nextStorage[:0]
	for len(current) > 0 {
		previous = nil
		next = next[:0]
		for _, branch := range current {
			t.refreshBranch(branch)
			if parent := branch.node.parent; parent != nil && parent != previous {
				next = append(next, parent)
				previous = parent
			}
		}
		current, next = next, current[:0]
	}
}

func (t *clientStructTree) detach(node *clientStructTreeNode) {
	if node == nil {
		return
	}
	if node.branch != nil {
		branch := node.branch
		for i := 0; i < branch.used; i++ {
			t.detach(branch.children[i])
			branch.children[i] = nil
		}
		branch.used = 0
	}
	node.parent = nil
}

func (t *clientStructTree) Replace(cursor clientStructTreeCursor, value abstractStruct) {
	t.requireCursor(cursor)
	cursor.leaf.items[cursor.slot] = value
	t.refreshUp(&cursor.leaf.node)
}

func (t *clientStructTree) rangeLengthAndSuccessor(first, last clientStructTreeCursor) (int, abstractStruct, bool) {
	length := 0
	cursor := first
	for {
		length++
		if cursor.leaf == last.leaf && cursor.slot == last.slot {
			next, more := cursor.Next()
			if !more {
				return length, nil, true
			}
			return length, next.Value(), true
		}
		next, more := cursor.Next()
		if !more {
			return 0, nil, false
		}
		cursor = next
	}
}

func (c clientStructTreeCursor) Valid() bool {
	return c.tree != nil && c.leaf != nil && c.leaf.tree == c.tree &&
		c.generation == c.tree.generation && c.slot >= 0 && c.slot < c.leaf.used
}

func (c clientStructTreeCursor) Value() abstractStruct {
	if !c.Valid() {
		panic("clientStructTreeCursor.Value: invalid cursor")
	}
	return c.leaf.items[c.slot]
}

func (c clientStructTreeCursor) Next() (clientStructTreeCursor, bool) {
	if c.tree == nil || c.leaf == nil {
		return clientStructTreeCursor{}, false
	}
	if !c.Valid() {
		panic("clientStructTreeCursor.Next: invalid cursor")
	}
	if c.slot+1 < c.leaf.used {
		return c.tree.cursorAt(c.leaf, c.slot+1), true
	}
	if c.leaf.next == nil {
		return clientStructTreeCursor{}, false
	}
	return c.tree.cursorAt(c.leaf.next, 0), true
}

func (c clientStructTreeCursor) Prev() (clientStructTreeCursor, bool) {
	if c.tree == nil || c.leaf == nil {
		return clientStructTreeCursor{}, false
	}
	if !c.Valid() {
		panic("clientStructTreeCursor.Prev: invalid cursor")
	}
	if c.slot > 0 {
		return c.tree.cursorAt(c.leaf, c.slot-1), true
	}
	if c.leaf.prev == nil {
		return clientStructTreeCursor{}, false
	}
	return c.tree.cursorAt(c.leaf.prev, c.leaf.prev.used-1), true
}

func (t *clientStructTree) cursorAt(leaf *clientStructTreeLeaf, slot int) clientStructTreeCursor {
	return clientStructTreeCursor{tree: t, leaf: leaf, slot: slot, generation: t.generation}
}

func (t *clientStructTree) requireCursor(cursor clientStructTreeCursor) {
	if cursor.tree != t || !cursor.Valid() {
		panic("clientStructTree: cursor is foreign, stale, or invalid")
	}
}

func (t *clientStructTree) minLeaf() int {
	return (t.leafLimit + 1) / 2
}

func (t *clientStructTree) minBranch() int {
	return (t.branchLimit + 1) / 2
}

func (t *clientStructTree) refreshLeaf(leaf *clientStructTreeLeaf) {
	leaf.node.structs = leaf.used
	leaf.node.endClock = 0
	for i := 0; i < leaf.used; i++ {
		value := leaf.items[i]
		end := value.getID().Clock + value.structLength()
		if end > leaf.node.endClock {
			leaf.node.endClock = end
		}
	}
}

func (t *clientStructTree) refreshBranch(branch *clientStructTreeBranch) {
	branch.node.structs = 0
	branch.node.endClock = 0
	for i := 0; i < branch.used; i++ {
		child := branch.children[i]
		branch.node.structs += child.structs
		if child.endClock > branch.node.endClock {
			branch.node.endClock = child.endClock
		}
	}
}

func (t *clientStructTree) refreshUp(node *clientStructTreeNode) {
	for node != nil {
		if node.leaf != nil {
			t.refreshLeaf(node.leaf)
		} else {
			t.refreshBranch(node.branch)
		}
		if node.parent == nil {
			return
		}
		node = &node.parent.node
	}
}

func (branch *clientStructTreeBranch) indexOf(node *clientStructTreeNode) int {
	for i := 0; i < branch.used; i++ {
		if branch.children[i] == node {
			return i
		}
	}
	panic("clientStructTree: parent does not contain child")
}

func (t *clientStructTree) insertSiblingAfter(left, right *clientStructTreeNode) {
	parent := left.parent
	if parent == nil {
		root := t.newBranch()
		root.used = 2
		root.children[0], root.children[1] = left, right
		left.parent, right.parent = root, root
		t.refreshBranch(root)
		t.root = &root.node
		return
	}
	index := parent.indexOf(left) + 1
	copy(parent.children[index+1:parent.used+1], parent.children[index:parent.used])
	parent.children[index] = right
	right.parent = parent
	parent.used++
	t.refreshUp(&parent.node)
	if parent.used > t.branchLimit {
		t.splitBranch(parent)
	}
}

func (t *clientStructTree) splitBranch(branch *clientStructTreeBranch) {
	noteClientStructTreeBranchSplit()
	split := branch.used / 2
	right := t.newBranch()
	right.used = branch.used - split
	copy(right.children[:right.used], branch.children[split:branch.used])
	for i := 0; i < right.used; i++ {
		right.children[i].parent = right
	}
	for i := split; i < branch.used; i++ {
		branch.children[i] = nil
	}
	branch.used = split
	t.refreshBranch(branch)
	t.refreshBranch(right)
	t.insertSiblingAfter(&branch.node, &right.node)
}

func (t *clientStructTree) leafAttached(leaf *clientStructTreeLeaf) bool {
	return leaf != nil && leaf.tree == t && (leaf.node.parent != nil || t.root == &leaf.node)
}

func (t *clientStructTree) rebalanceLeaf(leaf *clientStructTreeLeaf) *clientStructTreeLeaf {
	parent := leaf.node.parent
	if parent == nil || leaf.used >= t.minLeaf() {
		return leaf
	}
	noteClientStructTreeRebalance()
	index := parent.indexOf(&leaf.node)
	var left, right *clientStructTreeLeaf
	if index > 0 {
		left = parent.children[index-1].leaf
	}
	if index+1 < parent.used {
		right = parent.children[index+1].leaf
	}
	if left != nil && left.used > t.minLeaf() {
		copy(leaf.items[1:leaf.used+1], leaf.items[:leaf.used])
		leaf.items[0] = left.items[left.used-1]
		left.items[left.used-1] = nil
		left.used--
		leaf.used++
		t.refreshUp(&left.node)
		t.refreshUp(&leaf.node)
		return leaf
	}
	if right != nil && right.used > t.minLeaf() {
		leaf.items[leaf.used] = right.items[0]
		leaf.used++
		copy(right.items[:], right.items[1:right.used])
		right.used--
		right.items[right.used] = nil
		t.refreshUp(&right.node)
		t.refreshUp(&leaf.node)
		return leaf
	}
	if left != nil {
		copy(left.items[left.used:], leaf.items[:leaf.used])
		left.used += leaf.used
		for i := 0; i < leaf.used; i++ {
			leaf.items[i] = nil
		}
		leaf.used = 0
		t.unlinkLeaf(leaf)
		t.refreshLeaf(left)
		t.removeBranchChild(parent, index)
		return left
	}
	if right == nil {
		panic("clientStructTree: underfull leaf has no sibling")
	}
	copy(leaf.items[leaf.used:], right.items[:right.used])
	leaf.used += right.used
	for i := 0; i < right.used; i++ {
		right.items[i] = nil
	}
	right.used = 0
	t.unlinkLeaf(right)
	t.refreshLeaf(leaf)
	t.removeBranchChild(parent, index+1)
	return leaf
}

func (t *clientStructTree) unlinkLeaf(leaf *clientStructTreeLeaf) {
	if leaf.prev != nil {
		leaf.prev.next = leaf.next
	} else {
		t.first = leaf.next
	}
	if leaf.next != nil {
		leaf.next.prev = leaf.prev
	} else {
		t.last = leaf.prev
	}
	leaf.prev, leaf.next, leaf.tree = nil, nil, nil
}

func (t *clientStructTree) removeBranchChild(parent *clientStructTreeBranch, index int) {
	removed := parent.children[index]
	copy(parent.children[index:], parent.children[index+1:parent.used])
	parent.used--
	parent.children[parent.used] = nil
	removed.parent = nil
	t.refreshBranch(parent)
	t.rebalanceBranch(parent)
}

func (t *clientStructTree) rebalanceBranch(branch *clientStructTreeBranch) {
	if branch.node.parent == nil {
		if branch.used == 1 {
			child := branch.children[0]
			branch.children[0] = nil
			branch.used = 0
			child.parent = nil
			t.root = child
		}
		t.refreshUp(t.root)
		return
	}
	if branch.used >= t.minBranch() {
		t.refreshUp(&branch.node)
		return
	}
	parent := branch.node.parent
	index := parent.indexOf(&branch.node)
	var left, right *clientStructTreeBranch
	if index > 0 {
		left = parent.children[index-1].branch
	}
	if index+1 < parent.used {
		right = parent.children[index+1].branch
	}
	if left != nil && left.used > t.minBranch() {
		copy(branch.children[1:branch.used+1], branch.children[:branch.used])
		child := left.children[left.used-1]
		left.children[left.used-1] = nil
		left.used--
		branch.children[0] = child
		child.parent = branch
		branch.used++
		t.refreshUp(&left.node)
		t.refreshUp(&branch.node)
		return
	}
	if right != nil && right.used > t.minBranch() {
		child := right.children[0]
		branch.children[branch.used] = child
		child.parent = branch
		branch.used++
		copy(right.children[:], right.children[1:right.used])
		right.used--
		right.children[right.used] = nil
		t.refreshUp(&right.node)
		t.refreshUp(&branch.node)
		return
	}
	if left != nil {
		copy(left.children[left.used:], branch.children[:branch.used])
		for i := 0; i < branch.used; i++ {
			left.children[left.used+i].parent = left
			branch.children[i] = nil
		}
		left.used += branch.used
		branch.used = 0
		t.refreshBranch(left)
		t.removeBranchChild(parent, index)
		return
	}
	if right == nil {
		panic("clientStructTree: underfull branch has no sibling")
	}
	copy(branch.children[branch.used:], right.children[:right.used])
	for i := 0; i < right.used; i++ {
		branch.children[branch.used+i].parent = branch
		right.children[i] = nil
	}
	branch.used += right.used
	right.used = 0
	t.refreshBranch(branch)
	t.removeBranchChild(parent, index+1)
}

func (t *clientStructTree) appendDeletedRanges(dst []*deleteItem) []*deleteItem {
	inRun := false
	var clock, length Number
	flush := func() {
		if inRun {
			dst = append(dst, newDeleteItem(clock, length))
			inRun = false
		}
	}
	for leaf := t.first; leaf != nil; leaf = leaf.next {
		for _, value := range leaf.items[:leaf.used] {
			if !value.isDeleted() {
				flush()
				continue
			}
			if !inRun {
				clock, length, inRun = value.getID().Clock, 0, true
			}
			length += value.structLength()
		}
	}
	flush()
	return dst
}

func (t *clientStructTree) deletedRangeCount() int {
	ranges := 0
	inRun := false
	for leaf := t.first; leaf != nil; leaf = leaf.next {
		for _, value := range leaf.items[:leaf.used] {
			deleted := value.isDeleted()
			if deleted && !inRun {
				ranges++
			}
			inRun = deleted
		}
	}
	return ranges
}

func (t *clientStructTree) writeDeletedRanges(encoder dsEncoder, client Number, rangeCount int) error {
	encoder.resetDS()
	writeEncoderRestVarUint(encoder, uint64(client))
	writeEncoderRestVarUint(encoder, uint64(rangeCount))

	inRun := false
	var clock, length Number
	flush := func() error {
		if !inRun {
			return nil
		}
		encoder.writeDSClock(clock)
		if err := encoder.writeDSLength(length); err != nil {
			return err
		}
		inRun = false
		return nil
	}
	for leaf := t.first; leaf != nil; leaf = leaf.next {
		for _, value := range leaf.items[:leaf.used] {
			if !value.isDeleted() {
				if err := flush(); err != nil {
					return err
				}
				continue
			}
			if !inRun {
				clock, length, inRun = value.getID().Clock, 0, true
			}
			length += value.structLength()
		}
	}
	return flush()
}

func (t *clientStructTree) Validate() error {
	if t.root == nil {
		if t.structs != 0 || t.first != nil || t.last != nil {
			return fmt.Errorf("empty root with structs/first/last=%d/%p/%p", t.structs, t.first, t.last)
		}
		return nil
	}
	if t.root.parent != nil {
		return fmt.Errorf("root has parent %p", t.root.parent)
	}
	seen := make(map[*clientStructTreeNode]bool)
	leaves := make([]*clientStructTreeLeaf, 0)
	depth, structs, endClock, err := t.validateNode(t.root, true, seen, &leaves)
	if err != nil {
		return err
	}
	if depth < 1 || structs != t.structs || t.root.structs != t.structs || t.root.endClock != endClock {
		return fmt.Errorf("root depth/count/end=%d/%d/%d, tree=%d root=%d/%d", depth, structs, endClock, t.structs, t.root.structs, t.root.endClock)
	}
	if len(leaves) == 0 || t.first != leaves[0] || t.last != leaves[len(leaves)-1] {
		return fmt.Errorf("leaf endpoints=%p/%p, want %p/%p", t.first, t.last, leaves[0], leaves[len(leaves)-1])
	}
	var previous abstractStruct
	for i, leaf := range leaves {
		var wantPrev, wantNext *clientStructTreeLeaf
		if i > 0 {
			wantPrev = leaves[i-1]
		}
		if i+1 < len(leaves) {
			wantNext = leaves[i+1]
		}
		if leaf.prev != wantPrev || leaf.next != wantNext || leaf.tree != t {
			return fmt.Errorf("leaf %p links/tree=%p/%p/%p, want %p/%p/%p", leaf, leaf.prev, leaf.next, leaf.tree, wantPrev, wantNext, t)
		}
		for slot := 0; slot < leaf.used; slot++ {
			value := leaf.items[slot]
			if value == nil {
				return fmt.Errorf("leaf %p slot %d is nil", leaf, slot)
			}
			if previous != nil && previous.getID().Clock > value.getID().Clock {
				return fmt.Errorf("clock order %d before %d", previous.getID().Clock, value.getID().Clock)
			}
			previous = value
		}
		for slot := leaf.used; slot < len(leaf.items); slot++ {
			if leaf.items[slot] != nil {
				return fmt.Errorf("leaf %p retains item in unused slot %d", leaf, slot)
			}
		}
	}
	return nil
}

func (t *clientStructTree) validateNode(node *clientStructTreeNode, root bool, seen map[*clientStructTreeNode]bool, leaves *[]*clientStructTreeLeaf) (depth, structs int, endClock Number, err error) {
	if node == nil || seen[node] {
		return 0, 0, 0, fmt.Errorf("nil or duplicate tree node %p", node)
	}
	seen[node] = true
	if (node.leaf == nil) == (node.branch == nil) {
		return 0, 0, 0, fmt.Errorf("node %p has invalid leaf/branch tag", node)
	}
	if node.leaf != nil {
		leaf := node.leaf
		// Append creates a fresh right leaf instead of splitting the old one so
		// previously published leaf/slot cursors remain valid. That right-edge
		// append frontier may be below the ordinary minimum until it fills.
		if leaf.used <= 0 || leaf.used > t.leafLimit || !root && leaf != t.last && leaf.used < t.minLeaf() {
			return 0, 0, 0, fmt.Errorf("leaf %p occupancy=%d root=%v", leaf, leaf.used, root)
		}
		wantEnd := Number(0)
		for i := 0; i < leaf.used; i++ {
			value := leaf.items[i]
			if value == nil {
				return 0, 0, 0, fmt.Errorf("leaf %p slot %d is nil", leaf, i)
			}
			end := value.getID().Clock + value.structLength()
			if end > wantEnd {
				wantEnd = end
			}
		}
		if node.structs != leaf.used || node.endClock != wantEnd {
			return 0, 0, 0, fmt.Errorf("leaf %p aggregate=%d/%d, want %d/%d", leaf, node.structs, node.endClock, leaf.used, wantEnd)
		}
		*leaves = append(*leaves, leaf)
		return 1, leaf.used, wantEnd, nil
	}

	branch := node.branch
	minimum := t.minBranch()
	if root {
		minimum = 2
	}
	if branch.used < minimum || branch.used > t.branchLimit {
		return 0, 0, 0, fmt.Errorf("branch %p occupancy=%d root=%v", branch, branch.used, root)
	}
	wantDepth := 0
	wantStructs := 0
	wantEnd := Number(0)
	for i := 0; i < branch.used; i++ {
		child := branch.children[i]
		if child == nil || child.parent != branch {
			return 0, 0, 0, fmt.Errorf("branch %p child %d parent=%p", branch, i, child)
		}
		childDepth, childStructs, childEnd, childErr := t.validateNode(child, false, seen, leaves)
		if childErr != nil {
			return 0, 0, 0, childErr
		}
		if wantDepth == 0 {
			wantDepth = childDepth
		} else if childDepth != wantDepth {
			return 0, 0, 0, fmt.Errorf("branch %p child depths=%d/%d", branch, wantDepth, childDepth)
		}
		if i > 0 && branch.children[i-1].endClock > childEnd {
			return 0, 0, 0, fmt.Errorf("branch %p separators descend at child %d", branch, i)
		}
		wantStructs += childStructs
		if childEnd > wantEnd {
			wantEnd = childEnd
		}
	}
	for i := branch.used; i < len(branch.children); i++ {
		if branch.children[i] != nil {
			return 0, 0, 0, fmt.Errorf("branch %p retains child in unused slot %d", branch, i)
		}
	}
	if node.structs != wantStructs || node.endClock != wantEnd {
		return 0, 0, 0, fmt.Errorf("branch %p aggregate=%d/%d, want %d/%d", branch, node.structs, node.endClock, wantStructs, wantEnd)
	}
	return wantDepth + 1, wantStructs, wantEnd, nil
}
