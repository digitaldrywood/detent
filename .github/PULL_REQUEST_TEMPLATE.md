## Release pick

Pull requests are used only for release-branch picks; see [Release](../WORKFLOW.md#release).

- Develop commit picked with `-x`:
- Native work item:

Invariants touched: none, or affected INV IDs. Per-change rationale and evidence belong in this description or the work item, never in repository files ([INV-16](../docs/invariants.md#inv-16--the-tracker-database-is-the-only-shared-knowledge-channel)).

## UI Surface Contract

Adds visible UI? If yes, link the human-authored issue that names the surface or feature it belongs to.

- [ ] N/A, or the linked issue explicitly authorizes any high-impact UI surface, layout, density, first-viewport, or responsive visibility tradeoff.
- [ ] Persistent top-of-screen messaging is explicitly authorized by the issue, or this PR does not add it.
- [ ] Visual changes to primary operator screens include screenshot or browser verification below.

## Test Plan

- Validation already recorded for the develop commit, and any pick-specific conflict resolution checks.
