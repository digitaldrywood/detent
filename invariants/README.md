# In-repository invariant checks

This is repository-local behavioral enforcement. Detent uses the operator's
own GitHub login. No separate account, token, App, server-side trust boundary,
or approval workflow is required.
This tool does not protect itself against edits or authenticate policy exceptions.

Run `go run ./tools/invariantcheck` (also `make check-invariants`). The optional
`make check` targets and scheduled CI include this check; ordinary submission
and merge follow [WORKFLOW.md validation](../WORKFLOW.md#validation). Doctor runs
the same command for configured source repositories containing
`invariants/policy.json`; an absent manifest means the
repository has not opted in, and produces no invariant success claim.

The only option is `-policy <path>`, defaulting to `invariants/policy.json`.
The JSON object contains `tests`, mapping relative Go package paths to nonempty
lists of exact top-level test names. Unknown fields and empty manifests fail.
The checker runs fixed `go test -json -count=1 -run <anchored names> <package>`
arguments directly, without a shell. Exit codes: 0 means every named test passed;
1 means invalid manifest, execution failure, or absent/failed/skipped evidence;
2 means invalid CLI arguments. A skipped subtest also fails the gate. Output is
human-readable diagnostics; consumers should use the exit code.

The [repository invariants](../docs/invariants.md) register package-boundary,
reason-vocabulary, retired-mechanism, and workflow tests from `internal/invariants`
through this same interface. The manifest names the behavioral tests to execute;
it does not require or inspect a per-change `docs/invariants.md` edit. Only a
change to an invariant's rule edits that document.
Per-change rationale, evidence and verification belong in the Change
description or issue comments through the authorized tracker owner (INV-16).
These tests exercise specific behavior; they cannot prove every
natural-language product decision or prevent an operator credential
from changing repository controls. Release and deployment paths retain their
existing enforcement.

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
