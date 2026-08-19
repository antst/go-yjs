package conformance

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/persistence"
)

// The rest of this package drives one caller at a time. That left the contract's
// concurrent clauses uncertified: Store promises its methods are safe for
// concurrent use with atomic per-document decisions, Appender promises a
// revision "greater than every previously acknowledged revision", and Compactor
// promises to "preserve concurrent appends after Basis" — a sentence whose only
// test, "racing append survives compaction", runs entirely in program order.
//
// NONE OF THIS IS EXPORTED. Concurrency is not an optional capability of a
// persistence store, so a suite a consumer could omit would let a store claim
// conformance while skipping the rules hardest to satisfy. The canonical
// entrypoints call these; there is no opt-out.
//
// The assertions are chosen to hold under EVERY interleaving rather than to
// describe one. A concurrency test whose expected result depends on who won is a
// flake with a justification attached.

// concurrentAppend is one caller's outcome, kept so the assertions can be made
// over the acknowledged set rather than over the attempts.
type concurrentAppend struct {
	payload  string
	fence    backend.Fence
	revision persistence.Revision
	err      error
}

// runConcurrently releases every worker from one barrier and waits for all of
// them. Starting the goroutines is not enough — without the barrier the first
// worker usually finishes before the last one starts, and the test degrades into
// a sequential one that still passes.
func runConcurrently(workers int, work func(i int)) {
	var ready, done sync.WaitGroup
	release := make(chan struct{})
	ready.Add(workers)
	done.Add(workers)
	for i := range workers {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-release
			work(i)
		}(i)
	}
	ready.Wait()
	close(release)
	done.Wait()
}

func persistenceConcurrency(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("concurrent appends are all durable with distinct increasing revisions", func(t *testing.T) {
		store := factory()
		const writers = 24
		results := make([]concurrentAppend, writers)
		runConcurrently(writers, func(i int) {
			payload := fmt.Sprintf("update-%02d", i)
			revision, err := store.Append(context.Background(), persistence.AppendRequest{
				DocumentID: "doc", Update: []byte(payload),
			})
			results[i] = concurrentAppend{payload: payload, revision: revision, err: err}
		})

		acknowledged := make(map[persistence.Revision]string, writers)
		for _, r := range results {
			if r.err != nil {
				t.Fatalf("Append(%s) = %v; the contract has no licence to refuse a concurrent append", r.payload, r.err)
			}
			if previous, duplicate := acknowledged[r.revision]; duplicate {
				t.Fatalf("revision %d acknowledged for both %q and %q; revisions are not unique under concurrency",
					r.revision, previous, r.payload)
			}
			acknowledged[r.revision] = r.payload
		}

		history := loadComplete(t, store, "doc", 0)
		if len(history) != writers {
			t.Fatalf("history has %d records, want %d; an acknowledged append was lost under concurrency",
				len(history), writers)
		}
		var previous persistence.Revision
		for i, record := range history {
			if i > 0 && record.Revision <= previous {
				t.Fatalf("history revisions %d then %d are not increasing", previous, record.Revision)
			}
			previous = record.Revision
			want, acked := acknowledged[record.Revision]
			if !acked {
				t.Fatalf("history contains revision %d, which no Append acknowledged", record.Revision)
			}
			if string(record.Update) != want {
				t.Fatalf("revision %d holds %q, acknowledged for %q; a concurrent append overwrote another's payload",
					record.Revision, record.Update, want)
			}
			delete(acknowledged, record.Revision)
		}
		if len(acknowledged) != 0 {
			t.Fatalf("%d acknowledged appends are missing from history; an acknowledged append was lost under concurrency",
				len(acknowledged))
		}
	})

	t.Run("concurrent appends across documents do not interfere", func(t *testing.T) {
		store := factory()
		const documents = 8
		const perDocument = 6
		errs := make([]error, documents*perDocument)
		runConcurrently(documents*perDocument, func(i int) {
			document := backend.DocumentID(fmt.Sprintf("doc-%d", i/perDocument))
			_, errs[i] = store.Append(context.Background(), persistence.AppendRequest{
				DocumentID: document, Update: []byte(fmt.Sprintf("u-%d", i)),
			})
		})
		// Reported from the test goroutine, never panicked from a worker: a
		// reusable suite must name the contract that was violated rather than
		// crash through the harness running it.
		for i, err := range errs {
			if err != nil {
				t.Fatalf("Append to doc-%d = %v", i/perDocument, err)
			}
		}
		for d := range documents {
			document := backend.DocumentID(fmt.Sprintf("doc-%d", d))
			history := loadComplete(t, store, document, 0)
			if len(history) != perDocument {
				t.Fatalf("%s has %d records, want %d; per-document isolation broke under concurrency",
					document, len(history), perDocument)
			}
		}
	})
}

func persistenceCompactionConcurrency(t *testing.T, factory CompactingStoreFactory) {
	t.Helper()

	t.Run("appends racing a compaction are never lost", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		basis, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("basis")})
		if err != nil {
			t.Fatal(err)
		}

		const writers = 16
		results := make([]concurrentAppend, writers)
		var compactErr error
		// The compaction is worker 0 so the same barrier releases it and the
		// appends; sequencing it before or after reproduces the sequential test
		// this exists to complement.
		runConcurrently(writers+1, func(i int) {
			if i == 0 {
				compactErr = store.Compact(ctx, persistence.CompactRequest{
					DocumentID:       "doc",
					Basis:            basis,
					CheckpointUpdate: []byte("checkpoint"),
					StateVector:      []byte("state-vector"),
				})
				return
			}
			payload := fmt.Sprintf("racing-%02d", i)
			revision, err := store.Append(ctx, persistence.AppendRequest{
				DocumentID: "doc", Update: []byte(payload),
			})
			results[i-1] = concurrentAppend{payload: payload, revision: revision, err: err}
		})
		if compactErr != nil && !errors.Is(compactErr, persistence.ErrConflict) {
			t.Fatalf("Compact = %v, want nil or ErrConflict", compactErr)
		}

		acknowledged := make(map[persistence.Revision]string, writers)
		for _, r := range results {
			if r.err != nil {
				t.Fatalf("Append(%s) racing a compaction = %v; a compaction may not fail concurrent appends", r.payload, r.err)
			}
			acknowledged[r.revision] = r.payload
		}
		if len(acknowledged) != writers {
			t.Fatalf("%d distinct revisions for %d appends; a compaction reused revisions", len(acknowledged), writers)
		}

		page, err := store.Load(ctx, "doc", persistence.LoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		// The checkpoint must cover EXACTLY the requested basis. Excusing
		// anything at or below "whatever revision the checkpoint claims" hands a
		// broken store the answer: advance the claim to the current high-water
		// mark, discard the tail, and every lost append is written off as folded
		// in. The bytes are still the basis's, so the claim is a lie no caller
		// can detect.
		if compactErr == nil {
			if page.Checkpoint == nil {
				t.Fatal("Compact succeeded but no checkpoint was installed")
			}
			if page.Checkpoint.Revision != basis {
				t.Fatalf("checkpoint claims revision %d for a compaction whose basis was %d; a compaction discarded a concurrent append by claiming to cover it",
					page.Checkpoint.Revision, basis)
			}
		}
		found := make(map[persistence.Revision]struct{}, len(page.Updates))
		for _, record := range page.Updates {
			found[record.Revision] = struct{}{}
		}
		// Every racing append was acknowledged after the basis, so every one of
		// them belongs in the tail whether the compaction succeeded or conflicted.
		for revision, payload := range acknowledged {
			if revision <= basis {
				t.Fatalf("append %q was acknowledged at revision %d, at or below the basis %d; revisions did not advance",
					payload, revision, basis)
			}
			if _, present := found[revision]; !present {
				t.Fatalf("append %q at revision %d was acknowledged past the basis %d and is not in the tail; a compaction discarded a concurrent append",
					payload, revision, basis)
			}
		}
	})
}

// persistenceFencingConcurrency certifies that fence authority is decided by a
// single order, not by whichever owner happens to be scheduled. A store that
// validates the fence and then writes without holding the two together lets a
// superseded owner land a write after its successor's — the exact scenario
// fencing exists to prevent, and one a sequential test cannot see.
func persistenceFencingConcurrency(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("a superseded owner never lands a write after its successor", func(t *testing.T) {
		store := factory()
		const rounds = 16
		results := make([]concurrentAppend, rounds*2)
		runConcurrently(rounds*2, func(i int) {
			fence := backend.Fence(1)
			if i%2 == 1 {
				fence = backend.Fence(2)
			}
			payload := fmt.Sprintf("fence-%d-%02d", fence, i)
			revision, err := store.Append(context.Background(), persistence.AppendRequest{
				DocumentID: "doc", Fence: fence, Update: []byte(payload),
			})
			results[i] = concurrentAppend{payload: payload, fence: fence, revision: revision, err: err}
		})

		var accepted []concurrentAppend
		for _, r := range results {
			switch {
			case r.err == nil:
				accepted = append(accepted, r)
			case errors.Is(r.err, persistence.ErrStaleFence):
			default:
				t.Fatalf("Append(%s) = %v, want nil or ErrStaleFence", r.payload, r.err)
			}
		}
		if len(accepted) == 0 {
			t.Fatal("every append was rejected; this subtest would pass vacuously")
		}

		// THE PROPERTY: order the accepted writes by the order the store itself
		// assigned them, and the fences along that order must never go back
		// down. One stale write landing after a newer one is the split-brain
		// fencing prevents, and it appears here as a decrease.
		sort.Slice(accepted, func(i, j int) bool { return accepted[i].revision < accepted[j].revision })
		highest := backend.Fence(0)
		for _, r := range accepted {
			if r.fence < highest {
				t.Fatalf("revision %d was written at fence %d after fence %d had already been accepted; a superseded owner wrote after its successor",
					r.revision, r.fence, highest)
			}
			highest = r.fence
		}
		if highest < 2 {
			t.Fatalf("the successor never wrote (highest accepted fence %d); this run certified nothing", highest)
		}

		// And the decision is durable: the superseded owner is refused from now on.
		_, err := store.Append(context.Background(), persistence.AppendRequest{
			DocumentID: "doc", Fence: 1, Update: []byte("after"),
		})
		if !errors.Is(err, persistence.ErrStaleFence) {
			t.Fatalf("stale append after the race = %v, want ErrStaleFence", err)
		}
	})
}

// checkpointPersistenceConcurrency certifies that a checkpoint is ONE value. The
// profile keeps a single rewritable blob, so a store that writes the update and
// the state vector in two steps can serve one save's update beside another
// save's vector. Nothing downstream rejects that pairing: it decodes, and it is
// wrong — the same silent shape as an undeclared codec.
func checkpointPersistenceConcurrency(t *testing.T, factory CheckpointStoreFactory) {
	t.Helper()
	fixture := acceptedFixtures(t, factory, "concurrent")[0]
	fenced := factory().FenceMode() == persistence.Fenced

	t.Run("a load never returns a mixture of two saves", func(t *testing.T) {
		store := factory()
		const writers = 16
		// REAL document states, not synthetic bytes. A Checkpoint is the
		// document's state and a store is entitled to read it — one whose medium
		// cannot keep the state vector must decode the update to return a
		// correct one. Feeding arbitrary bytes fails such a store for honouring
		// a permission the contract grants.
		saves := make([]checkpointFixture, writers)
		pairs := make(map[string]string, writers+1)
		for i := range writers {
			saves[i] = fixtureIn(t, fixture.encoding, fmt.Sprintf("state-%02d", i))
			pairs[string(saves[i].update)] = string(saves[i].vector)
		}
		// Seeded before the race so a reader always has something to load: on a
		// fast store every save can complete between two reader iterations.
		seed := fixtureIn(t, fixture.encoding, "state-seed")
		pairs[string(seed.update)] = string(seed.vector)
		if _, err := store.SaveCheckpoint(context.Background(), checkpointSave(seed, fenced, 1)); err != nil {
			t.Fatal(err)
		}

		revisions := make([]persistence.Revision, writers)
		saveErrs := make([]error, writers)
		const readers = 4
		type mixture struct{ vector, want string }
		mixtures := make([]*mixture, readers)
		loads := make([]int, readers)
		loadErrs := make([]error, readers)

		// done closes when EVERY save has returned, not when one particular
		// worker has. Closing it from the highest-index writer let that single
		// goroutine finish first and stop all the readers while fifteen saves
		// were still in flight, which made "readers run for the duration of the
		// writes" false exactly when it mattered.
		done := make(chan struct{})
		var writing sync.WaitGroup
		writing.Add(writers)
		go func() { writing.Wait(); close(done) }()

		runConcurrently(writers+readers, func(i int) {
			if i >= writers {
				r := i - writers
				// EVERY path reaches the done check. An earlier version skipped
				// it on the error path, so a store that failed every load spun
				// here until the test binary's own timeout — a hang, which says
				// nothing at all about the store.
				for {
					loaded, err := store.LoadCheckpoint(context.Background(), "doc")
					if err != nil {
						if loadErrs[r] == nil {
							loadErrs[r] = err
						}
					} else {
						loads[r]++
						if want, known := pairs[string(loaded.Update)]; known &&
							string(loaded.StateVector) != want && mixtures[r] == nil {
							mixtures[r] = &mixture{vector: string(loaded.StateVector), want: want}
						}
					}
					select {
					case <-done:
						return
					default:
					}
				}
			}
			defer writing.Done()
			revisions[i], saveErrs[i] = store.SaveCheckpoint(
				context.Background(), checkpointSave(saves[i], fenced, 1))
		})

		seen := make(map[persistence.Revision]struct{}, writers)
		for i, err := range saveErrs {
			if err != nil {
				t.Fatalf("SaveCheckpoint(%d) = %v", i, err)
			}
			if _, duplicate := seen[revisions[i]]; duplicate {
				t.Fatalf("revision %d returned twice; checkpoint revisions are not strictly increasing under concurrency", revisions[i])
			}
			seen[revisions[i]] = struct{}{}
		}
		for r, err := range loadErrs {
			if err != nil {
				t.Fatalf("reader %d: LoadCheckpoint during concurrent saves = %v; a load must not fail because a save is in flight", r, err)
			}
		}

		total := 0
		for _, n := range loads {
			total += n
		}
		if total == 0 {
			t.Fatal("no load observed the store during the saves; this subtest checked nothing")
		}
		for r, m := range mixtures {
			if m != nil {
				t.Fatalf("reader %d loaded an update beside a state vector from a different save: got %x, want %x; the checkpoint was torn across two saves",
					r, m.vector, m.want)
			}
		}

		loaded, err := store.LoadCheckpoint(context.Background(), "doc")
		if err != nil {
			t.Fatal(err)
		}
		wantVector, ok := pairs[string(loaded.Update)]
		if !ok {
			t.Fatal("the settled load returned an update no save wrote")
		}
		if string(loaded.StateVector) != wantVector {
			t.Fatalf("the settled checkpoint pairs an update with another save's state vector: got %x, want %x; the checkpoint was torn across two saves",
				loaded.StateVector, wantVector)
		}
		if loaded.Encoding != fixture.encoding {
			t.Fatalf("load returned encoding %d, want %d", loaded.Encoding, fixture.encoding)
		}
	})
}

// checkpointPersistenceFencingConcurrency is the checkpoint profile's analogue
// of the log profile's fence race, and it is not redundant: this profile
// REPLACES the whole state on every save, so a superseded owner writing after
// its successor does not add a stale record, it overwrites the current one. The
// concurrency suite above uses one fence for every save and cannot see it.
func checkpointPersistenceFencingConcurrency(t *testing.T, factory FencedCheckpointStoreFactory) {
	t.Helper()
	fixture := acceptedFixtures(t, func() persistence.CheckpointStore { return factory() }, "fenced-concurrent")[0]

	t.Run("a superseded owner never overwrites its successor's state", func(t *testing.T) {
		store := factory()
		const rounds = 16
		stale := fixtureIn(t, fixture.encoding, "stale-owner-state")
		fresh := fixtureIn(t, fixture.encoding, "successor-state")

		type outcome struct {
			fence    backend.Fence
			revision persistence.Revision
			err      error
		}
		results := make([]outcome, rounds*2)
		runConcurrently(rounds*2, func(i int) {
			state, fence := stale, backend.Fence(1)
			if i%2 == 1 {
				state, fence = fresh, backend.Fence(2)
			}
			revision, err := store.SaveCheckpoint(context.Background(), checkpointSave(state, true, fence))
			results[i] = outcome{fence: fence, revision: revision, err: err}
		})

		var accepted []outcome
		for _, r := range results {
			switch {
			case r.err == nil:
				accepted = append(accepted, r)
			case errors.Is(r.err, persistence.ErrStaleFence):
			default:
				t.Fatalf("SaveCheckpoint at fence %d = %v, want nil or ErrStaleFence", r.fence, r.err)
			}
		}
		if len(accepted) == 0 {
			t.Fatal("every save was rejected; this subtest would pass vacuously")
		}
		sort.Slice(accepted, func(i, j int) bool { return accepted[i].revision < accepted[j].revision })
		highest := backend.Fence(0)
		for _, r := range accepted {
			if r.fence < highest {
				t.Fatalf("revision %d was saved at fence %d after fence %d had already been accepted; a superseded owner wrote after its successor",
					r.revision, r.fence, highest)
			}
			highest = r.fence
		}
		if highest < 2 {
			t.Fatalf("the successor never saved (highest accepted fence %d); this run certified nothing", highest)
		}

		// The durable state is what actually matters: an ordering that looked
		// monotone by revision is worthless if the stale owner's bytes are the
		// ones left behind.
		loaded, err := store.LoadCheckpoint(context.Background(), "doc")
		if err != nil {
			t.Fatal(err)
		}
		if string(loaded.Update) == string(stale.update) {
			t.Fatal("the durable checkpoint holds the superseded owner's state; a superseded owner wrote after its successor")
		}
		if string(loaded.Update) != string(fresh.update) {
			t.Fatal("the durable checkpoint holds neither owner's state")
		}
	})
}

// checkpointSave builds a save request, applying the fence only where the store
// requires one.
func checkpointSave(f checkpointFixture, fenced bool, fence backend.Fence) persistence.SaveCheckpointRequest {
	request := persistence.SaveCheckpointRequest{
		DocumentID: "doc", Encoding: f.encoding, Update: f.update, StateVector: f.vector,
	}
	if fenced {
		request.Fence = fence
	}
	return request
}
