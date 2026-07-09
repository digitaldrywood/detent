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
  # session, so a healthy Claude session re-counts its cached context each turn
  # and accrues millions of tokens within minutes. Set this as a runaway-session
  # guard, not a context limit; max_session_context_multiplier bounds context
  # growth separately.
  max_session_tokens: 25000000
  max_session_context_multiplier: 4
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
    source_state: Review
    pass_state: Ready for Pickup
    rework_state: Rework
    rework_limit: 3

gate:
  kind: artifact
  ci_failure_action: rework
  transient_ci_retry_limit: 2
  validator:
    enabled: false
    model: gpt-5.4-mini
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

When the artifact is ready for review, update or emit data so the local SQLite
work item field `render_status` becomes `pending_review`. When a human or
external renderer marks it `approved` or `valid`, Detent can auto-promote the
item to `Ready for Pickup`. Use `recut`, `invalid`, or `missing_assets` when the
item needs rework.
