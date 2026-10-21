## Summary

-

Selected tracker reference: link the native Cloud work item or GitHub issue
that owns this change. Use `Fixes #N` only for a GitHub-tracked issue.

Deployment or release blockers follow the [High-priority reporting
contract](../AGENTS.md#deployment-and-release-failure-reporting).

Invariants touched: none, or affected INV IDs. Only a change to an invariant's
rule or its enforcing check edits `docs/invariants.md`; link that edit when
applicable. Record per-change rationale, evidence and verification in this
Change/PR description or issue comments through the authorized tracker owner.
Under [INV-16](../docs/invariants.md#inv-16--the-tracker-database-is-the-only-shared-knowledge-channel),
never append them to shared repository files or use repository notebooks for
handoff between issues or runs; no repository file is append-only by convention.

## UI Surface Contract

Adds visible UI? If yes, link the human-authored issue that names the surface or feature it belongs to.

- [ ] N/A, or the linked issue explicitly authorizes any high-impact UI surface, layout, density, first-viewport, or responsive visibility tradeoff.
- [ ] Persistent top-of-screen messaging is explicitly authorized by the issue, or this PR does not add it.
- [ ] Visual changes to primary operator screens include screenshot or browser verification below.

## Test Plan

- Record the focused diagnostics or browser verification performed, their
  results, and any remaining evidence gaps. State when diagnostics were not run.
- Follow [repository validation](../AGENTS.md#validation): ordinary submission
  and merge do not wait for a full suite, coverage percentage, fuzz duration,
  local status, or scheduled run.
