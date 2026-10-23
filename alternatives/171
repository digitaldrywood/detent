# Startup worker accounting (#2505)

Read-only inspection of the local instance history identified session 6059 as a
validator, not a resumed implementation worker:

- `codex_sessions`: id 6059, project detent, identifier digitaldrywood/detent#2486,
  started_at 2026-09-11T19:54:12Z, work_attempt_id NULL, worker_pid 80484.
- `worker_session_started`: 2026-09-11T14:54:39.882457-05:00,
  detent_session_id 6059, issue_state Rework, role validator.
- `worker_check_started`: 2026-09-11T14:54:39.994634-05:00, same session and role.

Normal dispatch already publishes Running before launching its worker. Validator
launch instead recorded only an identity in validatorRuns and omitted the existing
ValidatorRequest.OnUsageUpdate callback. Health derives agent_memory and recognized
live sessions from snapshot.Running, explaining both reported symptoms.

The fix extends that existing registry with shared workerProgress and includes it
in observable state. It does not add a recovery path, change scheduler ownership,
or suppress orphan detection. Separate keys preserve simultaneous code and
validator sessions for the same issue. Completion snapshots read current validator
progress instead of retaining a finished validator in their cached state.

Regression evidence:

- TestValidatorRuntimeObservable failed before the fix with "validator launched
  without runtime usage registration" in both success and failure cases.
- The fixed test checks usage, memory, concurrent code/validator visibility,
  completion-time visibility, and validator removal on success/error.
- TestStartupSessionsAppearInStateAndHealth seeds an active work attempt with a
  still-running Codex session from a dead prior worker. It covers ordinary orphan
  resume and first-tick validator launch for Rework, then asserts Running/session
  identity, agent_memory, and zero orphan processes through the real HTTP handlers.
  Process observations are injected; no live process is spawned or signaled.
- Existing startup and completion observability regressions pass, including
  TestCompletionSnapshotFreezesWorkerProgress: dispatch progress and persisted
  heartbeat enrichment remain frozen while validator observations stay current.
- Independent validator review: pass, score 0.97, no P1/P2 findings.

Final validation: `make check-fast` passed (invariants, migrations, generated-code
checks, build, lint, vet, and all package tests). Race/coverage/nilaway are deferred
to the merge queue as configured.
