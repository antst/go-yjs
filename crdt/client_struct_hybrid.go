package crdt

const clientStructTreeHybridEnabled = true

type clientStructListTreeState struct {
	tree *clientStructTree
}

// clientStructCursorState is a two-word tagged union. A nil leaf denotes a
// flat-list ordinal; otherwise position is a leaf slot. The upper half retains
// the list generation so a shift invalidates every cursor without growing the
// pre-S2b 24-byte cursor.
type clientStructCursorState struct {
	leaf   *clientStructTreeLeaf
	packed uint64
}

const clientStructCursorPackedMask = uint64(^uint32(0))

func (s *clientStructListTreeState) active() *clientStructTree  { return s.tree }
func (s *clientStructListTreeState) set(tree *clientStructTree) { s.tree = tree }

func makeClientStructFlatCursorState(position int, generation uint64) clientStructCursorState {
	return makeClientStructCursorState(nil, position, generation)
}

func makeClientStructTreeCursorState(cursor clientStructTreeCursor, generation uint64) clientStructCursorState {
	return makeClientStructCursorState(cursor.leaf, cursor.slot, generation)
}

func makeClientStructCursorState(leaf *clientStructTreeLeaf, position int, generation uint64) clientStructCursorState {
	if position < 0 || uint64(position) > clientStructCursorPackedMask {
		panic("clientStructCursorState: position exceeds packed representation")
	}
	if generation > clientStructCursorPackedMask {
		panic("clientStructCursorState: generation exceeds packed representation")
	}
	return clientStructCursorState{
		leaf: leaf, packed: generation<<32 | uint64(position),
	}
}

func (s clientStructCursorState) flatPosition() int        { return int(uint32(s.packed)) }
func (s clientStructCursorState) cursorGeneration() uint64 { return s.packed >> 32 }

func (s clientStructCursorState) treeCursor(list *clientStructList) (clientStructTreeCursor, bool) {
	return s.treeCursorFor(list.tree.active())
}

func (s clientStructCursorState) treeCursorFor(tree *clientStructTree) (clientStructTreeCursor, bool) {
	if tree == nil || s.leaf == nil {
		return clientStructTreeCursor{}, false
	}
	return clientStructTreeCursor{
		tree: tree, leaf: s.leaf, slot: int(uint32(s.packed)), generation: tree.generation,
	}, true
}

func (c clientStructCursor) Valid() bool {
	if c.list == nil || c.cursorGeneration() != c.list.generation {
		return false
	}
	if c.leaf == nil {
		return c.list.tree.tree == nil && c.flatPosition() < len(c.list.items)
	}
	return c.validTree()
}

func (c clientStructCursor) Value() abstractStruct {
	if c.list != nil && c.leaf == nil &&
		c.cursorGeneration() == c.list.generation && c.list.tree.tree == nil {
		position := c.flatPosition()
		if position < len(c.list.items) {
			return c.list.items[position]
		}
	}
	return c.treeValue()
}

func (c clientStructCursor) Next() (clientStructCursor, bool) {
	if c.list == nil {
		return clientStructCursor{}, false
	}
	if c.leaf != nil {
		return c.nextTree()
	}
	if c.packed>>32 != c.list.generation || c.list.tree.tree != nil {
		panic("clientStructCursor.Next: invalid cursor")
	}
	position := int(uint32(c.packed))
	if position >= len(c.list.items) {
		panic("clientStructCursor.Next: invalid cursor")
	}
	next := position + 1
	if next >= len(c.list.items) {
		return clientStructCursor{}, false
	}
	c.packed = c.packed&^clientStructCursorPackedMask | uint64(uint32(next))
	return c, true
}

func (c clientStructCursor) Prev() (clientStructCursor, bool) {
	if c.list == nil {
		return clientStructCursor{}, false
	}
	generation := c.cursorGeneration()
	if c.leaf != nil {
		return c.prevTree()
	}
	if generation != c.list.generation || c.list.tree.tree != nil {
		panic("clientStructCursor.Prev: invalid cursor")
	}
	position := c.flatPosition()
	if position >= len(c.list.items) {
		panic("clientStructCursor.Prev: invalid cursor")
	}
	if position == 0 {
		return clientStructCursor{}, false
	}
	return clientStructCursor{
		list: c.list,
		clientStructCursorState: clientStructCursorState{
			packed: generation<<32 | uint64(uint32(position-1)),
		},
	}, true
}

func (c clientStructCursor) validTree() bool {
	if c.list == nil || c.cursorGeneration() != c.list.generation {
		return false
	}
	treeCursor, treeMode := c.treeCursorFor(c.list.tree.active())
	return treeMode && treeCursor.Valid()
}

func (c clientStructCursor) treeValue() abstractStruct {
	if c.list == nil || c.cursorGeneration() != c.list.generation {
		panic("clientStructCursor.Value: invalid cursor")
	}
	treeCursor, treeMode := c.treeCursorFor(c.list.tree.active())
	if !treeMode {
		panic("clientStructCursor.Value: invalid cursor")
	}
	return treeCursor.Value()
}

func (c clientStructCursor) nextTree() (clientStructCursor, bool) {
	if c.cursorGeneration() != c.list.generation {
		panic("clientStructCursor.Next: invalid cursor")
	}
	treeCursor, treeMode := c.treeCursorFor(c.list.tree.active())
	if !treeMode {
		panic("clientStructCursor.Next: invalid cursor")
	}
	next, ok := treeCursor.Next()
	if !ok {
		return clientStructCursor{}, false
	}
	return c.list.cursorAtTree(next), true
}

func (c clientStructCursor) prevTree() (clientStructCursor, bool) {
	if c.cursorGeneration() != c.list.generation {
		panic("clientStructCursor.Prev: invalid cursor")
	}
	treeCursor, treeMode := c.treeCursorFor(c.list.tree.active())
	if !treeMode {
		panic("clientStructCursor.Prev: invalid cursor")
	}
	previous, ok := treeCursor.Prev()
	if !ok {
		return clientStructCursor{}, false
	}
	return c.list.cursorAtTree(previous), true
}
