---
name: supervised-lsof-scan-overhead
description: Diagnose macOS lsof deadlines caused by global filesystem lookup and fork overhead, then consolidate timeout ownership at the command boundary.
when_to_use: Use when workspace scans return zero output at their deadline while lsof starts normally and focused cleanup tests are slow or fail under concurrent validation.
---

Compare the exact scanner command alone and under bounded concurrency. Record
elapsed time, exit status, output bytes, and host process/mount counts. Keep probes
read-only and samples in the attempt temporary directory.

Sample a running lsof process. Repeated fork/read/wait stacks differ from a binary
stuck in dyld startup. Compare name resolution, recursive traversal, PID selection,
and internal timeout machinery separately. A single-PID scan that remains slow
rules out attributing the overhead solely to process enumeration.

Do not apply `-O` alone: it removes lsof's protection from blocking operations,
and CommandContext's kill does not guarantee Wait returns. A replacement timeout
owner must return on cancellation independently of Wait, while retaining an
asynchronous waiter to reap the command. Give the waiter sole ownership of output
buffers; cancellation must not inspect buffers while output goroutines write.
Use a buffered result channel so a late completion cannot block after return.

Avoid apparently simpler alternatives without checking their call paths. `-b`
prints ambiguous filenames (control-byte caret notation can collide with literal
characters). Native PROC_PIDVNODEPATHINFO also calls filesystem stat internally,
so it does not by itself establish an interruptible deadline.

Reproduce blocked Wait deterministically with a descendant that retains stdout
and stderr after its parent exits. Cancel while those pipes remain open and prove
the scanner returns before releasing the descendant. Retain ownership, observer
survival, lock-release, error and output tests; run focused race tables and the
full repository gate after the final change.
