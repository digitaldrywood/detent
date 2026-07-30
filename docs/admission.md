# Admission criteria

[Back to README](../README.md#documentation)

- **Alignment** — Does this serve a stated current priority? Do not propose a
  candidate that maps to no stated priority.
- **Readiness** — Is the problem actionable, with the acceptance conditions
  needed by the agent that will implement it?
- **Size** — Is the work bounded enough for one agent to complete?

## Issue effort selection

- `medium` — Small, mechanical, and tightly specified.
- `high` — Standard feature or fix with ambiguity or a cross-cutting surface.
- `xhigh` — Tricky state, concurrency, restart, or recovery semantics.
```

The agent receives only the resolved criteria and bounded candidate data in a
fresh read-only session. It emits typed proposals. Detent re-fetches each issue,
validates every field and verbatim criteria quote, rejects proposals matching no
dimension, and persists valid records before posting comments. Repeated runs use
a title/body fingerprint, so the proposal's own comment cannot create a
duplicate. Unanswered proposals expire; an issue demoted after acceptance is
not proposed again.

`require_effort` is off by default, preserving admission behavior for existing
projects. When enabled, `effort_section` must identify a separate project-owned
rubric in the shared `WORKFLOW.md`. Effort names are taken from bold or
code-formatted list items in that section rather than from a built-in Detent
rubric. The read-only admission agent recommends one listed effort and explains
why. If the issue has no `detent-agent` block, Detent appends the recommendation
before moving it to the target state. Existing blocks remain authoritative and
are never modified. A missing or invalid recommendation prevents admission.

`auto_admit` is off by default. When enabled, Detent admits proposals whose
confidence is at least `auto_admit_min_confidence`, after revalidating the source
state, author allowlist, and excluded labels. Automatic admissions remain
bounded by `max_proposals_per_run` and `max_open_proposals`. Lower-confidence
proposals still require an explicit accept command.

Acceptance is attributed to the actor who posted the command. Detent records
the proposal ID and operator actor on the resulting target-state transition. An
accept for an issue that has left the configured source states resolves the
proposal as superseded with a recorded reason and does not move the issue.
Rejection, expiry, and supersession are separate outcomes: silence can expire a
proposal but never accepts or rejects one.

An untracked issue has no status to move from. Its proposal explicitly states
that acceptance is a two-part change: assigning the target status and admitting
the work for dispatch.

Capability is declared per tracker and per GitHub status source:

| Tracker | Status source | `sources.states` | `sources.labels` | `sources.untracked` | `authors.allow` | `authors.allow_association` |
| --- | --- | --- | --- | --- | --- | --- |
| `github` | `project_v2` | Supported | Unsupported | Unsupported | Local filter | Supported |
| `github` | `issue_field` | Supported | Supported | Unsupported | States query pushdown when no association union is configured; local filter otherwise | Supported |
| `github` | `label` | Supported | Supported | Supported | Local filter | Supported |
| `github_local` | local SQLite status | Supported | Supported | Unsupported | Local filter after GitHub hydration | Supported through GitHub hydration |
| `local_sqlite` | local SQLite status | Supported | Supported | Unsupported | Stored values only; discovery warning | Unsupported |
| `memory` | process-local status | Supported | Supported | Unsupported | Seeded values only; discovery warning | Unsupported |
| `linear` | Linear workflow state | Unsupported | Unsupported | Unsupported | Unsupported | Unsupported |

An enabled admission configuration fails validation when its tracker/status
source does not declare every enabled selector, so unsupported combinations
never validate cleanly and then degrade to an empty first scheduled read.
ProjectV2 label selection is unsupported because its issue query fetches only
the first 20 labels; configuring either `sources.labels` or `exclude_labels`
with ProjectV2 fails validation rather than risking a silent miss. Configuring
`authors.allow_association` on a tracker that cannot supply GitHub association
data also fails validation. `github_local` supports association explicitly by
hydrating each locally selected issue from GitHub before admission filters it.
`authors.allow` on `local_sqlite` or `memory` produces a doctor warning because
those trackers do not discover authors. The memory tracker is evaluation-only
across restarts: durable proposal records can survive while its process-local
comments and mutations do not.

Repository visibility is part of the GitHub repository diagnostic. On a public
repository, doctor warns whenever the configured author rules still let
untrusted associations reach the candidate set, including the states-only
configuration above. The warning is based on author reachability, not whether a
state, label, or untracked source found the issue. `OWNER`, `MEMBER`, and
`COLLABORATOR` are treated as trusted association scopes; allowing
`CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, or `NONE` keeps the public-exposure
warning active.

Integration accounts do not reliably receive a trusted association. Dependabot,
Sentry, and similar bot accounts commonly appear as `CONTRIBUTOR` or `NONE`.
When association scoping is used to protect an untracked or automation-fed
candidate source, list every intended bot explicitly in `authors.allow` by
handle. Otherwise the same association rule that excludes strangers also
excludes the automation the source was meant to catch.

Every lane-entry event records how the issue reached the state:

- `human` means the tracker explicitly identified a `User` actor for the
  transition. It confirms a person moved the issue, but does not imply that the
  person endorsed any admission proposal.
- `routine` and `retro` are unattended Detent-created transitions and imply no
  human vetting.
- `dependency` is a deterministic dependency auto-unblock or blocker promotion
  and implies no human vetting at transition time.
- `admission` means an explicit accept command was correlated with an open
  proposal and its later target-state transition. This is proposal-specific
  operator approval.
- `unknown` means the tracker did not provide enough actor or mechanism data.
  Detent never guesses that an unknown transition was human.

The board card and detail sheet show the current lane origin and actor when one
is available. The admission ledger records accepted, rejected, expired, and
superseded proposal counts with average decision time. Accepted proposals also
accumulate completion, rework entries, review events, and metered spend after
the decision. `detent doctor` reports that labeled history alongside the origin
distribution; it does not reduce calibration to a single acceptance
percentage. It also warns when criteria cannot be resolved, admission has never
run, tracker limitations apply, or three consecutive runs fail.
