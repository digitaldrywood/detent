---
name: workspace-process-ownership
description: Diagnose extra workspace reap matches caused by outside processes holding files, especially during macOS container validation.
when_to_use: Use when workspace cleanup counts vary under concurrent load or host services disappear while a workspace is reaped.
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

Repeat race tests under concurrent validation load, then run the full gate.
Separate reproduced identities from historical attribution when the original
failure recorded only a count.
