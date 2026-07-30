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
required checks is merged directly against the current base without rewriting
the checked head. Rebase is reserved for fallback cases; if a refreshed head
is missing required contexts, Detent routes the issue to `Rework` instead of
retrying an incomplete merge worker.

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
it with `go install` on cache miss. The official prebuilt action was evaluated,
but the prebuilt `v2.1.6` binary targets an older Go toolchain than this repo and
newer prebuilt lint releases change the enforced lint set. `GoReleaser Snapshot`
now runs on pull requests, `main`, release tags, nightly schedule, and manual
dispatch so packaging signal is available before and after merge.
