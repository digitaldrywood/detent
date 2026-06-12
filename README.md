<p align="center"><img src="docs/brand/detent-mark.svg" width="88" height="88" alt="Detent"></p>

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
non-code work: validation gates are now pluggable, while non-git or non-PR
deliverables remain follow-up work described in
[Execution Seams](docs/execution-seams.md).

It is a **system, not an agent.** You specify the work — the issues, acceptance
criteria, review gates, and merge rules — and Detent runs that process with
rigor, isolation, and parallelism across many issues at once. The intelligence
stays in your spec; the runtime supplies the discipline.

**See it for real:**
[`digitaldrywood/detent-orchestration`](https://github.com/digitaldrywood/detent-orchestration)
is Detent's own production config — it dispatches the agents that build Detent
itself. Copy it as a template, and use
[Bootstrap On A New Machine](#bootstrap-on-a-new-machine-humans-and-ai-agents)
to go from a bare machine to a running board. To onboard a new repository from
no board, no `WORKFLOW.md`, and no `global.yaml` entry to its first dispatched
issue, use the agent-executable [Project Onboarding](docs/ONBOARDING.md)
runbook.

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

See [How Detent compares to Symphony, Copilot, Cursor, Hermes, and OpenClaw](docs/comparison.md).

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
- **Pluggable validation gates.** Code defaults use `make check`, CI, and
  automated review, while workflow authors can choose a command gate or a human
  approval-label gate for files-in-repo work.
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

After installing, check for updates with:

```sh
detent update --check
```

Release-installer installs can update with `detent update`; use
`detent update --yes` for non-interactive automation and `detent update --json`
for machine-readable status. On Windows, replacement is staged and completes
after the running `detent.exe` exits. Homebrew installs delegate to
`brew upgrade digitaldrywood/tap/detent`. Go-installed binaries offer an
interactive choice: run
`go install github.com/digitaldrywood/detent/cmd/detent@latest`, switch to the
checksum-verified release binary, or abort. `detent update --yes` runs the Go
install command for go-installed binaries; `detent update --from-release`
switches the detected Go-installed binary to the release asset and pins future
updates to release-binary management. Source builds still print the recommended
command instead of overwriting the binary.

Release self-updates verify SHA256 checksums fetched from GitHub releases. The
checksum verifier supports detached minisign signature assets named
`<checksum>.minisig`, but enforcement is gated until the binary embeds the
pinned minisign public key for the release stream. Until that release signing
key is provisioned in #337, update integrity still depends on GitHub TLS plus
the published checksum file.

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
```

2. Find the GitHub ProjectV2 node id. Use the `id` field, which starts with
   `PVT_`, as `tracker.project_slug`:

```sh
gh project list --owner <org-or-user> --format json --limit 20
```

Detent auto-provisions any missing `Status` and `Priority` options on the board
the first time it runs, so you do not have to hand-create every column — but the
option names it creates and reads must match the states in your `WORKFLOW.md`.
GitHub uses single-select option order as board column order; Detent keeps the
known status options in canonical board order and leaves extra custom options
after the required Detent states.

3. Create a `WORKFLOW.md` in the repository you want Detent to work on:

```markdown
---
tracker:
  kind: github
  project_slug: PVT_replace_with_project_id
  http_max_idle_conns: 100
  http_max_idle_conns_per_host: 32
  http_idle_conn_timeout_ms: 90000
  github_graphql_warn_remaining: 500
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
  interval_ms: 120000
workspace:
  root: /absolute/path/to/detent-workspaces
  source_root: /absolute/path/to/project-checkout
  auto_branch: true
  cleanup_idle_ttl_ms: 86400000
  cleanup_sweep_interval_ms: 600000
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
gate:
  kind: command
  run: make check
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

`workspace.cleanup_idle_ttl_ms` controls how long non-active observed
workspaces can sit idle before cleanup. Terminal issues are cleaned immediately
when observed. `workspace.cleanup_sweep_interval_ms` controls the startup and
periodic cleanup cadence.

`polling.interval_ms` defaults to `120000` and must be at least `60000`.
Detent work is async, so it does not need sub-minute board scans. Detent polls
GitHub GraphQL, where board scans consume a shared rate-limit budget used by
Detent, spawned agents, and operator `gh` calls. Faster polling risks exhausting
that budget. This is an intentional divergence from Symphony's `30000` ms
default because Symphony polls Linear, which has a different rate model.

`gate` controls the validation contract the agent and operator flow follow.
Omitting it preserves the code default: `kind: command` with `run: make check`,
plus green CI and clean automated review before promotion. Use
`kind: human_review` with `approval_label` when the gate is a human label rather
than a command.

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
Detent values from `$env:WORKSPACE`, `$env:WORKSPACE_KEY`, `$env:BRANCH`,
and `$env:ISSUE_IDENTIFIER`. The older `DETENT_*` hook variables remain
available as deprecated aliases for one release.

4. Create the global config and add the project:

```sh
detent init
detent add-project \
  --id <id> \
  --workflow /absolute/path/to/project-checkout/WORKFLOW.md \
  --workdir /absolute/path/to/project-checkout
```

Edit the resolved `global.yaml` and set the top-level runtime keys:

```yaml
env: prod
log_level: info
github_token: gh
port: 4000
```

5. Verify the setup before dispatching:

   ```sh
   detent doctor
   ```

`detent doctor` is a preflight check: config resolution, the SQLite database,
the `codex` binary, the GitHub token and scopes, the git binary, and whether the
server port is free. Fix any `FAIL` before starting (missing `github_token: gh`
or an unauthenticated `codex` are the usual culprits).

6. Start Detent:

```sh
detent
```

Open the dashboard at <http://localhost:4000>. Use `--host` and `--port` to
override the address:

```sh
detent --host 127.0.0.1 --port 4001
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
   ```

   Verify: `gh auth status`. Use `github_token: gh` in `global.yaml` so
   Detent resolves this token at startup.

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
   `WORKFLOW.md` states, and Detent keeps known `Status` options in canonical
   board order.

5. **Clone the repository you want Detent to work on** (its checkout becomes
   `workspace.source_root`):

   ```sh
   git clone <repo-url> <source-root>
   ```

6. **Author the project contract.** Copy the canonical example as a starting
   point, then edit it. For from-zero board creation, interview questions, issue
   intake, and the first-dispatch smoke test, follow
   [Project Onboarding](docs/ONBOARDING.md):

   ```sh
   curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent-orchestration/main/WORKFLOW.md \
     -o <source-root>/WORKFLOW.md
   ```

   Set `tracker.project_slug` (your `PVT_` id), `workspace.source_root`
   (`<source-root>`), `workspace.root` (a worktrees directory), and the prompt
   body. The full field reference is in [Quick Start](#quick-start).

   Interactive alternative: when Detent starts without a resolved `global.yaml`
   and without a `WORKFLOW.md` in the current directory, it serves the
   `/onboarding` web wizard. Open `http://localhost:<port>/onboarding` to walk
   through tracker, credentials, project, agent, and write steps for generating
   `WORKFLOW.md`; then return to the runbook for board creation, global
   registration, issue intake, and the smoke test.

7. **Create global config and register the project:**

   ```sh
   detent init
   detent add-project --id <id> \
     --workflow <source-root>/WORKFLOW.md \
     --workdir <source-root>
   ```

   Edit the resolved `global.yaml` and set `github_token: gh` with any desired
   `env`, `log_level`, and `port` overrides.

8. **Verify everything:**

   ```sh
   detent doctor
   ```

   Every check must pass (the server-port check may fail if Detent is already
   running — that is expected).

9. **Start Detent and confirm the dashboard:**

   ```sh
   detent
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

The GitHub connector uses one pooled keep-alive HTTP client for GraphQL and
GitHub App REST token requests. Tune `tracker.http_max_idle_conns`,
`tracker.http_max_idle_conns_per_host`, and
`tracker.http_idle_conn_timeout_ms` when many Detent instances share one host.
Keep host-level agent concurrency within the machine's shared outbound
connection and ephemeral-port budget; the connector logs its live connection
count on GitHub requests to help spot pressure. For shared-board operation, see
[Running Multiple Instances](#running-multiple-instances).

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
  `WORKFLOW.md`. It also reorders the known `Status` options to Detent's
  canonical column order: `Backlog`, `Todo`, `In Progress`, `Blocked`,
  `Human Review`, `Rework`, `Merging`, then terminal states. Extra custom
  status options are preserved after the configured Detent states. It provisions
  the options, not the board or the fields themselves, so create the board (and
  the `Priority` field if used) first.
- **Blank `Status` values are not `Backlog`.** In the current release, an issue
  with no Project `Status` value is not dispatchable through the board state
  machine. Put unready work in the `Backlog` option explicitly.
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

- The resolved global config file lists projects and host-level scheduling settings.
- Each project has its own `WORKFLOW.md` with tracker credentials, states,
  workspace rules, Codex settings, budgets, hooks, and agent instructions.

A minimal global config looks like this:

```yaml
apiVersion: detent/v1
kind: GlobalConfig
env: prod
log_level: info
github_token: gh
port: 4000
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
detent add-project \
  --id <id> \
  --workflow <WORKFLOW.md> \
  --workdir <dir> \
  --weight 1 \
  --priority 3

detent pause <id>
detent unpause <id>
detent promote <id> --priority 1
detent remove-project <id>
```

These commands persist the global config. A running Detent process watches the
active `global.yaml` and reconciles project additions, removals, and
`global.startup` changes without a restart. Changes to
`global.max_concurrent_agents`, `global.scheduling`, and `global.fair_share`
require a restart before runtime concurrency and scheduling behavior changes.
Invalid edits are logged and ignored while the last valid config stays live.

## Running Multiple Instances

Run more than one Detent instance when a single GitHub ProjectV2 board should
be split across independent workers. Each instance is a separate `detent`
process with its own `global.yaml`, process identity, authorization selector,
and claim lease. The instances may point at the same `tracker.project_slug`,
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
`tracker.authorization` from `WORKFLOW.md` as an `and`, so both selectors must
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

Task-to-model routing also lives in `WORKFLOW.md`. If `agents.backends` is
omitted, routes can reference the legacy `codex` backend built from the top-level
`codex` block. Routes are evaluated in order, skipping defaults first; the first
non-default selector match wins, then the single `default` route is used. A
route can set a fixed `model`, read a model from a ProjectV2 field with
`model_field`, or fall back to an issue model override when neither is set.

```yaml
agents:
  routes:
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
ids. Today the shipped backend kind is `codex` with `protocol: app-server`.
Backend `options` use the same runtime fields as the top-level `codex` block,
including `shell`, `approval_policy`, `thread_sandbox`,
`turn_sandbox_policy`, `turn_timeout_ms`, and `read_timeout_ms`.

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
      command: codex app-server --profile high
  routes:
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

The dashboard and `/api/v1/state` surface each instance identity, authorization
scope, owner, lease renewal time, lease expiry, and selected model usage, which
lets operators verify that scoped instances are not contending for the same
work.

## Dashboard And APIs

The web dashboard starts with the main `detent` command. In running mode it
shows live counts, running issues, retry queue, blocked work, completed
sessions, token totals, budget status, Codex rate-limit snapshots, and GitHub
GraphQL rate-limit snapshots with per-cycle query cost contributors when the
GitHub connector reports them.

Useful endpoints:

| Route | Purpose |
| --- | --- |
| `/` | Web dashboard. |
| `/health` | Server health and configured dependency checks. |
| `/events` | Server-sent dashboard updates. |
| `/api/v1/state` | JSON telemetry snapshot. |
| `/api/v1/timeseries?window=10m&bucket=1m` | Fleet chart samples for running agents, tokens/sec, and completions. |
| `/api/v1/projects/<id>/state` | Project-scoped JSON telemetry snapshot. |
| `/api/v1/projects/<id>/timeseries?window=10m&bucket=1m` | Project chart samples for running agents, token spend, and board flow. |
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

`make dev` runs Air with `ENV=dev` and
`LOG_LEVEL=debug`, builds a `dev`-versioned `./tmp/detent` with the current
commit SHA and build date, rotates
`tmp/air-combined.log`, and streams combined build and application output to
`tmp/air-combined.log`.

`make check` runs the local release gate: build, `golangci-lint`, `go vet`,
race tests, and the 70 percent coverage check. Run `make generate` before
committing changes to Templ templates, sqlc queries, or Tailwind inputs.

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full contributor workflow.

## Logging

Detent logs with `log/slog`.

- `ENV=dev`, `development`, or `local` enables tint text logs.
- `ENV=prod` or any other non-development value keeps JSON logs.
- When no environment is configured, Detent defaults to `prod`.
- `LOG_LEVEL` accepts `debug`, `info`, `warn`, `warning`, and `error`.
- `--env` and `--log-level` override environment variables for one run.
- `DETENT_ENV` and `DETENT_LOG_LEVEL` remain deprecated fallbacks for one release. The unprefixed names win when both are set.
- Text logs are written to stdout; JSON logs are written to stderr.

## Configuration

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
| GitHub token | | `GITHUB_TOKEN` | `github_token` | required for GitHub projects |
| Web port | `--port` | `PORT` | `port` | `4000` |

Use `github_token: gh` in `global.yaml` to resolve the token from
`gh auth token` at startup. Literal token values also work but should not be
committed. `github_token: gh-auth`, `${gh auth token}`, and
`$(gh auth token)` are accepted aliases. If neither `GITHUB_TOKEN` nor
`github_token` is set, Detent falls back to existing per-workflow
`tracker.api_key` handling.

`detent doctor` prints the resolved runtime values and their sources, with the
GitHub token redacted.

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
