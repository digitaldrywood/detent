---
name: resilient-file-watch-reconciliation
description: Keep file-triggered runtime reconciliation reliable when parent directories appear late or filesystem events are missed.
when_to_use: Use when a long-running Detent watcher must react to atomic replacement, credentials or config paths may not exist at startup, or focused watcher tests become flaky under concurrent race suites.
---

# Resilient file-watch reconciliation

- Attach the initial filesystem watch synchronously before reporting readiness so an immediate write cannot race watcher startup.
- If the watched directory is absent, retain ownership of the target and retry attachment on a bounded interval instead of dropping it.
- Capture a comparable file stamp before returning from successful initial setup. Include modification time, size, and mode when content does not need to be read.
- Keep filesystem events as the fast path, but reconcile the stamp periodically. OS watcher delivery can miss an atomic rename under load.
- Route event-driven and periodic observations through the same stamp comparison so one replacement produces one notification.
- When attachment succeeds after retries, compare the current stamp with the last observation and notify if the file appeared or changed while detached.
- Stop retry timers, poll timers, and watcher goroutines on context cancellation, and expose a completion channel so the owner can wait for shutdown.
- Inject short retry and poll intervals in tests. Cover initial replacement, an absent parent followed by file creation, duplicate suppression, cancellation, and a race-enabled repetition.
