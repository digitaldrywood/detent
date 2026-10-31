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

Like the core PostgreSQL boundary, this package does not select the Cloud entry
backend or import the existing SQLite files. The SQLite entry service continues
to own its existing files until the shared-database runtime cutover. Backend
selection must switch all entry owners together; do not combine PostgreSQL
sessions with SQLite key, billing, provisioning or platform-administration owners.

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

Run the package and shared-schema tests with PostgreSQL 17 binaries on PATH,
`DETENT_TEST_POSTGRES=binary`, a worktree-local `TMPDIR`, `-race` and an explicit
Go test process deadline. Fixtures clone one migrated template, and concurrent
owners share its two-connection budget. Their assertions cover registry mapping,
login and PKCE exchange, permission scopes, secrets, replay, revocation, expiry,
concurrent mutation and cross-tenant refusals.
