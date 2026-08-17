package protocol

import (
	"bytes"
	"testing"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/internal/lib0"
)

// awareness_empty_state_drift_review_test.go reproduces the protocol side of the
// awareness decode DRIFT found in the code-review (PR antst/y-crdt#2): an EMPTY
// state string ("") was rejected by DecodeAwarenessMessage (it parsed
// unconditionally → JSON EOF → ErrMalformedAwarenessState) while the core
// decodeAwarenessEntries treated "" as a cleared state. The frame that clears a
// cursor was thus accepted by core but rejected on the websocket → a GHOST
// cursor.
//
// After the fix both paths route through crdt.ParseAwarenessStateJSON, so "" is
// a clean cleared state on both. This test asserts DecodeAwarenessMessage accepts
// an empty-state frame without error and yields an empty (cleared) state.

// buildEmptyStateAwarenessPayload builds a one-entry awareness payload whose
// state is the empty string "".
func buildEmptyStateAwarenessPayload(clientID, clock uint64) []byte {
	enc := new(bytes.Buffer)
	lib0.WriteVarUint(enc, 1)
	lib0.WriteVarUint(enc, clientID)
	lib0.WriteVarUint(enc, clock)
	_ = lib0.WriteString(enc, "") // empty state string
	return enc.Bytes()
}

func TestDecodeAwarenessMessageEmptyStateIsCleared(t *testing.T) {
	payload := buildEmptyStateAwarenessPayload(123, 1)
	cid, state, err := DecodeAwarenessMessage(payload)
	if err != nil {
		t.Fatalf("drift: DecodeAwarenessMessage rejected an empty-state (cleared) frame: %v — this leaves a ghost cursor (core accepts the same frame)", err)
	}
	if cid != crdt.Number(123) {
		t.Fatalf("DecodeAwarenessMessage clientID = %d, want 123", cid)
	}
	if state.Len() != 0 {
		t.Fatalf("drift: DecodeAwarenessMessage empty-state frame yielded a non-empty state %v; want cleared", state)
	}
}

// A "null" state must also decode as cleared (parity with core), and a real
// object must round-trip — no false reject of legitimate frames.
func TestDecodeAwarenessMessageNullAndObjectParity(t *testing.T) {
	// null → cleared
	enc := new(bytes.Buffer)
	lib0.WriteVarUint(enc, 1)
	lib0.WriteVarUint(enc, 9)
	lib0.WriteVarUint(enc, 1)
	_ = lib0.WriteString(enc, "null")
	if _, state, err := DecodeAwarenessMessage(enc.Bytes()); err != nil || state.Len() != 0 {
		t.Fatalf("null state: got (%v, %v); want cleared, nil error", state, err)
	}

	// object → that object
	payload := EncodeAwarenessMessage(crdt.Number(6), crdt.MakeObject("n", float64(1)))
	// EncodeAwarenessMessage frames a single entry directly as the payload body.
	dec := bytes.NewBuffer(payload)
	_, state, err := DecodeAwarenessMessage(dec.Bytes())
	if err != nil {
		t.Fatalf("object state: DecodeAwarenessMessage errored: %v", err)
	}
	if state.Len() == 0 {
		t.Fatalf("object state: DecodeAwarenessMessage yielded an empty state; want the object")
	}
}
