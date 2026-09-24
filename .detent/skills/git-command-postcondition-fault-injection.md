---
name: git-command-postcondition-fault-injection
description: "Test Git postconditions and capture stable file identity across filesystem replacements."
when_to_use: "Use when a Git subprocess intermittently leaves partial state or reports success while a required repository or worktree invariant is false. Also use for eager file identity snapshots."
---

# Inject Git postcondition faults

- Resolve the absolute real `git` executable before changing `PATH`, then place a same-named wrapper first on `PATH` for one isolated test. Delegate every command except the exact argument sequence under test to the real executable.
- For a partial-failure case, create only the filesystem evidence Git could leave behind, emit a distinctive stderr marker, and exit nonzero. Assert that the caller preserves the original error and handles the surviving state safely.
- For a false-success case, run the real command first and require its zero exit status, then mutate only the resulting fixture into the recorded bad postcondition before returning zero. This tests the caller's invariant instead of inventing a narrative root cause.
- Keep source, target, wrapper, markers, and logs under test-owned or Detent-provided temporary directories. Quote every embedded path and restore `PATH` through the test framework's scoped environment support.
- Assert temporal behavior as well as the final error: downstream hooks must not run, diagnostics must report observed and expected state, forensic files must survive, and pre-existing foreign repositories must remain untouched.
- Run the regression repeatedly and with the race detector, then remove any production fix that depends on the wrapper itself. The wrapper is fault injection, not a command abstraction to ship.

## Eager File Identity Snapshots

Use this case when a Go replacement test compares os.FileInfo values across a rename and passes on POSIX but reports the same file on Windows.

- Inspect the active Go toolchain's Windows `os` implementation before treating
  `os.FileInfo` from a pathname as a complete identity snapshot. Path-based
  `os.Stat` can defer loading the volume and file index until `os.SameFile`.
  Comparing after replacement can therefore resolve both snapshots to the new
  destination, even though replacement succeeded.
- Open the original file, call `file.Stat()`, and close the handle before the
  mutation. Handle-based stat captures the identity eagerly; closing it avoids
  imposing Windows open-handle replacement restrictions on the writer.
- After replacement, stat the destination and compare identities. Also verify
  the published content; identity alone does not establish content correctness.
- Use this deterministic snapshot ordering instead of timestamp sleeps or
  scheduler-sensitive reader/writer loops. Confirm the fixture rejects an
  in-place rewrite, and run it on Windows as well as a POSIX platform.
