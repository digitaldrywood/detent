set -euo pipefail
GOTOOLCHAIN=$(awk '$1 == "toolchain" {print $2}' go.mod)
export GOTOOLCHAIN=${GOTOOLCHAIN:-auto}
source scripts/check-evidence.sh

procs=${1:?process budget required}
if ! [[ "$procs" =~ ^[1-9][0-9]*$ ]]; then
    printf 'Process budget must be a positive integer.\n' >&2
    exit 1
fi
if [ "$#" -gt 1 ]; then
    case "$2" in
        app) check_with_evidence app make check-app TEST_PROCS="$procs" ;;
        generated) check_with_evidence generated make check-generated TEST_PROCS="$procs" ;;
        migrations) check_with_evidence migrations make check-migrations ;;
        invariants) check_with_evidence invariants make check-invariants ;;
        build) check_with_evidence build go build -p "$procs" ./... ;;
        windows-build-amd64) check_with_evidence windows-build-amd64 env GOOS=windows GOARCH=amd64 go build -p "$procs" ./... ;;
        windows-vet) check_with_evidence windows-vet env GOOS=windows GOARCH=amd64 go vet -p "$procs" ./... ;;
        windows-build-arm64) check_with_evidence windows-build-arm64 env GOOS=windows GOARCH=arm64 go build -p "$procs" ./... ;;
        vet) check_with_evidence vet make vet TEST_PROCS="$procs" ;;
        lint) check_with_evidence lint make lint TEST_PROCS="$procs" ;;
        scripts) check_with_evidence scripts python3 -m unittest scripts/nilaway_changed_test.py scripts/check_land_test.py ;;
        nilaway-changed) check_with_evidence nilaway-changed make nilaway-changed ;;
        race) check_with_evidence race make test-race TEST_PROCS="$procs" ;;
        subprocess-lifecycle) check_with_evidence subprocess-lifecycle env -u DETENT_API_TOKEN go test -race ./internal/claudecode ./internal/codex -run '^(TestRunTurnCancellationKillsChildProcessGroup|TestRunTurnDetectsExitedParentWithInheritedStdout|TestLocalTransportCloseKillsChildProcessGroup|TestLocalTransportReapsChildAfterParentExits)$' -count=20 -timeout=5m ;;
        full) check_with_evidence full make test TEST_PROCS="$procs" TEST_TIMEOUT=60m ;;
        security) check_with_evidence security make security ;;
        browser)
            specs=()
            if selection=$(python3 scripts/check-release.py browser-specs) && [ -n "$selection" ]; then
                while IFS= read -r spec; do specs+=("$spec"); done <<< "$selection"
            else
                specs=(tests/visual)
            fi
            check_with_evidence browser bash scripts/check-browser.sh "$procs" true "${specs[@]}"
            ;;
        *) printf 'Unknown release step: %s\n' "$2" >&2; exit 1 ;;
    esac
    exit
fi

started=$SECONDS
trap 'result=$?; printf "check-release finished: exit=%s wall=%ss\n" "$result" "$((SECONDS - started))"' EXIT
python3 scripts/check-release.py "$procs"
