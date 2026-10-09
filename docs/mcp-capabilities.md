# MCP operator capabilities

`operatortool.Registry` is the canonical protocol catalog. Deployments advertise
its tools according to application availability and current connection authority.
The dashboard's operator API uses the same application executors as MCP.
`TestOperatorToolAPIUsesSharedReadOnlyExecutor` checks that every tool advertised
by the dashboard is registered in this catalog. Add new dashboard commands to
the real catalog and preserve their authorization and execution regressions.

## Cloud issue attachments (native #177 and #238)

Shared entry plus the tenant Hub supports `upload_attachment`,
`read_attachment_metadata`, `read_attachment`, `reference_attachment` and
`delete_attachment` in the `work` toolset. Reads return authenticated metadata
and at most 32 KiB of base64 content per call, within the 256 KiB result bound.
The existing 60,000-character base64 upload bound remains unchanged. Item/comment
binding reuses the same-project reference owner. Destructive deletion uses the
current write authority; its
receipt distinguishes pending and confirmed object deletion. All calls resolve
current project grants and credential scope. Read-only credentials discover only
reads; local and dedicated deployments omit these shared-storage operations.
Internal expiry, existence and deletion-confirmation callbacks require signed
service authority and expose no model-callable tool. See
[Cloud attachments](cloud-attachments.md) for the transport and deletion contract.

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
owner. `local_project_configuration` reads its actual configuration
revision, effective policy and selected workflow policy, including private
overlays. The revision covers both global configuration and the selected project
definition. Provenance
contains identities and revisions, without file paths, workflow instructions,
credentials or private configuration values. A Cloud project, runner source
association and the runner's local project registration remain separate
authorities.

`apply_local_project_policy`, `resume_local_project`, `drain_local_project` and
`detach_local_project` require current admin scope for the exact project, current application authority,
`request_id`, `expected_config_revision` and `expected_policy_id`. They reuse
durable operator command receipts; retries reauthorize and return the original
receipt without repeating the effect. Policy application additionally requires
the exact approved `policy_id` and `source_revision` and settled work. External
local definitions without trusted repository provenance adopt authored shared
behavior from the durable Cloud approval, retaining host-only configuration and
private instructions. Trusted repository definitions remain authoritative;
commit and approve their intended shared definition at the configured source.
Read `selected_policy` for the application preview. The project is paused through
the existing owner
before policy application, then restored to its prior running, paused or draining
state. `resume_local_project` clears a saved or runtime pause through the same
owner and requires runner administration (`manage_runner`) as well as admin scope.
Policy application preserves private overlays without rewriting them. For the
existing `worker.allow_local_binding`
field, the owner resolves complete candidate descriptors from the actual
definition and its overlays. `local_binding_policy` and
`restricted_binding_policy` can be presented to the existing policy approval
owner; applying one requires the matching `allow_local_binding` argument. The
private split definition's `detent.local.yaml` retains all other settings.
No arbitrary configuration path, digest or worker privilege is accepted.

Cloud reaches the enrolled runner's same configuration owner through heartbeat
observations and requests, without starting or enabling the retired local board.
The current runner binding and project grants select the owner. `runner_id`
disambiguates multiple granted runners; Cloud commands also require
`expected_runner_revision`. The issuer's current authority, runner revision,
project grants and approved policy are rechecked before delivery. A selected
request awaiting heartbeat application returns `pending: true`; fresh configuration reads distinguish delivery
from saved configuration and applied runtime policy.
Completed commands remain visible in `last_operation`, including refusal
constraints, after fresh heartbeat observations. Ordinary policy adoption
reports `applied` without `saved` because local files remain unchanged; Cloud's
durable approval is used again on reload and restart.

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
supported installed configuration service; Cloud never edits a file directly
or starts a stopped board. Stale revisions, unsupported sources and
incomplete handoff return redacted constraints, never fabricated completion.

## Current connection authority

`operatortool.Identity` binds principal, organization, credential and session;
it contains no permissions. `Connection.Resolve` resolves current application
authority for every discovery/call. `AuthorizeCurrent` checks identity equality
and organization binding before delegating scope, project and resource ownership
to the application adapter. Approval/audit children must invoke it again with
the current authenticated connection when an approved action executes. Never
persist a resolved authority or derive permission from discovery, annotations,
client confirmation settings. Unknown resource kinds deny until their shared application
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
native administration uses its installed application adapters and current key scope.
Unavailable services remain unavailable through MCP. Their existing API commands remain available. Providers
without invitation-by-ID administration do not advertise invitation acceptance.
Absent services return opaque unavailable errors. No new approval credentials or
platform powers are installed.

Native #235 adds explicit bounded project grants to invitation sending and exposes
pending-invitation grant replacement and resend through their existing hosted
commands. Edit requires a complete grants list; an empty list removes project
access. Edit/resend discovery requires provider invitation-by-ID administration,
and resend also requires delivery support. Current organization, role, pending
status, expiry and grant authority are rechecked at execution.
Selected bearer keys cannot grant or remove access beyond their own project scope.
Typed requests include the invitation and grants; durable command receipts
prevent repeated provider delivery.
Dedicated/shared and stdio/HTTP fixture coverage uses no live mail. Native #26
retains strict final conformance and unconditional zero-pending acceptance.

Access-changing and destructive commands execute directly after current authority checks. Durable application receipts omit credential material; deliberate
credential results stay on their originating connection, recheck current authority
and the delivered credential's validity, and are never restored from durable retry
receipts. A fresh connection retry returns the resource receipt without its secret.
`organization_session` returns account/onboarding facts and a semantic destination
without browser form secrets. `session_logout` ends the originating account session
through the same command as browser sign-out, including shared-entry tenant
propagation. It executes directly with current account authority. The calling client receives a safe sign-out outcome, including whether provider sign-out was
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
tools; deployments advertise them only when their application services exist.

## Client confirmation and current authority

Every authorized MCP call executes directly, including terminal workflow moves,
deletion, credential revocation, billing and repository policy approval. The Hub
checks the API key's scope, project access, current membership and grants on every
call, including retries. Read keys cannot write; write keys cannot perform admin
operations. Revoked and expired keys fail even on an existing connection.

`connection_info` returns the authenticated connection and organization identity.
It has no mode or setup URL. Connection age, reconnects and idle session pruning
never select an approval policy. There are no server-side pending approvals or
`approval_url` responses. The retired `/chat/approval` URL returns 404.

Tool annotations remain descriptive: `readOnlyHint` and `destructiveHint` let the
client decide when to prompt. Claude Code, Codex and other clients own their local
confirmation policy. Use a narrower API key to limit a client's authority.

Luna's browser client shows proposed changes inline in the conversation and asks
for confirmation by default. **Ask me to confirm chat changes** is a per-user,
per-browser preference. Turning it off makes that client submit proposed calls
directly. The server stores conversation messages, not pending approval actions;
submitted calls are checked against current authority and expected revisions.

`action_result` remains a bounded read of completed connection receipts for
credential delivery and attachment object-deletion status. It never executes a
call or approves an action. Durable application receipts retain business retry
identity across reconnects; credential material stays out of conversation and
audit records.

Native form submission uses `file_issue`, `edit_item`, `move_item`,
`set_dependency`, `add_comment`, `edit_comment` and `create_change` through their
existing application owners. A `request_id` is a business retry key, independent
of JSON-RPC IDs. Changed arguments under the same actor, organization, project,
operation and key conflict. Native work-item and comment revisions are positive
decimal strings in both responses and `expected_revision` inputs. Copy the
observed revision unchanged into a mutation; numeric JSON revisions are rejected.
Tracker lane requests retain the orchestrator owner.

## Shared mutation audit and retries (#3338)

Application adapters carry trusted `mutation.Metadata`: principal, organization,
project/resource, action, source, client source and call outcome,
correlation and bound retry/input hashes. Authentication and authority
are never tool arguments. Every current MCP command provides this context;
future mutation children must forward it through their shared application command.
A fresh correlation identifies each submission; approved execution and rejection
retain the original submission correlation. Business keys, credentials, invitation
secrets, prompt/comment/body content, support tokens, billing secrets and raw
provider errors are absent from audit summaries and protocol errors.

Dashboard commands reuse `workflow_phase_events` operator records to bind the
first authorized submission.
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

Uncertain dashboard effects, invitation sends and portal creates retain their
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
4. Call `connection_info` when available to read the authenticated connection ID
   and organization. It carries no confirmation mode or setup URL.
5. Calls execute directly after scope, project access and current grant checks.
   Clients decide whether to prompt for sensitive operations using their local
   configuration and tool annotations.
6. Limit a client's authority with a read, write or admin API key and the
   appropriate project access. Connecting or confirming locally never adds grants.
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
roots, tasks, logging, caching or MRTR elicitation. Confirmation belongs to clients; Hub access control and application owners remain authoritative.

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
existing intake command directly. Dispatchable apply and batch retry execute with current authority;
selecting a destination retains the application's lane and
runner checks. Batch reads and receipts redact runner/provider diagnostics and
return at most 200 preview issues/items with a cursor for subsequent reads.

Project mutations, including repository policy approval, execute directly when
the key has the required admin or write scope and current project grants.
Deployment service availability, revision checks and policy provenance still apply.


`project_setup` returns the existing authenticated setup URL and required steps.
`demo_setup_scenarios` reads the shared browser scenario manifest when demo
scenarios are enabled, and returns opaque unavailable otherwise.
Credential and workflow-file setup stays in that browser flow: tool inputs,
results, approvals and audit records never carry provider secrets. Secret
metadata is available through its existing safe read; removal uses its existing
command. Settings, library and report reads and filters use the corresponding
application owners;
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
retries and reconnects. Completed requests return their original application
receipt after current project authority is checked. Workspace, attachment and
action deletion, conversation linking and execution controls execute directly.
A configured action run takes `action_id`, `workspace_id` and `expected_revision`;
editing the definition invalidates that request. Results include action status and
application data. `action_result` reads completed receipts and object-deletion
status; it never approves or executes a call.


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
tools. Operator mutations use the named MCP tools after current authority checks. An absent provider or native
workspace/runtime service returns an opaque unavailable result. No browser cookie,
authentication context, arbitrary session ID or approval setting is a tool argument.

The legacy board conversation panel reads tracker/PR comments through the
comments application owner.

Ordinary native work-item controls (native #145) use
`get_work_item_conversation({project_id, work_item_id})` to read the canonical
conversation, current execution owner, capabilities and bounded history. The
same snapshot is available at `GET /work-items/:item/conversation` under the
scoped project API. Lookup does not create a conversation, issue or attempt.
An authenticated live-capable worker's existing bind creates an empty shared
conversation when needed; historical comments remain issue comments.
Use `post_conversation_command` with the returned conversation ID and current
expected attempt/turn to steer, or request interrupt through its existing
current application authority. Existing accepted/delivered/error receipts remain
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
and optional `release`/`from_release`. Current runner administration authority is required. The local daemon retains its
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
