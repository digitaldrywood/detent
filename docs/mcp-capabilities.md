# Dashboard capability inventory

The [machine-readable matrix](../internal/operatortool/capability/matrix.json)
and its [readable view](mcp-capability-matrix.md) are the implementation checklist
for native #26 (imported #3259). #3335 inventories the surface; it does not
deliver full MCP parity.
The fixture is planning and review evidence, not a runtime authorization policy,
tool catalog, deployment feature flag, or tracker writer. The original five read
tool names and schemas in `internal/operatortool/catalog.go` remain unchanged;
the current advertised names, toolsets and schemas come from the existing
`operatortool.Registry` and deployment executors.

Native #239 reconciles current source decisions, including conversation subjects,
work search and attempt cursors, selected-workspace events, current key/routing/
composer forms and Cloud issue attachments. Original imported child owners in
the fixture retain their provenance. Native #234 delivered credit checkout and
automatic funding adapters; native #235 delivered invitation grants/edit/resend.
Their current implemented decisions retain the actual approval, retry and
deployment restrictions. Those children do not depend on this inventory repair
or on their acceptance parent. The parent completes typed conversation subject
create/list and native work-list filters and projections through the same
application owners, with execution coverage over stdio and HTTP. The source
inventory check now requires zero pending operator rows on every invocation.

Cloud issue `upload_attachment` is implemented only through shared Cloud entry:
the tenant checks current operator write authority, then entry stores bytes and
returns the genuine attachment receipt. Its schema has no `request_id` and its
idempotent hint is false; retrying may allocate another attachment. Direct Hub
markers do not establish storage success. Native #238 supplies metadata, bounded
byte reads, deletion and explicit item/comment references. Existing content
saves bind attachment references in the application transaction; conversation
attachments are a separate resource with their own tools and authority.
The public entry routes have organization-prefixed and canonical API aliases.
Signed tenant metadata calls retain native scope and project authority;
service-only retention, existence and object-deletion callbacks remain explicit
non-model boundaries.

Full parent acceptance still requires retry-safe Cloud uploads. The existing
`upload_attachment` path accepts no business retry key; repeating an identical
call allocates a distinct attachment ID and stores another object. The shared
entry upload/application owner must supply retry identity and a durable storage
receipt before this operation satisfies the parent's mutation-retry contract.
The Hub's `entry_upload` authorization marker is not a storage outcome. Zero
pending inventory rows establish capability coverage, not safe-retry acceptance.

Each stable operation ID has exact source-site decisions, deployment and tracker
availability, current role/scope/grant/ownership restrictions, preconditions,
argument-dependent confirmation classes, the application read/command and
required extraction, a bounded typed tool proposal, child owner, implementation
status and coverage evidence. Proposed annotations conservatively describe the
whole operation, including risky argument variants. Reads are retry-safe; mutations
remain non-idempotent hints until their shared retry contract is implemented. A route registration is a source site, not a
capability count. Browser/JSON aliases and frontend calls can share a row.
`pending` means required operator parity remains unfinished. It is never an
accepted exclusion. `implemented` describes delivered child behavior with current
connection authority, not parent acceptance. `excluded` is reserved for
explicit current asset, local presentation, authentication exchange, worker,
transport or staff authority sites. Staff decisions do not give organization
operators staff privileges. Each child records the availability of its delivered delete/revoke actions.

Deployment availability distinguishes the self-hosted dashboard and unhosted
hub, hosted dedicated organization hub, shared entry plus organization hub,
and the private credential-maintenance listener. Hosted application routes
require hosted configuration. The shared entry proxies organization-bound MCP
requests through the same authenticated assertion boundary as application API
and browser traffic. GitHub/native availability
is recorded per operation; native-only policy/workspace/changes services cannot
be assumed present in a GitHub-only daemon. Every proposed tool must return a
safe opaque unavailable result when its application service is absent; this
inventory does not install services or change current authority. The current
read executors already handle missing telemetry/explanation dependencies.

## Cloud issue attachments (native #177 and #238)

Shared entry plus the tenant Hub supports `upload_attachment`,
`read_attachment_metadata`, `read_attachment`, `reference_attachment` and
`delete_attachment` in the `work` toolset. Reads return authenticated metadata
and at most 32 KiB of base64 content per call, within the 256 KiB result bound.
The existing 60,000-character base64 upload bound remains unchanged. Item/comment
binding reuses the same-project reference owner. Destructive deletion uses the
existing exact browser action approval or human-selected connection YOLO; its
receipt distinguishes pending and confirmed object deletion. All calls resolve
current project grants and credential scope. Read-only credentials discover only
reads; local and dedicated deployments omit these shared-storage operations.
Internal expiry, existence and deletion-confirmation callbacks require signed
service authority and expose no model-callable tool. See
[Cloud attachments](cloud-attachments.md) for the transport and deletion contract.
These delivered adapters do not claim completion of the parent parity inventory.

## Work and board reads (#3340)

The original five tools retain their names and schemas. Additional reads use
`project_id`, plus an item `reference` where needed. Native IDs and scoped
numbers resolve through the existing application identity rules. Organization,
principal, grants and credential scope come from the connection on every call.

| Tools | Application result |
| --- | --- |
| `work_list`, `work_item`, `work_config` | Search/list/detail; configured lanes, priorities and labels |
| `work_comments`, `work_history`, `work_version` | Current comments with edit metadata, durable history, native issue/comment revisions |
| `work_relationships` | Visible dependencies and source references |
| `work_runs`, `work_references` | Attempts and PR/change/version/artifact identifiers and links |
| `board_activity`, `board_receipt` | Activity with durable/snapshot provenance; efficiency receipt |
| `board_session`, `board_session_history`, `work_attempt_receipt` | Session context, persisted rollout pages, owned attempt receipt |
| `work_export` | Native application issue JSON without browser form credentials |

Pages accept `limit` from 1 to 200 (default 100). Native tracker pages use their
credential/resource/filter-bound `cursor`; snapshot-backed GitHub lists and
application history use `offset` and return `next_offset`. Supplying the other
pagination mode returns invalid arguments. Empty pages contain `items: []`.
Every envelope identifies the project/resource and source freshness/time;
expired snapshots remain explicitly expired. Freshly fetched comments carry
their own read time. Artifact references include availability and expiration;
PR references retain the projection observation time. Results over 256 KiB
return a safe unavailable error rather than an unbounded payload.

`work_list` accepts singular `state`/`label` selectors and OR lists `states` and
`labels`. Native projects also accept `assignee`/`assignees`, integer
`priority`/`priorities` (0–3), `archived` (`"true"`, `"false"`, `"all"`), and
`include` (`["summary"]`, `["workspace"]`, `["work"]`, or workspace combined
with summary or work). Arrays are bounded at 32 selectors per dimension.
Native cursors bind all filters. The default is the compact summary projection;
`work` adds scoped lane totals and bounded compact operational items, while
`workspace` includes reserved workspace dispatch items. Detail bodies remain in
`work_item`. GitHub snapshots support state/label/query and offset pagination;
native-only selectors return invalid arguments for those snapshots.

`create_conversation.input.subject_work_item_id` creates a private issue Q&A
conversation through the shared command. `list_project_conversations` accepts
the same subject selector and preserves the creator-only audience. Subjects
must belong to the selected organization and project; these conversations do
not dispatch a worker, link to a work item, or publish an issue comment.

`work_history` is separate from `recent_activity`: GitHub's dashboard history
uses persisted workflow records when its connector has no durable event reader,
and identifies that source as `workflow`; native history uses collaboration
events. GitHub comment edits expose current body and update metadata; saved
revision reads and native exports require a native service. Hosted hubs without
daemon receipt/session services omit those tools from discovery and direct calls
return opaque unavailable errors. #3347 owns PR/review/change/diff/artifact
content details and reuses the extracted application reads; this child supplies
references through the shared detail owners.

## Managed local project configuration (native #94)

The `local_projects` toolset uses the selected installed local configuration
owner. `local_project_configuration` reads its actual global configuration
revision, effective policy and selected committed workflow policy. Provenance
contains identities and revisions, without file paths, workflow instructions,
credentials or private configuration values. A Cloud project, runner source
association and local board registration remain separate authorities.

`apply_local_project_policy`, `drain_local_project` and `detach_local_project`
require current admin scope for the exact project, existing operator approval,
`request_id`, `expected_config_revision` and `expected_policy_id`. They reuse
durable operator command receipts; retries reauthorize and return the original
receipt without repeating the effect. Policy application additionally requires
the exact approved `policy_id` and `source_revision` from the configured committed
workflow, a paused project and settled work. Local workflow overlays are refused.

Drain uses the existing project orchestrator to finish current work and reject
new dispatch. Local intake, routines, backlog admission and retro schedules must
already be migrated or disabled through the workflow owner. Detach requires a
paused or draining project, no active or deferred completions, and the exact
`checkpoint` from an authenticated cutover receipt for its mapped native Cloud
project and repository. It removes only that project's global registration
through the existing validating writer; unrelated values, pauses, instance
roles and native lease/publication authority retain their owners.

Receipts distinguish `saved` configuration from `applied` runtime policy/drain.
A saved removal returns `registered: false` while `runtime_registered: true`
until the existing reload removes the runtime project. Read again to verify
both registrations are absent. A missing/stopped owner explicitly requires a
supported installed configuration service; hosted Cloud calls cannot edit that
file or start the stopped board. Stale revisions, unsupported sources and
incomplete handoff return redacted constraints, never fabricated completion.

## Current connection authority

`operatortool.Identity` binds principal, organization, credential and session;
it contains no permissions. `Connection.Resolve` resolves current application
authority for every discovery/call. `AuthorizeCurrent` checks identity equality
and organization binding before delegating scope, project and resource ownership
to the application adapter. Approval/audit children must invoke it again with
the current authenticated connection when an approved action executes. Never
persist a resolved authority or derive permission from discovery, annotations,
approval or YOLO. Unknown resource kinds deny until their shared application
ownership check is implemented. Tool arguments cannot select credentials,
principals or organization authority.

Self-hosted `/mcp` requires an existing credential with read authority; write
and admin credentials retain their existing hierarchy. The dashboard now exposes
the four shared chat application commands where available (#3337). Scoped keys are supported and are authenticated again at execution.
An optional `X-Detent-Organization` header must match `self-hosted`. Legacy HTTP sessions
bind the full identity tuple; each HTTP request carries its own authority through
dispatch. The stdio client delegates both discovery and calls to the daemon's
application bridge, including legacy daemon-authorized loopback reads. It never
opens SQLite or creates an administrator identity. A loopback read bridge
connection has read authority only. Read-only private dashboard sessions retain
that limit and their existing token validity.

Project-scoped reads reuse the dashboard project projection, combine only
authorized counts and tokens, and omit global events, rate limits, host outages,
budgets and lifetime totals that have no project attribution. Structured blocker
references require an identity in the authorized snapshot. Explanation reads use
the same projection. Omitted global fields retain the old result schema's empty
values; per-project throughput remains in `projects`, while unscoped throughput
has no safe aggregation and remains empty for restricted credentials.

Unhosted hubs use `/api/v2/organizations/:organization/mcp`; hosted dedicated
hubs also mount `/mcp`, and shared entry exposes
`/organizations/:organization/mcp`. The hub adapter reuses existing token,
organization, provider/session/membership and project grant checks. Both hosted
paths take the lesser provider/assertion and local role. Shared entry still
validates audience, path/body, allocation generation, browser binding and access
expiry; the tenant rechecks the stored session, active membership and grants.
Hosted machine-token restrictions and staff/support boundaries remain those of
the application; worker and runner credentials never gain operator access.
CSRF is still required for browser POSTs. The credential-maintenance listener
does not mount MCP. Hubs currently have no daemon telemetry/explainer service:
the five telemetry/explanation reads are unavailable and direct legacy read calls return
opaque unavailable errors after resource authorization. The read/service children
must supply shared application reads, not a compatibility HTTP proxy.

Organization and credential administration (#3344) uses typed application tools:
`organization_session`, `organization_list`, `organization_switch`, `session_logout`,
`organization_create`, `organization_delete`, `invitation_accept`,
`invitation_send`, `invitation_edit`, `invitation_resend`, `invitation_revoke`, `membership_list`, `member_remove`,
`member_role`, `member_grant`, `credential_list`, `credential_create`,
`credential_rotate`, `credential_revoke`, `credential_grant`, and `support_start`.
Discovery filters these by installed services and current authority; direct calls
perform the same checks. The account entry is `/api/cloud/mcp`. Context selection
returns a destination requiring a fresh connection; it never changes the original
connection's grants. Organization lists include owned provisioning status where
that application offers it. Support entry returns the existing interactive browser
flow only to configured support actors.

Self-hosted dashboard API-key commands use its existing browser approver. Hosted
membership/account commands use the originating browser session and its existing
CSRF boundary. Native hubs expose credential metadata and context selection;
material native administration is unavailable through MCP because those deployments
have no browser approver. Their existing API commands remain available. Providers
without invitation-by-ID administration do not advertise invitation acceptance.
Absent services return opaque unavailable errors. No new approval credentials or
platform powers are installed.

Native #235 adds explicit bounded project grants to invitation sending and exposes
pending-invitation grant replacement and resend through their existing hosted
commands. Edit requires a complete grants list; an empty list removes project
access. Edit/resend discovery requires provider invitation-by-ID administration,
and resend also requires delivery support. Current organization, role, pending
status, expiry and grant authority are rechecked at approval and execution.
Selected bearer keys cannot grant or remove access beyond their own project scope.
Exact browser previews include the invitation and grants; durable command receipts
prevent repeated provider delivery. Only pending actions advertise approval URLs.
Dedicated/shared and stdio/HTTP fixture coverage uses no live mail. Parent #26
retains strict final conformance and unconditional zero-pending acceptance.

Access-changing and destructive commands require an exact preview and a real
browser decision by default. YOLO is a human connection decision and still checks
current authority. Durable application receipts omit credential material; deliberate
credential results stay on their originating connection, recheck current authority
and the delivered credential's validity, and are never restored from durable retry
receipts. A fresh connection retry returns the resource receipt without its secret.
`organization_session` returns account/onboarding facts and a semantic destination
without browser form secrets. `session_logout` ends the originating account session
through the same command as browser sign-out, including shared-entry tenant
propagation. It requires a real browser decision by default; a human may select
YOLO for that exact connection. The executing YOLO call or confirming browser
receives a safe sign-out outcome, including whether provider sign-out was
confirmed. Provider failure leaves local access ended. Later tool reads, action
results and retries are denied, so response loss never repeats the effect; sign
in and open a fresh connection to continue. Shared tenant hubs leave account
sign-out to `/api/cloud/mcp`.

Meaningful forms and redirects produce structured application data, command
receipts or destination URLs. Provider login/callback exchanges are connection
setup. Billing checkout/portal/export are meaningful owner-only operations, not
excluded redirects. Organization administration uses the shared commands above; project onboarding
remains separate application work.
Workspace relay frames are transport; bounded file reads and predefined project
actions belong to the shared application surface, never an arbitrary shell or
relay proxy. Stored change reviews and diffs use the change/artifact application
tools; their deployment availability is recorded in the matrix.

## Updating a decision

Edit only the affected operation rows, retaining IDs and owner references.
Implementing children update `status`, shared command/extraction, typed tool
proposal and execution coverage when their behavior actually ships. Keep
operator rows pending until implemented; do not relabel a missing service as an
excluded capability. Reassess role, scope, grant, ownership and confirmation
when application arguments or authorization change. Authentication and YOLO
are connection authority; tools cannot accept them as permission switches.
Mutations use the shared audit/idempotency contract. Lane requests go through
the orchestrator (INV-1). The matrix grants no execution permission.

`capability.Discover(os.DirFS(repositoryRoot))` independently walks actual
production Go registrations in all files under `internal/web`,
`internal/hubserver`, and `internal/cloudentry`, Templ sources, React/TypeScript
sources, and authored `static/js` sources. Go registrations are parsed with
`go/ast`; Echo parameter names, local aliases and groups are recognized. Route
constants are resolved from actual package declarations; changing a registered
path through its constant also produces drift. Legacy hub v1 APIs currently
refuse hosted sessions and native-only credentials; these restrictions remain
explicit availability decisions, not operator exclusions.
Frontend discovery conservatively records HTTP/request adapter calls (including
body/action arguments), HTMX/action/submit/navigation attributes, browser/client
forms/routes and named action-discriminator inputs/buttons. Exact definitions
are reviewable; line numbers are informational and not identity. Multiple
identical occurrences must each be decided. Generated Go/bundles, dependency
code and test fixtures are not dashboard definitions. Conservative client
matches get explicit local/transport decisions, rather than a source-directory
exclusion. No source list is copied from the matrix into discovery.

The scanner covers these existing source idioms, not arbitrary future languages
or dynamically generated routing frameworks. A change introducing a new route
registration/request idiom must extend the scanner and a synthetic regression
in the same PR. Do not add wildcard source exclusions or automatic decisions.
Only reviewed exact source sites belong in the fixture. To inspect candidates
without copying matrix data, run `go run ./internal/operatortool/capability/viewgen
-root . -sources` from the repository root (redirect scratch output to the provided
`TMPDIR`). Adding a route,
frontend request, browser form, client route or form action without a decision
fails; removed definitions and duplicate/orphan ownership fail too.

Focused diagnostics:

```sh
go test ./internal/operatortool/...
```

The source inventory diagnostic always enforces full operator parity:

```sh
go test -timeout=60s ./internal/operatortool/capability -run '^TestDashboardCapabilityCoverage$'
```

The test rejects every pending or excluded operator operation. It supplements
the parent's execution, authorization, approval, replay, client-conformance and
service-absence regressions; inventory coverage does not substitute for them.
The repository's configured gate remains `true`; this focused diagnostic is not
a new blocking product/commit gate.

Regenerate the readable view with `go generate ./internal/operatortool/capability`.
It calls `capability.RenderMarkdown(matrix)` on the loaded fixture. The source-coverage diagnostic checks source decisions, not the
text of the documentation. Regenerate the view after editing the fixture; no
templates, queries or CSS inputs are changed by this inventory.

## Operator confirmation and connection YOLO

Native form submission uses existing commands: `file_issue` for creation,
`edit_item`, `move_item`, `set_dependency`, `add_comment`, `edit_comment`, and
`create_change`. A native `file_issue` request may supply `github_issue_url`
without title or description; the native owner validates the attached repository,
canonicalizes the link and retains duplicate/retry identity. Native descriptions
may be empty. Creation priority ranks 1–4 map to native values 0–3; `edit_item`
uses native values 0–3 and the expected issue revision, and `edit_comment` uses
the expected comment revision. `create_change` accepts up to 32 `linked_issues`
owned by the same project. Content remains subject to the tools' bounded schemas.
Moves retain the existing orchestrator command and material/destructive actions
retain current browser approval or operator-selected connection YOLO.

Board issue discussion is available through `work_comments` (native cursors or
connector offsets) and `list_comments`. `work_pr_comments` reads the work item's
linked PR discussion using `project_id`, `reference`, `offset`, and `limit`
(1–200). The application selects the linked repository and PR; callers cannot
select an arbitrary forge resource. Only deployments with the existing PR comment
reader advertise it. Direct calls recheck current authority and ownership, and
missing services or provider failures return opaque unavailable errors. Both
transports use the same application adapter.

`move_item`, `set_priority`, `stop_run`, and `file_issue` use the same application
validation and commands as dashboard chat. Arguments are typed and bounded; a
`request_id` is an explicit business retry key for an exact command across
connections; it is independent of the JSON-RPC request ID. Changed arguments
with the same actor/organization/project/operation/key conflict. Expired or
revoked authority cannot replay a receipt.
`action_result` returns the original preview/outcome and never approves it. The
preview includes exact arguments, resolved project/resource and run identity,
organization, originating client, creation/resolution timestamps and a portable
dashboard approval URL. The application revalidates authority and target context
before executing the stored action. Tracker lane writes still use the orchestrator.

Ordinary writes execute directly. Moving Backlog to Todo differs from moving to
Cancelled/Done, resetting a terminal item, or moving an active item; stop/cancel
and unknown material actions require confirmation. Approval and rejection use the
existing chat preview in a browser form, with the dashboard's session/private
access session, same-origin form secret and browser cookie. An unprotected
dashboard UI cookie cannot approve actions: configure the existing dashboard
login or private access authentication before using approval or YOLO setup. API credentials,
MCP annotations, initialize metadata and repeated tool calls cannot approve.

`connection_info` returns a setup URL. Open it in the dashboard and explicitly
choose **YOLO · skip confirmations** for the displayed client and connection.
The same setup works for remote `/mcp` sessions and authenticated `detent mcp`
stdio bridges. The bridge registers one server-generated connection in default
confirmation mode; its setup identity stays fixed for discovery and calls. Mode
is server-side connection state, never a tool argument or mode header. YOLO
suppresses only confirmation: scope, grants, organization/resource ownership,
current credential validity, rate limits and workflow policy still apply. Audit
metadata records the connection mode, client, organization, action and retry ID.
Connections start in confirmation mode again after a new setup or server restart.

Hosted hubs with no dashboard command service return opaque unavailable results.
Conversation, access and billing commands use the same human approval and
connection authority boundary.


## Shared mutation audit and retries (#3338)

Application adapters carry trusted `mutation.Metadata`: principal, organization,
project/resource, action, source, actual connection mode/confirmation decision,
correlation and bound retry/input hashes. Authentication, mode and confirmation
are never tool arguments. Every current MCP command provides this context;
future mutation children must forward it through their shared application command.
A fresh correlation identifies each submission; approved execution and rejection
retain the original submission correlation. Business keys, credentials, invitation
secrets, prompt/comment/body content, support tokens, billing secrets and raw
provider errors are absent from audit summaries and protocol errors.

Dashboard commands reuse `workflow_phase_events` operator records to bind the
first authorized submission, including pending previews and rejected actions.
Only its server-created action can start that operation; a unique bound identity
permits only one execution across concurrent
connections, disconnects and daemon restarts. Receipts contain identifiers/URLs
and outcome, without arguments or rendered result text. Replays reauthorize the
current credential/project authority. The application owns persistence; neither
MCP transport writes SQLite nor a transport dedup cache decides business replay.
This adds no recovery or takeover loop. A missing durable application service
returns an opaque unavailable error before mutation.

Native connector commands forward the context retry identity through
`tracker.Mutation.IdempotencyKey` to the existing `native_commands` table.
Hosted session IDs no longer divide business receipts: current session and member
checks still precede replay. Hosted organization commands use the same extracted
receipt functions. Billing API keys bind actor/organization/operation/input in
`native_commands`, while checkout/customer retries continue using the existing
billing intents and stable provider keys. Portal completion replays its stored
URL; an uncertain portal attempt cannot repeat its provider effect. Hosted audit
rows and billing audit JSON accept the same content-free context.

Lost pending approval previews return a safe pending error after reconnect or
restart; no replacement action takes over their record. Rejected commands replay
the rejected outcome. Uncertain dashboard effects, invitation sends and portal
creates retain their
pending application receipt and return a safe error. Their providers cannot
prove a negative outcome or accept a business retry key, so there is no automatic
repeat. Inspect the resource/provider records before choosing a new key. Checkout
may resume a pending receipt only through the existing serialized billing intent
and provider idempotency semantics. Response persistence uses a bounded context
that survives a client disconnect. These guarantees apply through shared
application commands; protocol request IDs are never business retry keys.

Native revision-based edits must replay the exact originally submitted tracker
request, including its expected revision. Connector operations that recompute a
revision after an uncertain response can return a safe payload conflict; inspect
the resource instead of repeating the effect with a fresh key. Billing replay
also binds the configured provider account/mode and return destination, so a
configuration change cannot replay a purchase from another billing binding.

## Generic client setup

Cloud exposes setup information in **Settings → MCP**. The organization endpoint
comes from the application's canonical public URL: shared Cloud uses
`https://<cloud-origin>/organizations/<organization>/mcp`, and a dedicated
hosted origin uses `https://<organization-origin>/mcp`.

Hosted MCP currently requires the existing hosted browser session, current
membership and CSRF protection. Hosted operator authority rejects machine API
tokens, including otherwise valid scoped tokens. There is no MCP OAuth
authorization flow. A bearer-header configuration in an external client cannot
connect to Cloud today; do not export browser cookies or CSRF credentials as a
workaround. The private credential-maintenance listener does not mount MCP.

Self-hosted daemon clients can use `detent mcp` over stdio or the daemon's HTTPS
`/mcp` endpoint with an existing API key. Create a least-privilege expiring key
with `detent key add --name mcp-client --scope read --expires-in 30d`; select
write/admin scope and project restrictions only when needed. Keep the returned
token in private client configuration or its supported environment variables.
Use `detent key list` and `detent key revoke KEY_ID` to revoke it; deleting the
client entry does not revoke a key. These commands operate on the configured
daemon and do not create Cloud credentials. For unhosted hubs, use the separate
hub token-management API and `/api/v2/organizations/<organization>/mcp` route.

Follow the client-specific [Claude Code installation instructions](https://code.claude.com/docs/en/mcp)
or [Cursor installation instructions](https://cursor.com/docs/mcp), selecting
HTTP with an `Authorization: Bearer YOUR_DETENT_API_TOKEN` header for a
self-hosted endpoint. SSE and WebSocket transports are not supported.

Authoritative wire references are the
[2026-07-28 announcement](https://blog.modelcontextprotocol.io/posts/2026-07-28/),
[current specification](https://modelcontextprotocol.io/specification/2026-07-28),
[version compatibility](https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning),
[discovery](https://modelcontextprotocol.io/specification/2026-07-28/server/discover),
and [HTTP binding](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http).

1. Select an existing application identity and least-privilege credential with
   read authority, adding write authority and project grants for intended commands.
   Supply it through the stdio environment/config or each remote HTTP request.
   Self-hosted authority is `self-hosted`; if `X-Detent-Organization` is supplied,
   it must match. Hosted clients select an authorized organization through the
   existing organization-bound URL described above. Neither a tool argument nor
   client metadata can select an organization, role, scope, grant or principal.
2. Modern clients may call `server/discover` first and then send independent
   requests. Required `params._meta` keys are
   `io.modelcontextprotocol/protocolVersion: "2026-07-28"` and
   `io.modelcontextprotocol/clientCapabilities: {}`. Client info is optional display
   metadata. HTTP also requires matching `MCP-Protocol-Version` and `Mcp-Method`,
   and `Mcp-Name` for tool calls; encoded names follow the standard Base64 sentinel
   format. Unknown revisions, malformed metadata and mismatched headers produce
   bounded protocol errors. Older revisions use their original initialize/ready
   handshake and legacy HTTP sessions. No modern MCP session is created.
3. Load the authorized `tools/list` catalog; the default response includes all
   available definitions. Group definitions with
   each definition's `_meta["detent/toolset"]`. To load only selected groups, send
   `params._meta["detent/toolsets"]` as an array, for example
   `["board", "connection"]` or `["conversations_workspaces"]`. Omission or an
   empty array returns the complete catalog; unknown groups and invalid selectors
   are rejected. A changed catalog, selected group or identity invalidates an old
   cursor: restart discovery. Permissions are resolved on every discovery and
   invocation. Discovery is optional before a direct call and grants no authority.
   Only available typed application tools are advertised.
4. Call `connection_info` when available to get the application connection ID,
   current mode and dashboard setup URL. Stdio uses the authenticated bridge's
   server-issued handle; legacy HTTP uses its bound connection; modern HTTP binds
   the existing application approval conversation to the authenticated identity
   tuple. That application handle persists across POSTs and reconnects with the
   same credential/organization. Changing a credential or organization cannot
   retrieve another identity's pending actions. It is not an MCP protocol session.
5. Reads and ordinary non-destructive writes execute without a Detent confirmation.
   Material/destructive actions return an exact preview, `action_id`, approval URL
   and `result_tool`. Open the URL in an authenticated operator browser and confirm
   or reject the preview, then call `action_result` for the same action ID. This is
   portable across clients and needs no client-specific approval integration.
   Metadata, annotations, repeated calls and MCP input responses cannot approve.
6. YOLO requires the operator to explicitly enable it on the authenticated dashboard
   setup page for that application connection. It suppresses confirmation only;
   current authorization, ownership, exact targets, audits and retries still apply.
   It cannot be set in tool arguments, initialize/request metadata or mode headers.
   Modern HTTP callers using the same credential/organization share this existing
   application mode; use separate existing credentials to separate authority.
7. Mutations use a bounded explicit `request_id` business retry key, independent
   of JSON-RPC IDs. Reuse it only for identical arguments/operation/resource.
   The shared application command returns the receipt or a safe conflict/uncertain
   result; the MCP transport neither writes persistence nor decides business replay.

For example, a generic `2026-07-28` client can request only board and connection
tools with this JSON-RPC body over stdio or HTTP:

```json
{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"detent/toolsets":["board","connection"]}}}
```

Then invoke an authorized native list, using the actual project identifier:

```json
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"work_list","arguments":{"project_id":"YOUR_GRANTED_PROJECT_ID","archived":"false","states":["Todo","In Progress"],"limit":20,"include":["work"]},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}
```

HTTP adds the existing bearer credential, `Content-Type: application/json`,
`Accept: application/json, text/event-stream`, and `MCP-Protocol-Version: 2026-07-28`.
Use `Mcp-Method: tools/list` for the first body and `Mcp-Method: tools/call` plus
`Mcp-Name: work_list` for the second. Older clients send these same methods after
their negotiated handshake, omitting the modern protocol metadata and retaining
the legacy HTTP session/version headers. Toolset selection remains optional in
either lifecycle.

Remote connections require a trusted HTTPS endpoint and existing application
credentials. Terminate TLS at Detent or a trusted proxy configured with the public
application URL, preserve the intended origin, and redact credentials in logs.
Use localhost HTTP only for isolated local access; keep the daemon's established
listener and authorization policy. Untrusted forwarding headers cannot select a
public origin or principal. Cancellation is per-request: stdio supports
`notifications/cancelled`, and an HTTP disconnect cancels that request. Requests
and arguments are bounded, result objects are capped at 256 KiB, and encoded
response frames at 832 KiB on both transports, allowing the serialized text and
structured copies of a bounded result. Supported optional capabilities
are advertised accurately: this server exposes tools, not subscriptions, sampling,
roots, tasks, logging, caching or MRTR elicitation. Operator approval uses the
existing dashboard flow; no new revocation/recovery mechanism or lane writer is
introduced.

## Project settings and onboarding

The `projects` toolset (#3342) exposes authorized project listing/detail,
creation, onboarding progress, repository/integration configuration, import
jobs/records/advance, runner batch intake, cutover receipts, native/repository policy inspection,
policy approval/revoke and change-review policy. Use `repository_policy: true`
to address a legacy policy through its authorized project's repository binding;
clients never choose an unrelated owner/repository. Mutations take a typed
`input` and business `request_id`; revisions, policy IDs and cutover checkpoints
remain application preconditions. A completed retry returns the original receipt
after current authority checks, without another approval or provider fetch.
Batch discovery, additional preview pages and non-dispatchable apply use the
existing intake command directly. Dispatchable apply and batch retry require
exact approval; selecting a destination retains the application's lane and
runner checks. Batch reads and receipts redact runner/provider diagnostics and
return at most 200 preview issues/items with a cursor for subsequent reads.

Local daemon tools expose project settings, setup navigation and temporary
budget overrides. Reads and ordinary writes (including initial imports, progress,
import advance, disabled projection configuration and budget clear) execute
directly. Import restart, live cutover, policy changes, access grants, secret
removal, budget override and external projection/binding require exact browser
approval, unless the authenticated operator chose YOLO for that connection.
Dry-run cutover does not require confirmation. An unhosted hub has no hosted
browser approval service and returns opaque unavailable for material variants;
the local daemon and hosted dedicated/shared paths use their existing browser
authority. Hosted role, project grant and entitlement checks still apply in YOLO.

`project_setup` returns the existing authenticated setup URL and required steps.
`demo_setup_scenarios` reads the shared browser scenario manifest when demo
scenarios are enabled, and returns opaque unavailable otherwise.
Credential and workflow-file setup stays in that browser flow: tool inputs,
results, approvals and audit records never carry provider secrets. Secret
metadata is available through its existing safe read; removal uses its existing
command. Local project editing/tracker binding is part of interactive onboarding;
settings/library/reports have no configuration mutation in that dashboard.
Their read/filter parity uses the corresponding inventory application owners;
shared settings, budget and review-policy prerequisites remain enforced.

## Workspace, conversation and project actions

Native hub endpoints advertise the `conversations_workspaces` tools when their
application services exist. Workspace and conversation selectors use a current
`project_id` plus a typed resource ID. Organization conversation lists project
only the caller's readable projects. Lists accept at most 200 entries; message
history retains older-page cursors. Attachment, action output and terminal recording
reads return base64 byte chunks of at most 32 KiB with offsets and total size.
Attachment uploads accept at most 32 KiB per MCP request and retain the application's
attachment ownership, MIME, expiry and size checks.

Create/post/edit commands require a business `request_id`, reused unchanged across
retries and reconnects. Completed requests return their original application receipt
without another approval or action-definition lookup, including deleted actions,
after current project authority is checked. Workspace/attachment/action deletion, conversation linking,
execution controls and action command or automatic-run changes return a pending
preview in confirmation mode. A configured action run takes `action_id`,
`workspace_id` and `expected_revision`; its browser preview shows the command, and
editing the definition invalidates that run request. Ordinary messages and metadata
edits execute directly. Results include shared action status, application data and
an approval URL; `action_result` reads the current connection's outcome and never
approves it. Hosted operators use their existing login and CSRF-protected form at
`/chat/approval`. Only the originating hosted browser principal can select its session connection's YOLO, and
current grants are still enforced on every execution. Standalone native hubs without
an authenticated browser approval service return an opaque unavailable result for
material operations; reads and ordinary commands retain their application boundaries.

`workspace_file_list` lists one relative directory (empty `path` means the root),
with at most 500 entries and an opaque `next_cursor`; pass that cursor back for the
next page. `show_ignored` uses the runner's existing ignored-file policy.
`workspace_file_read` requires a relative `path` and accepts a nonnegative byte
`offset` and `length` from 1 to 32768 (default 32768). Results retain the runner's
`path`, MIME, file `size`, `offset`, `data`, `encoding` (absent for UTF-8, `base64`
for binary), and `truncated`. Decode base64 before advancing the offset by the
returned byte count. These reads use the current project read and runners grants,
workspace/subject ownership, lease/fencing and the existing runner file owner.
Read-only workspaces remain readable; traversal, symlink escape and denied secret
paths retain the existing refusals. No relay tickets, host paths or runner error
messages are returned.

Both tools return `status: available` with `result` for a successful page/chunk,
including an empty directory. Non-serving/ended workspaces return `unavailable`
with workspace state and an existing relay refusal `code`; a bound workspace
without file capability returns `unsupported_transport`. Missing services, revoked
credentials and transport timeout/failure remain safe errors, never fabricated
empty results. Each exchange uses the existing ten-second relay timeout and stream
limits and releases its stream on completion. The shared catalog supplies the same
schemas to hosted HTTP MCP and stdio/application bridge clients.

`workspace_terminal` still returns an authorized `unsupported_transport` result
with workspace state/capabilities and accepts no shell input or relay ticket.
`stream_conversation_events` returns an authorized
snapshot/cursor and an explicit unsupported SSE transport result; poll bounded
conversation history instead. Configured action runs and their output provide
headless action execution using the existing runner dispatch.

The daemon's `get_operator_chat` reads only the current connection's history;
`post_operator_chat` sends a bounded message to its configured provider using the
same durable audit/retry contract. The nested provider receives authorized read
tools. Operator mutations use the named MCP tools and existing approval surface,
never model-generated conversation confirmation. An absent provider or native
workspace/runtime service returns an opaque unavailable result. No browser cookie,
authentication context, arbitrary session ID or YOLO setting is a tool argument.

This child implements #3346, not the final deployment/tracker/transport parity
acceptance on #3259. The legacy board conversation panel reads tracker/PR comments,
so its matrix ownership is corrected to the comments child #3341.

Ordinary native work-item controls (native #145) use
`get_work_item_conversation({project_id, work_item_id})` to read the canonical
conversation, current execution owner, capabilities and bounded history. The
same snapshot is available at `GET /work-items/:item/conversation` under the
scoped project API. Lookup does not create a conversation, issue or attempt.
An authenticated live-capable worker's existing bind creates an empty shared
conversation when needed; historical comments remain issue comments.
Use `post_conversation_command` with the returned conversation ID and current
expected attempt/turn to steer, or request interrupt through its existing
material-action approval. Existing accepted/delivered/error receipts remain
authoritative; a conversation or running issue alone does not prove a provider
supports live control. Private conversation visibility and current grants apply
to lookup and command execution. Already-running workers that skipped binding
before the fix cannot gain a provider control object from an operator lookup.

## Enrolled runner update application (native #93)

Hosted `get_runner_update` and `update_apply` use current organization runner
administration, including all-project runner grants. Read support first with
`get_runner_update({"runner_id":"..."})`. The read returns the runner revision,
update-owner protocol/support, explicit available/up-to-date/unknown discovery
evidence, available version and discovery time, requested
state, applied evidence and the fresh running process build. Older runner
protocols, missing installed owners and stale heartbeats return `unavailable`;
denied scope remains an authorization refusal.

Hosted `update_apply` requires `request_id`, `runner_id`, and `change` containing
`expected_revision`, `expected_build_revision`, `service: "detent"`, `version`,
and optional `release`/`from_release`. Its existing browser confirmation or
operator-selected connection YOLO is required. The local daemon retains its
existing `release`/`from_release` contract and refuses enrolled-runner selectors.

The corresponding authenticated API routes are
`GET /api/v2/organizations/:organization/runners/:runner/update` and
`POST /api/v2/organizations/:organization/runners/:runner/update/apply`.
POST takes the same change fields plus `confirm: true` and `idempotency_key`.
Acceptance means `requested`, not applied or running. Inspect the read for
`draining`, `refused`, `uncertain`, `applied`, `restart_requested` or `running`.
Receipts carry selected version and request/application/observation timestamps;
release application carries the verified commit and installed binary checksum.

Delivery and acknowledgments reuse the authenticated runner heartbeat negotiated
with `runner_installed_update`. Application uses the same installed scheduler as
UI/CLI, including configured release discovery, artifact checks, drain/restart and
rollback. The observed release version is pinned before application. Windows detached
replacement exposes a verified target while application remains uncertain;
a matching post-start process observation confirms application. The existing
scheduler state retains delivery/application evidence across restart. Interrupted
application remains uncertain; duplicate delivery adds no retry/recovery loop.

A fresh post-start heartbeat must match the applied version, commit, checksum and
platform before reporting `running`. The confirmed running receipt survives
restart; a later local build change is `drifted`, preserving completed evidence
and permitting a fresh operator request. Dirty source or replaced Go dependencies are
reported as `private_patched_source`, not approved published releases. Source
details omit local module paths. Raw errors, executable paths, artifact URLs,
shell commands, environment and credentials are absent from this surface. Tenant
keys cannot control the shared Hub; standalone restart is unavailable.
