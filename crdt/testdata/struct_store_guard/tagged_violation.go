//go:build structstoreoracle

package guardfixture

func taggedRepresentationAccess(list *clientStructList) int {
	return len(list.items)
}
