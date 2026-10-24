#!/usr/bin/env bash
set -euo pipefail

scratch_dir="${TMPDIR:-${TMP:-${TEMP:?scheduled validation requires provided scratch}}}"
jobs_file="$scratch_dir/scheduled-ci-jobs.json"
issues_file="$scratch_dir/scheduled-ci-issues.json"
evidence_file="$scratch_dir/scheduled-ci-evidence.jsonl"
run_url="$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"

delete_prebuilt() {
  gh api --paginate "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?per_page=100" --jq '.artifacts[] | select(.name | startswith("release-prebuilt-")) | .id' |
    while read -r artifact; do
      gh api -X DELETE "repos/$GITHUB_REPOSITORY/actions/artifacts/$artifact" > /dev/null
    done
}

run_conclusion="$(gh api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/attempts/$GITHUB_RUN_ATTEMPT" --jq .conclusion)"
if [ "$run_conclusion" = cancelled ] || [ "${WORKFLOW_CANCELLED:-false}" = true ]; then
  echo "Scheduled validation was cancelled; skipping reporting and release."
  if [ -n "${CI_RELEASE_BRANCH:-}" ]; then
    delete_prebuilt || true
  fi
  exit 0
fi

gh api --paginate "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/attempts/$GITHUB_RUN_ATTEMPT/jobs?per_page=100" --jq '.jobs[]' | jq -s . > "$jobs_file"

report_validation() {
  go run ./tools/cifailure < "$jobs_file" >> "$evidence_file"
}

if [ "${CI_SHOULD_RUN:-true}" = false ]; then
  if [ "$GITHUB_REPOSITORY" = digitaldrywood/detent ]; then
    report_validation
  fi
  echo "Scheduled validation was already attempted for this commit; recorded skipped execution without publishing validation or release evidence."
  exit 0
fi

failures="$(jq -r '.[] | select(.name != "Finalize scheduled validation" and .conclusion != "success") | .name' "$jobs_file")"
if [ -n "$failures" ]; then
  if [ -n "${CI_RELEASE_BRANCH:-}" ]; then
    delete_prebuilt
  fi
  report_validation
  exit 1
fi

publish_failure() {
  trap - ERR
  if [ -n "${CI_RELEASE_BRANCH:-}" ]; then
    delete_prebuilt || true
  fi
  publisher_jobs_file="$scratch_dir/scheduled-ci-publisher-jobs.json"
  jq -n --arg url "$run_url" '[{name:"Finalize scheduled validation",conclusion:"failure",html_url:$url,id:0}]' > "$publisher_jobs_file"
  go run ./tools/cifailure < "$publisher_jobs_file" >> "$evidence_file"
  exit 1
}
trap publish_failure ERR

report_validated() {
  trap - ERR
  if ! go run ./tools/cifailure < "$jobs_file" >> "$evidence_file"; then
    echo "Validated release $1 is published; reporting the green suite to the tracker failed. The scheduled reporting artifact retains the evidence." >&2
    exit 1
  fi
}

dispatch_release() {
  local release_tag="$1" runs
  runs="$(gh api --paginate --slurp "repos/$GITHUB_REPOSITORY/actions/workflows/release.yml/runs?head_sha=$CI_DEVELOP_SHA&per_page=100")"
  if jq -e --arg tag "$release_tag" 'any(.[].workflow_runs[]; .head_branch == $tag)' <<< "$runs" > /dev/null; then
    echo "Release workflow already exists for $release_tag; retain its execution and retry failed jobs there."
    return
  fi
  gh api -X POST "repos/$GITHUB_REPOSITORY/actions/workflows/release.yml/dispatches" -f ref="$release_tag"
}

if [ -z "${CI_RELEASE_BRANCH:-}" ]; then
  gh api -X POST "repos/$GITHUB_REPOSITORY/statuses/$CI_DEVELOP_SHA" -f state=success -f context=develop-full-ci -f target_url="$run_url" -f description='Full develop validation passed' > /dev/null
  echo "Develop validation passed; release tags are published only from release branches."
  if [ "$GITHUB_REPOSITORY" = digitaldrywood/detent ]; then
    report_validated develop
  fi
  exit 0
fi
if [[ ! "$CI_RELEASE_BRANCH" =~ ^release/v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "Invalid release branch: $CI_RELEASE_BRANCH" >&2
  false
fi
series="${CI_RELEASE_BRANCH#release/}"
head_sha="$(git ls-remote origin "refs/heads/$CI_RELEASE_BRANCH" | cut -f1)"
if [ "$head_sha" != "$CI_DEVELOP_SHA" ]; then
  echo "Validated commit is no longer the release branch head; skipping release."
  delete_prebuilt
  if [ "$GITHUB_REPOSITORY" = digitaldrywood/detent ]; then
    report_validated "$CI_RELEASE_BRANCH"
  fi
  exit 0
fi

git fetch --tags --force origin
for existing in $(git tag --points-at "$CI_DEVELOP_SHA" --list "$series.*"); do
  if [[ "$existing" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] && [ "$(git cat-file -t "refs/tags/$existing")" = tag ]; then
    existing_message="$(git for-each-ref --format='%(contents)' "refs/tags/$existing")"
    if [[ "$existing_message" == *'"name":"scheduled-full-ci"'* ]]; then
      existing_provenance="${existing_message#*<!-- detent-release-provenance:}"
      existing_provenance="${existing_provenance%% -->*}"
      existing_status="$(jq -er --arg repository "$GITHUB_REPOSITORY" --arg tag "$existing" --arg commit "$CI_DEVELOP_SHA" '
        select(.schema == 1 and .repository == $repository and .tag == $tag and .commit == $commit)
        | [.checks[] | select(.name == "scheduled-full-ci" and .status == "completed" and .conclusion == "success")]
        | select(length == 1) | .[0].status_id | select(type == "number" and . > 0)
      ' <<< "$existing_provenance")"
      existing_evidence="$(gh api "repos/$GITHUB_REPOSITORY/commits/$CI_DEVELOP_SHA/status")"
      if ! jq -e --arg commit "$CI_DEVELOP_SHA" --argjson status_id "$existing_status" '
        .sha == $commit and any(.statuses[]; .context == "scheduled-full-ci" and .id == $status_id and .state == "success")
      ' <<< "$existing_evidence" > /dev/null; then
        echo "Release $existing evidence is missing or superseded; refusing to redispatch its immutable tag." >&2
        false
      fi
      echo "Validated release $existing already contains $CI_DEVELOP_SHA; preserving its tag and checking release dispatch."
      dispatch_release "$existing"
      if [ "$GITHUB_REPOSITORY" = digitaldrywood/detent ]; then
        report_validated "$existing"
      fi
      exit 0
    fi
  fi
done
status_id="$(gh api -X POST "repos/$GITHUB_REPOSITORY/statuses/$CI_DEVELOP_SHA" -f state=success -f context=scheduled-full-ci -f target_url="$run_url" -f description='Full release validation passed' --jq .id)"
required_checks="${DETENT_RELEASE_REQUIRED_CHECK_NAMES_JSON:-[]}"
jq -e 'type == "array" and all(.[]; type == "string" and length > 0)' <<< "$required_checks" > /dev/null
required_names="$(jq -r '.[] | select(. != "scheduled-full-ci")' <<< "$required_checks")"
if [ -n "$required_names" ]; then
  statuses="$(gh api "repos/$GITHUB_REPOSITORY/commits/$CI_DEVELOP_SHA/status")"
  while IFS= read -r name; do
    if ! jq -e --arg sha "$CI_DEVELOP_SHA" --arg name "$name" '
      .sha == $sha and any(.statuses[]; .context == $name and .state == "success")
    ' <<< "$statuses" > /dev/null; then
      echo "Release $series awaits required status $name on $CI_DEVELOP_SHA; not tagging."
      exit 0
    fi
  done <<< "$required_names"
fi
git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
latest="$(git tag --list "$series.*" --sort=-v:refname | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }')"
if [[ "$latest" =~ ^v[0-9]+\.[0-9]+\.([0-9]+)$ ]]; then
  tag="$series.$((BASH_REMATCH[1] + 1))"
else
  tag="$series.0"
fi

message_file="$scratch_dir/scheduled-ci-tag-message"
cat > "$message_file" <<EOF
Validated release build $tag

<!-- detent-release-provenance:{"schema":1,"repository":"$GITHUB_REPOSITORY","tag":"$tag","commit":"$CI_DEVELOP_SHA","checks":[{"name":"scheduled-full-ci","status":"completed","conclusion":"success","status_id":$status_id}]} -->
EOF
git tag -a "$tag" "$CI_DEVELOP_SHA" -F "$message_file"
git push origin "refs/tags/$tag"
dispatch_release "$tag"
if [ "$GITHUB_REPOSITORY" = digitaldrywood/detent ]; then
  report_validated "$tag"
else
  gh issue list --repo "$GITHUB_REPOSITORY" --label ci-scheduled-failure --state open --limit 10000 --json number > "$issues_file"
  for number in $(jq -r '.[].number' "$issues_file"); do
    gh issue close "$number" --repo "$GITHUB_REPOSITORY" --comment "Scheduled full validation passed: $run_url"
  done
fi
