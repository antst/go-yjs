package crdt_test

import (
	"testing"

	"github.com/antst/go-yjs/crdt"
)

// The failure mode OnUpdate exists to remove: a plausible handler on the generic
// seam that silently stores nothing, with no error and a perfectly correct
// document. Awareness also emits "update", with an Object payload, so this is the
// assertion someone copies from that path.
func TestUpdateSeamCanSilentlyDropEverything(t *testing.T) {
	doc := crdt.NewDoc("room", crdt.WithGC(false))

	var viaGeneric [][]byte
	doc.On("update", crdt.NewObserverHandler(func(args ...interface{}) {
		if b, ok := args[0].([]uint8); ok {
			viaGeneric = append(viaGeneric, b)
		}
	}))

	var mistyped []crdt.Object
	doc.On("update", crdt.NewObserverHandler(func(args ...interface{}) {
		if o, ok := args[0].(crdt.Object); ok {
			mistyped = append(mistyped, o)
		}
	}))

	doc.GetText("t").Insert(0, "hello", crdt.Object{})

	if len(viaGeneric) == 0 {
		t.Fatal("correct handler stored nothing; probe is broken")
	}
	if len(mistyped) != 0 {
		t.Fatalf("mistyped handler stored %d", len(mistyped))
	}
}

// OnUpdate delivers the bytes and the origin without an assertion, and OffUpdate
// stops delivery. Origin is what a relay uses to avoid echoing an update back to
// the peer that sent it, so it has to survive the typed wrapper.
func TestOnUpdateDeliversBytesAndOrigin(t *testing.T) {
	doc := crdt.NewDoc("room", crdt.WithGC(false))

	type received struct {
		update []byte
		origin any
	}
	var got []received
	handler := doc.OnUpdate(func(update []byte, origin any) {
		got = append(got, received{update: append([]byte(nil), update...), origin: origin})
	})

	doc.GetText("t").Insert(0, "hello", crdt.Object{})
	if len(got) != 1 {
		t.Fatalf("OnUpdate delivered %d updates, want 1", len(got))
	}
	if len(got[0].update) == 0 {
		t.Fatal("OnUpdate delivered an empty update")
	}

	// An update carrying an explicit origin must preserve it.
	peer := crdt.NewDoc("room", crdt.WithGC(false))
	peer.GetText("t").Insert(0, "x", crdt.Object{})
	diff, err := crdt.EncodeStateAsUpdate(peer, crdt.EncodeStateVector(doc))
	if err != nil {
		t.Fatal(err)
	}
	if err := crdt.ApplyUpdate(doc, diff, "peer-7"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("after applying a remote update, delivered %d, want 2", len(got))
	}
	if got[1].origin != "peer-7" {
		t.Fatalf("origin = %v, want peer-7; a relay uses this to avoid echoing", got[1].origin)
	}

	doc.OffUpdate(handler)
	doc.GetText("t").Insert(0, "z", crdt.Object{})
	if len(got) != 2 {
		t.Fatalf("OffUpdate did not stop delivery: %d", len(got))
	}
}

// The V1 and V2 streams are separate subscriptions; a consumer picks one.
func TestOnUpdateV2IsASeparateStream(t *testing.T) {
	doc := crdt.NewDoc("room", crdt.WithGC(false))
	var v1, v2 int
	doc.OnUpdate(func([]byte, any) { v1++ })
	doc.OnUpdateV2(func([]byte, any) { v2++ })

	doc.GetText("t").Insert(0, "hello", crdt.Object{})
	if v1 != 1 || v2 != 1 {
		t.Fatalf("v1=%d v2=%d, want 1 and 1", v1, v2)
	}
}
