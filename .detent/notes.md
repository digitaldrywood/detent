# Issue 3060 handoff

- Root path: `internal/orchestrator/autopromote.go` disabled the security audit gate for a clean Rework head with missing evidence, so Rework promotion never invoked `startSecurityAuditStage`. The existing merge gate still checks trusted evidence.
- Fix removes that bypass. `internal/orchestrator/rework_live_promotion_test.go` covers stale old-head run, pending current-head audit, and promotion after trusted pass. `internal/orchestrator/attempt_allowance_test.go` now expects audit start before Merging.
- `go test ./internal/orchestrator/...` and `go vet ./internal/orchestrator/...` passed. `make check-fast` passed 2026-09-24.
- The operator verified immutable trusted audit run 397 for PyroApex #2556, base `9378fc1f74bfcc5ab8f83b6370d59037b2d24fc4`, head `47461c60b4d7bbeeadd782a6f18974a7bbd277ad`: pass, started 10:50:09 UTC and completed 10:52:05 UTC. Detent merged the PR at 10:52:28 UTC as `d4b075967ad4715e04b99b49afaae6acf7a8ea31`. The observed defect was premature Rework → Merging at 10:40 while the audit was missing; the merge worker obtained the pass before merging. Worker workspaces cannot reach the production tailnet endpoint by design. The human waived direct worker verification of that already-merged PR and will check live behavior after release.
- Rework follow-up 2026-09-30: the current-head audit regression now supplies completed gate-wait state and verifies the dispatch planner returns `awaiting_gate` with no implementation dispatch while the audit runs. Focused audit, allowance, and completed-Rework dispatch regressions passed; focused vet passed. The initial added assertion failed because the audit-only fixture was not assigned to a worker; the fixture now explicitly enables candidate eligibility.
- Later mobile audit observations recovered through the existing lifecycle and do not demonstrate lost audit runs. This PR removes the independently reproducible promotion bypass, not read-model refresh lag. No outstanding actionable PR review remains. Current attempt gate is `true` (no validation/status publication); prior `make check-fast` evidence is historical. Current-head checks remain skipped, not passing test evidence; no merge-group pass is claimed. Rework completion hands off to the orchestrator and does not authorize worker lane mutation or merge.
- Skill draft: no — this follow-up adds a focused regression assertion, not a reusable procedure.

## Merge fallback 2026-09-30

- Merged fetched `origin/develop` at `9b6db4547e39220a30d4d989c69526904687fe4f` into the PR branch, preserving published PR head `335dd5a46a647bcc9d1b0d827fde08844db5b88f` as an ancestor.
- Only conflict: `.detent/notes.md` (add/add). Preserved this issue's handoff and the target branch's historical notes below. Source files merged automatically.
- Validation: no local gate, tests, or CI run in this session. Earlier validation above is historical; current-head checks remain skipped, not passing evidence. Detent owns resolved-head validation and publishing.
- Open items: Detent verification, validation, and publication. No out-of-scope findings.

## Codex Workpad

Plan: merge the target branch, resolve the notes conflict, and commit the resolution.

Validation: deferred to Detent under the merge-fallback instructions. No resolved-head test credit or merge-group pass claimed.

```detent-status
schema: 1
status: complete
blockers: []
human_action: null
```

## Historical target-branch notes

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
