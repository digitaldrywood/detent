# Detent

[![CI](https://github.com/digitaldrywood/detent/actions/workflows/ci.yml/badge.svg)](https://github.com/digitaldrywood/detent/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/digitaldrywood/detent)](LICENSE)
[![Release](https://img.shields.io/github/v/release/digitaldrywood/detent?include_prereleases&sort=semver)](https://github.com/digitaldrywood/detent/releases)

A **detent** is the catch that holds a moving part at a fixed position until it
is deliberately released — the click-stop on a dial, the notch on a ratchet.
Detent holds each piece of work at a defined stop on your board and only lets it
advance when a gate is cleared.

## What is this

Detent is board-driven agentic work orchestration, shipped as a single Go
binary, with code as its first proven domain. Today it watches a GitHub Projects
board, and for every code issue you mark ready it creates an isolated Git
worktree, dispatches a Codex coding agent against a workflow contract you wrote,
runs your validation gate, opens a pull request, waits for review, and merges
through a serialized train — with all of it live on a web dashboard and a
terminal UI. The same board-to-gated-review-to-done shape is the trajectory for
non-code work, and [#266](https://github.com/digitaldrywood/detent/issues/266)
tracks the domain-agnostic execution seams needed to unlock it.

It is a **system, not an agent.** You specify the work — the issues, acceptance
criteria, review gates, and merge rules — and Detent runs that process with
rigor, isolation, and parallelism across many issues at once. The intelligence
stays in your spec; the runtime supplies the discipline.

**See it for real:**
[`digitaldrywood/detent-orchestration`](https://github.com/digitaldrywood/detent-orchestration)
is Detent's own production config — it dispatches the agents that build Detent
itself. Copy it as a template, and use
[Bootstrap On A New Machine](#bootstrap-on-a-new-machine-humans-and-ai-agents)
to go from a bare machine to a running board.

## How it works

The board is the state machine; board status drives everything.

1. **You write the contract.** Each project has a `WORKFLOW.md`: the tracker
   binding, board states, the agent prompt, the validation gate, and the review
   policy.
2. **You mark an issue `Todo`.** Detent claims it, creates an isolated Git
   worktree from your source checkout, and dispatches a Codex agent with the
   contract — moving the issue to `In Progress`.
3. **The agent works** in its own branch, runs your validation gate, and opens
   or updates a PR, then moves the issue to `Human Review`.
4. **Gates decide.** Your approval plus automated review (e.g. Codex) clear it
   to `Merging`; unresolved feedback sends it to `Rework` for another pass.
5. **The merge train is serialized** — one rebase, CI-watch, and merge at a
   time, so concurrent candidates never invalidate each other's CI — then the
   issue is `Done`.
6. **One host, many repos.** `global.yaml` runs multiple projects with weights,
   priority, pause, and fair scheduling. The web dashboard and terminal UI show
   live counts, running agents, token / budget / rate-limit state, and board
   flow.

The full state table, connector model, and merge-train config are in
[Concepts](#concepts).

## How it's different

### From OpenAI's Symphony

Detent grew out of [OpenAI's Symphony](https://github.com/openai/symphony) — the
open `SPEC.md` for orchestrating Codex coding agents from a project board instead
of supervising them interactively ("manage work, not agents"). Symphony ships as
a spec plus an Elixir reference implementation that polls a **Linear** board.
Detent takes that idea from spec to a shipped system, and diverges where it
counts:

- **A product, not a spec.** One CGO-free Go binary for macOS, Linux, and
  Windows — `go install`, Homebrew, or copy a single file. No BEAM service to
  adapt, nothing to stand up.
- **GitHub Projects v2, not Linear.** Issues, status columns, priorities,
  labels, blockers, comments, and pull requests are the state machine.
- **Multi-project from one host.** `global.yaml` runs many repositories with
  weights, priority, pause, and fair scheduling.
- **Explicit gates + a serialized merge train.** CI plus automated (Codex)
  review plus a one-at-a-time `Merging` lane, so what lands is always green.
- **A real operator surface.** A live dashboard (charts, trends, timelines,
  hover detail, budget and rate-limit state) and terminal UI, `detent doctor`
  preflight checks, cross-platform config discovery, and a GoReleaser pipeline.

### From autonomy-first agents (OpenClaw, Hermes, …)

The difference is the interaction model. Autonomy-first tools center a
persistent assistant: you talk to an agent, it keeps sessions and memory, picks
its own tools, and acts on your behalf — you steer it and course-correct when it
drifts. Detent inverts that. **You write the issue first** — scope, acceptance
criteria, tests, dependency order, review policy, merge rule — and the board
state decides when it is eligible. The runtime executes your contract in an
isolated worktree and will not land the work until the gates you encoded pass.
You are not steering an agent; you are running your own engineering process at
scale.

Concretely: "add OAuth token rotation" in an autonomy-first tool starts as a
prompt and becomes a supervision loop — review the plan, inspect partial edits,
redirect when it misses migrations or tests. In Detent it starts as an issue
that names the storage change, CLI behavior, migration, rollback, and tests; the
worker produces a reviewable PR and does not merge until your gates are green.

The goal is not to replace engineers or hide work behind opaque behavior — it is
to scale the judgment of engineers who already have a high bar. The system does
not try to be smarter than you; it tries to be as disciplined as you would be,
every time, in parallel.

## Install

Install the latest Windows release with PowerShell:

```powershell
irm https://raw.githubusercontent.com/digitaldrywood/detent/main/install.ps1 | iex
```

The PowerShell installer downloads the Windows release archive, verifies the
SHA-256 checksum, installs `detent.exe` to `%LOCALAPPDATA%\detent\bin`, and
adds that directory to the user PATH.

Install with Homebrew on macOS or Linux:

```sh
brew install digitaldrywood/tap/detent
```

Install with Go on any platform:

```sh
go install github.com/digitaldrywood/detent/cmd/detent@latest
```

Requirements:

- Go 1.26 or newer when installing with `go install` or building from source.
- The [OpenAI Codex CLI](https://github.com/openai/codex) installed and signed
  in, so `codex app-server` runs on the host that dispatches agents. Detent
  drives every agent through this app-server. Verify with `codex --version`.
- The [GitHub CLI](https://cli.github.com) (`gh`) for authentication and board
  lookups (optional but assumed throughout this guide).
- A GitHub token with access to the target ProjectV2 board. For organization
  projects, `repo`, `read:org`, and `project` scopes are usually required.

macOS and Linux hosts can install the latest release with the shell installer:

```sh
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh
```

Source checkouts can also run the repository-local shell installer:

```sh
./install.sh
```

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

Detent auto-provisions any missing `Status` and `Priority` options on the board
the first time it runs, so you do not have to hand-create every column — but the
option names it creates and reads must match the states in your `WORKFLOW.md`.

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
  root: /absolute/path/to/detent-workspaces
  source_root: /absolute/path/to/project-checkout
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

`workspace.source_root` is the checked-out repository Detent uses for
`git worktree add`; `workspace.root` is where per-issue worktrees are created.
If `workspace.source_root` is omitted, Detent falls back to the project
`workdir` from global config, then to `workspace.root` for older single-root
setups.

For production, self-hosted, or multi-instance GitHub Projects, prefer GitHub
App installation authentication instead of a shared personal access token. App
installation tokens have a dedicated GraphQL budget per installation and scale
with larger installations, while a PAT shares one fixed user budget across
Detent, agents, and operator `gh` calls. Configure the tracker with
`github_app_id`, `github_app_installation_id`, and either
`github_app_private_key` or `github_app_private_key_path`; keep `api_key` for
small local setups or one-off evaluation.

Default workflows do not need worktree setup hooks. Detent creates and removes
Git worktrees natively, so a fresh Windows project can dispatch without bash.
Omit `codex.shell` and `hooks.shell` to use the per-OS defaults: `sh` on Unix
and `cmd` on Windows. For portable hooks, prefer no hook when Detent already
does the setup natively. When a hook is necessary, keep it to commands available
on every target or set `hooks.shell: pwsh` and write PowerShell that reads
Detent values from `$env:DETENT_WORKSPACE`, `$env:DETENT_WORKSPACE_KEY`,
`$env:DETENT_BRANCH`, and `$env:DETENT_ISSUE_IDENTIFIER`.

4. Create the global config and add the project:

```sh
detent init
detent add-project \
  --id <id> \
  --workflow /absolute/path/to/project-checkout/WORKFLOW.md \
  --workdir /absolute/path/to/project-checkout
```

5. Verify the setup before dispatching:

```sh
detent --config ~/.detent/global.yaml doctor
```

`detent doctor` is a preflight check: config resolution, the SQLite database,
the `codex` binary, the GitHub token and scopes, the git binary, and whether the
server port is free. Fix any `FAIL` before starting (a missing `GITHUB_TOKEN` or
an unauthenticated `codex` are the usual culprits).

6. Start Detent:

```sh
detent --config ~/.detent/global.yaml
```

Open the dashboard at <http://localhost:4000>. Use `--host` and `--port` to
override the address:

```sh
detent --config ~/.detent/global.yaml --host 127.0.0.1 --port 4001
```

## Bootstrap On A New Machine (Humans And AI Agents)

A complete, ordered runbook to take a bare machine to a running Detent. Every
step has a verification command — do not proceed until it passes. An AI agent
can execute these steps top to bottom; replace each `<...>` placeholder. The
[`detent-orchestration`](https://github.com/digitaldrywood/detent-orchestration)
repo is a real, working instance of this setup to copy from.

1. **Install Detent.** `brew install digitaldrywood/tap/detent` (macOS/Linux),
   `go install github.com/digitaldrywood/detent/cmd/detent@latest`, or a
   platform installer from [Install](#install). Verify: `detent version`.

2. **Install and authenticate the GitHub CLI.** Install
   [`gh`](https://cli.github.com), then:

   ```sh
   gh auth login --scopes "repo,read:org,project"
   export GITHUB_TOKEN="$(gh auth token)"
   ```

   Verify: `gh auth status`.

3. **Install and sign in to the Codex CLI.** Install the
   [OpenAI Codex CLI](https://github.com/openai/codex) and sign in. Detent
   dispatches every agent through `codex app-server`. Verify: `codex --version`.

4. **Choose the GitHub ProjectV2 board** Detent will drive and get its node id
   (starts with `PVT_`):

   ```sh
   gh project list --owner <org-or-user> --format json --limit 50
   ```

   The board only needs to exist — Detent auto-provisions missing `Status` and
   `Priority` options on first run. The option names must match your
   `WORKFLOW.md` states.

5. **Clone the repository you want Detent to work on** (its checkout becomes
   `workspace.source_root`):

   ```sh
   git clone <repo-url> <source-root>
   ```

6. **Author the project contract.** Copy the canonical example as a starting
   point, then edit it:

   ```sh
   curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent-orchestration/main/WORKFLOW.md \
     -o <source-root>/WORKFLOW.md
   ```

   Set `tracker.project_slug` (your `PVT_` id), `workspace.source_root`
   (`<source-root>`), `workspace.root` (a worktrees directory), and the prompt
   body. The full field reference is in [Quick Start](#quick-start).

7. **Create global config and register the project:**

   ```sh
   detent init
   detent add-project --id <id> \
     --workflow <source-root>/WORKFLOW.md \
     --workdir <source-root>
   ```

8. **Verify everything:**

   ```sh
   detent --config ~/.detent/global.yaml doctor
   ```

   Every check must pass (the server-port check may fail if Detent is already
   running — that is expected).

9. **Start Detent and confirm the dashboard:**

   ```sh
   detent --config ~/.detent/global.yaml
   curl -fsS http://localhost:4000/api/v1/state    # returns JSON telemetry
   ```

10. **Dispatch work.** Move a board issue to `Todo`. Detent claims it, creates
    an isolated worktree, dispatches an agent, and the issue appears under
    Running on the dashboard. Drive the rest with the board (`Todo` →
    `In Progress` → `Human Review` → `Merging` → `Done`).

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

### Set up the board

You bring the board; Detent fills in the rest.

- **You create** a GitHub **Projects v2** board (org or user) and point
  `tracker.project_slug` at its node id — the `PVT_…` id from
  `gh project list --owner <org-or-user> --format json`. The board has a default
  **`Status`** field; add a **`Priority`** single-select if you rank work.
- **Detent auto-provisions** the *missing options* inside those fields on first
  run — the `Todo` / `In Progress` / `Rework` / `Merging` / `Done` columns above
  and the `Urgent`…`Low` priorities — so the option names always match your
  `WORKFLOW.md`. It provisions the options, not the board or the fields
  themselves, so create the board (and the `Priority` field if used) first.
- **Detent reads** status, priority, labels, blockers, assignees, and linked
  pull requests from each issue, and **writes back** status transitions and a
  `## Codex Workpad` comment as the agent works.

Before you dispatch anything, run **`detent doctor`** — it checks config
resolution, the database, the `codex` binary, your GitHub token and scopes, git,
and the server port. A clean `doctor` means you can move an issue to `Todo` and
watch it run.

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

These commands persist the global config. A running Detent process watches the
active `global.yaml` and reconciles project additions, removals, and
`global.startup` changes without a restart. Changes to
`global.max_concurrent_agents`, `global.scheduling`, and `global.fair_share`
require a restart before runtime concurrency and scheduling behavior changes.
Invalid edits are logged and ignored while the last valid config stays live.

## Dashboard And APIs

The web dashboard starts with the main `detent` command. In running mode it
shows live counts, running issues, retry queue, blocked work, completed
sessions, token totals, budget status, Codex rate-limit snapshots, and GitHub
GraphQL rate-limit snapshots when the GitHub connector reports them.

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

## History

Detent began as an Elixir/OTP implementation of
[OpenAI's Symphony](https://github.com/openai/symphony) — the open spec for
orchestrating Codex agents from a project board — adapted from Symphony's Linear
target to GitHub Projects v2. It is now a ground-up Go rewrite: one CGO-free
binary instead of a BEAM service, plus multi-project orchestration, the gated
merge train, a richer operator dashboard, `detent doctor`, Windows support, and
a GoReleaser pipeline. That earlier Elixir implementation is archived.

## License

Detent is released under the MIT license. See [LICENSE](LICENSE).
