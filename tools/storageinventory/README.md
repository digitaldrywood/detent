# Storage routing inventory

`routing.json` is the source-to-family routing contract for the PostgreSQL Hub
port. It assigns every classified Hub, request-metrics, entry-registry and
entry-auth table to a target family, source-only disposition or explicit rebuild.
It also assigns production SQL owners, including historical migrations, generated
queries, their adapter callers and transaction bindings, prepared statement
constants and database lifecycle code. A target route
means work belongs to that family; it does not mean its serving adapter is wired.
Tenant access and billing remain distinct from global platform authority.

Run `go run ./tools/storageinventory` from the repository root. The same check runs
in `go test -timeout=120s ./tools/storageinventory`. `-scan` prints discovered
inventory for comparison; it never assigns routes or updates the contract.

The checker reads the four existing `*_classification.json` inventories without
changing their schema or storage-class checks. Those checks continue to reject
schema changes without classification. Routing rejects newly classified tables,
new SQL owner files, changed per-symbol execution/preparation inventories, new
named SQL queries and stale entries. Route families, transaction participants and
existing parity-test references must resolve. Updating an inventory requires
reviewing its family and transaction ownership, rather than copying a scan over
the routing map.

The scan recursively covers production Go and SQL beneath `internal`, `cmd` and
`db`, including generated files and all OS-specific source. Test execution and
`testdata`/`internal/testenv` helpers do not own production SQL. Test declarations
are read only to validate parity references. The local store, local CLI/board
imports and artifact-service catalog have explicit dispositions outside tenant
Hub persistence; they are not silently omitted. Conservative matches to URL
`Query`, process `Exec`, preparation APIs or instructional strings are explicitly
classified as `non-storage`.

This is a source inventory, not a Go call graph or SQL parser. Method selectors
include method values, SQL helpers, cached statements, generated query receivers
and native SQLite prepare calls. Generated-package imports and bindings, SQL
literals and named query sources cover indirect prepared/generated owners.
Table references are conservative identifier matches within each source
store, not proof that every reference executes together. Dynamic helper calls
still belong to their mapped file; review follows the helper into the actual
transaction. Adding another SQL execution API requires extending the scanner and
its negative fixtures. Source analysis neither inspects live data nor proves
managed PostgreSQL compatibility.

## Transactions and parity

Every detected `Begin`/`BeginTx` owner is enumerated by symbol in its file's
operation inventory. `atomic_groups` records the shared application contracts,
their transaction boundary, participating functions, tables and existing tests to
extend for PostgreSQL parity. Members of a group can represent separate
invocations of the same contract; the group's text identifies asynchronous work
and separate stores. The map does not merge these into one large transaction.

Claim ownership starts at `database.claimNextOn` in
`internal/hubserver/api_worker.go` and calls `database.claimInTransaction` in
`tracker_mutations.go`. Its current-authority checks, lease/fence allocation,
reservations, attempt state and history must share the tenant unit-of-work.
Candidate preflight is a read snapshot and grants no lease.

Completion enters through `Service.appendNativeRunEvent` in `native_events.go`
and uses `Service.commitNativeMutation` in `native_api.go`.
`Service.recordNativeRunEvent`, `recordNativeAttempt`, usage accounting,
dependency/progress updates, publication metadata, history and command receipt
must remain failure-atomic. The claim and completion groups name existing fencing,
replay, usage and object-preparation regressions for downstream parity tests.

The existing PostgreSQL core, lease and event callbacks each call
`Repository.transaction` independently. The `postgres-callback` group records
their generated query binding and rollback/claim/receipt tests. P02 must expose
the required family methods on one scoped session; composing the current
callbacks would commit separately.

The platform registry and auth stores currently have separate transactions.
Provisioning intent and auth revocation reconcile through durable state;
switching all platform owners together does not make existing cross-store work
one transaction. Retain this distinction in the platform port.

## Family slices

The broad plan families contain distinct contracts that should remain separate
implementation slices. Each slice retains the enclosing plan dependencies and
its mapped atomic group; these subdivisions do not authorize independent commits
inside a shared transaction.

| Plan family | Focused slices | Source owners |
| --- | --- | --- |
| P09 | Source custody and landing; test/release attestations; outbox delivery | `change_sources.go`, `change_landing_receipts.go`; `go_test_receipts.go`, `release_audit.go`, `release_delivery.go`; `outbox.go` |
| P17 | Thread/unread state; attachment metadata; artifact references and external catalog | `team_threads.go`, `team_thread_unread.go`; `cloud_attachments.go`, `attachment_archives.go`; `artifacts.go`, `internal/artifact` |
| P21 | Activity projections; health identity/facts; bounded projection cursors | `activity_projection.go`; `health_*.go`; `derived_refresh_revisions`, `health_detector_ticks` |
| P22 | Durable usage/accounting; prepared analytics; retained samples and rollup maintenance | `native_usage.go`, `usage_costs.go`, `provider_usage.go`; `analytics_rollups.go`; `analytics_drain.go`, `analytics_sample_retention.go` |
| P25 | Backend lifecycle/backup; object conversion; archive/reclaim; source-only migration helpers | `database.go`, `backup.go`; `storage_conversion*.go`, `payload_conversion.go`; `native_storage_archive.go`, `storage_reclaim.go`; `migrate.go`, `migration_steps`, `migrations` |

Rebuild dispositions retain original searchable content or typed base rows.
SQLite FTS shadow tables and engine catalogs are source-only. Durable history,
usage, retained analytics rollups and maintenance cursors have target dispositions;
being derived is not sufficient grounds for discarding retained data. P28 owns
column-level transforms and P43 reconciles final import coverage.

This tool enforces source routing only. It adds no serving SQL, request writes,
background work, locks or runtime configuration. Its filesystem work scales with
repository size, independently of the 10,000-tenant/runner fleet (INV-21). The
contracts preserve shared tracker knowledge (INV-16), bounded writes (INV-18) and
typed SQL/object references (INV-20). Runtime behavior and safety-critical
orchestrator files remain unchanged.

## Raw source column mappings

`mappings/v1.json` extends routing with a versioned, explicit entry for every
source column. The normal command validates both contracts; `-catalog` selects
another reviewed version. `-scan` remains a read-only owner/table inventory and
does not assign column transforms. Unknown or changed source definitions reject
validation. Unsupported target fields are explicit pending mappings that reject
row import, not evidence of complete target coverage.

See [the catalog contract](../../internal/storagecatalog/README.md) for source
organization bindings, encodings, precision, object preparation, rebuild/retired
dispositions and the distinction between catalog coverage and import readiness.
