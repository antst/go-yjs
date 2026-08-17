//go:build !structstoreoracle

package crdt

import "testing"

func resetClientStructTreeGateLifecycle()             {}
func requireClientStructTreeGateLifecycle(*testing.T) {}
