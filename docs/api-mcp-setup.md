# API & MCP setup

Native Hub `dashboard` and `board_state` reads use the same filtered board
inventory and bounded operational observations as the Work page. Their native
results contain per-project workflow definitions, lane totals, paged items and
operational items, plus `running`, `queued_inventory`, `open`,
`closed_inventory` and `total` counts. Queued inventory includes held items in
dispatchable lanes without a live worker; it is not a dispatch-readiness count.
Terminal states do not contribute to running or queued inventory. `board_state` accepts a
state filter. Both reads accept an optional project and an item limit (default
100, maximum 200); omitted projects resolve through current readable grants.
The item budget is shared across returned projects. `truncated` marks omitted
projects or items, so aggregate counts may cover only returned projects. Use
project-scoped reads or `work_list` cursors to continue inventory reads.
`aggregate_dispatch_readiness` remains explicitly unavailable; use the advertised
`eligibility_tool` (`explain_item`) for current native admission evidence,
including human ownership, dependencies and registered-runner conditions.

For a selected local board, discover the `local_projects` toolset on its
authenticated `/mcp` connection. `local_project_configuration` reads the actual
local revision and redacted selected/effective policy. Its admin commands apply
an approved committed policy, drain current work or detach one settled migrated
project. They use the existing operator command API and authority and receipt owner;
see [managed local project configuration](mcp-capabilities.md#managed-local-project-configuration-native-94)
for exact revision, policy and handoff requirements. Cloud also reaches the
enrolled runner's configuration owner through its existing heartbeat transport,
independently of the local board. Select `runner_id` when more than one granted
runner reports the project. Commands through Cloud additionally require that
runner's current `expected_runner_revision`. Runner administration and local
project commands require an Admin key. Hosted owners and admins have organization
runner access; members require runner management grants on the organization’s
projects. An all-project Write key
does not grant runner administration. A stopped or missing runner owner
returns an explicit constraint; these calls never start or enable a board.
Verify `registered` and `runtime_registered` through a
fresh read after a saved removal; a command retry returns its original receipt.

When `policy_mismatch` is true, the selected local policy differs from the
runner's effective policy. Cloud shows the selected descriptor in the existing
Integrations policy approval row and marks the runner as needing attention until
the effective policy matches. To approve it through MCP, pass the entire
`selected_policy` object unchanged as `approve_project_policy`'s `input.policy`,
with the current approved ID as `input.expected_policy_id`. Preserve every
returned field, including `authored`, `profile`, `configuration`, `workflow` and
`gates.human_review` when present. Reconstructing a subset can invalidate the
identity digest. Authorized approval executes directly; approval and runner
application are separate operations.

For the existing `worker.allow_local_binding` setting, read the effective
`allow_local_binding` and the complete `local_binding_policy` or
`restricted_binding_policy` descriptor. Approve the selected descriptor through
`approve_project_policy` using the current approved policy identity, then submit
`apply_local_project_policy` with the matching `allow_local_binding`, policy ID,
source revision and configuration revision. The owner changes only that field
in the configured project's private `detent.local.yaml`, preserving its other
settings. Configuration CAS includes the global configuration and actual project
definition. `pending` means the selected request awaits heartbeat application; read again
for `saved` and `applied` evidence. The project must be paused or draining with
settled work. After draining settles, application uses the existing pause owner
before changing policy. The grant
defaults to false and includes connections to other localhost services as well
as temporary test servers; the sandbox tier and host-service grants retain
their existing owners.

Without a Hub, a local instance uses its SQLite configuration owner for
`organization_project_rank`, `organization_project_rank_update`,
`get_organization_model_selection`, `update_organization_model_selection`,
`get_project_model_selection` and `update_project_model_selection`. The commands
are available through `/api/v1/operator-tools/:tool_name` and MCP with the same
arguments and revision semantics as Cloud. Reads of model selection require read
scope; mutations and project-rank operations require admin scope. Organization
settings require an unrestricted credential; project settings respect project
grants. Updates use the existing operator mutation receipts for retries.
`get_runner_routing` and `update_runner_routing` use runner ID `local` for the
allowed-projects list. Preserve the returned machine fields and change only
`project_ids`; disabling runner access retains the project and its rank/model
settings. The existing configuration watcher applies database edits to the
runtime without restarting projects for rank or model-selection changes.

Sign in to the intended Cloud organization and open **Settings → API & MCP**.
Existing `/settings/mcp` bookmarks open this same page. Create a named key,
choose Read, Write or Admin, and choose an expiry (7, 30 or 90 days, or Never). The creation API accepts 1–90
days
with `expires_days`, or `never_expires: true` with `expires_days: 0`. Keys with no
expiry are listed with `expires_at: null`.
Project access defaults to **All projects, including future projects** within
this organization and your current permissions. You can create this key before
any project exists. Choose **Selected projects** and select at least one project
to narrow access. Existing restricted keys keep their selected grants.
Copy the once-displayed key directly to a private secret store or environment
as `DETENT_API_KEY`. The page lists scope, all-project access or selected
project names, expiry, fingerprint and revocation state; it never retrieves an existing secret. Revoke keys here.
Removing client configuration does not revoke a key.

The same key authenticates direct API requests and MCP. Effective permissions
are the intersection of its scope/project grants and the issuing user's current
provider membership, local membership, role and project grants. Removing or
replacing membership, losing project access, expiry and revocation deny later
calls, including calls on an already initialized MCP session. An API key cannot
change organizations or impersonate another user. Runner and worker credentials
remain separate and cannot connect as operators.

The browser creation API accepts `project_access: "all" | "selected"` and
`project_ids`. Omitting the mode and project selection defaults to all projects;
legacy requests supplying project IDs remain selected. Explicit selected mode
without IDs returns 422 with a clear error. All mode with selected IDs is rejected
as ambiguous. Creation and listing metadata both return `project_access` and
`project_ids` (an empty array for all-project keys). All mode is durable, never a
snapshot of existing IDs; new authorized projects work with the original key and
MCP session without synchronization or reconnection. Removing grants from a
selected key never changes its mode or widens its access.

The hosted MCP `credential_create` tool uses the same `project_access` and
`project_ids` contract through the shared key owner. Its existing scope, expiry,
current authority and connection-bound secret delivery requirements still apply.

Key management requires the issuing user's browser session and CSRF state.
API and MCP client connections require only `Authorization: Bearer`, without
browser cookies, sessions or CSRF. The server executes calls directly after checking key scope, project access and
current grants. The MCP client owns user confirmation through its local settings;
there is no server confirmation mode, pending approval or approval URL. Revoked
or expired keys fail every later call and retry. Use narrower keys to limit clients.


## Organization URLs

Issue and comment attachment storage uses the authenticated organization and
project routes described in [Cloud attachments](cloud-attachments.md).

Copy the URLs shown on this page. On shared Cloud their complete shapes are:

```text
https://app.detent.cloud/organizations/ORG/mcp
https://app.detent.cloud/organizations/ORG/api/v2/organizations/ORG
```

The second URL is the direct API base. A dedicated/self-hosted organization uses
its own public HTTPS origin and deployment mount. Keep the complete shared
`/organizations/ORG` mount. Do not use another organization's endpoint, a project
page, `/sse`, or an invented `/api/v1/state` or OpenAPI route. Endpoint discovery
and request payloads are documented in [Hub API](hub-api.md), especially Native
collaboration and Changes. [Operator capabilities](mcp-capabilities.md) documents the real catalog and
deployment availability.

## Direct API agent handoff

Use **Copy API setup prompt** on the same page. It fills in the current
organization/base URL and an initial project context. Choose a project actually
granted to the key; if the suggested project is not selected on that key, replace
it with a granted ID from the key metadata.

The following is the handoff's executable read portion. Populate private
`DETENT_API_KEY` through the client's secret store, without pasting it into a
conversation, committed file, screenshot, URL, diagnostic output or shell trace.
Replace the public organization/project placeholders before running:

```sh
set +x
DETENT_API_BASE='https://app.detent.cloud/organizations/ORG/api/v2/organizations/ORG'
DETENT_PROJECT_ID='YOUR_GRANTED_PROJECT_ID'
curl --fail --silent --show-error --header "Authorization: Bearer $DETENT_API_KEY" "$DETENT_API_BASE/projects/$DETENT_PROJECT_ID"
curl --fail --silent --show-error --header "Authorization: Bearer $DETENT_API_KEY" "$DETENT_API_BASE/projects/$DETENT_PROJECT_ID/work-items?limit=20"
```

Verify returned project/organization IDs before reporting success. An empty
work-item list is valid after the project read verifies the context. Direct API
use needs no MCP client. Ordinary writes need Write scope and current write
access; consult each operation's documented payload, revision and idempotency
requirements. Sensitive operations execute directly when authorized. Configure confirmation in
the client and choose the key scope and project access accordingly. Workflow
requests still use the application's orchestrator lane owner.


A concise agent handoff is:

> Connect directly to the copied Detent API base for this organization. Have me
> create/select an expiring shared key on Settings → API & MCP with Read scope
> and the required projects. Ask me to put it directly in your private secret
> store/environment as DETENT_API_KEY, never this conversation. Send
> Authorization: Bearer privately with no cookies or CSRF. Run the two documented
> project/work-item GETs above for a granted project and verify the context.
> Follow Hub API request schemas and current authorization. Configure sensitive-operation confirmation in the client;
> authorized calls execute directly. Report success or an access error without secrets.

## MCP agent handoff

For native `file_issue` with `github_issue_url`, read `get_project_integration`
and `get_native_project` for the selected project. The URL must match its
attached repository or associated runner checkout. A missing association returns
`invalid_request` with the existing setup owner: an administrator uses
`bind_native_repository` with the observed integration `expected_revision`,
`repository` and `source: "runner_checkout"`. This requires a currently enrolled,
authorized runner reporting that matching checkout; Cloud needs no repository
credentials and GitHub tracker mode stays disabled. Choose a configured
nonterminal, non-dispatchable destination such as Backlog. Linked creation
records pending source context for existing runner intake, and repeated source
URLs reuse the native work item. Cross-repository requests retain their refusal.

Use **Copy MCP setup prompt** on the same page. It includes the exact endpoint,
organization/project context, the same private key instructions, supported
transport, initialization, paginated tool discovery and an authorized read.
For a supported Codex Streamable HTTP client, private configuration can use:

```toml
[mcp_servers.detent]
url = "https://app.detent.cloud/organizations/ORG/mcp"
bearer_token_env_var = "DETENT_API_KEY"
```

Configure another client using its documented private bearer-header support.
Send JSON POSTs with `Content-Type: application/json` and
`Accept: application/json, text/event-stream`. Clients using `2026-07-28` send
per-request protocol metadata and matching `MCP-Protocol-Version`, `Mcp-Method`
and tool-call `Mcp-Name` headers; they use stateless POSTs without initialize or
a protocol session. Older supported revisions initialize, retain
`Mcp-Session-Id` and the negotiated protocol version, then send
`notifications/initialized`. See [generic setup](mcp-capabilities.md#generic-client-setup)
for both lifecycle examples. `tools/list` returns the complete current catalog
by default; optional `_meta["detent/toolsets"]` selects groups. Call the
discovered `work_list` with `{"project_id":"YOUR_GRANTED_PROJECT_ID","limit":20}`.
Verify the result's organization/project. No browser state or OAuth login is
required. GET/SSE returns 405; clients that require SSE/OAuth or cannot send
private bearer headers are unsupported. Report this explicitly and stop.
An optional GET receiving 405 is supported by the tested SDK.

A concise agent handoff is:

> Configure your supported MCP client for the exact copied organization endpoint
> using the same expiring scoped key from Settings → API & MCP. Ask me to store
> it privately as DETENT_API_KEY; never include it in conversation, URLs, logs,
> screenshots, diagnostics or committed configuration. Use bearer JSON POSTs
> with no cookies, CSRF or OAuth. If your client cannot do this, say it is
> unsupported and stop. Use the lifecycle supported by your client, discover
> tools/list and call work_list for a project granted to that key. Verify the context
> and report success without secrets. Use local client permissions for sensitive-operation confirmation;
> the Hub executes authorized calls directly.

The official TypeScript SDK client used for verification was
`@modelcontextprotocol/sdk` **1.31.0**. In a private client directory containing
that dependency, the following uses the same private environment key:

```js
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const client = new Client({ name: "detent-agent", version: "1.0.0" });
const transport = new StreamableHTTPClientTransport(new URL(process.env.DETENT_MCP_URL), {
  requestInit: { headers: { Authorization: `Bearer ${process.env.DETENT_API_KEY}` } },
});
await client.connect(transport);
let cursor;
const tools = [];
do {
  const page = await client.listTools(cursor ? { cursor } : {});
  tools.push(...page.tools);
  cursor = page.nextCursor;
} while (cursor);
if (!tools.some((tool) => tool.name === "work_list")) throw new Error("Authorized read unavailable");
const result = await client.callTool({
  name: "work_list",
  arguments: { project_id: process.env.DETENT_PROJECT_ID, limit: 20 },
});
if (result.isError) throw new Error("Authorized read denied");
await client.close();
```

## Connection evidence and release verification

Pre-merge verification on October 1, 2026 used an isolated, ephemeral HTTPS
shared entry with a provisioned real organization tenant, a trusted fixture CA,
a browser-created 7-day Read key and one project grant. This exercises the real
Cloud entry/proxy/assertion, API-token authentication, membership provider,
project read and MCP handlers. The fixture provider replaces the external
identity service; this is not production deployment evidence.

Both project and work-item GETs returned **200** with the shared key and no
cookies or CSRF. The SDK initialized, discovered **55 tools** across all pages,
and `work_list` returned the authorized project. The copied API prompt's actual
curl examples also passed without browser state. Revoking the key denied the
next API GET and MCP request on the established session. Focused tests cover
current roles and grants, organization isolation, expiry, revocation, membership
removal/replacement, machine rejection, redaction and current authority.

Isolated Chromium verified the generated page at the existing bookmark,
key creation/revocation, masked one-time secret, complete API/MCP URLs, both
secret-free copied prompts, and desktop/mobile layout. Chrome DevTools was
unavailable in this worker. Screenshots were captured after dismissing secrets.

**Pending release acceptance:** Detent owns integration; the Cloud release owner
owns deployment and a repeat against the actual canonical Cloud organization
endpoint. After deploying the PR head, create a temporary Read key for one test
project on this page. Run the direct reads and a supported SDK/MCP client's
initialize/discovery/read with that same key and no browser state; record only
status, negotiated protocol, tool count and public organization/project context.
Verify both copy actions and key management on the deployed page. Revoke the
key and confirm subsequent requests fail. Do not record secrets or claim this
production check passed from the fixture result alone.

For 401, check private key configuration, expiry and revocation. For 403 or a
hidden 404, check current membership, role, key scope and project grants. Local
and self-hosted daemon instructions remain expandable on the same settings page.

## Native work-item list results

Native `work_list` accepts item limits through 200 and returns summaries in
`data.items` with an opaque `data.next_cursor` when more matching items remain.
The requested limit is a maximum: the existing issue page owner may return fewer
items to fit its 64 KiB serialized summary/cursor budget, derived from the 256 KiB
tool-result limit and reserving room for the envelope and MCP serialization.
Pass the cursor with the same project, query, state and label; the limit may
change. Items group in configured lane order, then use priority and newest
activity within nonterminal lanes, or newest activity within terminal lanes,
with identifiers breaking ties. Matching identities and order remain fixed while
paging, even if items change; item details remain current. Start a fresh query to
observe new membership or ordering. Cursor authority and one-hour expiry remain
intact.
Both stdio and HTTP return the same shape.

Local GitHub snapshot lists use the same lane comparator, with the snapshot's
`updated_at` as activity. Their existing offset pagination reads the current
snapshot on each request.

Summaries retain stable IDs, identifiers, URLs, revisions, title, state, priority,
labels, assignees, timestamps and authorized dependencies/blockers. Their
`omitted_fields` is `["body", "linked_source", "provenance",
"external_references", "change"]`; the empty body and null external-reference
placeholders mean omitted detail, not an empty original resource. Use `work_item`
or `work_export` for complete authorized detail under their existing byte bounds.
Queries still search full authorized original bodies. A single summary whose
metadata exceeds the page budget returns the existing safe unavailable error.

## Native validation evidence

Native `work_item`, `work_attempt_receipt` and `get_change` include a bounded
`validation_audit` projection of recorded local commands and scheduled failure
occurrences. Scheduled failures retain their existing issue/comment owner;
comparisons name those source records, their authors, and the run, job, attempt, Change and
version identities. The projection reads the selected project's database only
and makes no forge requests. `partial` identifies truncated records or candidate
comparisons; missing historical receipts, scope or environment remain unknown.

Command receipts retain the tested head/tree, result and duration. Commands
that emit `detent-check-evidence` records also supply their declared check scope,
command, Go host OS/architecture/toolchain and per-check result. Check durations
emitted by the shell recorder identify their one-second resolution. Dirty source
observations cannot establish the tested tree. The scheduled reporter retains
these records under the existing occurrence identity rather than substituting
the finalizer's environment for a failed job's environment.

A local-pass/scheduled-failure comparison requires identical recorded trees,
matching scope and command, matching observed environment, and a local pass
recorded before the failed check. Environment equality covers the recorded
fields only. Integrated commit equality is reported separately: an earlier
Change is only a candidate, and inclusion or causality is never inferred from
ancestry. Local output is omitted from the comparison projection and private
command text is redacted. Correlation grants no workflow authority, introduces
no merge requirement and does not change any project's check policy.

## Hosted application context reads

Hosted dedicated and shared connections expose `app_bootstrap_payload`,
`app_updates`, and `hosted_events` through the same adapter on HTTP and stdio.
`app_bootstrap_payload` takes `{}` and returns the current organization/directory,
actor, readable projects and workspace capabilities, support context, features,
coordinator availability, model/effort/access preferences, plan summary and hub
version. Browser CSRF, form tokens and session credentials are omitted. API keys
see only their current project grants and model catalogs.

`app_updates` takes `{}` and returns hub, minimum-runner and client builds, runner
versions, online/behind/refusal context and the behind count. It requires a
non-viewer member with runner management on every organization project and an API
key, when used, that can read every project. Removing a role or grant takes effect
on the next call and discovery; an old catalog grants no access.

`hosted_events` takes `project_id`, optional `workspace_id`, and optional `cursor`.
It returns one current collaboration invalidation sequence, an optional safe
workspace resource, `observed_at`, `freshness: "current_observation"`, `changed`
and a cursor bound to that organization/project/workspace. The first observation
sets `changed: true`; sending its cursor again sets it to false when both observed
revisions match. A project activity or workspace revision change sets it to true.
This is a current invalidation observation, not event history: callers can use the
existing work reads to refresh affected content. No heartbeat, SSE stream or
background polling is exposed. Each call checks current project/workspace access;
a cursor grants no authority. Use a new observation when changing scope.

These reads reject unknown fields and requests over 64 KiB. Identifiers are at
most 256 bytes and event cursors at most 2048 bytes. Project, organization-choice, model-choice and runner lists are
at most 200 items and all results at most 256 KiB; an oversized complete result
returns a safe unavailable error instead of truncation. Disabled optional services
are explicit in bootstrap; a required unavailable service returns no application
data or raw diagnostics. Hosted context is omitted from self-hosted and credential
maintenance discovery. Parent #3259 generic-client and zero-pending acceptance
remains with the parent owner.

## Staging MCP deployment smoke

After the origin smoke, `deploy-staging.yml` runs the MCP call pass against
the staging tenant on `staging.cloud.detent.build`. The operator provisions a dedicated
native throwaway project on that staging tenant and stores its expiring Write
key in the repository secret `DETENT_STAGING_API_KEY`. Set the existing
`DETENT_MCP_URL` variable in the staging deployment environment to the exact MCP
URL copied from that tenant’s API & MCP setup; shared tenants include their
organization mount. The probe refuses production or other origins. Restrict the key to that
one project; the probe discovers its ID through `list_projects` and refuses a
key that can see multiple projects. Do not store the key or project ID in source
or issues.

The project needs a permitted transition between two nondispatchable,
nonterminal states. The probe creates one item per deployment and reuses a
separate dependency fixture. It checks edits, comments, dependencies, movement,
queue operations, unchanged integration settings, and archive/restore revisions.
Read tools requiring existing runner, workspace, change, artifact or attempt
resources need those fixtures in the smoke project, linked to the stable
`MCP smoke dependency` item where applicable. Discovery reads run before their
resource detail calls; missing fixtures fail with
the tool name and advertised schema. Unsafe external and operator mutations are
explicitly skipped. Native archive and queue availability gaps are reported as
expected gaps owned by #615 until its hosted support lands.

An absent secret produces an explicit skip and permits deployment while the
operator provisions it. A skip is not live acceptance evidence. With the secret
present, failures stop the staging smoke and use the existing High deployment
failure reporter; the deployment artifact retains each tool's result. Production
deployment and local merge validation do not run this call pass.
