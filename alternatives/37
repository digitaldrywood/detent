---
name: provider-queue-removal-recovery
description: Consume remote queue outcomes once before retrying through existing admission and attempt accounting.
when_to_use: A scheduler treats an absent remote queue entry as retry permission, especially when local cache loss after restart can repeat failed integration work.
---

# Provider queue removal recovery

An absent queue entry is ambiguous: it may represent failure, cancellation,
completion, or a transient observation. Consult the provider's durable removal
history before deciding to enqueue again. Local cache presence alone cannot
preserve that decision across restart.

Bind removal evidence to the immutable work identity. For GitHub merge queues,
inspect the latest `RemovedFromMergeQueueEvent.beforeCommit.oid` alongside the
current PR head and queue entry. Use a known enqueue timestamp to distinguish the ended attempt; beforeCommit
can be an integration commit rather than the PR head. Consume each removal
once using existing durable event accounting, release ended ownership, and allow the
next pass through normal admission under the existing attempt budget. A queue
outcome alone is not evidence that a branch needs rework. Missing removal
identity requires a conservative disposition. An existing provider entry can
represent explicit re-enqueue and should be observed rather than duplicated.

Keep admission fencing separate from cleanup inspection. An observed changed
head must prevent admission, but inspection still needs to return the entry so
a revoked configuration can dequeue it. Scope caches to the checked identity
and use the provider's expected-head mutation precondition when available.

Test with a fresh scheduler state, not only a reused cache: removed current
identity, repaired identity, missing removal identity, explicit re-enqueue, and
unrelated passing candidates. Verify that deferral consumes no worker and does
not reserve the repository against those other candidates. Preserve existing
authorization gates; provider history does not authorize a policy bypass.

Persist the consumed outcome before permitting the next admission. Rehydrate the
count and removal identity from the existing event store after restart, and use
the existing budget disposition as the reset boundary. Test a real store reopen
both before retry admission and between distinct removals, plus read/write
failures; an in-memory fresh-state test alone does not prove durable accounting.
