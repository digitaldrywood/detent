# Issue #3211 merge fallback handoff, 2026-09-30

- Verified published PR #3281 head `2cbb7131b37b6e5b8d1554b36615aa11879d4816` has parents `27951a8e9f2950be91cd2a5830d3e161f93deda9` and previous target `d80e68213d2768b5ebf7e95313bf35f40042411f`. The earlier merge `27951a8e9` has the prior published head `d97be0deadf64576efb6da220c16b47be8b9ba50` and target `73d1c330b04253600438c12e0f774d3fe16b26d8` as parents, confirming the prior notes.
- Started source-clean without an in-progress merge or rebase. Fetched `origin/develop` at `d14138a37c7597542c30e19616271a971b62fc8f` and merged it into the assigned PR branch without rebasing; the published PR head remains the first parent.
- Only `.detent/notes.md` conflicted. Resolved it by keeping the current #3211 handoff and preserving other issues' historical notes below. Source files merged automatically; no manual source changes were needed.
- Prior resolution key files: `docs/invariants.md`, `internal/runner/prompt.go`, `internal/hubserver/migrations/00048_hosted_blocked_lane.sql`, `internal/hubserver/migrate.go`, and `internal/hubserver/hosted_review_lane_migration_test.go`. Prior schema 48 and 47→48 migration notes and validation claims apply to earlier heads only.

## Codex Workpad

Plan: commit the target merge and notes resolution, preserving the published PR head and fetched target as ancestors, then return immediately to Detent.

Validation: no tests, vet, local gate, or CI checks run or awaited in this merge fallback. Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI. No validation success is claimed for this head.

Open items: Detent must verify the committed head and source cleanliness, validate, and publish. No out-of-scope findings, PR merge, issue-state changes, or remote writes performed.

```detent-status
schema: 1
status: complete
blockers: []
human_action: null
```

# Historical target notes

These handoffs concern other issues and earlier heads; their validation claims and completion metadata do not apply to #3211's current head. Historical workpads are retained as prose under issue-specific headings.

# Issue #3401 merge fallback

- PR: https://github.com/digitaldrywood/detent/pull/3420; body already includes `Fixes #3401`.
- Merge target: fetched `origin/develop` at `cd50f7de8`; published PR head `bba9c169a9574e5b4529e435fe9622513c734ada` is preserved as the first parent of the merge.
- Resolved `internal/cli/runner_test.go`, `internal/orchestrator/review_head_test.go`, and `internal/orchestrator/startup_observability_test.go` with the overlapping fixture repairs from develop (#3419): SSH runner delegate assertion, optional automated-review deadline behavior, and terminal issues in the cleanup feed.
- PR changes in `internal/hubserver/workspace_runner_lane_test.go` and `tools/checklock/makefile_test.go` merged without conflicts.
- Validation: no local gate, tests, or CI checks run in this merge-fallback session. Earlier PR-body validation applies only to its prior head. Detent owns resolved-head validation, current-head checks, and publishing after return.
- Open items: Detent verification and publication; no out-of-scope findings. The inherited #3252 notes below are historical and are preserved.


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

# Issue #3403 merge fallback

- Issue #3403 / PR #3423 merge fallback: merge fetched `origin/develop` (`1eba695b60a6f903847178ac71ca297053c6ae3b`) into the published PR head (`b389174201070adc3aa1c325ba47b27ad17593c8`), preserving both as ancestors.
- Resolution: `internal/runner/ssh_workspace.go` retains develop's equivalent G204 justification on the unchanged clone command. `internal/cli/ssh_runner.go` retains the PR's G204 justification on the SSH command through the automatic merge.
- Prior notes above concern #3252; they are historical and do not establish validation for this resolved head. PR #3423 already includes `Fixes #3403`.
- Validation: no local gate, tests, or CI checks run during this fallback, as instructed. Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI waiting.
- Open items: Detent's verification and validation; no out-of-scope findings identified. No push, PR merge, or issue-state changes performed.
