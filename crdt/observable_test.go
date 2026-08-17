package crdt

import (
	"sync"
	"sync/atomic"
	"testing"
)

// ---------------------------------------------------------------- from observable_concurrency_test.go
// Regression for the round-2 review finding: two concurrent Emit calls for the same
// event both snapshotted the handler set before either removed the Once handler, so
// the once-handler fired more than once. Emit now "claims" a once-handler via Off's
// return, so it fires exactly once even under concurrent emission (the reaper + a
// consumer can both Emit the same event). Stress over many trials/goroutines.
func TestObservableOnceFiresExactlyOnceUnderConcurrentEmit(t *testing.T) {
	for trial := 0; trial < 300; trial++ {
		o := NewObservable()
		var count int32
		o.Once("e", NewObserverHandler(func(...interface{}) { atomic.AddInt32(&count, 1) }))

		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				o.Emit("e")
			}()
		}
		wg.Wait()

		if c := atomic.LoadInt32(&count); c != 1 {
			t.Fatalf("trial %d: once-handler fired %d times, want exactly 1", trial, c)
		}
	}
}

// ---------------------------------------------------------------- from observable_count_test.go
func TestObservableCountTracksRegistrationSemantics(t *testing.T) {
	o := NewObservable()
	firstCalls := 0
	first := NewObserverHandler(func(...interface{}) { firstCalls++ })
	second := NewObserverHandler(func(...interface{}) {})

	if o.HasObservers() || o.HasObserver("event") {
		t.Fatal("new observable reports handlers")
	}
	o.On("event", first)
	o.On("event", first) // Set semantics: duplicate registration is one handler.
	if got := o.observerCount.Load(); got != 1 {
		t.Fatalf("duplicate registration count = %d, want 1", got)
	}
	o.On("event", second)
	if !o.HasObservers() || !o.HasObserver("event") || o.HasObserver("missing") {
		t.Fatal("registered-handler lookup is inconsistent")
	}
	if o.Off("event", NewObserverHandler(func(...interface{}) {})) {
		t.Fatal("removing an unknown handler reported success")
	}
	if got := o.observerCount.Load(); got != 2 {
		t.Fatalf("unknown removal changed count to %d", got)
	}
	if !o.Off("event", first) || o.Off("event", first) {
		t.Fatal("handler removal did not preserve Set semantics")
	}
	if got := o.observerCount.Load(); got != 1 {
		t.Fatalf("single removal count = %d, want 1", got)
	}
	if !o.Off("event", second) || o.HasObservers() || o.HasObserver("event") {
		t.Fatal("removing the final handler did not restore the empty fast path")
	}
	if firstCalls != 0 {
		t.Fatalf("registration bookkeeping invoked callback %d times", firstCalls)
	}
}

func TestObservableCountTracksOnceAndDestroy(t *testing.T) {
	o := NewObservable()
	calls := 0
	once := NewObserverHandler(func(...interface{}) { calls++ })
	o.Once("once", once)
	o.Emit("once")
	o.Emit("once")
	if calls != 1 || o.HasObservers() || o.observerCount.Load() != 0 {
		t.Fatalf("once handler: calls=%d has=%v count=%d", calls, o.HasObservers(), o.observerCount.Load())
	}

	o.On("a", NewObserverHandler(func(...interface{}) {}))
	o.On("b", NewObserverHandler(func(...interface{}) {}))
	o.Destroy()
	if o.HasObservers() || o.HasObserver("a") || o.observerCount.Load() != 0 {
		t.Fatal("Destroy did not clear observer state and count")
	}
}

// ---------------------------------------------------------------- from observable_doc_race_test.go
// Regression for the full-review finding: the transaction update/updateV2 emit fast
// path read the doc's observer map directly, racing a concurrent doc.On/Off on the same
// map (now guarded via Observable.HasObserver). Under -race this is clean with the
// fix; pre-fix the direct map read races On/Off ("concurrent map read and map write").
func TestDocObserverRaceWithTransactions(t *testing.T) {
	doc := newDoc("g", false, nil, nil, false, WithClientID(1))
	m := doc.GetMap("m")

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// committer: runs transactions (each commit hits the update-emit fast path)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			m.Set("k", i)
		}
	}()

	// subscriber: churns update/updateV2 observers concurrently
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			h := NewObserverHandler(func(...interface{}) {})
			doc.On("update", h)
			doc.On("updateV2", h)
			doc.Off("update", h)
			doc.Off("updateV2", h)
		}
		close(stop)
	}()

	wg.Wait()
}
