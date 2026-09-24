---
name: join-canceled-startup-rollback
description: "Roll back canceled startup and attribute subprocess startup failures separately from cleanup errors."
when_to_use: "Use when shutdown returns while startup work still writes files, uses a closed store, or recreates temporary directories after cancellation. Also use for startup failure cleanup attribution."
---

# Join canceled startup rollback

1. Reproduce under the pressure mode that exposes the leak, such as coverage repetitions or the race suite. Record the resource touched after shutdown returns.
2. Trace the writer to its owning worker and follow startup failure rollback. Check whether rollback passes an already-canceled context to stop, wait, or close operations.
3. Preserve cancellation as the startup result, but derive cleanup with a bounded timeout over `context.WithoutCancel(ctx)`. Stop and join every started worker, close owned resources, and only remove them from registries after cleanup completes; retain incomplete resources so their owner can retry closure.
4. Add channel-driven regressions: verify normal rollback cannot finish before a blocked worker is released, then verify stalled rollback respects its cleanup bound without losing ownership of incomplete resources.
5. Rerun the focused regression under `-race`, then repeat the original pressure reproduction with coverage.

- When multiple workers publish one result each, include every worker in one
  owner-held wait group and cancel before waiting on every return path. Size the
  result buffer for all producers so an early error cannot strand a producer
  after the owner stops reading. Preserve worker-specific shutdown bounds.
- A helper that reads until one named result appears may discard other workers'
  completion messages. Do not call it sequentially to join several workers;
  use the shared wait group or retain all observed completion results.

## Separate startup failure from cleanup

Use this case when startup and transport-close deadlines are joined, or a killed process is being interpreted as the cause of a startup stall.

- Correlate recorded stage-start, failure, termination and process-exit times. An exit observed after cleanup starts cannot by itself establish the original cause.
- Snapshot safe process state before cleanup and again afterward. Use cumulative message counts, receive queue depth, decode status and stderr byte counts; never capture request parameters, prompts, credentials or tool payloads in generic diagnostics.
- Record process exit when wait returns, separately from completion of pipe drainers. Distinguish a termination request from confirmed transport completion.
- Classify retry behavior using the primary typed operation error. Keep cleanup errors in the chain for inspection, but do not let a cleanup deadline replace a provider rejection, EOF or other primary failure. Preserve explicit operator and parent cancellation precedence.
- Reproduce with a helper that acknowledges initialization, consumes the startup request, publishes an independent readiness signal and stops responding. Expire startup and close contexts explicitly, then assert process reaping and drainer completion. Use real time only as a deadlock guard.
- Test unrelated notifications against an absolute response deadline; a renewable per-message timeout does not bound startup.
- Preserve uncertainty when historical evidence lacks process samples or remote response details. Document a bounded recurrence capture and cause-specific remedies rather than inferring deadlock, OOM or outage from a timeout alone.
