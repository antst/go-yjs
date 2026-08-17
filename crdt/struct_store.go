package crdt

import (
	"errors"
)

var errStructNotFound = errors.New("not found")

type structStore struct {
	clients          map[Number]*clientStructList
	pendingStructs   *pendingStructsResult
	pendingDeleteSet []uint8

	// clientOrder records clients in FIRST-INSERTION order.
	//
	// The reference's undo restoration order originates here, not in the delete set: yjs's
	// StructStore.clients is a JS Map, getStateVector preserves its order into afterState, the
	// UndoManager builds its insertions delete set by iterating afterState, addToDeleteSet inserts
	// on first touch, sortAndMergeDeleteSet sorts each client's items but never reorders clients,
	// and iterateDeletedStructs walks that same Map. Go's map iteration is randomised, so the
	// order was lost at the source — which is why ordering the delete set alone could not have
	// reproduced it (research R1).
	//
	// Encoding paths deliberately do NOT read this: they already sort (e.g. writeStateVector
	// descending) and are byte-exact. This exists for the undo path.
	clientOrder []Number
}

// noteClient records a client the first time the store sees it. Idempotent and O(1) amortised;
// the membership check rides on the clients map rather than a second structure.
func (ss *structStore) noteClient(client Number) {
	if _, seen := ss.clients[client]; !seen {
		ss.clientOrder = append(ss.clientOrder, client)
	}
}

// orderedClients returns clients in first-insertion order — the reference's undo restoration order.
// A copy, so callers cannot mutate the store's ordering.
func (ss *structStore) orderedClients() []Number {
	out := make([]Number, 0, len(ss.clientOrder))
	for _, c := range ss.clientOrder {
		if _, still := ss.clients[c]; still {
			out = append(out, c)
		}
	}
	return out
}

// clientStructs is the shared comma-ok accessor for one client's opaque list.
// An absent client yields (nil, false). Centralizing this keeps the absent-client
// guard identical across decode/apply paths that can be handed attacker-named
// clients without exposing the backing representation.
func (ss *structStore) clientStructs(client Number) (*clientStructList, bool) {
	s, ok := ss.clients[client]
	return s, ok
}

// structsForClient returns a caller-owned flattened copy. Mutating the returned slice
// cannot mutate the store or constrain its future internal representation.
func (ss *structStore) structsForClient(client Number) []abstractStruct {
	structs, ok := ss.clientStructs(client)
	if !ok {
		return nil
	}
	return structs.Snapshot(nil)
}

func (ss *structStore) clientLength(client Number) int {
	structs, ok := ss.clientStructs(client)
	if !ok {
		return 0
	}
	return structs.Len()
}

func (ss *structStore) clientCapacity(client Number) int {
	structs, ok := ss.clientStructs(client)
	if !ok {
		return 0
	}
	return cap(structs.items)
}

func (ss *structStore) clientCountValue() int {
	return len(ss.clients)
}

// appendClientIDs appends a caller-owned, unordered client list to dst.
func (ss *structStore) appendClientIDs(dst []Number) []Number {
	dst = dst[:0]
	for client := range ss.clients {
		dst = append(dst, client)
	}
	return dst
}

func (ss *structStore) forEachClient(visit func(client Number, structs *clientStructList) bool) {
	for client, structs := range ss.clients {
		if !visit(client, structs) {
			return
		}
	}
}

func (ss *structStore) appendClientStruct(client Number, value abstractStruct) {
	structs, ok := ss.clientStructs(client)
	if !ok {
		ss.noteClient(client)
		structs = newClientStructList(1)
		ss.clients[client] = structs
	}
	structs.appendValue(value)
}

func (ss *structStore) markPrimitiveMapDeleted(trans *Transaction, parent *YMap) {
	for client, structs := range ss.clients {
		structs.markPrimitiveMapDeleted(trans, parent, client)
	}
}

func newStructStore() *structStore {
	return &structStore{
		clients: make(map[Number]*clientStructList),
	}
}

// Return the states as a Map<client,clock>.
// Note that clock refers to the next expected clock id.
func getStateVector(store *structStore) map[Number]Number {
	sm := make(map[Number]Number)

	store.forEachClient(func(client Number, structs *clientStructList) bool {
		last := structs.lastValue()
		sm[client] = last.getID().Clock + last.structLength()
		return true
	})

	return sm
}

func getState(store *structStore, client Number) Number {
	structs, exist := store.clientStructs(client)
	if !exist {
		return 0
	}

	lastStruct := structs.lastValue()
	return lastStruct.getID().Clock + lastStruct.structLength()
}

// addStruct appends a struct to its client's list, failing when the incoming
// clock does not continue that client's last struct.
//
// yjs throws in exactly this place and neither readUpdateV2 nor applyUpdateV2
// catches it; transact still runs its cleanup in a finally block, so the
// reference behaviour is a caller-visible failure with partial application
// possible. integrateStruct propagates this error for the same reason.
//
// MEASURED 2026-08-17, AND THE MEASUREMENT IS THE POINT: this branch is not
// reachable from any input we can currently produce. A counter in it stayed at
// zero across the entire crdt suite including every oracle differential, and a
// planted panic did not fire in roughly 240,000 fuzz executions across
// FuzzApplyUpdate and FuzzMergeUpdates. The decoder keeps malformed clocks away
// from it through the pending/missing mechanism, and every local integrate path
// derives its clock from getState, which is continuous by construction — which
// is also why those call sites discard the error.
//
// So treat this as an internal consistency assertion, not as input validation,
// and do NOT assume the error paths carrying it upward are exercised by any
// test. They are not. If you make this branch reachable, add the test that
// reaches it in the same change.
func addStruct(store *structStore, st abstractStruct) error {
	client := st.getID().Client
	ss, exist := store.clientStructs(client)

	if !exist {
		store.appendClientStruct(client, st)
	} else {
		lastStruct := ss.lastValue()
		if lastStruct.getID().Clock+lastStruct.structLength() != st.getID().Clock {
			return errors.New("unexpected case")
		}
		ss.appendValue(st)
	}

	return nil
}

func reserveClientStructCapacity(store *structStore, client Number, capacity int) bool {
	structs, ok := store.clientStructs(client)
	if !ok {
		return false
	}
	structs.Reserve(capacity)
	return true
}

func findIndexSS(ss []abstractStruct, clock Number) (Number, error) {
	index, err := binarySearch(ss, clock, 0, len(ss)-1)
	if err != nil {
		return 0, err
	}

	return index, nil
}

func binarySearch(ss []abstractStruct, clock Number, begin, end Number) (Number, error) {
	if begin > end {
		return 0, errStructNotFound
	}

	left, right := begin, end
	mid := (left + right) / 2

	// Match yjs findIndexSS: most stores are dense in clock space, so an
	// interpolation pivot usually lands directly on the target. The last-struct
	// check is also the common append/state lookup fast path.
	if begin == 0 && end == len(ss)-1 {
		last := ss[end]
		lastClock := last.getID().Clock
		if lastClock == clock {
			return end, nil
		}
		span := lastClock + last.structLength() - 1
		if span > 0 && clock >= 0 && clock <= span {
			mid = Number(float64(clock) / float64(span) * float64(end))
		}
	}

	for left <= right {
		s := ss[mid]
		start := s.getID().Clock
		if start <= clock {
			if clock < start+s.structLength() {
				return mid, nil
			}
			left = mid + 1
		} else {
			right = mid - 1
		}
		mid = (left + right) / 2
	}

	return 0, errStructNotFound
}

func findStruct(store *structStore, id ID) (abstractStruct, error) {
	ss, exist := store.clientStructs(id.Client)
	if !exist {
		return nil, errors.New("not exist client")
	}

	cursor, err := ss.Find(id.Clock)
	if err != nil {
		return nil, err
	}

	return cursor.Value(), nil
}

// getStruct returns the struct at id, or nil when the store does not hold one.
// Absence is a normal answer here rather than a failure — callers branch on the
// nil — so the lookup error carries no information the nil does not.
func getStruct(store *structStore, id ID) abstractStruct {
	item, _ := findStruct(store, id)
	return item
}

func findIndexCleanStart(trans *Transaction, ss *clientStructList, clock Number) (clientStructCursor, error) {
	cursor, err := ss.Find(clock)
	if err != nil {
		return clientStructCursor{}, err
	}

	s, ok := cursor.Value().(*itemStruct)
	if ok && s.getID().Clock < clock {
		return ss.InsertAfter(cursor, splitItem(trans, s, clock-s.getID().Clock)), nil
	}

	return cursor, nil
}

// Expects that id is actually in store. This function throws or is an infinite loop otherwise.
func getItemCleanStart(trans *Transaction, id ID) *itemStruct {
	ss, exist := trans.doc.store.clientStructs(id.Client)
	if !exist {
		return nil
	}

	cursor, err := findIndexCleanStart(trans, ss, id.Clock)
	if err != nil {
		return nil
	}

	item, _ := cursor.Value().(*itemStruct)
	return item
}

// Expects that id is actually in store. This function throws or is an infinite loop otherwise.
func getItemCleanEnd(trans *Transaction, store *structStore, id ID) *itemStruct {
	ss, exist := store.clientStructs(id.Client)
	if !exist {
		return nil
	}

	cursor, err := ss.Find(id.Clock)
	if err != nil {
		return nil
	}

	s, ok := cursor.Value().(*itemStruct)
	if !ok {
		return nil
	}

	if id.Clock != s.getID().Clock+s.structLength()-1 {
		rightItem := splitItem(trans, s, id.Clock-s.getID().Clock+1)
		ss.InsertAfter(cursor, rightItem)
	}

	return s
}

// Replace item(*GC|*Item) with newItem(*GC|*Item) in store
func replaceStruct(store *structStore, item abstractStruct, newItem abstractStruct) error {
	if item.getID().Client != newItem.getID().Client {
		return errors.New("cannot replace struct when tow items' client are different")
	}

	ss, exist := store.clientStructs(item.getID().Client)
	if !exist {
		return errors.New("not exist client")
	}

	cursor, err := ss.Find(item.getID().Clock)
	if err != nil {
		return err
	}

	ss.Replace(cursor, newItem)
	return nil
}

// Iterate over a range of structs
func iterateStructs(trans *Transaction, ss *clientStructList, clockStart Number, length Number, f func(s abstractStruct)) {
	if length == 0 {
		return
	}

	// SATURATE the range end (H8) — a DEFENSIVE FALLBACK only. clockStart/length here
	// are del.Clock/del.Length forwarded from IterateDeletedStructs, i.e. decoded
	// delete-set values now clamped to [0, maxSafeInteger] (9th review), so their sum
	// is <= 2^54-2 and cannot wrap: the `clockEnd < ...Clock+...Length` and
	// `...Clock >= clockEnd` loop bounds below are correct for all DECODED input
	// without the saturate. It is NOT "iterates to the end, as Yjs (float64) does":
	// Yjs THROWS at decode (readVarUint > 2^53) on EVERY path including
	// snapshot/permanent-user-data, so it never reaches this iteration with an
	// over-range value. The saturate survives only for a DeleteItem built WITHOUT the
	// decode clamp, where a wrapped negative clockEnd would otherwise make the
	// iteration (snapshot restore / undo / permanent-user-data) stop early and skip
	// structs.
	clockEnd := addClockSaturating(clockStart, length)
	cursor, err := findIndexCleanStart(trans, ss, clockStart)
	if err != nil {
		return
	}

	for {
		s := cursor.Value()

		if clockEnd < s.getID().Clock+s.structLength() {
			_, err := findIndexCleanStart(trans, ss, clockEnd)
			if err != nil {
				return
			}
		}

		f(s)

		// Splitting at clockEnd advances the list generation. Reacquire this
		// struct by its unchanged start clock before asking for its successor.
		if !cursor.Valid() {
			cursor, err = ss.Find(s.getID().Clock)
			if err != nil {
				return
			}
		}
		next, ok := cursor.Next()
		if !ok || next.Value().getID().Clock >= clockEnd {
			break
		}
		cursor = next
	}
}
