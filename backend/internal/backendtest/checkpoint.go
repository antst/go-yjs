package backendtest

import (
	"context"
	"sync"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/persistence"
	"github.com/antst/go-yjs/crdt"
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
	if err := acceptCheckpointEncoding(request.Encoding); err != nil {
		return 0, err
	}
	if err := acceptCheckpointFence(s.mode, s.fences, request.DocumentID, request.Fence); err != nil {
		return 0, err
	}
	s.revision++
	// Copy on the way in: the request's slices are borrowed only for this call.
	s.states[request.DocumentID] = persistence.Checkpoint{
		Revision:    s.revision,
		Encoding:    request.Encoding,
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

// acceptCheckpointEncoding enforces that the codec is stated. This reference
// stores what it is given rather than deriving, so it supports both codecs; a
// store that supports only one returns ErrUnsupportedEncoding for the other.
func acceptCheckpointEncoding(encoding persistence.CheckpointEncoding) error {
	switch encoding {
	case persistence.EncodingV1, persistence.EncodingV2:
		return nil
	case persistence.EncodingUnspecified:
		return persistence.ErrEncodingRequired
	default:
		return persistence.ErrUnsupportedEncoding
	}
}

// BareUpdateCheckpointStore models the medium the checkpoint profile's two
// permissions exist for: a store whose durable form is a bare Yjs update and
// nothing else. With nowhere to record which codec produced the bytes it
// supports exactly one and refuses the others with ErrUnsupportedEncoding, and
// with nowhere to keep the state vector it DERIVES one on load.
//
// It exists because nothing else in this repository takes either shape. Every
// other fixture accepts both codecs and stores the vector it was handed, so
// acceptedFixtures' skip, its supported==0 guard, and the contract's permission
// to read a checkpoint were all carried by the suites with no implementation to
// exercise them. A carve-out that only the documentation travels is a promise
// nobody has cashed: the code is correct, inert, and unfalsifiable by review,
// because there is nothing wrong to read. The first consumer built this shape
// for real and found two suite defects with it, which is the wrong place for
// them to surface.
//
// Deriving the vector is also a check on the SUITE. A conforming deriving store
// can only return the right bytes if every fixture's vector really is the
// derivation of its update — so if a suite ever seeds a checkpoint with a vector
// that does not match its update, this store fails and says so.
type BareUpdateCheckpointStore struct {
	*CheckpointStore
	supported persistence.CheckpointEncoding
}

// NewBareUpdateCheckpointStore returns a store that keeps only update bytes.
func NewBareUpdateCheckpointStore(supported persistence.CheckpointEncoding, mode persistence.FenceMode) *BareUpdateCheckpointStore {
	inner := NewCheckpointStore()
	inner.mode = mode
	return &BareUpdateCheckpointStore{CheckpointStore: inner, supported: supported}
}

func (s *BareUpdateCheckpointStore) SaveCheckpoint(ctx context.Context, request persistence.SaveCheckpointRequest) (persistence.Revision, error) {
	// Order matters: an unstated codec is a caller error whatever this store
	// supports, so ErrEncodingRequired must not be reported as unsupported.
	if err := acceptCheckpointEncoding(request.Encoding); err != nil {
		return 0, err
	}
	if request.Encoding != s.supported {
		return 0, persistence.ErrUnsupportedEncoding
	}
	revision, err := s.CheckpointStore.SaveCheckpoint(ctx, request)
	if err != nil {
		return 0, err
	}
	// The vector is not kept: this medium holds the update and nothing else.
	s.mutex.Lock()
	stored := s.states[request.DocumentID]
	stored.StateVector = nil
	s.states[request.DocumentID] = stored
	s.mutex.Unlock()
	return revision, nil
}

func (s *BareUpdateCheckpointStore) LoadCheckpoint(ctx context.Context, id backend.DocumentID) (persistence.Checkpoint, error) {
	loaded, err := s.CheckpointStore.LoadCheckpoint(ctx, id)
	if err != nil {
		return persistence.Checkpoint{}, err
	}
	// Both fields are reconstructed rather than recalled: the codec from what
	// this store is fixed to, the vector by decoding the update with it.
	loaded.Encoding = s.supported
	vector, err := deriveStateVector(s.supported, loaded.Update)
	if err != nil {
		// The stored bytes cannot form the state a successful save promised.
		return persistence.Checkpoint{}, persistence.ErrCorrupt
	}
	loaded.StateVector = vector
	return loaded, nil
}

func deriveStateVector(encoding persistence.CheckpointEncoding, update []byte) ([]byte, error) {
	if encoding == persistence.EncodingV2 {
		return crdt.EncodeStateVectorFromUpdateV2(update)
	}
	return crdt.EncodeStateVectorFromUpdate(update)
}
