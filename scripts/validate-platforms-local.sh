#!/usr/bin/env bash
set -euo pipefail

branch="${1:-}"
if [ "$#" -ne 1 ] || { [ "$branch" != develop ] && [[ ! "$branch" =~ ^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; }; then
  echo "Usage: scripts/validate-platforms-local.sh develop|release/vX.Y" >&2
  exit 1
fi
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
root="$(cd "$script_dir/.." && git rev-parse --show-toplevel)"
cd "$root"
git fetch origin "+refs/heads/$branch:refs/remotes/origin/$branch"
sha="$(git rev-parse --verify "refs/remotes/origin/$branch^{commit}")"
repository="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
procs="${TEST_PROCS:-4}"
toolchain="$(git show "$sha:go.mod" | awk '$1 == "toolchain" {sub(/^go/, "", $2); print $2}')"
image="detent-linux-validate:$toolchain"
cache="${DETENT_PLATFORM_CACHE:-$HOME/.cache/detent-platforms}"
mkdir -p "$root/tmp" "$cache/linux"
output_dir="$(mktemp -d "$root/tmp/platform-validate.XXXXXX")"
worktree="$output_dir/source"
cleanup() { git worktree remove --force "$worktree" > /dev/null 2>&1 || true; }
trap cleanup EXIT
git worktree add --detach "$worktree" "$sha"
host="$(hostname -s)"
gh api -X POST "repos/$repository/statuses/$sha" -f context=platform-full-ci -f state=pending -f description="Linux and Windows validation running on $host" > /dev/null
started=$SECONDS
(cd "$worktree" && make app TEST_PROCS="$procs") > "$output_dir/app.log" 2>&1

linux_context="${DETENT_LINUX_CONTEXT:-colima}"
docker --context "$linux_context" build -q --platform linux/amd64 --build-arg GO_VERSION="$toolchain" --build-arg UID="$(id -u)" \
  -t "$image" -f "$worktree/scripts/linux-validate.Dockerfile" "$worktree/scripts" > /dev/null
linux_source="$output_dir/linux-source"
rsync -a --exclude=.git --exclude=node_modules "$worktree/" "$linux_source/"
linux=failure
if docker --context "$linux_context" run --rm --platform linux/amd64 \
  -v "$linux_source:/src" -v "$cache/linux:/cache" -w /src \
  -e GOCACHE=/cache/go-build -e GOMODCACHE=/cache/mod -e GOTOOLCHAIN=local -e "GOFLAGS=-trimpath -buildvcs=false" \
  "$image" bash -c "set -euo pipefail; git init -q; git add -A; git commit -qm validation; go vet -p $procs ./...; env -u DETENT_API_TOKEN go test -p $procs -short -race -timeout=30m ./..." \
  > "$output_dir/linux.log" 2>&1; then
  linux=success
fi

windows=failure
if [ "${DETENT_PLATFORM_WINDOWS:-}" = unprovisioned ]; then
  windows=unprovisioned
  echo "Windows validation skipped: DETENT_PLATFORM_WINDOWS=unprovisioned" > "$output_dir/windows.log"
elif bash "$worktree/scripts/validate-windows-local.sh" "$worktree" > "$output_dir/windows.log" 2>&1; then
  windows=success
fi

minutes=$(((SECONDS - started + 59) / 60))
state=success
if [ "$linux" != success ] || { [ "$windows" != success ] && [ "$windows" != unprovisioned ]; }; then state=failure; fi
gh api -X POST "repos/$repository/statuses/$sha" -f context=platform-full-ci -f state="$state" -f description="Linux $linux, Windows $windows on $host in ${minutes}m" > /dev/null
echo "Posted platform-full-ci=$state on $sha (linux=$linux windows=$windows). Logs: $output_dir"
[ "$state" = success ]
