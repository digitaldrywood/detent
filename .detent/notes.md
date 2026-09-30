# Issue #3257 merge fallback

- PR: https://github.com/digitaldrywood/detent/pull/3362; verified its body already includes `Fixes #3257`.
- Published PR head verified for this retry: `c41f2476072d35767bde248137ff2cc13f25ea63`; PR #3362 remains open against `develop`.
- Fetched target for this retry: `origin/develop` at `c4455b9277dbcb7a9622a8d5a369bb1a7283cc5c`.
- Prior merge `c41f2476072d35767bde248137ff2cc13f25ea63` preserves original published head `2675c3705982d7eb2b615a17fe442cfa4e9c9036` and prior target `d14138a37c7597542c30e19616271a971b62fc8f` as parents. The retry merges the latest target into that published resolution; no rebase or push performed.
- Retry conflict: only `.detent/notes.md` conflicted. Retained the incoming #3273 handoff as historical prose and one current Workpad/status fence. Develop source and invariant changes merged automatically.
- Prior resolution key files: `internal/cli/boot.go` and `internal/cli/hub_client.go` combine the runner intake credential source with develop's runner-problem reporting and checkout repository callback. `internal/hubclient/scheduler.go` retains both branches' fields and initialization.
- Migration blocker: develop already owns versions 46 (runner problems) and 47 (checkout repository). Renamed the PR's linked-source migration from 46 to `internal/hubserver/migrations/00048_linked_issue_sources.sql` without changing its SQL; `internal/hubserver/migrate.go` now supports version 48. The published PR had a version-45 support constant despite containing migration 46; that head could not initialize a version-46 database through `runMigrations`.
- Prior generated conflict: regenerated `static/app/conversation/app.js` from merged source using `npm run build`. Asset generation succeeded; this is not gate or test credit. Build warnings concerned CSS highlight selectors, font resolution, component sourcemaps, and chunk sizes.
- Prior notes verified: the inherited #3401, #3252, #3403, and #3273 notes below describe other work and do not establish validation for #3257 or the resolved head. Historical status fences are removed so this file contains one current Workpad status.
- Open items: Detent's ancestry/cleanliness verification, bounded validation, current-head checks, and lease-protected publishing. No out-of-scope finding identified; no issue-state changes or PR merge performed.

## Codex Workpad

Plan: preserve the committed prior source resolution, merge freshly fetched develop into the published PR head, resolve the handoff-notes conflict, and commit the merge.

Validation: no tests, asset generation, local gate (including `true`), or CI checks run during this retry. Prior source and migration resolution verified from the existing merge commit and its parents. Prior PR-body diagnostics apply only to their recorded head. Quiet-window, gate/CI, slow-check, and post-merge CI timings are not measured here. Detent owns validation and publishing after return.

```detent-status
schema: 1
status: complete
blockers: []
human_action: null
```

# Historical notes inherited from develop

# Issue #3401 merge fallback

- PR: https://github.com/digitaldrywood/detent/pull/3420; body already includes `Fixes #3401`.
- Merge target: fetched `origin/develop` at `cd50f7de8`; published PR head `bba9c169a9574e5b4529e435fe9622513c734ada` is preserved as the first parent of the merge.
- Resolved `internal/cli/runner_test.go`, `internal/orchestrator/review_head_test.go`, and `internal/orchestrator/startup_observability_test.go` with the overlapping fixture repairs from develop (#3419): SSH runner delegate assertion, optional automated-review deadline behavior, and terminal issues in the cleanup feed.
- PR changes in `internal/hubserver/workspace_runner_lane_test.go` and `tools/checklock/makefile_test.go` merged without conflicts.
- Validation: no local gate, tests, or CI checks run in this merge-fallback session. Earlier PR-body validation applies only to its prior head. Detent owns resolved-head validation, current-head checks, and publishing after return.
- Open items: Detent verification and publication; no out-of-scope findings. The inherited #3252 notes below are historical and are preserved.

## Historical workpad prose

Plan: merge fetched develop into the published PR branch, resolve only overlapping fixture conflicts, and commit the resolution.

Validation: deferred to Detent by the merge-fallback instructions; no validation credit claimed for this head. Quiet-window, gate/CI, slow-check, and post-merge CI timings are not measured in this session.


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

## Historical workpad prose

- Issue #3403 / PR #3423 merge fallback: merge fetched `origin/develop` (`1eba695b60a6f903847178ac71ca297053c6ae3b`) into the published PR head (`b389174201070adc3aa1c325ba47b27ad17593c8`), preserving both as ancestors.
- Resolution: `internal/runner/ssh_workspace.go` retains develop's equivalent G204 justification on the unchanged clone command. `internal/cli/ssh_runner.go` retains the PR's G204 justification on the SSH command through the automatic merge.
- Prior notes above concern #3252; they are historical and do not establish validation for this resolved head. PR #3423 already includes `Fixes #3403`.
- Validation: no local gate, tests, or CI checks run during this fallback, as instructed. Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI waiting.
- Open items: Detent's verification and validation; no out-of-scope findings identified. No push, PR merge, or issue-state changes performed.

# Issue #3273 historical handoff

- Historical-source probe using v0.117.1 config/gate files found exactly three JSON differences versus develop: absent Worker.HostSelection -> "least_loaded", absent Worker.HostCaps -> null, and Gate.RequiredStatusChecks [] -> null. The live definition is not attached; its exact reported digest pair was not independently reproduced.
- `internal/config/runner_policy.go` normalizes a copy for the digest, collapsing least_loaded/absent host selection and preserving historical [] for empty required checks. Worker JSON omits empty host selection/caps, matching the existing LocalStatus omission. Runtime defaults, source matching, explicit policy inputs, approval, and runner grants are preserved.
- `TestRunnerPolicyUpgradeKeepsApprovedID` now pins an approval captured with historical sources and an explicit workspace root, reproducing the unchanged-source upgrade mismatch before the fix. Cases preserve absent/default equivalence and reject preference, caps, local status, required checks, command, workspace root, and prompt changes. It also passes with a second nested worker TMPDIR.
- Passed: `go test -p 4 ./internal/config/... ./internal/policy -count=1`; `go vet -p 4 ./internal/config/... ./internal/policy`; `git diff --check`. Configured gate is `true`; no full gates or CI were run. No generated inputs changed.
- Downstream approval/authorization diagnostics could not compile: `internal/runner/ssh_protocol.go:210:85: undefined: ErrSessionNoProgress`, already tracked by #3427. Added evidence under its existing fingerprint, without expanding this fix.
- INV-3 documents the policy normalization consolidation. Do not claim live Mac Cloud recovery before a repaired release is deployed and verified.
