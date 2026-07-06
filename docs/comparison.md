# Comparing Detent With Adjacent Agent Tools

We build Detent as a self-hosted, board-native agent orchestrator for software delivery; this is how we stack it against nearby tools.

## Feature Matrix

| Capability | Detent | OpenAI Symphony | Copilot agent | Cursor | Hermes | OpenClaw | Hyperagent |
|---|---|---|---|---|---|---|---|
| Self-hosted, no vendor control plane | ✅ | ✅ | ❌ | ❌ | ✅ | ✅ | ❌ hosted |
| Runs fully local / air-gappable | ✅ | ✅ | ❌ | ❌ | ✅ | ✅ | ❌ cloud |
| Board/tracker-native (issue→PR) | ✅ GH Projects, issue fields, or labels | ✅ Linear | ✅ GH Issues | ❌ | ❌ | ❌ | ❌ own workspace |
| Deterministic gated merge train | ✅ | 🟡 per spec | ❌ | ❌ | ❌ | ❌ | ❌ |
| Budget / cost caps | ✅ | ❌ | 🟡 | 🟡 | ❌ | ❌ | ✅ hosted controls |
| Multi-project | ✅ | ❌ | ✅ | ✅ | ❌ | ❌ | 🟡 workspace-scoped |
| Multi-instance fleet governance | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | 🟡 hosted agent controls |
| Model-agnostic, BYO incl. local | 🟡 codex now, seam shipped | ❌ Codex | ✅ vendor-managed | ✅ vendor-managed | ✅ | ✅ | ❌ cloud-managed |
| Local skills / workflows (your e2e etc.) | ✅ | 🟡 | ❌ | 🟡 | ✅ | ✅ | ✅ hosted skills/knowledge |
| Multi-channel triggers | 🟡 tracker-driven | 🟡 Linear | 🟡 GitHub | 🟡 IDE/cloud tasks | ✅ messaging gateway | ✅ local gateway | ✅ Slack, schedules, webhooks, email, Telegram, Live Mode |
| Open source | ✅ MIT | ✅ Apache-2.0 | ❌ | ❌ | ✅ MIT | ✅ MIT | ❌ closed-source |
| Free (BYO model cost) | ✅ | ✅ | ❌ paid | ❌ paid | ✅ | ✅ | ❌ usage-billed |
| Single static binary | ✅ | ❌ Elixir/BEAM | — SaaS | — SaaS | ❌ gateway | ❌ gateway | — SaaS |
| ~5-min setup | ✅ | ❌ | ✅ zero-install | ✅ | 🟡 | 🟡 | 🟡 hosted onboarding |

## What Each One Is

- **Detent**: [digitaldrywood/detent](https://github.com/digitaldrywood/detent) is our single-binary Go orchestrator for GitHub-native issue-to-PR work using ProjectV2 or boardless status sources.
- **OpenAI Symphony**: [openai/symphony](https://github.com/openai/symphony) is our origin point: an Apache-2.0 spec plus Elixir reference implementation for Codex on Linear.
- **GitHub Copilot coding agent**: [GitHub Copilot cloud agent](https://docs.github.com/en/copilot/concepts/agents/cloud-agent/about-cloud-agent) is GitHub's paid issue/prompt-to-branch-and-PR agent.
- **Cursor**: [Cursor cloud agents](https://cursor.com/cloud) is an IDE-first agent product with cloud/background agents, automations, and optional self-hosted workers.
- **Hermes**: [NousResearch/hermes-agent](https://github.com/NousResearch/hermes-agent) is Nous's MIT personal assistant with memory, skills, model providers, and messaging gateway.
- **OpenClaw**: [openclaw/openclaw](https://github.com/openclaw/openclaw) is an MIT personal assistant centered on a local gateway and cross-channel automation.
- **Hyperagent**: [Hyperagent](https://www.hyperagent.com/) is Airtable's closed-source, cloud-hosted agent OS / enterprise teammate platform ([confirmed by Browserbase](https://www.browserbase.com/blog/case-study-hyperagent)) for persistent agents with identity, tools, skills, knowledge, model/budget controls, multi-channel triggers, and a first-class [Library](https://www.hyperagent.com/docs/concepts/library) for artifacts.

## Where We're Different

We own the orchestration loop: no Detent vendor control plane, GitHub-native status sources, Detent's own Kanban board, deterministic gates, and a serialized merge train. We also care about operating fleets, not just launching one agent: multi-instance ownership, budget checks, local skills, and a single static binary with a setup path we expect to be measured in minutes. Copilot and Cursor have closed much of the "runs near my code" gap and win zero-install inside their platforms, but they do not give us the same board-native release runtime under our control.

_Last updated: July 6, 2026; verify vendor pricing/models before relying on them._
