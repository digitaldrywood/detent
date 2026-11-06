#!/usr/bin/env bash
set -euo pipefail

for variable in ${!GIT_@}; do
  unset "$variable"
done
export GIT_NO_REPLACE_OBJECTS=1 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_GLOBAL=/dev/null
release_git_directory="$(pwd -P)"
git() {
  command git --no-replace-objects -C "$release_git_directory" "$@"
}

scratch_dir="${TMPDIR:-${TMP:-${TEMP:?release finalization requires provided scratch}}}"
: "${CI_RELEASE_BRANCH:?release finalization requires a release branch}"
: "${CI_DEVELOP_SHA:?release finalization requires the validated commit}"

dispatch_release() {
  local release_tag="$1" runs
  runs="$(gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/actions/workflows/release.yml/runs?head_sha=$CI_DEVELOP_SHA&per_page=100")"
  if jq -e --arg tag "$release_tag" 'any(.[].workflow_runs[]; .head_branch == $tag and .event == "workflow_dispatch")' <<< "$runs" > /dev/null; then
    echo "Release workflow already exists for $release_tag; retain its execution and retry failed jobs there."
    return
  fi
  gh api -X POST "repos/$GITHUB_REPOSITORY/actions/workflows/release.yml/dispatches" -f ref="$release_tag"
}

if [[ ! "$CI_RELEASE_BRANCH" =~ ^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "Invalid release branch: $CI_RELEASE_BRANCH" >&2
  false
fi
series="${CI_RELEASE_BRANCH#release/}"
head_sha="$(git ls-remote origin "refs/heads/$CI_RELEASE_BRANCH" | cut -f1)"
if [ "$head_sha" != "$CI_DEVELOP_SHA" ]; then
  echo "Validated commit is no longer the release branch head; skipping release."
  exit 0
fi

required_checks="${DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON:-[]}"
required_checks="$(jq -ce --arg require_audit "${DETENT_RELEASE_REQUIRE_AUDIT:-false}" '
  if type == "array" and all(.[]; type == "string" and length > 0)
  then . + ["scheduled-full-ci"] | map(select(. != "release-security-audit"))
    | if $require_audit == "true" then . + ["release-security-audit"] else . end | unique
  else error("release requirements must be an array of nonempty names") end
' <<< "$required_checks")"

platform_status() {
  local commit statuses latest
  local now
  now="$(date -u +%s)"
  local ancestors
  ancestors="$(git rev-list --topo-order "$CI_DEVELOP_SHA")" || return 1
  for commit in $ancestors; do
    statuses="$(gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/commits/$commit/statuses?per_page=100")" || return 1
    latest="$(jq -c --argjson creators "$status_creators" '[.[][] | select(.context == "platform-full-ci" and (.creator.login as $creator | $creators | index($creator) != null))] | max_by(.id)' <<< "$statuses")" || return 1
    if [ "$latest" = null ]; then continue; fi
    jq -e --argjson now "$now" '
      .state == "success" and (.id | type == "number") and .id > 0
      and (.created_at | fromdateiso8601) <= $now
      and (.created_at | fromdateiso8601) >= ($now - 86400)
    ' <<< "$latest" > /dev/null
    return $?
  done
  return 1
}

status_creators="$(jq -ce '
  if type == "array" and length > 0 and all(.[]; type == "string" and length > 0)
  then . else error("release status creators must be a nonempty allowlist") end
' <<< "${DETENT_RELEASE_STATUS_CREATORS_JSON:-[]}")"

if [ "$#" != 0 ]; then
  echo "Release finalization does not accept arguments." >&2
  exit 1
fi
override_reason=""
load_override() {
  local version="$1" required="$2"
  override_reason=""
  if ! git show-ref --verify --quiet "refs/tags/release-override/$version"; then
    if [ "$required" = true ]; then
      echo "Release $version lacks its signed override approval; refusing to redispatch." >&2
      return 1
    fi
    return 0
  fi
  local reason_file="$scratch_dir/release-override-reason"
  if ! go run ./tools/releaseprovenance -verify-override -tag "$version" -commit "$CI_DEVELOP_SHA" -output "$reason_file"; then
    echo "Release $version has invalid signed override approval; refusing to publish or redispatch." >&2
    return 1
  fi
  override_reason="$(cat "$reason_file")"
}

required_evidence() {
  if [ -z "$override_reason" ]; then
    platform_status || return 1
  fi
  gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/commits/$CI_DEVELOP_SHA/statuses?per_page=100" |
    jq -ce --argjson required "$required_checks" --argjson creators "$status_creators" --arg reason "$override_reason" '
      [.[][]] as $statuses
      | ($required | map(select(. != "scheduled-full-ci" or $reason == ""))) as $mandatory
      | [$mandatory[] as $name
          | ($statuses | map(select(.context == $name)) | max_by(.id))
          | select(.state == "success" and (.id | type == "number") and .id > 0
              and (.creator.login as $creator | $creators | index($creator) != null))
          | {name: .context, status: "completed", conclusion: "success", status_id: .id}]
      | select(length == ($mandatory | length))
      | {checks: .}
      + if $reason == "" then {} else
          ($statuses | map(select(.context == "scheduled-full-ci")) | max_by(.id)) as $suite
          | {validation_override: {reason: $reason, status: ($suite.state // "missing"),
              status_id: ($suite.id // 0), status_url: ($suite.target_url // "")}}
        end
    '
}

git fetch --tags --force origin
for existing in $(git tag --points-at "$CI_DEVELOP_SHA" --list "$series.*"); do
  if [[ "$existing" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] && [ "$(git cat-file -t "refs/tags/$existing")" = tag ]; then
    existing_message="$(git for-each-ref --format='%(contents)' "refs/tags/$existing")"
    if [[ "$existing_message" == *'<!-- detent-release-provenance:'* ]]; then
      existing_provenance="$(sed -n 's/^<!-- detent-release-provenance:\(.*\) -->$/\1/p' <<< "$existing_message")"
      if jq -e '.validation_override != null' <<< "$existing_provenance" > /dev/null; then
        load_override "$existing" true
        if ! jq -e --arg reason "$override_reason" '.validation_override.reason == $reason' <<< "$existing_provenance" > /dev/null; then
          echo "Release $existing override differs from signed approval; refusing to redispatch." >&2
          exit 1
        fi
      else
        override_reason=""
      fi
      if ! evidence="$(required_evidence)" || ! jq -e \
        --arg repository "$GITHUB_REPOSITORY" --arg tag "$existing" --arg commit "$CI_DEVELOP_SHA" \
        --argjson evidence "$evidence" '
          .schema == 1 and .repository == $repository and .tag == $tag and .commit == $commit
          and (.checks | sort_by(.name)) == ($evidence.checks | sort_by(.name))
        ' <<< "$existing_provenance" > /dev/null; then
        echo "Release $existing required status evidence is missing, failed, superseded, or incompatible with its provenance; refusing to redispatch its immutable tag." >&2
        false
      fi
      echo "Validated release $existing already contains $CI_DEVELOP_SHA; preserving its tag and checking release dispatch."
      dispatch_release "$existing"
      exit 0
    fi
  fi
done
git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
latest="$(git tag --list "$series.*" --sort=-v:refname | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }')"
if [[ "$latest" =~ ^v[0-9]+\.[0-9]+\.([0-9]+)$ ]]; then
  tag="$series.$((BASH_REMATCH[1] + 1))"
else
  tag="$series.0"
fi

load_override "$tag" false
if ! evidence="$(required_evidence)"; then
  echo "Release $series awaits required statuses $required_checks on $CI_DEVELOP_SHA; missing, failed, stale platform-full-ci (24h on an ancestor), or superseded evidence; not tagging."
  exit 0
fi

if ! evidence="$(required_evidence)"; then
  echo "Release $series awaits required statuses; missing, failed, superseded, or untrusted evidence; not tagging."
  exit 0
fi
provenance="$(jq -cn --arg repository "$GITHUB_REPOSITORY" --arg tag "$tag" --arg commit "$CI_DEVELOP_SHA" --argjson evidence "$evidence" \
  '{schema:1, repository:$repository, tag:$tag, commit:$commit} + $evidence')"
message_file="$scratch_dir/release-tag-message"
{
  if [ -n "$override_reason" ]; then
    printf 'Release build %s with full-validation override\n\n' "$tag"
    jq -r '.validation_override | "Full validation (scheduled-full-ci): " + .status,
      "Owner override reason: " + (.reason | tojson),
      (if .status_url != "" then "Validation status URL: " + .status_url else empty end)' <<< "$evidence"
  else
    printf 'Validated release build %s\n' "$tag"
  fi
  printf '\n<!-- detent-release-provenance:%s -->\n' "$provenance"
} > "$message_file"
if [ "$(git ls-remote origin "refs/heads/$CI_RELEASE_BRANCH" | cut -f1)" != "$head_sha" ]; then
  echo "Release branch head changed during finalization; skipping release."
  exit 0
fi
git tag -a "$tag" "$CI_DEVELOP_SHA" -F "$message_file"
git push origin "refs/tags/$tag"
dispatch_release "$tag"
