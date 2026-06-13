# Agent-Executable Project Onboarding

This runbook takes a target repository from zero Detent setup to one
dispatched issue. It assumes the human has already named the target repository
and that the agent will inspect before asking questions. Replace every
`<...>` placeholder before running a command.

Use these placeholders consistently:

| Placeholder | Meaning |
| --- | --- |
| `<repo-owner>` | GitHub owner of the target repository. |
| `<repo-name>` | GitHub repository name. |
| `<source-root>` | Local checkout of `<repo-owner>/<repo-name>`. |
| `<worktree-root>` | Directory where Detent will create issue worktrees. |
| `<project-owner>` | GitHub org or user that owns the ProjectV2 board. |
| `<project-number>` | ProjectV2 number shown by `gh project list`. |
| `<project-node-id>` | ProjectV2 node id, starting with `PVT_`. |
| `<detent-project-id>` | Local `global.yaml` project id, such as `api`. |

## Phase 0 — Preconditions

1. **Confirm Detent is installed.** Follow
   [Bootstrap On A New Machine steps 1-3](../README.md#bootstrap-on-a-new-machine-humans-and-ai-agents)
   before project onboarding. Verify:

   ```sh
   detent version
   ```

2. **Confirm GitHub CLI auth and scopes.** Detent needs a token that can read
   the repo, org, and ProjectV2 board, plus write ProjectV2 status fields. For
   first-time auth, request every required scope:

   ```sh
   gh auth login --scopes "repo,read:org,read:project,project"
   ```

   If any scope check fails for existing auth, refresh the token:

   ```sh
   gh auth refresh -h github.com --scopes "repo,read:org,read:project,project"
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
   gh auth status 2>&1 | rg '\bread:project\b'
   gh auth status 2>&1 | rg "(^|[[:space:],'\"])project([[:space:],'\"]|$)"
   gh project list --owner <project-owner> --format json --limit 1
   ```

   `gh project list` verifies the `read:project` board discovery path. Defer
   write `project` verification until the first intentional ProjectV2 mutation,
   such as creating or linking a board, creating fields, or editing the status
   of a real existing item.

3. **Confirm Codex is installed and signed in.** Detent dispatches agents
   through the Codex app-server. Verify:

   ```sh
   codex --version
   ```

4. **Confirm the target checkout exists.** If `<source-root>` does not exist,
   clone it from the repository named by the human. Verify:

   ```sh
   git -C <source-root> remote get-url origin
   git -C <source-root> rev-parse --show-toplevel
   ```

## GitHub GraphQL Rate-Budget Discipline

Treat every `gh project ...` command as GitHub GraphQL work. GitHub
ProjectV2 is GraphQL-backed, so `gh project list`, `field-list`, `item-list`,
`item-add`, and `item-edit` spend the same GraphQL primary rate-limit budget
as Detent's GitHub connector. When `github_token: gh` is configured, the
operator shell, Detent, and spawned Codex agents all use the same `gh` user
token and therefore the same user-token GraphQL bucket.

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

Do not ask questions in this phase. Inspect the actual setup, write one
grounded recommendation per Phase 2 question, then interview the human.

1. **Create an onboarding notes directory.** Keep all discovery artifacts in
   one place so recommendations can cite evidence. Verify:

   ```sh
   ONBOARDING_DIR="${TMPDIR:-/tmp}/detent-onboarding-<repo-owner>-<repo-name>"
   mkdir -p "$ONBOARDING_DIR"
   test -d "$ONBOARDING_DIR"
   ```

2. **Record the initial GitHub GraphQL budget.** Use the REST rate-limit
   endpoint before the first ProjectV2 discovery command. If the remaining
   budget is low, record the warning in the recommendation and avoid
   GraphQL-heavy board inventory until reset or GitHub App auth is available.
   Verify:

   ```sh
   gh api rate_limit --jq '.resources.graphql | {limit, used, remaining, reset}' \
     > "$ONBOARDING_DIR/graphql-rate-limit.before-discovery.json"
   jq -r '"graphql remaining=\(.remaining) reset=\(.reset)"' \
     "$ONBOARDING_DIR/graphql-rate-limit.before-discovery.json"
   jq -e '.remaining >= 1000' "$ONBOARDING_DIR/graphql-rate-limit.before-discovery.json" \
     || printf 'WARNING: low GitHub GraphQL budget; avoid ProjectV2 inventory loops before reset\n'
   ```

3. **Inspect the validation surface.** Prefer a repo-local release gate over an
   invented command. If `make check` exists, recommend `gate.kind: command`
   with `gate.run: make check`. If there is no local command but CI is clear,
   recommend the closest local equivalent. If no command can be inferred,
   recommend `gate.kind: human_review` with an approval label only when the
   workflow explicitly wants a human label to promote. Verify:

   ```sh
   cd <source-root>
   {
     test -f Makefile && awk -F: '/^[A-Za-z0-9][A-Za-z0-9_.-]*:/ {print "make " $1}' Makefile
     fd -a '' .github/workflows 2>/dev/null || true
     rg -n 'make check|go test|npm test|pnpm test|pytest|cargo test|just|task' \
       Makefile .github/workflows package.json go.mod pyproject.toml justfile Taskfile.yml \
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
   detent config path > "$ONBOARDING_DIR/global-path.txt"
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

6. **Inspect existing ProjectV2 boards.** Recommend reuse when a board clearly
   belongs to this repo or workstream; otherwise recommend creating a new board
   named after the repo or product. This is the ProjectV2 read verification.
   Verify:

   ```sh
   gh project list --owner <project-owner> --format json --limit 50 \
     > "$ONBOARDING_DIR/projects.json"
   jq -e '.projects | length >= 0' "$ONBOARDING_DIR/projects.json"
   ```

7. **Inspect priority counts for reuse candidates.** `priority_in` depends on
   the ProjectV2 `Priority` field, so gather counts from the strongest reuse
   candidate with one paginated inventory pass saved to a local artifact. Do
   not repeatedly call `gh project item-list --limit 1000`. For a new board,
   record an empty count table and recommend no `priority_in` filter until
   issues have been added and ranked. Verify:

   ```sh
   REUSE_PROJECT_NODE_ID="<reuse-candidate-project-node-id-or-empty>"
   if test -n "$REUSE_PROJECT_NODE_ID"; then
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
     'board: <reuse-or-create recommendation, from projects.json>' \
     'rate_budget: <GraphQL remaining/reset and low-budget warning, from graphql-rate-limit.before-discovery.json>' \
     'scheduling: <priority/weight recommendation, from global-projects.txt>' \
     'authorization: <filter recommendation, from issue-counts.json and priority-counts.json>' \
     'dashboard_bind: <localhost/private-or-tailscale/all-interfaces recommendation>' \
     'gate: <gate recommendation, from gate.txt>' \
     'concurrency: <max agents and Merging cap recommendation>' \
     'review_policy: <hard stop or auto-promote recommendation>' \
     'prompt: <template or repo-specific recommendation, from repo docs>' \
     'intake: <bulk-add filter and initial Status recommendation>' \
     > "$ONBOARDING_DIR/recommendations.md"
   rg -n '^(board|rate_budget|scheduling|authorization|dashboard_bind|gate|concurrency|review_policy|prompt|intake):' \
     "$ONBOARDING_DIR/recommendations.md"
   ```

## Phase 2 — Interview The Human

Ask only these decision questions. Present each as question, grounded
recommendation, and default-if-silent. Record answers in
`$ONBOARDING_DIR/answers.env`.

1. **Board.** Ask: "Reuse an existing ProjectV2 board or create a new one?"
   List the boards from `$ONBOARDING_DIR/projects.json`.
   Recommendation source: matching board title, owner, item count, and whether
   the board already has repo work. Default if silent: reuse the strongest
   matching board; if none match, create `<repo-name>`. Verify:

   ```sh
   printf '%s\n' \
     'BOARD_MODE=<reuse|create>' \
     'PROJECT_OWNER=<project-owner>' \
     'PROJECT_NUMBER=<project-number-if-reuse>' \
     'PROJECT_TITLE=<project-title-if-create>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^BOARD_MODE=' "$ONBOARDING_DIR/answers.env"
   ```

2. **Scheduling.** Ask: "What `global.yaml` `priority` from 1-4 and `weight`
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

3. **Instance name.** Ask: "What optional instance name should appear in
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

4. **Authorization filters.** Ask: "Should Detent consider all board items or
   only items matching a filter?" Offer `none`, `labels.include`,
   `labels.exclude`, `assignee_in`, `author_in`, and `priority_in`.
   Recommendation source: live counts in `$ONBOARDING_DIR/issue-counts.json`
   and `$ONBOARDING_DIR/priority-counts.json`, plus any repo/workstream labels
   already in use. Show the total count for `none`, counts for each label,
   assignee, author, and priority option, and the remaining count for any
   proposed `labels.exclude`. Default if silent: no filter for a dedicated repo
   board; otherwise the narrowest label or assignee filter that matches the
   intended workstream. Verify:

   ```sh
   printf '%s\n' \
     'AUTHORIZATION_KIND=<none|labels.include|labels.exclude|assignee_in|author_in|priority_in>' \
     'AUTHORIZATION_VALUE=<value-or-empty>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^AUTHORIZATION_' "$ONBOARDING_DIR/answers.env"
   ```

5. **Dashboard bind.** Ask: "How should the Detent dashboard bind:
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

6. **Validation gate.** Ask: "Use the detected command, a custom command, or a
   human review label gate? If this is a command gate, should auto-promotion
   require an automated GitHub PR review from a bot?" Recommendation source:
   `$ONBOARDING_DIR/gate.txt`, Makefile targets, CI workflow commands, and the
   repo's review policy. Default if silent: detected `make check` when present
   with `require_automated_review: true`; otherwise `kind: human_review` with
   `approval_label: human-approved`. Verify:

   ```sh
   printf '%s\n' \
     'GATE_KIND=<command|human_review>' \
     'GATE_RUN=<command-if-command>' \
     'GATE_REQUIRE_AUTOMATED_REVIEW=<true|false-if-command>' \
     'GATE_APPROVAL_LABEL=<label-if-human-review>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^GATE_' "$ONBOARDING_DIR/answers.env"
   ```

7. **Concurrency.** Ask: "How many agents may this project run at once?"
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

8. **Review policy.** Ask: "Should Detent hard-stop at `Human Review`, or may
   it auto-promote to `Merging` after the human-defined criteria are true?"
   Recommendation source: repo risk, issue labels, review requirements, and how
   much trust the human wants to delegate. Default if silent:
   `agent.auto_promote.enabled: false`, the safe hard stop. Both modes are
   fully supported, and this is the human's call.

   For criteria-based auto-promote, use `agent.auto_promote.enabled`,
   `quiet_seconds`, `optout_label`, `allowed_issue_labels`, and the top-level
   command gate's `require_automated_review` setting. `quiet_seconds` is the
   quiet period after observed issue/status/review activity, `optout_label` is
   the per-issue escape hatch, and `allowed_issue_labels` is an allowlist such
   as `documentation` for low-risk issue classes. When automated review is
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

9. **Dependency waiting policy.** Ask: "Should dependency-waiting issues stay
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

10. **Prompt body.** Ask: "Use the template prompt or add repo-specific
   instructions?" Recommendation source: `CLAUDE.md`, `AGENTS.md`,
   `CONTRIBUTING.md`, README development commands, and CI workflows in
   `<source-root>`. Default if silent: template prompt plus any repo authority
   files found. Verify:

   ```sh
   {
     fd -a '^(CLAUDE|AGENTS|CONTRIBUTING)\.md$' <source-root> 2>/dev/null || true
     rg -n 'make check|go test|npm test|pnpm test|pytest|cargo test' \
       <source-root>/README.md <source-root>/CONTRIBUTING.md <source-root>/.github/workflows \
       2>/dev/null || true
   } > "$ONBOARDING_DIR/prompt-evidence.txt"
   printf 'PROMPT_MODE=<template|repo-specific>\n' >> "$ONBOARDING_DIR/answers.env"
   rg '^PROMPT_MODE=' "$ONBOARDING_DIR/answers.env"
   ```

11. **Issue intake.** Ask: "Which issue filter should be bulk-added, should the
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

## Phase 3 — Create Or Adopt The Board

1. **Adopt an existing board when the answer says reuse.** Record the number
   and node id for later commands. Verify:

   ```sh
   gh project view <project-number> --owner <project-owner> --format json \
     > "$ONBOARDING_DIR/project.json"
   jq -e '.id | startswith("PVT_")' "$ONBOARDING_DIR/project.json"
   ```

2. **Create a board when the answer says create.** GitHub creates the default
   `Status` field; Detent still needs you to create the board itself. Verify:

   ```sh
   gh project create --owner <project-owner> --title "<project-title>" --format json \
     > "$ONBOARDING_DIR/project.json"
   jq -e '.number and (.id | startswith("PVT_"))' "$ONBOARDING_DIR/project.json"
   ```

3. **Ensure the `Priority` field exists.** Detent can add missing options
   inside an existing field, but it never creates the field. Reused boards can
   already have this field, so check before creating it. If `Priority` exists
   but is not a single-select field, stop and ask the human to rename the
   conflicting field or choose another board. Verify:

   ```sh
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

4. **Link the repository to the board.** This keeps the project discoverable
   from the repo. Verify:

   ```sh
   gh project link "$PROJECT_NUMBER" --owner <project-owner> --repo <repo-name>
   gh project view "$PROJECT_NUMBER" --owner <project-owner> --format json --jq '.url'
   ```

5. **Confirm required fields.** Detent auto-provisions missing `Status` and
   `Priority` options on first run, but never the board or fields themselves.
   Verify:

   ```sh
   gh project field-list "$PROJECT_NUMBER" --owner <project-owner> --format json \
     | jq -e '[.fields[].name] as $names | all(["Status","Priority"][]; . as $want | $names | index($want))'
   ```

6. **Clean up inherited statuses before first Detent dispatch.** Reused boards
   often carry status options and stale item placements from a predecessor
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

## Phase 4 — Author WORKFLOW.md

1. **Fetch the canonical template.** Read the existing file first if one is
   present; this runbook is for from-zero repositories, so do not overwrite a
   human-authored contract without explicit approval. Verify:

   ```sh
   test ! -f <source-root>/WORKFLOW.md
   curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent-orchestration/main/WORKFLOW.md \
     -o <source-root>/WORKFLOW.md
   rg -n '^tracker:|^workspace:|^agent:' <source-root>/WORKFLOW.md
   ```

2. **Substitute the board and workspace answers.** Use the ProjectV2 node id as
   `tracker.project_slug`; use absolute paths for `workspace.source_root` and
   `workspace.root`. Verify:

   ```sh
   PROJECT_NODE_ID="$(jq -r '.id' "$ONBOARDING_DIR/project.json")"
   perl -0pi -e "s/project_slug: \"?PVT_[^\"\\n]+\"?/project_slug: \"$PROJECT_NODE_ID\"/" <source-root>/WORKFLOW.md
   perl -0pi -e 's#(?m)^  source_root: .*$#  source_root: <source-root>#' <source-root>/WORKFLOW.md
   perl -0pi -e 's#(?m)^  root: .*$#  root: <worktree-root>#' <source-root>/WORKFLOW.md
   rg -n "project_slug: \"$PROJECT_NODE_ID\"|source_root: <source-root>|root: <worktree-root>" \
     <source-root>/WORKFLOW.md
   ```

3. **Set the dashboard bind from the interview.** This writes the default
   `server.host` used when Detent starts without an explicit `--host`. Service
   managers can still override it in `ExecStart` with the same selected host.
   Verify:

   ```sh
   perl -0pi -e 's#(?m)^  host: .*$#  host: <dashboard-host>#' <source-root>/WORKFLOW.md
   rg -n '^server:|host: <dashboard-host>|port:' <source-root>/WORKFLOW.md
   ```

4. **Set the gate from the interview.** For command gates, include the command
   and whether an automated GitHub PR review is required for auto-promotion.
   For human gates, include the approval label. Verify:

   ```sh
   rg -n '^gate:|kind: <command|human_review>|run: <gate-command>|require_automated_review: <true|false>|approval_label: <label>' \
     <source-root>/WORKFLOW.md
   ```

   Command gate shape:

   ```yaml
   gate:
     kind: command
     run: <gate-command>
     require_automated_review: <true|false>
   ```

   Human review gate shape:

   ```yaml
   gate:
     kind: human_review
     approval_label: <approval-label>
   ```

5. **Set review policy, dependency waiting policy, and concurrency.** Keep
   `Merging: 1`. Use the review and dependency policy selected by the human.
   Verify:

   ```sh
   rg -n 'max_concurrent_agents: <max>|Merging: 1|auto_promote:|dependency_auto_unblock:|enabled: <true|false>|quiet_seconds: <seconds>|optout_label: <label>|allowed_issue_labels:|source_states:|target_state:|readiness:' \
     <source-root>/WORKFLOW.md
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
   to `Rework`. `detent doctor --port 0` reports sampled `Human Review`
   candidates and reasons such as `automated_review_missing` when that gate is
   not met.

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

6. **Write the prompt body.** Keep the `## Codex Workpad` instruction, include
   repo authority files discovered in Phase 2, and state the validation gate.
   Verify:

   ```sh
   awk 'seen {print} /^---$/ {count++; if (count == 2) seen=1}' <source-root>/WORKFLOW.md \
     | rg 'Codex Workpad|CLAUDE.md|AGENTS.md|CONTRIBUTING.md|<gate-command>|<repo-owner>/<repo-name>'
   ```

7. **Check the workflow contract before registration.** This is a structural
   check; `detent doctor` in Phase 5 is the full preflight. Verify:

   ```sh
   rg -n '^tracker:|project_slug:|^workspace:|source_root:|^agent:|max_concurrent_agents_by_state:|^gate:' \
     <source-root>/WORKFLOW.md
   ```

## Phase 5 — Register The Project

1. **Create global config if needed.** Use the resolved path Detent reports.
   Verify:

   ```sh
   detent init
   detent config path
   ```

2. **Register the project.** `priority` and `weight` are the scheduling answers
   from Phase 2. Verify:

   ```sh
   detent add-project \
     --id <detent-project-id> \
     --workflow <source-root>/WORKFLOW.md \
     --workdir <source-root> \
     --weight <global-weight> \
     --priority <global-priority>
   GLOBAL_CONFIG="$(detent config path | awk '/^path:/ {print $2}')"
   rg -n 'id: <detent-project-id>|workflow: <source-root>/WORKFLOW.md|workdir: <source-root>|weight: <global-weight>|priority: <global-priority>' \
     "$GLOBAL_CONFIG"
   ```

3. **Set runtime keys in `global.yaml`.** For local onboarding, prefer
   `github_token: gh` so Detent resolves the token from `gh auth token` at
   startup. This shares the operator's GraphQL budget with Detent and spawned
   agents; for production or high-volume boards, prefer GitHub App
   installation authentication in `WORKFLOW.md`. Set `instance_name` to the
   interview answer or the short hostname. Use a non-4000 port if another
   Detent instance is already running; keep the dashboard host in
   `WORKFLOW.md` `server.host` or pass it with `--host`. Verify:

   ```sh
   GLOBAL_CONFIG="$(detent config path | awk '/^path:/ {print $2}')"
   perl -0pi -e 's/^(global:)/env: prod\nlog_level: info\ngithub_token: gh\nport: <port>\ninstance_name: <instance-name>\n$1/m if !/^github_token:/m' "$GLOBAL_CONFIG"
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
   dispatch work from a failed doctor run. Verify:

   ```sh
   detent doctor
   ```

5. **Verify the systemd service PATH when Detent runs as a user service.**
   User services do not inherit the interactive shell PATH. `detent doctor`
   verifies Detent's direct dependencies, but a first dispatch can still fail
   if repo hooks or validation gates call tools from a project-local directory
   that is missing from the service environment, such as `$HOME/go/bin` for
   `air`. Copy the exact `Environment=PATH=...` value from `detent.service`,
   then verify Detent tools and every command used by `hooks.*` and the gate
   from that same service context:

   ```sh
   systemd-run --user --wait --collect --pipe \
     --property=Environment=PATH=<same-path-as-detent.service> \
     /usr/bin/bash -lc 'command -v gh; command -v codex; command -v git; command -v detent; command -v go; command -v npm; command -v npx; command -v air'
   ```

   Add every missing tool directory to the service PATH before dispatching. For
   example, if a repository bootstrap uses `air`, include the Go user binary
   directory:

   ```ini
   Environment=PATH=/home/<user>/.local/bin:/home/<user>/.npm-global/bin:/home/<user>/go/bin:/usr/local/go/bin:/usr/bin:/bin
   ```

   When the project defines `hooks.after_create` or other bootstrap hooks,
   dry-run that hook or the equivalent repo bootstrap script from an isolated
   throwaway worktree with the same service PATH before moving an issue to
   `Todo`.

## Phase 6 — Issue Intake

1. **Confirm the selected initial status option exists.** If this fails on a
   fresh board, start Detent once with no dispatchable items so it can
   auto-provision missing options, then repeat this verification. Verify:

   ```sh
   gh project field-list <project-number> --owner <project-owner> --format json \
     --jq '.fields[] | select(.name == "Status") | .options[].name' | rg -x '<initial-status>'
   ```

2. **Bulk-add issues by the selected filter and set initial `Status`.** Use
   the exact `gh issue list` flags from the intake answer. Use `Backlog` for
   broad intake and `Todo` only for work that should dispatch immediately.
   This verifies the write `project` scope if no earlier board creation,
   linking, field creation, or item edit has already done so.
   `gh project item-add` and `gh project item-edit` are GraphQL mutations, so
   do not start a broad intake when the budget warning from Phase 1 is still
   unresolved. Verify with one cached inventory after the mutations finish:

   ```sh
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

3. **Optionally enable auto-add.** This is a **human UI step** because GitHub's
   built-in ProjectV2 auto-add workflows are not configurable through the API.
   Click: Project → ⋯ → Workflows → Auto-add to project. Configure the same
   repo and filter chosen in the interview. Verify:

   ```sh
   gh project item-list <project-number> --owner <project-owner> \
     --query 'repo:<repo-owner>/<repo-name> is:issue is:open' \
     --format json > "$ONBOARDING_DIR/project-items.auto-add.json"
   jq '.totalCount' "$ONBOARDING_DIR/project-items.auto-add.json"
   ```

## Phase 7 — Smoke Test

1. **Start Detent or hot-reload the running process.** Use the configured port,
   not `4000` when another Detent instance owns that port. Use the dashboard
   host chosen in Phase 2: `127.0.0.1` for SSH tunnels, a private/Tailscale IP
   for VPN-only exposure, or `0.0.0.0` only on trusted private networks because
   it exposes every interface. Verify:

   ```sh
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

2. **Check the GraphQL budget before smoke dispatch.** This is the last stop
   before Detent and the spawned agent start spending GraphQL budget for
   polling, workpad, status, PR, and review work. Verify:

   ```sh
   gh api rate_limit --jq '.resources.graphql | {limit, used, remaining, reset}' \
     > "$ONBOARDING_DIR/graphql-rate-limit.before-smoke.json"
   jq -r '"graphql remaining=\(.remaining) reset=\(.reset)"' \
     "$ONBOARDING_DIR/graphql-rate-limit.before-smoke.json"
   jq -e '.remaining >= 500' "$ONBOARDING_DIR/graphql-rate-limit.before-smoke.json" \
     || printf 'WARNING: low GitHub GraphQL budget; defer smoke dispatch or use GitHub App auth\n'
   ```

3. **Move one real issue to `Todo`.** Use a real issue that matches the
   authorization filters. Do not verify this by polling ProjectV2 after the
   edit; switch to the local Detent API in the next step. Verify:

   ```sh
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
   ```

4. **Verify the issue dispatches locally.** Onboarding is not complete until
   the issue appears under Running on the dashboard. Once it does, stop
   operator GitHub ProjectV2 polling; the spawned agent owns GitHub work from
   here. Verify:

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

### Interview Answers To Config Keys

| Interview answer | Config or system target |
| --- | --- |
| Board node id | `tracker.project_slug` in `WORKFLOW.md`. |
| Project scheduling priority | `projects[].priority` in `global.yaml`. |
| Project scheduling weight | `projects[].weight` in `global.yaml`. |
| Authorization filters | `tracker.authorization` in `WORKFLOW.md`; optionally `projects[].authorization` in `global.yaml` for host-level scoping. |
| Dashboard bind | `server.host` in `WORKFLOW.md`, or `--host` in the startup command or service `ExecStart`. |
| Validation command | `gate.kind: command` and `gate.run` in `WORKFLOW.md`. |
| Automated PR review requirement | `gate.kind: command` and `gate.require_automated_review` in `WORKFLOW.md`. |
| Human validation label | `gate.kind: human_review` and `gate.approval_label` in `WORKFLOW.md`. |
| Per-project concurrency | `agent.max_concurrent_agents` in `WORKFLOW.md`. |
| Merge serialization | `agent.max_concurrent_agents_by_state.Merging: 1` in `WORKFLOW.md`. |
| Hard-stop review policy | `agent.auto_promote.enabled: false` in `WORKFLOW.md`. |
| Criteria-based auto-promote | `agent.auto_promote.enabled`, `quiet_seconds`, `optout_label`, and `allowed_issue_labels` in `WORKFLOW.md`. |
| Prompt body | Markdown body after the closing frontmatter `---` in `WORKFLOW.md`. |
| Intake filter | `gh issue list` flags and optional GitHub Project auto-add workflow. |
| Initial issue status | ProjectV2 `Status` field value, usually `Backlog` or `Todo`. |

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

- Exact command the agent must run, such as `make check`.
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
