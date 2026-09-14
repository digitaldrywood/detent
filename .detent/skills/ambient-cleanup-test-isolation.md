---
name: ambient-cleanup-test-isolation
description: Isolate provider artifact roots when adding filesystem retention to startup or reaper paths exercised by integration tests.
when_to_use: A new cleanup hook uses a test runtime database but resolves artifact paths from the real process environment, or a filesystem diagnostic consumes an otherwise mocked test's deadline.
---

Before exercising a new deletion hook through startup or a reaper, inspect the
whole integration-test wiring, including `TestMain`. A temporary runtime store
alone is insufficient: it has no active references to the developer's real
provider artifacts and can therefore incorrectly authorize their removal.

Give the test process a disposable provider home through the same environment
variable production resolves (for example, `CODEX_HOME`). Create it under the
provided temporary directory, preserve tests' explicit fixture overrides, and
remove it before `os.Exit`; deferred cleanup does not run after `os.Exit`.
Inspect explicit provider-home assignments in test commands as well.

For read-only diagnostics, use the existing dependency-injection boundary so a
mocked doctor run does not scan the host's real artifact tree. Test the real
scanner separately against bounded fixture trees. If a previously stable
short-deadline test fails after adding a diagnostic, reproduce it alone and
check ambient filesystem work before increasing its timeout.

Validate the cleanup itself with fixtures for ownership, age boundaries, active
references, and shared files. Then exercise the lifecycle hook with the isolated
provider home. A successful test does not establish that earlier unisolated
runs left production artifacts unchanged; avoid claiming that without evidence.
