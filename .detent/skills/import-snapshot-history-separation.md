---
name: import-snapshot-history-separation
description: Keep retained import history separate from current relationship membership when resumable reimport must handle source removals without overwriting native state.
when_to_use: Use when a paginated importer deduplicates source records but later derives dependencies or other mutable relationships from that retained history.
---

# Separate import history from current relationships

Deduplicated source records describe everything previously observed. They do not
necessarily describe the source's current dependencies or memberships. Deriving a
live graph from all retained records can resurrect removed relationships or block
cutover on a dependency that no longer exists.

Keep original payloads and source IDs for history, with separate metadata for the
current snapshot. For paginated replacement, update membership and the page cursor
in one transaction. Reset membership only when applying the first successful page,
retain it across continuation pages, and reject stale checkpoint writes. A failed
fetch must not silently replace the last observed snapshot with an empty one.

Distinguish incomplete traversal from a complete empty relationship set. Keep new
intake ineligible until required pages and dependency resolution finish. Exports
should identify historical edges that are no longer current.

Scope graph reconciliation to the source-owned or still-pending intake surface.
After native ownership begins, source additions and removals remain import evidence
unless an explicit operation authorizes changing native relationships.

Exercise the real importer and graph in regression cases: retained edge, removed
edge, removal before target import, repeated pages, restart during intake, and
source changes after native cutover. Verify that history remains deduplicated while
the permitted current graph changes, and that native relationships stay intact.
