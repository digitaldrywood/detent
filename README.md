<p align="center"><img src="docs/brand/detent-mark.svg" width="88" height="88" alt="Detent"></p>

# Detent

[![CI](https://github.com/digitaldrywood/detent/actions/workflows/ci.yml/badge.svg)](https://github.com/digitaldrywood/detent/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/digitaldrywood/detent)](LICENSE)
[![Release](https://img.shields.io/github/v/release/digitaldrywood/detent?include_prereleases&sort=semver)](https://github.com/digitaldrywood/detent/releases)

**[detent.build](https://detent.build)** — what Detent is, how it works, and how
to install it.

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
[Bootstrap On A New Machine](docs/bootstrap.md#bootstrap-on-a-new-machine-humans-and-ai-agents)
to go from a bare machine to a running board. To onboard a repository, verify an
existing install, or add a new project to an existing Detent host, use the
agent-executable [Project Onboarding](docs/ONBOARDING.md) runbook.
For project settings, start with the
[complete configuration reference](docs/config.md), the
[minimal example](config.example.yaml), or the
[annotated example](config.annotated.yaml).

## Documentation

[detent.build](https://detent.build) is the product site — the overview, the
mechanism, and install paths. The reference below is the authoritative source
for operating Detent; start there, then follow the focused document for the task
at hand.

### Get started

- [Quick Start](docs/getting-started.md) — configure a tracker and run Detent.
- [Project Onboarding](docs/ONBOARDING.md) — agent-guided installation and project setup.
- [Bootstrap a new machine](docs/bootstrap.md) — install prerequisites, templates, and service files.
- [Configuration](docs/config.md) — project and host configuration, generated field reference, and sample files.

### Operate Detent

- [Concepts](docs/concepts.md) — connectors, board states, cancellation, review gates, and Kanban modes.
- [Dependency workflows](docs/dependency-workflows.md) and [merge train](docs/merge-train.md).
- [Multi-project operation](docs/multi-project.md) and [machine-local workflow overlays](docs/workflow-overlays.md).
- [Webhook freshness](docs/webhook-freshness.md) and [scheduled operations](docs/scheduled-routines.md), including `backlog_admission`.
- [Admission criteria](docs/admission.md) and [efficiency retrospection](docs/retrospection.md).
- [Dashboard and APIs](docs/dashboard-api.md).

### Reference and contribute

- [CLI reference](docs/cli.md) — exit codes, JSON errors, logging, and structured output.
- [Release process](docs/release.md).
- [Development](docs/development.md) and [contribution guide](CONTRIBUTING.md).
- [Comparison](docs/comparison.md), [execution seams](docs/execution-seams.md), and [local models](docs/local-models-ollama.md).

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

The full state table and connector model are in
[Concepts](docs/concepts.md#concepts), and merge-train configuration is in
[Merge Train](docs/merge-train.md#merge-train).

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
