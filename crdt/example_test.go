package crdt_test

import (
	"bytes"
	"fmt"

	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/protocol"
)

// Two replicas edit the same document concurrently and converge.
//
// This is the whole CRDT contract in one example: the edits are made
// independently, the updates cross in both directions, and both replicas end up
// with the same text regardless of the order they arrived in.
func Example_convergence() {
	// Client IDs are pinned so the example's output is stable. Real documents
	// should let the library pick one.
	first := crdt.NewDoc("notes", crdt.WithClientID(1))
	second := crdt.NewDoc("notes", crdt.WithClientID(2))

	first.GetText("body").Insert(0, "hello", crdt.Object{})
	second.GetText("body").Insert(0, "world", crdt.Object{})

	// Each side ships the operations the other has not seen. EncodeStateVector
	// says "here is what I have"; EncodeStateAsUpdate answers with the rest.
	firstUpdate, err := crdt.EncodeStateAsUpdate(first, crdt.EncodeStateVector(second))
	if err != nil {
		panic(err)
	}
	secondUpdate, err := crdt.EncodeStateAsUpdate(second, crdt.EncodeStateVector(first))
	if err != nil {
		panic(err)
	}

	if err := crdt.ApplyUpdate(first, secondUpdate, nil); err != nil {
		panic(err)
	}
	if err := crdt.ApplyUpdate(second, firstUpdate, nil); err != nil {
		panic(err)
	}

	// Both inserted at position 0 with no knowledge of each other. The CRDT
	// breaks that tie deterministically by client ID, so every replica resolves
	// it the same way — and the same way the JavaScript implementation does,
	// which the differential oracle checks on every push.
	fmt.Println(first.GetText("body").ToString())
	fmt.Println(second.GetText("body").ToString() == first.GetText("body").ToString())
	// Output:
	// helloworld
	// true
}

// OnUpdate is the seam a server hangs persistence and broadcast off: it delivers
// the exact bytes to store or forward, plus the origin of the transaction that
// produced them.
//
// The origin is what stops an echo loop. A server applies a remote client's
// update with that client as the origin, and its own OnUpdate handler then sees
// the origin and knows not to send those bytes back where they came from.
func Example_serverSeam() {
	doc := crdt.NewDoc("notes", crdt.WithClientID(1))

	doc.OnUpdate(func(update []byte, origin any) {
		// The byte count is deliberately not printed: it is an encoding detail,
		// and an example that asserts it would fail on any legitimate change to
		// the wire format.
		fmt.Println("bytes to persist:", len(update) > 0, "origin:", origin)
	})

	doc.Transact(func(*crdt.Transaction) {
		doc.GetText("body").Insert(0, "hi", crdt.Object{})
	}, "conn-7")

	// Output:
	// bytes to persist: true origin: conn-7
}

// The sync protocol as a transport adapter drives it: a client announces its
// state, the server answers with the difference, and the client applies it.
//
// SyncHandler owns the message framing. A transport adapter is responsible only
// for moving the byte slices between the two sides.
func Example_syncHandshake() {
	server := crdt.NewDoc("notes", crdt.WithClientID(1))
	server.GetText("body").Insert(0, "shared state", crdt.Object{})

	client := crdt.NewDoc("notes", crdt.WithClientID(2))

	// The client opens with step 1: "this is everything I already have."
	step1 := protocol.EncodeSyncStep1(client)

	// The server replies with step 2: only what the client is missing.
	var reply bytes.Buffer
	if _, err := protocol.NewSyncHandler(server).HandleMessage(step1, &reply); err != nil {
		panic(err)
	}

	// The client applies the reply through its own handler.
	var unused bytes.Buffer
	if _, err := protocol.NewSyncHandler(client).HandleMessage(reply.Bytes(), &unused); err != nil {
		panic(err)
	}

	fmt.Println(client.GetText("body").ToString())
	// Output:
	// shared state
}
