set -euo pipefail
source scripts/check-evidence.sh

base=${1:?base ref required}
app=${2:?conversation directory required}
procs=${3:?process budget required}
started=$SECONDS
trap 'result=$?; printf "check-land finished: exit=%s wall=%ss\n" "$result" "$((SECONDS - started))"' EXIT

base=$(git merge-base "$base" HEAD)
app_changed=false
if ! git diff --quiet "$base" -- "$app" || [ -n "$(git ls-files --others --exclude-standard -- "$app")" ]; then
    app_changed=true
fi

if [ "$app_changed" = true ]; then
    check_with_evidence app make check-app
elif [ ! -f static/app/conversation/app.js ]; then
    make app
fi

check_with_evidence lint make lint
check_with_evidence vet make vet
check_with_evidence nilaway-changed make nilaway-changed
check_with_evidence build go build -p "$procs" ./...
check_with_evidence unit-short make test-fast
check_with_evidence invariants make check-invariants
check_with_evidence migrations make check-migrations
check_with_evidence generated make check-generated
if [ "$app_changed" = false ]; then
    printf 'Conversation sources unchanged; skipping check-app.\n'
fi
printf 'Landing checks passed.\n'
