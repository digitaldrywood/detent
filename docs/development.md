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

`make check` runs the local release gate: build, `golangci-lint`, `go vet`,
NilAway, race tests, and the 70 percent coverage check. Run `make generate`
before committing changes to Templ templates, sqlc queries, or Tailwind inputs.
`make security` runs the pinned `govulncheck` and standalone `gosec` scans used
by CI. The gosec baseline skips generated files and documents each legacy rule
excluded in the Makefile; new findings must be fixed or narrowly annotated.
`make modernize-check` runs the Go modernizer diff check with the repo's
selected safe analyzer set.

Several worktrees usually run gates on the same host at once. Every `make`
test, lint, vet, and build target is capped by `TEST_PROCS` (default 4): it
sets `go test -p`, `GOMAXPROCS` for the test binaries, `golangci-lint
--concurrency`, and vitest workers, so one worktree's gate leaves the machine
usable for the others. Raise it for a solo run with `TEST_PROCS=8 make test`.
Plain `go test ./...` outside `make` has no cap.

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
