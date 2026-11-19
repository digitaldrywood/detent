# Repository invariants

These rules apply to all repository changes. Follow the
[validation policy](../WORKFLOW.md#validation). Only a change to an invariant's
rule or its enforcing check edits this document. Identify affected INV IDs in
the Change description; put per-change rationale, evidence and verification
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

The Hub’s orchestrator alone applies lane transitions and grants one fenced renewable lease per item across phases. Workers submit authenticated outcomes. Lease loss ends their authority and makes the item eligible for continuation from its last recorded phase commit.

Blocked is reserved for an explicit operator stop or a genuine unresolved decision or permission request. Record the request as a typed `human_action`, surface it durably to the human and release executing capacity. Only authorized human action clears the request. Machine failures use the retry loop.

Enforcement: migration in progress under PLAN-ONE-SESSION; existing tests listed below remain until their code is replaced.

**Existing lane and lease checks:** `TestRepositorySources` in `internal/invariants`, `TestRetainedWorkAttemptsExpireOnTick`, `TestHubSchedulingReadinessBeforeClaim`, `TestBlockedRecoveryRecomputesDispatchWithoutDerivedHolds` and `TestStoredHoldRequiresProvenance` in `internal/orchestrator`, and `TestPostgresEntryKeyMoveItem` and `TestPostgresWorkerWorkflowTransitionPersistsFencedEventAndReplays` in `internal/hubserver`. `TestLocalSessionProcessOwnership` in `internal/runner`, `TestNativeDelayedResponsesPreserveSuccessor` in `internal/hubclient/dispatch` and `TestLocalWorkerReleaseAndPolicyCoexistence` in `internal/cli` cover process exit, finalization and release retries.

**Existing workflow authority checks:** `TestRepositoryNativeWorkflowProjection` in `internal/config`, `TestNativeCloudWorkflowFallback` in `internal/project`, and `TestHostedProjectWorkflowConfiguration`, `TestProjectPolicyAuthorizationAndAtomicClaims` and `TestHostedProjectTools` in `internal/hubserver`.

**Existing human-action checks:** `TestINV14DispatchQuestionTool`, `TestINV14LegacyQuestionRowDispatch`, `TestFirstHumanBlockerCompletionReachesBlocked`, `TestWorkpadHumanActionSnapshot` and `TestWorkpadHumanActionClearanceRecoversBlockedIssue` in `internal/orchestrator`.

## INV-2 — Instance-owned infrastructure failures

Merged into [INV-3](#inv-3--net-zero-mechanisms).

## INV-3 — Net-zero mechanisms

Use one execution loop and retry owner. Adding or expanding a protection or recovery mechanism requires removing or consolidating one. Reuse valid phase receipts, repair rejected work in place and decide the next phase from current workflow intent, typed dependencies, source, review, access, lease and capacity. Subscription usage never affects dispatch, claim eligibility, capacity, scheduling or retries.

Infrastructure failures belong to the instance. Retry the failed phase with capped jittered backoff and keep valid work and receipts. Machine failures stay dispatchable. Reconcile uncertain publication against the remote ref. A match with the intended SHA confirms publication, an unchanged or absent ref permits a compare-and-swap push, and a moved ref returns the item to Rework.

Lease renewal proves ownership only, and the existing stall owner ends a quiet provider session and restarts it in place. At the end of each round, an AI verdict judges progress against the issue’s acceptance criteria and open findings, never by commits or activity.

Enforcement: migration in progress under PLAN-ONE-SESSION; existing tests listed below remain until their code is replaced.

**Existing source and consolidation checks:** `TestRepositorySources` and `TestSourceViolations` in
`internal/invariants`; `TestChunkCoverage`, `TestLargeAuditAggregation` and
`TestChunkSourceAndContinuation` in `internal/securityaudit`, and
`TestRunnerChunkedAudit` in `internal/runner`. Review verifies removal or
consolidation.

**Existing retry checks:** `TestMachineFailuresUseRetryOwner`,
`TestMachineRetrySurvivesStoreRestart`, `TestDerivedHoldDispatchEquivalence`,
`TestHandleRunResultResetsRetryAfterProgress`,
`TestBackendCapacityHelperBoundaries` and
`TestPruneBudgetRefusalsReevaluatesDailyCap` in `internal/orchestrator`.
`FuzzSafetyCriticalOrchestratorBoundaries` retains completion-evidence,
capacity resume arithmetic, capped backoff, dispatch ordering and priority-only
real-capacity acquisition coverage. The retired-symbol checks prevent restoring
the deleted per-issue brake owners.

**Existing instance failure checks:** `TestPreTurnFailuresRetryWithoutPausingProject`, `TestObservedLanePreTurnFailureRemainsInstanceOwned`, `TestWorkspacePreparationRetriesIssueWithoutPausingProject`, `TestClassifyWorkspaceForgeReadFailure`, `TestWorkspaceSSHRefusalDoesNotTripProjectBreaker`, `TestRecoverDurableWorkspaceGitReadWait`, `TestWorkspaceDiskExhaustionRetriesWithoutProjectBreaker`, `TestNativeLandingRunCompletion` and `TestNativeChangeRunCompletion` in `internal/orchestrator`. `TestClaimCandidatesRequireUnansweredWorkItem` and `TestClaimCandidatesRetryInstancePreProviderFailures` in `internal/hubserver`, `TestHealthRunnerSetupFindings` in `internal/hubserver/healthsignals`, `TestLocalGitCreateReadsPublishedBranchWithRepositoryCredential` and `TestLocalGitLandChangeViaGitHub` in `internal/workspace`, `TestWorkerCredentialBlockerError` and `TestNativeInterruptedCodeRecoversPersistedSession` in `internal/runner`, `TestNativeExecutionSettlesFinishedRun`, `TestNativeExecutionLandsReviewedVersion` and `TestNativeRunnerOpensChangeAndLeavesDispatch` in `internal/hubclient/nativechange`, and `TestNativePlannerAutomaticHandoff` in `internal/hubclient/planner`.

**Existing eligibility and reconciliation checks:** `TestMutationDocuments` in `internal/invariants`, `TestApprovedPolicyClaimExposure`, `TestNativeDerivedHoldDispatchEquivalence` and `TestReviewConversationHistoryDoesNotVetoLanding` in `internal/hubserver`, `TestNativeRecoveryWipeDerivedHold` and `TestNativePublicationRecoveryOnAnyRunner` in `internal/runner`, and `TestAttemptRecoveryAfterPolicyChange` and `TestOrphanRecoveryRetainsPolicyAcrossHubTransition` in `internal/orchestrator`.

## INV-4 — Native merge queue

Moved to [Delivery policy](../WORKFLOW.md#delivery-policy).

## INV-5 — Local validation and release evidence

Acceptance and release decisions require authentic evidence bound to the exact source and relevant inputs. Reuse it only while those inputs remain valid. [WORKFLOW.md](../WORKFLOW.md#evidence-policy) specifies the commands, independent reviews and release authority.

**Existing evidence and release checks:** `TestRepositoryWorkflow`, `TestRepositoryHasNoPullRequestActions`, and
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

Machine defects have stable provenance and fingerprints. Update the existing incident only when evidence or disposition changes. Respect human cancellation and allow explicitly authorized, already-deduplicated filing without another confirmation. Retain historical occurrences without per-round issue narration.

Enforcement: migration in progress under PLAN-ONE-SESSION; existing tests listed below remain until their code is replaced.

**Existing issue identity checks:** `TestMachineIssueTool` in `internal/orchestrator`;
`TestNativeMachineDefectOccurrences` and `TestNativeReleaseStabilizationOccurrences` in `internal/hubserver` and
`TestDefectFingerprint` in `internal/issueorigin`;
`TestNativeMachineIntakeAuthorityAndFingerprint` in
`internal/hubclient/dispatch`; `TestParseProblems`, `TestReport`, and `TestCloudReport`
in `tools/cifailure`. Review checks fingerprint stability.

## INV-8 — No strict freshness protection

Moved to [Delivery policy](../WORKFLOW.md#delivery-policy).

## INV-9 — Retired mechanisms

Merged into [INV-3](#inv-3--net-zero-mechanisms).

## INV-10 — Priority only picks the next job

Priority chooses the next executable work. Active work keeps its execution slot, and retained item leases reserve no idle execution slots. Enforce configured capacity and access limits and explicit operator stops using current facts.

Enforcement: migration in progress under PLAN-ONE-SESSION; existing tests listed below remain until their code is replaced.

**Existing dispatch and capacity checks:** `TestGlobalDispatchGatePriorityOnlyPicksNextJob`,
`TestGlobalDispatchGateReadyRequests`,
`TestGlobalDispatchGateConcurrentReadyRequests`,
`TestQueuedDispatchRanksIndependentRequests`,
`TestQueuedDispatchPreservesProjectCeilings`,
`TestQueuedDispatchChoosesAvailableHost`, and
`TestStandingDispatchRequestUpdates` in `internal/scheduler`;
`TestRunDispatchesQueuedRequestsWithoutPolling`,
`TestRunDispatchesQueuedRequestsAcrossHostsWithoutPolling`, and
`TestStandingDispatchSurvivesProjectRefresh` in `internal/orchestrator`;
`TestGlobalDispatchGateStrictPriorityDoesNotReserveIdleCapacity` in
`internal/scheduler` and `TestProviderEmptyPreviewReachesClaim` in
`internal/hubclient/dispatch`; `TestRepositorySources` in `internal/invariants`.
`TestNativeAdmissionExplanation`, `TestNativeDependencyReadPermissions`,
`TestNativeRuntimeReadPreservesCurrentEvidence`, `TestNativeRuntimeReadPreservesSharedCapacity`,
`TestHostedAnalyticsReads` and `TestNativeAnalyticsRuntimePopulation` in
`internal/hubserver` enforce scoped current evidence and recorded-read boundaries.
`TestWorkerArtifactsRoutineUpdatePaths` and `TestSchedulerDrainPolicy` in
`internal/update` and `TestLocalWorkerReleaseAndPolicyCoexistence` in
`internal/cli` enforce compatible update capacity and exceptional-only drains.
Review rejects semantic reservation and preemption substitutes.

## INV-11 — Retired

## INV-12 — Native toolchain caches

Use native toolchain caches for speed only, preserving configured environments and active process outputs. Workspace and cache cleanup must preserve live work.

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

Moved to the [work-board contract](design-system/patterns.md#work-board-and-issues).

## INV-14 — Workers never wait on a human question

Merged into [INV-1](#inv-1--lane-ownership).

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

**Enforcement:** Review applies [AGENTS.md](../AGENTS.md#ui). No additional gate,
configuration, reason code or UI surface enforces this rule.

## INV-16 — The Hub carries execution context and Git carries source

<a id="inv-16--the-tracker-database-is-the-only-shared-knowledge-channel"></a>

The Hub carries issue direction, phase receipts, verdicts and findings. Git carries source commits. Agents never carry execution state through repository notes or machine issue-comment streams. Human discussion and final outcomes remain in the tracker.

Enforcement: migration in progress under PLAN-ONE-SESSION; existing tests listed below remain until their code is replaced.

**Existing history and receipt checks:** `TestNativeRuntimeCheckpointsDoNotGrowHistory` and `TestNativeAttemptTerminalActivityReceipts` in `internal/hubclient/runtimeevidence`. Review checks publication ownership against [AGENTS.md](../AGENTS.md#repository-invariants).

## INV-17 — Hub-owned assignment and retained item worktrees

<a id="inv-17--runners-are-stateless-like-github-actions-runners"></a>

The Hub alone owns assignment and execution authority through one fenced, renewable item lease. The runner retains the same worktree across the item’s phases and retries. Each completed phase pushes ordinary commits and records commit-bound receipts in the Hub. After lease loss, a new runner resumes from the last pushed phase commit at the first phase without a valid receipt, and uncommitted work may be lost.

Runners use their declared install configuration. Keep unrelated jobs independent of machine-local state. Never open a local application database, discover configuration by scanning the machine, route work by where it previously ran, or use local files or queues as execution or recovery authority.

Enforcement: migration in progress under PLAN-ONE-SESSION; existing tests listed below remain until their code is replaced.

**Existing workspace and recovery checks:** `TestCheckpointAuthorityAndJournalFailures` and `TestNativePublicationRecoveryOnAnyRunner` in `internal/runner`. Review checks assignment and workspace custody in runner, workspace, Hub client, recovery and placement changes.

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
`TestINV20HubTableLifecycles` checks every application table in the migrated Hub
template against `internal/invariants/table_lifecycle_policy.json`. Each table
declares immutable payload in object storage, derived data with an explicit
retention contract, hot state, or an index. SQLite-owned shadow and system
tables are excluded; new application tables require declarations.

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

Current page content may be projected into typed `page_read_model` tables so
page handlers do not fetch objects. These tables retain only the current page,
record the source revision and generation, source time and projection time,
and are rebuilt asynchronously through `hubmaintenance`. Project pages declare
project scope and use the tenant and project keys; work-item pages also carry
the work-item key. Their typed `body`
column is current display text, never a serialized history payload. JSON and
other serialized payload columns remain prohibited; the frozen legacy payload
list does not expand for read models.

**Enforcement:** Review enforces payload placement in object storage. `TestINV20NoSQLOverJSON` in `internal/hubserver` scans non-test
Go files in Hub and store (including generated SQL), and `db/queries` SQL sources.
Per-file function counts must equal `testdata/inv20_json_sql_baseline.txt`;
reductions must lower the baseline in the same change, and increases are forbidden.
Migrations from the INV-18 adoption cutoff (`20261015000000`) are also checked;
existing calls are baseline debt and new migrations have no allowance. The scan
conservatively counts function calls even on parameters or literals. It does not
recognize `->` or `->>` operators, to avoid false positives outside SQL strings;
review enforces these operators and conversion of edited queries to typed columns.

`TestINV20HubTableLifecycles` also rejects inline history and derived payload
columns unless their exact table and column appear in the frozen
`legacy_payloads` list in `internal/invariants/table_lifecycle_policy.json`.
Each debt entry names its owning conversion family, including families whose
conversion is still pending. This list may only shrink: never add entries,
broaden an entry to a table or pattern, or relabel history as hot state or an
index to bypass it. Removing a payload column requires removing its debt entry
in the same change. `TestTableLifecycleViolations` mutates copies of the migrated PostgreSQL
template to verify unknown tables, inline payloads and stale debt are rejected.
Discovery includes application tables in both the public and platform schemas;
non-public table names are schema-qualified in the lifecycle catalog.

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

Automated tests and development use isolated fixtures and no production credentials or effects. Explicitly authorized operational release checks follow the [release procedure](../WORKFLOW.md#release) and are recorded as production operations.

**Enforcement:** a test package whose code under test reads a credential from
the environment clears it in `TestMain` or injects a fake. Review rejects a
Change that enables a production credential in any development, test or CI
environment, or a test that can open a connection to a production endpoint.

## INV-23 — The tenant is never a process

The tenant is an attribute of a request, a job and a row, never of a process,
binary, physical connection pool, timer or configuration file. One Hub process
serves many organizations: no change starts a process per tenant, creates or
closes a physical pool when an organization is added or removed, runs a timer
or idle goroutine per tenant, or keeps a configuration file per tenant. The
tenant scope is built once from the verified request or the scheduler's
selected tenant, passed explicitly and rechecked where writes commit and
results are published; it is never read from process-wide configuration.
Background work is one shared scheduler per process with bounded pages,
workers and deadlines, and its cost is stated at INV-21 scale. The only
dedicated placement is a shard assignment Cory approves, with a recorded
reason, using the same shard machinery. This invariant incorporates
[the multi-tenancy architecture](architecture/multi-tenancy.md) as normative;
where they differ, this section wins.

**Enforcement:** `TestRepositoryTenancy` in `internal/invariants` is a
syntactic ratchet over non-test Go files in the whole repository. Process
package imports and launch symbols, and physical pool constructors and
factories, are pinned to named sites in
`internal/invariants/tenancy_policy.json`; occurrences in the hosted serving
tree, pool constructors outside `internal/pgtransport`, hosted-path timer
constructors and process-wide organization reads are debt that only shrinks.
Test-support packages are exempt only by name, and only the named scratch
paths and directories the Go toolchain ignores are skipped. Counts must equal
the policy. Landing refuses growth: `scripts/check-tenancy-policy.py`, run by
`make check-land` and the landing gate against the target revision, rejects
any raised debt or exception count and any test-support entry not already in
the target set. `-tenancy-baseline` is a local rewrite convenience, not
protection. `TestTenancyPolicyViolations`, `TestTenancyBaselineRefusesGrowth`
and `scripts/check_tenancy_policy_test.py` test the scanner and the landing
comparison. The ratchet does not follow call graphs, types or runtime
multiplicity, and an empty debt list is not proof the migration is complete;
review applies the remaining rules and the
[review checklist](architecture/multi-tenancy.md#review-checklist).
