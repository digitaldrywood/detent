---
name: join-canceled-startup-rollback
description: Diagnose and fix service startup rollback that abandons already-started background workers when the startup context is canceled.
when_to_use: Use when shutdown returns while startup work still writes files, uses a closed store, or recreates temporary directories after cancellation.
---

# Join canceled startup rollback

1. Reproduce under the pressure mode that exposes the leak, such as coverage repetitions or the race suite. Record the resource touched after shutdown returns.
2. Trace the writer to its owning worker and follow startup failure rollback. Check whether rollback passes an already-canceled context to stop, wait, or close operations.
3. Preserve cancellation as the startup result, but derive cleanup with `context.WithoutCancel(ctx)`. Stop and join every started worker, close owned resources, and only then remove them from registries.
4. Add a channel-driven regression test: block one started worker, cancel another startup, verify startup cannot finish before the worker is released, then verify the worker resource is closed before startup returns.
5. Rerun the focused regression under `-race`, then repeat the original pressure reproduction with coverage before the full validation gate.
