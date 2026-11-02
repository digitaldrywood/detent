# Platform PostgreSQL storage

This package owns the PostgreSQL registry and authentication storage boundary.
It uses the same database and migration chain as the core Hub repository, with
all platform tables qualified under `platform`. Provision the shared
`internal/hubserver/postgres/roles.sql` as an administrator before applying the
Goose migrations as `detent_migration`.

Give the platform login only `detent_platform` membership. Construct one
`Repository` with an operator-supplied pool, signing key, optional Hub secret
keyring and bounded serving/background connection budgets. Successful
construction transfers pool ownership; close it once at shutdown. Construction
rejects tenant-runtime, migration, owner and elevated authority, including
reachable roles whose privileges are not inherited. It requires the critical
platform tables and their DML grants. No credentials or databases are discovered
from host state.

Cloud entry selects this repository for every global entry owner through one
explicit storage backend. SQLite remains the default. Selecting PostgreSQL does
not import existing files; import and cutover require their own fence and verified
migration. Never combine PostgreSQL sessions with SQLite key, billing,
provisioning or platform-administration owners.

## Records and authorization

Organization, subject, session, binding, client, grant and key identifiers remain
text. Event counters retain importable bigint identities. Registry generation
checks, provider identity uniqueness and idempotent registration are atomic.
Organization enumeration uses an ID cursor and pages of at most 200 records.

The platform role intentionally crosses organizations. It is separate from the
untrusted tenant runtime role, which has no schema, table or sequence access.
Every tenant-specific repository operation includes `organization_id` in its
lookup and mutation. Tenant children reference their parent through composite
organization keys; OAuth requests also carry the grant's client ID. Global
credential hashes remain globally unique for unambiguous credential routing.

These methods persist already authenticated application decisions. The serving
owner must retain provider verification, current membership/role checks, CSRF,
client authentication, endpoint/resource validation and permission checks before
calling them. `Key` supplies the owner needed for membership revalidation;
`KeyAuthority` applies persisted permission, tenant/project context, personal-key
policy, approval and exclusions. It does not replace current provider membership.
Platform-member roles remain the existing admin/support/billing/viewer values.

Session and authorization identities have typed scalar columns. Key selections
and project exclusions have separate relational tables. Auth and registry
notification queues remain separate (`auth_notifications` and
`platform_notifications`). Audit detail belongs in compressed object storage;
the audit row has an object reference and typed event, retry, amount and reason
fields. The schema contains no JSON columns or JSON SQL operations.

## Secrets and concurrent mutation

Provider and GitHub tokens retain the existing signing-key-derived AES-GCM seal
and `hk1` envelope encoding. Versioned envelopes retain the existing purpose and
binding AAD, so copied ciphertext cannot grant another authorization or purpose.
Login verifiers and invitation tokens are also sealed, then cleared on
consumption. Supply the same signing key and retained keyring versions when
reading imported credentials or restarting the storage owner.

Login consumption is an atomic conditional update returning the locked original
record. It refuses replay, mismatched transactions and expiry boundaries.
Authorization replacement and refresh compare-and-swap lock the session row.
Session revocation updates that same row, and every authorization/token read
checks it. A committed revocation cannot be followed by a stale authorization or
refresh that restores access. Revoked provider bindings cannot be reactivated;
a fresh provider session creates a new binding.

OAuth code consumption, refresh rotation and grant revocation serialize on the
grant row. Codes are single-use and bound to organization, client, resource,
redirect URI and S256 verifier. Reusing a spent refresh token revokes its own
grant; other tenants remain unaffected. External provider refreshes remain the
application refresh owner's responsibility; no transaction spans a provider call.

## Cost and validation

INV-18: each mutation touches only its requested record and directly owned
children. Session revocation writes one row regardless of authorization history;
read checks enforce the result. No cleanup, backfill or derived refresh occurs
inside a request transaction.

INV-20: identities and scope lists are typed, with audit payload references.
Stored ciphertext is secret material, not serialized identity or audit payload.

INV-21: one fixed-size pool serves 10,000 tenants and 10,000 runners. Session,
provider, binding and credential lookups use indexed keys. Authorization mutation
locks one session briefly; OAuth mutation locks one grant briefly. A selected key
contains at most 200 organizations and 200 projects per organization; scope reads
have fixed row limits. No history scan, network call under a lock, tenant pool
registry or periodic fleet work is introduced.

Run the package and shared-schema tests with PostgreSQL 17 or 18 binaries on PATH,
`DETENT_TEST_POSTGRES=binary`, a worktree-local `TMPDIR`, `-race` and an explicit
Go test process deadline. Fixtures clone one migrated template, and concurrent
owners share its two-connection budget. Their assertions cover registry mapping,
login and PKCE exchange, permission scopes, secrets, replay, revocation, expiry,
concurrent mutation and cross-tenant refusals.

## Cloud entry backend selection

The entry configuration accepts `storage.backend: sqlite` or `postgres`.
PostgreSQL requires a platform-only DSN supplied through an environment variable,
explicit fixed connection budgets and the existing immutable attachment storage:

```yaml
storage:
  backend: postgres
  postgres_dsn_env: DETENT_PLATFORM_DSN
  max_connections: 4
  serving_connections: 3
  background_connections: 1
```

Keep the existing assertion signing key and retained Hub keyring versions. The
runtime never opens the SQLite registry or auth file in this mode. Invalid roles,
missing tables or incomplete storage configuration fail startup. The SQLite-only
disposable staging snapshot is rejected for PostgreSQL; use an isolated staging
database. Migration authority remains external to entry startup.

Registry, sessions, provider bindings, API keys, OAuth, GitHub connections,
provisioning, billing, notification queues, archive bindings and administration
share the selected repository and its serving/background budgets. Entry SQL uses
scoped read-committed transactions with a protected platform search path. Registry
and auth notification queues retain separate table identities. Registry and auth
reconciliation remain separate durable transactions; each existing atomic group
keeps one commit. Object writes finish before administration receipt transactions.
Structured output and support notes are compressed into immutable objects, and
receipt rows store typed output fields and object references. Secret status reads
through the service inspect the selected backend.

Organization identity, monotonic generations, deletion fences, immutable billing
mappings and retained administration/entitlement history preserve the SQLite
contracts. Generated organization slugs allocate a suffix through one counter row
per base. A preexisting custom alias uses a deterministic organization suffix;
allocation does not scan an expanding collision history. Imported slug aliases
remain authoritative.

The pool count is constant at 10,000 organizations and 10,000 runners. Point
operations use indexed identities and lock only the requested session, grant or
organization. Slug allocation touches its base counter, organization and alias;
receipt writes touch one receipt. Platform-member changes serialize only the
small member registry, without provider or object calls under that database lock.
The port retains the entry application's existing enumeration and reconciliation
contracts and adds no fleet scan or per-organization pool. The invariant scope is
INV-16, INV-18, INV-20, INV-21 and INV-22.

The entry backend-parity journeys exercise SQLite and disposable PostgreSQL with
isolated SQLite tenant Hubs: login and tenant routing, revoked keys, OAuth exchange
and refresh replay/rollback, provisioning/deletion, billing, notification identity,
receipt replay, selected-backend secret diagnostics and keyring-compatible restart.

## Platform snapshot transformation

`Repository.NewImporter` binds a frozen registry/auth snapshot to the P28 catalog,
both exact source schema manifests, one snapshot ID, the complete organization
allowlist, the original assertion signing public key and retained keyring versions.
The source pool must have an explicit connection limit. Both staging schemas use
text columns with primary B-tree keys in source key order and `C` collation.
Unknown columns, changed manifests, missing indexes, unsupported platform tables,
foreign organizations and unavailable signing/keyring authority fail closed.
Engine catalogs, schema-version rows and SQLite sequence bookkeeping remain
source-only; PostgreSQL sequences advance beyond every imported history ID.

Start with an empty platform target. Fence all platform writers and keep both
source schemas immutable through import and verification. The importer neither
fences writers nor enables serving or cutover. It is an offline storage owner,
not a route, command or background daemon. Do not change a snapshot's identity or
reset its checkpoint to import over existing platform authority.

Call `Step` with one to `ImportLimit` source records. A single durable typed
checkpoint binds the snapshot and catalog digest, resumes by source primary key,
and commits with the corresponding target rows and children. A competing step
with an older revision returns a revision conflict. Cancellation or a rejected
batch leaves its progress unchanged; restarting with the same manifest resumes
that batch. Completion replay writes nothing and cannot restore revoked access.
Registry and auth families retain their existing separate transaction boundaries.

Session and authorization identities expand into typed columns. Selected key
organizations/projects and blocked projects become relational children; reach,
approval and revocation state remain authoritative. OAuth child records resolve
the original grant's organization through an indexed lookup and retain code
consumption, refresh rotation and spent-token history. Provider and GitHub
ciphertext is validated with its original purpose and binding, then copied
unchanged. SQLite's plaintext login verifier and invitation fields are sealed
using the selected platform cipher, preserving their values on consumption.

Structured and support audit events retain their original bytes in the same
immutable compressed object format used by entry serving. Credit amounts decode
as exact decimal strings. Administration receipts expand into typed fields and
verified output objects; the greatest original audit ID selects the latest
receipt independently of staging text-key order. Object preparation and readback
finish before the database transaction. A failed database write may leave an
unreferenced content-addressed object; retry uses the same object identity.

INV-16 keeps migration authority and receipts out of repository runtime notes.
INV-18 limits each commit to its batch and directly owned children. Records are
capped at one MiB and key selections retain the serving limits of 200
organizations and 200 projects per organization. INV-20 keeps all target fields
typed, with audit/output data in objects. At 10,000 tenants and runners, INV-21
cost is at most `ImportLimit` indexed source rows per step plus their bounded
children, one checkpoint lock during database writes, and no object I/O under
that lock. Startup checks the fixed platform table inventory; no fleet scan or
per-tenant connection pool is introduced.
