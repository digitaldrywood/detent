# Repository invariants

These rules record the operator-approved [September 11 design](superpowers/specs/2026-09-11-merge-lane-operator-invariants-design.md)
and September 10 mechanism audit. The audit found repeated manual recovery and
mechanisms interacting to stop progress. Of the last 40 merged PRs, every PR
was force-pushed, 27 received review-bot threads, and each fix or rebase repeated
a 22-minute Verify job; median open-to-merge time was 2.7 hours.

Run `make check` (or `make check-invariants` for the invariant subset). The
existing [runner](../tools/invariantcheck/main.go) executes exact tests listed in
[the manifest](../invariants/policy.json), rejecting missing, skipped, or failed
test evidence. These are repository-local checks, not a server-side trust
boundary. An invariant or enforcement change requires an edit to its entry here
in the same PR, an explanation of the effect, and its ID in “Invariants touched.”
A passing test does not authorize weakening a rule.

## INV-1 — Lane ownership

**Statement:** The orchestrator is the only writer of tracker lane state.

**Why:** Worker lane writes and lane revocation competed with the orchestrator's
state accounting, requiring operator repairs after apparently valid moves.

**Enforcement:** `TestRepositorySources` uses `go/packages` type information to
reject tracker lane method references, including saved method values, outside
`internal/orchestrator/lane_ledger.go` and the enumerated connector adapter files.
The ledger owns both `UpdateIssueState` and state-field `SetIssueField` calls;
adapters implement/forward these operations rather than decide transitions.
Admission requires its injected orchestrator writer, verified by
`TestAdmissionRequiresLedgerWriter`. Worker outcomes belong in Workpad status.
This source check cannot recognize arbitrary raw HTTP tracker writes; review
must reject those bypasses too.

**Change:** Edit INV-1 in this PR before changing the ownership boundary or its
adapter exceptions; do not expand an exception to admit another lane owner.

## INV-2 — Instance-owned infrastructure failures

**Statement:** Infrastructure failures attach to the instance, never to the issue, whether they happen before the first agent turn or during a turn.

**Why:** Backend startup, protocol, and workspace-hook failures previously parked
innocent issues and left the operator to return them to work.

**Enforcement:** `TestPreTurnFailuresDrainInstance` and
`TestObservedLanePreTurnFailureRemainsInstanceOwned` exercise instance attribution
and preserve issue ownership even after an observed lane change.
`TestAttemptAllowanceTriageInfrastructureFailure` applies the same instance-owned
completion handlers to triage workspace, startup, transport, protocol, and capacity
failures. These failures publish no triage comment and do not consume the triage
pass; recovered admission uses the same durable allowance. The shared Codex
capacity classifier also sends typed in-turn transport/JSON-RPC failures through
that existing instance wait; model-parameter rejection and operator cancellation
retain their prior classification. `TestClassifyCapacityErrorTransportAndProtocol`
and `TestRunnerTriageClassifiesProviderFailure` cover the provider/runner boundary.
`TestIssueSpendSinceExcludesInstanceInfrastructureAttempts` excludes established
workspace, backend-startup, deliverable-authorization, forge, and tracker failure
classes from issue progress spend. `TestAgentRunProgressClassifiesPullRequestApprovalDecline`
and `TestApprovalDeniedDeliverableUsesInstanceForgeWait` preserve the connector
approval-denial classification through the existing instance-owned forge wait, while
`TestHandleRunResultReconcilesDeliverableRecoveryExactHead` preserves credential-failure
reconciliation. `TestWorkerCredentialBlockerError` preserves final-message credential
reports as write-path failures.
`TestHumanQuestionRejectsWorkerGitHubCredentialPrerequisite` checks that
credential and API-budget design questions reach durable recording (including write-access
and manual-PR design choices). Explicit support requests remain rejected across polite
auxiliaries and manual-open word order, while actual
access failures and explicit write-enablement/manual-PR requests remain
instance-owned. Question rejection shares the worker access-failure classifier;
credential or authentication terminology alone is not failure evidence (#2616).
The existing `TestCredentialForgeProbeRequiresSuccessfulWrite`
keeps the named project pause active until its write canary succeeds.
`TestCredentialWaitSurvivesOverlappingFailures` preserves credential precedence
when same-host failures overlap. `TestCredentialCanaryRecoversOverlappingHosts`
keeps one project credential canary admissible across overlapping host pauses,
including operator retries; the project remains paused while retained credential
conditions await a successful write canary.
`TestCredentialCanaryReplacementOverlappingHosts` preserves the same single-canary
admission when a condition needs a replacement issue.
`TestCredentialCanaryCrossHostFailureReleasesReservation` releases completed
canary ownership even when an error names another host, including terminal issues.
`TestCredentialCanaryDurableRecovery` preserves
inconclusive probes as durable waits and records successful write proof so restart
recovery cannot resurrect a resolved pause.
`TestCredentialConditionOutlivesOriginatingIssue` separates project health from
issue eligibility, and `TestCredentialClearSchedulesCanaryWithoutUnpausing`
consolidates the operator clear action onto that same write canary.
`TestObservedLaneCredentialCanaryCompletion` preserves instance recovery when
a canary observes a lane transition. When a condition outlives its original
issue, the existing dispatch reservation selects one replacement canary.
`TestRecoverBlockedIssuesFoldsLegacyCredentialParkIntoProjectPause` preserves migrated
credential waits across restart even after they leave the bounded general history window.
These execute through the invariant manifest. Runtime evidence is needed
to classify a new failure correctly; do not infer issue fault merely from a failed attempt.

**Change:** Edit INV-2 and its regression scenarios together in the same PR when
changing the first-turn boundary, in-turn infrastructure classification, or attribution.

Codex preflight scratch cleanup errors take precedence over simultaneous model
selection errors in implementation, validator, and security-audit launches
(#2561). The preceding selection error remains in logs rather than the returned
error chain or message, so typed and textual issue-configuration classification
cannot attribute an infrastructure cleanup failure to an issue.
`TestPreflightCleanupFailurePrecedence` exercises actual cleanup failure and
successful-cleanup controls for all three launch paths. This consolidates their
existing error handling without introducing a new failure class or recovery path.

Pre-dispatch provider capacity selection does not launch agent commands before an
attempt workspace exists (#2561). It shares configured model selection with the
worker and uses the existing provider report for availability. Attempt catalog
validation uses the reservation's advertised model scope, preserving automatic
fallback and legacy override rejection without changing the exact execution
reservation check. Reports advertise canonical backend model names; a stale
report cannot authorize a different runtime model. Catalog and effort validation
still run in the prepared workspace, where startup failures have attempt context.

Provider-identity bookkeeping failures during implementation, validator, and security-audit turns
are logged without failing the turn (#2626). Persistence uses a bounded detached
context and subsequent updates retry through the existing write path.
`TestProviderIdentityFailureDoesNotCancelTurn` covers cancelled and timed-out store
writes followed by successful persistence and normal turn completion.

## INV-3 — Mechanism moratorium

**Statement:** No new brake, breaker, lease, park, revocation, reason code, or reconciliation loop is allowed, unconditionally; any change to one must remove or consolidate an existing one, and the remedy is never a guard.

**Why:** The September 10 audit identified interactions among self-protection
mechanisms as the main source of incidents; adding another conditional guard
perpetuates that failure mode.

Session duration and absolute token guards resolve once per session from the
existing model-selection level (#2597), inheriting omitted limits from the flat
project values. This consolidates limit resolution into the existing selection
and guard paths; it adds no guard or escalation mechanism. Running sessions keep
the resolved limits across turns, checkpoints, and fallbacks; label/configuration
changes do not reset attempts or lifetime usage. Turn inactivity and no-progress
behavior remain unchanged. Covered by `TestModelSelectionSessionLimits`,
`TestRunnerSelectedSessionLimits`, and `TestResumedSelectionKeepsSessionLevel`.

Worker GitHub credential unavailability and connector write-policy denials reuse
the forge-availability pause and write canary (#2548). They do not park an issue or
create a human prerequisite, and the named project condition clears after a
successful write proves recovery.

Lane-entry refresh consolidates overlapping board and retained runtime snapshots
into one observation per issue (#2649). Freshly fetched board issues take
precedence, so stale attempt or pipeline lanes cannot reset the ledger and
replay an operator move on every refresh. This replaces lane-key deduplication
with issue-identity deduplication in the existing refresh; it adds no recovery
path or transition reason. `TestRefreshCurrentLaneEntriesOperatorMoveOnce`
checks three unchanged passes and a later genuine return move. Failed observations
preserve the prior cached entry and provenance without falling back to stale
runtime snapshots; the next refresh retries the board observation.

Allowance triage publication reuses the auto-promote gate against freshly hydrated
PR checks and review threads before moving an exhausted issue (#2685). A ready
head enters the configured promotion lane without publishing the historical
triage explanation; the persisted triage attempt still consumes its single pass.
If the issue still needs triage, the note includes the observed head, check-run
IDs, states, and UTC observation timestamps. Unavailable PR evidence leaves
publication pending. A merge discovered during hydration uses the existing merged-PR
lifecycle; missing or running audit and validator stages leave publication pending
while the existing stage producers run. `TestAttemptAllowanceLiveHead` covers this consolidation;
no new lane reason, allowance reset, or recovery mechanism is introduced.

**Enforcement:** `TestRepositorySources` checks constant lane-transition reasons
against [the existing vocabulary](../internal/invariants/source_policy.json).
Unknown constants, including concatenations, fail. Existing dynamic forwarding
functions have reviewed, formatted-source SHA-256 entries: a new dynamic call
or a changed forwarding function fails until explicitly reviewed. This
conservative check also flags unrelated edits to those functions. It does not
prove the vocabulary of arbitrary upstream data: provider removal messages,
operator reasons, and existing decision helpers remain review boundaries.
Review must verify removal or consolidation; renaming a mechanism or updating
a snapshot is not evidence of compliance.

Updater provenance (#2419) uses the existing candidate verification and startup
recovery paths to require the tested commit before accepting new updates. It
retains the existing rollback and retry limits. Recovery initialization errors
propagate before boot; unreadable or malformed state cannot disable verification.
Legacy updates keep their schema across failed startups and completed rollbacks
so the restored reader retains retry and recovery history. Legacy schema-1 pending updates
without a target commit are accepted when the running version matches the target
(#2618), logging legacy acceptance and using the existing healthy transition to
clear pending state and retire rollback material. Records carrying a target commit
still require an exact running-commit match; schema-2 records without a target
commit fail closed and preserve rollback material. New update writers still require
provenance. Successful target startup or a new provenanced update
migrates that state, consolidating compatibility
at the existing state writer and health transition without adding a recovery path. Recovery verifies the recorded
installation before accepting the previous build, and rejects unrelated restarted
versions without changing pending or failure records. This consolidates prior-build
acceptance into the existing binary identity verifier. The existing verification mechanism
must remain to reject unverified artifacts before replacement.
Startup, serving, and readiness workers share cancellation and are joined before
identity-failure exits allow caller-owned resources to be cleaned up.

GitHub dependency hydration uses native relations as the source of truth when
available, removing the union with prose dependencies (#2575). Unsupported
repositories fall back to issue-body lines; historical comments are diagnostic
only. Public reads select the dependency source before resolving blockers, and
tracker refreshes replace authoritative empty lists instead of restoring prior
refs. This also removes the orchestrator's historical-comment union and prevents
reparsing connector-owned dependencies. Degraded native reads propagate errors
instead of certifying prose fallback or suppressing subsequent native reads.
Ignored prose and structured workpad dependency predicates absent from the native
list are explained without introducing another blocking mechanism. Native-backed
dispatch uses the current relation list instead of the terminal attempt's saved
dependency-deferral metadata; past attempts cannot restore a removed relation.
Removed structured dependencies remain cleared evidence for the existing blocked
recovery transition. Legacy workpad dependency sections and phrases are not
reinterpreted as human actions on native repositories; explicit human-action
sections and non-dependency predicates retain their existing meaning.

Validator launch accounting uses the existing validator-run registry and shared
worker progress publisher. Validators appear alongside implementation workers in
runtime snapshots, including during startup and completion, and leave the registry
on exit. This consolidates observability with existing lifecycle ownership; it adds
no recovery path or orphan-suppression guard (#2505).

Git workspace cleanup removes the durable `preserve` latch and the residual
sweep's registration-only exemption (#2612). Recorded and discovered worktrees
share the existing checks for ownership, active issues/processes, uncommitted
files, and commits absent from verified live remote heads. Stale tracking refs
from deleted or force-pushed branches do not establish publication; unavailable
remotes and unfetched tips retain work until it can be verified. Every running
worker remains active through finalization, including terminal lane updates.
Legacy `preserve` JSON fields no
longer exempt a workspace; checkpoint journals and filesystem retention remain.
`TestLocalGitReconcileRechecksPreservation` covers restart, legacy records,
published and merged work, and continued retention of unsafe candidates. No
merge inference, expiration mechanism, or configuration is added. Expected
retention keeps path evidence without failing the sweep, so the existing sweep
interval advances even when local work remains.
`TestCleanupVerifiesLiveRemoteCommits` covers stale refs and remote failures in
residual, direct, and branch cleanup;
`TestResidualCleanupProtectsFinalizingTerminalWorkers` covers terminal worker
ownership until its completion event.

**Change:** Edit INV-3 in the same PR with the removed/consolidated mechanism and
why the final change complies. Review reason sources before changing the
allowlist or a dynamic-function digest; never refresh these blindly to pass CI.

Issue #2567 explicitly authorizes the read-only `lane_signal_ignored` explanation
reason as a narrow exception to the reason-code moratorium. It reports ignored
tracker inputs through the existing explanation, board health, and doctor
surfaces. It does not enter the lane-transition vocabulary or affect dispatch,
recovery, or tracker writes; the lane-transition allowlist is unchanged. This
exception does not authorize any other reason code or lane mechanism.

Lifetime-limit parks remove the cooldown timer, timed recovery transition, and
signature permit (#2486). The vocabulary consolidates onto the existing
`lifetime_limit_recovered` reason, emitted only when an override is present or
the configured limits no longer apply. Elapsed time cannot release the park.

Todo dispatch resolves non-terminal dependency refs through the existing dependency
auto-unblock resolver and readiness predicate, sharing blocker reads within each
dispatch refresh (#2509). Closed tracker state releases ordinary dependencies
without requiring a terminal lane observation; human completion evidence remains
required. This consolidates dependency readiness without adding a recovery loop.

Issue #2595 consolidates `rework_limit`, `no_progress_limit`, and dispatch-loop
attempt accounting into one fixed allowance: three code/rework sessions started
since the last merged PR or operator move out of Human Review (#2692). The
durable attempt log owns the count, with the window derived from existing lane
history. Human-origin moves, and moves not initiated by the Detent instance,
reset the window for any destination; Detent-instance moves do not. New commits,
new PR heads, CI signatures, ordinary lane changes, and acknowledgements alone
do not reset it. The existing triage comment receives a timestamped reset line
when comment updates are supported; publication failure does not undo the move. Instance-attributed startup, transport, workspace and restart failures
are excluded. Immutable PR merge times and merge observations in the durable lane timeline
reset prior work; subsequent activity on a merged PR is not a reset.

The fourth code dispatch becomes one read-only triage turn using the existing
code/rework role. Its durable attempt identity prevents another triage turn after
restart, excluding instance-attributed failures. Triage uses the shared native
reservation-aware selection and execution-start contract, plus metered budget
admission and in-turn projection enforcement. The runner has no mutation tools,
no resume state, no writable worktree,
and no workspace or publication hooks. It returns one fixed-format explanation;
the orchestrator persists that result, publishes one issue comment, and moves the
issue to Human Review with `attempt_allowance_exhausted`. Publication checks the
existing attempt marker before retrying; interrupted triage yields an explicit
incomplete-diagnosis note, never another worker turn. Automatic promotion also
honors exhaustion and existing running-worker ownership. Triage retains an
operator lane change observed at completion, including across publication retries.
Historical reason strings and progress records remain readable.
`TestImplementProgressBlockComment` preserves historical clean-workspace and
fingerprint-only evidence formatting without restoring dispatch-loop producers.
Triage resolves the same selected session duration and token limits as workers,
while retaining its single-turn, two-minute upper bound.
`TestRunnerSelectedSessionLimits` covers inherited and level-specific limits,
duration expiry, token usage, and configuration changes during triage.

This is the issue-authorized replacement reason and consolidation, not an
additional breaker, configuration key, or recovery loop. Non-PR artifact and
explicit operational completion workflows retain their own deliverable rules.
`TestAttemptAllowanceCountsIssueJourney`, `TestAttemptAllowanceDispatchAndRestart`,
`TestAttemptAllowanceTriagePublication`, `TestAttemptAllowanceNoteFormat`, and
`TestRunnerTriageIsReadOnly`, `TestAttemptAllowanceMergeTimeAndRunningOwnership`,
`TestAttemptAllowancePreservesOperatorCompletionLane`,
`TestAttemptAllowanceOperatorMove`, `TestAllowanceOperatorMoveBoundary`,
`TestAllowanceResetAnnotation`, and
`TestPullRequestMergeTimeSurvivesLaterActivity` enforce the allowance and restricted publication
contract. `TestRunnerTriageNativeAdmission` and `TestRunnerTriageBudget` verify
reservation identity, capacity loss, issue/daily budget refusal, and metered
projection enforcement without launching an unauthorized backend turn.
`TestRunnerTriageReportsTurnStart` preserves observed primary-turn evidence before
usage arrives; `TestAttemptAllowanceTriageFallbackCompletion` keeps budget refusal,
invalid output, tool refusal, and interrupted turns to one fallback comment.

Scheduled routine occurrences advance from their durable `scheduled_for` identity
regardless of success or failure. A selected occurrence whose existing ownership
context is already canceled is consumed without starting an agent. This removes the
implicit same-slot retry and relies on the existing schedule-ownership context rather
than adding a retry, backoff, or lease mechanism (#2526).

Backlog admission selection uses the existing run issue records for evaluation
identity, fingerprints, and stale verdicts. Unchanged stale candidates join the
existing epic and malformed-result exclusions before the window is capped;
remaining candidates rotate by last evaluation time. This replaces the fixed
evaluation window with selection from run/skip bookkeeping, without a
new routine, reason code, or suppression table (#2568).
Saved stale verdicts and new evaluations share the same point-lookup revalidation;
a candidate returning to its original eligible snapshot can re-enter the window.
This removes unconditional historical suppression across eligibility cycles.

Deliverable recovery opens a draft pull request when a worker already pushed an
exact-head branch but failed before creating the PR, and returns a missing remote
branch to Rework (#2528). This retires the human-owned no-PR park by consolidating
the repair into the existing deliverable-recovery and operator-routine paths.

Deliverable-park retirement transitions also acknowledge that retired park during
durable timeline replay (#2603), including the merged-PR failed-CI transition
from Blocked to Rework. This consolidates automatic retirement with the
existing park acknowledgement path, so a return to Rework cannot resurrect the
retired block. Independent human parks and later parks remain protected. Covered
by `TestDeliverableRecoveryRetirementAcknowledgement`.

Successful persisted attempts release dispatch ownership through the same completed
attempt cleanup even when completion-time pull-request hydration is unavailable
(#2553). Completion evidence and the existing hydration gate remain authoritative;
the claim no longer acts as an implicit park while no worker is running, and cleanup
is limited to the retained attempt claim so it cannot erase replacement ownership.

GitHub REST reserve and lookup decisions use credential-and-endpoint-family
snapshots instead of the last response labeled `core`. Ambiguous orchestrator
aggregate telemetry no longer creates the fleet-wide REST capacity outage;
explicit provider backoffs and worker/shared-pool reserve evidence retain their
existing handling (#2547). This consolidates the blanket dispatch pause into the
request-family gates that own the observed budget window.

Darwin process-environment inspection uses the existing workspace scan stage
budget instead of a separate per-process transient-read deadline (#2573).
Reconciliation and reaping both receive that stage budget; EINVAL/EIO retries do
not renew it, and expiration preserves the ownership error and verified partial
results. This removes a competing timer without adding a recovery mechanism.

Malformed Workpad blocker refs remain verbatim diagnostics, but do not reject a
clean, green PR (#2640). Auto-promote evaluates valid blockers and the existing
PR gate, including affected Rework cards, without another worker session.
Other schema errors retain their routing behavior. The default Kanban policy
allows operator Rework to Merging transitions; explicit project overrides remain
authoritative. This consolidates promotion under INV-3 without a new mechanism.

Structured Workpad status parsing ignores unknown YAML keys and records their paths
on the signal (#2604), removing strict field rejection from the existing parser.
Known predicate validation and completion authorization remain unchanged; no new
reason code, gate, or recovery mechanism is introduced.

Admission candidate scans consolidate REST label, repository, and issue-field
pagination into one reader, retaining page/item continuation in the existing
admission run ledger (#2574). Completed hydrated candidates remain usable when
the existing fanout budget ends a read; no retry routine, reason, or configuration
is added. Exhausting a source resets its continuation for the next scan.
If that budget also prevents a saved stale snapshot's early recheck, its
eligibility check consolidates into the mandatory final revalidation; other
completed candidates can still use the evaluation window.

Security audit verdict routing from Merging shares the existing auto-promote
consumer with source-lane completion (#2642). The merge completion handler no
longer independently fails or routes audit outcomes. Findings publish to the PR
before the existing lane writer routes to Rework; the trusted run marker prevents
repeat publication after a failed lane write. Comments remain explanatory, never
verdict evidence. Required-gate telemetry uses the same audit classifier.
`security_audit_wait`, explicitly requested by #2642, distinguishes an active run
from missing evidence in wait telemetry; it is not a new lane-transition reason
or recovery path. `TestMergingSecurityAuditVerdict` and
`TestSecurityAuditPublicationRetry` cover the shared routing and publication.

Native queue review failures reuse the auto-promote Rework transition, comment,
and prior-attempt handoff (#2643). Hydrated unresolved threads and GitHub's
conversation-resolution enqueue rejection use `unresolved_review_threads`;
transient enqueue failures retain their existing handling. This consolidates
review routing without adding a reason or retry mechanism.

## INV-4 — Native merge queue

**Statement:** Merges go through the repository's merge queue when one exists.

**Why:** Competing speculative merge work and repeated head invalidations
contributed to the measured rebase and CI loop.

**Enforcement:** `TestDelegateNativeMergeQueueIssuesEnqueuesGreenTrainWithoutWorkerDispatch`
exercises native queue delegation. `TestRepositoryWorkflow` requires
`merge_group: checks_requested` in this repository. Other repositories retain
their chosen settings; Detent detects the queue and uses its serialized merge
worker where no queue exists. These tests do not inspect live GitHub settings.

`detent doctor` reports runtime evidence: with a queue present, any
`merge_worker_programmatic_merge` lane event or `programmatic_merge_failed`
attempt in the last 24 hours fails the check. The store records no queue
activation time, so a merge from before a queue was enabled can fail the
check until it ages out of that day; a second queue removal routing to Human
Review is the budget, not a bypass.

Queue removal is consumed once by the existing removal accounting (#2621).
The first removal clears ended queue ownership and leaves the issue in Merging;
the next pass uses normal admission for an eligible PR. Repeated observations
of that removal do not route clean branches to Rework or spend another attempt.
The second distinct removal retains the Human Review budget outcome. Removal
accounting is persisted in the existing workflow timeline before retry admission
and restored after restart; the budget lane transition resets the accounting.
`TestNativeMergeQueueBudgetSurvivesRestart` exercises SQLite close/reopen between
attempts, and `TestNativeMergeQueueRemovalStorageFailure` verifies that unavailable
accounting cannot authorize admission.
`TestNativeMergeQueueRepeatedRemovalRetries` and `TestNativeMergeQueueAttemptBudget`
cover this consolidation of the Rework detour into existing queue admission.

**Change:** Edit INV-4 and the queue delegation tests in the same PR before
changing the ownership or fallback behavior.

## INV-5 — CI once per ready head

**Statement:** Real CI never runs on pull_request events. Pull requests carry only instant placeholder checks so the merge queue can accept them; the merge group runs the full suite once per batch and main runs the integration jobs after merge.

**Why:** Every reviewed PR was force-pushed and each fix/rebase repeated the long
Verify job; draft iteration avoids paying this cost before local review ends.

**Enforcement:** `TestRepositoryWorkflow` parses this repository's CI YAML,
requires the PR activity allowlist and draft exclusion on every PR job (including
the Invariant Gate), and restricts portability, Windows core, installer, and
snapshot jobs to main push or explicit manual dispatch. The manual dispatch is
a deliberate operator exception, not an automatic PR/merge-group trigger.
The worker convention is to finish local validation/review before marking ready;
Rework can produce a new ready head. Workflow assertions cannot deduplicate
manual reruns or repeated ready/reopened events. Other projects may opt into
label gating or their own CI convention.

CI push triggers are restricted to main: release tags run the release workflow
without creating newer mandatory check IDs that invalidate their own provenance
(#2419). `TestCoordinatorTagToSigningProvenance` exercises coordinator annotation,
the repository's workflow triggers, and the signing input gate, including retries
and rejection of missing, stale, cancelled, skipped, or failed evidence.

`detent doctor` also reads every workflow under the project's source root and
warns when a job runs on pull_request events: a job is exempt only when its
`if` contains `github.event_name != 'pull_request'`, is exactly the placeholder
equality, or is made of `||` alternatives that each require some other
`github.event_name`. Projects may keep per-push CI; the verdict is a warning.

**Change:** Edit INV-5 and workflow assertions in the same PR when changing these
triggers, draft handling, or the documented manual-run exception.

## INV-6 — Isolated Codex home

**Statement:** Workers run with an isolated Codex home; user-level instructions never reach a worker.

**Why:** Host-level instructions introduced competing worker prerequisites and
operator interventions unrelated to the assigned repository task.

**Enforcement:** `TestPrepareCodexCommandForServiceIsolatesInstructions` and
`TestPrepareWorkerCodexHomeExistingInstructions` exercise service profile
isolation, per-worker SQLite state, and rejection of inherited instructions. The
manifest runs both.
Repository instructions and the Detent-provided worktree remain authoritative.

**Change:** Edit INV-6 and isolation tests together in the same PR before changing
home construction or instruction inheritance.

## INV-7 — Machine issue identity

**Statement:** Machine-filed issues carry an origin block and a fingerprint, and duplicates comment instead of creating another issue.

**Why:** Repeated repairs and machine discoveries otherwise create duplicate
work and obscure whether an issue came from an operator or automation.

**Enforcement:** `TestMachineIssueTool` exercises the worker tool and intake
contract through the manifest; `TestMachineIssueDuplicate`,
`TestMachineIssueSeparateConnectors`, and `TestMachineOriginSurvivesBodyUpdates`
exercise duplicate commenting, concurrent publishers, and durable origin stamping. Use `file_machine_issue`, with a stable problem key,
for worker discoveries. Review must ensure a fingerprint describes the problem
rather than a timestamp, attempt, or wording variation.

**Change:** Edit INV-7 and origin/deduplication scenarios in the same PR before
changing identity format or duplicate handling.

## INV-8 — No strict freshness protection

**Statement:** No strict up-to-date branch protection is allowed on branches Detent merges into.

**Why:** Strict freshness invalidated already-tested heads after other merges
and fed the measured repeated rebase/CI loop.

**Enforcement:** The existing merge-queue doctor recommendation reads protection
and measured history (`TestDoctorMergeQueueRecordedHistory`). Repository tests
cannot guarantee live branch settings, and this PR does not change them or add
a live doctor check per ID. The operator must inspect configured merged-into
branches; do not claim this rule is universally enforced by local CI.

**Change:** Edit INV-8 in the same PR with the intended protection semantics and
live verification plan; code changes never imply permission to edit settings.

## INV-9 — Retired mechanisms

**Statement:** Retired lane revocation, indeterminate-lane stops, per-issue infrastructure parking, and root-level `rateLimit` in mutation documents stay retired.

**Why:** The audit removed these paths because they stopped healthy progress or
caused tracker protocol failures that the operator then had to repair.

**Enforcement:** `TestRepositorySources` rejects retired identifiers and constant
strings, including assembled constants; it tokenizes GraphQL strings to reject
`rateLimit` in mutation documents, including aliases and fragments. Obtain budget
information with a separate query. Comments and string-valued GraphQL arguments
are ignored. INV-2's behavioral tests cover infrastructure attribution. Negative
fixtures demonstrate forbidden behavior fails. Documentation and test fixtures
may name retired mechanisms; runtime code may not restore them. Novel names,
reflection, generated runtime documents, and semantic equivalents require review.

**Change:** Edit INV-9 and the corresponding negative/behavioral tests in the same
PR before changing retired-symbol or mutation rules; removal from a list alone
is not an authorized resurrection.

## INV-10 — Priority only picks the next job

**Statement:** Priority picks the next job. Nothing else.

- If a task can be started, start one. Always. Free capacity is never held,
  reserved, or kept idle for any project, lane, or priority.
- When a slot frees, dispatch the highest-ranked READY request; if none of the
  higher-ranked projects has anything ready, dispatch whatever is ready.
- The system never cancels, stops, or preempts running work. Only a user cancels work.

**Why:** Priority cancellation discarded running attempts, while selected-slot
reservations refused ready work even with real global capacity available.

**Enforcement:** The global gate queues eligible requests with a consuming event
loop and ranks them together with new acquisitions. Submission, release, and
capacity changes use the same real-slot acquisition operation. Pending requests
own no capacity; project, lane, and host ceilings apply atomically to grants.
Queued host assignment uses current real grants, including grants not yet
consumed by an owner. Eligible hosts remain alternatives until acquisition;
retry host preference cannot strand capacity on another available host.
Owners consume grants in acquisition order and revalidate current candidates;
refresh and shutdown discard pending requests and release unused grants.
All modes use the same lifecycle; strict priority is
only an ordering rule. Authorization, dependencies, retry readiness, and local
lane ceilings are checked by the callers before acquisition. Admission reads
and filters candidates before acquiring local or global capacity for evaluation.
`TestGlobalDispatchGatePriorityOnlyPicksNextJob`,
`TestGlobalDispatchGateReadyRequests`, `TestGlobalDispatchGateConcurrentReadyRequests`,
`TestQueuedDispatchRanksIndependentRequests`, `TestQueuedDispatchPreservesProjectCeilings`,
`TestRunDispatchesQueuedRequestsWithoutPolling`,
`TestRunDispatchesQueuedRequestsAcrossHostsWithoutPolling`,
`TestQueuedDispatchChoosesAvailableHost`, and
`TestAdmissionWithoutEligibleCandidatesAcquiresNoCapacity` run through the
manifest. The existing boundary fuzz seeds retain free-capacity and release
failure coverage. `TestRepositorySources` rejects retired selection, rescue,
reservation, and preemption symbols and reason strings, including assembled
constants. Only the two enumerated historical store readers and the dedicated
configuration compatibility reader may retain old reason strings; negative
fixtures cover that boundary. Previously valid staleness benign-reason overrides
remain accepted on startup and reload, but retired reasons are neither emitted
nor included in defaults.

This change also enforces INV-3 by deleting preemption callbacks, speculative
selection, reservation weights, lane rescue counters, and the separate strict
lifecycle. Elastic pools also drop reclaim targets and borrower queues that
held shared capacity for callers no longer acquiring. Registry dispatch ranks
executable requests across all active pools before acquiring shared capacity
(#2571), using the same request selector as standalone gates. A request that
cannot fit does not exclude another pool; release dispatches the next request
without polling. `TestPoolRegistryRanksBorrowersAcrossPools`,
`TestPoolRegistryQueuedBorrowersUseFreedCapacity`, and the idle-lender and
prior-refusal regressions run through the invariant manifest. Pool and burst ceilings
still apply. Queued acquisitions consolidate overlapping-call ordering and
retained demand into executable requests; they introduce no selected-slot
reservation, configuration, or recovery mechanism.

Merge scheduling serializes only running merges against the same repository/base
(#2572). CI waits, retries, claims without running work, and unready heads do not
exclude ready competitors. Aged heads retain ordering preference when ready.
`TestMergeIdleHeadDoesNotReserveSlot` checks planner dispatch, fresh candidates,
and native queue admission, including independent base branches. The existing
`merge_ci_reservation` reason now denotes only a running merge; its detail names
the running issue. `merge_fairness_head_reserved` is retired from producers and
covered by the source scanner; historical configuration remains readable.
CI wait metadata remains compatible on disk but is keyed by issue in memory so
concurrent waits preserve separate deadlines and refresh state across restart
(`TestMergeWaitMetadataRemainsIndependent`). This enforces INV-3 by removing the
fairness exclusion and idle repository ownership, without adding a mechanism.
Released idle wait metadata is removed from the active map during reconciliation;
release reasons remain diagnostic and historical. Redispatch therefore starts
with a fresh CI deadline and no stale refresh marker, while active waits retain
their deadlines (`TestMergeReleasedWaitStartsFreshOnRedispatch`). This removes
retained released state instead of adding separate admission exclusions.
Running workers retain their original metadata through completion; the same
idle reconciliation then handles its release.

Rework candidates reuse the merge worker's current-head CI status and pending-check
view before acquiring capacity (#2639, operator-approved). Queued or running CI
returns the existing `current_head_ci_wait` decision; the lane and retry attempt
remain unchanged, with no new timer or reservation. Terminal CI or no PR retains
normal eligibility. `TestReworkCurrentHeadCIDispatch` covers fresh and retry
candidates, immediate dispatch of the next eligible candidate, and release after
terminal CI. `TestReworkCurrentHeadCIConfiguredLane` preserves configured lane
selection. This consolidates CI classification with the merge worker (INV-3).

**Change:** Edit INV-10 and its tests in the same PR before changing priority or
capacity semantics. New names or indirect equivalents remain a review boundary.

## INV-11 — Human scope approval before Todo

**Statement:** No feature and no new or expanded operational mechanism enters
Todo without a human's explicit approval of that scope, regardless of who filed
the issue or how its title is typed. Only a fix with recorded runtime evidence
whose remedy removes or consolidates may reach Todo without a human.

**Why:** The operator's September 14, 2026 audit of 253 PRs merged between
August 31 and September 14 found 225 PRs (183,808 added lines) from the untagged
operator/assistant admission channel. Of those, 45 were features (94,884 lines);
many “fixes” expanded mechanisms, including outage backoff, durable operator
retries, review-thread gating, artifact breakers, credential pauses, and merge
reservations. Worker-filed issues produced 13 PRs and 1,909 lines, none in
orchestrator code. Non-test orchestrator code grew from 23,828 lines at v0.44.0
to 51,369 at v0.114.8. The growth came through unapproved scope, not machine
self-filing.

**Enforcement:** Admission Criteria rule 9 in the operator-managed
`detent-orchestration/WORKFLOW.md` leaves features and new or expanded mechanisms
in Backlog until a human approves their scope by moving them. Assistants file
that work to Backlog only. The rule applies regardless of title type or author;
only evidenced fixes that remove or consolidate qualify for automatic admission.
That workflow is outside this repository and was updated by the operator.

The doctor check `INV-11 human scope approval` inspects every applied Todo
ledger entry in the last seven days, including repeat entries. It fetches current
issue titles and bodies by ID in batches of at most 100 so GitHub's identity
lookup can audit busy projects. Conventional `feat`, `perf`, and `refactor` titles
and declarations in either titles or bodies adding or expanding config keys,
reason codes, brakes, breakers, leases, parks, recovery paths, or reservations
require a human move. Each
violation reports the issue, ledger origin, reason, and timestamp. Declaration
clauses are separated at punctuation and coordinating words so removal or
negation of one mechanism does not hide a later addition in the same sentence.
Human ledger origin is accepted; an operator move with an empty or operator ledger origin must
have matching phase-event provenance naming a human initiator and either an
authenticated human session or a human actor. Dashboard sessions record their
authentication basis without a tracker login. The
event must match the project, issue, source/target lanes, reason, and write time
interval. Admission acceptance and explicit machine origins cannot borrow a human
actor's identity. Failed and prepared writes are not Todo entries.

`TestDoctorInvariantAdmission`, `TestDoctorInvariantAdmissionBatches`, and
`TestDoctorInvariantScopeClassification` run through the invariant manifest. `TestDoctorInvariantRegistration` in
`internal/invariants` preserves the doctor entry point and check name. Missing
store/schema/tracker/issue evidence warns rather than passing silently. This is
a diagnostic, not an admission gate: body classification is a conservative
natural-language heuristic, current issue text can differ from admission-time
scope, and historical tracker moves absent from the ledger cannot be audited.
It does not prove that every ordinary fix contains sufficient runtime evidence;
admission rule 9 and review enforce that broader requirement.

**Change:** Editing INV-11 requires updating the doctor check and the admission
criteria in the same PR. Identify INV-11 in the PR template and record the
operator-managed workflow update when it lives outside the repository.

## Check boundaries

The source walk covers non-test Go packages under `internal`, `cmd`, and `tools`
for the build platform running the gate. CI's Linux, macOS, and Windows test
jobs cover their selected files. It deliberately leaves docs and negative test
fixtures available to explain violations. Source vocabulary and workflow checks
complement behavioral tests; they do not prove every natural-language rule,
protect themselves against edits, or enforce live settings. See the
[runner contract](../invariants/README.md) for the same limitations.
