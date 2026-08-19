package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/persistence"
)

// StoreFactory constructs an isolated empty persistence store for one
// conformance subtest.
type StoreFactory func() persistence.Store

// CompactingStoreFactory constructs an isolated empty checkpoint-capable store.
type CompactingStoreFactory func() persistence.CompactingStore

// FenceUpgradeFactory constructs unfenced and fenced Store views over the same
// durable data.
type FenceUpgradeFactory func() (unfenced persistence.Store, fenced persistence.Store)

// Persistence runs the required, non-clustered persistence contract. Fence
// zero is a first-class mode, not a degraded clustered configuration.
func Persistence(t *testing.T, factory StoreFactory) {
	t.Helper()
	if mode := factory().FenceMode(); mode != persistence.Unfenced {
		t.Fatalf("base persistence factory mode = %d, want Unfenced", mode)
	}
	t.Run("unclustered append load and ownership", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		first := []byte("first")
		revision1, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: first})
		if err != nil {
			t.Fatal(err)
		}
		first[0] = 'X'
		revision2, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("second")})
		if err != nil {
			t.Fatal(err)
		}
		if revision2 <= revision1 {
			t.Fatalf("revisions %d then %d are not increasing", revision1, revision2)
		}

		history := loadComplete(t, store, "doc", 1)
		if len(history) != 2 || string(history[0].Update) != "first" || string(history[1].Update) != "second" {
			t.Fatalf("history = %#v, want first/second", history)
		}
		history[0].Update[0] = 'Y'
		again := loadComplete(t, store, "doc", 1)
		if got := string(again[0].Update); got != "first" {
			t.Fatalf("durable update changed through returned alias: %q", got)
		}
	})

	t.Run("missing history is explicit", func(t *testing.T) {
		store := factory()
		_, err := store.Load(context.Background(), "missing", persistence.LoadOptions{})
		if !errors.Is(err, persistence.ErrNotFound) {
			t.Fatalf("Load missing = %v, want ErrNotFound", err)
		}
	})

	t.Run("pagination holds a fixed recovery view", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("first")}); err != nil {
			t.Fatal(err)
		}
		second, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("second")})
		if err != nil {
			t.Fatal(err)
		}
		firstPage, err := store.Load(ctx, "doc", persistence.LoadOptions{Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if firstPage.Through != second || firstPage.Next == "" || len(firstPage.Updates) != 1 {
			t.Fatalf("first page = %#v, want one update through %d and a continuation", firstPage, second)
		}
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("later")}); err != nil {
			t.Fatal(err)
		}
		continuation, err := store.Load(ctx, "doc", persistence.LoadOptions{PageToken: firstPage.Next, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if continuation.Through != second || continuation.Next != "" || len(continuation.Updates) != 1 || string(continuation.Updates[0].Update) != "second" {
			t.Fatalf("continuation after append = %#v, want only second through %d", continuation, second)
		}
		fresh := loadComplete(t, store, "doc", 1)
		if len(fresh) != 3 || string(fresh[2].Update) != "later" {
			t.Fatalf("fresh recovery view = %#v, want later append included", fresh)
		}
	})

	t.Run("unclustered mode rejects accidental authority", func(t *testing.T) {
		store := factory()
		_, err := store.Append(context.Background(), persistence.AppendRequest{DocumentID: "doc", Fence: 1, Update: []byte("x")})
		if !errors.Is(err, persistence.ErrUnexpectedFence) {
			t.Fatalf("Append fenced request to unfenced store = %v, want ErrUnexpectedFence", err)
		}
	})

	t.Run("cancelled calls do not succeed", func(t *testing.T) {
		store := factory()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("x")}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Append cancelled = %v, want context.Canceled", err)
		}
		if _, err := store.Load(ctx, "doc", persistence.LoadOptions{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Load cancelled = %v, want context.Canceled", err)
		}
	})
	persistenceConcurrency(t, factory)
}

// PersistenceCompaction runs the optional checkpoint and compaction contract.
func PersistenceCompaction(t *testing.T, factory CompactingStoreFactory) {
	t.Helper()
	t.Run("racing append survives compaction", func(t *testing.T) {
		store := factory()
		ctx := context.Background()
		_, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("first")})
		if err != nil {
			t.Fatal(err)
		}
		basis, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("second")})
		if err != nil {
			t.Fatal(err)
		}
		checkpoint := []byte("checkpoint")
		stateVector := []byte("state-vector")
		later, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("later")})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Compact(ctx, persistence.CompactRequest{
			DocumentID: "doc", Basis: basis, CheckpointUpdate: checkpoint, StateVector: stateVector,
		}); err != nil {
			t.Fatal(err)
		}
		checkpoint[0] = 'X'
		stateVector[0] = 'Y'

		page, err := store.Load(ctx, "doc", persistence.LoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if page.Checkpoint == nil || page.Checkpoint.Revision != basis || string(page.Checkpoint.Update) != "checkpoint" || string(page.Checkpoint.StateVector) != "state-vector" {
			t.Fatalf("checkpoint = %#v", page.Checkpoint)
		}
		if len(page.Updates) != 1 || page.Updates[0].Revision != later || string(page.Updates[0].Update) != "later" {
			t.Fatalf("tail = %#v, want only racing append", page.Updates)
		}
		page.Checkpoint.Update[0] = 'M'
		page.Checkpoint.StateVector[0] = 'N'
		again, err := store.Load(ctx, "doc", persistence.LoadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if string(again.Checkpoint.Update) != "checkpoint" || string(again.Checkpoint.StateVector) != "state-vector" {
			t.Fatalf("checkpoint changed through returned alias: %#v", again.Checkpoint)
		}
	})
	persistenceCompactionConcurrency(t, factory)
}

// PersistenceFencing runs the optional clustered-write contract. A backend
// that never imports cluster need not run this suite.
func PersistenceFencing(t *testing.T, factory StoreFactory) {
	t.Helper()
	t.Run("fenced mode rejects absent and stale authority", func(t *testing.T) {
		store := factory()
		if mode := store.FenceMode(); mode != persistence.Fenced {
			t.Fatalf("fenced persistence factory mode = %d, want Fenced", mode)
		}
		ctx := context.Background()
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("missing")}); !errors.Is(err, persistence.ErrFenceRequired) {
			t.Fatalf("Append without fence = %v, want ErrFenceRequired", err)
		}
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Fence: 1, Update: []byte("one")}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Fence: 2, Update: []byte("two")}); err != nil {
			t.Fatal(err)
		}
		if compactor, ok := store.(persistence.Compactor); ok {
			err := compactor.Compact(ctx, persistence.CompactRequest{
				DocumentID: "doc", Basis: 2, CheckpointUpdate: []byte("checkpoint"), StateVector: []byte("state-vector"),
			})
			if !errors.Is(err, persistence.ErrFenceRequired) {
				t.Fatalf("Compact without fence = %v, want ErrFenceRequired", err)
			}
		}
		for _, fence := range []backend.Fence{0, 1} {
			want := error(persistence.ErrStaleFence)
			if fence == 0 {
				want = persistence.ErrFenceRequired
			}
			if _, err := store.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Fence: fence, Update: []byte("stale")}); !errors.Is(err, want) {
				t.Fatalf("Append fence %d = %v, want %v", fence, err, want)
			}
		}
	})
	persistenceFencingConcurrency(t, factory)
}

// PersistenceFenceUpgrade proves that enabling clustering changes write
// authority without requiring durable history to be rewritten.
func PersistenceFenceUpgrade(t *testing.T, factory FenceUpgradeFactory) {
	t.Helper()
	unfenced, fenced := factory()
	if mode := unfenced.FenceMode(); mode != persistence.Unfenced {
		t.Fatalf("upgrade source mode = %d, want Unfenced", mode)
	}
	if mode := fenced.FenceMode(); mode != persistence.Fenced {
		t.Fatalf("upgrade target mode = %d, want Fenced", mode)
	}
	ctx := context.Background()
	if _, err := unfenced.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Update: []byte("before")}); err != nil {
		t.Fatal(err)
	}
	before := loadComplete(t, fenced, "doc", 1)
	if len(before) != 1 || string(before[0].Update) != "before" {
		t.Fatalf("fenced view of unfenced history = %#v", before)
	}
	if _, err := fenced.Append(ctx, persistence.AppendRequest{DocumentID: "doc", Fence: 1, Update: []byte("after")}); err != nil {
		t.Fatalf("first fenced append after upgrade: %v", err)
	}
	after := loadComplete(t, fenced, "doc", 1)
	if len(after) != 2 || string(after[0].Update) != "before" || string(after[1].Update) != "after" {
		t.Fatalf("upgraded history = %#v, want before/after", after)
	}
}

func loadComplete(t *testing.T, store persistence.Loader, document backend.DocumentID, limit int) []persistence.Record {
	t.Helper()
	var (
		token   persistence.PageToken
		through persistence.Revision
		result  []persistence.Record
	)
	for pages := 0; ; pages++ {
		if pages > 1000 {
			t.Fatal("Load did not terminate")
		}
		page, err := store.Load(context.Background(), document, persistence.LoadOptions{PageToken: token, Limit: limit})
		if err != nil {
			t.Fatal(err)
		}
		if limit > 0 && len(page.Updates) > limit {
			t.Fatalf("Load returned %d records with limit %d", len(page.Updates), limit)
		}
		if pages == 0 {
			through = page.Through
		} else if page.Through != through {
			t.Fatalf("continuation Through = %d, want fixed %d", page.Through, through)
		}
		result = append(result, page.Updates...)
		if page.Next == "" {
			return result
		}
		if page.Next == token {
			t.Fatalf("Load repeated page token %q", token)
		}
		token = page.Next
	}
}
