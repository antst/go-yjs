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

// The other suites in this package drive one caller at a time. That leaves the
// contract's concurrent clauses uncertified: Appender promises a revision
// "greater than every previously acknowledged revision", and Compactor promises
// to "preserve concurrent appends after Basis" — a sentence with no concurrent
// test behind it, because "racing append survives compaction" races nothing and
// runs entirely in program order.
//
// These suites are therefore genuinely concurrent, and the assertions are chosen
// so that they hold under EVERY interleaving rather than describing one. A
// concurrency test whose expected result depends on who won is a flake with a
// justification attached.
//
// Two failure modes are guarded against deliberately. A suite that spawns N
// callers proves nothing if the store serialises them into one operation, so
// every suite here asserts the COUNT of operations that actually reached the
// store, not only the outcome. And a suite that accepts "some appends failed"
// can pass against a store that fails all of them, so each requires the work to
// have actually been done.

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

// PersistenceConcurrency certifies the append contract under concurrent callers.
// A single-threaded store passes it unchanged; it exists to fail stores whose
// ordering is only correct when nobody else is writing.
func PersistenceConcurrency(t *testing.T, factory StoreFactory) {
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
			t.Fatalf("history has %d records, want %d; an acknowledged append was lost", len(history), writers)
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
			t.Fatalf("%d acknowledged appends are missing from history: %v", len(acknowledged), acknowledged)
		}
	})

	t.Run("concurrent appends across documents do not interfere", func(t *testing.T) {
		store := factory()
		const documents = 8
		const perDocument = 6
		runConcurrently(documents*perDocument, func(i int) {
			document := backend.DocumentID(fmt.Sprintf("doc-%d", i/perDocument))
			_, err := store.Append(context.Background(), persistence.AppendRequest{
				DocumentID: document, Update: []byte(fmt.Sprintf("u-%d", i)),
			})
			if err != nil {
				panic(fmt.Sprintf("Append(%s) = %v", document, err))
			}
		})
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

// PersistenceCompactionConcurrency certifies the sentence in the Compactor
// contract that says "preserve concurrent appends after Basis". The compaction
// suite's own case establishes the sequential CAS behaviour; this one actually
// runs appends alongside the compaction.
func PersistenceCompactionConcurrency(t *testing.T, factory CompactingStoreFactory) {
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
		// The compaction is worker 0 so it is released by the same barrier as
		// the appends; sequencing it before or after would reproduce the
		// sequential test this suite exists to complement.
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
		covered := persistence.Revision(0)
		if page.Checkpoint != nil {
			covered = page.Checkpoint.Revision
		}
		found := make(map[persistence.Revision]struct{}, writers)
		for _, record := range page.Updates {
			found[record.Revision] = struct{}{}
		}
		for revision, payload := range acknowledged {
			if revision <= covered {
				// Folded into the checkpoint is a legitimate outcome only if the
				// compaction's basis actually reached it.
				continue
			}
			if _, present := found[revision]; !present {
				t.Fatalf("append %q at revision %d was acknowledged, is past the checkpoint at %d, and is not in the tail; a compaction discarded a concurrent append",
					payload, revision, covered)
			}
		}
	})
}

// PersistenceFencingConcurrency certifies that fence authority is decided by a
// single order, not by whichever owner happens to be scheduled. A store that
// checks the fence and then writes without holding the two together lets a
// superseded owner land a write after its successor's — which is the exact
// scenario fencing exists to prevent, and the one a sequential test cannot see.
func PersistenceFencingConcurrency(t *testing.T, factory StoreFactory) {
	t.Helper()
	if mode := factory().FenceMode(); mode != persistence.Fenced {
		t.Fatalf("fencing concurrency factory mode = %d, want Fenced", mode)
	}

	t.Run("a superseded owner never lands a write after its successor", func(t *testing.T) {
		store := factory()
		const rounds = 16
		results := make([]concurrentAppend, rounds*2)
		// Old and new owner interleaved, so neither is systematically first.
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
			t.Fatal("every append was rejected; the suite would pass vacuously")
		}

		// THE PROPERTY: order the accepted writes by the order the store itself
		// assigned them, and the fences along that order must never go back
		// down. A single stale write landing after a newer one is exactly the
		// split-brain fencing prevents, and it shows up here as a decrease.
		sort.Slice(accepted, func(i, j int) bool { return accepted[i].revision < accepted[j].revision })
		highest := backend.Fence(0)
		for _, r := range accepted {
			if r.fence < highest {
				t.Fatalf("revision %d was written at fence %d after fence %d had already been accepted; a superseded owner wrote after its successor",
					r.revision, r.fence, highest)
			}
			highest = r.fence
		}

		// And the decision must be durable: once the successor has written, the
		// superseded owner is refused from then on.
		if highest < 2 {
			t.Fatalf("the successor never wrote (highest accepted fence %d); this run certified nothing", highest)
		}
		_, err := store.Append(context.Background(), persistence.AppendRequest{
			DocumentID: "doc", Fence: 1, Update: []byte("after"),
		})
		if !errors.Is(err, persistence.ErrStaleFence) {
			t.Fatalf("stale append after the race = %v, want ErrStaleFence", err)
		}
	})
}

// CheckpointPersistenceConcurrency certifies that a checkpoint is one value.
// The profile keeps a single rewritable blob, so a store that writes the update
// and the state vector as two steps can serve one save's update beside another
// save's vector. Nothing rejects that pairing: it decodes, and it is wrong —
// the same silent shape as an undeclared codec.
func CheckpointPersistenceConcurrency(t *testing.T, factory CheckpointStoreFactory) {
	t.Helper()
	fixtures := acceptedFixtures(t, factory, "concurrent")
	if len(fixtures) == 0 {
		t.Fatal("the store accepted no checkpoint encoding")
	}
	fixture := fixtures[0]
	fenced := factory().FenceMode() == persistence.Fenced

	t.Run("a load never returns a mixture of two saves", func(t *testing.T) {
		store := factory()
		const writers = 16
		// REAL document states, not synthetic bytes. A Checkpoint is the
		// document's state and a store is entitled to read it — one whose medium
		// cannot keep the state vector must decode the update to return a
		// correct one. Feeding arbitrary bytes fails such a store for honouring
		// a permission the contract grants, which is the rule the rest of this
		// package already follows.
		saves := make([]checkpointFixture, writers)
		pairs := make(map[string]string, writers+1)
		for i := range writers {
			saves[i] = fixtureIn(t, fixture.encoding, fmt.Sprintf("state-%02d", i))
			pairs[string(saves[i].update)] = string(saves[i].vector)
		}
		// Seeded before the race so a reader always has something to load. On a
		// fast store every save can complete between two reader iterations, and
		// the readers then observe only ErrNotFound.
		seed := fixtureIn(t, fixture.encoding, "state-seed")
		pairs[string(seed.update)] = string(seed.vector)
		seedRequest := persistence.SaveCheckpointRequest{
			DocumentID: "doc", Encoding: seed.encoding, Update: seed.update, StateVector: seed.vector,
		}
		if fenced {
			seedRequest.Fence = 1
		}
		if _, err := store.SaveCheckpoint(context.Background(), seedRequest); err != nil {
			t.Fatal(err)
		}

		revisions := make([]persistence.Revision, writers)
		saveErrs := make([]error, writers)
		// Readers run FOR THE DURATION of the writes rather than after them. A
		// torn checkpoint is a state the store passes through, so a suite that
		// only inspects the store once every writer has finished cannot see one
		// at all — it observes whatever the last writer left, which is usually
		// consistent no matter how the halves were written.
		const readers = 4
		done := make(chan struct{})
		type mixture struct {
			update, vector, want string
		}
		mixtures := make([]*mixture, readers)
		loads := make([]int, readers)
		loadErrs := make([]error, readers)

		runConcurrently(writers+readers, func(i int) {
			if i >= writers {
				r := i - writers
				// EVERY path through this loop reaches the done check. An
				// earlier version skipped it on the error path, so a store that
				// failed every load spun here until the test binary's own
				// timeout killed it — a hang rather than a failure, and the
				// suite reporting nothing at all about the store.
				for {
					loaded, err := store.LoadCheckpoint(context.Background(), "doc")
					switch {
					case err != nil:
						if loadErrs[r] == nil {
							loadErrs[r] = err
						}
					default:
						loads[r]++
						if want, known := pairs[string(loaded.Update)]; known &&
							string(loaded.StateVector) != want && mixtures[r] == nil {
							mixtures[r] = &mixture{
								update: string(loaded.Update),
								vector: string(loaded.StateVector),
								want:   want,
							}
						}
					}
					select {
					case <-done:
						return
					default:
					}
				}
			}
			request := persistence.SaveCheckpointRequest{
				DocumentID:  "doc",
				Encoding:    saves[i].encoding,
				Update:      saves[i].update,
				StateVector: saves[i].vector,
			}
			if fenced {
				request.Fence = 1
			}
			revisions[i], saveErrs[i] = store.SaveCheckpoint(context.Background(), request)
			if i == writers-1 {
				close(done)
			}
		})

		seenRevisions := make(map[persistence.Revision]struct{}, writers)
		for i, err := range saveErrs {
			if err != nil {
				t.Fatalf("SaveCheckpoint(%d) = %v", i, err)
			}
			if _, duplicate := seenRevisions[revisions[i]]; duplicate {
				t.Fatalf("revision %d returned twice; checkpoint revisions are not strictly increasing under concurrency", revisions[i])
			}
			seenRevisions[revisions[i]] = struct{}{}
		}
		for r, err := range loadErrs {
			if err != nil {
				t.Fatalf("reader %d: LoadCheckpoint during concurrent saves = %v; a load must not fail because a save is in flight", r, err)
			}
		}

		// Every reader completes at least one load, so this can only fire if the
		// reader loop stopped doing its job. It does NOT claim the loads
		// overlapped the saves: against a store fast enough to finish every save
		// before a reader is scheduled they observe the settled value instead.
		// The suite is still known to catch a tear — backendtest's
		// torn-checkpoint fixture is rejected by exactly this subtest.
		total := 0
		for _, n := range loads {
			total += n
		}
		if total == 0 {
			t.Fatal("no load observed the store during the saves; this subtest checked nothing")
		}
		for r, m := range mixtures {
			if m != nil {
				t.Fatalf("reader %d loaded an update beside a state vector belonging to a different save (%d loads observed): got vector %x, want %x",
					r, total, m.vector, m.want)
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
			t.Fatalf("the settled checkpoint pairs an update with another save's state vector: got %x, want %x",
				loaded.StateVector, wantVector)
		}
		if loaded.Encoding != fixture.encoding {
			t.Fatalf("load returned encoding %d, want %d", loaded.Encoding, fixture.encoding)
		}
	})
}
