package crdt

// clockend_overflow_review_test.go — reviewer's OWN reproduction of the 8th-review
// angle-H finding: delete_set.go:432 `clockEnd := clock + length` is UNGUARDED. On
// the V1 delete-set path clock and length are clamped INDEPENDENTLY to [0,MaxInt]
// (no shared DsCurrVal like V2), so their sum wraps NEGATIVE; the negative clockEnd
// then makes `state < clockEnd` and `st.Clock < clockEnd` false → the delete is
// SILENTLY DROPPED → permanent CRDT divergence (no crash). Must error/handle after fix.

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestReviewH8_FindIndexDS_OverflowMembership — sibling of the clockEnd drop, in the
// delete-set MEMBERSHIP path: FindIndexDS computes `midClock + mid.Length` on a stored
// DeleteItem (clock and length clamped INDEPENDENTLY), so the sum can wrap negative and
// defeat the `clock < end` test → IsDeleted reports a deleted id as LIVE, diverging
// from Yjs. After the saturate it must report the id deleted.
func TestReviewH8_FindIndexDS_OverflowMembership(t *testing.T) {
	ds := newDeleteSet()
	client := Number(1)
	addToDeleteSet(ds, client, Number(math.MaxInt)-99, Number(math.MaxInt)) // clock+length overflows
	id := &ID{Client: client, Clock: Number(math.MaxInt) - 50}              // logically inside the range
	if !isDeleted(ds, id) {
		t.Fatalf("IsDeleted=false for an id inside an overflowing delete range: midClock+length wrapped negative, defeating the membership test (deleted struct treated as live; Yjs divergence)")
	}
}

func TestReviewH8_DeleteClockEndOverflow_RejectConvergesWithYjs(t *testing.T) {
	// doc with 10 chars, then delete the last char (clock 9, length 1).
	src := newDoc("ceA", false, nil, nil, false)
	src.GetText("t").Insert(0, "abcdefghij", Object{})
	src.GetText("t").Delete(9, 1) // delete "j" → clock 9, length 1
	full, err := EncodeStateAsUpdate(src, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// The V1 update ends with the delete set: ...[client][numDeletes][clock=9][length=1].
	// The final byte is the delete length (1 = 0x01). Rewrite it to MaxInt so
	// clockEnd = 9 + MaxInt overflows negative.
	if full[len(full)-1] != 0x01 {
		t.Skipf("last byte %#x is not the expected delete length 1; update=%x", full[len(full)-1], full)
	}
	var big [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(big[:], uint64(1)<<63-1) // MaxInt
	poison := append(append([]byte{}, full[:len(full)-1]...), big[:n]...)

	// Apply the (valid-structs + overflowed-delete) update to a fresh doc.
	dst := newDoc("ceB", false, nil, nil, false)
	if panicked, val := recovers(func() { _ = ApplyUpdate(dst, poison, nil) }); panicked {
		t.Fatalf("ApplyUpdate panicked on delete clockEnd overflow: %v", val)
	}
	// Control: the same update with the real length (1) deletes "j".
	ctrl := newDoc("ceC", false, nil, nil, false)
	_ = ApplyUpdate(ctrl, full, nil)
	ctrlStr := ctrl.GetText("t").ToString()
	poisonStr := dst.GetText("t").ToString()
	t.Logf("control(len=1) deletes → %q ; poison(len=MaxInt) → %q", ctrlStr, poisonStr)
	// Differential testing (yjs@13.6.31) shows upstream Yjs THROWS "Integer out of
	// Range" on the MaxInt-length range and does NOT apply the delete — so the Go
	// port must also NOT apply it: structs stay, the malformed delete is rejected,
	// and all 10 chars are KEPT, converging with a Yjs peer. The regressions this
	// guards against: (a) a NEGATIVE-clock wrap that silently drops the delete, and
	// (b) SATURATING to delete-to-end (poison would lose "j", DIVERGING from a Yjs
	// peer that keeps it). The control (len=1) is a legitimate delete and removes "j".
	if poisonStr != "abcdefghij" {
		t.Fatalf("overflowing delete clockEnd was APPLIED (poison=%q); upstream Yjs throws and keeps all 10 chars, so the Go port must REJECT the malformed range (not saturate/delete-to-end) to converge with Yjs", poisonStr)
	}
}

// --- 9th-review: MAX_SAFE_INTEGER read-boundary clamp -------------------------
//
// The maintainer's differential analysis vs real yjs@13.6.31 + lib0 v0.2.117 found
// that lib0's readVarUint THROWS "Integer out of Range" on a SINGLE decoded varint
// exceeding MAX_SAFE_INTEGER (2^53-1), while the Go port previously clamped each
// read at math.MaxInt (2^63-1) — so a single decoded clock/length/clientID in the
// window (2^53, 2^63) was ACCEPTED by Go but ABORTED the apply in Yjs: a peer
// divergence on adversarial input. The fix lowers the per-read clamp (toNumber /
// nonNegNumber) to maxSafeInteger so the Go port rejects exactly what Yjs throws on.
// These tests pin: (a) a single DS length in (2^53, 2^63) — 2^54 — is now REJECTED
// AT DECODE (ApplyUpdate logs + returns an error; decodeStateVector returns an error) where
// before it was accepted; (b) the exact boundary maxSafeInteger (2^53-1) is still
// ACCEPTED; (c) a valid doc (all values < 2^53) round-trips byte-for-byte unchanged.

// injectTrailingDSLen re-encodes a 10-char doc whose last operation is "delete the
// final char" (clock 9, length 1), then rewrites the trailing delete-LENGTH varint
// (the final byte, 0x01) to `dsLen`. Returns the poisoned update + the pristine one.
func injectTrailingDSLen(t *testing.T, guid string, dsLen uint64) (poison, full []byte) {
	t.Helper()
	src := newDoc(guid, false, nil, nil, false)
	src.GetText("t").Insert(0, "abcdefghij", Object{})
	src.GetText("t").Delete(9, 1) // delete "j" → DS range clock 9, length 1
	full, err := EncodeStateAsUpdate(src, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	// V1 update tail: ...[client][numDeletes][clock=9][length=1]; final byte is the
	// delete length 1 (0x01). Rewrite it to dsLen.
	if full[len(full)-1] != 0x01 {
		t.Skipf("last byte %#x is not the expected delete length 1; update=%x", full[len(full)-1], full)
	}
	var enc [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(enc[:], dsLen)
	poison = append(append([]byte{}, full[:len(full)-1]...), enc[:n]...)
	return poison, full
}

// TestReview9_DSLenAbove2p53_RejectedAtDecode — a single DS length of 2^54 (inside
// the (2^53, 2^63) window) is now rejected at DECODE: ReadDsLen → toNumber errors,
// readDeleteSetOrdered bails BEFORE any mutation, ReadAndApplyDeleteSet returns the
// error, and ApplyUpdate logs + returns it. Net effect: the structs are applied, the
// over-range delete is refused, all 10 chars are KEPT — converging with a Yjs peer
// (which throws "Integer out of Range" on the same varint). Before the 2^53 clamp,
// 2^54 < math.MaxInt was ACCEPTED by toNumber and the reject only fired (if at all)
// at the apply-site addClock — this test pins the per-read decode reject.
func TestReview9_DSLenAbove2p53_RejectedAtDecode(t *testing.T) {
	const twoP54 = uint64(1) << 54 // 2^54, in (2^53-1, 2^63-1)
	poison, _ := injectTrailingDSLen(t, "r9rejA", twoP54)

	var gotErr error
	dst := newDoc("r9rejB", false, nil, nil, false)
	panicked, val := recovers(func() {
		gotErr = ApplyUpdate(dst, poison, nil)
	})
	if panicked {
		t.Fatalf("ApplyUpdate panicked on a 2^54 DS length; it must return an error: %v", val)
	}
	if gotErr == nil {
		t.Fatalf("2^54 DS length was not surfaced as an ApplyUpdate error; decode rejection must not be silently swallowed")
	}
	if got := dst.GetText("t").ToString(); got != "abcdefghij" {
		t.Fatalf("2^54 DS length was APPLIED (text=%q); a single decoded value > 2^53 must be REJECTED at decode (toNumber) so the delete is refused and all 10 chars kept, matching the Yjs throw", got)
	}
}

// TestReview9_DSLenAtMaxSafeInteger_Accepted — the exact boundary: a DS length of
// maxSafeInteger (2^53-1) is at the lib0 limit (readVarUint accepts <= 2^53-1), so
// toNumber must ACCEPT it. The decode succeeds, clockEnd = addClock(9, 2^53-1) does
// NOT overflow (< MaxInt64), and the delete applies delete-to-end from clock 9 —
// removing the only struct at/after clock 9 ("j"), exactly like the pristine len=1
// delete. Proves the clamp is `> 2^53-1` (strict), not `>= 2^53-1` (off-by-one).
func TestReview9_DSLenAtMaxSafeInteger_Accepted(t *testing.T) {
	maxSafe := uint64(maxSafeInteger) // 2^53 - 1
	poison, full := injectTrailingDSLen(t, "r9accA", maxSafe)

	dst := newDoc("r9accB", false, nil, nil, false)
	if panicked, val := recovers(func() { _ = ApplyUpdate(dst, poison, nil) }); panicked {
		t.Fatalf("ApplyUpdate panicked on a DS length at maxSafeInteger (2^53-1); the boundary must be ACCEPTED + applied: %v", val)
	}
	ctrl := newDoc("r9accC", false, nil, nil, false)
	_ = ApplyUpdate(ctrl, full, nil)
	got, want := dst.GetText("t").ToString(), ctrl.GetText("t").ToString()
	if want != "abcdefghi" {
		t.Fatalf("control sanity: pristine len=1 delete should leave %q, got %q", "abcdefghi", want)
	}
	if got != want {
		t.Fatalf("DS length at maxSafeInteger (2^53-1) was not accepted+applied as delete-to-end (text=%q, want %q); the per-read clamp must accept the exact boundary 2^53-1", got, want)
	}
}

// TestReview9_StateVectorClockAbove2p53_Rejected — the read clamp is UNIFORM: the
// state-vector decode path (readStateVector → readVarUintAsNumber → toNumber) must
// also reject a single clock in (2^53, 2^63). decodeStateVector returns an error on
// a 2^54 clock and succeeds on 2^53-1 (boundary). This is the state-vector half of
// the "every single-value read rejects > 2^53 uniformly" guarantee.
func TestReview9_StateVectorClockAbove2p53_Rejected(t *testing.T) {
	// State vector wire: [numClients=1][client=42][clock]. Build it by hand.
	build := func(clock uint64) []byte {
		b := appendUvarint(nil, 1)  // numClients
		b = appendUvarint(b, 42)    // client id
		b = appendUvarint(b, clock) // clock
		return b
	}

	// 2^54 clock → rejected at decode.
	if _, err := decodeStateVector(build(uint64(1) << 54)); err == nil {
		t.Fatalf("decodeStateVector accepted a 2^54 clock; a single decoded SV clock > 2^53 must be REJECTED (toNumber) to match the lib0 readVarUint throw")
	}
	// Boundary 2^53-1 clock → accepted.
	sv, err := decodeStateVector(build(uint64(maxSafeInteger)))
	if err != nil {
		t.Fatalf("decodeStateVector rejected a clock at maxSafeInteger (2^53-1); the boundary must be accepted: %v", err)
	}
	if sv[42] != Number(maxSafeInteger) {
		t.Fatalf("SV clock at boundary decoded wrong: got %d want %d", sv[42], Number(maxSafeInteger))
	}
}

// TestReview9_ValidHighClockDoc_NoFalseReject — the KEY false-reject guard: a VALID
// doc with high-ish (but < 2^53) clocks, large text, many clients with random
// uint32 client ids, and a large delete set must round-trip through BOTH V1 and V2
// encoders unchanged. Real clocks/lengths are < 2^53 and client ids < 2^32, so the
// 2^53 read clamp must NOT reject any legitimate data.
func TestReview9_ValidHighClockDoc_NoFalseReject(t *testing.T) {
	src := newDoc("r9valid", false, nil, nil, false)
	// Many clients, each a random uint32 id, each inserting a chunk of text and
	// deleting part of it → a doc with multiple clients, real lengths, and a
	// non-trivial delete set. Clocks stay well under 2^53.
	for c := 0; c < 24; c++ {
		// Pin a random-ish uint32 client id (top bit set → large, but < 2^32 — the
		// realistic high end of the legit range) via WithClientID so the store is
		// built with it from the start.
		cid := Number(uint32(0x8000_0000) | uint32(c*2654435761))
		sub := newDoc("r9sub", false, nil, nil, false, WithClientID(cid))
		txt := sub.GetText("t")
		txt.Insert(0, "the quick brown fox jumps over the lazy dog 0123456789", Object{})
		txt.Delete(4, 5) // delete "quick"
		u, err := EncodeStateAsUpdate(sub, nil)
		if err != nil {
			t.Fatalf("sub encode (client %d): %v", c, err)
		}
		_ = ApplyUpdate(src, u, nil)
	}

	// Encode V1 + V2 and round-trip both into fresh docs; confirm no decode error
	// (no false-reject) and identical observable state.
	v1, err := EncodeStateAsUpdate(src, nil)
	if err != nil {
		t.Fatalf("V1 encode: %v", err)
	}
	v2, err := EncodeStateAsUpdateV2(src, nil)
	if err != nil {
		t.Fatalf("V2 encode: %v", err)
	}

	srcStr := src.GetText("t").ToString()
	srcSV, err := decodeStateVector(encodeStateVectorWith(src, nil, newUpdateEncoderV1()))
	if err != nil {
		t.Fatalf("encode/decode src state vector: %v", err)
	}

	rtV1 := newDoc("r9rtV1", false, nil, nil, false)
	if panicked, val := recovers(func() { _ = ApplyUpdate(rtV1, v1, nil) }); panicked {
		t.Fatalf("V1 round-trip panicked on a valid high-clock doc (false-reject/crash): %v", val)
	}
	rtV2 := newDoc("r9rtV2", false, nil, nil, false)
	if panicked, val := recovers(func() { _ = ApplyUpdateV2(rtV2, v2, nil) }); panicked {
		t.Fatalf("V2 round-trip panicked on a valid high-clock doc (false-reject/crash): %v", val)
	}

	if got := rtV1.GetText("t").ToString(); got != srcStr {
		t.Fatalf("V1 round-trip diverged (false-reject?): got %q want %q", got, srcStr)
	}
	if got := rtV2.GetText("t").ToString(); got != srcStr {
		t.Fatalf("V2 round-trip diverged (false-reject?): got %q want %q", got, srcStr)
	}
	// The decoded state vector must survive — every client/clock < 2^53 accepted.
	rtSV, err := decodeStateVector(encodeStateVectorWith(rtV1, nil, newUpdateEncoderV1()))
	if err != nil {
		t.Fatalf("round-trip state vector decode failed (false-reject): %v", err)
	}
	if len(rtSV) != len(srcSV) {
		t.Fatalf("round-trip state vector lost clients (false-reject): got %d want %d", len(rtSV), len(srcSV))
	}
	for client, clock := range srcSV {
		if rtSV[client] != clock {
			t.Fatalf("round-trip state vector clock mismatch for client %d: got %d want %d", client, rtSV[client], clock)
		}
	}
}
