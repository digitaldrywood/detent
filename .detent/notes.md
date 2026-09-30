# Issue #3425 merge fallback

- PR: https://github.com/digitaldrywood/detent/pull/3429, targeting `develop`.
- Merged fetched `origin/develop` at `c4455b9277dbcb7a9622a8d5a369bb1a7283cc5c` into the published PR head `b3410d6065ff54d47f28646debded7966d442275`, preserving published history.
- The only conflict was `.detent/notes.md`; retained both the #3425 and #3401 handoffs and the shared historical #3252 notes. Source and `docs/invariants.md` merged automatically; no manual source changes.
- Validation: no local gate, tests, or CI checks run in this session. Prior validation below is historical and does not validate the merged head.
- Open items: Detent owns resolved-head verification, validation, publishing, and current-head CI waiting. No out-of-scope findings.

# Issue #3425 handoff

- Root cause established from read-only ledger and worker transcript: attempt 7018 / generation 25 published #3058's issue-body Workpad at 2026-09-30T13:58:57Z and verified it at 13:59:13Z, before completion at 13:59:24Z. Structured parsing only searched Workpad comments, so the durable receipt recorded no_progress / completed_clean_diff_without_pull_request. Restart reconciled terminal capacity at 14:14:16Z and could not restore an unaccepted delivery.
- Shared section parsing: internal/workpad/workpad.go, connector/github/issue_text.go, orchestrator/autopromote.go. Accepted signal and generation persist in work_attempts.go; completion_classification.go validates attribution and receipt fields. Promotion consumers compare accepted and current fields, keeping authorization and human-review gates.
- Regression TestOperationalBodyCompletionSurvivesRestart replays attempt 7018 / generation 25 and the 14:14:11Z restart in isolated SQLite, including reclamation of unrelated work. It covers timestamp-only edits, changed evidence, withdrawn authorization, invalid status, stale attempt/generation, and human-review routing. Live attribution extends the existing progress table; connector body parsing extends its existing parser table.
- INV-1 documentation and manifest updated. No new loop, lane writer, reason, or configuration.
- Focused completion-related tests across internal/workpad, internal/connector/github and internal/orchestrator passed; go vet passed for those trees. Both used a scratch Go overlay removing develop's obsolete ErrSessionNoProgress reference. Normal command fails to compile at ssh_protocol.go:210; follow-up #3427 records this independent build failure. The overlay is not shipped. No full gate, coverage or race suite ran; true is the configured gate and grants no test credit.
- Historical no_progress metadata is not retroactively accepted without durable attribution/acceptance proof. The live instance and tracker lanes were not modified.
- PR #3429: https://github.com/digitaldrywood/detent/pull/3429, targeting develop. Source diagnostics above cover the shipped source; subsequent notes edits do not change Go behavior. Final configured gate and current-head review/check evidence are recorded in the canonical issue Workpad. Skill draft: no — existing durable-tracker-authorizations guidance covers this method.

# Issue #3401 merge fallback

- PR: https://github.com/digitaldrywood/detent/pull/3420; body already includes `Fixes #3401`.
- Merge target: fetched `origin/develop` at `cd50f7de8`; published PR head `bba9c169a9574e5b4529e435fe9622513c734ada` is preserved as the first parent of the merge.
- Resolved `internal/cli/runner_test.go`, `internal/orchestrator/review_head_test.go`, and `internal/orchestrator/startup_observability_test.go` with the overlapping fixture repairs from develop (#3419): SSH runner delegate assertion, optional automated-review deadline behavior, and terminal issues in the cleanup feed.
- PR changes in `internal/hubserver/workspace_runner_lane_test.go` and `tools/checklock/makefile_test.go` merged without conflicts.
- Validation: no local gate, tests, or CI checks run in this merge-fallback session. Earlier PR-body validation applies only to its prior head. Detent owns resolved-head validation, current-head checks, and publishing after return.
- Open items: Detent verification and publication; no out-of-scope findings. The inherited #3252 notes below are historical and are preserved.

## Codex Workpad

Plan: merge fetched develop into the published PR branch, resolve only overlapping fixture conflicts, and commit the resolution.

Validation: deferred to Detent by the merge-fallback instructions; no validation credit claimed for this head. Quiet-window, gate/CI, slow-check, and post-merge CI timings are not measured in this session.

```detent-status
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7082"
  completion_generation: "14"
blockers: []
human_action: null
```

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

## Codex Workpad

- Issue #3403 / PR #3423 merge fallback: merge fetched `origin/develop` (`1eba695b60a6f903847178ac71ca297053c6ae3b`) into the published PR head (`b389174201070adc3aa1c325ba47b27ad17593c8`), preserving both as ancestors.
- Resolution: `internal/runner/ssh_workspace.go` retains develop's equivalent G204 justification on the unchanged clone command. `internal/cli/ssh_runner.go` retains the PR's G204 justification on the SSH command through the automatic merge.
- Prior notes above concern #3252; they are historical and do not establish validation for this resolved head. PR #3423 already includes `Fixes #3403`.
- Validation: no local gate, tests, or CI checks run during this fallback, as instructed. Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI waiting.
- Open items: Detent's verification and validation; no out-of-scope findings identified. No push, PR merge, or issue-state changes performed.

```detent-status
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7091"
  completion_generation: "22"
blockers: []
human_action: null
```

# Issue #3273 handoff

- Historical-source probe using v0.117.1 config/gate files found exactly three JSON differences versus develop: absent Worker.HostSelection -> "least_loaded", absent Worker.HostCaps -> null, and Gate.RequiredStatusChecks [] -> null. The live definition is not attached; its exact reported digest pair was not independently reproduced.
- `internal/config/runner_policy.go` normalizes a copy for the digest, collapsing least_loaded/absent host selection and preserving historical [] for empty required checks. Worker JSON omits empty host selection/caps, matching the existing LocalStatus omission. Runtime defaults, source matching, explicit policy inputs, approval, and runner grants are preserved.
- `TestRunnerPolicyUpgradeKeepsApprovedID` now pins an approval captured with historical sources and an explicit workspace root, reproducing the unchanged-source upgrade mismatch before the fix. Cases preserve absent/default equivalence and reject preference, caps, local status, required checks, command, workspace root, and prompt changes. It also passes with a second nested worker TMPDIR.
- Passed: `go test -p 4 ./internal/config/... ./internal/policy -count=1`; `go vet -p 4 ./internal/config/... ./internal/policy`; `git diff --check`. Configured gate is `true`; no full gates or CI were run. No generated inputs changed.
- Downstream approval/authorization diagnostics could not compile: `internal/runner/ssh_protocol.go:210:85: undefined: ErrSessionNoProgress`, already tracked by #3427. Added evidence under its existing fingerprint, without expanding this fix.
- INV-3 documents the policy normalization consolidation. Do not claim live Mac Cloud recovery before a repaired release is deployed and verified.
