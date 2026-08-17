#!/usr/bin/env bash
# Detect HISTORY-SENSITIVE benchmarks — those whose reported per-operation cost depends on how many
# iterations the harness chose to run.
#
#   bash bench/history-sensitivity.sh [lowCount] [highCount] [thresholdPercent]
#   defaults: 1000 10000 25   (wide spread on purpose; see CHOOSING COUNTS)
#
# WHY THIS EXISTS. Four benchmark-fairness defects have been found in this suite by hand, and
# hand-inspection does not scale. This automates the one that is mechanically detectable: a
# benchmark whose fixture accumulates state across iterations reports a different per-operation cost
# depending on how many iterations ran, so it cannot be compared against another implementation
# whose harness chose a different count.
#
# The signal is one-directional. Run the SAME benchmark at two fixed iteration counts; a benchmark
# that measures one operation reports the same ns/op either way, while one that accumulates gets
# SLOWER at the higher count because later iterations do more work. A faster high count means the
# opposite — one-off warmup spread over few iterations — so only a rise is flagged. Flagging both
# directions makes every cheap accessor a false positive, and a check that cries wolf gets ignored.
#
# Demonstrated: BenchmarkYText_RandomInsert_100k reports 1,032 ns/op at 1000x and 3,142 ns/op at
# 10000x, +204%. It is in ACKNOWLEDGED because all four implementations measure it at matched fixed
# counts, which is the correct handling rather than a suppression.
#
# WHAT THIS DOES NOT CATCH, stated because a tool that overstates its reach is worse than none.
# XmlSetAttribute was incomparable across the harnesses — Go autoscaled into millions of
# accumulated items, JS ran to a time budget, yrs stopped at 2000 — yet this scan does NOT flag it,
# because Go's per-operation cost for that workload rises only ~12% over that range. Its defect was
# a mismatch in FIXTURE STATE between implementations, which no single-implementation scan can see.
# Cross-harness fixture equivalence still needs reading the three harnesses side by side.
#
# CHOOSING COUNTS. Sensitivity depends entirely on spanning the range where accumulation bites. The
# same row that shows +204% between 1000x and 10000x is flat between 50x and 2000x. Prefer a wide
# spread, and treat a clean result at narrow counts as evidence of nothing.
#
# Fixed counts (-benchtime Nx), never durations: a time-based budget lets the iteration count vary
# with machine speed and load, which is the very confound being measured.
#
# This does not need an idle host to be useful — a history-sensitive row differs by a factor, not a
# few percent. Run it on a quiet machine anyway when the numbers themselves matter.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
LOW="${1:-1000}"
HIGH="${2:-10000}"
THRESHOLD="${3:-25}"
# BENCH_PATTERN narrows the scan while developing or when chasing one row. The default is every
# benchmark, because a selector that names what to include fails by omission and omission is the
# exact failure this scan exists to catch.
PATTERN="${BENCH_PATTERN:-^Benchmark}"
# Below this ns/op, timer resolution and one-off warmup swamp the comparison at small counts: a
# 2ns accessor measured over 20 iterations is not evidence of anything. Such rows are counted and
# reported, never silently dropped.
FLOOR="${FLOOR:-100}"

# Benchmarks whose history sensitivity is KNOWN and already handled by measuring them at fixed
# counts across all four implementations. Listing them here is not a suppression: it records that
# someone checked and that the comparison is made at matched counts elsewhere. A row that appears
# in the flagged output and is NOT in this list is an unhandled fairness defect.
ACKNOWLEDGED="BenchmarkYText_RandomInsert_100k"

cd "$ROOT" || exit 1
echo "=============================================================="
echo " history-sensitivity scan"
echo " commit    : $(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
echo " counts    : ${LOW}x vs ${HIGH}x"
echo " threshold : ${THRESHOLD}% change in ns/op"
echo " pattern   : ${PATTERN}"
echo " floor     : ${FLOOR} ns/op"
echo "=============================================================="

run_at() {
  # -benchmem is omitted deliberately: allocation is not the signal and printing it widens the
  # parse surface for no gain.
  go test -run '^$' -bench "$PATTERN" -benchtime "${1}x" -timeout 120m ./crdt 2>/dev/null \
    | awk '/^Benchmark/ {name=$1; sub(/-[0-9]+$/, "", name); print name, $3}'
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

echo "== running at ${LOW}x =="
run_at "$LOW" > "$TMP/low.txt"
echo "== running at ${HIGH}x =="
run_at "$HIGH" > "$TMP/high.txt"

if [ ! -s "$TMP/low.txt" ] || [ ! -s "$TMP/high.txt" ]; then
  echo "ERROR: no benchmark output parsed; the suite did not run" >&2
  exit 1
fi

# A row present at one count and absent at the other is itself a finding: it means the selector or
# the suite changed between runs, and a silently missing row is the failure mode this whole
# comparison exists to prevent.
awk -v thr="$THRESHOLD" -v ack="$ACKNOWLEDGED" -v floor="$FLOOR" '
  FNR==NR { low[$1]=$2; next }
  {
    name=$1; high=$2
    if (!(name in low)) { printf "MISSING-AT-LOW  %s\n", name; missing++; next }
    seen[name]=1
    l=low[name]
    if (l <= 0) next
    total++
    # Only a SLOWER high count is the signal. Accumulating state makes later iterations do more
    # work, so ns/op rises with the count. A faster high count is the opposite: the low run paid
    # one-off warmup spread over few iterations. Flagging both directions turns every cheap
    # benchmark into a false positive, and a check that cries wolf gets ignored.
    change=(high-l)/l*100
    if (l < floor && high < floor) { tooFast++; next }
    acked=(index(ack, name) > 0)
    if (change > thr) {
      status = acked ? "acknowledged" : "UNHANDLED"
      printf "%-14s %-38s %10.0f -> %10.0f ns/op  %+7.1f%%\n", status, name, l, high, change
      if (!acked) unhandled++
    }
  }
  END {
    for (n in low) if (!(n in seen)) { printf "MISSING-AT-HIGH %s\n", n; missing++ }
    printf "\ncompared %d benchmarks; %d unhandled, %d missing from one run", total, unhandled, missing
    if (tooFast > 0) printf ", %d below the %.0fns floor (timer resolution dominates)", tooFast, floor
    printf "\n"
    if (unhandled > 0 || missing > 0) {
      print "\nAn UNHANDLED row gets SLOWER as the iteration count rises, so its fixture"
      print "accumulates state across iterations. It cannot be compared against another"
      print "implementation whose harness picks a different iteration count. Either give every"
      print "implementation a fresh fixture per measured operation with setup excluded, or fix the"
      print "same iteration count on all of them, then add the name to ACKNOWLEDGED."
      exit 1
    }
    print "no unhandled history-sensitive benchmarks"
  }
' "$TMP/low.txt" "$TMP/high.txt"
