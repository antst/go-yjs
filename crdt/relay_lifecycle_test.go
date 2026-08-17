package crdt_test

import (
	"bytes"
	"testing"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/protocol"
)

// A whole relay lifecycle exercised from an EXTERNAL package, so it can only
// touch exported API: connect, catch-up, steady-state apply, persist to an
// append-only log, compact, and reload cold.
//
// WHY IT IS A TEST AND NOT A ONE-OFF. The privatization narrowed the public
// surface from 1,394 identifiers to 473, and the question that matters is not
// how small the surface is but whether a server can still be built on it. This
// fails if any step of that lifecycle stops being expressible from outside.
// EncodeStateVectorFromUpdate and ParseUpdateMeta were both privatized during
// that work and restored precisely because this flow needs them.
func TestRelayLifecycleFromOutside(t *testing.T) {
	// ---- storage stand-in: append-only update log per document
	var log [][]byte

	// ---- server holds a document
	server := crdt.NewDoc("room-1", crdt.WithGC(false))
	text := server.GetText("body")

	// capture every update the server produces, as a persistence layer would
	server.OnUpdate(func(update []byte, _ any) {
		log = append(log, append([]byte(nil), update...))
	})

	text.Insert(0, "hello", crdt.Object{})

	// ---- a client connects and syncs
	client := crdt.NewDoc("room-1", crdt.WithGC(false))
	step1 := protocol.EncodeSyncStep1(client)
	info, err := protocol.InspectMessage(step1)
	if err != nil {
		t.Fatalf("InspectMessage: %v", err)
	}
	if info.Type != protocol.MessageSync {
		t.Fatalf("step1 type = %d", info.Type)
	}

	handler := protocol.NewSyncHandler(server)
	var out bytes.Buffer
	if _, err := handler.HandleMessage(step1, &out); err != nil {
		t.Fatalf("HandleMessage(step1): %v", err)
	}

	// ---- steady state: client edits, server applies, server rebroadcasts
	clientText := client.GetText("body")
	clientText.Insert(0, "X", crdt.Object{})
	upd, err := crdt.EncodeStateAsUpdate(client, crdt.EncodeStateVector(server))
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if err := crdt.ApplyUpdate(server, upd, "client-1"); err != nil {
		t.Fatalf("ApplyUpdate: %v", err)
	}

	// ---- persistence: what does the stored log cover, WITHOUT rebuilding a doc?
	merged, err := crdt.MergeUpdates(log)
	if err != nil {
		t.Fatalf("MergeUpdates: %v", err)
	}
	sv, err := crdt.EncodeStateVectorFromUpdate(merged)
	if err != nil {
		t.Fatalf("EncodeStateVectorFromUpdate: %v", err)
	}
	if _, err := crdt.DecodeStateVector(sv); err != nil {
		t.Fatalf("DecodeStateVector: %v", err)
	}
	// A persistence layer uses this to decide what a stored blob covers without
	// building a document from it, which is why it must stay public.
	from, to, err := crdt.ParseUpdateMeta(merged)
	if err != nil {
		t.Fatalf("ParseUpdateMeta: %v", err)
	}
	if len(to) == 0 {
		t.Fatal("ParseUpdateMeta reported no end state for a non-empty log")
	}
	for client, endClock := range to {
		if endClock <= from[client] {
			t.Fatalf("client %d: end clock %d not past start %d", client, endClock, from[client])
		}
	}

	// ---- reload from storage into a cold document
	reloaded := crdt.NewDoc("room-1", crdt.WithGC(false))
	if err := crdt.ApplyUpdate(reloaded, merged, nil); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got, want := reloaded.GetText("body").ToString(), server.GetText("body").ToString(); got != want {
		t.Fatalf("reloaded = %q, server = %q", got, want)
	}
}
