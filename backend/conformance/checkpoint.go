package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/persistence"
	"github.com/antst/go-yjs/crdt"
)

// checkpointState builds a real document state. The fixtures must be genuine
// Yjs updates, not arbitrary bytes.
//
// WHY THIS MATTERS, and it was found by an implementation rather than by
// reasoning: the log suite uses opaque bytes deliberately, because a Record is
// whatever a transaction produced and a Store must never interpret it. A
// Checkpoint is different in kind — it IS the document's state — so a store is
// entitled to read it, and one whose medium has nowhere to keep the state
// vector must do exactly that to return a correct one. Feeding such a store
// arbitrary bytes would fail it for honouring a permission the contract grants.
//
// Byte fidelity is still asserted exactly; only the fixture changes.
func checkpointState(t *testing.T, text string) []byte {
	t.Helper()
	// The ClientID is pinned so the same text always encodes to the same bytes:
	// a random one would make a fixture built for the assertion differ from the
	// one that was saved, and the byte-fidelity checks would fail on identical
	// content.
	doc := crdt.NewDoc("conformance", crdt.WithGC(false), crdt.WithClientID(1))
	defer doc.Destroy()
	doc.GetText("body").Insert(0, text, crdt.Object{})
	update, err := crdt.EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatalf("building a checkpoint fixture: %v", err)
	}
	return update
}

// checkpointVector is the matching state vector for a fixture.
func checkpointVector(t *testing.T, update []byte) []byte {
	t.Helper()
	vector, err := crdt.EncodeStateVectorFromUpdate(update)
	if err != nil {
		t.Fatalf("building a state-vector fixture: %v", err)
	}
	return vector
}

// CheckpointStoreFactory returns a fresh, empty unfenced CheckpointStore.
type CheckpointStoreFactory func() persistence.CheckpointStore

// FencedCheckpointStoreFactory returns a fresh, empty CheckpointStore whose
// FenceMode is Fenced.
type FencedCheckpointStoreFactory func() persistence.CheckpointStore

// CheckpointPersistence checks the single-current-state profile.
//
// It deliberately does NOT assert per-record history or pagination: a
// CheckpointStore replaces on every save, so there is no earlier record to
// return and asking for one would be asserting the log profile against a medium
// that cannot provide it. What remains is everything that IS meaningful for the
// shape — round-trip fidelity, replacement, monotonic revisions, ownership of
// returned bytes in both directions, explicit absence, and cancellation.
//
// The one property this suite CANNOT check is that a caller only ever saves a
// state covering what it saved before. The bytes are opaque to persistence by
// design, so a regressing save is indistinguishable from a legitimate one. That
// obligation is on the caller and is documented on SaveCheckpointRequest.
func CheckpointPersistence(t *testing.T, factory CheckpointStoreFactory) {
	t.Helper()

	t.Run("save and load round-trip", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		if mode := store.FenceMode(); mode != persistence.Unfenced {
			t.Fatalf("unfenced checkpoint factory mode = %d, want Unfenced", mode)
		}
		update := checkpointState(t, "state-one")
		vector := checkpointVector(t, update)
		wantUpdate := string(update)
		revision, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Update: update, StateVector: vector,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Both inputs are borrowed only for the call; mutating them afterwards
		// must not reach durable state.
		update[0], vector[0] = 'X', 'X'

		got, err := store.LoadCheckpoint(ctx, "doc")
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Update) != wantUpdate {
			t.Fatalf("checkpoint update = %q, want the saved bytes", got.Update)
		}
		if got.Revision != revision {
			t.Fatalf("loaded revision %d, saved %d", got.Revision, revision)
		}
	})

	t.Run("a save replaces rather than accumulates", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		first, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Update: checkpointState(t, "state-one"), StateVector: checkpointVector(t, checkpointState(t, "state-one")),
		})
		if err != nil {
			t.Fatal(err)
		}
		latest := checkpointState(t, "state-two")
		second, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Update: latest, StateVector: checkpointVector(t, latest),
		})
		if err != nil {
			t.Fatal(err)
		}
		if second <= first {
			t.Fatalf("revisions %d then %d are not increasing", first, second)
		}
		got, err := store.LoadCheckpoint(ctx, "doc")
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Update) != string(latest) || got.Revision != second {
			t.Fatalf("after replacement the update is not the last saved one (rev %d, want %d)", got.Revision, second)
		}
	})

	t.Run("returned bytes are caller-owned", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		saved := checkpointState(t, "state-one")
		if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Update: saved, StateVector: checkpointVector(t, saved),
		}); err != nil {
			t.Fatal(err)
		}
		got, err := store.LoadCheckpoint(ctx, "doc")
		if err != nil {
			t.Fatal(err)
		}
		got.Update[0], got.StateVector[0] = 'Y', 'Y'
		again, err := store.LoadCheckpoint(ctx, "doc")
		if err != nil {
			t.Fatal(err)
		}
		if string(again.Update) != string(saved) {
			t.Fatal("durable update changed through a returned alias")
		}
		// The vector need not be the bytes supplied — a store may derive it —
		// but it must describe the stored update.
		if want := checkpointVector(t, saved); string(again.StateVector) != string(want) {
			t.Fatalf("StateVector does not describe the stored update:\n got %x\nwant %x", again.StateVector, want)
		}
	})

	t.Run("documents are isolated", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		for _, id := range []backend.DocumentID{"alpha", "beta"} {
			if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
				DocumentID: id, Update: checkpointState(t, "state-"+string(id)), StateVector: nil,
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range []backend.DocumentID{"alpha", "beta"} {
			got, err := store.LoadCheckpoint(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if want := checkpointState(t, "state-"+string(id)); string(got.Update) != string(want) {
				t.Fatalf("%s loaded another document's state", id)
			}
		}
	})

	t.Run("missing state is explicit", func(t *testing.T) {
		store := factory()
		if _, err := store.LoadCheckpoint(context.Background(), "absent"); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("LoadCheckpoint of an absent document = %v, want ErrNotFound", err)
		}
	})

	t.Run("unclustered mode rejects accidental authority", func(t *testing.T) {
		store := factory()
		_, err := store.SaveCheckpoint(context.Background(), persistence.SaveCheckpointRequest{
			DocumentID: "doc", Fence: 1, Update: checkpointState(t, "state"), StateVector: checkpointVector(t, checkpointState(t, "state")),
		})
		if !errors.Is(err, persistence.ErrUnexpectedFence) {
			t.Fatalf("fenced write to an unfenced store = %v, want ErrUnexpectedFence", err)
		}
	})

	t.Run("cancelled calls do not succeed", func(t *testing.T) {
		store := factory()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Update: checkpointState(t, "state"), StateVector: checkpointVector(t, checkpointState(t, "state")),
		}); err == nil {
			t.Fatal("SaveCheckpoint on a cancelled context returned nil")
		}
		if _, err := store.LoadCheckpoint(ctx, "doc"); err == nil {
			t.Fatal("LoadCheckpoint on a cancelled context returned nil")
		}
	})
}

// CheckpointPersistenceFencing checks the clustered profile. Run it only
// against a factory whose FenceMode is Fenced.
func CheckpointPersistenceFencing(t *testing.T, factory FencedCheckpointStoreFactory) {
	t.Helper()

	t.Run("fenced mode rejects absent and stale authority", func(t *testing.T) {
		store := factory()
		if mode := store.FenceMode(); mode != persistence.Fenced {
			t.Fatalf("fenced checkpoint factory mode = %d, want Fenced", mode)
		}
		ctx := context.Background()
		save := func(fence backend.Fence, state string) error {
			_, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
				DocumentID: "doc", Fence: fence, Update: checkpointState(t, state), StateVector: checkpointVector(t, checkpointState(t, state)),
			})
			return err
		}
		if err := save(0, "unfenced"); !errors.Is(err, persistence.ErrFenceRequired) {
			t.Fatalf("SaveCheckpoint without fence = %v, want ErrFenceRequired", err)
		}
		if err := save(1, "one"); err != nil {
			t.Fatal(err)
		}
		if err := save(2, "two"); err != nil {
			t.Fatal(err)
		}
		// A superseded owner must be rejected, and rejection must not have
		// installed anything: the last accepted state has to survive intact.
		if err := save(1, "stale"); !errors.Is(err, persistence.ErrStaleFence) {
			t.Fatalf("SaveCheckpoint with a superseded fence = %v, want ErrStaleFence", err)
		}
		got, err := store.LoadCheckpoint(ctx, "doc")
		if err != nil {
			t.Fatal(err)
		}
		if want := checkpointState(t, "two"); string(got.Update) != string(want) {
			t.Fatal("a rejected stale write changed the last accepted state")
		}
	})

	t.Run("loads do not require a fence", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Fence: 1, Update: checkpointState(t, "state"), StateVector: checkpointVector(t, checkpointState(t, "state")),
		}); err != nil {
			t.Fatal(err)
		}
		// Fence mode governs mutations, not reads — a recovering replica that
		// has not yet acquired ownership still has to be able to read.
		if _, err := store.LoadCheckpoint(ctx, "doc"); err != nil {
			t.Fatalf("LoadCheckpoint on a fenced store = %v, want success", err)
		}
	})
}
