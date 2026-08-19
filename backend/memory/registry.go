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
//
// THE CONTEXT IS THE REGISTRY'S, NOT ANY ONE ACQUIRER'S. It carries the values
// of the caller whose OpenFunc won the race — that caller supplied this function,
// so its values are the ones this function was written for — but it carries NO
// deadline, because a deadline belonging to one arbitrary participant would
// govern every other waiter too.
//
// It is cancelled when the last waiter stops waiting, or when the generation is
// invalidated. That bounds an open nobody wants any more; it does NOT bound an
// open that somebody is still waiting for.
//
// Note what that means for a document under load: every new acquirer that joins
// renews the condition, so a steady stream of arrivals can keep the waiter count
// above zero indefinitely and this context is then never cancelled. A busy
// document during a store outage is exactly that case, and it is exactly when
// the store is wedged. An implementation that must fail a hung load within a
// fixed time MUST impose that bound itself, derived from this context so the
// values survive; nothing here will do it.
//
// The context must not be retained or used after OpenFunc returns.
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
// Acquire runs each initialization on its own goroutine so that no single
// caller's context governs it; those goroutines are the only ones it starts, and
// Close reports ErrInUse rather than returning while one is still running.
// Evict never invalidates an outstanding Handle;
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
	// openCtx belongs to the GENERATION, not to whichever caller happened to
	// arrive first. Binding initialization to an arbitrary participant's
	// deadline meant that caller's cancellation was returned to every other
	// waiter, including ones whose own context was still live.
	//
	// It is cancelled when the generation is invalidated, when the registry
	// closes, or when the last waiter abandons — so nothing is uncancellable,
	// but no single caller owns it either. Re-electing a waiter was considered
	// and is impossible: a running call's context cannot be transplanted.
	openCtx    context.Context
	openCancel context.CancelFunc
	// waiters counts callers currently awaiting ready. At zero there is nobody
	// left who wants this document, so the open is cancelled and a doc it
	// nevertheless produces is destroyed rather than cached — this API promises
	// no warming.
	waiters int
	// claimed records that some caller has taken a reference to this
	// generation. It does NOT track current references: a generation whose
	// handles have all been released is still a legitimate cache entry, while
	// one nobody ever acquired is work for a request that no longer exists.
	claimed    bool
	doc        *crdt.Doc
	err        error
	refs       int
	closing    bool
	poisoned   bool
	destroying bool
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
// misses into one call to open.
//
// Initialization belongs to the GENERATION, not to whichever caller happened to
// arrive first. open runs under a context this registry owns, so one caller's
// deadline or cancellation never governs the others: a cancelled caller returns
// its own ctx.Err() and the open continues for whoever is still waiting. The
// open is cancelled only when the LAST waiter leaves, or when Invalidate or
// Close discards the generation. ctx therefore bounds this call, not the work.
//
// open may observe that cancellation and return anyway. A generation whose last
// waiter left is detached from the registry at that moment, so no later caller
// can join it, and whatever it produces is destroyed rather than cached — the
// next caller opens a fresh generation instead of receiving state loaded for a
// request that no longer exists.
//
// A caller whose context is cancelled at the point this call decides its outcome
// receives that cancellation and no handle, even if the document became ready in
// the same instant. Cancellation after that point races a decision already made
// and may still return a handle: a context and a registry share no atomic
// boundary, so no implementation can promise more than this.
//
// open must not retain or use the context it is given after it returns; see
// OpenFunc for what that context does and does not carry.
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
			// One wait path for joiners and for the creator alike, so the
			// caller-context recheck and the waiter accounting cannot diverge
			// between them.
			current.waiters++
			r.mu.Unlock()
			handle, err, retry := r.awaitEntry(ctx, id, current)
			if retry {
				continue
			}
			return handle, err
		}

		created := &entry{
			ready: make(chan struct{}), invalidated: make(chan struct{}), done: make(chan struct{}),
		}
		// WithoutCancel, not Background: the generation must not inherit this
		// caller's deadline (it would govern every other waiter), but it must
		// keep its VALUES. open was supplied by this caller, so tenant scope,
		// credentials and trace context are the ones it was written against;
		// silently swapping them for an empty context routes loads elsewhere
		// with nothing to report it.
		created.openCtx, created.openCancel = context.WithCancel(context.WithoutCancel(ctx))
		// The creator counts as a waiter from the outset, so an open that
		// finishes after the creator has walked away sees zero and discards.
		created.waiters = 1
		r.entries[id] = created
		r.mu.Unlock()

		// Off this goroutine deliberately: the creator must be able to select on
		// its OWN context while the open runs, exactly as a joiner does.
		go r.runOpen(id, created, open)

		handle, err, retry := r.awaitEntry(ctx, id, created)
		if retry {
			continue
		}
		return handle, err
	}
}

// awaitEntry waits for a generation on the CALLER's context. The caller must
// already be counted in e.waiters; this releases that count on every exit.
//
// Returns retry=true when the generation was poisoned while we waited, which
// means the caller should look again rather than receive a stale document.
func (r *InProcessRegistry) awaitEntry(ctx context.Context, id backend.DocumentID, e *entry) (Handle, error, bool) {
	select {
	case <-e.ready:
		r.mu.Lock()
		handle, err, retry := r.finalizeLocked(ctx, id, e)
		doc, finish := r.takePoisonedDestroyLocked(e)
		r.mu.Unlock()
		if finish {
			r.finishPoisoned(e, doc)
		}
		return handle, err, retry
	case <-ctx.Done():
		r.mu.Lock()
		r.dropWaiterLocked(id, e)
		doc, finish := r.takePoisonedDestroyLocked(e)
		r.mu.Unlock()
		if finish {
			r.finishPoisoned(e, doc)
		}
		return nil, ctx.Err(), false
	}
}

// finalizeLocked decides one waiter's outcome once the generation is ready. It
// is the single point at which a reference can be taken, which is what makes the
// context rule testable: everything it decides, it decides under r.mu.
//
// The caller's own cancellation is checked FIRST and decides alone. A select
// whose ready and ctx.Done cases are both live picks between them at random, so
// without this a caller that had already gone away would be handed a working
// handle half the time. It also outranks this generation's error: a caller that
// stopped waiting wants its own reason, not one it no longer cares about.
//
// Cancellation is only observable up to this point. A context cancelled after
// finalizeLocked returns races a success that has already happened, and this
// call reports the success — no shared boundary exists between a context and a
// registry, so no implementation can promise otherwise.
func (r *InProcessRegistry) finalizeLocked(ctx context.Context, id backend.DocumentID, e *entry) (Handle, error, bool) {
	if err := ctx.Err(); err != nil {
		r.dropWaiterLocked(id, e)
		return nil, err, false
	}
	if e.poisoned {
		r.dropWaiterLocked(id, e)
		return nil, nil, true
	}
	if e.err != nil {
		r.dropWaiterLocked(id, e)
		return nil, e.err, false
	}
	// Evict and Close retire a READY generation without poisoning it, and a
	// waiter holds its entry pointer across the wait rather than re-reading the
	// map. Without this check such a waiter takes a reference to a document that
	// has already been destroyed and is no longer the registry's instance for
	// this ID — two callers then mutating different documents under one ID,
	// which is the exact thing this registry exists to prevent. Retrying sends
	// the caller back to the map, where it finds a fresh generation or none.
	// Ordered after e.err so a failed open still reports its own failure.
	if e.closing {
		r.dropWaiterLocked(id, e)
		return nil, nil, true
	}
	// Claim BEFORE releasing the waiter count. In the other order the drop sees
	// a generation with no waiters and no references and discards the very
	// document this caller is about to take.
	e.claimed = true
	e.refs++
	r.dropWaiterLocked(id, e)
	return &handle{registry: r, entry: e, doc: e.doc}, nil, false
}

// dropWaiterLocked releases one waiter and ABANDONS the generation when the last
// one leaves without anybody ever having acquired it.
//
// Cancelling the open is not enough on its own. A generation left in the map is
// still joinable, so the next caller attaches to an initialization that is
// already cancelled and is then failed by a context that was never its own — or,
// if the open ignores the late cancellation and succeeds, is handed exactly the
// document that "destroy rather than cache" exists to prevent. Detaching and
// cancelling must therefore happen in the same critical section.
//
// Abandonment is expressed as poisoning because that machinery already covers
// both orders: takePoisonedDestroyLocked finishes a generation that is already
// ready, and runOpen finishes one whose open is still running. Membership in
// draining is what stops Close reporting success while that goroutine is still
// owned by this registry.
func (r *InProcessRegistry) dropWaiterLocked(id backend.DocumentID, e *entry) {
	if e.waiters == 0 {
		return
	}
	e.waiters--
	if e.waiters != 0 || e.refs != 0 || e.claimed || e.closing {
		return
	}
	e.closing = true
	e.poisoned = true
	if e.openCancel != nil {
		e.openCancel()
	}
	if r.entries[id] == e {
		delete(r.entries, id)
	}
	r.draining[e] = struct{}{}
}

// runOpen initialises a generation under the generation's own context.
func (r *InProcessRegistry) runOpen(id backend.DocumentID, created *entry, open OpenFunc) {
	doc, err := open(created.openCtx)
	if err == nil && doc == nil {
		err = ErrInvalidDocument
	}

	r.mu.Lock()
	created.doc = doc
	created.err = err
	close(created.ready)
	if created.poisoned {
		poisonedDoc, finish := r.takePoisonedDestroyLocked(created)
		r.mu.Unlock()
		if finish {
			r.finishPoisoned(created, poisonedDoc)
		}
		return
	}
	// No separate "nobody is waiting" case: a generation whose last waiter left
	// was already poisoned and detached by dropWaiterLocked, so the branch above
	// is the one that destroys it. Reaching here means somebody is still waiting.
	if err != nil {
		// closing marks this generation as already retired. Without it the last
		// waiter, on its way out with the error, would see a generation nobody
		// wants and abandon it a second time — closing done twice and panicking.
		// A failed open owns its own teardown; there is nothing left to abandon.
		created.closing = true
		if r.entries[id] == created {
			delete(r.entries, id)
		}
		close(created.done)
	}
	r.mu.Unlock()
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
//
// It does NOT wait for an abandoned initialization of the same document. Such a
// generation is already detached, cancelled, and destroyed on completion, so it
// can never publish to anyone; only its READ may still be in flight, and no
// caller's result depends on that having stopped. Close still refuses while any
// registry-owned goroutine remains.
//
// INVALIDATE IS NOT ENOUGH TO ERASE A DOCUMENT. It drains the CURRENT instance;
// a later Acquire opens a fresh one from persistence. So invalidating and then
// deleting durable state leaves a window where something re-acquires, loads the
// not-yet-deleted state, and saves it back after the delete lands. The content
// returns, nothing reports an error, and for an erasure request that is the
// worst available outcome.
//
// Erasure needs three steps in this order: stop admitting acquisitions for the
// document, in application state that OpenFunc consults — this registry has no
// concept of that and cannot supply it; then Invalidate; then delete durably.
// See persistence.Deleter.
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
	// An initialising generation must stop: without this, Invalidate waits for
	// an open it has already decided to discard, and Erase-style callers would
	// delete durable state while a load of it is still in flight.
	if current.openCancel != nil {
		current.openCancel()
	}
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
	// No entry can be initializing here: the ready check above ran in this same
	// critical section, and r.closed now turns away every later Acquire. Each
	// surviving entry's open context was already cancelled by its last departing
	// waiter, so there is nothing left to cancel.
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
