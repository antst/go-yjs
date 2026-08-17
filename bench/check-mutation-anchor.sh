#!/usr/bin/env bash
# Compare the three mutation canaries against the fixed pre-StructStore anchor.
#
# Immediate-parent comparisons missed four individually small regressions that
# accumulated to 6-11%. This check deliberately keeps the original anchor: a
# new commit does not get to redefine the baseline it is judged against.

set -euo pipefail

readonly anchor_commit="c21917ae33751cd2f2e1010eda548a0525469d73"
readonly benchmark_pattern='^(BenchmarkTextAppendLarge|BenchmarkArrayInsertSequential|BenchmarkMapSet)$'
readonly max_time_ratio="${MUTATION_ANCHOR_MAX_RATIO:-1.05}"
readonly max_extra_bytes="${MUTATION_ANCHOR_MAX_EXTRA_BYTES:-32}"

# MEASUREMENT AUDIT (2026-08-17). Keep this next to the limits: it prevents a
# noisy finding from being rediscovered and then "fixed" by changing the gate
# around the value that happened to fail.
#
# A Linux/amd64 run once put ArrayInsertSequential at about 1.08x against the
# anchor. Fixed-count controls killed that finding: later windows measured
# 1.0097x and 1.0381x at 1000x, and 1.0195x at 2000x. Do not reopen the claimed
# 8% Array regression without new evidence; it was measurement-regime variance,
# not a code regression.
#
# Pinning b.N was tested as a way to stabilize B/op and was rejected. Seven
# fresh-process 1000x rounds on Linux/amd64 at 761342f produced these ranges
# (anchor -> current, with seven-sample medians in parentheses):
#
#   TextAppendLarge        48952..48968 (48958) -> 48977..48992 (48982)
#   ArrayInsertSequential 116456..116462 (116457) -> 116480..116487 (116481)
#   MapSet                673204..673210 (673208) -> 673226..673234 (673229)
#
# Thus even fixed b.N left per-arm spreads of 6-16 B/op; the earlier
# byte-identical three-round anchor was luck. Darwin/arm64 independently showed
# the same mechanism was not iteration-count driven. In three fresh processes:
#
#   fixed 1000x: Text 48981/48987/48982; Array 116480/116481/116486;
#                Map 673234/673233/673234
#   autoscaled:  Text 48976/48981/48976 at N=1075/1069/1222;
#                Array 116480/116480/116480 at N=4780/4819/4916;
#                Map 673230/673228/673230 at N=2730/2755/2788
#
# Array was byte-exact while its autoscaled iteration count moved by 136, and
# became LESS stable when pinned. Allocation COUNTS stayed exact on both hosts;
# byte totals moved with heap/size decisions despite identical work. A source
# guard forbidding autoscaling would therefore freeze a disproven mechanism.
#
# Timing from separate processes is noisier still. The same seven-round amd64
# control gave median ratios Text=1.0489, Array=1.0381, Map=1.0659, while current
# MapSet spanned 767329..891915 ns/op (16.2%). Three-round medians changed their
# verdict across adjacent windows. The alternating arm order defeats monotonic
# drift, but not shared-host variance of that size; a fixed-anchor failure is not
# attributable to the newest change without an immediate-parent control.
#
# Finally, the observed median byte deltas were +24, +24 and +21 against the
# +32 allowance, while individual arm spreads reached 16. The allowance is
# inside combined baseline-plus-jitter, not outside it. It must eventually be
# re-derived from these distributions with stated headroom. Do not widen it ad
# hoc to pass a candidate, and do not claim fixed iterations solve it. This
# audit deliberately changes no threshold or verdict while that redesign is
# unresolved.

repo_root="$(git rev-parse --show-toplevel)"
if ! git cat-file -e "${anchor_commit}^{commit}" 2>/dev/null; then
	echo "mutation anchor ${anchor_commit} is unavailable; fetch full history" >&2
	exit 1
fi

# A timing comparison is evidence only when both commits run the same workload.
# Fail closed if the benchmark fixture moved; re-anchoring then requires an
# explicit review rather than quietly comparing different operations.
#
# The anchor itself is IMMUTABLE and pre-StructStore, and a guard test pins its
# SHA, because re-anchoring destroys exactly the cumulative-drift detection it
# exists for. But the privatization refactor renamed identifiers inside
# crdt/perf_bench_test.go, so the byte comparison below can never succeed again — which
# would leave this canary permanently fail-closed, and a permanently failing gate
# is a deleted gate.
#
# So a byte difference is no longer fatal by itself: the fixture must match
# contents that were reviewed and recorded here. Any FURTHER change alters the
# hash and fails closed again, preserving the property that matters — a moved
# fixture stops the comparison until a human looks at it — without retiring the
# anchor.
#
# Reviewed 2026-08-17 for the crdt/ move. The fixture is byte-identical to the
# previously reviewed content except for its package clause, which the move
# changed from y_crdt to crdt; the pre-move file hashed to the previous recorded
# value c85eb1ce, confirming nothing else had drifted under that record. Note
# that the anchor still holds this file at the repository root, so the comparison
# above spans the rename explicitly rather than through a pathspec — a pathspec
# naming crdt/perf_bench_test.go can never be quiet against a commit that
# predates the directory, which would silently reduce this to a hash check.
#
# Reviewed 2026-08-16 against c21917a. The diff is eight lines: six identifier
# renames, one comment, and two dropping a type assertion Doc.GetMap no longer
# needs now that it returns *YMap rather than IAbstractType. No timed operation
# changed. The comparison was re-established by hand BEFORE this was recorded:
# all three canary benchmarks measured interleaved against b7650b1, medians of
# six rounds, giving 1.012x, 0.984x and 0.997x. That same run surfaced a real
# bimodal regression, which was root-caused and fixed rather than recorded over.
readonly reviewed_fixture_sha256="3caed72b54ad149163ae07bacfbc50d8f7e4a5985421f7030b50a883b2186bc9"

if ! git show "${anchor_commit}:perf_bench_test.go" | diff -q - crdt/perf_bench_test.go >/dev/null 2>&1; then
	actual_fixture_sha256="$(shasum -a 256 crdt/perf_bench_test.go | cut -d' ' -f1)"
	if [ "$actual_fixture_sha256" != "$reviewed_fixture_sha256" ]; then
		echo "crdt/perf_bench_test.go differs from mutation anchor ${anchor_commit}" >&2
		echo "and does not match the reviewed fixture recorded in this script" >&2
		echo "the fixed-anchor comparison is invalid until the fixture change is reviewed" >&2
		echo "  reviewed: $reviewed_fixture_sha256" >&2
		echo "  actual:   $actual_fixture_sha256" >&2
		exit 1
	fi
	echo "note: fixture differs from the anchor but matches the reviewed content hash"
fi

scratch="$(mktemp -d "${TMPDIR:-/tmp}/y-crdt-mutation-anchor.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/anchor"
git archive "$anchor_commit" | tar -x -C "$scratch/anchor"

anchor_results="$scratch/anchor.tsv"
current_results="$scratch/current.tsv"

# The package path is per-arm, not shared: the anchor predates the crdt/ move and
# carries the package at its repository root, while the checkout carries it under
# crdt/. Hard-coding either one runs a build failure as if it were a measurement.
run_benchmarks() {
	local label="$1"
	local directory="$2"
	local results="$3"
	local package="$4"
	local raw="$scratch/${label}.$$.txt"

	(
		cd "$directory"
		go test -run '^$' -bench "$benchmark_pattern" -benchtime=1s -count=1 "$package"
	) >"$raw"

	awk '
		/^Benchmark(TextAppendLarge|ArrayInsertSequential|MapSet)-[0-9]+[[:space:]]/ {
			name = $1
			sub(/-[0-9]+$/, "", name)
			print name, $3, $5, $7
		}
	' "$raw" >>"$results"
}

# Alternate arms so monotonic host drift cannot consistently favour either
# side. Three samples make the median insensitive to one scheduling outlier.
for round in 1 2 3; do
	if [ "$round" = "2" ]; then
		run_benchmarks "current-${round}" "$repo_root" "$current_results" "./crdt"
		run_benchmarks "anchor-${round}" "$scratch/anchor" "$anchor_results" "."
	else
		run_benchmarks "anchor-${round}" "$scratch/anchor" "$anchor_results" "."
		run_benchmarks "current-${round}" "$repo_root" "$current_results" "./crdt"
	fi
done

median_for() {
	local file="$1"
	local benchmark="$2"
	local column="$3"
	local count

	count="$(awk -v benchmark="$benchmark" '$1 == benchmark { count++ } END { print count + 0 }' "$file")"
	if [ "$count" != "3" ]; then
		echo "${benchmark}: got ${count} samples, want 3" >&2
		exit 1
	fi
	awk -v benchmark="$benchmark" -v column="$column" '$1 == benchmark { print $column }' "$file" |
		sort -n | sed -n '2p'
}

failed=0
for benchmark in BenchmarkTextAppendLarge BenchmarkArrayInsertSequential BenchmarkMapSet; do
	anchor_ns="$(median_for "$anchor_results" "$benchmark" 2)"
	current_ns="$(median_for "$current_results" "$benchmark" 2)"
	anchor_bytes="$(median_for "$anchor_results" "$benchmark" 3)"
	current_bytes="$(median_for "$current_results" "$benchmark" 3)"
	anchor_allocs="$(median_for "$anchor_results" "$benchmark" 4)"
	current_allocs="$(median_for "$current_results" "$benchmark" 4)"
	ratio="$(awk -v current="$current_ns" -v anchor="$anchor_ns" 'BEGIN { printf "%.4f", current / anchor }')"

	printf '%-31s anchor=%9s ns current=%9s ns ratio=%s bytes=%s->%s allocs=%s->%s\n' \
		"$benchmark" "$anchor_ns" "$current_ns" "$ratio" \
		"$anchor_bytes" "$current_bytes" "$anchor_allocs" "$current_allocs"

	if awk -v ratio="$ratio" -v limit="$max_time_ratio" 'BEGIN { exit !(ratio > limit) }'; then
		echo "  regression: ratio ${ratio} exceeds fixed-anchor limit ${max_time_ratio}" >&2
		failed=1
	fi
	if [ "$current_allocs" != "$anchor_allocs" ]; then
		echo "  regression: allocation count changed" >&2
		failed=1
	fi
	if [ "$current_bytes" -gt $((anchor_bytes + max_extra_bytes)) ]; then
		echo "  regression: bytes exceed anchor allowance of ${max_extra_bytes}" >&2
		failed=1
	fi
done

exit "$failed"
