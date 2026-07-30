# Scheduled Operations

[Back to README](../README.md#documentation)

## Alert and Scheduled Intake

GitHub projects can turn external events and repository scans into normal board
issues with an `intake` policy in `detent.yaml`. Intake is disabled by default;
an omitted or empty `sources` list starts no receiver or scanner work.

```yaml
intake:
  sources:
    - name: production-errors
      kind: sentry
      secret: $DETENT_SENTRY_INTAKE_SECRET
      match: level:error
      creates:
        status: Backlog
        labels: [bug]
        title: "[{source}] {summary}"
        body: "{details}"
      dedupe_by: fingerprint
    - name: weekly-todos
      kind: schedule
      cron: "0 6 * * 1"
      scan: stale-todos
      creates:
        status: Backlog
        labels: [maintenance]
        title: "{summary}"
        body: "{details}"
      dedupe_by: fingerprint
```

Webhook kinds are `webhook`, `sentry`, `datadog`, and `slack`. Send JSON to
`POST /api/v1/intake/<project-id>/<source-name>` with the resolved source secret
in either `X-Detent-Intake-Token` or `Authorization: Bearer <secret>`. Generic
JSON uses `summary`, `details`, and `fingerprint`; the named adapters also map
their common nested event fields into those values. `match` uses
`field:value` against flattened JSON fields. `dedupe_by` can name any flattened
field and defaults to `fingerprint`.

Templates can reference `{source}`, `{kind}`, `{summary}`, `{details}`,
`{fingerprint}`, and flattened payload fields. Detent stores a hashed intake
marker in the issue body. A later event with the same source and fingerprint
updates that issue and adds configured labels without resetting its status, so
an operator promotion from Backlog remains intact. New issues receive the
configured starting status and then enter the existing label, issue-field, or
ProjectV2 gate pipeline. ProjectV2 intake also requires `tracker.repository` so
Detent knows where to create the repository issue before adding it to the board.

The built-in `stale-todos` scanner walks the project source root for TODO and
FIXME entries while skipping generated/dependency trees such as `.git`,
`node_modules`, `vendor`, `tmp`, `dist`, and `build`. Scanner schedules use
standard five-field cron expressions.

Host-local policy can override the workflow policy inside a project entry in
`global.yaml` using the same `intake` shape. An explicit `sources: []` override
disables workflow-defined intake for that project.

## Scheduled maintenance routines

GitHub and memory-backed projects can schedule repository maintenance agents
from `detent.yaml`. Detent provides the scheduling, proposal, deduplication, and
run-ledger mechanism; every sweep criterion remains project-owned configuration
instead of built-in Go behavior.

```yaml
routines:
  - name: dependency-audit
    schedule: "0 3 * * 1"
    prompt: |
      Apply the dependency-audit criteria in the Maintenance section below.
      Propose only actionable upgrades with repository evidence and explicit
      acceptance criteria.
  - name: flaky-test-sweep
    schedule: "30 4 * * *"
    prompt: |
      Inspect the configured test history and repository guidance for repeatable
      flaky-test findings. Ignore isolated failures without supporting evidence.
```

Names are normalized labels and must be unique. Schedules are standard
five-field cron expressions evaluated in the Detent process timezone, and each
routine requires a non-empty prompt. The prompt may contain the criteria
directly or point the agent to a named section or file in the repository.
Invalid blocks fail workflow validation and appear as `detent doctor` workflow
errors.

When a routine is enabled or its schedule changes, its first run is the next
scheduled occurrence; Detent does not replay missed occurrences. Each run uses
a fresh read-only agent session. The agent proposes zero or more findings with
a stable `dedup_key`, title, and issue body. Detent validates those proposals,
files accepted findings with the `detent:todo` label in `Todo`, and leaves
normal onboarding to the existing board loop.

Filed issue bodies carry a project/routine/key fingerprint. A later run with the
same fingerprint skips filing while the matching issue is open, including when
that issue has moved to another configured board state. Closing the issue allows
a future recurrence to file a new issue. Multiple identical proposals in one
run are also collapsed.

The runtime store records the scheduled, started, and completed times, filed
and deduplicated counts, issue references, and any failure for every run.
`detent doctor` shows the latest result for each configured routine, warns when
a routine has never run, and flags three consecutive failures. ProjectV2
trackers must set `tracker.repository` so Detent can create the repository issue
before adding it to the board.

## Scheduled backlog admission

Backlog admission evaluates existing issues and proposes which ones should
become eligible for dispatch. It does not create work like routines, classify
incoming work like intake, or rank eligible work like the scheduler. Every
proposal is a durable record with an expiry and one audit comment. To accept a
proposal, an operator posts the exact proposal-specific
`/detent admission accept <proposal-id>` command; Detent then moves the issue to
the configured target state. To reject it, the operator posts
`/detent admission reject <proposal-id>`. GitHub command authors must have
write, maintain, or admin permission on the repository.

Admission is disabled unless a project opts in:

```yaml
backlog_admission:
  enabled: true
  schedule: "*/15 * * * *"
  sources:
    states: [Backlog]
    labels: []
    untracked: false
  target_state: Todo
  criteria_section: "Admission criteria"
  require_effort: false
  effort_section: "Issue effort selection"
  exclude_labels: []
  authors:
    allow: []
    allow_association: []
  max_candidates_per_run: 50
  max_proposals_per_run: 3
  max_open_proposals: 10
  proposal_expiry_days: 7
  auto_admit: false
  auto_admit_min_confidence: 0.90
```

State names come from the project's workflow. `sources.labels` selects issues
carrying any configured non-status label, independent of workflow state. It
defaults to empty and does nothing until configured. Labels beginning with
`tracker.status_label_prefix` are rejected; workflow state selection belongs in
`sources.states`. `sources.untracked` defaults to `false`. In GitHub label mode,
enabling it includes open repository issues that carry none of the configured
status labels. This surfaces work filed directly by external automation such as
Sentry or Dependabot even when no repository labeling action runs. It is
unsupported for `project_v2`, `issue_field`, `github_local`, and non-GitHub
trackers because those status models do not define the same missing-label
condition.

Admission reads every enabled selector through the candidate-reader contract.
The tracker declares selector support, reads pages inside a hard per-run bound,
filters the request exactly, sorts by creation time and stable issue identity
before applying the result limit, and reports whether the source was truncated.
Selector results form a deduplicated union. Issues already in `target_state`,
and terminal or blocked issues reached only through `sources.labels`, are
skipped and counted because label selection does not override the workflow
state machine. `exclude_labels` is applied after the union.

`authors.allow` accepts GitHub handles without requiring `@`.
`authors.allow_association` accepts `OWNER`, `MEMBER`, `COLLABORATOR`,
`CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, and `NONE`. The two lists are an
allowlist union: an issue is eligible when either its handle or association
matches. Both lists default to empty, which intentionally leaves admission
unrestricted on every tracker. A GitHub-only restrictive default would silently
empty candidate sets on trackers that do not have GitHub association data and
would add no protection on private repositories.

GitHub `issue_field` reads push a handle-only allowlist into the search query.
Other GitHub status sources filter handles locally. When associations and
handles are both configured, Detent preserves the union by filtering locally;
pushing only the handles would incorrectly discard issues admitted by
association. Query-side author rejections are counted in aggregate just like
local rejections. They are recorded as `author` in the run ledger and never
produce comments on rejected issues.

Admission keeps a finite over-read window of at least 100 issues so priority
ordering, author and label filters, and `max_candidates_per_run` still apply
after connector reads. A reader truncation is recorded as `candidate_reader` in
the run ledger instead of making the bounded result look complete. A run defers
instead of queueing when project or fleet capacity is unavailable or its agent
would exceed the configured daily budget.

Criteria live in one named section of the shared `WORKFLOW.md`, never
`WORKFLOW.local.md`. Heading matching is case-insensitive, accepts ATX headings
at any level and Setext headings, and includes nested subsections until the next
heading of the same or higher level. Headings and bold list items inside fenced
code blocks are examples, not criteria. Missing, empty, unresolved, or
duplicate matching headings fail closed. Dimensions are project-owned: define
them as nested headings or bold list items, and replace the example rubric
wholesale when another project needs different judgments.

```markdown
