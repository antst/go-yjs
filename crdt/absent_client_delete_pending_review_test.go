package crdt

import (
	"testing"
)

// absent_client_delete_pending_review_test.go — 6th-review angle A regression.
//
// The N1/A#1 round added `structs, ok := store.clientStructs(client); if !ok {
// continue }` to ReadAndApplyDeleteSet, intending to crash-proof the `*structs`
// deref against an absent client. But `continue` DROPS the absent client's
// delete ranges entirely — and an absent client is the NORMAL CRDT case (a
// delete-only update applied before its structs arrive), not just a hostile one.
// Upstream Yjs reads `structs = store.clients.get(client) || []` and lets such a
// delete fall to the else branch, which queues it in unappliedDS as PENDING so it
// re-applies once the structs arrive. Dropping it => the receiver permanently
// keeps the range the sender deleted => PERMANENT peer divergence.
//
// This is an END-TO-END regression: doc A inserts text then deletes part of it;
// the STRUCTS update (u1) and the DELETE update (u2) are encoded separately; a
// fresh doc B applies u2 FIRST (client absent => the delete must be queued
// pending) THEN u1 (structs arrive => the pending delete must re-apply). The test
// asserts the deleted range IS deleted in B — i.e. the pending delete re-applied,
// not silently dropped.
func TestReview6_AbsentClientDelete_Pending(t *testing.T) {
	const cid = Number(7777)

	// Doc A: insert "abcdefghij" (clocks 0..9), one client.
	src := newDoc("guid", false, nil, nil, false, WithClientID(cid))
	src.GetText("t").Insert(0, "abcdefghij", Object{})

	// u1 = the STRUCTS update, captured BEFORE any delete (no delete set in it).
	u1 := mustBytes(EncodeStateAsUpdate(src, nil))

	// A now deletes the sub-range "cdef" — index 2, length 4 => clocks 2,3,4,5.
	src.GetText("t").Delete(2, 4)
	deletedClocks := []Number{2, 3, 4, 5}

	// Sanity: A really did delete exactly those clocks.
	if got := deletedClockCount(src, cid, deletedClocks); got != len(deletedClocks) {
		t.Fatalf("precondition: A should have %d clocks deleted, got %d", len(deletedClocks), got)
	}

	// u2 = a DELETE-only update for that range (zero struct refs + the delete set),
	// hand-built the same way as the BUG4 reproduction so the two updates are truly
	// separate frames.
	u2 := buildV1DeleteOnlyUpdate(cid, deletedClocks)

	// Fresh doc B: apply the DELETE update FIRST. Client cid is absent from B's
	// store at this point, so the delete cannot be applied yet — with the A
	// regression it is DROPPED; with the fix it is queued in PendingDs.
	dst := newDoc("guid", false, nil, nil, false)
	_ = ApplyUpdate(dst, u2, nil)

	// Nothing is deleted yet (the structs aren't even present).
	if got := deletedClockCount(dst, cid, deletedClocks); got != 0 {
		t.Fatalf("after u2 (client absent): expected 0 deleted in B, got %d", got)
	}

	// Now the STRUCTS arrive. Integrating them must trigger the pending-DS re-apply
	// (merge.go: every readUpdateV2 re-applies store.PendingDs), so the queued
	// delete lands.
	_ = ApplyUpdate(dst, u1, nil)

	// The text content must be present (the inserts integrated)...
	if got := dst.GetText("t").ToString(); len(got) == 0 {
		t.Fatalf("after u1: B's text is empty; structs did not integrate")
	}

	// ...and the deleted range must now be deleted in B — the pending delete
	// re-applied. If it is still 0, the absent-client delete was DROPPED (the A
	// regression) and B has permanently diverged from A.
	if got := deletedClockCount(dst, cid, deletedClocks); got != len(deletedClocks) {
		t.Fatalf("A regression: pending absent-client delete did NOT re-apply: %d/%d clocks deleted in B (the delete-only update applied before its structs was silently dropped => permanent divergence)", got, len(deletedClocks))
	}
}
