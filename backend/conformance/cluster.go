package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/cluster"
)

// ClusterHarness provides an isolated coordinator and a deterministic advance
// operation for lease-expiry tests.
type ClusterHarness struct {
	Coordinator cluster.Coordinator
	Advance     func(time.Duration)
}

// ClusterFactory constructs one isolated deterministic cluster harness.
type ClusterFactory func() ClusterHarness

// Cluster runs the optional coordinator contract suite.
func Cluster(t *testing.T, factory ClusterFactory) {
	t.Helper()
	harness := factory()
	if harness.Coordinator == nil || harness.Advance == nil {
		t.Fatal("cluster conformance requires a coordinator and deterministic clock")
	}
	ctx := context.Background()
	first, err := harness.Coordinator.Acquire(ctx, "doc", "node-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fence == 0 {
		t.Fatal("clustered lease returned fence zero")
	}
	if _, err := harness.Coordinator.Acquire(ctx, "doc", "node-b", time.Minute); !errors.Is(err, cluster.ErrLeaseHeld) {
		t.Fatalf("competing Acquire = %v, want ErrLeaseHeld", err)
	}
	harness.Advance(time.Second)
	renewed, err := harness.Coordinator.Renew(ctx, first, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Fence != first.Fence || !renewed.ExpiresAt.After(first.ExpiresAt) {
		t.Fatalf("renewed lease = %#v, first = %#v", renewed, first)
	}
	harness.Advance(2 * time.Minute)
	second, err := harness.Coordinator.Acquire(ctx, "doc", backend.NodeID("node-b"), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second.Fence <= first.Fence {
		t.Fatalf("new fence = %d, want greater than %d", second.Fence, first.Fence)
	}
	if _, err := harness.Coordinator.Renew(ctx, first, time.Minute); !errors.Is(err, cluster.ErrLeaseLost) {
		t.Fatalf("stale Renew = %v, want ErrLeaseLost", err)
	}
	located, err := harness.Coordinator.Locate(ctx, "doc")
	if err != nil {
		t.Fatal(err)
	}
	if located.NodeID != second.NodeID || located.Fence != second.Fence {
		t.Fatalf("Locate = %#v, want %#v", located, second)
	}
	if err := harness.Coordinator.Release(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := harness.Coordinator.Release(ctx, second); err != nil {
		t.Fatalf("idempotent Release: %v", err)
	}
}
