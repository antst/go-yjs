package crdt

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	repoRootOnce sync.Once
	repoRootDir  string
	repoRootErr  error
)

// repoPath resolves a path relative to the REPOSITORY ROOT rather than to the
// package directory.
//
// WHY. Twelve test files reference 29 repository-root paths — fuzz generators,
// bench scripts, testdata fixtures, the pre-push hook — as plain relative
// strings like "fuzz/generate.js". Those work only because the package happens
// to sit at the repository root today. The moment it moves to crdt/ every one
// of them resolves to crdt/fuzz/generate.js and fails.
//
// The fix must not be "../": that hard-codes the depth, so it breaks again on
// the next move and silently resolves to the wrong place if a test is ever run
// from elsewhere. Finding the root by walking up to go.mod is depth-independent
// and location-independent.
func repoPath(t *testing.T, parts ...string) string {
	t.Helper()
	root := repoRoot(t)
	return filepath.Join(append([]string{root}, parts...)...)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := locateRepoRoot()
	if err != nil {
		t.Fatalf("cannot locate the repository root by walking up to go.mod: %v", err)
	}
	return root
}

// locateRepoRoot is the error-returning form, for the guards that need the root
// outside a test function — the source importer in
// struct_store_representation_test.go resolves sibling packages through it and
// has no *testing.T to report against.
func locateRepoRoot() (string, error) {
	repoRootOnce.Do(func() {
		_, thisFile, _, ok := runtime.Caller(0)
		if !ok {
			repoRootErr = os.ErrNotExist
			return
		}
		dir := filepath.Dir(thisFile)
		for {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
				repoRootDir = dir
				return
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				repoRootErr = os.ErrNotExist
				return
			}
			dir = parent
		}
	})
	return repoRootDir, repoRootErr
}
