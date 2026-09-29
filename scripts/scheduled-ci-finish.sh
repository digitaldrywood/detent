#!/usr/bin/env bash
set -euo pipefail

jobs_file="$RUNNER_TEMP/scheduled-ci-jobs.json"
issues_file="$RUNNER_TEMP/scheduled-ci-issues.json"
run_url="$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID"

gh api --paginate "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/attempts/$GITHUB_RUN_ATTEMPT/jobs?per_page=100" --jq '.jobs[]' | jq -s . > "$jobs_file"
gh issue list --repo "$GITHUB_REPOSITORY" --label ci-scheduled-failure --state open --limit 100 --json number,body > "$issues_file"

report_failure() {
  local job="$1"
  local fingerprint number body_file
  fingerprint="$(printf 'scheduled-ci:%s:%s' "$GITHUB_REPOSITORY" "$job" | sha256sum | cut -d' ' -f1)"
  number="$(jq -r --arg fingerprint "$fingerprint" '.[] | select(.body | contains("fingerprint: " + $fingerprint)) | .number' "$issues_file" | head -1)"
  if [ -n "$number" ]; then
    gh issue comment "$number" --repo "$GITHUB_REPOSITORY" --body "Scheduled validation failed again: $run_url"
    return
  fi
  body_file="$RUNNER_TEMP/scheduled-ci-issue-${fingerprint}.md"
  cat > "$body_file" <<EOF
The scheduled full suite failed in job $job on development commit $GITHUB_SHA.

Run: $run_url

Fix the failing job, then let the next scheduled validation confirm the repair. A green run closes this issue.

\`\`\`detent-origin
origin_kind: doctor
instance_identity: github-actions
source_ref: $run_url
fingerprint: $fingerprint
\`\`\`
EOF
  gh issue create --repo "$GITHUB_REPOSITORY" --title "fix(ci): scheduled $job failure" --label detent:todo --label hotfix --label ci-scheduled-failure --body-file "$body_file"
}

failures="$(jq -r '.[] | select(.name != "Finalize scheduled validation" and .conclusion != "success") | .name' "$jobs_file")"
if [ -n "$failures" ]; then
  while IFS= read -r job; do
    report_failure "$job"
  done <<< "$failures"
  exit 1
fi

publish_failure() {
  trap - ERR
  report_failure 'Finalize scheduled validation'
  exit 1
}
trap publish_failure ERR

job_count="$(jq '[.[] | select(.name != "Finalize scheduled validation")] | length' "$jobs_file")"
if [ "$job_count" -ne 21 ]; then
  echo "Expected 21 scheduled validation jobs; observed $job_count" >&2
  false
fi

status_id="$(gh api -X POST "repos/$GITHUB_REPOSITORY/statuses/$GITHUB_SHA" -f state=success -f context=scheduled-full-ci -f target_url="$run_url" -f description='Full scheduled validation passed' --jq .id)"

git fetch --tags --force origin
git config user.name 'github-actions[bot]'
git config user.email '41898282+github-actions[bot]@users.noreply.github.com'
latest="$(git tag --list 'v*' --sort=-v:refname | awk '/^v[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }')"
if [[ "$latest" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
  tag="v${BASH_REMATCH[1]}.${BASH_REMATCH[2]}.$((BASH_REMATCH[3] + 1))"
else
  tag=v0.0.1
fi

message_file="$RUNNER_TEMP/scheduled-ci-tag-message"
cat > "$message_file" <<EOF
Validated development build $tag

<!-- detent-release-provenance:{"schema":1,"repository":"$GITHUB_REPOSITORY","tag":"$tag","commit":"$GITHUB_SHA","checks":[{"name":"scheduled-full-ci","status":"completed","conclusion":"success","status_id":$status_id}]} -->
EOF
git tag -a "$tag" "$GITHUB_SHA" -F "$message_file"
git push origin "refs/tags/$tag"
gh api -X POST "repos/$GITHUB_REPOSITORY/actions/workflows/release.yml/dispatches" -f ref="$tag"
for number in $(jq -r '.[].number' "$issues_file"); do
  gh issue close "$number" --repo "$GITHUB_REPOSITORY" --comment "Scheduled full validation passed: $run_url"
done
