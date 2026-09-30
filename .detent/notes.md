# Issue #3400 merge fallback handoff (attempt 7121, generation 51)

- Verified PR #3421 is open, targets `develop`, includes `Fixes #3400`, and has published head `33b7e0c024a6653911eb8017a5bf159f607e2c72`, matching the source-clean local branch. The preceding #3400 handoff describes the merge already present in this history.
- Merged freshly fetched develop commit `8a94591757c917387c3530454e30d25962ed0448` into the published head without rebasing.
- Only conflict: `.detent/notes.md`. Preserved both sides' historical handoffs, including incoming #3165 notes, and consolidated one current Workpad/status block. All source files merged automatically; no manual source edits were required.
- Validation: no tests, builds, local gate, CI checks, or waits run. Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI after return.
- Open items: Detent verification/validation/publication and native Windows scheduled validation. No new finding or out-of-scope investigation occurred. No push, PR merge, or issue/lane mutation performed.

# Issue #3400 merge fallback handoff (attempt 7114, generation 44)

- Verified PR #3421 is open, targets `develop`, includes `Fixes #3400`, and has published head `10364b20fa9e31f0c3567974e537b152b098a5fe`, matching the source-clean local branch. Prior handoffs describe merges already present in that history.
- Merged freshly fetched develop commit `c4455b9277dbcb7a9622a8d5a369bb1a7283cc5c` into the published head without rebasing.
- Only conflict: `.detent/notes.md`. Preserved historical handoffs, including incoming #3273 notes, and consolidated one current Workpad/status block. `docs/invariants.md`, `internal/config/config.go`, `internal/config/runner_policy.go`, and `internal/config/runner_policy_test.go` merged automatically; no manual source edits were required.
- Validation: no tests, builds, local gate, or CI checks run. Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI after return.
- Open items: Detent verification/validation/publication and native Windows scheduled validation. Incoming historical notes mention existing #3427; no new finding or out-of-scope investigation occurred. No push, PR merge, or issue/lane mutation performed.

# Issue #3400 merge fallback handoff (attempt 7103, generation 34)

- Verified PR #3421 is open, targets `develop`, includes `Fixes #3400`, and has published head `f68005c60c44dfe93c667666e928cefbac980066`, matching the source-clean local branch before this merge. The prior #3400 handoff is historical; the published head already contains its resolution.
- Merged freshly fetched develop commit `d14138a37c7597542c30e19616271a971b62fc8f` into the published PR head without rebasing, preserving published history.
- Only conflict: `.detent/notes.md`. Preserved the historical #3400 and incoming #3403 handoffs and consolidated the current Workpad. All source changes merged automatically, including develop's SSH subprocess justification in `internal/cli/ssh_runner.go`; no manual source edits were required.
- Validation: no tests, builds, local gate, or CI checks run. Prior macOS tests and Windows cross-compilation are historical and do not validate this head. Detent owns ancestry/cleanliness verification, bounded validation, lease-protected publishing, and current-head CI after return.
- Open items: Detent verification, validation/publication, and native Windows scheduled validation. No out-of-scope repair identified. No push, PR merge, or issue/lane mutation performed.

# Issue #3400 merge fallback handoff (attempt 7096, generation 27)

- Verified PR #3421 is open, targets `develop`, includes `Fixes #3400`, and has published head `c20d5099bb95e4991cb7def7d55488986b5690ca`. The workspace started source-clean with no rebase in progress; that head already includes the previous fallback merge described below.
- Merged freshly fetched develop commit `9b6db4547e39220a30d4d989c69526904687fe4f` into the published PR head, preserving published history without rebasing.
- Only conflict: `internal/cli/ssh_runner_test.go`. Kept the PR's native `sshGitHubCLIHelper` and develop's `sshTestProvider(ctx context.Context)` signature, matching the incoming context-aware call site and subprocess commands. Other target changes merged automatically; no production edits were added by conflict resolution.
- Validation: no tests, builds, local gate, or CI checks run in this fallback. Earlier macOS tests and Windows cross-compilation are historical and do not validate this head. Detent owns ancestry/cleanliness verification, bounded validation, lease-protected publishing, and current-head CI after return.
- Open items: Detent validation/publication of the resolved head and native Windows scheduled validation. No out-of-scope repair identified. No push, PR merge, or issue/lane mutation performed.

## Codex Workpad

Plan: merge fetched develop `8a94591757c917387c3530454e30d25962ed0448` into published PR #3421 head `33b7e0c024a6653911eb8017a5bf159f607e2c72` without rebasing. Resolve the notes-only conflict, preserve historical handoffs from both sides, and commit the merge with the published head as first parent.

Validation: deferred to Detent. No tests, builds, local gate, CI checks, or waits run in this session; historical results do not validate this head. Quiet-window, gate/CI, slow-check, and post-merge CI timings are not measured.

Open items: Detent ownership/cleanliness/ancestry verification, bounded validation, lease-protected publishing, current-head CI, and native Windows scheduled validation. Historical #3427 evidence was not reproduced or investigated here. No new out-of-scope repair identified. No push, PR merge, or issue/lane mutation performed.

```detent-status
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7121"
  completion_generation: "51"
blockers: []
human_action: null
```

# Issue #3401 merge fallback (historical target notes)

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


# Issue #3400 merge fallback handoff (attempt 7080, generation 12)

- Verified local and remote PR #3421 head at 85afc4271bf2f230dc8e7f73e86eb3882d364425, with a source-clean worktree and no rebase in progress. Earlier notes claiming current mergeability were historical.
- Merged freshly fetched develop commit 436986dea421d273686ce243b9f6725a1b4b616f into the existing PR branch, preserving the remote PR head as an ancestor. Restarted the uncommitted merge after the shared target ref advanced.
- Conflicts only: internal/cli/runner_test.go, internal/orchestrator/review_head_test.go, internal/orchestrator/startup_observability_test.go. Used develop's equivalent SSH wrapper/local-runner assertions, explicit optional automated-review mode matching deadline expectations, and active-candidate/terminal-cleanup fixture separation.
- Remaining PR code changes against the fetched target: internal/cli/boot_test.go and internal/cli/ssh_runner_test.go; native gh helper, explicit sh commands, Windows USERPROFILE/PATH handling, and POSIX sshd fixture skip. No production edits added by this resolution.
- Prior validation from the supplied #3400 notes: seven focused tests passed on macOS and both affected packages cross-compiled for Windows/amd64 at the original PR head. Those results are historical; native Windows execution remains pending.
- This fallback ran no tests, builds, local gate, or CI. Resolved-file conflict and whitespace inspection was clean. A broader Git whitespace inspection flagged inherited generated JavaScript in static/app/conversation/app.js; it was left unchanged and does not require conflict-resolution work.
- Detent owns resolved-head verification, bounded validation, lease-protected publishing, and CI after return. No push, PR merge, or issue/lane mutations were performed.
- Open items: Detent validation of the merged head and next native Windows scheduled validation. No additional repair work identified.

## Issue #3165 — prior merge fallback for PR #3320

- Verified the prior #3165 handoff before merging: the clean starting local head was 24b563e4ecf72eb0a0bdc7b4e79cf58fde2fce06, whose parents are published PR head c51d85d1c10df27965c8ad24519e68c779a553da and prior target 73d1c330b04253600438c12e0f774d3fe16b26d8. Earlier client, Go, and browser validation is historical evidence only.
- PR #3320 was open against develop on the expected isolated branch. Fetched develop and the PR ref; merged origin/develop at cd50f7de8776f4462b565d9a015765511f8c1509 into the local PR branch without rebasing published commits.
- This refresh merged automatically, including docs/config.md and internal/config/config.go. No unresolved conflicts, source edits, asset generation, or out-of-scope findings. The target introduced tracked .detent/notes.md with historical #3252 notes above; this section records the current #3165 handoff.
- Validation: no tests, local gate, builds, CI checks, or waits performed. No current-head validation credit claimed.
- Resolution committed as a merge preserving the published PR head, prior local merge, and fetched target as ancestors; source-clean handoff.
- Open items: Detent owns independent ownership, cleanliness, and ancestry verification, bounded validation, lease-protected publishing, current-head CI waiting, and eventual squash landing. No push, PR merge, issue state change, or tracker lane write performed here.

# Issue #3403 merge fallback (historical target notes)

- Issue #3403 / PR #3423 merge fallback: merge fetched `origin/develop` (`1eba695b60a6f903847178ac71ca297053c6ae3b`) into the published PR head (`b389174201070adc3aa1c325ba47b27ad17593c8`), preserving both as ancestors.
- Resolution: `internal/runner/ssh_workspace.go` retains develop's equivalent G204 justification on the unchanged clone command. `internal/cli/ssh_runner.go` retains the PR's G204 justification on the SSH command through the automatic merge.
- Prior notes above concern #3252; they are historical and do not establish validation for this resolved head. PR #3423 already includes `Fixes #3403`.
- Validation: no local gate, tests, or CI checks run during this fallback, as instructed. Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI waiting.
- Open items: Detent's verification and validation; no out-of-scope findings identified. No push, PR merge, or issue-state changes performed.

Historical status: complete; completion_work_attempt_id: 7091; completion_generation: 22; blockers: []; human_action: null.

## Historical workpad for #3165

- Plan: merge fetched `origin/develop` at `d14138a37c7597542c30e19616271a971b62fc8f` into the clean published PR #3320 head `d3a6fa74ad934a790ab322f4d7af2d92ee5f0f97`, preserving both as ancestors without rebasing.
- Prior #3165 notes verified against the starting merge's parents (`24b563e4ecf72eb0a0bdc7b4e79cf58fde2fce06` and `cd50f7de8776f4462b565d9a015765511f8c1509`). The fetched PR head matches the starting local head. PR #3320 remains open against develop on the expected branch and its body includes `Fixes #3165`.
- Resolution: only `.detent/notes.md` conflicted. Preserved historical handoffs from both sides and consolidated the active workpad here. All source files, including `static/app/conversation/app.js`, merged automatically; no manual source changes, asset generation, or out-of-scope findings.
- Validation: no local gate, tests, builds, CI checks, or waits performed. Earlier validation is historical only; no current-head validation credit claimed. Gate/CI, quiet-window, slow-check, and post-merge timings are unmeasured in this session.
- Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, current-head CI waiting, and eventual squash landing. No push, PR merge, issue-state change, or tracker lane write performed here.

Historical status: complete; blockers: []; human_action: null.

# Issue #3273 handoff (historical target notes)

- Historical-source probe using v0.117.1 config/gate files found exactly three JSON differences versus develop: absent Worker.HostSelection -> "least_loaded", absent Worker.HostCaps -> null, and Gate.RequiredStatusChecks [] -> null. The live definition is not attached; its exact reported digest pair was not independently reproduced.
- `internal/config/runner_policy.go` normalizes a copy for the digest, collapsing least_loaded/absent host selection and preserving historical [] for empty required checks. Worker JSON omits empty host selection/caps, matching the existing LocalStatus omission. Runtime defaults, source matching, explicit policy inputs, approval, and runner grants are preserved.
- `TestRunnerPolicyUpgradeKeepsApprovedID` now pins an approval captured with historical sources and an explicit workspace root, reproducing the unchanged-source upgrade mismatch before the fix. Cases preserve absent/default equivalence and reject preference, caps, local status, required checks, command, workspace root, and prompt changes. It also passes with a second nested worker TMPDIR.
- Passed: `go test -p 4 ./internal/config/... ./internal/policy -count=1`; `go vet -p 4 ./internal/config/... ./internal/policy`; `git diff --check`. Configured gate is `true`; no full gates or CI were run. No generated inputs changed.
- Downstream approval/authorization diagnostics could not compile: `internal/runner/ssh_protocol.go:210:85: undefined: ErrSessionNoProgress`, already tracked by #3427. Added evidence under its existing fingerprint, without expanding this fix.
- INV-3 documents the policy normalization consolidation. Do not claim live Mac Cloud recovery before a repaired release is deployed and verified.
