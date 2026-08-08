# Multi-Project Operation

[Back to README](../README.md#documentation)

Detent separates host-level orchestration from per-project definitions:

- The resolved global config file lists projects and host-level scheduling settings.
- Each project has `detent.yaml` for Detent-owned machine policy and
  `WORKFLOW.md` for portable agent instructions. The `projects[].workflow`
  path remains the definition anchor; Detent resolves the other files beside
  it, including when that directory is an explicit external definition root.

A minimal global config looks like this:

```yaml
apiVersion: detent/v1
kind: GlobalConfig
env: prod
log_level: info
github_token: gh
api_token: detent_replace_with_random_secret
port: 4000
instance_name: buildbox
update:
  auto_check_enabled: false
  check_interval_hours: 24
  auto_apply_enabled: false
global:
  max_concurrent_agents: 8
  scheduling: weighted
  active_hours:
    timezone: America/Chicago
    windows:
      - Mon-Sun 22:00-06:00
  agent_pools:
    - name: code
      max_concurrent_agents: 5
      burst_to: 8
    - name: video
      max_concurrent_agents: 10
      burst_to: 15
      scheduling: round_robin
  fair_share:
    half_life: 1h
  startup:
    jitter_seconds: 10
    max_spawn_per_second: 2
    max_concurrent_starts: 4
projects:
  - id: detent
    pool: code
    workflow: /absolute/path/to/detent/WORKFLOW.md
    workdir: /absolute/path/to/detent
    color: "#1192e8"
    weight: 2
    priority: 1
    active_hours:
      timezone: America/Chicago
      windows:
        - Mon-Fri 22:00-06:00
        - Sat-Sun 00:00-24:00
  - id: website
    workflow: /absolute/path/to/website/WORKFLOW.md
    workdir: /absolute/path/to/website
    weight: 1
    priority: 3
    paused: true
    paused_reason: waiting for website#42
    paused_at: 2026-07-27T12:00:00Z
    paused_until_issue: digitaldrywood/website#42
```

Project weights are relative scheduling weights. Higher weights receive a
larger dispatch share in weighted and fair-share scheduling modes. Project
priority is a rank: `0` is highest and `4` is lowest.

`global.agent_pools` defines independent agent-capacity partitions. Each
project belongs to exactly one pool through `projects[].pool`. A project with
no `pool` uses the implicit `default` pool, whose capacity is
`global.max_concurrent_agents` and whose policy is `global.scheduling`.
`default` is reserved and cannot be declared in `agent_pools`.

Every named pool requires a unique non-empty `name` and a positive
`max_concurrent_agents`. Its optional `scheduling` accepts `weighted`,
`strict`, `round_robin`, or `fair_share`; when omitted it inherits
`global.scheduling`. `max_concurrent_agents` is the pool's guaranteed
capacity. Optional `burst_to` must be greater than or equal to that guarantee
and lets the pool borrow unused guaranteed capacity from sibling pools up to
the configured ceiling. Omitting `burst_to`, or setting it equal to
`max_concurrent_agents`, keeps the pool rigid.

The sum of active pool guarantees is the shared capacity available for
borrowing. A borrower never displaces a running agent. When a lender has ready
work below its guarantee, new borrowed dispatches stop until natural
completion returns enough capacity; dispatch is not preempted across pool
boundaries. Contending borrowers are served in first-request order, one
admission at a time. Project selection, scheduling history, and strict-mode
preemption remain local to each pool. A configuration without `agent_pools` or
project `pool` fields retains the previous single-pool behavior.

`detent doctor` reports the last seven days of capacity waits for each project,
annotated with its local-heavy or cloud-only workload class. It identifies the
largest observed constraint across pool capacity, the project's
`agent.max_concurrent_agents`, lane-specific
`agent.max_concurrent_agents_by_state`, worker-host capacity, and subscription
provider rate-window backpressure. Each finding names the matching lever.
Rate-window backpressure recommends no configuration change because raising a
configured cap cannot increase the effective provider-paced limit.
Because pool refusals are sampled, all constraint reasons are normalized to
one observation per five-minute interval before doctor selects the binding
constraint. Telemetry from a project's previous pool assignment is ignored.

A pool-bound project in a single-class pool is told to raise that pool's
capacity, never to split it. For an elastic pool this names `burst_to`, the
reachable ceiling; rigid pools name `max_concurrent_agents`. When mixed
workload classes share the implicit default pool and pool waits bind, doctor
preserves the initial `code` / `cloud` split recommendation: the code pool
keeps the current cap, the cloud pool gets a provider-tuned starting cap, and
the affected projects are printed as valid YAML. Configured pools are reported
but are never repartitioned automatically.

Doctor also checks capacity coherence without requiring telemetry. It warns
when member project caps cannot add up to a pool's declared capacity, when a
project cap exceeds its pool, or when an active work lane other than the
intentionally serialized `Merging` lane is capped below the project.

Preview or apply that exact recommendation with:

```sh
detent fix agent-pools --dry-run
detent fix agent-pools
# Explicit non-interactive confirmation:
detent fix agent-pools --yes
```

The fixer prints an additions-only diff, requires confirmation unless `--yes`
is supplied, preserves unrelated YAML keys, comments, and project ordering,
and writes `global.yaml` with mode `0600`. It is a no-op unless mixed workload
classes have binding default-pool waits. It also declines any config that
already declares `global.agent_pools`; changing an existing partition is an
operator decision. The accepted change is picked up by global-config hot
reload without a process restart.

Set optional `projects[].color` to an opaque CSS hex color in `#RGB` or
`#RRGGBB` form when a project needs a fixed visual marker. The sidebar,
project cards, and top-level multi-project Kanban board keep the project name
or ID visible and use color only as an additional compact marker. Projects
without a configured color receive a deterministic automatic color from a
curated categorical palette based on the project ID, so colors remain stable
across restarts and do not depend on project order. When there are more
projects than palette entries, Detent deterministically reuses palette colors;
labels and project IDs remain the primary identifiers.

Set `projects[].workflow_ref` only after the workflow file already exists at
that git ref, such as after the first `WORKFLOW.md` merge to `origin/main`.
When set, the workflow file is read from a git ref in the configured source
checkout instead of the checkout's working-tree copy. `workflow` may be an
absolute path under `workdir` or a repository relative path such as
`WORKFLOW.md`. When the ref advances, Detent reloads the workflow content from
that ref. `WORKFLOW.local.md` remains machine-local in the working tree and is
applied over the ref-backed shared file. When `workflow_ref` is omitted, Detent
keeps reading the working-tree shared file. If `workflow_ref` points at a ref
that does not contain the workflow file, `detent doctor` reports a load failure
for `<ref>:WORKFLOW.md`. For GitHub pull-request projects, doctor also warns
when `workflow_ref` is omitted and compares the checked-out branch and
`detent.yaml` with the source repository's default branch. For a configured
remote-tracking ref, doctor compares the local ref revision with its remote
counterpart. This freshness check uses `git ls-remote` but does not fetch;
fetch the ref so Detent can load the new revision when doctor reports it stale.

Use the project administration commands to edit `global.yaml`:

```sh
detent add-project \
  --id <id> \
  --workflow <WORKFLOW.md> \
  --workdir <dir> \
  --weight 1 \
  --priority 3

detent pause <id> \
  --reason "maintenance" \
  --until 2026-08-01T12:00:00Z
detent unpause <id>
detent resume <id> --for 2h
detent promote <id> --priority 1
detent remove-project <id>
```

These commands persist the global config. A running Detent process watches the
active `global.yaml`, including symlinked config targets, and reconciles
supported live-reload fields without a process restart. Invalid edits are
logged and ignored while the last valid config stays live.

`detent pause` requires `--reason` and accepts either `--until-issue <ref>` or
`--until <RFC3339 timestamp>`. Detent polls only the referenced tracker issue
for an issue-based pause and automatically writes the unpause to `global.yaml`
when the issue closes or the timestamp passes. The CLI records `paused_at` for
doctor diagnostics. Hand-edited legacy `paused: true` entries remain valid
without pause metadata or an automatic exit condition.

Paused projects do not run workflow watchers or periodic workflow reconciliation.
`detent unpause <id>` synchronously reloads the project's current `WORKFLOW.md`
before dispatch resumes, so edits made while paused take effect on unpause. If
the current workflow cannot be loaded or prepared, unpause returns the error and
the project remains paused.

`active_hours` limits new agent dispatches to recurring wall-clock windows. It
may be set as a `global.active_hours` default, overridden by
`projects[].active_hours`, or placed in the project's `detent.yaml`. Host-local
`global.yaml` policy wins over project configuration. Every configured policy
requires an IANA `timezone` and one or more windows in
`Mon-Sun HH:MM-HH:MM` form. Weekday ranges are inclusive, `00:00-24:00`
represents a full day, and a range such as `22:00-06:00` wraps into the next
morning.

The gate is evaluated as a span on every dispatch decision, so a restart inside
a window admits work immediately and a restart outside it stays idle. At window
close Detent drains: running agents continue, while new dispatches receive the
benign `outside_active_window` refusal reason. Active hours never change
`paused` or its metadata, and manual pause remains stronger than an open
window.

Window edges keep wall-clock meaning across daylight-saving changes. A
spring-forward gap can shorten an overnight window by an hour; a fall-back
repeat can lengthen it by an hour. Membership evaluation avoids missed or
duplicated start events in both cases. `detent doctor` shows the next opening
and closing in the configured timezone and UTC.

Use `detent resume <id> --for 2h` or `detent resume <id> --until <RFC3339>` for
a one-shot active-hours override. The timestamp is persisted in
`active_hours_override_until`, admits dispatch outside the recurring window,
and expires without another command. It does not clear a manual pause; unpause
the project first when both gates apply.

For projects whose workflow file is already present on the target branch, you
can include `--workflow-ref origin/main` during registration or add
`workflow_ref: origin/main` to the project entry later.

| Field | Reload behavior |
| --- | --- |
| Project list and project settings, including active hours and overrides | Live reload |
| Credentials: `github_token`, `trust_loopback_peer_read`, and project credentials | Live reload |
| `dashboard_access` mode, token, and write access | Live reload; token changes invalidate private dashboard sessions |
| `auth` | Restart required; persisted sessions remain valid until their configured expiry |
| `global.startup` | Live reload |
| `instance_name` | Live reload |
| `global.identity` | Live reload; project runtimes restart in-process and `/api/v1/state.instance.name` updates after the next telemetry snapshot |
| `global.active_hours` | Live reload at the next dispatch decision; running agents drain when a window closes |
| `global.max_concurrent_agents`, `global.scheduling`, `global.agent_pools`, `global.fair_share`, and project pool assignments | Live reload at the next dispatch decision; adding, removing, or lowering `burst_to` preserves active workers and drains to the new ceiling, and removed pools drain their active workers before retirement |
| `log_level` | Live reload |
| `port`, `env`, `log_max_size_bytes`, `log_max_backups` | Restart required |

When a changed field requires restart, Detent logs
`global config setting change requires restart` with the field name.

## Running Multiple Instances

Run more than one Detent instance when a single GitHub ProjectV2 board should
be split across independent workers. Each instance is a separate `detent`
process with its own `global.yaml`, process identity, authorization selector,
runtime database, listener address, and claim lease. The instances may point at
the same `tracker.project_slug`,
but their authorization selectors should be disjoint so each issue belongs to
one worker set before claiming begins.

Use `global.identity` for the process identity in multi-instance operation.
That identity is applied to every project in that `global.yaml` and overrides
workflow-level identity while the project is loaded from global config. A
workflow can still define top-level `identity` for single-project runs, but do
not put identity under a `projects` entry in `global.yaml`; project entries only
carry scheduling, paths, credentials, pause state, and authorization selectors.

```yaml
apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 4
  scheduling: weighted
  identity:
    name: detent-alpha
    github_login: detent-alpha
    ownership_mode: field
    owner_field: Detent Owner
projects:
  - id: detent-alpha
    workflow: /absolute/path/to/detent/WORKFLOW.md
    workdir: /absolute/path/to/detent
    weight: 1
    priority: 1
    authorization:
      labels:
        include:
          - scope:alpha
```

A second instance can use the same workflow and board with a different identity
and a non-overlapping selector:

```yaml
apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 4
  scheduling: weighted
  identity:
    name: detent-beta
    github_login: detent-beta
    ownership_mode: field
    owner_field: Detent Owner
projects:
  - id: detent-beta
    workflow: /absolute/path/to/detent/WORKFLOW.md
    workdir: /absolute/path/to/detent
    weight: 1
    priority: 1
    authorization:
      labels:
        include:
          - scope:beta
```

The selector schema is the same in `projects[].authorization` and
`tracker.authorization`: `assignee_in`, `author_in`, `priority_in`,
`labels.include`, `labels.exclude`, `fields`, `and`, and `or`.
`projects[].authorization` from `global.yaml` is combined with
`tracker.authorization` from `detent.yaml` as an `and`, so both selectors must
match. Use `@me` inside `assignee_in`, `author_in`, or field selector values to
match the current instance identity (`github_login` and `name`). For example,
one common pattern is a global project selector for a broad lane label and a
workflow selector for a board field:

```yaml
tracker:
  authorization:
    fields:
      - name: Workstream
        value: engineering
```

Authorization only decides which issues an instance is allowed to consider.
Claiming is the final concurrent-dispatch guard. Enable it in the shared
workflow so all instances use the same lease field and TTL:

```yaml
tracker:
  claims:
    enabled: true
    lease_field: Detent Lease
    ttl_seconds: 900
    heartbeat_seconds: 120
```

When claims are enabled, Detent writes ownership first, then writes
`lease_field` with a UTC RFC3339 timestamp, refetches the issue, and dispatches
only if the refreshed owner and lease still match the current instance. With
`ownership_mode: assignee`, ownership is the GitHub assignee and `owner_field`
must be omitted. With `ownership_mode: field`, ownership is written to
`identity.owner_field`, which must exist on the board. While another owner has
a fresh lease, the issue is skipped. When the lease timestamp is stale by
`ttl_seconds` or missing, another matching instance may reclaim it. Detent
refreshes running claim leases every `heartbeat_seconds`; that value must be
greater than zero and less than or equal to `ttl_seconds`.

Task-to-model routing also lives in `detent.yaml`. If `agents.backends` is
omitted, routes can reference the legacy `codex` backend built from the top-level
`codex` block. Routes are evaluated in order, skipping defaults first; the first
non-default selector match wins, then the single `default` route is used. A
route can set a fixed `model`, read a model from a ProjectV2 field with
`model_field`, or fall back to an issue model override when neither is set.
Routes without `role` are code-agent routes. `Runner.Run` dispatches plan mode
with `role: plan`, Rework-state issues with `role: rework`, Merging-state
issues with `role: merge`, and all other implementation dispatches with
`role: code`. Set `role: validator` to give the validator-agent review its own
backend/model route when `gate.validator.enabled` is true. If a stage-specific
route does not match, Detent falls back to that role's default route and then to
the code default route, preserving the zero-config behavior.
If the validator runs through the Codex backend, prefer setting
`gate.validator.model: gpt-5.4-mini` as the cheap-tier override before adding a
separate validator route. Treat rework-rate per validator model as the quality
signal once cache/model telemetry lands; increase the validator tier only when
that rate worsens.

```yaml
agents:
  routes:
    - name: plan-cheap
      role: plan
      backend: codex
      model: gpt-5.4-mini
    - name: rework-high-context
      role: rework
      backend: codex
      model: gpt-5-codex-high
    - name: merge-standard
      role: merge
      backend: codex
      model: gpt-5-codex
    - name: high-context
      backend: codex
      model: gpt-5-codex-high
      selector:
        labels:
          include:
            - model:high
    - name: board-model
      backend: codex
      model_field: Model
    - name: default
      backend: codex
      model: gpt-5-codex
      default: true
```

For explicit backend profiles, configure `agents.backends` and route to those
ids. Supported backend kinds are `codex` with `protocol: app-server` and
`claude_code` with `protocol: headless`. Codex backend `options` use the same
runtime fields as the top-level `codex` block, including `shell`,
`approval_policy`, `thread_sandbox`, `turn_sandbox_policy`, `turn_timeout_ms`,
`read_timeout_ms`, and `stall_timeout_ms`. `agent.max_turns`,
`agent.max_turn_duration_ms`, `agent.max_session_duration_ms`, and
`agent.no_progress_timeout_ms` apply across backends rather than belonging to
an individual backend profile. Claude Code backend `options`
include `permission_mode`, `allowed_tools`, `disallowed_tools`,
`include_partial_messages`, `turn_timeout_ms`, `stall_timeout_ms`, `shell`, and
`extra_args`. When a Codex backend needs different configuration, launch
`codex app-server` with a dedicated `CODEX_HOME` or `-c` overrides. When a
Claude Code backend needs isolated state, launch `claude` with a dedicated
`CLAUDE_CONFIG_DIR`.

```yaml
agents:
  backends:
    - id: codex-standard
      kind: codex
      protocol: app-server
      command: codex app-server
    - id: codex-high
      kind: codex
      protocol: app-server
      command: env CODEX_HOME=/opt/detent/codex-high codex app-server
    - id: claude-worker
      kind: claude_code
      protocol: headless
      command: env CLAUDE_CONFIG_DIR=/var/lib/detent/claude/worker-1 claude
      options:
        permission_mode: bypassPermissions
        allowed_tools:
          - Bash
          - Edit
        disallowed_tools:
          - WebFetch
        extra_args:
          - --no-session-persistence
  routes:
    - name: validator
      role: validator
      backend: claude-worker
      model: fable
    - name: high-label
      backend: codex-high
      model: gpt-5-codex-high
      selector:
        labels:
          include:
            - model:high
    - name: default
      backend: codex-standard
      model: gpt-5-codex
      default: true
```

Claude Code auth is ambient and the backend is auth-agnostic. A logged-in
`claude` CLI uses the operator's subscription login. Setting
`ANTHROPIC_API_KEY` in the Detent worker environment switches the same backend
to API billing, and that key takes precedence over the subscription login.
Detent stores no Anthropic keys, mirroring the way Codex credentials stay
outside Detent.

Claude Pro/Max subscription limits are opaque in headless `claude -p` mode.
The 5-hour windows and weekly caps do not expose an in-band "limit approaching"
signal; a cap hit appears only as an error result from the turn. Use
subscription auth for bounded or bursty personal operation. Use
`ANTHROPIC_API_KEY` for sustained or parallel fleet runs where predictable
billing and capacity matter.

For fleet isolation, set a distinct `CLAUDE_CONFIG_DIR` per worker process so
concurrent `claude` invocations do not race on config or session state. Add
`--no-session-persistence` through `options.extra_args` when workers do not
need Claude Code session continuity; otherwise sessions accumulate under
`~/.claude/projects/`.

The sandbox model differs by backend. Codex runs turns under an OS-level
`workspace-write` sandbox. Claude Code headless runs inside Detent's isolated
git worktree with `permission_mode: bypassPermissions`, but that is not an OS
sandbox: allowed shell tools can still access the host as the Detent worker
user. Treat the worktree as the checkout boundary, use container, VM, or OS
sandbox isolation when you need a hard blast-radius boundary, and tighten role
exposure with `allowed_tools` plus `disallowed_tools`. Choose backend routes
with that trade-off in mind, especially for roles that can execute shell
commands or edit files.

For local Anthropic-compatible inference, point `ANTHROPIC_BASE_URL` at a local
server such as Ollama, which has native Anthropic API compatibility as of
January 2026, and keep using the `claude_code` backend. See
[Local Models With Codex And Ollama](local-models-ollama.md) for the
model sizing and context-window checks that also apply when evaluating local
agent backends.

The dashboard and `/api/v1/state` surface each instance identity, authorization
scope, owner, lease renewal time, lease expiry, and selected model usage, which
lets operators verify that scoped instances are not contending for the same
work.
