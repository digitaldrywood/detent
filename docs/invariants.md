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

Sprite runner wake work (native #212) resolves current project-selected
dispatchable states and reads one currently granted, active, stale runner
hostname at a time. Each provider request uses the current project secret and
records its own secret-use audit; duplicate hostnames do not grant authority.
Grant removal, token revocation, heartbeat updates, workflow changes and secret
replacement between requests take effect on the next candidate read.
`TestWakeSpriteRunnersFreshAuthority` and
`TestWakeSpriteRunnersProjectIsolation` preserve these boundaries.

Analytics parity (native #33, imported #3665) uses the same application adapters
for stdio, HTTP MCP and the daemon bridge. Each direct read resolves current
read authority and project grants before selecting projects or aggregating
usage, digest, efficiency and outcome data. Restricted credentials receive no
unattributed activity or organization totals. Scoped digest queries retain the
difference between unrestricted and explicitly empty project sets.
Native throughput counts exact-version Change landing receipts with native
provenance and merge SHA inside the requested half-open window; coding completion,
imported Done items and board counts do not count as shipping. Recorded phase
and skip aggregates retain source and observation times, population limits and
partial evidence. Queue time, private instruction causality, missing source data
and sub-hour usage attribution remain explicitly unavailable. These reads add no
lane writer, mechanism or persistent summary owner; native #91 retains summary
provenance. `TestMCPAnalyticsReads`, `TestMCPAnalyticsNativeProject`,
`TestHostedAnalyticsReads`, `TestNativeAnalyticsRuntimePopulation`,
`TestChangeLanding`, `TestDailyDigestReconcilesRuntimeTables`,
`TestDailyDigestOutcomeProvenance`, `TestAnalyticsUnavailableServices` and
`TestNativeAnalyticsUnavailableServices` cover authorization, transport parity,
bounded windows/populations/results and missing services.

Cloud issue attachments (native #175) retain the existing signed entry and
tenant membership/project authorities. Only entry holds Spaces credentials or
constructs organization-prefixed object keys; private startup probing and
opaque unauthorized reads preserve tenant isolation. Attachment bytes join the
existing collaboration allowance transaction. Orphans and deleted references
use entry's existing maintenance cycle, and deprovisioning erases the tenant
prefix. `TestAttachmentRoutesIsolation`, `TestAttachmentStorageProbe`,
`TestCloudAttachmentQuota`, `TestCloudAttachmentRetention`,
`TestCloudAttachmentReferenceDeletion`, `TestAttachmentOrphanSweepAndDeprovision`
and `TestCloudAllocationGeneratesTenantConfiguration` cover these boundaries.

Native #177 reuses those authorities for API, MCP, CLI and validation evidence.
MCP sessions and project write grants are checked by the Hub before entry stores
the bytes. Markdown references bind to the same project and item/comment in the
content transaction; copied references never grant read access or reassign a
bound upload. `TestAttachmentRoutesIsolation` covers API/MCP PNG round-trips,
foreign-project tokens, automatic comment binding and the evidence publisher.
Worker screenshot capture (#197) selects only changed image paths from the
existing attempt diff anchored before native Code/Rework execution. Unchanged
inherited images and deleted paths are not current evidence. Programmatic
landing never collects or republishes validation screenshots. The same rooted
filesystem, regular-file, ten-image and per-image byte limits remain in force;
no directory-wide fallback substitutes historical evidence when attribution is
unavailable. `TestValidationEvidenceUsesCurrentAttemptDiff` covers inherited,
new, modified, renamed and deleted images, bounds and publication failures.
`TestNativeLandingDoesNotRepublishValidationEvidence` preserves the genuine
landing receipt and successful completion with excess inherited images.
`TestArtifactsFinalizeBeforeWorkspaceCleanup` covers worker screenshot capture.
The client renders only the exact Cloud attachment path as a direct same-origin
image; other filesystem paths retain workspace classification.

Reviewed native landing (#170) uses a LocalGit checkout and ref scoped to the
issue and immutable reviewed head, independently of Code and operator source
worktrees. External versions retain their real PR repository, base, branch and
head separately from the local landing ref. The registered runner remains the
PR publication and merge owner; current version, approved policy, lease/fencing
and atomic reviewed-head merge checks remain authoritative. Preparation and
landing never reset, repoint or steal a retained Code or source checkout/ref.
`TestRunnerLandingPreservesCodeAndOperatorOwners` verifies both operator and
External versions through the real Runner and LocalGit without a provider turn,
including unchanged owner files, raw indexes, HEADs and source refs.

Issue and comment saves (native #176) record attachment references in the same
collaboration transaction and only within the current organization and project.
Multiple sources may retain one attachment; deleting one source keeps bytes
referenced by another. Markdown stores only opaque attachment IDs. Cloud renders
metadata authorized for the current project before assigning a same-origin byte
URL and retains `img-src 'self' data:`. `TestCloudAttachmentSavedReferences` covers
save, edit, deletion, orphan retention and project isolation.

Native failed coding and rework completion (#169) selects an allowed configured
review destination under the existing leased completion owner before publishing
`run.finished` or releasing the claim. It records the authentic failed or cancelled
outcome, preserves source, creates no Change/version and schedules no new coding
attempt. Stale lane authority remains a completion refusal.
The existing deferred completion receipt retains cancellation/timeout outcomes
and cleanup attribution across lane-write retries. `TestNativeChangeRunCompletion`
and `TestNativePlannerAutomaticHandoff` cover failed settlement, stale refusal,
preserved staged source and lease-retirement ordering.

Organization-scoped issue reads resolve the project from the work item ID,
then reuse the native read authority to check current organization and project
grants before returning the item (#174). Sidebar selection confers no authority.

Native runtime evidence (#92) reuses the scoped work, explanation and Change
application owners for API and MCP. Direct reads recheck current connection
scope, project grants and work-item/attempt ownership, including local attempt
selectors. Receipt snapshots preserve authentic native event actors, server
timestamps, ordered attempt identity, dispatch generation and reviewed immutable
version/head. Current lease freshness, routing/capacity authority and current
Change readiness are distinct from recorded scheduler decisions. Missing
historical decisions, activity or accounting are explicitly unavailable;
terminal success and imported Done are not landing evidence. Only the existing
exact-version Change landing authority can validate a landed receipt and merge
SHA. Instruction activity is bounded, content-free and partially observed, with
dropped/unpaired counts and explicit causality limits. REST operation headers
and existing attribution windows identify selected-client coverage, not the
account consumer. The runtime projection omits private bodies, raw commands,
instruction contents, paths and credentials. Focused owner, grant, stale,
pagination, redaction and refusal regressions cover these boundaries.

Runtime activity checkpoints update the existing mutable attempt observation;
they do not append cumulative profiles to immutable history. Idle and unchanged
observations perform no checkpoint writes. Genuine phase/identity/landing changes,
lifecycle checkpoints and completion retain timestamped history, including the
final activity profile. Agent completion joins the final profile checkpoint before
finishing the native attempt. Bounded recent activity detail and fixed-category
whole-attempt timing totals share that checkpoint owner. Detail and projection
omissions remain distinct from received-event loss; concurrency is counted once
and unfinished historical intervals retain explicit partial coverage. Summary-only
reads preserve recorded totals without expanding tool history. Lease renewal
does not manufacture activity freshness.
Candidate previews remain reads. Actual scheduling attempts retain changed decisions
through the existing history owner, deduplicated by runner, source, revision,
Change version/head and decision evidence; genuine fenced claims remain distinct.
This consolidates the observation producers without a new telemetry owner or
mechanism (INV-3).

Native machine reports copy scheduler state under the scheduler mutex, perform
capability negotiation, feature, checkout and heartbeat/registration requests
unlocked, and commit observations and the per-project heartbeat time under the
mutex again. A slow Hub call for one project therefore never serializes claims,
policy checks or lease renewals for other leases. Renewal still installs only
through the existing fencing-token comparison, and a stale pinned policy still
drops the claim through the existing policy owner, so the unlocked window adds
no guard, lease or recovery path. Concurrent reports may each reach the Hub;
machine reports are idempotent observations. `TestBlockedMachineReportDoesNotSerializeLeaseRenewal`
blocks a registration request and covers unrelated renewal, stale policy and
superseded fencing tokens.

The selected runtime read (#159) projects only the current immutable Change
version and its review/check evidence through the existing summary authority.
Indexed older-approval existence preserves stale review semantics without
loading historical versions, discussion, reviews or checks; explicit Change
detail still returns history. Merging eligibility shares the current summary.
Runner identities retain project grants, routing, health and isolation evidence;
shared host occupancy and organization-wide provider reports/reservations are
read once per snapshot, including live occupancy outside the returned runner
page. The 100-runner bound remains explicitly truncated and cannot establish a
complete exclusion decision. `TestNativeRuntimeReadHistoryCost` and
`TestNativeRuntimeReadSharedCapacityCost` cover indexed history-independent
reads, shared occupancy, scope, expiry, fencing and truncation; the existing
Change fixture compares current and full summary semantics.

Scheduled diagnostics for the migrated Detent repository call the existing
Cloud `file_issue`, revisioned `edit_item` priority and `add_comment` application
owners using the selected project and current scoped API/MCP connection. New diagnostics enter only a
configured nondispatchable Backlog; imported open items and operator holds
receive occurrences without lane writes. Scheduled success comments are
validation evidence, never completion, landing, admission or review authority.
Parsed source/test blockers of deployment or the scheduled validated release
receive at least High priority under this repository's operator policy;
priority changes preserve Urgent and do not admit work or remove holds.
Unknown infrastructure evidence remains instance-owned intake without priority
promotion or source-repair authority. This policy adds no local merge gate and
does not change scheduled all-job validation or pinned release provenance.
Other repositories keep their existing GitHub reporting policy. No tool,
scope, project configuration key or alternate tracker owner is introduced.

Hosted project workflow configuration uses the existing administrator-owned
integration settings command and revision/idempotency boundary. Reviewed
NativeState definitions remain project-specific, retain existing workflow row
identities and item/dependency records, and cannot remove states occupied by
active or archived items. Compatibility workflows retain source ownership;
worker operator-only transition rules and approved repository policy remain
authoritative. Native creation forms use the first configured state as their
initial lane. Workflow edits through MCP retain exact material-action approval.
`TestHostedProjectWorkflowConfiguration`, `TestActionConfirmationClassification`
and the client workflow settings/default-state regressions cover these boundaries.
No tracker lane writer or recovery mechanism is introduced (INV-3).

Luna project mutations reuse the hosted MCP chat action service and its exact
browser/session/CSRF approval form. Each message binds the originating browser
session, current role and project grants; previews stay within the conversation's
project and approval rechecks that authority and resource revisions. Chat always
requires explicit approval, including issue edits and comments. Native workflow
moves delegate to the same application command as the workflow HTTP endpoint;
workers gain no lane-writing authority. No parallel approval or recovery service
is added (INV-3). `TestCoordinatorProjectActions` covers authorized changes,
refusals, rejection and stale authority/revisions. The conversation browser
journey covers approving GitHub PR mode and retrying a blocked issue.


Permission outcome authority (#3758) belongs to the existing completion owners.
Ordinary tracker runs use fresh canonical Workpad comments, dependencies,
completion evidence and project gates; final prose or a final-only status block
cannot supersede a canonical comment. Native runs retain their final-only
contract: the existing native workflow completion owner settles human attention
before unchanged work can finish, including outcomes with no produced change.
Typed native landing results and immutable current-version review remain
authoritative. Workers never write tracker lane state.

Native concurrent-base merge refusals (#160) retain Merging and the current
immutable Change/version/head and review. The exact merge endpoint's typed
`Base branch was modified` evidence reaches the existing forge continuation
owner, never Blocked, source Rework or a new review request. Retry fetches current base truth
and repeats the atomic reviewed-head merge under the current lease and policy;
only an actual successful landing receipt can finish the item. Original HTTP
status, method, repository/PR and reviewed-head/fetched-base evidence remain in
attempt history. Actual head movement, closure, proven source conflict and
current protection/review/check refusals retain their existing owners; typed
quota and reset evidence retain capacity precedence.
Strict `Head branch is out of date` protection (#161) follows the existing native
protection refusal owner to Human Review or Blocked according to project policy,
without a same-version forge wait. The legacy connector retains its existing
head-refresh/rebase outcome for this message; managed project protections remain
binding.
`TestNativeLandingRunCompletion` drives the real Runner and LocalGit from the
recorded PUT merge 405 JSON through durable Merging continuation to a successful
same-version landing after a concurrent base advance, with no agent turn or
fabricated review.

Project MCP parity (#3342) calls the same dashboard project, onboarding,
integration, import, policy and budget application commands. Native command
receipts are checked before revisions/provider reads and before repeat approval,
with current role/scope/grant/resource ownership checked again. The current
application context is bound by the shared authority resolver, never by tool
arguments. Native and repository policy scopes resolve through the authorized
project; change-review policy retains its principal/provenance checks. Hosted
browser approval reuses the existing chat form with session, CSRF and exact
payload tokens, including the shared organization's path. Browser viewers cannot
inspect ungranted previews or toggle another principal's confirmation mode;
current operators need their own connection or administrator authority and each
action's current grant/scope. Created-project receipts recheck the returned
resource on cached, reconnect and action-result paths. YOLO suppresses only
confirmation. Approval by a different administrator retains the original
requester's freshly resolved native command scope and attribution; fleet
previews retain their existing explicit runner-grant authority. Unhosted hubs
without that browser authority return opaque unavailable errors for material
variants. Tools accept no credentials; browser
setup returns navigation/requirements, and import/provider errors are redacted
from reads, actions and replay results. This extends INV-1 authorization
adapters without a tracker lane writer or an INV-3 recovery/revocation mechanism.
`TestHostedProjectTools`, `TestProjectImportToolReceipts`,
`TestHostedOperatorCurrentAuthority`, `TestProjectArgumentBounds`,
`TestActionConfirmationClassification` and project/budget cases in
`TestMCPActionApprovalBoundary` cover these boundaries.

Runner batch intake reuses `commandGitHubBatchOperation` and the existing native
receipt contract. Discovery and non-dispatchable apply execute directly;
dispatchable apply and retry require exact operator approval. Current project
administrator/write grants, matching runner checkout, revisions and destination
lane checks remain application authority. Bounded batch reads/results redact
stored provider diagnostics. `TestGitHubBatchIntakeRetryAndNativeOwnership` and
`TestActionConfirmationClassification` cover the MCP adapter without adding an
intake mechanism or tracker lane writer.

Work/board MCP reads (#3340) share the dashboard application read models and
native organization/project/item reads. Direct calls resolve current read
scope and grants, then resource ownership; saved versions and history retain
the same dependency visibility rules. A daemon's broader native connector
credential does not expand its caller's local project grants; relationship
projection requires both application boundaries. Bounded pages carry identifiers, URLs
and source freshness. `work_history` reads durable application records;
`recent_activity` retains its live snapshot semantics. Board activity marks
durable and snapshot events explicitly. Review/diff/artifact bodies remain
with #3347; this child exposes their authorized references. No read adapter
writes tracker lanes, creates confirmations or adds a protection mechanism.
`TestOperatorGitHubWorkReads`, `TestOperatorNativeWorkReads`,
`TestHostedOperatorCurrentAuthority` and `TestProtocolWorkReadParity` exercise
the application boundary and both transports, including direct-call denial.

The native work-items read's optional `include=work` projection keeps scoped
lane totals and a bounded open selection independent of the inventory cursor
(#179). Live attempts with current leases sort before dispatchable lanes; lane
membership alone never establishes a running worker. Compact items omit issue
bodies and linked-source snapshots. The existing project/filter/archive scope,
grants, opaque cursor binding, cancellation and 24-item client enrichment budget
remain authoritative. Search over titles, identifiers and labels and OR selections
within each filter dimension are authoritative across the selected authorized
projects (native #181). Items and lane totals use the same complete filter scope. Transport cursors stay internal
to loading more matching results on the same Board/List; no global history page
controls remain. Filter changes cancel obsolete reads and reset continuation;
activity refresh retains visible work and the explicitly loaded matching depth.
The existing request owner finishes an in-flight same-selection read and coalesces
activity into one pending refresh; genuine selection changes still cancel it.
Refresh re-fetches server data within that chosen depth and enrichment budget,
without restarting on runner display-name updates or expanding unseen history.
A queued activity refresh cannot erase a simultaneously requested continuation.
Background refresh keeps existing controls available; initial and explicit
continuation reads retain their loading state. Both intents compose through the
same bounded request owner, while scope changes and authorization refusals
cancel or clear obsolete data.
The existing shared UI clock updates age and elapsed labels without rerendering
the board controls, unchanged card bodies, or list rows. Its timer stops when
no subscribers remain; source updates retain their existing render ownership.
Sorting and filter-choice discovery remain local to loaded items; terminal counts
describe inventory, not shipment.
`TestNativeWorkPageOperationalScope` and the conversation Work pagination fixture
cover scoped totals, expired leases, bounded reads and persistent operational
visibility across inventory pages, including refused and stale reads.

Native worker reads use the existing supplemental agent tool transport and
authenticated execution owner (#195). The supplied `work_item`,
`work_comments`, `work_history`, `list_changes` and `get_change` tools reuse
native application read methods and bounded schemas/results. Each invocation
validates the current execution authority and binds to its exact project;
canonical `wi_` references are required; foreign projects, writes and arbitrary
URLs are rejected. Identity renewal,
credentials and API requests remain on the host; worker network and filesystem
isolation do not change. Providers without supplemental tools use the supplied
native context and report unavailable required evidence as an instance
limitation. `TestNativeExecutionReadToolsKeepHostAuthority` exercises real
enrolled-runner authenticated reads, immutable version evidence, bounds and
authority loss. New Codex threads receive these definitions; pre-update resumed
threads retain their original definitions, and the Claude CLI currently lacks
this supplemental-tool adapter. This read authority does not change terminal
no-change completion classification.

Billing/usage MCP parity (#3345) delegates checkout/portal, plan and usage reads,
exports, and daemon budget overrides to the dashboard application operations.
Organization billing remains owner-only without support impersonation; plan
reads require owner/admin, while usage totals include only current project grants.
Platform entitlement administration and publisher artifact allowance callbacks
remain separate authority boundaries and are never organization tools. Material
actions use the existing exact browser approval conversation; YOLO changes only
confirmation. Approved checkout retries resume the existing durable purchase
intent/provider key, including after response loss; uncertain portal effects keep
their existing receipt. No billing ledger, recovery loop or tracker writer is
introduced. `TestHostedBillingMCP`, `TestBillingCommandResponseLoss`,
`TestMCPDaemonBilling`, `TestHostedOperatorCurrentAuthority`,
`TestUsageReportAggregates`, `TestHostedArtifactAllowanceBoundary`, and
`TestPlatformComplimentaryPlansThroughTenantHub` cover these boundaries.

Work-item MCP parity (#3341) reuses the dashboard work creation, discussion,
priority, native collaboration, park acknowledgement and security-disposition
commands. Native edits keep expected revisions and workflow authority; connection
approval and YOLO cannot bypass them. Compatibility dependencies, ordering and
priority use the hub's existing application commands. Board removal now delegates
project removal or lane-field clearing to `ReconcileOperatorMove`, consolidating
the former dashboard tracker writes under the orchestrator. Hub-only deployments
without a daemon lane owner or approval service return opaque unavailable results
for those commands. No tracker capability or lane writer is added to MCP.
The reviewed `applyOperatorMove` source digest changes for this consolidation;
the existing move reasons are unchanged and no mechanism or reason code is added
(INV-3).
`TestMCPNativeWorkCommands`, `TestHubMCPWorkCommands`,
`TestHubMCPCompatibilityCommands`, `TestMCPActionApprovalBoundary`,
`TestOperatorRemovalCommand` and `TestWorkArgumentsDirectCallBounds` cover
replay, native revisions, workflow refusal, ownership, bounded direct calls,
real approval/rejection, authorized YOLO and orchestrator-owned removal.
Native submission and board discussion parity (native #34) keeps every form
action with those existing work, governed move and Change commands. Linked
creation passes the permitted GitHub issue URL to the native creation owner;
creation ranks 1–4 map to native priorities 0–3, while edits retain native
priority values and expected revisions. Change creation preserves its additional
project-owned issue links. `work_pr_comments` derives the linked repository and
PR from the authorized work item and pages the existing forge discussion reader;
it accepts no arbitrary repository, PR number or URL. Board and tool discussion
reads share safe service errors. Deployments without that reader remain
unavailable. `TestHubMCPWorkCommands`, `TestOperatorNativeClientReads`,
`TestOperatorGitHubWorkReads`, `TestOperatorChangeCommands` and
`TestProtocolWorkReadParity` cover linkage, retries, priority mapping, ownership,
bounded discussion, redaction and current authority across both transports.
The existing approval/YOLO and orchestrator lane owners remain authoritative;
parent #3259 retains final conformance and zero-pending acceptance.
Change/review/artifact MCP parity (#3347) binds shared application reads/commands
to freshly resolved connection authority. Nested work-item/change/version and
exact artifact receipt ownership are checked before effects and replay. Native
HTTP and MCP reuse the same transaction, policy, revision/bundle, attribution
and retry commands; the existing chat browser approval owns material review and
policy/service-binding decisions. Global discovery honors the freshly resolved
lesser hosted role as direct calls do. Stored diffs and run detail match the
named attempt to its work item; PR reads reuse the existing connector projection
and its observation times without direct GitHub access. Diff pages bound encoded
JSON bytes and mark patch truncation. The daemon artifact library reuses its
project-scoped dashboard reads. Historical artifact downloads preserve read grants, retention,
TTL and original session authority; tokens appear only in the usable client result,
never in URLs, audit summaries or durable retry receipts. Operator publication
(native #196) calls the same immutable version command as HTTP, without run,
attempt, lease or caller-selected producer attribution. It requires the explicit
expected current version and current approved repository/review policy, retains
unverified artifact semantics and authentic external PR references, and returns
the stable publication receipt plus live current Change and lane state. The existing
promotion owner still applies normal review/check policy; no-review/no-check
versions may promote on publication. Worker fenced publication, expected CI
attestation, publisher callbacks and landing reports remain producer/service
operations. Repository protection and the runner's landing authority remain
independent. No merger or tracker lane writer is introduced.
`TestOperatorChangeCommands`, `TestOperatorArtifactResults`,
`TestOperatorChangePublication`, `TestHostedOperatorPolicyApproval`, and the existing hosted artifact pilot cover
these boundaries. This extracts application commands rather than adding a
protection mechanism (INV-3).

Hosted application context MCP parity (native #32, imported #3664) reuses the
browser bootstrap, runner update and project event application reads through one
adapter for stdio and HTTP. Bootstrap omits browser CSRF/session/form secrets and
filters project/model context by current member and API-key grants. Runner updates
retain non-viewer authority and runner grants on every project, including the key's
project boundary. Events return one current invalidation sequence and optional safe
workspace revision with observation/freshness semantics and a scope-bound comparison
cursor; an unchanged transport tick is not a project update or event replay. No new
poller, reconciler, grant cache or transport proxy is introduced. Results fit existing
item/byte bounds or return a safe unavailable response. `TestHostedContextMCP` and
`TestHostedContextKeyProjection` and `TestHostedContextUnavailable` cover dedicated/shared bindings through both
transports, shared browser content, current and removed authority, direct calls,
secret omission, bounded/unavailable reads and activity/workspace cursor changes.
Parent #3259 conformance and unconditional zero-pending acceptance remain pending.

MCP transport parity (#3339, native #207) uses one permission-filtered typed
registry with toolset metadata. Tool discovery returns the complete current
catalog in one response without a continuation cursor, resolving application
discovery once regardless of catalog size. Existing opaque cursors still validate
their principal/catalog digest and bounded offset against current authority,
then return all remaining tools without another cursor. The `2026-07-28` stateless
protocol validates per-request metadata and mirrored HTTP headers; older handshakes retain their
bound sessions. Modern `tools/list` and `server/discover` results carry
`ttlMs: 0` and `cacheScope: "private"` through the shared result serializer
(native #213), without retaining a catalog cache. Other modern results retain
their complete discriminator without cache hints; older result shapes stay
unchanged. Discovery cursors and client metadata confer no authority.
Project-resource tool discovery preserves member write capabilities using the existing role predicate,
while organization administration retains its separate role requirements.
Discovery has no selected project; actual calls always recheck current project
grants, credential scope and revocation.
Modern HTTP binds the existing application approval conversation to authenticated
principal/organization/credential/session identity, independently of the POST's
lifetime; browser approval retains the application resolver for current authority.
No protocol session, confirmation argument, persistence writer or tracker lane
writer is introduced. `TestProtocolApplicationParity`,
`TestModernHTTPMetadataValidation`, `TestCatalogCursorCurrentAuthority`, and
modern cases in `TestMCPActionApprovalBoundary` cover transport parity, forged
metadata, direct-call ownership, revoked authority and approval after a POST ends.

Organization, membership/grant and credential administration (#3344) uses the
same application commands as dashboard handlers. Dedicated bootstrap accounts
and shared-entry account sessions carry no project grants; context selection
returns a fresh authentication destination and leaves connection authority bound
to its original context. Support entry returns the existing interactive provider
flow only to configured support actors and grants no platform/customer powers.
Access changes retain exact target/input previews in the existing chat approval
service. Current role, scope, project grants and resource ownership are checked
again on execution and cached result delivery. Account receipts reuse existing
application audit ledgers; invitations and credentials retain the existing
application retry contract. Credential values are omitted from conversation,
browser, audit and durable retry records, and deliberate result delivery stays
bound to the originating identity/session. Revoked or replaced credentials
cannot deliver a stale secret. No lane writer or revocation/recovery mechanism
is added. `TestAdministrationExecution`, `TestHostedAdministrationAuthority`,
`TestNativeCredentialAdministration`, `TestDedicatedAdministrationSetup`,
`TestEntryAdministrationContext` and `TestMCPCredentialAdministration` cover these
boundaries with existing application identity fixtures.

Hosted key administration (native #95) exposes only list/create/revoke through
the organization-bound MCP connection, reusing the hosted API key owners.
The shared administration/chat requirement identifies credential administration
separately from general organization administration: members and viewers may
manage their own keys within their existing allowed scopes. Bearer keys and
support impersonation remain denied; account entry returns a destination and
requires reconnecting without transferring its authority. Dedicated and shared
key operations recheck the original provider session, current membership and
lesser role, issuer grants, target ownership, expiry and revocation at execution
and sensitive delivery. Exact browser approval and connection YOLO retain their
existing owners. Durable business retries retain safe metadata without the
created secret. `TestHostedCredentialMCP` and
`TestHostedCredentialMCPAuthorityChanges` cover both transports and deployments,
browser approval, current authority and connection-bound delivery. Native #48
adds durable all/selected project access through this same owner; browser and
MCP creation share the mode default and validation. Native #26 remains open for
final parity acceptance.

Invitation withdrawal revokes the provider invitation before atomically removing
the local invitation and releasing its seat. Provider refusal preserves both;
resend uses the existing invitation and the same owner/admin authority. Invitation
acceptance requires the locally issued pending record, including after login has
started. `TestHostedInvitationLifecycle`, `TestHostedLoginInvitationEmailEntry`
and the account browser invitation spec cover these INV-1 boundaries.

Organization invitations persist explicit project grants, bounded by the
inviter's current project access and write/runner permissions. Pending grants
can be edited through the same authorization boundary. Existing-member replacement
also authorizes every omitted hosted or principal project grant as a revocation,
using the existing member-grant authority before provider issuance and local
persistence. Pending-invitation edits enforce the same complete replacement scope.
Acceptance commits the
local membership, exact project grants, principal grants and invitation marker
in one transaction; revoked invitations cannot establish local access.
`TestHostedInvitationGrants` covers creation, edits, acceptance, rollback and
privilege escalation, and the account Playwright spec covers read-only access
to one project with no access to another.

Account/session parity (#3662) shares semantic landing/onboarding facts with browser
account reads and binds destinations to current session/membership checks,
including cached organization-switch delivery. `session_logout` shares browser
logout execution and current-session revocation, retaining shared-entry tenant
propagation; shared tenant hubs never substitute for shared-entry account
logout. Access-ending sign-out uses the existing real browser approval or
connection-bound human YOLO. Only the executing call/browser decision returns
the content-free sign-out outcome after revocation; later reads and retries
remain denied. Provider failure cannot restore local access or expose provider
errors; browser form/provider exchange secrets stay outside typed results and
audits. No new revocation or retry mechanism is added (INV-3).
`TestEntryAdministrationContext`, `TestHostedAccountSessionOperations`,
`TestSharedEntryLogoutRevokesLocalAndProviderSessions`,
`TestHostedLoginLogoutRevokesLocalAndProviderSessions`, and
`TestActionConfirmationClassification` cover these boundaries, including the
real entry adapter through both transports.

Creator provisioning parity (#3663) shares the browser's creator-bound allocation
lookup and retryable resume command with the administration MCP executor. Current
account session, managed allocation ownership and deployment availability are
checked on direct calls, approval, execution and retry delivery. Resume is a
material provisioning action: connection YOLO skips confirmation only, and the
existing audit receipts prevent a retried command from resetting a later failed
attempt. Status/results and previews omit allocation diagnostics, endpoints and
provider credentials; ready status returns the current destination. The existing
allocator retains admission, retry and capacity limits and its wake signal.
`TestEntryProvisioningAdministration`, `TestAdministrationInputBounds`,
`TestActionConfirmationClassification` and the existing provisioning capacity and
tenant-start journeys cover these boundaries through shared stdio/HTTP dispatch.
This extends INV-1 application authorization without adding an INV-3 mechanism.

MCP mutations carry content-free, trusted audit/correlation context (#3338).
Dashboard commands reuse durable operator events for retry receipts; native and
hosted commands reuse `native_commands`, with existing billing intents/provider
keys for resumable purchases. Replays reauthorize current authority and conflict
on changed payloads; uncertain effects retain pending records without takeover
or recovery. Audit records do not carry arguments, raw provider errors or secrets.
These records are application effects/audit only and confer no tracker lane
writing authority. The orchestrator remains the sole lane writer.

MCP and the stdio daemon bridge resolve current application authority for each
discovery/direct call (#3336). Hub discovery (#208) uses the credential resolved
by the current request's authentication and its local grant projection, without
per-tool provider calls, mutation rechecks, or token last-used writes. That
projection stays in the request context; the MCP session retains no discovered
permissions. Scope, lesser provider/local role, all-runner grants, and configured
service visibility still filter discovery. Direct calls and approved mutations
independently resolve current authority, including key/user project grant
intersection, expiry and revocation. `TestHubCatalogProviderCalls` counts provider,
resolver and administration calls for browser and project-scoped read/write/admin
keys in shared and dedicated deployments. Principal, organization, credential and session
are bound together; scope and project grants come from the existing credential
and hosted membership services. HTTP dispatch preserves the request authority
without retaining discovered permissions. Both hosted entry paths use the
existing lesser-role rule, so local downgrades apply immediately. Workers and
runners receive no operator authority, and neither transport writes tracker
lanes. MCP action previews reuse the existing chat session and direct dashboard commands
(#3337). Pending previews retain exact arguments, project/resource, client and
original connection authority. Only a browser with an existing authenticated dashboard login/private session
can confirm or reject them; public UI cookies and API credentials cannot approve; tools cannot approve themselves. Ordinary writes run directly, while
material actions require confirmation unless the operator selected YOLO for that
authenticated connection. Mode lives on the server, never in tool input, initialize
metadata or mode headers. Move commands share the dashboard mutation lock and
delegate tracker writes to `ReconcileOperatorMove`; stop commands use `StopRun`.
Approved and YOLO actions call `operatortool.AuthorizeCurrent` again at execution;
approval and YOLO never confer permission. This consolidates authorization in
application adapters without adding a revocation/recovery mechanism (INV-3).
`TestCurrentAuthorityExecution`, `TestRemoteMCPCurrentProjectAuthority`,
`TestHostedOperatorCurrentAuthority` and the shared-origin pilot cover denial,
resource projection and entry/session binding. `TestConnectionActions` and
`TestMCPActionApprovalBoundary` cover exact preview, replay, rejection, stale
targets, closed protocol sessions, self-approval and operator-selected YOLO.

Cloud API and MCP setup (#3745) reuse expiring `api_tokens`, bound to the
issuing user, organization and membership, with selected read/write/admin scope
and existing project grants. The canonical organization MCP endpoint accepts
bearer JSON POSTs without browser cookies or CSRF; shared-entry machine
assertions authenticate transport, while the tenant resolves the operator key.
Every API authentication and MCP discovery/call/approved execution resolves
current provider membership, the lesser provider/local role, active issuer
principal, key expiry/revocation and the intersection of key and user project
grants. Hosted keys persist an explicit all/selected project-access mode (#3837).
All-project keys use the issuer's current grants in the same organization,
including newly authorized projects without key updates or MCP reconnection.
Selected keys retain their own grant rows intersected with current issuer access;
legacy keys migrate to selected, and absent or removed grants never widen access.
Read projections and transaction-time writes enforce the same intersection.
Keys cannot switch organizations, elevate their issuer, become runner
credentials, manage keys through bearer traffic, or approve operations. Browser
key creation/revocation retain session/CSRF authorization; listings expose only
metadata and creation returns the secret once. Key-originated material previews
retain their original credential authority and use the existing browser approval
surface. No parallel credential system or lane writer is added.
`TestHostedAPIKeyCurrentAuthority`, `TestHostedAPIKeyManagement`,
`TestSharedOriginAPIKeyConnection` and key cases in
`TestHostedOperatorPolicyApproval` cover these boundaries. The tested client
handoff and release verification procedure are in [API & MCP setup](api-mcp-setup.md).

Workspace/conversation/action MCP commands (#3346) share extracted native dashboard
commands and the existing `native_commands` receipts. Direct calls and approved
execution resolve current project/resource authority and runner grants; hosted
checks use the same transaction to avoid reacquiring the hub's sole connection.
Conversation command keys, attachment ownership/size rules and configured action
revision checks remain application-owned. Successful action deletions replay their
principal-bound, input-bound receipt after checking current project authority. Workspace paths and relay-session internals
are omitted from MCP projections; terminal/file/SSE requests return explicit
transport decisions with authorized state and polling alternatives. Bounded message
history preserves continuation cursors, and attachment/output/recording reads use
bounded byte chunks. Hosted confirmation reuses connection chat actions and existing
browser session/CSRF authority; bearer credentials never approve. Only the originating hosted
browser principal may choose that session connection's YOLO mode. Daemon chat history and provider turns
bind to the current connection and durable retry identity. Action result payloads remain
private to that session and are delivered only through authorized tool results; public
previews and browser conversations omit them. Durable command receipts retain the
application data for authorized reconnect retries. A nested provider receives
only authorized read tools, while operator actions use the named command tools.
`TestWorkspaceOperatorConversation`, `TestWorkspaceOperatorAttachments`,
`TestWorkspaceOperatorActions`, `TestWorkspaceOperatorRunnerAuthority`,
`TestWorkspaceOperatorHistoryBudget`, `TestWorkspaceOperatorBrowserApproval`,
`TestWorkspaceOperatorUnavailable` and `TestMCPOperatorChatRetry` cover these boundaries.
No MCP adapter writes tracker lanes or introduces a new protection/recovery mechanism.

Issue Q&A conversations (#171) carry a separate, non-unique subject rather than
reusing the canonical worker link. Subject creation validates the current project,
threads remain private to their creator, and linking/sharing a subject chat is
refused. The coordinator refreshes subject records through the existing scoped
work reads each turn; private questions and answers never become comments without
the existing explicit comment approval. `TestConversationSubjectOwnership`,
`TestCoordinatorSubjectRefresh`, `TestCoordinatorSubjectPrompt` and
`TestReadIssueContext` preserve these INV-1 boundaries.

Ordinary native workers (native #145) create their canonical, empty shared
conversation inside the existing lease-fenced bind transaction when none exists.
The selected attempt/run/lease/fencing token remains the sole execution owner;
existing conversation-origin history and private visibility are retained.
Issue Activity/Surface and typed MCP work-item lookup resolve that canonical
conversation independently of paginated conversation lists, with current
project and conversation read authority. Comments and Workpads are never copied
into current control messages. Steering and interrupt retain the existing
delivery receipts, provider turn identity and material-action confirmation.
Unsupported backends remain unsupported, including the separate Claude work.
Workers that skipped binding before this change have no provider control object
to attach remotely; this change does not restart or replace them.
`TestConversationWorkerBind`, `TestWorkItemConversationLookup` and both origins
in `TestConversationRunnerExecutesLiveTurns` cover binding, visibility,
duplicate refusal, actual selected-turn delivery and preserved comments.
Ordinary observational binding preserves the Code/Rework native completion owner
(#165). The fenced bind response derives continuation intent from existing native
link receipts and explicit Continue messages, preserving initial interactive work
and lease-lost continuation ownership across consecutive replacement attempts even
after the original Continue settles as interrupted or unknown. Existing ordered
control events retain that intent until successful execution completion; replacement
binding neither replays the control nor rewrites its original attempt identity.
Matching-attempt Continue remains refused while execution is active.
Registered-runner turn-event POSTs reuse
`events` authorization and the unchanged scoped, runner/lease/fence/attempt owner
checks. `TestNativeRunnerOpensChangeAndLeavesDispatch` exercises successful binding,
host finalization, authenticated transcript events and exact-head version publication;
`TestConversationRunnerExecutesLiveTurns` preserves interactive and explicit Continue
ownership, and `TestConversationWorkerTurnEvents` exercises registered-runner middleware.
This consolidates existing conversation/control owners (INV-3); no lane writer,
recovery loop or second channel is introduced.

Scoped Cloud workspace policy (native #144) survives tenant regeneration through
the existing shared configuration builder. Only the matching organization's
existing `workspaces` section is retained; new tenants stay disabled, and shared
credentials and other settings keep their existing owners. Bootstrap workspace
availability reflects the hosted service, current project/person grants, fresh
active registered runner reports and the existing terminal isolation rule.
Unknown availability disables Surface controls with a safe explanation. Surface
requests name the selected attempt so retained-runner ownership and active-attempt
read-only restrictions remain application-owned. Projection confers no authority:
commands and relay traffic retain current role, grants, owner, path, lease and
plan checks. Terminal launch confinement is independent of provider-worker isolation;
native #162 binds registration, bind, heartbeat and opened isolation to the same
measured terminal support. Darwin uses a deny-default Seatbelt profile, verified
by an actual PTY probe and a confirmation emitted inside each sandbox before
the configured shell starts. Only the canonical worktree is writable; system
binaries/libraries are readable, runner environment and host IPC/network authority
are absent. Symlink escapes, runner-home roots and nested mounts are refused.
Sandbox descendants cannot change session or process group, including through
`posix_spawn`, and cannot read host process arguments/environment. This retains the existing
group teardown authority even after the shell exits. This restricts shell job
control within sandbox terminals. A failed host/probe/launch never opens an
unrestricted replacement. Linux has no implemented confined PTY and reports
`user`; explicit user-policy terminals retain their existing restrictions. Tenant
workspace/terminal defaults stay disabled and sandbox policy continues to refuse
user-only runners. Runtime probe skips do not establish production support.
`TestCloudAllocationGeneratesTenantConfiguration` and
`TestAppBootstrapWorkspaceAvailability` cover persistence and truthful availability.
`TestSandboxUnavailable`, `TestSandboxTerminalContainment`,
`TestSandboxRootAuthority`, `TestSandboxMountContainment`, `TestSandboxChildCannotDetach`,
`TestServiceOpenRefusals`, and the extended close/cancellation/exit table cover
launch refusal, environment/path confinement and descendant teardown. Existing
session and Hub relay tests retain current authority, lease and fencing coverage.
Reviewed deployment and separately authorized scoped enablement remain necessary
before real-workspace browser/API acceptance; see
[terminal operations](conversation/operations.md#terminal-isolation-and-runtime-acceptance).
No mechanism, control channel, configuration key or lane writer is introduced.

Files selection (native #167) stays keyed to the native work item while its
linked conversation becomes available. Navigating to another item restores that
item's panel state before any surface can request its workspace. Workspace
acquisition, event subscription and relay tickets share the current
client/project/item/attempt/capability binding;
changing that binding clears the previous workspace from the rendered surface
before acquisition completes. Revoked availability and unavailable file channels
use the existing Files explanation. `rightPanelWorkspace.test.tsx` follows the
enabled picker through panel state, scoped workspace metadata, ticket minting,
directory listing and a file read, and `workspaces.test.tsx` rejects stale attempt
binding. Existing relay and filesystem authority is unchanged.

Runner enrollment's existing name/new-ID connection match requires a known
initial fleet baseline (native #223). The command consumes that snapshot; an
unavailable read is never an invented empty roster. Initial read loading and
errors use the existing resource readiness and refresh controls, while a known
empty fleet remains valid. The enrollment's serial refresh, close cleanup and
selected project grants retain their existing owners.

Enrolled runner updates (native #93) reuse the installed update scheduler,
configured release discovery, signed artifact/provenance verification, runtime
drain/restart and startup rollback owners. Hub MCP and API queue typed delivery
through existing runner routing/heartbeat and native revision/idempotency receipts.
Current all-project runner administration is rechecked before effects and replay;
MCP retains material-action browser approval and API requires explicit confirmation.
Only the enrolled runner's selected `detent` service is addressable. Tenant authority
cannot update or restart the shared Hub, select a path/URL/command, or gain worker
credential authority.

A queued request is not application or running evidence. The existing scheduler
state retains the request and verified applied artifact before requesting restart;
duplicate delivery does not apply again. Interrupted delivery is uncertain and
does not introduce automatic recovery. Confirmed running receipts survive restart;
a later local update is reported as drift from completed evidence and does not
retain an unsettled remote request. A fresh authenticated heartbeat identifies
the actual process version, commit, source composition, checksum and platform.
Windows detached replacement retains a verified target as pending; matching
post-start process evidence confirms application. Only matching process and
applied evidence establishes running; private patched
source is explicit and never implicitly an approved published release. Missing,
old or stale observations/owners remain unavailable, distinct from denied authority.
Runner routing, leases, isolation, provider choices and reviewed heads remain with
their existing owners. Update refusal receipts omit raw errors, commands, paths
and credentials. `TestSchedulerEnrolledUpdate`,
`TestSchedulerEnrolledInterruptedReceipt`, `TestRunnerUpdateApplication`,
`TestRunnerCapacityHeartbeat` and update cases in `TestHostedMCPFleetControls`
exercise these INV-1 boundaries and the INV-3 consolidation.

Hosted workspace readiness (native #192) uses the existing project SSE owner,
with the selected workspace ID and the same current hosted session, project and
subject read authority as the workspace API. Initial subscription and reconnect
send its current flat resource; subsequent revision changes send its current
`workspace.<state>` resource. Each subscriber reads only that workspace, without
catalog or history replay. Relay session details retain the existing owner/admin
audience; Files, Terminal, close and dispatch-release policies retain their owners.
Project activity uses the indexed maximum collaboration-event row ID instead of
the maximum issue-local sequence, so a terminal event on a less active issue
still invalidates the board. `TestHostedWorkspaceEvents` and
`TestHostedActivityIncludesChangesBelowIssueMaximum` cover these INV-1 boundaries.

Urgent organization release requests (native #186) reuse runner administration,
material-action approval, revision/idempotency receipts and routing delivery.
The existing `draining` state excludes new claims while active leases remain
renewable. Urgency is retained for offline and newly enrolled older runners;
matching process evidence removes only the urgency-derived drain, preserving
administrator-owned disabled or draining settings. Enrolled urgent delivery
uses the runtime context asynchronously so heartbeat and lease renewal never
wait for sessions to finish. The installed updater drains through its existing
reservation and pins the selected published version through signature,
provenance, preflight and rollback verification. Automatic check/apply opt-outs
do not refuse an explicit urgent request. Urgency never cancels a session or
introduces a state, reason code, timer or reconciliation loop.
`TestUrgentRunnerUpdateFleet`, `TestUrgentUpdateOwnerKeepsHeartbeatsAvailable`,
the urgent cases in `TestSchedulerEnrolledUpdate`,
`TestServiceChoosesHubUpdateTarget` and `TestHostedMCPFleetControls` cover
delivery, non-cancellation, restart evidence, pinning and retained authority.

Effective runner capacity (native #90) uses the existing runner administration,
native command receipt, routing heartbeat, selected global configuration writer
and runtime reload owners. UI routing capacity edits and MCP/API capacity requests
deliver the same typed runner-bound application request. Capacity-only requests
preserve grants, isolation, accounts, models and project execution policies.
Expected runner/configuration revisions and current principal authority apply
before effects, and current resource authority is rechecked before receipt replay.
Reads do not record mutation usage. No raw file path, credential, shell command,
new YAML configuration key, polling loop or recovery owner is exposed.

Desired Cloud limits are distinct from observed saved/runtime limits and the
effective runner configuration ceiling. Fresh runner/provider evidence is required;
missing, future or stale evidence remains unknown. Shared host limits and plan
execution permission retain their existing owners. Project, pool and lane admission
remains authoritative at claim time. External provider-capacity producers are
unmanaged: responses name the account/backend ceiling and instruct the operator to
change the producer configuration, never the generated report.
The selected configuration owner reads back saved limits and their revision
after writing, while runtime limits retain reload authority. Application failures
remain visible in subsequent heartbeat observations only for the same selected
configuration revision; a changed configuration or corrected request replaces
that outcome. This retains application evidence without another retry or recovery
owner.

Capacity mutation (native #142) reads the current configuration, rechecks its
revision and enrollment, and changes only `global.max_concurrent_agents` and
`client.capacity` under the global configuration writer's shared mutation lock.
Supported in-process writes use that same lock and persistence implementation;
the capacity callback's mutex only serializes its application evidence. Its
former separate read/check/write could overwrite unrelated settings saved by
another writer, so configuration synchronization belongs to the existing writer.
An intervening saved edit rejects a stale request through the existing revision
constraint; already-saved requested limits retain replay without another write.
`TestRunnerCapacityOwner` deterministically saves credentials, pause, project
settings and membership after observation and before mutation, verifies the save
before resuming, and checks preservation and current-enrollment authority. This
consolidates INV-1 enforcement without another writer or recovery mechanism
(INV-3).

`TestRunnerCapacityOwner`, `TestRunnerCapacityHeartbeat`,
`TestRunnerCapacityApplication`, the capacity cases in `TestHostedMCPFleetControls`
and `TestFleetArgumentBoundary` cover persistence, reload, old-Hub compatibility,
current authority, bounded input, revisions, replay, redaction and real additional
claims. This extends INV-1 adapters without another capacity policy or lane writer.

Fleet/operator MCP adapters (#3343) share the dashboard refresh, availability
clear, canary, update, progress-credit, warning-acknowledgment, recovery and
runner administration commands. Runner display edits execute directly; capacity,
scheduling, access, enrollment, revoke, stop and other material actions use the
existing authenticated browser approval surface. Unknown action kinds still
require confirmation. Fresh authority resolution binds application command
credentials from the originating connection, including when another authorized
browser approves. Hosted runner administration retains current all-project
runner grants and transaction-time membership/credential checks. Browser previews
containing fleet actions also require current runner grants, including for owners.
Hosted fleet approvals (native #168) bind the selected runner, exact mutation
arguments and identity, current routing revision and credential-backed
revoked/expired state through the existing proposal and execution comparison.
Online/offline heartbeat health and problem observations remain runtime evidence;
they cannot invalidate a pending configuration approval. Material-change
classification uses the recorded configuration mismatch, not heartbeat freshness.
Execution retains material-change equality after exact browser confirmation;
actual capacity application still requires fresh evidence. Current
original-connection and approving-browser authority, project
grants, CSRF/form binding, durable command receipts and transaction-time expected
runner/configuration revisions retain their existing owners. The heartbeat,
revoked-runner, original-session, cross-organization and durable-retry cases in
`TestHostedMCPFleetControls` cover routing resume, capacity and identity revocation
in both hosted deployment modes.
Health and outbox reads retain dashboard deployment boundaries; AI debug projects
its snapshot through current project grants. Missing runtime
or approval services return opaque unavailable results; an unhosted hub bearer
credential cannot stand in for a human. Worker enrollment redemption, credential
renewal/rotation, claims, leases and heartbeat protocols remain worker-only.
`TestMCPFleetReads`, `TestMCPFleetMutation`, `TestMCPOperatorControls`,
`TestMCPStopExactRun`, `TestHostedMCPFleetControls`, `TestHubMCPFleetBoundary`
and `TestFleetArgumentBoundary` cover current authority, bounded calls, exact
targets, confirmation and safe retries. No adapter writes tracker lanes or adds
a protection/recovery mechanism (INV-3); recovery/stop use existing services.

SSH worker callbacks keep session persistence, Workpad tools, lane decisions, and execution authority on the central owner. Remote process IDs never become local reap authorities. `TestSSHCallbackDoesNotPublishRemotePID` and `TestSSHServiceProxyKeepsCentralAuthority` cover these transport boundaries (#3239).

Hub-native SSH runs capture Git artifacts and attempt diffs on the selected
host while the central execution owns upload journals, credentials, producer
lease/fencing identity, Change Request publication, landing reports and provider
reservations (#3358). SSH protocol version 2 prevents older workers from silently
omitting native callbacks. `TestSSHNativePublication`, `TestSSHWorkerLifecycle`,
`TestNativeExecutionLandsReviewedVersion` and `TestLocalProviderRevalidation`
cover exact remote publication, retained journals and native capabilities.

Managed local configuration commands (native #94) reauthorize the exact selected
project and reuse existing operator approval and durable command receipts. Reads
report actual selected configuration/workflow revisions and policy identities
without credentials, raw instructions or paths. Committed policy application
requires its exact approved identity, source revision, current configuration
revision and a paused project with settled work. Detach requires the mapped
native project's authenticated durable cutover checkpoint and no active or
deferred completions. It removes only that local registration through the
existing validating writer. Saved removal remains distinct from runtime detach
until the existing reload confirms it; missing or stopped owners never start
a board or claim application. Cloud routing, runner association, user pauses,
leases and immutable native Change/version/head authority retain their owners.
`TestManagedProjectConfiguration`, `TestMCPLocalProjectConfiguration`,
`TestLocalProjectCutoverReceipt` and `TestHostedProjectTools` cover these bounds.

**Statement:** The orchestrator is the only writer of tracker lane state.

Native worker finish prepares the result and Change Request while retaining the
existing claim. The orchestrator publishes plan and review evidence, writes the
lane through its ledger, and records `run.finished` when surrendering that claim
(#3437). Authorized native code and rework retain that completion owner during
graceful drain (#152); drain stops intake without discarding genuine publication
or its allowed lane transition. Final diff, Change reads and creation failures
remain publication diagnostics, while actual claim, lease and fencing loss keep
their existing instance owner. Version refusals retain the diagnostic and code,
enter the configured review lane, and cannot inherit an earlier version's approval.
Automated native plans return their review section in the worker result;
workers never publish approval comments or move lanes. Native workflows are
resolved before planner dispatch: approved plans enter In Progress, while holds
use the configured plan stop or review destination, including the existing
In Progress intermediate transition when required. Incompatible destinations
prevent planner dispatch. P1 plan rework retains planner provenance. Abandoned
plan attempts cannot undo an operator's implementation handoff just because
they have no PR. `TestNativePlannerAutomaticHandoff`,
`TestNativePlanWorkflowHandoff`, and
`TestReconcileTerminalAttemptRetryStatesDemotesRecoveredEmptyAttempt` cover these
boundaries, including actual subsequent Change Request version publication.

Merging promotion reads only the trusted audit verdict for the tick's hydrated
PR head and base (#3436). Passing and running audits do not repeat PR, check,
review, or thread hydration; merge preparation remains the owner of live merge
eligibility. A lane-changing audit verdict still refreshes the live PR identity
before routing to Rework, and degraded/unavailable snapshots cannot route it.
`TestMergingPromotionUsesFetchedAuditIdentity` covers these ownership boundaries.
Tick entry publishes the existing runtime overlay before blocking tracker reads;
`TestPromotionReadKeepsRecoveredWorkersObservable` replays nine active attempts
behind an empty prior snapshot and verifies fresh progress during slow promotion.


Validator reviews now refresh the PR head before dispatch, seed a clean review
workspace at that commit, and compare the current PR head again before storing
the verdict. A changed head produces the existing wait verdict under the head
that was actually reviewed; the next tick evaluates the new head. This keeps
validation evidence attributable to its commit without giving workers lane
ownership. The runner also verifies the review tree after `before_run`, after
the validator turn, and after `after_run`. It also checks HEAD before and after
cleanup, so a cleanup hook cannot hide a commit change. The PR ref is fetched
from the hydrated PR repository, even when the source checkout is a fork.
Workspace failures remain instance
failures even when the validator turn also fails, and cannot yield a verdict.
`TestRunnerValidateRejectsReviewWorkspaceMutation`, `TestValidatorStageTracksHeadBeforeAndDuringReview`,
`TestLocalGitSeedReviewHead`, `TestLocalGitSeedReviewHeadUsesPullRequestRepository`,
and `TestLocalGitVerifyReviewTreeAfterSeeding`
cover these boundaries (#3031).

Validator verdict identity also binds the current task title/body, referenced
authoritative qualification evidence, and effective review instructions to the
repository/PR/base/head provenance (#3087). Fresh tracker input is read before
reuse and again before publication. Legacy rows without a context digest cannot
approve or reject current code. Unrelated comments, timestamps and Workpad
progress prose do not change identity; referenced qualification evidence does.
`TestValidatorAcceptanceContextIdentity`, `TestValidatorContextMemoReuse`,
`TestValidatorContextSchedulesOnceAndRejectsHeldResult`,
`TestValidatorLegacyContextNotReusable`, and `TestValidatorContextStorageRestart`
cover these boundaries. This extends the existing validator lifecycle without
adding a recovery mechanism or granting a gate waiver.

Verified PR delivery uses immutable forge `MergedAt` when hydrated, otherwise a
successful programmatic merge's post-API observation time (#3482). The existing
lane ledger and phase metadata record delivery time and its source; worker attempt
completion retains the worker's timestamp. Receipts and merge durations use the
delivery clock. Historical ledger rows without integration evidence retain their
recorded attribution. `TestHandleRunResultProgrammaticallyMergesCleanMergeWorkerWithoutTerminalState`
replays the #3445/#3465 audit and Chicago midnight; the operator, observed-merge,
and merge-duration fixtures cover the other existing completion paths. Lane
ownership, failure routing and verified outcome deduplication are unchanged.

Completion classification preserves genuine diff progress even when a structured
Workpad reports `in_progress`; current unfinished work cannot promote on completion or
the Rework tick. Completion and promotion use the same forge-over-assertion
interpretation (#2949): a hydrated merged PR, or a non-draft open PR whose head
commit is strictly newer than the recorded structured Workpad, supersedes an
`in_progress` assertion without explicit blockers or human action. Missing or
equal timestamps retain the open-PR assertion. This interpretation does not
rewrite the parsed or stored Workpad, credit an artifact receipt to spend/progress,
or manufacture a current-attempt completion claim for cleanliness accounting.
`TestCompletionForgeEvidencePreservesReceipt` exercises these shared consumers.
`TestCompletionForgeEvidence`, `TestCompletionForgeEvidenceClassification`, and
`TestCompletionForgeEvidenceDispatch` cover classification and dispatch in both
active lanes; no-PR work remains eligible for implementation. A rebase with the
same captured PR diff fingerprint is no-progress unless a current-attempt Workpad
completion claim or superseded stale assertion is corroborated by an open,
non-draft, conflict-free PR with no failing CI (pending is allowed), or
the tracker stops reporting a known conflict in a recognized mergeability state.
Unknown includes tracker recomputation and credits progress without granting merge
readiness. Attempt identity prevents stale-claim replay; tracker evidence provides
the independent corroboration. The dispatch fingerprint and mergeability baseline
persist with the attempt across restart, including merge-mode conflict repairs
in In Progress and ordinary implementation repairs in Rework (#2929, #3547);
a changed head SHA alone is not implementation. No-progress outcomes retain
classification under the existing configured signature limits and cannot enter
Human Review or `awaiting_gate` as successful work. Historical lifetime session
counts no longer override current completion and dispatch evidence. Draft PRs
remain ineligible for review and the tick never marks them ready.
`TestReworkLivePullRequestPromotion`, `TestReworkLiveDraftPromotion`,
`TestUnfinishedCompletionClassification`, `TestCompletionRebaseProgress`,
`TestUnfinishedSessionsRetainConfiguredDispatch`, and
`TestCompletedActiveReviewRequiresFinishedWork` cover both active lanes,
appended prose, current signature classification, and ready-PR controls.

A persisted successful completion with a complete Workpad and ready PR remains
owned by the existing active-lane gate until the matching head is eligible for
Merging. A replacement head or an ended
attempt without completion remains in In Progress for normal dispatch; elapsed
time without a worker is diagnostic only. The stranded-active sweep no longer
writes a Todo or Rework lane transition (#3238), removing its competing lane
decision. `TestCompletedReadyPullRequestEntersMergeGate` and
`TestTickDispatchesPlanApprovedIssueAfterLongRefresh` cover completion and
next-cycle dispatch.

A structured merged-completion receipt can replace an absent or closed-unmerged
PR association only after native lookup verifies the exact repository, PR URL,
number, merged state, head, and base branch. The injected receipt contract names
the same fetched integration branch/head actually ancestry-tested, matching the
merging PR's base rather than the worker workspace branch. Pending scheduled
acceptance is not recorded as passed. The existing merged-PR owner scans
active and observed lanes, including Blocked, and completes that verified work
without another worker receipt. Completion classification shares that owner’s
already-merged decision rather than waiting for new head checks after merge.
Open associations, incomplete ancestry receipts,
human actions, unavailable native evidence, and failing checks retain their
existing behavior. `TestMergedCompletionReconcilesClosedDraft` covers stale draft
associations, native reopening, and both completion and refusal controls.

Successful persisted worker completion delegates an already-merged PR to this
same owner before scheduling a continuation (#3576). A finished worker's retained
claim no longer excludes merged delivery; running workers and claimed open PRs
remain excluded. Resolving a merged operational receipt preserves its supplied
attempt/generation attribution and requires native merged evidence, rather than
falling back to assertion-only operational completion.
`TestMergedCompletionRefreshReplacesClosedDraft` replays #3533's attempt 7516,
generation 118, clean workspace and merged PR #3555: Done replaces the continuation
that redispatched attempt 7522. Its stale-attribution and missing-evidence cases
retain rejection without changing lane ownership or adding a recovery mechanism.

Authorized no-PR operational delivery reads the structured receipt from the
canonical issue-body Workpad as well as Workpad comments (#3425). GitHub
hydration and completion classification share the section parser; protocol
examples outside that section cannot complete an issue. Current canonical
Workpad selection uses the comment's updated time, falling back to its created
time. Equal timestamps retain reverse input precedence; unavailable ordering
metadata retains that same precedence for the entire canonical comment set.
Selection precedes parsing: a selected invalid, cleared, or human-held Workpad
cannot fall back to older comments or issue-body evidence. Signal parsing,
artifact receipt/status hashes, session progress, and current human clearance
share this selection owner. Producer handoff instructions target that same
canonical comment: edit it when permitted, otherwise publish a new comment
through the existing tracker writer. Issue-body or final-answer completion
cannot supersede a canonical comment. Native/local event ownership remains
unchanged. Verified evidence for an unchanged PR head and test inputs can be
referenced to publish its receipt without rerunning solely for publication;
pending acceptance, project verification and human approvals remain required.
The explicit completed-receipt history search keeps
its existing reverse traversal and skip semantics. This consolidates current
comment authority under INV-3 without changing authorization, native dependency
proof, or adding a recovery mechanism. Acceptance still requires the dispatch-time
and current operational authorization, clean delivered workspace, and matching
attempt/generation when the receipt supplies them. The accepted signal and
generation persist with the existing successful attempt. Both cached promotion
and restart restoration require unchanged receipt fields; an unrelated issue
timestamp update does not revoke an accepted delivery. Legacy successful
unattributed receipts retain the existing timing check and cannot acquire a
generation from current issue text. Historical no-progress receipts do not
manufacture acceptance. The existing orchestrator completion transition retires
accepted work to Done without another model session, retaining Human Review when
required. `TestOperationalBodyCompletionSurvivesRestart` reproduces #3058's
attempt 7018 / generation 25 publication and completion followed by the
2026-09-30T14:14:11Z restart reclaiming unrelated work; it also covers refusal
of changed evidence and stale attribution. Live attribution cases extend
`TestHandleRunResultClassifiesImplementWorkerProgress`.

An operator rejects a reviewed PR by moving its card to Rework, including through
the dashboard. The existing lane history records the PR identity and hydrated
head. Both completion and auto-promotion consume this durable rejection evidence:
the same head cannot re-enter review after a restart or a successful worker report,
while a different head follows the normal gates. Rejected work remains eligible
for a repair worker instead of waiting on its old completion. Automated Rework
routing does not imply an operator rejection. This consolidates review eligibility
with the lane ledger without adding a label, reason code, or configuration key.
Forge hydration is best-effort so a PR endpoint failure cannot veto an operator
lane move or abort the project tick. Soft hydration failures also discard cached
heads. Lane observations remain authoritative when no timeline reader is configured.
Unknown rejected heads require a commit
strictly newer than the move. History read errors hold only the affected card;
the independently durable human lane observation also prevents promotion when
the best-effort history event was lost. Missing event evidence holds repair
dispatch until a newer commit establishes progress, rather than allowing a lane
change to erase the remaining rejection evidence.
`TestOperatorRejectionPromotion` and `TestOperatorRejectionRepairDispatch` cover
unchanged and new heads, drafts, dashboard and observed moves, and repair dispatch
(#2943).

Human Review is entered only when the project explicitly sets `review.human: true`
and a review outcome or gate requires it. With `review.human: false`, the
orchestrator routes review holds, legacy triage publication, gate timeouts,
and draft PRs to Blocked with their existing reasons. A legacy
`requires-human-review` label and a configured `optout_label` are review holds,
not permission to enter Human Review. New projects default to false. This
consolidates review routing under the project setting (#3211). Allowance triage
notes and historical `attempt_allowance_exhausted` receipts remain readable;
operator moves still clear auto-promote decision memory.
The dependency auto-unblock sweep retains these non-review decisions in Blocked
even when an unconsumed body or native dependency is already Done; both reasons
reuse the existing sticky-reason policy.
Current recorded sticky reasons are evaluated before dependency hydration and
comment reads; blocked cards that cannot auto-unblock do not consume tracker
requests. Freshly hydrated workpad reasons still use the same sticky policy,
and mismatched lane timestamps continue through ordinary hydration.
`TestDependencyAutoUnblockRetainsNonReviewDecision` covers the absence of tracker
reads for persisted allowance and workpad holds.

Any closed issue, regardless of its closure reason, retains its
non-terminal label snapshots in ordinary refresh reads, even without prior pipeline membership (#2865). The existing
`reconcileClosedCompletedIssueStatuses` owns their transition to Done; dispatch
continues to exclude closed issues. `TestTickReconcilesClosedLabelsWithoutPreviousPipeline`
covers first-refresh and between-refresh closure without direct-ID retention.
Any closed issue leaves non-terminal lanes on the first successful reconciliation.
Transition refresh reuses the current successful candidate and status scans for
retained pipeline, watched, and blocked identities. It fetches retained identities
missing from those scans so external moves and closures still resolve; a failed
status scan preserves the existing direct refresh and retention behavior.
`TestRefreshTransitionSetsReusesStatusSnapshots` covers fresh closed and moved
snapshots, missing identities, and failed status scans without duplicate reads.
Non-completed closures are also removed from the board, pipeline, and active-work
tracking, including the lane writer’s pending publication overlays (#2869).
Completed closures retain their existing immediate Done transition visibility.
Accepted operational completions also close the issue in the existing terminal
transition; merged PR completion does not depend on a closing keyword (#2911).
When GitHub closes a linked issue as completed during the merge, a redundant
close returning 422 is accepted only after a fresh issue read confirms that
closed/completed state. Open issues and other closure reasons retain the close
error, so the terminal lane is published only after confirmed closure (#3028).
The existing completion comment links the issue closure.
`TestCompletionTransitionClosesIssue` covers both completion paths.

ProjectV2 combined refresh applies the instance author, assignee, and label
predicates before scheduler enrichment (#2917), including fallback readers.
Selector connections must be complete. ProjectV2 refresh pushes representable
label and assignee predicates into the board query (#2930); author predicates
and search-syntax-sensitive values remain local. Excluded cards are never
dispatch candidates or authorized status repair targets. Filtered scans obtain
external blocker lanes through targeted project-item reads rather than whole-board
enumeration; native closed and human-prerequisite evidence remains authoritative.
Unfiltered scans retain thin excluded-card dependency evidence and diagnostics.
`TestRefreshBoardFilterCost` checks that estimated query points grow with selected
cards, not unrelated board size; `TestFilteredRefreshExternalBlocker` covers
external lanes and human prerequisites. Completed scans reread scheduler evidence even when issue
and item timestamps are unchanged. Interrupted scans retain their existing
resume validation. `TestProjectRefreshInstanceSelector`,
`TestRefreshSelectorPredicatesAndMetadata`, and
`TestRefreshSelectedEvidenceChangesWithoutIssueTimestamp` cover this boundary.
`TestProjectRefreshInstanceSelectorCandidateDelivery` verifies the five incident
identities reach the dispatch hook without additional issue reads.

Fetched candidates and observed lanes share the dispatch authorization selector
before tick recovery and reconciliation (#2914). Declined cards are removed from
retained transition inputs too; the configured-selector dependency sweep waits
for this boundary instead of writing from an earlier board snapshot. Each
excluded identity contributes to the aggregate skip count; only open cards in
configured active, non-terminal lanes record a per-card scheduler decision
(#2916). `TestTickAuthorizationDeclineMixedLanes` covers mixed lanes, custom
active states, terminal overlap, closed cards, and duplicate retained inputs. `TestTickAuthorizationBeforeRecovery` covers mixed instance labels across
stale Todo PR reconciliation, blocked recovery, and both
dependency sweep orderings. `TestTickAuthorizationSelectorSemantics` preserves
nested selectors, identity/field predicates, and declined retry cleanup.

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

Isolated landing preparation (#170) retains existing repository, dirty landing
checkout, moved head, unavailable source and conflict refusals and infrastructure
attribution. A dirty Code checkout or checked-out source branch is independent
source ownership, not a landing failure. Hydration uses only the authorized
source and existing authentication; no sibling-worktree authority or blanket
shared metadata write grant is added. `TestLocalGitCreateReviewedLanding` and
`TestLocalGitLandChangeViaGitHub` cover hydration, preserved owners and truthful
landing refusals, including External conflicts using the real source branch.

Worker process cleanup failures (#169) retain staged source and use the existing
instance workspace-failure owner, excluding them from issue failure allowance.
A cleanup-stage deadline does not establish parent or provider cancellation.
Genuine provider failures and cancellation never authorize source publication.

Native claim policy heartbeats (#151) share `nativeClaimError` with machine
heartbeat and lease renewal. Contention on the enrolled runner's identity-file
lock during credential rotation, policy transport failures, rate limits and Hub
unavailability retain the active claim and worker without an issue failure or
lane transition. These errors do not renew or extend the lease: existing native
fencing and execution deadlines still reject expired authority. Local descriptor
and runner-selector mismatches use the existing typed `policy_mismatch` and
`selector_no_match` errors; actual policy, lease, identity and scope refusals
remain fatal, and only the matching fencing token's cached claim is discarded.
`TestRunnerClientEnrollmentSchedulingAndRotationRecovery` pauses the real
credential rotation owner while a native execution is active, verifies unchanged
issue and attempt records, and then renews with the same identity and grants.
`TestNativeSchedulerReportsItsUnapprovedPolicyOnce`,
`TestNativeDelayedResponsesPreserveSuccessor` and
`TestNativeGuardDeadlineAndRenewal` retain mismatch, fencing and expiry coverage.
This removes the blanket policy-error claim-loss classification under INV-3;
credential rotation, atomic private persistence and pending-credential recovery
keep their existing owners, with no new mechanism or authority bypass.

Native execution and reporting (#210) share the retained fenced lease and actual
authority-loss classification with renewal. Temporary transport and server
unavailability inside that lease do not cancel the current worker or turn a
pending event into a failed issue attempt. Pending events retain their sequence
and idempotency identity; reconnect reuses the same execution. Unauthorized,
revoked, stale fencing and expired authority still stop execution, and mutations
continue fresh server-side authorization. Permanent protocol and publication
refusals are not temporary outages. A final publication outage remains with the
existing completion deferral, which retries the retained publisher before
replaying the completed result. Abandoned instance interruptions remain eligible
for existing recovery but do not count as issue failures.
`TestNativeExecutionTransportKeepsCurrentWorker`,
`TestNativeExecutionSettlesFinishedRun`, `TestNativeChangeRunCompletion` and
`TestConsecutiveRetryCycleCountAcrossServiceRestarts` cover these boundaries;
existing guard and successor regressions retain expiry and fencing safety.

Native code and rework completion (#152) requires a stored final diff and
available fenced publication authority before a genuine deliverable advances.
Missing diff and Change evidence retain their actual publication diagnostics
through the existing completion handoff. Version publication refusals retain
their diagnostic and code, preserve the previous immutable version, and move to
the configured review lane without inheriting earlier approval or redispatching
coding. Only actual claim, lease or fencing loss uses the existing
execution-authority completion owner, without charging an issue failure.
Absent native results remain with existing source and conversation continuation
owners. Claim release retains failed publication preparation.
`TestNativeExecutionSettlesFinishedRun` and
`TestNativeChangeRunCompletion` cover these outcomes.

Native runner GitHub PR landing (#89) uses the existing REST client classifier,
response accounting, and instance REST capacity completion owner. Actual primary
or secondary quota responses retain their credential identity, reset and
Retry-After evidence in the existing persisted wait contract, without fabricated
reserve or reset values. The reviewed Change Request/version/head stays in
Merging across retries and restart; capacity completions consume no failed coding
attempt allowance. Native coding and completions that need no exhausted GitHub
operation continue normally. Pre-claim readiness, dispatch, retry and completion
share the existing current-stage REST dependency decision (#150). Historical
retry capacity scope is retained as evidence, not an independent applicability
authority: due native Rework coding and Git-only landing remain eligible, while
current native GitHub PR landing and non-native GitHub work remain held during
an active REST outage. Retry ownership and immutable attempt history remain
unchanged.
Explicit native `worker.github_token` access retains
the existing worker credential policy, isolated environment and capacity accounting;
omitted access performs no worker credential lookup or GitHub request. Synthetic
quota probes cannot clear actual landing response evidence; the runner retries
through ordinary landing dispatch and fresh same-credential operation evidence
establishes recovery. Quota evidence takes precedence over joined repository
refusals while preserving the exact reviewed identity. Reviewed-head hydration
and external PR validation use the same runner REST client as landing, so quota
errors during workspace preparation retain that identity, actual usage and an
honest failed native Finish for the existing capacity completion owner. External
PR heads remain read-only and retain their repository/base authority. Authentication,
review, check and repository refusals keep their existing handling. Runner
credentials remain local. This consolidates classification and completion under
INV-1/INV-2/INV-3 without a mechanism or lane writer.
`TestLocalGitLandChangeViaGitHub`, `TestNativeLandingQuotaWait`,
`TestLandNativeChange`, `TestNativeLandingQuotaFinishesRun`,
`TestRunnerResolvesLandingBeforeWorkspace`, `TestLocalGitCreateReviewedLanding`,
`TestNativeRunnerPublishesOnlyAfterRecovery` and `TestSSHErrorRoundTrip` exercise
these boundaries.
`TestHubSchedulingReadinessBeforeClaim` covers historical REST scopes through
pre-claim readiness, retry planning and final dispatch admission.

Native code commit and rework Git preparation/finalization failures (#141, #156)
use the existing
`workspace_preparation` outcome, including failures after a source-resolution
turn. The existing instance failure owner retains the issue's runnable lane and
excludes the attempt from its failure allowance; successful source turns retain
their existing progress semantics. Genuine unresolved source conflicts remain
resolution failures. `TestWorkspacePreparationDrainsInstanceAndPreservesIssueFailureBreakers`
covers metadata/signing failures after resolution, issue allowance, and continued
eligibility of other runnable work without a new failure family or scheduler.
Host commit signing and authority failures preserve staged work and the existing
checkpoint/retention owner, skip immutable artifact capture and version publication,
and report a failed native Finish. They never become additional source findings
or authorize another issue coding attempt. Current requested-change feedback and
the prior immutable version remain intact; unavailable authority cannot write a
new checkpoint. The staged/signing cases in
`TestNativeReworkFinalizesBeforeImmutableEvidence` and
`TestNativeRunnerOpensChangeAndLeavesDispatch` cover this completion boundary.
Host staged commits suppress all repository hooks for that command with
`core.hooksPath` set to the platform null device. Signing capability remains
host-owned; tracked hooks cannot run with host credentials. Repository hooks
configuration, author identity and signing policy remain unchanged, covered by
`TestLocalGitNativeWorkDisablesTrackedHooks` for unsigned Code, SSH-signed Code
and SSH-signed Rework.

Native landing merge conflicts are repository refusals, not infrastructure
failures or shipped work. Explicit mergeability evidence in the decoded GitHub
HTTP 405 `message` requires current source verification before the existing
`LandRefusalConflict` can select Rework. A clean or unproven source response uses
the existing `LandRefusalBaseMoved` and item-local landing continuation (#193),
without registering or extending a host server outage. Authentic HTTP 5xx,
transport and timeout errors reuse the forge classifier and outage backoff.
Unspecified 405 responses, actual branch protection, required checks and reviews
retain their refusal classification.
Metadata and malformed response text cannot establish conflict or closed-PR state.
Quota responses retain their separate owner (#89) and never become conflict
evidence. Unreadable workflow state or an unavailable refusal transition uses
the existing deferred completion owner without inventing a successful landing.
`TestLocalGitLandChangeViaGitHub` and `TestNativeLandingRunCompletion` exercise these
classification and settlement boundaries (#96).

Cloud shared provider capacity aggregates reports only from runners with current,
unrevoked authority, using the existing runner validity interval contract.
Expired and revoked identities retain their historical reports but cannot clamp
live concurrency or availability. Draining and offline runners with valid
authority still contribute. Live, unexpired lease reservations remain counted
once per lease and retain their pinned concurrency bounds until release or
expiry, independently of reporting authority. Current same-account exhaustion,
unknown availability, organization scope and account isolation remain
conservative. `TestProviderPoolIsolation` covers these boundaries alongside the
existing provider observation and concurrent-claim fixtures. This consolidates
report authority under the existing runner owner (INV-3), without a cleanup,
recovery path or new capacity mechanism; provider capacity remains instance-owned.

SSH host loss uses the existing host-scoped instance-capacity retry without incrementing issue failure counts or draining healthy hosts. `TestSSHHostLossUsesInstanceRetry`, `TestSSHHostLossClearsResumeOnSpillover`, and `TestSSHLocalTargetIntegration` cover attribution and retry behavior (#3239).

**Statement:** Infrastructure failures attach to the instance, never to the issue, whether they happen before the first agent turn or during a turn.

Pi RPC launch, framing, correlation, and transport failures (#2469) reuse the
existing instance backend-capacity outcome, including local providers. No Pi
reason code or recovery mechanism is introduced. Restricted requests fail before
process launch because Pi cannot enforce sandbox/read-only policies.
`TestInfrastructureClassification`, `TestRestrictedRequestsNeverLaunchPi`, and
`TestRunTurnReapsProcessGroup` cover attribution and subprocess ownership.

Pull-request hydration failures retain the existing per-PR readiness refusal and
credential-scoped REST reserve/backoff (#3497). Fresh successful reads restore
normal admission without a project-wide worker-progress canary; missing or
unavailable PR evidence remains conservative. Backend recovery remains unchanged.
`TestPullRequestHydrationRecoveryKeepsAdmissionWithFreshEvidence` reproduces a
quota-refused PR followed by three fresh candidates without waiting for a worker
turn to admit the remaining candidates.

Linked native GitHub issues hydrate title, body and complete paginated discussion
with runner credentials before agent dispatch (#3257). Missing credentials,
inaccessible sources, throttling, partial reads and failed persistence release
the existing claim as `work_item_hydration_failed` and return the existing
instance scheduling diagnostic, without changing the issue lane or spending an
agent attempt. `TestNativeSourceIntakeRetryBeforeDispatch` covers failure,
retry and zero source calls after successful intake. Atomic intake retains the
source snapshot and imported comment provenance while native edit history
preserves field ownership (`TestLinkedIssueIntakeAtomicAndNativeEdits`).

Onboarding batch intake (#3258) reuses the same atomic source hydration and
native field ownership. Explicit source requests ride the enrolled runner's
existing heartbeat response; Hub never fetches GitHub issues or stores the
source credential. Failed or throttled items remain incomplete until an
operator retries, and completed intake delivers no further source tasks.
`TestGitHubBatchIntakeRetryAndNativeOwnership` and
`TestGitHubBatchDiscoveryFailureAndSourceValidation` cover these boundaries.
Selection defaults to a configured non-dispatchable lane; dispatchable intake
requires explicit operator approval and uses existing native scheduling,
without an additional hold or lane writer (INV-1 and INV-3).

Validator PR-diff provenance failures are production failures, not code findings (#3066).
The validator uses a repository/PR/base/head snapshot whose file list and patch
agree, records its digest and identity with the verdict, and reuses only a
matching persisted verdict. Missing or mismatched evidence stays in the
existing validator retry path and cannot publish a blocking severity finding.
GitHub diff fetches retain the client's retryability: permanent failures use
the existing instance-owned validator infrastructure path, while transient
fetches and changing PR evidence remain retryable. Persisted retry deadlines
also survive a restart when `max_attempts` is one and the stored attempt count
is zero.
`TestPullRequestValidationDiffConcurrentPRs`,
`TestValidationDiffRejectsOtherPRFiles`, and
`TestValidatorVerdictRejectsDifferentPRProvenance`,
`TestPullRequestValidationDiffPreservesFetchFailureClassification`,
`TestValidatorDiffFetchFailureUsesInstancePath`, and
`TestValidatorRetryDeadlineSurvivesReloadWithOneMaxAttempt` cover this boundary.

Terminal failure accounting excludes successful reports whose stored blocker
evidence includes an instance owner. Other ownership, absent evidence, and
malformed metadata do not grant that exclusion.

**Why:** Backend startup, protocol, and workspace-hook failures previously parked
innocent issues and left the operator to return them to work.

**Enforcement:** `TestPreTurnFailuresDrainInstance` and
`TestObservedLanePreTurnFailureRemainsInstanceOwned` exercise instance attribution
and preserve issue ownership even after an observed lane change.
`TestRunnerWorkspaceTimeoutIsNotForgeUnavailable` keeps `Create` failures under
workspace preparation even when an `after_create` hook includes a loopback URL
and timeout text; the runner no longer presents every workspace failure as a
`git fetch` failure. `TestWorkspacePreparationDrainsInstanceAndPreservesIssueFailureBreakers`
checks that these failures drain the instance without a forge condition or
issue retry accounting. Actual remote write timeouts still use forge availability.
Remote Git reads during workspace preparation use forge availability only when
a matching forge host and `ls-remote` or `fetch` operation identify the failure.
Structured Git command errors and direct `after_create` hook commands establish
the operation; compound hooks need the Git-specific SSH refusal in their output.
An unrelated later command in a compound hook cannot inherit an earlier Git read.
SSH refusal, transport loss, and 5xx responses retain the original workspace
error, enter the existing forge retry with backoff, and do not count toward the
project breaker. `TestRecoverDurableWorkspaceGitReadWait` verifies that an
`ls-remote` wait restores its forge condition and retry deadline after restart.
Workspace command and `after_create` hook ENOSPC failures use
the existing retry backoff as instance-owned capacity failures, with a host disk
alert on the board and health view; they do not count toward the project breaker.
`TestWorkspaceDiskExhaustionRetriesWithoutProjectBreaker` reproduces the recorded
Git-index and hook failures, and `TestBoardAndHealthShowHostDiskExhaustionRetry`
checks the operator signal. Other preparation failures remain instance-owned; their breaker
uses `agent.failure_breaker.cooldown_seconds` rather than the separate
blocked-recovery cooldown. A checked clean merge that needs no new workspace
always uses the existing no-workspace merge-control path, including when worker
capacity is free and no breaker or forge condition exists. Fresh hydration must
preserve the checked head and a non-strict base policy; advancement of a
non-strict base does not require workspace synchronization or a CI retry.
`TestReadyMergeAllowsNonStrictBaseAdvancement` covers the native merge of the
unchanged checked head; the safety matrix retains changed-head, strict-policy,
CI, draft, thread, and operator-withdrawal controls. A Git-read condition holds
its failed operation retry, not unrelated merge preparation. Dirty or unchecked
PRs still require their ordinary preparation and merge eligibility; removing
the separate read hold does not make them ready to merge. Forge write and
credential conditions retain their existing merge hold.
`TestClassifyWorkspaceForgeReadFailure`,
`TestWorkspaceSSHRefusalDoesNotTripProjectBreaker`,
`TestRecoverDurableWorkspaceGitReadWait`,
`TestWorkspaceBreakerHonorsFailureCooldown`, `TestReadyMergeAtWorkerCapacity`,
and `TestCheckedMergeUnderForgeCondition` cover these boundaries (#3067).
`TestAttemptAllowanceTriageInfrastructureFailure` applies the same instance-owned
completion handlers to triage workspace, startup, transport, protocol, and capacity
failures. These failures publish no triage comment and do not consume the triage
pass; recovered admission uses normal configured dispatch. The shared Codex
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
Worker access failures remain classified as instance-owned. In an autonomous
issue-worker run, product or design questions are expressed through a blocked
Workpad `human_action`, not a worker question tool: its Codex app-server is
never started with `default_mode_request_user_input`, and a question it asks
anyway is answered empty. A conversation turn, where a person is live in the
chat and the hub relays the question to them, may use `request_user_input`.
`TestAppServerMarksOnlyConversationTurns` and
`TestBuildCodexCommandEnablesQuestionsOnlyForConversationTurns` keep the
question feature off every ordinary issue run.
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

Completed plans checkpoint their result before publication and reuse durable
completion deferral when comment publication or the lane coordination fence
fails (#2919). The original attempt stays in `tracker_unavailable` wait, honors
typed GitHub retry/reset deadlines, and never charges project failure breakers
or launches another planner. A durable publication receipt and completion marker
reconcile retries and the crash window after a comment write. The existing shared
schedule owner excludes a second host while completion waits; no new ownership
mechanism is introduced. `TestPlanCompletionCoordinationDeferral` covers retries,
SQLite restart, two-host coordination, artifact counts, and attempt attribution.
`TestPlanCompletionPublicationRecovery` covers uncertain writes and the
publication checkpoint. Both run through the invariant manifest. Lane writes
remain in the existing orchestrator ledger (INV-1).

A permanently retired native fencing token uses the existing obsolete-completion
rejection, rather than tracker-outage retries (#3437). Operator abandon clears
the matching deferred completion as well as retry, running, and claimed state;
it cannot resurrect an abandoned completion without a restart.
`TestCompletionFenceDeferralOutcomes` and
`TestHandleWorkAttemptRecoveryAbandonsActiveAttemptAndAudits` cover rejection and
cleanup. The native finish and lane handoff share the existing claim lifecycle
instead of adding a retry or recovery mechanism (INV-3).

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

Invalid issue model and effort values reuse the existing override warning and
project-default selection path (#2841); they no longer become terminal issue
configuration errors. Malformed/schema blocks remain terminal. Pre-dispatch model
fallback uses the existing provider report without launching a catalog process.
`TestUnknownOverrideFallsBack`, `TestOverrideFallbackReachesAgent`, and
`TestInvalidOverrideSchemaRemainsTerminal` cover this removal. Cleanup precedence
continues to cover selection failures using an unavailable configured route model.
Issue override parsing shares dependency fence boundaries and ignores nested
fenced examples (#2893). The failure breaker no longer recognizes retired
model/effort rejection strings; YAML and schema rejection remain terminal.
`TestFromIssueBody` covers top-level overrides, fenced examples, and unfinished
outer fences.

Provider-identity bookkeeping failures during implementation, validator, and security-audit turns
are logged without failing the turn (#2626). Persistence uses a bounded detached
context and subsequent updates retry through the existing write path.
`TestProviderIdentityFailureDoesNotCancelTurn` covers cancelled and timed-out store
writes followed by successful persistence and normal turn completion.

Dispatch Workpad comment-read failures use the existing tracker availability observer
and tracker-unavailable dispatch reason; they never become issue dependency evidence.

Worker credential classification uses the connector's shared GraphQL secondary
cooldown, including Retry-After. The existing principal probe retains the generic
credential contract, including GitHub App installation tokens; it does not require
the narrower authenticated-user REST endpoint. A failed `GET /rate_limit`
observation logs an instance-scoped diagnostic and leaves the worker turn running (#3248).
A successful response still enforces the configured worker reserve. The old
credential-wide monitor condition, canary, retry restoration, and scheduler
skip reason are retired. Historical monitor attempts remain visible in attempt
history without restoring a dispatch hold. `TestWorkerGitHubProbeFailureIsInstanceDiagnostic`,
`TestWorkerGitHubPeriodicMonitorFailureThroughSupervisor`, and
`TestTimedOutWorkerProbeDoesNotHoldDispatch` cover the timeout and dispatch
sequence.

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
with the existing Needs-you human-action projection, including PR-less cards and recorded age;
`TestWorkpadHumanActionSnapshot` and `TestOperationsWorkpadHumanAction` cover that path.
Legacy blocker prose does not become a human action or hide dependency decisions.
No timer, recovery path, or reason code is added.
`TestSymbolicBlockerCompletion` and `TestSymbolicBlockerPromotion` cover attribution,
retained diagnostic detail, final usage/diff accounting, CI scheduling after a pushed head,
and promotion after clearance. This replaces symbolic
ref rejection and Rework routing without adding a park, timer, or recovery loop.

Already merged native GitHub PRs (native #199) obtain the real merge commit from
one authenticated GraphQL projection bound to repository, PR number, reviewed
head, source branch and target branch. REST API 2026-03-10 no longer supplies
`merge_commit_sha`. The existing LocalGit owner verifies the projected commit on
the fetched target ancestry before recording genuine landing. Open PRs retain
atomic reviewed-head merges and current GitHub protection. No already merged PR
is mutated, global API version downgraded or completion fabricated.
`TestLocalGitLandChangeViaGitHubAlreadyMerged` covers modern runner-created and
external PR responses, exact identity and missing/non-ancestor commit refusals.

## INV-3 — Mechanism moratorium

Sprite runner wake work (native #212) consolidates overlapping qualifying
mutations into one active wake pass per organization/project. The existing
postmutation owner retains one candidate and at most one sequential provider
request per pass, releases the pass on every exit, and joins canceled work
through Hub shutdown. Mutations never wait for provider availability. Later
mutations resolve fresh authority rather than replaying retained candidates.
This adds no queue, retry/reconciliation loop, lease, configuration or dispatch
owner. `TestWakeSpriteRunnersAfterBurst` records fixture request counts, retained
goroutines and mutation latency; `TestWakeSpriteRunnersCancellation` covers
context cancellation and service shutdown. This is fixture evidence of the
conditional source risk, not a measured production incident.

Hosted fleet approval (native #168) removes volatile heartbeat health from the
existing configuration approval fence and consolidates material classification
around the recorded configuration mismatch. Actual capacity application retains
its existing fresh-evidence requirement and approval retains material-change
equality.
The existing runner read still binds credential revocation/expiry, and the
proposal, browser confirmation and command owners retain exact target/input,
current authority, expected revisions and durable idempotency. This consolidates
INV-1 enforcement without a new guard, approval bypass, retry/recovery loop,
configuration key or fleet surface. `TestHostedMCPFleetControls` exercises
heartbeat transitions and durable refusals through the existing browser owner.

Hub migration collision repair (native #194) consolidates conversation origin
under forward migration 65. Applied attachment migration 62 and runner observation
migration 63 remain immutable. The existing Goose transaction restores missing
effects from either previously deployed origin lineage without overwriting existing
origin values or attachment data. Attachment-reference migration 64 executes its
unchanged SQL through the same Goose owner after repairing missing prerequisites;
databases that already applied 64 retain those effects. Fresh, schema-60,
attachment-62, origin-62, origin-63 and reference-64 fixtures exercise forward
upgrades. This adds no startup reconciliation loop, migration allocator, validation
gate or configuration key. Once migration 65 is applied, deployment rollback
requires a binary that supports schema 65. The subsequently added hosted-project
event index uses forward SQL migration 66, preserving the genuinely applied Go
origin migration 65. The schema-65 upgrade fixture verifies unchanged origin and
reference data plus the new event index. After 66 applies, rollback requires a
binary that supports schema 66.

Reviewed checkout isolation (#170) consolidates preparation under LocalGit's
existing source-operation lock, path confinement, workspace usage and cleanup
registry. Head-scoped landing paths retain the original issue identity for
active-issue protection; retirement inspects and archives the recorded path,
preserving Code. `TestRetentionCompletedWorkspace` covers active and expired
landing ownership. This adds no recovery loop, lease, reason code, configuration
or policy bypass; project-specific validation and forge protections remain intact.

Hub binary deploy continuity (native #187) retains the existing lease TTL, renewal,
expiry and fencing authorities. Shutdown closes the existing conversation broker
before HTTP drain; hosted activity streams use readiness on their existing tick.
Migration foreign-key verification moves into the final version-write transaction,
removing the repeated full-database scan from unchanged-schema startup without
stamping a failed migration. Offline verification retains its full checks.
`TestHubMigrationVerificationScope`, `TestConversationWorkerControlsLongPoll` and
`TestNativeOrderedAttemptLifecycle` cover validation rollback, no-op startup,
poll shutdown and same-attempt renewal/completion across reopen.
`TestHostedSecuritySSERevocation` covers the activity stream's shutdown exit.
Production downtime and active-run acceptance remain with the deployment/release owner under
the [Hub upgrade procedure](hub-self-hosting.md#upgrade-interruption-and-compatibility).
No restart grace period, lease state, outbox or recovery owner is added.

Bounded native admission (#190) consolidates due-retry disposal under the existing
dispatch planner. Planning, refill and tick cleanup no longer treat a newly
leased batch as complete tracker membership. Current closed, terminal, inactive
and authorization observations retain disposal authority; empty capacity batches
and missing or failed status observations do not. The existing native claim's
interrupted `resume_session` checkpoint also selects its exact persisted local
attempt through the existing session lookup, including overload-ended failed
Code sessions without a PR or experimental automatic resume. Native recovery
verifies policy, configured runtime identity, provider availability, host, head
and workspace digest before continuing. Native continuation no longer shares
the generic fresh-session fallback; unavailable or changed continuation remains
with the existing recovery-required outcome. Failed native turns retain their
checkpoint workspace. No retry loop, recovery path or reservation is added.
`TestHubSchedulingPreservesOverloadRetryAcrossAdmissions` covers bounded omission
and authentic invalidation; `TestNativeInterruptedCodeRecoversPersistedSession`
covers already-lost local retry ownership with clean and dirty workspaces.

Managed local project cutover (native #94) consolidates existing configuration,
policy, import/cutover, drain and operator command owners. Project drain closes
only that project's dispatch; it does not pause the shared dispatch gate or
prevent unrelated projects acquiring capacity. Deferred completion retains its
existing publication owner and prevents detach until settled. Local intake and
schedules must already be migrated or disabled through their workflow owner.
No configuration key, scheduling/recovery loop, generic file editor, daemon
auto-start or tracker lane writer is introduced.
`TestBeginDrainStopsPendingDispatchTick` also verifies unrelated capacity remains
available; managed configuration tests verify deferred work prevents detach.

Worker cleanup (#169) removes Darwin polling of unreadable unknown same-user
process environments. A single inspection cannot establish ownership from an
error; unknown candidates are never signaled. Existing authenticated process
groups, readable scratch ownership and independent workspace cwd evidence remain
cleanup authorities. A scratch-stage deadline retains its failure while allowing
the independent cwd stage to find owned children. No cleanup is bypassed for a
live positively owned child, and no recovery mechanism or lane writer is added.
`TestDarwinScratchEnvironmentProcessIDsScanBudget` covers unknown candidates and
owned TERM survivors; `TestWorkspaceProcessIDsScanBudget` preserves independent
cwd evidence and parent cancellation.

Native base races (#160) consolidate merge refusal classification with the
existing exact-endpoint `ErrPullRequestBaseOutOfDate` owner. The connector and
native workspace share endpoint and JSON `message` validation, with native
continuation scoped to `Base branch was modified` (#161). Strict
`Head branch is out of date` retains native protection refusal ownership and the
legacy connector's existing rebase ownership. Unrelated methods,
resources, noncanonical PR identities, malformed bodies and metadata cannot
supply base-race authority. Native landing reuses the existing forge wait,
restart recovery and merge continuation, preserving immutable review and
atomic head/lease/policy fences without a new reason code, retry loop, park,
brake, configuration key or exception path. Required branch checks and reviews
remain binding on every merge attempt; a clean source merge or base refusal
never counts as a landing. `TestConnectorMergePullRequestClassifiesBaseRefusal`,
`TestGitHubLandingAPIEndpointOwnership` and `TestNativeLandingRunCompletion`
cover the shared classification and completion boundaries.

Native runtime evidence (#92) records observations in the existing fenced
attempt event stream and records evaluated Merging eligibility, routing,
host/provider refusal and claim decisions in the existing scheduler
transactions. Ordinary usage and activity checkpoints and lease renewals publish
the bounded observation; no observer or polling loop is added. Reads take one
snapshot through existing application services and never dispatch, write lanes
or call the forge to manufacture history. Historical gaps stay unavailable.
Scoped local explanation and operator discovery APIs use the current-grant
application authority instead of the blanket aggregate-dashboard read refusal.
Current runtime evidence (#159) consolidates existing Change summary and
host/provider capacity reads inside that snapshot; it introduces no cache,
poller, configuration, endpoint, reason code, or read-triggered mutation.
The schema migration extends the existing append-only event vocabulary and
preserves all prior evidence; rollback refuses to discard new event types.

Native completion (#152) owns recorded publication results instead of falling
through to legacy pull-request remote truth. A successful nil native result stays
with existing dirty-source, interactive conversation and drain continuation owners; it does
not replay an immutable tracker-unavailable completion. A version missing without
a diagnostic remains an incomplete handoff. Clean preserved rework heads reuse
the genuine current immutable Change version only under its current approved
policy; a policy refusal cannot fabricate another version of unchanged source.
Changed source retains the existing fenced and idempotent version publisher.
The orchestrator alone selects an allowed review or landing lane, and landing
revalidates the exact reviewed head through its existing owner. Successful-attempt
revision/generation suppression stays intact. Graceful drain stops intake while
already-authorized native code and rework retain that completion owner, including
publication diagnostics and refused lane writes; drain never substitutes legacy PR
remote truth for a genuine native completion. `TestNativeExecutionSettlesFinishedRun`,
`TestNativeRunnerOpensChangeAndLeavesDispatch` and `TestNativeChangeRunCompletion`
cover consolidation without a retry loop, reason code, or policy bypass.

Native Rework context (#143) uses the same scoped current Change reader as
landing. Recovery carries Change detail, including discussion and formal review
findings with immutable version/head, actor and provenance, alongside existing
issue discussion, history and attempts. Historical references cannot select the
current version; historical feedback cannot become current approval or rejection.
Feedback bodies remain untrusted task content, and discussion is not formal
approval. Missing scoped reads return through the existing hydration error owner
before dispatch. `TestNativeRecoveryUsesPublishedVersion` and the Rework cases in
`TestNativeRunnerOpensChangeAndLeavesDispatch` cover the context through the real
Hub, scheduler, execution and provider prompt without a feedback store, recovery
loop, gate, configuration switch or tracker lane writer.

Externally published native versions (#98) resolve the reviewed current version
before the existing workspace owner prepares a landing. The target retains its
External PR reference. The runner verifies its authorized source, clean owned
worktree and immutable head, hydrates missing commits through authenticated Git,
and rechecks current-version authority after preparation. An explicit PR is read
directly and must match repository, branch, head and the authorized base; it is
never replaced or force-published. Ordinary worker PR publication retains its
atomic merge head authority below. Existing policy, scope, authentication,
review/check refusals and actual landing receipts remain authoritative; the Hub
receives no forge credentials and workers never write lanes. Source bundles and
occupied worktrees remain intact. `TestNativeExecutionOperatorLandingTarget`,
`TestLocalGitCreateReviewedLanding`, `TestLocalGitLandChangeViaGitHub`,
`TestRunnerResolvesLandingBeforeWorkspace` and `TestLandNativeChange` cover these
consolidated owners without a new mechanism.

Native GitHub landing (#137) consolidates reviewed-head authority on the
existing atomic merge PUT with `sha` equal to the immutable reviewed head.
After the runner's lease-protected push, the selected open PR's list head may
lag publication and cannot refuse landing. Repository, branch and base identity,
worktree HEAD verification, source locks, current policy/version and ownership
checks remain required. Only PUT on the exact
`repos/{owner}/{repo}/pulls/{positive-number}/merge` endpoint owns atomic head
refusals (#149). Its HTTP 409 response retains `LandRefusalHeadMoved` regardless
of response body language or serialization; unrelated endpoint 409 responses
remain generic errors. Explicit closed-PR HTTP 405 `message` evidence also
retains `LandRefusalHeadMoved`; reviews, checks, protection and
authentication remain enforced by GitHub. Typed quota evidence reaches the
existing capacity owner before repository refusal classification. Only actual
successful receipts reach the Hub, which receives no GitHub credentials.
`TestLocalGitLandChangeViaGitHub`, `TestGitHubLandingAPIEndpointOwnership` and
`TestLandNativeChange` cover stale list heads after rework publication, guarded
remote head rejection, refusal identity and quota precedence. No retry loop,
recovery path, reason code, policy bypass or lane writer is added.

Native conflict completion selects the project's existing configured
`ReworkState` through `CompletionLane` and the sole orchestrator lane writer
(#96). Missing, disallowed, operator-only or terminal rework destinations retain
the existing deferred completion handoff. Other refusals retain the configured
review or Blocked destination. This consolidates landing refusal routing under
the existing completion and rework owners; no configuration, reason code,
recovery loop or worker lane writer is added.

Explicit GitHub merge-conflict refusals (#155) establish source Rework only
when a bounded refresh identifies the exact reviewed published head, PR
repository/branch/base and current fetched and published base, and Git
`merge-tree --write-tree` confirms a conflict for those immutable commits.
Unknown, missing or contradictory source identity or Git refs remain with the
existing bounded merge continuation owner in Merging without source failure
allowance changes (#193). Clean Git evidence never authorizes landing after a refused
atomic merge. The original HTTP refusal and native Change/version/head remain
in attempt history; refresh quota evidence keeps capacity precedence. Existing
protected/review/check refusals and exact-endpoint 409 ownership remain enforced.
The existing landing continuation retains the exact reviewed version/head and
releases worker capacity without a provider coding turn. The existing durable
forge wait recovery reclassifies old synthetic native projection waits only when
the recorded host, class, exact PUT endpoint, HTTP 405 and decoded response
identify the former coercion. Other persisted waits keep their outage authority.
The existing completion/probe owner clears a synthetic projection condition on
a responsive typed continuation or proven conflict; independent genuine waits,
credential authority and runtime capacity limits remain binding. A successful
same-version landing receipt retains the existing recovery owner. Source
cleanliness alone never clears the merge refusal or establishes a landing.
`TestLocalGitLandChangeViaGitHub`, `TestLandNativeChange` and
`TestNativeLandingRunCompletion` cover these consolidated authorities under
INV-1 and INV-3 without a new reason, poller, configuration or recovery loop.

An exact typed atomic GitHub base-advance refusal (#221) belongs to the existing
item-local `LandRefusalBaseMoved` continuation, not the forge outage owner. The
original HTTP status and base-out-of-date cause remain inspectable; the same
immutable reviewed head retries without a coding turn or five-minute provider
wait. Real outages, quota, protection, malformed or unrelated 405 refusals retain
their existing owners. `TestLocalGitLandChangeViaGitHub` and the existing native
landing completion fixture enforce INV-1 and INV-3 without new mechanisms.

Current Git base verification (#164) removes the equality pin to GitHub's
projected PR base SHA. A stale base SHA alone cannot veto conflict evidence:
the freshly fetched base must match the current published base in the same
`ls-remote` read that verifies the exact immutable reviewed branch head.
`merge-tree` uses that fetched commit. PR repository, base ref, head branch and
head identity, worktree/source authority, policy and quota precedence remain
required. Changing or unproven Git refs and clean source trees retain the
existing item-local landing continuation; proven current source conflicts select configured Rework
without reapproval. `TestLocalGitLandChangeViaGitHub` and
`TestNativeLandingRunCompletion` cover advanced live bases with stale PR
projections for conflicting matrix files and clean source trees.

Native code/rework (#141, #156) consolidates commit finalization, rebase
preparation and continuation under
`LocalGit` and its existing source-operation lock. Workspace creation retains a
verified paused transaction on the assigned branch. Workers resolve and stage
authorized issue edits and source/index conflicts; the execution epilogue commits
an unpaused staged repair and finalizes the rebase before
capturing artifacts, diffs or the final checkpoint. Source diagnostics and the
model turn run outside the source lock. Commit finalization verifies the owned
worktree/common Git directory and assigned branch, then revalidates the active
execution lease/policy authority under that lock. Ordinary host commits retain
the project's effective signing configuration; unavailable signing fails as an
instance capability without an unsigned fallback, configuration change or worker
key access. Workers neither commit nor use signing workarounds. Ordinary tracker
workflow remains unchanged. The staged host commit alone overrides
`core.hooksPath` with the platform null device, disabling pre-commit,
prepare-commit-msg, commit-msg and post-commit hooks without changing repository
configuration, author identity or signing. `--no-verify` is insufficient for
this host boundary. Paused rebase replay retains its existing ownership.
Native machine replay explicitly avoids
personal signing, including a signer option saved by an earlier paused rebase;
host configuration and signer material remain untouched. Legacy Git signing and
actual forge signing/protection requirements retain their existing owners.
Paused transaction ownership, original branch head, additional ref updates and
source-only replay instructions are checked before privileged continuation.
Sandbox metadata roots grant only the assigned branch's ref, reflog and lock
files, plus the existing worktree metadata and objects; they never grant branch
parent directories, sibling ref writes or the common Git directory.

Late host replay conflicts (#163) share the existing successful dirty-source
continuation under INV-1, INV-2 and INV-3. A successful model turn may leave
staged preserved work that delays rebase preparation until the host commits it.
A conflict introduced by that preparation, or by continuing a resolved replay
into the next commit, preserves a successful native attempt and its dirty
checkpoint. The claim owner's existing already-answered predicate no longer
suppresses successful attempts with dirty checkpoints, so ordinary continuation
can claim the source-resolution session without a lane move, new dispatch request,
reason code or retry owner. Conflicts still unresolved when host finalization
begins remain genuine source failures and consume the existing failure allowance.
Native work without a forge PR uses existing workspace-diff progress accounting;
unpublished replay commits cannot establish stranded forge work. Repeated unchanged
diffs retain their existing no-progress accounting and configured budget controls.
Immutable artifact capture requires a clean or unpushed recovery checkpoint;
paused rebase indexes are excluded from attempt diff capture. Dirty native success
publishes no Change version, does not inherit review and establishes no landing.
`TestNativeReworkFinalizesBeforeImmutableEvidence` reproduces staged work,
late conflict and successive replay conflicts, including preserved resolved
progress. `TestNativeChangeRunCompletion` verifies durable progress metadata,
ordinary continuation and genuine failure-allowance consequences.
`TestClaimCandidatesKeepOfferingUnsuccessfulAttempts` retains clean-success
suppression and checks dirty-success claim eligibility.
`TestNativeRunnerOpensChangeAndLeavesDispatch` verifies the authentic successful
dirty receipt, unchanged prior version and feedback, subsequent resolution,
exact-head/version/policy publication and actual local-transport landing.

`TestLocalGitNativeReworkOwnsPausedRebase`,
`TestNativeReworkFinalizesBeforeImmutableEvidence` and
`TestGitMetadataWritableRootsForLinkedWorktree` cover these boundaries. The source
fixture attempts a nested macOS sandbox when the host permits it; a sandboxed
worker that forbids nesting still exercises source/index resolution, explicit
root authority, signer inheritance and transaction finalization. Nested sandbox
denial is not OS sandbox enforcement evidence.
Signed commit fixtures use real SSH signatures and verification; GPG-unavailable
fixtures exercise actual Git commit failure and retained staged work. Runtime
acceptance of a GPG-configured worker's OS denial of signer material and unrelated
common refs remains with the integration owner when nested sandboxing is unavailable.

`TestNativeRunnerOpensChangeAndLeavesDispatch` exercises rework preparation
against a local Git transport while preserving the HTTPS repository identity.
Its rework cases verify that the new immutable version names the final stored
diff head, unchanged policy and matching code artifact, without external Git
access or a duplicate publication fixture.

The existing rework worker regenerates derived artifacts through their build
owner when needed. It publishes a new
immutable Change Request version with its actual head and artifact identity;
the prior version's review cannot authorize that new version. Current-version
review, project checks and exact-head landing authority remain required. Only
an actual landing receipt establishes shipment; worker terminal success and
landing refusal metadata do not. `TestNativeLandingRunCompletion` and the
existing `TestNativeExecutionLandsReviewedVersion` cover those boundaries.
The integration/release owner must still verify a controlled conflict through
rework, regenerated output, new-version publication and exact-version landing
after this source change is integrated; local fixture results are not that
runtime receipt.

Cloud provider report aggregation consolidates validity under the existing runner
credential authority contract described in INV-2. Historical observations remain
stored; the existing lease accounting owner retains live pinned reservations.
No report cleanup, recovery loop, configuration or reason code is added.

Workflow duration reports exclude `agent_activity` in their existing SQL owner
before loading or converting the activity payloads that report aggregation does
not use. A finite report window searches both inclusive-start/exclusive-end
`finished_at` bounds through the existing index; absent bounds retain the
optional query contract. Flow overlap reads likewise seek a present lower
`finished_at` bound directly and retain the strict `started_at` upper bound.
They do not constrain the upper `finished_at`: long sessions ending after the
window still contribute their overlap. Absent lower bounds retain the optional
query. Timeline/activity readers, history revision, trends and representatives
are unchanged. Existing report fixtures check large activity exclusion, report
equivalence, optional bounds, strict overlap boundaries and the actual lower
index-seek operand. No index, cache, configuration or schema is added.

The existing workflow-history UPDATE trigger invalidates cached projections on
every non-activity update and on activity changes to event/project/issue
identity, identifier, URL, PR number or completion time. Activity checkpoints
that change only fields absent from those projections do not invalidate them.
INSERT/DELETE invalidation and the revision counter remain unchanged. Runtime
evidence retains table counts and completion bounds; detailed activity timelines
read fresh checkpoints directly. The forward migration replaces only this
trigger, and its rollback restores unconditional UPDATE invalidation.
`TestWorkflowHistoryRevision` checks current timeline payloads, unchanged
checkpoint evidence, NULL transitions, projected identity changes, other phase
updates and both migration directions.

Cycle-time reports and verified shipped outcomes share the existing global
workflow-history cache entry rather than scanning completed sessions, attempt
metadata or the lane ledger on each changed snapshot. Project views inherit
those global projections; project history entries do not repeat their queries.
Existing revision checks, coalesced loading, scope bounds and 30-second
wall/window freshness apply. Direct session, attempt or applied lane-ledger
changes without a phase revision remain bounded by that freshness limit.
Latest-phase delivery identity, URL and PR changes retain revision invalidation.
This read-only dashboard projection never supplies merge or lane authority.
A failed component query leaves successful components available but does not
cache the failed global composite; a failed shipped read preserves the input
snapshot instead of fabricating deliveries. Existing cadence/concurrency/failure
and changed-snapshot SSE fixtures preserve statistics, delivery identity and
timestamps, and verify heartbeat reuse, scopes, revisions, independent ledger
write bounds and failed-read retries. No cache, TTL, query, index or configuration
is added.

GitHub usage timing (#3767) extends the existing `RESTScope` owner, mutex,
fixed stage/step/family/outcome aggregation and single aggregate log event.
GraphQL timing adds only allowlisted existing query purposes, with unknown
purposes grouped as `graphql`; no URL, query, variable, body, header, credential,
or per-request row is retained. REST request/deferred/refused counts and GraphQL
quota query/cost accounting keep their meanings. HTTP attempts are measured
independently of quota points, including transport, body and close failures;
refusal before `Do` produces no HTTP observation. Attribution is captured at
request entry, before token resolution, and survives later scope stage changes.
Counts and timing aggregates drain together under the existing owner, including
timing-only outside-refresh cohorts. Completed observations carry attempt count,
timed count, elapsed sum and maximum in nanoseconds; absent observations remain
unknown and do not establish complete coverage of uninstrumented work.

`http_transport` sums the actual `Do`, body-read/counting and existing deferred
drain/close segments. Response logging, progress callbacks, header accounting,
decoding and recursive auth retries between those segments are excluded. The
existing request and cleanup order is unchanged.
It includes any connection-pool wait, DNS/TLS and server time inside `Do`, without
separately measuring those components or claiming pure network time.
`token_resolution_inclusive` measures the existing request-entry `Token` call,
including locking, fallback and nested installation-token HTTP. Cached token
retrieval is timed as token resolution and creates no HTTP attempt. Concurrent
HTTP sums can exceed stage wall elapsed, and token elapsed can contain HTTP
elapsed. These overlapping/inclusive sums are work measurements, never wall,
critical-path, busy-wall, savings, percentage attribution, or values to add as
disjoint time. Existing refresh timing remains the wall owner; actor queue,
local and uninstrumented work remain unknown without subtraction. No profiler,
queue owner, timer, recovery mechanism, storage table or configuration is added.
Existing scope, request/error/auth, quota, conditional, privacy, installation-token
and pooled-client fixtures cover attribution, overlap, coverage and no extra calls.
See [GitHub usage timing and privacy](diagnosis.md#github-usage-timing-and-privacy)
for coverage and the integration/release owner's pending runtime acceptance.

Permission completion consolidation (#3758) removes the competing global
final-message permission invocation. Only the existing native completion owner
reuses the final-outcome handler for native final-only human attention, including
non-completed producer outcomes with no Change Request. Existing human-owned
settlement and tracker-write deferral preserve attempt identity, usage and claim
settlement; no parser, reason code, recovery mechanism or configuration is added.
The existing PR-delivery failure predicate validates every joined error leaf for
both native human attention and delivery reconciliation. Mixed workspace,
checkpoint, lease or session failures retain their original error authority;
a nested delivery error cannot erase an unrelated failure or create a permission
park. The existing deferred-delivery receipt stores one summarized typed cause
only after that whole-error proof; replay validates the same predicate. Existing
Cause/Error diagnostics remain. Legacy text-only receipts cannot prove purity,
so replay preserves their full original error instead of creating human attention
or successful reconciliation. The same receipt retains the worker's typed forge
scope/class separately from tracker-fence availability, reusing existing forge
wait metadata. Replay restores that wrapper after its inner error and retains
summarized approval-denial evidence, so existing credential/forge owners keep
instance failures out of human holds and issue failure accounting. The same optional
metadata is present and empty when the current encoder observed no forge wrapper.
Absent or malformed metadata retains the full original error, including v35
typed-delivery receipts that cannot prove outer availability. Missing authority
is never reconstructed from text; no historical receipt is rewritten.
No top-level schema or recovery owner is added.
Historical Workpads, permission parks, allowance records and lanes are untouched.
`TestHandleRunResultPermissionWait`, `TestNativeChangeRunCompletion` and
`TestNativeLandingRunCompletion` cover canonical authority, nil-change human
attention and typed landing success/refusal.

Admission tool acceptance (#3756) belongs to the existing candidate validator,
which the collector runs before acknowledging a submission. Strict JSON and
semantic rejections receive bounded diagnostics without echoing candidate IDs,
criteria, quotes, rationale or raw input. A valid correction in the same runner
conversation retires the pending rejection; rejected calls do not count as
accepted evaluations. The producer prompt requires one accepted terminal result
and permits correcting rejected input within that same conversation, without
another runner invocation. Two valid submissions still violate the exact-one
contract, and an uncorrected later rejection remains malformed. All-invalid tool
conversations cannot fall through to final text. The tool-unavailable typed
final-text envelope uses the same validator, preserving candidate count and
fail-closed behavior. Delayed tool validation is removed; final source, dependency,
authorization, confidence, required-dimension and effort qualification retain
their existing owners. Mixed findings do not establish automatic eligibility.
Existing collector, per-candidate, mixed-result and final-text fixtures cover
these boundaries. No reason code, provider retry, turn request, reservation,
configuration, archive or recovery mechanism is added; existing malformed
evidence persistence and redaction remain unchanged.

Project state reads (#3748) use the existing published snapshot owner once the
first publication is ready, including while the actor is blocked on synchronous
refill. The competing actor state request and snapshot notification channels are
removed, along with the read-routing refresh flag. The shared refill owner
publishes confirmed operation state before its fresh candidate read; unresolved tracker writes remain unpublished. Completion
snapshots retain priority over runtime and full publications during completion
refill, until final publication clears the existing fence. Returned state remains
independent, with live runtime ownership, persisted worker heartbeat and validator
progress observable outside the fence. Full publication stores runtime ownership
before the full snapshot and initial readiness, so a newly observed full snapshot
cannot be overlaid with older runtime ownership. Reads do not advance tracker
freshness, complete an unfinished refresh or establish dispatch eligibility.
Drain and ForceQuit handlers publish their completed state before acknowledging
the caller, so an immediate State read observes draining and force-quit ownership
cleanup. The existing shutdown regressions assert this ordering (#3801).
Existing publication, operator-move, queued-completion, defensive-copy, worker-progress,
startup and promotion-read fixtures cover these boundaries. No new cache,
timeout, configuration, guard, lane writer or recovery mechanism is added.

Completion cleanliness (#3744) uses current, available workspace recovery
evidence before classifying blocked Workpad text as an intentional remainder.
After prior cleanliness rejections, verified zero-diff, zero-unpublished evidence
retains the existing clean-retry owner, with or without a current completion
declaration or committed/discarded metadata. Absent or unavailable recovery
evidence and history failures retain their existing refusal semantics. Actual
dirty or unpublished source remains refused; blocked intentional remainders
retain their existing escalation owner. Independent human actions, project
verification and PR promotion retain their existing owners: cleanliness alone
does not establish approval or successful acceptance. Historical human-owned
parks retain their authorized recovery lifecycle. The existing evaluator and
run-completion cleanliness fixtures cover these boundaries. This consolidates
classification order without a prose classifier, guard, reason, retry,
configuration or recovery mechanism.

Machine-issue authoring (#3742) exposes optional metadata labels through the
existing intake draft and connector label normalization. The tool omits lane
labels projected from the configured tracker prefix, lane states and state map,
including Backlog, while preserving unrelated metadata. This consolidates the
authoring contract with existing configured status ownership; it adds no policy
configuration, reader, recovery mechanism or lane writer. Source stamping and
fingerprint coordination retain their existing owners. Open fingerprint reuse
remains comment-only, without body, label or lane changes; only newly published
issues receive the orchestrator's Backlog state write. The existing
`TestMachineIssueTool` fixture covers metadata, omitted labels, configured/mapped
lane filtering, malformed requests, provenance, reuse and publication failures.

Admission candidate evaluations use the runner's existing current-clock fallback
for each logical session start (#3729). The admission batch no longer overrides
that clock. Run start, dependency observations, evaluation history and malformed
result history retain their batch observation time; budget, fingerprints, capacity
and fresh final eligibility validation retain their existing owners. Session start
remains distinct from physical provider launch. The existing mixed semantic-result
and runner admission fixtures cover these boundaries without adding a clock field
or mechanism.

Dispatch reference evidence (#3696) consolidates dependency hydration and recorded
predicate resolution under the existing per-plan blocker cache. Only successfully
resolved issue snapshots seed predicates, keyed by the normalized actual issue
identifier. Missing or failed references remain unknown and the existing resolver
still reads missing predicate references (`referencesAttempted=false`). Fresh
Workpad comment reads, PR/check hydration, native relation authority and human
completion requirements retain their existing owners. The dispatch callback
always discards reusable evidence on return, including a lane write followed by
a failed launch; retry polling also discards it because an exhausted merge worker
can write Blocked. Evidence never survives a dispatch plan or a mutation callback.
Ordering, capacity acquisition and launch outcomes are unchanged. Existing
`TestDispatchRecordedPullRequestBlocker` and
`TestDispatchWorkpadDependencyEvidence` and
`TestDispatchReadyIssuesRefreshesStaleBlocker` fixtures assert overlapping reads,
normalization, unknown/error and human negatives, fresh subsequent plans and
fresh reads after failed dispatch and retry-poll lane writes. No persistent cache,
reader, configuration, recovery path or tracker lane writer is introduced.

Retired-park recovery (#3687) consolidates its dependency and typed-predicate
reference reads into one fresh phase cohort through the existing identifier
resolver. Identifiers are deduplicated in first-request order, and GitHub's
existing resolver groups PR discovery once per repository. A successful cohort
replaces duplicate per-root resolver/discovery reads and repeat predicate
lookups for absent evidence. Each root retains its dependency provenance,
human/operator holds, predicate evidence and completion decisions. Missing
results in a successful cohort remain unknown for their roots. A failed cohort
is discarded, including any partial results, and the existing per-root recovery
owner reads fresh authority independently. An unavailable reference therefore
cannot hold an unrelated root whose predicate has cleared. Failed batches may
add one bounded resolver attempt; the existing per-root owner retains error
isolation without a new reader or recovery path. The existing dependency
mapping owner clears identifier-addressable positive state and human-readiness
fields before adopting fresh resolved authority; absent or failed reads cannot
retain stale completion. ID-only refs and connectors without a reference
resolver retain their existing inline authority. The existing request cap,
reserve and first-read rules still apply. A successful lane write discards the
cohort for remaining roots, which return to ordinary fresh reads; evidence
never survives a phase or operation. Root body/comments, exact-head
PR/status/reviews and operational receipt verification retain their existing
owners. Dispatch's capacity/lookahead bound and rotating dependency-priority
scan remain unchanged. `TestRetiredParkReferenceCohort` exercises repeated
references/repositories, 500/403 failure isolation, absence, cancellation,
finite cap/reserve, human holds, fresh subsequent phases and post-write reads;
the existing capped dependency-progress fixture records the requests made
available by this consolidation. No persistent cache, remote reader, recovery
path, configuration or lane writer is introduced.

Refresh dispatch (#3674) runs the existing retired-park and dependency-maintenance
owners after unrelated active work has passed current authorization, dependency,
human/operator, review and visual gates. One rotating dependency priority read remains before
ordinary reads on alternating refreshes. Backend-capacity recovery and blocker
promotion remain before dispatch because they can supply admission authority.
Current candidate Blocked status is applied before admission; full blocked-status tracking and missing-retry cleanup
remain after recovery, preserving started-work and park evidence. Authorization
refusals retain every durable per-issue row in one transaction per observation.
Schema-3 explanation reads preserve available evidence when a configured source
hits its deadline and mark that source unavailable; request cancellation still
propagates. This removes per-card commits and inapplicable pre-dispatch maintenance
without a cache, timer, configuration key, reason code or recovery owner.

Completion comment hydration (#3661) consumes the current producer's complete
comment result instead of immediately reading it again. `CommentsComplete` is
an operation-result contract excluded from JSON/YAML, not retained freshness
or timestamp authority. GitHub marks only a successful full comment read;
local results mark their current database read; composite results require both.
The completion owner replaces body, comment count, comments and completeness
from the matching fresh candidate, including empty values. Unknown, partial
and failed reads retain the explicit fresh comment owner. Native dependencies,
current head, Workpad edits and read-error refusal remain unchanged. Existing
fresh-Workpad and local/composite comment fixtures cover edited same-comment
negative evidence, cleared bodies, empty comments, marker reset and omission
from persisted data. This removes a duplicate reader without a cache or new
configuration, gate or recovery mechanism.

Admission reconciliation (#3719) applies the same operation-result contract to
decision comments returned by its current issue read. Complete results, including empty
comments, retain that current evidence; incomplete results keep the explicit
comment read and its error refusal. Comment identity and authorization, proposal
fingerprints, dependency checks and lane writes retain their existing owners.
This removes a duplicate comment reader without retaining evidence across runs.

Final workspace diagnostics (#3658) use the existing workspace-preparation
failure class even after a coding turn has finished. The existing runner error
classifier also recognizes the strictly anchored legacy final-diffstat Git
intent-to-add diagnostic receipt. Infrastructure terminal attribution reuses
that classifier only for persisted generic runner errors; genuine worker Git
failures remain chargeable. Existing allowance recovery returns corrected
historical holds to their prior lane while retaining human and dependency holds.
This consolidates failure ownership without a reset, reason code or recovery path.
`TestRunnerWorkAttemptErrorClass`, `TestAttemptAllowanceTriageInfrastructureFailure`,
`TestRetiredAttemptTriageParkRestoresPriorLane` and the runner final-diff fixture
cover typed diagnostics, legacy receipts, genuine failures and prior-lane recovery.

Workspace intent-to-add (#3653) uses ordinary Git ignore handling in the existing
temporary index. Runtime exclusion pathspecs remain on subsequent diagnostic
reads, but no longer cause Git add to reject an ignored scratch directory or
symlink. Shared exclusions and real indexes remain untouched. The existing
`TestWorkspaceDiagnosticsPreserveSharedGitMetadata` fixture covers both ignored
scratch forms and their absence from diagnostic output. This removes misapplied
exclusions without adding a retry, cleanup or recovery mechanism.

GitHub budget recovery resolves current credentials and reserves through the
existing policy constructor. Exact identical worker/orchestrator tokens already
establish shared-pool ownership without a duplicate principal lookup; recovery
only reads current budget evidence. Different-token principal comparisons and
fresh launch identity remain unchanged. The shared reserve check has one owner.
`TestRunnerWorkerGitHubBudgetRecoveryOwnership` covers ten recovery/launch pairs,
parallel current-budget reads, reserve reload, revoked credentials, and mutable
GitHub login names. It removes ten of twenty principal reads per batch while
retaining all twenty budget observations, without a retained identity cache,
configuration, or recovery mechanism. Post-deployment timings are separate
acceptance evidence, not implied by request counts.

Fallback dependency hydration uses the existing fresh comment reader when comment
or Workpad evidence is missing. The former issue-timestamp comment cache is
removed because its diagnostic comments also became Workpad decision authority.
`TestDependencyCommentEvidenceFreshWorkpad` covers authorized edits of the same
comment and cleared human action in Todo, including unchanged, missing, and zero
issue timestamps. Native dependency relations remain authoritative and fresh;
comment references remain diagnostic notes. No replacement cache or recovery
mechanism is added.

Epic completion (#3639) defers a parent when current tick evidence already
contains a nonterminal child, before reading that parent's linked children and
unresolved checklist references. This removes the duplicate completion scan for
an epic that cannot close. Tick evidence is negative authority only: terminal
tick evidence never replaces the existing fresh linked-child and identifier
reads used to close a parent. Unknown children and lookup failures retain the
existing completion and retry owners. `TestCloseCompletedEpics` covers a large
unresolved checklist, a later child reopen, and independent parent completion;
the existing affected-epic retry tests retain transient failure behavior. No
cache, timer, configuration, or recovery mechanism is added.

Rework gate restoration owns its fresh tracker and Workpad read once, retaining
the refreshed issue for later tick consumers. Required-gate snapshots project
that evidence without another tracker read. The existing completion reader still
fetches current evidence for callers outside restoration; history selection
consumes that evidence rather than reading it again. Fresh blocked Workpads and
unavailable reads cannot restore a wait, and current-head validator hydration
keeps its existing owner. `TestReworkGateWaitHistoryCannotResurrectSupersededWait`
checks one tracker/comment read, failed reads, and fresh blocker propagation;
the existing current-head restoration cases cover validator handoff. This
consolidates readers without a cache, configuration, or recovery mechanism.

Runner startup diagnostic reports (#3619) reuse the scheduler's heartbeat and
existing optional feature negotiation. The `runner_local_checks` capability
permits `local_checks`; older Hubs receive the original heartbeat without that
field. Startup observations remain scoped to their project; missing or unsupported
diagnostic authority remains absent, and current-Hub validation and admission
requirements remain unchanged. Registration uses the same negotiated heartbeat
encoder. `TestNativeOptionalReportsNegotiateHubSupport`,
`TestRunnerSetupHeartbeatOwnership`, and `TestOnboardingRunnerLocalChecks` cover
strict older/current schemas, failed/missing evidence, routing identity, and the
single startup heartbeat owner. No compatibility retry loop or gate is added.

Runner checkout readiness checks the source checkout's `.git` and shares the
project owner's configured workflow loader with repository reporting. Readiness
is independent of Git origin and forge; optional GitHub repository association
requires readiness and a canonical GitHub origin from that source checkout.
An explicitly configured workflow may live outside that checkout; registration
defaults to checkout-local `WORKFLOW.md` only without a configured workflow.
`TestRunnerCheckoutRepositoryReportsOnlyCanonicalOrigin` and
`TestCollectRunnerLocalChecks` cover external and ref-backed workflows, the
registration default, invalid/missing workflows, missing Git checkouts, ready
non-GitHub and origin-less checkouts with no association, and private-output
redaction. This consolidates the existing reporting checks without another
heartbeat owner or mechanism (native Cloud work item #2).

Label-based refresh reads current body, comments, Workpad, and native dependency
evidence through its existing combined hydration owner. The competing persistent
label-evidence cache is removed: issue timestamps cannot authorize reuse of a
previous human request or clearance. `TestLabelRefreshSharesFreshSchedulerEvidence`
covers repeated edits of the same authorized Workpad comment from blocked through
in-progress to complete, with unchanged issue/listing timestamps and cleared
dependencies. Existing project-item/ref caches and fresh PR readers remain intact.

Rework-breaker recovery reads its historical park only for a current Blocked
issue with automatic promotion enabled. Current-cause recovery remains the
first owner for every Blocked issue, including when promotion is disabled.
Configured recovery source lanes retain their existing maintenance behavior
without loading breaker history they cannot consume.
`TestRecoverBlockedIssuesReworkBreakerGuards` covers omitted history reads and
retained current park decisions. This consolidates the existing eligibility
condition before its read without a cache or recovery mechanism.

Scoped park summaries select physical rows through the existing identity
indexes before aggregating history. A fixed two or three `json_each` identity
branches serve any batch size; `rowid IN` deduplicates rows matching several
aliases without collapsing distinct usage rows. Project boundaries, one-hop
alias matching, list and invalid-identity behavior, park provenance, and
acknowledgment receipts remain unchanged. The existing park summary, bridging,
and acknowledgment tests cover these contracts, including a board-sized batch.
This consolidates scoped readers without a new index, cache, or mechanism.

Missing `gate.local_status` evidence is unproduced Detent-owned work (#3172).
The connector shares the ownership predicate with current-head CI telemetry and
merge missing-check accounting, removing that context from pending-CI suppression,
missing-check streaks, parks, and park-recovery waits. Todo duplicate suppression
and stale-PR routing also exclude heads with this worker-owned work, using the
existing worker path and CI wait reason. Required-check evidence remains present
until validation posts the status, so native queues and programmatic merges
cannot bypass it. Other missing contexts and projects without a local status
retain their behavior. `TestRequiredStatusCheckFailures`,
`TestHydrateMergingRulesetStatus`, `TestReworkCurrentHeadCIDispatch`,
`TestPostValidatedGateStatusOnlyForTheValidatedHead`, and
`TestPersistentlyMissingRequiredCheckParkRecovery` cover the boundary.
The existing missing-check mechanism remains for external required contexts that
need a human configuration repair. No mechanism, reason, or configuration key
is added.

Provider transport overload remains instance-owned after a worker has started.
Its completion no longer invokes the competing issue demotion/parking path;
the existing provider retry keeps the current lane. Terminal failure counting
and classification share the existing infrastructure attribution used by the
terminal retry owner. Infrastructure interruptions neither count as issue
failures nor reset earlier genuine failures. The existing terminal-limit
recovery owner evaluates that same cause before applying its cooldown, so a
historical instance-only park returns to its durable prior lane without manual
allowance or tracker mutations (#3575). Existing human, operator, dependency,
and genuine-failure holds remain authoritative. The SQLite restart matrix in
`TestConfiguredTerminalRetryAfterStoreRestart` covers both corrected legacy
parks and genuine failure cooldowns; completion cases cover configured zero
limits, unavailable stores, and active provider retries. Existing restart and
provider resumption eligibility remains independent of issue-failure accounting;
the shared infrastructure attribution applies at the existing counting and
issue-demotion callers, preserving resumability without charging or parking an
issue for an instance interruption.
Service-restart reconciliation uses the same existing source-lane restoration
owner as pre-turn failures, carrying pushed-product evidence so completed work
stays active. A recorded Rework source returns to Rework, legacy empty source
returns to Todo, and neither path enters the issue failure limit, including a
configured zero limit. `TestReconcileTerminalAttemptRetryStatesDemotesRecoveredEmptyAttempt`
checks these existing restoration contracts.
Legacy GitHub REST capacity attempts without durable wait metadata use that
same source-lane restoration owner rather than the issue-failure demoter.
Their existing pushed-product, unresolved-PR, and foreign-claim ownership
checks remain authoritative. Durable capacity waits stay in their current lane.
`TestReconcileTerminalAttemptRetryStatesHandlesGitHubRESTCapacityCompatibility`
covers legacy Todo and Rework restoration, configured zero limits, pushed work,
foreign claims, and durable wait preservation.

Merged-completion ownership (#3529) resolves stale closed associations through
existing native reference lookup and the existing merged-PR owner. Completion
classification uses that same decision instead of a separate post-merge CI wait;
no recovery loop, exception marker, reason, configuration, or lane writer is added.

The hourly source-invariant failure (#3532) refreshes the reviewed digests for
`handleSessionBrake`, `reworkMergeWorkerResult`, and
`updateIssueStateByIDWithMetadataMode`. The reviewed changes remove automatic
repository lesson evidence/writes (#3501) and attribute verified delivery time
to existing merge evidence (#3485). Their lane-reason sources and forwarding
remain unchanged; no mechanism or reason is added. `TestRepositorySources`
continues to reject edits to these functions until reviewed.

Human Workpad recovery (#3504) derives the existing human-action park from its
durable `workpad_blocker` lane entry when older entries have no recovery metadata.
The current Blocked entry already supplies the reason, prior active lane, and
entry time; requiring duplicate metadata made an authorized clearance unreachable.
The same clearance evaluator and recorded-blocker recovery restore the prior
In Progress or Rework lane. Unauthorized, stale, uncleared actions and unresolved
dependencies keep their existing holds. No new reason, loop, or configuration is
introduced. `TestWorkpadHumanActionClearanceRecoversBlockedIssue` includes real
SQLite legacy entries and both active lanes.
The recorded recovery receipt carries the same human-action park it released.
The existing park retainer recognizes that receipt across refresh and restart;
it does not acknowledge unrelated or later human parks. Scheduler skips retain
the stored cause and recovery reason instead of reporting an opaque Blocked
hold. The Workpad clearance regression also checks retained-park replay.
Older native `recorded_blocker_recovery` receipts may omit the duplicated park
metadata. The same park owner recognizes their authorized Workpad clearance
after a known fingerprinted human-action park. A later completed Workpad can
preserve an earlier explicit `in_progress` clearance, but cannot authorize one
by itself. Missing provenance, mismatched supplied park metadata, newer human
requests, unauthorized or invalid Workpads, and current dependencies retain
their holds. `TestLegacyRecordedHumanClearanceRetainsNativeAuthority` replays
these receipts through SQLite before and after restarting the in-memory state.
The existing workflow metadata updater persists the recognized park on that
same native recovery receipt before releasing the retained hold. This preserves
the accepted clearance when a worker edits the same Workpad comment to complete.
Receipt identity, provenance, and unknown metadata remain unchanged; no history
event is appended. Unsupported or failed persistence retains the hold, and write
failure is logged against the instance. `TestLegacyHumanClearanceSurvivesWorkpadOverwrite`
covers the same-comment edit and a new orchestrator using the same SQLite store.

Session token ceilings record their existing typed outcome, usage, and agent
session phase in the database without writing a repository lesson. Removing the
automatic lesson append keeps runtime failure evidence out of committable files
and prevents a filesystem error from changing the token ceiling failure.
Rework transitions also use existing lane database events instead of automatic
lesson captures in the primary project checkout. Project loading does not enable
or resolve an orchestration lesson file. Configured agent lesson recall still
reads project-authored knowledge when enabled.
`TestRunnerRunKillsSessionAtTokenCeilingWithoutLessonWrites` covers absent and
existing default/configured lesson paths with recall enabled or disabled.
`TestReworkTransitionsKeepDatabaseOwnership` covers separate projects and issues
recording their Rework exit/entry pairs and PR/provenance attribution.

The pull-request hydration recovery ramp is removed (#3497). Request-family
budget enforcement and each PR's existing hydration predicate own admission;
a second project-wide ramp no longer limits healthy candidates after reads
recover. No new recovery path, configuration, or reason is introduced.

Label refresh (#3017) consolidates candidate and observed lane enumeration into
one fresh pass. Routing lanes reuse the existing complete, paginated scheduler
evidence batches for comments and native dependencies; incomplete observations
retain the existing REST fallback. Non-routing observed lanes remain metadata
only, matching ProjectV2 refresh. Overlapping PR reads use observed freshness
once, preserving lane entry, actor, association, head and required-check policy.
The existing author, assignee and label selectors run before enrichment and
filter both returned routing sets; all lane metadata remains diagnostic input.
Native off-selector human prerequisites retain their current authority.
No cache, loop, reason or configuration is added.
`TestLabelRefreshSharesFreshSchedulerEvidence` covers read counts, unchanged
issue timestamps with changed comments, lane actors and incomplete evidence.
`TestLabelRefreshSelectorsExcludeUnownedEvidence` covers all selector predicates
in complete and fallback readers, including off-selector human dependencies.

Full ID and prerequisite-identifier state reads (#3761) reuse that same complete
scheduler evidence owner within each read operation, after fresh REST metadata.
Incomplete, invalid or unavailable aliases retain the existing REST hydration
and its read errors; unread native relations are never authoritative empties.
ID batches remain bounded to 25 aliases, and identifier reads retain their
per-issue ordering. No evidence survives the read operation or crosses the
planner's hydration/evaluation/mutation boundary. Accepted complete comments set
`CommentsComplete`; the final recorded-blocker canonical-comment reread and
independent claim/merge validation still own freshness before mutation.
`TestLabelRefreshSharesFreshSchedulerEvidence` also covers full-state request
counts, preserved REST metadata/order, edited canonical human holds, pagination,
conservative fallback and lightweight probes.

Workspace Git-read failures no longer apply a second admission brake to
unrelated merge workers (#3487). The affected operation retains its existing
forge retry and backoff; ordinary preparation owns its actual remote reads,
and normal merge policy owns eligibility. No probe, recovery loop, reason,
or configuration is added. `TestCheckedMergeUnderForgeCondition` covers dirty,
pending and failing heads without granting merge-control readiness, and
retains write, credential and affected-retry holds.

Review routing for projects with `review.human: false` uses the existing Blocked
lane and existing decision reasons (#3211). The reviewed fingerprints cover the
completed-run transition reason, merge-revocation destination, and the central
lane writer. These changes consolidate Human Review routing under the project
setting; they add no reason code or recovery mechanism.

Completed work remains active when both automatic promotion and human review
are disabled; that combination must not create a false Blocked handoff.
A persisted successful current-head completion invokes the existing completed
review/promotion owner before releasing its gate-wait claim. Gate eligibility
alone does not postpone a ready result until a later project refresh. Actual
CI, native checks, automated review, validator, human and unknown-evidence holds
retain that owner's decisions and the existing no-continuation claim release.
The persistence-failure, dirty, draining, spend and dependency ownership order
is unchanged. This consolidates INV-3 completion handoff on the existing owner,
without another promotion path, polling loop, reason, configuration or bypass.
The existing promotion owner re-evaluates its own completed-review Blocked
receipts against a successful implementation receipt for the same fresh PR and
head. Later lane entries, operator stops, changed heads, active workers, and
current Workpad requests retain their existing authority. This consolidates
completion handoff and promotion without an additional recovery loop, reason,
configuration, or lane writer. `TestCompletedReviewTransitionUsesExistingPromotionOwner`
and `TestCompletedActiveReviewTargetState` cover these boundaries.

Scheduled validation repairs (#3447, #3448, #3451) refresh the completed-run
transition fingerprint after reviewing #3429's operational-receipt freshness
check and #3281's opted-out review routing. The receipt check rejects changed
completion evidence before review routing; removing only that check recreates
the prior approved fingerprint. Its dynamic reason still selects an existing
auto-promotion decision reason or `completed_active_review_transition`; the
scanner's review boundary is retained. The orchestrator remains the sole lane
writer; no transition reason or runtime behavior changes in these repairs.
`TestRepositorySources` and `TestOperationalBodyCompletionSurvivesRestart`
cover the source fingerprint and refusal of changed receipts.

Runner credential expiry (#3382) no longer rejects the existing renewal
operation for the same enrolled runner and organization. The host keeps its
credential and machine binding across stops longer than 24 hours. Ordinary
API calls and rotation still enforce expiry; renewal keeps timestamp validity,
token-hash and revocation checks, including the existing transactional authority
recheck. Revocation remains final. This removes renewal's expiry rule and its
duplicate authority check without adding a recovery path or configuration.
`TestRunnerCredentialExpiryBoundaries` and `TestRunnerRenewRotateRevokeRestart`
cover expiry boundaries, extended stops and the recorded production sequence.

Runner isolation (#3168) replaces the undeliverable default `container` declaration
with `sandbox` and extends existing claim compatibility to backend-probed tiers.
Missing or withdrawn tier reports use the existing no-compatible-work result;
no new reason code, recovery path, lane writer, or configuration key is introduced.
Backend policy mapping fails closed instead of retrying as a native process.
Codex sandbox threads retain the selected `default_permissions` alongside the
named profile in session configuration, so workspace-requirements reloads keep
the same filesystem and limited command-network policy (#3753).
The operator-approved `worker.allow_local_binding` permission defaults to false.
Project definitions and machine-local overlays override `global.worker` defaults,
including an explicit false; default reloads preserve that local choice. The
effective grant changes the existing policy digest, while absent and false retain
the historical digest. Codex and Claude map the grant to their existing sandbox
local-binding setting and loopback hosts without changing the claimed tier,
filesystem roots, external domain allowlist, or Unix socket grants. This permits
local servers and host-loopback connections, not only test-owned servers. Codex
also disables its proxy private-destination check when this option is enabled;
proxy domain rules still apply. Restricted Codex turns retain disabled networking.
The operator-approved project `worker.extra_network_domains` list adds exact DNS
hosts to this same sandbox allowlist for builds needing external assets. It is
empty by default and changes the existing project policy digest when populated;
empty/absent retain historical approvals. Both backends use the per-turn project
grant. Wildcards, URLs, IPs, ports, duplicates and uppercase hosts are rejected.
The grant is scoped to the project, and restricted Codex turns keep networking
disabled. Domain validation, backend mapping and policy identity tests cover it.
`TestWorkerLocalBindingDefaults`, `TestRunnerPolicyUpgradeKeepsApprovedID`, and
the backend isolation tests cover inheritance, policy identity, and enforcement.

The existing lease owns execution isolation; the mutable routing cache is removed
from backend enforcement rather than adding another cache revision guard.
Claude verifies effective policy on its worker before sending a model prompt.
`TestRunnerIsolationClaims`, `TestRunnerIsolationHeartbeatWithdrawsTier`,
`TestProbeBackendTiers`, and each backend's `TestIsolationSettings` cover dispatch,
withdrawal, failed probes, and policy mapping. Workspace terminals report their
own measured isolation independently of agent sandbox support (native #162).

Runner policy identity (#3273) preserves the historical digest representation
of equivalent absent/default execution settings across binary upgrades. The
policy config normalization treats absent and `least_loaded` host selection
identically, omits empty host caps and unset local status, and retains the older
`[]` representation for empty required checks. Runtime defaults remain intact.
Explicit host preference, nonempty caps, local status, required checks, commands,
workspace paths, and effective prompts remain policy inputs requiring the existing
administrator approval. Source identity and administrator-authorized runner
requirements still match exactly; no mismatch bypass or recovery path is added.
`TestRunnerPolicyUpgradeKeepsApprovedID` pins a v0.117.1 approval with an explicit
workspace root and execution shells so host defaults cannot change the fixture.
Empty opt-out labels and the historical `requires-human-review` default share
the approved representation: runtime label matching always recognizes that
built-in label. Custom opt-out labels remain policy inputs. Zero-valued
human-review settings are omitted from policy
JSON, preserving the representation before that field existed, while enabling
human review changes both the config digest and gate descriptor (#3451).
Equivalent absent/default host and check settings still match; explicit policy
changes require reapproval (#3447, #3451).

Runner availability (#3169) reuses weekly-window evaluation and the existing
runner-capacity exclusion: runners report zero capacity outside the cached
window and do not claim new work locally. Active jobs retain lease authority
and finish normally. An optional deadline after the window closes cancels the
agent through its existing execution context, preserves and publishes unfinished
work to a WIP branch after confirmed worker shutdown, and reports interruption
through `Finish` without consuming failure retries. Publication reuses checkpoint
path, content, and history checks; a final checkpoint records the published head
under retained lease authority. Routing updates refresh the deadline in the
existing execution guard. A failed WIP
push retains local work and is logged as an instance failure. Sleep inhibition
is held during jobs and released when they finish. “Outside hours” is a plain
fleet status derived from the stored window; stale heartbeat, revoked, and
expired states take precedence. This adds no lane writer, recovery loop, or
exclusion reason.

Home-project spillover (#3170, human-approved) is claim-time eligibility using
one nullable `home_dry_since` timestamp per runner. Home projects are a subset
of administrator-authorized projects; spillover never widens grants or selectors.
Only dispatchable Todo/Rework home work counts, after dependency, policy,
selector, lease, and provider checks. Claims retain normal ordering among home
projects, clear the timestamp when home work is available, and start it only
when home work runs dry. General work becomes eligible after the configured
idle period. Active work finishes without preemption; workspace sessions do not
participate. There is no background loop or capacity reservation for home work.
`TestRunnerHomeClaims` and `TestRunnerHomeReturnAndOrdering` enforce this behavior.

Runner problems (#3171, human-approved) are contextual diagnostics, never
dispatch reason codes. Heartbeats replace the runner-reported `problems` list;
each problem carries a stable code, message, fix hint, and first-seen time.
Continuing codes retain their first-seen time, and omitted problems clear.
The existing instance telemetry loop also triggers the enrolled runner's
throttled heartbeat, so diagnostics remain reachable when every project fails
to start. Heartbeat requests do not block local telemetry publication.
The runner reports unavailable tiers, missing backends, unreachable host
services, invalid local settings, and sleep-inhibition failures without sending
local command output or service addresses in generated diagnostic messages.
The Hub derives unservable home work from the existing isolation advertisement,
rejected routing settings from the runner's application result, and unsupported
versions from the native protocol major (an omitted major remains compatible
with older heartbeats). Hub conditions clear when they no longer hold.
Problems raise `needs_attention` above online, outside hours, and offline;
revoked and expired credential states retain precedence. Separate connection
health preserves the existing offline dispatch exclusion. Diagnostics never
change grants, capacity, selectors, leases, or advertised isolation tiers.
Only needs-attention rows produce an alert signal. Outside hours, offline
outside the window, draining, and spillover remain plain statuses. The fleet
header links its nonzero attention count to a filtered list; messages and fix
hints remain on runner cards, with no global banner. Hosted readers do not see
home-project diagnostics for home assignments outside their project access.
`TestRunnerProblemsHeartbeat`, `TestRunnerHubProblems`, `TestMergeProblems`,
`TestIsolationProblems`, and `TestKeepAwakeProblems` cover replacement,
aggregation, timestamp stability, recovery, and dispatch independence.

Dispatch ordering (#3298) consolidates urgency into the existing comparator.
Merging remains first when the project config places it first; other lanes
compare tracker priority and configured label rank before lane rank, then retain
unblocker, age, and identifier ties.
A Todo hotfix can therefore precede ordinary Rework without a separate selector,
capacity change, configuration key, or new mechanism. `TestSortIssuesForDispatch`
covers cross-lane urgency, normalized Merging precedence, and equal-priority lane
ties; the existing safety boundary fuzz seeds remain required.

The stranded-active lane recovery is removed (#3238). In Progress remains an
active dispatch candidate after a long refresh, and the existing completion
transition owns finished attempts. The diagnostic snapshot still reports the
idle interval; `stranded_active_recovery` is removed from the transition-reason
allowlist so the deleted lane writer cannot be restored under that reason.
`TestTickDispatchesPlanApprovedIssueAfterLongRefresh` covers the reported
approval-to-dispatch sequence.

The worker GitHub REST budget monitor's credential-wide dispatch hold and
recovery canary are removed (#3248). A failed budget observation is now an
instance diagnostic; only a successful response can enforce the existing REST
reserve. This removes a self-protection path that converted a transient probe
timeout into a project-wide dispatch outage.

Generic issue no-progress outcomes do not become a shared project failure class
(#3489). Existing issue progress, retry, and workpad paths own those
outcomes; unrelated unpushed work and external evidence waits cannot pause the
whole project. Only structured backend errors, concrete startup failures, and
workspace infrastructure failures retain the configured project failure policy.
Arbitrary runner errors, merge receipt metadata, deliverable commands, token
ceilings, final states, and issue error classes remain with their existing
attempt, deliverable, policy, and provider capacity owners; they do not become
project-wide outages. This removes catch-all failure promotion under INV-2 and
INV-3 without another guard, reset, or recovery mechanism. Terminal attempt
outcomes and historical failure diagnostics remain unchanged.
`TestProjectAttemptFailureClass` and `TestGenericNoProgressDoesNotPauseProject`
cover repeated merge metadata failures, owned provider resets, unrelated stalls,
and preservation of a concrete backend failure pause.

Operator rejection (#2943) consolidates promotion eligibility with existing lane
history (INV-1). The reviewed `applyOperatorMove` fingerprint changes to hydrate
the PR best-effort before recording Rework and identify an otherwise unattributed
`operator_move` as human. Its dynamic reason selection and lane writer remain
unchanged; no new reason or recovery mechanism is introduced.

Startup workflow definition/validation failures use the project manager's existing terminal
unavailable-project reporting (#2969), including loads performed by the runner
factory. The optional historical-session attribution backfill defers when any
workflow definition is invalid, preserving ambiguity checks without aborting host
startup. Git execution and filesystem read failures remain instance-fatal, including
when a preceding project has an invalid definition. Classification lives in the
shared workflow loader instead of unconditional caller wrappers. The manager’s
duplicate startup/reload pause-reference validator is removed; the existing pause
monitor owns evaluation errors and keeps unresolved projects paused.
Workspace backend construction errors that identify an unusable configured path
(missing, non-directory, looping, too long, or unsupported) are project-definition
failures; they leave that project unavailable while other projects start. Host storage
and backend failures remain instance-fatal. An explicit manager reconciliation retries
terminal pending definitions even when the global project entry is unchanged, so a
corrected workflow path can recover without a restart. Runtime state-store failures
remain instance-fatal.
`TestStartupIsolatesWorkspacePathFailureAndReloads` covers both workspace and
source paths, another running project, and correction on reconciliation. It
asserts the failing backend stage and configured path across platforms rather
than requiring POSIX filesystem error wording (#3551).
`TestStartupIsolatesWorkflowLoadFailure` verifies healthy-project dispatch and
unavailable-project dashboard snapshots in both project orders, including a paused
project referencing the unavailable tracker. `TestStartupInfrastructureFailureRemainsFatal`
and `TestWorkflowLoadFailureClassification` preserve the infrastructure boundary
(INV-2); no retry or recovery path is added.

Codex workers use the backend's existing blocking terminal wait with a 50-minute
cap (#2936), replacing the instruction to return to the model every 55 seconds.
New and resumed worker threads receive the same native timeout override. The
worker's shorter stream-stall timeout is removed in favor of the existing stream
read timeout; runner turn/session deadlines and cancellation remain authoritative.
Tool-only operator sessions retain their configured stall timeout. This adds no
Detent configuration key, polling loop, watchdog, or recovery path.
`TestAgentBackendNativeCommandWait` covers quiet commands, resume, supplemental
tools, operator isolation, configured stream deadlines, and cancellation.

Operational completion (#2911) reuses the existing terminal issue closer and
`operational_completion` reason. Closure precedes terminal lane publication so
close failures leave the prior lane retryable; the existing completion comment
links closure across direct completion and stale merged/Merging reconciliation.
The reviewed transition fingerprints preserve existing reason
selection and add no mechanism, reason code, or recovery path.

Provider capacity retries use the earlier future provider resume deadline
(including reset jitter) or existing bounded probe deadline. A historical reset
time cannot suppress recovery probes after an external quota reset or account
change. The existing five-minute exponential backoff, capped at one hour, and
single in-flight probe still prevent full-width retry storms. Failed probes retain
the provider window and backoff; successful probes or authoritative available
status release existing capacity retries. Credential-file changes keep their
existing immediate probe notification. This replaces the deadline precedence from
#2912 without adding a watcher, pause, configuration, or recovery mechanism.
`TestBackendCapacityProviderResetWindow`,
`TestBackendCapacityDispatchAllowsOneResetProbe`, and
`TestBackendCapacityProbeFailureRefreshesProviderWindow` cover future reset
metadata, early restored capacity, single-probe ownership, retry release, backoff,
reset boundaries, and operator clear; safety fuzz seeds retain resume arithmetic.
`TestRunPausesBackendAfterQuotaErrorWithoutBreakerStrike` also asserts the
typed `usageLimitExceeded` completion keeps its provider reset and jittered
resume timestamps while scheduling the earlier bounded probe, without issue
failure strikes or a Blocked transition (#3680).

Workspace cleanup (#2913) uses one cancellable background execution of the existing
reaper instead of synchronous tick and completion sweeps. Each pass bounds tracker
candidate attempts and workspace removals to ten, rotating retained candidates.
The event loop alone applies cleanup results. Background cleanup never applies
tracker lane observations; the existing running-issue reconciliation owns those,
so a delayed sweep cannot restore a stale terminal lane after newer refreshes.
`TestWorkspaceCleanupNeverAppliesTrackerObservations` covers this boundary.
Workspace use and removal share
in-process synchronization so a stale sweep cannot delete a new worker's workspace.
Shutdown cancels and joins cleanup. `TestWorkspaceCleanupBatch`,
`TestStartupRefreshKeepsStateAndWorkerProgressObservable`,
`TestWorkspaceUseAndCleanup`, and `TestFilesystemCleanupBatch` cover batch bounds,
continued tick liveness, and workspace ownership. No configuration, lane writer,
recovery path, or operator-facing reason is added.

Cleanup delivery verification (#3484) uses the existing association reader with
status enrichment disabled and one fresh scalar PR read. Cached association
heads, issue closure, and branch deletion never prove delivery. Issue identity,
repository and PR number binding, merged state, merge time, and head remain
required before cleanup receives a delivered head. CI, workflow, review, and
branch-policy reads remain enabled for live merge evaluation.
`TestRevalidatePullRequestAssociationWithoutStatus` and
`TestVerifyCleanupDelivery` cover this read consolidation and evidence boundary.

PR conflict classification and conflict-cleared progress share connector helpers
(#2934), replacing separate completion, spend, dispatch, and display checks.
Both tracker conflict spellings are recognized. A transition to a reported
non-conflict state, including draft or unknown during recomputation, credits
progress without authorizing a merge. Rebase-only Workpad completion reuses the
attempt-and-generation matcher and requires an open, non-draft, conflict-free PR
with passing tracker CI. Stale completion assertions cannot credit later rebases.
`TestCompletionRebaseWorkpadIdentity`, `TestCompletionRebaseProgress`, and
`TestPullRequestConflictCleared` cover these consolidated decisions.

Pushed-branch deliverable recovery (#2601) resolves an absent workspace head from
GitHub's remote branch, removing local workspace retention as a prerequisite for
the existing exact-head draft creation and Rework transition. Remote lookup
failures reuse the existing unavailable-lookup defer outcome. No recovery loop,
reason code, or configuration is added. `TestRecoverBlockedReadyPullRequestExactHeadLookup`
covers human-owned parks with missing workspaces and remote lookup failures.

Heartbeat writes (#2871) share a bounded per-write context and one retry across
dedicated, tick, and worker-progress paths, replacing the unreachable same-context
retry. Caller cancellation and terminal attempts remain authoritative. Checkpoints
renew an expired lease only for the same active implementer attempt and generation
in the current tracker lane, then recheck runtime ownership after the write.
`TestHeartbeatWriteDeadlineRecovery` exercises real SQLite after a blocked write;
`TestCheckpointValidator` and `TestCheckpointRenewsExpiredLease` preserve ownership
and renewal behavior. This consolidates heartbeat persistence and removes elapsed
lease time as an independent veto on a live owner's checkpoint; no new recovery
loop, configuration, or reason code is added.


ProjectV2 refresh pages include supported project fields and their timestamps
(#2859), consolidating field retrieval into the board scan and removing dispatch's
per-candidate REST hydration. Label-only authorization declines run before the
hydrate hook, with field-dependent authorization still evaluated afterward.
`TestProjectRefreshDispatchAvoidsIssueReads` exercises two 150-candidate cycles
through the real connector and dispatch hydration hooks; nested selector and retry
coverage preserves existing authorization semantics. No cache, configuration,
reason code, or recovery mechanism is added.
Early authorization declines of due retries use the existing release helpers to
clear retry ownership and budget refusals while preserving blocked state.
`TestDispatchLabelAuthorizationBeforeHydration` covers that cleanup for blocked
and unblocked retries without hydration.

Candidate hydration (#2844) consolidates completed scheduler evidence into the
existing board refresh scan. Bounded hydration runs between board pages and
retains completed batches across interruptions; transient hydration failures no
longer discard progress and fan out through REST. Resume validates all retained
comment IDs and edit timestamps and each blocker's own updatedAt before reusing
evidence. A completed scan retained after a status failure restarts board
enumeration on the next refresh; project updatedAt alone cannot validate lanes.
Scheduler queries have at most 25 aliases. PR association and status observation
run once per scan page over completed scheduler evidence, including partial
progress on failure (#2860). PRs shared across scheduler batches are deduplicated
within the page; snapshots keep 100-item connections and batch up to 20 PRs per
request (208,020 connection nodes). No timer,
pacer, configuration key, or recovery loop is added.
`TestRefreshHydrationResumes`, `TestCandidateHydrationShape`, the large-board
`TestProjectRefreshHourlyWorkload` scenarios, `TestRefreshAfterStatusFailure`,
`TestCandidatePageObservation`, and
`TestCandidatePRLargeCollectionsRemainAuthoritative` cover this consolidation.

Resumed board scans reobserve retained PR evidence once before continuing
hydration (#3072). Checks and mergeability can change on the same base/head
without an issue revision; retaining scheduler evidence must not freeze those
observations. `TestRetainedRefreshObservesCompletedCheckOnSameHead` covers
pending, successful, and failed checks and entry into the trusted audit stage.
The hourly workload scenarios bound the additional resumed-scan requests.

Non-draft dirty PRs in In Progress reuse the existing merge-mode precheck,
fallback rebase prompt, and deterministic verification (#2842), regardless of
the programmatic merge fast-path flag. Verified repairs rejoin ordinary progress
accounting with the changed PR head and retain their source lane without merge
reservations or programmatic merging. Explicit fallback rework findings and a
head replaced after verification use the existing Rework handoff. Merging keeps
its existing CI wait and merge behavior. Rework uses ordinary implementation
routing even while the remote PR remains conflicted (#3475), so unfinished
source and test changes receive an implementation worker instead of repeatedly
entering conflict-only merge fallback. Same-lane Rework handoffs retain this
routing without adding a retry mechanism or exception flag.
`TestDispatchModeMergingFastPathFlag`,
`TestMergeFallbackRoutesBoundedOutcomesToRework`, and
`TestMergeFallbackResolvedHeadHandoff` cover this consolidation; no prompt, mode,
reason code, or recovery mechanism is added. Repair runs retain existing
configured progress and budget controls. Draft PRs retain implementation routing so their
author can finish. Repairs share the existing merge duration bound and treat
unstarted CI like implementation runs (waiting only after push or in waiting_ci).
The dispatch, CI parity, and duration regressions cover these boundaries (#2845).
`TestCompletionRebaseAfterRestart` preserves the persisted diff and conflict
baseline through restart for both Rework implementation and In Progress merge
repair. `TestRepairDurationBound` applies the merge duration ceiling only to
merge-mode runs and verifies ordinary cancellation for Rework (#3548).

Rework dispatch (#2800) reads the gate's live `AutomatedReviewPending()`
predicate for clean, green PRs without actionable threads or findings. The existing
`awaiting_gate` decision no longer requires retained completion evidence for this
case, preventing repeated sessions while a review is pending.
`TestReworkLiveReviewGateDispatch` replays post-answer scheduler passes and checks
that a current-head review or actionable PR state preserves dispatch eligibility.
Failed current-head CI ends a completed gate wait when `ci_failure_action: rework`
routes repair (#2909), including when the card is already in Rework. The existing
`ci_not_green` decision clears completion dispatch memory and supplies the worker
handoff without a redundant lane write. `TestCompletedReworkCIGateDispatch`
covers immediate repair dispatch and preserves pending CI and review waits.

Human-owned Workpad blockers route through the existing completion Blocked
transition on the first report (#2779). The repeated-report threshold is removed:
live blocker evaluation already suppresses the next dispatch, so a second
completion cannot be required. `TestFirstHumanBlockerCompletionReachesBlocked`
replays that conflict with and without a PR and preserves human-owned recovery.


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
Startup process reconciliation feeds confirmed-gone instance-owned attempts to
the existing expired-lease query across all projects before project loading
(#2749), including removed projects. The parallel bulk reclaim API/query is
removed. The store is per instance; `worker_host` values are scheduling pool
labels, not instance identities, so all registered processes and attempts are
reconciled regardless of their pool label. Failed process termination prevents
startup expiry.
Project refresh retains lease criteria and running-attempt exclusions; deferred
completions remain excluded in the shared query. Interrupted sessions retain
orphan-session resume eligibility. `TestStartupRetainsOwnedWorkAttempts` covers
processes whose termination fails with live/expired leases and local/pool labels.
`TestReapPoolWorkerProcesses` covers startup/shutdown reaping of pool-labelled
workers and startup reclamation before lease expiry. Historical abandoned
`service_restart` sessions remain eligible for resume when their project is
loaded and their issue is active; eligibility is intentionally not limited to
the latest boot.
Recorded processless stops use
the existing completion and pending operator-stop records without requiring a
running project; live workers still require the orchestrator. Live and recorded
stops share route normalization, validation, and completion construction. Configured
default/custom destinations survive project initialization. Unreadable or invalid pending
workflows still permit record-only and canonical stops; removed projects reuse the
standard Todo priority names through shared priority normalization. Linked session completion
is atomic with attempt completion and preserves an existing finish timestamp. Covered by
`TestStartupReclaimsProcesslessWorkAttempts`, `TestStopRecordedRunBeforeProjectStartup`,
`TestStopRecordedRun`, and `TestReapWorkerProcessesPreservesInterruptedSession`.

GitHub secondary throttling (#2829, #2833) uses the credential-scoped cooldown
deadline in the existing client backoff registry for both
request admission and the existing scheduler lookup backoff. Retry-After (or a
one-minute fallback, increasing on repeated failures) gates queries, probes, and
mutations. Usage flushes and primary-budget refreshes cannot clear this deadline;
an expired secondary deadline alone cannot restart dispatch recovery or suppress
a fresh connector/telemetry backoff signal. Clients sharing a credential identity
(including rotated installation tokens) share the deadline and failure count.
Only blank-status repair batches serialize on that same state and stop on
throttling (#2837). Ordinary mutations, including nested calls, proceed concurrently
outside the shared cooldown; during it they return the remaining retry delay. This consolidates
the two cooldown schedules without adding a timer or configuration surface.
`TestClientGraphQLSecondaryBackoffExpires`, `TestClientGraphQLSecondaryRepeatedFailures`,
`TestClientGraphQLSecondarySharedAcrossProjects`, `TestClientGraphQLSharedMutationAdmission`,
`TestGitHubLookupBackoffDelayAfterSecondaryExpiry`, and
`TestGitHubLookupBackoffSecondaryDeadline` cover these boundaries. Primary exhaustion
expires using the existing snapshot reset and clock-skew allowance (#2998), so
lookup-only clients can refresh their budget. Paused lookup retry delays count
down to that deadline and remain nonnegative. An active secondary deadline still
takes precedence; `TestClientGraphQLPrimaryExhaustionExpires` covers recovery
with and without a reserve and with an overlapping secondary cooldown.

Legacy worker caches are removed at project startup using an absolute, home-expanded
workspace root (#2742). The obsolete per-project shared-cache sweep and its state
fields are removed; the existing host cache trim and report are the single cache
mechanism. Doctor retains read-only warnings for legacy roots. `TestRemoveLegacy`
covers absent, absolute, and tilde roots.

Update and restart draining (#2745) reuses the runtime dispatch pause and session
limits. Manual runtime update requests use the same drain reservation as automatic
updates; SIGTERM shutdown uses that duration ceiling, including model-selection
levels. Managed restarts preserve child processes while the orchestrator drains.
Hub runners (#184) default to automatic updates through that same idle/drain
path, retaining explicit check/apply opt-outs. Checks and applies resolve the
Hub's running version through authenticated native capabilities and fetch that
exact published release; an older Hub never downgrades a runner, and unavailable
or invalid Hub targets never fall back to GitHub latest. Candidate signatures,
provenance and startup crash-loop rollback remain required.
`TestServiceChoosesHubUpdateTarget`, `TestHubRunnerUpdateDefaultsAndOptOut` and
`TestRunnerClientEnrollmentSchedulingAndRotationRecovery` cover target selection,
persisted opt-outs and enrolled version reads.
Hub claim admission (#185) publishes its semantic release version as the minimum
runner version in native capabilities, fleet and update reports. The existing
claim transaction refuses new leases for older reported releases with the same
version-specific reason rendered by the fleet badge and returned to runner logs.
Equal, newer and unversioned development builds remain eligible; a development
Hub publishes no floor. Existing session retries, renewals, events and completion
retain their current authority and are never revoked by the floor.
`TestNativeRunnerMinimumVersion`, `TestNativeClaimsEventsAndRestartWithoutGitHub`
and `TestAppUpdates` cover admission boundaries, continued sessions and the
shared refusal reason.

Urgent Hub organization requests (#186) invoke that existing enrolled drain
asynchronously with the runner's runtime context; claims stop while heartbeats,
lease renewals and active sessions continue. The selected urgent release stays
pinned through the drain, and existing startup evidence removes the derived
routing drain. `TestUrgentRunnerUpdateFleet` and
`TestUrgentUpdateOwnerKeepsHeartbeatsAvailable` cover this INV-3 reuse.
The shutdown drain uses the existing drain-budget timer rather than the five-second
cleanup context (#2795); shorter parent deadlines emit an error with both budgets.
`TestShutdownDrainBudget` covers delayed drain acknowledgment, and the live-session
shutdown regression crosses the cleanup deadline before allowing completion.
Startup no longer unconditionally bulk-reclaims live work attempts as
`service_restart`; the parallel reclaim store API and query are removed. Historical restart rows remain readable
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
changes do not reset attempts or lifetime usage. Turn inactivity remains
unchanged. The separate session no-progress cancellation was removed (#3252):
a live local gate wait relies on the gate lock deadline and absolute worker
session bound, with no issue Rework transition for unchanged work product during
the wait. Covered by `TestModelSelectionSessionLimits`,
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

Human Review conflict routing preserves durable `operator_move` lane entries.
Historical lifetime-allowance arrivals no longer veto ordinary conflict repair. This narrows the existing
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
The same fresh publication owner resolves an already-merged operational receipt
against current forge references before applying that merged lifecycle, including
a different merging PR from a closed draft. Its freshly read canonical comments
remain authoritative through PR hydration. The existing fixture covers corrected
receipts, wrong integration branches, missing references, current human action
and preserved lanes. An invalid receipt remains unverified; this does not repair
its ancestry assertion or erase historical failed attempts.
Ready In Progress PRs also enter the existing repair evaluation before worker
completion (#2976). Unresolved review threads and failing CI reuse the existing
Rework decisions and audit comments, including during the final running attempt.
Configured source, pass, and rework lanes retain their normal promotion behavior.
This consolidates repair routing with Human Review; it does not authorize
promotion of unfinished work or routing drafts. `TestAutoPromoteReadyPullRequestRepairs`
and `TestAutoPromoteConfiguredInProgressSourcePromotesReadyPullRequest` cover
both lanes, active workers, configured source lanes, draft exclusion, unfinished
clean heads, pending checks, closed PRs, opt-out labels, and unavailable evidence.
Idle Rework PRs enter the existing promotion evaluation without a worker completion
record (#2688). Promotion reuses the merge worker readiness predicate and live PR
hydration; unresolved threads and known audit failures still prevent promotion.
Missing audits start while the completed Rework issue waits, and a trusted
current-head pass is required before promotion to Merging. `TestReworkLivePullRequestPromotion`,
`TestReworkLiveDraftPromotion`, and `TestReworkLiveSecurityAudit` cover this
consolidation; no new transition reason or recovery loop is introduced.

Legacy active triage retains its single durable result while current-head CI is
queued or running and no required check is missing. It does not publish a stall
note or park an issue for that Detent-owned wait. Existing blocked recovery
retires historical lifetime-allowance parks to their recorded prior In Progress
or Rework lane behind live human, dependency and uncertainty checks.
`TestRetiredAttemptTriageParkRestoresPriorLane` preserves these boundaries with
real SQLite attempt and lane history. Returning to an active lane does not
accept sparse authored operational claims or closed drafts; verified completion
and actual merge evidence keep their existing owners. Recovery does not
synthesize a new allowance receipt for unavailable dependencies, database or PR
reads, and does not reset historical records.
Historical triage parks from In Progress or Rework return to that prior lane even
with an associated PR. CI and audit readiness remain with that lane's existing
owners; they do not hold recovery of the retired park.
A historical triage park from another lane with a recorded PR head still uses
the same promotion gate: a clean, green unchanged or newer head returns to Merging for
the exact-head audit. Recovery evaluates audit eligibility once through the
shared Rework readiness predicate, then applies the remaining promotion gates.
This local evaluation leaves the instance audit requirement enabled for Merging.
Human actions, failing checks, running or failed audits, and known audit findings
continue to hold. Historical parks cannot be removed from the tracker by the
triage change alone, so this reuses the existing sweep and lane action rather
than adding another recovery loop or reason code (#3064).
`TestAttemptAllowanceLiveHead` and
`TestAttemptTriageParkRecoversOnCleanGreenHead` cover the wait and recovery.

The hourly source-invariant repair (#3656) refreshes the reviewed digest for
`transitionCompletedActiveIssuesToReviewWithHydratedValidatorHeads` after
PR #3590 consolidated already-merged completion under the existing merged-PR
owner. Review confirms its fallback lane-reason selection is unchanged; the
merged path reuses that owner's existing reasons and ledger. No mechanism,
reason, or lane writer is added. `TestRepositorySources` still rejects further
edits until reviewed.

The scheduled source-invariant repair (#3771) refreshes two reviewed digests.
`applyOperatorMove` now returns an outcome for the existing dispatch refill path
(#3736); its reason remains the operator-supplied reason or `operator_move`,
written through the same lane ledger. `blockDeliverableRecoveryFailure` now
receives the delivery error classified by its caller (#3763), consolidating
whole-error ownership there instead of extracting a nested error again. Its
reason still comes from `deliverableRecoveryParkReason`, using the existing
delivery hold reasons and evidence. Neither change adds a mechanism, lane
writer, or reason source. The source check continues to reject further edits
until reviewed; the reason vocabulary and enforcement logic are unchanged.

The scheduled coverage repair (#3545) refreshes the reviewed digest for
`updateIssueStateByIDWithMetadataMode` after #3560 rewrote `!(A && B)` as
`!A || !B`. The equivalent delivery-time condition preserves reason sources
and ledger ownership; no lane-transition vocabulary or mechanism changes.

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

Runner boot (#132) publishes the existing initializing snapshot without reading
lifetime totals and serves HTTP before startup process cleanup, Codex retention,
board-cache loading, or local setup/policy/provider observations. The initial
snapshot has no scheduled refresh deadline until dispatch starts, so
maintenance time cannot turn initializing tracker state into a late refresh.
The existing listener identity probe runs in the joined startup worker before that work;
updater health verification and lifecycle readiness still wait for startup.
The manager receives its scheduling source only after actual setup observations
complete, so a responsive initializing health route grants no dispatch authority.
Enrolled identity, current approved policy, provider authority, grants, leases
and fencing retain their existing admission owners. Observation failures remain
instance-owned startup failures; cancellation joins startup and service resources
before closing the runtime store. No maintenance loop, heartbeat audit, route,
configuration, reason code or admission bypass is introduced. Startup logs
separate listener binding, HTTP response availability, setup observation duration
and lifecycle readiness; existing project refresh events retain refresh timing.
An aggregate restart interval cannot attribute time to any one operation.
`TestStartRunningServesWhileMaintenanceBlocked` covers HTTP/state availability,
honest readiness, deferred dispatch, missing Hub authority and joined cancellation;
existing setup-heartbeat, policy and lifecycle-failure tests retain their boundaries.

GitHub dependency hydration consolidates native relations and current issue-body
declarations into one blocker list (#2751). Native state wins for duplicate refs;
an empty native list does not discard a current `Depends on:` declaration.
Unsupported repositories use the same body parser. Historical comments remain
diagnostic only, and refresh replaces the previous list rather than restoring
removed declarations. Public reads select dependencies before resolving blocker
state; the orchestrator consumes that list through its existing dependency gate.
Degraded native reads and unresolved blocker-state lookups propagate errors,
preventing candidates with unknown blocker state from dispatching. Both parsers
share fence-aware declaration scanning; fenced examples never add blockers.
Declaration reference lists stop at sentence boundaries, end of line, or trailing
prose; explicit `none`, `n/a`, and `-` declarations add no blockers (#2873).
Native relations remain authoritative even with an explicit empty body declaration.
`TestDependencyDeclarationSentenceBoundary` and `TestDispatchDependencyProseCycle`
cover prose-only cycles and preserve explicit dependency waits. Workpad issue-state predicates
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

For a terminal native issue whose Change Request has a recorded landing,
cleanup also accepts a clean worktree and branch still at that landing's head.
This covers squash commits, which cannot prove delivery through branch ancestry.
Later commits, uncommitted files, nonterminal issues, and issues without a
recorded landing retain the existing protection. The normal reaper removes the
landed workspace and clears its cleanup failure; no new sweep or retention
state is introduced (#3234).
GitHub terminal cleanup and the supported `cleanup_workspace` action refresh
the issue-to-PR association and hydrate the PR again before accepting the exact
merged head (#3093). A verified repository/PR identity, successful merge time,
and available current head are required. This proof exists only for that cleanup
call and shares the native landing checks; it is never persisted as tracker
state. Deleted branches, cached merged snapshots, and issue closure alone do
not establish delivery. Both branch and worktree must still match the delivered
head, and the normal `before_remove` lifecycle rechecks local work.
`TestVerifyCleanupDelivery` covers unavailable and mismatched verification;
`TestLocalGitCleanupRecordedLanding` reproduces squash delivery after source
branch deletion and checks later/dirty/mismatched work and directory absence.
`TestRunnerReapSquashLandedWorkspace` covers proof propagation through the runner.

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
Quarantine removal in this existing sweep makes read-only directories writable
before deletion; persistent failures are warned once per path while later
sweeps still retry (#3039). This does not add a sweep or recovery path.
Warning deduplication uses native path separators on Windows as well as POSIX
(#3551). `TestRetentionQuarantineWarningOncePerPath` replays repeated removal
errors on every platform; the chmod-based filesystem fixture runs only on POSIX,
where removing directory write permission prevents removal.
`TestRetentionCompletionClock`, `TestRetentionCompletedWorkspace`, and
`TestRetentionRemovalFailureDeduplicatesArchives` cover terminal-state clocks,
lossless expiry, and repeated removal failures. See
[workspace retention](workspace-retention.md) for limits and recovery instructions.

The existing automated-review check retains stale and in-progress bot summary
evidence (#2764). Native queue admission and programmatic merge wait for an
established cycle at the current head only when the effective gate requires
automated review (#2905). Review requests are likewise posted only for a required
automated review. Trusted quota
or unavailable replies after the latest per-head request complete that head as
COMMENTED with no findings; an unanswered request alone is not review evidence.
The existing gate deadline also ends a pending-review wait, including when
automated review is disabled.
The existing `automated_review_missing` wait and PR comment publication are reused;
a per-head comment marker deduplicates `@codex review` requests across ticks and
restarts. No new gate, reason, or reconciliation loop is introduced.
`TestEvaluateAutomatedReviewModes`,
`TestEvaluatePendingReviewExpiresAfterRepeatedMissingDecisions`,
`TestAutoPromoteReviewAtHead`, `TestReviewHeadRequestAndWait`, and
`TestReviewSummaryRetainsPendingHead` cover decision, request failures/deduplication,
and trusted summary evidence respectively.
`TestReworkLiveDraftPromotion` keeps drafts out of promotion without marking
them ready; the implementation worker owns publishing a reviewable head (#2922).

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

Worker scratch cleanup (#3291, #3292) removes attempt data without deleting
its workspace parent or shared group. Explicit workspace deletion removes only
that workspace's scratch root. Keeping shared parents stable removes the
creation-versus-cleanup race and the bounded scratch-creation retry; no lock,
platform error retry, or new recovery path replaces them. Existing retention
continues to sweep stale attempts and roots of removed workspaces.
`TestPrepareWorkerScratchToleratesConcurrentSiblingCleanup` covers concurrent
attempt cleanup within one workspace and across siblings, plus sibling root
removal. `TestRemoveWorkerScratchRootKeepsSharedGroup` covers explicit
root deletion without shared-parent removal.

The approved Cloud domain migration (#3241) consolidates shared hosted origin
comparison around the explicit production and staging alias pairs. Reopening a
shared tenant may use its environment's old or canonical hostname while keeping
its immutable stored origin as history. Organization, provider, bootstrap identity,
deployment mode and allocation generation must still match. This introduces no
binding migration, revocation, configuration key or recovery path; self-hosted
origin binding stays exact. `TestHostedDatabaseCloudAliases` covers forward
migration, rollback, unchanged stored bindings and rejected environment/identity
substitutions.

The existing automatic backlog admission policy no longer shares the pending
human proposal storage cap. Per-run evaluation/comment budgets and current
criteria, confidence, author, effort and dependency checks remain authoritative;
failed criteria or low confidence cannot overflow the human proposal queue.
Prerequisite preference shares the existing dispatcher annotation and published
owner evidence instead of another graph reader or promotion mechanism (#3519).
Admission prerequisite ranking consumes the same fresh dependency readiness
evidence as its criteria checks. The existing dispatcher annotation receives a
ranking-only projection; unresolved, failed, or unverified human prerequisites
remain nonterminal. Only the resulting unblocker counts return to the original
candidate, preserving dependency identities, provenance, fingerprints and final
revalidation without another read or ranking mechanism.
The initial candidate window reuses successfully resolved canonical prerequisite
facts from its operation-owned candidate evidence (#3730). Each candidate retains
its own observation clock, readiness projection and fingerprint; missing or
failed references and unqualified local aliases are resolved independently.
Final candidate and dependency revalidation still reads fresh tracker evidence.
GitHub admission source reads preserve own-issue hydration and canonical native/body
prerequisite identities without final external state enrichment (#3737). Already-present
native facts survive the source read but do not establish readiness. Admission's
existing resolver owns current state, merged PR, and human completion authority
before history, ranking, and fingerprints, and independently revalidates after
model work. Missing, inaccessible, ambiguous, and failed references remain nonready.
Normal tracker candidate, observed, and state refresh enrichment and local tracker
delegation retain their existing behavior. `TestCandidateBodyDependencyRefresh`
checks this intentionally narrower reader contract and normal refresh controls.
This consolidates duplicate reference hydration without a separate cache or reader.
`TestManagerAdmissionUsesAcceptedDependencyFrontier` covers shared-reference reads
and unchanged evidence; `TestAdmissionRevalidatesDependenciesDuringEvaluation`
rejects both candidates when their shared prerequisite changes after evaluation.

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

The hardcoded lifetime ceiling of three implementation/rework sessions is retired.
Dispatch and automatic promotion use their existing current-head, acceptance,
gate, human, dependency, budget and ownership controls instead of reading all
historical sessions and replacing the fourth worker with triage. The existing
configured no-progress owner retains its evaluated signature and consecutive
count; a duplicate dispatch-loop reset no longer erases that decision. Terminal
no-product retries retain their configured limit (default three), and instance
failures remain instance-attributed. There is no replacement global ceiling.
With no-progress disabled (the default zero) and optional budgets disabled,
progressing or pushed-product retries may continue under those project policies.

Historical `attempt_allowance_exhausted` receipts and active read-only triage
attempts remain readable. Existing blocked-cause reconciliation returns retired
allowance parks to their recorded In Progress or Rework lane only after current
human, dependency and uncertain-evidence guards permit recovery. It does not
reset history, infer acceptance, manufacture Todo work, or bypass PR/validator
checks. Other recorded prior lanes retain the existing ready-PR recovery owner.
Legacy triage publication remains idempotent and preserves operator lane changes.
`TestDispatchRetainsConfiguredModeAfterPriorSessions`,
`TestUnfinishedSessionsRetainConfiguredDispatch`,
`TestRetiredAttemptTriageParkRestoresPriorLane`, and the existing implement
progress and terminal retry matrices cover the retirement and retained controls.
The invariant manifest requires the configured-dispatch and retired-park tests
instead of the renamed or deleted lifetime-cap tests (#3833), preserving
behavioral enforcement without restoring the retired mechanism.
`TestAttemptAllowanceTriagePublication`, `TestAttemptAllowanceNoteFormat`,
`TestAttemptAllowancePreservesOperatorCompletionLane`, and
`TestAttemptAllowanceTriageFallbackCompletion` preserve in-flight compatibility.

Non-PR artifact and explicit operational completion workflows retain their own
deliverable rules. Already-merged completion does not require pre-dispatch
authorization: the worker records the merging PR, commit, tracked branch/head,
successful ancestry check, and acceptance evidence. Existing operational
completion and current-attempt checks persist and publish this evidence before
Done; incomplete evidence retains the PR gate. `TestMergedCompletionEvidence`
and `TestTransitionAlreadyMergedCompletion` cover that contract.

Scheduled routine occurrences advance from their durable `scheduled_for` identity
regardless of success or failure. A selected occurrence whose existing ownership
context is already canceled is consumed without starting an agent. This removes the
implicit same-slot retry and relies on the existing schedule-ownership context rather
than adding a retry, backoff, or lease mechanism (#2526).

The backlog human proposal cap bounds pending human decisions, not opted-in
automatic evaluation. Automatic admission remains bounded by the existing run,
comment and auto-admission budgets, and requires the configured confidence,
criteria, author, effort and fresh dependency checks. Disabled automatic policy
keeps the pending proposal capacity limit. Failed criteria and low confidence do
not create extra human proposals when that queue is full. Durable proposal and
orchestrator lane ownership remain unchanged (#3519).

Validated admission outcomes are retained in the existing bounded run issue
receipts after fresh eligibility and dependency checks (#3718). Confidence,
historical threshold, effective automatic qualification, and bounded criterion
indices/fingerprints/pass-fail results are audit evidence only. Truncated results
retain their total; qualification still evaluates every configured dimension.
Missing, malformed, stale and legacy evaluations remain unknown. No private
criterion text or rationale is retained in these outcomes, and no reader, write,
policy or admission authority is added.

Admission prerequisite ranking uses the same unblocker annotation as dispatch,
with the orchestrator's published active/Blocked dependency cohort. It performs
no extra forge scan and authorizes no lane change; fresh admission checks remain
authoritative. This removes a disconnected ranking path (#3519).

Backlog admission selection uses the existing run issue records for evaluation
identity, fingerprints, and stale verdicts. Unchanged stale candidates join the
existing epic exclusions before the window is capped; remaining candidates rotate
by last evaluation time. This replaces the fixed evaluation window with selection
from run/skip bookkeeping, without a new routine, reason code, or suppression table
(#2568). Saved stale verdicts and new evaluations share the same point-lookup
revalidation; a candidate returning to its original eligible snapshot can re-enter
the window. This removes unconditional historical suppression across eligibility
cycles.

Malformed output has no lifetime eligibility veto (#3739). Unchanged candidates
remain eligible for scheduled reevaluation through the same rotation, window cap,
capacity and live budget checks. New unresolved observations stay retryable beyond
four attempts, retaining counts, fingerprints, redacted error metadata,
deduplication and timestamps. Legacy blocked observations remain readable as
historical evidence; valid evaluations resolve them through the existing success
owner. Malformed evidence grants no proposal or lane write, and valid evaluations
retain every current admission requirement.

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
against the project update timestamp and local cache revision before resuming,
using an updatedAt-only preflight. Changes during enumeration do not discard the
scan, but final pageInfo alone cannot prove completeness: the existing count
check rejects enumerations below the first-page totalCount and clears their cursor
for a fresh attempt (#2830). Later count growth does not raise this baseline.
Thin board metadata and scalar bodies supply lane diagnostics and avoid REST body fanout. Requested
candidates and configured active states, plus observed states supplied by the
auto-promote, plan-stop, dependency auto-unblock, blocked-recovery, and blocker
auto-promote readers, receive batched scheduler and authoritative PR evidence.
Observed PR fallback retains the existing status policy, avoiding duplicate REST
reads for candidate and active lanes.
Observed-only and configured terminal states stay thin; Backlog used as an active
or candidate state receives enrichment. No retry loop, timer, configuration, or lane writer
is added. `TestRefreshBoardResumesFailedPage` covers page failure and cancellation;
`TestRefreshBoardChangedBetweenAttempts` covers shifted pages and missing revisions;
`TestRefreshBusyBoardCompletes` covers continuous timestamp, count-growth, and
local changes. `TestRefreshConfiguredSchedulerStates` covers custom state sets; `TestRefreshHintRoutingStates` checks routing reader configuration and
`TestRefreshTruncatedEnumeration` rejects partial publication and verifies a fresh
retry. `TestRefreshThinBodyFallback` checks 152 candidates under GraphQL
backoff against the existing REST cap, without publishing partial results.
`TestProjectRefreshHourlyWorkload` checks the large-board request and point budget
using measured GraphQL response costs, including resumed preflight requests and
Backlog exclusion (#2825).

Admission candidate scans consolidate REST label, repository, and issue-field
pagination into one reader, retaining page/item continuation in the existing
admission run ledger (#2574). Completed hydrated candidates remain usable when
the existing fanout budget ends a read; no retry routine, reason, or configuration
is added. Exhausting a source resets its continuation for the next scan.
If that budget also prevents a saved stale snapshot's early recheck, its
eligibility check consolidates into the mandatory final revalidation; other
completed candidates can still use the evaluation window.

No Detent push may drop commits from a PR branch (#2874). Missing worktrees
restore an existing published branch from a freshly fetched origin ref; only
new branches start at the base. Before the fast path rebases, the observed remote
head must be an ancestor of the local head. The rebase may rewrite those commits,
but the push uses the lease on that same observed remote head. Agent conflict
resolution merges the target into the PR branch, preserving remote ancestry,
which is checked before validation. A resolved head passes the configured gate
before a normal push; a gate or push error fails the attempt, and a divergent
remote advance makes the push fail without replacing published work. A retry
with an unpublished local head repeats verified publication when it contains
the current target; otherwise it returns to worker conflict resolution instead
of the fast-path rebase and push. Unsafe local heads abort any unfinished rebase
and attempt to restore the observed PR head without discarding
uncommitted work. Restoration failures remain conflicts with diagnostic details,
not runner failures. No push may replace published work with a stale or freshly
created base branch.
`TestLocalGitMergePreservesRemoteHistory` covers recreation, stale local branches,
resolved heads, rebasing published history, unfinished rebases, and untracked
files that prevent restoration.

Security audit verdict routing from Merging shares the existing auto-promote
classifier and findings publisher with source-lane completion (#2642, #2726).
The merge completion handler routes actionable findings through its existing
Rework handler immediately, clears retries, and includes findings on the issue;
pending evidence and audit infrastructure failures retain their existing wait
without publishing issue findings. Findings publish to the PR
before the existing lane writer routes to Rework; the trusted run marker prevents
repeat publication after a failed lane write. Comments remain explanatory, never
verdict evidence. Required-gate telemetry uses the same audit classifier.
For an exact base and head, a durable trusted pass takes precedence over the
in-memory running-stage marker; failed or inconclusive reads retain the running
wait. Both the required-gate projection and
auto-promotion read that result on their next evaluation; an old-head result
cannot satisfy either gate. `TestDurableAuditPassSupersedesRunningStageForGate`
covers a pass recorded after an initially missing green-CI gate.
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

The shared lane writer attempts native queue withdrawal before a departure
from Merging, but withdrawal failure does not block the lane write (#2826). Its reviewed INV-3 dynamic-reason fingerprint changes
without altering reason forwarding; operator destinations and reasons remain
intact, covered by `TestNativeMergeQueueReviewReworkAfterEnqueue`.

Planning-only prompts do not append source or merge implementation handoffs.
The existing plan-only boundary owns those instructions; source workers continue
to yield when current-head CI is the only unfinished work.
`TestPlanOnlyPromptOmitsCIImplementationHandoff` covers this boundary (#3076).

Slot refill reuses ordinary dispatch eligibility and releases deferred Hub claims
through the existing claim writer. Fresh Hub scheduling results are already
claimed and remain eligible even when absent from the previous refresh; excluded
or ineligible claims are released. Ordinary tracker candidates use the same fresh
cohort and eligibility owner, without a previous-snapshot membership bound.
`TestHubRefillRetainsNewClaims` and
`TestEventAndTickDispatchEligibilityParity` cover these boundaries (#3219).

Successful owned operator lane writes into configured active lanes (#3732),
including native Backlog admission, trigger that existing refill once alongside
its Running-shrink trigger. The transition comes from the current issue state
already obtained before the strict write, carried privately to the actor;
request FromState and public Blocked-cleanup results retain their semantics.
Failed writes, no-op transitions, removals, inactive destinations and foreign
tracker writes do not trigger admission refill. Fresh candidates still pass
ordinary dispatch policy, capacity, draining and quiescence checks.
`TestRunDispatchesOperatorMovedIssue` covers admission before an hour-long poll
and the refusal controls without adding a timer, reader or dispatch owner.

Native issue archive (#3267) preserves workflow state and all issue, comment,
attempt, change, version, and audit records. Archive refuses live ownership
using the existing lease lifecycle (expired or released attempts are interrupted), and archived issues are
excluded from tracker and claim candidates. Restore allocates an unarchived
issue through the existing hosted transaction, as do native creation and import
pages. Hosted usage counts native unarchived issues per organization across
projects, including terminal issues; over-limit reads, exports, and reductions
remain available. Archive reuses the completion mutation exemptions for its
retained audit records. Catalogs predating the issue allowance default to 200
without rewriting immutable plan versions. Self-hosted databases have no quota.
`TestNativeArchiveLifecycle`, `TestNativeArchiveActiveWork`,
`TestHostedIssueAllowanceBoundaries`, `TestHostedIssueArchiveAndDowngrade`,
`TestHostedIssueConcurrentAllocation`, and `TestHostedIssueImportAllocation`
exercise these boundaries without adding a brake, lease, or recovery mechanism.

Hosted native mutation accounting (#201) requests only consumption metrics that
its existing mutation owner can grow. Ordered run events change attempt, history,
receipt and window accounting, not project, repository, issue or runner allocation.
Completion exemptions apply before selecting quota queries. Full explicit usage
reports and other hosted accounting callers retain their complete projection.
Exact retained-byte sums and history/event counts remain transactional where
enforced; no rolling usage window substitutes for stock totals. Quota rejection
rolls back data and business receipts, and idempotent replay remains uncharged.
This removes unrelated quota scans, not the retained-byte scan itself.

Active worker phase attribution retains the existing explicit validation lifecycle
owner and recognizes CI waits from leading, bounded wait statements. Ordinary
implementation words such as decisions or checking must not become CI waits or
trigger CI-outage cancellation. Typed merge/pushed-work handling and live
unstarted-check evidence remain unchanged. This removes a competing broad text
heuristic without a new classifier, reason, configuration, or control mechanism.
`TestCIUnavailableRepairImplementParity` covers phase and cancellation eligibility;
`TestParkCIUnavailableWaiters` preserves real wait cancellation while ordinary
implementation continues during the same outage.

Post-integration acceptance uses the existing Workpad/final handoff and permitted
Backlog follow-ups, rather than requiring a source worker to integrate or release
its own unmerged change. The current prompt and orphan-resume contract record the
pending criteria, exact source head, procedure, authorization, and existing owner.
Pending acceptance is never claimed as passed; explicit pre-merge runtime
requirements, human approvals, and project gates remain authoritative. Without a
permitted post-integration owner, the original requirement remains. This is an
instruction consolidation, not a new acceptance waiver, release owner, lane
writer, or recovery mechanism. `TestBuildPromptDocumentsWorkpadStatusContract`
and `TestRunnerRunCompletionLeaseOnOrphanResume` cover normal and resumed turns.

Repository notes handoff (#3498) is removed from normal, planning and merge
fallback prompts. Failed turns no longer append diagnostics to a repository
file. The canonical handoff contract and orphan restart nudge explicitly revoke
earlier notes instructions retained in provider history; normal completion
ownership remains unchanged. Existing issue Workpads, native completion contracts, attempt outcomes,
usage updates and provider/session records own handoff and diagnostics; no new
artifact or coordination mechanism is added. Native work item #180 retires this
repository's tracked historical `.detent/notes.md` and unused `internal/notes`
package, including its append writer and tests. Historical prose is not migrated
into current verdict authority. Workers continue to preserve existing user notes
and attachments; `.detent/lessons.md` and intentional project documentation remain
ordinary project files.
`TestPromptDoesNotUseRepositoryNotes`,
`TestBuildPromptUsesPriorAttemptWithoutRepoNotes` and
`TestRunnerFailureKeepsSessionDiagnosticsWithoutNotes` cover all prompt profiles,
prior-attempt findings, retained output and durable failed-session outcomes without
reading, creating or changing repository notes.

Workspace diagnostics never rewrite shared Git metadata, including human-authored
`info/exclude` (#3503). Diff statistics, fingerprints, patches, per-file diffs,
and recovery path evidence apply the existing runtime exclusions through
command-local pathspecs; tracked and untracked temporary artifacts stay excluded.
The session-local progress fixture also counts committed project knowledge and
excludes committed runtime scratch (#3547).
Human-authored `.detent/notes.md` and `.detent/lessons.md` remain ordinary project
files visible to diagnostics and recovery; removed automatic writers do not
justify suppressing intentional documentation changes. Existing operator ignore
rules remain untouched.
Workspace creation and worker scratch preparation no longer install repository
ignore rules. Diagnostic index copies live in worker-provided scratch and leave
the real worktree indexes intact.
`TestWorkspaceDiagnosticsPreserveSharedGitMetadata` runs diagnostics concurrently
in two linked worktrees and checks unchanged shared exclusions (contents, inode,
and modification time), unchanged real indexes, preserved human ignore behavior,
and worktree-specific source and human documentation changes. Its source and
worktree-root paths retain leading and embedded spaces on Windows, with trailing
spaces and embedded newlines additionally exercised on POSIX.
`TestPrepareWorkerScratchPreservesGitExclude`
covers scratch preparation without installing exclusions.

Workflow timeline reads consolidate identity matching through the existing
issue ID, project/identifier, and project/URL indexes. The indexed identity
union retains every matching durable event once, in timestamp/ID order, without
adding a cache, table, index, or history limit. This prevents repeated project
history scans during dispatch and lane observation.
`TestIssueWorkflowTimelineIndexedIdentityUnion` covers aliases, overlap,
complete history, ordering, and project isolation.

Workspace cleanup and retention completion checks share the existing optional
issue-ID probe owner without loading discussion or dependency evidence. The
retention callback keeps its 100-ID batches, closure/lane completion clocks,
unknown-clock retention, and existing transition-time fallback. Active ownership,
process checks, and the seven-day workspace lifetime remain unchanged. This
consolidates cleanup reads under INV-3 without a new reader or cleanup path.
Dispatch and reconciliation retain complete evidence reads. The ID probe is an
optional connector capability
because the existing state-list probe cannot select exact workspace identities;
connectors without it retain their current reader. Fresh scalar identity,
current lane, closure, and update time remain necessary before cleanup, while
existing association, merged PR, delivered head, and workspace usage checks own
safe deletion. No cache, recovery loop, configuration, or CI bypass is added.

## INV-4 — Native merge queue

Cached queue ownership belongs to its PR head; after provider inspection confirms a replacement head has no entry, discard old-head ownership so normal admission can enqueue the replacement.

**Statement:** Merges go through the repository's merge queue when one exists.

A queued PR with unresolved review threads must be withdrawn before the existing
review Rework handoff. Thread hydration follows the existing queue-entry refresh
cadence or a head change; unresolved threads already present in the snapshot
are refreshed before triggering the handoff on a cache hit. A card departing Merging attempts
withdrawal of its live provider entry, preserving the chosen destination and
transition reason even when withdrawal fails. Failures are logged and retain
ownership for existing pruning; they never prevent a lane write to Done.
Missing cards or PRs and landed or closed PRs release cached ownership without
a dequeue. A consumed provider entry is already withdrawn.
`TestNativeMergeQueueReviewReworkAfterEnqueue`,
`TestNativeMergeQueueWithdrawalDoesNotBlockDone`, and
`TestDelegateNativeMergeQueueIssuesCachesQueueEntries` cover withdrawal,
lane progress, and hydration request counts (#2826).

Queue admission distinguishes verification from enqueue eligibility: completed
success, skipped, and neutral checks may enter the native queue, but missing, running, cancelled, or
failed checks do not qualify through that path. The provider still enforces its
required contexts. `TestNativeMergeQueueSkippedChecks` and
`TestNativeMergeQueueNeutralChecks` cover this distinction;
`TestAttemptTriageSkippedChecks` ensures triage describes skipped checks as not
fully verified, even when the provider aggregate is green (#2948).

The command-gate promotion path shares this queue eligibility rule only after a
native queue inspection confirms availability for the same PR head. A pending
aggregate caused by completed skipped checks can then enter Merging; no queue,
missing or unfinished checks, failed checks, and a changed head stay ineligible.
This does not count skipped checks as passed tests. `TestAutoPromoteSkippedPRChecksOnlyWithNativeQueue`
and `TestCommandGateQueueEligibleCI` cover the handoff to the merge group.

**Why:** Competing speculative merge work and repeated head invalidations
contributed to the measured rebase and CI loop.

**Enforcement:** `TestDelegateNativeMergeQueueIssuesEnqueuesGreenTrainWithoutWorkerDispatch`
exercises native queue delegation. `TestRepositoryWorkflow` allows the
`merge_group` trigger only for pull requests into `main`. Other repositories retain
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

## INV-5 — Local pull-request validation and scheduled release evidence

Scheduled and manual full validation always inspect the pinned current
`develop` commit, including one carrying a release-provenance tag. Release
annotations are not proof that the scheduled suite passed and do not suppress
its run. The existing full-suite finalizer alone publishes scheduled success,
creates a validated tag and reports scheduled evidence; ordinary shipping
does not wait for that run. `TestRepositoryWorkflow` executes the preflight
with an emergency provenance annotation for both event types.

**Scope:** This is the Detent repository's development policy. Managed projects
choose their own workflow triggers, required checks, validation commands, and
release policies. Detent honors each project's configuration and branch rules;
PR CI, merge-group CI, and required status checks remain supported.

**Statement:** No workflow starts from `pull_request`, `pull_request_target`, or
`merge_group`, and no branch ruleset requires a status check. Pull requests do
not wait for CI or local validation gates before push or merge. The self-hosted
project uses the existing no-op command `true` and publishes no local status.
It explicitly sets `gate.required_status_checks: []`, so pending optional or absent
CI does not block progress. Reported failed CI still blocks, and native
base-branch requirements remain authoritative;
omitting the setting preserves aggregate CI behavior for other projects.
The existing branch-policy enrichment projects applicable running and unstarted
checks alongside CI status (#3477), so an optional queued housekeeping job
cannot hold implementation Rework after CI status already passes that policy.
Raw check observations and counts remain available for diagnosis; native
required pending checks, strict branch information, and reported failures remain
authoritative. `TestBranchPolicyProjectsApplicablePendingChecks` covers the
projection and preserves omitted and named policies without a scheduler bypass.
The operator also sets `gate.automated_review: "off"`; pending automated review
does not create a completion wait in that mode. Required and optional modes
retain their existing waits and timeout behavior; reported P1 findings still
route to Rework. The existing review-mode matrix covers this consolidation.

`make check-fast`, focused tests, and vet remain available for optional
diagnostics. When invoked, checks preserve failures. The local tools take no
shared validation lock and support concurrent worktrees: lint uses
`--allow-parallel-runners`, and each client test command uses at most two workers
while retaining file isolation and all tests. These tools do not become merge
requirements.

`TestMakeCheckFastOverlapsWorktrees` exercises the Make graph with controlled
tools in two linked worktrees while the legacy common-directory lock is held.
Both builds must start before either completes, and each writes its evidence
inside its own worktree. The pinned-linter tests also require the parallel-runner
flag (#3253).

GitHub Actions schedules the full suite hourly from the repository's default branch.
Preflight pins the current `develop` SHA for every scheduled or manual run,
independently of existing release tags. Every full-suite job runs on the pinned
commit. A green run posts `scheduled-full-ci` status, cuts an annotated patch version tag with
exact status evidence, and dispatches the release workflow. It does not merge
to `main` or deploy production. For this migrated repository a failing run
files or appends one native Backlog diagnostic per normalized fingerprint,
retaining pinned commit, run/attempt and job evidence. Unrecognized or unavailable
diagnostics remain CI-instance intake with conservative job-level identity.
A green run appends validation evidence through the same native comment owner;
it never closes work or invents a landed receipt. Other repositories retain
GitHub Todo source repairs, Backlog instance intake and green-result closure.
Manual dispatch can force `verify-fast` to fail to exercise diagnostic filing.
The finalizer retains publication payloads and original job results as artifacts
for retry and never falls back to GitHub issue writes on Cloud failure.

Every `develop` push deploys to staging even when that commit has not passed
scheduled validation. Production release artifacts use validated tags only.

Conversation output is generated by the existing build owners from their
selected source and lockfile, and is ignored in feature commits (#140).
`make check-app` retains source diagnostics and attribution without checking
committed-output freshness. Staging, scheduled fresh-checkout consumers,
GoReleaser releases and private operator builds prepare the embed inputs through
`make app`. GoReleaser publishes a prepared source archive with the complete
client and build identity for Go-only consumers. Raw module installation is
retired; version tags and signed provenance retain the validated source commit.
The focused conversation-build diagnostic merges independent source edits and
exercises embedded HTTP delivery and the prepared source build. It adds no
shipping gate, tracker lane writer, merge mechanism or project-wide CI policy.

The scheduled NilAway audit selects all Go packages, including unchanged
importers affected by a provider's inferred nilability. The real-analyzer
provider/importer fixture verifies that boundary and the reviewed baseline:
only diagnostics matching both location and source-line hash are accepted.

**Enforcement:** `TestRepositoryWorkflow` checks schedule, manual dispatch,
required full-suite jobs, pinned checkout, and finalizer. `TestRepositoryHasNoPullRequestActions`
checks every workflow for forbidden pull-request and merge-group events.
`TestWorkflowViolations` rejects trigger and coverage regressions.

The obsolete portability-stress configuration-text test is removed (#3547) per
the test-suite audit policy. It encoded the retired single-job layout instead of
executing stress behavior; manual stress suites retain their existing selection
and budgets. Scheduled workflow invariant checks remain in place.

**Change:** Update this invariant and its workflow assertions in the same pull
request when changing validation or release evidence.

## INV-6 — Isolated Codex home

Remote SSH lifecycles run the same provider isolation and private temporary GitHub credential setup on the selected host. The transport carries configuration over encrypted stdin and removes bootstrap credentials after teardown. `TestSSHLocalTargetIntegration` exercises remote hooks, gates, push, cleanup, and disconnect cancellation (#3239).

**Statement:** Workers run with an isolated Codex home; user-level instructions never reach a worker.

**Why:** Host-level instructions introduced competing worker prerequisites and
operator interventions unrelated to the assigned repository task.

**Enforcement:** `TestPrepareCodexCommandForServiceIsolatesInstructions` and
`TestPrepareWorkerCodexHomeExistingInstructions` exercise service profile
isolation, per-worker SQLite state, and rejection of inherited instructions.
Worker and launchd profiles own real `sessions` and `archived_sessions`
directories; sync replaces legacy symlinks without touching host transcripts.
Before verification or resume, only the requested persisted legacy thread is
copied into profile storage. Resumed writes leave the host rollout unchanged;
existing profile rollouts take precedence. `TestPrepareLegacyCodexRollout` and
`TestAppServerThreadPreparation` cover continuity across this migration.
History prefers profile transcripts and falls back to host history. Startup
retention removes profile rollouts older than 30 days, preserving unfinished
threads, their resume sources, and descendants of retained parents. It never
traverses shared transcript symlinks. The
manifest runs both.
Repository instructions and the Detent-provided worktree remain authoritative.

**Change:** Edit INV-6 and isolation tests together in the same PR before changing
home construction or instruction inheritance.

## INV-7 — Machine issue identity

**Statement:** Machine-filed issues carry an origin block and a fingerprint, and duplicates comment instead of creating another issue. Intake findings prefer an open issue when multiple issues match their durable marker; when all matches are closed, the newest issue (highest repository issue number) is already handled: they create no issue, comment, content update, or state change.

**Why:** Repeated repairs and machine discoveries otherwise create duplicate
work and obscure whether an issue came from an operator or automation.

**Enforcement:** `TestMachineIssueTool` exercises the worker tool and intake
contract through the manifest; `TestMachineIssueDuplicate`,
`TestMachineIssueSeparateConnectors`, and `TestMachineOriginSurvivesBodyUpdates`
exercise duplicate commenting, concurrent publishers, and durable origin stamping.
`TestManagerPreservesClosedFinding` and
`TestConnectorFindIntakeIssueSearchesDurableMarker` cover closed intake findings,
and `TestConnectorFindIntakeIssuePrefersOpenDuplicate` covers duplicate selection across search pages,
including completed and not-planned GitHub issues. Use `file_machine_issue`, with a stable problem key,
for worker discoveries. Review must ensure a fingerprint describes the problem
rather than a timestamp, attempt, or wording variation.

Scheduled validation (#3625, native #97) reuses `issueorigin.Fingerprint`, `Stamp`,
`Parse`, and `Occurrence` across the selected destination's paginated open issues
and native imported comments. Plain and JSON
Go failures identify the package-qualified test; failed subtests replace a
parent summary that has no independent assertion. Source diagnostics identify the repository-relative location
and message. Run, attempt, commit, and job evidence describe occurrences rather
than changing problem identity. Unknown evidence retains the existing job
fingerprint. The reporter remembers issues created in the same run, so coverage
and race failures attach to one repair without a new coordination mechanism.
`TestParseProblems` and `TestReport` in `tools/cifailure` exercise the recorded
missing `skip_reason=already_running` diagnostic, separate tests and diagnostics,
repeated runs, repository-wide matching, and conservative fallback.
Job-log fetches allow terminal escape sequences so colored output reaches the
existing ANSI-stripping parser (#3709). `TestReport` replays the recorded
colored-log refusal and verifies test identity and occurrence consolidation;
unreadable logs retain job identity and CI-instance attribution.
The existing source parser also recognizes recorded bracketed gosec findings
(native #37). Their repository-relative file, line, empty column and full finding
message form the source identity; the original finding remains occurrence evidence.
`TestParseProblems` and `TestCloudReport` cover the recorded format, ANSI removal,
duplicate locations, repeated jobs/runs and conservative tool-cache fallback.
The repository scheduled reporter creates unknown or unreadable job fallbacks
as Backlog intake, using the existing admission artifact contract (#3727).
Parsed test and source diagnostics remain Todo hotfix repairs in GitHub mode;
this migrated repository keeps all new diagnostics in native Backlog for existing
operator admission. Fallbacks retain
bounded log evidence and retrieval errors, their legacy fingerprints, and
repository-wide occurrence matching. Existing issues receive occurrences without
lane changes; this producer policy does not retroactively close or move an
instance-owned report. `TestReport` covers destination labels and runner shutdown
evidence alongside the existing fingerprint and repeated-run scenarios.

Native reporting discovers the existing scoped MCP tools and reads configured
workflow ownership before filing. Run/attempt/job/problem identities supply
stable command request IDs and occurrence markers across reconnects and retries.
Publication stops after a failed or ambiguous response; a retry reads durable
bodies/comments before another write. `TestCloudReport`, `TestCloudTransport`
and `TestCloudDestinationAuthority` cover imported provenance, response loss,
replay, held items, current connection failure and destination isolation.
For this repository's selected native reporting context, new parsed source/test
diagnostics enter Backlog at High. Qualifying occurrences, including replays of
existing imported evidence, promote unset or lower priority through `edit_item`
with the observed expected revision and a stable occurrence-based request ID.
High and Urgent remain unchanged. Conflicts stop publication through existing
command semantics, without a reporter retry loop. These same regressions cover
source priority, lost priority responses, replay identity, stale/missing
revisions and infrastructure-only intake; other projects retain their defaults.
Historical red evidence proves a pinned failure, not a current staging outage.
The finalizer fixture covers native green evidence without GitHub closure and
preserves scheduled tag/release publication. No capability parity changes or
additional intake, recovery or reconciliation owner are introduced (INV-3).

**Change:** Edit INV-7 and origin/deduplication scenarios in the same PR before
changing identity format or duplicate handling.

## INV-8 — No strict freshness protection

**Scope:** This is the Detent repository's branch protection policy. Managed
projects may require strict freshness; Detent honors their branch rules.

**Statement:** This repository does not require strict up-to-date branch protection on branches Detent merges into.

**Why:** Strict freshness invalidated already-tested heads after other merges
and fed the measured repeated rebase/CI loop.

**Enforcement:** Doctor reports live branch protection as project evidence,
without treating strict freshness as a failure. `TestDoctorRespectsProjectCIPolicy`
prevents this repository's CI and protection policy from becoming global advice.
Repository tests cannot guarantee live branch settings. The operator must inspect
this repository's merged-into branches; local CI does not enforce live settings.

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

Native claim and provider preview carry the project's existing dispatch state,
label and unblocker policy to the shared `dispatchpriority` comparator before
acquiring a lease. Merging overrides numeric priority only when that project
configures Merging first; native queue rank remains a tie-breaker after the
configured priorities. Both paths retain native scope, dependencies, live
leases, current reviewed Change Request readiness and policy/runner authority.
Runner home selection derives eligibility from each home project's dispatchable
workflow and approved runner policy, independently of dispatch ranking. The
requested project's state filter and comparator apply within that project;
partial orders do not hide unlisted eligible states or impose the requesting
project's ordering on another home project. Home grants and spillover rules
remain authoritative. The existing native capability negotiation requires a
Hub advertising `dispatch_priority` before a runner sends ranking fields to
claim or preview. Older Hubs produce an instance scheduling wait before either
request, preserving their strict request schema and issue failure budgets.
Native admission fills a bounded batch during the existing refresh. The batch
shares one free-slot plus eight-candidate evaluation allowance, and claims at
most the available project slots (or one existing merge-control candidate when
full), bounded by machine capacity. Each claim uses a distinct session from the
existing session owner; atomic Hub claims account for earlier leases against
machine, shared-host and provider capacity. Selected candidates update only a
planner copy so subsequent readiness checks retain project, stage, worker and
model limits. Preview ordering, filters, fairness, unblocker priority, immutable
policy identity and lease fencing retain their existing owners. Batch hydration
or claim errors release acquired leases through the existing native release
owner; dispatch releases candidates it cannot select. No additional refresh,
reservation, recovery path or configuration is introduced (#153).
Normal typed provider, runner and host capacity refusals terminate both empty
and partial batches successfully (#183). They acquire no rejected work and
leave candidate refresh health and configured cadence with their existing
owners, without scheduling-unavailable failure streaks or exponential delay.
Due provider retries omitted from these batches retain their exact attempt and
continuation through planning and final tick cleanup (#190; INV-2/INV-3).
Authorized current status and lane-transition observations dispose genuinely
invalid retries; admission absence supplies no such authority. Complete tracker
fetches retain their existing missing-item semantics. Capacity reopening resumes
through normal claim adoption and configured priority, so eligible urgent retries
remain ahead of ordinary new work. `TestHubSchedulingPreservesOverloadRetryAcrossAdmissions`
asserts those boundaries alongside the existing admission and Merging-slot tests.
Already-lost local retries require no queue reconstruction: normal native claim
hydration supplies recovery authority to the runner. Its exact prior local
attempt selects the established provider session and runtime before current
default selection, preserving policy and checkpoint verification. Completed
PR-scoped automatic resume retains its existing eligibility rules. Failed
session lookup cannot cross project, issue, attempt, backend, model or role.
Policy, authorization, operator draining and protocol failures retain their
error and lease-release owners. Provider quota/reset waits remain authoritative.
The existing preview evaluates local readiness before claim within that shared
allowance. The planner's existing known-wait classification
excludes known local waits from expensive evaluation. Unavailable local or
provider candidates fall through without a lease or capacity hold. Claim rechecks current revisions and
provider capacity, and dispatch retains its fresh checks. This consolidates
selection under INV-3 without a new queue, configuration key, reservation,
preemption or recovery mechanism. Landing protections and refusal evidence
remain owned by the existing landing path.
During a recorded active GitHub REST wait, the existing candidate-state owner
excludes native GitHub PR landing before the bounded native claim boundary.
Native coding and Git-only landing remain eligible, with the same dependency
decision used by planner, retry and final dispatch admission, including due
retries with historical REST capacity scopes (#150). Local merge slot
exclusion remains in force. If filtering holds all states, intake returns no
candidates without a claim; an empty Hub state filter means unrestricted work.
Expired waits restore eligibility without a fresh GitHub probe. Other projects
retain their tracker, CI and REST decisions and approved landing requirements.
`TestProviderQueueOrderAndSelectors` covers native claim/preview ordering and
unavailable-head fallthrough; `TestProviderSchedulerEndToEnd` covers pre-lease
local readiness, provider fallback, mixed-provider batches, stage capacity and
provider release on hydration failure. `TestNativeAdmissionBatch` and
`TestNativeAdmissionCompetingRunners` cover actual bounded admission, shared
lookahead, hydration release and competing claims. The batch case in
`TestNativeExecutionLandsReviewedVersion` records coding and merge starts from
one native fetch while retaining reviewed-version landing authority.
`TestRunnerHomeClaims`, `TestHubSchedulingCycle`,
`TestHubSchedulingReadinessBeforeClaim` and
`TestNativeOptionalReportsNegotiateHubSupport` cover home policy isolation,
recorded REST-wait fallthrough, empty-state intake and mixed-version negotiation.

Project dispatch evaluates candidates in the existing priority order, with at
most the initial free project slots plus eight candidates of lookahead per pass
(#3190). Failed hydration, dependency waits, due retries, and rejected dispatches
consume that same evaluation allowance; they cannot trigger a full-queue tracker
scan. Existing native dependency waits, running or claimed work, parked work
from non-dependency owners, deferred completions, and future retries are
ordered behind candidates without known waits before bounded hydration,
preserving priority within each group after merge ordering (#3570).
Partitioning reads existing in-memory state; it does not call eligibility
callbacks or replace the fresh dispatch decision. Dependency-derived Blocked
entries retain their source provenance: only native dependency snapshots may
change their ordering. Cached CI, artifact and completed-gate evidence retains
fresh hydration and operator-rejection evaluation before refusal.
`TestDispatchPlannerFindsReadyTailBeyondKnownWaits` covers each owner and a
mixed front of unavailable candidates with six ready slots.
`TestDispatchPlannerDependencyWaitProvenanceAcrossTicks` covers list-derived
holds and fresh dependency clearance over consecutive scheduler passes.
Due retries retain their polling order, and every admitted candidate still uses
fresh dispatch hydration. Local label rejections and project-capacity skips need
no evaluation.
The existing merge-control path remains available when project slots are full.
This bounds readiness discovery; it does not reserve or hold global capacity,
and discovered ready work still uses the existing global acquisition lifecycle.

**Statement:** Priority picks the next job. Nothing else.

- If a task can be started, start one. Always. Free capacity is never held,
  reserved, or kept idle for any project, lane, or priority.
- When a slot frees, dispatch the highest-ranked READY request; if none of the
  higher-ranked projects has anything ready, dispatch whatever is ready.
- The system never cancels, stops, or preempts running work. Only a user cancels work.

Provider capacity application (native #90) removes active reservation snapshots as
a second ceiling when a fresh report covers the same provider, backend, account,
sharing identity and reserved model. Reservations still count as occupied slots
and keep their original execution identity and history. Without matching fresh
evidence their existing conservative bound remains; shared reporter ceilings and
exhaustion still apply. `TestRunnerCapacityApplication` raises concurrency with two
active reservations intact, refuses a claim while the external producer remains at
two, and admits six only after fresh authoritative evidence. This consolidates
configuration ceiling authority under the current report owner (INV-3); it does
not change expired report handling, introduce recovery, or alter priority.

Hosted organization subscriptions price project and unarchived-issue capacity,
never seats or concurrent agent work (#3268). The Hub removes plan membership
and concurrent-work admission limits while preserving independent runner/host
capacity and provider safety limits. Free refuses new AI turns at the existing
execution feature boundary, including Luna coordinator chat and native claims;
downgrades preserve safe completion. `TestCapacityCatalog`,
`TestCoordinatorFreeRefusesLuna`, `TestCapacityPaidPlanChanges`, and
`TestHostedConcurrentClaimsDowngradeRelease` cover this boundary. No new scheduler
mechanism, reason code, reservation, or recovery loop is introduced.

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
Project run completion, lease release, and increased project capacity read fresh
candidates on the orchestrator event loop. The pass uses the tick's eligibility
planner before claiming, so stale snapshot membership cannot keep a newly ready
candidate from a free project slot. The unused previous-candidate snapshot and
membership restriction are removed (INV-3). After stop exclusions, the existing
durable retry-intent owner restores first-seen recovery metadata before dispatch,
including resume state and recovery attempt identity. Its early tick call remains
because tick reconciliation needs that intent before evaluating terminal attempts.
Refill adds the owner's local timeline read per retained candidate; further
attempt and receipt reads occur only when an existing intent is found. Existing
promotion, human, dependency, claim, budget, gate and stop owners remain in force.
The same event-loop owner handles the bounded cohort of already queued worker
completions before one fresh candidate refill. Each result retains its generation,
completion fence and stop handling; all completed operator stops remain excluded
from that refill. New arrivals wait for the next event-loop pass.
All modes use the same lifecycle; strict priority is
only an ordering rule. Authorization, dependencies, retry readiness, and local
lane ceilings are checked by the callers before acquisition. Admission reads
and filters candidates before acquiring local or global capacity for evaluation.
`TestGlobalDispatchGatePriorityOnlyPicksNextJob`,
`TestGlobalDispatchGateReadyRequests`, `TestGlobalDispatchGateConcurrentReadyRequests`,
`TestQueuedDispatchRanksIndependentRequests`, `TestQueuedDispatchPreservesProjectCeilings`,
`TestRunDispatchesQueuedRequestsWithoutPolling`,
`TestRunDispatchesQueuedRequestsAcrossHostsWithoutPolling`,
`TestCompletionRefillsProjectSlotWithoutRefresh`, `TestCapacityIncreaseRefillsWithoutRefresh`,
`TestEventAndTickDispatchEligibilityParity`, `TestQueuedDispatchUsesCurrentWorkpad`,
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

Merge scheduling admits ready workers in the same repository up to the configured
Merging state capacity (#3023). CI waits, retries, claims without running work,
and unready heads do not exclude ready competitors. Aged heads retain ordering
preference when ready. `TestMergeDispatchUsesMergingCapacity` checks fresh and
retry dispatch with limits of one and three; `TestMergeIdleHeadDoesNotReserveSlot`
checks planner dispatch, fresh candidates, and native queue admission.
The regular Merging agent prompt now hands a pushed head back immediately, even
when the project workflow asks the agent to watch CI. The existing current-head
CI retry releases the worker and claim; a green head re-enters the same fairness
order. `TestPushedMergeHeadRequeuesWithoutHoldingSlot` covers post-push admission
of a same-repository peer and the older issue's return after CI, while
`TestMergePromptHandsOffPushedHeadBeforeCI` covers the instruction boundary (#3045).
With explicit `required_status_checks: []`, a freshly hydrated green head with
no check runs or status contexts and a known base branch proceeds through the
existing merge path after a push. The native branch-policy hydration owns CI
eligibility; omitted configuration, observed producers, pending native checks,
missing native checks, and failures retain their existing post-push wait.
`TestMergingPushedHeadRespectsBranchPolicy` covers this removal of the unnecessary
CI retry without making absent CI generally passable.
The `merge_ci_reservation` dispatch reason is retired. Native merge queue admission
still defers enqueueing behind a running merge in its repository, without holding
a worker slot. `merge_fairness_head_reserved` remains retired from producers and
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
normal eligibility. Closed or merged PRs do not retain this CI wait; their
historical pending statuses leave replacement work or merged reconciliation to
the existing owner. Unknown PR state remains conservative, and open pending
checks retain the wait. `TestReworkCurrentHeadCIDispatch` covers fresh and retry
candidates, immediate dispatch of the next eligible candidate, and release after
terminal CI. `TestReworkCurrentHeadCIConfiguredLane` preserves configured lane
selection. This consolidates CI classification with the merge worker (INV-3).

Candidate refreshes retain unresolved blocker refs after retryable lookup failures
(#2836). Dispatch treats an empty blocker state as waiting, consistent with ranking,
while unrelated candidates remain eligible. This consolidates the dependency check
and removes whole-refresh failure for transient individual state lookups; native
relation failures, cancellation, and permanent lookup errors still fail the read.
`TestDispatchReadyIssuesUnresolvedDependencyDoesNotBlockUnrelated` covers the mixed
candidate dispatch behavior.

**Change:** Edit INV-10 and its tests in the same PR before changing priority or
capacity semantics. New names or indirect equivalents remain a review boundary.

## INV-11 — Human mechanism scope approval before Todo

**Statement:** No new or expanded operational mechanism enters Todo without a
human's explicit approval of that scope, regardless of who filed the issue or
how its title is typed. Only a mechanism fix with recorded runtime evidence
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

On October 1, 2026, the operator removed the feature scope gate. Features that
do not add or expand mechanisms may be filed straight to Todo. Human scope
approval remains required for new or expanded mechanisms, and the rest of the
mechanism moratorium remains in effect. The operator also updated Admission
Criteria rule 9 in the out-of-repo `detent-orchestration/WORKFLOW.md` to gate only
new or expanded mechanisms. This invariant and its doctor enforcement record
the in-repo half of that decision.

**Enforcement:** Admission Criteria rule 9 leaves new or expanded mechanisms in
Backlog until a human approves their scope by moving them. Assistants file that
work to Backlog only. The rule applies regardless of title type or author; only
evidenced mechanism fixes that remove or consolidate qualify for automatic
admission without human scope approval.

The doctor check `INV-11 human scope approval` inspects every applied Todo
ledger entry in the last seven days, including repeat entries. It fetches current
issue titles and bodies by ID in batches of at most 100 so GitHub's identity
lookup can audit busy projects. Declarations in either titles or bodies adding
or expanding config keys, reason codes, brakes, breakers, leases, parks, recovery
paths, or reservations require a human move. Conventional `feat`, `perf`, and
`refactor` title prefixes alone do not require scope approval. Each
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
It does not prove that every mechanism fix contains sufficient runtime evidence;
admission rule 9 and review enforce that requirement.

**Change:** Editing INV-11 requires updating the doctor check and the admission
criteria in the same PR. Identify INV-11 in the PR template and record the
operator-managed workflow update when it lives outside the repository.

## INV-12 — Native toolchain caches

SSH workers keep the remote host’s native toolchain caches and shared Go admission budget. Worker scratch uses only the host-provided TMPDIR/TMP/TEMP, and never points at the orchestrator’s scratch or caches. `TestSSHProbeRequiresProvidedScratch` and `TestSSHLocalTargetIntegration` exercise these boundaries (#3239).

Detent never substitutes its own cache or state location for a toolchain's native
one; bounding uses the toolchain's own mechanism. Workers inherit the host
toolchain caches. Detent may house-keep a native cache (age or size trim that the
toolchain tolerates) but never relocates it, never keys it per project or per
attempt, and never introduces a configuration key that does either. Per-attempt
isolation is limited to `TMPDIR`/`TMP`/`TEMP`.

Sandbox isolation (#3168) grants normal backend turns access to the existing
native Go build and module caches without relocating them. Restricted Codex turns
keep cache writes and network access disabled. Claude resolves its subprocess
temporary directory inside the existing worker scratch directory.
`TestBackendAppliesRunnerIsolation` covers normal and restricted cache grants.

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
Doctor reports native Go cache paths and readable sizes once per host, the last completed
reaper trim (or “not recorded”), and warns about legacy `.detent/cache` roots in
the workspace root or workdirs, including former per-attempt cache components
under `.detent/worker-tmp` and under each workdir's worker scratch root in the
OS temp directory (`detent-worker-scratch/`), where attempt scratch lives so
toolchain churn stays out of file-watched workspace trees. Reaper timing is recorded in `detent-trim.txt`
inside the native build cache; Go's own `trim.txt` is left untouched.
The native build cache defaults to 10% of total cache-volume capacity with the
existing 48-hour age trim; explicit bounds win, and unavailable capacity falls
back to 20 GiB with a doctor diagnostic. Omitted size bounds remain omitted
through config writes and are normalized only for runtime use. Doctor warns when the effective bound
exceeds 10% of total volume capacity or the last sweep evicted any bytes for size.
The existing trim marker also records age-expired bytes, size-evicted bytes, and
retained size; timestamp-only markers remain readable. Legacy-root checks remain
per project. The existing reaper reuses its trim walk to publish retained build-cache bytes and both removal counters plus last-sweep retained size in
`host_cache` in `/api/v1/state` as the sole cache surface (#2742). It does not rescan
the build cache or traverse the module cache; unmeasured module fields are omitted.

## INV-13 — A board card is a title and one status line

A board card renders exactly: the identity row (project, issue and PR references,
origin, model and configured effort; Compact may hide effort before model), the title, at most one status line, and the existing priority controls.
The status line is at most 48 Unicode characters and names the wait in words a
human acts on, for example "Waiting on #2129", "CI running", "Needs you",
"Blocked · 1", "Running". Scheduler evidence, tracker snapshot ages, timestamps,
token counts, attempt counts, fact grids, and diagnostic text are not card
content; they live in the detail sheet and hover titles. A change that adds a body
element to the card, or lengthens the status line, must change this invariant in
the same PR.

`TestINV13BoardCardContent` and `TestINV13SheetObservations`, registered in the
invariant manifest, enforce the content, character budget, and detail preservation. Playwright
checks one-line status layout in compact, cozy, and comfy densities, and verifies
that effort is visible at Cozy/Comfy and may be hidden before model at Compact.

## INV-14 — Workers never wait on a human question

Tracker human attention is authoritative only through the current canonical
Workpad; final prose and final-only status cannot replace it (#3758). Native
workers are explicitly forbidden to write Workpad comments and report blockers
in their final outcome. The existing native completion owner uses that contract
to settle human attention in Blocked, even without a produced change, rather
than finishing Done, entering Merging or automatically repeating the decision.
It retains existing human-owned recovery; final prose cannot fabricate a landed
version or replace a typed refusal. Canonical human actions, authorized
clearance, explicit operator pins, repository protections, configured human
review and actual backend approval declines keep their existing owners. Forge
authorization and infrastructure failures remain instance-owned.

Workers receive no `ask_human_question` tool in any project. Dispatch and
completion do not consult `human_questions` rows, including unanswered rows
left by older versions. Detent no longer writes those rows, migrates generated
questions into them, or projects them as active waits. Historical rows remain
readable as receipts.

A real product decision or physical action is recorded in the existing
structured Workpad `detent-status` block with `status: blocked` and a concrete
`human_action`. The card moves to Blocked and displays "Needs you". Instance and
infrastructure failures remain instance-owned. An authorized newer Workpad can
clear the action after it is performed; existing recorded-blocker recovery
moves the card back to executable work. Ordinary replies cannot clear it.
Legacy generated question prerequisites are converted by recording the action
on each dependent before removing the old dependency; the generated source
remains historical evidence.

`TestINV14DispatchQuestionTool` checks worker requests across automated,
human-review, and disabled auto-promotion configurations.
`TestINV14LegacyQuestionRowDispatch` checks dispatch with published and
unconfirmed unanswered legacy rows. `TestLegacyHumanQuestionReceiptsReadable`
checks historical access. `TestFirstHumanBlockerCompletionReachesBlocked`,
`TestWorkpadHumanActionSnapshot`, `TestOperationsWorkpadHumanAction`, and
`TestBoardCardSignalBudget` enforce the visible Blocked path.
`TestWorkpadHumanActionClearanceRecoversBlockedIssue` checks authorized
clearance, retained legacy dependencies, and refusal of stale updates.

## INV-15 — Visible UI changes require a human-authored issue

- Scope: every user-facing surface: the Cloud app (`web/conversation`), Hub
  server-rendered pages, and the local Templ dashboard.
- A change may add a visible element (row, line, banner, badge, chip, column,
  panel, page, tooltip text, status copy) only when a human-authored issue names
  that UI change. Machine-filed issues, agent-expanded scope, and "while I was
  here" additions never qualify, even when the issue itself is legitimate.
- Removing UI, and fixing an existing element in place without adding visible
  content, do not need that approval.
- Diagnostics, coverage, provenance and debugging data for agents go through
  the existing API and MCP reads and logs. They are never added to the UI as a
  way to make them observable, including the Diagnostics page, toggles, or
  debug flags.
- Agents that believe a UI change is needed describe it in their outcome or
  file a Backlog issue for a human to author or rewrite; they do not build it.

**Enforcement:** This document, [AGENTS.md](../AGENTS.md#implementation),
[CLAUDE.md](../CLAUDE.md#workflow), and the visible-UI line in the
[PR template](../.github/PULL_REQUEST_TEMPLATE.md) carry the rule. Enforcement
adds no gate, check, configuration key, reason code, or dashboard surface.

## Check boundaries

The source walk covers non-test Go packages under `internal`, `cmd`, and `tools`
for the build platform running the gate. CI's Linux, macOS, and Windows test
jobs cover their selected files. It deliberately leaves docs and negative test
fixtures available to explain violations. Source vocabulary and workflow checks
complement behavioral tests; they do not prove every natural-language rule,
protect themselves against edits, or enforce live settings. See the
[runner contract](../invariants/README.md) for the same limitations.

The unused draft-ready connector capability is removed (#2929). Draft promotion
checks retain read-only draft handling; workers own marking their PR ready.
