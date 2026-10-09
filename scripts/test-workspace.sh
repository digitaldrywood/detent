#!/usr/bin/env bash
set -euo pipefail

# Workspace tests stub the host-wide process scan (lsof / scratch environment
# inventory) unless a test opts into the real scanner, so the package no longer
# needs a serial run or the 30-minute budget recorded in
# .detent/validation/2962/README.md. Share this package-only budget across
# ordinary, race, and coverage gates.
gate() {
    env -u DETENT_API_TOKEN go run ./tools/testgate -timeout 15m "$@" ./internal/workspace
}

shards=${WORKSPACE_TEST_SHARDS:-1}
tests=
if [ "$#" -eq 0 ] && [ "$shards" -gt 1 ]; then
    tests=$(sed -nE 's/^func ((Test|Fuzz)[A-Za-z0-9_]*)\(.*/\1/p' internal/workspace/*_test.go 2>/dev/null | sort -u || true)
fi
if [ -z "$tests" ]; then
    exec env -u DETENT_API_TOKEN go run ./tools/testgate \
        -timeout 15m -output tmp/workspace-test-evidence "$@" ./internal/workspace
fi
pids=()
for ((i = 0; i < shards; i++)); do
    selected=$(printf '%s\n' "$tests" | awk -v n="$shards" -v i="$i" '(NR - 1) % n == i' | paste -sd '|' -)
    [ -n "$selected" ] || continue
    gate -output "tmp/workspace-test-evidence/shard-$i" -run "^(${selected})\$" &
    pids+=("$!")
done
status=0
for pid in "${pids[@]}"; do
    wait "$pid" || status=1
done
exit "$status"
