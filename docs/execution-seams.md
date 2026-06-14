# Execution Seams

Detent's orchestration loop is mostly domain-neutral: it reads board state,
dispatches work, tracks retries and budgets, and serializes the `Merging` lane.
The execution edge still carries these code/git/PR assumptions.

## Now Pluggable

### Agent Backend

- `internal/runner` owns `AgentBackend`, backend factories, and task routing.
- `internal/config` exposes `agents.backends` and `agents.routes`.
- Domain, label, priority, assignee, author, or ProjectV2 field based routing
  is handled by the existing selector and router.

### Validation Gate

- `internal/config` exposes top-level `gate`.
- `internal/gate` selects and evaluates gate behavior.
- `gate.kind: command` is the default and keeps the code workflow contract:
  run `gate.run` (`make check` by default) before Human Review, then require
  green CI and clean automated review before promotion.
- `gate.kind: command` with `require_automated_review: false` keeps the
  command, linked PR, green CI, no-P1, and quiet-period checks but does not
  require an automated GitHub PR review to exist before promotion.
- The quiet period resets on observed issue updates, Project status updates,
  automated PR review submission, and linked PR activity such as a fresh push
  to the PR head.
- `gate.kind: human_review` requires a PR plus `gate.approval_label`
  (`human-approved` by default) before promotion.
- `internal/runner/prompt.go` renders gate variables and appends gate
  instructions so `Todo`, `Rework`, and `Merging` agents see the configured
  validation contract.
- `internal/orchestrator/autopromote.go` delegates the pass/wait/rework decision
  to the configured gate while preserving the surrounding opt-out and label
  policy.

### Merge Waits

- The quiet window before auto-promotion is an intentional quality gate.
- The serialized `Merging` lane should run a focused rebase/smoke gate when the
  PR already passed the pre-review gate, the rebase is clean, and no source
  files change during the rebase.
- `Merging` must still run the full configured gate after merge-agent edits,
  conflict resolution, stale validation, or unknown validation state.
- Current-head CI waiting should use REST check-run polling with backoff and
  clear slow-check status rather than GraphQL-heavy PR polling loops.
- Merge telemetry should report quiet-window wait, local merge-gate duration,
  current-head PR CI duration, slow check names, and whether post-merge `main`
  CI is still running.
- Duplicate full local validation after a source-clean rebase, uncached tool
  install, noisy polling, and post-merge work that is not part of the merge
  decision are optimization targets.
- The CI lint job keeps the project-pinned golangci-lint version and caches its
  binary so cache hits avoid a per-run `go install` without changing lint
  coverage.
- Moving `GoReleaser Snapshot` off every PR or making it path-based is a
  release-policy decision because it trades release-package coverage for merge
  latency.

## Still Git/PR Coupled

### Workspace

- `internal/workspace` currently provides `local_git` only.
- `LocalGit` creates git worktrees, derives `detent/<issue-key>` branches,
  runs hooks, computes git diff stats, and removes worktrees.
- `internal/runner` depends on that backend interface, but no non-git backend is
  implemented yet.

### Deliverable

- `internal/connector/github` discovers pull requests by issue branch prefix and
  reads PR state, CI status, check-run timing, slow checks, running checks, and
  automated review state.
- `connector.Issue` stores `PRNumber` and `PullRequest` directly.
- The dashboard and telemetry models render a PR pipeline for `Human Review`,
  `Merging`, and terminal states.
- `internal/runner/prompt.go` appends a GitHub `Fixes #N` PR instruction when it
  can parse a GitHub issue identifier.

## Follow-Up Surface

Non-git or non-PR deliverables should start with a small deliverable backend
interface that can report:

- workspace identity and artifact location,
- review target URL or equivalent,
- validation status,
- final integration or publish result.

The default implementation should remain git worktree to PR so code, docs,
design assets, notebooks, and CSV models keep working as files-in-repo
deliverables.
