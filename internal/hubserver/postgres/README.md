# Hub PostgreSQL schema

This directory owns the PostgreSQL schema and transaction boundary for the
core records, request metrics and platform registry/authentication families.
Provision `roles.sql` as a database administrator, then apply `migrations/`
with the Goose PostgreSQL dialect as
`detent_migration_v2` on an empty PostgreSQL 17 database. Existing installations
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

The versioned names isolate this authority family from roles left by prior
installations. `postgres.RoleMigrations` renders the historical migration SQL
with exact legacy role identifiers mapped to these names when Goose reads it;
the checked-in migration files remain unchanged.

| Group | Authorized access |
| --- | --- |
| `detent_runtime_v2` | Tenant-scoped SELECT, INSERT, UPDATE and DELETE, plus issue identity sequence usage; no table ownership, schema creation or TRUNCATE. |
| `detent_platform_v2` | Explicitly authorized cross-organization core data access through its own RLS policies; no schema migration authority. |
| `detent_migration_v2` | Core table ownership and schema creation; forced RLS still denies unscoped data access until an authorized migration deliberately changes policy. |

An administrator provisions separate application, platform and migration login
credentials and explicitly grants each login only its corresponding group.
Never grant platform, migration, table-owner or elevated-role membership to the
runtime login, including memberships that do not inherit privileges. Keep
platform and migration pools out of request handling. Credentials and role
membership are installation inputs, never discovered from host state. For
example, after creating the three logins through the credential owner:

```sql
GRANT detent_runtime_v2 TO example_runtime;
GRANT detent_platform_v2 TO example_platform;
GRANT detent_migration_v2 TO example_migration;
```

Migration connections use `SET ROLE detent_migration_v2` before Goose runs, so new
tables and the migration ledger have the migration owner. For an existing
baseline created under a different owner, the administrator first transfers
`projects`, `issues`, `native_comments` and the Goose ledger `goose_db_version`
to `detent_migration_v2` before running the new migration as that role. Migrations run
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
the repository create methods. Zero requests a generated identity. A restricted
sequence allocator advances past imported IDs without moving backward. Generated
allocations share a transaction lock; imports take its exclusive counterpart,
so advancement cannot race with another repository allocation. Runtime roles
can call the allocator only for the two core record sequences and cannot call
`setval` directly. Imported revisions, event sequences and timestamps remain
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
`detent_migration_v2`; platform access remains restricted to `detent_platform_v2`.
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
numeric rowid and the same scope, and its continuation keeps ascending rowid
order for the whole result set so no unseen record is skipped. Missing legacy anchors, unsupported versions and scope
changes require restarting the query. Version 2 continuation works after its
anchor is deleted. Callers retain the existing signed, authorized HTTP cursor
envelope; these storage cursors are not authentication tokens. Legacy numeric
`WorkItems` and `Comments` arguments retain number and sequence ordering,
respectively, including continuations whose anchor has been deleted. Their
queries use the existing tenant-leading number and sequence indexes.

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

## Platform registry and authentication

The platform family lives under the `platform` schema, provisioned by `roles.sql`
with `detent_migration_v2` ownership and only `detent_platform_v2` usage. Its migration
shares this directory and Goose ledger with the tenant families. Tenant runtime
roles receive no platform access. The `roles.sql` schema definition is also an
input to sqlc; generated platform models stay in `pgquery`.

`../../cloudentry/platformstore` owns the typed registry/authentication adapter
and its separate platform pool. Its README defines the preserved identifiers,
secret envelopes, permission boundary and concurrent session/revocation behavior.
Platform storage is not a tenant repository and must never be injected into a
tenant-scoped request handler. Backend selection and existing-file import belong
to the shared-database runtime cutover, alongside the other PostgreSQL families.

## Events, changes and command receipts

`../eventstore` defines the event and change persistence boundary on the existing
`Repository`. `ReadRecords` and `WriteRecords` share its tenant transactions,
connection budgets, cancellation and error classification. This family contains
`collaboration_events`, `collaboration_versions`, `change_requests`,
`change_issue_links`, `change_versions`, `change_evidence` and `native_commands`.
All seven tables force RLS and use tenant-leading keys. Version relationships
include the owning change, preventing evidence or current-version references
from naming another change's version. Current-version validation is deferred to
commit so an import can insert the change and its versions in one transaction.

Use `eventstore.Content` with the existing compressed `attachment.DetailStore`.
Its preparation methods decode legacy JSON in Go, project typed identity,
actor, source, revision, timestamp and provenance fields, write the payload
object and verify it before returning the row to insert. Complete payloads retain
fields that are not query keys, including actor attribution and evidence bodies.
Objects use tenant-bound content-addressed keys, compressed digests and checked
compressed and original sizes. No inline payload or SQL JSON operation is used.
Prepare objects before a repository callback and hydrate returned rows after the
callback ends; neither object reads nor writes belong inside a database transaction.
Missing, corrupt and mismatched objects fail with `artifact.ErrIntegrity`.

`AppendEvent` locks only its owning issue and allocates its next sequence in the
same transaction as the event insert. Imported positive sequences remain exact
and advance the counter only when larger. Evidence uses the same rule on its
owning change. Rollback restores allocation. Event IDs, collaboration revisions,
version IDs and evidence source keys replay unchanged receipts; a changed receipt
conflicts. Imported evidence without a source key replays by its explicit sequence.
Events, collaboration versions, change versions, evidence and command receipts
reject every update and delete, including changes to object references.
`UpdateChange` compares and increments its revision. Linked issues are explicit
rows and must belong to the same tenant and project as the change.

Command receipts retain the organization/actor/operation/key identity and typed
request hash. `PutReceipt` atomically inserts or returns the existing response
reference for the same hash; another hash conflicts. A writer losing a concurrent
insert reads the committed receipt in a subsequent `READ COMMITTED` statement.
Callers check existing receipts before applying a command and publish the response
in the command's write transaction. Authentication and authorization still belong
to the existing application owner; actor credential foreign keys await the access
family. Receipt payloads retain the original response across retries.

Event and evidence pages seek by sequence, versions by number, changes by ID and
links by work item ID. Every page contains at most 200 records. At INV-21 scale,
10,000 tenants and runners share the fixed pool. Each append performs a constant
number of indexed reads and writes and holds one parent lock across database work
only; receipt insertion holds one unique-key lock. No history scan, fleet lock,
periodic task or derived write is introduced (INV-18). Object payloads stay outside
the database (INV-20); tracker history and provenance remain authoritative (INV-16).
The SQLite runtime, backend selection, whole-database import, run/attempt records,
change source bundles, policies, landing receipts, session attribution and other
record families remain separate follow-on work.

## Attempt claims and leases

`../leasestore` defines the lease transaction boundary on the existing repository
and serving/background budgets. `ReadLeases` and `WriteLeases` bind the authorized
organization exactly as the core family does. A write callback is the complete
idempotent transaction: return its first failure and retry the entire callback
through the application's existing retry owner when classified as
`corestore.ErrSerializationFailure`. The repository never retries a statement
or callback. Both statement and commit failures preserve `40001`/`40P01` and
`mutation.ErrConflict`; all preceding writes roll back together. Keep lease,
session, run and attempt identifiers and object references stable across retries.

The family contains `machines`, `leases`, `native_attempt_runs`, `native_attempts`
and `native_attempt_events`. Every key, relationship and access index starts
with `organization_id`; all five tables force RLS and are checked during runtime
construction. Leases reference their tenant's numeric issue identity and machine.
Attempts reference the exact lease, fence, issue and captured work-item revision.
Native project and item identities resolve through that issue, without a second
independently writable scope. Run identity binds to one item while allowing
successive attempts of the same run on that item. Attempt payloads, checkpoints
and artifact manifests are object references, never inline encoded content.
Machine routing/capabilities, attempt projections and event payload history
remain with their owning families; this schema does not activate the backend
or import legacy SQLite data.

`Claim` locks only its requested issue with `FOR NO KEY UPDATE`, checks the current
revision and archive state, and locks its unreleased lease if present. An active
claim by the same machine and session replays the original lease without extending
it; another owner conflicts. A replacement expires the previous lease, cancels
its running attempt and inserts a fresh, increasing fence in the same transaction.
The partial unique index independently enforces one unreleased lease per item.
Session IDs cannot be reused within an organization. PostgreSQL time is sampled
after row-lock acquisition. Renewal, expiry and release use conditional
`UPDATE ... WHERE ... RETURNING` checks on the tenant, lease, fence and expiry.
No tenant-wide writer lock, sweeper, brake or retry owner is introduced (INV-3).

`RecordAttempt` holds the lease row while checking live authority, advancing the
next sequence and writing its content digest receipt. Exact receipt replay is
read-only, including after release; conflicting content, run identity, generation
or sequence is rejected. Completion does not release a lease. Explicit release
or expiry removes its capacity usage and cancels only a still-running attempt;
a completed attempt retains its outcome. `Usage` counts only unexpired,
unreleased leases for the requested machine through the tenant/machine/expiry
index. Expired leases consume no capacity even before replacement or expiry
recording. The authenticated caller still owns claim eligibility, policy,
routing and capacity admission before this storage transaction; its captured
item revision must match at claim time.

Capacity reservation is a separate follow-on family in
[#2025](https://cloud.detent.build/drywood-creek-consulting/detent/issues/2025): provider-account admission,
runner and project limits, monthly budget admission, retained expired renewal
and grouped landing reservations require their typed policy/routing tables and
must be checked and reserved atomically before backend activation. This port
does not replace those existing SQLite owners or reinterpret their limits.

At INV-21 scale, 10,000 tenants and runners share the existing fixed pool. Each
claim, renewal, release, expiry or attempt event uses a fixed number of indexed
point operations and touches only that request's issue, machine, lease, run,
attempt and receipt. Release/expiry cancel at most one attempt through the
unique tenant/lease key. Usage reads only the requested machine's live leases,
bounded by its admitted execution capacity, with no expired-history walk.
Callbacks never perform external I/O or derived projection work while locked
(INV-18). There is no tenant registry, periodic scan or fleet fan-out.

## Runner runtime state

The runner's `internal/store` SQLite file has three responsibilities: disposable
observations, a cache of durable records and policy whose native authority belongs
in the Hub, and local process cleanup. Classification is by field and consumer, rather
than by database file: `codex_sessions` and `work_attempts` contain both durable
history and local cleanup identities. Opening a fresh local database is supported
for both the enrolled runner and the local CLI/board. This classification does
not switch either runtime's database backend or remove the local board's store.

| Family and local records | Classification for native runner jobs | Authority and continuation |
| --- | --- | --- |
| Source commits, staged/unstaged changes and untracked work represented by native checkpoints and immutable Change versions | Published to the Hub | The first portable family. Hub recovery selects the exact version and its verified source bundle, or the recorded checkpoint ref. Another runner restores it into its own workspace before starting the provider. The original SQLite file and workspace are unnecessary. |
| `worker_generations`; worker PID, PGID, start time, owner generation, scratch reservation and reap outcome in `codex_sessions`; active process ownership in `work_attempts` | Needed for local process cleanup | Local ownership identifies which processes and scratch paths this controller may clean up. Keep PID plus start-time checks and live sibling-generation exclusion. Neither a missing database nor a reused numeric attempt ID proves that an old worker exited. |
| `project_runner_setup` | Disposable | A setup hash is an observation of a checkout and hook configuration. Re-run setup in a fresh workspace; never use an old hash to authorize a different checkout. Native dependency removal belongs to #2033. |
| `session_progress`, `scheduler_decisions`, `project_dispatch_status`, `health_notification_states`, `staleness_warning_states` | Disposable | Recompute current progress, dispatch and health observations. Local diagnostics may retain them, but later jobs cannot require them. Required historical diagnostic evidence belongs in Hub runtime receipts (#2028); native dependency removal belongs to #2033. |
| `lane_ledger`, `lane_ledger_instance`, `lane_observations`, `workflow_history_revision` | Disposable | Local projections and installation observations do not replace tracker history, workflow intent or the current issue contract. Rebuild from their authorities (#2033). |
| `issue_contract_rollout.activated_at` | Published to the Hub as durable policy | The original cutoff controls contract enforcement for issues without explicit contract state. Preserve it across store and runner replacement through the existing Hub policy authority, or explicitly retire the cutoff dependency in #2033. Recreating the SQLite store must not silently grant exemptions to previously enforced issues. |
| `issue_progress_credits`, `issue_park_acknowledgements`, `merge_required_check_streaks` | Disposable | Historical hints never veto a native claim. Re-evaluate source, review and current external facts through the existing owner; remove obsolete native dependencies (#2033). |
| `detent_runs`; session identity, completion and usage in `codex_sessions`; attempt history in `work_attempts`; `work_attempt_sequence`, `usage_events`, `workflow_phase_events`, `efficiency_receipts`, `provenance_attribution_boundaries` | Published to the Hub | Existing native attempt/runtime observations provide partial publication. Complete durable diagnostics, timing, usage and provenance publication and native report reads in #2028. Local numeric IDs remain diagnostic attributes, never global job identities. |
| Provider thread/session identity and session configuration in `codex_sessions` and `work_attempts` | Published to the Hub when required for continuation | Bare provider IDs do not make a session portable. Required continuation identity and provider-supported artifacts need Hub publication (#2027). Until then, verified source recovery can start a fresh provider conversation; existing same-machine session reuse remains an optional optimization. |
| `validator_verdicts` | Published to the Hub | Decisions, context-bound findings and comment receipts belong with immutable Change evidence. Existing native reviews cover accepted evidence; complete memo publication and context-valid reuse in #2029. |
| `security_audit_runs`, `security_audit_dispositions` | Published to the Hub | Audit provenance and dispositions belong with the current source/version and independent review identity. Complete publication in #2030. |
| `backlog_admission_proposals`, `backlog_admission_runs`, `backlog_admission_run_issues`, `backlog_admission_downstream_outcomes`, `backlog_admission_declines` | Published to the Hub | Proposal, decision and external publication receipts survive runner replacement through the existing admission owner (#2031). |
| `backlog_admission_malformed_results` | Disposable | Malformed-result observations never become durable dispatch vetoes. Publish any required diagnostic receipt through the admission owner (#2031). |
| `retro_runs`, `routine_runs`, `routine_findings`, `scheduled_runs` | Published to the Hub | Automation identity, findings and scheduling/publication receipts survive runner replacement through their existing owner (#2032). |
| `human_questions`, `budget_overrides` | Published to the Hub for native work | Human intent and spending policy come from their existing Hub authorities. Preserve local CLI/board intent in the local store; isolate native consumers in #2033. Subscription quota never determines eligibility or retries. |
| `organizations`, `projects`, `token_grants`, `api_keys`, `api_usage_logs`, `auth_magic_links`, `auth_sessions` | Published to the Hub for native work | Native organization/project access and authentication come from authenticated Hub context and declared runner enrollment. These local tables retain local CLI/board configuration, authorization and audit behavior; they are not credentials to copy into jobs (#2033). |
| Goose migration bookkeeping and SQLite internal tables | Disposable | Database-local implementation metadata has no job continuation meaning. |

Opening a fresh SQLite file alone does not preserve issue contract rollout
policy: its migration initializes `activated_at` to the current time. An issue
without explicit contract state whose creation or stage-update time falls between
the original and replacement cutoffs would become exempt. Until #2033 publishes
the original cutoff or explicitly retires that dependency while preserving
enforcement, replacing the store requires preserving this policy. The fresh-store
process-cleanup tests do not establish policy portability; this classification
does not change runtime enforcement or implement policy transfer.

The local cleanup exception does not permit storing the only copy of unfinished
work on a runner. Process ownership is not source custody. A missing cleanup
record fails closed for signaling and cleanup acknowledgement, while another
eligible runner can continue from acknowledged, quiescent Hub-retained source.
Cleanup must not use another machine's published PID as local authority.

Portable source recovery reuses the existing `ChangeSourceExecution` and
`workspace.ChangeSource` owners. A source bundle retains committed source and
its checked digests; a checkpoint ref can retain staged, unstaged and untracked
changes before finalization. Recovery verifies the selected source, current
version, policy and execution lease. It starts a fresh provider conversation
when no verified continuation is available, preserving the distinction between
resuming durable work and reusing a provider session. Private provider files and
credentials are not transferred. The PostgreSQL event/change adapter preserves
the source manifest in its compressed version object, behind tenant-scoped typed
version keys. Source bundle storage and backend cutover retain their existing
separate owners; this classification introduces no new migration or inline
payload column.

The remaining family issues #2027 through #2033 have typed dependency edges to
#2022 and preserve local CLI/board behavior. Their issue bodies own implementation
scope and acceptance; this table defines the state ownership boundary (INV-16,
INV-17). At INV-21 scale, the selected recovery path reads one requested immutable
version and its size-bounded bundle, or one exact checkpoint ref. It does not
scan runner files, tenants or attempt history to discover source. PostgreSQL
uses the existing shared pool and tenant-leading keys. No new hot path, shared
lock, periodic task, retry owner or fleet fan-out is introduced.


## Bounded storage conversion

The single conversion owner remains `../storage_conversion.go`. Its tick drives
SQLite and PostgreSQL through `storageconversion.Storage`, preserving fair
family cursors, row/byte budgets and the existing background cadence. The Hub
continues selecting SQLite until backend cutover (#2020). `ConversionStorage`
implements storage operations for the PostgreSQL `runs`, `versions` and
`evidence` families; it has no scheduling loop or retry owner (INV-3).
It uses the existing `Repository`, background connection budget and compressed
`attachment.DetailStore`. Other SQLite families remain with their unported stores.

Current writes and existing PostgreSQL records use `payload_format_version=2`.
An import can explicitly preserve a historical object reference with version 1;
zero-valued repository inputs select the current format. Import payloads are
compressed objects before insertion, never inline staging columns (INV-20).
The format version describes the record's payload representation, independently
of its archive codec/schema and event schema. Conversion accepts a deterministic
family transform that preserves domain identity and produces the current payload.
The original reference and object remain readable after conversion.

Each PostgreSQL adapter is bound to an authenticated organization and supplies
three durable family cursors. Pending partial indexes return one old-format
record per family turn across that organization's projects and aggregates.
Cursor reads and writes are tenant restricted. Exhausted keysets wrap, including
after empty ticks, so late commits behind a cursor remain discoverable. Failed
records advance the current sweep and remain pending for the next tick; the
owner's per-tick visited set prevents retrying them within that tick. Restarting
an adapter resumes durable cursors, and converted rows leave the pending indexes.
Scoped repository pages additionally support bounded import inspection through
a fixed visible upper bound, rejecting cursor reuse in another scope.

Object reads, transformation, uploads and verification happen after the reader
transaction ends and before the writer transaction starts. The converter
derives the exact content-addressed target key before upload, checks it before
publishing, and reconciles even a failed upload by reading and verifying that
same key.
A lost database acknowledgement is recovered from the current record version.
No database transaction, connection budget slot or shared lock spans object I/O.
Missing or corrupt sources and unverified targets leave the old reference intact.

`convert_history_payload` is the only runtime path that replaces an immutable
history reference. It uses a fixed search path and the migration owner's forced
RLS policies, requires the bound organization to match, and conditionally compares
project, aggregate, explicit record identity, source key, digest, both sizes and
format version. Only a matching version-1 record advances to version 2. Its
trigger permits this reference-only transition under the migration identity;
ordinary runtime updates, deletes and metadata changes remain forbidden. The
same transaction updates one aggregate's typed progress row. A rejected write
rolls both changes back, and concurrent conversions count only the winner.
Runtime connections can read progress but cannot fabricate it (INV-18).

At 10,000 tenants and runners, pool and background budgets stay fixed. Each
tick uses tenant-scoped pending-index seeks and examines at most 100 records
across the three families, independent of tenant, project and history counts.
Object work stops at a 1 MiB source-plus-target byte budget, allowing one final
archive-sized record to avoid starving large payloads; an individual
source or target cannot exceed the existing 32 MiB archive limit. Each successful
conversion holds one history row and one aggregate progress row only during
bounded database work. There are no fleet scans, history walks, per-tenant pools,
unbounded transactions or SQL JSON operations (INV-18, INV-20, INV-21).

## Shared tenant unit of work

`tenantstore.Repository` exposes `ReadTenant` and `WriteTenant`. Their scoped
reader and writer combine the core, event and lease family operations. Each
callback uses one generated query binding, one tenant RLS context and one
read-committed transaction. A write commits once after the callback succeeds;
a callback, statement, commit or cancellation failure rolls back the whole unit.
The family-specific callbacks remain available for independent transactions.
Sequencing them does not provide cross-family atomicity.

Prepare and verify payload objects before entering a write callback. Pass their
typed references to the scoped operations, then hydrate returned references after
the read callback ends. The scope exposes database operations only, with no
object storage, transaction creation, nested callbacks or commit controls.
Scope values are valid only within their callback. Keep all participants of a
claim, completion or idempotent command in that callback; do not capture and call
another repository to extend it.

Shared scopes use the same fixed serving and background budgets as the existing
family callbacks. They acquire one slot and one connection for the entire unit,
not one per family. The transaction binds the authenticated organization locally,
rejects mismatched writes and releases the binding when it ends. Read scopes
remain read-only, and both modes explicitly use read-committed isolation even
when the connection default differs. The callback executes once; retry ownership
remains with the caller. Primary callback, statement and commit errors retain
their domain classification and SQLSTATE through rollback, including serialization
failures, deadlocks, revision conflicts and cancellation.

At 10,000 tenants and runners, opening a scope performs one budget acquisition,
one transaction begin and one tenant-context binding, independent of fleet and
history size. Work and row locks inside the callback are bounded by the command's
participants. No per-tenant pool, fleet scan, object I/O or derived refresh is
introduced (INV-16, INV-18, INV-21). SQLite remains on its existing transaction
implementation; this boundary does not enable unported serving paths.

## Raw source mappings (P28)

The [source catalog](../../storagecatalog/README.md) pins all P01 inventory
columns and their source definitions in `tools/storageinventory/mappings/v1.json`.
The storage inventory command rejects unmapped or changed columns as well as
unmapped tables and owners. Each supported mapping names existing typed target
columns; the PostgreSQL tests verify those declarations against the migrated
schema. Unsupported family fields and relational/object extractions remain
explicitly pending and reject import. Catalog coverage is not import coverage.

Source schema/version and organization bindings are explicit. Decoding preserves
IDs, revisions, fencing tokens, receipt identities and encrypted bytes; it rejects
malformed values, ambiguous encodings and timestamp precision loss. Payload
mappings preserve exact bytes and require the later importer to prepare and
verify objects before committing references and receipts within P01's atomic
groups. This catalog adds no schema, serving path, migration execution, network
work or SQL JSON extraction. Its per-row cost depends only on the supplied row,
with constant-time tenant membership checks, independently of fleet size.

## Tenant startup selection (P03)

Hub `Config.DatabaseBackend` selects `sqlite` (also the empty default) or
`postgres`. PostgreSQL requires `Config.Postgres` and forbids `DatabasePath`,
SQLite credential maintenance and SQLite binding migration. Supplying PostgreSQL
settings without selecting that backend is an error. Selection happens before
any filesystem access, lock, SQLite connection or migration; failures never fall
back to SQLite. Existing local startup and file ownership remain unchanged.

`postgres.Config` supplies a concrete `pq.Config` and fixed `PoolConfig`.
Host, port, database, login and TLS mode must be explicit. Use
`pq.NewConnectorConfig` semantics, without environment parsing, service files,
password files, startup options, tenant settings or caller-supplied session
parameters. Install configuration supplies credentials; startup discovers none
from other checkouts or host caches (INV-17). The lifecycle owns one pool and
the shared tenant repository, closes failed construction immediately, and closes
successful construction once when the Hub database owner shuts down. Connection
and statement waits use caller cancellation, with no added timeout policy.

Runtime startup performs the existing restricted-role and forced-RLS checks,
then requires the migration-owned public Goose ledger to contain exactly the
embedded migration versions. Missing, pending or newer migrations fail closed.
Runtime has read access to that ledger, never migration authority. Startup
does not apply migrations or provision roles. `hubserver.MigratePostgres` is a
separate installation operation with its own explicit connection configuration,
one connection and a migration-only login. Every migration connection assumes
`detent_migration_v2`, including reconnections; runtime, platform, combined and
elevated identities are rejected. Provision `roles.sql` separately before this
operation. Migration connections never enter serving or background owners.

The database lifecycle can initialize and use the ported repository contracts.
Full `hubserver.Open` currently closes that lifecycle and returns
`ErrPostgresServingUnavailable` before constructing the tracker, handlers or
workers. Remaining family ports and the cutover owner must wire all enabled
operations before removing this refusal. No PostgreSQL selection can currently
dispatch work, allocate a lease, run SQLite maintenance or open a SQLite file.

At 10,000 tenants and runners, one fixed pool and serving/background budgets
cover all tenant callbacks. Startup checks build-sized schema metadata and
role catalogs once, with no tenant enumeration, periodic work, derived writes
or per-tenant pools (INV-18, INV-21). Isolated PostgreSQL 17 fixtures cover
backend/configuration rejection, role authority, schema drift, cancellation,
connection release, separate migrations and refusal to start unported workers;
they use only private fixture endpoints and generated credentials (INV-22).

## Deployment capability contract (P27)

A deployment must pass this contract on a disposable PostgreSQL 17 environment
with the same role permissions, connection path and pooling mode intended for
the Hub. A local binary pass establishes upstream PostgreSQL compatibility only;
it does not establish a managed provider's compatibility. A provider that cannot
meet any requirement is a cutover blocker. Record the failing test, SQLSTATE and
capability difference without credentials; resolve it before enabling serving.
Never substitute an owner, administrator, platform or RLS-bypassing runtime login.

| Required capability | Executable proof |
| --- | --- |
| Administrator provisions three separate unprivileged groups; runtime cannot assume migration/platform authority or change schema. | `TestDatabaseRolesHaveSeparateAuthority`, `TestRuntimeRejectsCombinedAuthority`, `TestPlatformInventoryAndRoleSeparation` |
| A separately authenticated migration login applies the complete Goose chain to an empty database, can reapply it, and owns the schema and ledger. Runtime cannot migrate. | `TestPostgresMigrationAuthorityIsSeparate`, `TestCoreMigrationsFromEmpty` |
| Tenant tables enable and force RLS, including for their owner. Missing/unknown scope denies reads and writes; overlapping keys in two tenants stay isolated. | `TestCoreRowSecurity`, `TestCoreRowSecurityRejectsCrossTenantWrites`, `TestDatabaseRolesHaveSeparateAuthority`, `TestTenantTransactionIsolation` |
| Parameterized `set_config(..., true)` stays local to one transaction. Commit, rollback, statement errors and failed binding release it before reuse. | `TestTenantTransactionsDoNotLeakContext`, `TestTenantBindingFailureReleasesConnection` |
| The connection path preserves explicit read-committed/read-only transactions, cancellation, bounded serving/background budgets and pool reuse. It retains no tenant state between transactions. | `TestRepositoryReadCommittedAndReadOnly`, `TestRepositoryCancellation`, `TestRepositoryPoolBudgetsAndReuse` |
| PostgreSQL typed keys, identity sequences, partial indexes, deferred tenant foreign keys, PL/pgSQL triggers/security-definer functions, `ON CONFLICT`, `RETURNING`, row locks and `SKIP LOCKED` work through the actual connection path. No additional extension or JSON column is required. | Schema migrations plus core, event, lease, metrics, conversion and platform repository tests |
| Unsupported role grants, session defaults and schema drift refuse startup, release connections and expose no service capable of claiming work. | `TestDeploymentRejectsUnsupportedRuntimeCapabilities`, `TestPostgresStartupRejectsUnsafeRoles`, `TestPostgresStartupRejectsSessionDefaults`, `TestPostgresStartupRejectsSchemaDrift`, `TestPostgresStartupOwnsPoolWithoutSQLiteOrWorkers` |

`NewRuntime` checks both supplied pools before returning a transaction boundary.
It requires PostgreSQL major version 17, `row_security=on`, an empty session
tenant binding and an effective application search path of `public` or
`public, pg_catalog`. It verifies required tenant SELECT/INSERT/UPDATE/DELETE
grants, preserving the read-only conversion-progress and non-deletable cursor
exceptions, USAGE without UPDATE on identity/fencing sequences and EXECUTE on
the scoped allocator and history conversion functions. It rejects
TRUNCATE, public/catalog schema creation, database schema creation and platform schema
access, including permissions inherited from PUBLIC. The existing elevated-role,
owner-membership and forced-RLS checks remain authoritative. P03 additionally
checks the exact migration ledger and closes the pool on failure. Credentials,
grants and server settings must remain fixed while that runtime is in use.

Run the disposable binary proof from this worktree, with no ambient `PG*`
settings and a nonzero test-process deadline:

```sh
mkdir -p "$PWD/tmp"
export TMPDIR="$PWD/tmp"
export PATH="/opt/homebrew/opt/postgresql@17/bin:$PATH"
export DETENT_TEST_POSTGRES=binary
go test -race -v -timeout=10m ./internal/testenv/pgtest \
  ./internal/hubserver/postgres ./internal/hubserver/corestore \
  ./internal/hubserver/eventstore ./internal/hubserver/leasestore \
  ./internal/hubserver/metricsstore ./internal/cloudentry/platformstore
go test -race -v -timeout=10m ./internal/hubserver \
  -run '^(TestPostgres|TestDatabaseStartupCancellation)'
```

For a disposable provider equivalent, use the same commands with the explicit
loopback PostgreSQL URL supported by `internal/testenv/pgtest`; never use a
production endpoint, credential or tunnel. The fixture rejects remote endpoints,
ambient credential discovery and other PostgreSQL major versions. It requires
permission to provision test roles and clone/drop only its own databases; a
provider equivalent must supply these disposable administrative capabilities as
well as the three application authorities. Match the intended connection path
for restricted logins. Session pooling is supported by the backend-PID reuse
assertions. A transaction-pooling proxy must preserve the transaction contract;
if it cannot satisfy the same suite, that deployment mode remains blocked.
Skipped tests are not evidence of compatibility.

At INV-21 scale, validation queries only build-sized relation lists and role
catalog metadata at startup. No tenant data, runner history or fleet is scanned;
request transactions, fixed pool budgets and retry ownership are unchanged.
The harness uses generated logins and owned disposable databases only (INV-22).
No safety-critical orchestrator file, serving cutover, migration or SQL input
changes are part of this contract.

## P23: request metrics serving integration

The Hub database owner selects its request metrics collector from the configured
backend. PostgreSQL collectors share the tenant repository and never open a
metrics SQLite file or own the shared pool. SQLite collectors retain their
existing file binding and schema. Full PostgreSQL Hub startup remains subject to
the existing refusal until all serving families are ported.

A background flush submits at most 200 minute observations. Remaining records
stay pending for the next tick; failed batches retain their exact histograms.
HTTP usage accounting keeps an independent pending payload, so retrying that
sink cannot replay a committed latency batch or discard an unflushed batch.
Shutdown drains pending batches under the caller's cancellation before closing
the metrics file or shared Hub lifecycle. Collection and runner request admission
remain in memory; a storage failure does not change dispatch eligibility.

Repository totals return at most 20 callers and 20 routes from the previous
complete hour. Route history returns the busiest 20 routes in at most 240 time
buckets and ten status classes, within the retained hourly window. Both use the
serving budget and explicit tenant predicates plus forced RLS. The typed
`request_histogram_sum(bigint[])` aggregate preserves every imported minute and
hour bucket when computing percentiles. Imported daily totals remain available
through the existing grain reader; this integration does not reingest historical
rows through the live collector or reconstruct histograms from percentiles.

Retention uses the repository's existing background budget: at most 200 expired
minutes compact individually, at most 200 hours and days expire, and at most 200
empty caller windows expire per tick. Repeated ticks resume the backlog.
`runner_host_hours` remains in its mapped analytics family and heartbeat
transaction, including segment deduplication and acknowledgment. It is not part
of the request metrics repository or a separately opened metrics file.

For INV-18 and INV-21 at 10,000 tenants and runners, request collection performs
one in-memory aggregation with no database I/O. Each flush transaction contains
at most 200 observations and fixed 64-bucket arithmetic. Each retention tick has
the fixed bounds above and touches no other tenant. Serving queries aggregate
only the selected tenant's indexed time window and return the fixed result
bounds above; their scan cost depends on that tenant's retained observations in
the requested window. They perform no tenant/fleet enumeration, derived writes,
external I/O or locks held across network calls.

## Core project serving

`Config.TenantRepository` selects the shared tenant unit of work for native
project creation, lookup, rename, integration settings, content settings, model
selection, slug settings and retirement. Without it, the existing SQLite owners
serve those operations. Backend startup and access-store selection remain with
their respective owners; this interface alone does not enable an entire tenant.

`projectstore` describes the project record and its typed settings. Project,
settings, ordered workflow states and transitions are read in one SQL statement,
so a response cannot pair an old name with a newer settings revision. Integration,
content and model revisions remain independent. Updates compare the appropriate
revision after locking the live project. Workflow transitions use tenant/project
foreign keys, and an issue's optional workflow-state relationship prevents
removal of occupied states. Repository-owned workflows remain externally owned.

Slug reservations are permanent, including aliases and reservations belonging to
retired projects. Generated names use the ordinary slug when available, then a
deterministic project-identity suffix on collision. Allocation uses a bounded
number of indexed lookups; it never scans historical suffixes. Explicit slug
changes compare the previous slug and retain its reservation.

A project command prepares and verifies its object-backed result before opening
its writer scope. Its project change and immutable native-command receipt commit
in that same scope. Replays return the stored result, and a reused key with a
different request conflicts. Creation derives a stable project identity from the
tenant, actor and command key, preventing concurrent replays from creating two
projects. Workflow Markdown and model overrides use typed object references,
with object preparation and hydration outside database transactions.

The access owner retains its existing authority transaction through the bounded
tenant write, then releases it before any receipt hydration. Project retirement
is a soft deletion: it disables repository integration and intake while retaining
work items, history and slug reservations. Active leases and attempts prevent
retirement. Claim acquisition and work-item insertion share-lock the live project
row against retirement, and retirement never releases a lease or changes another
tenant's claims. `Config.ProjectParticipants` supplies the database-only participants owned by
access, billing, workspace, conversation, fleet and source storage. Hosted
creation requires its grant/audit/plan participants; retirement requires its
activity checks, secret audit and grant cleanup. Repository-bound integration
changes require their source participant. Each callback receives the same scoped
writer, authenticated actor and project record. Missing participants fail closed
before a hosted project can be created or retired; those paths never fall back
to SQLite. Their implementations remain with the corresponding family ports.

At 10,000 tenants and runners, each request uses the shared fixed connection
budget, indexed tenant/project lookups and rows bounded by the submitted workflow.
No fleet walk, per-tenant pool, derived refresh, inline payload or SQL JSON
operation is added (INV-16, INV-18, INV-20, INV-21).

## P42: retained request metrics import

`Repository.NewMetricsImporter` binds a frozen raw metrics snapshot to its exact
organization, staging schema, snapshot ID, P28 catalog and source schema
definitions. The source pool must have a fixed positive connection limit and
read access to that schema; the target uses the repository's background budget
and tenant-scoped transactions. Writers and metrics maintenance must remain
fenced throughout the import. Snapshot refresh, writer fencing and cutover belong
to their separate migration owners; this importer never starts or stops them.

Stage `tenant`, `request_days`, `request_minutes` and `request_hours` with exactly
their catalog columns. Scalar staging values use text; histogram values may use
text or bytea containing the original UTF-8 array. Cursor columns use text with
`C` collation and an ascending B-tree primary key matching the source key order:
`day,route,caller_kind,status_class`,
`minute,route,caller_kind,caller_id,status_class`, or `hour,route,status_class`.
The constructor rejects missing cursor indexes, extra or missing columns and a
tenant row that does not match the explicit organization binding. Source engine
bookkeeping stays source-only under the P28 catalog.

Each `Step` reads at most 200 raw rows using that keyset index, outside a target
transaction. It decodes values through the catalog, then commits the page and
its typed `request_metric_imports` checkpoint together. Checkpoints use forced
RLS and retain a catalog/schema digest, snapshot identity, fixed retention time,
stage, source key and processed count. Concurrent stale pages return the existing
revision-conflict classification. Restart or an uncertain commit resumes from
the durable checkpoint; completed imports are no-ops. Changed snapshot identity,
retention time or mapping rejects replay. Initial import requires empty target
metrics for that tenant, and conflicting target keys reject the page.

Daily history imports first. Minute rows at or after the 14-day boundary retain
their exact caller, status, count, sum, maximum and all 64 histogram buckets.
Older minute rows compact once into retained daily totals, including days that
already have compacted source rows. Existing daily totals retain 400 days;
hourly histograms retain 90 days and import independently of minutes, without
reconstructing or adding hourly observations again. Boundaries are inclusive
and aligned to UTC grains. Stored percentiles rebuild from count, maximum and
histogram; daily rows have no source histogram to invent. Empty anonymous and
overflow caller IDs remain empty. Every historical caller is preserved, even
when its minute exceeds today's caller budget; restored admission counters
saturate at 200 so later unknown callers use the existing overflow identity.

At 10,000 tenants and runners (INV-18, INV-21), each step processes one tenant,
one grain and at most 200 rows with fixed 64-bucket arithmetic. Source and target
key lookups cost a B-tree seek plus the bounded page, independently of fleet
size. Expired source history is traversed in bounded pages, never scanned in one
transaction. Checkpoint locking covers only that tenant's target page commit;
no target connection or lock spans raw-source reads or object I/O. Import writes
only metrics and their checkpoint and cannot affect claim eligibility.
## Tenant access and secrets (P14)

`accessstore` is the tenant access contract embedded in the shared tenant reader
and writer. Hosted tenant bindings, organizations, member principals, sessions,
login transactions, invitations and relational grants, key references,
reservations, binding migration receipts, audit records and encrypted provider
credentials use organization-leading keys and forced RLS. The runtime can access
only its bound tenant. The platform role has no grants on these tables; global
entry sessions, signing keys and credential uniqueness remain platform authority.

`Config.AccessRepository` selects these access operations. Supply the same shared
repository as `Config.TenantRepository`. Authentication, session storage and
revocation, member storage, invitation acceptance, credential commands, access
reads, secret mutations and startup key verification use tenant callbacks.
Project record mutations recheck access inside their shared writer, including
receipt replay. Legacy callbacks accepting SQLite transactions reject access
mutations when PostgreSQL access is selected. Other family command owners must
use the scoped access methods in their shared writer; separate access callbacks
are not atomic participants in billing, fleet, claim or completion commands.
Full PostgreSQL startup retains its existing refusal until serving integration
has selected every enabled family.

Authorization checks the current credential hash and validity, member and
principal revocation, membership identity, local role, session binding and project
grants. Mutation reads share-lock these rows until the protected write commits,
so revocation and grant changes serialize with that write. A locally recorded
downgrade takes effect even when an entry assertion carries an older role.
Reporter permissions use the existing action policy. Session revocation creates
a tombstone even before the binding exists, and replay cannot overwrite it.
Login state and invitation acceptance consume conditionally once. Invitation
grants and member replacement commit in the same tenant transaction.

Secret envelopes retain their ciphertext, nonce, wrapped data key and master key
version without decoding or rewriting imported bytes. Their existing JSON-array
AAD framing binds organization, project and kind; this encoding is authenticated
data, never a stored JSON column. Startup verifies every retained envelope using
the supplied keyring and fails on missing keys or authentication failure.
Provider validation, sealing, envelope verification and rewrapping occur outside
the writer. A secret change and its audit row commit together. Rewrap compares
the old envelope and update stamp before replacing only its wrapped key and
version, so a concurrent replacement wins. Audit mutation payloads are verified
object references; entry key metadata and all filterable fields are typed.

At 10,000 tenants and runners, authorization uses a fixed number of indexed
lookups in one tenant, with no fleet enumeration or per-tenant pool. Keyset pages
contain at most 200 records. Administrative lists read only the selected tenant's
requested result, releasing the connection between pages. Startup key validation
and rewrap likewise read at most 200 envelopes per transaction and release the
connection before cryptographic work. A mutation locks only its submitted access,
secret and command participants; it performs no history projection, derived
refresh or network/object I/O while holding those locks. These boundaries preserve
INV-16, INV-18, INV-20 and INV-21.

## Work item and comment serving (P05)

`Config.TenantRepository` routes individual native issue reads, issue creation and
editing, archive and restore, and comment creation, editing, tombstoning and
sequence pagination through core storage. SQLite remains the owner when no tenant
repository is selected. Core record cursors retain their existing timestamp and
identity boundaries. Ordered issue-page projections, relationships, readable URLs,
search, project-wide change watermarks and public event-history reads belong to their respective serving owners;
this selection does not authorize a SQLite fallback for those PostgreSQL records.
Individual issue reads mark the unavailable project-wide watermark as omitted.

Body object payloads encode text as a JSON string, preserving empty issue bodies
and comment tombstones without zero-byte archive objects. Issue contract and
report-context keys reference their own JSON object payloads.

A command prepares and verifies its full immutable version, event and command
result in object storage before opening a writer. Core row changes, immutable
versions, aggregate sequence advancement and command receipts share one tenant
transaction. A failed participant rolls back all database writes, including number
allocation. Object preparation can leave unreferenced objects after rejection.
A replay returns its original receipt; a different request using the same key
conflicts. Provenance keys deduplicate within the tenant, project and issue scope.

Number allocation locks the live project and reads the highest indexed number.
The prepared candidate is compared under that lock; a competing allocation causes
preparation to restart outside the transaction. Comment creation similarly checks
the issue's current aggregate sequence. Explicit issue and comment revision fences
remain conflicts rather than silently rebasing an edit. Comment deletion retains
the original sequence and immutable versions. Archive checks the issue's active
lease and never releases work. Retired projects and reporter ownership remain
visibility boundaries for reads and writes.

The current access owner checks authority before preparation. With tenant access
storage selected, it rechecks access inside the shared writer, including receipt
replay; legacy authority holds its existing transaction through the tenant commit.
Worker mutations, attachment bindings and repository-policy-backed content fail
closed until their own participant ports are available. No object I/O occurs while
an authority transaction or the tenant writer is held. Content hydration validates
tenant, project, record identity and revision against the typed row outside the
database callback.

At 10,000 tenants and runners, an allocation uses one project row lock and an
indexed number lookup. A mutation writes only its issue or comment, version, event
and receipt; it never walks history or rebuilds a projection. A comment read loads
at most one page plus its boundary record and hydrates those versions after
releasing the reader. Shared locks cover database work only. These contracts
preserve INV-1, INV-16, INV-18 and INV-21 without changing orchestrator controls.
## Scoped work and team-thread search

`../searchstore` owns search records, content preparation, ranked work-item
reads, thread reads and the existing one-item refresher. `ReadSearch` and
`WriteSearch` share the core repository's tenant scope and connection budgets.
Prepare the compressed content object before the owning source transaction;
hydrate selected snippets after the read transaction returns.

Work search indexes one document per work item. Its issue title, body, labels
and visible comments share a 64 KiB UTF-8 byte budget. Title and numeric identity
remain available even when the body fills the budget. The title retains its
weight; comments contribute body text in binary source-ID order. Events and
changes keep their existing searchable content in that same bounded aggregate.
Thread search continues to index titles only. The tokenizer preserves SQLite
Unicode61 classification, case folding, diacritic removal and Porter stemming.
Existing Unicode and snippet fixtures compare the implementations.

`search_documents` retains source identities, revisions, typed scope and object
integrity metadata. Only issue aggregate documents and threads own searchable
vectors, scoped postings and adjacency rows; comments are source inputs rather
than competing search documents. Aggregates have separate compressed object
receipts, so refreshing them never replaces an issue's original source content.
Comment edits, source deletion and comment visibility changes invalidate the
owning issue's aggregate through the existing refresh owner. A placeholder issue
provides the same aggregate identity when only comment sources exist. A later
issue source replaces that placeholder. Publication compares the source receipt,
revision and generation, fencing concurrent changes and delete/reuse races.

Each refresh reads at most 32 source objects. A typed generation and source cursor
retain a verified partial aggregate receipt between ticks; a changed generation
restarts from the current source. Only the completed aggregate publishes search
postings. A pending query that needs more than one source page reports index
refresh pending instead of reading an unbounded history or omitting its tail.

Project-name and project-ID labels followed by `#number` preserve
case-insensitive substring matching through tenant-qualified literal grams.
Project renames enqueue the existing bounded parent refresh. Word lexemes retain
tenant/project prefixes. Lexemes of 2 KiB or more including those prefixes are
omitted from vectors; full bounded word statistics and literal grams publish to
typed postings separately. Vector construction deduplicates lexemes, bounds
positions and accounts for PostgreSQL's 1 MiB vector limit.

Search pagination uses bounded examined-row windows for each term and scope.
Each work-item document has one deterministic owning term/lexeme stream; its
aggregate supplies coverage and ranking. There is no canonical source selection
or emitted-work-item history in the cursor. Result pages replay a bounded batch
until consumed, then advance each stream's document identity, impact, timestamp
and exhaustion state. Cursors from the earlier per-source format are rejected.
Visibility and text filtering advance the examined boundary
even when a page is empty. Thread cursors keep time-and-ID pagination.

Prefix expansion examines at most 128 lexemes per term and scope per window.
The 2,000-candidate budget is divided among those lexemes. Each lexeme selects
its candidates by impact, and the combined candidates receive coverage/BM25
ranking. This is approximate even when a prefix expands to fewer than 128
lexemes: a high-impact match beyond one lexeme's allocation can appear on a
later page than a lower-impact match from another lexeme. Prefixes expanding
beyond 128 lexemes also continue across lexeme windows. Continuation preserves
reachability, but ranking across candidate and lexeme windows is not global.
The beyond-budget test asserts complete, duplicate-free reachability rather
than an exact first result.
Each cursor retains at most 128 lexeme positions per term and scope; exhausted
lexemes advance to the next window without discarding remaining matches.

`search_postings` has tenant-leading indexes over scope, lexeme and either impact
or thread time. Scope keys encode project, work-item, search kind, thread scope
and status, archive selection and reporter identity. Each term window divides
its 2,000-candidate budget among the expanded lexemes. Each lexeme seek includes
one lookahead; work continuation uses separate bounded seeks for the current
impact's remaining IDs and lower impacts. Rare terms keep independent windows
when common terms overflow. A page can be empty and still carry a cursor.

Ordinary nonempty searches check bounded candidate document markers through
`search_documents_vector_idx`. Each GIN query uses at most 2,000 markers from
one requested term and scope; it does not scan the unbounded posting list of a
common text prefix. Scoped postings supply the text candidates, and typed
statistics or bounded source objects verify each match. Markers are lookup
hints; exact document IDs and current tenant/source authorization remain
mandatory. Within the combined bounded candidate set, final work ordering
preserves exact numeric lookup priority, then sorts by matched-term count
(descending), a BM25 approximation over candidate statistics (ascending), issue
number (descending) and binary document ID (ascending). Pagination uses that
same ordering. Scores can differ from SQLite corpus BM25. Coverage and stable ties apply
within each candidate window; pagination does not promise corpus-wide rank.

Provision `roles.sql` before applying the search migrations. Its stable,
read-only `search_index_candidates` helper belongs to `detent_platform_v2`, fixes
its search path to `pg_catalog` and denies public execution. It derives the
tenant exclusively from the transaction-local setting. This avoids the runtime
RLS barrier around PostgreSQL's non-leakproof text-search operator. The helper
checks current project, issue and comment visibility after its bounded indexed
lookups. Runtime fetches the returned IDs through forced RLS and checks those
facts again. Startup verifies the helper's owner, security mode, search path
and execution grant; runtime credentials gain no platform membership or pool.
The separately provisioned functions persist across schema rollback, like roles.

Issue and thread writes publish metadata-only pending/listing postings atomically.
Comment visibility changes update one source row and invalidate its aggregate.
Archive, reporter and project changes enqueue one parent cursor rather than
rewriting history. `search_scope_documents` is a
typed, trigger-maintained adjacency index with only a parent-leading primary key;
it finds the next child without a competing chronological index scanning other
parents. Issue and thread deletion removes their adjacency variants and postings.
The same refresh owner selects either a pending document or one cursor child.
Before object I/O it advances the attempt order. A cursor child transfers into
the source pending index before its cursor advances, so a failed object cannot
hold later children. Errors remain retryable through that owner. Publication
compares the source revision, generation and object receipt in a short
transaction, with object reads and tokenization outside database connections and locks (INV-3,
INV-18). Current visibility remains authoritative while cursors catch up;
bounded cursor reads supply candidates in the new scope before reindexing.

At INV-21 scale, 10,000 tenants/runners share the same fixed pool and budgets.
Candidate streams depend on at most 32 word terms, 32 literal grams and 101
requested scope variants. Each stream contributes at most 2,000 candidates
and intersects at most 2,000 document markers. Each indexed stream seek reads
at most 2,001 postings; work continuation combines at most two such seeks.
Prefix expansion visits at most 129 lexemes per term and scope, including one
lookahead, and ranks postings from at most 128 lexemes in each window.
Stream ownership examines only each candidate's bounded document postings,
with one indexed frontier read per requested term. A work
request has one project scope and at most 64 word/literal terms, for at most 128,000 posting
candidates. A thread request has at most 32 terms and 101 scopes, for at most
6,464,000 posting candidates. Ordinary single-scope, single-lexeme requests
return at most 2,000 matching GIN entries; excluded history does not grow that
candidate work. Prefix scans have no fixed vocabulary cutoff. Cursor fallback
examines at most 1,000 parents and one indexed child per parent. Each helper
returns the union of its bounded stream windows and the refresh candidates,
plus one boundary per nonempty stream when another window remains. A search
combines indexed and pending windows with one numeric match, then returns at
most 200 deduplicated results. Structural work-item identity and deterministic
stream ownership prevent duplicates without a growing set of emitted IDs in
the cursor. Empty thread pages use the same scoped, time-ordered postings
and binary-ID keysets. Every document publication is bounded by its 64 KiB
content, finite scope variants and vector budget. Archive and project changes
retain one parent cursor; source changes invalidate one issue. A refresh
handles one work-item or thread document. Aggregate source reads use
an indexed keyset in batches of at most 32 visible sources, release the read
transaction before object I/O, and stop at the 64 KiB budget. Each consumed source
uses at least one separator byte, bounding even empty-source walks by 65,535
inputs independently of history or fleet size. Source visibility is indexed and
maintained with the owning comment, so deleted history is not scanned.
All derived fields are typed and SQL reads no stored JSON (INV-20).

Backend selection, runtime wiring and whole-database cursor/import cutover
remain with the shared PostgreSQL runtime cutover.
