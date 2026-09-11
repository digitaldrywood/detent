#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
    0) exec make test-race-hub ;;
    1) exec make test-race-orchestrator ;;
    2|3) ;;
    *) echo 'usage: ci-race-shard.sh {0|1|2|3}' >&2; exit 2 ;;
esac

mkdir -p tmp
# Materialize go list first so discovery failures cannot silently omit tests.
go list ./... > "tmp/race-packages-$1.txt"
awk -v shard="$1" -f scripts/ci-race-packages.awk "tmp/race-packages-$1.txt" > "tmp/race-shard-$1.txt"
packages=()
while IFS= read -r package; do
    packages+=("$package")
done < "tmp/race-shard-$1.txt"
if [ "${#packages[@]}" -eq 0 ]; then
    echo "Race shard $1 has no packages" >&2
    exit 1
fi
mkdir -p "tmp/shard-$1-race-evidence"
env -u DETENT_API_TOKEN go test -race -json "${packages[@]}" | tee "tmp/shard-$1-race-evidence/tests.jsonl"
