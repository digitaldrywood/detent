---
name: sqlite-fenced-leases
description: Implement atomic SQLite lease ownership with monotonic fencing tokens, stale-writer rejection, and durable recovery metadata.
when_to_use: Use when multiple clients contend for durable work through one SQLite-owning service and expired owners must never mutate reassigned work.
---

# SQLite fenced leases

- Keep every ownership decision and its write in one transaction on the sole SQLite-owning service.
- Generate fencing tokens with an `INTEGER PRIMARY KEY AUTOINCREMENT` lease row. Never derive tokens from wall time or reuse a previous row.
- Read the unreleased lease first. Return an idempotent result only for the same machine and session; reject another unexpired owner; mark an expired row released before inserting its successor.
- Preserve lease rows and append-only session events. Return the previous session identity and latest durable recovery event to the successor instead of deleting expired metadata.
- Require the exact positive fencing token on renew, release, and event append. In the same transaction, verify the row is unreleased and unexpired before any mutation.
- Parse persisted timestamps and compare them as time values. Do not order RFC3339Nano strings in SQL because optional fractional seconds are not lexically ordered at equal whole seconds.
- Use the current lease row as the authority for machine and session identity when appending events; reject supplied identities that disagree.
- Test contention with a start barrier and distinct sessions, deterministic expiry with an injected clock, every stale mutation path after reassignment, and restart recovery of prior session metadata.
