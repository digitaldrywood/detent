---
name: fail-closed-durable-state-migrations
description: Add a newly required integrity or identity field to persisted state without rejecting legitimate legacy records or accepting missing data from current writers.
when_to_use: Use when durable JSON state gains a security-sensitive required field and old processes may leave records that predate it.
---

# Fail-closed durable state migrations

1. Identify the exact serialized form produced by the prior release. Write the regression fixture as raw legacy data that omits the new field; do not create it through the current validated writer.
2. Bump the state schema. Let the reader accept only the explicitly supported legacy schema and the current schema. Newly created operations must use the current schema; retain the legacy schema when rewriting unresolved legacy operations that cannot yet satisfy the new field requirement.
3. Keep the new field mandatory at the current write boundary. A compatibility read must not weaken validation for newly created state.
4. At the consuming decision, grandfather an absent field only when the loaded schema is the known legacy schema. If a legacy record contains the field, validate it normally. Reject an absent or invalid field in current-schema state.
5. Migrate only when the new reader is confirmed to remain in service or a validated current operation requires the new schema. A resolved rollback restores the old reader: preserve its schema even after clearing the pending operation. Failure, retry, or notification writes must not erase legacy eligibility or invent provenance. Preserve rollback or recovery material when current identity validation fails.
6. Use table-driven tests for a raw legacy record without the field, a current record without it, a current record with the wrong identity, and a valid current record. Include load → failure/save → reload → success across multiple retries, plus failure → rollback → old-reader reload, including writes after pending state is cleared. Assert both the operation outcome and the persisted schema/material after success or failure. Model the old reader's schema restriction explicitly; the current reader accepting its own output proves no downgrade compatibility.
7. Trace initialization errors through every caller to the startup boundary. Returning an error from a loader does not fail closed if a constructor resets to empty state or a CLI logs the error and continues. Test malformed and unreadable durable state through the root command, asserting no work or health transition starts and the original state survives.
