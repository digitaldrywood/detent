# Workspace package timeout follow-up (#2686)

The reported failure on `ff1d4ec9` predates the fix for #2646. That revision's
`make test` invoked `go test ./...`, leaving workspace at Go's default ten-minute
package timeout. The recorded timeout after 601.512 seconds, a one-second-old
active subtest, and parallel-slot waits match the earlier cumulative suite
budget evidence; they do not establish an individual operation stall.

PR #2667 merged as `7f4e8a09f6d6ae15905587b7d2544a523d0f2000` on
2026-09-14 at 21:44:49 UTC. It is an ancestor of this dispatch's baseline,
`c15d3742011cc1912b847a92b4bd0aa342e0e9c1`.

## Existing consolidation

- `scripts/test-workspace.sh` owns the 20-minute workspace package budget and
  serial test parallelism, removing inherited `DETENT_API_TOKEN`.
- `make test` (and therefore `make check-fast`), `make test-race`, and
  `make test-cover` invoke that script and exclude workspace from the remaining
  package invocation.
- `scripts/test-race-cover.sh` uses the same script for workspace race and
  coverage runs. The Windows evidence run also uses it, with parallelism four.
- The testgate runner retains JSON events and timing summaries under `tmp/`.
  Tests, fixture workload, and individual operation deadlines remain intact.

No further budget change is needed to address the reported pre-fix baseline.

## Validation

Ran the exact `make check-fast` without a `GO_TEST` override. The workspace
runner records its effective package timeout in
`tmp/workspace-test-evidence/summary.json` and raw events in `combined.jsonl`.

The workspace package passed in **460.645 seconds** on Go 1.27.1,
darwin/arm64. The retained compact [summary](workspace-summary.json) confirms
`package_timeout: 20m0s`, `test_parallelism: 1`, and `outcome: pass`.
This sample completed inside ten minutes; it verifies the current runner and
suite, rather than independently reproducing the historical host contention.

The complete `make check-fast` passed (exit 0), including invariants, migration
and generated checks, build, lint, vet, and every package. Lint used the
repository-pinned Go 1.26.6 toolchain; the default host Go was 1.27.1. Race,
coverage, and nilaway remain merge-queue checks as required by this dispatch.
