package crdt

import (
	"bytes"
	"testing"
)

// snapshot_malformed_regression_test.go covers the ReadDeleteSet nil-on-failure
// cluster (convergence-loop round 4). ReadDeleteSet used to return a bare nil on
// a truncated/malformed delete set; that nil propagated into Snapshot.Ds, so a
// hostile snapshot blob decoded "successfully" with Ds==nil and then panicked on
// the first content read (IsVisible/IsDeleted/IterateDeletedStructs/
// EqualSnapshots dereference Ds.Clients). DecodeSnapshot is public and takes
// arbitrary bytes, so this was a remote DoS.
//
// The root fix makes ReadDeleteSet return (DeleteSet, error) and threads the
// error through DecodeSnapshot. The belt-and-suspenders fix adds nil-Ds guards
// so even a directly-constructed Snapshot{Ds:nil} cannot panic a content read.

// buildSnapshotWithDeletes builds a doc with a real delete set and returns its
// encoded snapshot (a valid blob whose delete-set section is non-empty). This is
// the positive control: it must decode without error, proving the malformed-blob
// rejection below is triggered by the truncation, not by snapshot decoding being
// broken.
func buildSnapshotWithDeletes(t *testing.T) []uint8 {
	t.Helper()
	doc := newDoc("snap-g", true, defaultGCFilter, nil, false)
	a := doc.GetArray("a")
	doc.Transact(func(trans *Transaction) {
		a.Insert(0, ArrayAny{1, 2, 3, 4, 5})
	}, nil)
	doc.Transact(func(trans *Transaction) {
		a.Delete(1, 2) // produce a non-empty delete set
	}, nil)

	snap := NewSnapshotByDoc(doc)
	if len(snap.deleteSet.clients) == 0 {
		t.Fatalf("expected a non-empty delete set in the snapshot")
	}
	blob, err := EncodeSnapshot(snap)
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	return blob
}

// TestDecodeSnapshotValidRoundTrips is the positive control: a well-formed
// snapshot blob (with a real delete set) decodes without error and with a
// non-nil Ds.
func TestDecodeSnapshotValidRoundTrips(t *testing.T) {
	blob := buildSnapshotWithDeletes(t)

	snap, err := DecodeSnapshot(blob)
	if err != nil {
		t.Fatalf("DecodeSnapshot(valid): unexpected error: %v", err)
	}
	if snap == nil || snap.deleteSet == nil {
		t.Fatalf("DecodeSnapshot(valid): want non-nil snapshot with non-nil Ds, got %+v", snap)
	}
	if len(snap.deleteSet.clients) == 0 {
		t.Fatalf("DecodeSnapshot(valid): delete set lost in round-trip")
	}
}

// overlongVarUint is a 10-byte VarUint whose continuation bit is set on every
// byte — i.e. an 11th byte is promised but never supplied. binary.ReadUvarint
// (the reader behind ReadDsLen/ReadDsClock/readVarUint) consumes exactly these
// 10 bytes, detects 64-bit overflow, and returns ("varint overflows a 64-bit
// integer") WITHOUT draining the rest of the buffer. That "errors but leaves the
// trailing bytes intact" property is what makes the discriminating blob below
// actually discriminate (a plain EOF-truncated varint instead drains to empty,
// which is why the two header/EOF subcases are false-green — see the comment on
// discriminatingMidRangeTruncatedDS).
var overlongVarUint = []uint8{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80}

// discriminatingMidRangeTruncatedDS hand-builds a snapshot blob whose delete-set
// section is truncated MID-RANGE — a valid DS header (1 client, 1 delete range,
// a valid clock) followed by a `len` field that is an overlong/overflowing
// VarUint — and then appends WELL-FORMED state-vector bytes.
//
// This is the load-bearing input: it FAILS pre-fix and PASSES post-fix in a way
// the two existing subcases cannot.
//
//   - POST-FIX (ReadDeleteSet returns its error, DecodeSnapshotV2 propagates it):
//     reading the range `len` overflows -> ReadDeleteSet errors ->
//     DecodeSnapshotV2 returns (nil, error). Correct.
//
//   - PRE-FIX (`ds, _ := ReadDeleteSet(decoder)` — error discarded): the overlong
//     `len` still consumes its 10 bytes before overflowing, so on return the rest
//     buffer is positioned exactly at the appended SV bytes. readStateVector then
//     reads those bytes successfully and DecodeSnapshotV2 returns a NON-NIL
//     Snapshot with a NIL error — the silent-nil-DS DoS path. Wrong, and exactly
//     what this test must catch.
//
// Contrast the two pre-existing subcases ([varuint(1)] and valid[:1]): both leave
// the buffer EXHAUSTED right after the DS header, so readStateVector runs on an
// empty buffer and errors regardless of the fix — DecodeSnapshotV2 returns
// (nil, error) both pre- and post-fix, so those subcases do not guard the fix.
// Here the SV reader has real bytes to consume pre-fix, so only the propagated
// DS error distinguishes the two builds.
func discriminatingMidRangeTruncatedDS() []uint8 {
	blob := new(bytes.Buffer)
	// --- delete-set section, truncated mid-range ---
	writeVarUint(blob, 1)       // numClients = 1
	writeVarUint(blob, 7)       // client id = 7
	writeVarUint(blob, 1)       // range count = 1 (so we read into a range)
	writeVarUint(blob, 0)       // range[0] clock = 0 (valid)
	blob.Write(overlongVarUint) // range[0] len = overlong VarUint -> ReadDsLen overflows here
	// --- state-vector section, well-formed (the pre-fix discard path consumes this) ---
	writeVarUint(blob, 1)  // sv length = 1
	writeVarUint(blob, 42) // sv client = 42
	writeVarUint(blob, 5)  // sv clock = 5
	return blob.Bytes()
}

// TestDecodeSnapshotTruncatedDeleteSetErrors feeds snapshot blobs whose
// delete-set section is truncated and asserts DecodeSnapshot / DecodeSnapshotV2
// return a non-nil error (NOT a Snapshot{Ds:nil}). It also asserts no panic.
//
// The first two subcases are coarse truncations (they leave the buffer at EOF
// after the DS header). The third, discriminatingMidRangeTruncatedDeleteSet, is
// the load-bearing one: it errors pre-fix-vs-post-fix DIFFERENTLY (see
// discriminatingMidRangeTruncatedDS). Reverting DecodeSnapshotV2 to the old
// `ds, _ := ReadDeleteSet(decoder)` turns that subcase red while the first two
// stay green — that is the revert-to-red proof that this case guards the fix.
func TestDecodeSnapshotTruncatedDeleteSetErrors(t *testing.T) {
	// Case A: the delete-set header claims 1 client but no client bytes follow.
	// ReadDeleteSet must fail reading the client id.
	headerOnly := new(bytes.Buffer)
	writeVarUint(headerOnly, 1) // numClients = 1; nothing after -> truncated DS

	// Case B: a valid blob truncated mid-stream inside the delete-set section.
	valid := buildSnapshotWithDeletes(t)
	// The DS section is at the front (WriteDeleteSet runs before writeStateVector),
	// so chopping all but the first byte guarantees the cut lands inside the DS.
	midTruncated := append([]uint8(nil), valid[:1]...)

	cases := []struct {
		name string
		blob []uint8
	}{
		{"deleteSetHeaderClaimsClientWithNoBody", headerOnly.Bytes()},
		{"validBlobTruncatedInDeleteSet", midTruncated},
		// Load-bearing: DS truncated mid-range but trailed by valid SV bytes, so
		// only the PROPAGATED DS error (not incidental SV exhaustion) makes this
		// error. Pre-fix this decodes to a non-nil Snapshot{Ds:<partial>} with a
		// nil error; post-fix it errors.
		{"discriminatingMidRangeTruncatedDeleteSet", discriminatingMidRangeTruncatedDS()},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			assertNoPanic(t, "DecodeSnapshotV2", func() {
				snap, err := DecodeSnapshotV2(tc.blob)
				if err == nil {
					t.Fatalf("DecodeSnapshotV2(%s): expected a non-nil error, got snap=%+v", tc.name, snap)
				}
				if snap != nil {
					t.Fatalf("DecodeSnapshotV2(%s): expected nil snapshot on error, got %+v (a Snapshot{Ds:nil} is the bug)", tc.name, snap)
				}
			})
			assertNoPanic(t, "DecodeSnapshot", func() {
				snap, err := DecodeSnapshot(tc.blob)
				if err == nil {
					t.Fatalf("DecodeSnapshot(%s): expected a non-nil error, got snap=%+v", tc.name, snap)
				}
				if snap != nil {
					t.Fatalf("DecodeSnapshot(%s): expected nil snapshot on error, got %+v", tc.name, snap)
				}
			})
		})
	}
}

// TestReadDeleteSetErrorsOnMidRangeTruncation is the source-level contract test
// (approach B): it pins THE fix at its source — ReadDeleteSet itself must return
// a non-nil error on a delete set whose range `len` is a truncated/overlong
// VarUint — independent of any DecodeSnapshotV2 / readStateVector incidental
// behavior. Reverting ReadDeleteSet to return a bare nil on that read failure
// turns this red.
//
// It also asserts the discriminator's premise directly: the SV bytes appended
// after the malformed DS are themselves well-formed, so when ReadDeleteSet's
// error is (wrongly) discarded, readStateVector consumes them cleanly and yields
// a non-nil snapshot — i.e. the only thing standing between an attacker blob and
// a silent nil-Ds Snapshot is ReadDeleteSet returning (and DecodeSnapshotV2
// propagating) the error.
func TestReadDeleteSetErrorsOnMidRangeTruncation(t *testing.T) {
	blob := discriminatingMidRangeTruncatedDS()

	// (1) Source contract: ReadDeleteSet errors on the mid-range truncation.
	assertNoPanic(t, "ReadDeleteSet(mid-range truncated)", func() {
		ds, err := readDeleteSet(newUpdateDecoderV1(blob))
		if err == nil {
			t.Fatalf("ReadDeleteSet(mid-range truncated): expected a non-nil error, got ds=%+v (a bare nil is the silent-drop bug)", ds)
		}
	})

	// (2) Discriminator premise: feed ONLY the SV tail to readStateVector and
	// confirm it parses to the exact (client 42 -> clock 5) we appended. This is
	// why the pre-fix discard path produces a non-nil snapshot rather than an
	// error: the bytes after the malformed DS really are a valid state vector.
	svTail := new(bytes.Buffer)
	writeVarUint(svTail, 1)
	writeVarUint(svTail, 42)
	writeVarUint(svTail, 5)
	sv, err := readStateVector(newUpdateDecoderV1(svTail.Bytes()))
	if err != nil {
		t.Fatalf("readStateVector(appended SV tail): unexpected error: %v", err)
	}
	if got := sv[42]; got != 5 {
		t.Fatalf("readStateVector(appended SV tail): sv[42] = %d, want 5 (the discriminator's trailing SV must be well-formed)", got)
	}
}

// TestNilDsSnapshotContentReadsDoNotPanic is the belt-and-suspenders guard: even
// if a Snapshot is constructed directly with a nil Ds (or one slips through some
// other path), the content-read sinks that previously dereferenced Ds.Clients
// must treat it as empty and not panic.
func TestNilDsSnapshotContentReadsDoNotPanic(t *testing.T) {
	// A doc with a real item to read against.
	doc := newDoc("nilds-g", true, defaultGCFilter, nil, false)
	m := doc.GetMap("m")
	doc.Transact(func(trans *Transaction) {
		m.Set("k", "v")
		doc.GetArray("a").Insert(0, ArrayAny{"x"})
	}, nil)

	// Build a snapshot whose Ds is nil (mirrors the old DecodeSnapshot result on
	// attacker bytes) but whose Sv is populated so IsVisible reaches the IsDeleted
	// branch (state > clock).
	nilDsSnap := newSnapshot(nil, getStateVector(doc.store))

	// IsDeleted on a nil Ds must not panic.
	assertNoPanic(t, "IsDeleted(nil Ds)", func() {
		id := GenID(1, 0)
		_ = isDeleted(nilDsSnap.deleteSet, &id)
	})

	// IsVisible reaches IsDeleted(snapshot.Ds, ...) and must not panic.
	assertNoPanic(t, "IsVisible(nil Ds)", func() {
		doc.Transact(func(trans *Transaction) {
			for _, client := range doc.store.appendClientIDs(nil) {
				for _, s := range doc.store.structsForClient(client) {
					if item, ok := s.(*itemStruct); ok {
						_ = isVisible(item, nilDsSnap)
					}
				}
			}
		}, nil)
	})

	// IterateDeletedStructs over a nil Ds must not panic. This is the deletion-
	// iteration sink that SplitSnapshotAffectedStructs (reachable from
	// ToDelta/restore) funnels into; we exercise it directly because
	// SplitSnapshotAffectedStructs additionally trips an unrelated pre-existing
	// panic (it keys trans.Meta on a func value) that is out of scope here.
	assertNoPanic(t, "IterateDeletedStructs(nil Ds)", func() {
		doc.Transact(func(trans *Transaction) {
			iterateDeletedStructs(trans, nilDsSnap.deleteSet, func(s abstractStruct) {})
		}, nil)
	})

	// typeMapGetSnapshot is a public content read that funnels through IsVisible.
	assertNoPanic(t, "typeMapGetSnapshot(nil Ds)", func() {
		_ = typeMapGetSnapshot(m, "k", nilDsSnap)
	})

	// EqualSnapshots must tolerate a nil Ds on either side.
	assertNoPanic(t, "EqualSnapshots(nil Ds)", func() {
		full := NewSnapshotByDoc(doc)
		_ = EqualSnapshots(nilDsSnap, full)
		_ = EqualSnapshots(full, nilDsSnap)
		_ = EqualSnapshots(nilDsSnap, nilDsSnap)
	})
}

// encodeDeleteSetV1 returns the V1 wire bytes of ds (the per-user format
// permanentUserData stores in each user's "ds" array).
func encodeDeleteSetV1(t *testing.T, ds *deleteSet) []uint8 {
	t.Helper()
	enc := newUpdateEncoderV1()
	if err := writeDeleteSet(enc, ds); err != nil {
		t.Fatalf("WriteDeleteSet: %v", err)
	}
	return enc.toBytes()
}

// readPerUserDeleteSets mirrors permanentUserData.initUser's per-user DS read
// loop after the fix: each stored blob is decoded with ReadDeleteSet, and a
// malformed blob is skipped (logged) instead of appended as a silent nil. It
// returns the surviving sets (to merge) and how many entries were skipped.
func readPerUserDeleteSets(t *testing.T, blobs [][]uint8) (sets []*deleteSet, skipped int) {
	t.Helper()
	for _, data := range blobs {
		incoming, err := readDeleteSet(newUpdateDecoderV1(data))
		if err != nil {
			// This is the fixed behavior: error -> skip, never append a nil.
			skipped++
			continue
		}
		sets = append(sets, incoming)
	}
	return sets, skipped
}

// TestPermanentUserDataMalformedDeleteSetNotSilentlyDropped covers the
// permanentUserData per-user delete-set path (permanent_user_data.go:124,143). A
// user's stored delete sets are a mix of a valid encoded set and a
// malformed/truncated one.
//
// Before the fix, ReadDeleteSet returned a bare nil on the malformed blob;
// MergeDeleteSets skips nil entries, so the malformed entry vanished *silently* —
// indistinguishable from a valid empty set — and the failure was invisible. The
// fix makes ReadDeleteSet return an error so the malformed entry is explicitly
// logged+skipped, while the valid entry beside it survives the merge (its
// deletes are NOT dropped). This test reproduces initUser's read+merge loop and
// asserts exactly that.
func TestPermanentUserDataMalformedDeleteSetNotSilentlyDropped(t *testing.T) {
	// A valid delete set for client 7, clock 0, length 3.
	valid := newDeleteSet()
	addToDeleteSet(valid, 7, 0, 3)
	sortAndMergeDeleteSet(valid)
	validBlob := encodeDeleteSetV1(t, valid)

	// A malformed delete set: the header claims one client but no body follows.
	malformed := new(bytes.Buffer)
	writeVarUint(malformed, 1) // numClients = 1, then nothing -> truncated
	malformedBlob := malformed.Bytes()

	// First, the unit fact the fix rests on: ReadDeleteSet now ERRORS on the
	// malformed blob (it used to return (nil, <no error>)).
	if _, err := readDeleteSet(newUpdateDecoderV1(malformedBlob)); err == nil {
		t.Fatalf("ReadDeleteSet(malformed per-user blob): expected an error, got nil (the silent-drop bug)")
	}

	// Now the per-user read+merge loop, malformed entry beside the valid one.
	sets, skipped := readPerUserDeleteSets(t, [][]uint8{malformedBlob, validBlob})
	if skipped != 1 {
		t.Fatalf("expected exactly 1 malformed entry skipped, got %d", skipped)
	}
	if len(sets) != 1 {
		t.Fatalf("expected exactly 1 valid set to survive, got %d", len(sets))
	}

	merged := mergeDeleteSets(sets)
	if merged == nil || len(merged.clients[7]) == 0 {
		t.Fatalf("valid per-user delete dropped: merged set missing client 7 (%+v)", merged)
	}

	// And the surviving range still attributes the deleted ID. PermanentUserData
	// was removed with the unused object-graph API; this pins the delete-set
	// property that its former lookup method delegated to.
	id := GenID(7, 0)
	if !isDeleted(merged, &id) {
		t.Fatal("valid delete no longer attributes client 7 clock 0")
	}
}
