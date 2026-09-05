# Merge Train

[Back to README](../README.md#documentation)

`Merging` is intentionally serialized. Keep this in every production workflow:

```yaml
agent:
  max_concurrent_agents_by_state:
    Merging: 1
```

Do not cap `Todo`, `In Progress`, or `Rework` unless you have a specific
operational reason. Those states should share the global agent pool so workers
stay busy while merge candidates wait for CI or a clean base branch.

When GitHub reports `HAS_MERGE_QUEUE`, Detent delegates every eligible green
candidate to the repository's native merge queue instead of rebasing each PR
through the serialized worker. GitHub owns merge-group validation and batching;
Detent keeps the issues in `Merging`, observes their queue entries, and
reconciles them to `Done` after GitHub reports the PR merged. Without a native
queue, a BEHIND PR whose existing head is mergeable and already has green
required checks is submitted to the exact-head merge API without rewriting
the checked head. If GitHub explicitly rejects an out-of-date base, Detent
refreshes that head and waits for its required CI. Other refusals, including
failed checks, conflicts, permissions, changed heads, and native queue
requirements, do not authorize this refresh fallback. If a refreshed head
is missing required contexts, Detent routes the issue to `Rework` instead of
retrying an incomplete merge worker.

A selected merge worker reserves its repository across CI waits for at most
one hour from initial selection. Other same-repository retries cannot refresh
or merge ahead of it during that reservation. Waiting releases the worker and
issue claim, so implementation work and other repositories remain dispatchable
under the configured capacity limits. Dependency priority and queue age select
the next candidate after a reservation is released.

The reservation deadline survives Detent-created head refreshes and restart;
it is stored with the successful waiting work attempt. Failed checks, conflicts,
withdrawal, approval or CI-trigger revocation, native queue handoff, and external
head changes release it. An external base advance keeps the deadline and
requires a fresh correctness check; strict protection may require another
refresh. Missing or degraded tracker data cannot extend the deadline. Expiry
releases the repository even if CI stalls, while the existing CI timeout handles
the waiting issue. Recovery restores only an unexpired reservation whose latest
terminal attempt and current PR still match. A released candidate cannot renew
its own reservation before another candidate takes over. Recorded completion
releases the reservation even after the tracker omits the completed issue from
queue fetches. Diagnostics record
reservation release reasons, head/base identities, and validation invalidation.

Inside the serialized `Merging` lane, avoid duplicating the full local release
gate when it does not buy new signal. If the PR already passed the pre-review
gate, the branch rebases cleanly onto current `origin/main`, and no source files
change during rebase, the merge agent should run a focused rebase/smoke gate
locally and rely on required current-head CI for full enforcement. If the merge
agent edits code, resolves conflicts, detects stale or unknown validation state,
or cannot prove the final rebase was source-clean, it must run the full
configured gate again.

CI waiting should poll current-head REST check runs with backoff, not loop on
GraphQL-heavy PR status commands. Required release checks must run on the PR
head before merge; post-merge-only failures cannot be part of the merge
decision. Merge handoff telemetry should record the
quiet-window wait, GitHub queue/start wait, local merge-gate duration,
current-head PR CI duration, active slow-check runtimes, and whether post-merge
`main` CI is still running. The quiet window, current-head required CI, and
conflict/full-gate fallback are quality gates; repeated full local validation
after a source-clean rebase, noisy status polling, uncached tool install, and
duplicated non-blocking post-merge work are optimization targets.

The repository CI caches the project-pinned golangci-lint binary and only builds
it with `go install` on cache miss. CI builds golangci-lint `v2.9.0` with the
repository Go toolchain so analyzer behavior and toolchain provenance stay
aligned. `GoReleaser Snapshot` runs on pull requests, `main`, release tags,
nightly schedule, and manual dispatch so packaging signal is available before
and after merge.
