---
name: resilient-file-watch-reconciliation
description: Keep file-triggered runtime reconciliation reliable when parent directories appear late or filesystem events are missed.
when_to_use: Use when a long-running Detent watcher must react to atomic replacement, credentials or config paths may not exist at startup, or focused watcher tests become flaky under concurrent race suites.
---

# Resilient file-watch reconciliation

- Attach the initial filesystem watch synchronously before reporting readiness so an immediate write cannot race watcher startup.
- If the watched directory is absent, retain ownership of the target and retry attachment on a bounded interval instead of dropping it.
- Capture a comparable file stamp before returning from successful initial setup. Metadata alone cannot distinguish same-size rewrites or timestamp-preserving replacements. If stamp equality suppresses matched events, include a content digest for regular config files; otherwise preserve the event-triggered reload.
- Keep filesystem events as the fast path, but reconcile the stamp periodically. OS watcher delivery can miss an atomic rename under load.
- Route event-driven and periodic observations through the same stamp comparison so one replacement produces one notification.
- When attachment succeeds after retries, compare the current stamp with the last observation and notify if the file appeared or changed while detached.
- Stop retry timers, poll timers, and watcher goroutines on context cancellation, and expose a completion channel so the owner can wait for shutdown.
- Inject short retry and poll intervals in tests. Cover initial replacement, an absent parent followed by file creation, duplicate suppression, cancellation, and a race-enabled repetition.
- Reproduce metadata collisions by writing different equal-length content and restoring a fixed modification time with `os.Chtimes`. Assert the metadata is equal, then test both event and poll delivery with controlled debounce expiry.
- Compare watched symlink target directories after `filepath.EvalSymlinks`. Temporary paths can contain aliases on macOS and Windows; reproduce this locally with a temporary directory reached through a symlink.
