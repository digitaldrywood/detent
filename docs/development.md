# Development

[Back to README](../README.md#documentation)

Common development commands:

```sh
make setup
make dev
make check
make security
make modernize-check
```

`make dev` runs Air with `ENV=dev` and
`LOG_LEVEL=debug`, builds a `dev`-versioned `./tmp/detent` with the current
commit SHA and build date, rotates
`tmp/air-combined.log`, and streams combined build and application output to
`tmp/air-combined.log`.

`make check` is an optional diagnostic suite: build, `golangci-lint`, `go vet`,
NilAway, race tests, and the 70 percent coverage check. Run `make generate`
before committing changes to Templ templates, sqlc queries, or Tailwind inputs.
`make security` runs the pinned `govulncheck` and standalone `gosec` scans used
by CI. The gosec baseline skips generated files and documents each legacy rule
excluded in the Makefile; new findings must be fixed or narrowly annotated.
`make modernize-check` runs the Go modernizer diff check with the repo's
selected safe analyzer set.

Follow the repository's [validation policy](../AGENTS.md#validation) and
[failure reporting policy](../AGENTS.md#deployment-and-release-failure-reporting).
Focused diagnostics do not become merge gates or local-gate statuses. The
scheduled suite validates pinned integrated develop commits for release tags;
new proven, pinned Detent source blockers enter native Todo at least High under
the human-approved scheduled reporting policy. Reused items preserve Urgent,
existing lanes, human questions and operator holds; unknown instance diagnostics
remain in nondispatchable Backlog.

Several worktrees usually run gates on the same host at once. Every `make`
test, lint, vet, and build target is capped by `TEST_PROCS` (default 4): it
sets `go test -p`, `GOMAXPROCS` for the test binaries, `golangci-lint
--concurrency`, and vitest workers, so one worktree's gate leaves the machine
usable for the others. Raise it for a solo run with `TEST_PROCS=8 make test`.
Plain `go test ./...` outside `make` has no cap.

Scheduled Windows portability runs the Hub suite through
`make test-hub-portability`, reusing the same three exhaustive, disjoint
partitions as the race targets with four parallel tests and a ten-minute budget
per partition. The remaining package selection excludes Hubserver and workspace,
which have already run separately. Evidence for each Hub partition is included
in the existing Windows test artifact. Test selection and individual deadlines
remain unchanged.

Portability Stress is a manually dispatched macOS/Windows diagnostic workflow.
Its suites run on separate hosted runners through
`bash scripts/portability-stress.sh <suite>`, with `GOMAXPROCS=4`, bounded test
parallelism, and evidence under the supplied `TMPDIR`, `TMP`, or `TEMP`.
The CLI, runner, and checklock suites retain ten race repetitions; the other
suites retain the complete race package selection, including one more run of
those three packages. Windows also retains twenty SQLite lifecycle repetitions.
Hubserver uses the same three disjoint test partitions as the Makefile, with
two parallel tests per partition. Orchestrator and workspace run separately;
the remaining packages run one at a time through the existing evidence tool.

The Windows diagnostic job in run `36678733180` took 4933.681 seconds for ten
CLI repetitions and 1205.368 seconds for checklock. Their package budgets are
120 and 45 minutes, with job budgets of 150 and 60 minutes respectively.
Runner has a 45-minute package budget. Hubserver partitions have 45-minute
budgets after the unpartitioned suite exceeded 30 minutes on both platforms;
orchestrator and remaining packages have 30-minute budgets, and workspace
retains its existing 15-minute budget. Job budgets leave time for setup and
evidence upload. These bound cumulative suite work; individual production
deadlines and test workloads remain unchanged. Hosted resource contention is
still a hypothesis. Uploaded JSON events include run/pause/cont timestamps for
diagnosing queue time separately from execution time.

Packages that own transport, hub, watcher, orchestrator, and runner goroutines
also run `go.uber.org/goleak` from package-level tests, so `go test ./...`,
race tests, and `make check` fail on unexpected goroutines. Add goleak ignores
only in the package that needs them, and only after identifying the dependency
or intentionally shared test goroutine.

Nil safety is enforced by `make check` and can also be run directly while
iterating:

```sh
make nilaway-audit
```

The scheduled audit uses `make nilaway-changed`. Despite its historical name,
it analyzes all Go packages so changes to a provider's inferred nilability also
check unchanged importers. It accepts only reviewed legacy diagnostics whose
location and source-line hash match `scripts/nilaway-baseline.json`.

The project uses the standalone NilAway command instead of golangci-lint
integration because the linter integration requires a custom module-plugin
binary. Go 1.26's experimental `runtime/pprof` `goroutineleak` profile remains a
runtime audit aid behind `GOEXPERIMENT=goroutineleakprofile`; the stable CI
coverage for now is the goleak-backed test gate.

See [CONTRIBUTING.md](../CONTRIBUTING.md) for the full contributor workflow.


## Concurrent migrations and generated files

New Goose SQL migrations in `internal/store/migrations` and
`internal/hubserver/migrations` use UTC timestamp versions:
`YYYYMMDDHHMMSS_name.sql`. Use a descriptive name and Goose's default timestamp
mode, never `-s` (sequential mode):

```sh
make db-create NAME=add_feature
make db-create NAME=add_feature MIGRATIONS_DIR=internal/hubserver/migrations
```

The target runs `TZ=UTC goose -dir "$MIGRATIONS_DIR" create "$NAME" sql`.
The equivalent direct command is
`TZ=UTC goose -dir internal/store/migrations create add_feature sql`.
Existing numbered files remain unchanged: store through 68 and Hub through 71.
The older descriptive-name boundaries (store 68, Hub 70, registry 5, auth 4)
remain fixed in the checker; registry and auth retain their sequential convention.
`make check-migrations` diagnoses duplicate versions and invalid timestamp names
in the existing scheduled suite or during optional focused diagnostics.

Adding a SQL migration changes only its new file. The embedded directory supplies
the migration set and supported Hub version; the historical Go data conversions
remain registered at their original versions. Timestamp versions sort after
every historical sequential version. Fresh databases apply them in version
order. Existing databases apply pending files through Goose, including older
timestamps that land after a newer timestamp, without renumbering at landing.
Hub foreign-key verification stays in the last pending migration's transaction,
including when that migration has an older timestamp than the database maximum.

Independent migrations with distinct timestamps merge in either order. Timestamps
have one-second precision, so simultaneous creation can still produce a duplicate
version; the checker reports it even when descriptive filenames differ. Choose a
distinct UTC timestamp before publishing an unapplied migration. Never rename or
renumber a migration that may already have been applied; use a forward repair for
deployed collisions. Migrations that depend on another branch's schema still
require their source dependency to land first and an appropriate version order.

sqlc reads `internal/store/migrations` through `sqlc/sqlc.yaml`, including timestamp
files. Run `make sqlc` after schema or query changes, or `make generate` when
other generated inputs also changed. `make db-migrate` applies store migrations
with Goose's `-allow-missing` option; Hub migrations run automatically at startup.

Tracked sqlc Go, generated Templ Go, and Tailwind output have the Git merge
attribute unset. Concurrent changes to the same generated file therefore require
resolution even when their text edits do not overlap. Merge the source inputs,
then regenerate and stage the output with `make generate`; choosing one branch's
generated file alone does not resolve the combined source. Ordinary source
files retain Git's text merge behavior. Conversation bundles remain ignored
and are built from each consumer's complete pinned checkout with `make app`.

This source convention does not add a required check, strict branch freshness,
merge worker, or recovery path. It exposes these collisions to existing Git
conflict handling; unrelated semantic conflicts still rely on review and the
scheduled suite.

## Conversation assets

Conversation source and its npm lockfile are tracked; `static/app/conversation/`
is ignored build output. Do not stage that directory in feature commits.
`make app` installs the locked dependencies and builds the entire client.
`make generate` runs it before Go generators, and `make build` embeds that
output. Use Node 24 for the same toolchain as staging and release builds.

For a private operator build, first select the complete source composition in
this isolated checkout, then run `make build`. When supplying custom Go flags
or cross-compiling, run `make app` followed by the desired `go build` command.
Always regenerate after changing or integrating client source or the lockfile;
a direct Go build cannot determine whether existing ignored output is stale.
An unprepared checkout fails compilation rather than producing an absent UI.
Prepared release source archives already contain the embed inputs and
`BUILD_LDFLAGS`, so end users compile those with Go alone.

Staging and scheduled fresh-checkout consumers call the repository's
`prepare-conversation` action, which uses this same `make app` entry point.
GoReleaser's before hook calls it for release and snapshot builds and includes
all output in its prepared source archive. Every consumer builds from its
pinned checkout; assets from a different feature branch are never reused.

`make check-app` retains typechecking, client unit tests, rebuilding and
attribution diagnostics. It does not compare against committed bundles and is
not a shipping gate. The scheduled suite validates integrated source as before.

To reproduce the contention regression and exercise embedded delivery, run:

```sh
python3 scripts/verify-conversation-build.py
```

The diagnostic creates a repository under the provided worker scratch directory,
merges two independent client edits with source-only commits, builds a local
GoReleaser snapshot, and compiles its prepared source with Go alone. Both builds
exercise the existing embedded HTTP handler for every conversation asset from
an arbitrary working directory. It neither publishes a release nor touches
tracker state, shared refs or the running service.
