package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/persistence"
)

// DeletingStoreFactory returns a fresh, empty unfenced log store that deletes.
type DeletingStoreFactory func() persistence.DeletingStore

// DeletingCheckpointStoreFactory returns a fresh, empty unfenced checkpoint
// store that deletes.
type DeletingCheckpointStoreFactory func() persistence.DeletingCheckpointStore

// FencedDeletingStoreFactory returns a fresh, empty FENCED log store that
// deletes.
type FencedDeletingStoreFactory func() persistence.DeletingStore

// PersistenceDeletion checks Deleter against the log profile.
func PersistenceDeletion(t *testing.T, factory DeletingStoreFactory) {
	t.Helper()

	t.Run("delete removes the history", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("one")}); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc"}); err != nil {
			t.Fatal(err)
		}
		// Strict, not eventual: a load that still returns content is
		// indistinguishable from the delete not having happened.
		if _, err := store.Load(ctx, "doc", persistence.LoadOptions{}); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("Load after a successful Delete = %v, want ErrNotFound", err)
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		// A cascade retries. Deleting what is already gone, and deleting what
		// never existed, must both succeed or the retry fails the operation it
		// is completing.
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "never-saved"}); err != nil {
			t.Fatalf("Delete of a document that never existed = %v, want nil", err)
		}
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("one")}); err != nil {
			t.Fatal(err)
		}
		for attempt := 1; attempt <= 2; attempt++ {
			if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc"}); err != nil {
				t.Fatalf("Delete attempt %d = %v, want nil", attempt, err)
			}
		}
	})

	t.Run("delete leaves other documents alone", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		for _, id := range []backend.DocumentID{"alpha", "beta"} {
			if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: id, Update: []byte("v-" + id)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "alpha"}); err != nil {
			t.Fatal(err)
		}
		survivors := loadComplete(t, store, "beta", 8)
		if len(survivors) != 1 || string(survivors[0].Update) != "v-beta" {
			t.Fatalf("deleting alpha disturbed beta: %#v", survivors)
		}
	})

	t.Run("a document can be written again after deletion", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("old")}); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc"}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("new")}); err != nil {
			t.Fatal(err)
		}
		// The old history must not come back with it.
		history := loadComplete(t, store, "doc", 8)
		if len(history) != 1 || string(history[0].Update) != "new" {
			t.Fatalf("history after delete-then-write = %#v, want only new", history)
		}
	})

	t.Run("unclustered mode rejects accidental authority", func(t *testing.T) {
		store := factory()
		err := store.Delete(context.Background(), persistence.DeleteRequest{DocumentID: "doc", Fence: 1})
		if !errors.Is(err, persistence.ErrUnexpectedFence) {
			t.Fatalf("fenced delete against an unfenced store = %v, want ErrUnexpectedFence", err)
		}
	})

	t.Run("cancelled calls do not succeed", func(t *testing.T) {
		store := factory()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc"}); err == nil {
			t.Fatal("Delete on a cancelled context returned nil")
		}
	})
}

// PersistenceDeletionFencing checks that deletion honours cluster authority.
// Run it only against a factory whose FenceMode is Fenced.
func PersistenceDeletionFencing(t *testing.T, factory FencedDeletingStoreFactory) {
	t.Helper()

	t.Run("fenced mode rejects absent and stale authority", func(t *testing.T) {
		store := factory()
		if mode := store.FenceMode(); mode != persistence.Fenced {
			t.Fatalf("fenced deleting factory mode = %d, want Fenced", mode)
		}
		ctx := context.Background()
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Fence: 2, Update: []byte("kept")}); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc"}); !errors.Is(err, persistence.ErrFenceRequired) {
			t.Fatalf("Delete without a fence = %v, want ErrFenceRequired", err)
		}
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc", Fence: 1}); !errors.Is(err, persistence.ErrStaleFence) {
			t.Fatalf("Delete from a superseded owner = %v, want ErrStaleFence", err)
		}
		// A rejected delete must leave the state intact — a superseded owner
		// must not be able to erase what its replacement is serving.
		history := loadComplete(t, store, "doc", 8)
		if len(history) != 1 || string(history[0].Update) != "kept" {
			t.Fatalf("a rejected delete disturbed the history: %#v", history)
		}
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc", Fence: 2}); err != nil {
			t.Fatalf("Delete from the current owner = %v, want nil", err)
		}
	})
}

// CheckpointPersistenceDeletion checks Deleter against the checkpoint profile.
//
// RUN IT AGAINST WHATEVER OWNS THE WHOLE DOCUMENT. The load-after-delete
// assertion requires the implementation under test to own everything that makes
// the document findable. A store that holds the content while another system
// holds the pointer to it cannot satisfy that alone — after its delete the
// pointer dangles, and ErrNotFound's contract requires ErrCorrupt there, not
// ErrNotFound. Such a component is not a conforming Deleter and failing here is
// the correct outcome; run the suite against the cascade that removes both.
//
// The seeds use a codec the store actually accepts. Hardcoding V1 here failed
// every single-codec store by construction — the same stores CheckpointPersistence
// deliberately accommodates with its ErrUnsupportedEncoding skip. Two suites in
// one package disagreeing about whether a single-codec store is legitimate is a
// defect in the suites, and the one saying no was wrong.
//
// One accepted codec is enough, unlike CheckpointPersistence which runs every
// assertion against both: nothing here depends on how the state was encoded, only
// on whether the store still holds it. The seed exists to give Delete something
// to remove.
func CheckpointPersistenceDeletion(t *testing.T, factory DeletingCheckpointStoreFactory) {
	t.Helper()
	seed := acceptedFixtures(t, func() persistence.CheckpointStore { return factory() }, "state")[0]

	t.Run("delete removes the state", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Encoding: seed.encoding, Update: seed.update, StateVector: seed.vector,
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc"}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.LoadCheckpoint(ctx, "doc"); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("LoadCheckpoint after a successful Delete = %v, want ErrNotFound", err)
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "never-saved"}); err != nil {
			t.Fatalf("Delete of a document that never existed = %v, want nil", err)
		}
		if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Encoding: seed.encoding, Update: seed.update, StateVector: seed.vector,
		}); err != nil {
			t.Fatal(err)
		}
		for attempt := 1; attempt <= 2; attempt++ {
			if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc"}); err != nil {
				t.Fatalf("Delete attempt %d = %v, want nil", attempt, err)
			}
		}
	})

	t.Run("delete leaves other documents alone", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		states := map[backend.DocumentID]checkpointFixture{
			"alpha": fixtureIn(t, seed.encoding, "state-alpha"),
			"beta":  fixtureIn(t, seed.encoding, "state-beta"),
		}
		for _, id := range []backend.DocumentID{"alpha", "beta"} {
			if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
				DocumentID: id, Encoding: states[id].encoding,
				Update: states[id].update, StateVector: states[id].vector,
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "alpha"}); err != nil {
			t.Fatal(err)
		}
		got, err := store.LoadCheckpoint(ctx, "beta")
		if err != nil {
			t.Fatalf("deleting alpha removed beta: %v", err)
		}
		if string(got.Update) != string(states["beta"].update) {
			t.Fatal("deleting alpha changed beta's state")
		}
	})

	t.Run("cancelled calls do not succeed", func(t *testing.T) {
		store := factory()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc"}); err == nil {
			t.Fatal("Delete on a cancelled context returned nil")
		}
	})
}

// FencedDeletingCheckpointStoreFactory returns a fresh, empty FENCED checkpoint
// store that deletes.
type FencedDeletingCheckpointStoreFactory func() persistence.DeletingCheckpointStore

// CheckpointPersistenceDeletionFencing checks that checkpoint deletion honours
// cluster authority. Run it only against a factory whose FenceMode is Fenced.
//
// The log profile had this from the start and the checkpoint profile did not,
// which was an oversight rather than a decision: every checkpoint store that
// fences needs the same three assertions, and the third is the one that gets
// missed.
func CheckpointPersistenceDeletionFencing(t *testing.T, factory FencedDeletingCheckpointStoreFactory) {
	t.Helper()
	// Same reason as CheckpointPersistenceDeletion: seed with a codec the store
	// takes, or a single-codec store fails on the seed and never reaches the
	// fencing rules this suite exists to check.
	seed := acceptedFixtures(t, func() persistence.CheckpointStore { return factory() }, "kept")[0]

	t.Run("fenced mode rejects absent and stale authority", func(t *testing.T) {
		store := factory()
		if mode := store.FenceMode(); mode != persistence.Fenced {
			t.Fatalf("fenced deleting checkpoint factory mode = %d, want Fenced", mode)
		}
		ctx := context.Background()
		kept := seed.update
		if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Fence: 2, Encoding: seed.encoding, Update: kept, StateVector: seed.vector,
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc"}); !errors.Is(err, persistence.ErrFenceRequired) {
			t.Fatalf("Delete without a fence = %v, want ErrFenceRequired", err)
		}
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc", Fence: 1}); !errors.Is(err, persistence.ErrStaleFence) {
			t.Fatalf("Delete from a superseded owner = %v, want ErrStaleFence", err)
		}

		// THE ASSERTION THAT GETS MISSED. Returning the right error is not
		// enough: an implementation that checks the fence and removes state in
		// the same block passes an error-only check while having already erased
		// what the replacement owner is serving. Both rejections above must have
		// left the state untouched.
		got, err := store.LoadCheckpoint(ctx, "doc")
		if err != nil {
			t.Fatalf("a rejected delete removed the state: LoadCheckpoint = %v", err)
		}
		if string(got.Update) != string(kept) {
			t.Fatal("a rejected delete changed the stored state")
		}

		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc", Fence: 2}); err != nil {
			t.Fatalf("Delete from the current owner = %v, want nil", err)
		}
		if _, err := store.LoadCheckpoint(ctx, "doc"); !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("LoadCheckpoint after an accepted delete = %v, want ErrNotFound", err)
		}
	})

	t.Run("deletion does not reset the fence high-water mark", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		state := fixtureIn(t, seed.encoding, "state")
		save := func(fence backend.Fence) error {
			_, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
				DocumentID: "doc", Fence: fence, Encoding: state.encoding, Update: state.update, StateVector: state.vector,
			})
			return err
		}
		if err := save(2); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, persistence.DeleteRequest{DocumentID: "doc", Fence: 2}); err != nil {
			t.Fatal(err)
		}
		// A store that forgets the epoch when it forgets the state lets a
		// superseded owner write again the moment the document is deleted —
		// which is exactly when a cascade is running and a stale node is most
		// likely to still be trying.
		if err := save(1); !errors.Is(err, persistence.ErrStaleFence) {
			t.Fatalf("save from a superseded owner after deletion = %v, want ErrStaleFence", err)
		}
	})
}
