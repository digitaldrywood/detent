# Repository invariants

These rules apply to all repository changes. Follow the
[validation policy](../AGENTS.md#validation). Only a change to an invariant's
rule or its enforcing check edits this document. Identify affected INV IDs in
the Change/PR description; put per-change rationale, evidence and verification
there or in issue comments, as required by INV-16. A passing check does not
authorize weakening a rule.

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

**Enforcement:** `TestRepositorySources` in `internal/invariants`;
`TestAdmissionRequiresLedgerWriter` in `internal/admission`. Review rejects
lane-write bypasses and new ownership exceptions.

## INV-2 — Instance-owned infrastructure failures

Infrastructure failures attach to the instance, never to the issue, whether
they happen before the first agent turn or during a turn. Backend startup,
protocol, credential, network and workspace-hook failures do not consume the
issue's failure allowance or authorize source repair. Issue retry accounting
must distinguish genuine code failures from instance interruptions.

Workspace preparation retains the actual failure attribution. A matching
forge host and Git read operation are required to classify a workspace error
as forge unavailability; an unrelated hook failure cannot inherit that
classification. Unknown infrastructure diagnostics remain instance intake.

Native landing conflicts are source refusals, not infrastructure failures or
shipped work. A GitHub HTTP 405 mergeability refusal requires current source
conflict verification before selecting a conflict destination. Clean or
unproven source retains the existing item-local landing continuation.

Landing refusal destinations belong to the workflow owner. Conflicts prefer
configured Rework, then the first allowed dispatchable non-terminal lane.
Other refusals prefer the configured review lane, then the first allowed
non-dispatchable non-terminal lane. Terminal, operator-only and self-transitions
remain excluded; unreachable destinations retain the existing error handoff.

**Enforcement:** `TestPreTurnFailuresDrainInstance`,
`TestObservedLanePreTurnFailureRemainsInstanceOwned`,
`TestWorkspacePreparationDrainsInstanceAndPreservesIssueFailureBreakers`,
`TestClassifyWorkspaceForgeReadFailure`,
`TestWorkspaceSSHRefusalDoesNotTripProjectBreaker`,
`TestRecoverDurableWorkspaceGitReadWait`,
`TestWorkspaceDiskExhaustionRetriesWithoutProjectBreaker`, and
`TestNativeLandingRunCompletion` in `internal/orchestrator`;
`TestLocalGitLandChangeViaGitHub` in `internal/workspace`;
`TestWorkerCredentialBlockerError` in `internal/runner`;
`TestIssueSpendSinceExcludesInstanceInfrastructureAttempts`
in `internal/store`. Review preserves instance attribution outside those cases.

## INV-3 — Mechanism moratorium

No new brake, breaker, lease, park, recovery path, revocation, reason code or
reconciliation loop is allowed; any change to one must remove or consolidate
an existing one, and the remedy is never a guard. Do not add configuration keys,
CLI subcommands or dashboard surfaces to work around a mechanism. Fix the mechanism.
New or expanded mechanism scope also requires the human approval in INV-11.

Constant lane-transition reasons must use the
[existing vocabulary](../internal/invariants/source_policy.json). Dynamic
forwarding functions require reviewed formatted-source SHA-256 entries.
Renaming a mechanism or updating a snapshot does not establish removal or
consolidation. Provider messages, operator reasons and decision helpers remain
review boundaries.

**Enforcement:** `TestRepositorySources` and `TestSourceViolations` in
`internal/invariants`. Review verifies removal or consolidation and the
operator's scope authority.

## INV-4 — Native merge queue

Merges go through the repository's merge queue when one exists. Without a
queue, Detent uses its serialized merge worker. Other repositories retain
their chosen settings.

Queue ownership belongs to the current PR head. Missing, running, cancelled
or failed checks do not qualify for queue admission. Completed success,
skipped and neutral checks may enter the queue, subject to provider-required
contexts; skipped checks are not passed-test evidence. Unresolved review
threads require queue withdrawal before the configured Rework handoff.
Withdrawal failures must not prevent a lane write to Done.

Queue removal is accounted once per distinct removal and PR head. The first
removal permits normal readmission; a second on the same head routes to Rework
under the existing budget. Restart preserves that accounting, and a repaired
head starts a fresh count.

**Enforcement:** `TestDelegateNativeMergeQueueIssuesEnqueuesGreenTrainWithoutWorkerDispatch`,
`TestDelegateNativeMergeQueueIssuesCachesQueueEntries`,
`TestNativeMergeQueueReviewReworkAfterEnqueue`,
`TestNativeMergeQueueWithdrawalDoesNotBlockDone`,
`TestNativeMergeQueueSkippedChecks`, `TestNativeMergeQueueNeutralChecks`,
`TestNativeMergeQueueHeadBudget`, and
`TestNativeMergeQueueBudgetSurvivesRestart` in `internal/orchestrator`.
The `detent doctor` queue diagnostic reports programmatic merges while a queue
is present; live queue settings require operator inspection.

## INV-5 — Local pull-request validation and scheduled release evidence

This repository starts no validation workflow from `pull_request`,
`pull_request_target` or `merge_group`, and requires no branch status check.
The sole event exception is `.github/workflows/cla.yml`: its single job runs
only `contributor-assistant/github-action` on `pull_request_target` and
`issue_comment`, without checking out or executing PR code. The action owns
signature records on the dedicated, unprotected `cla-signatures` branch.

Ordinary submission and merge do not wait for CI, local validation, coverage,
fuzz duration or scheduled validation. The self-hosted project runs `true`,
publishes no local status and sets `gate.required_status_checks: []`.
Reported failed CI and native base-branch requirements remain authoritative.
`gate.automated_review: "off"` removes pending-review waits; reported P1
findings still require Rework. Other projects retain their configured policies.

Optional diagnostics preserve failures and support concurrent worktrees without
a shared validation lock. Tool invocations share the host through
`TEST_PROCS`; lint allows parallel runners and client tests retain file
isolation with at most two workers per command.

Scheduled and manual full validation pin the current `develop` SHA and run
every full-suite job on that commit, regardless of existing release tags.
Only the successful full-suite finalizer publishes `scheduled-full-ci`,
creates an annotated validated patch tag with exact status evidence and
dispatches release. It does not merge to `main` or deploy production.
Every `develop` push deploys staging independently; production release
artifacts use validated tags only.

Scheduled Detent failures use the selected native reporting owner and stable
fingerprints, preserving commit, run, attempt, job, imported history and holds.
Under the human-approved Detent scheduled reporting policy, newly reported
proven source or test blockers with reliable, pinned evidence enter Todo at
least High priority. The selected workflow must provide dispatchable,
nonterminal Todo and nondispatchable, nonterminal Backlog, neither operator-only.
Reused items retain their lanes, human questions, holds and terminal history;
priority updates preserve High and Urgent. Unknown setup, startup, download,
network, backend, protocol and authentication failures remain instance intake
in Backlog. Repair workers verify reported failures on their current base;
green results append tracker evidence without closing work or changing lanes.
Failed Cloud publication must not fall back to GitHub issue writes.

Conversation bundles are ignored feature output, prepared from the selected
source and lockfile by the build owner with Node 24 and `make app`. Prepared
release source archives include embed inputs and build identity. The scheduled
suite retains safety-critical coverage floors and boundary fuzz seeds.
The NilAway audit selects all Go packages, including unchanged importers;
baseline matches require both location and source-line hash.
The pinned analyzer uses Go's vettool protocol to carry dependency facts between
serial package processes. Its default Go soft memory limit is 1 GiB, not a
hard RSS bound. Installation, package loading and analyzer failures cannot be
accepted as reviewed findings. Grouped-finding explanation lines remain part
of their reviewed finding; unrelated tool errors still fail the audit.

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

Machine-filed issues carry an origin stamp and a stable problem fingerprint.
The fingerprint identifies the problem, excluding timestamps, attempt IDs and
wording variations. Use the selected tracker's supported filing owner; preserve
imported provenance and match open work before filing. Matching open issues
receive occurrences or comments instead of duplicate issues.

Intake findings prefer an open issue among durable-marker matches. If all
matches are closed, the newest issue (highest repository issue number for
GitHub intake) is already handled: create no issue, comment, content update or
state change. Occurrence evidence identifies runs, attempts, commits and jobs
without changing problem identity.

The worker's existing `file_machine_issue` tool accepts optional integer
`priority` creation ranks: 1=Urgent, 2=High, 3=Normal, 4=Low. Native intake maps
these to priorities 0–3; omission leaves new work unset, and body prose never
supplies priority. Fingerprint reuse may raise unset or weaker priority through
the existing mutation owner, with the observed revision and a priority-only
edit. It never lowers stronger priority, replaces origin or content, removes
operator holds, changes lanes or authorizes admission.

Host-owned native worker intake requires the current registered runner's scoped
source lease and fencing token. Its existing creation transaction creates
Backlog work or records an occurrence on matching open work. Filing priority
grants no general operator mutation or worker lane-writing authority.

**Enforcement:** `TestMachineIssueTool` in `internal/orchestrator`;
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
Regenerate tracked sqlc, Templ and Tailwind output from combined source inputs
instead of text-merging generated conflicts. Conversation bundles remain
build-owned ignored output.

**Enforcement:** `TestDoctorRespectsProjectCIPolicy` in `internal/cli`;
`TestIndividuallyValidBranchesRejectIntegratedCollision` in
`tools/migrationcheck`; `make check-migrations`. Doctor reports live branch
protection as project evidence; the operator verifies live settings.

## INV-9 — Retired mechanisms

Retired lane revocation, indeterminate-lane stops, per-issue infrastructure
parking and root-level `rateLimit` in mutation documents stay retired.
Obtain budget information through a separate query. Documentation and negative
fixtures may name retired mechanisms; runtime code must not restore them.
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
Review rejects semantic reservation and preemption substitutes.

## INV-11 — Human mechanism scope approval before Todo

No new or expanded operational mechanism enters Todo without a human's explicit
approval of that scope, regardless of author or title type. Assistants file
such work to Backlog. Only a mechanism fix with recorded runtime evidence whose
remedy removes or consolidates may reach Todo without a human.
Features that add or expand no mechanism may be filed straight to Todo.
Admission approval does not lift the mechanism moratorium.

**Enforcement:** Admission Criteria rule 9 and review enforce scope and runtime
evidence. The diagnostic `INV-11 human scope approval` checks applied Todo
ledger entries, current issue scope and matching human provenance; missing
evidence warns, and current text cannot prove admission-time scope.
`TestDoctorInvariantAdmission`, `TestDoctorInvariantAdmissionBatches`,
`TestDoctorInvariantScopeClassification`,
`TestDoctorInvariantAdmissionRepeatedEntries`, and
`TestDoctorInvariantAdmissionUnavailableEvidence` in `internal/cli`;
`TestDoctorInvariantRegistration` in `internal/invariants`.

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
existing priority controls. Compact may hide effort before model; Cozy and
Comfy retain effort. The status line contains at most 48 Unicode characters
and names the wait in actionable words.

Scheduler evidence, tracker ages, timestamps, token and attempt counts,
fact grids and diagnostic text are not card content. Existing observations
belong in the detail sheet and hover titles, subject to INV-15. Adding card
body content or lengthening the status line changes this rule.

**Enforcement:** `TestINV13BoardCardContent`,
`TestINV13SheetObservations`, and `TestBoardCardSignalBudget` in
`internal/web/templates`; browser review checks one-line status and effort
visibility across densities.

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
Hub server-rendered pages and the local Templ dashboard. A change may add a
visible element or content (row, line, banner, badge, chip, column, panel,
page, tooltip text or status copy) only when a human-authored issue names that
UI change. Machine-filed issues, agent-expanded scope and "while I was here"
additions never qualify.

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
`docs/invariants.md`. Following an unchanged invariant requires no edit.
The current completion contract owns tracker publication; this rule grants
workers no additional write authority.

**Enforcement:** Review applies [AGENTS.md](../AGENTS.md#repository-invariants),
[CLAUDE.md](../CLAUDE.md#repository-invariants) and the evidence instructions in
the [PR template](../.github/PULL_REQUEST_TEMPLATE.md). This is a review rule;
it adds no check, gate, configuration or runtime mechanism.
