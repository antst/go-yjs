//go:build structstoreoracle

package crdt

import "sync/atomic"

const (
	clientStructTreeActivationLimit   = 8
	clientStructTreeDeactivationLimit = 4
	clientStructTreeHybridLeafLimit   = 4
	clientStructTreeHybridBranchLimit = 3
)

type clientStructTreeLifecycle struct {
	activations   atomic.Uint64
	deactivations atomic.Uint64
	leafSplits    atomic.Uint64
	branchSplits  atomic.Uint64
	rebalances    atomic.Uint64
}

var clientStructTreeLifecycleCounts clientStructTreeLifecycle

func noteClientStructTreeActivation()   { clientStructTreeLifecycleCounts.activations.Add(1) }
func noteClientStructTreeDeactivation() { clientStructTreeLifecycleCounts.deactivations.Add(1) }
func noteClientStructTreeLeafSplit()    { clientStructTreeLifecycleCounts.leafSplits.Add(1) }
func noteClientStructTreeBranchSplit()  { clientStructTreeLifecycleCounts.branchSplits.Add(1) }
func noteClientStructTreeRebalance()    { clientStructTreeLifecycleCounts.rebalances.Add(1) }
