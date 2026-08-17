package crdt

import (
	"testing"
)

// struct_store_find_nilderef_review_test.go reproduces CRASH I#1 found in the
// code-review (PR antst/y-crdt#2): struct_store.go:108 Find(store, id) reads
// `ss := store.Clients[id.Client]` and immediately dereferences `*ss` at the
// FindIndexSS call. When the client is absent from the store the map read
// yields a nil *[]IAbstractStruct and the deref SIGSEGVs — unlike the sibling
// helpers GetItemCleanStart/GetItemCleanEnd/ReplaceStruct (:144/:160/:189)
// which all use the comma-ok form and return a "not found" error.
//
// REPRO payload (given): a malformed V1 update whose struct refs name a client
// that has no entry in the receiver store, so the integration path calls
// Find() with a missing client and crashes.
//
// The fix uses comma-ok and returns the existing "not found" error; the apply
// path then logs-and-skips rather than crashing the process.

func TestFindMissingClientErrorsNotCrash(t *testing.T) {
	// Recover so a SIGSEGV/panic is reported as a test failure, not a process
	// abort. (A nil-pointer deref surfaces as a runtime panic that recover()
	// catches in the same goroutine.)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CRASH I#1: ApplyUpdate panicked on a struct ref naming an absent client (Find nil-deref): %v", r)
		}
	}()

	payload := []byte{0x01, 0x30, 0x01, 0x00, 0x01, 0x30, 0x01, 0x30, 0x30}
	_ = ApplyUpdate(newDoc("g", false, nil, nil, false), payload, nil)
}

// Find() called directly with an absent client must return an error, never
// dereference a nil map value.
func TestFindDirectMissingClientReturnsError(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("CRASH I#1: Find() panicked on absent client instead of returning error: %v", r)
		}
	}()

	doc := newDoc("g", false, nil, nil, false)
	_, err := findStruct(doc.store, ID{Client: 999999, Clock: 0})
	if err == nil {
		t.Fatalf("CRASH I#1: Find() on an absent client should return a not-found error, got nil")
	}
}
