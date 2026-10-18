set -euo pipefail
procs=${1:?process budget required}
app_prepared=${2:?app preparation state required}
shift 2
if [ "$#" -eq 0 ]; then
    printf 'UI sources unchanged; skipping browser specs.\n'
    exit 0
fi
started=$SECONDS
trap 'result=$?; printf "Affected browser specs finished: exit=%s wall=%ss\n" "$result" "$((SECONDS - started))"' EXIT
printf 'Affected browser specs:\n'
printf '  %s\n' "$@"
if [ "$app_prepared" = true ]; then
    make -o app build TEST_PROCS="$procs"
else
    make build TEST_PROCS="$procs"
fi
go test -p "$procs" -c -o tmp/hubserver-preview.test ./internal/hubserver
go test -p "$procs" -c -o tmp/startup-preview.test ./internal/cli
go test -p "$procs" -c -o tmp/entry-preview.test ./internal/cloudentry
node_modules/.bin/playwright install chromium
DETENT_BINARY="$PWD/tmp/detent" \
DETENT_HOSTED_PREVIEW_BINARY="$PWD/tmp/hubserver-preview.test" \
DETENT_STARTUP_PREVIEW_BINARY="$PWD/tmp/startup-preview.test" \
DETENT_ENTRY_PREVIEW_BINARY="$PWD/tmp/entry-preview.test" \
node_modules/.bin/playwright test --workers="$procs" --retries=0 "$@"
