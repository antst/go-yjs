package backend_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/cluster"
	"github.com/antst/go-yjs/backend/hub"
	"github.com/antst/go-yjs/backend/internal/backendtest"
	"github.com/antst/go-yjs/backend/memory"
	"github.com/antst/go-yjs/backend/persistence"
	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/protocol"
)

// This is the defining backend acceptance test. It uses only exported CRDT and
// protocol APIs plus the four backend ports. The same lifecycle must remain
// expressible when the CRDT implementation moves out of the module root.
func TestBackendLifecycle(t *testing.T) {
	for _, clustered := range []bool{false, true} {
		name := "single-process-defaults"
		if clustered {
			name = "optional-cluster"
		}
		t.Run(name, func(t *testing.T) {
			runBackendLifecycle(t, clustered)
		})
	}
}

func runBackendLifecycle(t *testing.T, clustered bool) {
	t.Helper()
	ctx := context.Background()
	documentID := backend.DocumentID("room-1")
	store := backendtest.NewStore()
	if clustered {
		store = backendtest.NewFencedStore()
	}
	registry := memory.NewRegistry()
	fanout := hub.NewInProcess()
	defer func() { _ = fanout.Close() }()

	var (
		coordinator *backendtest.Coordinator
		lease       cluster.Lease
		fence       backend.Fence
	)
	if clustered {
		coordinator = backendtest.NewCoordinator()
		var err error
		lease, err = coordinator.Acquire(ctx, documentID, "node-a", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		fence = lease.Fence
	}

	handle, err := registry.Acquire(ctx, documentID, func(ctx context.Context) (*crdt.Doc, error) {
		return loadDocument(ctx, store, documentID)
	})
	if err != nil {
		t.Fatal(err)
	}
	server := handle.Doc()

	var (
		callbackErr  error
		lastRevision persistence.Revision
	)
	server.OnUpdate(func(update []byte, origin any) {
		if callbackErr != nil {
			return
		}
		lastRevision, callbackErr = store.Append(ctx, persistence.AppendRequest{
			DocumentID: documentID,
			Fence:      fence,
			Update:     update,
		})
		if callbackErr != nil {
			return
		}
		source, _ := origin.(backend.SourceID)
		callbackErr = fanout.Publish(ctx, hub.Message{
			DocumentID: documentID,
			SourceID:   source,
			Kind:       hub.DocumentUpdate,
			Payload:    update,
		})
	})

	server.GetText("body").Insert(0, "hello", crdt.Object{})
	if callbackErr != nil {
		t.Fatal(callbackErr)
	}

	// A browser connects through the public sync protocol and catches up.
	browserA := crdt.NewDoc("room-1", crdt.WithGC(false))
	syncDocs(t, server, browserA)
	if got := browserA.GetText("body").ToString(); got != "hello" {
		t.Fatalf("initial catch-up = %q, want hello", got)
	}
	browserB := crdt.NewDoc("room-1", crdt.WithGC(false))
	syncDocs(t, server, browserB)

	var echoed int
	subscriptionA, err := fanout.Subscribe(ctx, documentID, "browser-a", func(context.Context, hub.Message) error {
		echoed++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = subscriptionA.Close() }()
	subscriptionB, err := fanout.Subscribe(ctx, documentID, "browser-b", func(_ context.Context, message hub.Message) error {
		return crdt.ApplyUpdate(browserB, message.Payload, message.SourceID)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = subscriptionB.Close() }()

	stateBeforeX := crdt.EncodeStateVector(server)
	baselineUpdate, err := crdt.EncodeStateAsUpdate(server, nil)
	if err != nil {
		t.Fatal(err)
	}
	browserA.GetText("body").Insert(5, " X", crdt.Object{})
	updateX, err := crdt.EncodeStateAsUpdate(browserA, stateBeforeX)
	if err != nil {
		t.Fatal(err)
	}
	if err := crdt.ApplyUpdate(server, updateX, backend.SourceID("browser-a")); err != nil {
		t.Fatal(err)
	}
	if callbackErr != nil {
		t.Fatal(callbackErr)
	}
	if echoed != 0 {
		t.Fatalf("source received %d echoed updates", echoed)
	}
	if got, want := browserB.GetText("body").ToString(), server.GetText("body").ToString(); got != want {
		t.Fatalf("fan-out browser = %q, server = %q", got, want)
	}

	// A second dependent update gives the adversarial fan-out control a real
	// out-of-order case rather than two independent updates.
	stateBeforeY := crdt.EncodeStateVector(server)
	browserA.GetText("body").Insert(browserA.GetText("body").GetLength(), " Y", crdt.Object{})
	updateY, err := crdt.EncodeStateAsUpdate(browserA, stateBeforeY)
	if err != nil {
		t.Fatal(err)
	}
	if err := crdt.ApplyUpdate(server, updateY, backend.SourceID("browser-a")); err != nil {
		t.Fatal(err)
	}
	if callbackErr != nil {
		t.Fatal(callbackErr)
	}

	// The supported default is exercised with adversarial publication order and
	// duplication so the service cannot depend on its stronger synchronous
	// implementation behavior.
	adversarialDoc := crdt.NewDoc("room-1", crdt.WithGC(false))
	if err := crdt.ApplyUpdate(adversarialDoc, baselineUpdate, nil); err != nil {
		t.Fatal(err)
	}
	adversarialHub := hub.NewInProcess()
	defer func() { _ = adversarialHub.Close() }()
	_, err = adversarialHub.Subscribe(ctx, documentID, "reader", func(_ context.Context, message hub.Message) error {
		return crdt.ApplyUpdate(adversarialDoc, message.Payload, message.SourceID)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, update := range [][]byte{updateY, updateX, updateY} {
		if err := adversarialHub.Publish(ctx, hub.Message{DocumentID: documentID, SourceID: "writer", Kind: hub.DocumentUpdate, Payload: update}); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := adversarialDoc.GetText("body").ToString(), server.GetText("body").ToString(); got != want {
		t.Fatalf("adversarial fan-out = %q, server = %q", got, want)
	}

	// Build a checkpoint at the current durable revision, append one later
	// update, then compact. The later append must remain in the tail.
	basis := lastRevision
	checkpoint, err := crdt.EncodeStateAsUpdate(server, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpointStateVector := crdt.EncodeStateVector(server)
	browserA.GetText("body").Insert(browserA.GetText("body").GetLength(), " Z", crdt.Object{})
	updateZ, err := crdt.EncodeStateAsUpdate(browserA, checkpointStateVector)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: documentID, Fence: fence, Update: updateZ}); err != nil {
		t.Fatal(err)
	}
	if err := store.Compact(ctx, persistence.CompactRequest{
		DocumentID: documentID, Fence: fence, Basis: basis, CheckpointUpdate: checkpoint, StateVector: checkpointStateVector,
	}); err != nil {
		t.Fatal(err)
	}

	expected := crdt.NewDoc("room-1", crdt.WithGC(false))
	if err := crdt.ApplyUpdate(expected, checkpoint, nil); err != nil {
		t.Fatal(err)
	}
	if err := crdt.ApplyUpdate(expected, updateZ, nil); err != nil {
		t.Fatal(err)
	}

	handle.Release()
	if err := registry.Evict(documentID); err != nil {
		t.Fatal(err)
	}
	reloadedHandle, err := registry.Acquire(ctx, documentID, func(ctx context.Context) (*crdt.Doc, error) {
		return loadDocument(ctx, store, documentID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := reloadedHandle.Doc().GetText("body").ToString(), expected.GetText("body").ToString(); got != want {
		t.Fatalf("cold reload = %q, want %q", got, want)
	}
	reloadedHandle.Release()

	if clustered {
		coordinator.Advance(2 * time.Minute)
		newLease, err := coordinator.Acquire(ctx, documentID, "node-b", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: documentID, Fence: newLease.Fence, Update: updateZ}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: documentID, Fence: lease.Fence, Update: updateZ}); !errors.Is(err, persistence.ErrStaleFence) {
			t.Fatalf("stale owner append = %v, want ErrStaleFence", err)
		}
	}

	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
}

func loadDocument(ctx context.Context, store persistence.Loader, documentID backend.DocumentID) (*crdt.Doc, error) {
	doc := crdt.NewDoc(string(documentID), crdt.WithGC(false))
	var token persistence.PageToken
	for {
		page, err := store.Load(ctx, documentID, persistence.LoadOptions{PageToken: token, Limit: 2})
		if errors.Is(err, persistence.ErrNotFound) {
			return doc, nil
		}
		if err != nil {
			doc.Destroy()
			return nil, err
		}
		if page.Checkpoint != nil {
			if err := crdt.ApplyUpdate(doc, page.Checkpoint.Update, nil); err != nil {
				doc.Destroy()
				return nil, fmt.Errorf("apply checkpoint: %w", err)
			}
		}
		for _, record := range page.Updates {
			if err := crdt.ApplyUpdate(doc, record.Update, nil); err != nil {
				doc.Destroy()
				return nil, fmt.Errorf("apply revision %d: %w", record.Revision, err)
			}
		}
		if page.Next == "" {
			return doc, nil
		}
		token = page.Next
	}
}

func syncDocs(t *testing.T, server, client *crdt.Doc) {
	t.Helper()
	serverHandler := protocol.NewSyncHandler(server)
	clientHandler := protocol.NewSyncHandler(client)
	message := protocol.EncodeSyncStep1(client)
	queue := []struct {
		handler *protocol.SyncHandler
		message []byte
	}{{handler: serverHandler, message: message}}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		var reply bytes.Buffer
		if _, err := item.handler.HandleMessage(item.message, &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Len() == 0 {
			continue
		}
		next := clientHandler
		if item.handler == clientHandler {
			next = serverHandler
		}
		queue = append(queue, struct {
			handler *protocol.SyncHandler
			message []byte
		}{handler: next, message: append([]byte(nil), reply.Bytes()...)})
	}
}
