---
name: worker-process-lifecycle
description: Preserve Detent ownership of paid worker process trees across forced shutdowns and restarts.
when_to_use: Use when changing Codex or Claude process launch, cancellation, shutdown, startup recovery, or session persistence.
---

# Worker process lifecycle

- Configure every paid worker command through `internal/procgroup` before `Start` so its descendants share a dedicated process group while remaining in Detent's service cgroup.
- Inspect the launched process and persist PID, PGID, and OS start time on the Detent session before provider work begins.
- Validate PID and start time before signaling a recovered process record so PID reuse cannot terminate an unrelated process.
- On forced shutdown or startup recovery, send SIGTERM to the process group, wait for the bounded grace period, then send SIGKILL and verify the group exited.
- Give each worker turn a Detent-owned temporary directory inside its workspace through `TMPDIR`, `TMP`, and `TEMP`. Remove it only after the provider process tree exits, remediate read-only generated-cache permissions before retrying deletion, and never infer ownership by scanning arbitrary host temp-directory prefixes.
- Log one INFO lifecycle decision with the issue identifier for every persisted worker record and record the reap outcome.
- Test launch journaling, stale-identity refusal, process-group escalation, startup recovery, drain-timeout cleanup, provider temp-environment propagation, and read-only scratch cleanup without signaling the live Detent instance.
