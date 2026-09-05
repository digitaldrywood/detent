# Dependency workflows

[Back to README](../README.md#documentation)

Use Todo for ordinary dependency waiting and preserve the phase of existing PR
work. Human prerequisites remain non-dispatchable in Backlog.

## Human prerequisites

Human-owned work has a `human-owned` label or an issue-body `detent-human`
block. Both exclude it from admission and dispatch, including blocker automatic
promotion. A malformed block still excludes dispatch. Review opt-outs are not
human ownership markers. Keep human tasks in **Backlog** until the human
completes them. Tracking epics (`epic` label or `Epic:` title) also never execute;
only independently scoped children do.

```detent-human
schema: 1
key: test-account-authentication
owner: Account administrator
action: Enable authentication for the test tenant
completion_criteria: A test-account sign-in succeeds in the test tenant
approval_constraint: Production publishing still requires explicit approval
```

Choose a stable repository-scoped key for the smallest required milestone.
The human records `completion_evidence` in this block when its criteria are
satisfied, then closes the issue. A closed or Done human issue without that
valid contract and evidence remains an unresolved prerequisite. Reopening it
restores the wait. Record a safe verification summary, never a credential.

Workers on a writable GitHub tracker receive `ensure_human_prerequisite`, scoped
to their assigned issue. Pass `title`, `task` (the fields above), and optionally
`existing_identifier`. Reuse the existing key and exact milestone contract.
The tracker owner serializes concurrent requests, discovers existing markers
from the repository issue listing rather than the eventually indexed search,
and reuses the durable issue after restart. It creates the human issue through
the configured tracker status source in Backlog, verifies the dependency graph,
adds a native relation when supported, and appends an idempotent
`Depends on: owner/repo#123` line while preserving the remaining body and edges.
Partial writes are repaired on retry. Conflicting contracts, duplicates,
self-dependencies, cycles, malformed graph references, and unverifiable graphs
fail explicitly. Route every worker request through the same tracker owner;
independent services or direct concurrent `gh issue create` calls do not share
its serialization. The tool is not offered by read-only trackers.

The mutation stays inside the dependent repository. It neither reads nor copies
private evidence from another repository. Supply a safe milestone description;
publication protection also rejects protected cross-project details. For a
cross-repository prerequisite, have the owning human establish a safe reference
through the authorized tracker path without copying credentials or private
project evidence across visibility boundaries.

Keep queued executable children in **Todo**, with a visible dependency wait.
For an existing PR, retain its deliverable and **Rework** or **Merging** phase.
A worker can declare a structured dependency wait with `status: blocked` and
`human_action: null` without moving the issue to the Blocked lane; Detent
persists the deferral, releases the claim, and restores the resumable lane.
The structured status describes the wait, not the board lane. **Blocked** is
reserved for delivery failures, repeated-failure or spend parks, and unresolved
operational problems. Dependency closure never acknowledges those independent
parks. The board and scheduler explanation identify a pending human prerequisite
and retain the requirement for completion evidence even when it is closed.

Do not add a dependency when stub-testable implementation, independent
preparation, or an authorized fallback can satisfy the remaining scope. Finish
those tasks first. Do not add dependencies to already completed software. If
only account activation is required, use that milestone, not an entire release
or tracking epic.

### Migrate prose-only blockers

1. Inspect the remaining scope, PR, native edges, Workpad, and current breaker
   evidence. Separate actual human prerequisites from implementation work and
   independently parked delivery failures.
2. Reuse a focused human issue or use the worker tool to create one. For an
   existing issue, pass `existing_identifier` and its exact human milestone to
   add a marker without replacing its unrelated content. Preserve approval
   constraints and move the human issue to Backlog through the tracker.
3. Add each dependent's native relation and issue-body `Depends on:` line.
   Workpad prose alone is insufficient. Preserve all existing dependencies.
4. Put unstarted executable children in Todo and restore the prior PR phase
   for started work. Leave independent breaker parks untouched; migration is
   not permission to acknowledge a park.

### External-action authorization

Dependency readiness is scheduling evidence only. A human issue's closure,
completion evidence, or label is never authorization to publish, deploy, spend,
or perform destructive operations. Existing approval constraints and the
specific action's authorization gate remain in force. A worker must not assert
human completion evidence through the prerequisite-creation tool.

## Dependency configuration

Workpad blockers may declare live evidence directly. Each typed blocker names
an `owner`, a `predicate`, and a `recheck_interval` or `expires_at`. Detent
re-evaluates these records on every blocked-lane tick and clears the park when
all predicates are false. The supported predicate types are `issue_state`,
`pull_request_state`, `check_presence`, `budget_capacity`, and
`config_fingerprint`.

```detent-status
schema: 1
status: blocked
blockers:
  - ref: "owner/repo#123"
    reason: "waiting for the dependency to close"
    owner: orchestrator
    predicate:
      type: issue_state
      states: [open]
    recheck_interval: tick
human_action: null
```

Free-text blocker entries remain valid for compatibility, but Detent marks
them unverifiable, surfaces their owner and age, and never clears them
automatically. An expired predicate that still holds is also unverifiable;
orchestrator-owned unverifiable parks are escalated for operator attention.

- **Keep the issue in `Todo`.** Add a machine-readable dependency line such as
  `Depends on: #123`, `Blocked by: owner/repo#123`, or
  `Depends on: https://github.com/owner/repo/issues/123`. Detent keeps the
  issue out of dispatch while any referenced blocker is non-terminal, then
  dispatches it normally after blockers clear. This is the default behavior and
  needs no extra configuration.
- **Legacy dependency waiting in `Blocked`.** Enable `tracker.dependency_auto_unblock` when
  your team wants dependency-waiting issues to sit in a waiting column. Detent
  only moves issues that have explicit `Depends on:` or `Blocked by:` references.
  When all blockers are terminal, closed, or have a merged linked PR under the
  configured `readiness` rule, Detent updates the configured GitHub status
  source to `target_state` and posts an audit comment. Without
  `tracker.dependency_auto_unblock.enabled: true`, a `Blocked` issue is observed
  for display but will not be moved back to `Todo`. Human blockers without
  explicit dependency references stay blocked.
- **Queue fixable blockers automatically.** Enable
  `tracker.blocker_auto_promote` alongside dependency auto-unblock when a
  dependency-waiting issue should pull same-repository blockers out of inactive
  states such as `Backlog`, `Blocked`, or `Human Review`. Detent promotes only
  resolved, same-repository blockers to the configured `target_state`, respects
  current local agent capacity, and posts an audit comment on the promoted
  blocker. Agents that move work to `Blocked` because of another tracked issue
  or pull request must ensure the issue body contains a machine-readable
  `Blocked by:` or `Depends on:` line; Workpad mentions alone are not a durable
  dependency contract.
- **Recover explicit PR-maintenance parks.** Enable `tracker.blocked_recovery`
  only when structured PR-maintenance parks should move work to the configured
  repair lane. The worker that intentionally creates such a park must set
  `reason_code: merge_conflict`, `stale_base`, or `missing_current_head_ci` in
  its blocked `detent-status` block; Detent persists that code on the lane
  entry. Recovery also requires the corresponding PR condition to still hold
  and a new diff-fingerprint/base-OID pair. Issue descriptions, manual status
  moves, and other prose do not authorize recovery. Keep this disabled on
  boards where `Blocked` is also used for deliberate operator parking.

Before you dispatch anything, run **`detent doctor --allow-write-probes`** after
mutation authorization — it checks config
resolution, the database, the `codex` binary, GitHub auth mode, configured
tracker access, repository issue/PR access, required write proofs, rate-limit
visibility, git, and the server port. A clean pre-start `doctor` clears
Detent's direct preflight.
When a running Detent process already owns the configured port,
`detent doctor --port 0 --allow-write-probes` validates the config, database,
tools, token, and write proofs without treating the live listener as a blocker;
pair it with `/health` on the actual service before dispatching more work. Do
not dispatch from a failed doctor run unless the only failure is that expected
live-port collision and `/health` is green. If Detent runs under a systemd user
service, also verify the
service PATH resolves every command used by project hooks and validation gates;
`doctor` checks Detent's direct dependencies, not repo-specific bootstrap tools.
The onboarding runbook includes the service-context check.

Keep systemd's default `KillMode=control-group` for the Detent service. Detent
starts each Codex and Claude worker in its own process group without leaving the
service cgroup, so its persisted worker registry can terminate stale process
groups on shutdown or startup while systemd remains the final cleanup backstop.
Do not wrap worker commands in launchers that double-fork or explicitly move
children into another cgroup.

On shared Linux hosts, add `MemoryHigh` and `MemoryMax` to `detent.service` as
defense in depth. Size both for the aggregate orchestrator and worker footprint,
leave capacity for unrelated workloads, and keep `MemoryMax` above
`MemoryHigh`. For example, a 32 GiB host reserved partly for other work could
use this user-service drop-in as a starting point:

```ini
[Service]
MemoryHigh=24G
MemoryMax=28G
```

Create the drop-in with `systemctl --user edit detent.service`, then run
`systemctl --user daemon-reload` and restart the service during an approved
maintenance window. Verify the effective values with
`systemctl --user show detent.service -p MemoryHigh -p MemoryMax`. These cgroup
limits protect the host if Detent's per-agent RSS ceiling fails; they do not
replace the per-agent ceiling or memory, IO, and CPU pressure admission
controls.
