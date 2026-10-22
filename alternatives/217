# Concurrent validation evidence for #3253

## Existing implementation and shared writes

Commit `88f9afc39ee86b37a44d02430d1dc9200adf51e2` removed the common-Git-directory checklock wrapper from `check` and `check-fast` on 2026-09-29 at 22:49:43Z. Merged PR #3305 (`bd55ccb8472fbe8438321fc0b6ac144022cb1bf8`) subsequently removed the lint runner lock and repaired client failure propagation. This PR completes their regression evidence and fixes two stale lint assertions, rather than adding another mechanism.

Audit of the current Make graph:

| Step | Writes and coordination |
| --- | --- |
| Invariants and migrations | Fixture databases and test scratch use the provided temporary root; source checks read this worktree. |
| Generated checks | sqlc diff and config documentation compare this checkout; generated sources stay local. |
| Client checks | node_modules, Vite output, and bundle drift checks belong to this worktree; Vitest retains isolation with at most two workers. |
| Lint | Pinned binary lives under this worktree's tmp/tools; parallel runners use golangci-lint's existing parallel mode. |
| Vet, short tests, build | Go module/build caches have native content-addressed coordination; test scratch and build output remain local. |
| Git metadata | Make reads version, commit, and tracked bundle changes; no gate step writes shared refs or the common index. Each linked worktree has its own index. |

No gate step requires an additional whole-gate critical section. Explicit legacy checklock callers remain compatible, but current Make targets never join their queue. Validation evidence remains attached to the actual tested head; the configured no-op `true` does not supply test credit.

## Focused diagnostics

The original two root lint tests failed because they expected `run --timeout=15m`, despite the correctly pinned command now including `--allow-parallel-runners`. Both pass after the assertion repair.

`TestMakeCheckFastOverlapsWorktrees` creates two linked worktrees in a disposable Git repository, holds its legacy validation lock, and starts both real Make graphs with controlled Go tools. Expensive client/lint/vet/short-test prerequisites are explicitly omitted in this fixture. A pipe handshake requires both build steps to start before either can complete, then verifies separate worktree-local evidence. Both reached build in 568.584 ms and completed successfully. This is a focused graph/isolation regression, not a full check-fast pass or a host load benchmark.

- `go test . -run '^(TestGolangCILintUsesRepositoryPinnedVersion|TestMakeLintIgnoresAmbientBinary)$' -count=1`: passed.
- `go test ./tools/checklock -run '^(TestMakeCheckFastOverlapsWorktrees|TestMakeCheckPreflightWithoutSharedLock)$' -count=1 -v`: passed, including invariant and generated-input failure propagation.
- `go vet ./tools/checklock .`: passed.
- No full, coverage, or race suite was run, following current project policy. Exact final head and configured gate are recorded in the issue Workpad.

## Early rollout comparison

`measurements.json` retains a read-only snapshot of the common Git directory's legacy event history and origin/develop's first-parent squash-merge history. Local attempt attribution uses the host's read-only Detent database: project `detent`, null worker_host (local), and matching PR number. The observation ends at 2026-09-30T04:15:46Z; the repository head is `fb404585b52b636958e1bc026c469590858718c3`.

| Metric | Before removal: Sep 29 00:00–22:49:43Z | After removal: Sep 29 22:49:43Z–Sep 30 04:15:46Z |
| --- | ---: | ---: |
| Window hours | 22.829 | 5.434 |
| Repository squash-merged PRs | 35 | 15 |
| Repository PRs per 24h, extrapolated | 36.80 | 66.25 |
| Merges with local attempt history | 4 | 1 |
| Local-attributed PRs per 24h, extrapolated | 4.21 | 4.42 |
| Completed legacy gate runs ending in window | 68 | 4 |
| Legacy median / max wait, seconds | 0.201 / 5380.374 | 547.576 / 2735.016 |
| Legacy median / max hold, seconds | 680.461 / 1688.640 | 1524.396 / 1997.308 |
| Maximum observed legacy queue size | 12 | 2 |

The September 29 issue baseline was a >20-minute hold, nine other queued runs, a >90-minute wait, and a theoretical 72 gates/day ceiling. The retained history corroborates long waits and holds, but its coverage does not turn that single observation into a complete host baseline. Whole-run durations here are grouped by terminal timestamp, not summed as clipped daily totals. Terminal wait failures are counted separately in JSON; incomplete runs do not get invented final durations.

Current Make targets have no validation-lock wait or hold interval; zero lock cost follows from removal and the held-lock regression, not from new telemetry. The four post-removal legacy completions came from older invocations and do not describe the current graph. Repository merge counts include operator/native work not attributable to this host, and local attempt attribution is incomplete. Initial Done lane imports were excluded because they are not merge timestamps.

These unequal partial-day windows cannot establish sustained 100+ PRs/host/day or causation. Concurrent lint was already exercised across linked worktrees in #3305; simultaneous full check-fast runs under host load were not performed in this worker. Recompare a complete busy day after old worktrees drain using recorded merge/attempt history, preserving host attribution and instrumentation coverage. Scheduled full validation remains the integrated-source diagnostic.
