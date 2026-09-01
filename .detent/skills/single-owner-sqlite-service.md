---
name: single-owner-sqlite-service
description: Build or evolve a Detent service that must be the sole process opening its SQLite database, with fail-closed ownership, forward migrations, and online backup.
when_to_use: Use when adding or changing a long-running Detent service whose SQLite file must never be shared across service processes or opened remotely over NFS or SMB.
---

# Single-owner SQLite service

1. Keep the database handle private to the service package. Clients and sibling components cross the service API; they never receive a path, handle, query object, or transaction primitive.
2. Canonicalize the local database path before opening it. Create and resolve the parent directory, resolve an existing database symlink, reject in-memory and URI data sources, then acquire `<canonical-path>.lock` with `internal/instancelock` before `sql.Open`.
3. Retain the instance lock for the service lifetime. Drain or force-close HTTP work, close SQLite, and release the instance lock last. Every startup failure unwinds in the same order.
4. Use one SQLite connection unless concurrent access has been deliberately designed. Apply busy-timeout, foreign-key, and exclusive-lock pragmas to every connection, query them back, and acquire an exclusive transaction during startup. Use an SQLite `application_id` to reject a wrong database before enabling WAL or applying migrations.
5. Use goose's instance-scoped provider with a dedicated version table and global migration registration disabled. Apply only embedded forward migrations, refuse a database version newer than the embedded target, and verify the applied version equals the target before reporting healthy.
6. Back up through `database/sql.Conn.Raw` and `modernc.org/sqlite.NewBackup`. Copy bounded page batches, check cancellation between batches, always finish the backup, reject the source or an existing destination, and remove only a destination created by the failed call. Never copy the database, WAL, and shared-memory files directly.
7. Test exact schema inventory and constraints, queried-back pragmas, wrong identity, future-version refusal, same-process and helper-process ownership contention, release and reopen, append-only triggers, context-driven shutdown, and a live online backup followed by `PRAGMA integrity_check`.
