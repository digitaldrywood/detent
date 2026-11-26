set -euo pipefail
GOTOOLCHAIN=$(awk '$1 == "toolchain" {print $2}' go.mod)
export GOTOOLCHAIN=${GOTOOLCHAIN:-auto}

procs=${1:?process budget required}
if ! [[ "$procs" =~ ^[1-9][0-9]*$ ]]; then
    printf 'Process budget must be a positive integer.\n' >&2
    exit 1
fi
if [ "$#" -gt 1 ]; then
    case "$2" in
        app) make check-app TEST_PROCS="$procs" ;;
        generated) make check-generated TEST_PROCS="$procs" ;;
        migrations) make check-migrations ;;
        build) go build -p "$procs" ./... ;;
        windows-build-amd64) env GOOS=windows GOARCH=amd64 go build -p "$procs" ./... ;;
        windows-vet) env GOOS=windows GOARCH=amd64 go vet -p "$procs" ./... ;;
        windows-build-arm64) env GOOS=windows GOARCH=arm64 go build -p "$procs" ./... ;;
        vet) make vet TEST_PROCS="$procs" ;;
        lint) make lint TEST_PROCS="$procs" ;;
        race) make test-race TEST_PROCS="$procs" ;;
        subprocess-lifecycle) env -u DETENT_API_TOKEN go test -race ./internal/claudecode ./internal/codex -run '^(TestRunTurnCancellationKillsChildProcessGroup|TestRunTurnDetectsExitedParentWithInheritedStdout|TestLocalTransportCloseKillsChildProcessGroup|TestLocalTransportReapsChildAfterParentExits)$' -count=20 -timeout=5m ;;
        full) make test TEST_PROCS="$procs" TEST_TIMEOUT=60m ;;
        security) make security ;;
        browser)
            specs=()
            if selection=$(python3 scripts/check-release.py browser-specs) && [ -n "$selection" ]; then
                while IFS= read -r spec; do specs+=("$spec"); done <<< "$selection"
            else
                specs=(tests/visual)
            fi
            bash scripts/check-browser.sh "$procs" true "${specs[@]}"
            ;;
        *) printf 'Unknown release step: %s\n' "$2" >&2; exit 1 ;;
    esac
    exit
fi

started=$SECONDS
trap 'result=$?; printf "check-release finished: exit=%s wall=%ss\n" "$result" "$((SECONDS - started))"' EXIT
python3 scripts/check-release.py "$procs"
