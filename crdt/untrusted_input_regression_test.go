package crdt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// untrusted_input_regression_test.go feeds the exact malformed byte sequences
// that previously panicked, crashed, or triggered an unbounded allocation in the
// V2 decode / apply path, and asserts each now returns an error (or fails
// gracefully) with no panic. One test per hardened untrusted-input finding.
//
// The shared theme: a hostile update must surface a decode error, never abort
// the process. assertNoPanic wraps a call and fails the test if it panics.

// uvarintBytes returns the VarUint encoding of v.
func uvarintBytes(v uint64) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], v)
	return tmp[:n]
}

// assertNoPanic runs fn and converts a panic into a test failure, so a
// regression that reintroduces the crash is caught instead of aborting the run.
func assertNoPanic(t *testing.T, name string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: panicked on malformed input (must return an error instead): %v", name, r)
		}
	}()
	fn()
}

// --- Finding 1: ContentDoc opts is not an Object -------------------------------

// A ContentDoc body is WriteString(guid) followed by WriteAny(opts). lib0
// any-encoding tag 119 is a string, so an opts payload of a string (not an
// object) made the old unchecked `any.(Object)` panic. It must now error.
func TestReadContentDocRejectsNonObjectOpts(t *testing.T) {
	buf := new(bytes.Buffer)
	_ = writeString(buf, "guid") // guid
	// any-encoded string ("x"): tag 119 then VarString.
	writeByte(buf, 119)
	_ = writeString(buf, "x")

	dec := newUpdateDecoderV1(buf.Bytes())
	assertNoPanic(t, "ReadContentDoc", func() {
		if _, err := readContentDoc(dec); err == nil {
			t.Fatalf("expected error for non-object ContentDoc opts, got nil")
		}
	})
}

// --- Finding 2: array length DoS ------------------------------------------------

// An any-encoded array (tag 117) declares a VarUint length. A huge length with
// no element bytes must be rejected before make([]any, size) so it cannot
// trigger an unbounded allocation.
func TestReadAnyRejectsOversizedArrayLength(t *testing.T) {
	buf := make([]byte, 0, 16)
	buf = append(buf, 117)                    // array tag
	buf = append(buf, uvarintBytes(1<<40)...) // 1 TiB declared length
	// no element bytes follow -> length far exceeds remaining buffer
	assertNoPanic(t, "ReadAny(array)", func() {
		if _, err := readAny(bytes.NewBuffer(buf)); err == nil {
			t.Fatalf("expected error for oversized array length, got nil")
		}
	})
}

// --- Finding 3: unbounded recursion --------------------------------------------

// Deeply nested any-encoded arrays (each: tag 117 + VarUint(1)) recurse one
// level per element. lib0/JS would overflow the Go stack (a fatal, unrecoverable
// error); the depth cap must turn this into a returned error well before that.
func TestReadAnyRejectsDeepArrayNesting(t *testing.T) {
	var buf []byte
	const depth = maxAnyDepth + 50
	for i := 0; i < depth; i++ {
		buf = append(buf, 117)                // array tag
		buf = append(buf, uvarintBytes(1)...) // length 1: one nested element
	}
	// innermost element: null (tag 126), so the chain terminates cleanly were it
	// not for the depth cap.
	buf = append(buf, 126)

	assertNoPanic(t, "ReadAny(deep array)", func() {
		if _, err := readAny(bytes.NewBuffer(buf)); err == nil {
			t.Fatalf("expected error for over-deep array nesting, got nil")
		}
	})
}

// Same, but for nested objects (tag 118 + VarUint(1) key/value count + a key
// string), which recurse through readObjectDepth.
func TestReadAnyRejectsDeepObjectNesting(t *testing.T) {
	buf := new(bytes.Buffer)
	const depth = maxAnyDepth + 50
	for i := 0; i < depth; i++ {
		writeByte(buf, 118)        // object tag
		buf.Write(uvarintBytes(1)) // one key/value pair
		_ = writeString(buf, "k")  // key (VarString)
		// value is the next nested object, written by the next loop iteration.
	}
	writeByte(buf, 126) // innermost value: null

	assertNoPanic(t, "ReadAny(deep object)", func() {
		if _, err := readAny(bytes.NewBuffer(buf.Bytes())); err == nil {
			t.Fatalf("expected error for over-deep object nesting, got nil")
		}
	})
}

// --- Finding 4: awareness state is not an Object -------------------------------

// An awareness update entry is VarUint(clientID), VarUint(clock), VarString(JSON
// state). A state payload that is valid JSON but not an object (here the JSON
// string "hi") made the old `state.(Object)` panic. Both apply variants must now
// return ErrMalformedAwarenessState.
func buildAwarenessUpdate(clientID uint64, stateJSON string) []byte {
	const clock = uint64(1)
	enc := newEncoder()
	writeVarUint(enc, 1) // one entry
	writeVarUint(enc, clientID)
	writeVarUint(enc, clock)
	_ = writeString(enc, stateJSON)
	return enc.Bytes()
}

func TestApplyAwarenessUpdateRejectsNonObjectState(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	aw := NewAwareness(doc)
	update := buildAwarenessUpdate(99, `"hi"`) // JSON string, not an object

	assertNoPanic(t, "ApplyAwarenessUpdate", func() {
		if err := ApplyAwarenessUpdate(aw, update, "remote"); !errors.Is(err, ErrMalformedAwarenessState) {
			t.Fatalf("expected ErrMalformedAwarenessState, got %v", err)
		}
	})
}

func TestApplyAwarenessUpdateWithoutEventsRejectsNonObjectState(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	aw := NewAwareness(doc)
	// A JSON array is also a non-object.
	update := buildAwarenessUpdate(99, `[1,2,3]`)

	assertNoPanic(t, "applyAwarenessUpdateWithoutEvents", func() {
		if err := applyAwarenessUpdateWithoutEvents(aw, update); !errors.Is(err, ErrMalformedAwarenessState) {
			t.Fatalf("expected ErrMalformedAwarenessState, got %v", err)
		}
	})
}

// A null (cleared) state and a real object must still be accepted (no error),
// guarding that the rejection does not over-trigger.
func TestApplyAwarenessUpdateAcceptsNullAndObject(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	aw := NewAwareness(doc)
	if err := ApplyAwarenessUpdate(aw, buildAwarenessUpdate(1, `{"n":1}`), "remote"); err != nil {
		t.Fatalf("object state must be accepted, got %v", err)
	}
	if err := ApplyAwarenessUpdate(aw, buildAwarenessUpdate(2, `null`), "remote"); err != nil {
		t.Fatalf("null state must be accepted, got %v", err)
	}
}

// --- Finding 4b: per-entry ReadString error swallowed on a truncated frame -----

// buildTruncatedAwarenessUpdate frames one entry whose state-string length prefix
// claims `claimedLen` bytes but supplies none, so the per-entry
// ReadString(decoder) hits "buffer is not enough" mid-frame. clientID/clock are
// well-formed so the decoder reaches the ReadString before failing.
func buildTruncatedAwarenessUpdate(clientID, clock, claimedLen uint64) []byte {
	enc := newEncoder()
	writeVarUint(enc, 1) // one entry
	writeVarUint(enc, clientID)
	writeVarUint(enc, clock)
	writeVarUint(enc, claimedLen) // VarString length prefix, but no payload bytes follow
	return enc.Bytes()
}

// A truncated awareness frame previously did `data, _ := ReadString(decoder)`,
// dropping the error: data came back empty, jsonObject(data) was nil, and the
// entry was misread as a CLEARED state — silently deleting/overwriting the
// existing state for that client on hostile input. The fix propagates the
// ReadString error, so the frame must now be rejected AND the pre-existing state
// must be left intact.
func TestApplyAwarenessUpdateRejectsTruncatedFrame(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	aw := NewAwareness(doc)

	// Seed a known good state for client 7 at clock 1.
	if err := ApplyAwarenessUpdate(aw, buildAwarenessUpdate(7, `{"name":"alice"}`), "remote"); err != nil {
		t.Fatalf("seeding state must succeed, got %v", err)
	}
	if got := aw.GetStates()[7]; got.IsNil() || got.GetOr("name") != "alice" {
		t.Fatalf("precondition: expected seeded state {name:alice}, got %v", got)
	}

	// A higher clock (2) so that, absent the truncation guard, the swallowed-error
	// path would treat the entry as cleared and DELETE client 7's state.
	truncated := buildTruncatedAwarenessUpdate(7, 2, 64) // claims 64 state bytes, supplies 0

	assertNoPanic(t, "ApplyAwarenessUpdate(truncated)", func() {
		if err := ApplyAwarenessUpdate(aw, truncated, "remote"); err == nil {
			t.Fatalf("expected a non-nil error for a truncated awareness frame, got nil")
		}
	})

	// The rejection must not have mutated or cleared the pre-existing state.
	if got := aw.GetStates()[7]; got.IsNil() || got.GetOr("name") != "alice" {
		t.Fatalf("truncated frame must not mutate/clear existing state; want {name:alice}, got %v", got)
	}
	if clk := aw.meta[7].GetOr("clock").(Number); clk != 1 {
		t.Fatalf("truncated frame must not advance the clock; want 1, got %d", clk)
	}
}

// applyAwarenessUpdateWithoutEvents shares the same per-entry ReadString discard; assert
// it likewise rejects a truncated frame without mutating state.
func TestApplyAwarenessUpdateWithoutEventsRejectsTruncatedFrame(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	aw := NewAwareness(doc)

	if err := applyAwarenessUpdateWithoutEvents(aw, buildAwarenessUpdate(7, `{"name":"alice"}`)); err != nil {
		t.Fatalf("seeding state must succeed, got %v", err)
	}
	truncated := buildTruncatedAwarenessUpdate(7, 2, 64)

	assertNoPanic(t, "applyAwarenessUpdateWithoutEvents(truncated)", func() {
		if err := applyAwarenessUpdateWithoutEvents(aw, truncated); err == nil {
			t.Fatalf("expected a non-nil error for a truncated awareness frame, got nil")
		}
	})

	if got := aw.GetStates()[7]; got.IsNil() || got.GetOr("name") != "alice" {
		t.Fatalf("truncated frame must not mutate/clear existing state; want {name:alice}, got %v", got)
	}
}

// --- Round-2 finding (/code-review): the awareness apply must be ALL-OR-NOTHING ---
//
// The single-entry truncation tests above only proved the FIRST entry is rejected
// before it mutates. The real hazard is a MULTI-entry frame: previously the
// per-entry loop mutated States/Meta for entries 0..N-1 in place, and a failure on
// entry N returned mid-loop — leaving entries 0..N-1 already applied (a clock
// advanced, blocking a same-clock re-send) AND skipping the post-loop Emit (a
// silent partial apply observers never hear about). The fix decodes + validates
// ALL entries first, so a malformed frame mutates NOTHING.

// buildMultiEntryTruncatedAwarenessUpdate frames TWO entries: entry 0 is a
// well-formed object state for clientA; entry 1 is clientB whose state-string
// length prefix claims `claimedLen` bytes but supplies none — so the decode fails
// only AFTER entry 0 has been fully (and validly) read. A non-all-or-nothing apply
// would have committed entry 0 by the time entry 1 fails.
func buildMultiEntryTruncatedAwarenessUpdate(clientA, clockA uint64, stateAJSON string, clientB, clockB, claimedLen uint64) []byte {
	enc := newEncoder()
	writeVarUint(enc, 2) // two entries
	// entry 0 — valid
	writeVarUint(enc, clientA)
	writeVarUint(enc, clockA)
	_ = writeString(enc, stateAJSON)
	// entry 1 — truncated state string
	writeVarUint(enc, clientB)
	writeVarUint(enc, clockB)
	writeVarUint(enc, claimedLen) // length prefix, no payload bytes follow
	return enc.Bytes()
}

func TestApplyAwarenessUpdateMultiEntryIsAllOrNothing(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	aw := NewAwareness(doc)

	const clientA = 11
	const clientB = 22

	// Pre-seed BOTH clients with known good state at clock 1.
	if err := ApplyAwarenessUpdate(aw, buildAwarenessUpdate(clientA, `{"name":"alice"}`), "remote"); err != nil {
		t.Fatalf("seed A failed: %v", err)
	}
	if err := ApplyAwarenessUpdate(aw, buildAwarenessUpdate(clientB, `{"name":"bob"}`), "remote"); err != nil {
		t.Fatalf("seed B failed: %v", err)
	}

	// A 2-entry frame: entry 0 would VALIDLY update clientA to clock 2 (name=mallory),
	// entry 1 is a truncated clientB at clock 2. Under a partial apply, clientA's
	// state+clock would already be mutated when entry 1 fails.
	frame := buildMultiEntryTruncatedAwarenessUpdate(clientA, 2, `{"name":"mallory"}`, clientB, 2, 64)

	assertNoPanic(t, "ApplyAwarenessUpdate(multi-entry truncated)", func() {
		err := ApplyAwarenessUpdate(aw, frame, "remote")
		if err == nil {
			t.Fatalf("expected non-nil error for a multi-entry truncated frame, got nil")
		}
		// Distinguishable truncation sentinel (NOT the non-object-state sentinel).
		if !errors.Is(err, ErrTruncatedAwarenessFrame) {
			t.Fatalf("want errors.Is ErrTruncatedAwarenessFrame, got %v", err)
		}
		if errors.Is(err, ErrMalformedAwarenessState) {
			t.Fatalf("truncation must be distinguishable from ErrMalformedAwarenessState; got %v", err)
		}
	})

	// ALL-OR-NOTHING: entry 0 (clientA) must be UNTOUCHED — state still alice, clock
	// still 1 — proving the valid leading entry was NOT committed before the failure.
	if got := aw.GetStates()[clientA]; got.IsNil() || got.GetOr("name") != "alice" {
		t.Fatalf("clientA must be untouched (all-or-nothing); want {name:alice}, got %v", got)
	}
	if clk := aw.meta[clientA].GetOr("clock").(Number); clk != 1 {
		t.Fatalf("clientA clock must not advance (all-or-nothing); want 1, got %d", clk)
	}
	// And the pre-seeded clientB survives intact too.
	if got := aw.GetStates()[clientB]; got.IsNil() || got.GetOr("name") != "bob" {
		t.Fatalf("pre-seeded clientB must survive; want {name:bob}, got %v", got)
	}
	if clk := aw.meta[clientB].GetOr("clock").(Number); clk != 1 {
		t.Fatalf("clientB clock must not advance; want 1, got %d", clk)
	}
}

func TestApplyAwarenessUpdateWithoutEventsMultiEntryIsAllOrNothing(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	aw := NewAwareness(doc)

	const clientA = 33
	const clientB = 44

	if err := applyAwarenessUpdateWithoutEvents(aw, buildAwarenessUpdate(clientA, `{"name":"alice"}`)); err != nil {
		t.Fatalf("seed A failed: %v", err)
	}
	if err := applyAwarenessUpdateWithoutEvents(aw, buildAwarenessUpdate(clientB, `{"name":"bob"}`)); err != nil {
		t.Fatalf("seed B failed: %v", err)
	}

	frame := buildMultiEntryTruncatedAwarenessUpdate(clientA, 2, `{"name":"mallory"}`, clientB, 2, 64)

	assertNoPanic(t, "applyAwarenessUpdateWithoutEvents(multi-entry truncated)", func() {
		err := applyAwarenessUpdateWithoutEvents(aw, frame)
		if err == nil {
			t.Fatalf("expected non-nil error for a multi-entry truncated frame, got nil")
		}
		if !errors.Is(err, ErrTruncatedAwarenessFrame) {
			t.Fatalf("want errors.Is ErrTruncatedAwarenessFrame, got %v", err)
		}
	})

	if got := aw.GetStates()[clientA]; got.IsNil() || got.GetOr("name") != "alice" {
		t.Fatalf("clientA must be untouched (all-or-nothing); want {name:alice}, got %v", got)
	}
	if clk := aw.meta[clientA].GetOr("clock").(Number); clk != 1 {
		t.Fatalf("clientA clock must not advance (all-or-nothing); want 1, got %d", clk)
	}
	if got := aw.GetStates()[clientB]; got.IsNil() || got.GetOr("name") != "bob" {
		t.Fatalf("pre-seeded clientB must survive; want {name:bob}, got %v", got)
	}
}

// A multi-entry frame whose SECOND entry is a well-formed-but-non-object state
// must likewise mutate nothing, and surface ErrMalformedAwarenessState (distinct
// from the truncation sentinel) — proving both validation failure modes are
// all-or-nothing.
func TestApplyAwarenessUpdateMultiEntryNonObjectIsAllOrNothing(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	aw := NewAwareness(doc)

	const clientA = 55
	const clientB = 66
	if err := ApplyAwarenessUpdate(aw, buildAwarenessUpdate(clientA, `{"name":"alice"}`), "remote"); err != nil {
		t.Fatalf("seed A failed: %v", err)
	}

	// entry 0 valid (would bump clientA to clock 2); entry 1 is a JSON string, not an
	// object → ErrMalformedAwarenessState, and nothing must be committed.
	enc := newEncoder()
	writeVarUint(enc, 2)
	writeVarUint(enc, clientA)
	writeVarUint(enc, 2)
	_ = writeString(enc, `{"name":"mallory"}`)
	writeVarUint(enc, clientB)
	writeVarUint(enc, 1)
	_ = writeString(enc, `"not-an-object"`)

	assertNoPanic(t, "ApplyAwarenessUpdate(multi-entry non-object)", func() {
		err := ApplyAwarenessUpdate(aw, enc.Bytes(), "remote")
		if !errors.Is(err, ErrMalformedAwarenessState) {
			t.Fatalf("want errors.Is ErrMalformedAwarenessState, got %v", err)
		}
		if errors.Is(err, ErrTruncatedAwarenessFrame) {
			t.Fatalf("non-object must be distinguishable from truncation; got %v", err)
		}
	})

	if got := aw.GetStates()[clientA]; got.IsNil() || got.GetOr("name") != "alice" {
		t.Fatalf("clientA must be untouched (all-or-nothing); want {name:alice}, got %v", got)
	}
	if clk := aw.meta[clientA].GetOr("clock").(Number); clk != 1 {
		t.Fatalf("clientA clock must not advance; want 1, got %d", clk)
	}
}

// --- Round-3 finding (/code-review): unguarded make() from an attacker-controlled
// leading entry-count ---
//
// The round-2 all-or-nothing refactor introduced decodeAwarenessEntries, which read
// the leading entry-count VarUint and then `make([]awarenessEntry, 0, length)` with
// NO bound. length is attacker-controlled (up to math.MaxUint64): a hostile frame
// carrying a huge leading count and no (or few) entry bytes drives `makeslice: cap
// out of range` (a panic) or an unbounded allocation (OOM) — unrecovered on the
// public ApplyAwarenessUpdate / applyAwarenessUpdateWithoutEvents API, i.e. a remote DoS.
// The fix bounds length against decoder.Len() BEFORE the make: every entry needs
// >=3 bytes, so a count exceeding the remaining bytes is provably malformed and is
// rejected as a truncated frame. These tests assert the huge-count frame returns an
// error with NO panic and NO unbounded alloc.

// buildHugeCountAwarenessUpdate frames a leading entry-count of `count` followed by
// `entryBytes` raw bytes (far fewer than 3*count). With the bound, this is rejected
// before any allocation.
func buildHugeCountAwarenessUpdate(count uint64, entryBytes []byte) []byte {
	out := append([]byte(nil), uvarintBytes(count)...)
	return append(out, entryBytes...)
}

func TestApplyAwarenessUpdateRejectsHugeEntryCount(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	aw := NewAwareness(doc)

	// Pre-seed one client so we can also confirm the rejected frame mutates nothing.
	const seeded = 7
	if err := ApplyAwarenessUpdate(aw, buildAwarenessUpdate(seeded, `{"name":"alice"}`), "remote"); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	// A leading count of math.MaxUint64 with zero entry bytes: make(..., MaxUint64)
	// would panic with "cap out of range" (or OOM) without the bound.
	frame := buildHugeCountAwarenessUpdate(math.MaxUint64, nil)

	assertNoPanic(t, "ApplyAwarenessUpdate(huge entry count)", func() {
		err := ApplyAwarenessUpdate(aw, frame, "remote")
		if err == nil {
			t.Fatalf("expected non-nil error for a huge-entry-count frame, got nil")
		}
		// Bounded count == truncated frame; distinct from the non-object sentinel.
		if !errors.Is(err, ErrTruncatedAwarenessFrame) {
			t.Fatalf("want errors.Is ErrTruncatedAwarenessFrame, got %v", err)
		}
		if errors.Is(err, ErrMalformedAwarenessState) {
			t.Fatalf("a bounded entry count must be distinguishable from ErrMalformedAwarenessState; got %v", err)
		}
	})

	// The rejected frame must mutate nothing.
	if got := aw.GetStates()[seeded]; got.IsNil() || got.GetOr("name") != "alice" {
		t.Fatalf("seeded client must be untouched; want {name:alice}, got %v", got)
	}
	if clk := aw.meta[seeded].GetOr("clock").(Number); clk != 1 {
		t.Fatalf("seeded client clock must not advance; want 1, got %d", clk)
	}

	// A merely-oversized count (a few entry bytes present, but count >> bytes/3) is
	// rejected by the same bound — exercising the boundary, not just MaxUint64.
	frame2 := buildHugeCountAwarenessUpdate(1<<20, []byte{0x01, 0x01, 0x00}) // claims 2^20 entries, supplies 3 bytes
	assertNoPanic(t, "ApplyAwarenessUpdate(oversized entry count)", func() {
		if err := ApplyAwarenessUpdate(aw, frame2, "remote"); !errors.Is(err, ErrTruncatedAwarenessFrame) {
			t.Fatalf("want errors.Is ErrTruncatedAwarenessFrame for an oversized count, got %v", err)
		}
	})
}

func TestApplyAwarenessUpdateWithoutEventsRejectsHugeEntryCount(t *testing.T) {
	doc := newDoc("g", true, defaultGCFilter, nil, false)
	aw := NewAwareness(doc)

	const seeded = 9
	if err := applyAwarenessUpdateWithoutEvents(aw, buildAwarenessUpdate(seeded, `{"name":"bob"}`)); err != nil {
		t.Fatalf("seed failed: %v", err)
	}

	frame := buildHugeCountAwarenessUpdate(math.MaxUint64, nil)

	assertNoPanic(t, "applyAwarenessUpdateWithoutEvents(huge entry count)", func() {
		err := applyAwarenessUpdateWithoutEvents(aw, frame)
		if err == nil {
			t.Fatalf("expected non-nil error for a huge-entry-count frame, got nil")
		}
		if !errors.Is(err, ErrTruncatedAwarenessFrame) {
			t.Fatalf("want errors.Is ErrTruncatedAwarenessFrame, got %v", err)
		}
	})

	if got := aw.GetStates()[seeded]; got.IsNil() || got.GetOr("name") != "bob" {
		t.Fatalf("seeded client must be untouched; want {name:bob}, got %v", got)
	}
	if clk := aw.meta[seeded].GetOr("clock").(Number); clk != 1 {
		t.Fatalf("seeded client clock must not advance; want 1, got %d", clk)
	}
}

// --- Finding 5: V1 0-length delete cannot be re-encoded as V2 ------------------

// DSEncoderV2.WriteDsLen stores (length-1); a 0-length range is malformed and
// previously panicked. It must now return an error, and the convert path must
// fail gracefully — returning an *error* (not a silent nil) — when a V1-legal
// 0-length delete is converted to V2.
func TestWriteDsLenV2RejectsZeroLengthDirect(t *testing.T) {
	enc := newDefaultUpdateEncoderV2()
	assertNoPanic(t, "WriteDsLen(0)", func() {
		if err := enc.writeDSLength(0); err == nil {
			t.Fatalf("expected error for 0-length V2 delete range, got nil")
		}
	})
}

func TestConvertV1ToV2WithZeroLengthDeleteFailsGracefully(t *testing.T) {
	// Build a minimal V1 update: zero structs, then a delete set with one client
	// carrying a single 0-length delete range. V1's WriteDsLen accepts 0 (raw
	// VarUint), so this is a legal V1 byte stream that a hostile peer can send.
	enc := newUpdateEncoderV1()
	rest := enc.restEncoder()
	writeVarUint(rest, 0) // numOfStateUpdates = 0 (no structs)
	// delete set: 1 client, clientID 1, 1 delete range, clock 0, len 0.
	writeVarUint(rest, 1)    // numClients
	writeVarUint(rest, 1)    // clientID
	writeVarUint(rest, 1)    // numberOfDeletes
	enc.writeDSClock(0)      // clock
	_ = enc.writeDSLength(0) // V1 length 0 (legal on the wire)
	v1 := enc.toBytes()

	assertNoPanic(t, "ConvertUpdateFormatV1ToV2(0-len delete)", func() {
		// The threading-completion change makes the unconvertible case return a real
		// error (not a silent nil a caller would read as "empty/success").
		out, err := ConvertUpdateFormatV1ToV2(v1)
		if err == nil {
			t.Fatalf("expected an error converting a 0-length delete to V2, got nil (out=%v)", out)
		}
		if out != nil {
			t.Fatalf("expected nil bytes on the failed conversion, got %v", out)
		}
	})
}

// TestConvertV1ToV2WithHugeClockDiffFailsGracefully feeds a V1 update whose
// Item origin clock exceeds the IntDiffOptRle encodable bound (>2^61, a legal V1
// VarUint). On V1->V2 convert that origin clock is written through the V2
// leftClock column codec (Item.Write -> WriteLeftID), whose diff-from-0 of >2^61
// overflows the diff*2 framing. The conversion must return an *error* (not
// panic, not a silent nil): the deferred clock-column encode error must surface
// through ConvertUpdateFormatV1ToV2.
func TestConvertV1ToV2WithHugeClockDiffFailsGracefully(t *testing.T) {
	// Minimal V1 update: 1 client, 1 Item (info = bit8|ContentDeleted) whose
	// origin/left ID carries a clock of 2^62. The GC/startClock path writes the
	// item's own clock as a plain VarUint (no column), so the *origin* clock — fed
	// to WriteLeftID -> leftClock IntDiffOptRle column — is what overflows.
	enc := newUpdateEncoderV1()
	rest := enc.restEncoder()
	writeVarUint(rest, 1)             // numOfStateUpdates = 1 client
	writeVarUint(rest, 1)             // numberOfStructs = 1
	enc.writeClient(7)                // clientID
	writeVarUint(rest, 0)             // startClock = 0 (item's own clock)
	writeByte(rest, bit8|1)           // info: bit8 (origin present) | ContentDeleted(1)
	writeVarUint(rest, 7)             // leftID.client
	writeVarUint(rest, uint64(1)<<62) // leftID.clock = 2^62 (> 2^61 bound)
	writeVarUint(rest, 1)             // ContentDeleted length = 1
	writeVarUint(rest, 0)             // delete set: 0 clients
	v1 := enc.toBytes()

	assertNoPanic(t, "ConvertUpdateFormatV1ToV2(huge clock diff)", func() {
		out, err := ConvertUpdateFormatV1ToV2(v1)
		if err == nil {
			t.Fatalf("expected an error converting a >2^61 clock diff to V2, got nil (out=%v)", out)
		}
		if out != nil {
			t.Fatalf("expected nil bytes on the failed conversion, got %v", out)
		}
	})
}

// --- Finding 6: overlong signed varint in a column -----------------------------

// readVarIntSigned must reject an overlong varint (one that would overflow the
// 64-bit magnitude / wrap the running multiplier) instead of silently producing
// a bogus value. Exercise it via UintOptRleDecoder.Read (a single value is a
// signed varint).
func TestUintOptRleRejectsOverlongVarint(t *testing.T) {
	// 11 continuation bytes (0x80 each) then a terminator: far longer than any
	// 64-bit magnitude can be.
	buf := bytes.Repeat([]byte{0x80}, 11)
	buf = append(buf, 0x01)
	d := newUintOptRLEDecoder(buf)
	assertNoPanic(t, "UintOptRleDecoder.Read(overlong)", func() {
		if _, err := d.readValue(); err == nil {
			t.Fatalf("expected error for overlong signed varint, got nil")
		}
	})
}

func TestIntDiffOptRleRejectsOverlongVarint(t *testing.T) {
	buf := bytes.Repeat([]byte{0x80}, 11)
	buf = append(buf, 0x01)
	d := newIntDiffOptRLEDecoder(buf)
	assertNoPanic(t, "IntDiffOptRleDecoder.Read(overlong)", func() {
		if _, err := d.readValue(); err == nil {
			t.Fatalf("expected error for overlong signed varint, got nil")
		}
	})
}

// --- Finding 7: string column run length overruns the concatenation ------------

// StringDecoder.Read slices the concatenated string by a per-string UTF-16
// length pulled from a UintOptRle sub-column. A length larger than the units
// available must error (validated in uint64 BEFORE the int narrowing, so a
// 32-bit-truncating value cannot slip past the bounds check).
func TestStringDecoderRejectsOverrunLength(t *testing.T) {
	// Column layout: VarString(concat) then the UintOptRle length sub-column.
	// concat = "ab" (2 UTF-16 units); declare a single string of length 5.
	col := new(bytes.Buffer)
	_ = writeString(col, "ab")
	lens := newDefaultUintOptRLEEncoder()
	lens.writeValue(5) // 5 > 2 available units
	writeUint8Array(col, lens.bytes())

	d := newStringDecoder(col.Bytes())
	assertNoPanic(t, "StringDecoder.Read(overrun)", func() {
		if _, err := d.readValue(); err == nil {
			t.Fatalf("expected error for string column overrun, got nil")
		}
	})
}

// A 32-bit-truncating length (2^32 + 1) must also be rejected: int(n) would
// truncate it to 1 on 64-bit Go and to a small value on 32-bit, but the uint64
// pre-check catches it regardless.
func TestStringDecoderRejectsTruncating32BitLength(t *testing.T) {
	col := new(bytes.Buffer)
	_ = writeString(col, "ab")
	lens := newDefaultUintOptRLEEncoder()
	lens.writeValue((1 << 32) + 1) // truncates to 1 via int() on 64-bit
	writeUint8Array(col, lens.bytes())

	d := newStringDecoder(col.Bytes())
	assertNoPanic(t, "StringDecoder.Read(2^32+1)", func() {
		if _, err := d.readValue(); err == nil {
			t.Fatalf("expected error for 32-bit-truncating string length, got nil")
		}
	})
}

// --- Finding 8: IntDiffOptRle clock accumulator overflow -----------------------

// IntDiffOptRleDecoder.Read accumulates d.s += d.diff. A large diff plus a long
// run can drive the int64 accumulator past MaxInt64; that overflow must be
// detected and reported instead of wrapping to a bogus negative clock.
func TestIntDiffOptRleRejectsAccumulatorOverflow(t *testing.T) {
	// Craft a column whose first value sets a diff near MaxInt64, with a run so
	// the second accumulation overflows. encodedDiff = diff*2 + hasCount; we need
	// diff large and hasCount = 1 (run follows), then a small run count.
	//
	// Use the encoder to produce a valid run of two identical large diffs, which
	// on decode accumulates 2*diff and overflows.
	enc := newDefaultIntDiffOptRLEEncoder()
	const big = int64(1) << 60 // within the encoder's encodable bound (2^61)
	mustWrite := func(v int64) {
		if err := enc.writeValue(v); err != nil {
			t.Fatalf("encoder write %d: %v", v, err)
		}
	}
	mustWrite(big)     // first value: diff = big
	mustWrite(2 * big) // second value: diff = big again -> run of 2
	mustWrite(3 * big) // third value: diff = big again -> run of 3
	// Decoding replays diffs: s = big, 2big, 3big ... but we want the running
	// accumulator to exceed MaxInt64. 3*big = 3*2^60 < 2^63 still. Extend the run
	// until it overflows.
	for k := int64(4); k <= 20; k++ {
		mustWrite(k * big) // keeps diff = big, extending the run
	}
	data, err := enc.bytes()
	if err != nil {
		t.Fatalf("encoder ToUint8Array: %v", err)
	}

	d := newIntDiffOptRLEDecoder(data)
	assertNoPanic(t, "IntDiffOptRleDecoder.Read(overflow)", func() {
		var lastErr error
		// Read until the accumulator overflows (it must, before exhausting the run).
		for i := 0; i < 64; i++ {
			if _, err := d.readValue(); err != nil {
				lastErr = err
				break
			}
		}
		if lastErr == nil {
			t.Fatalf("expected an int64 accumulator overflow error, got none")
		}
	})
}

// Sanity: the overflow guard must not reject a normal monotonic clock column.
func TestIntDiffOptRleAcceptsNormalClocks(t *testing.T) {
	enc := newDefaultIntDiffOptRLEEncoder()
	for c := int64(0); c < 100; c++ {
		if err := enc.writeValue(c); err != nil {
			t.Fatalf("write %d: %v", c, err)
		}
	}
	data, err := enc.bytes()
	if err != nil {
		t.Fatalf("ToUint8Array: %v", err)
	}
	d := newIntDiffOptRLEDecoder(data)
	for c := int64(0); c < 100; c++ {
		v, err := d.readValue()
		if err != nil {
			t.Fatalf("unexpected error reading normal clock %d: %v", c, err)
		}
		if v != c {
			t.Fatalf("clock mismatch: got %d want %d", v, c)
		}
	}
}

// --- Finding 9: readStateVector swallows truncation ----------------------------

// A state vector is VarUint(numClients) then numClients * (VarUint client,
// VarUint clock). Declaring more clients than the bytes provide previously made
// readStateVector silently return a short/zero map (the per-entry readVarUint
// errors were ignored), which a caller reads as a valid — but wrong — state.
// decodeStateVector must now surface the truncation as an error.
func TestDecodeStateVectorRejectsTruncated(t *testing.T) {
	// numClients = 3, but only one (client, clock) pair follows -> truncated.
	buf := new(bytes.Buffer)
	writeVarUint(buf, 3) // declared 3 clients
	writeVarUint(buf, 1) // client[0]
	writeVarUint(buf, 5) // clock[0]
	// client[1]/clock[1] and client[2]/clock[2] are missing.

	assertNoPanic(t, "decodeStateVector(truncated)", func() {
		sv, err := decodeStateVector(buf.Bytes())
		if err == nil {
			t.Fatalf("expected an error for a truncated state vector, got nil (sv=%v)", sv)
		}
		if sv != nil {
			t.Fatalf("expected nil map on truncation, got %v", sv)
		}
	})
}

// A well-formed state vector (including the empty [0] form) must still decode
// without error, guarding that the truncation check does not over-trigger.
func TestDecodeStateVectorAcceptsWellFormed(t *testing.T) {
	buf := new(bytes.Buffer)
	writeVarUint(buf, 2)
	writeVarUint(buf, 10)
	writeVarUint(buf, 1)
	writeVarUint(buf, 20)
	writeVarUint(buf, 2)
	sv, err := decodeStateVector(buf.Bytes())
	if err != nil {
		t.Fatalf("well-formed state vector errored: %v", err)
	}
	if sv[10] != 1 || sv[20] != 2 {
		t.Fatalf("decoded state vector wrong: %v", sv)
	}
	// the canonical empty form
	empty, err := decodeStateVector([]byte{0})
	if err != nil {
		t.Fatalf("empty state vector errored: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty state vector should decode to an empty map, got %v", empty)
	}
}

// guard against an unused import if math is only referenced conditionally above.
var _ = math.MaxInt64
