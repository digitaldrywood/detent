# Comparing Detent With Adjacent Agent Tools

> *This comparison reflects publicly available information as of **June 2, 2026** and is provided for orientation only. The agentic-coding space moves extremely fast — these products ship near-monthly — so details (especially pricing, model support, and execution options) may already be outdated. Verify against each vendor's current documentation before relying on anything here.*

## Category Note

These tools are adjacent, but they are not all the same category.

- **Board / spec-driven code orchestration:** Detent, OpenAI Symphony, and
  GitHub Copilot cloud agent to a degree. The common shape is issue or board
  item to isolated work environment to pull request.
- **IDE / developer agent:** Cursor and GitHub Copilot. These start from the
  developer surface and can now hand work to remote or background agents.
- **Autonomous personal assistants:** Hermes Agent and OpenClaw. These are
  conversational, multi-channel assistants with memory, tools, and automation.
  They are useful context for interaction-model comparisons, but they are not
  board-native code orchestration systems.

## Comparison Matrix

| Dimension | Detent | OpenAI Symphony | GitHub Copilot cloud agent | Cursor | Hermes Agent | OpenClaw |
| --- | --- | --- | --- | --- | --- | --- |
| Category | Board-driven multi-agent code orchestration | Open spec plus Elixir reference implementation for Codex orchestration | GitHub-native issue / prompt to PR coding agent | IDE plus cloud / background agents | Autonomous personal assistant | Autonomous personal assistant |
| Primary interaction | GitHub Projects v2 board state | Linear board state in the reference implementation | Assign an issue to Copilot, or prompt from GitHub / VS Code | IDE chat, agent mode, cloud agents, Slack / web handoff | Chat / terminal / messaging gateway | Chat / messaging gateway / local assistant |
| Execution locality | Detent runtime runs on your node; current default workflow uses local worktrees, GitHub Projects, and Codex CLI | Self-hosted Elixir / BEAM service that launches Codex in per-issue workspaces | GitHub Actions-powered ephemeral environment; can use larger or self-hosted Actions runners, including ARC scale sets | Cursor-hosted cloud sandbox, or self-hosted cloud-agent workers in your infrastructure | Self-host or hosted option; gateway and terminal backends can run on your infrastructure | Local-first gateway on your machine / infrastructure |
| Control plane | No Detent SaaS control plane; sovereignty depends on the tracker and model backends you configure | No packaged SaaS product; you run the reference implementation, but Linear and Codex are external services | GitHub cloud and Copilot Agent Control Plane | Cursor cloud orchestrates inference / planning even when workers are self-hosted | Your hosted gateway when self-hosted | Your local gateway |
| Models | Codex app-server today; workflow routes can select Codex backend profiles and models; non-Codex / local backends are roadmap work | Codex app-server | Copilot, Claude, and Codex agents through GitHub-managed access | Cursor-managed first-party and frontier models, including Composer and third-party frontier models | BYO provider or endpoint; docs mention many providers and self-hosted endpoints | Configurable model providers, including OpenAI and other providers; local/self-hosted options depend on configuration |
| Tracker / board | GitHub Projects v2 | Linear in the reference implementation | GitHub Issues / PRs, plus integrations | Repository / IDE / cloud-agent task surfaces, not a board-native state machine | None for code orchestration | None for code orchestration |
| Multi-agent / fleet | Multi-project and multi-instance from self-hosted nodes; authorized and claimed ProjectV2 work; worktree-per-issue dispatch; serialized `Merging` lane | Multiple concurrent issues within one service instance | Multiple agent sessions, but GitHub governs the platform | Parallel cloud agents and team-level agent runs | Assistant can delegate / parallelize, but it is not a PR merge train | Multi-agent routing exists, but the product remains assistant-centric |
| Gating / merge discipline | Workflow-defined validation, Codex review, budget checks, and one-at-a-time merge train | Spec defines workflow gates and handoff states; the exact policy is implementation-defined | Draft PRs, branch protection, review, CI, and GitHub policies | PR review and generated artifacts; no board-native deterministic merge train | Not applicable | Not applicable |
| Memory / skills | Repository-local workflow contract, skills, workpad, telemetry, budget history | `WORKFLOW.md`, skills, tracker comments, runtime observability | Repository instructions, Copilot Memory, MCP, skills, custom agents | Rules, MCP, skills, hooks, cloud-agent memory / automations | Persistent memory, self-improving skills, agentskills.io compatibility | Persistent memory, workspace skills, channel sessions |
| License | MIT | Apache-2.0 | Proprietary SaaS | Proprietary SaaS | MIT | MIT |
| Pricing | Free OSS plus tracker / model / infrastructure costs | Free OSS reference implementation plus Linear / Codex / infrastructure costs | Paid Copilot plans plus AI Credits: Pro $10, Pro+ $39, Max $100 monthly; Business and Enterprise are per-seat with pooled credits; cloud agent usage can also consume Actions minutes | Hobby free; public pricing shows Individual Pro from $20/month and Teams Standard at $40/user/month, with usage-based model pools and on-demand usage; Pro+ / Ultra are positioned for heavier agent users | Free OSS plus model / hosting costs, with optional provider subscriptions | Free OSS plus model / hosting costs |
| Deployment | Single Go binary, web dashboard, TUI, local SQLite | Elixir / OTP service, optional Phoenix dashboard | GitHub.com / Copilot SaaS | Cursor app plus Cursor cloud; optional self-hosted workers for cloud agents | Python-based CLI / gateway across local, Docker, SSH, Modal, Daytona, and other backends | Node-based gateway / CLI, daemon mode, optional apps |
| Setup / time to value | Install one binary, point it at a GitHub Project, run `detent doctor`; board options are auto-provisioned | Clone Symphony, copy / customize `WORKFLOW.md`, configure Linear and Codex, run the Elixir service | Lowest friction for teams already using GitHub and Copilot | Install Cursor and sign in; cloud agents require account setup and repo access | Install / configure Hermes, choose model provider, set up gateway or channels | Install / onboard gateway, pair channels, configure model and workspace |
| Backing / maturity | Independent and early, self-hosted by the project itself | OpenAI-published spec and prototype reference implementation | GitHub / Microsoft platform | Anysphere platform | Nous Research and open source community | Open source community |

## Per-Tool Notes

### Detent

Detent is a board-driven orchestrator, not an IDE assistant and not a
general-purpose personal assistant. It watches GitHub Projects v2, claims work
by board state, creates isolated Git worktrees, dispatches Codex through
`codex app-server`, records a persistent workpad, and moves completed work
through review and a serialized merge lane.

Its clearest distinction is operational discipline: the tracker is the state
machine, the validation gate is explicit, and `Merging` is intentionally
serialized so multiple agents do not invalidate each other's CI at the last
minute. It also has a self-hosted operator surface: one binary, local SQLite,
web dashboard, TUI, `detent doctor`, budget checks, rate-limit telemetry, and
multi-project scheduling from one host.

The caveat is important: Detent does not currently make GitHub Projects or
Codex disappear. The Detent process has no vendor SaaS control plane of its
own, but a fully sovereign deployment depends on the tracker and model backends
available in your environment. Today the product path is GitHub Projects plus
Codex; workflow routing can choose Codex backend profiles and models, but
broader non-Codex / local-model support is a roadmap direction, not a reason to
claim parity with every BYO-model assistant.

Sources: [README](../README.md), [License](../LICENSE).

### OpenAI Symphony

Symphony is Detent's origin point: an OpenAI-published, Apache-2.0 open spec
for orchestrating Codex agents from tracker work, plus an experimental Elixir /
OTP reference implementation. The reference implementation polls Linear for
candidate work, creates a workspace per issue, launches Codex in app-server
mode, and keeps Codex working until the workflow says the item is done or
blocked.

OpenAI presents Symphony primarily as a pattern and spec. The repository itself
warns that the Elixir implementation is prototype software for evaluation and
recommends building a hardened implementation from `SPEC.md`. That is the core
Detent-vs-Symphony distinction: Symphony proved the board-driven pattern;
Detent packages the pattern as a Go product with GitHub Projects v2, native Git
worktrees, dashboard / TUI surfaces, multi-project scheduling, budget checks,
and a deterministic merge train.

OpenAI's launch post says Symphony turned a project-management board such as
Linear into a control plane for coding agents and reported a 500 percent
increase in landed pull requests on some internal teams. That is a useful proof
point for the pattern, but Symphony itself remains a spec plus reference
implementation rather than a polished packaged product.

Sources: [OpenAI Symphony repository](https://github.com/openai/symphony),
[Symphony SPEC.md](https://raw.githubusercontent.com/openai/symphony/main/SPEC.md),
[Elixir reference README](https://raw.githubusercontent.com/openai/symphony/main/elixir/README.md),
[OpenAI launch post](https://openai.com/index/open-source-codex-orchestration-symphony/).

### GitHub Copilot Cloud Agent

GitHub Copilot cloud agent is the closest incumbent to Detent's issue-to-PR
flow. GitHub's product announcement describes assigning a task or issue to
Copilot; it runs in the background with GitHub Actions and submits work as a
pull request. The current docs describe a GitHub Actions-powered ephemeral
development environment where Copilot can inspect code, edit, and run tests.
Organizations can customize setup steps, use larger runners, and run Copilot
cloud agent on self-hosted GitHub Actions runners, with GitHub recommending
ephemeral single-use runners and noting ARC as a common setup.

Copilot leads on distribution and onboarding for GitHub users. It is already
inside issues, pull requests, GitHub Mobile, VS Code, policies, audit logging,
branch protection, and enterprise controls. In 2026 GitHub also made Claude and
Codex available as coding agents in Copilot for paid users, so the platform is
not limited to one model family.

The tradeoff is control. GitHub remains the orchestrator and billing platform.
Usage-based billing is now the current model: GitHub AI Credits measure token
usage, 1 credit equals $0.01, paid individual plans include plan-specific
credits, and organization / enterprise plans pool included credits. GitHub docs
also state that cloud-agent features consume AI Credits, and Copilot code review
can consume GitHub Actions minutes as well. That can be attractive for
centralized governance, but it is not the same as a self-hosted orchestrator
that your team owns end to end.

Sources:
[Copilot coding agent announcement](https://github.blog/news-insights/product-news/github-copilot-meet-the-new-coding-agent/),
[Copilot cloud agent docs](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent),
[environment customization and self-hosted runners](https://docs.github.com/en/copilot/how-tos/copilot-on-github/customize-copilot/customize-cloud-agent/customize-the-agent-environment),
[Claude and Codex availability](https://github.blog/changelog/2026-02-26-claude-and-codex-now-available-for-copilot-business-pro-users/),
[usage-based billing announcement](https://github.blog/news-insights/company-news/github-copilot-is-moving-to-usage-based-billing/),
[individual AI Credits docs](https://docs.github.com/en/copilot/concepts/billing/usage-based-billing-for-individuals),
[organization / enterprise AI Credits docs](https://docs.github.com/en/copilot/concepts/billing/usage-based-billing-for-organizations-and-enterprises).

### Cursor

Cursor is IDE-first. It starts with the editor, chat, rules, MCP, skills, and
agentic development loops, then extends into cloud agents and background work.
Cursor's cloud-agent product can run agents asynchronously and across surfaces
such as the editor, web, Slack, and Microsoft Teams. Cursor also offers
self-hosted cloud-agent workers: code, tool execution, and build artifacts stay
inside the customer's network, while Cursor handles orchestration, inference,
planning, and the user experience through Cursor's cloud.

That self-hosted worker model closes much of the simple "does it run near my
code?" gap. The remaining architectural difference is control-plane ownership.
Cursor's own self-hosted-cloud-agent explanation says workers connect outbound
to Cursor's cloud; Cursor's agent harness handles inference and planning, and
tool results flow back for the next round. For many teams, that is the right
tradeoff: better product polish and model access with less platform work. For a
team that wants the orchestrator and policy loop to live entirely under its
control, Detent is a different bet.

On pricing, the public Cursor pricing page currently shows Hobby as free,
Individual Pro from $20/month, and Teams Standard at $40/user/month. Cursor's
June 2026 Teams pricing update also introduced a Teams Premium seat at
$120/user/month monthly, with more included usage for heavy agent users.
Because Cursor exposes usage pools and on-demand usage around model selection,
exact costs depend heavily on model and agent workload.

Sources:
[Cursor cloud agents](https://cursor.com/en-US/cloud),
[self-hosted cloud agents](https://cursor.com/blog/self-hosted-cloud-agents),
[Cursor pricing](https://cursor.com/pricing),
[Teams pricing update](https://cursor.com/blog/teams-pricing-june-2026),
[Cursor models](https://docs.cursor.com/models/).

### Hermes Agent

Hermes Agent is not a board-to-PR system. It is an autonomous personal
assistant from Nous Research with persistent memory, self-improving skills,
terminal and messaging interfaces, and flexible model-provider configuration.
The README describes it as "the self-improving AI agent built by Nous
Research," with a learning loop, persistent memory, cross-session search,
skills compatible with agentskills.io, and messaging across Telegram, Discord,
Slack, WhatsApp, Signal, and CLI through a gateway.

Hermes is strongest where the user wants an assistant that remembers
preferences, automates recurring personal workflows, works through chat
surfaces, and can use many tools. It is not designed around a tracker state
machine, deterministic pull-request gates, or a serialized merge train. In a
software organization, Hermes is better compared to an operator / assistant
layer than to Detent's work-queue runtime.

Sources: [Hermes Agent repository](https://github.com/NousResearch/hermes-agent),
[Hermes site](https://hermes-agent.ai/),
[features overview](https://hermes-agent.nousresearch.com/docs/user-guide/features/overview/),
[MIT license](https://raw.githubusercontent.com/NousResearch/hermes-agent/main/LICENSE).

### OpenClaw

OpenClaw is also a personal assistant, not a board-native software delivery
orchestrator. Its current README describes a personal AI assistant you run on
your own devices, answering through channels you already use. It lists
WhatsApp, Telegram, Slack, Discord, Google Chat, Signal, iMessage, Microsoft
Teams, Matrix, LINE, Mattermost, Twitch, WeChat, and other channels. The
highlighted architecture is a local-first gateway for sessions, channels,
tools, and events, with onboarding-driven setup and workspace skills.

OpenClaw is compelling when the job is "give my AI assistant hands across my
apps and channels." It is a poor feature-for-feature comparison for Detent
because it does not center GitHub Projects, issue eligibility, CI gates, PR
review state, merge ordering, or release discipline. The honest comparison is
interaction model: OpenClaw starts from a persistent assistant and channels;
Detent starts from a software delivery board and a workflow contract.

Sources: [OpenClaw repository](https://github.com/openclaw/openclaw),
[OpenClaw README](https://raw.githubusercontent.com/openclaw/openclaw/main/README.md),
[MIT license](https://raw.githubusercontent.com/openclaw/openclaw/main/LICENSE).

## Honest Positioning

### Where The Incumbents Lead

Copilot and Cursor lead on distribution, product polish, onboarding, and
ecosystem integration. If a team already lives in GitHub and wants the lowest
possible activation cost, assigning an issue to Copilot is hard to beat. If a
team wants an excellent IDE-first agent surface with cloud workers, model
choice, remote desktops, Slack handoff, and enterprise UX, Cursor is much more
mature.

Symphony leads on provenance. It came from OpenAI, published the pattern, and
showed that tracker-driven Codex orchestration could materially increase
landed pull requests. Hermes and OpenClaw lead in the personal-assistant
category: persistent memory, chat surfaces, channel integrations, voice /
automation features, and assistant-style workflows are their center of gravity.

### Where Detent Is Genuinely Distinct

Detent's distinction is the combination, not any single checkbox:

- A self-hosted orchestrator with no Detent SaaS control plane.
- GitHub Projects v2 as the native state machine.
- Worktree-per-issue execution owned by your node.
- Multi-instance authorization selectors plus claim / lease ownership for
  shared boards.
- Explicit workflow contracts, validation gates, workpad comments, and
  acceptance-driven handoff.
- Budget checks and operational telemetry in the same runtime that dispatches
  work.
- A deterministic, serialized merge lane instead of "many agents opened PRs;
  now humans sort out the landing order."
- One static Go binary with `detent doctor`, dashboard, TUI, config discovery,
  and board option provisioning.

Against Symphony specifically, Detent is the productized Go evolution: GitHub
Projects instead of Linear, native Git worktrees instead of a reference
workspace script, multi-project scheduling and multi-instance ownership from
self-hosted nodes, release-oriented operator surfaces, and the merge train
built in.

### Ease Of Use

Detent's time-to-first-value target is the self-hosted / sovereign lane: install
one binary, point it at a GitHub Projects board, run `detent doctor`, and move
an issue to `Todo`. Detent also auto-provisions missing `Status` and `Priority`
options on the board, which removes a common setup tax.

That does not make Detent lower friction than Copilot for an existing GitHub
Copilot customer or lower friction than Cursor for a Cursor-centered team.
Those products win zero-install or near-zero-install experiences inside their
own platforms. Detent's ease-of-use claim is narrower: it aims to be the
lowest-friction way to run a self-hosted, board-native software-delivery
orchestrator you control.

### Caveats

Copilot self-hosted Actions runners and Cursor self-hosted cloud-agent workers
have closed much of the local-execution gap. Detent should not claim "local
execution" as a unique advantage by itself.

Detent also should not overclaim model sovereignty before non-Codex backends
exist in production. The current system is Codex-first, even though workflow
routes can choose among configured Codex backend profiles and model fields. The
long-term edge is the combination of local orchestration, board-native
governance, pluggable tracker / model boundaries, and deterministic release
gates.

For organizations that value vendor-managed UX, enterprise policy integration,
and polished agent surfaces more than owning the orchestration loop, Copilot or
Cursor may be the better default. For individuals who want a memory-rich AI
assistant across chat channels, Hermes or OpenClaw is the right comparison set.
For teams that want a release process encoded as a board-driven runtime, Detent
is intentionally narrower and more opinionated.
