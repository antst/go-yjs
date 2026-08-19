package crdt

import (
	"os"
	"strings"
	"testing"
)

// The guard survives the removal of the timing and bytes verdicts because what
// it protects is unchanged: the anchor must stay fixed and the two arms must run
// the same workload. Comparing allocation counts against a baseline a commit is
// allowed to redefine detects nothing, and comparing them across different
// fixtures compares different work.
//
// It also asserts the removed verdicts stay removed. Both were documented as
// undecidable by the script's own measurement audit and still failed pushes, and
// a reader who has not seen that audit will reasonably assume a timing canary
// belongs in a pre-push hook.
func TestMutationBenchmarkGuardKeepsFixedPreStructStoreAnchor(t *testing.T) {
	const anchor = "c21917ae33751cd2f2e1010eda548a0525469d73"
	script, err := os.ReadFile(repoPath(t, "bench/check-mutation-anchor.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, required := range []string{
		"anchor_commit=\"" + anchor + "\"",
		// The fixture comparison must read the anchor's copy from the path the
		// anchor actually has it at. The anchor predates the crdt/ move, so a
		// pathspec naming crdt/perf_bench_test.go would report a difference
		// unconditionally and quietly demote this to a bare hash check.
		"git show \"${anchor_commit}:perf_bench_test.go\"",
		"diff -q - crdt/perf_bench_test.go",
		// Each arm must run its own package path for the same reason.
		"\"$scratch/anchor\" \"$anchor_results\" \".\"",
		"\"$repo_root\" \"$current_results\" \"./crdt\"",
		"BenchmarkTextAppendLarge",
		"BenchmarkArrayInsertSequential",
		"BenchmarkMapSet",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("fixed-anchor script lacks %q", required)
		}
	}
	if strings.Contains(text, "HEAD~1") {
		t.Fatal("mutation benchmark guard regressed to an immediate-parent baseline")
	}
	for _, removed := range []string{"max_time_ratio", "max_extra_bytes", "ratio="} {
		if strings.Contains(text, removed) {
			t.Errorf("the fixed-anchor script gates on %q again; the audit inside it shows that verdict is not decidable on a shared host, and it was removed rather than widened", removed)
		}
	}

	hook, err := os.ReadFile(repoPath(t, ".githooks/pre-push"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hook), "bash bench/check-mutation-anchor.sh") {
		t.Fatal("pre-push gate does not run the fixed-anchor mutation canaries")
	}
}
