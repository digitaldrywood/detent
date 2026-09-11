---
name: startup-failure-cleanup-attribution
description: Separate a subprocess startup fault from evidence and errors produced by cleanup.
when_to_use: Use when startup and transport-close deadlines are joined, or a killed process is being interpreted as the cause of a startup stall.
---

# Separate startup failure from cleanup

- Correlate recorded stage-start, failure, termination and process-exit times. An exit observed after cleanup starts cannot by itself establish the original cause.
- Snapshot safe process state before cleanup and again afterward. Use cumulative message counts, receive queue depth, decode status and stderr byte counts; never capture request parameters, prompts, credentials or tool payloads in generic diagnostics.
- Record process exit when wait returns, separately from completion of pipe drainers. Distinguish a termination request from confirmed transport completion.
- Classify retry behavior using the primary typed operation error. Keep cleanup errors in the chain for inspection, but do not let a cleanup deadline replace a provider rejection, EOF or other primary failure. Preserve explicit operator and parent cancellation precedence.
- Reproduce with a helper that acknowledges initialization, consumes the startup request, publishes an independent readiness signal and stops responding. Expire startup and close contexts explicitly, then assert process reaping and drainer completion. Use real time only as a deadlock guard.
- Test unrelated notifications against an absolute response deadline; a renewable per-message timeout does not bound startup.
- Preserve uncertainty when historical evidence lacks process samples or remote response details. Document a bounded recurrence capture and cause-specific remedies rather than inferring deadlock, OOM or outage from a timeout alone.
