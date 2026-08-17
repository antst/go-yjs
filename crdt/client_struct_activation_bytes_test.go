package crdt

import "testing"

// A real document past the PRODUCTION activation threshold must encode exactly
// as a flat one does.
//
// The surrounding coverage stops short of this. TestProductionStructTreeActivates-
// OnlyAfterExactThreshold pins the boundary using synthetic structs at the list
// level; the forced-tree differential runs under the oracle build, whose limits
// are 8/4 rather than 8192/4096. Neither drives a genuine Y.Text document through
// the production policy and compares wire bytes, which is the property activation
// actually has to preserve: the representation is an implementation detail and
// must not be observable to a peer.
//
// The destination is the control. It receives everything through ApplyUpdate,
// which appends and therefore never trips the middle-insertion trigger, so it
// stays flat while the source is tree-backed. Re-encoding it compares a flat
// encoding against a tree-active one in the same build.
func TestProductionTreeActivationDoesNotChangeEncodedBytes(t *testing.T) {
	doc := newDoc("activation-bytes", false, defaultGCFilter, nil, false, WithClientID(1))
	txt := doc.GetText("t")
	for i := 0; i < 20000; i++ {
		txt.Insert(txt.Length(), "x", Object{})
	}
	rng := perfRand()
	for i := 0; i < 20000; i++ {
		txt.Insert(rng.intn(txt.Length()), "y", Object{})
	}

	list, ok := doc.store.clientStructs(1)
	if !ok {
		t.Fatal("no structs for client 1")
	}
	if list.tree.active() == nil {
		t.Fatalf("fixture built %d structs but the tree did not activate (threshold %d); "+
			"this test is only meaningful while active", list.Len(), clientStructTreeActivationLimit)
	}

	upd, err := EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	dst := newDoc("activation-bytes-dst", false, defaultGCFilter, nil, false, WithClientID(2))
	if err := ApplyUpdate(dst, upd, nil); err != nil {
		t.Fatal(err)
	}
	if got, want := dst.GetText("t").ToString(), txt.ToString(); got != want {
		t.Fatalf("tree-active document did not round-trip: %d chars vs %d", len(got), len(want))
	}

	// Never skip on the destination's state. Comparing tree against tree is still
	// a real byte check, and a skip here would let the whole test quietly compare
	// nothing if ApplyUpdate ever started activating.
	dstList, _ := dst.store.clientStructs(1)
	dstActive := dstList != nil && dstList.tree.active() != nil
	t.Logf("source tree-active=true, destination tree-active=%v", dstActive)
	back, err := EncodeStateAsUpdate(dst, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(upd) {
		t.Fatalf("re-encode is %d bytes, tree-active encode is %d (dst active=%v)",
			len(back), len(upd), dstActive)
	}
	for i := range back {
		if back[i] != upd[i] {
			t.Fatalf("encodings diverge at byte %d (dst active=%v)", i, dstActive)
		}
	}
}
