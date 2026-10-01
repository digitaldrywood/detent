# Dashboard capability inventory

The [machine-readable matrix](../internal/operatortool/capability/matrix.json)
and its [readable view](mcp-capability-matrix.md) are the implementation checklist
for #3259. #3335 inventories the surface; it does not deliver full MCP parity.
The fixture is planning and review evidence, not a runtime authorization policy,
tool catalog, deployment feature flag, or tracker writer. The current five read
tool names and schemas in `internal/operatortool/catalog.go` remain unchanged.

Each stable operation ID has exact source-site decisions, deployment and tracker
availability, current role/scope/grant/ownership restrictions, preconditions,
argument-dependent confirmation classes, the application read/command and
required extraction, a bounded typed tool proposal, child owner, implementation
status and coverage evidence. Proposed annotations conservatively describe the
whole operation, including risky argument variants. Reads are retry-safe; mutations
remain non-idempotent hints until their shared retry contract is implemented. A route registration is a source site, not a
capability count. Browser/JSON aliases and frontend calls can share a row.
`pending` means required operator parity remains unfinished. It is never an
accepted exclusion. `implemented` describes the existing read slice with current
connection authority (#3336), not parent acceptance. `excluded` is reserved for
explicit current asset, local presentation, authentication exchange, worker,
transport or staff authority sites. Staff decisions do not give organization
operators staff privileges. Existing delete/revoke actions remain pending
operator work where their current authority allows them.

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
their authenticated read catalog is empty and direct legacy read calls return
opaque unavailable errors after resource authorization. The read/service children
must supply shared application reads, not a compatibility HTTP proxy.

Session/account/logout/context-selection and broader dashboard operations in
the matrix remain pending typed application work; this authority contract does
not claim their implementation or complete #3259.

Meaningful forms and redirects produce structured application data, command
receipts or destination URLs. Provider login/callback exchanges are connection
setup. Billing checkout/portal/export are meaningful owner-only operations, not
excluded redirects. Organization invitation, creation, selection, provisioning,
deletion, role/grant changes and project onboarding remain application work.
Workspace relay frames are transport; bounded file reads and predefined project
actions belong to the shared application surface, never an arbitrary shell or
relay proxy. The frontend PR-action request currently has no registered hub
route; it remains a pending #3347 operation with that service gap recorded.

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

The parent #3259 uses the same coverage test with strict parity enabled, after
all children implement their rows:

```sh
DETENT_MCP_REQUIRE_PARITY=1 go test ./internal/operatortool/capability -run '^TestDashboardCapabilityCoverage$'
```

Strict mode rejects every pending or excluded operator operation. It supplements
the parent's execution, authorization, approval, replay, client-conformance and
service-absence regressions; inventory coverage does not substitute for them.
The repository's configured gate remains `true`; this focused diagnostic is not
a new blocking product/commit gate.

Regenerate the readable view with `go generate ./internal/operatortool/capability`.
It calls `capability.RenderMarkdown(matrix)` on the loaded fixture. The source-coverage diagnostic checks source decisions, not the
text of the documentation. Regenerate the view after editing the fixture; no
templates, queries or CSS inputs are changed by this inventory.

## Operator confirmation and connection YOLO

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

Hosted hubs with no dashboard command service continue to return opaque
unavailable results; this child does not install missing hosted conversation,
access or billing operations. Their owner children must use this same human
approval/connection authority boundary when extracting their application commands.
Parent #3259 remains pending.


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
that survives a client disconnect. These guarantees do not install the pending
hosted/issue/comment tools or complete parent #3259.

Native revision-based edits must replay the exact originally submitted tracker
request, including its expected revision. Connector operations that recompute a
revision after an uncertain response can return a safe payload conflict; inspect
the resource instead of repeating the effect with a fresh key. Billing replay
also binds the configured provider account/mode and return destination, so a
configuration change cannot replay a purchase from another billing binding.

## Generic client setup

Use any MCP client with either a stdio command (`detent mcp`) or the deployed
organization's HTTPS MCP endpoint. Authoritative wire references are the
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
3. Load `tools/list` pages by following the returned opaque `nextCursor` until
   absent. Each page contains at most five tools; group definitions with
   `_meta["detent/toolset"]` (`board`, `fleet`, `telemetry`, `actions`, `connection`).
   A changed catalog/identity invalidates its cursor: restart discovery. Permissions
   are resolved on every page and invocation. Loading a page is not required for
   a direct call, and loading one never authorizes that call. Only available typed
   application tools are advertised; parent #3259's pending operations stay pending.
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

Remote connections require a trusted HTTPS endpoint and existing application
credentials. Terminate TLS at Detent or a trusted proxy configured with the public
application URL, preserve the intended origin, and redact credentials in logs.
Use localhost HTTP only for isolated local access; keep the daemon's established
listener and authorization policy. Untrusted forwarding headers cannot select a
public origin or principal. Cancellation is per-request: stdio supports
`notifications/cancelled`, and an HTTP disconnect cancels that request. Requests
and arguments are bounded, result objects are capped at 256 KiB, and encoded
response frames at 320 KiB on both transports. Supported optional capabilities
are advertised accurately: this server exposes tools, not subscriptions, sampling,
roots, tasks, logging, caching or MRTR elicitation. Operator approval uses the
existing dashboard flow; no new revocation/recovery mechanism or lane writer is
introduced.

## Project settings and onboarding

The `projects` toolset (#3342) exposes authorized project listing/detail,
creation, onboarding progress, repository/integration configuration, import
jobs/records/advance, cutover receipts, native/repository policy inspection,
policy approval/revoke and change-review policy. Use `repository_policy: true`
to address a legacy policy through its authorized project's repository binding;
clients never choose an unrelated owner/repository. Mutations take a typed
`input` and business `request_id`; revisions, policy IDs and cutover checkpoints
remain application preconditions. A completed retry returns the original receipt
after current authority checks, without another approval or provider fetch.

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
Credential and workflow-file setup stays in that browser flow: tool inputs,
results, approvals and audit records never carry provider secrets. Secret
metadata is available through its existing safe read; removal uses its existing
command. Local project editing/tracker binding is part of interactive onboarding;
settings/library/reports have no configuration mutation in that dashboard.
Their read/filter parity remains with the corresponding inventory owners. This
child implements its matrix rows and shared settings/budget/review-policy
prerequisites, and does not complete parent #3259 or those other children.
