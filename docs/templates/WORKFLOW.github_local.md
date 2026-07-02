---
tracker:
  kind: github_local
  repository: <repo-owner>/<repo-name>
  local_sqlite:
    path: .detent/github-local-work-items.db
    project_id: <local-detent-project-id>
  http_max_idle_conns: 100
  http_max_idle_conns_per_host: 32
  http_idle_conn_timeout_ms: 90000
  github_graphql_warn_remaining: 500
  github_graphql_min_remaining_reserve: 1000
  github_rest_min_remaining_reserve: 1000
  github_rest_fanout_max_requests: 80
  auto_provision: false
  active_states:
    - Todo
    - In Progress
    - Rework
    - Merging
  observed_states:
    - Backlog
    - Human Review
    - Blocked
  terminal_states:
    - Done
    - Cancelled
  state_map:
    Cancelled: Done
  priority_map:
    Urgent: 1
    High: 2
    Medium: 3
    Low: 4
    No priority: null
  dependency_auto_unblock:
    enabled: false
    source_states:
      - Blocked
    target_state: Todo
    readiness: terminal_or_merged
  blocker_auto_promote:
    enabled: false
    blocker_states:
      - Backlog
      - Blocked
      - Human Review
    target_state: Todo
polling:
  interval_ms: 120000
workspace:
  root: <worktree-root>
  source_root: <source-root>
  auto_branch: true
  cleanup_idle_ttl_ms: 86400000
  cleanup_sweep_interval_ms: 600000
agent:
  max_concurrent_agents: 5
  max_turns: 20
  max_retry_backoff_ms: 300000
  max_concurrent_agents_by_state:
    Merging: 1
  dispatch_priority_by_state:
    - Merging
    - Rework
    - In Progress
    - Todo
  dispatch_priority_by_label: []
  auto_promote:
    enabled: false
    quiet_seconds: 600
    optout_label: requires-human-review
    allowed_issue_labels: []
    rework_limit: 0
  skills:
    enabled: true
    path: .detent/skills
    max_skills_in_prompt: 50
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspaceWrite
    networkAccess: true
gate:
  kind: command
  run: make check
  require_automated_review: true
  ci_failure_action: rework
  transient_ci_retry_limit: 2
  validator:
    enabled: false
    # Recommended cheap override when enabled: gpt-5.4-mini.
    # Watch rework-rate per validator model once cache/model telemetry lands.
    model: ""
    min_score: 0.8
    max_inline_diff_bytes: 65536
    block_on:
      - p1
plan:
  enabled: false
  review: human
  approval_label: plan-approved
  stop: "Plan Review"
server:
  host: 127.0.0.1
  port: 4000
  kanban:
    mode: integration
hooks:
  timeout_ms: 60000
---
You are working on {{ issue.identifier }}: {{ issue.title }}.
Current Detent status: {{ issue.state }}.

This workflow uses `tracker.kind: github_local`. GitHub issues and pull
requests are read-only inputs. Detent status, claim fields, audit-trail
comments, and close decisions stay in the local SQLite database configured by
`tracker.local_sqlite.path`; do not add `tracker.github_status_source` to this
file.

Import issues explicitly before dispatching:

```sh
detent github-local import <local-detent-project-id> <issue-number>[,<issue-number>...] --state Todo
detent doctor --project <local-detent-project-id> --port 0
```

Follow repository instructions, keep changes scoped to the issue, and keep the
local `## Codex Workpad` record updated through Detent events. GitHub issue
comments, labels, Projects, issue fields, and issue close state must remain
untouched. Pull request comments and merges created by Detent are allowed for
Detent-owned PR lifecycle work.

## Required Execution Flow

Use the current Detent state as the source of truth for which section applies.

### For Todo

1. Move the issue to `In Progress`.
2. Fetch current `origin/main`, confirm this worktree is based on it, and
   confirm every `Depends on:` or `Blocked by:` issue or pull request is merged
   or otherwise terminal before coding.
3. Reproduce or confirm the reported behavior before changing code when the
   issue is a bug.
4. Implement the smallest complete change that satisfies the issue.
5. Run focused tests for touched packages, then run the configured validation
   gate.
6. Commit and push the branch.
7. Open or update a pull request that references the issue.
8. Move the issue to `Human Review` only after local validation passes and the
   PR is ready.

### For In Progress

Continue implementation from the current repository state. If implementation is
complete, run the validation gate, push the PR branch, and move the issue to
`Human Review`.

### For Rework

Read all review feedback, fix the requested changes, rerun validation, push the
branch, and move the issue back to `Human Review` only when the gate passes.

### For Merging

Rebase onto current `origin/main`, rerun the configured validation gate, push,
watch current-head CI, merge with the configured PR workflow once green, and
move the issue to `Done`. If an external blocker remains, keep the issue in
`Merging` and record the exact blocker locally.
