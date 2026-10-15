---
name: pollable-pty-shutdown
description: Diagnose PTY shutdown stalls caused by deferred shell signals and blocking master reads.
when_to_use: Use when terminal close consumes its kill grace although a shell has received SIGHUP, or closing a PTY does not release its read goroutine.
---

# Diagnose PTY shutdown

- Distinguish shell completion from group disappearance. Record the test-owned shell's PID, group, state, signal-probe result, and completion channel when close stalls; a live shell is not an empty-group probe failure.
- Require executed readiness output. A PTY echoes input before the shell runs it; split a marker across `printf` arguments so the complete marker appears only in output.
- Inspect the PTY library's allocation and Go's file-poller ownership. A blocking `os.NewFile` can retain its descriptor while a syscall read is active, even after `Close` returns. Setting nonblocking mode after wrapping does not register it with Go's poller.
- Before starting stream goroutines, duplicate the master with close-on-exec, set the duplicate nonblocking, wrap it with `os.NewFile`, and close the original wrapper. Never give two file wrappers ownership of the same descriptor.
- Model delayed signal handling with a test subprocess that installs its HUP handler before reporting readiness, receives HUP, and then waits for terminal input to end. Prove the regression fails against the old close ordering and completes normally after the fix.
- Preserve independent coverage for a child that ignores HUP: closing terminal input must not replace the existing group-wide kill after grace. Exercise both explicit close and context cancellation.
- Keep stress probes bounded and in worker scratch; distinguish allocation failures under artificial saturation from close-path failures. Follow the project's focused-validation policy.
