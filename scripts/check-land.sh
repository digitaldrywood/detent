set -euo pipefail

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
    make check-app
elif [ ! -f static/app/conversation/app.js ]; then
    make app
fi

make lint
make vet
make nilaway-changed
go build -p "$procs" ./...
make test-fast
make check-invariants
make check-migrations
make check-generated
if [ "$app_changed" = false ]; then
    printf 'Conversation sources unchanged; skipping check-app.\n'
fi
printf 'Landing checks passed.\n'
