package backendtest

import (
	"context"
	"sync"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/persistence"
)

// CheckpointStore is a conforming in-memory reference for the single-current-
// state profile. It exists so the conformance suite has an implementation known
// to satisfy it, which is what makes a failure in the suite mean something.
type CheckpointStore struct {
	mode  persistence.FenceMode
	mutex sync.Mutex
	// One entry per document: saving replaces, never appends. That is the whole
	// point of the profile.
	states   map[backend.DocumentID]persistence.Checkpoint
	fences   map[backend.DocumentID]backend.Fence
	revision persistence.Revision
}

// NewCheckpointStore returns an empty unfenced store.
func NewCheckpointStore() *CheckpointStore {
	return &CheckpointStore{
		mode:   persistence.Unfenced,
		states: make(map[backend.DocumentID]persistence.Checkpoint),
		fences: make(map[backend.DocumentID]backend.Fence),
	}
}

// NewFencedCheckpointStore returns an empty store that requires a fence.
func NewFencedCheckpointStore() *CheckpointStore {
	store := NewCheckpointStore()
	store.mode = persistence.Fenced
	return store
}

func (s *CheckpointStore) FenceMode() persistence.FenceMode { return s.mode }

func (s *CheckpointStore) SaveCheckpoint(ctx context.Context, request persistence.SaveCheckpointRequest) (persistence.Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if err := acceptCheckpointFence(s.mode, s.fences, request.DocumentID, request.Fence); err != nil {
		return 0, err
	}
	s.revision++
	// Copy on the way in: the request's slices are borrowed only for this call.
	s.states[request.DocumentID] = persistence.Checkpoint{
		Revision:    s.revision,
		Update:      append([]byte(nil), request.Update...),
		StateVector: append([]byte(nil), request.StateVector...),
	}
	if request.Fence != 0 {
		s.fences[request.DocumentID] = request.Fence
	}
	return s.revision, nil
}

func (s *CheckpointStore) LoadCheckpoint(ctx context.Context, id backend.DocumentID) (persistence.Checkpoint, error) {
	if err := ctx.Err(); err != nil {
		return persistence.Checkpoint{}, err
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	stored, ok := s.states[id]
	if !ok {
		return persistence.Checkpoint{}, persistence.ErrNotFound
	}
	// Copy on the way out: the caller owns what it receives.
	return cloneCheckpoint(stored), nil
}

// acceptCheckpointFence mirrors the log profile's rule. Fence state is tracked
// per document rather than per store so that one document's epoch cannot
// invalidate another's.
func acceptCheckpointFence(mode persistence.FenceMode, fences map[backend.DocumentID]backend.Fence, id backend.DocumentID, fence backend.Fence) error {
	if mode == persistence.Unfenced {
		if fence != 0 {
			return persistence.ErrUnexpectedFence
		}
		return nil
	}
	if fence == 0 {
		return persistence.ErrFenceRequired
	}
	if current, ok := fences[id]; ok && fence < current {
		return persistence.ErrStaleFence
	}
	return nil
}

// Delete removes a document's durable state, idempotently.
func (s *CheckpointStore) Delete(ctx context.Context, request persistence.DeleteRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if err := acceptCheckpointFence(s.mode, s.fences, request.DocumentID, request.Fence); err != nil {
		return err
	}
	delete(s.states, request.DocumentID)
	return nil
}
