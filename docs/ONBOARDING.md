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
   gh auth status 2>&1 | rg "'project'|, project|project,"
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

2. **Inspect the validation surface.** Prefer a repo-local release gate over an
   invented command. If `make check` exists, recommend `gate.kind: command`
   with `gate.run: make check`. If there is no local command but CI is clear,
   recommend the closest local equivalent. If no command can be inferred,
   recommend `gate.kind: human_review` with an approval label. Verify:

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

3. **Inspect existing global scheduling.** Show the current project table
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

4. **Inspect open issue distribution.** Count candidate issues by label,
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

5. **Inspect existing ProjectV2 boards.** Recommend reuse when a board clearly
   belongs to this repo or workstream; otherwise recommend creating a new board
   named after the repo or product. This is the ProjectV2 read verification.
   Verify:

   ```sh
   gh project list --owner <project-owner> --format json --limit 50 \
     > "$ONBOARDING_DIR/projects.json"
   jq -e '.projects | length >= 0' "$ONBOARDING_DIR/projects.json"
   ```

6. **Inspect priority counts for reuse candidates.** `priority_in` depends on
   the ProjectV2 `Priority` field, so gather counts from the strongest reuse
   candidate. For a new board, record an empty count table and recommend no
   `priority_in` filter until issues have been added and ranked. Verify:

   ```sh
   REUSE_PROJECT_NODE_ID="<reuse-candidate-project-node-id-or-empty>"
   if test -n "$REUSE_PROJECT_NODE_ID"; then
     gh api graphql \
       -f project="$REUSE_PROJECT_NODE_ID" \
       -f query='query($project:ID!){node(id:$project){... on ProjectV2{items(first:100){nodes{content{... on Issue{state repository{nameWithOwner}}}priorityValue:fieldValueByName(name:"Priority"){... on ProjectV2ItemFieldSingleSelectValue{name}}}}}}}' \
       > "$ONBOARDING_DIR/priority-items.json"
     jq --arg repo '<repo-owner>/<repo-name>' \
       '[.data.node.items.nodes[] | select(.content.repository.nameWithOwner == $repo and .content.state == "OPEN") | (.priorityValue.name // "No priority")] | sort | group_by(.) | map({name: .[0], count: length})' \
       "$ONBOARDING_DIR/priority-items.json" > "$ONBOARDING_DIR/priority-counts.json"
   else
     printf '[]\n' > "$ONBOARDING_DIR/priority-counts.json"
   fi
   jq -e 'type == "array"' "$ONBOARDING_DIR/priority-counts.json"
   ```

7. **Record recommendations before the interview.** The recommendation must
   cite the discovery artifact that produced it. Verify:

   ```sh
   printf '%s\n' \
     'board: <reuse-or-create recommendation, from projects.json>' \
     'scheduling: <priority/weight recommendation, from global-projects.txt>' \
     'authorization: <filter recommendation, from issue-counts.json and priority-counts.json>' \
     'gate: <gate recommendation, from gate.txt>' \
     'concurrency: <max agents and Merging cap recommendation>' \
     'review_policy: <hard stop or auto-promote recommendation>' \
     'prompt: <template or repo-specific recommendation, from repo docs>' \
     'intake: <bulk-add filter and initial Status recommendation>' \
     > "$ONBOARDING_DIR/recommendations.md"
   rg -n '^(board|scheduling|authorization|gate|concurrency|review_policy|prompt|intake):' \
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

3. **Authorization filters.** Ask: "Should Detent consider all board items or
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

4. **Validation gate.** Ask: "Use the detected command, a custom command, or a
   human review label gate?" Recommendation source:
   `$ONBOARDING_DIR/gate.txt`, Makefile targets, and CI workflow commands.
   Default if silent: detected `make check` when present; otherwise
   `kind: human_review` with `approval_label: human-approved`. Verify:

   ```sh
   printf '%s\n' \
     'GATE_KIND=<command|human_review>' \
     'GATE_RUN=<command-if-command>' \
     'GATE_APPROVAL_LABEL=<label-if-human-review>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^GATE_' "$ONBOARDING_DIR/answers.env"
   ```

5. **Concurrency.** Ask: "How many agents may this project run at once?"
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

6. **Review policy.** Ask: "Should Detent hard-stop at `Human Review`, or may
   it auto-promote to `Merging` after the human-defined criteria are true?"
   Recommendation source: repo risk, issue labels, review requirements, and how
   much trust the human wants to delegate. Default if silent:
   `agent.auto_promote.enabled: false`, the safe hard stop. Both modes are
   fully supported, and this is the human's call.

   For criteria-based auto-promote, use only existing config keys:
   `agent.auto_promote.enabled`, `quiet_seconds`, `optout_label`, and
   `allowed_issue_labels`. `quiet_seconds` is the quiet period after automated
   review activity, `optout_label` is the per-issue escape hatch, and
   `allowed_issue_labels` is an allowlist such as `documentation` for
   low-risk issue classes. Verify:

   ```sh
   printf '%s\n' \
     'AUTO_PROMOTE_ENABLED=<true|false>' \
     'AUTO_PROMOTE_QUIET_SECONDS=<seconds>' \
     'AUTO_PROMOTE_OPTOUT_LABEL=<label>' \
     'AUTO_PROMOTE_ALLOWED_LABELS=<comma-separated-labels-or-empty>' \
     >> "$ONBOARDING_DIR/answers.env"
   rg '^AUTO_PROMOTE_' "$ONBOARDING_DIR/answers.env"
   ```

7. **Prompt body.** Ask: "Use the template prompt or add repo-specific
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

8. **Issue intake.** Ask: "Which issue filter should be bulk-added, should the
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

3. **Set the gate from the interview.** For command gates, include the command.
   For human gates, include the approval label. Verify:

   ```sh
   rg -n '^gate:|kind: <command|human_review>|run: <gate-command>|approval_label: <label>' \
     <source-root>/WORKFLOW.md
   ```

   Command gate shape:

   ```yaml
   gate:
     kind: command
     run: <gate-command>
   ```

   Human review gate shape:

   ```yaml
   gate:
     kind: human_review
     approval_label: <approval-label>
   ```

4. **Set review policy and concurrency.** Keep `Merging: 1`. Use the review
   policy selected by the human. Verify:

   ```sh
   rg -n 'max_concurrent_agents: <max>|Merging: 1|auto_promote:|enabled: <true|false>|quiet_seconds: <seconds>|optout_label: <label>|allowed_issue_labels:' \
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

5. **Write the prompt body.** Keep the `## Codex Workpad` instruction, include
   repo authority files discovered in Phase 2, and state the validation gate.
   Verify:

   ```sh
   awk 'seen {print} /^---$/ {count++; if (count == 2) seen=1}' <source-root>/WORKFLOW.md \
     | rg 'Codex Workpad|CLAUDE.md|AGENTS.md|CONTRIBUTING.md|<gate-command>|<repo-owner>/<repo-name>'
   ```

6. **Check the workflow contract before registration.** This is a structural
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
   startup. Use a non-4000 port if another Detent instance is already running.
   Verify:

   ```sh
   GLOBAL_CONFIG="$(detent config path | awk '/^path:/ {print $2}')"
   perl -0pi -e 's/^(global:)/env: prod\nlog_level: info\ngithub_token: gh\nport: <port>\n$1/m if !/^github_token:/m' "$GLOBAL_CONFIG"
   rg -n '^(env|log_level|github_token|port):' "$GLOBAL_CONFIG"
   ```

   Required shape:

   ```yaml
   env: prod
   log_level: info
   github_token: gh
   port: <port>
   ```

4. **Run preflight until every check passes.** Fix every `FAIL`; do not
   dispatch work from a failed doctor run. Verify:

   ```sh
   detent doctor
   ```

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
   linking, field creation, or item edit has already done so. Verify:

   ```sh
   PROJECT_NODE_ID="$(gh project view <project-number> --owner <project-owner> --format json --jq '.id')"
   STATUS_FIELD_ID="$(gh project field-list <project-number> --owner <project-owner> --format json --jq '.fields[] | select(.name == "Status") | .id')"
   STATUS_OPTION_ID="$(gh project field-list <project-number> --owner <project-owner> --format json --jq '.fields[] | select(.name == "Status") | .options[] | select(.name == "<initial-status>") | .id')"

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
     --jq '[.items[] | select(.content.repository == "<repo-owner>/<repo-name>" and .status == "<initial-status>")] | length'
   ```

3. **Optionally enable auto-add.** This is a **human UI step** because GitHub's
   built-in ProjectV2 auto-add workflows are not configurable through the API.
   Click: Project → ⋯ → Workflows → Auto-add to project. Configure the same
   repo and filter chosen in the interview. Verify:

   ```sh
   gh project item-list <project-number> --owner <project-owner> \
     --query 'repo:<repo-owner>/<repo-name> is:issue is:open' \
     --format json --jq '.totalCount'
   ```

## Phase 7 — Smoke Test

1. **Start Detent or hot-reload the running process.** Use the configured port,
   not `4000` when another Detent instance owns that port. Verify:

   ```sh
   detent --host 127.0.0.1 --port <port>
   ```

   In another shell, verify:

   ```sh
   curl -fsS http://127.0.0.1:<port>/health | jq -e '.status == "ok" and .mode == "running"'
   ```

2. **Move one real issue to `Todo`.** Use a real issue that matches the
   authorization filters. Verify:

   ```sh
   TODO_OPTION_ID="$(gh project field-list <project-number> --owner <project-owner> --format json --jq '.fields[] | select(.name == "Status") | .options[] | select(.name == "Todo") | .id')"
   STATUS_FIELD_ID="$(gh project field-list <project-number> --owner <project-owner> --format json --jq '.fields[] | select(.name == "Status") | .id')"
   PROJECT_NODE_ID="$(gh project view <project-number> --owner <project-owner> --format json --jq '.id')"
   ITEM_ID="$(gh project item-list <project-number> --owner <project-owner> --format json --limit 1000 --jq '.items[] | select(.content.url == "https://github.com/<repo-owner>/<repo-name>/issues/<issue-number>") | .id')"
   gh project item-edit \
     --id "$ITEM_ID" \
     --project-id "$PROJECT_NODE_ID" \
     --field-id "$STATUS_FIELD_ID" \
     --single-select-option-id "$TODO_OPTION_ID"
   gh project item-list <project-number> --owner <project-owner> --format json --limit 1000 \
     --jq '.items[] | select(.content.url == "https://github.com/<repo-owner>/<repo-name>/issues/<issue-number>") | .status' | rg -x 'Todo'
   ```

3. **Verify the issue dispatches.** Onboarding is not complete until the issue
   appears under Running on the dashboard. Verify:

   ```sh
   curl -fsS http://127.0.0.1:<port>/api/v1/state \
     | jq -e '.running[] | select(.identifier == "<repo-owner>/<repo-name>#<issue-number>")'
   ```

4. **Verify Detent posted the workpad.** The issue must have a persistent
   `## Codex Workpad` comment. Verify:

   ```sh
   gh api repos/<repo-owner>/<repo-name>/issues/<issue-number>/comments --paginate \
     --jq '.[] | select(.body | startswith("## Codex Workpad")) | .html_url' | rg .
   ```

## Appendix

### Interview Answers To Config Keys

| Interview answer | Config or system target |
| --- | --- |
| Board node id | `tracker.project_slug` in `WORKFLOW.md`. |
| Project scheduling priority | `projects[].priority` in `global.yaml`. |
| Project scheduling weight | `projects[].weight` in `global.yaml`. |
| Authorization filters | `tracker.authorization` in `WORKFLOW.md`; optionally `projects[].authorization` in `global.yaml` for host-level scoping. |
| Validation command | `gate.kind: command` and `gate.run` in `WORKFLOW.md`. |
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
carry `Depends on: #A` and stay out of `Todo` until A has merged.
