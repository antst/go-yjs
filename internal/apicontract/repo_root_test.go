package apicontract

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

var (
	repoRootOnce sync.Once
	repoRootDir  string
	repoRootErr  error
)

// locateRepoRoot finds the repository root by walking up to go.mod.
//
// This package inventories the public API of every other package in the module,
// so it must address them from the root. Walking up to go.mod is
// depth-independent, unlike "../..", which silently resolves somewhere else the
// moment either this package or one of its targets moves.
//
// crdt carries its own copy for the same reason it carries its own
// receiverTypeName: the two are independent guards, and neither should be able
// to break the other.
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
