#!/usr/bin/env bash
set -euo pipefail
source scripts/check-evidence.sh

procs=${1:?process budget required}
started=$SECONDS
trap 'result=$?; printf "check-barrier finished: exit=%s wall=%ss\n" "$result" "$((SECONDS - started))"' EXIT

make assets generate-docs
[ -x node_modules/.bin/playwright ] || npm ci
node_modules/.bin/playwright install chromium
make build
mkdir -p tmp
go test -c -o tmp/hubserver-preview.test ./internal/hubserver
go test -c -o tmp/startup-preview.test ./internal/cli

(check_with_evidence barrier-go make -o generate-docs test TEST_PROCS="$procs" TEST_TIMEOUT=30m) &
go_pid=$!
(DETENT_BINARY="$PWD/tmp/detent" DETENT_HOSTED_PREVIEW_BINARY="$PWD/tmp/hubserver-preview.test" DETENT_STARTUP_PREVIEW_BINARY="$PWD/tmp/startup-preview.test" check_with_evidence barrier-browser node_modules/.bin/playwright test) &
browser_pid=$!
result=0
wait "$go_pid" || result=$?
wait "$browser_pid" || result=$?
exit "$result"
