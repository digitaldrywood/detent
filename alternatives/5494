#!/usr/bin/env bash
set -euo pipefail

release="${1:?usage: release-pick.sh <release/vX.Y> <develop-sha>...}"
shift
(($#)) || { echo 'usage: release-pick.sh <release/vX.Y> <develop-sha>...' >&2; exit 1; }
[[ "$release" =~ ^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo 'Target must be release/vX.Y' >&2; exit 1; }
test -z "$(git status --porcelain)" || { echo 'Cherry-pick requires a clean worktree' >&2; exit 1; }
git fetch origin develop "${release}:refs/remotes/origin/${release}"
base="$(git rev-parse "origin/$release")"
picks=()
for sha in "$@"; do
  sha="$(git rev-parse --verify "$sha^{commit}")"
  git merge-base --is-ancestor "$sha" origin/develop || { echo "Commit $sha must land on develop before release picking" >&2; exit 1; }
  picks+=("$sha")
done
git switch --detach "$base"
for sha in "${picks[@]}"; do
  if ! git cherry-pick -x "$sha"; then
    echo "Resolve the cherry-pick of $sha, run git cherry-pick --continue, pick the remaining commits with git cherry-pick -x, validate, and push with git push origin HEAD:refs/heads/${release}." >&2
    exit 1
  fi
done
git push --force-with-lease="${release}:${base}" origin "HEAD:refs/heads/${release}"
echo "Pushed $(git rev-parse HEAD) to $release. Validate it: scripts/release-validate-local.sh $release"
