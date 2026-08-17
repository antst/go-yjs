//go:build structstoreoracle

package crdt

import "testing"

func resetClientStructTreeGateLifecycle() {
	clientStructTreeLifecycleCounts.activations.Store(0)
	clientStructTreeLifecycleCounts.deactivations.Store(0)
	clientStructTreeLifecycleCounts.leafSplits.Store(0)
	clientStructTreeLifecycleCounts.branchSplits.Store(0)
	clientStructTreeLifecycleCounts.rebalances.Store(0)
}

func requireClientStructTreeGateLifecycle(t *testing.T) {
	t.Helper()
	counts := []struct {
		name  string
		value uint64
	}{
		{name: "activation", value: clientStructTreeLifecycleCounts.activations.Load()},
		{name: "deactivation", value: clientStructTreeLifecycleCounts.deactivations.Load()},
		{name: "leaf split", value: clientStructTreeLifecycleCounts.leafSplits.Load()},
		{name: "branch split", value: clientStructTreeLifecycleCounts.branchSplits.Load()},
		{name: "removal rebalance", value: clientStructTreeLifecycleCounts.rebalances.Load()},
	}
	for _, count := range counts {
		if count.value == 0 {
			t.Fatalf("struct-store oracle differential did not exercise %s", count.name)
		}
	}
}
