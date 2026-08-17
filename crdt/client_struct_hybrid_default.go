//go:build !structstoreoracle

package crdt

// Production activation is deliberately above the measured flat/tree
// crossover. At 6,419 physical structs the two representations are near parity;
// 8,192 keeps that row and the existing 3,237-struct real-caller delete fixture
// flat, while larger fragmented histories gain from logarithmic middle inserts.
const (
	clientStructTreeActivationLimit   = 8192
	clientStructTreeDeactivationLimit = 4096
	clientStructTreeHybridLeafLimit   = 64
	clientStructTreeHybridBranchLimit = 32
)

func noteClientStructTreeActivation()   {}
func noteClientStructTreeDeactivation() {}
func noteClientStructTreeLeafSplit()    {}
func noteClientStructTreeBranchSplit()  {}
func noteClientStructTreeRebalance()    {}
