# Repository invariants

These rules apply to all repository changes. Follow the
[validation policy](../WORKFLOW.md#validation). Only a change to an invariant's
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

INV numbers are permanent identifiers. A retired invariant keeps its number as
a retired entry; numbers are never reused or renumbered.

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
The lane ledger owns state transitions; connector adapters forward them
without deciding, and raw HTTP tracker writes must not bypass that boundary.

Execution ownership is a fenced, renewed lease held by one controller through
outcome publication and lease release. Worker exit alone never frees capacity.
Expiry returns the item to claim eligibility under current source custody and
human intent; no recovery park or park-clearing sweep owns that return.

Blocked has a closed list of causes: an explicit human question or permission
request with a typed `human_action`, or an explicit operator stop. Machine
failures, including validation, missing checks, delivery and retry exhaustion,
retry in a dispatchable lane through the existing retry owner and never
synthesize a human prerequisite. Derived hold metadata requires an owner,
source revision and expiry; wiping it and recomputing must produce the same
dispatch decision.

For native projects, a supplied repository workflow definition owns the state
and transition model; UI, API and MCP cannot override a repository-controlled
workflow, and definition authority does not transfer issue-move ownership.

**Lease enforcement:** `TestRetainedWorkAttemptsExpireOnTick` and
`TestHubSchedulingReadinessBeforeClaim` in `internal/orchestrator` and `TestTimeoutWorkAttemptsProcessEvidence` and
`TestStartupTimeoutWorkAttemptsScope` in `internal/store`.

**Closed-list enforcement:** `TestBlockedRecoveryRecomputesDispatchWithoutDerivedHolds`
and `TestStoredHoldRequiresProvenance` in `internal/orchestrator`, plus the
lane writer's typed-intent and metadata validation.

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

Machine failures have one retry owner. Per-issue failure, progress, spend,
dispatch-loop, token-ceiling and workspace retry brakes are consolidated into
it; machine failures stay dispatchable with growing attempts and capped
jittered backoff. A retry resumes at the first step whose receipt is no
longer valid for the current source, policy and review state; it never repeats
a step whose receipt is still valid. A rejected review, gate or audit result with any change-caused or unknown
finding, or new human feedback, returns work to implementation on the same
source. An outcome proven entirely independent of the change waits on typed
repair dependencies in a dispatchable lane, then resumes through this same
retry owner at the first invalid receipt.
Only verified changed source resets the attempt count.
Spending limits belong to the existing budget policy, not to retry brakes;
historical brake metadata never vetoes the next attempt, and explicit human
action and operator stops retain their authority.

**Enforcement:** `TestRepositorySources` and `TestSourceViolations` in
`internal/invariants`; `TestChunkCoverage`, `TestLargeAuditAggregation` and
`TestChunkSourceAndContinuation` in `internal/securityaudit`, and
`TestRunnerChunkedAudit` in `internal/runner`. Review verifies removal or
consolidation.

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
their chosen settings. Queue ownership belongs to the current PR head, and
Detent never bypasses a configured queue with a programmatic merge.

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

## INV-5 — Local validation and release evidence

GitHub Actions runs no validation. No workflow triggers on `pull_request`,
`merge_group` or `schedule`, and no workflow step runs Go tests, lint, vet,
security scans or browser journeys. The only pull-request event is
`.github/workflows/cla.yml` on `pull_request_target`, which runs only
`contributor-assistant/github-action` without checking out or executing PR
code. This repository requires no branch status check.

Each Change passes the finalization gate (`make check-landing`) on its own
reviewed head before landing. Develop accepts integration risk between Changes
that merge cleanly; `scripts/release-validate-local.sh develop` runs the full
suite, `make check-release`, on our own hardware through `detent validate`, so
passing Go test binaries are reused from Hub receipts, and posts
`develop-full-ci` on the exact commit. Ordinary landing waits for no additional
CI producer or full-suite validation.

A Go test receipt reuses a pass only for a hermetic test binary: one whose
complete input set is the binary, its flags, its package directory relative to
the checkout, the Go runtime environment, and the files and environment
variables it observably reads. A binary that starts a subprocess, opens a
missing relative path, reads an input that disappeared or changed while it ran,
reads a path outside the checkout, GOROOT and temporary directories, or whose
package source changed while it ran is never recorded. Code that runs before
Go's test log starts (`init`, `TestMain`, package variables and the package
functions and methods they reach), inspected in every package file regardless
of build constraints, is held to the same rule: a file read, glob or process
start there prevents recording, named environment reads there join the input
set, and an unnamed or shadowed read there or an environment listing anywhere
in the package keys the whole environment in its observed order. Read files
are fingerprinted by mode and content; stat-only inputs by size and mode, plus
modification time for temporary files, because a fresh checkout's timestamps
carry no meaning. Checkout and temporary directory prefixes in environment
values are normalized only to their length, so relocating to an equal-length
path is the only difference a receipt ignores. Like Go's own test cache,
receipts do not observe `Readlink` or `syscall.Getenv` during tests,
modification-time assertions on checkout files, the length of the path
`syscall.Getwd` returns, or network, clock and host state other than files and
environment; a test whose result depends on them is a defect under the
repository test rules, which already forbid ambient state, running processes
and real time.

Only a green `make check-release` on the exact release branch head produces
release evidence: `scripts/release-validate-local.sh release/vX.Y` posts
`scheduled-full-ci` on that head, and develop evidence is never reused after
cherry-picks. Every release starts from a frozen develop snapshot on a release
branch, fixes land on develop first and are cherry-picked with `-x` through
release-branch PRs, and no develop prerelease or source-gate-only deployment
path exists. Stable tags are created only by `release-finalize.yml` in GitHub
Actions, after the latest `scheduled-full-ci` and `release-security-audit`
statuses on the current release head are successful, and the latest
`platform-full-ci` status on the nearest validated ancestor (including the head)
is successful and no older than 24 hours. Platform validation runs daily on
local Colima, using the validated source and pinned toolchain. Release API calls belong
to operators and GitHub Actions, never Detent runners. The release flow is
specified in [the release workflow](../WORKFLOW.md#release).

**Enforcement:** checks assert outcomes only: `ci.yml`,
`windows-tests.yml` and `portability-stress.yml` do not exist; no workflow
triggers on `pull_request`, `merge_group` or `schedule`, `pull_request_target`
is limited to the `cla.yml` exception, and no workflow step runs validation
commands; `release-finalize.yml` triggers only on `workflow_dispatch` and runs
`scripts/release-finalize.sh`; the finalizer tags only when every required
status is green on the exact current release head and never redispatches a tag
whose provenance no longer matches; and `release.yml` verifies provenance of the
exact tagged commit before building.
`TestRepositoryWorkflow`, `TestRepositoryHasNoPullRequestActions`, and
`TestWorkflowViolations` in `internal/invariants`;
`TestReleaseWorkflowAuthenticatesExactCommitProvenance` in the repository root;
`TestValidateCommandReusesGoTestReceiptsAcrossCheckouts` in `internal/cli`;
`TestRunWrapperExecutesWhenUnobservedInputsChange`, `TestPreWindowInputs`,
`TestParseTestLog`, `TestKeySeparatesArguments`, `TestSnapshotTempEntries`,
`TestEnvironmentListingKeepsOrder` and `TestWrapperRejectsUnobservedInputs` in
`internal/gotestreceipt`;
`TestMakeCheckFastOverlapsWorktrees` in `tools/checklock`; `TestParseProblems`,
`TestReport`, `TestCloudReport`, `TestCloudDestinationAuthority`,
`TestReleaseStabilization`, and `TestReleaseFinalizer` in `tools/cifailure`.
Review preserves project-specific policy and release authority.

## INV-6 — Isolated agent home

Workers never inherit the operator's agent instructions or write to the
operator's transcript storage, for any backend. Repository instructions and
the Detent-provided worktree remain authoritative. Codex workers run with an
isolated Codex home; resuming a persisted thread copies only that thread into
the isolated profile and leaves the host copy unchanged.

**Enforcement:** `TestPrepareCodexCommandForServiceIsolatesInstructions`,
`TestPrepareWorkerCodexHomeExistingInstructions`, and
`TestPrepareLegacyCodexRollout` in `internal/cli`;
`TestAppServerThreadPreparation` in `internal/codex`. Review applies the rule
to other backends.

## INV-7 — Machine issue identity

Machine-filed issues carry an origin stamp and a problem fingerprint. A new
report of an open problem becomes an occurrence on that issue, never a
duplicate. A report matching a closed issue is ignored, unless scheduled
validation shows a fixed problem failing again, which starts a new issue. A
human's Cancelled or no-dispatch decision is never overridden. Interactive
native filing first returns likely duplicates and related items, and creates
nothing until the filer explicitly confirms the issue is distinct; machine
reports and supported imports deduplicate by fingerprint instead.

**Enforcement:** `TestMachineIssueTool` in `internal/orchestrator`;
`TestNativeMachineDefectOccurrences` and `TestNativeReleaseStabilizationOccurrences` in `internal/hubserver` and
`TestDefectFingerprint` in `internal/issueorigin`;
`TestManagerPreservesClosedFinding` in
`internal/intake`; `TestNativeMachineIntakeAuthorityAndFingerprint` in
`internal/hubclient`; `TestParseProblems`, `TestReport`, and `TestCloudReport`
in `tools/cifailure`. Review checks fingerprint stability.

## INV-8 — No strict freshness protection

This repository does not require strict up-to-date branch protection on
branches Detent merges into. Managed projects may require strict freshness;
Detent honors their branch rules. Code changes do not authorize live settings
changes. Concurrent changes resolve through ordinary Git conflict handling and
regenerated output, never landing-time renumbering or shared version edits.

**Enforcement:** `TestDoctorRespectsProjectCIPolicy` in `internal/cli`;
`TestIndividuallyValidBranchesRejectIntegratedCollision` in
`tools/migrationcheck`; `make check-migrations`. Doctor reports live branch
protection as project evidence; the operator verifies live settings.

## INV-9 — Retired mechanisms

These stay retired, and runtime code must not restore them under any name:
lane revocation, indeterminate-lane stops, per-issue infrastructure parking,
root-level `rateLimit` in mutation documents (obtain budget information through
a separate query), the rolling landing barrier with its Red landing hold,
repair, bisection and automatic revert owners, uncertain-publication and
`manual_recovery` holds, repository-policy approval as a claim or publication
gate, server-side MCP confirmation modes, YOLO, pending approval actions and the
`/chat/approval` page, historical external-blocker answers, per-item answer
cooldowns, recorded-dependency recovery fences, sticky reason strings, consumed
recovery signatures, generic managed/fingerprint and cross-host park vetoes,
and remembered review-conversation refusals.

Current facts replace them: publication reconciles the remote ref against the
intended SHA, typed dependencies and current review/check evidence own
readiness, and the Hub enforces key scope, project access and current grants on
every call. Historical tables, migrations and legacy decoding remain readable;
they do not authorize restarting a retired owner. Documentation and negative
fixtures may name retired mechanisms, and removing a forbidden symbol from a
list does not authorize its resurrection.

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
`TestAttemptRecoveryAfterPolicyChange` and
`TestOrphanRecoveryRetainsPolicyAcrossHubTransition` in `internal/orchestrator`
preserve retry availability and pinned continuation identity after policy changes.
`TestNativeDerivedHoldDispatchEquivalence` and
`TestReviewConversationHistoryDoesNotVetoLanding` in `internal/hubserver`,
`TestDerivedHoldDispatchEquivalence` in `internal/orchestrator`, and the retired
symbol checks in `internal/invariants` cover the retired vetoes. Review covers
novel names, reflection and generated semantic equivalents.

## INV-10 — Priority only picks the next job

Priority picks the next job. Free capacity is never held, reserved or kept idle
for a project, lane or priority. When a slot frees, dispatch the highest-ranked
READY request; if higher-ranked projects have nothing ready, dispatch whatever
is ready. The system never cancels, stops or preempts running work; only a user
cancels work.

Paid project Sprite capacity is the exception: project allocations keep paid
capacity with its configured project unless the pool explicitly opts in to
sharing idle capacity. Compatible releases and ordinary policy changes never
drain a runner or withhold its free slots; only a critical security release, a
concrete incompatibility or intentional operator maintenance uses the existing
drain owner. Native claim eligibility has one candidate owner, recomputed from
current facts, and admission reads never change eligibility.

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
`TestWorkerArtifactsRoutineUpdatePaths` and `TestSchedulerDrainPolicy` in
`internal/update` and `TestLocalWorkerReleaseAndPolicyCoexistence` in
`internal/cli` enforce compatible update capacity and exceptional-only drains.
Review rejects semantic reservation and preemption substitutes.

## INV-11 — Retired

Human mechanism scope approval before Todo is retired. Mechanism growth is
governed by INV-3, and visible UI scope by INV-15.

## INV-12 — Native toolchain caches

Workers inherit the host's native toolchain caches; cache contents may affect
speed but never correctness for runner jobs. An enrolled local Cloud runner
manages the host Go build cache resolved by `go env GOCACHE`, including
`GOCACHE` and `go env -w` settings, using its existing cache program, job scratch
pins, admission and eviction protocol. Age expiry removes entries whose
last-use mtime is older than `max_age`, at least 25 hours. Size eviction of
the managed cache reclaims unpinned entries whose last-use mtime is more than
3 hours old, oldest first toward the budget; the window covers Go's hourly
mtime refresh plus native builds of up to two hours. Caches without the cache
program keep a 25-hour size-eviction window. Size budgets and disk pressure
never evict entries within the window, and pinned outputs remain available
until job scratch cleanup. Bursts inside the window may grow the shared cache above its budget;
the budget is not a hard size cap. Runtime
stores share maintenance and the cache program without exclusive ownership;
a persistent owner marker never prevents a second or replacement runtime store
from participating. Native Go consumers share the same gentle trimming policy, with the same best-effort guarantee as Go's own trimming: an entry older than the window that a native build reuses at the instant it is removed can still be lost.
The previous `detent/go-build` directory is neither created nor deleted.
No cache is keyed per project or attempt, and no new cache configuration is
introduced; new `workspace.*cache*` keys are rejected. Per-attempt isolation is
limited to the host-provided `TMPDIR`/`TMP`/`TEMP`.

Apart from the resolved build cache, its cache program, the shared Go
admission budget's `-p`, `-trimpath` and `-toolexec` defaults, and the go test
receipt wrapper's `-trimpath` and `-exec` in gate and validator commands, the
runner must not set toolchain cache, home or flag variables (such as
`GOMODCACHE`, `GOFLAGS`, `npm_config_cache`, `CARGO_HOME` or `PIP_CACHE_DIR`)
unless the operator explicitly passes them through. Host values and explicit
overrides, including an operator-provided `GOCACHEPROG`, are preserved.

**Enforcement:** `TestINV12WorkerInheritsToolchainEnvironment`,
`TestINV12BackendToolchainEnvironment` in `internal/runner`;
`TestBackendAppliesRunnerIsolation` in `internal/codex`;
`TestINV12WorkspaceCacheKeys` in `internal/config`;
`TestINV12DoctorNativeCaches`, `TestINV12DoctorWorkspaceKinds`,
`TestSSHProbeRequiresProvidedScratch`, and
`TestSSHLocalTargetIntegration` in `internal/cli`;
`TestINV12TrimReadout` in `internal/toolcache`;
`TestRunReviewCommandReusesGoTestReceiptsAcrossWorkspaces` in `internal/workspace`.

## INV-13 — A board card is a title and one status line

A board card renders its identity row, the title, at most one status line and
the existing priority controls. The status line contains at most 48 Unicode
characters and names the wait in actionable words. Scheduler evidence, tracker
ages, absolute timestamps, token counts, fact grids and diagnostic text are not
card content; existing observations belong in the detail sheet and hover
titles, subject to INV-15. Adding card body content or lengthening the status
line changes this rule. The card layout is specified in the
[design system](design-system/patterns.md#work-board-and-issues).

**Enforcement:** `TestINV13BoardCardContent`,
`TestINV13SheetObservations`, and `TestBoardCardSignalBudget` in
`internal/web/templates`, plus the board-card content contract in
`web/conversation/tests/components/work.test.tsx`. The existing Cloud board
browser fixture checks split-footer containment and grouping across densities
and themes; browser review checks one-line status and effort visibility.

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

A human action is cleared only by an authorized human, either through a newer
Workpad or by moving the item out of Blocked through the transition owner; the
answer is recorded on the item when one was given. Ordinary replies never clear
it. Existing recovery owns the return to executable work. Infrastructure and
forge failures remain instance-owned. Human approvals, operator pins,
protections and actual backend approval declines retain their authority.

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

**Enforcement:** Review applies [AGENTS.md](../AGENTS.md#ui) and the
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

**Enforcement:** Review applies [AGENTS.md](../AGENTS.md#repository-invariants) and the evidence
instructions in the [PR template](../.github/PULL_REQUEST_TEMPLATE.md). This is a review rule;
it adds no check, gate, configuration or runtime mechanism.

## INV-17 — Runners are stateless, like GitHub Actions runners

A runner needs only its install configuration: the Hub URL, its identity, a
capacity limit, one workspace root that it owns, and a declared GitHub
credential: a configured worker credential or, when none is configured, the
host's authenticated GitHub CLI, whose login jobs then use. Nothing else about the machine may change which work it can
claim or how a job behaves: not other directories, symlinks, existing
checkouts, shell profiles, other tool logins or files left behind by earlier
jobs. A missing or expired declared credential is an instance setup failure.

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
writers.

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
connection. Stored data shape is governed by
[INV-20](#inv-20--sql-never-reads-or-writes-inside-stored-json): new Hub
columns are typed, never JSON.

**Enforcement:** `TestINV18RequestCommitDoesNoDerivedWork` and
`TestINV18HubMigrationsUseTypedColumns` in `internal/hubserver`. Review applies
the history bound and reader rule to Hub persistence changes.

## INV-19 — No flaky tests

A test that both passes and fails on the same commit is a defect, reported at
High. Its owner fixes the cause or deletes the test. An intermittent failure is
not "cannot reproduce": diagnose it from the failing run's evidence (logs,
captured snapshots and artifacts) or by repeating it under CI load, then fix
the product or test cause.

A flake is never accommodated: no added retries, sleeps or fixed waits, longer
timeouts, skips, `fixme` or expected-failure marks, or quarantine. A test's own
waits and timeouts never grow to hide a flake; a suite's overall execution
budget may be sized from measured runtime. CI keeps Playwright's
`failOnFlakyTests` on with at most one retry, so a test that only passes on
retry still fails the run and is reported.

**Enforcement:** `TestINV19NoFlakeAccommodation` in `internal/invariants`
checks the Playwright CI settings, that no browser spec skips, marks or retries
a test, and that no Go test skips itself as flaky.

## INV-20 — SQL never reads or writes inside stored JSON

Hub and store SQL never use JSON functions or operators over stored data:
`json`, `jsonb`, `json_extract`, `->`, `->>`, `json_each`, `json_tree`, `json_set`, `json_insert`,
`json_replace`, `json_patch`, `json_remove`, `json_object`, `json_array`,
`json_group_*`, `json_valid`, `json_type` or `json_array_length`. This includes
constructing JSON from stored columns. Every field SQL filters, joins, orders,
aggregates, projects or updates is a typed column.

Hub rows hold typed columns only. Audit, history and runtime payloads (attempt
detail, run events, checkpoints, evidence, conversation streams and similar
records kept for audit or history) are written compressed to object storage at
write time, and the row keeps only a typed reference to the object. JSON and
other payload blobs are never stored inline in Hub rows, and no derived table
copies them. Existing inline payload columns are migration debt to move to object
storage; no change may add one, and a change that touches one moves it.

**Enforcement:** Review enforces payload placement in object storage. `TestINV20NoSQLOverJSON` in `internal/hubserver` scans non-test
Go files in Hub and store (including generated SQL), and `db/queries` SQL sources.
Per-file function counts must equal `testdata/inv20_json_sql_baseline.txt`;
reductions must lower the baseline in the same change, and increases are forbidden.
Migrations from the INV-18 adoption cutoff (`20261015000000`) are also checked;
existing calls are baseline debt and new migrations have no allowance. The scan
conservatively counts function calls even on parameters or literals. It does not
recognize `->` or `->>` operators, to avoid false positives outside SQL strings;
review enforces these operators and conversion of edited queries to typed columns.

## INV-21 — Designs hold at ten thousand tenants and runners

Every design is judged at 10,000 tenants and 10,000 runners, with up to 1,000
runners, 100 projects and 1,000,000 attempts of history in one tenant. A choice
that only works at today's size is a defect.

Work per request is bounded by the size of that request. Work per periodic tick
is bounded by a fixed budget and resumes on the next tick. For a fixed request size or tick budget, the number of items processed does not grow with total tenant count, runner count, item count or history. No request or background job
holds a lock, connection or transaction shared by other requests across
per-item network calls or a walk over many objects; such work takes the shared
resource per item, briefly. Each heartbeat, poll, retry and reconnect processes a bounded number of items:
heartbeats, polls, retries and reconnects use capped jittered intervals, and the
control plane applies per-tenant and per-runner admission limits, so one tenant
or a synchronized fleet cannot starve the others.

**Enforcement:** a Change that adds or modifies a hot path (per-request,
per-heartbeat or per-event code), a shared lock, or a periodic job states its
cost at INV-21 scale in its description, for example "per heartbeat: one row
read and one row written, no history reads" or "per tick: at most 500 rows, lock
held around one object". Review rejects such a Change without that statement or
with unbounded scans or fan-out over history or the fleet. Hot-path changes attach
benchmark evidence where a benchmark exists. A manually run scale soak against a seeded
large tenant and a simulated runner fleet provides release evidence when it is
run; it never blocks merging or releasing.

## INV-22 — Tests never use production credentials or systems

No test, fixture, harness, smoke run or development environment uses an active
production credential or reaches a production system. A production credential
is one that consumes real usage or quota, or acts on production data: email
delivery, model provider, production Hub, tracker and deploy keys. Sandbox and
test-mode credentials, such as Stripe test keys or a WorkOS development
environment, are allowed. A production key may stay in a development
environment file only commented out, for reference.

Code under test that reads a credential from the process environment is
isolated from the developer's shell: its test package clears that variable or
injects a fake sender or client before any test runs.

**Enforcement:** a test package whose code under test reads a credential from
the environment clears it in `TestMain` or injects a fake. Review rejects a
Change that enables a production credential in any development, test or CI
environment, or a test that can open a connection to a production endpoint.
