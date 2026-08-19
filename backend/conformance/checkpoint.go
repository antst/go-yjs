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
// checkpointFixture is a document state in one codec, with the matching vector
// derived by that codec's decoder.
type checkpointFixture struct {
	encoding persistence.CheckpointEncoding
	update   []byte
	vector   []byte
}

// checkpointFixtures returns one fixture per supported codec. Every assertion
// below runs against BOTH, because a suite that exercises one codec makes the
// wrong decoder correct for the only bytes it ever sees — which is exactly how
// a real consumer passed this suite while deriving V1 vectors from V2 bytes.
func checkpointFixtures(t *testing.T, text string) []checkpointFixture {
	t.Helper()
	doc := crdt.NewDoc("conformance", crdt.WithGC(false), crdt.WithClientID(1))
	defer doc.Destroy()
	doc.GetText("body").Insert(0, text, crdt.Object{})

	v1, err := crdt.EncodeStateAsUpdate(doc, nil)
	if err != nil {
		t.Fatalf("building a V1 fixture: %v", err)
	}
	v1vec, err := crdt.EncodeStateVectorFromUpdate(v1)
	if err != nil {
		t.Fatalf("deriving a V1 vector: %v", err)
	}
	v2, err := crdt.EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatalf("building a V2 fixture: %v", err)
	}
	v2vec, err := crdt.EncodeStateVectorFromUpdateV2(v2)
	if err != nil {
		t.Fatalf("deriving a V2 vector: %v", err)
	}
	return []checkpointFixture{
		{persistence.EncodingV1, v1, v1vec},
		{persistence.EncodingV2, v2, v2vec},
	}
}

// fixtureIn builds one state in a specific codec, for suites that need several
// distinct states from the codec the store under test accepts.
func fixtureIn(t *testing.T, encoding persistence.CheckpointEncoding, text string) checkpointFixture {
	t.Helper()
	for _, fixture := range checkpointFixtures(t, text) {
		if fixture.encoding == encoding {
			return fixture
		}
	}
	t.Fatalf("no fixture for encoding %d", encoding)
	return checkpointFixture{}
}

func (f checkpointFixture) name() string {
	if f.encoding == persistence.EncodingV2 {
		return "v2"
	}
	return "v1"
}

// acceptedFixtures probes which codecs the implementation accepts, using a
// throwaway store per attempt so the probe cannot leave state behind.
//
// A store whose medium cannot record the codec fixes one and rejects the other;
// the rest of the suite must then exercise it with a codec it takes, or it
// would be asserting that a conforming store fails.
func acceptedFixtures(t *testing.T, newStore func() persistence.CheckpointStore, text string) []checkpointFixture {
	t.Helper()
	var accepted []checkpointFixture
	for _, fixture := range checkpointFixtures(t, text) {
		probe := newStore()
		// A fenced store rejects a fence-less write before it ever inspects the
		// encoding, so the probe must satisfy the fence to learn anything about
		// the codec.
		var fence backend.Fence
		if probe.FenceMode() == persistence.Fenced {
			fence = 1
		}
		_, err := probe.SaveCheckpoint(context.Background(), persistence.SaveCheckpointRequest{
			DocumentID: "encoding-probe", Fence: fence, Encoding: fixture.encoding,
			Update: fixture.update, StateVector: fixture.vector,
		})
		if errors.Is(err, persistence.ErrUnsupportedEncoding) {
			continue
		}
		if err != nil {
			t.Fatalf("probing %s support: %v", fixture.name(), err)
		}
		accepted = append(accepted, fixture)
	}
	if len(accepted) == 0 {
		t.Fatal("the store accepted no checkpoint codec at all")
	}
	return accepted
}

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
		supported := 0
		for _, fixture := range checkpointFixtures(t, "state-one") {
			t.Run(fixture.name(), func(t *testing.T) {
				store := factory()
				update := append([]byte(nil), fixture.update...)
				vector := append([]byte(nil), fixture.vector...)
				revision, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
					DocumentID: "doc", Encoding: fixture.encoding, Update: update, StateVector: vector,
				})
				if errors.Is(err, persistence.ErrUnsupportedEncoding) {
					// A store whose medium cannot record the codec supports one
					// and must reject the other LOUDLY. That is conforming;
					// decoding it anyway is the defect. The count below stops a
					// store rejecting everything and passing vacuously.
					t.Skipf("store does not support %s", fixture.name())
				}
				if err != nil {
					t.Fatal(err)
				}
				supported++
				// Both inputs are borrowed only for the call. Mutating them
				// afterwards must not reach durable state — and the vector is
				// asserted separately below, because a store that copies the
				// update while retaining the vector satisfies neither the
				// contract nor a caller reusing its buffer.
				update[0], vector[0] = 'X', 'X'

				got, err := store.LoadCheckpoint(ctx, "doc")
				if err != nil {
					t.Fatal(err)
				}
				if string(got.Update) != string(fixture.update) {
					t.Fatalf("checkpoint update = %q, want the saved bytes", got.Update)
				}
				if string(got.StateVector) != string(fixture.vector) {
					t.Fatalf("state vector = %x, want %x — a retained input buffer or the wrong decoder",
						got.StateVector, fixture.vector)
				}
				if got.Encoding != fixture.encoding {
					t.Fatalf("loaded encoding %d, saved %d", got.Encoding, fixture.encoding)
				}
				if got.Revision != revision {
					t.Fatalf("loaded revision %d, saved %d", got.Revision, revision)
				}
			})
		}
		if supported == 0 {
			t.Fatal("the store rejected every encoding; it supports no checkpoint codec at all")
		}
	})

	t.Run("the encoding must be stated", func(t *testing.T) {
		store := factory()
		fixture := checkpointFixtures(t, "state")[0]
		// There is no default. A store that treats the zero value as V1
		// recreates the defect this field was added to remove.
		_, err := store.SaveCheckpoint(context.Background(), persistence.SaveCheckpointRequest{
			DocumentID: "doc", Update: fixture.update, StateVector: fixture.vector,
		})
		if !errors.Is(err, persistence.ErrEncodingRequired) {
			t.Fatalf("SaveCheckpoint without an encoding = %v, want ErrEncodingRequired", err)
		}
	})

	t.Run("a save replaces rather than accumulates", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		usable := acceptedFixtures(t, factory, "state-one")[0]
		first, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Encoding: usable.encoding, Update: usable.update, StateVector: usable.vector,
		})
		if err != nil {
			t.Fatal(err)
		}
		next := acceptedFixtures(t, factory, "state-two")[0]
		second, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Encoding: next.encoding, Update: next.update, StateVector: next.vector,
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
		if string(got.Update) != string(next.update) || got.Revision != second {
			t.Fatalf("after replacement the update is not the last saved one (rev %d, want %d)", got.Revision, second)
		}
	})

	t.Run("returned bytes are caller-owned", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		saved := acceptedFixtures(t, factory, "state-one")[0]
		if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Encoding: saved.encoding, Update: saved.update, StateVector: saved.vector,
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
		if string(again.Update) != string(saved.update) {
			t.Fatal("durable update changed through a returned alias")
		}
		// The vector need not be the bytes supplied — a store may derive it —
		// but it must describe the stored update.
		if string(again.StateVector) != string(saved.vector) {
			t.Fatalf("StateVector does not describe the stored update:\n got %x\nwant %x", again.StateVector, saved.vector)
		}
	})

	t.Run("documents are isolated", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		want := map[backend.DocumentID]checkpointFixture{}
		for _, id := range []backend.DocumentID{"alpha", "beta"} {
			fixture := acceptedFixtures(t, factory, "state-"+string(id))[0]
			want[id] = fixture
			if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
				DocumentID: id, Encoding: fixture.encoding, Update: fixture.update, StateVector: fixture.vector,
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range []backend.DocumentID{"alpha", "beta"} {
			got, err := store.LoadCheckpoint(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Update) != string(want[id].update) {
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
		usable := acceptedFixtures(t, factory, "state")[0]
		_, err := store.SaveCheckpoint(context.Background(), persistence.SaveCheckpointRequest{
			DocumentID: "doc", Fence: 1, Encoding: usable.encoding, Update: usable.update, StateVector: usable.vector,
		})
		if !errors.Is(err, persistence.ErrUnexpectedFence) {
			t.Fatalf("fenced write to an unfenced store = %v, want ErrUnexpectedFence", err)
		}
	})

	t.Run("cancelled calls do not succeed", func(t *testing.T) {
		store := factory()
		usable := acceptedFixtures(t, factory, "state")[0]
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Encoding: usable.encoding, Update: usable.update, StateVector: usable.vector,
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
		usable := acceptedFixtures(t, func() persistence.CheckpointStore { return factory() }, "state")[0]
		if mode := store.FenceMode(); mode != persistence.Fenced {
			t.Fatalf("fenced checkpoint factory mode = %d, want Fenced", mode)
		}
		ctx := context.Background()
		save := func(fence backend.Fence, state string) error {
			fixture := fixtureIn(t, usable.encoding, state)
			_, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
				DocumentID: "doc", Fence: fence, Encoding: fixture.encoding,
				Update: fixture.update, StateVector: fixture.vector,
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
		if want := fixtureIn(t, usable.encoding, "two"); string(got.Update) != string(want.update) {
			t.Fatal("a rejected stale write changed the last accepted state")
		}
	})

	t.Run("loads do not require a fence", func(t *testing.T) {
		store := factory()
		usable := acceptedFixtures(t, func() persistence.CheckpointStore { return factory() }, "state")[0]
		ctx := context.Background()
		if _, err := store.SaveCheckpoint(ctx, persistence.SaveCheckpointRequest{
			DocumentID: "doc", Fence: 1, Encoding: usable.encoding, Update: usable.update, StateVector: usable.vector,
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
