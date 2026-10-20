# Concepts

[Back to README](../README.md#documentation)

### Tracker

Projects use the Hub's native tracker (`tracker.kind: hub_native`). The Hub
holds issues, lanes, comments, dependencies, the Workpad, and Change Requests.
Enrolled runners claim dispatchable work from the Hub, run agents in local
worktrees, publish Change Requests, and land accepted changes in `Merging`. See
[Cloud onboarding](cloud-onboarding.md) for runner enrollment and
[Hub self-hosting](hub-self-hosting.md) for running your own Hub. GitHub is an
optional integration for native projects; see
[GitHub profiles](github-profiles.md).

### Board States

A new hosted project starts with this workflow:

| State | Meaning |
| --- | --- |
| `Todo` | Ready for a runner to claim and dispatch. |
| `In Progress` | An agent is actively working or continuing work. |
| `Blocked` | Human-blocked work, or an unaccepted version or refused landing with its reason. Does not dispatch. |
| `Human Review` | A Change Request waits for a person when the project sets `review.human: true`. Does not dispatch. |
| `Merging` | The runner that holds the project lands the accepted head on the base branch with plain git. |
| `Done` | Complete. |

Customized workflows may add lanes such as `Backlog` (not dispatchable) or a
terminal `Cancelled`. [Hub API](hub-api.md) describes the default transitions
and the lane requirements for customized workflows.

### Cancellation Lifecycle

Manual `Cancelled` means Detent should stop managing the work item. On the
next poll that observes the cancelled terminal state, Detent revokes the
running worker lease, cancels its context, and terminates its persisted process
group. The same
revocation occurs when an item moves from any configured active state to any
non-active state, including `Blocked` and review lanes. Detent first sends the
graceful termination signal, waits 250 milliseconds by default, escalates to a
forced kill, and waits another 250 milliseconds to verify exit. Process
identity includes the recorded start time so a stale PID or process group is
never signaled.

Regular tracker polling observes a lane change within
`max(polling.interval_ms, 2 minutes)`; a targeted refresh begins revocation as
soon as that refresh is applied. These observation bounds are followed by the
two bounded process-stop waits above. Stop request, result, escalation, and
failure events are exposed in `/api/v1/state` telemetry.

Worker completion is fenced by both the dispatch generation and durable work
attempt ID. Detent also reads the current tracker lane before publishing normal
completion state or artifact-gate fields. A stale generation, a revoked lease,
or a non-active lane rejects the completion without changing the tracker. If
the final tracker read fails, Detent fails closed and stops the worker. Moving
the item back to an active state creates a fresh generation; output from the
prior generation remains invalid.

After the worker has exited, Detent releases the global dispatch slot, clears
configured claim lease state, and records the work attempt as `lane_revoked`.
For a terminal destination, it records the run as completed with that terminal
state and asks the workspace backend to remove the Detent worktree, prune git
worktrees, delete the generated `detent/` branch when safe, and reap workspace
processes.

Terminal cleanup is attempted on each poll for terminal states so a cancelled
non-running issue can still clean up an existing Detent workspace without
waiting for the idle cleanup sweep. The idle sweep interval still controls
non-terminal observed workspace cleanup.

Before deregistering a terminal Git worktree, Detent records a compact
ownership marker under the configured workspace root. The idle sweep uses
these markers to reconcile residual Detent directories without recursively
scanning the root. Reconciliation skips paths for active issues, registered
worktrees, active processes, and paths whose ownership marker cannot be
validated against the configured source repository.

Detent emits cleanup diagnostics in `/api/v1/state` and `/health` under
`workspace_cleanup_failures`. The field reports the affected path count and
last error without calculating directory sizes. A successful cleanup records
`workspace_reap_succeeded` with `worktrees=`, `branches=`, and `processes=`
counts. Cleanup failures record `workspace_reap_failed`, preserve the
ownership marker for a later retry, and keep the diagnostic visible until
reconciliation succeeds.
If Detent completes a terminal run but no workspace reaper is configured, it
records `workspace_reap_unverified`.

### Review gate

By default `review.human` is false, so nobody has to review a run. A run that
committed a change publishes a version the policy already accepts, and the
runner's orchestrator moves it straight to `Merging`. A run that committed
nothing moves to `Done`. With `review.human: true`, the Change Request waits in
`Human Review` (the default `auto_promote.source_state`) until a person
approves it, which moves it to `Merging`, or requests changes, which sends it
back to `In Progress`. See [Cloud onboarding](cloud-onboarding.md) and
[native review](native-review.md).
