package backendtest

import (
	"context"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/persistence"
)

// CheckpointViolation names one way an implementation can breach the checkpoint
// contract. These exist so the conformance suite can be shown to REJECT a
// broken store, not merely to accept a correct one — a suite that has only been
// run against a conforming implementation proves nothing about what it catches.
type CheckpointViolation string

const (
	// AliasOnSave retains the caller's slice instead of copying it, so a
	// caller reusing its buffer silently rewrites durable state.
	AliasOnSave CheckpointViolation = "alias-on-save"
	// AliasOnLoad hands out the internal slice, so one reader's mutation
	// corrupts the store and every later reader.
	AliasOnLoad CheckpointViolation = "alias-on-load"
	// FrozenRevision returns the same revision forever, destroying the ordering
	// a caller needs to tell a stale read from a fresh one.
	FrozenRevision CheckpointViolation = "frozen-revision"
	// SilentMissing returns a zero checkpoint and nil for an absent document,
	// which a caller cannot distinguish from an empty saved state.
	SilentMissing CheckpointViolation = "silent-missing"
	// IgnoreCancellation does the work anyway after the context is cancelled.
	IgnoreCancellation CheckpointViolation = "ignore-cancellation"
	// AcceptAnyFence skips fence checking entirely, so a superseded owner can
	// overwrite the state of the node that replaced it.
	AcceptAnyFence CheckpointViolation = "accept-any-fence"
	// DeleteNotIdempotent reports ErrNotFound for an absent document, which
	// fails a retrying cascade on its second attempt.
	DeleteNotIdempotent CheckpointViolation = "delete-not-idempotent"
	// DeleteLeavesState returns nil without removing anything — the shape a
	// deletion request must never silently take.
	DeleteLeavesState CheckpointViolation = "delete-leaves-state"
	// DeletePurgesEverything removes every document rather than the named one.
	DeletePurgesEverything CheckpointViolation = "delete-purges-everything"
	// DeleteIgnoresCancellation deletes anyway after the context is cancelled.
	DeleteIgnoresCancellation CheckpointViolation = "delete-ignores-cancellation"
)

// AllCheckpointViolations is every planted breach, so the rejection test cannot
// silently cover fewer than exist.
var AllCheckpointViolations = []CheckpointViolation{
	AliasOnSave, AliasOnLoad, FrozenRevision, SilentMissing, IgnoreCancellation, AcceptAnyFence,
}

// AllDeletionViolations is every planted breach of the Deleter contract.
var AllDeletionViolations = []CheckpointViolation{
	DeleteNotIdempotent, DeleteLeavesState, DeletePurgesEverything, DeleteIgnoresCancellation,
}

// BrokenCheckpointStore is the reference implementation with exactly one
// contract breach enabled.
type BrokenCheckpointStore struct {
	*CheckpointStore
	violation CheckpointViolation
}

// NewBrokenCheckpointStore returns a store breaching only the named rule. mode
// selects the fence profile so fence breaches can be exercised.
func NewBrokenCheckpointStore(violation CheckpointViolation, mode persistence.FenceMode) *BrokenCheckpointStore {
	inner := NewCheckpointStore()
	inner.mode = mode
	return &BrokenCheckpointStore{CheckpointStore: inner, violation: violation}
}

func (b *BrokenCheckpointStore) SaveCheckpoint(ctx context.Context, request persistence.SaveCheckpointRequest) (persistence.Revision, error) {
	if b.violation != IgnoreCancellation {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
	}
	b.mutex.Lock()
	defer b.mutex.Unlock()
	if b.violation != AcceptAnyFence {
		if err := acceptCheckpointFence(b.mode, b.fences, request.DocumentID, request.Fence); err != nil {
			return 0, err
		}
	}
	if b.violation != FrozenRevision {
		b.revision++
	} else if b.revision == 0 {
		b.revision = 1
	}
	stored := persistence.Checkpoint{Revision: b.revision}
	if b.violation == AliasOnSave {
		stored.Update, stored.StateVector = request.Update, request.StateVector
	} else {
		stored.Update = append([]byte(nil), request.Update...)
		stored.StateVector = append([]byte(nil), request.StateVector...)
	}
	b.states[request.DocumentID] = stored
	if request.Fence != 0 {
		b.fences[request.DocumentID] = request.Fence
	}
	return b.revision, nil
}

func (b *BrokenCheckpointStore) LoadCheckpoint(ctx context.Context, id backend.DocumentID) (persistence.Checkpoint, error) {
	if b.violation != IgnoreCancellation {
		if err := ctx.Err(); err != nil {
			return persistence.Checkpoint{}, err
		}
	}
	b.mutex.Lock()
	defer b.mutex.Unlock()
	stored, ok := b.states[id]
	if !ok {
		if b.violation == SilentMissing {
			return persistence.Checkpoint{}, nil
		}
		return persistence.Checkpoint{}, persistence.ErrNotFound
	}
	if b.violation == AliasOnLoad {
		return stored, nil
	}
	return cloneCheckpoint(stored), nil
}

// Delete breaches exactly one deletion rule, or behaves correctly for the
// violations that concern saving and loading.
func (b *BrokenCheckpointStore) Delete(ctx context.Context, request persistence.DeleteRequest) error {
	if b.violation != DeleteIgnoresCancellation {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	b.mutex.Lock()
	defer b.mutex.Unlock()
	if err := acceptCheckpointFence(b.mode, b.fences, request.DocumentID, request.Fence); err != nil {
		return err
	}
	switch b.violation {
	case DeleteNotIdempotent:
		if _, ok := b.states[request.DocumentID]; !ok {
			return persistence.ErrNotFound
		}
	case DeleteLeavesState:
		return nil
	case DeletePurgesEverything:
		b.states = make(map[backend.DocumentID]persistence.Checkpoint)
		return nil
	}
	delete(b.states, request.DocumentID)
	return nil
}
