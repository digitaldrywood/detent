---
name: definite-publication-failure-recovery
description: Recover durable publication reservations after definite rejection without duplicating an uncertain remote side effect.
when_to_use: Use when a failed comment, upload, or other non-idempotent publication leaves an internal reservation waiting forever despite no confirmed remote result.
---

# Recover a rejected publication

- Establish whether the external write was definitely rejected, definitely committed, or remains uncertain. Keep that distinction in typed connector errors or results; a generic error alone does not prove that no side effect occurred.
- Classify failures at the adapter that knows the operation and transport. Failed preflight reads and connection establishment can prove non-delivery. Use the provider's rejection semantics for HTTP responses; do not treat a lost response, malformed success body, or arbitrary server error as permission to repost.
- Release only the matching unposted reservation after definite rejection. Compare its scope, stable key, and payload, and preserve any recorded remote result or answer. Make the release durable so ordinary retry and restart can proceed.
- If the caller was cancelled after a definite rejection, use a separate bounded cleanup context for the release. Do not use that context to initiate a new external write.
- Reconcile uncertain writes by a stable remote marker or receipt before deciding what follows. Surface unresolved publication as an operational error rather than waiting for a response to an unconfirmed publication. Recovery does not expand the original authorization.
- Reproduce the stale reservation with a negative control, then test rejection followed by retry, restart, caller cancellation, concurrent retries, preservation of confirmed results, and loss of the response after a successful write. Assert actual remote write counts as well as durable state.
