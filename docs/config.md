# Configuration

Detent has two configuration layers:

- `global.yaml` selects projects and controls host-wide runtime settings.
- Each project has a checked-in `detent.yaml` machine contract, with optional
  machine-local overrides in `detent.local.yaml`. The project contract can also
  live in legacy `WORKFLOW.md` YAML frontmatter.

This page is the single reference for both configuration layers. Project
configuration is documented below after the host-wide settings.

## Host configuration

At startup, Detent resolves `global.yaml` in this order. The first matching rule wins.

| Order | Rule | Path |
| --- | --- | --- |
| 1 | `--config <path>` | Direct file path from the CLI flag |
| 2 | `CONFIG=<file>` | Direct file path from the environment |
| 3 | `CONFIG_HOME=<dir>` | `<dir>/global.yaml` |
| 4 | `os.UserConfigDir()` | `<config-dir>/detent/global.yaml` |
| 5 | Legacy home config | `~/.detent/global.yaml` |

`os.UserConfigDir()` maps to `%AppData%\detent\global.yaml` on Windows, `~/Library/Application Support/detent/global.yaml` on macOS, and `~/.config/detent/global.yaml` on Linux while honoring `XDG_CONFIG_HOME`.

`DETENT_CONFIG` and `DETENT_HOME` remain deprecated fallbacks for one release. Detent uses `CONFIG_HOME` instead of `HOME` because `HOME` is standard process state, not Detent configuration.

If no global config is found, Detent keeps the single-project fallback and looks for `WORKFLOW.md` in the current working directory. Use `detent config path` to print the resolved config path and the rule that selected it.

Runtime settings resolve in this order: explicit flag, environment variable,
`global.yaml`, then built-in default.

| Setting | Flag | Environment | `global.yaml` key | Default |
| --- | --- | --- | --- | --- |
| Environment | `--env` | `ENV`, then `DETENT_ENV` | `env` | `prod` |
| Log level | `--log-level` | `LOG_LEVEL`, then `DETENT_LOG_LEVEL` | `log_level` | `info` |
| Log max size | | `LOG_MAX_SIZE_BYTES`, then `DETENT_LOG_MAX_SIZE_BYTES` | `log_max_size_bytes` | `52428800` |
| Log backups | | `LOG_MAX_BACKUPS`, then `DETENT_LOG_MAX_BACKUPS` | `log_max_backups` | `5` |
| GitHub token | | `GITHUB_TOKEN` | `github_token` | required for GitHub projects |
| API token | | `DETENT_API_TOKEN` | `api_token` | open on loopback, fail closed on non-loopback |
| Loopback peer read trust | | | `trust_loopback_peer_read` | `false` |
| Private dashboard URL | | | `dashboard_access` | disabled |
| Hub scheduler | | | `client.hub_url` | disabled |
| Hub worker token | | `DETENT_HUB_TOKEN` | `client.token_env` names the environment variable | required when Hub scheduling is enabled |
| tmux window status | | `$TMUX` detection | `ops.tmux_window_status` | enabled inside tmux |
| Web port | `--port` | `PORT` | `port` | `4000` |
| Instance name | | | `instance_name` | short hostname |
| Health webhook | | | `notifications.health.webhook.url` | disabled |
| Health notification debounce | | | `notifications.health.debounce_seconds` | `300` |
| Health webhook timeout | | | `notifications.health.webhook.timeout_ms` | `5000` |
| Automatic update checks | | | `update.auto_check_enabled` | `false` |
| Update check interval | | | `update.check_interval_hours` | `6` |
| Automatic update apply when idle | | | `update.auto_apply_enabled` | `false` |
| Maximum automatic update deferral | | | `update.max_deferral_hours` | `6` |

Automatic apply waits for an idle runtime until `update.max_deferral_hours`
elapses, then pauses new dispatches and lets in-flight attempts finish within
their configured session ceilings before applying and restarting. Add
`[detent-critical]` to a GitHub release body to bypass the idle wait and start
that drain immediately.

The web host resolves from `--host`, then the first registered workflow's
`server.host`, then the built-in `127.0.0.1` default. It is not a top-level
`global.yaml` key.

When Detent starts inside tmux, it renames the current window to a compact
running, ready, waiting, and blocked count summary. It restores the original
name on clean shutdown. Set `ops.tmux_window_status: false` to disable this
behavior; Detent never invokes tmux when `$TMUX` is absent.

Use `github_token: gh` in `global.yaml` to resolve the token from
`gh auth token` at startup. Literal token values also work but should not be
committed. `github_token: gh-auth`, `${gh auth token}`, and
`$(gh auth token)` are accepted aliases. If neither `GITHUB_TOKEN` nor
`github_token` is set, Detent falls back to existing per-workflow
`tracker.api_key` handling.

Set `trust_loopback_peer_read: true` to grant tokenless read-scope API access
to direct loopback TCP peers even when Detent binds a non-loopback address or
has an `api_token`. The setting hot-reloads and applies only to `GET` routes;
admin routes and mutations still require credentials. Authentication uses only
the request's direct peer address and never trusts forwarded-client headers.
Do not enable this setting when a reverse proxy runs on the same host: every
request relayed by that proxy has a loopback direct peer, including requests
that originally came from remote clients.

Use `instance_name` to distinguish browser tabs and the dashboard navbar when
several Detent instances are open at once. Detent resolves the display name
from the first non-empty value in this order: top-level `instance_name` in
`global.yaml`, `global.identity.name`, the short hostname, then empty. In
single-project fallback mode without `global.yaml`, workflow top-level
`identity.name` is used before the short hostname. Names are trimmed, must be a
single line, and are capped at 40 characters in the web UI.

Configure `client.hub_url` to move candidate discovery and claiming to a Detent
Hub. The machine registers its identity, project and pool capabilities,
capacity, operating system, architecture, and binary version. It then
heartbeats while polling the Hub and renewing fenced work leases. The token is
read from the environment variable named by `client.token_env`; the token is
never stored in the configuration file.

```yaml
client:
  hub_url: https://hub.example.com
  token_env: DETENT_HUB_TOKEN
  machine_id: build-mac-01
  display_name: Build Mac 01
  capacity: 4
  heartbeat_interval_seconds: 30
  lease_ttl_seconds: 90
  request_timeout_ms: 10000
```

`machine_id` defaults to the configured global identity, instance name, or
hostname. `capacity` defaults to `global.max_concurrent_agents`. The heartbeat
interval must be shorter than the lease TTL. Hub outages mark project refresh
health degraded and back off with the normal scheduling loop; they do not
consume issue retry, no-progress, or work-failure budgets. GitHub remains in
use for event ingestion, reconciliation, write-back, and fresh safety checks,
but the scheduling claim cycle does not depend on a GitHub read.

Health notifications deliver fleet and project needs-attention transitions to
one generic webhook. They are disabled when
`notifications.health.webhook.url` is absent, preserving the silent default.
Configure the host-wide channel in `global.yaml`:

```yaml
notifications:
  health:
    debounce_seconds: 300
    webhook:
      url: https://alerts.example.com/detent
      headers:
        Authorization: Bearer replace-me
      timeout_ms: 5000
```

The five-minute default debounce applies independently to entry and recovery.
Project identities are stable per project and cause, currently
`ci_unavailable` and `dispatch_stall`; a separate fleet identity represents
the aggregate transition while any project cause remains active. A flap that
clears before the debounce expires emits neither entry nor recovery. Once an
entry is emitted, exactly one recovery is emitted after the identity remains
healthy for the debounce window.

Webhook payloads use event name `detent.health.transition` and include a stable
event ID, host and instance names, scope, optional project ID, entered state,
causes, optional wait reasons, and entry timestamps. Delivery state and pending
events are stored in the runtime SQLite database before sending, so a restart
does not re-fire active conditions or discard unsent transitions. Failed sends
retry at most five times with exponential backoff starting at 30 seconds and
capped at 15 minutes. Every failure is logged and appears in
`health_notification_failures` on `/health`; `detent doctor` reports both
retrying and exhausted deliveries. Notification configuration changes require
a restart. Treat configured headers as secrets and keep host-specific values
out of checked-in project configuration.

Automatic update checks are host-specific and disabled by default. When
enabled, Detent persists the last check and schedules the next jittered check
from that timestamp, so restarts preserve the remaining delay and overdue
checks run promptly. The six-hour default keeps release uptake within the same
day while using about four GitHub requests per host per day. Detent reports the
last check, available or applied version, and next check on `/health` and the
Health dashboard. `detent doctor` surfaces the last and next check times,
reports whether the host is opted in, and suggests enabling checks when it is
not. Automatic apply remains off unless explicitly enabled and only replaces
release-installer binaries; other install sources remain notification-only.
When work attempts are active, Detent shows the pending version in the web and
terminal dashboards and waits for the next fleet-wide idle window before
applying it. An operator can instead confirm immediate apply from the web
notification. A successful apply uses the normal graceful drain path, then
re-executes the replaced binary on POSIX systems or exits cleanly for an
external supervisor to restart it on other platforms.

`detent doctor` prints the resolved runtime values and their sources, with the
GitHub token redacted.

## Choose a starting point

- [`config.example.yaml`](../config.example.yaml) is the smallest working
  project config.
- [`config.annotated.yaml`](../config.annotated.yaml) is a realistic GitHub
  setup with explanations beside each deliberate choice.
- The [worked multi-project configuration](examples/multi-project/README.md)
  pairs an annotated host config with two complete project configs and explains
  the operator rationale behind their scheduling and isolation choices.
- [`config.reference.yaml`](../config.reference.yaml) contains every supported
  project key. It is fully commented so you can copy only what you need.

Keep `detent.yaml` portable and checked in. Put machine-specific paths or
secrets in `detent.local.yaml`, a credential reference, or host configuration.
The local file overlays the shared file recursively and is not a second,
independent configuration.

## How the subsystems fit together

### Identity, tracker, and polling

`identity` names the Detent persona that owns work. `tracker` connects that
persona to GitHub, Linear, an embedded SQLite board, or the in-memory tracker.
The tracker declares active, observed, and terminal workflow states; state maps,
transition automation, admission, planning, stop behavior, and routing all
refer back to those names. `polling` controls how often Detent refreshes the
tracker when no event gives it a fresher signal. Its
`refresh_failure_threshold` controls how many consecutive failures after a
successful refresh promote project and fleet health to `needs_attention`; a
project whose first refresh fails needs attention immediately.

Choose exactly one GitHub status source: ProjectV2, the repository issue
`Status` field, or repository labels. `dependency_auto_unblock`,
`blocked_recovery`, and `blocker_auto_promote` are opt-in tracker automations:
the first advances work whose dependencies cleared, the second revives
agent-recoverable PR maintenance, and the third promotes blockers when they
enter configured states.

GitHub and Linear trackers also receive a default Statuspage base URL through
`tracker.status_page_url`; set that key to override the provider endpoint.
Detent follows redirects and polls the unauthenticated Statuspage summary and
unresolved-incident feeds at a low-frequency baseline, increasing freshness
only while a Detent-observed `tracker_unavailable` condition is active. These
feeds decorate health output only: malformed, slow, or unreachable status
pages never create a condition or influence dispatch and recovery. Statuspage
incident webhooks are a possible future optimization, not part of the current
polling integration.

### Workspace, deliverable, and worker placement

`workspace` controls isolation, source and output roots, branch creation, cache
sharing, and cleanup. `deliverable` selects pull requests or file artifacts and
their review destination. `worker` distributes sessions across optional SSH
hosts; per-state and global concurrency limits remain under `agent`.

GitHub-capable workers never inherit `GITHUB_TOKEN`, `GH_TOKEN`, their
enterprise variants, or the host's GitHub CLI configuration. Leave
`worker.github_token` empty to run workers with no GitHub credential policy.
Set it to `gh` to resolve `gh auth token` in the Detent process and copy only
the resolved token into the worker's isolated `GH_CONFIG_DIR`. An environment
reference remains available when permission or revocation isolation is useful:

```yaml
worker:
  github_token: gh
  github_rest_min_remaining_reserve: 1250
  github_rest_poll_interval_ms: 60000
```

After resolving `gh`, Detent classifies an exact orchestrator token or any token
that resolves to the same GitHub user as `shared_budget`: GitHub's primary REST
limit is per user, so a second token changes permissions and revocation without
creating another rate-limit pool. Shared-pool telemetry uses
`consumer: shared_pool` and reports usage attribution as indeterminate rather
than assigning the pool's global usage to workers. A different GitHub user or
App installation is `distinct_principal`, the true rate-limit-isolation mode.

The default worker reserve is 1250, above the default orchestrator dispatch
floor of 1000. Shared-mode workers therefore brake first so dispatch can
continue. Detent probes the worker credential at launch and no more often than
the configured interval, and stops or refuses a worker at its reserve. Keep
literal tokens out of checked-in project files.

`workpad.structured_only` requires machine-readable workpad status instead of
accepting legacy narrative signals. `dependencies.source` chooses whether
native tracker dependencies, merged pull requests, or both determine
readiness.

### Agents, backends, and routing

`agent` is the orchestration policy: concurrency, turn and session limits,
retry brakes, spend limits, shutdown, dispatch priority, automatic promotion,
state-specific instructions, learned context, skill drafts, and follow-up
work. `agents.backends` defines pluggable Codex or Claude Code processes, while
`agents.routes` selects a backend and model by role or issue selector.

`codex` is the legacy/default Codex backend configuration and remains the
fallback when `agents.backends` is empty. A Codex backend inherits omitted
values from it. Claude Code backend options are interpreted only when the
backend `kind` is `claude_code`.

`agent.auto_promote` and `gate` work together. Auto-promotion decides when an
item may leave its source state; the gate decides whether CI, automated review,
human approval, a validator, or an artifact status is sufficient. `plan`
optionally inserts a plan-review stop before implementation.

### Operations, budgets, and observability

`server` controls the project dashboard bind and Kanban mutation policy.
`active_hours` optionally limits new agent dispatches to recurring weekday and
wall-clock spans in a required IANA timezone. Use window values such as
`Mon-Fri 22:00-06:00`; ranges that end before their start wrap past
midnight, and `00:00-24:00` covers a full day. A project entry in `global.yaml`
overrides `global.active_hours`, and either host-local value overrides the
project's checked-in setting. Window close drains existing agents instead of
stopping them.

Detent evaluates membership on each scheduler tick, preserving local
wall-clock hours through daylight-saving changes and restarts. Spring forward
can shorten a wrapping window by one hour, while fall back can lengthen it by
one hour. `detent doctor` previews opening and closing times in both the named
timezone and UTC. A temporary `detent resume <id> --for <duration>` override
admits work outside the window until its persisted expiry; manual pause remains
the stronger, independent gate.

`observability` controls dashboard refresh, efficiency anomaly thresholds, and
optional OTLP export. `budget` is the project-wide spend policy;
`agent.budget` can further constrain agent sessions. `release` enables
release-readiness automation after merged work accumulates.

`observability.stranded_active_threshold_seconds` controls when Health and
`detent doctor` report an issue that remains in `In Progress` without a live
worker and when Detent recovers it automatically. Issues with an open pull
request or recoverable workspace work move to `Rework`; issues confirmed to
have no recovery artifacts move to `Todo`. Detent holds the issue in place
when artifact evidence is unavailable. The default is 600 seconds, which
tolerates normal gaps between a completed session and prompt re-dispatch.

`hooks` run lifecycle commands around worktree creation, agent execution, and
cleanup. Treat them as trusted code: they run with the Detent process
permissions and should be short, deterministic, and safe to retry.

### Intake, retrospectives, routines, and admission

`intake.sources` turns webhook or scheduled external signals into tracker
items. `retro` periodically mines completed work for recurring operational
lessons. `routines` schedules arbitrary agent prompts against supported
trackers.

`backlog_admission` is an opt-in scheduled pass that evaluates items from
configured source states against a named criteria section in `WORKFLOW.md`,
then proposes a bounded number for a target state. Its five-field cron,
source-state membership, distinct target state, tracker support, and positive
run limits are enforced by the same validators that feed the generated table
below. When `require_effort` is enabled, `effort_section` names a separate
project-owned `WORKFLOW.md` rubric whose bold or code-formatted list items
define the allowed recommendations. Detent writes a recommended `detent-agent`
block before the admission transition only when the issue has no existing
block; the default remains disabled for compatibility.

Admission screens out non-deliverable tracker and intake artifacts before the
agent run. Add `<!-- detent:no-dispatch -->` to an issue body for a deterministic
operator opt-out. The decline remains in the configured source state, is
commented once, and is reconsidered only after the title or body changes.

When enabled, admission defaults to `*/15 * * * *`, which limits normal
candidate wait time to 15 minutes. An every-30-minute schedule implies up to 30
minutes of latency, an hourly schedule up to one hour, and a daily schedule up
to 24 hours. Restricting a daily schedule to weekdays creates a 72-hour weekend
gap. A slow schedule is valid, but it can make admission look broken because
issues accumulate in the source state while Detent is simply waiting for the
next run.

On the host used to establish the default, 20 admission runs and 18 admission
agent sessions averaged about 25 seconds of wall time per run and 36,000 tokens
per candidate-bearing run, with 655,368 tokens total. Runs with no eligible
candidates are cheaper because they do not start an admission agent. At the
15-minute default, there are at most 96 runs per day; use `detent doctor` to
compare your schedule with recent candidate-bearing runs and the runtime and
token cost observed on your own host. Choose a slower explicit schedule when
that latency/cost tradeoff is intentional.

The onboarding workflow builder accepts a named `INTAKE_PROFILE` and expands
these `answers.env` keys into the typed configuration below. The generated
field reference remains derived from the YAML schema; this table documents the
interview-to-config mapping that exists before YAML is rendered.

| `answers.env` key | Generated config target |
| --- | --- |
| `INTAKE_PROFILE` | Selects the manual, assisted, or autonomous answer expansion |
| `FOLLOWUPS_ENABLED` | `agent.followups.enabled` |
| `BACKLOG_ADMISSION_ENABLED` | `backlog_admission.enabled`; the entire block is omitted when false |
| `BACKLOG_ADMISSION_SCHEDULE` | `backlog_admission.schedule` |
| `BACKLOG_ADMISSION_SOURCE_STATE` | `backlog_admission.sources.states` |
| `BACKLOG_ADMISSION_TARGET_STATE` | `backlog_admission.target_state` |
| `BACKLOG_ADMISSION_CRITERIA_SECTION` | `backlog_admission.criteria_section` and the matching shared `WORKFLOW.md` heading |
| `BACKLOG_ADMISSION_MAX_CANDIDATES_PER_RUN` | `backlog_admission.max_candidates_per_run` |
| `BACKLOG_ADMISSION_MAX_PROPOSALS_PER_RUN` | `backlog_admission.max_proposals_per_run` |
| `BACKLOG_ADMISSION_MAX_OPEN_PROPOSALS` | `backlog_admission.max_open_proposals` |
| `BACKLOG_ADMISSION_PROPOSAL_EXPIRY_DAYS` | `backlog_admission.proposal_expiry_days` |
| `BACKLOG_ADMISSION_AUTO_ADMIT` | `backlog_admission.auto_admit` |
| `BACKLOG_ADMISSION_AUTO_ADMIT_MIN_CONFIDENCE` | `backlog_admission.auto_admit_min_confidence` |
| `BACKLOG_ADMISSION_AUTHORS_ALLOW_ASSOCIATION` | `backlog_admission.authors.allow_association` |
| `ROUTINES_ENABLED` | Controls whether the generated `routines` list is present |
| `ROUTINE_NAME` | `routines[].name` |
| `ROUTINE_SCHEDULE` | `routines[].schedule` |
| `ROUTINE_PROMPT` | `routines[].prompt` |
| `STALE_TODOS_ENABLED` | Controls whether the built-in scheduled `stale-todos` source is present |
| `STALE_TODOS_SCHEDULE` | `intake.sources[].cron` for the `stale-todos` source |

## Fleet staleness warnings

`observability.staleness` detects work items that exceed lane-specific aging
thresholds, non-empty dispatch or merge queues that stop advancing, and
repeated identical scheduler decisions. Lane residency starts at the most
recent entry. Repeated entries can still raise a cumulative warning when their
residency exceeds the lane threshold within
`observability.staleness.lane_reentry_window_hours`. Human-gate lanes emit one
reminder at their threshold, then remain quiet until the lane changes or
`observability.staleness.human_gate_rearm_hours` passes. Sticky breaker and
operator parks with a recorded cause are excluded from lane aging because the
Blocked lane already carries their operator signal. A `park_cause_stale`
warning replaces lane aging when recorded evidence is cleared or can no longer
be verified.

Operator-facing warnings use the same authorization selector as board and
runtime rows. Item warnings outside that selector are excluded before they are
logged, delivered, or counted. Project-level liveness warnings are computed
from authorized dispatch and merge queues. Repeated decisions about the
authorization boundary itself, including `authorization_selector_declined`,
remain visible even when the affected item is out of scope because the selector
decision is the condition the operator must correct.

A Blocked item observed from tracker state without a recorded blocker cause is
classified as `blocked, cause unrecorded`. The card keeps that classification
beside its lane provenance, and an eventual lane-aging warning names the missing
cause and the remedy instead of presenting an unexplained generic Blocked age.

The repeated-decision detector considers only current, non-terminal items.
Closed and merged items and items in a configured terminal state are excluded.
`observability.staleness.repeated_decision_benign_reasons` lists scheduler
reasons that represent healthy waits rather than operator-actionable stalls.
Its defaults cover active workers, dependency waits, global-capacity waits, and
GitHub REST pacing. Set the list explicitly to replace those defaults for a
project. Values must exactly match a scheduler-emitted reason. Configuration
validation reports the configured value and the emitted reason set when they do
not match; for example, `unauthorized` does not match
`authorization_selector_declined`. This validation is also surfaced by
`detent doctor` during config resolution.

The detector evaluates decisions inside
`observability.staleness.repeated_window_hours`, but the orchestrator retains
only the 500 most recent scheduler decisions. Its effective history is
therefore whichever is smaller: the configured time window or the retained
500-decision sample.

Warnings appear in the dashboard, `/health`, and `detent doctor`. Configure
`observability.staleness.webhook.url` to push each newly active warning as a
`detent.staleness.warning` JSON event. The payload includes a stable warning ID,
the affected project or item, the reason, and age and threshold values in
seconds. A delivered warning is not sent again on every scheduler tick. A
dashboard dismissal persists until the warning condition changes, such as a
new lane entry or a new scheduler cause.

## Unstarted GitHub checks

`tracker.github_unstarted_check_threshold_seconds` controls when a GitHub check
that remains queued without a `started_at` timestamp appears as unstarted. The
15-minute default is intentionally non-zero because a cold or busy runner pool
can legitimately take several minutes to pick up a job; setting the threshold
too low would turn ordinary queueing into an operator-facing signal.

## Codex deliverable elicitation

`codex.deliverable_elicitation_allowlist` declares exact MCP mutation tuples
that an unattended Codex worker may confirm for its configured deliverable. The
same field is available under `agents.backends[].options` for a routed Codex
backend. It defaults to an empty list. Each entry requires `server`, `tool`, and
`repository`; server-only entries are invalid.

```yaml
codex:
  approval_policy: never
  deliverable_elicitation_allowlist:
    - server: codex_apps
      tool: github.create_pull_request
      repository: digitaldrywood/detent
    - server: codex_apps
      tool: github.update_pull_request
      repository: digitaldrywood/detent
    - server: codex_apps
      tool: github.add_comment_to_issue
      repository: digitaldrywood/detent
    - server: codex_apps
      tool: github.update_issue_comment
      repository: digitaldrywood/detent
    - server: codex_apps
      tool: github.update_issue
      repository: digitaldrywood/detent
    - server: codex_apps
      tool: github.create_issue
      repository: digitaldrywood/detent
```

The allowlist is a narrow exception to `codex.approval_policy`, not a broader
approval mode. With either `never` or `on-request`, Detent accepts an MCP
elicitation only when it correlates to exactly one pending tool call on the
same server, thread, and turn; the correlated tool is a supported workflow
deliverable mutation; and both its repository argument and the corresponding
active work-item repository match the configured tuple. Pull-request mutations
match the linked pull-request repository. Issue and Workpad mutations match the
tracked issue repository and are accepted only when it is also the deliverable
repository. Detent declines these direct Codex issue mutations when the linked
pull request belongs to another repository because MCP elicitation cannot
rewrite the already-pending arguments through the visibility-aware publication
protector. Connector-backed issue writes remain available and apply that
protector before sending content. Pull-request and issue creation and updates use
`repository_full_name`; issue comment creation and updates use
`repo_full_name`. Missing or ambiguous correlation, other mutation tools
including `github.delete_issue`, and repository mismatches are declined.
Command, file-change, permission, and user-input requests retain their existing
handling.

Allowlisting `github.create_issue` authorizes issue creation only on the
workflow's own tracked repository. This supports workers filing scoped issues
they discover while delivering the current work item; it does not authorize
issue deletion, transfer, or creation in any other repository. Those mutations
remain non-deliverable.

Allowlisting `github.update_issue` permits its full exposed mutation surface:
title, body, state, assignees, milestone, and labels may all be replaced. The
allowlist correlates and constrains the repository, but does not constrain the
other approved arguments.

With `approval_policy: never` and an empty allowlist, non-browser MCP
elicitations are declined exactly as before. `chrome-devtools` empty-form tool
approval remains a separate built-in behavior and is unchanged by this list.

## Generated field reference

The generated block reflects the YAML-tagged Go structs reachable from the
project config. It reads effective defaults through the real normalization
path and derives validation text by exercising boundary values through the
real loader and validators. “None” means no field-level rule was surfaced;
subsystem interactions can still constrain a value.

Run `go generate ./...` after changing the config structs, defaults, or
validators. `make check` compares the committed generated artifacts with fresh
rendering and fails on drift.

`budget.per_issue_max_usd` and `agent.budget.per_issue_max_usd` are
estimate-based admission backstops, not mid-session spend ceilings. Detent
checks cumulative issue spend plus a preflight session estimate before starting
the agent. Actual session cost can exceed that estimate and the configured
backstop. Once the next projected dispatch would exceed the backstop, Detent
parks the issue on a hard hold until an operator raises or disables the cap, or
explicitly retries after resetting the hold. `refusal_cooldown_seconds` applies
only to resettable budget pacing and never clears a per-issue hard hold.

<!-- BEGIN GENERATED CONFIG REFERENCE -->

| Key | Type | Default | Required | Validation |
| --- | --- | --- | --- | --- |
| `active_hours` | `object` | `see child fields` | Conditional | .timezone: is required when active_hours is set<br>.timezone: must be a valid IANA timezone<br>.windows: must contain at least one recurring window |
| `active_hours.timezone` | `string` | `none` | No | None |
| `active_hours.windows` | `list<string>` | `[]` | No | values: must use <WEEKDAY-FROM>-<WEEKDAY-TO> <HH>:<MM>-<HH>:<MM> |
| `agent` | `object` | `see child fields` | No | None |
| `agent.auto_promote` | `object` | `see child fields` | No | None |
| `agent.auto_promote.allowed_issue_labels` | `list<string>` | `[]` | No | labels must not be blank |
| `agent.auto_promote.enabled` | `boolean` | `false` | No | None |
| `agent.auto_promote.gate_wait_state` | `string` | `"source"` | No | must be one of source, review |
| `agent.auto_promote.gate_wait_timeout_action` | `string` | `"human_review"` | No | must be one of merge, human_review |
| `agent.auto_promote.gate_wait_timeout_seconds` | `integer` | `3600` | No | must be greater than 0 |
| `agent.auto_promote.no_progress_limit` | `integer` | `3` | No | must be greater than or equal to 0<br>tracker.active_states, tracker.observed_states, or tracker.terminal_states must include Blocked when agent.auto_promote.no_progress_limit is greater than 0 |
| `agent.auto_promote.optout_label` | `string` | `"requires-human-review"` | No | must not be blank |
| `agent.auto_promote.pass_state` | `string` | `"Merging"` | No | None |
| `agent.auto_promote.quiet_seconds` | `integer` | `600` | No | must be greater than or equal to 0 |
| `agent.auto_promote.rework_limit` | `integer` | `3` | No | must be greater than or equal to 0 |
| `agent.auto_promote.rework_state` | `string` | `"Rework"` | No | None |
| `agent.auto_promote.source_state` | `string` | `"Human Review"` | No | None |
| `agent.budget` | `object` | `see child fields` | No | None |
| `agent.budget.billing_mode` | `string` | `none` | No | must be one of metered, subscription |
| `agent.budget.enabled` | `boolean` | `false` | No | None |
| `agent.budget.override_max_duration_seconds` | `integer` | `172800` | No | must be greater than 0 |
| `agent.budget.override_max_multiplier` | `number` | `3` | No | must be greater than 0 |
| `agent.budget.per_day_max_usd` | `number` | `50` | No | must be greater than 0 |
| `agent.budget.per_issue_max_usd` | `number` | `5` | No | must be greater than 0 |
| `agent.budget.pricing_path` | `string` | `"priv/pricing/models.yaml"` | No | is required |
| `agent.budget.refusal_cooldown_seconds` | `integer` | `3600` | No | must be greater than or equal to 0 |
| `agent.dispatch_priority_by_label` | `list<string>` | `[]` | No | labels must not be blank |
| `agent.dispatch_priority_by_state` | `list<string>` | `[]` | No | state names must be unique<br>state names must not be blank |
| `agent.effort` | `object` | `see child fields` | No | None |
| `agent.effort.code` | `string` | `none` | No | must be one of low, medium, high, xhigh, max, ultracode |
| `agent.effort.merge` | `string` | `none` | No | must be one of low, medium, high, xhigh, max, ultracode |
| `agent.effort.rework` | `string` | `none` | No | must be one of low, medium, high, xhigh, max, ultracode |
| `agent.experimental_thread_resume` | `boolean` | `true` | No | None |
| `agent.failure_breaker` | `object` | `see child fields` | No | None |
| `agent.failure_breaker.cooldown_seconds` | `integer` | `3600` | No | must be greater than 0 |
| `agent.failure_breaker.same_class_limit` | `integer` | `5` | No | must be greater than 0 |
| `agent.failure_breaker.window_seconds` | `integer` | `3600` | No | must be greater than 0 |
| `agent.followups` | `object` | `see child fields` | No | None |
| `agent.followups.enabled` | `boolean` | `true` | No | None |
| `agent.instructions_by_state` | `mapping<string, string>` | `{}` | No | state "__invalid__" must reference a configured workflow state |
| `agent.instructions_by_transition` | `mapping<string, mapping<string, string>>` | `{}` | No | source state "__invalid__" must reference a configured workflow state |
| `agent.knowledge` | `object` | `see child fields` | No | None |
| `agent.knowledge.enabled` | `boolean` | `true` | No | None |
| `agent.knowledge.max_bytes` | `integer` | `65536` | No | None |
| `agent.knowledge.sources` | `list<object>` | `[]` | No | None |
| `agent.knowledge.sources[].name` | `string` | `none` | No | None |
| `agent.knowledge.sources[].path` | `string` | `none` | Conditional | must not be blank |
| `agent.lessons` | `object` | `see child fields` | No | None |
| `agent.lessons.enabled` | `boolean` | `false` | No | None |
| `agent.lessons.max_entries` | `integer` | `50` | No | must be greater than 0 |
| `agent.lessons.path` | `string` | `".detent/lessons.md"` | No | must be a relative path inside the workspace |
| `agent.lessons.postmortem_max_tokens` | `integer` | `1024` | No | must be greater than 0 |
| `agent.lessons.recall_n` | `integer` | `10` | No | must be greater than or equal to 0 |
| `agent.lifetime_limit_cooldown_seconds` | `integer` | `3600` | No | must be greater than 0 when a lifetime limit is enabled |
| `agent.lifetime_limit_override_label` | `string` | `"allow-lifetime-limit"` | No | None |
| `agent.lifetime_session_limit` | `integer` | `120` | No | must be greater than or equal to 0 |
| `agent.lifetime_token_limit` | `integer` | `750000000` | No | must be greater than or equal to 0 |
| `agent.max_concurrent_agents` | `integer` | `10` | No | must be greater than 0 |
| `agent.max_concurrent_agents_by_state` | `mapping<string, integer>` | `{}` | No | limits must be positive integers |
| `agent.max_retry_backoff_ms` | `integer` | `300000` | No | must be greater than 0 |
| `agent.max_session_context_multiplier` | `number` | `0` | No | must be greater than or equal to 0 |
| `agent.max_session_duration_ms` | `integer` | `7200000` | No | must be greater than or equal to 0 |
| `agent.max_session_token_override_field` | `string` | `none` | No | None |
| `agent.max_session_token_override_label` | `string` | `none` | No | None |
| `agent.max_session_tokens` | `integer` | `0` | No | must be greater than or equal to 0 |
| `agent.max_turn_duration_ms` | `integer` | `0` | No | must be greater than or equal to 0 |
| `agent.max_turns` | `integer` | `20` | No | must be greater than 0 |
| `agent.merge_fallback_max_duration_ms` | `integer` | `1200000` | No | must be greater than 0 |
| `agent.merge_fast_path` | `object` | `see child fields` | No | None |
| `agent.merge_fast_path.enabled` | `boolean` | `true` | No | None |
| `agent.merge_fast_path.fairness_age_seconds` | `integer` | `7200` | No | must be greater than 0 |
| `agent.merge_worker_max_duration_ms` | `integer` | `21600000` | No | must be greater than 0 |
| `agent.merge_worker_startup_timeout_ms` | `integer` | `240000` | No | must be greater than 0 |
| `agent.no_progress_spend_limit_usd` | `number` | `3` | No | must be greater than or equal to 0 |
| `agent.no_progress_timeout_ms` | `integer` | `5400000` | No | must be greater than or equal to 0 |
| `agent.no_progress_token_limit` | `integer` | `25000000` | No | must be greater than or equal to 0 |
| `agent.output_truncation` | `object` | `see child fields` | No | None |
| `agent.output_truncation.max_bytes` | `integer` | `0` | No | must be greater than or equal to 0 |
| `agent.overload_retry_delay_ms` | `integer` | `45000` | No | must be greater than 0 |
| `agent.prioritize_unblockers` | `boolean` | `true` | No | None |
| `agent.rate_window_pacing` | `object` | `see child fields` | No | None |
| `agent.rate_window_pacing.floor_percent` | `number` | `20` | No | must be greater than 0 and less than or equal to 100 |
| `agent.rate_window_pacing.mode` | `string` | `"proportional"` | No | must be one of proportional, off, floor |
| `agent.rate_window_pacing.stale_after_seconds` | `integer` | `900` | No | must be greater than 0 |
| `agent.resume_orphaned_sessions` | `boolean` | `true` | No | None |
| `agent.shutdown` | `object` | `see child fields` | No | None |
| `agent.shutdown.drain_timeout_ms` | `integer` | `75000` | No | must be greater than or equal to 0 |
| `agent.skills` | `object` | `see child fields` | No | None |
| `agent.skills.creation` | `object` | `see child fields` | No | None |
| `agent.skills.creation.enabled` | `boolean` | `true` | No | None |
| `agent.skills.creation.max_drafts_per_run` | `integer` | `1` | No | must be greater than 0 |
| `agent.skills.enabled` | `boolean` | `true` | No | None |
| `agent.skills.max_skills_in_prompt` | `integer` | `50` | No | must be greater than 0 |
| `agent.skills.path` | `string` | `".detent/skills"` | No | must be a relative path inside the workspace |
| `agent.stop_run` | `object` | `see child fields` | No | None |
| `agent.stop_run.target_state` | `string` | `"Blocked"` | No | must be included in tracker.observed_states |
| `agents` | `object` | `see child fields` | No | None |
| `agents.backends` | `list<object>` | `[]` | No | None |
| `agents.backends[].command` | `string` | `none` | Conditional | is required |
| `agents.backends[].id` | `string` | `none` | Conditional | is required |
| `agents.backends[].kind` | `string` | `none` | Conditional | is required |
| `agents.backends[].options` | `mapping` | `see child fields` | No | None |
| `agents.backends[].options.allowed_tools` | `list<string>` | `[] for Claude Code` | No | None |
| `agents.backends[].options.approval_policy` | `string or mapping` | `{"reject":{"mcp_elicitations":true,"rules":true,"sandbox_approval":true}} for Codex` | No | None |
| `agents.backends[].options.deliverable_elicitation_allowlist` | `list<object>` | `[] for Codex` | Conditional | values.repository must be owner/name<br>values.server is required<br>values.tool is required |
| `agents.backends[].options.deliverable_elicitation_allowlist[].repository` | `string` | `none` | No | None |
| `agents.backends[].options.deliverable_elicitation_allowlist[].server` | `string` | `none` | No | None |
| `agents.backends[].options.deliverable_elicitation_allowlist[].tool` | `string` | `none` | No | None |
| `agents.backends[].options.disallowed_tools` | `list<string>` | `[] for Claude Code` | No | None |
| `agents.backends[].options.effort` | `string` | `none for Claude Code` | No | must be one of low, medium, high, xhigh, max, ultracode |
| `agents.backends[].options.extra_args` | `list<string>` | `[] for Claude Code` | No | None |
| `agents.backends[].options.include_partial_messages` | `boolean` | `false for Claude Code` | No | None |
| `agents.backends[].options.model_provider` | `string` | `none for Codex` | No | must be a sanitized label containing only letters, numbers, dots, underscores, or hyphens |
| `agents.backends[].options.permission_mode` | `string` | `"bypassPermissions" for Claude Code` | No | must be one of default, acceptEdits, bypassPermissions<br>must not be plan for unattended workers |
| `agents.backends[].options.read_timeout_ms` | `integer` | `5000 for Codex` | No | must be greater than or equal to 0 |
| `agents.backends[].options.service_tier` | `string` | `none for Codex` | No | None |
| `agents.backends[].options.shell` | `string` | `Codex: platform default shell; Claude Code: none` | No | None |
| `agents.backends[].options.stall_timeout_ms` | `integer` | `Codex: 300000; Claude Code: 0` | No | must be greater than or equal to 0 |
| `agents.backends[].options.thread_sandbox` | `string` | `"workspace-write" for Codex` | No | None |
| `agents.backends[].options.turn_sandbox_policy` | `mapping<string, value>` | `{} for Codex` | No | None |
| `agents.backends[].options.turn_timeout_ms` | `integer` | `Codex: 3600000; Claude Code: 0` | No | must be greater than or equal to 0 |
| `agents.backends[].protocol` | `string` | `none` | No | must be app-server for codex<br>must be headless for claude_code |
| `agents.backends[].provider` | `string` | `none` | No | must be a sanitized label containing only letters, numbers, dots, underscores, or hyphens |
| `agents.routes` | `list<object>` | `[]` | No | None |
| `agents.routes[].backend` | `string` | `none` | Conditional | is required<br>must reference a configured backend |
| `agents.routes[].default` | `boolean` | `false when configured` | No | None |
| `agents.routes[].model` | `string` | `none` | No | None |
| `agents.routes[].model_field` | `string` | `none` | No | None |
| `agents.routes[].name` | `string` | `none` | No | None |
| `agents.routes[].role` | `string` | `none` | No | None |
| `agents.routes[].selector` | `object` | `see child fields` | No | None |
| `agents.routes[].selector.and` | `list<mapping>` | `[]` | No | None |
| `agents.routes[].selector.assignee_in` | `list<string>` | `[]` | No | None |
| `agents.routes[].selector.author_in` | `list<string>` | `[]` | No | None |
| `agents.routes[].selector.fields` | `list<object>` | `[]` | No | None |
| `agents.routes[].selector.fields[].name` | `string` | `none` | No | None |
| `agents.routes[].selector.fields[].value` | `string` | `none` | No | None |
| `agents.routes[].selector.labels` | `object` | `see child fields` | No | None |
| `agents.routes[].selector.labels.exclude` | `list<string>` | `[]` | No | None |
| `agents.routes[].selector.labels.include` | `list<string>` | `[]` | No | None |
| `agents.routes[].selector.or` | `list<mapping>` | `[]` | No | None |
| `agents.routes[].selector.priority_in` | `list<integer>` | `[]` | No | values must be integers 1 through 4 |
| `backlog_admission` | `object` | `see child fields` | No | None |
| `backlog_admission.authors` | `object` | `see child fields` | No | None |
| `backlog_admission.authors.allow` | `list<string>` | `[]` | No | None |
| `backlog_admission.authors.allow_association` | `list<string>` | `[]` | No | requires author association, but tracker.kind memory cannot supply it<br>values must be one of OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR, NONE |
| `backlog_admission.auto_admit` | `boolean` | `false` | No | None |
| `backlog_admission.auto_admit_by_label` | `mapping<string, boolean>` | `{}` | No | None |
| `backlog_admission.auto_admit_min_confidence` | `number` | `0.9` | No | None |
| `backlog_admission.criteria_section` | `string` | `none` | Conditional | is required |
| `backlog_admission.effort_file` | `string` | `"WORKFLOW.md"` | No | must be WORKFLOW.md or AGENTS.md |
| `backlog_admission.effort_section` | `string` | `none` | Conditional | is required when require_effort is true |
| `backlog_admission.enabled` | `boolean` | `false` | No | None |
| `backlog_admission.exclude_labels` | `list<string>` | `[]` | No | None |
| `backlog_admission.max_candidates_per_run` | `integer` | `50` | No | must be greater than 0 |
| `backlog_admission.max_open_proposals` | `integer` | `10` | No | must be greater than 0 |
| `backlog_admission.max_proposals_per_run` | `integer` | `3` | No | must be greater than 0 |
| `backlog_admission.proposal_expiry_days` | `integer` | `7` | No | must be greater than 0 |
| `backlog_admission.require_effort` | `boolean` | `false` | No | None |
| `backlog_admission.schedule` | `string` | `"*/15 * * * *"` | No | must be a valid five-field cron expression |
| `backlog_admission.sources` | `object` | `see child fields` | No | must configure at least one selector |
| `backlog_admission.sources.labels` | `list<string>` | `[]` | No | None |
| `backlog_admission.sources.states` | `list<string>` | `[]` | No | values must differ from target_state<br>values must name a configured workflow state |
| `backlog_admission.sources.untracked` | `boolean` | `false` | No | requires candidate selector untracked, but tracker.kind memory does not provide github label status drift |
| `backlog_admission.target_state` | `string` | `none` | Conditional | is required<br>must name a configured workflow state |
| `budget` | `object` | `see child fields` | No | None |
| `budget.billing_mode` | `string` | `none` | No | must be one of metered, subscription |
| `budget.enabled` | `boolean` | `false` | No | None |
| `budget.override_max_duration_seconds` | `integer` | `172800` | No | must be greater than 0 |
| `budget.override_max_multiplier` | `number` | `3` | No | must be greater than 0 |
| `budget.per_day_max_usd` | `number` | `50` | No | must be greater than 0 |
| `budget.per_issue_max_usd` | `number` | `5` | No | must be greater than 0 |
| `budget.pricing_path` | `string` | `"priv/pricing/models.yaml"` | No | is required |
| `budget.refusal_cooldown_seconds` | `integer` | `3600` | No | must be greater than or equal to 0 |
| `codex` | `object` | `see child fields` | No | agents.backends.protocol must be app-server for codex |
| `codex.approval_policy` | `string or mapping` | `{"reject":{"mcp_elicitations":true,"rules":true,"sandbox_approval":true}}` | No | None |
| `codex.command` | `string` | `"codex app-server"` | No | is required |
| `codex.deliverable_elicitation_allowlist` | `list<object>` | `[]` | No | None |
| `codex.deliverable_elicitation_allowlist[].repository` | `string` | `none` | No | must be owner/name |
| `codex.deliverable_elicitation_allowlist[].server` | `string` | `none` | Conditional | is required |
| `codex.deliverable_elicitation_allowlist[].tool` | `string` | `none` | Conditional | is required |
| `codex.model_provider` | `string` | `none` | No | must be a sanitized label containing only letters, numbers, dots, underscores, or hyphens |
| `codex.read_timeout_ms` | `integer` | `5000` | No | must be greater than 0 |
| `codex.service_tier` | `string` | `none` | No | None |
| `codex.shell` | `string` | `platform default shell` | No | None |
| `codex.stall_timeout_ms` | `integer` | `300000` | No | must be greater than or equal to 0 |
| `codex.thread_sandbox` | `string` | `"workspace-write"` | No | None |
| `codex.turn_sandbox_policy` | `mapping<string, value>` | `{}` | No | None |
| `codex.turn_timeout_ms` | `integer` | `3600000` | No | must be greater than 0 |
| `deliverable` | `object` | `see child fields` | No | None |
| `deliverable.kind` | `string` | `"pull_request"` | No | must be one of pull_request, artifact |
| `deliverable.merge_method` | `string` | `"squash"` | No | must be one of squash, merge, rebase |
| `deliverable.output_root` | `string` | `none` | No | None |
| `deliverable.review_url` | `string` | `none` | No | None |
| `dependencies` | `object` | `see child fields` | No | None |
| `dependencies.source` | `string` | `"merged"` | No | must be one of merged, native_only |
| `gate` | `object` | `see child fields` | No | None |
| `gate.approval_label` | `string` | `"human-approved"` | No | None |
| `gate.artifact` | `object` | `see child fields` | No | None |
| `gate.artifact.pass_statuses` | `list<string>` | `["approved","complete","completed","pass","passed","valid"]` | No | None |
| `gate.artifact.rework_statuses` | `list<string>` | `["changes_requested","failed","invalid","rework"]` | No | None |
| `gate.artifact.status_field` | `string` | `"validation_status"` | No | None |
| `gate.artifact.wait_statuses` | `list<string>` | `["pending","review","reviewing","waiting"]` | No | None |
| `gate.automated_review` | `string` | `"required"` | No | must be one of required, optional, off |
| `gate.ci_failure_action` | `string` | `"rework"` | No | must be one of skip, rework |
| `gate.ci_trigger_label` | `string` | `none` | No | None |
| `gate.ci_trigger_label_stagger_seconds` | `integer` | `none` | No | must be greater than 0 |
| `gate.kind` | `string` | `"command"` | No | must be one of command, human_review, artifact |
| `gate.require_automated_review` | `boolean` | `true` | No | None |
| `gate.required_status_checks` | `list<string>` | `[]` | No | None |
| `gate.run` | `string` | `"make check"` | No | None |
| `gate.security_audit` | `object` | `see child fields` | No | None |
| `gate.security_audit.block_on` | `list<string>` | `["p1","p2"]` | No | severities must be p1, p2, or p3 |
| `gate.security_audit.enabled` | `boolean` | `false` | No | requires tracker.kind github or github_local |
| `gate.security_audit.max_attempts` | `integer` | `3` | No | must be greater than 0 |
| `gate.security_audit.max_diff_bytes` | `integer` | `262144` | No | must be greater than 0 |
| `gate.security_audit.model` | `string` | `none` | No | None |
| `gate.security_audit.turn_timeout_ms` | `integer` | `1200000` | No | must be greater than 0 |
| `gate.transient_ci_retry_limit` | `integer` | `2` | No | must be greater than or equal to 0 |
| `gate.validator` | `object` | `see child fields` | No | None |
| `gate.validator.block_on` | `list<string>` | `["p1"]` | No | None |
| `gate.validator.enabled` | `boolean` | `false` | No | None |
| `gate.validator.max_attempts` | `integer` | `3` | No | must be greater than 0 |
| `gate.validator.max_inline_diff_bytes` | `integer` | `65536` | No | must be greater than or equal to 0 |
| `gate.validator.min_score` | `number` | `0.8` | No | must be greater than 0 and less than or equal to 1 |
| `gate.validator.model` | `string` | `none` | No | None |
| `gate.validator.turn_timeout_ms` | `integer` | `0` | No | must be greater than or equal to 0 |
| `hooks` | `object` | `see child fields` | No | None |
| `hooks.after_create` | `string` | `none` | No | None |
| `hooks.after_run` | `string` | `none` | No | None |
| `hooks.before_remove` | `string` | `none` | No | None |
| `hooks.before_run` | `string` | `none` | No | None |
| `hooks.shell` | `string` | `platform default shell` | No | None |
| `hooks.timeout_ms` | `integer` | `60000` | No | must be greater than 0 |
| `identity` | `object` | `see child fields` | No | None |
| `identity.assignee_required` | `boolean` | `false` | No | None |
| `identity.github_login` | `string` | `none` | No | None |
| `identity.name` | `string` | `none` | Conditional | must not be blank |
| `identity.owner_field` | `string` | `none` | No | must be blank when identity.ownership_mode is assignee |
| `identity.ownership_mode` | `string` | `none` | No | identity.owner_field must be blank when identity.ownership_mode is assignee<br>must be one of assignee, field |
| `intake` | `object` | `see child fields` | Conditional | tracker.repository is required for intake sources |
| `intake.sources` | `list<object>` | `[]` | No | requires tracker.kind github |
| `intake.sources[].creates` | `object` | `see child fields` | No | None |
| `intake.sources[].creates.body` | `string` | `"{details}" when configured` | No | None |
| `intake.sources[].creates.labels` | `list<string>` | `[]` | No | None |
| `intake.sources[].creates.status` | `string` | `"Backlog" when configured` | No | must name a configured tracker state |
| `intake.sources[].creates.title` | `string` | `"[{source}] {summary}" when configured` | No | None |
| `intake.sources[].cron` | `string` | `none` | No | None |
| `intake.sources[].dedupe_by` | `string` | `"fingerprint" when configured` | No | None |
| `intake.sources[].kind` | `string` | `none` | Conditional | is required |
| `intake.sources[].match` | `string` | `none` | No | must use field:value syntax |
| `intake.sources[].name` | `string` | `none` | Conditional | is required<br>must contain only lowercase letters, numbers, dots, underscores, or hyphens |
| `intake.sources[].scan` | `string` | `none` | No | None |
| `intake.sources[].secret` | `string` | `none` | Conditional | is required for webhook sources |
| `observability` | `object` | `see child fields` | No | None |
| `observability.dashboard_enabled` | `boolean` | `true` | No | None |
| `observability.dispatch_stall_threshold_seconds` | `integer` | `7200` | No | must be greater than 0 |
| `observability.efficiency` | `object` | `see child fields` | No | None |
| `observability.efficiency.anomaly_dwell_multiple` | `number` | `3` | No | must be greater than 0 |
| `observability.efficiency.anomaly_sessions_multiple` | `number` | `3` | No | must be greater than 0 |
| `observability.efficiency.anomaly_tokens_multiple` | `number` | `3` | No | must be greater than 0 |
| `observability.otlp` | `object` | `see child fields` | No | None |
| `observability.otlp.endpoint` | `string` | `none` | No | must be an absolute http or https URL |
| `observability.otlp.headers` | `mapping<string, string>` | `{}` | No | None |
| `observability.otlp.service_name` | `string` | `"detent"` | No | None |
| `observability.otlp.timeout_ms` | `integer` | `5000` | No | None |
| `observability.park_review_threshold` | `integer` | `3` | No | must be greater than 0 |
| `observability.refresh_ms` | `integer` | `1000` | No | must be greater than 0 |
| `observability.render_interval_ms` | `integer` | `16` | No | must be greater than 0 |
| `observability.staleness` | `object` | `see child fields` | No | None |
| `observability.staleness.enabled` | `boolean` | `true` | No | None |
| `observability.staleness.human_gate_rearm_hours` | `integer` | `168` | No | must be greater than 0 |
| `observability.staleness.lane_reentry_window_hours` | `integer` | `168` | No | must be greater than 0 |
| `observability.staleness.lanes` | `list<object>` | `[{"State":"Human Review","ThresholdHours":72,"HumanGate":true},{"State":"Blocked","ThresholdHours":48,"HumanGate":false},{"State":"Merging","ThresholdHours":2,"HumanGate":false}]` | No | None |
| `observability.staleness.lanes[].human_gate` | `boolean` | `true` | No | None |
| `observability.staleness.lanes[].state` | `string` | `"Human Review"` | Conditional | is required |
| `observability.staleness.lanes[].threshold_hours` | `integer` | `72` | No | must be greater than 0 |
| `observability.staleness.no_completion_hours` | `integer` | `24` | No | must be greater than 0 |
| `observability.staleness.no_merge_hours` | `integer` | `12` | No | must be greater than 0 |
| `observability.staleness.repeated_decision_benign_reasons` | `list<string>` | `["already_running","blocked_by_dependency","github_rest_capacity_paused","github_rest_recovery","global_capacity_full","outside_active_window","provider_rate_window_backpressure","reserved_for_higher_priority_project"]` | No | contains "__invalid__", which does not match a scheduler-emitted reason; emitted reasons include ["already_running" "authorization_selector_declined" "blocked_by_dependency" "github_rest_capacity_paused" "github_rest_recovery" "global_capacity_full" "outside_active_window" "provider_rate_window_backpressure" "reserved_for_higher_priority_project"]<br>contains "todo", which does not match a scheduler-emitted reason; emitted reasons include ["already_running" "authorization_selector_declined" "blocked_by_dependency" "github_rest_capacity_paused" "github_rest_recovery" "global_capacity_full" "outside_active_window" "provider_rate_window_backpressure" "reserved_for_higher_priority_project"] |
| `observability.staleness.repeated_decision_count` | `integer` | `20` | No | must be greater than 0 |
| `observability.staleness.repeated_window_hours` | `integer` | `24` | No | must be greater than 0 |
| `observability.staleness.webhook` | `object` | `see child fields` | No | None |
| `observability.staleness.webhook.headers` | `mapping<string, string>` | `{}` | No | None |
| `observability.staleness.webhook.timeout_ms` | `integer` | `5000` | No | None |
| `observability.staleness.webhook.url` | `string` | `none` | No | must be an absolute http or https URL |
| `observability.stranded_active_threshold_seconds` | `integer` | `600` | No | must be greater than 0 |
| `plan` | `object` | `see child fields` | No | agents.backends.options.permission_mode must not be plan for unattended workers |
| `plan.approval_label` | `string` | `"plan-approved"` | No | None |
| `plan.enabled` | `boolean` | `false` | No | None |
| `plan.review` | `string` | `"human"` | No | must be one of human, automated, both |
| `plan.stop` | `string` | `"Plan Review"` | No | None |
| `polling` | `object` | `see child fields` | No | None |
| `polling.conditional` | `boolean` | `true` | No | None |
| `polling.interval_ms` | `integer` | `120000` | No | must be at least 60000<br>must be greater than 0 |
| `polling.refresh_failure_threshold` | `integer` | `3` | No | must be greater than 0 |
| `release` | `object` | `see child fields` | No | None |
| `release.enabled` | `boolean` | `false` | No | release.max_age_hours must be greater than 0 when release.enabled is true<br>release.min_merged_issues must be greater than 0 when release.enabled is true<br>release.require_green_ci must be true when release.enabled is true<br>requires tracker.kind github or github_local<br>tracker.repository must be owner/name when release.enabled is true |
| `release.flaky_check_names` | `list<string>` | `[]` | No | must not be empty when release.rerun_flaky_once is true |
| `release.max_age_hours` | `integer` | `24` | No | must be greater than 0 when release.enabled is true |
| `release.min_merged_issues` | `integer` | `5` | No | must be greater than 0 when release.enabled is true |
| `release.require_green_ci` | `boolean` | `true` | No | must be true when release.enabled is true |
| `release.rerun_flaky_once` | `boolean` | `false` | No | release.flaky_check_names must not be empty when release.rerun_flaky_once is true |
| `release.version_bump` | `string` | `"auto"` | No | must be auto |
| `retro` | `object` | `see child fields` | No | None |
| `retro.allow_public_cross_project_details` | `boolean` | `false` | No | None |
| `retro.daily_issue_cap` | `integer` | `3 when configured` | No | must be greater than 0 |
| `retro.enabled` | `boolean` | `false` | No | None |
| `retro.fallback_threshold` | `integer` | `3 when configured` | No | must be at least 2 |
| `retro.labels` | `list<string>` | `["retro"] when configured` | No | None |
| `retro.lookback_days` | `integer` | `7 when configured` | No | must be greater than 0 |
| `retro.min_occurrences` | `integer` | `2 when configured` | No | must be at least 2 |
| `retro.product_repository` | `string` | `"digitaldrywood/detent" when configured` | No | must be owner/name |
| `retro.receipt_baseline_multiple` | `number` | `4 when configured` | No | must be greater than 1 |
| `retro.schedule` | `string` | `"0 3 * * *" when configured` | No | must be a valid five-field cron expression |
| `retro.single_occurrence_severity` | `string` | `"critical" when configured` | No | must be one of info, warning, high, critical |
| `retro.target_state` | `string` | `"Backlog" when configured` | No | must name a configured tracker state |
| `routines` | `list<object>` | `[]` | No | None |
| `routines[].labels` | `list<string>` | `[]` | Conditional | labels must not be blank |
| `routines[].max_findings_per_run` | `integer` | `3 when configured` | No | must be greater than 0 |
| `routines[].max_open_findings` | `integer` | `10 when configured` | No | must be greater than 0 |
| `routines[].name` | `string` | `none` | No | must be a sanitized label containing only letters, numbers, dots, underscores, or hyphens |
| `routines[].prompt` | `string` | `none` | Conditional | is required |
| `routines[].schedule` | `string` | `none` | Conditional | is required<br>must be a valid five-field cron expression |
| `routines[].target_state` | `string` | `"Todo" when configured` | No | must name a configured workflow state |
| `schedule_ownership` | `object` | `see child fields` | No | None |
| `schedule_ownership.backend` | `string` | `"github_ref"` | No | must be github_ref |
| `schedule_ownership.branch` | `string` | `"detent-schedule-coordination"` | No | must be a valid branch name |
| `schedule_ownership.enabled` | `boolean` | `false` | No | must be true when scheduled work is configured; add a schedule_ownership block with enabled: true and a shared key |
| `schedule_ownership.endpoint` | `string` | `none` | No | None |
| `schedule_ownership.heartbeat_seconds` | `integer` | `60` | No | must be greater than zero<br>must leave more than twice max_clock_skew_seconds before lease expiry |
| `schedule_ownership.key` | `string` | `none` | Conditional | is required when enabled is true |
| `schedule_ownership.lease_seconds` | `integer` | `300` | No | must be greater than zero |
| `schedule_ownership.max_clock_skew_seconds` | `integer` | `15` | No | must be less than half lease_seconds<br>must not be negative |
| `schedule_ownership.repository` | `string` | `none` | No | must use owner/name syntax |
| `schedule_ownership.retry_seconds` | `integer` | `15` | No | must be greater than zero |
| `server` | `object` | `see child fields` | No | None |
| `server.board_snapshot_stale_after_seconds` | `integer` | `900` | No | must be greater than 0 |
| `server.host` | `string` | `"127.0.0.1"` | No | is required |
| `server.kanban` | `object` | `see child fields` | No | None |
| `server.kanban.allowed_transitions` | `mapping<string, list<string>>` | `{}` | No | None |
| `server.kanban.issue_state_field_id` | `integer` | `0` | No | must be greater than 0 when set |
| `server.kanban.mode` | `string` | `"read_only"` | No | must be one of read_only, integration |
| `server.kanban.show_blocked_alerts` | `boolean` | `false` | No | None |
| `server.port` | `integer` | `none` | No | must be greater than or equal to 0 |
| `tracker` | `object` | `see child fields` | No | intake.sources[].creates.status must name a configured tracker state<br>retro.target_state must name a configured tracker state |
| `tracker.active_states` | `list<string>` | `["Todo","In Progress"]` | No | , tracker.observed_states, or tracker.terminal_states must include Blocked when agent.auto_promote.no_progress_limit is greater than 0<br>must include Rework when tracker.dependency_auto_unblock.enabled is true<br>must include tracker.blocked_recovery.target_state when tracker.blocked_recovery.enabled is true<br>state names must be unique<br>state names must not be blank |
| `tracker.api_key` | `string` | `none` | Conditional | is required for linear<br>or GitHub App credentials are required for github<br>or GitHub App credentials are required for github_local |
| `tracker.assignee` | `string` | `none` | No | None |
| `tracker.authorization` | `object` | `see child fields` | No | None |
| `tracker.authorization.and` | `list<mapping>` | `[]` | No | None |
| `tracker.authorization.assignee_in` | `list<string>` | `[]` | No | None |
| `tracker.authorization.author_in` | `list<string>` | `[]` | No | None |
| `tracker.authorization.fields` | `list<object>` | `[]` | No | None |
| `tracker.authorization.fields[].name` | `string` | `none` | Conditional | must not be blank |
| `tracker.authorization.fields[].value` | `string` | `none` | Conditional | must not be blank |
| `tracker.authorization.labels` | `object` | `see child fields` | No | None |
| `tracker.authorization.labels.exclude` | `list<string>` | `[]` | No | None |
| `tracker.authorization.labels.include` | `list<string>` | `[]` | No | None |
| `tracker.authorization.or` | `list<mapping>` | `[]` | No | None |
| `tracker.authorization.priority_in` | `list<integer>` | `[]` | No | values must be integers 1 through 4 |
| `tracker.auto_provision` | `boolean` | `true` | No | None |
| `tracker.blocked_recovery` | `object` | `see child fields` | No | None |
| `tracker.blocked_recovery.breaker_cooldown_seconds` | `integer` | `86400` | No | must be greater than or equal to 0 |
| `tracker.blocked_recovery.enabled` | `boolean` | `false` | Conditional | tracker.active_states must include tracker.blocked_recovery.target_state when tracker.blocked_recovery.enabled is true<br>tracker.blocked_recovery.reason_codes must not be empty when tracker.blocked_recovery.enabled is true<br>tracker.blocked_recovery.source_states must not be empty when tracker.blocked_recovery.enabled is true<br>tracker.blocked_recovery.target_state is required when tracker.blocked_recovery.enabled is true |
| `tracker.blocked_recovery.reason_codes` | `list<string>` | `["merge_conflict","stale_base","missing_current_head_ci"]` | No | must contain only merge_conflict, stale_base, missing_current_head_ci<br>must not be empty when tracker.blocked_recovery.enabled is true |
| `tracker.blocked_recovery.source_states` | `list<string>` | `["blocked"]` | No | must not be empty when tracker.blocked_recovery.enabled is true<br>state names must be unique<br>state names must not be blank |
| `tracker.blocked_recovery.target_state` | `string` | `"Rework"` | Conditional | is required when tracker.blocked_recovery.enabled is true<br>tracker.active_states must include tracker.blocked_recovery.target_state when tracker.blocked_recovery.enabled is true |
| `tracker.blocker_auto_promote` | `object` | `see child fields` | No | None |
| `tracker.blocker_auto_promote.blocker_states` | `list<string>` | `["backlog","blocked","human review"]` | No | state names must be unique<br>state names must not be blank |
| `tracker.blocker_auto_promote.enabled` | `boolean` | `false` | Conditional | tracker.blocker_auto_promote.target_state is required when tracker.blocker_auto_promote.enabled is true |
| `tracker.blocker_auto_promote.source_states` | `list<string>` | `[]` | No | state names must be unique<br>state names must not be blank |
| `tracker.blocker_auto_promote.target_state` | `string` | `"Todo"` | Conditional | is required when tracker.blocker_auto_promote.enabled is true |
| `tracker.claims` | `object` | `see child fields` | No | None |
| `tracker.claims.enabled` | `boolean` | `false` | Conditional | tracker.claims.heartbeat_seconds must be greater than 0 when tracker.claims.enabled is true<br>tracker.claims.lease_field must not be blank when tracker.claims.enabled is true<br>tracker.claims.ttl_seconds must be greater than 0 when tracker.claims.enabled is true |
| `tracker.claims.heartbeat_seconds` | `integer` | `0` | No | must be greater than 0 when tracker.claims.enabled is true |
| `tracker.claims.lease_field` | `string` | `none` | Conditional | must not be blank when tracker.claims.enabled is true |
| `tracker.claims.ttl_seconds` | `integer` | `0` | No | must be greater than 0 when tracker.claims.enabled is true |
| `tracker.dependency_auto_unblock` | `object` | `see child fields` | No | None |
| `tracker.dependency_auto_unblock.enabled` | `boolean` | `false` | Conditional | tracker.active_states must include Rework when tracker.dependency_auto_unblock.enabled is true<br>tracker.dependency_auto_unblock.source_states must not be empty when tracker.dependency_auto_unblock.enabled is true<br>tracker.dependency_auto_unblock.target_state is required when tracker.dependency_auto_unblock.enabled is true |
| `tracker.dependency_auto_unblock.readiness` | `string` | `"terminal_or_merged"` | No | must be one of terminal, terminal_or_merged |
| `tracker.dependency_auto_unblock.source_states` | `list<string>` | `["blocked"]` | No | must not be empty when tracker.dependency_auto_unblock.enabled is true<br>state names must be unique<br>state names must not be blank |
| `tracker.dependency_auto_unblock.target_state` | `string` | `"Todo"` | Conditional | is required when tracker.dependency_auto_unblock.enabled is true |
| `tracker.endpoint` | `string` | `"https://api.linear.app/graphql" for linear; "https://api.github.com/graphql" for github or github_local; unused otherwise` | No | None |
| `tracker.github_app_id` | `string` | `none` | Conditional | is required for github app |
| `tracker.github_app_installation_id` | `string` | `none` | Conditional | is required for github app |
| `tracker.github_app_private_key` | `string` | `none` | Conditional | or tracker.github_app_private_key_path is required for github app |
| `tracker.github_app_private_key_path` | `string` | `none` | Conditional | tracker.github_app_private_key or tracker.github_app_private_key_path is required for github app |
| `tracker.github_graphql_min_remaining_reserve` | `integer` | `1000` | No | must be greater than 0 |
| `tracker.github_graphql_warn_remaining` | `integer` | `500` | No | must be greater than 0 |
| `tracker.github_rest_debug_logging` | `boolean` | `false` | No | None |
| `tracker.github_rest_fanout_max_requests` | `integer` | `80` | No | must be greater than or equal to 0 |
| `tracker.github_rest_min_remaining_reserve` | `integer` | `1000` | No | must be greater than 0 |
| `tracker.github_status_source` | `string` | `"project_v2"` | No | must be omitted when tracker.kind is github_local; Detent stores workflow status in tracker.local_sqlite |
| `tracker.github_unstarted_check_threshold_seconds` | `integer` | `900` | No | must be greater than 0 |
| `tracker.github_webhook_secret` | `string` | `none` | No | None |
| `tracker.http_idle_conn_timeout_ms` | `integer` | `90000` | No | must be greater than 0 |
| `tracker.http_max_idle_conns` | `integer` | `100` | No | must be greater than 0 |
| `tracker.http_max_idle_conns_per_host` | `integer` | `32` | No | must be greater than 0 |
| `tracker.issues` | `list<object>` | `[]` | No | None |
| `tracker.issues[].assigned_to_worker` | `boolean` | `false when configured` | No | None |
| `tracker.issues[].assignee_id` | `string` | `none` | No | None |
| `tracker.issues[].assignees` | `list<string>` | `[]` | No | None |
| `tracker.issues[].author_association` | `string` | `none` | No | None |
| `tracker.issues[].author_id` | `string` | `none` | No | None |
| `tracker.issues[].blocked_by` | `list<object>` | `[]` | No | None |
| `tracker.issues[].blocked_by[].id` | `string` | `none` | No | None |
| `tracker.issues[].blocked_by[].identifier` | `string` | `none` | No | None |
| `tracker.issues[].blocked_by[].source` | `string` | `none` | No | None |
| `tracker.issues[].blocked_by[].state` | `string` | `none` | No | None |
| `tracker.issues[].blocker_reason` | `string` | `none` | No | None |
| `tracker.issues[].branch_name` | `string` | `none` | No | None |
| `tracker.issues[].child_issues` | `list<object>` | `[]` | No | None |
| `tracker.issues[].child_issues[].id` | `string` | `none` | No | None |
| `tracker.issues[].child_issues[].identifier` | `string` | `none` | No | None |
| `tracker.issues[].child_issues[].source` | `string` | `none` | No | None |
| `tracker.issues[].child_issues[].state` | `string` | `none` | No | None |
| `tracker.issues[].closed` | `boolean` | `false when configured` | No | None |
| `tracker.issues[].closed_reason` | `string` | `none` | No | None |
| `tracker.issues[].comments` | `list<object>` | `[]` | No | None |
| `tracker.issues[].comments[].author_authorized` | `boolean` | `false when configured` | No | None |
| `tracker.issues[].comments[].author_display_name` | `string` | `none` | No | None |
| `tracker.issues[].comments[].author_kind` | `string` | `none` | No | None |
| `tracker.issues[].comments[].author_login` | `string` | `none` | No | None |
| `tracker.issues[].comments[].backend` | `string` | `none` | No | None |
| `tracker.issues[].comments[].body` | `string` | `none` | No | None |
| `tracker.issues[].comments[].can_delete` | `boolean` | `false when configured` | No | None |
| `tracker.issues[].comments[].can_edit` | `boolean` | `false when configured` | No | None |
| `tracker.issues[].comments[].created_at` | `mapping` | `none` | No | None |
| `tracker.issues[].comments[].id` | `string` | `none` | No | None |
| `tracker.issues[].comments[].local` | `boolean` | `false when configured` | No | None |
| `tracker.issues[].comments[].target_type` | `string` | `none` | No | None |
| `tracker.issues[].comments[].updated_at` | `mapping` | `none` | No | None |
| `tracker.issues[].comments[].url` | `string` | `none` | No | None |
| `tracker.issues[].created_at` | `mapping` | `none` | No | None |
| `tracker.issues[].deliverable` | `object` | `none` | No | None |
| `tracker.issues[].deliverable.external_id` | `string` | `none` | No | None |
| `tracker.issues[].deliverable.kind` | `string` | `none` | No | None |
| `tracker.issues[].deliverable.metadata` | `mapping<string, string>` | `{}` | No | None |
| `tracker.issues[].deliverable.path` | `string` | `none` | No | None |
| `tracker.issues[].deliverable.review_url` | `string` | `none` | No | None |
| `tracker.issues[].deliverable.validation_status` | `string` | `none` | No | None |
| `tracker.issues[].description` | `string` | `none` | No | None |
| `tracker.issues[].field_updated_at` | `mapping<string, mapping>` | `{}` | No | None |
| `tracker.issues[].fields` | `mapping<string, string>` | `{}` | No | None |
| `tracker.issues[].id` | `string` | `none` | No | None |
| `tracker.issues[].identifier` | `string` | `none` | No | None |
| `tracker.issues[].labels` | `list<string>` | `[]` | No | None |
| `tracker.issues[].metadata` | `mapping<string, string>` | `{}` | No | None |
| `tracker.issues[].model_override` | `string` | `none` | No | None |
| `tracker.issues[].number` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pr_number` | `integer` | `none` | No | None |
| `tracker.issues[].pr_repository` | `string` | `none` | No | None |
| `tracker.issues[].priority` | `integer` | `none` | No | None |
| `tracker.issues[].priority_name` | `string` | `none` | No | None |
| `tracker.issues[].pull_request` | `object` | `none` | No | None |
| `tracker.issues[].pull_request.activity_at` | `mapping` | `none` | No | None |
| `tracker.issues[].pull_request.base_ref` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.base_sha` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.branch_name` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.check_run_count` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.checks` | `list<object>` | `[]` | No | None |
| `tracker.issues[].pull_request.checks[].conclusion` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.checks[].details_url` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.checks[].duration_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.checks[].id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.checks[].name` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.checks[].queue_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.checks[].status` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.checks[].workflow_run_id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.ci_duration_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.ci_queue_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.ci_status` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.codex_review_api_state` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.codex_review_body_severity` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.codex_review_findings` | `list<object>` | `[]` | No | None |
| `tracker.issues[].pull_request.codex_review_findings[].body` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.codex_review_findings[].line` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.codex_review_findings[].path` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.codex_review_findings[].url` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.codex_review_state` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.codex_review_submitted_at` | `mapping` | `none` | No | None |
| `tracker.issues[].pull_request.diff_fingerprint` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.draft` | `boolean` | `false when configured` | No | None |
| `tracker.issues[].pull_request.head_sha` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.hydration_degraded_reason` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.hydration_next_retry_at` | `mapping` | `none` | No | None |
| `tracker.issues[].pull_request.hydration_unavailable_reason` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.labels` | `list<string>` | `[]` | No | None |
| `tracker.issues[].pull_request.latest_codex_review_commit_sha` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.latest_codex_review_state` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.latest_codex_review_submitted_at` | `mapping` | `none` | No | None |
| `tracker.issues[].pull_request.merge_queue_entry` | `object` | `none` | No | None |
| `tracker.issues[].pull_request.merge_queue_entry.depth` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.merge_queue_entry.enqueued_at` | `mapping` | `none` | No | None |
| `tracker.issues[].pull_request.merge_queue_entry.estimated_time_to_merge_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.merge_queue_entry.id` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.merge_queue_entry.position` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.merge_queue_entry.state` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.merge_queue_entry.url` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.mergeable_state` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.node_id` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.number` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.required_check_failures` | `list<object>` | `[]` | No | None |
| `tracker.issues[].pull_request.required_check_failures[].conclusion` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.required_check_failures[].details_url` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.required_check_failures[].duration_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.required_check_failures[].id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.required_check_failures[].name` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.required_check_failures[].queue_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.required_check_failures[].status` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.required_check_failures[].workflow_run_id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.running_checks` | `list<string>` | `[]` | No | None |
| `tracker.issues[].pull_request.slow_checks` | `list<object>` | `[]` | No | None |
| `tracker.issues[].pull_request.slow_checks[].conclusion` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.slow_checks[].details_url` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.slow_checks[].duration_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.slow_checks[].id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.slow_checks[].name` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.slow_checks[].queue_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.slow_checks[].status` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.slow_checks[].workflow_run_id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.stale_successful_checks` | `list<object>` | `[]` | No | None |
| `tracker.issues[].pull_request.stale_successful_checks[].conclusion` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.stale_successful_checks[].details_url` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.stale_successful_checks[].duration_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.stale_successful_checks[].id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.stale_successful_checks[].name` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.stale_successful_checks[].queue_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.stale_successful_checks[].status` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.stale_successful_checks[].workflow_run_id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.state` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.status_context_count` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.transient_failed_checks` | `list<object>` | `[]` | No | None |
| `tracker.issues[].pull_request.transient_failed_checks[].conclusion` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.transient_failed_checks[].details_url` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.transient_failed_checks[].duration_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.transient_failed_checks[].id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.transient_failed_checks[].name` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.transient_failed_checks[].queue_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.transient_failed_checks[].status` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.transient_failed_checks[].workflow_run_id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.unstarted_check_count` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.unstarted_checks` | `list<object>` | `[]` | No | None |
| `tracker.issues[].pull_request.unstarted_checks[].conclusion` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.unstarted_checks[].details_url` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.unstarted_checks[].duration_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.unstarted_checks[].id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.unstarted_checks[].name` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.unstarted_checks[].queue_seconds` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.unstarted_checks[].status` | `string` | `none` | No | None |
| `tracker.issues[].pull_request.unstarted_checks[].workflow_run_id` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].pull_request.url` | `string` | `none` | No | None |
| `tracker.issues[].stage_updated_actor` | `object` | `see child fields` | No | None |
| `tracker.issues[].stage_updated_actor.kind` | `string` | `none` | No | None |
| `tracker.issues[].stage_updated_actor.login` | `string` | `none` | No | None |
| `tracker.issues[].stage_updated_at` | `mapping` | `none` | No | None |
| `tracker.issues[].state` | `string` | `none` | No | None |
| `tracker.issues[].title` | `string` | `none` | No | None |
| `tracker.issues[].unblocker_count` | `integer` | `0 when configured` | No | None |
| `tracker.issues[].updated_at` | `mapping` | `none` | No | None |
| `tracker.issues[].url` | `string` | `none` | No | None |
| `tracker.issues[].workpad_signal` | `mapping` | `none` | No | None |
| `tracker.kind` | `string` | `none` | Yes | backlog_admission.authors.allow_association requires author association, but tracker.kind memory cannot supply it<br>backlog_admission.sources.untracked requires candidate selector untracked, but tracker.kind memory does not provide github label status drift<br>gate.security_audit.enabled requires tracker.kind github or github_local<br>intake.sources requires tracker.kind github<br>is required<br>must be one of github, github_local, linear, memory, local_sqlite<br>release.enabled requires tracker.kind github or github_local<br>tracker.github_status_source must be omitted when tracker.kind is github_local; Detent stores workflow status in tracker.local_sqlite |
| `tracker.local_sqlite` | `object` | `see child fields` | No | tracker.github_status_source must be omitted when tracker.kind is github_local; Detent stores workflow status in tracker.local_sqlite |
| `tracker.local_sqlite.path` | `string` | `none` | Conditional | is required for local_sqlite |
| `tracker.local_sqlite.project_id` | `string` | `none` | No | None |
| `tracker.observed_states` | `list<string>` | `["Backlog","Human Review","Blocked"]` | No | agent.stop_run.target_state must be included in tracker.observed_states<br>state names must be unique<br>state names must not be blank<br>tracker.active_states, tracker.observed_states, or tracker.terminal_states must include Blocked when agent.auto_promote.no_progress_limit is greater than 0 |
| `tracker.priority_map` | `string or mapping` | `{"High":2,"Low":4,"Medium":3,"No priority":null,"Urgent":1}` | No | option names must not be blank<br>ranks must be integers 1 through 4 or null |
| `tracker.project_slug` | `string` | `none` | Conditional | is required for github project_v2<br>is required for linear |
| `tracker.repository` | `string` | `none` | Conditional | is required for github_local<br>is required for intake sources<br>must be owner/name when release.enabled is true |
| `tracker.state_map` | `string or mapping` | `{}` | No | state names must not be blank |
| `tracker.status_field` | `string` | `"Status"` | No | None |
| `tracker.status_label_prefix` | `string` | `"detent:"` | No | None |
| `tracker.status_page_url` | `string` | `"https://linearstatus.com" for linear; "https://www.githubstatus.com" for github or github_local; unused otherwise` | No | must be an absolute http or https base URL without credentials, path, query, or fragment |
| `tracker.terminal_states` | `list<string>` | `["Closed","Cancelled","Canceled","Duplicate","Done"]` | No | state names must be unique<br>state names must not be blank<br>tracker.active_states, tracker.observed_states, or tracker.terminal_states must include Blocked when agent.auto_promote.no_progress_limit is greater than 0 |
| `tracker.write_probe_issue` | `string` | `none` | No | None |
| `worker` | `object` | `see child fields` | No | None |
| `worker.github_rest_min_remaining_reserve` | `integer` | `1250` | No | must be greater than 0 |
| `worker.github_rest_poll_interval_ms` | `integer` | `60000` | No | must be greater than or equal to 60000 |
| `worker.github_token` | `string` | `none` | No | None |
| `worker.max_concurrent_agents_per_host` | `integer` | `none` | No | must be greater than 0 |
| `worker.ssh_hosts` | `list<string>` | `[]` | No | None |
| `workpad` | `object` | `see child fields` | No | None |
| `workpad.structured_only` | `boolean` | `false` | No | None |
| `workspace` | `object` | `see child fields` | No | agent.lessons.path must be a relative path inside the workspace<br>agent.skills.path must be a relative path inside the workspace |
| `workspace.auto_branch` | `boolean` | `true` | No | None |
| `workspace.cache_strategy` | `string` | `"isolated"` | No | must be one of isolated, shared |
| `workspace.cleanup_idle_ttl_ms` | `integer` | `86400000` | No | must be greater than 0 |
| `workspace.cleanup_sweep_interval_ms` | `integer` | `600000` | No | must be greater than 0 |
| `workspace.kind` | `string` | `"local_git"` | No | must be one of local_git, filesystem |
| `workspace.output_root` | `string` | `none` | No | None |
| `workspace.root` | `string` | `OS temporary directory + /detent_workspaces` | No | None |
| `workspace.source_root` | `string` | `none` | No | None |

<!-- END GENERATED CONFIG REFERENCE -->
