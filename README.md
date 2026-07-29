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

Treat https://github.com/digitaldrywood/detent as the canonical Detent source
repository. Do not assume the current working directory is the Detent source
checkout or the target repository being onboarded. Use GitHub as the
first-class Detent documentation source when a verified local checkout is
absent, stale, or not desired. Do not clone Detent by default; cloning is only
an optional fallback when remote reads are unavailable or I explicitly ask for a
local source checkout. Keep the Detent source repository separate from any
target repository being onboarded; Detent may be a reference/source repository,
not the target.

Pin the Detent docs to a concrete canonical commit before relying on them. Run
`DETENT_DOCS_COMMIT="$(gh api repos/digitaldrywood/detent/git/ref/heads/main --jq '.object.sha')"`
and record `DETENT_DOCS_ACCESS_METHOD=github_api`,
`DETENT_DOCS_REPOSITORY=digitaldrywood/detent`, `DETENT_DOCS_REF=main`, and
`DETENT_DOCS_COMMIT` in the initial evidence packet. Also run
`detent --format json version` when the binary is available and report installed
binary version, binary commit, binary build date, and
`DETENT_BINARY_MATCHES_CANONICAL` against `DETENT_DOCS_COMMIT`. If a verified
local Detent checkout is present, you may additionally set `DETENT_SOURCE_ROOT`
and run `git -C "$DETENT_SOURCE_ROOT" fetch origin main:refs/remotes/origin/main`,
`git -C "$DETENT_SOURCE_ROOT" rev-parse HEAD`, and
`git -C "$DETENT_SOURCE_ROOT" rev-parse refs/remotes/origin/main`; report the
local source root, local `HEAD`, canonical `origin/main`, and
`DETENT_SOURCE_MATCHES_CANONICAL`. If the local checkout is absent, stale, or
cannot be proven current, read Detent docs from GitHub at `DETENT_DOCS_COMMIT`
instead of cloning or relying on local files.

From the pinned Detent documentation source, read README.md, AGENTS.md and
CLAUDE.md if present, docs/ONBOARDING.md, CONTRIBUTING.md, build and language
manifests, .github/workflows, install scripts, docs/templates, workflow
examples, and any existing WORKFLOW.md or global.yaml examples. Use
`gh api repos/digitaldrywood/detent/contents/<path>?ref="$DETENT_DOCS_COMMIT"`
or raw GitHub URLs pinned to the same commit. Detent can drive any project with
a clear workflow and validation gate, so use the repository evidence to
identify the stack, tools, and commands instead of starting from one language.
Do not inspect a target repository's
ProjectV2 boards, labels, issues, WORKFLOW.md, validation commands, or runtime
docs until the identity gate below is explicit and confirmed.

Use the pinned Detent documentation source's docs/ONBOARDING.md as the interrogation
guide. First determine which path applies: a new Detent install, an existing
Detent install that must be found and verified, or a new repository/project
being added to an existing Detent install. Distinguish reference repositories from the target repository being onboarded. In Phase 0.5, run
`detent onboarding draft-answers --output pretty` from the target checkout, or
pass `--target-source-root` if you are currently in the Detent source checkout.
When a local Detent checkout is available, also pass
`--detent-source-root "$DETENT_SOURCE_ROOT"` so the draft records source and
binary freshness evidence.
To write the candidate identity record, run
`detent onboarding draft-answers --answers "$ONBOARDING_DIR/answers.env" --write`.
Treat the draft as a candidate, not confirmation. The command should infer and
restate an identity candidate from the current git checkout before asking for
raw answer fields, using only identity-safe local evidence first: `pwd`,
`git rev-parse --show-toplevel`, `git remote get-url origin`, the Detent
documentation source identity, the installed Detent config path, and
registered project ids. If the current working directory is a GitHub checkout
and is not the canonical Detent source checkout, propose it as the target
candidate. If the current working directory is the Detent source checkout, do
not propose Detent as the target unless I explicitly say I am onboarding Detent
itself. Explain that the customer/workstream id is only a stable local
workstream id. Present the candidate in human-facing language first, then show
the `answers.env` representation. Set `IDENTITY_CONFIRMED=true` only after I
confirm the restatement, then run `detent onboarding validate-answers --answers "$ONBOARDING_DIR/answers.env" --phase identity`.
If I volunteer a status-source answer before identity is confirmed, such as
"use label for this repo", preserve it as a pending decision outside
`answers.env`. Restate it as pending in the conversation. For label mode, say:
"I have your status-source choice as label mode. I will keep it pending until
the identity gate validates, then append GITHUB_MODE=label and run the decision
validator without asking again." Do not write `GITHUB_MODE` to `answers.env`
until the identity phase passes; after identity validation succeeds, append
`GITHUB_MODE=label` and run the decision validator without asking again. For
non-label modes, carry the selected `GITHUB_MODE` value the same way.
If the `detent` binary is not installed yet, follow the Detent source README's
Install path or Bootstrap On A New Machine steps 1-3 first, verify the binary
with `detent version`, and defer `detent onboarding validate-answers` until the
binary is available.

For an existing install, find and verify the detent binary, config path, running
service or dashboard, registered projects, GitHub auth, Codex auth, and
read-only doctor status with `detent doctor --port 0` before recommending
changes. Do not pass `--allow-write-probes` until the mutation gate passes and I
explicitly confirm mutation. For a new install, follow the bootstrap flow and
verify each step before moving on. For adding a project, treat existing
registered projects as examples only; do not reuse tracker mode, status
namespace, validation gate, dashboard bind, workspace root, scheduling priority,
auto-promote policy, review policy, or mutation scope unless I explicitly
accept that setting for this customer/project.

Present findings with evidence and ask only the next necessary human decisions.
Ask and record `GITHUB_MODE` explicitly after the identity phase and before
target-specific discovery; never infer ProjectV2, issue-field, or label mode
from existing projects. Recommendations can cite evidence, but they are not
selected answers. Before recommending review, auto-promotion, dependency
unblock, or merging settings, ask in plain English whether I want full
autopilot, a Human Review gate, or conservative/manual approval. If I ask for
maximum automation, map that to `DELIVERY_PROFILE=full_autopilot` and summarize
the behavior before showing `answers.env` fields. Do not recommend
`AUTO_PROMOTE_ENABLED=false` or stopping at Human Review unless I selected
review gate or conservative/manual; do not create, link, mutate, or delete GitHub Projects, issue fields, labels,
issues, PRs, `WORKFLOW.md`, or `global.yaml`, or dispatch agents, until Phase 2
answers are recorded in `answers.env`, `detent onboarding validate-answers`
passes for the selected phase, and I explicitly confirm the mutation step.
Defaults are recommendations only; never execute a defaulted GitHub or config
mutation without my confirmation.
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
For project settings, start with the
[complete configuration reference](docs/config.md), the
[minimal example](config.example.yaml), or the
[annotated example](config.annotated.yaml).

## How it works

Configured GitHub status is the state machine; ProjectV2 board status, the
boardless issue field, or repository status labels drive everything.

1. **You write the contracts.** Each project has a checked-in `detent.yaml`
   machine contract for tracker bindings, states, lifecycle policy, scheduling,
   retries, leases, and gates, plus a checked-in, portable `WORKFLOW.md` agent
   instruction contract. The prompt declares the project's required CI stage
   categories and the project-specific commands and check names that satisfy
   each category. Agents use that declaration when they change CI configuration
   or review a change: every required stage must exist and pass on the current
   pull request head. Optional gitignored `detent.local.yaml` and
   `WORKFLOW.local.md` files apply machine-specific configuration and agent
   direction, respectively, without changing the shared contracts.
2. **You mark an issue `Todo`.** Detent claims it, creates an isolated Git
   worktree from your source checkout, and dispatches a Codex agent with the
   contract — moving the issue to `In Progress`.
3. **The agent works** in its own branch, runs your validation gate, and opens
   or updates a PR. Review-gate workflows move the issue to `Human Review`;
   autopilot workflows leave it active with `status: complete` in the Workpad.
4. **Gates decide.** The workflow decides whether promotion to `Merging` waits
   in `Human Review`, waits in the active lane, requires a current-head
   automated PR review, or only needs linked PR + green CI + quiet time.
   Unresolved feedback sends it to `Rework` for another pass.
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
subscription is the default that makes the economics work. Detent still supports
explicit backend routing, including Claude Code, so operators can choose a
backend per role when the limits, auth mode, and isolation trade-offs fit that
work.

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

CI runs the `Installer Smoke` confidence job on Ubuntu and Windows against the
current GitHub Release assets after merges to `main`, on release tags, on the
nightly schedule, and from manual workflow dispatch. The job runs `install.sh`
and `install.ps1` in release mode, checks checksum output, confirms the
requested install directory and installer lock metadata, then runs
`detent update --check` and `detent update --yes` from the release-installer
install.

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
  To route selected work to a local Ollama model without Detent code changes,
  see [Local Models With Codex And Ollama](docs/local-models-ollama.md).
- The Claude Code CLI installed and signed in when routing selected roles to
  `claude_code`. Verify with `claude --version`. Detent does not store Claude
  credentials; it uses the ambient `claude` CLI login or the
  `ANTHROPIC_API_KEY` environment visible to the Detent worker.
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

CI runs `GoReleaser Snapshot` after merges to `main`, on `v*` release tags, on
pull requests, on the nightly schedule, and from manual workflow dispatch so
release packaging is validated before the PR merge lane and after it lands.
Required branch checks must not pass as path- or event-dependent no-ops on pull
requests when the same check name runs real validation on `main`.

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
With `tracker.auto_provision` enabled, Detent creates missing repository status
labels for configured workflow states on startup.
Discover existing labels with:

```sh
gh api repos/<owner>/<repo>/labels --paginate --jq '.[].name'
```

3. Create `detent.yaml` and `WORKFLOW.md` in the repository you want Detent to
   work on. Start from a paired `docs/templates/detent.*.yaml` and
   `docs/templates/WORKFLOW.*.md` preset.

Existing combined `WORKFLOW.md` frontmatter remains readable during the
compatibility window. Run `detent doctor` to identify legacy, split, mixed, or
stale layouts. A migratable warning includes the exact
`detent fix workflow-layout --workflow <path>` command; preview it with
`--dry-run`, confirm interactively, or pass `--yes` for explicit
non-interactive confirmation. The fixer uses the normal global-config path and
GitHub credential resolution (`GITHUB_TOKEN`, top-level `github_token`, and
`github_token: gh`) without writing resolved secrets into project files. See
[Workflow Layout Migration](docs/workflow-layout-migration.md).

Legacy combined ProjectV2 example:

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
  github_graphql_min_remaining_reserve: 1000
  github_rest_min_remaining_reserve: 1000
  github_rest_fanout_max_requests: 80
  github_rest_debug_logging: false
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
  blocked_recovery:
    enabled: false
    source_states:
      - Blocked
    target_state: Rework
    reason_codes:
      - merge_conflict
      - stale_base
      - missing_current_head_ci
  blocker_auto_promote:
    enabled: false
    blocker_states:
      - Backlog
      - Blocked
      - Human Review
    target_state: Todo
polling:
  interval_ms: 120000
workspace:
  root: /absolute/path/to/detent-workspaces
  source_root: /absolute/path/to/project-checkout
  cache_strategy: isolated
  auto_branch: true
  cleanup_idle_ttl_ms: 86400000
  cleanup_sweep_interval_ms: 600000
agent:
  max_concurrent_agents: 5
  max_turns: 20
  max_turn_duration_ms: 0
  max_session_duration_ms: 0
  merge_worker_max_duration_ms: 21600000
  max_retry_backoff_ms: 300000
  overload_retry_delay_ms: 45000
  no_progress_spend_limit_usd: 3
  failure_breaker:
    same_class_limit: 5
    window_seconds: 3600
    cooldown_seconds: 3600
  resume_orphaned_sessions: true
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
  prioritize_unblockers: true
  auto_promote:
    enabled: false
    quiet_seconds: 600
    optout_label: requires-human-review
    allowed_issue_labels: []
    gate_wait_state: review
    gate_wait_timeout_seconds: 3600
    gate_wait_timeout_action: human_review
    rework_limit: 3
  skills:
    enabled: true
    path: .detent/skills
    max_skills_in_prompt: 50
    creation:
      enabled: true
      max_drafts_per_run: 1
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspaceWrite
    networkAccess: true
  turn_timeout_ms: 3600000
  read_timeout_ms: 5000
  stall_timeout_ms: 300000
gate:
  kind: command
  run: make check
  automated_review: required
  required_status_checks: []
  ci_failure_action: rework
  transient_ci_retry_limit: 2
  validator:
    enabled: false
    # Recommended cheap override when enabled: gpt-5.4-mini.
    # Watch rework-rate per validator model once cache/model telemetry lands.
    model: ""
    min_score: 0.8
    max_attempts: 3
    max_inline_diff_bytes: 65536
    block_on:
      - p1
server:
  host: 127.0.0.1
  port: 4000
  board_snapshot_stale_after_seconds: 900
hooks:
  timeout_ms: 60000
---
You are working on {{ issue.identifier }}: {{ issue.title }}.

Read the issue description, follow repository instructions, keep changes
scoped to the issue, run the project validation gate, and prepare the work for
human review.
```

Detent persists a last-known board snapshot beside the runtime database every
30 seconds. On restart it renders that snapshot with stale styling until live
tracker hydration completes. `server.board_snapshot_stale_after_seconds`
controls the maximum cache age; older snapshots are ignored.

Set `github_rest_fanout_max_requests` to `0` to disable the per-cycle fanout cap while retaining the REST remaining-reserve guard.

Tag-based projects can opt into release cadence in the same workflow file:

```yaml
release:
  enabled: true
  min_merged_issues: 5
  max_age_hours: 24
  require_green_ci: true
  version_bump: auto
  rerun_flaky_once: false
  flaky_check_names: []
```

Set `tracker.repository: owner/repo` as well, including in ProjectV2 mode, so
Detent can inspect commits and file failure issues on the tracked board.

The policy triggers after either threshold is reached, requires every check on
the default-branch head to finish green, creates an annotated semantic-version
tag, and observes the `Release` GitHub Actions workflow through completion.
`feat` commits bump the minor version; other conventional commits bump the
patch version. Failed candidate CI and failed release workflows create a
fingerprint-deduplicated `detent:todo` issue instead of retrying indefinitely.
The flaky rerun option is disabled by default and only reruns named workflow
checks once.

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
  required_status_checks: []
  ci_failure_action: rework
  transient_ci_retry_limit: 2
  validator:
    enabled: false
    # Recommended cheap override when enabled: gpt-5.4-mini.
    # Watch rework-rate per validator model once cache/model telemetry lands.
    model: ""
    min_score: 0.8
    max_inline_diff_bytes: 65536
    block_on:
      - p1
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
  required_status_checks: []
  ci_failure_action: rework
  transient_ci_retry_limit: 2
  validator:
    enabled: false
    # Recommended cheap override when enabled: gpt-5.4-mini.
    # Watch rework-rate per validator model once cache/model telemetry lands.
    model: ""
    min_score: 0.8
    max_inline_diff_bytes: 65536
    block_on:
      - p1
---
You are working on {{ issue.identifier }}: {{ issue.title }}.
```

In label mode, the `detent:*` labels are tracker state, not workstream filters.
Use `tracker.authorization.labels.*`, `projects[].authorization`, and
`agent.dispatch_priority_by_label` for selecting or ranking work by ordinary
labels such as `documentation`, `bug`, or `enhancement`.
`agent.prioritize_unblockers` defaults to `true` and ranks an unlabeled issue
ahead of otherwise-equal peers when it directly unblocks waiting work. State,
tracker priority, and every configured dispatch-label tier remain higher.

The fleet `/kanban` board stays read-only, as do observer dashboards and
shared dashboards where users should not mutate GitHub from Detent. For a
trusted operator-owned project board, default `server.kanban.mode:
integration` before mutation authorization. Skipped pre-mutation write probes
are not evidence for `read_only`; mutation still requires authorization and
post-authorization `detent doctor --allow-write-probes` to prove ProjectV2
status write, issue-field status write, or status-label update for the selected
status source, plus issue/PR comment write:

```yaml
server:
  kanban:
    mode: integration
    # Set show_blocked_alerts: true only when red blocked states should appear
    # as one compact top-of-board alert; dependency waits stay on cards.
    # show_blocked_alerts: true
    # Use mode: read_only for observer/shared dashboards, no-writes choices,
    # or failed post-authorization write probes.
    # allowed_transitions:
    #   In Progress: [Blocked, Cancelled]
    #   Rework: [Blocked, Cancelled]
    #   Merging: [Blocked, Cancelled]
    #   QA: [Blocked, Human Review]
```

When `allowed_transitions` is omitted, integration mode keeps conservative
defaults for manual moves from execution states: active work such as
`In Progress`, `Rework`, and `Merging` can only move to configured exception
states such as `Blocked` or `Cancelled`. Add source-specific entries to allow a
project workflow to expose extra manual moves without changing Detent's UI code.

Use `agent.instructions_by_state` and `agent.instructions_by_transition` when a
workflow stage needs prompt text that should stay out of the main
`WORKFLOW.md` body. The runner appends matching workflow instructions after the
base prompt and before Detent's deliverable and validation-gate blocks. State
keys and transition source/target keys must reference configured workflow
states.

For a non-code editorial workflow such as `Research -> Draft -> Review ->
Package -> Publish`, the stage-specific instructions can live in frontmatter:

```yaml
tracker:
  active_states: [Research, Draft, Review, Package]
  terminal_states: [Publish, Cancelled]
agent:
  instructions_by_state:
    Research: |
      Gather source notes, links, and open questions before drafting.
    Draft: |
      Write the article body and keep unresolved facts clearly marked.
    Review: |
      Address editorial feedback and leave a concise change summary.
    Package: |
      Prepare final assets, metadata, and publication handoff notes.
  instructions_by_transition:
    Review:
      Package: |
        Confirm all review comments are resolved before packaging.
```

`workspace.source_root` is the checked-out repository Detent uses for
`git worktree add`; `workspace.root` is where per-issue worktrees are created.
If `workspace.source_root` is omitted, Detent falls back to the project
`workdir` from global config, then to `workspace.root` for older single-root
setups.

`workspace.cache_strategy` controls the worker build environment. The default,
`isolated`, sets `GOCACHE`, `GOMODCACHE`, `GOBIN`, and
`GOLANGCI_LINT_CACHE` inside the per-turn scratch directory and removes them
after the worker exits. Set it to `shared` to place those caches in a stable,
per-project directory under `workspace.root/.detent/cache`, prepend the shared
`GOBIN` to `PATH`, and reuse compiled dependencies and tools across worktrees
and Detent restarts. `TMPDIR`, `TMP`, and `TEMP` remain per-turn in both modes.

When `workspace.auto_branch` is enabled and the source repository has an
`origin` remote, Detent fetches that remote's default branch before creating a
new managed branch. The source checkout's current branch and `HEAD` do not
affect the new branch base. Existing managed branches are reused unchanged;
local-only repositories without an `origin` continue to use `HEAD`.

`workspace.cleanup_idle_ttl_ms` controls how long non-active observed
workspaces can sit idle before cleanup. Terminal issues are cleaned on the next
poll when observed. `workspace.cleanup_sweep_interval_ms` controls the startup
and periodic idle cleanup cadence.

`polling.interval_ms` defaults to `120000` and must be at least `60000`.
`polling.conditional` defaults to `true`. GitHub label and issue-field trackers
cache response ETags for issue lists and REST hydration endpoints, send
`If-None-Match` on later polls, and reuse the cached representation after a
`304 Not Modified`. An authorized conditional request that returns `304` does
not consume GitHub's primary REST quota. This makes a `60000` interval practical
for an idle REST-backed board while preserving the default cadence for existing
workflows. Set `conditional: false` to restore unconditional requests; trackers
without conditional-request support continue using their existing polling path.
`polling.interval_ms` only controls tracker and project-board refreshes; it does
not control Codex terminal-command waits or the enclosing tool-call yield
horizon, which are governed by developer instructions on Codex turns.
See GitHub's [conditional request guidance](https://docs.github.com/en/enterprise-cloud@latest/rest/using-the-rest-api/best-practices-for-using-the-rest-api#use-conditional-requests-if-appropriate).

REST telemetry distinguishes `total_requests`, `conditional_requests`,
`not_modified_requests`, and `billable_requests`. Endpoint contributors expose
the same breakdown, and the REST rate-limit card's cycle cost uses billable
requests rather than free `304` checks.

### GitHub Webhook Freshness

Each GitHub-backed project can opt into near-real-time external updates by
setting a webhook secret in its `WORKFLOW.md`:

```yaml
tracker:
  kind: github
  github_status_source: label
  repository: digitaldrywood/detent
  github_webhook_secret: $DETENT_GITHUB_WEBHOOK_SECRET
polling:
  interval_ms: 60000
  conditional: true
```

Set `DETENT_GITHUB_WEBHOOK_SECRET` in the Detent process environment. Configure
the repository webhook with:

- Payload URL: `https://<detent-host>/api/v1/webhooks/github`
- Content type: `application/json`
- Secret: the same high-entropy value
- Events: Issues, Pull requests, Check suites, and Labels

Detent verifies `X-Hub-Signature-256` with HMAC-SHA256, routes the delivery only
to projects whose configured repository and secret match, and queues a fetch for
the affected issue. Pull-request and check-suite deliveries resolve Detent's
issue from its generated branch name. A repository-level label event has no
single issue target, so it queues a normal conditional refresh. Unknown
repositories never trigger a fleet-wide fallback. See GitHub's
[webhook signature validation](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
and [event payload reference](https://docs.github.com/en/webhooks/webhook-events-and-payloads).

For a ProjectV2 tracker spanning multiple repositories, omit `tracker.repository`
to let the connector verify project membership after signature validation. Set
`tracker.repository` when the project is repository-scoped and strict routing is
preferred.

GitHub must be able to reach the payload URL. For local testing, GitHub documents
[forwarding deliveries with smee.io](https://docs.github.com/en/webhooks/using-webhooks/handling-webhook-deliveries).
For a persistent host without public ingress, an outbound tunnel such as a
[Cloudflare Tunnel published application](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/get-started/create-remote-tunnel/#2a-publish-an-application)
can route a public HTTPS hostname to Detent. Configure relays and reverse proxies
to preserve the raw request body and GitHub signature headers; modifying either
causes signature verification to fail.

### Alert and Scheduled Intake

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

### Scheduled maintenance routines

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

### Scheduled backlog admission

Backlog admission evaluates existing issues and proposes which ones should
become eligible for dispatch. It does not create work like routines, classify
incoming work like intake, rank eligible work like the scheduler, or change
issue status. Every proposal is a durable record with an expiry and one audit
comment. To accept a proposal, an operator posts the exact proposal-specific
`/detent admission accept <proposal-id>` command and then moves the issue to the
configured target state. To reject it, the operator posts
`/detent admission reject <proposal-id>`.

Admission is disabled unless a project opts in:

```yaml
backlog_admission:
  enabled: true
  schedule: "0 6 * * 1-5"
  sources:
    states: [Backlog]
  target_state: Todo
  criteria_section: "Admission criteria"
  exclude_labels: []
  authors:
    allow: []
  max_candidates_per_run: 50
  max_proposals_per_run: 3
  max_open_proposals: 10
  proposal_expiry_days: 7
```

State names come from the project's workflow. Admission reads them through the
candidate-reader contract: the tracker declares selector support, reads pages
inside a hard per-run bound, filters the requested states exactly, sorts by
creation time and stable issue identity before applying the result limit, and
reports whether the source was truncated. Admission keeps a finite over-read
window of at least 100 issues so its existing priority ordering, author filter,
excluded-label filter, and `max_candidates_per_run` cap still apply after the
connector read. A reader truncation is recorded as `candidate_reader` in the
run ledger instead of making the bounded result look complete. A run defers
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
## Admission criteria

- **Alignment** — Does this serve a stated current priority? Do not propose a
  candidate that maps to no stated priority.
- **Readiness** — Is the problem actionable, with the acceptance conditions
  needed by the agent that will implement it?
- **Size** — Is the work bounded enough for one agent to complete?
```

The agent receives only the resolved criteria and bounded candidate data in a
fresh read-only session. It emits typed proposals. Detent re-fetches each issue,
validates every field and verbatim criteria quote, rejects proposals matching no
dimension, and persists valid records before posting comments. Confidence is
stored as telemetry and never authorizes a transition. Repeated runs use a
title/body fingerprint, so the proposal's own comment cannot create a
duplicate. Unanswered proposals expire; an issue demoted after acceptance is
not proposed again.

Acceptance is attributed only when the command names an open proposal and a
subsequent transition enters that proposal's target state. When both events
include actors, they must identify the same actor. A target-state transition
without that correlation remains an unattributed transition and does not accept
the proposal. Rejection, expiry, and supersession are separate outcomes:
silence can expire a proposal but never accepts or rejects one.

The only admission selector currently defined is `sources.states`. Capability
is declared per tracker and per GitHub status source:

| Tracker | Status source | `sources.states` |
| --- | --- | --- |
| `github` | `project_v2` | Supported |
| `github` | `issue_field` | Supported |
| `github` | `label` | Supported |
| `github_local` | local SQLite status | Supported |
| `local_sqlite` | local SQLite status | Supported |
| `memory` | process-local status | Supported |
| `linear` | Linear workflow state | Unsupported |

An enabled admission configuration fails validation when its tracker/status
source does not declare the selector, so Linear never validates cleanly and
then fails on its first scheduled read. `authors.allow` on `local_sqlite` or
`memory` produces a doctor warning because those trackers do not discover
authors. ProjectV2 configurations using `exclude_labels` warn that only the
first 20 issue labels are fetched. The memory tracker is evaluation-only across
restarts: durable proposal records can survive while its process-local comments
and mutations do not.

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

### Efficiency retrospection

Each project can opt into an evidence-based reflection loop over its runtime
telemetry. The pass runs after agent completions and on a daily schedule,
groups recurring inefficiency patterns, and creates or updates fingerprinted
board issues for human triage.

```yaml
retro:
  enabled: true
  schedule: "0 3 * * *"
  target_state: Backlog
  labels: [retro]
  product_repository: digitaldrywood/detent
  daily_issue_cap: 3
  lookback_days: 7
  min_occurrences: 2
  single_occurrence_severity: critical
  fallback_threshold: 3
  receipt_baseline_multiple: 4
```

Workflow findings stay on the project's board and include a governed proposed
`WORKFLOW.md` change when the evidence maps to a known setting. Product findings
such as completed-work re-dispatch or systemic capacity handling route to
`product_repository`. The configured `retro` label is always retained, and
`target_state: Todo` can be used when a project explicitly wants findings to
skip Backlog triage.

Detent files a finding after `min_occurrences`, or after one occurrence at or
above `single_occurrence_severity`. `daily_issue_cap` limits newly created
issues; recurrence updates to existing fingerprinted issues remain allowed.
Retrospection never edits workflow files, prompts, or runtime configuration.
Its issue body records the evidence, proposed change, and pending human outcome.
`detent doctor` reports the last run, finding count, and filed/updated issue
counts for every enabled project. ProjectV2 trackers must set
`tracker.repository` so Detent knows where to create project-level findings.

`gate` controls the validation contract the agent and operator flow follow.
Omitting it preserves the code default: `kind: command` with `run: make check`,
plus green CI, no P1 automated PR review findings, a quiet window, and a
current-head automated PR review before auto-promotion. Set
`automated_review: optional` to wait through
`agent.auto_promote.gate_wait_timeout_seconds` and then continue to `Merging`
when the remaining checks pass. Set `automated_review: off` to skip that wait.
The legacy `require_automated_review` boolean remains accepted and maps `true`
to `required` and `false` to `off`. The quiet period resets on observed
issue updates, Project status updates, automated PR review submission, and
linked PR activity such as a fresh push to the PR head. Failed or cancelled
current-head CI moves a `Human Review` item back to
`Rework` by default. Set `ci_failure_action: skip` only when red CI should park
in `Human Review`; pending CI stays parked. Use
`kind: human_review` with `approval_label` only when the workflow explicitly
requires a human approval label to promote.
Set `required_status_checks` to the exact branch-protection or ruleset check
names that are release-blocking for the project. Detent treats a configured
required check as non-green when it is missing, skipped, failed, cancelled,
neutral, or still running on the current PR head.
For required workflows that run only on a `pull_request` `labeled` event, set
`ci_trigger_label` to that label, such as `ci:ready`. Detent then instructs
implementation and rework workers to run the host-coordinated
`detent ci-trigger-label` command after every head-changing push before they
wait for current-head checks, passing the configured GitHub tracker host for
Enterprise installations. Generated commands encode label and host arguments
without shell-specific quoting. Detent records successful worker pushes and
reapplies the configured label at completion when the worker did not leave it
as the final CI trigger after the latest push. This preserves label ordering
when a project uses additional CI lane labels. Merging workers also reapply it
immediately after deterministic head-changing pushes and whenever current-head
hydration reports required checks as missing, including after a rebase or
another merge advances the base. Trigger events use a shared lock and persisted
timestamp per repository and are spaced by
`ci_trigger_label_stagger_seconds` (a positive value, default `15`) to avoid a
self-hosted CI stampede. `detent doctor` warns when it finds a label-gated
required check without this setting.

Set `gate.validator.enabled: true` to add a validator-agent review before
auto-promotion. The validator inspects the PR diff against the issue acceptance
criteria and returns a structured verdict, score, summary, and severity-tagged
findings. `gate.validator.model` optionally overrides the selected validator
route model, `min_score` below threshold routes to `Rework`, and any finding
severity listed in `block_on` routes to `Rework` regardless of score.
Validator production failures are retried with backoff up to `max_attempts`
(default `3`). Each failure is logged and visible to `detent doctor`; an
exhausted validator routes the item to `Rework` with the failure cause.
For Codex-backed validators, start with the cheap-tier override
`gate.validator.model: gpt-5.4-mini` and watch rework-rate per validator model
once cache/model telemetry lands. `gate.validator.max_inline_diff_bytes`
defaults to `65536`; validator prompts include the full diff only at or below
that size and otherwise seed stat-only context.

`plan` controls the optional plan-approval stop before implementation. It is
disabled by default, preserving the direct dispatch behavior. When enabled, the
first `Todo` dispatch runs in plan-only mode, posts a `## Detent Plan` issue
comment, and moves the issue to the configured `stop` such as `Plan Review`.
`review: human` waits for `approval_label` (`plan-approved` by default),
`review: automated` waits for a `## Detent Plan Review` issue comment or
current-head automated review state, and `review: both` accepts either path.
Blocking P1 plan findings route the issue to `Rework` with feedback.

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

For first-time onboarding, leave `--workflow-ref` unset until this
`WORKFLOW.md` has been merged to the ref Detent should read from.
`detent doctor` validates the configured ref; setting
`--workflow-ref origin/main` before `origin/main:WORKFLOW.md` exists will fail
even when the file exists locally in the working tree. After the first workflow
merge, add `workflow_ref: origin/main` to the project entry when you want Detent
to load the workflow from the branch tip instead of the working-tree file.

Edit the resolved `global.yaml` and set the top-level runtime keys:

```yaml
env: prod
log_level: info
github_token: gh
port: 4000
update:
  auto_check_enabled: true
  check_interval_hours: 24
  auto_apply_enabled: false
```

5. Verify the setup before dispatching:

   ```sh
   detent doctor --allow-write-probes
   ```

`detent doctor` is a preflight check: config resolution, the SQLite database,
the `codex` binary, GitHub auth mode, GitHub tracker readiness, git, and
whether the server port is free. It also reports each project's active
definition root, layout, revision, authority files, local overlays, and whether
the running process is stale. In ProjectV2 mode it checks project access,
Status options, board item reads, repository issue/PR access, and rate-limit
visibility. In issue-field mode it checks repository access, issue field
discovery, Status option discovery, issue reads by field value, and REST/GraphQL
rate-limit visibility. In label mode it checks repository access, status label
mappings, issue reads by configured status labels, and REST/GraphQL rate-limit
visibility. By default doctor is read-only: if a configured workflow would run
write probes, the report warns that they were skipped. Pass
`--allow-write-probes` only after the onboarding mutation gate has passed and
the operator has explicitly confirmed mutation. With that flag, ProjectV2 and
issue-field modes require `tracker.write_probe_issue` when integration needs
status-write proof, because their status mutations target a concrete project
item or issue field value. Label mode does not need a persistent status-labeled
scratch issue for the default permission proof: doctor sends intentionally
invalid repository-label and issue-create requests, expecting GitHub to reject
them with validation while proving the token has the repository Issues write
permission class. Configure `tracker.write_probe_issue` in label mode only for
legacy/deep issue-object proof, such as reapplying an existing status label on a
scratch issue. That proof is stronger for the chosen issue object, but the issue
must be kept off the board by removing Detent status labels or closing it after
migration. Before starting Detent, fix any `FAIL` (missing `github_token: gh` or
an unauthenticated `codex` are the usual culprits). If Detent is already running
on the configured port, the server-port check can fail because
the live service owns the port; use `detent doctor --port 0` for the same
read-only config, toolchain, token, and database preflight without the port
collision, or `detent doctor --port 0 --allow-write-probes` after mutation
authorization to prove writes, then verify the live service with `/health`.

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
as `Backlog -> Todo` so sheet-based Move actions can be exercised without
weakening production defaults. Demo cards cover Backlog, Todo, In Progress, Blocked,
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

Use the capture harness when you need the canonical video-production artifact
set from one command:

```sh
detent dev-runtime capture --out ./capture
```

The harness starts an isolated screenshots demo on an ephemeral local port,
loads the scenario manifest, captures the canonical still set, and writes a
deterministic terminal onboarding cast. It does not read or write the operator's
real `~/.config/detent/global.yaml`. Stable output paths are:

```text
capture/demo-capture-v1.json
capture/stills/v1/01-fleet-healthy-parallel-work.png
capture/stills/v1/02-fleet-kanban-multiproject.png
capture/stills/v1/03-kanban-full-integration.png
capture/stills/v1/04-project-active-overview.png
capture/stills/v1/05-reports-normal-window.png
capture/stills/v1/06-onboarding-project-selection.png
capture/terminal/v1/onboarding.cast
```

By default the browser viewport is `1920x1080` with
`--device-scale-factor 2`, producing 4K PNGs. Pass `--scenario <id>` one or
more times for a named subset, `--all-scenarios` for every browser-capturable
GET scenario in the manifest, `--width`, `--height`, and
`--device-scale-factor` for alternate framing, or `--demo-clock play` when a
motion capture needs advancing counters. The PNG capture uses a local
Chrome-family browser; pass `--browser <path>` or set `DETENT_CAPTURE_BROWSER`
when auto-detection cannot find one.

The CI browser visual gate runs Playwright when a PR changes UI-sensitive paths
such as `.github/workflows/ci.yml`, `go.mod`, `go.sum`, `package.json`,
`static/**`, `internal/web/**`, `internal/cli/dev_runtime*.go`,
`internal/devruntime/**`, Templ inputs, or screenshot/onboarding docs. It builds
the PR's Detent binary, starts isolated `dev-runtime` instances on port `0`,
captures current evidence under `tmp/playwright-evidence`, and uploads
Playwright reports, traces, screenshots, and image diffs when assertions fail.
PRs without UI-sensitive changes keep the required `Browser Visual` check fast
by building the Detent binary and running a CLI smoke instead of starting
Playwright.

Run the layout gate locally after installing Playwright's Chromium browser:

```sh
npx playwright install chromium
make visual-e2e
```

Committed image baselines are authoritative for GitHub Actions Ubuntu
x64/Chromium. On non-Linux hosts, `make visual-e2e` still runs the browser
layout assertions and captures evidence, but skips pixel comparison unless
`DETENT_VISUAL_STRICT=1` is set.

Update baselines only when the visual change is intentional. Run the update in
the same Ubuntu x64/Chromium environment as CI, then review and commit the
changed files under `tests/visual/__screenshots__/chromium/`:

```sh
make visual-e2e-update
```

Do not commit `tmp/playwright-evidence`, `tmp/playwright-report`, or
`tmp/playwright-results`; those are transient review and debugging artifacts.

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

On shutdown, the first Ctrl-C stops new dispatches and drains running agent
sessions while the terminal reports the blocker count and time remaining. A
second Ctrl-C force-quits immediately, interrupts those sessions, and re-queues
their issues. A new process using the same runtime database will fail with an
actionable error until the prior process has released it; a listener conflict
also fails before SQLite migrations begin.

- `127.0.0.1` keeps the dashboard local to the host and is the safest default
  for SSH tunnel access.
- A specific private or Tailscale IP exposes the dashboard only on that
  interface and is preferred for VPN-only access.
- `0.0.0.0` exposes the dashboard on every interface, not just Tailscale. Use
  it only on trusted private networks with the expected host firewall rules.

When Detent is bound to `127.0.0.1`, `curl` from the same host can work while
`http://<host>:4000/` fails from another machine because loopback is not
reachable remotely. Set `server.host` in `detent.yaml` for the default bind, or
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

6. **Author the project contract.** Copy the mode-specific template as a
   starting point, then edit it. For from-zero board creation, interview
   questions, issue intake, and the first-dispatch smoke test, follow
   [Project Onboarding](docs/ONBOARDING.md):

   ```sh
   GITHUB_MODE="${GITHUB_MODE:?set GITHUB_MODE to project_v2, issue_field, or label}"
   curl -fsSL "https://raw.githubusercontent.com/digitaldrywood/detent/main/docs/templates/WORKFLOW.${GITHUB_MODE}.md" \
     -o <source-root>/WORKFLOW.md
   ```

   The maintained templates are
   [`WORKFLOW.project_v2.md`](docs/templates/WORKFLOW.project_v2.md),
   [`WORKFLOW.issue_field.md`](docs/templates/WORKFLOW.issue_field.md),
   [`WORKFLOW.label.md`](docs/templates/WORKFLOW.label.md), and the CLI-only
   local-status template
   [`WORKFLOW.github_local.md`](docs/templates/WORKFLOW.github_local.md).
   Non-code artifact workflows can start from
   [`WORKFLOW.non_code_artifact.md`](docs/templates/WORKFLOW.non_code_artifact.md).
   A runnable local example with seeded work items, artifact metadata, API
   payloads, and artifact-gate transitions lives in
   [`docs/examples/non-code-artifact`](docs/examples/non-code-artifact).
   They set `server.kanban.mode: integration` for trusted project boards;
   change that to `read_only` only for an observer or shared dashboard,
   explicit no-writes choice, or failed post-authorization write probes. They
   also include a `## Required Execution Flow` with `For Todo`, `For In
   Progress`, `For Rework`, and `For Merging` sections so merge workers have a
   terminal instruction: invoke `$go-workflow:ship`, merge and move the issue to
   `Done`, move it to `Rework` with an actionable defect, or leave it in
   `Merging` with a concrete external blocker recorded. Before dispatching
   `Merging`, confirm the Detent host's Codex environment exposes
   `$go-workflow:ship`; otherwise install or enable that workflow, or replace
   the `For Merging` section with equivalent project-local merge instructions.

   For ProjectV2 mode, set `tracker.project_slug` (your `PVT_` id). For
   boardless issue-field mode, set `tracker.github_status_source:
   issue_field`, `tracker.repository: <repo-owner>/<repo-name>`, and optionally
   `tracker.status_field`. For boardless label mode, set
   `tracker.github_status_source: label`, `tracker.repository:
   <repo-owner>/<repo-name>`, and `tracker.status_label_prefix`. For
   externally owned or open-source repositories where Detent must not pollute
   issues, labels, fields, Projects, or issue comments, use
   `tracker.kind: github_local` from `WORKFLOW.github_local.md`; do not set
   `tracker.github_status_source`, configure `tracker.local_sqlite.path`, and
   import explicit issue numbers with
   `detent github-local import <project-id> <issue-number> --state Todo`. In
   every mode, set `workspace.source_root` (`<source-root>`), `workspace.root`
   (a worktrees directory), `write_probe_issue` for ProjectV2 or issue-field
   status-write proof, and the prompt body. In label mode, set
   `write_probe_issue` only when using legacy/deep issue-object probes.
   If the repository already has a `WORKFLOW.md`, audit its prompt body for the
   same Required Execution Flow and `Current Detent status: {{ issue.state }}`
   line before dispatching Detent against it.
   Registered projects can use `github_token: gh` in `global.yaml`; leave
   `tracker.api_key` out of the workflow unless you are intentionally using a
   workflow-local token. The full field reference is in [Quick Start](#quick-start).

   Interactive alternative: when Detent starts without a resolved `global.yaml`
   and without a `WORKFLOW.md` in the current directory, it serves the
   `/onboarding` web wizard. Open `http://localhost:<port>/onboarding` to walk
   through tracker, credentials, project, agent, and write steps for generating
   `WORKFLOW.md`. The first step asks for ProjectV2 board, organization issue
   field, or repository labels; label-mode users should choose **Repository
   labels**, then enter the repository and status label prefix such as
   `detent:`. The wizard hides ProjectV2 fields for boardless modes. Then
   return to the runbook for board creation, global registration, issue intake,
   and the smoke test.

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
   detent doctor --allow-write-probes
   ```

   Every check must pass before starting Detent. If Detent is already running on
   the configured port, the server-port check may fail because the live service
   owns the port. In that case, validate the rest of the setup without the port
   collision, then verify the running service:

   ```sh
   detent doctor --port 0 --allow-write-probes
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
    source: ProjectV2 `Status`, issue-field `Status`, the `detent:todo` status
    label, or the local SQLite state used by `github_local`. Detent claims it,
    creates an isolated worktree, dispatches an agent, and the issue appears
    under Running on the dashboard. Drive the rest through the configured
    status source (`Todo` → `In Progress` → `Human Review` → `Merging` →
    `Done`).

## Concepts

### Connectors

Detent isolates tracker integration behind a connector interface. The current
production connector is GitHub. It supports the current ProjectV2-backed board
mode, boardless issue-field mode, boardless label mode, and `github_local`
hybrid mode. A memory connector is available for local development, and the
connector boundary is where GitLab and Jira support will land later.

GitHub configuration lives in each project's `detent.yaml`. The
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
`tracker.kind: github_local` is a separate backend, not a fourth
`tracker.github_status_source` value. GitHub is read-only for issue, dependency,
parent/child, comment, PR, review, check, and rate-limit facts; Detent's SQLite
database is the source of truth for workflow state, claim fields, audit-trail
comments, priority, and local close decisions. In this mode Detent does not
create or mutate GitHub Projects, issue fields, repository labels, status
labels, GitHub issue comments, or GitHub issue close state. Pull request
lifecycle writes for Detent-owned PRs remain allowlisted.
Plain `detent doctor` is read-only and reports that write probes were skipped.
After mutation authorization, ProjectV2 and issue-field modes still need a
configured `tracker.write_probe_issue` to prove concrete status writes against
a project item or issue field value. Label mode can prove repository write
access without a scratch issue by sending intentionally invalid repository-label
and issue-create requests. GitHub should answer with validation errors, which
proves the Issues write permission class without creating board-visible work.
Set `tracker.write_probe_issue` in label mode only when you need legacy/deep
issue-object proof by replaying existing values on a scratch issue. That scratch
issue must already have one configured status label so doctor can reapply it;
after switching to the no-persistent probe path, remove any Detent status label
from old scratch issues or close them so they stop appearing as normal board
inventory.

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

### Cancellation Lifecycle

Manual `Cancelled` means Detent should stop managing the work item, not that
GitHub issues or pull requests are automatically closed. On the next poll that
observes the cancelled terminal state, Detent cancels any running agent context,
releases the global dispatch slot, clears configured claim lease state, records
the run as completed with final state `Cancelled`, and asks the workspace
backend to remove the Detent worktree, prune git worktrees, delete the generated
`detent/` branch when safe, and reap workspace processes.

Terminal cleanup is attempted on each poll for terminal states so a cancelled
non-running issue can still clean up an existing Detent workspace without
waiting for the idle cleanup sweep. The idle sweep interval still controls
non-terminal observed workspace cleanup.

Detent emits cleanup diagnostics in `/api/v1/state` under `events` and in
published telemetry snapshots. A successful cleanup records
`workspace_reap_succeeded` with `worktrees=`, `branches=`, and `processes=`
counts. Cleanup failures record `workspace_reap_failed`, leave the workspace
eligible for a later retry, and keep the diagnostic visible in recent events.
If Detent completes a terminal run but no workspace reaper is configured, it
records `workspace_reap_unverified`.

Detent does not close the GitHub issue when an item is manually cancelled. The
configured tracker state is the source of truth; in label mode, the default
`Cancelled: Done` state map means the repository label is `detent:done`.
Operators remain responsible for closing the GitHub issue with `not_planned` or
another reason when that is the desired repository record.

Detent also leaves linked pull requests open. Operators should close, comment
on, or reuse an open PR explicitly when cancelled work has one.

### Review gate

`Human Review` is the holding state before the merge train. Auto-promotion out
of that state is controlled by the workflow:

- `gate.kind: command` requires a linked open PR, green CI, no P1 automated PR
  review findings, and the configured quiet period. By default it also requires
  a current-head automated GitHub PR review.
- `gate.automated_review: optional` waits for a current-head review until the
  gate deadline, honors any review that arrives, and then promotes without one
  when the remaining checks pass. `off` does not wait; `required` waits and
  defaults to a Human Review timeout.
- `gate.required_status_checks` names release-blocking check runs or commit
  status contexts that must be present, completed, and successful on the current
  PR head.
- `gate.ci_trigger_label` re-fires label-gated required CI after each PR-head
  push and when Merging observes absent current-head checks;
  `gate.ci_trigger_label_stagger_seconds` spaces host-local reapplications by
  `15` seconds by default.
- `gate.ci_failure_action: rework` routes failed or cancelled current-head CI
  from `Human Review` back to `Rework` by default; set
  `gate.ci_failure_action: skip` only when non-green CI should leave the item
  parked.
- `gate.transient_ci_retry_limit` controls how many times Detent reruns
  failed checks with transient runner or infrastructure signals such as
  timeouts, startup failures, OOM kills, or `signal: killed` annotations before
  red CI is treated as a hard failure.
- `gate.validator.enabled: true` runs a validator-agent review before
  auto-promotion; verdicts below `min_score` or with severities in `block_on`
  route the issue to `Rework`.
- `agent.auto_promote.rework_limit` bounds repeated `Human Review` to `Rework`
  loops using persisted lane events; the default is `3`, and `0` is an explicit
  opt-out that leaves the loop unlimited. Positive limits require `Blocked` in a
  configured tracker state list and route the next over-limit rework decision
  there with repeated reasons summarized for handoff.
- `agent.auto_promote.gate_wait_state: source` keeps zero-quiet completed
  active issues in their current lane while CI/check gates are still pending;
  `review` restores the legacy behavior of parking those pending issues in the
  configured review source state. `gate_wait_timeout_seconds` bounds that active
  wait. `gate_wait_timeout_action: merge | human_review` makes the terminal
  action explicit; it defaults to `merge` for optional review and
  `human_review` for required review.
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
before expecting auto-promotion. `detent doctor` warns when required or optional
review is configured but none of the sampled recent merged PRs has an automated
review, which indicates the review producer may be inactive.

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
- **GitHub local-status mode:** configure `tracker.kind: github_local`,
  `tracker.repository: owner/name`, and `tracker.local_sqlite.path`. Do not set
  `tracker.github_status_source`. Import issues explicitly with
  `detent github-local import <project-id> 123,456 --state Todo`; unimported
  issues do not appear on Detent boards. Detent updates local workflow state
  only, while GitHub remains the read-only source for issue text, labels,
  assignees, dependencies, linked PRs, reviews, checks, and rate-limit health.
  The board surfaces divergence such as an upstream-closed issue that is still
  locally active.
- **Blank `Status` values and missing status labels are not `Backlog`.** In the
  current release, an issue with no configured issue-field value or status label
  is not dispatchable through the state machine. Put unready work in the
  `Backlog` option or `detent:backlog` label explicitly. Detent's own GitHub
  issue templates default to `detent:backlog` so new dogfood issues are visible
  to operators but are not dispatchable until triaged to `detent:todo` or
  another active state. Remove that label only when an issue is intentionally
  outside Detent.
- **Label-mode drift is surfaced as cleanup work.** In label mode,
  `detent doctor` and the dashboard report open repository issues with zero
  configured status labels, plus open issues that still carry a configured
  terminal status label such as `detent:done`. Add exactly one configured status
  label to untracked issues, and close or relabel stale-open terminal issues.
- **Detent reads** status, priority, labels, blockers, assignees, and linked
  pull requests from each issue, and **writes back** status transitions and a
  `## Codex Workpad` comment as the agent works, except in `github_local`,
  where those workflow writes are durable local records.

### Kanban Modes

Boardless projects use Detent's own dashboard as the day-to-day board. The
fleet `/kanban` board stays read-only because it is a cross-project observer
surface. A trusted operator project board should default to `integration`
before mutation authorization, so operators can move cards and post comments
from `/projects/<id>/kanban` after the mutation gate and write probes pass.
Skipped pre-mutation write probes are not evidence for `read_only`. Keep
`read_only` for an observer or shared dashboard, an explicit no-writes choice,
or failed post-authorization write probes for ProjectV2 status write in
ProjectV2 mode, issue-field status write in issue-field mode, status-label
update in label mode, or issue/PR comment write for comment forms.

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
fields. After the switch, run `detent doctor --port 0 --allow-write-probes` and
fix field discovery, option discovery, write-probe, comment-write, and
rate-limit checks before dispatching.

To switch a repository to boardless label mode, create status labels matching
the effective workflow states, copy current issue statuses by applying exactly
one configured status label per issue, then change the workflow to
`github_status_source: label` with `repository: owner/name` and
`status_label_prefix: "detent:"`. Detent does not automatically migrate
ProjectV2 items or issue-field values into labels. After the switch, run
`detent doctor --port 0 --allow-write-probes` and fix label mapping, issue reads
by label, write-probe, comment-write, and rate-limit checks before dispatching.

To switch a repository to local-only status mode, copy
`docs/templates/WORKFLOW.github_local.md`, set `tracker.repository`,
`tracker.local_sqlite.path`, workspace paths, and the prompt body, then import
only the issue numbers Detent should see:

```sh
detent github-local import <project-id> 123,456 --state Todo
detent doctor --project <project-id> --port 0
```

This mode does not migrate or mutate GitHub status data. Existing ProjectV2
items, issue fields, labels, and GitHub issue comments stay untouched. If
GitHub closes or transfers an imported issue while local state is still active,
Detent keeps the local row and surfaces the divergence on the board instead of
auto-resolving it.

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
- **Queue fixable blockers automatically.** Enable
  `tracker.blocker_auto_promote` alongside dependency auto-unblock when a
  dependency-waiting issue should pull same-repository blockers out of inactive
  states such as `Backlog`, `Blocked`, or `Human Review`. Detent promotes only
  resolved, same-repository blockers to the configured `target_state`, respects
  current local agent capacity, and posts an audit comment on the promoted
  blocker. Agents that move work to `Blocked` because of another tracked issue
  or pull request must ensure the issue body contains a machine-readable
  `Blocked by:` or `Depends on:` line; Workpad mentions alone are not a durable
  dependency contract.
- **Recover explicit PR-maintenance parks.** Enable `tracker.blocked_recovery`
  only when structured PR-maintenance parks should move work to the configured
  repair lane. The worker that intentionally creates such a park must set
  `reason_code: merge_conflict`, `stale_base`, or `missing_current_head_ci` in
  its blocked `detent-status` block; Detent persists that code on the lane
  entry. Recovery also requires the corresponding PR condition to still hold
  and a new diff-fingerprint/base-OID pair. Issue descriptions, manual status
  moves, and other prose do not authorize recovery. Keep this disabled on
  boards where `Blocked` is also used for deliberate operator parking.

Before you dispatch anything, run **`detent doctor --allow-write-probes`** after
mutation authorization — it checks config
resolution, the database, the `codex` binary, GitHub auth mode, configured
tracker access, repository issue/PR access, required write proofs, rate-limit
visibility, git, and the server port. A clean pre-start `doctor` clears
Detent's direct preflight.
When a running Detent process already owns the configured port,
`detent doctor --port 0 --allow-write-probes` validates the config, database,
tools, token, and write proofs without treating the live listener as a blocker;
pair it with `/health` on the actual service before dispatching more work. Do
not dispatch from a failed doctor run unless the only failure is that expected
live-port collision and `/health` is green. If Detent runs under a systemd user
service, also verify the
service PATH resolves every command used by project hooks and validation gates;
`doctor` checks Detent's direct dependencies, not repo-specific bootstrap tools.
The onboarding runbook includes the service-context check.

Keep systemd's default `KillMode=control-group` for the Detent service. Detent
starts each Codex and Claude worker in its own process group without leaving the
service cgroup, so its persisted worker registry can terminate stale process
groups on shutdown or startup while systemd remains the final cleanup backstop.
Do not wrap worker commands in launchers that double-fork or explicitly move
children into another cgroup.

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

When GitHub reports `HAS_MERGE_QUEUE`, Detent delegates every eligible green
candidate to the repository's native merge queue instead of rebasing each PR
through the serialized worker. GitHub owns merge-group validation and batching;
Detent keeps the issues in `Merging`, observes their queue entries, and
reconciles them to `Done` after GitHub reports the PR merged. Without a native
queue, a BEHIND PR whose existing head is mergeable and already has green
required checks is merged directly against the current base without rewriting
the checked head. Rebase is reserved for fallback cases; if a refreshed head
is missing required contexts, Detent routes the issue to `Rework` instead of
retrying an incomplete merge worker.

Inside the serialized `Merging` lane, avoid duplicating the full local release
gate when it does not buy new signal. If the PR already passed the pre-review
gate, the branch rebases cleanly onto current `origin/main`, and no source files
change during rebase, the merge agent should run a focused rebase/smoke gate
locally and rely on required current-head CI for full enforcement. If the merge
agent edits code, resolves conflicts, detects stale or unknown validation state,
or cannot prove the final rebase was source-clean, it must run the full
configured gate again.

CI waiting should poll current-head REST check runs with backoff, not loop on
GraphQL-heavy PR status commands. Required release checks must run on the PR
head before merge; post-merge-only failures cannot be part of the merge
decision. Merge handoff telemetry should record the
quiet-window wait, GitHub queue/start wait, local merge-gate duration,
current-head PR CI duration, active slow-check runtimes, and whether post-merge
`main` CI is still running. The quiet window, current-head required CI, and
conflict/full-gate fallback are quality gates; repeated full local validation
after a source-clean rebase, noisy status polling, uncached tool install, and
duplicated non-blocking post-merge work are optimization targets.

The repository CI caches the project-pinned golangci-lint binary and only builds
it with `go install` on cache miss. The official prebuilt action was evaluated,
but the prebuilt `v2.1.6` binary targets an older Go toolchain than this repo and
newer prebuilt lint releases change the enforced lint set. `GoReleaser Snapshot`
now runs on pull requests, `main`, release tags, nightly schedule, and manual
dispatch so packaging signal is available before and after merge.

## Multi-Project Operation

Detent separates host-level orchestration from per-project definitions:

- The resolved global config file lists projects and host-level scheduling settings.
- Each project has `detent.yaml` for Detent-owned machine policy and
  `WORKFLOW.md` for portable agent instructions. The `projects[].workflow`
  path remains the definition anchor; Detent resolves the other files beside
  it, including when that directory is an explicit external definition root.

### Machine-local workflow overlays

Keep `detent.yaml` and `WORKFLOW.md` checked in as separate shared contracts.
For settings that apply on one machine, create schema-versioned
`detent.local.yaml`. For machine-local agent direction, create prose-only
`WORKFLOW.local.md`. Add both local files to the repository's `.gitignore`.
Local configuration wins per structured leaf key; mapping siblings that are
not mentioned remain shared, while local lists and scalar values replace their
shared values. Local Markdown is appended after the shared direction under a
visible machine-local heading.

Detent loads all present definition files at startup. Its watcher reloads
edits, creation, and deletion without a restart, and periodic
reconciliation covers missed filesystem events. `detent doctor` reports an
active overlay, lists its structured override keys, and warns if Git tracks the
local file.

For example:

```yaml
# detent.local.yaml
schema: 1
tracker:
  assignee: local-operator
polling:
  interval_ms: 90000
```

```markdown
<!-- WORKFLOW.local.md -->
Use the tools installed on this build host.
```

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
for `<ref>:WORKFLOW.md`.

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

For projects whose workflow file is already present on the target branch, you
can include `--workflow-ref origin/main` during registration or add
`workflow_ref: origin/main` to the project entry later.

| Field | Reload behavior |
| --- | --- |
| Project list and project settings | Live reload |
| Credentials: `github_token` and project credentials | Live reload |
| `dashboard_access` mode, token, and write access | Live reload; token changes invalidate private dashboard sessions |
| `auth` | Restart required; persisted sessions remain valid until their configured expiry |
| `global.startup` | Live reload |
| `instance_name` | Live reload |
| `global.identity` | Live reload; project runtimes restart in-process and `/api/v1/state.instance.name` updates after the next telemetry snapshot |
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
`read_timeout_ms`, and `stall_timeout_ms`. `agent.max_turn_duration_ms` and
`agent.max_session_duration_ms` apply across backends rather than belonging to
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
[Local Models With Codex And Ollama](docs/local-models-ollama.md) for the
model sizing and context-window checks that also apply when evaluating local
agent backends.

The dashboard and `/api/v1/state` surface each instance identity, authorization
scope, owner, lease renewal time, lease expiry, and selected model usage, which
lets operators verify that scoped instances are not contending for the same
work.

## Dashboard And APIs

The web dashboard starts with the main `detent` command. In running mode it
shows live counts, running issues, retry queue, blocked work, completed
sessions, token totals, budget status, Codex rate-limit snapshots, and GitHub
REST and GraphQL rate-limit snapshots with per-cycle query cost contributors
when the GitHub connector reports them. The shared GitHub API health indicator
separates primary quota exhaustion from observed secondary REST backoff, so a
healthy primary budget can still show `backoff` when GitHub returned `429` for
endpoint families such as pull requests or check runs. Open the indicator to see
remaining primary quota, reset times, backed-off endpoint families, last status
codes, retry timing, and tracker refresh timing.

When an agent backend reports `model_context_window`, Detent also surfaces
context pressure for running and recent sessions. Context pressure is the
session's `total_tokens` divided by the model context window. The JSON snapshot
includes `context_pressure.total_tokens`, `context_limit_tokens`,
`percent_used`, and `threshold_state`; the web dashboard and TUI render the
same value as a compact percent. Thresholds are `normal` below 70%, `watch` at
70%, `warning` at 85%, and `critical` at 95%. Unknown context windows omit the
derived fields instead of reporting a misleading zero.

Context pressure is a model-window signal, not a Detent stop threshold.
For Codex, `codex.turn_timeout_ms` is an inter-message liveness bound, despite
its name. Each stream message starts a new timer. Omitting the key still uses
the one-hour (`3600000` ms) default, and continuous messages can keep one turn
alive indefinitely. Lowering this value therefore detects silence sooner but
does not cap total turn duration. `codex.stall_timeout_ms` is also reset by
stream activity; when both liveness bounds are enabled, the shorter deadline
wins for each receive.

`agent.max_turn_duration_ms` is the total wall-clock bound for each provider
turn attempt. `agent.max_session_duration_ms` spans the full persisted Detent
session, including a failed resume attempt and its fresh fallback. When both
are configured, the shorter applicable deadline wins. Both values default to
`0`, which disables that total-duration bound. These deadlines cancel the
worker through Detent's normal owned process context, so process-tree reaping,
scratch cleanup, and session completion still run.

`agent.merge_worker_max_duration_ms` is a separate hard wall-clock ceiling for
the full lifetime of a worker dispatched in `Merging`, starting when Detent
acquires its slot. It defaults to six hours (`21600000` ms), is not renewed by
progress, and overrides the disabled general duration defaults for merge work.
On breach, Detent cancels the owned worker, releases its slot, logs the elapsed
time and last progress marker at WARN, and parks the issue in `Blocked`.

`agent.max_session_tokens` is an absolute configured ceiling for a session.
`total_tokens` counts input, output, cache-created, and cache-read tokens,
accumulated across every turn of the session — cached context is re-counted on
each turn, so a healthy session accrues millions of tokens within minutes.
Size `max_session_tokens` as a runaway-session guard; a value near one turn's
worth of context terminates every session at its ceiling.
`agent.max_session_context_multiplier` derives a ceiling from the reported
context window when that window is known. A session can show high context
pressure before either ceiling is exceeded, and a low-pressure session can
still hit a lower absolute `agent.max_session_tokens` value. Token rows also
show cache-read efficiency when cached input is reported: cached input divided
by input tokens. Use that value with context pressure to evaluate whether
thread-resume behavior is preserving useful context without repeatedly filling
the window.

`budget.billing_mode` accepts `metered` or `subscription`. Metered mode
enforces configured USD caps and the spend-progress breaker. Subscription mode
keeps notional spend telemetry but never refuses dispatch or parks work based
on USD; Detent instead scales dispatch concurrency with the lowest reported
primary or secondary provider rate-window percentage. An omitted mode preserves
legacy metered enforcement when `budget.enabled=true`, and `detent doctor` warns
until the mode is declared.

`agent.no_progress_spend_limit_usd` defaults to a base limit of `3`, below the
default per-issue budget backstop. The effective limit scales with the session's
reasoning effort: unknown/low `1x`, medium `1.5x`, high `3x`, xhigh `6x`, and
max/ultracode `8x`. These multipliers assume one retry at the observed cost
profile should fit before the breaker fires; `detent doctor` warns when an
effort tier's effective limit is below its observed p50 per-session cost and
recommends a base limit with `1.5x` retry-cost headroom.

Duration limits stop a single overlong turn or session regardless of message
volume. The no-progress spend limit is a separate runaway control that can stop
repeated or expensive work even when each individual session stays below its
duration cap; it is enforced in metered billing mode and advisory in
subscription mode.

Detent sums persisted session cost for each issue after its latest accepted
lane or PR advancement. PR creation, a new head commit, a dirty-to-clean
mergeability transition, and a failing-to-passing CI transition each reset the
spend window. Spend accumulates only while the PR fingerprint remains static.
In metered mode, when spend exceeds the effective limit, Detent parks the issue
in `Blocked` and identifies whether no PR evidence was produced or a linked PR
remained static. The latter points operators toward merge-train capacity and
serialization tuning; the former recommends narrowing or splitting the task.
The next worker must explain the missing progress signal in its first Workpad
update before using tools. Set the value to `0` to disable the breaker and its
history/spend lookups. In subscription mode, the same calculation is advisory
telemetry and never parks the issue.

`agent.failure_breaker` pauses new project dispatches when the same failure
class reaches `same_class_limit` attempts inside `window_seconds`. The default
is five matching failures in one hour, followed by a one-hour cooldown. After
the cooldown or a workflow reload, Detent permits exactly one canary attempt;
a success or different failure class closes the breaker, while the same class
starts a fresh cooldown. The board banner shows the active class, count, and
window.

Daily budget caps are scoped to the configured project. Session rows persist
the project ID; on upgrade, Detent backfills older rows first from work
attempts and then by matching each identifier's repository prefix against the
configured project registry. Any session that remains unattributed counts
toward every project's daily cap as a conservative fallback. `detent doctor`
warns while unattributed completed sessions exist for the current UTC day.

`agent.resume_orphaned_sessions` defaults to `true`. After an unclean Detent
restart, active sessions whose provider identity was journaled are preflighted
and resumed with a short continuation prompt. Missing provider state,
unsupported backends, and failed resume handshakes automatically fall back to
the full fresh continuation prompt. Set the field to `false` to retain fresh
redispatch behavior for every restart.

Completed issues persist an efficiency receipt built from session, attempt,
usage, and workflow-lane rows. Receipts appear in the project Runs table and
issue detail sheet; Reports shows per-merged-issue percentiles, cache share,
first-attempt merge rate, dwell decomposition, anomalies, and a trailing-window
baseline. The default anomaly threshold is 3x the project baseline and can be
changed per workflow. OTLP lifecycle export is optional and disabled when no
endpoint is configured:

```yaml
observability:
  efficiency:
    anomaly_tokens_multiple: 3
    anomaly_sessions_multiple: 3
    anomaly_dwell_multiple: 3
  otlp:
    endpoint: http://127.0.0.1:4318
    service_name: detent
    timeout_ms: 5000
```

The exporter posts OTLP HTTP/JSON traces to `/v1/traces` with linked
`detent.dispatch`, `detent.session`, `detent.gate`, and `detent.merge` spans.
Static collector headers may be supplied with `observability.otlp.headers`.

Useful endpoints:

| Route | Purpose |
| --- | --- |
| `/` | Web dashboard. |
| `/kanban` | Read-only fleet Kanban board across all registered projects. The sidebar link appears only when more than one project is registered. |
| `/projects/<id>` | Project-scoped dashboard overview. |
| `/projects/<id>/kanban` | Project-scoped Kanban board; read-only or integration mode follows that project's workflow config. |
| `/projects/<id>/runs` | Project running, retry, blocked, and recent session details. |
| `/projects/<id>/configuration` | Project workflow and runtime configuration view. |
| `/projects/<id>/diagnostics` | Project health, board flow, and telemetry diagnostics. |
| `/settings` | Fleet settings and configuration summary. |
| `/reports` | Usage reports for spend, tokens, projects, issues, PRs, and models. |
| `/health` | Server health and configured dependency checks. |
| `/events` | Server-sent dashboard updates. Use `?view=kanban` for the fleet board and `?project=<id>&view=kanban` for a project board. |
| `/api/v1/state` | JSON telemetry snapshot. |
| `/api/v1/timeseries?window=10m&bucket=1m` | Fleet chart samples for running agents, tokens/sec, and completions. |
| `/api/v1/projects/<id>/state` | Project-scoped JSON telemetry snapshot. |
| `/api/v1/projects/<id>/timeseries?window=10m&bucket=1m` | Project chart samples for running agents, token spend, and board flow. |
| `/api/v1/projects/<id>/work-items` | Create a runtime work item with `POST` for `local_sqlite` and `github_local` trackers. |
| `/api/v1/refresh` | Request an orchestrator refresh with `POST`. |
| `/api/v1/webhooks/github` | Accept signed GitHub webhook deliveries with `POST`. |
| `/api/v1/<issue>` | JSON detail for a running, retrying, or blocked issue. |

### Dashboard Magic-Link Authentication

Magic-link authentication is disabled unless the top-level `auth` block selects
`magic_link`. When enabled, dashboard pages, browser API calls, SSE streams, and
mobile views require a persisted session. Existing API keys and `api_token`
credentials continue through their normal API checks, while signed GitHub and
token-authenticated intake webhooks keep their dedicated authentication.

```yaml
auth:
  mode: magic_link
  public_url: https://detent.example.com
  allowed_emails:
    - operator@example.com
  link_ttl: 15m
  session_ttl: 720h
  smtp:
    host: smtp.example.com
    port: 587
    username: smtp-user
    password: smtp-password
    from: detent@example.com
```

`public_url` is recommended for reverse-proxy deployments so email and CLI
links use the externally reachable origin. Without it, the running server uses
its resolved dashboard URL and the CLI uses the configured local port. SMTP
credentials are optional, but `username` and `password` must be set together.
Detent uses STARTTLS when the server advertises it. TLS termination for the
dashboard remains the operator's responsibility.

Allowed submissions always receive the same “check your inbox” page as denied
submissions. Links are single-use, short-lived, and stored only as SHA-256
hashes. Sessions are also hashed in SQLite, survive restarts, and expire after
`session_ttl`. Auth configuration changes require a Detent restart.

If SMTP delivery is unavailable, create the same one-time link directly:

```sh
detent auth link operator@example.com --format pretty
```

### Dashboard OIDC Authentication

Set `auth.mode` to `oidc` to use any OpenID Connect provider that supports
discovery and the authorization-code flow. Detent uses S256 PKCE, binds and
validates `state` and `nonce`, verifies the ID-token signature and time claims,
and requires the configured issuer and client ID to match the token exactly.
The provider must include a verified `email` claim; authentication alone does
not grant board access.

```yaml
auth:
  mode: oidc
  public_url: https://detent.example.com
  allowed_emails:
    - operator@example.com
  allowed_domains:
    - example.org
  session_ttl: 12h
  oidc:
    issuer_url: https://identity.example.com
    client_id: detent-dashboard
    client_secret: replace-with-provider-secret
    scopes:
      - profile
      - groups
```

Register this exact redirect URI with the provider:

```text
https://detent.example.com/auth/oidc/callback
```

`issuer_url` is the exact `issuer` value from the provider's
`/.well-known/openid-configuration` document, not the discovery-document URL.
Detent always requests `openid` and `email`, then appends configured `scopes`.
Allowed email and domain comparisons are case-insensitive; a domain entry is an
exact domain and does not implicitly include subdomains. An absent or false
`email_verified` claim is denied. Keep `client_secret` only in the
permission-restricted `global.yaml` and do not commit that file.

OIDC uses the same hashed SQLite sessions as magic links. Sessions survive
Detent restarts and expire after `session_ttl`; changing OIDC settings requires
a restart. Existing API keys, `api_token`, signed GitHub webhooks, and intake
webhooks retain their independent authentication paths.

For a Tailscale-only dashboard, set `public_url` and the provider redirect URI
to the HTTPS Tailscale hostname, such as
`https://buildbox.example-tailnet.ts.net`. The browser completing sign-in must
be connected to that tailnet. Behind a reverse proxy, use the externally
visible HTTPS origin for both values and forward requests to Detent without
rewriting `/auth/oidc/callback`. TLS termination remains the operator's
responsibility.

#### WorkOS AuthKit

Create an OAuth application in [WorkOS Connect](https://workos.com/docs/authkit/connect/oauth),
add Detent's callback to its redirect URIs, and copy an application credential's
client ID and secret. Use the AuthKit domain's issuer value, typically
`https://<subdomain>.authkit.app`; its OpenID configuration is available at
`/.well-known/openid-configuration`. WorkOS documents `email` and
`email_verified` in the issued ID token. A minimal WorkOS configuration is:

```yaml
auth:
  mode: oidc
  public_url: https://detent.example.com
  allowed_domains: [example.com]
  oidc:
    issuer_url: https://example.authkit.app
    client_id: client_01EXAMPLE
    client_secret: replace-with-workos-credential
    scopes: [profile]
```

See the WorkOS [OIDC metadata](https://workos.com/docs/reference/workos-connect/metadata/oauth-authorization-server)
and [token claims](https://workos.com/docs/reference/workos-connect/token)
references when confirming an environment's issuer and claims.

#### Clerk

Create an OAuth application in the Clerk Dashboard as described in
[Clerk's OIDC provider guide](https://clerk.com/docs/guides/configure/auth-strategies/oauth/single-sign-on),
allow the `openid`, `email`, and optional `profile` scopes, add Detent's callback
URI, and copy the client ID and secret. Copy the Discovery URL shown in the
application settings and use its returned `issuer` value. This is normally the
Clerk Frontend API origin, such as
`https://verb-noun-00.clerk.accounts.dev` in development or
`https://clerk.example.com` for a configured production domain.

```yaml
auth:
  mode: oidc
  public_url: https://detent.example.com
  allowed_emails: [operator@example.com]
  oidc:
    issuer_url: https://verb-noun-00.clerk.accounts.dev
    client_id: oauth_app_example
    client_secret: replace-with-clerk-secret
    scopes: [profile]
```

### API Authentication And Work-Item Submission

Configure a top-level `api_token` in `global.yaml`, or set
`DETENT_API_TOKEN` to override it at runtime. Use a high-entropy value; the
recommended shape is a `detent_` prefix followed by a random secret. Mutating
API routes require `Authorization: Bearer <token>` or `X-API-Key: <token>`.
When a token is configured, read-only `GET /api/v1/*` routes require it too.
`GET /health` stays unauthenticated, and the GitHub webhook keeps its HMAC
signature check.

If Detent binds a non-loopback host such as `0.0.0.0` without an `api_token`,
API routes fail closed and mutating routes return `403` until a token is
configured. With no token on loopback, read-only API routes remain open for
local development.

### Private Dashboard URL Access

For a personal deployment that needs remote dashboard access without a VPN,
Detent can require a private, unguessable URL. Enable it with the public URL
served by your TLS proxy:

```sh
detent auth token enable --base-url https://detent.example.com
```

The command generates a 256-bit URL-safe token, stores this configuration in
the permission-restricted global config, and prints the private URL once:

```yaml
dashboard_access:
  mode: private_token
  token: generated_value
  allow_write: false
```

Opening the printed `?token=...` URL establishes a Secure, HttpOnly, SameSite
session cookie and redirects to a clean URL. Dashboard pages, mobile views,
reports, SSE updates, and read APIs then work without putting the token in later links.
Requests without the token or a valid session receive the same non-revealing
`404` response as an unknown route. `/health`, signed GitHub webhooks, intake
webhooks, and API clients with their own bearer credentials remain independent
of dashboard access.

Private URL access is read-only by default. This matters because the URL is a
bearer credential: anyone who receives it can use the dashboard. Set
`dashboard_access.allow_write: true` only when every holder should also be able
to stop runs, move or comment on items, rotate API keys, and change runtime
state. Rotate a leaked or stale URL immediately:

```sh
detent auth token rotate --base-url https://detent.example.com
```

Rotation hot-reloads and invalidates every existing private dashboard session.
Detent redacts the token from reload logs, but browsers, chat systems, proxy
logs, and referrer destinations can still expose bearer URLs. This mode is
appropriate for a small personal deployment and is weaker than identity-based
magic-link or OAuth/OIDC access. TLS termination and proxy log redaction are
the deployer's responsibility. Public `--base-url` values must use HTTPS;
plain HTTP is accepted only for loopback testing. To disable the mode, remove
`dashboard_access` from the global config.

Create a runtime work item:

```sh
curl -fsS -X POST "http://127.0.0.1:4000/api/v1/projects/digitaldrywood-video/work-items" \
  -H "Authorization: Bearer $DETENT_API_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{
    "title": "Author beat visuals",
    "description": "Full markdown brief.",
    "state": "Todo",
    "labels": ["video-assets"],
    "fields": {"render_status": "queued"},
    "priority": 2,
    "deliverable": {
      "kind": "artifact",
      "review_url": "http://127.0.0.1:8090/v/example/g/assets"
    }
  }'
```

The response is `201` with `{"id":"...","identifier":"...","url":"..."}`.
Duplicate submitted identifiers return `409`, invalid states or missing title
and description return `422`, unknown projects return `404`, and trackers other
than `local_sqlite` or `github_local` return `501`.

The CLI uses the same validation and writes through the configured tracker
directly:

```sh
detent work-item add digitaldrywood-video \
  --title "Author beat visuals" \
  --body-file brief.md \
  --label video-assets \
  --field render_status=queued \
  --priority 2 \
  --deliverable-review-url "http://127.0.0.1:8090/v/example/g/assets" \
  --format json
```

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
make security
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
`make security` runs the pinned `govulncheck` and standalone `gosec` scans used
by CI. The gosec baseline skips generated files and documents each legacy rule
excluded in the Makefile; new findings must be fixed or narrowly annotated.
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
- The terminal dashboard writes JSON logs to `detent.log` next to the runtime
  database. `log_max_size_bytes` defaults to 52428800 and `log_max_backups`
  defaults to 5.
- Per-request GitHub REST request/response debug logs are off by default. Set
  `tracker.github_rest_debug_logging: true` only while diagnosing REST traffic.

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
| `detent add-project` | `{"id":"api","workflow":"/repo/WORKFLOW.md","workdir":"/repo","weight":1,"priority":0,"paused":false,"credential_ref":"github"}` |
| `detent pause api --reason "maintenance"` / `detent unpause api` | `{"status":"ok","project":"api","paused":true,"paused_reason":"maintenance"}` |
| `detent promote api --priority 1` | `{"status":"ok","project":"api","priority":1}` |
| `detent remove-project api` | `{"status":"ok","project":"api","removed":true}` |
| `detent work-item add api --title "..." --body "..."` | `{"id":"wi-...","identifier":"wi-...","url":"/projects/api/kanban"}` |
| `detent config path` | `{"path":"/path/global.yaml","rule":"--config"}` |
| `detent auth token enable` / `detent auth token rotate` | `{"url":"https://detent.example.com/?token=..."}` |
| `detent doctor` | `{"checks":[{"name":"Config resolution","status":"OK","detail":"...","hint":"..."}],"summary":{"ok":8,"warn":0,"fail":0},"result":"PASS"}` |

## Configuration

Project behavior is configured in `detent.yaml` or legacy `WORKFLOW.md`
frontmatter. The [complete project configuration reference](docs/config.md)
documents every key, default, required status, and validation rule. Start from
the [minimal example](config.example.yaml), read the
[annotated example](config.annotated.yaml), or search the generated
[fully commented reference](config.reference.yaml) when you need a capability
you have not configured before.

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
| Private dashboard URL | | | `dashboard_access` | disabled |
| Web port | `--port` | `PORT` | `port` | `4000` |
| Instance name | | | `instance_name` | short hostname |
| Automatic update checks | | | `update.auto_check_enabled` | `false` |
| Update check interval | | | `update.check_interval_hours` | `24` |
| Automatic update apply when idle | | | `update.auto_apply_enabled` | `false` |

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

Automatic update checks are host-specific and disabled by default. When
enabled, Detent checks on a jittered in-process schedule and reports the last
check, available or applied version, and next check on `/health` and the Health
dashboard. `detent doctor` reports whether the host is opted in and suggests
enabling checks when it is not. Automatic apply remains off unless explicitly
enabled and only replaces release-installer binaries; other install sources
remain notification-only. When work attempts are active, Detent shows the
pending version in the web and terminal dashboards and waits for the next
fleet-wide idle window before applying it. An operator can instead confirm
immediate apply from the web notification. A successful apply uses the normal
graceful drain path, then re-executes the replaced binary on POSIX systems or
exits cleanly for an external supervisor to restart it on other platforms.

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
