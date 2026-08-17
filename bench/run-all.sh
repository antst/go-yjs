#!/usr/bin/env bash
# Three-way performance comparison: this library, the yjs JavaScript reference, and the yrs Rust
# implementation — all on one machine, in one session, on identical workloads.
#
# WHY A SINGLE SCRIPT: the comparison is only meaningful if all three run on the same hardware
# under the same load. Numbers collected on a busy developer machine, or worse on three different
# machines, do not support the ratios anyone will quote from them. Run this on an idle host.
#
#   bash bench/run-all.sh [outdir]        # default outdir: ./bench-results
#
# LEGS selects which implementations to run; the default is all four.
#
#   LEGS=go bash bench/run-all.sh out/      # re-measure only this library
#   LEGS=go,yjs bash bench/run-all.sh out/  # this library against the JS reference
#
# For evidence-bearing runs, use run-bracketed.sh rather than invoking this
# collector directly. It predeclares the calibration band and keeps mpstat/PSI
# alive from before the pre-canary through completion of the post-canary.
#
# WHY THIS EXISTS. yjs, yrs and ygo are PINNED versions whose performance does not
# change when this library changes, so re-running them for every commit of ours
# re-measures a constant and costs roughly two thirds of the run. Reference columns
# are durable artifacts: collect them once per (implementation version, host) and
# reuse them.
#
# THE ONE CONDITION on reusing them. A cross-implementation RATIO is only evidence
# if both halves were measured under comparable host conditions, and columns from
# different windows are not automatically comparable. The instrument for that
# already exists — the calibration workload and its predeclared band. If the new
# run's calibration agrees with the calibration recorded alongside the reference
# columns, the windows are comparable and the columns splice; if it does not, they
# do not, and the reference legs must be re-run. Check it, do not assume it.
#
# Re-run the reference legs when: the pinned version changes, the host changes, a
# benchmark row has no reference counterpart yet, or that calibration check fails.
#
# Requires Go, Node (with `npm ci` already run in fuzz/), and a Rust toolchain. Any missing
# toolchain is reported and skipped rather than silently omitted, so a partial run cannot be
# mistaken for a complete one.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
OUT="${1:-$ROOT/bench-results}"
mkdir -p "$OUT"

LEGS="${LEGS:-go,yjs,yrs,ygo}"
leg_enabled() {
	case ",$LEGS," in
	*",$1,"*) return 0 ;;
	*) return 1 ;;
	esac
}
for requested in $(printf '%s' "$LEGS" | tr ',' ' '); do
	case "$requested" in
	go | yjs | yrs | ygo) ;;
	*)
		echo "!! unknown leg '$requested' in LEGS='$LEGS' (valid: go,yjs,yrs,ygo)" >&2
		exit 2
		;;
	esac
done

echo "=============================================================="
echo " three-way benchmark"
echo " commit : $(cd "$ROOT" && git rev-parse --short HEAD 2>/dev/null || echo unknown)"
echo " host   : $(uname -sm)"
echo " cpus   : $( (nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo '?') )"
echo " out    : $OUT"
echo "=============================================================="

# Load average is worth recording: a comparison run on a loaded box is not comparable to one on an
# idle box, and the number is free to capture.
uptime 2>/dev/null | sed 's/^/ load   : /'
echo

# Record provenance ALONGSIDE the raw data, not only on the terminal.
#
# WHY. status.py renders whatever raw files it finds, so re-running it over old
# data produces a page with a fresh timestamp and two-day-old numbers, and nothing
# on the page says which. That is exactly what happened here: bench-results/
# carried a status.html regenerated today from go.txt measured two days earlier,
# with no commit named anywhere on it. A rendered page gets handed around on its
# own; its provenance has to travel inside it.
{
	echo "measured: $(date -u '+%Y-%m-%d %H:%M UTC')"
	echo "commit: $(cd "$ROOT" && git rev-parse HEAD 2>/dev/null || echo unknown)"
	echo "legs: $LEGS"
	echo "host: $(uname -sm)"
	echo "cpus: $( (nproc 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo '?') )"
	echo "load: $(uptime 2>/dev/null | sed 's/.*load average[s]*: //')"
} > "$OUT/PROVENANCE"
echo

# ---------------------------------------------------------------- Go (this library)
if ! leg_enabled go; then
	echo "== go: SKIPPED (LEGS=$LEGS) — reuse the recorded column =="
	echo
elif command -v go >/dev/null 2>&1; then
  echo "== go: this library =="
  # EVERY benchmark, not an enumerated list. The list form silently went stale the moment the
  # suite grew from 14 scenarios to 54: it kept passing, kept producing a table, and simply left
  # 37 rows reading UNMEASURED -- a selector that names what to include fails by omission, and
  # omission is exactly what this comparison must never do quietly.
  #
  # The history-sensitive YText case is EXCLUDED here and appended below at fixed counts. Its
  # document grows as the loop runs, so an autoscaled figure measures a different workload; it is
  # also slow to autoscale against a 100k-character document.
  # -timeout is MANDATORY here, not decorative. Go defaults to a 10-minute package timeout, and
  # this leg alone needs 54 benchmarks x 5 counts x 2s = ~540s of timed work BEFORE per-benchmark
  # calibration and untimed setup. That was comfortable when the suite had 14 benchmarks and became
  # structurally unrunnable when it reached 54 -- the same way the enumerated selector went stale.
  # A timeout kill also destroys the whole leg, not just the benchmark that overran.
  ( cd "$ROOT" && go test -run '^$' -bench '^Benchmark' \
      -skip '^BenchmarkYText_RandomInsert_100k$' \
      -benchtime 2s -count=5 -timeout 120m ./crdt ) 2>&1 | tee "$OUT/go.txt" | grep '^Benchmark' | tail -5
  # The ygo-shaped case is history-sensitive, so it is reported at FIXED iteration counts rather
  # than a time budget — one autoscaled number for it is misleading.
  for bt in 10x 1000x 10000x; do
    ( cd "$ROOT" && go test -run '^$' -bench '^BenchmarkYText_RandomInsert_100k$' \
        -benchtime "$bt" -count=5 -timeout 120m ./crdt ) 2>&1 | grep '^Benchmark' | sed "s/\$/  # benchtime=$bt/"
  done >> "$OUT/go.txt"
  # Fail loudly on under-collection. The previous enumerated selector produced a perfectly
  # well-formed go.txt containing a third of the suite, and nothing anywhere said so -- the table
  # simply rendered the rest as UNMEASURED. Comparing against status.py's expected set makes a
  # stale selector an error at collection time instead of a quiet gap in the published page.
  collected=$(grep -c '^Benchmark' "$OUT/go.txt" || true)
  distinct=$(grep '^Benchmark' "$OUT/go.txt" | awk '{print $1}' | sort -u | wc -l | tr -d ' ')
  # Ask status.py for the count rather than grepping it: a regex over the source also matched the
  # NOT_APPLICABLE entries and reported 62 for a 54-benchmark table. A guard against staleness that
  # is itself computed by a fragile pattern is not a guard.
  expected=$(python3 -c "import sys; sys.path.insert(0, '$HERE'); import status; print(sum(len(e) for _, e in status.CATEGORIES))" 2>/dev/null || echo 0)
  echo "   -> $OUT/go.txt  ($collected rows, $distinct distinct benchmarks; status.py tracks $expected)"
  if [ "$expected" -gt 0 ] && [ "$distinct" -lt $(( expected * 3 / 4 )) ]; then
    echo "!! ONLY $distinct distinct benchmarks collected but status.py tracks $expected."
    echo "!! The Go selector is stale or a build failed. This run CANNOT produce a complete matrix."
    FAILED_GO=1
  fi
else
  echo "!! go not found — this library's numbers are MISSING from this run"
  FAILED_GO=1
fi
echo

# ---------------------------------------------------------------- JS (yjs reference)
if ! leg_enabled yjs; then
	echo "== node: yjs SKIPPED (LEGS=$LEGS) — pinned version, reuse the recorded column =="
elif command -v node >/dev/null 2>&1; then
  if [ -d "$ROOT/fuzz/node_modules/yjs" ]; then
    echo "== node: yjs reference =="
    ( cd "$ROOT/fuzz" && node perf_bench.mjs ) 2>/dev/null | tee "$OUT/yjs.json" | tail -4
    echo "   -> $OUT/yjs.json  (yjs $(node -e "console.log(require('$ROOT/fuzz/node_modules/yjs/package.json').version)" 2>/dev/null))"
  else
    echo "!! fuzz/node_modules missing — run 'cd fuzz && npm ci' first. yjs numbers MISSING."
    FAILED_YJS=1
  fi
else
  echo "!! node not found — yjs numbers MISSING from this run"
  FAILED_YJS=1
fi
echo

# ---------------------------------------------------------------- Rust (yrs)
if ! leg_enabled yrs; then
	echo "== cargo: yrs SKIPPED (LEGS=$LEGS) — pinned version, reuse the recorded column =="
elif command -v cargo >/dev/null 2>&1; then
  echo "== cargo: yrs =="
  ( cd "$HERE/yrs" && cargo build --release --quiet 2>&1 | tail -5 )
  if [ -x "$HERE/yrs/target/release/yrs-bench" ]; then
    "$HERE/yrs/target/release/yrs-bench" 2>&1 | tee "$OUT/yrs.txt" | tail -4
    echo "   -> $OUT/yrs.txt  (yrs $(grep -A1 'name = "yrs"' "$HERE/yrs/Cargo.lock" | grep version | head -1 | cut -d'"' -f2))"
  else
    echo "!! yrs harness failed to build — yrs numbers MISSING from this run"
    FAILED_YRS=1
  fi
else
  echo "!! cargo not found — yrs numbers MISSING from this run"
  FAILED_YRS=1
fi
echo

# ---------------------------------------------------------------- Go (reearth/ygo)
# A SEPARATE Go module: ygo pulls SQLite, Redis and a WebSocket library, none of which belongs in
# the library-under-test's go.sum for the sake of a benchmark.
if ! leg_enabled ygo; then
	echo "== ygo SKIPPED (LEGS=$LEGS) — pinned version, reuse the recorded column =="
elif command -v go >/dev/null 2>&1; then
  echo "== go: reearth/ygo =="
  ( cd "$HERE/ygo" && go build -o ygo-bench . 2>&1 | tail -5 )
  if [ -x "$HERE/ygo/ygo-bench" ]; then
    "$HERE/ygo/ygo-bench" 2>/dev/null | tee "$OUT/ygo.txt" | tail -4
    echo "   -> $OUT/ygo.txt  (ygo $(cd "$HERE/ygo" && go list -m github.com/reearth/ygo 2>/dev/null | awk '{print $2}'))"
  else
    echo "!! ygo harness failed to build — ygo numbers MISSING from this run"
    FAILED_YGO=1
  fi
fi
echo

echo "=============================================================="
echo " done. Compare with: python3 bench/compare.py $OUT"
echo "=============================================================="

# Consume the failure flags. Every branch above already printed a loud "!!", but
# printing is not reporting: FAILED_GO was set and never read, so a run that
# collected ZERO benchmarks announced it could not produce a complete matrix and
# then exited 0. Anything invoking this — CI, a wrapper, a person chaining
# `&& status.py` — saw success. The other legs did not even set a flag, so a run
# missing an entire reference implementation also exited 0.
#
# A partial run is still useful output, so the data is left on disk and named
# rather than deleted; the non-zero exit exists so nothing downstream treats it
# as a complete matrix. A leg skipped on purpose via LEGS= sets no flag and is
# not a failure.
failed_legs=""
for leg in GO YJS YRS YGO; do
	eval "flag=\${FAILED_$leg:-0}"
	if [ "$flag" = "1" ]; then
		failed_legs="$failed_legs $(printf '%s' "$leg" | tr 'A-Z' 'a-z')"
	fi
done
if [ -n "$failed_legs" ]; then
	echo
	echo "!! INCOMPLETE RUN — failed or missing legs:$failed_legs"
	echo "!! $OUT holds partial data. Do not publish it as a comparison."
	exit 1
fi
