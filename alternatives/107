# How Detent delivers a change

[Back to README](../README.md#documentation)

This page describes the path a native Cloud issue takes from dispatch to a landed commit, and which project settings control each step. Each project chooses its own validation commands, review requirements and workflow; Detent's own repository settings are not product defaults.

## Workspaces

Runners are stateless. Every job starts in a fresh workspace from an exact commit or a Hub-stored source bundle, and work that must survive is published to the Hub. A runner depends only on its install configuration, so nothing a later job needs is kept on one machine.

Project setup runs from repository files:

- `hooks.runner_setup` runs a repository-relative script once per runner, project and script content during runner preparation, before any work is claimed. Failures are attributed to that runner and project and block its claims without consuming an issue's attempt allowance.
- `hooks.after_create` and `hooks.before_run` prepare each worktree or attempt.
- Workspace environment, cache sharing and cleanup are set under `workspace`.

See [hooks and workspace configuration](config.md#operations-budgets-and-observability).

## Dispatch

The Hub offers dispatchable work (`Todo`, `In Progress`, `Merging` in the default workflow) to runners with project access and free capacity. Enrolled runners receive ready work ordered by project rank, then issue priority, aging boost and landing preference, then creation time, identifier and ID; other claims order by priority first ([claim order](hub-api.md#organization-runner-scope-and-claim-order)). Hub dependency relations hold a `Todo` item until its blockers are terminal ([dependency workflows](dependency-workflows.md)), and the project's issue contract holds items missing required sections ([admission criteria](admission.md)). Model, backend and effort come from the Cloud organization and project model selection; a per-issue `detent-agent` block can set `effort`, clamped to each stage's ceiling.

Each runner's [effective capacity](runner-capacity.md) bounds its concurrent agent sessions, and host pressure admission (CPU, memory, IO) holds new dispatches without stopping running ones. Runner capacity limits agent sessions, not every process those sessions start.

## Implementation and checkpoints

The implementation agent stages its finished changes and reports its outcome; the runner signs, commits, rebases and publishes the immutable Change under the active lease. Native runs never push, open pull requests or call the GitHub API unless the project explicitly sets `worker.github_token`. A duration, turn or no-progress stop requests a bounded recovery checkpoint, and `agent.checkpoint_interval_ms` enables periodic checkpoints. Checkpoints never satisfy completion, review or CI gates.

## Validation and review

After the agent finishes, the runner publishes an immutable Change version and runs the configured stages against it:

| Stage | Setting | Outcome |
| --- | --- | --- |
| Command gate | `gate.run`, `gate.required_status_checks` | The project's validation command; for native projects the local gate is the CI. |
| Implementation review | Native review | An independent read-only review in a fresh session, bound to the version. |
| Acceptance validation | `gate.validator` | Evidence for each acceptance criterion; `gate.validator.unverified_criteria` (default `rework`) decides whether gaps return the work. |
| Security audit | `gate.security_audit.enabled` (default `true`) | A review of the exact base and head; confirmed P1/P2 findings return the work. |
| Human review | `review.human` (default `false`) | When true, the Change waits in `Human Review` for a person to approve or request changes. |

A `rework` verdict returns the issue to the project's rework lane on the same source. Missing review evidence defers completion and retries with backoff; it never moves the issue to `Blocked`. See [native review](native-review.md) and the [per-change security audit](config.md#per-change-security-audit).

## Landing

An accepted version moves to `Merging`, where the runner that holds the project lands the head on the base branch with plain git and the Hub finishes the issue in `Done`. `gate.landing_mode` accepts only `per_landing`: each landing is validated individually. A rebase, an earlier batch member or a concurrent base update requires fresh evidence, including another security audit, for the resulting commit. `deliverable.github_pull_request: true` selects GitHub pull request delivery instead; GitHub branch protection and required reviews then remain authoritative. Release automation (`release.enabled`) is available only for `tracker.kind: github` projects.

## Failures and retries

Machine failures retry in a dispatchable lane through one owner with growing attempts and capped, jittered backoff. A retry resumes at the first step whose receipt is no longer valid for the current source, policy and review state. Infrastructure failures (backend startup, protocol errors, workspace hooks) are attributed to the runner instance, not the issue. `Blocked` holds only explicit human questions or permission requests and operator stops. Monthly budgets and per-issue spend limits hold dispatch through the budget policy described in [hosted allowances](hosted-allowances.md#monthly-budgets).

## Explaining state

The MCP `explain_item`, `work_runs`, `work_history` and `work_attempt_receipt` tools, and the corresponding API reads, report an item's lane, eligibility, attempts, receipts and the reasons a runner is not claiming it. Throughput and regressions require recorded history rather than a single snapshot; see [diagnosis](diagnosis.md).
