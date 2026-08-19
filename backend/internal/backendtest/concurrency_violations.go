package backendtest

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/persistence"
)

// ConcurrencyViolation names a store defect that only exists when more than one
// caller is inside the store at once.
type ConcurrencyViolation string

const (
	// DuplicateRevisionUnderRace reads the next revision, then writes it after
	// another caller has read the same value. Both callers are acknowledged at
	// the same revision.
	DuplicateRevisionUnderRace ConcurrencyViolation = "duplicate-revision-under-race"
	// LostAppendUnderRace rebuilds the record slice from a snapshot taken before
	// a concurrent append committed, discarding it.
	LostAppendUnderRace ConcurrencyViolation = "lost-append-under-race"
	// CompactionDiscardsRacingAppend truncates the whole log rather than only
	// what the basis covers, so an append acknowledged while the compaction was
	// running disappears.
	CompactionDiscardsRacingAppend ConcurrencyViolation = "compaction-discards-racing-append"
	// FenceValidatedThenReleased checks the fence, releases the lock, and writes
	// afterwards, so a superseded owner's write lands after its successor's.
	FenceValidatedThenReleased ConcurrencyViolation = "fence-validated-then-released"
	// TornCheckpointWrite stores the update and the state vector in two separate
	// critical sections, so a load can return one save's update beside another
	// save's vector.
	TornCheckpointWrite ConcurrencyViolation = "torn-checkpoint-write"
	// LoadFailsUnderConcurrentSaves refuses every load while a save is in
	// flight. The suite must REPORT that, not spin waiting for a load that will
	// never succeed: an earlier version of the reader loop skipped its exit
	// check on the error path and hung until the test binary's own timeout,
	// which says nothing about the store at all.
	LoadFailsUnderConcurrentSaves ConcurrencyViolation = "load-fails-under-concurrent-saves"
	// CompactionAdvancesCheckpointRevision discards the racing tail and then
	// claims the checkpoint covers the current high-water mark. The bytes are
	// still the basis's, so the claim is a lie: a suite that excuses everything
	// at or below "whatever the checkpoint says it covers" writes off every lost
	// append as folded in.
	CompactionAdvancesCheckpointRevision ConcurrencyViolation = "compaction-advances-checkpoint-revision"
	// CheckpointFenceValidatedThenReleased is the checkpoint profile's version
	// of FenceValidatedThenReleased, and the damage is worse: this profile
	// REPLACES the state on every save, so a superseded owner does not add a
	// stale record, it overwrites the successor's document.
	CheckpointFenceValidatedThenReleased ConcurrencyViolation = "checkpoint-fence-validated-then-released"
)

// AllStoreConcurrencyViolations are observable through the log-profile suites.
var AllStoreConcurrencyViolations = []ConcurrencyViolation{
	DuplicateRevisionUnderRace,
	LostAppendUnderRace,
}

// AllCompactionConcurrencyViolations are observable through the compaction suite.
var AllCompactionConcurrencyViolations = []ConcurrencyViolation{
	CompactionDiscardsRacingAppend,
	CompactionAdvancesCheckpointRevision,
}

// AllFencedConcurrencyViolations are observable only against a fenced store.
var AllFencedConcurrencyViolations = []ConcurrencyViolation{
	FenceValidatedThenReleased,
}

// AllFencedCheckpointConcurrencyViolations are observable only against a fenced
// checkpoint store.
var AllFencedCheckpointConcurrencyViolations = []ConcurrencyViolation{
	CheckpointFenceValidatedThenReleased,
}

// AllCheckpointConcurrencyViolations are observable through the checkpoint suite.
var AllCheckpointConcurrencyViolations = []ConcurrencyViolation{
	TornCheckpointWrite,
	LoadFailsUnderConcurrentSaves,
}

// rendezvousTimeout bounds every wait below. A planted defect must not be able
// to hang the suite that is meant to be judging it: if the partner never
// arrives, the defect simply does not fire and the case fails as "accepted",
// which is a legible result rather than a stuck test binary.
const rendezvousTimeout = 750 * time.Millisecond

// tornWindow is how long a torn checkpoint stays observable. It is generous on
// purpose: this fixture exists to prove the suite LOOKS during the race, and a
// window too short to observe would leave that unproven either way.
const tornWindow = 50 * time.Millisecond

// rendezvous makes a concurrency defect deterministic. Relying on the scheduler
// to produce the damaging interleaving gives a rejection test that passes most
// of the time, which is worse than not having one — it reports that a rule is
// enforced on the runs where the defect happened not to fire. Here the first
// caller waits for the second, so the bad order is the only order.
type rendezvous struct {
	mu      sync.Mutex
	count   int
	arrived chan struct{}
}

// waitForPartner blocks the FIRST caller until a second one reaches this point.
// Callers after the second pass straight through, so exactly one pair is
// affected and the rest of the workload behaves normally.
func (r *rendezvous) waitForPartner() {
	r.mu.Lock()
	r.count++
	switch r.count {
	case 1:
		r.arrived = make(chan struct{})
		wait := r.arrived
		r.mu.Unlock()
		select {
		case <-wait:
		case <-time.After(rendezvousTimeout):
		}
	case 2:
		close(r.arrived)
		r.mu.Unlock()
	default:
		r.mu.Unlock()
	}
}

// signal is a one-shot ordering edge used where the two participants are not
// interchangeable — the superseded owner must wait for its successor
// specifically, not for whichever caller happens to arrive second.
type signal struct {
	once sync.Once
	done chan struct{}
}

func newSignal() *signal { return &signal{done: make(chan struct{})} }

func (s *signal) fire() { s.once.Do(func() { close(s.done) }) }

func (s *signal) await() {
	select {
	case <-s.done:
	case <-time.After(rendezvousTimeout):
	}
}

// BrokenConcurrentStore is a Store whose named defect only appears when callers
// overlap. Every method not implicated by the violation delegates to the correct
// fixture, so a suite that fails against one of these is failing for the planted
// reason and not because the fixture is generally broken.
type BrokenConcurrentStore struct {
	*Store
	violation ConcurrencyViolation
	gate      rendezvous
	// Two edges, not one. The damaging fence order needs the superseded owner
	// to have validated BEFORE the successor commits; with a single edge the
	// successor can commit first, the stale owner is then correctly rejected,
	// and the defect never occurs at all — which reads as the suite passing.
	staleValidated     *signal
	successorCommitted *signal
	// compactStarted orders the compaction against the appends it is supposed
	// to discard. Without it the SETUP append — the one establishing the basis,
	// made before the race — satisfies the "an append has committed" edge, and
	// the compaction is then free to truncate before any racing append exists.
	// Nothing is lost, and the suite correctly reports no violation.
	compactStarted *signal
	appends        atomic.Int64
}

// NewBrokenConcurrentStore constructs a store carrying one concurrency defect.
func NewBrokenConcurrentStore(violation ConcurrencyViolation, mode persistence.FenceMode) *BrokenConcurrentStore {
	data := &storeData{docs: make(map[backend.DocumentID]*history)}
	return &BrokenConcurrentStore{
		Store:              newStore(mode, data),
		violation:          violation,
		staleValidated:     newSignal(),
		successorCommitted: newSignal(),
		compactStarted:     newSignal(),
	}
}

func (s *BrokenConcurrentStore) documentLocked(id backend.DocumentID) *history {
	document := s.data.docs[id]
	if document == nil {
		document = &history{}
		s.data.docs[id] = document
	}
	return document
}

func (s *BrokenConcurrentStore) Append(ctx context.Context, request persistence.AppendRequest) (persistence.Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	switch s.violation {
	case DuplicateRevisionUnderRace:
		s.data.mu.Lock()
		document := s.documentLocked(request.DocumentID)
		if err := acceptFence(s.mode, document, request.Fence); err != nil {
			s.data.mu.Unlock()
			return 0, err
		}
		next := document.revision + 1 // read
		s.data.mu.Unlock()

		s.gate.waitForPartner() // a partner reads the same value

		s.data.mu.Lock()
		document.revision = next // write, clobbering the partner's
		document.records = append(document.records, persistence.Record{
			Revision: next,
			Update:   append([]byte(nil), request.Update...),
		})
		s.data.mu.Unlock()
		return next, nil

	case LostAppendUnderRace:
		s.data.mu.Lock()
		document := s.documentLocked(request.DocumentID)
		if err := acceptFence(s.mode, document, request.Fence); err != nil {
			s.data.mu.Unlock()
			return 0, err
		}
		document.revision++
		revision := document.revision
		snapshot := document.records // aliased before the partner commits
		s.data.mu.Unlock()

		s.gate.waitForPartner()

		s.data.mu.Lock()
		document.records = append(snapshot[:len(snapshot):len(snapshot)], persistence.Record{
			Revision: revision,
			Update:   append([]byte(nil), request.Update...),
		})
		s.data.mu.Unlock()
		return revision, nil

	case CompactionDiscardsRacingAppend, CompactionAdvancesCheckpointRevision:
		if s.appends.Add(1) == 1 {
			return s.Store.Append(ctx, request) // the basis, established before the race
		}
		s.compactStarted.await()
		revision, err := s.Store.Append(ctx, request)
		if err == nil {
			s.successorCommitted.fire() // a racing append has now committed
		}
		return revision, err

	case FenceValidatedThenReleased:
		// The successor waits BEFORE validating, not after. Validation raises the
		// stored fence, so a successor that validates first makes every later
		// stale validation fail correctly and the defect never occurs — which a
		// rejection test cannot distinguish from the suite missing it.
		if request.Fence > 1 {
			s.staleValidated.await()
		}

		s.data.mu.Lock()
		document := s.documentLocked(request.DocumentID)
		if err := acceptFence(s.mode, document, request.Fence); err != nil {
			s.data.mu.Unlock()
			if request.Fence == 1 {
				// Never leave the successor waiting on an edge that can no
				// longer be fired.
				s.staleValidated.fire()
			}
			return 0, err
		}
		s.data.mu.Unlock()

		// The superseded owner has now validated. It releases the successor,
		// waits for it to commit, and then commits anyway on the strength of a
		// check made before any of that happened.
		if request.Fence == 1 {
			s.staleValidated.fire()
			s.successorCommitted.await()
		}

		s.data.mu.Lock()
		document.revision++
		revision := document.revision
		document.records = append(document.records, persistence.Record{
			Revision: revision,
			Update:   append([]byte(nil), request.Update...),
		})
		if request.Fence > document.fence {
			document.fence = request.Fence
		}
		s.data.mu.Unlock()
		if request.Fence > 1 {
			s.successorCommitted.fire()
		}
		return revision, nil
	}
	return s.Store.Append(ctx, request)
}

func (s *BrokenConcurrentStore) Compact(ctx context.Context, request persistence.CompactRequest) error {
	if s.violation != CompactionDiscardsRacingAppend && s.violation != CompactionAdvancesCheckpointRevision {
		return s.Store.Compact(ctx, request)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Release the racing appends, then wait until one has actually been
	// acknowledged, so the defect is about DISCARDING an acknowledged append
	// rather than about arriving before there was anything to discard.
	s.compactStarted.fire()
	s.successorCommitted.await()

	s.data.mu.Lock()
	defer s.data.mu.Unlock()
	document := s.data.docs[request.DocumentID]
	if document == nil {
		return persistence.ErrNotFound
	}
	if err := acceptFence(s.mode, document, request.Fence); err != nil {
		return err
	}
	covered := request.Basis
	if s.violation == CompactionAdvancesCheckpointRevision {
		// Claims to cover everything written so far while storing only the
		// basis's bytes — the discarded appends are then indistinguishable from
		// appends the checkpoint folded in.
		covered = document.revision
	}
	document.checkpoint = &persistence.Checkpoint{
		Revision:    covered,
		Update:      append([]byte(nil), request.CheckpointUpdate...),
		StateVector: append([]byte(nil), request.StateVector...),
	}
	// Truncates everything instead of only what the basis covers.
	document.records = nil
	return nil
}

// BrokenConcurrentCheckpointStore carries a checkpoint-profile concurrency
// defect.
type BrokenConcurrentCheckpointStore struct {
	*CheckpointStore
	violation          ConcurrencyViolation
	saving             atomic.Int64
	staleValidated     *signal
	successorCommitted *signal
}

func (s *BrokenConcurrentCheckpointStore) LoadCheckpoint(ctx context.Context, id backend.DocumentID) (persistence.Checkpoint, error) {
	if s.violation == LoadFailsUnderConcurrentSaves && s.saving.Load() > 0 {
		return persistence.Checkpoint{}, persistence.ErrCorrupt
	}
	return s.CheckpointStore.LoadCheckpoint(ctx, id)
}

// NewBrokenConcurrentCheckpointStore constructs a checkpoint store carrying one
// concurrency defect.
func NewBrokenConcurrentCheckpointStore(violation ConcurrencyViolation, mode persistence.FenceMode) *BrokenConcurrentCheckpointStore {
	inner := NewCheckpointStore()
	if mode == persistence.Fenced {
		inner = NewFencedCheckpointStore()
	}
	return &BrokenConcurrentCheckpointStore{
		CheckpointStore:    inner,
		violation:          violation,
		staleValidated:     newSignal(),
		successorCommitted: newSignal(),
	}
}

func (s *BrokenConcurrentCheckpointStore) SaveCheckpoint(ctx context.Context, request persistence.SaveCheckpointRequest) (persistence.Revision, error) {
	if s.violation == CheckpointFenceValidatedThenReleased {
		return s.saveWithReleasedFence(ctx, request)
	}
	if s.violation == LoadFailsUnderConcurrentSaves {
		s.saving.Add(1)
		defer s.saving.Add(-1)
		time.Sleep(tornWindow) // hold the window open so readers meet it
		return s.CheckpointStore.SaveCheckpoint(ctx, request)
	}
	if s.violation != TornCheckpointWrite {
		return s.CheckpointStore.SaveCheckpoint(ctx, request)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mutex.Lock()
	if err := acceptCheckpointEncoding(request.Encoding); err != nil {
		s.mutex.Unlock()
		return 0, err
	}
	if err := acceptCheckpointFence(s.mode, s.fences, request.DocumentID, request.Fence); err != nil {
		s.mutex.Unlock()
		return 0, err
	}
	s.revision++
	revision := s.revision
	stored := s.states[request.DocumentID]
	stored.Revision = revision
	stored.Encoding = request.Encoding
	stored.Update = append([]byte(nil), request.Update...)
	s.states[request.DocumentID] = stored
	if request.Fence != 0 {
		s.fences[request.DocumentID] = request.Fence
	}
	s.mutex.Unlock()

	// The two halves of one value, written in two critical sections. The pause
	// only WIDENS the window a real two-step write already has; it does not
	// create the defect. A suite that observes the store only after every writer
	// has finished cannot see this at all, however wide the window.
	time.Sleep(tornWindow)

	s.mutex.Lock()
	stored = s.states[request.DocumentID]
	stored.StateVector = append([]byte(nil), request.StateVector...)
	s.states[request.DocumentID] = stored
	s.mutex.Unlock()
	return revision, nil
}

// saveWithReleasedFence validates the fence, drops the lock, and writes
// afterwards. The successor waits BEFORE validating: validation raises the
// stored fence, so a successor that validates first makes every later stale
// validation fail correctly and the defect never occurs — which a rejection test
// cannot tell apart from the suite missing it.
func (s *BrokenConcurrentCheckpointStore) saveWithReleasedFence(ctx context.Context, request persistence.SaveCheckpointRequest) (persistence.Revision, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if request.Fence > 1 {
		s.staleValidated.await()
	}

	s.mutex.Lock()
	if err := acceptCheckpointEncoding(request.Encoding); err != nil {
		s.mutex.Unlock()
		return 0, err
	}
	if err := acceptCheckpointFence(s.mode, s.fences, request.DocumentID, request.Fence); err != nil {
		s.mutex.Unlock()
		if request.Fence == 1 {
			s.staleValidated.fire()
		}
		return 0, err
	}
	s.mutex.Unlock()

	if request.Fence == 1 {
		s.staleValidated.fire()
		s.successorCommitted.await()
	}

	s.mutex.Lock()
	s.revision++
	revision := s.revision
	s.states[request.DocumentID] = persistence.Checkpoint{
		Revision:    revision,
		Encoding:    request.Encoding,
		Update:      append([]byte(nil), request.Update...),
		StateVector: append([]byte(nil), request.StateVector...),
	}
	if request.Fence != 0 && request.Fence > s.fences[request.DocumentID] {
		s.fences[request.DocumentID] = request.Fence
	}
	s.mutex.Unlock()
	if request.Fence > 1 {
		s.successorCommitted.fire()
	}
	return revision, nil
}
