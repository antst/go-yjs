package crdt

import (
	"testing"
)

// state_vector_silent_truncation_review_test.go reproduces the silent-truncation
// correctness bug found in the code-review (PR antst/y-crdt#2):
// EncodeStateVectorFromUpdate* (updates.go) and ParseUpdateMeta* consumed the
// lazy struct reader but had NO error return. A cap-breach / decode error in the
// reader aborts the loop mid-update, so the functions returned a SILENTLY
// TRUNCATED state vector / meta — corrupting sync (WriteSyncStep1FromUpdate would
// advertise state the peer does not actually hold, so the peer skips structs it
// needs and the two diverge with no error).
//
// The fix threads reader.Err() out via a new error return on all four functions
// and WriteSyncStep1FromUpdate. This test feeds a malformed update that aborts
// the lazy reader (a struct ref whose item content is truncated) and asserts an
// error is returned rather than a partial result.

// buildTruncatedContentUpdate builds a V1 update declaring one client with one
// struct whose info byte requests item content the payload does not supply, so
// ReadItemContent fails inside the lazy reader.
func buildTruncatedContentUpdate() []byte {
	// numOfStateUpdates = 1 ; numberOfStructs = 1 ; client = 0 ; clock = 0
	// info byte 0x30 with no following content bytes -> ReadItemContent fails.
	return []byte{0x01, 0x01, 0x00, 0x00, 0x30}
}

func TestEncodeStateVectorFromUpdateErrorsOnTruncation(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("EncodeStateVectorFromUpdate panicked on a truncated update: %v", r)
		}
	}()

	bad := buildTruncatedContentUpdate()
	sv, err := encodeStateVectorFromUpdate(bad)
	if err == nil {
		t.Fatalf("silent truncation: EncodeStateVectorFromUpdate returned a (truncated) SV %v with no error on a malformed update", sv)
	}
	if sv != nil {
		t.Fatalf("silent truncation: EncodeStateVectorFromUpdate returned a non-nil SV (%v) alongside an error — must be all-or-nothing", sv)
	}
	t.Logf("EncodeStateVectorFromUpdate errored cleanly on truncation: %v", err)
}

func TestParseUpdateMetaErrorsOnTruncation(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ParseUpdateMeta panicked on a truncated update: %v", r)
		}
	}()

	bad := buildTruncatedContentUpdate()
	from, to, err := parseUpdateMeta(bad)
	if err == nil {
		t.Fatalf("silent truncation: ParseUpdateMeta returned (from=%v,to=%v) with no error on a malformed update", from, to)
	}
	if from != nil || to != nil {
		t.Fatalf("silent truncation: ParseUpdateMeta returned non-nil maps alongside an error — must be all-or-nothing")
	}
	t.Logf("ParseUpdateMeta errored cleanly on truncation: %v", err)
}

func TestWriteSyncStep1FromUpdateErrorsOnTruncation(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("WriteSyncStep1FromUpdate panicked on a truncated update: %v", r)
		}
	}()

	bad := buildTruncatedContentUpdate()
	enc := newUpdateEncoderV1()
	err := writeSyncStep1FromUpdate(enc, bad)
	if err == nil {
		t.Fatalf("silent truncation: WriteSyncStep1FromUpdate wrote a frame from a malformed update with no error — the peer would believe it is synced")
	}
	t.Logf("WriteSyncStep1FromUpdate errored cleanly on truncation: %v", err)
}

// A legitimate, well-formed update must still produce a state vector / meta with
// NO error (no false rejection).
func TestStateVectorAndMetaCleanOnValidUpdate(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(7))
	doc.GetText("t").Insert(0, "hello world", Object{})
	update := mustBytes(EncodeStateAsUpdate(doc, nil))

	if _, err := encodeStateVectorFromUpdate(update); err != nil {
		t.Fatalf("false reject: EncodeStateVectorFromUpdate errored on a valid update: %v", err)
	}
	if _, _, err := parseUpdateMeta(update); err != nil {
		t.Fatalf("false reject: ParseUpdateMeta errored on a valid update: %v", err)
	}
	enc := newUpdateEncoderV1()
	if err := writeSyncStep1FromUpdate(enc, update); err != nil {
		t.Fatalf("false reject: WriteSyncStep1FromUpdate errored on a valid update: %v", err)
	}
}
