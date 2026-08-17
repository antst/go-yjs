package crdt

import (
	"sync"
	"sync/atomic"
)

type ObserverHandler struct {
	once     bool
	callback func(v ...interface{})
}

type Observable struct {
	// mu guards observers. Emission snapshots the handler set under the lock and
	// invokes callbacks AFTER releasing it (mirroring yjs emit's `Array.from(...)`),
	// so the awareness reaper goroutine can Emit concurrently with consumer
	// On/Off/Emit without racing the map, and an observer callback may re-enter
	// On/Off/Emit without deadlocking.
	mu sync.RWMutex
	// Unexported deliberately. observerCount below is only correct while every mutation goes
	// through On/Off/Destroy; a caller writing to this map directly would drift the count, and an
	// undercount makes HasObservers report false while handlers are registered — the transaction
	// then skips the changed-type journal, the update emission and the before/after callbacks, so
	// the document is right and nobody is told about it. Exporting the field would make that
	// silent failure reachable from outside the package for no benefit; nothing outside
	// observable.go ever needed it.
	observers map[interface{}]Set
	// observerCount lets the overwhelmingly common unobserved mutation path reject every named
	// event without taking mu or hashing an interface key. On/Off still own the map under mu; the
	// count is only the empty-map fast path and preserves the locked lookup once it is non-zero.
	observerCount atomic.Int64
}

func (o *Observable) On(name interface{}, handle *ObserverHandler) {
	o.mu.Lock()
	defer o.mu.Unlock()
	observers, exist := o.observers[name]
	if !exist {
		observers = NewSet()
		o.observers[name] = observers
	}
	if !observers.Has(handle) {
		observers.Add(handle)
		o.observerCount.Add(1)
	}
}

func (o *Observable) Once(name interface{}, handler *ObserverHandler) {
	handler.once = true
	o.On(name, handler)
}

// Off removes handler from name's observer set and reports whether it was present.
// The bool lets Emit atomically "claim" a Once handler, so two concurrent emits of
// the same event (e.g. the reaper and a consumer) invoke it exactly once.
func (o *Observable) Off(name interface{}, handler *ObserverHandler) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	observers, exist := o.observers[name]
	if !exist {
		return false
	}
	had := observers.Has(handler)
	observers.Delete(handler)
	if had {
		o.observerCount.Add(-1)
	}
	if len(observers) == 0 {
		delete(o.observers, name)
	}
	return had
}

func (o *Observable) Emit(name interface{}, v ...interface{}) {
	// Snapshot the handlers under the read lock, then release before invoking them
	// (yjs does `from(this._observers.get(name) || [])`). This keeps callbacks off
	// the lock so they may call On/Off/Emit re-entrantly, and makes concurrent
	// emission (the awareness reaper) safe against On/Off mutating the map.
	o.mu.RLock()
	observers, exist := o.observers[name]
	var handlers []*ObserverHandler
	if exist {
		handlers = make([]*ObserverHandler, 0, len(observers))
		for h := range observers {
			if handler, ok := h.(*ObserverHandler); ok {
				handlers = append(handlers, handler)
			}
		}
	}
	o.mu.RUnlock()

	for _, handler := range handlers {
		if handler.once {
			// Claim the once-handler: invoke it only if THIS emit actually removed it,
			// so two concurrent emits of the same event don't both fire it.
			if !o.Off(name, handler) {
				continue
			}
		}
		handler.callback(v...)
	}
}

// HasObserver reports whether any handler is registered for name, under the lock.
// Callers that previously read the observers map directly (e.g. the transaction
// update-emit fast path) must use this so they don't race On/Off/Destroy on the map.
func (o *Observable) HasObserver(name interface{}) bool {
	if o.observerCount.Load() == 0 {
		return false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	_, exist := o.observers[name]
	return exist
}

// HasObservers reports whether any event has a registered handler.
func (o *Observable) HasObservers() bool {
	return o.observerCount.Load() > 0
}

func (o *Observable) Destroy() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.observers = make(map[interface{}]Set)
	o.observerCount.Store(0)
}

func NewObservable() *Observable {
	return &Observable{
		observers: make(map[interface{}]Set),
	}
}

func NewObserverHandler(f func(v ...interface{})) *ObserverHandler {
	return &ObserverHandler{
		callback: f,
	}
}
