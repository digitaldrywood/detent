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

The project uses the standalone NilAway command instead of golangci-lint
integration because the linter integration requires a custom module-plugin
binary. Go 1.26's experimental `runtime/pprof` `goroutineleak` profile remains a
runtime audit aid behind `GOEXPERIMENT=goroutineleakprofile`; the stable CI
coverage for now is the goleak-backed test gate.

See [CONTRIBUTING.md](../CONTRIBUTING.md) for the full contributor workflow.
