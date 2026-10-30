#!/usr/bin/env bash
set -euo pipefail

branch="${1:-}"
if [ "$#" -ne 1 ] || [[ ! "$branch" =~ ^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "Usage: scripts/release-audit-local.sh release/vX.Y" >&2
  exit 1
fi
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
root="$(git -C "$script_dir" rev-parse --show-toplevel)"
cd "$root"
git fetch --tags origin "+refs/heads/$branch:refs/remotes/origin/$branch" '+refs/heads/release/v*:refs/remotes/origin/release/v*'
sha="$(git rev-parse --verify "refs/remotes/origin/$branch^{commit}")"
previous="$(git for-each-ref --format='%(refname:strip=3)' refs/remotes/origin/release/ | jq -Rrs --arg branch "$branch" '
  def version: capture("^release/v(?<major>0|[1-9][0-9]*)\\.(?<minor>0|[1-9][0-9]*)$") | [.major, .minor] | map(tonumber);
  ($branch | version) as $current
  | [split("\n")[] | select(test("^release/v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$"))
     | {branch: ., version: version} | select(.version < $current)]
  | sort_by(.version) | last | .branch // empty
')"
if [ -z "$previous" ]; then
  echo "No previous release series branch exists below $branch; cannot select an audit base." >&2
  exit 1
fi
base="$(git merge-base "refs/remotes/origin/$previous" "$sha")"
repository="$(gh repo view --json nameWithOwner --jq .nameWithOwner)"
mkdir -p "$root/tmp"
output_dir="$(mktemp -d "$root/tmp/release-audit.XXXXXX")"
worktree="$output_dir/source"
last_message="$output_dir/last-message.json"
findings="$output_dir/findings.json"
cleanup() {
  git worktree remove --force "$worktree"
}
trap cleanup EXIT
git worktree add --detach "$worktree" "$sha"
prompt="$(cat "$script_dir/release-audit-prompt.md")"
prompt="${prompt//\{BASE\}/$base}"
prompt="${prompt//\{HEAD\}/$sha}"
echo "Audit output: $output_dir/codex.log"
echo "Audit findings: $findings"
if ! codex exec --sandbox read-only -C "$worktree" -o "$last_message" "$prompt" < /dev/null > "$output_dir/codex.log" 2>&1; then
  echo "Codex failed; no audit status posted. See $output_dir/codex.log" >&2
  exit 1
fi
if ! jq -se '
  length == 1 and (.[0] |
    type == "object" and (keys == ["findings", "verdict"]) and
    (.verdict == "pass" or .verdict == "fail") and
    (.findings | type == "array") and
    all(.findings[];
      type == "object" and (keys == ["file", "line", "severity", "summary"]) and
      (.severity == "p1" or .severity == "p2" or .severity == "p3") and
      (.file | type == "string" and length > 0) and
      (.line | type == "number" and . >= 1 and floor == .) and
      (.summary | type == "string" and length > 0)) and
    ((.verdict == "fail") == any(.findings[]; .severity == "p1" or .severity == "p2")))
' "$last_message" > /dev/null; then
  echo "Invalid audit verdict; no audit status posted. See $last_message" >&2
  exit 1
fi
jq . "$last_message" > "$findings"
count="$(jq '[.findings[] | select(.severity == "p1" or .severity == "p2")] | length' "$findings")"
state=success
description="No P1/P2 findings (${base:0:7}..${sha:0:7})"
if [ "$count" -gt 0 ]; then
  state=failure
  description="$count P1/P2 findings"
fi
gh api -X POST "repos/$repository/statuses/$sha" -f context=release-security-audit -f state="$state" -f description="$description" > /dev/null
echo "Posted release-security-audit=$state on $sha."
if [ "$state" = failure ]; then
  exit 1
fi
if ! gh api --paginate --slurp "repos/$repository/commits/$sha/status?per_page=100" |
  jq -e --arg sha "$sha" '
    all(.[]; .sha == $sha) and
    ([.[].statuses[] | select(.context == "scheduled-full-ci")] | max_by(.id) |
      .state == "success" and (.id | type == "number") and .id > 0)
  ' > /dev/null; then
  echo "Waiting for local validation: no current scheduled-full-ci success on $sha; run scripts/release-validate-local.sh $branch."
  exit 0
fi
gh workflow run release-finalize.yml --repo "$repository" --ref "$branch"
echo "Dispatched release-finalize.yml on $branch."
