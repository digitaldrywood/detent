# Concepts

[Back to README](../README.md#documentation)

### Connectors

Detent isolates tracker integration behind a connector interface. The current
production connector is GitHub. It supports the current ProjectV2-backed board
mode, boardless issue-field mode, boardless label mode, and `github_local`
hybrid mode. A memory connector is available for local development, and the
connector boundary is where GitLab and Jira support will land later.

GitHub configuration lives in each project's `detent.yaml`. The
default `github_status_source: project_v2` mode uses `project_slug` as the
GitHub ProjectV2 node id. Detent reads issue state, priority, labels, blockers,
and assignment from the board, then writes comments and state transitions back
through the connector. Boardless `github_status_source: issue_field` mode uses
`repository: owner/name` and an organization issue field such as
`status_field: Status`; Detent reads issues by issue-field value and updates
that field for state transitions. Boardless `github_status_source: label` mode
uses `repository: owner/name` and repository labels named by
`status_label_prefix`; Detent reads issues by configured status labels and
updates state by replacing the previous status label with the target one.
`tracker.kind: github_local` is a separate backend, not a fourth
`tracker.github_status_source` value. GitHub is read-only for issue, dependency,
parent/child, comment, PR, review, check, and rate-limit facts; Detent's SQLite
database is the source of truth for workflow state, claim fields, audit-trail
comments, priority, and local close decisions. In this mode Detent does not
create or mutate GitHub Projects, issue fields, repository labels, status
labels, GitHub issue comments, or GitHub issue close state. Pull request
lifecycle writes for Detent-owned PRs remain allowlisted.
Plain `detent doctor` is read-only and reports that write probes were skipped.
After mutation authorization, ProjectV2 and issue-field modes still need a
configured `tracker.write_probe_issue` to prove concrete status writes against
a project item or issue field value. Label mode can prove repository write
access without a scratch issue by sending intentionally invalid repository-label
and issue-create requests. GitHub should answer with validation errors, which
proves the Issues write permission class without creating board-visible work.
Set `tracker.write_probe_issue` in label mode only when you need legacy/deep
issue-object proof by replaying existing values on a scratch issue. That scratch
issue must already have one configured status label so doctor can reapply it;
after switching to the no-persistent probe path, remove any Detent status label
from old scratch issues or close them so they stop appearing as normal board
inventory.

GitHub issue fields apply to issues, not pull requests. In issue-field mode,
boardless status comes from the linked issue. In label mode, Detent treats
repository issues with configured status labels as work items. Detent still
displays linked PR state and can comment on PRs through GitHub's shared
issue-comment endpoints, but a PR-only card without a linked issue is not
dispatchable through issue-field or label status.

The GitHub connector uses one pooled keep-alive HTTP client for GraphQL and
GitHub App REST token requests. Tune `tracker.http_max_idle_conns`,
`tracker.http_max_idle_conns_per_host`, and
`tracker.http_idle_conn_timeout_ms` when many Detent instances share one host.
Keep host-level agent concurrency within the machine's shared outbound
connection and ephemeral-port budget; the connector logs its live connection
count on GitHub requests to help spot pressure. For shared-board operation, see
[Running Multiple Instances](multi-project.md#running-multiple-instances).

### Board States

The recommended GitHub Project board states are:

| State | Meaning |
| --- | --- |
| `Backlog` | Not eligible for agents yet. |
| `Todo` | Ready for Detent to claim and dispatch. |
| `In Progress` | An agent is actively working or continuing work. |
| `Blocked` | Human-blocked work, or dependency-waiting work when auto-unblock is enabled. |
| `Human Review` | The PR is ready for review/soak until the workflow's promotion criteria pass. |
| `Rework` | Human or bot feedback needs another agent pass. |
| `Merging` | Final rebase, merge-gate check, CI watch, and merge. |
| `Done` | Complete. |
| `Cancelled` | Terminal state mapped to `Done` in the default release flow. |

### Cancellation Lifecycle

Manual `Cancelled` means Detent should stop managing the work item, not that
GitHub issues or pull requests are automatically closed. On the next poll that
observes the cancelled terminal state, Detent cancels any running agent context,
releases the global dispatch slot, clears configured claim lease state, records
the run as completed with final state `Cancelled`, and asks the workspace
backend to remove the Detent worktree, prune git worktrees, delete the generated
`detent/` branch when safe, and reap workspace processes.

Terminal cleanup is attempted on each poll for terminal states so a cancelled
non-running issue can still clean up an existing Detent workspace without
waiting for the idle cleanup sweep. The idle sweep interval still controls
non-terminal observed workspace cleanup.

Detent emits cleanup diagnostics in `/api/v1/state` under `events` and in
published telemetry snapshots. A successful cleanup records
`workspace_reap_succeeded` with `worktrees=`, `branches=`, and `processes=`
counts. Cleanup failures record `workspace_reap_failed`, leave the workspace
eligible for a later retry, and keep the diagnostic visible in recent events.
If Detent completes a terminal run but no workspace reaper is configured, it
records `workspace_reap_unverified`.

Detent does not close the GitHub issue when an item is manually cancelled. The
configured tracker state is the source of truth; in label mode, the default
`Cancelled: Done` state map means the repository label is `detent:done`.
Operators remain responsible for closing the GitHub issue with `not_planned` or
another reason when that is the desired repository record.

Detent also leaves linked pull requests open. Operators should close, comment
on, or reuse an open PR explicitly when cancelled work has one.

### Review gate

`Human Review` is the holding state before the merge train. Auto-promotion out
of that state is controlled by the workflow:

- `gate.kind: command` requires a linked open PR, green CI, no P1 automated PR
  review findings, and the configured quiet period. By default it also requires
  a current-head automated GitHub PR review.
- `gate.automated_review: optional` waits for a current-head review until the
  gate deadline, honors any review that arrives, and then promotes without one
  when the remaining checks pass. `off` does not wait; `required` waits and
  defaults to a Human Review timeout.
- `gate.required_status_checks` names release-blocking check runs or commit
  status contexts that must be present, completed, and successful on the current
  PR head.
- `gate.ci_trigger_label` re-fires label-gated required CI after each PR-head
  push and when Merging observes absent current-head checks;
  `gate.ci_trigger_label_stagger_seconds` spaces host-local reapplications by
  `15` seconds by default.
- `gate.ci_failure_action: rework` routes failed or cancelled current-head CI
  from `Human Review` back to `Rework` by default; set
  `gate.ci_failure_action: skip` only when non-green CI should leave the item
  parked.
- `gate.transient_ci_retry_limit` controls how many times Detent reruns
  failed checks with transient runner or infrastructure signals such as
  timeouts, startup failures, OOM kills, or `signal: killed` annotations before
  red CI is treated as a hard failure.
- `gate.validator.enabled: true` runs a validator-agent review before
  auto-promotion; verdicts below `min_score` or with severities in `block_on`
  route the issue to `Rework`.
- `agent.auto_promote.rework_limit` bounds repeated `Human Review` to `Rework`
  loops using persisted lane events; the default is `3`, and `0` is an explicit
  opt-out that leaves the loop unlimited. Positive limits require `Blocked` in a
  configured tracker state list and route the next over-limit rework decision
  there with repeated reasons summarized for handoff.
- `agent.auto_promote.gate_wait_state: source` keeps zero-quiet completed
  active issues in their current lane while CI/check gates are still pending;
  `review` restores the legacy behavior of parking those pending issues in the
  configured review source state. `gate_wait_timeout_seconds` bounds that active
  wait. `gate_wait_timeout_action: merge | human_review` makes the terminal
  action explicit; it defaults to `merge` for optional review and
  `human_review` for required review.
- `gate.kind: human_review` requires a linked open PR plus the configured
  `approval_label` on the issue.

The quiet period resets on observed issue updates, Project status updates,
automated PR review submission, and linked PR activity such as a fresh push to
the PR head.

The quiet period is an intentional quality gate. Tune
`agent.auto_promote.quiet_seconds` when reviewer soak time is too conservative,
but keep the gate explicit so faster merges are a policy choice rather than an
accidental bypass.

A Codex coding session that created the PR is not the same signal as a
Codex/ChatGPT/Claude GitHub PR review. If automated PR review is required and
the PR head changes after a review, request or wait for a fresh automated review
before expecting auto-promotion. `detent doctor` warns when required or optional
review is configured but none of the sampled recent merged PRs has an automated
review, which indicates the review producer may be inactive.

### Set up status

You choose where GitHub status lives; Detent fills in the rest.

- **ProjectV2 mode:** create a GitHub **Projects v2** board (org or user) and point
  `tracker.project_slug` at its node id — the `PVT_…` id from
  `gh project list --owner <org-or-user> --format json`. The board has a default
  **`Status`** field; add a **`Priority`** single-select if you rank work.
- **Detent auto-provisions** the *missing options* inside those fields on first
  run — the `Todo` / `In Progress` / `Rework` / `Merging` / `Done` columns above
  and the `Urgent`…`Low` priorities — so the option names always match your
  `WORKFLOW.md`. It also reorders the known `Status` options to Detent's
  canonical column order: `Backlog`, `Todo`, `In Progress`, `Blocked`,
  `Human Review`, `Rework`, `Merging`, then terminal states. Extra custom
  status options are preserved after the configured Detent states. It provisions
  the options, not the board or the fields themselves, so create the board (and
  the `Priority` field if used) first.
- **Boardless issue-field mode:** create or reuse an organization issue field,
  normally a single-select `Status` field, and make it available to the
  repository. Configure `tracker.github_status_source: issue_field`,
  `tracker.repository: owner/name`, and optionally `tracker.status_field` when
  the field is not named `Status`. Detent's Kanban/dashboard view becomes the
  board surface; no GitHub ProjectV2 board or `tracker.project_slug` is needed.
- **Boardless label mode:** create or reuse repository labels for every
  effective Detent state. Configure `tracker.github_status_source: label`,
  `tracker.repository: owner/name`, and optionally
  `tracker.status_label_prefix` when the prefix is not `detent:`. The label name
  is the prefix plus the slugified mapped state: `In Progress` becomes
  `detent:in-progress`. The issue should have exactly one configured status
  label at a time. Detent's Kanban/dashboard view becomes the board surface; no
  GitHub ProjectV2 board, `tracker.project_slug`, or organization issue field
  is needed.
- **GitHub local-status mode:** configure `tracker.kind: github_local`,
  `tracker.repository: owner/name`, and `tracker.local_sqlite.path`. Do not set
  `tracker.github_status_source`. Import issues explicitly with
  `detent github-local import <project-id> 123,456 --state Todo`; unimported
  issues do not appear on Detent boards. Detent updates local workflow state
  only, while GitHub remains the read-only source for issue text, labels,
  assignees, dependencies, linked PRs, reviews, checks, and rate-limit health.
  The board surfaces divergence such as an upstream-closed issue that is still
  locally active.
- **Blank `Status` values and missing status labels are not `Backlog`.** In the
  current release, an issue with no configured issue-field value or status label
  is not dispatchable through the state machine. Put unready work in the
  `Backlog` option or `detent:backlog` label explicitly. Detent's own GitHub
  issue templates default to `detent:backlog` so new dogfood issues are visible
  to operators but are not dispatchable until triaged to `detent:todo` or
  another active state. Remove that label only when an issue is intentionally
  outside Detent.
- **Label-mode drift is surfaced as cleanup work.** In label mode,
  `detent doctor` and the dashboard report open repository issues with zero
  configured status labels, plus open issues that still carry a configured
  terminal status label such as `detent:done`. Add exactly one configured status
  label to untracked issues, and close or relabel stale-open terminal issues.
- **Detent reads** status, priority, labels, blockers, assignees, and linked
  pull requests from each issue, and **writes back** status transitions and a
  `## Codex Workpad` comment as the agent works, except in `github_local`,
  where those workflow writes are durable local records.

### Kanban Modes

Boardless projects use Detent's own dashboard as the day-to-day board. The
fleet `/kanban` board stays read-only because it is a cross-project observer
surface. A trusted operator project board should default to `integration`
before mutation authorization, so operators can move cards and post comments
from `/projects/<id>/kanban` after the mutation gate and write probes pass.
Skipped pre-mutation write probes are not evidence for `read_only`. Keep
`read_only` for an observer or shared dashboard, an explicit no-writes choice,
or failed post-authorization write probes for ProjectV2 status write in
ProjectV2 mode, issue-field status write in issue-field mode, status-label
update in label mode, or issue/PR comment write for comment forms.

### Migration Notes

Existing users do not need to migrate. Leaving
`tracker.github_status_source` unset keeps ProjectV2 as the source of truth,
and existing `tracker.project_slug` workflows remain valid. This is the
compatibility path when the GitHub Project board is where humans already plan,
rank, and move work.

To switch a repository to boardless issue-field mode, create the organization
issue `Status` field and options, copy current issue statuses from the
ProjectV2 board manually or with a one-off script outside Detent, then change
the workflow to `github_status_source: issue_field` with `repository:
owner/name`. Detent does not automatically migrate ProjectV2 items to issue
fields. After the switch, run `detent doctor --port 0 --allow-write-probes` and
fix field discovery, option discovery, write-probe, comment-write, and
rate-limit checks before dispatching.

To switch a repository to boardless label mode, create status labels matching
the effective workflow states, copy current issue statuses by applying exactly
one configured status label per issue, then change the workflow to
`github_status_source: label` with `repository: owner/name` and
`status_label_prefix: "detent:"`. Detent does not automatically migrate
ProjectV2 items or issue-field values into labels. After the switch, run
`detent doctor --port 0 --allow-write-probes` and fix label mapping, issue reads
by label, write-probe, comment-write, and rate-limit checks before dispatching.

To switch a repository to local-only status mode, copy
`docs/templates/WORKFLOW.github_local.md`, set `tracker.repository`,
`tracker.local_sqlite.path`, workspace paths, and the prompt body, then import
only the issue numbers Detent should see:

```sh
detent github-local import <project-id> 123,456 --state Todo
detent doctor --project <project-id> --port 0
```

This mode does not migrate or mutate GitHub status data. Existing ProjectV2
items, issue fields, labels, and GitHub issue comments stay untouched. If
GitHub closes or transfers an imported issue while local state is still active,
Detent keeps the local row and surfaces the divergence on the board instead of
auto-resolving it.
