#!/usr/bin/env bash
# Differential-oracle entrypoint (T015). Generates cases with the pinned yjs@13.6.31 reference and
# replays them through this fork, asserting byte-identical state / canonical convergence.
#
# Pinned CLI contract:
#   ./run-gate.sh [--seeds N] [--surface all|<name>] [--dir A|B|both]
#     --seeds    N       total seeds AGGREGATE across the selected surfaces (default 2000);
#                        each selected surface gets N/|surfaces|, floored at 200
#     --surface  name    all | <any registered surface>  (default: all)
#                        The list is DERIVED from internal/oracle's registry, never hardcoded here.
#     --dir      A|B|both  direction A = Yjs generates ops -> Go replays -> byte-compare.
#                        Direction B (Go generates -> Yjs decodes/re-encodes) is available when the
#                        registry says a surface realizes it — asked at run time, not hardcoded.
#                        Default: A.
#     --skip-dirb        skip the direction-B differential. It is ONE test covering several
#                        surfaces, so a caller looping per-surface would otherwise repeat it once
#                        per surface; run it once separately instead.
#     --tier     fast|full|scale|ultimate   selects the default seed volume (overridable with
#                        --seeds). `ultimate` is 10x scale (~2.5h) for rare deep runs; it samples
#                        the generators' space harder, it does not widen it.
#
# Surfaces:
#   text array map xml applyDelta  -> native-op differentials (Go replays yjs's OWN native ops)
#   update                         -> V1/V2 update gate: encode/apply paths + the convergence
#                                     invariant (`concurrent` replays 6 permuted apply orders).
#                                     Honours the FUZZ_STRICT_* env flags for subdoc/gc/snapshot/xml.
#
# Legacy positional form (still supported, used by feature-002 docs):
#   ./run-gate.sh [mode] [cases] [opsPerCase] [seedStart]     mode = single|concurrent|both
#
# Exit code is non-zero if any surface fails (CI-safe).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
YCRDT="$(cd "$HERE/.." && pwd)"
GO="${GO:-$(command -v go)}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ---------------------------------------------------------------- legacy positional path
run_one_legacy () {
  local mode="$1" cases="$2" ops="$3" seed="$4" file="$TMP/$1.ndjson"
  echo "== generating mode=$mode cases=$cases opsPerCase=$ops seedStart=$seed =="
  node "$HERE/generate.js" "$mode" "$seed" "$cases" "$ops" > "$file"
  local diverged
  diverged="$(grep -c jsDiverged "$file" || true)"
  echo "   JS-internal non-convergence records: $diverged"
  echo "== verifying mode=$mode through the fork (V1 + V2) =="
  ( cd "$YCRDT" && FUZZ_FILE="$file" FUZZ_MODE="$mode" \
      "$GO" test -gcflags="all=-l" -count=1 -run TestFuzzGate -v -timeout 60m ./crdt ) \
    | grep -E "FUZZ_SUMMARY|DIVERGENCE|PANIC|serialize error|--- FAIL|--- PASS|^ok|^FAIL"
}

if [ $# -gt 0 ] && [[ "$1" != --* ]]; then
  MODE="${1:-both}"; CASES="${2:-1000}"; OPS="${3:-100}"; SEED="${4:-1}"
  if [ "$MODE" = "both" ]; then
    run_one_legacy single     "$CASES" "$OPS" "$SEED"
    run_one_legacy concurrent "$CASES" "$OPS" "$SEED"
  else
    run_one_legacy "$MODE" "$CASES" "$OPS" "$SEED"
  fi
  exit 0
fi

# ---------------------------------------------------------------- pinned flag contract
SEEDS=2000
SURFACE=all
DIR=A
TIER=fast
SKIP_DIRB=0

while [ $# -gt 0 ]; do
  case "$1" in
    --seeds)   SEEDS="${2:?--seeds needs a value}"; shift 2 ;;
    --surface) SURFACE="${2:?--surface needs a value}"; shift 2 ;;
    --dir)     DIR="${2:?--dir needs a value}"; shift 2 ;;
    --tier)    TIER="${2:?--tier needs a value}"; shift 2 ;;
    --skip-dirb) SKIP_DIRB=1; shift ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

# Default seed volume per tier. T071a measured the curve on the reference machine: a fixed ~11s of
# process/compile overhead plus ~0.82ms per seed, all 13 surfaces in BOTH directions. The fast tier
# was 2000 aggregate (200/surface, the floor) purely because nobody had measured it; that is ~11s,
# i.e. ~0.2% of the 10-minute ceiling, so the PR gate was leaving almost all of its budget unused.
#
# Fast is raised to 20000 aggregate (~24s here, ~2min on a runner 5x slower) — a 10x increase in
# what every PR actually checks while still leaving the bulk of the ceiling to the `-race` suite in
# the same job, which is the genuinely expensive half. Weights are tuned, never cell MEMBERSHIP:
# every realized cell runs in the fast tier regardless of volume (SC-001a, T009/T070).
case "$TIER" in
  fast)  [ "$SEEDS" = "2000" ] && SEEDS=20000 ;;
  full)  [ "$SEEDS" = "2000" ] && SEEDS=200000 ;;
  scale) [ "$SEEDS" = "2000" ] && SEEDS=1000000 ;;
  # The ultimate tier is 10x scale, invoked deliberately rather than scheduled. Roughly 2.5 hours.
  # Read the note on oracle.TierFloor before reaching for it: seed volume samples the space the
  # GENERATORS define and does not widen it — measured, 100x the seeds bought TWO statements.
  ultimate) [ "$SEEDS" = "2000" ] && SEEDS=10000000 ;;
  *) echo "ERROR: --tier must be fast, full, scale or ultimate (got '$TIER')" >&2; exit 2 ;;
esac

case "$DIR" in
  A|B|both) ;;
  *) echo "ERROR: --dir must be A, B, or both (got '$DIR')" >&2; exit 2 ;;
esac

# Direction availability comes from the REGISTRY, not a hardcoded rejection. The previous version
# hard-errored on --dir B citing feature 003's T013 — the task this feature implements — so the
# entrypoint would have kept refusing direction B after it was built. Asking the registry means the
# answer changes when the code does.
if [ "$DIR" != "A" ]; then
  PENDING_B="$(cd "$YCRDT" && "$GO" run ./internal/oracle/cmd/surfaces -cells | grep ':B$' || true)"
  if [ -z "$PENDING_B" ]; then
    echo "ERROR: no surface realizes direction B yet — no generator has been registered for it." >&2
    echo "       This is a registry fact, not a hardcoded refusal; it changes when the generators land." >&2
    exit 2
  fi
fi

# Derived from the registry, never a literal. A hardcoded list cannot include surfaces added later,
# so a surface could be registered while this entrypoint silently never ran it — the feature-003
# hollow gate in a new place. Deriving makes registry/CLI drift impossible rather than noticeable.
ALL_SURFACES="$(cd "$YCRDT" && "$GO" run ./internal/oracle/cmd/surfaces | tr '\n' ' ')"
if [ "$SURFACE" = "all" ]; then
  SELECTED="$ALL_SURFACES"
else
  # shellcheck disable=SC2076
  if [[ " $ALL_SURFACES " != *" $SURFACE "* ]]; then
    echo "ERROR: --surface must be one of: all $ALL_SURFACES (got '$SURFACE')" >&2; exit 2
  fi
  SELECTED="$SURFACE"
fi

NSURF="$(echo "$SELECTED" | wc -w | tr -d ' ')"
PER=$(( SEEDS / NSURF ))
[ "$PER" -lt 200 ] && PER=200

# SC-001's per-cell seed floor, enforced MECHANICALLY (T071b). Before this, the floor existed only
# as prose in the spec: the scale tier could run with any volume and still report success, so the
# claim "every realized cell saw >=10,000 seeds" rested on a human reading recorded volumes. The
# floor value lives in the registry (oracle.TierFloor) so the shell cannot drift from the invariant.
if ! (cd "$YCRDT" && "$GO" run ./internal/oracle/cmd/surfaces -check-volume "$PER" -tier "$TIER"); then
  echo "::error::per-cell seed floor not met for tier=$TIER (see above)" >&2
  exit 1
fi

echo "== oracle gate: tier=$TIER seeds=$SEEDS dir=$DIR =="
echo "== cells covered (surface:direction), volume per cell=$PER =="
(cd "$YCRDT" && "$GO" run ./internal/oracle/cmd/surfaces -cells -tier "$TIER") | sed 's/^/     /'
PENDING="$(cd "$YCRDT" && "$GO" run ./internal/oracle/cmd/surfaces -pending || true)"
if [ -n "$PENDING" ]; then
  echo "== NOT YET REGISTERED (canonical surfaces without generators): $PENDING =="
  echo "   'every surface' means every REGISTERED surface until this list is empty."
fi

FAILED=""

run_native () {
  local name="$1" script="$2" envvar="$3" testname="$4" file="$TMP/$1.ndjson"
  echo "== [$name] generating $PER cases =="
  node "$HERE/$script" 1 "$PER" > "$file"
  echo "== [$name] verifying through the fork =="
  if ( cd "$YCRDT" && env "$envvar=$file" "$GO" test -run "$testname" -count=1 -v -timeout 80m ./crdt ) \
       | grep -E "DIFF|--- FAIL|--- PASS|^ok|^FAIL"; then :; else FAILED="$FAILED $name"; fi
}

run_update () {
  local mode file
  # Default the strict surfaces ON. Without this a plain `run-gate.sh --surface update` (or the
  # default --surface all) generated and verified with all four assertions OFF, so subdoc/gc/
  # snapshot/xml were silently not checked — the caller had to know to export them. An explicit
  # `FUZZ_STRICT_X=0` from the caller still wins.
  export FUZZ_STRICT_SUBDOCS="${FUZZ_STRICT_SUBDOCS:-1}"
  export FUZZ_STRICT_GC="${FUZZ_STRICT_GC:-1}"
  export FUZZ_STRICT_SNAPSHOT="${FUZZ_STRICT_SNAPSHOT:-1}"
  export FUZZ_STRICT_XML="${FUZZ_STRICT_XML:-1}"
  for mode in single concurrent; do
    file="$TMP/update_$mode.ndjson"
    echo "== [update:$mode] generating $PER cases =="
    node "$HERE/generate.js" "$mode" 1 "$PER" 100 > "$file"
    echo "   JS-internal non-convergence records: $(grep -c jsDiverged "$file" || true)"
    echo "== [update:$mode] verifying through the fork (V1 + V2) =="
    if ( cd "$YCRDT" && FUZZ_FILE="$file" FUZZ_MODE="$mode" \
           "$GO" test -gcflags="all=-l" -count=1 -run TestFuzzGate -v -timeout 60m ./crdt ) \
         | grep -E "FUZZ_SUMMARY|DIVERGENCE|PANIC|serialize error|--- FAIL|--- PASS|^ok|^FAIL"; then
      # Free the corpus as soon as it has been verified. Both modes previously accumulated in $TMP
      # for the whole run, so peak disk was the SUM of the two rather than the larger of them --
      # at 1.5M seeds the single corpus alone reaches ~13 GiB, and holding both overran a 32 GiB
      # filesystem and stalled the sweep. This is the same defect class as the direction-B OOM,
      # where the harness scaled a resource with seed count instead of holding it flat.
      #
      # Kept deliberately on FAILURE: the corpus is the reproduction, and deleting the evidence of
      # the one run that found something would be a bad trade for disk.
      rm -f "$file"
    else FAILED="$FAILED update:$mode"; fi
  done
}

# Direction B is one differential covering several surfaces (this library builds, the reference
# re-encodes), so it runs once rather than per-surface — and only when direction B was asked for.
# Direction B is a SINGLE differential covering several surfaces, so it runs once per invocation
# regardless of --surface. A caller looping surface-by-surface therefore repeats the whole thing
# once per surface. --skip-dirb lets such a loop run it exactly once, separately.
#
# Its volume is also decoupled from --seeds: direction B BUILDS every document in-process (far more
# expensive per seed than replaying a corpus), so inheriting a scale/ultimate --seeds value made it
# the dominant cost of the run. DIRB_SEEDS overrides it explicitly.
DIRB_SEEDS="${DIRB_SEEDS:-$PER}"
if [ "$SKIP_DIRB" = "1" ]; then
  echo "== [direction B] skipped (--skip-dirb) =="
elif [ "$DIR" != "A" ]; then
  echo "== [direction B] this library builds -> reference decodes + re-encodes (seeds=$DIRB_SEEDS) =="
  # Capture to a file rather than piping straight into grep. A pipe made the SHELL see grep's
  # exit status, and the grep pattern discarded anything that was not a DIRB_ line — so when the
  # test process was OOM-killed, the run reported a bare "FAIL" with no reason at all. A gate that
  # fails without saying why is the hollow-gate problem wearing a different hat.
  DIRB_LOG="$TMP/dirB.log"
  if ( cd "$YCRDT" && DIRB_SEEDS="$DIRB_SEEDS" "$GO" test -run TestDirBDiff -count=1 -v -timeout 120m ./crdt ) \
       > "$DIRB_LOG" 2>&1; then
    grep -E "DIRB_DIFF|DIRB_SNAPSHOT|DIRB_GC|--- PASS|^ok" "$DIRB_LOG" || true
  else
    grep -E "DIRB_DIFF|DIRB_SNAPSHOT|DIRB_GC|--- FAIL|^FAIL" "$DIRB_LOG" || true
    echo "   -- direction B failed; last 20 lines of its output --"
    tail -20 "$DIRB_LOG" | sed 's/^/   /'
    FAILED="$FAILED dirB"
  fi
fi

for s in $SELECTED; do
  case "$s" in
    text)       run_native text       native_diff_gen.mjs   FUZZ_NATIVE_FILE TestNativeOpDiff ;;
    array)      run_native array      native_diff_arr.mjs   FUZZ_ARR_FILE    TestNativeArrayDiff ;;
    map)        run_native map        native_diff_map.mjs   FUZZ_MAP_FILE    TestNativeMapDiff ;;
    xml)        run_native xml        native_diff_xml.mjs   FUZZ_XML_FILE    TestNativeXmlDiff ;;
    applyDelta) run_native applyDelta native_diff_delta.mjs FUZZ_DELTA_FILE  TestNativeDeltaDiff ;;
    update)     run_update ;;
    undo)       run_native undo       undo_gen.mjs          FUZZ_UNDO_FILE   TestUndoDiff ;;
    relpos)     run_native relpos     relpos_gen.mjs        FUZZ_RELPOS_FILE TestRelPosDiff ;;
    sync)       run_native sync       sync_gen.mjs          FUZZ_SYNC_FILE   TestSyncDiff ;;
    awareness)  run_native awareness  awareness_gen.mjs     FUZZ_AWARENESS_FILE TestAwarenessDiff ;;
    # snapshot/gc/subdoc have no standalone generator: they are asserted INSIDE the update gate via
    # the FUZZ_STRICT_* flags, which run_update turns on by default. Named explicitly so they are
    # visibly accounted for rather than silently skipped by a missing arm.
    snapshot|gc|subdoc)
      echo "== [$s] covered by the update gate's FUZZ_STRICT_$(echo "$s" | tr '[:lower:]' '[:upper:]') assertions ==" ;;
    # A registered surface with no dispatch arm must FAIL, never silently do nothing. Without this
    # default, adding a surface to the registry and forgetting its arm here would leave it reported
    # as covered while never running — the same hollow-gate shape this feature exists to close.
    *)
      echo "ERROR: surface '$s' is registered but has no dispatch arm in run-gate.sh" >&2
      FAILED="$FAILED $s(no-dispatch)" ;;
  esac
done

if [ -n "$FAILED" ]; then
  echo "GATE FAILED:$FAILED" >&2
  exit 1
fi
echo "GATE PASSED: surfaces=[$SELECTED] dir=$DIR seeds=$SEEDS"
