#!/usr/bin/env bash
#
# Run envtest packages concurrently while keeping the success surface compact.
# Full package logs are printed only for failures; green runs show package
# status plus slow tests so the next speed target is obvious.

set -euo pipefail

race=0
coverprofile=""
timeout="10m"

usage() {
    cat >&2 <<'EOF'
usage: scripts/run-envtest-packages.sh [--race] [--coverprofile FILE] [--timeout DURATION] -- PKG...

Environment:
  ENVTEST_JOBS                 max concurrent packages (default: number of packages)
  ENVTEST_SLOW_TEST_THRESHOLD  minimum seconds to show in slow-test summary (default: 2)
  ENVTEST_SLOW_TEST_LIMIT      max slow tests per package (default: 8)
EOF
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --race)
            race=1
            shift
            ;;
        --coverprofile)
            coverprofile="${2:-}"
            if [[ -z "${coverprofile}" ]]; then
                usage
                exit 2
            fi
            shift 2
            ;;
        --timeout)
            timeout="${2:-}"
            if [[ -z "${timeout}" ]]; then
                usage
                exit 2
            fi
            shift 2
            ;;
        --)
            shift
            break
            ;;
        -*)
            usage
            exit 2
            ;;
        *)
            break
            ;;
    esac
done

if [[ $# -eq 0 ]]; then
    usage
    exit 2
fi

mapfile -t packages < <(go list "$@")
if [[ ${#packages[@]} -eq 0 ]]; then
    echo "envtest: no packages matched" >&2
    exit 1
fi

jobs="${ENVTEST_JOBS:-${#packages[@]}}"
if ! [[ "${jobs}" =~ ^[0-9]+$ ]] || [[ "${jobs}" -lt 1 ]]; then
    echo "envtest: ENVTEST_JOBS must be a positive integer" >&2
    exit 2
fi
if [[ "${jobs}" -gt "${#packages[@]}" ]]; then
    jobs="${#packages[@]}"
fi

slow_threshold="${ENVTEST_SLOW_TEST_THRESHOLD:-2}"
slow_limit="${ENVTEST_SLOW_TEST_LIMIT:-8}"

workdir="$(mktemp -d)"
cleanup() {
    rm -rf "${workdir}"
}
trap cleanup EXIT

declare -A pid_pkg=()
declare -A pid_log=()
declare -A pid_cov=()
declare -A pid_start=()
declare -a pids=()

next=0
failed=0

start_one() {
    local pkg="$1"
    local safe_pkg log cov
    safe_pkg="$(printf '%s' "${pkg}" | tr '/:.' '___')"
    log="${workdir}/${safe_pkg}.log"
    cov="${workdir}/${safe_pkg}.cov"

    local cmd=(go test -v -count=1 -timeout "${timeout}")
    if [[ "${race}" -eq 1 ]]; then
        cmd+=(-race)
    fi
    if [[ -n "${coverprofile}" ]]; then
        cmd+=("-coverprofile=${cov}")
    fi
    cmd+=("${pkg}")

    printf '==> %s\n' "${pkg}"
    "${cmd[@]}" >"${log}" 2>&1 &
    local pid=$!
    pid_pkg["${pid}"]="${pkg}"
    pid_log["${pid}"]="${log}"
    pid_cov["${pid}"]="${cov}"
    pid_start["${pid}"]="$(date +%s)"
    pids+=("${pid}")
}

print_slow_tests() {
    local log="$1"
    awk -v threshold="${slow_threshold}" -v limit="${slow_limit}" '
        /--- PASS:|--- FAIL:/ {
            name=$3
            sec=$NF
            gsub(/[()s]/, "", sec)
            if (sec + 0 >= threshold) {
                rows[++count]=sprintf("    %6.2fs  %s", sec, name)
                times[count]=sec + 0
            }
        }
        END {
            for (i = 1; i <= count; i++) {
                for (j = i + 1; j <= count; j++) {
                    if (times[j] > times[i]) {
                        tmp=times[i]; times[i]=times[j]; times[j]=tmp
                        tmp=rows[i]; rows[i]=rows[j]; rows[j]=tmp
                    }
                }
            }
            shown = count < limit ? count : limit
            for (i = 1; i <= shown; i++) {
                print rows[i]
            }
        }
    ' "${log}"
}

finish_one() {
    local pid="$1"
    local pkg="${pid_pkg[${pid}]}"
    local log="${pid_log[${pid}]}"
    local rc=0
    wait "${pid}" || rc=$?

    local elapsed=$(( $(date +%s) - ${pid_start[${pid}]} ))
    if [[ "${rc}" -eq 0 ]]; then
        local ok_line
        ok_line="$(grep -E '^ok[[:space:]]+' "${log}" | tail -n 1 || true)"
        if [[ -n "${ok_line}" ]]; then
            printf 'PASS %s (%ss)  %s\n' "${pkg}" "${elapsed}" "${ok_line}"
        else
            printf 'PASS %s (%ss)\n' "${pkg}" "${elapsed}"
        fi
        local slow
        slow="$(print_slow_tests "${log}")"
        if [[ -n "${slow}" ]]; then
            printf '  slow tests >= %ss:\n%s\n' "${slow_threshold}" "${slow}"
        fi
    else
        failed=1
        printf 'FAIL %s (%ss)\n' "${pkg}" "${elapsed}" >&2
        # Print the full log on failure. Prior head-240 + tail-160 truncation
        # routinely hid the actual `--- FAIL:` marker in mid-run output,
        # forcing local reproduction to identify which test failed. CI log
        # surfaces tolerate the extra volume; a failing run is rare.
        cat "${log}" >&2
        # Echo a FAIL summary at the end so the failing test name is easy to
        # spot in long logs.
        local fail_summary
        fail_summary="$(grep -E '^--- FAIL:' "${log}" || true)"
        if [[ -n "${fail_summary}" ]]; then
            printf '\n=== %s failed tests ===\n%s\n' "${pkg}" "${fail_summary}" >&2
        fi
    fi

    unset "pid_pkg[${pid}]" "pid_log[${pid}]" "pid_cov[${pid}]" "pid_start[${pid}]"
}

is_running_job() {
    local want="$1"
    local running
    for running in $(jobs -r -p); do
        [[ "${running}" == "${want}" ]] && return 0
    done
    return 1
}

while [[ "${next}" -lt "${#packages[@]}" || "${#pids[@]}" -gt 0 ]]; do
    while [[ "${next}" -lt "${#packages[@]}" && "${#pids[@]}" -lt "${jobs}" ]]; do
        start_one "${packages[${next}]}"
        next=$((next + 1))
    done

    remaining=()
    for pid in "${pids[@]}"; do
        if is_running_job "${pid}"; then
            remaining+=("${pid}")
        else
            finish_one "${pid}"
        fi
    done
    pids=("${remaining[@]}")

    if [[ "${#pids[@]}" -gt 0 ]]; then
        sleep 0.2
    fi
done

if [[ -n "${coverprofile}" ]]; then
    {
        echo "mode: atomic"
        for cov in "${workdir}"/*.cov; do
            [[ -s "${cov}" ]] && tail -n +2 "${cov}"
        done
    } >"${coverprofile}"
fi

exit "${failed}"
