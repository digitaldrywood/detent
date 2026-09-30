# Issue #3211 merge fallback handoff, 2026-09-30

- Verified the previous local merge `27951a8e9f2950be91cd2a5830d3e161f93deda9` has published PR head `d97be0deadf64576efb6da220c16b47be8b9ba50` and previous target `73d1c330b04253600438c12e0f774d3fe16b26d8` as parents. Started source-clean, with no in-progress merge or rebase.
- Fetched PR #3281's target `origin/develop` at `d80e68213d2768b5ebf7e95313bf35f40042411f` and its unchanged remote PR head `d97be0deadf64576efb6da220c16b47be8b9ba50`. Merged the target into the assigned branch without rebasing. Git reported no conflicts; no source edits were needed.
- Prior resolution key files: `docs/invariants.md`, `internal/runner/prompt.go`, `internal/hubserver/migrations/00048_hosted_blocked_lane.sql`, `internal/hubserver/migrate.go`, and `internal/hubserver/hosted_review_lane_migration_test.go`. Prior notes recorded schema 48 and the migration's 47→48 range; those earlier validation claims apply only to earlier heads.
- The target introduces a tracked `.detent/notes.md` containing another issue's handoff. Its inherited contents are retained below as historical notes; this entry records the current #3211 handoff.

## Codex Workpad

Plan: finish and commit the clean target merge, preserving the published PR head as an ancestor, then return immediately to Detent.

Validation: no tests, vet, local gate, or CI checks run or awaited in this merge fallback. Detent independently owns head verification, bounded validation, lease-protected publishing, and current-head CI. No validation success is claimed for this head.

Open items: Detent must verify the committed head and source cleanliness, validate, and publish. No unrelated findings, PR merge, issue-state changes, or remote writes performed.

```detent-status
schema: 1
status: complete
blockers: []
human_action: null
```

# Historical target notes

# Issue #3252 handoff

- `internal/runner/session_brake.go` held the separate no-progress ticker that canceled a live gate queue wait. The runner no longer creates that ticker or emits `session_no_progress`; duration and turn limits remain.
- `internal/orchestrator/session_brake.go` no longer maps that retired error to an issue no-progress attempt and Rework transition. The old `agent.no_progress_timeout_ms` config key is accepted only for compatibility and ignored; onboarding/templates omit it.
- Regression: `TestRunnerGateWaitOutlivesFormerNoProgressLimit` models #2976/#3052, a committed PR head first in the gate queue beyond the old threshold, then verifies validation starts on release. `TestRunnerGateFailureReportsCommand` preserves failing-command reporting.
- `make generate` passed. Before rebase, full runner, orchestrator, and config package tests passed; focused CLI onboarding and doctor tests passed. After rebase, runner and orchestrator passed, while new develop test `TestRunnerPolicyUpgradeKeepsApprovedID` failed with a fixed-ID mismatch (#3273). Full CLI tests failed in known #3231 because worker TMPDIR is inside this worktree; the process later timed out traversing the native cache. `go vet` passed for all four touched Go package trees.
- The INV-3 source policy fingerprint for `handleSessionBrake` was updated. PR #3277 is non-draft. The stale onboarding step was corrected in `c399e6da0`; its review thread is resolved.
- Latest verification: full runner and orchestrator tests, focused onboarding/doctor tests, and vet for all four touched Go package trees passed. The config suite reproduced the independent fixed-ID mismatch tracked in #3273.
- Operator correction: project gates and CI do not block repairs; `gate.run` is now `true` with no local status. The former full validation run was interrupted by worker cancellation. Do not resume it or report it as passing. Earlier shorter-command and `local-gate` claims are historical, not evidence that `make check-fast` passed.
- After release: monitor `session_no_progress` Rework transitions and gate wait versus merged PR throughput. This cannot be measured against released code before merge/deployment.
- Recovery verification: both focused gate regressions passed again (`go test ./internal/runner/... -run '^(TestRunnerGateWaitOutlivesFormerNoProgressLimit|TestRunnerGateFailureReportsCommand)$' -count=1`). The recovered notes correction belongs to this issue and is being committed; no source changed during this retry. PR #3277 remains non-draft with its review thread resolved. The configured `true` gate is required immediately before push; it is not test evidence.
