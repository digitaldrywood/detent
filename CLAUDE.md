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

- Follow [AGENTS.md issue authoring](AGENTS.md#issue-authoring) for new work: use the selected project's supplied tracker authority and supported context. Repository identity and PR landing do not select the issue tracker.
- Work from a Detent-created worktree branch, never directly on `develop` or `main`. Branch from and target `develop`; `main` is production (see [docs/branching.md](docs/branching.md)).
- Keep generated files and runtime output inside the current worktree.
- Do not bind development or tests to `127.0.0.1:4000`; use ephemeral ports in tests.
- Before implementation, confirm dependencies listed in the issue are merged into `origin/develop`.
- Keep changes scoped to the active issue.
- Follow [INV-15](docs/invariants.md#inv-15--visible-ui-changes-require-a-human-authored-issue): add visible elements or content on any user-facing surface (Cloud app, Hub server-rendered pages, or local Templ dashboard) only when a human-authored issue names that UI change. Machine-filed issues, agent-expanded scope, and "while I was here" additions never qualify. Removing UI or fixing an existing element in place without adding visible content does not need that approval.
- Keep agent diagnostics, coverage, provenance, and debugging data in existing API and MCP reads and logs; never add them to UI, including the Diagnostics page, toggles, or debug flags. Describe a proposed UI addition in the outcome or file a Backlog issue for a human to author or rewrite; do not build it.
- Publish Workpad status through the project's existing tracker owner. For comment-based Workpads, update the authoritative `## Codex Workpad` comment, or post a new canonical comment when editing is unavailable. An issue-body or final-answer status does not supersede an existing canonical comment. Preserve native/local event ownership.
- Run `make generate` before committing when templates, sqlc queries, or CSS inputs change.
- Commit only when explicitly requested by the workflow or human, and use conventional commit messages.

## Issue effort selection

Model and reasoning effort are orchestration settings, not authoring decisions.
They are configured once in the operator's instance config, not in this repo,
and split by stage: Codex Astra (`gpt-6-astra`) plans at `low` effort and
validates at `medium`, and Codex Sol (`gpt-6-sol`) builds (code, rework, merge)
at `high`. `detent.yaml` deliberately carries an empty `agents.model_selection`
block so it inherits that split.

Do not put a `model` in a `detent-agent` block: it overrides every stage,
including Astra planning and validation. Use `effort: high`; each stage clamps
it to its own ceiling, so planning still runs at `low` and validation at
`medium`.

```detent-agent
schema: 1
effort: high
```

Escalation is an operator action: applying the `complexity:very-complex` label
routes the issue to Astra at `medium`. Agents never apply complexity labels and
never assign `xhigh` or `max`.

## Mechanism moratorium

Effective 2026-09-10 until the operator lifts it. Detent has grown a large set
of interacting self-protection mechanisms (brakes, breakers, leases, parks,
recovery sweeps, revocations, reconcilers). Their interactions are now the main
source of incidents.

- INV-11: only new or expanded mechanisms require explicit human scope approval before Todo; assistants file them to Backlog. Features that do not add or expand mechanisms may be filed straight to Todo.
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

- Follow the validation rule in [AGENTS.md](AGENTS.md#validation).
- Follow [deployment and release failure reporting](AGENTS.md#deployment-and-release-failure-reporting): genuine Detent source blockers require at least High priority through the existing native owners, preserving Urgent, Backlog, imported history and operator holds. Instance failures remain instance-owned. Scheduled release validation is separate from ordinary issue merging; do not demand local-gate publication or blocking CI.
- New or modified Go behavior requires focused table-driven tests using only the standard library.
- Add a test only when it asserts a behavior no existing test asserts; extend an existing table or fixture before adding a sibling function. No tests for generated code, documentation text, or coverage numbers. Rationale: [docs/test-suite-audit.md](docs/test-suite-audit.md).
- Generated Go files such as `*_templ.go` and sqlc output do not need hand-written tests.

### Safety-critical orchestrator validation

- `internal/orchestrator/implement_progress.go`, `internal/orchestrator/backend_capacity.go`, `internal/orchestrator/spend_progress.go`, `internal/orchestrator/ranking.go`, `internal/scheduler/global_gate.go`, and the capacity path in `internal/admission/manager.go` are safety-critical brakes and dispatch controls.
- Changes to these files must preserve meaningful safety regression coverage. Changes to their comparison, signature, time-window, ordering, reservation, or capacity-cleanup logic must preserve the seed cases in `FuzzSafetyCriticalOrchestratorBoundaries`, which covers diffstat cleanliness, signature equality, capacity resume arithmetic, spend-progress baselines, dispatch ordering, and priority-only real-capacity acquisition.
- The [scheduled suite](docs/invariants.md#inv-5--local-pull-request-validation-and-scheduled-release-evidence) retains the exact-file coverage floors of at least 90% in [scripts/coverage-exceptions.txt](scripts/coverage-exceptions.txt) and execution of the boundary fuzz seeds on pinned integrated `develop` commits.
- Focused coverage and fuzzing are available as diagnostics during an edit; for example, `go test ./internal/orchestrator -run '^$' -fuzz=. -fuzztime=30s -timeout=60s`. Under [AGENTS.md validation](AGENTS.md#validation), ordinary submission, completion, admission, and merge do not wait for a coverage percentage, fuzz duration, local status, or scheduled run.

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

Follow the [targeted invariant reading guidance](AGENTS.md#repository-invariants)
and [docs/invariants.md](docs/invariants.md). Changes to an invariant or its
enforcement must update that document in the same PR and identify the invariant
in the PR template.
