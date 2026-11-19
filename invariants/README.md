# In-repository invariant checks

[docs/invariants.md](../docs/invariants.md) states the rules, and review applies them. A few rules also have source scanners: ordinary Go tests in `internal/invariants` and `internal/hubserver` that read the repository and fail when a banned pattern appears, usually against a baseline that only shrinks. `make check-invariants` runs the `internal/invariants` package, and the landing gate runs it on every change.

A scanner is a backstop, not the rule. A passing scanner does not authorize weakening a rule, and scanners cannot judge a natural-language product decision.

## Scanner details

These notes describe how the source scanners work. They are implementation detail, not rules.

### INV-18

`TestINV18RequestCommitDoesNoDerivedWork` and
`TestINV18HubMigrationsUseTypedColumns` in `internal/hubserver`. Review applies
the history bound and reader rule to Hub persistence changes.
`TestINV20HubTableLifecycles` checks every application table in the migrated Hub
template against `internal/invariants/table_lifecycle_policy.json`. Each table
declares immutable payload in object storage, derived data with an explicit
retention contract, hot state, or an index. SQLite-owned shadow and system
tables are excluded; new application tables require declarations.

### INV-19

`TestINV19NoFlakeAccommodation` in `internal/invariants`
checks the Playwright CI settings, that no browser spec skips, marks or retries
a test, and that no Go test skips itself as flaky.

### INV-20

The JSON functions and operators the rule covers: `json`, `jsonb`, `json_extract`, `->`, `->>`, `json_each`, `json_tree`, `json_set`, `json_insert`, `json_replace`, `json_patch`, `json_remove`, `json_object`, `json_array`, `json_group_*`, `json_valid`, `json_type` and `json_array_length`.

Current page content may be projected into typed `page_read_model` tables so
page handlers do not fetch objects. These tables retain only the current page,
record the source revision and generation, source time and projection time,
and are rebuilt asynchronously through `hubmaintenance`. Project pages declare
project scope and use the tenant and project keys; work-item pages also carry
the work-item key. Their typed `body`
column is current display text, never a serialized history payload. JSON and
other serialized payload columns remain prohibited; the frozen legacy payload
list does not expand for read models.

Review enforces payload placement in object storage. `TestINV20NoSQLOverJSON` in `internal/hubserver` scans non-test
Go files in Hub and store (including generated SQL), and `db/queries` SQL sources.
Per-file function counts must equal `testdata/inv20_json_sql_baseline.txt`;
reductions must lower the baseline in the same change, and increases are forbidden.
Migrations from the INV-18 adoption cutoff (`20261015000000`) are also checked;
existing calls are baseline debt and new migrations have no allowance. The scan
conservatively counts function calls even on parameters or literals. It does not
recognize `->` or `->>` operators, to avoid false positives outside SQL strings;
review enforces these operators and conversion of edited queries to typed columns.

`TestINV20HubTableLifecycles` also rejects inline history and derived payload
columns unless their exact table and column appear in the frozen
`legacy_payloads` list in `internal/invariants/table_lifecycle_policy.json`.
Each debt entry names its owning conversion family, including families whose
conversion is still pending. This list may only shrink: never add entries,
broaden an entry to a table or pattern, or relabel history as hot state or an
index to bypass it. Removing a payload column requires removing its debt entry
in the same change. `TestTableLifecycleViolations` mutates copies of the migrated PostgreSQL
template to verify unknown tables, inline payloads and stale debt are rejected.
Discovery includes application tables in both the public and platform schemas;
non-public table names are schema-qualified in the lifecycle catalog.

### INV-23

`TestRepositoryTenancy` in `internal/invariants` is a
syntactic ratchet over non-test Go files in the whole repository. Process
package imports and launch symbols, and physical pool constructors and
factories, are pinned to named sites in
`internal/invariants/tenancy_policy.json`; occurrences in the hosted serving
tree, pool constructors outside `internal/pgtransport`, hosted-path timer
constructors and process-wide organization reads are debt that only shrinks.
Test-support packages are exempt only by name, and only the named scratch
paths and directories the Go toolchain ignores are skipped. Counts must equal
the policy. Landing refuses growth: `scripts/check-tenancy-policy.py`, run by
`make check-land` and the landing gate against the target revision, rejects
any raised debt or exception count and any test-support entry not already in
the target set. `-tenancy-baseline` is a local rewrite convenience, not
protection. `TestTenancyPolicyViolations`, `TestTenancyBaselineRefusesGrowth`
and `scripts/check_tenancy_policy_test.py` test the scanner and the landing
comparison. The ratchet does not follow call graphs, types or runtime
multiplicity, and an empty debt list is not proof the migration is complete;
review applies the remaining rules and the
[review checklist](../docs/architecture/multi-tenancy.md#review-checklist).
