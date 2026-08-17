#!/usr/bin/env bash
# Fast protocol test for run-bracketed.sh. All benchmark and telemetry commands
# are fakes: this tests artifact immutability, event order, sampler lifetime and
# failure handling without collecting performance data.
set -euo pipefail

readonly HERE="$(cd "$(dirname "$0")" && pwd)"
readonly SCRIPT="${BRACKET_SCRIPT:-$HERE/run-bracketed.sh}"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/y-crdt-bracket-test.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT
mkdir -p "$scratch/bin" "$scratch/pressure"

for resource in cpu io memory; do
	printf 'some avg10=0.00 avg60=0.00 avg300=0.00 total=0\n' >"$scratch/pressure/$resource.pressure"
done

apply_fake() {
	local path="$1"
	shift
	printf '%s\n' "$@" >"$path"
	chmod +x "$path"
}

apply_fake "$scratch/bin/fake-go" '#!/usr/bin/env bash' '
set -euo pipefail
if [ "${1:-}" = version ]; then echo "go version fake"; exit 0; fi
counter_file="$FAKE_STATE/go-calls"
calibration_calls="${FAKE_CALIBRATION_CALLS:-3}"
canary_processes="${FAKE_CANARY_PROCESSES:-9}"
calls=0
[ ! -f "$counter_file" ] || calls="$(cat "$counter_file")"
calls=$((calls + 1))
echo "$calls" >"$counter_file"
printf "%s\n" "$@" >"$FAKE_STATE/go-args-$calls"
if [ "$calls" -gt "$calibration_calls" ] && [ -z "${FAKE_ALLOW_DEAD_POST:-}" ]; then
	[ "$(cat "$FAKE_STATE/mpstat-state")" = running ]
fi
samples=1
for arg in "$@"; do
	case "$arg" in -count=*) samples="${arg#-count=}" ;; esac
done
for ((i = 0; i < samples; i++)); do
	if [ "$calls" -le "$calibration_calls" ]; then
		base=100
		if [ -z "${FAKE_ZERO_DISPERSION:-}" ]; then
			base=$((base + 2 * (calls - 1)))
		fi
	else
		base="${FAKE_GO_BASE:-100}"
	fi
	if [ "$calls" -gt $((calibration_calls + canary_processes)) ] && [ -n "${FAKE_POST_BASE:-}" ]; then base="$FAKE_POST_BASE"; fi
	value=$((base + i))
	# Make the pooled calibration median (104) differ from the median of
	# fresh-process medians (103). A protocol that regresses to pooling must fail.
	if [ "$calls" -eq 1 ] && [ "$i" -eq 2 ] && [ -z "${FAKE_ZERO_DISPERSION:-}" ]; then value=200; fi
	printf "BenchmarkMapSet-4  1  %d ns/op  0 B/op  0 allocs/op\n" "$value"
done
echo PASS
'

apply_fake "$scratch/bin/fake-mpstat" '#!/usr/bin/env bash' '
set -euo pipefail
echo running >"$FAKE_STATE/mpstat-state"
echo $$ >"$FAKE_STATE/mpstat-pid"
sleep_pid=""
trap '\''echo stopped >"$FAKE_STATE/mpstat-state"; if [ -n "$sleep_pid" ]; then kill "$sleep_pid" 2>/dev/null || true; fi; exit 0'\'' TERM INT
echo "fake mpstat started"
while :; do sleep 0.1 & sleep_pid=$!; wait "$sleep_pid" || true; sleep_pid=""; done
'

apply_fake "$scratch/bin/fake-run-all" '#!/usr/bin/env bash' '
set -euo pipefail
out="$1"
[ "$(cat "$FAKE_STATE/mpstat-state")" = running ]
echo "collector saw telemetry" >"$FAKE_STATE/collector-ran"
echo "commit: fake" >"$out/PROVENANCE"
if [ -n "${FAKE_KILL_MPSTAT:-}" ]; then
	kill "$(cat "$FAKE_STATE/mpstat-pid")"
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		[ "$(cat "$FAKE_STATE/mpstat-state")" = stopped ] && break
		sleep 0.05
	done
fi
exit "${FAKE_COLLECTOR_STATUS:-0}"
'

apply_fake "$scratch/bin/fake-git" '#!/usr/bin/env bash' '
set -euo pipefail
if [ "${1:-}" = -C ]; then shift 2; fi
case "${1:-}" in
status)
	[ -z "${FAKE_GIT_DIRTY:-}" ] || echo "?? uncommitted-benchmark.go"
	;;
rev-parse)
	echo fake-commit
	;;
*)
	echo "unexpected fake-git arguments: $*" >&2
	exit 1
	;;
esac
'

export FAKE_STATE="$scratch/state"
mkdir -p "$FAKE_STATE"
export BRACKET_GO="$scratch/bin/fake-go"
export BRACKET_GIT="$scratch/bin/fake-git"
export BRACKET_MPSTAT="$scratch/bin/fake-mpstat"
export BRACKET_RUN_ALL="$scratch/bin/fake-run-all"
export FAKE_CALIBRATION_CALLS=9
export FAKE_CANARY_PROCESSES=9

# A dirty tree cannot honestly be labelled with `git rev-parse HEAD`. Reject it
# before a benchmark process runs or any immutable criterion is declared.
dirty="$scratch/dirty"
export FAKE_GIT_DIRTY=1
set +e
dirty_error="$(bash "$SCRIPT" calibrate "$dirty" --cpus unpinned \
	--processes 9 --samples 3 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" 2>&1)"
dirty_status=$?
set -e
unset FAKE_GIT_DIRTY
if [ "$dirty_status" = 0 ]; then
	echo "dirty worktree unexpectedly declared a benchmark band" >&2
	exit 1
fi
case "$dirty_error" in
*"benchmark provenance requires a clean worktree"*) ;;
*)
	echo "dirty worktree failed for the wrong reason: $dirty_error" >&2
	exit 1
	;;
esac
[ ! -e "$dirty/bracket-band.txt" ]
[ ! -e "$FAKE_STATE/go-calls" ]

out="$scratch/pass"
bash "$SCRIPT" calibrate "$out" --cpus unpinned \
	--processes 9 --samples 3 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" \
	--reference 109:90:130 >/dev/null
grep -Fxq './crdt' "$FAKE_STATE/go-args-1" || {
	echo "MapSet canary did not target ./crdt" >&2
	exit 1
}
if ! grep -q '^format: y-crdt-benchmark-bracket-v2$' "$out/bracket-band.txt" ||
	! grep -q '^band_method: fresh-process-group-median-mad-student-t-99pct$' "$out/bracket-band.txt" ||
	! grep -q '^calibration_processes: 9$' "$out/bracket-band.txt" ||
	! grep -q '^canary_processes: 9$' "$out/bracket-band.txt" ||
	! grep -q '^acceptance: grouped-medians-plus-paired-drift$' "$out/bracket-band.txt" ||
	! grep -q '^calibration_center_ns: 109$' "$out/bracket-band.txt" ||
	! grep -q '^calibration_process_mad_ns: 4$' "$out/bracket-band.txt" ||
	! grep -q '^scale_degrees_of_freedom: 8$' "$out/bracket-band.txt" ||
	! grep -q '^prediction_student_t_quantile: 3.352160238$' "$out/bracket-band.txt" ||
	! grep -q '^scale_uncertainty_inflation: 1.301391$' "$out/bracket-band.txt" ||
	! grep -q '^center_uncertainty_factor: 0.590818$' "$out/bracket-band.txt" ||
	! grep -q '^endpoint_drift_multiplier: 1.980516$' "$out/bracket-band.txt" ||
	! grep -q '^band_halfwidth_ns: 12$' "$out/bracket-band.txt" ||
	! grep -q '^band_halfwidth_percent: 11.009$' "$out/bracket-band.txt" ||
	! grep -q '^endpoint_drift_halfwidth_ns: 12$' "$out/bracket-band.txt" ||
	! grep -q '^lower_ns: 97$' "$out/bracket-band.txt" ||
	! grep -q '^upper_ns: 121$' "$out/bracket-band.txt"; then
	echo "calibration band did not use fresh-process dispersion" >&2
	exit 1
fi
printf '101\n103\n105\n107\n109\n111\n113\n115\n117\n' | cmp -s - "$out/calibration-process-medians-ns.txt" || {
	echo "calibration did not preserve fresh-process medians" >&2
	exit 1
}
band_hash="$(sha256sum "$out/bracket-band.txt")"
events_hash="$(sha256sum "$out/bracket-events.txt")"

# A declared band is immutable. Use fresh fake state so an exhausted fake or a
# missing sampler cannot make this assertion pass for the wrong reason. Require
# the exact refusal, unchanged artifacts and zero benchmark calls.
original_state="$FAKE_STATE"
export FAKE_STATE="$scratch/immutability-state"
mkdir -p "$FAKE_STATE"
set +e
immutability_error="$(bash "$SCRIPT" calibrate "$out" --cpus unpinned \
	--processes 9 --samples 3 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" 2>&1)"
immutability_status=$?
set -e
if [ "$immutability_status" = 0 ]; then
	echo "second calibration unexpectedly replaced a declared band" >&2
	exit 1
fi
# Compare against the RESOLVED path the script reports, not the raw one. macOS
# sets TMPDIR with a trailing slash, so "$scratch/pass" carries a double slash
# that `cd && pwd` normalises away — an exact match against the unresolved form
# passes on Linux and fails here, which is a host-specific test rather than a
# portable one.
out_abs="$(cd "$out" && pwd)"
expected_error="error: $out_abs/bracket-band.txt is already declared and immutable; use a fresh output directory"
[ "$immutability_error" = "$expected_error" ] || {
	echo "second calibration failed for the wrong reason: $immutability_error" >&2
	exit 1
}
[ "$(sha256sum "$out/bracket-band.txt")" = "$band_hash" ]
[ "$(sha256sum "$out/bracket-events.txt")" = "$events_hash" ]
[ ! -e "$FAKE_STATE/go-calls" ]
export FAKE_STATE="$original_state"

# The tree may change between declaration and collection without HEAD moving.
# Collection must re-check cleanliness before starting telemetry or a canary.
export FAKE_GIT_DIRTY=1
set +e
dirty_collect_error="$(LEGS=go bash "$SCRIPT" collect "$out" 2>&1)"
dirty_collect_status=$?
set -e
unset FAKE_GIT_DIRTY
if [ "$dirty_collect_status" = 0 ]; then
	echo "dirty worktree unexpectedly started a bracketed collection" >&2
	exit 1
fi
case "$dirty_collect_error" in
*"benchmark provenance requires a clean worktree"*) ;;
*)
	echo "dirty collection failed for the wrong reason: $dirty_collect_error" >&2
	exit 1
	;;
esac
[ ! -e "$FAKE_STATE/mpstat-state" ]
[ ! -e "$FAKE_STATE/collector-ran" ]

# Identical process medians contain no evidence about process-to-process
# dispersion. Calibration must fail closed rather than declare a zero-width
# band that no future process can realistically satisfy.
export FAKE_STATE="$scratch/zero-dispersion-state"
mkdir -p "$FAKE_STATE"
zero_dispersion="$scratch/zero-dispersion"
export FAKE_ZERO_DISPERSION=1
set +e
zero_error="$(bash "$SCRIPT" calibrate "$zero_dispersion" --cpus unpinned \
	--processes 9 --samples 3 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" 2>&1)"
zero_status=$?
set -e
unset FAKE_ZERO_DISPERSION
if [ "$zero_status" = 0 ]; then
	echo "zero-dispersion calibration unexpectedly declared a band" >&2
	exit 1
fi
case "$zero_error" in
*"calibration process medians have zero MAD; cannot estimate a predictive band"*) ;;
*)
	echo "zero-dispersion calibration failed for the wrong reason: $zero_error" >&2
	exit 1
	;;
esac
[ ! -e "$zero_dispersion/bracket-band.txt" ]
export FAKE_STATE="$original_state"

# Pre/post follow the nine calibration processes and therefore assert that
# mpstat is alive for all 18 endpoint processes.
LEGS=go bash "$SCRIPT" collect "$out" >/dev/null
[ "$(cat "$FAKE_STATE/go-calls")" = 27 ] || {
	echo "pre and post canaries did not each use nine fresh processes" >&2
	exit 1
}
grep -q '^result: accepted$' "$out/bracket-verdict.txt"
grep -q '^complete$' "$out/telemetry-status.txt"
grep -q '^telemetry_coverage: pass$' "$out/bracket-verdict.txt"
grep -q '^band_integrity: pass$' "$out/bracket-verdict.txt"
grep -q '^collector saw telemetry$' "$FAKE_STATE/collector-ran"
[ "$(cat "$FAKE_STATE/mpstat-state")" = stopped ]

event_line() {
	grep -n " $1" "$out/bracket-events.txt" | tail -1 | cut -d: -f1
}
telemetry_start="$(event_line telemetry-start)"
pre_start="$(event_line pre-canary-start)"
post_end="$(event_line post-canary-end)"
telemetry_stop="$(event_line telemetry-stop)"
[ "$telemetry_start" -lt "$pre_start" ]
[ "$pre_start" -lt "$post_end" ]
[ "$post_end" -lt "$telemetry_stop" ]

# Pin the gate's deterministic sensitivity, not only its construction. The
# synthetic center is 109 and the upper bound is inclusive at 121: +11.01% is
# still accepted, while 122 (+11.93%) is the first integer median rejected.
sensitivity_edge="$scratch/sensitivity-edge"
cp -a "$out" "$sensitivity_edge"
printf '120\n120\n120\n120\n121\n121\n121\n121\n121\n' >"$sensitivity_edge/pre-canary-process-medians-ns.txt"
cp "$sensitivity_edge/pre-canary-process-medians-ns.txt" "$sensitivity_edge/post-canary-process-medians-ns.txt"
if ! bash "$SCRIPT" evaluate "$sensitivity_edge" >/dev/null; then
	echo "canary at the declared upper edge was unexpectedly rejected" >&2
	exit 1
fi
grep -q '^post_median_ns: 121$' "$sensitivity_edge/bracket-verdict.txt"

sensitivity_reject="$scratch/sensitivity-reject"
cp -a "$out" "$sensitivity_reject"
printf '120\n120\n120\n120\n121\n121\n121\n121\n121\n' >"$sensitivity_reject/pre-canary-process-medians-ns.txt"
printf '121\n121\n121\n121\n122\n122\n122\n122\n122\n' >"$sensitivity_reject/post-canary-process-medians-ns.txt"
if bash "$SCRIPT" evaluate "$sensitivity_reject" >/dev/null; then
	echo "canary beyond the declared sensitivity unexpectedly passed" >&2
	exit 1
fi
grep -q '^post_median_ns: 122$' "$sensitivity_reject/bracket-verdict.txt"
grep -q '^post_decision: fail$' "$sensitivity_reject/bracket-verdict.txt"

# Both endpoints can agree with calibration while disagreeing with each other.
# Pin the paired drift verdict so a host that moves underneath a long collection
# cannot pass merely because each endpoint lands in an opposite side of the band.
drifted="$scratch/drifted"
cp -a "$out" "$drifted"
printf '97\n97\n97\n97\n97\n97\n97\n97\n97\n' >"$drifted/pre-canary-process-medians-ns.txt"
printf '121\n121\n121\n121\n121\n121\n121\n121\n121\n' >"$drifted/post-canary-process-medians-ns.txt"
if bash "$SCRIPT" evaluate "$drifted" >/dev/null; then
	echo "endpoint drift beyond the paired band unexpectedly passed" >&2
	exit 1
fi
grep -q '^pre_decision: pass$' "$drifted/bracket-verdict.txt"
grep -q '^post_decision: pass$' "$drifted/bracket-verdict.txt"
grep -q '^endpoint_drift_ns: 24$' "$drifted/bracket-verdict.txt"
grep -q '^endpoint_drift_decision: fail$' "$drifted/bracket-verdict.txt"

# Evaluation must reject an edited band even when every timing sample passes.
tampered="$scratch/tampered"
cp -a "$out" "$tampered"
printf 'tampered: yes\n' >>"$tampered/bracket-band.txt"
if bash "$SCRIPT" evaluate "$tampered" >/dev/null; then
	echo "edited band unexpectedly retained an accepted verdict" >&2
	exit 1
fi
grep -q '^band_integrity: fail$' "$tampered/bracket-verdict.txt"

# Moving the pre phase after telemetry-stop invalidates the claimed coverage.
out_of_order="$scratch/out-of-order"
cp -a "$out" "$out_of_order"
sed -i 's/ pre-canary-start$/ pre-canary-start-original/' "$out_of_order/bracket-events.txt"
printf '%s pre-canary-start\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >>"$out_of_order/bracket-events.txt"
if bash "$SCRIPT" evaluate "$out_of_order" >/dev/null; then
	echo "out-of-order phases unexpectedly retained an accepted verdict" >&2
	exit 1
fi
grep -q '^telemetry_coverage: fail$' "$out_of_order/bracket-verdict.txt"

# A pre-canary outside the already-declared band stops before the collector.
rm -rf "$FAKE_STATE"
mkdir -p "$FAKE_STATE"
rejected="$scratch/pre-rejected"
bash "$SCRIPT" calibrate "$rejected" --cpus unpinned \
	--processes 9 --samples 3 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" \
	--reference 109:90:130 >/dev/null
export FAKE_GO_BASE=200
if LEGS=go bash "$SCRIPT" collect "$rejected" >/dev/null 2>&1; then
	echo "out-of-band pre-canary unexpectedly started an accepted collection" >&2
	exit 1
fi
if [ -e "$FAKE_STATE/collector-ran" ]; then
	echo "out-of-band pre-canary started the collector" >&2
	exit 1
fi
grep -q '^pre_decision: fail$' "$rejected/bracket-verdict.txt"
grep -q '^collector_exit_status: not-run$' "$rejected/bracket-verdict.txt"
[ "$(cat "$FAKE_STATE/mpstat-state")" = stopped ]
unset FAKE_GO_BASE

# A passing pre-canary cannot save an out-of-band post-canary.
rm -rf "$FAKE_STATE"
mkdir -p "$FAKE_STATE"
post_rejected="$scratch/post-rejected"
bash "$SCRIPT" calibrate "$post_rejected" --cpus unpinned \
	--processes 9 --samples 3 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" \
	--reference 109:90:130 >/dev/null
export FAKE_POST_BASE=200
if LEGS=go bash "$SCRIPT" collect "$post_rejected" >/dev/null; then
	echo "out-of-band post-canary unexpectedly produced an accepted verdict" >&2
	exit 1
fi
grep -q '^pre_decision: pass$' "$post_rejected/bracket-verdict.txt"
grep -q '^post_decision: fail$' "$post_rejected/bracket-verdict.txt"
grep -q '^result: rejected$' "$post_rejected/bracket-verdict.txt"
unset FAKE_POST_BASE

# Kill mpstat inside the collector while allowing the post-canary itself to
# complete. Sampler liveness, not a coincidental canary failure, must reject it.
rm -rf "$FAKE_STATE"
mkdir -p "$FAKE_STATE"
dead_sampler="$scratch/dead-sampler"
bash "$SCRIPT" calibrate "$dead_sampler" --cpus unpinned \
	--processes 9 --samples 3 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" \
	--reference 109:90:130 >/dev/null
export FAKE_KILL_MPSTAT=1
export FAKE_ALLOW_DEAD_POST=1
if LEGS=go bash "$SCRIPT" collect "$dead_sampler" >/dev/null; then
	echo "dead sampler unexpectedly produced an accepted verdict" >&2
	exit 1
fi
grep -q '^post_decision: pass$' "$dead_sampler/bracket-verdict.txt"
grep -q '^telemetry_status: failed$' "$dead_sampler/bracket-verdict.txt"
grep -q '^result: rejected$' "$dead_sampler/bracket-verdict.txt"
unset FAKE_KILL_MPSTAT FAKE_ALLOW_DEAD_POST

# A collector failure still gets a sampled post-canary and an explicit partial
# verdict; telemetry must not die with the collector.
rm -rf "$FAKE_STATE"
mkdir -p "$FAKE_STATE"
failed="$scratch/failed"
bash "$SCRIPT" calibrate "$failed" --cpus unpinned \
	--processes 9 --samples 3 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" \
	--reference 109:90:130 >/dev/null
export FAKE_COLLECTOR_STATUS=7
if LEGS=go bash "$SCRIPT" collect "$failed" >/dev/null; then
	echo "collector failure unexpectedly produced an accepted bracket" >&2
	exit 1
fi
grep -q '^7$' "$failed/collector-exit-status.txt"
grep -q '^post_decision: pass$' "$failed/bracket-verdict.txt"
grep -q '^telemetry_status: complete$' "$failed/bracket-verdict.txt"
grep -q '^result: rejected$' "$failed/bracket-verdict.txt"
[ "$(cat "$FAKE_STATE/mpstat-state")" = stopped ]

echo "run-bracketed protocol test: PASS"
