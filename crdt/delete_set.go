package crdt

import (
	"errors"
	"fmt"
	"sort"
)

/*
 * We no longer maintain a DeleteStore. DeleteSet is a temporary object that is created when needed.
 * - When created in a transaction, it must only be accessed after sorting, and merging
 *   - This DeleteSet is send to other clients
 * - We do not create a DeleteSet when we send a sync message. The DeleteSet message is created directly from StructStore
 * - We read a DeleteSet as part of a sync/update message. In this case the DeleteSet is already sorted and merged.
 */

type deleteItem struct {
	clock  Number
	length Number
}

type deleteSet struct {
	clients map[Number][]*deleteItem

	// clientOrder records clients in FIRST-INSERTION order.
	//
	// The reference's DeleteSet.clients is a JS Map, so `iterateDeletedStructs` walks clients in
	// insertion order. Go map iteration is randomised, which made undo restoration order vary
	// RUN TO RUN — not merely differ from the reference. Ordering only the delete set's
	// construction is not enough: the order has to be carried through iteration too, which is
	// what this field is for (research R1).
	clientOrder []Number
}

// noteClient records a client the first time this delete set sees it.
func (ds *deleteSet) noteClient(client Number) {
	if _, seen := ds.clients[client]; !seen {
		ds.clientOrder = append(ds.clientOrder, client)
	}
}

// orderedClients returns clients in first-insertion order, falling back to any client present in the
// map but not yet recorded (a delete set built by direct map assignment rather than through
// AddToDeleteSet). Sorted for those, so the result is never randomised.
func (ds *deleteSet) orderedClients() []Number {
	out := make([]Number, 0, len(ds.clients))
	seen := make(map[Number]bool, len(ds.clients))
	for _, c := range ds.clientOrder {
		if _, still := ds.clients[c]; still && !seen[c] {
			out = append(out, c)
			seen[c] = true
		}
	}
	if len(out) == len(ds.clients) {
		return out
	}
	var rest []Number
	for c := range ds.clients {
		if !seen[c] {
			rest = append(rest, c)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	return append(out, rest...)
}

func newDeleteItem(clock Number, length Number) *deleteItem {
	return &deleteItem{clock: clock, length: length}
}

func newDeleteSet() *deleteSet {
	return &deleteSet{
		clients: make(map[Number][]*deleteItem),
	}
}

// Iterate over all structs that the DeleteSet gc's.
// f func(*GC|*Item)
func iterateDeletedStructs(trans *Transaction, ds *deleteSet, f func(s abstractStruct)) {
	// Belt-and-suspenders: a nil delete set (e.g. an attacker-crafted snapshot
	// blob whose Ds failed to decode) is treated as empty rather than
	// dereferenced (a nil map range is legal in Go, but a nil *DeleteSet is not).
	if ds == nil {
		return
	}
	// Insertion order, not Go map order. The reference iterates a JS Map here, and undo
	// restoration order is observable, so a randomised walk made the result vary between runs of
	// the same input (research R1).
	for _, clientID := range ds.orderedClients() {
		deletes := ds.clients[clientID]
		// The delete set can name a client absent from the store (e.g. a crafted
		// snapshot/update). store.Clients[clientID] is then nil and len(*ss) derefs
		// -> SIGSEGV; comma-ok before the deref (an absent client has nothing to
		// iterate). Routed through the shared clientStructs accessor (F#5/H#4).
		ss, ok := trans.doc.store.clientStructs(clientID)
		if !ok || ss.Len() == 0 {
			continue
		}

		for i := 0; i < len(deletes); i++ {
			del := deletes[i]
			iterateStructs(trans, ss, del.clock, del.length, f)
		}
	}
}

func findIndexDS(dis []*deleteItem, clock Number) (Number, error) {
	left := 0
	right := len(dis) - 1

	for left <= right {
		midIndex := (left + right) / 2
		mid := dis[midIndex]
		midClock := mid.clock

		if midClock <= clock {
			// Saturate midClock+mid.Length (H8) — a DEFENSIVE FALLBACK only. After the
			// per-read clamp dropped to maxSafeInteger (9th review), a decoded
			// DeleteItem's clock and length are each <= 2^53-1, so midClock+mid.Length
			// is <= 2^54-2 and cannot wrap: this membership test is correct for all
			// DECODED input without the saturate. It is NOT a "matches-Yjs
			// (float64 delete-to-end)" behavior — Yjs THROWS at decode on an over-range
			// length and never reaches a membership probe with one. The saturate
			// survives only for a DeleteItem built WITHOUT the decode clamp (direct
			// AddToDeleteSet, as the H8 unit test does), keeping IsDeleted from
			// reporting a genuinely-deleted struct as live on a wrapped sum. No effect
			// on valid (small) ranges.
			if clock < addClockSaturating(midClock, mid.length) {
				return midIndex, nil
			}
			left = midIndex + 1
		} else {
			right = midIndex - 1
		}
	}

	return 0, errors.New("not found")
}

func isDeleted(ds *deleteSet, id *ID) bool {
	// Belt-and-suspenders: a nil delete set is treated as "nothing deleted"
	// rather than dereferenced (Ds.Clients on a nil *DeleteSet panics).
	if ds == nil {
		return false
	}
	dis := ds.clients[id.Client]
	_, err := findIndexDS(dis, id.Clock)
	return dis != nil && err == nil
}

func sortAndMergeDeleteSet(ds *deleteSet) {
	for client, dels := range ds.clients {
		if len(dels) < 2 {
			continue
		}
		if len(dels) <= 16 {
			for i := 1; i < len(dels); i++ {
				item := dels[i]
				j := i
				for j > 0 && dels[j-1].clock > item.clock {
					dels[j] = dels[j-1]
					j--
				}
				dels[j] = item
			}
		} else {
			sort.Slice(dels, func(i, j int) bool {
				return dels[i].clock < dels[j].clock
			})
		}

		// merge items without filtering or splicing the array
		// i is the current pointer
		// j refers to the current insert position for the pointed item
		// try to merge dels[i] into dels[j-1] or set dels[j]=dels[i]
		i, j := 1, 1
		for ; i < len(dels); i++ {
			left := dels[j-1]
			right := dels[i]

			// SATURATE both range ends (H8) — a DEFENSIVE FALLBACK only. left/right are
			// DeleteItems whose Clock and Length came from ReadDsClock/ReadDsLen, now
			// each clamped to [0, maxSafeInteger] (9th review), so a `Clock+Length` end
			// is <= 2^54-2 and cannot wrap: this merge is correct for all DECODED input
			// without the saturate, and it is NOT a Yjs-matching behavior (Yjs THROWS at
			// decode on an over-range length, never reaching the merge with one). The
			// saturate survives only for a DeleteItem built WITHOUT the decode clamp:
			// without it a wrapped left end would make the `>= right.Clock` merge test
			// spuriously FALSE (splitting a range that should merge), and a wrapped
			// right end would corrupt the merged length via Max.
			if addClockSaturating(left.clock, left.length) >= right.clock {
				left.length = maxNumber(left.length, addClockSaturating(right.clock, right.length)-left.clock)
			} else {
				if j < i {
					dels[j] = right
				}
				j++
			}
		}

		ds.clients[client] = dels[:j]
	}
}

func mergeDeleteSets(dss []*deleteSet) *deleteSet {
	merged := newDeleteSet()

	for _, ds := range dss {
		// ReadDeleteSet returns nil on a truncated/malformed delete set; skip
		// such entries rather than dereferencing a nil *DeleteSet (panic).
		if ds == nil {
			continue
		}
		for _, client := range ds.orderedClients() {
			source := ds.clients[client]
			dels, exist := merged.clients[client]
			if !exist {
				merged.noteClient(client)
				if source != nil {
					dels = make([]*deleteItem, 0, len(source))
				}
			}
			// The merged set owns every range it sorts and coalesces. Appending the
			// source pointers aliases caller state: SortAndMergeDeleteSet mutates the
			// left range's Length, whereas pinned yjs replaces it with a new DeleteItem.
			for _, item := range source {
				if item == nil {
					dels = append(dels, nil)
				} else {
					dels = append(dels, newDeleteItem(item.clock, item.length))
				}
			}
			merged.clients[client] = dels
		}
	}

	sortAndMergeDeleteSet(merged)
	return merged
}

func addToDeleteSet(ds *deleteSet, client Number, clock Number, length Number) {
	if ds.clients == nil {
		ds.clients = make(map[Number][]*deleteItem)
	}
	// Record BEFORE the append: noteClient keys off the map.
	ds.noteClient(client)
	ds.clients[client] = append(ds.clients[client], newDeleteItem(clock, length))
}

func newDeleteSetFromStructStore(ss *structStore) *deleteSet {
	ds := newDeleteSet()
	// Store insertion order, matching yjs's createDeleteSetFromStructStore, which iterates
	// ss.clients — a JS Map. Ranging the Go map randomised the resulting delete set's client
	// order, which is observable through undo restoration order.
	for _, client := range ss.orderedClients() {
		structs, ok := ss.clientStructs(client)
		if !ok {
			continue
		}
		disItems := structs.appendDeletedRanges(nil)

		if len(disItems) > 0 {
			ds.noteClient(client)
			ds.clients[client] = disItems
		}
	}

	return ds
}

// writeDeleteSet serializes a DeleteSet through any DSEncoder (V1 or V2). The
// per-format delta coding of clock/len is encapsulated in the encoder, so this
// single function replaces the former writeDeleteSet/WriteDeleteSetV2 pair.
//
// It returns an error if the encoder rejects a delete range (the V2 encoder
// rejects a 0-length range, which a hostile V1 update can carry through the
// convert/merge paths). Delete sets built from live document state never trip
// this, so trusted encode paths can ignore the error; the convert/merge/diff
// paths propagate it so a malformed re-encode fails gracefully instead of
// emitting a corrupt stream or panicking.
func writeDeleteSet(encoder dsEncoder, ds *deleteSet) error {
	// A nil delete set (e.g. ReadDeleteSet returned nil on a truncated update)
	// is written as an empty set rather than dereferenced — keeps the encoder
	// from panicking on malformed input fed through the convert/merge paths.
	if ds == nil {
		writeEncoderRestVarUint(encoder, 0)
		return nil
	}
	writeEncoderRestVarUint(encoder, uint64(len(ds.clients)))

	// Yjs writes the delete set in a deterministic order — clients sorted by
	// clientID descending (writeDeleteSet: `.sort((a, b) => b[0] - a[0])`). A raw
	// Go map range is non-deterministic, so iterating it directly breaks the
	// byte-identical-vs-Yjs goal whenever the set spans more than one client.
	// Sort clientIDs descending to match.
	clients := make([]Number, 0, len(ds.clients))
	for client := range ds.clients {
		clients = append(clients, client)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })

	for _, client := range clients {
		dsItems := ds.clients[client]
		encoder.resetDS()
		writeEncoderRestVarUint(encoder, uint64(client))

		length := len(dsItems)
		writeEncoderRestVarUint(encoder, uint64(length))

		for i := 0; i < length; i++ {
			item := dsItems[i]
			encoder.writeDSClock(item.clock)
			if err := encoder.writeDSLength(item.length); err != nil {
				return err
			}
		}
	}

	return nil
}

// deletedStructRangeCount returns the number of contiguous deleted runs in a
// client's StructStore slice. StructStore entries are clock ordered, so each
// run is already in the order WriteDeleteSet requires.
func deletedStructRangeCount(structs []abstractStruct) int {
	ranges := 0
	wasDeleted := false
	for _, s := range structs {
		deleted := s.isDeleted()
		if deleted && !wasDeleted {
			ranges++
		}
		wasDeleted = deleted
	}
	return ranges
}

// writeDeletedStructRanges writes one client's deleted runs directly from its
// live StructStore slice. This is the allocation-free counterpart of building
// a temporary DeleteSet with NewDeleteSetFromStructStore and immediately
// serializing it. It is used only by full-state encoders; callers that need a
// durable DeleteSet (snapshots, undo, decoded updates) retain the materialized
// representation.
func writeDeletedStructRanges(encoder dsEncoder, client Number, structs []abstractStruct, rangeCount int) error {
	encoder.resetDS()
	writeEncoderRestVarUint(encoder, uint64(client))
	writeEncoderRestVarUint(encoder, uint64(rangeCount))

	for i := 0; i < len(structs); i++ {
		s := structs[i]
		if !s.isDeleted() {
			continue
		}
		clock := s.getID().Clock
		length := s.structLength()
		for i+1 < len(structs) && structs[i+1].isDeleted() {
			i++
			length += structs[i].structLength()
		}
		encoder.writeDSClock(clock)
		if err := encoder.writeDSLength(length); err != nil {
			return err
		}
	}
	return nil
}

// writeDeleteSetFromStructStore serializes the delete set directly from live
// store state. sortedClients, when non-nil, must contain all store clients in
// descending numeric order; the full-state encoders reuse the client slice they
// already sorted for struct output. The single-client path needs no slice.
func writeDeleteSetFromStructStore(encoder dsEncoder, ss *structStore, sortedClients []Number) error {
	if ss.clientCountValue() == 0 {
		writeEncoderRestVarUint(encoder, 0)
		return nil
	}

	if ss.clientCountValue() == 1 {
		var writeErr error
		ss.forEachClient(func(client Number, structs *clientStructList) bool {
			rangeCount := structs.deletedRangeCount()
			if rangeCount == 0 {
				writeEncoderRestVarUint(encoder, 0)
				return false
			}
			writeEncoderRestVarUint(encoder, 1)
			writeErr = structs.writeDeletedRanges(encoder, client, rangeCount)
			return false
		})
		return writeErr
	}

	clients := sortedClients
	if len(clients) != ss.clientCountValue() {
		clients = ss.appendClientIDs(make([]Number, 0, ss.clientCountValue()))
		sort.Slice(clients, func(i, j int) bool { return clients[i] > clients[j] })
	}

	deletedClients := 0
	for _, client := range clients {
		structs, _ := ss.clientStructs(client)
		if structs.deletedRangeCount() > 0 {
			deletedClients++
		}
	}
	writeEncoderRestVarUint(encoder, uint64(deletedClients))
	for _, client := range clients {
		structs, _ := ss.clientStructs(client)
		rangeCount := structs.deletedRangeCount()
		if rangeCount > 0 {
			if err := structs.writeDeletedRanges(encoder, client, rangeCount); err != nil {
				return err
			}
		}
	}
	return nil
}

// readDeleteSet decodes a delete set from the decoder's rest buffer.
//
// It surfaces a truncation/malformation error (like readStateVector) instead of
// returning a bare nil. A bare nil-without-error was read by callers as an
// empty-but-valid delete set, with dangerous downstream consequences: a
// malformed snapshot blob decoded "successfully" with Ds==nil and then panicked
// on the first content read (IsDeleted / IterateDeletedStructs dereference
// Ds.Clients), and a malformed per-user delete set was silently dropped
// (deletes lost). Every per-entry read error is now checked and returned, so a
// malformed delete set fails loudly at decode time instead of much later.
// readDeleteSet reads a delete set through a DS-level decoder (ReadDsClock/
// ReadDsLen + rest-framing). Narrowed from UpdateDecoder to DSDecoder so a bare
// DSDecoderV2 (snapshots) can be passed; UpdateDecoder still satisfies DSDecoder,
// so existing callers are unaffected.
func readDeleteSet(decoder dsDecoder) (*deleteSet, error) {
	ds := newDeleteSet()

	rest := decoder.restDecoder()
	n, err := readVarUintAny(rest)
	if err != nil {
		return nil, fmt.Errorf("read delete set: client count: %w", err)
	}

	numClients := n.(uint64)
	for i := uint64(0); i < numClients; i++ {
		decoder.resetDS()

		// Clamp the DS client id through readVarUintAsNumber: a value in
		// [2^63, 2^64) must be REJECTED, not wrapped to a NEGATIVE Number that keys
		// the delete-set map and later flows into GetState / clientStructs (the
		// negative-wrap class, H#3).
		client, err := readVarUintAsNumber(rest)
		if err != nil {
			return nil, fmt.Errorf("read delete set: client[%d]: %w", i, err)
		}

		n, err = readVarUintAny(rest)
		if err != nil {
			return nil, fmt.Errorf("read delete set: range count[%d]: %w", i, err)
		}

		numberOfDeletes := n.(uint64)

		for j := uint64(0); j < numberOfDeletes; j++ {
			dsClock, err := decoder.readDSClock()
			if err != nil {
				return nil, fmt.Errorf("read delete set: client[%d] range[%d] clock: %w", i, j, err)
			}

			dsLength, err := decoder.readDSLength()
			if err != nil {
				return nil, fmt.Errorf("read delete set: client[%d] range[%d] len: %w", i, j, err)
			}

			// Record wire order: a decoded delete set's client order is the order the peer
			// wrote it, and it must not degrade to a Go map's random iteration afterwards.
			ds.noteClient(client)
			ds.clients[client] = append(ds.clients[client], newDeleteItem(dsClock, dsLength))
		}
	}

	return ds, nil
}

// dsClientBlock is one client's delete ranges from a delete-set frame, captured
// in STREAM order so the apply (and the re-encoded pending-DS output) reproduce
// the original ordering byte-for-byte.
type dsClientBlock struct {
	client Number
	ranges []*deleteItem
}

// readDeleteSetOrdered fully decodes a delete-set frame into per-client blocks in
// stream order, returning an error on ANY truncation BEFORE a single byte of it
// is applied. This is what makes ReadAndApplyDeleteSet all-or-nothing: the old
// code decoded-and-applied in one streaming pass and returned a bare nil mid-way
// on truncation, so a truncated delete-only update applied a graded, silent
// SUBSET of its deletes — the receiver then permanently kept deletions the
// sender had removed, with no error and no panic. Decoding fully first (the same
// discipline as the hardened ReadDeleteSet / decodeAwarenessEntries) means a
// truncated frame is rejected with zero mutation.
func readDeleteSetOrdered(decoder updateDecoder) ([]dsClientBlock, error) {
	rest := decoder.restDecoder()
	n, err := readVarUintAny(rest)
	if err != nil {
		return nil, fmt.Errorf("read delete set: client count: %w", err)
	}
	numClients := n.(uint64)

	// DoS 3: numClients is an attacker-controlled varint; bound it against the
	// bytes that actually remain before make() so a hostile count (e.g. 2^62)
	// can't trigger `makeslice: cap out of range`. Every client block is at least
	// 2 bytes on the wire (a client-id varint + a range-count varint), so a count
	// larger than rest.Len()/2 is provably truncated. Error (all-or-nothing) —
	// mirroring decoding.go's `size > decoder.Len()` guard and the awareness /
	// readArrayDepth bounds — rather than clamp and silently decode a partial set.
	if boundExceeded(numClients, rest.Len(), 2) {
		return nil, fmt.Errorf("read delete set: client count %d exceeds remaining %d bytes (each client needs >=2)", numClients, rest.Len())
	}

	blocks := make([]dsClientBlock, 0, numClients)
	for i := uint64(0); i < numClients; i++ {
		decoder.resetDS()

		// Clamp the DS client id (see ReadDeleteSet): reject [2^63, 2^64) rather than
		// wrap NEGATIVE into the delete-set map key (H#3).
		client, err := readVarUintAsNumber(rest)
		if err != nil {
			return nil, fmt.Errorf("read delete set: client[%d]: %w", i, err)
		}

		n, err = readVarUintAny(rest)
		if err != nil {
			return nil, fmt.Errorf("read delete set: range count[%d]: %w", i, err)
		}
		numberOfDeletes := n.(uint64)

		// DoS 3: numberOfDeletes is an attacker-controlled varint; bound it against
		// the bytes that actually remain before make(). Every delete range is at
		// least 2 bytes on the wire (a clock varint + a len varint), so a count
		// larger than rest.Len()/2 is provably truncated. Error (all-or-nothing) —
		// same discipline as the numClients bound above.
		if boundExceeded(numberOfDeletes, rest.Len(), 2) {
			return nil, fmt.Errorf("read delete set: range count[%d] %d exceeds remaining %d bytes (each range needs >=2)", i, numberOfDeletes, rest.Len())
		}

		block := dsClientBlock{client: client, ranges: make([]*deleteItem, 0, numberOfDeletes)}
		for j := uint64(0); j < numberOfDeletes; j++ {
			clock, err := decoder.readDSClock()
			if err != nil {
				return nil, fmt.Errorf("read delete set: client[%d] range[%d] clock: %w", i, j, err)
			}

			// Yjs uses readDsLen here (delta-coded under V2); the former
			// ReadLen happened to be byte-identical only for V1.
			length, err := decoder.readDSLength()
			if err != nil {
				return nil, fmt.Errorf("read delete set: client[%d] range[%d] len: %w", i, j, err)
			}

			block.ranges = append(block.ranges, newDeleteItem(clock, length))
		}
		blocks = append(blocks, block)
	}

	return blocks, nil
}

// readAndApplyDeleteSet reads a delete set and applies the deletes whose target
// structs are already present, returning a V2-encoded pending delete set for the
// ranges that could not yet be applied (or nil if none), and an error on a
// truncated/malformed frame.
//
// The error return (added to close a silent-delete-drop bug) lets the live apply
// path (readUpdateV2) distinguish a truncation from a clean apply that yielded no
// pending DS: previously both returned a bare nil, so a truncated delete-only
// update silently dropped a graded subset of its deletes and the caller treated
// the nil as "no pending DS". The whole frame is now decoded (in stream order)
// before any mutation, so a truncated frame is rejected with NOTHING applied.
func readAndApplyDeleteSet(decoder updateDecoder, trans *Transaction, store *structStore) ([]uint8, error) {
	blocks, err := readDeleteSetOrdered(decoder)
	if err != nil {
		return nil, err
	}

	unappliedDS := newDeleteSet()

	for _, block := range blocks {
		client := block.client
		// A delete set can name a client ABSENT from the store — and that is the
		// NORMAL CRDT case, not just a hostile one: a peer may delete a range whose
		// structs have not arrived yet (e.g. the delete-only update is applied before
		// the structs update). Upstream Yjs reads `structs = store.clients.get(client)
		// || []` and lets such a delete fall to the `else` branch below, which queues
		// it in unappliedDS as PENDING so it re-applies once the structs arrive.
		//
		// So we must NOT skip an absent client (that would DROP its delete ranges and
		// permanently diverge the peer — A regression). Instead keep the comma-ok and
		// gate ONLY the list lookup on presence: with `ok && clock < state`, an
		// absent client (!ok) always takes the else->unappliedDS (pending) branch,
		// while a present client behaves exactly as before.
		structs, ok := store.clientStructs(client)
		state := getState(store, client) // 0 for an absent client (no deref)
		for _, del := range block.ranges {
			clock := del.clock
			length := del.length
			// REJECT a range whose clock+length overflows Number (H8) — now
			// DEFENSE-IN-DEPTH. After the per-read clamp dropped to maxSafeInteger
			// (9th review), ReadDsClock/ReadDsLen reject any single decoded DS
			// clock or length > 2^53 AT DECODE (toNumber), so every (clock, length)
			// reaching here is <= 2^53-1 and clock+length is <= 2^54-2 — it can no
			// longer approach, let alone wrap at, 2^63. This addClock therefore only
			// fires for a range constructed WITHOUT the decode clamp; it is kept so a
			// wrapped-negative clockEnd can never silently defeat the
			// `state < clockEnd` / `st.Clock < clockEnd` filters below (silent delete
			// drop).
			//
			// On convergence with Yjs: the earlier comment said Yjs "THROWS and does
			// NOT apply it", implying a throw-before-any-apply with rollback. That is
			// WRONG — Yjs's readAndApplyDeleteSet is STREAMING: it applies each range
			// as it decodes and only THROWS "Integer out of Range" when it later reads
			// an over-range field, with NO rollback of the ranges it already applied.
			// So both peers end in the SAME partial-apply state (earlier ranges
			// applied, the malformed range and everything after it dropped) and
			// CONVERGE. (Here the reject moves to decode, which likewise applies the
			// structs first then refuses the delete frame.) Saturating to
			// delete-to-end would instead DIVERGE — deleting content a Yjs peer keeps.
			clockEnd, ceErr := addClock(clock, length)
			if ceErr != nil {
				return nil, fmt.Errorf("read and apply delete set: client[%d] range [clock %d, +%d] overflows Number: %w", client, clock, length, ceErr)
			}

			if ok && clock < state {
				if state < clockEnd {
					addToDeleteSet(unappliedDS, client, state, clockEnd-state)
				}

				if err := structs.applyDeleteRange(trans, clock, clockEnd); err != nil {
					return nil, fmt.Errorf("read and apply delete set: client[%d] clock %d: %w", client, clock, err)
				}
			} else {
				addToDeleteSet(unappliedDS, client, clock, clockEnd-clock)
			}
		}
	}

	if len(unappliedDS.clients) > 0 {
		ds := newDefaultUpdateEncoderV2()
		writeVarUint(ds.restEncoder(), 0)
		// The pending delete set is re-encoded as V2 (len-1 framing). A malformed
		// source update can carry a 0-length range that V2 cannot represent; on
		// that error drop the pending DS rather than panic — the applicable
		// structs were already deleted above, and a corrupt residual is not worth
		// crashing the apply for.
		if err := writeDeleteSet(ds, unappliedDS); err != nil {
			return nil, nil
		}
		return ds.toBytes(), nil
	}

	return nil, nil
}

// equalDeleteSets reports whether two delete sets describe exactly the same deletions.
//
// Faithful to yjs equalDeleteSets (src/utils/DeleteSet.js): same client count, and for every client
// the same ranges in the same ORDER. Range order is significant in the reference and is preserved
// here; client order is not, which matters because Go map iteration is randomised — comparing by
// lookup rather than by iteration pairing is what keeps the result deterministic.
func equalDeleteSets(ds1, ds2 *deleteSet) bool {
	if ds1 == nil || ds2 == nil {
		return ds1 == ds2
	}
	if len(ds1.clients) != len(ds2.clients) {
		return false
	}
	for client, items1 := range ds1.clients {
		items2, ok := ds2.clients[client]
		if !ok || len(items1) != len(items2) {
			return false
		}
		for i := range items1 {
			if items1[i].clock != items2[i].clock || items1[i].length != items2[i].length {
				return false
			}
		}
	}
	return true
}
