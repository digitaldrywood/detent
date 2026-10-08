set -euo pipefail
source scripts/check-evidence.sh

base=${1:?base ref required}
app=${2:?conversation directory required}
procs=${3:?process budget required}
if ! [[ "$procs" =~ ^[1-9][0-9]*$ ]]; then
    printf 'Process budget must be a positive integer.\n' >&2
    exit 1
fi
started=$SECONDS
trap 'result=$?; printf "check-land finished: exit=%s wall=%ss\n" "$result" "$((SECONDS - started))"' EXIT

base=$(git merge-base "$base" HEAD)
app_changed=false
if ! git diff --quiet "$base" -- "$app" || [ -n "$(git ls-files --others --exclude-standard -- "$app")" ]; then
    app_changed=true
fi

if [ "$app_changed" = true ]; then
    check_with_evidence app make check-app
fi

check_with_evidence generated make check-generated

stages=(lint vet build unit-short)
workers=$procs
if [ "$workers" -gt "${#stages[@]}" ]; then workers=${#stages[@]}; fi
result=0
for ((offset = 0; offset < ${#stages[@]}; offset += workers)); do
    pids=()
    for ((slot = 0; slot < workers && offset + slot < ${#stages[@]}; slot++)); do
        budget=$((procs / workers))
        if [ "$slot" -lt "$((procs % workers))" ]; then budget=$((budget + 1)); fi
        (
            export GOMAXPROCS=$budget
            case "${stages[offset + slot]}" in
                lint) check_with_evidence lint make lint TEST_PROCS="$budget" ;;
                vet) check_with_evidence vet make vet TEST_PROCS="$budget" ;;
                build) check_with_evidence build go build -p "$budget" ./... ;;
                unit-short) check_with_evidence unit-short env -u DETENT_API_TOKEN go test -short -count=1 -p "$budget" -timeout=10m ./... ;;
            esac
        ) &
        pids+=("$!")
    done
    for pid in "${pids[@]}"; do
        if wait "$pid"; then :; else result=$?; fi
    done
    if [ "$result" -ne 0 ]; then exit "$result"; fi
done

changed=$({ git diff --name-only "$base" || true; git ls-files --others --exclude-standard || true; } | sort -u)
changed_go_packages=()
while IFS= read -r dir; do
    [ -d "$dir" ] || continue
    if package=$(go list "./$dir" 2>/dev/null); then changed_go_packages+=("$package"); fi
done < <(printf '%s\n' "$changed" | grep '\.go$' | xargs -r -n1 dirname | sort -u)
if [ "${#changed_go_packages[@]}" -gt 0 ]; then
    check_with_evidence nilaway python3 scripts/nilaway-changed.py "${NILAWAY_VERSION:?NILAWAY_VERSION required}" "${NILAWAY_INCLUDE_PKGS:?NILAWAY_INCLUDE_PKGS required}" "${changed_go_packages[@]}"
fi
script_tests=()
while IFS= read -r path; do
    case "$path" in
        Makefile) test=scripts/check_land_test.py ;;
        scripts/*_test.py) test=$path ;;
        scripts/*) name=${path#scripts/}; name=${name%.*}; test=scripts/${name//-/_}_test.py ;;
        *) continue ;;
    esac
    if [ -f "$test" ] && [[ " ${script_tests[*]:-} " != *" $test "* ]]; then script_tests+=("$test"); fi
done <<< "$changed"
if [ "${#script_tests[@]}" -gt 0 ]; then
    check_with_evidence scripts python3 -m unittest "${script_tests[@]}"
fi

check_with_evidence invariants env -u DETENT_API_TOKEN go test -count=1 -p "$procs" -timeout=60s ./internal/invariants
check_with_evidence migrations make check-migrations
if [ "$app_changed" = false ]; then
    printf 'Conversation sources unchanged; skipping check-app.\n'
fi
printf 'Landing checks passed.\n'
