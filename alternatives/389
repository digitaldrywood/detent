# Test suite audit (2026-09-30)

The suite had grown to 988 test files and 380K test lines against 350K lines
of production code, and `go test ./...` took 642 seconds of wall clock on a
16-core host. Almost none of that time came from the number of tests. It came
from a handful of real-host operations and one package that pinned itself to a
single test at a time. This document records what was measured, what changed,
and the rule used to remove duplicated tests, so the next audit can start from
evidence instead of repeating it.

## Where the time went

Every package was timed alone, because the full run oversubscribes the host and
inflates individual test times by 5x to 80x. The uncontended costs were:

| Package | Wall alone | Cause |
| --- | --- | --- |
| `internal/workspace` (via `scripts/test-workspace.sh`) | 350s | `-parallel 1` plus a host-wide `lsof` and scratch-environment scan on every workspace cleanup |
| `internal/cli` | 215s | one doctor test walked the developer's real Go build cache for 135s |
| `internal/hubserver` | 85s | every fixture opened a fresh SQLite file and ran 38 migrations under `synchronous(FULL)` |
| `internal/orchestrator` | 44s | volume only |

## What changed

- The workspace package exposes its process scanner through the
  `reapProcessScanner` variable. `TestMain` stubs it; a test that needs the
  real scan opts in with `useRealProcessScan(t, root)`. The `-parallel 1`
  override and the 30-minute budget in `scripts/test-workspace.sh` are gone;
  the package defaults to eight parallel tests.
- The cli doctor resolves cache inspection through `defaultInspectCaches`,
  which the cli `TestMain` replaces with an empty report.
- Hub tests migrate one template database per process and copy the file for
  each fixture, so a test opens an already-migrated database.
- `TEST_PROCS` in the Makefile caps package parallelism, `GOMAXPROCS`, lint
  concurrency, and vitest workers for every `make` gate, because several
  worktrees run gates on this host at the same time.

Result on the same host: `go test ./...` 642s to 245s, workspace 350s to about
125s, cli 215s to 86s, hubserver 85s to 66s.

## Duplicate removal rule

Per-test statement coverage was collected for the six largest packages by
building each test binary with `-cover` and running every top-level test on
its own (3764 tests). Candidates had to meet all of the following:

1. Every statement the test executes is also executed by one other single
   test in the same file.
2. It executes at least 80 percent as many statements as that test, so the
   pair exercises the same path rather than a tiny unit test being swallowed
   by a large integration test.
3. It is not named in `docs/invariants.md` or `invariants/policy.json`.

That produced 195 candidates. Two independent Codex passes then read each
pair. The first kept 170 because the candidate asserted something its sibling
did not; a second pass on the remaining 25, required to quote the surviving
test's matching assertion, kept 11 more. Fourteen tests were removed:

| Package | Removed |
| --- | --- |
| `internal/orchestrator` | 5 |
| `internal/cli` | 6 |
| `internal/connector/github` | 1 |
| `internal/runner` | 1 |
| `internal/web` | 1 |

Package statement coverage is unchanged by construction. Every removed test,
its surviving sibling, and both review verdicts are recorded in
[docs/validation/test-suite-audit/](validation/test-suite-audit/):
`near-duplicates.tsv` (the 195 candidates), `codex-review-keep.tsv` (first
pass), `codex-verify-25.tsv` (second pass with quoted evidence), and
`deleted.tsv` (the 14 removals).

## Why there are no coverage floors (2026-10-06)

The scheduled suite carried a 70 percent aggregate threshold, a 50 percent
package floor and exact-file floors of 90 percent on the safety-critical
orchestrator files. The first time every test passed after the develop
cleanup, the only red job was the floor check: a fixture package at 0 percent,
a two-function settings package at 0 percent, `internal/dispatchpriority` at
49.0 percent, and `internal/orchestrator/ranking.go` at 87.5 percent because a
refactor had moved its logic into `dispatchpriority` and left an 8-statement
wrapper under a 90 percent floor. None of those numbers said anything about
dispatch safety; the named regression tables and
`FuzzSafetyCriticalOrchestratorBoundaries` still ran and still passed.

OpenClaw reached the same conclusion at scale in 2026: about 400K lines of
agent-written tests were deleted with coverage essentially unchanged, because
models write a test for every change and then rewrite the test to match the
next change. Their deletion criteria are now the review rule in
[AGENTS.md](../AGENTS.md#validation): no assertion or self-comparison, expected
value computed by the code under test, breaks on a behavior-preserving
refactor, duplicates an existing assertion, or exports a private function only
for a test. Coverage remains a profile the scheduled suite records as
evidence for the next audit; it gates nothing.

The finding worth keeping: statement overlap is a poor proxy for duplication
in this suite. Of 1680 tests that add no unique statements to their package,
and of the 195 that looked like near-duplicates by every structural measure,
181 verified a distinct behavior. Deleting tests is not where the time is.

## Left alone, with reasons

- `TestWorkspaceLaneServesAHostedWorkspaceUntilItsLeaseIsLost` waits a real
  `workspacesession.HeartbeatInterval` (34s). The interval is a constant the
  client shares; shortening it in tests needs a production seam.
- `TestFreshInstallBootsOnboardingWizardAndRunsSubcommands` runs the real
  installer (about 30s). It is the only end-to-end install check.
- `internal/config/configdoc` re-runs the generated-docs check inside a test
  (22s); `make check-generated` already enforces the same thing.
- `internal/invariants` type-checks the repository with `go/packages` (18s).
  It is the invariant gate's enforcement and stays.
