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

On an enrolled runner host, discover the `local_projects` toolset on the
runner's authenticated `/mcp` connection. `local_project_configuration` reads the actual
local revision and redacted selected/effective policy. Its admin commands apply
an approved policy, drain current work or detach one settled migrated
project. They use the existing operator command API and authority and receipt owner;
see [managed local project configuration](mcp-capabilities.md#managed-local-project-configuration-native-94)
for exact revision, policy and handoff requirements. Cloud also reaches the
enrolled runner's configuration owner through its existing heartbeat transport.
Select `runner_id` when more than one granted
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

For an external local definition without trusted repository provenance, the
approved authored Cloud descriptor supplies shared behavior. Private overlays
remain on the host and do not prevent adoption. Read `selected_policy` as the
preview, then apply its exact approved policy ID and source revision after work
settles. Application preserves host execution settings and the prior pause or
drain state; it does not rewrite definition files or runner routing. The durable
Cloud approval supplies the same shared behavior on subsequent reloads and
restarts. `applied: true` confirms runtime adoption; `saved: false` means no local
configuration file was changed. Cloud readback retains the completed receipt,
including refusal constraints, in `last_operation` after later heartbeats.
Trusted repository definitions retain their repository authority: commit the
intended shared files at the configured source revision and approve that
descriptor. An authored `source_revision` digest is not a Git commit.

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

## Account and organization service keys

Personal keys are user-owned and stored in the shared entry authentication store.
The account API creates and lists them at `/api/cloud/account/api-keys` and
revokes them with `DELETE /api/cloud/account/api-keys/:key`. These management
routes require the signed-in account's browser session and same-origin writes.
Open **Account settings → API keys** (`/settings/api-keys`) to create, rotate
or revoke your personal keys, inspect effective reach today and last use, and
copy secret-free MCP or direct API setup prompts. A key never grants more than
its owner has. `GET /api/cloud/account/key-context` supplies the current
organizations, roles, permitted projects and single multi-org MCP endpoint.
The key list includes current effective reach and last use. Rotation uses
`POST /api/cloud/account/api-keys/:key/rotate`: it replaces the token immediately,
retains the key's scope, expiry, approvals and org blocks, and returns the new
secret once. Update clients after saving it privately.

Creation accepts `name`, `permission` (`read`, `write`, `admin`),
`access_context` (`global`, `selected`, `project`), `organizations`, and either
`expires_days` (1–90) or `never_expires: true`. Each organization entry contains
`organization_id`, `project_access` (`all`, `selected`, `project`) and
`project_ids`. Selected project access requires at least one ID; project access
requires exactly one. A global personal key reaches all current and future
organizations, with all projects by default; explicit organization entries can
narrow individual organizations. Selected context reaches only the supplied
organizations. Project context requires exactly one organization and one project.
The secret is returned only on creation or rotation.

Use one bearer key with `https://cloud.detent.build/mcp`. Project and organization
tools accept `organization_id` alongside their existing project selectors.
`organization_list` lists reachable organizations; discovery returns their tool
union. Calls recheck the target organization's current role, grants and plan.
For REST, `GET /api/v2/organizations` lists reachable organizations; select the
target with `X-Detent-Organization` for `/api/v2/projects` and
`/api/v2/projects/:project/...`. Explicit `/api/v2/organizations/:organization/...`
and existing per-org MCP endpoints also accept a key that reaches that org.
Browser-only settings routes continue to require browser sessions.

Personal keys act as their human owner. Saving an issue body through REST,
MCP `edit_item`, or a CLI using a personal key confirms its issue-contract
sections. Collaboration actors record `kind: human`, the owner's principal,
and `via_key`; audit records retain the owner and key ID. Service, runner and
worker credentials cannot confirm issue contracts. Revoking a key refuses
subsequent calls without removing earlier human confirmations.

Historical cards with unconfirmed sections require an authorized human to
save the current body through the web UI or a personal key. An unchanged body
save confirms those sections. Key migration does not backfill confirmations;
service, runner and worker keys cannot perform this confirmation.

Org owners/admins manage service keys at
`/api/cloud/organizations/:organization/service-keys`, using the same creation
fields with one organization and selected or project context. Service keys are
org-owned and remain valid after their creator leaves; permission and project
context still restrict each call.

Open **Org settings → API & MCP** to manage service keys, member keys, personal
key policy and the approval queue. Non-admins see only a link to their own
account keys; the management APIs enforce current owner/admin authority.

Org owners/admins read or set `personal_keys` (`allowed`, `approval`, `blocked`)
through `/api/cloud/organizations/:organization/key-policy` (GET/PUT). The
default is `allowed`. `approval` refuses personal keys until an admin approves
each key with `PUT external-keys/:key/approve`; `blocked` refuses all personal
keys, including keys scoped to one project. The old `allow_external_keys` write
field remains accepted, mapping true to `allowed` and false to `blocked`.
Existing false policies migrate to `blocked`.

`GET /api/cloud/organizations/:organization/external-keys` includes active
personal keys whose owner still belongs to the org and whose scope includes
it, including unused keys. It returns only this org's reach and last use.
Pending entries form the approval queue. `PUT external-keys/:key/block` with
`{}` revokes access to this org without affecting other orgs;
`{"project_id":"..."}` blocks a single project. An approval never clears an org
block. The owner's account receives a notice through
`/api/cloud/account/key-notifications`. Calls and policy, approval and block
changes are audited in the target org with key and owner identity.

Personal keys intersect their scope with the owner's current provider and local
membership, role, project grants and entitlement on each call. Membership loss,
key expiry, revocation and org blocking take effect on the next call. Named
refusals include `key_organization_denied`, `key_project_denied`,
`membership_denied`, `key_permission_denied`, `role_denied`,
`organization_key_policy_denied`, `organization_key_blocked` and `key_inactive`.

## Existing per-organization keys

Previously issued org keys keep working through their existing API and MCP
endpoints. Shared Cloud uses the account key screen for newly issued personal
keys and the org screen for service keys. Existing org keys remain available
through `GET /api/v2/organizations/:organization/api-keys` and can be revoked
through `DELETE /api/v2/organizations/:organization/api-keys/:key`.

Existing keys created from a member's account settings are personal keys;
runner and worker credentials remain service credentials. Per-organization
key creation accepts `kind: personal` (the default for signed-in account
creation) or `kind: service`. Both retain the existing permission,
organization and project grant checks. Returned key metadata includes `kind`.

On dedicated deployments, sign in to the intended Cloud organization and open **Settings → API & MCP**.
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
calls, including calls on an already initialized MCP session. An existing per-org API key cannot
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

## Platform credit administration

Platform administrators use the shared entry's `/api/cloud/platform/mcp`
endpoint for `platform_adjust_ai_credits`. Authenticate each request with the
existing entitlement-administrator service bearer token, or with an account
session authorized by the platform entitlement-administrator check and its
`X-CSRF-Token`. Organization API keys and organization-owner sessions cannot
grant credits. The platform endpoint uses the same MCP protocol as the
organization endpoints and advertises only the platform credit command.

The tool accepts `organization_id`, `idempotency_key`, `amount_usd` and `reason`.
For example:

```json
{
  "organization_id": "org_example",
  "idempotency_key": "complimentary-pilot-1",
  "amount_usd": "10.00",
  "reason": "Complimentary pilot credits"
}
```

Use a signed decimal string for `amount_usd`: positive to grant credits,
negative to correct the balance, with at most six fractional digits. Preserve
the key, amount and reason when retrying. The result contains the organization's
current balance in USD micros. The tenant writes the ledger and balance on its
open database; the command requires no tenant restart or database file access.
See [Platform AI credit adjustments](hub-api.md#platform-ai-credit-adjustments)
for the REST equivalent, bounds and audit contract.

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

Hosted allowance refusals are MCP tool results with `isError: true`, rather than
transport failures. Their JSON content includes `code: "allowance_exhausted"`,
`http_status: 429`, `resource`, `allowance`, `consumption`, a safe message and
recovery guidance. Current MCP versions also return this as `structuredContent`.
Inspect `billing_usage.hosted_plan_page` with owner/admin authority or
`billing_usage.billing_export` with owner authority and resolve the affected
allowance before submitting another business action. A refusal does not report a
successful write. `policy_mismatch` directs callers to `get_project_policy` to
refresh approved policy inputs; uncertain mutation outcomes still require resource
inspection before retrying. Unexpected failures remain redacted as
`service_unavailable` with a correlation ID.

Native `verify_fix` answers a Done issue whose `verification.state` is `pending`: `result: verified` records the verifier, and `result: not_fixed` requires `comment`, which may embed `upload_attachment` references, and reopens the issue to Todo. Only the filer, or a member and above, may answer. `work_list created_by=me verification=pending` lists the caller's issues awaiting verification.

Native `file_issue` accepts `type: bug` or `type: change_request`. Reporters must provide one; other roles may omit it. `edit_item` changes the type for members, and `work_item` and `work_list` return it when set. `work_list type=change_request` filters by the stored classification independently of labels.

`edit_item` replaces the body with the complete reviewed text; it never truncates
or merges partial content. Its body limit is 64 KiB of UTF-8, and the complete
serialized JSON arguments, including escaping and other fields, must fit 64 KiB.
An existing body above 32 KiB can be corrected when the full replacement fits
that request bound. For larger native replacements, use the supported
`PATCH /api/v2/organizations/{organization_id}/projects/{project_id}/work-items/{work_item_id}`
with `idempotency_key`, `expected_revision` and the complete `body`, under the
same write authority and project grants. That application command accepts a
body up to 256 KiB and a request up to 1 MiB. Personal keys confirm the supplied
body's issue-contract sections; service and worker keys preserve existing
confirmations without confirming changed sections. Comment and filing MCP
limits remain 32 KiB. See [native collaboration](hub-api.md#native-collaboration-protocol)
for the application request contract.

Native `file_issue` also accepts an optional `report_context` object with `url` (at most 2048 bytes) and `app_build`, `browser`, `os`, `viewport` and `captured_at` (at most 256 bytes each), all single-line strings. Unknown keys, oversized or non-string values are refused with `invalid_request`, and the refusal never repeats the submitted values. `work_item` returns the stored context; list and board reads omit it. A Reporter bug whose `report_context` carries `browser` and `app_build` satisfies the template's Environment section. Upload network HAR exports (`.har`) with `detent attach` or the REST attachment upload API, because `upload_attachment` accepts only about 44 KB; use `upload_attachment` for small console log exports (`.log`). Read either back with `read_attachment`.

Reporter filings enter Triage and require nonempty sections. Bugs use `Steps to reproduce`, `Expected`, `Actual`, `Where` (screen or URL), and `Environment` (browser/OS and app build; use available context). Change requests use `Current behavior`, `Desired behavior`, and `Why`. Missing or empty sections return `invalid_request` with the missing headings and create no issue. `work_config` returns `reporter_templates`, a map from each type to its effective required headings. The New issue form pre-fills these headings when a type is selected.

Projects can override either template in WORKFLOW, using the same list syntax as the engineering issue contract:

```markdown
## Reporter Contract
### bug
- **Steps to reproduce** — A reproducible sequence.
- Expected
- Actual
- Where
- Environment

### change_request
- Current behavior
- Desired behavior
- Why
```

An omitted type keeps its default template. Triage is exempt from the engineering issue contract; after promotion into the configured admission lane, the existing contract check requires the member-authored engineering sections and uses the existing Blocked human-action recovery when they are missing or unconfirmed. Preserve the Reporter sections as evidence.


For native `file_issue` with `github_issue_url`, read `get_project_integration`
and `get_native_project` for the selected project. The URL must match its
attached repository or associated runner checkout. A missing association returns
`invalid_request` with the existing setup owner: an administrator uses
`bind_native_repository` with the observed integration `expected_revision`,
`repository` and `source: "runner_checkout"`. The association supplies the
repository and clone URL to authorized runners, which clone using their own Git
credentials and report the matching origin. The integration read returns
`checkout_verified` once an active runner has reported that checkout recently;
a binding alone does not verify it. Cloud needs no repository credentials and
GitHub tracker mode stays disabled. Choose a configured
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

### Codex connection and confirmation failures

Distinguish client startup, client confirmation and Hub authorization before
changing the connection. These failures have different owners:

| Symptom | Cause and supported next step |
| --- | --- |
| `Environment variable DETENT_API_KEY for MCP server 'detent' is not set` | The process hosting MCP lacks the configured environment variable. A shared Codex app-server may have started before the project environment loaded. Loading direnv in a later terminal does not update that process. Start a separate client from the loaded project environment; do not restart a shared daemon serving other sessions. |
| `MCP tool call requires approval, but approval policy is never` | Codex rejected the call before contacting Detent because this tool still requires client confirmation. The user must select an interactive session to review it. A previously saved tool-specific approval can allow later calls without a prompt; repeating the refused call or reconnecting to Detent does not approve it. |
| A Detent response reports `403` or insufficient scope/grants | The request reached the Hub, which refused the current authority. Check the key scope, selected project and account grants through the existing Settings and connection reads. Client confirmation does not grant server permissions. |

For an interactive operator session, the user can run the following from a
project whose direnv environment already supplies the private key:

```sh
direnv exec . codex --no-daemon --sandbox workspace-write --ask-for-approval on-request
```

These options are supported by Codex CLI 0.162.1; check `codex --help` for the
installed version. `--no-daemon` runs this session separately with the current
environment. The user handles Codex's confirmation prompt before the MCP call
reaches Detent. An agent running under a policy that forbids approval must not
change that policy to retry a refused operation. Noninteractive `codex exec`
can perform permitted reads with the loaded environment, but its ability to
read does not establish permission to execute write tools. Inspect the actual
session settings when resuming; do not assume the original sandbox or a changed
user request altered its confirmation policy.

In that interactive session, the user can choose **Always allow** for a specific
tool they intend to authorize on subsequent calls. Codex persists that selection
in its user configuration, normally `~/.codex/config.toml` (or
`$CODEX_HOME/config.toml`). For example, selecting it for `update_runner_routing`
on the server named `detent` saves:

```toml
[mcp_servers.detent.tools.update_runner_routing]
approval_mode = "approve"
```

The general key is `mcp_servers.detent.tools.<tool>.approval_mode`. This records
the user's client preference for that tool; it grants no Hub role, key scope or
project access. Keep the selection specific to the intended tool rather than
approving every tool on the server. A fresh `codex exec` loads the user
configuration by default and can use the saved approval even with
`approval_policy = "never"`, since that call no longer needs a prompt. Check for
a different `CODEX_HOME`, configuration overrides or `--ignore-user-config` if
the preference is absent. An existing session may retain its earlier settings;
start a separate session with the intended configuration and loaded environment
instead of restarting a shared daemon. See the
[Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference)
for per-tool approval settings.

After connecting, verify the selected account and organization with
`connection_info`, then read the intended project before making an authorized
change. Detent executes authorized writes directly and returns their result;
there is no Detent approval URL to open or pending approval to release. Preserve
truthful tool annotations and scoped credentials. Do not relabel writes as
read-only, change approval settings without the user's authorization or broaden
grants to conceal a client failure.
See the [Codex MCP configuration reference](https://developers.openai.com/codex/mcp/)
for client settings and [client confirmation and current authority](mcp-capabilities.md#client-confirmation-and-current-authority)
for the server contract.

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

Native `work_list` accepts `created_by` as a principal ID or `me`. `me` selects
issues filed by the calling human, including through that human's personal key;
service keys and worker credentials cannot use `me`. The selector combines with
state, label, query and archived filters within the caller's authorized project.
`work_item` and `work_list` expose the available filer's display identity in
`actor.display_name` beside the stable principal ID.

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

Native `work_bulk_export` exports complete stored items and comments from one
project. It accepts `states`, `labels`, `open` and `archived` with the same OR/AND
and archive semantics as `work_list`. Pages default to 500 items, accept limits
from 1 through 500, and retain full bodies, confirmed contract sections,
authorized dependencies/blockers, linked-source snapshots, provenance and
external references. The top-level `items`, `total`, `truncated`, `next_cursor`,
`generated_at` and `freshness` describe the export. Pass the cursor with the same
project and filters; the limit may change. A page may contain fewer items to fit
its 16 MiB serialized budget. An individual item exceeding that budget is
refused explicitly; no item content is silently shortened. Item and comment
revisions expose later edits. Reads use stored data and never refresh GitHub.
This tool requires the same read scope and current project grants as `work_item`.
Existing tools retain their limits and response shapes.

## Native validation evidence

`get_change` defaults to the current immutable version and current review,
check and discussion records. Its summary always evaluates all stored current
gate evidence. `read.version_id` identifies the selected evidence version;
`read.sections` gives explicit completeness and `next_cursor` for versions,
reviews, checks and discussion. Follow each cursor with `get_change` using
the same project, work item and Change selectors. A cursor fixes the selected
version and record boundary and is rejected if the current head changes.
An omitted record is not passing evidence. `section: "versions"` traverses
historical versions excluding the selected version already returned by the
default read; `version_id` selects exact historical evidence. Existing
`get_change_version` reads remain available. Bounded Change reads retain policy
identity, digests, gate requirements and review/check expectations, while omitting
authored file contents, runtime configuration and workflow definitions.
`read.policy_details_omitted` discloses this projection; retrieve the full immutable
policy through `get_change_version`. Policy omission never establishes approval.
Bounded Change reads return command
output as UTF-8 excerpts of at most 4 KiB, setting `output_truncated: true` when
output is shortened and preserving any existing truncation flag. Verdicts,
criteria, findings, command results and receipt identities remain complete;
section completeness describes records, not the completeness of output text.
The canonical scoped Change HTTP GET without `view=bounded` retains the full
stored reviews and output; exact-version reads also retain stored output.
Records whose remaining semantic evidence exceeds the budget still return
`change_record_too_large` without record content or passing evidence. The result
budget remains 256 KiB.
Native workers use the same projection through the Change HTTP read with
`view=bounded` and the same section, cursor, version and limit selectors.

Native `work_attempt_receipt` includes a bounded `validation_audit` projection of
recorded local commands and project scheduled failure occurrences. A `work_item`
whose body contains scheduled check evidence includes only that item's scheduled
reports and their candidate comparisons. Ordinary work-item and bounded Change
reads omit this audit; their own content and command receipts remain available.
Scheduled failures retain their existing issue/comment owner;
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

## Deployment MCP smoke

After the origin smoke, `deploy-staging.yml` and `deploy-production.yml` run the
MCP call pass against a dedicated smoke tenant on `staging.cloud.detent.build`
and `cloud.detent.build`. The operator provisions, in each environment, a
dedicated org owned by the platform account with one native throwaway project,
and stores a key restricted to that project as the environment secret
`DETENT_SMOKE_API_KEY`. Set the environment variable `DETENT_MCP_URL` to the
exact MCP URL copied from that tenant's API & MCP setup. The probe refuses any
other origin for the environment, discovers the project ID through
`list_projects`, and refuses a key that can see more than one project. Do not
store the key or project ID in source or issues.

The project needs a permitted transition between two nondispatchable,
nonterminal states. The probe creates one item per deployment and reuses a
separate dependency fixture. It checks edits, comments, dependencies, movement,
queue operations, unchanged integration settings, and archive/restore revisions.
Provision the persistent resources using [deployment MCP fixtures](deployment-mcp-fixtures.md)
once in each environment. The smoke discovers the conversation by title and
the attachment, change, artifact receipt and attempt through the stable
`MCP smoke dependency` item. Discovery reads run before their resource detail
calls; missing fixtures produce explicit read skips with recorded reasons.
Unsafe external and operator mutations are explicitly skipped. The two GitHub
import reads are also skipped with a named reason: a native smoke project has
no external tracker import.
The operator-only `get_change`, `get_change_version` and `get_native_run` detail
reads are skipped because they exceed the dedicated project key's authority.
Available fixture reads run, and tool errors still fail deployment.
`runner_project_diagnostics` selects the dependency attempt's runner explicitly,
including when multiple runners report the project.
`get_project_policy` accepts either an approved policy or the documented
`policy_mismatch` refusal beginning with `No approved repository policy`;
other refusals fail the smoke.

Native projects refuse `order_item` and `set_queue_priority` with
`invalid_request`: `Queue ordering and priority overrides require a
GitHub-compatible project; native projects use issue priority`. These commands
remain available for GitHub-compatible projects. A project without a cutover
receipt returns `not_found`: `No cutover receipt exists for this project`.
`get_change_review_policy` returns `not_found`: `No approved Change review
policy exists for this project` until a policy is approved. Missing session
activity returns `not_found`: `No recorded session activity exists for this
work item and attempt` from `board_session_history`. The smoke accepts only
these named refusals for these tools; other errors still fail deployment.
When integration reads report automatic native intake, the unchanged-settings
update omits `intake` to preserve it under the update contract.

Tools that are known to fail are listed in `smokeKnownGaps` in
`tools/cifailure` with their owning issue. They are reported but do not fail the
deployment. Any other failure fails it, and so does a listed tool that starts
passing, so the list only shrinks. An absent secret fails the smoke. Failures
use the existing High deployment failure reporter, and the deployment artifact
retains each tool's result.
