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

Use generic names in fixtures, mock data, designs, demo screenshots, instructional
copy, and issue-brief examples. Write repository examples as
`example-org/example-project` or `owner/repository`; never hardcode a real
customer's or operator's organization, project, repository, or local path into
these examples. Real names shown by the product must come from the selected
runtime context. Screenshots of actual project behavior may retain that context;
historical bug reports may also retain real names as factual evidence. Do not
reuse either as synthetic example data or product copy.

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

Write acceptance criteria the system can satisfy before merge: tests, fixtures,
source and documentation checks, and UI screenshots. Never require an outside
system, a deployed environment or human action, and add no post-deployment
verification step; a human files a new issue if deployed behavior is unwanted.
See the Issue Contract in WORKFLOW.md.

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
  are attributed to the instance, never to the issue. Pre-provider checkpoint
  and recovery refusals retry through the same instance owner with capped
  backoff; they never move an item to Blocked. Execution fencing and source
  verification remain authoritative.
- Claim eligibility is recomputed from current workflow intent, typed dependency
  edges, source, review, access, lease and instance-capacity facts. Historical
  failure answers, recovery signatures, reason strings and cross-host park
  markers are diagnostics, never additional eligibility vetoes. Genuine human
  actions and permission requests retain their authority.
- Uncertain publication is reconciled against the remote ref before execution:
  an intended SHA match is published, an unchanged ref permits a compare-and-swap
  push (including an absent ref), and a moved ref returns to Rework/rebase.
  Historical `manual_recovery` checkpoint judgments never hold an item; verify
  current source and lease authority through the existing publication owner.
- Blocked is a closed list: only an explicit human question or permission request
  with a typed `human_action`, or an explicit operator stop, may enter it.
  Every machine failure uses the existing capped-backoff retry owner in a
  dispatchable lane; infrastructure remains instance-attributed. Terminal-attempt
  and workspace retry limits never park work or synthesize operator review.
  Existing machine-written Blocked lanes are recovered from current intent on
  every recovery read, without fingerprint, retry-limit or consumed-signature vetoes.
  Stored derived hold metadata requires an owner, source revision and expiry;
  deleting derived holds must not change recomputed dispatch decisions.
- Execution ownership is a fenced, renewed lease. Deferred completion payloads
  do not extend ownership beyond lease expiry. Expired attempts return to claim
  eligibility without a recovery park or park-clearing sweep; source custody
  and genuine human intent remain authoritative.
- Machine failures have one retry owner with growing attempts and capped
  jittered backoff; per-issue failure, progress, spend, dispatch-loop,
  token-ceiling and workspace retry brakes are consolidated into it. A retry
  resumes at the first step whose receipt is no longer valid for the current
  source, policy and review state and never repeats a step whose receipt is
  still valid; rejected review, gate or audit results and new human feedback
  return work to implementation on the same source. Only verified changed
  source resets the attempt count. Spending limits belong to the existing budget
  policy, not to retry brakes. Historical brake metadata never parks work or
  vetoes dispatch.
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

Follow [INV-17](docs/invariants.md#inv-17--runners-are-stateless-like-github-actions-runners):
a runner depends only on its install configuration (Hub URL, identity, capacity,
workspace root, and a declared GitHub credential: a configured worker credential
or, when none is configured, the host's authenticated GitHub CLI). Every job starts in a fresh workspace from an exact commit or a
Hub-stored source bundle, and work that must survive is published to the Hub.
Never add machine-local state that later jobs depend on, configuration discovered
by scanning the machine, or routing by where earlier work ran.

Follow [INV-18](docs/invariants.md#inv-18--hub-writes-stay-small):
a request's write transaction writes only what that request owns and never
rebuilds rollups or reprojects other rows; derived data is refreshed in the
background one item per short transaction. New Hub columns are typed, never JSON.

Follow [INV-19](docs/invariants.md#inv-19--no-flaky-tests): a test that passes
and fails on the same commit is a High defect; fix its cause or delete it,
never retry, wait longer, skip or quarantine it. A suite's overall execution
budget may be sized from measured runtime.

Follow [INV-20](docs/invariants.md#inv-20--sql-never-reads-or-writes-inside-stored-json):
Hub and store SQL never read or write inside stored JSON; fields SQL filters,
joins, orders, aggregates, projects or updates are typed columns. Existing JSON
query uses are migration debt: add none, migrate edited queries to typed columns,
and lower the enforcement baseline when calls are removed. Opaque payloads are
decoded whole by the application; new code adds none, as required by INV-18.

Follow [INV-21](docs/invariants.md#inv-21--designs-hold-at-ten-thousand-tenants-and-runners):
judge every design at 10,000 tenants and 10,000 runners. Request work is bounded
by the request and periodic work by a fixed per-tick budget, never by fleet size
or history; shared locks are never held across per-item network calls or object
walks; fleet traffic is jittered. Changes to hot paths, shared locks or periodic
jobs state their cost at that scale in the Change description.

## Validation

Every change gets a per-change security audit before merge, bound to its exact
base and head. P1/P2 findings send the change to Rework. A native Cloud
project's release-time review found about 29 real P1/P2 issues in twelve days,
but too late to stop the change that caused them. The audit runs through the
configured `security_audit` stage and incurs the same provider usage and costs
as other stages. A project turns it off with `gate.security_audit.enabled: false`.


Workers run focused tests for the packages they change while working and do not
run `make check-land` themselves. The runner's finalization runs the approved
`make check-landing` gate once on the finished source and returns a failure to
Rework with its output. The Cloud project's approved
policy must use `gate.run: make check-landing` and `gate.landing_mode: per_landing`;
the operator reviews and re-approves this policy in the Hub.
Signed commit finalization and Change publication still occur. Clean squashes
join one ordered staging batch and push without a per-landing gate or GitHub
API call; develop accepts that integration risk, and the hourly full suite and
scheduled reporting own any resulting failure. After
a rebase, validate the build, short Go tests for touched packages, and lint of
touched files; a failed command returns the item to Rework with its output.
GitHub pull-request landing retains its configured target and checks. This
repository uses `make check-landing` on the combined tree: build, short tests
for changed packages and their dependents, and lint of changed Go files. Module
changes widen the checks. `make check-land` remains available to operators.
The target generates docs and checks SQL output once, then runs lint, vet,
build and the whole-repository short unit suite without the race detector
concurrently within one `TEST_PROCS` budget. It retains source and workflow
invariants and migration checks. The rest depends on what the change touches
relative to the merge base:

- Changed Go packages: the baseline-aware NilAway audit of those packages.
- `web/conversation` changes: typecheck, vitest and build.
- A changed `scripts/` file: its own `scripts/*_test.py`. A changed `Makefile`
  runs `scripts/check_land_test.py`.

Full browser, race, full Go tests, security checks, integration, repository-wide
NilAway, coverage and extended fuzzing run in the scheduled suite and never
hold landing.
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

Tests never sleep or wait out a hard timeout: they wait on the condition they
assert through a signal from the code under test, `testing/synctest`, or
`testenv.WaitUntil`, bounded only by the `go test -timeout` deadline.

Code that creates rows per key, member, organization, project or other entity
is tested with at least two of those entities in the same database and scope,
and asserts that both are usable. One row proves nothing about uniqueness.
Every UNIQUE or PRIMARY KEY column such a row writes derives from its entity,
never from a fixed label. `TestInsertsNeverWriteConstantUniqueKeys` in each
SQLite store package refuses an INSERT that writes constants into every column
of a unique key, except a singleton upsert whose `ON CONFLICT` key is itself
constant. A new store package adds the same test.

Focused `go test -timeout=60s ./<touched-package>/...`, `go vet`, targeted
regressions, and [safety-critical fuzzing](CLAUDE.md#safety-critical-orchestrator-validation)
are the validation during edits. The finalization `make check-landing` gate is
the completion gate. Give each diagnostic `go test` command an explicit timeout appropriate to
the selected fixtures instead of using Go's ten-minute default. Use a longer
timeout when the named diagnostic needs it. Inspect a timeout or instance failure
before repeating the command; do not retry an unchanged unsupported fixture.
Repeat diagnostics only to verify changed behavior or resolve a concrete remaining
risk. Ordinary submission, completion, admission, and merge do not wait for a
coverage percentage, fuzz duration, local status, or scheduled run. The scheduled
full suite validates pinned `develop` commits and each dispatched release branch
head.
Every release starts by cutting and freezing a `release/vX.Y` branch from
`develop`. Fixes land on `develop` first, then reach the release branch through
reviewed `git cherry-pick -x` PRs. Only the release branch's green full suite
permits a version tag. That tag deploys staging and passes its smoke before
production. Never use an operator develop prerelease or source-gate-only release
path. Follow [the release workflow](docs/branching.md#release-branches), including
for urgent recovery; ordinary develop landing remains independent.

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

UI changes attach screenshots (light/dark, desktop/390 as relevant) to the
issue or PR as review evidence. Screenshots are never compared by pixels or
hashes in any test or gate. Every browser journey runs the same outcome
assertions on every operating system. The frozen
`internal/invariants/browser_policy.json` allowlists may only shrink; remove
entries when files or patterns disappear.

Fix or delete flaky browser tests. Retries provide diagnosis, never a fix.

## Deployment and release failure reporting

For `digitaldrywood/detent` and its selected native Cloud project, failing
`release/*` full suites create or update one open stabilization issue per
release series through the existing native reporting owner, at least High
priority. Each failed job retains its run, attempt, job URLs, pinned SHA,
diagnostics, commands, infrastructure attribution and original replay identity
as a bounded occurrence on that owner. Develop suites retain per-job reporting;
other projects retain their configured reporting behavior. Source, test,
unclassified and infrastructure failures all enter Todo; infrastructure and
unclassified reports carry the infrastructure label and remain attributed to
the CI instance. Reproducible source or test failures that prevent deployment
also require at least High priority. Validate that the selected workflow has
dispatchable, nonterminal Todo and nondispatchable, nonterminal Backlog, neither
operator-only. Other unknown instance diagnostics remain Backlog intake.

Match the open release-series fingerprint for release suites and open job
fingerprints and imported occurrences for develop. Reference matching existing
repairs without restarting them or transferring their ownership. Subsequent
release occurrences update the stabilization owner; group fixes by reproduced
root cause and keep historical failures distinct from current reproduction.
Comment with the new run URL, findings and pinned SHA instead of filing duplicates. Raise unset, Normal or Low priority through the
existing expected-revision edit owner, preserving High and Urgent. Promote
matching scheduled Backlog intake to Todo through the existing native workflow
owner. Preserve human questions, migration/operator holds, active and review
lanes, terminal history, origin stamps, imported provenance and stable replay
identity. A new occurrence does not reopen terminal work or clear holds. A later failed
release occurrence starts a new stabilization episode after its owner completes;
replaying an occurrence from a terminal episode changes nothing. Green release
evidence reconciles only that series' failure set and does not close issues.
Explicit operator cancellation creates no repair issues, including when the
finalizer would otherwise still be running before the run conclusion settles.
Cancelled jobs without workflow cancellation, genuine timeouts and infrastructure
failures remain reportable, and an incomplete suite never permits a release tag.

Scheduled infrastructure reporting does not authorize source repair without a
reproducible test or source diagnostic and does not consume issue failure
allowance. A historical pinned failure does not prove that the current head
fails or staging is down; repair workers reproduce on their actual implementation-start
base before edits and retain that SHA, command and observed failure. An unrelated
later rebase preserves diagnosis provenance; relevant intervening changes require
reassessment. Validate the integrated candidate and reuse applicable exact-source
host receipts under [WORKFLOW.md](WORKFLOW.md#scheduled-ci-repair-contract).
Staging and production deploy sequentially from newly validated release tags,
which require every configured scheduled job to succeed; no release is tagged
for a red suite. Repair guidance must not demand a local-gate status, blocking
CI or CI waiting in ordinary issue merging. Other projects retain their chosen
reporting priority and validation policy.

The operator retired the private Mac hourly producer for Detent on 2026-10-02;
it continues to serve other repositories. Do not restore its Detent selection
or copy its private configuration into this repository. The existing native
scheduled reporter owns durable Detent failure reporting.
