//go:build structstoreoracle

package crdt

// exerciseClientStructTreeDifferentialLifecycle makes the reference-compared direction-B surface
// cross every hybrid lifecycle boundary. It is tag-only: default production remains unable to
// activate the tree. One ContentAny is split into alternating live/deleted pieces, which activates
// and grows a multi-level tree; deleting the survivors lets cleanup coalesce the pieces and
// deactivates it again. The root remains in the update so yjs decodes and re-encodes the exact
// bytes produced through all of those transitions.
func exerciseClientStructTreeDifferentialLifecycle(seed int, doc *Doc) error {
	if seed != 1 {
		return nil
	}
	// Use an isolated source client. If these edits shared doc's client, the unrelated direction-B
	// structs would keep that list above the deactivation threshold even after this run coalesced.
	source := newDoc("__struct_tree_lifecycle", false, defaultGCFilter, nil, false, WithClientID(777))
	array := source.GetArray("__struct_tree_lifecycle")
	values := make(ArrayAny, 32)
	for i := range values {
		values[i] = i
	}
	array.Insert(0, values)
	for index := 30; index >= 0; index -= 2 {
		array.Delete(Number(index), 1)
	}
	array.Delete(0, array.GetLength())
	update, err := EncodeStateAsUpdateV2(source, nil)
	if err != nil {
		return err
	}
	return ApplyUpdateV2(doc, update, nil)
}
