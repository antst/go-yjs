package crdt

// A mutable position index is built from blocks of linked Items rather than from
// one tree node per Item. It is deliberately separate from listReadIndex: the
// latter is immutable and safe for concurrent readers, while this index is used
// only by the single-writer mutation path.

const (
	// Eight Items per block is the mutation-latency knee at 64k-256k: four doubles
	// activation/index memory without improving the walk, while sixteen is ~30% slower at 256k.
	listPositionBlockItems      = 8
	listPositionBlockMaxItems   = 16
	itemInfoListBlockAnchor     = bit6
	itemInfoListPositionIndexed = bit7
	buildListPositionIndexItems = 16_000
	// Formatted text has no mutable marker cache, so every pre-index position walks from Start.
	// Its block index therefore pays back much earlier than the plain-sequence index.
	buildFormattedListPositionIndexItems = 512
)

type abstractTypeCarrier interface {
	abstractTypeState() *abstractTypeBase
}

func (t *abstractTypeBase) abstractTypeState() *abstractTypeBase {
	return t
}

func (t *abstractTypeBase) destroyOwnedListPositionIndex() {
	doc := t.doc
	if doc == nil || doc.positionIndexes == nil {
		return
	}
	index := doc.positionIndexes[t]
	if index == nil {
		return
	}
	for item := t.start; item != nil; item = item.right {
		item.setInfo(itemInfoListPositionIndexed, false)
	}
	index.destroy()
	delete(doc.positionIndexes, t)
	if len(doc.positionIndexes) == 0 {
		doc.positionIndexes = nil
	}
}

func abstractTypeState(parent abstractType) *abstractTypeBase {
	if carrier, ok := parent.(abstractTypeCarrier); ok {
		return carrier.abstractTypeState()
	}
	return nil
}

func destroyListPositionIndex(parent abstractType) {
	if state := abstractTypeState(parent); state != nil {
		state.destroyOwnedListPositionIndex()
	}
}

func (doc *Doc) destroyListPositionIndexes() {
	for state := range doc.positionIndexes {
		state.destroyOwnedListPositionIndex()
	}
}

func ownedListPositionIndex(parent abstractType) (*abstractTypeBase, *listPositionIndex) {
	state := abstractTypeState(parent)
	if state == nil || state.doc == nil || state.doc.positionIndexes == nil {
		return state, nil
	}
	return state, state.doc.positionIndexes[state]
}

func clearMutableListMarkers(parent abstractType) {
	markers := parent.getSearchMarker()
	if markers == nil || *markers == nil {
		return
	}
	for _, marker := range *markers {
		if marker != nil && marker.p != nil {
			marker.p.setMarker(false)
		}
	}
	*markers = (*markers)[:0]
}

func listPositionIndexActivationItems(parent abstractType) Number {
	markers := parent.getSearchMarker()
	if markers != nil && *markers == nil {
		return buildFormattedListPositionIndexItems
	}
	return buildListPositionIndexItems
}

// indexedMutationPosition activates the writer-only block tree at large physical Item counts.
// Aggregate validation is constant-time and protects production from a missed maintenance hook:
// on any discrepancy the pure accelerator is destroyed and the caller uses the linked-list path.
func activeListPositionIndex(parent abstractType, itemCount Number, requireMarkers bool) *listPositionIndex {
	markers := parent.getSearchMarker()
	if requireMarkers && (markers == nil || *markers == nil) {
		return nil
	}
	state := abstractTypeState(parent)
	if state == nil || state.doc == nil {
		return nil
	}
	index := state.doc.positionIndexes[state]
	if index == nil {
		clearMutableListMarkers(parent)
		index = buildListPositionIndex(parent)
		if state.doc.positionIndexes == nil {
			state.doc.positionIndexes = make(map[*abstractTypeBase]*listPositionIndex)
		}
		state.doc.positionIndexes[state] = index
	}
	if index.root == nil || index.items != itemCount ||
		index.root.subtreeVisible != parent.GetLength() {
		destroyListPositionIndex(parent)
		return nil
	}
	return index
}

func indexedMutationPosition(parent abstractType, target, itemCount Number) (*itemStruct, Number, bool) {
	index := activeListPositionIndex(parent, itemCount, true)
	if index == nil {
		return nil, 0, false
	}
	item, start, ok := index.findPosition(target)
	if !ok {
		destroyListPositionIndex(parent)
		return nil, 0, false
	}
	return item, start, true
}

// indexedFormattedTextPosition reconstructs the active formatting before the selected block from
// live ContentFormat Items. Content-bearing subtrees with no formats are skipped by aggregate
// count, so the common long formatting run costs O(log N + F) for F format boundaries instead of
// walking N content Items. The final in-block walk remains in findNextPosition, preserving its
// exact boundary semantics.
func indexedFormattedTextPosition(
	parent abstractType,
	target, itemCount Number,
	currentAttributes Object,
) (*itemStruct, Number, Object, bool) {
	index := activeListPositionIndex(parent, itemCount, false)
	if index == nil {
		return nil, 0, currentAttributes, false
	}
	item, start, attributes, ok := index.findFormattedPosition(target, currentAttributes)
	if !ok {
		destroyListPositionIndex(parent)
		return nil, 0, currentAttributes, false
	}
	return item, start, attributes, true
}

// indexedReadPosition reuses an already-active writer index without mutating or rebuilding it.
// Relative-position reads must remain safe for concurrent readers on a quiescent document, so a
// missing or inconsistent accelerator simply falls back to the linked walk.
func indexedReadPosition(parent abstractType, target Number) (*itemStruct, Number, bool) {
	_, index := ownedListPositionIndex(parent)
	if index == nil || index.root == nil || index.items != listItemCount(parent) ||
		index.root.subtreeVisible != parent.GetLength() {
		return nil, 0, false
	}
	return index.findPosition(target)
}

// indexedVisibleStart returns the visible index immediately before item. The block lookup is
// bounded by listPositionBlockMaxItems; the parent walk accumulates complete left subtrees, so the
// result is O(log blocks + block size) instead of a walk from the list head.
func indexedVisibleStart(parent abstractType, item *itemStruct) (Number, bool) {
	_, index := ownedListPositionIndex(parent)
	if index == nil || index.root == nil || index.items != listItemCount(parent) ||
		index.root.subtreeVisible != parent.GetLength() {
		return 0, false
	}
	node := index.blockFor(item)
	if node == nil {
		return 0, false
	}
	visible := nodeVisible(node.left)
	for current := node; current.parent != nil; current = current.parent {
		if current == current.parent.right {
			visible += nodeVisible(current.parent.left) + current.parent.visible
		}
	}
	for current, remaining := node.first, node.items; current != nil && remaining > 0; current, remaining = current.right, remaining-1 {
		if current == item {
			return visible, true
		}
		visible += itemVisibleLength(current)
	}
	return 0, false
}

// findMutationPosition shares the physical Item-count read with the mutable marker path. Below
// the activation threshold this adds only one predictable comparison to findMarker's existing
// accounting; it must not double the interface lookup on every ordinary mutation.
func findMutationPosition(parent abstractType, target Number) (*itemStruct, Number, bool) {
	itemCount := listItemCount(parent)
	if itemCount >= buildListPositionIndexItems {
		if item, start, ok := indexedMutationPosition(parent, target, itemCount); ok {
			return item, start, true
		}
	}
	if marker := findMarkerWithItemCount(parent, target, itemCount); marker != nil {
		return marker.p, marker.index, true
	}
	return nil, 0, false
}

func updateListPositionIndexAfterIntegrate(parent abstractType, item *itemStruct) {
	_, index := ownedListPositionIndex(parent)
	if index == nil {
		return
	}
	if !index.insertItem(item, itemVisibleLength(item)) {
		destroyListPositionIndex(parent)
	}
}

func updateListPositionIndexAfterSplit(parent abstractType, right *itemStruct) {
	_, index := ownedListPositionIndex(parent)
	if index == nil {
		return
	}
	if !index.insertItem(right, 0) {
		destroyListPositionIndex(parent)
	}
}

func updateListPositionIndexAfterDelete(parent abstractType, item *itemStruct, oldVisibleLength Number) {
	_, index := ownedListPositionIndex(parent)
	if index == nil {
		return
	}
	if !index.deleteItem(item, oldVisibleLength) {
		destroyListPositionIndex(parent)
	}
}

// updateListPositionIndexAfterTailGrowth records a compact append that extends an existing Item
// without routing through Item.Integrate. No physical node enters the list, but the owning block's
// visible length and every ancestor aggregate still need the same delta as the parent type.
func updateListPositionIndexAfterTailGrowth(parent abstractType, item *itemStruct, visibleDelta Number) {
	_, index := ownedListPositionIndex(parent)
	if index == nil {
		return
	}
	node := index.blockFor(item)
	if node == nil || visibleDelta <= 0 || item.isDeleted() || !item.countable() {
		destroyListPositionIndex(parent)
		return
	}
	node.visible += visibleDelta
	index.refreshFrom(node)
}

func updateListPositionIndexBeforeMerge(parent abstractType, left, right *itemStruct) {
	_, index := ownedListPositionIndex(parent)
	if index == nil {
		right.setInfo(itemInfoListPositionIndexed, false)
		return
	}
	if !index.removeMergedRight(left, right) {
		right.setInfo(itemInfoListPositionIndexed, false)
		destroyListPositionIndex(parent)
		return
	}
	// Below the activation threshold the reference-compatible marker cache is cheaper, and this
	// invariant lets every ordinary lookup skip the tree probe entirely.
	if listItemCount(parent) < listPositionIndexActivationItems(parent) {
		destroyListPositionIndex(parent)
	}
}

type listPositionIndex struct {
	root    *listPositionNode
	anchors map[*itemStruct]*listPositionNode
	items   Number
	// nodeBlocks owns separately allocated fixed-capacity arrays. Appending a block may relocate
	// this outer slice of headers, but never the arrays it points at — which is the entire reason
	// a node address handed out earlier stays valid. Do NOT collapse this to a single growable
	// []listPositionNode: it would compile, pass most tests, and hand out dangling pointers after
	// the first growth, and the symptom would not look like a memory bug. It would look like the
	// tree quietly returning wrong positions.
	nodeBlocks        [][]listPositionNode
	nodeBlockUsed     int
	nodeBlockCapacity int
}

type listPositionNode struct {
	first *itemStruct

	items              Number
	visible            Number
	subtreeItems       Number
	subtreeVisible     Number
	subtreeLiveFormats Number

	left   *listPositionNode
	right  *listPositionNode
	parent *listPositionNode
	height int8
	// A block holds at most 16 Items. Keeping this beside height leaves the node at 80 bytes on
	// 64-bit Go; a Number here would cross into the 96-byte allocation class for every plain block.
	liveFormats uint8
}

func itemVisibleLength(item *itemStruct) Number {
	if item != nil && !item.isDeleted() && item.countable() {
		return item.length
	}
	return 0
}

func itemFormatCount(item *itemStruct) uint8 {
	if item != nil && !item.isDeleted() {
		if _, ok := item.content.(*contentFormat); ok {
			return 1
		}
	}
	return 0
}

func buildListPositionIndex(parent abstractType) *listPositionIndex {
	blockCapacity := int(listItemCount(parent))/listPositionBlockItems + 1
	index := &listPositionIndex{
		anchors:           make(map[*itemStruct]*listPositionNode, blockCapacity),
		nodeBlockCapacity: blockCapacity,
	}
	nodes := make([]*listPositionNode, 0, blockCapacity)
	for item := parent.startItem(); item != nil; {
		node := index.allocateNode()
		node.first = item
		node.height = 1
		item.setInfo(itemInfoListBlockAnchor, true)
		index.anchors[item] = node
		for item != nil && node.items < listPositionBlockItems {
			item.setInfo(itemInfoListPositionIndexed, true)
			node.items++
			node.visible += itemVisibleLength(item)
			node.liveFormats += itemFormatCount(item)
			item = item.right
		}
		node.subtreeItems = node.items
		node.subtreeVisible = node.visible
		node.subtreeLiveFormats = Number(node.liveFormats)
		index.items += node.items
		nodes = append(nodes, node)
	}
	index.root = buildBalancedListPositionNodes(nodes, 0, len(nodes), nil)
	return index
}

// allocateNode hands out stable addresses from fixed-capacity blocks. A position tree grows
// to roughly one node per eight physical Items; allocating each split node independently made
// those nodes 70% of all allocation objects in large formatted insertion. Blocks never move after
// publication. The capacity is the exact node count at activation, so later blocks add no large
// geometric over-reservation: formatted indexes use about 5 KiB blocks and plain indexes about
// 160 KiB blocks at their respective thresholds.
// Nodes are never individually freed and there is no node-removal path. That is correct rather
// than merely convenient, and the reason is worth keeping: the tree indexes PHYSICAL Items, and
// physical Items are never unlinked — a delete tombstones an Item and leaves it in the list, and GC
// swaps its content without removing it. So the node population tracks a quantity that only grows,
// which is exactly what an append-only arena can serve. Verified from both directions: an
// append-and-merge-heavy workload took nodes 2002 to 2511 at 8.0 Items per node against a target
// block size of 8, and a deletion-heavy one left node and Item counts essentially unchanged, both
// with zero emptied blocks.
//
// If Item unlinking is ever introduced, this becomes a leak and needs reclamation or a free list.
func (index *listPositionIndex) allocateNode() *listPositionNode {
	if len(index.nodeBlocks) == 0 || index.nodeBlockUsed == len(index.nodeBlocks[len(index.nodeBlocks)-1]) {
		capacity := index.nodeBlockCapacity
		if capacity < 1 {
			capacity = 1
		}
		index.nodeBlocks = append(index.nodeBlocks, make([]listPositionNode, capacity))
		index.nodeBlockUsed = 0
	}
	block := index.nodeBlocks[len(index.nodeBlocks)-1]
	node := &block[index.nodeBlockUsed]
	index.nodeBlockUsed++
	return node
}

func buildBalancedListPositionNodes(nodes []*listPositionNode, begin, end int, parent *listPositionNode) *listPositionNode {
	if begin >= end {
		return nil
	}
	middle := int(uint(begin+end) >> 1)
	node := nodes[middle]
	node.parent = parent
	node.left = buildBalancedListPositionNodes(nodes, begin, middle, node)
	node.right = buildBalancedListPositionNodes(nodes, middle+1, end, node)
	node.recalculate()
	return node
}

func (index *listPositionIndex) destroy() {
	if index == nil {
		return
	}
	for item := range index.anchors {
		item.setInfo(itemInfoListBlockAnchor, false)
	}
	index.root = nil
	index.anchors = nil
	index.items = 0
	index.nodeBlocks = nil
	index.nodeBlockUsed = 0
	index.nodeBlockCapacity = 0
}

func nodeHeight(node *listPositionNode) int8 {
	if node == nil {
		return 0
	}
	return node.height
}

func nodeItems(node *listPositionNode) Number {
	if node == nil {
		return 0
	}
	return node.subtreeItems
}

func nodeVisible(node *listPositionNode) Number {
	if node == nil {
		return 0
	}
	return node.subtreeVisible
}

func nodeFormats(node *listPositionNode) Number {
	if node == nil {
		return 0
	}
	return node.subtreeLiveFormats
}

func (node *listPositionNode) recalculate() {
	if node == nil {
		return
	}
	node.subtreeItems = node.items + nodeItems(node.left) + nodeItems(node.right)
	node.subtreeVisible = node.visible + nodeVisible(node.left) + nodeVisible(node.right)
	node.subtreeLiveFormats = Number(node.liveFormats) + nodeFormats(node.left) + nodeFormats(node.right)
	leftHeight, rightHeight := nodeHeight(node.left), nodeHeight(node.right)
	if leftHeight > rightHeight {
		node.height = leftHeight + 1
	} else {
		node.height = rightHeight + 1
	}
}

func (index *listPositionIndex) replaceParentLink(old, replacement *listPositionNode) {
	parent := old.parent
	replacement.parent = parent
	if parent == nil {
		index.root = replacement
	} else if parent.left == old {
		parent.left = replacement
	} else {
		parent.right = replacement
	}
}

func (index *listPositionIndex) rotateLeft(node *listPositionNode) *listPositionNode {
	right := node.right
	middle := right.left
	index.replaceParentLink(node, right)
	right.left = node
	node.parent = right
	node.right = middle
	if middle != nil {
		middle.parent = node
	}
	node.recalculate()
	right.recalculate()
	return right
}

func (index *listPositionIndex) rotateRight(node *listPositionNode) *listPositionNode {
	left := node.left
	middle := left.right
	index.replaceParentLink(node, left)
	left.right = node
	node.parent = left
	node.left = middle
	if middle != nil {
		middle.parent = node
	}
	node.recalculate()
	left.recalculate()
	return left
}

func (index *listPositionIndex) rebalanceFrom(node *listPositionNode) {
	for node != nil {
		node.recalculate()
		balance := int(nodeHeight(node.left) - nodeHeight(node.right))
		if balance > 1 {
			if nodeHeight(node.left.left) < nodeHeight(node.left.right) {
				index.rotateLeft(node.left)
			}
			node = index.rotateRight(node)
		} else if balance < -1 {
			if nodeHeight(node.right.right) < nodeHeight(node.right.left) {
				index.rotateRight(node.right)
			}
			node = index.rotateLeft(node)
		}
		node = node.parent
	}
}

func (index *listPositionIndex) refreshFrom(node *listPositionNode) {
	for node != nil {
		node.recalculate()
		node = node.parent
	}
}

func (index *listPositionIndex) insertNodeAfter(left, inserted *listPositionNode) {
	if index.root == nil {
		index.root = inserted
		inserted.height = 1
		return
	}
	var parent *listPositionNode
	if left.right == nil {
		parent = left
		left.right = inserted
	} else {
		parent = left.right
		for parent.left != nil {
			parent = parent.left
		}
		parent.left = inserted
	}
	inserted.parent = parent
	inserted.height = 1
	inserted.subtreeItems = inserted.items
	inserted.subtreeVisible = inserted.visible
	inserted.subtreeLiveFormats = Number(inserted.liveFormats)
	index.rebalanceFrom(parent)
}

func (index *listPositionIndex) changeFirst(node *listPositionNode, first *itemStruct) {
	if node.first != nil {
		delete(index.anchors, node.first)
		node.first.setInfo(itemInfoListBlockAnchor, false)
	}
	node.first = first
	if first != nil {
		first.setInfo(itemInfoListBlockAnchor, true)
		index.anchors[first] = node
	}
}

func (index *listPositionIndex) blockFor(item *itemStruct) *listPositionNode {
	// A missing block-first anchor must not be laundered into the previous block's perfectly valid
	// anchor. Blocks never exceed listPositionBlockMaxItems, so finding no owned anchor within that
	// bound is an index failure; callers destroy/rebuild instead of updating a plausible wrong node.
	for steps := 0; item != nil && steps < listPositionBlockMaxItems; steps++ {
		if item.info&itemInfoListBlockAnchor != 0 {
			if node := index.anchors[item]; node != nil {
				// Reaching an anchor is not sufficient: if this block's anchor was lost, the
				// previous block's anchor is also reachable by walking left. Its declared item
				// count proves whether it actually owns the starting Item.
				if Number(steps) < node.items {
					return node
				}
				return nil
			}
		}
		item = item.left
	}
	return nil
}

func (index *listPositionIndex) splitBlock(node *listPositionNode) {
	if node == nil || node.items <= listPositionBlockMaxItems {
		return
	}
	rightFirst := node.first
	for i := Number(0); i < listPositionBlockItems && rightFirst != nil; i++ {
		rightFirst = rightFirst.right
	}
	if rightFirst == nil {
		return
	}
	right := index.allocateNode()
	right.first = rightFirst
	right.items = node.items - listPositionBlockItems
	right.height = 1
	for item, remaining := rightFirst, right.items; item != nil && remaining > 0; item, remaining = item.right, remaining-1 {
		right.visible += itemVisibleLength(item)
		right.liveFormats += itemFormatCount(item)
	}
	right.subtreeItems = right.items
	right.subtreeVisible = right.visible
	right.subtreeLiveFormats = Number(right.liveFormats)
	rightFirst.setInfo(itemInfoListBlockAnchor, true)
	index.anchors[rightFirst] = right
	node.items = listPositionBlockItems
	node.visible -= right.visible
	node.liveFormats -= right.liveFormats
	index.insertNodeAfter(node, right)
}

// insertItem records an Item after it has been linked into the list. visibleDelta is normally the
// new Item's visible length; SplitItem passes zero because the right split receives exactly the
// visible length removed from its left half.
func (index *listPositionIndex) insertItem(item *itemStruct, visibleDelta Number) bool {
	if index == nil || item == nil {
		return false
	}
	var node *listPositionNode
	if item.left != nil {
		node = index.blockFor(item.left)
	} else if item.right != nil {
		node = index.blockFor(item.right)
		if node != nil {
			index.changeFirst(node, item)
		}
	}
	if node == nil {
		if index.root != nil {
			return false
		}
		node = index.allocateNode()
		node.first = item
		node.height = 1
		item.setInfo(itemInfoListBlockAnchor, true)
		index.anchors[item] = node
		index.root = node
	}
	node.items++
	node.visible += visibleDelta
	node.liveFormats += itemFormatCount(item)
	index.items++
	item.setInfo(itemInfoListPositionIndexed, true)
	index.refreshFrom(node)
	index.splitBlock(node)
	return true
}

func (index *listPositionIndex) deleteItem(item *itemStruct, visibleLength Number) bool {
	node := index.blockFor(item)
	if node == nil || visibleLength > node.visible {
		return false
	}
	node.visible -= visibleLength
	index.refreshFrom(node)
	return true
}

// removeMergedRight records the physical removal performed by Item.MergeWith. The merge predicate
// guarantees equal deletion/countability state. If the pair straddles blocks, the right Item's
// visible length moves to the left block before the right node is removed; total visible length is
// unchanged.
func (index *listPositionIndex) removeMergedRight(left, right *itemStruct) bool {
	leftNode, rightNode := index.blockFor(left), index.blockFor(right)
	if leftNode == nil || rightNode == nil || rightNode.items == 0 || index.items == 0 {
		return false
	}
	visible := itemVisibleLength(right)
	if leftNode != rightNode {
		leftNode.visible += visible
		if visible > rightNode.visible {
			return false
		}
		rightNode.visible -= visible
	}
	rightNode.items--
	index.items--
	right.setInfo(itemInfoListPositionIndexed, false)
	if rightNode.first == right {
		var next *itemStruct
		if rightNode.items > 0 {
			next = right.right
		}
		index.changeFirst(rightNode, next)
	}
	index.refreshFrom(leftNode)
	if rightNode != leftNode {
		index.refreshFrom(rightNode)
	}
	return true
}

func applyLiveFormatsInBlock(node *listPositionNode, attributes *Object) {
	if node == nil || node.liveFormats == 0 {
		return
	}
	for item, remaining := node.first, node.items; item != nil && remaining > 0; item, remaining = item.right, remaining-1 {
		format, ok := item.content.(*contentFormat)
		if !ok || item.isDeleted() {
			continue
		}
		if attributes.IsNil() {
			*attributes = newObject()
		}
		updateCurrentAttributes(*attributes, format)
	}
}

func applyLiveFormatsInSubtree(node *listPositionNode, attributes *Object) {
	if node == nil || node.subtreeLiveFormats == 0 {
		return
	}
	applyLiveFormatsInSubtree(node.left, attributes)
	applyLiveFormatsInBlock(node, attributes)
	applyLiveFormatsInSubtree(node.right, attributes)
}

// findFormattedPosition is findPosition plus the formatting state immediately before the returned
// block. It applies only format-bearing preceding subtrees; findNextPosition consumes the selected
// block itself so an index exactly on a format boundary behaves identically to a walk from Start.
func (index *listPositionIndex) findFormattedPosition(
	target Number,
	currentAttributes Object,
) (*itemStruct, Number, Object, bool) {
	if index == nil || index.root == nil || target < 0 || target > index.root.subtreeVisible ||
		index.root.subtreeVisible == 0 {
		return nil, 0, currentAttributes, false
	}
	coordinate := target
	if coordinate == index.root.subtreeVisible {
		coordinate--
	}
	node := index.root
	base := Number(0)
	for node != nil {
		leftVisible := nodeVisible(node.left)
		if coordinate < base+leftVisible {
			node = node.left
			continue
		}
		applyLiveFormatsInSubtree(node.left, &currentAttributes)
		nodeBase := base + leftVisible
		if node.visible > 0 && coordinate < nodeBase+node.visible {
			return node.first, nodeBase, currentAttributes, true
		}
		applyLiveFormatsInBlock(node, &currentAttributes)
		base = nodeBase + node.visible
		node = node.right
	}
	return nil, 0, currentAttributes, false
}

// findPosition returns the first Item in the indexed block and its exact visible start. Callers
// finish with a bounded linked walk, preserving the existing insertion/deletion boundary logic.
func (index *listPositionIndex) findPosition(target Number) (*itemStruct, Number, bool) {
	if index == nil || index.root == nil || target < 0 || target > index.root.subtreeVisible {
		return nil, 0, false
	}
	// The visible end belongs to the rightmost block that contributes visible content, not
	// necessarily the physically rightmost block: a list may end in an arbitrarily long tombstone
	// run. Descend by subtree aggregates so Insert(length) retains the index instead of treating that
	// valid boundary as an index failure and rebuilding on the following operation.
	if target == index.root.subtreeVisible {
		node := index.root
		base := Number(0)
		for node != nil {
			leftVisible := nodeVisible(node.left)
			nodeBase := base + leftVisible
			if nodeVisible(node.right) > 0 {
				base = nodeBase + node.visible
				node = node.right
				continue
			}
			if node.visible > 0 {
				return node.first, nodeBase, true
			}
			node = node.left
		}
		return nil, 0, false
	}
	node := index.root
	base := Number(0)
	var fallback *listPositionNode
	var fallbackBase Number
	for node != nil {
		leftVisible := nodeVisible(node.left)
		if target < base+leftVisible {
			node = node.left
			continue
		}
		nodeBase := base + leftVisible
		if node.visible > 0 {
			if target < nodeBase+node.visible {
				return node.first, nodeBase, true
			}
			fallback, fallbackBase = node, nodeBase
		}
		base = nodeBase + node.visible
		if nodeVisible(node.right) == 0 {
			break
		}
		node = node.right
	}
	if fallback != nil {
		return fallback.first, fallbackBase, true
	}
	return nil, 0, false
}
