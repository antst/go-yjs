#!/usr/bin/env bash
# Characterise the predictive-band rule against recorded amd64 canaries. This
# is retrospective validation of the implementation, not permission to change
# any historical verdict: every historical run remains governed by the band it
# declared before collection.
set -euo pipefail

readonly TEST_HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=run-bracketed.sh
source "$TEST_HERE/run-bracketed.sh"

scratch="$(mktemp -d "${TMPDIR:-/tmp}/y-crdt-bracket-history.XXXXXX")"
trap 'rm -rf "$scratch"' EXIT

characterise() {
	local name="$1"
	local medians="$2"
	local expected="$3"
	local medians_file="$scratch/$name-medians.txt"
	printf '%s\n' $medians >"$medians_file"
	local actual
	actual="$(derive_predictive_band "$medians_file" 3 9 "$scratch")"
	[ "$actual" = "$expected" ] || {
		echo "$name: predictive band changed: got '$actual', want '$expected'" >&2
		exit 1
	}
}

# Process medians reconstruct the three fresh-process groups recorded with each
# old 3x5 calibration. The expected bounds pin the real implementation rather
# than duplicating its formula here.
characterise amd64-7013af8 \
	"780925 790372 855868" \
	"790372 9447 14006.122 2 9.924843201 3.853067 0.835543 8.292631 5.863776 116148 82129 674224 906520"

characterise amd64-adc4759-attempt1 \
	"876464 907104 855915" \
	"876464 20549 30465.947 2 9.924843201 3.853067 0.835543 8.292631 5.863776 252643 178646 623821 1129107"
# Its center remains outside the reusable-reference band. A dispersion-aware
# local bracket cannot make two cross-window calibrations comparable.
inside_band 876464 778126 860034 && {
	echo "adc4759 attempt 1 unexpectedly became reference-compatible" >&2
	exit 1
}

characterise amd64-adc4759-attempt2 \
	"819080 814367 827998" \
	"819080 4713 6987.494 2 9.924843201 3.853067 0.835543 8.292631 5.863776 57945 40974 761135 877025"

characterise amd64-dc98ff3 \
	"838149 810037 802124" \
	"810037 7913 11731.814 2 9.924843201 3.853067 0.835543 8.292631 5.863776 97288 68793 712749 907325"

characterise amd64-eb3c6ca-rejected \
	"842947 864325 788041" \
	"842947 21378 31695.023 2 9.924843201 3.853067 0.835543 8.292631 5.863776 262836 185853 580111 1105783"
# eb3c6ca remains rejected. It declared a different sha256-pinned rule before
# collection; choosing this rule after observing its post-canary would turn a
# predeclared criterion into a rationalisation.

# Old endpoint verdicts cannot be re-evaluated under the grouped-canary rule:
# every historical endpoint is one process, while v2 requires k>=9. Applying a
# k=9 interval to a k=1 observation would be false precision.
# These rows therefore pin only how recorded calibration medians feed the v2
# construction. All also had n=3, so scale uncertainty at df=2 is enormous;
# v2 refuses n<9. No old run establishes the discriminating power of v2.

echo "bracket historical characterization: PASS"
