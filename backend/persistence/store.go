// Package persistence defines durable update-log and checkpoint contracts for
// Yjs-compatible backends. Implementations may use SQL, files, object storage,
// or another medium; the contract is expressed only in bytes and revisions.
//
// TWO PROFILES, CHOSEN BY THE MEDIUM. Store is the log profile: appended
// records stay independently readable, so recovery can be incremental and
// compaction is an optimisation. CheckpointStore is the single-current-state
// profile, for a medium whose durable unit is a document-sized blob rewritten in
// place — an object-store key, a file whose id is a stable pointer, one row.
//
// Pick by asking whether the medium can retain two writes as two separately
// readable records. Forcing a blob medium into the log profile costs either one
// blob per record, abandoning the stable pointer the medium exists to provide,
// or a log framed inside the blob, which breaks the stored format for anything
// else that reads or writes it.
//
// Fence mode governs mutations, not stored history. A deployment may reopen
// data written through an Unfenced store as Fenced without migrating it: loads
// return the existing recovery view, and the first fenced mutation establishes
// the epoch sequence.
package persistence

import (
	"context"
	"errors"

	"github.com/antst/go-yjs/backend"
)

var (
	// ErrNotFound reports that no durable history exists for a document.
	ErrNotFound = errors.New("persistence: document not found")
	// ErrConflict reports that a checkpoint basis is no longer admissible.
	ErrConflict = errors.New("persistence: revision conflict")
	// ErrCorrupt reports durable bytes that cannot form the history promised by
	// a successful load.
	ErrCorrupt = errors.New("persistence: corrupt history")
	// ErrStaleFence reports a write from a superseded cluster owner.
	ErrStaleFence = errors.New("persistence: stale fence")
	// ErrFenceRequired reports a missing fence on a store configured for
	// clustered writes.
	ErrFenceRequired = errors.New("persistence: fence required")
	// ErrUnexpectedFence reports a clustered write sent to a store configured
	// for the unclustered mode.
	ErrUnexpectedFence = errors.New("persistence: unexpected fence")
)

// FenceMode is a fixed property of a Store. It is not inferred from individual
// write requests, because doing so would let clustered code silently lose its
// stale-owner protection when one call accidentally omitted a fence.
type FenceMode uint8

const (
	// Unfenced accepts only Fence(0) and is the normal single-process mode.
	Unfenced FenceMode = iota
	// Fenced requires a non-zero fence on every mutation and rejects stale
	// epochs.
	Fenced
)

// Revision is an opaque, monotonically increasing position in one document's
// durable log. It is a storage serialization token, not a Yjs state vector.
type Revision uint64

// PageToken continues a load at the same fixed Through revision. Its contents
// are private to the persistence implementation.
type PageToken string

// Record is one durably appended Yjs transaction update.
//
// Update is owned by the caller of Load. Mutating it must not modify durable
// state or another returned record.
type Record struct {
	Revision Revision
	Update   []byte
}

// Checkpoint is a merged update that covers all records through Revision.
// StateVector describes the same logical coverage without requiring a caller
// to instantiate a CRDT document.
//
// Both byte slices returned by Load are caller-owned.
type Checkpoint struct {
	Revision    Revision
	Update      []byte
	StateVector []byte
}

// LoadOptions controls one page of a consistent history read.
type LoadOptions struct {
	// AfterStateVector lets an implementation omit updates the caller already
	// has. An implementation that cannot use it may return a complete recovery
	// history instead.
	AfterStateVector []byte
	// PageToken is empty for the first page and is the prior page's Next value
	// thereafter.
	PageToken PageToken
	// Limit is the maximum number of tail records. Zero lets the
	// implementation choose a bounded default.
	Limit int
}

// Page is one page of a self-consistent recovery view.
//
// Through is fixed by the first page. Continuations must read the same view,
// excluding appends with a later revision. Next being empty is the only signal
// that the view is complete; returning a partial history with an empty Next is
// a contract violation. Checkpoint is normally present only on the first page.
type Page struct {
	Checkpoint *Checkpoint
	Updates    []Record
	Through    Revision
	Next       PageToken
}

// AppendRequest appends one transaction update.
//
// Update is borrowed only until Append returns. An implementation that retains
// or asynchronously writes it must copy it first. Fence zero is the ordinary
// non-clustered mode.
type AppendRequest struct {
	DocumentID backend.DocumentID
	Fence      backend.Fence
	Update     []byte
}

// CompactRequest atomically installs a checkpoint and removes only log records
// at or before Basis. Records appended after Basis must survive the operation.
//
// CheckpointUpdate and StateVector are borrowed only until Compact returns.
// Fence zero is the ordinary non-clustered mode.
type CompactRequest struct {
	DocumentID       backend.DocumentID
	Fence            backend.Fence
	Basis            Revision
	CheckpointUpdate []byte
	StateVector      []byte
}

// Appender durably appends transaction updates.
type Appender interface {
	// Append returning nil means the update crossed the implementation's
	// documented durability boundary. The returned revision belongs to this
	// document and is greater than every previously acknowledged revision.
	Append(context.Context, AppendRequest) (Revision, error)
}

// Loader reads a complete or explicitly paginated recovery history.
type Loader interface {
	Load(context.Context, backend.DocumentID, LoadOptions) (Page, error)
}

// Store is the minimum persistence implementation a backend must provide. Its
// FenceMode is fixed when the implementation is constructed.
//
// Fence mode governs mutation authority, not the durable history format. A
// Fenced store must be able to read history previously written through an
// Unfenced store over the same data, and its first fenced mutation establishes
// the initial epoch without requiring a history migration.
type Store interface {
	Appender
	Loader
	FenceMode() FenceMode
}

// Compactor is the optional checkpoint/compaction capability.
type Compactor interface {
	// Compact is compare-and-swap against Basis. It must preserve concurrent
	// appends after Basis and return ErrConflict when the basis cannot safely be
	// installed.
	Compact(context.Context, CompactRequest) error
}

// CompactingStore is a Store that supports durable checkpoints.
type CompactingStore interface {
	Store
	Compactor
}

// SaveCheckpointRequest installs the complete durable state of one document.
//
// Update must cover EVERYTHING the caller has previously saved for this
// document. A CheckpointStore replaces rather than merges, so a save that
// regresses coverage discards the difference permanently and the store cannot
// detect it — the bytes are opaque to persistence by design. Callers normally
// produce Update with crdt.EncodeStateAsUpdate over the live document, which
// gives that property automatically.
//
// StateVector describes the same coverage without requiring a reader to
// instantiate a document. It may be derived from Update with
// crdt.EncodeStateVectorFromUpdate or its V2 form.
//
// Both slices are borrowed only until SaveCheckpoint returns. An implementation
// that retains or asynchronously writes either must copy first. Fence zero is
// the ordinary non-clustered mode.
type SaveCheckpointRequest struct {
	DocumentID  backend.DocumentID
	Fence       backend.Fence
	Update      []byte
	StateVector []byte
}

// CheckpointStore persists exactly ONE current state per document, replacing it
// on every save.
//
// This is the profile for a medium whose durable unit is a document-sized blob
// rewritten in place — an object-store key, a file whose id is a stable
// pointer, a single row. Store is the profile for a medium that can hold an
// ordered log of independently durable records.
//
// WHICH PROFILE TO IMPLEMENT. Ask whether the medium can retain two writes as
// two separately readable records. If it can, implement Store: an append log
// preserves every transaction, so a reader can replay history and a compaction
// is an optimisation rather than a requirement. If a second write necessarily
// overwrites the first, implement CheckpointStore; making such a medium satisfy
// Store means either one blob per record — which abandons the stable-pointer
// model the medium exists to provide — or framing a log inside the blob, which
// breaks the format for anything else that reads it.
//
// WHAT IS GIVEN UP. A checkpoint store cannot serve incremental recovery, so
// there is no pagination, no per-record history, and no Compact: every load is
// the whole document. It also cannot detect a caller that saves a regressing
// state. In exchange it needs no envelope, so the stored bytes can be exactly a
// Yjs update that another system reads and writes directly.
//
// The methods are name-qualified rather than Save/Load so that one type can
// offer both profiles; Loader.Load already takes a different signature, and Go
// would otherwise make the two mutually exclusive.
type CheckpointStore interface {
	// SaveCheckpoint returning nil means the state crossed the implementation's
	// durability boundary. Revisions are strictly increasing per document.
	//
	// Errors: ErrStaleFence when a superseded owner writes, ErrFenceRequired
	// when a fenced store receives fence zero, ErrUnexpectedFence when an
	// unfenced store receives a non-zero fence.
	SaveCheckpoint(context.Context, SaveCheckpointRequest) (Revision, error)
	// LoadCheckpoint returns the current state, or ErrNotFound when the
	// document has never been saved. Both slices in the result are owned by the
	// caller: mutating them must not change durable state or another reader's
	// result.
	LoadCheckpoint(context.Context, backend.DocumentID) (Checkpoint, error)
	// FenceMode is fixed at construction, for the same reason it is on Store.
	FenceMode() FenceMode
}
