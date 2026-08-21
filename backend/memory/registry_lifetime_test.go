package memory

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/antst/go-yjs/crdt"
)

// These are white-box on purpose. The properties are about who OWNS an
// in-flight initialization, which a black-box caller cannot observe without
// racing; here the waiter count is visible, so the tests are deterministic
// rather than timing-dependent.

// closeRegistry asserts the shutdown succeeded. Close reports ErrInUse when a
// document is still acquired or initializing, which in these tests means a
// generation leaked rather than that shutdown is merely inconvenient.
func closeRegistry(t *testing.T, r *InProcessRegistry) {
	t.Helper()
	if err := r.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// waitForWaiters blocks until the "doc" entry has the expected waiter count, so
// a test never has to sleep and hope.
func waitForWaiters(t *testing.T, r *InProcessRegistry, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r.mu.Lock()
		e := r.entries["doc"]
		got := 0
		if e != nil {
			got = e.waiters
		}
		r.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// One caller cancelling must not cancel an initialization others are waiting
// on. Before this, initialization belonged to whichever caller arrived first,
// so an arbitrary participant's deadline governed everyone.
func TestOneCallersCancellationDoesNotCancelASharedOpen(t *testing.T) {
	registry := NewRegistry()
	defer closeRegistry(t, registry)

	entered := make(chan struct{})
	release := make(chan struct{})
	var opens int
	var mu sync.Mutex
	openSeenErr := make(chan error, 1)

	var open OpenFunc = func(ctx context.Context) (*crdt.Doc, error) {
		mu.Lock()
		opens++
		mu.Unlock()
		close(entered)
		select {
		case <-release:
			openSeenErr <- ctx.Err()
			return crdt.NewDoc("doc"), nil
		case <-ctx.Done():
			openSeenErr <- ctx.Err()
			return nil, ctx.Err()
		}
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	aErr := make(chan error, 1)
	go func() { _, err := registry.Acquire(ctxA, "doc", open); aErr <- err }()
	<-entered
	waitForWaiters(t, registry, 1)

	bHandle := make(chan Handle, 1)
	bErr := make(chan error, 1)
	go func() {
		h, err := registry.Acquire(context.Background(), "doc", open)
		bHandle <- h
		bErr <- err
	}()
	waitForWaiters(t, registry, 2)

	cancelA()
	if err := <-aErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller got %v, want context.Canceled", err)
	}
	waitForWaiters(t, registry, 1)

	// The open must still be live: B is waiting on it.
	close(release)
	if err := <-openSeenErr; err != nil {
		t.Fatalf("the shared open saw %v; one caller's cancellation reached it", err)
	}
	if err := <-bErr; err != nil {
		t.Fatalf("the surviving waiter got %v, want a handle", err)
	}
	h := <-bHandle
	if h == nil {
		t.Fatal("the surviving waiter got no handle")
	}
	h.Release()

	mu.Lock()
	defer mu.Unlock()
	if opens != 1 {
		t.Fatalf("open called %d times; the second caller did not join the first's generation", opens)
	}
}

// When the last waiter abandons, the open is cancelled — and a document it
// produces ANYWAY must be destroyed rather than cached. The success case is the
// one that matters: an open far enough along to return a document does not have
// to honour a late cancellation, and caching that document would hand the next
// caller state loaded for a request that no longer exists.
func TestAbandonedInitializationIsCancelledAndNotCached(t *testing.T) {
	registry := NewRegistry()
	defer closeRegistry(t, registry)

	entered := make(chan struct{})
	sawCancel := make(chan error, 1)
	destroyed := make(chan struct{})
	var once sync.Once
	var opens int
	var mu sync.Mutex

	var open OpenFunc = func(ctx context.Context) (*crdt.Doc, error) {
		mu.Lock()
		opens++
		first := opens == 1
		mu.Unlock()
		if !first {
			return crdt.NewDoc("doc"), nil
		}
		close(entered)
		<-ctx.Done()
		sawCancel <- ctx.Err()
		doc := crdt.NewDoc("doc")
		// Registered before returning, so the registry cannot destroy it before
		// the observer is in place.
		doc.On("destroyed", crdt.NewObserverHandler(func(...interface{}) {
			once.Do(func() { close(destroyed) })
		}))
		return doc, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := registry.Acquire(ctx, "doc", open); done <- err }()
	<-entered
	waitForWaiters(t, registry, 1)

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoning caller got %v", err)
	}
	if err := <-sawCancel; !errors.Is(err, context.Canceled) {
		t.Fatalf("open saw %v after its last waiter left, want context.Canceled", err)
	}

	select {
	case <-destroyed:
	case <-time.After(5 * time.Second):
		t.Fatal("the document from an abandoned initialization was cached, not destroyed")
	}

	// And a later acquisition starts a FRESH open rather than inheriting it.
	h, err := registry.Acquire(context.Background(), "doc", open)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	mu.Lock()
	defer mu.Unlock()
	if opens != 2 {
		t.Fatalf("open called %d times; the abandoned generation was reused", opens)
	}
}

// Invalidate must stop an initialization it has already decided to discard, and
// must not return until that open has actually finished. Without the cancel it
// waits on an open nothing will ever complete, which is a deadlock rather than a
// slow path — so this bounds the wait instead of hanging the suite.
//
// The caller does NOT fail: a poisoned generation sends it back around the loop
// to open a fresh one, which is the documented behaviour and is why open here
// has to serve a second call.
func TestInvalidateCancelsAnInitializingGeneration(t *testing.T) {
	registry := NewRegistry()
	defer closeRegistry(t, registry)

	entered := make(chan struct{})
	seen := make(chan error, 1)
	var openReturned atomic.Bool
	var opens int
	var mu sync.Mutex
	var open OpenFunc = func(ctx context.Context) (*crdt.Doc, error) {
		mu.Lock()
		opens++
		first := opens == 1
		mu.Unlock()
		if !first {
			return crdt.NewDoc("doc"), nil
		}
		close(entered)
		<-ctx.Done()
		seen <- ctx.Err()
		openReturned.Store(true)
		return nil, ctx.Err()
	}

	acquired := make(chan Handle, 1)
	acquireErr := make(chan error, 1)
	go func() {
		h, err := registry.Acquire(context.Background(), "doc", open)
		acquired <- h
		acquireErr <- err
	}()
	<-entered

	invalidated := make(chan error, 1)
	go func() { invalidated <- registry.Invalidate(context.Background(), "doc") }()

	select {
	case err := <-invalidated:
		if err != nil {
			t.Fatalf("Invalidate during initialization = %v", err)
		}
		if !openReturned.Load() {
			t.Fatal("Invalidate returned while the initialization was still running")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Invalidate did not cancel the initialization it was discarding")
	}

	if err := <-seen; !errors.Is(err, context.Canceled) {
		t.Fatalf("open saw %v, want context.Canceled", err)
	}

	// The caller retried onto a fresh generation rather than receiving the
	// discarded one, and must be drained before shutdown.
	if err := <-acquireErr; err != nil {
		t.Fatalf("caller got %v; a poisoned generation should send it to a fresh one", err)
	}
	h := <-acquired
	if h == nil {
		t.Fatal("caller got no handle after retrying")
	}
	h.Release()
	mu.Lock()
	defer mu.Unlock()
	if opens != 2 {
		t.Fatalf("open called %d times, want 2 (discarded, then fresh)", opens)
	}
}

// A caller whose context is cancelled when its outcome is decided gets that
// cancellation, not a handle.
//
// This calls finalizeLocked directly, and that is the point rather than a
// shortcut. The guarantee is about ONE decision made under r.mu, and the window
// cannot be reached through Acquire at all: Acquire rechecks ctx.Err() at the top
// of every iteration, so an already-cancelled caller returns long before the
// select. Driving it from outside would mean racing a nanosecond gap and calling
// whatever came out deterministic. Entering the decision in exactly the guarded
// state needs no race and no repetition — remove the ctx check and this fails
// every time, not most of the time.
func TestCancelledCallerIsNotHandedAHandleAtFinalization(t *testing.T) {
	registry := NewRegistry()
	defer closeRegistry(t, registry)

	var open OpenFunc = func(context.Context) (*crdt.Doc, error) { return crdt.NewDoc("doc"), nil }
	held, err := registry.Acquire(context.Background(), "doc", open)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()

	registry.mu.Lock()
	e := registry.entries["doc"]
	registry.mu.Unlock()
	if e == nil {
		t.Fatal("no ready generation to finalize against")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	registry.mu.Lock()
	e.waiters++
	handle, err, retry := registry.finalizeLocked(ctx, "doc", e)
	refs, waiters := e.refs, e.waiters
	registry.mu.Unlock()

	if retry {
		t.Fatal("unexpected poison")
	}
	if handle != nil {
		handle.Release()
		t.Fatal("a cancelled caller was handed a handle")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if refs != 1 {
		t.Fatalf("refs = %d, want 1 (only the handle held by this test)", refs)
	}
	if waiters != 0 {
		t.Fatalf("waiters = %d, want 0; the rejected waiter was not released", waiters)
	}
}

// The public counterpart: an already-cancelled caller gets an error from
// Acquire. This is satisfied by the top-of-loop check rather than by
// finalizeLocked, so it is a contract test and NOT the isolator for the defect
// above — both are needed and neither substitutes for the other.
func TestAcquireRejectsAnAlreadyCancelledCaller(t *testing.T) {
	registry := NewRegistry()
	defer closeRegistry(t, registry)

	var opens int
	var open OpenFunc = func(context.Context) (*crdt.Doc, error) {
		opens++
		return crdt.NewDoc("doc"), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h, err := registry.Acquire(ctx, "doc", open)
	if err == nil {
		h.Release()
		t.Fatal("Acquire succeeded for a cancelled caller")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if opens != 0 {
		t.Fatalf("open ran %d times for a caller that had already given up", opens)
	}
}

// A generation whose last waiter left is CANCELLED, so it must also stop being
// joinable in the same instant. Otherwise the next caller attaches to an
// initialization that can no longer succeed: it is failed by a context that was
// never its own, or — if the open ignores the late cancellation — handed exactly
// the document that "destroy rather than cache" exists to prevent.
//
// This is the case the abandonment test cannot see, because that one waits for
// the discarded generation to finish before acquiring again. Here B arrives
// while the abandoned open is still running.
func TestArrivingCallerDoesNotJoinAnAbandonedGeneration(t *testing.T) {
	registry := NewRegistry()
	defer closeRegistry(t, registry)

	firstEntered := make(chan struct{})
	firstMayReturn := make(chan struct{})
	secondOpened := make(chan struct{})
	var opens int
	var mu sync.Mutex

	var open OpenFunc = func(ctx context.Context) (*crdt.Doc, error) {
		mu.Lock()
		opens++
		first := opens == 1
		mu.Unlock()
		if !first {
			close(secondOpened)
			return crdt.NewDoc("second"), nil
		}
		close(firstEntered)
		<-ctx.Done()
		// Deliberately succeeds despite the cancellation: an open far enough
		// along to return a document is exactly the case where the abandoned
		// generation could be handed to somebody.
		<-firstMayReturn
		return crdt.NewDoc("first"), nil
	}

	ctxA, cancelA := context.WithCancel(context.Background())
	aDone := make(chan error, 1)
	go func() { _, err := registry.Acquire(ctxA, "doc", open); aDone <- err }()
	<-firstEntered
	waitForWaiters(t, registry, 1)

	cancelA()
	if err := <-aDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoning caller got %v", err)
	}

	// A is gone and the first open is still running, blocked on firstMayReturn.
	// B must not be able to see that generation at all.
	registry.mu.Lock()
	stillMapped := registry.entries["doc"] != nil
	registry.mu.Unlock()
	if stillMapped {
		t.Fatal("the abandoned generation is still joinable while its open runs")
	}

	bHandle := make(chan Handle, 1)
	bErr := make(chan error, 1)
	go func() {
		h, err := registry.Acquire(context.Background(), "doc", open)
		bHandle <- h
		bErr <- err
	}()

	// B's own open must start without waiting for the abandoned one.
	select {
	case <-secondOpened:
	case <-time.After(5 * time.Second):
		t.Fatal("B joined the abandoned generation instead of opening a fresh one")
	}

	close(firstMayReturn)
	if err := <-bErr; err != nil {
		t.Fatalf("B got %v; another caller's cancellation reached it", err)
	}
	h := <-bHandle
	if h == nil {
		t.Fatal("B got no handle")
	}
	if got := h.Doc().GUID; got != "second" {
		t.Fatalf("B received the document %q from the abandoned generation", got)
	}
	h.Release()

	// The orphan must be fully drained before shutdown, or Close reports it.
	deadline := time.Now().Add(5 * time.Second)
	for {
		registry.mu.Lock()
		n := len(registry.draining)
		registry.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d abandoned generation(s) never finished draining", n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A generation that becomes ready while every waiter walks away has still never
// been acquired, so it must not settle into the cache. This is abandonment
// reached from the other side: runOpen has already passed its own checks by the
// time the last waiter leaves, so only the drop can catch it.
//
// The test makes ITSELF the second waiter. That is what makes the ordering
// deterministic: the generation can be brought to ready with a waiter still
// registered, and the last-waiter departure then happens at a point the test
// chooses rather than one it has to race for. Driving this through two ordinary
// callers would mean hoping the cancel lands between ready closing and the
// waiter claiming — an unobservable window, and an earlier version of this test
// that tried it failed about half the time.
func TestReadyButNeverClaimedGenerationIsNotCached(t *testing.T) {
	registry := NewRegistry()
	defer closeRegistry(t, registry)

	release := make(chan struct{})
	destroyed := make(chan struct{})
	var once sync.Once
	var opens int
	var mu sync.Mutex
	var open OpenFunc = func(context.Context) (*crdt.Doc, error) {
		mu.Lock()
		opens++
		n := opens
		mu.Unlock()
		if n > 1 {
			return crdt.NewDoc("doc"), nil
		}
		<-release
		doc := crdt.NewDoc("doc")
		doc.On("destroyed", crdt.NewObserverHandler(func(...interface{}) {
			once.Do(func() { close(destroyed) })
		}))
		return doc, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := registry.Acquire(ctx, "doc", open); done <- err }()
	waitForWaiters(t, registry, 1)

	registry.mu.Lock()
	e := registry.entries["doc"]
	e.waiters++ // the test joins as a second waiter
	registry.mu.Unlock()
	waitForWaiters(t, registry, 2)

	// The ordinary caller leaves while the open is still running. One waiter
	// remains — this test — so the generation survives and completes.
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller got %v, want context.Canceled", err)
	}
	waitForWaiters(t, registry, 1)

	close(release)
	<-e.ready
	registry.mu.Lock()
	readyAndCached := registry.entries["doc"] == e && !e.claimed
	registry.mu.Unlock()
	if !readyAndCached {
		t.Fatal("setup failed: wanted a ready, unclaimed, still-mapped generation")
	}

	// Now the last waiter leaves too, at a point of this test's choosing. The
	// call sequence below is exactly awaitEntry's ready branch.
	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()
	registry.mu.Lock()
	handle, err, retry := registry.finalizeLocked(expired, "doc", e)
	doc, finish := registry.takePoisonedDestroyLocked(e)
	registry.mu.Unlock()
	if finish {
		registry.finishPoisoned(e, doc)
	}
	if retry || handle != nil {
		t.Fatalf("last waiter got handle=%v retry=%v, want neither", handle, retry)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}

	registry.mu.Lock()
	cached := registry.entries["doc"] != nil
	registry.mu.Unlock()
	if cached {
		t.Fatal("a generation nobody ever acquired was left in the cache")
	}
	select {
	case <-destroyed:
	case <-time.After(5 * time.Second):
		t.Fatal("the unclaimed document was not destroyed")
	}

	h, err := registry.Acquire(context.Background(), "doc", open)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	mu.Lock()
	defer mu.Unlock()
	if opens != 2 {
		t.Fatalf("open called %d times; the unclaimed generation was reused", opens)
	}
}

type openCtxKey struct{}

// What the generation context carries is a contract two ways, and both halves
// have already caused a bug somewhere. Dropping the caller's VALUES silently
// reroutes a tenant-scoped or credential-scoped load. Keeping the caller's
// DEADLINE hands one arbitrary participant's clock to every other waiter, which
// is the defect this whole change exists to fix.
func TestOpenContextKeepsCallerValuesAndDropsCallerDeadline(t *testing.T) {
	registry := NewRegistry()
	defer closeRegistry(t, registry)

	type observed struct {
		value       any
		hasDeadline bool
	}
	seen := make(chan observed, 1)
	var open OpenFunc = func(ctx context.Context) (*crdt.Doc, error) {
		_, hasDeadline := ctx.Deadline()
		seen <- observed{value: ctx.Value(openCtxKey{}), hasDeadline: hasDeadline}
		return crdt.NewDoc("doc"), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	ctx = context.WithValue(ctx, openCtxKey{}, "tenant-a")

	h, err := registry.Acquire(ctx, "doc", open)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()

	got := <-seen
	if got.value != "tenant-a" {
		t.Fatalf("open saw value %v; the caller's context values were dropped, so a scoped load would silently go elsewhere", got.value)
	}
	if got.hasDeadline {
		t.Fatal("open inherited the caller's deadline; one participant's clock would govern every other waiter")
	}
}

// A failed open must retire its generation exactly once. runOpen already deletes
// the entry and closes done on the error path, so the last waiter leaving with
// that error must not also treat it as an abandoned generation — doing both
// closes done twice. The registry's own conformance suite caught this; the
// package had no test where an open FAILS with a waiter still present.
func TestFailedOpenRetiresItsGenerationOnce(t *testing.T) {
	registry := NewRegistry()
	defer closeRegistry(t, registry)

	openErr := errors.New("load failed")
	release := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	var opens int
	var mu sync.Mutex
	var open OpenFunc = func(context.Context) (*crdt.Doc, error) {
		mu.Lock()
		opens++
		n := opens
		mu.Unlock()
		if n > 1 {
			return crdt.NewDoc("doc"), nil
		}
		once.Do(func() { close(entered) })
		<-release
		return nil, openErr
	}

	// Two waiters, so the last one out is not the same caller that created the
	// generation — that is the ordering the double-close needs.
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := registry.Acquire(context.Background(), "doc", open)
			errs <- err
		}()
	}
	<-entered
	waitForWaiters(t, registry, 2)

	close(release)
	for range 2 {
		if err := <-errs; !errors.Is(err, openErr) {
			t.Fatalf("waiter got %v, want the open's error", err)
		}
	}

	registry.mu.Lock()
	cached, draining := registry.entries["doc"] != nil, len(registry.draining)
	registry.mu.Unlock()
	if cached {
		t.Fatal("a failed generation was left in the cache")
	}
	if draining != 0 {
		t.Fatalf("draining = %d after a failed open, want 0", draining)
	}

	// A failure must not poison the document: the next caller retries.
	h, err := registry.Acquire(context.Background(), "doc", open)
	if err != nil {
		t.Fatalf("retry after a failed open = %v", err)
	}
	defer h.Release()
}

// A waiter must not be handed a generation that was retired while it waited.
// Evict and Close destroy a ready generation without poisoning it, and a waiter
// holds its entry pointer across the wait — so without a liveness check it takes
// a reference to a destroyed document, and the next acquisition opens a
// different one. Two callers would then be mutating different documents under
// the same ID.
//
// Driven through finalizeLocked because the window is between observing ready
// and acquiring the lock, which no external caller can be parked inside.
func TestWaiterDoesNotReceiveAGenerationRetiredWhileItWaited(t *testing.T) {
	registry := NewRegistry()
	defer closeRegistry(t, registry)

	var opens int
	var open OpenFunc = func(context.Context) (*crdt.Doc, error) {
		opens++
		return crdt.NewDoc("doc"), nil
	}

	h, err := registry.Acquire(context.Background(), "doc", open)
	if err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	e := registry.entries["doc"]
	registry.mu.Unlock()
	h.Release()

	// Park a waiter on the ready generation, exactly where one would sit.
	registry.mu.Lock()
	e.waiters++
	registry.mu.Unlock()

	if err := registry.Evict("doc"); err != nil {
		t.Fatalf("Evict = %v", err)
	}

	registry.mu.Lock()
	handle, err, retry := registry.finalizeLocked(context.Background(), "doc", e)
	registry.mu.Unlock()
	if handle != nil {
		handle.Release()
		t.Fatal("waiter received a handle to a generation that had already been evicted and destroyed")
	}
	if err != nil {
		t.Fatalf("waiter got %v, want a retry", err)
	}
	if !retry {
		t.Fatal("waiter was not sent back to look for a live generation")
	}

	next, err := registry.Acquire(context.Background(), "doc", open)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release()
	if opens != 2 {
		t.Fatalf("open called %d times, want 2 (the evicted one, then a fresh one)", opens)
	}
}
