# Repository invariants

These rules record the operator-approved [September 11 design](superpowers/specs/2026-09-11-merge-lane-operator-invariants-design.md)
and September 10 mechanism audit. The audit found repeated manual recovery and
mechanisms interacting to stop progress. Of the last 40 merged PRs, every PR
was force-pushed, 27 received review-bot threads, and each fix or rebase repeated
a 22-minute Verify job; median open-to-merge time was 2.7 hours.

Follow the [validation rule](../AGENTS.md#validation); `make check-invariants`
runs only the invariant subset during focused iteration. The existing
[runner](../tools/invariantcheck/main.go) executes exact tests listed in
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
Generated onboarding workflows and refresh proposals reinforce INV-1 by replacing
worker lane-transition commands with orchestrator ownership and a reference to
the appended handoff contract; workers report outcomes rather than move lanes.
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
Compound push commands whose follow-up GitHub CLI read cannot log in reuse the
instance token-resolution wait (`worker_github_cli_auth` in the diagnostic), not
the project forge outage. `TestCompoundPushCLIAuth` preserves this distinction
from rejected Git writes. `TestCompoundPushCLIAuthCompletion` verifies that
these instance waits survive restart using their persisted retry deadline, including
when a tracker lane change is observed before worker completion. Diagnostic
resolution timeouts are not required to restore a wait.
`TestCredentialCanaryExcludesMergeWorker` keeps merge
workers out of credential write-canary admission; retained merge retries do not
prevent a write-capable replacement. Configured forge host provenance takes
precedence over unrelated URLs in command output (#2731).
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

Dispatch Workpad comment-read failures use the existing tracker availability observer
and tracker-unavailable dispatch reason; they never become issue dependency evidence.

Worker GitHub CLI preflight (#2741) checks that `gh auth token` can read the
selected credential from its private per-attempt `hosts.yml`. Failures reuse
`WorkerGitHubBudgetMonitorError` and its existing instance attribution; no issue
question or lane writer is added. `TestWorkerGitHubCLIAuthenticationPreflight`
checks missing credentials and secret-free diagnostics, including credentials
inherited from the top-level `github_token` (#2794). Startup, reload, and doctor
share the same worker default, preserving project overrides; doctor reports
overrides that resolve empty as an instance credential problem.
`TestProjectHotReloadAppliesRuntimeGitHubTokenBeforeValidation` and
`TestDoctorWorkerGitHubCredentialResolution` cover these paths, and
`TestWorkerGitHubCLIAuthStatus` verifies authentication with token environment
variables absent against an isolated local HTTP fixture.

Scheme-prefixed Workpad blocker refs (#2748) retain their reference and text as
instance-owned, unverifiable evidence. Successful workers reporting them release
their claim without issue no-progress strikes or lane changes. Existing live
Workpad evaluation retains the promotion hold until the report clears;
instance-owned reports do not veto issue dispatch (#2802). Infrastructure eligibility
belongs to the existing instance controls, and the next eligible worker can
re-verify the report. `TestRecordedBlockerDispatchOwnership` covers fresh and retry
dispatch, mixed human-action holds, and preserved scheduler wait detail. Only
explicit human actions in structured blocked Workpads share evaluated evidence
with the existing Needs-you question projection, including PR-less cards and recorded age;
`TestWorkpadHumanActionSnapshot` and `TestOperationsWorkpadHumanAction` cover that path.
Legacy blocker prose does not become a human question or hide dependency decisions.
No timer, recovery path, or reason code is added.
`TestSymbolicBlockerCompletion` and `TestSymbolicBlockerPromotion` cover attribution,
retained diagnostic detail, final usage/diff accounting, CI scheduling after a pushed head,
and promotion after clearance. This replaces symbolic
ref rejection and Rework routing without adding a park, timer, or recovery loop.

## INV-3 — Mechanism moratorium

Rework dispatch (#2800) reads the gate's live `AutomatedReviewPending()`
predicate for clean, green PRs without actionable threads or findings. The existing
`awaiting_gate` decision no longer requires retained completion evidence for this
case, so question waits cannot cause repeated sessions while a review is pending.
`TestReworkLiveReviewGateDispatch` replays post-answer scheduler passes and checks
that a current-head review or actionable PR state preserves dispatch eligibility.

Question closure resolution (#2793) uses the existing transition refresh and
answer columns. Durable unanswered issue IDs join that refresh so questions
left behind across restart resolve when the tracker reports closure or a terminal
lane. No separate reconciliation loop is introduced. Closure markers never become
authorized human replies; reopening requires a new question key.
`TestRefreshResolvesTerminalHumanQuestions` covers terminal states, reopening,
tracker failure, and the distinction between closure and human authorization.

Human-owned Workpad blockers route through the existing completion Blocked
transition on the first report (#2779). The repeated-report threshold is removed:
live blocker evaluation already suppresses the next dispatch, so a second
completion cannot be required. `TestFirstHumanBlockerCompletionReachesBlocked`
replays that conflict with and without a PR and preserves human-owned recovery.
Question waits retain their current lane; automated Blocked transitions do not
renew the attempt allowance.


**Statement:** No new brake, breaker, lease, park, revocation, reason code, or reconciliation loop is allowed, unconditionally; any change to one must remove or consolidate an existing one, and the remedy is never a guard.

GitHub refresh pacing (#2763) replaces proactive global lookup-floor backoff
and the low-positive-GraphQL reset pause with the existing per-project refresh
timer. Quiet projects lengthen their interval under shared budget pressure, capped
at five minutes (or their configured base interval if longer); active candidates,
claims, and pending completion writes keep the normal cadence. Budget reset
restores the base interval. Actual exhaustion, provider throttles, and connector
REST write protection remain authoritative. `TestProjectRefreshColdFleetRequestCounts`
and `TestProjectRefreshActiveFleetDispatch` cover cold five/ten-project reads,
eventual refresh, reset recovery, free permits, and dispatch transition writes.

GitHub secondary throttling (#2829) uses the client cooldown deadline for both
request admission and the existing scheduler lookup backoff. Retry-After (or a
one-minute fallback, increasing on repeated failures) gates queries, probes, and
mutations. Usage flushes and primary-budget refreshes cannot clear this deadline;
an expired secondary response cannot restart dispatch recovery. This consolidates
the two cooldown schedules without adding a timer or configuration surface.
`TestClientGraphQLSecondaryBackoffExpires`, `TestClientGraphQLSecondaryRepeatedFailures`,
and `TestGitHubLookupBackoffSecondaryDeadline` cover these boundaries.

Legacy worker caches are removed at project startup using an absolute, home-expanded
workspace root (#2742). The obsolete per-project shared-cache sweep and its state
fields are removed; the existing host cache trim and report are the single cache
mechanism. Doctor retains read-only warnings for legacy roots. `TestRemoveLegacy`
covers absent, absolute, and tilde roots.

Update and restart draining (#2745) reuses the runtime dispatch pause and session
limits. Manual runtime update requests use the same drain reservation as automatic
updates; SIGTERM shutdown uses that duration ceiling, including model-selection
levels. Managed restarts preserve child processes while the orchestrator drains.
The shutdown drain uses the existing drain-budget timer rather than the five-second
cleanup context (#2795); shorter parent deadlines emit an error with both budgets.
`TestShutdownDrainBudget` covers delayed drain acknowledgment, and the live-session
shutdown regression crosses the cleanup deadline before allowing completion.
Startup no longer bulk-reclaims live work attempts as `service_restart`; the
reclaim store API and query are removed. Historical restart rows remain readable
for retry and accounting compatibility. Existing expired-lease recovery runs at startup and on normal refresh, including
tracker pauses. Its query excludes currently owned attempts and deferred completions;
retained crash orphans expire without requiring another restart.
`TestRetainedWorkAttemptsExpireOnTick` covers this lifecycle.
The issue explicitly authorizes the existing update banner to show the drain count.
`TestSchedulerApplyPendingWaitsForBothAttempts` and
`TestStartupDoesNotReclaimLiveWorkAttempts` cover the update and persistence boundary
and replace the old startup-reclaim regression in the invariant manifest.
`TestSchedulerExplicitReleaseDrainsWhenAutomaticUpdatesDisabled` also runs through
the manifest to preserve CLI coordination when automatic updates are off.

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

Issue-body effort is bounded by the selected complexity level's effective effort
(including an explicit stage effort), rather than the maximum across all levels
(#2663). The preset no longer promotes complexity from issue-body effort alone;
its complexity labels remain the escalation path. This consolidates the existing
ceiling and removes the preset effort escalation rule without adding a mechanism.
`TestIssueEffortUsesComplexityCeiling` covers defaults, clamps, and log provenance.
Resume classification uses the requested issue effort for custom effort rules,
not the previously clamped value. Each bound evaluation clears old clamp
provenance; `TestResumeEffortClampProvenance` covers unchanged requests and
operator reductions.

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

Human Review conflict routing preserves durable `operator_move` and
`attempt_allowance_exhausted` lane entries (#2814). This narrows the existing
Rework route rather than introducing a park mechanism; ordinary arrivals still
route conflicts to Rework, and ready PRs can still promote. Repeated ticks and
reopened history are covered by `TestTickAutoPromoteHumanReviewIssuesConflictParks`.

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
Idle Rework PRs enter the existing promotion evaluation without a worker completion
record (#2688). Promotion reuses the merge worker readiness predicate and live PR
hydration; unresolved threads and known audit failures still prevent promotion.
Missing audits run in Merging. `TestReworkLivePullRequestPromotion`,
`TestReworkLiveDraftPromotion`, and `TestReworkLiveSecurityAudit` cover this
consolidation; no new transition reason or recovery loop is introduced.

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

GitHub dependency hydration consolidates native relations and current issue-body
declarations into one blocker list (#2751). Native state wins for duplicate refs;
an empty native list does not discard a current `Depends on:` declaration.
Unsupported repositories use the same body parser. Historical comments remain
diagnostic only, and refresh replaces the previous list rather than restoring
removed declarations. Public reads select dependencies before resolving blocker
state; the orchestrator consumes that list through its existing dependency gate.
Degraded native reads and unresolved blocker-state lookups propagate errors,
preventing candidates with unknown blocker state from dispatching. Both parsers
share fence-aware declaration scanning; fenced examples never add blockers. Workpad issue-state predicates
absent from the current combined list are explained and cleared as before;
explicit human actions and non-dependency predicates retain their meaning.
`TestDependencyAuthority` reproduces text-only hydration, including bold labels;
`TestDispatchDependencyRetry` covers repeated Todo waits and release on closure.
This repairs the body exclusion introduced in #2575 without adding a dispatch
gate, tracker write, reason code, or recovery loop.

Validator launch accounting uses the existing validator-run registry and shared
worker progress publisher. Validators appear alongside implementation workers in
runtime snapshots, including during startup and completion, and leave the registry
on exit. This consolidates observability with existing lifecycle ownership; it adds
no recovery path or orphan-suppression guard (#2505).

Standing capacity requests (#2611) remove request teardown at project-pass
boundaries and consolidate refresh updates into the existing request set.
INV-10 below documents the retained request identity and grant lifecycle.

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
published and merged work, and continued retention of unsafe candidates. That
change added no merge inference, expiration mechanism, or configuration. Expected
retention keeps path evidence without failing the sweep, so the existing sweep
interval advances even when local work remains.
`TestCleanupVerifiesLiveRemoteCommits` covers stale refs and remote failures in
residual, direct, and branch cleanup;
`TestResidualCleanupProtectsFinalizingTerminalWorkers` covers terminal worker
ownership until its completion event.

The operator-approved September 14 retention scope (#2681, INV-11 approval
recorded in the issue) extends this same reaper sweep: completed workspaces
expire after seven days in a configured terminal lane or after issue closure,
quarantine after three days or beyond the newest five entries, hook logs after
fourteen days, and unregistered attempt scratch after one hour. Terminal attempt
scratch and ownership records for missing paths are removed in that sweep too.
This consolidates artifact cleanup into the existing invocation, with no new
loop, configuration, or lane writer. Ordinary preservation remains necessary
for nonterminal work and unverified completion times; the approved expiry first
archives a verified Git bundle, staged and working-tree diffs, and all working
files. Active issues and live processes remain protected. Content-addressed
archives prevent identical recovery copies from accumulating after removal
failures while preserving earlier snapshots after partial deletion.
`TestRetentionCompletionClock`, `TestRetentionCompletedWorkspace`, and
`TestRetentionRemovalFailureDeduplicatesArchives` cover terminal-state clocks,
lossless expiry, and repeated removal failures. See
[workspace retention](workspace-retention.md) for limits and recovery instructions.

The existing automated-review check retains stale and in-progress bot summary
evidence (#2764): an established review cycle must complete at the current head
before promotion, native queue admission, or programmatic merge. A disabled
initial-review requirement or expired review deadline does not waive that cycle.
The existing `automated_review_missing` wait and PR comment publication are reused;
a per-head comment marker deduplicates `@codex review` requests across ticks and
restarts. No new gate, reason, or reconciliation loop is introduced.
`TestAutoPromoteReviewAtHead`, `TestReviewHeadRequestAndWait`, and
`TestReviewSummaryRetainsPendingHead` cover decision, request failures/deduplication,
and trusted summary evidence respectively.
`TestReworkLiveDraftPromotion` preserves marking a clean draft ready to trigger
review while keeping stale-review promotion pending.

Completed Rework cards with unresolved review threads use the existing same-lane
handoff (#2721). The completion-specific thread park is removed: it previously
filtered cards out of every refresh while retaining their completion state.
`TestCompletedReworkCandidatesRemainVisible` reproduces the three incident cards
and checks repeated candidate/board retention and completion cleanup. The
read-only `refresh.candidates_missing_vs_tracker` count compares identities from
the successful candidate read with the published board (including lane changes).
Doctor reports it per project. Null means no completed comparison; the count
retains its last successful value on refresh failure and does not independently
audit GitHub or include observed-only lanes. No additional polling or dispatch
gate is introduced.

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

Every dispatch, including In Progress retries and stranded re-dispatches, resolves
non-terminal dependency refs through the existing dependency
auto-unblock resolver and readiness predicate, sharing blocker reads within each
dispatch refresh (#2509). Closed tracker state releases ordinary dependencies
without requiring a terminal lane observation; human completion evidence remains
required. Current structured Workpad blockers in `in_progress` or `blocked` with
an open `issue_state` predicate join current body dependencies; native relations
provide state for duplicate references. Current body declarations survive an
empty native list (#2751), while historical comments remain diagnostic only. Unresolved Workpad evidence preserves the dependency wait;
explicit open predicates use tracker closure, even in terminal lanes. A dependency
wait preserves retry attempt and resume state.
`TestDispatchDependencyRetry` covers open-to-closed transitions for native, body,
and Workpad sources (#2699). This consolidates dependency readiness without
adding a recovery loop.

Dispatch also consults the existing recorded-blocker evaluator for structured
Workpad predicates, including direct PR references (#2645). Normal dispatch,
due retries, and queued grants wait while evidence holds or is unverifiable;
cleared predicates release dispatch without a new park or recovery loop. Native
relations and current body declarations share authority for issue-state dependencies.

Issue #2595 consolidates `rework_limit`, `no_progress_limit`, and dispatch-loop
attempt accounting into one fixed allowance: three code/rework sessions started
since the last merged PR or operator lane move (#2692, #2729). The
durable attempt log owns the count, with the window derived from existing lane
history. Human-origin moves, and moves not initiated by the Detent instance,
reset the window regardless of source or destination lane, including Merging or
Blocked to Rework; Detent-instance moves and same-lane observations do not. Sessions
started at the operator-move timestamp count in the renewed window because lane
observation precedes dispatch in the same tick; merge boundaries remain exclusive. New commits,
new PR heads, CI signatures, ordinary lane changes, and acknowledgements alone
do not reset it. The existing triage comment receives a timestamped reset line
when comment updates are supported; publication failure does not undo the move. Instance-attributed startup, transport, workspace and restart failures
are excluded. Question-ending successful waits and sessions with a live structured
human blocker are also excluded (#2789). Conflicted PR sessions count toward the
allowance because conflict resolution is worker-owned Rework (#2807); a conflict
does not override a genuine human-wait exclusion. The existing
start record retains external-wait evidence through completion and restart;
completion metadata can also record a wait observed at completion. This narrows
the existing allowance rather than adding another brake or recovery path.
Exclusions never synthesize operator moves or reset other chargeable sessions.
Historical question receipts remain recognizable. Attempts already stamped
with external-wait evidence remain excluded because the stored flag does not
record its cause; the conflict correction bounds future sessions only. `TestAttemptAllowanceExternalWaits`,
`TestAttemptAllowanceExternalEvidencePersistence`,
`TestAttemptAllowanceExternalWaitRestart`, and
`TestAttemptAllowanceDispatchAndRestart` cover exclusions and unchanged failure
exhaustion. Immutable PR merge times and merge observations in the durable lane timeline
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
Already-merged completion no longer requires pre-dispatch authorization (#2778):
the worker records the merging PR, commit, tracked branch/head, successful ancestry
check, and acceptance evidence. Existing operational completion and current-attempt
checks persist and publish this evidence before Done; incomplete evidence retains
the PR gate. `TestMergedCompletionEvidence` and
`TestTransitionAlreadyMergedCompletion` cover this consolidation.
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

ProjectV2 combined refresh (#2818) reuses the candidate page/item cursor contract
inside the existing board scanner. Failed pages retain the private accumulator;
only a completed enumeration publishes a snapshot. Retained progress is verified
against the project update timestamp, item count, and local cache revision before
resuming; changes during enumeration discard the private scan. Thin board metadata
and scalar bodies supply lane diagnostics and avoid REST body fanout. Requested
candidates and configured active/observed states receive batched scheduler and
authoritative PR evidence, excluding configured terminal states. No retry loop, timer, configuration, or lane writer
is added. `TestRefreshBoardResumesFailedPage` covers page failure and cancellation;
`TestRefreshBoardChangedBetweenAttempts` covers shifted pages, count changes, and
missing revisions. `TestRefreshConfiguredSchedulerStates` covers custom state
sets, and `TestRefreshThinBodyFallback` checks 152 candidates under GraphQL
backoff against the existing REST cap, without publishing partial results.
`TestProjectRefreshHourlyWorkload` checks the large-board request and point budget
using measured GraphQL response costs.

Admission candidate scans consolidate REST label, repository, and issue-field
pagination into one reader, retaining page/item continuation in the existing
admission run ledger (#2574). Completed hydrated candidates remain usable when
the existing fanout budget ends a read; no retry routine, reason, or configuration
is added. Exhausting a source resets its continuation for the next scan.
If that budget also prevents a saved stale snapshot's early recheck, its
eligibility check consolidates into the mandatory final revalidation; other
completed candidates can still use the evaluation window.

Security audit verdict routing from Merging shares the existing auto-promote
classifier and findings publisher with source-lane completion (#2642, #2726).
The merge completion handler routes actionable findings through its existing
Rework handler immediately, clears retries, and includes findings on the issue;
pending evidence and audit infrastructure failures retain their existing wait
without publishing issue findings. Findings publish to the PR
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

Security audit continuations (#2746, operator-approved) reuse the existing durable
run history for an unchanged base and descendant head. They audit the delta and
prior finding files, require explicit finding resolution, and retain p1/p2 defaults.
Changed bases or diverged history receive a full audit. Full findings, including
advisory and resolved entries, are published before routing; only unresolved
configured severities block. This consolidates repeated full audits without adding
configuration, routing mechanisms, or dashboard surfaces. `TestSecurityAuditCarriesPriorVerdict`,
`TestSecurityAuditDeltaSnapshot`, and `TestSecurityAuditAdvisoryPublication` cover the behavior. The reviewed dynamic-reason
fingerprint changes only to publish advisory findings before the existing lane write;
transition reasons and lane ownership are unchanged.

Draft merge revocation (#2759) and idle-Merging draft reconciliation share
current-head review/CI classification. Unresolved threads reuse
`unresolved_review_threads`; failing checks reuse `ci_not_green`, routing to the
configured Rework lane with finding bodies, locations, and check names. Only an
unexplained draft retains the configured review lane. Review-thread hydration
uses the existing reader before classifying drafts; unavailable evidence uses the
existing hydration wait without a CI retry or attempt increment. Idle findings
route to Rework even while another PR in the repository has an active worker.
`TestDraftMergeRoutesByEvidence` covers both routing paths. The reviewed dynamic
revocation writer forwards those existing reasons without a `merge_revoked:`
prefix; other revocations retain their existing vocabulary. No mechanism or
reason is added.

The shared lane writer withdraws native queue ownership before a departure
from Merging (#2822). Its reviewed INV-3 dynamic-reason fingerprint changes
without altering reason forwarding; operator destinations and reasons remain
intact, covered by `TestNativeMergeQueueReviewReworkAfterEnqueue`.

## INV-4 — Native merge queue

Cached queue ownership belongs to its PR head; after provider inspection confirms a replacement head has no entry, discard old-head ownership so normal admission can enqueue the replacement.

**Statement:** Merges go through the repository's merge queue when one exists.

A queued PR with unresolved review threads must be withdrawn before the existing
review Rework handoff. Thread hydration precedes every cached or provider-entry
shortcut. A card departing Merging must withdraw its live provider entry before
local ownership is discarded; other departures preserve the chosen destination
and transition reason. Failed withdrawal retains ownership for the existing
error/retry handling. Landed or closed PRs have no live entry to withdraw.
`TestNativeMergeQueueReviewReworkAfterEnqueue` covers cached and refreshed
reviews, resolved threads, withdrawal failure, and operator departures (#2822).

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
check until it ages out of that day; a second queue removal on the same PR head routes to Rework under the
existing budget, not through a merge bypass.

Queue removal is consumed once by the existing removal accounting (#2621).
The first removal clears ended queue ownership and leaves the issue in Merging;
the next pass uses normal admission for an eligible PR. Repeated observations
of that removal do not route clean branches to Rework or spend another attempt.
The second distinct removal on the same PR head routes to the configured Rework
lane with available failed job, workflow run, and test diagnostics (#2738). Removal
accounting is persisted with the PR head in the existing workflow timeline before
retry admission and restored after restart. Rework retains the exhausted head
budget; a repaired head starts a fresh count. Legacy events without a head retain
their conservative issue-wide accounting. GitHub commit comparison attributes an
integration-commit removal only when it contains the current PR head, including
after local queue-cache loss. `TestNativeMergeQueueHeadBudget` and
`TestMergeGroupRemovalIdentityAndFailure` cover head identity and failure evidence.
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
Rework can produce a new ready head. Detent also marks an idle Rework draft ready
when its other promotion gates pass (#2688), then re-reads its ready state and
current-head checks on a later tick before promotion. `TestReworkLiveDraftPromotion`
covers ready marking, failure, and rechecking live evidence. Workflow assertions cannot deduplicate
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
refreshes retain standing requests and update their actions and slot requirements
in place. Refreshed eligibility decisions remove obsolete requests; shutdown and
explicit pause/configuration invalidation discard requests and release unused grants.
All modes use the same lifecycle; strict priority is
only an ordering rule. Authorization, dependencies, retry readiness, and local
lane ceilings are checked by the callers before acquisition. Admission reads
and filters candidates before acquiring local or global capacity for evaluation.
`TestGlobalDispatchGatePriorityOnlyPicksNextJob`,
`TestGlobalDispatchGateReadyRequests`, `TestGlobalDispatchGateConcurrentReadyRequests`,
`TestQueuedDispatchRanksIndependentRequests`, `TestQueuedDispatchPreservesProjectCeilings`,
`TestRunDispatchesQueuedRequestsWithoutPolling`,
`TestRunDispatchesQueuedRequestsAcrossHostsWithoutPolling`,
`TestQueuedDispatchChoosesAvailableHost`,
`TestStandingDispatchSurvivesProjectRefresh`, `TestStandingDispatchRequestUpdates`, and
`TestAdmissionWithoutEligibleCandidatesAcquiresNoCapacity` run through the
manifest. The existing boundary fuzz seeds retain free-capacity and release
failure coverage. The no-polling queue fixture waits for both initial ticks to
complete before mutating candidate state or releasing the occupied slot (#2772).
A submission notification precedes its return, and startup State reads can serve
snapshots; neither alone proves the request reached the pending-grant path.
`TestRepositorySources` rejects retired selection, rescue,
reservation, and preemption symbols and reason strings, including assembled
constants. Only the two enumerated historical store readers and the dedicated
configuration compatibility reader may retain old reason strings; negative
fixtures cover that boundary. Previously valid staleness benign-reason overrides
remain accepted on startup and reload, but retired reasons are neither emitted
nor included in defaults.

Project refresh boundaries no longer cancel and resubmit pending requests (#2611).
The existing request set is reconciled from dispatch decisions, retaining result
identity and applying updated lane, host, and capacity requirements under the
existing gate locks. This consolidates the per-pass lifecycle without adding a
reservation or recovery mechanism. Grants delivered during a synchronous refresh
are consumed when the project event loop returns, with existing fresh-candidate
validation before launch.

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

## INV-12 — Native toolchain caches

Detent never substitutes its own cache or state location for a toolchain's native
one; bounding uses the toolchain's own mechanism. Workers inherit the host
toolchain caches. Detent may house-keep a native cache (age or size trim that the
toolchain tolerates) but never relocates it, never keys it per project or per
attempt, and never introduces a configuration key that does either. Per-attempt
isolation is limited to `TMPDIR`/`TMP`/`TEMP`.

Detent's former per-attempt and per-project Go caches duplicated a content-addressed,
concurrency-safe host cache. Two Detent-owned caches of about 750 GB on one host,
plus the operator's copy, filled the disk and took twelve CI runners down. The
same duplication risk applies to npm, pnpm, Cargo, pip, uv, Playwright browsers,
and Docker layers.

The runner must not set `GOCACHE`, `GOMODCACHE`, `GOBIN`, `GOLANGCI_LINT_CACHE`,
`GOFLAGS` (including injected `-modcacherw` flags), `npm_config_cache`, `PNPM_HOME`,
`YARN_CACHE_FOLDER`, `CARGO_HOME`, `CARGO_TARGET_DIR`, `PIP_CACHE_DIR`, `UV_CACHE_DIR`,
`PLAYWRIGHT_BROWSERS_PATH`, or `DOCKER_CONFIG`, unless explicitly passed through
by the operator. Host values and explicit environment overrides are preserved.
New `workspace.*cache*` keys fail configuration loading with an INV-12 message.
The removed `workspace.cache_strategy` retains #2680's one-release warning and
is ignored; it does not select a cache location.

`TestINV12WorkerInheritsToolchainEnvironment` tests the common runner preparation;
`TestINV12BackendToolchainEnvironment` launches local helper processes through both
Codex and Claude Code backends and checks absent, inherited, and explicit values.
`TestINV12WorkspaceCacheKeys`, `TestINV12DoctorNativeCaches`,
`TestINV12DoctorWorkspaceKinds`, and `TestINV12TrimReadout` enforce config rejection
and read-only diagnostics. All are registered in the invariant manifest.
Doctor reports native Go cache paths and sizes per project, the last completed
reaper trim (or “not recorded”), and warns about legacy `.detent/cache` roots in
the workspace root or workdirs, including former per-attempt cache components
under `.detent/worker-tmp`. Reaper timing is recorded in `detent-trim.txt`
inside the native build cache; Go's own `trim.txt` is left untouched.
The native build cache defaults to 20 GiB with the existing 48-hour age trim.
Doctor warns when the effective bound exceeds 10% of available space on the
cache volume. The existing reaper reuses its trim walk to publish retained build-cache bytes in
`host_cache` in `/api/v1/state` as the sole cache surface (#2742). It does not rescan
the build cache or traverse the module cache; unmeasured module fields are omitted.

## Check boundaries

The source walk covers non-test Go packages under `internal`, `cmd`, and `tools`
for the build platform running the gate. CI's Linux, macOS, and Windows test
jobs cover their selected files. It deliberately leaves docs and negative test
fixtures available to explain violations. Source vocabulary and workflow checks
complement behavioral tests; they do not prove every natural-language rule,
protect themselves against edits, or enforce live settings. See the
[runner contract](../invariants/README.md) for the same limitations.

Draft-ready credential and connector-policy failures (#2688) reuse the instance
forge condition and existing worker write canary (INV-2/INV-3). The auto-promote
writer waits for that recovery instead of resetting the probe on every tick;
transient ready failures retain normal tick retries.
`TestReworkLiveDraftPromotion` verifies this attribution without issue failure
strikes or lane changes.
