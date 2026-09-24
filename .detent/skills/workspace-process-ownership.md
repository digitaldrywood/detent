---
name: workspace-process-ownership
description: "Diagnose workspace process ownership and macOS lsof scan overhead without harming unrelated processes."
when_to_use: "Use when workspace cleanup counts vary under concurrent load or host services disappear while a workspace is reaped. Also use for supervised lsof scan overhead."
---

# Diagnose workspace process ownership

Capture identities before cleanup signals erase the evidence. On macOS,
`lsof -FpcRfn +D "$workspace"` records PID, parent PID, command, descriptor,
and matched path. Compare each process's working directory with its ordinary
file descriptors; an open file alone does not establish worker ownership.
Keep the original reap count and termination assertions until every extra
match is explained.

Docker Desktop can make host file-sharing services hold descriptors inside
a bind-mounted workspace. Mounting a parent directory and opening a file in
a child workspace from a bounded container can reproduce this safely. Run
only read-only process scans against that probe; do not feed host service
PIDs to the old reaper. Allow the probe container to exit naturally. A mount
of the workspace itself can also expose the Docker backend's mount-root
descriptor, producing an additional match.

Check the intended ownership contract across platforms. Detent's Linux
scanner selects `/proc/<pid>/cwd` beneath the workspace. The corresponding
lsof selection is `-a -d cwd -t +D "$workspace"`; `-a` intersects the
descriptor and directory selectors. Validate both the workspace root and
its descendants when changing this boundary. Keep process-group cleanup
responsible for registered worker descendants independently of cwd.

Turn the observed distinction into a deterministic regression without
depending on Docker: start one helper with cwd inside the workspace and an
exclusive lock, and another with cwd outside but a different locked file
inside. Use readiness pipes to establish both locks before completing or
canceling the turn. Require exactly one reap, the inside helper's exit and
released lock, and the outside helper's survival and retained lock. Capture
the native file matches before reaping so failures retain ownership evidence.

Repeat race tests under concurrent validation load.
Separate reproduced identities from historical attribution when the original
failure recorded only a count.

## Supervised Lsof Scan Overhead

Use this case when workspace scans return zero output at their deadline while lsof starts normally and focused cleanup tests are slow or fail under concurrent validation.

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
survival, lock-release, error and output tests; run focused race tables.
