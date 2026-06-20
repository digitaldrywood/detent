# Agent-Executable Project Onboarding

This runbook takes a target repository from zero Detent setup to one dispatched
issue, or adds a new project to an existing Detent install. It starts with an
identity gate so a reference repository, example setup, current shell directory,
or existing Detent project cannot become the implicit target. Replace every
`<...>` placeholder before running a command.

Use these placeholders consistently:

| Placeholder | Meaning |
| --- | --- |
| `<customer-id>` | Customer or workstream id being onboarded. |
| `<repo-owner>` | GitHub owner of the target repository. |
| `<repo-name>` | GitHub repository name. |
| `<source-root>` | Local checkout of `<repo-owner>/<repo-name>`. |
| `<reference-repositories>` | Comma-separated `owner/repo` list used only for docs, examples, or tooling, or empty. |
| `<worktree-root>` | Directory where Detent will create issue worktrees. |
| `<github-mode>` | `project_v2` for GitHub ProjectV2-backed status, `issue_field` for boardless repository issue-field status, or `label` for repository issue-label status. |
| `<project-owner>` | GitHub org or user that owns the ProjectV2 board. |
| `<project-number>` | ProjectV2 number shown by `gh project list`. |
| `<project-node-id>` | ProjectV2 node id, starting with `PVT_`. |
| `<status-field-name>` | Organization issue field Detent uses as status in boardless mode, usually `Status`. |
| `<status-label-prefix>` | Repository label prefix Detent uses for status labels in label mode, usually `detent:`. |
| `<write-probe-issue>` | Scratch issue reference such as `<repo-owner>/<repo-name>#123` for doctor write probes. |
| `<detent-project-id>` | Local `global.yaml` project id, such as `api`. |

## Start Here — Determine The Mode

Do not assume this is a fresh install, and do not infer the target project from
the current directory or a pasted repository URL. You may inspect Detent
installation evidence before the identity gate, but repository-specific
discovery waits for Phase 0.5.

Classify the work into one of these modes:

- `new-install`: Detent is not installed or the human wants a fresh host. Follow
  [Bootstrap On A New Machine](../README.md#bootstrap-on-a-new-machine-humans-and-ai-agents)
  through tool installation and authentication, then continue with this runbook.
- `existing-install`: Detent is already installed or a service/dashboard appears
  to be running. Verify the binary, config path, registered projects, service
  health, GitHub auth, Codex auth, and `detent doctor` before proposing changes.
  If the configured workflow has `tracker.write_probe_issue`, treat
  `detent doctor` as mutating and defer that check until Phase 2.5 authorizes
  GitHub mutations.
- `add-project`: An existing Detent install is present and the target repository
  is not registered yet. Reuse the existing `global.yaml`, preserve current
  runtime settings unless the human chooses otherwise, then create or adopt a
  board, author `WORKFLOW.md`, register the project, and smoke test.

Record only Detent install/mode evidence before the identity interview:

```sh
ONBOARDING_DIR="${TMPDIR:-/tmp}/detent-onboarding-<repo-owner>-<repo-name>"
mkdir -p "$ONBOARDING_DIR"

{
  command -v detent || true
  detent version 2>/dev/null || true
  detent --format pretty config path 2>/dev/null || true
  gh auth status 2>&1 || true
  codex --version 2>/dev/null || true
} > "$ONBOARDING_DIR/mode-evidence.txt"

if command -v detent >/dev/null 2>&1; then
  detent --format pretty config path \
    > "$ONBOARDING_DIR/global-path.txt" 2>/dev/null || true
  GLOBAL_CONFIG="$(
    awk '/^path:/ {print $2}' "$ONBOARDING_DIR/global-path.txt" 2>/dev/null || true
  )"
  if test -n "$GLOBAL_CONFIG" && test -f "$GLOBAL_CONFIG"; then
    sed -n '1,240p' "$GLOBAL_CONFIG" > "$ONBOARDING_DIR/global-config.before.txt"
    awk '/^projects:/ {show=1} show {print}' "$GLOBAL_CONFIG" \
      > "$ONBOARDING_DIR/global-projects.txt"
  fi
fi

test -s "$ONBOARDING_DIR/mode-evidence.txt"
```

If Detent appears to be running, verify the live service before changing config.
Use the configured port when known, and use `detent doctor --port 0` when the
live process already owns the dashboard port:

```sh
detent doctor --port 0
curl -fsS http://127.0.0.1:<port>/health | jq -e '.status == "ok"'
curl -fsS http://127.0.0.1:<port>/api/v1/state
```

Before adding a project to an existing install, check whether it is already
registered and decide whether this is a new registration or a repair/update:

```sh
rg -n 'id: <detent-project-id>|workflow: .*<repo-name>|workdir: .*<repo-name>' \
  "$ONBOARDING_DIR/global-config.before.txt" 2>/dev/null || true
```

Treat existing registered projects as examples only. Do not reuse tracker mode,
status namespace, validation gate, workspace root, dashboard bind, scheduling
priority or weight, auto-promote policy, review policy, or mutation scope unless
the operator explicitly accepts that setting for this customer/project.

## Phase 0 — Preconditions

1. **Confirm Detent is installed or intentionally new.** For `new-install`,
   follow [Bootstrap On A New Machine steps 1-3](../README.md#bootstrap-on-a-new-machine-humans-and-ai-agents)
   before project onboarding. For `existing-install` and `add-project`, verify
   the detected binary and config path before changing anything. Verify:

   ```sh
   command -v detent
   detent version
   detent --format pretty config path
   ```

2. **Confirm GitHub CLI auth and scopes.** Detent needs GitHub auth for the
   selected tracker mode. Before the identity gate, verify only the local `gh`
   auth state and token scopes. Do not list target ProjectV2 boards,
   organization issue fields, repository labels, or target issues until Phase
   0.5 identity is confirmed and Phase 2 records `GITHUB_MODE`.

   ProjectV2 mode needs repository, organization, and ProjectV2 read/write
   scopes. Boardless issue-field mode needs repository issue access plus
   organization issue-field read access; classic PATs use `repo` and
   `read:org`. Label mode needs repository issue and label read/write access;
   classic PATs can use `repo`.

   For ProjectV2 mode, request:

   ```sh
   gh auth login --scopes "repo,read:org,read:project,project"
   ```

   If any ProjectV2 scope check fails for existing auth, refresh the token:

   ```sh
   gh auth refresh -h github.com --scopes "repo,read:org,read:project,project"
   ```

   For boardless issue-field mode with a classic PAT, request:

   ```sh
   gh auth login --scopes "repo,read:org"
   ```

   For repository label mode with a classic PAT, request:

   ```sh
   gh auth login --scopes "repo"
   ```

   In a remote or headless shell, avoid launching a remote GUI browser while
   preserving the terminal-side device flow:

   ```sh
   BROWSER=/usr/bin/true gh auth refresh -h github.com --scopes "repo,read:org,read:project,project"
   ```

   Press Enter in the terminal so `gh` starts polling, open
   `https://github.com/login/device` in the operator browser, enter the code,
   and wait for the terminal to print authentication completion. Verify each
   required scope independently:

   ```sh
   gh auth status
   gh auth status 2>&1 | rg '\brepo\b'
   gh auth status 2>&1 | rg '\bread:org\b'
   # ProjectV2 mode only:
   gh auth status 2>&1 | rg '\bread:project\b'
   gh auth status 2>&1 | rg "(^|[[:space:],'\"])project([[:space:],'\"]|$)"
   ```

   After Phase 0.5 identity confirmation and the explicit Phase 2
   `GITHUB_MODE` answer, run only the selected mode's target-specific read
   probe: `gh project list --owner <project-owner> --format json --limit 1`
   for ProjectV2, `gh api /orgs/<repo-owner>/issue-fields` for issue-field
   mode, or `gh api repos/<repo-owner>/<repo-name>/labels --paginate` for label
   mode. Defer write `project` verification until the first intentional
   ProjectV2 mutation, such as creating or linking a board, creating fields, or
   editing the status of a real existing item. Fine-grained PATs and GitHub
   Apps should grant Issue Fields organization read for issue-field mode,
   repository label access for label mode, Issues repository read/write when
   status moves or comments are enabled, Pull requests read/checks read for PR
   gates, and selected repository access.

3. **Confirm Codex is installed and signed in.** Detent dispatches agents
   through the Codex app-server. Verify:

   ```sh
   codex --version
   ```

4. **Stop before target-specific discovery.** Do not inspect target ProjectV2
   boards, organization issue fields, repository labels, issues, `WORKFLOW.md`,
   validation commands, deployment docs, or local target checkout contents until
   Phase 0.5 identity answers are explicit and confirmed.

## Phase 0.5 — Identity Gate

Before Phase 1 repository-specific discovery, create and validate a
customer/project identity record in `$ONBOARDING_DIR/answers.env`. This is an
operator decision checkpoint, not a recommendation. Recommendations can cite
evidence later, but they must not become selected answers.

Ask for and record these answers:

```sh
printf '%s\n' \
  'CUSTOMER_ID=<customer-or-workstream-id>' \
  'DETENT_PROJECT_ID=<local-detent-project-id>' \
  'TARGET_REPOSITORY=<repo-owner>/<repo-name>' \
  'TARGET_SOURCE_ROOT=<absolute-local-checkout-path>' \
  'REFERENCE_REPOSITORIES=<comma-separated-owner/repo-list-or-empty>' \
  'DETENT_ONBOARDING_MODE=<new-install|existing-install|add-project>' \
  > "$ONBOARDING_DIR/answers.env"
```

Present the interpretation back to the operator before inspecting target
resources:

```text
I will onboard project `<detent-project-id>` for customer/workstream
`<customer-id>`. The target repository is `<repo-owner>/<repo-name>` at
`<source-root>`. The following repositories are references only and will not be
registered as the target: `<reference-repositories>`. The onboarding mode is
`<new-install|existing-install|add-project>`. Is that correct?
```

Only after the operator confirms, append the identity confirmation:

```sh
printf '%s\n' \
  'IDENTITY_CONFIRMED=true' \
  >> "$ONBOARDING_DIR/answers.env"
```

Validate the identity gate:

```sh
test -f "$ONBOARDING_DIR/answers.env"
rg '^CUSTOMER_ID=[A-Za-z0-9_.-]+$' "$ONBOARDING_DIR/answers.env"
rg '^DETENT_PROJECT_ID=[A-Za-z0-9_.-]+$' "$ONBOARDING_DIR/answers.env"
rg '^TARGET_REPOSITORY=[^/]+/[^/]+$' "$ONBOARDING_DIR/answers.env"
rg '^TARGET_SOURCE_ROOT=/' "$ONBOARDING_DIR/answers.env"
rg '^REFERENCE_REPOSITORIES=' "$ONBOARDING_DIR/answers.env"
rg '^DETENT_ONBOARDING_MODE=(new-install|existing-install|add-project)$' "$ONBOARDING_DIR/answers.env"
rg '^IDENTITY_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase identity
```

The CLI validator rejects missing customer/workstream id, missing Detent project
id, malformed target repository, missing or relative target source root, a
target source root that is not a git checkout for the target repository, missing
reference repository separation, missing onboarding mode, missing
`IDENTITY_CONFIRMED=true`, and `GITHUB_MODE` answers recorded before identity is
valid.

If `<source-root>` does not exist, clone the confirmed target repository there
before validation:

```sh
git clone "https://github.com/<repo-owner>/<repo-name>.git" <source-root>
```

Confirm the target checkout only after identity is validated:

```sh
git -C <source-root> remote get-url origin
git -C <source-root> rev-parse --show-toplevel
```

Do not use a reference or tooling repository as the target. A wrong target repository failure example looks like this: the operator mentions
`digitaldrywood/detent-orchestration` as an example config and
`customer/api` as the actual project, but the agent runs issue, label, board,
or validation discovery against `digitaldrywood/detent-orchestration`. Stop,
rewrite `TARGET_REPOSITORY=customer/api`, verify `TARGET_SOURCE_ROOT` points to
that checkout, and rerun `detent onboarding validate-answers --phase identity`
before discovery resumes.

For repeated customer onboarding, a reviewable manifest may carry the same
answers in addition to `answers.env`:

```yaml
apiVersion: detent.dev/onboarding/v1
kind: OnboardingAnswers
customer:
  id: <customer-or-workstream-id>
  display_name: <human-readable-name>
project:
  id: <local-detent-project-id>
  repository: <repo-owner>/<repo-name>
  source_root: <absolute-local-checkout-path>
references:
  repositories:
    - <owner>/<repo-used-only-for-docs-or-examples>
detent:
  mode: add-project
tracker:
  github_status_source: <project_v2|issue_field|label>
mutation:
  confirmed: false
```

## Phase 0.6 — Status Source Decision

After identity is confirmed and before target-specific discovery, ask the
GitHub status-source question. This is separate from identity confirmation, and
it must still be an explicit operator answer.

Ask: "Use ProjectV2 board mode, boardless issue-field mode, or repository label
mode?" Explain that this answer maps to `tracker.github_status_source:
project_v2`, `tracker.github_status_source: issue_field`, or
`tracker.github_status_source: label` in `WORKFLOW.md`. Do not choose label
mode for the operator, and do not infer ProjectV2, issue-field, or label mode
from existing registered projects.

Record and validate the explicit answer:

```sh
printf '%s\n' \
  'GITHUB_MODE=<project_v2|issue_field|label>' \
  >> "$ONBOARDING_DIR/answers.env"
rg '^GITHUB_MODE=' "$ONBOARDING_DIR/answers.env"
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase decision
```

Hard stop: do not inspect target ProjectV2 boards, organization issue fields,
repository labels, target issues, `WORKFLOW.md`, validation commands, or
deployment docs until this `GITHUB_MODE` answer is recorded and
`detent onboarding validate-answers --phase decision` passes.

## GitHub GraphQL Rate-Budget Discipline

Treat every `gh project ...` command as GitHub GraphQL work. GitHub
ProjectV2 is GraphQL-backed, so `gh project list`, `field-list`, `item-list`,
`item-add`, and `item-edit` spend the same GraphQL primary rate-limit budget
as Detent's GitHub connector. When `github_token: gh` is configured, the
operator shell, Detent, and spawned Codex agents all use the same `gh` user
token and therefore the same user-token GraphQL bucket.

Boardless issue-field mode uses REST for field discovery and issue-field value
writes. Label mode uses REST for repository label discovery, issue reads by
label, and status-label writes. GitHub still reports REST and GraphQL budgets
separately, and `detent doctor` surfaces both so operators can see whether
boardless work is healthy without spending ProjectV2 GraphQL inventory budget.

GitHub's documented primary GraphQL limit for user-backed tokens is 5,000
points per hour. GitHub App installation tokens receive their own
installation-scoped GraphQL budget and can scale for larger installations.

GitHub reports REST and GraphQL primary limits separately. Check the GraphQL
bucket before ProjectV2 discovery, before bulk status cleanup, and before the
first smoke dispatch:

```sh
gh api rate_limit --jq '.resources.graphql | {limit, used, remaining, reset}'
```

If the remaining budget is low for the planned board size, stop before
dispatching an agent. Wait for reset, reduce ProjectV2 inventory work, or move
Detent to GitHub App installation authentication so the orchestrator receives
an installation-scoped GraphQL budget instead of sharing the operator's user
budget.

Use one saved inventory artifact per step. Avoid loops that repeatedly run
`gh project item-list --limit 1000` for board inventory, status cleanup, or
smoke verification. Prefer one paginated GraphQL inventory query or one
`gh project item-list` result written to `$ONBOARDING_DIR`, then run `jq`
against the local file.

Once an agent is running, stop operator polling against GitHub ProjectV2. The
agent still needs GraphQL budget for workpad, status, PR, and review activity.
Use the Detent dashboard or `/api/v1/state` for smoke verification and ongoing
operator checks.

Future Detent tooling should replace hand-rolled ProjectV2 inventory loops
with a CLI command or onboarding wizard page that inventories a board once and
prints status and priority counts from cached data.

## Phase 1 — Discover And Recommend

Do not ask questions in this phase. First re-run the identity and decision
validators, then inspect only the confirmed target setup and selected status
source. Write one grounded recommendation per remaining Phase 2 question, then
interview the human. For an existing install, include the mode evidence, current
config path, registered project table, and service health in the
recommendations before asking what to change.

1. **Create an onboarding notes directory.** Keep all discovery artifacts in
   one place so recommendations can cite evidence. Verify:

   ```sh
   ONBOARDING_DIR="${TMPDIR:-/tmp}/detent-onboarding-<repo-owner>-<repo-name>"
   mkdir -p "$ONBOARDING_DIR"
   test -d "$ONBOARDING_DIR"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase identity
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase decision
   ```

2. **Record the initial GitHub GraphQL budget for ProjectV2 mode.** Use the
   REST rate-limit endpoint before the first ProjectV2 discovery command. For
   `GITHUB_MODE=issue_field` or `GITHUB_MODE=label`, record a skipped artifact
   and do not spend GraphQL budget on board discovery. If the remaining budget
   is low, record the warning in the recommendation and avoid GraphQL-heavy
   board inventory until reset or GitHub App auth is available. Verify:

   ```sh
   GITHUB_MODE="$(awk -F= '/^GITHUB_MODE=/ {value=$2} END {print value}' "$ONBOARDING_DIR/answers.env")"
   if test "$GITHUB_MODE" = "project_v2"; then
     gh api rate_limit --jq '.resources.graphql | {limit, used, remaining, reset}' \
       > "$ONBOARDING_DIR/graphql-rate-limit.before-discovery.json"
     jq -r '"graphql remaining=\(.remaining) reset=\(.reset)"' \
       "$ONBOARDING_DIR/graphql-rate-limit.before-discovery.json"
     jq -e '.remaining >= 1000' "$ONBOARDING_DIR/graphql-rate-limit.before-discovery.json" \
       || printf 'WARNING: low GitHub GraphQL budget; avoid ProjectV2 inventory loops before reset\n'
   else
     printf '{"skipped":true,"reason":"GITHUB_MODE=%s"}\n' "$GITHUB_MODE" \
       > "$ONBOARDING_DIR/graphql-rate-limit.before-discovery.json"
   fi
   ```

3. **Inspect the validation surface.** Prefer a repo-local release gate over an
   invented command. Detent is stack-agnostic at the project boundary: identify
   the target repository's manifests, package managers, task runners, CI
   workflows, and existing release commands. If `make check` exists, recommend
   `gate.kind: command` with `gate.run: make check`. Otherwise recommend the
   closest local equivalent for the detected ecosystem, such as `mix test`,
   `bundle exec rspec`, `npm test`, `pnpm test`, `pytest`, `cargo test`,
   `composer test`, `mvn test`, `gradle test`, `dotnet test`, or a documented
   static-site/build check. If no command can be inferred, recommend
   `gate.kind: human_review` with an approval label only when the workflow
   explicitly wants a human label to promote. Verify:

   ```sh
   cd <source-root>
   MANIFEST_PATTERN='^(Makefile|Justfile|justfile|Taskfile\.ya?ml|package\.json|pnpm-lock\.yaml|yarn\.lock|bun\.lockb|deno\.jsonc?|go\.mod|mix\.exs|Gemfile|Rakefile|pyproject\.toml|requirements\.txt|tox\.ini|noxfile\.py|Cargo\.toml|composer\.json|pom\.xml|build\.gradle(\.kts)?|settings\.gradle(\.kts)?|Package\.swift|pubspec\.yaml|build\.zig|\.tool-versions|mise\.toml)$'
   GATE_PATTERN='make check|make test|go test|npm (run )?test|npm test|pnpm (run )?test|yarn test|bun test|deno test|mix test|bundle exec rspec|bundle exec rake|rspec|rake test|pytest|python -m pytest|tox|nox|cargo test|composer test|phpunit|mvn test|gradle test|./gradlew test|dotnet test|swift test|flutter test|zig build test|just |task '
   {
     printf 'Detected manifests:\n'
     fd -H -a -d 4 "$MANIFEST_PATTERN" . 2>/dev/null || true
     printf '\nCandidate validation commands:\n'
     test -f Makefile && awk -F: '/^[A-Za-z0-9][A-Za-z0-9_.-]*:/ {print "make " $1}' Makefile
     fd -a '' .github/workflows 2>/dev/null || true
     rg -n "$GATE_PATTERN" \
       .github/workflows Makefile Justfile justfile Taskfile.yml Taskfile.yaml \
       package.json deno.json deno.jsonc go.mod mix.exs Gemfile Rakefile \
       pyproject.toml tox.ini noxfile.py Cargo.toml composer.json pom.xml \
       build.gradle build.gradle.kts Package.swift pubspec.yaml build.zig \
       2>/dev/null || true
   } > "$ONBOARDING_DIR/gate.txt"
   test -f "$ONBOARDING_DIR/gate.txt"
   ```

4. **Inspect existing global scheduling.** Show the current project table
   before recommending `priority` and `weight`. Recommend `weight: 1` and
   `priority: 3` when no stronger signal exists. Recommend a higher weight or
   lower priority number only when the existing table shows this repo should
   outrank peers. Verify:

   ```sh
   detent --format pretty config path > "$ONBOARDING_DIR/global-path.txt"
   GLOBAL_CONFIG="$(awk '/^path:/ {print $2}' "$ONBOARDING_DIR/global-path.txt")"
   if test -f "$GLOBAL_CONFIG"; then
     awk '/^projects:/ {show=1} show {print}' "$GLOBAL_CONFIG" > "$ONBOARDING_DIR/global-projects.txt"
   else
     printf 'No existing global.yaml at %s\n' "$GLOBAL_CONFIG" > "$ONBOARDING_DIR/global-projects.txt"
   fi
   test -s "$ONBOARDING_DIR/global-projects.txt"
   ```

5. **Inspect open issue distribution.** Count candidate issues by label,
   assignee, author, and milestone before recommending authorization and intake
   filters. Verify:

   ```sh
   gh issue list --repo <repo-owner>/<repo-name> --state open --limit 1000 \
     --json number,title,labels,assignees,milestone,author,url \
     > "$ONBOARDING_DIR/issues.json"
   jq '{
     total: length,
     labels: ([.[] | .labels[].name] | sort | group_by(.) | map({name: .[0], count: length})),
     assignees: ([.[] | if (.assignees | length) == 0 then "Unassigned" else .assignees[].login end] | sort | group_by(.) | map({name: .[0], count: length})),
     authors: ([.[] | .author.login] | sort | group_by(.) | map({name: .[0], count: length})),
     milestones: ([.[] | .milestone.title // "No milestone"] | sort | group_by(.) | map({name: .[0], count: length}))
   }' "$ONBOARDING_DIR/issues.json" > "$ONBOARDING_DIR/issue-counts.json"
   jq -e '.total >= 0' "$ONBOARDING_DIR/issue-counts.json"
   ```

6. **Inspect existing ProjectV2 boards only for ProjectV2 mode.** Recommend
   reuse when a board clearly belongs to this repo or workstream; otherwise
   recommend creating a new board named after the repo or product. Skip this
   step for `GITHUB_MODE=issue_field` and `GITHUB_MODE=label`. This is the
   ProjectV2 read verification. Verify:

   ```sh
   GITHUB_MODE="$(awk -F= '/^GITHUB_MODE=/ {value=$2} END {print value}' "$ONBOARDING_DIR/answers.env")"
   if test "$GITHUB_MODE" = "project_v2"; then
     gh project list --owner <project-owner> --format json --limit 50 \
       > "$ONBOARDING_DIR/projects.json"
   else
     printf '{"projects":[],"skipped":true,"reason":"GITHUB_MODE=%s"}\n' "$GITHUB_MODE" \
       > "$ONBOARDING_DIR/projects.json"
   fi
   jq -e '.projects | length >= 0' "$ONBOARDING_DIR/projects.json"
   ```

7. **Inspect priority counts for ProjectV2 reuse candidates.** `priority_in`
   depends on the ProjectV2 `Priority` field, so gather counts from the
   strongest reuse candidate with one paginated inventory pass saved to a local
   artifact. Do not repeatedly call `gh project item-list --limit 1000`. For a
   new board or non-ProjectV2 mode, record an empty count table and recommend no
   `priority_in` filter until issues have been added and ranked. Verify:

   ```sh
   GITHUB_MODE="$(awk -F= '/^GITHUB_MODE=/ {value=$2} END {print value}' "$ONBOARDING_DIR/answers.env")"
   REUSE_PROJECT_NODE_ID="<reuse-candidate-project-node-id-or-empty>"
   if test "$GITHUB_MODE" = "project_v2" && test -n "$REUSE_PROJECT_NODE_ID"; then
     PRIORITY_QUERY='
       query($project: ID!, $after: String) {
         node(id: $project) {
           ... on ProjectV2 {
             items(first: 100, after: $after) {
               pageInfo { hasNextPage endCursor }
               nodes {
                 content {
                   ... on Issue {
                     state
                     repository { nameWithOwner }
                   }
                 }
                 priorityValue: fieldValueByName(name: "Priority") {
                   ... on ProjectV2ItemFieldSingleSelectValue { name }
                 }
               }
             }
           }
         }
       }'
     : > "$ONBOARDING_DIR/priority-items.jsonl"
     AFTER=""
     while :; do
       if test -n "$AFTER"; then
         gh api graphql \
           -f project="$REUSE_PROJECT_NODE_ID" \
           -f after="$AFTER" \
           -f query="$PRIORITY_QUERY" \
           > "$ONBOARDING_DIR/priority-page.json"
       else
         gh api graphql \
           -f project="$REUSE_PROJECT_NODE_ID" \
           -f query="$PRIORITY_QUERY" \
           > "$ONBOARDING_DIR/priority-page.json"
       fi
       jq -c '.data.node.items.nodes[]' "$ONBOARDING_DIR/priority-page.json" \
         >> "$ONBOARDING_DIR/priority-items.jsonl"
       jq -e '.data.node.items.pageInfo.hasNextPage' "$ONBOARDING_DIR/priority-page.json" \
         >/dev/null || break
       AFTER="$(jq -r '.data.node.items.pageInfo.endCursor' "$ONBOARDING_DIR/priority-page.json")"
     done
     jq -s --arg repo '<repo-owner>/<repo-name>' \
       '[.[] | select(.content.repository.nameWithOwner == $repo and .content.state == "OPEN") | (.priorityValue.name // "No priority")] | sort | group_by(.) | map({name: .[0], count: length})' \
       "$ONBOARDING_DIR/priority-items.jsonl" > "$ONBOARDING_DIR/priority-counts.json"
   else
     printf '[]\n' > "$ONBOARDING_DIR/priority-counts.json"
   fi
   jq -e 'type == "array"' "$ONBOARDING_DIR/priority-counts.json"
   ```

8. **Record recommendations before the interview.** The recommendation must
   cite the discovery artifact that produced it. Verify:

   ```sh
   printf '%s\n' \
     'mode: <new-install|existing-install|add-project, from mode evidence>' \
     'board: <reuse-or-create recommendation, from projects.json>' \
     'rate_budget: <GraphQL remaining/reset and low-budget warning>' \
     'scheduling: <priority/weight recommendation, from global-projects.txt>' \
     'authorization: <filter recommendation, from issue-counts.json and priority-counts.json>' \
     'dashboard_bind: <localhost/private-or-tailscale/all-interfaces recommendation>' \
     'gate: <gate recommendation, from gate.txt>' \
     'concurrency: <max agents and Merging cap recommendation>' \
     'review_policy: <hard stop or auto-promote recommendation>' \
     'prompt: <template or repo-specific recommendation, from repo docs>' \
     'intake: <bulk-add filter and initial Status recommendation>' \
     > "$ONBOARDING_DIR/recommendations.md"
   rg -n '^(mode|board|rate_budget|scheduling|authorization|dashboard_bind):' \
     "$ONBOARDING_DIR/recommendations.md"
   rg -n '^(gate|concurrency|review_policy|prompt|intake):' \
     "$ONBOARDING_DIR/recommendations.md"
   ```

## Phase 2 — Interview The Human

Ask only these decision questions. Present each as question, grounded
recommendation, and default-if-silent. Defaults are recommendations only; they
do not authorize GitHub, issue, label, `WORKFLOW.md`, or `global.yaml`
mutations. Record explicit answers in `$ONBOARDING_DIR/answers.env`.

0. **Mode.** `DETENT_ONBOARDING_MODE` was selected in Phase 0.5. If discovery
   shows the chosen mode is wrong, stop, update the identity answers, present
   the full interpretation again, and rerun
   `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase identity`.
   Do not infer mode from an existing registered project or carry it forward as
   a default.

1. **GitHub status source.** `GITHUB_MODE` was selected in Phase 0.6. If
   discovery shows the chosen status source is wrong, stop, update the explicit
   answer, rerun
   `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase decision`,
   and repeat Phase 1 for the selected mode. Do not inspect your recommendation
   as if it selected the mode, and do not continue to Phase 3, issue-field,
   label, `WORKFLOW.md`, or `global.yaml` mutation without this explicit
   `GITHUB_MODE` answer.

2. **ProjectV2 board.** Ask this only when `GITHUB_MODE=project_v2`: "Reuse an
   existing ProjectV2 board or create a new one?" List the boards from
   `$ONBOARDING_DIR/projects.json`. Recommendation source: matching board title,
   owner, item count, and whether the board already has repo work. Default if
   silent: reuse the strongest matching board; if none match, create
   `<repo-name>`. Verify:

   ```sh
   printf '%s\n' \
     'BOARD_MODE=<reuse|create>' \
     'PROJECT_OWNER=<project-owner>' \
     'PROJECT_NUMBER=<project-number-if-reuse>' \
     'PROJECT_TITLE=<project-title-if-create>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^BOARD_MODE=' "$ONBOARDING_DIR/answers.env"
   ```

3. **Boardless issue field.** Ask this only when `GITHUB_MODE=issue_field`:
   "Which organization issue field should Detent use for status?" Recommend
   `Status` unless inspection found a different single-select workflow field.
   Confirm that GitHub issue fields are issue-only: linked PR cards derive
   status from the linked issue, and PR-only cards cannot be moved by
   issue-field status writes. Verify:

   ```sh
   printf '%s\n' \
     'STATUS_FIELD_NAME=<status-field-name>' \
     >> "$ONBOARDING_DIR/answers.env"
   gh api /orgs/<repo-owner>/issue-fields \
     --jq '.[] | select(.name == "<status-field-name>") | {name,data_type,options}'
   rg '^STATUS_FIELD_NAME=' "$ONBOARDING_DIR/answers.env"
   ```

4. **Repository status labels.** Ask this only when `GITHUB_MODE=label`: "What
   repository label prefix should Detent use for status?" Recommend `detent:`
   unless the repository already has an intentional Detent status-label
   namespace. Do not choose label mode for the operator; ask even if label mode
   appears easiest for the repository. Explain that Detent maps each configured
   workflow state through `tracker.state_map`, slugifies the resulting state name,
   and prefixes it:
   `Todo` maps to `detent:todo`, `In Progress` maps to
   `detent:in-progress`, and with the default release flow
   `Cancelled: Done` state map, `Cancelled` maps to `detent:done`. These labels
   are the status source of truth; they are distinct from
   `tracker.authorization.labels.*` filters, `projects[].authorization`, and
   `agent.dispatch_priority_by_label`, which select or rank work but do not
   define state. Verify:

   ```sh
   printf '%s\n' \
     'STATUS_LABEL_PREFIX=<status-label-prefix>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^STATUS_LABEL_PREFIX=' "$ONBOARDING_DIR/answers.env"
   ```

5. **Kanban interaction.** Ask: "Should Detent's project Kanban be read-only or
   allow GitHub mutations from the dashboard?" Keep fleet `/kanban` read-only.
   For a project-scoped board on an operator-owned local or private Detent
   instance, recommend `integration` after `detent doctor` proves the relevant
   write probes. For a shared observer dashboard, or when write probes are
   missing or failing, recommend `read_only` until writes are proven.
   Integration mode requires doctor to prove ProjectV2 status write or
   issue-field status write, or status-label update, plus issue/PR comment
   write. Verify:

   ```sh
   printf '%s\n' \
     'KANBAN_MODE=<read_only|integration>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^KANBAN_MODE=' "$ONBOARDING_DIR/answers.env"
   ```

6. **Scheduling.** Ask: "What `global.yaml` `priority` from 1-4 and `weight`
   should this project receive?" Show `$ONBOARDING_DIR/global-projects.txt`.
   Disambiguate this from the board `Priority` field: `global.yaml` `priority`
   ranks projects on the host; the board `Priority` field ranks issues inside
   one project. Recommendation source: existing project weights, priority
   ranks, and whether this repo is release-critical relative to peers. Default
   if silent: `priority: 3`, `weight: 1`. Verify:

   ```sh
   printf '%s\n' \
     'GLOBAL_PRIORITY=<1-4>' \
     'GLOBAL_WEIGHT=<positive-integer>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^GLOBAL_(PRIORITY|WEIGHT)=' "$ONBOARDING_DIR/answers.env"
   ```

7. **Project color.** Ask: "Should this project have a fixed dashboard color,
   or should Detent assign one automatically?" Show
   `$ONBOARDING_DIR/global-projects.txt` so existing colors are discoverable.
   Explain that `projects[].color` is optional, accepts opaque CSS hex values
   such as `#1192e8`, and missing colors are deterministic from the project ID.
   Colors appear in the sidebar and the top-level multi-project `/kanban`
   board, but project names and IDs remain visible. Verify:

   ```sh
   printf '%s\n' \
     'PROJECT_COLOR=<#RRGGBB-or-empty>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^PROJECT_COLOR=' "$ONBOARDING_DIR/answers.env"
   ```

8. **Dispatch label ordering.** Ask: "When two issues have the same configured
   `Priority`, should labels break the tie before age?" Show the label counts
   from `$ONBOARDING_DIR/issue-counts.json` and recommend an ordered list from
   labels that represent work type or risk, such as `bug`, `regression`, then
   `enhancement` when those labels are common. Explain that `Priority` still
   wins first when available, unlisted labels rank last, and an empty answer
   means no label ordering. In label status-source mode, do not use
   `agent.dispatch_priority_by_label` for `detent:*` status labels unless the
   team intentionally wants status labels to also affect tie-breaking. Verify:

   ```sh
   printf '%s\n' \
     'DISPATCH_PRIORITY_BY_LABEL=<comma-separated-labels-or-empty>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^DISPATCH_PRIORITY_BY_LABEL=' "$ONBOARDING_DIR/answers.env"
   ```

9. **Instance name.** Ask: "What optional instance name should appear in
   Detent browser tabs and the navbar?" Recommendation source: the short
   hostname, existing `global.identity.name`, and any operator naming
   convention for this host. Default if silent: the short hostname. Verify:

   ```sh
   INSTANCE_NAME="$(hostname -s 2>/dev/null || hostname)"
   printf '%s\n' \
     "INSTANCE_NAME=${INSTANCE_NAME}" \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^INSTANCE_NAME=' "$ONBOARDING_DIR/answers.env"
   ```

10. **Authorization filters.** Ask: "Should Detent consider all board items or
   only items matching a filter?" Offer `none`, `labels.include`,
   `labels.exclude`, `assignee_in`, `author_in`, and `priority_in`.
   Recommendation source: live counts in `$ONBOARDING_DIR/issue-counts.json`
   and `$ONBOARDING_DIR/priority-counts.json`, plus any repo/workstream labels
   already in use. Show the total count for `none`, counts for each label,
   assignee, author, and priority option, and the remaining count for any
   proposed `labels.exclude`. Default if silent: no filter for a dedicated repo
   board; otherwise the narrowest label or assignee filter that matches the
   intended workstream. In label status-source mode, keep authorization filters
   focused on workstream labels such as `documentation` or `backend`; do not
   use the `detent:*` status labels as authorization filters unless you are
   deliberately narrowing the state machine. Verify:

   ```sh
   printf '%s\n' \
     'AUTHORIZATION_KIND=<none|labels.include|labels.exclude|assignee_in|author_in|priority_in>' \
     'AUTHORIZATION_VALUE=<value-or-empty>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^AUTHORIZATION_' "$ONBOARDING_DIR/answers.env"
   ```

11. **Dashboard bind.** Ask: "How should the Detent dashboard bind:
   localhost-only, a private/Tailscale IP, or all interfaces?" Recommendation
   source: the operator's access path, whether SSH tunnels or VPN/Tailscale are
   expected, the host firewall, and any known private interface addresses.
   Default if silent: `127.0.0.1` for localhost-only access through SSH
   tunnels. Use a specific private or Tailscale IP for VPN-only exposure. Use
   `0.0.0.0` only on trusted private networks because it exposes the dashboard
   on every interface, not just Tailscale. Verify:

   ```sh
   printf '%s\n' \
     'DASHBOARD_HOST=<127.0.0.1|tailscale-or-private-ip|0.0.0.0>' \
     'DASHBOARD_REMOTE_HOST=<tailscale-or-private-ip-or-empty>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^DASHBOARD_' "$ONBOARDING_DIR/answers.env"
   ```

12. **Validation gate.** Ask: "Use the detected command, a custom command, or a
   human review label gate? If this is a command gate, should auto-promotion
   require an automated GitHub PR review from a bot?" Recommendation source:
   `$ONBOARDING_DIR/gate.txt`, detected manifests, task runners, CI workflow
   commands, and the repo's review policy. Default if silent: detected
   `make check` when present with `require_automated_review: true`; otherwise
   use the strongest ecosystem-specific command from the repo evidence, or
   `kind: human_review` with `approval_label: human-approved` when no reliable
   local command exists. Verify:

   ```sh
   printf '%s\n' \
     'GATE_KIND=<command|human_review>' \
     'GATE_RUN=<command-if-command>' \
     'GATE_REQUIRE_AUTOMATED_REVIEW=<true|false-if-command>' \
     'GATE_APPROVAL_LABEL=<label-if-human-review>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^GATE_' "$ONBOARDING_DIR/answers.env"
   ```

12. **Concurrency.** Ask: "How many agents may this project run at once?"
   Recommendation source: host capacity, existing `global.yaml` projects, and
   the repo's gate cost. Default if silent: `agent.max_concurrent_agents: 5`
   for an active code repo, lower for expensive gates. State that
   `Merging: 1` is required. Verify:

   ```sh
   printf '%s\n' \
     'MAX_CONCURRENT_AGENTS=<positive-integer>' \
     'MERGING_CONCURRENCY=1' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^MERGING_CONCURRENCY=1$' "$ONBOARDING_DIR/answers.env"
   ```

   `agent.max_concurrent_agents_by_state` is **per-project** configuration: it
   lives only in each WORKFLOW.md (`internal/config/config.go`), is enforced
   per project orchestrator (`internal/orchestrator/dispatch_planner.go`
   `stateSlotsAvailable`) and per project scheduler
   (`internal/project/project.go` `CapacityByState`). The global settings
   struct (`internal/config/global/config.go`) has no by-state field, and the
   global dispatch gate (`internal/scheduler/global_gate.go`) is state-blind —
   it only arbitrates total pool slots and weighted/fair-share selection.

   `Merging: 1` serializes merges **within one project only**; multiple
   projects merge concurrently because each merge train targets its own
   repository. For multiple instances sharing one board/repo, serialization
   comes from `tracker.claims`, not the per-state cap.

13. **Review policy.** Ask: "Should Detent hard-stop at `Human Review`, or may
   it auto-promote to `Merging` after the human-defined criteria are true?"
   Recommendation source: repo risk, issue labels, review requirements, and how
   much trust the human wants to delegate. Default if silent:
   `agent.auto_promote.enabled: false`, the safe hard stop. Both modes are
   fully supported, and this is the human's call.

   For criteria-based auto-promote, use `agent.auto_promote.enabled`,
   `quiet_seconds`, `optout_label`, `allowed_issue_labels`, and the top-level
   command gate's `require_automated_review` setting. `quiet_seconds` is the
   quiet period after observed issue/status/review activity and linked PR
   activity such as a fresh push to the PR head, `optout_label` is the
   per-issue escape hatch, and `allowed_issue_labels` is an allowlist such as
   `documentation` for low-risk issue classes. When automated review is
   required, a Codex/ChatGPT/Claude review on an older commit does not clear
   this gate. Verify:

   ```sh
   printf '%s\n' \
     'AUTO_PROMOTE_ENABLED=<true|false>' \
     'AUTO_PROMOTE_QUIET_SECONDS=<seconds>' \
     'AUTO_PROMOTE_REQUIRE_AUTOMATED_REVIEW=<true|false-if-command>' \
     'AUTO_PROMOTE_OPTOUT_LABEL=<label>' \
     'AUTO_PROMOTE_ALLOWED_LABELS=<comma-separated-labels-or-empty>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^AUTO_PROMOTE_' "$ONBOARDING_DIR/answers.env"
   ```

14. **Dependency waiting policy.** Ask: "Should dependency-waiting issues stay
   in `Todo` and be gated by Detent, or should they sit in `Blocked` and be
   auto-unblocked when dependencies clear?" Default if silent:
   `tracker.dependency_auto_unblock.enabled: false`. Use the `Blocked`
   auto-unblock mode only when the team writes explicit `Depends on:` or
   `Blocked by:` lines for dependency blockers; Detent will not clear unrelated
   human blockers. If dependency-waiting issues are placed in `Blocked` while
   this setting stays disabled, Detent will only display them as blocked and
   will not move them back to `Todo`. Verify:

   ```sh
   printf '%s\n' \
     'DEPENDENCY_AUTO_UNBLOCK_ENABLED=<true|false>' \
     'DEPENDENCY_AUTO_UNBLOCK_SOURCE_STATES=Blocked' \
     'DEPENDENCY_AUTO_UNBLOCK_TARGET_STATE=Todo' \
     'DEPENDENCY_AUTO_UNBLOCK_READINESS=terminal_or_merged' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^DEPENDENCY_AUTO_UNBLOCK_' "$ONBOARDING_DIR/answers.env"
   ```

15. **Prompt body.** Ask: "Use the template prompt or add repo-specific
   instructions?" Recommendation source: `CLAUDE.md`, `AGENTS.md`,
   `CONTRIBUTING.md`, README development commands, manifests, and CI workflows
   in `<source-root>`. Default if silent: template prompt plus any repo
   authority files found. Verify:

   ```sh
   AUTHORITY_PATTERN='^(CLAUDE|AGENTS|CONTRIBUTING|README)\.md$'
   MANIFEST_PATTERN='^(Makefile|Justfile|justfile|Taskfile\.ya?ml|package\.json|deno\.jsonc?|go\.mod|mix\.exs|Gemfile|Rakefile|pyproject\.toml|Cargo\.toml|composer\.json|pom\.xml|build\.gradle(\.kts)?|Package\.swift|pubspec\.yaml|build\.zig|\.tool-versions|mise\.toml)$'
   GATE_PATTERN='make check|make test|go test|npm (run )?test|npm test|pnpm (run )?test|yarn test|bun test|deno test|mix test|bundle exec rspec|rspec|rake test|pytest|python -m pytest|cargo test|composer test|phpunit|mvn test|gradle test|./gradlew test|dotnet test|swift test|flutter test|zig build test'
   {
     fd -H -a -d 4 "$AUTHORITY_PATTERN" <source-root> 2>/dev/null || true
     fd -H -a -d 4 "$MANIFEST_PATTERN" <source-root> 2>/dev/null || true
     rg -n "$GATE_PATTERN" \
       <source-root>/README.md <source-root>/CONTRIBUTING.md \
       <source-root>/.github/workflows <source-root>/package.json \
       <source-root>/mix.exs <source-root>/Gemfile <source-root>/Rakefile \
       <source-root>/pyproject.toml <source-root>/Cargo.toml \
       <source-root>/composer.json <source-root>/pom.xml \
       <source-root>/build.gradle <source-root>/build.gradle.kts \
       2>/dev/null || true
   } > "$ONBOARDING_DIR/prompt-evidence.txt"
   printf 'PROMPT_MODE=<template|repo-specific>\n' >> "$ONBOARDING_DIR/answers.env"
   rg '^PROMPT_MODE=' "$ONBOARDING_DIR/answers.env"
   ```

16. **Issue intake.** Ask: "Which issue filter should be bulk-added, should the
   initial `Status` be `Backlog` or `Todo`, and should the human enable the
   auto-add workflow?" Recommendation source: `$ONBOARDING_DIR/issue-counts.json`
   and the authorization answer. Default if silent: bulk-add the narrowest safe
   filter to `Backlog`, then move one known issue to `Todo` for the smoke test.
   Verify:

   ```sh
   printf '%s\n' \
     'INTAKE_GH_FLAGS=<gh-issue-list-flags>' \
     'INITIAL_STATUS=<Backlog|Todo>' \
     'ENABLE_AUTO_ADD=<true|false>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^(INTAKE_GH_FLAGS|INITIAL_STATUS|ENABLE_AUTO_ADD)=' "$ONBOARDING_DIR/answers.env"
   ```

## Phase 2.5 — Mutation Authorization

Stop here before Phase 3 or any other command that can create, link, mutate, or
delete GitHub Projects, issue fields, labels, issues, PRs, `WORKFLOW.md`, or
`global.yaml`. Show the operator `$ONBOARDING_DIR/recommendations.md` and the
complete `$ONBOARDING_DIR/answers.env`, then ask: "May I execute the selected
mutation steps using these exact answers?" Defaults from Phase 2 are still only
recommendations; an unanswered default must not authorize any external or local
config mutation.

Record the explicit confirmation only after the operator says yes. This removes
stale confirmations first and appends the new confirmation as the final
nonblank line. If any Phase 2 answer changes later, rerun Phase 2.5 and record
a fresh confirmation.

```sh
CONFIRMATION_FILE="$(mktemp)"
rg -v '^MUTATION_CONFIRMED=' "$ONBOARDING_DIR/answers.env" > "$CONFIRMATION_FILE" || true
mv "$CONFIRMATION_FILE" "$ONBOARDING_DIR/answers.env"
printf '%s\n' \
  'MUTATION_CONFIRMED=true' \
  >> "$ONBOARDING_DIR/answers.env"
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"
```

Run this gate before every mutating phase and before every one-off mutating
command:

```sh
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
test -f "$ONBOARDING_DIR/answers.env"
rg '^DETENT_ONBOARDING_MODE=' "$ONBOARDING_DIR/answers.env"
rg '^GITHUB_MODE=(project_v2|issue_field|label)$' "$ONBOARDING_DIR/answers.env"
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

GITHUB_MODE="$(
  awk -F= '/^GITHUB_MODE=/ {value=$2} END {print value}' "$ONBOARDING_DIR/answers.env"
)"
case "$GITHUB_MODE" in
  project_v2)
    rg '^BOARD_MODE=(reuse|create)$' "$ONBOARDING_DIR/answers.env"
    rg '^PROJECT_OWNER=' "$ONBOARDING_DIR/answers.env"
    BOARD_MODE="$(
      awk -F= '/^BOARD_MODE=/ {value=$2} END {print value}' "$ONBOARDING_DIR/answers.env"
    )"
    case "$BOARD_MODE" in
      reuse) rg '^PROJECT_NUMBER=' "$ONBOARDING_DIR/answers.env" ;;
      create) rg '^PROJECT_TITLE=' "$ONBOARDING_DIR/answers.env" ;;
    esac
    ;;
  issue_field)
    rg '^STATUS_FIELD_NAME=' "$ONBOARDING_DIR/answers.env"
    ;;
  label)
    rg '^STATUS_LABEL_PREFIX=' "$ONBOARDING_DIR/answers.env"
    ;;
  *)
    printf 'Unsupported GITHUB_MODE=%s\n' "$GITHUB_MODE" >&2
    exit 1
    ;;
esac
```

## Phase 3 — Create Or Adopt The Status Source

Run the ProjectV2 steps only when `GITHUB_MODE=project_v2`. When
`GITHUB_MODE=issue_field`, skip board creation/linking and run the boardless
issue-field verification step instead. When `GITHUB_MODE=label`, skip both
ProjectV2 and issue-field setup and verify repository status labels.

Before any command in this phase that can mutate GitHub ProjectV2, issue-field,
or label resources, rerun the Phase 2.5 gate:

```sh
test -f "$ONBOARDING_DIR/answers.env"
rg '^GITHUB_MODE=(project_v2|issue_field|label)$' "$ONBOARDING_DIR/answers.env"
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"
```

Before any ProjectV2 board mutation such as `gh project create`, `gh project
link`, `gh project field-create`, or `gh project item-edit`, also run:

```sh
rg '^GITHUB_MODE=project_v2$' "$ONBOARDING_DIR/answers.env"
rg '^BOARD_MODE=(reuse|create)$' "$ONBOARDING_DIR/answers.env"
rg '^PROJECT_OWNER=' "$ONBOARDING_DIR/answers.env"
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"
```

1. **Adopt an existing board when the answer says reuse.** Record the number
   and node id for later commands. Run only when `GITHUB_MODE=project_v2`.
   Verify:

   ```sh
   gh project view <project-number> --owner <project-owner> --format json \
     > "$ONBOARDING_DIR/project.json"
   jq -e '.id | startswith("PVT_")' "$ONBOARDING_DIR/project.json"
   ```

2. **Create a board when the answer says create.** Run only when
   `GITHUB_MODE=project_v2`. GitHub creates the default `Status` field; Detent
   still needs you to create the board itself. Verify:

   ```sh
   rg '^GITHUB_MODE=project_v2$' "$ONBOARDING_DIR/answers.env"
   rg '^BOARD_MODE=create$' "$ONBOARDING_DIR/answers.env"
   rg '^PROJECT_OWNER=' "$ONBOARDING_DIR/answers.env"
   rg '^PROJECT_TITLE=' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   gh project create --owner <project-owner> --title "<project-title>" --format json \
     > "$ONBOARDING_DIR/project.json"
   jq -e '.number and (.id | startswith("PVT_"))' "$ONBOARDING_DIR/project.json"
   ```

3. **Ensure the `Priority` field exists.** Run only when
   `GITHUB_MODE=project_v2`.
   Detent can add missing options inside an existing field, but it never
   creates the field. Reused boards can already have this field, so check before
   creating it. If `Priority` exists but is not a single-select field, stop and
   ask the human to rename the conflicting field or choose another board. Verify:

   ```sh
   rg '^GITHUB_MODE=project_v2$' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   PROJECT_NUMBER="$(jq -r '.number' "$ONBOARDING_DIR/project.json")"
   gh project field-list "$PROJECT_NUMBER" --owner <project-owner> --format json \
     > "$ONBOARDING_DIR/fields.before.json"
   if jq -e '.fields[] | select(.name == "Priority" and (.options | type == "array"))' \
     "$ONBOARDING_DIR/fields.before.json" >/dev/null; then
     echo "Priority field already exists; reusing it"
   elif jq -e '.fields[] | select(.name == "Priority")' \
     "$ONBOARDING_DIR/fields.before.json" >/dev/null; then
     echo "Priority exists but is not a single-select field" >&2
     exit 1
   else
     gh project field-create "$PROJECT_NUMBER" --owner <project-owner> \
       --name Priority \
       --data-type SINGLE_SELECT \
       --single-select-options "Urgent,High,Medium,Low"
   fi
   gh project field-list "$PROJECT_NUMBER" --owner <project-owner> --format json \
     | jq -e '.fields[] | select(.name == "Priority" and (.options | type == "array"))'
   ```

4. **Link the repository to the board.** Run only when
   `GITHUB_MODE=project_v2`.
   This keeps the project discoverable from the repo. Verify:

   ```sh
   rg '^GITHUB_MODE=project_v2$' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   gh project link "$PROJECT_NUMBER" --owner <project-owner> --repo <repo-name>
   gh project view "$PROJECT_NUMBER" --owner <project-owner> --format json --jq '.url'
   ```

5. **Confirm required ProjectV2 fields.** Run only when
   `GITHUB_MODE=project_v2`.
   Detent auto-provisions missing `Status` and `Priority` options on first run,
   but never the board or fields themselves. Verify:

   ```sh
   gh project field-list "$PROJECT_NUMBER" --owner <project-owner> --format json \
     | jq -e '[.fields[].name] as $names | all(["Status","Priority"][]; . as $want | $names | index($want))'
   ```

6. **Clean up inherited ProjectV2 statuses before first Detent dispatch.** Run
   only when `GITHUB_MODE=project_v2`. Reused boards often carry status options
   and stale item placements from a predecessor
   orchestrator. Inventory the board before the first Detent start so the
   operator can distinguish intentional custom lanes from old active or
   terminal columns. Verify:

   ```sh
   PROJECT_NUMBER="$(jq -r '.number' "$ONBOARDING_DIR/project.json")"
   PROJECT_NODE_ID="$(jq -r '.id' "$ONBOARDING_DIR/project.json")"
   gh project field-list "$PROJECT_NUMBER" --owner <project-owner> --format json \
     > "$ONBOARDING_DIR/fields.before-detent.json"
   gh project item-list "$PROJECT_NUMBER" --owner <project-owner> --format json --limit 1000 \
     > "$ONBOARDING_DIR/project-items.before-detent.json"
   gh project item-list "$PROJECT_NUMBER" --owner <project-owner> \
     --query 'repo:<repo-owner>/<repo-name> is:issue is:open' \
     --format json --limit 1000 \
     > "$ONBOARDING_DIR/repo-open-project-items.before-detent.json"

   jq -r '.fields[] | select(.name == "Status") | .options[].name' \
     "$ONBOARDING_DIR/fields.before-detent.json" \
     > "$ONBOARDING_DIR/status-options.before-detent.txt"
   cat "$ONBOARDING_DIR/status-options.before-detent.txt"
   ```

   Detent will add missing canonical `Status` options from `WORKFLOW.md` on
   first run and reorder known options into Detent's configured order. It
   preserves extra custom options after the configured Detent states; do not
   delete a custom option unless the operator confirms it is a predecessor
   leftover.

   Print status counts for every board item and for open issues from the target
   repository:

   ```sh
   jq '[.items[] | (.status // "No status")] | sort | group_by(.) | map({status: .[0], count: length})' \
     "$ONBOARDING_DIR/project-items.before-detent.json"
   jq '[.items[] | (.status // "No status")] | sort | group_by(.) | map({status: .[0], count: length})' \
     "$ONBOARDING_DIR/repo-open-project-items.before-detent.json"
   ```

   Compare option names against the configured Detent states, including
   case-only differences such as `In progress` versus `In Progress`. If this
   project uses custom state names in `WORKFLOW.md`, replace the default list
   below with those configured states. Verify:

   ```sh
   printf '%s\n' \
     Backlog Todo 'In Progress' Blocked 'Human Review' Rework Merging Done Cancelled \
     > "$ONBOARDING_DIR/detent-status-options.txt"
   awk '
     NR == FNR {
       canonical[$0] = 1
       canonical_lower[tolower($0)] = $0
       next
     }
     !($0 in canonical) {
       if (tolower($0) in canonical_lower) {
         printf "case-different\t%s\tcanonical:%s\n", $0, canonical_lower[tolower($0)]
       } else {
         printf "custom-or-legacy\t%s\n", $0
       }
     }
   ' "$ONBOARDING_DIR/detent-status-options.txt" \
     "$ONBOARDING_DIR/status-options.before-detent.txt"
   ```

   Treat old active or terminal names such as `Ready`, `In progress`,
   `In review`, or `Done` as migration questions, not automatic truths. Ask the
   operator: "Should open issues currently in inherited active or terminal
   statuses stay where they are, be closed, or move back to `Backlog` before
   Detent starts?"

   Also ask what to do with empty non-Detent `Status` options that do not map
   to the configured workflow. The operator should choose one action for each
   option: remove it from the board, keep it as an intentional custom column, or
   map it through the workflow state configuration. The default recommendation
   is to remove empty non-mapping predecessor options during setup after status
   counts have been reported.

   The default recommendation is to move open issues from predecessor active or
   terminal statuses back to `Backlog`, unless the operator confirms a status
   is intentional or the issue should be closed. If `Backlog` does not exist
   yet, create it manually or start Detent once with no dispatchable items so it
   can provision canonical options, then repeat this cleanup before moving any
   issue to `Todo`. Verify the selected cleanup set before editing items:

   ```sh
   LEGACY_STATUS_REGEX='^(Ready|In progress|In review|Done)$'
   jq -r --arg re "$LEGACY_STATUS_REGEX" \
     '.items[] | select((.status // "No status") | test($re)) | [.id, .status, .content.url] | @tsv' \
     "$ONBOARDING_DIR/repo-open-project-items.before-detent.json"
   ```

   After the operator confirms the cleanup set, move those open items to
   `Backlog`:

   ```sh
   rg '^GITHUB_MODE=project_v2$' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   STATUS_FIELD_ID="$(gh project field-list "$PROJECT_NUMBER" --owner <project-owner> --format json --jq '.fields[] | select(.name == "Status") | .id')"
   BACKLOG_OPTION_ID="$(gh project field-list "$PROJECT_NUMBER" --owner <project-owner> --format json --jq '.fields[] | select(.name == "Status") | .options[] | select(.name == "Backlog") | .id')"
   jq -r --arg re "$LEGACY_STATUS_REGEX" \
     '.items[] | select((.status // "No status") | test($re)) | .id' \
     "$ONBOARDING_DIR/repo-open-project-items.before-detent.json" |
   while IFS= read -r item_id; do
     gh project item-edit \
       --id "$item_id" \
       --project-id "$PROJECT_NODE_ID" \
       --field-id "$STATUS_FIELD_ID" \
       --single-select-option-id "$BACKLOG_OPTION_ID" >/dev/null
   done
   ```

7. **Verify the boardless issue field when selected.** Run this only when
   `GITHUB_MODE=issue_field`. Detent needs an organization-level single-select
   issue field and options matching the configured workflow states. Issue
   fields are issue-only, so linked PR cards will use their linked issue's
   status. Verify:

   ```sh
   STATUS_FIELD_NAME="<status-field-name>"
   gh api /orgs/<repo-owner>/issue-fields > "$ONBOARDING_DIR/issue-fields.json"
   jq --arg name "$STATUS_FIELD_NAME" -e '.[] | select(.name == $name)' \
     "$ONBOARDING_DIR/issue-fields.json" > "$ONBOARDING_DIR/issue-field.json"
   jq -e '.data_type == "single_select"' "$ONBOARDING_DIR/issue-field.json"
   jq -r '.options[].name' "$ONBOARDING_DIR/issue-field.json" \
     > "$ONBOARDING_DIR/issue-field-status-options.txt"
   printf '%s\n' \
     Backlog Todo 'In Progress' Blocked 'Human Review' Rework Merging Done Cancelled \
     > "$ONBOARDING_DIR/detent-status-options.txt"
   sort "$ONBOARDING_DIR/detent-status-options.txt" > "$ONBOARDING_DIR/detent-status-options.sorted.txt"
   sort "$ONBOARDING_DIR/issue-field-status-options.txt" > "$ONBOARDING_DIR/issue-field-status-options.sorted.txt"
   comm -23 "$ONBOARDING_DIR/detent-status-options.sorted.txt" "$ONBOARDING_DIR/issue-field-status-options.sorted.txt"
   ```

   If the `comm` output is not empty, add the missing issue-field options in
   GitHub before starting Detent, or change the workflow states to match the
   existing options. Before any issue-field creation or option update, rerun:

   ```sh
   rg '^GITHUB_MODE=issue_field$' "$ONBOARDING_DIR/answers.env"
   rg '^STATUS_FIELD_NAME=' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"
   ```

   Do not create or link a GitHub ProjectV2 board for this mode.

8. **Verify repository status labels when selected.** Run this only when
   `GITHUB_MODE=label`. Detent needs one repository label for each effective
   configured workflow state. Apply `tracker.state_map` first, then slugify the
   result and prepend `tracker.status_label_prefix`. With the default prefix
   and release-flow `Cancelled: Done` mapping, the required labels are:

   ```text
   detent:backlog
   detent:todo
   detent:in-progress
   detent:blocked
   detent:human-review
   detent:rework
   detent:merging
   detent:done
   ```

   Create or verify the labels before the first `detent doctor` run so doctor
   can prove readiness instead of reporting missing label mappings. Detent's
   default `tracker.auto_provision: true` creates the same missing labels on
   project start, so the first start can mutate repository labels. Before
   creating labels manually or starting Detent with auto-provision enabled,
   rerun:

   ```sh
   rg '^GITHUB_MODE=label$' "$ONBOARDING_DIR/answers.env"
   rg '^STATUS_LABEL_PREFIX=' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"
   ```

   Verify:

   ```sh
   STATUS_LABEL_PREFIX="<status-label-prefix>"
   printf '%s\n' \
     "${STATUS_LABEL_PREFIX}backlog" "${STATUS_LABEL_PREFIX}todo" \
     "${STATUS_LABEL_PREFIX}in-progress" "${STATUS_LABEL_PREFIX}blocked" \
     "${STATUS_LABEL_PREFIX}human-review" "${STATUS_LABEL_PREFIX}rework" \
     "${STATUS_LABEL_PREFIX}merging" "${STATUS_LABEL_PREFIX}done" \
     > "$ONBOARDING_DIR/detent-status-labels.required.txt"
   gh api repos/<repo-owner>/<repo-name>/labels --paginate --jq '.[].name' \
     > "$ONBOARDING_DIR/repo-labels.txt"
   sort "$ONBOARDING_DIR/detent-status-labels.required.txt" \
     > "$ONBOARDING_DIR/detent-status-labels.required.sorted.txt"
   sort "$ONBOARDING_DIR/repo-labels.txt" \
     > "$ONBOARDING_DIR/repo-labels.sorted.txt"
   comm -23 "$ONBOARDING_DIR/detent-status-labels.required.sorted.txt" \
     "$ONBOARDING_DIR/repo-labels.sorted.txt"
   ```

   If `comm` prints missing labels and you are using the default prefix, create
   them before starting Detent:

   ```sh
   gh label create detent:backlog --repo <repo-owner>/<repo-name> \
     --color cfd3d7 --description "Not ready for Detent dispatch."
   gh label create detent:todo --repo <repo-owner>/<repo-name> \
     --color cfd3d7 --description "Ready for Detent dispatch."
   gh label create detent:in-progress --repo <repo-owner>/<repo-name> \
     --color fbca04 --description "Work is currently active."
   gh label create detent:blocked --repo <repo-owner>/<repo-name> \
     --color d73a4a --description "Cannot continue without human input."
   gh label create detent:human-review --repo <repo-owner>/<repo-name> \
     --color 6f42c1 --description "Waiting for human review."
   gh label create detent:rework --repo <repo-owner>/<repo-name> \
     --color d93f0b --description "Changes are requested before review can continue."
   gh label create detent:merging --repo <repo-owner>/<repo-name> \
     --color 6f42c1 --description "Approved work is being integrated."
   gh label create detent:done --repo <repo-owner>/<repo-name> \
     --color 0e8a16 --description "Work is complete."
   ```

   For a custom `STATUS_LABEL_PREFIX` or custom workflow states, generate the
   required label list from the actual `WORKFLOW.md` state names instead of
   copying the defaults above. Do not create or link a GitHub ProjectV2 board
   or organization issue field for this mode.

## Phase 4 — Author WORKFLOW.md

Before writing, overwriting, or editing `<source-root>/WORKFLOW.md`, rerun:

```sh
test -f "$ONBOARDING_DIR/answers.env"
rg '^GITHUB_MODE=(project_v2|issue_field|label)$' "$ONBOARDING_DIR/answers.env"
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"
```

1. **Fetch the selected mode template.** Read the existing file first if one is
   present; this runbook is for from-zero repositories, so do not overwrite a
   human-authored contract without explicit approval. The maintained templates
   are `docs/templates/WORKFLOW.project_v2.md`,
   `docs/templates/WORKFLOW.issue_field.md`, and
   `docs/templates/WORKFLOW.label.md`. Verify:

   ```sh
   test ! -f <source-root>/WORKFLOW.md
   GITHUB_MODE="$(sed -n 's/^GITHUB_MODE=//p' "$ONBOARDING_DIR/answers.env" | tail -n 1)"
   case "$GITHUB_MODE" in
     project_v2|issue_field|label) ;;
     *) printf 'invalid GITHUB_MODE: %s\n' "$GITHUB_MODE" >&2; exit 1 ;;
   esac
   curl -fsSL "https://raw.githubusercontent.com/digitaldrywood/detent/main/docs/templates/WORKFLOW.${GITHUB_MODE}.md" \
     -o <source-root>/WORKFLOW.md
   rg -n "github_status_source: ${GITHUB_MODE}|^tracker:|^workspace:|^agent:" <source-root>/WORKFLOW.md
   ```

2. **Substitute the tracker and workspace answers.** In ProjectV2 mode, use the
   ProjectV2 node id as `tracker.project_slug`. In boardless issue-field mode,
   set the repository and issue field. In label mode, set the repository and
   status-label prefix. In every mode, set `write_probe_issue` when using write
   probes, use absolute paths for `workspace.source_root` and `workspace.root`,
   and leave `tracker.api_key` out unless this workflow intentionally carries a
   workflow-local token instead of using `github_token: gh` from `global.yaml`.
   Verify the selected tracker block:

   ProjectV2 tracker snippet:

   ```yaml
   tracker:
     kind: github
     github_status_source: project_v2
     project_slug: <project-node-id>
     write_probe_issue: <write-probe-issue>
   ```

   Boardless issue-field tracker snippet:

   ```yaml
   tracker:
     kind: github
     github_status_source: issue_field
     repository: <repo-owner>/<repo-name>
     status_field: <status-field-name>
     write_probe_issue: <write-probe-issue>
   ```

   Repository label tracker snippet:

   ```yaml
   tracker:
     kind: github
     github_status_source: label
     repository: <repo-owner>/<repo-name>
     status_label_prefix: "<status-label-prefix>"
     write_probe_issue: <write-probe-issue>
   ```

   ```sh
   # ProjectV2 mode:
   PROJECT_NODE_ID="$(jq -r '.id' "$ONBOARDING_DIR/project.json")"
   rg -n 'github_status_source: project_v2|project_slug: <project-node-id>|write_probe_issue:' <source-root>/WORKFLOW.md

   # Boardless issue-field mode:
   rg -n 'github_status_source: issue_field|repository: <repo-owner>/<repo-name>|status_field: <status-field-name>|write_probe_issue:' <source-root>/WORKFLOW.md

   # Label mode:
   rg -n 'github_status_source: label|repository: <repo-owner>/<repo-name>|status_label_prefix: "<status-label-prefix>"|write_probe_issue:' <source-root>/WORKFLOW.md

   # All modes:
   perl -0pi -e 's#(?m)^  source_root: .*$#  source_root: <source-root>#' <source-root>/WORKFLOW.md
   perl -0pi -e 's#(?m)^  root: .*$#  root: <worktree-root>#' <source-root>/WORKFLOW.md
   rg -n 'source_root: <source-root>|root: <worktree-root>' <source-root>/WORKFLOW.md
   ```

3. **Set Kanban interaction mode when supported.** Do not add this block to
   current releases unless the Detent binary supports Kanban interaction
   configuration. Maintained templates set project boards to `integration` for
   a trusted operator-owned local or private Detent instance. Keep fleet
   `/kanban` read-only; this setting only controls `/projects/<id>/kanban`.
   For a shared observer dashboard, or when write probes are not configured or
   not passing, set `read_only` until `detent doctor` proves status write and
   issue/PR comment write. Verify:

   ```yaml
   server:
     kanban:
       mode: integration
       # Use mode: read_only for observer/shared dashboards or until write probes pass.
       # Optional allowed_transitions expose broader manual status editing.
       # allowed_transitions:
       #   In Progress: [Blocked, Cancelled]
       #   Rework: [Blocked, Cancelled]
       #   Merging: [Blocked, Cancelled]
   ```

   ```sh
   rg -n 'kanban:|mode: read_only|mode: integration|allowed_transitions' <source-root>/WORKFLOW.md || true
   ```

4. **Set the dashboard bind from the interview.** This writes the default
   `server.host` used when Detent starts without an explicit `--host`. Service
   managers can still override it in `ExecStart` with the same selected host.
   Verify:

   ```sh
   perl -0pi -e 's#(?m)^  host: .*$#  host: <dashboard-host>#' <source-root>/WORKFLOW.md
   rg -n '^server:|host: <dashboard-host>|port:' <source-root>/WORKFLOW.md
   ```

5. **Set the gate from the interview.** For command gates, include the command,
   whether an automated GitHub PR review is required for auto-promotion, and
   whether failed current-head CI parks in `Human Review` or routes to
   `Rework`. For human gates, include the approval label. Verify:

   ```sh
   rg -n '^gate:|kind: <command|human_review>|run: <gate-command>|require_automated_review: <true|false>|ci_failure_action: <skip|rework>|approval_label: <label>' \
     <source-root>/WORKFLOW.md
   ```

   Command gate shape:

   ```yaml
   gate:
     kind: command
     run: <gate-command>
     require_automated_review: <true|false>
     ci_failure_action: <skip|rework>
   ```

   Human review gate shape:

   ```yaml
   gate:
     kind: human_review
     approval_label: <approval-label>
   ```

6. **Set dispatch ordering, review policy, dependency waiting policy, and
   concurrency.** Keep `Merging: 1`. Use the dispatch label ordering, review
   policy, and dependency policy selected by the human. Verify:

   ```sh
   rg -n 'max_concurrent_agents: <max>|Merging: 1|dispatch_priority_by_label:|auto_promote:|dependency_auto_unblock:|enabled: <true|false>|quiet_seconds: <seconds>|optout_label: <label>|allowed_issue_labels:|source_states:|target_state:|readiness:' \
     <source-root>/WORKFLOW.md
   ```

   Label tie-breaker shape:

   ```yaml
   agent:
     dispatch_priority_by_label:
       - <highest-ranked-label>
       - <next-ranked-label>
   ```

   Empty list shape:

   ```yaml
   agent:
     dispatch_priority_by_label: []
   ```

   Hard-stop review policy:

   ```yaml
   agent:
     auto_promote:
       enabled: false
       quiet_seconds: 600
       optout_label: requires-human-review
       allowed_issue_labels: []
   ```

   Criteria-based auto-promote example:

   ```yaml
   agent:
     auto_promote:
       enabled: true
       quiet_seconds: <seconds>
       optout_label: <optout-label>
       allowed_issue_labels:
         - <allowed-label>
   ```

   For a command gate, auto-promote requires a linked open PR, green CI, no P1
   automated PR review findings, and the configured quiet period. With
   `require_automated_review: true`, it also requires a current-head automated
   GitHub PR review. With `require_automated_review: false`, bot PR review is
   not required to exist, but any observed P1 bot findings still route the item
   to `Rework`. With `ci_failure_action: rework`, failed or cancelled
   current-head CI also routes the item to `Rework`; the default `skip` leaves
   non-green CI parked in `Human Review`. Pending CI stays parked. The quiet
   period resets on observed issue updates, Project
   status updates, automated PR review submission, and linked PR activity such
   as a fresh push to the PR head. `detent doctor --port 0` reports sampled
   `Human Review` candidates and reasons such as `automated_review_missing`
   when that gate is not met.

   Dependency auto-unblock default:

   ```yaml
   tracker:
     dependency_auto_unblock:
       enabled: false
       source_states:
         - Blocked
       target_state: Todo
       readiness: terminal_or_merged
   ```

   Enable it only for projects that use `Blocked` as a dependency-waiting state
   with explicit machine-readable dependency references. Without this enabled,
   `Blocked` is an observed/display state and dependency completion will not
   move issues back to `Todo`.

7. **Write the prompt body.** Keep the `## Codex Workpad` instruction, include
   repo authority files discovered in Phase 2, and state the validation gate.
   Verify:

   ```sh
   awk 'seen {print} /^---$/ {count++; if (count == 2) seen=1}' <source-root>/WORKFLOW.md \
     | rg 'Codex Workpad|CLAUDE.md|AGENTS.md|CONTRIBUTING.md|<gate-command>|<repo-owner>/<repo-name>'
   ```

8. **Check the workflow contract before registration.** This is a structural
   check; `detent doctor` in Phase 5 is the full preflight. Verify:

   ```sh
   rg -n '^tracker:|project_slug:|^workspace:|source_root:|^agent:|max_concurrent_agents_by_state:|^gate:' \
     <source-root>/WORKFLOW.md
   ```

## Phase 5 — Register The Project

Before running `detent init`, `detent add-project`, mutating `global.yaml`, or
running `detent doctor` with configured write probes, rerun:

```sh
test -f "$ONBOARDING_DIR/answers.env"
rg '^GITHUB_MODE=(project_v2|issue_field|label)$' "$ONBOARDING_DIR/answers.env"
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"
```

1. **Create global config only when needed.** Use the resolved path Detent
   reports. For `add-project`, read and preserve the existing config; do not
   reinitialize or overwrite runtime keys unless the human selected that change.
   Verify:

   ```sh
   detent --format pretty config path
   GLOBAL_CONFIG="$(
     detent --format pretty config path | awk '/^path:/ {print $2}'
   )"
   if test -f "$GLOBAL_CONFIG"; then
     sed -n '1,240p' "$GLOBAL_CONFIG" \
       > "$ONBOARDING_DIR/global-config.before-register.txt"
   else
     detent init
   fi
   detent --format pretty config path
   ```

2. **Register the project.** Skip this if the project is already registered and
   the human chose to repair or update the existing entry. `priority` and
   `weight` are the scheduling answers from Phase 2. Verify:

   ```sh
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   GLOBAL_CONFIG="$(
     detent --format pretty config path | awk '/^path:/ {print $2}'
   )"
   PROJECT_ENTRY_PATTERN='id: <detent-project-id>|workflow: <source-root>/WORKFLOW.md|workdir: <source-root>'
   if rg -n "$PROJECT_ENTRY_PATTERN" "$GLOBAL_CONFIG"; then
     printf 'Project already registered; confirm before editing\n'
   else
     detent add-project \
       --id <detent-project-id> \
       --workflow <source-root>/WORKFLOW.md \
       --workdir <source-root> \
       --weight <global-weight> \
       --priority <global-priority>
   fi
   GLOBAL_CONFIG="$(
     detent --format pretty config path | awk '/^path:/ {print $2}'
   )"
   PROJECT_ENTRY_PATTERN='id: <detent-project-id>|workflow: <source-root>/WORKFLOW.md|workdir: <source-root>|weight: <global-weight>|priority: <global-priority>'
   rg -n "$PROJECT_ENTRY_PATTERN" "$GLOBAL_CONFIG"
   ```

3. **Set or preserve runtime keys in `global.yaml`.** For local onboarding,
   prefer `github_token: gh` so Detent resolves the token from `gh auth token`
   at startup. In ProjectV2 mode this shares the operator's GraphQL budget with
   Detent and spawned agents; in issue-field mode it still shares REST
   issue-field and comment write limits; in label mode it shares REST label,
   issue, and comment write limits. For production or high-volume projects,
   prefer
   GitHub App installation authentication in `WORKFLOW.md`. Set `instance_name`
   to the interview answer or the short hostname on new installs. For existing
   installs, preserve
   `env`, `log_level`, `github_token`, `port`, and `instance_name` unless the
   human selected different values. Use a non-4000 port if another Detent
   instance is already running; keep the dashboard host in `WORKFLOW.md`
   `server.host` or pass it with `--host`. Verify:

   ```sh
   GLOBAL_CONFIG="$(
     detent --format pretty config path | awk '/^path:/ {print $2}'
   )"
   sed -n '1,240p' "$GLOBAL_CONFIG"
   # Run this edit only for new installs or confirmed runtime-key changes.
   GLOBAL_RUNTIME_INSERT='
   if (!/^github_token:/m) {
     my $runtime = "env: prod\nlog_level: info\ngithub_token: gh\n";
     $runtime .= "port: <port>\ninstance_name: <instance-name>\n";
     s/^(global:)/${runtime}$1/m;
   }
   '
   perl -0pi -e "$GLOBAL_RUNTIME_INSERT" "$GLOBAL_CONFIG"
   rg -n '^(env|log_level|github_token|port|instance_name):' "$GLOBAL_CONFIG"
   ```

   Required shape:

   ```yaml
   env: prod
   log_level: info
   github_token: gh
   port: <port>
   instance_name: <instance-name>
   ```

   GitHub App installation auth is configured in `WORKFLOW.md` with
   `github_app_id`, `github_app_installation_id`, and either
   `github_app_private_key` or `github_app_private_key_path`.

4. **Run preflight until every check passes.** Fix every `FAIL`; do not
   dispatch work from a failed doctor run. In ProjectV2 mode, confirm doctor
   reports project access, Status option discovery, repository issue/PR access,
   write probes when configured, and rate-limit visibility. In issue-field mode,
   confirm repository access, issue field discovery, option discovery, issue
   reads by field value, optional issue-field write probe, issue/PR comment
   write when integration-capable features are configured, and rate-limit
   visibility. In label mode, confirm repository access, status label mappings,
   issue reads by configured status labels, optional status-label write probe,
   issue/PR comment write when integration-capable features are configured, and
   rate-limit visibility. A label-mode write probe only runs when
   `tracker.write_probe_issue` points to a scratch issue that already has one
   configured status label; doctor reapplies that same label to prove write
   permission without changing the issue's state. Verify:

   ```sh
   detent doctor
   ```

5. **Verify the systemd service PATH when Detent runs as a user service.**
   User services do not inherit the interactive shell PATH. `detent doctor`
   verifies Detent's direct dependencies, but a first dispatch can still fail
   if repo hooks or validation gates call tools from project-local, language
   manager, or user binary directories that are missing from the service
   environment. Copy the exact `Environment=PATH=...` value from
   `detent.service`, then verify Detent tools and every command used by
   `hooks.*` and the selected gate from that same service context. Replace the
   placeholder tools below with the actual binaries required by the target repo,
   such as `mix`, `bundle`, `ruby`, `node`, `pnpm`, `python`, `cargo`, `composer`,
   `mvn`, `gradle`, `dotnet`, or a static-site generator:

   ```sh
   systemd-run --user --wait --collect --pipe \
     --property=Environment=PATH=<same-path-as-detent.service> \
     /usr/bin/bash -lc '
       for tool in gh codex git detent <gate-tool> <hook-tool>; do
         command -v "$tool"
       done
     '
   ```

   Add every missing tool directory to the service PATH before dispatching. Use
   the directories required by the target repo's language manager and selected
   validation gate. For example:

   ```ini
   Environment=PATH=/home/<user>/.local/bin:/home/<user>/.asdf/shims:/home/<user>/.cargo/bin:/usr/local/bin:/usr/bin:/bin
   ```

   When the project defines `hooks.after_create` or other bootstrap hooks,
   dry-run that hook or the equivalent repo bootstrap script from an isolated
   throwaway worktree with the same service PATH before moving an issue to
   `Todo`.

## Phase 6 — Issue Intake

Before adding issues to a ProjectV2 board, setting issue-field values, changing
status labels, or enabling ProjectV2 auto-add, rerun:

```sh
test -f "$ONBOARDING_DIR/answers.env"
rg '^GITHUB_MODE=(project_v2|issue_field|label)$' "$ONBOARDING_DIR/answers.env"
rg '^INTAKE_GH_FLAGS=' "$ONBOARDING_DIR/answers.env"
rg '^INITIAL_STATUS=' "$ONBOARDING_DIR/answers.env"
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"
```

1. **Confirm the selected initial status option exists.** In ProjectV2 mode, if
   this fails on a fresh board, start Detent once with no dispatchable items so
   it can auto-provision missing options, then repeat this verification.
   In label mode, verify the initial status label exists before assigning it.
   Verify:

   ```sh
   # ProjectV2 mode:
   gh project field-list <project-number> --owner <project-owner> --format json \
     --jq '.fields[] | select(.name == "Status") | .options[].name' | rg -x '<initial-status>'

   # Boardless issue-field mode:
   jq -r '.options[].name' "$ONBOARDING_DIR/issue-field.json" | rg -x '<initial-status>'

   # Label mode:
   rg -x '<status-label-prefix><initial-status-slug>' \
     "$ONBOARDING_DIR/repo-labels.txt"
   ```

2. **ProjectV2 intake: bulk-add issues by the selected filter and set initial
   `Status`.** Run only when `GITHUB_MODE=project_v2`. Use the exact
   `gh issue list` flags from the intake answer. Use `Backlog` for broad intake
   and `Todo` only for work that should dispatch immediately. This verifies the
   write `project` scope if no earlier board creation, linking, field creation,
   or item edit has already done so.
   `gh project item-add` and `gh project item-edit` are GraphQL mutations, so
   do not start a broad intake when the budget warning from Phase 1 is still
   unresolved. Verify with one cached inventory after the mutations finish:

   ```sh
   rg '^GITHUB_MODE=project_v2$' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   PROJECT_NODE_ID="$(gh project view <project-number> --owner <project-owner> --format json --jq '.id')"
   gh project field-list <project-number> --owner <project-owner> --format json \
     > "$ONBOARDING_DIR/project-fields.intake.json"
   STATUS_FIELD_ID="$(jq -r '.fields[] | select(.name == "Status") | .id' "$ONBOARDING_DIR/project-fields.intake.json")"
   STATUS_OPTION_ID="$(jq -r '.fields[] | select(.name == "Status") | .options[] | select(.name == "<initial-status>") | .id' "$ONBOARDING_DIR/project-fields.intake.json")"

   gh issue list --repo <repo-owner>/<repo-name> --state open <chosen-gh-issue-list-flags> \
     --limit 1000 --json url --jq '.[].url' |
   while IFS= read -r issue_url; do
     item_id="$(gh project item-add <project-number> --owner <project-owner> --url "$issue_url" --format json --jq '.id')"
     gh project item-edit \
       --id "$item_id" \
       --project-id "$PROJECT_NODE_ID" \
       --field-id "$STATUS_FIELD_ID" \
       --single-select-option-id "$STATUS_OPTION_ID" >/dev/null
   done

   gh project item-list <project-number> --owner <project-owner> --format json --limit 1000 \
     > "$ONBOARDING_DIR/project-items.after-intake.json"
   jq '[.items[] | select(.content.repository == "<repo-owner>/<repo-name>" and .status == "<initial-status>")] | length' \
     "$ONBOARDING_DIR/project-items.after-intake.json"
   ```

3. **Issue-field intake: set initial issue-field Status on selected issues.**
   Run only when `GITHUB_MODE=issue_field`. Use `Backlog` for broad intake and
   `Todo` only for work that should dispatch immediately. Issue-field writes
   can trigger notifications and secondary rate limits, so keep broad edits
   deliberate and use `detent doctor` to prove the write probe first. Verify:

   ```sh
   rg '^GITHUB_MODE=issue_field$' "$ONBOARDING_DIR/answers.env"
   rg '^STATUS_FIELD_NAME=' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   STATUS_FIELD_ID="$(jq -r '.id' "$ONBOARDING_DIR/issue-field.json")"
   gh issue list --repo <repo-owner>/<repo-name> --state open <chosen-gh-issue-list-flags> \
     --limit 1000 --json number --jq '.[].number' |
   while IFS= read -r issue_number; do
     jq -n --argjson field_id "$STATUS_FIELD_ID" --arg value "<initial-status>" \
       '{issue_field_values: [{field_id: $field_id, value: $value}]}' |
     gh api --method POST \
       "/repos/<repo-owner>/<repo-name>/issues/${issue_number}/issue-field-values" \
       --input - >/dev/null
   done

   gh issue list --repo <repo-owner>/<repo-name> --state open <chosen-gh-issue-list-flags> \
     --limit 1000 --json number,url > "$ONBOARDING_DIR/issues.after-intake.json"
   jq '. | length' "$ONBOARDING_DIR/issues.after-intake.json"
   ```

4. **Label intake: set the initial status label on selected issues.** Run only
   when `GITHUB_MODE=label`. Use `Backlog` for broad intake and `Todo` only for
   work that should dispatch immediately. Each issue should have exactly one
   configured status label with the selected prefix. Preserve ordinary labels
   such as `documentation`, `bug`, or `enhancement`; remove only labels that
   start with `STATUS_LABEL_PREFIX`. Verify:

   ```sh
   rg '^GITHUB_MODE=label$' "$ONBOARDING_DIR/answers.env"
   rg '^STATUS_LABEL_PREFIX=' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   STATUS_LABEL_PREFIX="<status-label-prefix>"
   STATUS_LABEL="${STATUS_LABEL_PREFIX}<initial-status-slug>"
   gh issue list --repo <repo-owner>/<repo-name> --state open <chosen-gh-issue-list-flags> \
     --limit 1000 --json number --jq '.[].number' |
   while IFS= read -r issue_number; do
     gh api "repos/<repo-owner>/<repo-name>/issues/${issue_number}/labels" \
       --jq '.[].name' |
     while IFS= read -r label; do
       case "$label" in
         "$STATUS_LABEL_PREFIX"*)
           encoded_label="$(jq -rn --arg value "$label" '$value | @uri')"
           gh api --method DELETE \
             "/repos/<repo-owner>/<repo-name>/issues/${issue_number}/labels/${encoded_label}" \
             --silent
           ;;
       esac
     done
     gh api --method POST \
       "/repos/<repo-owner>/<repo-name>/issues/${issue_number}/labels" \
       -f "labels[]=$STATUS_LABEL" \
       --silent
   done

   gh issue list --repo <repo-owner>/<repo-name> --state open \
     --label "$STATUS_LABEL" <chosen-gh-issue-list-flags> \
     --limit 1000 --json number,url > "$ONBOARDING_DIR/issues.after-intake.json"
   jq '. | length' "$ONBOARDING_DIR/issues.after-intake.json"
   ```

5. **Optionally enable ProjectV2 auto-add.** Run only when
   `GITHUB_MODE=project_v2`. This is a **human UI step** because GitHub's
   built-in ProjectV2 auto-add workflows are not configurable through the API.
   Click: Project -> ... -> Workflows -> Auto-add to project. Configure the same
   repo and filter chosen in the interview. Verify:

   ```sh
   rg '^GITHUB_MODE=project_v2$' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   gh project item-list <project-number> --owner <project-owner> \
     --query 'repo:<repo-owner>/<repo-name> is:issue is:open' \
     --format json > "$ONBOARDING_DIR/project-items.auto-add.json"
   jq '.totalCount' "$ONBOARDING_DIR/project-items.auto-add.json"
   ```

## Phase 7 — Smoke Test

Before starting Detent, restarting a service, hot-reloading a running process,
or moving a smoke-test issue to `Todo`, rerun:

```sh
test -f "$ONBOARDING_DIR/answers.env"
rg '^GITHUB_MODE=(project_v2|issue_field|label)$' "$ONBOARDING_DIR/answers.env"
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"
```

1. **Start Detent or hot-reload the running process.** Use the configured port,
   not `4000` when another Detent instance owns that port. Use the dashboard
   host chosen in Phase 2: `127.0.0.1` for SSH tunnels, a private/Tailscale IP
   for VPN-only exposure, or `0.0.0.0` only on trusted private networks because
   it exposes every interface. Verify:

   ```sh
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   detent --host <dashboard-host> --port <port>
   ```

   For a user-level systemd service, use the same bind choice in `ExecStart`,
   then restart the user service:

   ```ini
   ExecStart=/home/<user>/.local/bin/detent --headless --host <dashboard-host> --port <port>
   ```

   In another shell, verify the listener and the API URL that should work from
   that shell. Use `127.0.0.1` for localhost-only binds, the selected
   private/Tailscale IP for VPN-only binds, or `127.0.0.1` for same-host
   checks when binding `0.0.0.0`:

   ```sh
   ss -ltnp | rg ':<port>|detent'
   curl -fsS http://<dashboard-check-host>:<port>/health | jq -e '.status == "ok" and .mode == "running"'
   curl -fsS http://<dashboard-check-host>:<port>/api/v1/state
   ```

   If the chosen host is a private/Tailscale IP or `0.0.0.0`, verify the remote
   URL from another machine on that network:

   ```sh
   curl -fsS http://<tailscale-or-private-ip>:<port>/api/v1/state
   ```

2. **Check rate-limit budget before smoke dispatch.** In ProjectV2 mode, this
   is the last stop before Detent and the spawned agent start spending GraphQL
   budget for polling, workpad, status, PR, and review work. In boardless
   issue-field and label modes, review REST core and GraphQL visibility from
   `detent doctor`; issue-field polling, status-label polling, and status writes
   are REST-backed, while PR relationship checks can still use GraphQL. Verify:

   ```sh
   # ProjectV2 mode:
   gh api rate_limit --jq '.resources.graphql | {limit, used, remaining, reset}' \
     > "$ONBOARDING_DIR/graphql-rate-limit.before-smoke.json"
   jq -r '"graphql remaining=\(.remaining) reset=\(.reset)"' \
     "$ONBOARDING_DIR/graphql-rate-limit.before-smoke.json"
   jq -e '.remaining >= 500' "$ONBOARDING_DIR/graphql-rate-limit.before-smoke.json" \
     || printf 'WARNING: low GitHub GraphQL budget; defer smoke dispatch or use GitHub App auth\n'

   # Boardless issue-field mode:
   detent doctor --port 0 | rg 'GitHub API rate limit|GitHub issue field'

   # Label mode:
   detent doctor --port 0 | rg 'GitHub API rate limit|GitHub status label'
   ```

3. **Move one real issue to `Todo`.** Use a real issue that matches the
   authorization filters. Do not verify this by polling ProjectV2 after the
   edit; switch to the local Detent API in the next step. Verify:

   ```sh
   # ProjectV2 mode:
   rg '^GITHUB_MODE=project_v2$' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   gh project field-list <project-number> --owner <project-owner> --format json \
     > "$ONBOARDING_DIR/project-fields.smoke.json"
   TODO_OPTION_ID="$(jq -r '.fields[] | select(.name == "Status") | .options[] | select(.name == "Todo") | .id' "$ONBOARDING_DIR/project-fields.smoke.json")"
   STATUS_FIELD_ID="$(jq -r '.fields[] | select(.name == "Status") | .id' "$ONBOARDING_DIR/project-fields.smoke.json")"
   PROJECT_NODE_ID="$(gh project view <project-number> --owner <project-owner> --format json --jq '.id')"
   ITEMS_JSON="$ONBOARDING_DIR/project-items.after-intake.json"
   if ! test -f "$ITEMS_JSON"; then
     gh project item-list <project-number> --owner <project-owner> --format json --limit 1000 \
       > "$ONBOARDING_DIR/project-items.smoke.json"
     ITEMS_JSON="$ONBOARDING_DIR/project-items.smoke.json"
   fi
   ITEM_ID="$(jq -er '[.items[] | select(.content.url == "https://github.com/<repo-owner>/<repo-name>/issues/<issue-number>") | .id][0]' "$ITEMS_JSON")"
   gh project item-edit \
     --id "$ITEM_ID" \
     --project-id "$PROJECT_NODE_ID" \
     --field-id "$STATUS_FIELD_ID" \
     --single-select-option-id "$TODO_OPTION_ID"

   # Boardless issue-field mode:
   rg '^GITHUB_MODE=issue_field$' "$ONBOARDING_DIR/answers.env"
   rg '^STATUS_FIELD_NAME=' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   STATUS_FIELD_ID="$(jq -r '.id' "$ONBOARDING_DIR/issue-field.json")"
   jq -n --argjson field_id "$STATUS_FIELD_ID" --arg value "Todo" \
     '{issue_field_values: [{field_id: $field_id, value: $value}]}' |
   gh api --method POST \
     "/repos/<repo-owner>/<repo-name>/issues/<issue-number>/issue-field-values" \
     --input -

   # Label mode:
   rg '^GITHUB_MODE=label$' "$ONBOARDING_DIR/answers.env"
   rg '^STATUS_LABEL_PREFIX=' "$ONBOARDING_DIR/answers.env"
   detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
   rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
   awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"

   STATUS_LABEL_PREFIX="<status-label-prefix>"
   TODO_STATUS_LABEL="${STATUS_LABEL_PREFIX}todo"
   gh api repos/<repo-owner>/<repo-name>/issues/<issue-number>/labels \
     --jq '.[].name' |
   while IFS= read -r label; do
     case "$label" in
       "$STATUS_LABEL_PREFIX"*)
         encoded_label="$(jq -rn --arg value "$label" '$value | @uri')"
         gh api --method DELETE \
           "/repos/<repo-owner>/<repo-name>/issues/<issue-number>/labels/${encoded_label}" \
           --silent
         ;;
     esac
   done
   gh api --method POST \
     "/repos/<repo-owner>/<repo-name>/issues/<issue-number>/labels" \
     -f "labels[]=$TODO_STATUS_LABEL"
   ```

4. **Verify the issue dispatches locally.** Onboarding is not complete until
   the issue appears under Running on the dashboard. Once it does, stop
   operator ProjectV2, issue-field, or label polling; the spawned agent owns
   GitHub work from here. Verify:

   ```sh
   curl -fsS http://127.0.0.1:<port>/api/v1/state \
     | jq -e '.running[] | select(.identifier == "<repo-owner>/<repo-name>#<issue-number>")'
   ```

5. **Verify Detent posted the workpad.** The issue must have a persistent
   `## Codex Workpad` comment. Verify:

   ```sh
   gh api repos/<repo-owner>/<repo-name>/issues/<issue-number>/comments --paginate \
     --jq '.[] | select(.body | startswith("## Codex Workpad")) | .html_url' | rg .
   ```

## Reconfiguration Closeout

Use this checklist after editing an existing Detent setup, especially after
changing `global.yaml` or `WORKFLOW.md`.

Before any closeout command that edits config, restarts a service, reruns
write-probe preflight, or moves issues, rerun:

```sh
test -f "$ONBOARDING_DIR/answers.env"
rg '^GITHUB_MODE=(project_v2|issue_field|label)$' "$ONBOARDING_DIR/answers.env"
detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase mutation
rg '^MUTATION_CONFIRMED=true$' "$ONBOARDING_DIR/answers.env"
awk 'NF {last=$0} END {exit last == "MUTATION_CONFIRMED=true" ? 0 : 1}' "$ONBOARDING_DIR/answers.env"
```

1. **Verify the binary you are about to run.** After pulling, rebuilding, or
   installing updates, confirm the expected version, commit, and build date:

   ```sh
   detent version
   ```

2. **Run the preflight again.** `detent doctor` should pass before starting a
   stopped instance because Phase 5 expects the configured server port to be
   free:

   ```sh
   detent doctor
   ```

   If Detent is already running on the configured port, the server-port check
   can fail because the live service owns that port. In that case, rerun the
   config, toolchain, token, and database preflight with an ephemeral port, then
   verify the live service separately:

   ```sh
   detent doctor --port 0
   curl -fsS http://<dashboard-check-host>:<port>/health | jq -e '.status == "ok" and .mode == "running"'
   ```

3. **Reload the runtime that needs the change.** Detent watches the active
   `global.yaml`, including symlinked config targets, and applies this reload
   matrix:

   | Field | Reload behavior |
   | --- | --- |
   | Project list and project settings | Live reload |
   | Credentials: `github_token` and project credentials | Live reload |
   | `global.startup` | Live reload |
   | `instance_name` | Live reload |
   | `global.identity` | Live reload; project runtimes restart in-process and `/api/v1/state.instance.name` updates after the next telemetry snapshot |
   | `global.max_concurrent_agents`, `global.scheduling`, `global.fair_share` | Restart required |
   | `port`, `env`, `log_level` | Restart required |

   When a changed field requires restart, Detent logs
   `global config setting change requires restart` with the field name.

4. **Hold dispatch on failed preflight.** Do not move new work to `Todo` from a
   failed `detent doctor` run unless the only failure is the expected live-port
   collision, `detent doctor --port 0` passes, and `/health` is green for the
   running service.

## Appendix

### ProjectV2 And Boardless Migration Notes

Existing ProjectV2 workflows remain valid. Leaving
`tracker.github_status_source` unset keeps `project_v2` as the default
compatibility path, and `tracker.project_slug` remains required only for that
mode. Choose this path when humans still use the GitHub Project board as the
source of truth for planning, ranking, and status changes.

Switch to boardless issue-field mode only after the repository has an
organization issue `Status` field with options matching the Detent workflow.
Change `WORKFLOW.md` to:

```yaml
tracker:
  kind: github
  github_status_source: issue_field
  repository: <repo-owner>/<repo-name>
  status_field: <status-field-name>
```

Detent does not automatically migrate ProjectV2 item statuses into issue
fields. Copy existing statuses manually or with a one-off script outside
Detent, then run `detent doctor --port 0` and fix repository access, issue
field discovery, option discovery, write-probe, comment-write, and rate-limit
checks before dispatching. GitHub issue fields apply to issues only, so linked
PR cards derive status from the linked issue.

Switch to repository label mode only after the repository has status labels
matching the effective workflow states. Change `WORKFLOW.md` to:

```yaml
tracker:
  kind: github
  github_status_source: label
  repository: <repo-owner>/<repo-name>
  status_label_prefix: "<status-label-prefix>"
```

Detent does not automatically migrate ProjectV2 item statuses or issue-field
values into labels. With `tracker.auto_provision` enabled, Detent creates
missing prefixed status labels on startup, but it does not assign those labels
to existing issues. Copy existing statuses by applying exactly one configured
status label per issue, then run `detent doctor --port 0` and fix repository
access, status label mappings, issue reads by label, write-probe,
comment-write, and rate-limit checks before dispatching. GitHub status labels
apply to issues only, so linked PR cards derive status from the linked issue.

### Interview Answers To Config Keys

| Interview answer | Config or system target |
| --- | --- |
| GitHub status source | `tracker.github_status_source: project_v2`, `issue_field`, or `label` in `WORKFLOW.md`. |
| Board node id | `tracker.project_slug` in `WORKFLOW.md` for ProjectV2 mode only. |
| Boardless repository | `tracker.repository` in `WORKFLOW.md` for issue-field and label modes. |
| Boardless issue field | `tracker.status_field` in `WORKFLOW.md`; defaults to `Status` when omitted. |
| Status label prefix | `tracker.status_label_prefix` in `WORKFLOW.md`; defaults to `detent:` for label mode. |
| Kanban interaction | Fleet and observer boards stay read-only; trusted project boards use `integration` after doctor proves write permissions. |
| Project scheduling priority | `projects[].priority` in `global.yaml`. |
| Project scheduling weight | `projects[].weight` in `global.yaml`. |
| Project color | Optional `projects[].color` in `global.yaml`; missing colors are deterministic and appear in the sidebar and multi-project Kanban. |
| Dispatch label ordering | `agent.dispatch_priority_by_label` in `WORKFLOW.md`. |
| Authorization filters | `tracker.authorization` in `WORKFLOW.md`; optionally `projects[].authorization` in `global.yaml` for host-level scoping. |
| Dashboard bind | `server.host` in `WORKFLOW.md`, or `--host` in the startup command or service `ExecStart`. |
| Validation command | `gate.kind: command` and `gate.run` in `WORKFLOW.md`. |
| Automated PR review requirement | `gate.kind: command` and `gate.require_automated_review` in `WORKFLOW.md`. |
| Failed CI recovery | `gate.kind: command` and `gate.ci_failure_action` in `WORKFLOW.md`. |
| Human validation label | `gate.kind: human_review` and `gate.approval_label` in `WORKFLOW.md`. |
| Per-project concurrency | `agent.max_concurrent_agents` in `WORKFLOW.md`. |
| Merge serialization | `agent.max_concurrent_agents_by_state.Merging: 1` in `WORKFLOW.md`. |
| Hard-stop review policy | `agent.auto_promote.enabled: false` in `WORKFLOW.md`. |
| Criteria-based auto-promote | `agent.auto_promote.enabled`, `quiet_seconds`, `optout_label`, and `allowed_issue_labels` in `WORKFLOW.md`. |
| Prompt body | Markdown body after the closing frontmatter `---` in `WORKFLOW.md`. |
| Intake filter | `gh issue list` flags and optional GitHub Project auto-add workflow. |
| Initial issue status | ProjectV2 or issue-field `Status` value, or one status label in label mode, usually `Backlog` or `Todo`. |

### What A Good Detent Issue Looks Like

Use issues as executable contracts. A good issue has enough specificity that an
agent can finish without inventing product intent.

```markdown
## Problem

What is broken, missing, or valuable, and who cares?

## Scope

- Files, packages, screens, commands, or docs expected to change.
- Explicit non-goals so the agent does not expand the work.

## Acceptance Criteria

- [ ] Observable behavior or documentation outcome.
- [ ] Edge case or failure mode covered.
- [ ] README, docs, or examples updated when user-facing behavior changes.

## Validation

- Exact command the agent must run, such as `make check`, `mix test`,
  `bundle exec rspec`, `npm test`, or another repo-specific gate.
- Any focused tests or manual checks expected before the full gate.

## Dependencies

Depends on: #<issue-number>

State whether the dependency must be merged into `origin/main` before this
issue starts. If there is no dependency, omit the line.
```

Keep dependency order explicit. If issue B relies on issue A, issue B should
carry `Depends on: #A` and stay out of `Todo` until A has merged. Same-repo
`#A`, cross-repo `owner/repo#A`, and full
`https://github.com/owner/repo/issues/A` issue URLs are supported inside
`Depends on:` and `Blocked by:` lines.

Alternatively, if the project has opted into
`tracker.dependency_auto_unblock.enabled`, issue B can sit in a configured
waiting state such as `Blocked` with the same `Depends on:` or `Blocked by:`
line. Detent will move it to the configured ready state after every blocker is
terminal, closed, or merged according to the workflow readiness rule. Do not use
that mode for free-form human blockers without explicit dependency references.
If auto-unblock is disabled, a dependency-waiting issue in `Blocked` will remain
there even after the dependency clears.
