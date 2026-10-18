# CLAUDE.md

## Project Conventions

- Use English for code, comments, documentation, errors, tests, commits, and examples.
- Target Go 1.26 and idiomatic standard-library-first Go.
- Keep application code feature-packaged under `internal/` as the system grows.
- Prefer constructor dependency injection over global state or wire/fx.
- Use interfaces and factories only at backend/plugin boundaries where they remove real coupling.
- Use `log/slog` for logging. Wrap returned errors with `%w` and a short step name; log once at the handling edge with the `error` attribute, never log and return the same error.
- Error messages never embed customer content (titles, bodies, prompts, secrets).
- Use Echo for HTTP, sqlc with goose migrations for persistence, and `modernc.org/sqlite` for SQLite.
- Use Templ, HTMX, and Tailwind v4 for server-rendered UI.
- For Cloud UI work, follow the [Detent design system](docs/design-system/README.md) and its [component contracts](docs/design-system/components.md); it is the source of truth for tokens, components and patterns. Reuse existing components and semantic tokens within the authorized UI scope. `make check-app` runs the design token and catalog checks.
- Use Air for local hot reload and golangci-lint v2 for linting.
- The live dashboard region (`#snapshot`) is updated by **morphing in place** (idiomorph, `hx-swap="morph:innerHTML"`), never a destructive `innerHTML` swap — otherwise hover popovers/tooltips inside it are torn down and rebuilt on every SSE tick and flicker. Any new element added inside the live region must tolerate in-place morph; render hover tooltips/popovers through a single body-level host (see `helpTooltipHost`) outside the swapped region, and re-assert open state on `htmx:afterSettle`. Do not reintroduce an `innerHTML` swap on `#snapshot`.

## Workflow

- Follow [AGENTS.md issue authoring](AGENTS.md#issue-authoring) for new work: use the selected project's supplied tracker authority and supported context. Repository identity and PR landing do not select the issue tracker.
- Work from a Detent-created worktree branch, never directly on `develop` or `main`. Branch from and target `develop`; `main` is production (see [docs/branching.md](docs/branching.md)).
- Always cut and freeze a release branch from `develop`. Land fixes on `develop` first, then cherry-pick with `-x` through release-branch PRs. Tag only after that release branch passes the full suite; deploy the tag to staging, pass smoke, then production. No operator develop prerelease or source-gate-only release path is permitted, including urgent recovery. Follow [release branches](docs/branching.md#release-branches).
- Keep generated files and runtime output inside the current worktree.
- Do not bind development or tests to `127.0.0.1:4000`; use ephemeral ports in tests.
- Before implementation, confirm dependencies listed in the issue are merged into `origin/develop`.
- Keep changes scoped to the active issue.
- Acceptance criteria are limited to what the system can verify before merge (tests, fixtures, source and docs checks, UI screenshots). Never write one that needs an outside system, a deployment or a human, and never add a post-deploy verification step; see the Issue Contract in WORKFLOW.md.
- Follow [INV-15](docs/invariants.md#inv-15--visible-ui-changes-require-a-human-authored-issue): add visible elements or content on any user-facing surface (Cloud app, Hub server-rendered pages, or local Templ dashboard) only when a human-authored issue names the surface or feature it belongs to; a feature issue authorizes the whole surface it describes. Machine-filed issues, agent-expanded scope, and "while I was here" additions never qualify. Removing UI or fixing an existing element in place without adding visible content does not need that approval.
- Keep agent diagnostics, coverage, provenance, and debugging data in existing API and MCP reads and logs; never add them to UI, including the Diagnostics page, toggles, or debug flags. Describe a proposed UI addition in the outcome or file a Backlog issue for a human to author or rewrite; do not build it.
- Publish Workpad status through the project's existing tracker owner. For comment-based Workpads, update the authoritative `## Codex Workpad` comment, or post a new canonical comment when editing is unavailable. An issue-body or final-answer status does not supersede an existing canonical comment. Preserve native/local event ownership.
- Run `make generate` before committing when templates, sqlc queries, or CSS inputs change.
- Commit only when explicitly requested by the workflow or human, and use conventional commit messages.

## Issue effort selection

Model and reasoning effort are orchestration settings, not authoring decisions.
Cloud organization settings hold the default selection; Cloud project settings
may override it. Runners receive the effective selection from the Hub. Do not
configure model selection in `global.yaml` or `detent.yaml`.

The seeded default uses Codex Sol (`gpt-6.1-sol`) at `high` for normal and complex
work, Codex Astra (`gpt-6-astra`) at `low` for planning and `medium` for validation,
and Astra at `medium` for `complexity:very-complex`. Per-issue effort blocks still
clamp to each stage's configured ceiling.

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

## Mechanism budget

Detent carries a large set of interacting self-protection mechanisms (brakes,
breakers, leases, parks, recovery sweeps, revocations, reconcilers). Their
interactions are the main source of incidents, so the set may not grow.

- INV-3: a change that adds or expands a mechanism removes or consolidates an
  existing one in the same change. "Add a guard for the new case" is not a fix.
- Infrastructure failures (backend startup, protocol errors, workspace hooks)
  are attributed to the instance, never to the issue.
- The orchestrator is the only writer of tracker lane state; workers report
  outcomes and never write lane labels.
- This project has no human review step. Never move a Detent issue to Human
  Review. An operator session that recovers or hand-lands work moves the issue
  to Done once its PR merges into `develop`; work that needs a human goes to
  Blocked with a `human_action`.
- Do not add configuration keys, CLI subcommands, or dashboard surfaces to work
  around a mechanism. Fix the mechanism.
- Machine-filed issues carry an origin stamp and a fingerprint; never file a
  duplicate of an open issue, comment on it instead.

## Validation

Every change gets a per-change security audit before merge, bound to its exact
base and head. P1/P2 findings send the change to Rework. A native Cloud
project's release-time review found about 29 real P1/P2 issues in twelve days,
but too late to stop the change that caused them. The audit runs through the
configured `security_audit` stage and incurs the same provider usage and costs
as other stages. A project turns it off with `gate.security_audit.enabled: false`.


- Follow the validation rule in [AGENTS.md](AGENTS.md#validation).
- Follow [deployment and release failure reporting](AGENTS.md#deployment-and-release-failure-reporting): Detent release full-suite failures share one open stabilization owner per release series at least High, with bounded per-job evidence and original replay identities, including unclassified and infrastructure failures. Develop retains per-job reporting; other projects retain their configured behavior. Reference existing repairs without transferring ownership, group fixes by reproduced root cause, and keep historical failures separate from current reproduction. Later failures after completion start a new episode; terminal replays change nothing. Green release evidence reconciles only its own series. Explicit workflow cancellation creates no repairs; genuine timeouts and cancelled jobs without workflow cancellation remain visible. Reuse the matching owner with new occurrence evidence, promote matching Backlog intake through the native workflow owner, and preserve Urgent, imported history, human questions, active/review lanes and operator holds. Infrastructure reports carry the infrastructure label and retain CI-instance attribution; other unknown instance diagnostics remain Backlog intake. A red suite receives no release tag. Scheduled release validation is separate from ordinary issue merging; do not demand local-gate publication or blocking CI.
- New or modified Go behavior requires focused table-driven tests using only the standard library.
- Add a test only when it asserts a behavior no existing test asserts; extend an existing table or fixture before adding a sibling function. No tests for generated code, documentation text, or coverage numbers. A test is low value and is removed in review when it has no assertion or compares a value to itself, computes its expected value with the code under test, breaks on a behavior-preserving refactor, duplicates an existing assertion, or exports a private function only so a test can call it. Coverage percentages are evidence, never a gate; no package or file floor exists. Rationale: [docs/test-suite-audit.md](docs/test-suite-audit.md).
- Generated Go files such as `*_templ.go` and sqlc output do not need hand-written tests.

### Safety-critical orchestrator validation

- `internal/orchestrator/implement_progress.go`, `internal/orchestrator/backend_capacity.go`, `internal/orchestrator/spend_progress.go`, `internal/orchestrator/ranking.go`, `internal/scheduler/global_gate.go`, and the capacity path in `internal/admission/manager.go` are safety-critical brakes and dispatch controls.
- Changes to these files must preserve meaningful safety regression coverage. Changes to their comparison, signature, time-window, ordering, reservation, or capacity-cleanup logic must preserve the seed cases in `FuzzSafetyCriticalOrchestratorBoundaries`, which covers diffstat cleanliness, signature equality, capacity resume arithmetic, spend-progress baselines, dispatch ordering, and priority-only real-capacity acquisition.
- The [scheduled suite](docs/invariants.md#inv-5--local-pull-request-validation-and-scheduled-release-evidence) executes the boundary fuzz seeds and the named safety regression tables on pinned integrated `develop` commits. The guard is those named tests, not a coverage percentage.
- Focused tests and fuzzing are available as diagnostics during an edit; for example, `go test ./internal/orchestrator -run '^$' -fuzz=. -fuzztime=30s -timeout=60s`. Under [AGENTS.md validation](AGENTS.md#validation), ordinary submission, completion, admission, and merge do not wait for a coverage percentage, fuzz duration, local status, or scheduled run.

## Diagnosis

- Follow [docs/diagnosis.md](docs/diagnosis.md) before making causal claims about runtime behavior.
- Throughput, concurrency, regressions, and slowdowns require recorded history; `/api/v1/state` is only a point-in-time snapshot.
- Follow the [recovery operating procedure](docs/diagnosis.md#recovery-operating-procedure) through post-rollout delivery verification and explicit monitoring handoff; a release or occupied slots alone do not establish recovery.

## Tooling

- `make dev` runs Air and rotates `tmp/air-combined.log`.
- `make generate` runs `go generate`, Templ, sqlc, and Tailwind when their inputs exist.
- `make setup` installs Air, Templ, sqlc, goose, and golangci-lint v2.
- `make sqlc` uses `sqlc/sqlc.yaml` by default.
- `make db-create NAME=description` creates a UTC timestamp Goose SQL migration in `internal/store/migrations`; set `MIGRATIONS_DIR=internal/hubserver/migrations` for Hub changes. Keep existing numbered migrations unchanged; do not use Goose sequential mode or renumber at landing.
- `make db-migrate` uses goose against `internal/store/migrations` by default and applies missing timestamp migrations that land after newer versions. Hub migrations run on startup.
- sqlc reads the store migration directory from `sqlc/sqlc.yaml`; a new timestamp migration needs no schema registry edit. Regenerate sqlc output when changing its schema or queries.

## Repository invariants

Follow the [targeted invariant reading guidance](AGENTS.md#repository-invariants)
and [docs/invariants.md](docs/invariants.md). Only a change to an invariant's
rule or its enforcing check edits `docs/invariants.md`. Identify affected INV
IDs in the Change/PR description; per-change rationale, evidence and
verification go there or in issue comments.

Follow [INV-16](docs/invariants.md#inv-16--the-tracker-database-is-the-only-shared-knowledge-channel):
agents, runs and issues share knowledge only through the selected tracker
database (issue bodies, comments, the Workpad, Change/PR records and run history).
Never use repository files to pass knowledge between issues or runs. Keep
repository documentation normative; do not append evidence, notes or history,
and do not treat any repository file as append-only. The current completion
contract retains ownership of tracker publication.

Follow [INV-17](docs/invariants.md#inv-17--runners-are-stateless-like-github-actions-runners):
runners are stateless like GitHub Actions runners. A runner depends only on its
install configuration; every job starts in a fresh workspace from an exact commit
or a Hub-stored source bundle; work that must survive is published to the Hub.
Never pin work to a machine or add machine-local state later jobs depend on.

Follow [INV-18](docs/invariants.md#inv-18--hub-writes-stay-small):
a request's write transaction writes only what that request owns and never
rebuilds rollups or reprojects other rows; derived data is refreshed in the
background one item per short transaction. New Hub columns are typed, never JSON.
