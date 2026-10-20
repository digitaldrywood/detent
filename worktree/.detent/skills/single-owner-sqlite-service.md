---
name: single-owner-sqlite-service
aliases:
  - sqlite-text-time-ordering
description: "Operate a sole-owner SQLite service and preserve chronological ordering for TEXT timestamps."
when_to_use: "Use when adding or changing a long-running Detent service whose SQLite file must never be shared across service processes or opened remotely over NFS or SMB. Also use for sqlite text time ordering."
---

# Single-owner SQLite service

1. Keep the database handle private to the service package. Clients and sibling components cross the service API; they never receive a path, handle, query object, or transaction primitive.
2. Canonicalize the local database path before opening it. Create and resolve the parent directory, resolve an existing database symlink, reject in-memory and URI data sources, then acquire `<canonical-path>.lock` with `internal/instancelock` before `sql.Open`.
3. Retain the instance lock for the service lifetime. Drain or force-close HTTP work, close SQLite, and release the instance lock last. Every startup failure unwinds in the same order.
4. Use one SQLite connection unless concurrent access has been deliberately designed. Apply busy-timeout, foreign-key, and exclusive-lock pragmas to every connection, query them back, and acquire an exclusive transaction during startup. Use an SQLite `application_id` to reject a wrong database before enabling WAL or applying migrations.
5. Use goose's instance-scoped provider with a dedicated version table and global migration registration disabled. Apply only embedded forward migrations, refuse a database version newer than the embedded target, and verify the applied version equals the target before reporting healthy.
6. Back up through `database/sql.Conn.Raw` and `modernc.org/sqlite.NewBackup`. Copy bounded page batches, check cancellation between batches, always finish the backup, reject the source or an existing destination, and remove only a destination created by the failed call. Never copy the database, WAL, and shared-memory files directly.
7. Test exact schema inventory and constraints, queried-back pragmas, wrong identity, future-version refusal, same-process and helper-process ownership contention, release and reopen, append-only triggers, context-driven shutdown, and a live online backup followed by `PRAGMA integrity_check`.

## Keep SQLite text timestamps chronologically sortable

Use this case when a SQLite query compares or orders RFC3339-like TEXT timestamps, especially when boundary tests disagree with application time arithmetic.

Variable-width RFC3339 encodings are not safely ordered as text. A whole-second value ending in `Z` can sort after a later value with a fractional suffix because `.` sorts before `Z`.

Use one representation for every value in an order-sensitive column:

- Prefer an integer epoch when the schema permits it.
- Otherwise store UTC text with a fixed-width fraction, such as Go's `2006-01-02T15:04:05.000000000Z` layout.
- Keep parsing compatible with the stored precision, and return malformed persisted values as errors instead of silently substituting zero time.

Do not mix offsets, fractional widths, or integer units in the same compared column. When changing an existing representation, normalize existing rows in a migration or stop relying on lexical comparison until the data is uniform.

Exercise the real SQLite predicate in a table-driven test. Include a whole-second timestamp, a subsecond retry or expiry after it, equality at the boundary, and a later value. Assert both `ORDER BY` and the exact `<=` or `>=` predicate used by production.
