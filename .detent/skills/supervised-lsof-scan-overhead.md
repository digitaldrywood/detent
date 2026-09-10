---
name: supervised-lsof-scan-overhead
description: Diagnose macOS lsof scan deadlines caused by per-operation fork overhead on hosts with many processes.
when_to_use: Use when workspace scans return zero output at their deadline while lsof starts normally and focused cleanup tests are slow or fail under concurrent validation.
---

Compare the exact scanner command alone and under bounded concurrency. Record
elapsed time, exit status, output bytes, and process count. Keep probes read-only
and their samples in the attempt temporary directory.

Sample a running lsof process. Repeated fork/read/wait stacks indicate a different
failure from a binary stuck in dyld startup. Compare one variable at a time:
name resolution, recursive directory traversal, then lsof's internal timeout
machinery. Do not attribute zero output alone to any of these causes.

When fork-based timeout overhead is established and the caller already owns a
bounded command context, evaluate lsof `-O` to remove the duplicate timeout layer.
This option disables lsof's protection against blocking kernel operations; retain
the caller's cancellation and deadline, and do not apply it to unsupervised scans.
Do not increase deadlines or broaden process selection to compensate for overhead.

Verify root and nested working directories are selected, sibling-prefix paths are
excluded, and a process outside the workspace holding an open workspace file
survives cleanup. Retain lock-release and command-cancellation assertions. Run the
focused race tables and the full repository gate on the final contents.
