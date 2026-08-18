// Package backendtest provides internal contract fixtures. These are not
// supported backend defaults; persistence remains a port the application must
// implement.
package backendtest

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/persistence"
)

// Store is a deterministic persistence contract fixture.
type Store struct {
	mode persistence.FenceMode
	data *storeData
}

type storeData struct {
	mu   sync.Mutex
	docs map[backend.DocumentID]*history
}

type history struct {
	revision   persistence.Revision
	fence      backend.Fence
	checkpoint *persistence.Checkpoint
	records    []persistence.Record
}

// NewStore constructs an empty fixture store.
func NewStore() *Store {
	return newStore(persistence.Unfenced, &storeData{docs: make(map[backend.DocumentID]*history)})
}

// NewFencedStore constructs an empty fixture that requires a fence on every
// mutation.
func NewFencedStore() *Store {
	return newStore(persistence.Fenced, &storeData{docs: make(map[backend.DocumentID]*history)})
}

// NewFenceUpgradePair constructs unfenced and fenced views over the same
// durable fixture data.
func NewFenceUpgradePair() (*Store, *Store) {
	data := &storeData{docs: make(map[backend.DocumentID]*history)}
	return newStore(persistence.Unfenced, data), newStore(persistence.Fenced, data)
}

func newStore(mode persistence.FenceMode, data *storeData) *Store {
	return &Store{mode: mode, data: data}
}

// FenceMode reports the store's immutable write-authority mode.
func (s *Store) FenceMode() persistence.FenceMode { return s.mode }

func (s *Store) Append(ctx context.Context, request persistence.AppendRequest) (persistence.Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.data.mu.Lock()
	defer s.data.mu.Unlock()
	document := s.data.docs[request.DocumentID]
	if document == nil {
		document = &history{}
		s.data.docs[request.DocumentID] = document
	}
	if err := acceptFence(s.mode, document, request.Fence); err != nil {
		return 0, err
	}
	document.revision++
	document.records = append(document.records, persistence.Record{
		Revision: document.revision,
		Update:   append([]byte(nil), request.Update...),
	})
	return document.revision, nil
}

func (s *Store) Load(ctx context.Context, id backend.DocumentID, options persistence.LoadOptions) (persistence.Page, error) {
	if err := ctx.Err(); err != nil {
		return persistence.Page{}, err
	}
	s.data.mu.Lock()
	defer s.data.mu.Unlock()
	document := s.data.docs[id]
	if document == nil {
		return persistence.Page{}, persistence.ErrNotFound
	}
	through, offset, err := parsePageToken(options.PageToken)
	if err != nil {
		return persistence.Page{}, err
	}
	if options.PageToken == "" {
		through = document.revision
	}
	records := make([]persistence.Record, 0, len(document.records))
	for _, record := range document.records {
		if record.Revision <= through {
			records = append(records, record)
		}
	}
	if offset > len(records) {
		return persistence.Page{}, persistence.ErrCorrupt
	}
	end := len(records)
	if options.Limit > 0 && end > offset+options.Limit {
		end = offset + options.Limit
	}
	page := persistence.Page{Through: through, Updates: cloneRecords(records[offset:end])}
	if options.PageToken == "" && document.checkpoint != nil {
		checkpoint := cloneCheckpoint(*document.checkpoint)
		page.Checkpoint = &checkpoint
	}
	if end < len(records) {
		page.Next = persistence.PageToken(fmt.Sprintf("%d:%d", through, end))
	}
	return page, nil
}

func (s *Store) Compact(ctx context.Context, request persistence.CompactRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.data.mu.Lock()
	defer s.data.mu.Unlock()
	document := s.data.docs[request.DocumentID]
	if document == nil {
		return persistence.ErrNotFound
	}
	if err := acceptFence(s.mode, document, request.Fence); err != nil {
		return err
	}
	if request.Basis > document.revision || (document.checkpoint != nil && request.Basis < document.checkpoint.Revision) {
		return persistence.ErrConflict
	}
	document.checkpoint = &persistence.Checkpoint{
		Revision:    request.Basis,
		Update:      append([]byte(nil), request.CheckpointUpdate...),
		StateVector: append([]byte(nil), request.StateVector...),
	}
	kept := document.records[:0]
	for _, record := range document.records {
		if record.Revision > request.Basis {
			kept = append(kept, record)
		}
	}
	document.records = kept
	return nil
}

func acceptFence(mode persistence.FenceMode, document *history, fence backend.Fence) error {
	if mode == persistence.Unfenced {
		if fence != 0 {
			return persistence.ErrUnexpectedFence
		}
		return nil
	}
	if fence == 0 {
		return persistence.ErrFenceRequired
	}
	if fence < document.fence {
		return persistence.ErrStaleFence
	}
	if fence > document.fence {
		document.fence = fence
	}
	return nil
}

func parsePageToken(token persistence.PageToken) (persistence.Revision, int, error) {
	if token == "" {
		return 0, 0, nil
	}
	parts := strings.Split(string(token), ":")
	if len(parts) != 2 {
		return 0, 0, persistence.ErrCorrupt
	}
	through, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, 0, persistence.ErrCorrupt
	}
	offset, err := strconv.Atoi(parts[1])
	if err != nil || offset < 0 {
		return 0, 0, persistence.ErrCorrupt
	}
	return persistence.Revision(through), offset, nil
}

func cloneRecords(records []persistence.Record) []persistence.Record {
	result := make([]persistence.Record, len(records))
	for i, record := range records {
		result[i] = persistence.Record{Revision: record.Revision, Update: append([]byte(nil), record.Update...)}
	}
	return result
}

func cloneCheckpoint(checkpoint persistence.Checkpoint) persistence.Checkpoint {
	return persistence.Checkpoint{
		Revision:    checkpoint.Revision,
		Update:      append([]byte(nil), checkpoint.Update...),
		StateVector: append([]byte(nil), checkpoint.StateVector...),
	}
}

var _ persistence.CompactingStore = (*Store)(nil)

// Delete removes a document's durable history. Idempotent by contract: an
// absent document is a successful delete, not ErrNotFound.
func (s *Store) Delete(ctx context.Context, request persistence.DeleteRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.data.mu.Lock()
	defer s.data.mu.Unlock()
	document := s.data.docs[request.DocumentID]
	if document == nil {
		// Fence rules still apply to a document with no history: a superseded
		// owner must not learn it is superseded only when data happens to exist.
		document = &history{}
	}
	if err := acceptFence(s.mode, document, request.Fence); err != nil {
		return err
	}
	delete(s.data.docs, request.DocumentID)
	return nil
}
