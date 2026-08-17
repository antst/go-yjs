// Package conformance provides public contract suites for backend
// implementations. Implementers run these suites against their own factories;
// the module's supported defaults run the same suites.
package conformance

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/memory"
	"github.com/antst/go-yjs/crdt"
)

// RegistryFactory constructs an isolated empty registry for one conformance
// subtest.
type RegistryFactory func() memory.Registry

// Memory runs the document-registry contract suite.
func Memory(t *testing.T, factory RegistryFactory) {
	t.Helper()
	t.Run("concurrent acquire coalesces initialization", func(t *testing.T) {
		registry := factory()
		var opens atomic.Int32
		started := make(chan struct{})
		proceed := make(chan struct{})
		open := func(context.Context) (*crdt.Doc, error) {
			if opens.Add(1) == 1 {
				close(started)
			}
			<-proceed
			return crdt.NewDoc("coalesced", crdt.WithGC(false)), nil
		}

		const callers = 8
		handles := make([]memory.Handle, callers)
		errs := make([]error, callers)
		var wait sync.WaitGroup
		wait.Add(callers)
		for i := range callers {
			go func(i int) {
				defer wait.Done()
				handles[i], errs[i] = registry.Acquire(context.Background(), "doc", open)
			}(i)
		}
		<-started
		close(proceed)
		wait.Wait()
		if got := opens.Load(); got != 1 {
			t.Fatalf("initializer calls = %d, want 1", got)
		}
		var doc *crdt.Doc
		for i := range callers {
			if errs[i] != nil {
				t.Fatalf("Acquire[%d]: %v", i, errs[i])
			}
			if doc == nil {
				doc = handles[i].Doc()
			} else if handles[i].Doc() != doc {
				t.Fatalf("Acquire[%d] returned a distinct document", i)
			}
			handles[i].Release()
			handles[i].Release()
		}
		if err := registry.Evict("doc"); err != nil {
			t.Fatal(err)
		}
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("failed initialization does not poison retry", func(t *testing.T) {
		registry := factory()
		failure := errors.New("load failed")
		if _, err := registry.Acquire(context.Background(), "doc", func(context.Context) (*crdt.Doc, error) {
			return nil, failure
		}); !errors.Is(err, failure) {
			t.Fatalf("first Acquire error = %v, want %v", err, failure)
		}
		handle, err := registry.Acquire(context.Background(), "doc", func(context.Context) (*crdt.Doc, error) {
			return crdt.NewDoc("retry", crdt.WithGC(false)), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		handle.Release()
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("eviction never invalidates a handle", func(t *testing.T) {
		registry := factory()
		var opens int
		open := func(context.Context) (*crdt.Doc, error) {
			opens++
			return crdt.NewDoc("lifecycle", crdt.WithGC(false)), nil
		}
		first, err := registry.Acquire(context.Background(), backend.DocumentID("doc"), open)
		if err != nil {
			t.Fatal(err)
		}
		var firstDestroyed int
		first.Doc().On("destroy", crdt.NewObserverHandler(func(...interface{}) { firstDestroyed++ }))
		if err := registry.Evict("doc"); !errors.Is(err, memory.ErrInUse) {
			t.Fatalf("Evict while acquired = %v, want ErrInUse", err)
		}
		if firstDestroyed != 0 {
			t.Fatal("Evict destroyed an acquired document")
		}
		first.Release()
		if err := registry.Evict("doc"); err != nil {
			t.Fatal(err)
		}
		if firstDestroyed != 1 {
			t.Fatalf("first document destroy count = %d, want 1", firstDestroyed)
		}
		if err := registry.Evict("doc"); err != nil {
			t.Fatal(err)
		}
		if firstDestroyed != 1 {
			t.Fatalf("repeat eviction destroy count = %d, want 1", firstDestroyed)
		}
		second, err := registry.Acquire(context.Background(), "doc", open)
		if err != nil {
			t.Fatal(err)
		}
		if opens != 2 {
			t.Fatalf("initializer calls after eviction = %d, want 2", opens)
		}
		if second.Doc() == first.Doc() {
			t.Fatal("eviction reused the destroyed document")
		}
		var secondDestroyed int
		second.Doc().On("destroy", crdt.NewObserverHandler(func(...interface{}) { secondDestroyed++ }))
		second.Release()
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
		if secondDestroyed != 1 {
			t.Fatalf("second document destroy count = %d, want 1", secondDestroyed)
		}
		if _, err := registry.Acquire(context.Background(), "doc", open); !errors.Is(err, memory.ErrClosed) {
			t.Fatalf("Acquire after Close = %v, want ErrClosed", err)
		}
	})

	t.Run("invalidation signals drains and diverts acquisition", func(t *testing.T) {
		registry := factory()
		var opens int
		open := func(context.Context) (*crdt.Doc, error) {
			opens++
			return crdt.NewDoc("generation", crdt.WithGC(false)), nil
		}
		stale, err := registry.Acquire(context.Background(), "doc", open)
		if err != nil {
			t.Fatal(err)
		}
		var staleDestroyed int
		stale.Doc().On("destroy", crdt.NewObserverHandler(func(...interface{}) { staleDestroyed++ }))

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		invalidated := make(chan error, 1)
		go func() { invalidated <- registry.Invalidate(ctx, "doc") }()
		select {
		case <-stale.Done():
		case <-ctx.Done():
			t.Fatal("Invalidate did not signal the stale handle")
		}
		select {
		case err := <-invalidated:
			t.Fatalf("Invalidate returned before the stale handle drained: %v", err)
		default:
		}

		fresh, err := registry.Acquire(ctx, "doc", open)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Doc() == stale.Doc() {
			t.Fatal("Acquire during invalidation returned the poisoned document")
		}
		if opens != 2 {
			t.Fatalf("initializer calls = %d, want a fresh generation", opens)
		}
		select {
		case <-fresh.Done():
			t.Fatal("fresh handle inherited the old generation's poison")
		default:
		}

		stale.Release()
		if err := <-invalidated; err != nil {
			t.Fatal(err)
		}
		if staleDestroyed != 1 {
			t.Fatalf("stale document destroy count = %d, want 1", staleDestroyed)
		}
		joined, err := registry.Acquire(ctx, "doc", open)
		if err != nil {
			t.Fatal(err)
		}
		if joined.Doc() != fresh.Doc() || opens != 2 {
			t.Fatal("post-invalidation Acquire did not join the fresh generation")
		}
		fresh.Release()
		joined.Release()
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cancelled drain remains poisoned", func(t *testing.T) {
		registry := factory()
		open := func(context.Context) (*crdt.Doc, error) {
			return crdt.NewDoc("cancelled-drain", crdt.WithGC(false)), nil
		}
		stale, err := registry.Acquire(context.Background(), "doc", open)
		if err != nil {
			t.Fatal(err)
		}
		var staleDestroyed int
		stale.Doc().On("destroy", crdt.NewObserverHandler(func(...interface{}) { staleDestroyed++ }))

		ctx, cancel := context.WithCancel(context.Background())
		invalidated := make(chan error, 1)
		go func() { invalidated <- registry.Invalidate(ctx, "doc") }()
		select {
		case <-stale.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("Invalidate did not poison before waiting")
		}
		cancel()
		if err := <-invalidated; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Invalidate = %v, want context.Canceled", err)
		}
		if err := registry.Close(); !errors.Is(err, memory.ErrInUse) {
			t.Fatalf("Close with a cancelled drain = %v, want ErrInUse", err)
		}
		fresh, err := registry.Acquire(context.Background(), "doc", open)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Doc() == stale.Doc() {
			t.Fatal("cancelled Invalidate made the poisoned document current again")
		}
		stale.Release()
		if staleDestroyed != 1 {
			t.Fatalf("stale document destroy count = %d after cancelled drain, want 1", staleDestroyed)
		}
		fresh.Release()
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("invalidation does not double-destroy an evicting generation", func(t *testing.T) {
		registry := factory()
		handle, err := registry.Acquire(context.Background(), "doc", func(context.Context) (*crdt.Doc, error) {
			return crdt.NewDoc("evict-race", crdt.WithGC(false)), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		destroyStarted := make(chan struct{})
		allowDestroy := make(chan struct{})
		var destroys atomic.Int32
		handle.Doc().On("destroy", crdt.NewObserverHandler(func(...interface{}) {
			if destroys.Add(1) == 1 {
				close(destroyStarted)
			}
			<-allowDestroy
		}))
		handle.Release()

		evicted := make(chan error, 1)
		go func() { evicted <- registry.Evict("doc") }()
		select {
		case <-destroyStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("Evict did not begin destruction")
		}
		if err := registry.Invalidate(context.Background(), "doc"); err != nil {
			t.Fatal(err)
		}
		close(allowDestroy)
		if err := <-evicted; err != nil {
			t.Fatal(err)
		}
		if got := destroys.Load(); got != 1 {
			t.Fatalf("destroy count after Evict/Invalidate race = %d, want 1", got)
		}
		if err := registry.Close(); err != nil {
			t.Fatal(err)
		}
	})
}
