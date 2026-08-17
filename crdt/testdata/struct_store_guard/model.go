package guardfixture

type StructStore struct {
	clients map[int]*clientStructList
}

type clientStructList struct {
	items []int
}

type clientStructTree struct {
	root *clientStructTreeNode
}

type clientStructTreeNode struct{}

type clientStructTreeLeaf struct {
	items []int
}

type clientStructTreeBranch struct {
	children []*clientStructTreeNode
}
