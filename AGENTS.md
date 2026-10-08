# AGENTS.md - Detent Agent Notes

- For future requested product or code changes, do not implement directly by default.
- File focused work through the selected project's issue authority using its provided supported MCP/API context, and let Detent dogfood the work (see [Issue authoring](#issue-authoring)).
- Only make direct code changes when the human explicitly asks for manual implementation, asks to finish an already-started fix, or asks for local review and diagnostics that require edits.

## Implementation

For Cloud UI work, use the [Detent design system](docs/design-system/README.md)
and its [component contracts](docs/design-system/components.md); it is the
source of truth for tokens, components and patterns, and `make check-app` runs
its token and catalog checks.
Reuse existing primitives and screen compositions; select semantic tokens and
component variants before introducing feature-specific styling. Proposed
components and layout baselines do not expand the authorized UI scope.

Follow [INV-15](docs/invariants.md#inv-15--visible-ui-changes-require-a-human-authored-issue):
add visible UI only when a human-authored issue names the surface or feature it
belongs to. This covers the Cloud app (`web/conversation`), Hub server-rendered
pages, and the local Templ dashboard. A human-authored feature issue authorizes
the whole surface it describes, including the rows, controls, states, empty and
error copy, and responsive layout that surface needs. Machine-filed issues,
agent-expanded scope, and "while I was here" additions never qualify.
Removing UI or fixing an existing element in place without adding visible
content does not need that approval.

Diagnostics, coverage, provenance, and debugging data for agents belong in the
existing API and MCP reads and logs. Never add UI to make them observable,
including on the Diagnostics page or behind toggles or debug flags.

New Hub and store SQL migrations use UTC Goose timestamps:
`YYYYMMDDHHMMSS_name.sql`. Preserve historical migration filenames and the fixed
cutover versions in `tools/migrationcheck`; do not advance those cutovers.
Adding a SQL migration requires only its new file, with no schema-version
registry edit or landing-time renumbering. Resolve generated-file conflicts by
regenerating from combined source inputs.
See [concurrent migrations and generated files](docs/development.md#concurrent-migrations-and-generated-files).

## Issue authoring

Use the selected project's actual tracker and supplied project context.
A GitHub repository identity or PR/merge landing path does not make GitHub
the issue authority.

- For a native project, file through its provided project-scoped `file_issue`
  or supported application owner. Keep issue content, discussion, typed
  dependencies, workflow and deduplication with that native owner, including
  imported provenance. Follow the supplied scoped connection and request
  contracts; see [API & MCP setup](docs/api-mcp-setup.md) and
  [Hub API](docs/hub-api.md).
- File GitHub issues only when the selected tracker/project context makes
  GitHub the issue authority. Retain that project's configured filing path
  and workflow mapping, including `detent:todo` only where configured and
  authorized. Preserve the Backlog scope rules below.
- Preserve origin stamps, stable problem fingerprints and duplicate checks
  in the selected tracker. Match open work before filing; comment on a
  matching issue instead of creating a duplicate when authorized.

Respect supplied authority, grants and the current completion contract.
If the required supported context is missing or contradictory, record that
limitation through the existing workflow; do not silently write to another
tracker. Do not invent endpoints, project IDs, credentials or routing, or
extract credentials from hidden browser state or raw runtime databases.

Under [INV-15](docs/invariants.md#inv-15--visible-ui-changes-require-a-human-authored-issue),
an agent that believes a visible UI addition is needed describes it in its outcome
or files a Backlog issue for a human to author or rewrite; it does not build it.
A machine-filed issue does not authorize the addition.

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
- Do not add configuration keys, CLI subcommands, or dashboard surfaces to work
  around a mechanism. Fix the mechanism.
- Machine-filed issues carry an origin stamp and a fingerprint; never file a
  duplicate of an open issue, comment on it instead.

## Repository invariants

Follow [docs/invariants.md](docs/invariants.md). Start with its headings and
read the sections governing the touched behavior, including linked prerequisites.
Read additional sections when the affected boundary requires them; do not dump
the entire document into startup context. All repository invariants still apply.
Only a change to an invariant's rule or its enforcing check edits
`docs/invariants.md`. Identify affected INV IDs in the Change/PR description;
per-change rationale, evidence and verification go there or in issue comments.

Follow [INV-16](docs/invariants.md#inv-16--the-tracker-database-is-the-only-shared-knowledge-channel):
agents, runs and issues share knowledge only through the selected tracker
database (issue bodies, comments, the Workpad, Change/PR records and run history).
Never use repository files to pass knowledge between issues or runs. The
repository holds product source and normative documentation, never shared logs
or notebooks; no repository file is append-only by convention. The current
completion contract owns tracker publication and grants workers no extra writes.

## Validation

Run `make check-land` before reporting source work complete and fix every
failure in the same run. Do not hand back a red gate. Native landing reuses the
successful receipt for the reviewed head and tree: clean squashes join one
ordered staging batch and push without another gate or GitHub API call. After
a rebase, validate the build, short Go tests for touched packages, and lint of
touched files; a failed command returns the item to Rework with its output.
GitHub pull-request landing retains its configured target and checks.
The target generates docs and checks SQL output once, then runs lint, vet,
build and the whole-repository short unit suite concurrently within one
`TEST_PROCS` budget. It retains source and workflow invariants and migration
checks. Conversation changes also run typecheck, vitest and build. Full
behavioral invariants, integration, race, coverage, fuzz and the baseline-aware
NilAway audit remain scheduled release validation.
No local commit status or additional CI producer is required. Every target
shares the host through `TEST_PROCS` (default 4); see
[docs/development.md](docs/development.md).

Add a test only when it asserts a behavior no existing test asserts. Before
writing one, name the failure it would catch; if you cannot, do not write it.
Extend an existing table or fixture with a case instead of adding a sibling
function that rebuilds the same setup. Do not add tests for generated code,
for the text of documentation or configuration, or to move a coverage number.
A test that only re-executes a path another test already asserts is removed in
review, as is a test with no assertion or a self-comparison, a test whose
expected value is computed by the code under test, a test that breaks on a
behavior-preserving refactor, or a private function exported only for a test
(see [docs/test-suite-audit.md](docs/test-suite-audit.md)). Coverage profiles
are scheduled evidence only; no package, file or aggregate coverage floor
exists, and no change is made to move a percentage.

Focused `go test -timeout=60s ./<touched-package>/...`, `go vet`, targeted
regressions, and [safety-critical fuzzing](CLAUDE.md#safety-critical-orchestrator-validation)
are available for diagnostics during edits. `make check-land` is the
completion gate. Give each diagnostic `go test` command an explicit timeout appropriate to
the selected fixtures instead of using Go's ten-minute default. Use a longer
timeout when the named diagnostic needs it. Inspect a timeout or instance failure
before repeating the command; do not retry an unchanged unsupported fixture.
Repeat diagnostics only to verify changed behavior or resolve a concrete remaining
risk. Ordinary submission, completion, admission, and merge do not wait for a
coverage percentage, fuzz duration, local status, or scheduled run. The scheduled
full suite validates pinned `develop` commits and tags only green commits.
A new validated release deploys staging and passes its smoke before deploying production.

Other projects using Detent choose their own validation commands, required
checks, workflow triggers, and release policies. Do not introduce a product-wide
bypass to implement this repository's policy.

Detent workers must use their provided `TMPDIR`, `TMP`, or `TEMP`; never
fall back to host scratch space in a worker.

### Browser tests

Browser specs in `tests/visual` cover critical user journeys only. Verify UI
issues by default with Vitest component tests in `web/conversation` or Go handler
tests. Add a browser test only for a journey no existing spec covers and a
component or API test cannot catch; otherwise extend the existing journey spec
instead of adding a file.

Prefer the fastest test layer that catches the failure. Before adding browser
coverage, check existing Playwright, Vitest and Go tests for the same behavior;
do not duplicate coverage. Do not consolidate spec files solely to reduce file
count. Performance changes must preserve valuable coverage and demonstrate an
improvement through comparable measurements.

Every test creates its own data, through the API where possible, and never
depends on another test's leftovers or test order. Do not use describe serial
mode or keep mutable shared state in `beforeAll`. Reset or isolate server state
per test, using a unique project or account or resetting persisted user
preferences in `beforeEach`.

Locate elements by `getByRole`, then `getByLabel`, then `getByText`, then
`getByTestId`; do not use CSS or XPath structure. Assert the user's outcome,
not layout, exact counts or element order unless the issue concerns that detail.
Use web-first assertions; never use `waitForTimeout` or fixed sleeps. Wait for
the UI state that follows an animation or debounce.

Use `toHaveScreenshot` only for the short allowlisted set of key pages. Generate
baselines only in the pinned Playwright Linux container and only in a change
whose purpose is visual. The frozen `internal/invariants/browser_policy.json`
allowlists may only shrink; remove entries when files or patterns disappear.

Fix or delete flaky browser tests. Retries provide diagnosis, never a fix.

## Deployment and release failure reporting

For `digitaldrywood/detent` and its selected native Cloud project, every
failing scheduled full-suite job creates or updates one issue at least High
priority through the existing selected native reporting owner. Source, test,
unclassified and infrastructure failures all enter Todo; infrastructure and
unclassified reports carry the infrastructure label and remain attributed to
the CI instance. Reproducible source or test failures that prevent deployment
also require at least High priority. Validate that the selected workflow has
dispatchable, nonterminal Todo and nondispatchable, nonterminal Backlog, neither
operator-only. Other unknown instance diagnostics remain Backlog intake.

Match open job fingerprints and imported occurrences first. Comment with the
new run URL, failing test names or lint/vet findings, and pinned develop SHA
instead of filing duplicates. Raise unset, Normal or Low priority through the
existing expected-revision edit owner, preserving High and Urgent. Promote
matching scheduled Backlog intake to Todo through the existing native workflow
owner. Preserve human questions, migration/operator holds, active and review
lanes, terminal history, origin stamps, imported provenance and stable replay
identity. A new occurrence does not reopen terminal work or clear holds.

Scheduled infrastructure reporting does not authorize source repair without a
reproducible test or source diagnostic and does not consume issue failure
allowance. A historical pinned failure does not prove that the current head
fails or staging is down; repair workers verify the failure on their current
base. Staging and production deploy sequentially from newly validated release tags,
which require every configured scheduled job to succeed; no release is tagged
for a red suite. Repair guidance must not demand a local-gate status, blocking
CI or CI waiting in ordinary issue merging. Other projects retain their chosen
reporting priority and validation policy.

The operator retired the private Mac hourly producer for Detent on 2026-10-02;
it continues to serve other repositories. Do not restore its Detent selection
or copy its private configuration into this repository. The existing native
scheduled reporter owns durable Detent failure reporting.
