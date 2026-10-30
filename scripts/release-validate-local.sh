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
trap 'cleanup' EXIT
cleanup() { git worktree remove --force "$worktree" > /dev/null 2>&1 || true; }
git worktree add --detach "$worktree" "$sha"
host="$(hostname -s)"
gh api -X POST "repos/$repository/statuses/$sha" -f context="$context" -f state=pending -f description="Local validation running on $host" > /dev/null
fail_setup() {
  gh api -X POST "repos/$repository/statuses/$sha" -f context="$context" -f state=failure -f description="$1" > /dev/null
  tail -40 "$log" >&2 || true
  echo "$1; log: $log" >&2
  exit 1
}
gopath="$(go env GOPATH)" || fail_setup "Local validation could not read the Go environment on $host"
gomodcache="$(go env GOMODCACHE)" || fail_setup "Local validation could not read the Go environment on $host"
npm_cache="${npm_config_cache:-$HOME/.npm}"
browsers="${PLAYWRIGHT_BROWSERS_PATH:-$HOME/Library/Caches/ms-playwright}"
[ -n "${DETENT_VALIDATE_CONFIG:-}" ] || fail_setup "Local validation needs DETENT_VALIDATE_CONFIG on $host"
validator_dir="$(mktemp -d "$root/tmp/release-validator.XXXXXX")"
chmod 700 "$validator_dir"
validator="$validator_dir/detent"
cleanup() {
  git worktree remove --force "$worktree" > /dev/null 2>&1 || true
  rm -rf "$validator_dir"
}
installed="${DETENT_VALIDATOR:-$(command -v detent || true)}"
[ -n "$installed" ] && [ -x "$installed" ] || fail_setup "Local validation found no installed detent validator on $host"
cp "$installed" "$validator"
chmod 700 "$validator"
validator_digest="$(shasum -a 256 "$validator" | cut -d' ' -f1)"
build_home="$output_dir/home"
build_tmp="$output_dir/tmp"
mkdir -p "$build_home" "$build_tmp"
validator_home="$validator_dir/home"
mkdir -p "$validator_home"
toolchain_env=(PATH="$PATH" LANG="${LANG:-en_US.UTF-8}" TMPDIR="$build_tmp" GOCACHE="$GOCACHE" GOPATH="$gopath" GOMODCACHE="$gomodcache" GOTOOLCHAIN="${GOTOOLCHAIN:-auto}" npm_config_cache="$npm_cache" PLAYWRIGHT_BROWSERS_PATH="$browsers" TEST_PROCS="$procs")
build_env=(env -i HOME="$build_home" "${toolchain_env[@]}")
validator_env=(env -i HOME="$validator_home" "${toolchain_env[@]}")
started=$SECONDS
if ! (cd "$worktree" && "${build_env[@]}" make app TEST_PROCS="$procs" && "${build_env[@]}" make build TEST_PROCS="$procs") >> "$log" 2>&1; then
  fail_setup "Local validation could not build ${sha:0:7} on $host"
fi
if [ "$(shasum -a 256 "$validator" | cut -d' ' -f1)" != "$validator_digest" ]; then
  fail_setup "Local validation found the validator changed during the build on $host"
fi
state=success
if ! "${validator_env[@]}" "$validator" validate ${config_args[@]+"${config_args[@]}"} --project "$project" --dir "$worktree" "make check-release TEST_PROCS=$procs" >> "$log" 2>&1; then
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
