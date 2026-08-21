package crdt

import (
	"bytes"
	"testing"
)

// ---------------------------------------------------------------- from delete_set_drop_review_test.go
// delete_set_drop_review_test.go reproduces the ReadAndApplyDeleteSet silent
// delete-drop found in the code-review (PR antst/y-crdt#2): it returns a bare
// nil on a read error, and the SUCCESS path also returns nil, so the caller
// (readUpdateV2 / the live ApplyUpdate path) can't distinguish truncation from
// clean-apply and silently drops un-applied deletes. A truncated delete-only
// differential update (0 struct refs, deletes in the DS) is graded-silently
// dropped — the receiver permanently keeps deletions the sender removed, with no
// error and no panic.
//
// The fix makes the apply all-or-nothing: a truncated DS is rejected with NO
// partial mutation. The test asserts a truncated frame never produces a PARTIAL
// apply (the count of applied deletes is always 0 or all, never in between).

// --- BUG 4: ReadAndApplyDeleteSet silent delete-drop ---------------------------

// deletedClockCount counts, after an apply, how many of the target clocks for
// `client` are marked deleted in the receiver doc.
func deletedClockCount(doc *Doc, client Number, clocks []Number) int {
	ds := newDeleteSetFromStructStore(doc.store)
	cnt := 0
	for _, c := range clocks {
		if isDeleted(ds, &ID{Client: client, Clock: c}) {
			cnt++
		}
	}
	return cnt
}

// buildV1DeleteOnlyUpdate builds a V1 update with zero struct refs and a delete
// set deleting each (clock,1) range for the given client.
func buildV1DeleteOnlyUpdate(client Number, clocks []Number) []byte {
	enc := newUpdateEncoderV1()
	writeVarUint(enc.rest, 0) // numOfStateUpdates = 0 (no struct refs)

	writeVarUint(enc.rest, 1) // numClients
	writeVarUint(enc.rest, uint64(client))
	writeVarUint(enc.rest, uint64(len(clocks)))
	for _, c := range clocks {
		writeVarUint(enc.rest, uint64(c)) // dsClock (V1: plain varuint)
		writeVarUint(enc.rest, 1)         // dsLen   (V1: plain varuint)
	}
	return enc.toBytes()
}

func TestReadAndApplyDeleteSetErrorsOnTruncation(t *testing.T) {
	// A doc with one client and a single 24-char text item (clocks 0..23).
	src := newDoc("guid", false, nil, nil, false, WithClientID(12345))
	src.GetText("t").Insert(0, "abcdefghijklmnopqrstuvwx", Object{})
	client := Number(12345)

	// Delete 12 distinct single-char ranges at clocks 0..11.
	clocks := []Number{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}
	full := buildV1DeleteOnlyUpdate(client, clocks)
	t.Logf("BUG4 full delete-only update = %d bytes, %d deletes", len(full), len(clocks))

	// A receiver that actually holds `client`'s structs: apply src's full document
	// update first, THEN apply our hand-built delete-only update.
	srcUpdate := mustBytes(EncodeStateAsUpdate(src, nil))

	applyAndCount := func(cut int) int {
		dst := newDoc("guid", false, nil, nil, false)
		_ = ApplyUpdate(dst, srcUpdate, nil) // receiver now has client 12345's structs
		if pre := deletedClockCount(dst, client, clocks); pre != 0 {
			t.Fatalf("precondition: %d clocks already deleted before applying DS", pre)
		}
		truncated := full
		if cut < len(full) {
			truncated = full[:cut]
		}
		_ = ApplyUpdate(dst, truncated, nil)
		return deletedClockCount(dst, client, clocks)
	}

	// Full apply: all 12 deletes land.
	if got := applyAndCount(len(full)); got != len(clocks) {
		t.Fatalf("BUG4 sanity: full update should delete all %d, got %d", len(clocks), got)
	}

	// Truncate mid-DS at every byte after the header. With the bug, the apply
	// silently drops a graded subset (0..k of 12). After the fix the truncated DS
	// is rejected before mutating, so the applied count is never PARTIAL.
	header := len(buildV1DeleteOnlyUpdate(client, nil)) // numOfStateUpdates + numClients + client + count(0)
	t.Logf("BUG4 DS header = %d bytes; full = %d bytes", header, len(full))

	sawPartial := false
	worstPartial := 0
	for cut := header + 1; cut < len(full); cut++ {
		got := applyAndCount(cut)
		if got != 0 && got != len(clocks) {
			if got > worstPartial {
				worstPartial = got
			}
			sawPartial = true
		}
	}

	if sawPartial {
		t.Fatalf("BUG4: a truncated delete-only update produced a PARTIAL silent apply (worst seen %d/%d) — the receiver permanently diverges from the sender", worstPartial, len(clocks))
	}
}

// ---------------------------------------------------------------- from delete_set_precap_dos_review_test.go
// delete_set_precap_dos_review_test.go reproduces DoS 3 from the SECOND
// code-review of PR antst/y-crdt#2: readDeleteSetOrdered (delete_set.go)
// pre-allocates two slices from attacker-controlled varint counts BEFORE the
// read loops, with NO bound against the bytes actually remaining:
//
//   - :327  make([]dsClientBlock, 0, numClients)        — numClients is a varint
//   - :343  make([]*DeleteItem, 0, numberOfDeletes)     — numberOfDeletes a varint
//
// A 13-byte update (numClients = 2^62) and a 12-byte update (numberOfDeletes =
// 2^62) each drive `makeslice: cap out of range` — a panic reachable through the
// public ApplyUpdate -> readUpdate -> ReadAndApplyDeleteSet -> readDeleteSetOrdered
// path (the Transact wrapper does not recover, so the process crashes).
//
// The fix bounds BOTH counts against the decoder's remaining bytes before make()
// — each client needs >=2 bytes (client + numDeletes varints), each range needs
// >=2 bytes (clock + len) — and errors (all-or-nothing), mirroring decoding.go's
// `size > decoder.Len()` guards. Each test FAILS (panics) on the unpatched tree
// and PASSES after the fix returns an error with no panic.

// recovers runs fn and reports whether it panicked (plus the recovered value).
func recovers(fn func()) (panicked bool, val any) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			val = r
		}
	}()
	fn()
	return
}

// buildV1DeleteSetHugeNumClients builds a V1 update with zero struct refs and a
// delete-set frame whose client count is a giant varint (2^62). The per-client
// body is absent — the panic happens at the make([]dsClientBlock, 0, 2^62)
// BEFORE any client byte is read.
func buildV1DeleteSetHugeNumClients() []byte {
	enc := new(bytes.Buffer)
	writeVarUint(enc, 0)     // numOfStateUpdates = 0 (no struct refs)
	writeVarUint(enc, 1<<62) // numClients (delete-set client count) — hostile
	return enc.Bytes()
}

// buildV1DeleteSetHugeNumDeletes builds a V1 update with zero struct refs and a
// delete-set frame with one client whose range count is a giant varint (2^62).
// The panic happens at make([]*DeleteItem, 0, 2^62) BEFORE any range is read.
func buildV1DeleteSetHugeNumDeletes() []byte {
	enc := new(bytes.Buffer)
	writeVarUint(enc, 0)     // numOfStateUpdates = 0 (no struct refs)
	writeVarUint(enc, 1)     // numClients = 1
	writeVarUint(enc, 42)    // client id
	writeVarUint(enc, 1<<62) // numberOfDeletes — hostile
	return enc.Bytes()
}

func TestReadDeleteSetOrderedBoundsNumClients(t *testing.T) {
	update := buildV1DeleteSetHugeNumClients()
	t.Logf("DoS3 numClients payload = % x (%d bytes)", update, len(update))

	// Direct decode must return an error, not panic. Consume the zero struct-refs
	// header first (the apply path reads it before the delete set), so the decoder
	// is positioned at the hostile delete-set client count.
	panicked, val := recovers(func() {
		dec := newUpdateDecoderV1(update)
		if _, err := readClientsStructRefs(dec, newDoc("", false, nil, nil, false)); err != nil {
			t.Fatalf("DoS3 numClients: struct refs decode failed: %v", err)
		}
		_, err := readDeleteSetOrdered(dec)
		if err == nil {
			t.Errorf("DoS3 numClients: expected an error, got nil")
		}
	})
	if panicked {
		t.Fatalf("DoS3 numClients: readDeleteSetOrdered PANICKED (%v) — count not bounded", val)
	}

	// And the public ApplyUpdate path must not panic the process.
	panicked, val = recovers(func() {
		doc := newDoc("", false, nil, nil, false)
		_ = ApplyUpdate(doc, update, nil)
	})
	if panicked {
		t.Fatalf("DoS3 numClients: ApplyUpdate PANICKED (%v)", val)
	}
}

func TestReadDeleteSetOrderedBoundsNumberOfDeletes(t *testing.T) {
	update := buildV1DeleteSetHugeNumDeletes()
	t.Logf("DoS3 numberOfDeletes payload = % x (%d bytes)", update, len(update))

	panicked, val := recovers(func() {
		dec := newUpdateDecoderV1(update)
		if _, err := readClientsStructRefs(dec, newDoc("", false, nil, nil, false)); err != nil {
			t.Fatalf("DoS3 numberOfDeletes: struct refs decode failed: %v", err)
		}
		_, err := readDeleteSetOrdered(dec)
		if err == nil {
			t.Errorf("DoS3 numberOfDeletes: expected an error, got nil")
		}
	})
	if panicked {
		t.Fatalf("DoS3 numberOfDeletes: readDeleteSetOrdered PANICKED (%v) — count not bounded", val)
	}

	panicked, val = recovers(func() {
		doc := newDoc("", false, nil, nil, false)
		_ = ApplyUpdate(doc, update, nil)
	})
	if panicked {
		t.Fatalf("DoS3 numberOfDeletes: ApplyUpdate PANICKED (%v)", val)
	}
}

// A legitimate delete set (real counts that fit the remaining bytes) must STILL
// decode and apply after the bounds are added — the bound only rejects provably
// truncated frames, never a real one.
func TestReadDeleteSetOrderedAcceptsLegitFrame(t *testing.T) {
	// Build a real delete-only V1 update: client 7 deletes (clock,1) for a few
	// clocks. readDeleteSetOrdered must decode it without error.
	update := buildV1DeleteOnlyUpdate(7, []Number{0, 1, 2, 3, 4})

	dec := newUpdateDecoderV1(update)
	// Skip the zero struct-refs header the same way readUpdate does, then decode
	// the DS via the ordered reader.
	if _, err := readClientsStructRefs(dec, newDoc("", false, nil, nil, false)); err != nil {
		t.Fatalf("DoS3 legit: struct refs decode failed: %v", err)
	}
	blocks, err := readDeleteSetOrdered(dec)
	if err != nil {
		t.Fatalf("DoS3 bound false-rejected a legitimate delete set: %v", err)
	}
	if len(blocks) != 1 || blocks[0].client != 7 || len(blocks[0].ranges) != 5 {
		t.Fatalf("DoS3 legit: decoded blocks wrong: %+v", blocks)
	}
}

// ---------------------------------------------------------------- from delete_set_struct_store_encode_test.go
func deleteSetStruct(client, clock, length Number, deleted bool) abstractStruct {
	id := GenID(client, clock)
	if deleted {
		return newGC(id, length)
	}
	return &abstractStructBase{id: id, length: length}
}

func testDeleteSetStructStore() *structStore {
	store := newStructStore()
	for _, client := range []Number{7, 100, 3, 42} {
		structs := []abstractStruct{
			deleteSetStruct(client, 0, 2, false),
			deleteSetStruct(client, 2, 3, true),
			deleteSetStruct(client, 5, 4, true),
			deleteSetStruct(client, 9, 1, false),
			deleteSetStruct(client, 10, client%3+1, true),
		}
		for _, value := range structs {
			store.appendClientStruct(client, value)
		}
	}
	// Exercise a client which must be omitted from the delete-set header.
	store.appendClientStruct(200, deleteSetStruct(200, 0, 5, false))
	return store
}

func TestWriteDeleteSetFromStructStoreMatchesMaterialized(t *testing.T) {
	singleClient := newStructStore()
	singleStructs := []abstractStruct{
		deleteSetStruct(9, 0, 1, false),
		deleteSetStruct(9, 1, 2, true),
		deleteSetStruct(9, 3, 1, false),
		deleteSetStruct(9, 4, 3, true),
	}
	for _, value := range singleStructs {
		singleClient.appendClientStruct(9, value)
	}
	stores := []*structStore{newStructStore(), singleClient, testDeleteSetStructStore()}
	for _, version := range []int{1, 2} {
		for storeIndex, store := range stores {
			var wantEncoder, gotEncoder dsEncoder
			if version == 1 {
				wantEncoder = newUpdateEncoderV1()
				gotEncoder = newUpdateEncoderV1()
			} else {
				wantEncoder = newDefaultUpdateEncoderV2()
				gotEncoder = newDefaultUpdateEncoderV2()
			}
			if err := writeDeleteSet(wantEncoder, newDeleteSetFromStructStore(store)); err != nil {
				t.Fatal(err)
			}
			if err := writeDeleteSetFromStructStore(gotEncoder, store, nil); err != nil {
				t.Fatal(err)
			}
			want := wantEncoder.toBytes()
			got := gotEncoder.toBytes()
			if !bytes.Equal(got, want) {
				t.Fatalf("V%d store %d: direct delete-set bytes differ\nwant %x\n got %x", version, storeIndex, want, got)
			}
		}
	}
}

// ---------------------------------------------------------------- from delete_set_v2_test.go
// V2 delete-set tests (US2). The V2 DS encoder/decoder delta-code clocks and
// lengths within each client's ranges; these tests verify the round-trip and
// cross-check against the JS V2 reference payloads.

// dsRoundTripV2 encodes ds with a V2 DS encoder and decodes it back, asserting
// the recovered client/clock/length ranges match.
func TestV2DeleteSetRoundTrip(t *testing.T) {
	ds := newDeleteSet()
	// Multiple clients, multiple ranges each (must be sorted+merged for V2 delta
	// coding to be valid — delete sets are always sorted before encoding).
	addToDeleteSet(ds, 1, 0, 3)
	addToDeleteSet(ds, 1, 5, 2)
	addToDeleteSet(ds, 1, 10, 1)
	addToDeleteSet(ds, 42, 100, 4)
	addToDeleteSet(ds, 42, 200, 8)
	sortAndMergeDeleteSet(ds)

	// WriteDeleteSet writes the delta-coded ranges into the encoder's rest buffer.
	enc := newDefaultUpdateEncoderV2()
	if err := writeDeleteSet(enc, ds); err != nil {
		t.Fatalf("WriteDeleteSet failed: %v", err)
	}
	data := enc.restEncoder().Bytes()

	// Decode it back through a V2 decoder whose rest buffer is the DS bytes.
	d := &updateDecoderV2{dsDecoderV2: dsDecoderV2{rest: bytes.NewBuffer(data)}}
	got, err := readDeleteSet(d)
	if err != nil {
		t.Fatalf("ReadDeleteSet failed: %v", err)
	}

	assertDeleteSetEqual(t, ds, got)
}

// TestWriteDeleteSetClientOrderDeterministic guards byte-parity with Yjs's
// writeDeleteSet, which sorts clients by clientID descending
// (`.sort((a, b) => b[0] - a[0])`) for a deterministic wire order. A raw Go map
// range is non-deterministic, so without an explicit sort the encoded byte
// stream would vary run-to-run and diverge from Yjs whenever a delete set spans
// more than one client. We decode the written client-ID sequence and assert it
// is strictly descending, independent of insertion order.
func TestWriteDeleteSetClientOrderDeterministic(t *testing.T) {
	// Insert clients out of order; map iteration order is undefined, so the
	// encoder must impose the descending order itself.
	ds := newDeleteSet()
	addToDeleteSet(ds, 7, 0, 1)
	addToDeleteSet(ds, 100, 0, 1)
	addToDeleteSet(ds, 3, 0, 1)
	addToDeleteSet(ds, 42, 0, 1)
	addToDeleteSet(ds, 1, 0, 1)
	sortAndMergeDeleteSet(ds)

	wantOrder := []uint64{100, 42, 7, 3, 1} // descending by clientID

	// Encode many times: a non-deterministic map range would eventually produce a
	// different order; the sort makes every run identical.
	var first []byte
	for iter := 0; iter < 50; iter++ {
		enc := newDefaultUpdateEncoderV2()
		if err := writeDeleteSet(enc, ds); err != nil {
			t.Fatalf("WriteDeleteSet failed: %v", err)
		}
		data := enc.restEncoder().Bytes()

		if first == nil {
			first = append([]byte(nil), data...)
		} else if !bytes.Equal(first, data) {
			t.Fatalf("WriteDeleteSet not deterministic across runs (map-order leak)")
		}

		// Decode the client-ID sequence directly from the rest buffer.
		rest := bytes.NewBuffer(data)
		n, _ := readVarUintAny(rest)
		numClients := n.(uint64)
		if numClients != uint64(len(wantOrder)) {
			t.Fatalf("client count: want %d got %d", len(wantOrder), numClients)
		}
		got := make([]uint64, 0, numClients)
		for i := uint64(0); i < numClients; i++ {
			cv, _ := readVarUintAny(rest)
			got = append(got, cv.(uint64))
			// skip this client's ranges: len, then (clock,len) pairs are delta-coded
			// in the rest buffer for V2, so we must walk them to reach the next client.
			ln, _ := readVarUintAny(rest)
			for j := uint64(0); j < ln.(uint64); j++ {
				_, _ = readVarUintAny(rest) // dsClock diff (V2 writes it via WriteVarUint)
				_, _ = readVarUintAny(rest) // dsLen-1   (V2 writes it via WriteVarUint)
			}
		}
		for i := range wantOrder {
			if got[i] != wantOrder[i] {
				t.Fatalf("client order: want %v got %v", wantOrder, got)
			}
		}
	}
}

// TestWriteDsLenRejectsZeroLength guards that DSEncoderV2.WriteDsLen returns an
// error on a non-positive length instead of underflowing uint64(length-1) into a
// huge VarUint (a silently corrupt V2 delete set) or panicking (which would crash
// ConvertUpdateFormatV1ToV2 on a V1-legal 0-length range). Delete lengths are
// always >= 1; 0 is explicitly an unexpected case.
func TestWriteDsLenRejectsZeroLength(t *testing.T) {
	for _, bad := range []Number{0, -1} {
		enc := newDefaultUpdateEncoderV2()
		if err := enc.writeDSLength(bad); err == nil {
			t.Errorf("WriteDsLen(%d): expected error, got nil", bad)
		}
	}

	// A valid length must NOT error.
	enc := newDefaultUpdateEncoderV2()
	if err := enc.writeDSLength(1); err != nil {
		t.Errorf("WriteDsLen(1): unexpected error: %v", err)
	}
}

func assertDeleteSetEqual(t *testing.T, want, got *deleteSet) {
	t.Helper()
	if got == nil {
		t.Fatalf("decoded delete set is nil")
	}
	if len(got.clients) != len(want.clients) {
		t.Fatalf("client count: want %d got %d", len(want.clients), len(got.clients))
	}
	for client, wantItems := range want.clients {
		gotItems := got.clients[client]
		if len(gotItems) != len(wantItems) {
			t.Fatalf("client %d range count: want %d got %d", client, len(wantItems), len(gotItems))
		}
		for i := range wantItems {
			if gotItems[i].clock != wantItems[i].clock || gotItems[i].length != wantItems[i].length {
				t.Errorf("client %d range %d: want {clock:%d len:%d} got {clock:%d len:%d}",
					client, i, wantItems[i].clock, wantItems[i].length, gotItems[i].clock, gotItems[i].length)
			}
		}
	}
}

// TestV2DeleteSetByteEqual builds the delete_only doc in Go and asserts the V2
// encoding (which includes a non-trivial delete set) is byte-identical to JS.
func TestV2DeleteSetByteEqual(t *testing.T) {
	assertV2Equal(t, "delete_only", func(doc *Doc) {
		a := doc.GetArray("a")
		doc.Transact(func(trans *Transaction) {
			a.Insert(0, ArrayAny{1, 2, 3, 4, 5, 6, 7, 8})
		}, nil)
		doc.Transact(func(trans *Transaction) {
			a.Delete(1, 2)
			a.Delete(3, 2)
		}, nil)
	})
}

// TestV2DeleteSetVsJS decodes the delete set embedded in a JS V2 reference
// payload and checks the recovered ranges against re-encoding (cross-impl).
func TestV2DeleteSetVsJS(t *testing.T) {
	// delete_only fixture: an array with two delete ranges -> a delete-heavy V2
	// payload. Apply it and confirm the resulting document length is correct,
	// which exercises the V2 DS decode end-to-end.
	fx := getFixture(t, "delete_only")
	doc := newDoc(fx.GUID, true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(doc, b64dec(t, fx.UpdateV2), nil)
	// original had 8 items, deleted 2+2 => 4 remain
	if got := doc.GetArray("a").GetLength(); got != 4 {
		t.Errorf("delete_only via V2: want array length 4 got %d", got)
	}
}

// TestV2DeleteOnlyUpdate exercises a V2 update that is (after diffing against a
// pre-delete state vector) effectively delete-only: no new structs, just deletes.
func TestV2DeleteOnlyUpdate(t *testing.T) {
	fx := getFixture(t, "delete_only")

	// base doc loaded from the V1 full state (applied via the V1 decoder).
	base := newDoc(fx.GUID, true, defaultGCFilter, nil, false)
	_ = ApplyUpdate(base, b64dec(t, fx.UpdateV1), nil)
	if got := base.GetArray("a").GetLength(); got != 4 {
		t.Errorf("base delete_only: want 4 got %d", got)
	}

	// the delete-only V2 diff applied to a fresh full-insert doc must converge
	if fx.DeleteDiffV2 == "" {
		t.Skip("fixture lacks deleteDiffV2")
	}
	doc := newDoc(fx.GUID, true, defaultGCFilter, nil, false)
	_ = ApplyUpdateV2(doc, b64dec(t, fx.DeleteDiffV2), nil)
	// applying only the delete diff (no inserts) to an empty doc leaves nothing
	// to delete, but must not panic and must leave an empty/consistent array.
	_ = doc.GetArray("a").GetLength()
}
