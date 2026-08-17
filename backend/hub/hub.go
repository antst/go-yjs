// Package hub defines ephemeral in-process or distributed fan-out and ships a
// supported in-process default. Durable completeness comes from persistence,
// not from this contract.
package hub

import (
	"context"
	"errors"
	"sync"

	"github.com/antst/go-yjs/backend"
)

var (
	// ErrClosed reports use of a closed hub or subscription.
	ErrClosed = errors.New("hub: closed")
	// ErrInvalidMessage reports an unknown message kind or empty document ID.
	ErrInvalidMessage = errors.New("hub: invalid message")
)

// MessageKind separates durable document updates from ephemeral awareness.
// They deliberately do not share recovery or persistence semantics.
type MessageKind uint8

const (
	DocumentUpdate MessageKind = iota + 1
	AwarenessUpdate
)

// Message is a transport-neutral fan-out value.
//
// Payload passed to Publish is borrowed only until Publish returns. Payload
// received by a Handler is owned by that invocation and may be retained.
type Message struct {
	DocumentID backend.DocumentID
	SourceID   backend.SourceID
	Kind       MessageKind
	Payload    []byte
}

// Handler consumes one fan-out message. Returning an error applies explicit
// backpressure to synchronous implementations such as InProcess.
type Handler func(context.Context, Message) error

// Subscription is an active fan-out registration.
type Subscription interface {
	SourceID() backend.SourceID
	Close() error
}

// Hub is ephemeral fan-out.
//
// Publish success means that the hub accepted a message, not that every remote
// subscriber received it. Implementations may duplicate or reorder delivery,
// and disconnected subscribers may miss messages. Applications must recover
// completeness through persistence and state-vector catch-up. Implementations
// must honor SourceID echo suppression and must not silently discard a message
// merely because an active local subscriber queue is full; they apply
// backpressure or report an error instead.
type Hub interface {
	Subscribe(context.Context, backend.DocumentID, backend.SourceID, Handler) (Subscription, error)
	Publish(context.Context, Message) error
	Close() error
}

// InProcess is the supported single-process Hub implementation. Delivery is
// synchronous and applies backpressure through Handler results. Callers must
// not depend on its current ordering or single-delivery behavior because the
// Hub contract deliberately promises neither.
type InProcess struct {
	mu     sync.Mutex
	closed bool
	nextID uint64
	subs   map[backend.DocumentID]map[uint64]*inProcessSubscription
}

// NewInProcess constructs an empty in-process hub.
func NewInProcess() *InProcess {
	return &InProcess{subs: make(map[backend.DocumentID]map[uint64]*inProcessSubscription)}
}

type inProcessSubscription struct {
	hub      *InProcess
	id       uint64
	document backend.DocumentID
	source   backend.SourceID
	handler  Handler
	once     sync.Once
}

// SourceID returns the logical source excluded from its own publications.
func (s *inProcessSubscription) SourceID() backend.SourceID { return s.source }

// Close removes the subscription. It is safe to call more than once.
func (s *inProcessSubscription) Close() error {
	s.once.Do(func() {
		s.hub.mu.Lock()
		defer s.hub.mu.Unlock()
		if subscriptions := s.hub.subs[s.document]; subscriptions != nil {
			delete(subscriptions, s.id)
			if len(subscriptions) == 0 {
				delete(s.hub.subs, s.document)
			}
		}
	})
	return nil
}

// Subscribe registers a handler for one document.
func (h *InProcess) Subscribe(ctx context.Context, document backend.DocumentID, source backend.SourceID, handler Handler) (Subscription, error) {
	if document == "" || handler == nil {
		return nil, ErrInvalidMessage
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	h.nextID++
	subscription := &inProcessSubscription{
		hub: h, id: h.nextID, document: document, source: source, handler: handler,
	}
	if h.subs[document] == nil {
		h.subs[document] = make(map[uint64]*inProcessSubscription)
	}
	h.subs[document][subscription.id] = subscription
	return subscription, nil
}

// Publish synchronously delivers a private payload copy to every active
// subscriber for the document except subscribers with the same non-empty
// SourceID.
func (h *InProcess) Publish(ctx context.Context, message Message) error {
	if message.DocumentID == "" || (message.Kind != DocumentUpdate && message.Kind != AwarenessUpdate) {
		return ErrInvalidMessage
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrClosed
	}
	subscriptions := make([]*inProcessSubscription, 0, len(h.subs[message.DocumentID]))
	for _, subscription := range h.subs[message.DocumentID] {
		if message.SourceID != "" && subscription.source == message.SourceID {
			continue
		}
		subscriptions = append(subscriptions, subscription)
	}
	h.mu.Unlock()

	var deliveryErrors []error
	for _, subscription := range subscriptions {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(deliveryErrors, err)...)
		}
		delivered := message
		delivered.Payload = append([]byte(nil), message.Payload...)
		if err := subscription.handler(ctx, delivered); err != nil {
			deliveryErrors = append(deliveryErrors, err)
		}
	}
	return errors.Join(deliveryErrors...)
}

// Close removes all subscriptions and rejects future use. It is idempotent.
func (h *InProcess) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	h.closed = true
	h.subs = nil
	return nil
}
