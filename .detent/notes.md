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

## Historical workpad for #3401

Plan: merge fetched develop into the published PR branch, resolve only overlapping fixture conflicts, and commit the resolution.

Validation: deferred to Detent by the merge-fallback instructions; no validation credit claimed for this head. Quiet-window, gate/CI, slow-check, and post-merge CI timings are not measured in this session.

Historical status: complete; completion_work_attempt_id: 7082; completion_generation: 14; blockers: []; human_action: null.

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


## Issue #3165 — prior merge fallback for PR #3320

- Verified the prior #3165 handoff before merging: the clean starting local head was 24b563e4ecf72eb0a0bdc7b4e79cf58fde2fce06, whose parents are published PR head c51d85d1c10df27965c8ad24519e68c779a553da and prior target 73d1c330b04253600438c12e0f774d3fe16b26d8. Earlier client, Go, and browser validation is historical evidence only.
- PR #3320 was open against develop on the expected isolated branch. Fetched develop and the PR ref; merged origin/develop at cd50f7de8776f4462b565d9a015765511f8c1509 into the local PR branch without rebasing published commits.
- This refresh merged automatically, including docs/config.md and internal/config/config.go. No unresolved conflicts, source edits, asset generation, or out-of-scope findings. The target introduced tracked .detent/notes.md with historical #3252 notes above; this section records the current #3165 handoff.
- Validation: no tests, local gate, builds, CI checks, or waits performed. No current-head validation credit claimed.
- Resolution committed as a merge preserving the published PR head, prior local merge, and fetched target as ancestors; source-clean handoff.
- Open items: Detent owns independent ownership, cleanliness, and ancestry verification, bounded validation, lease-protected publishing, current-head CI waiting, and eventual squash landing. No push, PR merge, issue state change, or tracker lane write performed here.

# Issue #3015 handoff

- `internal/connector/github/statuslabel.go` requests five closing PR references in the initial status-label hydration query. Existing overflow requests still fetch 100 references per page. The timeline window and candidate observation query are unchanged.
- `internal/connector/github/statuslabel_test.go` covers six/seven references with the selected PR on the overflow page, preserving PR identity, lane timestamp, and actor. A separate regression checks every one of 106 references across two overflow pages and verifies cursor progression.
- The prior September 30 exact-operation comparison recorded 102 points at `first:100` and seven at `first:5` on the same 100 issues, with identical 57 PR references and 310 timeline events. No sampled issue overflowed; fixtures cover overflow. This retry did not repeat the live comparison.
- #3014 is Done, and the operator's September 29 comment authorizes proceeding. Its PR #3025 merge `5774d908b` is not an ancestor of the fetched develop branch. The measurement above is a direct-query comparison, not an instrumented runtime refresh baseline.
- Retry verification: the first-page, six/seven-reference, and 106-reference regressions passed with `-count=1` after rebasing onto develop `d14138a37`. The rebase left the GitHub connector and `go.mod`/`go.sum` unchanged relative to published head `cade2c6ba`. Earlier full connector tests, connector vet, and `make generate` are recorded in the issue Workpad; generation produced no tracked changes.
- PR #3426 targets develop and includes `Fixes #3015`. It is non-draft, with no actionable reviews or threads. The review bot reported its usage limit; automated review is not required for promotion. Current-head checks are absent and provide no test credit. The configured gate is `true`; no full suite, coverage, race suite, or `make check-fast` is required.
- Attempt 7098 corrected the stale local notes and refreshed the completion Workpad for generation 29.
- Retry attempt 7107, generation 37: fetched develop remains `d14138a37`; PR #3426 remains non-draft and mergeable at verified published head `2dba2ea9a`, with no actionable reviews, comments, or threads. Connector source and Go dependencies match the tested implementation exactly. No tests or live measurements were repeated because no implementation changed. This notes-only update refreshes the handoff; run the configured `true` gate on its committed head immediately before push. Current-head checks remain absent and provide no test evidence. No lane labels or tracker status fields are written. Merging and post-merge validation belong to the subsequent Merging stage.
- Retry attempt 7117, generation 47: PR #3426 had no actionable feedback at published head `590f19b82`. Rebased onto fetched develop `c4455b927` (#3428), then onto `8a9459175` (#3320) after develop advanced during publication. The second rebase conflicted only in these notes; preserved both historical handoffs. The connector and `go.mod`/`go.sum` still exactly match tested implementation `cade2c6ba`; retained existing regression, vet, generation, and direct-query measurement evidence without rerunning them. Updated these notes only. Run configured `true` on the committed head immediately before publishing with an explicit lease against intermediate published head `cc7270477eb1d88b59d150fb8dc8227468dc626d`, then re-inspect feedback and checks on that exact head. Current-head checks were absent at initial inspection and provide no test evidence. No lane labels or tracker status fields are written; merge and post-merge validation remain with the Merging stage.

## Historical workpad for #3403

- Issue #3403 / PR #3423 merge fallback: merge fetched `origin/develop` (`1eba695b60a6f903847178ac71ca297053c6ae3b`) into the published PR head (`b389174201070adc3aa1c325ba47b27ad17593c8`), preserving both as ancestors.
- Resolution: `internal/runner/ssh_workspace.go` retains develop's equivalent G204 justification on the unchanged clone command. `internal/cli/ssh_runner.go` retains the PR's G204 justification on the SSH command through the automatic merge.
- Prior notes above concern #3252; they are historical and do not establish validation for this resolved head. PR #3423 already includes `Fixes #3403`.
- Validation: no local gate, tests, or CI checks run during this fallback, as instructed. Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI waiting.
- Open items: Detent's verification and validation; no out-of-scope findings identified. No push, PR merge, or issue-state changes performed.

Historical status: complete; completion_work_attempt_id: 7091; completion_generation: 22; blockers: []; human_action: null.

## Codex Workpad

- Plan: merge fetched `origin/develop` at `d14138a37c7597542c30e19616271a971b62fc8f` into the clean published PR #3320 head `d3a6fa74ad934a790ab322f4d7af2d92ee5f0f97`, preserving both as ancestors without rebasing.
- Prior #3165 notes verified against the starting merge's parents (`24b563e4ecf72eb0a0bdc7b4e79cf58fde2fce06` and `cd50f7de8776f4462b565d9a015765511f8c1509`). The fetched PR head matches the starting local head. PR #3320 remains open against develop on the expected branch and its body includes `Fixes #3165`.
- Resolution: only `.detent/notes.md` conflicted. Preserved historical handoffs from both sides and consolidated the active workpad here. All source files, including `static/app/conversation/app.js`, merged automatically; no manual source changes, asset generation, or out-of-scope findings.
- Validation: no local gate, tests, builds, CI checks, or waits performed. Earlier validation is historical only; no current-head validation credit claimed. Gate/CI, quiet-window, slow-check, and post-merge timings are unmeasured in this session.
- Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, current-head CI waiting, and eventual squash landing. No push, PR merge, issue-state change, or tracker lane write performed here.

```detent-status
schema: 1
status: complete
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

# Issue #3427 handoff

- Removed the retired `ErrSessionNoProgress` entry from `internal/runner/ssh_protocol.go`; remaining SSH sentinel and structured-error encoding is unchanged. No invariant or enforcement change.
- Reproduced the recorded command on baseline `c4455b9277dbcb7a9622a8d5a369bb1a7283cc5c`: `go test ./internal/orchestrator -run '^TestOperationalBodyCompletionSurvivesRestart$' -count=1 -p 4` exited 1 with the reported undefined symbol.
- After the removal, the same command exits 0 and compiles the orchestrator, but reports `[no tests to run]`: that test belongs to separate work. This is compilation evidence only.
- Passed existing runner protocol diagnostics: `go test ./internal/runner -run '^(TestSSHErrorRoundTrip|TestSSHPeerConcurrentCallbacksAndDisconnect|TestSSHCallbackDoesNotPublishRemotePID|TestSSHRunResponseRetainsResultOnFailure)$' -count=1 -p 4` (0.406s package time). Existing tests cover sentinel identity, wrappers, structured errors, callbacks, disconnects, and failure results; no duplicate test added.
- No generated inputs changed. Configured gate is `true`; no full gates, coverage, race suite, or CI wait. No out-of-scope discovery or reusable skill draft.
- Source repair and focused diagnostics are complete. PR publication, current-head review, and completion for attempt 7112 / generation 42 are tracked in the canonical issue Workpad.
