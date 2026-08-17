// Package backend defines the neutral identifiers shared by backend ports.
// It contains no CRDT values, wire frames, transports, or implementation
// policy.
package backend

// DocumentID identifies one collaborative document within a backend.
type DocumentID string

// SourceID identifies a logical update source for fan-out echo suppression.
// It is deliberately not a network connection or transport address.
type SourceID string

// NodeID identifies a logical backend node. Resolving a NodeID to a network
// endpoint is application and transport policy.
type NodeID string

// Fence is a monotonically increasing document-ownership epoch.
//
// Zero means that clustering is not in use. An unfenced persistence store
// accepts zero as the normal single-process mode. A fenced store rejects zero
// and older epochs, so accidentally dropping authority from one clustered
// write fails immediately instead of silently disabling stale-owner protection.
type Fence uint64

// Clustered reports whether a write is protected by a cluster fence.
func (f Fence) Clustered() bool { return f != 0 }
