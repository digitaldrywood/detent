---
tracker:
  kind: local_sqlite
  local_sqlite:
    path: .detent/work-items.db
    project_id: video-production
  active_states:
    - Todo
    - Production
    - Rework
  observed_states:
    - Backlog
    - Review
    - Blocked
  terminal_states:
    - Ready for Pickup
    - Done
    - Cancelled

workspace:
  kind: filesystem
  root: .detent/workspaces
  source_root: .
  output_root: output

deliverable:
  kind: artifact
  output_root: output
  review_url: http://127.0.0.1:8080/review

agent:
  # Per-session ceiling on total_tokens. total_tokens counts input + output +
  # cache-created + cache-read tokens, accumulated across every turn of the
  # session, so cached context is re-counted each turn. Use max_session_tokens
  # as the runaway brake. max_session_context_multiplier is an opt-in, coarse
  # cap on roughly how many full-context turns fit; leave it unset by default.
  max_session_tokens: 25000000
  max_session_token_override_label: allow-large-session
  # For a non-code workflow such as Research -> Draft -> Review -> Package ->
  # Publish, add stage-specific prompt additions here instead of turning the
  # main prompt into a conditional block:
  # instructions_by_state:
  #   Research: |
  #     Gather source notes, links, and open questions before drafting.
  #   Draft: |
  #     Write the artifact body and keep unresolved facts clearly marked.
  #   Review: |
  #     Address feedback and leave a concise change summary.
  #   Package: |
  #     Prepare final assets, metadata, and publication handoff notes.
  # instructions_by_transition:
  #   Review:
  #     Package: |
  #       Confirm all review comments are resolved before packaging.
  auto_promote:
    enabled: true
    quiet_seconds: 0
    optout_label: requires-human-review
    gate_wait_state: source
    gate_wait_timeout_seconds: 3600
    source_state: Review
    pass_state: Ready for Pickup
    rework_state: Rework
    rework_limit: 3

codex:
  # Optional model_reasoning_effort is unset because not every model accepts it.
  # An issue-body ```detent-agent``` block can override effort for that issue;
  # see docs/ONBOARDING.md#per-issue-agent-overrides.
  command: codex app-server # Provider default: upgrades automatically and avoids retirement breakage.

gate:
  kind: artifact
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
  artifact:
    status_field: render_status
    pass_statuses:
      - approved
      - valid
    wait_statuses:
      - queued
      - rendering
      - pending_review
    rework_statuses:
      - recut
      - invalid
      - missing_assets

server:
  kanban:
    mode: integration
    # Set show_blocked_alerts: true only when red blocked states should appear
    # as one compact top-of-board alert; dependency waits stay on cards.
    # show_blocked_alerts: true
    allowed_transitions:
      Backlog:
        - Todo
      Todo:
        - Production
        - Blocked
      Production:
        - Review
        - Blocked
      Review:
        - Ready for Pickup
        - Rework
        - Blocked
      Rework:
        - Production
        - Blocked
      Blocked:
        - Todo
        - Production
      Ready for Pickup:
        - Done
      Done: []
      Cancelled: []
budget:
  enabled: true
  per_day_max_usd: 50
  per_issue_max_usd: 5
  refusal_cooldown_seconds: 3600
  pricing_path: priv/pricing/models.yaml
---
# Non-Code Artifact Workflow

You are working on a local production work item, not a GitHub issue-to-PR task.
Use the filesystem workspace and configured output directory. Do not require a
git branch, pull request, CI run, or merge train unless the work item explicitly
asks for one.

Maintain the local `## Codex Workpad` record through Detent events. Every
Workpad update must include one `detent-status` fenced block. Detent reads
blocker and human-action declarations from that block; narrative sentences are
never read as blockers. `status` must be one of `in_progress`, `blocked`, or
`complete`.

Use `in_progress` while production or validation is still active:

```detent-status
schema: 1
status: in_progress
blockers: []
human_action: null
```

Use `complete` only when the artifact manifest is written, local validation is
green, and no actionable review feedback remains:

```detent-status
schema: 1
status: complete
blockers: []
human_action: null
```

Use `blocked` when required source assets, credentials, or human-only decisions
are missing:

```detent-status
schema: 1
status: blocked
blockers: []
human_action: "Provide the missing source assets."
```

Read the work item title, description, fields, metadata, and deliverable data.
Use the project source folder for instructions, scripts, media assets, product
copy, and production constraints. If required source assets are missing, record
the missing inputs clearly in the output manifest and set `render_status` to
`missing_assets` through the local status store or handoff process.

Produce a machine-readable artifact manifest under the work item output
directory. For video ad production, include:

- work item id and external id
- source asset paths used
- generated script or storyboard path
- render instructions or render output paths
- preview or review URL when available
- validation status and validation notes
- next external-system action

## Required Execution Flow

This workflow uses the artifact autopilot handoff: `agent.auto_promote.enabled:
true`, `quiet_seconds: 0`, and `gate_wait_state: source`. Completed agents keep
the work item in `Production`, set the Workpad `detent-status` block to
`status: complete`, set `render_status` to `valid` when the artifact gate is
satisfied, and let Detent promote the item to `Ready for Pickup`. Do not
self-move work items to `Review`.

### For Todo

1. Move the work item to `Production`.
2. Read the work item title, description, fields, metadata, and deliverable
   data.
3. Produce the artifact manifest under the configured output directory.
4. When the artifact is ready and local validation passes, set `render_status`
   to `valid`, update the Workpad block to `status: complete` with
   `blockers: []` and `human_action: null`, leave the work item in
   `Production`, and do not move it to `Review`.

### For Production

Continue production from the current filesystem state. When the artifact is
ready and local validation passes, set `render_status` to `valid`, set the
Workpad block to `status: complete`, and leave the work item in `Production`.

### For Rework

Move the work item to `Production`, address the requested changes, rerun the
artifact validation gate, set `render_status` to `valid`, set the Workpad block
to `status: complete`, and do not move the work item to `Review`.

### For Review

Review is reserved for explicit human opt-out or gate-wait timeout. Re-read the
feedback, update the artifact, then follow the Rework flow. Use `recut`,
`invalid`, or `missing_assets` when the item needs rework.
