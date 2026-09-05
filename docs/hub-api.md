# Detent Hub API

Detent Hub owns its SQLite database and exposes fleet coordination through an authenticated HTTP API. Clients must never open or copy the live database files.

This page documents implemented behavior, including native collaboration through `/api/v2`. The [native Hub and Cloud RFC](cloud-hub-rfc.md) defines the broader architecture; runner enrollment, native Changes, hosted identity, artifact custody, and deployment contracts remain separate deliverables.

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

The GitHub webhook route is the only transport-specific exception to bearer authentication. `/api/v1/webhooks/github` uses GitHub's `X-Hub-Signature-256` HMAC authentication and never accepts a Hub token as a substitute.

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
separate from tenant access; hosted membership and enrollment are owned by #2193
and #2184.

Every native project route requires both its organization and project grant.
Unknown, guessed and inaccessible resource IDs return 404. Native machine IDs
are bound to their organization and token principal; another token cannot take
over registration or renew its leases. A legacy registration cannot overwrite a
native machine. Native tokens cannot read legacy global lists through v1.

## Native collaboration protocol

`GET /api/v2/capabilities` reports server identity, supported protocol majors,
event schemas, required native features, request size and page limits. Native
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
| `GET /work-items` | Paged issues; optional exact `state`, `label`, `assignee`, `priority` filters |
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
| `POST /claims` | `machine_id`, unique `session_id`, `ttl_seconds`, protocol negotiation, optional `work_item_id`, workflow and author/assignee/label filters |
| `POST /leases/{lease_id}/renew` | `fencing_token`, `ttl_seconds` |
| `POST /leases/{lease_id}/release` | `fencing_token`, typed `reason` |
| `POST /work-items/{id}/events` | Idempotent, versioned run fact with current lease and typed references |

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
later native edits. Full resumable GitHub import and ownership cutover belong to
#2187; attaching provenance is not a claim that all external history was imported.

Workers can edit their own comments. Operators can edit project comments, with
the editor recorded separately from the original actor and provenance. Explicit
issue and comment content can contain code or secrets and is retained as
collaboration data. The service does not promise that authored text is secret-free.

History records have an event ID, organization/project, aggregate identity and
sequence, type, schema version, server recording time, authenticated actor and
typed data. `issue.created`, `issue.edited`, `comment.created`, `comment.edited`,
`dependency.changed` and `workflow.transitioned` are server-generated. They refer
to content revisions instead of duplicating full text in event data.

Worker schema 1 accepts `run.started`, `run.finished` and `run.checkpointed`.
Their `data` requires `lease_id`, positive string `fencing_token`, and typed
`run_`, `attempt_` and `policy_` IDs. Finished outcomes are `succeeded`, `failed`,
`cancelled` and `interrupted`. Only checkpoints accept `artifact_ids`, at most 20
typed `artifact_` IDs. Arbitrary URLs, signed download capabilities, raw prompts,
transcripts, tool output, local paths, source, diffs and artifact bytes have no
payload fields. The current lease and machine principal must match the item.
Idempotent retries return the committed result; new events with an expired fence
fail. A run outcome records a worker report, not a verified check or merge grant.
Durable run/policy registration and richer recovery bindings are owned by #2186
and #2183; these references do not assert those future authorities exist.

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
this issue does not synthesize GitHub identities or implement native Changes.

Schema migration retains existing issue/repository integer keys, GitHub node IDs,
issue numbers, queue entries, dependencies, leases/fencing, work events and outbox
links. Compatibility repositories receive stable project aliases in the local
organization under the existing instance-administrator boundary. Project grants
are an explicit administrator decision, never inferred from a current login.
Existing v1 clients keep their compatibility IDs and content authority. Native
rows have no mandatory repository or GitHub issue reference and cannot be fetched
by guessing their integer keys through v1. Compatibility event history remains
in v1; no arbitrary historical payload is automatically copied to v2.

## Retention, export and deletion design

Collaboration content, content versions, typed events, idempotency responses and
owner backups all contain retained data. This release has no automatic native
content expiry or issue-deletion endpoint. Append-only triggers deliberately
reject ordinary history deletion. Retention durations, backup expiry and a
privileged maintenance command remain deployment decisions and implementation
work for #2197; append-only storage is not a promise of permanent retention.

The privileged deletion procedure must use the following contract:

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
They do not claim that the future privileged purge/restore maintenance command is
implemented. Until it is, operators must retain backups deliberately and must not
disable triggers on a running Hub as a substitute for supported deletion.
