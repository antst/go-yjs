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
	// ErrNotFound reports that the store holds NOTHING for a document — it was
	// never saved, or was deleted.
	//
	// It must not be used for state the store knows about but cannot produce.
	// A store that resolves a document through a pointer, finds the pointer set
	// and the target gone, has not found "no history": reporting ErrNotFound
	// there makes the caller treat a document that HAD content as new, seed it
	// with create-time content, and overwrite the last good state on the next
	// save. That is silent data loss arriving through the error type. Use
	// ErrCorrupt.
	ErrNotFound = errors.New("persistence: document not found")
	// ErrConflict reports that a checkpoint basis is no longer admissible.
	ErrConflict = errors.New("persistence: revision conflict")
	// ErrCorrupt reports durable state the store cannot produce: bytes that
	// cannot form the history a successful load promised, and equally state
	// that is referenced but absent — a pointer whose target has gone.
	//
	// The distinction from ErrNotFound is load-bearing rather than cosmetic.
	// ErrNotFound says "there was never anything here", which invites a caller
	// to initialise. ErrCorrupt says "there was something and it cannot be
	// returned", which must not.
	ErrCorrupt = errors.New("persistence: corrupt history")
	// ErrStaleFence reports a write from a superseded cluster owner.
	ErrStaleFence = errors.New("persistence: stale fence")
	// ErrFenceRequired reports a missing fence on a store configured for
	// clustered writes.
	ErrFenceRequired = errors.New("persistence: fence required")
	// ErrEncodingRequired reports a checkpoint save that did not state which
	// codec produced its update. There is no default: defaulting is how a
	// V2 caller silently got a V1 reader.
	ErrEncodingRequired = errors.New("persistence: checkpoint encoding required")
	// ErrUnsupportedEncoding reports a checkpoint whose codec this store cannot
	// handle. A store that supports one codec must reject the other loudly
	// rather than decode it as if it were its own.
	ErrUnsupportedEncoding = errors.New("persistence: unsupported checkpoint encoding")
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
	Revision Revision
	// Encoding is the codec of Update, as supplied when it was saved. A reader
	// needs it for the same reason the store did: the bytes do not say.
	Encoding    CheckpointEncoding
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

// CheckpointEncoding names the Yjs codec that produced a checkpoint's update.
//
// IT EXISTS BECAUSE GUESSING IS SILENT. The V1 state-vector decoder applied to
// V2 update bytes does not fail — it returns no error and a vector describing
// ZERO clients. A store that derives the vector rather than storing it cannot
// tell the codecs apart from the bytes, so without this field it must guess,
// and the wrong guess produces a confident wrong answer rather than a
// diagnosable one.
//
// This was not hypothetical. A consumer stored bare V2 snapshots, derived with
// the V1 decoder, and passed the conformance suite — because the suite's own
// fixtures were V1, which made the wrong decoder correct for the only bytes it
// ever saw.
//
// VERIFYING A DECLARATION YOU DID NOT MAKE. A store often holds bytes it did
// not encode: another system writes the document on create, or a migration
// backfills it. Declaring an encoding for those bytes is a claim about that
// other system, so it is worth checking rather than trusting. Measured, both
// directions and every size: the WRONG decoder returns no error and an EMPTY
// state vector — never a plausible-but-wrong one. So a store can verify a
// declaration by decoding with it and, if the vector comes back empty, decoding
// with the other codec; a non-empty result from the other proves the
// declaration wrong. Empty from both means the document is genuinely empty,
// which is unambiguous. That turns a cross-system assumption into a check that
// fails loudly at the boundary instead of producing a zero-client vector.
type CheckpointEncoding uint8

const (
	// EncodingUnspecified is the zero value and is never valid. A save carrying
	// it is rejected with ErrEncodingRequired. Making the zero value mean V1
	// would recreate the original defect for every caller who forgot the field.
	EncodingUnspecified CheckpointEncoding = iota
	// EncodingV1 marks an update from EncodeStateAsUpdate, whose vector is
	// derived with EncodeStateVectorFromUpdate.
	EncodingV1
	// EncodingV2 marks an update from EncodeStateAsUpdateV2, whose vector is
	// derived with EncodeStateVectorFromUpdateV2.
	EncodingV2
)

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
// instantiate a document. THE CALLER MUST SUPPLY IT. It is free for the caller,
// which has just encoded the update, and requiring it is what keeps a store
// free of any obligation to parse CRDT bytes: a store that persists it
// alongside the update never interprets either.
//
// AN IMPLEMENTATION MAY IGNORE IT. What LoadCheckpoint returns must be correct
// for the stored update; it need not be the same bytes the caller supplied. A
// store with somewhere cheap to put it should keep it and save every reader a
// derivation; a store whose medium has no room — a blob with no free-form
// metadata — may discard it and derive on read with
// crdt.EncodeStateVectorFromUpdate or its V2 form. Both are conforming, and the
// choice is invisible to callers.
//
// Both slices are borrowed only until SaveCheckpoint returns. An implementation
// that retains or asynchronously writes either must copy first. Fence zero is
// the ordinary non-clustered mode.
type SaveCheckpointRequest struct {
	DocumentID backend.DocumentID
	Fence      backend.Fence
	// Encoding states which codec produced Update. Required; there is no
	// default. A store that supports only one codec returns
	// ErrUnsupportedEncoding for the other rather than decoding it anyway.
	Encoding    CheckpointEncoding
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
	// It MUST reject EncodingUnspecified with ErrEncodingRequired, and a codec
	// it does not support with ErrUnsupportedEncoding. Neither may be treated
	// as a default: a store that decodes unknown bytes with its preferred codec
	// produces a confident wrong answer, which is the defect this field exists
	// to remove.
	//
	// Errors: ErrStaleFence when a superseded owner writes, ErrFenceRequired
	// when a fenced store receives fence zero, ErrUnexpectedFence when an
	// unfenced store receives a non-zero fence.
	SaveCheckpoint(context.Context, SaveCheckpointRequest) (Revision, error)
	// LoadCheckpoint returns the current state, or ErrNotFound when the
	// document has never been saved. The returned Encoding must be the one the
	// state was saved with. Both slices in the result are owned by the
	// caller: mutating them must not change durable state or another reader's
	// result.
	LoadCheckpoint(context.Context, backend.DocumentID) (Checkpoint, error)
	// FenceMode is fixed at construction, for the same reason it is on Store.
	//
	// FENCED MODE NEEDS SOMEWHERE TO PERSIST THE EPOCH. Rejecting a superseded
	// owner means remembering, per document and durably, the highest fence
	// accepted so far. A medium that stores only a content blob with no
	// free-form metadata cannot do that, and holding the epoch in a separate
	// service is not a substitute: this contract is meant to be the FINAL
	// stale-owner rejection precisely because a partitioned holder can stay
	// alive, so a rejection that depends on reaching another service is not the
	// backstop it looks like. Such a medium should report Unfenced and rely on
	// the cluster lease alone.
	FenceMode() FenceMode
}

// DeleteRequest removes a document's durable state.
//
// Fence zero is the ordinary non-clustered mode.
type DeleteRequest struct {
	DocumentID backend.DocumentID
	Fence      backend.Fence
}

// Deleter removes a document's durable state entirely.
//
// OPTIONAL, AND DELIBERATELY SO. Deletion is a correctness and compliance
// requirement rather than a performance strategy, so the temptation is to put
// it on Store and CheckpointStore directly. It is optional anyway, because some
// media are forbidden to delete: WORM storage, object locks, regulated archival
// tiers. The same compliance pressure that demands erasure in one regime demands
// immutability in another, and a contract with a mandatory Delete cannot express
// the second. A store that cannot delete simply does not implement this, and a
// caller that needs erasure must type-assert and fail loudly when it is absent —
// which is a better outcome than an implementation whose Delete silently does
// nothing.
//
// ORDERING AGAINST THE IN-MEMORY REGISTRY, which is the part that goes wrong.
// Deleting durable state while a document is still live resurrects it: the live
// document is unaffected by the delete, and its next save writes the state back.
// memory.Registry.Invalidate is necessary but NOT sufficient — it drains the
// current generation, and a later Acquire opens a fresh one from storage that
// has not been deleted yet, which then saves the content back after the delete
// lands. The safe sequence has three steps:
//
//  1. stop admitting acquisitions for the document, in application state that
//     the registry's OpenFunc consults — the registry has no concept of this;
//  2. Invalidate, so the live generation drains and is destroyed;
//  3. Delete.
//
// Getting that order wrong loses no data and reports no error; it silently
// restores content that was supposed to be erased, which for a deletion request
// is the worst available failure.
type Deleter interface {
	// Delete removes the document's durable state. Returning nil means the
	// removal crossed the same durability boundary a write does, and a
	// subsequent Load or LoadCheckpoint MUST report ErrNotFound. "Eventually
	// gone" is not permitted: a load that still returns content after a
	// successful delete is indistinguishable from the delete not happening, so
	// it is not a result a caller can act on.
	//
	// IDEMPOTENT. Deleting a document with no durable state succeeds and
	// returns nil. It must not report ErrNotFound: a cascade retries, and the
	// second attempt must not fail the operation it is completing.
	//
	// Errors: ErrStaleFence when a superseded owner deletes, ErrFenceRequired
	// when a fenced store receives fence zero, ErrUnexpectedFence when an
	// unfenced store receives a non-zero fence. A rejected delete must leave
	// the durable state intact.
	Delete(context.Context, DeleteRequest) error
}

// DeletingStore is the log profile plus deletion.
type DeletingStore interface {
	Store
	Deleter
}

// DeletingCheckpointStore is the checkpoint profile plus deletion.
type DeletingCheckpointStore interface {
	CheckpointStore
	Deleter
}
