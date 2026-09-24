---
name: worker-process-lifecycle
aliases:
  - subprocess-lifecycle-fixture-testing
  - subprocess-pressure-tracing
description: "Diagnose worker process ownership, subprocess fixture lifecycle, and command fan-out under pressure."
when_to_use: "Use when changing Codex or Claude process launch, completion, cancellation, shutdown, startup recovery, retry scheduling, or session persistence. Also use for subprocess lifecycle fixture testing, subprocess pressure tracing."
---

# Worker process lifecycle

- Configure every paid worker command through `internal/procgroup` before `Start` so its descendants share a dedicated process group while remaining in Detent's service cgroup.
- Inspect the launched process and persist PID, PGID, and OS start time on the Detent session before provider work begins.
- Observe provider-parent exit concurrently with stdout and stderr consumption. Use Detent-owned pipes so `Wait` cannot be held open by descendants that inherited output descriptors, then reap the process group before finishing the drains.
- Validate PID and start time before signaling a recovered process record so PID reuse cannot terminate an unrelated process.
- When a process-group signal returns `EPERM`, inspect the group before classifying the result. Accept visible zombie-only members as exited. For an empty snapshot, also require a signal-0 probe to return `ESRCH`, because restricted process visibility can hide a live foreign group. Preserve the original error when a live member remains, existence cannot be disproved, or inspection fails.
- Reproduce ambiguous process-group signal races by injecting the signal, inspection, and existence-probe boundaries; cover exited, zombie-only, visible and hidden live unauthorized, and inspection-failure results without relying on the host kernel to hit the race.
- On forced shutdown or startup recovery, send SIGTERM to the process group, wait for the bounded grace period, then send SIGKILL and verify the group exited.
- Give each worker turn a Detent-owned temporary directory inside its workspace through `TMPDIR`, `TMP`, and `TEMP`. Remove it only after the provider process tree exits, remediate read-only generated-cache permissions before retrying deletion, and never infer ownership by scanning arbitrary host temp-directory prefixes.
- Treat run completion as an ownership boundary: remove the running session and release its persisted and in-memory claim before scheduling a continuation. Represent retry pacing with retry state, and make ordinary dispatch honor a pending retry without relying on a live claim.
- Log one INFO lifecycle decision with the issue identifier for every persisted worker record and record the reap outcome.
- Test launch journaling, dead-parent detection with inherited output descriptors, status-less active-state completion, claim release with a pending continuation, stale-identity refusal, process-group escalation, startup recovery, drain-timeout cleanup, provider temp-environment propagation, and read-only scratch cleanup without signaling the live Detent instance.

## Verify subprocess lifecycle fixtures

Use this case when subprocess cancellation or parent-exit tests fail readiness deadlines, leak helpers after failures, or need to preserve strict child-termination assertions under scheduler pressure.

- Retrieve the original failing job log, not only the latest attempt of a rerun. Separate observed errors from hypotheses about scheduling or production cleanup.
- Reproduce startup sensitivity with a controlled delay before readiness publication. Keep the real process-group setup and lifecycle implementation intact. A delayed-start reproduction proves a fixture timing assumption, not the cause of an untraced historical incident.
- Wait for backend startup acknowledgement as well as child readiness before cancellation. Hold parent-exit fixtures on a separate control pipe or stdin command until the test confirms the child is alive, then release the parent. Preserve inherited output descriptors when they are the behavior under test.
- Use bounded OS deadlock guards around readiness and completion; retain exact cancellation, exit-status, and descendant-termination assertions. Do not substitute fake time for external process scheduling.
- Register cancellation and joining before waiting for readiness, with cleanup ordered before temporary directory removal. Once a child PID is known, register independent emergency termination that runs only after the assertions. Isolate coverage output for Go test-binary helpers.
- Use temporary Go overlays to verify negative controls without editing the production worktree: restore the old readiness guard to prove the regression fails; omit production post-exit cleanup to prove the lifecycle assertion still fails. Independently check the recorded child PID after each failing test exits to verify emergency fixture cleanup removed it.
- Repeat focused race and coverage tests for each affected package, and verify Linux execution with a proper init process or hosted runner. Record what each negative control establishes and leave unsupported historical causes explicitly unresolved.

## Trace subprocess pressure

Use this case when focused tests pass but concurrent or race suites intermittently crash or hang an external command.

- Put a temporary executable with the same name as the target command first on `PATH`. Keep it under the Detent-provided temporary directory and have it append PID, timestamp, and arguments to a trace file before `exec` replaces it with the absolute real executable.
- Run the smallest concurrent package set that has exhibited the failure. Do not add retries or sleeps; preserve the original test scheduling and race settings.
- Group trace entries by normalized arguments and caller scenario. Separate required integration commands from accidental discovery against fake workspaces or ancestor repositories.
- Correlate the trace with the full failing log or stack. A failure in a later test-helper command after the production call succeeded indicates environmental process instability, not necessarily invalid fixture lifecycle.
- Prefer eliminating launches: reject non-target paths before spawning, bound repository discovery, and combine read-only queries into one command when the tool supports it.
- If required fan-out remains unsafe across concurrent worktrees, serialize only the top-level validation gate with a crash-safe OS lock scoped to the Git common directory. Bound the wait, share no outputs through the lock location, and keep temp files, generated assets, binaries, and coverage profiles worktree-local.
- Repeat the same traced command after the change and record before/after launch counts. Then remove the tracing wrapper from `PATH` and run focused race repetitions.
- Keep real integration coverage for the external tool's observable behavior; replace only redundant setup or expectation commands with direct fixture facts when those facts are independently known.
