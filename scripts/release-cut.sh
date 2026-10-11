#!/usr/bin/env bash
set -euo pipefail

version="${1:-}"
source_sha="${2:-}"
repository="${GITHUB_REPOSITORY:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}"
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
  runs="$(gh api --paginate --slurp "repos/$repository/actions/workflows/ci.yml/runs?status=completed&branch=develop&per_page=100" | jq -c '[.[].workflow_runs[] | select(.head_branch == "develop" and (.event == "schedule" or .event == "workflow_dispatch")) | {head_sha, html_url, conclusion}]')"
  develop_history="$(git rev-list origin/develop)"
  while IFS= read -r candidate; do
    candidate_runs="$(jq -c --arg sha "$candidate" '[.[] | select(.head_sha == $sha) | {html_url, conclusion}]' <<< "$runs")"
    if [ "$candidate_runs" = '[]' ]; then continue; fi
    statuses="$(gh api --paginate --slurp "repos/$repository/commits/$candidate/statuses?per_page=100")"
    if jq -e --argjson runs "$candidate_runs" '
      [.[][] | select(.context == "scheduled-full-ci" and (.target_url as $url | any($runs[]; .html_url == $url)))]
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
gh api -X POST "repos/$repository/git/refs" -f ref="refs/heads/$branch" -f sha="$source_sha"
gh workflow run ci.yml --repo "$repository" --ref "$branch" -f force=true
printf 'Created %s at %s and dispatched the full suite.\n' "$branch" "$source_sha"
