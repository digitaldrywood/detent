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
An optional `X-Detent-Organization` header must match `self-hosted`. HTTP sessions
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
`request_id` identifies retries of an exact command within the connection. Changed
arguments with the same ID deny, and expired connections cannot recreate receipts.
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
