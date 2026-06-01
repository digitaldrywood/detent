# Detent

[![CI](https://github.com/digitaldrywood/detent/actions/workflows/ci.yml/badge.svg)](https://github.com/digitaldrywood/detent/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/digitaldrywood/detent)](LICENSE)
[![Release](https://img.shields.io/github/v/release/digitaldrywood/detent?include_prereleases&sort=semver)](https://github.com/digitaldrywood/detent/releases)

Detent is an agent orchestrator for tracker-backed work queues, delivered as
a single Go binary. It polls a project board, creates isolated Git worktrees,
dispatches Codex agents, and exposes live status through a web dashboard and a
terminal status model.

Detent is designed for self-hosted software work: the same queue that tracks
release-readiness issues can dispatch the agents that implement them.

The previous Elixir implementation is retained only as a cutover reference and
should remain archived after the Go repository is renamed to
`digitaldrywood/detent`.

## Philosophy / Why

This project is a system, not an agent. Most agent tools are positioned around
autonomy: give the model a goal, give it tools, and let it discover a path.
This project takes the opposite bet. It is an execution system for engineers
who already know how the work should be done and want that process run
consistently across many issues at once.

The engineer authors the workflow: the `WORKFLOW.md` contract, the board
states, the issue breakdown, the dependency rules, the review gates, and the
merge discipline. The system executes that contract. The useful intelligence is
in the spec and in the engineer's judgment. The runtime supplies rigor,
parallelism, isolation, observability, and control.

That distinction matters because the unit of work is a well-specified issue,
not a freeform prompt. If the spec is vague, the result will be vague. That is
intentional. This project rewards the same habits that make engineering teams
effective without agents: small scopes, explicit acceptance criteria, tests,
reviewable diffs, green CI, and clean merges.

The original Elixir Detent was an OTP agent orchestrator for a single
project-backed engineering queue. It established the important shape:
tracker-backed dispatch, pluggable connector boundaries, explicit workflow
states, deterministic scheduling, dashboard visibility, and a terminal status
surface. This codebase credits that lineage, but it is not a runtime port. It
is a ground-up Go rewrite delivered as a single CGO-free binary for macOS,
Linux, and Windows, so there is no BEAM runtime to deploy. Distribution is
meant to stay simple: `go install`, Homebrew once the tap lands, or copying one
binary to a host.

The Go version has also moved beyond the original scope. A single host can run
many repositories from `global.yaml`, with project weights, priority, pause
controls, and fair scheduling. GitHub Projects v2 is the production board and
state machine: issues, status columns, priorities, labels, blockers, comments,
and pull requests drive dispatch. The live dashboard has grown into an
operator surface with charts, trends, timelines, hover detail, rate-limit
state, budget state, and session detail. The CLI includes `detent doctor`
for preflight checks, flexible config discovery across explicit flags,
environment variables, OS config paths, and legacy locations, plus a GoReleaser
pipeline for release archives and checksums.

Compared with autonomy-first frameworks such as OpenClaw or Hermes, the
difference is the interaction model. Those projects center a persistent
assistant experience: a user talks to an agent through a CLI or messaging
channel, the assistant keeps sessions or memory, selects tools or skills, and
acts on behalf of the user. That is useful for exploration and personal
automation. It is not the operating model here.

With an autonomy-first agent, you might say "add feature X", let the agent
decide how to decompose the work, watch the run, interrupt when it drifts, and
course-correct from whatever state it produced. You are steering an agent.

With this system, you write the issue first. You decide the scope, acceptance
criteria, expected tests, dependency ordering, review policy, CI gate, and
merge rule. The board state and priority determine when the work is eligible.
The runtime creates an isolated Git worktree, dispatches the agent with the
project contract, runs validation, opens or updates a PR, waits for human and
automated review gates, and serializes the merge train. You are running your
own engineering process at scale.

For the same task, the difference is concrete. In an autonomy-first workflow,
"add OAuth token rotation" starts as a prompt and becomes an interactive
supervision loop: review the plan, inspect partial edits, redirect when the
agent misses migration or test requirements, and decide when the result is good
enough. In this system, the task starts as an issue that names the storage
change, CLI behavior, migration expectations, rollback concerns, and tests.
The worker receives that issue, works in its own branch and worktree, produces
a reviewable PR, and does not land until the gates you encoded pass.

The goal is not to replace engineers or hide the work behind opaque agent
behavior. The goal is to scale the judgment of engineers who already have a
high bar. You stay in control of the workflow, the state, the review, and the
merge. The system does not try to be smarter than you; it tries to be as
disciplined as you would be, every time, in parallel.

## Install

Install the latest released binary with Go:

```sh
go install github.com/digitaldrywood/detent/cmd/detent@latest
```

Requirements:

- Go 1.26 or newer.
- A working `codex app-server` command on the host that runs agents.
- A GitHub token with access to the target ProjectV2 board. For organization
  projects, `repo`, `read:org`, and `project` scopes are usually required.

Source checkouts can also run the repository-local installer:

```sh
./install.sh
```

Homebrew support is tracked separately in #98.

## Release

Cut releases from `main` by pushing a semver tag:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

Tags matching `v*` trigger the release workflow, which runs GoReleaser and
publishes the GitHub Release archives and checksums.

## Quick Start

The quickest production-shaped setup is one GitHub ProjectV2 board and one
local repository checkout.

1. Authenticate GitHub access:

```sh
gh auth login --scopes "repo,read:org,project"
export GITHUB_TOKEN="$(gh auth token)"
```

2. Find the GitHub ProjectV2 node id. Use the `id` field, which starts with
   `PVT_`, as `tracker.project_slug`:

```sh
gh project list --owner <org-or-user> --format json --limit 20
```

3. Create a `WORKFLOW.md` in the repository you want Detent to work on:

```markdown
---
tracker:
  kind: github
  api_key: $GITHUB_TOKEN
  project_slug: PVT_replace_with_project_id
  active_states:
    - Todo
    - In Progress
    - Rework
    - Merging
  observed_states:
    - Backlog
    - Human Review
    - Blocked
  terminal_states:
    - Done
    - Cancelled
  state_map:
    Cancelled: Done
  priority_map:
    Urgent: 1
    High: 2
    Medium: 3
    Low: 4
    No priority: null
polling:
  interval_ms: 30000
workspace:
  root: /absolute/path/to/project-checkout
  auto_branch: true
agent:
  max_concurrent_agents: 5
  max_turns: 20
  max_retry_backoff_ms: 300000
  max_concurrent_agents_by_state:
    Merging: 1
  dispatch_priority_by_state:
    - Merging
    - Rework
    - In Progress
    - Todo
  auto_promote:
    enabled: false
    quiet_seconds: 600
    optout_label: requires-human-review
    allowed_issue_labels: []
  skills:
    enabled: true
    path: .detent/skills
    max_skills_in_prompt: 50
codex:
  command: codex app-server
  shell: sh
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspaceWrite
    networkAccess: true
server:
  host: 127.0.0.1
  port: 4000
hooks:
  shell: sh
  timeout_ms: 60000
---
You are working on {{ issue.identifier }}: {{ issue.title }}.

Read the issue description, follow repository instructions, keep changes
scoped to the issue, run the project validation gate, and prepare the work for
human review.
```

Omit `codex.shell` and `hooks.shell` to use the per-OS defaults: `sh` on Unix
and `cmd` on Windows. Set either field to `pwsh` or `powershell` when hooks or
the Codex app-server command should run through PowerShell.

4. Create the global config and add the project:

```sh
detent init
detent add-project \
  --id <id> \
  --workflow /absolute/path/to/project-checkout/WORKFLOW.md \
  --workdir /absolute/path/to/project-checkout
```

5. Start Detent:

```sh
detent --config ~/.detent/global.yaml
```

Open the dashboard at <http://localhost:4000>. Use `--host` and `--port` to
override the address:

```sh
detent --config ~/.detent/global.yaml --host 127.0.0.1 --port 4001
```

## Concepts

### Connectors

Detent isolates tracker integration behind a connector interface. The current
production connector is GitHub Projects. A memory connector is available for
local development, and the connector boundary is where GitLab and Jira support
will land later.

GitHub configuration lives in each project's `WORKFLOW.md` frontmatter. The
`project_slug` value is the GitHub ProjectV2 node id. Detent reads issue
state, priority, labels, blockers, and assignment from the board, then writes
comments and state transitions back through the connector.

### Board States

The recommended GitHub Project board states are:

| State | Meaning |
| --- | --- |
| `Backlog` | Not eligible for agents yet. |
| `Todo` | Ready for Detent to claim and dispatch. |
| `In Progress` | An agent is actively working or continuing work. |
| `Blocked` | Detent cannot continue without human action. |
| `Human Review` | The PR is ready for approval. |
| `Rework` | Human or bot feedback needs another agent pass. |
| `Merging` | Final rebase, validation, CI watch, and merge. |
| `Done` | Complete. |
| `Cancelled` | Terminal state mapped to `Done` in the default release flow. |

### Merge Train

`Merging` is intentionally serialized. Keep this in every production workflow:

```yaml
agent:
  max_concurrent_agents_by_state:
    Merging: 1
```

Do not cap `Todo`, `In Progress`, or `Rework` unless you have a specific
operational reason. Those states should share the global agent pool so workers
stay busy while merge candidates wait for CI or a clean base branch.

## Multi-Project Operation

Detent separates host-level orchestration from per-project workflow:

- `~/.detent/global.yaml` lists projects and host-level scheduling settings.
- Each project has its own `WORKFLOW.md` with tracker credentials, states,
  workspace rules, Codex settings, budgets, hooks, and agent instructions.

A minimal global config looks like this:

```yaml
apiVersion: detent/v1
kind: GlobalConfig
global:
  max_concurrent_agents: 8
  scheduling: weighted
  fair_share:
    half_life: 1h
  startup:
    jitter_seconds: 10
    max_spawn_per_second: 2
projects:
  - id: detent
    workflow: /absolute/path/to/detent/WORKFLOW.md
    workdir: /absolute/path/to/detent
    weight: 2
    priority: 1
  - id: website
    workflow: /absolute/path/to/website/WORKFLOW.md
    workdir: /absolute/path/to/website
    weight: 1
    priority: 3
    paused: true
```

Project weights are relative scheduling weights. Higher weights receive a
larger dispatch share in weighted and fair-share scheduling modes. Project
priority is a rank: `1` is highest, `4` is lowest, and `0` or an omitted value
means no explicit priority.

Use the project administration commands to edit `global.yaml`:

```sh
detent --config ~/.detent/global.yaml add-project \
  --id <id> \
  --workflow <WORKFLOW.md> \
  --workdir <dir> \
  --weight 1 \
  --priority 3

detent --config ~/.detent/global.yaml pause <id>
detent --config ~/.detent/global.yaml unpause <id>
detent --config ~/.detent/global.yaml promote <id> --priority 1
detent --config ~/.detent/global.yaml remove-project <id>
```

These commands persist the global config. Restart a foreground Detent process
after structural edits unless your deployment wires the live manager signal path
into the same process.

## Dashboard And APIs

The web dashboard starts with the main `detent` command. In running mode it
shows live counts, running issues, retry queue, blocked work, completed
sessions, token totals, budget status, and Codex rate-limit snapshots.

Useful endpoints:

| Route | Purpose |
| --- | --- |
| `/` | Web dashboard. |
| `/health` | Server health and configured dependency checks. |
| `/events` | Server-sent dashboard updates. |
| `/api/v1/state` | JSON telemetry snapshot. |
| `/api/v1/refresh` | Request an orchestrator refresh with `POST`. |
| `/api/v1/<issue>` | JSON detail for a running, retrying, or blocked issue. |

The terminal TUI renders the same telemetry snapshot model for terminal-first
operator surfaces. The default binary path starts the web dashboard; embedding
the TUI uses the `internal/tui` Bubble Tea model with a telemetry hub.

The standing Go-vs-Elixir parity checklist is maintained in
[docs/parity-audit.md](docs/parity-audit.md).

## Development

Common development commands:

```sh
make setup
make dev
make check
```

`make dev` runs Air with `DETENT_ENV=dev` and
`DETENT_LOG_LEVEL=debug`, builds `./tmp/detent`, rotates
`tmp/air-combined.log`, and streams combined build and application output to
`tmp/air-combined.log`.

`make check` runs the local release gate: build, `golangci-lint`, `go vet`,
race tests, and the 70 percent coverage check. Run `make generate` before
committing changes to Templ templates, sqlc queries, or Tailwind inputs.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full contributor workflow.

## Logging

Detent logs with `log/slog`.

- `DETENT_ENV=dev`, `development`, or `local` enables tint text logs.
- `DETENT_ENV=prod` or any other non-development value keeps JSON logs.
- When `DETENT_ENV` is unset, interactive stdout TTY runs use tint text logs;
  non-TTY runs use JSON logs.
- `DETENT_LOG_LEVEL` accepts `debug`, `info`, `warn`, `warning`, and `error`.
- Text logs are written to stdout; JSON logs are written to stderr.

## Configuration

At startup, Detent resolves `global.yaml` in this order. The first matching rule wins.

| Order | Rule | Path |
| --- | --- | --- |
| 1 | `--config <path>` | Direct file path from the CLI flag |
| 2 | `DETENT_CONFIG=<file>` | Direct file path from the environment |
| 3 | `DETENT_HOME=<dir>` | `<dir>/global.yaml` |
| 4 | `os.UserConfigDir()` | `<config-dir>/detent/global.yaml` |
| 5 | Legacy home config | `~/.detent/global.yaml` |

`os.UserConfigDir()` maps to `%AppData%\detent\global.yaml` on Windows, `~/Library/Application Support/detent/global.yaml` on macOS, and `~/.config/detent/global.yaml` on Linux while honoring `XDG_CONFIG_HOME`.

If no global config is found, Detent keeps the single-project fallback and looks for `WORKFLOW.md` in the current working directory. Use `detent config path` to print the resolved config path and the rule that selected it.

## License

Detent is released under the MIT license. See [LICENSE](LICENSE).
