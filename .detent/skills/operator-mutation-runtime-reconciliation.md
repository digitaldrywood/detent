---
name: operator-mutation-runtime-reconciliation
description: Reconcile confirmed tracker mutations with event-loop-owned scheduler memory before degraded refresh fallback can restore stale state.
when_to_use: Use when an operator mutation succeeds in the tracker or UI but scheduling, API counts, or runtime telemetry continue using stale item state.
---

# Reconcile operator mutations with runtime state

1. Reproduce the split view: confirm the mutation response and tracker state changed while the runtime snapshot or scheduler still reports the prior state.
2. Trace the mutation boundary separately from the refresh path. Identify every item-scoped runtime map that can veto dispatch and every degraded-refresh fallback that can restore prior observations.
3. Send a synchronous request through the scheduler owner's event loop after the tracker mutation succeeds. Treat the reply as the serialization barrier that guarantees the next scheduler decision sees the mutation.
4. Update cached issue state and clear only matching item memory. Preserve unrelated blocked entries, retries, claims, failure signals, and project-wide breaker evidence.
5. Record structured runtime telemetry with the operator mutation as the reason and whether each veto was cleared.
6. Build the regression with one moved item and one unrelated stale-status item. Fail the unrelated transition refresh, prove the moved item dispatches, and assert board, API snapshot, and scheduler-decision counts converge.
7. Run focused package tests, the race suite, and the full repository validation gate.
