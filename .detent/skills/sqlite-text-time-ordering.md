---
name: sqlite-text-time-ordering
description: Preserve chronological ordering when SQLite timestamps are stored as TEXT and used in range, retry, lease, or stale-work predicates.
when_to_use: Use when a SQLite query compares or orders RFC3339-like TEXT timestamps, especially when boundary tests disagree with application time arithmetic.
---

# Keep SQLite text timestamps chronologically sortable

Variable-width RFC3339 encodings are not safely ordered as text. A whole-second value ending in `Z` can sort after a later value with a fractional suffix because `.` sorts before `Z`.

Use one representation for every value in an order-sensitive column:

- Prefer an integer epoch when the schema permits it.
- Otherwise store UTC text with a fixed-width fraction, such as Go's `2006-01-02T15:04:05.000000000Z` layout.
- Keep parsing compatible with the stored precision, and return malformed persisted values as errors instead of silently substituting zero time.

Do not mix offsets, fractional widths, or integer units in the same compared column. When changing an existing representation, normalize existing rows in a migration or stop relying on lexical comparison until the data is uniform.

Exercise the real SQLite predicate in a table-driven test. Include a whole-second timestamp, a subsecond retry or expiry after it, equality at the boundary, and a later value. Assert both `ORDER BY` and the exact `<=` or `>=` predicate used by production.
