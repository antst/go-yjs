package protocol

import (
	"bytes"
	"errors"
	"sync"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/internal/lib0"
)

// awarenessClocks tracks a per-client monotonically-increasing clock so that
// successive EncodeAwarenessMessage broadcasts for the same client are not
// dropped by the receiver's "newer clock wins" rule (ApplyAwarenessUpdate only
// applies an entry when currClock < clock). This mirrors the root Awareness
// type, which increments its per-client metadata clock on every SetLocalState
// (readable via Awareness.GetMeta()).
var (
	awarenessClockMu sync.Mutex
	awarenessClocks  = map[crdt.Number]uint64{}
)

// nextAwarenessClock returns the next monotonic clock for clientID. The first
// call yields 1 (not 0): ApplyAwarenessUpdate applies an entry only when
// currClock < clock, and a fresh receiver that has never seen this client
// defaults currClock to 0. Starting at 0 would make 0 < 0 false, so the very
// first broadcast would be silently dropped by every fresh receiver — defeating
// the helper's purpose. Starting at 1 guarantees the first broadcast is applied
// (0 < 1) while still incrementing monotonically thereafter.
func nextAwarenessClock(clientID crdt.Number) uint64 {
	awarenessClockMu.Lock()
	defer awarenessClockMu.Unlock()
	clock, seen := awarenessClocks[clientID]
	if seen {
		clock++
	} else {
		clock = 1
	}
	awarenessClocks[clientID] = clock
	return clock
}

// ResetAwarenessClock drops the cached broadcast clock for a single client.
// Long-running servers should call this when a client disconnects so the
// package-level clock map does not grow without bound as unique clientIDs come
// and go. After a reset the client's next EncodeAwarenessMessage broadcast
// starts again at clock 1 (still applied by fresh receivers).
func ResetAwarenessClock(clientID crdt.Number) {
	awarenessClockMu.Lock()
	defer awarenessClockMu.Unlock()
	delete(awarenessClocks, clientID)
}

// ResetAllAwarenessClocks clears every cached broadcast clock. Useful on handler
// teardown or test isolation to release the package-level map.
func ResetAllAwarenessClocks() {
	awarenessClockMu.Lock()
	defer awarenessClockMu.Unlock()
	awarenessClocks = map[crdt.Number]uint64{}
}

// awareness.go provides thin helpers around the root package's awareness wire
// format. The awareness update payload is, per client:
//
//	[clientCount][clientID][clock][jsonState] ...
//
// which is exactly what crdt.EncodeAwarenessUpdate / ApplyAwarenessUpdate use.

// ErrEmptyAwareness is returned when decoding an awareness payload with no
// client entries.
var ErrEmptyAwareness = errors.New("protocol: empty awareness update")

// ErrMalformedAwarenessState is returned when a client entry's state JSON parses
// successfully but is not a JSON object or null (awareness state must be an
// object, or null to clear it).
var ErrMalformedAwarenessState = errors.New("protocol: awareness state is not a JSON object")

// EncodeAwarenessMessage encodes a single-client awareness update payload (not
// framed) for broadcast. The clock is derived from a per-client monotonic
// counter (see nextAwarenessClock) so that each successive broadcast for the
// same client carries a strictly increasing clock — otherwise the receiver's
// newer-clock rule would silently drop every broadcast after the first. Callers
// holding a root Awareness should prefer crdt.EncodeAwarenessUpdate, whose
// clock comes from the root Awareness metadata (see Awareness.GetMeta()).
func EncodeAwarenessMessage(clientID crdt.Number, state crdt.Object) []byte {
	enc := new(bytes.Buffer)
	lib0.WriteVarUint(enc, 1) // one client
	lib0.WriteVarUint(enc, uint64(clientID))
	lib0.WriteVarUint(enc, nextAwarenessClock(clientID)) // monotonic clock
	// A cleared/removed state (the zero Object, IsNil) must serialize as the JSON
	// literal "null", not "{}" — otherwise receivers decode a present empty state
	// and never remove the client (ghost cursors). Use the single core boundary
	// (crdt.AwarenessStateJSON) shared with EncodeAwarenessUpdate /
	// ModifyAwarenessUpdate.
	_ = lib0.WriteString(enc, crdt.AwarenessStateJSON(state))
	return enc.Bytes()
}

// DecodeAwarenessMessage decodes the first client entry of an awareness update
// payload.
func DecodeAwarenessMessage(data []byte) (clientID crdt.Number, state crdt.Object, err error) {
	dec := bytes.NewBuffer(data)
	// Use the error-surfacing varUint reads so a truncated/malformed payload is
	// rejected rather than misread: ReadVarUint discards decode errors, so a
	// truncated count would look like an empty update (count=0) and a truncated
	// clientID/clock would silently yield 0.
	count, err := lib0.ReadVarUint(dec)
	if err != nil {
		return 0, crdt.Object{}, err
	}
	if count == 0 {
		return 0, crdt.Object{}, ErrEmptyAwareness
	}

	cid, err := lib0.ReadVarUint(dec)
	if err != nil {
		return 0, crdt.Object{}, err
	}
	clientID = crdt.Number(cid)

	if _, err = lib0.ReadVarUint(dec); err != nil { // clock
		return clientID, crdt.Object{}, err
	}

	js, err := lib0.ReadString(dec)
	if err != nil {
		return clientID, crdt.Object{}, err
	}

	// Classify the state JSON via the SHARED crdt.ParseAwarenessStateJSON — the
	// single source of truth used by both this path and the core
	// decodeAwarenessEntries — so an empty/null/object state is handled identically
	// on both sides. Previously this path parsed unconditionally, so an EMPTY state
	// string ("") EOF'd the JSON parser and was rejected as malformed, while the
	// core path treated "" as a cleared state — the drift left a GHOST cursor on the
	// websocket (the core cleared it, the protocol rejected the clearing frame).
	state, err = crdt.ParseAwarenessStateJSON(js)
	if err != nil {
		// The shared helper returns crdt.ErrMalformedAwarenessState for a valid-JSON
		// non-object; map it to THIS package's sentinel so callers' errors.Is checks
		// keep working. Any other error is an underlying JSON parse failure.
		if errors.Is(err, crdt.ErrMalformedAwarenessState) {
			return clientID, crdt.Object{}, ErrMalformedAwarenessState
		}
		return clientID, crdt.Object{}, err
	}
	return clientID, state, nil
}

// EncodeAwarenessUpdateMessage frames a full awareness update (as produced by
// crdt.EncodeAwarenessUpdate) as a MessageAwareness message.
func EncodeAwarenessUpdateMessage(payload []byte) []byte {
	out := new(bytes.Buffer)
	lib0.WriteVarUint(out, uint64(MessageAwareness))
	lib0.WriteVarUint8Array(out, payload)
	return out.Bytes()
}
