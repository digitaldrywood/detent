# API & MCP setup

Sign in to the intended Cloud organization and open **Settings → API & MCP**.
Existing `/settings/mcp` bookmarks open this same page. Create a named key,
choose Read, Write or Admin, select only the required projects, and choose an
expiry (7, 30 or 90 days in the UI; the creation API accepts 1–90 days).
Copy the once-displayed key directly to a private secret store or environment
as `DETENT_API_KEY`. The page lists scope, project IDs, expiry, fingerprint and
revocation state; it never retrieves an existing secret. Revoke keys here.
Removing client configuration does not revoke a key.

The same key authenticates direct API requests and MCP. Effective permissions
are the intersection of its scope/project grants and the issuing user's current
provider membership, local membership, role and project grants. Removing or
replacing membership, losing project access, expiry and revocation deny later
calls, including calls on an already initialized MCP session. An API key cannot
change organizations or impersonate another user. Runner and worker credentials
remain separate and cannot connect as operators.

Key management requires the issuing user's browser session and CSRF state.
API and MCP client connections require only `Authorization: Bearer`, without
browser cookies, sessions or CSRF. The browser approval surface still requires
its existing signed-in session, CSRF and exact-action token. A key cannot approve
its own material/destructive operation. Approval rechecks the originating key's
current authority, so revocation before confirmation prevents execution.

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
collaboration and Changes. [Capability coverage](mcp-capabilities.md) retains
#3259 as the separate parity tracker.

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
requirements. For material/destructive operations, present the supplied
preview/approval URL for the user to review in their signed-in browser. When a
direct route has no approved preview flow, stop and ask the user to perform that
operation in Detent; do not substitute an unapproved mutation. Keys cannot write
orchestrator lane state.

A concise agent handoff is:

> Connect directly to the copied Detent API base for this organization. Have me
> create/select an expiring shared key on Settings → API & MCP with Read scope
> and the required projects. Ask me to put it directly in your private secret
> store/environment as DETENT_API_KEY, never this conversation. Send
> Authorization: Bearer privately with no cookies or CSRF. Run the two documented
> project/work-item GETs above for a granted project and verify the context.
> Follow Hub API request schemas and current authorization. Present existing
> material-action browser approvals; if no approved direct flow exists, ask me
> to do the operation in Detent. Report success or an access error without secrets.

## MCP agent handoff

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
`Accept: application/json, text/event-stream`. Initialize, retain
`Mcp-Session-Id` and the negotiated protocol version, send
`notifications/initialized`, and follow every `tools/list` cursor. Call the
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
> unsupported and stop. Initialize, send initialized, discover every tools/list
> page and call work_list for a project granted to that key. Verify the context
> and report success without secrets. Present material/destructive previews for
> my signed-in browser approval; you and the key cannot approve them.

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
removal/replacement, machine rejection, redaction and browser approval.

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
