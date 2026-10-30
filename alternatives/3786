#!/usr/bin/env bash
set -euo pipefail

sha="${1:?usage: release-pick.sh <sha> <release/vX.Y>}"
release="${2:?usage: release-pick.sh <sha> <release/vX.Y>}"
[[ "$release" =~ ^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo 'Target must be release/vX.Y' >&2; exit 1; }
test -z "$(git status --porcelain)" || { echo 'Cherry-pick requires a clean worktree' >&2; exit 1; }
git fetch origin develop "${release}:refs/remotes/origin/${release}"
sha="$(git rev-parse --verify "$sha^{commit}")"
git merge-base --is-ancestor "$sha" origin/develop || { echo 'Commit must land on develop before release picking' >&2; exit 1; }
branch="pick/${release#release/}/${sha:0:12}"
git switch --no-track -c "$branch" "origin/$release"
if ! git cherry-pick -x "$sha"; then
  echo 'Resolve the cherry-pick, run git cherry-pick --continue, validate, push, and open the PR to the release branch.' >&2
  exit 1
fi
git push -u origin "$branch"
gh pr create --base "$release" --head "$branch" --title "fix(release): pick ${sha:0:12}" --body "Cherry-pick develop commit $sha onto $release with its original commit trailer. Validate the fix before merging. After the batch of picks merges, validate the release head on a runner host: scripts/release-validate-local.sh $release"
