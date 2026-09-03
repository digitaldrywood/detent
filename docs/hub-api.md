# Detent Hub API

Detent Hub owns its SQLite database and exposes fleet coordination through an authenticated HTTP API. Clients must never open or copy the live database files.

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

## Work-item queries and cursors

`GET /api/v1/work-items` accepts repeatable or comma-separated filters: `repository`, `workflow_state`, `readiness`, `priority`, `label`, `assignee`, `machine`, `lease`, `pr`, and `sync_health`.

Supported sort values are `priority`, `created`, `updated`, `identifier`, and `workflow_state`; `order` is `asc` or `desc`. The default priority order uses priority, queue-rank presence, queue rank, creation time, repository owner/name, issue number, and internal ID. Every other sort also ends with repository owner/name, issue number, and internal ID, giving every page a stable total order.

List responses contain an opaque `next_cursor`. Pass it back with the same sort and order. `limit` defaults to 50 and may not exceed 200. Repository freshness and outbox health use the same `limit` and opaque `cursor` convention. Work-item detail timelines use `timeline_limit` and `timeline_cursor`.

Omit `work_item_id` from `POST /api/v1/claims` to claim next. Candidate selection and lease creation execute in one SQLite transaction; clients must not list candidates and then attempt a separate specific claim. Renew, release, and event requests must carry the positive `fencing_token` returned by the claim.
