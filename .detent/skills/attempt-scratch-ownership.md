---
name: attempt-scratch-ownership
description: Fence temporary artifacts across worker shutdown, escaped descendants, and resumed attempts.
when_to_use: A subprocess loses its executable, fixture, or coverage directory during restart, or a retry shares a cleanup path with a previous owner.
---

Trace every deleter, including persisted process cleanup, terminal-turn cleanup,
startup retries, and scratch preparation. Correlate process identity and cleanup
timestamps with the failing command; distinguish confirmed deletion attempts
from attribution of individual missing files.

Reproduce with a parent that launches a child in a separate process group and
then exits. Have the child acknowledge readiness through a pipe, hold a lock,
and reexecute a scratch-resident program against a fixture on SIGTERM. Hold its
exit behind another pipe. Assert cleanup waits for exit, the lock is released,
and a resumed attempt's files survive. Use generous failure deadlines and
handshakes for ordering, with unique subprocess coverage output directories.

Verify both registered process-group exit and escaped workspace-process exit
before deleting artifacts or releasing the durable recovery record. A stale
process identity must not authorize a broader workspace reap. Preserve bounded
termination and retain artifacts when exit cannot be verified.

Give each attempt its own path and persist the exact path used by the provider.
Place new attempt directories outside any legacy cleanup root that a recovered
record can recursively remove. Preparation must not delete prior scratch, and
delayed cleanup must target only its original attempt. Check all API callers,
Git exclusions, and fixed-path assertions when changing this contract.
