package crdt

import (
	"bytes"
	"testing"
)

// state_vector_framing_review_test.go reproduces the EncodeStateVectorFromUpdate
// empty-branch framing bug found in the code-review (PR antst/y-crdt#2): the
// empty-update branch returns encoder.ToUint8Array() directly; for the V2 variant
// `encoder` is a *UpdateEncoderV2, so it emits the 12-byte V2 columnar envelope
// instead of the 1-byte V1 state vector [0], violating the "state vector is
// always V1-encoded" contract. EncodeStateVectorFromUpdateV2 of an empty update
// must yield the V1 [0].

// --- BUG 5: EncodeStateVectorFromUpdate empty branch wrong framing -------------

func TestEncodeStateVectorFromUpdateV2EmptyIsV1(t *testing.T) {
	// An "empty" V2 update is one with no structs; EncodeStateAsUpdateV2 of a
	// fresh doc yields exactly that.
	empty := newDoc("", false, nil, nil, false)
	emptyV2 := mustBytes(EncodeStateAsUpdateV2(empty, nil))

	sv, err := encodeStateVectorFromUpdateV2(emptyV2)
	if err != nil {
		t.Fatalf("BUG5: EncodeStateVectorFromUpdateV2 on a valid empty update errored: %v", err)
	}
	t.Logf("BUG5 empty-V2 state vector = %v (len %d)", sv, len(sv))

	want := []byte{0}
	if !bytes.Equal(sv, want) {
		t.Fatalf("BUG5: empty V2 state vector = %v (len %d), want V1 %v (len 1)", sv, len(sv), want)
	}

	// And it must decode as a valid (empty) V1 state vector.
	m, err := decodeStateVector(sv)
	if err != nil {
		t.Fatalf("BUG5: empty V2 state vector does not decode as V1: %v", err)
	}
	if len(m) != 0 {
		t.Fatalf("BUG5: empty state vector decoded to %d entries, want 0", len(m))
	}
}

// The empty V1 path must be unaffected (regression guard for the shared branch).
func TestEncodeStateVectorFromUpdateV1EmptyIsV1(t *testing.T) {
	empty := newDoc("", false, nil, nil, false)
	emptyV1 := mustBytes(EncodeStateAsUpdate(empty, nil))

	sv, err := encodeStateVectorFromUpdate(emptyV1)
	if err != nil {
		t.Fatalf("EncodeStateVectorFromUpdate on a valid empty update errored: %v", err)
	}
	if !bytes.Equal(sv, []byte{0}) {
		t.Fatalf("empty V1 state vector = %v, want [0]", sv)
	}
}
