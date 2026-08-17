#!/usr/bin/env bash
# Prove that each load-bearing verdict condition makes the protocol test fail.
# Applying a mutation is itself fail-closed: the exact source line must occur
# once, and the resulting script must differ before its expected failure counts.
set -euo pipefail

readonly HERE="$(cd "$(dirname "$0")" && pwd)"
readonly SOURCE="$HERE/run-bracketed.sh"
readonly PROTOCOL_TEST="$HERE/test-run-bracketed.sh"
scratch="$(mktemp -d "${TMPDIR:-/tmp}/y-crdt-bracket-mutations.XXXXXX")"
mutants=()
cleanup() {
	rm -rf "$scratch"
	local mutant
	for mutant in "${mutants[@]}"; do
		rm -f "$mutant"
	done
}
trap cleanup EXIT

prove_mutation() {
	local name="$1"
	local needle="$2"
	local replacement="$3"
	local expected_failure="$4"
	local mutant
	mutant="$(mktemp "$HERE/.run-bracketed-mutant.${name}.XXXXXX")"
	local log="$scratch/$name.log"
	mutants+=("$mutant")

	awk -v needle="$needle" -v replacement="$replacement" '
		$0 == needle { print replacement; changed++; next }
		{ print }
		END { if (changed != 1) exit 42 }
	' "$SOURCE" >"$mutant" || {
		echo "$name: mutation did not match exactly one source line" >&2
		exit 1
	}
	chmod +x "$mutant"
	if cmp -s "$SOURCE" "$mutant"; then
		echo "$name: mutation left the source unchanged" >&2
		exit 1
	fi

	if BRACKET_SCRIPT="$mutant" bash "$PROTOCOL_TEST" >"$log" 2>&1; then
		echo "$name: broken implementation still passed the protocol test" >&2
		exit 1
	fi
	if ! grep -Fq "$expected_failure" "$log"; then
		echo "$name: protocol failed for the wrong reason" >&2
		sed -n '1,120p' "$log" >&2
		exit 1
	fi
	echo "  caught $name"
}

prove_mutation band-immutability \
	$'\t[ ! -e "$band" ] ||' \
	$'\ttrue ||' \
	'second calibration unexpectedly replaced a declared band'

prove_mutation band-integrity \
	$'\t\t[ "$band_integrity" = pass ] && [ "$telemetry_coverage" = pass ]; then' \
	$'\t\ttrue && [ "$telemetry_coverage" = pass ]; then' \
	'edited band unexpectedly retained an accepted verdict'

prove_mutation telemetry-order \
	$'\t\t[ "$band_integrity" = pass ] && [ "$telemetry_coverage" = pass ]; then' \
	$'\t\t[ "$band_integrity" = pass ] && true; then' \
	'out-of-order phases unexpectedly retained an accepted verdict'

prove_mutation pre-canary-band \
	$'\tif ! inside_band "$pre_median" "$lower" "$upper"; then' \
	$'\tif false; then' \
	'out-of-band pre-canary started the collector'

prove_mutation post-canary-band \
	$'\tif [ "$pre_decision" = pass ] && [ "$post_decision" = pass ] &&' \
	$'\tif [ "$pre_decision" = pass ] && true &&' \
	'out-of-band post-canary unexpectedly produced an accepted verdict'

prove_mutation sampler-liveness \
	$'\t\t[ "$collector_status" = 0 ] && [ "$telemetry_status" = complete ] &&' \
	$'\t\t[ "$collector_status" = 0 ] && true &&' \
	'dead sampler unexpectedly produced an accepted verdict'

prove_mutation collector-success \
	$'\t\t[ "$collector_status" = 0 ] && [ "$telemetry_status" = complete ] &&' \
	$'\t\ttrue && [ "$telemetry_status" = complete ] &&' \
	'collector failure unexpectedly produced an accepted bracket'

prove_mutation canary-package \
	$'\t\t\t-count="$samples" -timeout=30m ./crdt' \
	$'\t\t\t-count="$samples" -timeout=30m .' \
	'MapSet canary did not target ./crdt'

echo "run-bracketed mutation tests: PASS"
