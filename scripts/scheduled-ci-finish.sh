#!/usr/bin/env bash
set -euo pipefail

scratch_dir="${TMPDIR:-${TMP:-${TEMP:?scheduled validation requires provided scratch}}}"
jobs_file="$scratch_dir/scheduled-ci-jobs.json"
issues_file="$scratch_dir/scheduled-ci-issues.json"
evidence_file="$scratch_dir/scheduled-ci-evidence.jsonl"
run_url="$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"

run_conclusion="$(gh api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/attempts/$GITHUB_RUN_ATTEMPT" --jq .conclusion)"
if [ "$run_conclusion" = cancelled ]; then
  echo "Scheduled validation was cancelled; skipping reporting and release."
  exit 0
fi

gh api --paginate "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/attempts/$GITHUB_RUN_ATTEMPT/jobs?per_page=100" --jq '.jobs[]' | jq -s . > "$jobs_file"
if [ "${WORKFLOW_CANCELLED:-false}" = true ] && jq -e 'any(.[]; .conclusion == "cancelled")' "$jobs_file" > /dev/null; then
  echo "Scheduled validation jobs were cancelled; skipping reporting and release."
  exit 0
fi

report_failure() {
  go run ./tools/cifailure < "$jobs_file" >> "$evidence_file"
}

failures="$(jq -r '.[] | select(.name != "Finalize scheduled validation" and .conclusion != "success") | .name' "$jobs_file")"
if [ -n "$failures" ]; then
  report_failure
  exit 1
fi

publish_failure() {
  trap - ERR
  publisher_jobs_file="$scratch_dir/scheduled-ci-publisher-jobs.json"
  jq -n --arg url "$run_url" '[{name:"Finalize scheduled validation",conclusion:"failure",html_url:$url,id:0}]' > "$publisher_jobs_file"
  go run ./tools/cifailure < "$publisher_jobs_file" >> "$evidence_file"
  exit 1
}
trap publish_failure ERR

job_count="$(jq '[.[] | select(.name != "Finalize scheduled validation")] | length' "$jobs_file")"
if [ "$job_count" -ne 15 ]; then
  echo "Expected 15 scheduled validation jobs; observed $job_count" >&2
  false
fi

status_id="$(gh api -X POST "repos/$GITHUB_REPOSITORY/statuses/$CI_DEVELOP_SHA" -f state=success -f context=scheduled-full-ci -f target_url="$run_url" -f description='Full scheduled validation passed' --jq .id)"

git fetch --tags --force origin
for existing in $(git tag --points-at "$CI_DEVELOP_SHA" --list 'v*'); do
  if [[ "$existing" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] && [ "$(git cat-file -t "refs/tags/$existing")" = tag ]; then
    existing_message="$(git for-each-ref --format='%(contents)' "refs/tags/$existing")"
    if [[ "$existing_message" == *'"name":"scheduled-full-ci"'* ]]; then
      echo "Validated release $existing already contains $CI_DEVELOP_SHA; no new release or deployment."
      if [ "$GITHUB_REPOSITORY" = digitaldrywood/detent ]; then
        go run ./tools/cifailure < "$jobs_file" >> "$evidence_file"
      fi
      exit 0
    fi
  fi
done
git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
latest="$(git tag --list 'v*' --sort=-v:refname | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }')"
if [[ "$latest" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  tag="v${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.$((BASH_REMATCH[3] + 1))"
else
  tag=v0.0.1
fi

message_file="$scratch_dir/scheduled-ci-tag-message"
cat > "$message_file" <<EOF
Validated development build $tag

<!-- detent-release-provenance:{"schema":1,"repository":"$GITHUB_REPOSITORY","tag":"$tag","commit":"$CI_DEVELOP_SHA","checks":[{"name":"scheduled-full-ci","status":"completed","conclusion":"success","status_id":$status_id}]} -->
EOF
git tag -a "$tag" "$CI_DEVELOP_SHA" -F "$message_file"
git push origin "refs/tags/$tag"
gh api -X POST "repos/$GITHUB_REPOSITORY/actions/workflows/release.yml/dispatches" -f ref="$tag"
if [ "$GITHUB_REPOSITORY" = digitaldrywood/detent ]; then
  go run ./tools/cifailure < "$jobs_file" >> "$evidence_file"
else
  gh issue list --repo "$GITHUB_REPOSITORY" --label ci-scheduled-failure --state open --limit 10000 --json number > "$issues_file"
  for number in $(jq -r '.[].number' "$issues_file"); do
    gh issue close "$number" --repo "$GITHUB_REPOSITORY" --comment "Scheduled full validation passed: $run_url"
  done
fi
