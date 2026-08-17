// Package memory defines document registry and lifecycle contracts and ships a
// supported single-process implementation.
package memory

import (
	"context"
	"errors"
	"sync"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/crdt"
)

var (
	// ErrClosed reports use of a closed registry.
	ErrClosed = errors.New("memory: registry closed")
	// ErrInUse reports an eviction or close that would invalidate a handed-out
	// document.
	ErrInUse = errors.New("memory: document in use")
	// ErrInvalidDocument reports an empty ID or an initializer returning nil.
	ErrInvalidDocument = errors.New("memory: invalid document")
)

// OpenFunc constructs and restores a document after a registry cache miss. The
// registry coalesces concurrent calls for the same document ID, but deliberately
// does not prescribe how persistence is loaded.
type OpenFunc func(context.Context) (*crdt.Doc, error)

// Handle keeps one acquired document alive until Release. Release is
// idempotent. A session must stop reading or mutating Doc and release the
// handle when Done closes: the registry has poisoned this instance and will
// reopen the document from persistence for new acquisitions. Done is a
// cooperative signal, not capability revocation: a holder that ignores it can
// keep using a *crdt.Doc it already obtained, but doing so violates this
// contract and may silently diverge from durable state.
type Handle interface {
	Doc() *crdt.Doc
	Done() <-chan struct{}
	Release()
}

// Registry owns in-process document identity and teardown.
type Registry interface {
	Acquire(context.Context, backend.DocumentID, OpenFunc) (Handle, error)
	Evict(backend.DocumentID) error
	Invalidate(context.Context, backend.DocumentID) error
	Close() error
}

// InProcessRegistry is the supported single-process Registry implementation.
// It starts no goroutines. Evict never invalidates an outstanding Handle;
// Invalidate is the explicit recovery path that signals and drains them.
type InProcessRegistry struct {
	mu       sync.Mutex
	closed   bool
	entries  map[backend.DocumentID]*entry
	draining map[*entry]struct{}
}

type entry struct {
	ready       chan struct{}
	invalidated chan struct{}
	done        chan struct{}
	doc         *crdt.Doc
	err         error
	refs        int
	closing     bool
	poisoned    bool
	destroying  bool
}

type handle struct {
	registry *InProcessRegistry
	entry    *entry
	doc      *crdt.Doc
	once     sync.Once
}

// NewRegistry constructs an empty in-process registry.
func NewRegistry() *InProcessRegistry {
	return &InProcessRegistry{
		entries:  make(map[backend.DocumentID]*entry),
		draining: make(map[*entry]struct{}),
	}
}

// Doc returns the acquired document. It remains valid until Release unless
// Done closes first, in which case the caller must stop serving it and release.
func (h *handle) Doc() *crdt.Doc { return h.doc }

// Done closes when the acquired document is invalidated. It remains open for a
// normally released or evicted document.
func (h *handle) Done() <-chan struct{} { return h.entry.invalidated }

// Release relinquishes this acquisition. It is safe to call more than once.
func (h *handle) Release() {
	h.once.Do(func() {
		h.registry.mu.Lock()
		if h.entry.refs <= 0 {
			h.registry.mu.Unlock()
			panic("memory: released handle without a reference")
		}
		h.entry.refs--
		doc, finish := h.registry.takePoisonedDestroyLocked(h.entry)
		h.registry.mu.Unlock()
		if finish {
			h.registry.finishPoisoned(h.entry, doc)
		}
	})
}

// Acquire returns the one live document for id, coalescing concurrent cache
// misses into one call to open. Callers waiting for another initializer may
// return early when their own context is cancelled without cancelling that
// initializer.
func (r *InProcessRegistry) Acquire(ctx context.Context, id backend.DocumentID, open OpenFunc) (Handle, error) {
	if id == "" || open == nil {
		return nil, ErrInvalidDocument
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, ErrClosed
		}
		if current := r.entries[id]; current != nil {
			if current.closing {
				done := current.done
				r.mu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			select {
			case <-current.ready:
				if current.poisoned {
					r.mu.Unlock()
					continue
				}
				if current.err != nil {
					r.mu.Unlock()
					return nil, current.err
				}
				current.refs++
				doc := current.doc
				r.mu.Unlock()
				return &handle{registry: r, entry: current, doc: doc}, nil
			default:
				ready := current.ready
				r.mu.Unlock()
				select {
				case <-ready:
					r.mu.Lock()
					poisoned := current.poisoned
					waitErr := current.err
					r.mu.Unlock()
					if poisoned {
						continue
					}
					if waitErr != nil {
						return nil, waitErr
					}
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}

		created := &entry{
			ready: make(chan struct{}), invalidated: make(chan struct{}), done: make(chan struct{}),
		}
		r.entries[id] = created
		r.mu.Unlock()

		doc, err := open(ctx)
		if err == nil && doc == nil {
			err = ErrInvalidDocument
		}

		r.mu.Lock()
		created.doc = doc
		created.err = err
		close(created.ready)
		if created.poisoned {
			doc, finish := r.takePoisonedDestroyLocked(created)
			r.mu.Unlock()
			if finish {
				r.finishPoisoned(created, doc)
			}
			continue
		}
		if err != nil {
			if r.entries[id] == created {
				delete(r.entries, id)
			}
			close(created.done)
		} else {
			created.refs = 1
		}
		r.mu.Unlock()

		if err != nil {
			return nil, err
		}
		return &handle{registry: r, entry: created, doc: doc}, nil
	}
}

// Invalidate poisons the current document instance, signals every outstanding
// Handle through Done, and waits for those handles to release before destroying
// the instance. The poison is installed before it waits: a concurrent Acquire
// opens or joins a fresh instance and can never receive the stale one.
//
// Invalidation cannot revoke a *crdt.Doc already returned by Handle.Doc.
// Holders must observe Handle.Done and stop serving the stale document; a
// holder that ignores the signal violates the contract and may silently
// diverge from durable state.
//
// Context cancellation bounds the wait but does not undo poisoning. The last
// releasing handle still completes destruction, so recovery cannot silently
// revert to serving the stale document.
func (r *InProcessRegistry) Invalidate(ctx context.Context, id backend.DocumentID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return ErrClosed
	}
	current := r.entries[id]
	if current == nil {
		r.mu.Unlock()
		return nil
	}
	// Evict has already made this idle generation unavailable to Acquire and
	// owns its destruction. Treat that as successful invalidation rather than
	// racing a second Destroy call against it.
	if current.closing {
		r.mu.Unlock()
		return nil
	}
	current.closing = true
	current.poisoned = true
	delete(r.entries, id)
	r.draining[current] = struct{}{}
	close(current.invalidated)
	doc, finish := r.takePoisonedDestroyLocked(current)
	done := current.done
	r.mu.Unlock()
	if finish {
		r.finishPoisoned(current, doc)
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *InProcessRegistry) takePoisonedDestroyLocked(current *entry) (*crdt.Doc, bool) {
	if !current.poisoned || current.destroying || current.refs != 0 {
		return nil, false
	}
	select {
	case <-current.ready:
	default:
		return nil, false
	}
	current.destroying = true
	return current.doc, true
}

func (r *InProcessRegistry) finishPoisoned(current *entry, doc *crdt.Doc) {
	if doc != nil {
		doc.Destroy()
	}
	r.mu.Lock()
	delete(r.draining, current)
	close(current.done)
	r.mu.Unlock()
}

// Evict destroys an idle document. It returns ErrInUse rather than waiting or
// invalidating a live Handle.
func (r *InProcessRegistry) Evict(id backend.DocumentID) error {
	r.mu.Lock()
	current := r.entries[id]
	if current == nil {
		r.mu.Unlock()
		return nil
	}
	select {
	case <-current.ready:
	default:
		r.mu.Unlock()
		return ErrInUse
	}
	if current.refs != 0 || current.closing {
		r.mu.Unlock()
		return ErrInUse
	}
	current.closing = true
	r.mu.Unlock()

	current.doc.Destroy()

	r.mu.Lock()
	if r.entries[id] == current {
		delete(r.entries, id)
	}
	close(current.done)
	r.mu.Unlock()
	return nil
}

// Close destroys every idle document and rejects future acquisition. If any
// document is initializing or acquired it returns ErrInUse and changes no
// state.
func (r *InProcessRegistry) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	if len(r.draining) != 0 {
		r.mu.Unlock()
		return ErrInUse
	}
	for _, current := range r.entries {
		select {
		case <-current.ready:
		default:
			r.mu.Unlock()
			return ErrInUse
		}
		if current.refs != 0 || current.closing {
			r.mu.Unlock()
			return ErrInUse
		}
	}
	r.closed = true
	entries := r.entries
	for _, current := range entries {
		current.closing = true
	}
	r.mu.Unlock()

	for _, current := range entries {
		current.doc.Destroy()
	}

	r.mu.Lock()
	for id, current := range entries {
		delete(r.entries, id)
		close(current.done)
	}
	r.mu.Unlock()
	return nil
}
