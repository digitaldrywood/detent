#!/usr/bin/env bash
set -euo pipefail

commit="${1:?commit required}"
latest="${2:?latest published release required}"
check_runs="${3:?authenticated check runs required}"
check_id="${4:?operator source check ID required}"
scratch="${TMPDIR:-${TMP:-${TEMP:?operator release requires provided scratch}}}"
[[ "$commit" =~ ^[a-f0-9]{40}$ ]]
[[ "$latest" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
tag="v${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.$((BASH_REMATCH[3] + 1))-op.${commit:0:12}"
[[ "$check_id" =~ ^[1-9][0-9]*$ ]]
test "$(git rev-parse HEAD)" = "$commit"
git merge-base --is-ancestor "$commit" origin/develop

manifest="$(jq -ec --arg repository "$GITHUB_REPOSITORY" --arg tag "$tag" --arg commit "$commit" --argjson id "$check_id" '
  [.check_runs[] | select(.id == $id and .name == "Verify operator-landed source" and .head_sha == $commit and .status == "completed" and .conclusion == "success" and .app.slug == "github-actions")]
  | if length == 1 then {schema:1, repository:$repository, tag:$tag, commit:$commit, checks:[{name:.[0].name, status:.[0].status, conclusion:.[0].conclusion, check_run_id:.[0].id}]} else error("operator source requires its authenticated successful Actions check") end
' "$check_runs")"

if git show-ref --verify --quiet "refs/tags/$tag"; then
  test "$(git rev-parse "refs/tags/$tag^{commit}")" = "$commit"
  test "$(git cat-file -t "refs/tags/$tag")" = tag
else
  message="$(mktemp "$scratch/operator-release-message.XXXXXX")"
  trap 'rm -f "$message"' EXIT
  printf 'Operator-landed Cloud build %s\n\n<!-- detent-release-provenance:%s -->\n' "$tag" "$manifest" > "$message"
  git -c user.name='github-actions[bot]' -c user.email='41898282+github-actions[bot]@users.noreply.github.com' tag -a "$tag" "$commit" -F "$message"
fi
printf '%s\n' "$tag"
