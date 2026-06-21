---
tracker:
  kind: github
  github_status_source: project_v2
  project_slug: <project-node-id>
  write_probe_issue: <repo-owner>/<repo-name>#<issue-number>
  http_max_idle_conns: 100
  http_max_idle_conns_per_host: 32
  http_idle_conn_timeout_ms: 90000
  github_graphql_warn_remaining: 500
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
  ci_failure_action: skip
  validator:
    enabled: false
    model: ""
    min_score: 0.8
    block_on:
      - p1
server:
  host: 127.0.0.1
  port: 4000
  kanban:
    mode: integration
    # Use mode: read_only for observer/shared dashboards or until write probes pass.
    # Optional allowed_transitions expose broader manual status editing.
    # allowed_transitions:
    #   In Progress: [Blocked, Cancelled]
    #   Rework: [Blocked, Cancelled]
    #   Merging: [Blocked, Cancelled]
hooks:
  timeout_ms: 60000
---
You are working on {{ issue.identifier }}: {{ issue.title }}.

Follow repository instructions, keep changes scoped to the issue, update the
persistent `## Codex Workpad` comment, run the validation gate, and prepare a
pull request for human review.
