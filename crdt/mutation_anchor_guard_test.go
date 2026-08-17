package crdt

import (
	"os"
	"strings"
	"testing"
)

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

	hook, err := os.ReadFile(repoPath(t, ".githooks/pre-push"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hook), "bash bench/check-mutation-anchor.sh") {
		t.Fatal("pre-push gate does not run the fixed-anchor mutation canaries")
	}
}
