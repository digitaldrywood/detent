#!/usr/bin/env bash
set -euo pipefail
GOTOOLCHAIN=$(awk '$1 == "toolchain" {print $2}' go.mod)
export GOTOOLCHAIN=${GOTOOLCHAIN:-auto}
source scripts/check-evidence.sh

procs=${1:?process budget required}
started=$SECONDS
trap 'result=$?; printf "check-barrier finished: exit=%s wall=%ss\n" "$result" "$((SECONDS - started))"' EXIT

checks=(lint vet generated migrations app security nilaway checkland)
check_command() {
    case $1 in
        lint) make lint ;;
        vet) make vet ;;
        invariants) make check-invariants ;;
        generated) make check-generated ;;
        migrations) make check-migrations ;;
        app) make check-app ;;
        security) make security ;;
        nilaway) make nilaway-changed ;;
        checkland) python3 -m unittest scripts/check_land_test.py scripts/barrier_failures_test.py ;;
        *) return 2 ;;
    esac
}

failed=${DETENT_BARRIER_FAILED:-}
select_scope() { printf '%s\n' "$failed" | python3 scripts/barrier_failures.py select "$1"; }
go_items=$(select_scope go)
race_items=$(select_scope race)
browser_items=$(select_scope browser)
check_items=$(select_scope check)
run_go=1
run_race=1
run_browser=1
run_checks=("${checks[@]}")
if [ -n "$failed" ]; then
    [ -n "$go_items" ] || run_go=0
    [ -n "$race_items" ] || run_race=0
    [ -n "$browser_items" ] || run_browser=0
    run_checks=()
    if [ -n "$check_items" ]; then
        IFS=$'\n' read -r -d '' -a run_checks <<<"$check_items" || true
    fi
fi

rerun_tests() {
    local items=$1 race=$2 item package tests status=0
    while IFS= read -r item; do
        [ -n "$item" ] || continue
        package=${item%%:*}
        tests=
        [ "$item" = "$package" ] || tests=${item#*:}
        local args=(-count=1 -timeout=30m)
        [ "$race" = 1 ] && args+=(-race -short)
        [ -n "$tests" ] && args+=(-run "^(${tests})\$")
        env -u DETENT_API_TOKEN go test "${args[@]}" "$package" || status=$?
    done <<<"$items"
    return "$status"
}

run_go_tests() {
    if [ -n "$failed" ]; then
        rerun_tests "$go_items" 0
    else
        local packages workspace status=0
        WORKSPACE_TEST_SHARDS=4 bash scripts/test-workspace.sh &
        workspace=$!
        packages=$(go list ./... | grep -v -e '/internal/workspace$' -e '/internal/web$')
        env -u DETENT_API_TOKEN go test -count=1 -p "$procs" -timeout=30m $packages || status=$?
        wait "$workspace" || status=1
        return "$status"
    fi
}

run_race_tests() {
    if [ -n "$failed" ]; then rerun_tests "$race_items" 1; else make -o generate-docs test-race TEST_PROCS="$procs"; fi
}

run_browser_specs() {
    local specs=()
    if [ -n "$browser_items" ]; then
        IFS=$'\n' read -r -d '' -a specs <<<"$browser_items" || true
    fi
    node_modules/.bin/playwright test "${specs[@]}"
}

run_checks_in_order() {
    local name pid status=0 pids=() names=()
    : > tmp/barrier-checks.failed
    for name in "$@"; do
        check_with_evidence "barrier-$name" check_command "$name" > "tmp/barrier-check-$name.log" 2>&1 &
        pids+=("$!")
        names+=("$name")
    done
    for i in "${!pids[@]}"; do
        if ! wait "${pids[$i]}"; then
            echo "${names[$i]}" >> tmp/barrier-checks.failed
            status=1
        fi
        cat "tmp/barrier-check-${names[$i]}.log"
    done
    return "$status"
}

make assets generate-docs
mkdir -p tmp
if [ -f web/conversation/package-lock.json ] && [ ! -d web/conversation/node_modules ]; then (cd web/conversation && npm ci); fi
if [ "$run_browser" = 1 ]; then
    [ -x node_modules/.bin/playwright ] || npm ci
    node_modules/.bin/playwright install chromium
    make build
    go test -c -o tmp/hubserver-preview.test ./internal/hubserver
    go test -c -o tmp/startup-preview.test ./internal/cli
fi

go_pid=
race_pid=
browser_pid=
checks_pid=
if [ "$run_go" = 1 ]; then
    (set -o pipefail; check_with_evidence barrier-go run_go_tests 2>&1 | tee tmp/barrier-go.log) &
    go_pid=$!
fi
if [ "$run_race" = 1 ]; then
    (set -o pipefail; check_with_evidence barrier-race run_race_tests 2>&1 | tee tmp/barrier-race.log) &
    race_pid=$!
fi
if [ "$run_browser" = 1 ]; then
    (set -o pipefail; DETENT_BINARY="$PWD/tmp/detent" DETENT_HOSTED_PREVIEW_BINARY="$PWD/tmp/hubserver-preview.test" DETENT_STARTUP_PREVIEW_BINARY="$PWD/tmp/startup-preview.test" check_with_evidence barrier-browser run_browser_specs 2>&1 | tee tmp/barrier-browser.log) &
    browser_pid=$!
fi
if [ "${#run_checks[@]}" -gt 0 ]; then
    (set -o pipefail; run_checks_in_order "${run_checks[@]}" 2>&1 | tee tmp/barrier-checks.log) &
    checks_pid=$!
fi

kill_tree() {
    local child
    for child in $(pgrep -P "$1" 2>/dev/null); do
        kill_tree "$child"
    done
    kill "$1" 2>/dev/null || true
}

result=0
go_result=0
race_result=0
browser_result=0
checks_result=0
stopped=
pending="$go_pid $race_pid $browser_pid $checks_pid"
while [ -n "${pending// /}" ]; do
    remaining=
    for pid in $pending; do
        if kill -0 "$pid" 2>/dev/null; then
            remaining="$remaining $pid"
            continue
        fi
        status=0
        wait "$pid" || status=$?
        case $pid in
            "$go_pid") go_result=$status ;;
            "$race_pid") race_result=$status ;;
            "$browser_pid") browser_result=$status ;;
            "$checks_pid") checks_result=$status ;;
        esac
        if [ "$status" != 0 ] && [ -z "$stopped" ]; then
            stopped=$pid
        fi
    done
    pending=$remaining
    if [ -n "$stopped" ] && [ -n "${pending// /}" ]; then
        printf 'check-barrier: a check group failed; stopping the remaining groups\n'
        for pid in $pending; do kill_tree "$pid"; done
        for pid in $pending; do wait "$pid" 2>/dev/null || true; done
        pending=
    fi
    [ -z "${pending// /}" ] || sleep 2
done
if [ -z "$stopped" ] && [ "$run_go" = 1 ] && [ -z "$failed" ]; then
    if ! (set -o pipefail; check_with_evidence barrier-web env -u DETENT_API_TOKEN go test -count=1 ./internal/web 2>&1 | tee -a tmp/barrier-go.log); then
        go_result=1
        stopped=$go_pid
    fi
fi
if [ "$go_result" != 0 ] && [ "$stopped" = "$go_pid" ]; then
    result=$go_result
    printf 'detent-barrier-failed: go %s\n' "$(python3 scripts/barrier_failures.py extract go tmp/barrier-go.log | tr '\n' ' ')"
fi
if [ "$race_result" != 0 ] && [ "$stopped" = "$race_pid" ]; then
    result=$race_result
    printf 'detent-barrier-failed: race %s\n' "$(python3 scripts/barrier_failures.py extract go tmp/barrier-race.log | tr '\n' ' ')"
fi
if [ "$browser_result" != 0 ] && [ "$stopped" = "$browser_pid" ]; then
    result=$browser_result
    printf 'detent-barrier-failed: browser %s\n' "$(python3 scripts/barrier_failures.py extract browser tmp/barrier-browser.log | tr '\n' ' ')"
fi
if [ "$checks_result" != 0 ] && [ "$stopped" = "$checks_pid" ]; then
    result=$checks_result
    printf 'detent-barrier-failed: check %s\n' "$(tr '\n' ' ' < tmp/barrier-checks.failed)"
fi
exit "$result"
