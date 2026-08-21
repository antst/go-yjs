package crdt

import (
	"bytes"
	"errors"
	"fmt"
)

type Snapshot struct {
	deleteSet   *deleteSet
	stateVector map[Number]Number // state map
}

func newSnapshot(ds *deleteSet, sv map[Number]Number) *Snapshot {
	return &Snapshot{
		deleteSet:   ds,
		stateVector: sv,
	}
}

func EqualSnapshots(snap1, snap2 *Snapshot) bool {
	// Belt-and-suspenders: treat a nil Ds as an empty client map rather than
	// dereferencing it (snap.Ds.Clients panics when Ds is nil). DecodeSnapshot
	// now rejects a malformed blob, but a Snapshot can also be built directly
	// with a nil Ds, so guard here too.
	var ds1, ds2 map[Number][]*deleteItem
	if snap1.deleteSet != nil {
		ds1 = snap1.deleteSet.clients
	}
	if snap2.deleteSet != nil {
		ds2 = snap2.deleteSet.clients
	}
	sv1 := snap1.stateVector
	sv2 := snap2.stateVector

	if len(sv1) != len(sv2) || len(ds1) != len(ds2) {
		return false
	}

	for key, value := range sv1 {
		if sv2[key] != value {
			return false
		}
	}

	for client, delItems1 := range ds1 {
		delItems2 := ds2[client]
		if len(delItems1) != len(delItems2) {
			return false
		}

		for i := 0; i < len(delItems1); i++ {
			delItem1 := delItems1[i]
			delItem2 := delItems2[i]

			if delItem1.clock != delItem2.clock || delItem1.length != delItem2.length {
				return false
			}
		}
	}

	return true
}

// encodeSnapshot writes the snapshot's delete set + state vector through encoder.
// A DSEncoderV1 (NewUpdateEncoderV1) yields V1 bytes, a DSEncoderV2
// (NewUpdateEncoderV2) yields V2 bytes — matching yjs encodeSnapshotV2(snapshot,
// encoder). The delete set uses the encoder's DS codec (delta-coded under V2); the
// state vector uses the encoder's varint rest-framing (identical for V1 and V2).
func encodeSnapshot(snapshot *Snapshot, encoder dsEncoder) ([]uint8, error) {
	// A snapshot's delete set comes from live document state (length >= 1), so
	// this cannot error in practice; propagate it rather than swallow it silently
	// (consistent with the merge/diff/convert encode entry points).
	if err := writeDeleteSet(encoder, snapshot.deleteSet); err != nil {
		return nil, fmt.Errorf("encode snapshot: delete set encode failed: %w", err)
	}
	writeStateVector(encoder, snapshot.stateVector)
	// The struct/content writers cannot return an error directly; a deferred
	// column-encode failure (e.g. a V2 clock-column overflow) is surfaced via
	// Error() after ToUint8Array. Check it so a truncated snapshot is refused.
	out := encoder.toBytes()
	if err := encoder.encodeError(); err != nil {
		return nil, fmt.Errorf("encode snapshot: encode failed: %w", err)
	}
	return out, nil
}

// EncodeSnapshotV2 encodes a snapshot in the V2 format (yjs encodeSnapshotV2
// default) using a BARE DSEncoderV2 — the full UpdateEncoderV2 would prepend empty
// struct-column headers, which a snapshot must not contain.
func EncodeSnapshotV2(snapshot *Snapshot) ([]uint8, error) {
	return encodeSnapshot(snapshot, &dsEncoderV2{rest: new(bytes.Buffer)})
}

// EncodeSnapshotV1 encodes a snapshot in the V1 format (yjs encodeSnapshot, a bare
// DSEncoderV1).
func EncodeSnapshotV1(snapshot *Snapshot) ([]uint8, error) {
	return encodeSnapshot(snapshot, &dsEncoderV1{rest: new(bytes.Buffer)})
}

// EncodeSnapshot defaults to V1 — the historical, wire-compatible default.
func EncodeSnapshot(snapshot *Snapshot) ([]uint8, error) {
	return EncodeSnapshotV1(snapshot)
}

// decodeSnapshot reads a snapshot's delete set + state vector through decoder. A
// malformed/truncated section fails here, not silently producing a Snapshot{Ds:nil}
// that later panics on the first content read.
func decodeSnapshot(decoder dsDecoder) (*Snapshot, error) {
	ds, err := readDeleteSet(decoder)
	if err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	sv, err := readStateVector(decoder)
	if err != nil {
		return nil, fmt.Errorf("decode snapshot: %w", err)
	}
	return newSnapshot(ds, sv), nil
}

// DecodeSnapshotV2 decodes a V2-encoded snapshot (yjs decodeSnapshotV2 default),
// using a bare DSDecoderV2.
func DecodeSnapshotV2(buf []uint8) (*Snapshot, error) {
	return decodeSnapshot(newDSDecoderV2(bytes.NewBuffer(buf)))
}

// DecodeSnapshotV1 decodes a V1-encoded snapshot (yjs decodeSnapshot). UpdateDecoderV1
// is the V1 DS decoder (V1 has no column header) and satisfies DSDecoder.
func DecodeSnapshotV1(buf []uint8) (*Snapshot, error) {
	return decodeSnapshot(newUpdateDecoderV1(buf))
}

// DecodeSnapshot defaults to V1 — the historical, wire-compatible default.
// CALLER BEWARE, and this is a property of the FORMAT rather than of this implementation: a
// snapshot encoding carries no version marker, so neither this library nor the reference can tell
// V1 bytes from V2 bytes. Feeding one to the other's decoder SUCCEEDS and yields a structurally
// valid but WRONG snapshot. Verified identical in yjs@13.6.31, where decodeSnapshotV2(v1Bytes) and
// decodeSnapshot(v2Bytes) both return a snapshot that fails equalSnapshots against the original.
//
// Making a mismatch an error here would therefore be a DEVIATION from the reference, not a fix: it
// would reject input yjs accepts. The mitigation is on the caller — record which encoding you
// wrote and read it back with the matching DecodeSnapshotV1 / DecodeSnapshotV2 rather than relying
// on this default. Pinned by TestSnapshotCrossFormatDecodeMatchesReference.
func DecodeSnapshot(buf []uint8) (*Snapshot, error) {
	return DecodeSnapshotV1(buf)
}

func EmptySnapshot() *Snapshot {
	return newSnapshot(newDeleteSet(), make(map[Number]Number))
}

// NewSnapshotByDoc returns a snapshot of doc's current state.
func NewSnapshotByDoc(doc *Doc) *Snapshot {
	return newSnapshot(newDeleteSetFromStructStore(doc.store), getStateVector(doc.store))
}

func isVisible(item *itemStruct, snapshot *Snapshot) bool {
	if snapshot == nil {
		return !item.isDeleted()
	}

	state := snapshot.stateVector[item.id.Client]
	return state > item.id.Clock && !isDeleted(snapshot.deleteSet, &item.id)
}

// transactionMetaKey is a comparable, collision-free key type for Transaction.Meta.
//
// The reference keys this bucket with the FUNCTION ITSELF —
// `transaction.meta.set(splitSnapshotAffectedStructs, ...)` — which JS allows because a Map keys
// functions by identity. That was ported literally into a Go `map[interface{}]Set`, where func
// values are NOT hashable: every call panicked with "hash of unhashable type". Since this is the
// only path that reaches it, `ToDelta` with any non-nil snapshot panicked outright — the whole
// track-changes rendering was dead code that had never been executed.
//
// A distinct named type gives the same "unique key identifying this operation" semantics the
// reference gets from function identity, without being confusable with any other interface{} key.
type transactionMetaKey string

const splitSnapshotAffectedStructsKey transactionMetaKey = "splitSnapshotAffectedStructs"

func splitSnapshotAffectedStructs(trans *Transaction, snapshot *Snapshot) {
	_, exist := trans.meta[splitSnapshotAffectedStructsKey]
	if !exist {
		trans.meta[splitSnapshotAffectedStructsKey] = NewSet()
	}

	meta := trans.meta[splitSnapshotAffectedStructsKey]
	store := trans.doc.store

	// check if we already split for this snapshot
	if _, exist := meta[snapshot]; !exist {
		for client, clock := range snapshot.stateVector {
			if clock < getState(store, client) {
				getItemCleanStart(trans, GenID(client, clock))
			}
		}

		iterateDeletedStructs(trans, snapshot.deleteSet, func(s abstractStruct) {})
		meta.Add(snapshot)
	}
}

func CreateDocFromSnapshot(originDoc *Doc, snapshot *Snapshot, newDoc *Doc) (*Doc, error) {
	if originDoc.GC {
		// we should not try to restore a GC-ed document, because some of the restored items might have their content deleted
		return nil, errors.New("originDoc must not be garbage collected")
	}

	ds, sv := snapshot.deleteSet, snapshot.stateVector
	encoder := newUpdateEncoderV1()
	var writeErr error
	originDoc.Transact(func(trans *Transaction) {
		// The state-update count written to the header MUST equal the number of
		// client blocks actually written below, or the resulting update is malformed
		// (the decoder reads `size` blocks). A snapshot's SV can name a client absent
		// from originDoc (e.g. a crafted snapshot blob) — such a client has no
		// structs to restore. Rather than duplicate the eligibility predicate
		// (clock > 0 AND present in store) across a count loop and a write loop —
		// where the two could drift — build the eligible-client list ONCE; the header
		// count is then len(list) and the write loop iterates the same list, so
		// count == blocks is true by construction (F#4). Yjs-faithful: a real
		// snapshot is built from a live doc, so every SV client exists.
		type eligible struct {
			client  Number
			clock   Number
			structs *clientStructList
		}
		eligibles := make([]eligible, 0, len(sv))
		for client, clock := range sv {
			if clock == 0 {
				continue
			}
			structs, ok := originDoc.store.clientStructs(client)
			if !ok {
				continue
			}
			eligibles = append(eligibles, eligible{client: client, clock: clock, structs: structs})
		}

		writeVarUint(encoder.rest, uint64(len(eligibles)))
		// splitting the structs before writing them to the encoder
		for _, e := range eligibles {
			client, clock, structs := e.client, e.clock, e.structs

			if clock < getState(originDoc.store, client) {
				// May splice the list in place; the captured list remains current.
				getItemCleanStart(trans, GenID(client, clock))
			}

			if err := structs.writeSnapshotPrefix(encoder, client, clock); err != nil {
				writeErr = fmt.Errorf("create doc from snapshot: write struct: %w", err)
				return
			}
		}
		// ds is the snapshot's delete set (length >= 1 ranges); encoding cannot
		// error here. Surface it rather than swallow it silently.
		if err := writeDeleteSet(encoder, ds); err != nil {
			writeErr = fmt.Errorf("create doc from snapshot: delete set encode failed: %w", err)
		}
	}, nil)

	if writeErr != nil {
		return nil, writeErr
	}

	if err := ApplyUpdate(newDoc, encoder.toBytes(), "snapshot"); err != nil {
		return nil, fmt.Errorf("create doc from snapshot: apply restored update: %w", err)
	}
	return newDoc, nil
}

// snapshotContainsUpdateWith reports whether a snapshot already includes everything an update
// carries, reading the update with the given decoder.
//
// Faithful to yjs snapshotContainsUpdateV2 (src/utils/Snapshot.js): every struct in the update must
// end at or before the snapshot's state for its client, AND merging the update's delete set into the
// snapshot's must leave the snapshot's delete set unchanged. The delete-set half is what a
// state-vector-only check would miss — an update carrying only a later DELETION advances no clock.
func snapshotContainsUpdateWith(snapshot *Snapshot, update []uint8,
	mkDecoder func([]byte) updateDecoder) (bool, error) {
	if snapshot == nil {
		return false, errors.New("snapshot contains update: nil snapshot")
	}
	updateDecoder := mkDecoder(update)
	lazyDecoder := newLazyStructReader(updateDecoder, false)

	for curr := lazyDecoder.curr; curr != nil; curr = lazyDecoder.nextStruct() {
		id := curr.getID()
		if snapshot.stateVector[id.Client] < id.Clock+curr.structLength() {
			return false, nil
		}
	}
	if err := lazyDecoder.decodeError(); err != nil {
		return false, fmt.Errorf("snapshot contains update: %w", err)
	}
	ds, err := readDeleteSet(updateDecoder)
	if err != nil {
		return false, fmt.Errorf("snapshot contains update: read delete set: %w", err)
	}
	merged := mergeDeleteSets([]*deleteSet{snapshot.deleteSet, ds})
	return equalDeleteSets(snapshot.deleteSet, merged), nil
}

// SnapshotContainsUpdate is snapshotContainsUpdateWith for a V1-encoded update. Named to match the
// reference's default-argument pair rather than exposing the decoder, which is how the rest of this
// package spells the same shape (ConvertUpdateFormatWith, ParseUpdateMetaWith, MergeUpdatesWith).
func SnapshotContainsUpdate(snapshot *Snapshot, update []uint8) (bool, error) {
	return snapshotContainsUpdateWith(snapshot, update,
		func(b []byte) updateDecoder { return newUpdateDecoderV1(b) })
}

// SnapshotContainsUpdateV2 is snapshotContainsUpdateWith for a V2-encoded update.
func SnapshotContainsUpdateV2(snapshot *Snapshot, update []uint8) (bool, error) {
	return snapshotContainsUpdateWith(snapshot, update,
		func(b []byte) updateDecoder { return newUpdateDecoderV2(b) })
}
