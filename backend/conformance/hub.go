package conformance

import (
	"context"
	"errors"
	"testing"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/hub"
)

// HubFactory constructs an isolated empty hub for one conformance subtest.
type HubFactory func() hub.Hub

// Hub runs the ephemeral fan-out contract suite. It intentionally makes no
// assertion about cross-publication ordering or exactly-once delivery.
func Hub(t *testing.T, factory HubFactory) {
	t.Helper()
	t.Run("document routing and source exclusion", func(t *testing.T) {
		fanout := factory()
		defer func() { _ = fanout.Close() }()
		var source, peer, other int
		mustSubscribe(t, fanout, "doc", "source", func(context.Context, hub.Message) error {
			source++
			return nil
		})
		mustSubscribe(t, fanout, "doc", "peer", func(context.Context, hub.Message) error {
			peer++
			return nil
		})
		mustSubscribe(t, fanout, "other", "other", func(context.Context, hub.Message) error {
			other++
			return nil
		})
		if err := fanout.Publish(context.Background(), hub.Message{
			DocumentID: "doc", SourceID: "source", Kind: hub.DocumentUpdate, Payload: []byte("u"),
		}); err != nil {
			t.Fatal(err)
		}
		if source != 0 || peer != 1 || other != 0 {
			t.Fatalf("deliveries source=%d peer=%d other=%d, want 0/1/0", source, peer, other)
		}
	})

	t.Run("payload ownership", func(t *testing.T) {
		fanout := factory()
		defer func() { _ = fanout.Close() }()
		var retained []byte
		mustSubscribe(t, fanout, "doc", "peer", func(_ context.Context, message hub.Message) error {
			retained = message.Payload
			return nil
		})
		payload := []byte("owned")
		if err := fanout.Publish(context.Background(), hub.Message{
			DocumentID: "doc", SourceID: "source", Kind: hub.DocumentUpdate, Payload: payload,
		}); err != nil {
			t.Fatal(err)
		}
		payload[0] = 'X'
		if got := string(retained); got != "owned" {
			t.Fatalf("retained payload = %q after caller mutation", got)
		}
	})

	t.Run("backpressure errors are observable", func(t *testing.T) {
		fanout := factory()
		defer func() { _ = fanout.Close() }()
		failure := errors.New("subscriber failed")
		mustSubscribe(t, fanout, "doc", "peer", func(context.Context, hub.Message) error { return failure })
		err := fanout.Publish(context.Background(), hub.Message{DocumentID: "doc", Kind: hub.AwarenessUpdate})
		if !errors.Is(err, failure) {
			t.Fatalf("Publish error = %v, want subscriber failure", err)
		}
	})

	t.Run("close rejects future use", func(t *testing.T) {
		fanout := factory()
		if err := fanout.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := fanout.Subscribe(context.Background(), "doc", "peer", func(context.Context, hub.Message) error { return nil }); !errors.Is(err, hub.ErrClosed) {
			t.Fatalf("Subscribe after Close = %v, want ErrClosed", err)
		}
		if err := fanout.Publish(context.Background(), hub.Message{DocumentID: "doc", Kind: hub.DocumentUpdate}); !errors.Is(err, hub.ErrClosed) {
			t.Fatalf("Publish after Close = %v, want ErrClosed", err)
		}
	})

	t.Run("cancelled subscribe is rejected", func(t *testing.T) {
		fanout := factory()
		defer func() { _ = fanout.Close() }()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := fanout.Subscribe(ctx, "doc", "peer", func(context.Context, hub.Message) error { return nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("Subscribe cancelled = %v, want context.Canceled", err)
		}
	})
}

func mustSubscribe(t *testing.T, fanout hub.Hub, document backend.DocumentID, source backend.SourceID, handler hub.Handler) hub.Subscription {
	t.Helper()
	subscription, err := fanout.Subscribe(context.Background(), document, source, handler)
	if err != nil {
		t.Fatal(err)
	}
	return subscription
}
