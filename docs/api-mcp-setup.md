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

Native Hub operators can call `stop_run` directly with `project_id`, `request_id`,
`identifier`, `expected_revision` (a decimal string), `lease_id`, `fencing_token`
(an integer), and `destination` (`Blocked`, `Backlog`, `Cancelled`, or `Todo`).
Use the current item revision and exact active lease/fence from the supported
work-item and attempt reads. Optional `priority` uses ranks 1–4; `reason` accepts
up to 280 bytes. The existing stop owner checks current write access,
revision, workflow transition and lease fence atomically, preserves retained
source, and releases only that attempt. The returned `action_id` is readable
through `action_result`; its result records the released lease/fence and the
item's resulting state and revision. Reuse the same `request_id` and arguments
for a retry. This operation requires no conversation or coordinator turn and
does not grant access to private conversations.

On an enrolled runner host, discover the `local_projects` toolset on the
runner's authenticated `/mcp` connection. `local_project_configuration` reads the actual
local revision and redacted selected/effective policy. Its admin commands apply
an approved policy, drain current work or detach one settled migrated
project. They use the existing operator command API and authority and receipt owner;
see [managed local project configuration](mcp-capabilities.md#managed-local-project-configuration)
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

## Policy approval without the web app

<!-- BEGIN POLICY APPROVAL RULE -->
Repository configuration reports do not require approval before dispatch.
Trusted workflow definitions from the bound repository's default-branch history
apply automatically. Explicit approval is required for administrative
configuration adoption, such as applying an external local definition or changing
local-binding permission. Configuration identity changes do not revoke active
leases or invalidate recovered source.

On a self-hosted Hub without the web app, read `get_project_policy` and
`get_onboarding`, then use `approve_project_policy` with the current
`expected_policy_id` and the chosen `observed_policy_id`. The REST equivalent is
GET and PUT on `/api/v2/organizations/{organization}/projects/{project}/policy`,
with GET on the project's `/onboarding` for observed candidates. Approval needs
administrator authority and preserves revision checks. Cloud owners and admins
can continue approving in **Settings > Integrations**.
<!-- END POLICY APPROVAL RULE -->

A difference between the selected and effective configuration remains diagnostic
information; it does not by itself block claims, revoke leases or invalidate
recovered source. The configuration identity records what an attempt used.
Legacy configuration-management operations remain separate from dispatch and
still require their documented administrative authority and revisions.

Use the Hub's organization-scoped MCP connection (the `projects` toolset) with
an Admin key authorized for the project. An enrolled runner's credential reports
configuration; it cannot approve it. No configured project list on the runner
is needed. Use the native project ID, such as `prj_example`.

1. Call `get_project_policy` with `{"project_id":"prj_example"}`. Its `data`
   contains the current approval; copy `data.policy.policy_id` for
   `expected_policy_id`. If it returns `policy_mismatch` because no approval
   exists, use an empty string for first approval.
2. Call `get_onboarding` for the same project. Inspect
   `data.observed_policies` and the selected descriptor, source revision and
   reporting runners. Choose its `policy.policy_id` as `observed_policy_id`.
   Conflicting or previously approved reports are not new approval candidates;
   resolve the configuration conflict rather than approving a stale report.
   An empty candidate list can mean the trusted report already applied; read
   the current policy again instead of inventing a candidate.
3. Call `approve_project_policy` using the same project and a unique
   `request_id`, with the following `input` (replace the example policy IDs
   with the values just read; use `""` for `expected_policy_id` on first approval):

   ```json
   {
     "project_id": "prj_example",
     "request_id": "approve-shared-configuration-1",
     "input": {
       "expected_policy_id": "policy_current",
       "observed_policy_id": "policy_selected"
     }
   }
   ```

   Omit `input.policy` when approving by reference. Alternatively, supply the
   complete, unchanged `local_project_configuration.selected_policy` as
   `input.policy`, preserving every field; do not reconstruct its identity.
4. Read `get_project_policy` again to confirm the approved identity and
   provenance. Approval and runner adoption are separate: use
   `local_project_configuration` to inspect adoption if applying a local
   configuration. On a revision conflict, read the current state and review
   the candidate again before submitting a new request. Exact retries use
   the same `request_id` and unchanged arguments.

For a bearer-authenticated self-hosted Hub, use the instance administrator
credential for REST approval. GET
`/api/v2/organizations/{organization}/projects/{project}/policy` returns the
approval directly (`policy.policy_id`); GET the same project's `/onboarding`
returns `observed_policies` directly. PUT to `/policy` takes the two fields
inside `input` above as its top-level JSON body, without the MCP envelope.
The Hub resolves the stored descriptor and checks `expected_policy_id`
atomically. A stale expected identity returns HTTP 409; reread before replacing
it. On hosted Cloud, the web app uses the owner/admin
`/onboarding/policy` route; the instance-admin `/policy` PUT is not served there.

The `hub policy` CLI resolves a configured project from a global project list;
it is not the approval path for enrolled runners. Use the API/MCP procedure
above for a native project ID. The legacy policy-revocation operation returns
HTTP 410; pause or drain the runner, or remove project access, to stop execution.

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
intended shared files to the bound repository's default branch so the runner
can report and apply that revision automatically. An authored `source_revision`
digest is not a Git commit.

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

## ChatGPT and Claude custom connectors (Detent Cloud)

Use `https://cloud.detent.build/mcp` with OAuth. No API key, custom header,
client ID or client secret is needed. Detent uses dynamic client registration
and the authorization code flow with S256 PKCE. Your normal Cloud sign-in,
workspace membership, role, project grants and organization key policy apply.
A reporter can file issues and use the same tools as with a personal key.

Registration returns a sealed client ID without storing an unused client.
Pending authorization lives in one encrypted browser cookie, expires after ten
minutes, and creates no server rows. Registration bodies are limited to 64 KiB,
with at most 20 redirect URIs and a 2,048-character client ID; the pending
cookie is limited to 3,800 characters. Only authenticated consent stores the
client, grant and single-use authorization code. Anonymous registration and
pending requests therefore need no database quota or cleanup sweep.
Registration and authorization share the existing API token-bucket limiter:
120 requests per minute per client IP with a burst of 30. Excess requests get
HTTP 429 with `Retry-After: 60`; tokens refill over time. Only the loopback TLS
proxy is trusted to append `X-Forwarded-For`; client-supplied prefixes cannot
change the caller's quota. This uses the same limiter implementation as API
key authentication, without adding a separate throttling or cleanup mechanism.

### ChatGPT

1. Open ChatGPT Plugins, select the plus button, then **Add custom MCP server**.
   In older interfaces, enable developer mode and use Settings → Apps (or
   Connectors). Your plan and workspace policy must permit custom servers.
2. Enter a name and `https://cloud.detent.build/mcp`, and select OAuth and
   dynamic client registration if prompted. Leave client credentials empty.
3. Connect, sign in to Detent with your usual WorkOS login, choose the workspace
   to grant, review the client and return address, and select **Connect assistant**.
4. In a conversation, enable the connector, discover its tools, select your
   organization and project, and ask it to file an issue. Confirm the issue is
   attributed to your account.

See [OpenAI's connection steps](https://developers.openai.com/plugins/deploy/connect-chatgpt)
and [MCP authentication guide](https://developers.openai.com/plugins/build/auth).
The published callbacks are
`https://chatgpt.com/connector_platform_oauth_redirect` for issuer-aware servers
and `https://chatgpt.com/connector/oauth/{callback_id}` for callback-specific
connections. Detent returns the issuer on authorization responses.

### Claude (claude.ai or Claude Desktop)

1. Open Customize → Connectors → Add → Add custom connector (Organization
   settings → Connectors for workspace owners). Enter a name and
   `https://cloud.detent.build/mcp` as the remote MCP URL, then continue.
2. Choose **Sign in now** for authentication and **Register automatically**
   for the OAuth client when those choices are shown, then add the connector.
   No client ID or secret is required.
3. Sign in to Detent, choose the workspace, review the client and return
   address, and select **Connect assistant**.
4. Enable the connector in a conversation, discover its tools and select your
   organization and project before filing an issue. Confirm your account is
   recorded as the author.

See [Claude's custom connector setup](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp)
and [authentication requirements](https://claude.com/docs/connectors/building/authentication).
Claude's hosted connector callback is `https://claude.ai/api/mcp/auth_callback`,
including connections from Claude Desktop.

### Grant lifetime and disconnecting

An access token lasts one hour. Refresh tokens rotate on each use and expire
30 days after issuance. Reusing a consumed refresh token revokes the grant.
Rotation replaces the grant's single stored refresh credential. Sealed token
identity and expiry preserve replay detection without retaining token history;
expired tokens are refused without revoking a newer valid credential.
Revoking a grant makes its next MCP request return 401, including requests
using an existing `Mcp-Session-Id`.

Signed-in owners can `GET /api/cloud/account/oauth-grants` and
`DELETE /api/cloud/account/oauth-grants/GRANT_ID` (same-origin browser request).
Personal-key and OAuth MCP clients can use `list_oauth_grants` and
`revoke_oauth_grant` to list or revoke their own grants (revocation requires
write scope). Listings include the client, workspace, connection time, last use
and revocation time, never tokens.
Pass a returned `next_cursor` as `cursor` to retrieve subsequent pages.
Organizations that block personal keys or require administrator approval also
refuse new assistant grants; ask an organization administrator about its policy.

Bearer API keys remain supported for Claude Code and Codex, including their
existing expiry, rotation, revocation and access restrictions.

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


## Attachments

Issue and comment attachments are stored privately by the Cloud entry; clients never receive bucket URLs, storage credentials or presigned URLs. Dedicated and self-hosted Hubs without the entry's attachment store omit the attachment tools.

Upload with `POST /organizations/ORG/api/v2/projects/PROJECT/attachments` (browser) or `POST /api/v2/organizations/ORG/projects/PROJECT/attachments` (API clients), sending one multipart `file` part or the file body with `Content-Type` and `X-Attachment-Name`. Browser mutations need the organization CSRF token in `X-CSRF-Token` and the same origin; API keys use `Authorization: Bearer`, and read keys cannot upload or delete. The response carries a random `att_` identifier and a markdown `reference`: `![name](attachment:att_<id>)` for images and `[name](attachment:att_<id>)` for other files. Embedding the reference in an issue or comment body binds it in the same transaction; a reference can be reused across items and comments in the same project. `POST .../attachments/ATTACHMENT/reference` with `work_item_id` and optional `comment_id` binds an upload explicitly.

Files are limited to 20 MiB. PNG, JPEG, GIF and WebP images (up to 16 megapixels) are re-encoded to remove metadata; GIF and WebP become PNG. PDF, UTF-8 plain text, Markdown, CSV, valid JSON, `.log` console logs and `.har` network exports are stored as downloads. SVG, HTML, mismatched text and malformed images are refused. Stored bytes count toward the organization's `collaboration_bytes` allowance and remain counted until object deletion is confirmed; a quota refusal returns `allowance_exhausted`.

Projects can opt in to `require_visual_attachments` through the project
[content settings API](hub-api.md#project-content-settings). It defaults to false.
When enabled, each external visual source in a new or changed issue title/body,
comment body, Change title/body, Change discussion/review body or authored review
criterion-evidence reference must share its paragraph, list item or table cell with a readable
uploaded image or PDF from that project. Put one source in each block, for example:

```markdown
[Design reference](https://figma.com/design/example) ![Screenshot](attachment:att_<uploaded-id>)

Screenshot: https://example.test/preview [Uploaded PDF](attachment:att_<uploaded-pdf-id>)
```

The canonical attachment Markdown returned by Cloud is also supported. A file
elsewhere in the content or on another issue/comment does not satisfy a source;
text/log uploads, deleted files, fabricated IDs and cross-project references do
not qualify. Attachment-only content remains supported. Metadata confirms type,
dimensions and digest, but cannot establish that the upload depicts the source.

Detection parses Markdown links and images (including reference links),
autolinks, bare HTTP(S) URLs and HTML image sources. It ignores inline and fenced
code. External image embeds, decoded paths ending in `.png`, `.jpg`, `.jpeg`,
`.gif`, `.webp`, `.avif` or `.svg`, exact `claude.ai` artifact paths
(`/artifacts/…` or `/public/artifacts/…`) and exact `figma.com`/`www.figma.com`
`design`, `file`, `board` and `proto` paths qualify. Query strings and fragments
do not affect path detection. Arbitrary destinations qualify when introduced
by `Screenshot:`, `Screenshots:`, `Design reference:`, `Design board:`, `Mockup:`
or `Wireframe:`, or those exact Markdown link labels (case-insensitive, with an
optional final colon). Ordinary documentation, PDF links, repository/PR/CI
links, Claude chats and report context URLs do not qualify merely by being URLs.
Unlabeled redirects and arbitrary board URLs cannot reliably be recognized;
use an explicit label. Detent performs no network probing or semantic matching.

Refused writes return HTTP 422 `invalid_request` or the same structured MCP
refusal, naming the field and block without repeating customer content or URLs:
“This project requires uploaded screenshots and design references. Upload the
file to Detent and include its attachment reference beside each visual source.”
The entire write rolls back. Metadata-only edits, unchanged content, comment
deletion and previously accepted idempotency/provenance replays remain allowed.
Changed fields are validated in full, including retained old visual sources.
Changing the setting affects subsequent writes, never historical records or
dispatch. New Change version `artifacts` and check-result `evidence` also apply
the policy. A visual artifact uses the existing `artifact` kind and a Detent
attachment URI (`attachment:att_<uploaded-id>` or its canonical project attachment
path), the uploaded file's recorded SHA-256 and `availability: "available"`.
External visual URIs are refused even beside an unrelated uploaded artifact.
Code, diff, manifest, test, log and checkpoint artifacts keep their existing
meaning. Ordinary repository, PR, CI and documentation identities remain valid.

Accepted Change references bind transactionally to the Change's primary work
item, using the existing attachment access and retention rules. Those bindings
survive issue text edits and subsequent policy toggles. Only newly submitted
fields are inspected; stored versions/reviews, source patches, raw command/log
output, signatures and archival records are not rescanned. A reference neither
approves a design nor records acceptance inspection.

`GET .../attachments/ATTACHMENT` reads the file under current organization and project authority; other organizations, other projects and requests without a session or valid key get 404, so a copied link grants no access. `offset` and `length` query parameters return a bounded base64 chunk instead of the file. `GET .../attachments/ATTACHMENT/metadata` returns type, size, SHA-256, dimensions and `referenced_by` without object bytes. `DELETE .../attachments/ATTACHMENT` hides the attachment and deletes the object. An attachment with no remaining references is removed after seven days; referenced attachments do not expire.

| MCP tool | Input and result |
| --- | --- |
| `upload_attachment` | `project_id`, `name`, `content_base64`, optional `content_type`; the attachment metadata and `reference`. The 64 KiB MCP argument limit allows about 44 KB of file content; larger files use the upload API or `detent attach`. |
| `read_attachment_metadata` | `project_id`, `attachment_id`; recorded type, size, SHA-256, dimensions and `referenced_by` |
| `read_attachment` | The same selector, optional `offset` and `length` (1 to 32,768 bytes, default 32 KiB); `metadata`, `content_base64`, `offset`, `returned_bytes`, `eof` and `max_content_bytes` |
| `reference_attachment` | The same selector, `request_id`, `work_item_id` and optional `comment_id`; the binding receipt |
| `delete_attachment` | The same selector and `request_id`; a receipt whose `object_deletion` is `pending` or `confirmed`, also readable through `action_result` |

Reads require project read authority; binding and deletion require project write authority. `detent attach screenshot.png --project prj_example` uploads with the configured Cloud client and prints the markdown reference; `--hub-url`, `--organization` and `--token-env` select an explicit endpoint and key.

Native issue agents read attachments with `read_attachment_metadata` and `read_attachment` under their execution authority, and upload workspace files with `attach_evidence` (a workspace-relative `path` and a single-line `caption`), which publishes an evidence comment through the runner's existing `events` grant. Paths outside the workspace, escaping symlinks, nonregular files, unsupported types and files over 20 MiB are refused. Native coding and rework workers also publish up to ten screenshots from `.detent/validation/` as a validation evidence comment.

An organization owner can read or replace the organization's archive storage binding with `GET` or `PUT /api/cloud/organizations/ORG/archive-storage`, supplying `expected_revision`, `backend` (`hosted` or `s3`) and, for `s3`, an HTTPS `endpoint`, `region`, `bucket`, optional `prefix` and a `credential_reference`. Secret values are never sent to this API; the entry resolves them from its service environment. Existing archive references keep the binding they were written with. `GET /api/cloud/organizations/ORG/archive-deletion` returns the retained deletion receipt after organization deletion.

## Organization URLs

Copy the URLs shown on this page. On shared Cloud their complete shapes are:

```text
https://cloud.detent.build/organizations/ORG/mcp
https://cloud.detent.build/organizations/ORG/api/v2/organizations/ORG
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
DETENT_API_BASE='https://cloud.detent.build/organizations/ORG/api/v2/organizations/ORG'
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


## MCP results and refusals

Hosted allowance refusals are MCP tool results with `isError: true`, rather than
transport failures. Their JSON content includes `code: "allowance_exhausted"`,
`http_status: 429`, `resource`, `allowance`, `consumption`, a safe message and
recovery guidance. Current MCP versions also return this as `structuredContent`.
Inspect `billing_usage.hosted_plan_page` with owner/admin authority or
`billing_usage.billing_export` with owner authority and resolve the affected
allowance. A refusal does not report a
successful write. `policy_mismatch` directs callers to `get_project_policy` to
refresh approved policy inputs; uncertain mutation outcomes still require resource
inspection before retrying. Unexpected failures remain redacted as
`service_unavailable` with a correlation ID.

Native `verify_fix` answers a Done issue whose `verification.state` is `pending`: `result: verified` records the verifier, and `result: not_fixed` requires `comment`, which may embed `upload_attachment` references, and reopens the issue to Todo. Only the filer, or a member and above, may answer. `work_list created_by=me verification=pending` lists the caller's issues awaiting verification.

Native `file_issue` accepts `type: bug` or `type: change_request`. Reporters must provide one; other roles may omit it. `edit_item` changes the type for members, and `work_item` and `work_list` return it when set. `work_list type=change_request` filters by the stored classification independently of labels.

Native `file_issue` may return `possible_duplicates` and a `challenge_token`
without creating an issue. Repeating `file_issue` with the same `request_id` and
content, the returned `challenge_token` and `not_duplicate_of` listing the
challenged issue numbers creates the issue. The challenge grants no filing or
issue-contract confirmation authority. See the [native filing challenge contract](hub-api.md#native-collaboration-protocol)
for expiry, replay and related-history behavior.

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

Acceptance validators inspect attachments through `read_attachment`, reading
each supported attachment from offset 0 to obtain a review-bound inspection
receipt; ordinary implementation and rework reads do not issue receipts. The
inspection bound is 32768 bytes of UTF-8 `text/plain` per attachment, so larger
evidence is published as ordered parts. In acceptance validation, unsupported
MIME such as `application/json` returns `inspection_error`, and oversized text
returns a bounded preview with `truncated: true` and `inspection_error`; neither
supplies `read_receipt`.

Reporter filings enter Triage and require nonempty sections. Bugs use `Steps to reproduce`, `Expected`, `Actual`, `Where` (screen or URL), and `Environment` (browser/OS and app build; use available context). Change requests use `Current behavior`, `Desired behavior`, and `Why`. Missing or empty sections return `invalid_request` with the missing headings and create no issue. `work_config` returns `reporter_templates`, a map from each type to its effective required headings. It also returns `issue_contract.required_sections`, the effective engineering headings required for executable work. Both come from the same approved project policy, including workflow overrides. When the approved policy delegates bounded scheduled repair, `issue_contract.scheduled_repair` also exposes its `policy_id`, configured `repository`, `workflow`, authorized `branches`, declared `priority` (0 Urgent, 1 High), generated contract `body`, and `evidence_schema`. Native reports must carry the scheduled evidence stamp and pass independent repository CI verification before that contract can be confirmed. The repair template must match the effective required sections exactly. Configured workflows may authorize an exact branch or explicitly authorize the `release/v*` family; other wildcards are refused. The legacy CI delegation retains `develop` and the `release/v*` prefix family. Origin stamps may retain the corresponding GitHub run URL without an attempt suffix; an explicit attempt must match the independently verified evidence. These reads do not confirm issue criteria or grant permission to dispatch. The New issue form pre-fills these headings when a type is selected.

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

An omitted type keeps its default template. Triage is exempt from the engineering issue contract; after promotion into the configured admission lane, the existing contract check requires the member-authored engineering sections and moves the issue to Blocked with a human action when they are missing or unconfirmed.


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
url = "https://cloud.detent.build/organizations/ORG/mcp"
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
required. GET/SSE returns 405; self-hosted clients that require SSE or cannot send
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

For an interactive session, a client started from a project whose direnv
environment supplies the key handles Codex's confirmation prompt before the MCP
call reaches Detent:

```sh
direnv exec . codex --no-daemon --sandbox workspace-write --ask-for-approval on-request
```

`--no-daemon` runs the session separately with the current environment (Codex
CLI 0.162.1; see `codex --help` for the installed version). Choosing **Always
allow** for a tool persists a per-tool preference in the Codex user
configuration, normally `~/.codex/config.toml`:

```toml
[mcp_servers.detent.tools.update_runner_routing]
approval_mode = "approve"
```

The general key is `mcp_servers.detent.tools.<tool>.approval_mode`. It records a
client preference only and grants no Hub role, key scope or project access. A
fresh `codex exec` loads it by default, so a saved tool approval also applies
under `approval_policy = "never"`. See the
[Codex configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference).

`connection_info` reports the connected account and organization. Detent
executes authorized writes directly and returns their result; there is no
Detent approval URL or pending approval.
See the [Codex MCP configuration reference](https://developers.openai.com/codex/mcp/)
for client settings and [client confirmation and current authority](mcp-capabilities.md#client-confirmation-and-current-authority)
for the server contract.

A minimal client using the official TypeScript SDK (`@modelcontextprotocol/sdk`
1.31.0) with the same private environment key:

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

## Connection errors

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

## Publish a prepared release-pick PR

The project-scoped `publish_release_pick` operator tool uses the
[native publication contract](hub-api.md#publish-a-prepared-release-pick-pr).
Supply the dependent `work_item_id`, current `expected_revision`, stable
`request_id`, and `release_pick` source/repository/head/base/frozen identities
without a PR number for initial publication. For a settled delivery that refused
`base_moved`, supply its existing `pr_number` and the refreshed exact base/head,
retaining its source, repository, branches and frozen SHA. The prepared branch
must already contain exactly one `-x`
pick of the audited, landed source fix on the frozen release base.

The tool uses the existing GitHub transport and returns the exact PR identity.
An identical retry recovers one matching open PR after a lost create response.
It refuses stale refs, holds, conflicting PRs and unauthorized scope. It grants
no approval or deployment authority and does not admit the issue. Associate the
returned PR separately, then use normal native audit and landing.

## Associate an existing release-pick PR

The project-scoped `associate_release_pick` MCP tool uses the supported
[native release-pick delivery contract](hub-api.md#existing-release-pick-delivery).
Supply the dependent `work_item_id`, its `expected_revision`, a stable
`request_id`, and the exact source/PR/release identities in `release_pick`.
The source dependency must already be terminal and its fix merged on develop.
The owner verifies the repository and commit provenance, opens an independent
native Change, and preserves all holds. The existing runner captures the exact
source, reviews, gates, and lands on the recorded frozen release branch.
The tool does not merge or deploy. Worker completion contracts do not grant
permission to call this operator mutation.

The release coordinator handles `base_moved` by preparing the same pick on the
current head of the same frozen release branch, verifying the existing PR through
`publish_release_pick`, then refreshing through `associate_release_pick`. It uses
the current delivery revision and waits for active execution to settle. Refresh
retires the current version binding while retaining prior immutable evidence and
returns the existing Change to source capture and fresh validation, review and
audit. On interruption, read the association first; replay unchanged request
identities when needed. Never retry the old tuple indefinitely or reset a release
branch. Terminal deliveries cannot be refreshed or reopened by this path.

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
When a review cannot fit with its required envelope, a `section: "reviews"`
continuation returns `fragment` using the retained recovery record format:
scoped item, record and version identities, byte offset, total byte length,
SHA-256 and base64-encoded canonical stored JSON. Each fragment carries at most
64 KiB of record bytes. Follow `read.sections.reviews.next_cursor` on the same
Change until the record is complete; verify contiguous offsets, stable identity,
length and digest before using its review. Fragmented records retain full command
output. Section completion describes traversal progress and never makes a partial
record passing evidence. Change cursors retain their selected Change, version,
head and history boundary, including for a non-latest Change on the same item.
Other unrepresentable records or envelopes still return `change_record_too_large`
without record content or passing evidence. Every serialized result remains
within the existing 256 KiB budget.
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

Organization productivity is available through the hosted organization
`GET /reports?scope=organization&project=all&range=30d` read and the existing `reports` MCP tool
with `{"scope":"organization","range":"30d"}`. Presets are `24h`, `7d`,
`30d` and `90d`; REST custom windows use inclusive UTC `from`/`to` dates,
and MCP accepts explicit timestamps aligned to complete UTC hours. Presets use the same
calendar window resolution as project Reports. Both return an equal-length
previous window. Buckets are hourly for short windows and daily for the 7d, 30d and 90d
presets, and custom windows at least six days long.

The organization result contains `current` and `previous` productivity metrics,
`projects` with the same metrics per project, `series` and `previous_series`
with filed/completed counts and project splits, and `filed_by`/`completed_by`
counts for both periods. Completion cost includes lifetime usage before each
window's end for issues currently Done whose close falls inside that window.
Rework uses the project Reports unique-worked-issue denominator and excludes
infrastructure returns from its numerator. Missing cost or rework denominators
and unobserved cycle times return null rates or durations.

The `labels`, `priorities`, `types`, `filed_by`, `runners` and `projects`
filters accept arrays in MCP and repeated query parameters in REST. Dimensions
combine with AND; values within a dimension combine with OR. Project filters
intersect current readable grants and cannot expand access. A single MCP
`project_id` further narrows organization scope. Omitting REST `scope` retains
the existing Reports client response, including for `project=all`. Project-scoped
Reports retains its existing response. Organization reads use typed background-maintained
summaries through the reader pool; they may lag writes and never parse attempt
payloads or replay event history on the request path. Filer kinds are `people`,
`agents`, `automation` and `unknown`; completion attribution uses the final
attempt's enrolled runner or the human who recorded the Done transition, with
`unknown` retained when identity is missing.

Hosted dedicated and shared connections expose `app_bootstrap_payload`,
`app_updates`, `release_notes`, and `hosted_events` through the same adapter on HTTP and stdio.
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

`release_notes` takes optional `limit` (default 20, maximum 100) and `cursor`.
It returns the embedded builds newest first, entries, internal counts, running
and operator-patch flags, per-user unread state, `has_unread`, and an optional
`next_cursor`. Viewers can read it. It shares the hosted release-notes API owner
and performs no external requests; marking seen uses the authenticated browser
API. Bootstrap also includes `has_unread`.

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
maintenance discovery.

## Native refusals

Native projects refuse `order_item` and `set_queue_priority` with
`invalid_request`: `Queue ordering and priority overrides require a
GitHub-compatible project; native projects use issue priority`. A project
without a cutover receipt returns `not_found`: `No cutover receipt exists for
this project`. `get_change_review_policy` returns `not_found`: `No approved
Change review policy exists for this project` until a policy is approved.
`get_project_policy` returns `policy_mismatch` beginning with `No approved
repository policy` before the first approval. Missing session activity returns
`not_found`: `No recorded session activity exists for this work item and
attempt` from `board_session_history`.
