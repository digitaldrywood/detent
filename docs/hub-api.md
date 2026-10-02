# Detent Hub API

Detent Hub owns its SQLite database and exposes fleet coordination through an authenticated HTTP API. Clients must never open or copy the live database files.

This page documents implemented behavior, including native collaboration, Changes and scoped runner enrollment through `/api/v2`. See [self-hosted operations](hub-self-hosting.md) for deployment, export/import and recovery, and [artifact deployment](artifacts-deployment.md) for independent durable storage. The [native Hub and Cloud RFC](cloud-hub-rfc.md) defines the broader architecture.

Cloud members get one expiring scoped key for direct API and MCP access from
**Settings → API & MCP**, at the preserved `/settings/mcp` bookmark. See
[API & MCP setup](api-mcp-setup.md) for organization URLs, tested requests,
private credential handling, agent prompts and the browser approval contract.
Cloud keys do not authorize compatibility, worker, runner or instance-admin routes.

## Approved repository policy

Every API claim requires an approved `policy_id`. Compatibility claims also
supply exactly one `repositories` entry; native claims use their authenticated
organization/project scope. Policies are resolved on the customer host. See
[configuration precedence and approval](config.md#repository-policy-with-hub-execution).

| Endpoint suffix | Method | Authority and payload |
| --- | --- | --- |
| `/api/v1/repositories/{owner}/{repo}/policy` | `GET` | Read current descriptor and approval provenance with a compatibility credential |
| `/api/v2/organizations/{organization}/projects/{project}/policy` | `GET` | Read current descriptor and approval provenance within native project grants |
| Either policy endpoint | `PUT` | Instance administrator only; `policy` descriptor and `expected_policy_id` (empty for first approval). Exact retries are idempotent; concurrent distinct replacements have one winner. |
| Either policy endpoint | `DELETE` | Instance administrator only; `expected_policy_id`. Revokes current authority while retaining revision/lease history. |
| `/api/v2/organizations/{organization}/projects/{project}/policy/observed` | `POST` | A runner with the `heartbeat` operation reports the descriptor it resolved when nothing is approved or the approved policy differs. The Hub keeps the latest report per project and offers it for approval as `observed_policy` in `GET .../onboarding`; it grants nothing by itself. |

Hosted organizations approve through `PUT .../projects/{project}/onboarding/policy`
(owner or admin); the generic `PUT .../policy` above stays instance-administrator
only and is not served in hosted mode. A runner reports each new unapproved
descriptor once, so after the repository's `detent.yaml` or `WORKFLOW.md`
changes, project settings shows "Approve reported policy" with the runner's own
descriptor and nobody pastes JSON.

Descriptors reject unknown JSON fields and validate all metadata, hashes and
content-derived identities. Worker/operator tokens cannot approve or revoke a
policy. Native cross-project and cross-organization reads follow existing grant
checks. Approval is a reviewed authorization of repository files, not a settings
override. Commands, prose, paths, labels/check names and secrets are not accepted
as policy fields.

Claim allocation checks approval and requirements in the lease transaction and
persists the identity with the lease. Responses include `policy_id`; native run
events must match it. Missing/stale approval returns `409 policy_mismatch` before
allocation. Unsupported or unmatched requirements return `409 selector_no_match`.
Current policy cannot be replaced while active leases exist. Revocation denies
renewals and run events but permits release. Legacy rows migrate without policy
pins and cannot be reused through the API as approved execution claims.

## Start the Hub

Set a high-entropy bootstrap administrator token, then start the service on its loopback default:

```sh
export DETENT_HUB_ADMIN_TOKEN="$(openssl rand -hex 32)"
detent hub serve --database /var/lib/detent/hub.db
```

The bootstrap token is inserted only when the Hub database has no bootstrap credential. Later restarts do not replace a token that was rotated or revoked through the API.

The default listener is `127.0.0.1:7777`. A non-loopback `--listen` value is rejected unless either `--tls-cert` and `--tls-key` are both set or `--trusted-proxy` explicitly declares that a trusted reverse proxy terminates TLS. The proxy or host firewall must prevent clients from bypassing that proxy.

## Authentication

Send a bearer token with every control-plane and health request:

```text
Authorization: Bearer <token>
```

Tokens have one of three scopes:

- `worker` registers and heartbeats machines, claims work, renews or releases leases, and appends fenced events.
- `operator` reads state and performs typed workflow, dependency, priority, and queue-order mutations.
- `admin` can perform all operations and create, rotate, or revoke tokens.

Only SHA-256 token hashes and hash-derived fingerprints are stored. Plaintext is returned once by token creation or rotation responses, which carry `Cache-Control: no-store`. Token values are never included in Hub logs.

Create and rotate scoped tokens with:

```text
POST   /api/v1/tokens
POST   /api/v1/tokens/{id}/rotate
DELETE /api/v1/tokens/{id}
```

Enrollment redemption accepts its separate one-time bearer token, which cannot call other APIs. `/api/v1/webhooks/github` uses GitHub's `X-Hub-Signature-256` HMAC authentication and never accepts a Hub token as a substitute.

## Scoped runner onboarding

| Credential | Owner and lifetime | Revocation |
| --- | --- | --- |
| Enrollment token | Hub issues a grant for one organization, explicit projects, operations and, optionally, host-generated runner/machine IDs; valid for 1–900 seconds and one redemption | Administrator deletes the unconsumed enrollment; expiry/revocation does not end an enrolled session |
| Runner credential | Customer host generates a random 256-bit bearer credential; Hub stores its SHA-256 hash; valid for 24 hours from enrollment or renewal | Administrator revokes the runner; no resurrection by renewal, rotation, generic token rotation or ID reuse |
| Provider/repository/storage credential | Customer login, keychain, workload identity or private host configuration; may outlive many runner sessions | Customer revokes it at its provider; revoking Hub access does not revoke this credential |

Each logical runner has its own credential and an administrator-approved machine binding.
`runner_` and `machine_` IDs are generated independently of hostname/display name.
Renaming preserves both IDs. Reinstallation requires new IDs, a new credential
and explicit enrollment; existing IDs remain reserved after revocation. Another
enrollment or a legacy registration cannot silently take over an existing ID.
Copying the private identity file copies bearer authority: never clone it into
machine images or share it across hosts. Multiple logical runners on one host
share the same machine ID and capacity ceiling through explicit enrollment
approval. Hardware attestation is not implemented.

### Register a runner with one command

The Enroll dialog in the organization's runner settings asks for a display name
and the projects the runner may work on, creates a token-first enrollment, and
shows one command to run on the host:

```sh
detent hub runner register --url https://cloud.detent.build/organizations/org_example \
  --token det_enroll_example --name "Build host" --capacity 2 --service
```

`register` does, in order:

1. Generates the runner and machine IDs and a random credential on the host and
   writes them to `identity.json` beside the runner configuration (default
   `~/.config/detent-runner/`, private, outside any repository). An existing
   identity there is reused, so a retry never creates a second runner.
2. Redeems the token. The request carries the IDs, host name, display name,
   capacity, version, OS and architecture, and the host credential, which the
   Hub stores only as a SHA-256 hash. No provider or repository credential is
   read or sent.
3. Reads the names of the granted projects and writes `global.yaml` once: the
   `client:` block (`hub_url`, `identity_file`, `organization_id`,
   `native_projects`, `display_name`, `capacity`), `service_name:
   detent.runner`, and one `projects:` entry per project whose `workdir` is the
   project's checkout under `--workspace-root` (default `~/detent-runner/NAME`).
   An existing `global.yaml` is left untouched; checkout checks use its
   configured projects and `workdir` paths instead of `--workspace-root`.
4. With `--service`, installs and starts the `detent.runner` background service
   (launchd `com.digitaldrywood.detent.runner`, systemd `detent.runner.service`),
   separate from a local board's `detent` service on the same host. If a
   project's checkout is missing, it prints the clone step and the
   `detent start --config ... --yes` command to run afterwards instead.

`detent start` and `detent status` default to the runner service when the
configuration has a `client.hub_url`, including older configurations without
`service_name`. An explicit `service_name` selects that service. If launchd
cannot bootstrap a disabled label, the error names it and prints the
`launchctl enable` command to run before retrying.

The token appears in the command because it is single-use and expires within 15
minutes; once redeemed it grants nothing. To keep it out of shell history,
omit `--token` and set `DETENT_RUNNER_ENROLLMENT_TOKEN` instead. A self-hosted
Hub whose URL does not include `/organizations/ORG` needs `--organization`.
Projects listed in `client.native_projects` use the Hub in place of the
GitHub tracker their committed `detent.yaml` names, so the checkout needs no
local override. `register` starts the service only once every checkout has its
`WORKFLOW.md`, and refuses an existing identity or configuration that belongs
to another Hub, organization or project set rather than reusing it.

`init` and `enroll` below remain for scripted setups that bind the IDs before
the token exists.

An enrollment created without `runner_id` and `machine_id` is token-first: it
binds to the fresh IDs the host presents when it redeems the token, and the Hub
records them on the enrollment at that moment. The host still generates its IDs
and credential locally, the IDs must be unused, and the token can be redeemed
once. The steps below bind the IDs up front instead.

1. On the customer host, run `detent hub runner init --hub-url https://hub.example.com`.
   It prints only runner/machine IDs and stores the credential under the OS user
   config directory at `detent/runner/identity.json`. An explicit
   `--identity-file` must be an absolute private path outside repositories and
   ordinary workspaces. Initialization refuses to overwrite an identity.
2. Send those public IDs to an instance administrator. The administrator creates
   an enrollment using `POST /api/v2/organizations/{org}/runner-enrollments`:

   ```json
   {
     "runner_id": "runner_0123456789abcdef0123456789abcdef",
     "machine_id": "machine_fedcba9876543210fedcba9876543210",
     "project_ids": ["prj_0123456789abcdef0123456789abcdef"],
     "operations": ["read", "claim", "heartbeat", "events", "collaborate"],
     "ttl_seconds": 300
   }
   ```

   Use the actual IDs from initialization. The response contains `id`, `token`
   and `expires_at` with `Cache-Control: no-store`. Deliver the token through an
   approved private channel; never an issue, repository, shell argument or log.
3. Supply it in `DETENT_RUNNER_ENROLLMENT_TOKEN` for the enrollment command and
   run `detent hub runner enroll --organization org_example --display-name 'Build host'`.
   Remove the enrollment variable from the host environment after the command.
   `--enrollment-token-env` selects another variable name. No provider credential
   is read or sent. The command sends its separate host-generated credential
   over HTTPS, and the Hub atomically consumes the enrollment, registers the
   machine, creates hashed authentication and installs the exact project grants.
4. Configure the host scheduler using the private identity path:

   ```yaml
   client:
     hub_url: https://hub.example.com
     identity_file: /private/host-config/detent/runner/identity.json
     organization_id: org_example
     native_projects:
       example: prj_example
     display_name: Build host
   ```

   Substitute the real private path and organization/project IDs. `identity_file`
   and `token_env` are mutually exclusive. An optional `machine_id` must exactly
   match the enrolled ID. Every configured native project must be in the grant.
   The host retains its existing local model login/API-key configuration.

| Operation grant | Permitted native operations |
| --- | --- |
| `read` | Capability negotiation and authorized project/issues/comments/history reads |
| `claim` | Claim, renew and release fenced work leases in authorized projects |
| `heartbeat` | Refresh only the authenticated machine's registration/heartbeat |
| `events` | Submit typed, fenced run events for this runner's own leases |
| `collaborate` | Create/edit issues and comments, transition workflow and edit dependencies within authorized projects |

Grants must explicitly list at least one project and operation. No empty list
means “all.” Enrolled runners cannot use v1, global health/outbox APIs, tenant
administration or token-management endpoints. Administrators may replace project
access through runner routing settings; operation grants require fresh enrollment.
Generic token grants cannot widen runner authority. Claims, lease mutations and
events check the individual runner's binding, organization, project and fencing,
including when another runner uses the same host. A supplied `machine_id`, tag,
capability or hostname never changes authority.

## Named runners, host capacity and eligibility

Repository `runners.profiles` declare requirements; they do not register hosts,
assign tags or grant access. The approved policy descriptor pins the selected
profile and requirements to each lease. Runner and host names are descriptive;
selectors always use stable `runner_` and `machine_` IDs.

| Selector | Deterministic behavior |
| --- | --- |
| Required tags | Every required tag must be present in administrator-owned runner tags. Tags are trimmed, lowercased, sorted and deduplicated at configuration/edit boundaries; canonical tags are lowercase ASCII tokens, at most 64 characters, with at most 32 distinct tags. |
| Runner and machine IDs | Every supplied constraint must match exactly, together with all tags. Surrounding whitespace is trimmed in repository configuration; IDs are case-sensitive canonical lowercase tokens. Names never participate. |
| Empty selector | Any otherwise authorized and eligible executor may claim. Legacy registrations retain this compatibility behavior. |
| Unknown valid tag or ID | Work stays queued with `selector_no_match`; there is no fallback or selector widening. Invalid syntax is rejected during policy validation. |
| Renames | Administrator renames preserve stable IDs, grants, tags and active leases. Worker registration/heartbeat cannot overwrite approved names. |
| OS/architecture | Reported at enrollment, registration and heartbeat for operator visibility. Reports are capabilities, not grants or privileged routing tags. |

Selection, health and capacity are checked within the atomic Hub claim transaction.
An enrolled runner requires a heartbeat newer than two minutes; timestamps in the
future fail closed. All unreleased, unexpired leases on a machine count against its
shared host ceiling, including leases held by other runners and projects. Each
runner also has an administrator limit and a reported local capacity; the lower
value applies. The first enrollment initializes the host ceiling from its capacity;
subsequent shared enrollment and worker reports cannot raise it. Existing local
pool, project and backend admission limits still apply in addition to Hub limits.

To initialize another runner on the same actual host, choose a separate private
destination and refer to the existing enrolled identity:

```sh
detent hub runner init --hub-url https://hub.example.com \
  --identity-file /private/detent/release-runner.json \
  --host-identity-file /private/detent/build-runner.json
```

This generates a new runner ID and credential, retaining only the machine ID.
The administrator must set `shared_machine: true` in the new enrollment request.
The host must already belong to that organization. Omitting explicit shared-host
approval preserves collision rejection; neither an enrollment token nor a new
credential can take over another runner's leases. Never copy a credential onto
another computer to simulate a shared host.

| Endpoint | Authority and behavior |
| --- | --- |
| `GET /api/v2/organizations/{org}/runners` | Instance administrator: fleet routing, host identity, platform, health, capacity and active-run eligibility. |
| `GET /api/v2/organizations/{org}/runners/{runner}/routing` | That enrolled runner or instance administrator: public routing and active leases; no secrets. |
| `PUT /api/v2/organizations/{org}/runners/{runner}/routing` | Instance administrator: `expected_revision`, `display_name`, `tags`, `state`, `capacity_limit`, `project_ids`. Replaces routing/access atomically; stale revisions return `409 revision_conflict`. Empty project access denies every project. |
| `PUT /api/v2/organizations/{org}/machines/{machine}/routing` | Instance administrator: `expected_revision`, `display_name`, `capacity`. Capacity is shared by all bound runners. |
| `POST /api/v2/organizations/{org}/projects/{project}/leases/{lease}/validate` | Owning runner: `fencing_token`. Revalidates authority, selectors and pinned policy within one transaction. The customer scheduler checks the returned binding against its private local identity before adopting or renewing work. |

The Fleet page links to runner list/detail and project eligibility. Project views,
including Runs, link to the same eligibility details. Runner detail includes active
run policy, selectors, lease expiry and contextual authority exclusions. The configured
Hub administrator connection can edit routing; an enrolled worker connection sees
its own runner without edit controls. Dashboard mutation authentication still applies.

| Change or exclusion | New dispatches | Active leases |
| --- | --- | --- |
| Draining | `runner_draining`; queued | May validate, renew and finish. |
| Disabled | `runner_disabled`; queued | Validation, renewal and new Hub mutations are denied. |
| Required tag removed | `selector_no_match`; queued | Affected validation, renewal and run events are denied. Unrelated tag edits preserve eligibility. |
| Project access or credential revoked | No authority to dispatch | Further affected operations are denied; ownership never transfers to another runner on the host. |
| Capacity reduced/full | `runner_capacity` or `host_capacity`; queued | Existing leases retain occupancy until release/expiry; no new lease exceeds the limit. |
| Offline exact target | `runner_offline` or incompatible-caller `selector_no_match`; queued | Existing fencing and expiry remain authoritative. |

Routing refusals do not create work attempts, consume failure retries or change the
issue to Failed. The scheduler preserves their structured code as a scheduling
deferral. Revocation is enforced on Hub operations immediately and on local execution
at the next pre-start/renewal check. Work already executing on a disconnected host
cannot be remotely undone; loss of renewal authority cancels the local attempt.
Routing state, project access, host capacity and individual lease ownership survive
Hub restart. The schema migration preserves existing identities and lease history.

## Runner renewal, rotation and revocation

| Endpoint | Authority | Behavior |
| --- | --- | --- |
| `POST /api/v2/organizations/{org}/runner-enrollments/redeem` | Enrollment bearer | Strict host identity/credential/machine metadata body; one transactional winner; replay, expired/revoked grant and wrong binding return 401; collisions return 409 |
| `DELETE /api/v2/organizations/{org}/runner-enrollments/{id}` | Instance admin | Revoke an unconsumed grant |
| `GET /api/v2/organizations/{org}/runners/{runner}` | That runner | Read its public identity, grants and expiry; no credential value |
| `POST /api/v2/organizations/{org}/runners/{runner}/renew` | That active runner | Body `{}`; retain credential and extend expiry to 24 hours from Hub time |
| `POST /api/v2/organizations/{org}/runners/{runner}/rotate` | That active runner | Body `{"credential":"<new host-generated credential>"}`; replace the hash and extend expiry atomically; previous credential stops working immediately |
| `DELETE /api/v2/organizations/{org}/runners/{runner}` | Instance admin | Revoke all future authenticated access for this identity |
| `POST /api/v2/organizations/{org}/projects/{project}/machines/{machine}/heartbeat` | Owning runner with `heartbeat` | `display_name`, `capacity`, `version`, optional `os`/`architecture`; enrolled runners update reported capacity/platform and liveness, preserving administrator-owned names and limits |

Hub time decides validity, with an inclusive start and exclusive expiry boundary.
An invalid clock or time before issuance fails closed. Times are parsed before
comparison, including fractional-second boundaries. There is no grace period or
offline renewal of expired credentials: generate and enroll a new host identity.
Clients renew automatically before requests when fewer than 12 hours remain.
`detent hub runner renew` forces renewal; `detent hub runner rotate` generates
and persists a replacement before sending it to the Hub. Credential-management
operations cannot grant additional projects or operations.

The host file includes a pending credential during rotation. A restart first
tries that credential, then retries the same rotation with the old credential
only if the pending one is unauthorized. A lost enrollment response is recovered
by authenticating the already-persisted credential, without redeeming again.
File locks serialize local credential maintenance; atomic private-file replacement
preserves recovery state. Identity responses, audit records and CLI output never
contain runner credentials. Identity audit rows record enrollment, renewal,
rotation and revocation using stable actor IDs and Hub timestamps.

Every request checks expiry and revocation against the owner database; there is
no authentication cache. An already authenticated in-flight operation may finish.
The enrolled scheduler treats subsequent authentication/authorization failure
during lease maintenance as lost ownership, invoking its existing stop path.
Previously granted leases remain fenced and expire through the existing lease
recovery mechanism; revocation never transfers a running lease to a new host.
It cannot forcibly erase credentials or stop an offline or hostile customer
machine. Stop that host and revoke provider/repository access separately if needed.

## Customer credential isolation and cleanup

Keep runner identity in the service account's private config directory (0700)
with a regular 0600 credential file. On Windows, provision the equivalent private
service-account/profile ACLs; POSIX permission bits do not enforce Windows ACLs.
Do not expose the runner identity file, enrollment variable, Hub admin token,
provider login directory or storage configuration through repository files,
ordinary workspaces, shared caches, artifacts or verbose shell tracing.
Back up private identity only into customer-controlled secret storage.

Use existing provider login or API-key facilities on the customer host. A trusted
backend wrapper should select only the required provider environment variables
or private credential mounts for the chosen backend. Run untrusted jobs under a
separate OS account/container/VM with a restricted filesystem and environment;
mount only that job's credential facilities. Environment variables and file mode
bits do **not** protect secrets from code running with the same identity and
execution context. Keep Hub runner authority in the control process's context,
separate from the job. Neither the Hub nor these enrollment commands fetch,
relay, store or validate provider/storage credentials.

Use operator-owned wrappers and the existing hooks as this cleanup contract:

- `hooks.before_run`: fail before execution if the approved sandbox, private
  credential mounts and environment allowlist cannot be established. Hook child
  process exports do not change the worker environment; perform injection in
  the trusted backend launcher. Never echo credential values.
- `hooks.after_run`: stop job processes, unmount credential facilities, remove
  private job scratch and discard job-only capabilities. Make cleanup idempotent.
- `hooks.before_remove`: verify teardown before ordinary workspace removal.
  Both cleanup hooks are best effort and log failures; they are not security
  boundaries or guaranteed crash cleanup. An operator-owned host janitor must
  reconcile orphaned sandboxes after worker/host failure before reuse.

Raw prompts, credential fields and artifact content are rejected by native event
schemas. Explicit user-authored comments remain content and must not contain
secrets. Encrypted provider-key provisioning through Hub is a future design
option requiring authenticated recipient keys and a browser/control-plane trust
model; no such relay is implemented.

## Legacy token migration

Migration 9 adds nullable expiry and separate enrollment/identity tables.
Existing worker/operator/admin tokens retain their scopes, hashes, grants and
non-expiring behavior; existing machine IDs and leases are not rewritten. A Hub
restart or upgrade does not disconnect installations. Generic token creation,
rotation and revocation still apply to legacy tokens; enrolled runners use the
dedicated endpoints above. Legacy shared tokens retain their previous trust
boundary and should only be used by mutually trusted clients.

Migrate deliberately: initialize and enroll a new identity, stop legacy claims,
allow existing leases to finish/release, switch native project configuration to
`identity_file`, and verify claims/heartbeats with the new IDs. Then revoke the
legacy token after every host sharing it has migrated. During overlap the old
token remains authorized under its old scope. Never convert a shared token in
place or reuse a legacy machine ID to transfer trust. Compatibility v1 projects
can continue using their legacy configuration until their native cutover.

## Work and fleet endpoints

| Endpoint | Minimum scope | Purpose |
| --- | --- | --- |
| `GET /health` | any scoped token | Service, schema, repository, and outbox health |
| `GET /api/v1/work-items` | any scoped token | Filtered, sorted work-item page |
| `GET /api/v1/work-items/{id}` | any scoped token | Normalized item, graph, PRs, lease, workpad, and event timeline |
| `POST /api/v1/claims` | worker | Atomically claim a requested item or the next compatible item |
| `POST /api/v1/leases/{id}/renew` | worker | Renew with the exact fencing token |
| `POST /api/v1/leases/{id}/release` | worker | Release with the exact fencing token |
| `POST /api/v1/work-items/{id}/events` | worker | Append a session event with the exact fencing token |
| `POST /api/v1/machines/register` | worker | Register or refresh a machine |
| `POST /api/v1/machines/{id}/heartbeat` | worker | Update heartbeat, capacity, version, or capabilities |
| `POST /api/v1/work-items/{id}/workflow` | operator | Change workflow state and enqueue its managed GitHub label |
| `POST /api/v1/work-items/{id}/dependencies` | operator | Add or remove a Hub-authoritative dependency |
| `POST /api/v1/work-items/{id}/priority` | operator | Change priority and enqueue its managed GitHub label |
| `POST /api/v1/work-items/{id}/order` | operator | Change Hub-authoritative queue rank |
| `GET /api/v1/repositories/freshness` | any scoped token | Repository synchronization health page |
| `GET /api/v1/outbox/health` | any scoped token | Outbox counts and operator-action page |

Worker events and operator mutations expose typed state changes only. They do not provide a generic GitHub request or arbitrary mutation surface.

The legacy event upload accepts `kind: progress` with an optional typed `payload.step`: `plan`, `implement`, `test`, `review`, or `complete`. Unknown payload fields and event kinds are rejected. Existing locally recorded v1 history remains readable; it is not copied into native event payloads. New integrations should use the versioned v2 schemas below.

## Work-item queries and cursors

`GET /api/v1/work-items` accepts repeatable or comma-separated filters: `repository`, `workflow_state`, `readiness`, `priority`, `label`, `assignee`, `machine`, `lease`, `pr`, and `sync_health`.

Supported sort values are `priority`, `created`, `updated`, `identifier`, and `workflow_state`; `order` is `asc` or `desc`. The default priority order uses priority, queue-rank presence, queue rank, creation time, repository owner/name, issue number, and internal ID. Every other sort also ends with repository owner/name, issue number, and internal ID, giving every page a stable total order.

List responses contain an opaque `next_cursor`. Pass it back with the same sort and order. `limit` defaults to 50 and may not exceed 200. Repository freshness and outbox health use the same `limit` and opaque `cursor` convention. Work-item detail timelines use `timeline_limit` and `timeline_cursor`.

Omit `work_item_id` from `POST /api/v1/claims` to claim next. Candidate selection and lease creation execute in one SQLite transaction; clients must not list candidates and then attempt a separate specific claim. Renew, release, and event requests must carry the positive `fencing_token` returned by the claim.

## Native organization and project setup

Each database receives a stable `hub_` identity and a local `org_` organization.
Local operation requires no hosted account. Native projects, issues and comments
use opaque `prj_`, `wi_` and `cmt_` IDs. IDs contain 128 random bits and survive
restart and owner backups. Project issue numbers are display values; APIs and
workers use immutable IDs. External references are optional.

An instance administrator can list organizations with `GET /api/v2/organizations`,
create another organization with `POST /api/v2/organizations` and `{"name":"Team"}`,
and create a project with
`POST /api/v2/organizations/{organization_id}/projects`:

```json
{
  "idempotency_key": "create-project-example",
  "name": "Example project",
  "require_dependencies": true,
  "states": [
    {"name":"Todo","dispatchable":true,"terminal":false,"transitions":["In Progress"]},
    {"name":"In Progress","dispatchable":true,"terminal":false,"transitions":["Review"]},
    {"name":"Review","dispatchable":false,"terminal":false,"operator_only":true,"transitions":["Done","Todo"]},
    {"name":"Done","dispatchable":false,"terminal":true,"transitions":[]}
  ]
}
```

Project state names and transitions are explicit; there is no prescribed workflow.
A project created through the hosted client starts from a fixed template instead:
`Todo` and `In Progress` (dispatchable), `Human Review` (neither dispatchable nor
terminal, and not `operator_only`, so the orchestrator can move a completed run's
Change Request there), `Merging` (dispatchable: an accepted Change Request waits
there for the runner that lands it) and `Done` (terminal). `In Progress` may move
to `Todo`, `Human Review`, `Merging` or `Done`; `Human Review` may move to `Done`,
`Merging` or back to `In Progress`; `Merging` may move to `Done`, back to
`Human Review`, or back to `In Progress`. A completed run whose published
version the review policy already accepts moves from `In Progress` straight to
`Merging`; any other committed change waits in the review lane.
Hub migrations 36, 38 and 39 move projects whose workflow is exactly an earlier
template onto it and leave customized workflows unchanged. A native project's Change Requests wait in the lane named by the
runner's `auto_promote.source_state` (default `Human Review`), so a customized
workflow needs that lane, non-`operator_only` and reachable from its active
lanes, for completed runs to reach review.
`operator_only` prevents workers from creating an issue in, or transitioning to,
that state. `require_dependencies` defaults to true. Setting it false disables
dependency readiness gating for that project while retaining scope and cycle
validation. A resolved dependency has a terminal workflow state. This setting
does not bypass lease ownership or machine capacity.

Create a worker or operator token with the existing administrator token endpoint,
then grant it project access with `POST /api/v2/tokens/{token_id}/grants`:

```json
{"organization_id":"org_example","project_id":"prj_example"}
```

Use the actual returned IDs. The grant operation is idempotent and converts the
token to native-only access. Such tokens cannot use v1 or instance administration,
including when their stored role is `admin`. Token rotation preserves grants;
revocation prevents subsequent authenticated requests. The bootstrap administrator
cannot be converted to a project token. Instance administration is deliberately
separate from tenant access; hosted human membership remains owned by #2193.
Enrolled runners use the scoped host identities described above.

Every native project route requires both its organization and project grant.
Unknown, guessed and inaccessible resource IDs return 404. Native machine IDs
are bound to their organization and token principal; another token cannot take
over registration or renew its leases. A legacy registration cannot overwrite a
native machine. Native tokens cannot read legacy global lists through v1.

## Native collaboration protocol

`GET /api/v2/capabilities` reports server identity, the Hub's running `version`,
supported protocol majors, event schemas, required native features, request size
and page limits. Native
clients negotiate major 2 and schema 1. A native claim supplies `protocol_major: 2`
and `capabilities: ["native_issues", "scoped_collaboration"]`. Incompatible
negotiation fails without switching tracker or scheduler.

The following paths are relative to
`/api/v2/organizations/{organization_id}/projects/{project_id}`. Worker and
operator tokens can read and author collaboration; claims, machines and run
events require worker scope. Instance administrators can perform these operations.

| Method and path | Input or result |
| --- | --- |
| `GET /` | Project profile, states, transitions and readiness policy |
| `POST /work-items` | `idempotency_key`, `title`, full `body`, configured `state`, optional `priority`, `labels`, `assignees`, import `provenance` |
| `GET /work-items` | Paged issues; repeatable exact `state`, `label`, `assignee`, `priority` filters (OR within a dimension, AND across dimensions; at most 32 values and 4096 bytes per dimension). `q` matches title, project name or ID plus `#number`, and labels; without `include=work`, it also matches body. `include=work` adds compact body-free items, bounded current open selection, and complete-filter lane/running totals independent of cursor |
| `GET /work-items/{id}` | Full content, immutable scope, revision, authenticated author, import provenance, dependencies and optional external references |
| `PATCH /work-items/{id}` | `idempotency_key`, `expected_revision`, supplied `title`, `body`, `priority`, `labels` or `assignees` |
| `POST /work-items/{id}/workflow` | `idempotency_key`, `expected_revision`, target `state`, typed `reason` |
| `POST /work-items/{id}/dependencies` | `idempotency_key`, `expected_revision`, `related_work_item_id`, `operation: add` or `remove` |
| `POST /work-items/{id}/comments` | `idempotency_key`, explicit `body`, optional import `provenance` |
| `GET /work-items/{id}/comments` | Paged discussion with authorship, provenance, revisions, editor and timestamps |
| `PATCH /work-items/{id}/comments/{comment_id}` | `idempotency_key`, `expected_revision`, explicit `body` |
| `GET /work-items/{id}/history` | Paged typed collaboration and run events |
| `GET /work-items/{id}/versions/{revision}` | Immutable issue content snapshot |
| `GET /work-items/{id}/comments/{comment_id}/versions/{revision}` | Immutable comment content snapshot |
| `POST /machines/register` | `id`, `hostname`, `display_name`, `capacity`, `version`; registration also refreshes heartbeat |
| `POST /claims` | Approved `policy_id`, `machine_id`, unique `session_id`, `ttl_seconds`, protocol negotiation, optional `work_item_id`, workflow and author/assignee/label filters |
| `POST /leases/{lease_id}/renew` | `fencing_token`, `ttl_seconds` |
| `POST /leases/{lease_id}/release` | `fencing_token`, typed `reason` |
| `POST /work-items/{id}/events` | Idempotent, versioned run fact with current lease and typed references |

Every work-item representation returned by create, get, list and update includes
`web_url`. Hosted Hubs build this absolute link from the configured
`hosted.public_url`, never from request Host or forwarding headers. Shared Cloud
links use
`https://cloud.detent.build/organizations/{organization_id}/work/i/{work_item_id}`;
dedicated hosted Hubs use `{public_url}/work/i/{work_item_id}`. The link opens the
existing conversation app route. Non-hosted Hubs have no configured public URL
or conversation app and return an empty string. Replayed mutations and historical
work-item versions project the link using the serving Hub's current configuration;
the URL is not immutable content.

Collaboration mutations return the committed representation with status 200,
including identical retries. Idempotency keys are scoped to the authenticated
principal, organization and operation/resource. Reusing a key with different
content returns 409 `idempotency_conflict`. Concurrent edits return 409
`revision_conflict` with `current_revision`. Revisions, event sequences and fencing
tokens are decimal strings on the wire. Mutations, their saved content versions,
history and retry result commit in one SQLite transaction. Ordinary clients cannot
update or delete history.

Titles are limited to 500 bytes, bodies to 256 KiB, comments to 64 KiB, and requests
to 1 MiB. There are at most 100 labels and 100 assignees, each at most 200 bytes;
priority ranges from 0 (urgent) to 3 (low). Empty label/assignee arrays clear them.
Unknown request fields and unsupported query parameters fail validation.
Workflow reasons are `user_requested`, `worker_progress` and `dependency_ready`.
Release reasons are `completed`, `cancelled`, `failed`, `released`,
`work_item_hydration_failed` and `work_item_identity_missing`.

Native pages use `limit` (1–200, default 50) and an opaque `cursor`. Issue numbers,
comment creation sequences and aggregate history sequences provide increasing
page order. Signed cursors bind protocol, principal, organization, project,
resource and query; they expire after one hour. Reuse with a different scope or
filter fails. Restart preserves the signing key. These are live pages rather than
a transactional export snapshot: concurrent edits can change which issues match
a filter, so a consistent export requires a quiescent owner backup.

Dependency writes require access to both projects within the same organization.
Cross-organization links are prohibited by the database and API. Transitive
cycle detection and the graph edit execute in the same transaction, preventing
two concurrent edits from jointly introducing a cycle. Dependency history changes
the dependent issue revision, so stale graph edits conflict too.

## Authorship and event custody

The server derives `actor` from the authenticated token. Imported author IDs and
names remain in `provenance`; they never grant authenticated user authority.
Operator-authorized imports supply provider `github`, stable `external_id`,
`author_id`, optional `author_display_name`, and source `created_at`, `updated_at`
and `observed_at` timestamps. Local creation/update times remain server times.
Repeated source IDs return the existing imported record and do not overwrite
later native edits. [GitHub profiles and import](github-profiles.md) documents
resumable history retrieval, explicit cutover, optional summaries and limitations;
attaching provenance alone is not a claim that all external history was imported.

Workers can edit their own comments. Operators can edit project comments, with
the editor recorded separately from the original actor and provenance. Explicit
issue and comment content can contain code or secrets and is retained as
collaboration data. The service does not promise that authored text is secret-free.

History records have an event ID, organization/project, aggregate identity and
sequence, type, schema version, server recording time, authenticated actor and
typed data. `issue.created`, `issue.edited`, `comment.created`, `comment.edited`,
`dependency.changed` and `workflow.transitioned` are server-generated. They refer
to content revisions instead of duplicating full text in event data.

Worker schema 1 accepts `run.started`, `run.finished`, `run.checkpointed` and, with the optional `native_runtime_evidence` capability, ordered `run.observed`.
Their `data` requires `lease_id`, positive string `fencing_token`, and typed
`run_`, `attempt_` and `policy_` IDs. Finished outcomes are `succeeded`, `failed`,
`cancelled` and `interrupted`. Only checkpoints accept `artifact_ids`, at most 20
typed `artifact_` IDs. Arbitrary URLs, signed download capabilities, raw prompts,
transcripts, tool output, local paths, source, diffs and artifact bytes have no
payload fields. The current lease and machine principal must match the item.
Idempotent retries return the committed result; new events with an expired fence
fail. A run outcome records a worker report, not a verified check or merge grant.
The policy ID is `policy_` followed by its 64-character lowercase SHA-256 digest
and must equal the approved descriptor pinned to the lease.

### Native runtime evidence

With `native_runtime_evidence`, `GET /work-items/{item}/runtime` selects the
latest fenced attempt or an exact `native_attempt_id` (typed native ID) or
positive `attempt_id` (recorded local attempt ID). Selectors are mutually
exclusive and ownership is scoped to the item and current project grant;
ambiguous local IDs require the native selector. An absent selected attempt
returns not found. An item with no recorded attempt returns explicit unavailable
evidence. Older deployments return unsupported availability separately from
grant denial.

The typed snapshot includes the current native item, authentic last workflow
transition and recorded scheduler decision, current Change readiness and
enrolled runner routing/host/provider capacity. A current fenced lease is shown
independently of the selected attempt, including a lease without a started run;
lease reads omit private isolation configuration. Current candidate readiness is
not full runner eligibility, and board counts or nil PRs cannot establish it.
Historical decisions are recorded only when the existing scheduler evaluates
them, with actor, item revision, source and server time; reads add no history.

`GET /work-items?include=work` adds a compact `work` member to the existing
inventory page. `work.lanes` contains state, total and observed running counts
for the same authorized project and server filters, independent of the cursor.
Running counts require a recorded running attempt with a current unreleased
lease; In Progress membership is not evidence of a live worker.
`work.as_of` is the server observation time. `work.items` selects at most the
requested page limit of nonterminal items, ordered by live attempt, dispatchable
state and descending issue number. `work.truncated` marks additional open items;
state filters and the existing inventory cursor still navigate them.
Both item selections omit issue bodies and linked-source snapshots in this
projection; the existing item detail read retains full content. The ordinary
inventory page keeps ascending issue-number ordering and its scoped opaque
cursor. Clients retain bounded per-item observations with known, unchecked,
unavailable and partial coverage rather than inferring worker state from lanes.

Ordered events may include `runtime`: local attempt/generation attribution,
observed backend/model/effort provenance, phase intervals, heartbeat, bounded
instruction profile, landing receipt and existing REST accounting. Attribution
cannot change and heartbeat cannot move backwards in an attempt. Runtime
freshness is unavailable without an observation, expired for future/old
heartbeats or lost running leases, and available for recorded terminal
observations. Lease renewal is not proof of agent activity.

Activity spans contain hashes and allowlisted evidence labels, never command
text, instruction contents or filesystem paths. The projection retains at most
128 KiB, 64 instruction sources and 32 actions/span, with partial coverage,
dropped/unpaired counts and no claim that reading an instruction caused an
action. `board_session_history` provides offset/limit pages (1–200).
Receipts, attempt lists, work history and explanations include profile metadata
and explicitly identify omitted activity spans; full spans have their existing
board_session_history owner. Exact runtime and attempt detail retain the bounded
recorded profile.

A landing observation names Change/version/head and either actual landed
merge SHA/base/method or a bounded refusal kind. The Hub verifies actual landing
against recorded Change authority; terminal attempt success alone cannot prove
a ship. REST data projects existing operation-header observations and attribution
windows, with credential digests, resource, timestamp, HTTP status and the
presence of a Used header. Selected-client counts exclude worker subprocesses,
other clients and hosts; an unaccounted delta does not identify its consumer.
At most 64 operation and attribution windows are returned, with dropped counts.
This adds no quota capacity policy or aggregate reporting owner.

The existing `explain_item`, `board_receipt`, `board_session`,
`board_session_history`, `work_attempt_receipt`, `work_history` and
`board_activity` operations delegate to this native application evidence
through supported local and Cloud adapters. MCP results retain the 256 KiB
bound and return unavailable on overflow. Native efficiency aggregates and
unrecorded runtime data remain explicitly unavailable. Hub schema 59 preserves
prior event history and refuses rollback when new event types would be lost.

### Ordered attempts and recovery

The `fenced_run_history` capability extends schema 1 with a positive decimal-string
`data.sequence` and `identity: {role, backend, model}`. Identity components are
bounded to 128 identifier characters. The server derives machine, runner and Hub
session identity from the authenticated lease. The policy digest remains pinned.
One lease binds one ordered attempt; a run can span successive fenced attempts of
the same issue. Sequence 1 must be `run.started`; subsequent sequences are
contiguous. `run.finished` closes the attempt and cannot finish a successor.
An identical sequence replay returns success without adding history, even when
the command key changes. Changed content, gaps, identity changes, repeated starts
and progress after completion conflict. Client occurrence times never order events.
Legacy schema 1 events without sequence remain readable and writable for legacy
attempts, but cannot be mixed into an ordered attempt.

`GET /work-items/{item}/attempts` uses the same authorized pagination contract as
history. Attempts are ordered by the existing monotonic lease fence. The response
contains effective identity/policy, last accepted sequence, server timestamps,
terminal outcome and the latest retained checkpoint. A running attempt whose lease
is released or expired reads as `interrupted`. Hub restart retains both the
projection and append-only history; expired leases and attempts are not deleted.

The native scheduler hydrates full issue content, paginated discussion, attempts
and history, including legacy events, before dispatch. Runner prompts receive this
Hub context without consulting GitHub issue APIs. Checkpoints may carry the last
reported typed Change/version/head reference; it is not an independently verified
current Change or a review/merge grant. Native Change registration and authoritative
version lookup remain the separate #2191 deliverable; absent references are omitted.

Ordered `run.checkpointed` events require a bounded `handoff` object:

| Field | Accepted values or format |
| --- | --- |
| `resume` | `resume_session`, `fresh_checkout`, `manual_recovery` |
| `storage` | `local_only`, `customer_store` |
| `availability` | `available`, `missing`, `inaccessible`, `unverified` |
| `worktree_state` | `clean`, `dirty`, `unpushed`, `unknown` |
| `head_sha`, `expected_head_sha`, `workspace_digest` | Optional hexadecimal Git commit IDs or workspace digest |
| `external_effect` | `none`, `git_push`, `pr_create`, `provider_turn` |
| `effect_state` | `none`, `pending`, `confirmed`, `ambiguous` |
| `effect_id` | Typed opaque `effect_` identity for an external effect |
| `change` | Optional typed `change_id`, `version_id`, and immutable `head_sha` |

Customer-store checkpoints require artifact IDs and cannot assert `available`:
independent durable receipt/integrity/access verification belongs to #2190 and the
[artifact access contract](artifact-access-contract.md). IDs convey neither a
download capability nor proof of ownership or availability. Hub never receives
workspace paths, provider session state, manifest contents, source, diffs, raw
transcripts, storage credentials, signed URLs or artifact bytes through handoffs.
Runner-local checkpoints remain local even when the metadata survives in Hub.

Resume requires a locally present workspace, matching machine/head/digest and
policy/runtime identity, plus successful backend session verification. Otherwise
the runner starts a fresh session while retaining recoverable local work, or
requires explicit recovery for unavailable dirty/unpushed checkpoints and ambiguous
Git/PR effects. A clean missing checkpoint permits a fresh checkout/session. This
does not claim that a local workspace survives machine loss. Before epilogue hooks,
dirty/unpushed work uses the existing workspace retention mechanism from #2138;
checkpoint publication is metadata only and does not duplicate #2133's push work.

Native executions revalidate the pinned policy and current lease before startup,
provider turns and epilogue hooks. Lease responses include `server_time`; the
local deadline uses the remaining server lifetime minus request elapsed time and
a safety margin (10% of TTL, capped at five seconds). Replaying an existing claim
does not incorrectly grant a fresh TTL, and clock skew cannot extend authority.
Renewal failure does not extend it. Loss of authority cancels the provider context;
reconnection cannot revive that context. External epilogue hooks are skipped on
claim loss, and local retention remains allowed. Hub outages use the existing
scheduler backoff; no independent local scheduler starts and failure retry counts
are preserved. Once ordered execution exists, worker issue/comment/dependency/
workflow mutations also require the current `lease_id` and `fencing_token`.
Off-lease human collaboration uses operator authority. Exact command replays may
return a previously committed response after expiry, but perform no new mutation.

Database fencing is not an exactly-once guarantee for external effects. An accepted
Git push, PR creation or provider request can outlive a lost response or race
cancellation. In-flight provider tool calls are bounded by process cancellation,
not an atomic transaction with Hub. Reconcile an uncertain push against the exact
remote ref and expected head, reuse an existing PR only after verifying its head
branch, and use provider idempotency/session lookup when available. Never blindly
repeat an ambiguous external write. Existing expected-ref/head and worktree
preservation paths remain authoritative. The client retains an unacknowledged event
and replays its exact sequence and command before advancing; a restart retrieves
durable attempts rather than assuming the last request failed.

## Worker connector inventory and compatibility

`internal/hubclient/native_connector.go` supplies the existing connector interfaces:

| Orchestration or agent operation | Native route and behavior |
| --- | --- |
| Candidate discovery and adoption | Existing Hub scheduler, candidate query and fenced lease writer |
| Refresh by issue ID or workflow state | Full native issue detail or paginated list |
| Create issue, update body, title, assignee or priority | Typed issue mutation with revision check |
| State update | Configured native transition |
| Create/update workpad or discussion comment | Native comment mutation with edit revision |
| Read comments/events for subsequent workers | Consume every authorized comment/history page |
| Comment author authorization | Locally authenticated human provenance; imports confer no authority |
| Add/remove native dependencies | Scoped, revision-checked graph mutation |

Unsupported generic fields and absent optional connector capabilities fail
explicitly. No native connector method delegates to a GitHub issue connector.
GitHub repository/PR/CI/review/merge capabilities remain separate integrations;
native Change Requests link to these integrations through optional external PR
references and retain their identity when no GitHub PR exists.

Schema migration retains existing issue/repository integer keys, GitHub node IDs,
issue numbers, queue entries, dependencies, leases/fencing, work events and outbox
links. Compatibility repositories receive stable project aliases in the local
organization under the existing instance-administrator boundary. Project grants
are an explicit administrator decision, never inferred from a current login.
Existing v1 clients keep their compatibility IDs and content authority. Native
rows have no mandatory repository or GitHub issue reference and cannot be fetched
by guessing their integer keys through v1. Compatibility event history remains
in v1; no arbitrary historical payload is automatically copied to v2.

## Native Change Requests

The `change_requests` capability adds stable `change_...` records to a project.
Each record has a primary issue, additional issue links in the same project,
title/discussion, and an ordered set of immutable `version_...` records. These
records work in native and GitHub-compatible projects without changing issue-field
ownership. Creating a Change Request does not create a PR or authorize a merge.

A runner opens a native item's Change Request when a work run finishes with
commits, under the run's lease, and then publishes the run's head as the
change's version: `base_sha` and `merge_base_sha` are the attempt diff's base,
`repository` is the https form of the checkout's origin remote, and `code`
names the head commit under that repository with `availability` `unverified`.
A rework run whose head is already the current version publishes nothing; any
other head becomes the next version, so every reviewed head is an immutable
record. A version the Hub refuses (no https remote, a stale review policy) is
reported on the run's completion comment and the Change Request stays a draft
until the next successful run publishes one.

All paths below follow `/api/v2/organizations/{organization}/projects/{project}`.
Mutations require the existing `idempotency_key`; workers publishing versions
also supply their current `lease_id`, `fencing_token`, `run_id`, and `attempt_id`.

| Method and suffix | Authority and result |
| --- | --- |
| `GET /work-items/{item}/changes` | Project-scoped linked Change Requests |
| `POST /work-items/{item}/changes` | Worker/operator; `title`, `body`, optional `linked_issues` |
| `GET /work-items/{item}/changes/{change}` | Consistent detail with versions, decisions, checks, discussion and current summary |
| `POST /work-items/{item}/changes/{change}/versions` | Publish through the primary issue with `expected_version_id` |
| `POST /work-items/{item}/changes/{change}/discussion` | Append `body`, optional `version_id`; only operators can import `provenance` |
| `POST /work-items/{item}/changes/{change}/versions/{version}/reviews` | Operator decision: `approved`, `changes_requested`, or `commented`, plus optional `body` |
| `POST /work-items/{item}/changes/{change}/versions/{version}/checks` | Credential pinned in the immutable expected check set |
| `POST /work-items/{item}/changes/{change}/versions/{version}/landing` | Worker under its lease: `merge_sha`, `base_ref`, `method`; records the landed commit and finishes the primary issue |
| `GET /change-review-policy` | Inspect the approved native review/CI expectations |
| `PUT /change-review-policy` | Self-hosted instance administrator, or hosted owner/admin with a target-project write grant; compare `expected_review_policy_id` and approve `policy` |

Hosted approval requires an active organization owner/admin membership, a valid
human session and CSRF token, and a write grant for the target project. Runner
management grants are not required. Bearer tokens cannot replace human membership.
The server rechecks the session, membership role and project grant inside the
mutation transaction, including before returning a cached approval response.

Before publishing, an administrator approves a review policy tied to the current
repository `policy_id`. Approving a native project's repository policy seeds the
default review policy when the project has none: no CI check is pinned, and
`require_review` follows the repository policy's `review.human` setting, which
defaults to false. A review policy that pins no checks follows that setting: approving the
repository policy again rewrites it to the default, and an approval that expects
no review policy may replace it. A review policy an administrator shaped by
pinning checks is never rewritten; it goes stale when the repository policy
changes and must be approved again. A repository policy whose gates the default
cannot satisfy (a required check count or validator) seeds nothing. Hub
migration 40 applies the same rule once to every project with an approved
repository policy, so projects approved before seeding existed can publish.
Its `require_review` setting cannot weaken a repository
human-review gate. `required_checks` cannot fall below the repository check count,
and a repository validator requires an independent check. Every check pins `name`,
`principal_id` (an existing token with a project grant), `workflow_id`,
`workflow_sha256`, `source` (`customer` or `independent`), and `max_age_seconds`
(60 seconds to 7 days). Independent checks require operator credentials. Use a
dedicated CI credential, kept outside the implementation runner's environment.
These settings do not modify repository auto-promotion, opt-out, security audit,
validator, external required review, or merge-method configuration.

Version requests include lowercase `base_sha`, `head_sha`, `merge_base_sha`, the
approved `policy_id`, a `repository` reference, a `code` reference, and optional
`artifacts`. Worker versions identify a real fenced run/attempt. Human-authored
versions may omit a run. `external`, when present, contains `provider: github`,
the PR number as string `id`, and its `url`. The server snapshots the approved
repository descriptor and review policy; workers cannot choose weaker checks.
`expected_version_id` is empty for the first version and must match the current
version thereafter. Reusing a command with different content or publishing from
an old current version returns a conflict. New versions require new approval and
fresh evidence even when a publisher reports the same head.

Code, manifests, diff bundles, logs and other artifacts remain in customer storage.
Hub retains only `kind`, `uri`, `sha256`, and `availability`. References support
`https`, `s3`, or `gs`, without embedded credentials, query tokens or fragments.
Kinds are `code`, `manifest`, `diff`, `test`, `log`, `checkpoint`, or `artifact`;
availability is `available`, `missing`, `inaccessible`, or `unverified`. Availability
is a report by the authenticated publisher, not a Hub download or independent
verification. A future viewer can resolve these references using the same change
and version IDs. Raw patches, source and artifact bytes are not API fields.

Every version generates fresh `check_run_id` values. CI submits the expected
`check_run_id`, `head_sha`, native `run_id` (empty for a human version without a
run), `policy_id`, `config_digest`, `workflow_id`, `workflow_sha256`, and `source`,
plus terminal `conclusion`, `completed_at`, and nonempty `evidence` references.
The submitting credential must match the expectation. Completion cannot predate
the version or be in the future. Only `success` with available, unexpired evidence
can pass; skipped, cancelled, missing, inaccessible and stale results do not.
An approved policy with no required checks reports `not_required`, never a
synthetic success; its native review requirement still applies.
Freshness is measured from completion, not delivery. The authenticated CI source
is responsible for producing truthful results for the pinned workflow and head.

Terminal results are immutable. Exact redelivery is idempotent even with a new
command key; changing an existing result conflicts. Late callbacks remain attached
to their original version and cannot move the current pointer. CI callbacks do not
require a still-running implementation lease. To rerun validation, publish a new
version with fresh check identities. Customer-run evidence is visibly distinct
from independent validation and cannot satisfy an independent expectation.

A `changes_requested` decision puts the review text on the primary issue as a
comment by the reviewer, where the next run reads it with the rest of the
discussion, and moves an issue waiting in review or in the landing lane back to
a dispatchable working lane (`In Progress` when the workflow has it), so the
runner picks it up again; an issue already being worked stays where it is. The
rework run reuses the item's Change Request and publishes its new head as the
next version.

An `approved` decision that leaves the current version `reviewed` moves the
primary issue to the landing lane: the lane named `Merging`, when the issue's
lane may move there and it dispatches. The runner that holds the project claims
the issue there and lands the reviewed head with its own credentials. By
default it uses plain git: it fetches the base branch (the remote's default), verifies the
worktree still stands at the reviewed head, combines the two by the approved
policy's `merge_method` (a squash commit, a merge commit, or the head's commits
replayed onto the base), and pushes the result to the base branch, refusing to
move a base that changed underneath it. It then reports the commit through the
landing route, and the Hub records it on the Change Request (`landed`, with the
version, head, merge commit, base branch, method and actor), marks the summary
`landed`, and moves the issue to the first terminal lane its lane may reach, in
one transaction. A landed Change Request accepts no further review or landing.
The Hub never holds git credentials and makes no forge API call to land.

A landing the repository does not allow is refused, never worked around: a
worktree that moved past the reviewed head, a reviewed head the checkout no
longer has, a conflict with the base, a base that already contains the head, a
base branch that moved during the landing, or a base branch the forge protects
(a pull request requirement, a protected branch, a hook that declines). The
runner reports the reason, and the orchestrator moves the issue back to the
review lane with a comment that carries it, such as "allow the runner to push
to develop, or enable GitHub pull request mode for this project". Only an
infrastructure failure (the remote unreachable, the Hub refusing the report)
fails the run and retries it.

A project may opt in with `deliverable.github_pull_request: true` in its
approved repository policy. For a github.com origin and an authenticated `gh`
on the project runner, landing then publishes the reviewed attempt branch,
finds or opens a pull request against the remote default branch, and asks
GitHub to merge the exact reviewed head using the policy's `merge_method`.
GitHub's branch protection, required checks, and required reviews decide
whether the merge succeeds. The runner verifies that the returned merge commit
is on the base branch before reporting it to the Hub. A refused merge returns
the issue to review with the GitHub reason; the reviewer can approve it again
after fixing that condition. This mode uses the runner's `gh` credentials and
does not give them to the Hub. The default remains plain git and requires no
GitHub API access.

Native approval never becomes a required GitHub review. Detail optionally reuses
the existing projected `PullRequestSummary`, labels it as a snapshot, and identifies
a mismatched external head. Existing protected-merge verification and the exact-head
merge mutation remain authoritative; the native API has no auto-merge switch.

`detent hub issue` exposes `changes`, `create-change`, `change`, `publish-change`,
`review-change`, `check-change`, `discuss-change`, and `approve-review-policy`.
The existing `--project`, `--organization`, `--hub-url`, `--identity-file`, and
`--token-env` flags select the scope and credentials. Mutations read typed JSON
from stdin. For example:

```sh
detent hub issue create-change wi_example --project prj_example <<'JSON'
{"idempotency_key":"propose-work","title":"Proposed work","body":"Review context"}
JSON
detent hub issue changes wi_example --project prj_example
detent hub issue change wi_example change_example --project prj_example
```

The Work board/list opens native issue detail, which links to Change Request
detail. Detail provides version selection, stale/missing-evidence messages,
discussion provenance, artifact availability, and issue/run navigation. Native
run recovery uses published change references from ordered issue history rather
than trusting a stale worker checkpoint to identify the current version.

## Retention, export and deletion design

Collaboration content, content versions, typed events, idempotency responses and
owner backups all contain retained data. This release has no automatic native
content expiry or issue-deletion endpoint. Append-only triggers deliberately
reject ordinary history deletion. [Self-hosted operations](hub-self-hosting.md)
defines full-instance export/import, credential fencing on restore, whole-instance
retirement and operator-managed backup expiry. Scoped purge remains unimplemented;
append-only storage is not a promise of permanent retention.

A future scoped deletion procedure must use the following contract:

1. Authorize the organization and record the deletion scope outside the affected
   content. Fence writers and active leases, then take an owner backup when
   policy permits a temporary recovery copy. Export current issues, every content
   revision, comments, history and identity mappings when requested; artifact
   content must come from its separate authorized service.
2. Run maintenance through the single database owner, with ordinary API writes
   stopped. In one transaction, temporarily replace the append-only triggers
   with maintenance-only enforcement, remove or redact affected current content,
   revisions, event references, idempotency response bodies, legacy workpad/outbox
   copies, webhook payload copies and affected graph links, and write permitted
   content-free tombstones. Restore append-only triggers before committing and
   verify foreign keys. A crash must roll back both the data and trigger changes.
3. Retain a deletion ledger outside older backups. Do not reuse deleted IDs.
   Any future content retention gap must invalidate old cursors with an explicit
   snapshot/resync response rather than silently presenting incomplete history.
4. Expire every affected backup according to the approved policy. Restore into an
   isolated owner, replay the deletion ledger and rotate cursor/session authority
   before exposing restored data or accepting claims. A restore must not resurrect
   deleted content or accept a formerly valid lease.
5. Record which artifact stores and external projections require separate deletion
   and their outcomes. Database deletion does not erase a GitHub projection,
   independently hosted artifact, exported file or customer backup.

The implemented tests prove ordinary append-only enforcement, immutable identity,
scoped revision retrieval, backup-compatible migration and restart persistence.
They do not claim that a privileged scoped purge command is implemented.
The supported full-instance restore revokes copied credentials and releases leases;
it does not implement scoped erasure or automatically replay a customer deletion
ledger. Operators must retain backups deliberately and must not disable triggers
on a running Hub as a substitute for supported deletion.

## Provider capacity reports

Enrolled native runners can report provider capacity with an optional global
client setting. Existing unconfigured runners retain their scheduling behavior.

```yaml
client:
  hub_url: https://hub.example.com
  identity_file: /absolute/private/runner/identity.json
  organization_id: org_example
  native_projects:
    application: prj_example
  provider_capacity_file: /absolute/private/runner/provider-capacity.json
```

The file is a JSON array written by an operator-managed local collector. Detent
does not log into provider accounts or scrape billing pages to populate it. Use
atomic file replacement when publishing a new observation. The collector maps
each backend ID to the credentials already used by that local backend; reporting
an alias never switches the backend's account. Each backend has exactly one local
account in a report. Configure separate backend IDs for separately selected
accounts through the existing agent routing configuration.

```json
[
  {
    "provider": "openai",
    "backend": "codex",
    "account_alias": "local-work",
    "shared_account_alias": "team-subscription-a",
    "models": ["gpt-5.6-sol", "gpt-6-astra"],
    "model_details": [
      {
        "id": "gpt-6-astra",
        "label": "GPT-6-Astra",
        "provider": "openai",
        "default": true,
        "reasoning_efforts": ["low", "medium", "high", "xhigh", "max"],
        "default_reasoning_effort": "low"
      },
      { "id": "gpt-5.6-sol", "legacy": true, "reasoning_efforts": ["low", "high"] }
    ],
    "max_concurrent": 2,
    "availability": "unknown",
    "observed_at": "2026-09-05T12:00:00Z"
  }
]
```

`model_details` is optional and advisory: it describes models `models` already advertises and never widens what capacity matches. The collector writes it; the runner does not yet fill it from its backends' model catalogues. Each entry describes one reported model at most once, with at most 16 distinct reasoning efforts in the backend's own order (the Hub does not check the order), a `default_reasoning_effort` that must be in that list when the list is non-empty, a label of at most 128 characters, and `legacy` set when the provider has named a successor. A report carrying detail for a model outside `models` is rejected. When a conversation's preferences change, the Hub checks them against fresh runner reports: an explicit model must be one a fresh report advertises or the Hub's configured `conversation.model`, which is accepted without any runner report, and an effort outside the fixed vocabulary is accepted only when that model's detail publishes it (docs/conversation/decisions.md section 14); `GET /app/bootstrap` does not republish it yet, so `preferences.models[]` stays empty.

Only these fields, `model_details` and optional `reset_at` are accepted. Files are bounded to
256 KiB, 32 backends and 128 model identifiers per backend. Aliases are opaque
lowercase ASCII tokens, at most 64 characters; use no email addresses, API keys,
login sessions, billing records or customer prompts. Provider/backend/model
identifiers are bounded, case-sensitive tokens; use the same provider ID on every
machine sharing an account. `models` must advertise the identifiers selected
by local routing. An existing unpinned route uses the explicit `provider_default`
capability; capacity never picks a different model or rewrites effort.

`max_concurrent` is an operator-declared bound on simultaneous reserved runs,
between 1 and 10000. It is not a token balance or an estimate of transferable
credits. `availability` is `available`, `exhausted` or `unknown`. Observations at
least two minutes old, observations in the future, and reset hints reached at
equality become `unknown`, never automatically `available`. Unknown quota permits
dispatch only within the declared concurrency bound and existing local brakes.
An exhausted fresh observation waits for a refreshed report or its reset hint.

Declare the same `provider` and `shared_account_alias` on every machine using the
same provider account, even when local aliases or backend IDs differ. Hub scopes
sharing to the organization, uses the lowest declared limit and honors exhaustion
from any overlapping report. Distinct explicit aliases declare independent
accounts. An omitted shared alias means sharing is unknown: its reports and
reservations overlap every account of that provider in the organization. Separate
local aliases or reports never imply independent capacity. Drain work before
changing account mappings; existing leases retain their original reservation.

Registration and heartbeats accept typed `provider_reports` on the native machine
endpoints. Once a runner advertises reports, requests without selected candidates
cannot bypass reservations. A missing/invalid configured report file defers
dispatch; it does not silently disable capacity checks. Omitted heartbeat reports
retain the last report, including its original observation time.

`POST .../claims/preview` returns authorized candidates in the existing Hub queue
order, in pages of up to 100 (`after`/`next` are opaque cursor values). The client
uses the existing local role/model/override resolution, then includes
`provider_candidates` (work item ID, revision and selected role/backend/model)
in `POST .../claims`. Hub rechecks current issue revision, approved policy,
organization/project authority, tags, fixed host, health, host capacity and
provider compatibility inside the claim transaction. Capacity removes ineligible
choices from that order; it does not reprioritize work or select a fallback model.
The negotiated feature is `provider_capacity_reservations`.

The claim response includes `provider_reservation`, pinned to the fenced lease.
The client rechecks the local account and model immediately before `run.started`;
Hub rechecks the shared pool while accepting the first ordered start. A changed
identity or newly exhausted pool defers the start without spending the issue's
failure budget. Existing local budgets, pools, host pressure and provider backoff
still apply. Cancellation, failed preparation, hydration failure and normal
release free capacity through the existing lease. Expiry frees abandoned
reservations after restart; stale fencing tokens cannot release a successor's
reservation. Lost acknowledgments remain idempotent. Capacity changes affect new
starts and preserve active execution identity and history.

These reservations coordinate enrolled Detent runs using reported accounts;
external provider consumers still require fresh observations and local provider
error handling. Fleet details show reports, shared usage and waiting reasons;
run details show the selected role/backend/model/account and reason. `detent
doctor` reports the same capacity view. There is no additional global banner.

### Selective onboarding issue intake

Native projects can import selected GitHub issues with runner credentials and
GitHub transport disabled on Hub. Associate a matching runner checkout first.
`GET /api/v2/organizations/:organization/projects/:project/onboarding/issue-intake`
returns `batch` (nullable) and configured `lanes`. Administrator mutations use
`POST` to the same path with `idempotency_key`, `revision`, and `action`:

- `discover`: choose `runner_id`, optional `labels` and explicit `include_closed`.
  Default is open issues. The assigned runner reads one page of at most 100
  issues using GraphQL and publishes previews through its existing heartbeat.
- `more`: request the next preview page, up to 1,000 previews per intake. Narrow
  larger sets with labels. Selecting all matching requires loading all pages.
- `apply`: provide selected `numbers` and `destination`. A configured
  non-dispatchable, nonterminal lane is the default; dispatchable destinations
  require `allow_dispatch: true`. Source URL and node identity deduplicate
  linked issues and older imports. Existing issues are skipped without changes.
- `retry`: resume failed discovery/items, optionally on another `runner_id`.
  Reported source retry deadlines must have elapsed. Completed items are retained.

The runner publishes one complete snapshot or an incomplete-context diagnostic
per selected item to the worker-only `/onboarding/issue-intake/result` endpoint.
Snapshot, provenance, comments and completion checkpoint commit together;
native edits retain field ownership. At most 20 comment pages are read per
issue. Context exceeding that bound remains incomplete. Rate limits stop
unstarted items until an explicit retry; no idle GitHub polling occurs. The
wizard refreshes progress only on request. Reopening setup resumes the persisted
selection and results. Intake performs no GitHub writes. New authoring and
landing continue through native Hub operations, including the existing Done
transaction; any later explicit source action keeps its own write authorization.
