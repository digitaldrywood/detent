#!/usr/bin/env bash
set -euo pipefail

branch="${1:-}"
if [ "$#" -ne 1 ] || { [ "$branch" != develop ] && [[ ! "$branch" =~ ^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; }; then
  echo "Usage: scripts/release-validate-local.sh develop|release/vX.Y" >&2
  exit 1
fi
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
root="$(cd "$script_dir/.." && git rev-parse --show-toplevel)"
cd "$root"
git fetch origin "+refs/heads/$branch:refs/remotes/origin/$branch"
sha="$(git rev-parse --verify "refs/remotes/origin/$branch^{commit}")"
repository="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
project="${DETENT_VALIDATE_PROJECT:-${repository#*/}}"
config_args=()
if [ -n "${DETENT_VALIDATE_CONFIG:-}" ]; then config_args=(--config "$DETENT_VALIDATE_CONFIG"); fi
procs="${TEST_PROCS:-4}"
export GOCACHE="${DETENT_VALIDATE_GOCACHE:-$HOME/.cache/detent-validate/go-build}"
mkdir -p "$GOCACHE"
context=scheduled-full-ci
if [ "$branch" = develop ]; then context=develop-full-ci; fi
mkdir -p "$root/tmp"
output_dir="$(mktemp -d "$root/tmp/release-validate.XXXXXX")"
worktree="$output_dir/source"
log="$output_dir/validation.log"
cleanup() { git worktree remove --force "$worktree" > /dev/null 2>&1 || true; }
trap cleanup EXIT
git worktree add --detach "$worktree" "$sha"
host="$(hostname -s)"
gh api -X POST "repos/$repository/statuses/$sha" -f context="$context" -f state=pending -f description="Local validation running on $host" > /dev/null
started=$SECONDS
if ! (cd "$worktree" && make app TEST_PROCS="$procs" && make build TEST_PROCS="$procs") > "$log" 2>&1; then
  gh api -X POST "repos/$repository/statuses/$sha" -f context="$context" -f state=failure -f description="Local validation could not build ${sha:0:7} on $host" > /dev/null
  tail -40 "$log" >&2
  echo "Validation build failed; log: $log" >&2
  exit 1
fi
state=success
if ! TMPDIR="$output_dir" "$worktree/tmp/detent" validate ${config_args[@]+"${config_args[@]}"} --project "$project" --dir "$worktree" "make check-release TEST_PROCS=$procs" >> "$log" 2>&1; then
  state=failure
fi
source_changed=false
if [ "$(cd "$worktree" && git rev-parse HEAD)" != "$sha" ] || [ -n "$(cd "$worktree" && git status --porcelain)" ]; then
  source_changed=true
  state=failure
  echo "Validation changed the checkout at $sha; the result does not certify that commit." >> "$log"
fi
minutes=$(((SECONDS - started + 59) / 60))
receipts="$(grep -E '^go test receipts:' "$log" | tail -1 | sed -E 's/^go test receipts: ([0-9]+) of ([0-9]+) test binaries reused.*/\1 of \2 test binaries reused/' || true)"
description="Local validation on $host in ${minutes}m"
if [ "$state" = failure ]; then description="Local validation failed on $host after ${minutes}m"; fi
if [ "$source_changed" = true ]; then description="Local validation changed the checkout on $host"; fi
if [ -n "$receipts" ]; then description="$description; $receipts"; fi
gh api -X POST "repos/$repository/statuses/$sha" -f context="$context" -f state="$state" -f description="${description:0:140}" > /dev/null
echo "Posted $context=$state on $sha. Log: $log"
if [ "$state" = failure ]; then
  grep -E 'detent-check-evidence: .*"exit_code":[1-9]|^--- FAIL|^FAIL[[:space:]]' "$log" | tail -20 >&2 || true
  exit 1
fi
if [ "$branch" != develop ]; then
  gh workflow run release-finalize.yml --repo "$repository" --ref "$branch"
  echo "Dispatched release-finalize.yml on $branch; it tags only when every required status is green on $sha."
fi
