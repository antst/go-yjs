package crdt

// crash_repro_review_test.go — reviewer's OWN independent reproduction of the
// 4th-review fuzz-confirmed crash classes (antst/y-crdt#2). PRE-fix every case
// here panics (SIGSEGV / nil deref); POST-fix each public entry point must
// reject the malformed bytes cleanly (return/no-op) instead of crashing.
//
// This file is the verification backbone: it is NOT trusted from any agent — it
// is built and run by the reviewer both before the fix (to confirm the bugs are
// live and reproducible) and after (to confirm they are closed). It also serves
// as a regression guard.

import "testing"

// Reuses helpers from the committed review tests in this package:
//   mustHex(t, s)  — lazy_struct_reader_crash_review_test.go
//   recovers(fn)   — delete_set_precap_dos_review_test.go  (panicked bool, val any)

// K1 — eager readClientsStructRefs swallows the parent ReadLeftID() error
// (merge.go:353) → typed-nil *ID parent → GetMissing (item.go:90) deref.
func TestReview4_K1_ApplyUpdate_TypedNilParent(t *testing.T) {
	update := mustHex(t, "0102000000000100ffffffffffffffffffff00")
	doc := newDoc("k1", false, nil, nil, false)
	if panicked, val := recovers(func() { _ = ApplyUpdate(doc, update, nil) }); panicked {
		t.Fatalf("ApplyUpdate panicked on malformed update (eager typed-nil parent): %v", val)
	}
}

// K1 (network-facing instance) — same root reached via the public sync decode.
func TestReview4_K1_ReadSyncMessage_TypedNilParent(t *testing.T) {
	msg := mustHex(t, "02130102000000000100ffffffffffffffffffff00")
	doc := newDoc("k1b", false, nil, nil, false)
	if panicked, val := recovers(func() {
		readSyncMessageForTest(newDecoderV1(msg), newEncoderV1(), doc, nil)
	}); panicked {
		t.Fatalf("ReadSyncMessage panicked on malformed SyncStep2/Update: %v", val)
	}
}

// K2 — CreateDocFromSnapshot: snapshot SV names a client absent from originDoc
// → store.Clients[client] is nil → *structs deref (snapshot.go:175).
func TestReview4_K2_CreateDocFromSnapshot_AbsentClient(t *testing.T) {
	blob := mustHex(t, "00010101")
	snap, err := DecodeSnapshot(blob)
	if err != nil {
		t.Skipf("DecodeSnapshot rejected the blob up front (also acceptable): %v", err)
	}
	origin := newDoc("k2-origin", false, nil, nil, false)
	newDoc := newDoc("k2-new", false, nil, nil, false)
	if panicked, val := recovers(func() { _, _ = CreateDocFromSnapshot(origin, snap, newDoc) }); panicked {
		t.Fatalf("CreateDocFromSnapshot panicked on absent-client snapshot SV: %v", val)
	}
}

// K3 — negative-clock state vector (clock = 2^63 wraps to negative Number at
// merge.go:918) defeats the writeClientsStructs filter → WriteStructs derefs a
// nil slice for a client absent from the store (merge.go:171).
func TestReview4_K3_EncodeStateAsUpdate_NegativeClock(t *testing.T) {
	sv := mustHex(t, "010080808080808080808001")
	doc := newDoc("k3", false, nil, nil, false)
	if panicked, val := recovers(func() { _, _ = EncodeStateAsUpdate(doc, sv) }); panicked {
		t.Fatalf("EncodeStateAsUpdate panicked on negative-clock SV: %v", val)
	}
}

// --- 5th-review finding A#1 / fuzz I: delete-set apply path nil-deref ---
// The negative-wrap class minted at the decoder layer (ReadDsClock/ReadDsLen
// cast uint64->Number(int) unchecked, NOT covered by K3's readStateVector
// toNumber). A delete range with a clock varuint in [2^63,2^64) wraps NEGATIVE;
// for a client absent from the store GetState()==0, so `clock < state`
// (neg < 0) is TRUE and ReadAndApplyDeleteSet derefs nil *structs at
// delete_set.go:422. Network-reachable via ReadSyncMessage; ~15-byte trigger.
// Payloads from the angle-I fuzz (reproduced as non-recovered SIGSEGV).

func TestReview5_DeleteSet_NegClock_ApplyUpdate(t *testing.T) {
	update := mustHex(t, "000101018080808080808080800101")
	doc := newDoc("r5a", false, nil, nil, false)
	if panicked, val := recovers(func() { _ = ApplyUpdate(doc, update, nil) }); panicked {
		t.Fatalf("ApplyUpdate panicked on absent-client negative-DS-clock update: %v", val)
	}
}

func TestReview5_DeleteSet_NegClock_readSyncMessageForTest(t *testing.T) {
	msg := mustHex(t, "010f000101018080808080808080800101")
	doc := newDoc("r5b", false, nil, nil, false)
	if panicked, val := recovers(func() {
		readSyncMessageForTest(newDecoderV1(msg), newEncoderV1(), doc, nil)
	}); panicked {
		t.Fatalf("ReadSyncMessage panicked on absent-client negative-DS-clock update: %v", val)
	}
}

func TestReview5_DeleteSet_NegClock_ApplyUpdateV2(t *testing.T) {
	update := mustHex(t, "0000000000000100000000000101018080808080808080800100")
	doc := newDoc("r5c", false, nil, nil, false)
	if panicked, val := recovers(func() { _ = ApplyUpdateV2(doc, update, nil) }); panicked {
		t.Fatalf("ApplyUpdateV2 panicked on absent-client negative-DS-clock update: %v", val)
	}
}
