<p align="center"><img src="docs/brand/detent-mark.svg" width="88" height="88" alt="Detent"></p>

# Detent

[![CI](https://github.com/digitaldrywood/detent/actions/workflows/ci.yml/badge.svg)](https://github.com/digitaldrywood/detent/actions/workflows/ci.yml)
[![License: FSL-1.1-ALv2](https://img.shields.io/badge/license-FSL--1.1--ALv2-blue)](LICENSE)
[![Release](https://img.shields.io/github/v/release/digitaldrywood/detent?include_prereleases&sort=semver)](https://github.com/digitaldrywood/detent/releases)

Detent runs your engineering process with coding agents. You write issues on a board; Detent hands each ready issue to an agent on a machine you control, checks the result against the gates you defined, and lands it. Many issues run at once, each in its own isolated workspace.

A **detent** is the catch that holds a moving part at a fixed position until it is deliberately released, like the click-stop on a dial. Detent holds each piece of work at a defined stop on the board and only lets it advance when a gate is cleared.

**[detent.build](https://detent.build)** has the product overview. **[cloud.detent.build](https://cloud.detent.build)** is Detent Cloud.

## Start with AI

Detent is meant to be set up by an agent. Paste this prompt into Codex or Claude Code from inside the repository you want Detent to work on:

```text
Help me set up Detent for this repository.

Treat https://github.com/digitaldrywood/detent (branch main) as the source of truth for Detent's docs. Read README.md and docs/cloud-onboarding.md from GitHub; do not clone Detent unless I ask. The repository in the current directory is the target, not Detent itself.

First ask whether I am using Detent Cloud (https://cloud.detent.build) or a self-hosted Hub, and which machine will run the agents (the runner). If self-hosted, also read docs/hub-self-hosting.md.

Then inspect this repository (language, build and test commands, CI workflows, existing AGENTS.md or CLAUDE.md) and draft two files for my review:
- detent.yaml: tracker.kind hub_native, the lanes I want, and a gate whose command is this repository's real check command.
- WORKFLOW.md: the agent instructions for working in this repository. Do not include GitHub pull request or comment steps; native projects land through Detent.

After I approve the files, walk me through: creating the project in the Hub, enrolling the runner (Settings > Providers & runners > Enroll a runner, then the detent hub runner register command it shows), cloning the repository into the directory register prints, signing in to Codex or Claude Code on the runner, and approving the repository policy the runner reports. Finish by filing one small issue in Todo and watching it land.

If I give you a Detent API key (Settings > API & MCP), connect to the Hub's MCP endpoint and use its tools to read state. Ask before every change to the Hub, the repository, or the runner.
```

## How it works

Detent has two parts, both in the same `detent` binary.

1. **The Hub** holds the board, the issues, the conversation history, the review decisions, and the policy each project is allowed to run under. Use Detent Cloud, or run your own Hub.
2. **Runners** are machines you enroll: a laptop, a build server, a VM, or a Fly Sprite. A runner keeps your repository checkout, your agent sign-ins, and your toolchain. Nothing that touches your code runs on the Hub.

A project's behavior lives in two files that you commit to the repository:

- `detent.yaml` is the machine contract: lanes, gate command, review policy, scheduling, retries, and budgets.
- `WORKFLOW.md` is the agent contract: how to work in this repository and what done means.

The runner reports the resolved policy to the Hub, and an owner approves it before work runs. A changed `detent.yaml` needs a new approval, so a commit cannot quietly widen what agents may do.

Work then moves through the board:

1. **You put an issue in `Todo`.** A runner that holds the project claims it, creates an isolated worktree, and starts an agent (Codex or Claude Code) with the issue and `WORKFLOW.md`. The issue moves to `In Progress`.
2. **The agent works** on its own branch and runs your gate command. When it commits, the runner publishes a Change Request on the Hub with the diff.
3. **Gates decide.** With `review.human: false` an accepted change goes straight to `Merging`. With `review.human: true` it waits in `Human Review` until a person approves it or sends it back with requested changes.
4. **Landing is serialized.** In `Merging` the runner that holds the project lands the change on the base branch with plain git, one change at a time, and the Hub moves the issue to `Done`. A change that cannot land goes to `Blocked` with its reason.

Native projects make no GitHub API calls by default; git clone and push still work against any host. GitHub is an optional integration for importing issues and projecting summaries. See [GitHub profiles](docs/github-profiles.md).

## Get started on Detent Cloud

1. Sign in at [cloud.detent.build](https://cloud.detent.build) and create or join an organization.
2. Create a project. New projects start with `Todo`, `In Progress`, `Merging`, `Done`, `Blocked`, and `Human Review`.
3. Commit `detent.yaml` and `WORKFLOW.md` to the repository (the [Start with AI](#start-with-ai) prompt drafts them).
4. [Install Detent](#install) on the runner machine, and sign in there to the agent you plan to use (`codex login` or `claude auth login`).
5. In **Settings > Providers & runners**, choose **Enroll a runner**, pick its projects, and run the command it shows on the runner:

   ```sh
   detent hub runner register --url https://cloud.detent.build/organizations/ORGANIZATION_ID \
     --token TOKEN --name "Build host" --service
   ```

   This creates the runner's identity, writes its configuration, and installs a background service.
6. Clone the repository into the directory that `register` printed (`~/detent-runner/PROJECT` by default) and install its dependencies.
7. When the project settings show "A runner is waiting for a new policy", review it and choose **Approve reported policy**.
8. File an issue in `Todo` and watch it move across the board.

[Cloud onboarding](docs/cloud-onboarding.md) covers each step in detail, including GitHub and artifact storage. For runners on Fly Sprites, see [Sprite runners](docs/sprite-runners.md).

## Self-host the Hub

Running Detent yourself means running the same Hub on your own machine or VM and enrolling runners against it. There is no separate local mode. Self-hosting is free and needs no Detent Cloud account or billing.

```sh
DETENT_HUB_ADMIN_TOKEN=... detent hub serve --database /var/lib/detent-hub/hub.db \
  --listen 127.0.0.1:7777 --github-disabled
```

Put a TLS proxy in front of it, create the organization and project, then enroll runners with your Hub's URL in place of `cloud.detent.build`. [Self-hosted Hub operations](docs/hub-self-hosting.md) covers the systemd unit, Caddy example, authentication, runner enrollment, backup, and upgrades.

## Agents and MCP

Runners drive agents through their own CLIs, using the sign-in already on the runner. Detent stores no model credentials.

- [OpenAI Codex CLI](https://github.com/openai/codex), through `codex app-server`.
- [Claude Code](https://code.claude.com), through the `claude` CLI or `ANTHROPIC_API_KEY`.
- [Pi](docs/pi-agent.md) and [local models through Ollama](docs/local-models-ollama.md).

Model and reasoning effort are set per organization and project in the Hub, not in `detent.yaml`.

The Hub exposes its board, issues, changes, runners, and settings over MCP, so your own agent can file issues, read run history, and review changes. Create a key in **Settings > API & MCP** and point your client at `https://cloud.detent.build/organizations/ORGANIZATION_ID/mcp`. See [API & MCP setup](docs/api-mcp-setup.md).

## Install

Install the latest release on macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh
```

Or with Homebrew:

```sh
brew install digitaldrywood/tap/detent
```

Linux `.deb` and `.rpm` packages, Windows builds, and checksums are attached to each [release](https://github.com/digitaldrywood/detent/releases). Verify the install with `detent --version`.

Update a release install with `detent update` (add `--yes` for automation). On a runner, `detent update --yes` drains running work before it restarts. Upgrade Homebrew and native package installs through their package manager.

To build from source, download `detent_<version>_source.tar.gz` from a release, verify it against that release's checksums, and run `go build -trimpath -ldflags "$(cat BUILD_LDFLAGS)" -o detent ./cmd/detent` inside it. It needs only Go 1.26. Raw git checkouts need Node 24 and `make build`; see [Development](docs/development.md).

## Documentation

Set up:

- [Cloud onboarding](docs/cloud-onboarding.md): organizations, projects, workflow definitions, runner enrollment, and GitHub.
- [Self-hosted Hub operations](docs/hub-self-hosting.md): install, authentication, backup, and recovery.
- [Sprite runners](docs/sprite-runners.md): runners on Fly Sprites that wake when work arrives.
- [Configuration reference](docs/config.md): every `detent.yaml` and runner setting.

Operate:

- [Concepts](docs/concepts.md): lanes, the native tracker, cancellation, and review.
- [Dependency workflows](docs/dependency-workflows.md) and [admission criteria](docs/admission.md).
- [Runner capacity](docs/runner-capacity.md) and [runner usage](docs/runner-usage.md).
- [Runner telemetry and APIs](docs/dashboard-api.md) and [operations reports](docs/operations.md).
- [Native review](docs/native-review.md) of Change Requests.
- [API & MCP setup](docs/api-mcp-setup.md), [MCP capabilities](docs/mcp-capabilities.md), and [Hub API](docs/hub-api.md).
- [Diagnosis](docs/diagnosis.md): how to investigate runtime behavior from recorded history.

Reference and contribute:

- [CLI reference](docs/cli.md).
- [Comparison with other agent tools](docs/comparison.md).
- [Development](docs/development.md), [branching](docs/branching.md), [release process](docs/release.md), and the [contribution guide](CONTRIBUTING.md).
- [Repository invariants](docs/invariants.md).

## License

[FSL-1.1-ALv2](LICENSE) permits reading, self-hosting, internal use, modification, and redistribution under its terms. It prohibits offering Detent as a competing commercial product or service during the first two years after each release. Each release becomes available under the Apache License 2.0 after two years. Earlier MIT-licensed tags remain under the MIT License as published with those tags. Learn more at [fsl.software](https://fsl.software/).
