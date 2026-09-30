# Issue #2976 handoff

- PR: https://github.com/digitaldrywood/detent/pull/3052; ready, targets `develop`, includes `Fixes #2976`.
- Recovered both issue commits and rebased without conflicts onto `origin/develop` at `d14138a37c7597542c30e19616271a971b62fc8f`. All recovered source changes belong to this issue; no stray files remain.
- Key files: `internal/orchestrator/autopromote_tick.go`, `internal/orchestrator/autopromote_tick_test.go`, and INV-3 in `docs/invariants.md`.
- Ready In Progress PRs reuse the existing Rework transition for unresolved review threads or failing CI. Drafts and unfinished clean PRs remain unaffected. Configured source/pass/rework lanes retain normal behavior, and final running attempts can take the repair route. No new mechanism or reason is introduced.
- Both existing automated review threads are resolved. No newer actionable feedback was present before publication.
- Current diagnostic attempt: `go test ./internal/orchestrator/... -run '^(TestAutoPromote|TestTickAutoPromote|TestApplyAutoPromote)' -count=1` failed before test execution, and `go vet ./internal/orchestrator/...` failed during compilation. Both report `internal/runner/ssh_protocol.go:210:85: undefined: ErrSessionNoProgress`, also present on develop. Reused fingerprint `runner-ssh-sentinels-retired-session-no-progress` to attach this occurrence to existing #3427. No unrelated source fix was included.
- Earlier full focused-package test/vet results apply to the previous rebased head only. No test pass is claimed on the current head.
- Current protocol sets `gate.run` to `true`; blocking local gates and local status publication are disabled. Run it once immediately before pushing; it provides no test evidence. Prior `make check-fast`/`local-gate` instructions are superseded.
- Remaining publication steps: push with a lease on the observed PR head `7910444fa5fd3f5e8b1052d819379119061c6bde`, update the PR validation description, inspect current-head reviews/checks once, and complete the canonical issue Workpad. The orchestrator owns lane state and later merge processing.
- Skill draft: no — no new reusable procedure was needed.
