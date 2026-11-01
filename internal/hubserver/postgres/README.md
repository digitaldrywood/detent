# Hub PostgreSQL schema

This directory owns the PostgreSQL schema and transaction boundary for the
core records, request metrics and platform registry/authentication families.
Provision `roles.sql` as a database administrator, then apply `migrations/`
with the Goose PostgreSQL dialect as
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
with `detent_migration` ownership and only `detent_platform` usage. Its migration
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
