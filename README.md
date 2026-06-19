<p align="center"><img src="docs/brand/detent-mark.svg" width="88" height="88" alt="Detent"></p>

# Detent

[![CI](https://github.com/digitaldrywood/detent/actions/workflows/ci.yml/badge.svg)](https://github.com/digitaldrywood/detent/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/digitaldrywood/detent)](LICENSE)
[![Release](https://img.shields.io/github/v/release/digitaldrywood/detent?include_prereleases&sort=semver)](https://github.com/digitaldrywood/detent/releases)

## Start With AI

Hi, welcome to Detent. If you are reading this as a human, pause here and paste
the prompt below into Codex or Claude Code. Detent is meant to be driven from
the top down by AI agents, so the fastest way to get moving is to let an agent
inspect the repo, interrogate the onboarding runbook, and guide you through the
right setup path. You can keep reading by hand too; nobody will revoke your
keyboard.

```text
You are onboarding Detent with me. Treat this as an AI-driven project, not a
manual README skim.

Start by inspecting this repository. Read README.md, CLAUDE.md or AGENTS.md if
present, docs/ONBOARDING.md, CONTRIBUTING.md, build and language manifests,
.github/workflows, install scripts, and any existing WORKFLOW.md or global.yaml
examples. Detent can drive any project with a clear workflow and validation
gate, so use the repository evidence to identify the stack, tools, and commands
instead of starting from one language. Do not ask setup questions until you have
gathered local evidence.

Use docs/ONBOARDING.md as the interrogation guide. First determine which path
applies: a new Detent install, an existing Detent install that must be found and
verified, or a new repository/project being added to an existing Detent install.

For an existing install, find and verify the detent binary, config path, running
service or dashboard, registered projects, GitHub auth, Codex auth, and doctor
status before recommending changes. For a new install, follow the bootstrap flow
and verify each step before moving on. For adding a project, inspect the target
repository and existing global.yaml, then walk me through the board, workflow,
registration, issue intake, and smoke-test decisions.

Present findings with evidence, ask only the next necessary human decisions, and
do not create, link, mutate, or delete GitHub Projects, issue fields, labels,
issues, PRs, `WORKFLOW.md`, or `global.yaml`, or dispatch agents, until Phase 2
answers are recorded in `answers.env` and I explicitly confirm the mutation
step. Defaults are recommendations only; never execute a defaulted GitHub or
config mutation without my confirmation.
```

A **detent** is the catch that holds a moving part at a fixed position until it
is deliberately released — the click-stop on a dial, the notch on a ratchet.
Detent holds each piece of work at a defined stop on your board and only lets it
advance when a gate is cleared.

## What is this

Detent is status-driven agentic work orchestration, shipped as a single Go
binary, with code as its first proven domain. Today it can use a GitHub
ProjectV2 board as the source of truth, or it can run boardless from a
repository's GitHub issue `Status` field or repository status labels while
Detent supplies the Kanban view. For every code issue you mark ready it creates
an isolated Git worktree,
dispatches a Codex coding agent against a workflow contract you wrote, runs
your validation gate, opens a pull request, waits for review, and merges through
a serialized train — with all of it live on a web dashboard and a terminal UI.
The same status-to-gated-review-to-done shape is the trajectory for non-code
work: validation gates are now pluggable, while non-git or non-PR deliverables
remain follow-up work described in
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
to go from a bare machine to a running board. To onboard a repository, verify an
existing install, or add a new project to an existing Detent host, use the
agent-executable [Project Onboarding](docs/ONBOARDING.md) runbook.

## How it works

Configured GitHub status is the state machine; ProjectV2 board status, the
boardless issue field, or repository status labels drive everything.

1. **You write the contract.** Each project has a `WORKFLOW.md`: the tracker
   binding, board states, the agent prompt, the validation gate, and the review
   policy.
2. **You mark an issue `Todo`.** Detent claims it, creates an isolated Git
   worktree from your source checkout, and dispatches a Codex agent with the
   contract — moving the issue to `In Progress`.
3. **The agent works** in its own branch, runs your validation gate, and opens
   or updates a PR, then moves the issue to `Human Review`.
4. **Gates decide.** `Human Review` is the holding state. The workflow decides
   whether promotion to `Merging` waits for a human label, a current-head
   automated PR review, or only linked PR + green CI + quiet time. Unresolved
   feedback sends it to `Rework` for another pass.
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
- **[GitHub Projects v2, not Linear](#why-these-defaults).** Issues, status
  columns, priorities, labels, blockers, comments, and pull requests are the
  state machine.
- **Multi-project from one host.** `global.yaml` runs many repositories with
  weights, priority, pause, and fair scheduling.
- **Explicit gates + a serialized merge train.** CI, optional automated PR
  review criteria, and a one-at-a-time `Merging` lane, so what lands is always
  green.
- **Pluggable validation gates.** Code defaults use `make check`, CI, and
  automated review, while workflow authors can choose whether a command gate
  requires automated PR review or instead only waits for CI and the quiet
  window. A human approval-label gate is available when the workflow explicitly
  wants one.
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

## Why these defaults

Two choices define Detent's footprint: **GitHub Projects** as the board and
**Codex** as the coding agent. Both are deliberate.

### Why GitHub Projects, not Linear

The reference design Detent grew from polls a **Linear** board while code, pull
requests, and CI live in **GitHub** — two systems for one unit of work. That
split forces you to map Linear issue IDs onto GitHub PR numbers and to read
discussion in two places: planning comments in Linear, review comments in
GitHub. Detent puts the whole state machine in one system. A GitHub Project
*is* the board; its issues are the work items, its pull requests are the
deliverables, and its comments and reviews are where every conversation
happens. One ID space, one place to look.

It is also cheaper at the scale where orchestration matters:

- **Cost.** GitHub Projects has no per-seat charge and ships with repositories
  most teams already pay for. Linear's Business plan is \$16/user/month — about
  \$9,600/year at 50 seats — and its free tier is capped.
- **API headroom.** Authenticated GitHub REST allows 5,000 requests/hour (more
  for GitHub Apps); Linear allows 1,500 requests/hour against a complexity
  budget. A poller driving many repositories wants the larger ceiling.

### Why Codex, not Claude

Detent dispatches agents non-interactively, headless, many at once. The
important question is how that mode is metered.

- A **ChatGPT** plan (Plus, Pro, Business) covers Codex CLI usage *including
  scripted `codex exec` automation*, billed against the subscription you already
  have.
- **Claude Code** keeps interactive terminal and IDE use on subscription usage
  limits. Effective **June 15, 2026**, Anthropic moves headless `claude -p`,
  the Agent SDK, Claude Code GitHub Actions, and third-party Agent SDK apps to
  a separate monthly Agent SDK credit. That credit is per-user, does not roll
  over, and overages move to usage credits at standard API rates when enabled.

For an orchestrator that runs agents around the clock in parallel, the Codex
subscription is the one that makes the economics work. Codex is the agent
backend Detent ships; the `agents.backends` config keeps the door open for
others as their automation terms allow.

## Install

On Windows, use the package manager that already manages your developer tools.
Use Winget when the Detent package is available from the Windows Package Manager
community source:

```powershell
winget install --id DigitalDrywood.Detent --source winget
```

Use Scoop when you want a user-local install managed from a Scoop bucket:

```powershell
scoop bucket add digitaldrywood https://github.com/digitaldrywood/scoop-bucket
scoop install detent
```

Use the PowerShell installer for bootstrap, CI images, or machines where you do
not want to configure a Windows package manager first:

```powershell
irm https://raw.githubusercontent.com/digitaldrywood/detent/main/install.ps1 | iex
```

The PowerShell installer downloads the Windows release archive, verifies the
SHA-256 checksum, installs `detent.exe` to `%LOCALAPPDATA%\detent\bin`, and
adds that directory to the user PATH. Set `DETENT_INSTALL_DIR` to override the
install directory. Winget and Scoop installs also expose `detent.exe` on PATH; verify any Windows install with `detent --version`.

Install the latest Linux release with the shell installer:

```sh
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh
```

The shell installer downloads the Linux release archive, verifies the SHA-256
checksum, installs `detent` to `/usr/local/bin` when writable or
`$HOME/.local/bin` otherwise, and prints PATH guidance when the chosen install
directory is not already on PATH. Set `DETENT_INSTALL_DIR` to override the
install directory. Source checkouts can also run the repository-local shell
installer:

```sh
./install.sh
```

Use a native Linux package when you want apt, dnf, rpm, or another system
package workflow to own the binary, removal, and upgrades:

```sh
DETENT_VERSION=0.5.2 # release version without leading v
DETENT_ARCH=amd64 # or arm64
curl -LO "https://github.com/digitaldrywood/detent/releases/download/v${DETENT_VERSION}/detent_${DETENT_VERSION}_linux_${DETENT_ARCH}.deb"
sudo apt install "./detent_${DETENT_VERSION}_linux_${DETENT_ARCH}.deb"
detent --version
```

```sh
DETENT_VERSION=0.5.2 # release version without leading v
DETENT_ARCH=amd64 # or arm64
curl -LO "https://github.com/digitaldrywood/detent/releases/download/v${DETENT_VERSION}/detent_${DETENT_VERSION}_linux_${DETENT_ARCH}.rpm"
sudo rpm -Uvh "./detent_${DETENT_VERSION}_linux_${DETENT_ARCH}.rpm"
detent --version
```

Use the shell installer for a user-local install without sudo, for Linux
distributions that do not use `.deb` or `.rpm`, or for bootstrap scripts that
should fall back to `go install` when a release asset is unavailable.

Use Homebrew on macOS or Linux when you already manage CLI tools with Homebrew:

```sh
brew install digitaldrywood/tap/detent
```

Use Go on any platform when you want to build from source instead of using a
release archive:

```sh
go install github.com/digitaldrywood/detent/cmd/detent@latest
```

After installing, check for updates with:

```sh
detent update --check
```

Release-installer installs can update with `detent update`; use
`detent update --yes` for non-interactive automation and
`detent update --format json` for machine-readable status. The legacy
`detent update --json` flag remains supported. On Windows, replacement is
staged and completes after the running `detent.exe` exits. Package-manager
installs should be upgraded by the package manager:

```sh
winget upgrade --id DigitalDrywood.Detent
scoop update detent
brew upgrade digitaldrywood/tap/detent
```

Native Linux packages are owned by the system package manager; install a newer
`.deb` with `sudo apt install ./detent_<version>_linux_<arch>.deb`, or a newer
`.rpm` with `sudo rpm -Uvh ./detent_<version>_linux_<arch>.rpm` or the distro
wrapper you normally use. Go-installed binaries offer an
interactive choice: run
`go install github.com/digitaldrywood/detent/cmd/detent@latest`, switch to the
checksum-verified release binary, or abort. `detent update --yes` runs the Go
install command for go-installed binaries; `detent update --from-release`
switches the detected Go-installed binary to the release asset and pins future
updates to release-binary management. Source builds still print the recommended
command instead of overwriting the binary.

CI runs the `Installer Smoke` job on Ubuntu and Windows against the current
GitHub Release assets. The job runs `install.sh` and `install.ps1` in release
mode, checks checksum output, confirms the requested install directory and
installer lock metadata, then runs `detent update --check` and
`detent update --yes` from the release-installer install.

Release self-updates verify SHA256 checksums fetched from GitHub releases. The
checksum verifier supports detached minisign signature assets named
`<checksum>.minisig`, but enforcement is gated until the binary embeds the
pinned minisign public key for the release stream. Until that release signing
key is provisioned in #337, update integrity still depends on GitHub TLS plus
the published checksum file.

Requirements:

- Go 1.26 or newer when installing with `go install` or building from source.
- The [OpenAI Codex CLI](https://github.com/openai/codex) installed and signed
  in, so `codex app-server` runs on the host that dispatches agents. Detent
  drives every agent through this app-server. Verify with `codex --version`.
- The [GitHub CLI](https://cli.github.com) (`gh`) for authentication and GitHub
  lookups (optional but assumed throughout this guide).
- A GitHub token for the selected tracker mode. ProjectV2 mode usually needs
  `repo`, `read:org`, `read:project`, and write `project`. Boardless
  issue-field mode needs repository issue access plus organization issue-field
  read access; classic PATs use `repo` and `read:org`.

## CLI exit codes

Detent uses stable process exit codes so scripts and agents can branch on the
failure class.

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | General or unexpected error |
| 2 | Auth or GitHub token problem |
| 3 | Input validation error |
| 4 | Not found or config conflict |

## CLI JSON error envelopes

When the resolved output format is JSON, command failures write one
RFC 9457-style problem object to stderr. Human-readable pretty-mode errors are
unchanged.

```json
{
  "type": "https://detent.dev/errors/project_not_found",
  "code": "project_not_found",
  "title": "Project not found",
  "detail": "project \"ap\" not found",
  "exit_code": 4,
  "suggested_fix": "available: api, web, infra\ndid you mean \"api\"? see `detent config path`, then retry",
  "did_you_mean": ["api"],
  "docs_url": "https://detent.dev/docs/cli#project-not-found"
}
```

Envelope fields:

| Field | Required | Meaning |
| --- | --- | --- |
| `type` | Yes | Stable problem type URL, using the code slug. |
| `code` | Yes | Stable machine-readable slug. |
| `title` | Yes | Short human title for the error class. |
| `detail` | Yes | Specific failure detail. |
| `exit_code` | Yes | Process exit code for the failure. |
| `suggested_fix` | No | Actionable next step when Detent has a hint. |
| `did_you_mean` | No | Candidate correction list when Detent has suggestions. |
| `docs_url` | No | Documentation URL for the error class. |

Stable JSON error codes:

| Code | Type URL | Exit code | Source |
| --- | --- | --- | --- |
| <a id="general"></a>`general` | `https://detent.dev/errors/general` | 1 | Unexpected error. |
| <a id="validation"></a>`validation` | `https://detent.dev/errors/validation` | 3 | Input validation, invalid config, or invalid output format. |
| <a id="unknown-command"></a>`unknown_command` | `https://detent.dev/errors/unknown_command` | 3 | Unknown command. |
| <a id="unknown-flag"></a>`unknown_flag` | `https://detent.dev/errors/unknown_flag` | 3 | Unknown flag. |
| <a id="github-auth"></a>`github_auth` | `https://detent.dev/errors/github_auth` | 2 | GitHub token or authentication failure. |
| <a id="config-exists"></a>`config_exists` | `https://detent.dev/errors/config_exists` | 4 | `ErrConfigExists`. |
| <a id="project-exists"></a>`project_exists` | `https://detent.dev/errors/project_exists` | 4 | `ErrProjectExists`. |
| <a id="project-not-found"></a>`project_not_found` | `https://detent.dev/errors/project_not_found` | 4 | `ErrProjectNotFound`. |
| <a id="doctor-failed"></a>`doctor_failed` | `https://detent.dev/errors/doctor_failed` | 1 | `ErrDoctorFailed`. |
| <a id="shutdown-forced"></a>`shutdown_forced` | `https://detent.dev/errors/shutdown_forced` | 1 | `ErrShutdownForced`. |
| <a id="shutdown-timeout"></a>`shutdown_timeout` | `https://detent.dev/errors/shutdown_timeout` | 1 | `ErrShutdownTimeout`. |

## Release

Cut releases from `main` by pushing a semver tag:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

Tags matching `v*` trigger the release workflow, which runs GoReleaser and
publishes the GitHub Release archives, checksums, Homebrew formula, and Windows
package-manager manifests. Scoop publishing targets
`digitaldrywood/scoop-bucket`; Winget publishing pushes to the
`digitaldrywood/winget-pkgs` fork and opens a pull request against
`microsoft/winget-pkgs`. GoReleaser generates the manifests during snapshots and
skips publishing when `SCOOP_BUCKET_GITHUB_TOKEN` or `WINGET_GITHUB_TOKEN` is
not configured.

CI also runs `GoReleaser Snapshot` on every push to `main` so the current
merge head remains release-package validated. Pull requests run that snapshot
only when release packaging inputs change: `.goreleaser.yaml`, the CI or
release workflows, `Makefile`, Go module files, installer scripts, the release
public key, or removal/rename of the top-level `README.md` and `LICENSE` files
that are bundled into release archives. Other pull requests keep the required
lint, build, vet, test, race, coverage, and Windows checks without the release
packaging tail.

## Quick Start

The quickest compatibility setup is one GitHub ProjectV2 board and one local
repository checkout. New projects can also run boardless: Detent reads and
writes either a repository's organization-level GitHub issue `Status` field or
repository status labels, then shows workflow visibility in Detent's own
Kanban/dashboard surface.

1. Authenticate GitHub access for the mode you want:

```sh
# ProjectV2-backed board mode.
gh auth login --scopes "repo,read:org,read:project,project"
# For existing auth:
gh auth refresh -h github.com --scopes "repo,read:org,read:project,project"

gh auth status 2>&1 | rg '\brepo\b'
gh auth status 2>&1 | rg '\bread:org\b'
gh auth status 2>&1 | rg '\bread:project\b'
gh auth status 2>&1 | rg "(^|[[:space:],'\"])project([[:space:],'\"]|$)"

# Boardless issue-field mode with a classic PAT.
gh auth login --scopes "repo,read:org"
gh auth status 2>&1 | rg '\brepo\b'
gh auth status 2>&1 | rg '\bread:org\b'

# Boardless label mode with a classic PAT.
gh auth login --scopes "repo"
gh auth status 2>&1 | rg '\brepo\b'
```

Fine-grained PATs and GitHub Apps should grant Issues repository read/write
when Detent will move work or post comments, Pull requests read/checks read for
PR gates, Issue Fields organization read for issue-field status discovery, and
repository label access for label mode. Issue-field writes use the issue field
values API and require issue or pull request repository write permission plus
push access to the repository. Label mode uses repository label reads/writes and
issue label updates. If Kanban integration mode is enabled in a release that
supports it, comment submission also requires issue/PR comment write.

2. Choose the GitHub status source.

For the current/default compatibility path, use a GitHub ProjectV2 board. Find
the node id and use the `id` field, which starts with `PVT_`, as
`tracker.project_slug`:

```sh
gh project list --owner <org-or-user> --format json --limit 20
```

The `gh project list` command verifies the token can read ProjectV2 boards.
The write `project` scope is verified when Detent first performs an intentional
board mutation, such as provisioning fields or editing an issue status.

Detent auto-provisions any missing `Status` and `Priority` options on the board
the first time it runs, so you do not have to hand-create every column — but the
option names it creates and reads must match the states in your `WORKFLOW.md`.
GitHub uses single-select option order as board column order; Detent keeps the
known status options in canonical board order and leaves extra custom options
after the required Detent states.

For issue-field mode, create or reuse an organization-level single-select issue
field named `Status` and make sure it is available to the repository. GitHub
issue fields are issue-only: linked PR cards in Detent derive status from the
linked issue, not from a PR field. Discover the field and options with:

```sh
gh api /orgs/<org>/issue-fields --jq '.[] | select(.name == "Status")'
```

For label mode, create or reuse repository labels for the effective Detent
states. Detent applies `tracker.state_map`, slugifies the resulting state name,
and prefixes it with `tracker.status_label_prefix`, which defaults to
`detent:`. With the default release flow, the required labels are
`detent:backlog`, `detent:todo`, `detent:in-progress`, `detent:blocked`,
`detent:human-review`, `detent:rework`, `detent:merging`, and `detent:done`.
Discover existing labels with:

```sh
gh api repos/<owner>/<repo>/labels --paginate --jq '.[].name'
```

3. Create a `WORKFLOW.md` in the repository you want Detent to work on.

ProjectV2-backed board mode:

```markdown
---
tracker:
  kind: github
  github_status_source: project_v2
  project_slug: PVT_replace_with_project_id
  write_probe_issue: owner/repo#123
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
  dependency_auto_unblock:
    enabled: false
    source_states:
      - Blocked
    target_state: Todo
    readiness: terminal_or_merged
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
  dispatch_priority_by_label:
    - bug
    - regression
    - enhancement
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
  require_automated_review: true
  ci_failure_action: skip
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

Boardless issue-field mode:

```markdown
---
tracker:
  kind: github
  github_status_source: issue_field
  repository: owner/repo
  status_field: Status
  write_probe_issue: owner/repo#123
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
workspace:
  root: /absolute/path/to/detent-workspaces
  source_root: /absolute/path/to/project-checkout
agent:
  max_concurrent_agents_by_state:
    Merging: 1
gate:
  kind: command
  run: make check
  ci_failure_action: skip
---
You are working on {{ issue.identifier }}: {{ issue.title }}.
```

Boardless label mode:

```markdown
---
tracker:
  kind: github
  github_status_source: label
  repository: owner/repo
  status_label_prefix: "detent:"
  write_probe_issue: owner/repo#123
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
workspace:
  root: /absolute/path/to/detent-workspaces
  source_root: /absolute/path/to/project-checkout
agent:
  max_concurrent_agents_by_state:
    Merging: 1
gate:
  kind: command
  run: make check
  ci_failure_action: skip
---
You are working on {{ issue.identifier }}: {{ issue.title }}.
```

In label mode, the `detent:*` labels are tracker state, not workstream filters.
Use `tracker.authorization.labels.*`, `projects[].authorization`, and
`agent.dispatch_priority_by_label` for selecting or ranking work by ordinary
labels such as `documentation`, `bug`, or `enhancement`.

Kanban display is read-only by default. Keep that default for observers,
shared dashboards, the top-level `/kanban` fleet board, and initial rollout.
Project-specific Kanban pages can use integration mode when operators are
allowed to move cards and post issue or PR comments from Detent, and only after
`detent doctor` proves ProjectV2 status write, issue-field status write, or
status-label update for the selected status source, plus issue/PR comment
write:

```yaml
server:
  kanban:
    mode: read_only
    # mode: integration
    # allowed_transitions:
    #   In Progress: [Blocked, Cancelled]
    #   Rework: [Blocked, Cancelled]
    #   Merging: [Blocked, Cancelled]
    #   QA: [Blocked, Human Review]
```

When `allowed_transitions` is omitted, integration mode keeps a conservative
default for manual moves from execution states: active work such as
`In Progress`, `Rework`, and `Merging` can only move to configured exception
states such as `Blocked` or `Cancelled`. Add source-specific entries to allow a
project workflow to expose extra manual moves without changing Detent's UI code.

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
plus green CI, no P1 automated PR review findings, a quiet window, and a
current-head automated PR review before auto-promotion. Set
`require_automated_review: false` on a command gate when the workflow should
auto-promote from `Human Review` after a linked open PR, green CI, no P1 bot
review findings, and the quiet period. The quiet period resets on observed
issue updates, Project status updates, automated PR review submission, and
linked PR activity such as a fresh push to the PR head. Set
`ci_failure_action: rework` when failed or cancelled current-head CI should move
a `Human Review` item back to `Rework`; the default `skip` parks it in
`Human Review`, and pending CI stays parked. Use
`kind: human_review` with `approval_label` only when the workflow explicitly
requires a human approval label to promote.

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
  --workflow-ref origin/main \
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
the `codex` binary, GitHub auth mode, GitHub tracker readiness, git, and
whether the server port is free. In ProjectV2 mode it checks project access,
Status options, board item reads, repository issue/PR access, write probes, and
rate-limit visibility. In issue-field mode it checks repository access, issue
field discovery, Status option discovery, issue reads by field value, optional
issue-field write probes, issue/PR comment write when integration-capable
features are configured, and REST/GraphQL rate-limit visibility. In label mode
it checks repository access, status label mappings, issue reads by configured
status labels, optional status-label write probes, issue/PR comment write when
integration-capable features are configured, and REST/GraphQL rate-limit
visibility. Before
starting Detent, fix any `FAIL` (missing `github_token: gh` or an
unauthenticated `codex` are the usual culprits). Configure
`tracker.write_probe_issue` with a scratch issue when you want doctor to prove
write capabilities instead of reporting WARN for unproven writes. If Detent is
already running on the configured port, the server-port check can fail because
the live service owns the port; use `detent doctor --port 0` for the same
config, toolchain, token, and database preflight without the port collision,
then verify the live service with `/health`.

For Detent dogfood/self-tests that need a running server, start an isolated mock
runtime instead of stopping or reusing the live process on `127.0.0.1:4000`:

```sh
detent dev-runtime --port 0
```

The command prints `Mode: isolated dev runtime`, the selected dashboard URL,
temp home, DB mode, tracker mode, and fixture path. By default it uses a temp
config/workspace home, an in-memory SQLite database, a stateful fixture-backed
memory tracker, and a fake runner; it does not call GitHub or mutate a real
ProjectV2 board. It refuses the live dogfood port and live
`~/.detent/detent.db` unless explicitly overridden.

Use the built-in Kanban demo when you want to evaluate the operator board and
mutation dialogs without a GitHub token, a real ProjectV2 board, or production
database state:

```sh
detent dev-runtime --demo kanban --port 0
```

Pass `--demo-project` to choose the generated project ID when you want generic
demo URLs and labels instead of the default dogfood-safe ID:

```sh
detent dev-runtime --demo kanban --demo-project demo-project --port 0
```

Demo runtimes bind to `0.0.0.0` when `--host` is omitted so the selected
random port can be reached from trusted network interfaces. From another
machine on Tailscale, replace the local banner host with the Tailscale
hostname. With the override above, open `http://prometheus:<port>/kanban` for
the mixed-project board or
`http://prometheus:<port>/projects/demo-project/kanban` for the generated
project's interactive board. Pass `--host 127.0.0.1` for a local-only demo run.

The Kanban demo keeps the runtime isolated on the memory tracker, seeds at
least four projects with one or two cards each, and mixes configured project
colors with deterministic automatic colors. The fleet `/kanban` board is
read-only and shows cards across those projects; project-specific pages such as
`/projects/demo-project/kanban` enable integration mode for the generated demo
workflow. The demo includes explicit `server.kanban.allowed_transitions` such
as `Backlog -> Todo` so drag/drop moves can be exercised without weakening
production defaults. Demo cards cover Backlog, Todo, In Progress, Blocked,
Human Review, Rework, Merging, Done, and Cancelled states, including
issue-only cards, linked PR cards, CI pass, pending, and failure states, Codex
review clean and finding states, labels, assignees, blockers, and wait
metadata. Issue and PR comments are captured by the memory connector with no
external side effects.

Use the screenshots demo when you need deterministic pages, HTMX fragments, API
responses, reports, and SSE payloads for documentation screenshots, video
recording, or visual e2e baselines:

```sh
detent dev-runtime --demo screenshots --port 0
```

The screenshots demo uses the same isolation model and demo bind default as the
Kanban demo: memory
tracker, fake runner, isolated home, isolated database, isolated workspaces,
fake `https://github.test/...` URLs, no GitHub calls, no real ProjectV2
mutation, and no live dogfood port by default. It freezes demo time at
`2026-06-15T12:00:00Z` unless started with `--demo-clock play`, which advances
SSE ticks and visible running-work counters for video capture. The boot banner
prints the scenario manifest location. Screenshots mode intentionally keeps the
primary project fixed at `dogfood` so page routes and visual baselines remain
deterministic:

```text
Scenario manifest: /api/v1/demo/scenarios
```

Select a scenario with `X-Detent-Demo-Scenario`; the visible URL stays on the
normal page route:

```ts
const scenarios = [
  ["fleet-healthy-parallel-work", "/"],
  ["fleet-kanban-multiproject", "/kanban"],
  ["kanban-full-integration", "/projects/dogfood/kanban"],
  ["reports-normal-window", "/reports"],
];

for (const [scenario, route] of scenarios) {
  await page.setExtraHTTPHeaders({ "X-Detent-Demo-Scenario": scenario });
  await page.goto(`${baseURL}${route}`);
  await page.waitForLoadState("networkidle");
  await expect(page).toHaveScreenshot(`${scenario}.png`);
}
```

For visual comparisons, keep the screenshot environment stable: browser,
viewport, fonts, OS rendering, device scale factor, and generated assets should
match the baseline environment. The manifest includes each scenario ID, route,
required header, recommended viewport, screenshot name, and wait selector. A
quick JSON smoke check looks like this:

```sh
curl -H 'X-Detent-Demo-Scenario: fleet-healthy-parallel-work' "$DETENT_URL/api/v1/state"
```

Use the normal live runtime, `detent` with your global config, only when you
intend to operate on the configured tracker and ProjectV2 board. Use
`detent dev-runtime --fixture <path>` for focused fixture validation such as
autopromote behavior, `--demo kanban` for safe board exploration, and
`--demo screenshots` for deterministic page-addressable screenshots.

6. Start Detent:

```sh
detent
```

Open the dashboard at <http://localhost:4000>. Use `--host` and `--port` to
override the address. Before exposing a remote URL such as
`http://prometheus:4000/`, choose the dashboard bind mode:

- `127.0.0.1` keeps the dashboard local to the host and is the safest default
  for SSH tunnel access.
- A specific private or Tailscale IP exposes the dashboard only on that
  interface and is preferred for VPN-only access.
- `0.0.0.0` exposes the dashboard on every interface, not just Tailscale. Use
  it only on trusted private networks with the expected host firewall rules.

When Detent is bound to `127.0.0.1`, `curl` from the same host can work while
`http://<host>:4000/` fails from another machine because loopback is not
reachable remotely. Set `server.host` in `WORKFLOW.md` for the default bind, or
set `--host` in the CLI command or service `ExecStart`:

```sh
detent --host 127.0.0.1 --port 4000
detent --host <tailscale-or-private-ip> --port 4000
detent --headless --host 0.0.0.0 --port 4000
```

Verify the listener and the local or VPN URL you intend operators to use:

```sh
ss -ltnp | rg ':4000|detent'
curl -fsS http://127.0.0.1:4000/api/v1/state
curl -fsS http://<tailscale-or-private-ip>:4000/api/v1/state
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
   [`gh`](https://cli.github.com), then choose scopes for the tracker mode:

   ```sh
   # ProjectV2-backed board mode.
   gh auth login --scopes "repo,read:org,read:project,project"
   # For existing auth:
   gh auth refresh -h github.com --scopes "repo,read:org,read:project,project"

   # Boardless issue-field mode.
   gh auth login --scopes "repo,read:org"

   # Boardless label mode.
   gh auth login --scopes "repo"
   ```

   Verify the required classic PAT scopes independently. Boardless issue-field
   and label modes do not require `read:project` or `project`; label mode also
   does not require `read:org` unless another workflow setting needs it.

   ```sh
   gh auth status 2>&1 | rg '\brepo\b'
   gh auth status 2>&1 | rg '\bread:org\b'
   gh auth status 2>&1 | rg '\bread:project\b'
   gh auth status 2>&1 | rg "(^|[[:space:],'\"])project([[:space:],'\"]|$)"
   ```

   Use `github_token: gh` in `global.yaml` so Detent resolves this token at
   startup.

3. **Install and sign in to the Codex CLI.** Install the
   [OpenAI Codex CLI](https://github.com/openai/codex) and sign in. Detent
   dispatches every agent through `codex app-server`. Verify: `codex --version`.

4. **Choose the GitHub status source.** For the current/default compatibility
   path, choose the GitHub ProjectV2 board Detent will drive and get its node
   id (starts with `PVT_`):

   ```sh
   gh project list --owner <org-or-user> --format json --limit 50
   ```

   This verifies the token can read ProjectV2 boards. The write `project` scope
   is verified when Detent first performs an intentional board mutation. The
   board only needs to exist — Detent auto-provisions missing `Status` and
   `Priority` options on first run. The option names must match your
   `WORKFLOW.md` states, and Detent keeps known `Status` options in canonical
   board order.

   For boardless issue-field mode, skip ProjectV2 board creation and instead
   confirm the repository's organization has a single-select issue field named
   `Status`:

   ```sh
   gh api /orgs/<org>/issue-fields --jq '.[] | select(.name == "Status")'
   ```

   For boardless label mode, skip ProjectV2 board creation and organization
   issue-field setup, then confirm the repository has status labels with the
   configured prefix:

   ```sh
   gh api repos/<owner>/<repo>/labels --paginate --jq '.[].name' | rg '^detent:'
   ```

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

   For ProjectV2 mode, set `tracker.project_slug` (your `PVT_` id). For
   boardless issue-field mode, set `tracker.github_status_source:
   issue_field`, `tracker.repository: <repo-owner>/<repo-name>`, and optionally
   `tracker.status_field`. For boardless label mode, set
   `tracker.github_status_source: label`, `tracker.repository:
   <repo-owner>/<repo-name>`, and `tracker.status_label_prefix`. In every mode,
   set `workspace.source_root` (`<source-root>`), `workspace.root` (a worktrees
   directory), and the prompt body. The full field reference is in
   [Quick Start](#quick-start).

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

   Every check must pass before starting Detent. If Detent is already running on
   the configured port, the server-port check may fail because the live service
   owns the port. In that case, validate the rest of the setup without the port
   collision, then verify the running service:

   ```sh
   detent doctor --port 0
   curl -fsS http://127.0.0.1:4000/health | jq -e '.status == "ok" and .mode == "running"'
   ```

9. **Start Detent and confirm the dashboard:**

   ```sh
   detent --host 127.0.0.1 --port 4000
   ss -ltnp | rg ':4000|detent'
   curl -fsS http://127.0.0.1:4000/api/v1/state
   ```

   Keep `127.0.0.1` for SSH tunnels. For VPN access, use the selected private
   or Tailscale IP instead and verify it from another machine:

   ```sh
   detent --host <tailscale-or-private-ip> --port 4000
   curl -fsS http://<tailscale-or-private-ip>:4000/api/v1/state
   ```

   Use `--host 0.0.0.0` only when every host interface is trusted for dashboard
   access; it is not limited to Tailscale.

10. **Dispatch work.** Move an issue to `Todo` through the configured status
    source: ProjectV2 `Status`, issue-field `Status`, or the `detent:todo`
    status label. Detent claims it, creates an isolated worktree, dispatches an
    agent, and the issue appears under Running on the dashboard. Drive the rest
    through the configured status source (`Todo` → `In Progress` →
    `Human Review` → `Merging` → `Done`).

## Concepts

### Connectors

Detent isolates tracker integration behind a connector interface. The current
production connector is GitHub. It supports the current ProjectV2-backed board
mode, boardless issue-field mode, and boardless label mode. A memory connector
is available for local development, and the connector boundary is where GitLab
and Jira support will land later.

GitHub configuration lives in each project's `WORKFLOW.md` frontmatter. The
default `github_status_source: project_v2` mode uses `project_slug` as the
GitHub ProjectV2 node id. Detent reads issue state, priority, labels, blockers,
and assignment from the board, then writes comments and state transitions back
through the connector. Boardless `github_status_source: issue_field` mode uses
`repository: owner/name` and an organization issue field such as
`status_field: Status`; Detent reads issues by issue-field value and updates
that field for state transitions. Boardless `github_status_source: label` mode
uses `repository: owner/name` and repository labels named by
`status_label_prefix`; Detent reads issues by configured status labels and
updates state by replacing the previous status label with the target one.
Set `tracker.write_probe_issue` to a scratch issue already present on that
ProjectV2 board or in the boardless repository if `detent doctor` should prove
write operations by replaying existing values and sending non-mutating
validation probes. In label mode, the probe issue must already have one
configured status label so doctor can reapply it. Without a probe issue, doctor
reports required write capabilities as WARN instead of inferring that broad
token scopes are enough.

GitHub issue fields apply to issues, not pull requests. In issue-field mode,
boardless status comes from the linked issue. In label mode, Detent treats
repository issues with configured status labels as work items. Detent still
displays linked PR state and can comment on PRs through GitHub's shared
issue-comment endpoints, but a PR-only card without a linked issue is not
dispatchable through issue-field or label status.

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
| `Blocked` | Human-blocked work, or dependency-waiting work when auto-unblock is enabled. |
| `Human Review` | The PR is ready for review/soak until the workflow's promotion criteria pass. |
| `Rework` | Human or bot feedback needs another agent pass. |
| `Merging` | Final rebase, merge-gate check, CI watch, and merge. |
| `Done` | Complete. |
| `Cancelled` | Terminal state mapped to `Done` in the default release flow. |

### Review gate

`Human Review` is the holding state before the merge train. Auto-promotion out
of that state is controlled by the workflow:

- `gate.kind: command` requires a linked open PR, green CI, no P1 automated PR
  review findings, and the configured quiet period. By default it also requires
  a current-head automated GitHub PR review.
- `gate.kind: command` with `require_automated_review: false` keeps the linked
  PR, green CI, no-P1, and quiet-period checks but does not require a bot PR
  review to exist.
- `gate.ci_failure_action: rework` routes failed or cancelled current-head CI
  from `Human Review` back to `Rework`; the default `skip` leaves the item
  parked while CI is not green.
- `gate.kind: human_review` requires a linked open PR plus the configured
  `approval_label` on the issue.

The quiet period resets on observed issue updates, Project status updates,
automated PR review submission, and linked PR activity such as a fresh push to
the PR head.

The quiet period is an intentional quality gate. Tune
`agent.auto_promote.quiet_seconds` when reviewer soak time is too conservative,
but keep the gate explicit so faster merges are a policy choice rather than an
accidental bypass.

A Codex coding session that created the PR is not the same signal as a
Codex/ChatGPT/Claude GitHub PR review. If automated PR review is required and
the PR head changes after a review, request or wait for a fresh automated review
before expecting auto-promotion.

### Set up status

You choose where GitHub status lives; Detent fills in the rest.

- **ProjectV2 mode:** create a GitHub **Projects v2** board (org or user) and point
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
- **Boardless issue-field mode:** create or reuse an organization issue field,
  normally a single-select `Status` field, and make it available to the
  repository. Configure `tracker.github_status_source: issue_field`,
  `tracker.repository: owner/name`, and optionally `tracker.status_field` when
  the field is not named `Status`. Detent's Kanban/dashboard view becomes the
  board surface; no GitHub ProjectV2 board or `tracker.project_slug` is needed.
- **Boardless label mode:** create or reuse repository labels for every
  effective Detent state. Configure `tracker.github_status_source: label`,
  `tracker.repository: owner/name`, and optionally
  `tracker.status_label_prefix` when the prefix is not `detent:`. The label name
  is the prefix plus the slugified mapped state: `In Progress` becomes
  `detent:in-progress`. The issue should have exactly one configured status
  label at a time. Detent's Kanban/dashboard view becomes the board surface; no
  GitHub ProjectV2 board, `tracker.project_slug`, or organization issue field
  is needed.
- **Blank `Status` values and missing status labels are not `Backlog`.** In the
  current release, an issue with no configured issue-field value or status label
  is not dispatchable through the state machine. Put unready work in the
  `Backlog` option or `detent:backlog` label explicitly.
- **Detent reads** status, priority, labels, blockers, assignees, and linked
  pull requests from each issue, and **writes back** status transitions and a
  `## Codex Workpad` comment as the agent works.

### Kanban Modes

Boardless projects use Detent's own dashboard as the day-to-day board. Keep
Kanban in `read_only` mode by default: operators can inspect lanes, linked PRs,
CI, review state, blockers, labels, and assignees without granting dashboard
write access. Enable `integration` mode only in a release that supports it and
only for trusted operators who should move cards and post comments from the
dashboard. Integration mode needs the same GitHub write permissions that
`detent doctor` probes: ProjectV2 status write in ProjectV2 mode, issue-field
status write in issue-field mode, status-label update in label mode, and
issue/PR comment write for comment forms.

### Migration Notes

Existing users do not need to migrate. Leaving
`tracker.github_status_source` unset keeps ProjectV2 as the source of truth,
and existing `tracker.project_slug` workflows remain valid. This is the
compatibility path when the GitHub Project board is where humans already plan,
rank, and move work.

To switch a repository to boardless issue-field mode, create the organization
issue `Status` field and options, copy current issue statuses from the
ProjectV2 board manually or with a one-off script outside Detent, then change
the workflow to `github_status_source: issue_field` with `repository:
owner/name`. Detent does not automatically migrate ProjectV2 items to issue
fields. After the switch, run `detent doctor --port 0` and fix field discovery,
option discovery, write-probe, comment-write, and rate-limit checks before
dispatching.

To switch a repository to boardless label mode, create status labels matching
the effective workflow states, copy current issue statuses by applying exactly
one configured status label per issue, then change the workflow to
`github_status_source: label` with `repository: owner/name` and
`status_label_prefix: "detent:"`. Detent does not automatically migrate
ProjectV2 items or issue-field values into labels. After the switch, run
`detent doctor --port 0` and fix label mapping, issue reads by label,
write-probe, comment-write, and rate-limit checks before dispatching.

### Dependency workflows

Detent supports two dependency patterns. Use the one that matches how much of
the wait should be visible on the board.

- **Keep the issue in `Todo`.** Add a machine-readable dependency line such as
  `Depends on: #123`, `Blocked by: owner/repo#123`, or
  `Depends on: https://github.com/owner/repo/issues/123`. Detent keeps the
  issue out of dispatch while any referenced blocker is non-terminal, then
  dispatches it normally after blockers clear. This is the default behavior and
  needs no extra configuration.
- **Keep the issue in `Blocked`.** Enable `tracker.dependency_auto_unblock` when
  your team wants dependency-waiting issues to sit in a waiting column. Detent
  only moves issues that have explicit `Depends on:` or `Blocked by:` references.
  When all blockers are terminal, closed, or have a merged linked PR under the
  configured `readiness` rule, Detent updates the configured GitHub status
  source to `target_state` and posts an audit comment. Without
  `tracker.dependency_auto_unblock.enabled: true`, a `Blocked` issue is observed
  for display but will not be moved back to `Todo`. Human blockers without
  explicit dependency references stay blocked.

Before you dispatch anything, run **`detent doctor`** — it checks config
resolution, the database, the `codex` binary, GitHub auth mode, configured
tracker access, repository issue/PR access, required write proofs, rate-limit
visibility, git, and the server port. A clean pre-start `doctor` clears
Detent's direct preflight.
When a running Detent process already owns the configured port,
`detent doctor --port 0` validates the config, database, tools, and token
without treating the live listener as a blocker; pair it with `/health` on the
actual service before dispatching more work. Do not dispatch from a failed
doctor run unless the only failure is that expected live-port collision and
`/health` is green. If Detent runs under a systemd user service, also verify the
service PATH resolves every command used by project hooks and validation gates;
`doctor` checks Detent's direct dependencies, not repo-specific bootstrap tools.
The onboarding runbook includes the service-context check.

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

Inside the serialized `Merging` lane, avoid duplicating the full local release
gate when it does not buy new signal. If the PR already passed the pre-review
gate, the branch rebases cleanly onto current `origin/main`, and no source files
change during rebase, the merge agent should run a focused rebase/smoke gate
locally and rely on required current-head CI for full enforcement. If the merge
agent edits code, resolves conflicts, detects stale or unknown validation state,
or cannot prove the final rebase was source-clean, it must run the full
configured gate again.

CI waiting should poll current-head REST check runs with backoff, not loop on
GraphQL-heavy PR status commands. Merge handoff telemetry should record the
quiet-window wait, local merge-gate duration, current-head PR CI duration, slow
check names, and whether post-merge `main` CI is still running. The quiet
window, current-head required CI, and conflict/full-gate fallback are quality
gates; repeated full local validation after a source-clean rebase, noisy status
polling, uncached tool install, and duplicated post-merge work are optimization
targets.

The repository CI caches the project-pinned golangci-lint binary and only builds
it with `go install` on cache miss. The official prebuilt action was evaluated,
but the prebuilt `v2.1.6` binary targets an older Go toolchain than this repo and
newer prebuilt lint releases change the enforced lint set. `GoReleaser Snapshot`
continues to run on every PR in this workflow; moving it off PRs or making it
path-based is a release-policy decision because it trades package coverage for
merge latency.

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
instance_name: buildbox
global:
  max_concurrent_agents: 8
  scheduling: weighted
  fair_share:
    half_life: 1h
  startup:
    jitter_seconds: 10
    max_spawn_per_second: 2
    max_concurrent_starts: 4
projects:
  - id: detent
    workflow: /absolute/path/to/detent/WORKFLOW.md
    workflow_ref: origin/main
    workdir: /absolute/path/to/detent
    color: "#1192e8"
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

Set optional `projects[].color` to an opaque CSS hex color in `#RGB` or
`#RRGGBB` form when a project needs a fixed visual marker. The sidebar,
project cards, and top-level multi-project Kanban board keep the project name
or ID visible and use color only as an additional compact marker. Projects
without a configured color receive a deterministic automatic color from a
curated categorical palette based on the project ID, so colors remain stable
across restarts and do not depend on project order. When there are more
projects than palette entries, Detent deterministically reuses palette colors;
labels and project IDs remain the primary identifiers.

Set `projects[].workflow_ref` when the workflow file should be read from a git
ref in the configured source checkout instead of the checkout's working-tree
copy. `workflow` may be an absolute path under `workdir` or a repository
relative path such as `WORKFLOW.md`. When the ref advances, Detent reloads the
workflow content from that ref; when `workflow_ref` is omitted, Detent keeps
reading the working-tree file.

Use the project administration commands to edit `global.yaml`:

```sh
detent add-project \
  --id <id> \
  --workflow <WORKFLOW.md> \
  --workflow-ref origin/main \
  --workdir <dir> \
  --weight 1 \
  --priority 3

detent pause <id>
detent unpause <id>
detent promote <id> --priority 1
detent remove-project <id>
```

These commands persist the global config. A running Detent process watches the
active `global.yaml`, including symlinked config targets, and reconciles
supported live-reload fields without a process restart. Invalid edits are
logged and ignored while the last valid config stays live.

| Field | Reload behavior |
| --- | --- |
| Project list and project settings | Live reload |
| Credentials: `github_token` and project credentials | Live reload |
| `global.startup` | Live reload |
| `instance_name` | Live reload |
| `global.identity` | Live reload; project runtimes restart in-process and `/api/v1/state.instance.name` updates after the next telemetry snapshot |
| `global.max_concurrent_agents`, `global.scheduling`, `global.fair_share` | Restart required |
| `port`, `env`, `log_level` | Restart required |

When a changed field requires restart, Detent logs
`global config setting change requires restart` with the field name.

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
| `/kanban` | Read-only fleet Kanban board across all registered projects. The sidebar link appears only when more than one project is registered. |
| `/projects/<id>` | Project-scoped dashboard overview. |
| `/projects/<id>/kanban` | Project-scoped Kanban board; read-only or integration mode follows that project's workflow config. |
| `/health` | Server health and configured dependency checks. |
| `/events` | Server-sent dashboard updates. Use `?view=kanban` for the fleet board and `?project=<id>&view=kanban` for a project board. |
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
make modernize-check
```

`make dev` runs Air with `ENV=dev` and
`LOG_LEVEL=debug`, builds a `dev`-versioned `./tmp/detent` with the current
commit SHA and build date, rotates
`tmp/air-combined.log`, and streams combined build and application output to
`tmp/air-combined.log`.

`make check` runs the local release gate: build, `golangci-lint`, `go vet`,
NilAway, race tests, and the 70 percent coverage check. Run `make generate`
before committing changes to Templ templates, sqlc queries, or Tailwind inputs.
`make modernize-check` runs the Go modernizer diff check with the repo's
selected safe analyzer set.

Packages that own transport, hub, watcher, orchestrator, and runner goroutines
also run `go.uber.org/goleak` from package-level tests, so `go test ./...`,
race tests, and `make check` fail on unexpected goroutines. Add goleak ignores
only in the package that needs them, and only after identifying the dependency
or intentionally shared test goroutine.

Nil safety is enforced by `make check` and can also be run directly while
iterating:

```sh
make nilaway-audit
```

The project uses the standalone NilAway command instead of golangci-lint
integration because the linter integration requires a custom module-plugin
binary. Go 1.26's experimental `runtime/pprof` `goroutineleak` profile remains a
runtime audit aid behind `GOEXPERIMENT=goroutineleakprofile`; the stable CI
coverage for now is the goleak-backed test gate.

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

## CLI Output

Detent command output is selected by `--format pretty|json`. The explicit flag
wins, then `DETENT_FORMAT`, then the stdout terminal check. Interactive
terminals default to `pretty`; pipes, redirects, and agent subprocesses default
to `json`. JSON is written to stdout. Progress and logs that would corrupt a
JSON stdout stream are written to stderr in JSON mode.

This changes piped output for scripts that parsed the old prose output. Use
`--format pretty` for a single command or `DETENT_FORMAT=pretty` for a process
environment that must keep the old text shape.

Structured command objects:

| Command | JSON object |
| --- | --- |
| `detent version` | `{"version":"v0.1.0","commit":"abc1234","build_date":"2026-06-13T00:00:00Z","go_version":"go1.26.4","os":"linux","arch":"amd64"}` |
| `detent update` | The update status object, including `current_version`, `latest_version`, `latest_tag`, `update_available`, `install_source`, `action`, `message`, and `command` when present. |
| `detent init` | `{"status":"ok","path":"/path/global.yaml","rule":"--config"}` |
| `detent add-project` | `{"id":"api","workflow":"/repo/WORKFLOW.md","workflow_ref":"origin/main","workdir":"/repo","weight":1,"priority":0,"paused":false,"credential_ref":"github"}` |
| `detent pause api` / `detent unpause api` | `{"status":"ok","project":"api","paused":true}` |
| `detent promote api --priority 1` | `{"status":"ok","project":"api","priority":1}` |
| `detent remove-project api` | `{"status":"ok","project":"api","removed":true}` |
| `detent config path` | `{"path":"/path/global.yaml","rule":"--config"}` |
| `detent doctor` | `{"checks":[{"name":"Config resolution","status":"OK","detail":"...","hint":"..."}],"summary":{"ok":8,"warn":0,"fail":0},"result":"PASS"}` |

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
| Instance name | | | `instance_name` | short hostname |

The web host resolves from `--host`, then the first registered workflow's
`server.host`, then the built-in `127.0.0.1` default. It is not a top-level
`global.yaml` key.

Use `github_token: gh` in `global.yaml` to resolve the token from
`gh auth token` at startup. Literal token values also work but should not be
committed. `github_token: gh-auth`, `${gh auth token}`, and
`$(gh auth token)` are accepted aliases. If neither `GITHUB_TOKEN` nor
`github_token` is set, Detent falls back to existing per-workflow
`tracker.api_key` handling.

Use `instance_name` to distinguish browser tabs and the dashboard navbar when
several Detent instances are open at once. Detent resolves the display name
from the first non-empty value in this order: top-level `instance_name` in
`global.yaml`, `global.identity.name`, the short hostname, then empty. In
single-project fallback mode without `global.yaml`, workflow top-level
`identity.name` is used before the short hostname. Names are trimmed, must be a
single line, and are capped at 40 characters in the web UI.

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
