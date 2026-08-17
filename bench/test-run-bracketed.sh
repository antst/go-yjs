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
calls=0
[ ! -f "$counter_file" ] || calls="$(cat "$counter_file")"
calls=$((calls + 1))
echo "$calls" >"$counter_file"
printf "%s\n" "$@" >"$FAKE_STATE/go-args-$calls"
if [ "$calls" -eq 2 ] || { [ "$calls" -gt 2 ] && [ -z "${FAKE_ALLOW_DEAD_POST:-}" ]; }; then
	[ "$(cat "$FAKE_STATE/mpstat-state")" = running ]
fi
samples=1
for arg in "$@"; do
	case "$arg" in -count=*) samples="${arg#-count=}" ;; esac
done
for ((i = 0; i < samples; i++)); do
	base="${FAKE_GO_BASE:-100}"
	if [ "$calls" -gt 2 ] && [ -n "${FAKE_POST_BASE:-}" ]; then base="$FAKE_POST_BASE"; fi
	value=$((base + i))
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

export FAKE_STATE="$scratch/state"
mkdir -p "$FAKE_STATE"
export BRACKET_GO="$scratch/bin/fake-go"
export BRACKET_MPSTAT="$scratch/bin/fake-mpstat"
export BRACKET_RUN_ALL="$scratch/bin/fake-run-all"

out="$scratch/pass"
bash "$SCRIPT" calibrate "$out" --cpus unpinned \
	--processes 1 --samples 3 --tolerance-percent 10 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" \
	--reference 101:90:111 >/dev/null
grep -Fxq './crdt' "$FAKE_STATE/go-args-1" || {
	echo "MapSet canary did not target ./crdt" >&2
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
	--processes 1 --samples 3 --tolerance-percent 20 \
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

# Pre/post are calls 2 and 3 and therefore assert that mpstat is alive.
echo 1 >"$FAKE_STATE/go-calls"
LEGS=go bash "$SCRIPT" collect "$out" >/dev/null
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
	--processes 1 --samples 3 --tolerance-percent 10 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" \
	--reference 101:90:111 >/dev/null
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
	--processes 1 --samples 3 --tolerance-percent 10 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" \
	--reference 101:90:111 >/dev/null
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
	--processes 1 --samples 3 --tolerance-percent 10 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" \
	--reference 101:90:111 >/dev/null
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
	--processes 1 --samples 3 --tolerance-percent 10 \
	--telemetry-interval 1 --pressure-dir "$scratch/pressure" \
	--reference 101:90:111 >/dev/null
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
