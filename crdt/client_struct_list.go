package crdt

import "strings"

// clientStructList owns one client's clock-ordered structs. Its flat slice is
// deliberately private: StructStore callers work through cursors so a later
// chunked representation can replace the slice without another package-wide
// migration.
type clientStructList struct {
	tree   clientStructListTreeState
	items  []abstractStruct
	oracle clientStructListOracle
	// generation advances only when ordinal positions shift. Cursors remain
	// valid across append, reserve, and replace operations.
	generation uint64
}

// clientStructCursor is an opaque position in a clientStructList. The ordinal
// is representation state, not part of the API. Insertions and removals advance
// the list generation, so using any cursor from the prior layout panics instead
// of silently addressing the struct that moved into its ordinal.
type clientStructCursor struct {
	list *clientStructList
	clientStructCursorState
}

func newClientStructList(capacity int) *clientStructList {
	if capacity < 0 {
		capacity = 0
	}
	return &clientStructList{items: make([]abstractStruct, 0, capacity)}
}

func (l *clientStructList) Len() int {
	if l == nil {
		return 0
	}
	if tree := l.tree.active(); tree != nil {
		return tree.Len()
	}
	return len(l.items)
}

func (l *clientStructList) Reserve(capacity int) {
	if l.tree.active() != nil {
		return
	}
	if capacity <= cap(l.items) {
		return
	}
	reserved := make([]abstractStruct, len(l.items), capacity)
	copy(reserved, l.items)
	l.items = reserved
}

func (l *clientStructList) Append(value abstractStruct) clientStructCursor {
	if tree := l.tree.active(); tree != nil {
		cursor := tree.Append(value)
		l.oracle.insert(value)
		l.oracle.checkList(l)
		return l.cursorAtTree(cursor)
	}
	l.items = append(l.items, value)
	l.oracle.insert(value)
	l.oracle.checkList(l)
	return l.cursorAt(len(l.items) - 1)
}

// appendValue is the production append path when the caller does not need a
// cursor. Keep the flat arm small and direct: compact local mutations call this
// once per operation, while the cursor-returning Append above is primarily the
// representation API used by navigation and tests.
func (l *clientStructList) appendValue(value abstractStruct) {
	if l.tree.tree != nil {
		l.appendTreeValue(value)
		return
	}
	l.items = append(l.items, value)
	if clientStructListOracleEnabled {
		l.oracle.insert(value)
		l.oracle.checkList(l)
	}
}

//go:noinline
func (l *clientStructList) appendTreeValue(value abstractStruct) {
	l.tree.tree.Append(value)
	l.oracle.insert(value)
	l.oracle.checkList(l)
}

// Snapshot appends a caller-owned flat view to dst. No returned slice aliases
// the list's backing storage.
func (l *clientStructList) Snapshot(dst []abstractStruct) []abstractStruct {
	dst = dst[:0]
	if l == nil {
		return dst
	}
	if tree := l.tree.active(); tree != nil {
		return tree.Snapshot(dst)
	}
	return append(dst, l.items...)
}

func (l *clientStructList) First() (clientStructCursor, bool) {
	if l == nil || l.Len() == 0 {
		return clientStructCursor{}, false
	}
	if tree := l.tree.active(); tree != nil {
		cursor, _ := tree.First()
		return l.cursorAtTree(cursor), true
	}
	return l.cursorAt(0), true
}

func (l *clientStructList) Last() (clientStructCursor, bool) {
	if l == nil || l.Len() == 0 {
		return clientStructCursor{}, false
	}
	if tree := l.tree.active(); tree != nil {
		cursor, _ := tree.Last()
		return l.cursorAtTree(cursor), true
	}
	return l.cursorAt(len(l.items) - 1), true
}

// lastValue reads the tail without manufacturing an opaque cursor. State
// checks are on every local mutation, so the direct flat arm is materially
// cheaper than Last followed by Value while preserving the cursor boundary for
// callers that actually navigate. StructStore never retains an empty client
// list, so internal callers have the same non-empty precondition as a successful
// Last call.
func (l *clientStructList) lastValue() abstractStruct {
	if l.tree.tree != nil {
		return l.lastTreeValue()
	}
	return l.items[len(l.items)-1]
}

func (l *clientStructList) lastTreeValue() abstractStruct {
	cursor, _ := l.tree.tree.Last()
	return cursor.Value()
}

func (l *clientStructList) Find(clock Number) (clientStructCursor, error) {
	if tree := l.tree.active(); tree != nil {
		cursor, err := tree.Find(clock)
		if err != nil {
			l.oracle.checkFind(clock, nil, false)
			return clientStructCursor{}, err
		}
		result := l.cursorAtTree(cursor)
		if clientStructListOracleEnabled {
			l.oracle.checkFind(clock, cursor.Value(), true)
		}
		return result, nil
	}
	index, err := findIndexSS(l.items, clock)
	if err != nil {
		l.oracle.checkFind(clock, nil, false)
		return clientStructCursor{}, err
	}
	cursor := l.cursorAt(int(index))
	if clientStructListOracleEnabled {
		l.oracle.checkFind(clock, cursor.Value(), true)
	}
	return cursor, nil
}

func (l *clientStructList) InsertAfter(cursor clientStructCursor, value abstractStruct) clientStructCursor {
	l.requireCursor(cursor)
	if tree := l.tree.active(); tree != nil {
		treeCursor, _ := cursor.treeCursor(l)
		inserted := tree.InsertAfter(treeCursor, value)
		l.generation++
		l.oracle.insert(value)
		l.oracle.checkList(l)
		return l.cursorAtTree(inserted)
	}
	// InsertAfter is the hybrid activation boundary because it is the only primitive that both
	// grows the list and shifts an unbounded suffix. Append grows only at the end; Reserve preserves
	// ordinals; Replace is in-place. Remove can shift a suffix, but every production removal follows
	// a successful adjacent merge or run coalescing and therefore shrinks the representation. A
	// partial delete first creates its clean clock boundary through InsertAfter, so the growing
	// fragmented workloads that make repeated removal costly have already crossed this point.
	if clientStructTreeHybridEnabled && len(l.items) >= clientStructTreeActivationLimit {
		anchor := cursor.Value()
		tree := newClientStructTreeFromFlat(
			l.items, clientStructTreeHybridLeafLimit, clientStructTreeHybridBranchLimit,
		)
		treeCursor, found := tree.findValue(anchor)
		if !found {
			panic("clientStructList.InsertAfter: activation lost anchor")
		}
		l.items = nil
		l.tree.set(tree)
		noteClientStructTreeActivation()
		l.oracle.checkList(l)
		inserted := tree.InsertAfter(treeCursor, value)
		l.generation++
		l.oracle.insert(value)
		l.oracle.checkList(l)
		return l.cursorAtTree(inserted)
	}
	inserted := cursor.flatPosition() + 1
	spliceStruct(&l.items, Number(inserted), 0, []abstractStruct{value})
	l.generation++
	l.oracle.insert(value)
	l.oracle.checkList(l)
	return l.cursorAt(inserted)
}

// Remove deletes the inclusive cursor range and returns the struct that
// followed it, if any. Both cursors must belong to this list.
func (l *clientStructList) Remove(first, last clientStructCursor) (clientStructCursor, bool) {
	l.requireCursor(first)
	l.requireCursor(last)
	tree := l.tree.active()
	if tree != nil {
		firstTree, _ := first.treeCursor(l)
		lastTree, _ := last.treeCursor(l)
		if tree.Index(firstTree) > tree.Index(lastTree) {
			panic("clientStructList.Remove: reversed cursor range")
		}
	} else if first.flatPosition() > last.flatPosition() {
		panic("clientStructList.Remove: reversed cursor range")
	}
	var firstValue, lastValue abstractStruct
	var firstTree, lastTree clientStructTreeCursor
	if tree != nil {
		firstTree, _ = first.treeCursor(l)
		lastTree, _ = last.treeCursor(l)
		firstValue, lastValue = firstTree.Value(), lastTree.Value()
	} else {
		firstValue, lastValue = first.Value(), last.Value()
	}
	l.oracle.removeRange(firstValue, lastValue)
	if tree != nil {
		next, ok := tree.Remove(firstTree, lastTree)
		l.generation++
		if tree.Len() <= clientStructTreeDeactivationLimit {
			var nextValue abstractStruct
			if ok {
				nextValue = next.Value()
			}
			l.items = tree.Snapshot(l.items[:0])
			l.tree.set(nil)
			noteClientStructTreeDeactivation()
			l.oracle.checkList(l)
			if !ok {
				return clientStructCursor{}, false
			}
			for position, value := range l.items {
				if value == nextValue {
					return l.cursorAt(position), true
				}
			}
			panic("clientStructList.Remove: deactivation lost successor")
		}
		l.oracle.checkList(l)
		if !ok {
			return clientStructCursor{}, false
		}
		return l.cursorAtTree(next), true
	}
	start := first.flatPosition()
	lastPosition := last.flatPosition()
	spliceStruct(&l.items, Number(start), Number(lastPosition-start+1), nil)
	l.generation++
	l.oracle.checkList(l)
	if start >= len(l.items) {
		return clientStructCursor{}, false
	}
	return l.cursorAt(start), true
}

func (l *clientStructList) Replace(cursor clientStructCursor, value abstractStruct) {
	l.requireCursor(cursor)
	if tree := l.tree.active(); tree != nil {
		treeCursor, _ := cursor.treeCursor(l)
		old := treeCursor.Value()
		tree.Replace(treeCursor, value)
		l.oracle.replace(old, value)
	} else {
		position := cursor.flatPosition()
		old := l.items[position]
		l.items[position] = value
		l.oracle.replace(old, value)
	}
	l.oracle.checkList(l)
}

func (l *clientStructList) refreshValue(value abstractStruct) {
	if l.tree.tree == nil {
		return
	}
	l.refreshTreeValue(value)
}

func (l *clientStructList) refreshTreeValue(value abstractStruct) {
	tree := l.tree.tree
	cursor, found := tree.findValue(value)
	if !found {
		panic("clientStructList.refreshValue: value is not in the tree")
	}
	tree.refreshUp(&cursor.leaf.node)
	l.oracle.checkList(l)
}

// applyDeleteRange keeps the original contiguous-slice loop for a flat list and
// pays cursor dispatch only after the client has actually activated its tree.
// ReadAndApplyDeleteSet calls this once per decoded range; putting the branch at
// the range boundary keeps append-built maps and small text histories on the
// same compiler-visible loop they used before hybrid storage existed.
func (l *clientStructList) applyDeleteRange(trans *Transaction, clock, clockEnd Number) error {
	if l.tree.active() == nil {
		return l.applyDeleteRangeFlat(trans, clock, clockEnd)
	}
	cursor, err := l.Find(clock)
	if err != nil {
		return err
	}
	return l.applyDeleteRangeTree(trans, cursor, clock, clockEnd)
}

func (l *clientStructList) applyDeleteRangeFlat(trans *Transaction, clock, clockEnd Number) error {
	position, err := findIndexSS(l.items, clock)
	if err != nil {
		return err
	}
	index := int(position)
	first := l.items[index]
	if first.isDeleted() && clockEnd <= first.getID().Clock+first.structLength() {
		return nil
	}
	if !first.isDeleted() && first.getID().Clock < clock {
		cursor := l.InsertAfter(
			l.cursorAt(index), splitItem(trans, first.(*itemStruct), clock-first.getID().Clock),
		)
		if l.tree.active() != nil {
			return l.applyDeleteRangeTree(trans, cursor, clock, clockEnd)
		}
		index = cursor.flatPosition()
	}

	for index < len(l.items) {
		current := l.items[index]
		if current.getID().Clock >= clockEnd {
			break
		}
		if !current.isDeleted() {
			if item, ok := current.(*itemStruct); ok {
				if clockEnd < item.getID().Clock+item.structLength() {
					l.InsertAfter(
						l.cursorAt(index),
						splitItem(trans, item, clockEnd-current.getID().Clock),
					)
					item.deleteItemStruct(trans)
					break
				}
				item.deleteItemStruct(trans)
			}
		}
		index++
	}
	return nil
}

func (l *clientStructList) applyDeleteRangeTree(
	trans *Transaction, cursor clientStructCursor, clock, clockEnd Number,
) error {
	first := cursor.Value()
	if first.isDeleted() && clockEnd <= first.getID().Clock+first.structLength() {
		return nil
	}
	if !first.isDeleted() && first.getID().Clock < clock {
		cursor = l.InsertAfter(
			cursor, splitItem(trans, first.(*itemStruct), clock-first.getID().Clock),
		)
	}

	for cursor.Valid() {
		current := cursor.Value()
		if current.getID().Clock >= clockEnd {
			break
		}
		if !current.isDeleted() {
			if item, ok := current.(*itemStruct); ok {
				if clockEnd < item.getID().Clock+item.structLength() {
					l.InsertAfter(
						cursor, splitItem(trans, item, clockEnd-current.getID().Clock),
					)
					item.deleteItemStruct(trans)
					break
				}
				item.deleteItemStruct(trans)
			}
		}
		next, more := cursor.Next()
		if !more {
			break
		}
		cursor = next
	}
	return nil
}

func (l *clientStructList) appendDeletedRanges(dst []*deleteItem) []*deleteItem {
	if tree := l.tree.active(); tree != nil {
		return tree.appendDeletedRanges(dst)
	}
	for i := 0; i < len(l.items); i++ {
		value := l.items[i]
		if !value.isDeleted() {
			continue
		}
		clock := value.getID().Clock
		length := value.structLength()
		for i+1 < len(l.items) && l.items[i+1].isDeleted() {
			i++
			length += l.items[i].structLength()
		}
		dst = append(dst, newDeleteItem(clock, length))
	}
	return dst
}

func (l *clientStructList) deletedRangeCount() int {
	if tree := l.tree.active(); tree != nil {
		return tree.deletedRangeCount()
	}
	return deletedStructRangeCount(l.items)
}

func (l *clientStructList) writeDeletedRanges(encoder dsEncoder, client Number, rangeCount int) error {
	if tree := l.tree.active(); tree != nil {
		return tree.writeDeletedRanges(encoder, client, rangeCount)
	}
	return writeDeletedStructRanges(encoder, client, l.items, rangeCount)
}

func (l *clientStructList) writeStructs(encoder updateEncoder, client, clock Number) error {
	if tree := l.tree.active(); tree != nil {
		first, _ := tree.First()
		clock = maxNumber(clock, first.Value().getID().Clock)
		start, err := tree.Find(clock)
		if err != nil {
			return err
		}
		writeEncoderRestVarUint(encoder, uint64(tree.Len()-tree.Index(start)))
		encoder.writeClient(client)
		writeEncoderRestVarUint(encoder, uint64(clock))
		return writeClientStructTreeChunks(encoder, tree.ChunkFrom(start), clock-start.Value().getID().Clock)
	}
	clock = maxNumber(clock, l.items[0].getID().Clock)
	startNewStructs, _ := findIndexSS(l.items, clock)
	writeEncoderRestVarUint(encoder, uint64(len(l.items)-startNewStructs))
	encoder.writeClient(client)
	writeEncoderRestVarUint(encoder, uint64(clock))

	firstStruct := l.items[startNewStructs]
	if fast, ok := encoder.(*fastUpdateEncoderV1); ok {
		return fast.writeStructs(l.items, startNewStructs, clock-firstStruct.getID().Clock)
	}
	if fast, ok := encoder.(*updateEncoderV2); ok {
		return fast.writeStructs(l.items, startNewStructs, clock-firstStruct.getID().Clock)
	}
	if err := firstStruct.writeStruct(encoder, clock-firstStruct.getID().Clock); err != nil {
		return err
	}
	for i := startNewStructs + 1; i < len(l.items); i++ {
		if err := l.items[i].writeStruct(encoder, 0); err != nil {
			return err
		}
	}
	return nil
}

func (l *clientStructList) writeFullStateV1(encoder *fastUpdateEncoderV1, client Number) error {
	if tree := l.tree.active(); tree != nil {
		first, _ := tree.First()
		clock := first.Value().getID().Clock
		encoder.writeRestVarUint(uint64(tree.Len()))
		encoder.writeRestVarUint(uint64(client))
		encoder.writeRestVarUint(uint64(clock))
		return writeClientStructTreeChunks(encoder, tree.ChunkFrom(first), 0)
	}
	first := l.items[0]
	clock := first.getID().Clock
	encoder.writeRestVarUint(uint64(len(l.items)))
	encoder.writeRestVarUint(uint64(client))
	encoder.writeRestVarUint(uint64(clock))
	return encoder.writeStructs(l.items, 0, 0)
}

func (l *clientStructList) writeFullStateV2(encoder *updateEncoderV2, client Number) error {
	if tree := l.tree.active(); tree != nil {
		first, _ := tree.First()
		writeVarUint(encoder.rest, uint64(tree.Len()))
		encoder.writeTrustedClient(client)
		writeVarUint(encoder.rest, uint64(first.Value().getID().Clock))
		for chunk, ok := tree.ChunkFrom(first), true; ok; chunk, ok = chunk.Next() {
			if err := encoder.writeFullStateStructs(chunk.Values()); err != nil {
				return err
			}
		}
		return nil
	}
	first := l.items[0]
	writeVarUint(encoder.rest, uint64(len(l.items)))
	encoder.writeTrustedClient(client)
	writeVarUint(encoder.rest, uint64(first.getID().Clock))
	return encoder.writeFullStateStructs(l.items)
}

func (l *clientStructList) writeSnapshotPrefix(encoder updateEncoder, client, clock Number) error {
	if tree := l.tree.active(); tree != nil {
		last, err := tree.Find(clock - 1)
		if err != nil {
			return err
		}
		remaining := tree.Index(last) + 1
		writeEncoderRestVarUint(encoder, uint64(remaining))
		encoder.writeClient(client)
		writeEncoderRestVarUint(encoder, 0)
		first, _ := tree.First()
		for chunk, ok := tree.ChunkFrom(first), true; ok && remaining > 0; chunk, ok = chunk.Next() {
			values := chunk.Values()
			if len(values) > remaining {
				values = values[:remaining]
			}
			for _, value := range values {
				if err := value.writeStruct(encoder, 0); err != nil {
					return err
				}
			}
			remaining -= len(values)
		}
		return nil
	}
	lastStructIndex, err := findIndexSS(l.items, clock-1)
	if err != nil {
		return err
	}
	writeEncoderRestVarUint(encoder, uint64(lastStructIndex+1))
	encoder.writeClient(client)
	writeEncoderRestVarUint(encoder, 0)
	for i := Number(0); i <= lastStructIndex; i++ {
		if err := l.items[i].writeStruct(encoder, 0); err != nil {
			return err
		}
	}
	return nil
}

func (l *clientStructList) clocksFitV2FastPath() bool {
	if tree := l.tree.active(); tree != nil {
		if tree.Len() == 0 {
			return true
		}
		lastCursor, _ := tree.Last()
		last := lastCursor.Value()
		clock := int64(last.getID().Clock)
		length := int64(last.structLength())
		return clock >= 0 && length >= 0 && clock <= maxIntDiffOptRleDiff && length <= maxIntDiffOptRleDiff-clock
	}
	if len(l.items) == 0 {
		return true
	}
	last := l.items[len(l.items)-1]
	clock := int64(last.getID().Clock)
	length := int64(last.structLength())
	return clock >= 0 && length >= 0 && clock <= maxIntDiffOptRleDiff && length <= maxIntDiffOptRleDiff-clock
}

func writeClientStructTreeChunks(encoder updateEncoder, chunk clientStructTreeChunkCursor, firstOffset Number) error {
	for ok := true; ok; chunk, ok = chunk.Next() {
		values := chunk.Values()
		if fast, fastOK := encoder.(*fastUpdateEncoderV1); fastOK {
			if err := fast.writeStructs(values, 0, firstOffset); err != nil {
				return err
			}
		} else if fast, fastOK := encoder.(*updateEncoderV2); fastOK {
			if err := fast.writeStructs(values, 0, firstOffset); err != nil {
				return err
			}
		} else {
			start := 0
			if firstOffset != 0 {
				if err := values[0].writeStruct(encoder, firstOffset); err != nil {
					return err
				}
				start = 1
			}
			for _, value := range values[start:] {
				if err := value.writeStruct(encoder, 0); err != nil {
					return err
				}
			}
		}
		firstOffset = 0
	}
	return nil
}

func (l *clientStructList) tryMergeWithLeft(position int) {
	if l.tree.active() == nil {
		left := l.items[position-1]
		right := l.items[position]
		if left.isDeleted() != right.isDeleted() || !isSameType(left, right) || !left.mergeStructWith(right) {
			return
		}
		l.removePositions(position, 1)
		if r, ok := right.(*itemStruct); ok && r.parentSub != "" && r.parent.(abstractType).getMap()[r.parentSub] == right {
			r.parent.(abstractType).getMap()[r.parentSub] = left.(*itemStruct)
		}
		return
	}
	rightCursor, ok := l.cursorAtPosition(position)
	if !ok {
		return
	}
	_, _ = l.tryMergeCursor(rightCursor)
}

func (l *clientStructList) tryMergeCursor(rightCursor clientStructCursor) (clientStructCursor, bool) {
	leftCursor, ok := rightCursor.Prev()
	if !ok {
		return clientStructCursor{}, false
	}
	left := leftCursor.Value()
	right := rightCursor.Value()
	if left.isDeleted() != right.isDeleted() || !isSameType(left, right) || !left.mergeStructWith(right) {
		return leftCursor, true
	}
	l.Remove(rightCursor, rightCursor)
	if r, ok := right.(*itemStruct); ok && r.parentSub != "" && r.parent.(abstractType).getMap()[r.parentSub] == right {
		r.parent.(abstractType).getMap()[r.parentSub] = left.(*itemStruct)
	}
	leftCursor, err := l.Find(left.getID().Clock)
	if err != nil {
		panic("clientStructList.tryMergeCursor: merged left disappeared")
	}
	return leftCursor, true
}

func (l *clientStructList) tryGcDeleteItems(deleteItems []*deleteItem, store *structStore, gcFilter func(item *itemStruct) bool) {
	if l.tree.active() == nil {
		for di := len(deleteItems) - 1; di >= 0; di-- {
			deleteItem := deleteItems[di]
			endDeleteItemClock := deleteItem.clock + deleteItem.length
			si, _ := findIndexSS(l.items, deleteItem.clock)
			s := l.items[si]
			for si < len(l.items) && s.getID().Clock < endDeleteItemClock {
				s = l.items[si]
				if endDeleteItemClock <= s.getID().Clock {
					break
				}
				if item, ok := s.(*itemStruct); ok && item.isDeleted() && !item.keep() && gcFilter(item) {
					item.gcItem(store, false)
				}
				si++
			}
		}
		return
	}
	for di := len(deleteItems) - 1; di >= 0; di-- {
		deleteItem := deleteItems[di]
		endDeleteItemClock := deleteItem.clock + deleteItem.length
		cursor, err := l.Find(deleteItem.clock)
		if err != nil {
			continue
		}
		for {
			s := cursor.Value()
			if endDeleteItemClock <= s.getID().Clock {
				break
			}
			if item, ok := s.(*itemStruct); ok && item.isDeleted() && !item.keep() && gcFilter(item) {
				item.gcItem(store, false)
			}
			next, ok := cursor.Next()
			if !ok {
				break
			}
			cursor = next
		}
	}
}

func (l *clientStructList) tryMergeDeleteItems(deleteItems []*deleteItem) {
	if l.tree.active() == nil {
		for di := len(deleteItems) - 1; di >= 0; di-- {
			deleteItem := deleteItems[di]
			n, _ := findIndexSS(l.items, deleteItem.clock+deleteItem.length-1)
			position := minNumber(len(l.items)-1, 1+n)
			s := l.items[position]
			for position > 0 && s.getID().Clock >= deleteItem.clock {
				l.tryMergeWithLeft(position)
				position--
				s = l.items[position]
			}
		}
		return
	}
	for di := len(deleteItems) - 1; di >= 0; di-- {
		deleteItem := deleteItems[di]
		cursor, err := l.Find(deleteItem.clock + deleteItem.length - 1)
		if err != nil {
			continue
		}
		if next, ok := cursor.Next(); ok {
			cursor = next
		}
		for cursor.Value().getID().Clock >= deleteItem.clock {
			left, ok := l.tryMergeCursor(cursor)
			if !ok {
				break
			}
			cursor = left
		}
	}
}

func (l *clientStructList) mergeTransactionRange(beforeClock, clock Number) {
	if beforeClock == clock {
		return
	}
	if l.tree.active() == nil {
		index, _ := findIndexSS(l.items, beforeClock)
		firstChangePos := maxNumber(index, 1)
		if clock-beforeClock >= 64 {
			if tail, ok := l.items[len(l.items)-1].(*itemStruct); ok {
				if _, stringTail := tail.content.(*contentString); stringTail {
					l.coalesceContentStringRuns(firstChangePos - 1)
				}
			}
		}
		l.coalesceContentAnyRuns(firstChangePos - 1)
		for i := len(l.items) - 1; i >= firstChangePos; i-- {
			l.tryMergeWithLeft(i)
		}
		return
	}
	index, _ := l.findIndex(beforeClock)
	firstChangePos := maxNumber(index, 1)
	if clock-beforeClock >= 64 {
		last, _ := l.Last()
		if tail, ok := last.Value().(*itemStruct); ok {
			if _, stringTail := tail.content.(*contentString); stringTail {
				l.coalesceContentStringRuns(firstChangePos - 1)
			}
		}
	}
	l.coalesceContentAnyRuns(firstChangePos - 1)
	cursor, _ := l.Last()
	for position := l.Len() - 1; position >= firstChangePos; position-- {
		left, ok := l.tryMergeCursor(cursor)
		if !ok {
			break
		}
		cursor = left
	}
}

func (l *clientStructList) mergeAround(clock Number) {
	if l.tree.active() == nil {
		position, err := findIndexSS(l.items, clock)
		if err != nil {
			return
		}
		if position+1 < len(l.items) {
			l.tryMergeWithLeft(int(position + 1))
		}
		if position > 0 {
			l.tryMergeWithLeft(int(position))
		}
		return
	}
	position, err := l.findIndex(clock)
	if err != nil {
		return
	}
	if position+1 < l.Len() {
		l.tryMergeWithLeft(int(position + 1))
	}
	if position > 0 {
		l.tryMergeWithLeft(int(position))
	}
}

func (l *clientStructList) markPrimitiveMapDeleted(trans *Transaction, parent *YMap, client Number) {
	start := Number(-1)
	length := Number(0)
	flush := func() {
		if length > 0 {
			trans.addToDeleteSet(client, start, length)
			start = -1
			length = 0
		}
	}
	mark := func(values []abstractStruct) {
		for _, abstract := range values {
			item, ok := abstract.(*itemStruct)
			if !ok || item.isDeleted() || item.parent != parent || item.parentSub == "" || parent.typeMap[item.parentSub] != item {
				flush()
				continue
			}
			item.markDeleted()
			if start >= 0 && start+length == item.id.Clock {
				length += item.length
			} else {
				flush()
				start = item.id.Clock
				length = item.length
			}
		}
	}
	if l.tree.active() == nil {
		mark(l.items)
	} else {
		l.forEachChunk(func(values []abstractStruct) bool {
			mark(values)
			return true
		})
	}
	flush()
}

func (l *clientStructList) coalesceContentAnyRuns(start Number) {
	if l.tree.active() != nil {
		l.coalesceContentAnyRunsTree(start)
		return
	}
	if start < 0 {
		start = 0
	}
	for start+1 < len(l.items) {
		left, leftOK := l.items[start].(*itemStruct)
		right, rightOK := l.items[start+1].(*itemStruct)
		if !leftOK || !rightOK || !canMergeContentAnyPair(left, right) {
			start++
			continue
		}

		runEnd := start + 2
		totalLength := len(left.content.(*contentAny).arr) + len(right.content.(*contentAny).arr)
		previous := right
		for runEnd < len(l.items) {
			next, ok := l.items[runEnd].(*itemStruct)
			if !ok || !canMergeContentAnyPair(previous, next) {
				break
			}
			totalLength += len(next.content.(*contentAny).arr)
			previous = next
			runEnd++
		}
		if runEnd == start+2 {
			start++
			continue
		}

		content := left.content.(*contentAny)
		if cap(content.arr) < totalLength {
			combined := make(ArrayAny, len(content.arr), totalLength)
			copy(combined, content.arr)
			content.arr = combined
		}
		mergedEnd := start + 1
		if parent, ok := left.parent.(abstractType); ok {
			invalidateListReadIndex(parent)
			destroyListPositionIndex(parent)
		}
		for i := start + 1; i < runEnd; i++ {
			if !left.mergeWithWithoutReadIndexInvalidation(l.items[i]) {
				break
			}
			mergedEnd = i + 1
		}
		if mergedEnd > start+1 {
			l.removePositions(int(start+1), int(mergedEnd-start-1))
		}
		start++
	}
}

func (l *clientStructList) coalesceContentStringRuns(start Number) {
	if l.tree.active() != nil {
		l.coalesceContentStringRunsTree(start)
		return
	}
	if start < 0 {
		start = 0
	}
	for start+1 < len(l.items) {
		left, leftOK := l.items[start].(*itemStruct)
		right, rightOK := l.items[start+1].(*itemStruct)
		if !leftOK || !rightOK || !l.coalesceContentStringRun(start, left, right) {
			start++
			continue
		}
		start++
	}
}

func (l *clientStructList) coalesceContentStringRun(start Number, left, right *itemStruct) bool {
	if !canMergeContentStringPair(left, right) {
		return false
	}
	runEnd := start + 2
	totalBytes := len(left.content.(*contentString).value) + len(right.content.(*contentString).value)
	previous := right
	for runEnd < len(l.items) {
		next, ok := l.items[runEnd].(*itemStruct)
		if !ok || !canMergeContentStringPair(previous, next) {
			break
		}
		totalBytes += len(next.content.(*contentString).value)
		previous = next
		runEnd++
	}
	if runEnd == start+2 {
		return false
	}

	leftContent := left.content.(*contentString)
	if parent, ok := left.parent.(abstractType); ok {
		destroyListPositionIndex(parent)
	}
	var combined strings.Builder
	combined.Grow(totalBytes)
	combined.WriteString(leftContent.value)
	mergedEnd := start + 1
	for i := start + 1; i < runEnd; i++ {
		next := l.items[i].(*itemStruct)
		if !left.canMergeWith(next) {
			break
		}
		nextContent := next.content.(*contentString)
		combined.WriteString(nextContent.value)
		left.completeMergeWith(next)
		mergedEnd = i + 1
	}
	leftContent.setMergedString(combined.String())
	if mergedEnd > start+1 {
		l.removePositions(int(start+1), int(mergedEnd-start-1))
	}
	return true
}

func (l *clientStructList) coalesceContentAnyRunsTree(start Number) {
	if start < 0 {
		start = 0
	}
	leftCursor, ok := l.cursorAtPosition(int(start))
	if !ok {
		return
	}
	for {
		rightCursor, more := leftCursor.Next()
		if !more {
			return
		}
		left, leftOK := leftCursor.Value().(*itemStruct)
		right, rightOK := rightCursor.Value().(*itemStruct)
		if !leftOK || !rightOK || !canMergeContentAnyPair(left, right) {
			leftCursor = rightCursor
			continue
		}

		runCount := 2
		totalLength := len(left.content.(*contentAny).arr) + len(right.content.(*contentAny).arr)
		previous := right
		scan := rightCursor
		for {
			next, hasNext := scan.Next()
			if !hasNext {
				break
			}
			item, itemOK := next.Value().(*itemStruct)
			if !itemOK || !canMergeContentAnyPair(previous, item) {
				break
			}
			totalLength += len(item.content.(*contentAny).arr)
			previous = item
			scan = next
			runCount++
		}
		if runCount == 2 {
			leftCursor = rightCursor
			continue
		}

		content := left.content.(*contentAny)
		if cap(content.arr) < totalLength {
			combined := make(ArrayAny, len(content.arr), totalLength)
			copy(combined, content.arr)
			content.arr = combined
		}
		if parent, parentOK := left.parent.(abstractType); parentOK {
			invalidateListReadIndex(parent)
			destroyListPositionIndex(parent)
		}

		mergeCursor := rightCursor
		lastMerged := clientStructCursor{}
		merged := 0
		for i := 1; i < runCount; i++ {
			if !left.mergeWithWithoutReadIndexInvalidation(mergeCursor.Value()) {
				break
			}
			lastMerged = mergeCursor
			merged++
			if i+1 < runCount {
				mergeCursor, _ = mergeCursor.Next()
			}
		}
		if merged == 0 {
			leftCursor = rightCursor
			continue
		}
		next, hasNext := l.Remove(rightCursor, lastMerged)
		if !hasNext {
			return
		}
		leftCursor = next
	}
}

func (l *clientStructList) coalesceContentStringRunsTree(start Number) {
	if start < 0 {
		start = 0
	}
	leftCursor, ok := l.cursorAtPosition(int(start))
	if !ok {
		return
	}
	for {
		rightCursor, more := leftCursor.Next()
		if !more {
			return
		}
		left, leftOK := leftCursor.Value().(*itemStruct)
		right, rightOK := rightCursor.Value().(*itemStruct)
		if !leftOK || !rightOK || !canMergeContentStringPair(left, right) {
			leftCursor = rightCursor
			continue
		}

		runCount := 2
		totalBytes := len(left.content.(*contentString).value) + len(right.content.(*contentString).value)
		previous := right
		scan := rightCursor
		for {
			next, hasNext := scan.Next()
			if !hasNext {
				break
			}
			item, itemOK := next.Value().(*itemStruct)
			if !itemOK || !canMergeContentStringPair(previous, item) {
				break
			}
			totalBytes += len(item.content.(*contentString).value)
			previous = item
			scan = next
			runCount++
		}
		if runCount == 2 {
			leftCursor = rightCursor
			continue
		}

		leftContent := left.content.(*contentString)
		if parent, parentOK := left.parent.(abstractType); parentOK {
			destroyListPositionIndex(parent)
		}
		var combined strings.Builder
		combined.Grow(totalBytes)
		combined.WriteString(leftContent.value)

		mergeCursor := rightCursor
		lastMerged := clientStructCursor{}
		merged := 0
		for i := 1; i < runCount; i++ {
			next := mergeCursor.Value().(*itemStruct)
			if !left.canMergeWith(next) {
				break
			}
			nextContent := next.content.(*contentString)
			combined.WriteString(nextContent.value)
			left.completeMergeWith(next)
			lastMerged = mergeCursor
			merged++
			if i+1 < runCount {
				mergeCursor, _ = mergeCursor.Next()
			}
		}
		if merged == 0 {
			leftCursor = rightCursor
			continue
		}
		leftContent.setMergedString(combined.String())
		next, hasNext := l.Remove(rightCursor, lastMerged)
		if !hasNext {
			return
		}
		leftCursor = next
	}
}

func (l *clientStructList) removePositions(start, count int) {
	if count <= 0 {
		return
	}
	if l.tree.active() != nil {
		first, firstOK := l.cursorAtPosition(start)
		last, lastOK := l.cursorAtPosition(start + count - 1)
		if !firstOK || !lastOK {
			panic("clientStructList.removePositions: range outside tree")
		}
		l.Remove(first, last)
		return
	}
	removed := l.items[start : start+count]
	l.oracle.remove(removed)
	spliceStruct(&l.items, Number(start), Number(count), nil)
	l.generation++
	l.oracle.checkList(l)
}

func (l *clientStructList) requireCursor(cursor clientStructCursor) {
	if cursor.list != l || !cursor.Valid() {
		panic("clientStructList: cursor is foreign, stale, or invalid")
	}
}

func (l *clientStructList) cursorAt(position int) clientStructCursor {
	return clientStructCursor{
		list: l, clientStructCursorState: makeClientStructFlatCursorState(position, l.generation),
	}
}

func (l *clientStructList) cursorAtTree(cursor clientStructTreeCursor) clientStructCursor {
	return clientStructCursor{
		list: l, clientStructCursorState: makeClientStructTreeCursorState(cursor, l.generation),
	}
}

func (l *clientStructList) cursorAtPosition(position int) (clientStructCursor, bool) {
	if tree := l.tree.active(); tree != nil {
		cursor, ok := tree.At(position)
		if !ok {
			return clientStructCursor{}, false
		}
		return l.cursorAtTree(cursor), true
	}
	if position < 0 || position >= len(l.items) {
		return clientStructCursor{}, false
	}
	return l.cursorAt(position), true
}

func (l *clientStructList) findIndex(clock Number) (int, error) {
	if tree := l.tree.active(); tree != nil {
		cursor, err := tree.Find(clock)
		if err != nil {
			return 0, err
		}
		return tree.Index(cursor), nil
	}
	index, err := findIndexSS(l.items, clock)
	return int(index), err
}

func (l *clientStructList) forEachChunk(visit func([]abstractStruct) bool) {
	if tree := l.tree.active(); tree != nil {
		first, ok := tree.First()
		if !ok {
			return
		}
		for chunk, more := tree.ChunkFrom(first), true; more; chunk, more = chunk.Next() {
			if !visit(chunk.Values()) {
				return
			}
		}
		return
	}
	visit(l.items)
}
