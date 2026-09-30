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
accepted exclusion. `implemented` describes the existing read slice only, not
completed principal binding or parent acceptance. `excluded` is reserved for
explicit current asset, local presentation, authentication exchange, worker,
transport or staff authority sites. Staff decisions do not give organization
operators staff privileges. Existing delete/revoke actions remain pending
operator work where their current authority allows them.

Deployment availability distinguishes the self-hosted dashboard and unhosted
hub, hosted dedicated organization hub, shared entry plus organization hub,
and the private credential-maintenance listener. Hosted application routes
require hosted configuration. The shared entry proxies organization API and
browser traffic, not arbitrary MCP HTTP requests. GitHub/native availability
is recorded per operation; native-only policy/workspace/changes services cannot
be assumed present in a GitHub-only daemon. Every proposed tool must return a
safe opaque unavailable result when its application service is absent; this
inventory does not install services or change current authority. The current
read executors already handle missing telemetry/explanation dependencies.

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
