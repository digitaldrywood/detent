# Designing a project for reliable agent delivery

[Back to README](../README.md#documentation)

Agents can produce changes faster than a project can validate and release them.
Increase parallel work only when reviewed changes keep reaching a validated
release. More landed commits with an aging release candidate is a growing repair
queue, not more delivered work.

This guide is for owners of projects using Detent. Your project chooses its
validation commands, required checks, workflow and release policy. The Detent
repository's own [development](development.md) and [branching](branching.md)
rules are not product-wide defaults. The recommendations below describe choices
for your repository and CI; they do not add automatic Detent configuration or
promise that Detent implements your release pipeline.

## Define what each successful check proves

Record these contracts in the project's contributor and agent instructions, and
keep the commands consistent with the approved repository policy and CI.

| Boundary | Evidence to require | Typical checks |
| --- | --- | --- |
| Source completion | Successful commands against the submitted source version | Build, lint, unit and component tests, applicable invariants |
| Integration | Evidence that covers the candidate combined with its landing base, under the project's chosen landing policy | Reliable checks for affected behavior and dependencies; required native queue checks when configured |
| Broad validation | A pinned integrated commit, command versions, environment and complete results | Full integration, race, browser, security and platform suites |
| Release | A validated commit and identifiable immutable artifacts, followed by deployment and smoke results | Project release requirements and checks of the deployed version |

A source check does not establish that two individually correct changes work
together. A fast integration check does not establish that a broad suite passed.
A successful build does not establish that publication or deployment completed.
Keep those outcomes distinct, including tracker-reporting failures.

For a small project, begin with a reliable integration command and broad checks
on isolated CI. Put security, correctness or compliance checks at the boundary
your risk requires; an expensive check is not automatically safe to defer.
Choose the contract explicitly before moving a check out of a blocking gate.

Use the repository's native merge queue when configured and supported by its
landing path. Otherwise use Detent's serialized merge worker. Verify the actual
branch rules and required contexts, rather than relying on a ruleset's name.
Avoid two independent queues or competing merge owners. A managed queue is an
optional repository choice, not a prerequisite for adopting Detent. Start with
bounded queue concurrency; speculation spends additional test capacity.

## Keep the integration gate small and meaningful

Prefer the lowest test layer that catches the failure. Unit, component and API
tests should carry most behavior checks; browser tests should cover critical
journeys that those layers cannot establish. Before adding a test, name the
failure it detects and check whether an existing case already detects it.

Select affected tests from the diff against the actual integration base. Include
dependents of changed libraries, not just directories containing edited files.
Changes to shared fixtures, schemas, build tools, test configuration or dependency
manifests need a wider selection. For a small repository, a committed path-to-test
map can be sufficient if its owner reviews it with affected changes. Give unknown
paths a conservative fallback and retain full scheduled coverage. A smoke set
should contain a few named critical journeys, not a large file with an unrelated
collection of tests.

Keep local and CI tool versions and command semantics aligned. Validate that
required test manifests still refer to tests that exist. A renamed test should
not make local validation green while a scheduled verifier waits for its old
name. Record which checks actually ran; skipped checks are not passed evidence.

Tests should create their own data, use explicit credentials or test doubles,
and avoid dependencies on another test's leftovers, a developer's login, fixed
ports or execution order. Browser tests should wait for the relevant state to
settle before sampling it. Assert user outcomes; assert exact counts, ordering
or copy only when those details are the required behavior. Use screenshots as
review evidence when appropriate. Increasing sleeps, deadlines or retries does
not repair an incorrect readiness assumption.

## Budget the whole host

Runner capacity limits agent sessions, not all processes those agents launch.
A per-command worker limit also does not reserve that much capacity across the
host. Several agent gates can each start parallel package tests, browser workers,
compilers and linters. Nested parallelism multiplies demand.

Measure a representative gate alone, then under the intended concurrent agent
load. Compare wall time, memory pressure, CPU use and first-attempt failures.
Leave room for the operating system and interactive use on a shared workstation.
Put broad suites on isolated CI or dedicated capacity when practical. Keep
fixture isolation even when executions share toolchain caches.

Use the existing [runner capacity controls](runner-capacity.md) and your test
tools' concurrency settings together. Raise one limit at a time, then compare
completed work and waiting time. If adding agents makes gates slower or less
reliable without increasing releases, reduce concurrent demand before extending
timeouts. Load average alone does not identify the cause; inspect the processes
and recorded run evidence.

## Classify failures before retrying or quarantining

Capture the commit, test name, first failure, environment, command and run link.
Distinguish a reproducible source regression, a fixture or assertion defect, a
concurrency failure, and an unavailable or overloaded execution environment.
Passing one test alone after a suite failure does not prove a harmless flake.
It can also indicate shared-state interference or a genuine concurrency defect.

A retry can gather evidence, but retain the first failure and count it in
reliability measurements. Do not turn an eventual pass into a claim that the
first attempt was healthy. Fix deterministic failures directly.

When evidence justifies quarantine, use the project's existing CI selection and
tracker rather than building another scheduler. Record the test, evidence,
responsible owner, tracker issue, review date and re-entry criteria. Continue
running it separately. Removing a check from a merge or release requirement is
an explicit project risk decision, especially for security and concurrency
coverage. Quarantine must not silently weaken the release contract.

Set a project reliability target for blocking checks and review confirmed false
failures against it. Treat that flake budget as a maintenance signal: assign
repairs, remove redundant coverage or improve isolation. Do not add another
repository-wide brake when the budget is exceeded. Restore a test only after
the cause is understood and the fix holds under the relevant execution
conditions. More retries are not re-entry evidence.

## Release snapshots, not a moving branch name

Record the last-known-green commit together with its full validation evidence.
A passing result belongs to that exact commit and environment; it does not
transfer to the current branch tip. Require complete evidence, not a green
workflow whose required jobs were skipped.

Choose scheduled release opportunities independently of agent throughput. For
a small project, releasing an eligible green snapshot may be enough. Add a
stabilization branch only when the project needs fixes while development
continues. Keep the number of active release branches small. Define how fixes
reach both branches; landing on the development branch first and cherry-picking
with provenance is one workable policy. Revalidate the release branch after
each change.

Build immutable artifacts from the validated source and promote those same
artifacts through environments. Verify deployed identity and smoke behavior
before proceeding. Define whether a production branch is the deployment trigger
or a record of what was deployed; those are different contracts.

Make publication resumable after partial success. An existing tag does not prove
that artifact upload, workflow dispatch or deployment succeeded. Resume the
unfinished step without replacing immutable provenance or rerunning successful
validation unnecessarily. A tracker outage must preserve validation and
publication evidence for later reporting.

If no eligible candidate is ready, keep the previous release and report the
missed opportunity. Do not relabel a stale snapshot as newly delivered work or
weaken checks to satisfy the calendar. Document what happens when the first
release candidate predates the release workflow itself.

## Keep work explainable through policy changes

For an idle runner or an old queue entry, inspect the project's actual workflow,
issue contract, explicit dispatch exclusions, dependencies, attempt limits,
reviewed version, approved policy and available runner capacity. Use existing
MCP/API explanations and [recorded history](diagnosis.md), including observation
times and revisions. A lane name or a single readiness snapshot does not prove
that a claim can succeed. If explanation and claim results disagree, preserve
both results and report the mismatch through the selected tracker; do not clear
unrelated holds to force progress.

Before changing required issue sections, gate commands or workflow states,
inspect open work and plan its transition through supported owners. Ensure
machine-created issues satisfy the same contract they will encounter at
dispatch. Coordinate approved policy with deployed runner capabilities and
available commands. Verify representative existing work after the change.
Preserve human holds, dependencies, lease ownership and publication receipts;
restarting a runner is not a migration plan. See [onboarding policy authority](cloud-onboarding.md#workflow-definition-authority)
and [admission criteria](admission.md).

## Review delivery, then tune

Use a consistent observation window and retain failures and cancellations in
the data. Track:

- Accepted work reaching a validated release, and the age of the newest eligible
  release candidate.
- Queue age by stage, plus source-to-release time.
- Gate duration under normal concurrent load and time waiting for capacity.
- First-attempt failures, confirmed false failures and outstanding quarantine age.
- Operator interventions and repeated repair attempts.

Before increasing concurrency, demonstrate that a representative change can
move through review, integration, broad validation and release, and that a
failed check has an owned repair path. Keep normative project instructions in
the repository; keep incidents, ownership, decisions and run evidence in the
selected tracker and CI records. Tune the boundary that limits delivery rather
than optimizing commit count.
