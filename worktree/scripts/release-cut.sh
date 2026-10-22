#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
source_sha="${2:-}"
repository="${GITHUB_REPOSITORY:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"
script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
automatic=false
if [ -z "$source_sha" ] || [ "${GITHUB_EVENT_NAME:-}" = schedule ]; then automatic=true; fi
admit_release() {
  local result
  if python3 "$script_dir/release-cut-admission.py" "$repository" "${GITHUB_RUN_ID:-}" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"; then
    return
  else
    result=$?
    if [ "$result" -eq 10 ]; then exit 0; fi
    exit "$result"
  fi
}
if [ "$automatic" = true ]; then admit_release; fi
git fetch origin develop '+refs/heads/release/*:refs/remotes/origin/release/*' --tags

if [ -z "$version" ]; then
  latest="$( { git tag --list 'v*'; git for-each-ref --format='%(refname:strip=4)' refs/remotes/origin/release; } | sed -nE 's/^v([0-9]+)\.([0-9]+)(\.[0-9]+)?$/\1 \2/p' | sort -k1,1nr -k2,2nr | awk 'NR == 1 { print }')"
  if [ -n "$latest" ]; then
    read -r major minor <<< "$latest"
    version="v$major.$((minor + 1))"
  else
    version=v0.1
  fi
fi
[[ "$version" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo 'Release version must be vX.Y' >&2; exit 1; }
branch="release/$version"
if git show-ref --verify --quiet "refs/remotes/origin/$branch"; then
  echo "Release branch already exists: $branch" >&2
  exit 1
fi

if [ -z "$source_sha" ]; then
  coverage_refs=()
  while IFS= read -r ref; do
    if [[ "$ref" =~ ^refs/remotes/origin/release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ || "$ref" =~ ^refs/tags/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
      coverage_refs+=("$ref")
    fi
  done < <(git for-each-ref --format='%(refname)' refs/remotes/origin/release/ refs/tags/)
  develop_history="$(git rev-list origin/develop --not "${coverage_refs[@]}")"
  if [ -z "$develop_history" ]; then
    echo "No unreleased develop commits; existing release series or stable tags already cover develop: ${coverage_refs[*]}"
    exit 0
  fi
  runs="$(gh api --paginate --slurp "repos/$repository/actions/workflows/ci.yml/runs?status=completed&branch=develop&per_page=100" | jq -c '[.[].workflow_runs[] | select(.head_branch == "develop" and (.event == "schedule" or .event == "workflow_dispatch")) | {head_sha, html_url, conclusion}]')"
  while IFS= read -r candidate; do
    candidate_runs="$(jq -c --arg sha "$candidate" '[.[] | select(.head_sha == $sha) | {html_url, conclusion}]' <<< "$runs")"
    if [ "$candidate_runs" = '[]' ]; then continue; fi
    statuses="$(gh api --paginate --slurp "repos/$repository/commits/$candidate/statuses?per_page=100")"
    if jq -e --argjson runs "$candidate_runs" '
      [.[][] | select((.context == "develop-full-ci" or .context == "scheduled-full-ci") and (.target_url as $url | any($runs[]; .html_url == $url)))]
      | first
      | .state == "success" and (.target_url as $url | any($runs[]; .html_url == $url and .conclusion == "success"))
    ' <<< "$statuses" > /dev/null; then
      source_sha="$candidate"
      break
    fi
  done <<< "$develop_history"
  source_sha="${source_sha:-origin/develop}"
fi
source_sha="$(git rev-parse --verify "$source_sha^{commit}")"
git merge-base --is-ancestor "$source_sha" origin/develop || { echo 'Release source must be on develop' >&2; exit 1; }
for path in scripts/release-cut.sh scripts/scheduled-ci-finish.sh .github/workflows/ci.yml .github/workflows/release.yml; do
  if ! git cat-file -e "$source_sha:$path"; then
    echo "Release source lacks $path; select a newer develop SHA containing the release flow." >&2
    exit 1
  fi
done
candidate_ci="$(git show "$source_sha:.github/workflows/ci.yml")"
candidate_finalizer="$(git show "$source_sha:scripts/scheduled-ci-finish.sh")"
if [[ "$candidate_ci" != *'CI_RELEASE_BRANCH:'* || "$candidate_finalizer" != *'${CI_RELEASE_BRANCH'* ]]; then
  echo 'Release source does not pass release-branch identity from CI to its finalizer; select a newer develop SHA.' >&2
  exit 1
fi
covered_branches="$(git for-each-ref --contains="$source_sha" --format='%(refname:strip=3)' refs/remotes/origin/release/)"
while IFS= read -r existing; do
  if [[ "$existing" =~ ^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    echo "Release source already belongs to $existing; finish that series or select a newer develop SHA." >&2
    exit 1
  fi
done <<< "$covered_branches"
covered_tags="$(git tag --contains "$source_sha" --list 'v*')"
while IFS= read -r existing; do
  if [[ "$existing" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    echo "Release source is already contained in stable release $existing; select a newer develop SHA." >&2
    exit 1
  fi
done <<< "$covered_tags"
if [ "$automatic" = true ]; then admit_release; fi
gh api -X POST "repos/$repository/git/refs" -f ref="refs/heads/$branch" -f sha="$source_sha"
gh workflow run ci.yml --repo "$repository" --ref "$branch" -f force=true
printf 'Created %s at %s and dispatched the full suite.\n' "$branch" "$source_sha"
