---
name: fail-closed-durable-state-migrations
description: Add a newly required integrity or identity field to persisted state without rejecting legitimate legacy records or accepting missing data from current writers.
when_to_use: Use when durable JSON state gains a security-sensitive required field and old processes may leave records that predate it.
---

# Fail-closed durable state migrations

1. Identify the exact serialized form produced by the prior release. Write the regression fixture as raw legacy data that omits the new field; do not create it through the current validated writer.
2. Bump the state schema. Let the reader accept only the explicitly supported legacy schema and the current schema, while every writer always emits the current schema.
3. Keep the new field mandatory at the current write boundary. A compatibility read must not weaken validation for newly created state.
4. At the consuming decision, grandfather an absent field only when the loaded schema is the known legacy schema. If a legacy record contains the field, validate it normally. Reject an absent or invalid field in current-schema state.
5. Let the next successful state write migrate the record to the current schema. Preserve rollback or recovery material when current identity validation fails.
6. Use table-driven tests for a raw legacy record without the field, a current record without it, a current record with the wrong identity, and a valid current record. Assert both the operation outcome and the persisted schema/material after success or failure.
