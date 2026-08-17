package backend_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/conformance"
	"github.com/antst/go-yjs/backend/hub"
	"github.com/antst/go-yjs/backend/memory"
	"github.com/antst/go-yjs/backend/persistence"
	"github.com/antst/go-yjs/crdt"
	"github.com/antst/go-yjs/protocol"
)

// exampleLogStore is the persistence adapter an application must supply. A
// real service would put the same contract over SQL, files, or object storage;
// this small implementation keeps the example executable and is checked by
// the public persistence conformance suite below.
type exampleLogStore struct {
	mu   sync.Mutex
	docs map[backend.DocumentID]*exampleHistory
}

type exampleHistory struct {
	revision persistence.Revision
	records  []persistence.Record
}

func newExampleLogStore() *exampleLogStore {
	return &exampleLogStore{docs: make(map[backend.DocumentID]*exampleHistory)}
}

func (*exampleLogStore) FenceMode() persistence.FenceMode { return persistence.Unfenced }

func (s *exampleLogStore) Append(ctx context.Context, request persistence.AppendRequest) (persistence.Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if request.Fence != 0 {
		return 0, persistence.ErrUnexpectedFence
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	history := s.docs[request.DocumentID]
	if history == nil {
		history = &exampleHistory{}
		s.docs[request.DocumentID] = history
	}
	history.revision++
	history.records = append(history.records, persistence.Record{
		Revision: history.revision,
		Update:   append([]byte(nil), request.Update...),
	})
	return history.revision, nil
}

func (s *exampleLogStore) Load(ctx context.Context, document backend.DocumentID, options persistence.LoadOptions) (persistence.Page, error) {
	if err := ctx.Err(); err != nil {
		return persistence.Page{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	history := s.docs[document]
	if history == nil {
		return persistence.Page{}, persistence.ErrNotFound
	}

	tokenDocument, through, offset, err := parseExamplePageToken(options.PageToken)
	if err != nil {
		return persistence.Page{}, err
	}
	if options.PageToken == "" {
		through = history.revision
	} else if tokenDocument != document {
		return persistence.Page{}, persistence.ErrCorrupt
	}
	visible := make([]persistence.Record, 0, len(history.records))
	for _, record := range history.records {
		if record.Revision <= through {
			visible = append(visible, record)
		}
	}
	if offset > len(visible) {
		return persistence.Page{}, persistence.ErrCorrupt
	}
	end := len(visible)
	if options.Limit > 0 && end > offset+options.Limit {
		end = offset + options.Limit
	}
	page := persistence.Page{Through: through, Updates: cloneExampleRecords(visible[offset:end])}
	if end < len(visible) {
		encodedDocument := base64.RawURLEncoding.EncodeToString([]byte(document))
		page.Next = persistence.PageToken(fmt.Sprintf("%s:%d:%d", encodedDocument, through, end))
	}
	return page, nil
}

func parseExamplePageToken(token persistence.PageToken) (backend.DocumentID, persistence.Revision, int, error) {
	if token == "" {
		return "", 0, 0, nil
	}
	parts := strings.Split(string(token), ":")
	if len(parts) != 3 {
		return "", 0, 0, persistence.ErrCorrupt
	}
	document, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", 0, 0, persistence.ErrCorrupt
	}
	through, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return "", 0, 0, persistence.ErrCorrupt
	}
	offset, err := strconv.Atoi(parts[2])
	if err != nil || offset < 0 {
		return "", 0, 0, persistence.ErrCorrupt
	}
	return backend.DocumentID(document), persistence.Revision(through), offset, nil
}

func cloneExampleRecords(records []persistence.Record) []persistence.Record {
	cloned := make([]persistence.Record, len(records))
	for i, record := range records {
		cloned[i] = persistence.Record{Revision: record.Revision, Update: append([]byte(nil), record.Update...)}
	}
	return cloned
}

// exampleBackend is the transport-independent part of a single-process
// collaboration service. The application supplies persistence and a transport
// adapter; the module supplies the registry and fan-out defaults.
type exampleBackend struct {
	store    persistence.Store
	registry memory.Registry
	hub      hub.Hub
}

func newExampleBackend(store persistence.Store) *exampleBackend {
	return &exampleBackend{store: store, registry: memory.NewRegistry(), hub: hub.NewInProcess()}
}

type exampleSession struct {
	serverHandle memory.Handle
	subscription hub.Subscription
}

func (s *exampleSession) Close() {
	_ = s.subscription.Close()
	s.serverHandle.Release()
}

func (s *exampleBackend) Close() error {
	if err := s.hub.Close(); err != nil {
		return err
	}
	return s.registry.Close()
}

func (s *exampleBackend) connect(ctx context.Context, document backend.DocumentID, source backend.SourceID, client *crdt.Doc) (*exampleSession, error) {
	handle, err := s.registry.Acquire(ctx, document, func(ctx context.Context) (*crdt.Doc, error) {
		return loadExampleDocument(ctx, s.store, document)
	})
	if err != nil {
		return nil, err
	}
	subscription, err := s.hub.Subscribe(ctx, document, source, func(_ context.Context, message hub.Message) error {
		return crdt.ApplyUpdate(client, message.Payload, message.SourceID)
	})
	if err != nil {
		handle.Release()
		return nil, err
	}
	if err := exchangeExampleSync(handle.Doc(), client); err != nil {
		_ = subscription.Close()
		handle.Release()
		return nil, err
	}
	return &exampleSession{serverHandle: handle, subscription: subscription}, nil
}

// ingest is the transport adapter's update path. This example chooses
// apply-then-append: malformed updates never poison durable history, but an
// append failure leaves the live document ahead of storage and requires
// Registry.Invalidate. A service that trusts its update producer may instead
// append info.Body before HandleMessageWithOrigin; that avoids live-state
// rollback but accepts that a semantically invalid update can poison replay.
// Both orderings fan out only after the durability boundary.
func (s *exampleBackend) ingest(ctx context.Context, document backend.DocumentID, source backend.SourceID, server *crdt.Doc, frame []byte) error {
	info, err := protocol.InspectMessage(frame)
	if err != nil {
		return err
	}
	if info.Type != protocol.MessageSync || info.SyncType != protocol.SyncMessageUpdate {
		return fmt.Errorf("transport: want sync update, got type %d subtype %d", info.Type, info.SyncType)
	}
	if _, err := protocol.NewSyncHandler(server).HandleMessageWithOrigin(frame, &bytes.Buffer{}, source); err != nil {
		return err
	}
	if _, err := s.store.Append(ctx, persistence.AppendRequest{DocumentID: document, Update: info.Body}); err != nil {
		return err
	}
	return s.hub.Publish(ctx, hub.Message{DocumentID: document, SourceID: source, Kind: hub.DocumentUpdate, Payload: info.Body})
}

func loadExampleDocument(ctx context.Context, store persistence.Loader, document backend.DocumentID) (*crdt.Doc, error) {
	doc := crdt.NewDoc(string(document), crdt.WithGC(false))
	var token persistence.PageToken
	for {
		page, err := store.Load(ctx, document, persistence.LoadOptions{PageToken: token, Limit: 128})
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
				return nil, err
			}
		}
		for _, record := range page.Updates {
			if err := crdt.ApplyUpdate(doc, record.Update, nil); err != nil {
				doc.Destroy()
				return nil, err
			}
		}
		if page.Next == "" {
			return doc, nil
		}
		token = page.Next
	}
}

func exchangeExampleSync(server, client *crdt.Doc) error {
	serverHandler := protocol.NewSyncHandler(server)
	clientHandler := protocol.NewSyncHandler(client)
	queue := []struct {
		handler *protocol.SyncHandler
		message []byte
	}{{handler: serverHandler, message: protocol.EncodeSyncStep1(client)}}
	for len(queue) > 0 {
		item := queue[0]
		queue = queue[1:]
		var reply bytes.Buffer
		if _, err := item.handler.HandleMessage(item.message, &reply); err != nil {
			return err
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
	return nil
}

func TestExampleLogStoreConformance(t *testing.T) {
	conformance.Persistence(t, func() persistence.Store { return newExampleLogStore() })
}

func mustExample(err error) {
	if err != nil {
		panic(err)
	}
}

// Example_singleProcessBackend is an executable integration path. In a real
// service, connect and ingest are called by a WebSocket or other transport
// adapter; no transport dependency enters the CRDT or backend packages.
func Example_singleProcessBackend() {
	ctx := context.Background()
	document := backend.DocumentID("room-1")
	service := newExampleBackend(newExampleLogStore())

	alice := crdt.NewDoc("alice", crdt.WithGC(false))
	bob := crdt.NewDoc("bob", crdt.WithGC(false))
	aliceSession, err := service.connect(ctx, document, "alice", alice)
	mustExample(err)
	bobSession, err := service.connect(ctx, document, "bob", bob)
	mustExample(err)

	serverState := crdt.EncodeStateVector(aliceSession.serverHandle.Doc())
	alice.GetText("body").Insert(0, "hello", crdt.Object{})
	update, err := crdt.EncodeStateAsUpdate(alice, serverState)
	mustExample(err)
	mustExample(service.ingest(ctx, document, "alice", aliceSession.serverHandle.Doc(), protocol.EncodeUpdate(update)))
	fmt.Println("bob:", bob.GetText("body").ToString())

	invalidated := make(chan error, 1)
	go func() { invalidated <- service.registry.Invalidate(ctx, document) }()
	<-aliceSession.serverHandle.Done() // a session loop stops serving on this signal
	aliceSession.Close()
	bobSession.Close()
	mustExample(<-invalidated)
	reloaded := crdt.NewDoc("reloaded", crdt.WithGC(false))
	reloadedSession, err := service.connect(ctx, document, "reloaded", reloaded)
	mustExample(err)
	fmt.Println("reloaded:", reloaded.GetText("body").ToString())
	reloadedSession.Close()
	mustExample(service.Close())
	alice.Destroy()
	bob.Destroy()
	reloaded.Destroy()

	// Output:
	// bob: hello
	// reloaded: hello
}

var _ persistence.Store = (*exampleLogStore)(nil)
