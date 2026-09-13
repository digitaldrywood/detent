---
name: darwin-zombie-process-group-cancellation
description: Diagnose and test macOS process-group cancellation when signaling a zombie-only group returns EPERM even though the command completed successfully.
when_to_use: Use when a Go subprocess cancellation path reports operation not permitted or exec cancel errors after successful output, especially when process snapshots contain only Z-state members.
---

# Handle Darwin zombie-only process groups

- Reproduce the kernel behavior separately from the test harness. Start a short-lived command with `SysProcAttr.Setpgid`, delay `Wait`, confirm `ps` reports only `Z`-prefixed states, signal the negative process-group ID, and record both the signal error and final wait result. On Darwin, a zombie-only group can return `EPERM` while `Wait` succeeds.
- Keep the snapshot result structured. Distinguish a successful snapshot from an unavailable one, and track whether any non-zombie member was observed. Do not infer that the group exited when process-table collection failed.
- Treat a successful empty or zombie-only snapshot as already done. For Go `exec.Cmd.Cancel`, return `os.ErrProcessDone` so `Wait` can preserve a successful cancellation race.
- Continue signaling groups with live members or unavailable snapshots. Map `ESRCH` to `os.ErrProcessDone`, but preserve every other signal error for diagnostics.
- Exercise the decision as a pure stdlib table-driven test with an injected signal function. Cover unavailable, live, empty, zombie-only, `ESRCH`, and another failure such as `EPERM`; assert the negative group ID and signal as well as whether signaling occurred.
- Keep integration coverage for both race orders: cancellation while a live member exists must preserve the context cause, while completion before a conclusive non-live snapshot must preserve success. Coordinate with channels or file descriptors rather than sleeps.
- Preserve bounded evidence collection and cleanup. Snapshot only process IDs, parent/group IDs, states, and executable names; do not collect arguments or environment values unless the incident explicitly requires them.
- Run focused affected-platform tests and the repository validation gate. Run race validation only where the project protocol permits it, and distinguish a newly reproduced cancellation defect from any earlier uninstrumented timeout.
