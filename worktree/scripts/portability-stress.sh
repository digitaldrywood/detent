#!/usr/bin/env bash
set -euo pipefail

# Keep subprocess-heavy repetitions and persisted fixture suites on separate
# hosted runners. Preserve ten repetitions plus the complete race selection.
suite=${1:?usage: portability-stress.sh SUITE}
scratch=${TMPDIR:-${TMP:-${TEMP:?a worker temporary directory is required}}}
evidence="$scratch/portability-stress-evidence"
mkdir -p "$evidence"
export GOMAXPROCS=${GOMAXPROCS:-4}
go version | tee "$evidence/toolchain.txt"
go env GOOS GOARCH >> "$evidence/toolchain.txt"
printf 'GOMAXPROCS=%s\nsuite=%s\n' "$GOMAXPROCS" "$suite" >> "$evidence/toolchain.txt"

case "$suite" in
    stress-cli|stress-runner|stress-checklock)
        case "$suite" in
            stress-cli) package=./internal/cli; budget=120m ;;
            stress-runner) package=./internal/runner; budget=45m ;;
            stress-checklock) package=./tools/checklock; budget=45m ;;
        esac
        env -u DETENT_API_TOKEN go test -race -json -p 4 -parallel 4 \
            -count=10 -timeout="$budget" "$package" | tee "$evidence/tests.jsonl"
        ;;
    hub-a|hub-b|hub-c)
        # These are the exhaustive, disjoint partitions used by Makefile.
        selection=()
        case "$suite" in
            hub-a) selection=(-run '^Test[A-GI-O]') ;;
            hub-b) selection=(-run '^Test[HW]') ;;
            hub-c) selection=(-skip '^Test[A-GI-O]|^Test[HW]') ;;
        esac
        env -u DETENT_API_TOKEN go run -p 4 ./tools/testgate -race \
            -parallel 2 -timeout 45m -output "$evidence" \
            "${selection[@]}" ./internal/hubserver
        ;;
    orchestrator)
        env -u DETENT_API_TOKEN go run -p 4 ./tools/testgate -race \
            -parallel 4 -timeout 30m -output "$evidence" ./internal/orchestrator
        ;;
    workspace)
        bash scripts/test-workspace.sh -race -parallel 4 -output "$evidence"
        ;;
    rest)
        # Materialize discovery first so a go list failure cannot omit coverage.
        go list -p 4 ./... > "$evidence/packages.txt"
        packages=()
        while IFS= read -r package; do
            case "$package" in
                github.com/digitaldrywood/detent/internal/hubserver|\
                github.com/digitaldrywood/detent/internal/orchestrator|\
                github.com/digitaldrywood/detent/internal/workspace) ;;
                *) packages+=("$package") ;;
            esac
        done < "$evidence/packages.txt"
        if [ "${#packages[@]}" -eq 0 ]; then
            echo 'No remaining race packages discovered' >&2
            exit 1
        fi
        env -u DETENT_API_TOKEN go run -p 4 ./tools/testgate -race \
            -parallel 4 -timeout 30m -output "$evidence" "${packages[@]}"
        ;;
    *) echo "Unknown portability stress suite: $suite" >&2; exit 2 ;;
esac
