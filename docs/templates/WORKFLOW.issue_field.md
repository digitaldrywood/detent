---
tracker:
  kind: github
  github_status_source: issue_field
  repository: <repo-owner>/<repo-name>
  status_field: Status
  http_max_idle_conns: 100
  http_max_idle_conns_per_host: 32
  http_idle_conn_timeout_ms: 90000
  github_graphql_warn_remaining: 500
  github_graphql_min_remaining_reserve: 1000
  github_rest_min_remaining_reserve: 1000
  github_rest_fanout_max_requests: 80
  github_rest_debug_logging: false
  # github_webhook_secret: $DETENT_GITHUB_WEBHOOK_SECRET
  auto_provision: true
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
  interval_ms: 60000
  conditional: true
intake:
  sources: []
workspace:
  root: <worktree-root>
  source_root: <source-root>
  auto_branch: true
  cleanup_idle_ttl_ms: 86400000
  cleanup_sweep_interval_ms: 600000
deliverable:
  merge_method: squash
agent:
  max_concurrent_agents: 5
  # Optional per-role reasoning defaults. Unset roles retain backend behavior.
  # effort:
  #   merge: high
  max_turns: 20
  max_retry_backoff_ms: 300000
  overload_retry_delay_ms: 45000
  no_progress_spend_limit_usd: 3
  resume_orphaned_sessions: true
  # Per-session ceiling on total_tokens. total_tokens counts input + output +
  # cache-created + cache-read tokens, accumulated across every turn of the
  # session, so cached context is re-counted each turn. Use max_session_tokens
  # as the runaway brake. max_session_context_multiplier is an opt-in, coarse
  # cap on roughly how many full-context turns fit; leave it unset by default.
  max_session_tokens: 25000000
  max_session_token_override_label: allow-large-session
  max_concurrent_agents_by_state:
    Merging: 1
  dispatch_priority_by_state:
    - Merging
    - Rework
    - In Progress
    - Todo
  dispatch_priority_by_label: []
  prioritize_unblockers: true
  auto_promote:
    enabled: false
    quiet_seconds: 600
    optout_label: requires-human-review
    allowed_issue_labels: []
    gate_wait_state: review
    gate_wait_timeout_seconds: 3600
    source_state: Human Review
    pass_state: Merging
    rework_state: Rework
    rework_limit: 3
  # Skills guide: https://github.com/digitaldrywood/detent/blob/main/docs/ONBOARDING.md#skills-and-skill-creation
  skills:
    enabled: true
    path: .detent/skills
    max_skills_in_prompt: 50
    creation:
      enabled: true
      max_drafts_per_run: 1
codex:
  # Optional model_reasoning_effort is unset because not every model accepts it.
  # An issue-body ```detent-agent``` block can override effort for that issue;
  # see docs/ONBOARDING.md#per-issue-agent-overrides.
  command: codex app-server # Provider default: upgrades automatically and avoids retirement breakage.
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspaceWrite
    networkAccess: true
# Optional per-stage model routing. Unrouted roles fall back to the default code route.
# agents:
#   routes:
#     - name: plan-cheap
#       role: plan
#       backend: codex
#       model: gpt-5.4-mini
#     - name: rework-high-context
#       role: rework
#       backend: codex
#       model: gpt-5-codex-high
#     - name: merge-standard
#       role: merge
#       backend: codex
#       model: gpt-5-codex
#     - name: default
#       backend: codex
#       model: gpt-5-codex
#       default: true
gate:
  kind: command
  run: make check
  require_automated_review: true
  required_status_checks: []
  ci_failure_action: rework
  transient_ci_retry_limit: 2
  validator:
    enabled: false
    # Optional deliberate pin; leave empty to inherit the route/provider default.
    # Pinned models require manual updates before provider retirement.
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
    # Set show_blocked_alerts: true only when red blocked states should appear
    # as one compact top-of-board alert; dependency waits stay on cards.
    # show_blocked_alerts: true
    # Use mode: read_only for observer/shared dashboards or when write permissions are unavailable.
    # Optional allowed_transitions expose broader manual status editing.
    # allowed_transitions:
    #   In Progress: [Blocked, Cancelled]
    #   Rework: [Blocked, Cancelled]
    #   Merging: [Blocked, Cancelled]
budget:
  billing_mode: metered
  enabled: true
  per_day_max_usd: 50
  per_issue_max_usd: 5
  refusal_cooldown_seconds: 3600
  pricing_path: priv/pricing/models.yaml
hooks:
  timeout_ms: 60000
---
You are working on {{ issue.identifier }}: {{ issue.title }}.
Current Detent status: {{ issue.state }}.

Follow repository instructions, keep changes scoped to the issue, and keep a
single persistent `## Codex Workpad` issue comment updated with the plan,
validation evidence, and final handoff. Every Workpad update must include one
`detent-status` fenced block. Detent reads blocker and human-action
declarations from that block; narrative sentences are never read as blockers.
`status` must be one of `in_progress`, `blocked`, or `complete`.

Use `in_progress` while implementation or validation is still active:

```detent-status
schema: 1
status: in_progress
blockers: []
human_action: null
```

Use `complete` only when the pull request is open, references the issue,
validation is green, and no actionable review comments remain:

```detent-status
schema: 1
status: complete
blockers: []
human_action: null
```

For dependency blockers, use this order:

1. Create GitHub's native `blocked_by` dependency relation.

```sh
BLOCKED_NUMBER=<blocked-issue-number>
BLOCKER_NUMBER=<blocker-issue-number>
BLOCKER_ID="$(gh api repos/{owner}/{repo}/issues/$BLOCKER_NUMBER --jq '.id')"
gh api --method POST "repos/{owner}/{repo}/issues/$BLOCKED_NUMBER/dependencies/blocked_by" -F issue_id="$BLOCKER_ID"
```

2. Declare the blocker in the Workpad status block with `status: blocked`.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "waiting for the dependency to merge"
human_action: null
```

3. Legacy fallback during the deprecation window: if native dependencies are
   unavailable and the project has not migrated, keep a machine-readable
   issue-body line such as `Blocked by: #123` or `Depends on: owner/repo#123`.

If meaningful out-of-scope work is discovered, file a separate tracker issue in Backlog with a best-guess `detent-agent` effort block instead of expanding the current work item.

## Required Execution Flow

Use the current Detent state as the source of truth for which section applies.

Before any rebase, capture the branch's effective diff against its merge base
or preserve the pre-rebase ref. After the rebase, compare with `git range-diff`
or an equivalent diff-stat and confirm the same files and hunks remain. If
changes are missing without explanation or conflict resolution dropped hunks,
stop before pushing and move the issue to the configured blocked or exception
state.

### For Todo

1. Move the issue to `In Progress`.
2. Create or update the persistent `## Codex Workpad` comment with the plan,
   acceptance criteria, validation plan, and the `in_progress`
   `detent-status` block shown above.
3. Fetch current `origin/main`, confirm this worktree is based on it, and
   confirm every native dependency relation, `detent-status` blocker, and
   issue-body `Depends on:` reference is merged or otherwise terminal before
   coding.
4. Reproduce or confirm the reported behavior before changing code when the
   issue is a bug.
5. Implement the smallest complete change that satisfies the issue.
6. Run focused tests for touched packages, then run the configured validation
   gate.
7. Commit and push the branch.
8. Open or update a pull request that references the issue.
9. Re-check pull request comments, inline review comments, and CI after the
   latest push.
10. Move the issue to `Human Review` only after the pull request is open, not a
    draft, references the issue, validation is green, and no actionable review
    comments remain.

### For In Progress

1. Re-read the issue, pull request, comments, and `## Codex Workpad`, including
   the `detent-status` block.
2. Continue from the current repository and tracker state.
3. If implementation is complete, run the full pre-review gate, update the
   Workpad block to `status: complete` with `blockers: []` and
   `human_action: null`, and move the issue to `Human Review` only when the
   gate passes.

### For Rework

1. Re-read all human and bot feedback.
2. Move the issue to `In Progress`.
3. Fix the requested changes.
4. Push updates to the pull request.
5. Run the full pre-review gate again.
6. Move the issue back to `Human Review` only when the gate passes.

### For Merging

1. Confirm `$go-workflow:ship` is available in the Codex environment. If it is
   unavailable, keep the issue in `Merging` and record the missing ship workflow
   as `human_action` in the `detent-status` block.
2. Invoke and follow `$go-workflow:ship`.
3. Do not call `gh pr merge` directly outside the ship workflow.
4. End with exactly one terminal outcome:
   - pull request merged and issue moved to `Done`;
   - issue moved to `Rework` with an actionable defect;
   - issue remains in `Merging` with a concrete external blocker recorded in
     the `detent-status` block and described in the `## Codex Workpad`.
5. Move the issue to `Done` only after the pull request is merged.
