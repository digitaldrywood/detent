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

## Issue effort selection

The default is Codex Sol (`gpt-5.6-sol`) at `high` effort for all work: features,
fixes, tests, reviews, and routine implementation, including cross-component work.

Every issue must include an explicit `detent-agent` block, with `model` unset:

```detent-agent
schema: 1
effort: high
```

- `high` — the default; use it unless the issue states a documented reason not to.
- `low` — an exception that requires a written reason in the issue, and only for
  trivial mechanical edits.
- `medium` — an exception that requires a written reason in the issue.
- `xhigh` and `max` — operator-designated only; never assign automatically.

Concurrency, recovery, routing, multiple files, or a new endpoint alone never
justify changing the effort. Preserve intentional operator exceptions and leave
`model` unset so the issue inherits the fleet-standard model.

## Mechanism moratorium

Effective 2026-09-10 until the operator lifts it. Detent has grown a large set
of interacting self-protection mechanisms (brakes, breakers, leases, parks,
recovery sweeps, revocations, reconcilers). Their interactions are now the main
source of incidents.

- Do not add a new brake, breaker, lease, park, recovery path, revocation,
  reason code, or reconciliation loop.
- A fix for a misbehaving mechanism must remove or consolidate a mechanism, or
  state in the PR why it cannot. "Add a guard for the new case" is not a fix.
- Infrastructure failures (backend startup, protocol errors, workspace hooks)
  are attributed to the instance, never to the issue.
- The orchestrator is the only writer of tracker lane state; workers report
  outcomes and never write lane labels.
- Do not add configuration keys, CLI subcommands, or dashboard surfaces to work
  around a mechanism. Fix the mechanism.
- Machine-filed issues carry an origin stamp and a fingerprint; never file a
  duplicate of an open issue, comment on it instead.

## Validation

- `make check` is the local pre-review gate.
- `make check` runs build, golangci-lint, go vet, race tests, and a 70% coverage gate.
- New or modified Go behavior requires focused table-driven tests using only the standard library.
- Generated Go files such as `*_templ.go` and sqlc output do not need hand-written tests.

### Safety-critical orchestrator validation

- `internal/orchestrator/implement_progress.go`, `internal/orchestrator/backend_capacity.go`, `internal/orchestrator/spend_progress.go`, `internal/orchestrator/ranking.go`, `internal/scheduler/global_gate.go`, and the capacity path in `internal/admission/manager.go` are safety-critical brakes and dispatch controls.
- Changes to these files must preserve their exact-file coverage floor of at least 90% in `scripts/coverage-exceptions.txt` and pass `make check`.
- Changes to their comparison, signature, time-window, ordering, reservation, or capacity-cleanup logic must preserve the seed cases and pass `FuzzSafetyCriticalOrchestratorBoundaries`, which covers diffstat cleanliness, signature equality, capacity resume arithmetic, spend-progress baselines, dispatch ordering, and demand-driven priority reservations.
- Run `go test ./internal/orchestrator -run '^$' -fuzz=. -fuzztime=30s` before submitting such changes.

## Diagnosis

- Follow [docs/diagnosis.md](docs/diagnosis.md) before making causal claims about runtime behavior.
- Throughput, concurrency, regressions, and slowdowns require recorded history; `/api/v1/state` is only a point-in-time snapshot.

## Tooling

- `make dev` runs Air and rotates `tmp/air-combined.log`.
- `make generate` runs `go generate`, Templ, sqlc, and Tailwind when their inputs exist.
- `make setup` installs Air, Templ, sqlc, goose, and golangci-lint v2.
- `make sqlc` uses `sqlc/sqlc.yaml` by default.
- `make db-migrate` uses goose against `internal/store/migrations` by default.

## Repository invariants

Follow [docs/invariants.md](docs/invariants.md). Changes to an invariant or its
enforcement must update that document in the same PR and identify the invariant
in the PR template. Run `make check`, including the existing invariant gate.
