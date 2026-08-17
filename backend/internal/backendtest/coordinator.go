package backendtest

import (
	"context"
	"sync"
	"time"

	"github.com/antst/go-yjs/backend"
	"github.com/antst/go-yjs/backend/cluster"
)

// Coordinator is a deterministic cluster contract fixture.
type Coordinator struct {
	mu     sync.Mutex
	now    time.Time
	next   map[backend.DocumentID]backend.Fence
	owners map[backend.DocumentID]cluster.Lease
}

// NewCoordinator constructs a fixture at a fixed initial time.
func NewCoordinator() *Coordinator {
	return &Coordinator{
		now:    time.Unix(1_700_000_000, 0),
		next:   make(map[backend.DocumentID]backend.Fence),
		owners: make(map[backend.DocumentID]cluster.Lease),
	}
}

func (c *Coordinator) Acquire(ctx context.Context, document backend.DocumentID, node backend.NodeID, ttl time.Duration) (cluster.Lease, error) {
	if err := ctx.Err(); err != nil {
		return cluster.Lease{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if current, ok := c.owners[document]; ok && c.now.Before(current.ExpiresAt) {
		return cluster.Lease{}, cluster.ErrLeaseHeld
	}
	c.next[document]++
	lease := cluster.Lease{DocumentID: document, NodeID: node, Fence: c.next[document], ExpiresAt: c.now.Add(ttl)}
	c.owners[document] = lease
	return lease, nil
}

func (c *Coordinator) Renew(ctx context.Context, lease cluster.Lease, ttl time.Duration) (cluster.Lease, error) {
	if err := ctx.Err(); err != nil {
		return cluster.Lease{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.owners[lease.DocumentID]
	if !ok || current.NodeID != lease.NodeID || current.Fence != lease.Fence || !c.now.Before(current.ExpiresAt) {
		return cluster.Lease{}, cluster.ErrLeaseLost
	}
	current.ExpiresAt = c.now.Add(ttl)
	c.owners[lease.DocumentID] = current
	return current, nil
}

func (c *Coordinator) Release(ctx context.Context, lease cluster.Lease) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.owners[lease.DocumentID]
	if !ok {
		return nil
	}
	if current.NodeID != lease.NodeID || current.Fence != lease.Fence {
		return cluster.ErrLeaseLost
	}
	delete(c.owners, lease.DocumentID)
	return nil
}

func (c *Coordinator) Locate(ctx context.Context, document backend.DocumentID) (cluster.Lease, error) {
	if err := ctx.Err(); err != nil {
		return cluster.Lease{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.owners[document]
	if !ok || !c.now.Before(current.ExpiresAt) {
		return cluster.Lease{}, cluster.ErrNotFound
	}
	return current, nil
}

// Advance moves the fixture clock without sleeping.
func (c *Coordinator) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

var _ cluster.Coordinator = (*Coordinator)(nil)
