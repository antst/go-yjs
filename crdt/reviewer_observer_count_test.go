package crdt

import (
	"fmt"
	"testing"
)

// The observer count gates DELIVERY, so drift is silent data loss.
//
// WHY THIS IS A SHARPER HAZARD THAN THE ITEM COUNTER. A wrong linked-item count mis-sizes a cache:
// slower, never wrong. A wrong observer count makes HasObservers return false while handlers are
// registered, and every caller then skips the work — transaction.go gates the changed-type journal,
// the update/updateV2 emission, and the before/after transaction callbacks on it. An undercount
// therefore does not produce a wrong document; it produces a document nobody is told about. No
// existing gate can see that, because the differential oracle compares documents, not notifications.
//
// Overcounting is harmless: HasObserver falls through to the unchanged locked lookup, so it costs a
// lock and returns the truth. Only undercounting loses events. Both are checked here anyway, since
// an overcount signals a leak that would eventually become a drift in the other direction.

// totalHandlers is the ground truth the counter must equal: the number of distinct handlers across
// every event name, computed from the map itself.
func totalHandlers(o *Observable) int64 {
	o.mu.RLock()
	defer o.mu.RUnlock()
	n := int64(0)
	for _, set := range o.observers {
		n += int64(len(set))
	}
	return n
}

func assertObserverCount(t *testing.T, o *Observable, label string) {
	t.Helper()
	got, want := o.observerCount.Load(), totalHandlers(o)
	if got != want {
		verdict := "OVERCOUNT (wasteful but safe)"
		if got < want {
			verdict = "UNDERCOUNT — registered handlers will be SILENTLY SKIPPED"
		}
		t.Fatalf("%s: observerCount=%d, actual handlers=%d — %s", label, got, want, verdict)
	}
	if (got > 0) != o.HasObservers() {
		t.Fatalf("%s: HasObservers()=%v disagrees with count %d", label, o.HasObservers(), got)
	}
}

// Randomized On/Off/Once/Emit/Destroy, checked after every single operation.
func TestObserverCountMatchesMapUnderRandomOps(t *testing.T) {
	names := []interface{}{"update", "updateV2", "destroy", "afterTransaction", 42}
	for seed := 0; seed < 400; seed++ {
		o := NewObservable()
		rng := markerLCG(uint32(seed*2654435761 + 17))
		var handlers []*ObserverHandler
		var handlerNames []interface{}

		for step := 0; step < 40; step++ {
			switch rng(6) {
			case 0, 1: // register, sometimes a DUPLICATE of an existing handler+name pair
				name := names[rng(len(names))]
				var h *ObserverHandler
				if len(handlers) > 0 && rng(3) == 0 {
					h = handlers[rng(len(handlers))] // duplicate registration must not double-count
				} else {
					h = NewObserverHandler(func(...interface{}) {})
				}
				o.On(name, h)
				handlers = append(handlers, h)
				handlerNames = append(handlerNames, name)
			case 2: // once
				name := names[rng(len(names))]
				h := NewObserverHandler(func(...interface{}) {})
				o.Once(name, h)
				handlers = append(handlers, h)
				handlerNames = append(handlerNames, name)
			case 3: // remove, sometimes one that was never registered under that name
				if len(handlers) > 0 {
					i := rng(len(handlers))
					name := handlerNames[i]
					if rng(4) == 0 {
						name = names[rng(len(names))]
					}
					o.Off(name, handlers[i])
				}
			case 4: // emit: consumes Once handlers, which must decrement through Off
				o.Emit(names[rng(len(names))])
			default:
				if rng(12) == 0 {
					o.Destroy()
					handlers, handlerNames = nil, nil
				}
			}
			assertObserverCount(t, o, fmt.Sprintf("seed %d step %d", seed, step))
		}
	}
}

// End-to-end: the counter is an optimization, so what must be defended is that events still ARRIVE.
// A pure counter test would pass on an implementation that never delivers anything.
func TestDocEventsStillFireThroughTheFastPath(t *testing.T) {
	for _, name := range []string{"update", "updateV2", "afterTransaction", "afterAllTransactions",
		"beforeAllTransactions", "beforeObserverCalls", "afterTransactionCleanup"} {
		t.Run(name, func(t *testing.T) {
			doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
			fired := 0
			doc.On(name, NewObserverHandler(func(...interface{}) { fired++ }))
			if !doc.HasObserver(name) {
				t.Fatalf("HasObserver(%q) false immediately after registering", name)
			}
			doc.GetText("t").Insert(0, "hello", Object{})
			if fired == 0 {
				t.Fatalf("%q never fired; the empty-observer fast path skipped a registered handler", name)
			}

			// After removal the fast path must engage again and stop delivery.
			before := fired
			doc.Destroy()
			doc2 := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
			doc2.GetText("t").Insert(0, "x", Object{})
			if fired != before {
				t.Fatalf("%q fired %d more times after Destroy", name, fired-before)
			}
		})
	}
}

// An observer registered DURING a live transaction must still be seen by the callback checks that
// run at cleanup. Capturing the count at transaction start would be a natural optimization and
// would silently break this.
func TestObserverRegisteredMidTransactionStillReceives(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	updates := 0
	Transact(doc, func(*Transaction) {
		doc.GetText("t").Insert(0, "a", Object{})
		// Registered after the transaction opened and after a mutation already happened.
		doc.On("update", NewObserverHandler(func(...interface{}) { updates++ }))
		doc.GetText("t").Insert(1, "b", Object{})
	}, nil, true)
	if updates == 0 {
		t.Fatal("an observer registered mid-transaction received no update event")
	}
}

// Deep type observers travel a different path (EH/DEH rather than Observable), so this confirms the
// doc-level fast path did not disturb them.
func TestTypeAndDeepObserversUnaffected(t *testing.T) {
	doc := newDoc("g", false, defaultGCFilter, nil, false, WithClientID(1))
	arr := doc.GetArray("a")
	shallow, deep := 0, 0
	arr.Observe(func(interface{}, interface{}) { shallow++ })
	arr.ObserveDeep(func(interface{}, interface{}) { deep++ })

	nested := NewYMap(nil)
	arr.Insert(0, ArrayAny{nested})
	if shallow == 0 {
		t.Fatal("shallow type observer never fired")
	}
	before := deep
	nested.Set("k", 1) // mutate the CHILD: only the deep observer should see it
	if deep == before {
		t.Fatal("deep observer never fired for a nested mutation")
	}
}

// Once must fire exactly once and leave the count at zero, including when emitted repeatedly.
func TestOnceFiresOnceAndClearsTheCount(t *testing.T) {
	o := NewObservable()
	fired := 0
	o.Once("e", NewObserverHandler(func(...interface{}) { fired++ }))
	assertObserverCount(t, o, "after Once")
	for i := 0; i < 5; i++ {
		o.Emit("e")
	}
	if fired != 1 {
		t.Fatalf("Once handler fired %d times", fired)
	}
	assertObserverCount(t, o, "after emits")
	if o.HasObservers() {
		t.Fatal("HasObservers still true after the only Once handler was consumed")
	}
}
