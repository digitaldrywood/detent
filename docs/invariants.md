# Repository invariants

These rules apply to all repository changes. Follow the
[validation policy](../AGENTS.md#validation). Only a change to an invariant's
rule or its enforcing check edits this document. Identify affected INV IDs in
the Change/PR description; put per-change rationale, evidence and verification
there or in issue comments, as required by INV-16. A passing check does not
authorize weakening a rule.

An invariant is a rule, not a specification. Each section states its rule in
a few short paragraphs and then names its enforcing checks. Implementation
detail that changes with ordinary feature work (how a path verifies, retries,
routes or formats) belongs in the named tests and in package documentation,
never here. A change that edits this document without changing a rule or an
enforcing check fails review.

**Checks:** `make check-invariants` runs the exact behavioral tests in
[policy.json](../invariants/policy.json) through
[tools/invariantcheck](../tools/invariantcheck/main.go); missing, skipped or
failed test evidence fails the check. These repository-local checks do not
authenticate policy exceptions, enforce live settings or protect themselves
against edits. Source checks cover non-test Go packages under `internal`,
`cmd` and `tools` for the selected build platform. Natural-language and raw
HTTP bypasses remain review boundaries. See the
[check contract](../invariants/README.md).

## INV-1 — Lane ownership

The orchestrator is the only writer of tracker lane state. Workers report
outcomes through the selected tracker's completion contract; they never write
lane labels, status fields or approval comments. Native workers report through
the authenticated final outcome, and the host owns publication and lane writes.

An ephemeral execution process never owns a second claim, renewal or completion
writer. Its controller retains the fenced execution, local store and worker
cleanup through acknowledged outcome publication and lease release. Worker
exit alone never frees execution capacity. Local process journals cross the
worker protocol only for sessions opened by that worker's channel; remote
process identities never enter the local reaper.

A completed turn's stale runtime heartbeat cannot expire an execution while
its controller or worker cleanup still owns finalization. The existing bounded
finalization context owns that timeout. Completed executions without a live
local owner still expire and release their fenced claim.

Blocked has a closed list of causes: an explicit human question or permission
request with a typed `human_action`, or an explicit operator stop. Machine
failures, including validation, missing checks, delivery and retry exhaustion,
retry in a dispatchable lane through the existing capped-backoff owner. They
never synthesize a human prerequisite. Current source and execution fences,
dependencies, gate requirements and budget admission remain authoritative.

The existing Blocked recovery read derives this decision from current human
intent and the current operator-stop entry on every read. Historical machine
parks, retry counts, fingerprints and consumed recovery signatures do not own
the lane. No migration or new recovery sweep clears them.

Execution ownership is a fenced, renewed lease. Completion replay payloads
never exempt an attempt from lease expiry or independently veto a claim. Expiry
returns the item to claim eligibility under current source custody and human
intent; no recovery park or park-clearing sweep owns that return.

**Lease enforcement:** `TestRetainedWorkAttemptsExpireOnTick` and
`TestHubSchedulingReadinessBeforeClaim` in `internal/orchestrator` and `TestTimeoutWorkAttemptsProcessEvidence` and
`TestStartupTimeoutWorkAttemptsScope` in `internal/store`.

Derived hold metadata requires an owner, source revision and expiry when
written. It cannot replace current intent: wiping it and recomputing must
produce the same dispatch decision. Expiry of derived metadata does not expire
an unanswered human action or an explicit operator stop.

**Closed-list enforcement:** `TestBlockedRecoveryRecomputesDispatchWithoutDerivedHolds`
and `TestStoredHoldRequiresProvenance` in `internal/orchestrator`, plus the
lane writer's typed-intent and metadata validation.

The lane ledger owns `UpdateIssueState` and state-field `SetIssueField` calls.
Enumerated connector adapters implement or forward those operations without
deciding transitions. Admission uses the injected orchestrator writer. Raw
HTTP tracker writes must not bypass this ownership boundary.

For native projects, a supplied repository workflow definition owns the state
and transition model through trusted repository configuration reports. Applying
the resolved model is atomic and rejects removal of occupied states. Cloud Markdown authoring is available only without repository
definition authority; UI, API and MCP cannot independently override a repository-controlled
workflow. Definition authority does not transfer issue-move ownership.

Orphaned provider-session resumes use the new dispatch's durable attempt ID
and generation in their completion handoff. Resume selection rejects an
orphaned request missing either value before starting a turn; the ordinary
retry starts a fresh attempt.

**Enforcement:** `TestRepositorySources` in `internal/invariants`;
`TestAdmissionRequiresLedgerWriter` in `internal/admission`. Review rejects
lane-write bypasses and new ownership exceptions.
`TestLocalSessionProcessOwnership` in `internal/runner`,
`TestNativeDelayedResponsesPreserveSuccessor` in `internal/hubclient`, and
`TestLocalWorkerReleaseAndPolicyCoexistence` in `internal/cli` preserve execution
ownership across process exit, finalization and release retries.

**Workflow authority enforcement:** `TestRepositoryNativeWorkflowProjection` in
`internal/config`, `TestNativeCloudWorkflowFallback` in `internal/project`, and
`TestHostedProjectWorkflowConfiguration`, `TestProjectPolicyAuthorizationAndAtomicClaims`
and `TestHostedProjectTools` in `internal/hubserver`.

## INV-2 — Instance-owned infrastructure failures

Infrastructure failures attach to the instance, never to the issue, whether
they happen before the first agent turn or during a turn. Backend startup,
protocol, credential, network and workspace-hook failures do not consume the
issue's failure allowance or authorize source repair. Retry accounting
distinguishes genuine code failures from instance interruptions, and unknown
infrastructure diagnostics remain instance intake. A workspace error is forge
unavailability only when a matching forge host and Git read operation fail;
an unrelated hook failure cannot inherit that classification.

Native landing conflicts and failed landing commands are source refusals, not
infrastructure failures or shipped work. Landing refusal destinations belong
to the workflow owner: conflicts, moved heads and command gate failures prefer configured
Rework, other refusals prefer the configured review lane, and terminal,
operator-only and self-transitions are excluded. Recovery and Rework resume
only from a verified preserved source checkpoint; unknown changes and
unavailable authority are refusals, never checkpoint rewrites or relaxed
comparisons. Human and permission holds retain their authority. Pre-provider checkpoint
and recovery refusals, including changed local checkpoints and policy mismatches,
retry through the existing instance owner with capped backoff. They never
write Blocked or consume an issue failure allowance. Unavailable execution
authority still forbids lane writes; retry never relaxes source verification.

**Enforcement:** `TestPreTurnFailuresRetryWithoutPausingProject`,
`TestObservedLanePreTurnFailureRemainsInstanceOwned`,
`TestWorkspacePreparationRetriesIssueWithoutPausingProject`,
`TestClassifyWorkspaceForgeReadFailure`,
`TestWorkspaceSSHRefusalDoesNotTripProjectBreaker`,
`TestRecoverDurableWorkspaceGitReadWait`,
`TestWorkspaceDiskExhaustionRetriesWithoutProjectBreaker`, and
`TestNativeLandingRunCompletion` and `TestNativeChangeRunCompletion`
in `internal/orchestrator`;
`TestNativeExecutionSettlesFinishedRun` in `internal/hubclient`;
`TestClaimCandidatesRequireUnansweredWorkItem`,
`TestClaimCandidatesRetryInstancePreProviderFailures` and
`TestHealthRunnerSetupFindings` in `internal/hubserver`;
`TestLocalGitCreateReadsPublishedBranchWithRepositoryCredential` in
`internal/workspace`;
`TestNativeExecutionLandsReviewedVersion` in `internal/hubclient`;
`TestNativeRunnerOpensChangeAndLeavesDispatch` in `internal/hubclient`;
`TestLocalGitLandChangeViaGitHub` in `internal/workspace`;
`TestWorkerCredentialBlockerError` in `internal/runner`;
`TestIssueSpendSinceExcludesInstanceInfrastructureAttempts`
in `internal/store`; `TestNativeInterruptedCodeRecoversPersistedSession` in
`internal/runner`; `TestNativePlannerAutomaticHandoff` in `internal/hubclient`.
Review preserves instance attribution outside those cases.

## INV-3 — Net-zero mechanisms

A change that adds or expands a brake, breaker, lease, park, recovery path,
revocation, reason code or reconciliation loop must remove or consolidate an
existing one in the same change, and the remedy for a misbehaving mechanism is
never a guard. Do not add configuration keys, CLI subcommands or dashboard
surfaces to work around a mechanism. Fix the mechanism.

Constant lane-transition reasons must use the
[existing vocabulary](../internal/invariants/source_policy.json). Dynamic
forwarding functions require reviewed formatted-source SHA-256 entries.
Renaming a mechanism or updating a snapshot does not establish removal or
consolidation. Provider messages, operator reasons and decision helpers remain
review boundaries.

The complete-diff security audit uses bounded turns within the existing audit
session and deadline. Its host-owned aggregation requires every deterministic
source range and retains all unresolved findings; chunking does not introduce
another recovery or capacity mechanism or exempt any changed content.

**Enforcement:** `TestRepositorySources` and `TestSourceViolations` in
`internal/invariants`; `TestChunkCoverage`, `TestLargeAuditAggregation` and
`TestChunkSourceAndContinuation` in `internal/securityaudit`, and
`TestRunnerChunkedAudit` in `internal/runner`. Review verifies removal or
consolidation.

Per-issue instant/repeated failure, no-progress, spend-since-progress,
dispatch-loop, token-ceiling and workspace retry brakes are consolidated into
the existing machine retry owner. Machine failures remain dispatchable with
growing attempts and capped jittered backoff, including native failures,
no-product completions, session-brake retries and merge failures, refusals and
unavailable-evidence waits. Durable restoration requires configured
`workflowMetrics`. Successful, non-rejected implement continuations (generic
and native) reset to attempt 1 only for a host-observed changed PR head or
committed tree against the dispatch baseline. Unchanged or unavailable source
evidence and rejected completions retain growing retries. Productive merge and session-brake
continuations also reset on verified changed committed/published source with
custody and rejection checks intact. Resumable assertions and dirty-only changes
do not qualify. Successful CI wait clearance preserves the attempt; unproductive
waits and repeated breaches grow. Accepted stage and deliverable
completions advance or finish without a new commit or a continuation reset;
arbitrary Workpad changes do not establish source progress. Budget/usage checks, provider capacity and session
ceilings are existing policies, not an unconditional spend bound. An unset or
disabled budget imposes no monetary cap; drain mode can admit already-admitted
work. Capacity limits concurrency and configured session ceilings limit one run,
not cumulative retries. Historical brake metadata cannot veto the next attempt.
Explicit human action and operator stops retain their authority.

**Retry enforcement:** `TestMachineFailuresUseRetryOwner`,
`TestMachineRetrySurvivesStoreRestart`, `TestDerivedHoldDispatchEquivalence`,
`TestHandleRunResultResetsRetryAfterProgress`, and
`TestBackendCapacityHelperBoundaries` and
`TestPruneBudgetRefusalsReevaluatesDailyCap` in `internal/orchestrator`.
`FuzzSafetyCriticalOrchestratorBoundaries` retains completion-evidence,
capacity resume arithmetic, capped backoff, dispatch ordering and priority-only
real-capacity acquisition coverage. The retired-symbol checks prevent restoring
the deleted per-issue brake owners.

## INV-4 — Native merge queue

Merges go through the repository's merge queue when one exists. Without a
queue, Detent uses its serialized merge worker. Other repositories retain
their chosen settings.

Queue ownership belongs to the current PR head. Only completed success, skipped
and neutral checks qualify for queue admission, subject to provider-required
contexts; skipped checks are not passed-test evidence. Unresolved review
threads require queue withdrawal before the configured Rework handoff, and a
withdrawal failure never prevents a lane write to Done. Queue removal is
counted once per distinct removal and PR head: the first permits readmission,
the second on the same head routes to Rework, the count survives restart, and
a repaired head starts fresh. After a verified native landing merge, the
landing owner closes earlier open landing pull requests for the same item only
when the pull head matches the SHA encoded in its landing branch.

**Enforcement:** `TestDelegateNativeMergeQueueIssuesEnqueuesGreenTrainWithoutWorkerDispatch`,
`TestDelegateNativeMergeQueueIssuesCachesQueueEntries`,
`TestNativeMergeQueueReviewReworkAfterEnqueue`,
`TestNativeMergeQueueWithdrawalDoesNotBlockDone`,
`TestNativeMergeQueueSkippedChecks`, `TestNativeMergeQueueNeutralChecks`,
`TestNativeMergeQueueHeadBudget`, and
`TestNativeMergeQueueBudgetSurvivesRestart` in `internal/orchestrator`.
`TestLocalGitLandingReplacement` in `internal/workspace` enforces superseded
landing ownership, cleanup and retention of moved or unknown heads.
The `detent doctor` queue diagnostic reports programmatic merges while a queue
is present; live queue settings require operator inspection.

## INV-5 — Local pull-request validation and scheduled release evidence

This repository requires no branch status check. The only PR validation is
`.github/workflows/ci.yml` on `pull_request`: its existing Linux `verify-fast`
job checks the PR merge commit with Windows amd64/arm64 cross-builds and amd64
vet in its Vet step. Other jobs and broader checks do not run on PR events;
the Windows commands do not run in the scheduled or manual full suite.
No Windows VM, new job or landing gate is added.
No validation workflow starts from `pull_request_target` or `merge_group`.
The other event exception is `.github/workflows/cla.yml`: its single job runs
only `contributor-assistant/github-action` without checking out or executing
PR code, and owns signature records on the unprotected `cla-signatures` branch.

Source completion relies on the finalization gate (`make check-landing`); workers
run focused tests during edits. The finalized immutable native version carries its successful command receipt
for the exact reviewed head and tree. Native direct landing verifies that receipt,
squashes clean Changes in queue order onto one staging head, and pushes the
batch without repeating validation or calling a GitHub API. A conflicting
member leaves the clean batch and retries by rebasing; in `per_landing` mode the rebased tree
runs build, short Go tests for touched packages, and lint of touched files.
The receipt records the landing path and tested packages. A moved base repeats
the clean-squash decision on the new head. GitHub PR landing stays behind its
project policy opt-in, except for an operator-associated, provenance-verified
release PR delivery. That association keeps the project policy unchanged,
requires independent review, and retains the exact reviewed release base and
source tree through landing. An associated single-commit release pick lands
with an atomic Git compare-and-swap against that reviewed base, after verifying
the exact PR, reviews, checks and active authority. Repository push protections
remain authoritative; a refused push never falls back to an unpinned PR merge.
GitHub PR delivery retains its configured
validation and native checks; security audits cover the immutable version
base and head. Native direct landing also audits its integrated commit.
This repository configures `make check-landing` for the combined GitHub landing
tree: build, short tests for touched packages and dependents, and lint of changed
Go files. Module changes widen validation.
A failed per-landing command returns to configured Rework with the failing output. The project publishes no local status and sets
`gate.required_status_checks: []` and `gate.automated_review: "off"`; ordinary
merge waits for no additional CI producer or scheduled validation, while
reported failed CI, native base-branch requirements and reported P1 findings
remain authoritative. Diagnostics support concurrent worktrees without a shared
validation lock. Other projects retain their configured policies.

Source completion checks repository source and workflow invariants and verifies
that behavioral test references exist without duplicate package groups. The
full behavioral invariant manifest executes only in the scheduled suite.
Full browser, integration, race, coverage, fuzz, NilAway and generated-output validation
run in the full suite: hourly on pinned `develop` commits, and on every
dispatch to `release/*`; pushes to a release branch do not start it. Develop receives one full suite per SHA across
scheduled and manual runs. Develop preflight skips completed non-cancelled
runs regardless of conclusion; manual `force: true` or a non-`none` `fail_job`
runs it again. The finalizer reports skipped scheduled activity without
publishing green validation or release evidence. Release runs always validate
their event SHA, independently of
develop and other release branches. Release evidence is the green full suite
on the release branch head, never develop evidence reused after cherry-picks.
Successful develop validation publishes `develop-full-ci`. Release validation
publishes `scheduled-full-ci` only when creating new immutable release evidence.
On a release branch, the finalizer verifies the validated SHA is still the branch head, creates
an annotated `vX.Y.Z` provenance tag within that branch's `release/vX.Y` series,
and dispatches the existing release workflow. Green develop commits select
future cuts but do not publish stable tags. Cuts are weekly by default or
operator-dispatched. Automatic cuts proceed only when no cut, release
validation, signed build or deployment is active and
no successful production deployment and smoke completed within one cadence
derived from the cut workflow's UTC cron. Explicit manual SHA cuts remain
available within that window and bypass frozen-series admission. Actions history
and remote release heads own admission; API failures are errors. Outside the
cadence window, an unfinished current or newer frozen series defers automatic
cuts until its head has a successful stable-tag production deployment and both
smokes. Older series superseded by a later successful production series do not
defer newer uncovered source.
Automatic source selection uses the newest green develop ancestor
not already covered by a release series or stable tag, or unreleased develop tip
if none exists; it succeeds without cutting when develop is already covered.
Explicit covered sources remain errors. Picks are develop-to-release only, with
`-x` provenance and a PR.
All release API calls belong to operators and GitHub Actions, never Detent
runners. Develop landing does not wait for release stabilization. Release
deploys staging, runs its smoke, then deploys production and runs its smoke;
a failed smoke stops promotion. Operators promote the validated tag tree to
`main` through a reviewed PR. A commit already tagged in that series receives no
new stable tag or replacement status. A retry authenticates the original tag
evidence, refuses superseded evidence, and resumes dispatch only when no release
run exists for that tag. Every release, including operator recovery, starts from
a frozen develop snapshot on a release branch. Fixes land on develop first and
are cherry-picked with `-x` through release-branch PRs. No develop prerelease or
source-gate-only deployment path is permitted. Staging and production deploy
the same validated version and binary; enrolled runners follow the Hub through
the existing coordinated update owner.
Failing Detent release suites share one open stabilization owner per release
series, with each failed job's evidence and replay identity retained in bounded
occurrences. Develop retains per-job reporting under stable job fingerprints;
other projects retain their configured reporting behavior. Reports enter native
Todo at least High,
following [deployment and release failure reporting](../AGENTS.md#deployment-and-release-failure-reporting);
infrastructure reports carry the infrastructure label and CI-instance
attribution, and failed Cloud publication never falls back to GitHub issue
writes. Explicit workflow cancellation stops finalization and creates no repair
issues, including before the run conclusion settles; timeouts and cancelled
jobs without workflow cancellation remain reportable and never authorize a tag. Plain checkouts compile and test without ignored assets; build owners
prepare conversation bundles and configuration references before deployment.

**Enforcement:** checks assert outcomes only: no workflow triggers on
`pull_request`, `pull_request_target` or `merge_group` except the bounded
`ci.yml` Windows guard and the `cla.yml` exception; `ci.yml` triggers only on
`pull_request`, `schedule` and `workflow_dispatch`, isolates its other jobs from
PR events, and its
finalize job needs every other job in the parsed workflow and runs
`scripts/scheduled-ci-finish.sh`; the finalizer fails the run when any job
concluded other than success and creates stable tags only after a green
release-branch suite; and `release.yml` verifies provenance of the exact tagged
commit before building. Checks must not pin workflow shape such as job
conditions, schedules, shard counts, timeouts or command text except where
needed to enforce the bounded PR guard.
`TestCIWindowsChecksUseExistingLinuxVetStep` in the repository root;
`TestRepositoryWorkflow`, `TestRepositoryHasNoPullRequestActions`, and
`TestWorkflowViolations` in `internal/invariants`;
`TestReleaseWorkflowAuthenticatesExactCommitProvenance` in the repository root;
`TestMakeCheckFastOverlapsWorktrees` in `tools/checklock`; `TestParseProblems`,
`TestReport`, `TestCloudReport`, `TestCloudDestinationAuthority`,
`TestReleaseStabilization`, and `TestScheduledFinalizer` in `tools/cifailure`.
Review preserves project-specific policy and release authority.

## INV-6 — Isolated Codex home

Workers run with an isolated Codex home; user-level instructions never reach
a worker. Repository instructions and the Detent-provided worktree remain
authoritative. Worker state and transcript directories belong to the isolated
profile and must not write through host transcript symlinks.

Legacy resume copies only the requested persisted thread into profile storage
before verification or resume. Existing profile rollouts take precedence, and
resumed writes leave the host rollout unchanged. Retention preserves unfinished
threads, their resume sources and descendants of retained parents.

**Enforcement:** `TestPrepareCodexCommandForServiceIsolatesInstructions`,
`TestPrepareWorkerCodexHomeExistingInstructions`, and
`TestPrepareLegacyCodexRollout` in `internal/cli`;
`TestAppServerThreadPreparation` in `internal/codex`.

## INV-7 — Machine issue identity

Machine-filed issues carry an origin stamp and a stable problem fingerprint
that identifies the problem, excluding timestamps, attempt IDs and wording
variations. Filing uses the selected tracker's supported owner, preserves
imported provenance and matches open work before filing: a matching open issue
receives an occurrence or comment, never a duplicate, and when every match is
closed the newest one is already handled and nothing is created or changed.
Occurrence evidence identifies runs, attempts, commits and jobs without
changing problem identity. Detent release stabilization uses a stable release-series
fingerprint, retaining the original job occurrence identities. A later failure
after a stabilization owner completes starts a new episode without reopening
terminal work; replaying a terminal occurrence changes nothing.

The worker's `file_machine_issue` tool accepts integer priority ranks 1–4
(Urgent to Low); omission leaves priority unset and body prose never supplies
it. A fingerprint match may raise unset or weaker priority through the existing
mutation owner; it never lowers stronger priority, replaces origin or content,
removes operator holds, changes lanes or authorizes admission. Host-owned
worker intake requires the current runner's scoped lease. Reproducible tests, source diagnostics
and migration failures in the selected Detent Cloud project share the scheduled
reporting owner and host-derived defect identity, entering Todo at least High.
Other worker follow-ups remain Backlog. Reuse preserves existing lanes, holds
and scheduled job origin stamps.

**Enforcement:** `TestMachineIssueTool` in `internal/orchestrator`;
`TestNativeMachineDefectOccurrences` and `TestNativeReleaseStabilizationOccurrences` in `internal/hubserver` and
`TestDefectFingerprint` in `internal/issueorigin`;
`TestMachineIssueDuplicate`, `TestMachineIssueSeparateConnectors`,
`TestMachineOriginSurvivesBodyUpdates`,
`TestConnectorFindIntakeIssueSearchesDurableMarker`, and
`TestConnectorFindIntakeIssuePrefersOpenDuplicate` in
`internal/connector/github`; `TestManagerPreservesClosedFinding` in
`internal/intake`; `TestNativeMachineIntakeAuthorityAndFingerprint` in
`internal/hubclient`; `TestParseProblems`, `TestReport`, and `TestCloudReport`
in `tools/cifailure`. Review checks fingerprint stability.

## INV-8 — No strict freshness protection

This repository does not require strict up-to-date branch protection on
branches Detent merges into. Managed projects may require strict freshness;
Detent honors their branch rules. Code changes do not authorize live settings
changes.

Concurrent migration and generated-output collisions use ordinary Git conflict
handling. New Hub and store SQL migrations use UTC Goose
`YYYYMMDDHHMMSS_name.sql` versions; historical names and fixed migration-checker
cutovers remain unchanged. Adding a SQL migration requires only its new file,
with no shared version-count edit or landing-time renumbering.
Regenerate tracked sqlc and Templ Go output from combined source inputs
instead of text-merging generated conflicts. Tailwind, conversation bundles and
configuration references remain ignored output generated by build, test and
docs-publish consumers.

**Enforcement:** `TestDoctorRespectsProjectCIPolicy` in `internal/cli`;
`TestIndividuallyValidBranchesRejectIntegratedCollision` in
`tools/migrationcheck`; `make check-migrations`. Doctor reports live branch
protection as project evidence; the operator verifies live settings.

## INV-9 — Retired mechanisms

Retired lane revocation, indeterminate-lane stops, per-issue infrastructure
parking and root-level `rateLimit` in mutation documents stay retired.
Obtain budget information through a separate query. Documentation and negative
fixtures may name retired mechanisms; runtime code must not restore them.

The rolling landing barrier and its Red landing hold, execution, repair,
bisection and automatic revert owners stay retired. Historical barrier tables
and migrations, legacy policy decoding and stale-version handling remain
readable; they do not authorize restarting those owners.

Uncertain publication and historical `manual_recovery` checkpoint judgments
never hold an item. Before execution, the publication owner compares the remote
ref with the intended SHA: a match is published, an unchanged ref permits a
compare-and-swap retry (including an absent ref), and a moved ref requires
Rework/rebase. Definitive remote observations settle the effect checkpoint;
unavailable remote evidence is retryable infrastructure failure. Current source
custody, lease authority and explicit human intent remain authoritative.
No stored exclusion replaces this current-fact reconciliation.

Repository-policy approval matching no longer gates claims, lease renewal,
source publication or checkpoint recovery. Configuration reports retain source
provenance and the selected workflow; they are not recurring permission requests.
Authentication, project access, execution isolation, lease fencing and configured
review and validation requirements remain enforced.

Server-side MCP confirmation modes, YOLO, pending approval actions and the
`/chat/approval` page stay retired. The Hub enforces key scope, project access and
current grants on every call; authorized calls execute directly. Clients own
confirmation. Luna asks inline by default using its per-user client preference.
Removing a forbidden symbol from a list does not authorize its resurrection.

**Enforcement:** `TestRepositorySources`, `TestSourceViolations`, and
`TestMutationDocuments` in `internal/invariants` reject retired identifiers,
assembled constants and GraphQL mutation budget queries, including aliases
and fragments. `TestApprovedPolicyClaimExposure` in `internal/hubserver`
preserves claim exposure across approved-policy behavior and release drift.
INV-2's behavioral checks preserve infrastructure attribution.
`TestNativeRecoveryWipeDerivedHold` and `TestNativePublicationRecoveryOnAnyRunner`
in `internal/runner`, `TestNativeExecutionSettlesFinishedRun` in
`internal/hubclient`, `TestLocalGitLandChangeViaGitHub` in `internal/workspace`,
and `TestNativeChangeRunCompletion` in `internal/orchestrator` enforce effect
reconciliation and dispatch equivalence after removing stored hold judgments.
Review covers novel names, reflection and generated semantic equivalents.

Historical external-blocker answers, per-item instance/untyped answer cooldowns,
recorded-dependency recovery fences, sticky reason strings, consumed recovery
signatures, generic managed/fingerprint and cross-host park vetoes, and remembered
review-conversation refusals stay retired. Current typed dependencies and current
review/check evidence own readiness; explicit human and permission requests remain
human intent. No derived hold may be persisted without an owner, source revision
and expiry, and a stale revision gives it no weight. This does not expire human
intent or replace current facts with a timer.

**Enforcement:** `TestNativeDerivedHoldDispatchEquivalence` and
`TestReviewConversationHistoryDoesNotVetoLanding` in `internal/hubserver`,
`TestDerivedHoldDispatchEquivalence` in `internal/orchestrator`, and the retired
symbol checks in `internal/invariants`.

## INV-10 — Priority only picks the next job

Priority picks the next job. Free capacity is never held, reserved or kept idle
for a project, lane or priority. When a slot frees, dispatch the highest-ranked
READY request; if higher-ranked projects have nothing ready, dispatch whatever
is ready. The system never cancels, stops or preempts running work; only a user
cancels work.

Paid project Sprite capacity is the exception: project grants authorize which
projects a Sprite may serve, and project allocations keep paid capacity with
its configured project by default. The pool's `share_idle_capacity` opt-in
explicitly authorizes idle project Sprites to take other granted projects'
highest-ranked ready work. Eligible work from the Sprite's own project takes
precedence on its next claim; running work is never preempted. Allocation
ownership and desired counts do not change when idle capacity is shared.

Compatible execution releases and ordinary policy changes do not drain a
runner or withhold its free slots. Each new attempt captures its verified
execution release and effective policy; continuations keep that identity.
Routine controller replacement waits for an idle reservation while existing
workers continue and new compatible workers can launch through the older
controller. Only an identified critical security release, a concrete incompatible
execution protocol or shared-state contract, or intentional operator maintenance
uses the existing drain owner. Update requests, version skew and elapsed
deferral time alone never authorize a drain.

Pending requests own no capacity. Submission, release and capacity changes
share real-slot acquisition with atomic project, lane and host ceilings.
Eligible hosts remain alternatives until acquisition; retry host preference
must not strand available capacity. Refreshes retain and update standing
requests; acquisition revalidates current eligibility. Authorization,
dependencies, retry readiness, local ceilings and stop exclusions remain
authoritative. Admission filters eligible candidates before acquiring capacity.

Native claim eligibility has one candidate owner, recomputed from current facts.
Wiping historical derived hold evidence must preserve the dispatch decision.
Native admission reads reuse the claim owner's candidate predicates and retain
authorized current exclusion evidence without changing eligibility. Required
unfinished dependencies include their scoped identity and observed nonterminal
state; optional dependencies are never attributed as exclusions. Multiple
observed dependencies remain separate, and unproven exclusions or truncated
evidence remain explicitly unavailable. Snapshot observation times describe the
current read, never a recorded scheduler decision. Reads write no claims or
scheduler events and preserve active-lease, workflow, intake and project-grant
authority. Current refusals do not supply historical skip counts, missing
durations, private instruction causality or zero-valued analytics.

**Enforcement:** `TestGlobalDispatchGatePriorityOnlyPicksNextJob`,
`TestGlobalDispatchGateReadyRequests`,
`TestGlobalDispatchGateConcurrentReadyRequests`,
`TestQueuedDispatchRanksIndependentRequests`,
`TestQueuedDispatchPreservesProjectCeilings`,
`TestQueuedDispatchChoosesAvailableHost`, and
`TestStandingDispatchRequestUpdates` in `internal/scheduler`;
`TestRunDispatchesQueuedRequestsWithoutPolling`,
`TestRunDispatchesQueuedRequestsAcrossHostsWithoutPolling`, and
`TestStandingDispatchSurvivesProjectRefresh` in `internal/orchestrator`;
`TestAdmissionWithoutEligibleCandidatesAcquiresNoCapacity` in
`internal/admission`; `TestRepositorySources` in `internal/invariants`.
`TestNativeAdmissionExplanation`, `TestNativeDependencyReadPermissions`,
`TestNativeRuntimeReadHistoryCost`, `TestNativeRuntimeReadSharedCapacityCost`,
`TestHostedAnalyticsReads` and `TestNativeAnalyticsRuntimePopulation` in
`internal/hubserver` enforce scoped current evidence and recorded-read boundaries.
Review rejects semantic reservation and preemption substitutes.

`TestWorkerArtifactsRoutineUpdatePaths` and `TestSchedulerDrainPolicy` in
`internal/update` and `TestLocalWorkerReleaseAndPolicyCoexistence` in
`internal/cli` enforce compatible update capacity and exceptional-only drains.

## INV-12 — Native toolchain caches

Workers inherit the host's native toolchain caches, except that an enrolled
local Cloud runner may use one runner-owned Go build cache shared across its
projects and daemon generations. That cache is bound to one canonical runtime
store. The existing cache cleanup expires old entries and reclaims recent
managed entries, oldest first, toward its retained-size budget. Size eviction
tries the existing admission and exclusive consumer locks without waiting and
yields when either is busy. It never removes recent entries while a consumer
is active or holds admission closed waiting for that consumer. The existing
cleanup schedule retries reclamation; continuous consumers can defer it, so the
budget is an eventual target rather than a hard size limit. On the managed
cache's own filesystem, low free space lets that same cleanup reclaim below
its normal target using the existing reserve and cache budget for recovery
headroom. Unrelated filesystem pressure does not expand reclamation. Actual
low disk space retains its existing admission owner when reclamation is
insufficient; already-running consumers can continue writing during that
admission hold. Native operator caches retain age-only expiry.

Explicit operator cache overrides remain native and unmanaged. Local-board and
SSH workers retain native build caches. The old native build cache is not
removed automatically because operators and other applications may still use
it. Native caches may only be trimmed through mechanisms the toolchain
supports. No cache is keyed per project or attempt, and no new cache
configuration is introduced. Per-attempt isolation is limited to the
host-provided `TMPDIR`/`TMP`/`TEMP`; workers never fall back to host scratch or
use the orchestrator's scratch.

Apart from that owned build cache, the runner must not set `GOCACHE`,
`GOMODCACHE`, `GOBIN`, `GOLANGCI_LINT_CACHE`, `GOFLAGS`, `npm_config_cache`,
`PNPM_HOME`, `YARN_CACHE_FOLDER`, `CARGO_HOME`, `CARGO_TARGET_DIR`,
`PIP_CACHE_DIR`, `UV_CACHE_DIR`, `PLAYWRIGHT_BROWSERS_PATH` or `DOCKER_CONFIG`
unless explicitly passed through by the operator, except for the existing
shared Go admission budget's `-p`, `-trimpath` and `-toolexec` defaults.
That same budget applies to agent subprocesses and direct review/landing
validation, including repositories whose Go modules are nested. Explicit Go
flags and process limits remain authoritative; a missing root `go.mod` does
not exempt subprocesses from the configured host budget. Preserve host values and
explicit overrides. New `workspace.*cache*` keys are rejected; the removed
`workspace.cache_strategy` is ignored with its compatibility warning.

Sandbox grants follow the selected cache paths. Restricted turns keep cache
writes and network access disabled. SSH workers use remote host caches,
scratch and the shared Go admission budget.

**Enforcement:** `TestINV12WorkerInheritsToolchainEnvironment`,
`TestINV12BackendToolchainEnvironment` in `internal/runner`;
`TestBackendAppliesRunnerIsolation` in `internal/codex`;
`TestINV12WorkspaceCacheKeys` in `internal/config`;
`TestINV12DoctorNativeCaches`, `TestINV12DoctorWorkspaceKinds`,
`TestSSHProbeRequiresProvidedScratch`, and
`TestSSHLocalTargetIntegration` in `internal/cli`;
`TestINV12TrimReadout` in `internal/toolcache`.

## INV-13 — A board card is a title and one status line

A board card renders exactly the identity row (project, issue and PR references,
origin, model and configured effort), the title, at most one status line and
existing priority controls. The approved Cloud split footer places status and
priority above a divider, then a compact muted row containing only attempt
count and relative update time with small icons. Its menu stays inside the
identity header, and titles wrap without a fixed height budget. Compact may
hide effort before model; Cozy and Comfy retain effort. The status line
contains at most 48 Unicode characters and names the wait in actionable words.

Scheduler evidence, tracker ages, absolute timestamps, token counts, fact grids
and diagnostic text are not card content. Attempt count and relative update
time are permitted only in the approved compact metadata row. Existing
observations belong in the detail sheet and hover titles, subject to INV-15.
Adding card body content or lengthening the status line changes this rule.

**Enforcement:** `TestINV13BoardCardContent`,
`TestINV13SheetObservations`, and `TestBoardCardSignalBudget` in
`internal/web/templates`, plus the board-card content contract in
`web/conversation/tests/components/work.test.tsx`. The existing Cloud board
browser fixture checks split-footer containment and grouping at
240/260/320/400 CSS pixels across densities and themes; browser review checks
one-line status and effort visibility across densities.

## INV-14 — Workers never wait on a human question

Workers receive no `ask_human_question` tool. Dispatch and completion do not
consult legacy `human_questions` rows. Those rows remain readable historical
receipts; Detent neither writes them nor projects them as active waits.

A real product decision or physical action uses the current canonical
Workpad's structured `detent-status` block with `status: blocked` and a
concrete `human_action`. Native workers forbidden to write Workpad comments
report through the authenticated final outcome; the existing native completion
owner settles human attention in Blocked. Prose cannot replace authoritative
typed status or fabricate a landed version.

Only an authorized newer Workpad clears the action after it is performed;
ordinary replies do not. Existing recovery owns the return to executable work.
Legacy generated prerequisites record the action on each dependent before
removing the old dependency. Infrastructure and forge failures remain
instance-owned. Human approvals, operator pins, protections and actual backend
approval declines retain their authority.

**Enforcement:** `TestINV14DispatchQuestionTool`,
`TestINV14LegacyQuestionRowDispatch`,
`TestFirstHumanBlockerCompletionReachesBlocked`,
`TestWorkpadHumanActionSnapshot`, and
`TestWorkpadHumanActionClearanceRecoversBlockedIssue` in
`internal/orchestrator`; `TestLegacyHumanQuestionReceiptsReadable` in
`internal/store`; `TestOperationsWorkpadHumanAction` in `internal/web`;
`TestBoardCardSignalBudget` in `internal/web/templates`.

## INV-15 — Visible UI changes require a human-authored issue

Every user-facing surface is covered: the Cloud app (`web/conversation`),
Hub server-rendered pages and the local Templ dashboard. A change may add
visible elements or content only when a human-authored issue names the surface
or feature they belong to. A human-authored feature issue authorizes the whole
surface it describes, including the rows, controls, states, empty and error
copy and responsive layout that surface needs; it does not have to enumerate
each element. Machine-filed issues, agent-expanded scope and "while I was
here" additions never qualify.

Removing UI or fixing an existing element in place without adding visible
content does not need that approval. Agent diagnostics, coverage, provenance
and debugging data belong in existing API and MCP reads and logs. Never add
UI to make them observable, including on Diagnostics, behind toggles or debug
flags. Agents describe proposed UI additions in their outcome or file Backlog
work for a human to author or rewrite; they do not build them.

**Enforcement:** Review applies [AGENTS.md](../AGENTS.md#implementation),
[CLAUDE.md](../CLAUDE.md#workflow) and the
[PR template](../.github/PULL_REQUEST_TEMPLATE.md). No additional gate,
configuration, reason code or UI surface enforces this rule.

## INV-16 — The tracker database is the only shared knowledge channel

Agents, runs and issues share knowledge only through the selected tracker
database: issue bodies, comments, the Workpad, Change/PR records and run
history. Agents never use repository files to pass knowledge between issues
or runs. The repository holds product source and normative documentation,
never a shared log or notebook.

Per-change evidence, rationale and verification go in the Change/PR description
or issue comments through the authorized tracker owner. No change appends
evidence, notes or history to a shared repository file. No repository file is
append-only by convention. Edit documentation to state the current rule or
contract; do not accumulate per-change narratives. Historical evidence remains
recoverable through git history.

Only a change to an invariant's rule or its enforcing check edits
`docs/invariants.md`. Following an unchanged invariant requires no edit, and
a change that edits the document without changing a rule or check fails
review. Implementation detail that moves with ordinary feature work belongs
in the enforcing tests and package documentation, not in an invariant.
The current completion contract owns tracker publication; this rule grants
workers no additional write authority.

**Enforcement:** Review applies [AGENTS.md](../AGENTS.md#repository-invariants),
[CLAUDE.md](../CLAUDE.md#repository-invariants) and the evidence instructions in
the [PR template](../.github/PULL_REQUEST_TEMPLATE.md). This is a review rule;
it adds no check, gate, configuration or runtime mechanism.

## INV-17 — Runners are stateless, like GitHub Actions runners

A runner needs only its install configuration: the Hub URL, its identity, a
capacity limit and one workspace root that it owns. Nothing else about the
machine may change which work it can claim or how a job behaves: not other
directories, symlinks, existing checkouts, shell profiles, tool logins or
files left behind by earlier jobs.

Every job starts in a workspace the runner prepares itself under its workspace
root, from an exact commit or a Hub-stored source bundle. Work a job must keep
(commits, uncommitted changes, plan and validation receipts) is published to
the Hub when the job checkpoints or ends; the copy on the machine is only a
cache. Any eligible runner can claim any item. No item is pinned to the machine
that last ran it, and losing a machine loses no work.

Verified execution artifacts are immutable versioned caches. The existing startup and update owner
resolves and verifies releases; new attempts acquire its latest prepared compatible
artifact without release discovery. Discovery failures preserve that verified
selection, never authorize an unchecked binary, and never clear an explicit
critical or incompatible update refusal. A captured artifact remains owned
until its workers and cleanup finish; only then may an obsolete artifact be
removed. Execution processes access local session state through the controller's
bounded typed protocol, never concurrent SQLite migrations or independent store
writers. The supported protocol and lifecycle are documented in
[ephemeral execution workers](development.md#ephemeral-execution-workers).

A change may not add machine-local state that later jobs depend on, discover
configuration by scanning the machine, or route work by where earlier work
ran. A failure caused by machine-local state is fixed by publishing that state
to the Hub or removing the dependency, never by adding pinning, transfer or
recovery logic. Existing local-only checkpoints, source-owner pinning and
source transfer are being removed under this rule; no change may extend them.

**Enforcement:** Review applies this rule to changes in runner, workspace,
hubclient, recovery and placement code. This is a review rule; it adds no
check, gate, configuration or runtime mechanism.

## INV-18 — Hub writes stay small

A request's write transaction writes only the rows that request owns. It never
rebuilds rollups, recounts totals, cleans up, backfills or reprojects other
rows. Derived data is maintained by background work, one item per short
transaction, and may lag the request that changed it.

Work inside any write transaction is bounded by the size of the request, never
by the size of history. Reads use the reader pool, never the writer
connection.

New Hub tables and columns store typed columns, never JSON documents. Existing
JSON columns are being migrated to typed columns under this rule, and the
in-commit board card refresh is being moved to background work; no change may
extend either.

**Enforcement:** `TestINV18RequestCommitDoesNoDerivedWork` and
`TestINV18HubMigrationsUseTypedColumns` in `internal/hubserver`. Review applies
the history bound and reader rule to Hub persistence changes.

## INV-19 — No flaky tests

A test that both passes and fails on the same commit is a defect, reported at
High. Its owner fixes the cause or deletes the test the day it is found. An
intermittent failure is not "cannot reproduce": diagnose it from the failing
run's evidence (logs, captured snapshots and artifacts) or by repeating it under
CI load, then fix the product or test cause.

A flake is never accommodated: no added retries, sleeps or fixed waits, longer
timeouts, skips, `fixme` or expected-failure marks, or quarantine. CI keeps
Playwright's `failOnFlakyTests` on with at most one retry, so a test that only
passes on retry still fails the run and is reported.

**Enforcement:** `TestINV19NoFlakeAccommodation` in `internal/invariants`
checks the Playwright CI settings, that no browser spec skips, marks or retries
a test, and that no Go test skips itself as flaky.
