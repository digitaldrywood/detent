---
name: provider-queue-removal-recovery
description: Preserve remote queue failure and cancellation decisions when missing entries would otherwise be automatically re-enqueued.
when_to_use: A scheduler treats an absent remote queue entry as retry permission, especially when local cache loss after restart can repeat failed integration work.
---

# Provider queue removal recovery

An absent queue entry is ambiguous: it may represent failure, cancellation,
completion, or a transient observation. Consult the provider's durable removal
history before deciding to enqueue again. Local cache presence alone cannot
preserve that decision across restart.

Bind removal evidence to the immutable work identity. For GitHub merge queues,
inspect the latest `RemovedFromMergeQueueEvent.beforeCommit.oid` alongside the
current PR head and queue entry. A removal for the current head prevents an
automatic retry; a newly checked head can be eligible again. Missing removal
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
