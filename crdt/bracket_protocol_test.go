package crdt

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// The bracket protocol is executable repository policy, not a manual recipe.
// Keep its happy path, mutation proofs and historical characterisation in the
// ordinary test suite so a change cannot bypass them by simply not invoking a
// shell-only test.
func TestBenchmarkBracketProtocol(t *testing.T) {
	for _, script := range []string{
		"test-run-bracketed.sh",
		"test-run-bracketed-mutations.sh",
		"test-bracket-band-history.sh",
	} {
		script := script
		t.Run(script, func(t *testing.T) {
			path := repoPath(t, "bench", script)
			cmd := exec.Command("bash", path)
			cmd.Dir = filepath.Dir(filepath.Dir(path))
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s: %v\n%s", script, err, output)
			}
		})
	}
}
