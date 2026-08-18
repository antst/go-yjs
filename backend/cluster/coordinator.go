// Package cluster defines optional multi-node document ownership. A complete
// single-process backend does not need to import or implement this package.
package cluster

import (
	"context"
	"errors"
	"time"

	"github.com/antst/go-yjs/backend"
)

var (
	// ErrLeaseHeld reports a document currently owned by another live lease.
	ErrLeaseHeld = errors.New("cluster: lease held")
	// ErrLeaseLost reports an expired or superseded lease.
	ErrLeaseLost = errors.New("cluster: lease lost")
	// ErrNotFound reports that a document currently has no owner.
	ErrNotFound = errors.New("cluster: owner not found")
)

// Lease is one node's time-bounded authority over a document. Renewals retain
// the same Fence. A later acquisition after expiry must return a strictly newer
// Fence.
type Lease struct {
	DocumentID backend.DocumentID
	NodeID     backend.NodeID
	Fence      backend.Fence
	ExpiresAt  time.Time
}

// Coordinator assigns optional multi-node document ownership.
//
// A lease holder must stop serving writes when its lease expires or renewal
// fails. Because a partitioned holder can remain alive, durable mutations also
// carry Lease.Fence and a FENCED persistence store provides the final
// stale-owner rejection.
//
// THAT SECOND LAYER IS NOT ALWAYS THERE. A store reports Unfenced when its
// medium has nowhere durable to keep a per-document epoch — a bare content blob
// is the common case. Against such a store the lease is the ONLY thing standing
// between a partitioned holder and a write, so "stop serving when the lease is
// lost" stops being defence in depth and becomes the whole defence. Check
// FenceMode before assuming a backstop exists, and shed clients on lease loss
// rather than merely declining to write: a node that keeps serving reads and
// presence while silently failing to persist is split-brain the CRDT cannot
// resolve for you.
type Coordinator interface {
	Acquire(context.Context, backend.DocumentID, backend.NodeID, time.Duration) (Lease, error)
	Renew(context.Context, Lease, time.Duration) (Lease, error)
	Release(context.Context, Lease) error
	Locate(context.Context, backend.DocumentID) (Lease, error)
}
