---
name: demand-driven-priority-reservations
description: Diagnose and test strict scheduler reservations that confuse project liveness or stale cycle state with pending dispatch demand.
when_to_use: Use when lower-priority work reports a priority reservation despite free global capacity, no slot holders, or an empty higher-priority queue.
---

# Keep priority reservations demand-driven

1. Reproduce the contradiction at the capacity gate and capture the decision reason, selected project, capacity, usage, and holders.
2. Trace every transition of the reserving project's demand state. Include empty scan completion, ready registration, acquisition, release, reconfiguration, and shutdown rather than searching only for explicit idle calls.
3. Treat an empty completed scan as idle until that same project begins a new scan, registers ready work, or releases one of its own slots and may have capacity for work skipped during the scan. Unrelated slot releases must not create demand or invalidate the idle result.
4. Clear idle state at the earliest project-owned demand signal so a newly ready higher-priority project reclaims precedence immediately.
5. Use a table-driven regression with four cases: an idle higher-priority project stays idle across another project's acquire and release; its own slot release invalidates idle state; pending higher-priority demand reserves capacity; and an idle-to-ready transition restores the reservation.
6. Assert both admission and diagnostics. A free idle fleet must grant the lower request instead of emitting a priority reservation, while genuine contention must retain the priority reservation reason.
7. Run the focused test before and after the fix, then run the scheduler race tests and the repository validation gate.
