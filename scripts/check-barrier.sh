#!/usr/bin/env bash
set -euo pipefail
source scripts/check-evidence.sh

procs=${1:?process budget required}
started=$SECONDS
trap 'result=$?; printf "check-barrier finished: exit=%s wall=%ss\n" "$result" "$((SECONDS - started))"' EXIT

failed=${DETENT_BARRIER_FAILED:-}
go_items=$(printf '%s\n' "$failed" | python3 scripts/barrier_failures.py select go)
browser_items=$(printf '%s\n' "$failed" | python3 scripts/barrier_failures.py select browser)
run_go=1
run_browser=1
if [ -n "$failed" ]; then
    [ -n "$go_items" ] || run_go=0
    [ -n "$browser_items" ] || run_browser=0
fi

rerun_go() {
    local item package tests status=0
    while IFS= read -r item; do
        [ -n "$item" ] || continue
        package=${item%%:*}
        tests=
        [ "$item" = "$package" ] || tests=${item#*:}
        if [ -n "$tests" ]; then
            env -u DETENT_API_TOKEN go test -count=1 -timeout=30m -run "^(${tests})\$" "$package" || status=$?
        else
            env -u DETENT_API_TOKEN go test -count=1 -timeout=30m "$package" || status=$?
        fi
    done <<<"$go_items"
    return "$status"
}

run_browser_specs() {
    local specs=()
    if [ -n "$browser_items" ]; then
        IFS=$'\n' read -r -d '' -a specs <<<"$browser_items" || true
    fi
    node_modules/.bin/playwright test "${specs[@]}"
}

make assets generate-docs
mkdir -p tmp
if [ "$run_browser" = 1 ]; then
    [ -x node_modules/.bin/playwright ] || npm ci
    node_modules/.bin/playwright install chromium
    make build
    go test -c -o tmp/hubserver-preview.test ./internal/hubserver
    go test -c -o tmp/startup-preview.test ./internal/cli
fi

go_pid=
browser_pid=
if [ "$run_go" = 1 ]; then
    if [ -n "$failed" ]; then
        (set -o pipefail; check_with_evidence barrier-go rerun_go 2>&1 | tee tmp/barrier-go.log) &
    else
        (set -o pipefail; check_with_evidence barrier-go make -o generate-docs test TEST_PROCS="$procs" TEST_TIMEOUT=30m 2>&1 | tee tmp/barrier-go.log) &
    fi
    go_pid=$!
fi
if [ "$run_browser" = 1 ]; then
    (set -o pipefail; DETENT_BINARY="$PWD/tmp/detent" DETENT_HOSTED_PREVIEW_BINARY="$PWD/tmp/hubserver-preview.test" DETENT_STARTUP_PREVIEW_BINARY="$PWD/tmp/startup-preview.test" check_with_evidence barrier-browser run_browser_specs 2>&1 | tee tmp/barrier-browser.log) &
    browser_pid=$!
fi
result=0
go_result=0
browser_result=0
if [ -n "$go_pid" ]; then wait "$go_pid" || go_result=$?; fi
if [ -n "$browser_pid" ]; then wait "$browser_pid" || browser_result=$?; fi
if [ "$go_result" != 0 ]; then
    result=$go_result
    printf 'detent-barrier-failed: go %s\n' "$(python3 scripts/barrier_failures.py extract go tmp/barrier-go.log | tr '\n' ' ')"
fi
if [ "$browser_result" != 0 ]; then
    result=$browser_result
    printf 'detent-barrier-failed: browser %s\n' "$(python3 scripts/barrier_failures.py extract browser tmp/barrier-browser.log | tr '\n' ' ')"
fi
exit "$result"
