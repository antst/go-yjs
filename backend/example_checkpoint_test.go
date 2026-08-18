package backend_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/conformance"
	"github.com/antst/go-yjs/backend/persistence"
	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/protocol"
)

// exampleBlobStore is the CheckpointStore an application supplies when its
// medium keeps one rewritable blob per document — an object-store key, a file
// whose id is a stable pointer, one row.
//
// It deliberately stores ONLY the update bytes, with no envelope and no stored
// state vector, because that is the constraint that makes this profile
// necessary: the blob is often a format another system also reads and writes,
// so nothing of ours may be wrapped around it. The state vector is derived on
// read, which the contract explicitly permits.
type exampleBlobStore struct {
	mutex     sync.Mutex
	blobs     map[backend.DocumentID][]byte
	revisions map[backend.DocumentID]persistence.Revision
	revision  persistence.Revision
	writes    int
}

func newExampleBlobStore() *exampleBlobStore {
	return &exampleBlobStore{
		blobs:     make(map[backend.DocumentID][]byte),
		revisions: make(map[backend.DocumentID]persistence.Revision),
	}
}

func (*exampleBlobStore) FenceMode() persistence.FenceMode { return persistence.Unfenced }

func (s *exampleBlobStore) SaveCheckpoint(ctx context.Context, request persistence.SaveCheckpointRequest) (persistence.Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if request.Fence != 0 {
		return 0, persistence.ErrUnexpectedFence
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	s.revision++
	// The request's slice is borrowed only for this call.
	s.blobs[request.DocumentID] = append([]byte(nil), request.Update...)
	s.revisions[request.DocumentID] = s.revision
	s.writes++
	return s.revision, nil
}

func (s *exampleBlobStore) LoadCheckpoint(ctx context.Context, id backend.DocumentID) (persistence.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return persistence.Checkpoint{}, err
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	blob, ok := s.blobs[id]
	if !ok {
		return persistence.Checkpoint{}, persistence.ErrNotFound
	}
	// Derived rather than stored: this medium has nowhere to put it.
	vector, err := crdt.EncodeStateVectorFromUpdate(blob)
	if err != nil {
		return persistence.Checkpoint{}, fmt.Errorf("%w: %s", persistence.ErrCorrupt, err)
	}
	return persistence.Checkpoint{
		Revision:    s.revisions[id],
		Update:      append([]byte(nil), blob...),
		StateVector: vector,
	}, nil
}

// The example's own store is held to the public suite, so the worked example
// cannot quietly demonstrate a non-conforming implementation.
func TestExampleBlobStoreConformance(t *testing.T) {
	conformance.CheckpointPersistence(t, func() persistence.CheckpointStore {
		return newExampleBlobStore()
	})
}

// Example_checkpointBackend drives the checkpoint profile through a real
// service flow: open a document, persist on every update, then recover it from
// storage alone in a fresh process-equivalent and check a second client
// converges on the recovered state.
//
// The conformance suite proves the interface is SATISFIABLE. This proves it is
// USABLE — that saving from the update seam, recovering cold, and feeding the
// stored bytes back through ApplyUpdate actually compose.
func Example_checkpointBackend() {
	ctx := context.Background()
	store := newExampleBlobStore()
	const document = backend.DocumentID("room-1")

	// --- a live session, persisting whole state on every update -------------
	live := crdt.NewDoc(string(document), crdt.WithGC(false))
	saveWholeState := func() {
		// EncodeStateAsUpdate over the live document is what satisfies the
		// caller's monotonicity obligation: it always covers everything saved
		// before. A checkpoint store replaces rather than merges, so handing it
		// a partial state would silently discard the difference.
		update, err := crdt.EncodeStateAsUpdate(live, nil)
		mustExample(err)
		_, err = store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: document, Update: update,
		})
		mustExample(err)
	}
	live.OnUpdate(func([]byte, any) { saveWholeState() })

	live.GetText("body").Insert(0, "hello", crdt.Object{})
	live.GetText("body").Insert(5, " world", crdt.Object{})
	live.Destroy()

	// --- cold recovery: storage is the only input ---------------------------
	recovered, err := loadCheckpointDocument(ctx, store, document)
	mustExample(err)
	fmt.Println("recovered:", recovered.GetText("body").ToString())

	// --- a client syncs against the recovered document ----------------------
	client := crdt.NewDoc(string(document), crdt.WithGC(false))
	step2, err := protocol.EncodeSyncStep2(recovered, crdt.EncodeStateVector(client))
	mustExample(err)
	_, err = protocol.NewSyncHandler(client).HandleMessage(step2, nil)
	mustExample(err)
	fmt.Println("client:", client.GetText("body").ToString())

	// One blob, rewritten per update — not a growing log.
	fmt.Println("blobs stored:", len(store.blobs), "writes:", store.writes)

	recovered.Destroy()
	client.Destroy()
	// Output:
	// recovered: hello world
	// client: hello world
	// blobs stored: 1 writes: 2
}

// loadCheckpointDocument is the whole recovery path for this profile: one read,
// no pagination, no replay. Compare loadExampleDocument, which pages a log.
func loadCheckpointDocument(ctx context.Context, store persistence.CheckpointStore, document backend.DocumentID) (*crdt.Doc, error) {
	doc := crdt.NewDoc(string(document), crdt.WithGC(false))
	checkpoint, err := store.LoadCheckpoint(ctx, document)
	if errors.Is(err, persistence.ErrNotFound) {
		return doc, nil
	}
	if err != nil {
		doc.Destroy()
		return nil, err
	}
	if err := crdt.ApplyUpdate(doc, checkpoint.Update, nil); err != nil {
		doc.Destroy()
		return nil, err
	}
	return doc, nil
}
