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


## Issue #3165 — merge fallback for PR #3320

- Verified the prior #3165 handoff before merging: the clean starting local head was 24b563e4ecf72eb0a0bdc7b4e79cf58fde2fce06, whose parents are published PR head c51d85d1c10df27965c8ad24519e68c779a553da and prior target 73d1c330b04253600438c12e0f774d3fe16b26d8. Earlier client, Go, and browser validation is historical evidence only.
- PR #3320 was open against develop on the expected isolated branch. Fetched develop and the PR ref; merged origin/develop at cd50f7de8776f4462b565d9a015765511f8c1509 into the local PR branch without rebasing published commits.
- This refresh merged automatically, including docs/config.md and internal/config/config.go. No unresolved conflicts, source edits, asset generation, or out-of-scope findings. The target introduced tracked .detent/notes.md with historical #3252 notes above; this section records the current #3165 handoff.
- Validation: no tests, local gate, builds, CI checks, or waits performed. No current-head validation credit claimed.
- Resolution committed as a merge preserving the published PR head, prior local merge, and fetched target as ancestors; source-clean handoff.
- Open items: Detent owns independent ownership, cleanliness, and ancestry verification, bounded validation, lease-protected publishing, current-head CI waiting, and eventual squash landing. No push, PR merge, issue state change, or tracker lane write performed here.
