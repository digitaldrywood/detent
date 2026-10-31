# Hub PostgreSQL schema

This directory owns the PostgreSQL schema and transaction boundary for the
core records and request metrics families. Provision `roles.sql` as a database
administrator, then apply `migrations/` with the Goose PostgreSQL dialect as
`detent_migration` on an empty PostgreSQL 17 database. Existing installations
apply only the new timestamp migration after provisioning the roles. The
SQLite migration chain and runtime remain separate; this package does not
switch the Hub database backend or import tenant data.

## Core persistence boundary

`../corestore` defines the domain records and repository contracts for projects,
work items and comments. `sqlc.yaml` and `queries/` generate the private SQL
adapter in `pgquery/`; the local store's sqlc configuration is independent.
`make generate` regenerates both adapters and `make check-generated` checks both.

Construct one `NewRepository` at Hub startup with a single runtime `sql.DB` and
operator-supplied `PoolConfig`. Inject that repository into serving and background
owners, and close it once at shutdown. Never construct a repository or pool per
organization. The repository owns the supplied pool after successful construction;
the caller closes it on construction failure. `MaxConnections` bounds physical
connections; positive serving and background connection budgets must fit together
within that limit. Each workload waits cancellably for its own budget before
acquiring a connection, reserving capacity for the other workload. Both reads and
writes reuse the same pool, with read-only transactions for reads.

`Read` and `Write` bind one authenticated organization for the callback transaction
and explicitly select `READ COMMITTED`, including when a server or login defaults
to another isolation level. Callbacks perform only request-bounded database work;
never hold a transaction across object-storage calls, network calls or history
walks. Store compressed payload objects before writing their typed references.
Callback errors, panics and context cancellation roll back the transaction. Retain
neither the reader nor writer beyond the callback, and do not nest repository
callbacks. Transaction methods also take the request context so statement waits
cancel with the caller.

Updates compare the supplied revision and increment it atomically. Keyset pages
require a limit from 1 through 200. Unique violations map to
`corestore.ErrUniqueViolation`, and PostgreSQL `40001` and `40P01` map to
`corestore.ErrSerializationFailure`; both retain `mutation.ErrConflict`
classification. Cancellation preserves `context.Canceled` or
`context.DeadlineExceeded`. Missing records retain `sql.ErrNoRows` and map to
`corestore.ErrNotFound`. Error chains preserve the database cause for diagnostics
without rendering PostgreSQL detail or customer content in the error message.
The repository does not retry transactions; the application retry owner chooses
the next attempt from the classified primary failure.

At INV-21 scale, pool and budget sizes remain constant across 10,000 tenants and
10,000 runners. Identity reads and writes use tenant-leading indexed keys and
touch one requested entity. A page reads at most 200 matching rows through its
tenant-leading keyset index. Startup validates the runtime role, and each
transaction adds one tenant-setting statement. No periodic work, tenant pool
registry, history scan or derived write is introduced (INV-18).

## Roles and transaction scope

`roles.sql` provisions three cluster-wide group roles without login, superuser,
role administration, database creation, replication or `BYPASSRLS` privileges.
It rejects pre-existing groups with elevated attributes or combined authority.
The schema grants apply only to the selected database. Re-running provisioning
preserves existing roles and requires a database administrator; Goose migrations
never create or drop cluster-wide roles.

| Group | Authorized access |
| --- | --- |
| `detent_runtime` | Tenant-scoped SELECT, INSERT, UPDATE and DELETE, plus issue identity sequence usage; no table ownership, schema creation or TRUNCATE. |
| `detent_platform` | Explicitly authorized cross-organization core data access through its own RLS policies; no schema migration authority. |
| `detent_migration` | Core table ownership and schema creation; forced RLS still denies unscoped data access until an authorized migration deliberately changes policy. |

An administrator provisions separate application, platform and migration login
credentials and explicitly grants each login only its corresponding group.
Never grant platform, migration, table-owner or elevated-role membership to the
runtime login, including memberships that do not inherit privileges. Keep
platform and migration pools out of request handling. Credentials and role
membership are installation inputs, never discovered from host state. For
example, after creating the three logins through the credential owner:

```sql
GRANT detent_runtime TO example_runtime;
GRANT detent_platform TO example_platform;
GRANT detent_migration TO example_migration;
```

Migration connections use `SET ROLE detent_migration` before Goose runs, so new
tables and the migration ledger have the migration owner. For an existing
baseline created under a different owner, the administrator first transfers
`projects`, `issues`, `native_comments` and the Goose ledger `goose_db_version`
to `detent_migration` before running the new migration as that role. Migrations run
through a dedicated connection, with no runtime or platform role membership.
The migration login needs no superuser or `BYPASSRLS` attribute.

Construct `NewRuntime(ctx, reader, writer)` from runtime-only reader and writer
pools. Construction rejects privileged identities, reachable elevated roles,
platform or migration membership, table ownership, and disabled or unforced
core RLS. These checks run at startup, not on every request. Pool credentials
and role grants must remain fixed for the lifetime of a runtime instance.

After the existing identity and organization authorization checks succeed,
pass their `tracker.OrganizationID` to `Runtime.Begin(ctx, organization, options)`.
Never derive this argument directly from a URL, header or unverified claim.
This boundary binds an already authorized tenant; it does not authenticate a
caller or replace authorization. Empty or whitespace-only IDs are rejected
before acquiring a connection. Read-only transactions use the reader pool;
other transactions use the writer pool. Use the returned transaction for every
tenant query, and commit or roll it back before returning.

Every transaction binds `detent.organization_id` with parameterized
`set_config(..., true)` before returning a usable transaction. Binding failure
rolls the transaction back. Commit, rollback and aborted-transaction rollback
clear the local value before the connection is reused. Do not set this custom
parameter at session scope or configure a tenant in connection startup options.
An unscoped connection sees no core rows, even if a query omits its tenant
filter. Scope checks remain in queries as an additional boundary. All three
core tables enable and force RLS, with matching read and write checks; missing,
empty and unknown settings deny access, and another tenant's key cannot grant
visibility or satisfy a write policy.

Focused tests use actual restricted logins through `template.OpenRole`, seed two
tenants with overlapping identifiers, and exercise every core table. They cover
omitted and wrong filters, missing and unknown scope, cross-tenant writes,
separate reader and writer pools, binding errors, and reuse of the same backend
PID after commit, rollback and SQL errors. Owner reads also prove forced RLS;
role tests prove explicit platform access and rejected privilege escalation.

## Core family

The source inventory is `../testdata/hub_schema.json`; ownership and inline
payload classification come from `../testdata/hub_classification.json`.
`projects`, `issues` and `native_comments` are organization-owned. The work item
table keeps the existing name `issues`, its numeric `id`, and its text
`native_id`. Project and comment IDs remain text. Explicit IDs can be imported
unchanged; generated project and work item text IDs retain `prj_` and `wi_`
prefixes. Numeric issue IDs and comment `record_id` values accept imported rowids through
the repository create methods. Zero requests a generated identity. An importer
must advance both identity sequences past all imported IDs before enabling
generated writes. Imported revisions, event sequences and timestamps remain
explicit inputs; zero revisions and omitted timestamps use current-write defaults.

The baseline includes core identity, project name and slug, revision and
sequence, source deduplication, title, actor and provenance scalars, archive
and deletion state, and creation and update timestamps. Scheduling rank and
workflow source revisions retain their typed project fields. GitHub ingress,
workflow states, labels, assignees, dispatch, verification, integration settings,
search projections and other families are outside this baseline. Their later
migrations must preserve their source identifiers and establish tenant-safe
relationships when introducing their owning tables.

The classification marks project workflow, states and model selection; issue
body, contract, report context, actor, labels, assignees, provenance and verifier;
and comment body, actor, editor and provenance as inline payload debt. The
baseline replaces project workflow, states and model selection and issue body,
contract and report context with nullable object keys. Comment bodies use a
required object key. Actors, editors and provenance use typed scalar columns.
Labels, assignees and verification belong to later families. Writers store
payloads in compressed object storage before publishing their references;
object keys are references, never encoded inline payloads.

## Pattern for each subsequent family

1. Select one bounded family from the SQLite inventory and classification.
   Preserve identifier types and values, and identify its owning organization
   through the classified ownership path. Add a non-null `organization_id` to
   every tenant table, including children whose SQLite file implied ownership.
2. Lead every primary key, unique constraint and access index with
   `organization_id`. Scope entity numbers and source keys to the owning project
   or work item. Optional source keys remain nullable so unrelated records can
   omit them. Keep empty project slugs outside the unique slug index.
3. Include `organization_id` in both sides of every foreign key. Include
   `project_id` when a child carries both project and work item identity.
   `native_comments` therefore references
   `issues(organization_id, project_id, native_id)`. A parent existing in another
   tenant or another project cannot satisfy the relationship. Parent deletion
   and key changes are rejected while children reference them.
4. Use `timestamptz` for instants, `bigint` for counters and numeric identifiers,
   `boolean` for flags and `text` for text identifiers and scalars. Replace inline
   payloads with typed object references or normalized owned rows. Add no JSON,
   JSONB or encoded payload columns. Preserve current-write validation even when
   legacy import needs recovery handling.
5. Create a fresh UTC timestamp migration with
   `make db-create NAME=description MIGRATIONS_DIR=internal/hubserver/postgres/migrations`.
   Do not modify the SQLite chain or edit a previously landed migration. Supply
   reverse dependency order in Goose Down migrations.
6. Use one `pgtest.NewTemplate` per test process. Migrate its empty database and
   clone isolated databases for focused table-driven tests on PostgreSQL 17.
   Test reused identifiers in two tenants, multiple entities within each tenant,
   same-tenant uniqueness, missing tenant keys, and cross-tenant inserts and
   updates. Keep deadlines derived from the test process and temporary files
   inside the worktree. Follow `../../testenv/pgtest/README.md` for server setup.

These constraints enforce relationship integrity alongside forced RLS.
Application reads and writes must still authorize the organization and bind
its ID in every tenant query and transaction.

INV-18 and INV-20 require typed columns and object references. INV-21 requires
tenant-bounded access: core identity lookups and parent checks use tenant-leading
B-tree keys, and comment pages use tenant/project/work-item/creation-time/record-ID indexes.
At 10,000 tenants these operations use indexed lookups rather than tenant or
history walks. Later readers must bound page sizes; this schema introduces no
request handlers, periodic jobs, fleet scans or shared application locks.
Transaction startup adds one constant-size setting statement, independent of
tenant and history counts, and writes no application rows (INV-18). RLS compares
the setting against the indexed leading tenant key (INV-21); runtime role and
core-table catalog checks run only during pool initialization.

## Request metrics family

`../metricsstore` defines the metrics persistence boundary implemented by the
same `Repository`. Invoke `MergeRequestMetrics` from the background flush owner,
with at most 200 minute observations in one atomic transaction. Each observation
supplies an exact count, sum in milliseconds, maximum and 64 integer histogram
buckets. Bucket zero covers up to one microsecond; subsequent buckets double
the upper bound. `Latency.Percentiles` derives the existing bounded p50 and p95
from the merged buckets. The database stores checked `bigint[]` values, never
serialized histograms or stored percentile estimates. Histogram, count and sum
upserts add to the current row; the maximum uses `greatest`.

`RequestMetrics` uses the serving connection budget and reads tenant-scoped
keyset pages of at most 200 rows within an inclusive/exclusive time range.
Minute, hour and day cursors use the last returned record's complete grain key.
The authenticated organization is an explicit argument to every operation,
and all five metrics tables enable and force RLS. Runtime construction validates
these tables alongside the core family. The migration installs under
`detent_migration`; platform access remains restricted to `detent_platform`.
This family boundary follows the core port: backend selection and legacy SQLite
import remain separate integration work.

Detailed minutes retain 14 days, hourly route histograms retain 90 days, and
daily route/caller-kind totals retain 400 days. The cutoff is truncated to its
grain in UTC; rows exactly at the cutoff survive. `RetainRequestMetrics` compacts
at most 200 expired minute records per call, each in its own transaction,
atomically deleting the detail and adding its count, sum and maximum to a daily
row without caller identifiers. It discards records beyond daily retention,
expires at most 200 hourly and 200 daily records, and removes at most 200 empty
expired caller windows in separate transactions. Repeated calls resume the
backlog; there are no fleet or tenant scans.

Caller attribution retains at most 200 distinct kind/ID pairs per tenant-minute.
New pairs beyond that bound merge into `other` with an empty caller ID, preserving
all counts, sums, maxima and buckets. Existing callers continue to accumulate
under their identities. Caller admission serializes on the tenant-minute row,
with window, minute and hour keys acquired in deterministic order. These locks
cover bounded database work only; callers do not perform external I/O in these
transactions. Expired caller windows cascade to their at-most-200 registry rows.

At INV-21 scale, 10,000 tenants and runners reuse the existing fixed pool and
workload budgets. A merge performs indexed operations for at most 200 input
records, with fixed 64-bucket arithmetic; reads return at most 200 records using
tenant-leading keys. Per retention call, at most 200 one-record compactions,
two 200-row deletes and 200 one-window deletions occur. Each window deletion
removes at most 200 caller keys in one short transaction. This is a fixed tick
budget independent of history or fleet size (INV-18, INV-20, INV-21).

## Record pagination and background sweeps

Work items retain their numeric `id` and native text identity. Comments retain
text `id` for event identity and add bigint `record_id` for imported SQLite
rowids. Both numeric keys are unique within an organization and immutable, as
is `created_at`. Identity allocation is not commit ordering. Mutable item
numbers and comment sequences do not determine user page order.

`WorkItemPage` and `CommentPage` order ascending by `(created_at, numeric ID)`.
An empty cursor starts a query. Version 2 cursors carry that tuple and its
organization, project and optional work-item scope. Version 1 carries a preserved
numeric rowid and the same scope; a tenant-scoped identity lookup translates it
into the current tuple. Missing legacy anchors, unsupported versions and scope
changes require restarting the query. Version 2 continuation works after its
anchor is deleted. Callers retain the existing signed, authorized HTTP cursor
envelope; these storage cursors are not authentication tokens. Legacy numeric
`WorkItems` and `Comments` arguments resolve the old number or sequence once,
then use the same tuple page query.

`ScanWorkItems` and `ScanComments` perform repeating bounded sweeps. A zero scan
cursor captures the highest visible numeric ID for that sweep. Each call reads
at most the requested budget, capped at 200, through that fixed upper bound.
The returned zero cursor ends the sweep and starts the next sweep on the next
tick. Consumers must keep repeating and apply results idempotently by tenant,
native identity and revision; the cursor is never a durable commit watermark.
A transaction that commits behind a sweep's position is revisited next sweep.
New inserts above its upper bound cannot prevent a sweep from completing.

At INV-21 scale, each page uses a tenant-leading B-tree range and returns at
most 200 rows. A sweep start adds one reverse index lookup; subsequent ticks
read at most 200 rows without walking previous pages. No transaction, lock or
connection survives the repository callback. Sweeps introduce no persistent
queue, retry owner or derived writes (INV-18), and all new columns remain typed
(INV-20).
