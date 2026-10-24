#!/usr/bin/env bash
set -euo pipefail

scratch="${TMPDIR:-${TMP:-${TEMP:?release security audit requires provided scratch}}}"
request="$scratch/release-security-audit-request.json"
result="$scratch/release-security-audit-result.jsonl"
diagnostic="$scratch/release-security-audit-diagnostic.log"

jq -n \
  --arg project "$DETENT_PROJECT_ID" \
  --arg repository "$GITHUB_REPOSITORY" \
  --arg commit "$CI_DEVELOP_SHA" \
  --arg series "$CI_RELEASE_BRANCH" \
  --arg run "$GITHUB_RUN_ID" \
  --arg attempt "$GITHUB_RUN_ATTEMPT" \
  --arg url "$GITHUB_SERVER_URL/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/attempts/$GITHUB_RUN_ATTEMPT" \
  --arg directory "$2" \
  --arg model "${DETENT_RELEASE_AUDIT_MODEL:?release audit requires the project-selected model}" \
  '{identity: {project_id: $project, repository: $repository, commit: $commit, release_series: $series, run_id: $run, attempt: $attempt, job_id: "release-security-audit", run_url: $url}, repository_directory: $directory, model: $model, allowance: {invocations: 40, usage: {input_tokens: 200000, output_tokens: 50000, cached_input_tokens: 150000}}}' > "$request"

status=0
DETENT_RELEASE_AUDIT=true "$1" < "$request" > "$result" 2> "$diagnostic" || status=$?
cat "$result"
cat "$diagnostic" >&2
if [ "$status" -eq 0 ]; then
  exit 0
fi
if jq -Rse --arg commit "$CI_DEVELOP_SHA" '
  [split("\n")[] | fromjson? | select(has("status") and has("identity"))] | last
  | .identity.commit == $commit and .status == "incomplete" and .reason == "allowance_exhausted" and .infrastructure == []
' "$result" > /dev/null; then
  echo "Release audit allowance exhausted; retained incomplete coverage is advisory."
  exit 0
fi
echo "Release security audit infrastructure failure; the scheduled reporter retains CI instance attribution." >&2
exit "$status"
