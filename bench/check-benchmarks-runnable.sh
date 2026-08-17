#!/usr/bin/env bash
#
# Every benchmark must COMPLETE at the default benchtime.
#
# WHY THIS EXISTS. Twice in one day a benchmark shipped that reported a healthy
# per-op number while being unbounded in wall clock, and both times it was found
# by a collaborator whose run died rather than by anything here.
#
#   BenchmarkClientStructTreeRemoveMiddle  rebuilt a 64,000-entry tree
#   BenchmarkContentAnySplitBalanced*      rebuilt an n-element ArrayAny
#
# both inside b.StopTimer()/b.StartTimer(), on every iteration. StopTimer keeps
# that work out of ns/op but not out of the clock, and Go sizes b.N from the
# timed body alone. The fatal shape is therefore specific and detectable:
#
#   an O(1) timed body + O(n) untimed per-iteration setup
#
# The O(1) body makes b.N autoscale into the millions; the untimed setup then
# multiplies by that. Note which rows did NOT stall — AppendThenDelete and
# ContentAnySplitCopyOut have the same StopTimer shape but timed bodies large
# enough to keep b.N small. Grepping for StopTimer finds 18 sites and cannot
# tell them apart, so this check runs them instead of reading them.
#
# THE DEFAULT BENCHTIME IS THE WHOLE POINT. Every one of these benchmarks was
# developed and validated with an explicit -benchtime=NNx, and a pinned
# iteration count is exactly the condition under which the defect cannot appear:
# b.N never autoscales, so the untimed setup never multiplies. A benchmark
# validated only under a pinned count has not been validated.
#
# Usage: bash bench/check-benchmarks-runnable.sh [per-benchmark-timeout-seconds]

set -uo pipefail
cd "$(dirname "$0")/.."

LIMIT="${1:-45}"
BIN="$(mktemp -t ycrdt-bench-XXXXXX)"
trap 'rm -f "$BIN"' EXIT

echo "▶ building the benchmark binary"
go test -c -o "$BIN" ./crdt || { echo "✗ build failed" >&2; exit 1; }

names="$("$BIN" -test.list 'Benchmark' 2>/dev/null | grep '^Benchmark')"
total="$(printf '%s\n' "$names" | wc -l | tr -d ' ')"
echo "▶ running $total benchmarks at the DEFAULT benchtime, ${LIMIT}s cap each"

# A WALL-CLOCK CAP ALONE IS HOST-DEPENDENT, and that is a real weakness: the same
# benchmark passed here and failed on a slower machine, and a gate whose verdict
# depends on how fast your box is will eventually be disbelieved. So the timeout is
# only the backstop for benchmarks that never finish. The primary detector is the
# defect's actual signature, which is host-INDEPENDENT: untimed per-iteration work
# dominating the timed body.
#
# Go reports iterations and ns/op, so the timed total is iterations x ns/op summed
# over a benchmark's rows. Comparing that against measured wall time gives a ratio
# that is roughly 1 for a healthy benchmark and grows without bound as untimed
# setup takes over. Measured on this suite: MapSet and ArrayInsertSequential 1.0,
# RelaySteadyState and ContentAnySplitCopyOut 1.2, TextAppendLarge 1.6,
# AppendThenDelete8000 1.8, and FindAndRemoveMiddle 3.5 — the last being the
# amortised-batch shape, which legitimately keeps some untimed build. Against that,
# ContentStringTailDeleteCrossover2 measured 68.8s wall against 2.5s timed: 28x.
#
# The threshold sits at 8x with more than a doubling of headroom on each side, and
# only applies above a wall-clock floor so a fast row whose fixture is a few
# milliseconds cannot trip it.
readonly max_untimed_ratio="${MUTATION_BENCH_MAX_UNTIMED_RATIO:-8}"
readonly ratio_floor_ms="${MUTATION_BENCH_RATIO_FLOOR_MS:-8000}"

# Benchmarks whose setup is INTRINSICALLY dearer than the operation they time,
# with the reason. A destructive operation needs a fresh fixture per round, so
# when building that fixture costs more than the operation itself the ratio is a
# property of the measurement rather than a defect. Each entry needs a reason and
# an argument that it is bounded — an exemption without one is how a gate rots.
#
#   BenchmarkMapClear — times Clear on a 2,000-key Y.Map. Building the map is
#   about 1 ms of Set operations against roughly 86 us to clear it, so the ratio
#   is near 12 whatever we do. It is BOUNDED: the timed body is large, so b.N
#   settles around 14,000 rather than autoscaling, and total wall clock is about
#   15 s. Batching cannot help because each round consumes its map.
ratio_exempt() {
	case "$1" in
	BenchmarkMapClear) return 0 ;;
	*) return 1 ;;
	esac
}

stalled=""
lopsided=""
while IFS= read -r name; do
	started="$(date +%s%N)"
	if ! output="$(timeout "$LIMIT" "$BIN" -test.run '^$' -test.bench "^${name}\$" -test.benchtime=1s 2>/dev/null)"; then
		echo "  STALL  $name" >&2
		stalled="$stalled $name"
		continue
	fi
	finished="$(date +%s%N)"
	wall_ms=$(( (finished - started) / 1000000 ))
	timed_ms="$(printf '%s\n' "$output" | awk '/^Benchmark/ { total += $2 * $3 } END { printf "%d", total / 1000000 }')"
	[ -z "$timed_ms" ] && timed_ms=0
	if [ "$wall_ms" -ge "$ratio_floor_ms" ] && [ "$timed_ms" -gt 0 ]; then
		ratio=$(( wall_ms / timed_ms ))
		if [ "$ratio" -ge "$max_untimed_ratio" ] && ! ratio_exempt "$name"; then
			echo "  UNTIMED  $name  wall=${wall_ms}ms timed=${timed_ms}ms ratio=${ratio}x" >&2
			lopsided="$lopsided $name"
		fi
	fi
done <<< "$names"

if [ -n "$lopsided" ] && [ -z "$stalled" ]; then
	cat >&2 <<EOF

✗ these benchmarks spend most of their wall clock OUTSIDE the timed region:
   $lopsided

  They still finish here, but only because this host is fast enough. The same
  shape is what makes a benchmark unrunnable on a slower machine, and it has
  already killed two cross-implementation runs. Fix it now rather than when
  someone else's run dies: build the fixture ONCE and reset it in O(1) inside the
  timed region, or amortise one build across a batch of timed operations.
EOF
	exit 1
fi

if [ -n "$stalled" ]; then
	cat >&2 <<EOF

✗ these benchmarks did not finish within ${LIMIT}s at the default benchtime:
   $stalled

  Almost always: an O(n) fixture built inside b.StopTimer() on every iteration,
  with a timed body small enough that b.N autoscales into the millions. Fixes,
  in order of preference:

    1. Build the fixture ONCE and reset it in O(1) inside the timed region --
       a slice-header assignment costs nothing against a real body.
    2. Amortise one build across a batch of timed operations.
    3. Only if neither works, shrink the fixture.

  Do NOT "fix" it by pinning -benchtime: that hides the defect from this check
  and from nobody else, least of all whoever runs the full suite next.
EOF
	exit 1
fi

echo "✓ all $total benchmarks complete at the default benchtime"
