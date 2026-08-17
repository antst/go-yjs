package crdt

import (
	"reflect"
	"testing"
)

// mapIdentity distinguishes "the same map" from "an equal map". Go forbids ==
// on maps, and every comparison below is about identity: the whole question is
// whether two conflict sets ended up sharing one backing map.
func mapIdentity(m map[*itemStruct]struct{}) uintptr {
	if m == nil {
		return 0
	}
	return reflect.ValueOf(m).Pointer()
}

// The update-local conflict maps must never be lent twice at once.
//
// WHY THIS TEST HAS TO ASK THE FUNCTION DIRECTLY. No input can produce a second
// borrower today: the window between lend and restore walks o = o.Right calling
// Add/Has/Reset, and the content integration that could re-enter runs after the
// maps go back. A 20,000-case both-direction gate run over 4.4M operations, with
// the scratch instrumented to panic on a double borrow, never tripped it. That is
// exactly why the guard needs a test of its own rather than coverage from the
// suite: an invariant that no ordinary input can violate is one that a future
// edit can quietly start violating, with nothing failing until conflict
// resolution corrupts under concurrency in a way that reads as a rare divergence.
//
// So this asserts the property at the only place it is decidable, and the teeth
// are real: delete the `s.borrowed` check from lend and the second borrower is
// handed the map the first is still reading, failing here.
func TestIntegrationScratchDeclinesToLendWhileBorrowed(t *testing.T) {
	items := make([]itemStruct, 12)
	pointers := make([]*itemStruct, len(items))
	for i := range items {
		pointers[i] = &items[i]
	}

	// A scratch carrying real maps, as it would be after the first Item of an
	// update. A nil-map scratch would make the sharing invisible.
	warmA, warmB := integrationItemSet{}, integrationItemSet{}
	for _, item := range pointers[:6] {
		warmA.Add(item)
		warmB.Add(item)
	}
	scratch := &integrationItemScratch{conflicting: warmA.Release(), before: warmB.Release()}
	if scratch.conflicting == nil || scratch.before == nil {
		t.Fatal("fixture failed to warm both scratch maps, so a shared map would not be observable")
	}
	lentConflicting := mapIdentity(scratch.conflicting)

	var outerConflicting, outerBefore integrationItemSet
	if !scratch.lend(&outerConflicting, &outerBefore) {
		t.Fatal("a fresh scratch refused to lend; the optimization is inert")
	}
	if mapIdentity(outerConflicting.reusable) != lentConflicting {
		t.Fatal("lend did not hand over the retained map")
	}

	// The re-entrant borrower. It must be refused and must not receive the maps.
	var innerConflicting, innerBefore integrationItemSet
	if scratch.lend(&innerConflicting, &innerBefore) {
		t.Fatal("scratch lent its maps to a second borrower while the first still held them; " +
			"both conflict sets would write into one map and resolve insertions against each " +
			"other's membership")
	}
	if innerConflicting.reusable != nil || innerBefore.reusable != nil {
		t.Fatal("a refused borrower still received the retained maps")
	}

	// Refused means it allocates its own, which is the pre-optimization behaviour:
	// correct, one allocation more expensive, and crucially not shared.
	for _, item := range pointers[:6] {
		innerConflicting.Add(item)
		outerConflicting.Add(item)
	}
	if innerConflicting.overflow == nil || outerConflicting.overflow == nil {
		t.Fatal("fixture did not promote both sets to map storage, so sharing cannot be detected")
	}
	if mapIdentity(innerConflicting.overflow) == mapIdentity(outerConflicting.overflow) {
		t.Fatal("the refused borrower promoted into the same map as the active one")
	}

	// Returning the maps must reopen lending, or the scratch is used exactly once
	// per update and the amortization silently stops after the first Item.
	scratch.restore(&outerConflicting, &outerBefore)
	if scratch.borrowed {
		t.Fatal("restore left the scratch marked as lent")
	}
	var nextConflicting, nextBefore integrationItemSet
	if !scratch.lend(&nextConflicting, &nextBefore) {
		t.Fatal("scratch refused to lend after its maps were returned; every later Item in the " +
			"update would allocate its own conflict maps")
	}
	if mapIdentity(nextConflicting.reusable) != lentConflicting {
		t.Fatal("the map returned by restore was not the one lent to the next borrower")
	}
}

// Local integration passes no scratch at all, and must keep working.
func TestIntegrationScratchNilLendsNothing(t *testing.T) {
	var scratch *integrationItemScratch
	var conflicting, before integrationItemSet
	if scratch.lend(&conflicting, &before) {
		t.Fatal("a nil scratch reported that it lent maps")
	}
	if conflicting.reusable != nil || before.reusable != nil {
		t.Fatal("a nil scratch handed out storage")
	}
}
