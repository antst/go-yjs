//go:build !structstoreoracle

package crdt

func exerciseClientStructTreeDifferentialLifecycle(_ int, _ *Doc) error { return nil }
