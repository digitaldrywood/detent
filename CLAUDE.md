# CLAUDE.md

## Project Conventions

- Use English for code, comments, documentation, errors, tests, commits, and examples.
- Target Go 1.26 and idiomatic standard-library-first Go.
- Keep application code feature-packaged under `internal/` as the system grows.
- Prefer constructor dependency injection over global state or wire/fx.
- Use interfaces and factories only at backend/plugin boundaries where they remove real coupling.
- Use `log/slog` for logging.
- Use Echo for HTTP, sqlc with goose migrations for persistence, and `modernc.org/sqlite` for SQLite.
- Use Templ, HTMX, and Tailwind v4 for server-rendered UI.
- Use Air for local hot reload and golangci-lint v2 for linting.
- The live dashboard region (`#snapshot`) is updated by **morphing in place** (idiomorph, `hx-swap="morph:innerHTML"`), never a destructive `innerHTML` swap — otherwise hover popovers/tooltips inside it are torn down and rebuilt on every SSE tick and flicker. Any new element added inside the live region must tolerate in-place morph; render hover tooltips/popovers through a single body-level host (see `helpTooltipHost`) outside the swapped region, and re-assert open state on `htmx:afterSettle`. Do not reintroduce an `innerHTML` swap on `#snapshot`.

## Workflow

- Work from a Detent-created worktree branch, never directly on `main`.
- Keep generated files and runtime output inside the current worktree.
- Do not bind development or tests to `127.0.0.1:4000`; use ephemeral ports in tests.
- Before implementation, confirm dependencies listed in the issue are merged into `origin/main`.
- Keep changes scoped to the active issue.
- Run `make check` before pushing or opening a PR.
- Run `make generate` before committing when templates, sqlc queries, or CSS inputs change.
- Commit only when explicitly requested by the workflow or human, and use conventional commit messages.

## Validation

- `make check` is the local pre-review gate.
- `make check` runs build, golangci-lint, go vet, race tests, and a 70% coverage gate.
- New or modified Go behavior requires focused table-driven tests using only the standard library.
- Generated Go files such as `*_templ.go` and sqlc output do not need hand-written tests.

## Tooling

- `make dev` runs Air and rotates `tmp/air-combined.log`.
- `make generate` runs `go generate`, Templ, sqlc, and Tailwind when their inputs exist.
- `make setup` installs Air, Templ, sqlc, goose, and golangci-lint v2.
- `make sqlc` uses `sqlc/sqlc.yaml` by default.
- `make db-migrate` uses goose against `internal/store/migrations` by default.
