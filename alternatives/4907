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
  while IFS= read -r candidate; do
    statuses="$(gh api --paginate --slurp "repos/$repository/commits/$candidate/statuses?per_page=100")"
    if jq -e '[.[][] | select(.context == "develop-full-ci")] | first | . != null and .state == "success"' <<< "$statuses" > /dev/null; then
      source_sha="$candidate"
      break
    fi
  done <<< "$develop_history"
  source_sha="${source_sha:-origin/develop}"
fi
source_sha="$(git rev-parse --verify "$source_sha^{commit}")"
git merge-base --is-ancestor "$source_sha" origin/develop || { echo 'Release source must be on develop' >&2; exit 1; }
for path in scripts/release-cut.sh scripts/release-validate-local.sh scripts/release-finalize.sh .github/workflows/release-finalize.yml .github/workflows/release.yml; do
  if ! git cat-file -e "$source_sha:$path"; then
    echo "Release source lacks $path; select a newer develop SHA containing the release flow." >&2
    exit 1
  fi
done
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
printf 'Created %s at %s. Validate it on a runner host with scripts/release-validate-local.sh %s and audit it with scripts/release-audit-local.sh %s.\n' "$branch" "$source_sha" "$branch" "$branch"
