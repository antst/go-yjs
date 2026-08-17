#!/usr/bin/env bash
# Collect a benchmark leg inside a predeclared, continuously sampled host bracket.
#
# This protocol is intentionally split into separate commands:
#
#   bash bench/run-bracketed.sh calibrate OUT --cpus 1,4,9,38
#   LEGS=go bash bench/run-bracketed.sh collect OUT
#   bash bench/run-bracketed.sh evaluate OUT
#
# `calibrate` writes bracket-band.txt BEFORE any implementation leg runs.
# `collect` refuses to recompute that band: it reads the recorded criterion,
# starts mpstat and PSI before the pre-canary, and stops them only after the
# post-canary. `evaluate` reads the same immutable band and the raw canaries.
# Separating declaration from observation is what makes grouped-median endpoint
# and paired-drift acceptance predeclared criteria rather than rules chosen
# after seeing the samples.
#
# CPU set, process/sample counts and telemetry interval are inputs. The band is
# derived from dispersion between fresh-process medians, not from a fixed
# tolerance: pooled samples would pretend measurements in one process are
# independent observations of process-to-process host noise.
# Pass `--cpus unpinned` deliberately for an unpinned run. Reference columns may
# be reused only when their recorded calibration agrees:
#
#   ... calibrate OUT --cpus 1,4,9,38 \
#       --reference 819080:778126:860034
#
# The reference argument is MEDIAN:LOWER:UPPER in ns/op. A partial-LEGS run
# requires a recorded reference PASS; collecting all four implementations needs
# no reference because no cross-window splice is made. For an explicitly
# unpaired diagnostic, invoke run-all.sh directly rather than manufacturing a
# bracketed comparison without its other half.
set -euo pipefail

readonly HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly ROOT="$(cd "$HERE/.." && pwd)"
readonly BAND_FORMAT="y-crdt-benchmark-bracket-v2"
readonly BAND_NAME="bracket-band.txt"
readonly BAND_METHOD="fresh-process-group-median-mad-student-t-99pct"
readonly ACCEPTANCE_RULE="grouped-medians-plus-paired-drift"
readonly PREDICTION_LEVEL_PERCENT=99
readonly PREDICTION_Z=2.5758293035489004
readonly MAD_NORMAL_SCALE=1.4826
readonly PI=3.141592653589793

die() {
	echo "error: $*" >&2
	exit 2
}

usage() {
	cat >&2 <<'EOF'
usage:
  bench/run-bracketed.sh calibrate OUT --cpus LIST [options]
  bench/run-bracketed.sh collect OUT
  bench/run-bracketed.sh evaluate OUT

calibrate options:
  --cpus LIST                  taskset CPU list, or the literal "unpinned"
  --processes N                odd fresh calibration processes >=9 (default 9)
  --canary-processes K         odd fresh processes per endpoint >=9 (default 9)
  --samples N                  odd samples per process and canary (default 11)
  --telemetry-interval SEC     mpstat/PSI interval (default 10)
  --pressure-dir DIR           PSI directory (default /sys/fs/cgroup)
  --reference MEDIAN:LOW:HIGH  optional reusable-reference calibration

Environment:
  LEGS                         passed through to run-all.sh (default all four)
  BRACKET_GO                   Go command, used by the protocol self-test
  BRACKET_GIT                  Git command, used by the protocol self-test
  BRACKET_MPSTAT               mpstat command, used by the protocol self-test
  BRACKET_RUN_ALL              collector path, used by the protocol self-test
EOF
	exit 2
}

utc_now() {
	date -u '+%Y-%m-%dT%H:%M:%SZ'
}

require_positive_integer() {
	local label="$1"
	local value="$2"
	case "$value" in
	'' | *[!0-9]*) die "$label must be a positive integer (got '$value')" ;;
	esac
	[ "$value" -gt 0 ] || die "$label must be greater than zero"
}

require_nonnegative_number() {
	local label="$1"
	local value="$2"
	awk -v value="$value" 'BEGIN { exit !(value ~ /^[0-9]+([.][0-9]+)?$/) }' ||
		die "$label must be a non-negative number (got '$value')"
}

require_odd_at_least_three() {
	local label="$1"
	local value="$2"
	require_positive_integer "$label" "$value"
	[ "$value" -ge 3 ] && [ $((value % 2)) -eq 1 ] ||
		die "$label must be an odd integer of at least 3 (got '$value')"
}

require_odd_at_least_nine() {
	local label="$1"
	local value="$2"
	require_positive_integer "$label" "$value"
	[ "$value" -ge 9 ] && [ $((value % 2)) -eq 1 ] ||
		die "$label must be an odd integer of at least 9 (got '$value')"
}

band_value() {
	local key="$1"
	local band="$2"
	awk -v key="$key" 'index($0, key ": ") == 1 { print substr($0, length(key) + 3); found++ }
		END { if (found != 1) exit 1 }' "$band"
}

repo_git() {
	"${BRACKET_GIT:-git}" -C "$ROOT" "$@"
}

require_clean_worktree() {
	local status
	status="$(repo_git status --porcelain=v1 --untracked-files=normal)" ||
		die "cannot inspect the repository worktree"
	[ -z "$status" ] ||
		die "benchmark provenance requires a clean worktree; commit or remove every tracked and untracked change first"
}

record_event() {
	local out="$1"
	shift
	printf '%s %s\n' "$(utc_now)" "$*" >>"$out/bracket-events.txt"
}

run_on_cpus() {
	local cpus="$1"
	shift
	if [ "$cpus" = "unpinned" ]; then
		"$@"
	else
		taskset -c "$cpus" "$@"
	fi
}

run_mapset() {
	local cpus="$1"
	local samples="$2"
	local destination="$3"
	local go_command="${BRACKET_GO:-go}"
	(
		cd "$ROOT"
		run_on_cpus "$cpus" "$go_command" test -run '^$' \
			-bench '^BenchmarkMapSet$' -benchmem -benchtime=1s \
			-count="$samples" -timeout=30m ./crdt
	) >"$destination" 2>&1
}

extract_mapset_ns() {
	local source="$1"
	awk '
		/^BenchmarkMapSet(-[0-9]+)?[[:space:]]/ {
			if ($3 !~ /^[0-9]+([.][0-9]+)?$/ || $4 != "ns/op") exit 2
			print $3
		}
	' "$source"
}

median_from_sorted() {
	local source="$1"
	local count
	count="$(wc -l <"$source" | tr -d ' ')"
	require_positive_integer "sample count" "$count"
	awk -v count="$count" '
		NR == int((count + 1) / 2) { left = $1 }
		NR == int((count + 2) / 2) { right = $1 }
		END {
			if (count % 2) printf "%.0f\n", left
			else printf "%.0f\n", (left + right) / 2
		}
	' "$source"
}

sample_median() {
	local source="$1"
	local expected="$2"
	local scratch="$3"
	extract_mapset_ns "$source" | sort -n >"$scratch"
	local actual
	actual="$(wc -l <"$scratch" | tr -d ' ')"
	[ "$actual" = "$expected" ] ||
		die "$source contains $actual MapSet samples, want $expected"
	median_from_sorted "$scratch"
}

# Derive a predictive interval for a future median of fresh-process medians.
# MAD is normalised by 1.4826. A Student-t-like quantile with processes-1
# degrees of freedom accounts for uncertainty in the estimated scale; treating
# MAD from nine observations as known would make an accidentally small MAD
# produce the same false rejections this protocol is intended to prevent. The
# sqrt term separately accounts for uncertainty in the calibration center.
#
# The inputs here are medians, whose distribution is appreciably nearer normal
# than the right-skewed raw timings. "Robust" describes the center and scale
# estimators, not a claim that this symmetric interval is distribution-free.
# Every approximation and factor is recorded in the immutable band.
derive_predictive_band() {
	local process_medians="$1"
	local processes="$2"
	local canary_processes="$3"
	local scratch="$4"
	local actual center mad
	actual="$(wc -l <"$process_medians" | tr -d ' ')"
	[ "$actual" = "$processes" ] ||
		die "$process_medians contains $actual process medians, want $processes"
	sort -n "$process_medians" >"$scratch/process-medians.sorted"
	center="$(median_from_sorted "$scratch/process-medians.sorted")"
	awk -v center="$center" '{ delta = $1 - center; if (delta < 0) delta = -delta; print delta }' \
		"$process_medians" | sort -n >"$scratch/process-median-deviations.sorted"
	mad="$(median_from_sorted "$scratch/process-median-deviations.sorted")"
	[ "$mad" -gt 0 ] ||
		die "calibration process medians have zero MAD; cannot estimate a predictive band"
	awk -v center="$center" -v mad="$mad" -v processes="$processes" -v canaryProcesses="$canary_processes" \
		-v scale="$MAD_NORMAL_SCALE" -v z="$PREDICTION_Z" -v pi="$PI" '
		BEGIN {
			df = processes - 1
			# Exact lower-df quantiles keep historical 3x5 characterisation
			# meaningful. Production requires df>=8, where this order-three
			# Cornish-Fisher expansion is within 0.1% of t(0.995, df).
			if (df == 2) student = 9.924843200918070
			else if (df == 4) student = 4.604094871415897
			else if (df == 6) student = 3.707428021324907
			else student = z + (z^3 + z) / (4 * df) + (5 * z^5 + 16 * z^3 + 3 * z) / (96 * df^2) + (3 * z^7 + 19 * z^5 + 17 * z^3 - 15 * z) / (384 * df^3)
			normalized = scale * mad
			scaleInflation = student / z
			# Both endpoints are medians of canaryProcesses fresh-process
			# medians. Their variance shrinks with k just as the calibration
			# center shrinks with n; a single-process canary would leave a
			# full sigma^2 floor no amount of calibration could reduce.
			centerFactor = sqrt(pi / (2 * canaryProcesses) + pi / (2 * processes))
			totalMultiplier = student * centerFactor
			width = normalized * totalMultiplier
			driftMultiplier = student * sqrt(pi / canaryProcesses)
			driftWidth = normalized * driftMultiplier
			half = int(width)
			if (half < width) half++
			driftHalf = int(driftWidth)
			if (driftHalf < driftWidth) driftHalf++
			lower = center - half
			if (lower < 0) lower = 0
			upper = center + half
			printf "%.0f %.0f %.3f %.0f %.9f %.6f %.6f %.6f %.6f %.0f %.0f %.0f %.0f\n",
				center, mad, normalized, df, student, scaleInflation,
				centerFactor, totalMultiplier, driftMultiplier, half, driftHalf, lower, upper
		}'
}

run_mapset_group() {
	local cpus="$1"
	local processes="$2"
	local samples="$3"
	local destination="$4"
	local process_medians="$5"
	local round_raw="${destination}.round.tmp"
	local round_sorted="${destination}.round-sorted.tmp"
	: >"$destination"
	: >"$process_medians"
	for ((round = 1; round <= processes; round++)); do
		printf '== process %d ==\n' "$round" >>"$destination"
		local round_status=0
		run_mapset "$cpus" "$samples" "$round_raw" || round_status=$?
		if [ "$round_status" != 0 ]; then
			rm -f "$round_raw" "$round_sorted"
			return "$round_status"
		fi
		cat "$round_raw" >>"$destination"
		sample_median "$round_raw" "$samples" "$round_sorted" >>"$process_medians"
	done
	rm -f "$round_raw" "$round_sorted"
}

recorded_process_median() {
	local source="$1"
	local expected="$2"
	local scratch="$3"
	awk '/^[0-9]+([.][0-9]+)?$/ { print; next } { exit 2 }' "$source" | sort -n >"$scratch"
	local actual
	actual="$(wc -l <"$scratch" | tr -d ' ')"
	[ "$actual" = "$expected" ] ||
		die "$source contains $actual process medians, want $expected"
	median_from_sorted "$scratch"
}

recorded_process_median_if_complete() {
	local source="$1"
	local expected="$2"
	local scratch="$3"
	awk '/^[0-9]+([.][0-9]+)?$/ { print; next } { exit 2 }' "$source" | sort -n >"$scratch" || return 1
	local actual
	actual="$(wc -l <"$scratch" | tr -d ' ')"
	[ "$actual" = "$expected" ] || return 1
	median_from_sorted "$scratch"
}

inside_band() {
	local value="$1"
	local lower="$2"
	local upper="$3"
	awk -v value="$value" -v lower="$lower" -v upper="$upper" \
		'BEGIN { exit !(value >= lower && value <= upper) }'
}

all_legs_selected() {
	local legs=",${LEGS:-go,yjs,yrs,ygo},"
	for leg in go yjs yrs ygo; do
		case "$legs" in
		*",$leg,"*) ;;
		*) return 1 ;;
		esac
	done
}

calibrate() {
	[ "$#" -ge 1 ] || usage
	local out="$1"
	shift
	local cpus=""
	local processes=9
	local canary_processes=9
	local samples=11
	local telemetry_interval=10
	local pressure_dir=/sys/fs/cgroup
	local reference=""
	local go_command="${BRACKET_GO:-go}"

	while [ "$#" -gt 0 ]; do
		case "$1" in
		--cpus | --processes | --canary-processes | --samples | --telemetry-interval | --pressure-dir | --reference)
			[ "$#" -ge 2 ] || die "$1 requires a value"
			case "$1" in
			--cpus) cpus="$2" ;;
			--processes) processes="$2" ;;
			--canary-processes) canary_processes="$2" ;;
			--samples) samples="$2" ;;
			--telemetry-interval) telemetry_interval="$2" ;;
			--pressure-dir) pressure_dir="$2" ;;
			--reference) reference="$2" ;;
			esac
			shift 2
			;;
		*) die "unknown calibrate option '$1'" ;;
		esac
	done

	[ -n "$cpus" ] || die "--cpus is required (use --cpus unpinned deliberately)"
	require_clean_worktree
	require_odd_at_least_nine "--processes" "$processes"
	require_odd_at_least_nine "--canary-processes" "$canary_processes"
	require_odd_at_least_three "--samples" "$samples"
	require_positive_integer "--telemetry-interval" "$telemetry_interval"
	command -v "$go_command" >/dev/null 2>&1 || die "Go command not found"
	if [ "$cpus" != "unpinned" ]; then
		command -v taskset >/dev/null 2>&1 || die "taskset is required for --cpus $cpus"
		taskset -c "$cpus" true >/dev/null 2>&1 || die "invalid or unavailable CPU set '$cpus'"
	fi

	local reference_median=""
	local reference_lower=""
	local reference_upper=""
	if [ -n "$reference" ]; then
		IFS=: read -r reference_median reference_lower reference_upper extra <<<"$reference"
		[ -n "$reference_median" ] && [ -n "$reference_lower" ] &&
			[ -n "$reference_upper" ] && [ -z "${extra:-}" ] ||
			die "--reference must be MEDIAN:LOWER:UPPER"
		require_nonnegative_number "reference median" "$reference_median"
		require_nonnegative_number "reference lower bound" "$reference_lower"
		require_nonnegative_number "reference upper bound" "$reference_upper"
		awk -v lower="$reference_lower" -v median="$reference_median" -v upper="$reference_upper" \
			'BEGIN { exit !(lower <= median && median <= upper) }' ||
			die "reference bounds must contain the reference median"
	fi

	mkdir -p "$out"
	local out_abs
	out_abs="$(cd "$out" && pwd)"
	local band="$out_abs/$BAND_NAME"
	[ ! -e "$band" ] ||
		die "$band is already declared and immutable; use a fresh output directory"
	local scratch
	scratch="$(mktemp -d "${TMPDIR:-/tmp}/y-crdt-bracket-calibrate.XXXXXX")"
	trap 'rm -rf "${scratch:-}"' EXIT
	local raw="$scratch/calibration-mapset.txt"
	local process_medians="$scratch/calibration-process-medians-ns.txt"
	: >"$raw"
	: >"$process_medians"
	record_event "$out_abs" "calibration-start cpus=$cpus processes=$processes canary_processes=$canary_processes samples=$samples"
	run_mapset_group "$cpus" "$processes" "$samples" "$raw" "$process_medians"

	local sorted="$scratch/calibration-mapset-sorted-ns.txt"
	extract_mapset_ns "$raw" | sort -n >"$sorted"
	local expected=$((processes * samples))
	local actual
	actual="$(wc -l <"$sorted" | tr -d ' ')"
	[ "$actual" = "$expected" ] ||
		die "calibration produced $actual MapSet samples, want $expected"
	local median mad normalized_scale scale_df student_quantile scale_inflation
	local center_factor total_multiplier drift_multiplier halfwidth drift_halfwidth lower upper
	read -r median mad normalized_scale scale_df student_quantile scale_inflation \
		center_factor total_multiplier drift_multiplier halfwidth drift_halfwidth lower upper < <(
		derive_predictive_band "$process_medians" "$processes" "$canary_processes" "$scratch"
	)

	local reference_decision=not-requested
	if [ -n "$reference" ]; then
		if inside_band "$median" "$reference_lower" "$reference_upper"; then
			reference_decision=pass
		else
			reference_decision=fail
		fi
	fi

	local candidate="$scratch/$BAND_NAME"
	local cpu_model
	cpu_model="$(awk -F: '/model name/ { sub(/^[[:space:]]+/, "", $2); print $2; exit }' /proc/cpuinfo 2>/dev/null || true)"
	[ -n "$cpu_model" ] || cpu_model=unknown
	{
		echo "format: $BAND_FORMAT"
		echo "declared_at: $(utc_now)"
		echo "commit: $(repo_git rev-parse HEAD)"
		echo "hostname: $(hostname 2>/dev/null || echo unknown)"
		echo "host: $(uname -a 2>/dev/null || echo unknown)"
		echo "go_version: $("$go_command" version 2>/dev/null || echo unknown)"
		echo "cpu_model: $cpu_model"
		echo "cpus: $cpus"
		echo "calibration_processes: $processes"
		echo "canary_processes: $canary_processes"
		echo "samples_per_process: $samples"
		echo "telemetry_interval_seconds: $telemetry_interval"
		echo "pressure_dir: $pressure_dir"
		echo "benchmark: BenchmarkMapSet"
		echo "benchtime: 1s"
		echo "acceptance: $ACCEPTANCE_RULE"
		echo "band_method: $BAND_METHOD"
		echo "prediction_level_percent: $PREDICTION_LEVEL_PERCENT"
		echo "prediction_normal_quantile: $PREDICTION_Z"
		echo "mad_normal_scale: $MAD_NORMAL_SCALE"
		echo "scale_degrees_of_freedom: $scale_df"
		echo "prediction_student_t_quantile: $student_quantile"
		echo "scale_uncertainty_inflation: $scale_inflation"
		echo "center_uncertainty_factor: $center_factor"
		echo "prediction_total_multiplier: $total_multiplier"
		echo "endpoint_drift_multiplier: $drift_multiplier"
		echo "calibration_center_ns: $median"
		echo "calibration_process_mad_ns: $mad"
		echo "calibration_process_scale_ns: $normalized_scale"
		echo "band_halfwidth_ns: $halfwidth"
		echo "band_halfwidth_percent: $(awk -v half="$halfwidth" -v center="$median" 'BEGIN { printf "%.3f", 100 * half / center }')"
		echo "endpoint_drift_halfwidth_ns: $drift_halfwidth"
		echo "endpoint_drift_halfwidth_percent: $(awk -v half="$drift_halfwidth" -v center="$median" 'BEGIN { printf "%.3f", 100 * half / center }')"
		echo "lower_ns: $lower"
		echo "upper_ns: $upper"
		echo "reference_median_ns: $reference_median"
		echo "reference_lower_ns: $reference_lower"
		echo "reference_upper_ns: $reference_upper"
		echo "reference_decision: $reference_decision"
	} >"$candidate"

	# Copy the raw evidence first, then atomically declare the criterion. A
	# collector can never observe a partially written band, and calibration
	# refuses to alter a band once declared.
	cp "$raw" "$out_abs/calibration-mapset.txt"
	cp "$sorted" "$out_abs/calibration-mapset-sorted-ns.txt"
	cp "$process_medians" "$out_abs/calibration-process-medians-ns.txt"
	mv "$candidate" "$band"
	record_event "$out_abs" "band-declared sha256=$(sha256sum "$band" | awk '{print $1}') center=$median mad=$mad lower=$lower upper=$upper acceptance=$ACCEPTANCE_RULE"

	cat "$band"
	if [ "$reference_decision" = fail ]; then
		echo "reference calibration FAIL: $median is outside $reference_lower..$reference_upper" >&2
		exit 3
	fi
}

pressure_sampler() {
	local pressure_dir="$1"
	local interval="$2"
	local destination="$3"
	local sleep_pid=""
	trap 'if [ -n "$sleep_pid" ]; then kill "$sleep_pid" 2>/dev/null || true; fi; exit 0' TERM INT
	while :; do
		{
			utc_now
			uptime 2>/dev/null || true
			for resource in cpu io memory; do
				while IFS= read -r line; do
					printf '%s %s\n' "$resource" "$line"
				done <"$pressure_dir/$resource.pressure"
			done
		} >>"$destination"
		sleep "$interval" &
		sleep_pid=$!
		wait "$sleep_pid" || true
		sleep_pid=""
	done
}

MPSTAT_PID=""
PRESSURE_PID=""
TELEMETRY_RUNNING=0

stop_telemetry() {
	[ "$TELEMETRY_RUNNING" = 1 ] || return 0
	local status=0
	for pid in "$MPSTAT_PID" "$PRESSURE_PID"; do
		if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
			kill "$pid" 2>/dev/null || true
		fi
	done
	for pid in "$MPSTAT_PID" "$PRESSURE_PID"; do
		if [ -n "$pid" ]; then
			wait "$pid" 2>/dev/null || true
		fi
	done
	TELEMETRY_RUNNING=0
	return "$status"
}

start_telemetry() {
	local out="$1"
	local cpus="$2"
	local pressure_dir="$3"
	local interval="$4"
	local mpstat_command="${BRACKET_MPSTAT:-mpstat}"
	local mpstat_cpus="$cpus"
	[ "$mpstat_cpus" != "unpinned" ] || mpstat_cpus=ALL

	command -v "$mpstat_command" >/dev/null 2>&1 || {
		echo "error: mpstat is required; refusing an uninstrumented bracket" >&2
		return 1
	}
	command -v stdbuf >/dev/null 2>&1 || {
		echo "error: stdbuf is required to preserve mpstat output on interruption" >&2
		return 1
	}
	for resource in cpu io memory; do
		[ -r "$pressure_dir/$resource.pressure" ] || {
			echo "error: missing PSI sampler input $pressure_dir/$resource.pressure" >&2
			return 1
		}
	done

	: >"$out/during-mpstat.txt"
	: >"$out/during-pressure.txt"
	stdbuf -oL -eL "$mpstat_command" -P "$mpstat_cpus" "$interval" \
		>"$out/during-mpstat.txt" 2>&1 &
	MPSTAT_PID=$!
	pressure_sampler "$pressure_dir" "$interval" "$out/during-pressure.txt" &
	PRESSURE_PID=$!
	TELEMETRY_RUNNING=1
	# PSI writes immediately; checking both processes prevents a missing sampler
	# from becoming a review-time discovery after an hour-long run.
	for _ in 1 2 3 4 5 6 7 8 9 10; do
		[ -s "$out/during-pressure.txt" ] && [ -s "$out/during-mpstat.txt" ] && break
		sleep 0.1
	done
	kill -0 "$MPSTAT_PID" 2>/dev/null || {
		echo "error: mpstat sampler exited before the pre-canary" >&2
		return 1
	}
	kill -0 "$PRESSURE_PID" 2>/dev/null || {
		echo "error: PSI sampler exited before the pre-canary" >&2
		return 1
	}
	[ -s "$out/during-pressure.txt" ] || {
		echo "error: PSI sampler produced no data before the pre-canary" >&2
		return 1
	}
	[ -s "$out/during-mpstat.txt" ] || {
		echo "error: mpstat sampler produced no data before the pre-canary" >&2
		return 1
	}
}

write_verdict() {
	local out="$1"
	local band="$out/$BAND_NAME"
	local canary_processes lower upper drift_halfwidth acceptance
	canary_processes="$(band_value canary_processes "$band")"
	lower="$(band_value lower_ns "$band")"
	upper="$(band_value upper_ns "$band")"
	drift_halfwidth="$(band_value endpoint_drift_halfwidth_ns "$band")"
	acceptance="$(band_value acceptance "$band")"
	[ "$acceptance" = "$ACCEPTANCE_RULE" ] || die "unsupported acceptance rule '$acceptance'"

	local pre_median="missing"
	local post_median="missing"
	local pre_decision=missing
	local post_decision=missing
	local pre_outside=missing
	local post_outside=missing
	local pre_spread=missing
	local post_spread=missing
	local endpoint_drift=missing
	local endpoint_drift_decision=missing
	local scratch
	scratch="$(mktemp -d "${TMPDIR:-/tmp}/y-crdt-bracket-evaluate.XXXXXX")"
	if [ -s "$out/pre-canary-process-medians-ns.txt" ]; then
		if pre_median="$(recorded_process_median_if_complete "$out/pre-canary-process-medians-ns.txt" "$canary_processes" "$scratch/pre.sorted")"; then
			if inside_band "$pre_median" "$lower" "$upper"; then pre_decision=pass; else pre_decision=fail; fi
			read -r pre_outside pre_spread < <(awk -v median="$pre_median" -v lower="$lower" -v upper="$upper" '
				NR == 1 { min = $1 } { max = $1; if ($1 < lower || $1 > upper) outside++ }
				END { printf "%d %.2f\n", outside + 0, 100 * (max - min) / median }
			' "$scratch/pre.sorted")
		else
			pre_median=invalid
			pre_decision=invalid
		fi
	fi
	if [ -s "$out/post-canary-process-medians-ns.txt" ]; then
		if post_median="$(recorded_process_median_if_complete "$out/post-canary-process-medians-ns.txt" "$canary_processes" "$scratch/post.sorted")"; then
			if inside_band "$post_median" "$lower" "$upper"; then post_decision=pass; else post_decision=fail; fi
			read -r post_outside post_spread < <(awk -v median="$post_median" -v lower="$lower" -v upper="$upper" '
				NR == 1 { min = $1 } { max = $1; if ($1 < lower || $1 > upper) outside++ }
				END { printf "%d %.2f\n", outside + 0, 100 * (max - min) / median }
			' "$scratch/post.sorted")
		else
			post_median=invalid
			post_decision=invalid
		fi
	fi
	if [[ "$pre_median" =~ ^[0-9]+$ ]] && [[ "$post_median" =~ ^[0-9]+$ ]]; then
		endpoint_drift=$((post_median - pre_median))
		[ "$endpoint_drift" -ge 0 ] || endpoint_drift=$((-endpoint_drift))
		if [ "$endpoint_drift" -le "$drift_halfwidth" ]; then
			endpoint_drift_decision=pass
		else
			endpoint_drift_decision=fail
		fi
	fi

	local collector_status=not-run
	[ ! -s "$out/collector-exit-status.txt" ] || collector_status="$(tr -d '[:space:]' <"$out/collector-exit-status.txt")"
	local telemetry_status=missing
	[ ! -s "$out/telemetry-status.txt" ] || telemetry_status="$(tr -d '[:space:]' <"$out/telemetry-status.txt")"
	local band_integrity=missing
	if [ -s "$out/bracket-band.sha256" ]; then
		local recorded_hash actual_hash
		recorded_hash="$(awk 'NR == 1 { print $1 }' "$out/bracket-band.sha256")"
		actual_hash="$(sha256sum "$band" | awk '{print $1}')"
		if [ "$recorded_hash" = "$actual_hash" ]; then band_integrity=pass; else band_integrity=fail; fi
	fi
	local telemetry_coverage=missing
	if [ -s "$out/bracket-events.txt" ]; then
		local telemetry_start pre_start post_end telemetry_stop
		telemetry_start="$(grep -n ' telemetry-start ' "$out/bracket-events.txt" | tail -1 | cut -d: -f1 || true)"
		pre_start="$(grep -n ' pre-canary-start' "$out/bracket-events.txt" | tail -1 | cut -d: -f1 || true)"
		post_end="$(grep -n ' post-canary-end ' "$out/bracket-events.txt" | tail -1 | cut -d: -f1 || true)"
		telemetry_stop="$(grep -n ' telemetry-stop ' "$out/bracket-events.txt" | tail -1 | cut -d: -f1 || true)"
		if [ -n "$telemetry_start" ] && [ -n "$pre_start" ] && [ -n "$post_end" ] &&
			[ -n "$telemetry_stop" ] && [ "$telemetry_start" -lt "$pre_start" ] &&
			[ "$pre_start" -lt "$post_end" ] && [ "$post_end" -lt "$telemetry_stop" ]; then
			telemetry_coverage=pass
		else
			telemetry_coverage=fail
		fi
	fi
	local result=rejected
	if [ "$pre_decision" = pass ] && [ "$post_decision" = pass ] &&
		[ "$endpoint_drift_decision" = pass ] &&
		[ "$collector_status" = 0 ] && [ "$telemetry_status" = complete ] &&
		[ "$band_integrity" = pass ] && [ "$telemetry_coverage" = pass ]; then
		result=accepted
	fi
	{
		echo "format: $BAND_FORMAT"
		echo "evaluated_at: $(utc_now)"
		echo "acceptance: $acceptance (predeclared in $BAND_NAME)"
		echo "band_ns: $lower..$upper"
		echo "pre_median_ns: $pre_median"
		echo "pre_decision: $pre_decision"
		echo "pre_process_medians_outside_band: $pre_outside/$canary_processes"
		echo "pre_process_median_spread_percent: $pre_spread"
		echo "post_median_ns: $post_median"
		echo "post_decision: $post_decision"
		echo "post_process_medians_outside_band: $post_outside/$canary_processes"
		echo "post_process_median_spread_percent: $post_spread"
		echo "endpoint_drift_ns: $endpoint_drift"
		echo "endpoint_drift_band_ns: 0..$drift_halfwidth"
		echo "endpoint_drift_decision: $endpoint_drift_decision"
		echo "collector_exit_status: $collector_status"
		echo "telemetry_status: $telemetry_status"
		echo "telemetry_coverage: $telemetry_coverage"
		echo "band_integrity: $band_integrity"
		echo "result: $result"
	} >"$out/bracket-verdict.txt.tmp"
	mv "$out/bracket-verdict.txt.tmp" "$out/bracket-verdict.txt"
	cat "$out/bracket-verdict.txt"
	rm -rf "$scratch"
	[ "$result" = accepted ]
}

collect() {
	[ "$#" = 1 ] || usage
	local out="$1"
	[ -d "$out" ] || die "output directory '$out' does not exist; run calibrate first"
	require_clean_worktree
	local out_abs
	out_abs="$(cd "$out" && pwd)"
	local band="$out_abs/$BAND_NAME"
	[ -s "$band" ] || die "$band is missing; run calibrate first"
	[ "$(band_value format "$band")" = "$BAND_FORMAT" ] || die "unsupported band format"
	[ "$(band_value commit "$band")" = "$(repo_git rev-parse HEAD)" ] ||
		die "current commit differs from the commit recorded in $band"
	local cpus samples canary_processes interval pressure_dir reference_decision
	cpus="$(band_value cpus "$band")"
	samples="$(band_value samples_per_process "$band")"
	canary_processes="$(band_value canary_processes "$band")"
	interval="$(band_value telemetry_interval_seconds "$band")"
	pressure_dir="$(band_value pressure_dir "$band")"
	reference_decision="$(band_value reference_decision "$band")"
	if [ "$reference_decision" != pass ] && ! all_legs_selected; then
		die "partial LEGS requires a recorded reference calibration PASS; got '$reference_decision'"
	fi
	for artifact in pre-canary.txt pre-canary-process-medians-ns.txt post-canary.txt post-canary-process-medians-ns.txt during-mpstat.txt during-pressure.txt \
		run.log collector-exit-status.txt telemetry-status.txt bracket-verdict.txt; do
		[ ! -e "$out_abs/$artifact" ] || die "$out_abs/$artifact already exists; use a fresh output directory"
	done
	if [ "$cpus" != unpinned ]; then
		command -v taskset >/dev/null 2>&1 || die "taskset is required for recorded CPU set $cpus"
		taskset -c "$cpus" true >/dev/null 2>&1 || die "recorded CPU set '$cpus' is unavailable"
	fi

	local band_hash
	band_hash="$(sha256sum "$band" | awk '{print $1}')"
	echo "$band_hash  $BAND_NAME" >"$out_abs/bracket-band.sha256"
	record_event "$out_abs" "collection-start band_sha256=$band_hash legs=${LEGS:-go,yjs,yrs,ygo}"
	{
		utc_now
		uptime 2>/dev/null || true
	} >"$out_abs/uptime-before-bracket.txt"

	trap 'stop_telemetry' EXIT
	trap 'exit 130' INT TERM HUP
	if ! start_telemetry "$out_abs" "$cpus" "$pressure_dir" "$interval"; then
		echo not-run >"$out_abs/collector-exit-status.txt"
		echo failed >"$out_abs/telemetry-status.txt"
		record_event "$out_abs" "telemetry-start-failed"
		stop_telemetry
		record_event "$out_abs" "telemetry-stop status=failed reason=start-failed"
		write_verdict "$out_abs" || true
		die "telemetry failed before the pre-canary; collector not started"
	fi
	record_event "$out_abs" "telemetry-start mpstat_pid=$MPSTAT_PID pressure_pid=$PRESSURE_PID"

	record_event "$out_abs" "pre-canary-start"
	local pre_status=0
	run_mapset_group "$cpus" "$canary_processes" "$samples" \
		"$out_abs/pre-canary.txt" "$out_abs/pre-canary-process-medians-ns.txt" || pre_status=$?
	record_event "$out_abs" "pre-canary-end status=$pre_status"
	[ "$pre_status" = 0 ] || {
		echo not-run >"$out_abs/collector-exit-status.txt"
		echo complete >"$out_abs/telemetry-status.txt"
		stop_telemetry
		record_event "$out_abs" "telemetry-stop status=complete reason=pre-canary-command-failed"
		write_verdict "$out_abs" || true
		die "pre-canary command failed"
	}
	local pre_median lower upper
	pre_median="$(recorded_process_median "$out_abs/pre-canary-process-medians-ns.txt" \
		"$canary_processes" "$out_abs/pre-canary-process-medians-sorted-ns.txt")"
	lower="$(band_value lower_ns "$band")"
	upper="$(band_value upper_ns "$band")"
	if ! inside_band "$pre_median" "$lower" "$upper"; then
		record_event "$out_abs" "pre-canary-rejected median=$pre_median band=$lower..$upper"
		echo not-run >"$out_abs/collector-exit-status.txt"
		echo complete >"$out_abs/telemetry-status.txt"
		stop_telemetry
		record_event "$out_abs" "telemetry-stop status=complete reason=pre-canary-rejected"
		write_verdict "$out_abs" || true
		die "pre-canary median $pre_median is outside predeclared band $lower..$upper; collector not started"
	fi
	record_event "$out_abs" "pre-canary-accepted median=$pre_median band=$lower..$upper"

	{
		utc_now
		uptime 2>/dev/null || true
	} >"$out_abs/uptime-before-leg.txt"
	record_event "$out_abs" "collector-start"
	local collector_start collector_end collector_status
	collector_start="$(date +%s.%N)"
	set +e
	run_on_cpus "$cpus" bash "${BRACKET_RUN_ALL:-$HERE/run-all.sh}" "$out_abs" \
		2>&1 | tee "$out_abs/run.log"
	collector_status="${PIPESTATUS[0]}"
	set -e
	collector_end="$(date +%s.%N)"
	echo "$collector_status" >"$out_abs/collector-exit-status.txt"
	awk -v start="$collector_start" -v end="$collector_end" \
		'BEGIN { printf "real %.2f\n", end - start }' >"$out_abs/wall-seconds.txt"
	record_event "$out_abs" "collector-end status=$collector_status"
	{
		utc_now
		uptime 2>/dev/null || true
	} >"$out_abs/uptime-after-leg.txt"

	# The post-canary runs even after a collector failure. It decides whether a
	# partial's completed columns were bracketed, and it remains inside telemetry.
	record_event "$out_abs" "post-canary-start"
	local post_status=0
	run_mapset_group "$cpus" "$canary_processes" "$samples" \
		"$out_abs/post-canary.txt" "$out_abs/post-canary-process-medians-ns.txt" || post_status=$?
	record_event "$out_abs" "post-canary-end status=$post_status"

	local sampler_status=complete
	kill -0 "$MPSTAT_PID" 2>/dev/null || sampler_status=failed
	kill -0 "$PRESSURE_PID" 2>/dev/null || sampler_status=failed
	[ "$post_status" = 0 ] || sampler_status=failed
	[ "$(sha256sum "$band" | awk '{print $1}')" = "$band_hash" ] || sampler_status=failed
	echo "$sampler_status" >"$out_abs/telemetry-status.txt"
	stop_telemetry
	record_event "$out_abs" "telemetry-stop status=$sampler_status"
	{
		utc_now
		uptime 2>/dev/null || true
	} >"$out_abs/uptime-after-bracket.txt"
	trap - EXIT INT TERM HUP

	{
		echo "bracket_band: $BAND_NAME"
		echo "bracket_band_sha256: $band_hash"
		echo "bracket_acceptance: $ACCEPTANCE_RULE (declared before collection)"
		echo "bracket_band_method: $BAND_METHOD (99% predictive interval for a median of fresh-process medians)"
		echo "bracket_canary_unit: median of $canary_processes fresh-process medians, $samples samples each"
		echo "bracket_endpoint_drift: abs(post-pre) <= $(band_value endpoint_drift_halfwidth_ns "$band") ns"
		echo "bracket_cpus: $cpus"
		echo "bracket_telemetry: before pre-canary through after post-canary"
	} >>"$out_abs/PROVENANCE"

	write_verdict "$out_abs"
}

evaluate() {
	[ "$#" = 1 ] || usage
	local out="$1"
	[ -s "$out/$BAND_NAME" ] || die "$out/$BAND_NAME is missing"
	write_verdict "$(cd "$out" && pwd)"
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	[ "$#" -ge 1 ] || usage
	command="$1"
	shift
	case "$command" in
	calibrate) calibrate "$@" ;;
	collect) collect "$@" ;;
	evaluate) evaluate "$@" ;;
	*) usage ;;
	esac
fi
