# Outbox shutdown investigation

Original Windows job: https://github.com/digitaldrywood/detent/actions/runs/34314494525/job/102347759577

The original job reports the combined Close timeout at 05:27:37.705Z and a 7.52-second test duration. It contains no stage diagnostics or goroutine dump, so it cannot identify the historical stalled stage. A later rerun of the same run is not the original evidence; retrieve the original job logs by job ID.

The unchanged test passed 10 focused macOS race repetitions. A controlled reproduction reserved the sole database connection after the backend started and held it until the existing two-second Close guard fired. The captured stack in blocked-persistence.txt shows Close joining the worker in outboxWorker.stop, and the canceled worker waiting in database/sql.(*DB).conn through failOutbox. This establishes a post-cancellation persistence timeout failure mode, not the precise cause of the historical Windows delay.

ProcessOutbox deliberately persists the canceled result with a fresh BusyTimeout context (five seconds by default). The old two-second guard could fail before that legitimate work completed. No production lifecycle defect was demonstrated.

The revised test explicitly observes backend startup, cancellation, release, and return; exercises available and held database connections; and reopens SQLite after Close to verify the canceled item was persisted for retry. Thirty-second timers are OS integration deadlock guards, not behavioral timing assertions. Timeout failures identify the stage and include all goroutine stacks.

Historical validation (September 9, before restoration): original focused race count=10 passed; controlled original-test reproduction failed at the expected guard; revised focused race count=20 passed. Full gate and current-head Windows CI are recorded in the issue Workpad.

A temporary Go overlay removed outboxWorker.stop's WaitGroup join without changing the worktree. Both revised cases failed: the reopened outbox item remained processing instead of retrying. This confirms the persistence assertion detects a missing worker join.

Restoration on September 11: focused non-race count=20 passed on current main plus this test patch. The worker protocol reserves new race/coverage runs for the merge queue and selects `make check-fast` as the local gate. Current-head local gate, validator review, and Windows CI results are recorded in the issue Workpad.
