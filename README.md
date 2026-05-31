# Symphony

[![CI](https://github.com/digitaldrywood/symphony/actions/workflows/ci.yml/badge.svg)](https://github.com/digitaldrywood/symphony/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/digitaldrywood/symphony)](LICENSE)
[![Release](https://img.shields.io/github/v/release/digitaldrywood/symphony?include_prereleases&sort=semver)](https://github.com/digitaldrywood/symphony/releases)

Symphony is an agent orchestrator for tracker-backed work queues, delivered as
a single Go binary. It polls a project board, creates isolated Git worktrees,
dispatches Codex agents, and exposes live status through a web dashboard and a
terminal status model.

Symphony is designed for self-hosted software work: the same queue that tracks
release-readiness issues can dispatch the agents that implement them.

The previous Elixir implementation is retained only as a cutover reference and
should remain archived after the Go repository is renamed to
`digitaldrywood/symphony`.

## Install

Install the latest released binary with Go:

```sh
go install github.com/digitaldrywood/symphony/cmd/symphony@latest
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

3. Create a `WORKFLOW.md` in the repository you want Symphony to work on:

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
    path: .symphony/skills
    max_skills_in_prompt: 50
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspaceWrite
    networkAccess: true
server:
  host: 127.0.0.1
  port: 4000
hooks:
  timeout_ms: 60000
---
You are working on {{ issue.identifier }}: {{ issue.title }}.

Read the issue description, follow repository instructions, keep changes
scoped to the issue, run the project validation gate, and prepare the work for
human review.
```

4. Create the global config and add the project:

```sh
symphony init
symphony add-project \
  --id <id> \
  --workflow /absolute/path/to/project-checkout/WORKFLOW.md \
  --workdir /absolute/path/to/project-checkout
```

5. Start Symphony:

```sh
symphony --config ~/.symphony/global.yaml
```

Open the dashboard at <http://localhost:4000>. Use `--host` and `--port` to
override the address:

```sh
symphony --config ~/.symphony/global.yaml --host 127.0.0.1 --port 4001
```

## Concepts

### Connectors

Symphony isolates tracker integration behind a connector interface. The current
production connector is GitHub Projects. A memory connector is available for
local development, and the connector boundary is where GitLab and Jira support
will land later.

GitHub configuration lives in each project's `WORKFLOW.md` frontmatter. The
`project_slug` value is the GitHub ProjectV2 node id. Symphony reads issue
state, priority, labels, blockers, and assignment from the board, then writes
comments and state transitions back through the connector.

### Board States

The recommended GitHub Project board states are:

| State | Meaning |
| --- | --- |
| `Backlog` | Not eligible for agents yet. |
| `Todo` | Ready for Symphony to claim and dispatch. |
| `In Progress` | An agent is actively working or continuing work. |
| `Blocked` | Symphony cannot continue without human action. |
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

Symphony separates host-level orchestration from per-project workflow:

- `~/.symphony/global.yaml` lists projects and host-level scheduling settings.
- Each project has its own `WORKFLOW.md` with tracker credentials, states,
  workspace rules, Codex settings, budgets, hooks, and agent instructions.

A minimal global config looks like this:

```yaml
apiVersion: symphony/v1
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
  - id: symphony
    workflow: /absolute/path/to/symphony/WORKFLOW.md
    workdir: /absolute/path/to/symphony
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
symphony --config ~/.symphony/global.yaml add-project \
  --id <id> \
  --workflow <WORKFLOW.md> \
  --workdir <dir> \
  --weight 1 \
  --priority 3

symphony --config ~/.symphony/global.yaml pause <id>
symphony --config ~/.symphony/global.yaml unpause <id>
symphony --config ~/.symphony/global.yaml promote <id> --priority 1
symphony --config ~/.symphony/global.yaml remove-project <id>
```

These commands persist the global config. Restart a foreground Symphony process
after structural edits unless your deployment wires the live manager signal path
into the same process.

## Dashboard And APIs

The web dashboard starts with the main `symphony` command. In running mode it
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

## Development

Common development commands:

```sh
make setup
make dev
make check
```

`make dev` runs Air with `SYMPHONY_ENV=dev` and
`SYMPHONY_LOG_LEVEL=debug`, builds `./tmp/symphony`, rotates
`tmp/air-combined.log`, and streams combined build and application output to
`tmp/air-combined.log`.

`make check` runs the local release gate: build, `golangci-lint`, `go vet`,
race tests, and the 70 percent coverage check. Run `make generate` before
committing changes to Templ templates, sqlc queries, or Tailwind inputs.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full contributor workflow.

## Logging

Symphony logs with `log/slog`.

- `SYMPHONY_ENV=dev`, `development`, or `local` enables tint text logs.
- `SYMPHONY_ENV=prod` or any other non-development value keeps JSON logs.
- When `SYMPHONY_ENV` is unset, interactive stdout TTY runs use tint text logs;
  non-TTY runs use JSON logs.
- `SYMPHONY_LOG_LEVEL` accepts `debug`, `info`, `warn`, `warning`, and `error`.
- Text logs are written to stdout; JSON logs are written to stderr.

## License

Symphony is released under the MIT license. See [LICENSE](LICENSE).
