# Hub PostgreSQL schema

This directory owns the PostgreSQL baseline for the projects, work items and
comments family. Apply `migrations/` with the Goose PostgreSQL dialect on an
empty PostgreSQL 17 database. The existing SQLite migration chain and runtime
remain separate; this baseline does not switch the Hub database backend or
import tenant data.

## Core family

The source inventory is `../testdata/hub_schema.json`; ownership and inline
payload classification come from `../testdata/hub_classification.json`.
`projects`, `issues` and `native_comments` are organization-owned. The work item
table keeps the existing name `issues`, its numeric `id`, and its text
`native_id`. Project and comment IDs remain text. Explicit IDs can be imported
unchanged; generated project and work item text IDs retain `prj_` and `wi_`
prefixes. Numeric issue IDs accept imported values; an importer must advance
the identity sequence past imported IDs before enabling generated writes.

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

These constraints enforce relationship integrity. Application reads and writes
must still authorize the organization and bind its ID in every tenant query;
this baseline does not introduce row-level security or database roles.

INV-18 and INV-20 require typed columns and object references. INV-21 requires
tenant-bounded access: core identity lookups and parent checks use tenant-leading
B-tree keys, and comment pages use the tenant/project/work-item/sequence index.
At 10,000 tenants these operations use indexed lookups rather than tenant or
history walks. Later readers must bound page sizes; this schema introduces no
request handlers, periodic jobs, fleet scans or shared application locks.
