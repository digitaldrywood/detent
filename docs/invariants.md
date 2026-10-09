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

The lane ledger owns `UpdateIssueState` and state-field `SetIssueField` calls.
Enumerated connector adapters implement or forward those operations without
deciding transitions. Admission uses the injected orchestrator writer. Raw
HTTP tracker writes must not bypass this ownership boundary.

For native projects, a supplied repository workflow definition owns the state
and transition model through the existing trusted-revision policy approval
path. Approval applies the resolved model atomically and rejects removal of
occupied states. Cloud Markdown authoring is available only without repository
definition authority; UI, API and MCP cannot independently override an approved
repository workflow. Definition authority does not transfer issue-move ownership.

**Enforcement:** `TestRepositorySources` in `internal/invariants`;
`TestAdmissionRequiresLedgerWriter` in `internal/admission`. Review rejects
lane-write bypasses and new ownership exceptions.

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
to the workflow owner: conflicts and command gate failures prefer configured
Rework, other refusals prefer the configured review lane, and terminal,
operator-only and self-transitions are excluded. Recovery and Rework resume
only from a verified preserved source checkpoint; unknown changes and
unavailable authority are refusals, never checkpoint rewrites or relaxed
comparisons. Human and permission holds retain their authority.

**Enforcement:** `TestPreTurnFailuresDrainInstance`,
`TestObservedLanePreTurnFailureRemainsInstanceOwned`,
`TestWorkspacePreparationDrainsInstanceAndPreservesIssueFailureBreakers`,
`TestClassifyWorkspaceForgeReadFailure`,
`TestWorkspaceSSHRefusalDoesNotTripProjectBreaker`,
`TestRecoverDurableWorkspaceGitReadWait`,
`TestWorkspaceDiskExhaustionRetriesWithoutProjectBreaker`, and
`TestNativeLandingRunCompletion` and `TestNativeChangeRunCompletion`
in `internal/orchestrator`;
`TestNativeExecutionSettlesFinishedRun` in `internal/hubclient`;
`TestClaimCandidatesRequireUnansweredWorkItem` in `internal/hubserver`;
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

**Enforcement:** `TestRepositorySources` and `TestSourceViolations` in
`internal/invariants`. Review verifies removal or consolidation.

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

This repository starts no validation workflow from `pull_request`,
`pull_request_target` or `merge_group`, and requires no branch status check.
The sole event exception is `.github/workflows/cla.yml`: its single job runs
only `contributor-assistant/github-action` without checking out or executing
PR code, and owns signature records on the unprotected `cla-signatures` branch.

Source completion runs `make check-land` and fixes failures before submission.
The finalized immutable native version carries its successful command receipt
for the exact reviewed head and tree. Native direct landing verifies that receipt,
squashes clean Changes in queue order onto one staging head, and pushes the
batch without repeating validation or calling a GitHub API. A conflicting
member leaves the clean batch and retries by rebasing; in `per_landing` mode the rebased tree
runs build, short Go tests for touched packages, and lint of touched files.
The receipt records the landing path and tested packages. A moved base repeats
the clean-squash decision on the new head. GitHub PR landing stays behind its
project policy opt-in and retains its configured validation and native checks.
The approved `rolling_barrier` mode skips landing and merge-resolution commands,
running the configured gate on the base tip outside the source lock after landings.
Landing continues during validation; a red barrier stops repository landing
except for its native repair issue. Repair proceeds forward without auto-revert,
and a green barrier reopens landing. A failed per-landing command returns to
configured Rework with the failing output. The project publishes no local status and sets
`gate.required_status_checks: []` and `gate.automated_review: "off"`; ordinary
merge waits for no additional CI producer or scheduled validation, while
reported failed CI, native base-branch requirements and reported P1 findings
remain authoritative. Diagnostics support concurrent worktrees without a shared
validation lock. Other projects retain their configured policies.

Landing checks repository source and workflow invariants; the full behavioral
invariant manifest runs only in the scheduled suite.
Full integration, race, coverage, fuzz, NilAway and generated-output validation
run in the scheduled suite, which pins the current `develop` SHA. Each new SHA
receives one full suite across scheduled and manual dispatch runs. Preflight
skips a SHA that has a completed non-cancelled run of this workflow, regardless
of event or conclusion; manual dispatch with `force: true` runs it again, and
any non-`none` `fail_job` selection implies force. Only the successful full-suite
finalizer publishes `scheduled-full-ci`, creates an annotated validated patch
tag and dispatches release; release deploys staging, runs its smoke, then
deploys production and runs its smoke, and a failed smoke stops promotion. An
already validated commit receives no new stable tag or deploy. Operator-landed
Cloud deploys may dispatch the existing release workflow on a pinned develop commit:
its successful `make check-land` Actions check is authenticated by the existing
provenance owner, and the workflow publishes a signed next-patch `op.<sha12>`
prerelease before deploying the same version and binary through staging and
production. These prereleases do not replace stable installer or package feeds;
enrolled runners follow the Hub through the existing coordinated update owner.
Every failing scheduled job is reported through the
selected native owner at least High into Todo under a stable job fingerprint,
following [deployment and release failure reporting](../AGENTS.md#deployment-and-release-failure-reporting);
infrastructure reports carry the infrastructure label and CI-instance
attribution, and failed Cloud publication never falls back to GitHub issue
writes. Plain checkouts compile and test without ignored assets; build owners
prepare conversation bundles and configuration references before deployment.

**Enforcement:** `TestRepositoryWorkflow`,
`TestRepositoryHasNoPullRequestActions`, and `TestWorkflowViolations` in
`internal/invariants`; `TestMakeCheckFastOverlapsWorktrees` in
`tools/checklock`; `TestParseProblems`, `TestReport`, `TestCloudReport`,
`TestCloudDestinationAuthority`, and `TestScheduledFinalizer` in
`tools/cifailure`. Review preserves project-specific policy and release authority.

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
A newly red rolling landing barrier requires an open repair owner: it reuses an
open match, or creates a fresh repair when only terminal repairs match, keeping
that terminal history intact.
Occurrence evidence identifies runs, attempts, commits and jobs without
changing problem identity.

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
`TestNativeMachineDefectOccurrences` and `TestRollingLandingBarrier` in `internal/hubserver` and
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

Server-side MCP confirmation modes, YOLO, pending approval actions and the
`/chat/approval` page stay retired. The Hub enforces key scope, project access and
current grants on every call; authorized calls execute directly. Clients own
confirmation. Luna asks inline by default using its per-user client preference.
Removing a forbidden symbol from a list does not authorize its resurrection.

**Enforcement:** `TestRepositorySources`, `TestSourceViolations`, and
`TestMutationDocuments` in `internal/invariants` reject retired identifiers,
assembled constants and GraphQL mutation budget queries, including aliases
and fragments. INV-2's behavioral checks preserve infrastructure attribution.
Review covers novel names, reflection and generated semantic equivalents.

## INV-10 — Priority only picks the next job

Priority picks the next job. Free capacity is never held, reserved or kept idle
for a project, lane or priority. When a slot frees, dispatch the highest-ranked
READY request; if higher-ranked projects have nothing ready, dispatch whatever
is ready. The system never cancels, stops or preempts running work; only a user
cancels work.

Pending requests own no capacity. Submission, release and capacity changes
share real-slot acquisition with atomic project, lane and host ceilings.
Eligible hosts remain alternatives until acquisition; retry host preference
must not strand available capacity. Refreshes retain and update standing
requests; acquisition revalidates current eligibility. Authorization,
dependencies, retry readiness, local ceilings and stop exclusions remain
authoritative. Admission filters eligible candidates before acquiring capacity.

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

## INV-12 — Native toolchain caches

Workers inherit the host's native toolchain caches. Detent may trim a native
cache through mechanisms the toolchain tolerates, but never relocates it,
keys it per project or attempt, or introduces configuration that does either.
Per-attempt isolation is limited to the host-provided `TMPDIR`/`TMP`/`TEMP`;
workers never fall back to host scratch or use the orchestrator's scratch.

The runner must not set `GOCACHE`, `GOMODCACHE`, `GOBIN`,
`GOLANGCI_LINT_CACHE`, `GOFLAGS`, `npm_config_cache`, `PNPM_HOME`,
`YARN_CACHE_FOLDER`, `CARGO_HOME`, `CARGO_TARGET_DIR`, `PIP_CACHE_DIR`,
`UV_CACHE_DIR`, `PLAYWRIGHT_BROWSERS_PATH` or `DOCKER_CONFIG` unless
explicitly passed through by the operator. Preserve host values and explicit
overrides. New `workspace.*cache*` keys are rejected; the removed
`workspace.cache_strategy` is ignored with its compatibility warning.

Sandbox grants use the existing native caches without relocation. Restricted
turns keep cache writes and network access disabled. SSH workers use remote
host caches, scratch and the shared Go admission budget.

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

A change may not add machine-local state that later jobs depend on, discover
configuration by scanning the machine, or route work by where earlier work
ran. A failure caused by machine-local state is fixed by publishing that state
to the Hub or removing the dependency, never by adding pinning, transfer or
recovery logic. Existing local-only checkpoints, source-owner pinning and
source transfer are being removed under this rule; no change may extend them.

**Enforcement:** Review applies this rule to changes in runner, workspace,
hubclient, recovery and placement code. This is a review rule; it adds no
check, gate, configuration or runtime mechanism.
