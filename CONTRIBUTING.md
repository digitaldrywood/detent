# Contributing

Detent is a Go agent orchestrator delivered as a single binary. Keep changes small, scoped to the issue or pull request, and aligned with the existing project conventions.

## Prerequisites

- Go 1.26.
- Git.
- Node.js and npm for Tailwind CSS.
- GitHub CLI for issue and pull request workflow.

Install the project tools with:

```sh
make setup
```

`make setup` installs Air, Templ, sqlc, goose, golangci-lint v2, and npm dependencies when `package.json` is present.

## Clone And Start

```sh
git clone https://github.com/digitaldrywood/detent.git
cd detent
make setup
make dev
```

`make dev` runs Air with `ENV=dev` and `LOG_LEVEL=debug`, builds `./tmp/detent`, rotates `tmp/air-combined.log`, and streams combined build and application output to `tmp/air-combined.log`.

The default web bind is `127.0.0.1:4000` when no config or port is supplied. If another Detent process is already using that port, do not start a second server on it. Run a built binary with `./tmp/detent --port 0` when you need an ephemeral port.

## Logging

Detent logs with `log/slog`.

- `ENV=dev`, `development`, or `local` enables tint text logs.
- `ENV=prod` or any other non-development value keeps JSON logs.
- When no environment is configured, Detent defaults to `prod`.
- `LOG_LEVEL` accepts `debug`, `info`, `warn`, `warning`, and `error`.
- `--env` and `--log-level` override environment variables for one run.
- `DETENT_ENV` and `DETENT_LOG_LEVEL` remain deprecated fallbacks for one release. The unprefixed names win when both are set.
- Text logs are written to stdout; JSON logs are written to stderr.

## Global Config Discovery

Detent must resolve `global.yaml` consistently across supported operating systems. Keep this precedence intact when changing startup or config commands:

1. `--config <path>` uses the direct CLI file path.
2. `CONFIG=<file>` uses the direct environment file path.
3. `CONFIG_HOME=<dir>` uses `<dir>/global.yaml`.
4. `os.UserConfigDir()` uses `<config-dir>/detent/global.yaml`.
5. The legacy default uses `~/.detent/global.yaml`.

The OS-native config directory is `%AppData%\detent\global.yaml` on Windows, `~/Library/Application Support/detent/global.yaml` on macOS, and `~/.config/detent/global.yaml` on Linux, with `XDG_CONFIG_HOME` honored by `os.UserConfigDir()`.

`DETENT_CONFIG` and `DETENT_HOME` remain deprecated fallbacks for one release. Detent uses `CONFIG_HOME` instead of `HOME` because `HOME` is standard process state, not Detent configuration.

After global config lookup fails, startup may fall back to a valid `WORKFLOW.md` in the current working directory for single-project mode. `detent config path` should continue to report both the selected path and the matching rule.

## Validation

Run the full local gate before every commit and pull request:

```sh
make check
```

`make check` runs:

- `make build`, which runs `make generate` before building `./tmp/detent`.
- `golangci-lint run --timeout=5m` with golangci-lint v2.
- `go vet ./...`.
- `make nilaway-audit`.
- `go test -race ./...`.
- Coverage with a 70% minimum, excluding generated Templ output and sqlc output.

Run focused package tests while iterating, then finish with `make check`.

## Generated Assets

Run the generation pipeline after changing templates, SQL queries, migrations, or Tailwind inputs:

```sh
make generate
```

`make generate` runs:

- `go generate ./...`.
- Templ generation when `.templ` files exist.
- `sqlc generate -f sqlc/sqlc.yaml` when the sqlc config exists.
- Tailwind CSS from `static/css/input.css` to `static/css/output.css` when the input exists.

Commit generated output with the source change that produced it.

## Go Conventions

- Use Go 1.26 and standard-library-first code.
- Keep application code feature-packaged under `internal/`.
- Use constructor dependency injection instead of global state or wire/fx.
- Use interfaces and factories at backend or plugin boundaries where they remove real coupling.
- Use `log/slog` for logging.
- Use Echo for HTTP, sqlc with goose migrations for persistence, and `modernc.org/sqlite` for SQLite.
- Use Templ, HTMX, and Tailwind v4 for server-rendered UI.
- Prefer self-documenting code over comments.

## Tests

New or changed observable behavior needs tests.

- Use standard-library table-driven tests.
- Do not add testify.
- Keep tests close to the package they cover.
- Use ephemeral ports in tests that start servers.
- Do not rely on process state from a running Detent orchestrator.

## Commits

Use conventional commits:

```text
<type>(<scope>): <subject>
```

Use one of `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`, or `perf`. Keep the subject imperative, under 50 characters when practical, and without a trailing period.

Examples:

```text
docs(contributing): add contributor workflow
fix(store): close rows after migration lookup
test(scheduler): cover fair-share selection
```

## Branch And Pull Request Flow

1. Start from current `origin/main`.
2. Create a focused branch for the issue.
3. Make the smallest complete change that satisfies the issue.
4. Run focused tests for touched packages.
5. Run `make check`.
6. Open a pull request with a clear summary, a `Fixes #N` line, and the exact test plan.
7. Address review feedback with follow-up commits on the same branch.

Do not commit directly to `main`. Do not bypass hooks. If validation fails, fix the blocker before requesting review.
