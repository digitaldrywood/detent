---
name: fenced-event-acknowledgment-recovery
description: Recover ordered event publication when the server commits but the client loses the acknowledgment.
when_to_use: Use when fenced clients publish checkpoints or terminal events over an at-least-once transport and reconnects must not duplicate progress.
---

- Keep the last unacknowledged command, payload and sequence intact. Flush it before constructing a later event; do not infer failure from a lost response.
- Deduplicate both the command identity and the attempt/sequence identity on the sole state owner. An exact replay is a read of the committed result, while changed content at that sequence conflicts.
- Commit event history and the current attempt projection in the same fenced transaction. Require a contiguous sequence and enforce terminal state separately from lease ownership.
- Test response loss after the real handler commits, then retry start, checkpoint and completion. Assert one durable event per logical transition, including a second completion retry after flushing its pending acknowledgment.
- Preserve historical checkpoints when the owner expires, and stop a live client before its conservative local lease deadline. Reconnection creates or revalidates authority; it never revives a cancelled execution context.
- Keep external-effect reconciliation separate: a database receipt cannot establish whether Git, a PR provider or a model provider accepted an in-flight operation.
