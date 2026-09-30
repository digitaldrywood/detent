## Codex Workpad

Plan and result: Rework attempt 7298 / generation 7 verified and retained published PR #3455 head 57d60d8707a060c7a703921e82eebb281b0adbf9, then merged fetched develop 3ce76fc185cf1d090eacaa27779ac77cfc84587c. Rework arose from stale target ancestry, with no actionable review findings. Only handoff notes conflicted; both histories are preserved. The sign-in source matches the original implementation, and source/generated assets match the published head. Incoming workspace test repairs are retained unchanged from develop.

Key files: web/conversation/src/app/account/Login.tsx, app/entry/EntryScreens.tsx, app/main.tsx, app/index.css, internal/hubserver/hosted_ui.go, hosted_login.go, internal/cloudentry/login.go, tests/visual/hub-login.spec.js.

Validation: all 99 account/entry component tests and TypeScript checking passed (13.4s command, 4.89s Vitest). Focused Hub shell/HTTP/OIDC and shared-entry transaction/invitation diagnostics passed (Hub 2.276s, shared entry 1.468s); affected package vet passed (14.3s combined command). Linux/arm64 preview cross-compilation passed (23.5s). Four Linux Chromium cases using mcr.microsoft.com/playwright:v1.61.0-noble passed with snapshot updates disabled (6.3s suite, 7.2s command): both public screens, actual sign-in/sign-up provider requests, card errors, authenticated /login, mobile overflow, keyboard and accessibility. No pre-existing desktop baseline changed. Full issue diff and merge diff reviewed; whitespace checks passed. No generated inputs changed during this target refresh, so prior make generate evidence is retained. No invariant changes.

Handoff: commit this resolved merge, run configured true on that exact head immediately before push, then refresh the canonical issue Workpad with exact publication/ancestry and current-head review/check evidence. The gate is a no-op and gives no test credit. Initial current-head checks were absent, with no actionable reviews or threads; automated review reported its usage limit and is not required. No quiet window, PR CI wait, merge-group workflow, full gate, coverage or race suite applies. Orchestrator owns lane changes, squash merging and scheduled integrated-develop validation. No dependencies or out-of-scope findings. Preview evidence stays under supplied TMPDIR; live port 4000 was untouched.

Skill draft: no — existing preview and merge-resolution guidance covers this refresh.

```detent-status
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7298"
  completion_generation: "7"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs retained from both sides

## Historical Workpad

- Plan and result: retain unpublished merge `5a6b32a097fc13037bac02e65fead5b2c3eb97dd`, merge freshly fetched develop `05725ad76853799c54c45db74deebcad0035b4f6` into the existing PR history without rebasing, commit the merge, and return immediately to Detent.
- Verified PR #3455 is open against `develop` on the assigned isolated branch. Fetched published head is `0f3f6a6e720b487a609a4807fca6c5cb726e5b62`, an ancestor of the source-clean starting local head. The retained local merge's parents are that published head and prior target `69781ca9505d8356771e2e4d5c1552d5316cbac5`, matching the prior handoff. The preceding published merge's parent record also matches Git history. Prior diagnostics remain historical only.
- Current target merged automatically with no conflicts or blockers. Retained the existing source and generated asset resolution, with no manual source edits, asset generation, or out-of-scope findings. Updated only these handoff notes beyond the automatic merge, preserving historical records below.
- Key issue files remain `web/conversation/src/app/account/Login.tsx`, `web/conversation/src/app/entry/EntryScreens.tsx`, `web/conversation/src/app/main.tsx`, `web/conversation/src/app/index.css`, `internal/hubserver/hosted_ui.go`, `internal/hubserver/hosted_login.go`, and `internal/cloudentry/login.go`. These files and `static/app/conversation/app.js` / `app.css` match the fetched published PR head.
- Validation: no tests, lint, vet, typecheck, builds, local gate (including `true`), CI checks, or validation waits run in this session. No current-head validation credit claimed; gate/CI and post-merge timings are unmeasured.
- Open items: Detent owns independent branch ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.
# Issue #3452 Rework handoff — 2026-09-30

## Historical Workpad

Plan: attempt 7270 / generation 26 refreshes ready PR #3469 after merge_fast_path_head_not_ready. Verified clean starting local/published head 6d0197539304ed9162a089c05fbbfe2ed60aaa6f. Merged fetched develop f6c8aa5746b049b71fad5c7704fe74054a5630e6, preserving published history. Develop advanced during publication; merged second fetched target 69781ca9505d8356771e2e4d5c1552d5316cbac5 into published refresh bc1bb6f680d71a5dfa87f9ff67fb41489b046c8c. Both merges conflicted only in notes; retained historical handoffs. No manual source edits were needed.

Key files: internal/workspace/land_github_test.go, internal/workspace/retention_test.go, internal/workspace/workspace_test.go. Issue source matches the published repair; complete source diff reviewed. Native gh helper retains existing landing/API-refusal assertions. Retention fixtures compare observed permissions and restrict POSIX denial semantics to POSIX hosts.

Validation: Go 1.26.6 focused landing/API-refusal/quarantine tests passed (7.603s package, 8.467s command); workspace vet passed (0.299s); Windows/amd64 test cross-compilation passed (0.615s), with output under supplied TMPDIR. GOMAXPROCS=4 and -p 4. Full issue diff reviewed; whitespace check passed. Second target merge leaves all three issue workspace files unchanged from the tested source; diagnostics were not repeated. Incoming generated template output is retained unchanged from develop. Configured true runs on the committed head immediately before push. Cross-compilation proves build compatibility only; native Windows acceptance remains with the next scheduled validation. No full suite, coverage, race suite or CI wait.

Handoff: ready PR #3469 targets develop and includes Fixes #3452. At initial inspection, no actionable reviews or unresolved threads existed; automated review hit its usage limit and is not required. Current-head checks were absent, an expected skip with no test credit. Exact committed-head gate, publication verification and final feedback evidence belong in the canonical issue Workpad. No quiet window, PR CI or merge-group workflow applies. Orchestrator owns lane changes, squash merging, and scheduled issue closure. No dependencies, generated inputs, invariant changes or out-of-scope findings.

Skill draft: no — existing native-helper and portable-fixture guidance covers this repair.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7270"
  completion_generation: "26"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs

These records concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

## Historical Workpad (#3440 retained unpublished merge)

- Plan and result: merge freshly fetched develop `69781ca9505d8356771e2e4d5c1552d5316cbac5` into published PR #3455 head `0f3f6a6e720b487a609a4807fca6c5cb726e5b62`, preserving published history without rebasing, commit the resolution, and return immediately to Detent.
- Verified PR #3455 is open against `develop` on the assigned isolated branch. Starting local HEAD and fetched published PR head match, with a source-clean workspace and no merge or rebase in progress. Verified the preceding merge's parents `6452077fd8df58d6680f7e726ee0bfb8e39691b7` and `09011d003ef74adb8a0fab414e7be80cbce6661a`; the earlier resolution's parents match its notes. Key issue source files and generated assets are unchanged between those two preceding resolutions. Historical diagnostics do not validate this head.
- Only conflict: `.detent/notes.md`. Preserved both sides' historical handoffs, including prior #3440 and incoming #3448 records, with one current Workpad/status fence. All source files and generated assets merged automatically; no manual source edits, asset generation, or out-of-scope findings.
- Key issue files remain `web/conversation/src/app/account/Login.tsx`, `web/conversation/src/app/entry/EntryScreens.tsx`, `web/conversation/src/app/main.tsx`, `web/conversation/src/app/index.css`, `internal/hubserver/hosted_ui.go`, `internal/hubserver/hosted_login.go`, and `internal/cloudentry/login.go`; the preceding generated asset resolution is retained.
- Validation: no tests, lint, vet, typecheck, builds, local gate (including `true`), CI checks, or validation waits run in this session. No current-head validation credit claimed; gate/CI and post-merge timings are unmeasured.
- Open items: Detent owns independent branch ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.
# Issue #3452 merge fallback handoff — 2026-09-30

- PR #3469 is open against `develop`, on the assigned isolated branch, and includes `Fixes #3452`. Source-clean starting local HEAD and fetched published PR head match `9c62696155653bfd09b10fb0666edebe6ab305cc`. No merge or rebase was in progress at session start.
- Prior #3452 notes match the published commit's three workspace test files: `internal/workspace/land_github_test.go`, `internal/workspace/retention_test.go`, and `internal/workspace/workspace_test.go`. Their diagnostics are historical and do not validate this resolved head.
- Merged fetched target `09011d003ef74adb8a0fab414e7be80cbce6661a` into the published PR history without rebasing; the published head is preserved as the first parent.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs and retained one current Workpad/status fence. All source files merged automatically; no manual source edits, generated-input changes, or out-of-scope findings.

## Historical Workpad (#3452)

Plan and result: finish and commit the resolved target merge, preserving the published PR head and fetched target as ancestors, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run in this session. Historical diagnostics below do not validate this resolved head. Gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. Next scheduled native Windows validation confirms the portability repair. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs

The following records concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

## Historical Workpad (#3440 preceding merge)

- Plan and result: merge freshly fetched develop `09011d003ef74adb8a0fab414e7be80cbce6661a` into published PR #3455 head `6452077fd8df58d6680f7e726ee0bfb8e39691b7`, preserving published history without rebasing, commit the resolution, and return to Detent.
- Verified PR #3455 is open against develop on the assigned isolated branch. Starting local and fetched published head match; the workspace was source-clean with no merge or rebase in progress. The preceding resolution's parents are `78cb74b12f414df43b128e9cd7cbe2ef49c6fae6` and `0721153854edcd0bd1fecd81edc90edbfc1853ba`, matching its notes. The recorded implementation parent `a80b710099516938a38c8cadb785f7fa61a56386` also matches Git history. Historical validation is not evidence for this head.
- Only conflict: `.detent/notes.md`. Retained both sides' historical handoffs, including the preceding #3440 resolution and incoming #3451/#3447 records, with one current Workpad/status fence. All source files and generated assets merged automatically; no manual source edits, asset generation, or out-of-scope findings.
- Key issue files remain `web/conversation/src/app/account/Login.tsx`, `web/conversation/src/app/entry/EntryScreens.tsx`, `web/conversation/src/app/main.tsx`, `web/conversation/src/app/index.css`, `internal/hubserver/hosted_ui.go`, `internal/hubserver/hosted_login.go`, and `internal/cloudentry/login.go`. These files match the prior implementation; the preceding generated asset resolution is retained.
- Validation: no tests, lint, vet, typecheck, builds, local gate (including `true`), CI checks, or validation waits run in this session. No current-head validation credit claimed; gate/CI and post-merge timings are unmeasured.
- Open items: Detent owns independent branch ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs retained from both sides

The following records concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

## Historical Workpad

- Plan and result: merge fetched develop into PR #3455 for issue #3440, commit the conflict resolution, and return immediately to Detent with a source-clean workspace.
- Verified PR #3455 is open against `develop` on the assigned isolated branch. The source-clean starting local head and remote PR head both match `78cb74b12f414df43b128e9cd7cbe2ef49c6fae6`; its parent is `a80b710099516938a38c8cadb785f7fa61a56386`, matching the prior #3440 rebase notes. Prior validation is historical only.
- Merged freshly fetched target `0721153854edcd0bd1fecd81edc90edbfc1853ba` into the published PR history without rebasing; the remote PR head is retained as the first parent.
- Conflicts: `.detent/notes.md`, `static/app/conversation/app.js`, and `static/app/conversation/app.css`. Preserved both sides' historical handoffs, including #3440 and incoming #3408, with one current Workpad/status fence. All source files merged automatically; no manual source changes or out-of-scope findings.
- Key issue files: `web/conversation/src/app/account/Login.tsx`, `web/conversation/src/app/entry/EntryScreens.tsx`, `web/conversation/src/app/main.tsx`, `web/conversation/src/app/index.css`, `internal/hubserver/hosted_ui.go`, `internal/hubserver/hosted_login.go`, and `internal/cloudentry/login.go` remain unchanged from the published PR head. Regenerated both conflicted client assets from the combined sources, retaining the incoming Sprites settings UI; no other generated assets changed.
- Validation: no tests, lint, vet, typecheck, local gate (including `true`), CI checks, or validation waits run. `npm run build` completed solely to regenerate the conflicted assets (4.9s command); this is not validation credit. It emitted CSS highlight, component sourcemap, font-resolution and chunk-size warnings, consistent with prior generation notes. Historical evidence does not validate this head; gate/CI and post-merge timings are unmeasured.
- Open items: Detent owns independent branch ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.
The following records concern earlier heads or other issues. Their validation and completion metadata do not apply to this resolved head or authorize validation or publication in this session.

# Issue #3452 Windows portability repair

- Recorded evidence: scheduled run 36758716559, Windows job 110035613055, failed eight GitHub landing cases and the API-refusal case by launching installed gh (exit 4, missing GH_TOKEN), plus two retention cases based on POSIX chmod semantics. The workspace script stopped before other packages ran.
- Key files: `internal/workspace/land_github_test.go` installs a native copy of the test executable as gh/gh.exe under a path containing spaces and asserts PATH resolution; `workspace_test.go` dispatches that helper before normal test initialization. Existing fixture responses, Git mutations, operation receipts, and landing assertions are preserved. No new sibling tests or production changes.
- `internal/workspace/retention_test.go` compares the symlink target's observed pre-sweep permissions and still checks preserved content. The chmod-denial fixture is explicitly POSIX-only; Windows does not deny directory removal through those mode bits.
- Diagnostics: original selected tests passed on macOS, confirming no local reproduction of the Windows failure. Repaired selected landing/API-refusal/quarantine tests, affected workspace vet, and Windows/amd64 test cross-compilation pass with Go 1.26.6 and a four-process budget. Cross-compilation is not native Windows execution. Final source diagnostics and exact-head publication are recorded in the canonical issue Workpad.
- No generated inputs or invariants changed; no full checks, coverage, race suite, Actions rerun, CI wait, or live-instance changes. Configured gate: `true`. No out-of-scope findings or dependencies.
- Open items: next scheduled native Windows validation confirms the repair. PR publication/readiness and completion belong to the canonical issue Workpad; merging and tracker lanes remain orchestrator-owned.
- Skill draft: no — existing native helper and portable fixture guidance cover this repair.


# Historical notes from incoming develop

# Issue #3448 merge fallback handoff — 2026-09-30

- Verified PR #3464 is open against `develop`, uses the assigned isolated branch, and includes `Fixes #3448`. Source-clean starting local and fetched published PR head: `09d2120cc0fe01058491ee8d0be6903388ff8df7`; no merge or rebase was in progress.
- Verified the preceding handoff against that merge's parents: prior published head `318380f53ea6b72576a673f1abe9faab9b0cd284` and prior target `0309a9b4882c39a96ed3b60c2b09fee1aa1a9f38`. The earlier merge and repair parent records also match Git history. Prior diagnostics remain historical only.
- Merged freshly fetched target `6fc38f8e772f5489b2ec42d5d27dcfdc7eff7ec5` into the published PR history without rebasing; the published head is retained as the first parent.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs, including incoming #3450 records, with one current Workpad/status fence. Source files and `docs/invariants.md` merged automatically. The issue's INV-3 completion-transition evidence and `internal/invariants/source_policy.json` fingerprint are retained. No manual source changes or out-of-scope findings.

## Historical Workpad (incoming develop)

Plan and result: finish and commit the resolved target merge, preserving published history and target ancestry, then return immediately with a source-clean workspace. Key issue files: `internal/invariants/source_policy.json` and INV-3 in `docs/invariants.md`.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run in this session. Historical diagnostics do not validate this head; gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent branch ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7234"
  completion_generation: "102"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs retained from both branches

These records concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

# Issue #3448 merge fallback handoff — 2026-09-30

- Verified PR #3464 is open against `develop`, uses the assigned isolated branch, and includes `Fixes #3448`. Source-clean starting local and fetched published PR head: `318380f53ea6b72576a673f1abe9faab9b0cd284`; no merge or rebase was in progress.
- Verified the preceding handoff against that merge's parents: published repair `dc3c6172c300086e1a9d88df8450e7cc37aea2c6` and prior target `09011d003ef74adb8a0fab414e7be80cbce6661a`. The repair's recorded parent `0721153854edcd0bd1fecd81edc90edbfc1853ba` also matches Git history. Earlier diagnostics remain historical.
- Merged freshly fetched target `0309a9b4882c39a96ed3b60c2b09fee1aa1a9f38` into the published PR history without rebasing, retaining the remote PR head as the first parent.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs, including incoming #3445 notes, with one current Workpad/status fence. All source files merged automatically. The preceding INV-3 resolution in `docs/invariants.md` and fingerprint in `internal/invariants/source_policy.json` remain unchanged from the published PR head. No manual source changes or out-of-scope findings.

## Historical Workpad (#3448 preceding merge)

Plan and result: finish and commit the resolved target merge, preserving published history and target ancestry, then return immediately with a source-clean workspace. Key issue files: `internal/invariants/source_policy.json` and INV-3 in `docs/invariants.md`.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run in this session. Historical diagnostics do not validate this head; gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent branch ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs retained from both branches

These records concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

# Issue #3448 merge fallback handoff — 2026-09-30

- Verified PR #3464 is open against `develop`, on the assigned isolated branch, and includes `Fixes #3448`. The clean starting local head and fetched published PR head both match `dc3c6172c300086e1a9d88df8450e7cc37aea2c6`. No rebase or merge was in progress after the restart.
- Verified the preceding #3448 retry notes against the starting commit: its parent is `0721153854edcd0bd1fecd81edc90edbfc1853ba`, and its changes are the fingerprint, INV-3 evidence, and handoff notes. Prior diagnostics are historical only.
- Merged freshly fetched develop `09011d003ef74adb8a0fab414e7be80cbce6661a` into the published PR history without rebasing. The published head is preserved as the first parent.
- Conflicts only: `.detent/notes.md` and `docs/invariants.md`. Preserved both sides' historical handoffs and consolidated their equivalent completed-run fingerprint explanations in INV-3. `internal/invariants/source_policy.json` already agrees on both branches and merged unchanged; no manual source edits or out-of-scope findings.

## Historical Workpad (#3448 prior merge)

Plan and result: finish and commit the resolved target merge, retaining the published PR head and fetched target as ancestors, then return immediately with a source-clean workspace. Key issue files: `internal/invariants/source_policy.json` and INV-3 in `docs/invariants.md`.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run in this session. Current-head checks were not inspected; historical diagnostics below do not validate this head. Gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7242"
  completion_generation: "1"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Issue #3450 current merge fallback handoff — 2026-09-30

- Verified PR #3461 is open against `develop`, on the assigned isolated branch, and includes `Fixes #3450`. Source-clean starting local and fetched published PR head: `feb9df0aa5e976a26f9a1cfe16c2fa4c2399fbaf`. No merge or rebase was in progress.
- Verified the preceding handoff against that head's parents: `eb17006372d1ee262080af5d46820b556abb952c` and prior target `09011d003ef74adb8a0fab414e7be80cbce6661a`. The recorded parents of `eb17006372d1ee262080af5d46820b556abb952c` also match Git history; the schema fixture matches the prior target. Earlier diagnostics are historical only.
- Merged freshly fetched target `0309a9b4882c39a96ed3b60c2b09fee1aa1a9f38` into the published PR history without rebasing; the published PR head is retained as the first parent.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs, including incoming #3445 records, with one current Workpad/status fence. All source files merged automatically; no manual source edits, generated-input changes, or out-of-scope findings.

## Historical Workpad (#3450 merge fallback)

Plan and result: finish and commit the resolved target merge, preserving published history and fetched target ancestry, then return immediately with a source-clean workspace.

Key issue file: `internal/hubserver/database_test.go`; the linked-source schema repair and project-secret table expectations are retained without manual edits.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run in this session. Historical evidence below does not validate the resolved head; gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, tracker lane write, or live-instance mutation performed here.

```yaml
schema: 1
status: complete
fields:
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs retained from both merge parents

The following records concern earlier heads or other issues. Their completion metadata and validation evidence do not apply to this head or authorize validation or publication in this session.

# Issue #3450 merge fallback handoff — restarted attempt 7243, generation 2

- Verified open PR #3461 targets `develop`, uses the assigned isolated branch, and includes `Fixes #3450`. Clean starting local and fetched published PR head: `eb17006372d1ee262080af5d46820b556abb952c`.
- Verified the prior resolution against Git history: starting head has parents `44aea6576c4afe4bb38578f82491f6bb65fff933` and `0721153854edcd0bd1fecd81edc90edbfc1853ba`; its schema fixture exactly matches that prior target. Earlier diagnostics are historical only.
- Merged freshly fetched develop `09011d003ef74adb8a0fab414e7be80cbce6661a` into the retained published history without rebasing. The published PR head remains the first parent.
- Only conflict: `.detent/notes.md`. Preserved both sides' historical handoffs, including incoming #3451 and #3447 records, and consolidated one current Workpad/status fence. All source files merged automatically; no manual source edits or out-of-scope findings.

## Historical Workpad

Plan and result: commit the resolved target merge while retaining the published PR head and fetched target as ancestors, then return immediately with a source-clean workspace.

Key file: `internal/hubserver/database_test.go`; the prior linked-source schema repair and incoming project-secret table expectations are retained without further edits.

Validation: no tests, lint, vet, builds, local gate (including `true`), current-head CI checks, or validation waits run in this restarted merge-fallback session. Historical evidence below does not validate this head; gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent branch ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, tracker lane write, or live-instance mutation performed here.

# Issue #3445 merge fallback handoff — 2026-09-30

- PR #3465 is open against `develop` on the assigned isolated branch and includes `Fixes #3445`. Source-clean starting local HEAD and fetched published PR head match `917ca781f3d8a49cf8042deebd398064dabeb889`; no merge or rebase was in progress.
- Verified the prior handoff against the starting merge's parents: original PR repair `10ef2151e8f179508e262e62a598d1f7a0ff9ad0` and prior target `0721153854edcd0bd1fecd81edc90edbfc1853ba`. The recorder fixture retains both the context-aware call/persistence assertions and the nonnil recorder assertion. Prior diagnostics are historical only.
- Merged freshly fetched target `09011d003ef74adb8a0fab414e7be80cbce6661a` into the published PR history without rebasing; the published PR head is retained as the first parent.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs, including incoming #3451/#3447 notes, with one current Workpad/status fence. All source files merged automatically; no manual source edits, generated-input changes, or out-of-scope findings.
- Key issue files remain `internal/runner/activity_profile.go`, its fixtures/callers, and the typed native-issue creation paths in `internal/hubserver`.

## Historical Workpad

Plan and result: finish and commit the resolved target merge, preserving published history and fetched target ancestry, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run in this session. Historical diagnostics below do not validate the resolved head; gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7243"
  completion_generation: "2"

  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs retained from both merge parents

The records below concern earlier heads or other issues. Their completion metadata and validation evidence do not apply to this head or authorize validation or publication in this session.

# Issue #3450 merge fallback handoff — 2026-09-30

- Verified PR #3461 is open, targets `develop`, includes `Fixes #3450`, and has fetched published head `44aea6576c4afe4bb38578f82491f6bb65fff933`, matching the clean starting local branch. No merge or rebase was in progress; the failed pre-check rebase was already absent.
- Verified prior implementation notes against the published commit: its only source change adds `linked_issue_sources` to `internal/hubserver/database_test.go`. Prior diagnostic results are historical and do not validate this merged head.
- Merged fetched develop `0721153854edcd0bd1fecd81edc90edbfc1853ba` into the published PR history without rebasing. The published PR head is retained as the first parent.
- Text conflict: `.detent/notes.md`. Preserved both sides' historical handoffs, including incoming #3446 notes, with one current Workpad/status fence.
- Required merge blocker: develop independently adds `linked_issue_sources` alongside migration 50's secret tables in the same schema fixture. Automatic merging duplicated that table. Removed the PR-position duplicate so the fixture exactly matches fetched develop, retaining one linked-source entry and both secret tables. All other source files merged automatically; no unrelated changes or out-of-scope findings.

## Historical Workpad

Plan and result: finish and commit the target merge and scoped resolution, preserving both the published PR head and fetched target as ancestors, then return immediately with a source-clean workspace.

Key file: `internal/hubserver/database_test.go`; the existing schema equality assertion retains the linked-source repair and incoming migration 50 tables.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run in this session. Historical evidence below does not validate this head; validation timings are unmeasured.

Open items: Detent owns independent branch ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

# Historical handoffs

The following records concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

# Issue #3448 handoff

- Key files: `internal/invariants/source_policy.json`, INV-3 in `docs/invariants.md`. Refreshed one reviewed dynamic-reason fingerprint; production code, approved reasons, scanner behavior, and generated inputs are unchanged.
- Recorded failure: scheduled run 36758716559 / Invariant Gate job 110035612795 on develop 3844f54e75edc8df931d7221c7f62dcb3d71b37e rejected `transitionCompletedActiveIssuesToReviewWithHydratedValidatorHeads` with hash `13f7c086bb3eeaa53ca70bcb5a404e96f7bf871f9371b0697d235ea21be710d9`; make exited 2.
- Reproduced at clean starting/fetched develop 640b8abc10f0adb4ad580d1835b349796062130e: `env -u DETENT_API_TOKEN GOMAXPROCS=4 go test -p 4 ./internal/invariants -run '^TestRepositorySources$' -count=1` failed with the identical function/hash (exit 1, 16.7s command wall time).
- Review evidence: formatting and hashing the merged function reproduces the reported hash. Removing only #3429's operational-receipt check reproduces the old approved hash `0163d1dac68ac92b9605cb98b44f3cde7e64439592383a717b4d6473fb79e59e`. The check rejects changed evidence before #3281's review routing; existing reason selection is preserved.
- Passed: all `internal/invariants` tests (5.591s package / 5.9s command); six focused orchestrator regressions for operational restart receipts, opted-out review routing, unfinished work, human-required review, unresolved threads, and running workers (0.864s package / 9.0s command); `git diff --check`.
- Existing tests assert the recorded failure and affected behavior. No duplicate test or new mechanism added. No full gate, coverage, race suite, Actions rerun, or live-instance mutation.

Publication and completion: the canonical GitHub Workpad on issue #3448 records the PR, final published head, configured `true` gate, and current-head feedback/check observation. It is authoritative for readiness and attempt 7229 / generation 97; these local notes record the tested source.

Open items: orchestrator promotion and merge, followed by the next scheduled full validation to confirm the integrated repair and close the issue. No dependency or out-of-scope discovery. Quiet window is not configured; the no-op gate takes under 1s. Slow checks, merge-group CI, and post-merge CI are not applicable to this In Progress delivery.

Skill draft: no — existing source-scan and fingerprint-review procedures cover this repair.

Retry verification: starting local HEAD and ready PR #3464 both matched `e467f8fd79830803279b0fe025f4969975398c06`. Rebased onto fetched develop `0721153854edcd0bd1fecd81edc90edbfc1853ba`; only these notes conflicted. Preserved both handoffs. The fingerprint repair and completion-transition source are unchanged; current-head invariant diagnostics and final publication evidence are recorded in the canonical Workpad. Automated review reported its usage limit, with no findings or review threads.

## Historical incoming develop handoff

# Issue #3445 merge fallback handoff — 2026-09-30

- PR #3465 is open against `develop` on the assigned isolated branch and includes `Fixes #3445`. Source-clean starting local HEAD and fetched published PR head match `10ef2151e8f179508e262e62a598d1f7a0ff9ad0`; no merge or rebase was in progress. Verified prior #3445 handoff against that commit's scoped lint repair and context regression; its diagnostics are historical only.
- Merged freshly fetched target `0721153854edcd0bd1fecd81edc90edbfc1853ba` into the published PR head without rebasing, preserving published history.
- Conflicts: `.detent/notes.md` and `internal/runner/activity_profile_test.go`. Retained the PR's context-aware recorder call and context persistence assertions together with develop's nonnil recorder assertion. Preserved both sides' historical handoffs. Other files merged automatically; no unrelated edits or out-of-scope findings.
- Key issue files remain `internal/runner/activity_profile.go`, its fixtures and callers, and the typed native-issue creation paths in `internal/hubserver`. No generated inputs or invariant enforcement changed through resolution.

## Historical Workpad

Plan and result: finish and commit the resolved target merge, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks or validation waits run in this session. No validation success claimed for the resolved head. Prior diagnostics below apply only to their recorded heads; gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent branch ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing and current-head CI waiting. No push, PR merge, issue-state change or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7232"
  completion_generation: "100"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs

These records concern earlier heads or other issues; their validation and completion metadata do not apply to the current resolved head or authorize validation or publication in this session.

# Issue #3445 historical implementation handoff

- Reproduced all seven findings from scheduled Lint job 110035612634 / run 36758716559 against clean develop 640b8abc10f0adb4ad580d1835b349796062130e using golangci-lint v2.9.0 built with Go 1.26.6. Focused runner/hubserver lint exited 1 with the same two contextcheck, two errcheck, one nilnil, and two staticcheck findings.
- Key files: `internal/runner/activity_profile.go` accepts the run context and checkpoints through a two-second `context.WithoutCancel` timeout; implementation/validation callers and fixtures supply their contexts. The validation-phase switch and delayed span pointer remove staticcheck findings. Read-only file cleanup has a scoped errcheck explanation.
- `internal/hubserver/native_issues.go` and `linked_issue_sources.go` return typed native issues, removing unchecked and redundant assertions in linked creation, conversation linking, GitHub import, workspace dispatch, and the existing archive fixture. The absent linked-source result remains valid and has a scoped nilnil explanation. No invariant or enforcement change.
- Extended `TestActivityRecorderDoesNotWaitForPersistence` to catch lost context attribution, canceled initial/final persistence after run cancellation, and an unbounded persistence deadline. The regression fails twice on the old background-context implementation (exit 1), then passes with the inherited, detached context. No sibling test added.
- Diagnostics passed: identical focused pinned lint reports zero issues; activity, linked creation/intake, native archive, conversation linking, workspace dispatch/items, and GitHub-import tests pass (runner 0.417s, hubserver 0.874s). Native issue mutation/history and field clearing also pass (hubserver 0.582s). Whitespace inspection passes. Lint includes govet. No generated inputs changed.
- Publication and current-head review/gate evidence belong to the canonical issue Workpad. Configured gate is `true`; no full suite, coverage, race suite, make check/check-fast, Actions rerun, or CI wait. The next scheduled run confirms integrated develop; lane transitions and merging remain orchestrator-owned. No out-of-scope discovery or live-instance mutation.
- Skill draft: no — existing Go and toolchain-alignment guidance covers this focused lint repair.

# Historical notes from develop

# Issue #3446 implementation handoff

- Reproduced all five scheduled NilAway findings from job 110035612674 / run 36758716559 on starting develop 640b8abc10f0adb4ad580d1835b349796062130e. Build and Vet passed in that run; NilAway was the failing step. Local pinned audit exited 1, with the same primary findings and grouped native-landing dereferences.
- Key files: internal/hubclient/{scheduler_test.go,native_change_test.go,native_landing_test.go}, internal/runner/activity_profile_test.go, scripts/nilaway-baseline.json. Existing tests now assert nonnil connector/execution/recorder values before dereferencing and exact machine report counts before indexing. Removed the retired native-change suppression; no replacement or new suppression.
- Pinned diagnostics: Go 1.26.6, GOMAXPROCS=4, NilAway v0.0.0-20260612163715-2d8907f431ca over internal/hubclient and internal/runner passes with zero diagnostics (5.8s). Five affected test names pass with -p 4 -count=1 and DETENT_API_TOKEN cleared (hubclient 1.681s, runner 0.464s; 16.3s command). Focused go vet -p 4 over both packages passes (12.7s). git diff --check passes.
- No production behavior, invariant enforcement, generated inputs, UI, or mechanism changed. No duplicate tests added: assertions extend existing fixtures; the pinned audit reproduces the recorded failure. No full gates, coverage, race suites, Actions reruns, or CI waiting.
- Configured gate is true, to run on the final committed head before push. PR publication, current-head feedback/check evidence and completion for attempt 7217 / generation 85 belong to the canonical issue Workpad. Next scheduled validation confirms the integrated repair and closes the issue; lane transitions and merge remain orchestrator-owned.
- No dependencies or out-of-scope findings. Skill draft: no — existing focused NilAway diagnostics and fixture assertions cover this routine repair.

## Historical handoffs

# Issue #2976 merge fallback handoff (attempt 7212, generation 80)

- PR #3052 is open against `develop` on the assigned isolated branch; its body includes `Fixes #2976`. Fetched published head: `99f99e34cea4fea794c0ddc7a8cea973c24e2957`.
- Source-clean starting local HEAD: `363438eab844ff57de1f9b1b71d90856920e77a5`, the retained unpublished merge with parents `99f99e34cea4fea794c0ddc7a8cea973c24e2957` and prior target `bb537faaff5f86411d65d165726ba1386fa60aad`. Verified the preceding handoff and retained source resolution against Git history; the remote PR head remains an ancestor. No merge or rebase was in progress at session start.
- Merged freshly fetched develop `a80b710099516938a38c8cadb785f7fa61a56386` into the retained local history without rebasing. No conflicts occurred; incoming `internal/gate/gate.go` and `internal/gate/gate_test.go` merged automatically. No manual source changes or out-of-scope findings.
- Key issue files remain `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`. Prior resolution retains develop's audit-only Merging path and the PR's In Progress repair-only filter in the non-Merging decision path.
- Updated handoff notes with one current Workpad/status fence; prior handoffs are historical.

## Historical Workpad (#2976 merge fallback)

Plan and result: retain the unpublished resolution, finish and commit the current develop merge while preserving the published PR head and fetched target as ancestors, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run in this session. Historical evidence does not validate this head. Gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing and current-head CI waiting. No push, PR merge, issue-state change or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7212"
  completion_generation: "80"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

## Historical handoffs

The following records apply to earlier heads or other issues and do not authorize validation or publication in this session.


# Additional historical handoffs from fetched develop

# Issue #3451 merge fallback handoff — 2026-09-30

- Verified PR #3467 is open against `develop`, on the assigned isolated branch, and includes `Fixes #3451`. Source-clean starting local and fetched published PR head: `2429cc3d68638044edef991268dcdbce35b8fbdd`. No rebase or merge was in progress; prior implementation notes match that commit's scoped changes. Their diagnostics are historical only.
- Merged freshly fetched target `3169b2f5f54519a049d156283658709f57828b71` into the published PR history without rebasing; the published head remains the first parent.
- Conflicts: `.detent/notes.md`, `ci_workflow_test.go`, `docs/invariants.md`, `internal/config/runner_policy_test.go`, `internal/orchestrator/blocked_cause_recovery_test.go`, and `internal/runner/prompt_test.go`. Retained develop's removal of the release-configuration text test and fixed capped-skills fixture; retained the PR's passing-audit recovery case. The historical approval fixture retains the PR's explicit historical opt-out label and zero-Review representation with develop's portable shell pins. Its existing label-removal case rejects changed review defaults; the incompatible post-change digest baseline is superseded by the historical baseline. INV-3 documents the combined resolution.
- Required automatic-merge blocker: removed the duplicate `linked_issue_sources` schema expectation; `internal/hubserver/database_test.go` now matches fetched develop exactly, retaining incoming project-secret tables.
- Notes preserve both sides' historical handoffs below. No unrelated source edits, generated-input changes, or out-of-scope findings.

## Historical Workpad (#3451 merge fallback)

Plan and result: finish and commit the resolved target merge, preserving the published PR head and fetched target as ancestors, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run in this session. Historical diagnostics below do not validate the resolved head. Gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7230"
  completion_generation: "98"

  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs

The following records concern earlier heads or other issues. Their validation and completion metadata do not apply to this merge or authorize validation or publication in this session.

# Issue #3450 handoff

- Key file: `internal/hubserver/database_test.go`. Added `linked_issue_sources` to the expected schema table list, matching the existing migration 49. Exact schema equality and SQLite configuration checks are retained. Production behavior, migrations, and invariants are unchanged.
- Recorded failure: scheduled run 36758716559, Verify race (0), job 110035613023, development commit `3844f54e75edc8df931d7221c7f62dcb3d71b37e`. The job had exactly one failing test: `TestOpenCreatesHubSchemaAndConfiguresSQLite`, whose expected tables omitted `linked_issue_sources`. No data race report was present.
- Reproduced unchanged baseline `640b8abc10f0adb4ad580d1835b349796062130e`: `env -u DETENT_API_TOKEN GOMAXPROCS=4 GOTOOLCHAIN=go1.26.6 go test -p 4 ./internal/hubserver -run '^TestOpenCreatesHubSchemaAndConfiguresSQLite$' -count=1` exited 1 with the same mismatch (11.0s command wall time).
- Passed after repair: `env -u DETENT_API_TOKEN GOMAXPROCS=4 GOTOOLCHAIN=go1.26.6 go test -p 4 ./internal/hubserver -run '^(TestOpenCreatesHubSchemaAndConfiguresSQLite|TestHubMigrationPreservesExistingData|TestLinkedIssueCreation|TestLinkedIssueIntakeAtomicAndNativeEdits)$' -count=1` (5.9s command, 1.207s package). Raw job and diagnostic logs are under provided TMPDIR. No new tests: the existing exact-schema regression reproduced the recorded failure.
- No generated inputs changed, no full suite/coverage/race suite, no Actions rerun or CI wait. Configured gate is `true` and publishes no status. The next scheduled suite confirms integrated behavior. No unrelated discovery, dependency, tracker lane writes, or live-instance mutation.

## Implementation handoff

- PR: https://github.com/digitaldrywood/detent/pull/3461 targets `develop` and includes Fixes #3450. The initial draft head `71f6ae83b3f5365563c27137f5fcf8cb6e831d71` was mergeable, with no reviews, comments, threads, or check rollup at inspection. Absent checks are expected and provide no test credit.
- Focused diagnostics above validate the final test source; this update changes notes only. Configured `true` passed immediately before initial publication and runs again on this committed notes update before push. Full checks and CI waits are disabled by repository policy. Quiet window not configured; no-op gate under 1s; PR/merge-group CI, slow checks, and post-merge main-CI not applicable to this worker handoff.
- Final readiness and exact-head review/check evidence, plus the completion receipt for attempt 7221 / generation 89, belong to the canonical issue #3450 Workpad. It is authoritative; this file records implementation evidence only.
- Open item: next scheduled validation confirms the integrated repair. Detent owns squash merge, tracker lane transitions, and scheduled green-run closure; the worker performs no lane writes.

Skill draft: no — the existing schema assertion directly reproduces the failure; no new reusable procedure.

# Historical handoffs

# Historical incoming develop handoff

The following records concern earlier heads or other issues. Their validation and completion metadata do not apply to this resolved head or authorize validation or publication in this session.

## Historical PR implementation handoff

# Issue #3451 handoff

- Reproduced all seven test failures from macOS job 110035613042 in scheduled run 36758716559 on clean develop `640b8abc10f0adb4ad580d1835b349796062130e`: focused diagnostics exited 1 in 20.7s with the recorded release-hook, policy-ID, schema-list, source-fingerprint, missing-audit, blank-label, and capped-skills failures.
- `internal/config/config.go` omits zero-valued Review from JSON, restoring the pre-field policy representation. The existing historical approval fixture explicitly preserves v0.117.1's opt-out label; added cases reject human-review activation and opt-out removal. Diagnostic digest comparison recovered the exact historical `f984256f...` digest only when the historical label was retained and the zero Review field omitted.
- Existing fixtures now reflect dependency-only release hooks, migration 49's linked_issue_sources table, explicit routine opt-out settings, expected skill-cap drops, and required-audit waits. The added passing-audit recovery case catches a failure to advance with trusted passing evidence; no new test functions or mechanisms were added.
- Refreshed the INV-3 fingerprint for the already-landed completion transition after reviewing #3429's receipt freshness and #3281's existing decision-reason routing. `docs/invariants.md` records this review and the policy representation boundary. No orchestrator production behavior changed.
- All seven targeted regressions pass (15.7s command). Adjacent config/policy/routine/skills package tests, prompt rendering, required-gate/audit evaluation, and schema constraints pass; config/policy/routine/skills vet passes (6.9s combined commands). Whitespace inspection passes. No generated inputs changed; no full gate, coverage, race suite, or Actions rerun/wait.
- Configured gate is `true`, to run on the committed head immediately before publication. Current-head check/review and final publication evidence belong in the canonical #3451 issue Workpad. Next scheduled integrated validation confirms the macOS repair and owns issue closure; orchestrator owns promotion, squash merge, and lane transitions.
- No dependencies, out-of-scope findings, or live-instance mutation. Skill draft: no — existing debugging guidance covers these fixture and serialization repairs.

## Historical handoffs from develop

The records below concern earlier heads and other issues; their completion and validation evidence does not apply to #3451.

## Historical incoming develop handoffs

# Issue #3447 implementation handoff

- Scheduled Test Coverage job 110035612791 failed on develop 3844f54e75edc8df931d7221c7f62dcb3d71b37e before threshold evaluation. Seven failures reproduce without instrumentation on current baseline 640b8abc1; the eighth (terminal helper exit 2) reproduces with focused coverage instrumentation. Canonical Workpad: digitaldrywood/detent#3447.
- Key files: ci_workflow_test.go removes the obsolete release-configuration text test; internal/config/runner_policy_test.go pins both shells and the post-#3281 approval, while asserting the intentionally changed defaults reject the old approval; develop #3456 already repairs internal/hubserver/database_test.go with linked_issue_sources; internal/routine/manager_test.go supplies its opt-out label; internal/orchestrator/blocked_cause_recovery_test.go holds missing audits; internal/runner/prompt_test.go uses a fixed capped skill fixture; internal/workspaceterminal/close_unix_test.go gives coverage teardown writable stdout after its PTY closes.
- INV-3: reviewed #3281's dynamic completion-transition reason selection and refreshed only its source fingerprint in internal/invariants/source_policy.json, documented in docs/invariants.md. No production Go, runtime behavior, mechanism, reason code, configuration, or generated input changed.
- Passed: selected regressions across root/config/Hub/invariants/orchestrator/routine/runner/terminal packages (11.0s command); terminal close with coverage enabled, both close modes, five repetitions (2.0s command). Baseline reproduction exited 1; focused instrumented terminal reproduction exited 1. Shell pinning is portable fixture setup, not native Windows validation.
- Configured gate is true. No full suite, coverage threshold gate, race suite, make check/check-fast, Actions rerun, or CI wait. Run true on the committed head before publication. Publication rebase onto develop 072115385 conflicted only in notes; preserved incoming historical handoffs. The first rebased diagnostic exited 1 because the automatic merge duplicated linked_issue_sources, now already supplied by develop #3456 alongside project_secrets. Removed this PR's duplicate so the Hub schema fixture matches develop exactly. All other selected diagnostics passed on the rebased source (11.3s command); the resolved Hub schema regression then passed (5.8s command). Other issue source files remain identical to the previously tested repair, including the instrumented terminal helper. Ready promotion, current-head review/check evidence, and completion for attempt 7218 / generation 86 belong to the canonical issue Workpad. The next scheduled validation confirms the integrated repair; the orchestrator owns merge and lane transitions.
- No dependencies or out-of-scope findings. Live port 4000 untouched. Scratch stayed in provided TMPDIR. Skill draft: no — existing guidance covers focused fixture repair and helper output teardown.

# Historical notes

# Issue #3446 implementation handoff

- Reproduced all five scheduled NilAway findings from job 110035612674 / run 36758716559 on starting develop 640b8abc10f0adb4ad580d1835b349796062130e. Build and Vet passed in that run; NilAway was the failing step. Local pinned audit exited 1, with the same primary findings and grouped native-landing dereferences.
- Key files: internal/hubclient/{scheduler_test.go,native_change_test.go,native_landing_test.go}, internal/runner/activity_profile_test.go, scripts/nilaway-baseline.json. Existing tests now assert nonnil connector/execution/recorder values before dereferencing and exact machine report counts before indexing. Removed the retired native-change suppression; no replacement or new suppression.
- Pinned diagnostics: Go 1.26.6, GOMAXPROCS=4, NilAway v0.0.0-20260612163715-2d8907f431ca over internal/hubclient and internal/runner passes with zero diagnostics (5.8s). Five affected test names pass with -p 4 -count=1 and DETENT_API_TOKEN cleared (hubclient 1.681s, runner 0.464s; 16.3s command). Focused go vet -p 4 over both packages passes (12.7s). git diff --check passes.
- No production behavior, invariant enforcement, generated inputs, UI, or mechanism changed. No duplicate tests added: assertions extend existing fixtures; the pinned audit reproduces the recorded failure. No full gates, coverage, race suites, Actions reruns, or CI waiting.
- Configured gate is true, to run on the final committed head before push. PR publication, current-head feedback/check evidence and completion for attempt 7217 / generation 85 belong to the canonical issue Workpad. Next scheduled validation confirms the integrated repair and closes the issue; lane transitions and merge remain orchestrator-owned.
- No dependencies or out-of-scope findings. Skill draft: no — existing focused NilAway diagnostics and fixture assertions cover this routine repair.

## Historical handoffs

# Issue #2976 merge fallback handoff (attempt 7212, generation 80)

- PR #3052 is open against `develop` on the assigned isolated branch; its body includes `Fixes #2976`. Fetched published head: `99f99e34cea4fea794c0ddc7a8cea973c24e2957`.
- Source-clean starting local HEAD: `363438eab844ff57de1f9b1b71d90856920e77a5`, the retained unpublished merge with parents `99f99e34cea4fea794c0ddc7a8cea973c24e2957` and prior target `bb537faaff5f86411d65d165726ba1386fa60aad`. Verified the preceding handoff and retained source resolution against Git history; the remote PR head remains an ancestor. No merge or rebase was in progress at session start.
- Merged freshly fetched develop `a80b710099516938a38c8cadb785f7fa61a56386` into the retained local history without rebasing. No conflicts occurred; incoming `internal/gate/gate.go` and `internal/gate/gate_test.go` merged automatically. No manual source changes or out-of-scope findings.
- Key issue files remain `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`. Prior resolution retains develop's audit-only Merging path and the PR's In Progress repair-only filter in the non-Merging decision path.
- Updated handoff notes with one current Workpad/status fence; prior handoffs are historical.

## Historical Workpad (#2976 merge fallback)

Plan and result: retain the unpublished resolution, finish and commit the current develop merge while preserving the published PR head and fetched target as ancestors, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run in this session. Historical evidence does not validate this head. Gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing and current-head CI waiting. No push, PR merge, issue-state change or tracker lane write performed here.

Historical status: complete; attempt 7212; generation 80; cleanliness resolution committed; blockers []; human_action null.

## Historical handoffs

The following records apply to earlier heads or other issues and do not authorize validation or publication in this session.

# Issue #2976 merge fallback handoff (attempt 7206, generation 77)

- PR: https://github.com/digitaldrywood/detent/pull/3052; verified open against `develop` on the assigned isolated branch. Source-clean starting local HEAD and fetched published PR head both match `99f99e34cea4fea794c0ddc7a8cea973c24e2957`. No merge or rebase was in progress at session start.
- Verified the preceding handoff against Git history: starting HEAD has parents `286ca8e40567e6c34decc77591eccdb982737a17` and prior target `3844f54e75edc8df931d7221c7f62dcb3d71b37e`; the preceding recorded merge parents also match.
- Merged fetched develop `bb537faaff5f86411d65d165726ba1386fa60aad` into the published PR history without rebasing. Conflicts were limited to `.detent/notes.md` and `internal/orchestrator/autopromote_tick.go`.
- Source resolution retains develop's audit-only Merging decision path and confines the PR's In Progress repair-only filter to the full promotion decision in the non-Merging path. Other source files and `docs/invariants.md` merged automatically. No unrelated changes or out-of-scope findings.
- Notes resolution preserves both sides' historical handoffs with one current Workpad/status fence. Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`; incoming INV-1 ownership documentation is retained.

## Historical Workpad

Plan and result: finish and commit the resolved develop merge, preserving the published PR head and fetched target as ancestors, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run. Historical evidence applies only to earlier heads; no current-head validation credit claimed. Gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing and current-head CI waiting. No push, PR merge, issue-state change or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7206"
  completion_generation: "77"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

## Historical handoffs

The records below concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

# Issue #2976 merge fallback handoff (attempt 7198, generation 69)

- PR: https://github.com/digitaldrywood/detent/pull/3052; verified open against `develop` on the assigned isolated branch. Source-clean starting local HEAD and fetched published PR head both match `286ca8e40567e6c34decc77591eccdb982737a17`. No merge or rebase was in progress at session start.
- Verified the prior handoff against Git history: starting HEAD has parents `3b81e1ce7fb21405994b678e498a989ac2d71be0` and prior target `d406805ccd1cf83692852af9daf5fc7bdbf2848e`; the preceding recorded merge parents also match.
- Merged fetched develop `3844f54e75edc8df931d7221c7f62dcb3d71b37e` into the published PR history without rebasing. Only `.detent/notes.md` conflicted; retained both sides' historical handoffs with one current Workpad/status fence. All source files merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`.

## Historical Workpad

Plan and result: finish and commit the resolved develop merge, preserving the published PR head and fetched target as ancestors, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run. Historical evidence applies only to earlier heads; no current-head validation credit claimed. Gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing and current-head CI waiting. No push, PR merge, issue-state change or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7198"
  completion_generation: "69"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

## Historical handoffs

The records below concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

# Issue #2976 merge fallback handoff (attempt 7193, generation 64)

- PR: https://github.com/digitaldrywood/detent/pull/3052; verified open against `develop`, on the assigned isolated branch, with published PR head matching source-clean starting HEAD `3b81e1ce7fb21405994b678e498a989ac2d71be0`. No merge or rebase was in progress at session start.
- Verified prior handoff against Git history: starting HEAD has parents `b38f887d92a7b56ac27cedebbfde442067cd8ffe` and prior target `9d1ef11cf04e04db0babeab159b989abac01a5aa`; the preceding merge parents match the recorded handoff.
- Merged freshly fetched develop `d406805ccd1cf83692852af9daf5fc7bdbf2848e` into the published PR history without rebasing. Only `.detent/notes.md` conflicted; preserved historical notes from both sides with one current Workpad/status block. All source files merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`.

## Historical Workpad

Plan and result: commit the resolved develop merge, preserving the published PR head and fetched target as ancestors, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate, CI checks, or validation waits run. Historical diagnostics apply only to earlier heads; no current-head validation credit claimed. Gate/CI and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7193"
  completion_generation: "64"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

## Historical handoffs

The records below concern earlier heads or other issues. Their validation and completion metadata do not apply to this head. Historical instructions do not authorize validation or publication in this session.

# Issue #2976 merge fallback handoff (attempt 7182, generation 53)

- PR: https://github.com/digitaldrywood/detent/pull/3052; verified open against `develop` on the assigned isolated branch. Fetched published PR head and source-clean starting HEAD both match `b38f887d92a7b56ac27cedebbfde442067cd8ffe`. No rebase or merge was in progress at session start.
- Verified prior handoff against Git history: starting HEAD has parents `df4efb81c975099bd85a734bd144a211f58032f7` and prior target `e109b2f8e8eaa40633f597769859c1ee4fa1f1ce`; preceding merge parents match the recorded handoff.
- Merged freshly fetched develop `9d1ef11cf04e04db0babeab159b989abac01a5aa` into published PR history without rebasing. Only `.detent/notes.md` conflicted; preserved historical handoffs from both sides, including incoming #3060 notes, with one current Workpad/status block. All source files merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`.

## Historical Workpad

Plan: commit the resolved develop merge while preserving the published PR head and prior merges as ancestors, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate, CI checks, or validation waits run. No current-head validation credit claimed; historical diagnostics apply only to earlier heads. Historical gate/publication instructions are superseded by this session's merge-fallback protocol. Gate/CI and post-merge timings are unmeasured.

Open items: Detent independently verifies ownership, cleanliness, and target ancestry, then performs bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7182"
  completion_generation: "53"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

## Historical handoffs

These notes concern earlier heads or other issues; their validation and completion metadata do not apply to this merged head. Historical instructions do not authorize validation or publication in this session.

# Issue #2976 merge fallback handoff (historical, attempt 7177, generation 48)

- PR: https://github.com/digitaldrywood/detent/pull/3052; verified open against `develop` on the assigned isolated branch. Fetched published PR head and source-clean starting HEAD both match `df4efb81c975099bd85a734bd144a211f58032f7`. No rebase or merge was in progress at session start.
- Verified prior handoff against Git history: starting HEAD has parents `ae43267ce9f88ca2a79fb37df612048347f33d68` and prior target `73859c96fa25b6351c9e9ef77e372077c4b2fa44`; the preceding merge parents match the recorded handoff. Earlier published head `139792ad36b0c63b58c6bc58c92363ffb2163d8b` remains an ancestor.
- Merged freshly fetched develop `e109b2f8e8eaa40633f597769859c1ee4fa1f1ce` into the published PR history without rebasing. Only `.detent/notes.md` conflicted; preserved historical handoffs from both sides, including incoming #3271 notes, with one current Workpad/status block. All source files merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`.

## Historical Workpad

Plan: finish and commit the resolved develop merge while preserving the published PR head and prior merges as ancestors, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate, CI checks, or validation waits run. No current-head validation credit claimed; historical diagnostics apply only to earlier heads. Historical gate/publication instructions are superseded by this session's merge-fallback protocol. Gate/CI and post-merge timings are unmeasured.

Open items: Detent independently verifies ownership, cleanliness, and target ancestry, then performs bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7177"
  completion_generation: "48"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

## Historical handoffs

These notes concern earlier heads or other issues; their validation and completion metadata do not apply to this merged head. Historical instructions do not authorize validation or publication in this session.

# Issue #2976 merge fallback handoff (historical, attempt 7170, generation 41)

- PR: https://github.com/digitaldrywood/detent/pull/3052; verified open against `develop` on the assigned isolated branch. Fetched published PR head and source-clean starting HEAD both match `ae43267ce9f88ca2a79fb37df612048347f33d68`. No rebase or merge was in progress at session start.
- Verified prior handoff against Git history: starting HEAD has parents `4ce74ef9e0ba3241e3700c931fd4842396d4d04b` and prior target `4e4334ef3e97a4bf593b81654b3be8d457c8b1e4`; preceding recorded merge parents match. Earlier published head `139792ad36b0c63b58c6bc58c92363ffb2163d8b` remains an ancestor.
- Merged freshly fetched develop `73859c96fa25b6351c9e9ef77e372077c4b2fa44` into the published PR history without rebasing. Only `.detent/notes.md` conflicted; preserved historical handoffs from both sides, including incoming #3433 notes, with one current Workpad/status block. The incoming `internal/workspaceterminal/pty_unix.go` change merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`.

## Historical Workpad

Plan: commit the resolved develop merge while preserving the published PR head and prior merges as ancestors, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate, CI checks, or validation waits run. No current-head validation credit claimed; historical diagnostics apply only to earlier heads. Historical gate/publication instructions are superseded by this session's merge-fallback protocol. Gate/CI and post-merge timings are unmeasured.

Open items: Detent independently verifies ownership, cleanliness, and target ancestry, then performs bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7170"
  completion_generation: "41"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

## Historical handoffs

These notes concern earlier heads or other issues; their validation and completion metadata do not apply to this merged head. Historical instructions do not authorize validation or publication in this session.

# Issue #2976 merge fallback handoff (historical, attempt 7163, generation 34)

- PR: https://github.com/digitaldrywood/detent/pull/3052; verified open against `develop` on the assigned isolated branch. The fetched published PR head and source-clean starting head both match `4ce74ef9e0ba3241e3700c931fd4842396d4d04b`.
- Verified the prior handoff against Git history: the starting head has parents `208ca1d01eafcb57224be6321dc190bbbfc5e7e3` and prior target `8c7b5bfb3685eac99854cefa52d4af02763f7aec`; preceding merge parents match the recorded history. Earlier published head `139792ad36b0c63b58c6bc58c92363ffb2163d8b` remains an ancestor. No rebase or merge was in progress at session start.
- Merged freshly fetched develop `4e4334ef3e97a4bf593b81654b3be8d457c8b1e4` into the published PR history without rebasing. Only `.detent/notes.md` conflicted; preserved both sides' historical handoffs, including incoming #3211 notes, and retained one current Workpad/status block. All source files merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`.

## Historical Workpad (prior #2976 head)

Plan: commit the resolved develop merge while preserving the published PR head as an ancestor, then return immediately with a source-clean workspace.

Validation: no tests, vet, builds, local gate, CI checks, or validation waits run. No current-head validation credit claimed; historical diagnostics apply only to earlier heads. Historical gate/publication instructions are superseded by this session's merge-fallback protocol. Gate/CI and post-merge timings are unmeasured.

Open items: Detent independently verifies ownership, cleanliness, and target ancestry, then performs bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7163"
  completion_generation: "34"
blockers: []
human_action: null
```

## Historical handoffs

These notes concern earlier heads or other issues; their validation and completion metadata do not apply to this merged head.

# Issue #2976 merge fallback handoff (historical, attempt 7154)

- PR: https://github.com/digitaldrywood/detent/pull/3052; verified open against `develop` on the expected isolated branch. Fetched published PR head remains `139792ad36b0c63b58c6bc58c92363ffb2163d8b` and is preserved as an ancestor.
- Verified prior notes against source-clean starting head `208ca1d01eafcb57224be6321dc190bbbfc5e7e3`, whose parents are prior local merge `cf5a63430178a9e66db8c9381089b8b15d59f839` and prior target `e687a22a5324187fb5a24de3aa5668e8e61f28f5`. Both unpublished merges are preserved.
- Merged freshly fetched develop `8c7b5bfb3685eac99854cefa52d4af02763f7aec` without rebasing. Only `.detent/notes.md` conflicted; preserved both handoff histories, including incoming #3019 notes, and consolidated one current Workpad/status block. All source files merged automatically; no manual source edits were required.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`.
- Validation: no tests, builds, local gate, CI checks, or waits run. Historical diagnostics do not validate this merged head.
- Open items: Detent independently verifies ownership, cleanliness, and target ancestry, then performs bounded validation, lease-protected publishing, and current-head CI waiting. No out-of-scope finding; no push, PR merge, issue-state change, or tracker lane write performed.

## Historical Workpad (prior #2976 head)

Plan: preserve published PR #3052 history and prior unpublished merges; merge freshly fetched develop, resolve the notes conflict, and commit the resolution for Detent handoff.

Validation: deferred to Detent under the merge-fallback protocol. No current-head test or CI credit claimed. Historical instructions below to run a gate or publish are superseded for this session; no gate/CI or post-merge timings measured.

Open items: Detent verification, validation, publication, and current-head CI processing after return.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7154"
  completion_generation: "25"
blockers: []
human_action: null
```

## Historical handoffs

# Issue #2976 merge fallback handoff (historical, attempt 7146)

- PR: https://github.com/digitaldrywood/detent/pull/3052; verified open against `develop` on the expected isolated branch. Fetched published PR head remains `139792ad36b0c63b58c6bc58c92363ffb2163d8b`.
- Verified prior notes against source-clean starting head `cf5a63430178a9e66db8c9381089b8b15d59f839`: its parents are the published PR head and prior target `9826088a821ba72ecfd033bc60b9ed3138762507`. That unpublished merge is preserved.
- Merged freshly fetched develop `e687a22a5324187fb5a24de3aa5668e8e61f28f5` into the existing local merge without rebasing. All files merged automatically; no conflicts or manual source edits were required. Retained incoming historical #3001 notes.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`.
- Validation: no tests, builds, local gate, CI checks, or waits run. Historical diagnostics do not validate this merged head.
- Open items: Detent independently verifies ownership, cleanliness, and target ancestry, then performs bounded validation, lease-protected publishing, and current-head CI waiting. No new out-of-scope finding; no push, PR merge, issue-state change, or tracker lane write performed.

## Historical workpad for #2976 (attempt 7146)

Plan: preserve the prior unpublished merge and published PR #3052 history; merge freshly fetched develop, refresh handoff notes, and commit the automatic resolution for Detent handoff.

Validation: deferred to Detent under the merge-fallback protocol. No current-head test or CI credit claimed. Historical instructions below to run a gate or publish are superseded for this session; no gate/CI or post-merge timings measured.

Open items: Detent verification, validation, publication, and current-head CI processing after return.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7146"
  completion_generation: "17"
blockers: []
human_action: null
```

## Historical workpad for #3019 (attempt 7141)

- Plan: merge fetched `origin/develop` at `e687a22a5324187fb5a24de3aa5668e8e61f28f5` into PR #3026's published head `bb60dc4e827b413fffa8562ec135b402761e83ff`, preserving both as ancestors without rebasing.
- Prior notes verified against the clean starting head and fetched PR ref. The historical #3019 notes name an earlier rebased head; the published head for this fallback is `bb60dc4e827b413fffa8562ec135b402761e83ff`. Prior diagnostics are historical only.
- Resolution: only `.detent/notes.md` conflicted. Preserved historical handoffs from both sides, including #3019 and #3001, and consolidated this current Workpad/status block. All source files merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/cli/boot.go`, `internal/cli/dev_runtime_e2e_test.go`; both remain unchanged from the published PR head.
- Validation: no tests, builds, local gate, CI checks, or waits run in this merge-fallback session. No current-head validation credit claimed; gate/CI and post-merge timings are unmeasured.
- Open items: Detent owns resolved-head ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7141"
  completion_generation: "12"
blockers: []
human_action: null
```

# Issue #2976 previous merge fallback (historical)

- PR: https://github.com/digitaldrywood/detent/pull/3052; verified open against `develop` on the expected branch. Local starting head and fetched published PR head both match `139792ad36b0c63b58c6bc58c92363ffb2163d8b`. Prior publication and diagnostic notes below are historical.
- Merged fetched `origin/develop` at `9826088a821ba72ecfd033bc60b9ed3138762507` into the published PR history without rebasing. Both heads are retained as merge parents.
- Only conflict: `.detent/notes.md`. Preserved both historical handoffs and consolidated the current Workpad below. All source files merged automatically; no manual source changes were required.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`.
- Validation: no tests, builds, local gate, CI checks, or waits run in this fallback. Prior diagnostics do not validate this merged head.
- Open items: Detent independently verifies ownership, cleanliness, and target ancestry, then performs bounded validation, lease-protected publishing, and current-head CI waiting. No new out-of-scope finding; no push, PR merge, issue-state change, or tracker lane write performed.

# Issue #2976 prior handoff (historical)

- PR: https://github.com/digitaldrywood/detent/pull/3052; ready, targets `develop`, includes `Fixes #2976`.
- Recovered both issue commits and rebased without conflicts onto `origin/develop` at `d14138a37c7597542c30e19616271a971b62fc8f`. All recovered source changes belong to this issue; no stray files remain.
- Key files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`.
- Ready In Progress PRs reuse the existing Rework transition for unresolved review threads or failing CI. Drafts and unfinished clean PRs remain unaffected. Configured source/pass/rework lanes retain normal behavior, and final running attempts can take the repair route. No new mechanism or reason is introduced.
- Both existing automated review threads are resolved. No newer actionable feedback was present before publication.
- Last diagnostic attempt: `go test ./internal/orchestrator/... -run '^(TestAutoPromote|TestTickAutoPromote|TestApplyAutoPromote)' -count=1` exited 1 before test execution, and `go vet ./internal/orchestrator/...` exited 1 during compilation. Both report `internal/runner/ssh_protocol.go:210:85: undefined: ErrSessionNoProgress`, also present on develop. Reused fingerprint `runner-ssh-sentinels-retired-session-no-progress` to attach this occurrence to existing #3427. No unrelated source fix was included. This continuation changes notes only; the tested source remains unchanged.
- Earlier full focused-package test/vet results apply to the previous rebased head only. No test pass is claimed on the current head.
- Current protocol sets `gate.run` to `true`; blocking local gates and local status publication are disabled. Run it once immediately before pushing; it provides no test evidence. Prior `make check-fast`/`local-gate` instructions are superseded.
- Publication verified on continuation: local HEAD, tracked upstream, and ready PR #3052 all matched `f193a2273300c61df9babb8cc08d79016197a309`. The PR validation description already records the exact diagnostic failures and configured no-op gate. Both review threads remain resolved; there are no newer actionable reviews or comments, and the head has no check/status contexts. Refresh these notes and the canonical Workpad for attempt 7118, generation 48. The orchestrator owns lane state and later merge processing.
- Skill draft: no — no new reusable procedure was needed.

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

## Historical workpad for #3400

Plan: Rework repair for #3400 preserves recovered merge 83598d050 and merges develop a53a7f204a278ae2f965560d6c4c10f96eb5b22d into PR #3421's published history. Only notes conflicted; retained historical handoffs from both sides and one current status block. No manual source edits or generated-input changes were required. The remaining issue source diff is internal/cli/boot_test.go and internal/cli/ssh_runner_test.go.

Recovery: inspected all 24 paths in 83598d050; inherited develop changes and its issue-owned notes resolution are retained for publication. No stray artifacts were present. Publication resolves the recovered unpushed commit; no stash or tracker lane writes.

Validation: focused CLI/orchestrator command passed all seven selected regression names at the resolved source (CLI 9.977s; orchestrator 0.612s). Windows/amd64 CLI test cross-compilation passed (10.1s), with output under the provided TMPDIR. Compilation is not native Windows execution. No source changes followed these diagnostics. No full suite, coverage, race suite, Actions rerun, or blocking CI wait ran. Configured `true` must run on the final committed head immediately before publication; exact gate and current-head eligibility evidence are recorded in the canonical issue Workpad.

Open items: next scheduled native Windows validation confirms the portability repair; squash merge and lane transitions remain orchestrator-owned. PR #3421 is already non-draft against develop and references Fixes #3400. No actionable reviews or threads existed at the pre-publication read; current-head checks were absent under repository policy and confer no test credit. No merge-group CI is configured. No new out-of-scope finding or dependency.

Timings: focused diagnostic command 20.5s; CLI Windows cross-compilation 10.1s. No quiet window is configured. The no-op gate takes under 1s; PR CI wait, slow checks, and post-merge main-CI are not applicable to this Rework handoff. Final publication verification belongs in the issue Workpad.

Historical status record for #3400:

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7129"
  completion_generation: "59"
  completion_cleanliness_resolution: committed
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

# Issue #3427 handoff

- Removed the retired `ErrSessionNoProgress` entry from `internal/runner/ssh_protocol.go`; remaining SSH sentinel and structured-error encoding is unchanged. No invariant or enforcement change.
- Reproduced the recorded command on baseline `c4455b9277dbcb7a9622a8d5a369bb1a7283cc5c`: `go test ./internal/orchestrator -run '^TestOperationalBodyCompletionSurvivesRestart$' -count=1 -p 4` exited 1 with the reported undefined symbol.
- After the removal, the same command exits 0 and compiles the orchestrator, but reports `[no tests to run]`: that test belongs to separate work. This is compilation evidence only.
- Passed existing runner protocol diagnostics: `go test ./internal/runner -run '^(TestSSHErrorRoundTrip|TestSSHPeerConcurrentCallbacksAndDisconnect|TestSSHCallbackDoesNotPublishRemotePID|TestSSHRunResponseRetainsResultOnFailure)$' -count=1 -p 4` (0.406s package time). Existing tests cover sentinel identity, wrappers, structured errors, callbacks, disconnects, and failure results; no duplicate test added.
- No generated inputs changed. Configured gate is `true`; no full gates, coverage, race suite, or CI wait. No out-of-scope discovery or reusable skill draft.
- Source repair and focused diagnostics are complete. PR publication, current-head review, and completion for attempt 7112 / generation 42 are tracked in the canonical issue Workpad.

# Issue #3019 (historical implementation handoff)

- Key files: `internal/cli/boot.go`, `internal/cli/dev_runtime_e2e_test.go`.
- Existing fix publishes the dashboard address immediately after binding, before runtime store and board snapshot initialization. The blocked snapshot-load regression covers fixture and screenshots modes and checks `/health` after releasing the load.
- Rework was an old CI failure, with no actionable human or bot review. #2975 and #3009 are closed; their fixes are merged into `origin/main`. Current `develop` has independent platform fixes; prior full-CI failures are historical under the operator's disabled blocking-gate policy.
- Rebased the single issue commit cleanly onto `origin/develop` at `bccee4c50f86019fd961a4c457a59a2c91e9eb31`. Rebased head: `1cae634d21b925d2ef4570a6061d0888414342ae`.
- Current-head diagnostics: affected startup tests passed three repetitions (27.313s); four concurrent GOMAXPROCS=1 runs passed (10.056–11.679s); `go test ./internal/cli/... -count=1 -timeout=15m` passed (54.533s); `go vet ./internal/cli/...` passed. DETENT_API_TOKEN was cleared for tests.
- Prior deterministic regression failed on the old boot order after the unchanged 10-second empty-banner wait. Prior ambient host stress did not reproduce the observed host slowdown; no blanket timeout increase was made.
- Configured `true` gate passed exactly once immediately before pushing. Rebased head is pushed, ready PR #3026 targets `develop`, and the issue Workpad reports `complete` with no blockers. GitHub recheck confirmed exact head, no review threads/findings, and no current-head check rollup; no skipped checks are credited as tests.
- Open items: none for implementation/Rework. The orchestrator owns promotion and merging; do not merge during Rework or change tracker lane labels.
- Skill draft: no; the deterministic startup-order regression needs no new reusable procedure.

# Issue #3001

PR: https://github.com/digitaldrywood/detent/pull/3004 (ready, target develop).
Validated code head: d36c44c837dfbea11c1ee3d229c791638a4b1a67; rebased onto develop bccee4c50f86019fd961a4c457a59a2c91e9eb31 without conflict edits.

Key files: internal/cli/runner.go classifies identified unusable configured paths with project.ErrProjectDefinition; internal/workspace/workspace.go preserves canonicalization PathError identity; internal/project/manager.go retries terminal pending definitions on explicit reconciliation. Regression tests live in internal/cli/runner_test.go and startup_workflow_test.go; INV-3 is updated in docs/invariants.md.

Human review finding is fixed and resolved. ENOSPC, EIO, permission, backend and state-store failures remain fatal. No mechanism or configuration key added.

Dependencies #2975 (PR #2986) and #3009 (PR #3018) are closed/merged to main; their main merge SHAs are not ancestors of develop. Historical full PR CI is no longer a blocker under current operator policy. Focused doctor and workspace portability cases pass on macOS; no Windows test credit claimed.

Diagnostics: focused startup, classification, manager, doctor and workspace tests passed; go vet ./internal/cli/... ./internal/project/... ./internal/workspace/... passed. First rebased attempt could not compile because of retired SSH sentinel ErrSessionNoProgress; already fixed by merged #3431, incorporated in second clean rebase. Configured gate true is next immediately before push. No full checks or CI polling performed.

Handoff: publish the rebased ready PR with an exact lease and a complete canonical issue Workpad; the configured true gate runs immediately before that push. Orchestrator owns lane transitions and merge dispatch. Skill draft: no — routine rebase and diagnostics added no reusable procedure.

## Issue #3076 merge fallback — 2026-09-30

- Verified remote PR #3296 head `717af073f247280b76bd5e645c8f9d5bd353af38` matches the retained local head; historical publication notes refer to an older head.
- Merged current `origin/develop` without rebasing; retained both independent additions in `docs/invariants.md` and both regressions in `internal/runner/prompt_test.go`. `internal/runner/prompt.go` merged automatically.
- No local validation or CI run in this fallback session. Prior test results are historical, not evidence for the resolved head.
- Open items: Detent verifies the clean head and ancestry, validates, publishes with lease protection, and waits for current-head CI. No unrelated work identified.

# Additional historical target handoffs

# Historical #3211 merge fallback handoff, 2026-09-30

- Verified the source-clean starting published PR head `d2a12be6c4b0ca9dfdf4a6745547dbd744a2edcd` has parents `319375baf3832cabbf176c0909c66cce01f22b7b` and prior target `e687a22a5324187fb5a24de3aa5668e8e61f28f5`. Verified the preceding handoff's starting-head parent record against Git history.
- PR #3281 remains open against develop on the assigned isolated branch. Fetched the PR branch at `d2a12be6c4b0ca9dfdf4a6745547dbd744a2edcd` and target develop at `8c7b5bfb3685eac99854cefa52d4af02763f7aec`. Merged that pinned target into the published PR history without rebasing, preserving the remote PR head as an ancestor.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs, including incoming #3019 notes, and retained one current Workpad/status block. Incoming changes to `docs/invariants.md`, `internal/cli/boot.go`, `internal/cli/dev_runtime_e2e_test.go`, `internal/orchestrator/completion_transition_test.go`, `internal/runner/prompt.go`, and `internal/runner/prompt_test.go` merged automatically. No manual source changes or out-of-scope findings.

## Historical Workpad (prior #3211 head)

Plan: commit the resolved target merge, then return immediately to Detent with a source-clean workspace.

Validation: no tests, vet, builds, local gate, CI checks, or validation waits performed. No validation success is claimed for this head. Historical validation below applies only to earlier heads. Gate/CI and other validation timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state changes, or tracker lane writes performed.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7152"
  completion_generation: "23"
blockers: []
human_action: null
```

# Historical handoffs

These notes concern earlier heads or other issues; their validation and completion metadata do not apply to this head. Historical instructions do not authorize validation or publication in this merge-fallback session.

# Historical #3211 merge fallback handoff, 2026-09-30

- Verified the source-clean starting local head `319375baf3832cabbf176c0909c66cce01f22b7b` has parents `0d5e8028643d0d0de7b3ecf4e07aff0bcde629a0` and prior target `9826088a821ba72ecfd033bc60b9ed3138762507`. The preceding handoff's local merge and published PR parent records match Git history.
- PR #3281 remains open against develop on the assigned isolated branch. Fetched the PR branch at `d56be7735f63444bc99fa5a25ebbf510ef3c56b4` and target develop at `e687a22a5324187fb5a24de3aa5668e8e61f28f5`. Merged that pinned target into the existing local history without rebasing; the published PR head remains an ancestor.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs, consolidated the duplicate #3427 section, and retained one current Workpad/status block. Incoming changes to `docs/invariants.md`, `internal/cli/runner.go`, `internal/cli/runner_test.go`, `internal/cli/startup_workflow_test.go`, `internal/project/manager.go`, and `internal/workspace/workspace.go` merged automatically. No manual source changes or out-of-scope findings.

## Historical Workpad (prior #3211 head)

Plan: finish and commit the resolved target merge, then return immediately to Detent with a source-clean workspace.

Validation: no tests, vet, builds, local gate, CI checks, or validation waits performed. No validation success is claimed for this head. Historical validation below applies only to earlier heads. Gate/CI and other validation timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state changes, or tracker lane writes performed.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7144"
  completion_generation: "15"
blockers: []
human_action: null

# Historical #3211 merge fallback handoff, 2026-09-30

- Verified the source-clean starting local merge `0d5e8028643d0d0de7b3ecf4e07aff0bcde629a0` has parents published PR head `d56be7735f63444bc99fa5a25ebbf510ef3c56b4` and prior target `4686438c63f97741e8306726836a1d703961c664`, confirming the preceding handoff. The published head's recorded parents also match Git history.
- PR #3281 remains open against develop on the assigned isolated branch. Fetched the PR branch and target `origin/develop` at `9826088a821ba72ecfd033bc60b9ed3138762507`. Merged that target into the existing local PR history without rebasing; the published PR head remains an ancestor.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs and one current Workpad/status block. Incoming changes to `internal/cli/boot_test.go`, `internal/cli/ssh_runner_test.go`, `internal/connector/github/statuslabel.go`, and `internal/connector/github/statuslabel_test.go` merged automatically; no manual source changes or out-of-scope findings.

## Historical Workpad (prior #3211 local head)

Plan: commit the resolved target merge and return immediately to Detent with a source-clean workspace.

Validation: no tests, vet, builds, local gate, or CI checks run or awaited in this session. No validation success is claimed for this head. Historical validation below applies only to earlier heads. Gate/CI and other validation timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state changes, or tracker lane writes performed.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7135"
  completion_generation: "6"
blockers: []
human_action: null

# Historical target and PR handoffs

The following notes concern earlier heads or other issues; their validation and completion metadata do not apply to the current #3211 head. Historical instructions do not authorize validation or publication in this merge-fallback session.

# Historical #3211 merge fallback handoff, 2026-09-30

- Verified the clean starting published PR #3281 head `d56be7735f63444bc99fa5a25ebbf510ef3c56b4` has parents `056f1273a0b402f9a2778b8024c06b91845799f0` and prior target `8a94591757c917387c3530454e30d25962ed0448`, confirming the previous handoff. Earlier recorded merge parents were also verified against Git history. PR #3281 remains open against develop and includes `Fixes #3211`.
- Fetched the PR branch and target `origin/develop` at `4686438c63f97741e8306726836a1d703961c664`. Merged the target into the assigned PR branch without rebasing; the published PR head is preserved as the first parent.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs below and consolidated the active workpad here. All source files merged automatically; no manual source changes or out-of-scope findings.

## Historical Workpad (prior #3211 head)

Plan: finish and commit the target merge, then return immediately to Detent with a source-clean workspace.

Validation: no tests, vet, builds, local gate, or CI checks run or awaited in this session. No validation success is claimed for this head. Earlier validation claims below apply only to historical heads. Gate/CI and other validation timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state changes, or tracker lane writes performed.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7127"
  completion_generation: "57"
blockers: []
human_action: null

# Historical target and PR notes

The following handoffs concern earlier heads or other issues; their validation claims and completion metadata do not apply to the current #3211 head.

# Historical #3211 merge fallback handoff, 2026-09-30

- Verified the clean starting published PR #3281 head `056f1273a0b402f9a2778b8024c06b91845799f0` has parents `74d20e4a358b95a6a05bd055aae8238140cdb4c1` and prior target `c4455b9277dbcb7a9622a8d5a369bb1a7283cc5c`, confirming the previous handoff. Earlier merge-parent notes were also verified against Git history.
- Fetched the PR branch and target `origin/develop` at `8a94591757c917387c3530454e30d25962ed0448`. Merged the target into the assigned PR branch without rebasing; the published PR head is preserved as the first parent.
- Resolved only two conflicts: `internal/policy/policy.go` retains both the PR's `HumanReview` field and develop's `GitHubPullRequest` field with their existing JSON tags; `.detent/notes.md` preserves historical handoffs below with one active workpad. All remaining files merged automatically.

# Historical #3211 merge fallback handoff, 2026-09-30

- Verified published PR #3281 head `74d20e4a358b95a6a05bd055aae8238140cdb4c1` has parents `2cbb7131b37b6e5b8d1554b36615aa11879d4816` and previous target `d14138a37c7597542c30e19616271a971b62fc8f`. Verified the prior published head's parents `27951a8e9f2950be91cd2a5830d3e161f93deda9` and `d80e68213d2768b5ebf7e95313bf35f40042411f`, and the earlier merge's parents `d97be0deadf64576efb6da220c16b47be8b9ba50` and `73d1c330b04253600438c12e0f774d3fe16b26d8`, confirming prior notes.
- Started source-clean without an in-progress merge or rebase. Fetched `origin/develop` at `c4455b9277dbcb7a9622a8d5a369bb1a7283cc5c` and merged it into the assigned PR branch without rebasing; the published PR head remains the first parent.
- Only `.detent/notes.md` conflicted. Resolved it by keeping the current #3211 handoff and preserving other issues' historical notes below. Source files merged automatically; no manual source changes were needed.
- Prior resolution key files: `docs/invariants.md`, `internal/runner/prompt.go`, `internal/hubserver/migrations/00048_hosted_blocked_lane.sql`, `internal/hubserver/migrate.go`, and `internal/hubserver/hosted_review_lane_migration_test.go`. Prior schema 48 and 47→48 migration notes and validation claims apply to earlier heads only.

# Additional historical target handoffs (#3433)

# Issue #3433 merge fallback handoff

- Verified PR #3434 is open, targets `develop`, includes `Fixes #3433`, and has published head `1ffd4361802762ac780bcdfe725eac84b63ef98c`, matching the clean starting local branch. No rebase or merge was in progress. The prior #3433 handoff matches that commit's notes and scoped PTY annotation change; its validation is historical only.
- Merged freshly fetched develop commit `4e4334ef3e97a4bf593b81654b3be8d457c8b1e4` into the published head without rebasing. The published PR head is retained as the first parent.
- Only conflict: `.detent/notes.md`. Preserved both sides' historical handoffs and consolidated one current Workpad/status block. All source files merged automatically; no manual source edits or generated-input changes were required.
- Key issue file: `internal/workspaceterminal/pty_unix.go`; the four best-effort cleanup annotations remain unchanged from the published PR head.

## Historical Workpad (#3433 merge fallback)

Plan: finish and commit the resolved target merge, then return immediately to Detent with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate, CI checks, or waits run in this merge-fallback session. Prior lint and package-test results below do not validate this resolved head. Gate/CI, quiet-window, slow-check, and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No out-of-scope findings. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7161"
  completion_generation: "32"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Issue #3433 handoff

- Reproduced the recorded four `errcheck` findings on clean develop `9826088a821ba72ecfd033bc60b9ed3138762507`, using the repository-pinned golangci-lint v2.9.0 built with Go 1.26.6 against `internal/workspaceterminal/...` (exit 1, 2.2s).
- `internal/workspaceterminal/pty_unix.go` adds four line-scoped `nolint:errcheck` annotations to documented best-effort descriptor cleanup. PTY startup, ownership, returned errors, and cleanup operations are unchanged; lint configuration and invariants are unchanged.
- The identical focused lint command passes with zero issues (0.9s). Existing package tests pass: `env -u DETENT_API_TOKEN GOMAXPROCS=4 GOTOOLCHAIN=go1.26.6 go test -p 4 ./internal/workspaceterminal/... -count=1` (5.6s command, 5.289s package). `git diff --check` passes. No new test: annotations change no runtime behavior; the pinned lint invocation reproduces the recorded failure.
- No generated inputs, UI surfaces, mechanisms, or out-of-scope findings. No full check-fast, coverage, race suite, or Actions wait. The configured gate is `true` and publishes no local-gate status. Publication, exact-head gate/check/review evidence, and completion for attempt 7134 / generation 5 belong to the canonical issue Workpad. Squash merge and lane transitions remain orchestrator-owned.
- Skill draft: no — this is a routine scoped lint annotation fix.

## Historical Workpad (#3019 merge fallback)

- Plan: merge fetched `origin/develop` at `e687a22a5324187fb5a24de3aa5668e8e61f28f5` into PR #3026's published head `bb60dc4e827b413fffa8562ec135b402761e83ff`, preserving both as ancestors without rebasing.
- Prior notes verified against the clean starting head and fetched PR ref. The historical #3019 notes name an earlier rebased head; the published head for this fallback is `bb60dc4e827b413fffa8562ec135b402761e83ff`. Prior diagnostics are historical only.
- Resolution: only `.detent/notes.md` conflicted. Preserved historical handoffs from both sides, including #3019 and #3001, and consolidated this current Workpad/status block. All source files merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/cli/boot.go`, `internal/cli/dev_runtime_e2e_test.go`; both remain unchanged from the published PR head.
- Validation: no tests, builds, local gate, CI checks, or waits run in this merge-fallback session. No current-head validation credit claimed; gate/CI and post-merge timings are unmeasured.
- Open items: Detent owns resolved-head ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7141"
  completion_generation: "12"
blockers: []
human_action: null

# Issue #3019 (historical implementation handoff)

- Key files: `internal/cli/boot.go`, `internal/cli/dev_runtime_e2e_test.go`.
- Existing fix publishes the dashboard address immediately after binding, before runtime store and board snapshot initialization. The blocked snapshot-load regression covers fixture and screenshots modes and checks `/health` after releasing the load.
- Rework was an old CI failure, with no actionable human or bot review. #2975 and #3009 are closed; their fixes are merged into `origin/main`. Current `develop` has independent platform fixes; prior full-CI failures are historical under the operator's disabled blocking-gate policy.
- Rebased the single issue commit cleanly onto `origin/develop` at `bccee4c50f86019fd961a4c457a59a2c91e9eb31`. Rebased head: `1cae634d21b925d2ef4570a6061d0888414342ae`.
- Current-head diagnostics: affected startup tests passed three repetitions (27.313s); four concurrent GOMAXPROCS=1 runs passed (10.056–11.679s); `go test ./internal/cli/... -count=1 -timeout=15m` passed (54.533s); `go vet ./internal/cli/...` passed. DETENT_API_TOKEN was cleared for tests.
- Prior deterministic regression failed on the old boot order after the unchanged 10-second empty-banner wait. Prior ambient host stress did not reproduce the observed host slowdown; no blanket timeout increase was made.
- Configured `true` gate passed exactly once immediately before pushing. Rebased head is pushed, ready PR #3026 targets `develop`, and the issue Workpad reports `complete` with no blockers. GitHub recheck confirmed exact head, no review threads/findings, and no current-head check rollup; no skipped checks are credited as tests.
- Open items: none for implementation/Rework. The orchestrator owns promotion and merging; do not merge during Rework or change tracker lane labels.
- Skill draft: no; the deterministic startup-order regression needs no new reusable procedure.

## Issue #3076 merge fallback — 2026-09-30

- Verified remote PR #3296 head `717af073f247280b76bd5e645c8f9d5bd353af38` matches the retained local head; historical publication notes refer to an older head.
- Merged current `origin/develop` without rebasing; retained both independent additions in `docs/invariants.md` and both regressions in `internal/runner/prompt_test.go`. `internal/runner/prompt.go` merged automatically.
- No local validation or CI run in this fallback session. Prior test results are historical, not evidence for the resolved head.
- Open items: Detent verifies the clean head and ancestry, validates, publishes with lease protection, and waits for current-head CI. No unrelated work identified.

## Historical Workpad

Merge fallback for #3271 / PR #3355, 2026-09-30.

- Plan: merge fetched `origin/develop` (`8c7b5bfb3685eac99854cefa52d4af02763f7aec`) into the published PR branch, retaining remote PR head `f5b842502b7aeffe08d80c37cada0e0419514ac1` as an ancestor. No rebase or publication in this session.
- Key files: `web/conversation/src/app/account/Setup.tsx` retains develop's runner-checkout association behavior and the PR's contextual help. Updated adjacent help and `tests/visual/onboarding-help.spec.js` references for the renamed control. Regenerated conflicted `static/app/conversation/app.js` with `npm run build`; no other generated files changed.
- Prior notes: no #3271 handoff was present; notes imported from develop concern other issues and remain historical. PR body already includes `Fixes #3271`.
- Validation: bundle generation completed successfully; no tests, local gate (including `true`), CI checks, or CI waiting ran. Prior PR test evidence is historical and does not validate this resolved head.
- Open items: Detent owns clean-head/ancestry verification, bounded validation, lease-protected push, and current-head CI waiting. No unrelated work identified. The PR remains open and tracker state is unchanged.

```yaml
schema: 1
status: complete
blockers: []
human_action: null
```

# Additional historical target handoffs (#3060)

## Historical Workpad

Plan: merged fetched develop `73859c96fa25b6351c9e9ef77e372077c4b2fa44` into source-clean published PR #3061 head `78cb53ad273c059c5bf97afe6d962110713b192d`, preserving published history without rebasing. Verified the starting head's parents against the preceding #3060 handoff.

# Issue #3417 merge fallback handoff, 2026-09-30

- Verified the source-clean starting local merge `cd0b59d585e6de1fcf7cd29d8599d08bbaadb705` has parents published PR head `c6eb99858d9bc6d99441cc5c297323e10c2fa99c` and prior target `73859c96fa25b6351c9e9ef77e372077c4b2fa44`, confirming the preceding handoff. The issue fixture matches preceding published head `44d367cc86a9d96650ded69e3cc78da189bf4e30`; prior validation remains historical.
- PR #3435 is open against develop on the assigned branch. Fetched published PR head `c6eb99858d9bc6d99441cc5c297323e10c2fa99c` and target `9d1ef11cf04e04db0babeab159b989abac01a5aa`. Merged the pinned target into the existing local PR history without rebasing, preserving the published head as an ancestor.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs and consolidated one current Workpad/status fence. All source files merged automatically; no manual source changes or out-of-scope findings.

## Historical Workpad

Plan and result: commit the resolved target merge and return immediately to Detent with a source-clean workspace. Key issue file: `internal/orchestrator/orchestrator_test.go`; the fixture repair is retained without manual edits.

Validation: no tests, lint, vet, builds, local gate, CI checks, or validation waits performed in this session. No validation success is claimed for this head; historical diagnostics below apply only to earlier heads. Gate/CI and other validation timings are unmeasured.

Open items: Detent owns independent branch ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, tracker lane write, or live-instance mutation performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7181"
  completion_generation: "52"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs

The following records concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

# Issue #3417 merge fallback handoff, 2026-09-30

- Verified source-clean local and fetched published PR #3435 head `c6eb99858d9bc6d99441cc5c297323e10c2fa99c`, with parent `2d4bf665ad72c88cd4c7183b0187d68516073a92`. No merge or rebase was in progress; the pre-check rebase was already absent. PR #3435 is open against `develop` on the assigned isolated branch.
- Prior #3417 Rework notes were verified against history: the published commits descend from target `4e4334ef3e97a4bf593b81654b3be8d457c8b1e4`, and the repaired `internal/orchestrator/orchestrator_test.go` fixture matches the preceding published head `44d367cc86a9d96650ded69e3cc78da189bf4e30`. Verified the supplied #3211 starting-head parent record against Git history. All prior validation is historical only.
- Merged fetched target `73859c96fa25b6351c9e9ef77e372077c4b2fa44` into the published PR history without rebasing, preserving the published PR head as the first parent.
- Only `.detent/notes.md` conflicted: retained both sides' historical handoffs and consolidated one current Workpad/status fence. Incoming `internal/workspaceterminal/pty_unix.go` changes merged automatically; no manual source changes or out-of-scope findings.

## Historical Workpad (#3417 prior merge)

Plan and result: resolve and commit the target merge, then return immediately to Detent with a source-clean workspace. Key issue file: `internal/orchestrator/orchestrator_test.go`; the fixture repair is retained without manual edits.

Validation: no tests, lint, vet, builds, local gate, CI checks, or validation waits performed in this merge-fallback session. No validation success is claimed for this head; historical diagnostics below apply only to earlier heads. Gate/CI, quiet-window, slow-check, and post-merge timings are unmeasured.

Open items: Detent owns independent branch ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, tracker lane write, or live-instance mutation performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7173"
  completion_generation: "44"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs

These notes concern earlier heads or other issues. Their validation and completion metadata do not apply to this #3417 merge. Historical instructions do not authorize validation or publication in this session.

# Issue #3257 current merge fallback

- PR #3362 is open against `develop` on the assigned isolated branch. Starting source-clean local and fetched published PR head: `5e09128949f00054d84c99db6c62fc42520c41fa`.
- Verified the preceding handoff against the starting merge's parents: `132fc057840d8b963fa39d20c29285dce6fb66b1` and prior target `9d1ef11cf04e04db0babeab159b989abac01a5aa`. The recorded parents of `132fc057840d8b963fa39d20c29285dce6fb66b1` also match Git history.
- Merged freshly fetched target `d406805ccd1cf83692852af9daf5fc7bdbf2848e` into the published PR history without rebasing. The remote PR head is preserved as the first parent.
- Only `.detent/notes.md` conflicted. Preserved historical handoffs from both sides, including incoming #3417 notes, with one current Workpad/status fence. All source files merged automatically; no manual source edits, asset generation, or out-of-scope findings.
- Key prior issue files remain `internal/cli/boot.go`, `internal/cli/hub_client.go`, `internal/hubclient/scheduler.go`, `internal/hubserver/migrations/00049_linked_issue_sources.sql`, and the linked-issue conversation sources and bundle. This retry only resolves handoff notes.

## Historical Workpad

- Plan and result: merge fetched develop into the retained local history for issue #3436 / PR #3438, commit the notes resolution, and return immediately to Detent.
- Verified the clean starting local merge `c32c89c81844637fa638afb4093f46067bcb70a3` has parents `19aff34b43970dba18029928a233f966d66b1d43` and prior target `d406805ccd1cf83692852af9daf5fc7bdbf2848e`, confirming the preceding handoff. The prior local merge's parents and published head's parent also match the recorded Git history.
- PR #3438 is open against `develop` on the assigned isolated branch. Fetched published PR head `5d9ea37404468adfa7d78f697d67893d9cd48b3b` remains an ancestor of the retained local history. Merged pinned fetched target `3844f54e75edc8df931d7221c7f62dcb3d71b37e` without rebasing published commits.
- Resolution: only `.detent/notes.md` conflicted. Preserved historical records from both sides with one current Workpad/status fence. All source files and generated assets merged automatically; no manual source changes, generation, or out-of-scope findings.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/orchestrator.go`, existing audit/snapshot tests, `docs/invariants.md`, and `docs/diagnostics/promotion-3436.md`.
- Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits performed. Historical diagnostics do not validate this head; validation timings are unmeasured.
- Open items: Detent owns independent ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7201"
  completion_generation: "72"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical target and PR records

The following records concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

## Historical Workpad

- Plan and result: finish the target merge for issue #3436 / PR #3438, commit the notes resolution, and return immediately to Detent.
- Verified clean retained local merge `19aff34b43970dba18029928a233f966d66b1d43` has parents published PR head `5d9ea37404468adfa7d78f697d67893d9cd48b3b` and prior target `dc5cfec36143ec025709263bce3f76a03253a125`, confirming the preceding notes. The published head's parent is `51f3aca78915f715f5aa382050158e4e91a46f44`. PR #3438 remains open against `develop` on the assigned branch; its fetched published head remains unchanged and is an ancestor of the retained local history.
- Merged freshly fetched target `d406805ccd1cf83692852af9daf5fc7bdbf2848e` into the retained unpublished local merge without rebasing published commits.
- Resolution: only `.detent/notes.md` conflicted. Preserved distinct historical sections from both sides and one current Workpad/status fence. Incoming `internal/orchestrator/orchestrator_test.go` fixture repair merged automatically; no manual source edits or generated-input changes were required.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/orchestrator.go`, existing audit/snapshot tests, `docs/invariants.md`, and `docs/diagnostics/promotion-3436.md`.
- Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run. Prior diagnostics are historical and do not validate this head; validation timings are unmeasured.
- Open items: Detent owns independent ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No out-of-scope findings. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7196"
  completion_generation: "67"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical target and PR handoffs

These records concern earlier heads or other issues. Their validation and completion metadata do not apply to this resolved head or authorize validation or publication in this fallback session.

## Historical Workpad

- Plan: resolve the target merge for issue #3436 / PR #3438 and commit it, then return immediately to Detent.
- Verified PR #3438 is open, targets `develop`, includes `Fixes #3436`, and has published head `5d9ea37404468adfa7d78f697d67893d9cd48b3b`, matching the clean starting local head on the assigned branch. No merge or rebase was in progress. The published commit's parent is `51f3aca78915f715f5aa382050158e4e91a46f44`; earlier rebase and diagnostic notes below describe historical work, not this fallback.
- Merged freshly fetched target `dc5cfec36143ec025709263bce3f76a03253a125` into the published PR history without rebasing, retaining the remote PR head as the first parent.
- Resolution: only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs and one current Workpad/status block. All source files and `docs/invariants.md` merged automatically; no manual source edits or generated-input changes were required.
- Key issue files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/orchestrator.go`, existing audit/snapshot tests, `docs/invariants.md`, and profile/replay report `docs/diagnostics/promotion-3436.md`.
- Validation: no tests, lint, vet, builds, local gate (including `true`), CI checks, or validation waits run in this fallback. Historical diagnostics do not validate this resolved head. Gate/CI, quiet-window, slow-check, and post-merge timings are unmeasured.
- Open items: Detent owns independent ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No out-of-scope findings. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7189"
  completion_generation: "60"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical target and PR handoffs

These records concern earlier heads or other issues. Their validation and completion metadata do not apply to the current #3436 merge and do not authorize validation or publication in this fallback session.

## Historical Workpad

- Issue #3436: remove Merging's repeated PR/check/review/thread hydration and full gate evaluation when its trusted head/base audit leaves the lane unchanged. A lane-changing audit verdict retains the existing live refresh; merge preparation keeps live eligibility ownership. No mechanism, projection, config, or lane writer added.
- Key files: `internal/orchestrator/autopromote_tick.go`, `orchestrator.go`, existing audit/snapshot tests; INV-1 ownership notes updated. Recorded profile and replay harness: `docs/diagnostics/promotion-3436.md`.
- Reproduced before editing: one repeated PR/thread hydration in Merging; nine active attempts behind an empty published snapshot returned zero during slow promotion.
- Measurements: Mac Detent same-head cold/warm 2.267041s/1.979825s -> 208.750µs/14.250µs; Prometheus Pyro 2.218932s/1.628063s -> 119.461µs/20.428µs. Counted requests 7 REST + 1 GraphQL cold, 5 REST + 1 GraphQL warm -> zero for passing Merging audits on both hosts. Controlled isolated promotion, not deployed fleet throughput.
- Validation: focused promotion/audit/merge-worker/required-policy/snapshot diagnostics passed across orchestrator and GitHub connector; orchestrator vet passed. No full suite or CI gate; configured gate `true` runs immediately before publication.
- Rebased onto current develop `e109b2f8e8eaa40633f597769859c1ee4fa1f1ce`; only historical notes conflicted. Repeated focused diagnostics passed (orchestrator 2.711s; GitHub connector 0.269s), and orchestrator vet passed.
- Open items: draft PR publication, review, ready mark, final canonical issue Workpad. Orchestrator owns lanes and merge. Remaining applicable Human Review/Rework reads can still occupy the actor; background worker snapshots remain observable. Live post-deployment refresh/throughput unmeasured.
- Skill draft: no — request instrumentation uses existing constructor DI and the established worker scratch/isolation contract.

```yaml
schema: 1
status: in_progress
blockers: []
human_action: null
```

## Historical Workpad

Plan: merged fetched develop `73859c96fa25b6351c9e9ef77e372077c4b2fa44` into source-clean published PR #3061 head `78cb53ad273c059c5bf97afe6d962110713b192d`, preserving published history without rebasing. Verified the starting head's parents against the preceding #3060 handoff.

Resolution: only `.detent/notes.md` conflicted. Preserved both sides' historical notes below. Source files merged automatically; no manual source changes or out-of-scope findings. Key issue files: `internal/orchestrator/autopromote.go`, `internal/orchestrator/rework_live_promotion_test.go`, and `internal/orchestrator/attempt_allowance_test.go`.

Validation: no tests, builds, local gate, CI checks, or validation waits run. Historical evidence does not validate this head; timings are unmeasured.

Open items: Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane mutation performed.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7171"
  completion_generation: "42"
blockers: []
human_action: null
```

# Historical PR notes

These historical records apply to earlier heads or other issues, and do not authorize validation or publication in this session.

## Historical Workpad

Plan: merged fetched develop `4e4334ef3e97a4bf593b81654b3be8d457c8b1e4` into source-clean published PR #3061 head `0b6f79d6de02e29042cf36d0f11840bec8b20a64`, preserving published history without rebasing. The starting head's parents match the prior #3060 merge handoff below.

Resolution: only `.detent/notes.md` conflicted; preserved both sides' historical handoffs. All source files merged automatically, with no manual source edits or out-of-scope findings. Key issue files: `internal/orchestrator/autopromote.go`, `internal/orchestrator/rework_live_promotion_test.go`, and `internal/orchestrator/attempt_allowance_test.go`.

Validation: no tests, builds, local gate, CI checks, or validation waits run. Historical diagnostics do not validate this head; timings are unmeasured.

Open items: Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane mutation performed.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7165"
  completion_generation: "36"
blockers: []
human_action: null
```

# Historical handoffs

The following evidence and status metadata concern earlier heads or other issues.

# Issue 3060 handoff

- Root path: `internal/orchestrator/autopromote.go` disabled the security audit gate for a clean Rework head with missing evidence, so Rework promotion never invoked `startSecurityAuditStage`. The existing merge gate still checks trusted evidence.
- Fix removes that bypass. `internal/orchestrator/rework_live_promotion_test.go` covers stale old-head run, pending current-head audit, and promotion after trusted pass. `internal/orchestrator/attempt_allowance_test.go` now expects audit start before Merging.
- `go test ./internal/orchestrator/...` and `go vet ./internal/orchestrator/...` passed. `make check-fast` passed 2026-09-24.
- The operator verified immutable trusted audit run 397 for PyroApex #2556, base `9378fc1f74bfcc5ab8f83b6370d59037b2d24fc4`, head `47461c60b4d7bbeeadd782a6f18974a7bbd277ad`: pass, started 10:50:09 UTC and completed 10:52:05 UTC. Detent merged the PR at 10:52:28 UTC as `d4b075967ad4715e04b99b49afaae6acf7a8ea31`. The observed defect was premature Rework → Merging at 10:40 while the audit was missing; the merge worker obtained the pass before merging. Worker workspaces cannot reach the production tailnet endpoint by design. The human waived direct worker verification of that already-merged PR and will check live behavior after release.
- Rework follow-up 2026-09-30: the current-head audit regression now supplies completed gate-wait state and verifies the dispatch planner returns `awaiting_gate` with no implementation dispatch while the audit runs. Focused audit, allowance, and completed-Rework dispatch regressions passed; focused vet passed. The initial added assertion failed because the audit-only fixture was not assigned to a worker; the fixture now explicitly enables candidate eligibility.
- Later mobile audit observations recovered through the existing lifecycle and do not demonstrate lost audit runs. This PR removes the independently reproducible promotion bypass, not read-model refresh lag. No outstanding actionable PR review remains. Current attempt gate is `true` (no validation/status publication); prior `make check-fast` evidence is historical. Current-head checks remain skipped, not passing test evidence; no merge-group pass is claimed. Rework completion hands off to the orchestrator and does not authorize worker lane mutation or merge.
- Skill draft: no — this follow-up adds a focused regression assertion, not a reusable procedure.

## Historical Workpad

Plan: merged fetched develop `8c7b5bfb3685eac99854cefa52d4af02763f7aec` into published PR #3061 head `ac3f91893bf2330b008f6a580f451ad0565a7665`, which matched the clean starting local head. The starting merge parents confirm the prior #3060 fallback recorded above; published history is preserved without rebasing.

Resolution: only `.detent/notes.md` conflicted. Preserved #3060 recovery evidence and incoming historical #3019 and #3076 handoffs. Source files merged automatically; no manual source edits or out-of-scope findings. Key issue files remain `internal/orchestrator/autopromote.go`, `internal/orchestrator/rework_live_promotion_test.go`, and `internal/orchestrator/attempt_allowance_test.go`.

Validation: no tests, builds, local gate, CI checks, or waits run in this fallback. Prior diagnostics are historical and do not validate this head. Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI waiting; timings are unmeasured here.

Open items: Detent verification, validation, and publication. No push, PR merge, issue-state change, or tracker lane mutation performed.

## Historical workpad for #3019

- Plan: merge fetched `origin/develop` at `e687a22a5324187fb5a24de3aa5668e8e61f28f5` into PR #3026's published head `bb60dc4e827b413fffa8562ec135b402761e83ff`, preserving both as ancestors without rebasing.
- Prior notes verified against the clean starting head and fetched PR ref. The historical #3019 notes name an earlier rebased head; the published head for this fallback is `bb60dc4e827b413fffa8562ec135b402761e83ff`. Prior diagnostics are historical only.
- Resolution: only `.detent/notes.md` conflicted. Preserved historical handoffs from both sides, including #3019 and #3001, and consolidated this current Workpad/status block. All source files merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/cli/boot.go`, `internal/cli/dev_runtime_e2e_test.go`; both remain unchanged from the published PR head.
- Validation: no tests, builds, local gate, CI checks, or waits run in this merge-fallback session. No current-head validation credit claimed; gate/CI and post-merge timings are unmeasured.
- Open items: Detent owns resolved-head ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

# Issue #3211 merge fallback handoff, 2026-09-30

- Verified the source-clean starting published PR head `d2a12be6c4b0ca9dfdf4a6745547dbd744a2edcd` has parents `319375baf3832cabbf176c0909c66cce01f22b7b` and prior target `e687a22a5324187fb5a24de3aa5668e8e61f28f5`. Verified the preceding handoff's starting-head parent record against Git history.
- PR #3281 remains open against develop on the assigned isolated branch. Fetched the PR branch at `d2a12be6c4b0ca9dfdf4a6745547dbd744a2edcd` and target develop at `8c7b5bfb3685eac99854cefa52d4af02763f7aec`. Merged that pinned target into the published PR history without rebasing, preserving the remote PR head as an ancestor.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs, including incoming #3019 notes, and retained one current Workpad/status block. Incoming changes to `docs/invariants.md`, `internal/cli/boot.go`, `internal/cli/dev_runtime_e2e_test.go`, `internal/orchestrator/completion_transition_test.go`, `internal/runner/prompt.go`, and `internal/runner/prompt_test.go` merged automatically. No manual source changes or out-of-scope findings.

## Historical Workpad

Plan: commit the resolved target merge, then return immediately to Detent with a source-clean workspace.

Validation: no tests, vet, builds, local gate, CI checks, or validation waits performed. No validation success is claimed for this head. Historical validation below applies only to earlier heads. Gate/CI and other validation timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state changes, or tracker lane writes performed.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7150"
  completion_generation: "21"

# Incoming historical status fields:
  completion_work_attempt_id: "7152"
  completion_generation: "23"
blockers: []
human_action: null
```

## Historical target-branch notes

# Historical handoffs

These notes concern earlier heads or other issues; their validation and completion metadata do not apply to this head. Historical instructions do not authorize validation or publication in this merge-fallback session.

# Historical #3211 merge fallback handoff, 2026-09-30

- Verified the source-clean starting local head `319375baf3832cabbf176c0909c66cce01f22b7b` has parents `0d5e8028643d0d0de7b3ecf4e07aff0bcde629a0` and prior target `9826088a821ba72ecfd033bc60b9ed3138762507`. The preceding handoff's local merge and published PR parent records match Git history.
- PR #3281 remains open against develop on the assigned isolated branch. Fetched the PR branch at `d56be7735f63444bc99fa5a25ebbf510ef3c56b4` and target develop at `e687a22a5324187fb5a24de3aa5668e8e61f28f5`. Merged that pinned target into the existing local history without rebasing; the published PR head remains an ancestor.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs, consolidated the duplicate #3427 section, and retained one current Workpad/status block. Incoming changes to `docs/invariants.md`, `internal/cli/runner.go`, `internal/cli/runner_test.go`, `internal/cli/startup_workflow_test.go`, `internal/project/manager.go`, and `internal/workspace/workspace.go` merged automatically. No manual source changes or out-of-scope findings.

## Historical Workpad (prior #3211 head)

Plan: finish and commit the resolved target merge, then return immediately to Detent with a source-clean workspace.

Validation: no tests, vet, builds, local gate, CI checks, or validation waits performed. No validation success is claimed for this head. Historical validation below applies only to earlier heads. Gate/CI and other validation timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state changes, or tracker lane writes performed.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7144"
  completion_generation: "15"
blockers: []
human_action: null

# Historical #3211 merge fallback handoff, 2026-09-30

- Verified the source-clean starting local merge `0d5e8028643d0d0de7b3ecf4e07aff0bcde629a0` has parents published PR head `d56be7735f63444bc99fa5a25ebbf510ef3c56b4` and prior target `4686438c63f97741e8306726836a1d703961c664`, confirming the preceding handoff. The published head's recorded parents also match Git history.
- PR #3281 remains open against develop on the assigned isolated branch. Fetched the PR branch and target `origin/develop` at `9826088a821ba72ecfd033bc60b9ed3138762507`. Merged that target into the existing local PR history without rebasing; the published PR head remains an ancestor.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs and one current Workpad/status block. Incoming changes to `internal/cli/boot_test.go`, `internal/cli/ssh_runner_test.go`, `internal/connector/github/statuslabel.go`, and `internal/connector/github/statuslabel_test.go` merged automatically; no manual source changes or out-of-scope findings.

## Historical Workpad (prior #3211 local head)

Plan: commit the resolved target merge and return immediately to Detent with a source-clean workspace.

Validation: no tests, vet, builds, local gate, or CI checks run or awaited in this session. No validation success is claimed for this head. Historical validation below applies only to earlier heads. Gate/CI and other validation timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state changes, or tracker lane writes performed.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7135"
  completion_generation: "6"
blockers: []
human_action: null

# Historical target and PR handoffs

The following notes concern earlier heads or other issues; their validation and completion metadata do not apply to the current #3211 head. Historical instructions do not authorize validation or publication in this merge-fallback session.

# Historical #3211 merge fallback handoff, 2026-09-30

- Verified the clean starting published PR #3281 head `d56be7735f63444bc99fa5a25ebbf510ef3c56b4` has parents `056f1273a0b402f9a2778b8024c06b91845799f0` and prior target `8a94591757c917387c3530454e30d25962ed0448`, confirming the previous handoff. Earlier recorded merge parents were also verified against Git history. PR #3281 remains open against develop and includes `Fixes #3211`.
- Fetched the PR branch and target `origin/develop` at `4686438c63f97741e8306726836a1d703961c664`. Merged the target into the assigned PR branch without rebasing; the published PR head is preserved as the first parent.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs below and consolidated the active workpad here. All source files merged automatically; no manual source changes or out-of-scope findings.

## Historical Workpad (prior #3211 head)

Plan: finish and commit the target merge, then return immediately to Detent with a source-clean workspace.

Validation: no tests, vet, builds, local gate, or CI checks run or awaited in this session. No validation success is claimed for this head. Earlier validation claims below apply only to historical heads. Gate/CI and other validation timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state changes, or tracker lane writes performed.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7127"
  completion_generation: "57"
blockers: []
human_action: null

# Historical target and PR notes

The following handoffs concern earlier heads or other issues; their validation claims and completion metadata do not apply to the current #3211 head.

# Historical #3211 merge fallback handoff, 2026-09-30

- Verified the clean starting published PR #3281 head `056f1273a0b402f9a2778b8024c06b91845799f0` has parents `74d20e4a358b95a6a05bd055aae8238140cdb4c1` and prior target `c4455b9277dbcb7a9622a8d5a369bb1a7283cc5c`, confirming the previous handoff. Earlier merge-parent notes were also verified against Git history.
- Fetched the PR branch and target `origin/develop` at `8a94591757c917387c3530454e30d25962ed0448`. Merged the target into the assigned PR branch without rebasing; the published PR head is preserved as the first parent.
- Resolved only two conflicts: `internal/policy/policy.go` retains both the PR's `HumanReview` field and develop's `GitHubPullRequest` field with their existing JSON tags; `.detent/notes.md` preserves historical handoffs below with one active workpad. All remaining files merged automatically.

# Historical #3211 merge fallback handoff, 2026-09-30

- Verified published PR #3281 head `74d20e4a358b95a6a05bd055aae8238140cdb4c1` has parents `2cbb7131b37b6e5b8d1554b36615aa11879d4816` and previous target `d14138a37c7597542c30e19616271a971b62fc8f`. Verified the prior published head's parents `27951a8e9f2950be91cd2a5830d3e161f93deda9` and `d80e68213d2768b5ebf7e95313bf35f40042411f`, and the earlier merge's parents `d97be0deadf64576efb6da220c16b47be8b9ba50` and `73d1c330b04253600438c12e0f774d3fe16b26d8`, confirming prior notes.
- Started source-clean without an in-progress merge or rebase. Fetched `origin/develop` at `c4455b9277dbcb7a9622a8d5a369bb1a7283cc5c` and merged it into the assigned PR branch without rebasing; the published PR head remains the first parent.
- Only `.detent/notes.md` conflicted. Resolved it by keeping the current #3211 handoff and preserving other issues' historical notes below. Source files merged automatically; no manual source changes were needed.
- Prior resolution key files: `docs/invariants.md`, `internal/runner/prompt.go`, `internal/hubserver/migrations/00048_hosted_blocked_lane.sql`, `internal/hubserver/migrate.go`, and `internal/hubserver/hosted_review_lane_migration_test.go`. Prior schema 48 and 47→48 migration notes and validation claims apply to earlier heads only.

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

## Historical workpad for #3403

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

# Additional historical develop handoffs

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

## Historical Workpad (incoming develop head)

Plan: Rework repair for #3400 preserves recovered merge 83598d050 and merges develop a53a7f204a278ae2f965560d6c4c10f96eb5b22d into PR #3421's published history. Only notes conflicted; retained historical handoffs from both sides and one current status block. No manual source edits or generated-input changes were required. The remaining issue source diff is internal/cli/boot_test.go and internal/cli/ssh_runner_test.go.

Recovery: inspected all 24 paths in 83598d050; inherited develop changes and its issue-owned notes resolution are retained for publication. No stray artifacts were present. Publication resolves the recovered unpushed commit; no stash or tracker lane writes.

Validation: focused CLI/orchestrator command passed all seven selected regression names at the resolved source (CLI 9.977s; orchestrator 0.612s). Windows/amd64 CLI test cross-compilation passed (10.1s), with output under the provided TMPDIR. Compilation is not native Windows execution. No source changes followed these diagnostics. No full suite, coverage, race suite, Actions rerun, or blocking CI wait ran. Configured `true` must run on the final committed head immediately before publication; exact gate and current-head eligibility evidence are recorded in the canonical issue Workpad.

Open items: next scheduled native Windows validation confirms the portability repair; squash merge and lane transitions remain orchestrator-owned. PR #3421 is already non-draft against develop and references Fixes #3400. No actionable reviews or threads existed at the pre-publication read; current-head checks were absent under repository policy and confer no test credit. No merge-group CI is configured. No new out-of-scope finding or dependency.

Timings: focused diagnostic command 20.5s; CLI Windows cross-compilation 10.1s. No quiet window is configured. The no-op gate takes under 1s; PR CI wait, slow checks, and post-merge main-CI are not applicable to this Rework handoff. Final publication verification belongs in the issue Workpad.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7129"
  completion_generation: "59"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null

# Issue #3400 merge fallback handoff (attempt 7080, generation 12)

- Verified local and remote PR #3421 head at 85afc4271bf2f230dc8e7f73e86eb3882d364425, with a source-clean worktree and no rebase in progress. Earlier notes claiming current mergeability were historical.
- Merged freshly fetched develop commit 436986dea421d273686ce243b9f6725a1b4b616f into the existing PR branch, preserving the remote PR head as an ancestor. Restarted the uncommitted merge after the shared target ref advanced.
- Conflicts only: internal/cli/runner_test.go, internal/orchestrator/review_head_test.go, internal/orchestrator/startup_observability_test.go. Used develop's equivalent SSH wrapper/local-runner assertions, explicit optional automated-review mode matching deadline expectations, and active-candidate/terminal-cleanup fixture separation.
- Remaining PR code changes against the fetched target: internal/cli/boot_test.go and internal/cli/ssh_runner_test.go; native gh helper, explicit sh commands, Windows USERPROFILE/PATH handling, and POSIX sshd fixture skip. No production edits added by this resolution.
- Prior validation from the supplied #3400 notes: seven focused tests passed on macOS and both affected packages cross-compiled for Windows/amd64 at the original PR head. Those results are historical; native Windows execution remains pending.
- This fallback ran no tests, builds, local gate, or CI. Resolved-file conflict and whitespace inspection was clean. A broader Git whitespace inspection flagged inherited generated JavaScript in static/app/conversation/app.js; it was left unchanged and does not require conflict-resolution work.
- Detent owns resolved-head verification, bounded validation, lease-protected publishing, and CI after return. No push, PR merge, or issue/lane mutations were performed.
- Open items: Detent validation of the merged head and next native Windows scheduled validation. No additional repair work identified.

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

## Recovery verification 2026-09-30

- Recovered merge commit belongs to #3060 and is retained for publication. Fetched develop advanced to `bccee4c50f86019fd961a4c457a59a2c91e9eb31`; merged it, preserving both historical note sections.
- Initial focused test and vet diagnostics exited 1: `internal/runner/ssh_protocol.go:210:85: undefined: ErrSessionNoProgress`. Recorded follow-up #3432; current develop already fixes this in #3431, so no runner edit was made here.
- Skill draft: no — recovery and focused validation add no reusable procedure.
- Resolved-source diagnostics passed: focused current-head audit, Rework audit evaluation, allowance live-head, and completed-Rework dispatch regressions; `go vet ./internal/orchestrator/...` passed. Configured `true` gate runs before publication; no status or full-suite pass is claimed.

# Issue #3019 (historical implementation handoff)

- Key files: `internal/cli/boot.go`, `internal/cli/dev_runtime_e2e_test.go`.
- Existing fix publishes the dashboard address immediately after binding, before runtime store and board snapshot initialization. The blocked snapshot-load regression covers fixture and screenshots modes and checks `/health` after releasing the load.
- Rework was an old CI failure, with no actionable human or bot review. #2975 and #3009 are closed; their fixes are merged into `origin/main`. Current `develop` has independent platform fixes; prior full-CI failures are historical under the operator's disabled blocking-gate policy.
- Rebased the single issue commit cleanly onto `origin/develop` at `bccee4c50f86019fd961a4c457a59a2c91e9eb31`. Rebased head: `1cae634d21b925d2ef4570a6061d0888414342ae`.
- Current-head diagnostics: affected startup tests passed three repetitions (27.313s); four concurrent GOMAXPROCS=1 runs passed (10.056–11.679s); `go test ./internal/cli/... -count=1 -timeout=15m` passed (54.533s); `go vet ./internal/cli/...` passed. DETENT_API_TOKEN was cleared for tests.
- Prior deterministic regression failed on the old boot order after the unchanged 10-second empty-banner wait. Prior ambient host stress did not reproduce the observed host slowdown; no blanket timeout increase was made.
- Configured `true` gate passed exactly once immediately before pushing. Rebased head is pushed, ready PR #3026 targets `develop`, and the issue Workpad reports `complete` with no blockers. GitHub recheck confirmed exact head, no review threads/findings, and no current-head check rollup; no skipped checks are credited as tests.
- Open items: none for implementation/Rework. The orchestrator owns promotion and merging; do not merge during Rework or change tracker lane labels.
- Skill draft: no; the deterministic startup-order regression needs no new reusable procedure.

# Issue #3001

PR: https://github.com/digitaldrywood/detent/pull/3004 (ready, target develop).
Validated code head: d36c44c837dfbea11c1ee3d229c791638a4b1a67; rebased onto develop bccee4c50f86019fd961a4c457a59a2c91e9eb31 without conflict edits.

Key files: internal/cli/runner.go classifies identified unusable configured paths with project.ErrProjectDefinition; internal/workspace/workspace.go preserves canonicalization PathError identity; internal/project/manager.go retries terminal pending definitions on explicit reconciliation. Regression tests live in internal/cli/runner_test.go and startup_workflow_test.go; INV-3 is updated in docs/invariants.md.

Human review finding is fixed and resolved. ENOSPC, EIO, permission, backend and state-store failures remain fatal. No mechanism or configuration key added.

Dependencies #2975 (PR #2986) and #3009 (PR #3018) are closed/merged to main; their main merge SHAs are not ancestors of develop. Historical full PR CI is no longer a blocker under current operator policy. Focused doctor and workspace portability cases pass on macOS; no Windows test credit claimed.

Diagnostics: focused startup, classification, manager, doctor and workspace tests passed; go vet ./internal/cli/... ./internal/project/... ./internal/workspace/... passed. First rebased attempt could not compile because of retired SSH sentinel ErrSessionNoProgress; already fixed by merged #3431, incorporated in second clean rebase. Configured gate true is next immediately before push. No full checks or CI polling performed.

Handoff: publish the rebased ready PR with an exact lease and a complete canonical issue Workpad; the configured true gate runs immediately before that push. Orchestrator owns lane transitions and merge dispatch. Skill draft: no — routine rebase and diagnostics added no reusable procedure.

## Historical Workpad (#3019 merge fallback)

- Plan: merge fetched `origin/develop` at `e687a22a5324187fb5a24de3aa5668e8e61f28f5` into PR #3026's published head `bb60dc4e827b413fffa8562ec135b402761e83ff`, preserving both as ancestors without rebasing.
- Prior notes verified against the clean starting head and fetched PR ref. The historical #3019 notes name an earlier rebased head; the published head for this fallback is `bb60dc4e827b413fffa8562ec135b402761e83ff`. Prior diagnostics are historical only.
- Resolution: only `.detent/notes.md` conflicted. Preserved historical handoffs from both sides, including #3019 and #3001, and consolidated this current Workpad/status block. All source files merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/cli/boot.go`, `internal/cli/dev_runtime_e2e_test.go`; both remain unchanged from the published PR head.
- Validation: no tests, builds, local gate, CI checks, or waits run in this merge-fallback session. No current-head validation credit claimed; gate/CI and post-merge timings are unmeasured.
- Open items: Detent owns resolved-head ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7141"
  completion_generation: "12"
blockers: []
human_action: null

## Issue #3076 merge fallback — 2026-09-30

- Verified remote PR #3296 head `717af073f247280b76bd5e645c8f9d5bd353af38` matches the retained local head; historical publication notes refer to an older head.
- Merged current `origin/develop` without rebasing; retained both independent additions in `docs/invariants.md` and both regressions in `internal/runner/prompt_test.go`. `internal/runner/prompt.go` merged automatically.
- No local validation or CI run in this fallback session. Prior test results are historical, not evidence for the resolved head.
- Open items: Detent verifies the clean head and ancestry, validates, publishes with lease protection, and waits for current-head CI. No unrelated work identified.

# Historical incoming develop notes

# Issue #3433 merge fallback handoff

- Verified PR #3434 is open, targets `develop`, includes `Fixes #3433`, and has published head `1ffd4361802762ac780bcdfe725eac84b63ef98c`, matching the clean starting local branch. No rebase or merge was in progress. The prior #3433 handoff matches that commit's notes and scoped PTY annotation change; its validation is historical only.
- Merged freshly fetched develop commit `4e4334ef3e97a4bf593b81654b3be8d457c8b1e4` into the published head without rebasing. The published PR head is retained as the first parent.
- Only conflict: `.detent/notes.md`. Preserved both sides' historical handoffs and consolidated one current Workpad/status block. All source files merged automatically; no manual source edits or generated-input changes were required.
- Key issue file: `internal/workspaceterminal/pty_unix.go`; the four best-effort cleanup annotations remain unchanged from the published PR head.

## Historical workpad before #3436

Plan: finish and commit the resolved target merge, then return immediately to Detent with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate, CI checks, or waits run in this merge-fallback session. Prior lint and package-test results below do not validate this resolved head. Gate/CI, quiet-window, slow-check, and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No out-of-scope findings. No push, PR merge, issue-state change, or tracker lane write performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7161"
  completion_generation: "32"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical target and PR handoffs

The following notes concern earlier heads or other issues. Their validation and completion metadata do not apply to the current #3433 merge; historical instructions do not authorize validation or publication in this session.

## Historical Workpad (#3211 merge fallback)

Plan: commit the resolved target merge, then return immediately to Detent with a source-clean workspace.

Validation: no tests, vet, builds, local gate, CI checks, or validation waits performed. No validation success is claimed for this head. Historical validation below applies only to earlier heads. Gate/CI and other validation timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state changes, or tracker lane writes performed.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7152"
  completion_generation: "23"
blockers: []
human_action: null

# Issue #3433 handoff

- Reproduced the recorded four `errcheck` findings on clean develop `9826088a821ba72ecfd033bc60b9ed3138762507`, using the repository-pinned golangci-lint v2.9.0 built with Go 1.26.6 against `internal/workspaceterminal/...` (exit 1, 2.2s).
- `internal/workspaceterminal/pty_unix.go` adds four line-scoped `nolint:errcheck` annotations to documented best-effort descriptor cleanup. PTY startup, ownership, returned errors, and cleanup operations are unchanged; lint configuration and invariants are unchanged.
- The identical focused lint command passes with zero issues (0.9s). Existing package tests pass: `env -u DETENT_API_TOKEN GOMAXPROCS=4 GOTOOLCHAIN=go1.26.6 go test -p 4 ./internal/workspaceterminal/... -count=1` (5.6s command, 5.289s package). `git diff --check` passes. No new test: annotations change no runtime behavior; the pinned lint invocation reproduces the recorded failure.
- No generated inputs, UI surfaces, mechanisms, or out-of-scope findings. No full check-fast, coverage, race suite, or Actions wait. The configured gate is `true` and publishes no local-gate status. Publication, exact-head gate/check/review evidence, and completion for attempt 7134 / generation 5 belong to the canonical issue Workpad. Squash merge and lane transitions remain orchestrator-owned.
- Skill draft: no — this is a routine scoped lint annotation fix.

## Historical workpad before #3436

Merge fallback for #3271 / PR #3355, 2026-09-30.

- Plan: merge fetched `origin/develop` (`8c7b5bfb3685eac99854cefa52d4af02763f7aec`) into the published PR branch, retaining remote PR head `f5b842502b7aeffe08d80c37cada0e0419514ac1` as an ancestor. No rebase or publication in this session.
- Key files: `web/conversation/src/app/account/Setup.tsx` retains develop's runner-checkout association behavior and the PR's contextual help. Updated adjacent help and `tests/visual/onboarding-help.spec.js` references for the renamed control. Regenerated conflicted `static/app/conversation/app.js` with `npm run build`; no other generated files changed.
- Prior notes: no #3271 handoff was present; notes imported from develop concern other issues and remain historical. PR body already includes `Fixes #3271`.
- Validation: bundle generation completed successfully; no tests, local gate (including `true`), CI checks, or CI waiting ran. Prior PR test evidence is historical and does not validate this resolved head.
- Open items: Detent owns clean-head/ancestry verification, bounded validation, lease-protected push, and current-head CI waiting. No unrelated work identified. The PR remains open and tracker state is unchanged.

```yaml
schema: 1
status: complete
blockers: []
human_action: null
```

# Issue #3417 merge fallback handoff, 2026-09-30

- Verified the source-clean starting local merge `cd0b59d585e6de1fcf7cd29d8599d08bbaadb705` has parents published PR head `c6eb99858d9bc6d99441cc5c297323e10c2fa99c` and prior target `73859c96fa25b6351c9e9ef77e372077c4b2fa44`, confirming the preceding handoff. The issue fixture matches preceding published head `44d367cc86a9d96650ded69e3cc78da189bf4e30`; prior validation remains historical.
- PR #3435 is open against develop on the assigned branch. Fetched published PR head `c6eb99858d9bc6d99441cc5c297323e10c2fa99c` and target `9d1ef11cf04e04db0babeab159b989abac01a5aa`. Merged the pinned target into the existing local PR history without rebasing, preserving the published head as an ancestor.
- Only `.detent/notes.md` conflicted. Preserved both sides' historical handoffs and consolidated one current Workpad/status fence. All source files merged automatically; no manual source changes or out-of-scope findings.

## Historical Workpad

Plan and result: commit the resolved target merge and return immediately to Detent with a source-clean workspace. Key issue file: `internal/orchestrator/orchestrator_test.go`; the fixture repair is retained without manual edits.

Validation: no tests, lint, vet, builds, local gate, CI checks, or validation waits performed in this session. No validation success is claimed for this head; historical diagnostics below apply only to earlier heads. Gate/CI and other validation timings are unmeasured.

Open items: Detent owns independent branch ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, tracker lane write, or live-instance mutation performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7181"
  completion_generation: "52"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs

The following records concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

# Issue #3417 merge fallback handoff, 2026-09-30

- Verified source-clean local and fetched published PR #3435 head `c6eb99858d9bc6d99441cc5c297323e10c2fa99c`, with parent `2d4bf665ad72c88cd4c7183b0187d68516073a92`. No merge or rebase was in progress; the pre-check rebase was already absent. PR #3435 is open against `develop` on the assigned isolated branch.
- Prior #3417 Rework notes were verified against history: the published commits descend from target `4e4334ef3e97a4bf593b81654b3be8d457c8b1e4`, and the repaired `internal/orchestrator/orchestrator_test.go` fixture matches the preceding published head `44d367cc86a9d96650ded69e3cc78da189bf4e30`. Verified the supplied #3211 starting-head parent record against Git history. All prior validation is historical only.
- Merged fetched target `73859c96fa25b6351c9e9ef77e372077c4b2fa44` into the published PR history without rebasing, preserving the published PR head as the first parent.
- Only `.detent/notes.md` conflicted: retained both sides' historical handoffs and consolidated one current Workpad/status fence. Incoming `internal/workspaceterminal/pty_unix.go` changes merged automatically; no manual source changes or out-of-scope findings.

## Historical Workpad (#3417 prior merge)

Plan and result: resolve and commit the target merge, then return immediately to Detent with a source-clean workspace. Key issue file: `internal/orchestrator/orchestrator_test.go`; the fixture repair is retained without manual edits.

Validation: no tests, lint, vet, builds, local gate, CI checks, or validation waits performed in this merge-fallback session. No validation success is claimed for this head; historical diagnostics below apply only to earlier heads. Gate/CI, quiet-window, slow-check, and post-merge timings are unmeasured.

Open items: Detent owns independent branch ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, tracker lane write, or live-instance mutation performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7173"
  completion_generation: "44"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs

These notes concern earlier heads or other issues. Their validation and completion metadata do not apply to this #3417 merge. Historical instructions do not authorize validation or publication in this session.

## Historical Workpad (#3433 merge fallback)

Plan: finish and commit the resolved target merge, then return immediately to Detent with a source-clean workspace.

Validation: no tests, lint, vet, builds, local gate, CI checks, or waits run in this merge-fallback session. Prior lint and package-test results below do not validate this resolved head. Gate/CI, quiet-window, slow-check, and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness, and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No out-of-scope findings. No push, PR merge, issue-state change, or tracker lane write performed here.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7161"
  completion_generation: "32"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null

# Historical target and PR handoffs

The following notes concern earlier heads or other issues. Their validation and completion metadata do not apply to the current #3417 merge; historical instructions do not authorize validation or publication in this session.

## Historical Workpad (#3417 implementation and Rework)

Plan and result: #3417 repairs the existing operator-move regression fixture. The prior attempt reproduced the timeout on develop 9826088a821ba72ecfd033bc60b9ed3138762507: snapshot reuse skipped transition reads for fetched cards, leaving the untargeted injected error for dispatch hydration. The fixture supplies the unrelated Blocked card through the initial observed feed, then omits it from both feeds to force its transition lookup to fail. Existing assertions prove Rework dispatch and preservation of the unrelated block; the added assertion proves error consumption. Production behavior and invariants are unchanged.

Rework attempt 7162 / generation 33: verified clean local HEAD matched published PR #3435 head 44d367cc86a9d96650ded69e3cc78da189bf4e30 and read the canonical issue Workpad and feedback. Rework was caused by merge conflicts, with no actionable review findings. Rebased onto fetched develop 4e4334ef3e97a4bf593b81654b3be8d457c8b1e4. Only .detent/notes.md conflicted; preserved target historical handoffs and consolidated one current Workpad/status fence. The repaired Go test file exactly matches the prior published source.

Validation: related operator-move, blocked-status, and transition-snapshot diagnostics passed five repetitions on the rebased source: env -u DETENT_API_TOKEN GOMAXPROCS=4 go test -p 4 ./internal/orchestrator -run '^(TestRunDispatchesOperatorMovedBlockedIssueDuringDegradedTransitionRefresh|TestHandleOperatorMove.*|TestRunTracksBlockedStatusIssuesForDisplayOnly|TestRefreshTransitionSets.*|TestTrackBlockedStatusIssuesResolvesCauseByPrecedence)$' -count=5 (0.692s package time; 8.2s command wall time). Original reproduction and prior repaired tests are recorded in the canonical issue Workpad. Whitespace inspection passed. No generated inputs changed; subsequent edits are notes only. Configured gate: true, to run on the committed head immediately before publication. No full gate, coverage, race suite, or CI wait.

Handoff: PR #3435 remains open, non-draft, targets develop, and includes Fixes #3417. At inspection, reviews and review threads were empty; the review bot reported its usage limit without findings. Current-head checks were absent, an expected skip with no test credit. Final committed-head gate, publication, mergeability, and feedback evidence are recorded in the canonical issue Workpad. Quiet window not configured; no-op gate under 1s; PR CI, merge-group CI, and slow checks not applicable. Merge and post-merge integrated develop validation remain orchestrator-owned. No dependency, out-of-scope finding, tracker lane writes, or live-instance mutation.

Skill draft: no — existing guidance covers this fixture repair and notes conflict resolution.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7162"
  completion_generation: "33"
blockers: []
human_action: null

## Historical Workpad (incoming develop)

Plan: merged fetched develop `73859c96fa25b6351c9e9ef77e372077c4b2fa44` into source-clean published PR #3061 head `78cb53ad273c059c5bf97afe6d962110713b192d`, preserving published history without rebasing. Verified the starting head's parents against the preceding #3060 handoff.

Resolution: only `.detent/notes.md` conflicted. Preserved both sides' historical notes below. Source files merged automatically; no manual source changes or out-of-scope findings. Key issue files: `internal/orchestrator/autopromote.go`, `internal/orchestrator/rework_live_promotion_test.go`, and `internal/orchestrator/attempt_allowance_test.go`.

Validation: no tests, builds, local gate, CI checks, or validation waits run. Historical evidence does not validate this head; timings are unmeasured.

Open items: Detent owns resolved-head verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane mutation performed.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7171"
  completion_generation: "42"
blockers: []
human_action: null
```

# Historical #3211 status metadata from develop

## Historical Workpad (incoming develop)

Merge fallback for #3271 / PR #3355, 2026-09-30.

- Plan: merge fetched `origin/develop` (`8c7b5bfb3685eac99854cefa52d4af02763f7aec`) into the published PR branch, retaining remote PR head `f5b842502b7aeffe08d80c37cada0e0419514ac1` as an ancestor. No rebase or publication in this session.
- Key files: `web/conversation/src/app/account/Setup.tsx` retains develop's runner-checkout association behavior and the PR's contextual help. Updated adjacent help and `tests/visual/onboarding-help.spec.js` references for the renamed control. Regenerated conflicted `static/app/conversation/app.js` with `npm run build`; no other generated files changed.
- Prior notes: no #3271 handoff was present; notes imported from develop concern other issues and remain historical. PR body already includes `Fixes #3271`.
- Validation: bundle generation completed successfully; no tests, local gate (including `true`), CI checks, or CI waiting ran. Prior PR test evidence is historical and does not validate this resolved head.
- Open items: Detent owns clean-head/ancestry verification, bounded validation, lease-protected push, and current-head CI waiting. No unrelated work identified. The PR remains open and tracker state is unchanged.

```yaml
schema: 1
status: complete
blockers: []
human_action: null
```

## Additional incoming historical records

- Plan: merge fetched `origin/develop` at `e687a22a5324187fb5a24de3aa5668e8e61f28f5` into PR #3026's published head `bb60dc4e827b413fffa8562ec135b402761e83ff`, preserving both as ancestors without rebasing.
- Prior notes verified against the clean starting head and fetched PR ref. The historical #3019 notes name an earlier rebased head; the published head for this fallback is `bb60dc4e827b413fffa8562ec135b402761e83ff`. Prior diagnostics are historical only.
- Resolution: only `.detent/notes.md` conflicted. Preserved historical handoffs from both sides, including #3019 and #3001, and consolidated this current Workpad/status block. All source files merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/cli/boot.go`, `internal/cli/dev_runtime_e2e_test.go`; both remain unchanged from the published PR head.
- Validation: no tests, builds, local gate, CI checks, or waits run in this merge-fallback session. No current-head validation credit claimed; gate/CI and post-merge timings are unmeasured.
- Open items: Detent owns resolved-head ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

# Issue #3211 merge fallback handoff, 2026-09-30

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7161"
  completion_generation: "32"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

The following notes concern earlier heads or other issues. Their validation and completion metadata do not apply to the current #3433 merge; historical instructions do not authorize validation or publication in this session.

# Additional historical records from fetched develop

# Issue #3257 current merge fallback

- PR #3362 is open against `develop` on the assigned isolated branch. Starting source-clean local and fetched published PR head: `5e09128949f00054d84c99db6c62fc42520c41fa`.
- Verified the preceding handoff against the starting merge's parents: `132fc057840d8b963fa39d20c29285dce6fb66b1` and prior target `9d1ef11cf04e04db0babeab159b989abac01a5aa`. The recorded parents of `132fc057840d8b963fa39d20c29285dce6fb66b1` also match Git history.
- Merged freshly fetched target `d406805ccd1cf83692852af9daf5fc7bdbf2848e` into the published PR history without rebasing. The remote PR head is preserved as the first parent.
- Only `.detent/notes.md` conflicted. Preserved historical handoffs from both sides, including incoming #3417 notes, with one current Workpad/status fence. All source files merged automatically; no manual source edits, asset generation, or out-of-scope findings.
- Key prior issue files remain `internal/cli/boot.go`, `internal/cli/hub_client.go`, `internal/hubclient/scheduler.go`, `internal/hubserver/migrations/00049_linked_issue_sources.sql`, and the linked-issue conversation sources and bundle. This retry only resolves handoff notes.

## Historical Workpad

Plan and result: finish and commit the resolved target merge, then return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, typecheck, local gate (including `true`), CI checks, or validation waits run in this session. Historical evidence below does not validate this head. Gate/CI, quiet-window, slow-check, and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane mutation performed here.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7192"
  completion_generation: "63"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null
```

# Historical handoffs retained from the published PR

These records concern earlier heads or other issues. Their validation and completion metadata do not apply to this head or authorize validation or publication in this session.

# Issue #3257 previous merge fallback

- PR #3362 remains open against `develop` on the assigned isolated branch.
- Starting source-clean local and fetched published PR head: `132fc057840d8b963fa39d20c29285dce6fb66b1`, with parents `9f00a70849fbc40d427fad02fcc8bb4b9b6f8220` and prior target `73859c96fa25b6351c9e9ef77e372077c4b2fa44`. Verified the preceding handoff and its starting-head parent records against Git history.
- Merged freshly fetched target `9d1ef11cf04e04db0babeab159b989abac01a5aa` into the published PR history without rebasing.
- Conflicts: `.detent/notes.md` and `static/app/conversation/app.js`. Preserved historical notes from both sides, including incoming #3060 and #3271 handoffs, with one current Workpad/status fence. Regenerated JavaScript from the automatically merged TypeScript sources to retain linked-issue hydration and contextual onboarding help.
- Prior resolution verified: `internal/hubserver/migrations/00049_linked_issue_sources.sql` and supported schema version 49 retain the migration collision repair. No manual source changes or out-of-scope findings.

## Historical Workpad

Plan: commit the resolved target merge and return immediately with a source-clean workspace.

Validation: no tests, lint, vet, typecheck, local gate (including `true`), CI checks or validation waits run. `npm run build` completed solely to regenerate the conflicted JavaScript; it is not validation credit. Generation emitted CSS, component sourcemap, font-resolution and chunk-size warnings. Historical results below do not validate this head. Gate/CI, quiet-window, slow-check and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing and current-head CI waiting. No push, PR merge or issue/lane mutation performed.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7179"
  completion_generation: "50"
blockers: []
human_action: null
```

# Historical handoffs

Historical validation and completion metadata do not apply to this head and do not authorize validation or publication in this session.

# Issue #3257 previous merge fallback

- PR #3362 remains open against `develop` on the assigned isolated branch.
- Starting source-clean local and fetched published PR head: `9f00a70849fbc40d427fad02fcc8bb4b9b6f8220`. Verified its parents `806d934c73b31ef2ee0c72967d0f0f6e00904875` and prior target `4e4334ef3e97a4bf593b81654b3be8d457c8b1e4` against the preceding handoff; the preceding starting-head parent record also matches Git history.
- Merged freshly fetched target `73859c96fa25b6351c9e9ef77e372077c4b2fa44` into the published PR history without rebasing.
- Only conflict: `.detent/notes.md`. Preserved historical handoffs from both sides, including incoming #3433 notes, and retained one current Workpad/status fence. Develop's `internal/workspaceterminal/pty_unix.go` cleanup annotations merged automatically; no manual source edits or asset generation required.
- Prior resolution verified: `internal/hubserver/migrations/00049_linked_issue_sources.sql` and supported schema version 49 retain the earlier migration collision repair. `internal/hubclient/native_landing_test.go` retains both landing-policy cases and the linked-source journey.

## Historical Workpad

Plan: commit the resolved target merge and return immediately with a source-clean workspace.

Validation: no tests, lint, vet, builds, asset generation, local gate (including `true`), CI checks or waits run. Historical results below do not validate this head. Gate/CI, quiet-window, slow-check and post-merge timings are unmeasured.

Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing and current-head CI waiting. No out-of-scope findings. No push, PR merge or issue/lane mutation performed.

```yaml
schema: 1
status: complete
fields:
  completion_work_attempt_id: "7172"
  completion_generation: "43"
blockers: []
human_action: null
```

# Issue #3257 previous merge fallback

- PR: https://github.com/digitaldrywood/detent/pull/3362 remains open against `develop` on the expected isolated branch; its body includes `Fixes #3257`.
- Starting source-clean local and fetched published PR head: `806d934c73b31ef2ee0c72967d0f0f6e00904875`, with parents `6fb4fdf88c238c9ad340e18c53a03aa72ba918fd` and prior target `8c7b5bfb3685eac99854cefa52d4af02763f7aec`. Verified the preceding handoff and its recorded merge parents; published history is retained without rebasing.
- Merged freshly fetched target `origin/develop` at `4e4334ef3e97a4bf593b81654b3be8d457c8b1e4` into the published PR head.
- Only text conflict: `.detent/notes.md`. Preserved historical handoffs from both sides, including incoming #3211 notes, with one current Workpad/status fence. Source files and `docs/invariants.md` merged automatically.
- Required merge blocker: incoming develop owns `00048_hosted_blocked_lane.sql`, colliding with this PR's linked-source migration. Kept develop's landed migration and its existing 47-to-48 fixture unchanged; renamed the PR migration to `internal/hubserver/migrations/00049_linked_issue_sources.sql` without changing its SQL and set `internal/hubserver/migrate.go` to supported schema version 49. No additional migration or mechanism added.
- Prior key file verified: `internal/hubclient/native_landing_test.go` retains plain-git and approved-GitHub-policy cases plus the linked-source journey. Earlier generated JavaScript resolution is retained; no asset generation required.
- Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing and current-head CI waiting. No out-of-scope finding identified; no push, PR merge or issue/lane mutation performed.

## Historical Workpad

Plan: merge fetched develop into the published PR history, resolve the handoff-notes conflict and migration-number collision, commit the resolution, and return immediately for Detent verification.

Validation: no tests, builds, asset generation, typecheck, local gate (including `true`), CI checks or waits run in this session. Historical validation and generation claims below do not validate this head. Quiet-window, gate/CI, slow-check and post-merge timings are unmeasured. Detent owns validation and publishing after return.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7164"
  completion_generation: "35"
blockers: []
human_action: null

# Issue #3257 previous merge fallback

- PR: https://github.com/digitaldrywood/detent/pull/3362 is open against `develop` on the expected isolated branch and includes `Fixes #3257`.
- Starting source-clean local head: `6fb4fdf88c238c9ad340e18c53a03aa72ba918fd`, with parents `0aa6169a6f5fe69faf05a961ea47fb6127ca06cc` and prior target `e687a22a5324187fb5a24de3aa5668e8e61f28f5`. Verified the preceding merge's parents against prior notes.
- Fetched published PR head: `a694184e469b0d4af4ce4c9430ce237dd013e5f9`; verified it remains an ancestor of the starting local head. Merged fetched `origin/develop` at `8c7b5bfb3685eac99854cefa52d4af02763f7aec` into the retained local resolution without rebasing.
- Prior key files verified: `internal/hubclient/native_landing_test.go` retains plain-git and approved-GitHub-policy cases plus the linked-source journey; `internal/hubserver/migrations/00048_linked_issue_sources.sql` and supported schema version 48 remain present. Earlier generated JavaScript resolution is retained.
- Only conflict: `.detent/notes.md`. Preserved historical handoffs from both sides, including incoming #3019 and #3076 notes, with one current Workpad/status fence. Source files and `docs/invariants.md` merged automatically; no manual source changes or asset generation required.
- Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing and current-head CI waiting. No out-of-scope finding identified; no push, PR merge or issue/lane mutation performed.

## Historical workpad retained from prior handoff

Plan: retain the unpublished committed resolution, merge freshly fetched develop, resolve only the handoff-notes conflict, and commit the merge for Detent verification.

Validation: no tests, builds, asset generation, typecheck, local gate (including `true`), CI checks or waits run in this session. Historical validation and generation claims below do not validate this head. Quiet-window, gate/CI, slow-check and post-merge timings are unmeasured. Detent owns validation and publishing after return.

Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7155"
  completion_generation: "26"
blockers: []
human_action: null


# Issue #3257 previous merge fallback

- PR: https://github.com/digitaldrywood/detent/pull/3362 is open against `develop` on the expected isolated branch; its body includes `Fixes #3257`.
- Starting source-clean local head: `0aa6169a6f5fe69faf05a961ea47fb6127ca06cc`, an unpublished merge of prior resolution `c8c63287456d730f8ea0c21178d8269f6d876ed2` and prior target `9826088a821ba72ecfd033bc60b9ed3138762507`. Verified the recorded parents and fetched PR head `a694184e469b0d4af4ce4c9430ce237dd013e5f9`, which remains an ancestor.
- Fetched target: `origin/develop` at `e687a22a5324187fb5a24de3aa5668e8e61f28f5`; merged into the existing local resolution without rebasing published commits.
- Prior key files verified: `internal/hubclient/native_landing_test.go` retains the plain-git and approved-GitHub-policy cases plus the linked-source journey; `internal/hubserver/migrations/00048_linked_issue_sources.sql` and supported schema version 48 remain present. Prior generated JavaScript resolution is retained.
- Only conflict: `.detent/notes.md`. Preserved historical handoffs from both sides, including incoming #3001, and retained one current Workpad/status fence. Source files and `docs/invariants.md` merged automatically; no manual source changes or asset generation required.
- Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing and current-head CI waiting. No out-of-scope finding identified; no push, PR merge or issue/lane mutation performed.

## Historical workpad retained from prior handoff

Plan: retain the unpublished committed resolution, merge freshly fetched develop into it, resolve the handoff-notes conflict, and commit the merge for Detent verification.

Validation: no tests, builds, asset generation, typecheck, local gate (including `true`), CI checks or waits run in this session. Historical validation and generation claims below do not validate this head. Quiet-window, gate/CI, slow-check and post-merge timings are unmeasured. Detent owns validation and publishing after return.

## Historical workpad retained from prior handoff

- Plan: merge fetched `origin/develop` at `e687a22a5324187fb5a24de3aa5668e8e61f28f5` into PR #3026's published head `bb60dc4e827b413fffa8562ec135b402761e83ff`, preserving both as ancestors without rebasing.
- Prior notes verified against the clean starting head and fetched PR ref. The historical #3019 notes name an earlier rebased head; the published head for this fallback is `bb60dc4e827b413fffa8562ec135b402761e83ff`. Prior diagnostics are historical only.
- Resolution: only `.detent/notes.md` conflicted. Preserved historical handoffs from both sides, including #3019 and #3001, and consolidated this current Workpad/status block. All source files merged automatically; no manual source edits or out-of-scope findings.
- Key issue files: `internal/cli/boot.go`, `internal/cli/dev_runtime_e2e_test.go`; both remain unchanged from the published PR head.
- Validation: no tests, builds, local gate, CI checks, or waits run in this merge-fallback session. No current-head validation credit claimed; gate/CI and post-merge timings are unmeasured.
- Open items: Detent owns resolved-head ownership, cleanliness, target-ancestry verification, bounded validation, lease-protected publishing, and current-head CI waiting. No push, PR merge, issue-state change, or tracker lane write performed here.

Historical statuses: #3257 attempt 7147 / generation 18 and #3019 attempt 7141 / generation 12 each recorded schema 1, status complete, blockers [], human_action null.

# Issue #3257 previous notes-only merge fallback

- PR: https://github.com/digitaldrywood/detent/pull/3362 is open against `develop` on the expected isolated branch; its body includes `Fixes #3257`.
- Starting source-clean local head: `c8c63287456d730f8ea0c21178d8269f6d876ed2`, an unpublished merge of published PR head `a694184e469b0d4af4ce4c9430ce237dd013e5f9` and prior target `8a94591757c917387c3530454e30d25962ed0448`. The fetched PR ref still matches that published head.
- Current fetched target: `origin/develop` at `9826088a821ba72ecfd033bc60b9ed3138762507`. Merged into the existing local resolution without rebasing; the published head remains an ancestor.
- Prior notes verified against recorded merge parents and key files: `internal/hubclient/native_landing_test.go` retains both landing-policy cases and the linked-source journey; migration `internal/hubserver/migrations/00048_linked_issue_sources.sql` and supported schema version 48 remain present. The existing generated JavaScript resolution is retained.
- Only conflict: `.detent/notes.md`. Preserved historical handoffs from both sides, with one current Workpad/status fence. Source files and `docs/invariants.md` merged automatically; no manual source changes or asset regeneration were required.
- Open items: Detent owns independent ownership, cleanliness and target-ancestry verification, bounded validation, lease-protected publishing and current-head CI waiting. No out-of-scope finding identified; no push, PR merge or issue/lane mutation performed.

## Historical workpad for the previous #3257 notes-only merge

Plan: retain the unpublished committed resolution, merge freshly fetched develop into it, resolve the handoff-notes conflict, and commit the merge for Detent verification.

Validation: no tests, builds, asset generation, typecheck, local gate (including `true`), CI checks or waits run in this retry. All validation and generation claims below are historical and do not validate this head. Quiet-window, gate/CI, slow-check and post-merge timings are unmeasured. Detent owns validation and publishing after return.

Historical status: complete; blockers: []; human_action: null.

# Issue #3257 previous source merge fallback

- PR: https://github.com/digitaldrywood/detent/pull/3362 remains open against `develop`; its body includes `Fixes #3257`.
- Starting local and published PR head: `a694184e469b0d4af4ce4c9430ce237dd013e5f9`. Fetched target: `origin/develop` at `8a94591757c917387c3530454e30d25962ed0448`. Merge preserves both as parents; no rebase or push.
- Prior notes verified against the existing merge history: `a694184e` merges `c41f2476` and `c4455b92`; `c41f2476` merges `2675c370` and `d14138a3`. Migration 48 and its supported schema version remain present. Earlier validation claims concern their recorded heads only.
- Conflict resolution: `internal/hubclient/native_landing_test.go` retains develop's plain-git and approved-GitHub-policy cases alongside the PR's linked-issue intake/landing journey, using separate helper arguments for source linking and landing policy. No new test scenario added.
- `.detent/notes.md` preserves historical handoffs from both sides with one current Workpad/status fence. `static/app/conversation/app.js` is regenerated from the automatically merged TypeScript source to retain both branches' UI changes.
- Open items: Detent owns ownership, cleanliness and ancestry verification, bounded validation, lease-protected publishing and current-head checks. No out-of-scope finding identified. No issue-state changes or PR merge performed.

## Historical workpad for the previous #3257 source merge

Plan: merge the freshly fetched target into the published PR head, resolve only overlapping fixture, generated-asset and handoff-note conflicts, and commit the merge.

Validation: no tests, local gate (including `true`), typecheck, CI checks or waits run in this session. `npm run build` completed successfully solely to regenerate the conflicted JavaScript; warnings concerned generated CSS, component source maps, fonts and chunk sizes. This is not validation credit. Historical evidence below does not establish validation for this head. Quiet-window, gate/CI, slow-check and post-merge timings are unmeasured. Detent owns validation and publishing after return.

Historical status record: status: complete; blockers: []; human_action: null.

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

## Historical workpad for the previous #3257 retry

Plan: preserve the committed prior source resolution, merge freshly fetched develop into the published PR head, resolve the handoff-notes conflict, and commit the merge.

Validation: no tests, asset generation, local gate (including `true`), or CI checks run during this retry. Prior source and migration resolution verified from the existing merge commit and its parents. Prior PR-body diagnostics apply only to their recorded head. Quiet-window, gate/CI, slow-check, and post-merge CI timings are not measured here. Detent owns validation and publishing after return.

Historical status: complete; blockers: []; human_action: null.

## Historical Workpad (incoming develop head)

Plan: Rework repair for #3400 preserves recovered merge 83598d050 and merges develop a53a7f204a278ae2f965560d6c4c10f96eb5b22d into PR #3421's published history. Only notes conflicted; retained historical handoffs from both sides and one current status block. No manual source edits or generated-input changes were required. The remaining issue source diff is internal/cli/boot_test.go and internal/cli/ssh_runner_test.go.

Recovery: inspected all 24 paths in 83598d050; inherited develop changes and its issue-owned notes resolution are retained for publication. No stray artifacts were present. Publication resolves the recovered unpushed commit; no stash or tracker lane writes.

Validation: focused CLI/orchestrator command passed all seven selected regression names at the resolved source (CLI 9.977s; orchestrator 0.612s). Windows/amd64 CLI test cross-compilation passed (10.1s), with output under the provided TMPDIR. Compilation is not native Windows execution. No source changes followed these diagnostics. No full suite, coverage, race suite, Actions rerun, or blocking CI wait ran. Configured `true` must run on the final committed head immediately before publication; exact gate and current-head eligibility evidence are recorded in the canonical issue Workpad.

Open items: next scheduled native Windows validation confirms the portability repair; squash merge and lane transitions remain orchestrator-owned. PR #3421 is already non-draft against develop and references Fixes #3400. No actionable reviews or threads existed at the pre-publication read; current-head checks were absent under repository policy and confer no test credit. No merge-group CI is configured. No new out-of-scope finding or dependency.

Timings: focused diagnostic command 20.5s; CLI Windows cross-compilation 10.1s. No quiet window is configured. The no-op gate takes under 1s; PR CI wait, slow checks, and post-merge main-CI are not applicable to this Rework handoff. Final publication verification belongs in the issue Workpad.

Historical status record: status: complete; completion_work_attempt_id: "7129"; completion_generation: "59"; completion_cleanliness_resolution: committed; blockers: []; human_action: null.

Historical status record for #3400:
Historical status metadata:

schema: 1
status: complete
fields:
  completion_work_attempt_id: "7129"
  completion_generation: "59"
  completion_cleanliness_resolution: committed
blockers: []
human_action: null


# Issue #3440 handoff — Hub sign-in

- Both public sign-in surfaces share the detent.build mark, seven absolute site navigation links, and a 400px card with only Sign in and Create account. Removed invitation token controls and vendor wording from LoginCard; other account screens retain their existing layout.
- Key files: web/conversation/src/app/account/Login.tsx, app/entry/EntryScreens.tsx, app/main.tsx, app/index.css. Public /login renders before bootstrap, including for an already signed-in browser. internal/hubserver/hosted_ui.go now routes /login to the existing application shell instead of the legacy vendor-branded server page; browser verification reproduced that bypass before its repair.
- internal/hubserver/hosted_login.go and internal/cloudentry/login.go forward only screen_hint=sign-up through existing authorization URLs. Organization scoping, state, cookies and PKCE remain intact; invitation email flows pass no sign-up hint.
- Extended existing component and Go tables. The new hint cases fail against original handlers using a scratch Go overlay and pass against the implementation. No invariant or enforcement changes, mechanism, product configuration or CLI additions.
- Passed: 99 account/entry component tests; TypeScript checking; focused Hub shell, shared-entry shell, hosted HTTP, OIDC callback, transaction and invitation diagnostics; vet for internal/hubserver and internal/cloudentry; make generate. Generated app.js and app.css are included.
- Browser: Chrome DevTools verified both public pages and error rendering on isolated ephemeral previews. Linux/arm64 Chromium in mcr.microsoft.com/playwright:v1.61.0-noble passed four focused Playwright cases (6.4s): both sign-in surfaces, actual provider hint/absence, error card, mobile viewport/overflow, authenticated /login, keyboard and accessibility. Four new desktop baselines under hub-login.spec.js were generated and then compared with updates disabled. All pre-existing desktop baselines are unchanged.
- Preview helpers keep fixture output in provided scratch and accept cross-compiled preview test binaries for Linux browser diagnostics. No live process or port-4000 mutation. No full validation gate, coverage, race suite or CI wait; configured gate is true and gives no test credit.
- Rebased onto develop a80b71009 after PR publication. Only shared notes and generated app.js conflicted; preserved incoming handoffs and rebuilt the combined client. Focused Go diagnostics, vet, 99 component tests, TypeScript checking and four strict Linux Playwright cases passed again on the rebased source.
- PR #3455 targets develop and references Fixes #3440. No implementation open items; final gate, publication and current-head review evidence are in the canonical issue Workpad. Orchestrator owns lane transitions and merging.
- Skill draft: no — existing preview guidance covers this routine UI and OIDC change.

# Issue #3408 implementation

- Key files: `internal/hubsecrets/envelope.go`, Hub `project_secrets.go` / `project_sprites.go`, migration 50, Hub serve environment loading and `hub secrets rotate`, account `SpritesCard.tsx`.
- Per-project envelope encryption uses independent AES-256-GCM data keys, row-bound AAD and version-authenticated wrapping. Presence-only metadata; owner/admin writes recheck authority after validation. No token-bearing native command receipts. Offline rotation uses the existing database ownership lock and rolls back all rows/audits on failure.
- Focused crypto, provider validation, role/lifecycle, startup and multi-row rotation rollback diagnostics pass, along with existing schema/migration/member checks. All 80 selected account/policy/Sprites UI tests, frontend typechecking and Go vet pass. `make generate` completed and generated app assets are included. Chrome verified set, replace, remove, input clearing, no value in rendered text and viewer controls, with synthetic provider credentials on ephemeral ports. Desktop and narrow (Chrome minimum 500px) layouts were inspected; isolated previews stopped cleanly. Final publication evidence belongs in the canonical issue Workpad.
- No invariant enforcement change, new operational mechanism, or out-of-scope findings. The issue explicitly authorizes the secret store, settings card and rotation command. Live Detent on port 4000 is untouched.
- Skill draft: no — standard envelope encryption, focused fixtures and existing preview procedures need no new reusable method.

- Rebased onto develop a80b71009. Renumbered this unpublished migration to 50 because develop already landed linked-issue migration 49; no shipped database has used this branch's former migration 49. Retained develop notes and regenerated the conflicted bundle from combined sources. Prior browser evidence covers the unchanged card. Resolved-source focused Go crypto/Hub/CLI diagnostics, schema/migration checks, vet, frontend typechecking and all 80 selected UI tests passed after this rebase. Canonical issue Workpad carries exact pushed-head gate and review/check evidence.


# Issue #3457 implementation handoff

- Key files: internal/store/reports_digest.go joins verified applied lane outcomes to receipt metrics; internal/web/reports_digest.go uses durable per-project shipped totals; reportsv2 view/template explicitly state calendar-day runtime usage, lifetime shipped-cohort receipt coverage, lane dwell and unknown gaps. Releases are unavailable because the runtime has no durable release history and cannot observe external releases. Existing release status elsewhere is unchanged.
- First verified completion per project/issue is counted from the existing ledger, independently of snapshots or mutable receipt refreshes. Successful sessions, historical Done imports, failed writes and abandoned work do not qualify. Configured terminal artifact outcomes are stamped in existing workflow phase metadata; intermediate ready promotions do not qualify. Historical artifact transitions without terminal evidence remain unverified. No table, migration, configuration, CLI, background loop, mechanism or lane behavior is added.
- Regression reproduces the recorded 42-receipt versus 13-visible-card audit (41 programmatic merges and one closed-completed running transition). It failed before the fix with shipped/cohort 13/46. The repaired fixture covers duplicates across dates, snapshot pruning, database reopen, Chicago midnight, both DST calendar boundaries, and the September 23–30 lifetime attribution. Raw recorded lane dwell is recovered; residual lifetime time stays unknown instead of working.
- Focused store, web/template, artifact history and completion transition diagnostics pass. Go vet passes for store/web/templates/orchestrator. Initial added orchestrator test had incorrect gate type names, corrected before its passing run. make generate succeeded (24.4s), with existing CSS/sourcemap/font/chunk warnings; only the expected reportsv2 generated template changed.
- Chrome inspected the real rendered Reports page on an ephemeral isolated server: 42 shipped, receipt coverage, lifetime unknown dwell and unavailable releases; desktop 1440px and mobile 500px had no page overflow. Overlay and screenshots remain under the provided TMPDIR. Browser closed and the preview test exited successfully. The live port-4000 instance was not mutated.
- Publication and exact-head true gate/check/review evidence belong to the canonical issue Workpad. No full gate, coverage, race suite or Actions polling. No out-of-scope findings or dependencies. Orchestrator owns lane transitions and merge.
- Skill draft: no — existing Go debugging and isolated preview guidance covers this repair.
